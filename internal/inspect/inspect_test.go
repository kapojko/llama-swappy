package inspect

import (
	"os"
	"path/filepath"
	"testing"
)

const bashScript = `#!/bin/bash

# Paths
MODEL_PATH="$HOME/models/test/model.gguf"
SERVER_BIN="$HOME/llama.cpp/build/bin/llama-server"

# Check if model exists
if [ ! -f "$MODEL_PATH" ]; then
    echo "Error: Model not found at $MODEL_PATH"
    exit 1
fi

# Server run arguments
SERVER_ARGS=(
    -m "$MODEL_PATH"
    --host 0.0.0.0
    --port ${PORT:-12380}

    --n-gpu-layers all

    #-c 98304
    #-c 114688
    -c 131072
    -ctk q4_0 -ctv q4_0
    -np 1

    # -b 2048 -ub 512

    --reasoning on
    --reasoning-preserve

    --chat-template-kwargs {\"reasoning_effort\":\"high\"}
    --temp 1.0
    --threads $(nproc)
)

# Run the server
$SERVER_BIN "${SERVER_ARGS[@]}"
`

func writeFile(t *testing.T, name, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write %s: %v", name, err)
	}
	return path
}

func TestInspectBashScript(t *testing.T) {
	path := writeFile(t, "run.sh", bashScript)
	info, err := Inspect("env PORT=${PORT} "+path, 12390)
	if err != nil {
		t.Fatalf("Inspect: %v", err)
	}
	// The last uncommented -c wins over the commented-out values.
	if info.ContextSize != 131072 {
		t.Errorf("ContextSize = %d, want 131072", info.ContextSize)
	}
	if info.Reasoning == nil || !*info.Reasoning {
		t.Errorf("Reasoning = %v, want true", info.Reasoning)
	}
	if info.MaxTokens != 0 {
		t.Errorf("MaxTokens = %d, want 0 (not set in script)", info.MaxTokens)
	}
}

func TestInspectCRLFLineEndings(t *testing.T) {
	crlf := "# comment\r\nSERVER_ARGS=(\r\n    #-c 4096\r\n    -c 65536\r\n    --reasoning off\r\n)\r\n"
	path := writeFile(t, "run.sh", crlf)
	info, err := Inspect(path, 0)
	if err != nil {
		t.Fatalf("Inspect: %v", err)
	}
	if info.ContextSize != 65536 {
		t.Errorf("ContextSize = %d, want 65536", info.ContextSize)
	}
	if info.Reasoning == nil || *info.Reasoning {
		t.Errorf("Reasoning = %v, want false", info.Reasoning)
	}
}

func TestInspectPowerShellScript(t *testing.T) {
	ps1 := `# PowerShell wrapper
$MODEL = "$HOME\models\test\model.gguf"
$args = @(
    '-m', $MODEL,
    '--host', '0.0.0.0',
    '-c', '147456',
    '--max-tokens', '8192',
    '--reasoning', 'on'
)
& llama-server @args
`
	path := writeFile(t, "run.ps1", ps1)
	info, err := Inspect("pwsh -File "+path, 0)
	if err != nil {
		t.Fatalf("Inspect: %v", err)
	}
	if info.ContextSize != 147456 {
		t.Errorf("ContextSize = %d, want 147456", info.ContextSize)
	}
	if info.MaxTokens != 8192 {
		t.Errorf("MaxTokens = %d, want 8192", info.MaxTokens)
	}
	if info.Reasoning == nil || !*info.Reasoning {
		t.Errorf("Reasoning = %v, want true", info.Reasoning)
	}
}

func TestInspectCtxSizeEqualsForm(t *testing.T) {
	path := writeFile(t, "run.sh", "#!/bin/bash\nllama-server --ctx-size=8192 --reasoning=true\n")
	info, err := Inspect(path, 0)
	if err != nil {
		t.Fatalf("Inspect: %v", err)
	}
	if info.ContextSize != 8192 {
		t.Errorf("ContextSize = %d, want 8192", info.ContextSize)
	}
	if info.Reasoning == nil || !*info.Reasoning {
		t.Errorf("Reasoning = %v, want true", info.Reasoning)
	}
}

func TestInspectNoContextFlag(t *testing.T) {
	path := writeFile(t, "run.sh", "#!/bin/bash\nllama-server -m model.gguf --reasoning\n")
	info, err := Inspect(path, 0)
	if err != nil {
		t.Fatalf("Inspect: %v", err)
	}
	// Absent -c leaves ContextSize unknown (not an error); bare
	// --reasoning means on.
	if info.ContextSize != 0 {
		t.Errorf("ContextSize = %d, want 0 (unknown)", info.ContextSize)
	}
	if info.Reasoning == nil || !*info.Reasoning {
		t.Errorf("Reasoning = %v, want true (bare flag)", info.Reasoning)
	}
}

func TestInspectMissingFile(t *testing.T) {
	if _, err := Inspect("env PORT=${PORT} /nonexistent/run.sh", 12390); err == nil {
		t.Fatal("expected error for missing script")
	}
}

func TestInspectBinaryFile(t *testing.T) {
	// A .sh file with binary content must be rejected.
	path := writeFile(t, "run.sh", "\x00\x01\x02not-a-text-file\x00")
	if _, err := Inspect(path, 0); err == nil {
		t.Fatal("expected error for binary script")
	}
}

func TestInspectDirectCommandLine(t *testing.T) {
	// No wrapper script: the cmd itself is parsed as server args.
	info, err := Inspect(`llama-server -m model.gguf --port ${PORT} -c 8192 --reasoning off`, 12390)
	if err != nil {
		t.Fatalf("Inspect: %v", err)
	}
	if info.ContextSize != 8192 {
		t.Errorf("ContextSize = %d, want 8192", info.ContextSize)
	}
	if info.Reasoning == nil || *info.Reasoning {
		t.Errorf("Reasoning = %v, want false", info.Reasoning)
	}
}

func TestInspectPortSubstitution(t *testing.T) {
	// The ${PORT} placeholder in the script path must be substituted
	// with the port before the file is read.
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "run_12390.sh"),
		[]byte("#!/bin/bash\nllama-server -c 1024\n"), 0o600); err != nil {
		t.Fatalf("write script: %v", err)
	}
	info, err := Inspect("env PORT=${PORT} "+filepath.Join(dir, `run_${PORT}.sh`), 12390)
	if err != nil {
		t.Fatalf("Inspect: %v", err)
	}
	if info.ContextSize != 1024 {
		t.Errorf("ContextSize = %d, want 1024", info.ContextSize)
	}
}

func TestMaxTokensResolution(t *testing.T) {
	cases := []struct {
		name     string
		explicit int
		info     Info
		want     int
	}{
		{"explicit wins", 4096, Info{ContextSize: 131072}, 4096},
		{"script max-tokens", 0, Info{ContextSize: 131072, MaxTokens: 16384}, 16384},
		{"derived half of context", 0, Info{ContextSize: 40960}, 20480},
		{"derived capped", 0, Info{ContextSize: 131072}, 32768},
		{"unknown context, nothing else", 0, Info{}, 0},
		{"explicit with unknown context", 8192, Info{}, 8192},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := MaxTokens(tc.explicit, tc.info); got != tc.want {
				t.Errorf("MaxTokens = %d, want %d", got, tc.want)
			}
		})
	}
}
