package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"
)

func step5(c *client) step {
	return step{N: 5, Title: "Ingestion: Wazuh push, queue, filters, dead letters", Checks: []check{
		{"5.1", "api and ingest are ready", "make ps; docker logs soar-ingest-1 --tail 50", func() (string, error) {
			for _, u := range []string{c.apiURL + "/readyz", c.ingestURL + "/readyz"} {
				r, err := c.send(http.MethodGet, u, nil, nil)
				if err != nil {
					return "", err
				}
				if r.Status != 200 || str(r.Body, "status") != "ok" {
					return "", fmt.Errorf("%s answered HTTP %d: %s", u, r.Status, short(r.Raw))
				}
			}
			return "", nil
		}},

		{"5.2", "the Wazuh pack is installed (push source wazuh-prod)", "make seed", func() (string, error) {
			r, err := c.apiWant(http.MethodGet, "/sources?adapter=webhook", nil, 200)
			if err != nil {
				return "", err
			}
			for _, it := range list(r.Body, "items") {
				if str(it, "name") == "wazuh-prod" {
					if at(it, "enabled") != true {
						return "", fmt.Errorf("wazuh-prod exists but is disabled")
					}
					return "", nil
				}
			}
			return "", fmt.Errorf("no webhook source named wazuh-prod")
		}},

		{"5.3", "a pushed Wazuh alert is stored (severity, MITRE, vendor fields, observables)", "docker logs soar-processor-1 --tail 50", func() (string, error) {
			title, host := c.uniq("stepcheck push"), c.uniq("stepcheck-host")
			r, err := c.pushWant(wazuhAlert(c.uniq("sc.push"), title, host, 3), nil)
			if err != nil {
				return "", err
			}
			if n := len(list(r.Body, "event_ids")); n != 1 {
				return "", fmt.Errorf("want 1 event id, got %d: %s", n, short(r.Raw))
			}
			t0 := time.Now()
			a, err := c.waitAlert(title, 30*time.Second, "the alert to be searchable", nil)
			if err != nil {
				return "", err
			}
			took := time.Since(t0)
			var p problems
			if num(a, "severity") != 1 || str(a, "original_severity") != "3" {
				p.add("Wazuh level 3 must become severity 1 (got severity %v, original %q)", at(a, "severity"), str(a, "original_severity"))
			}
			if num(a, "occurrence_count") != 1 {
				p.add("occurrence_count %v, want 1", at(a, "occurrence_count"))
			}
			if str(a, "mitre", "technique_ids", "0") != "T1078" {
				p.add("MITRE technique missing: %v", at(a, "mitre"))
			}
			if at(a, "ext", "decoder") == nil {
				p.add("vendor fields must be kept under ext: %v", at(a, "ext"))
			}
			if err := waitFor(30*time.Second, "the host observable", func() (bool, error) {
				r, err := c.apiWant(http.MethodGet, "/observables?type=host&value="+q(host), nil, 200)
				return err == nil && len(list(r.Body, "items")) == 1, err
			}); err != nil {
				p.add("%v", err)
			}
			return fmt.Sprintf("searchable %.1fs after the push", took.Seconds()), p.err()
		}},

		{"5.4", "one request can carry several records", "docker logs soar-ingest-1 --tail 50", func() (string, error) {
			var batch []any
			var titles []string
			for i := 0; i < 3; i++ {
				t := c.uniq("stepcheck batch")
				titles = append(titles, t)
				batch = append(batch, wazuhAlert(c.uniq("sc.batch"), t, c.uniq("stepcheck-batch"), 5))
			}
			r, err := c.pushWant(batch, nil)
			if err != nil {
				return "", err
			}
			if n := len(list(r.Body, "event_ids")); n != 3 {
				return "", fmt.Errorf("want 3 event ids, got %d: %s", n, short(r.Raw))
			}
			for _, t := range titles {
				if _, err := c.waitAlert(t, 30*time.Second, "alert "+t, nil); err != nil {
					return "", err
				}
			}
			return "3 records in one request, 3 alerts", nil
		}},

		{"5.5", "the same alert sent twice is stored once", "docker logs soar-processor-1 --tail 50", func() (string, error) {
			title := c.uniq("stepcheck dup")
			a := wazuhAlert(c.uniq("sc.dup"), title, c.uniq("stepcheck-dup"), 3)
			// different Idempotency-Keys get past the queue's own deduplication, so only the
			// processor's duplicate guard can stop the second copy
			for _, k := range []string{c.uniq("k1"), c.uniq("k2")} {
				if _, err := c.pushWant(a, map[string]string{"Idempotency-Key": k}); err != nil {
					return "", err
				}
			}
			if _, err := c.waitAlert(title, 30*time.Second, "the alert", nil); err != nil {
				return "", err
			}
			time.Sleep(4 * time.Second) // time for a wrongly counted second copy to show up
			as, err := c.findAlerts(title)
			if err != nil {
				return "", err
			}
			if len(as) != 1 {
				return "", fmt.Errorf("want one alert, got %d", len(as))
			}
			if n := num(as[0], "occurrence_count"); n != 1 {
				return "", fmt.Errorf("the second copy was counted: occurrence_count %v, want 1", n)
			}
			return "", nil
		}},

		{"5.6", "repeats are grouped into one alert and its severity rises", "docker logs soar-processor-1 --tail 50", func() (string, error) {
			title, host := c.uniq("stepcheck repeat"), c.uniq("stepcheck-rep")
			for _, level := range []int{3, 3, 10} {
				if _, err := c.pushWant(wazuhAlert(c.uniq("sc.rep"), title, host, level), nil); err != nil {
					return "", err
				}
			}
			a, err := c.waitAlert(title, 30*time.Second, "three occurrences in one alert", func(a map[string]any) bool {
				return num(a, "occurrence_count") == 3
			})
			if err != nil {
				return "", err
			}
			if num(a, "severity") != 4 {
				return "", fmt.Errorf("severity must rise to the worst occurrence (level 10 -> 4), got %v", at(a, "severity"))
			}
			return "3 pushes -> 1 alert, occurrence_count 3, severity 1 -> 4", nil
		}},

		{"5.7", "bad requests are refused (401, 404, 413, 422)", "docker logs soar-ingest-1 --tail 50", func() (string, error) {
			var p problems
			expect := func(what string, want int, r reply, err error) {
				if err != nil {
					p.add("%s: %v", what, err)
				} else if r.Status != want {
					p.add("%s: HTTP %d, want %d", what, r.Status, want)
				}
			}
			r, err := c.send(http.MethodPost, c.ingestURL+"/ingest/wazuh-prod", []byte(`{}`), map[string]string{"Authorization": "Bearer wrong-token"})
			expect("wrong token", 401, r, err)
			r, err = c.send(http.MethodPost, c.ingestURL+"/ingest/stepcheck-no-such-source", []byte(`{}`), map[string]string{"Authorization": "Bearer " + c.token})
			expect("unknown source", 404, r, err)
			big := wazuhAlert(c.uniq("sc.big"), c.uniq("stepcheck big"), "stepcheck-big", 3)
			big["full_log"] = strings.Repeat("x", 1_100_000)
			r, err = c.push(big, nil)
			expect("body over 1 MB", 413, r, err)
			var many []any
			for i := 0; i < 501; i++ {
				many = append(many, map[string]any{"id": fmt.Sprint(i)})
			}
			r, err = c.push(many, nil)
			expect("501 records", 422, r, err)
			return "", p.err()
		}},

		{"5.8", "low-level alerts are filtered; broken records go to the DLQ, not lost", "docker logs soar-processor-1 --tail 50; api GET /dlq", func() (string, error) {
			low := c.uniq("stepcheck low")
			r, err := c.pushWant(wazuhAlert(c.uniq("sc.low"), low, "stepcheck-low", 1), nil)
			if err != nil {
				return "", err
			}
			if num(r.Body, "filtered") != 1 || len(list(r.Body, "event_ids")) != 0 {
				return "", fmt.Errorf("a level-1 alert is below the filter floor and must be filtered: %s", short(r.Raw))
			}
			before, err := c.lastDLQSeq()
			if err != nil {
				return "", err
			}
			broken := wazuhAlert(c.uniq("sc.bad"), c.uniq("stepcheck broken"), "stepcheck-bad", 5)
			delete(broken, "timestamp")
			r, err = c.pushWant(broken, nil)
			if err != nil {
				return "", err
			}
			if num(r.Body, "failed") != 1 {
				return "", fmt.Errorf("a record that cannot be mapped must be accepted as failed (for the DLQ): %s", short(r.Raw))
			}
			var reason string
			if err := waitFor(20*time.Second, "the broken record in the DLQ", func() (bool, error) {
				r, err := c.apiWant(http.MethodGet, "/dlq?limit=20", nil, 200)
				if err != nil {
					return false, err
				}
				for _, it := range list(r.Body, "items") {
					if num(it, "seq") > before && str(it, "origin_subject") == "events.wazuh-prod" {
						reason = str(it, "reason")
						return true, nil
					}
				}
				return false, nil
			}); err != nil {
				return "", err
			}
			if as, _ := c.findAlerts(low); len(as) != 0 {
				return "", fmt.Errorf("the filtered alert was stored anyway")
			}
			return "DLQ reason: " + firstLine(reason), nil
		}},

		{"5.9", "test endpoints show the result and store nothing", "api POST /sources/<id>/test", func() (string, error) {
			id, err := c.sourceID("webhook", "wazuh-prod")
			if err != nil {
				return "", err
			}
			title := c.uniq("stepcheck dry")
			sample := wazuhAlert(c.uniq("sc.dry"), title, "stepcheck-dry", 5)
			r, err := c.apiWant(http.MethodPost, "/sources/"+id+"/test", map[string]any{"payload": sample}, 200)
			if err != nil {
				return "", err
			}
			if n := len(list(r.Body, "events")); n != 1 {
				return "", fmt.Errorf("source test: want 1 normalized event, got %d: %s", n, short(r.Raw))
			}
			if _, err := c.apiWant(http.MethodPost, "/mappings/test", map[string]any{"mapping": "wazuh_alert@1", "sample": sample}, 200); err != nil {
				return "", err
			}
			time.Sleep(7 * time.Second)
			if as, _ := c.findAlerts(title); len(as) != 0 {
				return "", fmt.Errorf("a test run stored an alert")
			}
			return "", nil
		}},

		{"5.10", "a list edit changes filtering within seconds, without a restart", "docker logs soar-ingest-1 --tail 50", func() (string, error) {
			const name = "wazuh_noisy_rule_ids"
			cur, err := c.apiWant(http.MethodGet, "/lists/"+name, nil, 200)
			if err != nil {
				return "", err
			}
			orig := []string{}
			for _, v := range list(cur.Body, "items") {
				orig = append(orig, fmt.Sprint(v))
			}
			defer func() { _, _ = c.api(http.MethodPut, "/lists/"+name, map[string]any{"items": orig}) }() // put the list back
			rule := fmt.Sprint(70000 + time.Now().UnixNano()%9000)
			if _, err := c.apiWant(http.MethodPut, "/lists/"+name, map[string]any{"items": append(append([]string{}, orig...), rule)}, 200); err != nil {
				return "", err
			}
			sample := wazuhAlert(c.uniq("sc.noisy"), c.uniq("stepcheck noisy"), "stepcheck-noisy", 5)
			sample["rule"].(map[string]any)["id"] = rule
			r, err := c.apiWant(http.MethodPost, "/filters/test", map[string]any{"filter_set": "wazuh-default@1", "sample": sample}, 200)
			if err != nil {
				return "", err
			}
			if at(r.Body, "drop") != true {
				return "", fmt.Errorf("the filter test must drop rule %s at once: %s", rule, short(r.Raw))
			}
			t0 := time.Now()
			err = waitFor(40*time.Second, "ingest to apply the edited list", func() (bool, error) {
				a := wazuhAlert(c.uniq("sc.noisy"), c.uniq("stepcheck noisy"), "stepcheck-noisy", 5)
				a["rule"].(map[string]any)["id"] = rule
				r, err := c.pushWant(a, nil)
				return err == nil && num(r.Body, "filtered") == 1, err
			})
			return fmt.Sprintf("filter test dropped it at once; ingest after %.0fs (list restored)", time.Since(t0).Seconds()), err
		}},

		{"5.11", "queues are healthy (EVENTS, STEPS, RESULTS, DLQ)", "api GET /queues/stats; docker logs soar-processor-1 --tail 50", func() (string, error) {
			r, err := c.apiWant(http.MethodGet, "/queues/stats", nil, 200)
			if err != nil {
				return "", err
			}
			found := map[string]any{}
			for _, s := range list(r.Body, "streams") {
				found[str(s, "name")] = s
			}
			var p problems
			for _, n := range []string{"EVENTS", "STEPS", "RESULTS", "DLQ"} {
				if found[n] == nil {
					p.add("stream %s is missing", n)
				}
			}
			pending := -1.0
			for _, cons := range list(found["EVENTS"], "consumers") {
				if str(cons, "name") == "processor" {
					pending = num(cons, "pending") + num(cons, "ack_pending")
				}
			}
			switch {
			case pending < 0:
				p.add("EVENTS has no processor consumer")
			case pending > 10000:
				p.add("%v events are waiting for the processor (backlog)", pending)
			}
			return fmt.Sprintf("EVENTS waiting %v, DLQ holds %v message(s)", pending, at(found["DLQ"], "messages")), p.err()
		}},

		{"5.12", "pull source (Wazuh indexer) is healthy", "api GET /sources/<id>/health; docker logs soar-collector-1 --tail 50", func() (string, error) {
			r, err := c.apiWant(http.MethodGet, "/sources?adapter=poll_http", nil, 200)
			if err != nil {
				return "", err
			}
			var src any
			for _, it := range list(r.Body, "items") {
				if str(it, "name") == "wazuh-pull" {
					src = it
				}
			}
			if src == nil {
				return "", skip("no wazuh-pull source (run make seed)")
			}
			if at(src, "enabled") != true {
				return "", skip("wazuh-pull is disabled because no Wazuh indexer is configured (checklist 5.B shows how to test pulling)")
			}
			if at(src, "config", "key_path") == nil {
				return "", fmt.Errorf("wazuh-pull still has the paging config of an older build: run `make sync-pull-source`")
			}
			base, err := c.indexerURL()
			if err != nil {
				return "", err
			}
			if strings.Contains(base, "://opensearch:") {
				// the fake indexer of checklist 5.B: the dev OpenSearch itself
				return c.pullFromFakeIndexer()
			}
			// a real indexer: never write to it, only read the source's health
			h, err := c.apiWant(http.MethodGet, "/sources/"+str(src, "id")+"/health", nil, 200)
			if err != nil {
				return "", err
			}
			if s := str(h.Body, "status"); s != "ok" {
				return "", fmt.Errorf("wazuh-pull health is %q: %s", s, short(h.Raw))
			}
			return "real indexer " + base + ": status ok, last event " + str(h.Body, "last_event_at"), nil
		}},
	}}
}

func (c *client) sourceID(adapter, name string) (string, error) {
	r, err := c.apiWant(http.MethodGet, "/sources?adapter="+adapter, nil, 200)
	if err != nil {
		return "", err
	}
	for _, it := range list(r.Body, "items") {
		if str(it, "name") == name {
			return str(it, "id"), nil
		}
	}
	return "", fmt.Errorf("no %s source named %s (run make seed)", adapter, name)
}

// lastDLQSeq is the sequence number of the newest DLQ message (0 when empty).
func (c *client) lastDLQSeq() (float64, error) {
	r, err := c.apiWant(http.MethodGet, "/dlq?limit=1", nil, 200)
	if err != nil {
		return 0, err
	}
	return num(r.Body, "items", "0", "seq"), nil
}

func firstLine(s string) string {
	s, _, _ = strings.Cut(s, "\n")
	if len(s) > 160 {
		s = s[:160] + "..."
	}
	return s
}

// indexerURL is the base_url of the wazuh_indexer connection that `make seed` creates.
func (c *client) indexerURL() (string, error) {
	r, err := c.apiWant(http.MethodGet, "/connections?limit=200", nil, 200)
	if err != nil {
		return "", err
	}
	for _, it := range list(r.Body, "items") {
		if str(it, "name") == "wazuh_indexer" {
			return str(it, "config", "base_url"), nil
		}
	}
	return "", fmt.Errorf("wazuh-pull is enabled but there is no wazuh_indexer connection (set SOAR_WAZUH_INDEXER_URL, then make seed)")
}

const indexerFixture = "integrations/wazuh/testdata/06_indexer_hit_fim_modified.json"

// pullFromFakeIndexer writes 20 Wazuh indexer documents into today's wazuh-alerts index
// of the dev OpenSearch and waits until the collector has pulled every one of them.
func (c *client) pullFromFakeIndexer() (string, error) {
	raw, err := os.ReadFile(indexerFixture)
	if err != nil {
		return "", fmt.Errorf("read %s (run from the repository root): %w", indexerFixture, err)
	}
	var hit struct {
		Source map[string]any `json:"_source"`
	}
	if err := json.Unmarshal(raw, &hit); err != nil {
		return "", err
	}
	now := time.Now().UTC()
	index := "wazuh-alerts-4.x-" + now.Format("2006.01.02")
	mapping := `{"settings":{"number_of_replicas":0},"mappings":{"date_detection":false,"properties":{"@timestamp":{"type":"date"},"id":{"type":"keyword"},"timestamp":{"type":"keyword"}}}}`
	if r, err := c.send(http.MethodPut, c.osURL+"/"+index, []byte(mapping), nil); err != nil {
		return "", err
	} else if r.Status != 200 && !strings.Contains(r.Raw, "resource_already_exists") {
		return "", fmt.Errorf("create %s: HTTP %d %s", index, r.Status, short(r.Raw))
	}
	const n = 20
	prefix := "stepcheck." + c.run + "."
	var bulk strings.Builder
	for i := 0; i < n; i++ {
		doc := map[string]any{}
		_ = json.Unmarshal(raw, &hit) // a fresh copy of the fixture per document
		for k, v := range hit.Source {
			doc[k] = v
		}
		id := fmt.Sprintf("%s%d", prefix, i)
		ts := now.Add(time.Duration(i-n) * time.Second)
		doc["id"] = id
		doc["@timestamp"] = ts.Format(time.RFC3339Nano)
		doc["timestamp"] = ts.Format("2006-01-02T15:04:05.000-0700")
		if sc, ok := doc["syscheck"].(map[string]any); ok {
			sc["path"] = "/etc/stepcheck/" + id // a distinct file per document: no grouping
		}
		meta, _ := json.Marshal(map[string]any{"index": map[string]any{"_index": index, "_id": id}})
		body, _ := json.Marshal(doc)
		bulk.Write(meta)
		bulk.WriteByte('\n')
		bulk.Write(body)
		bulk.WriteByte('\n')
	}
	r, err := c.send(http.MethodPost, c.osURL+"/_bulk?refresh=true", []byte(bulk.String()), nil)
	if err != nil {
		return "", err
	}
	if r.Status != 200 || at(r.Body, "errors") != false {
		return "", fmt.Errorf("bulk write to %s: HTTP %d %s", index, r.Status, short(r.Raw))
	}
	t0 := time.Now()
	got := 0
	err = waitFor(90*time.Second, fmt.Sprintf("%d pulled alerts (the source polls every 15 s)", n), func() (bool, error) {
		r, err := c.apiWant(http.MethodGet, "/alerts?sort=-received_at&limit=200", nil, 200)
		if err != nil {
			return false, err
		}
		got = 0
		for _, it := range list(r.Body, "items") {
			if strings.HasPrefix(str(it, "external_id"), prefix) {
				got++
			}
		}
		return got == n, fmt.Errorf("%d of %d pulled", got, n)
	})
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("fake indexer: %d documents written to %s, all %d pulled in %.0fs", n, index, got, time.Since(t0).Seconds()), nil
}
