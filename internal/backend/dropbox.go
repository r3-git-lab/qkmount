package backend

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

func init() {
	Register("dropbox", func(name string, opts map[string]string) (Backend, error) {
		st, err := newDropbox(opts)
		if err != nil {
			return nil, err
		}
		return NewObjectFS(st, strings.Trim(opts["path"], "/")), nil
	})
}

const (
	dropboxAPI     = "https://api.dropboxapi.com/2"
	dropboxContent = "https://content.dropboxapi.com/2"
)

type dropboxStore struct {
	client *http.Client
	root   string
}

func newDropbox(opts map[string]string) (*dropboxStore, error) {
	tok, err := parseOAuthToken(opts["token"])
	if err != nil {
		return nil, fmt.Errorf("dropbox: %w", err)
	}
	client := oauthHTTPClient(context.Background(), tok, opts["client_id"], opts["client_secret"],
		"https://www.dropbox.com/oauth2/authorize",
		"https://api.dropboxapi.com/oauth2/token")
	return &dropboxStore{client: client}, nil
}

func (s *dropboxStore) Kind() string { return "dropbox" }

// StatDir reports whether a key is a folder, without listing its children.
func (s *dropboxStore) StatDir(key string) (bool, error) {
	var e dropboxEntry
	err := s.rpc("/files/get_metadata", map[string]any{"path": "/" + strings.TrimPrefix(key, "/")}, &e)
	if err != nil {
		if strings.Contains(err.Error(), "not_found") {
			return false, nil
		}
		return false, err
	}
	return e.Tag == "folder", nil
}

func (s *dropboxStore) Init() error {
	return s.rpc("/users/get_current_account", struct{}{}, nil)
}
func (s *dropboxStore) Close() error { return nil }

func (s *dropboxStore) StatFS() (uint64, uint64, uint64) {
	var res struct {
		Allocation struct {
			Allocated int64 `json:"allocated"`
		} `json:"allocation"`
		Used int64 `json:"used"`
	}
	if err := s.rpc("/users/get_space_usage", struct{}{}, &res); err != nil {
		return 1 << 40, 1 << 39, 1 << 39
	}
	alloc, used := res.Allocation.Allocated, res.Used
	if alloc <= 0 {
		return 1 << 40, 1 << 39, 1 << 39
	}
	free := alloc - used
	if free < 0 {
		free = 0
	}
	return uint64(alloc), uint64(free), uint64(free)
}

type dropboxEntry struct {
	Tag            string `json:".tag"`
	Name           string `json:"name"`
	ID             string `json:"id"`
	Size           int64  `json:"size"`
	ClientModified string `json:"client_modified"`
	ServerModified string `json:"server_modified"`
	PathDisplay    string `json:"path_display"`
	PathLower      string `json:"path_lower"`
}

func (s *dropboxStore) rpc(path string, body any, out any) error {
	return putJSON(s.client, "POST", dropboxAPI+path, body, out, nil)
}

func (s *dropboxStore) dirPath(prefix string) string {
	p := "/" + strings.Trim(prefix, "/")
	if p == "/" {
		return ""
	}
	return p
}

func (s *dropboxStore) List(prefix, delimiter string) ([]ObjMeta, []string, error) {
	type listRes struct {
		Entries []dropboxEntry `json:"entries"`
		Cursor  string         `json:"cursor"`
		HasMore bool           `json:"has_more"`
	}
	var res listRes
	if err := s.rpc("/files/list_folder", map[string]any{"path": s.dirPath(prefix), "recursive": false}, &res); err != nil {
		return nil, nil, err
	}
	all := res.Entries
	for res.HasMore {
		var next listRes
		if err := s.rpc("/files/list_folder/continue", map[string]any{"cursor": res.Cursor}, &next); err != nil {
			break
		}
		all = append(all, next.Entries...)
		res = next
	}
	pfx := prefix
	if pfx != "" && !strings.HasSuffix(pfx, "/") {
		pfx += "/"
	}
	var objs []ObjMeta
	var prefixes []string
	for _, e := range all {
		if e.Tag == "deleted" {
			continue
		}
		key := pfx + e.Name
		if e.Tag == "folder" {
			prefixes = append(prefixes, key+"/")
			continue
		}
		m := ObjMeta{Key: key, Size: e.Size}
		if t, err := time.Parse(time.RFC3339, e.ServerModified); err == nil {
			m.Mtime = t
		}
		objs = append(objs, m)
	}
	return objs, prefixes, nil
}

func (s *dropboxStore) Head(key string) (*ObjMeta, error) {
	var e dropboxEntry
	err := s.rpc("/files/get_metadata", map[string]any{"path": "/" + strings.TrimPrefix(key, "/")}, &e)
	if err != nil {
		if strings.Contains(err.Error(), "not_found") {
			return nil, nil
		}
		return nil, err
	}
	if e.Tag != "file" {
		return nil, nil
	}
	m := &ObjMeta{Key: key, Size: e.Size}
	if t, err := time.Parse(time.RFC3339, e.ServerModified); err == nil {
		m.Mtime = t
	}
	return m, nil
}

func (s *dropboxStore) Get(key string, off, size int64) ([]byte, error) {
	arg, _ := json.Marshal(map[string]any{"path": "/" + strings.TrimPrefix(key, "/")})
	req, err := http.NewRequest("POST", dropboxContent+"/files/download", nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Dropbox-API-Arg", string(arg))
	resp, err := s.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return nil, httpError(resp)
	}
	// Dropbox does not support byte ranges, so we fetch and slice.
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if off >= int64(len(data)) {
		return nil, nil
	}
	data = data[off:]
	if size >= 0 && int64(len(data)) > size {
		data = data[:size]
	}
	return data, nil
}

func (s *dropboxStore) Put(key string, r io.Reader, size int64) error {
	key = strings.TrimPrefix(key, "/")
	if dir := Parent("/" + key); dir != "/" {
		if err := s.ensureDir(dir); err != nil {
			return err
		}
	}
	arg, _ := json.Marshal(map[string]any{"path": "/" + key, "mode": "overwrite", "mute": true})
	req, err := http.NewRequest("POST", dropboxContent+"/files/upload", r)
	if err != nil {
		return err
	}
	req.Header.Set("Dropbox-API-Arg", string(arg))
	req.Header.Set("Content-Type", "application/octet-stream")
	req.ContentLength = size
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

func (s *dropboxStore) Mkdir(dir string) error {
	return s.ensureDir(Clean(dir))
}

func (s *dropboxStore) ensureDir(dir string) error {
	clean := Clean(dir)
	if clean == "/" {
		return nil
	}
	var out map[string]any
	err := s.rpc("/files/create_folder_v2", map[string]any{"path": clean}, &out)
	if err != nil {
		if strings.Contains(err.Error(), "conflict") || strings.Contains(err.Error(), "exists") {
			return nil
		}
		return err
	}
	return nil
}

func (s *dropboxStore) Delete(key string) error {
	return s.rpc("/files/delete_v2", map[string]any{"path": "/" + strings.TrimPrefix(key, "/")}, nil)
}

func (s *dropboxStore) RemoveDir(dir string) error {
	return s.rpc("/files/delete_v2", map[string]any{"path": Clean(dir)}, nil)
}

func (s *dropboxStore) Copy(src, dst string) error {
	return s.rpc("/files/copy_v2", map[string]any{
		"from_path": "/" + strings.TrimPrefix(src, "/"),
		"to_path":   "/" + strings.TrimPrefix(dst, "/"),
	}, nil)
}

func (s *dropboxStore) RenameObject(src, dst string) error {
	return s.rpc("/files/move_v2", map[string]any{
		"from_path": Clean(src),
		"to_path":   Clean(dst),
	}, nil)
}
