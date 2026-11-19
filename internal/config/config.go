package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// PortPlaceholder is the token in a model proxy URL that gets replaced
// with the assigned port.
const PortPlaceholder = "${PORT}"

// Model describes one loadable model.
type Model struct {
	Name  string `yaml:"name"`
	Cmd   string `yaml:"cmd"`
	Proxy string `yaml:"proxy"`
	// MaxTokens optionally overrides the per-response output token
	// limit reported by /llama-swappy/info. Zero (the default) lets the
	// app derive it from the run script / context size.
	MaxTokens int `yaml:"maxTokens,omitempty"`
}

// MonitorConfig is the optional hardware monitoring section (the
// `monitor` key). It is fully opt-in: when it is absent or Enabled is
// false, the application performs no hardware access at all.
//
// Fields:
//   - Enabled: turns monitoring on; false by default
//   - LogDir: optional directory for JSONL logs (one file per day,
//     monitor-YYYY-MM-DD.jsonl). Empty means monitoring runs and the
//     /llama-swappy/monitor endpoint serves live data, but nothing is
//     written to disk.
//   - Interval: sampling interval as a Go duration string ("5s");
//     empty means 5s
//   - RetentionDays: how many days of log files to keep; 0 means 7
type MonitorConfig struct {
	Enabled       bool   `yaml:"enabled"`
	LogDir        string `yaml:"logDir,omitempty"`
	Interval      string `yaml:"interval,omitempty"`
	RetentionDays int    `yaml:"retentionDays,omitempty"`
}

// BackupConfig is the optional config backup section (the `backup`
// key). Disabled by default: when it is absent or Enabled is false the
// application never touches the backup directory.
//
// Fields:
//   - Enabled: turns config backup on; false by default
//   - Dir: mandatory when enabled. Absolute path to the backup root
//     (e.g. a git repository holding per-machine config trees). The
//     files for this machine are kept under Dir/<machineName>/.
//   - MachineName: optional. The sub-directory name identifying this
//     machine. When empty it is derived from the host name and the OS
//     (e.g. "workpc_linux").
type BackupConfig struct {
	Enabled     bool   `yaml:"enabled"`
	Dir         string `yaml:"dir,omitempty"`
	MachineName string `yaml:"machineName,omitempty"`
}

// PoweroffConfig is the optional idle poweroff section (the
// `poweroff` key). Disabled by default: when it is absent or Enabled
// is false, the application never attempts to power the system off.
//
// Fields:
//   - Enabled: turns the feature on; false by default
//   - IdleSeconds: how long the system must stay without a loaded
//     model before the health check runs and the poweroff command
//     executes. Required when enabled. Counting starts only after the
//     first model load and its subsequent unload, restarts from the
//     beginning on every unload, and is cancelled by any new model
//     load; an unhealthy system restarts the full countdown.
//   - Command: the poweroff command line (split on whitespace, double
//     quotes honored, no shell). Optional on Linux, where it defaults
//     to /usr/bin/systemctl poweroff; required on other platforms.
type PoweroffConfig struct {
	Enabled     bool   `yaml:"enabled"`
	IdleSeconds int    `yaml:"idleSeconds,omitempty"`
	Command     string `yaml:"command,omitempty"`
}

// Config is the application configuration.
type Config struct {
	GlobalTTL int64            `yaml:"globalTTL"`
	StartPort int              `yaml:"startPort"`
	Models    map[string]Model `yaml:"models"`
	// Monitor is nil when the config has no monitor section at all.
	Monitor *MonitorConfig `yaml:"monitor,omitempty"`
	// Poweroff is nil when the config has no poweroff section at all.
	Poweroff *PoweroffConfig `yaml:"poweroff,omitempty"`
	// Backup is nil when the config has no backup section at all.
	Backup *BackupConfig `yaml:"backup,omitempty"`
}

// Load reads and validates the YAML config at path.
func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config: %w", err)
	}
	var c Config
	if err := yaml.Unmarshal(data, &c); err != nil {
		return nil, fmt.Errorf("parse config: %w", err)
	}
	if err := c.Validate(); err != nil {
		return nil, err
	}
	return &c, nil
}

// Validate checks that all supported parameters are present and sane.
func (c *Config) Validate() error {
	if c.GlobalTTL <= 0 {
		return fmt.Errorf("globalTTL must be > 0, got %d", c.GlobalTTL)
	}
	if c.StartPort < 1 || c.StartPort > 65535 {
		return fmt.Errorf("startPort must be in [1, 65535], got %d", c.StartPort)
	}
	if len(c.Models) == 0 {
		return fmt.Errorf("models: at least one model is required")
	}
	for key, m := range c.Models {
		if m.Name == "" {
			return fmt.Errorf("model %q: name is required", key)
		}
		if strings.TrimSpace(m.Cmd) == "" {
			return fmt.Errorf("model %q: cmd is required", key)
		}
		if !strings.Contains(m.Proxy, PortPlaceholder) {
			return fmt.Errorf("model %q: proxy must contain %s", key, PortPlaceholder)
		}
		if m.MaxTokens < 0 {
			return fmt.Errorf("model %q: maxTokens must be >= 0, got %d", key, m.MaxTokens)
		}
	}
	if err := c.validateMonitor(); err != nil {
		return err
	}
	if err := c.validatePoweroff(); err != nil {
		return err
	}
	return c.validateBackup()
}

// validatePoweroff checks the optional poweroff section. An absent or
// disabled section is always valid: it must never keep the app from
// starting. An enabled one requires idleSeconds (there is no default);
// the command default / platform check lives in poweroff.New, where
// the runtime platform is resolved once.
func (c *Config) validatePoweroff() error {
	if c.Poweroff == nil || !c.Poweroff.Enabled {
		return nil
	}
	if c.Poweroff.IdleSeconds <= 0 {
		return fmt.Errorf("poweroff: idleSeconds is required and must be > 0, got %d", c.Poweroff.IdleSeconds)
	}
	return nil
}

// validateBackup checks the optional backup section. A disabled or
// absent section is always valid: it must never keep the app from
// starting. An enabled one requires an absolute backup directory; the
// machine name is optional (derived at runtime when omitted).
func (c *Config) validateBackup() error {
	if c.Backup == nil || !c.Backup.Enabled {
		return nil
	}
	if c.Backup.Dir == "" {
		return fmt.Errorf("backup: dir is required when enabled")
	}
	if !filepath.IsAbs(c.Backup.Dir) {
		return fmt.Errorf("backup: dir must be an absolute path, got %q", c.Backup.Dir)
	}
	return nil
}

// validateMonitor checks the optional monitor section. A disabled or
// absent section is always valid: it must never keep the app from
// starting. An enabled one gets the same sanity treatment as the rest
// of the config, so a typo is reported at startup instead of being
// silently ignored by the sampler.
func (c *Config) validateMonitor() error {
	if c.Monitor == nil || !c.Monitor.Enabled {
		return nil
	}
	if c.Monitor.Interval != "" {
		d, err := time.ParseDuration(c.Monitor.Interval)
		if err != nil {
			return fmt.Errorf("monitor: interval must be a duration like 5s, got %q", c.Monitor.Interval)
		}
		if d < time.Second {
			return fmt.Errorf("monitor: interval must be >= 1s, got %s", d)
		}
	}
	if c.Monitor.RetentionDays < 0 {
		return fmt.Errorf("monitor: retentionDays must be >= 0, got %d", c.Monitor.RetentionDays)
	}
	if c.Monitor.LogDir != "" && !filepath.IsAbs(c.Monitor.LogDir) {
		return fmt.Errorf("monitor: logDir must be an absolute path, got %q", c.Monitor.LogDir)
	}
	return nil
}

// ProxyURL returns the model proxy URL with the port placeholder replaced.
func (m Model) ProxyURL(port int) string {
	return strings.ReplaceAll(m.Proxy, PortPlaceholder, strconv.Itoa(port))
}
