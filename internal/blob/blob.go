// Package blob is the only access path to object storage (spec section 5.1). The
// interface is S3-shaped so any S3-compatible store works (the dev stack runs RustFS); FS and
// Memory implementations serve development and tests.
//
// Write order rule: upload the object first, then write the document or publish the
// message that references it. An orphaned object is harmless (lifecycle expiry
// removes it); a dangling reference is never created.
package blob

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/yourorg/soar/internal/domain"
)

// Buckets (spec 5.1).
const (
	BucketRaw      = "soar-raw"
	BucketPayloads = "soar-payloads"
	BucketEvidence = "soar-evidence"
	BucketExports  = "soar-exports"
)

// InlineLimit is the size above which payloads travel by reference (spec: 64 KB).
const InlineLimit = 64 << 10

// ErrNotFound is returned when an object does not exist.
var ErrNotFound = errors.New("blob: not found")

// Ref identifies a stored object: blob://<bucket>/<yyyy>/<mm>/<dd>/<ulid>, with the
// SHA-256 and size kept beside it.
type Ref struct {
	Bucket string `json:"bucket"`
	Key    string `json:"key"`
	SHA256 string `json:"sha256,omitempty"`
	Size   int64  `json:"size,omitempty"`
}

// String renders the reference URI.
func (r Ref) String() string { return "blob://" + r.Bucket + "/" + r.Key }

// ParseRef parses a blob:// URI.
func ParseRef(s string) (Ref, error) {
	rest, ok := strings.CutPrefix(s, "blob://")
	bucket, key, ok2 := strings.Cut(rest, "/")
	if !ok || !ok2 || bucket == "" || key == "" {
		return Ref{}, fmt.Errorf("blob: %q is not a blob:// reference", s)
	}
	return Ref{Bucket: bucket, Key: key}, nil
}

// Blob is the storage interface of spec 5.1.
type Blob interface {
	Put(ctx context.Context, bucket, key string, r io.Reader, size int64, meta map[string]string) (Ref, error)
	Get(ctx context.Context, ref Ref) (io.ReadCloser, error)
	Delete(ctx context.Context, ref Ref) error
	PresignGet(ctx context.Context, ref Ref, ttl time.Duration) (string, error)
}

// NewKey returns <yyyy>/<mm>/<dd>/<ulid> for t.
func NewKey(t time.Time) string {
	t = t.UTC()
	return fmt.Sprintf("%04d/%02d/%02d/%s", t.Year(), t.Month(), t.Day(), domain.NewULID(t))
}

// PutBytes stores b under a fresh key and returns its reference with hash and size.
func PutBytes(ctx context.Context, b Blob, bucket string, data []byte, contentType string) (Ref, error) {
	sum := sha256.Sum256(data)
	ref, err := b.Put(ctx, bucket, NewKey(time.Now()), bytes.NewReader(data), int64(len(data)), map[string]string{"content-type": contentType})
	if err != nil {
		return Ref{}, err
	}
	ref.SHA256, ref.Size = hex.EncodeToString(sum[:]), int64(len(data))
	return ref, nil
}

// GetBytes reads a whole object (callers only use it for payloads, which are bounded).
func GetBytes(ctx context.Context, b Blob, ref Ref, max int64) ([]byte, error) {
	rc, err := b.Get(ctx, ref)
	if err != nil {
		return nil, err
	}
	defer rc.Close()
	data, err := io.ReadAll(io.LimitReader(rc, max+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > max {
		return nil, fmt.Errorf("blob: %s is larger than %d bytes", ref, max)
	}
	return data, nil
}

// Offload returns v inline when its JSON is at most InlineLimit bytes, otherwise it
// stores the JSON in bucket and returns the reference URI. b may be nil, in which
// case large values are returned inline (development without object storage).
func Offload(ctx context.Context, b Blob, bucket string, v any) (inline any, ref string, err error) {
	data, err := json.Marshal(v)
	if err != nil {
		return nil, "", err
	}
	if len(data) <= InlineLimit || b == nil {
		return v, "", nil
	}
	r, err := PutBytes(ctx, b, bucket, data, "application/json")
	if err != nil {
		return nil, "", err
	}
	return nil, r.String(), nil
}

// Load reads a JSON value stored by Offload.
func Load(ctx context.Context, b Blob, ref string) (any, error) {
	if b == nil {
		return nil, fmt.Errorf("blob: %s cannot be read: no object storage configured", ref)
	}
	r, err := ParseRef(ref)
	if err != nil {
		return nil, err
	}
	data, err := GetBytes(ctx, b, r, 64<<20)
	if err != nil {
		return nil, err
	}
	var v any
	return v, json.Unmarshal(data, &v)
}
