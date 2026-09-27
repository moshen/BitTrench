package engine

import (
	"fmt"
	"log/slog"
	"time"

	"github.com/moshen/bittrench/internal/store"
)

// monitorInterval is how often completion and the seed caps are re-checked.
// Ten seconds is often enough that a cap is enforced promptly and rare enough
// that the walk over every torrent costs nothing.
const monitorInterval = 10 * time.Second

// runSampler differences the cumulative counters into rates, once a second.
func (e *Engine) runSampler() {
	defer e.wg.Done()
	ticker := time.NewTicker(sampleInterval)
	defer ticker.Stop()
	for {
		select {
		case <-e.ctx.Done():
			return
		case now := <-ticker.C:
			for _, rec := range e.snapshot() {
				e.sampler.observe(rec.ID, rec.Torrent.Stats(), now)
			}
		}
	}
}

// runMonitor tracks completion and enforces the seed ratio and seed time caps.
//
// Both caps need a record of when a torrent first finished, which nothing in
// anacrolix/torrent provides, so it is recorded here and persisted - a
// restart must not reset the seeding clock.
func (e *Engine) runMonitor() {
	defer e.wg.Done()

	ratioLimit := e.cfg.Torrent.Limits.SeedRatioLimit
	timeLimit := time.Duration(e.cfg.Torrent.Limits.SeedTimeLimitSecs) * time.Second
	if ratioLimit <= 0 && timeLimit == 0 {
		slog.Info("seed ratio and time caps disabled")
	} else {
		slog.Info("seed monitor started",
			"seed_ratio_limit", ratioLimit, "seed_time_limit_secs", e.cfg.Torrent.Limits.SeedTimeLimitSecs)
	}

	ticker := time.NewTicker(monitorInterval)
	defer ticker.Stop()
	for {
		select {
		case <-e.ctx.Done():
			return
		case <-ticker.C:
			e.checkSeedLimits(ratioLimit, timeLimit)
		}
	}
}

func (e *Engine) checkSeedLimits(ratioLimit float64, timeLimit time.Duration) {
	now := time.Now()
	for _, rec := range e.snapshot() {
		t := rec.Torrent
		if t.Info() == nil {
			continue
		}
		complete := t.BytesMissing() == 0

		e.mu.Lock()
		finishedAt := rec.FinishedAt
		paused := rec.Paused
		limits := rec.SeedLimits
		switch {
		case complete && finishedAt.IsZero():
			rec.FinishedAt = now
			finishedAt = now
		case !complete && !finishedAt.IsZero():
			// It stopped being complete - a file was removed underneath us,
			// or a previously skipped file was selected. Restart the clock.
			rec.FinishedAt = time.Time{}
			finishedAt = time.Time{}
		}
		e.mu.Unlock()

		if finishedAt != rec.storedFinishedAt {
			if err := e.store.SetFinishedAt(e.ctx, rec.ID, finishedAt); err != nil {
				slog.Warn("failed to record the finish time", "id", rec.ID, "error", err)
			} else {
				rec.storedFinishedAt = finishedAt
			}
		}

		if !complete || paused || finishedAt.IsZero() {
			continue
		}

		// A torrent's own caps win over the session's when its mode says so.
		effectiveRatio, effectiveTime := effectiveSeedLimits(limits, ratioLimit, timeLimit)

		stats := t.Stats()
		reason := seedLimitReason(stats.BytesWrittenData.Int64(), t.Length(),
			now.Sub(finishedAt), effectiveRatio, effectiveTime)
		if reason == "" {
			continue
		}
		slog.Info("seed limit reached, pausing torrent",
			"id", rec.ID, "name", t.Name(), "reason", reason)
		if err := e.Stop(e.ctx, rec.ID); err != nil {
			slog.Warn("failed to pause a torrent at its seed limit", "id", rec.ID, "error", err)
		}
	}
}

// Transmission's per-torrent limit modes, as a client sets them through
// torrent-set and reads them back through torrent-get.
const (
	// seedLimitGlobal follows the session-wide limit.
	seedLimitGlobal = 0
	// seedLimitSingle uses the torrent's own value.
	seedLimitSingle = 1
	// seedLimitUnlimited seeds without a cap.
	seedLimitUnlimited = 2
)

// effectiveSeedLimits resolves one torrent's caps against the session's.
//
// A zero ratio limit and a zero duration both mean "no cap", which is how the
// session limits already express being disabled - so unlimited and a disabled
// session limit land on the same values, deliberately.
func effectiveSeedLimits(l store.SeedLimits, sessionRatio float64, sessionTime time.Duration) (float64, time.Duration) {
	ratio := sessionRatio
	switch l.RatioMode {
	case seedLimitSingle:
		ratio = l.RatioLimit
	case seedLimitUnlimited:
		ratio = 0
	}

	idle := sessionTime
	switch l.IdleMode {
	case seedLimitSingle:
		// Transmission carries the idle limit in minutes.
		idle = time.Duration(l.IdleLimit) * time.Minute
	case seedLimitUnlimited:
		idle = 0
	}
	return ratio, idle
}

// seedLimitReason returns a human-readable reason when a cap is exceeded, or
// "" when neither is. A ratio limit <= 0 disables the ratio cap; a zero time
// limit disables the time cap.
func seedLimitReason(uploaded, total int64, seeded time.Duration, ratioLimit float64, timeLimit time.Duration) string {
	if ratioLimit > 0 && total > 0 {
		ratio := float64(uploaded) / float64(total)
		if ratio >= ratioLimit {
			return fmt.Sprintf("ratio %.2f >= %.2f", ratio, ratioLimit)
		}
	}
	if timeLimit > 0 && seeded >= timeLimit {
		return fmt.Sprintf("seeded for %s >= %s", seeded.Round(time.Second), timeLimit)
	}
	return ""
}
