package backup

import (
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"llama-swappy/internal/config"
)

func discard() *slog.Logger {
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(data)
}

func changelogPath(root, machine string) string {
	return filepath.Join(root, sanitizeName(machine), changelogName)
}

func TestSyncLifecycle(t *testing.T) {
	log := discard()
	backupRoot := t.TempDir()
	cur := t.TempDir()
	machine := "workpc_linux"

	cfgPath := filepath.Join(cur, "llama-swap-config.yaml")
	script := filepath.Join(cur, "run_a.sh")
	writeFile(t, cfgPath, "v1")
	writeFile(t, script, "echo a")

	sources := []Source{
		{Src: cfgPath, Name: "llama-swap-config.yaml"},
		{Src: script, Name: "run_a.sh"},
	}
	machineDir := filepath.Join(backupRoot, machine)
	cl := changelogPath(backupRoot, machine)

	// First sync: both files added, changelog created.
	if err := Sync(Options{Dir: backupRoot, MachineName: machine, Sources: sources}, log); err != nil {
		t.Fatalf("first sync: %v", err)
	}
	if got := readFile(t, filepath.Join(machineDir, "llama-swap-config.yaml")); got != "v1" {
		t.Errorf("config backup = %q, want v1", got)
	}
	if got := readFile(t, filepath.Join(machineDir, "run_a.sh")); got != "echo a" {
		t.Errorf("script backup = %q, want 'echo a'", got)
	}
	cl1 := readFile(t, cl)
	if !strings.Contains(cl1, "added llama-swap-config.yaml") || !strings.Contains(cl1, "added run_a.sh") {
		t.Errorf("changelog missing added entries: %q", cl1)
	}

	// Second sync with no changes: changelog unchanged.
	if err := Sync(Options{Dir: backupRoot, MachineName: machine, Sources: sources}, log); err != nil {
		t.Fatalf("second sync: %v", err)
	}
	if got := readFile(t, cl); got != cl1 {
		t.Errorf("changelog changed without modification:\n%s", got)
	}

	// Modify the script: it is updated and recorded.
	writeFile(t, script, "echo a2")
	if err := Sync(Options{Dir: backupRoot, MachineName: machine, Sources: sources}, log); err != nil {
		t.Fatalf("third sync: %v", err)
	}
	if got := readFile(t, filepath.Join(machineDir, "run_a.sh")); got != "echo a2" {
		t.Errorf("script backup = %q, want 'echo a2'", got)
	}
	cl3 := readFile(t, cl)
	if !strings.Contains(cl3, "updated run_a.sh") {
		t.Errorf("changelog missing 'updated run_a.sh': %q", cl3)
	}

	// Drop the script from the source list: it is removed from backup.
	if err := Sync(Options{Dir: backupRoot, MachineName: machine, Sources: sources[:1]}, log); err != nil {
		t.Fatalf("fourth sync: %v", err)
	}
	if _, err := os.Stat(filepath.Join(machineDir, "run_a.sh")); !os.IsNotExist(err) {
		t.Errorf("run_a.sh still present, want removed (err=%v)", err)
	}
	if cl := readFile(t, cl); !strings.Contains(cl, "removed run_a.sh") {
		t.Errorf("changelog missing 'removed run_a.sh': %q", cl)
	}
}

func TestSyncKeepsChangelogOnOutOfBandFile(t *testing.T) {
	log := discard()
	backupRoot := t.TempDir()
	machine := "m1"
	machineDir := filepath.Join(backupRoot, machine)
	if err := os.MkdirAll(machineDir, 0o755); err != nil {
		t.Fatal(err)
	}

	// A file dropped into the machine dir that is not a source is
	// removed as outdated, but CHANGELOG.txt is preserved.
	writeFile(t, filepath.Join(machineDir, "stale.sh"), "old")
	writeFile(t, filepath.Join(machineDir, changelogName), "2024-01-01 00:00:00\n  added x\n")

	src := filepath.Join(t.TempDir(), "cfg.yaml")
	writeFile(t, src, "v1")

	if err := Sync(Options{Dir: backupRoot, MachineName: machine, Sources: []Source{{Src: src, Name: "cfg.yaml"}}}, log); err != nil {
		t.Fatalf("sync: %v", err)
	}
	if _, err := os.Stat(filepath.Join(machineDir, "stale.sh")); !os.IsNotExist(err) {
		t.Errorf("stale.sh not removed")
	}
	cl := readFile(t, filepath.Join(machineDir, changelogName))
	if !strings.Contains(cl, "added x") || !strings.Contains(cl, "removed stale.sh") {
		t.Errorf("changelog should keep old entries and add removal: %q", cl)
	}
}

func TestSourcesForConfig(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "my-config.yaml")
	writeFile(t, cfgPath, "x")
	writeFile(t, filepath.Join(dir, "run_a.sh"), "#!/bin/sh")
	writeFile(t, filepath.Join(dir, "run_b.ps1"), "param()")

	cfg := &config.Config{
		GlobalTTL: 900,
		StartPort: 12390,
		Models: map[string]config.Model{
			"a": {Name: "A", Cmd: "env PORT=${PORT} run_a.sh", Proxy: "http://127.0.0.1:${PORT}"},
			"b": {Name: "B", Cmd: "powershell -File run_b.ps1 -Port ${PORT}", Proxy: "http://127.0.0.1:${PORT}"},
			"c": {Name: "C", Cmd: "/usr/local/bin/llama-server -c 8192", Proxy: "http://127.0.0.1:${PORT}"},
		},
	}

	sources, warns := SourcesForConfig(cfg, cfgPath)
	if len(warns) != 0 {
		t.Errorf("unexpected warnings: %v", warns)
	}
	names := map[string]bool{}
	for _, s := range sources {
		names[s.Name] = true
		// Every source path must be absolute and, for scripts, resolved
		// against the config file's directory.
		if !filepath.IsAbs(s.Src) {
			t.Errorf("source %q not absolute", s.Src)
		}
	}
	want := []string{"my-config.yaml", "run_a.sh", "run_b.ps1"}
	for _, w := range want {
		if !names[w] {
			t.Errorf("missing source %q; got %v", w, names)
		}
	}
	if names["/usr/local/bin/llama-server"] || len(sources) != 3 {
		t.Errorf("unexpected sources (server binary should be excluded): %v", names)
	}
}

func TestSyncCreatesDirectories(t *testing.T) {
	log := discard()
	// A backup root that does not exist yet (and is nested): both the
	// root and the machine sub-directory must be created by Sync.
	nested := filepath.Join(t.TempDir(), "a", "b")
	src := filepath.Join(t.TempDir(), "cfg.yaml")
	writeFile(t, src, "v1")

	err := Sync(Options{Dir: nested, MachineName: "m", Sources: []Source{{Src: src, Name: "cfg.yaml"}}}, log)
	if err != nil {
		t.Fatalf("sync: %v", err)
	}
	if _, err := os.Stat(filepath.Join(nested, "m", "cfg.yaml")); err != nil {
		t.Errorf("expected created backup file, got %v", err)
	}
}

func TestSyncWriteFailure(t *testing.T) {
	log := discard()
	root := t.TempDir()
	machineDir := filepath.Join(root, "m")
	if err := os.MkdirAll(machineDir, 0o755); err != nil {
		t.Fatal(err)
	}
	// Make the destination a directory so the write into it must fail.
	if err := os.MkdirAll(filepath.Join(machineDir, "cfg.yaml"), 0o755); err != nil {
		t.Fatal(err)
	}
	src := filepath.Join(t.TempDir(), "cfg.yaml")
	writeFile(t, src, "v1")

	if err := Sync(Options{Dir: root, MachineName: "m", Sources: []Source{{Src: src, Name: "cfg.yaml"}}}, log); err == nil {
		t.Fatal("expected an error when the backup write fails")
	}
}

func TestSyncMissingSource(t *testing.T) {
	log := discard()
	missing := filepath.Join(t.TempDir(), "nope.yaml")
	if err := Sync(Options{Dir: t.TempDir(), MachineName: "m", Sources: []Source{{Src: missing, Name: "nope.yaml"}}}, log); err == nil {
		t.Fatal("expected an error when the source file is missing")
	}
}

func TestDefaultMachineName(t *testing.T) {
	got := DefaultMachineName()
	if got == "" {
		t.Fatal("DefaultMachineName returned empty")
	}
	if strings.ContainsAny(got, `\/:*?\"<>|`) {
		t.Errorf("DefaultMachineName has unsafe chars: %q", got)
	}
	if !strings.HasSuffix(got, "_"+runtime.GOOS) {
		t.Errorf("DefaultMachineName = %q, want suffix _%s", got, runtime.GOOS)
	}
}
