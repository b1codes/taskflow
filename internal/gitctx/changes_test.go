package gitctx

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func newRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if err := exec.Command("git", "-C", dir, "init", "-b", "main").Run(); err != nil {
		t.Skip("skipping git test: git init failed")
	}
	for _, kv := range [][2]string{{"user.name", "Test"}, {"user.email", "test@test.com"}, {"commit.gpgsign", "false"}} {
		git(t, dir, "config", kv[0], kv[1])
	}
	return dir
}

func git(t *testing.T, dir string, args ...string) string {
	t.Helper()
	out, err := exec.Command("git", append([]string{"-C", dir}, args...)...).CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

func write(t *testing.T, dir, rel, content string) {
	t.Helper()
	p := filepath.Join(dir, rel)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func commitAll(t *testing.T, dir, msg string) {
	t.Helper()
	git(t, dir, "add", "-A")
	git(t, dir, "commit", "-q", "-m", msg)
}

func summarize(files []ChangedFile) []string {
	var out []string
	for _, f := range files {
		s := string(f.Type) + " " + f.Path
		if f.OldPath != "" {
			s += " <- " + f.OldPath
		}
		out = append(out, s)
	}
	slices.Sort(out)
	return out
}

func TestChangedSince_CommittedUncommittedUntracked(t *testing.T) {
	dir := newRepo(t)
	write(t, dir, "a.txt", "one")
	write(t, dir, "old.txt", "a file that will be renamed, long enough for rename detection")
	write(t, dir, "gone.txt", "bye")
	commitAll(t, dir, "initial")
	base, err := HeadSHA(dir)
	if err != nil || len(base) != 40 {
		t.Fatalf("HeadSHA = %q, %v", base, err)
	}

	write(t, dir, "b.txt", "committed after base")
	commitAll(t, dir, "second")
	write(t, dir, "a.txt", "uncommitted edit")
	write(t, dir, "untracked/dir/c.txt", "new")
	write(t, dir, ".b1codes/session.lock", "lock")
	git(t, dir, "mv", "old.txt", "new.txt")
	git(t, dir, "rm", "-q", "gone.txt")

	cs, err := ChangedSince(dir, base)
	if err != nil {
		t.Fatalf("ChangedSince: %v", err)
	}
	want := []string{"A b.txt", "A untracked/dir/c.txt", "D gone.txt", "M a.txt", "R new.txt <- old.txt"}
	if got := summarize(cs.Files); !slices.Equal(got, want) {
		t.Errorf("files = %v\nwant    %v", got, want)
	}
	if cs.Base != base || cs.Partial || len(cs.Warnings) != 0 {
		t.Errorf("base=%s partial=%v warnings=%v", cs.Base, cs.Partial, cs.Warnings)
	}
}

func TestChangedSince_EmptyTreeBase(t *testing.T) {
	dir := newRepo(t)
	base, err := HeadSHA(dir)
	if err != nil || base != EmptyTreeSHA {
		t.Fatalf("HeadSHA in a repo with no commits = %q, %v; want the empty tree", base, err)
	}
	write(t, dir, "staged.txt", "x")
	git(t, dir, "add", "staged.txt")
	write(t, dir, "loose.txt", "y")

	cs, err := ChangedSince(dir, base)
	if err != nil {
		t.Fatalf("ChangedSince: %v", err)
	}
	if got, want := summarize(cs.Files), []string{"A loose.txt", "A staged.txt"}; !slices.Equal(got, want) {
		t.Errorf("before first commit: files = %v, want %v", got, want)
	}

	commitAll(t, dir, "first")
	cs, err = ChangedSince(dir, base)
	if err != nil {
		t.Fatalf("ChangedSince: %v", err)
	}
	if got, want := summarize(cs.Files), []string{"A loose.txt", "A staged.txt"}; !slices.Equal(got, want) {
		t.Errorf("after first commit: files = %v, want %v", got, want)
	}
}

func TestChangedSince_UnreachableBase(t *testing.T) {
	const unreachable = "0123456789abcdef0123456789abcdef01234567"

	t.Run("falls back to merge-base with the default branch", func(t *testing.T) {
		dir := newRepo(t)
		write(t, dir, "base.txt", "x")
		commitAll(t, dir, "initial")
		git(t, dir, "checkout", "-q", "-b", "feature")
		write(t, dir, "feature.txt", "y")
		commitAll(t, dir, "feature work")

		cs, err := ChangedSince(dir, unreachable)
		if err != nil {
			t.Fatalf("ChangedSince: %v", err)
		}
		if got, want := summarize(cs.Files), []string{"A feature.txt"}; !slices.Equal(got, want) {
			t.Errorf("files = %v, want %v", got, want)
		}
		if cs.Partial || len(cs.Warnings) != 1 || !strings.Contains(cs.Warnings[0], "unreachable") || !strings.Contains(cs.Warnings[0], "merge-base with main") {
			t.Errorf("partial=%v warnings=%v", cs.Partial, cs.Warnings)
		}
	})

	t.Run("default branch from .agents/config.yaml", func(t *testing.T) {
		dir := newRepo(t)
		git(t, dir, "checkout", "-q", "-b", "develop")
		write(t, dir, ".agents/config.yaml", "default_branch: develop\n")
		commitAll(t, dir, "initial")
		git(t, dir, "checkout", "-q", "-b", "feature")
		write(t, dir, "feature.txt", "y")
		commitAll(t, dir, "feature work")

		cs, err := ChangedSince(dir, "")
		if err != nil {
			t.Fatalf("ChangedSince: %v", err)
		}
		if got, want := summarize(cs.Files), []string{"A feature.txt"}; !slices.Equal(got, want) {
			t.Errorf("files = %v, want %v", got, want)
		}
		if len(cs.Warnings) != 1 || !strings.Contains(cs.Warnings[0], "merge-base with develop") {
			t.Errorf("warnings = %v", cs.Warnings)
		}
	})

	t.Run("no merge-base reads uncommitted changes only", func(t *testing.T) {
		dir := newRepo(t)
		git(t, dir, "checkout", "-q", "-b", "trunk") // no "main" branch exists
		write(t, dir, "committed.txt", "x")
		commitAll(t, dir, "initial")
		write(t, dir, "committed.txt", "edited")
		write(t, dir, "new.txt", "y")

		cs, err := ChangedSince(dir, unreachable)
		if err != nil {
			t.Fatalf("ChangedSince: %v", err)
		}
		if got, want := summarize(cs.Files), []string{"A new.txt", "M committed.txt"}; !slices.Equal(got, want) {
			t.Errorf("files = %v, want %v", got, want)
		}
		if !cs.Partial || len(cs.Warnings) != 1 || !strings.Contains(cs.Warnings[0], "only uncommitted changes") {
			t.Errorf("partial=%v warnings=%v", cs.Partial, cs.Warnings)
		}
	})
}

func TestChangedSince_ProjectSubdirectory(t *testing.T) {
	dir := newRepo(t)
	write(t, dir, "app/main.go", "package main")
	commitAll(t, dir, "initial")
	base, _ := HeadSHA(filepath.Join(dir, "app"))
	write(t, dir, "app/feature.go", "package main")
	write(t, dir, "elsewhere.txt", "outside the project")

	cs, err := ChangedSince(filepath.Join(dir, "app"), base)
	if err != nil {
		t.Fatalf("ChangedSince: %v", err)
	}
	if got, want := summarize(cs.Files), []string{"A feature.go"}; !slices.Equal(got, want) {
		t.Errorf("paths must be relative to the project and stay inside it: %v, want %v", got, want)
	}
}

func TestChangedSince_Cap(t *testing.T) {
	dir := newRepo(t)
	git(t, dir, "commit", "-q", "--allow-empty", "-m", "empty")
	base, _ := HeadSHA(dir)
	for i := 0; i < maxChangedFiles+5; i++ {
		write(t, dir, fmt.Sprintf("gen/f%03d.txt", i), "x")
	}
	cs, err := ChangedSince(dir, base)
	if err != nil {
		t.Fatalf("ChangedSince: %v", err)
	}
	if len(cs.Files) != maxChangedFiles || !cs.Partial || len(cs.Warnings) != 1 {
		t.Errorf("len=%d partial=%v warnings=%v", len(cs.Files), cs.Partial, cs.Warnings)
	}
}

func TestChangedSince_NotARepo(t *testing.T) {
	if _, err := ChangedSince(t.TempDir(), ""); !errors.Is(err, ErrNotAGitRepository) {
		t.Errorf("expected ErrNotAGitRepository, got %v", err)
	}
	if _, err := HeadSHA(t.TempDir()); !errors.Is(err, ErrNotAGitRepository) {
		t.Errorf("HeadSHA: expected ErrNotAGitRepository, got %v", err)
	}
}

func TestChangedSince_OnDefaultBranchWithoutBase(t *testing.T) {
	dir := newRepo(t)
	git(t, dir, "checkout", "-q", "-b", "main")
	write(t, dir, "a.txt", "x")
	commitAll(t, dir, "initial")
	write(t, dir, "b.txt", "y")
	commitAll(t, dir, "session work") // committed, so invisible without a base

	cs, err := ChangedSince(dir, "")
	if err != nil {
		t.Fatalf("ChangedSince: %v", err)
	}
	if !cs.Partial || len(cs.Warnings) != 1 || !strings.Contains(cs.Warnings[0], "HEAD is on main") {
		t.Errorf("partial=%v warnings=%v, want partial: the merge-base is HEAD itself", cs.Partial, cs.Warnings)
	}
}

func TestChangedSince_BaseNotAncestor(t *testing.T) {
	dir := newRepo(t)
	git(t, dir, "checkout", "-q", "-b", "main")
	write(t, dir, "a.txt", "x")
	commitAll(t, dir, "initial")
	git(t, dir, "checkout", "-q", "-b", "feature")
	write(t, dir, "old.txt", "y")
	commitAll(t, dir, "work that gets reset away")
	base := git(t, dir, "rev-parse", "HEAD")
	git(t, dir, "reset", "-q", "--hard", "main")
	write(t, dir, "new.txt", "z")
	commitAll(t, dir, "new work")

	cs, err := ChangedSince(dir, base)
	if err != nil {
		t.Fatalf("ChangedSince: %v", err)
	}
	if got, want := summarize(cs.Files), []string{"A new.txt"}; !slices.Equal(got, want) {
		t.Errorf("files = %v, want %v (diffing from the stale base would also report old.txt deleted)", got, want)
	}
	if len(cs.Warnings) != 1 || !strings.Contains(cs.Warnings[0], "no longer in HEAD's history") {
		t.Errorf("warnings = %v", cs.Warnings)
	}
}

func TestChangedSince_IgnoresMergedInChanges(t *testing.T) {
	dir := newRepo(t)
	git(t, dir, "checkout", "-q", "-b", "main")
	write(t, dir, "shared.txt", "1")
	commitAll(t, dir, "initial")
	git(t, dir, "checkout", "-q", "-b", "feature")
	base := git(t, dir, "rev-parse", "HEAD")
	write(t, dir, "mine.txt", "m")
	commitAll(t, dir, "session work")

	git(t, dir, "checkout", "-q", "main")
	write(t, dir, "teammate.txt", "t")
	write(t, dir, "shared.txt", "2")
	commitAll(t, dir, "upstream work")
	git(t, dir, "checkout", "-q", "feature")
	git(t, dir, "merge", "-q", "--no-edit", "main")
	write(t, dir, "shared.txt", "3") // the session edits a merged-in file too

	cs, err := ChangedSince(dir, base)
	if err != nil {
		t.Fatalf("ChangedSince: %v", err)
	}
	if got, want := summarize(cs.Files), []string{"A mine.txt", "M shared.txt"}; !slices.Equal(got, want) {
		t.Errorf("files = %v, want %v", got, want)
	}
	if cs.Partial || len(cs.Warnings) != 1 || !strings.Contains(cs.Warnings[0], "1 file(s) changed only by merges") {
		t.Errorf("partial=%v warnings=%v", cs.Partial, cs.Warnings)
	}
}
