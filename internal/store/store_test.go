package store

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/anacrolix/torrent/metainfo"
)

func open(t *testing.T) (*Store, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "state.db")
	s, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s, path
}

func hash(b byte) metainfo.Hash {
	var h metainfo.Hash
	h[0] = b
	return h
}

// The gid must be stable across restarts and must not be reused after a
// removal: Sonarr and Radarr key their downloads on it and lose track of one
// whose id moves.
func TestGidsAreStableAndNotReused(t *testing.T) {
	ctx := context.Background()
	s, path := open(t)

	first, err := s.Put(ctx, &Torrent{InfoHash: hash(1), SavePath: "/data"})
	if err != nil {
		t.Fatalf("Put: %v", err)
	}
	second, err := s.Put(ctx, &Torrent{InfoHash: hash(2), SavePath: "/data"})
	if err != nil {
		t.Fatalf("Put: %v", err)
	}
	if first == second {
		t.Fatalf("two torrents got the same gid %d", first)
	}

	// Re-adding a known infohash returns the same gid rather than a new row.
	again, err := s.Put(ctx, &Torrent{InfoHash: hash(1), SavePath: "/elsewhere"})
	if err != nil {
		t.Fatalf("Put: %v", err)
	}
	if again != first {
		t.Errorf("re-adding infohash 1 returned gid %d, want %d", again, first)
	}

	if err := s.Delete(ctx, second); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	third, err := s.Put(ctx, &Torrent{InfoHash: hash(3), SavePath: "/data"})
	if err != nil {
		t.Fatalf("Put: %v", err)
	}
	if third == second {
		t.Errorf("gid %d was reused after the torrent was removed", third)
	}

	// And they survive a reopen.
	s.Close()
	reopened, err := Open(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer reopened.Close()
	got, err := reopened.IDByInfoHash(ctx, hash(1))
	if err != nil {
		t.Fatalf("IDByInfoHash after reopen: %v", err)
	}
	if got != first {
		t.Errorf("gid after reopen = %d, want %d", got, first)
	}
}

func TestTorrentStateRoundTrips(t *testing.T) {
	ctx := context.Background()
	s, path := open(t)

	added := time.Now().Add(-time.Hour).Truncate(time.Second)
	id, err := s.Put(ctx, &Torrent{
		InfoHash: hash(7),
		Source:   "magnet:?xt=urn:btih:07",
		SavePath: "/data/tv",
		Paused:   true,
		AddedAt:  added,
	})
	if err != nil {
		t.Fatalf("Put: %v", err)
	}

	finished := time.Now().Truncate(time.Second)
	for _, step := range []struct {
		name string
		err  error
	}{
		{"SetName", s.SetName(ctx, id, "Some.Release")},
		{"SetError", s.SetError(ctx, id, "no files matched the allow-list")},
		{"SetFinishedAt", s.SetFinishedAt(ctx, id, finished)},
		{"SetFiltered", s.SetFiltered(ctx, id, true)},
		{"SetPaused", s.SetPaused(ctx, id, false)},
	} {
		if step.err != nil {
			t.Fatalf("%s: %v", step.name, step.err)
		}
	}

	s.Close()
	reopened, err := Open(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer reopened.Close()

	list, err := reopened.List(ctx)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(list) != 1 {
		t.Fatalf("List returned %d torrents, want 1", len(list))
	}
	got := list[0]
	if got.ID != id || got.InfoHash != hash(7) {
		t.Errorf("identity did not survive: %+v", got)
	}
	if got.Name != "Some.Release" || got.Source != "magnet:?xt=urn:btih:07" || got.SavePath != "/data/tv" {
		t.Errorf("strings did not survive: %+v", got)
	}
	if got.Paused {
		t.Error("paused should have been cleared")
	}
	if !got.Filtered {
		t.Error("filtered should have survived")
	}
	if got.Error != "no files matched the allow-list" {
		t.Errorf("error = %q", got.Error)
	}
	if !got.AddedAt.Equal(added) {
		t.Errorf("added_at = %v, want %v", got.AddedAt, added)
	}
	if !got.FinishedAt.Equal(finished) {
		t.Errorf("finished_at = %v, want %v", got.FinishedAt, finished)
	}

	// Clearing finished_at must be possible: a torrent can stop being complete.
	if err := reopened.SetFinishedAt(ctx, id, time.Time{}); err != nil {
		t.Fatalf("SetFinishedAt zero: %v", err)
	}
	list, _ = reopened.List(ctx)
	if !list[0].FinishedAt.IsZero() {
		t.Errorf("finished_at = %v, want zero", list[0].FinishedAt)
	}
}

// anacrolix/torrent will not restore file priorities, so this is the only
// record of an allow-list's decision.
func TestFileSelectionRoundTrips(t *testing.T) {
	ctx := context.Background()
	s, _ := open(t)
	id, err := s.Put(ctx, &Torrent{InfoHash: hash(9), SavePath: "/data"})
	if err != nil {
		t.Fatalf("Put: %v", err)
	}

	if sel, err := s.FileSelection(ctx, id); err != nil || sel != nil {
		t.Errorf("a torrent with no recorded selection = %v, %v; want nil meaning everything", sel, err)
	}

	want := []bool{true, false, false, true}
	if err := s.SetFileSelection(ctx, id, want); err != nil {
		t.Fatalf("SetFileSelection: %v", err)
	}
	got, err := s.FileSelection(ctx, id)
	if err != nil {
		t.Fatalf("FileSelection: %v", err)
	}
	if len(got) != len(want) {
		t.Fatalf("selection length %d, want %d", len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("file %d selected = %v, want %v", i, got[i], want[i])
		}
	}

	// Replacing must not merge with the previous selection.
	if err := s.SetFileSelection(ctx, id, []bool{false}); err != nil {
		t.Fatalf("SetFileSelection: %v", err)
	}
	if got, _ := s.FileSelection(ctx, id); len(got) != 1 {
		t.Errorf("selection = %v, want it replaced", got)
	}
}

// Deleting a torrent must take its file selection with it, which needs
// foreign keys actually enabled - SQLite leaves them off by default.
func TestDeleteCascadesToFileSelection(t *testing.T) {
	ctx := context.Background()
	s, _ := open(t)
	id, err := s.Put(ctx, &Torrent{InfoHash: hash(11), SavePath: "/data"})
	if err != nil {
		t.Fatalf("Put: %v", err)
	}
	if err := s.SetFileSelection(ctx, id, []bool{true, false}); err != nil {
		t.Fatalf("SetFileSelection: %v", err)
	}
	if err := s.Delete(ctx, id); err != nil {
		t.Fatalf("Delete: %v", err)
	}

	var count int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM file_selection WHERE torrent_id = ?`, id).Scan(&count); err != nil {
		t.Fatalf("counting orphans: %v", err)
	}
	if count != 0 {
		t.Errorf("%d file_selection rows were orphaned; foreign keys are not enabled", count)
	}
}
