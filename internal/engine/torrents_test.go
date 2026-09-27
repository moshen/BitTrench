package engine

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/anacrolix/torrent"
	"github.com/anacrolix/torrent/bencode"
	"github.com/anacrolix/torrent/metainfo"
)

// buildTorrent writes a small multi-file torrent to disk and returns its
// metainfo bytes plus the directory holding the data, so the engine can be
// exercised end to end without a swarm.
func buildTorrent(t *testing.T, files map[string]string) ([]byte, string) {
	t.Helper()
	root := t.TempDir()
	content := filepath.Join(root, "release")
	if err := os.MkdirAll(content, 0o755); err != nil {
		t.Fatal(err)
	}
	for name, body := range files {
		path := filepath.Join(content, name)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	info := metainfo.Info{PieceLength: 16 << 10}
	if err := info.BuildFromFilePath(content); err != nil {
		t.Fatalf("BuildFromFilePath: %v", err)
	}
	infoBytes, err := bencode.Marshal(info)
	if err != nil {
		t.Fatalf("bencode info: %v", err)
	}
	mi := metainfo.MetaInfo{InfoBytes: infoBytes}
	mi.SetDefaults()
	var buf bytes.Buffer
	if err := mi.Write(&buf); err != nil {
		t.Fatalf("write metainfo: %v", err)
	}
	return buf.Bytes(), root
}

// The extension allow-list is a product decision: only files whose extension is
// listed are downloaded.
func TestExtensionAllowListSelectsOnlyMatchingFiles(t *testing.T) {
	mi, dataDir := buildTorrent(t, map[string]string{
		"movie.mkv":  "video data here",
		"sample.avi": "sample data",
		"readme.nfo": "notes",
	})

	h := newHarness(t, func(c *cfgOpts) {
		c.allowedExtensions = []string{"mkv", ".MP4"}
		c.savePath = dataDir
	})
	id, _, err := h.engine.Add(context.Background(), AddRequest{Metainfo: mi})
	if err != nil {
		t.Fatalf("Add: %v", err)
	}

	h.waitForMetadata(t, id)
	files := h.engine.Files(id)
	if len(files) != 3 {
		t.Fatalf("expected 3 files, got %d", len(files))
	}
	for _, f := range files {
		want := filepath.Ext(f.Path) == ".mkv"
		if f.Selected != want {
			t.Errorf("%s selected = %v, want %v", f.Path, f.Selected, want)
		}
	}

	// The selection must be persisted, or a restart downloads everything.
	selection, err := h.store.FileSelection(context.Background(), id)
	if err != nil {
		t.Fatalf("FileSelection: %v", err)
	}
	if len(selection) != 3 {
		t.Fatalf("persisted selection covers %d files, want 3", len(selection))
	}
}

// A torrent whose files all fall outside the allow-list is reported as failed
// and never starts downloading.
func TestExtensionAllowListRejectsATorrentWithNoMatches(t *testing.T) {
	mi, dataDir := buildTorrent(t, map[string]string{"readme.nfo": "notes"})

	h := newHarness(t, func(c *cfgOpts) {
		c.allowedExtensions = []string{"mkv"}
		c.savePath = dataDir
	})
	id, _, err := h.engine.Add(context.Background(), AddRequest{Metainfo: mi})
	if err != nil {
		t.Fatalf("Add: %v", err)
	}
	h.waitForMetadata(t, id)

	// The error is recorded asynchronously, right after metadata arrives.
	deadline := time.Now().Add(5 * time.Second)
	var status Status
	for time.Now().Before(deadline) {
		status, _ = h.engine.Status(id)
		if status.Error != "" {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if status.Error == "" {
		t.Fatal("a torrent matching no allowed extension should record an error")
	}
	if status.State != StateError {
		t.Errorf("state = %q, want %q", status.State, StateError)
	}
	// And nothing may be selected: resuming with everything selected is the
	// exact "allow-list ignored" bug the filter exists to prevent.
	for _, f := range h.engine.Files(id) {
		if f.Selected {
			t.Errorf("%s was selected despite matching no allowed extension", f.Path)
		}
	}
}

// Pause is engine-owned state, since anacrolix/torrent has none, and it must
// survive a restart.
func TestPausedStateIsOwnedAndPersisted(t *testing.T) {
	mi, dataDir := buildTorrent(t, map[string]string{"movie.mkv": "video"})
	h := newHarness(t, func(c *cfgOpts) { c.savePath = dataDir })
	ctx := context.Background()

	id, _, err := h.engine.Add(ctx, AddRequest{Metainfo: mi})
	if err != nil {
		t.Fatalf("Add: %v", err)
	}
	if s, _ := h.engine.Status(id); s.Paused {
		t.Error("a torrent added unpaused reports paused")
	}

	if err := h.engine.Stop(ctx, id); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	s, _ := h.engine.Status(id)
	if !s.Paused || s.State != StateStopped {
		t.Errorf("after Stop: paused=%v state=%q", s.Paused, s.State)
	}
	saved, err := h.store.List(ctx)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(saved) != 1 || !saved[0].Paused {
		t.Error("the paused flag was not persisted")
	}

	if err := h.engine.Start(ctx, id); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if s, _ := h.engine.Status(id); s.Paused {
		t.Error("Start did not clear the paused flag")
	}
	saved, _ = h.store.List(ctx)
	if saved[0].Paused {
		t.Error("clearing the paused flag was not persisted")
	}
}

// Adding paused must not download anything in the window before the pause is
// applied, which is why it goes on the add itself.
func TestAddPausedStartsStopped(t *testing.T) {
	mi, dataDir := buildTorrent(t, map[string]string{"movie.mkv": "video"})
	h := newHarness(t, func(c *cfgOpts) { c.savePath = dataDir })

	id, _, err := h.engine.Add(context.Background(), AddRequest{Metainfo: mi, Paused: true})
	if err != nil {
		t.Fatalf("Add: %v", err)
	}
	s, ok := h.engine.Status(id)
	if !ok || !s.Paused || s.State != StateStopped {
		t.Errorf("add paused gave paused=%v state=%q", s.Paused, s.State)
	}
}

// The gid, the paused flag and the file selection must all come back.
func TestRestoreBringsTorrentsBack(t *testing.T) {
	mi, dataDir := buildTorrent(t, map[string]string{"movie.mkv": "video", "extra.nfo": "notes"})
	ctx := context.Background()

	h := newHarness(t, func(c *cfgOpts) { c.savePath = dataDir })
	id, _, err := h.engine.Add(ctx, AddRequest{Metainfo: mi, Paused: true})
	if err != nil {
		t.Fatalf("Add: %v", err)
	}
	h.waitForMetadata(t, id)
	// Select by path: the info dict orders files itself, and hardcoding an
	// index makes the test assert the wrong thing when that order changes.
	files := h.engine.Files(id)
	selection := make([]bool, len(files))
	for i, f := range files {
		selection[i] = filepath.Ext(f.Path) == ".mkv"
	}
	if err := h.engine.SetFileSelection(ctx, id, selection); err != nil {
		t.Fatalf("SetFileSelection: %v", err)
	}

	restored := h.restart(t)
	if err := restored.Restore(ctx); err != nil {
		t.Fatalf("Restore: %v", err)
	}

	s, ok := restored.Status(id)
	if !ok {
		t.Fatalf("gid %d did not come back", id)
	}
	if !s.Paused {
		t.Error("the paused flag did not survive the restart")
	}
	if s.SavePath != dataDir {
		t.Errorf("save path = %q, want %q", s.SavePath, dataDir)
	}

	waitForMetadata(t, restored, id)
	restoredFiles := restored.Files(id)
	if len(restoredFiles) != 2 {
		t.Fatalf("expected 2 files after restore, got %d", len(restoredFiles))
	}
	// The torrent came back paused, and paused means nothing is wanted - so
	// the selection is recorded but not applied yet.
	for _, f := range restoredFiles {
		if f.Selected {
			t.Errorf("after restoring a paused torrent %s is already wanted", f.Path)
		}
	}
	saved, err := h.store.FileSelection(ctx, id)
	if err != nil {
		t.Fatalf("FileSelection: %v", err)
	}
	if len(saved) != 2 {
		t.Fatalf("the persisted selection covers %d files, want 2", len(saved))
	}

	// Starting it applies exactly what was persisted.
	if err := restored.Start(ctx, id); err != nil {
		t.Fatalf("Start: %v", err)
	}
	for _, f := range restored.Files(id) {
		want := filepath.Ext(f.Path) == ".mkv"
		if f.Selected != want {
			t.Errorf("after restore and start %s selected = %v, want %v", f.Path, f.Selected, want)
		}
	}
}

// Radarr polls torrent-get with fields:["files"] immediately after
// torrent-add, and Torrent.Files() dereferences a nil pointer before the info
// dict resolves. Nothing on this path may panic.
func TestMetadataDependentCallsAreSafeBeforeInfo(t *testing.T) {
	h := newHarness(t, nil)
	id, _, err := h.engine.Add(context.Background(), AddRequest{
		Source: "magnet:?xt=urn:btih:0000000000000000000000000000000000000001&dn=pending",
	})
	if err != nil {
		t.Fatalf("Add: %v", err)
	}

	s, ok := h.engine.Status(id)
	if !ok {
		t.Fatal("Status on a fresh magnet returned nothing")
	}
	if s.HasMetadata {
		t.Fatal("a fresh magnet reported metadata")
	}
	if s.Name == "" {
		t.Error("Name should fall back to the magnet's display name")
	}
	if s.State != StateInitialising {
		t.Errorf("state = %q, want %q", s.State, StateInitialising)
	}
	if s.TotalBytes != 0 || s.PieceCount != 0 {
		t.Errorf("metadata-dependent fields are non-zero: %+v", s)
	}
	if files := h.engine.Files(id); files != nil {
		t.Errorf("Files before metadata = %v, want nil", files)
	}
	if bits, pieces := h.engine.Bitfield(id); bits != nil || pieces != 0 {
		t.Errorf("Bitfield before metadata = %v, %d", bits, pieces)
	}
	if _, err := h.engine.List(), error(nil); err != nil {
		t.Fatal(err)
	}
}

// Torrent.Drop() explicitly does not delete data, so removal has to.
func TestRemoveDeletesDataAndPrunesDirectories(t *testing.T) {
	mi, dataDir := buildTorrent(t, map[string]string{
		"movie.mkv":      "video",
		"subs/movie.srt": "subtitles",
	})
	ctx := context.Background()
	h := newHarness(t, func(c *cfgOpts) { c.savePath = dataDir })

	id, _, err := h.engine.Add(ctx, AddRequest{Metainfo: mi, Paused: true})
	if err != nil {
		t.Fatalf("Add: %v", err)
	}
	h.waitForMetadata(t, id)

	movie := filepath.Join(dataDir, "release", "movie.mkv")
	subs := filepath.Join(dataDir, "release", "subs")
	if _, err := os.Stat(movie); err != nil {
		t.Fatalf("the test data is missing: %v", err)
	}

	if err := h.engine.Remove(ctx, id, true); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if _, err := os.Stat(movie); !os.IsNotExist(err) {
		t.Errorf("the torrent data was not deleted: %v", err)
	}
	if _, err := os.Stat(subs); !os.IsNotExist(err) {
		t.Errorf("an emptied torrent directory was not pruned: %v", err)
	}
	// Never the save path itself.
	if _, err := os.Stat(dataDir); err != nil {
		t.Errorf("the save path was deleted: %v", err)
	}
	if _, ok := h.engine.Status(id); ok {
		t.Error("the torrent is still listed after removal")
	}
	if saved, _ := h.store.List(ctx); len(saved) != 0 {
		t.Errorf("%d rows survived removal", len(saved))
	}
}

// Removing without delete-local-data must leave the files alone.
func TestRemoveKeepsDataWhenNotAsked(t *testing.T) {
	mi, dataDir := buildTorrent(t, map[string]string{"movie.mkv": "video"})
	ctx := context.Background()
	h := newHarness(t, func(c *cfgOpts) { c.savePath = dataDir })

	id, _, err := h.engine.Add(ctx, AddRequest{Metainfo: mi, Paused: true})
	if err != nil {
		t.Fatalf("Add: %v", err)
	}
	h.waitForMetadata(t, id)
	if err := h.engine.Remove(ctx, id, false); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dataDir, "release", "movie.mkv")); err != nil {
		t.Errorf("data was deleted without delete-local-data: %v", err)
	}
}

// Deletion is the only data-destroying path in the daemon, so it is
// constrained to the torrent's own save path.
func TestUnderRootRejectsEscapes(t *testing.T) {
	root := filepath.Clean("/data/torrents")
	for _, c := range []struct {
		path string
		want bool
	}{
		{filepath.Join(root, "movie.mkv"), true},
		{filepath.Join(root, "subs", "movie.srt"), true},
		{root, true},
		{filepath.Clean("/data/other/movie.mkv"), false},
		{filepath.Clean("/data/torrents/../etc/passwd"), false},
		{filepath.Clean("/"), false},
	} {
		if got := underRoot(root, c.path); got != c.want {
			t.Errorf("underRoot(%q, %q) = %v, want %v", root, c.path, got, c.want)
		}
	}
}

func TestExtensionMatchingIsCaseInsensitiveAndDotOptional(t *testing.T) {
	e := &Engine{allowedExtensions: normaliseExtensions([]string{"mkv", ".MP4", " srt "})}
	for _, c := range []struct {
		path string
		want bool
	}{
		{"Movie.mkv", true},
		{"Movie.MKV", true},
		{"Movie.mp4", true},
		{"Movie.srt", true},
		{"Movie.avi", false},
		{"README", false},
		{"archive.tar.gz", false},
	} {
		if got := e.extensionAllowed(c.path); got != c.want {
			t.Errorf("extensionAllowed(%q) = %v, want %v", c.path, got, c.want)
		}
	}
}

// An empty allow-list disables the filter entirely - all files download.
func TestEmptyAllowListDisablesTheFilter(t *testing.T) {
	mi, dataDir := buildTorrent(t, map[string]string{"movie.mkv": "v", "readme.nfo": "n"})
	h := newHarness(t, func(c *cfgOpts) { c.savePath = dataDir })
	id, _, err := h.engine.Add(context.Background(), AddRequest{Metainfo: mi})
	if err != nil {
		t.Fatalf("Add: %v", err)
	}
	h.waitForMetadata(t, id)
	for _, f := range h.engine.Files(id) {
		if !f.Selected {
			t.Errorf("%s was skipped with no allow-list configured", f.Path)
		}
	}
	if s, _ := h.engine.Status(id); s.Error != "" {
		t.Errorf("unexpected error with no allow-list: %s", s.Error)
	}
}

func TestDownloadDirMustBeAbsolute(t *testing.T) {
	h := newHarness(t, nil)
	_, err := h.engine.resolveDownloadDir("relative/path")
	if err == nil {
		t.Fatal("a relative download-dir should be rejected")
	}
	dir := filepath.Join(t.TempDir(), "new", "nested")
	got, err := h.engine.resolveDownloadDir(dir)
	if err != nil {
		t.Fatalf("resolveDownloadDir: %v", err)
	}
	if got != dir {
		t.Errorf("resolveDownloadDir = %q, want %q", got, dir)
	}
	if _, err := os.Stat(dir); err != nil {
		t.Errorf("the download directory was not created: %v", err)
	}
}

var _ = torrent.PiecePriorityNone

// Pause must be expressed as "no piece is wanted", never as
// Torrent.DisallowDataDownload().
//
// That call is the obvious one and it crashes the daemon: it makes
// Piece.ignoreForRequests return true for every piece (piece.go:303) without
// updating t._pendingPieces, so the library's own consistency check -
// checkPendingPiecesMatchesRequestOrder, reached from the piece hasher and the
// tracker announce dispatcher - sees an empty request order against a full
// pending set and panics the process. torrent-stop is routine, so that is a
// crash on a path the *arr clients exercise constantly.
//
// The panic is amortized and so not reliably reproducible in a test; what is
// testable is the invariant that avoids it. If someone reintroduces the
// DisallowDataDownload approach, priorities stop being cleared and this fails.
func TestPauseClearsPiecePrioritiesRatherThanDisallowingDownload(t *testing.T) {
	mi, dataDir := buildTorrent(t, map[string]string{"movie.mkv": "video", "extra.nfo": "notes"})
	ctx := context.Background()
	h := newHarness(t, func(c *cfgOpts) { c.savePath = dataDir })

	id, _, err := h.engine.Add(ctx, AddRequest{Metainfo: mi})
	if err != nil {
		t.Fatalf("Add: %v", err)
	}
	h.waitForMetadata(t, id)
	for _, f := range h.engine.Files(id) {
		if !f.Selected {
			t.Fatalf("%s should be selected while running", f.Path)
		}
	}

	if err := h.engine.Stop(ctx, id); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	for _, f := range h.engine.Files(id) {
		if f.Selected {
			t.Errorf("%s is still wanted after Stop; pause must clear piece priorities", f.Path)
		}
	}
	rec := h.engine.record(id)
	for _, f := range rec.Torrent.Files() {
		if f.Priority() != torrent.PiecePriorityNone {
			t.Errorf("%s priority = %v after Stop, want None", f.Path(), f.Priority())
		}
	}

	if err := h.engine.Start(ctx, id); err != nil {
		t.Fatalf("Start: %v", err)
	}
	for _, f := range h.engine.Files(id) {
		if !f.Selected {
			t.Errorf("%s was not restored on Start", f.Path)
		}
	}
}

// Stopping and starting must restore the *persisted* selection, not simply
// select everything - otherwise pausing a torrent filtered by the allow-list
// and resuming it would quietly start downloading the files it excluded.
func TestResumeRestoresTheSelectionRatherThanEverything(t *testing.T) {
	mi, dataDir := buildTorrent(t, map[string]string{"movie.mkv": "video", "extra.nfo": "notes"})
	ctx := context.Background()
	h := newHarness(t, func(c *cfgOpts) {
		c.savePath = dataDir
		c.allowedExtensions = []string{"mkv"}
	})

	id, _, err := h.engine.Add(ctx, AddRequest{Metainfo: mi})
	if err != nil {
		t.Fatalf("Add: %v", err)
	}
	h.waitForMetadata(t, id)
	if err := h.engine.Stop(ctx, id); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if err := h.engine.Start(ctx, id); err != nil {
		t.Fatalf("Start: %v", err)
	}
	for _, f := range h.engine.Files(id) {
		want := filepath.Ext(f.Path) == ".mkv"
		if f.Selected != want {
			t.Errorf("after a stop/start cycle %s selected = %v, want %v", f.Path, f.Selected, want)
		}
	}
}

// A torrent added paused must want nothing at all - not even briefly.
func TestPausedAddWantsNothing(t *testing.T) {
	mi, dataDir := buildTorrent(t, map[string]string{"movie.mkv": "video"})
	h := newHarness(t, func(c *cfgOpts) { c.savePath = dataDir })

	id, _, err := h.engine.Add(context.Background(), AddRequest{Metainfo: mi, Paused: true})
	if err != nil {
		t.Fatalf("Add: %v", err)
	}
	h.waitForMetadata(t, id)
	for _, f := range h.engine.Files(id) {
		if f.Selected {
			t.Errorf("%s is wanted on a paused add", f.Path)
		}
	}
}

// SelectedBytes is what the one-shot download waits on, so it has to be true
// for a filtered torrent - the case Status cannot express. The data is already
// on disk here, so the hash check completes it without a swarm.
func TestSelectedBytesCountsOnlyTheSelectedFiles(t *testing.T) {
	mi, dataDir := buildTorrent(t, map[string]string{
		"movie.mkv":  "video data here",
		"sample.avi": "sample data",
		"readme.nfo": "notes",
	})

	h := newHarness(t, func(c *cfgOpts) {
		c.allowedExtensions = []string{"mkv"}
		c.savePath = dataDir
	})
	id, _, err := h.engine.Add(context.Background(), AddRequest{Metainfo: mi})
	if err != nil {
		t.Fatalf("Add: %v", err)
	}
	h.waitForMetadata(t, id)

	var wantTotal int64
	for _, f := range h.engine.Files(id) {
		if filepath.Ext(f.Path) == ".mkv" {
			wantTotal = f.Length
		}
	}
	if wantTotal == 0 {
		t.Fatal("the fixture has no .mkv file to select")
	}

	// The engine hashes what is already on disk, so wait for the selected
	// files to read as complete rather than assuming the check has finished.
	deadline := time.Now().Add(15 * time.Second)
	var completed, total int64
	var files int
	for {
		completed, total, files = h.engine.SelectedBytes(id)
		if files > 0 && completed >= total {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("selected bytes stalled at %d/%d over %d files", completed, total, files)
		}
		time.Sleep(50 * time.Millisecond)
	}

	if files != 1 {
		t.Errorf("selected files = %d, want 1 (only the .mkv)", files)
	}
	if total != wantTotal {
		t.Errorf("selected total = %d, want %d (the .mkv alone)", total, wantTotal)
	}

	// The point of the method: the selected view is narrower than the whole
	// torrent, which is the number Status reports. Status.MissingBytes is not
	// asserted on here - this fixture's three files are all already on disk and
	// small enough to share a single piece, so the hash check completes them
	// whether they were selected or not. What makes MissingBytes unusable is a
	// real swarm, where the deselected bytes never arrive.
	st, ok := h.engine.Status(id)
	if !ok {
		t.Fatal("no status for the torrent")
	}
	if st.TotalBytes <= total {
		t.Errorf("torrent total %d should exceed the selected total %d", st.TotalBytes, total)
	}
}

// Before metadata arrives there is nothing selected yet, and a zero file count
// is how a caller tells that from "all of it is done".
func TestSelectedBytesReportsNoFilesBeforeMetadata(t *testing.T) {
	h := newHarness(t, nil)
	id, _, err := h.engine.Add(context.Background(), AddRequest{
		Source: "magnet:?xt=urn:btih:0123456789abcdef0123456789abcdef01234567",
	})
	if err != nil {
		t.Fatalf("Add: %v", err)
	}
	if completed, total, files := h.engine.SelectedBytes(id); files != 0 || completed != 0 || total != 0 {
		t.Errorf("SelectedBytes = (%d, %d, %d), want all zero before metadata", completed, total, files)
	}
	if _, _, files := h.engine.SelectedBytes(4242); files != 0 {
		t.Errorf("an unknown gid reported %d selected files, want 0", files)
	}
}

// Labels are engine-owned state that must survive a restart, like the paused
// flag and the file selection: Sonarr and Radarr filter their queue by the
// category they put there, so a torrent that loses its label drops out of it.
func TestLabelsSurviveARestart(t *testing.T) {
	mi, dataDir := buildTorrent(t, map[string]string{"movie.mkv": "data"})
	h := newHarness(t, func(c *cfgOpts) { c.savePath = dataDir })
	ctx := context.Background()

	id, _, err := h.engine.Add(ctx, AddRequest{Metainfo: mi, Labels: []string{"tv-sonarr"}})
	if err != nil {
		t.Fatalf("Add: %v", err)
	}
	if st, _ := h.engine.Status(id); len(st.Labels) != 1 || st.Labels[0] != "tv-sonarr" {
		t.Fatalf("labels after add = %v, want [tv-sonarr]", st.Labels)
	}

	// Sonarr relabels on import, so the live path matters as much as the add.
	if err := h.engine.SetLabels(ctx, id, []string{"tv-sonarr-imported"}); err != nil {
		t.Fatalf("SetLabels: %v", err)
	}

	e := h.restart(t)
	if err := e.Restore(ctx); err != nil {
		t.Fatalf("Restore: %v", err)
	}
	st, ok := e.Status(id)
	if !ok {
		t.Fatal("the torrent did not come back")
	}
	if len(st.Labels) != 1 || st.Labels[0] != "tv-sonarr-imported" {
		t.Errorf("labels after restart = %v, want [tv-sonarr-imported]", st.Labels)
	}
}

// Status must not hand out the record's live slice; SetLabels replaces it
// wholesale, and a caller holding the old one would otherwise observe a write.
func TestStatusLabelsAreACopy(t *testing.T) {
	mi, dataDir := buildTorrent(t, map[string]string{"movie.mkv": "data"})
	h := newHarness(t, func(c *cfgOpts) { c.savePath = dataDir })
	ctx := context.Background()

	id, _, err := h.engine.Add(ctx, AddRequest{Metainfo: mi, Labels: []string{"original"}})
	if err != nil {
		t.Fatalf("Add: %v", err)
	}
	st, _ := h.engine.Status(id)
	st.Labels[0] = "mutated"

	again, _ := h.engine.Status(id)
	if again.Labels[0] != "original" {
		t.Errorf("mutating a returned label changed the engine's copy: %v", again.Labels)
	}
}
