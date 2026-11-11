//go:build integration

package integration

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/yourorg/soar/internal/domain"
	"github.com/yourorg/soar/internal/store"
	"github.com/yourorg/soar/internal/store/opensearch"
	"github.com/yourorg/soar/internal/store/opensearch/schema"
	"github.com/yourorg/soar/internal/store/storetest"
)

// clean removes every document so each subtest starts empty without recreating indices.
func clean(t *testing.T, c *opensearch.Client, alias string) {
	t.Helper()
	_, _, err := c.Do(context.Background(), http.MethodPost, "/"+alias+"/_delete_by_query?refresh=true&conflicts=proceed",
		map[string]any{"query": map[string]any{"match_all": map[string]any{}}})
	if err != nil {
		t.Fatalf("clean %s: %v", alias, err)
	}
}

func TestOpenSearchRepoSatisfiesStateSuite(t *testing.T) {
	r, c, prefix := testRunner(t)
	if _, err := r.Apply(context.Background()); err != nil {
		t.Fatal(err)
	}
	storetest.RunStateSuite(t, func(t *testing.T, maxRetries int) store.Repo[domain.Case] {
		clean(t, c, prefix+"-cases")
		return opensearch.NewRepo[domain.Case](c, prefix+"-cases", schema.State, opensearch.Options{Refresh: "true", MaxRetries: maxRetries})
	})
}

func TestOpenSearchRepoSatisfiesTimeSeriesSuite(t *testing.T) {
	r, c, prefix := testRunner(t)
	if _, err := r.Apply(context.Background()); err != nil {
		t.Fatal(err)
	}
	storetest.RunTimeSeriesSuite(t, func(t *testing.T) store.Repo[domain.Alert] {
		clean(t, c, prefix+"-alerts")
		return opensearch.NewRepo[domain.Alert](c, prefix+"-alerts", schema.TimeSeries, opensearch.Options{Refresh: "true", MaxRetries: 30})
	})
}

func TestBulkMixedResults(t *testing.T) {
	r, c, prefix := testRunner(t)
	ctx := context.Background()
	if _, err := r.Apply(ctx); err != nil {
		t.Fatal(err)
	}
	st := opensearch.NewStore(c, prefix, opensearch.Options{Refresh: "true"})
	b := opensearch.NewBulk(c, opensearch.BulkConfig{Refresh: "true"})
	idx := st.Names.Index("cases")

	existing := domain.Case{Meta: domain.NewMeta(domain.PrefixCase), Title: "exists", Status: "open", Severity: 2}
	v, err := st.Cases.Create(ctx, existing)
	if err != nil {
		t.Fatal(err)
	}
	fresh := domain.Case{Meta: domain.NewMeta(domain.PrefixCase), Title: "fresh", Status: "open", Severity: 1}
	stale := store.Version{SeqNo: v.SeqNo + 100, PrimaryTerm: v.PrimaryTerm}

	res, err := b.Write(ctx, []store.BulkOp{
		{Action: store.BulkCreate, Index: idx, ID: fresh.ID, Doc: fresh},                                      // 0 ok
		{Action: store.BulkCreate, Index: idx, ID: existing.ID, Doc: existing},                                // 1 conflict
		{Action: store.BulkIndex, Index: idx, ID: existing.ID, Doc: existing, Version: &stale},                // 2 conflict (stale version)
		{Action: store.BulkIndex, Index: idx, ID: existing.ID, Doc: existing, Version: &v},                    // 3 ok (right version)
		{Action: store.BulkIndex, Index: idx, ID: "cas_bad", Doc: map[string]any{"id": "cas_bad", "nope": 1}}, // 4 permanent (strict mapping)
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(res) != 5 {
		t.Fatalf("want 5 results, got %d", len(res))
	}
	if !res[0].OK() || res[0].Version.SeqNo < 0 {
		t.Errorf("0: %+v", res[0])
	}
	if !res[1].Conflict() || !res[2].Conflict() {
		t.Errorf("1/2 must conflict: %+v %+v", res[1], res[2])
	}
	if !res[3].OK() {
		t.Errorf("3 must succeed with the right version: %+v", res[3])
	}
	if !res[4].Permanent() || !strings.Contains(res[4].Err.Type, "mapp") {
		t.Errorf("4 must be rejected by strict mapping: %+v", res[4])
	}
}

// The processor's dedup pattern (spec 9.9): deterministic _id + scripted upsert increments occurrence_count.
func TestBulkScriptedUpsertCountsOccurrences(t *testing.T) {
	r, c, prefix := testRunner(t)
	ctx := context.Background()
	if _, err := r.Apply(ctx); err != nil {
		t.Fatal(err)
	}
	st := opensearch.NewStore(c, prefix, opensearch.Options{Refresh: "true"})
	b := opensearch.NewBulk(c, opensearch.BulkConfig{Refresh: "true"})
	idx := st.Names.Index("alerts")

	now := time.Now().UTC()
	a := domain.Alert{
		Meta: domain.NewMeta(domain.PrefixAlert), Kind: "alert", SourceID: "wazuh-prod", Title: "ssh brute force",
		Severity: 3, ReceivedAt: now, OccurrenceCount: 1, LastSeen: &now,
	}
	upsert := func() store.BulkOp {
		return store.BulkOp{Action: store.BulkUpdate, Index: idx, ID: "alt_fixed", Retries: 5, Body: map[string]any{
			"script": map[string]any{
				"lang":   "painless",
				"source": "ctx._source.occurrence_count += 1; ctx._source.last_seen = params.t; ctx._source.updated_at = params.t",
				"params": map[string]any{"t": time.Now().UTC().Format(time.RFC3339Nano)},
			},
			"upsert": a,
		}}
	}
	for i := 0; i < 3; i++ {
		res, err := b.Write(ctx, []store.BulkOp{upsert()})
		if err != nil || !res[0].OK() {
			t.Fatalf("round %d: %v %+v", i, err, res)
		}
	}
	// Note: the upsert document is stored as given (count 1) and each later update adds one.
	got, _, err := st.Alerts.Get(ctx, "alt_fixed")
	if err != nil {
		t.Fatal(err)
	}
	if got.OccurrenceCount != 3 {
		t.Errorf("occurrence_count = %d, want 3 (1 from upsert + 2 increments)", got.OccurrenceCount)
	}
}

func TestGroupWriterAgainstRealCluster(t *testing.T) {
	r, c, prefix := testRunner(t)
	ctx := context.Background()
	if _, err := r.Apply(ctx); err != nil {
		t.Fatal(err)
	}
	st := opensearch.NewStore(c, prefix, opensearch.Options{})
	g := opensearch.NewGroupWriter(opensearch.NewBulk(c, opensearch.BulkConfig{Refresh: "true"}), opensearch.GroupConfig{FlushEvery: 50 * time.Millisecond})
	defer g.Close()
	idx := st.Names.Index("timeline")

	const writers, per = 40, 5
	var wg sync.WaitGroup
	for w := 0; w < writers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			ops := make([]store.BulkOp, per)
			for i := range ops {
				ev := domain.TimelineEvent{Meta: domain.NewMeta("evt"), SubjectID: fmt.Sprint("cas_", w), EventType: "note", Actor: "test"}
				ops[i] = store.BulkOp{Action: store.BulkCreate, Index: idx, ID: ev.ID, Doc: ev}
			}
			res, err := g.Write(ctx, ops)
			if err != nil || len(res) != per {
				t.Errorf("writer %d: %v n=%d", w, err, len(res))
				return
			}
			for _, x := range res {
				if !x.OK() {
					t.Errorf("writer %d: %+v", w, x)
				}
			}
		}(w)
	}
	wg.Wait()

	_, out, err := c.Do(ctx, http.MethodGet, "/"+idx+"/_count", nil)
	if err != nil {
		t.Fatal(err)
	}
	var cnt struct{ Count int }
	_ = json.Unmarshal(out, &cnt)
	if cnt.Count != writers*per {
		t.Errorf("count = %d, want %d: every acknowledged write must exist exactly once", cnt.Count, writers*per)
	}
}

func TestNewStoreCoversEveryMigratedIndex(t *testing.T) {
	r, c, prefix := testRunner(t)
	ctx := context.Background()
	if _, err := r.Apply(ctx); err != nil {
		t.Fatal(err)
	}
	st := opensearch.NewStore(c, prefix, opensearch.Options{Refresh: "true"})
	// One real write+read per entity type through its own repository proves mapping and Go type agree.
	ok := func(err error, what string) {
		t.Helper()
		if err != nil {
			t.Errorf("%s: %v", what, err)
		}
	}
	now := time.Now().UTC()
	_, err := st.Workflows.Create(ctx, domain.Workflow{Meta: domain.NewMeta(domain.PrefixWorkflow), Name: "w", Version: 1, Status: "draft", Definition: domain.JSON{"nodes": []any{}}, Triggers: []domain.Trigger{{Type: "alert", Match: "true"}}})
	ok(err, "workflows")
	_, err = st.Executions.Create(ctx, domain.Execution{Meta: domain.NewMeta(domain.PrefixExecution), WorkflowID: "wf_1", Status: "queued", Lease: &domain.Lease{Owner: "o", ExpiresAt: now}, Outbox: []domain.OutboxEntry{{Key: "k", Subject: "s", CreatedAt: now}}})
	ok(err, "executions")
	_, err = st.StepRuns.Create(ctx, domain.StepRun{Meta: domain.NewMeta(domain.PrefixStepRun), ExecutionID: "exe_1", NodeID: "n", Status: "ok", Error: &domain.StepError{Code: "x"}})
	ok(err, "steprun")
	_, err = st.CaseTasks.Create(ctx, domain.CaseTask{Meta: domain.NewMeta(domain.PrefixTask), CaseID: "cas_1", Title: "t", Status: "open"})
	ok(err, "case-tasks")
	_, err = st.CaseAlerts.Create(ctx, domain.CaseAlert{Meta: domain.NewMeta("cal"), CaseID: "cas_1", AlertID: "alt_1", Snapshot: domain.JSON{"a": 1}, LinkedAt: now})
	ok(err, "case-alerts")
	_, err = st.Observables.Create(ctx, domain.ObservableDoc{Meta: domain.NewMeta(domain.PrefixObservable), Type: "ip", Value: "10.0.0.1", FirstSeen: now, LastSeen: now, Reputation: &domain.Reputation{Score: 1}})
	ok(err, "observables")
	_, err = st.Audit.Create(ctx, domain.AuditRecord{Meta: domain.NewMeta("aud"), Actor: "a", ActorType: "user", Action: "x", Resource: "r", IP: "10.1.2.3"})
	ok(err, "audit")
	_, err = st.Secrets.Create(ctx, domain.Secret{Meta: domain.NewMeta("sec"), Name: "n", Ciphertext: "YWJj", KeyID: "k", Nonce: "bm9uY2U="})
	ok(err, "secrets")
	_, err = st.Principals.Create(ctx, domain.Principal{Meta: domain.NewMeta("prn"), Type: "user", Name: "n", Roles: []string{"analyst"}})
	ok(err, "principals")
	_, err = st.Idempotency.Create(ctx, domain.Idempotency{Meta: domain.NewMeta("idm"), Key: "k", Status: "done", ExpiresAt: now})
	ok(err, "idempotency")
	_, err = st.Timers.Create(ctx, domain.Timer{Meta: domain.NewMeta("tmr"), ExecutionID: "exe_1", NodeID: "n", DueAt: now, Kind: "wait"})
	ok(err, "timers")
	_, err = st.Sources.Create(ctx, domain.Source{Meta: domain.NewMeta(domain.PrefixSource), Name: "s", Adapter: "webhook", Parser: "json", MappingID: "m@1", Enabled: true, Health: &domain.SourceHealth{Status: "ok"}, Auth: domain.JSON{"type": "hmac"}})
	ok(err, "sources")
	_, err = st.Mappings.Create(ctx, domain.Mapping{Meta: domain.NewMeta("map"), Name: "m", Version: 1, Kind: "alert", Definition: domain.JSON{"fields": 1}})
	ok(err, "mappings")
	_, err = st.Filters.Create(ctx, domain.FilterSet{Meta: domain.NewMeta("flt"), Name: "f", Version: 1, Mode: "first_match", Default: "keep", Rules: []domain.JSON{{"name": "r"}}})
	ok(err, "filters")
	_, err = st.Lists.Create(ctx, domain.List{Meta: domain.NewMeta("lst"), Name: "l", Items: []string{"1", "2"}})
	ok(err, "lists")
	_, err = st.Cursors.Create(ctx, domain.Cursor{Meta: domain.NewMeta("cur"), SourceID: "sr_1", Cursor: "https://graph.microsoft.com/v1.0/delta?token=abc"})
	ok(err, "cursors")
	_, err = st.Raw.Create(ctx, domain.RawRecord{Meta: domain.NewMeta("raw"), SourceID: "sr_1", ReceivedAt: now, Body: `{"a":1}`})
	ok(err, "raw")
	_, err = st.Seen.Create(ctx, domain.Seen{Meta: domain.NewMeta("see"), Key: "k", FirstSeen: now})
	ok(err, "seen")
	_, err = st.Connections.Create(ctx, domain.Connection{Meta: domain.NewMeta("con"), ConnectorID: "iris", Name: "n", AuthType: "bearer", SecretRefs: domain.JSON{"token": "s"}})
	ok(err, "connections")
	_, err = st.Timeline.Create(ctx, domain.TimelineEvent{Meta: domain.NewMeta("evt"), SubjectID: "cas_1", EventType: "x", Data: domain.JSON{"k": "v"}})
	ok(err, "timeline")
	_, err = st.Cases.Create(ctx, domain.Case{Meta: domain.NewMeta(domain.PrefixCase), Title: "c", Status: "open", SLA: &domain.SLA{Policy: "p"}, ExternalRefs: []domain.ExternalRef{{System: "iris", ID: "1"}}})
	ok(err, "cases")
}
