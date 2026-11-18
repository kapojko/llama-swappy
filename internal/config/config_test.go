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
	want := []string{"qwen3.8-27b", "ornith-1.5-9b", "ornith-1.5-9b-np4", "swift-1.5-qwen3.8-27b"}
	if len(c.Models) < len(want) {
		t.Fatalf("len(Models) = %d, want at least %d", len(c.Models), len(want))
	}
	for _, key := range want {
		if _, ok := c.Models[key]; !ok {
			t.Errorf("expected model key %s", key)
		}
	}
}

func TestValidateMonitor(t *testing.T) {
	validCfg := func() *Config {
		return &Config{
			GlobalTTL: 900,
			StartPort: 12390,
			Models: map[string]Model{
				"m1": {Name: "M", Cmd: "c", Proxy: "http://127.0.0.1:${PORT}"},
			},
		}
	}
	base := func() *MonitorConfig {
		return &MonitorConfig{Enabled: true, LogDir: filepath.Join(t.TempDir(), "logs")}
	}
	cases := []struct {
		name    string
		mutate  func(*MonitorConfig)
		wantErr string
	}{
		{"bad interval", func(m *MonitorConfig) { m.Interval = "five" }, "interval"},
		{"zero interval", func(m *MonitorConfig) { m.Interval = "0s" }, "interval"},
		{"relative logDir", func(m *MonitorConfig) { m.LogDir = "logs" }, "logDir"},
		{"negative retention", func(m *MonitorConfig) { m.RetentionDays = -1 }, "retentionDays"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := validCfg()
			c.Monitor = base()
			tc.mutate(c.Monitor)
			err := c.Validate()
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("Validate() = %v, want error containing %q", err, tc.wantErr)
			}
		})
	}
	// A disabled or absent section is always valid, whatever it says.
	for _, m := range []*MonitorConfig{nil, {Enabled: false, Interval: "garbage"}} {
		c := validCfg()
		c.Monitor = m
		if err := c.Validate(); err != nil {
			t.Errorf("Validate() with monitor %v = %v, want nil", m, err)
		}
	}
}

func TestLoadMonitorSection(t *testing.T) {
	logDir := filepath.Join(t.TempDir(), "monitor-logs")
	yaml := validYAML + `
monitor:
  enabled: true
  logDir: ` + logDir + `
  interval: 5s
  retentionDays: 14
`
	c, err := Load(writeConfig(t, yaml))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.Monitor == nil || !c.Monitor.Enabled || c.Monitor.LogDir != logDir ||
		c.Monitor.Interval != "5s" || c.Monitor.RetentionDays != 14 {
		t.Fatalf("Monitor = %+v", c.Monitor)
	}
}

func TestValidatePoweroff(t *testing.T) {
	validCfg := func() *Config {
		return &Config{
			GlobalTTL: 900,
			StartPort: 12390,
			Models: map[string]Model{
				"m1": {Name: "M", Cmd: "c", Proxy: "http://127.0.0.1:${PORT}"},
			},
		}
	}
	// A disabled or absent section is always valid, whatever it says.
	for _, p := range []*PoweroffConfig{nil, {Enabled: false, IdleSeconds: 0}, {Enabled: false, IdleSeconds: -5}} {
		c := validCfg()
		c.Poweroff = p
		if err := c.Validate(); err != nil {
			t.Errorf("Validate() with poweroff %+v = %v, want nil", p, err)
		}
	}
	cases := []struct {
		name string
		pc   *PoweroffConfig
	}{
		{"zero idleSeconds", &PoweroffConfig{Enabled: true}},
		{"negative idleSeconds", &PoweroffConfig{Enabled: true, IdleSeconds: -1}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := validCfg()
			c.Poweroff = tc.pc
			err := c.Validate()
			if err == nil || !strings.Contains(err.Error(), "poweroff: idleSeconds") {
				t.Errorf("Validate() = %v, want error about poweroff idleSeconds", err)
			}
		})
	}
	// Enabled with idleSeconds set is valid on any platform (the
	// command default / platform check lives in poweroff.New).
	c := validCfg()
	c.Poweroff = &PoweroffConfig{Enabled: true, IdleSeconds: 3600}
	if err := c.Validate(); err != nil {
		t.Errorf("Validate() with enabled poweroff = %v, want nil", err)
	}
}

func TestLoadPoweroffSection(t *testing.T) {
	yaml := validYAML + `
poweroff:
  enabled: true
  idleSeconds: 3600
  command: "/usr/bin/systemctl poweroff"
`
	c, err := Load(writeConfig(t, yaml))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.Poweroff == nil || !c.Poweroff.Enabled || c.Poweroff.IdleSeconds != 3600 ||
		c.Poweroff.Command != "/usr/bin/systemctl poweroff" {
		t.Fatalf("Poweroff = %+v", c.Poweroff)
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
