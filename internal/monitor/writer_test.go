package monitor

import (
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// testLogger discards output; backends under test log through it.
func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func TestWriterRollsPerDayAndDeletesOld(t *testing.T) {
	dir := t.TempDir()
	now := time.Date(2026, 1, 2, 23, 59, 50, 0, time.UTC)
	w := newLogWriter(dir, 7, testLogger())
	w.now = func() time.Time { return now }
	defer w.close()

	// An ancient file well outside the retention window, plus one just
	// inside it: only the ancient one must go.
	old := filepath.Join(dir, "monitor-2019-06-01.jsonl")
	recent := filepath.Join(dir, "monitor-2025-12-28.jsonl")
	for _, p := range []string{old, recent} {
		if err := os.WriteFile(p, []byte("{}\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	if err := w.write(ModelEvent{Type: ModelLoadedType, TS: now, Model: "m1"}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "monitor-2026-01-02.jsonl")); err != nil {
		t.Fatalf("day file not created: %v", err)
	}
	if _, err := os.Stat(old); !os.IsNotExist(err) {
		t.Error("ancient log file was not deleted")
	}
	if _, err := os.Stat(recent); err != nil {
		t.Error("log file inside the retention window was deleted")
	}

	// Cross midnight: a second file appears, the first is untouched.
	now = now.Add(20 * time.Second) // 2026-01-03 00:00:10
	if err := w.write(ModelEvent{Type: ModelUnloadedType, TS: now, Model: "m1"}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "monitor-2026-01-03.jsonl")); err != nil {
		t.Fatalf("next day file not created: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "monitor-2026-01-02.jsonl")); err != nil {
		t.Error("previous day file disappeared")
	}
}

func TestWriterRecentFiles(t *testing.T) {
	dir := t.TempDir()
	w := newLogWriter(dir, 7, testLogger())
	for _, day := range []string{"2026-01-01", "2026-01-02", "2026-01-03", "2026-01-05"} {
		if err := os.WriteFile(filepath.Join(dir, "monitor-"+day+".jsonl"), []byte("{}\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	// Non-log files in the directory must be ignored.
	if err := os.WriteFile(filepath.Join(dir, "notes.txt"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	files := w.recentFiles(3)
	if len(files) != 3 {
		t.Fatalf("recentFiles(3) = %v, want 3 files", files)
	}
	want := "monitor-2026-01-02.jsonl"
	if filepath.Base(files[0]) != want {
		t.Errorf("files[0] = %s, want %s (ascending, newest three)", filepath.Base(files[0]), want)
	}
	if files := w.recentFiles(10); len(files) != 4 {
		t.Errorf("recentFiles(10) = %d files, want 4", len(files))
	}
}
