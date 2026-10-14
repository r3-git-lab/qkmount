package backend

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

func init() {
	Register("onedrive", func(name string, opts map[string]string) (Backend, error) {
		st, err := newOneDrive(opts)
		if err != nil {
			return nil, err
		}
		return NewObjectFS(st, strings.Trim(opts["path"], "/")), nil
	})
}

type oneDriveStore struct {
	client *http.Client
	base   string // e.g. https://graph.microsoft.com/v1.0/me/drive
}

type odItem struct {
	ID     string `json:"id"`
	Name   string `json:"name"`
	Size   int64  `json:"size"`
	Folder *struct {
		ChildCount int `json:"childCount"`
	} `json:"folder"`
	File *struct {
		MimeType string `json:"mimeType"`
	} `json:"file"`
	LastModified string `json:"lastModifiedDateTime"`
}

func (i *odItem) isDir() bool { return i.Folder != nil }

func newOneDrive(opts map[string]string) (*oneDriveStore, error) {
	tok, err := parseOAuthToken(opts["token"])
	if err != nil {
		return nil, fmt.Errorf("onedrive: %w", err)
	}
	client := oauthHTTPClient(context.Background(), tok, opts["client_id"], opts["client_secret"],
		"https://login.microsoftonline.com/common/oauth2/v2.0/authorize",
		"https://login.microsoftonline.com/common/oauth2/v2.0/token")
	drive := "me/drive"
	if id := opts["drive_id"]; id != "" {
		drive = "drives/" + id
	}
	return &oneDriveStore{client: client, base: "https://graph.microsoft.com/v1.0/" + drive}, nil
}

func (s *oneDriveStore) Kind() string { return "onedrive" }

// StatDir reports whether a key is a folder, without listing its children.
func (s *oneDriveStore) StatDir(key string) (bool, error) {
	it, err := s.item("/" + strings.TrimPrefix(key, "/"))
	if err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, err
	}
	return it.isDir(), nil
}

func (s *oneDriveStore) Init() error {
	var res map[string]any
	return putJSON(s.client, "GET", s.base+"/root", nil, &res, nil)
}
func (s *oneDriveStore) Close() error { return nil }

func (s *oneDriveStore) StatFS() (uint64, uint64, uint64) {
	var res struct {
		Total     int64 `json:"total"`
		Remaining int64 `json:"remaining"`
	}
	if err := putJSON(s.client, "GET", s.base+"/quota", nil, &res, nil); err != nil {
		return 1 << 40, 1 << 39, 1 << 39
	}
	if res.Total <= 0 {
		return 1 << 40, 1 << 39, 1 << 39
	}
	return uint64(res.Total), uint64(res.Remaining), uint64(res.Remaining)
}

// encPath encodes a graph path for the "root:/path:" syntax.
func encPath(p string) string {
	clean := Clean(p)
	if clean == "/" {
		return ""
	}
	segs := strings.Split(strings.Trim(clean, "/"), "/")
	for i, s := range segs {
		segs[i] = url.PathEscape(s)
	}
	return "/" + strings.Join(segs, "/")
}

func (s *oneDriveStore) itemURL(p string) string {
	ep := encPath(p)
	if ep == "" {
		return s.base + "/root"
	}
	return s.base + "/root:" + ep + ":"
}

func (s *oneDriveStore) childrenURL(p string) string {
	ep := encPath(p)
	if ep == "" {
		return s.base + "/root/children"
	}
	return s.base + "/root:" + ep + ":/children"
}

func (s *oneDriveStore) item(p string) (*odItem, error) {
	var it odItem
	err := putJSON(s.client, "GET", s.itemURL(p)+"?$select=id,name,size,folder,file,lastModifiedDateTime", nil, &it, nil)
	if err != nil {
		return nil, err
	}
	return &it, nil
}

func (s *oneDriveStore) List(prefix, delimiter string) ([]ObjMeta, []string, error) {
	dir := "/" + strings.Trim(prefix, "/")
	if strings.Trim(prefix, "/") == "" {
		dir = "/"
	}
	next := s.childrenURL(dir) + "?$top=1000&$select=id,name,size,folder,file,lastModifiedDateTime"
	pfx := prefix
	if pfx != "" && !strings.HasSuffix(pfx, "/") {
		pfx += "/"
	}
	var objs []ObjMeta
	var prefixes []string
	for next != "" {
		var page struct {
			Value    []odItem `json:"value"`
			NextLink string   `json:"@odata.nextLink"`
		}
		if err := putJSON(s.client, "GET", next, nil, &page, nil); err != nil {
			return nil, nil, err
		}
		for _, it := range page.Value {
			key := pfx + it.Name
			if it.isDir() {
				prefixes = append(prefixes, key+"/")
				continue
			}
			m := ObjMeta{Key: key, Size: it.Size}
			if t, err := time.Parse(time.RFC3339, it.LastModified); err == nil {
				m.Mtime = t
			}
			objs = append(objs, m)
		}
		next = page.NextLink
	}
	return objs, prefixes, nil
}

func (s *oneDriveStore) Head(key string) (*ObjMeta, error) {
	it, err := s.item("/" + strings.TrimPrefix(key, "/"))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	if it.isDir() {
		return nil, nil
	}
	m := &ObjMeta{Key: key, Size: it.Size}
	if t, err := time.Parse(time.RFC3339, it.LastModified); err == nil {
		m.Mtime = t
	}
	return m, nil
}

func (s *oneDriveStore) Get(key string, off, size int64) ([]byte, error) {
	req, err := http.NewRequest("GET", s.itemURL("/"+strings.TrimPrefix(key, "/"))+"/content", nil)
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
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode == 200 && off > 0 {
		if off >= int64(len(data)) {
			return nil, nil
		}
		data = data[off:]
	}
	return data, nil
}

func (s *oneDriveStore) Put(key string, r io.Reader, size int64) error {
	path := "/" + strings.TrimPrefix(key, "/")
	if dir := Parent(path); dir != "/" {
		if err := s.ensureDir(dir); err != nil {
			return err
		}
	}
	if size <= 4*1024*1024 {
		req, err := http.NewRequest("PUT", s.itemURL(path)+"/content", r)
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
		return nil
	}
	return s.uploadLarge(path, r, size)
}

func (s *oneDriveStore) uploadLarge(path string, r io.Reader, size int64) error {
	var session struct {
		UploadURL string `json:"uploadUrl"`
	}
	if err := putJSON(s.client, "POST", s.itemURL(path)+"/createUploadSession", map[string]any{}, &session, nil); err != nil {
		return err
	}
	const chunk = 4 * 1024 * 1024
	buf := make([]byte, chunk)
	var offset int64
	for offset < size {
		n, err := io.ReadFull(r, buf)
		if err != nil && err != io.ErrUnexpectedEOF && err != io.EOF {
			return err
		}
		if n == 0 {
			break
		}
		req, err := http.NewRequest("PUT", session.UploadURL, strings.NewReader(string(buf[:n])))
		if err != nil {
			return err
		}
		req.ContentLength = int64(n)
		req.Header.Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", offset, offset+int64(n)-1, size))
		resp, err := s.client.Do(req)
		if err != nil {
			return err
		}
		resp.Body.Close()
		if resp.StatusCode >= 300 {
			return fmt.Errorf("onedrive: upload chunk failed: %s", resp.Status)
		}
		offset += int64(n)
	}
	return nil
}

func (s *oneDriveStore) Mkdir(dir string) error {
	if Clean(dir) == "/" {
		return nil
	}
	return s.ensureDir(Clean(dir))
}

func (s *oneDriveStore) ensureDir(dir string) error {
	clean := Clean(dir)
	if clean == "/" {
		return nil
	}
	if it, err := s.item(clean); err == nil && it.isDir() {
		return nil
	}
	parent := Parent(clean)
	if parent != "/" {
		if err := s.ensureDir(parent); err != nil {
			return err
		}
	}
	body := map[string]any{
		"name":                              Base(clean),
		"folder":                            map[string]any{},
		"@microsoft.graph.conflictBehavior": "replace",
	}
	var out odItem
	if err := putJSON(s.client, "POST", s.childrenURL(parent), body, &out, nil); err != nil {
		return err
	}
	return nil
}

func (s *oneDriveStore) RemoveDir(dir string) error {
	return s.delete(dir)
}

func (s *oneDriveStore) Delete(key string) error {
	return s.delete("/" + strings.TrimPrefix(key, "/"))
}

func (s *oneDriveStore) delete(p string) error {
	it, err := s.item(p)
	if err != nil {
		return err
	}
	return putJSON(s.client, "DELETE", s.base+"/items/"+it.ID, nil, nil, nil)
}

func (s *oneDriveStore) Copy(src, dst string) error {
	se, err := s.item("/" + strings.TrimPrefix(src, "/"))
	if err != nil {
		return err
	}
	dp, err := s.item(Parent("/" + strings.TrimPrefix(dst, "/")))
	if err != nil {
		return err
	}
	body := map[string]any{
		"name":            Base("/" + strings.TrimPrefix(dst, "/")),
		"parentReference": map[string]any{"id": dp.ID},
	}
	req, err := http.NewRequest("POST", s.base+"/items/"+se.ID+"/copy", jsonReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := s.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return httpError(resp)
	}
	return nil
}

func (s *oneDriveStore) RenameObject(src, dst string) error {
	se, err := s.item(Clean(src))
	if err != nil {
		return err
	}
	body := map[string]any{"name": Base(Clean(dst))}
	dp, err := s.item(Parent(Clean(dst)))
	if err == nil && dp.ID != "" {
		body["parentReference"] = map[string]any{"id": dp.ID}
	}
	return putJSON(s.client, "PATCH", s.base+"/items/"+se.ID, body, nil, nil)
}

func jsonReader(v any) io.Reader {
	raw, _ := json.Marshal(v)
	return strings.NewReader(string(raw))
}
