package backend

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
)

// TestGDriveNegativeCache verifies that repeated lookups of a missing path do
// not re-list the parent directory, which is what prevents a burst of FUSE
// lookups from exhausting the provider's rate limit.
func TestGDriveNegativeCache(t *testing.T) {
	var listCalls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Only the list endpoint (/files?q=...) is interesting here.
		if strings.HasSuffix(r.URL.Path, "/files") && r.URL.Query().Get("q") != "" {
			atomic.AddInt32(&listCalls, 1)
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]any{
				"files": []map[string]any{
					{"id": "folder-1", "name": "Docs", "mimeType": folderMime},
				},
			})
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{}`))
	}))
	defer srv.Close()

	st := newTestGDrive(t, srv)

	// First lookup of a missing path lists the parent once.
	if _, err := st.resolve("/missing-1"); !os.IsNotExist(err) {
		t.Fatalf("expected not-exist, got %v", err)
	}
	after := atomic.LoadInt32(&listCalls)
	if after == 0 {
		t.Fatal("expected at least one list call for the first lookup")
	}

	// Many more lookups of other missing paths must not add list calls.
	for i := 0; i < 20; i++ {
		if _, err := st.resolve("/missing-many"); !os.IsNotExist(err) {
			t.Fatalf("expected not-exist, got %v", err)
		}
	}
	if got := atomic.LoadInt32(&listCalls); got != after {
		t.Fatalf("negative cache ineffective: list calls %d -> %d", after, got)
	}

	// A known child resolves from the cached listing without new list calls.
	e, err := st.resolve("/Docs")
	if err != nil {
		t.Fatalf("resolve known child: %v", err)
	}
	if e.ID != "folder-1" {
		t.Fatalf("wrong entry: %+v", e)
	}
	if got := atomic.LoadInt32(&listCalls); got != after {
		t.Fatalf("known-child resolve triggered a list: %d -> %d", after, got)
	}
}

// TestGDriveCreateVisibleAfterNegativeLookup is a regression test: after a
// lookup that returned "not found", creating that file must make it resolvable
// again. Otherwise the negative cache hides the file the user just wrote.
func TestGDriveCreateVisibleAfterNegativeLookup(t *testing.T) {
	var uploaded int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/files") &&
			strings.Contains(r.URL.RawQuery, "uploadType=multipart"):
			atomic.StoreInt32(&uploaded, 1)
			_, _ = io.Copy(io.Discard, r.Body)
			w.Header().Set("Content-Type", "application/json")
			w.Write([]byte(`{"id":"file-1","name":"new.txt","mimeType":"text/plain","size":"5"}`))
		case strings.HasSuffix(r.URL.Path, "/files") && r.URL.Query().Get("q") != "":
			w.Header().Set("Content-Type", "application/json")
			if atomic.LoadInt32(&uploaded) == 1 {
				w.Write([]byte(`{"files":[{"id":"file-1","name":"new.txt","mimeType":"text/plain","size":"5"}]}`))
			} else {
				w.Write([]byte(`{"files":[]}`))
			}
		default:
			w.Header().Set("Content-Type", "application/json")
			w.Write([]byte(`{}`))
		}
	}))
	defer srv.Close()

	st := newTestGDrive(t, srv)

	// Simulate the FUSE lookup that precedes a create: it reports not-found and
	// primes the negative cache for the parent.
	if _, err := st.resolve("/new.txt"); !os.IsNotExist(err) {
		t.Fatalf("expected not-exist before create, got %v", err)
	}

	if err := st.Put("/new.txt", strings.NewReader("hello"), 5); err != nil {
		t.Fatalf("put: %v", err)
	}

	// The freshly created file must be visible immediately.
	e, err := st.resolve("/new.txt")
	if err != nil {
		t.Fatalf("created file hidden by negative cache: %v", err)
	}
	if e.ID != "file-1" {
		t.Fatalf("wrong entry: %+v", e)
	}
	m, err := st.Head("new.txt")
	if err != nil {
		t.Fatalf("head: %v", err)
	}
	if m == nil {
		t.Fatal("head returned nil for a file that exists")
	}
}

func newTestGDrive(t *testing.T, srv *httptest.Server) *gdriveStore {
	t.Helper()
	resetRateLimit()
	t.Cleanup(resetRateLimit)
	oldAPI := gdriveAPIBase
	oldUp := gdriveUploadBase
	gdriveAPIBase = srv.URL
	gdriveUploadBase = srv.URL
	t.Cleanup(func() {
		gdriveAPIBase = oldAPI
		gdriveUploadBase = oldUp
	})
	st, err := newGDrive(map[string]string{"token": "test-token"})
	if err != nil {
		t.Fatal(err)
	}
	st.client = srv.Client()
	return st
}
