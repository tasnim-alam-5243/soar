package collector

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/yourorg/soar/integrations"
	"github.com/yourorg/soar/internal/domain"
	"github.com/yourorg/soar/internal/ingest"
	"github.com/yourorg/soar/internal/ingest/pack"
	"github.com/yourorg/soar/internal/queue"
	"github.com/yourorg/soar/internal/queue/queuetest"
	"github.com/yourorg/soar/internal/security"
	"github.com/yourorg/soar/internal/store"
	"github.com/yourorg/soar/internal/store/memory"
)

// indexer imitates the Wazuh indexer's _search endpoint for the poller's query shape:
// range gte/lte on @timestamp, sort on @timestamp (and id), search_after, size.
type indexer struct {
	mu           sync.Mutex
	docs         []map[string]any // ascending by (@timestamp, id)
	requests     []string         // paths
	bodies       []string
	user         string
	fail         bool
	rejectIDSort bool // answer 400 when asked to sort on id, like a cluster where id is not sortable
	rejected     int
}

func tsOf(d map[string]any) time.Time {
	t, _ := time.Parse(time.RFC3339Nano, d["@timestamp"].(string))
	return t
}

func (x *indexer) addAt(ts time.Time, id string) {
	x.appendDoc(ts, id)
	x.sortDocs()
}

func (x *indexer) appendDoc(ts time.Time, id string) {
	x.docs = append(x.docs, map[string]any{
		"@timestamp": ts.UTC().Format("2006-01-02T15:04:05.000Z"), "timestamp": ts.Format("2006-01-02T15:04:05.000-0700"),
		"id": id, "rule": map[string]any{"level": 5, "id": "554", "description": "File added to the system."},
		"agent": map[string]any{"id": "001", "name": "web01"}, "syscheck": map[string]any{"path": "/tmp/" + id},
	})
}

func (x *indexer) sortDocs() {
	sort.SliceStable(x.docs, func(i, j int) bool {
		ti, tj := tsOf(x.docs[i]), tsOf(x.docs[j])
		if !ti.Equal(tj) {
			return ti.Before(tj)
		}
		return x.docs[i]["id"].(string) < x.docs[j]["id"].(string)
	})
}

func (x *indexer) add(n int, from time.Time, step time.Duration, prefix string) {
	x.mu.Lock()
	defer x.mu.Unlock()
	for i := 0; i < n; i++ {
		x.appendDoc(from.Add(time.Duration(i)*step), fmt.Sprintf("%s.%06d", prefix, i))
	}
	x.sortDocs()
}

// cmpKey compares sort keys [millis, id] (or [millis]).
func cmpKey(a, b []any) int {
	am, bm := a[0].(float64), b[0].(float64)
	switch {
	case am < bm:
		return -1
	case am > bm:
		return 1
	}
	if len(a) > 1 && len(b) > 1 {
		return strings.Compare(a[1].(string), b[1].(string))
	}
	return 0
}

func (x *indexer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	x.mu.Lock()
	defer x.mu.Unlock()
	x.requests = append(x.requests, r.URL.Path)
	if u, _, ok := r.BasicAuth(); !ok || u != x.user {
		w.WriteHeader(401)
		return
	}
	if x.fail {
		w.WriteHeader(500)
		_, _ = w.Write([]byte(`{"error":"shard failure"}`))
		return
	}
	raw, _ := io.ReadAll(r.Body)
	x.bodies = append(x.bodies, string(raw))
	var body struct {
		Size        int                 `json:"size"`
		Sort        []map[string]string `json:"sort"`
		SearchAfter []any               `json:"search_after"`
		Query       struct {
			Bool struct {
				Filter []struct {
					Range map[string]map[string]string `json:"range"`
				} `json:"filter"`
			} `json:"bool"`
		} `json:"query"`
	}
	if err := json.Unmarshal(raw, &body); err != nil {
		w.WriteHeader(400)
		return
	}
	if x.rejectIDSort && len(body.Sort) > 1 {
		x.rejected++
		w.WriteHeader(400)
		_, _ = w.Write([]byte(`{"error":{"type":"search_phase_execution_exception","reason":"No mapping found for [id] in order to sort on"}}`))
		return
	}
	rg := body.Query.Bool.Filter[0].Range["@timestamp"]
	gte, _ := time.Parse(time.RFC3339Nano, rg["gte"])
	var lte time.Time
	if s := rg["lte"]; s != "" {
		lte, _ = time.Parse(time.RFC3339Nano, s)
	}
	desc := len(body.Sort) > 0 && body.Sort[0]["@timestamp"] == "desc"
	docs := append([]map[string]any(nil), x.docs...)
	if desc {
		for i, j := 0, len(docs)-1; i < j; i, j = i+1, j-1 {
			docs[i], docs[j] = docs[j], docs[i]
		}
	}
	hits := []map[string]any{}
	for _, d := range docs {
		ts := tsOf(d)
		if ts.Before(gte) || (!lte.IsZero() && ts.After(lte)) {
			continue
		}
		key := []any{float64(ts.UnixMilli())}
		if len(body.Sort) > 1 {
			key = append(key, d["id"])
		}
		if body.SearchAfter != nil {
			c := cmpKey(key, body.SearchAfter)
			if (!desc && c <= 0) || (desc && c >= 0) {
				continue
			}
		}
		if len(hits) == body.Size {
			break
		}
		hits = append(hits, map[string]any{"_index": "wazuh-alerts-4.x-x", "_id": d["id"], "_source": d, "sort": key})
	}
	_ = json.NewEncoder(w).Encode(map[string]any{"hits": map[string]any{"hits": hits}})
}

// countingQ counts published messages, so tests can tell "not republished" from
// "republished and then dropped by the JetStream duplicate window".
type countingQ struct {
	ingest.Queue
	published atomic.Int64
}

func (c *countingQ) PublishMany(ctx context.Context, ms []queue.Msg) []error {
	c.published.Add(int64(len(ms)))
	return c.Queue.PublishMany(ctx, ms)
}

type rig struct {
	t   *testing.T
	idx *indexer
	st  *store.Store
	q   *queue.Client
	cq  *countingQ
	reg *ingest.Registry
	col *Collector
	srv *httptest.Server
}

func newRig(t *testing.T) *rig {
	t.Helper()
	t.Setenv("SOAR_SECRET_T_USER", "reader")
	t.Setenv("SOAR_SECRET_T_PASS", "pw")
	idx := &indexer{user: "reader"}
	srv := httptest.NewServer(idx)
	t.Cleanup(srv.Close)

	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	st := memory.NewStore("soar")
	b, err := pack.Load(integrations.Packs, "wazuh")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pack.Install(context.Background(), st, b); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if _, err := st.Connections.Create(ctx, domain.Connection{
		Meta: domain.NewMeta("con"), ConnectorID: "wazuh-indexer", Name: "wazuh_indexer", AuthType: "basic",
		SecretRefs: domain.JSON{"username": "env:SOAR_SECRET_T_USER", "password": "env:SOAR_SECRET_T_PASS"}, Config: domain.JSON{"base_url": srv.URL},
	}); err != nil {
		t.Fatal(err)
	}
	// The pack's pull source has no auth block: pull sources authenticate outbound
	// through their connection, and must load without one.
	if _, _, err := st.Sources.Update(ctx, domain.DeterministicID(domain.PrefixSource, "wazuh-pull"), func(s *domain.Source) error {
		s.Enabled = true
		s.Schedule = "every 1s"
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	q, err := queue.Connect([]string{queuetest.Start(t)}, "test")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(q.Close)
	if err := q.EnsureStreams(ctx, queue.StreamOptions{EventsMaxBytes: 256 << 20}); err != nil {
		t.Fatal(err)
	}
	reg := ingest.NewRegistry(st, log, security.ResolveSecret)
	reg.Serves = ingest.IsPull
	if err := reg.Refresh(ctx); err != nil {
		t.Fatal(err)
	}
	cq := &countingQ{Queue: q}
	return &rig{
		t: t, idx: idx, st: st, q: q, cq: cq, reg: reg, srv: srv,
		col: &Collector{Reg: reg, Q: cq, St: st, Log: log, Secrets: security.ResolveSecret, Owner: "collector-a"},
	}
}

func (r *rig) queued() uint64 {
	st, _ := r.q.JS().Stream(context.Background(), queue.EventsStream)
	info, _ := st.Info(context.Background())
	return info.State.Msgs
}

func (r *rig) runtime() *ingest.Runtime {
	rt, ok := r.reg.Get("wazuh-pull")
	if !ok {
		r.t.Fatal("wazuh-pull is not loaded")
	}
	return rt
}

func (r *rig) cycle() (bool, error) {
	return r.col.cycle(context.Background(), r.runtime(), time.Second)
}

func (r *rig) cursor() string {
	c, _, err := r.st.Cursors.Get(context.Background(), cursorID("wazuh-pull"))
	if err != nil {
		return ""
	}
	return c.Cursor
}

func (r *rig) position() position {
	p, err := parsePosition(r.cursor())
	if err != nil {
		r.t.Fatal(err)
	}
	return p
}

func (r *rig) resetRequests() {
	r.idx.mu.Lock()
	r.idx.requests, r.idx.bodies = nil, nil
	r.idx.mu.Unlock()
}

func (r *rig) setConfig(key string, v any) {
	r.t.Helper()
	ctx := context.Background()
	if _, _, err := r.st.Sources.Update(ctx, domain.DeterministicID(domain.PrefixSource, "wazuh-pull"), func(s *domain.Source) error {
		s.Config[key] = v
		return nil
	}); err != nil {
		r.t.Fatal(err)
	}
	if err := r.reg.Refresh(ctx); err != nil {
		r.t.Fatal(err)
	}
}

func TestEachRoleLoadsOnlyItsOwnSources(t *testing.T) {
	r := newRig(t)
	// collector registry: the pull source loads without an auth block, and the push
	// source (whose token variable is not set here) is not even compiled
	if _, ok := r.reg.Get("wazuh-pull"); !ok {
		t.Fatal("the pull source must load in the collector")
	}
	for _, rt := range r.reg.All() {
		if rt.Source.Name == "wazuh-prod" {
			t.Fatal("the collector must not load push sources (it would need their inbound secrets)")
		}
		if rt.Err != nil {
			t.Fatalf("source %s failed to load: %v", rt.Source.Name, rt.Err)
		}
	}
	// ingest registry: the push source loads with its token; the pull source is skipped
	t.Setenv("SOAR_SOURCE_TOKEN_WAZUH_PROD", "tok")
	in := ingest.NewRegistry(r.st, slog.New(slog.NewTextHandler(io.Discard, nil)), security.ResolveSecret)
	in.Serves = ingest.IsPush
	if err := in.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	if rt, ok := in.Get("wazuh-prod"); !ok || rt.Auth == nil {
		t.Fatal("the push source must load in ingest, with its authenticator")
	}
	if len(in.All()) != 1 {
		t.Fatalf("ingest must load only push sources, got %d", len(in.All()))
	}
}

func TestCycleCatchesUpOverSeveralPagesAndStoresTheCursor(t *testing.T) {
	r := newRig(t)
	start := time.Now().Add(-30 * time.Minute).Truncate(time.Millisecond)
	r.idx.add(1200, start, time.Second, "w") // 1200 alerts: three pages of 500
	behind, err := r.cycle()
	if err != nil || behind {
		t.Fatalf("cycle: behind=%v err=%v", behind, err)
	}
	if r.queued() != 1200 || r.cq.published.Load() != 1200 {
		t.Fatalf("every alert must be published and queued exactly once: published=%d queued=%d", r.cq.published.Load(), r.queued())
	}
	pos := r.position()
	if want := start.Add(1199 * time.Second); !pos.TS.Equal(want.UTC()) {
		t.Errorf("cursor must be the newest @timestamp seen: got %s want %s", pos.TS, want.UTC())
	}
	if len(pos.After) != 2 || pos.After[1] != "w.001199" {
		t.Errorf("cursor must hold the sort key of the newest record, got %v", pos.After)
	}
	// three new-data pages; the very first poll has read nothing before, so nothing to sweep
	if len(r.idx.requests) != 3 {
		t.Errorf("1200 alerts at 500 per page need 3 requests, got %d", len(r.idx.requests))
	}
	if !strings.Contains(r.idx.bodies[1], `"search_after":[`) || strings.Contains(r.idx.bodies[0], "search_after") {
		t.Errorf("pages after the first must use search_after: %s", r.idx.bodies[1])
	}
	for _, p := range r.idx.requests {
		if strings.Contains(p, "wazuh-alerts-*") || !strings.Contains(p, "wazuh-alerts-4.x-") {
			t.Errorf("the poller must name daily indices, never a wildcard: %s", p)
		}
	}
}

func TestOverlapIsReReadButNotPublishedTwice(t *testing.T) {
	r := newRig(t)
	r.idx.add(50, time.Now().Add(-10*time.Minute), time.Second, "o")
	if _, err := r.cycle(); err != nil {
		t.Fatal(err)
	}
	first := r.queued()
	// nothing new; the next cycle re-reads the 60 s overlap, which contains these alerts
	r.resetRequests()
	if _, err := r.cycle(); err != nil {
		t.Fatal(err)
	}
	if len(r.idx.requests) != 2 || !strings.Contains(r.idx.bodies[1], `"desc"`) {
		t.Fatalf("expected a new-data poll and a newest-first overlap sweep, got %v", r.idx.bodies)
	}
	if r.queued() != first || r.cq.published.Load() != int64(first) {
		t.Fatalf("re-read alerts must not be published again: published=%d queued %d -> %d", r.cq.published.Load(), first, r.queued())
	}
	// new alerts after the cursor are picked up
	r.idx.add(5, time.Now().Add(-5*time.Minute), time.Second, "n")
	if _, err := r.cycle(); err != nil {
		t.Fatal(err)
	}
	if r.queued() != first+5 {
		t.Fatalf("new alerts must arrive: %d -> %d", first, r.queued())
	}
}

func TestLateArrivalInsideTheOverlapIsPickedUp(t *testing.T) {
	r := newRig(t)
	base := time.Now().Add(-10 * time.Minute)
	r.idx.add(20, base, time.Second, "a")
	if _, err := r.cycle(); err != nil {
		t.Fatal(err)
	}
	before := r.position()
	// indexed late: its @timestamp is 15 s behind the cursor
	r.idx.mu.Lock()
	r.idx.addAt(before.TS.Add(-15*time.Second), "late.1")
	r.idx.mu.Unlock()
	if _, err := r.cycle(); err != nil {
		t.Fatal(err)
	}
	if r.queued() != 21 {
		t.Fatalf("a record indexed late inside the overlap must still arrive, queue has %d", r.queued())
	}
	if after := r.position(); !after.TS.Equal(before.TS) {
		t.Errorf("a late record must not move the cursor: %s -> %s", before.TS, after.TS)
	}
}

// At peak rate the overlap window holds more records than one cycle's page budget.
// The cursor must still move forward every cycle and every record must arrive once.
func TestBusySourceNeverMovesBackwardAndCatchesUp(t *testing.T) {
	r := newRig(t)
	r.setConfig("max_pages_per_cycle", float64(2))
	r.setConfig("overlap_pages", float64(2))
	r.idx.add(4800, time.Now().Add(-2*time.Minute), 10*time.Millisecond, "b") // 100/s for 48 s: all inside one overlap window
	var prev time.Time
	cycles := 0
	for {
		cycles++
		behind, err := r.cycle()
		if err != nil {
			t.Fatal(err)
		}
		pos := r.position()
		if pos.TS.Before(prev) {
			t.Fatalf("cycle %d moved the cursor backward: %s -> %s", cycles, prev, pos.TS)
		}
		prev = pos.TS
		if !behind {
			break
		}
		if cycles > 10 {
			t.Fatalf("the collector did not catch up after %d cycles (stuck re-reading the overlap?), queue has %d", cycles, r.queued())
		}
	}
	if cycles != 5 {
		t.Errorf("4800 records at 2 pages of 500 per cycle need 5 cycles, took %d", cycles)
	}
	if r.queued() != 4800 || r.cq.published.Load() != 4800 {
		t.Fatalf("every record exactly once: published=%d queued=%d", r.cq.published.Load(), r.queued())
	}
}

func TestSecondarySortRejectedFallsBackToTheCursorField(t *testing.T) {
	r := newRig(t)
	r.idx.rejectIDSort = true
	r.idx.add(1200, time.Now().Add(-30*time.Minute), time.Second, "f")
	if _, err := r.cycle(); err != nil {
		t.Fatal(err)
	}
	if r.queued() != 1200 {
		t.Fatalf("all alerts must arrive with the fallback sort, queue has %d", r.queued())
	}
	if r.idx.rejected != 1 {
		t.Errorf("the rejected sort must be tried once and then remembered, rejected=%d", r.idx.rejected)
	}
	r.idx.add(3, time.Now().Add(-time.Minute), time.Second, "g")
	if _, err := r.cycle(); err != nil {
		t.Fatal(err)
	}
	if r.idx.rejected != 1 || r.queued() != 1203 {
		t.Errorf("the fallback must persist across cycles: rejected=%d queued=%d", r.idx.rejected, r.queued())
	}
}

func TestOldTimestampCursorIsStillRead(t *testing.T) {
	p, err := parsePosition("2026-11-04T11:17:29.805Z")
	if err != nil || p.TS.IsZero() || p.After != nil {
		t.Fatalf("a cursor stored by an older version must parse: %+v %v", p, err)
	}
	q, err := parsePosition(position{TS: p.TS, After: []any{1.7e12, "x"}}.String())
	if err != nil || !q.TS.Equal(p.TS) || len(q.After) != 2 {
		t.Fatalf("round trip: %+v %v", q, err)
	}
}

func TestCursorSurvivesARestart(t *testing.T) {
	r := newRig(t)
	r.idx.add(30, time.Now().Add(-20*time.Minute), time.Second, "s")
	if _, err := r.cycle(); err != nil {
		t.Fatal(err)
	}
	c1 := r.cursor()
	restarted := &Collector{Reg: r.reg, Q: r.q, St: r.st, Log: r.col.Log, Secrets: security.ResolveSecret, Owner: "collector-a"}
	r.resetRequests()
	if _, err := restarted.cycle(context.Background(), r.runtime(), time.Second); err != nil {
		t.Fatal(err)
	}
	if r.queued() != 30 || r.cursor() != c1 {
		t.Errorf("a restarted collector continues from the stored cursor: queued=%d cursor %q -> %q", r.queued(), c1, r.cursor())
	}
}

func TestSourceErrorsDoNotAdvanceTheCursor(t *testing.T) {
	r := newRig(t)
	r.idx.add(10, time.Now().Add(-5*time.Minute), time.Second, "e")
	if _, err := r.cycle(); err != nil {
		t.Fatal(err)
	}
	before, queuedBefore := r.cursor(), r.queued()
	r.idx.add(10, time.Now().Add(-2*time.Minute), time.Second, "e2")
	r.idx.fail = true
	if _, err := r.cycle(); err == nil || !strings.Contains(err.Error(), "500") {
		t.Fatalf("the indexer failure must surface, got %v", err)
	}
	if r.cursor() != before || r.queued() != queuedBefore {
		t.Error("a failed poll must change nothing")
	}
	r.idx.fail = false
	if _, err := r.cycle(); err != nil {
		t.Fatal(err)
	}
	if r.queued() != queuedBefore+10 {
		t.Errorf("after recovery the missed alerts arrive: %d", r.queued())
	}
}

func TestOnlyOneCollectorPollsASourceAtATime(t *testing.T) {
	r := newRig(t)
	r.idx.add(5, time.Now().Add(-5*time.Minute), time.Second, "l")
	other := &Collector{Reg: r.reg, Q: r.q, St: r.st, Log: r.col.Log, Secrets: security.ResolveSecret, Owner: "collector-b"}
	if _, err := r.cycle(); err != nil {
		t.Fatal(err)
	}
	if _, err := other.cycle(context.Background(), r.runtime(), time.Second); !errors.Is(err, errNoLease) {
		t.Fatalf("collector-b must not poll while collector-a holds the lease, got %v", err)
	}
	// once the lease expires the other replica takes over
	other.Now = func() time.Time { return time.Now().Add(10 * time.Minute) }
	if _, err := other.cycle(context.Background(), r.runtime(), time.Second); err != nil {
		t.Fatalf("an expired lease must be taken over: %v", err)
	}
	// and the previous owner can no longer advance the cursor
	if err := r.col.advance(context.Background(), "wazuh-pull", "2030-01-01T00:00:00Z", time.Minute); err == nil {
		t.Error("a replica that lost the lease must not move the cursor")
	}
}

func TestRunStartsAndStopsSourceWorkers(t *testing.T) {
	r := newRig(t)
	r.idx.add(20, time.Now().Add(-5*time.Minute), time.Second, "run")
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- r.col.Run(ctx) }()
	deadline := time.Now().Add(15 * time.Second)
	for r.queued() < 20 && time.Now().Before(deadline) {
		time.Sleep(100 * time.Millisecond)
	}
	if r.queued() != 20 {
		t.Fatalf("the running collector must poll on schedule, queue has %d", r.queued())
	}
	cancel()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("Run must return after cancel")
	}
}

// Health reports update the source document every 15 s while data flows. They must not
// look like a configuration change: no pipeline rebuild, no restart of the source worker.
func TestHealthReportsDoNotReloadTheSource(t *testing.T) {
	r := newRig(t)
	r.idx.add(10, time.Now().Add(-5*time.Minute), time.Second, "h")
	before := r.runtime()
	if _, err := r.cycle(); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	r.reg.ReportHealth(ctx, nil) // first sample
	r.idx.add(5, time.Now().Add(-time.Minute), time.Second, "h2")
	if _, err := r.cycle(); err != nil {
		t.Fatal(err)
	}
	r.reg.ReportHealth(ctx, nil) // counters changed: the source document is written
	s, _, err := r.st.Sources.Get(ctx, domain.DeterministicID(domain.PrefixSource, "wazuh-pull"))
	if err != nil || s.Health == nil || s.Health.LastEventAt == nil {
		t.Fatalf("health must have been recorded: %+v %v", s.Health, err)
	}
	if err := r.reg.Refresh(ctx); err != nil {
		t.Fatal(err)
	}
	after := r.runtime()
	if after != before || after.Signature() != before.Signature() {
		t.Fatal("a health report must not rebuild the source runtime")
	}
	// a real configuration change does
	r.setConfig("max_pages_per_cycle", float64(7))
	if r.runtime().Signature() == before.Signature() {
		t.Fatal("a configuration change must produce a new signature")
	}
}

// A record indexed late, just before a burst that takes several cycles to read, must
// still be found: the sweep re-reads the overlap before where the catch-up began, not
// before where it ended (found in a live run against OpenSearch).
func TestLateArrivalDuringABurstIsPickedUp(t *testing.T) {
	r := newRig(t)
	r.setConfig("max_pages_per_cycle", float64(2))
	r.idx.add(20, time.Now().Add(-10*time.Minute), time.Second, "a")
	if _, err := r.cycle(); err != nil {
		t.Fatal(err)
	}
	base := r.position().TS
	r.idx.mu.Lock()
	r.idx.addAt(base.Add(-20*time.Second), "late.1") // indexed late, behind the cursor
	r.idx.mu.Unlock()
	r.idx.add(3000, time.Now().Add(-2*time.Minute), 10*time.Millisecond, "burst") // three cycles of new data
	for i := 0; ; i++ {
		behind, err := r.cycle()
		if err != nil {
			t.Fatal(err)
		}
		if !behind {
			break
		}
		if i > 10 {
			t.Fatal("did not catch up")
		}
	}
	if r.queued() != 20+1+3000 {
		t.Fatalf("the late record must arrive along with the burst, queue has %d (want %d)", r.queued(), 20+1+3000)
	}
}
