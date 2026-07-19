package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/BurntSushi/toml"
)

type ClickUpConfig struct {
	APIKey string `toml:"api_key"`
}

type GitConfig struct {
	AutoContext bool `toml:"auto_context"`
}

type SyncConfig struct {
	MaxRetries    int `toml:"max_retries"`
	RateLimitMS   int `toml:"rate_limit_ms"`
	DrainTimeoutS int `toml:"drain_timeout_s"`
}

type DatabaseConfig struct {
	Path string `toml:"path"`
}

type Config struct {
	ClickUp  ClickUpConfig  `toml:"clickup"`
	Git      GitConfig      `toml:"git"`
	Sync     SyncConfig     `toml:"sync"`
	Database DatabaseConfig `toml:"database"`
}

// DefaultConfigContent is the default configuration content.
const DefaultConfigContent = `[clickup]
api_key = ""

[git]
auto_context = true

[sync]
max_retries = 5
rate_limit_ms = 600
drain_timeout_s = 5

[database]
path = "~/.taskflow/sessions.db"
`

func expandPath(path string) string {
	if strings.HasPrefix(path, "~") {
		home, err := os.UserHomeDir()
		if err != nil {
			return path
		}
		if path == "~" {
			return home
		}
		return filepath.Join(home, path[2:])
	}
	return path
}

func (c *Config) DBPath() string {
	return expandPath(c.Database.Path)
}

func EnsureDir() error {
	home, err := os.UserHomeDir()
	if err != nil {
		return fmt.Errorf("failed to get user home directory: %w", err)
	}
	taskflowDir := filepath.Join(home, ".taskflow")
	if err := os.MkdirAll(taskflowDir, 0755); err != nil {
		return fmt.Errorf("failed to create directory %s: %w", taskflowDir, err)
	}

	configPath := filepath.Join(taskflowDir, "config.toml")
	if _, err := os.Stat(configPath); os.IsNotExist(err) {
		if err := os.WriteFile(configPath, []byte(DefaultConfigContent), 0644); err != nil {
			return fmt.Errorf("failed to write default config: %w", err)
		}
	}
	return nil
}

// EnsureDirIn allows specifying a custom directory (useful for tests)
func EnsureDirIn(dir string) error {
	if err := os.MkdirAll(dir, 0755); err != nil {
		return fmt.Errorf("failed to create directory %s: %w", dir, err)
	}
	configPath := filepath.Join(dir, "config.toml")
	if _, err := os.Stat(configPath); os.IsNotExist(err) {
		customDefault := strings.Replace(DefaultConfigContent, `path = "~/.taskflow/sessions.db"`, fmt.Sprintf(`path = "%s/sessions.db"`, dir), 1)
		if err := os.WriteFile(configPath, []byte(customDefault), 0644); err != nil {
			return fmt.Errorf("failed to write custom default config: %w", err)
		}
	}
	return nil
}

func Load() (*Config, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, fmt.Errorf("failed to get user home directory: %w", err)
	}
	configPath := filepath.Join(home, ".taskflow", "config.toml")
	return LoadFrom(configPath)
}

func LoadFrom(configPath string) (*Config, error) {
	cfg := &Config{
		Git: GitConfig{
			AutoContext: true,
		},
		Sync: SyncConfig{
			MaxRetries:    5,
			RateLimitMS:   600,
			DrainTimeoutS: 5,
		},
		Database: DatabaseConfig{
			Path: "~/.taskflow/sessions.db",
		},
	}

	if _, err := os.Stat(configPath); err == nil {
		if _, err := toml.DecodeFile(configPath, cfg); err != nil {
			return nil, fmt.Errorf("failed to decode config TOML: %w", err)
		}
	}

	if apiKey := os.Getenv("CLICKUP_API_KEY"); apiKey != "" {
		cfg.ClickUp.APIKey = apiKey
	}

	return cfg, nil
}
