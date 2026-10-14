package backend

import (
	"os"
	"strings"
	"testing"
)

// Live Google Drive test. Run with:
//
//	QKMOUNT_TEST_GDRIVE_TOKEN='<token json>' go test -tags fuse3 -run TestGDriveLive ./internal/backend/ -v
//
// Skipped when the token is absent.
func TestGDriveLive(t *testing.T) {
	tok := os.Getenv("QKMOUNT_TEST_GDRIVE_TOKEN")
	if tok == "" {
		t.Skip("QKMOUNT_TEST_GDRIVE_TOKEN not set")
	}
	opts := map[string]string{"token": tok}
	st, err := newGDrive(opts)
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	if err := st.Init(); err != nil {
		t.Fatalf("init: %v", err)
	}
	name := "qk-live-test.txt"
	content := "hello live drive"
	if err := st.Put(name, strings.NewReader(content), int64(len(content))); err != nil {
		t.Fatalf("put: %v", err)
	}
	defer st.Delete(name)

	got, err := st.Get(name, 0, 0)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if string(got) != content {
		t.Fatalf("get mismatch: %q", got)
	}
	t.Logf("round-trip ok, %d bytes", len(got))

	if err := st.Mkdir("qk-live-dir"); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	defer st.RemoveDir("qk-live-dir")
	if isDir, err := st.StatDir("qk-live-dir"); err != nil || !isDir {
		t.Fatalf("statdir: isDir=%v err=%v", isDir, err)
	}
	t.Log("mkdir/statdir ok")
}
