// Package backend defines the common contract every QKMount connection type
// implements, plus shared helpers for turning remote services into something
// that looks like a POSIX filesystem.
//
// Only a handful of methods are required; the FUSE adapter in package vfs
// builds the rest on top of them.  Writes are staged to a local temporary
// file and uploaded on close, which keeps every backend simple and works for
// services without random-write support (S3, Drive, Dropbox, ...).
package backend

import (
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"time"
)

// ErrNotSupported is returned when a backend cannot perform an operation.
var ErrNotSupported = errors.New("qkmount: operation not supported by this backend")

// Attr describes a single filesystem entry.
type Attr struct {
	IsDir bool
	Size  int64
	Mode  uint32 // permission bits only, e.g. 0o644
	Mtime time.Time
	Atime time.Time
	Ctime time.Time
}

// DirEntry is one item returned by ReadDir.
type DirEntry struct {
	Name  string
	IsDir bool
	Size  int64
	Mtime time.Time
	Mode  uint32
}

// ReadHandle provides random-access reads.
type ReadHandle interface {
	ReadAt(p []byte, off int64) (int, error) // io.EOF at end
	Size() int64
	Close() error
}

// WriteHandle provides staged writes.  Data is buffered locally and pushed to
// the remote service on Flush/Close.
type WriteHandle interface {
	WriteAt(p []byte, off int64) (int, error)
	Truncate(size int64) error
	Flush() error
	Close() error
	Size() int64
}

// Backend is the interface every connection type implements.
type Backend interface {
	// Kind returns the short type name, e.g. "sftp".
	Kind() string

	// Init opens the connection.  Called once before serving.
	Init() error

	// Stat returns metadata for path.
	Stat(path string) (*Attr, error)

	// ReadDir lists the immediate children of path.
	ReadDir(path string) ([]DirEntry, error)

	Mkdir(path string, mode uint32) error
	Remove(path string) error
	RemoveDir(path string) error
	Rename(oldPath, newPath string) error
	Truncate(path string, size int64) error
	Chmod(path string, mode uint32) error
	Chtimes(path string, atime, mtime time.Time) error

	OpenRead(path string) (ReadHandle, error)
	OpenWrite(path string, truncate bool) (WriteHandle, error)

	// StatFS reports total/free/available bytes (best effort).
	StatFS() (total, free, avail uint64)

	// Close releases the connection.
	Close() error
}

// ---------------------------------------------------------------------------
// Path helpers (always POSIX-style, rooted at "/")
// ---------------------------------------------------------------------------

// Join appends name to a slash separated path.
func Join(base, name string) string {
	if base == "" || base == "/" {
		return "/" + name
	}
	return strings.TrimRight(base, "/") + "/" + name
}

// Parent returns the parent directory of path.
func Parent(path string) string {
	path = strings.TrimRight(path, "/")
	if path == "" {
		return "/"
	}
	i := strings.LastIndex(path, "/")
	if i <= 0 {
		return "/"
	}
	return path[:i]
}

// Base returns the final element of path.
func Base(path string) string {
	path = strings.TrimRight(path, "/")
	if path == "" {
		return "/"
	}
	return path[strings.LastIndex(path, "/")+1:]
}

// Clean normalises a FUSE path to "/a/b" form.
func Clean(path string) string {
	if path == "" || path == "/" {
		return "/"
	}
	parts := strings.Split(path, "/")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p == "" || p == "." {
			continue
		}
		out = append(out, p)
	}
	return "/" + strings.Join(out, "/")
}

// SortEntries sorts directory entries by name.
func SortEntries(entries []DirEntry) {
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name < entries[j].Name })
}

// ---------------------------------------------------------------------------
// Staged writer used by every backend
// ---------------------------------------------------------------------------

// stagedWriter buffers writes in a local temp file and uploads on Close.
type stagedWriter struct {
	f      *os.File
	path   string
	size   int64
	closed bool
	upload func(path string, r io.Reader, size int64) error
	flush  func() error
}

// NewStagedWriter creates a writer that calls upload(path, reader, size) on
// Flush/Close.  flush (optional) is invoked on Flush for extra durability.
func NewStagedWriter(path string, upload func(path string, r io.Reader, size int64) error, flush func() error) (WriteHandle, error) {
	f, err := os.CreateTemp("", "qkmount-*.part")
	if err != nil {
		return nil, err
	}
	return &stagedWriter{f: f, path: path, upload: upload, flush: flush}, nil
}

func (w *stagedWriter) WriteAt(p []byte, off int64) (int, error) {
	n, err := w.f.WriteAt(p, off)
	if end := off + int64(n); end > w.size {
		w.size = end
	}
	return n, err
}

func (w *stagedWriter) Truncate(size int64) error {
	if err := w.f.Truncate(size); err != nil {
		return err
	}
	w.size = size
	return nil
}

func (w *stagedWriter) Size() int64 { return w.size }

func (w *stagedWriter) Flush() error {
	if w.flush != nil {
		return w.flush()
	}
	return nil
}

func (w *stagedWriter) Close() error {
	if w.closed {
		return nil
	}
	w.closed = true
	defer os.Remove(w.f.Name())
	defer w.f.Close()
	if _, err := w.f.Seek(0, io.SeekStart); err != nil {
		return err
	}
	return w.upload(w.path, w.f, w.size)
}

// UploadFromFile is a helper for backends that prefer a path.
func UploadFromFile(path string, src *os.File, size int64, put func(string, io.Reader, int64) error) error {
	if _, err := src.Seek(0, io.SeekStart); err != nil {
		return err
	}
	return put(path, src, size)
}

// BytesReader converts a byte slice to an io.Reader.
func BytesReader(b []byte) io.Reader { return strings.NewReader(string(b)) }

// EnsureParentDirs is a no-op helper kept for backends that need to create
// intermediate directories before an upload.
type EnsureParentDirs func(path string) error

// NotImplemented returns a wrapped ErrNotSupported with context.
func NotImplemented(op, kind string) error {
	return fmt.Errorf("%s: %w (%s)", op, ErrNotSupported, kind)
}

// directWriter is implemented by backends whose write handles edit the remote
// file in place (so opening without truncate must not pre-load the content).
type directWriter interface {
	DirectWrite() bool
}

// IsDirectWrite reports whether a backend writes directly to the remote file.
func IsDirectWrite(b Backend) bool {
	if d, ok := b.(directWriter); ok {
		return d.DirectWrite()
	}
	return false
}
