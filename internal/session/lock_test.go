package session

import (
	"os"
	"testing"
)

func TestSessionLock(t *testing.T) {
	tempDir, err := os.MkdirTemp("", "taskflow-lock-test-*")
	if err != nil {
		t.Fatalf("failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tempDir)

	// Read on nonexistent directory/lock
	id, err := ReadLock(tempDir)
	if err != nil {
		t.Fatalf("ReadLock on nonexistent failed: %v", err)
	}
	if id != "" {
		t.Errorf("expected empty session ID, got: %s", id)
	}

	// Write
	err = WriteLock(tempDir, "session-123")
	if err != nil {
		t.Fatalf("WriteLock failed: %v", err)
	}

	// Read again
	id, err = ReadLock(tempDir)
	if err != nil {
		t.Fatalf("ReadLock failed: %v", err)
	}
	if id != "session-123" {
		t.Errorf("expected session-123, got: %s", id)
	}

	// Remove
	err = RemoveLock(tempDir)
	if err != nil {
		t.Fatalf("RemoveLock failed: %v", err)
	}

	// Read after remove
	id, err = ReadLock(tempDir)
	if err != nil {
		t.Fatalf("ReadLock after remove failed: %v", err)
	}
	if id != "" {
		t.Errorf("expected empty session ID, got: %s", id)
	}
}
