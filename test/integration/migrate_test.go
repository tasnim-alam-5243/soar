//go:build integration

package integration

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/yourorg/soar/internal/domain"
	"github.com/yourorg/soar/internal/store/opensearch"
	"github.com/yourorg/soar/internal/store/opensearch/migrate"
	"github.com/yourorg/soar/internal/store/opensearch/schema"
)

func osClient() *opensearch.Client {
	url := os.Getenv("SOAR_OS_URLS")
	if url == "" {
		url = "http://127.0.0.1:9200"
	}
	return opensearch.New(strings.Split(url, ","), os.Getenv("SOAR_OS_USER"), os.Getenv("SOAR_OS_PASSWORD"))
}

// testRunner uses a random index prefix, so it never touches real soar-* indices.
func testRunner(t *testing.T) (*migrate.Runner, *opensearch.Client, string) {
	t.Helper()
	c := osClient()
	ret, err := schema.LoadRetention("../../config/retention.yaml")
	if err != nil {
		t.Fatal(err)
	}
	prefix := fmt.Sprintf("soartest%d", time.Now().UnixNano()%1_000_000_000)
	r := &migrate.Runner{C: c, Log: slog.New(slog.NewTextHandler(io.Discard, nil)),
		Opts: schema.Options{Prefix: prefix, Retention: ret}}
	t.Cleanup(func() { cleanup(c, prefix) })
	return r, c, prefix
}

func cleanup(c *opensearch.Client, prefix string) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	_, out, err := c.Do(ctx, http.MethodGet, "/_cat/indices/"+prefix+"-*?format=json&h=index", nil, 404)
	if err == nil {
		var rows []struct {
			Index string `json:"index"`
		}
		_ = json.Unmarshal(out, &rows)
		for _, r := range rows {
			_, _, _ = c.Do(ctx, http.MethodDelete, "/"+r.Index, nil, 404)
		}
	}
	_, _, _ = c.Do(ctx, http.MethodDelete, "/_index_template/"+prefix+"-*", nil, 404)
	for _, e := range schema.Entities() {
		if e.Kind == schema.TimeSeries {
			_, _, _ = c.Do(ctx, http.MethodDelete, "/_plugins/_ism/policies/"+prefix+"-"+e.Name+"-retention", nil, 404)
		}
	}
}

func TestMigrateRepeatableAgainstRealOpenSearch(t *testing.T) {
	r, _, _ := testRunner(t)
	ctx := context.Background()

	first, err := r.Apply(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, x := range first {
		if x.Failed() {
			t.Fatalf("%s: %s %s", x.Alias, x.Action, x.Detail)
		}
	}
	second, err := r.Apply(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, x := range second {
		if x.Action != "unchanged" {
			t.Errorf("second run %s: %s %s (migrations must be repeatable)", x.Alias, x.Action, x.Detail)
		}
	}
	check, err := r.Check(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, x := range check {
		if x.Failed() {
			t.Errorf("check %s: %s", x.Alias, x.Detail)
		}
	}
}

func TestStrictMappingRejectsUnknownFields(t *testing.T) {
	r, c, prefix := testRunner(t)
	ctx := context.Background()
	if _, err := r.Apply(ctx); err != nil {
		t.Fatal(err)
	}

	// A valid document is accepted.
	wf := domain.Workflow{Meta: domain.NewMeta(domain.PrefixWorkflow), Name: "phishing_triage", Version: 1,
		Status: "draft", Definition: domain.JSON{"nodes": []any{map[string]any{"id": "a"}}}}
	if _, _, err := c.Do(ctx, http.MethodPut, "/"+prefix+"-workflows/_doc/"+wf.ID+"?refresh=true", wf); err != nil {
		t.Fatalf("valid workflow rejected: %v", err)
	}

	// An unknown field must be rejected (dynamic: strict) - the exit gate.
	bad := map[string]any{"id": "wf_x", "name": "x", "surprise_field": 1}
	_, _, err := c.Do(ctx, http.MethodPut, "/"+prefix+"-workflows/_doc/wf_x", bad)
	var apiErr *opensearch.APIError
	if !errors.As(err, &apiErr) || apiErr.Status != 400 || !strings.Contains(apiErr.Body, "strict_dynamic_mapping_exception") {
		t.Fatalf("unknown field must fail with strict_dynamic_mapping_exception, got: %v", err)
	}

	// Same rule inside nested objects.
	badNested := domain.JSON{"id": "alt_x", "title": "t", "observables": []any{map[string]any{"type": "ip", "value": "1.2.3.4", "oops": true}}}
	if _, _, err := c.Do(ctx, http.MethodPut, "/"+prefix+"-alerts/_doc/alt_x", badNested); err == nil {
		t.Fatal("unknown field inside observables must be rejected")
	}
}

func TestAlertsRolloverAliasAndFlatExt(t *testing.T) {
	r, c, prefix := testRunner(t)
	ctx := context.Background()
	if _, err := r.Apply(ctx); err != nil {
		t.Fatal(err)
	}
	a := domain.Alert{Meta: domain.NewMeta(domain.PrefixAlert), Kind: "alert", SourceID: "wazuh-prod",
		Title: "sshd: authentication success", Severity: 1, ReceivedAt: time.Now().UTC(), OccurrenceCount: 1,
		Observables: []domain.Observable{{Type: "ip", Value: "10.10.192.1", Role: "source"}},
		// Vendor fields can have any shape under ext without being mapped one by one.
		Ext: domain.JSON{"rule": map[string]any{"id": "5715", "groups": []any{"syslog", "sshd"}}, "anything_new": "ok"}}
	// Write through the write alias, as the processor will.
	if _, _, err := c.Do(ctx, http.MethodPut, "/"+prefix+"-alerts/_doc/"+a.ID+"?refresh=true", a); err != nil {
		t.Fatalf("alert write via alias failed: %v", err)
	}
	q := map[string]any{"query": map[string]any{"term": map[string]any{"ext.rule.id": "5715"}}}
	_, out, err := c.Do(ctx, http.MethodPost, "/"+prefix+"-alerts/_search", q)
	if err != nil || !strings.Contains(string(out), `"value":1`) {
		t.Fatalf("flat_object search on ext.rule.id failed: %v %s", err, out)
	}
}

func TestOutboxIsNestedSoSweeperQueriesAreExact(t *testing.T) {
	r, c, prefix := testRunner(t)
	ctx := context.Background()
	if _, err := r.Apply(ctx); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	e := domain.Execution{Meta: domain.NewMeta(domain.PrefixExecution), WorkflowID: "wf_1", WorkflowVersion: 1, Status: "running",
		Outbox: []domain.OutboxEntry{
			{Key: "k1", Subject: "steps.normal.http", Published: true, CreatedAt: now},
			{Key: "k2", Subject: "steps.normal.http", Published: false, CreatedAt: now},
		}}
	if _, _, err := c.Do(ctx, http.MethodPut, "/"+prefix+"-executions/_doc/"+e.ID+"?refresh=true", e); err != nil {
		t.Fatal(err)
	}
	count := func(key string) string {
		q := map[string]any{"query": map[string]any{"nested": map[string]any{"path": "outbox", "query": map[string]any{"bool": map[string]any{"filter": []any{
			map[string]any{"term": map[string]any{"outbox.key": key}},
			map[string]any{"term": map[string]any{"outbox.published": false}},
		}}}}}}
		_, out, err := c.Do(ctx, http.MethodPost, "/"+prefix+"-executions/_count", q)
		if err != nil {
			t.Fatal(err)
		}
		return string(out)
	}
	if !strings.Contains(count("k2"), `"count":1`) {
		t.Error("k2 (unpublished) must match")
	}
	if !strings.Contains(count("k1"), `"count":0`) {
		t.Error("k1 (published) must not match; nested mapping keeps conditions on one entry")
	}
}

// TestLiveStackSchemaMatches checks the real soar-* indices that `make up` created
// through the migrate service, the same check as `make migrate-check`.
func TestLiveStackSchemaMatches(t *testing.T) {
	ret, err := schema.LoadRetention("../../config/retention.yaml")
	if err != nil {
		t.Fatal(err)
	}
	r := &migrate.Runner{C: osClient(), Log: slog.New(slog.NewTextHandler(io.Discard, nil)),
		Opts: schema.Options{Prefix: envOr("SOAR_INDEX_PREFIX", "soar"), Retention: ret,
			StateReplicas: envInt("SOAR_OS_STATE_REPLICAS"), TimeSeriesReplicas: envInt("SOAR_OS_TS_REPLICAS")}}
	res, err := r.Check(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(res) != 22 {
		t.Fatalf("want 22 indices checked, got %d", len(res))
	}
	for _, x := range res {
		if x.Failed() {
			t.Errorf("%s: %s", x.Alias, x.Detail)
		}
	}
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func envInt(k string) int {
	n, _ := strconv.Atoi(os.Getenv(k))
	return n
}
