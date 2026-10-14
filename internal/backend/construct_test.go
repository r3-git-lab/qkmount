package backend

import "testing"

// TestAllBackendsConstruct verifies every registered backend can be built from
// options without panicking. It does not touch the network (Init is not
// called), so it safely covers the cloud backends whose credentials are not
// available in CI.
func TestAllBackendsConstruct(t *testing.T) {
	cases := []struct {
		kind string
		opts map[string]string
	}{
		{"local", map[string]string{"path": t.TempDir()}},
		{"sftp", map[string]string{"host": "example.com", "user": "u", "password": "p"}},
		{"ftp", map[string]string{"host": "example.com", "user": "u", "password": "p"}},
		{"ftps", map[string]string{"host": "example.com", "user": "u", "password": "p"}},
		{"webdav", map[string]string{"url": "https://example.com/dav", "user": "u", "password": "p"}},
		{"s3", map[string]string{"bucket": "b", "access_key": "a", "secret_key": "s", "region": "us-east-1"}},
		{"gdrive", map[string]string{"token": `{"access_token":"x"}`}},
		{"dropbox", map[string]string{"token": "x"}},
		{"onedrive", map[string]string{"token": `{"access_token":"x"}`}},
		{"b2", map[string]string{"account_id": "a", "key": "k", "bucket": "b"}},
		{"azure", map[string]string{"account": "a", "key": "aGVsbG8=", "container": "c"}},
		{"smb", map[string]string{"host": "h", "share": "s", "user": "u", "password": "p"}},
	}
	for _, c := range cases {
		t.Run(c.kind, func(t *testing.T) {
			be, err := Build(c.kind, "test", c.opts)
			if err != nil {
				t.Fatalf("Build(%q): %v", c.kind, err)
			}
			if be == nil {
				t.Fatalf("Build(%q) returned nil backend", c.kind)
			}
			// The common Backend contract must be satisfied.
			var _ Backend = be
		})
	}
}
