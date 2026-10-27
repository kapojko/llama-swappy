//go:build unix

package model

import (
	"bytes"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"llama-swappy/internal/config"
	"llama-swappy/internal/testutil"
)

// TestFailedLoadAutoRetryIntegration exercises the fail-fast + auto-retry
// path with the real ProcessStarter: the first attempt exits with an
// error like a llama-server that failed to load (e.g. OOM while the GPU
// driver still holds the previous model's memory). The manager must
// return quickly instead of burning the readiness timeout, and the
// scheduled retry must bring the model up without a client request.
func TestFailedLoadAutoRetryIntegration(t *testing.T) {
	bin := testutil.BuildFakeServer(t)
	port := testutil.FreePort(t)
	marker := filepath.Join(t.TempDir(), "next-attempt-loads")
	script := filepath.Join(t.TempDir(), "model.sh")
	body := fmt.Sprintf(`#!/bin/sh
if [ -f %q ]; then
	exec %q -port %s
fi
touch %q
echo "simulated: exiting due to model loading error" >&2
exit 1
`, marker, bin, port, marker)
	if err := os.WriteFile(script, []byte(body), 0o700); err != nil {
		t.Fatalf("write wrapper script: %v", err)
	}
	cfg := &config.Config{
		GlobalTTL: 900,
		StartPort: mustAtoi(t, port),
		Models: map[string]config.Model{
			"m1": {
				Name:  "M1",
				Cmd:   fmt.Sprintf("/bin/sh %q", script),
				Proxy: "http://127.0.0.1:${PORT}",
			},
		},
	}
	var out bytes.Buffer
	m := New(cfg, Options{
		Logger:       quietLogger(),
		Out:          &out,
		Starter:      ProcessStarter{},
		TTL:          time.Hour,
		ReadyTimeout: 30 * time.Second,
		PollInterval: 100 * time.Millisecond,
		RestartDelay: 500 * time.Millisecond,
	})
	defer m.Close()

	start := time.Now()
	if _, err := m.EnsureModel("m1"); err == nil {
		t.Fatal("expected the first (failing) start to return an error")
	}
	if elapsed := time.Since(start); elapsed > 10*time.Second {
		t.Fatalf("first start returned after %s; a crashed load must fail fast, not burn the readiness timeout", elapsed)
	}

	// The scheduled retry (after 500ms) runs the real server; /health
	// must come up without any further client request.
	client := &http.Client{Timeout: 2 * time.Second}
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		resp, err := client.Get("http://127.0.0.1:" + port + "/health")
		if err == nil {
			_ = resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("model not up after auto-retry; log: %s", out.String())
}
