package gitctx

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// EmptyTreeSHA is git's empty tree: the base for a repo with no commits.
const EmptyTreeSHA = "4b825dc642cb6eb9a060e54bf8d69288fbee4904"

// maxChangedFiles caps a change set, matching Capture's dirty-file cap.
const maxChangedFiles = 500

type ChangeType string

const (
	ChangeAdded    ChangeType = "A"
	ChangeModified ChangeType = "M"
	ChangeDeleted  ChangeType = "D"
	ChangeRenamed  ChangeType = "R"
)

// ChangedFile is a path relative to the project path given to ChangedSince.
type ChangedFile struct {
	Path    string
	OldPath string // renames only
	Type    ChangeType
}

// ChangeSet is what changed in a project since a base commit.
type ChangeSet struct {
	Files []ChangedFile
	// Base is the commit or tree actually compared against.
	Base string
	// Partial is set when Files may be missing changes: only uncommitted
	// changes could be read, or the cap was hit.
	Partial  bool
	Warnings []string
}

// HeadSHA returns the full SHA of HEAD, or EmptyTreeSHA in a repo with no
// commits yet.
func HeadSHA(projectPath string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := checkRepo(ctx, projectPath); err != nil {
		return "", err
	}
	sha, err := runGitCmd(ctx, projectPath, "rev-parse", "--verify", "--quiet", "HEAD")
	if err != nil || sha == "" {
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return "", fmt.Errorf("git command timed out: %w", ctx.Err())
		}
		return EmptyTreeSHA, nil
	}
	return sha, nil
}

// ChangedSince lists files changed in projectPath since baseSHA: committed
// and uncommitted changes to tracked files (git diff against the working
// tree, with rename detection) plus untracked files, which count as added.
// Paths under .b1codes/ are left out.
//
// When baseSHA is empty or no longer in HEAD's history (rebased away, branch
// reset), the merge-base of HEAD and the default branch is used instead. If
// there is none, or it is HEAD itself (working on the default branch), only
// uncommitted changes are read and the set is marked Partial.
//
// Files that changed only because a merge on HEAD's first-parent line
// brought them in (e.g. main merged into the branch) are left out.
func ChangedSince(projectPath, baseSHA string) (*ChangeSet, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := checkRepo(ctx, projectPath); err != nil {
		return nil, err
	}

	cs := &ChangeSet{}
	head, _ := runGitCmd(ctx, projectPath, "rev-parse", "--verify", "--quiet", "HEAD")
	exists := baseSHA != "" && objectExists(ctx, projectPath, baseSHA)
	switch {
	case baseSHA == EmptyTreeSHA, exists && isAncestor(ctx, projectPath, baseSHA, head):
		cs.Base = baseSHA
	default:
		why := "no base commit was recorded for this session"
		if exists {
			why = fmt.Sprintf("base commit %s is no longer in HEAD's history", short(baseSHA))
		} else if baseSHA != "" {
			why = fmt.Sprintf("base commit %s is unreachable", short(baseSHA))
		}
		branch := defaultBranch(projectPath)
		mb, err := runGitCmd(ctx, projectPath, "merge-base", "HEAD", branch)
		switch {
		case err == nil && mb != "" && mb != head:
			cs.Base = mb
			cs.Warnings = append(cs.Warnings, fmt.Sprintf("%s; comparing against the merge-base with %s (%s)", why, branch, short(mb)))
		default:
			cs.Base = "HEAD"
			if head == "" {
				cs.Base = EmptyTreeSHA
			}
			cs.Partial = true
			if err == nil && mb != "" {
				cs.Warnings = append(cs.Warnings, fmt.Sprintf("%s and HEAD is on %s; only uncommitted changes are checked", why, branch))
			} else {
				cs.Warnings = append(cs.Warnings, fmt.Sprintf("%s and there is no merge-base with %s; only uncommitted changes are checked", why, branch))
			}
		}
	}

	diff, err := runGitCmdRaw(ctx, projectPath, "diff", "--name-status", "-z", "-M", "--relative", cs.Base)
	if err != nil {
		return nil, gitErr(ctx, "git diff", err)
	}
	cs.Files = parseNameStatus(diff)
	if cs.Base == baseSHA && baseSHA != EmptyTreeSHA {
		cs.Files = dropMergedIn(ctx, projectPath, baseSHA, cs)
	}

	untracked, err := runGitCmdRaw(ctx, projectPath, "ls-files", "--others", "--exclude-standard", "-z")
	if err != nil {
		return nil, gitErr(ctx, "git ls-files", err)
	}
	for _, p := range bytes.Split(untracked, []byte{0}) {
		if len(p) > 0 {
			cs.Files = append(cs.Files, ChangedFile{Path: string(p), Type: ChangeAdded})
		}
	}

	kept := cs.Files[:0]
	for _, f := range cs.Files {
		if f.Path == ".b1codes" || strings.HasPrefix(f.Path, ".b1codes/") {
			continue
		}
		kept = append(kept, f)
	}
	cs.Files = kept
	if len(cs.Files) > maxChangedFiles {
		cs.Files = cs.Files[:maxChangedFiles]
		cs.Partial = true
		cs.Warnings = append(cs.Warnings, fmt.Sprintf("more than %d files changed; only the first %d are checked", maxChangedFiles, maxChangedFiles))
	}
	return cs, nil
}

// dropMergedIn removes the files that differ from base only because a merge
// on HEAD's first-parent line brought them in: touched by a merge (compared
// to its first parent) but not by the branch's own commits or uncommitted
// edits. On any git error the files are returned as they are.
func dropMergedIn(ctx context.Context, projectPath, base string, cs *ChangeSet) []ChangedFile {
	rng := base + "..HEAD"
	merged, err := runGitCmdRaw(ctx, projectPath, "log", "--first-parent", "--merges", "--diff-merges=first-parent",
		"--no-renames", "--name-only", "--relative", "-z", "--format=", rng)
	if err != nil || len(bytes.Trim(merged, "\x00\n")) == 0 {
		return cs.Files
	}
	own, err := runGitCmdRaw(ctx, projectPath, "log", "--first-parent", "--no-merges",
		"--no-renames", "--name-only", "--relative", "-z", "--format=", rng)
	if err != nil {
		return cs.Files
	}
	dirty, err := runGitCmdRaw(ctx, projectPath, "diff", "--no-renames", "--name-only", "--relative", "-z", "HEAD")
	if err != nil {
		return cs.Files
	}
	upstream, mine := pathSet(merged), pathSet(own)
	for p := range pathSet(dirty) {
		mine[p] = true
	}
	kept := cs.Files[:0]
	dropped := 0
	for _, f := range cs.Files {
		if upstream[f.Path] && !mine[f.Path] && (f.OldPath == "" || !mine[f.OldPath]) {
			dropped++
			continue
		}
		kept = append(kept, f)
	}
	if dropped > 0 {
		cs.Warnings = append(cs.Warnings, fmt.Sprintf("%d file(s) changed only by merges into this branch are not counted", dropped))
	}
	return kept
}

// pathSet splits NUL-separated `git log --name-only -z --format=` output.
// Each commit's list starts on a new line, so the separators are NUL and
// newline.
func pathSet(out []byte) map[string]bool {
	set := map[string]bool{}
	for _, p := range bytes.FieldsFunc(out, func(r rune) bool { return r == 0 || r == '\n' }) {
		set[string(p)] = true
	}
	return set
}

// parseNameStatus parses `git diff --name-status -z` output: a status token
// followed by one path, or two (old, new) for renames and copies.
func parseNameStatus(out []byte) []ChangedFile {
	toks := bytes.Split(out, []byte{0})
	var files []ChangedFile
	for i := 0; i < len(toks); {
		status := string(toks[i])
		if status == "" {
			i++
			continue
		}
		switch status[0] {
		case 'R', 'C':
			if i+2 >= len(toks) {
				return files
			}
			f := ChangedFile{Path: string(toks[i+2]), Type: ChangeAdded}
			if status[0] == 'R' {
				f.Type, f.OldPath = ChangeRenamed, string(toks[i+1])
			}
			files = append(files, f)
			i += 3
		default:
			if i+1 >= len(toks) {
				return files
			}
			t := ChangeModified // M, T (type change), U (unmerged)
			switch status[0] {
			case 'A':
				t = ChangeAdded
			case 'D':
				t = ChangeDeleted
			}
			files = append(files, ChangedFile{Path: string(toks[i+1]), Type: t})
			i += 2
		}
	}
	return files
}

func checkRepo(ctx context.Context, projectPath string) error {
	if _, err := exec.LookPath("git"); err != nil {
		return fmt.Errorf("%w: %v", ErrGitNotFound, err)
	}
	out, err := runGitCmd(ctx, projectPath, "rev-parse", "--is-inside-work-tree")
	if err != nil {
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return fmt.Errorf("git command timed out: %w", ctx.Err())
		}
		return ErrNotAGitRepository
	}
	if out != "true" {
		return ErrNotAGitRepository
	}
	return nil
}

// objectExists reports whether rev names a commit or tree in the repo.
func objectExists(ctx context.Context, projectPath, rev string) bool {
	_, err := runGitCmd(ctx, projectPath, "rev-parse", "--verify", "--quiet", rev+"^{tree}")
	return err == nil
}

// isAncestor reports whether commit rev is head or one of its ancestors.
func isAncestor(ctx context.Context, projectPath, rev, head string) bool {
	if head == "" {
		return false
	}
	_, err := runGitCmd(ctx, projectPath, "merge-base", "--is-ancestor", rev, head)
	return err == nil
}

// defaultBranch reads default_branch from .agents/config.yaml, else "main".
func defaultBranch(projectPath string) string {
	var cfg struct {
		DefaultBranch string `yaml:"default_branch"`
	}
	if data, err := os.ReadFile(filepath.Join(projectPath, ".agents", "config.yaml")); err == nil {
		_ = yaml.Unmarshal(data, &cfg)
	}
	if cfg.DefaultBranch == "" {
		return "main"
	}
	return cfg.DefaultBranch
}

func gitErr(ctx context.Context, what string, err error) error {
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return fmt.Errorf("git command timed out: %w", ctx.Err())
	}
	return fmt.Errorf("%s failed: %w", what, err)
}

func short(sha string) string {
	if len(sha) > 7 {
		return sha[:7]
	}
	return sha
}
