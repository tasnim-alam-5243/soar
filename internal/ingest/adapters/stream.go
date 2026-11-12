package adapters

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/yourorg/soar/internal/domain"
)

// Streams (Step 10) are transports that run continuously instead of being polled on a
// schedule: syslog listeners, Kafka consumers, file/object watchers. The collector
// parses what they hand over with the source's parser and runs it through the same
// mapping and filter pipeline as every other source.

// Message is one raw message from a stream, before parsing.
type Message struct {
	Body []byte
	// Meta are transport facts (remote address, Kafka topic/partition/offset, object
	// key) added to every record parsed from Body under "_transport".
	Meta map[string]any
	// ID is a stable identity when the transport has one (Kafka offset, object key):
	// re-sending the same batch after a failure is then deduplicated by the queue.
	ID string
}

// Emit hands a batch to the pipeline and returns once it is queued (nil) or cannot be
// (error). On an error a stream keeps the batch and tries again later (Kafka does not
// commit its offsets, a TCP syslog connection stops being read, a file stays unread),
// so nothing is dropped silently.
type Emit func(ctx context.Context, batch []Message) error

// Stream is a long-running transport. Run returns when ctx ends or the stream cannot
// continue (the collector restarts it with a back-off).
type Stream interface {
	Run(ctx context.Context, emit Emit) error
}

// CursorStore keeps the position of an exclusive stream (the source's cursor document).
type CursorStore interface {
	Load(ctx context.Context) (string, error)
	Save(ctx context.Context, value string) error
}

// StreamDeps is what a stream gets besides its source.
type StreamDeps struct {
	Conn    *domain.Connection               // nil when the source names no connection
	Resolve func(ref string) (string, error) // the connection's secrets
	Cursor  CursorStore                      // exclusive streams only
	Log     *slog.Logger
	Now     func() time.Time
	// LocalDirs are the directories (and their subdirectories) a stream may read from
	// the collector's own disk (SOAR_INGEST_DIRS); empty allows none.
	LocalDirs []string
	// CertDir holds the certificate and key files sources and connections may name
	// (SOAR_STREAM_CERTS_DIR). Empty when the API only validates: nothing is read then.
	CertDir string
}

// readCertFile reads a certificate or key file named by a source or connection: a name
// inside CertDir, or an absolute path inside it. The read goes through os.Root, so a link
// cannot lead out of the directory, and the error does not tell a missing file from an
// unreadable one.
func readCertFile(dir, name string) ([]byte, error) {
	if dir == "" {
		return nil, errors.New("no certificate directory is configured (SOAR_STREAM_CERTS_DIR)")
	}
	rel := name
	if filepath.IsAbs(name) {
		r, err := filepath.Rel(filepath.Clean(dir), filepath.Clean(name))
		if err != nil || r == ".." || strings.HasPrefix(r, ".."+string(filepath.Separator)) {
			return nil, fmt.Errorf("%s must be a file in %s (SOAR_STREAM_CERTS_DIR)", name, dir)
		}
		rel = r
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		return nil, fmt.Errorf("the certificate directory %s cannot be opened", dir)
	}
	defer root.Close()
	b, err := root.ReadFile(rel)
	if err != nil {
		return nil, fmt.Errorf("%s is not a readable file in %s (SOAR_STREAM_CERTS_DIR)", name, dir)
	}
	return b, nil
}

// StreamSpec registers a stream transport.
type StreamSpec struct {
	// New checks the source and builds the stream without starting it. deps.Conn is nil
	// when the API only validates a source: connection settings are checked in Run.
	New func(src domain.Source, deps StreamDeps) (Stream, error)
	// Exclusive streams run on one collector replica at a time (the one holding the
	// source's lease), for transports without their own coordination (a bucket, a
	// directory). Syslog listeners run on every replica; Kafka consumer groups share
	// partitions by themselves.
	Exclusive bool
	// NeedsConnection: the source must name a connection (config.connection).
	NeedsConnection bool
}

var streams = map[string]StreamSpec{}

// RegisterStream adds a stream transport under a name. It panics on duplicates.
func RegisterStream(name string, s StreamSpec) {
	if _, dup := streams[name]; dup {
		panic("adapters: duplicate stream " + name)
	}
	if _, dup := registry[name]; dup {
		panic("adapters: " + name + " is already a pull adapter")
	}
	streams[name] = s
}

// GetStream returns the stream transport registered under name.
func GetStream(name string) (StreamSpec, bool) { s, ok := streams[name]; return s, ok }

// StreamNames lists registered stream transports.
func StreamNames() []string {
	out := make([]string, 0, len(streams))
	for n := range streams {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// Known reports whether name is a pull adapter or a stream.
func Known(name string) bool {
	_, pull := registry[name]
	_, stream := streams[name]
	return pull || stream
}
