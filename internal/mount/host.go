// Package mount serves a backend as a real mounted filesystem and supervises
// the lifetime of those mounts.
package mount

import (
	"fmt"
	"strings"

	"github.com/winfsp/cgofuse/fuse"

	"qkmount/internal/backend"
	"qkmount/internal/vfs"
)

// Serve blocks, serving the backend at conn.MountPoint until it is unmounted.
// The caller must have already opened the backend with Init.
func Serve(be backend.Backend, name, mountpoint string, readOnly bool) error {
	fs := vfs.New(be, name, readOnly)
	host := fuse.NewFileSystemHost(fs)
	host.SetCapReaddirPlus(true)
	opts := MountOptions(name, readOnly)
	if !host.Mount(mountpoint, opts) {
		return fmt.Errorf("failed to mount %s at %s", name, mountpoint)
	}
	return nil
}

// MountOptions builds the cgofuse/FUSE option list for a connection.
func MountOptions(name string, readOnly bool) []string {
	fsname := "QKMount:" + strings.NewReplacer(" ", "_", ",", "_", ":", "_").Replace(name)
	opts := []string{"fsname=" + fsname, "subtype=qkmount"}
	if readOnly {
		opts = append(opts, "ro")
	}
	return []string{"-o", strings.Join(opts, ",")}
}
