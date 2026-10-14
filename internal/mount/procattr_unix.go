//go:build !windows

package mount

import "syscall"

// sysProcAttr puts each mount in its own session so it keeps running if the
// UI/daemon is restarted, and so it is not killed by the controlling terminal.
func sysProcAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{Setsid: true}
}
