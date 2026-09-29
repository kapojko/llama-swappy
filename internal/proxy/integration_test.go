package proxy

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"llama-swappy/internal/testutil"
)

// End-to-end coverage of the self-description endpoints: real run
// scripts are written to disk (bash with Windows line endings and
// commented-out values, PowerShell, and a missing file), the real app
// binary is started, and /llama-swappy/info, /v1/models and a proxied
// request are verified over real HTTP.
//
// TestInfoEndpointFlow runs it natively (Unix hosts); on Windows,
// TestInfoEndpointWSL re-runs it inside the default WSL distro so the
// Unix path is covered there too.

const fakeWrapperBash = `#!/bin/bash
# Wrapper for the fake server. The parser must pick up the flags below
# (last uncommented occurrence wins); the real invocation only passes
# --port. Written with Unix line endings: a CRLF bash script would not
# run (the CRLF fixture is the .ps1, which is only parsed).
SERVER_ARGS=(
    #--port ${PORT} -c 32768
    --port ${PORT}
    -c 131072
    --reasoning on
)
exec %s --port ${PORT}
`

const psWrapper = `# PowerShell wrapper (parsed, never executed in this test)
$args = @(
    '-m', 'model.gguf',
    '-c', '8192'
)
& llama-server @args
`

// TestInfoEndpointFlow builds the app and a fake llama-server, writes
// run-script fixtures, starts the app and verifies both info endpoints
// plus a proxied request. Skipped on Windows (covered by
// TestInfoEndpointWSL, which runs this test inside WSL).
func TestInfoEndpointFlow(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("runs natively on Unix; on Windows it is re-run inside WSL by TestInfoEndpointWSL")
	}
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("go toolchain not available, skipping integration test")
	}

	dir := t.TempDir()
	root := filepath.Join("..", "..")
	appBin := filepath.Join(dir, "llama-swappy")
	c := exec.Command("go", "build", "-o", appBin, ".")
	c.Dir = root
	if out, err := c.CombinedOutput(); err != nil {
		t.Fatalf("build app: %v\n%s", err, out)
	}
	fakeBin := testutil.BuildFakeServer(t)

	// Fixtures: the bash wrapper runs for real (Unix endings); the .ps1
	// is only parsed (Windows endings, per requirement on mixed endings).
	bashScript := fmt.Sprintf(fakeWrapperBash, fakeBin)
	psScript := strings.ReplaceAll(psWrapper, "\n", "\r\n")
	bashPath := filepath.Join(dir, "run_fake.sh")
	psPath := filepath.Join(dir, "run.ps1")
	if err := os.WriteFile(bashPath, []byte(bashScript), 0o700); err != nil {
		t.Fatalf("write bash fixture: %v", err)
	}
	if err := os.WriteFile(psPath, []byte(psScript), 0o600); err != nil {
		t.Fatalf("write ps1 fixture: %v", err)
	}

	listenPort := testutil.FreePort(t)
	cfg := fmt.Sprintf(`globalTTL: 900
startPort: %s
models:
  flow-fake:
    name: "Flow Fake"
    cmd: 'env PORT=${PORT} %s'
    proxy: http://127.0.0.1:${PORT}
  flow-ps:
    name: "Flow PS"
    cmd: 'pwsh -File %s'
    proxy: http://127.0.0.1:${PORT}
    maxTokens: 1024
  flow-missing:
    name: "Flow Missing"
    cmd: 'env PORT=${PORT} %s'
    proxy: http://127.0.0.1:${PORT}
`, testutil.FreePort(t), bashPath, psPath, filepath.Join(dir, "no-such.sh"))
	cfgPath := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(cfgPath, []byte(cfg), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}

	var appOut bytes.Buffer
	pc := exec.Command(appBin, "--config", cfgPath, "--listen", "127.0.0.1:"+listenPort)
	pc.Stdout = &appOut
	pc.Stderr = &appOut
	if err := pc.Start(); err != nil {
		t.Fatalf("start app: %v", err)
	}
	t.Cleanup(func() {
		_ = pc.Process.Kill()
		_, _ = pc.Process.Wait()
	})

	base := "http://127.0.0.1:" + listenPort
	var infoResp map[string]any
	deadline := time.Now().Add(30 * time.Second)
	for {
		resp, err := http.Get(base + InfoPath)
		if err == nil {
			var body map[string]any
			if err := json.NewDecoder(resp.Body).Decode(&body); err == nil {
				infoResp = body
				break
			}
		}
		if resp != nil {
			resp.Body.Close()
		}
		if time.Now().After(deadline) {
			t.Fatalf("app did not come up; output:\n%s", appOut.String())
		}
		time.Sleep(100 * time.Millisecond)
	}

	// No model loaded yet.
	if infoResp["version"] == "" {
		t.Error("version missing from info response")
	}
	if infoResp["current"] != nil {
		t.Errorf("current = %v, want null (no model requested)", infoResp["current"])
	}
	models, _ := infoResp["models"].([]any)
	if len(models) != 3 {
		t.Fatalf("models = %v, want 3 entries", infoResp["models"])
	}
	byKey := map[string]map[string]any{}
	for _, m := range models {
		mm := m.(map[string]any)
		byKey[mm["key"].(string)] = mm
	}
	bash := byKey["flow-fake"]
	if bash["contextSize"] != float64(131072) {
		t.Errorf("flow-fake.contextSize = %v, want 131072 (last uncommented -c)", bash["contextSize"])
	}
	if bash["maxTokens"] != float64(32768) { // min(32768, 131072/2)
		t.Errorf("flow-fake.maxTokens = %v, want 32768", bash["maxTokens"])
	}
	if bash["reasoning"] != true {
		t.Errorf("flow-fake.reasoning = %v, want true", bash["reasoning"])
	}
	if bash["input"] == nil {
		t.Error("flow-fake.input missing")
	}
	ps := byKey["flow-ps"]
	if ps["contextSize"] != float64(8192) {
		t.Errorf("flow-ps.contextSize = %v, want 8192", ps["contextSize"])
	}
	if ps["maxTokens"] != float64(1024) { // YAML override wins
		t.Errorf("flow-ps.maxTokens = %v, want 1024 (YAML override)", ps["maxTokens"])
	}
	if _, present := ps["reasoning"]; present {
		t.Errorf("flow-ps.reasoning = %v, want omitted", ps["reasoning"])
	}
	missing := byKey["flow-missing"]
	for _, field := range []string{"contextSize", "maxTokens", "reasoning"} {
		if _, present := missing[field]; present {
			t.Errorf("flow-missing.%s = %v, want omitted (script missing)", field, missing[field])
		}
	}

	// The standard model list must report all configured model keys.
	resp, err := http.Get(base + ModelsPath)
	if err != nil {
		t.Fatalf("GET /v1/models: %v", err)
	}
	var list struct {
		Object string `json:"object"`
		Data   []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&list); err != nil {
		t.Fatalf("decode /v1/models: %v", err)
	}
	resp.Body.Close()
	ids := map[string]bool{}
	for _, d := range list.Data {
		ids[d.ID] = true
	}
	if list.Object != "list" || len(list.Data) != 3 || !ids["flow-fake"] || !ids["flow-ps"] || !ids["flow-missing"] {
		t.Errorf("/v1/models = %+v, want list of the 3 configured keys", list)
	}

	// Request the fake model: it is started through the bash wrapper,
	// the request is proxied, and the info endpoint now reports it.
	reqBody := `{"model":"flow-fake","ping":1}`
	resp, err = http.Post(base+"/v1/chat/completions", "application/json", bytes.NewReader([]byte(reqBody)))
	if err != nil {
		t.Fatalf("POST chat/completions: %v", err)
	}
	echo, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("proxied request status = %d, body: %s", resp.StatusCode, echo)
	}
	if string(echo) != reqBody {
		t.Errorf("proxied body = %q, want the request echoed", echo)
	}

	resp, err = http.Get(base + InfoPath)
	if err != nil {
		t.Fatalf("GET info: %v", err)
	}
	var infoResp2 map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&infoResp2); err != nil {
		t.Fatalf("decode info: %v", err)
	}
	resp.Body.Close()
	cur, ok := infoResp2["current"].(map[string]any)
	if !ok {
		t.Fatalf("current = %v, want object after a served request", infoResp2["current"])
	}
	if cur["key"] != "flow-fake" || cur["name"] != "Flow Fake" {
		t.Errorf("current = %v, want flow-fake", cur)
	}
	secs, _ := cur["secondsToUnload"].(float64)
	if secs <= 0 || secs > 900 {
		t.Errorf("secondsToUnload = %v, want in (0, 900]", secs)
	}

	// Startup log must report the parsed metadata and the parse failure.
	logs := appOut.String()
	if !strings.Contains(logs, "model metadata") {
		t.Errorf("startup log missing 'model metadata' lines:\n%s", logs)
	}
	if !strings.Contains(logs, "parse failed") || !strings.Contains(logs, "flow-missing") {
		t.Errorf("startup log missing parse-failure warning for flow-missing:\n%s", logs)
	}
}

// TestInfoEndpointWSL re-runs TestInfoEndpointFlow inside the default
// WSL distro (repo accessed via the /mnt/<drive> mount). The distro is
// expected to have a modern Go toolchain in $HOME/sdk/go (installed
// from the official tarball); if it is missing the test is skipped.
func TestInfoEndpointWSL(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("Windows only: it delegates to TestInfoEndpointFlow inside WSL")
	}
	if _, err := exec.LookPath("wsl"); err != nil {
		t.Skip("wsl not available on this machine")
	}
	absRoot, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatalf("repo root: %v", err)
	}
	out, err := exec.Command("wsl", "-e", "wslpath", "-u", absRoot).Output()
	if err != nil {
		t.Skipf("wslpath failed: %v", err)
	}
	wslRoot := strings.TrimSpace(string(out))

	// Check the toolchain first so the skip is cheap and precise.
	verOut, err := exec.Command("wsl", "-e", "bash", "-c",
		"$HOME/sdk/go/bin/go version || { echo NO_GO; exit 0; }").Output()
	if err != nil {
		t.Fatalf("wsl go version: %v", err)
	}
	if strings.Contains(string(verOut), "NO_GO") {
		t.Skip("no modern Go toolchain in WSL ($HOME/sdk/go); install from https://go.dev/dl")
	}

	cmdStr := fmt.Sprintf(
		`export PATH=$HOME/sdk/go/bin:$PATH; cd %s && go test -count=1 -timeout 15m -run '^TestInfoEndpointFlow$' ./internal/proxy`,
		wslRoot)
	c := exec.Command("wsl", "-e", "bash", "-c", cmdStr)
	res, err := c.CombinedOutput()
	if err != nil {
		t.Fatalf("go test inside WSL failed: %v\n%s", err, res)
	}
}
