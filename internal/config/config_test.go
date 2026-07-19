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
	if _, err := os.Stat(configPath); os.IsNotExist(err) {
		t.Errorf("config.toml was not created")
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
