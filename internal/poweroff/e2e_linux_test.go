//go:build linux

package poweroff

import (
	"log/slog"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"llama-swappy/internal/config"
)

// E2E tests against the real machine (WSL included). They are skipped
// unless LLAMA_SWAPPY_E2E=1 is set:
//
//	LLAMA_SWAPPY_E2E=1 go test ./internal/poweroff -run E2E -v
//
// Warning: TestE2EPoweroffFlow runs the real poweroff command (with a
// reduced 5s timer), so the machine — or the WSL VM, on WSL2 — shuts
// down. Run it deliberately; the go test output does not come back.
func e2eEnabled(t *testing.T) {
	t.Helper()
	if os.Getenv("LLAMA_SWAPPY_E2E") == "" {
		t.Skip("e2e: set LLAMA_SWAPPY_E2E=1 to run poweroff e2e tests")
	}
}

// TestE2ESafetyCheck exercises the real /proc scan with a tmux session
// started and killed by the test.
func TestE2ESafetyCheck(t *testing.T) {
	e2eEnabled(t)
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux not installed; cannot exercise the unsafe path")
	}
	c := safetyChecker{}
	h0, d0 := c.Check()
	t.Logf("baseline: safe=%v detail=%q", h0, d0)

	if err := exec.Command("tmux", "new-session", "-d", "-s", "llama-swappy-e2e").Run(); err != nil {
		t.Fatalf("start tmux: %v", err)
	}
	killTmux := func() { _ = exec.Command("tmux", "kill-session", "-t", "llama-swappy-e2e").Run() }
	defer killTmux()
	time.Sleep(200 * time.Millisecond)

	h1, d1 := c.Check()
	if h1 || !strings.Contains(d1, "tmux") {
		t.Fatalf("with tmux running: safe=%v detail=%q, want unsafe with tmux", h1, d1)
	}

	killTmux()
	time.Sleep(200 * time.Millisecond)
	h2, d2 := c.Check()
	if h0 && !h2 {
		t.Fatalf("after tmux exit: safe=%v detail=%q, want back to baseline (safe)", h2, d2)
	}
	if !strings.Contains(d0, "tmux") && strings.Contains(d2, "tmux") {
		t.Fatalf("after tmux exit: detail %q still mentions tmux", d2)
	}
}

// TestE2EPoweroffFlow runs the full real scenario with a reduced 5s
// timer: model load+unload -> countdown -> real safety check -> real
// poweroff command. It is destructive: on success the machine/WSL VM
// goes down and the test process dies with it (a missing "PASS" line
// in the go test output is the pass signal).
func TestE2EPoweroffFlow(t *testing.T) {
	e2eEnabled(t)
	// The flow must reach the command execution; a machine with
	// active sessions (sshd/screen/tmux) would restart the countdown
	// forever.
	c := safetyChecker{}
	if safe, detail := c.Check(); !safe {
		t.Skipf("system is not safe to power off, refusing to run the destructive test (%s)", detail)
	}
	log := slog.New(slog.NewTextHandler(os.Stderr, nil))
	po, err := New(&config.PoweroffConfig{Enabled: true, IdleSeconds: 5}, log)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer po.Close()
	po.OnLoaded()
	po.OnUnloaded()
	t.Logf("COUNTDOWN: the real poweroff command (%s) will run in ~5s and this machine/WSL VM will shut down", po.Command())

	// Wait until the counter leaves "counting" (initiated or failed).
	deadline := time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) {
		if st := po.Status(); st.State == StateInitiated || st.State == StateFailed {
			break
		}
		time.Sleep(250 * time.Millisecond)
	}
	switch st := po.Status(); st.State {
	case StateInitiated:
		// The command ran; the system should be going down within
		// seconds. If we are still alive long after, report it.
		time.Sleep(30 * time.Second)
		t.Fatal("poweroff command succeeded but the machine did not shut down within 30s (on WSL: is systemd enabled in this distro?)")
	case StateFailed:
		t.Fatalf("poweroff command failed: %s", st.Error)
	default:
		t.Fatalf("state = %s, want initiated or failed (note: %s)", st.State, st.Note)
	}
}
