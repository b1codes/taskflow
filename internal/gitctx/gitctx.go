package gitctx

import (
	"bytes"
	"context"
	"os/exec"
	"strings"
	"time"

	"github.com/b1codes/taskflow/internal/session"
)

func Capture(projectPath string) (*session.GitContext, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	_, err := runGitCmd(ctx, projectPath, "rev-parse", "--is-inside-work-tree")
	if err != nil {
		return nil, nil
	}

	branch, err := runGitCmd(ctx, projectPath, "rev-parse", "--abbrev-ref", "HEAD")
	if err != nil {
		return nil, nil
	}

	sha, err := runGitCmd(ctx, projectPath, "rev-parse", "--short", "HEAD")
	if err != nil {
		return nil, nil
	}

	statusOutput, err := runGitCmd(ctx, projectPath, "status", "--porcelain")
	if err != nil {
		return nil, nil
	}

	var dirtyFiles []string
	if statusOutput != "" {
		lines := strings.Split(statusOutput, "\n")
		for _, line := range lines {
			if len(line) > 3 {
				path := line[3:]
				path = strings.Trim(path, "\"")
				dirtyFiles = append(dirtyFiles, path)
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
	cmd := exec.CommandContext(ctx, "git", append([]string{"-C", dir}, args...)...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return "", err
	}
	return strings.TrimSpace(stdout.String()), nil
}
