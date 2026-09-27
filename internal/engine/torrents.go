package engine

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/anacrolix/torrent"
	"github.com/anacrolix/torrent/metainfo"
	"github.com/anacrolix/torrent/storage"

	"github.com/moshen/bittrench/internal/store"
)

// maxTorrentFileBytes caps a .torrent fetched by URL. This runs on an RPC
// handler's critical path against a remote server, so it needs a ceiling.
const maxTorrentFileBytes = 8 << 20

// record is the engine's own view of a torrent: everything anacrolix/torrent
// does not model, which is most of what a Transmission client asks about.
type record struct {
	ID       int64
	Torrent  *torrent.Torrent
	Source   string
	SavePath string

	// Paused is engine-owned state. anacrolix/torrent has no pause, so there
	// is nothing to infer it from - every consumer of "state" reads this.
	Paused     bool
	AddedAt    time.Time
	FinishedAt time.Time
	Error      string
	// Filtered records that the allow-list has been applied, so a restored
	// torrent keeps its selection instead of being filtered again.
	Filtered bool
	// Labels are the Transmission labels. Sonarr and Radarr carry their
	// category in them, and both filter their queue by it, so they are
	// engine-owned state that has to survive a restart.
	Labels []string
	// SeedLimits are this torrent's own seed caps. The zero value follows the
	// session limits, which is what every torrent does until a client sets
	// otherwise through torrent-set.
	SeedLimits store.SeedLimits
	// QueuePosition is the torrent's place in the download queue.
	QueuePosition int
	// Queued is set while the queue is holding this torrent back. It is
	// deliberately not Paused: both are expressed as "nothing is wanted", but
	// only the user can clear Paused, while the queue clears Queued as slots
	// free up. Runtime state - the position is what persists.
	Queued bool

	// storedFinishedAt is what the database currently holds, so the monitor
	// writes only when the value actually changes rather than every tick.
	storedFinishedAt time.Time

	// ready closes once the post-metadata work has run: the name recorded,
	// the completion bitfields sized, and the file selection applied. It
	// closes even when that work is skipped, so a waiter never hangs.
	ready chan struct{}
}

// AddRequest is one torrent to add.
type AddRequest struct {
	// Source is a magnet URI or an http(s) URL to a .torrent. Exactly one of
	// Source and Metainfo must be set.
	Source   string
	Metainfo []byte
	// DownloadDir overrides the session-wide save path for this torrent.
	DownloadDir string
	// Paused starts the torrent stopped.
	Paused bool
	// Labels are the Transmission labels to record against the torrent.
	Labels []string
}

// Add adds a torrent and returns its gid.
//
// duplicate reports that the infohash is already managed, in which case nothing
// was added and id names the torrent that was already there. Transmission
// answers such an add with `torrent-duplicate` rather than `torrent-added`, and
// clients tell the two apart.
func (e *Engine) Add(ctx context.Context, req AddRequest) (id int64, duplicate bool, err error) {
	spec, err := e.spec(ctx, req)
	if err != nil {
		return 0, false, err
	}

	dir, err := e.resolveDownloadDir(req.DownloadDir)
	if err != nil {
		return 0, false, err
	}
	spec.Storage = storage.NewFileWithCompletion(dir, e.completion)

	// Nothing is downloaded until a selection is applied: anacrolix starts
	// every piece at priority None. That is what makes both the paused add and
	// the allow-list's paused-until-decided add free of any window in which
	// the whole torrent is being fetched - there is nothing to suppress.
	//
	// Only upload is disallowed up front, since that flag is independent of
	// the piece bookkeeping. See applyPaused for why the download flag is not
	// used at all.
	filtering := len(e.allowedExtensions) > 0
	if req.Paused {
		spec.DisallowDataUpload = true
	}

	// anacrolix's "not already in the client" is not the question here - see
	// the record check below.
	t, _, err := e.client.AddTorrentSpec(spec)
	if err != nil {
		return 0, false, fmt.Errorf("failed to add the torrent: %w", err)
	}

	id, err = e.store.Put(ctx, &store.Torrent{
		InfoHash: t.InfoHash(),
		Name:     t.Name(),
		Source:   req.Source,
		Metainfo: req.Metainfo,
		SavePath: dir,
		Paused:   req.Paused,
		AddedAt:  time.Now(),
		Labels:   req.Labels,
	})
	if err != nil {
		return 0, false, err
	}
	// Already managed: hand the gid back rather than treating it as an error,
	// and say that is what happened. The record is the test, not anacrolix's
	// isNew - a torrent the store knows but that is not in our records (a `get`
	// run, which restores nothing) is not in the session, so it is not a
	// duplicate and does need a record built below.
	if existing := e.record(id); existing != nil {
		slog.Info("torrent already added", "id", id, "name", t.Name(),
			"infohash", t.InfoHash().HexString())
		return id, true, nil
	}

	rec := &record{
		ID:            id,
		Torrent:       t,
		Source:        req.Source,
		SavePath:      dir,
		Paused:        req.Paused,
		AddedAt:       time.Now(),
		Labels:        slices.Clone(req.Labels),
		QueuePosition: e.nextQueuePosition(),
		ready:         make(chan struct{}),
	}
	e.mu.Lock()
	e.records[id] = rec
	e.mu.Unlock()

	if err := e.store.SetQueuePositions(ctx, map[int64]int{id: rec.QueuePosition}); err != nil {
		// A position that was not written is recovered on the next start, where
		// an unplaced torrent sorts last by gid - the same place it is now.
		slog.Warn("failed to record the queue position", "id", id, "error", err)
	}
	if req.Paused {
		e.applyPaused(rec)
	}
	e.watch(rec, filtering)
	// The new torrent may have to wait, or may be able to start immediately.
	e.reconcileQueue()
	slog.Info("torrent added", "id", id, "name", t.Name(),
		"infohash", t.InfoHash().HexString(), "dir", dir, "paused", req.Paused)
	return id, false, nil
}

// spec turns an AddRequest into a TorrentSpec, fetching a .torrent by URL
// through the tunnel when necessary and rewriting UDP announce URLs so no
// hostname ever reaches the tracker client.
func (e *Engine) spec(ctx context.Context, req AddRequest) (*torrent.TorrentSpec, error) {
	var spec *torrent.TorrentSpec

	switch {
	case len(req.Metainfo) > 0:
		mi, err := metainfo.Load(bytes.NewReader(req.Metainfo))
		if err != nil {
			return nil, fmt.Errorf("failed to parse the torrent file: %w", err)
		}
		if spec, err = torrent.TorrentSpecFromMetaInfoErr(mi); err != nil {
			return nil, fmt.Errorf("failed to read the torrent file: %w", err)
		}
	case strings.HasPrefix(req.Source, "magnet:"):
		var err error
		if spec, err = torrent.TorrentSpecFromMagnetUri(req.Source); err != nil {
			return nil, fmt.Errorf("failed to parse the magnet URI: %w", err)
		}
	case strings.HasPrefix(req.Source, "http://"), strings.HasPrefix(req.Source, "https://"):
		mi, err := e.fetchMetainfo(ctx, req.Source)
		if err != nil {
			return nil, err
		}
		if spec, err = torrent.TorrentSpecFromMetaInfoErr(mi); err != nil {
			return nil, fmt.Errorf("failed to read the torrent file from %s: %w", req.Source, err)
		}
	default:
		return nil, fmt.Errorf("unsupported torrent source %q: expected a magnet URI or an http(s) URL", req.Source)
	}

	for i, tier := range spec.Trackers {
		spec.Trackers[i] = e.trackers.RewriteAll(ctx, tier)
	}
	return spec, nil
}

// fetchMetainfo downloads a .torrent through the tunnel.
func (e *Engine) fetchMetainfo(ctx context.Context, url string) (*metainfo.MetaInfo, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("invalid torrent URL %q: %w", url, err)
	}
	resp, err := e.net.HTTPClient(trackerHTTPTimeout).Do(req)
	if err != nil {
		return nil, fmt.Errorf("failed to fetch %s through the tunnel: %w", url, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("fetching %s returned %s", url, resp.Status)
	}
	mi, err := metainfo.Load(io.LimitReader(resp.Body, maxTorrentFileBytes))
	if err != nil {
		return nil, fmt.Errorf("failed to parse the torrent file from %s: %w", url, err)
	}
	return mi, nil
}

// resolveDownloadDir validates a per-torrent download directory and creates it.
//
// It is deliberately not restricted to subdirectories of save_path: the RPC
// endpoint binds localhost behind basic auth, and operators legitimately point
// Sonarr and Radarr at separate trees. The residual risk is stated plainly in
// the handoff - anything that can reach the RPC endpoint with credentials can
// make the daemon create directories and write torrent data anywhere the
// process user can write. Clean plus absolute stops path confusion, not a
// determined caller.
func (e *Engine) resolveDownloadDir(dir string) (string, error) {
	if dir == "" {
		dir = e.cfg.Torrent.SavePath
	}
	dir = filepath.Clean(dir)
	if !filepath.IsAbs(dir) {
		return "", fmt.Errorf("download-dir must be an absolute path, got %q", dir)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", fmt.Errorf("failed to create the download directory %s: %w", dir, err)
	}
	return dir, nil
}

// watch waits for metadata and then does the per-torrent work that needs it:
// recording the name and metainfo, sizing the completion bitfields, and
// applying either the allow-list or a restored file selection.
func (e *Engine) watch(rec *record, filtering bool) {
	e.wg.Add(1)
	go func() {
		defer e.wg.Done()
		defer close(rec.ready)
		select {
		case <-rec.Torrent.GotInfo():
		case <-rec.Torrent.Closed():
			return
		case <-e.ctx.Done():
			return
		}
		e.onMetadata(rec, filtering)
	}()
}

func (e *Engine) onMetadata(rec *record, filtering bool) {
	t := rec.Torrent
	ctx := e.ctx

	if err := e.store.SetName(ctx, rec.ID, t.Name()); err != nil {
		slog.Warn("failed to record the torrent name", "id", rec.ID, "error", err)
	}
	// Store the resolved metainfo so a restart re-adds the torrent directly
	// rather than waiting on the swarm for a magnet's info dict again.
	if mi := t.Metainfo(); mi.InfoBytes != nil {
		var buf bytes.Buffer
		if err := mi.Write(&buf); err != nil {
			slog.Warn("failed to serialise the torrent metainfo", "id", rec.ID, "error", err)
		} else if err := e.store.SetMetainfo(ctx, rec.ID, buf.Bytes()); err != nil {
			slog.Warn("failed to record the torrent metainfo", "id", rec.ID, "error", err)
		}
	}
	e.completion.Register(t.InfoHash(), t.NumPieces())

	if filtering {
		e.applyAllowList(rec)
		return
	}

	e.mu.RLock()
	held := rec.Paused || rec.Queued
	e.mu.RUnlock()
	if held {
		// Paused, or waiting in the queue: either way nothing is wanted.
		// Leaving every piece at the priority anacrolix starts them on is
		// exactly that, so there is nothing to do until it is let go.
		return
	}

	// A restored torrent keeps whatever selection was persisted.
	selection, err := e.store.FileSelection(ctx, rec.ID)
	if err != nil {
		slog.Warn("failed to read the persisted file selection", "id", rec.ID, "error", err)
	}
	if len(selection) > 0 {
		if err := applySelection(t, selection); err != nil {
			slog.Warn("failed to restore the file selection", "id", rec.ID, "error", err)
		}
		return
	}

	// Otherwise select every file. This is not optional: anacrolix/torrent
	// starts a torrent with every piece at priority None, so a torrent with no
	// selection applied sits at 0% forever while looking perfectly healthy.
	//
	// Per-file priorities rather than Torrent.DownloadAll(), which raises
	// piece priorities without touching File.prio - so DownloadAll leaves
	// every File.Priority() reporting None, and anything deriving "is this
	// file wanted" from it (the RPC layer, the UI) would say no about a
	// torrent that is downloading fine.
	all := make([]bool, len(t.Files()))
	for i := range all {
		all[i] = true
	}
	if err := applySelection(t, all); err != nil {
		slog.Warn("failed to select the torrent's files", "id", rec.ID, "error", err)
	}
}

// applyAllowList downloads only the files whose extension is allowed.
//
// The rule that matters: if applying the selection fails, the torrent is NOT
// resumed. Resuming would download everything, which is exactly the
// user-visible "allow-list ignored" bug this filter exists to prevent.
func (e *Engine) applyAllowList(rec *record) {
	t := rec.Torrent
	files := t.Files()

	selection := make([]bool, len(files))
	matched := 0
	for i, f := range files {
		if e.extensionAllowed(f.DisplayPath()) {
			selection[i] = true
			matched++
		}
	}

	if matched == 0 {
		msg := fmt.Sprintf("no files match the allowed_extensions allow-list (%s)",
			strings.Join(e.cfg.Torrent.AllowedExtensions, ", "))
		e.setError(rec, msg)
		slog.Warn("torrent rejected by the extension allow-list", "id", rec.ID, "name", t.Name())
		return
	}

	// Record the decision before acting on it: if the write fails we must not
	// start downloading, because a restart would then have no record of what
	// was wanted and would fetch everything.
	if err := e.store.SetFileSelection(e.ctx, rec.ID, selection); err != nil {
		e.setError(rec, fmt.Sprintf("failed to record the file selection: %v", err))
		slog.Error("not resuming: the file selection could not be recorded",
			"id", rec.ID, "name", t.Name(), "error", err)
		return
	}
	if !rec.Paused {
		if err := applySelection(t, selection); err != nil {
			e.setError(rec, fmt.Sprintf("failed to apply the extension allow-list: %v", err))
			slog.Error("not resuming: applying the allow-list failed, and resuming would download everything",
				"id", rec.ID, "name", t.Name(), "error", err)
			return
		}
	}

	e.mu.Lock()
	rec.Filtered = true
	e.mu.Unlock()
	if err := e.store.SetFiltered(e.ctx, rec.ID, true); err != nil {
		slog.Warn("failed to record the filtered flag", "id", rec.ID, "error", err)
	}

	slog.Info("extension allow-list applied", "id", rec.ID, "name", t.Name(),
		"selected", matched, "of", len(files))
}

// extensionAllowed reports whether a path's extension is in the allow-list.
func (e *Engine) extensionAllowed(path string) bool {
	ext := strings.ToLower(strings.TrimPrefix(filepath.Ext(path), "."))
	if ext == "" {
		return false
	}
	_, ok := e.allowedExtensions[ext]
	return ok
}

// applySelection maps a per-file boolean selection onto piece priorities.
func applySelection(t *torrent.Torrent, selection []bool) error {
	files := t.Files()
	if len(selection) != len(files) {
		return fmt.Errorf("selection covers %d files but the torrent has %d", len(selection), len(files))
	}
	for i, f := range files {
		if selection[i] {
			f.SetPriority(torrent.PiecePriorityNormal)
		} else {
			f.SetPriority(torrent.PiecePriorityNone)
		}
	}
	return nil
}

// SetLabels replaces a torrent's labels and persists them. Sonarr relabels a
// torrent when it imports one, so this is a live path and not a one-off at add
// time.
func (e *Engine) SetLabels(ctx context.Context, id int64, labels []string) error {
	rec := e.record(id)
	if rec == nil {
		return fmt.Errorf("no torrent with id %d", id)
	}
	if err := e.store.SetLabels(ctx, id, labels); err != nil {
		return err
	}
	// Read back rather than trusting the argument: the store drops blanks and
	// duplicates and sorts what is left, and a client polling torrent-get
	// straight afterwards must see exactly what was kept.
	stored, err := e.store.Labels(ctx, id)
	if err != nil {
		return err
	}
	e.mu.Lock()
	rec.Labels = stored
	e.mu.Unlock()
	slog.Info("torrent labels set", "id", id, "labels", stored)
	return nil
}

// SetSeedLimits records a torrent's own seed caps and persists them.
//
// The caps are enforced by the seed monitor, and are also reported back through
// torrent-get: Sonarr decides for itself when a download has seeded enough and
// reads them back to do it, so storing without reporting would be half a
// feature.
func (e *Engine) SetSeedLimits(ctx context.Context, id int64, limits store.SeedLimits) error {
	rec := e.record(id)
	if rec == nil {
		return fmt.Errorf("no torrent with id %d", id)
	}
	if err := e.store.SetSeedLimits(ctx, id, limits); err != nil {
		return err
	}
	e.mu.Lock()
	rec.SeedLimits = limits
	e.mu.Unlock()
	slog.Info("torrent seed limits set", "id", id,
		"ratio_limit", limits.RatioLimit, "ratio_mode", limits.RatioMode,
		"idle_limit_minutes", limits.IdleLimit, "idle_mode", limits.IdleMode)
	return nil
}

// SetFileSelection applies and persists a per-file selection.
func (e *Engine) SetFileSelection(ctx context.Context, id int64, selection []bool) error {
	rec := e.record(id)
	if rec == nil {
		return fmt.Errorf("no torrent with id %d", id)
	}
	if rec.Torrent.Info() == nil {
		return errors.New("the torrent's metadata has not arrived yet")
	}
	if err := applySelection(rec.Torrent, selection); err != nil {
		return err
	}
	return e.store.SetFileSelection(ctx, id, selection)
}

// Stop pauses a torrent. anacrolix/torrent has no pause, so this is emulated
// and the flag we set is the authoritative record of it.
func (e *Engine) Stop(ctx context.Context, id int64) error {
	rec := e.record(id)
	if rec == nil {
		return fmt.Errorf("no torrent with id %d", id)
	}
	e.mu.Lock()
	rec.Paused = true
	e.mu.Unlock()
	e.applyPaused(rec)
	slog.Info("torrent stopped", "id", id)
	// Stopping frees a download slot for whatever is waiting behind it.
	defer e.reconcileQueue()
	return e.store.SetPaused(ctx, id, true)
}

// Start resumes a torrent.
func (e *Engine) Start(ctx context.Context, id int64) error {
	rec := e.record(id)
	if rec == nil {
		return fmt.Errorf("no torrent with id %d", id)
	}
	e.mu.Lock()
	rec.Paused = false
	e.mu.Unlock()
	e.resume(rec)
	slog.Info("torrent started", "id", id)
	// Starting may mean this torrent has to wait its turn, or that it takes a
	// slot from nothing - reconcile decides which, and undoes the resume above
	// if the queue is full.
	defer e.reconcileQueue()
	return e.store.SetPaused(ctx, id, false)
}

// applyPaused stops a torrent by making nothing wanted.
//
// Deliberately NOT Torrent.DisallowDataDownload(), which is the obvious call
// and which crashes the daemon. Setting that flag makes Piece.ignoreForRequests
// return true for every piece (piece.go:303) without updating
// t._pendingPieces, so the library's own consistency check -
// checkPendingPiecesMatchesRequestOrder, reached from the piece hasher and the
// tracker announce dispatcher - finds an empty request order against a full
// pending set and panics the process. torrent-stop is routine, so that is a
// crash on a path Sonarr and Radarr exercise constantly.
//
// Priority changes go through updatePiecePriorities, which maintains both
// structures together, so "no piece is wanted" is the representation of paused
// that the library actually supports. The selection is persisted, so resuming
// restores exactly what was wanted before.
func (e *Engine) applyPaused(rec *record) {
	if rec.Torrent.Info() != nil {
		for _, f := range rec.Torrent.Files() {
			f.SetPriority(torrent.PiecePriorityNone)
		}
	}
	// Upload is a separate flag with no piece bookkeeping behind it, so this
	// one is safe.
	rec.Torrent.DisallowDataUpload()
	rec.Torrent.SetMaxEstablishedConns(0)
}

// resume reapplies the wanted selection. Before metadata there is nothing to
// apply and the watcher does it when the info dict arrives.
func (e *Engine) resume(rec *record) {
	rec.Torrent.AllowDataUpload()
	rec.Torrent.SetMaxEstablishedConns(e.maxPeers)
	if rec.Torrent.Info() == nil {
		return
	}
	if err := applySelection(rec.Torrent, e.desiredSelection(rec)); err != nil {
		slog.Warn("failed to apply the file selection on resume", "id", rec.ID, "error", err)
	}
}

// desiredSelection is what the torrent should be downloading when running: the
// persisted selection, or everything when none was ever recorded.
func (e *Engine) desiredSelection(rec *record) []bool {
	files := rec.Torrent.Files()
	selection, err := e.store.FileSelection(e.ctx, rec.ID)
	if err != nil {
		slog.Warn("failed to read the persisted file selection", "id", rec.ID, "error", err)
	}
	if len(selection) == len(files) {
		return selection
	}
	all := make([]bool, len(files))
	for i := range all {
		all[i] = true
	}
	return all
}

func (e *Engine) setError(rec *record, msg string) {
	e.mu.Lock()
	rec.Error = msg
	e.mu.Unlock()
	if err := e.store.SetError(e.ctx, rec.ID, msg); err != nil {
		slog.Warn("failed to record the torrent error", "id", rec.ID, "error", err)
	}
}

// Remove drops a torrent, optionally deleting its data.
//
// The order is not negotiable. Torrent.Drop() does not delete data - its doc
// is explicit that "no data corruption can, or should occur to either the
// torrent's data" - so deletion is ours to do, and it must happen after the
// drop: on Windows an open handle makes the unlink fail outright. The file
// list has to be snapshotted before both, because Files() is unusable after
// Drop and panics before metadata.
func (e *Engine) Remove(ctx context.Context, id int64, deleteData bool) error {
	rec := e.record(id)
	if rec == nil {
		return fmt.Errorf("no torrent with id %d", id)
	}

	var paths []string
	if deleteData && rec.Torrent.Info() != nil {
		for _, f := range rec.Torrent.Files() {
			paths = append(paths, f.Path())
		}
	}

	infoHash := rec.Torrent.InfoHash()
	rec.Torrent.Drop()

	e.mu.Lock()
	delete(e.records, id)
	e.mu.Unlock()

	if deleteData {
		e.deleteData(rec.SavePath, paths)
	}
	// Completion must go too: left behind, a later re-add of the same
	// infohash would claim pieces that are no longer on disk.
	e.completion.Forget(infoHash)
	slog.Info("torrent removed", "id", id, "delete_data", deleteData)
	return e.store.Delete(ctx, id)
}

// deleteData removes a torrent's files and the directories it created, all
// constrained to its recorded save path.
func (e *Engine) deleteData(savePath string, paths []string) {
	root := filepath.Clean(savePath)
	dirs := make(map[string]struct{})
	for _, p := range paths {
		full := filepath.Clean(filepath.Join(root, p))
		if !underRoot(root, full) {
			slog.Warn("refusing to delete a path outside the torrent's save path", "path", full, "root", root)
			continue
		}
		if err := os.Remove(full); err != nil && !os.IsNotExist(err) {
			slog.Warn("failed to delete a torrent file", "path", full, "error", err)
			continue
		}
		slog.Info("deleted torrent file", "path", full)
		for dir := filepath.Dir(full); underRoot(root, dir) && dir != root; dir = filepath.Dir(dir) {
			dirs[dir] = struct{}{}
		}
	}

	// Prune the directories the torrent created, deepest first, and only
	// while they are empty. Never the save path itself.
	for len(dirs) > 0 {
		deepest := ""
		for dir := range dirs {
			if len(dir) > len(deepest) {
				deepest = dir
			}
		}
		delete(dirs, deepest)
		if err := os.Remove(deepest); err == nil {
			slog.Info("removed empty torrent directory", "path", deepest)
		}
	}
}

// underRoot reports whether path is root or inside it, after cleaning. It is
// a lexical check: symlinks are not followed, and a path that escapes via one
// is rejected by the caller deleting only what the torrent declared.
func underRoot(root, path string) bool {
	rel, err := filepath.Rel(root, path)
	if err != nil {
		return false
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// Restore re-adds every persisted torrent, reapplying its paused flag and file
// selection. Torrents restored this way are exempt from the extension filter:
// their selection was already decided, and re-running the filter would
// overwrite a client's later choice.
func (e *Engine) Restore(ctx context.Context) error {
	saved, err := e.store.List(ctx)
	if err != nil {
		return err
	}
	var errs []error
	for _, s := range saved {
		if err := e.restoreOne(ctx, s); err != nil {
			errs = append(errs, fmt.Errorf("torrent %d (%s): %w", s.ID, s.Name, err))
		}
	}
	slog.Info("restored torrents", "count", len(saved)-len(errs), "failed", len(errs))
	// Once, after the whole list is back: restoring ten torrents into a
	// five-deep queue must leave five of them waiting, not all ten running.
	e.reconcileQueue()
	return errors.Join(errs...)
}

func (e *Engine) restoreOne(ctx context.Context, s store.Torrent) error {
	req := AddRequest{Source: s.Source, Metainfo: s.Metainfo, DownloadDir: s.SavePath, Paused: s.Paused}
	spec, err := e.spec(ctx, req)
	if err != nil {
		return err
	}
	spec.Storage = storage.NewFileWithCompletion(s.SavePath, e.completion)
	if s.Paused {
		spec.DisallowDataUpload = true
	}
	t, _, err := e.client.AddTorrentSpec(spec)
	if err != nil {
		return err
	}

	rec := &record{
		ID: s.ID, Torrent: t, Source: s.Source, SavePath: s.SavePath,
		Paused: s.Paused, AddedAt: s.AddedAt, FinishedAt: s.FinishedAt,
		Error: s.Error, Filtered: s.Filtered, storedFinishedAt: s.FinishedAt,
		Labels:        s.Labels,
		SeedLimits:    s.SeedLimits,
		QueuePosition: s.QueuePosition,
		ready:         make(chan struct{}),
	}
	e.mu.Lock()
	e.records[s.ID] = rec
	e.mu.Unlock()

	if s.Paused {
		e.applyPaused(rec)
	} else {
		rec.Torrent.SetMaxEstablishedConns(e.maxPeers)
	}
	// filtering=false: a restored torrent's selection was already decided.
	e.watch(rec, false)
	return nil
}

// record returns the record for a gid, or nil.
func (e *Engine) record(id int64) *record {
	e.mu.RLock()
	defer e.mu.RUnlock()
	return e.records[id]
}

// records snapshot, for iteration without holding the lock.
func (e *Engine) snapshot() []*record {
	e.mu.RLock()
	defer e.mu.RUnlock()
	out := make([]*record, 0, len(e.records))
	for _, rec := range e.records {
		out = append(out, rec)
	}
	return out
}

// normaliseExtensions lowercases the allow-list and strips leading dots, so
// `mkv` and `.MKV` both work.
func normaliseExtensions(exts []string) map[string]struct{} {
	if len(exts) == 0 {
		return nil
	}
	out := make(map[string]struct{}, len(exts))
	for _, ext := range exts {
		ext = strings.ToLower(strings.TrimPrefix(strings.TrimSpace(ext), "."))
		if ext != "" {
			out[ext] = struct{}{}
		}
	}
	return out
}
