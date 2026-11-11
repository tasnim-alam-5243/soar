package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// node and def keep the workflow literals short.
type node = map[string]any

func def(nodes []any, extra ...any) map[string]any {
	d := map[string]any{"nodes": nodes}
	for i := 0; i+1 < len(extra); i += 2 {
		d[extra[i].(string)] = extra[i+1]
	}
	return d
}

func step6(c *client) step {
	var trigWF string         // set by 6.8, used by 6.9
	var titleA, titleB string // alert titles that fire 6.8's two triggers
	var published string      // a published workflow, for 6.11

	return step{N: 6, Title: "Engine: validator, branches, retries, foreach, waits, triggers, approvals", Checks: []check{
		{"6.1", "the validator rejects broken workflows with a path-addressed error", "go test ./internal/engine", func() (string, error) {
			cases := []struct {
				name string
				def  string
			}{
				{"cycle", `{"nodes":[{"id":"a","type":"set","vars":{"x":"1"}},{"id":"b","type":"set","vars":{"y":"2"}}],"edges":["a -> b","b -> a"]}`},
				{"unknown action", `{"nodes":[{"id":"a","action":"nope.nothing"}]}`},
				// conditions must be CEL; values (params, set vars) fall back to literal text instead
				{"condition that does not compile", `{"nodes":[{"id":"a","type":"set","when":"1 +","vars":{"x":"1"}}]}`},
				{"missing required parameter", `{"nodes":[{"id":"a","action":"http.request"}]}`},
				{"agent node (not enabled)", `{"nodes":[{"id":"a","type":"agent"}]}`},
				{"duplicate node id", `{"nodes":[{"id":"a","type":"set","vars":{"x":"1"}},{"id":"a","type":"set","vars":{"y":"1"}}]}`},
			}
			var p problems
			for _, tc := range cases {
				r, err := c.apiWant(http.MethodPost, "/workflows/validate", map[string]any{"definition": json.RawMessage(tc.def)}, 200)
				if err != nil {
					p.add("%s: %v", tc.name, err)
					continue
				}
				errs := list(r.Body, "errors")
				if at(r.Body, "valid") != false || len(errs) == 0 || str(errs[0], "message") == "" || str(errs[0], "path") == "" {
					p.add("%s: want valid=false with an error, got %s", tc.name, short(r.Raw))
				}
			}
			return fmt.Sprintf("%d broken definitions refused", len(cases)), p.err()
		}},

		{"6.2", "a valid workflow passes validation", "go test ./internal/engine", func() (string, error) {
			r, err := c.apiWant(http.MethodPost, "/workflows/validate", map[string]any{"definition": def([]any{node{"id": "a", "type": "set", "vars": node{"x": "1"}}})}, 200)
			if err != nil {
				return "", err
			}
			if at(r.Body, "valid") != true {
				return "", fmt.Errorf("want valid=true: %s", short(r.Raw))
			}
			return "", nil
		}},

		{"6.3", "branch: only the matching path runs", "docker logs soar-orchestrator-1 --tail 50", func() (string, error) {
			d := def([]any{
				node{"id": "decide", "type": "branch", "cases": []any{node{"when": "inputs.n > 3", "next": []any{"big"}}}, "default": []any{"small"}},
				node{"id": "big", "type": "set", "vars": node{"size": `"big"`}},
				node{"id": "small", "type": "set", "vars": node{"size": `"small"`}},
			}, "inputs", []any{node{"name": "n", "default": 0}})
			wf, exeID, err := c.start("stepcheck-branch", d, map[string]any{"inputs": node{"n": 5}})
			if err != nil {
				return "", err
			}
			published = wf
			exe, err := c.waitExecution(exeID, 30*time.Second)
			if err != nil {
				return "", err
			}
			var p problems
			if s := str(exe, "status"); s != "succeeded" {
				p.add("execution %s", s)
			}
			if v := str(exe, "context", "vars", "size"); v != "big" {
				p.add("vars.size = %q, want big", v)
			}
			if s := str(exe, "context", "nodes", "small", "status"); s != "skipped" {
				p.add("the other branch must be skipped, it is %q", s)
			}
			return "inputs.n=5 took the 'big' path, 'small' skipped", p.err()
		}},

		{"6.4", "a failing action is retried with backoff, then the error path runs", "docker logs soar-worker-1 --tail 50", func() (string, error) {
			d := def([]any{
				node{
					"id": "call", "action": "http.request", "params": node{"method": "GET", "url": `"http://10.255.255.1/"`},
					"timeout": "2s", "retry": node{"max_attempts": 3, "initial": "1s", "max": "2s"}, "on_error": []any{"handle"},
				},
				node{"id": "handle", "type": "set", "vars": node{"handled": "true"}},
			})
			_, exeID, err := c.start("stepcheck-retry", d, nil)
			if err != nil {
				return "", err
			}
			exe, err := c.waitExecution(exeID, 90*time.Second)
			if err != nil {
				return "", err
			}
			var p problems
			if s := str(exe, "status"); s != "succeeded" {
				p.add("execution %s (the error path should have handled the failure)", s)
			}
			code := str(exe, "context", "nodes", "call", "error", "code")
			if code != "timeout" && code != "network" {
				p.add("call: want a timeout/network error, got %s", nodeSummary(exe, "call"))
			}
			if n := num(exe, "context", "nodes", "call", "attempt"); n != 3 {
				p.add("call ran %v attempt(s), want 3", n)
			}
			if s := str(exe, "context", "nodes", "handle", "status"); s != "succeeded" {
				p.add("the on_error node did not run: %s", nodeSummary(exe, "handle"))
			}
			return fmt.Sprintf("3 attempts (%s), then on_error", code), p.err()
		}},

		{"6.5", "foreach runs every item, never more than 'concurrency' at once", "docker logs soar-worker-1 --tail 50", func() (string, error) {
			d := def([]any{node{
				"id": "each", "type": "foreach", "items": "[1, 2, 3, 4]", "action": "delay.sleep",
				"params": node{"seconds": "1"}, "concurrency": 2,
			}})
			_, exeID, err := c.start("stepcheck-foreach", d, nil)
			if err != nil {
				return "", err
			}
			exe, err := c.waitExecution(exeID, 60*time.Second)
			if err != nil {
				return "", err
			}
			if s := str(exe, "status"); s != "succeeded" {
				return "", fmt.Errorf("execution %s: %s", s, nodeSummary(exe, "each"))
			}
			if n := len(list(exe, "context", "nodes", "each", "output")); n != 4 {
				return "", fmt.Errorf("want 4 item results, got %d", n)
			}
			runs, err := c.stepRuns(exeID, "each", 4, 20*time.Second)
			if err != nil {
				return "", err
			}
			var starts, ends []time.Time
			for _, r := range runs {
				s, e1 := time.Parse(time.RFC3339Nano, str(r, "started_at"))
				e, e2 := time.Parse(time.RFC3339Nano, str(r, "ended_at"))
				if e1 != nil || e2 != nil {
					return "", fmt.Errorf("step run without start/end time: %v", r)
				}
				starts, ends = append(starts, s), append(ends, e)
			}
			if m := maxOverlap(starts, ends); m > 2 {
				return "", fmt.Errorf("%d items ran at the same time, concurrency is 2", m)
			}
			return "4 items, at most 2 at a time", nil
		}},

		{"6.6", "a wait pauses the execution; cancel stops it", "docker logs soar-scheduler-1 --tail 50", func() (string, error) {
			_, exeID, err := c.start("stepcheck-wait", def([]any{node{"id": "pause", "type": "wait", "duration": "5m"}}), nil)
			if err != nil {
				return "", err
			}
			if _, err := c.waitExecution(exeID, 30*time.Second, "waiting"); err != nil {
				return "", err
			}
			if _, err := c.apiWant(http.MethodPost, "/executions/"+exeID+"/cancel", nil, 202); err != nil {
				return "", err
			}
			if _, err := c.waitExecution(exeID, 30*time.Second, "cancelled"); err != nil {
				return "", err
			}
			return "waiting -> cancelled", nil
		}},

		{"6.7", "a dry run reports what it would do and sends nothing", "docker logs soar-worker-1 --tail 50", func() (string, error) {
			d := def([]any{node{"id": "call", "action": "http.request", "params": node{"method": "POST", "url": `"https://example.com/stepcheck"`}}})
			_, exeID, err := c.start("stepcheck-dryrun", d, map[string]any{"dry_run": true})
			if err != nil {
				return "", err
			}
			exe, err := c.waitExecution(exeID, 30*time.Second)
			if err != nil {
				return "", err
			}
			if str(exe, "status") != "succeeded" || at(exe, "context", "nodes", "call", "output", "dry_run") != true {
				return "", fmt.Errorf("want succeeded with output.dry_run=true: %s, output %v", nodeSummary(exe, "call"), at(exe, "context", "nodes", "call", "output"))
			}
			return "would send " + str(exe, "context", "nodes", "call", "output", "would_send", "method") + " " +
				str(exe, "context", "nodes", "call", "output", "would_send", "url"), nil
		}},

		{"6.8", "triggers in the definition and at top level are both stored on the workflow", "internal/api/workflowcheck.go (liftTriggers)", func() (string, error) {
			titleA, titleB = c.uniq("stepcheck trigger top"), c.uniq("stepcheck trigger def")
			d := def([]any{node{"id": "mark", "type": "set", "vars": node{"seen": "alert.title"}}},
				"triggers", []any{node{"type": "alert", "match": fmt.Sprintf("alert.title == %q", titleB)}})
			wf, err := c.createWorkflow(c.uniq("stepcheck-trigger"), d, []any{node{"type": "alert", "match": fmt.Sprintf("alert.title == %q", titleA)}})
			if err != nil {
				return "", err
			}
			trigWF = str(wf, "id")
			if n := len(list(wf, "triggers")); n != 2 {
				return "", fmt.Errorf("want 2 triggers on the workflow, got %d: %v", n, at(wf, "triggers"))
			}
			if at(wf, "definition", "triggers") != nil {
				return "", fmt.Errorf("triggers were left inside the definition")
			}
			return "", nil
		}},

		{"6.9", "an ingested alert starts the matching workflow (both trigger forms)", "docker logs soar-processor-1 --tail 50 (triggers reload every 10 s)", func() (string, error) {
			if trigWF == "" {
				return "", skip("needs 6.8")
			}
			if err := c.publish(trigWF); err != nil {
				return "", err
			}
			time.Sleep(11 * time.Second) // the processor reloads workflow triggers every 10 s
			for _, t := range []string{titleA, titleB} {
				if _, err := c.pushWant(wazuhAlert(c.uniq("sc.trg"), t, c.uniq("stepcheck-trg"), 5), nil); err != nil {
					return "", err
				}
			}
			var seen []string
			err := waitFor(60*time.Second, "two finished executions of the workflow", func() (bool, error) {
				r, err := c.apiWant(http.MethodGet, "/executions?workflow_id="+trigWF, nil, 200)
				if err != nil {
					return false, err
				}
				seen = seen[:0]
				done := 0
				for _, it := range list(r.Body, "items") {
					seen = append(seen, str(it, "status"))
					if str(it, "status") == "succeeded" {
						done++
					}
				}
				return done == 2, fmt.Errorf("executions: %v", seen)
			})
			return "top-level trigger and definition trigger each started one run", err
		}},

		{"6.10", "an approval holds the execution until someone decides", "docker logs soar-orchestrator-1 --tail 50", func() (string, error) {
			d := def([]any{
				node{"id": "ask", "type": "approval", "message": `"stepcheck: approve to continue"`},
				node{"id": "after", "type": "set", "vars": node{"approved": "steps.ask.output.approved"}},
			}, "edges", []any{"ask -> after"})
			_, exeID, err := c.start("stepcheck-approval", d, nil)
			if err != nil {
				return "", err
			}
			if _, err := c.waitExecution(exeID, 30*time.Second, "waiting"); err != nil {
				return "", err
			}
			var aprID string
			if err := waitFor(20*time.Second, "a pending approval", func() (bool, error) {
				r, err := c.apiWant(http.MethodGet, "/approvals?execution_id="+exeID+"&status=pending", nil, 200)
				aprID = str(r.Body, "items", "0", "id")
				return err == nil && aprID != "", err
			}); err != nil {
				return "", err
			}
			if _, err := c.apiWant(http.MethodPost, "/approvals/"+aprID+"/decision", map[string]any{"approved": true, "comment": "stepcheck"}, 200); err != nil {
				return "", err
			}
			exe, err := c.waitExecution(exeID, 30*time.Second)
			if err != nil {
				return "", err
			}
			if str(exe, "status") != "succeeded" || at(exe, "context", "vars", "approved") != true {
				return "", fmt.Errorf("after approving: %s, %s, vars %v", str(exe, "status"), nodeSummary(exe, "after"), at(exe, "context", "vars"))
			}
			return "waiting -> approved by " + str(exe, "context", "nodes", "ask", "output", "decided_by") + " -> succeeded", nil
		}},

		{"6.11", "a published version cannot be edited", "internal/api/workflows.go", func() (string, error) {
			if published == "" {
				return "", skip("needs 6.3")
			}
			wf, err := c.apiWant(http.MethodGet, "/workflows/"+published, nil, 200)
			if err != nil {
				return "", err
			}
			r, err := c.api(http.MethodPut, "/workflows/"+published, map[string]any{"name": str(wf.Body, "name"), "definition": at(wf.Body, "definition")})
			if err != nil {
				return "", err
			}
			if r.Status != http.StatusConflict {
				return "", fmt.Errorf("editing a published workflow answered HTTP %d, want 409: %s", r.Status, short(r.Raw))
			}
			return "PUT answered 409 " + strings.TrimSpace(str(r.Body, "title")), nil
		}},
	}}
}
