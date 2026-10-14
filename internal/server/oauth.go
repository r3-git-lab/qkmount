package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sync"

	"qkmount/internal/backend"
	"qkmount/internal/config"
	"qkmount/internal/oauthflow"
)

// oauthSession tracks the single in-flight browser sign-in. Only one runs at a
// time; starting another while one is active returns the active one.
type oauthSession struct {
	mu      sync.Mutex
	kind    string
	status  string // "", "running", "done", "error"
	authURL string
	message string
	connID  string
	cancel  context.CancelFunc
}

type oauthStatus struct {
	Status       string `json:"status"`
	Kind         string `json:"kind,omitempty"`
	AuthURL      string `json:"auth_url,omitempty"`
	Message      string `json:"message,omitempty"`
	ConnectionID string `json:"connection_id,omitempty"`
}

func (o *oauthSession) snapshot() oauthStatus {
	o.mu.Lock()
	defer o.mu.Unlock()
	return oauthStatus{Status: o.status, Kind: o.kind, AuthURL: o.authURL, Message: o.message, ConnectionID: o.connID}
}

func (s *Server) oauthStart(w http.ResponseWriter, r *http.Request) {
	kind := r.PathValue("kind")
	prov, err := oauthflow.ProviderParams(kind)
	if err != nil {
		writeErr(w, 400, err)
		return
	}
	name := r.URL.Query().Get("name")
	// Allow the UI to supply a private OAuth client so a distribution or a
	// power user can avoid the shared, rate-limited default client.
	var body struct {
		ClientID     string `json:"client_id"`
		ClientSecret string `json:"client_secret"`
		Name         string `json:"name"`
	}
	if r.Body != nil {
		_ = json.NewDecoder(r.Body).Decode(&body)
	}
	if body.ClientID != "" {
		prov.Params.ClientID = body.ClientID
		prov.Params.ClientSecret = body.ClientSecret
	}
	if body.Name != "" {
		name = body.Name
	}

	s.oauth.mu.Lock()
	if s.oauth.status == "running" {
		snap := oauthStatus{Status: s.oauth.status, Kind: s.oauth.kind, AuthURL: s.oauth.authURL, Message: s.oauth.message, ConnectionID: s.oauth.connID}
		s.oauth.mu.Unlock()
		writeJSON(w, 200, snap)
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	s.oauth.kind = prov.Kind
	s.oauth.status = "running"
	s.oauth.authURL = ""
	s.oauth.message = "Waiting for you to finish signing in."
	s.oauth.connID = ""
	s.oauth.cancel = cancel
	s.oauth.mu.Unlock()

	go s.runOAuth(ctx, prov, name)
	writeJSON(w, 202, s.oauth.snapshot())
}

func (s *Server) oauthStatus(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, 200, s.oauth.snapshot())
}

func (s *Server) oauthCancel(w http.ResponseWriter, r *http.Request) {
	s.oauth.mu.Lock()
	if s.oauth.cancel != nil {
		s.oauth.cancel()
	}
	s.oauth.status = ""
	s.oauth.message = "Sign-in cancelled."
	s.oauth.mu.Unlock()
	writeJSON(w, 200, map[string]bool{"ok": true})
}

func (s *Server) runOAuth(ctx context.Context, prov oauthflow.Provider, name string) {
	fail := func(err error) {
		// If the user cancelled, the cancel handler already set the state;
		// don't overwrite it with "context canceled".
		if ctx.Err() != nil {
			return
		}
		s.oauth.mu.Lock()
		s.oauth.status = "error"
		s.oauth.message = err.Error()
		s.oauth.mu.Unlock()
	}

	res, err := oauthflow.Run(ctx, prov.Params, func(u string) error {
		s.oauth.mu.Lock()
		s.oauth.authURL = u
		s.oauth.mu.Unlock()
		return oauthflow.OpenBrowser(u)
	})
	if err != nil {
		fail(err)
		return
	}
	tokenJSON, err := oauthflow.TokenJSON(res.Token)
	if err != nil {
		fail(err)
		return
	}

	opts := map[string]string{"token": tokenJSON}
	if prov.Kind == "gdrive" {
		if email, e := backend.GoogleAccountEmail(tokenJSON, prov.Params.ClientID, prov.Params.ClientSecret); e == nil && email != "" {
			if name == "" {
				name = email
			}
		}
		if prov.Params.ClientID != oauthflow.DefaultGoogleClientID {
			opts["client_id"] = prov.Params.ClientID
			opts["client_secret"] = prov.Params.ClientSecret
		}
	}
	connID, displayName, updated, err := s.saveOAuthConnection(prov, name, opts)
	if err != nil {
		fail(err)
		return
	}
	s.oauth.mu.Lock()
	s.oauth.status = "done"
	s.oauth.connID = connID
	if updated {
		s.oauth.message = fmt.Sprintf("Updated %q", displayName)
	} else {
		s.oauth.message = fmt.Sprintf("Added %q", displayName)
	}
	s.oauth.mu.Unlock()
}

// saveOAuthConnection stores the result of a successful sign-in. If a
// connection with the same account name and type already exists it is updated
// in place (and remounted with the fresh credentials) so that re-authorising
// with your own OAuth client does not pile up duplicates like "name (2)".
func (s *Server) saveOAuthConnection(prov oauthflow.Provider, name string, opts map[string]string) (id, displayName string, updated bool, err error) {
	_ = s.Store.Reload()
	if name != "" {
		if existing := s.Store.Get(name); existing != nil && existing.Type == prov.Kind {
			u := *existing
			if u.Options == nil {
				u.Options = map[string]string{}
			}
			for k, v := range opts {
				u.Options[k] = v
			}
			if prov.Kind == "gdrive" && prov.Params.ClientID == oauthflow.DefaultGoogleClientID {
				// Reverting to the shared client: drop any stale private client.
				delete(u.Options, "client_id")
				delete(u.Options, "client_secret")
			}
			running := s.Mgr != nil && s.Mgr.IsRunning(existing.ID)
			if running {
				_ = s.Mgr.Stop(existing.ID)
			}
			if err := s.Store.Update(&u); err != nil {
				return "", "", false, err
			}
			if running {
				_ = s.Mgr.Start(&u)
			}
			return u.ID, u.Name, true, nil
		}
	}
	if name == "" {
		name = prov.Label
	}
	name = s.uniqueName(name)
	c := &config.Connection{Name: name, Type: prov.Kind, Options: opts}
	if err := s.Store.Add(c); err != nil {
		return "", "", false, err
	}
	return c.ID, c.Name, false, nil
}

func (s *Server) uniqueName(base string) string {
	_ = s.Store.Reload()
	if s.Store.Get(base) == nil {
		return base
	}
	for i := 2; i < 1000; i++ {
		cand := fmt.Sprintf("%s (%d)", base, i)
		if s.Store.Get(cand) == nil {
			return cand
		}
	}
	return base
}
