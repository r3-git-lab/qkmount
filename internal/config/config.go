// Package config persists QKMount's connections and settings as a small JSON
// file (mode 0600) under the user's config directory.
package config

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"qkmount/internal/backend"
)

// Sensitive option keys are redacted before a connection is sent to the UI.
var sensitiveKeys = map[string]bool{
	"password": true, "pass": true, "secret": true, "secret_key": true,
	"access_key": false, // access keys are not secret on their own
	"token":      true, "refresh_token": true, "client_secret": true, "key": true,
	"private_key": true, "passphrase": true,
}

// Connection is a single saved remote/server.
type Connection struct {
	ID         string            `json:"id"`
	Name       string            `json:"name"`
	Type       string            `json:"type"`
	Options    map[string]string `json:"options"`
	MountPoint string            `json:"mountpoint"`
	AutoMount  bool              `json:"auto_mount"`
	ReadOnly   bool              `json:"read_only"`
	CreatedAt  string            `json:"created_at"`
}

// Settings holds global preferences.
type Settings struct {
	MountRoot string `json:"mount_root"`
	WebPort   int    `json:"web_port"`
	WebBind   string `json:"web_bind"`
}

// Config is the on-disk document.
type Config struct {
	Connections []*Connection `json:"connections"`
	Settings    Settings      `json:"settings"`
}

// Store loads and saves the config file.
type Store struct {
	Path string
	data Config
}

// DefaultDir returns ~/.config/qkmount (or the platform equivalent).
func DefaultDir() string {
	base, err := os.UserConfigDir()
	if err != nil {
		home, _ := os.UserHomeDir()
		base = filepath.Join(home, ".config")
	}
	return filepath.Join(base, "qkmount")
}

// DefaultMountRoot returns ~/QKMount.
func DefaultMountRoot() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return filepath.Join(os.TempDir(), "QKMount")
	}
	return filepath.Join(home, "QKMount")
}

// Open loads the store, creating defaults if the file does not exist.
func Open(dir string) (*Store, error) {
	if dir == "" {
		dir = DefaultDir()
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	s := &Store{Path: filepath.Join(dir, "config.json")}
	s.data = Config{
		Settings: Settings{MountRoot: DefaultMountRoot(), WebPort: 8321, WebBind: "127.0.0.1"},
	}
	raw, err := os.ReadFile(s.Path)
	if errors.Is(err, os.ErrNotExist) {
		return s, nil
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(raw, &s.data); err != nil {
		return nil, err
	}
	if s.data.Settings.MountRoot == "" {
		s.data.Settings.MountRoot = DefaultMountRoot()
	}
	if s.data.Settings.WebPort == 0 {
		s.data.Settings.WebPort = 8321
	}
	if s.data.Settings.WebBind == "" {
		s.data.Settings.WebBind = "127.0.0.1"
	}
	return s, nil
}

// Save writes the file with restrictive permissions.
func (s *Store) Save() error {
	raw, err := json.MarshalIndent(&s.data, "", "  ")
	if err != nil {
		return err
	}
	tmp := s.Path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, s.Path)
}

// Reload re-reads the config file so a running daemon sees changes made by the
// command line tool (or another process).
func (s *Store) Reload() error {
	raw, err := os.ReadFile(s.Path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	var fresh Config
	if err := json.Unmarshal(raw, &fresh); err != nil {
		return err
	}
	if fresh.Settings.MountRoot == "" {
		fresh.Settings.MountRoot = s.data.Settings.MountRoot
	}
	if fresh.Settings.WebPort == 0 {
		fresh.Settings.WebPort = s.data.Settings.WebPort
	}
	if fresh.Settings.WebBind == "" {
		fresh.Settings.WebBind = s.data.Settings.WebBind
	}
	s.data = fresh
	return nil
}

// Settings returns current settings.
func (s *Store) Settings() Settings { return s.data.Settings }

// SetSettings replaces settings.
func (s *Store) SetSettings(v Settings) { s.data.Settings = v }

// List returns connections sorted by name.
func (s *Store) List() []*Connection {
	out := make([]*Connection, len(s.data.Connections))
	copy(out, s.data.Connections)
	sort.Slice(out, func(i, j int) bool {
		return strings.ToLower(out[i].Name) < strings.ToLower(out[j].Name)
	})
	return out
}

// Get finds a connection by id or name.
func (s *Store) Get(idOrName string) *Connection {
	for _, c := range s.data.Connections {
		if c.ID == idOrName || c.Name == idOrName {
			return c
		}
	}
	return nil
}

// Add inserts a connection, assigning an ID and default mount point.
func (s *Store) Add(c *Connection) error {
	if c.Name == "" {
		return errors.New("name is required")
	}
	if c.ID == "" {
		c.ID = newID()
	}
	if c.Options == nil {
		c.Options = map[string]string{}
	}
	if c.MountPoint == "" {
		c.MountPoint = filepath.Join(s.data.Settings.MountRoot, sanitize(c.Name))
	}
	s.data.Connections = append(s.data.Connections, c)
	return s.Save()
}

// Update replaces an existing connection by ID.
//
// Options are merged rather than replaced: keys the caller did not send are
// kept, and a blank value for a sensitive key (token, password, ...) means
// "unchanged". This matters because the UI edit form only carries the fields
// it displays — an OAuth token is never shown — so a plain rename must not
// drop the stored token.
func (s *Store) Update(c *Connection) error {
	for i, existing := range s.data.Connections {
		if existing.ID == c.ID {
			merged := map[string]string{}
			for k, v := range existing.Options {
				merged[k] = v
			}
			for k, v := range c.Options {
				if v == "" && sensitiveKeys[k] {
					continue // blank secret means "leave it as it was"
				}
				merged[k] = v
			}
			c.Options = merged
			s.data.Connections[i] = c
			return s.Save()
		}
	}
	return errors.New("connection not found: " + c.ID)
}

// Remove deletes a connection by ID.
func (s *Store) Remove(id string) error {
	out := s.data.Connections[:0]
	found := false
	for _, c := range s.data.Connections {
		if c.ID == id {
			found = true
			continue
		}
		out = append(out, c)
	}
	if !found {
		return errors.New("connection not found: " + id)
	}
	s.data.Connections = out
	return s.Save()
}

// Redacted returns a copy safe to expose to the UI (secrets replaced by "*").
func (c *Connection) Redacted() *Connection {
	cp := *c
	cp.Options = map[string]string{}
	for k, v := range c.Options {
		if sensitiveKeys[k] && v != "" {
			cp.Options[k] = ""
			cp.Options[k+"__set"] = "1"
			continue
		}
		cp.Options[k] = v
	}
	return &cp
}

func sanitize(name string) string {
	repl := strings.NewReplacer("/", "-", "\\", "-", ":", "-", "\x00", "")
	return strings.TrimSpace(repl.Replace(name))
}

func newID() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// BuildBackend is wired up by package backend's registry; declared here to
// avoid an import cycle.  Set from main.
var BuildBackend func(c *Connection) (backend.Backend, error)
