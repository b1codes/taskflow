package adapters

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/b1codes/taskflow/internal/clickup"
	"github.com/b1codes/taskflow/internal/session"
)

func TestGitCapturer_Capture(t *testing.T) {
	t.Parallel()
	g := NewGitCapturer()

	tempDir := t.TempDir()

	// 1. Non-git dir -> returns nil, ErrNotAGitRepository
	ctx, err := g.Capture(tempDir)
	if err == nil || !errors.Is(err, session.ErrNotAGitRepository) {
		t.Fatalf("expected ErrNotAGitRepository on non-git dir, got err: %v", err)
	}
	if ctx != nil {
		t.Errorf("expected nil context for non-git dir, got %+v", ctx)
	}

	// 2. Git repo with a commit
	if err := exec.Command("git", "-C", tempDir, "init").Run(); err != nil {
		t.Skip("skipping git test: git init failed")
	}
	_ = exec.Command("git", "-C", tempDir, "config", "user.name", "Test").Run()
	_ = exec.Command("git", "-C", tempDir, "config", "user.email", "test@test.com").Run()
	_ = exec.Command("git", "-C", tempDir, "config", "commit.gpgsign", "false").Run()

	file1 := filepath.Join(tempDir, "file1.txt")
	_ = os.WriteFile(file1, []byte("hello"), 0644)
	_ = exec.Command("git", "-C", tempDir, "add", ".").Run()
	_ = exec.Command("git", "-C", tempDir, "commit", "-m", "init").Run()

	ctx, err = g.Capture(tempDir)
	if err != nil {
		t.Fatalf("Capture failed: %v", err)
	}
	if ctx == nil {
		t.Fatal("expected non-nil GitContext")
	}
	if ctx.SHA == "" {
		t.Errorf("expected non-empty SHA")
	}
}

func TestClickUpAdapter(t *testing.T) {
	t.Parallel()

	if NewClickUpAdapter(nil) != nil {
		t.Errorf("expected NewClickUpAdapter(nil) to return nil")
	}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/task/task-adapter-1" {
			_, _ = w.Write([]byte(`{
				"id": "task-adapter-1",
				"name": "Adapter Task",
				"text_content": "Test Description",
				"status": {"status": "in progress", "type": "active"},
				"list": {"id": "list-adapter-1", "name": "Adapter List"}
			}`))
			return
		}
		if r.URL.Path == "/list/list-adapter-1" {
			_, _ = w.Write([]byte(`{
				"id": "list-adapter-1",
				"name": "Adapter List",
				"statuses": [
					{"status": "Open", "type": "open", "orderindex": 0},
					{"status": "Closed", "type": "closed", "orderindex": 1}
				]
			}`))
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer server.Close()

	client := clickup.New("test-key").WithBaseURL(server.URL)
	adapter := NewClickUpAdapter(client)

	ctx := context.Background()

	// Test GetTask
	taskInfo, err := adapter.GetTask(ctx, "task-adapter-1")
	if err != nil {
		t.Fatalf("GetTask failed: %v", err)
	}
	if taskInfo.ID != "task-adapter-1" {
		t.Errorf("expected ID task-adapter-1, got %s", taskInfo.ID)
	}
	if taskInfo.Name != "Adapter Task" {
		t.Errorf("expected Name 'Adapter Task', got %s", taskInfo.Name)
	}
	if taskInfo.Description != "Test Description" {
		t.Errorf("expected Description 'Test Description', got %s", taskInfo.Description)
	}
	if taskInfo.ListID != "list-adapter-1" {
		t.Errorf("expected ListID 'list-adapter-1', got %s", taskInfo.ListID)
	}

	// Test GetListStatuses
	statuses, err := adapter.GetListStatuses(ctx, "list-adapter-1")
	if err != nil {
		t.Fatalf("GetListStatuses failed: %v", err)
	}
	if len(statuses) != 2 {
		t.Fatalf("expected 2 statuses, got %d", len(statuses))
	}
	if statuses[0].Status != "Open" || statuses[0].Type != "open" {
		t.Errorf("status 0 mismatch: %+v", statuses[0])
	}
	if statuses[1].Status != "Closed" || statuses[1].Type != "closed" {
		t.Errorf("status 1 mismatch: %+v", statuses[1])
	}

	// Test error path
	_, err = adapter.GetTask(ctx, "nonexistent-task")
	if err == nil {
		t.Errorf("expected error for nonexistent task, got nil")
	}

	_, err = adapter.GetListStatuses(ctx, "nonexistent-list")
	if err == nil {
		t.Errorf("expected error for nonexistent list, got nil")
	}
}

// On the default branch with no recorded base, only uncommitted changes are
// readable; a stored obligation whose trigger was already committed must
// still be re-verified.
func TestCompletenessEvaluator_PartialRechecksStored(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	git := func(args ...string) {
		t.Helper()
		if out, err := exec.Command("git", append([]string{"-C", dir}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	write := func(rel, content string) {
		t.Helper()
		p := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	git("init", "-q", "-b", "main")
	git("config", "user.name", "Test")
	git("config", "user.email", "test@test.com")
	git("config", "commit.gpgsign", "false")
	write(".agents/completeness.yaml", `
version: 1
rules:
  - id: unit
    when: { added: "src/{name}.ts", exclude: ["**/*.test.ts"] }
    require: [{ id: test, describe: "test for {name}", verify: { exists: "src/{name}.test.ts" } }]
`)
	write("src/Foo.ts", "")
	git("add", "-A")
	git("commit", "-q", "-m", "trigger committed")
	write("src/Foo.test.ts", "") // the work, done after the trigger left the change set

	e := NewCompletenessEvaluator()
	sess := &session.Session{ID: "s1", ProjectPath: dir}
	stored := []*session.Obligation{{ID: "o1", RuleID: "unit", RequirementID: "test", Subject: "src/Foo.ts", Status: session.ObligationOpen}}

	ev, err := e.Evaluate(context.Background(), sess, stored)
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	if !ev.Partial {
		t.Fatalf("expected a partial evaluation on main without a base, warnings: %v", ev.Warnings)
	}
	if len(ev.Findings) != 1 || ev.Findings[0].Subject != "src/Foo.ts" || !ev.Findings[0].Satisfied {
		t.Errorf("findings = %+v, want src/Foo.ts satisfied", ev.Findings)
	}

	ev, err = e.Evaluate(context.Background(), sess, nil)
	if err != nil || len(ev.Findings) != 0 {
		t.Errorf("without stored obligations: findings = %+v, %v", ev.Findings, err)
	}
}
