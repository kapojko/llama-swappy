//go:build unix

package model

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// testOut mirrors production wiring (the app passes os.Stdout, a real file):
// with an io.Writer that is not an *os.File, exec creates a pipe and Wait
// then also waits for the grandchild to close it, which would hide the very
// case under test.
func testOut(t *testing.T) *os.File {
	t.Helper()
	f, err := os.OpenFile(os.DevNull, os.O_WRONLY, 0)
	if err != nil {
		t.Fatalf("open %s: %v", os.DevNull, err)
	}
	t.Cleanup(func() { _ = f.Close() })
	return f
}

// alive reports whether pid still exists (zombies count as gone: kill -0
// succeeds for them, so also check the process state).
func alive(pid int) bool {
	if err := syscall.Kill(pid, 0); err != nil {
		return false
	}
	data, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return false
	}
	// state is the field right after the parenthesised command name.
	if i := strings.LastIndexByte(string(data), ')'); i >= 0 && i+2 < len(data) {
		return data[i+2] != 'Z'
	}
	return true
}

// TestKillStopsServerLeftBehindByWrapper covers a wrapper script that dies on
// SIGTERM before the server it spawned does: the direct child is gone, the
// server is not, and it ignores SIGTERM (llama.cpp's second-interrupt path
// can deadlock in GPU teardown). Only the escalation to SIGKILL on the group
// ends it -- waiting for the direct child alone would report success while
// the server keeps holding VRAM and the port.
func TestKillStopsServerLeftBehindByWrapper(t *testing.T) {
	dir := t.TempDir()
	pidFile := filepath.Join(dir, "child.pid")
	script := filepath.Join(dir, "wrapper.sh")
	// run_*.sh templates are bash scripts; bash is what exits at the first
	// SIGTERM here (dash does too, but the template shape is bash).
	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("bash not available")
	}
	// The wrapper starts a grandchild that ignores SIGTERM (stands in for a
	// hung llama-server) and then loops in short sleeps, with an explicit
	// TERM trap so it exits at the first signal instead of waiting for its
	// foreground child -- exactly the shape of `run_*.sh` without `exec`.
	body := "#!/bin/bash\n" +
		"trap 'exit 0' TERM\n" +
		"sh -c 'trap \"\" TERM; while :; do sleep 1; done' &\n" +
		"echo $! > " + pidFile + "\n" +
		"while :; do sleep 0.1; done\n"
	if err := os.WriteFile(script, []byte(body), 0o700); err != nil {
		t.Fatalf("write wrapper: %v", err)
	}

	h, err := (ProcessStarter{}).Start(bash+" "+script, testOut(t))
	if err != nil {
		t.Fatalf("start: %v", err)
	}

	var raw []byte
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if raw, err = os.ReadFile(pidFile); err == nil && len(raw) > 0 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	child, err := strconv.Atoi(strings.TrimSpace(string(raw)))
	if err != nil || child <= 0 {
		t.Fatalf("parse grandchild pid %q: %v", raw, err)
	}
	if !alive(child) {
		t.Fatalf("grandchild %d should be running", child)
	}

	start := time.Now()
	if err := h.Kill(); err != nil {
		t.Fatalf("Kill: %v", err)
	}
	elapsed := time.Since(start)

	if alive(child) {
		_ = syscall.Kill(child, syscall.SIGKILL)
		t.Fatalf("grandchild %d survived Kill (%v): the group was not watched", child, elapsed)
	}
	if elapsed < termGrace/2 {
		t.Errorf("Kill returned after %v: the wrapper was expected to exit on SIGTERM "+
			"while the grandchild ignores it, forcing the group escalation", elapsed)
	}
	if elapsed > termGrace+killGrace+2*time.Second {
		t.Errorf("Kill took %v, expected at most about %v", elapsed, termGrace+killGrace)
	}
}

// TestKillReturnsPromptlyForWellBehavedServer guards the common path: a
// server that exits on SIGTERM must not pay the grace period.
func TestKillReturnsPromptlyForWellBehavedServer(t *testing.T) {
	h, err := (ProcessStarter{}).Start("/bin/sh -c 'exec sleep 300'", testOut(t))
	if err != nil {
		t.Fatalf("start: %v", err)
	}

	start := time.Now()
	if err := h.Kill(); err != nil {
		t.Fatalf("Kill: %v", err)
	}
	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Fatalf("Kill took %v for a server that exits on SIGTERM", elapsed)
	}
}
