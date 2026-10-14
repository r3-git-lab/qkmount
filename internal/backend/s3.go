package backend

import (
	"crypto/hmac"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
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
	Register("s3", func(name string, opts map[string]string) (Backend, error) {
		st, err := newS3Store("s3", opts)
		if err != nil {
			return nil, err
		}
		return NewObjectFS(st, Opt(opts, "path", "")), nil
	})
}

// ---------------------------------------------------------------------------
// AWS Signature Version 4
// ---------------------------------------------------------------------------

func hmacSHA256(key, data []byte) []byte {
	h := hmac.New(sha256.New, key)
	h.Write(data)
	return h.Sum(nil)
}

func sha256hex(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// awsURIEncode implements the AWS canonical URI encoding.
func awsURIEncode(s string, encodeSlash bool) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'A' && c <= 'Z', c >= 'a' && c <= 'z', c >= '0' && c <= '9',
			c == '-', c == '_', c == '.', c == '~':
			b.WriteByte(c)
		case c == '/' && !encodeSlash:
			b.WriteByte(c)
		default:
			fmt.Fprintf(&b, "%%%02X", c)
		}
	}
	return b.String()
}

func canonicalQuery(v url.Values) string {
	keys := make([]string, 0, len(v))
	for k := range v {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var parts []string
	for _, k := range keys {
		vals := v[k]
		sort.Strings(vals)
		for _, val := range vals {
			parts = append(parts, awsURIEncode(k, true)+"="+awsURIEncode(val, true))
		}
	}
	return strings.Join(parts, "&")
}

// signV4 computes the Authorization header.  headers must contain at least
// host, x-amz-date and x-amz-content-sha256.  signedHeaders must be sorted.
func signV4(method, canonicalURI, canonicalQueryStr string, headers map[string]string,
	signedHeaders []string, payloadHash string, t time.Time,
	region, service, accessKey, secretKey string) string {

	amzDate := t.UTC().Format("20060102T150405Z")
	dateStamp := t.UTC().Format("20060102")

	var ch strings.Builder
	for _, h := range signedHeaders {
		ch.WriteString(h)
		ch.WriteByte(':')
		ch.WriteString(strings.TrimSpace(headers[h]))
		ch.WriteByte('\n')
	}
	canonicalHeaders := ch.String()
	signed := strings.Join(signedHeaders, ";")

	canonicalRequest := strings.Join([]string{
		method,
		canonicalURI,
		canonicalQueryStr,
		canonicalHeaders,
		signed,
		payloadHash,
	}, "\n")

	scope := strings.Join([]string{dateStamp, region, service, "aws4_request"}, "/")
	stringToSign := strings.Join([]string{
		"AWS4-HMAC-SHA256",
		amzDate,
		scope,
		sha256hex([]byte(canonicalRequest)),
	}, "\n")

	kDate := hmacSHA256([]byte("AWS4"+secretKey), []byte(dateStamp))
	kRegion := hmacSHA256(kDate, []byte(region))
	kService := hmacSHA256(kRegion, []byte(service))
	kSigning := hmacSHA256(kService, []byte("aws4_request"))
	signature := hex.EncodeToString(hmacSHA256(kSigning, []byte(stringToSign)))

	return "AWS4-HMAC-SHA256 Credential=" + accessKey + "/" + scope +
		", SignedHeaders=" + signed + ", Signature=" + signature
}

// ---------------------------------------------------------------------------
// S3 store
// ---------------------------------------------------------------------------

type s3Store struct {
	kind      string
	scheme    string
	host      string // host header (may include bucket for virtual-host style)
	region    string
	bucket    string
	accessKey string
	secretKey string
	token     string
	pathStyle bool
	client    *http.Client
}

func newS3Store(kind string, opts map[string]string) (*s3Store, error) {
	bucket := opts["bucket"]
	if bucket == "" {
		return nil, fmt.Errorf("s3: bucket is required")
	}
	region := Opt(opts, "region", "us-east-1")
	accessKey := opts["access_key"]
	secretKey := opts["secret_key"]
	client := &http.Client{
		Transport: &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: OptBool(opts, "insecure", false)}},
	}
	st := &s3Store{
		kind:      kind,
		scheme:    "https",
		region:    region,
		bucket:    bucket,
		accessKey: accessKey,
		secretKey: secretKey,
		token:     opts["session_token"],
		client:    client,
	}
	endpoint := opts["endpoint"]
	if endpoint == "" {
		// AWS default (virtual-hosted style).
		st.host = fmt.Sprintf("%s.s3.%s.amazonaws.com", bucket, region)
		st.pathStyle = false
	} else {
		u, err := url.Parse(endpoint)
		if err != nil {
			return nil, fmt.Errorf("s3: bad endpoint: %w", err)
		}
		if u.Scheme != "" {
			st.scheme = u.Scheme
		}
		st.host = u.Host
		st.pathStyle = OptBool(opts, "path_style", true)
		if u.Path != "" && u.Path != "/" {
			st.host = u.Host
			// custom base path is uncommon; fold into bucket key handling
		}
	}
	return st, nil
}

func (s *s3Store) Kind() string { return s.kind }

func (s *s3Store) Init() error {
	_, _, err := s.listRaw("", "/", "")
	return err
}
func (s *s3Store) Close() error { return nil }

// pathFor builds the request path (canonical URI) for an object key.
func (s *s3Store) pathFor(key string) string {
	enc := awsURIEncode(key, false)
	if s.pathStyle {
		return "/" + s.bucket + "/" + enc
	}
	return "/" + enc
}

func (s *s3Store) bucketPath() string {
	if s.pathStyle {
		return "/" + s.bucket
	}
	return "/"
}

func (s *s3Store) do(method, canonicalURI, rawQuery string, query url.Values,
	body io.Reader, size int64, extraHeaders map[string]string) (*http.Response, error) {

	t := time.Now().UTC()
	payloadHash := sha256hex(nil)
	if body != nil {
		payloadHash = "UNSIGNED-PAYLOAD"
	}
	headers := map[string]string{
		"host":                 s.host,
		"x-amz-content-sha256": payloadHash,
		"x-amz-date":           t.Format("20060102T150405Z"),
	}
	if s.token != "" {
		headers["x-amz-security-token"] = s.token
	}
	for k, v := range extraHeaders {
		headers[strings.ToLower(k)] = v
	}
	signedHeaders := make([]string, 0, len(headers))
	for k := range headers {
		signedHeaders = append(signedHeaders, k)
	}
	sort.Strings(signedHeaders)

	canonicalQ := ""
	if query != nil {
		canonicalQ = canonicalQuery(query)
	} else if rawQuery != "" {
		canonicalQ = rawQuery
	}

	auth := signV4(method, canonicalURI, canonicalQ, headers, signedHeaders,
		payloadHash, t, s.region, "s3", s.accessKey, s.secretKey)

	urlStr := s.scheme + "://" + s.host + canonicalURI
	if canonicalQ != "" {
		urlStr += "?" + canonicalQ
	}
	req, err := http.NewRequest(method, urlStr, body)
	if err != nil {
		return nil, err
	}
	for k, v := range headers {
		switch strings.ToLower(k) {
		case "host":
			req.Host = v
		default:
			req.Header.Set(k, v)
		}
	}
	req.Header.Set("Authorization", auth)
	if body != nil {
		req.ContentLength = size
	}
	return s.client.Do(req)
}

type s3ListResult struct {
	XMLName     xml.Name `xml:"ListBucketResult"`
	IsTruncated bool     `xml:"IsTruncated"`
	NextToken   string   `xml:"NextContinuationToken"`
	Contents    []struct {
		Key          string `xml:"Key"`
		Size         int64  `xml:"Size"`
		LastModified string `xml:"LastModified"`
	} `xml:"Contents"`
	CommonPrefixes []struct {
		Prefix string `xml:"Prefix"`
	} `xml:"CommonPrefixes"`
}

func (s *s3Store) listRaw(prefix, delimiter, token string) ([]ObjMeta, []string, error) {
	q := url.Values{}
	q.Set("list-type", "2")
	if prefix != "" {
		q.Set("prefix", prefix)
	}
	if delimiter != "" {
		q.Set("delimiter", delimiter)
	}
	if token != "" {
		q.Set("continuation-token", token)
	}
	resp, err := s.do("GET", s.bucketPath(), "", q, nil, 0, nil)
	if err != nil {
		return nil, nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return nil, nil, s3Err(resp)
	}
	var lr s3ListResult
	if err := xml.NewDecoder(resp.Body).Decode(&lr); err != nil {
		return nil, nil, err
	}
	var objs []ObjMeta
	for _, c := range lr.Contents {
		m := ObjMeta{Key: c.Key, Size: c.Size}
		if t, err := time.Parse(time.RFC3339, c.LastModified); err == nil {
			m.Mtime = t
		}
		objs = append(objs, m)
	}
	var prefixes []string
	for _, p := range lr.CommonPrefixes {
		prefixes = append(prefixes, p.Prefix)
	}
	if lr.IsTruncated && lr.NextToken != "" {
		moreObjs, morePrefixes, err := s.listRaw(prefix, delimiter, lr.NextToken)
		if err != nil {
			return objs, prefixes, nil
		}
		objs = append(objs, moreObjs...)
		prefixes = append(prefixes, morePrefixes...)
	}
	return objs, prefixes, nil
}

func (s *s3Store) List(prefix, delimiter string) ([]ObjMeta, []string, error) {
	return s.listRaw(prefix, delimiter, "")
}

func (s *s3Store) Head(key string) (*ObjMeta, error) {
	resp, err := s.do("HEAD", s.pathFor(key), "", nil, nil, 0, nil)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == 404 {
		return nil, nil
	}
	if resp.StatusCode != 200 {
		return nil, s3Err(resp)
	}
	m := &ObjMeta{Key: key, Size: resp.ContentLength}
	if lm := resp.Header.Get("Last-Modified"); lm != "" {
		if t, err := http.ParseTime(lm); err == nil {
			m.Mtime = t
		}
	}
	return m, nil
}

func (s *s3Store) Get(key string, off, size int64) ([]byte, error) {
	hdr := map[string]string{}
	if size > 0 {
		hdr["Range"] = fmt.Sprintf("bytes=%d-%d", off, off+size-1)
	}
	resp, err := s.do("GET", s.pathFor(key), "", nil, nil, 0, hdr)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == 404 {
		return nil, ErrNotSupported
	}
	if resp.StatusCode != 200 && resp.StatusCode != 206 {
		return nil, s3Err(resp)
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
	if size >= 0 && int64(len(data)) > size {
		data = data[:size]
	}
	return data, nil
}

func (s *s3Store) Put(key string, r io.Reader, size int64) error {
	resp, err := s.do("PUT", s.pathFor(key), "", nil, r, size, nil)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, resp.Body)
	if resp.StatusCode == 200 {
		return nil
	}
	return s3Err(resp)
}

func (s *s3Store) Delete(key string) error {
	resp, err := s.do("DELETE", s.pathFor(key), "", nil, nil, 0, nil)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode == 204 || resp.StatusCode == 200 || resp.StatusCode == 404 {
		return nil
	}
	return s3Err(resp)
}

func (s *s3Store) Copy(src, dst string) error {
	source := "/" + s.bucket + "/" + awsURIEncode(src, false)
	if !s.pathStyle {
		source = "/" + awsURIEncode(src, false)
	}
	hdr := map[string]string{"x-amz-copy-source": source}
	resp, err := s.do("PUT", s.pathFor(dst), "", nil, nil, 0, hdr)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode == 200 {
		return nil
	}
	return s3Err(resp)
}

func (s *s3Store) StatFS() (uint64, uint64, uint64) {
	const tb = 1 << 40
	return tb, tb / 2, tb / 2
}

type s3ErrorDoc struct {
	Code    string `xml:"Code"`
	Message string `xml:"Message"`
}

func s3Err(resp *http.Response) error {
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	var doc s3ErrorDoc
	_ = xml.Unmarshal(body, &doc)
	switch resp.StatusCode {
	case 403:
		return fmt.Errorf("s3: access denied (%s)", doc.Code)
	case 404:
		return ErrNotSupported
	}
	if doc.Code != "" {
		return fmt.Errorf("s3: %s: %s", doc.Code, doc.Message)
	}
	return fmt.Errorf("s3: %s", resp.Status)
}
