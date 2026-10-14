package backend

import (
	"encoding/base64"
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
	Register("b2", func(name string, opts map[string]string) (Backend, error) {
		st, err := newB2(opts)
		if err != nil {
			return nil, err
		}
		return NewObjectFS(st, strings.Trim(opts["path"], "/")), nil
	})
}

type b2Store struct {
	client      *http.Client
	keyID       string
	appKey      string
	apiURL      string
	downloadURL string
	token       string
	accountID   string
	bucketName  string
	bucketID    string
}

func newB2(opts map[string]string) (*b2Store, error) {
	if opts["account_id"] == "" || opts["key"] == "" || opts["bucket"] == "" {
		return nil, fmt.Errorf("b2: key id, application key and bucket are required")
	}
	return &b2Store{
		client:     &http.Client{},
		keyID:      opts["account_id"],
		appKey:     opts["key"],
		bucketName: opts["bucket"],
	}, nil
}

func (s *b2Store) Kind() string { return "b2" }

func (s *b2Store) Init() error {
	req, _ := http.NewRequest("GET", "https://api.backblazeb2.com/b2api/v2/b2_authorize_account", nil)
	cred := base64.StdEncoding.EncodeToString([]byte(s.keyID + ":" + s.appKey))
	req.Header.Set("Authorization", "Basic "+cred)
	resp, err := s.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return httpError(resp)
	}
	var auth struct {
		APIURL      string `json:"apiUrl"`
		DownloadURL string `json:"downloadUrl"`
		Token       string `json:"authorizationToken"`
		AccountID   string `json:"accountId"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&auth); err != nil {
		return err
	}
	s.apiURL = auth.APIURL
	s.downloadURL = auth.DownloadURL
	s.token = auth.Token
	s.accountID = auth.AccountID

	var buckets struct {
		Buckets []struct {
			BucketID   string `json:"bucketId"`
			BucketName string `json:"bucketName"`
		} `json:"buckets"`
	}
	if err := s.api("b2_list_buckets", map[string]any{"accountId": s.accountID}, &buckets); err != nil {
		return err
	}
	for _, b := range buckets.Buckets {
		if b.BucketName == s.bucketName {
			s.bucketID = b.BucketID
			break
		}
	}
	if s.bucketID == "" {
		return fmt.Errorf("b2: bucket %q not found", s.bucketName)
	}
	return nil
}

func (s *b2Store) Close() error { return nil }

func (s *b2Store) StatFS() (uint64, uint64, uint64) {
	const tb = 1 << 40
	return tb, tb / 2, tb / 2
}

func (s *b2Store) api(op string, body any, out any) error {
	raw, _ := json.Marshal(body)
	req, err := http.NewRequest("POST", s.apiURL+"/b2api/v2/"+op, strings.NewReader(string(raw)))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", s.token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := s.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return httpError(resp)
	}
	if out != nil {
		return json.NewDecoder(resp.Body).Decode(out)
	}
	return nil
}

type b2File struct {
	FileName        string `json:"fileName"`
	FileID          string `json:"fileId"`
	ContentLength   int64  `json:"contentLength"`
	UploadTimestamp int64  `json:"uploadTimestamp"`
	Action          string `json:"action"`
}

func (s *b2Store) listNames(prefix, delimiter string, max int) ([]b2File, string, error) {
	var res struct {
		Files        []b2File `json:"files"`
		NextFileName string   `json:"nextFileName"`
	}
	body := map[string]any{
		"bucketId":     s.bucketID,
		"prefix":       prefix,
		"maxFileCount": max,
	}
	if delimiter != "" {
		body["delimiter"] = delimiter
	}
	if err := s.api("b2_list_file_names", body, &res); err != nil {
		return nil, "", err
	}
	return res.Files, res.NextFileName, nil
}

func (s *b2Store) List(prefix, delimiter string) ([]ObjMeta, []string, error) {
	var objs []ObjMeta
	var prefixes []string
	start := ""
	for {
		var res struct {
			Files        []b2File `json:"files"`
			NextFileName string   `json:"nextFileName"`
		}
		body := map[string]any{"bucketId": s.bucketID, "prefix": prefix, "delimiter": delimiter, "maxFileCount": 10000}
		if start != "" {
			body["startFileName"] = start
		}
		if err := s.api("b2_list_file_names", body, &res); err != nil {
			return nil, nil, err
		}
		for _, f := range res.Files {
			if f.Action == "folder" || strings.HasSuffix(f.FileName, "/") {
				if f.FileName == prefix {
					continue
				}
				prefixes = append(prefixes, strings.TrimSuffix(f.FileName, "/")+"/")
				continue
			}
			m := ObjMeta{Key: f.FileName, Size: f.ContentLength}
			if f.UploadTimestamp > 0 {
				m.Mtime = time.UnixMilli(f.UploadTimestamp)
			}
			objs = append(objs, m)
		}
		if res.NextFileName == "" {
			break
		}
		start = res.NextFileName
	}
	return objs, prefixes, nil
}

func (s *b2Store) Head(key string) (*ObjMeta, error) {
	files, _, err := s.listNames(key, "", 1)
	if err != nil {
		return nil, err
	}
	if len(files) == 0 || files[0].FileName != key {
		return nil, nil
	}
	m := &ObjMeta{Key: key, Size: files[0].ContentLength}
	if files[0].UploadTimestamp > 0 {
		m.Mtime = time.UnixMilli(files[0].UploadTimestamp)
	}
	return m, nil
}

func (s *b2Store) Get(key string, off, size int64) ([]byte, error) {
	u := s.downloadURL + "/file/" + url.PathEscape(s.bucketName) + "/" + escapePath(key)
	req, err := http.NewRequest("GET", u, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", s.token)
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

func (s *b2Store) Put(key string, r io.Reader, size int64) error {
	var up struct {
		UploadURL string `json:"uploadUrl"`
		Token     string `json:"authorizationToken"`
	}
	if err := s.api("b2_get_upload_url", map[string]any{"bucketId": s.bucketID}, &up); err != nil {
		return err
	}
	req, err := http.NewRequest("POST", up.UploadURL, r)
	if err != nil {
		return err
	}
	req.ContentLength = size
	req.Header.Set("Authorization", up.Token)
	req.Header.Set("X-Bz-File-Name", escapePath(key))
	req.Header.Set("Content-Type", "b2/x-auto")
	req.Header.Set("X-Bz-Content-Sha1", "do_not_verify")
	resp, err := s.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return httpError(resp)
	}
	return nil
}

func (s *b2Store) fileID(key string) (string, error) {
	files, _, err := s.listNames(key, "", 1)
	if err != nil {
		return "", err
	}
	for _, f := range files {
		if f.FileName == key {
			return f.FileID, nil
		}
	}
	return "", os.ErrNotExist
}

func (s *b2Store) Delete(key string) error {
	id, err := s.fileID(key)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	return s.api("b2_delete_file_version", map[string]any{"fileName": key, "fileId": id}, nil)
}

func (s *b2Store) Copy(src, dst string) error {
	id, err := s.fileID(src)
	if err != nil {
		return err
	}
	return s.api("b2_copy_file", map[string]any{"sourceFileId": id, "fileName": dst}, nil)
}
