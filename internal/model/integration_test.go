package model

import (
	"bytes"
	"fmt"
	"net"
	"strconv"
	"strings"
	"testing"
	"time"

	"llama-swappy/internal/config"
	"llama-swappy/internal/testutil"
)

// TestRealProcessLifecycle exercises the real ProcessStarter end to end:
// spawning a real process, capturing its stdout, idle-unloading it,
// verifying the port is released, and restarting it.
func TestRealProcessLifecycle(t *testing.T) {
	bin := testutil.BuildFakeServer(t)
	port := testutil.FreePort(t)
	cfg := &config.Config{
		GlobalTTL: 900,
		StartPort: mustAtoi(t, port),
		Models: map[string]config.Model{
			"m1": {
				Name:  "M1",
				Cmd:   fmt.Sprintf(`"%s" -port %s`, bin, port),
				Proxy: "http://127.0.0.1:${PORT}",
			},
		},
	}
	var out bytes.Buffer
	m := New(cfg, Options{
		Logger:       quietLogger(),
		Out:          &out,
		Starter:      ProcessStarter{},
		TTL:          1200 * time.Millisecond,
		ReadyTimeout: 30 * time.Second,
		PollInterval: 100 * time.Millisecond,
	})
	defer m.Close()

	u, err := m.EnsureModel("m1")
	if err != nil {
		t.Fatalf("EnsureModel: %v", err)
	}
	if want := "http://127.0.0.1:" + port; u != want {
		t.Fatalf("proxy URL = %q, want %q", u, want)
	}
	if !strings.Contains(out.String(), "FAKE-READY") {
		t.Errorf("captured stdout %q does not contain FAKE-READY", out.String())
	}

	deadline := time.Now().Add(5 * time.Second)
	for m.Current() != "" && time.Now().Before(deadline) {
		time.Sleep(100 * time.Millisecond)
	}
	if m.Current() != "" {
		t.Fatal("model not unloaded after idle TTL")
	}

	conn, err := net.DialTimeout("tcp", "127.0.0.1:"+port, 500*time.Millisecond)
	if err == nil {
		_ = conn.Close()
		t.Fatal("model port still in use after unload")
	}

	if _, err := m.EnsureModel("m1"); err != nil {
		t.Fatalf("restart after idle unload: %v", err)
	}
}

func mustAtoi(t *testing.T, s string) int {
	t.Helper()
	n, err := strconv.Atoi(s)
	if err != nil {
		t.Fatalf("atoi %q: %v", s, err)
	}
	return n
}
