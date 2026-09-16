package proxy

import (
	"bytes"
	"encoding/json"
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
	"llama-swappy/internal/model"
)

// fakeHandle mimics a running process: it only "exits" when Kill is
// called, and Wait returns the cached exit error idempotently.
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
	select {
	case h.done <- nil:
	default:
	}
	h.mu.Lock()
	h.killed = true
	h.mu.Unlock()
	return nil
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

type fakeStarter struct {
	mu      sync.Mutex
	handles []*fakeHandle
}

func (s *fakeStarter) Start(_ string, _ io.Writer) (model.Handle, error) {
	h := &fakeHandle{done: make(chan error, 1)}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.handles = append(s.handles, h)
	return h, nil
}

// backend mimics a llama-server: /health returns 200, other paths echo
// the request body.
func backend(t *testing.T) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/health" {
			w.WriteHeader(http.StatusOK)
			return
		}
		body, _ := io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(body)
	}))
}

func newTestServer(t *testing.T, backend *httptest.Server) (*Server, *model.Manager, *fakeStarter) {
	t.Helper()
	u, err := url.Parse(backend.URL)
	if err != nil {
		t.Fatalf("parse backend url: %v", err)
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
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	mgr := model.New(cfg, model.Options{
		Logger:       log,
		Out:          io.Discard,
		Starter:      fs,
		TTL:          time.Minute,
		ReadyTimeout: 5 * time.Second,
		PollInterval: 20 * time.Millisecond,
	})
	t.Cleanup(mgr.Close)
	return New(cfg, mgr, log), mgr, fs
}

func post(t *testing.T, srv *httptest.Server, payload string) *http.Response {
	t.Helper()
	resp, err := http.Post(srv.URL+"/v1/chat/completions", "application/json", bytes.NewReader([]byte(payload)))
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	return resp
}

func TestProxyRoundsTrip(t *testing.T) {
	be := backend(t)
	defer be.Close()
	srv, mgr, fs := newTestServer(t, be)
	ts := httptest.NewServer(srv)
	defer ts.Close()

	req := `{"model":"m1","messages":[{"role":"user","content":"hi"}]}`
	resp := post(t, ts, req)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, body: %s", resp.StatusCode, body)
	}
	body, _ := io.ReadAll(resp.Body)
	if string(body) != req {
		t.Errorf("proxied body = %q, want the request body echoed", body)
	}
	if mgr.Current() != "m1" {
		t.Errorf("Current = %q, want m1", mgr.Current())
	}
	if len(fs.handles) != 1 {
		t.Errorf("handles = %d, want 1", len(fs.handles))
	}
}

func TestProxyUnknownModel(t *testing.T) {
	be := backend(t)
	defer be.Close()
	srv, _, _ := newTestServer(t, be)
	ts := httptest.NewServer(srv)
	defer ts.Close()

	resp := post(t, ts, `{"model":"nope"}`)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, body: %s", resp.StatusCode, body)
	}
	var e apiError
	if err := json.NewDecoder(resp.Body).Decode(&e); err != nil {
		t.Fatalf("decode error body: %v", err)
	}
	if e.Error.Message == "" {
		t.Error("expected error message in body")
	}
}

func TestProxyMissingModel(t *testing.T) {
	be := backend(t)
	defer be.Close()
	srv, _, _ := newTestServer(t, be)
	ts := httptest.NewServer(srv)
	defer ts.Close()

	for _, payload := range []string{`{}`, `not json`} {
		resp := post(t, ts, payload)
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("payload %q: status = %d, want 400 (body: %s)", payload, resp.StatusCode, body)
		}
	}
}

func TestProxySwap(t *testing.T) {
	be := backend(t)
	defer be.Close()
	srv, mgr, fs := newTestServer(t, be)
	ts := httptest.NewServer(srv)
	defer ts.Close()

	resp1 := post(t, ts, `{"model":"m1"}`)
	if resp1.StatusCode != http.StatusOK {
		t.Fatalf("m1: status = %d", resp1.StatusCode)
	}
	resp1.Body.Close()
	resp := post(t, ts, `{"model":"m2"}`)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("m2: status = %d", resp.StatusCode)
	}
	if mgr.Current() != "m2" {
		t.Errorf("Current = %q, want m2", mgr.Current())
	}
	if len(fs.handles) != 2 {
		t.Errorf("handles = %d, want 2 (one per model)", len(fs.handles))
	}
}
