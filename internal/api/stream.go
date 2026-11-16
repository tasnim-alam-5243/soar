package api

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/yourorg/soar/internal/blob"
	"github.com/yourorg/soar/internal/security"
)

// UIEvent is a change notification for the web UI (GET /stream, server-sent events).
type UIEvent struct {
	Topic string         `json:"topic"` // executions | cases | approvals | alerts
	ID    string         `json:"id"`
	Data  map[string]any `json:"data,omitempty"`
	At    time.Time      `json:"at"`
}

// Broker fans UI events out to connected stream clients. Each API replica feeds its
// broker from NATS (core subjects ui.<topic>), so a client sees changes made anywhere.
type Broker struct {
	mu   sync.Mutex
	subs map[chan UIEvent]map[string]bool
}

// NewBroker returns an empty broker.
func NewBroker() *Broker { return &Broker{subs: map[chan UIEvent]map[string]bool{}} }

// Publish delivers an event to interested subscribers; slow clients miss events
// rather than block the publisher (the UI refetches on reconnect).
func (b *Broker) Publish(ev UIEvent) {
	b.mu.Lock()
	defer b.mu.Unlock()
	for ch, topics := range b.subs {
		if len(topics) > 0 && !topics[ev.Topic] {
			continue
		}
		select {
		case ch <- ev:
		default:
		}
	}
}

func (b *Broker) subscribe(topics []string) chan UIEvent {
	ch := make(chan UIEvent, 64)
	set := map[string]bool{}
	for _, t := range topics {
		if t != "" {
			set[t] = true
		}
	}
	b.mu.Lock()
	b.subs[ch] = set
	b.mu.Unlock()
	return ch
}

func (b *Broker) unsubscribe(ch chan UIEvent) {
	b.mu.Lock()
	delete(b.subs, ch)
	b.mu.Unlock()
}

// Events exposes the broker so the API role can feed it from NATS.
func (s *Server) Events() *Broker { return s.events }

func (s *Server) stream(w http.ResponseWriter, r *http.Request) error {
	fl, ok := w.(http.Flusher)
	if !ok {
		return newProblem(http.StatusInternalServerError, "no-streaming", "Streaming unsupported", "the server cannot stream responses")
	}
	topics := strings.Split(r.URL.Query().Get("topics"), ",")
	ch := s.events.subscribe(topics)
	defer s.events.unsubscribe(ch)
	// The server's WriteTimeout (30 s) would cut every stream; instead each write gets its
	// own deadline, so a live client stays connected and a dead one is dropped within a minute.
	rc := http.NewResponseController(w)
	extend := func() { _ = rc.SetWriteDeadline(time.Now().Add(streamWriteWindow)) }
	extend()
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	fmt.Fprintf(w, "retry: 3000\n: connected\n\n")
	fl.Flush()
	tick := time.NewTicker(streamKeepAlive)
	defer tick.Stop()
	for {
		select {
		case <-r.Context().Done():
			return nil
		case <-tick.C:
			extend()
			fmt.Fprint(w, ": keep-alive\n\n")
			fl.Flush()
		case ev := <-ch:
			extend()
			b, _ := json.Marshal(ev)
			fmt.Fprintf(w, "event: %s\ndata: %s\n\n", ev.Topic, b)
			fl.Flush()
		}
	}
}

// Keep-alives keep proxies and the per-write deadline from closing an idle stream.
var (
	streamKeepAlive   = 15 * time.Second
	streamWriteWindow = time.Minute
)

func slogActor(p security.Principal) slog.Attr { return slog.String("actor", p.Name) }

func sortStrings(l []string) { sort.Strings(l) }

// WithBlob enables evidence uploads and downloads.
func (s *Server) WithBlob(b blob.Blob) *Server { s.blob = b; return s }

// MountCORS answers preflight requests for the configured web origins (the UI is
// normally served from the same origin through nginx, which needs no CORS at all).
func (s *Server) MountCORS(mux *http.ServeMux, origins []string) {
	if len(origins) == 0 {
		return
	}
	allowed := map[string]bool{}
	for _, o := range origins {
		allowed[o] = true
	}
	mux.HandleFunc("OPTIONS "+Prefix+"/", func(w http.ResponseWriter, r *http.Request) {
		if o := r.Header.Get("Origin"); allowed[o] {
			w.Header().Set("Access-Control-Allow-Origin", o)
			w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PUT, PATCH, DELETE")
			w.Header().Set("Access-Control-Allow-Headers", "Authorization, Content-Type, If-Match, Idempotency-Key, X-Request-ID")
			w.Header().Set("Access-Control-Max-Age", "600")
		}
		w.WriteHeader(http.StatusNoContent)
	})
}
