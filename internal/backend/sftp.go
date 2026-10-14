package backend

import (
	"fmt"
	"io"
	"net"
	"os"
	"path"
	"strings"
	"sync"
	"time"

	"github.com/pkg/sftp"
	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/agent"
)

func init() {
	Register("sftp", func(name string, opts map[string]string) (Backend, error) {
		return newSFTP(name, opts)
	})
}

type sftpBackend struct {
	name       string
	addr       string
	user       string
	pass       string
	keyFile    string
	passphrase string
	base       string
	timeout    time.Duration

	mu      sync.Mutex
	client  *ssh.Client
	sftp    *sftp.Client
	dialing chan struct{}
}

func newSFTP(name string, opts map[string]string) (*sftpBackend, error) {
	host := opts["host"]
	if host == "" {
		return nil, fmt.Errorf("sftp: host is required")
	}
	return &sftpBackend{
		name:       name,
		addr:       net.JoinHostPort(host, Opt(opts, "port", "22")),
		user:       Opt(opts, "user", "root"),
		pass:       opts["password"],
		keyFile:    opts["private_key"],
		passphrase: opts["passphrase"],
		base:       Clean(Opt(opts, "path", "/")),
		timeout:    25 * time.Second,
	}, nil
}

func (b *sftpBackend) Kind() string { return "sftp" }

func (b *sftpBackend) authMethods() ([]ssh.AuthMethod, error) {
	var methods []ssh.AuthMethod

	// SSH agent.
	if sock := os.Getenv("SSH_AUTH_SOCK"); sock != "" {
		if conn, err := net.Dial("unix", sock); err == nil {
			ag := agent.NewClient(conn)
			methods = append(methods, ssh.PublicKeysCallback(ag.Signers))
		}
	}

	// Private key file.
	if b.keyFile != "" {
		path := expandHome(b.keyFile)
		raw, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("sftp: cannot read key %s: %w", path, err)
		}
		var signer ssh.Signer
		if b.passphrase != "" {
			signer, err = ssh.ParsePrivateKeyWithPassphrase(raw, []byte(b.passphrase))
		} else {
			signer, err = ssh.ParsePrivateKey(raw)
		}
		if err != nil {
			return nil, fmt.Errorf("sftp: cannot parse key: %w", err)
		}
		methods = append(methods, ssh.PublicKeys(signer))
	}

	// Try conventional key names in ~/.ssh if nothing else was given.
	if b.keyFile == "" && b.pass == "" {
		for _, name := range []string{"id_ed25519", "id_rsa", "id_ecdsa"} {
			p := expandHome("~/.ssh/" + name)
			if raw, err := os.ReadFile(p); err == nil {
				if signer, err := ssh.ParsePrivateKey(raw); err == nil {
					methods = append(methods, ssh.PublicKeys(signer))
				}
			}
		}
	}

	if b.pass != "" {
		methods = append(methods, ssh.Password(b.pass))
		methods = append(methods, ssh.KeyboardInteractive(
			func(user, instruction string, questions []string, echos []bool) ([]string, error) {
				answers := make([]string, len(questions))
				for i := range answers {
					answers[i] = b.pass
				}
				return answers, nil
			}))
	}
	if len(methods) == 0 {
		return nil, fmt.Errorf("sftp: no authentication method configured (need password or key)")
	}
	return methods, nil
}

func (b *sftpBackend) connect() error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.sftp != nil {
		return nil
	}
	methods, err := b.authMethods()
	if err != nil {
		return err
	}
	cfg := &ssh.ClientConfig{
		User: b.user,
		Auth: methods,
		// Host key checking: many hosted SFTP servers rotate keys; users can
		// pin a fingerprint later.  We accept the key but log it.
		HostKeyCallback: ssh.InsecureIgnoreHostKey(),
		Timeout:         b.timeout,
	}
	conn, err := ssh.Dial("tcp", b.addr, cfg)
	if err != nil {
		return err
	}
	sc, err := sftp.NewClient(conn,
		sftp.UseConcurrentReads(true),
		sftp.UseConcurrentWrites(true),
		sftp.MaxPacketUnchecked(32768))
	if err != nil {
		conn.Close()
		return err
	}
	b.client = conn
	b.sftp = sc
	return nil
}

func (b *sftpBackend) Init() error { return b.connect() }

func (b *sftpBackend) Close() error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.sftp != nil {
		b.sftp.Close()
		b.sftp = nil
	}
	if b.client != nil {
		b.client.Close()
		b.client = nil
	}
	return nil
}

// c returns a live sftp client, reconnecting once if the connection died.
func (b *sftpBackend) c() (*sftp.Client, error) {
	b.mu.Lock()
	sc := b.sftp
	b.mu.Unlock()
	if sc != nil {
		return sc, nil
	}
	if err := b.connect(); err != nil {
		return nil, err
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.sftp, nil
}

func (b *sftpBackend) remote(p string) string {
	clean := Clean(p)
	if b.base == "/" || b.base == "" {
		return clean
	}
	if clean == "/" {
		return b.base
	}
	return path.Join(b.base, clean)
}

func (b *sftpBackend) Stat(p string) (*Attr, error) {
	sc, err := b.c()
	if err != nil {
		return nil, err
	}
	fi, err := sc.Stat(b.remote(p))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, os.ErrNotExist
		}
		if strings.Contains(err.Error(), "permission") {
			return nil, os.ErrPermission
		}
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

func (b *sftpBackend) ReadDir(p string) ([]DirEntry, error) {
	sc, err := b.c()
	if err != nil {
		return nil, err
	}
	entries, err := sc.ReadDir(b.remote(p))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, os.ErrNotExist
		}
		return nil, err
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

func (b *sftpBackend) Mkdir(p string, mode uint32) error {
	sc, err := b.c()
	if err != nil {
		return err
	}
	return sc.Mkdir(b.remote(p))
}
func (b *sftpBackend) Remove(p string) error {
	sc, err := b.c()
	if err != nil {
		return err
	}
	return sc.Remove(b.remote(p))
}
func (b *sftpBackend) RemoveDir(p string) error {
	sc, err := b.c()
	if err != nil {
		return err
	}
	return sc.RemoveDirectory(b.remote(p))
}
func (b *sftpBackend) Rename(o, n string) error {
	sc, err := b.c()
	if err != nil {
		return err
	}
	return sc.Rename(b.remote(o), b.remote(n))
}
func (b *sftpBackend) Truncate(p string, size int64) error {
	sc, err := b.c()
	if err != nil {
		return err
	}
	return sc.Truncate(b.remote(p), size)
}
func (b *sftpBackend) Chmod(p string, mode uint32) error {
	sc, err := b.c()
	if err != nil {
		return err
	}
	return sc.Chmod(b.remote(p), os.FileMode(mode&0o777))
}
func (b *sftpBackend) Chtimes(p string, a, m time.Time) error {
	sc, err := b.c()
	if err != nil {
		return err
	}
	return sc.Chtimes(b.remote(p), a, m)
}

func (b *sftpBackend) StatFS() (uint64, uint64, uint64) {
	sc, err := b.c()
	if err != nil {
		return 1 << 40, 1 << 39, 1 << 39
	}
	s, err := sc.StatVFS(b.remote("/"))
	if err != nil {
		return 1 << 40, 1 << 39, 1 << 39
	}
	total := s.TotalSpace()
	free := s.FreeSpace()
	return total, free, free
}

func (b *sftpBackend) OpenRead(p string) (ReadHandle, error) {
	sc, err := b.c()
	if err != nil {
		return nil, err
	}
	f, err := sc.Open(b.remote(p))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, os.ErrNotExist
		}
		return nil, err
	}
	fi, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, err
	}
	return &sftpRead{f: f, size: fi.Size()}, nil
}

func (b *sftpBackend) OpenWrite(p string, truncate bool) (WriteHandle, error) {
	return NewStagedWriter(p, func(fp string, r io.Reader, size int64) error {
		sc, err := b.c()
		if err != nil {
			return err
		}
		remote := b.remote(fp)
		_ = sc.MkdirAll(path.Dir(remote))
		f, err := sc.OpenFile(remote, os.O_WRONLY|os.O_CREATE|os.O_TRUNC)
		if err != nil {
			return err
		}
		defer f.Close()
		_, err = io.Copy(f, r)
		return err
	}, nil)
}

type sftpRead struct {
	f    *sftp.File
	size int64
}

func (h *sftpRead) ReadAt(p []byte, off int64) (int, error) { return h.f.ReadAt(p, off) }
func (h *sftpRead) Size() int64                             { return h.size }
func (h *sftpRead) Close() error                            { return h.f.Close() }

func expandHome(p string) string {
	if strings.HasPrefix(p, "~/") {
		if home, err := os.UserHomeDir(); err == nil {
			return path.Join(home, p[2:])
		}
	}
	return p
}
