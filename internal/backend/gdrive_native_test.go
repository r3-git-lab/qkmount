package backend

import (
	"bytes"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestGDriveVisibleTransform(t *testing.T) {
	cases := []struct {
		name      string
		in        gdEntry
		wantShow  bool
		wantName  string
		wantMime  string
		wantNativ bool
	}{
		{
			name:      "google doc gets docx extension",
			in:        gdEntry{Name: "Report", Mime: "application/vnd.google-apps.document"},
			wantShow:  true,
			wantName:  "Report.docx",
			wantMime:  "application/vnd.openxmlformats-officedocument.wordprocessingml.document",
			wantNativ: true,
		},
		{
			name:      "google sheet gets xlsx extension",
			in:        gdEntry{Name: "Budget", Mime: "application/vnd.google-apps.spreadsheet"},
			wantShow:  true,
			wantName:  "Budget.xlsx",
			wantNativ: true,
		},
		{
			name:      "extension is not doubled",
			in:        gdEntry{Name: "Report.docx", Mime: "application/vnd.google-apps.document"},
			wantShow:  true,
			wantName:  "Report.docx",
			wantNativ: true,
		},
		{
			name:     "shortcut is hidden",
			in:       gdEntry{Name: "link", Mime: shortcutMime},
			wantShow: false,
		},
		{
			name:     "unknown native type is hidden",
			in:       gdEntry{Name: "clip", Mime: "application/vnd.google-apps.video"},
			wantShow: false,
		},
		{
			name:     "folder is untouched",
			in:       gdEntry{Name: "Docs", Mime: folderMime},
			wantShow: true,
			wantName: "Docs",
			wantMime: folderMime,
		},
		{
			name:     "binary file is untouched",
			in:       gdEntry{Name: "photo.jpg", Mime: "image/jpeg"},
			wantShow: true,
			wantName: "photo.jpg",
			wantMime: "image/jpeg",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e := c.in
			if got := e.visible(); got != c.wantShow {
				t.Fatalf("visible() = %v, want %v", got, c.wantShow)
			}
			if !c.wantShow {
				return
			}
			if e.Name != c.wantName {
				t.Fatalf("name = %q, want %q", e.Name, c.wantName)
			}
			if c.wantMime != "" && e.Mime != c.wantMime {
				t.Fatalf("mime = %q, want %q", e.Mime, c.wantMime)
			}
			if e.isNative() != c.wantNativ {
				t.Fatalf("isNative() = %v, want %v", e.isNative(), c.wantNativ)
			}
		})
	}
}

// TestGDriveExportRead verifies that a Google Docs document is listed with its
// export name and size, and that reading it fetches and slices the export.
func TestGDriveExportRead(t *testing.T) {
	const exportBody = "HELLO-EXPORTED-BODY"
	var exportCalls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/export"):
			exportCalls++
			if got := r.URL.Query().Get("mimeType"); !strings.Contains(got, "wordprocessingml") {
				t.Errorf("export mimeType = %q", got)
			}
			w.Write([]byte(exportBody))
		case strings.HasSuffix(r.URL.Path, "/files") && r.URL.Query().Get("q") != "":
			w.Header().Set("Content-Type", "application/json")
			w.Write([]byte(`{"files":[` +
				`{"id":"doc-1","name":"Report","mimeType":"application/vnd.google-apps.document","modifiedTime":"2026-01-01T00:00:00Z"},` +
				`{"id":"file-1","name":"photo.jpg","mimeType":"image/jpeg","size":"1234","modifiedTime":"2026-01-01T00:00:00Z"},` +
				`{"id":"sc-1","name":"shortcut","mimeType":"application/vnd.google-apps.shortcut","modifiedTime":"2026-01-01T00:00:00Z"}` +
				`]}`))
		default:
			w.Header().Set("Content-Type", "application/json")
			w.Write([]byte(`{}`))
		}
	}))
	defer srv.Close()

	st := newTestGDrive(t, srv)

	objs, prefixes, err := st.List("", "/")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(prefixes) != 0 {
		t.Fatalf("unexpected prefixes: %v", prefixes)
	}
	if len(objs) != 2 {
		t.Fatalf("expected 2 visible objects (shortcut hidden), got %d: %+v", len(objs), objs)
	}
	byKey := map[string]ObjMeta{}
	for _, o := range objs {
		byKey[o.Key] = o
	}
	doc, ok := byKey["Report.docx"]
	if !ok {
		t.Fatalf("Report.docx missing; got %+v", objs)
	}
	if doc.Size != int64(len(exportBody)) {
		t.Fatalf("doc size = %d, want %d", doc.Size, len(exportBody))
	}
	if _, ok := byKey["photo.jpg"]; !ok {
		t.Fatalf("binary file missing; got %+v", objs)
	}

	// Full read.
	got, err := st.Get("Report.docx", 0, 0)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if !bytes.Equal(got, []byte(exportBody)) {
		t.Fatalf("Get = %q, want %q", got, exportBody)
	}

	// Range read.
	got, err = st.Get("Report.docx", 6, 4)
	if err != nil {
		t.Fatalf("Get range: %v", err)
	}
	if string(got) != "EXPO" {
		t.Fatalf("Get range = %q, want %q", got, "EXPO")
	}

	// Read past the end returns no bytes and no error.
	got, err = st.Get("Report.docx", int64(len(exportBody)), 10)
	if err != nil {
		t.Fatalf("Get past end: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("Get past end = %q, want empty", got)
	}

	// The export is fetched twice: once to measure the size (headers only,
	// during listing) and once to download the content.
	if exportCalls != 2 {
		t.Fatalf("export fetched %d times, want 2 (probe + download)", exportCalls)
	}

	// A subsequent read must be served from the content cache.
	if _, err := st.Get("Report.docx", 0, 3); err != nil {
		t.Fatalf("Get (cached): %v", err)
	}
	if exportCalls != 2 {
		t.Fatalf("export re-fetched on cached read: %d calls", exportCalls)
	}

	// Head reports the exported size, not zero.
	m, err := st.Head("Report.docx")
	if err != nil {
		t.Fatalf("Head: %v", err)
	}
	if m == nil || m.Size != int64(len(exportBody)) {
		t.Fatalf("Head = %+v, want size %d", m, len(exportBody))
	}
}

func TestBinaryContentErrorIsNotPermissionDenied(t *testing.T) {
	body := []byte(`{"error":{"code":403,"message":"Only files with binary content can be downloaded. Use Export with Docs Editors files.","errors":[{"reason":"fileNotDownloadable"}]}}`)
	err := httpErrorBody(403, "403 Forbidden", body)
	if !errors.Is(err, ErrNotDownloadable) {
		t.Fatalf("expected ErrNotDownloadable, got %v", err)
	}
	if errors.Is(err, ErrRateLimited) {
		t.Fatal("must not be classified as a rate limit")
	}
}
