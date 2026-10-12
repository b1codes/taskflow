package gitctx

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/b1codes/taskflow/internal/session"
)

// worktreeRepo makes a repository with the project in a subdirectory.
func worktreeRepo(t *testing.T) (repo, proj string, git func(args ...string) string, write func(rel, content string), hash func() string) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	repo = t.TempDir()
	proj = filepath.Join(repo, "proj")
	git = func(args ...string) string {
		t.Helper()
		out, err := exec.Command("git", append([]string{"-C", repo}, args...)...).CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
		return string(out)
	}
	write = func(rel, content string) {
		t.Helper()
		p := filepath.Join(repo, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	hash = func() string {
		t.Helper()
		h, err := WorkTreeHash(proj)
		if err != nil || h == "" {
			t.Fatalf("WorkTreeHash: %q, %v", h, err)
		}
		return h
	}
	git("init", "-q", "-b", "main")
	git("config", "user.name", "Test")
	git("config", "user.email", "test@test.com")
	git("config", "commit.gpgsign", "false")
	write(".gitignore", "node_modules/\n.env\n")
	write("proj/web/src/a.ts", "a")
	write("proj/api/main.py", "x")
	write("shared/lib.ts", "s")
	git("add", "-A")
	git("commit", "-q", "-m", "init")
	return
}

func TestWorkTreeHash(t *testing.T) {
	t.Parallel()
	_, _, git, write, hash := worktreeRepo(t)
	base := hash()
	if hash() != base {
		t.Fatal("the hash should be stable")
	}

	steps := []struct {
		name   string
		change func()
	}{
		{"an edit in the project", func() { write("proj/web/src/a.ts", "edited") }},
		{"an untracked file", func() { write("proj/web/src/new.ts", "new") }},
		{"an edit outside the project, in shared code", func() { write("shared/lib.ts", "s2") }},
		{"an ignored file", func() { write("proj/.env", "SECRET=a") }},
		{"an ignored file rewritten", func() { write("proj/.env", "SECRET=bb") }},
		{"an ignored directory gaining a child", func() { write("proj/web/node_modules/dep/index.js", "x") }},
	}
	prev := base
	for _, s := range steps {
		s.change()
		if h := hash(); h == prev {
			t.Errorf("%s should change the hash", s.name)
		} else {
			prev = h
		}
	}

	// The repository's index is untouched: new.ts is still untracked.
	if status := git("status", "--porcelain"); !strings.Contains(status, "?? proj/web/src/new.ts") {
		t.Errorf("status = %q", status)
	}
	// Committing the same content doesn't change it.
	git("add", "-A")
	git("commit", "-q", "-m", "work")
	if hash() != prev {
		t.Error("committing unchanged content shouldn't change the hash")
	}

	if _, err := WorkTreeHash(t.TempDir()); !errors.Is(err, session.ErrNotAGitRepository) {
		t.Errorf("not a repo: err = %v", err)
	}
}

// Index flags and caches that tell git a file is unchanged mustn't hide an
// edit from the hash.
func TestWorkTreeHash_IndexFlags(t *testing.T) {
	t.Parallel()
	_, _, git, write, hash := worktreeRepo(t)
	write("proj/web/src/b.ts", "b")
	git("add", "-A")
	git("commit", "-q", "-m", "b")
	git("update-index", "--assume-unchanged", "proj/web/src/a.ts")
	git("update-index", "--skip-worktree", "proj/web/src/b.ts")
	base := hash()

	write("proj/web/src/a.ts", "edited under assume-unchanged")
	h := hash()
	if h == base {
		t.Error("an assume-unchanged file's edit should change the hash")
	}
	write("proj/web/src/b.ts", "edited under skip-worktree")
	if hash() == h {
		t.Error("a skip-worktree file's edit should change the hash")
	}
	// The flags stay as they were in the real index.
	if out := git("ls-files", "-v", "proj/web/src"); !strings.Contains(out, "h proj/web/src/a.ts") || !strings.Contains(out, "S proj/web/src/b.ts") {
		t.Errorf("real index flags changed: %q", out)
	}
}

func TestWorkTreeHash_Submodule(t *testing.T) {
	t.Parallel()
	repo, _, git, _, hash := worktreeRepo(t)
	sub := t.TempDir()
	for _, args := range [][]string{{"init", "-q", "-b", "main"}, {"config", "user.name", "T"}, {"config", "user.email", "t@t"},
		{"config", "commit.gpgsign", "false"}} {
		if out, err := exec.Command("git", append([]string{"-C", sub}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("%v\n%s", err, out)
		}
	}
	if err := os.WriteFile(filepath.Join(sub, "lib.go"), []byte("package lib"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"add", "-A"}, {"commit", "-q", "-m", "lib"}} {
		if out, err := exec.Command("git", append([]string{"-C", sub}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("%v\n%s", err, out)
		}
	}
	git("-c", "protocol.file.allow=always", "submodule", "add", "-q", sub, "vendor/lib")
	git("commit", "-q", "-m", "submodule")
	base := hash()
	if err := os.WriteFile(filepath.Join(repo, "vendor/lib/lib.go"), []byte("package lib // edited"), 0o644); err != nil {
		t.Fatal(err)
	}
	if hash() == base {
		t.Error("an edit inside a submodule should change the hash")
	}
}

func TestWorkTreeHash_NoCommits(t *testing.T) {
	t.Parallel()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	dir := t.TempDir()
	if out, err := exec.Command("git", "-C", dir, "init", "-q").CombinedOutput(); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("a"), 0o644); err != nil {
		t.Fatal(err)
	}
	h, err := WorkTreeHash(dir)
	if err != nil || h == "" {
		t.Errorf("hash = %q, %v", h, err)
	}
	if _, err := os.Stat(filepath.Join(dir, ".git", "index")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the real index shouldn't be created: %v", err)
	}
}
