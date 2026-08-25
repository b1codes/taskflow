package session

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestSessionLock(t *testing.T) {
	tempDir, err := os.MkdirTemp("", "taskflow-lock-test-*")
	if err != nil {
		t.Fatalf("failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tempDir)

	// Read on nonexistent directory/lock
	info, err := ReadLockInfo(tempDir)
	if err != nil {
		t.Fatalf("ReadLockInfo on nonexistent failed: %v", err)
	}
	if info != nil {
		t.Errorf("expected nil LockInfo, got: %+v", info)
	}

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

	// Verify .gitignore was created with *
	gitignoreData, err := os.ReadFile(tempDir + "/.b1codes/.gitignore")
	if err != nil {
		t.Fatalf("expected .b1codes/.gitignore to exist: %v", err)
	}
	if string(gitignoreData) != "*\n" {
		t.Errorf("expected .gitignore content '*\\n', got %q", string(gitignoreData))
	}

	// Read LockInfo
	info, err = ReadLockInfo(tempDir)
	if err != nil {
		t.Fatalf("ReadLockInfo failed: %v", err)
	}
	if info == nil {
		t.Fatal("expected non-nil LockInfo")
	}
	if info.SessionID != "session-123" {
		t.Errorf("expected session-123, got: %s", info.SessionID)
	}
	if info.PID != os.Getpid() {
		t.Errorf("expected PID %d, got: %d", os.Getpid(), info.PID)
	}
	if info.CreatedAt.IsZero() {
		t.Errorf("expected non-zero CreatedAt")
	}

	// Read simple ID
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

func TestSessionLock_LegacyPlainString(t *testing.T) {
	tempDir := t.TempDir()
	dir := filepath.Join(tempDir, ".b1codes")
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatalf("mkdir failed: %v", err)
	}
	lockFile := filepath.Join(dir, "session.lock")
	if err := os.WriteFile(lockFile, []byte("legacy-session-uuid-456\n"), 0644); err != nil {
		t.Fatalf("write legacy lock failed: %v", err)
	}

	info, err := ReadLockInfo(tempDir)
	if err != nil {
		t.Fatalf("ReadLockInfo failed: %v", err)
	}
	if info == nil {
		t.Fatal("expected non-nil LockInfo for legacy lock")
	}
	if info.SessionID != "legacy-session-uuid-456" {
		t.Errorf("expected legacy-session-uuid-456, got %s", info.SessionID)
	}
	if info.PID != 0 {
		t.Errorf("expected PID 0 for legacy lock, got %d", info.PID)
	}

	id, err := ReadLock(tempDir)
	if err != nil {
		t.Fatalf("ReadLock failed: %v", err)
	}
	if id != "legacy-session-uuid-456" {
		t.Errorf("expected legacy-session-uuid-456, got %s", id)
	}
}

func TestIsProcessAlive(t *testing.T) {
	if !IsProcessAlive(os.Getpid()) {
		t.Errorf("expected current process PID %d to be alive", os.Getpid())
	}
	if IsProcessAlive(0) {
		t.Errorf("expected PID 0 to not be alive")
	}
	if IsProcessAlive(-1) {
		t.Errorf("expected PID -1 to not be alive")
	}
	if IsProcessAlive(99999999) {
		t.Errorf("expected non-existent PID 99999999 to not be alive")
	}
}

func TestSessionLock_CustomInfoMarshal(t *testing.T) {
	tempDir := t.TempDir()
	dir := filepath.Join(tempDir, ".b1codes")
	_ = os.MkdirAll(dir, 0755)

	custom := LockInfo{
		SessionID: "custom-id",
		PID:       12345,
		CreatedAt: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
	}
	data, _ := json.Marshal(custom)
	_ = os.WriteFile(filepath.Join(dir, "session.lock"), data, 0644)

	info, err := ReadLockInfo(tempDir)
	if err != nil {
		t.Fatalf("ReadLockInfo failed: %v", err)
	}
	if info.SessionID != "custom-id" || info.PID != 12345 {
		t.Errorf("unexpected LockInfo: %+v", info)
	}
}
