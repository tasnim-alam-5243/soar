package adapters

import (
	"context"
	"encoding/xml"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/yourorg/soar/internal/domain"
)

type memCursor struct {
	mu sync.Mutex
	v  string
}

func (m *memCursor) Load(context.Context) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.v, nil
}

func (m *memCursor) Save(_ context.Context, v string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.v = v
	return nil
}

var fileNow = time.Date(2026, 11, 11, 18, 0, 0, 0, time.UTC)

// writeAged writes a file last modified age before fileNow.
func writeAged(t *testing.T, dir, name, body string, age time.Duration) {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(p, fileNow.Add(-age), fileNow.Add(-age)); err != nil {
		t.Fatal(err)
	}
}

func newFO(t *testing.T, cfg domain.JSON, deps StreamDeps) *fileObject {
	t.Helper()
	deps.Now = func() time.Time { return fileNow }
	st, err := newFileObject(domain.Source{Name: "files", Config: cfg}, deps)
	if err != nil {
		t.Fatal(err)
	}
	f := st.(*fileObject)
	f.interval = 10 * time.Millisecond
	return f
}

// runFO runs the stream until want messages arrived and one more round passed.
func runFO(t *testing.T, f *fileObject, c *collect, want int) error {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- f.Run(ctx, c.emit) }()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) && len(c.bodies()) < want {
		select {
		case err := <-done:
			cancel()
			return err
		case <-time.After(10 * time.Millisecond):
		}
	}
	time.Sleep(100 * time.Millisecond) // a wrongly repeated object would show up now
	cancel()
	return <-done
}

func ids(c *collect) []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	var out []string
	for _, m := range c.msgs {
		out = append(out, m.ID)
	}
	return out
}

// keep mode: settled regular files in name order; dot files, folders, links and files
// still being written are left; the cursor moves past each queued file, so a restart
// reads only what is new.
func TestFileObjectLocalKeep(t *testing.T) {
	allowed := t.TempDir()
	dir := filepath.Join(allowed, "drop")
	if err := os.MkdirAll(filepath.Join(dir, "sub"), 0o700); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(t.TempDir(), "secret")
	if err := os.WriteFile(outside, []byte("not for ingest"), 0o600); err != nil {
		t.Fatal(err)
	}
	writeAged(t, dir, "002.json", `{"n":2}`, time.Minute)
	writeAged(t, dir, "001.ndjson", "{\"n\":1}\n{\"n\":11}\n", time.Minute)
	writeAged(t, dir, ".003.partial", `{"n":3}`, time.Minute)
	writeAged(t, dir, "009.json", `{"n":9}`, time.Second) // younger than settle
	if err := os.Symlink(outside, filepath.Join(dir, "005-link.json")); err != nil {
		t.Fatal(err)
	}
	cur := &memCursor{}
	cfg := domain.JSON{"url": "file://" + dir}
	deps := StreamDeps{Cursor: cur, LocalDirs: []string{allowed}}

	c := &collect{refuse: 1}
	if err := runFO(t, newFO(t, cfg, deps), c, 2); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(ids(c), ","); got != "001.ndjson,002.json" {
		t.Fatalf("ids %s", got)
	}
	if b := c.bodies(); b[0] != "{\"n\":1}\n{\"n\":11}\n" || b[1] != `{"n":2}` {
		t.Errorf("bodies %q", b)
	}
	c.mu.Lock()
	meta := c.msgs[0].Meta
	c.mu.Unlock()
	if meta["transport"] != "file_object" || meta["object"] != "001.ndjson" || meta["dir"] != dir || meta["size"] != int64(17) {
		t.Errorf("meta %v", meta)
	}
	if v, _ := cur.Load(context.Background()); v != "002.json" {
		t.Errorf("cursor %q", v)
	}
	for _, n := range []string{"001.ndjson", "002.json"} {
		if _, err := os.Stat(filepath.Join(dir, n)); err != nil {
			t.Errorf("keep mode removed %s", n)
		}
	}

	// restart: only the new, settled file
	writeAged(t, dir, "003.json", `{"n":3}`, time.Minute)
	c2 := &collect{}
	if err := runFO(t, newFO(t, cfg, deps), c2, 1); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(ids(c2), ","); got != "003.json" {
		t.Errorf("after restart %s", got)
	}
}

// delete mode: each file is removed only after it is queued, and a later file under
// the same name is read again (it is a new object).
func TestFileObjectLocalDelete(t *testing.T) {
	dir := t.TempDir()
	writeAged(t, dir, "alerts.json", `{"n":1}`, time.Minute)
	writeAged(t, dir, "big.json", strings.Repeat("x", 200), time.Minute)
	deps := StreamDeps{LocalDirs: []string{dir}}
	cfg := domain.JSON{"url": "file://" + dir, "after": "delete", "max_object_bytes": 100}

	c := &collect{refuse: 2}
	if err := runFO(t, newFO(t, cfg, deps), c, 1); err != nil {
		t.Fatal(err)
	}
	if b := c.bodies(); len(b) != 1 || b[0] != `{"n":1}` {
		t.Fatalf("bodies %q", b)
	}
	if _, err := os.Stat(filepath.Join(dir, "alerts.json")); !os.IsNotExist(err) {
		t.Errorf("a queued file must be deleted: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "big.json")); err != nil {
		t.Errorf("an oversized file is skipped, not deleted: %v", err)
	}

	writeAged(t, dir, "alerts.json", `{"n":2}`, 30*time.Second)
	c2 := &collect{}
	if err := runFO(t, newFO(t, cfg, deps), c2, 1); err != nil {
		t.Fatal(err)
	}
	got := ids(c2)
	if len(got) != 1 || c2.bodies()[0] != `{"n":2}` || got[0] == ids(c)[0] {
		t.Errorf("the rewritten file must be read as a new object: %v / %v", got, ids(c))
	}
}

// Local directories must be inside SOAR_INGEST_DIRS, links resolved.
func TestFileObjectLocalDirectoryAllowlist(t *testing.T) {
	allowed := t.TempDir()
	other := t.TempDir()
	link := filepath.Join(allowed, "escape")
	if err := os.Symlink(other, link); err != nil {
		t.Fatal(err)
	}
	sibling := allowed + "-sibling"
	if err := os.Mkdir(sibling, 0o700); err != nil {
		t.Fatal(err)
	}
	defer os.Remove(sibling)
	for name, c := range map[string]struct {
		dir     string
		allowed []string
	}{
		"none allowed":      {other, nil},
		"outside":           {other, []string{allowed}},
		"link out":          {link, []string{allowed}},
		"name prefix":       {sibling, []string{allowed}},
		"parent of allowed": {filepath.Dir(allowed), []string{allowed}},
	} {
		f := newFO(t, domain.JSON{"url": "file://" + c.dir}, StreamDeps{Cursor: &memCursor{}, LocalDirs: c.allowed})
		err := f.Run(context.Background(), (&collect{}).emit)
		if err == nil || !strings.Contains(err.Error(), "SOAR_INGEST_DIRS") {
			t.Errorf("%s: want a SOAR_INGEST_DIRS error, got %v", name, err)
		}
	}
}

// fakeS3 serves ListObjectsV2, GET and DELETE for one bucket.
type fakeS3 struct {
	mu      sync.Mutex
	objects map[string]string
	lists   int
}

func (s *fakeS3) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	bucket, key, _ := strings.Cut(strings.TrimPrefix(r.URL.Path, "/"), "/")
	if bucket != "logs" || !strings.HasPrefix(r.Header.Get("Authorization"), "AWS4-HMAC-SHA256 Credential=AK/") {
		http.Error(w, "denied", http.StatusForbidden)
		return
	}
	switch {
	case r.Method == http.MethodGet && key == "" && r.URL.Query().Get("list-type") == "2":
		s.lists++
		q := r.URL.Query()
		max, _ := strconv.Atoi(q.Get("max-keys"))
		var keys []string
		for k := range s.objects {
			if strings.HasPrefix(k, q.Get("prefix")) && k > q.Get("start-after") {
				keys = append(keys, k)
			}
		}
		sort.Strings(keys)
		type content struct {
			Key          string
			Size         int
			LastModified string
			ETag         string
		}
		out := struct {
			XMLName     xml.Name `xml:"ListBucketResult"`
			IsTruncated bool
			Contents    []content
		}{}
		if len(keys) > max {
			keys, out.IsTruncated = keys[:max], true
		}
		for _, k := range keys {
			out.Contents = append(out.Contents, content{k, len(s.objects[k]), "2026-11-11T17:00:00.000Z", `"` + strconv.Itoa(len(s.objects[k])) + `"`})
		}
		_ = xml.NewEncoder(w).Encode(out)
	case r.Method == http.MethodGet:
		body, ok := s.objects[key]
		if !ok {
			http.Error(w, "<Error><Code>NoSuchKey</Code></Error>", http.StatusNotFound)
			return
		}
		_, _ = io.WriteString(w, body)
	case r.Method == http.MethodDelete:
		delete(s.objects, key)
		w.WriteHeader(http.StatusNoContent)
	default:
		http.Error(w, "unsupported", http.StatusBadRequest)
	}
}

func (s *fakeS3) keys() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []string
	for k := range s.objects {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func s3Deps(srv *httptest.Server, cur CursorStore) StreamDeps {
	conn := &domain.Connection{
		Name: "s3-dev", ConnectorID: "s3", Config: domain.JSON{"endpoint": srv.URL},
		SecretRefs: domain.JSON{"access_key": "AK", "secret_key": "SK"},
	}
	return StreamDeps{Conn: conn, Resolve: func(ref string) (string, error) { return ref, nil }, Cursor: cur}
}

// S3, keep mode: pages through the prefix (folder markers and other prefixes left
// out), resumes after the cursor, and never deletes.
func TestFileObjectS3Keep(t *testing.T) {
	fake := &fakeS3{objects: map[string]string{
		"in/2026/": "", "in/2026/a.json": `{"n":1}`, "in/2026/b.json": `{"n":2}`, "in/2026/c/": "",
		"in/2026/d.json": `{"n":4}`, "in/2026/e.json": `{"n":5}`, "other/x.json": `{"n":0}`,
	}}
	srv := httptest.NewServer(fake)
	defer srv.Close()
	cur := &memCursor{}
	cfg := domain.JSON{"url": "s3://logs/in/", "connection": "s3-dev"}
	f := newFO(t, cfg, s3Deps(srv, cur))
	f.page = 2
	c := &collect{}
	if err := runFO(t, f, c, 4); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(ids(c), ","); got != "in/2026/a.json,in/2026/b.json,in/2026/d.json,in/2026/e.json" {
		t.Fatalf("ids %s", got)
	}
	c.mu.Lock()
	meta := c.msgs[0].Meta
	c.mu.Unlock()
	if meta["bucket"] != "logs" || meta["object"] != "in/2026/a.json" || meta["last_modified"] != "2026-11-11T17:00:00Z" {
		t.Errorf("meta %v", meta)
	}
	if v, _ := cur.Load(context.Background()); v != "in/2026/e.json" {
		t.Errorf("cursor %q", v)
	}
	if len(fake.keys()) != 7 {
		t.Errorf("keep mode deleted objects: %v", fake.keys())
	}

	fake.mu.Lock()
	fake.objects["in/2026/f.json"] = `{"n":6}`
	fake.mu.Unlock()
	f2 := newFO(t, cfg, s3Deps(srv, cur))
	c2 := &collect{}
	if err := runFO(t, f2, c2, 1); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(ids(c2), ","); got != "in/2026/f.json" {
		t.Errorf("after restart %s", got)
	}
}

// S3, delete mode: every queued object is deleted; folder markers and other prefixes stay.
func TestFileObjectS3Delete(t *testing.T) {
	fake := &fakeS3{objects: map[string]string{
		"in/a.json": `{"n":1}`, "in/b.json": `{"n":2}`, "in/c/": "", "in/d.json": `{"n":4}`, "other/x.json": `{"n":0}`,
	}}
	srv := httptest.NewServer(fake)
	defer srv.Close()
	f := newFO(t, domain.JSON{"url": "s3://logs/in/", "connection": "s3-dev", "after": "delete"}, s3Deps(srv, nil))
	f.page = 2
	c := &collect{}
	if err := runFO(t, f, c, 3); err != nil {
		t.Fatal(err)
	}
	if b := c.bodies(); strings.Join(b, ",") != `{"n":1},{"n":2},{"n":4}` {
		t.Errorf("bodies %v", b)
	}
	if got := strings.Join(fake.keys(), ","); got != "in/c/,other/x.json" {
		t.Errorf("left %s", got)
	}
}

func TestFileObjectConfigValidation(t *testing.T) {
	for name, cfg := range map[string]domain.JSON{
		"no url":        {},
		"bad scheme":    {"url": "ftp://host/dir"},
		"relative":      {"url": "file://dir/x"},
		"no bucket":     {"url": "s3:///prefix", "connection": "c"},
		"s3 no conn":    {"url": "s3://logs/in/"},
		"bad after":     {"url": "file:///var/spool/soar", "after": "move"},
		"bad settle":    {"url": "file:///var/spool/soar", "settle": "soon"},
		"neg settle":    {"url": "file:///var/spool/soar", "settle": "-1s"},
		"bad url parse": {"url": "s3://%zz"},
	} {
		if _, err := newFileObject(domain.Source{Config: cfg}, StreamDeps{}); err == nil {
			t.Errorf("%s: want an error", name)
		}
	}
	if _, err := newFileObject(domain.Source{Config: domain.JSON{"url": "s3://logs/in/", "connection": "c"}}, StreamDeps{}); err != nil {
		t.Errorf("source-only validation: %v", err)
	}
	// an s3 source without its connection fails when it starts
	f := newFO(t, domain.JSON{"url": "s3://logs/in/", "connection": "c"}, StreamDeps{Cursor: &memCursor{}})
	if err := f.Run(context.Background(), (&collect{}).emit); err == nil {
		t.Error("s3 without a connection must fail")
	}
}
