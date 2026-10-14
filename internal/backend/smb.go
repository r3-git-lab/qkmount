package backend

import (
	"fmt"
	"io"
	"net"
	"os"
	"strings"
	"time"

	"github.com/hirochachacha/go-smb2"
)

func init() {
	Register("smb", func(name string, opts map[string]string) (Backend, error) {
		return newSMB(name, opts)
	})
}

type smbBackend struct {
	name   string
	addr   string
	share  string
	domain string
	user   string
	pass   string
	base   string

	session *smb2.Session
	fs      *smb2.Share
}

func newSMB(name string, opts map[string]string) (*smbBackend, error) {
	host := opts["host"]
	share := opts["share"]
	if host == "" || share == "" {
		return nil, fmt.Errorf("smb: host and share are required")
	}
	return &smbBackend{
		name:   name,
		addr:   net.JoinHostPort(host, Opt(opts, "port", "445")),
		share:  share,
		domain: opts["domain"],
		user:   opts["user"],
		pass:   opts["password"],
		base:   Clean(Opt(opts, "path", "/")),
	}, nil
}

func (b *smbBackend) Kind() string      { return "smb" }
func (b *smbBackend) DirectWrite() bool { return true }

func (b *smbBackend) Init() error {
	conn, err := net.DialTimeout("tcp", b.addr, 20*time.Second)
	if err != nil {
		return err
	}
	d := &smb2.Dialer{
		Initiator: &smb2.NTLMInitiator{
			User:     b.user,
			Password: b.pass,
			Domain:   b.domain,
		},
	}
	s, err := d.Dial(conn)
	if err != nil {
		conn.Close()
		return err
	}
	fs, err := s.Mount(b.share)
	if err != nil {
		s.Logoff()
		return err
	}
	b.session = s
	b.fs = fs
	_, err = b.fs.Stat(b.remote("/"))
	return err
}

func (b *smbBackend) Close() error {
	if b.fs != nil {
		b.fs.Umount()
	}
	if b.session != nil {
		b.session.Logoff()
	}
	return nil
}

func (b *smbBackend) remote(p string) string {
	clean := strings.TrimPrefix(Clean(p), "/")
	if b.base == "/" || b.base == "" {
		if clean == "" {
			return "."
		}
		return clean
	}
	if clean == "" {
		return strings.TrimPrefix(b.base, "/")
	}
	return strings.TrimPrefix(b.base, "/") + "/" + clean
}

func (b *smbBackend) Stat(p string) (*Attr, error) {
	fi, err := b.fs.Stat(b.remote(p))
	if err != nil {
		return nil, mapSMBErr(err)
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

func (b *smbBackend) ReadDir(p string) ([]DirEntry, error) {
	entries, err := b.fs.ReadDir(b.remote(p))
	if err != nil {
		return nil, mapSMBErr(err)
	}
	out := make([]DirEntry, 0, len(entries))
	for _, e := range entries {
		out = append(out, DirEntry{
			Name:  e.Name(),
			IsDir: e.IsDir(),
			Size:  e.Size(),
			Mtime: e.ModTime(),
			Mode:  uint32(e.Mode().Perm()),
		})
	}
	SortEntries(out)
	return out, nil
}

func (b *smbBackend) Mkdir(p string, mode uint32) error {
	return mapSMBErr(b.fs.Mkdir(b.remote(p), os.FileMode(mode&0o777)|0o700))
}
func (b *smbBackend) Remove(p string) error {
	return mapSMBErr(b.fs.Remove(b.remote(p)))
}
func (b *smbBackend) RemoveDir(p string) error {
	return mapSMBErr(b.fs.RemoveAll(b.remote(p)))
}
func (b *smbBackend) Rename(o, n string) error {
	return mapSMBErr(b.fs.Rename(b.remote(o), b.remote(n)))
}
func (b *smbBackend) Truncate(p string, size int64) error {
	return mapSMBErr(b.fs.Truncate(b.remote(p), size))
}
func (b *smbBackend) Chmod(p string, mode uint32) error {
	return mapSMBErr(b.fs.Chmod(b.remote(p), os.FileMode(mode&0o777)))
}
func (b *smbBackend) Chtimes(p string, a, m time.Time) error {
	return mapSMBErr(b.fs.Chtimes(b.remote(p), a, m))
}

func (b *smbBackend) StatFS() (uint64, uint64, uint64) {
	info, err := b.fs.Statfs(b.remote("/"))
	if err != nil {
		return 1 << 40, 1 << 39, 1 << 39
	}
	blk := info.BlockSize()
	total := blk * info.TotalBlockCount()
	free := blk * info.AvailableBlockCount()
	return total, free, free
}

func (b *smbBackend) OpenRead(p string) (ReadHandle, error) {
	f, err := b.fs.Open(b.remote(p))
	if err != nil {
		return nil, mapSMBErr(err)
	}
	fi, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, err
	}
	return &smbRead{f: f, size: fi.Size()}, nil
}

func (b *smbBackend) OpenWrite(p string, truncate bool) (WriteHandle, error) {
	flags := os.O_RDWR | os.O_CREATE
	if truncate {
		flags |= os.O_TRUNC
	}
	f, err := b.fs.OpenFile(b.remote(p), flags, 0o644)
	if err != nil {
		return nil, mapSMBErr(err)
	}
	return &smbWrite{f: f}, nil
}

type smbRead struct {
	f    *smb2.File
	size int64
}

func (h *smbRead) ReadAt(p []byte, off int64) (int, error) { return h.f.ReadAt(p, off) }
func (h *smbRead) Size() int64                             { return h.size }
func (h *smbRead) Close() error                            { return h.f.Close() }

type smbWrite struct {
	f *smb2.File
}

func (h *smbWrite) WriteAt(p []byte, off int64) (int, error) { return h.f.WriteAt(p, off) }
func (h *smbWrite) Truncate(size int64) error                { return h.f.Truncate(size) }
func (h *smbWrite) Flush() error                             { return h.f.Sync() }
func (h *smbWrite) Close() error                             { return h.f.Close() }
func (h *smbWrite) Size() int64 {
	fi, err := h.f.Stat()
	if err != nil {
		return 0
	}
	return fi.Size()
}

func mapSMBErr(err error) error {
	if err == nil {
		return nil
	}
	if os.IsNotExist(err) {
		return os.ErrNotExist
	}
	if os.IsPermission(err) {
		return os.ErrPermission
	}
	if os.IsExist(err) {
		return os.ErrExist
	}
	return err
}

var _ = io.EOF
