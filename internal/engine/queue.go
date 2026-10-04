package engine

import (
	"context"
	"fmt"
	"log/slog"
	"slices"

	"github.com/moshen/bittrench/internal/store"
)

// The download queue.
//
// anacrolix/torrent has no queue: every torrent it holds downloads at once. A
// Transmission client assumes one exists - it shows a queue position per
// torrent, offers to reorder it, and reads a distinct "waiting to download"
// status - and Sonarr moves a download to the top of it when its priority says
// so. So the queue is the daemon's own, persisted in the state database, and
// enforced here.
//
// A held torrent is expressed exactly as a paused one: nothing is wanted. That
// is not a shortcut, it is the only representation the library supports - see
// applyHeld for why the obvious DisallowDataDownload crashes the process. The
// difference between held and paused is who did it, which is why Queued is
// tracked separately from Paused rather than reusing it.

// Move is where in the queue a torrent should go.
type Move int

// The moves a Transmission client can ask for, one per queue-move-* method.
const (
	MoveTop Move = iota
	MoveUp
	MoveDown
	MoveBottom
)

// queueOrder returns every record ordered by queue position, with unplaced
// torrents last in gid order - which is the order they were added in.
//
// Callers must hold at least a read lock.
func (e *Engine) queueOrderLocked() []*record {
	recs := make([]*record, 0, len(e.records))
	for _, rec := range e.records {
		recs = append(recs, rec)
	}
	slices.SortFunc(recs, func(a, b *record) int {
		switch {
		case a.QueuePosition == b.QueuePosition:
			return int(a.ID - b.ID)
		case a.QueuePosition == store.Unplaced:
			return 1
		case b.QueuePosition == store.Unplaced:
			return -1
		default:
			return a.QueuePosition - b.QueuePosition
		}
	})
	return recs
}

// nextQueuePosition is the position a newly added torrent takes: the end.
func (e *Engine) nextQueuePosition() int {
	e.mu.RLock()
	defer e.mu.RUnlock()
	next := 0
	for _, rec := range e.records {
		if rec.QueuePosition != store.Unplaced && rec.QueuePosition >= next {
			next = rec.QueuePosition + 1
		}
	}
	return next
}

// MoveInQueue repositions a torrent and reconciles what is running.
//
// The whole order is renumbered afterwards so positions stay contiguous from
// zero, as Transmission's do: a client renders them directly, and gaps would
// show up as missing places in the queue.
func (e *Engine) MoveInQueue(ctx context.Context, id int64, move Move) error {
	if e.record(id) == nil {
		return fmt.Errorf("no torrent with id %d", id)
	}

	e.mu.Lock()
	order := e.queueOrderLocked()
	from := slices.IndexFunc(order, func(rec *record) bool { return rec.ID == id })
	to := from
	switch move {
	case MoveTop:
		to = 0
	case MoveUp:
		to = max(from-1, 0)
	case MoveDown:
		to = min(from+1, len(order)-1)
	case MoveBottom:
		to = len(order) - 1
	}
	rec := order[from]
	order = slices.Delete(order, from, from+1)
	order = slices.Insert(order, to, rec)

	positions := make(map[int64]int, len(order))
	for i, rec := range order {
		rec.QueuePosition = i
		positions[rec.ID] = i
	}
	e.mu.Unlock()

	if err := e.store.SetQueuePositions(ctx, positions); err != nil {
		return err
	}
	slog.Info("torrent moved in the queue", "id", id, "position", to)
	e.reconcileQueue()
	return nil
}

// QueuePosition returns a torrent's place in the queue, or false if the gid is
// unknown.
func (e *Engine) QueuePosition(id int64) (int, bool) {
	rec := e.record(id)
	if rec == nil {
		return 0, false
	}
	e.mu.RLock()
	defer e.mu.RUnlock()
	return rec.QueuePosition, true
}

// reconcileQueue starts and holds torrents so that no more than the configured
// number download at once, in queue order.
//
// Serialised on its own mutex: it is called from the monitor tick and from every
// path that changes what should be running, and two runs interleaving could
// leave a torrent held with a free slot in front of it.
func (e *Engine) reconcileQueue() {
	size := int(e.cfg.Torrent.DownloadQueueSize)
	if size <= 0 {
		return // no queue configured: everything runs, as it always did
	}

	e.queueMu.Lock()
	defer e.queueMu.Unlock()

	var hold, release []*record

	// Completion is read before taking the lock: it walks the wanted files and
	// File.BytesCompleted takes the torrent client's lock, which is not a lock
	// to be holding e.mu across.
	complete := make(map[int64]bool)
	for _, rec := range e.snapshot() {
		complete[rec.ID] = e.selectedComplete(rec)
	}

	e.mu.Lock()
	slots := size
	for _, rec := range e.queueOrderLocked() {
		// A torrent stopped by hand, one that failed, and one that has finished
		// downloading all occupy no slot: the first two are not trying to
		// download, and the third is seeding.
		if rec.Paused || rec.Error != "" || complete[rec.ID] {
			// A finished torrent that is still marked queued would otherwise
			// never be let go, since nothing below considers it again.
			if rec.Queued {
				rec.Queued = false
				release = append(release, rec)
			}
			continue
		}
		if slots > 0 {
			slots--
			if rec.Queued {
				rec.Queued = false
				release = append(release, rec)
			}
			continue
		}
		if !rec.Queued {
			rec.Queued = true
			hold = append(hold, rec)
		}
	}
	e.mu.Unlock()

	// Outside the lock: converging touches the torrent client, which takes its
	// own. It reads the flags afresh rather than acting on this pass's
	// decision, so a torrent stopped since then stays stopped.
	for _, rec := range hold {
		slog.Info("torrent queued", "id", rec.ID, "name", rec.Torrent.Name(),
			"position", rec.QueuePosition)
		e.converge(rec)
	}
	for _, rec := range release {
		slog.Info("torrent leaving the queue", "id", rec.ID, "name", rec.Torrent.Name(),
			"position", rec.QueuePosition)
		e.converge(rec)
	}
}

// selectedComplete reports whether every file a torrent wants has arrived.
//
// Measured against the record's desired selection rather than the live
// priorities: a held torrent wants nothing at this moment, and asking the
// library would call every held torrent incomplete - including one whose data
// is already on disk, which would then sit in the queue waiting for a slot it
// does not need.
func (e *Engine) selectedComplete(rec *record) bool {
	e.mu.RLock()
	selection := slices.Clone(rec.Selection)
	e.mu.RUnlock()

	t := rec.Torrent
	if t.Info() == nil || len(selection) == 0 {
		return false
	}
	sizeWhenDone, have := selectionBytes(t, selection, t.Length(), t.BytesCompleted())
	return sizeWhenDone > 0 && have >= sizeWhenDone
}
