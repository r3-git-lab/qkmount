//go:build windows

package mount

import (
	"os"
	"path/filepath"
	"strings"
)

// IsMounted reports whether path looks like an active mount point.  WinFsp
// mounts appear either as a drive letter (X:\) or a reparse-point directory.
func IsMounted(path string) bool {
	if path == "" {
		return false
	}
	fi, err := os.Stat(path)
	if err != nil {
		return false
	}
	if !fi.IsDir() {
		return false
	}
	// A drive root (e.g. "X:\\") is a strong signal.
	clean := filepath.Clean(path)
	if strings.HasSuffix(clean, ":\\") || strings.HasSuffix(clean, ":") {
		return true
	}
	// Directory reparse points are reported via symlink mode.
	return fi.Mode()&os.ModeSymlink != 0
}

// ForceUnmount is a no-op on Windows: terminating the FUSE host process
// detaches the WinFsp volume.
func ForceUnmount(path string) error { return nil }

// DefaultMountRoot returns a sensible base directory for mounts.
func DefaultMountRoot() string {
	home, _ := os.UserHomeDir()
	if home == "" {
		home = `C:\QKMount`
	}
	return filepath.Join(home, "QKMount")
}
