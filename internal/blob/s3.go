package blob

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/md5"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"
)

// S3 talks to any S3-compatible store (AWS S3, RustFS, MinIO, Ceph RGW, ...) with
// path-style requests and AWS Signature Version 4. It has no dependency beyond the
// standard library.
type S3 struct {
	Endpoint  string // http://s3:9000 (dev compose), https://s3.example
	Region    string // default us-east-1
	AccessKey string
	SecretKey string
	SSE       bool // request server-side encryption (x-amz-server-side-encryption: AES256)
	HTTP      *http.Client
	now       func() time.Time
}

// NewS3 validates the settings and returns a client.
func NewS3(endpoint, accessKey, secretKey, region string, sse bool) (*S3, error) {
	u, err := url.Parse(endpoint)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return nil, fmt.Errorf("blob: endpoint %q must be an http(s) URL", endpoint)
	}
	if accessKey == "" || secretKey == "" {
		return nil, fmt.Errorf("blob: access key and secret key are required")
	}
	if region == "" {
		region = "us-east-1"
	}
	return &S3{
		Endpoint: strings.TrimRight(endpoint, "/"), Region: region, AccessKey: accessKey, SecretKey: secretKey,
		SSE: sse, HTTP: &http.Client{Timeout: 5 * time.Minute}, now: time.Now,
	}, nil
}

func (s *S3) objectURL(bucket, key string) string {
	return s.Endpoint + "/" + bucket + "/" + escapePath(key)
}

func (s *S3) do(ctx context.Context, method, rawURL string, body io.Reader, size int64, hdr http.Header) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, method, rawURL, body)
	if err != nil {
		return nil, err
	}
	if size >= 0 && body != nil {
		req.ContentLength = size
	}
	for k, v := range hdr {
		req.Header[k] = v
	}
	payloadHash := "UNSIGNED-PAYLOAD"
	if body == nil {
		payloadHash = emptySHA256
	}
	if b, ok := body.(*bytes.Reader); ok && b.Len() < 1<<20 {
		data, _ := io.ReadAll(b)
		sum := sha256.Sum256(data)
		payloadHash = hex.EncodeToString(sum[:])
		req.Body = io.NopCloser(bytes.NewReader(data))
		req.GetBody = func() (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(data)), nil }
	}
	signV4(req, s.AccessKey, s.SecretKey, s.Region, "s3", payloadHash, s.now().UTC())
	return s.HTTP.Do(req)
}

func s3Error(resp *http.Response) error {
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	var e struct {
		Code    string `xml:"Code"`
		Message string `xml:"Message"`
	}
	_ = xml.Unmarshal(b, &e)
	if resp.StatusCode == http.StatusNotFound || e.Code == "NoSuchKey" {
		return ErrNotFound
	}
	return fmt.Errorf("blob: s3 %s: HTTP %d %s %s", resp.Request.Method, resp.StatusCode, e.Code, e.Message)
}

func (s *S3) Put(ctx context.Context, bucket, key string, r io.Reader, size int64, meta map[string]string) (Ref, error) {
	h := http.Header{}
	for k, v := range meta {
		if strings.EqualFold(k, "content-type") {
			h.Set("Content-Type", v)
			continue
		}
		h.Set("X-Amz-Meta-"+k, v)
	}
	if s.SSE {
		h.Set("X-Amz-Server-Side-Encryption", "AES256")
	}
	resp, err := s.do(ctx, http.MethodPut, s.objectURL(bucket, key), r, size, h)
	if err != nil {
		return Ref{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return Ref{}, s3Error(resp)
	}
	return Ref{Bucket: bucket, Key: key, Size: size}, nil
}

func (s *S3) Get(ctx context.Context, ref Ref) (io.ReadCloser, error) {
	resp, err := s.do(ctx, http.MethodGet, s.objectURL(ref.Bucket, ref.Key), nil, -1, nil)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode/100 != 2 {
		defer resp.Body.Close()
		return nil, s3Error(resp)
	}
	return resp.Body, nil
}

func (s *S3) Delete(ctx context.Context, ref Ref) error {
	resp, err := s.do(ctx, http.MethodDelete, s.objectURL(ref.Bucket, ref.Key), nil, -1, nil)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return s3Error(resp)
	}
	return nil
}

// PresignGet returns a short-lived download URL (query-string SigV4).
func (s *S3) PresignGet(_ context.Context, ref Ref, ttl time.Duration) (string, error) {
	if ttl <= 0 || ttl > 7*24*time.Hour {
		return "", fmt.Errorf("blob: presign ttl must be between 1s and 7d")
	}
	u, err := url.Parse(s.objectURL(ref.Bucket, ref.Key))
	if err != nil {
		return "", err
	}
	return presignV4(u, s.AccessKey, s.SecretKey, s.Region, "s3", s.now().UTC(), ttl), nil
}

// EnsureBucket creates a bucket if it does not exist and applies an expiry lifecycle
// rule (days > 0) generated from retention.yaml (spec 5.2).
func (s *S3) EnsureBucket(ctx context.Context, bucket string, expireDays int) error {
	resp, err := s.do(ctx, http.MethodPut, s.Endpoint+"/"+bucket, nil, -1, nil)
	if err != nil {
		return err
	}
	resp.Body.Close()
	if resp.StatusCode/100 != 2 && resp.StatusCode != http.StatusConflict {
		return fmt.Errorf("blob: create bucket %s: HTTP %d", bucket, resp.StatusCode)
	}
	if expireDays <= 0 {
		return nil
	}
	// The S3 namespace is what AWS SDKs send; MinIO ignores it, stricter servers expect it.
	body := fmt.Sprintf(`<LifecycleConfiguration xmlns="http://s3.amazonaws.com/doc/2006-03-01/"><Rule><ID>soar-retention</ID><Status>Enabled</Status><Filter><Prefix></Prefix></Filter><Expiration><Days>%d</Days></Expiration></Rule></LifecycleConfiguration>`, expireDays)
	sum := md5Base64([]byte(body))
	h := http.Header{"Content-Md5": {sum}, "Content-Type": {"application/xml"}}
	resp, err = s.do(ctx, http.MethodPut, s.Endpoint+"/"+bucket+"?lifecycle=", bytes.NewReader([]byte(body)), int64(len(body)), h)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return s3Error(resp)
	}
	return nil
}

// Ping checks that the endpoint answers (used by /readyz).
func (s *S3) Ping(ctx context.Context) error {
	resp, err := s.do(ctx, http.MethodGet, s.Endpoint+"/minio/health/live", nil, -1, nil)
	if err != nil {
		return err
	}
	resp.Body.Close()
	if resp.StatusCode >= 500 {
		return fmt.Errorf("blob: endpoint answered HTTP %d", resp.StatusCode)
	}
	return nil
}

// ---- Signature Version 4 ----

const emptySHA256 = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"

func hmacSHA256(key []byte, data string) []byte {
	h := hmac.New(sha256.New, key)
	h.Write([]byte(data))
	return h.Sum(nil)
}

func sha256Hex(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

func signingKey(secret, date, region, service string) []byte {
	k := hmacSHA256([]byte("AWS4"+secret), date)
	k = hmacSHA256(k, region)
	k = hmacSHA256(k, service)
	return hmacSHA256(k, "aws4_request")
}

// escapePath encodes each path segment per SigV4 (unreserved characters kept).
func escapePath(p string) string {
	parts := strings.Split(p, "/")
	for i, s := range parts {
		parts[i] = uriEncode(s, true)
	}
	return strings.Join(parts, "/")
}

func uriEncode(s string, encodeSlash bool) string {
	var b strings.Builder
	for _, c := range []byte(s) {
		switch {
		case 'A' <= c && c <= 'Z', 'a' <= c && c <= 'z', '0' <= c && c <= '9', c == '-', c == '_', c == '.', c == '~':
			b.WriteByte(c)
		case c == '/' && !encodeSlash:
			b.WriteByte(c)
		default:
			fmt.Fprintf(&b, "%%%02X", c)
		}
	}
	return b.String()
}

func canonicalQuery(q url.Values) string {
	keys := make([]string, 0, len(q))
	for k := range q {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var parts []string
	for _, k := range keys {
		vs := append([]string(nil), q[k]...)
		sort.Strings(vs)
		for _, v := range vs {
			parts = append(parts, uriEncode(k, true)+"="+uriEncode(v, true))
		}
	}
	return strings.Join(parts, "&")
}

func canonicalPath(u *url.URL) string {
	p := u.EscapedPath()
	if p == "" {
		return "/"
	}
	return p
}

// signV4 adds the Authorization header (header-based signing).
func signV4(req *http.Request, ak, sk, region, service, payloadHash string, t time.Time) {
	amzDate := t.Format("20060102T150405Z")
	date := t.Format("20060102")
	req.Header.Set("X-Amz-Date", amzDate)
	req.Header.Set("X-Amz-Content-Sha256", payloadHash)
	host := req.URL.Host
	if req.Host != "" {
		host = req.Host
	}

	headers := map[string]string{"host": host}
	for k, v := range req.Header {
		lk := strings.ToLower(k)
		if lk == "host" || strings.HasPrefix(lk, "x-amz-") || lk == "content-type" || lk == "content-md5" || lk == "range" {
			headers[lk] = strings.TrimSpace(strings.Join(v, ","))
		}
	}
	names := make([]string, 0, len(headers))
	for k := range headers {
		names = append(names, k)
	}
	sort.Strings(names)
	var ch strings.Builder
	for _, n := range names {
		ch.WriteString(n + ":" + headers[n] + "\n")
	}
	signed := strings.Join(names, ";")
	creq := strings.Join([]string{req.Method, canonicalPath(req.URL), canonicalQuery(req.URL.Query()), ch.String(), signed, payloadHash}, "\n")
	scope := date + "/" + region + "/" + service + "/aws4_request"
	sts := "AWS4-HMAC-SHA256\n" + amzDate + "\n" + scope + "\n" + sha256Hex(creq)
	sig := hex.EncodeToString(hmacSHA256(signingKey(sk, date, region, service), sts))
	req.Header.Set("Authorization", "AWS4-HMAC-SHA256 Credential="+ak+"/"+scope+", SignedHeaders="+signed+", Signature="+sig)
}

// presignV4 returns a URL signed in the query string, valid for ttl.
func presignV4(u *url.URL, ak, sk, region, service string, t time.Time, ttl time.Duration) string {
	amzDate := t.Format("20060102T150405Z")
	date := t.Format("20060102")
	scope := date + "/" + region + "/" + service + "/aws4_request"
	q := u.Query()
	q.Set("X-Amz-Algorithm", "AWS4-HMAC-SHA256")
	q.Set("X-Amz-Credential", ak+"/"+scope)
	q.Set("X-Amz-Date", amzDate)
	q.Set("X-Amz-Expires", strconv.Itoa(int(ttl.Seconds())))
	q.Set("X-Amz-SignedHeaders", "host")
	creq := strings.Join([]string{http.MethodGet, canonicalPath(u), canonicalQuery(q), "host:" + u.Host + "\n", "host", "UNSIGNED-PAYLOAD"}, "\n")
	sts := "AWS4-HMAC-SHA256\n" + amzDate + "\n" + scope + "\n" + sha256Hex(creq)
	sig := hex.EncodeToString(hmacSHA256(signingKey(sk, date, region, service), sts))
	return u.Scheme + "://" + u.Host + canonicalPath(u) + "?" + canonicalQuery(q) + "&X-Amz-Signature=" + sig
}

func md5Base64(b []byte) string {
	sum := md5.Sum(b)
	return base64.StdEncoding.EncodeToString(sum[:])
}
