package server

import (
	"testing"

	"qkmount/internal/config"
	"qkmount/internal/oauthflow"
)

func newTestServer(t *testing.T) *Server {
	t.Helper()
	st, err := config.Open(t.TempDir())
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	return &Server{Store: st}
}

// Re-authorising the same account must update the existing connection in place,
// not create "name (2)".
func TestSaveOAuthConnectionUpdatesInPlace(t *testing.T) {
	s := newTestServer(t)
	first := &config.Connection{
		Name:    "me@example.com",
		Type:    "gdrive",
		Options: map[string]string{"token": "old-token"},
	}
	if err := s.Store.Add(first); err != nil {
		t.Fatal(err)
	}

	prov := oauthflow.Provider{
		Kind:   "gdrive",
		Label:  "Google Drive",
		Params: oauthflow.Params{ClientID: "custom-client", ClientSecret: "cs"},
	}
	id, name, updated, err := s.saveOAuthConnection(prov, "me@example.com", map[string]string{
		"token":         "new-token",
		"client_id":     "custom-client",
		"client_secret": "cs",
	})
	if err != nil {
		t.Fatalf("save: %v", err)
	}
	if !updated {
		t.Fatal("expected an in-place update")
	}
	if id != first.ID || name != "me@example.com" {
		t.Fatalf("wrong identity: id=%q name=%q (want %q)", id, name, first.ID)
	}
	if conns := s.Store.List(); len(conns) != 1 {
		t.Fatalf("expected 1 connection, got %d", len(conns))
	}
	got := s.Store.Get("me@example.com")
	if got.Options["token"] != "new-token" {
		t.Fatalf("token not refreshed: %q", got.Options["token"])
	}
	if got.Options["client_id"] != "custom-client" || got.Options["client_secret"] != "cs" {
		t.Fatalf("private client not persisted: %+v", got.Options)
	}
}

// Reverting to the built-in shared client must clear any previously stored
// private client so refreshes use the shared credentials again.
func TestSaveOAuthConnectionRevertsPrivateClient(t *testing.T) {
	s := newTestServer(t)
	c := &config.Connection{
		Name:    "me@example.com",
		Type:    "gdrive",
		Options: map[string]string{"token": "old", "client_id": "old-client", "client_secret": "old-secret"},
	}
	if err := s.Store.Add(c); err != nil {
		t.Fatal(err)
	}
	prov := oauthflow.Provider{
		Kind:   "gdrive",
		Label:  "Google Drive",
		Params: oauthflow.Params{ClientID: oauthflow.DefaultGoogleClientID},
	}
	if _, _, _, err := s.saveOAuthConnection(prov, "me@example.com", map[string]string{"token": "new"}); err != nil {
		t.Fatal(err)
	}
	got := s.Store.Get("me@example.com")
	if _, ok := got.Options["client_id"]; ok {
		t.Fatalf("stale client_id not cleared: %+v", got.Options)
	}
	if _, ok := got.Options["client_secret"]; ok {
		t.Fatalf("stale client_secret not cleared: %+v", got.Options)
	}
}

// A different account must still create a new connection.
func TestSaveOAuthConnectionAddsNew(t *testing.T) {
	s := newTestServer(t)
	if err := s.Store.Add(&config.Connection{Name: "other@example.com", Type: "gdrive"}); err != nil {
		t.Fatal(err)
	}
	prov := oauthflow.Provider{Kind: "gdrive", Label: "Google Drive", Params: oauthflow.Params{ClientID: "custom"}}
	id, name, updated, err := s.saveOAuthConnection(prov, "me@example.com", map[string]string{"token": "t"})
	if err != nil {
		t.Fatal(err)
	}
	if updated {
		t.Fatal("did not expect an update for a new account")
	}
	if id == "" || name != "me@example.com" {
		t.Fatalf("unexpected new connection: id=%q name=%q", id, name)
	}
	if conns := s.Store.List(); len(conns) != 2 {
		t.Fatalf("expected 2 connections, got %d", len(conns))
	}
}
