package store

import (
	"context"
	"path/filepath"
	"slices"
	"sync"
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
	_, _, decideErr := s.DecideFileSelection(ctx, id, Decision{Selection: []bool{false}, Filtered: true})
	for _, step := range []struct {
		name string
		err  error
	}{
		{"SetName", s.SetName(ctx, id, "Some.Release")},
		{"DecideFileSelection", decideErr},
		{"SetError", s.SetError(ctx, id, "no files matched the allow-list")},
		{"SetFinishedAt", s.SetFinishedAt(ctx, id, finished)},
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

// The first decision is final: a later proposal - a resume racing the
// metadata watcher, or a restart under a changed allow-list - gets the
// recorded one back, and only a client's own SetFileSelection replaces it.
func TestDecideFileSelectionRecordsOnlyTheFirstProposal(t *testing.T) {
	ctx := context.Background()
	s, _ := open(t)
	id, err := s.Put(ctx, &Torrent{InfoHash: hash(10), SavePath: "/data"})
	if err != nil {
		t.Fatalf("Put: %v", err)
	}

	first := Decision{Selection: []bool{true, false}, Filtered: true}
	got, decided, err := s.DecideFileSelection(ctx, id, first)
	if err != nil {
		t.Fatalf("DecideFileSelection: %v", err)
	}
	if !decided || !slices.Equal(got.Selection, first.Selection) || !got.Filtered {
		t.Fatalf("first proposal = %+v, decided %v; want it recorded", got, decided)
	}

	got, decided, err = s.DecideFileSelection(ctx, id, Decision{Selection: []bool{true, true}})
	if err != nil {
		t.Fatalf("DecideFileSelection: %v", err)
	}
	if decided || !slices.Equal(got.Selection, first.Selection) || !got.Filtered {
		t.Errorf("second proposal = %+v, decided %v; want the first one back", got, decided)
	}

	// A client's choice replaces the decision, and is what is decided after.
	if err := s.SetFileSelection(ctx, id, []bool{false, true}); err != nil {
		t.Fatalf("SetFileSelection: %v", err)
	}
	got, _, err = s.DecideFileSelection(ctx, id, first)
	if err != nil {
		t.Fatalf("DecideFileSelection: %v", err)
	}
	if !slices.Equal(got.Selection, []bool{false, true}) {
		t.Errorf("after a client's choice the decision = %v, want [false true]", got.Selection)
	}
}

// A rejection is recorded with its error in the same transaction, so a
// restart cannot find the one without the other.
func TestDecideFileSelectionRecordsTheRejection(t *testing.T) {
	ctx := context.Background()
	s, _ := open(t)
	id, err := s.Put(ctx, &Torrent{InfoHash: hash(12), SavePath: "/data"})
	if err != nil {
		t.Fatalf("Put: %v", err)
	}
	rejection := Decision{Selection: []bool{false, false}, Filtered: true, Error: "no files match"}
	if _, _, err := s.DecideFileSelection(ctx, id, rejection); err != nil {
		t.Fatalf("DecideFileSelection: %v", err)
	}

	list, err := s.List(ctx)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if !list[0].Filtered || list[0].Error != "no files match" {
		t.Errorf("recorded torrent = %+v, want it filtered and in error", list[0])
	}
	got, _, err := s.DecideFileSelection(ctx, id, Decision{Selection: []bool{true, true}})
	if err != nil {
		t.Fatalf("DecideFileSelection: %v", err)
	}
	if got.Error != "no files match" || slices.Contains(got.Selection, true) {
		t.Errorf("a later proposal = %+v, want the rejection back", got)
	}
}

// Many deciders at once - the watcher, a resume, the queue - all come away
// with the same selection, and exactly one of them recorded it. Each proposes
// something different, so any interleaving shows up as disagreement.
func TestConcurrentDecidersAgree(t *testing.T) {
	ctx := context.Background()
	s, _ := open(t)
	id, err := s.Put(ctx, &Torrent{InfoHash: hash(13), SavePath: "/data"})
	if err != nil {
		t.Fatalf("Put: %v", err)
	}

	const deciders = 16
	results := make([]Decision, deciders)
	won := make([]bool, deciders)
	errs := make([]error, deciders)
	var wg sync.WaitGroup
	for i := range deciders {
		wg.Go(func() {
			proposal := make([]bool, deciders)
			proposal[i] = true
			results[i], won[i], errs[i] = s.DecideFileSelection(ctx, id, Decision{Selection: proposal})
		})
	}
	wg.Wait()

	winners := 0
	for i := range deciders {
		if errs[i] != nil {
			t.Fatalf("decider %d: %v", i, errs[i])
		}
		if won[i] {
			winners++
		}
		if !slices.Equal(results[i].Selection, results[0].Selection) {
			t.Errorf("decider %d got %v, decider 0 got %v", i, results[i].Selection, results[0].Selection)
		}
	}
	if winners != 1 {
		t.Errorf("%d deciders recorded a selection, want exactly 1", winners)
	}
	recorded, err := s.FileSelection(ctx, id)
	if err != nil {
		t.Fatalf("FileSelection: %v", err)
	}
	if !slices.Equal(recorded, results[0].Selection) {
		t.Errorf("recorded %v, deciders were told %v", recorded, results[0].Selection)
	}
}

func TestDecideFileSelectionForAnUnknownTorrentFails(t *testing.T) {
	s, _ := open(t)
	if _, _, err := s.DecideFileSelection(context.Background(), 404, Decision{Selection: []bool{true}}); err == nil {
		t.Error("deciding for a torrent that does not exist succeeded")
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

// Labels carry Sonarr's and Radarr's category, so they have to survive a
// restart like any other engine-owned state.
func TestLabelsRoundTrip(t *testing.T) {
	s, _ := open(t)
	ctx := context.Background()

	id, err := s.Put(ctx, &Torrent{
		InfoHash: hash(1), SavePath: "/data", AddedAt: time.Now(),
		Labels: []string{"tv-sonarr", "readarr"},
	})
	if err != nil {
		t.Fatalf("Put: %v", err)
	}

	labels, err := s.Labels(ctx, id)
	if err != nil {
		t.Fatalf("Labels: %v", err)
	}
	if len(labels) != 2 || labels[0] != "readarr" || labels[1] != "tv-sonarr" {
		t.Errorf("labels = %v, want them sorted: [readarr tv-sonarr]", labels)
	}

	// The list read on startup carries them, or a restored torrent loses its
	// category and drops out of the client's queue.
	list, err := s.List(ctx)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(list) != 1 || len(list[0].Labels) != 2 {
		t.Fatalf("List did not carry the labels: %+v", list)
	}

	// Blanks and duplicates are dropped, and a set replaces rather than merges.
	if err := s.SetLabels(ctx, id, []string{"movies", "", "movies"}); err != nil {
		t.Fatalf("SetLabels: %v", err)
	}
	labels, err = s.Labels(ctx, id)
	if err != nil {
		t.Fatalf("Labels: %v", err)
	}
	if len(labels) != 1 || labels[0] != "movies" {
		t.Errorf("labels = %v, want [movies]", labels)
	}

	// Clearing them is a legitimate operation, not a no-op.
	if err := s.SetLabels(ctx, id, nil); err != nil {
		t.Fatalf("SetLabels(nil): %v", err)
	}
	if labels, _ = s.Labels(ctx, id); len(labels) != 0 {
		t.Errorf("labels = %v, want none", labels)
	}
}

// Deleting a torrent must take its labels with it, or the next torrent to be
// assigned that gid inherits them.
func TestDeleteRemovesLabels(t *testing.T) {
	s, _ := open(t)
	ctx := context.Background()
	id, err := s.Put(ctx, &Torrent{
		InfoHash: hash(2), SavePath: "/data", AddedAt: time.Now(),
		Labels: []string{"tv-sonarr"},
	})
	if err != nil {
		t.Fatalf("Put: %v", err)
	}
	if err := s.Delete(ctx, id); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if labels, _ := s.Labels(ctx, id); len(labels) != 0 {
		t.Errorf("labels survived the delete: %v", labels)
	}
}

// A torrent with no seed-limit row follows the session, which is what every
// torrent added before the table existed must do.
func TestSeedLimitsDefaultToFollowingTheSession(t *testing.T) {
	s, _ := open(t)
	ctx := context.Background()
	id, err := s.Put(ctx, &Torrent{InfoHash: hash(3), SavePath: "/data", AddedAt: time.Now()})
	if err != nil {
		t.Fatalf("Put: %v", err)
	}

	limits, err := s.SeedLimitsFor(ctx, id)
	if err != nil {
		t.Fatalf("SeedLimitsFor: %v", err)
	}
	if limits != (SeedLimits{}) {
		t.Errorf("limits = %+v, want the zero value", limits)
	}

	want := SeedLimits{RatioLimit: 1.5, RatioMode: 1, IdleLimit: 30, IdleMode: 1}
	if err := s.SetSeedLimits(ctx, id, want); err != nil {
		t.Fatalf("SetSeedLimits: %v", err)
	}
	// Setting twice must update rather than fail on the primary key.
	want.RatioLimit = 2.5
	if err := s.SetSeedLimits(ctx, id, want); err != nil {
		t.Fatalf("SetSeedLimits again: %v", err)
	}
	if got, _ := s.SeedLimitsFor(ctx, id); got != want {
		t.Errorf("limits = %+v, want %+v", got, want)
	}

	list, err := s.List(ctx)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(list) != 1 || list[0].SeedLimits != want {
		t.Errorf("List carried %+v, want %+v", list[0].SeedLimits, want)
	}
}
