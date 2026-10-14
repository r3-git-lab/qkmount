// Package vfs adapts a backend.Backend to cgofuse's FileSystemInterface so a
// remote service can be served as a mounted filesystem.
package vfs

import (
	"errors"
	"io"
	"log"
	"os"
	"sync"
	"time"

	"github.com/winfsp/cgofuse/fuse"

	"qkmount/internal/backend"
)

// FS implements fuse.FileSystemInterface on top of a backend.Backend.
type FS struct {
	fuse.FileSystemBase

	be       backend.Backend
	name     string
	readOnly bool
	uid      uint32
	gid      uint32

	mu     sync.Mutex
	nextFh uint64
	open   map[uint64]*openFile
}

type openFile struct {
	path string
	r    backend.ReadHandle
	w    backend.WriteHandle
	size int64
}

// New creates a filesystem serving the given backend.
func New(be backend.Backend, name string, readOnly bool) *FS {
	return &FS{
		be:       be,
		name:     name,
		readOnly: readOnly,
		uid:      uint32(os.Getuid()),
		gid:      uint32(os.Getgid()),
		nextFh:   1,
		open:     map[uint64]*openFile{},
	}
}

// MountPoint/model -----------------------------------------------------------------

func (fs *FS) allocFh(of *openFile) uint64 {
	fs.mu.Lock()
	defer fs.mu.Unlock()
	fh := fs.nextFh
	fs.nextFh++
	fs.open[fh] = of
	return fh
}

func (fs *FS) getFh(fh uint64) *openFile {
	fs.mu.Lock()
	defer fs.mu.Unlock()
	return fs.open[fh]
}

func (fs *FS) dropFh(fh uint64) *openFile {
	fs.mu.Lock()
	defer fs.mu.Unlock()
	of := fs.open[fh]
	delete(fs.open, fh)
	return of
}

// errno maps a backend error to a negative errno value. Unexpected errors are
// logged so they surface in the mount log instead of only as an errno.
func errno(err error) int {
	switch {
	case err == nil:
		return 0
	case errors.Is(err, os.ErrNotExist):
		return -fuse.ENOENT
	case errors.Is(err, os.ErrExist):
		return -fuse.EEXIST
	case errors.Is(err, backend.ErrNotSupported):
		return -fuse.ENOSYS
	case errors.Is(err, backend.ErrNotDownloadable):
		log.Printf("qkmount: %v", err)
		return -fuse.EOPNOTSUPP
	case errors.Is(err, io.EOF):
		return 0
	case errors.Is(err, backend.ErrRateLimited):
		log.Printf("qkmount: %v (transient: the provider is rate-limiting; retry shortly or supply your own OAuth client)", err)
		return -fuse.EIO
	case errors.Is(err, os.ErrPermission):
		log.Printf("qkmount: %v", err)
		return -fuse.EACCES
	}
	log.Printf("qkmount: %v", err)
	return -fuse.EIO
}

func fillStat(st *fuse.Stat_t, a *backend.Attr, uid, gid uint32) {
	mode := a.Mode
	if mode == 0 {
		if a.IsDir {
			mode = 0o755
		} else {
			mode = 0o644
		}
	}
	if a.IsDir {
		st.Mode = fuse.S_IFDIR | mode
		st.Nlink = 2
	} else {
		st.Mode = fuse.S_IFREG | mode
		st.Nlink = 1
	}
	st.Size = a.Size
	st.Uid = uid
	st.Gid = gid
	now := time.Now()
	at, mt, ct := a.Atime, a.Mtime, a.Ctime
	if mt.IsZero() {
		mt = now
	}
	if at.IsZero() {
		at = mt
	}
	if ct.IsZero() {
		ct = mt
	}
	st.Atim = fuse.NewTimespec(at)
	st.Mtim = fuse.NewTimespec(mt)
	st.Ctim = fuse.NewTimespec(ct)
	st.Blksize = 4096
	st.Blocks = (a.Size + 511) / 512
}

// --------------------------------------------------------------------------
// Lifecycle
// --------------------------------------------------------------------------

// Init is called by cgofuse once the filesystem is mounted.  The connection
// is opened by the caller beforehand so failures can be reported cleanly.
func (fs *FS) Init() {}

// Destroy releases the backend.
func (fs *FS) Destroy() {
	fs.mu.Lock()
	for fh, of := range fs.open {
		if of.w != nil {
			_ = of.w.Close()
		}
		if of.r != nil {
			_ = of.r.Close()
		}
		delete(fs.open, fh)
	}
	fs.mu.Unlock()
	_ = fs.be.Close()
}

// --------------------------------------------------------------------------
// Metadata
// --------------------------------------------------------------------------

func (fs *FS) Getattr(path string, st *fuse.Stat_t, fh uint64) int {
	if path == "/" {
		fillStat(st, &backend.Attr{IsDir: true, Mode: 0o755}, fs.uid, fs.gid)
		return 0
	}
	if of := fs.getFh(fh); of != nil && of.w != nil {
		fillStat(st, &backend.Attr{Size: of.w.Size(), Mtime: time.Now()}, fs.uid, fs.gid)
		return 0
	}
	a, err := fs.be.Stat(path)
	if err != nil {
		return errno(err)
	}
	fillStat(st, a, fs.uid, fs.gid)
	return 0
}

func (fs *FS) Statfs(path string, st *fuse.Statfs_t) int {
	total, free, avail := fs.be.StatFS()
	st.Bsize = 4096
	st.Frsize = 4096
	st.Blocks = total / 4096
	st.Bfree = free / 4096
	st.Bavail = avail / 4096
	st.Files = 1 << 24
	st.Ffree = 1 << 24
	st.Namemax = 255
	return 0
}

func (fs *FS) Access(path string, mask uint32) int {
	if fs.readOnly && mask&2 != 0 { // W_OK
		return -fuse.EROFS
	}
	if path == "/" {
		return 0
	}
	if _, err := fs.be.Stat(path); err != nil {
		return errno(err)
	}
	return 0
}

// --------------------------------------------------------------------------
// Directories
// --------------------------------------------------------------------------

func (fs *FS) Opendir(path string) (int, uint64) {
	if _, err := fs.be.Stat(path); err != nil {
		return errno(err), 0
	}
	return 0, fs.allocFh(&openFile{path: path})
}

func (fs *FS) Readdir(path string,
	fill func(name string, stat *fuse.Stat_t, ofst int64) bool,
	ofst int64,
	fh uint64) int {
	fill(".", nil, 0)
	fill("..", nil, 0)
	entries, err := fs.be.ReadDir(path)
	if err != nil {
		return errno(err)
	}
	for _, e := range entries {
		st := &fuse.Stat_t{}
		fillStat(st, &backend.Attr{IsDir: e.IsDir, Size: e.Size, Mode: e.Mode, Mtime: e.Mtime}, fs.uid, fs.gid)
		if !fill(e.Name, st, 0) {
			break
		}
	}
	return 0
}

func (fs *FS) Releasedir(path string, fh uint64) int {
	fs.dropFh(fh)
	return 0
}

func (fs *FS) Mkdir(path string, mode uint32) int {
	if fs.readOnly {
		return -fuse.EROFS
	}
	if err := fs.be.Mkdir(path, mode); err != nil {
		return errno(err)
	}
	return 0
}

func (fs *FS) Rmdir(path string) int {
	if fs.readOnly {
		return -fuse.EROFS
	}
	if err := fs.be.RemoveDir(path); err != nil {
		return errno(err)
	}
	return 0
}

func (fs *FS) Unlink(path string) int {
	if fs.readOnly {
		return -fuse.EROFS
	}
	if err := fs.be.Remove(path); err != nil {
		return errno(err)
	}
	return 0
}

func (fs *FS) Rename(oldpath, newpath string) int {
	if fs.readOnly {
		return -fuse.EROFS
	}
	if err := fs.be.Rename(oldpath, newpath); err != nil {
		return errno(err)
	}
	return 0
}

func (fs *FS) Chmod(path string, mode uint32) int {
	if fs.readOnly {
		return 0 // silently ignore; many backends have no permissions
	}
	if err := fs.be.Chmod(path, mode); err != nil {
		return errno(err)
	}
	return 0
}

func (fs *FS) Utimens(path string, tmsp []fuse.Timespec) int {
	if fs.readOnly || len(tmsp) < 2 {
		return 0
	}
	atime := time.Unix(tmsp[0].Sec, tmsp[0].Nsec)
	mtime := time.Unix(tmsp[1].Sec, tmsp[1].Nsec)
	if err := fs.be.Chtimes(path, atime, mtime); err != nil {
		return errno(err)
	}
	return 0
}

// --------------------------------------------------------------------------
// Files
// --------------------------------------------------------------------------

func (fs *FS) Open(path string, flags int) (int, uint64) {
	writable := flags&(fuse.O_WRONLY|fuse.O_RDWR) != 0
	if writable {
		if fs.readOnly {
			return -fuse.EROFS, 0
		}
		truncate := flags&fuse.O_TRUNC != 0
		of, err := fs.openForWrite(path, truncate)
		if err != nil {
			return errno(err), 0
		}
		return 0, fs.allocFh(of)
	}
	rh, err := fs.be.OpenRead(path)
	if err != nil {
		return errno(err), 0
	}
	of := &openFile{path: path, r: rh, size: rh.Size()}
	return 0, fs.allocFh(of)
}

func (fs *FS) Create(path string, flags int, mode uint32) (int, uint64) {
	if fs.readOnly {
		return -fuse.EROFS, 0
	}
	of, err := fs.openForWrite(path, true)
	if err != nil {
		return errno(err), 0
	}
	return 0, fs.allocFh(of)
}

// openForWrite creates a write handle; when truncate is false and the file
// already exists its contents are pre-loaded so partial edits behave.
func (fs *FS) openForWrite(path string, truncate bool) (*openFile, error) {
	w, err := fs.be.OpenWrite(path, truncate)
	if err != nil {
		return nil, err
	}
	of := &openFile{path: path, w: w}
	if !truncate && !backend.IsDirectWrite(fs.be) {
		if a, err := fs.be.Stat(path); err == nil && a.Size > 0 {
			if err := fs.prefill(of, a.Size); err != nil {
				// Non-fatal: continue with an empty file.
				_ = err
			}
		}
	}
	return of, nil
}

func (fs *FS) prefill(of *openFile, size int64) error {
	rh, err := fs.be.OpenRead(of.path)
	if err != nil {
		return err
	}
	defer rh.Close()
	buf := make([]byte, 128*1024)
	var off int64
	for off < size {
		n, err := rh.ReadAt(buf, off)
		if n > 0 {
			if _, werr := of.w.WriteAt(buf[:n], off); werr != nil {
				return werr
			}
			off += int64(n)
		}
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}
		if n == 0 {
			break
		}
	}
	return nil
}

func (fs *FS) Read(path string, buff []byte, ofst int64, fh uint64) int {
	of := fs.getFh(fh)
	if of == nil {
		return -fuse.EBADF
	}
	// Reading a staged file (opened for write): read from the writer's buffer.
	if of.r == nil && of.w == nil {
		return -fuse.EBADF
	}
	if of.r == nil {
		return 0 // no read data available for write-only handles
	}
	n, err := of.r.ReadAt(buff, ofst)
	if n > 0 {
		return n
	}
	if err == nil || err == io.EOF {
		return 0
	}
	return errno(err)
}

func (fs *FS) Write(path string, buff []byte, ofst int64, fh uint64) int {
	if fs.readOnly {
		return -fuse.EROFS
	}
	of := fs.getFh(fh)
	if of == nil || of.w == nil {
		return -fuse.EBADF
	}
	n, err := of.w.WriteAt(buff, ofst)
	if err != nil {
		return errno(err)
	}
	return n
}

func (fs *FS) Truncate(path string, size int64, fh uint64) int {
	if fs.readOnly {
		return -fuse.EROFS
	}
	if of := fs.getFh(fh); of != nil && of.w != nil {
		if err := of.w.Truncate(size); err != nil {
			return errno(err)
		}
		return 0
	}
	if err := fs.be.Truncate(path, size); err != nil {
		return errno(err)
	}
	return 0
}

func (fs *FS) Flush(path string, fh uint64) int {
	if of := fs.getFh(fh); of != nil && of.w != nil {
		if err := of.w.Flush(); err != nil {
			return errno(err)
		}
	}
	return 0
}

func (fs *FS) Fsync(path string, datasync bool, fh uint64) int {
	return fs.Flush(path, fh)
}

func (fs *FS) Release(path string, fh uint64) int {
	of := fs.dropFh(fh)
	if of == nil {
		return 0
	}
	var firstErr error
	if of.w != nil {
		if err := of.w.Close(); err != nil {
			firstErr = err
		}
	}
	if of.r != nil {
		_ = of.r.Close()
	}
	if firstErr != nil {
		return errno(firstErr)
	}
	return 0
}
