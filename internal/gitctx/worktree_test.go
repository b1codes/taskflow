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

func TestWorkTreeHash(t *testing.T) {
	t.Parallel()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	repo := t.TempDir()
	proj := filepath.Join(repo, "proj") // a project in a subdirectory of the repo
	git := func(args ...string) string {
		t.Helper()
		out, err := exec.Command("git", append([]string{"-C", repo}, args...)...).CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
		return string(out)
	}
	write := func(rel, content string) {
		t.Helper()
		p := filepath.Join(proj, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	hash := func(root string) string {
		t.Helper()
		h, err := WorkTreeHash(proj, root)
		if err != nil {
			t.Fatalf("WorkTreeHash(%s): %v", root, err)
		}
		return h
	}
	git("init", "-q", "-b", "main")
	git("config", "user.name", "Test")
	git("config", "user.email", "test@test.com")
	git("config", "commit.gpgsign", "false")
	write(".gitignore", "node_modules/\n")
	write("web/src/a.ts", "a")
	write("api/main.py", "x")
	git("add", "-A")
	git("commit", "-q", "-m", "init")

	web, api, all := hash("web"), hash("api"), hash(".")
	if web == api || web == all || hash("web") != web {
		t.Fatalf("hashes web=%s api=%s all=%s", web, api, all)
	}

	write("web/src/a.ts", "edited")
	edited := hash("web")
	if edited == web {
		t.Error("an edit should change the root's hash")
	}
	if hash("api") != api {
		t.Error("an edit under web shouldn't change api's hash")
	}

	write("web/src/new.ts", "new") // untracked
	withNew := hash("web")
	if withNew == edited {
		t.Error("an untracked file should change the hash")
	}
	write("web/node_modules/dep.js", "ignored")
	if hash("web") != withNew {
		t.Error("an ignored file shouldn't change the hash")
	}

	// The repository's index is untouched: new.ts is still untracked.
	if status := git("status", "--porcelain"); !strings.Contains(status, "?? proj/web/src/new.ts") {
		t.Errorf("status = %q", status)
	}

	// Committing the same content doesn't change it; reverting restores it.
	git("add", "-A")
	git("commit", "-q", "-m", "work")
	if hash("web") != withNew {
		t.Error("committing unchanged content shouldn't change the hash")
	}
	write("web/src/a.ts", "a")
	if err := os.Remove(filepath.Join(proj, "web/src/new.ts")); err != nil {
		t.Fatal(err)
	}
	if hash("web") != web {
		t.Error("content back as it was should hash as it was")
	}

	if hash("missing") != EmptyRoot {
		t.Error("a root with nothing in it")
	}
	if _, err := WorkTreeHash(t.TempDir(), "."); !errors.Is(err, session.ErrNotAGitRepository) {
		t.Errorf("not a repo: err = %v", err)
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
	h, err := WorkTreeHash(dir, ".")
	if err != nil || h == "" || h == EmptyRoot {
		t.Errorf("hash = %q, %v", h, err)
	}
	if _, err := os.Stat(filepath.Join(dir, ".git", "index")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the real index shouldn't be created: %v", err)
	}
}
