package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
)

type client struct {
	apiURL, ingestURL string
	osURL             string // dev OpenSearch, only for the fake Wazuh indexer of check 5.12
	key, token        string
	hc                *http.Client
	run               string // unique per run, part of every name this tool creates
	seq               atomic.Int64
}

type reply struct {
	Status int
	Body   map[string]any
	Raw    string
}

// uniq returns a name unique to this run: "<prefix>-<run>-<n>".
func (c *client) uniq(prefix string) string {
	return fmt.Sprintf("%s-%s-%d", prefix, c.run, c.seq.Add(1))
}

func (c *client) send(method, u string, body any, hdr map[string]string) (reply, error) {
	var rd io.Reader
	switch b := body.(type) {
	case nil:
	case []byte:
		rd = bytes.NewReader(b)
	default:
		j, err := json.Marshal(b)
		if err != nil {
			return reply{}, err
		}
		rd = bytes.NewReader(j)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, method, u, rd)
	if err != nil {
		return reply{}, err
	}
	if rd != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		return reply{}, fmt.Errorf("%s %s: %w", method, u, err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	r := reply{Status: resp.StatusCode, Raw: string(raw)}
	_ = json.Unmarshal(raw, &r.Body)
	return r, nil
}

// api calls /api/v1<path> with the admin key.
func (c *client) api(method, path string, body any) (reply, error) {
	return c.send(method, c.apiURL+"/api/v1"+path, body, map[string]string{"Authorization": "Bearer " + c.key})
}

// apiWant is api plus a status check.
func (c *client) apiWant(method, path string, body any, want int) (reply, error) {
	r, err := c.api(method, path, body)
	if err != nil {
		return r, err
	}
	if r.Status != want {
		return r, fmt.Errorf("%s %s answered HTTP %d (want %d): %s", method, path, r.Status, want, short(r.Raw))
	}
	return r, nil
}

// push posts to the wazuh-prod webhook source with its token.
func (c *client) push(body any, hdr map[string]string) (reply, error) {
	h := map[string]string{"Authorization": "Bearer " + c.token}
	for k, v := range hdr {
		h[k] = v
	}
	return c.send(http.MethodPost, c.ingestURL+"/ingest/wazuh-prod", body, h)
}

// pushWant pushes and requires HTTP 202.
func (c *client) pushWant(body any, hdr map[string]string) (reply, error) {
	r, err := c.push(body, hdr)
	if err != nil {
		return r, err
	}
	if r.Status != http.StatusAccepted {
		return r, fmt.Errorf("ingest answered HTTP %d (want 202): %s", r.Status, short(r.Raw))
	}
	return r, nil
}

// wazuhAlert is a Wazuh manager alert (Integrator format) with the given identity.
// The agent is part of the grouping key, so each check uses its own host.
func wazuhAlert(id, title, host string, level int) map[string]any {
	return map[string]any{
		"timestamp": time.Now().Format("2006-01-02T15:04:05.000-0700"), "id": id,
		"rule": map[string]any{
			"level": level, "id": "5715", "description": title, "groups": []string{"syslog", "sshd"},
			"mitre": map[string]any{"id": []string{"T1078"}, "tactic": []string{"Initial Access"}, "technique": []string{"Valid Accounts"}},
		},
		"agent":   map[string]any{"id": host, "name": host},
		"decoder": map[string]any{"name": "sshd"},
		"data":    map[string]any{"srcip": "10.10.192.1", "dstuser": "root"},
	}
}

// findAlerts returns the wazuh-prod alerts with this title (newest 200 alerts searched).
func (c *client) findAlerts(title string) ([]map[string]any, error) {
	r, err := c.apiWant(http.MethodGet, "/alerts?source_id=wazuh-prod&sort=-received_at&limit=200", nil, 200)
	if err != nil {
		return nil, err
	}
	var out []map[string]any
	for _, it := range list(r.Body, "items") {
		if m, ok := it.(map[string]any); ok && m["title"] == title {
			out = append(out, m)
		}
	}
	return out, nil
}

// waitAlert waits until exactly one alert with this title exists and ok(alert) holds.
func (c *client) waitAlert(title string, d time.Duration, what string, ok func(map[string]any) bool) (map[string]any, error) {
	var a map[string]any
	err := waitFor(d, what, func() (bool, error) {
		as, err := c.findAlerts(title)
		if err != nil || len(as) == 0 {
			return false, err
		}
		if len(as) > 1 {
			return false, fmt.Errorf("%d alert documents with the title %q, want one", len(as), title)
		}
		a = as[0]
		return ok == nil || ok(a), nil
	})
	return a, err
}

// ---- workflows and executions ----

func (c *client) createWorkflow(name string, def map[string]any, triggers []any) (map[string]any, error) {
	in := map[string]any{"name": name, "definition": def}
	if triggers != nil {
		in["triggers"] = triggers
	}
	r, err := c.apiWant(http.MethodPost, "/workflows", in, 201)
	if err != nil {
		return nil, err
	}
	return r.Body, nil
}

func (c *client) publish(id string) error {
	_, err := c.apiWant(http.MethodPost, "/workflows/"+id+"/publish", nil, 200)
	return err
}

// start creates a workflow, publishes it (unless the run is a dry run) and runs it.
// It returns the workflow and execution ids.
func (c *client) start(name string, def map[string]any, run map[string]any) (string, string, error) {
	wf, err := c.createWorkflow(c.uniq(name), def, nil)
	if err != nil {
		return "", "", err
	}
	id := str(wf, "id")
	if run["dry_run"] != true {
		if err := c.publish(id); err != nil {
			return id, "", err
		}
	}
	if run == nil {
		run = map[string]any{}
	}
	r, err := c.apiWant(http.MethodPost, "/workflows/"+id+"/run", run, 202)
	if err != nil {
		return id, "", err
	}
	return id, str(r.Body, "id"), nil
}

var terminal = []string{"succeeded", "failed", "cancelled", "timed_out"}

// waitExecution polls an execution until its status is one of statuses (default: finished).
func (c *client) waitExecution(id string, d time.Duration, statuses ...string) (map[string]any, error) {
	if len(statuses) == 0 {
		statuses = terminal
	}
	var exe map[string]any
	err := waitFor(d, "execution "+id+" to reach "+strings.Join(statuses, "/"), func() (bool, error) {
		r, err := c.apiWant(http.MethodGet, "/executions/"+id, nil, 200)
		if err != nil {
			return false, err
		}
		exe = r.Body
		s := str(exe, "status")
		for _, want := range statuses {
			if s == want {
				return true, nil
			}
		}
		return false, fmt.Errorf("status is %s", s)
	})
	return exe, err
}

// stepRuns waits until node has at least n step-run records (they are searchable after
// the next index refresh, up to 5 s after the step ends).
func (c *client) stepRuns(exeID, node string, n int, d time.Duration) ([]map[string]any, error) {
	var runs []map[string]any
	err := waitFor(d, fmt.Sprintf("%d step run(s) of %s", n, node), func() (bool, error) {
		r, err := c.apiWant(http.MethodGet, "/executions/"+exeID+"/steps?limit=200", nil, 200)
		if err != nil {
			return false, err
		}
		runs = runs[:0]
		for _, it := range list(r.Body, "items") {
			if m, ok := it.(map[string]any); ok && m["node_id"] == node {
				runs = append(runs, m)
			}
		}
		return len(runs) >= n, fmt.Errorf("found %d", len(runs))
	})
	return runs, err
}

// nodeSummary describes a node's state for error messages.
func nodeSummary(exe map[string]any, node string) string {
	n := at(exe, "context", "nodes", node)
	if n == nil {
		return node + ": no state"
	}
	s := fmt.Sprintf("%s: %s", node, str(n, "status"))
	if code := str(n, "error", "code"); code != "" {
		s += fmt.Sprintf(" (%s: %s)", code, str(n, "error", "message"))
	}
	return s
}

// ---- small helpers ----

// waitFor polls fn every 500 ms until it returns true or d passes. An error from fn is
// remembered as the last observation and reported on timeout.
func waitFor(d time.Duration, what string, fn func() (bool, error)) error {
	deadline := time.Now().Add(d)
	var last error
	for {
		ok, err := fn()
		if ok {
			return nil
		}
		if err != nil {
			last = err
		}
		if time.Now().After(deadline) {
			if last != nil {
				return fmt.Errorf("timed out after %s waiting for %s (last seen: %w)", d, what, last)
			}
			return fmt.Errorf("timed out after %s waiting for %s", d, what)
		}
		time.Sleep(500 * time.Millisecond)
	}
}

// at walks decoded JSON: at(v, "context", "nodes", "a", "status"); list indexes are numbers.
func at(v any, path ...string) any {
	for _, p := range path {
		switch x := v.(type) {
		case map[string]any:
			v = x[p]
		case []any:
			i, err := strconv.Atoi(p)
			if err != nil || i < 0 || i >= len(x) {
				return nil
			}
			v = x[i]
		default:
			return nil
		}
	}
	return v
}

func str(v any, path ...string) string {
	switch x := at(v, path...).(type) {
	case string:
		return x
	case nil:
		return ""
	default:
		return fmt.Sprint(x)
	}
}

func num(v any, path ...string) float64 { f, _ := at(v, path...).(float64); return f }

func list(v any, path ...string) []any { l, _ := at(v, path...).([]any); return l }

func short(s string) string {
	s = strings.TrimSpace(s)
	if len(s) > 300 {
		return s[:300] + "..."
	}
	return s
}

func q(s string) string { return url.QueryEscape(s) }

// problems collects mismatches; err() joins them.
type problems []string

func (p *problems) add(format string, a ...any) { *p = append(*p, fmt.Sprintf(format, a...)) }

func (p problems) err() error {
	if len(p) == 0 {
		return nil
	}
	return fmt.Errorf("%s", strings.Join(p, "; "))
}

// maxOverlap returns the largest number of [start, end) intervals open at the same time.
func maxOverlap(starts, ends []time.Time) int {
	type ev struct {
		t     time.Time
		delta int
	}
	var evs []ev
	for i := range starts {
		evs = append(evs, ev{starts[i], 1}, ev{ends[i], -1})
	}
	sort.Slice(evs, func(i, j int) bool {
		if evs[i].t.Equal(evs[j].t) {
			return evs[i].delta < evs[j].delta // an end at t closes before a start at t opens
		}
		return evs[i].t.Before(evs[j].t)
	})
	cur, best := 0, 0
	for _, e := range evs {
		cur += e.delta
		if cur > best {
			best = cur
		}
	}
	return best
}
