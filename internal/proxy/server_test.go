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
	"llama-swappy/internal/inspect"
	"llama-swappy/internal/model"
	"llama-swappy/internal/version"
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

// count returns the number of started handles under the lock.
func (s *fakeStarter) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.handles)
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

func newTestServer(t *testing.T, backend *httptest.Server, ttl time.Duration, infos map[string]inspect.Info) (*Server, *model.Manager, *fakeStarter) {
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
		TTL:          ttl,
		ReadyTimeout: 5 * time.Second,
		PollInterval: 20 * time.Millisecond,
	})
	t.Cleanup(mgr.Close)
	return New(cfg, mgr, log, infos), mgr, fs
}

func getJSON(t *testing.T, url string) (int, map[string]any) {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	defer resp.Body.Close()
	var body map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("GET %s: decode: %v", url, err)
	}
	return resp.StatusCode, body
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
	srv, mgr, fs := newTestServer(t, be, time.Minute, nil)
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
	if fs.count() != 1 {
		t.Errorf("handles = %d, want 1", fs.count())
	}
}

func TestProxyUnknownModel(t *testing.T) {
	be := backend(t)
	defer be.Close()
	srv, _, _ := newTestServer(t, be, time.Minute, nil)
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
	srv, _, _ := newTestServer(t, be, time.Minute, nil)
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
	srv, mgr, fs := newTestServer(t, be, time.Minute, nil)
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
	if fs.count() != 2 {
		t.Errorf("handles = %d, want 2 (one per model)", fs.count())
	}
}

// TestProxyServedRefreshesIdleTTL verifies that a successful proxied
// request refreshes the model's idle timer: more than TTL may have
// passed since the model started, but since the response completed less
// than TTL ago the model must stay loaded, and it is unloaded TTL after
// the response.
func TestProxyServedRefreshesIdleTTL(t *testing.T) {
	// Backend whose response takes a while, so the request start and
	// completion are far enough apart for the refresh to matter.
	be := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/health" {
			w.WriteHeader(http.StatusOK)
			return
		}
		time.Sleep(100 * time.Millisecond)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer be.Close()
	srv, mgr, _ := newTestServer(t, be, 150*time.Millisecond, nil)
	ts := httptest.NewServer(srv)
	defer ts.Close()

	resp := post(t, ts, `{"model":"m1"}`)
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		t.Fatalf("status = %d, body: %s", resp.StatusCode, body)
	}
	resp.Body.Close()

	// More than TTL has passed since the model started (~170ms), but
	// less than TTL since the response completed (~70ms): the model
	// must still be loaded.
	time.Sleep(70 * time.Millisecond)
	if mgr.Current() != "m1" {
		t.Fatal("model unloaded although its last response completed within the TTL")
	}

	// Then it must be unloaded TTL after the response.
	deadline := time.Now().Add(3 * time.Second)
	for mgr.Current() != "" && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if mgr.Current() != "" {
		t.Fatal("model still loaded long after the refreshed idle TTL")
	}
}

func TestInfoEndpointNoModelLoaded(t *testing.T) {
	be := backend(t)
	defer be.Close()
	infos := map[string]inspect.Info{
		"m1": {ContextSize: 131072, Reasoning: boolPtr(true)},
		// m2 has no entry: its script failed to parse at startup.
	}
	srv, _, _ := newTestServer(t, be, time.Minute, infos)
	ts := httptest.NewServer(srv)
	defer ts.Close()

	code, body := getJSON(t, ts.URL+InfoPath)
	if code != http.StatusOK {
		t.Fatalf("status = %d", code)
	}
	if body["version"] != version.Version {
		t.Errorf("version = %v, want %q", body["version"], version.Version)
	}
	if body["current"] != nil {
		t.Errorf("current = %v, want null", body["current"])
	}
	models, ok := body["models"].([]any)
	if !ok || len(models) != 2 {
		t.Fatalf("models = %v, want 2 entries", body["models"])
	}
	// Sorted by key: m1 first.
	m1 := models[0].(map[string]any)
	if m1["key"] != "m1" || m1["name"] != "Model One" {
		t.Errorf("m1 = %v", m1)
	}
	if m1["contextSize"] != float64(131072) {
		t.Errorf("m1.contextSize = %v, want 131072", m1["contextSize"])
	}
	if m1["maxTokens"] != float64(32768) { // min(32768, 131072/2)
		t.Errorf("m1.maxTokens = %v, want 32768", m1["maxTokens"])
	}
	if m1["reasoning"] != true {
		t.Errorf("m1.reasoning = %v, want true", m1["reasoning"])
	}
	if _, present := m1["input"]; !present {
		t.Error("m1.input missing")
	}
	m2 := models[1].(map[string]any)
	for _, field := range []string{"contextSize", "maxTokens", "reasoning"} {
		if _, present := m2[field]; present {
			t.Errorf("m2.%s = %v, want omitted (parse failed)", field, m2[field])
		}
	}
}

func TestInfoEndpointCurrentModel(t *testing.T) {
	be := backend(t)
	defer be.Close()
	srv, mgr, _ := newTestServer(t, be, time.Minute, nil)
	ts := httptest.NewServer(srv)
	defer ts.Close()

	if _, err := mgr.EnsureModel("m2"); err != nil {
		t.Fatalf("EnsureModel: %v", err)
	}
	code, body := getJSON(t, ts.URL+InfoPath)
	if code != http.StatusOK {
		t.Fatalf("status = %d", code)
	}
	cur, ok := body["current"].(map[string]any)
	if !ok {
		t.Fatalf("current = %v, want object", body["current"])
	}
	if cur["key"] != "m2" || cur["name"] != "Model Two" {
		t.Errorf("current = %v", cur)
	}
	secs, _ := cur["secondsToUnload"].(float64)
	if secs < 0 || secs > 60 {
		t.Errorf("secondsToUnload = %v, want in [0, 60]", secs)
	}

	// After unload the current model is null again.
	mgr.StopAll()
	_, body = getJSON(t, ts.URL+InfoPath)
	if body["current"] != nil {
		t.Errorf("current = %v, want null after StopAll", body["current"])
	}
}

func TestModelsEndpoint(t *testing.T) {
	be := backend(t)
	defer be.Close()
	srv, _, _ := newTestServer(t, be, time.Minute, nil)
	ts := httptest.NewServer(srv)
	defer ts.Close()

	code, body := getJSON(t, ts.URL+ModelsPath)
	if code != http.StatusOK {
		t.Fatalf("status = %d", code)
	}
	if body["object"] != "list" {
		t.Errorf("object = %v, want list", body["object"])
	}
	data, ok := body["data"].([]any)
	if !ok || len(data) != 2 {
		t.Fatalf("data = %v, want 2 entries", body["data"])
	}
	ids := []string{data[0].(map[string]any)["id"].(string), data[1].(map[string]any)["id"].(string)}
	if ids[0] != "m1" || ids[1] != "m2" {
		t.Errorf("ids = %v, want [m1 m2]", ids)
	}

	// Non-GET requests are rejected.
	for _, path := range []string{InfoPath, ModelsPath} {
		resp, err := http.Post(ts.URL+path, "application/json", bytes.NewReader([]byte(`{}`)))
		if err != nil {
			t.Fatalf("POST %s: %v", path, err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusMethodNotAllowed {
			t.Errorf("POST %s: status = %d, want 405", path, resp.StatusCode)
		}
	}
}

func boolPtr(b bool) *bool { return &b }
