package rclone

import (
	"os"
	"path/filepath"
	"testing"
)

func TestParse(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "rclone.conf")
	content := `; a comment
[My Drive]
type = drive
token = {"access_token":"tok","refresh_token":"ref"}
team_drive = 

[Box]
type = sftp
host = example.com
user = alice
pass = obscuredvalue
port = 2222
`
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	remotes, err := Parse(path)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(remotes) != 2 {
		t.Fatalf("got %d remotes, want 2", len(remotes))
	}
	if remotes[0].Name != "My Drive" || remotes[0].Items["type"] != "drive" {
		t.Fatalf("remote 0 = %+v", remotes[0])
	}
	if remotes[1].Items["host"] != "example.com" || remotes[1].Items["port"] != "2222" {
		t.Fatalf("remote 1 = %+v", remotes[1])
	}
}

func TestConvert(t *testing.T) {
	drive, ok := Convert(&Remote{Name: "D", Items: map[string]string{
		"type":  "drive",
		"token": `{"access_token":"t"}`,
	}})
	if !ok || drive.Type != "gdrive" {
		t.Fatalf("drive convert: ok=%v c=%+v", ok, drive)
	}
	if drive.Options["token"] == "" {
		t.Fatalf("drive token not mapped")
	}

	sftp, ok := Convert(&Remote{Name: "S", Items: map[string]string{
		"type": "sftp", "host": "h", "user": "u", "pass": "notobscured",
	}})
	if !ok || sftp.Type != "sftp" {
		t.Fatalf("sftp convert: ok=%v c=%+v", ok, sftp)
	}
	if sftp.Options["host"] != "h" || sftp.Options["user"] != "u" {
		t.Fatalf("sftp options: %+v", sftp.Options)
	}

	if _, ok := Convert(&Remote{Name: "X", Items: map[string]string{"type": "unknownthing"}}); ok {
		t.Fatalf("unknown type should not convert")
	}
}

// TestRevealRoundTrip checks that Reveal reverses an obfuscated value produced
// with rclone's algorithm (random IV + AES-CTR with the fixed key).
func TestRevealRoundTrip(t *testing.T) {
	const plain = "S3cret-P@ssword!"
	obscured := obscureForTest(t, plain)
	got := Reveal(obscured)
	if got != plain {
		t.Fatalf("Reveal = %q, want %q", got, plain)
	}
	// A value that is not valid base64 is returned unchanged.
	if Reveal("plaintext with spaces") != "plaintext with spaces" {
		t.Fatalf("plaintext should pass through unchanged")
	}
	if Reveal("") != "" {
		t.Fatalf("empty should stay empty")
	}
}
