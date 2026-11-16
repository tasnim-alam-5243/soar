package processor

import (
	"context"
	"encoding/json"
	"log/slog"
	"sync"
	"time"

	"github.com/yourorg/soar/internal/domain"
	"github.com/yourorg/soar/internal/expr"
	"github.com/yourorg/soar/internal/store"
)

// Match is a workflow that should run for an alert.
type Match struct {
	WorkflowID   string
	WorkflowName string
	Version      int
	Checksum     string
}

type compiled struct {
	m     Match
	kind  string
	match *expr.Program // nil = every alert of that kind
}

// TriggerIndex holds the compiled alert triggers of the latest published version of
// every workflow. It is refreshed periodically, so publishing a workflow takes effect
// within seconds without restarting the processor.
type TriggerIndex struct {
	st  *store.Store
	log *slog.Logger
	eng *expr.Engine
	mu  sync.RWMutex
	by  []compiled
	byN map[string]Match
}

// NewTriggerIndex builds an empty index.
func NewTriggerIndex(st *store.Store, log *slog.Logger) (*TriggerIndex, error) {
	eng, err := expr.New(nil, "alert")
	if err != nil {
		return nil, err
	}
	return &TriggerIndex{st: st, log: log, eng: eng, byN: map[string]Match{}}, nil
}

// Refresh reloads published workflows.
func (t *TriggerIndex) Refresh(ctx context.Context) error {
	latest := map[string]domain.Workflow{}
	cursor := ""
	for {
		p, err := t.st.Workflows.List(ctx, store.ListQuery{Terms: map[string][]string{"status": {"published"}}, Limit: 200, After: cursor})
		if err != nil {
			return err
		}
		for _, w := range p.Items {
			if cur, ok := latest[w.Name]; !ok || w.Version > cur.Version {
				latest[w.Name] = w
			}
		}
		if p.NextCursor == "" {
			break
		}
		cursor = p.NextCursor
	}
	var list []compiled
	names := map[string]Match{}
	for _, w := range latest {
		m := Match{WorkflowID: w.ID, WorkflowName: w.Name, Version: w.Version, Checksum: w.Checksum}
		names[w.Name] = m
		for _, tr := range w.AllTriggers() { // document triggers and any written inside the definition
			if tr.Type != "alert" && tr.Type != "event" {
				continue
			}
			c := compiled{m: m, kind: tr.Kind}
			if tr.Match != "" {
				prg, err := t.eng.Compile(tr.Match)
				if err != nil {
					t.log.Error("workflow trigger does not compile; it will not fire", "workflow", w.Name, "error", err.Error())
					continue
				}
				c.match = prg
			}
			list = append(list, c)
		}
	}
	t.mu.Lock()
	t.by, t.byN = list, names
	t.mu.Unlock()
	return nil
}

// Run refreshes every interval until ctx ends.
func (t *TriggerIndex) Run(ctx context.Context, every time.Duration) {
	tick := time.NewTicker(every)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			if err := t.Refresh(ctx); err != nil {
				t.log.Warn("could not refresh workflow triggers; keeping the previous set", "error", err.Error())
			}
		}
	}
}

// For returns the workflows to start for an event: matching triggers plus the workflow
// named by a route filter. Each workflow appears once.
func (t *TriggerIndex) For(ev domain.Event) []Match {
	t.mu.RLock()
	list, byN := t.by, t.byN
	t.mu.RUnlock()
	seen := map[string]bool{}
	var out []Match
	var alertVars map[string]any
	for _, c := range list {
		if c.kind != "" && c.kind != ev.Alert.Kind {
			continue
		}
		if c.match != nil {
			if alertVars == nil {
				alertVars = map[string]any{"alert": toMap(ev.Alert)}
			}
			ok, err := c.match.Bool(alertVars)
			if err != nil {
				t.log.Debug("trigger expression failed", "workflow", c.m.WorkflowName, "error", err.Error())
				continue
			}
			if !ok {
				continue
			}
		}
		if !seen[c.m.WorkflowName] {
			seen[c.m.WorkflowName] = true
			out = append(out, c.m)
		}
	}
	if ev.Route != "" {
		if m, ok := byN[ev.Route]; ok && !seen[m.WorkflowName] {
			out = append(out, m)
		} else if !ok {
			t.log.Warn("route filter names a workflow that is not published", "workflow", ev.Route, "event", ev.EventID)
		}
	}
	return out
}

func toMap(v any) map[string]any {
	b, _ := json.Marshal(v)
	var m map[string]any
	_ = json.Unmarshal(b, &m)
	if m == nil {
		m = map[string]any{}
	}
	return m
}
