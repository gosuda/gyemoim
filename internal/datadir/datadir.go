// Package datadir resolves and prepares Gyemoim's per-user data directory.
package datadir

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
)

// Path returns the platform-specific Gyemoim data directory.
func Path() (string, error) {
	switch runtime.GOOS {
	case "linux":
		if xdg := os.Getenv("XDG_DATA_HOME"); xdg != "" && filepath.IsAbs(xdg) {
			return filepath.Join(xdg, "gyemoim"), nil
		}
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("find the user home directory: %w", err)
		}
		return filepath.Join(home, ".local", "share", "gyemoim"), nil
	case "darwin":
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("find the user home directory: %w", err)
		}
		return filepath.Join(home, "Library", "Application Support", "Gyemoim"), nil
	default:
		return "", fmt.Errorf("Gyemoim supports Linux and macOS; current platform is %s", runtime.GOOS)
	}
}

// Prepare creates the directory and enforces owner-only permissions.
func Prepare(path string) error {
	if err := os.MkdirAll(path, 0o700); err != nil {
		return fmt.Errorf("create data directory %q: %w", path, err)
	}
	if err := os.Chmod(path, 0o700); err != nil {
		return fmt.Errorf("set data directory permissions on %q: %w", path, err)
	}
	return nil
}
