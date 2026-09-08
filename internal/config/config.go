package config

import (
	"fmt"
	"os"
	"strconv"
	"strings"

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
}

// Config is the application configuration.
type Config struct {
	GlobalTTL int64            `yaml:"globalTTL"`
	StartPort int              `yaml:"startPort"`
	Models    map[string]Model `yaml:"models"`
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
	}
	return nil
}

// ProxyURL returns the model proxy URL with the port placeholder replaced.
func (m Model) ProxyURL(port int) string {
	return strings.ReplaceAll(m.Proxy, PortPlaceholder, strconv.Itoa(port))
}
