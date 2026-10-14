package backend

import (
	"bytes"
	"io"
	"net/http/httptest"
	"testing"

	"golang.org/x/net/webdav"
)

// TestWebDAVLifecycle exercises the WebDAV backend against an in-process
// WebDAV server (no external network needed).
func TestWebDAVLifecycle(t *testing.T) {
	dir := t.TempDir()
	h := &webdav.Handler{
		Prefix:     "/",
		FileSystem: webdav.Dir(dir),
		LockSystem: webdav.NewMemLS(),
	}
	srv := httptest.NewServer(h)
	defer srv.Close()

	be, err := Build("webdav", "test", map[string]string{"url": srv.URL})
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if err := be.Init(); err != nil {
		t.Fatalf("init: %v", err)
	}

	// mkdir
	if err := be.Mkdir("/docs", 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	a, err := be.Stat("/docs")
	if err != nil || !a.IsDir {
		t.Fatalf("stat docs: %+v err=%v", a, err)
	}

	// write a file
	content := []byte("hello webdav world")
	w, err := be.OpenWrite("/docs/a.txt", true)
	if err != nil {
		t.Fatalf("openwrite: %v", err)
	}
	if _, err := w.WriteAt(content, 0); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	// read it back, in two ranges
	a, err = be.Stat("/docs/a.txt")
	if err != nil {
		t.Fatalf("stat file: %v", err)
	}
	if a.Size != int64(len(content)) {
		t.Fatalf("size = %d, want %d", a.Size, len(content))
	}
	rh, err := be.OpenRead("/docs/a.txt")
	if err != nil {
		t.Fatalf("openread: %v", err)
	}
	got := make([]byte, a.Size)
	if _, err := rh.ReadAt(got, 0); err != nil && err != io.EOF {
		t.Fatalf("readat: %v", err)
	}
	rh.Close()
	if !bytes.Equal(got, content) {
		t.Fatalf("read = %q, want %q", got, content)
	}

	// a range in the middle
	rh, _ = be.OpenRead("/docs/a.txt")
	mid := make([]byte, 5)
	if _, err := rh.ReadAt(mid, 6); err != nil && err != io.EOF {
		t.Fatalf("mid read: %v", err)
	}
	rh.Close()
	if string(mid) != "webda" {
		t.Fatalf("mid = %q", mid)
	}

	// list the directory
	names, err := be.ReadDir("/docs")
	if err != nil {
		t.Fatalf("readdir: %v", err)
	}
	found := false
	for _, e := range names {
		if e.Name == "a.txt" && !e.IsDir {
			found = true
		}
	}
	if !found {
		t.Fatalf("a.txt not listed: %+v", names)
	}

	// rename
	if err := be.Rename("/docs/a.txt", "/docs/b.txt"); err != nil {
		t.Fatalf("rename: %v", err)
	}
	if _, err := be.Stat("/docs/b.txt"); err != nil {
		t.Fatalf("stat renamed: %v", err)
	}
	if _, err := be.Stat("/docs/a.txt"); err == nil {
		t.Fatalf("old name still exists")
	}

	// remove
	if err := be.Remove("/docs/b.txt"); err != nil {
		t.Fatalf("remove: %v", err)
	}
	if err := be.RemoveDir("/docs"); err != nil {
		t.Fatalf("rmdir: %v", err)
	}
	if _, err := be.Stat("/docs"); err == nil {
		t.Fatalf("docs still exists")
	}
}
