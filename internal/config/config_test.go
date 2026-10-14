package config

import (
	"testing"
)

// TestUpdatePreservesUnsentSecrets is a regression test: the web UI edit form
// does not carry an OAuth token, so renaming a connection must not drop it.
func TestUpdatePreservesUnsentSecrets(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	c := &Connection{Name: "Google Drive", Type: "gdrive", Options: map[string]string{
		"token": "SECRET-TOKEN",
		"user":  "bob",
	}}
	if err := s.Add(c); err != nil {
		t.Fatal(err)
	}

	// Simulate the UI payload for a rename: only the visible fields are sent.
	edited := &Connection{
		ID:   c.ID,
		Name: "AF Drive",
		Type: "gdrive",
		// client_id/client_secret are the gdrive form fields; blank here.
		Options: map[string]string{"client_id": "", "client_secret": ""},
	}
	if err := s.Update(edited); err != nil {
		t.Fatal(err)
	}

	got := s.Get(c.ID)
	if got == nil {
		t.Fatal("connection vanished")
	}
	if got.Name != "AF Drive" {
		t.Fatalf("name = %q, want AF Drive", got.Name)
	}
	if got.Options["token"] != "SECRET-TOKEN" {
		t.Fatalf("token lost on rename: %q", got.Options["token"])
	}
	if got.Options["user"] != "bob" {
		t.Fatalf("non-form option lost: %q", got.Options["user"])
	}
}

// TestUpdateBlankSemantics verifies that a blank sensitive value means
// "unchanged", while a blank non-sensitive value clears the field.
func TestUpdateBlankSemantics(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	c := &Connection{Name: "srv", Type: "sftp", Options: map[string]string{
		"password": "hunter2",
		"user":     "bob",
	}}
	if err := s.Add(c); err != nil {
		t.Fatal(err)
	}

	edited := &Connection{
		ID:      c.ID,
		Name:    "srv",
		Type:    "sftp",
		Options: map[string]string{"password": "", "user": ""},
	}
	if err := s.Update(edited); err != nil {
		t.Fatal(err)
	}

	got := s.Get(c.ID)
	if got.Options["password"] != "hunter2" {
		t.Fatalf("blank password should mean unchanged, got %q", got.Options["password"])
	}
	if got.Options["user"] != "" {
		t.Fatalf("blank non-sensitive value should clear, got %q", got.Options["user"])
	}
}
