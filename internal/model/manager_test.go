package model

import (
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"sync"
	"testing"
	"time"

	"llama-swappy/internal/config"
)

// fakeHandle mimics a running process: Wait blocks until the process
// "exits" (Kill sends a clean exit, crash sends an unexpected one) and
// returns the cached exit error idempotently.
type fakeHandle struct {
	mu     sync.Mutex
	killed bool
	done   chan error
	res    error
	reaped bool
}

// Kill signals a clean exit first (without the lock, since Wait may be
// blocked on the channel while holding it), then records the kill.
func (h *fakeHandle) Kill() error {
	h.signalExit(nil)
	h.mu.Lock()
	h.killed = true
	h.mu.Unlock()
	return nil
}

// crash simulates an unexpected process exit, e.g. a segfault.
func (h *fakeHandle) crash() {
	h.signalExit(fmt.Errorf("exit status 139 (signal: segmentation fault)"))
}

func (h *fakeHandle) signalExit(err error) {
	select {
	case h.done <- err:
	default:
	}
}

func (h *fakeHandle) Wait() error {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.reaped {
		return h.res
	}
	h.res = <-h.done
	h.reaped = true
	return h.res
}

func (h *fakeHandle) wasKilled() bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.killed
}

type fakeStarter struct {
	mu      sync.Mutex
	handles []*fakeHandle
	cmds    []string
}

func (s *fakeStarter) Start(cmd string, _ io.Writer) (Handle, error) {
	h := &fakeHandle{done: make(chan error, 1)}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.handles = append(s.handles, h)
	s.cmds = append(s.cmds, cmd)
	return h, nil
}

func (s *fakeStarter) last() *fakeHandle {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.handles) == 0 {
		return nil
	}
	return s.handles[len(s.handles)-1]
}

func quietLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// newTestManager wires a manager whose single model port is the port of
// ts, an httptest server standing in for the real model.
func newTestManager(t *testing.T, ts *httptest.Server, ttl time.Duration, readyTimeout time.Duration, restartDelay time.Duration) (*Manager, *fakeStarter) {
	t.Helper()
	u, err := url.Parse(ts.URL)
	if err != nil {
		t.Fatalf("parse test server url: %v", err)
	}
	port, err := strconv.Atoi(u.Port())
	if err != nil {
		t.Fatalf("parse port: %v", err)
	}
	cfg := &config.Config{
		GlobalTTL: 900,
		StartPort: port,
		Models: map[string]config.Model{
			"m1": {Name: "Model One", Cmd: "cmd1 ${PORT}", Proxy: "http://127.0.0.1:${PORT}"},
			"m2": {Name: "Model Two", Cmd: "cmd2", Proxy: "http://127.0.0.1:${PORT}"},
		},
	}
	fs := &fakeStarter{}
	m := New(cfg, Options{
		Logger:       quietLogger(),
		Out:          io.Discard,
		Starter:      fs,
		TTL:          ttl,
		ReadyTimeout: readyTimeout,
		PollInterval: 20 * time.Millisecond,
		RestartDelay: restartDelay,
	})
	t.Cleanup(m.Close)
	return m, fs
}

// waitForCurrent polls until Current() equals want or the test fails.
func waitForCurrent(t *testing.T, m *Manager, want string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for m.Current() != want && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if m.Current() != want {
		t.Fatalf("Current = %q, want %q", m.Current(), want)
	}
}

func readyBackend() *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
}

func TestEnsureModelStarts(t *testing.T) {
	ts := readyBackend()
	defer ts.Close()
	m, fs := newTestManager(t, ts, time.Minute, 5*time.Second, time.Second)

	got, err := m.EnsureModel("m1")
	if err != nil {
		t.Fatalf("EnsureModel: %v", err)
	}
	if want := ts.URL; got != want {
		t.Errorf("proxy URL = %q, want %q", got, want)
	}
	if m.Current() != "m1" {
		t.Errorf("Current = %q, want m1", m.Current())
	}
	if fs.last() == nil || fs.last().wasKilled() {
		t.Error("model handle should be running, not killed")
	}

	// Second call for the same model must not restart it.
	before := len(fs.handles)
	if _, err := m.EnsureModel("m1"); err != nil {
		t.Fatalf("second EnsureModel: %v", err)
	}
	if len(fs.handles) != before {
		t.Errorf("handle count changed on same-model EnsureModel")
	}
}

func TestEnsureModelSubstitutesPort(t *testing.T) {
	ts := readyBackend()
	defer ts.Close()
	m, fs := newTestManager(t, ts, time.Minute, 5*time.Second, time.Second)

	if _, err := m.EnsureModel("m1"); err != nil {
		t.Fatalf("EnsureModel: %v", err)
	}
	u, err := url.Parse(ts.URL)
	if err != nil {
		t.Fatalf("parse test server url: %v", err)
	}
	port, _ := strconv.Atoi(u.Port())
	if len(fs.cmds) != 1 {
		t.Fatalf("expected 1 start, got %d", len(fs.cmds))
	}
	want := "cmd1 " + strconv.Itoa(port)
	if fs.cmds[0] != want {
		t.Errorf("cmd = %q, want %q", fs.cmds[0], want)
	}
}

func TestEnsureModelUnknownKey(t *testing.T) {
	ts := readyBackend()
	defer ts.Close()
	m, _ := newTestManager(t, ts, time.Minute, 5*time.Second, time.Second)
	if _, err := m.EnsureModel("nope"); err == nil {
		t.Fatal("expected error for unknown model")
	}
}

func TestSwapOutPrevious(t *testing.T) {
	ts := readyBackend()
	defer ts.Close()
	m, fs := newTestManager(t, ts, time.Minute, 5*time.Second, time.Second)

	if _, err := m.EnsureModel("m1"); err != nil {
		t.Fatalf("EnsureModel(m1): %v", err)
	}
	first := fs.last()
	if _, err := m.EnsureModel("m2"); err != nil {
		t.Fatalf("EnsureModel(m2): %v", err)
	}
	if m.Current() != "m2" {
		t.Errorf("Current = %q, want m2", m.Current())
	}
	if !first.wasKilled() {
		t.Error("previous model handle should have been killed on swap")
	}
}

func TestIdleUnload(t *testing.T) {
	ts := readyBackend()
	defer ts.Close()
	m, fs := newTestManager(t, ts, 100*time.Millisecond, 5*time.Second, time.Second)

	if _, err := m.EnsureModel("m1"); err != nil {
		t.Fatalf("EnsureModel: %v", err)
	}
	if m.Current() == "" {
		t.Fatal("model should be loaded right after EnsureModel")
	}
	deadline := time.Now().Add(3 * time.Second)
	for m.Current() != "" && time.Now().Before(deadline) {
		time.Sleep(50 * time.Millisecond)
	}
	if m.Current() != "" {
		t.Fatalf("model still loaded after idle TTL; handles: %d", len(fs.handles))
	}
	if h := fs.last(); h == nil || !h.wasKilled() {
		t.Error("handle should have been killed by idle unload")
	}
}

func TestReadinessTimeout(t *testing.T) {
	// Backend whose /health never succeeds.
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer ts.Close()
	m, fs := newTestManager(t, ts, time.Minute, 100*time.Millisecond, time.Second)

	_, err := m.EnsureModel("m1")
	if err == nil {
		t.Fatal("expected readiness timeout error")
	}
	if h := fs.last(); h == nil || !h.wasKilled() {
		t.Error("failed model handle should have been killed")
	}
	if m.Current() != "" {
		t.Errorf("Current = %q, want empty after failed start", m.Current())
	}
}

func TestStopAll(t *testing.T) {
	ts := readyBackend()
	defer ts.Close()
	m, fs := newTestManager(t, ts, time.Minute, 5*time.Second, time.Second)
	if _, err := m.EnsureModel("m1"); err != nil {
		t.Fatalf("EnsureModel: %v", err)
	}
	m.StopAll()
	if m.Current() != "" {
		t.Errorf("Current = %q, want empty after StopAll", m.Current())
	}
	if h := fs.last(); h == nil || !h.wasKilled() {
		t.Error("handle should be killed after StopAll")
	}
}

func TestCrashAutoRestart(t *testing.T) {
	ts := readyBackend()
	defer ts.Close()
	m, fs := newTestManager(t, ts, time.Minute, 5*time.Second, 100*time.Millisecond)

	if _, err := m.EnsureModel("m1"); err != nil {
		t.Fatalf("EnsureModel: %v", err)
	}
	if len(fs.handles) != 1 {
		t.Fatalf("handles = %d, want 1", len(fs.handles))
	}

	fs.last().crash()
	waitForCurrent(t, m, "")
	waitForCurrent(t, m, "m1")
	if len(fs.handles) != 2 {
		t.Errorf("handles = %d, want 2 after auto-restart", len(fs.handles))
	}
}

func TestCrashThenRequestNoDoubleStart(t *testing.T) {
	ts := readyBackend()
	defer ts.Close()
	m, fs := newTestManager(t, ts, time.Minute, 5*time.Second, 200*time.Millisecond)

	if _, err := m.EnsureModel("m1"); err != nil {
		t.Fatalf("EnsureModel: %v", err)
	}
	fs.last().crash()
	waitForCurrent(t, m, "")

	// A request during the restart window restarts immediately and the
	// pending timer must no-op afterwards.
	if _, err := m.EnsureModel("m1"); err != nil {
		t.Fatalf("EnsureModel after crash: %v", err)
	}
	if len(fs.handles) != 2 {
		t.Fatalf("handles = %d, want 2", len(fs.handles))
	}
	time.Sleep(400 * time.Millisecond)
	if len(fs.handles) != 2 {
		t.Errorf("timer restarted after manual start: handles = %d, want 2", len(fs.handles))
	}
	if m.Current() != "m1" {
		t.Errorf("Current = %q, want m1", m.Current())
	}
}

func TestCrashNoRestartAfterClose(t *testing.T) {
	ts := readyBackend()
	defer ts.Close()
	m, fs := newTestManager(t, ts, time.Minute, 5*time.Second, 200*time.Millisecond)

	if _, err := m.EnsureModel("m1"); err != nil {
		t.Fatalf("EnsureModel: %v", err)
	}
	fs.last().crash()
	waitForCurrent(t, m, "")

	m.Close()
	time.Sleep(400 * time.Millisecond)
	if len(fs.handles) != 1 {
		t.Errorf("handles = %d, want 1 (no restart after Close)", len(fs.handles))
	}
}

func TestCrashLoopCap(t *testing.T) {
	ts := readyBackend()
	defer ts.Close()
	m, fs := newTestManager(t, ts, time.Minute, 5*time.Second, 100*time.Millisecond)

	if _, err := m.EnsureModel("m1"); err != nil {
		t.Fatalf("EnsureModel: %v", err)
	}
	for i := 0; i < maxAutoRestarts; i++ {
		fs.last().crash()
		waitForCurrent(t, m, "")
		waitForCurrent(t, m, "m1")
	}
	if len(fs.handles) != maxAutoRestarts+1 {
		t.Fatalf("handles = %d, want %d", len(fs.handles), maxAutoRestarts+1)
	}

	// The next unserved crash exceeds the cap: no auto-restart.
	fs.last().crash()
	waitForCurrent(t, m, "")
	time.Sleep(300 * time.Millisecond)
	if m.Current() != "" {
		t.Fatalf("auto-restart not suppressed after %d consecutive unserved crashes", maxAutoRestarts+1)
	}

	// A manual request still starts the model and is never capped.
	if _, err := m.EnsureModel("m1"); err != nil {
		t.Fatalf("manual EnsureModel after cap: %v", err)
	}
	if len(fs.handles) != maxAutoRestarts+2 {
		t.Errorf("handles = %d, want %d", len(fs.handles), maxAutoRestarts+2)
	}
}

// TestExplicitStartResetsCrashLoop verifies that a user-initiated start
// clears the crash counter, so one model's unserved-crash streak cannot
// suppress auto-restarts of the next model (or of the same model after a
// manual restart).
func TestExplicitStartResetsCrashLoop(t *testing.T) {
	ts := readyBackend()
	defer ts.Close()
	m, fs := newTestManager(t, ts, time.Minute, 5*time.Second, 100*time.Millisecond)

	// m1 burns through the whole cap without serving a request.
	if _, err := m.EnsureModel("m1"); err != nil {
		t.Fatalf("EnsureModel(m1): %v", err)
	}
	for i := 0; i < maxAutoRestarts; i++ {
		fs.last().crash()
		waitForCurrent(t, m, "")
		waitForCurrent(t, m, "m1")
	}
	fs.last().crash()
	waitForCurrent(t, m, "")
	time.Sleep(300 * time.Millisecond)
	if m.Current() != "" {
		t.Fatal("m1 should stay down after exceeding the crash cap")
	}

	// A request for another model must not inherit m1's crash streak:
	// its first unserved crash still earns an auto-restart.
	if _, err := m.EnsureModel("m2"); err != nil {
		t.Fatalf("EnsureModel(m2): %v", err)
	}
	if len(fs.handles) != maxAutoRestarts+2 {
		t.Fatalf("handles = %d, want %d", len(fs.handles), maxAutoRestarts+2)
	}
	fs.last().crash()
	waitForCurrent(t, m, "")
	waitForCurrent(t, m, "m2")
	if len(fs.handles) != maxAutoRestarts+3 {
		t.Errorf("handles = %d, want %d", len(fs.handles), maxAutoRestarts+3)
	}
}

func TestServedCrashResetsLoop(t *testing.T) {
	ts := readyBackend()
	defer ts.Close()
	m, fs := newTestManager(t, ts, time.Minute, 5*time.Second, 100*time.Millisecond)

	if _, err := m.EnsureModel("m1"); err != nil {
		t.Fatalf("EnsureModel: %v", err)
	}
	for i := 0; i < maxAutoRestarts; i++ {
		fs.last().crash()
		waitForCurrent(t, m, "")
		waitForCurrent(t, m, "m1")
	}
	// Without the reset the next crash would be the (maxAutoRestarts+1)th
	// unserved one and auto-restart would be suppressed.
	m.MarkServed()
	fs.last().crash()
	waitForCurrent(t, m, "")
	waitForCurrent(t, m, "m1")
	if len(fs.handles) != maxAutoRestarts+2 {
		t.Errorf("handles = %d, want %d", len(fs.handles), maxAutoRestarts+2)
	}
}
