package opensearch

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/yourorg/soar/internal/domain"
	"github.com/yourorg/soar/internal/store"
	"github.com/yourorg/soar/internal/store/opensearch/schema"
)

type recorded struct {
	Method, Path, Query, Body, CT string
}

// stub answers with canned responses chosen by handler and records every request.
func stub(t *testing.T, handler func(r recorded) (int, string)) (*Client, *[]recorded) {
	t.Helper()
	var mu sync.Mutex
	var log []recorded
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		rec := recorded{r.Method, r.URL.Path, r.URL.RawQuery, string(b), r.Header.Get("Content-Type")}
		mu.Lock()
		log = append(log, rec)
		mu.Unlock()
		code, body := handler(rec)
		w.WriteHeader(code)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(s.Close)
	return New([]string{s.URL}, "", ""), &log
}

func caseRepo(c *Client, refresh string) *Repo[domain.Case] {
	return NewRepo[domain.Case](c, "soar-cases", schema.State, Options{Refresh: refresh, MaxRetries: 2})
}

func TestCreateUsesCreateEndpointAndMaps409(t *testing.T) {
	c, log := stub(t, func(r recorded) (int, string) {
		if strings.Contains(r.Path, "dup") {
			return 409, `{"error":{"type":"version_conflict_engine_exception"},"status":409}`
		}
		return 201, `{"_index":"soar-cases-v1","_seq_no":7,"_primary_term":2,"result":"created"}`
	})
	r := caseRepo(c, "true")
	doc := domain.Case{Meta: domain.NewMeta(domain.PrefixCase), Title: "x"}
	doc.ID = "cas_new"
	v, err := r.Create(context.Background(), doc)
	if err != nil || v != (store.Version{Index: "soar-cases-v1", SeqNo: 7, PrimaryTerm: 2}) {
		t.Fatalf("create: %v %+v", err, v)
	}
	got := (*log)[0]
	if got.Method != "PUT" || got.Path != "/soar-cases/_create/cas_new" || got.Query != "refresh=true" {
		t.Errorf("unexpected request: %+v", got)
	}
	doc.ID = "cas_dup"
	if _, err := r.Create(context.Background(), doc); !errors.Is(err, store.ErrAlreadyExists) {
		t.Errorf("409 must map to ErrAlreadyExists, got %v", err)
	}
}

func TestSaveSendsSeqNoAndTargetsConcreteIndex(t *testing.T) {
	c, log := stub(t, func(r recorded) (int, string) {
		if r.Query != "" && strings.Contains(r.Query, "if_seq_no=3") {
			return 200, `{"_index":"soar-alerts-v1-000002","_seq_no":4,"_primary_term":1}`
		}
		return 409, `{"error":{"type":"version_conflict_engine_exception"}}`
	})
	r := NewRepo[domain.Alert](c, "soar-alerts", schema.TimeSeries, Options{})
	a := domain.Alert{Meta: domain.NewMeta(domain.PrefixAlert)}
	a.ID = "alt_1"
	nv, err := r.Save(context.Background(), a, store.Version{Index: "soar-alerts-v1-000002", SeqNo: 3, PrimaryTerm: 1})
	if err != nil || nv.SeqNo != 4 {
		t.Fatalf("save: %v %+v", err, nv)
	}
	if g := (*log)[0]; g.Path != "/soar-alerts-v1-000002/_doc/alt_1" || g.Query != "if_primary_term=1&if_seq_no=3" {
		t.Errorf("unexpected request: %+v", g)
	}
	_, err = r.Save(context.Background(), a, store.Version{SeqNo: 9, PrimaryTerm: 1})
	if !errors.Is(err, store.ErrVersionConflict) {
		t.Errorf("409 must map to ErrVersionConflict, got %v", err)
	}
}

func TestConflictHookAndUpdateRetries(t *testing.T) {
	gets := 0
	c, _ := stub(t, func(r recorded) (int, string) {
		if r.Method == "GET" {
			gets++
			return 200, `{"_index":"i","_id":"cas_1","_seq_no":1,"_primary_term":1,"found":true,"_source":{"id":"cas_1","title":"t"}}`
		}
		return 409, `{"error":{"type":"version_conflict_engine_exception"}}`
	})
	conflicts := 0
	r := NewRepo[domain.Case](c, "soar-cases", schema.State, Options{MaxRetries: 2, OnConflict: func(string) { conflicts++ }})
	_, _, err := r.Update(context.Background(), "cas_1", func(*domain.Case) error { return nil })
	if !errors.Is(err, store.ErrConflictExhausted) || gets != 3 || conflicts != 3 {
		t.Fatalf("err=%v gets=%d conflicts=%d (want exhausted, 3, 3)", err, gets, conflicts)
	}
}

func TestGetStateDistinguishesMissingDocFromMissingIndex(t *testing.T) {
	c, _ := stub(t, func(r recorded) (int, string) {
		if strings.Contains(r.Path, "nodoc") {
			return 404, `{"_index":"soar-cases-v1","_id":"nodoc","found":false}`
		}
		return 404, `{"error":{"type":"index_not_found_exception","reason":"no such index"},"status":404}`
	})
	r := caseRepo(c, "")
	if _, _, err := r.Get(context.Background(), "nodoc"); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("missing doc: want ErrNotFound, got %v", err)
	}
	_, _, err := r.Get(context.Background(), "x")
	var ae *APIError
	if errors.Is(err, store.ErrNotFound) || !errors.As(err, &ae) {
		t.Errorf("missing index must surface as APIError, got %v", err)
	}
}

func TestTimeSeriesGetUsesIdsSearchAndReturnsConcreteIndex(t *testing.T) {
	c, log := stub(t, func(recorded) (int, string) {
		return 200, `{"hits":{"hits":[{"_index":"soar-alerts-v1-000003","_seq_no":11,"_primary_term":1,"_source":{"id":"alt_1","title":"x"}}]}}`
	})
	r := NewRepo[domain.Alert](c, "soar-alerts", schema.TimeSeries, Options{})
	a, v, err := r.Get(context.Background(), "alt_1")
	if err != nil || a.ID != "alt_1" || v.Index != "soar-alerts-v1-000003" || v.SeqNo != 11 {
		t.Fatalf("get: %v %+v %+v", err, a, v)
	}
	if g := (*log)[0]; g.Path != "/soar-alerts/_search" || !strings.Contains(g.Body, `"seq_no_primary_term":true`) || !strings.Contains(g.Body, `"ids"`) {
		t.Errorf("unexpected request: %+v", g)
	}
	if err := r.Delete(context.Background(), "alt_1"); !errors.Is(err, store.ErrUnsupported) {
		t.Errorf("time-series delete must be unsupported, got %v", err)
	}
}

func TestListBuildsQueryAndCursor(t *testing.T) {
	c, log := stub(t, func(recorded) (int, string) {
		return 200, `{"hits":{"hits":[
		 {"_source":{"id":"cas_a"},"sort":[3,"cas_a"]},
		 {"_source":{"id":"cas_b"},"sort":[3,"cas_b"]},
		 {"_source":{"id":"cas_c"},"sort":[2,"cas_c"]}]}}`
	})
	r := caseRepo(c, "")
	cursor := base64.RawURLEncoding.EncodeToString([]byte(`[5,"cas_z"]`))
	p, err := r.List(context.Background(), store.ListQuery{
		Terms: map[string][]string{"status": {"open", "in_progress"}}, Ranges: []store.Range{{Field: "severity", Gte: 2}},
		Sort: "severity", Desc: true, Limit: 2, After: cursor,
	})
	if err != nil || len(p.Items) != 2 {
		t.Fatalf("list: %v n=%d", err, len(p.Items))
	}
	raw, _ := base64.RawURLEncoding.DecodeString(p.NextCursor)
	if string(raw) != `[3,"cas_b"]` {
		t.Errorf("next cursor = %s, want the sort values of the last returned row", raw)
	}
	var body map[string]any
	_ = json.Unmarshal([]byte((*log)[0].Body), &body)
	if body["size"] != float64(3) || body["search_after"] == nil || body["track_total_hits"] != false {
		t.Errorf("unexpected body: %s", (*log)[0].Body)
	}
	for _, want := range []string{`"terms":{"status":["open","in_progress"]}`, `"range":{"severity":{"gte":2}}`, `"order":"desc"`} {
		if !strings.Contains((*log)[0].Body, want) {
			t.Errorf("body lacks %s: %s", want, (*log)[0].Body)
		}
	}
	if _, err := r.List(context.Background(), store.ListQuery{After: "%%%"}); !errors.Is(err, store.ErrInvalid) {
		t.Errorf("bad cursor: want ErrInvalid, got %v", err)
	}
}

func TestBulkEncodingChunkingAndResults(t *testing.T) {
	calls := 0
	c, log := stub(t, func(r recorded) (int, string) {
		calls++
		var items []string
		for _, line := range strings.Split(strings.TrimSpace(r.Body), "\n") {
			var m map[string]map[string]any
			if json.Unmarshal([]byte(line), &m) != nil {
				continue
			}
			for action, meta := range m {
				if _, isMeta := meta["_index"]; !isMeta {
					continue
				}
				switch {
				case meta["_id"] == "dup":
					items = append(items, `{"`+action+`":{"_index":"i","_id":"dup","status":409,"error":{"type":"version_conflict_engine_exception","reason":"exists"}}}`)
				case meta["_id"] == "bad":
					items = append(items, `{"`+action+`":{"_index":"i","_id":"bad","status":400,"error":{"type":"strict_dynamic_mapping_exception","reason":"no"}}}`)
				case meta["_id"] == "busy":
					items = append(items, `{"`+action+`":{"_index":"i","_id":"busy","status":429,"error":{"type":"es_rejected_execution_exception","reason":"queue full"}}}`)
				default:
					items = append(items, `{"`+action+`":{"_index":"i","_id":"`+meta["_id"].(string)+`","status":201,"_seq_no":5,"_primary_term":1}}`)
				}
			}
		}
		return 200, `{"errors":true,"items":[` + strings.Join(items, ",") + `]}`
	})
	b := NewBulk(c, BulkConfig{MaxDocs: 3})
	ops := []store.BulkOp{
		{Action: store.BulkCreate, Index: "soar-alerts", ID: "a1", Doc: map[string]any{"x": 1}},
		{Action: store.BulkCreate, Index: "soar-alerts", ID: "dup", Doc: map[string]any{"x": 1}},
		{Action: store.BulkIndex, Index: "soar-executions", ID: "e1", Doc: map[string]any{"x": 1}, Version: &store.Version{SeqNo: 4, PrimaryTerm: 1}},
		{Action: store.BulkUpdate, Index: "soar-alerts", ID: "bad", Body: map[string]any{"doc": map[string]any{"y": 1}}, Retries: 3},
		{Action: store.BulkDelete, Index: "soar-seen", ID: "busy"},
	}
	res, err := b.Write(context.Background(), ops)
	if err != nil {
		t.Fatal(err)
	}
	if calls != 2 || len(res) != 5 {
		t.Fatalf("calls=%d results=%d (want 2 requests for 5 ops with MaxDocs=3)", calls, len(res))
	}
	if !res[0].OK() || res[0].Version.SeqNo != 5 {
		t.Errorf("op0: %+v", res[0])
	}
	if !res[1].Conflict() || res[1].OK() || res[1].Permanent() {
		t.Errorf("op1 should be a conflict: %+v", res[1])
	}
	if !res[3].Permanent() || res[3].Err.Type != "strict_dynamic_mapping_exception" {
		t.Errorf("op3 should be permanent: %+v", res[3])
	}
	if !res[4].Retryable() || res[4].Permanent() {
		t.Errorf("op4 should be retryable: %+v", res[4])
	}

	first := (*log)[0]
	if first.CT != "application/x-ndjson" || first.Path != "/_bulk" || !strings.HasSuffix(first.Body, "\n") {
		t.Errorf("bad bulk request: %+v", first)
	}
	if !strings.Contains(first.Body, `{"index":{"_id":"e1","_index":"soar-executions","if_primary_term":1,"if_seq_no":4}}`) {
		t.Errorf("optimistic concurrency fields missing: %s", first.Body)
	}
	second := (*log)[1].Body
	if !strings.Contains(second, `"retry_on_conflict":3`) || !strings.Contains(second, `{"doc":{"y":1}}`) || !strings.Contains(second, `{"delete":`) {
		t.Errorf("second chunk wrong: %s", second)
	}
}

func TestBulkRejectsBadOpsAndPropagatesRequestFailure(t *testing.T) {
	c, _ := stub(t, func(recorded) (int, string) { return 429, `{"error":"rejected"}` })
	b := NewBulk(c, BulkConfig{})
	for name, op := range map[string]store.BulkOp{
		"no index":     {Action: store.BulkCreate, ID: "a", Doc: 1},
		"update no id": {Action: store.BulkUpdate, Index: "i", Body: map[string]any{}},
		"unknown":      {Action: "merge", Index: "i", ID: "a"},
		"delete no id": {Action: store.BulkDelete, Index: "i"},
	} {
		if _, err := b.Write(context.Background(), []store.BulkOp{op}); !errors.Is(err, store.ErrInvalid) {
			t.Errorf("%s: want ErrInvalid, got %v", name, err)
		}
	}
	_, err := b.Write(context.Background(), []store.BulkOp{{Action: store.BulkCreate, Index: "i", ID: "a", Doc: 1}})
	var ae *APIError
	if !errors.As(err, &ae) || ae.Status != 429 {
		t.Errorf("whole-request 429 must surface as APIError, got %v", err)
	}
}

type fakeBulk struct {
	mu      sync.Mutex
	batches []int
}

func (f *fakeBulk) Write(_ context.Context, ops []store.BulkOp) ([]store.BulkResult, error) {
	f.mu.Lock()
	f.batches = append(f.batches, len(ops))
	f.mu.Unlock()
	out := make([]store.BulkResult, len(ops))
	for i, op := range ops {
		out[i] = store.BulkResult{Status: 201, ID: op.ID}
	}
	return out, nil
}

func TestGroupWriterMergesConcurrentCallersAndRoutesResults(t *testing.T) {
	fb := &fakeBulk{}
	g := NewGroupWriter(fb, GroupConfig{FlushEvery: 100 * time.Millisecond})
	defer g.Close()
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			id := string(rune('a' + i))
			res, err := g.Write(context.Background(), []store.BulkOp{{Action: store.BulkCreate, Index: "i", ID: id + "1"}, {Action: store.BulkCreate, Index: "i", ID: id + "2"}})
			if err != nil || len(res) != 2 || res[0].ID != id+"1" || res[1].ID != id+"2" {
				t.Errorf("caller %d got %+v %v", i, res, err)
			}
		}(i)
	}
	wg.Wait()
	total := 0
	for _, n := range fb.batches {
		total += n
	}
	if total != 40 || len(fb.batches) > 5 {
		t.Errorf("want 40 ops in a few merged batches, got %v", fb.batches)
	}
	g.Close()
	if _, err := g.Write(context.Background(), []store.BulkOp{{Action: store.BulkCreate, Index: "i", ID: "x"}}); !errors.Is(err, ErrClosed) {
		t.Errorf("write after close: want ErrClosed, got %v", err)
	}
}

func TestGroupWriterFlushesWhenFullBeforeTimer(t *testing.T) {
	fb := &fakeBulk{}
	g := NewGroupWriter(fb, GroupConfig{MaxDocs: 2, FlushEvery: 10 * time.Second})
	defer g.Close()
	start := time.Now()
	if _, err := g.Write(context.Background(), []store.BulkOp{{Action: store.BulkCreate, Index: "i", ID: "a"}, {Action: store.BulkCreate, Index: "i", ID: "b"}}); err != nil {
		t.Fatal(err)
	}
	if time.Since(start) > 2*time.Second {
		t.Error("a full batch must flush immediately, not wait for the timer")
	}
}
