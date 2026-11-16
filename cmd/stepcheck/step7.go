package main

import (
	"fmt"
	"net/http"
	"strings"
	"time"
)

func step7(c *client) step {
	secretName := "stepcheck_iris_key_" + c.run
	secretValue := "stepcheck-not-a-real-key-" + c.run
	var connID string

	return step{
		N: 7, Title: "Worker: built-in actions, SSRF guard, Python runner, secrets",
		After: func() { // remove what 7.4 created, whatever happened
			if connID != "" {
				_, _ = c.api(http.MethodDelete, "/connections/"+connID, nil)
			}
			_, _ = c.api(http.MethodDelete, "/secrets/"+secretName, nil)
		},
		Checks: []check{
			{"7.1", "the action catalog lists built-ins and the IRIS, Outlook and Wazuh connectors", "docker logs soar-api-1 --tail 50", func() (string, error) {
				r, err := c.apiWant(http.MethodGet, "/connectors", nil, 200)
				if err != nil {
					return "", err
				}
				have := map[string]bool{}
				for _, it := range list(r.Body, "items") {
					have[str(it, "id")] = true
				}
				var p problems
				for _, id := range []string{"builtin", "dfir_iris", "outlook", "wazuh"} {
					if !have[id] {
						p.add("connector %s missing", id)
					}
				}
				r, err = c.apiWant(http.MethodGet, "/connectors/dfir_iris/actions", nil, 200)
				if err != nil {
					return "", err
				}
				n := 0
				for _, it := range list(r.Body, "items") {
					n++
					if str(it, "class") != "python" || str(it, "side_effect") == "" || at(it, "params_schema") == nil {
						p.add("%s: incomplete manifest", str(it, "id"))
						break
					}
				}
				return fmt.Sprintf("%d connectors, %d IRIS actions", len(have), n), p.err()
			}},

			{"7.2", "built-in actions run in the worker (HTTP request, then a log line)", "docker logs soar-worker-1 --tail 50", func() (string, error) {
				d := def([]any{
					node{"id": "get", "action": "http.request", "params": node{"method": "GET", "url": `"http://api:8080/healthz"`}},
					node{"id": "log", "action": "notify.log", "params": node{"message": `"stepcheck: api answered " + string(steps.get.output.status)`}},
				}, "edges", []any{"get -> log"})
				_, exeID, err := c.start("stepcheck-builtin", d, nil)
				if err != nil {
					return "", err
				}
				exe, err := c.waitExecution(exeID, 30*time.Second)
				if err != nil {
					return "", err
				}
				if str(exe, "status") != "succeeded" || num(exe, "context", "nodes", "get", "output", "status") != 200 {
					return "", fmt.Errorf("execution %s: %s; %s", str(exe, "status"), nodeSummary(exe, "get"), nodeSummary(exe, "log"))
				}
				return "worker -> http://api:8080/healthz answered 200", nil
			}},

			{"7.3", "the SSRF guard blocks metadata and loopback addresses, without retries", "internal/actions/builtin.go (BlockedIP)", func() (string, error) {
				var p problems
				for _, u := range []string{"http://169.254.169.254/latest/meta-data/", "http://127.0.0.1:8080/healthz"} {
					d := def([]any{node{
						"id": "call", "action": "http.request", "params": node{"method": "GET", "url": fmt.Sprintf("%q", u)},
						"retry": node{"max_attempts": 3},
					}})
					_, exeID, err := c.start("stepcheck-ssrf", d, nil)
					if err != nil {
						return "", err
					}
					exe, err := c.waitExecution(exeID, 30*time.Second)
					if err != nil {
						return "", err
					}
					if str(exe, "status") != "failed" || str(exe, "context", "nodes", "call", "error", "code") != "ssrf_blocked" ||
						num(exe, "context", "nodes", "call", "attempt") != 1 {
						p.add("%s: want failed once with ssrf_blocked, got %s after %v attempt(s)", u, nodeSummary(exe, "call"),
							at(exe, "context", "nodes", "call", "attempt"))
					}
				}
				return "169.254.169.254 and 127.0.0.1 refused (1 attempt each)", p.err()
			}},

			{"7.4", "a Python action runs through the runner sandbox (IRIS connection test)", "docker logs soar-runner-1 --tail 50; docker logs soar-worker-1 --tail 50", func() (string, error) {
				r, err := c.api(http.MethodPut, "/secrets/"+secretName, map[string]any{"value": secretValue})
				if err != nil {
					return "", err
				}
				if r.Status/100 != 2 {
					return "", fmt.Errorf("PUT /secrets/%s answered HTTP %d: %s", secretName, r.Status, short(r.Raw))
				}
				r, err = c.apiWant(http.MethodPost, "/connections", map[string]any{
					"connector_id": "dfir_iris", "name": c.uniq("stepcheck-iris"),
					"auth_type": "bearer", "secret_refs": map[string]any{"api_key": secretName}, "config": map[string]any{"base_url": "https://iris.invalid"},
				}, 201)
				if err != nil {
					return "", err
				}
				connID = str(r.Body, "id")
				r, err = c.apiWant(http.MethodPost, "/connections/"+connID+"/test", nil, 200)
				if err != nil {
					return "", err
				}
				// iris.invalid never resolves: a "network" error proves the call went
				// api -> orchestrator -> worker -> runner -> sandboxed Python and back.
				msg := str(r.Body, "error")
				if at(r.Body, "ok") != false || !strings.HasPrefix(msg, "network") {
					return "", fmt.Errorf("want ok=false with a network error from the Python action, got ok=%v status=%s error=%q",
						at(r.Body, "ok"), str(r.Body, "status"), msg)
				}
				return "the action ran and reported: " + firstLine(msg), nil
			}},

			{"7.5", "stored secrets are never returned by the API", "internal/api (secrets, connections)", func() (string, error) {
				if connID == "" {
					return "", skip("needs 7.4")
				}
				var p problems
				r, err := c.apiWant(http.MethodGet, "/secrets?limit=200", nil, 200)
				if err != nil {
					return "", err
				}
				found := false
				for _, it := range list(r.Body, "items") {
					if str(it, "name") == secretName {
						found = true
						if at(it, "value") != nil {
							p.add("GET /secrets returns the value")
						}
					}
				}
				if !found {
					p.add("the secret is not listed")
				}
				if strings.Contains(r.Raw, secretValue) {
					p.add("GET /secrets contains the secret value")
				}
				if r, err := c.apiWant(http.MethodGet, "/connections/"+connID, nil, 200); err != nil || strings.Contains(r.Raw, secretValue) {
					p.add("GET /connections/{id} leaks the value or failed: %v", err)
				}
				return "metadata only; the value appears in no response", p.err()
			}},

			{"7.6", "a Python action without a valid connection fails at once (no retries)", "internal/worker/worker.go (connection)", func() (string, error) {
				d := def([]any{node{
					"id": "iocs", "action": "iris.ioc.list", "params": node{"case_id": 1, "connection": `"stepcheck-no-such-connection"`},
					"retry": node{"max_attempts": 3},
				}})
				_, exeID, err := c.start("stepcheck-noconn", d, nil)
				if err != nil {
					return "", err
				}
				exe, err := c.waitExecution(exeID, 30*time.Second)
				if err != nil {
					return "", err
				}
				if str(exe, "status") != "failed" || str(exe, "context", "nodes", "iocs", "error", "code") != "invalid_connection" ||
					num(exe, "context", "nodes", "iocs", "attempt") != 1 {
					return "", fmt.Errorf("want failed once with invalid_connection, got %s after %v attempt(s)", nodeSummary(exe, "iocs"),
						at(exe, "context", "nodes", "iocs", "attempt"))
				}
				return "invalid_connection after 1 attempt", nil
			}},
		},
	}
}
