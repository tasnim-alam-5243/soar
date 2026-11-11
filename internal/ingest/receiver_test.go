package ingest_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/nats-io/nats.go/jetstream"

	"github.com/yourorg/soar/integrations"
	"github.com/yourorg/soar/internal/domain"
	"github.com/yourorg/soar/internal/ingest"
	"github.com/yourorg/soar/internal/ingest/pack"
	"github.com/yourorg/soar/internal/obs"
	"github.com/yourorg/soar/internal/queue"
	"github.com/yourorg/soar/internal/queue/queuetest"
	"github.com/yourorg/soar/internal/security"
	"github.com/yourorg/soar/internal/store"
	"github.com/yourorg/soar/internal/store/memory"
)

type rig struct {
	t   *testing.T
	srv *httptest.Server
	q   *queue.Client
	st  *store.Store
	reg *ingest.Registry
}

func newRig(t *testing.T, opts queue.StreamOptions) *rig {
	t.Helper()
	t.Setenv("SOAR_SOURCE_TOKEN_WAZUH_PROD", "s3cret")
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	st := memory.NewStore("soar")
	b, err := pack.Load(integrations.Packs, "wazuh")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pack.Install(context.Background(), st, b); err != nil {
		t.Fatal(err)
	}
	q, err := queue.Connect([]string{queuetest.Start(t)}, "test")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(q.Close)
	if opts.EventsMaxBytes == 0 {
		opts.EventsMaxBytes = 256 << 20
	}
	if err := q.EnsureStreams(context.Background(), opts); err != nil {
		t.Fatal(err)
	}
	reg := ingest.NewRegistry(st, log, security.ResolveSecret)
	if err := reg.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	(&ingest.Receiver{Reg: reg, Q: q, St: st, Log: log}).Mount(mux)
	srv := httptest.NewServer(obs.Middleware(log, mux))
	t.Cleanup(srv.Close)
	return &rig{t: t, srv: srv, q: q, st: st, reg: reg}
}

func fixture(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile("../../integrations/wazuh/testdata/" + name)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func (r *rig) post(body []byte, headers ...string) (*http.Response, map[string]any) {
	r.t.Helper()
	req, _ := http.NewRequestWithContext(context.Background(), "POST", r.srv.URL+"/ingest/wazuh-prod", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer s3cret")
	req.Header.Set("Content-Type", "application/json")
	for i := 0; i+1 < len(headers); i += 2 {
		if headers[i+1] == "" {
			req.Header.Del(headers[i])
		} else {
			req.Header.Set(headers[i], headers[i+1])
		}
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		r.t.Fatal(err)
	}
	defer resp.Body.Close()
	var m map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&m)
	return resp, m
}

func (r *rig) queued() uint64 {
	st, _ := r.q.JS().Stream(context.Background(), queue.EventsStream)
	info, _ := st.Info(context.Background())
	return info.State.Msgs
}

func (r *rig) dlq() uint64 {
	st, _ := r.q.JS().Stream(context.Background(), queue.DLQStream)
	info, _ := st.Info(context.Background())
	return info.State.Msgs
}

func TestSingleAlertIsPublishedAndAcknowledgedWith202(t *testing.T) {
	r := newRig(t, queue.StreamOptions{})
	resp, body := r.post(fixture(t, "05_sshd_auth_success.json"))
	if resp.StatusCode != 202 || len(body["event_ids"].([]any)) != 1 || body["filtered"] != float64(0) || body["failed"] != float64(0) {
		t.Fatalf("status=%d body=%v", resp.StatusCode, body)
	}
	if resp.Header.Get("X-Request-Id") == "" {
		t.Error("request id missing")
	}
	if r.queued() != 1 {
		t.Fatalf("202 must mean the event is durably queued, queue has %d", r.queued())
	}
	// the message on the stream is a complete normalized event
	cons, _ := r.q.EventsConsumer(context.Background())
	batch, err := cons.Fetch(1, jetstream.FetchMaxWait(time.Second))
	if err != nil {
		t.Fatal(err)
	}
	for m := range batch.Messages() {
		var ev struct {
			EventID  string `json:"event_id"`
			SourceID string `json:"source_id"`
			Alert    struct {
				Title       string `json:"title"`
				ExternalID  string `json:"external_id"`
				Fingerprint string `json:"fingerprint"`
				Severity    int    `json:"severity"`
			} `json:"alert"`
		}
		if err := json.Unmarshal(m.Data(), &ev); err != nil {
			t.Fatal(err)
		}
		if ev.SourceID != "wazuh-prod" || ev.Alert.Title != "sshd: authentication success." || ev.Alert.Severity != 1 || ev.Alert.ExternalID == "" || ev.Alert.Fingerprint == "" {
			t.Errorf("event: %+v", ev)
		}
		if m.Subject() != "events.wazuh-prod" {
			t.Errorf("subject %s", m.Subject())
		}
		_ = m.Ack()
	}
}

func TestSameAlertSentTwiceIsStoredOnce(t *testing.T) {
	r := newRig(t, queue.StreamOptions{})
	for i := 0; i < 3; i++ {
		if resp, _ := r.post(fixture(t, "05_sshd_auth_success.json")); resp.StatusCode != 202 {
			t.Fatalf("attempt %d: %d", i, resp.StatusCode)
		}
	}
	if r.queued() != 1 {
		t.Fatalf("a sender retry must not create a second message (system:external_id dedup), queue has %d", r.queued())
	}
}

func TestBatchCountsFilteredAndFailedAndNeverDropsFailures(t *testing.T) {
	r := newRig(t, queue.StreamOptions{})
	good := json.RawMessage(fixture(t, "05_sshd_auth_success.json"))
	low := json.RawMessage(`{"id":"low.1","timestamp":"2026-10-29T12:48:31.597+0600","rule":{"level":1,"id":"9","description":"low"},"agent":{"id":"1","name":"h"}}`)
	broken := json.RawMessage(`{"id":"bad.1","rule":{"level":5,"id":"9","description":"no timestamp"}}`)
	body, _ := json.Marshal([]json.RawMessage{good, low, broken})
	resp, out := r.post(body)
	if resp.StatusCode != 202 || len(out["event_ids"].([]any)) != 1 || out["filtered"] != float64(1) || out["failed"] != float64(1) {
		t.Fatalf("status=%d body=%v", resp.StatusCode, out)
	}
	if r.queued() != 1 {
		t.Errorf("only the good alert is queued, got %d", r.queued())
	}
	if r.dlq() != 1 {
		t.Errorf("the unmappable record must be dead-lettered, DLQ has %d", r.dlq())
	}
	entries, _ := r.q.DLQList(context.Background(), 5, 0)
	if len(entries) != 1 || !strings.Contains(entries[0].Reason, "map") || entries[0].OriginSubject != "events.wazuh-prod" {
		t.Errorf("dlq entry: %+v", entries)
	}
	raws, _ := r.st.Raw.List(context.Background(), store.ListQuery{Limit: 10})
	if len(raws.Items) != 1 || raws.Items[0].ParseError == "" || raws.Items[0].SourceID != "wazuh-prod" {
		t.Errorf("failure must also be recorded for debugging: %+v", raws.Items)
	}
}

func TestAuthentication(t *testing.T) {
	r := newRig(t, queue.StreamOptions{})
	body := fixture(t, "05_sshd_auth_success.json")
	for name, hdr := range map[string][]string{
		"wrong token":  {"Authorization", "Bearer nope"},
		"no token":     {"Authorization", ""},
		"empty bearer": {"Authorization", "Bearer "},
		"other scheme": {"Authorization", "Basic s3cret"},
	} {
		if resp, _ := r.post(body, hdr...); resp.StatusCode != 401 {
			t.Errorf("%s: want 401, got %d", name, resp.StatusCode)
		}
	}
	if resp, _ := r.post(body, "Authorization", "", "X-Soar-Token", "s3cret"); resp.StatusCode != 202 {
		t.Errorf("X-Soar-Token is an accepted alternative, got %d", resp.StatusCode)
	}
	if r.queued() != 1 {
		t.Errorf("rejected requests must queue nothing; queue has %d", r.queued())
	}
}

func TestRejections(t *testing.T) {
	r := newRig(t, queue.StreamOptions{})
	req, _ := http.NewRequestWithContext(context.Background(), "POST", r.srv.URL+"/ingest/nosuch", strings.NewReader("{}"))
	if resp, _ := http.DefaultClient.Do(req); resp.StatusCode != 404 {
		t.Errorf("unknown source: %d", resp.StatusCode)
	}
	if resp, out := r.post([]byte(`{"broken`)); resp.StatusCode != 400 || out["error"] == nil {
		t.Errorf("malformed JSON: %d %v", resp.StatusCode, out)
	}
	if resp, _ := r.post([]byte(`"just a string"`)); resp.StatusCode != 400 {
		t.Errorf("non-object: %d", resp.StatusCode)
	}
	big := `{"x":"` + strings.Repeat("a", ingest.MaxBodyBytes) + `"}`
	if resp, _ := r.post([]byte(big)); resp.StatusCode != 413 {
		t.Errorf("1 MiB limit: %d", resp.StatusCode)
	}
	many := make([]string, ingest.MaxBatchRecords+1)
	for i := range many {
		many[i] = `{}`
	}
	if resp, _ := r.post([]byte("[" + strings.Join(many, ",") + "]")); resp.StatusCode != 422 {
		t.Errorf("batch limit: %d", resp.StatusCode)
	}
	deep := strings.Repeat(`{"a":`, 70) + `1` + strings.Repeat(`}`, 70)
	if resp, out := r.post([]byte(deep)); resp.StatusCode != 400 || !strings.Contains(fmt.Sprint(out["error"]), "levels deep") {
		t.Errorf("depth limit: %d %v", resp.StatusCode, out)
	}
	if r.queued() != 0 {
		t.Errorf("nothing may be queued after rejections, got %d", r.queued())
	}
}

func TestIdempotencyKeyDeduplicatesAtTheQueue(t *testing.T) {
	r := newRig(t, queue.StreamOptions{})
	a := fixture(t, "04_pam_session_opened.json")
	changed := strings.Replace(string(a), "1793256700.12000", "1793256700.99999", 1) // a different external id
	r.post(a, "Idempotency-Key", "k-1")
	r.post([]byte(changed), "Idempotency-Key", "k-1")
	if r.queued() != 1 {
		t.Fatalf("the same Idempotency-Key must dedupe, queue has %d", r.queued())
	}
}

func TestRateLimitAnswers429WithRetryAfter(t *testing.T) {
	r := newRig(t, queue.StreamOptions{})
	list, _ := r.st.Sources.List(context.Background(), store.ListQuery{Terms: map[string][]string{"name": {"wazuh-prod"}}})
	id := list.Items[0].ID
	_, _, err := r.st.Sources.Update(context.Background(), id, func(s *domain.Source) error {
		s.RateLimit = map[string]any{"rps": 1.0, "burst": 2.0}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := r.reg.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	got429 := false
	for i := 0; i < 6; i++ {
		resp, _ := r.post(fixture(t, "05_sshd_auth_success.json"), "Idempotency-Key", fmt.Sprint("rl-", i))
		if resp.StatusCode == 429 {
			got429 = true
			if resp.Header.Get("Retry-After") == "" {
				t.Error("429 needs Retry-After")
			}
		}
	}
	if !got429 {
		t.Error("expected the limiter to reject some of six rapid requests")
	}
}

func TestFullQueueAnswers503WithRetryAfterAndLosesNothingAccepted(t *testing.T) {
	r := newRig(t, queue.StreamOptions{EventsMaxBytes: 32 << 10})
	accepted, rejected := 0, 0
	for i := 0; i < 200; i++ {
		raw := strings.Replace(string(fixture(t, "05_sshd_auth_success.json")), "1793256701.12020", fmt.Sprint("1793256701.", 50000+i), 1)
		resp, _ := r.post([]byte(raw))
		switch resp.StatusCode {
		case 202:
			accepted++
		case 503:
			rejected++
			if resp.Header.Get("Retry-After") == "" {
				t.Error("503 needs Retry-After")
			}
		default:
			t.Fatalf("unexpected %d", resp.StatusCode)
		}
	}
	if accepted == 0 || rejected == 0 {
		t.Fatalf("expected both outcomes, accepted=%d rejected=%d", accepted, rejected)
	}
	if int(r.queued()) != accepted {
		t.Fatalf("every 202 must be a stored message: 202s=%d stored=%d", accepted, r.queued())
	}
}
