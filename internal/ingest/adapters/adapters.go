// Package adapters holds the transports that fetch data (spec 9.3). Push transports
// (webhook) live in the ingest receiver; this package has the pull side, registered by name.
package adapters

import (
	"context"
	"fmt"
	"sort"
	"time"

	"github.com/yourorg/soar/internal/domain"
)

// Query asks a pull source for one page of records, ordered by the cursor field.
type Query struct {
	From time.Time // records with a cursor-field value at or after From
	To   time.Time // and at or before To; zero means no upper bound
	// After is the sort key of the last record already read (keyset paging). Records up
	// to and including it are skipped. Nil starts at From.
	After  []any
	Newest bool      // newest first (the late-arrival sweep); otherwise oldest first
	Now    time.Time // the collector's clock, for sources that derive index names from dates
}

// Page is one fetch from a pull source.
type Page struct {
	Items []map[string]any
	// IDs holds a stable identity per item ("" when the record has none). The collector
	// uses it to skip records it published moments ago when it re-reads the overlap.
	IDs  []string
	Last time.Time // newest cursor-field value in Items
	// LastKey is the sort key of the final item, passed back as Query.After to read the
	// next page. Nil when the source has no keyset paging; the collector then pages by time.
	LastKey []any
	Full    bool // the source returned a full page, so more may be waiting
}

// Poller fetches one page of records.
type Poller interface {
	Page(ctx context.Context, q Query) (Page, error)
}

// Factory builds a poller for a source and the connection it refers to.
type Factory func(src domain.Source, conn domain.Connection, resolve func(ref string) (string, error)) (Poller, error)

var registry = map[string]Factory{}

// Register adds a pull adapter under a name. It panics on duplicates.
func Register(name string, f Factory) {
	if _, dup := registry[name]; dup {
		panic("adapters: duplicate adapter " + name)
	}
	registry[name] = f
}

// Get returns the factory registered under name.
func Get(name string) (Factory, bool) { f, ok := registry[name]; return f, ok }

// Names lists registered pull adapters.
func Names() []string {
	out := make([]string, 0, len(registry))
	for n := range registry {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// ParseSchedule reads "every 15s".
func ParseSchedule(s string) (time.Duration, error) {
	var d string
	if _, err := fmt.Sscanf(s, "every %s", &d); err != nil {
		return 0, fmt.Errorf("schedule %q: expected \"every <duration>\", for example \"every 15s\"", s)
	}
	dur, err := time.ParseDuration(d)
	if err != nil || dur < time.Second {
		return 0, fmt.Errorf("schedule %q: the interval must be a duration of at least 1s", s)
	}
	return dur, nil
}
