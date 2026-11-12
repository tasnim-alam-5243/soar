package opensearch

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"
)

// A large purge runs as a background task that is polled, never as one request that the
// 60 s HTTP timeout could cut off (the scheduler logged exactly that after a load test).
func TestPurgeRunsAsAPolledBackgroundTask(t *testing.T) {
	old := TaskPollInterval
	TaskPollInterval = time.Millisecond
	t.Cleanup(func() { TaskPollInterval = old })

	var mu sync.Mutex
	polls := map[string]int{}
	c, log := stub(t, func(r recorded) (int, string) {
		mu.Lock()
		defer mu.Unlock()
		switch {
		case r.Method == "POST" && strings.HasSuffix(r.Path, "/_delete_by_query"):
			return 200, `{"task":"node:` + strings.TrimPrefix(strings.Split(r.Path, "/")[1], "soar-") + `"}`
		case strings.HasPrefix(r.Path, "/_tasks/"):
			polls[r.Path]++
			if polls[r.Path] < 3 {
				return 200, `{"completed":false,"task":{"status":{"deleted":1}}}`
			}
			return 200, `{"completed":true,"response":{"deleted":7,"failures":[]}}`
		}
		return 404, `{}`
	})
	n, err := PurgeExpired(context.Background(), c, "soar", time.Now(), 24*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if n != 21 { // seen, idempotency, subscriptions
		t.Errorf("deleted = %d, want 7 per index", n)
	}
	for _, r := range *log {
		if r.Method == "POST" && !strings.Contains(r.Query, "wait_for_completion=false") {
			t.Errorf("the delete must start as a background task: %s?%s", r.Path, r.Query)
		}
	}
	if polls["/_tasks/node:seen"] != 3 || polls["/_tasks/node:idempotency"] != 3 {
		t.Errorf("each task must be polled until it completes: %v", polls)
	}
}

func TestDeleteByQueryReportsTaskFailures(t *testing.T) {
	c, _ := stub(t, func(r recorded) (int, string) {
		if r.Method == "POST" {
			return 200, `{"task":"node:1"}`
		}
		return 200, `{"completed":true,"response":{"deleted":2,"failures":[{"cause":{"type":"es_rejected_execution_exception"}}]}}`
	})
	n, err := DeleteByQuery(context.Background(), c, "soar-cases", map[string]any{"match_all": map[string]any{}})
	if err == nil || !strings.Contains(err.Error(), "es_rejected_execution_exception") || n != 2 {
		t.Fatalf("failures inside the task must surface: n=%d err=%v", n, err)
	}
}

func TestDeleteByQueryStopsWaitingWhenTheContextEnds(t *testing.T) {
	old := TaskPollInterval
	TaskPollInterval = 5 * time.Millisecond
	t.Cleanup(func() { TaskPollInterval = old })
	c, _ := stub(t, func(r recorded) (int, string) {
		if r.Method == "POST" {
			return 200, `{"task":"node:1"}`
		}
		return 200, `{"completed":false}`
	})
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if _, err := DeleteByQuery(ctx, c, "soar-cases", map[string]any{"match_all": map[string]any{}}); err == nil || !strings.Contains(err.Error(), "still running") {
		t.Fatalf("got %v", err)
	}
}
