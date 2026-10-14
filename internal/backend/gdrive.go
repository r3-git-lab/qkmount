package backend

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

func init() {
	Register("gdrive", func(name string, opts map[string]string) (Backend, error) {
		st, err := newGDrive(opts)
		if err != nil {
			return nil, err
		}
		return NewObjectFS(st, ""), nil
	})
}

var (
	gdriveAPIBase    = "https://www.googleapis.com/drive/v3"
	gdriveUploadBase = "https://www.googleapis.com/upload/drive/v3"
)

const folderMime = "application/vnd.google-apps.folder"

// shortcutMime marks a Drive shortcut: a pointer to another file. We hide
// these so a file that is shortcut-linked in several places does not appear
// twice.
const shortcutMime = "application/vnd.google-apps.shortcut"

// googleExport maps Google Docs Editors MIME types to the format QKMount
// exports them as. Docs Editors files (Docs, Sheets, Slides, ...) have no
// binary content of their own: requesting their bytes fails with
// "Only files with binary content can be downloaded. Use Export ...". So we
// present them with the export's extension and export them on read.
var googleExport = map[string]struct {
	Mime string
	Ext  string
}{
	"application/vnd.google-apps.document": {
		"application/vnd.openxmlformats-officedocument.wordprocessingml.document", ".docx"},
	"application/vnd.google-apps.spreadsheet": {
		"application/vnd.openxmlformats-officedocument.spreadsheetml.sheet", ".xlsx"},
	"application/vnd.google-apps.presentation": {
		"application/vnd.openxmlformats-officedocument.presentationml.presentation", ".pptx"},
	"application/vnd.google-apps.drawing": {"image/png", ".png"},
	"application/vnd.google-apps.jam":     {"application/pdf", ".pdf"},
	"application/vnd.google-apps.form":    {"application/zip", ".zip"},
	"application/vnd.google-apps.site":    {"text/plain", ".txt"},
	"application/vnd.google-apps.script":  {"application/json", ".json"},
}

type gdriveStore struct {
	client *http.Client
	rootID string

	mu     sync.Mutex
	cache  map[string]*gdEntry  // path -> entry
	listed map[string]time.Time // directory path -> when its children were last fetched
	ttl    time.Duration

	// Export caches for Google Docs Editors files, keyed by id@modifiedTime.
	expMu   sync.Mutex
	expSize map[string]int64
	expData map[string][]byte
}

type gdEntry struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	Mime    string `json:"mimeType"`
	SizeStr string `json:"size"`
	MTime   string `json:"modifiedTime"`

	// Export is non-empty for Google Docs Editors files and holds the MIME
	// type they must be exported as. It is never sent to or parsed from the API.
	Export string `json:"-"`
}

func (e *gdEntry) isDir() bool { return e.Mime == folderMime }

// isNative reports whether the entry must be exported rather than downloaded.
func (e *gdEntry) isNative() bool { return e.Export != "" }

// visible rewrites a raw entry for the mount. Google-native files are renamed
// with their export extension and flagged for export-on-read; unrepresentable
// native types (and shortcuts) are hidden, reporting false.
func (e *gdEntry) visible() bool {
	if e.Mime == folderMime {
		return true
	}
	if e.Mime == shortcutMime {
		return false
	}
	if x, ok := googleExport[e.Mime]; ok {
		e.Export = x.Mime
		e.Mime = x.Mime
		if !strings.HasSuffix(strings.ToLower(e.Name), x.Ext) {
			e.Name += x.Ext
		}
		return true
	}
	// Other Google-native types (audio, video, map, ...) cannot be downloaded
	// in any useful form; hide them rather than fail on read.
	if strings.HasPrefix(e.Mime, "application/vnd.google-apps.") {
		return false
	}
	return true
}

// exportKey identifies a cached export; the modified time invalidates it when
// the document changes.
func (e *gdEntry) exportKey() string { return e.ID + "@" + e.MTime }

func (e *gdEntry) size() int64 {
	var n int64
	fmt.Sscanf(e.SizeStr, "%d", &n)
	return n
}

func newGDrive(opts map[string]string) (*gdriveStore, error) {
	tok, err := parseOAuthToken(opts["token"])
	if err != nil {
		return nil, fmt.Errorf("gdrive: %w", err)
	}
	clientID := Opt(opts, "client_id", defaultGoogleClientID)
	clientSecret := Opt(opts, "client_secret", defaultGoogleClientSecret)
	client := oauthHTTPClient(context.Background(), tok, clientID, clientSecret,
		"https://accounts.google.com/o/oauth2/auth",
		"https://oauth2.googleapis.com/token")
	root := Opt(opts, "root_id", "root")
	return &gdriveStore{
		client:  client,
		rootID:  root,
		cache:   map[string]*gdEntry{},
		listed:  map[string]time.Time{},
		ttl:     15 * time.Second,
		expSize: map[string]int64{},
		expData: map[string][]byte{},
	}, nil
}

func (s *gdriveStore) Kind() string { return "gdrive" }

// GoogleAccountEmail uses a freshly obtained token to look up the signed-in
// account's e-mail address, so imported connections can be named after the
// account the way Mountain Duck does.
func GoogleAccountEmail(tokenJSON, clientID, clientSecret string) (string, error) {
	tok, err := parseOAuthToken(tokenJSON)
	if err != nil {
		return "", err
	}
	if clientID == "" {
		clientID = defaultGoogleClientID
	}
	if clientSecret == "" {
		clientSecret = defaultGoogleClientSecret
	}
	client := oauthHTTPClient(context.Background(), tok, clientID, clientSecret,
		"https://accounts.google.com/o/oauth2/auth",
		"https://oauth2.googleapis.com/token")
	var about struct {
		User struct {
			EmailAddress string `json:"emailAddress"`
		} `json:"user"`
	}
	if err := putJSON(client, "GET", gdriveAPIBase+"/about?fields=user", nil, &about, nil); err != nil {
		return "", err
	}
	return about.User.EmailAddress, nil
}

func (s *gdriveStore) Init() error {
	var about struct {
		User struct {
			EmailAddress string `json:"emailAddress"`
		} `json:"user"`
	}
	if err := putJSON(s.client, "GET", gdriveAPIBase+"/about?fields=user", nil, &about, nil); err != nil {
		return fmt.Errorf("gdrive: authentication failed: %w", err)
	}
	return nil
}
func (s *gdriveStore) Close() error { return nil }

func (s *gdriveStore) StatFS() (uint64, uint64, uint64) {
	var about struct {
		StorageQuota struct {
			Limit string `json:"limit"`
			Usage string `json:"usage"`
		} `json:"storageQuota"`
	}
	if err := putJSON(s.client, "GET", gdriveAPIBase+"/about?fields=storageQuota", nil, &about, nil); err != nil {
		return 1 << 40, 1 << 39, 1 << 39
	}
	var limit, usage int64
	fmt.Sscanf(about.StorageQuota.Limit, "%d", &limit)
	fmt.Sscanf(about.StorageQuota.Usage, "%d", &usage)
	if limit <= 0 {
		return 1 << 40, 1 << 39, 1 << 39
	}
	free := limit - usage
	if free < 0 {
		free = 0
	}
	return uint64(limit), uint64(free), uint64(free)
}

// listChildren returns the children of a folder id.
func (s *gdriveStore) listChildren(id string) ([]*gdEntry, error) {
	var out []*gdEntry
	pageToken := ""
	for {
		q := url.Values{}
		q.Set("q", fmt.Sprintf("'%s' in parents and trashed = false", escapeDriveQ(id)))
		q.Set("fields", "nextPageToken,files(id,name,mimeType,size,modifiedTime)")
		q.Set("pageSize", "1000")
		if pageToken != "" {
			q.Set("pageToken", pageToken)
		}
		var res struct {
			NextPageToken string     `json:"nextPageToken"`
			Files         []*gdEntry `json:"files"`
		}
		if err := putJSON(s.client, "GET", gdriveAPIBase+"/files?"+q.Encode(), nil, &res, nil); err != nil {
			return nil, err
		}
		for _, f := range res.Files {
			if f.visible() {
				out = append(out, f)
			}
		}
		if res.NextPageToken == "" {
			break
		}
		pageToken = res.NextPageToken
	}
	return out, nil
}

// nativeFallbackSize is reported for a Google-native file whose exported size
// could not be probed (for example while rate limited). It is deliberately
// large so a read is never truncated by the size the kernel believes.
const nativeFallbackSize = 1 << 30

// exportURL returns the Docs Editors export URL for an entry.
func (s *gdriveStore) exportURL(e *gdEntry) string {
	return gdriveAPIBase + "/files/" + url.PathEscape(e.ID) + "/export?mimeType=" + url.QueryEscape(e.Export)
}

// exportSize returns the size of a native file's exported bytes. Content-Length
// is usually enough, so the body is not downloaded. The result is cached per
// id and modified time.
func (s *gdriveStore) exportSize(e *gdEntry) (int64, error) {
	k := e.exportKey()
	s.expMu.Lock()
	if n, ok := s.expSize[k]; ok {
		s.expMu.Unlock()
		return n, nil
	}
	s.expMu.Unlock()

	req, err := http.NewRequest("GET", s.exportURL(e), nil)
	if err != nil {
		return 0, err
	}
	resp, err := s.client.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return 0, httpError(resp)
	}
	var n int64
	if cl := resp.Header.Get("Content-Length"); cl != "" {
		n, _ = strconv.ParseInt(cl, 10, 64)
	} else {
		if n, err = io.Copy(io.Discard, resp.Body); err != nil {
			return 0, err
		}
	}
	s.expMu.Lock()
	s.expSize[k] = n
	s.expMu.Unlock()
	return n, nil
}

// nativeSize is the size reported for a native file, falling back to a large
// bound when the export cannot be measured so reads still terminate correctly.
func (s *gdriveStore) nativeSize(e *gdEntry) int64 {
	if n, err := s.exportSize(e); err == nil {
		return n
	}
	return nativeFallbackSize
}

// maxExportCache caps how much exported content is cached in memory.
const maxExportCache = 8 << 20

// exportData returns the full exported content of a native file. Small results
// are cached so repeated reads do not re-export the document.
func (s *gdriveStore) exportData(e *gdEntry) ([]byte, error) {
	k := e.exportKey()
	s.expMu.Lock()
	if b, ok := s.expData[k]; ok {
		s.expMu.Unlock()
		return b, nil
	}
	s.expMu.Unlock()

	req, err := http.NewRequest("GET", s.exportURL(e), nil)
	if err != nil {
		return nil, err
	}
	resp, err := s.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, httpError(resp)
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, maxExportCache+1))
	if err != nil {
		return nil, err
	}
	s.expMu.Lock()
	s.expSize[k] = int64(len(b))
	if int64(len(b)) <= maxExportCache {
		s.expData[k] = b
	}
	s.expMu.Unlock()
	return b, nil
}

// resolve walks a path, returning the entry (file or folder).
func (s *gdriveStore) resolve(path string) (*gdEntry, error) {
	clean := Clean(path)
	if clean == "/" {
		return &gdEntry{ID: s.rootID, Name: "", Mime: folderMime}, nil
	}
	s.mu.Lock()
	if e, ok := s.cache[clean]; ok {
		s.mu.Unlock()
		return e, nil
	}
	// If the parent's children were listed recently, a miss means the path
	// really does not exist; answer without another API call. This is what
	// keeps a burst of lookups from exhausting provider quotas.
	if at, ok := s.listed[Parent(clean)]; ok && time.Since(at) < s.ttl {
		s.mu.Unlock()
		return nil, os.ErrNotExist
	}
	s.mu.Unlock()

	parent, err := s.resolve(Parent(clean))
	if err != nil {
		return nil, err
	}
	if !parent.isDir() {
		return nil, os.ErrNotExist
	}
	children, err := s.listChildren(parent.ID)
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	for _, c := range children {
		cp := s.cachePath(Parent(clean), c.Name)
		s.cache[cp] = c
	}
	s.listed[Parent(clean)] = time.Now()
	e := s.cache[clean]
	s.mu.Unlock()
	if e == nil {
		return nil, os.ErrNotExist
	}
	return e, nil
}

func (s *gdriveStore) cachePath(parent, name string) string {
	if parent == "/" {
		return "/" + name
	}
	return parent + "/" + name
}

// invalidate drops a path from the entry cache and forgets any remembered
// listing of that path.
func (s *gdriveStore) invalidate(path string) {
	clean := Clean(path)
	s.mu.Lock()
	delete(s.cache, clean)
	delete(s.listed, clean)
	s.mu.Unlock()
}

// invalidateList forgets that a directory was listed recently, so its children
// are fetched again on the next lookup. Every mutation must call this for the
// affected directory, otherwise the negative cache can hide a new or renamed
// entry until the TTL expires.
func (s *gdriveStore) invalidateList(dir string) {
	clean := Clean(dir)
	s.mu.Lock()
	delete(s.listed, clean)
	s.mu.Unlock()
}

func (s *gdriveStore) List(prefix, delimiter string) ([]ObjMeta, []string, error) {
	dir := "/" + strings.Trim(prefix, "/")
	if strings.Trim(prefix, "/") == "" {
		dir = "/"
	}
	e, err := s.resolve(dir)
	if err != nil {
		return nil, nil, err
	}
	if !e.isDir() {
		return nil, nil, os.ErrNotExist
	}
	children, err := s.listChildren(e.ID)
	if err != nil {
		return nil, nil, err
	}
	var objs []ObjMeta
	var prefixes []string
	pfx := prefix
	if pfx != "" && !strings.HasSuffix(pfx, "/") {
		pfx += "/"
	}
	for _, c := range children {
		key := pfx + c.Name
		s.mu.Lock()
		s.cache[s.cachePath(dir, c.Name)] = c
		s.mu.Unlock()
		if c.isDir() {
			prefixes = append(prefixes, key+"/")
			continue
		}
		size := c.size()
		if c.isNative() {
			size = s.nativeSize(c)
		}
		m := ObjMeta{Key: key, Size: size}
		if t, err := time.Parse(time.RFC3339, c.MTime); err == nil {
			m.Mtime = t
		}
		objs = append(objs, m)
	}
	return objs, prefixes, nil
}

func (s *gdriveStore) Head(key string) (*ObjMeta, error) {
	e, err := s.resolve("/" + strings.TrimPrefix(key, "/"))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	if e.isDir() {
		return nil, nil
	}
	size := e.size()
	if e.isNative() {
		size = s.nativeSize(e)
	}
	m := &ObjMeta{Key: key, Size: size}
	if t, err := time.Parse(time.RFC3339, e.MTime); err == nil {
		m.Mtime = t
	}
	return m, nil
}

func (s *gdriveStore) Get(key string, off, size int64) ([]byte, error) {
	e, err := s.resolve("/" + strings.TrimPrefix(key, "/"))
	if err != nil {
		return nil, err
	}
	if e.isNative() {
		// Docs Editors files have no binary content; fetch the export and slice
		// it locally (the export endpoint does not support range requests).
		data, err := s.exportData(e)
		if err != nil {
			return nil, err
		}
		if off < 0 {
			off = 0
		}
		if off >= int64(len(data)) {
			return nil, nil
		}
		end := int64(len(data))
		if size > 0 && off+size < end {
			end = off + size
		}
		return data[off:end], nil
	}
	req, err := http.NewRequest("GET", gdriveAPIBase+"/files/"+e.ID+"?alt=media", nil)
	if err != nil {
		return nil, err
	}
	if size > 0 {
		req.Header.Set("Range", fmt.Sprintf("bytes=%d-%d", off, off+size-1))
	}
	resp, err := s.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 && resp.StatusCode != 206 {
		return nil, httpError(resp)
	}
	return io.ReadAll(resp.Body)
}

func (s *gdriveStore) Put(key string, r io.Reader, size int64) error {
	path := "/" + strings.TrimPrefix(key, "/")
	e, err := s.resolve(path)
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	if e != nil && !e.isDir() {
		// Update existing content.
		req, err := http.NewRequest("PATCH", gdriveUploadBase+"/files/"+e.ID+"?uploadType=media", r)
		if err != nil {
			return err
		}
		req.ContentLength = size
		req.Header.Set("Content-Type", "application/octet-stream")
		resp, err := s.client.Do(req)
		if err != nil {
			return err
		}
		defer resp.Body.Close()
		if resp.StatusCode >= 300 {
			return httpError(resp)
		}
		s.invalidate(path)
		s.invalidateList(Parent(path))
		return nil
	}
	// Create new file in the parent folder.
	parent, err := s.ensureDir(Parent(path))
	if err != nil {
		return err
	}
	meta := map[string]any{"name": Base(path), "parents": []string{parent.ID}}
	raw, _ := json.Marshal(meta)
	boundary := "qkmount" + fmt.Sprint(time.Now().UnixNano())
	var body io.Reader = io.MultiReader(
		strings.NewReader(fmt.Sprintf("--%s\r\nContent-Type: application/json; charset=UTF-8\r\n\r\n%s\r\n--%s\r\nContent-Type: application/octet-stream\r\n\r\n", boundary, raw, boundary)),
		r,
		strings.NewReader(fmt.Sprintf("\r\n--%s--", boundary)),
	)
	req, err := http.NewRequest("POST", gdriveUploadBase+"/files?uploadType=multipart", body)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "multipart/related; boundary="+boundary)
	resp, err := s.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return httpError(resp)
	}
	s.invalidate(path)
	s.invalidateList(Parent(path))
	return nil
}

// ensureDir resolves a folder, creating it (and parents) if needed.
func (s *gdriveStore) ensureDir(path string) (*gdEntry, error) {
	e, err := s.resolve(path)
	if err == nil {
		if !e.isDir() {
			return nil, os.ErrExist
		}
		return e, nil
	}
	if !os.IsNotExist(err) {
		return nil, err
	}
	return s.mkdir(path)
}

func (s *gdriveStore) Mkdir(dir string) error {
	_, err := s.mkdir(dir)
	return err
}

func (s *gdriveStore) mkdir(dir string) (*gdEntry, error) {
	clean := Clean(dir)
	parent, err := s.ensureDir(Parent(clean))
	if err != nil {
		return nil, err
	}
	meta := map[string]any{"name": Base(clean), "mimeType": folderMime, "parents": []string{parent.ID}}
	var created gdEntry
	created.Mime = folderMime
	if err := putJSON(s.client, "POST", gdriveAPIBase+"/files?fields=id,name,mimeType", meta, &created, nil); err != nil {
		return nil, err
	}
	s.mu.Lock()
	s.cache[clean] = &created
	s.mu.Unlock()
	s.invalidateList(Parent(clean))
	return &created, nil
}

func (s *gdriveStore) RemoveDir(dir string) error {
	return s.remove(dir)
}

func (s *gdriveStore) remove(path string) error {
	e, err := s.resolve(path)
	if err != nil {
		return err
	}
	if err := putJSON(s.client, "DELETE", gdriveAPIBase+"/files/"+e.ID, nil, nil, nil); err != nil {
		return err
	}
	s.invalidate(path)
	s.invalidateList(Parent(path))
	return nil
}

func (s *gdriveStore) Delete(key string) error {
	return s.remove("/" + strings.TrimPrefix(key, "/"))
}

func (s *gdriveStore) Copy(src, dst string) error {
	se, err := s.resolve("/" + strings.TrimPrefix(src, "/"))
	if err != nil {
		return err
	}
	parent, err := s.ensureDir(Parent("/" + strings.TrimPrefix(dst, "/")))
	if err != nil {
		return err
	}
	body := map[string]any{"name": Base("/" + strings.TrimPrefix(dst, "/")), "parents": []string{parent.ID}}
	if err := putJSON(s.client, "POST", gdriveAPIBase+"/files/"+se.ID+"/copy", body, nil, nil); err != nil {
		return err
	}
	dstPath := "/" + strings.TrimPrefix(dst, "/")
	s.invalidate(dstPath)
	s.invalidateList(Parent(dstPath))
	return nil
}

func (s *gdriveStore) RenameObject(src, dst string) error {
	se, err := s.resolve(src)
	if err != nil {
		return err
	}
	srcParent, err := s.resolve(Parent(src))
	if err != nil {
		return err
	}
	dstParent, err := s.ensureDir(Parent(dst))
	if err != nil {
		return err
	}
	q := url.Values{}
	q.Set("addParents", dstParent.ID)
	q.Set("removeParents", srcParent.ID)
	q.Set("fields", "id")
	body := map[string]any{"name": Base(dst)}
	if err := putJSON(s.client, "PATCH", gdriveAPIBase+"/files/"+se.ID+"?"+q.Encode(), body, nil, nil); err != nil {
		return err
	}
	s.invalidate(src)
	s.invalidate(dst)
	s.invalidateList(Parent(src))
	s.invalidateList(Parent(dst))
	return nil
}

// StatDir reports whether a key is a directory, without listing its children.
func (s *gdriveStore) StatDir(key string) (bool, error) {
	e, err := s.resolve("/" + strings.TrimPrefix(key, "/"))
	if err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, err
	}
	return e.isDir(), nil
}

func escapeDriveQ(s string) string {
	return strings.ReplaceAll(s, "'", "\\'")
}
