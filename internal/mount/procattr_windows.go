//go:build windows

package mount

import "syscall"

// sysProcAttr detaches the child from the parent console on Windows.
func sysProcAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{
		HideWindow:    true,
		CreationFlags: 0x00000008 | 0x00000200, // DETACHED_PROCESS | CREATE_NEW_PROCESS_GROUP
	}
}
