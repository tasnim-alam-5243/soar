package blob

import (
	"context"
	"encoding/xml"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"
)

// AWS documentation examples for Signature Version 4 (S3 "GET Object" and presigned URL).
const (
	awsAK = "AKIAIOSFODNN7EXAMPLE"
	awsSK = "wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY"
)

func TestSigV4HeaderMatchesAWSExample(t *testing.T) {
	req, _ := http.NewRequest(http.MethodGet, "https://examplebucket.s3.amazonaws.com/test.txt", nil)
	req.Header.Set("Range", "bytes=0-9")
	signV4(req, awsAK, awsSK, "us-east-1", "s3", emptySHA256, time.Date(2013, 5, 24, 0, 0, 0, 0, time.UTC))
	want := "f0e8bdb87c964420e857bd35b5d6ed310bd44f0170aba48dd91039c6036bdb41"
	if got := req.Header.Get("Authorization"); !strings.HasSuffix(got, "Signature="+want) {
		t.Fatalf("signature mismatch:\n%s", got)
	}
}

func TestPresignMatchesAWSExample(t *testing.T) {
	u, _ := url.Parse("https://examplebucket.s3.amazonaws.com/test.txt")
	got := presignV4(u, awsAK, awsSK, "us-east-1", "s3", time.Date(2013, 5, 24, 0, 0, 0, 0, time.UTC), 86400*time.Second)
	want := "aeeed9bbccd4d02ee5c0109b86d86835f995330da4c265957d157751f604d404"
	if !strings.HasSuffix(got, "X-Amz-Signature="+want) {
		t.Fatalf("presign mismatch:\n%s", got)
	}
}

func TestRefRoundTripAndOffload(t *testing.T) {
	ctx := context.Background()
	m := NewMemory()
	small := map[string]any{"a": 1}
	v, ref, err := Offload(ctx, m, BucketPayloads, small)
	if err != nil || ref != "" || v == nil {
		t.Fatalf("small payload must stay inline: %v %q %v", v, ref, err)
	}
	big := map[string]any{"s": strings.Repeat("x", InlineLimit+10)}
	v, ref, err = Offload(ctx, m, BucketPayloads, big)
	if err != nil || ref == "" || v != nil {
		t.Fatalf("big payload must be offloaded: %q %v", ref, err)
	}
	if !strings.HasPrefix(ref, "blob://soar-payloads/") {
		t.Fatal(ref)
	}
	back, err := Load(ctx, m, ref)
	if err != nil || len(back.(map[string]any)["s"].(string)) != InlineLimit+10 {
		t.Fatalf("load: %v", err)
	}
	if _, err := ParseRef("http://x"); err == nil {
		t.Fatal("bad ref accepted")
	}
}

func TestFSStore(t *testing.T) {
	ctx := context.Background()
	f := FS{Root: t.TempDir()}
	ref, err := PutBytes(ctx, f, BucketEvidence, []byte("hello"), "text/plain")
	if err != nil {
		t.Fatal(err)
	}
	if ref.SHA256 == "" || ref.Size != 5 {
		t.Fatalf("ref %+v", ref)
	}
	data, err := GetBytes(ctx, f, ref, 100)
	if err != nil || string(data) != "hello" {
		t.Fatalf("%q %v", data, err)
	}
	if err := f.Delete(ctx, ref); err != nil {
		t.Fatal(err)
	}
	if _, err := f.Get(ctx, ref); !errors.Is(err, ErrNotFound) {
		t.Fatalf("want not found, got %v", err)
	}
	if _, err := f.Put(ctx, "b", "../../etc/passwd", strings.NewReader("x"), 1, nil); err == nil {
		t.Fatal("path traversal accepted")
	}
}

// fakeS3 checks that requests are signed and stores objects.
func TestS3ClientAgainstFake(t *testing.T) {
	objs := map[string]string{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.Header.Get("Authorization"), "AWS4-HMAC-SHA256 Credential=ak/") {
			w.WriteHeader(http.StatusForbidden)
			return
		}
		switch r.Method {
		case http.MethodPut:
			b, _ := io.ReadAll(r.Body)
			objs[r.URL.Path] = string(b)
		case http.MethodGet:
			v, ok := objs[r.URL.Path]
			if !ok {
				w.WriteHeader(http.StatusNotFound)
				_, _ = w.Write([]byte("<Error><Code>NoSuchKey</Code></Error>"))
				return
			}
			_, _ = w.Write([]byte(v))
		case http.MethodDelete:
			delete(objs, r.URL.Path)
			w.WriteHeader(http.StatusNoContent)
		}
	}))
	defer srv.Close()
	s, err := NewS3(srv.URL, "ak", "sk", "", true)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	ref, err := PutBytes(ctx, s, BucketPayloads, []byte(`{"x":1}`), "application/json")
	if err != nil {
		t.Fatal(err)
	}
	data, err := GetBytes(ctx, s, ref, 100)
	if err != nil || string(data) != `{"x":1}` {
		t.Fatalf("%q %v", data, err)
	}
	if err := s.Delete(ctx, ref); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Get(ctx, ref); !errors.Is(err, ErrNotFound) {
		t.Fatalf("want not found, got %v", err)
	}
	if u, err := s.PresignGet(ctx, ref, time.Minute); err != nil || !strings.Contains(u, "X-Amz-Signature=") {
		t.Fatalf("presign %q %v", u, err)
	}
}

// EnsureBucket creates the bucket (an existing one is fine) and installs the expiry rule
// as a namespaced S3 LifecycleConfiguration with a matching Content-MD5.
func TestEnsureBucketCreatesBucketAndLifecycleRule(t *testing.T) {
	var mu sync.Mutex
	buckets := map[string]bool{"soar-existing": true}
	rules := map[string]string{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		if !strings.HasPrefix(r.Header.Get("Authorization"), "AWS4-HMAC-SHA256 Credential=ak/") || r.Method != http.MethodPut {
			w.WriteHeader(http.StatusForbidden)
			return
		}
		bucket := strings.Trim(r.URL.Path, "/")
		if _, ok := r.URL.Query()["lifecycle"]; !ok {
			if buckets[bucket] {
				w.WriteHeader(http.StatusConflict)
				_, _ = w.Write([]byte("<Error><Code>BucketAlreadyOwnedByYou</Code></Error>"))
				return
			}
			buckets[bucket] = true
			return
		}
		body, _ := io.ReadAll(r.Body)
		if r.Header.Get("Content-Md5") != md5Base64(body) {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte("<Error><Code>BadDigest</Code></Error>"))
			return
		}
		var cfg struct {
			XMLName xml.Name `xml:"http://s3.amazonaws.com/doc/2006-03-01/ LifecycleConfiguration"`
			Rules   []struct {
				ID         string `xml:"ID"`
				Status     string `xml:"Status"`
				Prefix     string `xml:"Filter>Prefix"`
				ExpireDays int    `xml:"Expiration>Days"`
			} `xml:"Rule"`
		}
		if err := xml.Unmarshal(body, &cfg); err != nil || len(cfg.Rules) != 1 || !buckets[bucket] {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte("<Error><Code>MalformedXML</Code></Error>"))
			return
		}
		rl := cfg.Rules[0]
		rules[bucket] = rl.ID + "/" + rl.Status + "/" + rl.Prefix + "/" + time.Duration(rl.ExpireDays*24*int(time.Hour)).String()
	}))
	defer srv.Close()
	s, err := NewS3(srv.URL, "ak", "sk", "", false)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	for _, c := range []struct {
		bucket string
		days   int
	}{{"soar-raw", 7}, {"soar-existing", 30}, {"soar-evidence", 0}} {
		if err := s.EnsureBucket(ctx, c.bucket, c.days); err != nil {
			t.Fatalf("%s: %v", c.bucket, err)
		}
	}
	if !buckets["soar-raw"] || !buckets["soar-evidence"] {
		t.Fatalf("buckets %v", buckets)
	}
	want := map[string]string{"soar-raw": "soar-retention/Enabled//168h0m0s", "soar-existing": "soar-retention/Enabled//720h0m0s"}
	if len(rules) != len(want) || rules["soar-raw"] != want["soar-raw"] || rules["soar-existing"] != want["soar-existing"] {
		t.Fatalf("lifecycle rules %v, want %v (no rule for evidence)", rules, want)
	}
}
