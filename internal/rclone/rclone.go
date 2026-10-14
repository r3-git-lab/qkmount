// Package rclone reads rclone configuration files and converts remotes into
// QKMount connections, so existing rclone setups keep working without
// re-authenticating.
package rclone

import (
	"bufio"
	"crypto/aes"
	"crypto/cipher"
	"encoding/base64"
	"errors"
	"os"
	"path/filepath"
	"strings"

	"qkmount/internal/config"
)

// cryptKey is the fixed key rclone uses to obfuscate secrets in its config.
var cryptKey = []byte{
	0x9c, 0x93, 0x5b, 0x48, 0x73, 0x0a, 0x55, 0x4d,
	0x6b, 0xfd, 0x7c, 0x63, 0xc8, 0x86, 0xa9, 0x2b,
	0xd3, 0x90, 0x19, 0x8e, 0xb8, 0x12, 0x8a, 0xfb,
	0xf4, 0xde, 0x16, 0x2b, 0x8b, 0x95, 0xf6, 0x38,
}

// DefaultPath returns the default rclone config location.
func DefaultPath() string {
	if v := os.Getenv("RCLONE_CONFIG"); v != "" {
		return v
	}
	base, err := os.UserConfigDir()
	if err != nil {
		home, _ := os.UserHomeDir()
		base = filepath.Join(home, ".config")
	}
	return filepath.Join(base, "rclone", "rclone.conf")
}

// Remote is a named group of key/value items from an rclone config.
type Remote struct {
	Name  string
	Items map[string]string
}

// Parse reads an rclone config file into remotes, in file order.
func Parse(path string) ([]*Remote, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	var remotes []*Remote
	var cur *Remote
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, ";") {
			continue
		}
		if strings.HasPrefix(line, "[") && strings.HasSuffix(line, "]") {
			cur = &Remote{Name: strings.TrimSuffix(strings.TrimPrefix(line, "["), "]"), Items: map[string]string{}}
			remotes = append(remotes, cur)
			continue
		}
		if cur == nil {
			continue
		}
		i := strings.Index(line, "=")
		if i < 0 {
			continue
		}
		k := strings.TrimSpace(line[:i])
		v := strings.TrimSpace(line[i+1:])
		cur.Items[k] = v
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	return remotes, nil
}

// Reveal reverses rclone's password obfuscation. If the value is not obscured
// (not valid base64, or too short) it is returned unchanged.
func Reveal(s string) string {
	if s == "" {
		return ""
	}
	raw, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil || len(raw) < aes.BlockSize {
		return s
	}
	block, err := aes.NewCipher(cryptKey)
	if err != nil {
		return s
	}
	iv := raw[:aes.BlockSize]
	ct := raw[aes.BlockSize:]
	out := make([]byte, len(ct))
	cipher.NewCTR(block, iv).XORKeyStream(out, ct)
	// Reject results with control characters (a sign the input was plaintext).
	for _, b := range out {
		if b < 0x09 || (b > 0x0d && b < 0x20) {
			return s
		}
	}
	return string(out)
}

// Convert maps an rclone remote onto a QKMount connection. The second return
// value is false when the remote type is not supported.
func Convert(r *Remote) (*config.Connection, bool) {
	t := strings.ToLower(r.Items["type"])
	c := &config.Connection{Name: r.Name, Options: map[string]string{}}
	set := func(k, v string) {
		if v != "" {
			c.Options[k] = v
		}
	}
	switch t {
	case "drive":
		c.Type = "gdrive"
		set("token", r.Items["token"])
		set("root_id", r.Items["team_drive"])
		set("client_id", r.Items["client_id"])
		set("client_secret", Reveal(r.Items["client_secret"]))
	case "sftp":
		c.Type = "sftp"
		set("host", r.Items["host"])
		set("port", r.Items["port"])
		set("user", r.Items["user"])
		set("password", Reveal(r.Items["pass"]))
		set("private_key", r.Items["key_file"])
		set("passphrase", Reveal(r.Items["key_file_pass"]))
		set("path", r.Items["path"])
	case "ftp":
		c.Type = "ftp"
		set("host", r.Items["host"])
		set("port", r.Items["port"])
		set("user", r.Items["user"])
		set("password", Reveal(r.Items["pass"]))
		if r.Items["explicit_tls"] == "true" {
			set("tls", "1")
		}
		set("path", r.Items["path"])
	case "webdav":
		c.Type = "webdav"
		set("url", r.Items["url"])
		set("vendor", r.Items["vendor"])
		set("user", r.Items["user"])
		set("password", Reveal(r.Items["pass"]))
		set("path", r.Items["path"])
	case "s3":
		c.Type = "s3"
		set("access_key", r.Items["access_key_id"])
		set("secret_key", Reveal(r.Items["secret_access_key"]))
		set("region", r.Items["region"])
		set("endpoint", r.Items["endpoint"])
		if r.Items["force_path_style"] == "true" {
			set("path_style", "1")
		}
	case "b2":
		c.Type = "b2"
		set("account_id", r.Items["account"])
		set("key", Reveal(r.Items["key"]))
	case "azureblob":
		c.Type = "azure"
		set("account", r.Items["account"])
		set("key", Reveal(r.Items["key"]))
	case "onedrive":
		c.Type = "onedrive"
		set("token", r.Items["token"])
		set("drive_id", r.Items["drive_id"])
	case "smb":
		c.Type = "smb"
		set("host", r.Items["host"])
		set("share", r.Items["share"])
		set("user", r.Items["user"])
		set("password", Reveal(r.Items["pass"]))
		set("domain", r.Items["domain"])
	default:
		return nil, false
	}
	if len(c.Options) == 0 {
		return c, false
	}
	return c, true
}

// ErrNoConfig indicates the rclone config file could not be found.
var ErrNoConfig = errors.New("no rclone config found")
