package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/b1codes/taskflow/internal/session"
)

func TestLoadDefaults(t *testing.T) {
	cfg, err := LoadFrom("nonexistent.toml")
	if err != nil {
		t.Fatalf("LoadFrom failed: %v", err)
	}
	if !cfg.Git.AutoContext {
		t.Errorf("expected Git.AutoContext to be true")
	}
	if cfg.Sync.MaxRetries != 5 {
		t.Errorf("expected Sync.MaxRetries to be 5, got %d", cfg.Sync.MaxRetries)
	}
	if cfg.Database.Path != "~/.taskflow/sessions.db" {
		t.Errorf("expected Database.Path to be ~/.taskflow/sessions.db, got %s", cfg.Database.Path)
	}
}

func TestLoadEnvOverride(t *testing.T) {
	t.Setenv("CLICKUP_API_KEY", "env-api-key")
	cfg, err := LoadFrom("nonexistent.toml")
	if err != nil {
		t.Fatalf("LoadFrom failed: %v", err)
	}
	if cfg.ClickUp.APIKey != "env-api-key" {
		t.Errorf("expected ClickUp.APIKey to be overridden by env, got %s", cfg.ClickUp.APIKey)
	}
}

func TestEnsureDirIn(t *testing.T) {
	tempDir, err := os.MkdirTemp("", "taskflow-test-*")
	if err != nil {
		t.Fatalf("failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tempDir)

	err = EnsureDirIn(tempDir)
	if err != nil {
		t.Fatalf("EnsureDirIn failed: %v", err)
	}

	configPath := filepath.Join(tempDir, "config.toml")
	info, err := os.Stat(configPath)
	if err != nil {
		t.Fatalf("config.toml was not created: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0600 {
		t.Errorf("expected config.toml permissions 0600, got %#o", perm)
	}

	cfg, err := LoadFrom(configPath)
	if err != nil {
		t.Fatalf("LoadFrom custom file failed: %v", err)
	}
	expectedDBPath := filepath.Join(tempDir, "sessions.db")
	if cfg.DBPath() != expectedDBPath {
		t.Errorf("expected DBPath to be %s, got %s", expectedDBPath, cfg.DBPath())
	}
}

func TestLoadStatusMap(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "config.toml")
	content := `[clickup]
api_key = "k"

[clickup.status_map]
start = "in progress"
complete = "complete"
review = "in review"
`
	if err := os.WriteFile(configPath, []byte(content), 0600); err != nil {
		t.Fatalf("failed to write config: %v", err)
	}

	cfg, err := LoadFrom(configPath)
	if err != nil {
		t.Fatalf("LoadFrom failed: %v", err)
	}

	want := StatusMapConfig{Start: "in progress", Complete: "complete", Review: "in review"}
	if got := cfg.ClickUp.StatusMap; got != want {
		t.Errorf("expected status map %+v, got %+v", want, got)
	}
}

func TestLoadCompletenessGate(t *testing.T) {
	load := func(t *testing.T, content string) (*Config, error) {
		t.Helper()
		configPath := filepath.Join(t.TempDir(), "config.toml")
		if err := os.WriteFile(configPath, []byte(content), 0600); err != nil {
			t.Fatalf("failed to write config: %v", err)
		}
		return LoadFrom(configPath)
	}

	cfg, err := load(t, "[git]\nauto_context = true\n")
	if err != nil || cfg.Completeness.Gate != "review" {
		t.Errorf("default gate = %q, %v; want review", cfg.Completeness.Gate, err)
	}
	cfg, err = load(t, "[completeness]\ngate = \"block\"\n")
	if err != nil || cfg.Completeness.Gate != "block" {
		t.Errorf("gate = %q, %v; want block", cfg.Completeness.Gate, err)
	}
	if _, err := load(t, "[completeness]\ngate = \"strict\"\n"); err == nil || !strings.Contains(err.Error(), "completeness.gate") {
		t.Errorf("an unknown gate should fail to load, got %v", err)
	}

	// The default config file sets the default gate.
	cfg, err = load(t, DefaultConfigContent)
	if err != nil || cfg.Completeness.Gate != "review" {
		t.Errorf("DefaultConfigContent gate = %q, %v", cfg.Completeness.Gate, err)
	}

	// An explicitly empty gate is the default gate.
	cfg, err = load(t, "[completeness]\ngate = \"\"\n")
	if err != nil || cfg.Completeness.Gate != "review" {
		t.Errorf("empty gate = %q, %v; want review", cfg.Completeness.Gate, err)
	}
}

func TestCompletenessConfigGateMode(t *testing.T) {
	for gate, want := range map[string]session.GateMode{
		"":       session.GateReview,
		"report": session.GateReport,
		"review": session.GateReview,
		"block":  session.GateBlock,
	} {
		if got, err := (CompletenessConfig{Gate: gate}).GateMode(); err != nil || got != want {
			t.Errorf("GateMode(%q) = %q, %v; want %q", gate, got, err, want)
		}
	}
	// A Config not built by LoadFrom is checked the same way.
	if got, err := (CompletenessConfig{Gate: "blok"}).GateMode(); err == nil || got != "" || !strings.Contains(err.Error(), "completeness.gate") {
		t.Errorf("GateMode(blok) = %q, %v; want an error", got, err)
	}
}
