package engine

import (
	"sync"
	"time"

	"github.com/anacrolix/torrent"
)

// anacrolix/torrent exposes cumulative counters only: there is no rate anywhere
// in its API. So rates are sampled here, a snapshot per tick differenced against
// the previous one and exponentially smoothed. Everything downstream
// (session-stats, torrent-get's rateDownload/rateUpload, the UI's sparkline)
// reads this rather than computing its own.
//
// The counters are bytes and so is every rate derived from them. Nothing
// downstream converts units.

const (
	// sampleInterval is the tick. 1 Hz matches what the UI polls at.
	sampleInterval = time.Second
	// smoothing weights the newest sample. Rates over BitTorrent are spiky
	// enough that a raw per-second difference reads as noise.
	smoothing = 0.4
)

// rates is a torrent's current transfer rates, in bytes per second.
type rates struct {
	Download float64
	Upload   float64
}

type sample struct {
	read    int64
	written int64
	at      time.Time
}

// sampler tracks per-torrent rates by differencing cumulative counters.
type sampler struct {
	mu   sync.RWMutex
	prev map[int64]sample
	cur  map[int64]rates
}

func newSampler() *sampler {
	return &sampler{prev: make(map[int64]sample), cur: make(map[int64]rates)}
}

// Rates returns the current rates for a torrent, zero if not yet sampled.
func (s *sampler) Rates(id int64) rates {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.cur[id]
}

// Totals returns the summed rates across all torrents, for session-stats.
func (s *sampler) Totals() rates {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var total rates
	for _, r := range s.cur {
		total.Download += r.Download
		total.Upload += r.Upload
	}
	return total
}

// observe folds one snapshot into the smoothed rates.
func (s *sampler) observe(id int64, stats torrent.TorrentStats, now time.Time) {
	// ConnStats counters are Count, not int64, and Int64 has a pointer
	// receiver - so stats has to be a local before reading them.
	s.observeRaw(id, stats.BytesReadData.Int64(), stats.BytesWrittenData.Int64(), now)
}

// observeRaw is observe with the counters already extracted, so the rate
// arithmetic can be tested without constructing a TorrentStats.
func (s *sampler) observeRaw(id, read, written int64, now time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	prev, ok := s.prev[id]
	s.prev[id] = sample{read: read, written: written, at: now}
	if !ok {
		return
	}
	elapsed := now.Sub(prev.at).Seconds()
	if elapsed <= 0 {
		return
	}
	// A counter that went backwards means the torrent was re-added; treat it
	// as a fresh baseline rather than reporting a negative rate.
	down := float64(read-prev.read) / elapsed
	up := float64(written-prev.written) / elapsed
	if down < 0 {
		down = 0
	}
	if up < 0 {
		up = 0
	}

	old := s.cur[id]
	s.cur[id] = rates{
		Download: old.Download + smoothing*(down-old.Download),
		Upload:   old.Upload + smoothing*(up-old.Upload),
	}
}

// forget drops a removed torrent, so its last rates do not linger in the
// session totals.
func (s *sampler) forget(id int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.prev, id)
	delete(s.cur, id)
}
