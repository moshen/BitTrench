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
	// Filtered records that the extension allow-list has already been applied,
	// so a restored torrent keeps its persisted selection and is exempt from
	// being filtered again.
	Filtered bool
	// Labels are the Transmission labels, which Sonarr and Radarr use to carry
	// their category. Sorted, so a client polling twice sees the same order.
	Labels []string
	// SeedLimits are the per-torrent seed caps. The zero value is "follow the
	// session limits".
	SeedLimits SeedLimits
}

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
	dsn := path + "?_pragma=journal_mode(WAL)&_pragma=foreign_keys(1)&_pragma=busy_timeout(5000)"
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
	return out, nil
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

// SetFiltered marks the extension allow-list as applied.
func (s *Store) SetFiltered(ctx context.Context, id int64, filtered bool) error {
	return s.update(ctx, `UPDATE torrents SET filtered = ? WHERE id = ?`, boolToInt(filtered), id)
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
	stmt, err := tx.PrepareContext(ctx,
		`INSERT INTO file_selection (torrent_id, file_index, selected) VALUES (?, ?, ?)`)
	if err != nil {
		return fmt.Errorf("failed to record the file selection: %w", err)
	}
	defer stmt.Close()
	for i, sel := range selected {
		if _, err := stmt.ExecContext(ctx, id, i, boolToInt(sel)); err != nil {
			return fmt.Errorf("failed to record the selection of file %d: %w", i, err)
		}
	}
	return tx.Commit()
}

// FileSelection returns the recorded selection, or nil when none is recorded -
// which means "everything", not "nothing".
func (s *Store) FileSelection(ctx context.Context, id int64) ([]bool, error) {
	rows, err := s.db.QueryContext(ctx,
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
