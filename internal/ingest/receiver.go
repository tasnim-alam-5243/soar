package ingest

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/yourorg/soar/internal/domain"
	"github.com/yourorg/soar/internal/obs"
	"github.com/yourorg/soar/internal/queue"
	"github.com/yourorg/soar/internal/security"
	"github.com/yourorg/soar/internal/store"
)

// Limits from spec 9.9.
const (
	MaxBodyBytes    = 1 << 20
	MaxBatchRecords = 500
	maxEventBytes   = 900 << 10 // NATS refuses messages above 1 MiB
	maxInlineRaw    = 64 << 10
)

// Queue is the part of the NATS client the receiver and collector need.
type Queue interface {
	PublishMany(ctx context.Context, ms []queue.Msg) []error
	SendToDLQ(ctx context.Context, origin, reason, subject string, data []byte, id string, extra map[string]string) error
}

// EventMessage turns an event into a NATS message. The message ID deduplicates for
// five minutes: events with an external id use system:external_id, so a sender's retry
// (or the same alert arriving by push and by pull) is stored once.
func EventMessage(ev domain.Event, system string, idemKey string) (queue.Msg, error) {
	data, err := json.Marshal(ev)
	if err != nil {
		return queue.Msg{}, err
	}
	if len(data) > maxEventBytes {
		return queue.Msg{}, fmt.Errorf("event is %d bytes, the limit is %d", len(data), maxEventBytes)
	}
	id := ev.EventID
	switch {
	case idemKey != "":
		id = idemKey
	case ev.Alert.ExternalID != "":
		id = system + ":" + ev.Alert.ExternalID
	}
	return queue.Msg{Subject: queue.EventSubject(ev.SourceID), ID: id, Data: data}, nil
}

// SendFailures dead-letters records that could not be processed, so nothing is dropped
// silently (spec 9.9). The durable copy goes to NATS first; the OpenSearch raw record
// is best effort and helps debugging.
func SendFailures(ctx context.Context, q Queue, st *store.Store, log *slog.Logger, source, transport string, fails []Failure) {
	for _, f := range fails {
		payload, _ := json.Marshal(map[string]any{"source": source, "stage": f.Stage, "error": f.Err, "record": f.Record})
		sum := sha256.Sum256(payload)
		id := "ingest-" + hex.EncodeToString(sum[:12])
		if err := q.SendToDLQ(ctx, "ingest", f.Stage+": "+f.Err, queue.EventSubject(source), payload, id, nil); err != nil {
			log.Error("could not dead-letter a failed record", "source", source, "stage", f.Stage, "error", err.Error())
		}
		recordRaw(ctx, st, log, source, transport, payload, f.Stage+": "+f.Err)
	}
}

func recordRaw(ctx context.Context, st *store.Store, log *slog.Logger, source, transport string, body []byte, parseErr string) {
	if st == nil {
		return
	}
	sum := sha256.Sum256(body)
	rec := domain.RawRecord{
		Meta: domain.NewMeta("raw"), SourceID: source, Transport: transport, ReceivedAt: time.Now().UTC(),
		Size: int64(len(body)), SHA256: hex.EncodeToString(sum[:]), ParseError: parseErr,
	}
	if len(body) <= maxInlineRaw {
		rec.Body = string(body)
	}
	cctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	if _, err := st.Raw.Create(cctx, rec); err != nil {
		log.Warn("could not store the raw record", "source", source, "error", err.Error())
	}
}

// Receiver is the HTTP push endpoint: authenticate, parse, filter, map, publish, answer 202.
// It never waits for OpenSearch on the request path.
type Receiver struct {
	Reg *Registry
	Q   Queue
	St  *store.Store
	Log *slog.Logger
}

type ingestReply struct {
	EventIDs []string `json:"event_ids"`
	Filtered int      `json:"filtered"`
	Failed   int      `json:"failed"`
}

func reply(w http.ResponseWriter, r *http.Request, status int, v any, retryAfter int) {
	if retryAfter > 0 {
		w.Header().Set("Retry-After", strconv.Itoa(retryAfter))
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func errReply(w http.ResponseWriter, r *http.Request, status int, msg string, retryAfter int) {
	reply(w, r, status, map[string]string{"error": msg, "request_id": obs.RequestID(r.Context())}, retryAfter)
}

// Mount registers POST /ingest/{source}.
func (rc *Receiver) Mount(mux *http.ServeMux) {
	mux.HandleFunc("POST /ingest/{source}", rc.handle)
}

func (rc *Receiver) handle(w http.ResponseWriter, r *http.Request) {
	started := time.Now()
	name := r.PathValue("source")
	rt, ok := rc.Reg.Get(name)
	if !ok || !IsPush(rt.Source) {
		errReply(w, r, http.StatusNotFound, "unknown or disabled source", 0)
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, MaxBodyBytes))
	if err != nil {
		errReply(w, r, http.StatusRequestEntityTooLarge, fmt.Sprintf("body exceeds %d bytes", MaxBodyBytes), 0)
		return
	}
	if err := rt.Auth.Authenticate(r, body); err != nil {
		errReply(w, r, http.StatusUnauthorized, "unauthorized", 0)
		return
	}

	if d := security.JSONDepth(body); d > security.MaxJSONDepth {
		errReply(w, r, http.StatusBadRequest, fmt.Sprintf("body nests %d levels deep; the limit is %d", d, security.MaxJSONDepth), 0)
		return
	}
	recs, err := rt.Pipeline.Parser.Parse(body)
	if err != nil {
		rt.Pipeline.Stats.Failed.Add(1)
		recordRaw(r.Context(), rc.St, rc.Log, name, "webhook", body, "parse: "+err.Error())
		errReply(w, r, http.StatusBadRequest, "cannot parse body: "+err.Error(), 0)
		return
	}
	if len(recs) > MaxBatchRecords {
		errReply(w, r, http.StatusUnprocessableEntity, fmt.Sprintf("a request may carry at most %d records, got %d", MaxBatchRecords, len(recs)), 0)
		return
	}
	if !rt.Limiter.Allow(len(recs)) {
		errReply(w, r, http.StatusTooManyRequests, "rate limit exceeded for this source", 1)
		return
	}

	out := rt.Pipeline.ProcessRecords(recs)
	idem := r.Header.Get("Idempotency-Key")
	msgs := make([]queue.Msg, 0, len(out.Events))
	ids := make([]string, 0, len(out.Events))
	for i, ev := range out.Events {
		key := ""
		if idem != "" {
			key = idem
			if len(out.Events) > 1 {
				key = idem + "-" + strconv.Itoa(i)
			}
		}
		m, err := EventMessage(ev, rt.Pipeline.System, key)
		if err != nil {
			out.Failures = append(out.Failures, Failure{Record: ev.Alert.Ext, Stage: "publish", Err: err.Error()})
			continue
		}
		msgs = append(msgs, m)
		ids = append(ids, ev.EventID)
	}

	for _, perr := range rc.Q.PublishMany(r.Context(), msgs) {
		switch {
		case perr == nil:
		case errors.Is(perr, queue.ErrQueueFull):
			obs.SourceEvents.WithLabelValues(name, "rejected").Add(float64(len(msgs)))
			errReply(w, r, http.StatusServiceUnavailable, "the event queue is full; retry later", 5)
			return
		default:
			obs.SourceEvents.WithLabelValues(name, "rejected").Add(float64(len(msgs)))
			errReply(w, r, http.StatusServiceUnavailable, "the event queue is unavailable; retry later", 5)
			return
		}
	}
	if len(out.Failures) > 0 {
		SendFailures(r.Context(), rc.Q, rc.St, rc.Log, name, "webhook", out.Failures)
	}
	obs.IngestEvents.WithLabelValues(name).Add(float64(len(ids)))
	obs.SourceEvents.WithLabelValues(name, "accepted").Add(float64(len(ids)))
	obs.SourceEvents.WithLabelValues(name, "filtered").Add(float64(out.Filtered))
	obs.SourceEvents.WithLabelValues(name, "parse_error").Add(float64(len(out.Failures)))
	obs.IngestLatency.WithLabelValues(name).Observe(time.Since(started).Seconds())
	reply(w, r, http.StatusAccepted, ingestReply{EventIDs: nonNil(ids), Filtered: out.Filtered, Failed: len(out.Failures)}, 0)
}

func nonNil(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}
