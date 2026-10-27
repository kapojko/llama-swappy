package model

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"llama-swappy/internal/config"
)

// fakeHandle mimics a running process: Wait blocks until the process
// "exits" (Kill sends a clean exit, crash sends an unexpected one) and
// returns the cached exit error idempotently. Kill exits with exitErr
// if it is set, to simulate e.g. a real signal-terminated exit.
type fakeHandle struct {
	mu        sync.Mutex
	killed    bool
	exitErr   error
	done      chan error
	finished  chan struct{}
	closeOnce sync.Once
	res       error
	reaped    bool
}

// Kill signals a clean exit first (without the lock, since Wait may be
// blocked on the channel while holding it), then records the kill.
func (h *fakeHandle) Kill() error {
	h.signalExit(h.exitErr)
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
	h.closeOnce.Do(func() { close(h.finished) })
}

// Done reports the exit, mirroring procHandle.Done.
func (h *fakeHandle) Done() <-chan struct{} { return h.finished }

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
	starts  []time.Time
	// failAll makes every started handle exit immediately, simulating
	// processes that die during model load.
	failAll bool
}

func (s *fakeStarter) Start(cmd string, _ io.Writer) (Handle, error) {
	h := &fakeHandle{done: make(chan error, 1), finished: make(chan struct{})}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.failAll {
		h.signalExit(fmt.Errorf("exit status 1"))
	}
	s.handles = append(s.handles, h)
	s.cmds = append(s.cmds, cmd)
	s.starts = append(s.starts, time.Now())
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

// count returns the number of started handles under the lock.
func (s *fakeStarter) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.handles)
}

func quietLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// recordLogger captures every record it receives, so tests can assert
// on log output (e.g. that an expected exit is not logged as a warning).
type recordLogger struct {
	mu  sync.Mutex
	rec []slog.Record
}

func (r *recordLogger) Enabled(_ context.Context, _ slog.Level) bool { return true }

func (r *recordLogger) Handle(_ context.Context, rec slog.Record) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.rec = append(r.rec, rec.Clone())
	return nil
}

func (r *recordLogger) WithAttrs([]slog.Attr) slog.Handler { return r }

func (r *recordLogger) WithGroup(string) slog.Handler { return r }

func (r *recordLogger) warns() []slog.Record {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []slog.Record
	for _, rec := range r.rec {
		if rec.Level == slog.LevelWarn {
			out = append(out, rec)
		}
	}
	return out
}

// dump renders the captured records as text, for failure messages.
func (r *recordLogger) dump() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	var b strings.Builder
	for _, rec := range r.rec {
		_ = slog.NewTextHandler(&b, nil).Handle(context.Background(), rec)
	}
	return b.String()
}

// realExitError runs a process that terminates abnormally (non-zero
// exit code on Windows, SIGTERM on Unix) and returns the genuine
// *exec.ExitError such an exit produces.
func realExitError(t *testing.T) error {
	t.Helper()
	var c *exec.Cmd
	if runtime.GOOS == "windows" {
		c = exec.Command("cmd", "/c", "exit 3")
	} else {
		c = exec.Command("sh", "-c", "kill -TERM $$")
	}
	err := c.Run()
	var ee *exec.ExitError
	if !errors.As(err, &ee) {
		t.Fatalf("expected *exec.ExitError, got %T (%v)", err, err)
	}
	return err
}

// newTestManager wires a manager whose single model port is the port of
// ts, an httptest server standing in for the real model.
func newTestManager(t *testing.T, ts *httptest.Server, ttl time.Duration, readyTimeout time.Duration, restartDelay time.Duration) (*Manager, *fakeStarter) {
	t.Helper()
	return newTestManagerWithLogger(t, ts, quietLogger(), ttl, readyTimeout, restartDelay)
}

// newTestManagerWithLogger is newTestManager with a custom logger.
func newTestManagerWithLogger(t *testing.T, ts *httptest.Server, log *slog.Logger, ttl time.Duration, readyTimeout time.Duration, restartDelay time.Duration) (*Manager, *fakeStarter) {
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
		Logger:       log,
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
	before := fs.count()
	if _, err := m.EnsureModel("m1"); err != nil {
		t.Fatalf("second EnsureModel: %v", err)
	}
	if fs.count() != before {
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
		t.Fatalf("model still loaded after idle TTL; handles: %d", fs.count())
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
	if fs.count() != 1 {
		t.Fatalf("handles = %d, want 1", fs.count())
	}

	fs.last().crash()
	waitForCurrent(t, m, "")
	waitForCurrent(t, m, "m1")
	if fs.count() != 2 {
		t.Errorf("handles = %d, want 2 after auto-restart", fs.count())
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
	if fs.count() != 2 {
		t.Fatalf("handles = %d, want 2", fs.count())
	}
	time.Sleep(400 * time.Millisecond)
	if fs.count() != 2 {
		t.Errorf("timer restarted after manual start: handles = %d, want 2", fs.count())
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
	if fs.count() != 1 {
		t.Errorf("handles = %d, want 1 (no restart after Close)", fs.count())
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
	if fs.count() != maxAutoRestarts+1 {
		t.Fatalf("handles = %d, want %d", fs.count(), maxAutoRestarts+1)
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
	if fs.count() != maxAutoRestarts+2 {
		t.Errorf("handles = %d, want %d", fs.count(), maxAutoRestarts+2)
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
	if fs.count() != maxAutoRestarts+2 {
		t.Fatalf("handles = %d, want %d", fs.count(), maxAutoRestarts+2)
	}
	fs.last().crash()
	waitForCurrent(t, m, "")
	waitForCurrent(t, m, "m2")
	if fs.count() != maxAutoRestarts+3 {
		t.Errorf("handles = %d, want %d", fs.count(), maxAutoRestarts+3)
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
	m.MarkServed("m1")
	fs.last().crash()
	waitForCurrent(t, m, "")
	waitForCurrent(t, m, "m1")
	if fs.count() != maxAutoRestarts+2 {
		t.Errorf("handles = %d, want %d", fs.count(), maxAutoRestarts+2)
	}
}

// failingBackend serves /health with 500 until healthy is set, standing
// in for a model server that never becomes ready.
func failingBackend(healthy *atomic.Bool) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if !healthy.Load() {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
}

// TestFailedStartFailsFastAndAutoRetries verifies that a model whose
// process exits during the readiness wait (e.g. a llama-server that fails
// to load) returns the error immediately instead of burning the readiness
// timeout, and is auto-retried after the restart delay.
func TestFailedStartFailsFastAndAutoRetries(t *testing.T) {
	var healthy atomic.Bool
	ts := failingBackend(&healthy)
	defer ts.Close()
	m, fs := newTestManager(t, ts, time.Minute, 30*time.Second, 200*time.Millisecond)

	fs.failAll = true
	start := time.Now()
	if _, err := m.EnsureModel("m1"); err == nil {
		t.Fatal("expected failed start error")
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("EnsureModel returned after %s; an exit during readiness must fail fast", elapsed)
	}
	if m.Current() != "" {
		t.Errorf("Current = %q, want empty after failed start", m.Current())
	}
	if fs.count() != 1 {
		t.Fatalf("handles = %d, want 1", fs.count())
	}

	// The auto-retry must bring the model up once the transient cause
	// (here: the failing backend) is gone — without a client request.
	fs.failAll = false
	healthy.Store(true)
	waitForCurrent(t, m, "m1")
	if fs.count() != 2 {
		t.Errorf("handles = %d, want 2 after auto-retry", fs.count())
	}
}

// TestFailedStartBackoffGrows verifies that the retry delay doubles with
// each consecutive failure: with restartDelay d the second attempt fires
// after ~d, the third after ~d+2d, not after d+d.
func TestFailedStartBackoffGrows(t *testing.T) {
	var healthy atomic.Bool
	ts := failingBackend(&healthy)
	defer ts.Close()
	m, fs := newTestManager(t, ts, time.Minute, 5*time.Second, 100*time.Millisecond)
	fs.failAll = true

	if _, err := m.EnsureModel("m1"); err == nil {
		t.Fatal("expected failed start error")
	}
	deadline := time.Now().Add(3 * time.Second)
	for fs.count() < 3 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if fs.count() < 3 {
		t.Fatalf("handles = %d, want at least 3 attempts", fs.count())
	}
	fs.mu.Lock()
	d1 := fs.starts[1].Sub(fs.starts[0])
	d2 := fs.starts[2].Sub(fs.starts[1])
	fs.mu.Unlock()
	if d1 > 150*time.Millisecond {
		t.Errorf("first retry delay = %s, want ~100ms", d1)
	}
	if d2 < 150*time.Millisecond {
		t.Errorf("second retry delay = %s, want ~200ms (backoff must double)", d2)
	}
}

// TestFailedStartCapSuppressesRetry verifies that failed starts stop
// being retried once the consecutive-failure cap is exhausted.
func TestFailedStartCapSuppressesRetry(t *testing.T) {
	var healthy atomic.Bool
	ts := failingBackend(&healthy)
	defer ts.Close()
	m, fs := newTestManager(t, ts, time.Minute, 5*time.Second, 50*time.Millisecond)
	fs.failAll = true
	if _, err := m.EnsureModel("m1"); err == nil {
		t.Fatal("expected failed start error")
	}

	// Initial attempt + maxAutoRestarts retries. With backoff the last
	// (5th) retry fires 50*(1+2+4+8+16) = 1550ms after the first start.
	deadline := time.Now().Add(5 * time.Second)
	for fs.count() < maxAutoRestarts+1 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if fs.count() != maxAutoRestarts+1 {
		t.Fatalf("handles = %d, want %d", fs.count(), maxAutoRestarts+1)
	}
	// A 6th retry would fire 1600ms after the 5th failure; make sure it
	// never comes.
	time.Sleep(1800 * time.Millisecond)
	if fs.count() != maxAutoRestarts+1 {
		t.Errorf("handles = %d; retries must stop after %d consecutive failures", fs.count(), maxAutoRestarts)
	}
	if m.Current() != "" {
		t.Errorf("Current = %q, want empty while retries are suppressed", m.Current())
	}
}

// TestStopSignalExitNoWarn verifies that a graceful stop whose process
// exits with a termination status (signal, non-zero code) is not logged
// as a "wait model failed" warning: that exit is the expected outcome
// of the stop.
func TestStopSignalExitNoWarn(t *testing.T) {
	ts := readyBackend()
	defer ts.Close()
	rec := &recordLogger{}
	m, fs := newTestManagerWithLogger(t, ts, slog.New(rec), time.Minute, 5*time.Second, time.Second)

	if _, err := m.EnsureModel("m1"); err != nil {
		t.Fatalf("EnsureModel: %v", err)
	}
	fs.last().exitErr = realExitError(t)

	m.StopAll()

	if m.Current() != "" {
		t.Errorf("Current = %q, want empty after StopAll", m.Current())
	}
	if warns := rec.warns(); len(warns) != 0 {
		t.Errorf("stop logged %d warning(s), want 0; log: %s", len(warns), rec.dump())
	}
}

// TestStopNonExitErrorWarns verifies that a wait error that is not a
// process exit (e.g. a pipe that outlives the process) is still
// reported as a warning.
func TestStopNonExitErrorWarns(t *testing.T) {
	ts := readyBackend()
	defer ts.Close()
	rec := &recordLogger{}
	m, fs := newTestManagerWithLogger(t, ts, slog.New(rec), time.Minute, 5*time.Second, time.Second)

	if _, err := m.EnsureModel("m1"); err != nil {
		t.Fatalf("EnsureModel: %v", err)
	}
	fs.last().exitErr = errors.New("stdout pipe stuck")

	m.StopAll()

	if len(rec.warns()) == 0 {
		t.Errorf("expected a warning for a non-exit wait error; log: %s", rec.dump())
	}
}

// TestMarkServedRefreshesIdleTTL verifies that the idle timer is counted
// from the last completed response: a model started long ago but which
// just served a request is not unloaded, and is unloaded TTL after the
// response.
func TestMarkServedRefreshesIdleTTL(t *testing.T) {
	ts := readyBackend()
	defer ts.Close()
	m, _ := newTestManager(t, ts, 150*time.Millisecond, 5*time.Second, time.Second)

	if _, err := m.EnsureModel("m1"); err != nil {
		t.Fatalf("EnsureModel: %v", err)
	}
	time.Sleep(100 * time.Millisecond)
	m.MarkServed("m1")

	// More than TTL has passed since the start, but less than TTL since
	// the served response: the model must still be loaded.
	time.Sleep(100 * time.Millisecond)
	if m.Current() != "m1" {
		t.Fatal("model unloaded although a response completed within the TTL")
	}

	// Then it must be unloaded TTL after the refresh.
	deadline := time.Now().Add(3 * time.Second)
	for m.Current() != "" && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if m.Current() != "" {
		t.Fatal("model still loaded long after the refreshed idle TTL")
	}
}

// TestMarkServedOtherModel is a no-op: a 2xx response that completes
// after its model has been swapped out must not refresh the idle timer
// of the model that replaced it.
func TestMarkServedOtherModel(t *testing.T) {
	ts := readyBackend()
	defer ts.Close()
	m, _ := newTestManager(t, ts, 150*time.Millisecond, 5*time.Second, time.Second)

	if _, err := m.EnsureModel("m1"); err != nil {
		t.Fatalf("EnsureModel: %v", err)
	}
	time.Sleep(100 * time.Millisecond)
	m.MarkServed("m2")

	// The timer was not refreshed: the model is unloaded by the
	// original TTL since the start, i.e. within ~70ms more. If the
	// timer had been (wrongly) refreshed it would survive ~150ms more,
	// so a tight deadline distinguishes the two.
	deadline := time.Now().Add(110 * time.Millisecond)
	for m.Current() != "" && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if m.Current() != "" {
		t.Fatal("MarkServed for another model must not refresh the idle TTL")
	}
}


