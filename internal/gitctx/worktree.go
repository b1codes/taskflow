package gitctx

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// maxSubmoduleDepth bounds how deep WorkTreeHash follows nested submodules.
const maxSubmoduleDepth = 4

// WorkTreeHash fingerprints the whole working tree of the repository that
// holds projectPath, for telling whether anything a command could see
// changed since it ran. It changes whenever:
//   - a tracked or untracked file changes anywhere in the repository (not
//     just under a stack root: tests read shared code), whatever is
//     committed: it hashes the git tree the content would commit as;
//   - an ignored file or top-level ignored directory (.env, a build output)
//     is added, removed, or rewritten, by path, size, and mtime;
//   - an initialized submodule's working tree changes (recursively).
//
// Files deep inside ignored directories (node_modules/…) are seen only
// through their directory's entry, which changes when a direct child is
// added or removed.
//
// It stages into a temporary copy of the index, with assume-unchanged and
// skip-worktree flags cleared and fsmonitor and the untracked cache off, so
// no change is hidden from it and the repository's own index is never
// touched; like `git stash create`, it does write new files' blobs to the
// object store. Any failure is an error, never a hash.
func WorkTreeHash(projectPath string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	if err := checkRepo(ctx, projectPath); err != nil {
		return "", err
	}
	return workTreeHash(ctx, projectPath, 0)
}

func workTreeHash(ctx context.Context, dir string, depth int) (string, error) {
	top, err := runGitCmd(ctx, dir, "rev-parse", "--show-toplevel")
	if err != nil {
		return "", gitErr(ctx, "git rev-parse --show-toplevel", err)
	}
	index, err := runGitCmd(ctx, top, "rev-parse", "--git-path", "index")
	if err != nil {
		return "", gitErr(ctx, "git rev-parse --git-path index", err)
	}
	if !filepath.IsAbs(index) {
		index = filepath.Join(top, index)
	}
	tmpDir, err := os.MkdirTemp("", "taskflow-index-*")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(tmpDir)
	tmpIndex := filepath.Join(tmpDir, "index")
	// Starting from the real index keeps its stat cache, so only changed
	// files are hashed. Its mtime is kept too: git rehashes entries as new as
	// the index ("racy git"), and a fresh mtime would make it trust them.
	// (Not covered by a test: a same-second rewrite with an unchanged ctime
	// can't be staged reliably.)
	if err := copyFile(index, tmpIndex); err != nil && !errors.Is(err, os.ErrNotExist) {
		return "", fmt.Errorf("failed to copy the git index: %w", err)
	}

	env := append(os.Environ(), "GIT_INDEX_FILE="+tmpIndex, "GIT_OPTIONAL_LOCKS=0")
	// No cache may vouch for a file being unchanged.
	noCache := []string{"-c", "core.fsmonitor=false", "-c", "core.untrackedCache=false", "-c", "core.ignoreStat=false"}
	git := func(stdin []byte, args ...string) (string, error) {
		return runGitEnv(ctx, top, env, stdin, append(append([]string(nil), noCache...), args...)...)
	}
	if err := clearIndexFlags(git); err != nil {
		return "", gitErr(ctx, "clearing index flags (temporary index)", err)
	}
	if _, err := git(nil, "add", "--all", "--", ":/"); err != nil {
		return "", gitErr(ctx, "git add (temporary index)", err)
	}
	tree, err := git(nil, "write-tree")
	if err != nil {
		return "", gitErr(ctx, "git write-tree (temporary index)", err)
	}

	sum := sha256.New()
	fmt.Fprintf(sum, "tree %s\n", tree)
	if err := hashIgnored(sum, git, top); err != nil {
		return "", gitErr(ctx, "listing ignored files", err)
	}
	if depth < maxSubmoduleDepth {
		stage, err := git(nil, "ls-files", "--stage", "-z", "--", ":/")
		if err != nil {
			return "", gitErr(ctx, "git ls-files --stage", err)
		}
		for _, entry := range strings.Split(stage, "\x00") {
			// "<mode> <object> <stage>\t<path>"; gitlinks are mode 160000.
			meta, p, ok := strings.Cut(entry, "\t")
			if !ok || !strings.HasPrefix(meta, "160000 ") {
				continue
			}
			subDir := filepath.Join(top, filepath.FromSlash(p))
			if _, err := os.Stat(filepath.Join(subDir, ".git")); err != nil {
				continue // not initialized: the gitlink in the tree is all there is
			}
			h, err := workTreeHash(ctx, subDir, depth+1)
			if err != nil {
				return "", fmt.Errorf("submodule %s: %w", p, err)
			}
			fmt.Fprintf(sum, "submodule %q %s\n", p, h)
		}
	}
	return hex.EncodeToString(sum.Sum(nil)), nil
}

// clearIndexFlags clears the assume-unchanged and skip-worktree flags in
// the temporary index: git add skips changes to files that carry them. One
// update-index call per flag, since git applies only one of the two when
// given both.
func clearIndexFlags(git func(stdin []byte, args ...string) (string, error)) error {
	out, err := git(nil, "ls-files", "-v", "-z", "--", ":/")
	if err != nil {
		return err
	}
	var assumed, skipped bytes.Buffer
	for _, entry := range strings.Split(out, "\x00") {
		tag, p, ok := strings.Cut(entry, " ")
		if !ok || tag == "" {
			continue
		}
		// A lowercase tag is assume-unchanged; S (or s) is skip-worktree.
		if c := tag[0]; c >= 'a' && c <= 'z' {
			assumed.WriteString(p + "\x00")
		}
		if tag == "S" || tag == "s" {
			skipped.WriteString(p + "\x00")
		}
	}
	for flag, paths := range map[string]*bytes.Buffer{"--no-assume-unchanged": &assumed, "--no-skip-worktree": &skipped} {
		if paths.Len() == 0 {
			continue
		}
		// ls-files paths are relative to the top level, where git runs.
		if _, err := git(paths.Bytes(), "update-index", flag, "-z", "--stdin"); err != nil {
			return err
		}
	}
	return nil
}

// hashIgnored writes every ignored file and top-level ignored directory to
// sum by path, size, and mtime, in path order.
func hashIgnored(sum io.Writer, git func(stdin []byte, args ...string) (string, error), top string) error {
	out, err := git(nil, "ls-files", "-z", "--others", "--ignored", "--exclude-standard", "--directory", "--", ":/")
	if err != nil {
		return err
	}
	paths := strings.Split(out, "\x00")
	sort.Strings(paths)
	for _, p := range paths {
		if p == "" {
			continue
		}
		fi, err := os.Lstat(filepath.Join(top, filepath.FromSlash(strings.TrimSuffix(p, "/"))))
		if err != nil {
			// Gone between listing and stat: as good as changed.
			fmt.Fprintf(sum, "ignored %q missing\n", p)
			continue
		}
		fmt.Fprintf(sum, "ignored %q %d %d %s\n", p, fi.Size(), fi.ModTime().UnixNano(), fi.Mode())
	}
	return nil
}

// runGitEnv is runGitCmd with an environment and optional stdin.
func runGitEnv(ctx context.Context, dir string, env []string, stdin []byte, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", append([]string{"-C", dir}, args...)...)
	cmd.Env = env
	if stdin != nil {
		cmd.Stdin = bytes.NewReader(stdin)
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		if msg := strings.TrimSpace(stderr.String()); msg != "" {
			return "", fmt.Errorf("%w: %s", err, msg)
		}
		return "", err
	}
	return strings.TrimRight(stdout.String(), "\n"), nil
}

// copyFile copies src to dst, keeping src's modification time.
func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	fi, err := in.Stat()
	if err != nil {
		return err
	}
	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	if err := out.Close(); err != nil {
		return err
	}
	return os.Chtimes(dst, fi.ModTime(), fi.ModTime())
}
