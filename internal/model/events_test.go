package model

import (
	"io"
	"log/slog"
	"net/url"
	"strconv"
	"sync"
	"testing"
	"time"

	"llama-swappy/internal/config"
)

// TestOnEventLifecycle verifies the Options.OnEvent callback wiring
// used by the hardware monitor: a load emits loaded, a swap emits
// unloaded+loaded, and a stop emits unloaded.
func TestOnEventLifecycle(t *testing.T) {
	ts := readyBackend()
	defer ts.Close()
	u, err := url.Parse(ts.URL)
	if err != nil {
		t.Fatal(err)
	}
	port, _ := strconv.Atoi(u.Port())
	cfg := &config.Config{
		GlobalTTL: 900,
		StartPort: port,
		Models: map[string]config.Model{
			"m1": {Name: "Model One", Cmd: "cmd1", Proxy: "http://127.0.0.1:${PORT}"},
			"m2": {Name: "Model Two", Cmd: "cmd2", Proxy: "http://127.0.0.1:${PORT}"},
		},
	}

	var mu sync.Mutex
	var events []string
	record := func(action, key string) {
		mu.Lock()
		events = append(events, action+" "+key)
		mu.Unlock()
	}

	m := New(cfg, Options{
		Logger:       slog.New(slog.NewTextHandler(io.Discard, nil)),
		Out:          io.Discard,
		Starter:      &fakeStarter{},
		TTL:          time.Minute,
		ReadyTimeout: 5 * time.Second,
		PollInterval: 20 * time.Millisecond,
		OnEvent:      record,
	})
	t.Cleanup(m.Close)

	if _, err := m.EnsureModel("m1"); err != nil {
		t.Fatal(err)
	}
	waitForCurrent(t, m, "m1")
	if _, err := m.EnsureModel("m2"); err != nil {
		t.Fatal(err)
	}
	waitForCurrent(t, m, "m2")
	m.StopAll()

	want := []string{
		EventLoaded + " m1",
		EventUnloaded + " m1",
		EventLoaded + " m2",
		EventUnloaded + " m2",
	}
	mu.Lock()
	defer mu.Unlock()
	if len(events) != len(want) {
		t.Fatalf("events = %v, want %v", events, want)
	}
	for i := range want {
		if events[i] != want[i] {
			t.Errorf("events[%d] = %q, want %q", i, events[i], want[i])
		}
	}
}
