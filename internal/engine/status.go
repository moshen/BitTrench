package engine

import (
	"slices"
	"time"

	"github.com/anacrolix/torrent"
	"github.com/anacrolix/torrent/metainfo"

	"github.com/moshen/bittrench/internal/store"
)

// State is a torrent's coarse state, as both the Transmission RPC layer and
// the web UI need it. It is derived from the engine's own paused flag and
// error registry, never inferred from anacrolix/torrent - which models
// neither.
type State string

const (
	StateStopped      State = "stopped"
	StateInitialising State = "initializing"
	StateDownloading  State = "downloading"
	StateSeeding      State = "seeding"
	StateError        State = "error"
)

// Status is everything the layers above need about one torrent.
type Status struct {
	ID       int64
	InfoHash metainfo.Hash
	Name     string
	State    State
	Paused   bool
	Error    string
	SavePath string

	// TotalBytes is 0 until metadata arrives.
	TotalBytes     int64
	CompletedBytes int64
	MissingBytes   int64
	UploadedBytes  int64

	DownloadRate float64 // bytes/second, from the sampler
	UploadRate   float64

	Peers          int
	Seeders        int
	PiecesComplete int
	PieceCount     int

	AddedAt    time.Time
	FinishedAt time.Time

	// HasMetadata is false while a magnet is still resolving its info dict.
	// Every metadata-dependent field above is zero until it is true, and
	// Files() must not be called: it dereferences a nil pointer.
	HasMetadata bool
	// Labels are the Transmission labels, copied out of the record - Sonarr
	// and Radarr keep their category here.
	Labels []string
	// SeedLimits are this torrent's own seed caps, reported as set. The zero
	// value means the session limits apply.
	SeedLimits store.SeedLimits
}

// Ratio is uploaded over total, or 0 when the total is unknown.
func (s Status) Ratio() float64 {
	if s.TotalBytes <= 0 {
		return 0
	}
	return float64(s.UploadedBytes) / float64(s.TotalBytes)
}

// ETA is how long the remaining bytes will take at the current rate, or -1
// when that cannot be answered - which is what Transmission clients expect
// for "unknown".
func (s Status) ETA() time.Duration {
	if s.MissingBytes <= 0 || s.DownloadRate <= 0 {
		return -1
	}
	return time.Duration(float64(s.MissingBytes)/s.DownloadRate) * time.Second
}

// Status returns one torrent's status, or false if the gid is unknown.
func (e *Engine) Status(id int64) (Status, bool) {
	rec := e.record(id)
	if rec == nil {
		return Status{}, false
	}
	return e.status(rec), true
}

// List returns every torrent's status, ordered by gid so clients see a stable
// order across polls.
func (e *Engine) List() []Status {
	recs := e.snapshot()
	out := make([]Status, 0, len(recs))
	for _, rec := range recs {
		out = append(out, e.status(rec))
	}
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j-1].ID > out[j].ID; j-- {
			out[j-1], out[j] = out[j], out[j-1]
		}
	}
	return out
}

// SessionRates returns the summed transfer rates, for session-stats.
func (e *Engine) SessionRates() (down, up float64) {
	totals := e.sampler.Totals()
	return totals.Download, totals.Upload
}

// Files returns the per-file view, empty before metadata arrives.
type File struct {
	Index     int
	Path      string
	Length    int64
	Completed int64
	Selected  bool
}

// Files returns a torrent's files, or nil if the gid is unknown or metadata
// has not arrived. Nil rather than a panic is the whole point: Torrent.Files()
// dereferences a nil pointer before the info dict resolves, and Radarr polls
// torrent-get with fields:["files"] immediately after torrent-add.
func (e *Engine) Files(id int64) []File {
	rec := e.record(id)
	if rec == nil || rec.Torrent.Info() == nil {
		return nil
	}
	files := rec.Torrent.Files()
	out := make([]File, len(files))
	for i, f := range files {
		out[i] = File{
			Index:     i,
			Path:      f.DisplayPath(),
			Length:    f.Length(),
			Completed: f.BytesCompleted(),
			Selected:  f.Priority() != torrent.PiecePriorityNone,
		}
	}
	return out
}

// SelectedBytes returns how many bytes of the files a torrent actually wants
// are complete, out of how many, and how many files are selected.
//
// This, not Status, is the honest progress and completion signal for a torrent
// whose files are not all selected. Torrent.BytesMissing counts every
// incomplete piece whether it is wanted or not, so an extension allow-list -
// or any deselected file - leaves MissingBytes above zero for good and
// CompletedBytes measured against data that will never arrive. Anything
// waiting for MissingBytes to reach zero waits forever.
//
// files is 0 before metadata arrives and for an unknown gid, which is how a
// caller tells "nothing is selected yet" from "all of it is done".
func (e *Engine) SelectedBytes(id int64) (completed, total int64, files int) {
	for _, f := range e.Files(id) {
		if !f.Selected {
			continue
		}
		completed += f.Completed
		total += f.Length
		files++
	}
	return completed, total, files
}

// Bitfield returns the completed-pieces bitfield and the piece count.
// Returning the count explicitly is what stops the last byte's padding bits
// from reading as missing pieces in the UI.
func (e *Engine) Bitfield(id int64) ([]byte, int) {
	rec := e.record(id)
	if rec == nil {
		return nil, 0
	}
	return e.completion.Bitfield(rec.Torrent.InfoHash())
}

// status builds the DTO. Every metadata-dependent accessor is guarded: Files,
// NumPieces, PieceState and Piece all panic or misreport before the info dict
// resolves.
func (e *Engine) status(rec *record) Status {
	t := rec.Torrent

	e.mu.RLock()
	paused, errMsg := rec.Paused, rec.Error
	addedAt, finishedAt := rec.AddedAt, rec.FinishedAt
	// Cloned inside the lock: the record's slice is replaced wholesale by
	// SetLabels, and handing a caller the live one lets it observe a write.
	labels := slices.Clone(rec.Labels)
	seedLimits := rec.SeedLimits
	e.mu.RUnlock()

	stats := t.Stats()
	rates := e.sampler.Rates(rec.ID)

	s := Status{
		ID:       rec.ID,
		InfoHash: t.InfoHash(),
		// Name is safe before metadata: it falls back to the magnet's display
		// name and then to "infohash:<hex>", and never panics.
		Name:           t.Name(),
		Paused:         paused,
		Error:          errMsg,
		SavePath:       rec.SavePath,
		CompletedBytes: t.BytesCompleted(),
		UploadedBytes:  stats.BytesWrittenData.Int64(),
		DownloadRate:   rates.Download,
		UploadRate:     rates.Upload,
		Peers:          int(stats.ActivePeers),
		Seeders:        int(stats.ConnectedSeeders),
		PiecesComplete: int(stats.PiecesComplete),
		AddedAt:        addedAt,
		FinishedAt:     finishedAt,
		HasMetadata:    t.Info() != nil,
		Labels:         labels,
		SeedLimits:     seedLimits,
	}
	if s.HasMetadata {
		s.TotalBytes = t.Length()
		s.MissingBytes = t.BytesMissing()
		s.PieceCount = t.NumPieces()
	}
	s.State = state(s)
	return s
}

func state(s Status) State {
	switch {
	case s.Error != "":
		return StateError
	case s.Paused:
		return StateStopped
	case !s.HasMetadata:
		return StateInitialising
	case s.MissingBytes == 0:
		return StateSeeding
	default:
		return StateDownloading
	}
}

// Peer is one live peer connection, for the UI's peer list.
type Peer struct {
	Addr   string
	Client string
	// Source is how the peer was discovered (tracker, DHT, PEX, incoming).
	// anacrolix keeps the outgoing/incoming flag unexported, and the
	// discovery source is the more useful answer anyway.
	Source       string
	DownloadRate float64
	UploadRate   float64
	BytesRead    int64
	BytesWritten int64
	// PiecesHave is how many pieces the peer says it has.
	PiecesHave int
}

// Peers returns the torrent's live peer connections. anacrolix hands out
// *PeerConn rather than a stats struct, so the per-peer rates come from the
// library here rather than from our sampler - it tracks them per connection
// already, which our per-torrent sampler cannot.
func (e *Engine) Peers(id int64) []Peer {
	rec := e.record(id)
	if rec == nil {
		return nil
	}
	conns := rec.Torrent.PeerConns()
	out := make([]Peer, 0, len(conns))
	for _, c := range conns {
		stats := c.Stats()
		client, _ := c.PeerClientName.Load().(string)
		addr := ""
		if c.RemoteAddr != nil {
			addr = c.RemoteAddr.String()
		}
		out = append(out, Peer{
			Addr:         addr,
			Client:       client,
			Source:       string(c.Discovery),
			DownloadRate: stats.DownloadRate,
			UploadRate:   stats.LastWriteUploadRate,
			BytesRead:    stats.BytesReadData.Int64(),
			BytesWritten: stats.BytesWrittenData.Int64(),
			PiecesHave:   stats.RemotePieceCount,
		})
	}
	return out
}

// AnnounceURLs returns the announce URLs in use, for the detail view. Note
// these are the post-rewrite URLs: a UDP tracker shows as an IP literal,
// because a hostname is precisely what must never reach the tracker client.
func (e *Engine) AnnounceURLs(id int64) []string {
	rec := e.record(id)
	if rec == nil || rec.Torrent.Info() == nil {
		return nil
	}
	var out []string
	for _, tier := range rec.Torrent.Metainfo().AnnounceList {
		out = append(out, tier...)
	}
	return out
}
