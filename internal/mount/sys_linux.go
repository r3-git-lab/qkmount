//go:build linux

package mount

import (
	"os"
	"os/exec"
	"strconv"
	"strings"
)

// IsMounted reports whether path is currently a mount point.
func IsMounted(path string) bool {
	path = strings.TrimRight(path, "/")
	if path == "" {
		path = "/"
	}
	raw, err := os.ReadFile("/proc/self/mountinfo")
	if err != nil {
		return false
	}
	for _, line := range strings.Split(string(raw), "\n") {
		// mountinfo: id parent major:minor root mountpoint opts ...
		fields := strings.Fields(line)
		if len(fields) < 5 {
			continue
		}
		if unescape(fields[4]) == path {
			return true
		}
	}
	return false
}

func unescape(s string) string {
	// mountinfo escapes space, tab, newline and backslash as \ooo
	repl := strings.NewReplacer(`\040`, " ", `\011`, "\t", `\012`, "\n", `\134`, `\`)
	return repl.Replace(s)
}

// ForceUnmount unmounts path, using FUSE-aware helpers first.
func ForceUnmount(path string) error {
	for _, cmd := range [][]string{
		{"fusermount3", "-u", "-z", path},
		{"fusermount", "-u", "-z", path},
		{"umount", "-l", path},
	} {
		if _, err := exec.LookPath(cmd[0]); err != nil {
			continue
		}
		if err := exec.Command(cmd[0], cmd[1:]...).Run(); err == nil {
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

var _ = strconv.Itoa
