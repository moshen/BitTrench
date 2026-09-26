package logging

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestWritesToTodaysFile(t *testing.T) {
	dir := t.TempDir()
	w, err := NewRollingWriter(dir, "bittrench", 7)
	if err != nil {
		t.Fatalf("NewRollingWriter: %v", err)
	}
	defer w.Close()

	if _, err := w.Write([]byte("hello\n")); err != nil {
		t.Fatalf("Write: %v", err)
	}
	name := fmt.Sprintf("bittrench-%s.log", time.Now().UTC().Format(dateLayout))
	got, err := os.ReadFile(filepath.Join(dir, name))
	if err != nil {
		t.Fatalf("reading %s: %v", name, err)
	}
	if string(got) != "hello\n" {
		t.Errorf("log contents = %q", got)
	}
}

// The day boundary is the whole point of the writer, and it is the part that
// only ever exercises itself at midnight in production.
func TestRotatesOnTheUTCDayBoundary(t *testing.T) {
	dir := t.TempDir()
	w, err := NewRollingWriter(dir, "wg", 0)
	if err != nil {
		t.Fatalf("NewRollingWriter: %v", err)
	}
	defer w.Close()
	if _, err := w.Write([]byte("day one\n")); err != nil {
		t.Fatalf("Write: %v", err)
	}

	tomorrow := time.Now().UTC().AddDate(0, 0, 1)
	if err := w.rotate(tomorrow); err != nil {
		t.Fatalf("rotate: %v", err)
	}
	if _, err := w.file.WriteString("day two\n"); err != nil {
		t.Fatalf("write after rotate: %v", err)
	}

	today := fmt.Sprintf("wg-%s.log", time.Now().UTC().Format(dateLayout))
	next := fmt.Sprintf("wg-%s.log", tomorrow.Format(dateLayout))
	if got := readFile(t, dir, today); got != "day one\n" {
		t.Errorf("today's file = %q", got)
	}
	if got := readFile(t, dir, next); got != "day two\n" {
		t.Errorf("tomorrow's file = %q", got)
	}
}

func TestPruneDeletesOnlyOldMatchingFiles(t *testing.T) {
	dir := t.TempDir()
	now := time.Now().UTC()
	stale := fmt.Sprintf("wg-%s.log", now.AddDate(0, 0, -9).Format(dateLayout))
	edge := fmt.Sprintf("wg-%s.log", now.AddDate(0, 0, -7).Format(dateLayout))
	fresh := fmt.Sprintf("wg-%s.log", now.AddDate(0, 0, -1).Format(dateLayout))
	// Files that are not ours, or not date-stamped, must survive: the log
	// directory is the operator's, and it may hold anything.
	foreign := "other-2000-01-01.log"
	unstamped := "wg-notes.log"
	for _, n := range []string{stale, edge, fresh, foreign, unstamped} {
		if err := os.WriteFile(filepath.Join(dir, n), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	// Opening the writer prunes immediately, which is what a daemon restart
	// on a long-stale log directory relies on.
	w, err := NewRollingWriter(dir, "wg", 7)
	if err != nil {
		t.Fatalf("NewRollingWriter: %v", err)
	}
	defer w.Close()

	if exists(t, dir, stale) {
		t.Errorf("%s should have been pruned", stale)
	}
	for _, n := range []string{edge, fresh, foreign, unstamped} {
		if !exists(t, dir, n) {
			t.Errorf("%s should have been kept", n)
		}
	}
}

// max_age_days = 0 disables cleanup entirely.
func TestPruneDisabled(t *testing.T) {
	dir := t.TempDir()
	ancient := "wg-1999-12-31.log"
	if err := os.WriteFile(filepath.Join(dir, ancient), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	w, err := NewRollingWriter(dir, "wg", 0)
	if err != nil {
		t.Fatalf("NewRollingWriter: %v", err)
	}
	defer w.Close()
	if !exists(t, dir, ancient) {
		t.Error("max_age_days = 0 must disable pruning")
	}
}

func readFile(t *testing.T, dir, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, name))
	if err != nil {
		t.Fatalf("reading %s: %v", name, err)
	}
	return string(b)
}

func exists(t *testing.T, dir, name string) bool {
	t.Helper()
	_, err := os.Stat(filepath.Join(dir, name))
	return err == nil
}
