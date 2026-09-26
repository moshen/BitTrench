// Package logging provides the daily-rotating file writer behind `log/slog`.
//
// One file per UTC day is written under a configurable directory using the
// naming scheme `<prefix>-YYYY-MM-DD.log`. When the UTC day changes the writer
// opens a new file and deletes files older than `max_age_days`.
//
// Why hand-rolled? The standard library has no log-rotation facility and none
// is planned, but retention plus a daily boundary is ~60 lines, and the
// dependency rule in AGENTS.md is worth more than that. This is a deliberate
// choice, not an oversight.
package logging

import (
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

const dateLayout = "2006-01-02"

// RollingWriter is an io.WriteCloser that swaps files on the UTC day boundary.
// It is safe for concurrent use: slog handlers write from any goroutine.
type RollingWriter struct {
	dir        string
	prefix     string
	maxAgeDays uint32

	mu   sync.Mutex
	file *os.File
	day  string // the UTC date currently open, in dateLayout form
}

// NewRollingWriter creates dir if needed, opens today's file and prunes stale
// ones immediately, so an existing log directory is tidied at startup rather
// than at the next midnight.
func NewRollingWriter(dir, prefix string, maxAgeDays uint32) (*RollingWriter, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("failed to create log directory %s: %w", dir, err)
	}
	w := &RollingWriter{dir: dir, prefix: prefix, maxAgeDays: maxAgeDays}
	if err := w.rotate(time.Now().UTC()); err != nil {
		return nil, err
	}
	return w, nil
}

func (w *RollingWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if err := w.rotate(time.Now().UTC()); err != nil {
		return 0, err
	}
	return w.file.Write(p)
}

// Close closes the current file. The writer is unusable afterwards.
func (w *RollingWriter) Close() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.file == nil {
		return nil
	}
	err := w.file.Close()
	w.file = nil
	return err
}

// rotate opens the file for now's UTC date if a different one (or none) is
// open. Callers other than NewRollingWriter must hold w.mu.
func (w *RollingWriter) rotate(now time.Time) error {
	day := now.Format(dateLayout)
	if day == w.day && w.file != nil {
		return nil
	}
	path := filepath.Join(w.dir, fmt.Sprintf("%s-%s.log", w.prefix, day))
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return fmt.Errorf("failed to open log file %s: %w", path, err)
	}
	if w.file != nil {
		w.file.Close()
	}
	w.file = f
	w.day = day
	w.prune(now)
	return nil
}

// prune deletes log files older than maxAgeDays. Errors on individual files
// are ignored: a cleanup hiccup must never break logging.
func (w *RollingWriter) prune(now time.Time) {
	if w.maxAgeDays == 0 {
		return
	}
	cutoff := now.AddDate(0, 0, -int(w.maxAgeDays))
	entries, err := os.ReadDir(w.dir)
	if err != nil {
		return
	}
	stem := w.prefix + "-"
	for _, e := range entries {
		name := e.Name()
		if !strings.HasPrefix(name, stem) || !strings.HasSuffix(name, ".log") {
			continue
		}
		datePart := name[len(stem) : len(name)-len(".log")]
		t, err := time.Parse(dateLayout, datePart)
		if err != nil {
			continue
		}
		if t.Before(cutoff.Truncate(24 * time.Hour)) {
			os.Remove(filepath.Join(w.dir, name))
		}
	}
}

// Setup installs the process-wide slog logger, writing to stderr and - when a
// log directory is usable - to a daily-rotating file as well. It returns the
// rolling writer so shutdown can close it; the writer is nil when file logging
// could not be started, which is a warning rather than a fatal error, since a
// daemon that cannot write logs should still torrent.
//
// Call exactly once per process. The level follows BITTRENCH_LOG, and falls
// back to info when it is unset or unparseable.
func Setup(logDir, prefix string, maxAgeDays uint32) *RollingWriter {
	var level slog.Level
	if err := level.UnmarshalText([]byte(os.Getenv("BITTRENCH_LOG"))); err != nil {
		level = slog.LevelInfo
	}

	var out io.Writer = os.Stderr
	writer, err := NewRollingWriter(logDir, prefix, maxAgeDays)
	if err != nil {
		fmt.Fprintf(os.Stderr, "warning: file logging disabled - %v\n", err)
	} else {
		out = io.MultiWriter(os.Stderr, writer)
	}

	slog.SetDefault(slog.New(slog.NewTextHandler(out, &slog.HandlerOptions{Level: level})))
	return writer
}
