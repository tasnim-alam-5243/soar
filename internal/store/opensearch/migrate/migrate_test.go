package migrate

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/yourorg/soar/internal/store/opensearch"
	"github.com/yourorg/soar/internal/store/opensearch/schema"
)

// fakeOS is a tiny in-memory OpenSearch that implements only what the runner calls.
type fakeOS struct {
	mu        sync.Mutex
	indices   map[string]map[string]any // index -> mappings
	aliases   map[string]string         // alias -> index
	templates map[string]map[string]any
	policies  map[string]int
	docs      map[string]json.RawMessage
	calls     []string
}

func newFake() *fakeOS {
	return &fakeOS{indices: map[string]map[string]any{}, aliases: map[string]string{}, templates: map[string]map[string]any{},
		policies: map[string]int{}, docs: map[string]json.RawMessage{}}
}

func (f *fakeOS) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	b, _ := io.ReadAll(r.Body)
	p := r.URL.Path
	f.calls = append(f.calls, r.Method+" "+p)
	var body map[string]any
	_ = json.Unmarshal(b, &body)
	reply := func(code int, v any) {
		w.WriteHeader(code)
		if v != nil && r.Method != http.MethodHead {
			_ = json.NewEncoder(w).Encode(v)
		}
	}
	switch {
	case strings.HasPrefix(p, "/_cluster/health"):
		reply(200, map[string]any{"status": "yellow"})
	case strings.HasPrefix(p, "/_alias/"):
		if _, ok := f.aliases[strings.TrimPrefix(p, "/_alias/")]; ok {
			reply(200, nil)
		} else {
			reply(404, nil)
		}
	case strings.HasPrefix(p, "/_index_template/") && r.Method == http.MethodPut:
		f.templates[strings.TrimPrefix(p, "/_index_template/")] = body
		reply(200, map[string]any{"acknowledged": true})
	case strings.HasPrefix(p, "/_plugins/_ism/policies/"):
		id := strings.TrimPrefix(p, "/_plugins/_ism/policies/")
		if r.Method == http.MethodGet {
			if n, ok := f.policies[id]; ok {
				reply(200, map[string]any{"_seq_no": n, "_primary_term": 1})
			} else {
				reply(404, map[string]any{"error": "not found"})
			}
			return
		}
		if _, ok := f.policies[id]; ok && r.URL.Query().Get("if_seq_no") == "" {
			reply(409, map[string]any{"error": "exists"}) // matches real behavior: update needs seq_no
			return
		}
		f.policies[id]++
		reply(200, map[string]any{})
	case strings.HasPrefix(p, "/_plugins/_ism/"):
		reply(200, map[string]any{"updated_indices": 1, "failures": false})
	case strings.Contains(p, "/_doc/") || strings.Contains(p, "/_source/"):
		key := p[strings.LastIndex(p, "/")+1:]
		if r.Method == http.MethodPut {
			f.docs[key] = b
			reply(201, map[string]any{"result": "created"})
		} else if d, ok := f.docs[key]; ok {
			w.WriteHeader(200)
			_, _ = w.Write(d)
		} else {
			reply(404, map[string]any{"found": false})
		}
	case strings.HasSuffix(p, "/_mapping"):
		alias := strings.TrimSuffix(strings.TrimPrefix(p, "/"), "/_mapping")
		idx, ok := f.aliases[alias]
		if !ok {
			reply(404, map[string]any{"error": "index_not_found_exception"})
			return
		}
		if r.Method == http.MethodPut {
			f.indices[idx] = body
			reply(200, map[string]any{"acknowledged": true})
		} else {
			reply(200, map[string]any{idx: map[string]any{"mappings": f.indices[idx]}})
		}
	case strings.HasSuffix(p, "/_settings"):
		reply(200, map[string]any{"acknowledged": true})
	case r.Method == http.MethodPut: // create index
		name := strings.TrimPrefix(p, "/")
		if _, dup := f.indices[name]; dup {
			reply(400, map[string]any{"error": "resource_already_exists_exception"})
			return
		}
		m, _ := body["mappings"].(map[string]any)
		for tn, t := range f.templates { // apply matching template
			if strings.HasPrefix(name, tn) {
				m = t["template"].(map[string]any)["mappings"].(map[string]any)
			}
		}
		f.indices[name] = m
		if a, ok := body["aliases"].(map[string]any); ok {
			for al := range a {
				f.aliases[al] = name
			}
		}
		reply(200, map[string]any{"acknowledged": true})
	default:
		reply(500, map[string]any{"error": "fake: unhandled " + r.Method + " " + p})
	}
}

func newRunner(t *testing.T, f *fakeOS) *Runner {
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	cl := opensearch.New([]string{srv.URL}, "", "")
	ret, _ := schema.ParseRetention([]byte("default: 7d\nclasses:\n  cases: 3d\n  seen: ttl\n  idempotency: ttl\n"))
	return &Runner{C: cl, Log: slog.New(slog.NewTextHandler(io.Discard, nil)),
		Opts: schema.Options{Prefix: "soar", Retention: ret}}
}

func TestApplyIsRepeatable(t *testing.T) {
	f := newFake()
	r := newRunner(t, f)
	ctx := context.Background()

	first, err := r.Apply(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, res := range first {
		if res.Action != "created" {
			t.Errorf("first run %s: want created, got %s %s", res.Alias, res.Action, res.Detail)
		}
	}
	if len(first) != 22 {
		t.Fatalf("want 22 results, got %d", len(first))
	}
	writes := len(f.calls)

	second, err := r.Apply(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, res := range second {
		if res.Action != "unchanged" {
			t.Errorf("second run %s: want unchanged, got %s %s", res.Alias, res.Action, res.Detail)
		}
	}
	for _, c := range f.calls[writes:] {
		if strings.HasPrefix(c, "PUT ") || strings.HasPrefix(c, "POST ") {
			t.Errorf("second run must not write, saw %s", c)
		}
	}

	check, err := r.Check(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, res := range check {
		if res.Failed() {
			t.Errorf("check %s: %s %s", res.Alias, res.Action, res.Detail)
		}
	}
}

func TestRetentionChangeUpdatesPolicyOnly(t *testing.T) {
	f := newFake()
	r := newRunner(t, f)
	ctx := context.Background()
	if _, err := r.Apply(ctx); err != nil {
		t.Fatal(err)
	}
	r.Opts.Retention.Classes["audit"] = "365d"
	res, err := r.Apply(ctx)
	if err != nil {
		t.Fatal(err)
	}
	changed := 0
	for _, x := range res {
		if x.Action == "updated" {
			changed++
			if x.Alias != "soar-audit" {
				t.Errorf("unexpected update of %s", x.Alias)
			}
		} else if x.Action != "unchanged" {
			t.Errorf("%s: %s %s", x.Alias, x.Action, x.Detail)
		}
	}
	if changed != 1 {
		t.Errorf("want exactly soar-audit updated, got %d changes", changed)
	}
	if f.policies["soar-audit-retention"] != 2 {
		t.Errorf("audit policy should have been rewritten once")
	}
}

func TestCheckDetectsDriftAndNonStrict(t *testing.T) {
	f := newFake()
	r := newRunner(t, f)
	ctx := context.Background()
	if _, err := r.Apply(ctx); err != nil {
		t.Fatal(err)
	}
	f.indices["soar-cases-v1"]["dynamic"] = "true"
	delete(f.indices["soar-alerts-v1-000001"]["properties"].(map[string]any), "severity")
	res, _ := r.Check(ctx)
	bad := map[string]string{}
	for _, x := range res {
		if x.Failed() {
			bad[x.Alias] = x.Detail
		}
	}
	if len(bad) != 2 || !strings.Contains(bad["soar-cases"], "not strict") || !strings.Contains(bad["soar-alerts"], "severity") {
		t.Errorf("unexpected check output: %v", bad)
	}
}

func TestCheckBeforeApplyReportsDrift(t *testing.T) {
	r := newRunner(t, newFake())
	res, err := r.Check(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, x := range res {
		if !x.Failed() {
			t.Errorf("%s should be drift before any migration", x.Alias)
		}
	}
}
