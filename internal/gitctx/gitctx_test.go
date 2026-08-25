package gitctx

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/b1codes/taskflow/internal/session"
)

func TestGitContext_Capture(t *testing.T) {
	// Create temp directory
	tempDir, err := os.MkdirTemp("", "taskflow-git-test-*")
	if err != nil {
		t.Fatalf("failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tempDir)

	// 1. Run capture on non-git dir -> should return ErrNotAGitRepository
	ctx, err := Capture(tempDir)
	if err == nil || !errors.Is(err, ErrNotAGitRepository) {
		t.Fatalf("expected ErrNotAGitRepository for non-git dir, got err: %v, ctx: %+v", err, ctx)
	}
	if ctx != nil {
		t.Errorf("expected Capture to return nil context for non-git dir, got: %+v", ctx)
	}

	// 2. Initialize git repo in tempDir
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

func TestGitContext_UncommittedRepo(t *testing.T) {
	tempDir := t.TempDir()

	if err := exec.Command("git", "-C", tempDir, "init").Run(); err != nil {
		t.Skip("skipping git test: git init failed")
	}
	_ = exec.Command("git", "-C", tempDir, "config", "user.name", "Test").Run()
	_ = exec.Command("git", "-C", tempDir, "config", "user.email", "test@test.com").Run()
	_ = exec.Command("git", "-C", tempDir, "config", "commit.gpgsign", "false").Run()

	// Fresh repo with no commits yet, but 1 dirty file
	file1 := filepath.Join(tempDir, "new_file.txt")
	_ = os.WriteFile(file1, []byte("hello uncommitted"), 0644)

	gctx, err := Capture(tempDir)
	if err != nil {
		t.Fatalf("Capture failed on uncommitted repo: %v", err)
	}
	if gctx == nil {
		t.Fatal("expected non-nil git context")
	}
	if gctx.Branch == "" {
		t.Errorf("expected branch name to be detected (e.g. main/master/HEAD), got empty")
	}
	if gctx.SHA != "" {
		t.Errorf("expected empty SHA on uncommitted repo, got %s", gctx.SHA)
	}
	if len(gctx.DirtyFiles) != 1 || gctx.DirtyFiles[0] != "new_file.txt" {
		t.Errorf("expected dirty files to contain new_file.txt, got %v", gctx.DirtyFiles)
	}
}

func TestGitContext_RenamedAndSpecialChars(t *testing.T) {
	tempDir := t.TempDir()

	if err := exec.Command("git", "-C", tempDir, "init").Run(); err != nil {
		t.Skip("skipping git test: git init failed")
	}
	_ = exec.Command("git", "-C", tempDir, "config", "user.name", "Test").Run()
	_ = exec.Command("git", "-C", tempDir, "config", "user.email", "test@test.com").Run()
	_ = exec.Command("git", "-C", tempDir, "config", "commit.gpgsign", "false").Run()

	// Initial commit
	file1 := filepath.Join(tempDir, "file with spaces.txt")
	_ = os.WriteFile(file1, []byte("initial"), 0644)
	_ = exec.Command("git", "-C", tempDir, "add", ".").Run()
	_ = exec.Command("git", "-C", tempDir, "commit", "-m", "init").Run()

	// Rename file
	_ = exec.Command("git", "-C", tempDir, "mv", "file with spaces.txt", "renamed file.txt").Run()

	gctx, err := Capture(tempDir)
	if err != nil {
		t.Fatalf("Capture failed on renamed file: %v", err)
	}
	if gctx == nil {
		t.Fatal("expected non-nil git context")
	}
	if len(gctx.DirtyFiles) != 1 || gctx.DirtyFiles[0] != "renamed file.txt" {
		t.Errorf("expected dirty files to contain 'renamed file.txt', got %v", gctx.DirtyFiles)
	}
}

func TestGitContext_LockFileIgnoredAndNotReportedDirty(t *testing.T) {
	tempDir, err := os.MkdirTemp("", "taskflow-git-lock-test-*")
	if err != nil {
		t.Fatalf("failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tempDir)

	if err := exec.Command("git", "-C", tempDir, "init").Run(); err != nil {
		t.Skip("skipping git test: git command failed to run git init")
	}

	_ = exec.Command("git", "-C", tempDir, "config", "user.name", "Test").Run()
	_ = exec.Command("git", "-C", tempDir, "config", "user.email", "test@test.com").Run()
	_ = exec.Command("git", "-C", tempDir, "config", "commit.gpgsign", "false").Run()

	// Initial clean commit
	file1 := filepath.Join(tempDir, "file1.txt")
	if err := os.WriteFile(file1, []byte("hello"), 0644); err != nil {
		t.Fatalf("failed to write file: %v", err)
	}
	_ = exec.Command("git", "-C", tempDir, "add", ".").Run()
	_ = exec.Command("git", "-C", tempDir, "commit", "-m", "initial").Run()

	// Create session lock (which creates .b1codes/session.lock and .b1codes/.gitignore)
	if err := session.WriteLock(tempDir, "sess-lock-test"); err != nil {
		t.Fatalf("WriteLock failed: %v", err)
	}

	// Verify git status --porcelain is completely clean
	out, err := exec.Command("git", "-C", tempDir, "status", "--porcelain").Output()
	if err != nil {
		t.Fatalf("git status failed: %v", err)
	}
	if len(out) != 0 {
		t.Errorf("expected clean git status after WriteLock, got:\n%s", string(out))
	}

	// Verify Capture returns 0 dirty files
	gctx, err := Capture(tempDir)
	if err != nil {
		t.Fatalf("Capture failed: %v", err)
	}
	if len(gctx.DirtyFiles) != 0 {
		t.Errorf("expected 0 dirty files, got: %v", gctx.DirtyFiles)
	}
}
