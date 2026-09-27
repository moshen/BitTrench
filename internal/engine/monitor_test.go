package engine

import (
	"strings"
	"testing"
	"time"

	"github.com/moshen/bittrench/internal/store"
)

func TestSeedLimitReason(t *testing.T) {
	const gb = int64(1) << 30
	for _, c := range []struct {
		name       string
		uploaded   int64
		total      int64
		seeded     time.Duration
		ratioLimit float64
		timeLimit  time.Duration
		want       string // "" means no limit reached
	}{
		{name: "under both", uploaded: gb, total: gb, seeded: time.Minute, ratioLimit: 2, timeLimit: time.Hour},
		{name: "ratio reached", uploaded: 2 * gb, total: gb, ratioLimit: 2, want: "ratio"},
		{name: "ratio exceeded", uploaded: 5 * gb, total: gb, ratioLimit: 2, want: "ratio"},
		{name: "time reached", total: gb, seeded: 2 * time.Hour, timeLimit: time.Hour, want: "seeded for"},
		// A zero or negative ratio limit disables the ratio cap entirely,
		// which is how an operator turns off seeding limits.
		{name: "ratio cap disabled", uploaded: 100 * gb, total: gb, ratioLimit: 0},
		{name: "negative ratio cap disabled", uploaded: 100 * gb, total: gb, ratioLimit: -1},
		// A zero time limit disables the time cap.
		{name: "time cap disabled", total: gb, seeded: 1000 * time.Hour, timeLimit: 0},
		// An unknown total must not divide by zero or fire the cap.
		{name: "unknown total", uploaded: gb, total: 0, ratioLimit: 2},
	} {
		t.Run(c.name, func(t *testing.T) {
			got := seedLimitReason(c.uploaded, c.total, c.seeded, c.ratioLimit, c.timeLimit)
			if c.want == "" && got != "" {
				t.Errorf("got %q, want no limit reached", got)
			}
			if c.want != "" && !strings.Contains(got, c.want) {
				t.Errorf("got %q, want it to mention %q", got, c.want)
			}
		})
	}
}

// anacrolix exposes cumulative counters only, so rates are differenced here.
func TestSamplerDifferencesCounters(t *testing.T) {
	s := newSampler()
	start := time.Now()

	// The first observation is a baseline and yields no rate.
	s.observeRaw(1, 0, 0, start)
	if r := s.Rates(1); r.Download != 0 || r.Upload != 0 {
		t.Errorf("the first sample produced a rate: %+v", r)
	}

	// 1 MiB read and 512 KiB written over one second.
	s.observeRaw(1, 1<<20, 1<<19, start.Add(time.Second))
	r := s.Rates(1)
	if r.Download <= 0 || r.Upload <= 0 {
		t.Fatalf("no rate after a second sample: %+v", r)
	}
	// Smoothing means the first non-zero sample lands at `smoothing` of the
	// raw value, not the whole of it.
	wantDown := smoothing * float64(int64(1)<<20)
	if diff := r.Download - wantDown; diff > 1 || diff < -1 {
		t.Errorf("download rate = %v, want about %v", r.Download, wantDown)
	}

	// Repeating the same counters means no traffic, and the rate must decay
	// towards zero rather than sticking.
	for i := 2; i < 20; i++ {
		s.observeRaw(1, 1<<20, 1<<19, start.Add(time.Duration(i)*time.Second))
	}
	if r := s.Rates(1); r.Download > 1000 {
		t.Errorf("the rate did not decay with no traffic: %v", r.Download)
	}
}

// A counter that goes backwards means the torrent was re-added; that must not
// produce a negative rate.
func TestSamplerIgnoresCounterResets(t *testing.T) {
	s := newSampler()
	start := time.Now()
	s.observeRaw(1, 5<<20, 5<<20, start)
	s.observeRaw(1, 0, 0, start.Add(time.Second))
	r := s.Rates(1)
	if r.Download < 0 || r.Upload < 0 {
		t.Errorf("a counter reset produced negative rates: %+v", r)
	}
}

func TestSamplerTotalsAndForget(t *testing.T) {
	s := newSampler()
	start := time.Now()
	for _, id := range []int64{1, 2} {
		s.observeRaw(id, 0, 0, start)
		s.observeRaw(id, 1<<20, 0, start.Add(time.Second))
	}
	totals := s.Totals()
	one := s.Rates(1)
	if diff := totals.Download - 2*one.Download; diff > 1 || diff < -1 {
		t.Errorf("totals = %v, want twice one torrent's %v", totals.Download, one.Download)
	}

	s.forget(1)
	if r := s.Rates(1); r.Download != 0 {
		t.Errorf("a forgotten torrent still reports %v", r.Download)
	}
	if totals := s.Totals(); totals.Download != s.Rates(2).Download {
		t.Errorf("a forgotten torrent still counts towards the session totals")
	}
}

// ETA is -1 for "unknown", which is what Transmission clients expect.
func TestETAAndRatio(t *testing.T) {
	s := Status{MissingBytes: 1000, DownloadRate: 100}
	if got := s.ETA(); got != 10*time.Second {
		t.Errorf("ETA = %v, want 10s", got)
	}
	if got := (Status{MissingBytes: 1000}).ETA(); got != -1 {
		t.Errorf("ETA with no rate = %v, want -1", got)
	}
	if got := (Status{MissingBytes: 0, DownloadRate: 100}).ETA(); got != -1 {
		t.Errorf("ETA when complete = %v, want -1", got)
	}
	if got := (Status{UploadedBytes: 300, TotalBytes: 100}).Ratio(); got != 3 {
		t.Errorf("Ratio = %v, want 3", got)
	}
	if got := (Status{UploadedBytes: 300}).Ratio(); got != 0 {
		t.Errorf("Ratio with an unknown total = %v, want 0", got)
	}
}

// A torrent's own caps override the session's, which is the whole point of
// storing them: Sonarr pushes a per-download seed criterion and expects it to
// be the one that applies.
func TestEffectiveSeedLimits(t *testing.T) {
	const sessionRatio = 2.0
	sessionTime := 2 * time.Hour

	tests := []struct {
		name      string
		limits    store.SeedLimits
		wantRatio float64
		wantTime  time.Duration
	}{
		{
			name:      "an untouched torrent follows the session",
			limits:    store.SeedLimits{},
			wantRatio: sessionRatio,
			wantTime:  sessionTime,
		},
		{
			name:      "mode single uses the torrent's own values",
			limits:    store.SeedLimits{RatioLimit: 0.5, RatioMode: 1, IdleLimit: 30, IdleMode: 1},
			wantRatio: 0.5,
			wantTime:  30 * time.Minute,
		},
		{
			name:      "mode unlimited lifts the cap entirely",
			limits:    store.SeedLimits{RatioMode: 2, IdleMode: 2},
			wantRatio: 0,
			wantTime:  0,
		},
		{
			name:      "the two modes are independent",
			limits:    store.SeedLimits{RatioLimit: 4, RatioMode: 1, IdleMode: 2},
			wantRatio: 4,
			wantTime:  0,
		},
		{
			name:      "an idle limit in minutes becomes a duration",
			limits:    store.SeedLimits{IdleLimit: 90, IdleMode: 1},
			wantRatio: sessionRatio,
			wantTime:  90 * time.Minute,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ratio, idle := effectiveSeedLimits(tc.limits, sessionRatio, sessionTime)
			if ratio != tc.wantRatio {
				t.Errorf("ratio = %v, want %v", ratio, tc.wantRatio)
			}
			if idle != tc.wantTime {
				t.Errorf("idle = %v, want %v", idle, tc.wantTime)
			}
		})
	}
}
