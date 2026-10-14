package backend

import (
	"io"
	"os"
	"strings"
	"sync"
	"time"
)

// ObjectStore is the low level contract for key/value style services.
type ObjectStore interface {
	Kind() string
	Init() error
	Close() error

	// List returns objects and child "directories" immediately below prefix.
	List(prefix, delimiter string) (objects []ObjMeta, prefixes []string, err error)

	// Head returns metadata for an object, or (nil, nil) if it does not exist.
	Head(key string) (*ObjMeta, error)

	// Get returns up to size bytes starting at off.
	Get(key string, off, size int64) ([]byte, error)

	// Put stores size bytes from r at key.
	Put(key string, r io.Reader, size int64) error

	Delete(key string) error
	Copy(src, dst string) error
	StatFS() (total, free, avail uint64)
}

// ObjMeta describes one stored object.
type ObjMeta struct {
	Key   string
	Size  int64
	Mtime time.Time
}

// objectFS adapts an ObjectStore to the Backend interface, synthesising
// directories from key prefixes.
type objectFS struct {
	store  ObjectStore
	prefix string // key prefix for the mount root (no leading/trailing slash)

	mu    sync.Mutex
	cache map[string]cacheEntry
}

type cacheEntry struct {
	at     time.Time
	names  []DirEntry
	byName map[string]DirEntry
}

// NewObjectFS wraps a store, mapping the mount root to the given key prefix.
func NewObjectFS(store ObjectStore, prefix string) Backend {
	return &objectFS{
		store:  store,
		prefix: strings.Trim(prefix, "/"),
		cache:  map[string]cacheEntry{},
	}
}

func (fs *objectFS) Kind() string                     { return fs.store.Kind() }
func (fs *objectFS) Init() error                      { return fs.store.Init() }
func (fs *objectFS) Close() error                     { return fs.store.Close() }
func (fs *objectFS) StatFS() (uint64, uint64, uint64) { return fs.store.StatFS() }

// keyFor converts a FUSE path to an object key.
func (fs *objectFS) keyFor(path string) string {
	clean := strings.TrimPrefix(Clean(path), "/")
	if fs.prefix == "" {
		return clean
	}
	if clean == "" {
		return fs.prefix
	}
	return fs.prefix + "/" + clean
}

// dirPrefixFor returns the listing prefix (always ending in "/" or empty).
func (fs *objectFS) dirPrefixFor(path string) string {
	k := fs.keyFor(path)
	if k == "" {
		return ""
	}
	return k + "/"
}

func (fs *objectFS) invalidate(path string) {
	fs.mu.Lock()
	delete(fs.cache, Clean(path))
	fs.mu.Unlock()
}

func (fs *objectFS) list(path string) ([]DirEntry, map[string]DirEntry, error) {
	clean := Clean(path)
	fs.mu.Lock()
	if e, ok := fs.cache[clean]; ok && time.Since(e.at) < 5*time.Second {
		fs.mu.Unlock()
		return e.names, e.byName, nil
	}
	fs.mu.Unlock()

	prefix := fs.dirPrefixFor(path)
	objs, prefixes, err := fs.store.List(prefix, "/")
	if err != nil {
		return nil, nil, err
	}
	byName := map[string]DirEntry{}
	var names []DirEntry

	for _, p := range prefixes {
		name := strings.TrimSuffix(strings.TrimPrefix(p, prefix), "/")
		if name == "" {
			continue
		}
		byName[name] = DirEntry{Name: name, IsDir: true, Mode: 0o755}
	}
	for _, o := range objs {
		name := strings.TrimPrefix(o.Key, prefix)
		if name == "" || strings.HasSuffix(name, "/") || strings.Contains(name, "/") {
			continue // the directory marker itself or a nested key
		}
		byName[name] = DirEntry{Name: name, Size: o.Size, Mtime: o.Mtime, Mode: 0o644}
	}
	for _, de := range byName {
		names = append(names, de)
	}
	SortEntries(names)

	fs.mu.Lock()
	fs.cache[clean] = cacheEntry{at: time.Now(), names: names, byName: byName}
	fs.mu.Unlock()
	return names, byName, nil
}

func (fs *objectFS) Stat(path string) (*Attr, error) {
	clean := Clean(path)
	if clean == "/" && fs.prefix == "" {
		return &Attr{IsDir: true, Mode: 0o755}, nil
	}
	// Check as an object first.
	if m, err := fs.store.Head(fs.keyFor(path)); err != nil {
		return nil, err
	} else if m != nil {
		return &Attr{Size: m.Size, Mode: 0o644, Mtime: m.Mtime, Atime: m.Mtime, Ctime: m.Mtime}, nil
	}
	// Otherwise it may be a virtual directory.
	if sd, ok := fs.store.(StatDirStore); ok {
		if isDir, err := sd.StatDir(fs.keyFor(path)); err != nil {
			return nil, err
		} else if isDir {
			return &Attr{IsDir: true, Mode: 0o755}, nil
		}
	}
	names, _, err := fs.list(path)
	if err != nil {
		return nil, err
	}
	if len(names) > 0 {
		return &Attr{IsDir: true, Mode: 0o755}, nil
	}
	// A marker object (key ending in "/") also denotes a directory.
	if m, _ := fs.store.Head(fs.dirPrefixFor(path)); m != nil {
		return &Attr{IsDir: true, Mode: 0o755}, nil
	}
	return nil, os.ErrNotExist
}

func (fs *objectFS) ReadDir(path string) ([]DirEntry, error) {
	names, _, err := fs.list(path)
	if err != nil {
		return nil, err
	}
	if names == nil {
		names = []DirEntry{}
	}
	return names, nil
}

func (fs *objectFS) Mkdir(path string, mode uint32) error {
	// Stores with real directories create a folder; prefix-only stores create
	// a marker object so empty folders persist.
	if ds, ok := fs.store.(DirectoryStore); ok {
		err := ds.Mkdir(Clean(path))
		fs.invalidate(Parent(path))
		fs.invalidate(path)
		return err
	}
	err := fs.store.Put(fs.dirPrefixFor(path), strings.NewReader(""), 0)
	fs.invalidate(Parent(path))
	fs.invalidate(path)
	return err
}

func (fs *objectFS) RemoveDir(path string) error {
	names, _, err := fs.list(path)
	if err != nil {
		return err
	}
	if len(names) > 0 {
		return os.ErrExist
	}
	if ds, ok := fs.store.(DirectoryStore); ok {
		if err := ds.RemoveDir(Clean(path)); err != nil {
			return err
		}
	} else {
		if err := fs.store.Delete(fs.dirPrefixFor(path)); err != nil {
			return err
		}
	}
	fs.invalidate(Parent(path))
	fs.invalidate(path)
	return nil
}

func (fs *objectFS) Remove(path string) error {
	if err := fs.store.Delete(fs.keyFor(path)); err != nil {
		return err
	}
	fs.invalidate(Parent(path))
	return nil
}

func (fs *objectFS) Rename(oldPath, newPath string) error {
	a, err := fs.Stat(oldPath)
	if err != nil {
		return err
	}
	if a.IsDir {
		if rs, ok := fs.store.(RenameStore); ok {
			if err := rs.RenameObject(Clean(oldPath), Clean(newPath)); err != nil {
				return err
			}
			fs.invalidate(Parent(oldPath))
			fs.invalidate(Parent(newPath))
			return nil
		}
		return ErrNotSupported
	}
	if err := fs.store.Copy(fs.keyFor(oldPath), fs.keyFor(newPath)); err != nil {
		return err
	}
	if err := fs.store.Delete(fs.keyFor(oldPath)); err != nil {
		return err
	}
	fs.invalidate(Parent(oldPath))
	fs.invalidate(Parent(newPath))
	return nil
}

func (fs *objectFS) Truncate(path string, size int64) error {
	// Read the prefix we keep, then re-upload.
	var data []byte
	if size > 0 {
		buf, err := fs.store.Get(fs.keyFor(path), 0, size)
		if err != nil {
			return err
		}
		data = buf
	}
	return fs.store.Put(fs.keyFor(path), strings.NewReader(string(data)), int64(len(data)))
}

func (fs *objectFS) Chmod(path string, mode uint32) error      { return nil }
func (fs *objectFS) Chtimes(path string, a, m time.Time) error { return nil }

func (fs *objectFS) OpenRead(path string) (ReadHandle, error) {
	a, err := fs.Stat(path)
	if err != nil {
		return nil, err
	}
	return &objectRead{fs: fs, key: fs.keyFor(path), size: a.Size}, nil
}

func (fs *objectFS) OpenWrite(path string, truncate bool) (WriteHandle, error) {
	return NewStagedWriter(path, func(p string, r io.Reader, size int64) error {
		if err := fs.store.Put(fs.keyFor(p), r, size); err != nil {
			return err
		}
		fs.invalidate(Parent(p))
		return nil
	}, nil)
}

type objectRead struct {
	fs   *objectFS
	key  string
	size int64
}

func (h *objectRead) Size() int64  { return h.size }
func (h *objectRead) Close() error { return nil }

func (h *objectRead) ReadAt(p []byte, off int64) (int, error) {
	if off >= h.size {
		return 0, io.EOF
	}
	want := int64(len(p))
	if off+want > h.size {
		want = h.size - off
	}
	data, err := h.fs.store.Get(h.key, off, want)
	if err != nil {
		return 0, err
	}
	if len(data) == 0 {
		// The store had nothing more to give (e.g. an export whose reported
		// size was an upper bound): treat it as end of file.
		return 0, io.EOF
	}
	n := copy(p, data)
	if int64(n) < h.size-off {
		return n, nil
	}
	return n, io.EOF
}
