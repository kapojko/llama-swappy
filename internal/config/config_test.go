package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const validYAML = `
globalTTL: 900
startPort: 12390
models:
  m1:
    name: "Model One"
    cmd: env PORT=${PORT} /home/yury/run_m1.sh
    proxy: http://127.0.0.1:${PORT}
  m2:
    name: "Model Two"
    cmd: env PORT=${PORT} /home/yury/run_m2.sh
    proxy: http://127.0.0.1:${PORT}
`

func writeConfig(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	return path
}

func TestLoadValid(t *testing.T) {
	c, err := Load(writeConfig(t, validYAML))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.GlobalTTL != 900 {
		t.Errorf("GlobalTTL = %d, want 900", c.GlobalTTL)
	}
	if c.StartPort != 12390 {
		t.Errorf("StartPort = %d, want 12390", c.StartPort)
	}
	if len(c.Models) != 2 {
		t.Fatalf("len(Models) = %d, want 2", len(c.Models))
	}
	m := c.Models["m1"]
	if m.Name != "Model One" {
		t.Errorf("m1.Name = %q, want %q", m.Name, "Model One")
	}
	got := m.ProxyURL(12390)
	want := "http://127.0.0.1:12390"
	if got != want {
		t.Errorf("ProxyURL = %q, want %q", got, want)
	}
}

func TestLoadMissingFile(t *testing.T) {
	if _, err := Load(filepath.Join(t.TempDir(), "nope.yaml")); err == nil {
		t.Fatal("expected error for missing file")
	}
}

func TestLoadInvalidYAML(t *testing.T) {
	if _, err := Load(writeConfig(t, "{{{")); err == nil {
		t.Fatal("expected error for invalid yaml")
	}
}

func TestValidate(t *testing.T) {
	cases := []struct {
		name    string
		mutate  func(*Config)
		wantErr string
	}{
		{"zero ttl", func(c *Config) { c.GlobalTTL = 0 }, "globalTTL"},
		{"negative ttl", func(c *Config) { c.GlobalTTL = -1 }, "globalTTL"},
		{"port too low", func(c *Config) { c.StartPort = 0 }, "startPort"},
		{"port too high", func(c *Config) { c.StartPort = 70000 }, "startPort"},
		{"no models", func(c *Config) { c.Models = nil }, "at least one model"},
		{"empty name", func(c *Config) { m := c.Models["m1"]; m.Name = ""; c.Models["m1"] = m }, "name is required"},
		{"empty cmd", func(c *Config) { m := c.Models["m1"]; m.Cmd = "   "; c.Models["m1"] = m }, "cmd is required"},
		{"proxy without placeholder", func(c *Config) { m := c.Models["m1"]; m.Proxy = "http://127.0.0.1:12390"; c.Models["m1"] = m }, "proxy must contain"},
		{"negative maxTokens", func(c *Config) { m := c.Models["m1"]; m.MaxTokens = -1; c.Models["m1"] = m }, "maxTokens"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c, err := Load(writeConfig(t, validYAML))
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			tc.mutate(c)
			err = c.Validate()
			if err == nil {
				t.Fatal("expected validation error, got nil")
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("error = %q, want it to contain %q", err.Error(), tc.wantErr)
			}
		})
	}
}

func TestLoadExampleFile(t *testing.T) {
	// The shipped example config must load and validate.
	c, err := Load(filepath.Join("..", "..", "example", "llama-swap-config.yaml"))
	if err != nil {
		t.Fatalf("Load example: %v", err)
	}
	if len(c.Models) != 2 {
		t.Fatalf("len(Models) = %d, want 2", len(c.Models))
	}
	if _, ok := c.Models["qwen3.8-27b"]; !ok {
		t.Error("expected model key qwen3.8-27b")
	}
}

func TestLoadMaxTokensOverride(t *testing.T) {
	yaml := strings.Replace(validYAML, "    proxy: http://127.0.0.1:${PORT}\n  m2:",
		"    proxy: http://127.0.0.1:${PORT}\n    maxTokens: 4096\n  m2:", 1)
	c, err := Load(writeConfig(t, yaml))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.Models["m1"].MaxTokens != 4096 {
		t.Errorf("m1.MaxTokens = %d, want 4096", c.Models["m1"].MaxTokens)
	}
	if c.Models["m2"].MaxTokens != 0 {
		t.Errorf("m2.MaxTokens = %d, want 0 (unset)", c.Models["m2"].MaxTokens)
	}
}
