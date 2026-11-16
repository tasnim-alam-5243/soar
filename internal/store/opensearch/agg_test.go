package opensearch

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/yourorg/soar/internal/domain"
	"github.com/yourorg/soar/internal/store"
	"github.com/yourorg/soar/internal/store/opensearch/schema"
)

func TestHistogramRequestAndDecode(t *testing.T) {
	from := time.Date(2026, 11, 16, 10, 0, 0, 0, time.UTC)
	c, log := stub(t, func(r recorded) (int, string) {
		return 200, `{"aggregations":{"h":{"buckets":[
			{"key":` + itoa64(from.UnixMilli()) + `,"doc_count":3,"s":{"buckets":[{"key":5,"doc_count":1},{"key":4,"doc_count":2}]}},
			{"key":` + itoa64(from.Add(time.Hour).UnixMilli()) + `,"doc_count":0,"s":{"buckets":[]}}]}}}`
	})
	r := NewRepo[domain.Alert](c, "soar-alerts", schema.TimeSeries, Options{})
	b, err := r.Histogram(context.Background(), store.ListQuery{}, "received_at", from, from.Add(time.Hour), time.Hour, "severity")
	if err != nil {
		t.Fatal(err)
	}
	if len(b) != 2 || !b[0].Start.Equal(from) || b[0].Counts["5"] != 1 || b[0].Counts["4"] != 2 || len(b[1].Counts) != 0 {
		t.Errorf("decoded: %+v", b)
	}
	var body map[string]any
	_ = json.Unmarshal([]byte((*log)[0].Body), &body)
	h := body["aggs"].(map[string]any)["h"].(map[string]any)
	dh := h["date_histogram"].(map[string]any)
	if (*log)[0].Path != "/soar-alerts/_search" || body["size"] != float64(0) || dh["fixed_interval"] != "3600000ms" || dh["min_doc_count"] != float64(0) {
		t.Errorf("request: %s", (*log)[0].Body)
	}
	if h["aggs"].(map[string]any)["s"].(map[string]any)["terms"].(map[string]any)["field"] != "severity" {
		t.Errorf("split: %s", (*log)[0].Body)
	}
}

func TestTopTermsDecodesStringAndNumberKeys(t *testing.T) {
	c, _ := stub(t, func(r recorded) (int, string) {
		return 200, `{"aggregations":{"t":{"buckets":[{"key":"web-01","doc_count":7},{"key":3,"doc_count":2}]}}}`
	})
	r := NewRepo[domain.Alert](c, "soar-alerts", schema.TimeSeries, Options{})
	top, err := r.TopTerms(context.Background(), store.ListQuery{}, "entities.hosts", 5)
	if err != nil {
		t.Fatal(err)
	}
	if len(top) != 2 || top[0] != (store.Term{Value: "web-01", Count: 7}) || top[1] != (store.Term{Value: "3", Count: 2}) {
		t.Errorf("terms: %+v", top)
	}
}

func TestQueryForOrMissingAndSearch(t *testing.T) {
	q := queryFor(store.ListQuery{
		Terms: map[string][]string{"status": {"new"}}, OrMissing: []string{"status"},
		Search: "brute force", SearchFields: []string{"title", "observables.value"},
	})
	b, _ := json.Marshal(q)
	s := string(b)
	for _, want := range []string{`"must_not":{"exists":{"field":"status"}}`, `"minimum_should_match":1`, `"simple_query_string"`, `"default_operator":"and"`, `"query":"brute force"`} {
		if !strings.Contains(s, want) {
			t.Errorf("query lacks %s: %s", want, s)
		}
	}
	if b, _ := json.Marshal(queryFor(store.ListQuery{})); string(b) != `{"match_all":{}}` {
		t.Errorf("empty query: %s", b)
	}
}

func itoa64(n int64) string { b, _ := json.Marshal(n); return string(b) }
