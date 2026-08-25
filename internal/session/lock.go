package session

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

// LockInfo contains metadata written to .b1codes/session.lock.
type LockInfo struct {
	SessionID string    `json:"session_id"`
	PID       int       `json:"pid"`
	CreatedAt time.Time `json:"created_at"`
}

// WriteLock writes session lock metadata (session ID, PID, timestamp) to .b1codes/session.lock.
func WriteLock(projectPath, sessionID string) error {
	dir := filepath.Join(projectPath, ".b1codes")
	if err := os.MkdirAll(dir, 0755); err != nil {
		return err
	}
	gitignoreFile := filepath.Join(dir, ".gitignore")
	if _, err := os.Stat(gitignoreFile); os.IsNotExist(err) {
		_ = os.WriteFile(gitignoreFile, []byte("*\n"), 0644)
	}

	info := LockInfo{
		SessionID: sessionID,
		PID:       os.Getpid(),
		CreatedAt: time.Now().UTC(),
	}

	data, err := json.Marshal(info)
	if err != nil {
		return err
	}

	lockFile := filepath.Join(dir, "session.lock")
	return os.WriteFile(lockFile, data, 0644)
}

// ReadLockInfo reads and parses the session lock file.
// It returns nil if the lock file does not exist.
// Supports both modern JSON formatted lock files and legacy plain-string UUID lock files.
func ReadLockInfo(projectPath string) (*LockInfo, error) {
	lockFile := filepath.Join(projectPath, ".b1codes", "session.lock")
	data, err := os.ReadFile(lockFile)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}

	trimmed := strings.TrimSpace(string(data))
	if trimmed == "" {
		return nil, nil
	}

	var info LockInfo
	if err := json.Unmarshal(data, &info); err == nil && info.SessionID != "" {
		return &info, nil
	}

	// Backwards compatibility fallback for plain UUID string lock files
	return &LockInfo{
		SessionID: trimmed,
	}, nil
}

// ReadLock reads the session ID from the lock file.
func ReadLock(projectPath string) (string, error) {
	info, err := ReadLockInfo(projectPath)
	if err != nil {
		return "", err
	}
	if info == nil {
		return "", nil
	}
	return info.SessionID, nil
}

// RemoveLock removes .b1codes/session.lock from the project path.
func RemoveLock(projectPath string) error {
	lockFile := filepath.Join(projectPath, ".b1codes", "session.lock")
	err := os.Remove(lockFile)
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

// IsProcessAlive checks whether a process with the given PID is currently alive.
func IsProcessAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	process, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	err = process.Signal(syscall.Signal(0))
	if err == nil {
		return true
	}
	if errors.Is(err, syscall.ESRCH) {
		return false
	}
	if errors.Is(err, syscall.EPERM) {
		return true
	}
	return false
}

