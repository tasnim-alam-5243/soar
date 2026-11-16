package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/yourorg/soar/integrations"
	"github.com/yourorg/soar/internal/domain"
	"github.com/yourorg/soar/internal/expr"
	"github.com/yourorg/soar/internal/ingest"
	"github.com/yourorg/soar/internal/ingest/pack"
)

// The generator is worthless if the platform filters or rejects its events, so check
// that every generated event passes the real Wazuh pipeline.
func TestGeneratedEventsPassTheWazuhPipeline(t *testing.T) {
	b, _ := pack.Load(integrations.Packs, "wazuh")
	src := b.Sources[1] // wazuh-prod
	if src.Name != "wazuh-prod" {
		src = b.Sources[0]
	}
	p, err := ingest.BuildPipeline(src, expr.NoLists{}, ingest.Resolver{
		Mappings: func(string, int) (domain.Mapping, error) { return b.Mappings[0], nil },
		Filters:  func(string, int) (domain.FilterSet, error) { return b.Filters[0], nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	var recs []map[string]any
	for n := 0; n < 500; n++ {
		raw, _ := json.Marshal(event("abc123", n, 200, time.Now()))
		var m map[string]any
		_ = json.Unmarshal(raw, &m)
		recs = append(recs, m)
	}
	out := p.ProcessRecords(recs)
	if len(out.Events) != 500 || out.Filtered != 0 || len(out.Failures) != 0 {
		t.Fatalf("events=%d filtered=%d failures=%v", len(out.Events), out.Filtered, out.Failures)
	}
	fps, ids := map[string]bool{}, map[string]bool{}
	for _, e := range out.Events {
		fps[e.Alert.Fingerprint] = true
		ids[e.Alert.ExternalID] = true
		hosts := e.Alert.Entities.Hosts
		if len(hosts) != 1 || hosts[0] != "load-abc123" {
			t.Fatalf("the verification query relies on entities.hosts = load-<run>: %v", hosts)
		}
	}
	if len(ids) != 500 || len(fps) != 200 {
		t.Errorf("500 unique ids and 200 alert groups expected, got %d and %d", len(ids), len(fps))
	}
}

func TestPercentile(t *testing.T) {
	var d []time.Duration
	for i := 1; i <= 100; i++ {
		d = append(d, time.Duration(i)*time.Millisecond)
	}
	if percentile(d, .5) != 50*time.Millisecond || percentile(d, .99) != 99*time.Millisecond || percentile(nil, .5) != 0 {
		t.Error("percentile")
	}
}

func TestSendPacesRequestsAndCountsAcceptedEvents(t *testing.T) {
	var got atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body any
		_ = json.NewDecoder(r.Body).Decode(&body)
		n := 1
		if arr, ok := body.([]any); ok {
			n = len(arr)
		}
		got.Add(int64(n))
		ids := make([]string, n)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(202)
		_ = json.NewEncoder(w).Encode(map[string]any{"event_ids": ids, "filtered": 0, "failed": 0})
	}))
	defer srv.Close()
	c := config{ingestURL: srv.URL, source: "wazuh-prod", token: "x", rate: 200, workers: 16, batch: 4, groups: 10, duration: 2 * time.Second}
	start := time.Now()
	res := send(context.Background(), c, "t")
	if took := time.Since(start); took < 1500*time.Millisecond || took > 4*time.Second {
		t.Errorf("200 events/s for 2 s should take about 2 s, took %s", took)
	}
	if res.sent != 400 || res.accepted != 400 || got.Load() != 400 || res.status[202] != 100 {
		t.Errorf("sent=%d accepted=%d server=%d status=%v", res.sent, res.accepted, got.Load(), res.status)
	}
}
