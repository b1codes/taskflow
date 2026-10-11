package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/BurntSushi/toml"
	"github.com/b1codes/taskflow/internal/session"
)

type ClickUpConfig struct {
	APIKey    string          `toml:"api_key"`
	StatusMap StatusMapConfig `toml:"status_map"`
}

// StatusMapConfig optionally pins session lifecycle roles to ClickUp status
// names. Empty fields use type-based defaults (see clickup.ResolveStatusForRole).
type StatusMapConfig struct {
	Start string `toml:"start"`
	// Reopen is used when a session starts on a task that is finished in
	// ClickUp; empty falls back to Start.
	Reopen   string `toml:"reopen"`
	Complete string `toml:"complete"`
	// Review is used when a session completes with required completeness
	// obligations open under the review gate.
	Review  string `toml:"review"`
	Archive string `toml:"archive"`
}

type GitConfig struct {
	AutoContext bool `toml:"auto_context"`
}

type SyncConfig struct {
	MaxRetries    int `toml:"max_retries"`
	RateLimitMS   int `toml:"rate_limit_ms"`
	DrainTimeoutS int `toml:"drain_timeout_s"`
}

// CompletenessConfig configures completeness enforcement for projects with a
// completeness manifest.
type CompletenessConfig struct {
	// Gate decides what stopping a session as completed does while required
	// obligations are open: report, review, or block. It is a floor: a
	// manifest's gate can make it stricter, never looser.
	Gate string `toml:"gate"`
}

// GateMode parses Gate with session.ParseGateMode, the one check of gate
// names, so a typo is an error rather than a gate that fails open. "" is the
// default gate.
func (c CompletenessConfig) GateMode() (session.GateMode, error) {
	g, err := session.ParseGateMode(c.Gate)
	if err != nil {
		return "", fmt.Errorf("invalid config: completeness.gate: %w", err)
	}
	return g, nil
}

type DatabaseConfig struct {
	Path string `toml:"path"`
}

type Config struct {
	ClickUp      ClickUpConfig      `toml:"clickup"`
	Git          GitConfig          `toml:"git"`
	Sync         SyncConfig         `toml:"sync"`
	Completeness CompletenessConfig `toml:"completeness"`
	Database     DatabaseConfig     `toml:"database"`
}

// DefaultConfigContent is the default configuration content.
const DefaultConfigContent = `[clickup]
api_key = ""

# Optional: pin session lifecycle events to status names on your lists.
# Unset roles use defaults: start = the "custom" status named like
# "in progress", complete = first "done" (else "closed"), review = the
# "custom" status named like "in review" (else unchanged), archive = first
# "closed" named like "cancelled" (else unchanged). Reopening a finished task
# uses reopen, else start. Taskflow never moves a task back from a status
# someone else chose.
[clickup.status_map]
# start = "in progress"
# reopen = "in progress"
# complete = "complete"
# review = "in review"
# archive = "cancelled"

[git]
auto_context = true

[sync]
max_retries = 5
rate_limit_ms = 600
drain_timeout_s = 5

# Completion gate for projects with a completeness manifest, when a session
# is stopped as completed with required obligations open:
#   report = complete as usual; the stop comment lists what's open
#   review = complete locally, move the ClickUp task to review (default)
#   block  = refuse to complete until the work is done, attested, or waived
# This is a floor: a project manifest's "gate:" can make it stricter, never
# looser, so an agent can't lower it by editing the repo.
[completeness]
gate = "review"

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
	if err := os.MkdirAll(taskflowDir, 0700); err != nil {
		return fmt.Errorf("failed to create directory %s: %w", taskflowDir, err)
	}

	configPath := filepath.Join(taskflowDir, "config.toml")
	if _, err := os.Stat(configPath); os.IsNotExist(err) {
		if err := os.WriteFile(configPath, []byte(DefaultConfigContent), 0600); err != nil {
			return fmt.Errorf("failed to write default config: %w", err)
		}
	}
	return nil
}

// EnsureDirIn allows specifying a custom directory (useful for tests)
func EnsureDirIn(dir string) error {
	if err := os.MkdirAll(dir, 0700); err != nil {
		return fmt.Errorf("failed to create directory %s: %w", dir, err)
	}
	configPath := filepath.Join(dir, "config.toml")
	if _, err := os.Stat(configPath); os.IsNotExist(err) {
		customDefault := strings.Replace(DefaultConfigContent, `path = "~/.taskflow/sessions.db"`, fmt.Sprintf(`path = "%s/sessions.db"`, dir), 1)
		if err := os.WriteFile(configPath, []byte(customDefault), 0600); err != nil {
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
		Completeness: CompletenessConfig{
			Gate: string(session.DefaultGate),
		},
		Database: DatabaseConfig{
			Path: "~/.taskflow/sessions.db",
		},
	}

	if fi, err := os.Stat(configPath); err == nil {
		// Warn if the config file containing credentials is group/world-accessible
		if fi.Mode().Perm()&0077 != 0 {
			fmt.Fprintf(os.Stderr, "Warning: config file %s is readable by other users (mode %#o). Recommend: chmod 0600 %s\n", configPath, fi.Mode().Perm(), configPath)
		}
		if _, err := toml.DecodeFile(configPath, cfg); err != nil {
			return nil, fmt.Errorf("failed to decode config TOML: %w", err)
		}
	}

	gate, err := cfg.Completeness.GateMode()
	if err != nil {
		return nil, err
	}
	cfg.Completeness.Gate = string(gate)

	if apiKey := os.Getenv("CLICKUP_API_KEY"); apiKey != "" {
		cfg.ClickUp.APIKey = apiKey
	}

	return cfg, nil
}
