package engine

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/moshen/bittrench/internal/store"
)

// addN adds n torrents with distinct content, so each gets its own infohash.
func addN(t *testing.T, h *harness, n int) []int64 {
	t.Helper()
	ids := make([]int64, 0, n)
	for i := 0; i < n; i++ {
		mi, _ := buildTorrent(t, map[string]string{
			"file.bin": string(rune('a'+i)) + "-payload-that-differs",
		})
		id, _, err := h.engine.Add(context.Background(), AddRequest{Metainfo: mi})
		if err != nil {
			t.Fatalf("Add %d: %v", i, err)
		}
		ids = append(ids, id)
	}
	return ids
}

func states(t *testing.T, e *Engine, ids []int64) []State {
	t.Helper()
	out := make([]State, 0, len(ids))
	for _, id := range ids {
		st, ok := e.Status(id)
		if !ok {
			t.Fatalf("no status for torrent %d", id)
		}
		out = append(out, st.State)
	}
	return out
}

// The queue's whole point: past the configured depth, torrents wait instead of
// all downloading at once.
func TestQueueHoldsTorrentsPastItsDepth(t *testing.T) {
	h := newHarness(t, func(c *cfgOpts) { c.downloadQueueSize = 2 })
	ids := addN(t, h, 4)

	queued := 0
	for i, state := range states(t, h.engine, ids) {
		if state == StateQueued {
			queued++
		} else if i >= 2 {
			t.Errorf("torrent %d (position %d) is %s, expected it to wait", ids[i], i, state)
		}
	}
	if queued != 2 {
		t.Errorf("%d torrents queued, want 2 of 4 behind a queue of depth 2", queued)
	}

	// A queued torrent wants nothing - the only representation of "not
	// downloading" the library supports.
	rec := h.engine.record(ids[3])
	if rec.Torrent.Info() != nil {
		for _, f := range rec.Torrent.Files() {
			if f.Priority() != 0 {
				t.Errorf("a queued torrent's file %q is still wanted", f.DisplayPath())
			}
		}
	}
}

// A queue of zero is no queue at all, which is what every deployment had before
// the feature existed.
func TestQueueSizeZeroHoldsNothing(t *testing.T) {
	h := newHarness(t, func(c *cfgOpts) { c.downloadQueueSize = 0 })
	ids := addN(t, h, 3)
	for i, state := range states(t, h.engine, ids) {
		if state == StateQueued {
			t.Errorf("torrent %d is queued with the queue disabled", ids[i])
		}
	}
}

// Stopping a running torrent hands its slot to whatever is waiting.
func TestStoppingATorrentReleasesItsSlot(t *testing.T) {
	h := newHarness(t, func(c *cfgOpts) { c.downloadQueueSize = 1 })
	ids := addN(t, h, 2)
	ctx := context.Background()

	if got := states(t, h.engine, ids); got[1] != StateQueued {
		t.Fatalf("second torrent is %s, want queued", got[1])
	}
	if err := h.engine.Stop(ctx, ids[0]); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if got := states(t, h.engine, ids); got[1] == StateQueued {
		t.Error("the waiting torrent was not released when the slot freed")
	}
}

// A torrent the queue is holding when its metadata arrives wants nothing, and
// once let go wants exactly what the allow-list chose - not nothing, and not
// everything.
func TestAQueuedTorrentIsFilteredButNotStarted(t *testing.T) {
	h := newHarness(t, func(c *cfgOpts) {
		c.downloadQueueSize = 1
		c.allowedExtensions = []string{"mkv"}
	})
	ctx := context.Background()
	first, _ := buildTorrent(t, map[string]string{"first.mkv": "occupies the only slot"})
	second, _ := buildTorrent(t, map[string]string{"second.mkv": "video", "second.nfo": "notes"})

	running, _, err := h.engine.Add(ctx, AddRequest{Metainfo: first})
	if err != nil {
		t.Fatalf("Add: %v", err)
	}
	waiting, _, err := h.engine.Add(ctx, AddRequest{Metainfo: second})
	if err != nil {
		t.Fatalf("Add: %v", err)
	}
	h.waitForMetadata(t, waiting)

	if st, _ := h.engine.Status(waiting); st.State != StateQueued {
		t.Fatalf("second torrent is %s, want queued", st.State)
	}
	for _, f := range h.engine.Files(waiting) {
		if f.Selected {
			t.Errorf("%s is wanted while the torrent waits in the queue", f.Path)
		}
	}

	if err := h.engine.Stop(ctx, running); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	for _, f := range h.engine.Files(waiting) {
		want := filepath.Ext(f.Path) == ".mkv"
		if f.Selected != want {
			t.Errorf("after leaving the queue %s selected = %v, want %v", f.Path, f.Selected, want)
		}
	}
}

// A torrent stopped by hand must not be started by the queue: only the user
// clears Paused.
func TestTheQueueDoesNotStartAPausedTorrent(t *testing.T) {
	h := newHarness(t, func(c *cfgOpts) { c.downloadQueueSize = 5 })
	mi, _ := buildTorrent(t, map[string]string{"a.bin": "payload"})
	ctx := context.Background()
	id, _, err := h.engine.Add(ctx, AddRequest{Metainfo: mi, Paused: true})
	if err != nil {
		t.Fatalf("Add: %v", err)
	}
	h.engine.reconcileQueue()

	st, _ := h.engine.Status(id)
	if st.State != StateStopped {
		t.Errorf("state = %s, want stopped - the queue must not resume it", st.State)
	}
}

// Positions are assigned in add order and survive a restart, or a client's
// queue renders in an order that changes every time the daemon starts.
func TestQueuePositionsAreAssignedAndPersisted(t *testing.T) {
	h := newHarness(t, func(c *cfgOpts) { c.downloadQueueSize = 1 })
	ids := addN(t, h, 3)
	ctx := context.Background()

	for want, id := range ids {
		if got, _ := h.engine.QueuePosition(id); got != want {
			t.Errorf("torrent %d is at position %d, want %d", id, got, want)
		}
	}

	if err := h.engine.MoveInQueue(ctx, ids[2], MoveTop); err != nil {
		t.Fatalf("MoveInQueue: %v", err)
	}
	e := h.restart(t)
	if err := e.Restore(ctx); err != nil {
		t.Fatalf("Restore: %v", err)
	}
	for want, id := range []int64{ids[2], ids[0], ids[1]} {
		if got, ok := e.QueuePosition(id); !ok || got != want {
			t.Errorf("after the restart torrent %d is at %d, want %d", id, got, want)
		}
	}
	// And the order is what decides who runs: the promoted torrent took the
	// single slot.
	if st, _ := e.Status(ids[2]); st.State == StateQueued {
		t.Error("the torrent moved to the top is still waiting")
	}
}

func TestQueueMoves(t *testing.T) {
	tests := []struct {
		name string
		move Move
		on   int
		want []int
	}{
		{name: "top", move: MoveTop, on: 2, want: []int{2, 0, 1, 3}},
		{name: "bottom", move: MoveBottom, on: 1, want: []int{0, 2, 3, 1}},
		{name: "up", move: MoveUp, on: 2, want: []int{0, 2, 1, 3}},
		{name: "down", move: MoveDown, on: 1, want: []int{0, 2, 1, 3}},
		{name: "up at the top is a no-op", move: MoveUp, on: 0, want: []int{0, 1, 2, 3}},
		{name: "down at the bottom is a no-op", move: MoveDown, on: 3, want: []int{0, 1, 2, 3}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t, func(c *cfgOpts) { c.downloadQueueSize = 1 })
			ids := addN(t, h, 4)
			if err := h.engine.MoveInQueue(context.Background(), ids[tc.on], tc.move); err != nil {
				t.Fatalf("MoveInQueue: %v", err)
			}
			for position, index := range tc.want {
				if got, _ := h.engine.QueuePosition(ids[index]); got != position {
					t.Errorf("torrent %d (added %dth) is at %d, want %d",
						ids[index], index, got, position)
				}
			}
		})
	}
}

// Positions stay contiguous from zero: a client renders them directly, and a
// gap shows up as a missing place in the queue.
func TestQueuePositionsStayContiguous(t *testing.T) {
	h := newHarness(t, func(c *cfgOpts) { c.downloadQueueSize = 1 })
	ids := addN(t, h, 4)
	ctx := context.Background()

	for _, move := range []Move{MoveBottom, MoveTop, MoveUp, MoveDown} {
		if err := h.engine.MoveInQueue(ctx, ids[2], move); err != nil {
			t.Fatalf("MoveInQueue: %v", err)
		}
	}
	seen := map[int]bool{}
	for _, id := range ids {
		position, _ := h.engine.QueuePosition(id)
		if position < 0 || position >= len(ids) {
			t.Errorf("torrent %d is at position %d, outside 0..%d", id, position, len(ids)-1)
		}
		if seen[position] {
			t.Errorf("two torrents claim position %d", position)
		}
		seen[position] = true
	}
}

// A torrent the store has never placed sorts last rather than first: position
// -1 would otherwise jump an old database's torrents to the head of the queue.
func TestUnplacedTorrentsSortLast(t *testing.T) {
	h := newHarness(t, func(c *cfgOpts) { c.downloadQueueSize = 1 })
	ids := addN(t, h, 2)

	// Simulate a row written before the queue table existed.
	h.engine.mu.Lock()
	h.engine.records[ids[0]].QueuePosition = store.Unplaced
	h.engine.mu.Unlock()

	h.engine.mu.RLock()
	order := h.engine.queueOrderLocked()
	h.engine.mu.RUnlock()
	if order[len(order)-1].ID != ids[0] {
		t.Errorf("the unplaced torrent is at position 0 of the order, want last")
	}
}

// The monitor tick reconciles, so a torrent that finishes hands its slot on
// without anyone asking.
func TestReconcileIsIdempotent(t *testing.T) {
	h := newHarness(t, func(c *cfgOpts) { c.downloadQueueSize = 2 })
	ids := addN(t, h, 4)

	before := states(t, h.engine, ids)
	for i := 0; i < 3; i++ {
		h.engine.reconcileQueue()
		time.Sleep(10 * time.Millisecond)
	}
	after := states(t, h.engine, ids)
	for i := range before {
		if before[i] != after[i] {
			t.Errorf("torrent %d moved from %s to %s on a repeat reconcile",
				ids[i], before[i], after[i])
		}
	}
}
