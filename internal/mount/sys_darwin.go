//go:build darwin

package mount

import (
	"os"
	"os/exec"
	"strings"
)

// IsMounted reports whether path is currently a mount point.
func IsMounted(path string) bool {
	out, err := exec.Command("/sbin/mount").Output()
	if err != nil {
		return false
	}
	needle := " on " + strings.TrimRight(path, "/") + " "
	return strings.Contains(string(out), needle)
}

// ForceUnmount unmounts path using macFUSE/diskutil.
func ForceUnmount(path string) error {
	if _, err := exec.LookPath("umount"); err == nil {
		if err := exec.Command("umount", "-f", path).Run(); err == nil {
			return nil
		}
	}
	return nil
}

// DefaultMountRoot returns a sensible base directory for mounts.
func DefaultMountRoot() string {
	home, err := os.UserHomeDir()
	if err != nil {
		home = os.TempDir()
	}
	return home + "/QKMount"
}
