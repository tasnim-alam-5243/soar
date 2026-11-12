package opensearch

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"time"
)

// PurgeExpired deletes documents of the TTL classes (retention.yaml "ttl"): soar-seen
// keys older than seenTTL and idempotency records past their expires_at. It returns how
// many documents were removed. Safe to run from several processes at once.
func PurgeExpired(ctx context.Context, c *Client, prefix string, now time.Time, seenTTL time.Duration) (int, error) {
	total := 0
	for _, q := range []struct {
		index string
		field string
		cut   time.Time
	}{
		{prefix + "-seen", "first_seen", now.Add(-seenTTL)},
		{prefix + "-idempotency", "expires_at", now},
		{prefix + "-subscriptions", "expires_at", now},
	} {
		query := map[string]any{"range": map[string]any{q.field: map[string]any{"lt": q.cut.UTC().Format(time.RFC3339Nano)}}}
		n, err := deleteByQueryTask(ctx, c, q.index, query, 2000)
		total += n
		if err != nil {
			return total, fmt.Errorf("purge %s: %w", q.index, err)
		}
	}
	return total, nil
}

// DeleteByQuery removes the documents of index matching query (throttled, conflicts
// skipped) and returns how many were deleted. Used by the retention jobs.
func DeleteByQuery(ctx context.Context, c *Client, index string, query map[string]any) (int, error) {
	n, err := deleteByQueryTask(ctx, c, index, query, 1000)
	if err != nil {
		return n, fmt.Errorf("delete_by_query %s: %w", index, err)
	}
	return n, nil
}

// TaskPollInterval is how often a background delete is checked (a variable for tests).
var TaskPollInterval = 2 * time.Second

// deleteByQueryTask runs _delete_by_query as a background task and polls it until it
// finishes. A throttled delete of a large index takes longer than any sensible HTTP
// timeout (126k soar-seen keys at 2,000/s is over a minute), so it must not be one
// blocking request. If ctx ends first the task keeps running on the cluster; the next
// run deletes whatever is left.
func deleteByQueryTask(ctx context.Context, c *Client, index string, query map[string]any, rps int) (int, error) {
	path := fmt.Sprintf("/%s/_delete_by_query?conflicts=proceed&wait_for_completion=false&slices=auto&requests_per_second=%d", index, rps)
	_, out, err := c.Do(ctx, http.MethodPost, path, map[string]any{"query": query})
	if err != nil {
		return 0, err
	}
	var started struct {
		Task string `json:"task"`
	}
	if err := json.Unmarshal(out, &started); err != nil || started.Task == "" {
		return 0, fmt.Errorf("no task id in the answer: %.200s", out)
	}
	for {
		_, out, err := c.Do(ctx, http.MethodGet, "/_tasks/"+url.PathEscape(started.Task), nil)
		if err != nil {
			if ctx.Err() != nil { // gave up waiting; the task itself carries on
				return 0, fmt.Errorf("task %s still running: %w", started.Task, ctx.Err())
			}
			return 0, fmt.Errorf("task %s: %w", started.Task, err)
		}
		var st struct {
			Completed bool `json:"completed"`
			Response  struct {
				Deleted  int               `json:"deleted"`
				Failures []json.RawMessage `json:"failures"`
			} `json:"response"`
			Error json.RawMessage `json:"error"`
		}
		if err := json.Unmarshal(out, &st); err != nil {
			return 0, fmt.Errorf("task %s: %w", started.Task, err)
		}
		if st.Completed {
			if len(st.Error) > 0 && string(st.Error) != "null" {
				return st.Response.Deleted, fmt.Errorf("task %s failed: %.300s", started.Task, st.Error)
			}
			if len(st.Response.Failures) > 0 {
				return st.Response.Deleted, fmt.Errorf("task %s: %d failures, first: %.300s", started.Task, len(st.Response.Failures), st.Response.Failures[0])
			}
			return st.Response.Deleted, nil
		}
		select {
		case <-ctx.Done():
			return 0, fmt.Errorf("task %s still running: %w", started.Task, ctx.Err())
		case <-time.After(TaskPollInterval):
		}
	}
}
