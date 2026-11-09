//go:build windows

package model

import (
	"bytes"
	"flag"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"syscall"
	"testing"
	"time"

	"llama-swappy/internal/config"
	"llama-swappy/internal/testutil"
)

// The test binary re-runs itself as a process-tree helper: "wrapper"
// spawns a "sleep" grandchild (like the Strata server re-exec'ing
// python, which then spawns the engine), reports the grandchild's PID
// and waits; "sleep" just sleeps until it is killed.

var (
	treeHelper = flag.String("llama-swappy-tree-helper", "", "internal: process-tree helper mode (sleep|wrapper)")
	treeFile   = flag.String("llama-swappy-tree-file", "", "internal: file the wrapper helper writes the grandchild PID to")
)

func TestMain(m *testing.M) {
	flag.Parse()
	switch *treeHelper {
	case "sleep":
		time.Sleep(2 * time.Minute)
		return
	case "wrapper":
		exe, err := os.Executable()
		if err != nil {
			os.Exit(3)
		}
		child := exec.Command(exe, "-llama-swappy-tree-helper=sleep")
		child.Stdout = os.Stdout
		child.Stderr = os.Stderr
		if err := child.Start(); err != nil {
			os.Exit(3)
		}
		_ = os.WriteFile(*treeFile, []byte(strconv.Itoa(child.Process.Pid)), 0o644)
		_ = child.Wait()
		return
	}
	os.Exit(m.Run())
}

const (
	_PROCESS_QUERY_LIMITED_INFORMATION = 0x00001000
	_SYNCHRONIZE                       = 0x00100000
	_WAIT_TIMEOUT                      = 0x00000102
)

var procWaitForSingleObject = kernel32.NewProc("WaitForSingleObject")

// processAlive reports whether the process is still running (a
// terminated process whose PID has not been reused reports dead).
func processAlive(pid int) bool {
	ph, _, _ := procOpenProcess.Call(_SYNCHRONIZE|_PROCESS_QUERY_LIMITED_INFORMATION, 0, uintptr(pid))
	if ph == 0 {
		return false
	}
	defer syscall.CloseHandle(syscall.Handle(ph))
	res, _, _ := procWaitForSingleObject.Call(ph, 0)
	return res == _WAIT_TIMEOUT
}

// TestJobObjectKillsWholeTree verifies the Windows unload mechanism
// directly: a wrapper process with its own grandchild is killed via the
// job object, and the grandchild dies with it — the case the old
// direct-child TerminateProcess leaked.
func TestJobObjectKillsWholeTree(t *testing.T) {
	exe, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable: %v", err)
	}
	pidFile := filepath.Join(t.TempDir(), "grandchild.pid")
	cmd := fmt.Sprintf("%q -llama-swappy-tree-helper=wrapper -llama-swappy-tree-file=%q", exe, pidFile)

	start := time.Now()
	h, err := (ProcessStarter{}).Start(cmd, io.Discard)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}

	var grandchild int
	deadline := time.Now().Add(30 * time.Second)
	for grandchild == 0 && time.Now().Before(deadline) {
		if b, err := os.ReadFile(pidFile); err == nil {
			grandchild, _ = strconv.Atoi(string(bytes.TrimSpace(b)))
		}
		time.Sleep(50 * time.Millisecond)
	}
	if grandchild == 0 {
		t.Fatal("wrapper helper did not report its grandchild")
	}
	if !processAlive(grandchild) {
		t.Fatal("grandchild not alive before Kill")
	}

	if err := h.Kill(); err != nil {
		t.Fatalf("Kill: %v", err)
	}
	if elapsed := time.Since(start); elapsed > 30*time.Second {
		t.Fatalf("Kill took %v, expected a prompt job termination", elapsed)
	}

	deadline = time.Now().Add(10 * time.Second)
	for processAlive(grandchild) && time.Now().Before(deadline) {
		time.Sleep(100 * time.Millisecond)
	}
	if processAlive(grandchild) {
		t.Fatalf("grandchild (pid %d) survived Kill: the job object did not cover the tree", grandchild)
	}
}

// TestGrandchildProcessUnload is the Windows mirror of the Unix
// end-to-end test: the model cmd is cmd.exe running the fake server as
// a grandchild. After the idle TTL both must be dead, so the port is
// released — with the old direct-child-only kill the server survived
// and kept the port and its VRAM.
func TestGrandchildProcessUnload(t *testing.T) {
	bin := testutil.BuildFakeServer(t)
	port := testutil.FreePort(t)

	cfg := &config.Config{
		GlobalTTL: 900,
		StartPort: mustAtoi(t, port),
		Models: map[string]config.Model{
			"m1": {
				Name:  "M1",
				Cmd:   fmt.Sprintf("cmd /c %s -port %s", bin, port),
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
