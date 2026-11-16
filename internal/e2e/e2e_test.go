// Package e2e wires the receiver, a real JetStream server and the processor together
// in one process, to catch integration problems that unit tests cannot (the real
// OpenSearch parts are covered by test/integration).
package e2e

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/yourorg/soar/integrations"
	"github.com/yourorg/soar/internal/domain"
	"github.com/yourorg/soar/internal/ingest"
	"github.com/yourorg/soar/internal/ingest/pack"
	"github.com/yourorg/soar/internal/obs"
	"github.com/yourorg/soar/internal/processor"
	"github.com/yourorg/soar/internal/queue"
	"github.com/yourorg/soar/internal/queue/queuetest"
	"github.com/yourorg/soar/internal/security"
	"github.com/yourorg/soar/internal/store"
	"github.com/yourorg/soar/internal/store/memory"
)

// countingBulk applies the processor's operations to maps, like OpenSearch would.
type countingBulk struct {
	st          *store.Store
	mu          sync.Mutex
	alerts      map[string]float64 // alert id -> occurrence_count
	seen        map[string]bool
	observables map[string]bool
}

func (c *countingBulk) Write(_ context.Context, ops []store.BulkOp) ([]store.BulkResult, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]store.BulkResult, len(ops))
	for i, op := range ops {
		switch op.Index {
		case "soar-seen":
			if c.seen[op.ID] {
				out[i] = store.BulkResult{Status: 409, Err: &store.BulkError{Type: "version_conflict_engine_exception"}}
				continue
			}
			c.seen[op.ID] = true
			b, _ := json.Marshal(op.Doc) // the store and the bulk writer share one OpenSearch in production
			var sd domain.Seen
			_ = json.Unmarshal(b, &sd)
			_, _ = c.st.Seen.Put(context.Background(), sd)
		case "soar-alerts":
			c.alerts[op.ID]++
		case "soar-observables":
			c.observables[op.ID] = true
		}
		out[i] = store.BulkResult{Status: 201, ID: op.ID}
	}
	return out, nil
}

func (c *countingBulk) totals() (alerts int, occurrences float64, seen, obs int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, n := range c.alerts {
		occurrences += n
	}
	return len(c.alerts), occurrences, len(c.seen), len(c.observables)
}

func TestIngestToProcessorAt1000PerSecondLosesNothing(t *testing.T) {
	t.Setenv("SOAR_SOURCE_TOKEN_WAZUH_PROD", "tok")
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	st := memory.NewStore("soar")
	b, _ := pack.Load(integrations.Packs, "wazuh")
	if _, err := pack.Install(ctx, st, b); err != nil {
		t.Fatal(err)
	}
	q, err := queue.Connect([]string{queuetest.Start(t)}, "e2e")
	if err != nil {
		t.Fatal(err)
	}
	defer q.Close()
	if err := q.EnsureStreams(ctx, queue.StreamOptions{EventsMaxBytes: 512 << 20}); err != nil {
		t.Fatal(err)
	}
	reg := ingest.NewRegistry(st, log, (&security.SecretStore{}).Resolver())
	if err := reg.Refresh(ctx); err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	(&ingest.Receiver{Reg: reg, Q: q, St: st, Log: log}).Mount(mux)
	srv := httptest.NewServer(obs.Middleware(log, mux))
	defer srv.Close()

	cons, err := q.EventsConsumer(ctx)
	if err != nil {
		t.Fatal(err)
	}
	bulk := &countingBulk{st: st, alerts: map[string]float64{}, seen: map[string]bool{}, observables: map[string]bool{}}
	tr, _ := processor.NewTriggerIndex(st, log)
	proc := &processor.Processor{Cons: cons, Bulk: bulk, St: st, DLQ: q, Triggers: tr, Log: log, Cfg: processor.Config{Workers: 3}}
	done := make(chan struct{})
	go func() { _ = proc.Run(ctx); close(done) }()

	const rate, seconds, groups = 1000, 4, 100
	total := rate * seconds
	var next, accepted, other atomic.Int64
	var wg sync.WaitGroup
	client := &http.Client{Transport: &http.Transport{MaxIdleConnsPerHost: 64}}
	start := time.Now()
	for w := 0; w < 64; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				n := int(next.Add(1) - 1)
				if n >= total {
					return
				}
				if d := time.Until(start.Add(time.Duration(n) * time.Second / rate)); d > 0 {
					time.Sleep(d)
				}
				g := n % groups
				body, _ := json.Marshal(map[string]any{
					"timestamp": time.Now().Format("2006-01-02T15:04:05.000-0700"), "id": fmt.Sprintf("e2e.%d", n),
					"rule":  map[string]any{"level": 5, "id": fmt.Sprint(5000 + g), "description": "d"},
					"agent": map[string]any{"id": "1", "name": "host-e2e"}, "data": map[string]any{"srcip": fmt.Sprintf("10.0.0.%d", g%250)},
				})
				req, _ := http.NewRequestWithContext(ctx, "POST", srv.URL+"/ingest/wazuh-prod", bytes.NewReader(body))
				req.Header.Set("Authorization", "Bearer tok")
				resp, err := client.Do(req)
				if err != nil {
					other.Add(1)
					continue
				}
				resp.Body.Close()
				if resp.StatusCode == 202 {
					accepted.Add(1)
				} else {
					other.Add(1)
				}
			}
		}()
	}
	wg.Wait()
	sendTime := time.Since(start)

	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		stm, _ := q.JS().Stream(ctx, queue.EventsStream)
		info, _ := stm.Info(ctx)
		ci, _ := cons.Info(ctx)
		if info.State.Msgs == 0 && ci.NumAckPending == 0 && ci.NumPending == 0 {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	cancel()
	<-done

	alerts, occ, seen, obsN := bulk.totals()
	t.Logf("sent %d in %s; accepted %d; alert docs %d; occurrences %.0f; seen %d; observables %d", total, sendTime.Round(time.Millisecond), accepted.Load(), alerts, occ, seen, obsN)
	if other.Load() != 0 || accepted.Load() != int64(total) {
		t.Fatalf("every request must be accepted: accepted=%d other=%d", accepted.Load(), other.Load())
	}
	if occ != float64(total) || seen != total {
		t.Fatalf("zero loss and exactly once: occurrences=%.0f seen=%d, want %d", occ, seen, total)
	}
	if alerts < groups || alerts > groups*2 { // the dedup window may split a group at a bucket boundary
		t.Errorf("repeats must collapse into about %d alert documents, got %d", groups, alerts)
	}
	if proc.Stats.Duplicates.Load() != 0 || proc.Stats.DeadLettered.Load() != 0 {
		t.Errorf("duplicates=%d dead=%d", proc.Stats.Duplicates.Load(), proc.Stats.DeadLettered.Load())
	}
}
