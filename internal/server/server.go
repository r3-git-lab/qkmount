// Package server exposes QKMount's connections and mount status over a small
// local HTTP API and serves the embedded web UI.
package server

import (
	"embed"
	"encoding/json"
	"fmt"
	"io/fs"
	"net/http"
	"os/exec"
	"runtime"
	"strings"
	"time"

	"qkmount/internal/backend"
	"qkmount/internal/config"
	"qkmount/internal/mount"
)

//go:embed web/*
var webFS embed.FS

// Server wires the store and mount manager to HTTP handlers.
type Server struct {
	Store *config.Store
	Mgr   *mount.Manager

	// Quit, when set, is invoked by POST /api/quit to stop the daemon.
	Quit func()

	// oauth tracks the single in-flight browser sign-in.
	oauth oauthSession
}

// Handler builds the HTTP mux.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()

	// Each request reloads the config so CLI changes are picked up.
	reload := func(next http.HandlerFunc) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			_ = s.Store.Reload()
			next(w, r)
		}
	}

	mux.HandleFunc("GET /api/connections", reload(s.listConnections))
	mux.HandleFunc("POST /api/connections", reload(s.createConnection))
	mux.HandleFunc("PUT /api/connections/{id}", reload(s.updateConnection))
	mux.HandleFunc("DELETE /api/connections/{id}", reload(s.deleteConnection))
	mux.HandleFunc("POST /api/connections/{id}/mount", reload(s.mountConn))
	mux.HandleFunc("POST /api/connections/{id}/unmount", reload(s.unmountConn))
	mux.HandleFunc("POST /api/connections/{id}/open", reload(s.openConn))
	mux.HandleFunc("GET /api/status", reload(s.status))
	mux.HandleFunc("GET /api/kinds", reload(s.kinds))
	mux.HandleFunc("GET /api/settings", reload(s.getSettings))
	mux.HandleFunc("PUT /api/settings", reload(s.putSettings))
	mux.HandleFunc("POST /api/quit", reload(s.quit))

	// Interactive browser sign-in (Google Drive, etc.).
	mux.HandleFunc("POST /api/oauth/{kind}/start", reload(s.oauthStart))
	mux.HandleFunc("GET /api/oauth/{kind}/status", s.oauthStatus)
	mux.HandleFunc("POST /api/oauth/{kind}/cancel", s.oauthCancel)

	// Static UI.
	sub, _ := fs.Sub(webFS, "web")
	mux.Handle("/", http.FileServer(http.FS(sub)))
	return mux
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, code int, err error) {
	writeJSON(w, code, map[string]string{"error": err.Error()})
}

func (s *Server) listConnections(w http.ResponseWriter, r *http.Request) {
	conns := s.Store.List()
	out := make([]*config.Connection, 0, len(conns))
	for _, c := range conns {
		out = append(out, c.Redacted())
	}
	writeJSON(w, 200, out)
}

func (s *Server) createConnection(w http.ResponseWriter, r *http.Request) {
	var c config.Connection
	if err := json.NewDecoder(r.Body).Decode(&c); err != nil {
		writeErr(w, 400, err)
		return
	}
	if c.Type == "" {
		writeErr(w, 400, fmt.Errorf("type is required"))
		return
	}
	if err := s.Store.Add(&c); err != nil {
		writeErr(w, 400, err)
		return
	}
	if c.AutoMount {
		_ = s.Mgr.Start(&c)
	}
	writeJSON(w, 201, c.Redacted())
}

func (s *Server) updateConnection(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	existing := s.Store.Get(id)
	if existing == nil {
		writeErr(w, 404, fmt.Errorf("not found"))
		return
	}
	var c config.Connection
	if err := json.NewDecoder(r.Body).Decode(&c); err != nil {
		writeErr(w, 400, err)
		return
	}
	c.ID = existing.ID
	if c.MountPoint == "" {
		c.MountPoint = existing.MountPoint
	}
	wasRunning := s.Mgr.IsRunning(existing.ID)
	if wasRunning {
		_ = s.Mgr.Stop(existing.ID)
	}
	if err := s.Store.Update(&c); err != nil {
		writeErr(w, 400, err)
		return
	}
	if wasRunning {
		_ = s.Mgr.Start(&c)
	}
	writeJSON(w, 200, c.Redacted())
}

func (s *Server) deleteConnection(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	_ = s.Mgr.Stop(id)
	if err := s.Store.Remove(id); err != nil {
		writeErr(w, 404, err)
		return
	}
	writeJSON(w, 200, map[string]bool{"ok": true})
}

func (s *Server) mountConn(w http.ResponseWriter, r *http.Request) {
	c := s.Store.Get(r.PathValue("id"))
	if c == nil {
		writeErr(w, 404, fmt.Errorf("not found"))
		return
	}
	if err := s.Mgr.Start(c); err != nil {
		writeErr(w, 500, err)
		return
	}
	writeJSON(w, 200, map[string]bool{"ok": true})
}

func (s *Server) unmountConn(w http.ResponseWriter, r *http.Request) {
	if err := s.Mgr.Stop(r.PathValue("id")); err != nil {
		writeErr(w, 500, err)
		return
	}
	writeJSON(w, 200, map[string]bool{"ok": true})
}

func (s *Server) status(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, 200, s.Mgr.Snapshot())
}

func (s *Server) kinds(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, 200, backend.Kinds())
}

// openConn opens the connection's mount point in the desktop file manager.
func (s *Server) openConn(w http.ResponseWriter, r *http.Request) {
	c := s.Store.Get(r.PathValue("id"))
	if c == nil {
		writeErr(w, 404, fmt.Errorf("not found"))
		return
	}
	path := c.MountPoint
	if path == "" {
		writeErr(w, 400, fmt.Errorf("no mount point"))
		return
	}
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.Command("open", path)
	case "windows":
		cmd = exec.Command("explorer", path)
	default:
		cmd = exec.Command("xdg-open", path)
	}
	if err := cmd.Start(); err != nil {
		writeErr(w, 500, err)
		return
	}
	writeJSON(w, 200, map[string]bool{"ok": true})
}

func (s *Server) getSettings(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, 200, s.Store.Settings())
}

func (s *Server) putSettings(w http.ResponseWriter, r *http.Request) {
	var v config.Settings
	if err := json.NewDecoder(r.Body).Decode(&v); err != nil {
		writeErr(w, 400, err)
		return
	}
	if v.MountRoot == "" {
		v.MountRoot = s.Store.Settings().MountRoot
	}
	if v.WebPort == 0 {
		v.WebPort = s.Store.Settings().WebPort
	}
	if strings.TrimSpace(v.WebBind) == "" {
		v.WebBind = "127.0.0.1"
	}
	s.Store.SetSettings(v)
	if err := s.Store.Save(); err != nil {
		writeErr(w, 500, err)
		return
	}
	writeJSON(w, 200, v)
}

func (s *Server) quit(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, 200, map[string]bool{"ok": true})
	go func() {
		time.Sleep(150 * time.Millisecond)
		if s.Quit != nil {
			s.Quit()
		}
	}()
}

// ListenAndServe starts the UI/API server.
func (s *Server) ListenAndServe(bind string, port int) error {
	addr := fmt.Sprintf("%s:%d", bind, port)
	return http.ListenAndServe(addr, s.Handler())
}
