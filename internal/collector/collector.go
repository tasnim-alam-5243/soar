// Package collector runs the pull adapters: it polls sources on a schedule, publishes
// what it finds to NATS and advances a persisted cursor only after JetStream
// acknowledged every message (spec 9.9). One lease per source keeps replicas from
// polling the same source at once.
//
// Each cycle has two phases:
//
//  1. New data: records after the stored position, oldest first, with keyset paging
//     (search_after) where the source supports it. The position only moves forward.
//     When the per-cycle page cap is reached the next cycle starts at once, so a busy
//     source catches up instead of waiting a full interval.
//  2. Late arrivals (once caught up): the overlap window before the position is re-read
//     newest first, within its own page budget, so records indexed late are not missed
//     (spec 10.1: 60 s overlap). Records this collector published moments ago are
//     skipped locally; anything else repeated is dropped by the processor's
//     exact-duplicate guard.
package collector

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/yourorg/soar/internal/domain"
	"github.com/yourorg/soar/internal/ingest"
	"github.com/yourorg/soar/internal/ingest/adapters"
	"github.com/yourorg/soar/internal/queue"
	"github.com/yourorg/soar/internal/security"
	"github.com/yourorg/soar/internal/store"
)

// Collector manages one polling goroutine per enabled pull source.
type Collector struct {
	Reg     *ingest.Registry
	Q       ingest.Queue
	St      *store.Store
	Log     *slog.Logger
	Secrets func(ref, use string, consumers ...string) (string, error) // security.SecretStore.Resolver
	Owner   string                                                     // identifies this replica in leases
	Now     func() time.Time
	Dirs    []string // local directories file_object sources may read (SOAR_INGEST_DIRS)
	CertDir string   // certificate files sources and connections may name (SOAR_STREAM_CERTS_DIR)

	mu      sync.Mutex
	running map[string]*worker
	states  map[string]*sourceState
}

type worker struct {
	cancel context.CancelFunc
	sig    string
	done   chan struct{}
}

// sourceState is what a replica keeps between cycles of one source.
type sourceState struct {
	poller  adapters.Poller
	connSig string // rebuild the poller when the connection changes
	recent  recentSet
	// sweepBase is the position where the current catch-up began. The late-arrival
	// sweep re-reads the overlap before it, so a burst that takes several cycles to
	// read cannot push the sweep window past records that arrived late meanwhile.
	sweepBase time.Time
}

func (c *Collector) now() time.Time {
	if c.Now != nil {
		return c.Now()
	}
	return time.Now().UTC()
}

// Run starts and stops source workers as the configuration changes, until ctx ends.
func (c *Collector) Run(ctx context.Context) error {
	c.mu.Lock()
	c.running = map[string]*worker{}
	c.mu.Unlock()
	tick := time.NewTicker(2 * time.Second)
	defer tick.Stop()
	for {
		c.reconcile(ctx)
		select {
		case <-ctx.Done():
			c.mu.Lock()
			for _, w := range c.running {
				w.cancel()
			}
			c.mu.Unlock()
			for _, w := range c.running {
				<-w.done
			}
			return nil
		case <-tick.C:
		}
	}
}

func (c *Collector) reconcile(ctx context.Context) {
	want := map[string]*ingest.Runtime{}
	for _, rt := range c.Reg.All() {
		if adapters.Known(rt.Source.Adapter) && rt.Source.Enabled && rt.Pipeline != nil && rt.Err == nil {
			want[rt.Source.Name] = rt
		}
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	for name, w := range c.running {
		rt, keep := want[name]
		if !keep || w.sig != rt.Signature() {
			w.cancel()
			<-w.done
			delete(c.running, name)
			delete(c.states, name) // a changed source starts with a fresh poller
		}
	}
	for name, rt := range want {
		if _, ok := c.running[name]; ok {
			continue
		}
		wctx, cancel := context.WithCancel(ctx)
		w := &worker{cancel: cancel, sig: rt.Signature(), done: make(chan struct{})}
		c.running[name] = w
		go func(rt *ingest.Runtime) {
			defer close(w.done)
			c.runSource(wctx, rt)
		}(rt)
	}
}

func (c *Collector) connection(ctx context.Context, rt *ingest.Runtime) (domain.Connection, error) {
	name, _ := rt.Source.Config["connection"].(string)
	if name == "" {
		return domain.Connection{}, fmt.Errorf("config.connection is required")
	}
	p, err := c.St.Connections.List(ctx, store.ListQuery{Terms: map[string][]string{"name": {name}}, Limit: 1})
	if err != nil {
		return domain.Connection{}, err
	}
	if len(p.Items) == 0 {
		return domain.Connection{}, fmt.Errorf("connection %q does not exist", name)
	}
	return p.Items[0], nil
}

// state returns the per-source state, building the poller on first use and whenever
// the connection document changed (rotated password, new CA).
func (c *Collector) state(ctx context.Context, rt *ingest.Runtime) (*sourceState, error) {
	conn, err := c.connection(ctx, rt)
	if err != nil {
		return nil, err
	}
	connSig := conn.ID + "|" + conn.UpdatedAt.Format(time.RFC3339Nano)
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.states == nil {
		c.states = map[string]*sourceState{}
	}
	s := c.states[rt.Source.Name]
	if s != nil && s.poller != nil && s.connSig == connSig {
		return s, nil
	}
	factory, _ := adapters.Get(rt.Source.Adapter)
	// the poller gets secrets for this connection only: env references must fit a
	// connection and scoped secrets must name its connector, name or host
	poller, err := factory(rt.Source, conn, func(ref string) (string, error) {
		return c.Secrets(ref, security.UseConnection, security.ConnectionConsumers(conn)...)
	})
	if err != nil {
		return nil, err
	}
	if s == nil {
		s = &sourceState{}
		c.states[rt.Source.Name] = s
	}
	s.poller, s.connSig = poller, connSig
	return s, nil
}

func (c *Collector) runSource(ctx context.Context, rt *ingest.Runtime) {
	if spec, ok := adapters.GetStream(rt.Source.Adapter); ok {
		c.runStream(ctx, rt, spec)
		return
	}
	log := c.Log.With("source", rt.Source.Name)
	interval, err := adapters.ParseSchedule(rt.Source.Schedule)
	if err != nil {
		log.Error("source not polled", "error", err.Error())
		return
	}
	fail := 0
	for ctx.Err() == nil {
		wait := interval
		behind, err := c.cycle(ctx, rt, interval)
		switch {
		case err != nil:
			fail++
			wait = min(interval*time.Duration(1<<min(fail, 6)), 5*time.Minute) // back off on source errors
			if !errors.Is(err, errNoLease) {
				log.Warn("poll failed; backing off", "error", err.Error(), "retry_in", wait.String())
			}
		case behind:
			fail = 0
			wait = min(interval, 250*time.Millisecond) // more is waiting: poll again at once
		default:
			fail = 0
		}
		select {
		case <-ctx.Done():
		case <-time.After(wait):
		}
	}
}

var errNoLease = errors.New("another collector holds the lease")

func cursorID(name string) string { return domain.DeterministicID("cur", name) }

// position is the stored cursor: the newest cursor-field value read and, for keyset
// sources, the sort key of that record. Older cursors were a bare RFC 3339 time.
type position struct {
	TS    time.Time `json:"ts"`
	After []any     `json:"after,omitempty"`
}

func parsePosition(s string) (position, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return position{}, nil
	}
	if strings.HasPrefix(s, "{") {
		var p position
		if err := json.Unmarshal([]byte(s), &p); err != nil {
			return position{}, fmt.Errorf("stored cursor %q: %w", s, err)
		}
		return p, nil
	}
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		return position{}, fmt.Errorf("stored cursor %q is not a timestamp: %w", s, err)
	}
	return position{TS: t}, nil
}

func (p position) String() string {
	b, _ := json.Marshal(position{TS: p.TS.UTC(), After: p.After})
	return string(b)
}

// acquire takes or renews the lease for a source and returns the cursor document.
func (c *Collector) acquire(ctx context.Context, name string, ttl time.Duration) (domain.Cursor, error) {
	id := cursorID(name)
	now := c.now()
	exp := now.Add(ttl)
	cur, v, err := c.St.Cursors.Get(ctx, id)
	if errors.Is(err, store.ErrNotFound) {
		doc := domain.Cursor{Meta: domain.Meta{ID: id}, SourceID: name, LeaseOwner: c.Owner, LeaseExpiresAt: &exp}
		if _, err := c.St.Cursors.Create(ctx, doc); err != nil {
			if errors.Is(err, store.ErrAlreadyExists) {
				return domain.Cursor{}, errNoLease
			}
			return domain.Cursor{}, err
		}
		return doc, nil
	}
	if err != nil {
		return domain.Cursor{}, err
	}
	held := cur.LeaseOwner != "" && cur.LeaseOwner != c.Owner && cur.LeaseExpiresAt != nil && cur.LeaseExpiresAt.After(now)
	if held {
		return domain.Cursor{}, errNoLease
	}
	cur.LeaseOwner, cur.LeaseExpiresAt = c.Owner, &exp
	if _, err := c.St.Cursors.Save(ctx, cur, v); err != nil {
		if errors.Is(err, store.ErrVersionConflict) {
			return domain.Cursor{}, errNoLease // someone else took it first
		}
		return domain.Cursor{}, err
	}
	return cur, nil
}

// advance stores the new cursor if this replica still holds the lease.
func (c *Collector) advance(ctx context.Context, name, value string, ttl time.Duration) error {
	exp := c.now().Add(ttl)
	_, _, err := c.St.Cursors.Update(ctx, cursorID(name), func(d *domain.Cursor) error {
		if d.LeaseOwner != c.Owner {
			return errNoLease
		}
		d.Cursor, d.LeaseExpiresAt = value, &exp
		return nil
	})
	return err
}

func durationOf(rt *ingest.Runtime, key string, def time.Duration) time.Duration {
	cfg, _ := rt.Source.Config["cursor"].(map[string]any)
	if s, _ := cfg[key].(string); s != "" {
		if d, err := time.ParseDuration(s); err == nil && d >= 0 {
			return d
		}
	}
	return def
}

// initialLookback reads cursor.initial ("1h"): how far back the first poll reaches.
func initialLookback(rt *ingest.Runtime) time.Duration { return durationOf(rt, "initial", time.Hour) }

// overlap reads cursor.overlap ("60s"): the window re-read for late arrivals. 0 disables it.
func overlap(rt *ingest.Runtime) time.Duration { return durationOf(rt, "overlap", 60*time.Second) }

func intConfig(rt *ingest.Runtime, key string, def int) int {
	switch n := rt.Source.Config[key].(type) {
	case float64:
		if n > 0 {
			return int(n)
		}
	case int:
		if n > 0 {
			return n
		}
	}
	return def
}

// maxPages caps new-data pages per cycle so one cycle stays short; a capped cycle is
// followed immediately by the next one.
func maxPages(rt *ingest.Runtime) int { return intConfig(rt, "max_pages_per_cycle", 20) }

// overlapPages caps the late-arrival sweep. Default 40 pages of 500 = the whole 60 s
// overlap up to ~330 records/s. Above that the overlap holds more than the budget and
// its newest part is re-read (late records are usually only seconds late); reading it
// all at peak would mean re-reading 60,000 records every cycle. Records re-read this
// way are skipped before mapping, so the cost is the indexer query, not the pipeline.
func overlapPages(rt *ingest.Runtime) int { return intConfig(rt, "overlap_pages", 40) }

// cycle polls once. behind is true when it stopped at the page cap with more waiting.
func (c *Collector) cycle(ctx context.Context, rt *ingest.Runtime, interval time.Duration) (behind bool, err error) {
	name := rt.Source.Name
	ttl := max(3*interval, 30*time.Second)
	cur, err := c.acquire(ctx, name, ttl)
	if err != nil {
		return false, err
	}
	st, err := c.state(ctx, rt)
	if err != nil {
		return false, err
	}
	pos, err := parsePosition(cur.Cursor)
	if err != nil {
		return false, err
	}
	now := c.now()
	fresh := pos.TS.IsZero() // first poll ever: nothing was read before, so nothing can be late
	if fresh {
		pos = position{TS: now.Add(-initialLookback(rt))}
	}
	if st.sweepBase.IsZero() && !fresh {
		st.sweepBase = pos.TS // a new catch-up starts here (in steady state: this cycle's start)
	}

	// Phase 1: new data, oldest first.
	from, after := pos.TS, pos.After
	caughtUp := false
	for page := 0; page < maxPages(rt); page++ {
		pg, err := st.poller.Page(ctx, adapters.Query{From: from, After: after, Now: now})
		if err != nil {
			return false, err
		}
		if err := c.publish(ctx, rt, st, pg, now); err != nil {
			return false, err // the cursor stays put; the next cycle retries this window
		}
		next := pos
		if pg.Last.After(next.TS) {
			next.TS = pg.Last
		}
		if pg.LastKey != nil {
			next.After = pg.LastKey
		} else if !next.TS.Equal(pos.TS) {
			next.After = nil
		}
		if next.String() != pos.String() {
			if err := c.advance(ctx, name, next.String(), ttl); err != nil {
				return false, err
			}
		}
		if !pg.Full {
			caughtUp = true
			pos = next
			break
		}
		if pg.LastKey != nil {
			from, after = next.TS, pg.LastKey
		} else {
			if !pg.Last.After(from) {
				c.Log.Warn("a full page did not advance the cursor; stopping this cycle", "source", name)
				pos = next
				caughtUp = true
				break
			}
			from, after = pg.Last, nil
		}
		pos = next
	}

	// Phase 2: late arrivals. Re-read the overlap before the position where this catch-up
	// began (spec 10.1: 60 s), newest first. It runs only once caught up, so a backlog is
	// never starved by re-reads. Records that arrive late inside what phase 1 just read
	// are covered by the next cycle's sweep.
	if caughtUp {
		if ov := overlap(rt); ov > 0 && !st.sweepBase.IsZero() {
			base := st.sweepBase
			after = nil
			for page := 0; page < overlapPages(rt); page++ {
				pg, err := st.poller.Page(ctx, adapters.Query{From: base.Add(-ov), To: base, After: after, Newest: true, Now: now})
				if err != nil {
					return false, err // sweepBase is kept: the next cycle sweeps the same window
				}
				if err := c.publish(ctx, rt, st, pg, now); err != nil {
					return false, err
				}
				if !pg.Full || pg.LastKey == nil {
					break
				}
				after = pg.LastKey
			}
		}
		st.sweepBase = time.Time{}
	}
	st.recent.prune(now.Add(-(overlap(rt) + 3*interval + time.Minute)))
	return !caughtUp, nil
}

// publish maps and queues one page. Records this replica already published inside the
// overlap are skipped before mapping, so re-reads cost neither queue space nor counters.
func (c *Collector) publish(ctx context.Context, rt *ingest.Runtime, st *sourceState, pg adapters.Page, now time.Time) error {
	items := pg.Items
	if len(pg.IDs) == len(pg.Items) {
		items = items[:0:0]
		for i, it := range pg.Items {
			if !st.recent.has(pg.IDs[i]) {
				items = append(items, it)
			}
		}
	}
	if len(items) == 0 {
		return nil
	}
	out := rt.Pipeline.ProcessRecords(items)
	msgs := make([]queue.Msg, 0, len(out.Events))
	for _, ev := range out.Events {
		m, err := ingest.EventMessage(ev, rt.Pipeline.System, "")
		if err != nil {
			out.Failures = append(out.Failures, ingest.Failure{Record: ev.Alert.Ext, Stage: "publish", Err: err.Error()})
			continue
		}
		msgs = append(msgs, m)
	}
	for _, perr := range c.Q.PublishMany(ctx, msgs) {
		if perr != nil {
			return fmt.Errorf("publish: %w", perr)
		}
	}
	ingest.SendFailures(ctx, c.Q, c.St, c.Log, rt.Source.Name, rt.Source.Adapter, out.Failures)
	for _, id := range pg.IDs {
		st.recent.add(id, now)
	}
	return nil
}

// recentSet remembers record identities published recently (wall-clock time of publish).
type recentSet struct {
	m map[string]time.Time
}

const recentMax = 1_000_000 // ~1,000/s over the overlap and a few intervals, with room to spare

func (r *recentSet) has(id string) bool {
	if id == "" || r.m == nil {
		return false
	}
	_, ok := r.m[id]
	return ok
}

func (r *recentSet) add(id string, at time.Time) {
	if id == "" {
		return
	}
	if r.m == nil {
		r.m = map[string]time.Time{}
	}
	if len(r.m) >= recentMax {
		r.m = map[string]time.Time{} // never grow without bound; the duplicate guard still holds
	}
	r.m[id] = at
}

func (r *recentSet) prune(before time.Time) {
	for id, at := range r.m {
		if at.Before(before) {
			delete(r.m, id)
		}
	}
}
