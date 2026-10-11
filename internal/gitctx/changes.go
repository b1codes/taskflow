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
// When baseSHA is empty or unreachable (rebased away, branch reset), the
// merge-base of HEAD and the default branch is used instead; if that fails
// too, only uncommitted changes are read and the set is marked Partial.
func ChangedSince(projectPath, baseSHA string) (*ChangeSet, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := checkRepo(ctx, projectPath); err != nil {
		return nil, err
	}

	cs := &ChangeSet{}
	if baseSHA != "" && objectExists(ctx, projectPath, baseSHA) {
		cs.Base = baseSHA
	} else {
		why := "no base commit was recorded for this session"
		if baseSHA != "" {
			why = fmt.Sprintf("base commit %s is unreachable", short(baseSHA))
		}
		branch := defaultBranch(projectPath)
		if mb, err := runGitCmd(ctx, projectPath, "merge-base", "HEAD", branch); err == nil && mb != "" {
			cs.Base = mb
			cs.Warnings = append(cs.Warnings, fmt.Sprintf("%s; comparing against the merge-base with %s (%s)", why, branch, short(mb)))
		} else {
			cs.Base = "HEAD"
			if !objectExists(ctx, projectPath, "HEAD") {
				cs.Base = EmptyTreeSHA
			}
			cs.Partial = true
			cs.Warnings = append(cs.Warnings, fmt.Sprintf("%s and there is no merge-base with %s; only uncommitted changes are checked", why, branch))
		}
	}

	diff, err := runGitCmdRaw(ctx, projectPath, "diff", "--name-status", "-z", "-M", "--relative", cs.Base)
	if err != nil {
		return nil, gitErr(ctx, "git diff", err)
	}
	cs.Files = parseNameStatus(diff)

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
