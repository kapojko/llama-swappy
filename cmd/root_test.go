package cmd

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"llama-swappy/internal/testutil"
)

// TestRunFullPipeline runs the full application wiring (run): real config
// file, real model process, real HTTP listener.
func TestRunFullPipeline(t *testing.T) {
	bin := testutil.BuildFakeServer(t)
	modelPort := testutil.FreePort(t)
	listenPort := testutil.FreePort(t)
	listen := "127.0.0.1:" + listenPort

	cfgPath := filepath.Join(t.TempDir(), "config.yaml")
	// |- block scalar: keeps backslashes in Windows paths literal and
	// strips the trailing newline.
	cfg := fmt.Sprintf("globalTTL: 900\nstartPort: %s\nmodels:\n  m1:\n    name: M1\n    cmd: |-\n      %s -port %s\n    proxy: http://127.0.0.1:${PORT}\n",
		modelPort, bin, modelPort)
	if err := os.WriteFile(cfgPath, []byte(cfg), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- run(ctx, cfgPath, listen) }()

	waitFor := func(f func() bool, timeout time.Duration) {
		t.Helper()
		deadline := time.Now().Add(timeout)
		for time.Now().Before(deadline) {
			if f() {
				return
			}
			time.Sleep(200 * time.Millisecond)
		}
		t.Fatalf("timed out waiting for listener")
	}
	waitFor(func() bool {
		resp, err := http.Post("http://"+listen+"/v1/chat/completions", "application/json", bytes.NewReader([]byte(`{"model":"unknown"}`)))
		if err != nil {
			return false
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
		return resp.StatusCode == http.StatusNotFound
	}, 15*time.Second)

	req := `{"model":"m1","echo":42}`
	resp, err := http.Post("http://"+listen+"/v1/chat/completions", "application/json", bytes.NewReader([]byte(req)))
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, body: %s", resp.StatusCode, body)
	}
	if string(body) != req {
		t.Errorf("proxied body = %q, want %q", body, req)
	}

	cancel()
	if err := <-done; err != nil {
		t.Fatalf("run: %v", err)
	}
}

func TestRequiredConfigFlag(t *testing.T) {
	oldArgs := os.Args
	defer func() { os.Args = oldArgs }()
	os.Args = []string{"llama-swappy"}
	var buf bytes.Buffer
	rootCmd.SetOut(&buf)
	rootCmd.SetErr(&buf)
	err := rootCmd.Execute()
	if err == nil || !strings.Contains(err.Error(), "config") {
		t.Fatalf("expected missing --config error, got %v", err)
	}
}
