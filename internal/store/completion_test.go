package store

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/anacrolix/torrent/metainfo"
	"github.com/anacrolix/torrent/storage"
)

func completion(t *testing.T) (*Store, *Completion, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "state.db")
	s, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	c, err := s.NewCompletion(context.Background())
	if err != nil {
		t.Fatalf("NewCompletion: %v", err)
	}
	t.Cleanup(func() { c.Close(); s.Close() })
	return s, c, path
}

func pk(ih metainfo.Hash, index int) metainfo.PieceKey {
	return metainfo.PieceKey{InfoHash: ih, Index: index}
}

// The heart of the design: Ok and Complete are distinct, and collapsing them
// produces one of two silent bugs - a torrent that re-downloads data it
// already has, or one that rehashes everything on every restart.
func TestUnknownCompleteAndMissingAreThreeDistinctStates(t *testing.T) {
	_, c, _ := completion(t)
	ih := hash(1)
	c.Register(ih, 8)

	// Never set: not Ok, so anacrolix hashes to find out.
	got, err := c.Get(pk(ih, 0))
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Ok {
		t.Errorf("an unrecorded piece reported Ok=%v Complete=%v; it must be unknown", got.Ok, got.Complete)
	}

	// Set complete: trusted good, no hashing.
	if err := c.Set(pk(ih, 1), true); err != nil {
		t.Fatalf("Set: %v", err)
	}
	if got, _ := c.Get(pk(ih, 1)); !got.Ok || !got.Complete {
		t.Errorf("a completed piece = Ok:%v Complete:%v, want both true", got.Ok, got.Complete)
	}

	// Set missing: trusted missing, re-download without hashing. This is the
	// state a single bitfield cannot represent.
	if err := c.Set(pk(ih, 2), false); err != nil {
		t.Fatalf("Set: %v", err)
	}
	if got, _ := c.Get(pk(ih, 2)); !got.Ok || got.Complete {
		t.Errorf("a known-missing piece = Ok:%v Complete:%v, want Ok true and Complete false", got.Ok, got.Complete)
	}

	// An unrelated torrent shares nothing.
	if got, _ := c.Get(pk(hash(2), 1)); got.Ok {
		t.Error("completion leaked between infohashes")
	}
}

// Fast resume: what was recorded before a restart must be trusted after it.
func TestCompletionSurvivesAReopen(t *testing.T) {
	ctx := context.Background()
	s, c, path := completion(t)
	ih := hash(3)

	if _, err := s.Put(ctx, &Torrent{InfoHash: ih, SavePath: "/data"}); err != nil {
		t.Fatalf("Put: %v", err)
	}
	c.Register(ih, 20)
	for _, i := range []int{0, 3, 19} {
		if err := c.Set(pk(ih, i), true); err != nil {
			t.Fatalf("Set: %v", err)
		}
	}
	if err := c.Set(pk(ih, 5), false); err != nil {
		t.Fatalf("Set: %v", err)
	}
	if err := c.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	s.Close()

	reopened, err := Open(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer reopened.Close()
	c2, err := reopened.NewCompletion(ctx)
	if err != nil {
		t.Fatalf("NewCompletion: %v", err)
	}
	defer c2.Close()

	for _, i := range []int{0, 3, 19} {
		if got, _ := c2.Get(pk(ih, i)); !got.Ok || !got.Complete {
			t.Errorf("piece %d after reopen = Ok:%v Complete:%v, want complete", i, got.Ok, got.Complete)
		}
	}
	if got, _ := c2.Get(pk(ih, 5)); !got.Ok || got.Complete {
		t.Errorf("piece 5 after reopen = Ok:%v Complete:%v, want known-missing", got.Ok, got.Complete)
	}
	if got, _ := c2.Get(pk(ih, 7)); got.Ok {
		t.Error("a piece that was never set came back known after reopen")
	}
}

// Completion is only written on a flush, so a torrent removed and re-added
// must not inherit the old record - those pieces are no longer on disk.
func TestForgetClearsPersistedCompletion(t *testing.T) {
	ctx := context.Background()
	s, c, path := completion(t)
	ih := hash(4)
	if _, err := s.Put(ctx, &Torrent{InfoHash: ih, SavePath: "/data"}); err != nil {
		t.Fatalf("Put: %v", err)
	}
	c.Register(ih, 8)
	if err := c.Set(pk(ih, 1), true); err != nil {
		t.Fatalf("Set: %v", err)
	}
	if err := c.Flush(); err != nil {
		t.Fatalf("Flush: %v", err)
	}
	c.Forget(ih)
	if got, _ := c.Get(pk(ih, 1)); got.Ok {
		t.Error("Forget left completion in memory")
	}
	c.Close()
	s.Close()

	reopened, err := Open(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer reopened.Close()
	c2, err := reopened.NewCompletion(ctx)
	if err != nil {
		t.Fatalf("NewCompletion: %v", err)
	}
	defer c2.Close()
	if got, _ := c2.Get(pk(ih, 1)); got.Ok {
		t.Error("Forget left completion in the database")
	}
}

// GetRange must agree with Get; anacrolix uses whichever is convenient.
func TestGetRangeMatchesGet(t *testing.T) {
	_, c, _ := completion(t)
	ih := hash(5)
	c.Register(ih, 16)
	for i := range 16 {
		if err := c.Set(pk(ih, i), i%3 == 0); err != nil {
			t.Fatalf("Set: %v", err)
		}
	}
	i := 4
	for got := range c.GetRange(ih, 4, 12) {
		want, _ := c.Get(pk(ih, i))
		if got != want {
			t.Errorf("GetRange piece %d = %+v, Get = %+v", i, got, want)
		}
		i++
	}
	if i != 12 {
		t.Errorf("GetRange yielded %d pieces, want 8", i-4)
	}
}

// The bitfield the UI draws must come with an explicit piece count, or the
// trailing padding bits of the last byte read as missing pieces.
func TestBitfieldCarriesAnExplicitPieceCount(t *testing.T) {
	_, c, _ := completion(t)
	ih := hash(6)
	c.Register(ih, 12) // 12 pieces is 2 bytes with 4 bits of padding
	for i := range 12 {
		if err := c.Set(pk(ih, i), true); err != nil {
			t.Fatalf("Set: %v", err)
		}
	}
	bits, pieces := c.Bitfield(ih)
	if pieces != 12 {
		t.Errorf("piece count = %d, want 12", pieces)
	}
	if len(bits) != 2 {
		t.Errorf("bitfield is %d bytes, want 2", len(bits))
	}
	for i := range 12 {
		if !testBit(bits, i) {
			t.Errorf("piece %d reads as missing in a fully complete torrent", i)
		}
	}
	// The returned slice must be a copy; handing out the live one would let a
	// caller corrupt completion.
	bits[0] = 0
	if again, _ := c.Bitfield(ih); again[0] == 0 {
		t.Error("Bitfield returned the live slice, not a copy")
	}
}

// Set on a torrent that was never registered must still record, growing the
// bitfield rather than dropping the completion on the floor.
func TestSetGrowsAnUnregisteredTorrent(t *testing.T) {
	_, c, _ := completion(t)
	ih := hash(8)
	if err := c.Set(pk(ih, 100), true); err != nil {
		t.Fatalf("Set: %v", err)
	}
	if got, _ := c.Get(pk(ih, 100)); !got.Ok || !got.Complete {
		t.Errorf("piece 100 = %+v, want complete", got)
	}
	if _, pieces := c.Bitfield(ih); pieces != 101 {
		t.Errorf("piece count = %d, want 101", pieces)
	}
}

var _ storage.PieceCompletion = (*Completion)(nil)
