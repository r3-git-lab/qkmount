package mount

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"qkmount/internal/config"
)

// Status describes the state of a mount.
type Status string

const (
	StatusStopped  Status = "stopped"
	StatusMounting Status = "mounting"
	StatusMounted  Status = "mounted"
	StatusError    Status = "error"
)

// Instance is a snapshot of one connection's mount state (JSON friendly).
type Instance struct {
	ConnID     string `json:"id"`
	Name       string `json:"name"`
	Type       string `json:"type"`
	MountPoint string `json:"mountpoint"`
	Status     Status `json:"status"`
	Message    string `json:"message,omitempty"`
	PID        int    `json:"pid,omitempty"`
	Restarts   int    `json:"restarts"`
	Since      string `json:"since,omitempty"`
}

type inst struct {
	conn     *config.Connection
	cmd      *exec.Cmd
	status   Status
	message  string
	pid      int
	restarts int
	since    time.Time
	stopped  bool
	logPath  string
}

// Manager supervises mount child processes.
type Manager struct {
	store       *config.Store
	configDir   string
	logDir      string
	self        string
	autoRestart bool

	mu    sync.Mutex
	insts map[string]*inst
}

// NewManager creates a supervisor.
func NewManager(store *config.Store, configDir string) *Manager {
	self, err := os.Executable()
	if err != nil {
		self = os.Args[0]
	}
	logDir := filepath.Join(configDir, "logs")
	_ = os.MkdirAll(logDir, 0o700)
	return &Manager{
		store:       store,
		configDir:   configDir,
		logDir:      logDir,
		self:        self,
		autoRestart: true,
		insts:       map[string]*inst{},
	}
}

// SetAutoRestart enables or disables automatic re-mounting.
func (m *Manager) SetAutoRestart(v bool) {
	m.mu.Lock()
	m.autoRestart = v
	m.mu.Unlock()
}

// Start launches the mount for a connection.
func (m *Manager) Start(c *config.Connection) error {
	m.mu.Lock()
	if existing, ok := m.insts[c.ID]; ok && existing.status != StatusStopped {
		m.mu.Unlock()
		return nil // already running
	}
	it := &inst{
		conn:    c,
		status:  StatusMounting,
		since:   time.Now(),
		logPath: filepath.Join(m.logDir, c.ID+".log"),
	}
	m.insts[c.ID] = it
	m.mu.Unlock()
	return m.spawn(it)
}

func (m *Manager) spawn(it *inst) error {
	mp := it.conn.MountPoint
	if mp == "" {
		return fmt.Errorf("connection %q has no mount point", it.conn.Name)
	}
	if err := os.MkdirAll(mp, 0o755); err != nil {
		m.fail(it, fmt.Sprintf("cannot create mount point: %v", err))
		return err
	}
	if IsMounted(mp) {
		// Already mounted (perhaps left over) - treat as running.
		m.mu.Lock()
		it.status = StatusMounted
		it.pid = 0
		m.mu.Unlock()
		return nil
	}

	logf, err := os.OpenFile(it.logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		logf = nil
	}
	cmd := exec.Command(m.self, "__mount", m.configDir, it.conn.ID)
	cmd.Env = append(os.Environ(), "QKMOUNT_CHILD=1")
	cmd.Stdout = logf
	cmd.Stderr = logf
	cmd.SysProcAttr = sysProcAttr()

	if err := cmd.Start(); err != nil {
		if logf != nil {
			logf.Close()
		}
		m.fail(it, fmt.Sprintf("failed to start mount: %v", err))
		return err
	}

	m.mu.Lock()
	it.cmd = cmd
	it.pid = cmd.Process.Pid
	it.status = StatusMounting
	m.mu.Unlock()

	go m.pollReady(it)
	go m.watch(it, logf)
	return nil
}

// pollReady waits until the mountpoint appears (or the process dies).
func (m *Manager) pollReady(it *inst) {
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		m.mu.Lock()
		stopped := it.stopped
		status := it.status
		m.mu.Unlock()
		if stopped || status == StatusError {
			return
		}
		if IsMounted(it.conn.MountPoint) {
			m.mu.Lock()
			if it.status == StatusMounting {
				it.status = StatusMounted
				it.message = ""
				it.since = time.Now()
			}
			m.mu.Unlock()
			return
		}
		time.Sleep(300 * time.Millisecond)
	}
	// Timed out but process may still be up; report as error.
	m.mu.Lock()
	if it.status == StatusMounting {
		it.status = StatusError
		it.message = "mount timed out"
	}
	m.mu.Unlock()
}

// watch reaps the child and restarts it if it dies unexpectedly.
func (m *Manager) watch(it *inst, logf *os.File) {
	err := it.cmd.Wait()
	if logf != nil {
		logf.Close()
	}
	m.mu.Lock()
	stopped := it.stopped
	it.pid = 0
	m.mu.Unlock()
	if stopped {
		m.mu.Lock()
		it.status = StatusStopped
		it.message = ""
		m.mu.Unlock()
		return
	}
	// Unexpected exit.
	tail := tailLog(it.logPath, 800)
	m.mu.Lock()
	it.status = StatusError
	if err != nil {
		it.message = fmt.Sprintf("mount exited: %v %s", err, tail)
	} else {
		it.message = strings.TrimSpace("mount exited unexpectedly " + tail)
	}
	auto := m.autoRestart
	m.mu.Unlock()

	ForceUnmount(it.conn.MountPoint)

	if !auto {
		return
	}
	// Re-mount with capped exponential backoff.
	for attempt := 0; ; attempt++ {
		delay := time.Duration(1<<min(attempt, 6)) * time.Second
		if delay > 60*time.Second {
			delay = 60 * time.Second
		}
		time.Sleep(delay)
		m.mu.Lock()
		if it.stopped {
			m.mu.Unlock()
			return
		}
		it.restarts++
		it.status = StatusMounting
		it.message = fmt.Sprintf("reconnecting (attempt %d)", it.restarts)
		m.mu.Unlock()
		ForceUnmount(it.conn.MountPoint)
		if err := m.spawn(it); err == nil {
			return
		}
	}
}

func (m *Manager) fail(it *inst, msg string) {
	m.mu.Lock()
	it.status = StatusError
	it.message = msg
	m.mu.Unlock()
}

// Stop unmounts and terminates the mount for a connection.
func (m *Manager) Stop(connID string) error {
	m.mu.Lock()
	it, ok := m.insts[connID]
	if !ok {
		m.mu.Unlock()
		return nil
	}
	it.stopped = true
	cmd := it.cmd
	mp := it.conn.MountPoint
	m.mu.Unlock()

	// Unmount first; the FUSE process normally exits on its own afterwards.
	ForceUnmount(mp)
	if cmd != nil && cmd.Process != nil {
		_ = cmd.Process.Signal(os.Interrupt)
		time.Sleep(200 * time.Millisecond)
		if !processExited(cmd) {
			_ = cmd.Process.Kill()
		}
	}
	m.mu.Lock()
	it.status = StatusStopped
	it.pid = 0
	m.mu.Unlock()
	return nil
}

// StopAll unmounts every managed connection.
func (m *Manager) StopAll() {
	m.mu.Lock()
	ids := make([]string, 0, len(m.insts))
	for id := range m.insts {
		ids = append(ids, id)
	}
	m.mu.Unlock()
	for _, id := range ids {
		_ = m.Stop(id)
	}
}

// IsRunning reports whether a connection currently has a live process.
func (m *Manager) IsRunning(connID string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	it, ok := m.insts[connID]
	return ok && (it.status == StatusMounted || it.status == StatusMounting)
}

// Snapshot returns the current state of all known connections.
func (m *Manager) Snapshot() []Instance {
	conns := m.store.List()
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]Instance, 0, len(conns))
	for _, c := range conns {
		inst := Instance{
			ConnID:     c.ID,
			Name:       c.Name,
			Type:       c.Type,
			MountPoint: c.MountPoint,
		}
		if it, ok := m.insts[c.ID]; ok {
			inst.Status = it.status
			inst.Message = it.message
			inst.PID = it.pid
			inst.Restarts = it.restarts
			if !it.since.IsZero() {
				inst.Since = it.since.Format(time.RFC3339)
			}
		} else {
			inst.Status = StatusStopped
		}
		out = append(out, inst)
	}
	return out
}

// reconcile makes the running mounts match the connections flagged AutoMount.
func (m *Manager) Reconcile() {
	for _, c := range m.store.List() {
		if c.AutoMount && !m.IsRunning(c.ID) {
			_ = m.Start(c)
		}
	}
}

// StartAllAuto starts every connection marked auto-mount.
func (m *Manager) StartAllAuto() { m.Reconcile() }

func tailLog(path string, n int) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return ""
	}
	size := fi.Size()
	start := int64(0)
	if size > int64(n) {
		start = size - int64(n)
	}
	buf := make([]byte, size-start)
	if _, err := f.ReadAt(buf, start); err != nil {
		return ""
	}
	return strings.TrimSpace(string(buf))
}

func processExited(cmd *exec.Cmd) bool {
	if cmd == nil || cmd.ProcessState != nil {
		return true
	}
	return false
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
