//go:build unix

package model

import (
	"bytes"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"llama-swappy/internal/config"
	"llama-swappy/internal/testutil"
)

// TestGrandchildProcessUnload verifies that unloading stops a whole
// process tree: the model cmd is a shell script that spawns the fake
// server as a background grandchild without exec. After the idle TTL
// both the script and the server must be dead, so the port is released.
func TestGrandchildProcessUnload(t *testing.T) {
	bin := testutil.BuildFakeServer(t)
	port := testutil.FreePort(t)

	script := filepath.Join(t.TempDir(), "wrapper.sh")
	body := fmt.Sprintf("#!/bin/sh\n\"%s\" -port %s &\nwait\n", bin, port)
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
		TTL:          1200 * time.Millisecond,
		ReadyTimeout: 30 * time.Second,
		PollInterval: 100 * time.Millisecond,
	})
	defer m.Close()

	if _, err := m.EnsureModel("m1"); err != nil {
		t.Fatalf("EnsureModel: %v", err)
	}

	deadline := time.Now().Add(20 * time.Second)
	for m.Current() != "" && time.Now().Before(deadline) {
		time.Sleep(100 * time.Millisecond)
	}
	if m.Current() != "" {
		t.Fatal("model not unloaded after idle TTL")
	}

	conn, err := net.DialTimeout("tcp", "127.0.0.1:"+port, 500*time.Millisecond)
	if err == nil {
		_ = conn.Close()
		t.Fatal("grandchild server still listening after unload")
	}
}
