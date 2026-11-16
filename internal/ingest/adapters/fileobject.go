package adapters

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/yourorg/soar/internal/blob"
	"github.com/yourorg/soar/internal/domain"
	"github.com/yourorg/soar/internal/obs"
)

func init() {
	RegisterStream("file_object", StreamSpec{New: newFileObject, Exclusive: true})
}

// file_object reads files that land in an S3 bucket prefix or a local directory
// (Step 10), in key order, one object at a time: the object is parsed with the
// source's parser (json, ndjson, syslog, ...) and queued. Runs on one collector
// replica (lease).
//
// Source config:
//   - url: "s3://bucket/prefix" (config.connection names a connection with
//     config.endpoint, config.region and secret_refs access_key and secret_key) or
//     "file:///absolute/dir", which must be inside a directory the collector allows
//     (SOAR_INGEST_DIRS; none by default).
//   - after: keep (default) or delete. keep remembers the last key handled (the source
//     cursor) and reads only keys that sort after it, so write new objects under
//     increasing names (a timestamp prefix); a rewritten object is not read again.
//     delete removes each object once it is queued and reads whatever is there, so
//     names may repeat; an object that cannot be deleted stops the source (it would be
//     read again).
//   - max_object_bytes: default 64 MiB; larger objects are skipped and counted as dropped.
//   - settle: local files modified more recently are left for a later round (default 10s).
//
// The source's schedule sets how often to look (default 30s).
type fileObject struct {
	source   string
	kind     string // s3 | file
	bucket   string
	prefix   string // s3 key prefix, or the directory
	after    string
	maxBytes int64
	settle   time.Duration
	interval time.Duration
	deps     StreamDeps
	log      *slog.Logger
	s3       *blob.S3
	root     *os.Root        // file: the directory, opened while running
	skipped  map[string]bool // delete mode: oversized objects already reported
	page     int             // keys per list request
}

const fileObjectPage = 100 // keys per S3 list request

func newFileObject(src domain.Source, deps StreamDeps) (Stream, error) {
	f := &fileObject{
		source: src.Name, after: "keep", maxBytes: 64 << 20, settle: 10 * time.Second,
		interval: 30 * time.Second, deps: deps, log: deps.Log, skipped: map[string]bool{}, page: fileObjectPage,
	}
	if f.log == nil {
		f.log = slog.Default()
	}
	raw, _ := src.Config["url"].(string)
	u, err := url.Parse(raw)
	if err != nil || raw == "" {
		return nil, fmt.Errorf("config.url must be s3://bucket/prefix or file:///directory, got %q", raw)
	}
	switch u.Scheme {
	case "s3":
		if u.Host == "" {
			return nil, fmt.Errorf("config.url s3://bucket/prefix needs a bucket")
		}
		if name, _ := src.Config["connection"].(string); name == "" {
			return nil, fmt.Errorf("an s3:// url needs config.connection (endpoint and keys)")
		}
		f.kind, f.bucket, f.prefix = "s3", u.Host, strings.TrimPrefix(u.Path, "/")
	case "file":
		if u.Host != "" || !filepath.IsAbs(u.Path) {
			return nil, fmt.Errorf("config.url file:///directory must be an absolute path, got %q", raw)
		}
		f.kind, f.prefix = "file", filepath.Clean(u.Path)
	default:
		return nil, fmt.Errorf("config.url must start with s3:// or file://, got %q", raw)
	}
	if a, _ := src.Config["after"].(string); a != "" {
		if a != "keep" && a != "delete" {
			return nil, fmt.Errorf("config.after must be keep or delete, got %q", a)
		}
		f.after = a
	}
	if n := intOf(src.Config["max_object_bytes"]); n > 0 {
		f.maxBytes = int64(n)
	}
	if s, _ := src.Config["settle"].(string); s != "" {
		d, err := time.ParseDuration(s)
		if err != nil || d < 0 {
			return nil, fmt.Errorf("config.settle must be a duration, got %q", s)
		}
		f.settle = d
	}
	if src.Schedule != "" {
		if d, err := ParseSchedule(src.Schedule); err == nil {
			f.interval = d
		}
	}
	return f, nil
}

func s3Client(deps StreamDeps) (*blob.S3, error) {
	c := deps.Conn
	endpoint, _ := c.Config["endpoint"].(string)
	region, _ := c.Config["region"].(string)
	ref := func(k string) (string, error) {
		r, _ := c.SecretRefs[k].(string)
		if r == "" {
			return "", fmt.Errorf("connection %q: secret_refs.%s is required", c.Name, k)
		}
		return deps.Resolve(r)
	}
	ak, err := ref("access_key")
	if err != nil {
		return nil, err
	}
	sk, err := ref("secret_key")
	if err != nil {
		return nil, err
	}
	return blob.NewS3(endpoint, ak, sk, region, false)
}

// openDir opens the source directory if it lies inside one of the allowed directories
// (symbolic links resolved). The returned root keeps every later open, stat and remove
// inside it, so a link placed in the directory cannot lead elsewhere.
func openDir(dir string, allowed []string) (*os.Root, error) {
	real, err := filepath.EvalSymlinks(dir)
	if err != nil {
		return nil, err
	}
	for _, a := range allowed {
		ra, err := filepath.EvalSymlinks(a)
		if err != nil {
			continue
		}
		if rel, err := filepath.Rel(ra, real); err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return os.OpenRoot(real)
		}
	}
	return nil, fmt.Errorf("directory %s is not inside a directory this collector may read (SOAR_INGEST_DIRS)", dir)
}

type object struct {
	key      string
	version  string // s3 ETag, or the file's modification time and size
	size     int64
	modified time.Time
}

func (f *fileObject) Run(ctx context.Context, emit Emit) error {
	switch f.kind {
	case "s3":
		if f.deps.Conn == nil || f.deps.Resolve == nil {
			return fmt.Errorf("s3 file_object sources need a connection with endpoint and keys")
		}
		cl, err := s3Client(f.deps)
		if err != nil {
			return err
		}
		f.s3 = cl
	case "file":
		root, err := openDir(f.prefix, f.deps.LocalDirs)
		if err != nil {
			return err
		}
		defer root.Close()
		f.root = root
	}
	if f.after == "keep" && f.deps.Cursor == nil {
		return fmt.Errorf("file_object needs the source cursor")
	}
	for ctx.Err() == nil {
		if err := f.round(ctx, emit); err != nil {
			if ctx.Err() != nil {
				return nil // stopped while a request was in flight
			}
			return err
		}
		sleepCtx(ctx, f.interval)
	}
	return nil
}

// round handles every object present: keep mode starts after the cursor, delete mode
// at the beginning (handled objects are gone).
func (f *fileObject) round(ctx context.Context, emit Emit) error {
	start := ""
	if f.after == "keep" {
		c, err := f.deps.Cursor.Load(ctx)
		if err != nil {
			return err
		}
		start = c
	}
	for ctx.Err() == nil {
		objs, next, more, err := f.list(ctx, start)
		if err != nil {
			return err
		}
		for _, o := range objs {
			if err := f.handle(ctx, o, emit); err != nil || ctx.Err() != nil {
				return err
			}
		}
		if !more || next <= start {
			return nil
		}
		start = next
	}
	return nil
}

func (f *fileObject) handle(ctx context.Context, o object, emit Emit) error {
	id := o.key
	if f.after == "delete" {
		id = o.key + "@" + o.version // a later object under the same name is new
	}
	if o.size > f.maxBytes {
		if !f.skipped[id] {
			f.log.Warn("object larger than max_object_bytes; skipped", "object", o.key, "size", o.size, "limit", f.maxBytes)
			obs.SourceEvents.WithLabelValues(f.source, "dropped").Inc()
		}
		if f.after == "keep" {
			return f.deps.Cursor.Save(ctx, o.key)
		}
		f.skipped[id] = true // left in place for the operator
		return nil
	}
	body, err := f.read(ctx, o)
	if err != nil {
		return fmt.Errorf("read %s: %w", o.key, err)
	}
	meta := map[string]any{
		"transport": "file_object", "object": o.key, "size": o.size,
		"last_modified": o.modified.UTC().Format(time.RFC3339Nano),
	}
	if f.kind == "s3" {
		meta["bucket"] = f.bucket
	} else {
		meta["dir"] = f.prefix
	}
	// the object counts as handled only once it is queued; a repeat after a crash
	// carries the same id, which the queue deduplicates
	for attempt := 0; ; attempt++ {
		err := emit(ctx, []Message{{Body: body, ID: id, Meta: meta}})
		if err == nil {
			break
		}
		if ctx.Err() != nil {
			return nil
		}
		f.log.Warn("object not queued yet; retrying", "object", o.key, "error", err.Error())
		sleepCtx(ctx, min(200*time.Millisecond<<min(attempt, 5), 5*time.Second))
	}
	if f.after == "keep" {
		return f.deps.Cursor.Save(ctx, o.key)
	}
	if err := f.remove(ctx, o); err != nil {
		return fmt.Errorf("delete handled object %s: %w", o.key, err)
	}
	return nil
}

func (f *fileObject) now() time.Time {
	if f.deps.Now != nil {
		return f.deps.Now()
	}
	return time.Now()
}

// list returns the objects after a key, the key the next page starts after (the last
// one listed, folder markers included) and whether more pages follow.
func (f *fileObject) list(ctx context.Context, after string) ([]object, string, bool, error) {
	if f.kind == "s3" {
		infos, more, err := f.s3.List(ctx, f.bucket, f.prefix, after, f.page)
		if err != nil || len(infos) == 0 {
			return nil, after, false, err
		}
		out := make([]object, 0, len(infos))
		for _, i := range infos {
			if strings.HasSuffix(i.Key, "/") {
				continue // folder marker
			}
			out = append(out, object{key: i.Key, version: i.ETag, size: i.Size, modified: i.LastModified})
		}
		return out, infos[len(infos)-1].Key, more, nil
	}
	d, err := f.root.Open(".")
	if err != nil {
		return nil, after, false, err
	}
	entries, err := d.ReadDir(-1)
	_ = d.Close()
	if err != nil {
		return nil, after, false, err
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		// regular files only (no links or folders); dot files are partial uploads
		if e.Type().IsRegular() && e.Name() > after && !strings.HasPrefix(e.Name(), ".") {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)
	now := f.now()
	var out []object
	for _, n := range names {
		info, err := f.root.Lstat(n)
		if err != nil || !info.Mode().IsRegular() {
			continue
		}
		if now.Sub(info.ModTime()) < f.settle {
			break // still being written: keep the order and look again next round
		}
		out = append(out, object{
			key: n, size: info.Size(), modified: info.ModTime(),
			version: fmt.Sprintf("%d-%d", info.ModTime().UnixNano(), info.Size()),
		})
	}
	return out, after, false, nil
}

func (f *fileObject) read(ctx context.Context, o object) ([]byte, error) {
	var rc io.ReadCloser
	if f.kind == "s3" {
		r, err := f.s3.Get(ctx, blob.Ref{Bucket: f.bucket, Key: o.key})
		if err != nil {
			return nil, err
		}
		rc = r
	} else {
		r, err := f.root.Open(o.key)
		if err != nil {
			return nil, err
		}
		rc = r
	}
	defer rc.Close()
	b, err := io.ReadAll(io.LimitReader(rc, f.maxBytes+1))
	if err != nil {
		return nil, err
	}
	if int64(len(b)) > f.maxBytes {
		return nil, errors.New("object grew beyond max_object_bytes while reading")
	}
	return b, nil
}

func (f *fileObject) remove(ctx context.Context, o object) error {
	if f.kind == "s3" {
		return f.s3.Delete(ctx, blob.Ref{Bucket: f.bucket, Key: o.key})
	}
	return f.root.Remove(o.key)
}
