package backend

import (
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"
)

// mockS3 is a tiny in-memory S3-compatible server sufficient for the
// operations QKMount performs (path-style addressing).
type mockS3 struct {
	objects map[string][]byte
	modtime map[string]time.Time
}

func (m *mockS3) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	path := strings.TrimPrefix(r.URL.Path, "/bucket")
	path = strings.TrimPrefix(path, "/")
	key := path

	switch r.Method {
	case http.MethodGet:
		if r.URL.Query().Get("list-type") == "2" {
			m.list(w, r)
			return
		}
		data, ok := m.objects[key]
		if !ok {
			http.NotFound(w, r)
			return
		}
		if rng := r.Header.Get("Range"); rng != "" {
			var a, b int64
			fmt.Sscanf(rng, "bytes=%d-%d", &a, &b)
			if a >= int64(len(data)) {
				w.WriteHeader(http.StatusRequestedRangeNotSatisfiable)
				return
			}
			if b >= int64(len(data)) {
				b = int64(len(data)) - 1
			}
			w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", a, b, len(data)))
			w.WriteHeader(http.StatusPartialContent)
			w.Write(data[a : b+1])
			return
		}
		w.Write(data)
	case http.MethodHead:
		data, ok := m.objects[key]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Length", strconv.Itoa(len(data)))
		w.Header().Set("Last-Modified", m.modtime[key].UTC().Format(http.TimeFormat))
		w.WriteHeader(http.StatusOK)
	case http.MethodPut:
		if src := r.Header.Get("x-amz-copy-source"); src != "" {
			src = strings.TrimPrefix(src, "/bucket/")
			data, ok := m.objects[src]
			if !ok {
				http.NotFound(w, r)
				return
			}
			m.objects[key] = append([]byte(nil), data...)
			m.modtime[key] = time.Now()
			fmt.Fprint(w, `<CopyObjectResult><ETag>"x"</ETag></CopyObjectResult>`)
			return
		}
		body, _ := io.ReadAll(r.Body)
		m.objects[key] = body
		m.modtime[key] = time.Now()
		w.WriteHeader(http.StatusOK)
	case http.MethodDelete:
		delete(m.objects, key)
		w.WriteHeader(http.StatusNoContent)
	default:
		w.WriteHeader(http.StatusMethodNotAllowed)
	}
}

type mockListResult struct {
	XMLName        xml.Name           `xml:"ListBucketResult"`
	Name           string             `xml:"Name"`
	Prefix         string             `xml:"Prefix"`
	IsTruncated    bool               `xml:"IsTruncated"`
	Contents       []mockContent      `xml:"Contents"`
	CommonPrefixes []mockCommonPrefix `xml:"CommonPrefixes"`
}

type mockContent struct {
	XMLName      xml.Name `xml:"Contents"`
	Key          string   `xml:"Key"`
	Size         int64    `xml:"Size"`
	LastModified string   `xml:"LastModified"`
}

type mockCommonPrefix struct {
	XMLName xml.Name `xml:"CommonPrefixes"`
	Prefix  string   `xml:"Prefix"`
}

func (m *mockS3) list(w http.ResponseWriter, r *http.Request) {
	prefix := r.URL.Query().Get("prefix")
	delim := r.URL.Query().Get("delimiter")
	res := mockListResult{Name: "bucket", Prefix: prefix}
	common := map[string]bool{}
	var keys []string
	for k := range m.objects {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		if !strings.HasPrefix(k, prefix) {
			continue
		}
		rest := k[len(prefix):]
		if delim != "" {
			if i := strings.Index(rest, delim); i >= 0 {
				common[prefix+rest[:i+len(delim)]] = true
				continue
			}
		}
		if k == prefix {
			continue
		}
		res.Contents = append(res.Contents, mockContent{
			Key:          k,
			Size:         int64(len(m.objects[k])),
			LastModified: m.modtime[k].UTC().Format(time.RFC3339),
		})
	}
	for p := range common {
		res.CommonPrefixes = append(res.CommonPrefixes, mockCommonPrefix{Prefix: p})
	}
	sort.Slice(res.CommonPrefixes, func(i, j int) bool { return res.CommonPrefixes[i].Prefix < res.CommonPrefixes[j].Prefix })
	w.Header().Set("Content-Type", "application/xml")
	xml.NewEncoder(w).Encode(res)
}

func TestS3MockLifecycle(t *testing.T) {
	mock := &mockS3{objects: map[string][]byte{}, modtime: map[string]time.Time{}}
	// pre-existing object to read
	mock.objects["docs/readme.txt"] = []byte("existing content")
	mock.modtime["docs/readme.txt"] = time.Now()

	srv := httptest.NewServer(mock)
	defer srv.Close()

	be, err := Build("s3", "test", map[string]string{
		"bucket":     "bucket",
		"access_key": "AKIA",
		"secret_key": "secret",
		"region":     "us-east-1",
		"endpoint":   srv.URL,
		"path_style": "1",
	})
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if err := be.Init(); err != nil {
		t.Fatalf("init: %v", err)
	}

	// list root -> docs/ prefix
	entries, err := be.ReadDir("/")
	if err != nil {
		t.Fatalf("readdir root: %v", err)
	}
	if len(entries) != 1 || entries[0].Name != "docs" || !entries[0].IsDir {
		t.Fatalf("root entries = %+v", entries)
	}

	// read existing file
	a, err := be.Stat("/docs/readme.txt")
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if a.Size != int64(len("existing content")) {
		t.Fatalf("size = %d", a.Size)
	}
	rh, err := be.OpenRead("/docs/readme.txt")
	if err != nil {
		t.Fatalf("openread: %v", err)
	}
	buf := make([]byte, a.Size)
	if _, err := rh.ReadAt(buf, 0); err != nil && err != io.EOF {
		t.Fatalf("readat: %v", err)
	}
	rh.Close()
	if string(buf) != "existing content" {
		t.Fatalf("read = %q", buf)
	}

	// write a new file (staged)
	w, err := be.OpenWrite("/docs/new.txt", true)
	if err != nil {
		t.Fatalf("openwrite: %v", err)
	}
	if _, err := w.WriteAt([]byte("brand new"), 0); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	if string(mock.objects["docs/new.txt"]) != "brand new" {
		t.Fatalf("uploaded = %q", mock.objects["docs/new.txt"])
	}

	// mkdir creates a marker; list then shows the empty dir
	if err := be.Mkdir("/empty", 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if _, ok := mock.objects["empty/"]; !ok {
		t.Fatalf("marker not created: %v", mock.objects)
	}
	ent, _ := be.ReadDir("/")
	foundEmpty := false
	for _, e := range ent {
		if e.Name == "empty" && e.IsDir {
			foundEmpty = true
		}
	}
	if !foundEmpty {
		t.Fatalf("empty dir not listed: %+v", ent)
	}

	// rename via copy+delete
	if err := be.Rename("/docs/new.txt", "/docs/renamed.txt"); err != nil {
		t.Fatalf("rename: %v", err)
	}
	if string(mock.objects["docs/renamed.txt"]) != "brand new" {
		t.Fatalf("renamed content wrong")
	}
	if _, ok := mock.objects["docs/new.txt"]; ok {
		t.Fatalf("old key still present")
	}

	// delete
	if err := be.Remove("/docs/renamed.txt"); err != nil {
		t.Fatalf("remove: %v", err)
	}
	if _, ok := mock.objects["docs/renamed.txt"]; ok {
		t.Fatalf("not deleted")
	}
}
