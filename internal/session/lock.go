package session

import (
	"os"
	"path/filepath"
	"strings"
)

func WriteLock(projectPath, sessionID string) error {
	dir := filepath.Join(projectPath, ".b1codes")
	if err := os.MkdirAll(dir, 0755); err != nil {
		return err
	}
	lockFile := filepath.Join(dir, "session.lock")
	return os.WriteFile(lockFile, []byte(sessionID), 0644)
}

func ReadLock(projectPath string) (string, error) {
	lockFile := filepath.Join(projectPath, ".b1codes", "session.lock")
	data, err := os.ReadFile(lockFile)
	if err != nil {
		if os.IsNotExist(err) {
			return "", nil
		}
		return "", err
	}
	return strings.TrimSpace(string(data)), nil
}

func RemoveLock(projectPath string) error {
	lockFile := filepath.Join(projectPath, ".b1codes", "session.lock")
	err := os.Remove(lockFile)
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}
