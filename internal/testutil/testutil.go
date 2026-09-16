// Package testutil provides shared helpers for the integration tests:
// building a fake "llama-server" binary and finding a free port.
package testutil

import (
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"testing"
)

// fakeserverSrc is a minimal HTTP server that stands in for llama-server:
// it listens on the given port, prints FAKE-READY when listening, serves
// /health with 200, and echoes the request body on any other path. With
// -crash-after <duration> it exits with status 1 after that duration.
const fakeserverSrc = `package main

import (
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"time"
)

func main() {
	port := flag.String("port", "", "listen port")
	crashAfter := flag.Duration("crash-after", 0, "exit with status 1 after this duration")
	flag.Parse()
	ln, err := net.Listen("tcp", "127.0.0.1:" + *port)
	if err != nil {
		fmt.Fprintln(os.Stderr, "listen:", err)
		os.Exit(1)
	}
	fmt.Println("FAKE-READY")
	_ = os.Stdout.Sync()
	if *crashAfter > 0 {
		go func() {
			time.Sleep(*crashAfter)
			os.Exit(1)
		}()
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(b)
	})
	_ = http.Serve(ln, mux)
}
`

// BuildFakeServer compiles the fake server into a temp directory and
// returns the executable path. Skips the test if the go toolchain is
// unavailable.
func BuildFakeServer(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("go toolchain not available, skipping integration test")
	}
	dir := t.TempDir()
	src := filepath.Join(dir, "fakeserver.go")
	if err := os.WriteFile(src, []byte(fakeserverSrc), 0o600); err != nil {
		t.Fatalf("write fake server source: %v", err)
	}
	name := "fakeserver"
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	bin := filepath.Join(dir, name)
	c := exec.Command("go", "build", "-o", bin, src)
	c.Dir = dir
	out, err := c.CombinedOutput()
	if err != nil {
		t.Fatalf("build fake server: %v\n%s", err, out)
	}
	return bin
}

// FreePort returns a port that is free at the moment of the call.
func FreePort(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserve port: %v", err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	_ = ln.Close()
	return strconv.Itoa(port)
}
