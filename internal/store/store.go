// Package store is the daemon's persistence: the torrent list, the
// Transmission gids, per-torrent state, file selections, and piece completion,
// all in one SQLite file.
//
// It covers ground anacrolix/torrent does not: the library remembers nothing
// across restarts beyond piece completion, and its own completion backends are
// either cgo-only or a second database. One file, one driver.
package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/anacrolix/torrent/metainfo"
	_ "modernc.org/sqlite" // cgo-free driver, registered as "sqlite"
)

// schema is applied on open. Every statement is idempotent so opening an
// existing database is the same code path as creating one.
//
// `id` is the Transmission gid. It is assigned here, monotonically, rather
// than derived from the infohash (unstable ordering) or from a slice position
// (changes on removal): Sonarr and Radarr require a stable integer id and will
// lose track of a download whose id moves.
//
// `queue` holds the download-queue order. A torrent with no row has not been
// placed yet and is sorted after those that have, by gid - which is add order -
// so a database written before this table existed keeps a sensible order.
//
// `seed_limits` holds the per-torrent seed caps a client sets through
// torrent-set. A missing row means "follow the session limits", which is what
// Transmission's mode 0 means and what every torrent added before this table
// existed should do.
//
// Labels live in their own table rather than in a column on `torrents`: a
// label is a set member, and a new table keeps every statement here a
// CREATE ... IF NOT EXISTS, so an existing state database opens without a
// migration step. ON DELETE CASCADE plus the foreign_keys pragma means
// removing a torrent takes its labels with it.
//
// `have` and `known` are the piece-completion bitfields, ceil(piece_count/8)
// bytes each. Two of them, because Completion carries Ok *and* Complete and
// they mean different things - see completion.go.
const schema = `
CREATE TABLE IF NOT EXISTS torrents (
  id           INTEGER PRIMARY KEY AUTOINCREMENT,
  info_hash    BLOB    NOT NULL UNIQUE,
  name         TEXT    NOT NULL DEFAULT '',
  source       TEXT    NOT NULL DEFAULT '',
  metainfo     BLOB,
  save_path    TEXT    NOT NULL,
  paused       INTEGER NOT NULL DEFAULT 0,
  added_at     INTEGER NOT NULL,
  finished_at  INTEGER,
  error        TEXT    NOT NULL DEFAULT '',
  filtered     INTEGER NOT NULL DEFAULT 0,
  piece_count  INTEGER NOT NULL DEFAULT 0,
  have         BLOB,
  known        BLOB
);
CREATE TABLE IF NOT EXISTS labels (
  torrent_id  INTEGER NOT NULL REFERENCES torrents(id) ON DELETE CASCADE,
  label       TEXT    NOT NULL,
  PRIMARY KEY (torrent_id, label)
);
CREATE TABLE IF NOT EXISTS queue (
  torrent_id  INTEGER NOT NULL PRIMARY KEY REFERENCES torrents(id) ON DELETE CASCADE,
  position    INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS seed_limits (
  torrent_id  INTEGER NOT NULL PRIMARY KEY REFERENCES torrents(id) ON DELETE CASCADE,
  ratio_limit REAL    NOT NULL DEFAULT 0,
  ratio_mode  INTEGER NOT NULL DEFAULT 0,
  idle_limit  INTEGER NOT NULL DEFAULT 0,
  idle_mode   INTEGER NOT NULL DEFAULT 0
);
CREATE TABLE IF NOT EXISTS file_selection (
  torrent_id  INTEGER NOT NULL REFERENCES torrents(id) ON DELETE CASCADE,
  file_index  INTEGER NOT NULL,
  selected    INTEGER NOT NULL,
  PRIMARY KEY (torrent_id, file_index)
);
`

// Torrent is one row of the torrents table: everything about a torrent that
// the engine cannot recover from anacrolix/torrent after a restart.
type Torrent struct {
	ID       int64 // the Transmission gid
	InfoHash metainfo.Hash
	Name     string
	// Source is the magnet URI or URL the torrent was added from, kept so a
	// magnet whose metadata never resolved can be re-added on restart.
	Source   string
	Metainfo []byte
	SavePath string
	Paused   bool
	AddedAt  time.Time
	// FinishedAt is when the torrent first completed, and drives the seed
	// time cap. Zero means it has not finished.
	FinishedAt time.Time
	// Error is the per-torrent error surfaced as Transmission
	// error/errorString, e.g. an allow-list that matched no files.
	Error string
	// Filtered records that the extension allow-list made the recorded file
	// selection, as opposed to every file being selected.
	Filtered bool
	// Labels are the Transmission labels, which Sonarr and Radarr use to carry
	// their category. Sorted, so a client polling twice sees the same order.
	Labels []string
	// SeedLimits are the per-torrent seed caps. The zero value is "follow the
	// session limits".
	SeedLimits SeedLimits
	// QueuePosition is the torrent's place in the download queue, or -1 when it
	// has never been placed.
	QueuePosition int
}

// Unplaced is the QueuePosition of a torrent that has never been given one.
const Unplaced = -1

// SeedLimits are one torrent's seed caps, in the shape Transmission reports
// them: a limit plus a mode saying whether to use it.
//
// RatioMode and IdleMode are Transmission's TR_RATIOLIMIT_*/TR_IDLELIMIT_*:
// 0 follows the session limit, 1 uses the value here, 2 means unlimited.
// IdleLimit is in minutes, as the protocol has it.
type SeedLimits struct {
	RatioLimit float64
	RatioMode  int
	IdleLimit  int64
	IdleMode   int
}

// Store is the open database.
type Store struct {
	db *sql.DB
}

// Open opens (creating if needed) the state database at path.
func Open(path string) (*Store, error) {
	// WAL for concurrent readers alongside the completion flusher; foreign
	// keys so deleting a torrent takes its file selection with it - SQLite
	// leaves them off by default, which would silently orphan rows.
	//
	// Immediate transactions because every transaction here writes, and some
	// read first to decide what to write. A deferred BEGIN takes the write
	// lock only at the first write, so two of them can both read "undecided"
	// and the loser fails with SQLITE_BUSY rather than waiting - busy_timeout
	// cannot help a transaction that already holds a stale snapshot. BEGIN
	// IMMEDIATE takes the lock up front and the second simply queues.
	dsn := path + "?_pragma=journal_mode(WAL)&_pragma=foreign_keys(1)&_pragma=busy_timeout(5000)&_txlock=immediate"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("failed to open the state database %s: %w", path, err)
	}
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, fmt.Errorf("failed to initialise the state database %s: %w", path, err)
	}
	return &Store{db: db}, nil
}

// Close closes the database.
func (s *Store) Close() error { return s.db.Close() }

// Put inserts a torrent, assigning and returning its gid, or returns the
// existing gid when the infohash is already known.
func (s *Store) Put(ctx context.Context, t *Torrent) (int64, error) {
	if id, err := s.IDByInfoHash(ctx, t.InfoHash); err == nil {
		return id, nil
	} else if !errors.Is(err, sql.ErrNoRows) {
		return 0, err
	}

	res, err := s.db.ExecContext(ctx,
		`INSERT INTO torrents (info_hash, name, source, metainfo, save_path, paused, added_at, error, filtered)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		t.InfoHash.Bytes(), t.Name, t.Source, t.Metainfo, t.SavePath,
		boolToInt(t.Paused), addedAt(t).Unix(), t.Error, boolToInt(t.Filtered))
	if err != nil {
		return 0, fmt.Errorf("failed to record the torrent: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("failed to read the assigned torrent id: %w", err)
	}
	if len(t.Labels) > 0 {
		if err := s.SetLabels(ctx, id, t.Labels); err != nil {
			return 0, err
		}
	}
	return id, nil
}

// IDByInfoHash returns the gid for an infohash, or sql.ErrNoRows.
func (s *Store) IDByInfoHash(ctx context.Context, ih metainfo.Hash) (int64, error) {
	var id int64
	err := s.db.QueryRowContext(ctx, `SELECT id FROM torrents WHERE info_hash = ?`, ih.Bytes()).Scan(&id)
	return id, err
}

// List returns every torrent, oldest gid first, which is the order they should
// be restored in.
func (s *Store) List(ctx context.Context) ([]Torrent, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, info_hash, name, source, metainfo, save_path, paused, added_at, finished_at, error, filtered
		 FROM torrents ORDER BY id`)
	if err != nil {
		return nil, fmt.Errorf("failed to list torrents: %w", err)
	}
	defer rows.Close()

	var out []Torrent
	for rows.Next() {
		var (
			t          Torrent
			hash       []byte
			paused     int
			filtered   int
			addedAt    int64
			finishedAt sql.NullInt64
		)
		if err := rows.Scan(&t.ID, &hash, &t.Name, &t.Source, &t.Metainfo, &t.SavePath,
			&paused, &addedAt, &finishedAt, &t.Error, &filtered); err != nil {
			return nil, fmt.Errorf("failed to read a torrent row: %w", err)
		}
		copy(t.InfoHash[:], hash)
		t.Paused = paused != 0
		t.Filtered = filtered != 0
		t.AddedAt = time.Unix(addedAt, 0)
		if finishedAt.Valid {
			t.FinishedAt = time.Unix(finishedAt.Int64, 0)
		}
		out = append(out, t)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	// One query for every torrent's labels rather than one per torrent: the
	// list is read on every start, and Restore is already the slowest part of
	// it.
	if err := s.attachLabels(ctx, out); err != nil {
		return nil, err
	}
	if err := s.attachSeedLimits(ctx, out); err != nil {
		return nil, err
	}
	if err := s.attachQueuePositions(ctx, out); err != nil {
		return nil, err
	}
	return out, nil
}

// attachQueuePositions fills in QueuePosition across a torrent list, leaving -1
// on any torrent that has never been placed.
func (s *Store) attachQueuePositions(ctx context.Context, torrents []Torrent) error {
	if len(torrents) == 0 {
		return nil
	}
	rows, err := s.db.QueryContext(ctx, `SELECT torrent_id, position FROM queue`)
	if err != nil {
		return fmt.Errorf("failed to read the queue order: %w", err)
	}
	defer rows.Close()

	byID := make(map[int64]int)
	for rows.Next() {
		var id, position int
		if err := rows.Scan(&id, &position); err != nil {
			return fmt.Errorf("failed to read a queue row: %w", err)
		}
		byID[int64(id)] = position
	}
	if err := rows.Err(); err != nil {
		return err
	}
	for i := range torrents {
		if position, ok := byID[torrents[i].ID]; ok {
			torrents[i].QueuePosition = position
			continue
		}
		torrents[i].QueuePosition = Unplaced
	}
	return nil
}

// attachSeedLimits fills in SeedLimits across a torrent list in one query.
// Torrents with no row keep the zero value, which means "follow the session".
func (s *Store) attachSeedLimits(ctx context.Context, torrents []Torrent) error {
	if len(torrents) == 0 {
		return nil
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT torrent_id, ratio_limit, ratio_mode, idle_limit, idle_mode FROM seed_limits`)
	if err != nil {
		return fmt.Errorf("failed to read the seed limits: %w", err)
	}
	defer rows.Close()

	byID := make(map[int64]SeedLimits)
	for rows.Next() {
		var id int64
		var l SeedLimits
		if err := rows.Scan(&id, &l.RatioLimit, &l.RatioMode, &l.IdleLimit, &l.IdleMode); err != nil {
			return fmt.Errorf("failed to read a seed-limit row: %w", err)
		}
		byID[id] = l
	}
	if err := rows.Err(); err != nil {
		return err
	}
	for i := range torrents {
		torrents[i].SeedLimits = byID[torrents[i].ID]
	}
	return nil
}

// attachLabels fills in Labels across a torrent list in one query.
func (s *Store) attachLabels(ctx context.Context, torrents []Torrent) error {
	if len(torrents) == 0 {
		return nil
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT torrent_id, label FROM labels ORDER BY torrent_id, label`)
	if err != nil {
		return fmt.Errorf("failed to read the torrent labels: %w", err)
	}
	defer rows.Close()

	byID := make(map[int64][]string)
	for rows.Next() {
		var id int64
		var label string
		if err := rows.Scan(&id, &label); err != nil {
			return fmt.Errorf("failed to read a label row: %w", err)
		}
		byID[id] = append(byID[id], label)
	}
	if err := rows.Err(); err != nil {
		return err
	}
	for i := range torrents {
		torrents[i].Labels = byID[torrents[i].ID]
	}
	return nil
}

// SetPaused records the paused flag. anacrolix/torrent has no pause, so this
// is the authoritative record of it - every consumer of "state" reads it
// rather than inferring one.
func (s *Store) SetPaused(ctx context.Context, id int64, paused bool) error {
	return s.update(ctx, `UPDATE torrents SET paused = ? WHERE id = ?`, boolToInt(paused), id)
}

// SetName records the torrent name once metadata resolves.
func (s *Store) SetName(ctx context.Context, id int64, name string) error {
	return s.update(ctx, `UPDATE torrents SET name = ? WHERE id = ?`, name, id)
}

// SetMetainfo stores the resolved info dict so a restart need not re-fetch it
// from the swarm.
func (s *Store) SetMetainfo(ctx context.Context, id int64, mi []byte) error {
	return s.update(ctx, `UPDATE torrents SET metainfo = ? WHERE id = ?`, mi, id)
}

// SetError records (or clears, with "") the per-torrent error.
func (s *Store) SetError(ctx context.Context, id int64, msg string) error {
	return s.update(ctx, `UPDATE torrents SET error = ? WHERE id = ?`, msg, id)
}

// SetFinishedAt records when a torrent first completed. A zero time clears it,
// which happens when a torrent stops being complete.
func (s *Store) SetFinishedAt(ctx context.Context, id int64, at time.Time) error {
	if at.IsZero() {
		return s.update(ctx, `UPDATE torrents SET finished_at = NULL WHERE id = ?`, id)
	}
	return s.update(ctx, `UPDATE torrents SET finished_at = ? WHERE id = ?`, at.Unix(), id)
}

// Delete removes a torrent and, by cascade, its file selection. The piece
// completion bitfields live in the same row, so they go too - leaving them
// behind would let a later re-add of the same infohash claim pieces that are
// no longer on disk.
func (s *Store) Delete(ctx context.Context, id int64) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM torrents WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("failed to delete torrent %d: %w", id, err)
	}
	return nil
}

// SetFileSelection replaces the recorded per-file selection. anacrolix/torrent
// will not restore file priorities across restarts, so this is the only record
// of which files an allow-list or a client chose.
func (s *Store) SetFileSelection(ctx context.Context, id int64, selected []bool) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("failed to record the file selection: %w", err)
	}
	defer tx.Rollback()

	if _, err := tx.ExecContext(ctx, `DELETE FROM file_selection WHERE torrent_id = ?`, id); err != nil {
		return fmt.Errorf("failed to clear the previous file selection: %w", err)
	}
	if err := writeFileSelection(ctx, tx, id, selected); err != nil {
		return err
	}
	return tx.Commit()
}

// FileSelection returns the recorded selection, or nil when none is recorded -
// which means the selection has not been decided yet.
func (s *Store) FileSelection(ctx context.Context, id int64) ([]bool, error) {
	return readFileSelection(ctx, s.db, id)
}

// Decision is what a torrent should download, as decided once its metadata
// is known: the per-file selection, whether the extension allow-list made it,
// and the error to surface when the allow-list matched nothing.
type Decision struct {
	Selection []bool
	Filtered  bool
	Error     string
}

// DecideFileSelection records proposal as the torrent's selection unless one
// is already recorded, and returns whichever is recorded afterwards. decided
// reports that this call's proposal is the one that was recorded.
//
// This is what makes the database, not whichever goroutine got there first,
// the answer to "what does this torrent want". The metadata watcher, a resume
// and the download queue can all ask at once; each proposes, one proposal is
// recorded, and every caller applies that one. Read and write share an
// immediate transaction, so no caller can read "undecided" while another is
// recording.
func (s *Store) DecideFileSelection(ctx context.Context, id int64, proposal Decision) (_ Decision, decided bool, err error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Decision{}, false, fmt.Errorf("failed to decide the file selection: %w", err)
	}
	defer tx.Rollback()

	selection, err := readFileSelection(ctx, tx, id)
	if err != nil {
		return Decision{}, false, err
	}
	if len(selection) > 0 {
		recorded := Decision{Selection: selection}
		var filtered int
		if err := tx.QueryRowContext(ctx, `SELECT filtered, error FROM torrents WHERE id = ?`, id).
			Scan(&filtered, &recorded.Error); err != nil {
			return Decision{}, false, fmt.Errorf("failed to read the recorded file selection: %w", err)
		}
		recorded.Filtered = filtered != 0
		return recorded, false, tx.Commit()
	}

	if err := writeFileSelection(ctx, tx, id, proposal.Selection); err != nil {
		return Decision{}, false, err
	}
	res, err := tx.ExecContext(ctx, `UPDATE torrents SET filtered = ?, error = ? WHERE id = ?`,
		boolToInt(proposal.Filtered), proposal.Error, id)
	if err != nil {
		return Decision{}, false, fmt.Errorf("failed to record the file selection: %w", err)
	}
	// The selection rows would be refused by the foreign key anyway, but say
	// which torrent was missing rather than reporting a constraint.
	if n, err := res.RowsAffected(); err == nil && n == 0 {
		return Decision{}, false, fmt.Errorf("failed to record the file selection: no torrent with id %d", id)
	}
	if err := tx.Commit(); err != nil {
		return Decision{}, false, fmt.Errorf("failed to record the file selection: %w", err)
	}
	return proposal, true, nil
}

// querier is what the file-selection helpers need, so they run the same
// inside a transaction and outside one.
type querier interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
}

func writeFileSelection(ctx context.Context, q querier, id int64, selected []bool) error {
	for i, sel := range selected {
		if _, err := q.ExecContext(ctx,
			`INSERT INTO file_selection (torrent_id, file_index, selected) VALUES (?, ?, ?)`,
			id, i, boolToInt(sel)); err != nil {
			return fmt.Errorf("failed to record the selection of file %d: %w", i, err)
		}
	}
	return nil
}

func readFileSelection(ctx context.Context, q querier, id int64) ([]bool, error) {
	rows, err := q.QueryContext(ctx,
		`SELECT file_index, selected FROM file_selection WHERE torrent_id = ? ORDER BY file_index`, id)
	if err != nil {
		return nil, fmt.Errorf("failed to read the file selection: %w", err)
	}
	defer rows.Close()

	var out []bool
	for rows.Next() {
		var index int
		var selected int
		if err := rows.Scan(&index, &selected); err != nil {
			return nil, fmt.Errorf("failed to read a file selection row: %w", err)
		}
		for len(out) <= index {
			out = append(out, false)
		}
		out[index] = selected != 0
	}
	return out, rows.Err()
}

// SetQueuePositions writes the whole queue order in one transaction.
//
// The order is rewritten wholesale rather than patched, because a move
// renumbers everything after it and a half-applied renumber would leave two
// torrents claiming one position.
func (s *Store) SetQueuePositions(ctx context.Context, positions map[int64]int) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("failed to record the queue order: %w", err)
	}
	defer tx.Rollback()

	stmt, err := tx.PrepareContext(ctx,
		`INSERT INTO queue (torrent_id, position) VALUES (?, ?)
		 ON CONFLICT(torrent_id) DO UPDATE SET position = excluded.position`)
	if err != nil {
		return fmt.Errorf("failed to record the queue order: %w", err)
	}
	defer stmt.Close()
	for id, position := range positions {
		if _, err := stmt.ExecContext(ctx, id, position); err != nil {
			return fmt.Errorf("failed to record the queue position of torrent %d: %w", id, err)
		}
	}
	return tx.Commit()
}

// SetSeedLimits records a torrent's seed caps, replacing any already stored.
func (s *Store) SetSeedLimits(ctx context.Context, id int64, l SeedLimits) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO seed_limits (torrent_id, ratio_limit, ratio_mode, idle_limit, idle_mode)
		 VALUES (?, ?, ?, ?, ?)
		 ON CONFLICT(torrent_id) DO UPDATE SET
		   ratio_limit = excluded.ratio_limit, ratio_mode = excluded.ratio_mode,
		   idle_limit  = excluded.idle_limit,  idle_mode  = excluded.idle_mode`,
		id, l.RatioLimit, l.RatioMode, l.IdleLimit, l.IdleMode)
	if err != nil {
		return fmt.Errorf("failed to record the seed limits for torrent %d: %w", id, err)
	}
	return nil
}

// SeedLimitsFor returns a torrent's seed caps, or the zero value - "follow the
// session limits" - when none are recorded.
func (s *Store) SeedLimitsFor(ctx context.Context, id int64) (SeedLimits, error) {
	var l SeedLimits
	err := s.db.QueryRowContext(ctx,
		`SELECT ratio_limit, ratio_mode, idle_limit, idle_mode FROM seed_limits WHERE torrent_id = ?`,
		id).Scan(&l.RatioLimit, &l.RatioMode, &l.IdleLimit, &l.IdleMode)
	if errors.Is(err, sql.ErrNoRows) {
		return SeedLimits{}, nil
	}
	if err != nil {
		return SeedLimits{}, fmt.Errorf("failed to read the seed limits for torrent %d: %w", id, err)
	}
	return l, nil
}

// SetLabels replaces a torrent's labels. Duplicates and blanks are dropped, so
// what comes back out of Labels is what a client can meaningfully match on.
func (s *Store) SetLabels(ctx context.Context, id int64, labels []string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("failed to record the torrent labels: %w", err)
	}
	defer tx.Rollback()

	if _, err := tx.ExecContext(ctx, `DELETE FROM labels WHERE torrent_id = ?`, id); err != nil {
		return fmt.Errorf("failed to clear the previous torrent labels: %w", err)
	}
	stmt, err := tx.PrepareContext(ctx,
		`INSERT OR IGNORE INTO labels (torrent_id, label) VALUES (?, ?)`)
	if err != nil {
		return fmt.Errorf("failed to record the torrent labels: %w", err)
	}
	defer stmt.Close()
	for _, label := range labels {
		if label == "" {
			continue
		}
		if _, err := stmt.ExecContext(ctx, id, label); err != nil {
			return fmt.Errorf("failed to record the label %q: %w", label, err)
		}
	}
	return tx.Commit()
}

// Labels returns a torrent's labels, sorted, or nil when it has none.
func (s *Store) Labels(ctx context.Context, id int64) ([]string, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT label FROM labels WHERE torrent_id = ? ORDER BY label`, id)
	if err != nil {
		return nil, fmt.Errorf("failed to read the torrent labels: %w", err)
	}
	defer rows.Close()

	var out []string
	for rows.Next() {
		var label string
		if err := rows.Scan(&label); err != nil {
			return nil, fmt.Errorf("failed to read a label row: %w", err)
		}
		out = append(out, label)
	}
	return out, rows.Err()
}

func (s *Store) update(ctx context.Context, query string, args ...any) error {
	if _, err := s.db.ExecContext(ctx, query, args...); err != nil {
		return fmt.Errorf("failed to update the torrent record: %w", err)
	}
	return nil
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

func addedAt(t *Torrent) time.Time {
	if t.AddedAt.IsZero() {
		return time.Now()
	}
	return t.AddedAt
}
