package scheduler

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/yourorg/soar/internal/domain"
	"github.com/yourorg/soar/internal/engine"
	"github.com/yourorg/soar/internal/orchestrator"
	"github.com/yourorg/soar/internal/store"
)

// Cron starts workflows with schedule triggers (spec section 1: "schedule" sources of
// executions). Each workflow name keeps its last fired slot in soar-cursors. When the
// scheduler was down, only the most recent missed slot runs (no backlog storm), and
// the execution id is derived from the slot so a slot never runs twice.
type Cron struct {
	St         *store.Store
	Q          Publisher
	Log        *slog.Logger
	Partitions int
	Now        func() time.Time
}

func (c *Cron) now() time.Time {
	if c.Now != nil {
		return c.Now()
	}
	return time.Now().UTC()
}

// Tick checks every published workflow's schedule triggers.
func (c *Cron) Tick(ctx context.Context) error {
	latest := map[string]domain.Workflow{}
	cursor := ""
	for {
		page, err := c.St.Workflows.List(ctx, store.ListQuery{Terms: map[string][]string{"status": {"published"}}, Limit: 200, After: cursor})
		if err != nil {
			return err
		}
		for _, w := range page.Items {
			if cur, ok := latest[w.Name]; !ok || w.Version > cur.Version {
				latest[w.Name] = w
			}
		}
		if page.NextCursor == "" {
			break
		}
		cursor = page.NextCursor
	}
	now := c.now()
	for _, w := range latest {
		for _, t := range w.AllTriggers() { // document triggers and any written inside the definition
			if t.Type != "schedule" || t.Cron == "" {
				continue
			}
			cr, err := engine.ParseCron(t.Cron)
			if err != nil {
				c.Log.Warn("invalid cron in a published workflow", "workflow", w.Name, "cron", t.Cron, "error", err.Error())
				continue
			}
			if err := c.fire(ctx, w, t.Cron, cr, now); err != nil {
				c.Log.Warn("cron trigger failed", "workflow", w.Name, "error", err.Error())
			}
		}
	}
	return nil
}

func (c *Cron) fire(ctx context.Context, w domain.Workflow, spec string, cr *engine.Cron, now time.Time) error {
	key := "cron:" + w.Name + ":" + spec
	id := domain.DeterministicID("cur", key)
	cur, ver, err := c.St.Cursors.Get(ctx, id)
	if errors.Is(err, store.ErrNotFound) {
		// first sight: start counting from now, do not run past slots
		doc := domain.Cursor{Meta: domain.Meta{ID: id}, SourceID: key, Cursor: now.Format(time.RFC3339)}
		_, err := c.St.Cursors.Create(ctx, doc)
		if errors.Is(err, store.ErrAlreadyExists) {
			return nil
		}
		return err
	}
	if err != nil {
		return err
	}
	last, err := time.Parse(time.RFC3339, cur.Cursor)
	if err != nil {
		last = now
	}
	var slot time.Time
	for next := cr.Next(last); !next.IsZero() && !next.After(now); next = cr.Next(next) {
		slot = next // keep only the most recent due slot
	}
	if slot.IsZero() {
		return nil
	}
	exeID := domain.DeterministicID(domain.PrefixExecution, "cron", w.Name, slot.Unix())
	exe := orchestrator.NewExecution(w, exeID, domain.ExecutionTrigger{Type: "schedule", ScheduledFor: &slot}, c.Partitions, false)
	st := &engine.State{Status: engine.StatusQueued, Inputs: map[string]any{"scheduled_for": slot.Format(time.RFC3339)}}
	exe.Context = st.JSON()
	if _, err := c.St.Executions.Create(ctx, exe); err != nil && !errors.Is(err, store.ErrAlreadyExists) {
		return err
	}
	m, err := orchestrator.StartMessage(exe, orchestrator.StartPayload{Inputs: st.Inputs})
	if err != nil {
		return err
	}
	if err := c.Q.Publish(ctx, m); err != nil {
		return err // the cursor stays: the next tick retries this slot (same execution id)
	}
	cur.Cursor = slot.Format(time.RFC3339)
	if _, err := c.St.Cursors.Save(ctx, cur, ver); err != nil && !errors.Is(err, store.ErrVersionConflict) {
		return err
	}
	c.Log.Info("scheduled workflow started", "workflow", w.Name, "slot", slot, "execution_id", exeID)
	return nil
}
