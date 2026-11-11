package scheduler

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/yourorg/soar/internal/domain"
	"github.com/yourorg/soar/internal/engine"
	"github.com/yourorg/soar/internal/orchestrator"
	"github.com/yourorg/soar/internal/queue"
	"github.com/yourorg/soar/internal/store"
)

// Publisher is the part of the queue client the scheduler needs.
type Publisher interface {
	Publish(ctx context.Context, m queue.Msg) error
	PublishMany(ctx context.Context, ms []queue.Msg) []error
}

// Jobs are the scheduler's periodic tasks. Every job is idempotent, so a second
// scheduler that briefly overlaps after a lease takeover causes no harm.
type Jobs struct {
	St         *store.Store
	Q          Publisher
	Orch       *orchestrator.Orchestrator // Flush for the outbox sweeper
	Log        *slog.Logger
	Partitions int
	Now        func() time.Time
}

func (j *Jobs) now() time.Time {
	if j.Now != nil {
		return j.Now()
	}
	return time.Now().UTC()
}

// FireDueTimers publishes TimerFired for every due timer (spec 8.6) and marks it
// fired. Nats-Msg-Id is the timer id, so a crash between publish and mark is harmless.
func (j *Jobs) FireDueTimers(ctx context.Context, limit int) (int, error) {
	now := j.now()
	page, err := j.St.Timers.List(ctx, store.ListQuery{
		Terms:  map[string][]string{"fired": {"false"}},
		Ranges: []store.Range{{Field: "due_at", Lte: now.Format(time.RFC3339Nano)}}, Sort: "due_at", Limit: limit,
	})
	if err != nil {
		return 0, err
	}
	if len(page.Items) == 0 {
		return 0, nil
	}
	msgs := make([]queue.Msg, len(page.Items))
	for i, t := range page.Items {
		if msgs[i], err = orchestrator.TimerMessage(t); err != nil {
			return 0, err
		}
	}
	errs := j.Q.PublishMany(ctx, msgs)
	fired := 0
	for i, t := range page.Items {
		if errs[i] != nil {
			j.Log.Warn("could not fire timer; will retry", "timer", t.ID, "error", errs[i].Error())
			continue
		}
		at := j.now()
		if _, _, err := j.St.Timers.Update(ctx, t.ID, func(x *domain.Timer) error { x.Fired, x.FiredAt = true, &at; return nil }); err != nil {
			j.Log.Warn("timer fired but not marked; it will be re-fired and deduplicated", "timer", t.ID, "error", err.Error())
			continue
		}
		fired++
	}
	return fired, nil
}

// OldestTimerLag reports now - due_at of the oldest unfired timer (spec 8.9 "timer lag").
func (j *Jobs) OldestTimerLag(ctx context.Context) (time.Duration, error) {
	page, err := j.St.Timers.List(ctx, store.ListQuery{Terms: map[string][]string{"fired": {"false"}}, Sort: "due_at", Limit: 1})
	if err != nil || len(page.Items) == 0 {
		return 0, err
	}
	lag := j.now().Sub(page.Items[0].DueAt)
	if lag < 0 {
		lag = 0
	}
	return lag, nil
}

// SweepOutbox completes outbox entries left unpublished for more than olderThan (spec 8.4 step 5).
func (j *Jobs) SweepOutbox(ctx context.Context, olderThan time.Duration, limit int) (int, error) {
	page, err := j.St.Executions.List(ctx, store.ListQuery{
		Terms:  map[string][]string{"outbox_pending": {"true"}},
		Ranges: []store.Range{{Field: "updated_at", Lte: j.now().Add(-olderThan).Format(time.RFC3339Nano)}}, Sort: "updated_at", Limit: limit,
	})
	if err != nil {
		return 0, err
	}
	n := 0
	for _, exe := range page.Items {
		cur, ver, err := j.St.Executions.Get(ctx, exe.ID) // real-time read: the search may be stale
		if err != nil || !cur.OutboxPending {
			continue
		}
		if err := j.Orch.Flush(ctx, cur, ver); err != nil {
			j.Log.Warn("outbox sweep incomplete", "execution_id", exe.ID, "error", err.Error())
			continue
		}
		n++
	}
	return n, nil
}

// SweepQueued republishes the start message of executions still queued after
// olderThan (a lost start message must not leave an execution stuck).
func (j *Jobs) SweepQueued(ctx context.Context, olderThan time.Duration, limit int) (int, error) {
	page, err := j.St.Executions.List(ctx, store.ListQuery{
		Terms:  map[string][]string{"status": {engine.StatusQueued}},
		Ranges: []store.Range{{Field: "created_at", Lte: j.now().Add(-olderThan).Format(time.RFC3339Nano)}}, Sort: "created_at", Limit: limit,
	})
	if err != nil {
		return 0, err
	}
	n := 0
	for _, exe := range page.Items {
		m, err := orchestrator.StartMessage(exe, StartFromContext(exe))
		if err != nil {
			continue
		}
		m.ID = fmt.Sprintf("restart:%s:%d", exe.ID, j.now().Unix()/60) // outside the original dedup window
		if err := j.Q.Publish(ctx, m); err != nil {
			return n, err
		}
		n++
	}
	return n, nil
}

// StartFromContext rebuilds a start payload from what was stored with a queued execution.
func StartFromContext(exe domain.Execution) orchestrator.StartPayload {
	st, err := engine.StateFromJSON(exe.Context)
	if err != nil || st == nil {
		return orchestrator.StartPayload{DryRun: exe.DryRun}
	}
	return orchestrator.StartPayload{Inputs: st.Inputs, Alert: st.Alert, DryRun: exe.DryRun || st.DryRun, Parent: st.Parent}
}

// ---- leader election ----

// Lease is a leader lease stored in soar-cursors (spec 2: the scheduler is leader-elected).
type Lease struct {
	St    *store.Store
	Name  string
	Owner string
	TTL   time.Duration
	Now   func() time.Time
}

func (l *Lease) now() time.Time {
	if l.Now != nil {
		return l.Now()
	}
	return time.Now().UTC()
}

// Acquire takes or renews the lease; false means another replica leads.
func (l *Lease) Acquire(ctx context.Context) (bool, error) {
	id := domain.DeterministicID("cur", "lock:"+l.Name)
	now := l.now()
	exp := now.Add(l.TTL)
	cur, v, err := l.St.Cursors.Get(ctx, id)
	if errors.Is(err, store.ErrNotFound) {
		doc := domain.Cursor{Meta: domain.Meta{ID: id}, SourceID: "lock:" + l.Name, LeaseOwner: l.Owner, LeaseExpiresAt: &exp}
		if _, err := l.St.Cursors.Create(ctx, doc); err != nil {
			if errors.Is(err, store.ErrAlreadyExists) {
				return false, nil
			}
			return false, err
		}
		return true, nil
	}
	if err != nil {
		return false, err
	}
	if cur.LeaseOwner != l.Owner && cur.LeaseExpiresAt != nil && cur.LeaseExpiresAt.After(now) {
		return false, nil
	}
	cur.LeaseOwner, cur.LeaseExpiresAt = l.Owner, &exp
	if _, err := l.St.Cursors.Save(ctx, cur, v); err != nil {
		if errors.Is(err, store.ErrVersionConflict) {
			return false, nil
		}
		return false, err
	}
	return true, nil
}

// Release gives the lease up (on shutdown) so another replica takes over at once.
func (l *Lease) Release(ctx context.Context) {
	id := domain.DeterministicID("cur", "lock:"+l.Name)
	_, _, _ = l.St.Cursors.Update(ctx, id, func(c *domain.Cursor) error {
		if c.LeaseOwner == l.Owner {
			past := l.now().Add(-time.Second)
			c.LeaseExpiresAt = &past
		}
		return nil
	})
}
