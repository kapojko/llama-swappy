package model

import (
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

type fakeHandle struct {
	mu     sync.Mutex
	killed bool
}

func (h *fakeHandle) Kill() error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.killed = true
	return nil
}

func (h *fakeHandle) Wait() error { return nil }

func (h *fakeHandle) wasKilled() bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.killed
}

type fakeStarter struct {
	mu      sync.Mutex
	handles []*fakeHandle
}

func (s *fakeStarter) Start(_ string, _ io.Writer) (Handle, error) {
	h := &fakeHandle{}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.handles = append(s.handles, h)
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
func newTestManager(t *testing.T, ts *httptest.Server, ttl time.Duration, readyTimeout time.Duration) (*Manager, *fakeStarter) {
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
			"m1": {Name: "Model One", Cmd: "cmd1", Proxy: "http://127.0.0.1:${PORT}"},
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
	})
	t.Cleanup(m.Close)
	return m, fs
}

func readyBackend() *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
}

func TestEnsureModelStarts(t *testing.T) {
	ts := readyBackend()
	defer ts.Close()
	m, fs := newTestManager(t, ts, time.Minute, 5*time.Second)

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

func TestEnsureModelUnknownKey(t *testing.T) {
	ts := readyBackend()
	defer ts.Close()
	m, _ := newTestManager(t, ts, time.Minute, 5*time.Second)
	if _, err := m.EnsureModel("nope"); err == nil {
		t.Fatal("expected error for unknown model")
	}
}

func TestSwapOutPrevious(t *testing.T) {
	ts := readyBackend()
	defer ts.Close()
	m, fs := newTestManager(t, ts, time.Minute, 5*time.Second)

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
	m, fs := newTestManager(t, ts, 100*time.Millisecond, 5*time.Second)

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
	m, fs := newTestManager(t, ts, time.Minute, 100*time.Millisecond)

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
	m, fs := newTestManager(t, ts, time.Minute, 5*time.Second)
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
