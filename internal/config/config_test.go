package config

import (
	"os"
	"path/filepath"
	"testing"
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
`
	if err := os.WriteFile(configPath, []byte(content), 0600); err != nil {
		t.Fatalf("failed to write config: %v", err)
	}

	cfg, err := LoadFrom(configPath)
	if err != nil {
		t.Fatalf("LoadFrom failed: %v", err)
	}

	want := StatusMapConfig{Start: "in progress", Complete: "complete"}
	if got := cfg.ClickUp.StatusMap; got != want {
		t.Errorf("expected status map %+v, got %+v", want, got)
	}
}
