package scheduler

import (
	"context"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/yourorg/soar/internal/domain"
	"github.com/yourorg/soar/internal/queue"
	"github.com/yourorg/soar/internal/store"
	"github.com/yourorg/soar/internal/store/memory"
)

type recordingQ struct {
	mu   sync.Mutex
	msgs []queue.Msg
}

func (q *recordingQ) Publish(_ context.Context, m queue.Msg) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.msgs = append(q.msgs, m)
	return nil
}

func (q *recordingQ) PublishMany(ctx context.Context, ms []queue.Msg) []error {
	errs := make([]error, len(ms))
	for i, m := range ms {
		errs[i] = q.Publish(ctx, m)
	}
	return errs
}

// Schedule triggers fire whether they are stored on the workflow document or still
// inside its definition (the spec 6 DSL form), and a slot runs once.
func TestCronFiresDocumentAndDefinitionTriggers(t *testing.T) {
	ctx := context.Background()
	st := memory.NewStore("soar")
	for _, w := range []domain.Workflow{
		{
			Meta: domain.NewMeta(domain.PrefixWorkflow), Name: "doc-cron", Version: 1, Status: "published",
			Definition: domain.JSON{"nodes": []any{}}, Triggers: []domain.Trigger{{Type: "schedule", Cron: "@every 1m"}},
		},
		{
			Meta: domain.NewMeta(domain.PrefixWorkflow), Name: "def-cron", Version: 1, Status: "published",
			Definition: domain.JSON{"nodes": []any{}, "triggers": []any{map[string]any{"type": "schedule", "cron": "@every 1m"}}},
		},
		{
			Meta: domain.NewMeta(domain.PrefixWorkflow), Name: "draft-cron", Version: 1, Status: "draft",
			Definition: domain.JSON{"nodes": []any{}, "triggers": []any{map[string]any{"type": "schedule", "cron": "@every 1m"}}},
		},
	} {
		if _, err := st.Workflows.Create(ctx, w); err != nil {
			t.Fatal(err)
		}
	}
	q := &recordingQ{}
	now := time.Date(2026, 11, 11, 10, 0, 30, 0, time.UTC)
	c := &Cron{St: st, Q: q, Log: slog.New(slog.NewTextHandler(io.Discard, nil)), Partitions: 2, Now: func() time.Time { return now }}

	if err := c.Tick(ctx); err != nil { // first sight: remember "now", run nothing
		t.Fatal(err)
	}
	now = now.Add(90 * time.Second)
	if err := c.Tick(ctx); err != nil {
		t.Fatal(err)
	}
	if err := c.Tick(ctx); err != nil { // same slot again: nothing new
		t.Fatal(err)
	}
	page, err := st.Executions.List(ctx, store.ListQuery{Limit: 50})
	if err != nil {
		t.Fatal(err)
	}
	names := map[string]int{}
	for _, e := range page.Items {
		names[e.WorkflowName]++
	}
	if names["doc-cron"] != 1 || names["def-cron"] != 1 || names["draft-cron"] != 0 || len(page.Items) != 2 {
		t.Fatalf("one execution per published workflow and slot, got %v", names)
	}
	if len(q.msgs) != 2 {
		t.Fatalf("two start messages, got %d", len(q.msgs))
	}
}
