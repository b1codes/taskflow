package gitctx

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func TestGitContext_Capture(t *testing.T) {
	// Create temp directory
	tempDir, err := os.MkdirTemp("", "taskflow-git-test-*")
	if err != nil {
		t.Fatalf("failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tempDir)

	// Run capture on non-git dir -> should return nil, nil
	ctx, err := Capture(tempDir)
	if err != nil {
		t.Fatalf("Capture on non-git dir failed: %v", err)
	}
	if ctx != nil {
		t.Errorf("expected Capture to return nil context for non-git dir, got: %+v", ctx)
	}

	// Initialize git repo in tempDir
	if err := exec.Command("git", "-C", tempDir, "init").Run(); err != nil {
		t.Skip("skipping git test: git command failed to run git init")
	}

	// Configure mock git user
	_ = exec.Command("git", "-C", tempDir, "config", "user.name", "Test").Run()
	_ = exec.Command("git", "-C", tempDir, "config", "user.email", "test@test.com").Run()
	_ = exec.Command("git", "-C", tempDir, "config", "commit.gpgsign", "false").Run()

	// Write and commit a file
	file1 := filepath.Join(tempDir, "file1.txt")
	if err := os.WriteFile(file1, []byte("hello"), 0644); err != nil {
		t.Fatalf("failed to write file: %v", err)
	}
	_ = exec.Command("git", "-C", tempDir, "add", ".").Run()
	_ = exec.Command("git", "-C", tempDir, "commit", "-m", "initial").Run()

	// Write an uncommitted file
	file2 := filepath.Join(tempDir, "file2.txt")
	if err := os.WriteFile(file2, []byte("world"), 0644); err != nil {
		t.Fatalf("failed to write uncommitted file: %v", err)
	}

	// Capture git context
	gctx, err := Capture(tempDir)
	if err != nil {
		t.Fatalf("Capture failed: %v", err)
	}
	if gctx == nil {
		t.Fatalf("expected git context to be non-nil")
	}

	if gctx.Branch == "" {
		t.Errorf("expected branch name to be populated")
	}
	if gctx.SHA == "" {
		t.Errorf("expected SHA to be populated")
	}
	if len(gctx.DirtyFiles) != 1 || gctx.DirtyFiles[0] != "file2.txt" {
		t.Errorf("expected dirty files to contain file2.txt, got %v", gctx.DirtyFiles)
	}
}
