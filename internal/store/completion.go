package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"iter"
	"log/slog"
	"sync"
	"time"

	"github.com/anacrolix/torrent/metainfo"
	"github.com/anacrolix/torrent/storage"
	"github.com/anacrolix/torrent/types/infohash"
)

// Completion implements storage.PieceCompletion against the state database.
//
// Rolling our own is not tidiness. storage.NewSqlitePieceCompletion is behind
// `//go:build cgo && !nosqlite`, so in a cgo-free build it does not exist and
// NewDefaultPieceCompletionForDir resolves to the bbolt backend instead - a
// second database, in a second format, keyed on a *directory*, which would
// fragment the moment per-torrent download-dirs landed. Ours is keyed on the
// infohash, like PieceKey itself, so where the files sit is irrelevant.
//
// Storage is a bitfield pair per torrent, held in the torrents row. A 40 GiB
// torrent at 4 MiB pieces is 10,000 pieces: a 1,250-byte blob, against a row
// per piece in both upstream backends.
//
// Two bitfields, not one, and this is the part that is easy to get wrong.
// Completion carries Ok *and* Complete:
//
//	Ok=false             → anacrolix hashes the piece to find out
//	Ok=true, Complete=false → trusted: the piece is missing, re-download it
//	Ok=true, Complete=true  → trusted: the piece is good, don't hash it
//
// A single bitfield encodes only Complete, so it cannot distinguish "known
// missing" from "no idea". Collapsing them gives one of two silent bugs:
// always Ok=true and a torrent added over existing files re-downloads
// everything it already has (the overwrite re-add path Radarr exercises
// constantly); always Ok=false and every restart rehashes the whole torrent,
// which is fast-resume not working.
type Completion struct {
	db *sql.DB

	mu     sync.RWMutex
	states map[infohash.T]*bitfields
	dirty  map[infohash.T]bool

	flushInterval time.Duration
	closeOnce     sync.Once
	done          chan struct{}
	flusherDone   chan struct{}
}

type bitfields struct {
	pieces int
	have   []byte
	known  []byte
}

// flushInterval is how long a completed piece may go unrecorded.
//
// The failure mode is bounded and already tolerated: an unclean kill loses at
// most this much completion, so those pieces rehash on restart - which is
// exactly what fast-resume is for. It is not a data-corruption path; the data
// on disk is untouched, only our record of it. Writing the whole blob on every
// Set would be ~1.25 GB of write amplification over a 100k-piece download.
const flushInterval = 5 * time.Second

// NewCompletion loads the recorded bitfields and starts the flusher.
func (s *Store) NewCompletion(ctx context.Context) (*Completion, error) {
	c := newCompletion(s.db)
	if err := c.load(ctx); err != nil {
		return nil, err
	}
	go c.flusher()
	return c, nil
}

// NewMemoryCompletion is the fast_resume = false case: completion is tracked
// for the life of the process but never written, so every restart rehashes.
// It is the same implementation with no database behind it, so the web UI's
// pieces view keeps working either way.
func NewMemoryCompletion() *Completion {
	c := newCompletion(nil)
	go c.flusher()
	return c
}

func newCompletion(db *sql.DB) *Completion {
	return &Completion{
		db:            db,
		states:        make(map[infohash.T]*bitfields),
		dirty:         make(map[infohash.T]bool),
		flushInterval: flushInterval,
		done:          make(chan struct{}),
		flusherDone:   make(chan struct{}),
	}
}

func (c *Completion) load(ctx context.Context) error {
	rows, err := c.db.QueryContext(ctx, `SELECT info_hash, piece_count, have, known FROM torrents`)
	if err != nil {
		return fmt.Errorf("failed to load piece completion: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var (
			hash        []byte
			pieces      int
			have, known []byte
		)
		if err := rows.Scan(&hash, &pieces, &have, &known); err != nil {
			return fmt.Errorf("failed to read a piece completion row: %w", err)
		}
		if pieces == 0 {
			continue
		}
		var ih infohash.T
		copy(ih[:], hash)
		c.states[ih] = &bitfields{
			pieces: pieces,
			have:   resize(have, byteLen(pieces)),
			known:  resize(known, byteLen(pieces)),
		}
	}
	return rows.Err()
}

// Get reports what is known about a piece. It never touches SQLite, which
// makes it faster than either upstream backend - anacrolix queries piece state
// constantly.
func (c *Completion) Get(pk metainfo.PieceKey) (storage.Completion, error) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.get(pk.InfoHash, int(pk.Index)), nil
}

// GetRange implements storage.PieceCompletionGetRanger, taking the lock once
// for a whole run of pieces instead of once each.
func (c *Completion) GetRange(ih infohash.T, begin, end int) iter.Seq[storage.Completion] {
	return func(yield func(storage.Completion) bool) {
		c.mu.RLock()
		defer c.mu.RUnlock()
		for i := begin; i < end; i++ {
			if !yield(c.get(ih, i)) {
				return
			}
		}
	}
}

// get requires at least a read lock.
func (c *Completion) get(ih infohash.T, index int) storage.Completion {
	bf, ok := c.states[ih]
	if !ok || index < 0 || index >= bf.pieces || !testBit(bf.known, index) {
		// Not Ok: we have no record, so anacrolix should hash the piece.
		return storage.Completion{}
	}
	return storage.Completion{Ok: true, Complete: testBit(bf.have, index)}
}

// Set records a piece as complete or missing. Either way the piece becomes
// *known*, which is what stops the next restart from rehashing it.
func (c *Completion) Set(pk metainfo.PieceKey, complete bool) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	bf, ok := c.states[pk.InfoHash]
	if !ok {
		// A torrent whose piece count was never registered. Grow on demand so
		// completion is never silently dropped; Register sizes it properly.
		bf = &bitfields{}
		c.states[pk.InfoHash] = bf
	}
	index := int(pk.Index)
	if index < 0 {
		return fmt.Errorf("negative piece index %d", index)
	}
	if index >= bf.pieces {
		bf.pieces = index + 1
		bf.have = resize(bf.have, byteLen(bf.pieces))
		bf.known = resize(bf.known, byteLen(bf.pieces))
	}
	setBit(bf.known, index, true)
	setBit(bf.have, index, complete)
	c.dirty[pk.InfoHash] = true
	return nil
}

// Register sizes a torrent's bitfields once its piece count is known. Calling
// it is optional - Set grows on demand - but doing it at add time means the
// blob is allocated once rather than repeatedly.
func (c *Completion) Register(ih infohash.T, pieces int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	bf, ok := c.states[ih]
	if !ok {
		bf = &bitfields{}
		c.states[ih] = bf
	}
	if pieces > bf.pieces {
		bf.pieces = pieces
		bf.have = resize(bf.have, byteLen(pieces))
		bf.known = resize(bf.known, byteLen(pieces))
		c.dirty[ih] = true
	}
}

// Bitfield returns a copy of the have bitfield and the piece count, for the
// web UI's pieces view. Returning the count explicitly is what stops the
// trailing padding bits of the last byte from reading as missing pieces.
func (c *Completion) Bitfield(ih infohash.T) ([]byte, int) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	bf, ok := c.states[ih]
	if !ok {
		return nil, 0
	}
	out := make([]byte, len(bf.have))
	copy(out, bf.have)
	return out, bf.pieces
}

// Forget drops a torrent's completion from memory and from the database. Used
// on removal: completion left behind would let a re-add of the same infohash
// claim pieces that are no longer on disk.
func (c *Completion) Forget(ih infohash.T) {
	c.mu.Lock()
	delete(c.states, ih)
	delete(c.dirty, ih)
	c.mu.Unlock()

	if c.db == nil {
		return
	}
	_, err := c.db.Exec(
		`UPDATE torrents SET piece_count = 0, have = NULL, known = NULL WHERE info_hash = ?`, ih.Bytes())
	if err != nil {
		slog.Warn("failed to clear piece completion", "infohash", ih.HexString(), "error", err)
	}
}

// Persistent implements storage.PieceCompletionPersistenter. Reporting true
// lets anacrolix skip flushing pieces it has already marked complete - which
// is only safe when there is a database to flush to.
func (c *Completion) Persistent() bool { return c.db != nil }

// Flush writes every dirty bitfield. Call it after Client.Close(): closing the
// client can emit a final round of Set calls, and flushing before that loses
// exactly the pieces verified last - which looks like a fast-resume bug rather
// than a shutdown-ordering one.
func (c *Completion) Flush() error {
	if c.db == nil {
		c.mu.Lock()
		clear(c.dirty)
		c.mu.Unlock()
		return nil
	}
	c.mu.Lock()
	pending := make(map[infohash.T]bitfields, len(c.dirty))
	for ih := range c.dirty {
		bf := c.states[ih]
		if bf == nil {
			continue
		}
		pending[ih] = bitfields{
			pieces: bf.pieces,
			have:   append([]byte(nil), bf.have...),
			known:  append([]byte(nil), bf.known...),
		}
	}
	clear(c.dirty)
	c.mu.Unlock()

	if len(pending) == 0 {
		return nil
	}
	tx, err := c.db.Begin()
	if err != nil {
		return fmt.Errorf("failed to flush piece completion: %w", err)
	}
	defer tx.Rollback()
	stmt, err := tx.Prepare(`UPDATE torrents SET piece_count = ?, have = ?, known = ? WHERE info_hash = ?`)
	if err != nil {
		return fmt.Errorf("failed to flush piece completion: %w", err)
	}
	defer stmt.Close()
	var errs []error
	for ih, bf := range pending {
		if _, err := stmt.Exec(bf.pieces, bf.have, bf.known, ih.Bytes()); err != nil {
			errs = append(errs, fmt.Errorf("infohash %s: %w", ih.HexString(), err))
		}
	}
	if err := tx.Commit(); err != nil {
		errs = append(errs, err)
	}
	return errors.Join(errs...)
}

// Close stops the flusher and writes everything outstanding.
func (c *Completion) Close() error {
	var err error
	c.closeOnce.Do(func() {
		close(c.done)
		<-c.flusherDone
		err = c.Flush()
	})
	return err
}

func (c *Completion) flusher() {
	defer close(c.flusherDone)
	ticker := time.NewTicker(c.flushInterval)
	defer ticker.Stop()
	for {
		select {
		case <-c.done:
			return
		case <-ticker.C:
			if err := c.Flush(); err != nil {
				slog.Warn("failed to flush piece completion", "error", err)
			}
		}
	}
}

func byteLen(pieces int) int { return (pieces + 7) / 8 }

func resize(b []byte, n int) []byte {
	if len(b) >= n {
		return b[:n]
	}
	out := make([]byte, n)
	copy(out, b)
	return out
}

func testBit(b []byte, i int) bool {
	idx := i / 8
	if idx >= len(b) {
		return false
	}
	return b[idx]&(1<<(uint(i)%8)) != 0
}

func setBit(b []byte, i int, on bool) {
	idx := i / 8
	if idx >= len(b) {
		return
	}
	mask := byte(1 << (uint(i) % 8))
	if on {
		b[idx] |= mask
	} else {
		b[idx] &^= mask
	}
}

// Compile-time proof that we satisfy the interfaces anacrolix looks for.
var (
	_ storage.PieceCompletion             = (*Completion)(nil)
	_ storage.PieceCompletionGetRanger    = (*Completion)(nil)
	_ storage.PieceCompletionPersistenter = (*Completion)(nil)
)
