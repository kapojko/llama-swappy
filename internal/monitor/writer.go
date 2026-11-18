package monitor

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"sync"
	"time"
)

// fileNamePrefix and fileDateRe define the log file naming scheme:
// one JSONL file per calendar day, monitor-YYYY-MM-DD.jsonl, which
// sorts lexicographically by date — the daily "biggest" and weekly
// "biggest" windows map directly onto the newest file and the last
// seven files.
const (
	fileNamePrefix = "monitor-"
	fileNameExt   = ".jsonl"
)

var fileDateRe = regexp.MustCompile(`^monitor-(\d{4}-\d{2}-\d{2})\.jsonl$`)

// logWriter appends JSON records to the day's file, syncing after
// every line (the strata-monitor property: the log is written on the
// assumption it will be read after a hard reset, and journald-style
// tail loss is exactly what it exists to counter). It rolls to a new
// file at midnight and deletes files older than the retention window
// at every roll.
type logWriter struct {
	log           *slog.Logger
	dir           string
	retentionDays int
	now           func() time.Time

	mu  sync.Mutex
	day string
	f   *os.File
}

func newLogWriter(dir string, retentionDays int, log *slog.Logger) *logWriter {
	if retentionDays <= 0 {
		retentionDays = 7
	}
	return &logWriter{
		log:           log,
		dir:           dir,
		retentionDays: retentionDays,
		now:           time.Now,
	}
}

// write marshals rec as one JSONL line into the current day's file.
// A write error is returned to the caller, which rate-limits its
// logging; the writer itself keeps going (the next write may succeed
// once the transient cause, e.g. a full disk, is gone).
func (w *logWriter) write(rec any) error {
	line, err := json.Marshal(rec)
	if err != nil {
		return fmt.Errorf("marshal: %w", err)
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	day := w.now().Format("2006-01-02")
	if w.f == nil || day != w.day {
		if err := w.rollLocked(day); err != nil {
			return err
		}
	}
	if _, err := w.f.Write(append(line, '\n')); err != nil {
		return fmt.Errorf("write: %w", err)
	}
	if err := w.f.Sync(); err != nil {
		return fmt.Errorf("sync: %w", err)
	}
	return nil
}

// rollLocked closes the previous day's file, creates the new one and
// enforces the retention window. The caller must hold w.mu.
func (w *logWriter) rollLocked(day string) error {
	if w.f != nil {
		_ = w.f.Close()
		w.f = nil
	}
	if err := os.MkdirAll(w.dir, 0o755); err != nil {
		return fmt.Errorf("create log dir: %w", err)
	}
	f, err := os.OpenFile(w.filePath(day), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return fmt.Errorf("open log file: %w", err)
	}
	w.f = f
	w.day = day
	w.deleteOldLocked()
	return nil
}

// close flushes and closes the current file, if any.
func (w *logWriter) close() {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.f != nil {
		_ = w.f.Close()
		w.f = nil
	}
}

// filePath is the log file of the given day.
func (w *logWriter) filePath(day string) string {
	return filepath.Join(w.dir, fileNamePrefix+day+fileNameExt)
}

// deleteOldLocked removes log files older than retentionDays days.
// Failures are logged and skipped: retention must never stop the
// heartbeat. The caller must hold w.mu.
func (w *logWriter) deleteOldLocked() {
	entries, err := os.ReadDir(w.dir)
	if err != nil {
		return
	}
	today := w.now().Truncate(24 * time.Hour)
	cutoff := today.AddDate(0, 0, -(w.retentionDays - 1))
	for _, e := range entries {
		m := fileDateRe.FindStringSubmatch(e.Name())
		if m == nil {
			continue
		}
		date, err := time.ParseInLocation("2006-01-02", m[1], today.Location())
		if err != nil {
			continue
		}
		if date.Before(cutoff) {
			if err := os.Remove(filepath.Join(w.dir, e.Name())); err == nil {
				w.log.Info("monitor: old log deleted", "file", e.Name())
			}
		}
	}
}

// recentFiles returns the paths of the newest n day files (the
// current day first is not guaranteed, only the newest n), sorted by
// date ascending. It is the file list behind the daily/weekly maxima.
func (w *logWriter) recentFiles(n int) []string {
	entries, err := os.ReadDir(w.dir)
	if err != nil {
		return nil
	}
	var days []string
	for _, e := range entries {
		if fileDateRe.MatchString(e.Name()) {
			days = append(days, e.Name())
		}
	}
	sort.Strings(days)
	if len(days) > n {
		days = days[len(days)-n:]
	}
	out := make([]string, 0, len(days))
	for _, d := range days {
		out = append(out, filepath.Join(w.dir, d))
	}
	return out
}

// todayFile returns the path of today's log file (which may not exist
// yet — the reader treats a missing file as no samples).
func (w *logWriter) todayFile() string {
	return w.filePath(w.now().Format("2006-01-02"))
}
