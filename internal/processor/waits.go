package processor

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"

	"github.com/yourorg/soar/internal/domain"
	"github.com/yourorg/soar/internal/engine"
	"github.com/yourorg/soar/internal/expr"
	"github.com/yourorg/soar/internal/orchestrator"
	"github.com/yourorg/soar/internal/queue"
	"github.com/yourorg/soar/internal/store"
)

// WaitIndex holds the open event subscriptions (wait nodes with event, Step 10) with
// their compiled matches. It is refreshed periodically from soar-subscriptions, so a
// wait started anywhere, also before a restart, is matched within seconds.
type WaitIndex struct {
	st  *store.Store
	log *slog.Logger
	eng *expr.Engine

	mu    sync.RWMutex
	subs  []waitSub
	progs map[string]*expr.Program // compiled matches by source, kept across refreshes
}

type waitSub struct {
	sub  domain.Subscription
	prg  *expr.Program
	with map[string]any
}

// MaxOpenWaits bounds how many open subscriptions one refresh loads.
const MaxOpenWaits = 10000

// NewWaitIndex builds an empty index.
func NewWaitIndex(st *store.Store, log *slog.Logger) (*WaitIndex, error) {
	eng, err := engine.NewEventCEL()
	if err != nil {
		return nil, err
	}
	return &WaitIndex{st: st, log: log, eng: eng, progs: map[string]*expr.Program{}}, nil
}

// Refresh reloads the open subscriptions whose wait has not timed out.
func (w *WaitIndex) Refresh(ctx context.Context) error {
	var list []waitSub
	cursor := ""
	now := time.Now().UTC()
	for len(list) < MaxOpenWaits {
		p, err := w.st.Subscriptions.List(ctx, store.ListQuery{Terms: map[string][]string{"status": {"open"}}, Limit: 500, After: cursor})
		if err != nil {
			return err
		}
		for _, s := range p.Items {
			if s.DueAt.Before(now) {
				continue // its timeout timer fails or continues the node
			}
			prg, ok := w.progs[s.Match]
			if !ok {
				var err error
				if prg, err = w.eng.Compile(s.Match); err != nil {
					w.log.Error("event wait does not compile; it will only time out", "execution_id", s.ExecutionID, "node_id", s.NodeID, "error", err.Error())
					continue
				}
				w.progs[s.Match] = prg
			}
			with := map[string]any(s.With)
			if with == nil {
				with = map[string]any{}
			}
			list = append(list, waitSub{sub: s, prg: prg, with: with})
		}
		if p.NextCursor == "" {
			break
		}
		cursor = p.NextCursor
	}
	if len(list) >= MaxOpenWaits {
		w.log.Warn("more open event waits than one refresh loads; the oldest are matched first", "limit", MaxOpenWaits)
	}
	w.mu.Lock()
	w.subs = list
	w.mu.Unlock()
	return nil
}

// Run refreshes every interval until ctx ends.
func (w *WaitIndex) Run(ctx context.Context, every time.Duration) {
	tick := time.NewTicker(every)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			if err := w.Refresh(ctx); err != nil {
				w.log.Warn("could not refresh event waits; keeping the previous set", "error", err.Error())
			}
		}
	}
}

// Match returns the open subscriptions an event satisfies. event is the stored alert
// or event as a map (the `event` variable of the match).
func (w *WaitIndex) Match(event map[string]any) []domain.Subscription {
	w.mu.RLock()
	list := w.subs
	w.mu.RUnlock()
	var out []domain.Subscription
	for _, s := range list {
		ok, err := s.prg.Bool(map[string]any{"event": event, "wait": s.with})
		if err != nil || !ok {
			continue // a field missing from this event is simply no match
		}
		out = append(out, s.sub)
	}
	return out
}

// Forget drops a subscription from the in-memory set once it matched, so the next
// events of the same batch do not match it again before the next refresh.
func (w *WaitIndex) Forget(id string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	for i, s := range w.subs {
		if s.sub.ID == id {
			w.subs = append(w.subs[:i:i], w.subs[i+1:]...)
			return
		}
	}
}

// resumeWaits matches the events of a batch that were stored (not duplicates, not
// failed) against the open event waits. A match is published to its execution first
// and only then marked matched; if the publish fails the event is retried (nak), so a
// match is never lost, and the engine ignores a second delivery of the same match.
func (p *Processor) resumeWaits(ctx context.Context, items []*item) {
	if p.Waits == nil || p.Starts == nil {
		return
	}
	type hit struct {
		it  *item
		sub domain.Subscription
	}
	var hits []hit
	var msgs []queue.Msg
	taken := map[string]bool{} // one event per subscription per batch
	for _, it := range items {
		if it.done || it.dup || it.retryErr != nil {
			continue
		}
		ev := toMap(it.ev.Alert)
		ev["id"], ev["event_id"] = it.alertID, it.ev.EventID
		for _, s := range p.Waits.Match(ev) {
			if taken[s.ID] {
				continue
			}
			m, err := orchestrator.EventMessage(s, ev)
			if err != nil {
				p.Log.Error("cannot build the event message for a wait", "subscription_id", s.ID, "error", err.Error())
				continue
			}
			taken[s.ID] = true
			hits = append(hits, hit{it, s})
			msgs = append(msgs, m)
		}
	}
	if len(msgs) == 0 {
		return
	}
	errs := p.Starts.PublishMany(ctx, msgs)
	now := time.Now().UTC()
	for i, h := range hits {
		if errs[i] != nil {
			p.Log.Warn("could not resume a waiting execution; retrying the event", "subscription_id", h.sub.ID, "error", errs[i].Error())
			if h.it.retryErr == nil {
				h.it.retryErr = errs[i]
			}
			continue
		}
		p.Waits.Forget(h.sub.ID)
		p.Stats.WaitsResumed.Add(1)
		if _, _, err := p.St.Subscriptions.Update(ctx, h.sub.ID, func(s *domain.Subscription) error {
			if s.Status == "open" {
				s.Status, s.MatchedEventID, s.MatchedAt = "matched", h.it.ev.EventID, &now
			}
			return nil
		}); err != nil && !errors.Is(err, store.ErrNotFound) {
			p.Log.Warn("could not mark a wait as matched; the engine ignores a repeat", "subscription_id", h.sub.ID, "error", err.Error())
		}
	}
}
