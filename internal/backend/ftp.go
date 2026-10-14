package backend

import (
	"crypto/tls"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/jlaffaye/ftp"
)

func init() {
	Register("ftp", func(name string, opts map[string]string) (Backend, error) {
		return newFTP("ftp", name, opts)
	})
	Register("ftps", func(name string, opts map[string]string) (Backend, error) {
		return newFTP("ftps", name, opts)
	})
}

type ftpBackend struct {
	kind     string
	name     string
	addr     string
	user     string
	pass     string
	tls      bool
	insecure bool
	base     string
}

func newFTP(kind, name string, opts map[string]string) (*ftpBackend, error) {
	host := opts["host"]
	if host == "" {
		return nil, fmt.Errorf("ftp: host is required")
	}
	port := Opt(opts, "port", "21")
	tlsOn := OptBool(opts, "tls", false) || kind == "ftps"
	return &ftpBackend{
		kind:     kind,
		name:     name,
		addr:     fmt.Sprintf("%s:%s", host, port),
		user:     Opt(opts, "user", "anonymous"),
		pass:     opts["password"],
		tls:      tlsOn,
		insecure: OptBool(opts, "insecure", false),
		base:     Clean(Opt(opts, "path", "/")),
	}, nil
}

func (b *ftpBackend) Kind() string { return b.kind }

func (b *ftpBackend) dial() (*ftp.ServerConn, error) {
	opts := []ftp.DialOption{ftp.DialWithTimeout(20 * time.Second)}
	if b.tls {
		opts = append(opts, ftp.DialWithExplicitTLS(&tls.Config{
			ServerName:         strings.Split(b.addr, ":")[0],
			InsecureSkipVerify: b.insecure,
		}))
	}
	c, err := ftp.Dial(b.addr, opts...)
	if err != nil {
		return nil, err
	}
	if err := c.Login(b.user, b.pass); err != nil {
		c.Quit()
		return nil, err
	}
	return c, nil
}

func (b *ftpBackend) Init() error {
	c, err := b.dial()
	if err != nil {
		return err
	}
	return c.Quit()
}
func (b *ftpBackend) Close() error { return nil }

func (b *ftpBackend) remote(path string) string {
	clean := Clean(path)
	if b.base == "/" {
		return clean
	}
	if clean == "/" {
		return b.base
	}
	return b.base + clean
}

// withConn runs fn on a fresh connection.
func (b *ftpBackend) withConn(fn func(*ftp.ServerConn) error) error {
	c, err := b.dial()
	if err != nil {
		return err
	}
	defer c.Quit()
	return fn(c)
}

func (b *ftpBackend) Stat(path string) (*Attr, error) {
	if Clean(path) == "/" && b.base == "/" {
		return &Attr{IsDir: true, Mode: 0o755}, nil
	}
	var attr *Attr
	err := b.withConn(func(c *ftp.ServerConn) error {
		// Try as a file first.
		if size, err := c.FileSize(b.remote(path)); err == nil {
			t := time.Now()
			attr = &Attr{IsDir: false, Size: size, Mode: 0o644, Mtime: t, Atime: t, Ctime: t}
			return nil
		}
		// Otherwise try listing it as a directory.
		if entries, err := c.List(b.remote(path)); err == nil {
			_ = entries
			t := time.Now()
			attr = &Attr{IsDir: true, Mode: 0o755, Mtime: t, Atime: t, Ctime: t}
			return nil
		}
		return os.ErrNotExist
	})
	if err != nil {
		return nil, err
	}
	return attr, nil
}

func (b *ftpBackend) ReadDir(path string) ([]DirEntry, error) {
	var out []DirEntry
	err := b.withConn(func(c *ftp.ServerConn) error {
		entries, err := c.List(b.remote(path))
		if err != nil {
			if os.IsNotExist(err) {
				return os.ErrNotExist
			}
			return err
		}
		for _, e := range entries {
			if e.Name == "." || e.Name == ".." {
				continue
			}
			de := DirEntry{Name: e.Name, Size: int64(e.Size)}
			switch e.Type {
			case ftp.EntryTypeFolder:
				de.IsDir = true
				de.Mode = 0o755
			default:
				de.Mode = 0o644
			}
			de.Mtime = e.Time
			out = append(out, de)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	SortEntries(out)
	return out, nil
}

func (b *ftpBackend) Mkdir(path string, mode uint32) error {
	return b.withConn(func(c *ftp.ServerConn) error { return c.MakeDir(b.remote(path)) })
}
func (b *ftpBackend) Remove(path string) error {
	return b.withConn(func(c *ftp.ServerConn) error { return c.Delete(b.remote(path)) })
}
func (b *ftpBackend) RemoveDir(path string) error {
	return b.withConn(func(c *ftp.ServerConn) error { return c.RemoveDir(b.remote(path)) })
}
func (b *ftpBackend) Rename(o, n string) error {
	return b.withConn(func(c *ftp.ServerConn) error { return c.Rename(b.remote(o), b.remote(n)) })
}

func (b *ftpBackend) Truncate(path string, size int64) error {
	// Read, truncate, re-upload.
	rh, err := b.OpenRead(path)
	if err != nil {
		return err
	}
	defer rh.Close()
	data, err := io.ReadAll(io.NewSectionReader(rh, 0, size))
	if err != nil {
		return err
	}
	return b.putStream(path, strings.NewReader(string(data)))
}

func (b *ftpBackend) Chmod(path string, mode uint32) error { return nil }
func (b *ftpBackend) Chtimes(path string, a, m time.Time) error {
	return nil
}

func (b *ftpBackend) StatFS() (uint64, uint64, uint64) {
	const tb = 1 << 40
	return tb, tb / 2, tb / 2
}

func (b *ftpBackend) OpenRead(path string) (ReadHandle, error) {
	c, err := b.dial()
	if err != nil {
		return nil, err
	}
	size, err := c.FileSize(b.remote(path))
	if err != nil {
		c.Quit()
		return nil, os.ErrNotExist
	}
	return &ftpRead{ftp: b, conn: c, path: b.remote(path), size: size, off: -1}, nil
}

func (b *ftpBackend) OpenWrite(path string, truncate bool) (WriteHandle, error) {
	return NewStagedWriter(path, func(p string, r io.Reader, size int64) error {
		return b.putStream(p, r)
	}, nil)
}

func (b *ftpBackend) putStream(path string, r io.Reader) error {
	return b.withConn(func(c *ftp.ServerConn) error {
		return c.Stor(b.remote(path), r)
	})
}

type ftpRead struct {
	ftp  *ftpBackend
	conn *ftp.ServerConn
	path string
	size int64
	off  int64
	rc   io.ReadCloser
}

func (h *ftpRead) Size() int64 { return h.size }

func (h *ftpRead) ReadAt(p []byte, off int64) (int, error) {
	if off >= h.size {
		return 0, io.EOF
	}
	if h.rc == nil || off != h.off {
		if h.rc != nil {
			h.rc.Close()
			h.rc = nil
		}
		rc, err := h.conn.RetrFrom(h.path, uint64(off))
		if err != nil {
			return 0, err
		}
		h.rc = rc
		h.off = off
	}
	n, err := io.ReadFull(h.rc, p)
	h.off += int64(n)
	if err == io.ErrUnexpectedEOF || err == io.EOF {
		err = io.EOF
	}
	return n, err
}

func (h *ftpRead) Close() error {
	if h.rc != nil {
		h.rc.Close()
	}
	return h.conn.Quit()
}
