package backend

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"
)

func init() {
	Register("azure", func(name string, opts map[string]string) (Backend, error) {
		st, err := newAzure(opts)
		if err != nil {
			return nil, err
		}
		return NewObjectFS(st, strings.Trim(opts["path"], "/")), nil
	})
}

const azureVersion = "2021-08-06"

type azureStore struct {
	client    *http.Client
	account   string
	key       []byte
	baseURL   string // https://account.blob.core.windows.net
	container string
}

func newAzure(opts map[string]string) (*azureStore, error) {
	account := opts["account"]
	key := opts["key"]
	container := opts["container"]
	if account == "" || key == "" || container == "" {
		return nil, fmt.Errorf("azure: account, key and container are required")
	}
	decoded, err := base64.StdEncoding.DecodeString(key)
	if err != nil {
		return nil, fmt.Errorf("azure: account key is not valid base64")
	}
	suffix := Opt(opts, "endpoint", "core.windows.net")
	ep := strings.Trim(opts["url"], "/")
	base := "https://" + account + ".blob." + suffix
	if ep != "" {
		base = ep
	}
	return &azureStore{
		client:    &http.Client{},
		account:   account,
		key:       decoded,
		baseURL:   base,
		container: container,
	}, nil
}

func (s *azureStore) Kind() string { return "azure" }

func (s *azureStore) Init() error {
	_, _, err := s.list("", "/")
	return err
}
func (s *azureStore) Close() error { return nil }

func (s *azureStore) StatFS() (uint64, uint64, uint64) {
	const tb = 1 << 40
	return tb, tb / 2, tb / 2
}

// sign computes the SharedKey Authorization header.
func (s *azureStore) sign(req *http.Request) {
	req.Header.Set("x-ms-date", time.Now().UTC().Format(http.TimeFormat))
	req.Header.Set("x-ms-version", azureVersion)

	// Canonicalized headers.
	var msHeaders []string
	for k := range req.Header {
		lk := strings.ToLower(k)
		if strings.HasPrefix(lk, "x-ms-") {
			msHeaders = append(msHeaders, lk+":"+strings.TrimSpace(req.Header.Get(k)))
		}
	}
	sort.Strings(msHeaders)
	canonHeaders := strings.Join(msHeaders, "\n")

	// Canonicalized resource.
	path := req.URL.EscapedPath()
	if path == "" {
		path = "/"
	}
	canonResource := "/" + s.account + path
	if len(req.URL.Query()) > 0 {
		var params []string
		for k := range req.URL.Query() {
			params = append(params, strings.ToLower(k)+":"+req.URL.Query().Get(k))
		}
		sort.Strings(params)
		canonResource += "\n" + strings.Join(params, "\n")
	}

	contentLength := ""
	if req.ContentLength > 0 {
		contentLength = fmt.Sprintf("%d", req.ContentLength)
	}
	stringToSign := strings.Join([]string{
		req.Method,
		req.Header.Get("Content-Encoding"),
		req.Header.Get("Content-Language"),
		contentLength,
		req.Header.Get("Content-MD5"),
		req.Header.Get("Content-Type"),
		"", // Date (we use x-ms-date)
		req.Header.Get("If-Modified-Since"),
		req.Header.Get("If-Match"),
		req.Header.Get("If-None-Match"),
		req.Header.Get("If-Unmodified-Since"),
		req.Header.Get("Range"),
		canonHeaders,
		canonResource,
	}, "\n")

	mac := hmac.New(sha256.New, s.key)
	mac.Write([]byte(stringToSign))
	sig := base64.StdEncoding.EncodeToString(mac.Sum(nil))
	req.Header.Set("Authorization", "SharedKey "+s.account+":"+sig)
}

func (s *azureStore) blobURL(key string) string {
	return s.baseURL + "/" + s.container + "/" + escapePath(key)
}

func (s *azureStore) do(method, urlPath string, body io.Reader, size int64, headers map[string]string) (*http.Response, error) {
	req, err := http.NewRequest(method, urlPath, body)
	if err != nil {
		return nil, err
	}
	if body != nil {
		req.ContentLength = size
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	s.sign(req)
	return s.client.Do(req)
}

type azureListResult struct {
	Blobs struct {
		Blob []struct {
			Name       string `xml:"Name"`
			Properties struct {
				ContentLength int64  `xml:"Content-Length"`
				LastModified  string `xml:"Last-Modified"`
			} `xml:"Properties"`
		} `xml:"Blob"`
		BlobPrefix []struct {
			Name string `xml:"Name"`
		} `xml:"BlobPrefix"`
	} `xml:"Blobs"`
	NextMarker string `xml:"NextMarker"`
}

func (s *azureStore) list(prefix, delimiter string) ([]ObjMeta, []string, error) {
	var objs []ObjMeta
	var prefixes []string
	marker := ""
	for {
		q := url.Values{}
		q.Set("restype", "container")
		q.Set("comp", "list")
		if prefix != "" {
			q.Set("prefix", prefix)
		}
		if delimiter != "" {
			q.Set("delimiter", delimiter)
		}
		if marker != "" {
			q.Set("marker", marker)
		}
		resp, err := s.do("GET", s.baseURL+"/"+s.container+"?"+q.Encode(), nil, 0, nil)
		if err != nil {
			return nil, nil, err
		}
		var res azureListResult
		decErr := xml.NewDecoder(resp.Body).Decode(&res)
		resp.Body.Close()
		if resp.StatusCode != 200 {
			return nil, nil, fmt.Errorf("azure: list failed: %s", resp.Status)
		}
		if decErr != nil {
			return nil, nil, decErr
		}
		for _, b := range res.Blobs.Blob {
			m := ObjMeta{Key: b.Name, Size: b.Properties.ContentLength}
			if t, err := http.ParseTime(b.Properties.LastModified); err == nil {
				m.Mtime = t
			}
			objs = append(objs, m)
		}
		for _, p := range res.Blobs.BlobPrefix {
			prefixes = append(prefixes, p.Name)
		}
		if res.NextMarker == "" {
			break
		}
		marker = res.NextMarker
	}
	return objs, prefixes, nil
}

func (s *azureStore) List(prefix, delimiter string) ([]ObjMeta, []string, error) {
	return s.list(prefix, delimiter)
}

func (s *azureStore) Head(key string) (*ObjMeta, error) {
	resp, err := s.do("HEAD", s.blobURL(key), nil, 0, nil)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, resp.Body)
	if resp.StatusCode == 404 {
		return nil, nil
	}
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("azure: head: %s", resp.Status)
	}
	m := &ObjMeta{Key: key, Size: resp.ContentLength}
	if lm := resp.Header.Get("Last-Modified"); lm != "" {
		if t, err := http.ParseTime(lm); err == nil {
			m.Mtime = t
		}
	}
	return m, nil
}

func (s *azureStore) Get(key string, off, size int64) ([]byte, error) {
	hdr := map[string]string{}
	if size > 0 {
		hdr["Range"] = fmt.Sprintf("bytes=%d-%d", off, off+size-1)
	}
	resp, err := s.do("GET", s.blobURL(key), nil, 0, hdr)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 && resp.StatusCode != 206 {
		return nil, fmt.Errorf("azure: get: %s", resp.Status)
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

func (s *azureStore) Put(key string, r io.Reader, size int64) error {
	resp, err := s.do("PUT", s.blobURL(key), r, size, map[string]string{
		"x-ms-blob-type": "BlockBlob",
		"Content-Type":   "application/octet-stream",
	})
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, resp.Body)
	if resp.StatusCode != 201 {
		return fmt.Errorf("azure: put: %s", resp.Status)
	}
	return nil
}

func (s *azureStore) Delete(key string) error {
	resp, err := s.do("DELETE", s.blobURL(key), nil, 0, nil)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, resp.Body)
	if resp.StatusCode == 202 || resp.StatusCode == 404 {
		return nil
	}
	return fmt.Errorf("azure: delete: %s", resp.Status)
}

func (s *azureStore) Copy(src, dst string) error {
	source := s.baseURL + "/" + s.container + "/" + escapePath(src)
	resp, err := s.do("PUT", s.blobURL(dst), nil, 0, map[string]string{
		"x-ms-copy-source": source,
	})
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, resp.Body)
	if resp.StatusCode == 202 || resp.StatusCode == 201 {
		return nil
	}
	return fmt.Errorf("azure: copy: %s", resp.Status)
}
