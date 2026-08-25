package gitctx

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"time"

	"github.com/b1codes/taskflow/internal/session"
)

var (
	ErrNotAGitRepository = session.ErrNotAGitRepository
	ErrGitNotFound       = errors.New("git binary not found in PATH")
)

func Capture(projectPath string) (*session.GitContext, error) {
	if _, err := exec.LookPath("git"); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrGitNotFound, err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	out, err := runGitCmd(ctx, projectPath, "rev-parse", "--is-inside-work-tree")
	if err != nil {
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return nil, fmt.Errorf("git command timed out: %w", ctx.Err())
		}
		return nil, ErrNotAGitRepository
	}
	if out != "true" {
		return nil, ErrNotAGitRepository
	}

	branch, err := runGitCmd(ctx, projectPath, "symbolic-ref", "--short", "HEAD")
	if err != nil {
		branch, err = runGitCmd(ctx, projectPath, "rev-parse", "--abbrev-ref", "HEAD")
		if err != nil {
			if errors.Is(ctx.Err(), context.DeadlineExceeded) {
				return nil, fmt.Errorf("git command timed out: %w", ctx.Err())
			}
			branch = "HEAD"
		}
	}

	sha, err := runGitCmd(ctx, projectPath, "rev-parse", "--short", "HEAD")
	if err != nil {
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return nil, fmt.Errorf("git command timed out: %w", ctx.Err())
		}
		// In a fresh repo with no commits yet, HEAD has no SHA
		sha = ""
	}

	statusOutput, err := runGitCmdRaw(ctx, projectPath, "status", "--porcelain=v1", "-z")
	if err != nil {
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return nil, fmt.Errorf("git command timed out: %w", ctx.Err())
		}
		return nil, fmt.Errorf("git status failed: %w", err)
	}

	var dirtyFiles []string
	if len(statusOutput) > 0 {
		entries := bytes.Split(statusOutput, []byte{0})
		for i := 0; i < len(entries); i++ {
			entry := entries[i]
			if len(entry) < 3 {
				continue
			}
			status := string(entry[:2])
			path := string(entry[3:])
			if strings.HasPrefix(path, ".b1codes") || strings.HasPrefix(path, ".b1codes/") {
				continue
			}
			dirtyFiles = append(dirtyFiles, path)
			// For renames or copies (status starts with 'R' or 'C'), the next entry in -z output is the original path
			if len(status) > 0 && (status[0] == 'R' || status[0] == 'C') && i+1 < len(entries) {
				i++
			}
			if len(dirtyFiles) >= 500 {
				break
			}
		}
	}

	return &session.GitContext{
		Branch:     branch,
		SHA:        sha,
		DirtyFiles: dirtyFiles,
	}, nil
}

func runGitCmd(ctx context.Context, dir string, args ...string) (string, error) {
	b, err := runGitCmdRaw(ctx, dir, args...)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(b)), nil
}

func runGitCmdRaw(ctx context.Context, dir string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "git", append([]string{"-C", dir}, args...)...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		errMsg := strings.TrimSpace(stderr.String())
		if errMsg != "" {
			return nil, fmt.Errorf("%w: %s", err, errMsg)
		}
		return nil, err
	}
	return stdout.Bytes(), nil
}

