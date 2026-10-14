package backend

import (
	"bytes"
	"crypto/tls"
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

func init() {
	Register("webdav", func(name string, opts map[string]string) (Backend, error) {
		return newWebDAV(name, opts)
	})
}

type webDAV struct {
	name   string
	base   *url.URL // full base URL including any sub-path
	user   string
	pass   string
	client *http.Client
}

func newWebDAV(name string, opts map[string]string) (*webDAV, error) {
	raw := opts["url"]
	if raw == "" {
		return nil, fmt.Errorf("webdav: url is required")
	}
	u, err := url.Parse(raw)
	if err != nil {
		return nil, fmt.Errorf("webdav: bad url: %w", err)
	}
	if u.Scheme == "" {
		u.Scheme = "https"
	}
	if sub := strings.Trim(opts["path"], "/"); sub != "" {
		u.Path = strings.TrimRight(u.Path, "/") + "/" + sub
	}
	tr := &http.Transport{
		TLSClientConfig: &tls.Config{InsecureSkipVerify: OptBool(opts, "insecure", false)},
	}
	return &webDAV{
		name:   name,
		base:   u,
		user:   opts["user"],
		pass:   opts["password"],
		client: &http.Client{Transport: tr, Timeout: 0},
	}, nil
}

func (b *webDAV) Kind() string { return "webdav" }
func (b *webDAV) Init() error {
	// Verify the base is reachable.
	_, err := b.propfind("/", "0")
	return err
}
func (b *webDAV) Close() error { return nil }

func (b *webDAV) urlFor(path string, withSlash bool) string {
	u := *b.base
	clean := Clean(path)
	esc := strings.TrimPrefix(u.EscapedPath(), "/")
	full := "/"
	if esc != "" {
		full = "/" + esc
	}
	if clean != "/" {
		full = strings.TrimRight(full, "/") + escapePath(clean)
	}
	if withSlash && !strings.HasSuffix(full, "/") {
		full += "/"
	}
	u.Path = ""
	u.RawPath = ""
	u.Opaque = ""
	return strings.TrimRight(u.String(), "/") + full
}

func escapePath(p string) string {
	parts := strings.Split(strings.Trim(p, "/"), "/")
	for i, s := range parts {
		parts[i] = url.PathEscape(s)
	}
	return "/" + strings.Join(parts, "/")
}

func (b *webDAV) do(method, url string, body io.Reader, hdr map[string]string) (*http.Response, error) {
	req, err := http.NewRequest(method, url, body)
	if err != nil {
		return nil, err
	}
	if b.user != "" || b.pass != "" {
		req.SetBasicAuth(b.user, b.pass)
	}
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	return b.client.Do(req)
}

// -- PROPFIND parsing ------------------------------------------------------

type davMultiStatus struct {
	Responses []davResponse `xml:"response"`
}
type davResponse struct {
	Href      string        `xml:"href"`
	Propstats []davPropstat `xml:"propstat"`
}
type davPropstat struct {
	Status string  `xml:"status"`
	Prop   davProp `xml:"prop"`
}
type davProp struct {
	ContentLength string          `xml:"getcontentlength"`
	LastModified  string          `xml:"getlastmodified"`
	ResourceType  davResourceType `xml:"resourcetype"`
	QuotaAvail    string          `xml:"quota-available-bytes"`
	QuotaUsed     string          `xml:"quota-used-bytes"`
}
type davResourceType struct {
	Collection *struct{} `xml:"collection"`
}

const propfindBody = `<?xml version="1.0" encoding="utf-8"?>
<d:propfind xmlns:d="DAV:"><d:prop>
<d:resourcetype/><d:getcontentlength/><d:getlastmodified/>
<d:quota-available-bytes/><d:quota-used-bytes/>
</d:prop></d:propfind>`

func (b *webDAV) propfind(path, depth string) (*davMultiStatus, error) {
	url := b.urlFor(path, path == "/")
	resp, err := b.do("PROPFIND", url, strings.NewReader(propfindBody), map[string]string{"Depth": depth})
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	switch {
	case resp.StatusCode == 207 || resp.StatusCode == 200:
	case resp.StatusCode == 401:
		return nil, os.ErrPermission
	case resp.StatusCode == 404:
		return nil, os.ErrNotExist
	default:
		return nil, fmt.Errorf("webdav: PROPFIND %s: %s", path, resp.Status)
	}
	var ms davMultiStatus
	if err := xml.NewDecoder(resp.Body).Decode(&ms); err != nil {
		return nil, fmt.Errorf("webdav: parse: %w", err)
	}
	return &ms, nil
}

func (b *webDAV) propFor(path string) (*davProp, error) {
	ms, err := b.propfind(path, "0")
	if err != nil {
		return nil, err
	}
	if len(ms.Responses) == 0 {
		return nil, os.ErrNotExist
	}
	return &ms.Responses[0].Propstats[0].Prop, nil
}

func toAttr(p *davProp) *Attr {
	a := &Attr{IsDir: p.ResourceType.Collection != nil}
	if n, err := strconv.ParseInt(p.ContentLength, 10, 64); err == nil {
		a.Size = n
	}
	if t, err := http.ParseTime(p.LastModified); err == nil {
		a.Mtime = t
		a.Atime = t
		a.Ctime = t
	}
	if a.IsDir {
		a.Mode = 0o755
	} else {
		a.Mode = 0o644
	}
	return a
}

func (b *webDAV) Stat(path string) (*Attr, error) {
	if Clean(path) == "/" {
		return &Attr{IsDir: true, Mode: 0o755}, nil
	}
	p, err := b.propFor(path)
	if err != nil {
		return nil, err
	}
	// Some servers report no resourcetype for collections; probe with trailing slash.
	if p.ResourceType.Collection == nil {
		if p2, err2 := b.propFor(strings.TrimRight(path, "/") + "/"); err2 == nil {
			if a := toAttr(p2); a.IsDir {
				return a, nil
			}
		}
	}
	return toAttr(p), nil
}

func (b *webDAV) ReadDir(path string) ([]DirEntry, error) {
	ms, err := b.propfind(path, "1")
	if err != nil {
		return nil, err
	}
	self := Clean(path)
	var out []DirEntry
	for i, r := range ms.Responses {
		child := b.fusePath(r.Href)
		if Clean(child) == self {
			continue
		}
		if i == 0 && Clean(child) == self {
			continue
		}
		prop := r.Propstats[0].Prop
		a := toAttr(&prop)
		out = append(out, DirEntry{
			Name:  Base(strings.TrimRight(child, "/")),
			IsDir: a.IsDir,
			Size:  a.Size,
			Mtime: a.Mtime,
			Mode:  a.Mode,
		})
	}
	SortEntries(out)
	return out, nil
}

// fusePath converts a DAV href into a path relative to our root.
func (b *webDAV) fusePath(href string) string {
	p := href
	if u, err := url.Parse(href); err == nil && u.Path != "" {
		p = u.Path
	}
	if dec, err := url.PathUnescape(p); err == nil {
		p = dec
	}
	basePath := strings.TrimRight(b.base.Path, "/")
	if basePath != "" && strings.HasPrefix(p, basePath) {
		p = p[len(basePath):]
	}
	if p == "" {
		p = "/"
	}
	return Clean(p)
}

func (b *webDAV) Mkdir(path string, mode uint32) error {
	resp, err := b.do("MKCOL", b.urlFor(path, false), nil, nil)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode == 201 || resp.StatusCode == 200 || resp.StatusCode == 405 {
		return nil
	}
	return webdavErr(resp)
}

func (b *webDAV) Remove(path string) error    { return b.delete(path) }
func (b *webDAV) RemoveDir(path string) error { return b.delete(path) }

func (b *webDAV) delete(path string) error {
	resp, err := b.do("DELETE", b.urlFor(path, false), nil, nil)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode == 404 {
		return os.ErrNotExist
	}
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return nil
	}
	return webdavErr(resp)
}

func (b *webDAV) Rename(oldPath, newPath string) error {
	src := b.urlFor(oldPath, false)
	dst := b.urlFor(newPath, false)
	hdr := map[string]string{"Destination": dst, "Overwrite": "T"}
	resp, err := b.do("MOVE", src, nil, hdr)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return nil
	}
	// Fall back to copy+delete for servers without MOVE.
	if resp.StatusCode == 405 || resp.StatusCode == 501 {
		if err := b.copyFallback(oldPath, newPath); err != nil {
			return err
		}
		return b.delete(oldPath)
	}
	return webdavErr(resp)
}

func (b *webDAV) copyFallback(oldPath, newPath string) error {
	rh, err := b.OpenRead(oldPath)
	if err != nil {
		return err
	}
	defer rh.Close()
	return b.putStream(newPath, io.NewSectionReader(rh, 0, rh.Size()), rh.Size())
}

func (b *webDAV) Truncate(path string, size int64) error {
	// Download, truncate locally, re-upload.
	rh, err := b.OpenRead(path)
	if err != nil {
		return err
	}
	data := make([]byte, size)
	if size > 0 {
		if _, err := rh.ReadAt(data, 0); err != nil && err != io.EOF {
			rh.Close()
			return err
		}
	}
	rh.Close()
	return b.putStream(path, bytes.NewReader(data), size)
}

func (b *webDAV) Chmod(path string, mode uint32) error { return nil }
func (b *webDAV) Chtimes(path string, a, m time.Time) error {
	return nil
}

func (b *webDAV) StatFS() (uint64, uint64, uint64) {
	const tb = 1 << 40
	return tb, tb / 2, tb / 2
}

func (b *webDAV) OpenRead(path string) (ReadHandle, error) {
	a, err := b.Stat(path)
	if err != nil {
		return nil, err
	}
	return &webdavRead{b: b, path: path, size: a.Size}, nil
}

func (b *webDAV) OpenWrite(path string, truncate bool) (WriteHandle, error) {
	return NewStagedWriter(path, b.putStream, nil)
}

func (b *webDAV) putStream(path string, r io.Reader, size int64) error {
	resp, err := b.do("PUT", b.urlFor(path, false), r, nil)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return nil
	}
	return webdavErr(resp)
}

type webdavRead struct {
	b    *webDAV
	path string
	size int64
}

func (h *webdavRead) Size() int64  { return h.size }
func (h *webdavRead) Close() error { return nil }

func (h *webdavRead) ReadAt(p []byte, off int64) (int, error) {
	if off >= h.size {
		return 0, io.EOF
	}
	end := off + int64(len(p)) - 1
	if end >= h.size {
		end = h.size - 1
	}
	hdr := map[string]string{"Range": fmt.Sprintf("bytes=%d-%d", off, end)}
	resp, err := h.b.do("GET", h.b.urlFor(h.path, false), nil, hdr)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == 401 {
		return 0, os.ErrPermission
	}
	if resp.StatusCode == 404 {
		return 0, os.ErrNotExist
	}
	if resp.StatusCode != 206 && resp.StatusCode != 200 {
		return 0, fmt.Errorf("webdav: GET %s: %s", h.path, resp.Status)
	}
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return 0, err
	}
	if resp.StatusCode == 200 && off > 0 {
		// Server ignored Range; slice it out.
		if off >= int64(len(data)) {
			return 0, io.EOF
		}
		data = data[off:]
	}
	n := copy(p, data)
	if int64(n) < h.size-off {
		return n, nil
	}
	return n, io.EOF
}

func webdavErr(resp *http.Response) error {
	switch resp.StatusCode {
	case 401, 403:
		return os.ErrPermission
	case 404:
		return os.ErrNotExist
	case 405:
		return ErrNotSupported
	case 507:
		return fmt.Errorf("webdav: insufficient storage")
	}
	return fmt.Errorf("webdav: %s", resp.Status)
}
