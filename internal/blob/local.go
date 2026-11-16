package blob

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// Memory keeps objects in process memory (tests).
type Memory struct {
	mu   sync.Mutex
	objs map[string][]byte
}

// NewMemory returns an empty in-memory store.
func NewMemory() *Memory { return &Memory{objs: map[string][]byte{}} }

func (m *Memory) Put(_ context.Context, bucket, key string, r io.Reader, _ int64, _ map[string]string) (Ref, error) {
	data, err := io.ReadAll(r)
	if err != nil {
		return Ref{}, err
	}
	m.mu.Lock()
	m.objs[bucket+"/"+key] = data
	m.mu.Unlock()
	return Ref{Bucket: bucket, Key: key, Size: int64(len(data))}, nil
}

func (m *Memory) Get(_ context.Context, ref Ref) (io.ReadCloser, error) {
	m.mu.Lock()
	data, ok := m.objs[ref.Bucket+"/"+ref.Key]
	m.mu.Unlock()
	if !ok {
		return nil, ErrNotFound
	}
	return io.NopCloser(bytes.NewReader(data)), nil
}

func (m *Memory) Delete(_ context.Context, ref Ref) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.objs[ref.Bucket+"/"+ref.Key]; !ok {
		return ErrNotFound
	}
	delete(m.objs, ref.Bucket+"/"+ref.Key)
	return nil
}

func (m *Memory) PresignGet(_ context.Context, ref Ref, _ time.Duration) (string, error) {
	return "memory://" + ref.Bucket + "/" + ref.Key, nil
}

// Len reports the number of stored objects.
func (m *Memory) Len() int { m.mu.Lock(); defer m.mu.Unlock(); return len(m.objs) }

// FS stores objects as files under a root directory (single-node development).
type FS struct{ Root string }

func (f FS) path(bucket, key string) (string, error) {
	p := filepath.Join(f.Root, bucket, filepath.FromSlash(key))
	if !strings.HasPrefix(p, filepath.Clean(f.Root)+string(os.PathSeparator)) || strings.Contains(key, "..") {
		return "", fmt.Errorf("blob: invalid key %q", key)
	}
	return p, nil
}

func (f FS) Put(_ context.Context, bucket, key string, r io.Reader, _ int64, _ map[string]string) (Ref, error) {
	p, err := f.path(bucket, key)
	if err != nil {
		return Ref{}, err
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o750); err != nil {
		return Ref{}, err
	}
	tmp := p + ".tmp"
	out, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o640)
	if err != nil {
		return Ref{}, err
	}
	n, err := io.Copy(out, r)
	if cerr := out.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		_ = os.Remove(tmp)
		return Ref{}, err
	}
	if err := os.Rename(tmp, p); err != nil {
		return Ref{}, err
	}
	return Ref{Bucket: bucket, Key: key, Size: n}, nil
}

func (f FS) Get(_ context.Context, ref Ref) (io.ReadCloser, error) {
	p, err := f.path(ref.Bucket, ref.Key)
	if err != nil {
		return nil, err
	}
	fh, err := os.Open(p)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, ErrNotFound
	}
	return fh, err
}

func (f FS) Delete(_ context.Context, ref Ref) error {
	p, err := f.path(ref.Bucket, ref.Key)
	if err != nil {
		return err
	}
	err = os.Remove(p)
	if errors.Is(err, fs.ErrNotExist) {
		return ErrNotFound
	}
	return err
}

func (f FS) PresignGet(_ context.Context, ref Ref, _ time.Duration) (string, error) {
	return "", fmt.Errorf("blob: the filesystem store cannot presign URLs; configure SOAR_BLOB_ENDPOINT")
}
