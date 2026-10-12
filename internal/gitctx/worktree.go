package gitctx

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"strings"
	"time"
)

// EmptyRoot is WorkTreeHash's hash for a root with no files git would track.
// It is a fixed value, so an empty root still fingerprints consistently.
const EmptyRoot = "empty"

// WorkTreeHash returns a content hash of the working tree under root
// (relative to projectPath): the id of the git tree it would be if committed
// as it is now, tracked and untracked files included, ignored ones left out.
// The same content gives the same hash whatever was committed, so it says
// whether anything a command could see changed since the hash was taken.
//
// It stages into a temporary copy of the index, so the repository's index is
// never touched; like `git stash create`, it does write the new files' blobs
// to the object store.
func WorkTreeHash(projectPath, root string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := checkRepo(ctx, projectPath); err != nil {
		return "", err
	}

	index, err := runGitCmd(ctx, projectPath, "rev-parse", "--git-path", "index")
	if err != nil {
		return "", gitErr(ctx, "git rev-parse --git-path index", err)
	}
	if !filepath.IsAbs(index) {
		index = filepath.Join(projectPath, index)
	}
	tmpDir, err := os.MkdirTemp("", "taskflow-index-*")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(tmpDir)
	tmpIndex := filepath.Join(tmpDir, "index")
	// Starting from the real index keeps its stat cache, so only changed
	// files are hashed. A repo with nothing staged yet has none.
	if err := copyFile(index, tmpIndex); err != nil && !errors.Is(err, os.ErrNotExist) {
		return "", fmt.Errorf("failed to copy the git index: %w", err)
	}

	env := append(os.Environ(), "GIT_INDEX_FILE="+tmpIndex, "GIT_OPTIONAL_LOCKS=0")
	if _, err := runGitEnv(ctx, projectPath, env, "add", "--all", "--", "."); err != nil {
		return "", gitErr(ctx, "git add (temporary index)", err)
	}
	tree, err := runGitEnv(ctx, projectPath, env, "write-tree")
	if err != nil {
		return "", gitErr(ctx, "git write-tree (temporary index)", err)
	}
	rel := path.Clean(filepath.ToSlash(root))
	if rel == "." {
		rel = ""
	}
	// "<tree>:./<path>" is relative to the project, which may be a
	// subdirectory of the repository.
	sub, err := runGitCmd(ctx, projectPath, "rev-parse", "--verify", "--quiet", tree+":./"+rel)
	if err != nil {
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return "", gitErr(ctx, "git rev-parse", err)
		}
		// Nothing under root that git would track.
		return EmptyRoot, nil
	}
	return sub, nil
}

// runGitEnv is runGitCmd with an environment.
func runGitEnv(ctx context.Context, dir string, env []string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", append([]string{"-C", dir}, args...)...)
	cmd.Env = env
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		if msg := strings.TrimSpace(stderr.String()); msg != "" {
			return "", fmt.Errorf("%w: %s", err, msg)
		}
		return "", err
	}
	return strings.TrimSpace(stdout.String()), nil
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}
