package backend

import (
	"io"
	"os"
	"path/filepath"
	"time"
)

func init() {
	Register("local", func(name string, opts map[string]string) (Backend, error) {
		return &localBackend{root: Opt(opts, "root", Opt(opts, "path", "/"))}, nil
	})
}

// localBackend mounts an ordinary directory.  It is used for testing and as
// the "Local folder" connection type.
type localBackend struct {
	root string
}

func (b *localBackend) Kind() string      { return "local" }
func (b *localBackend) DirectWrite() bool { return true }
func (b *localBackend) Init() error {
	if b.root == "" {
		return os.ErrNotExist
	}
	_, err := os.Stat(b.root)
	return err
}
func (b *localBackend) Close() error { return nil }

func (b *localBackend) real(path string) string {
	return filepath.Join(b.root, filepath.FromSlash(Clean(path)))
}

func (b *localBackend) Stat(path string) (*Attr, error) {
	fi, err := os.Lstat(b.real(path))
	if err != nil {
		return nil, err
	}
	return &Attr{
		IsDir: fi.IsDir(),
		Size:  fi.Size(),
		Mode:  uint32(fi.Mode().Perm()),
		Mtime: fi.ModTime(),
		Atime: fi.ModTime(),
		Ctime: fi.ModTime(),
	}, nil
}

func (b *localBackend) ReadDir(path string) ([]DirEntry, error) {
	entries, err := os.ReadDir(b.real(path))
	if err != nil {
		return nil, err
	}
	out := make([]DirEntry, 0, len(entries))
	for _, e := range entries {
		info, ierr := e.Info()
		de := DirEntry{Name: e.Name(), IsDir: e.IsDir()}
		if ierr == nil {
			de.Size = info.Size()
			de.Mtime = info.ModTime()
			de.Mode = uint32(info.Mode().Perm())
		}
		out = append(out, de)
	}
	SortEntries(out)
	return out, nil
}

func (b *localBackend) Mkdir(path string, mode uint32) error {
	return os.Mkdir(b.real(path), os.FileMode(mode&0o777)|0o700)
}
func (b *localBackend) Remove(path string) error    { return os.Remove(b.real(path)) }
func (b *localBackend) RemoveDir(path string) error { return os.Remove(b.real(path)) }
func (b *localBackend) Rename(o, n string) error    { return os.Rename(b.real(o), b.real(n)) }
func (b *localBackend) Truncate(path string, size int64) error {
	return os.Truncate(b.real(path), size)
}
func (b *localBackend) Chmod(path string, mode uint32) error {
	return os.Chmod(b.real(path), os.FileMode(mode&0o777))
}
func (b *localBackend) Chtimes(path string, a, m time.Time) error {
	return os.Chtimes(b.real(path), a, m)
}

func (b *localBackend) OpenRead(path string) (ReadHandle, error) {
	f, err := os.Open(b.real(path))
	if err != nil {
		return nil, err
	}
	fi, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, err
	}
	return &fileReadHandle{f: f, size: fi.Size()}, nil
}

func (b *localBackend) OpenWrite(path string, truncate bool) (WriteHandle, error) {
	flags := os.O_RDWR | os.O_CREATE
	if truncate {
		flags |= os.O_TRUNC
	}
	f, err := os.OpenFile(b.real(path), flags, 0o644)
	if err != nil {
		return nil, err
	}
	return &fileWriteHandle{f: f}, nil
}

func (b *localBackend) StatFS() (uint64, uint64, uint64) {
	// Report a generous fixed size for local folders.
	const tb = 1 << 40
	return tb, tb / 2, tb / 2
}

type fileReadHandle struct {
	f    *os.File
	size int64
}

func (h *fileReadHandle) ReadAt(p []byte, off int64) (int, error) { return h.f.ReadAt(p, off) }
func (h *fileReadHandle) Size() int64                             { return h.size }
func (h *fileReadHandle) Close() error                            { return h.f.Close() }

type fileWriteHandle struct {
	f *os.File
}

func (h *fileWriteHandle) WriteAt(p []byte, off int64) (int, error) { return h.f.WriteAt(p, off) }
func (h *fileWriteHandle) Truncate(size int64) error                { return h.f.Truncate(size) }
func (h *fileWriteHandle) Flush() error                             { return h.f.Sync() }
func (h *fileWriteHandle) Close() error                             { return h.f.Close() }
func (h *fileWriteHandle) Size() int64 {
	fi, err := h.f.Stat()
	if err != nil {
		return 0
	}
	return fi.Size()
}

var _ = io.EOF
