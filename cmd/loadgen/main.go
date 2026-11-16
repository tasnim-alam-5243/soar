// Command loadgen is the Step 5 exit-gate tool: it sends unique Wazuh-style alerts to the
// ingest endpoint at a fixed rate, waits for the pipeline to drain, then proves in
// OpenSearch that every acknowledged event was stored exactly once.
//
//	make loadtest RATE=1000 DURATION=30m     # the 30-minute peak test
//	make loadtest-burst                      # 2,000/s for 60 s
package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

type config struct {
	ingestURL, apiURL, osURL, source, token string
	indexPrefix, apiKey                     string
	rate, workers, batch, groups            int
	duration, drainTimeout                  time.Duration
	verify                                  bool
}

func main() {
	var c config
	flag.StringVar(&c.ingestURL, "ingest-url", "http://127.0.0.1:8081", "ingest base URL")
	flag.StringVar(&c.apiURL, "api-url", "http://127.0.0.1:8080", "API base URL (queue statistics)")
	flag.StringVar(&c.osURL, "os-url", "http://127.0.0.1:9200", "OpenSearch URL (verification)")
	flag.StringVar(&c.indexPrefix, "index-prefix", envOr("SOAR_INDEX_PREFIX", "soar"), "index prefix of the stack under test (SOAR_INDEX_PREFIX)")
	flag.StringVar(&c.apiKey, "api-key", envOr("SOAR_API_KEY", envOr("SOAR_BOOTSTRAP_API_KEY", "soar_dev-bootstrap-key-change-me-0123456789abcdef")), "API key for the queue statistics (SOAR_API_KEY; default: the Compose development key)")
	flag.StringVar(&c.source, "source", "wazuh-prod", "source name")
	flag.StringVar(&c.token, "token", envOr("SOAR_SOURCE_TOKEN_WAZUH_PROD", "dev-wazuh-token-change-me"), "source token")
	flag.IntVar(&c.rate, "rate", 1000, "events per second")
	flag.IntVar(&c.workers, "workers", 128, "concurrent HTTP workers")
	flag.IntVar(&c.batch, "batch", 1, "events per request (Wazuh sends 1; the endpoint accepts up to 500)")
	flag.IntVar(&c.groups, "groups", 200, "distinct alert groups (repeats of the same alert collapse into one document)")
	flag.DurationVar(&c.duration, "duration", 60*time.Second, "how long to send")
	flag.DurationVar(&c.drainTimeout, "drain-timeout", 10*time.Minute, "how long to wait for the queue to drain")
	flag.BoolVar(&c.verify, "verify", true, "check OpenSearch for exactly-once storage")
	flag.Parse()
	os.Exit(run(c))
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

// event builds the n-th unique alert of a run. Every event has a unique Wazuh id, so the
// duplicate guard never drops it; events share fingerprints (rule/agent/srcip), so they
// group into at most `groups` alert documents per 10-minute window.
func event(run string, n, groups int, now time.Time) map[string]any {
	g := n % groups
	return map[string]any{
		"timestamp": now.Format("2006-01-02T15:04:05.000-0700"),
		"id":        fmt.Sprintf("load.%s.%d", run, n),
		"rule":      map[string]any{"level": 5, "id": fmt.Sprint(100000 + g), "description": "sshd: authentication success.", "groups": []string{"syslog", "sshd"}},
		"agent":     map[string]any{"id": "900", "name": "load-" + run},
		"decoder":   map[string]any{"name": "sshd"},
		"data":      map[string]any{"srcip": fmt.Sprintf("10.%d.%d.%d", (g>>16)&255, (g>>8)&255, g&255), "dstuser": "root"},
		"full_log":  "Accepted password for root from 10.0.0.1 port 51234 ssh2",
	}
}

func percentile(sorted []time.Duration, p float64) time.Duration {
	if len(sorted) == 0 {
		return 0
	}
	i := int(float64(len(sorted)-1) * p)
	return sorted[i]
}

type result struct {
	sent, accepted, filtered, failedRecords int64
	status                                  map[int]int64
	latencies                               []time.Duration
	errs                                    int64
}

func send(ctx context.Context, c config, run string) result {
	total := c.rate * int(c.duration/time.Second)
	requests := (total + c.batch - 1) / c.batch
	client := &http.Client{Transport: &http.Transport{MaxIdleConnsPerHost: c.workers, MaxConnsPerHost: c.workers}, Timeout: 30 * time.Second}
	url := c.ingestURL + "/ingest/" + c.source

	var next atomic.Int64
	var mu sync.Mutex
	res := result{status: map[int]int64{}}
	start := time.Now()
	var wg sync.WaitGroup
	for w := 0; w < c.workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			var lat []time.Duration
			local := map[int]int64{}
			var sent, accepted, filtered, failed, errs int64
			for {
				r := int(next.Add(1) - 1)
				if r >= requests || ctx.Err() != nil {
					break
				}
				// open-loop pacing: request r is due at start + r*batch/rate, regardless of how slow earlier ones were
				due := start.Add(time.Duration(float64(r*c.batch) / float64(c.rate) * float64(time.Second)))
				if d := time.Until(due); d > 0 {
					time.Sleep(d)
				}
				n0 := r * c.batch
				count := min(c.batch, total-n0)
				now := time.Now()
				var body []byte
				if count == 1 {
					body, _ = json.Marshal(event(run, n0, c.groups, now))
				} else {
					evs := make([]map[string]any, count)
					for i := range evs {
						evs[i] = event(run, n0+i, c.groups, now)
					}
					body, _ = json.Marshal(evs)
				}
				req, _ := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
				req.Header.Set("Content-Type", "application/json")
				req.Header.Set("Authorization", "Bearer "+c.token)
				t0 := time.Now()
				resp, err := client.Do(req)
				lat = append(lat, time.Since(t0))
				sent += int64(count)
				if err != nil {
					errs++
					continue
				}
				var out struct {
					EventIDs []string `json:"event_ids"`
					Filtered int      `json:"filtered"`
					Failed   int      `json:"failed"`
				}
				raw, _ := io.ReadAll(resp.Body)
				resp.Body.Close()
				local[resp.StatusCode]++
				if resp.StatusCode == http.StatusAccepted {
					_ = json.Unmarshal(raw, &out)
					accepted += int64(len(out.EventIDs))
					filtered += int64(out.Filtered)
					failed += int64(out.Failed)
				}
			}
			mu.Lock()
			res.latencies = append(res.latencies, lat...)
			for k, v := range local {
				res.status[k] += v
			}
			res.sent += sent
			res.accepted += accepted
			res.filtered += filtered
			res.failedRecords += failed
			res.errs += errs
			mu.Unlock()
		}()
	}
	wg.Wait()
	return res
}

func getJSON(ctx context.Context, url string, body any, out any, headers ...string) error {
	var rd io.Reader
	method := http.MethodGet
	if body != nil {
		b, _ := json.Marshal(body)
		rd, method = bytes.NewReader(b), http.MethodPost
	}
	req, _ := http.NewRequestWithContext(ctx, method, url, rd)
	req.Header.Set("Content-Type", "application/json")
	for i := 0; i+1 < len(headers); i += 2 {
		req.Header.Set(headers[i], headers[i+1])
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 300 {
		return fmt.Errorf("%s: HTTP %d: %s", url, resp.StatusCode, strings.TrimSpace(string(raw)))
	}
	return json.Unmarshal(raw, out)
}

// drain waits until the events stream is empty and the processor has nothing in flight.
func drain(ctx context.Context, c config) (time.Duration, error) {
	start := time.Now()
	deadline := start.Add(c.drainTimeout)
	for time.Now().Before(deadline) {
		var st struct {
			Streams []struct {
				Name      string `json:"name"`
				Messages  uint64 `json:"messages"`
				Consumers []struct {
					Pending    uint64 `json:"pending"`
					AckPending int    `json:"ack_pending"`
				} `json:"consumers"`
			} `json:"streams"`
		}
		var auth []string
		if c.apiKey != "" {
			auth = []string{"Authorization", "Bearer " + c.apiKey}
		}
		if err := getJSON(ctx, c.apiURL+"/api/v1/queues/stats", nil, &st, auth...); err != nil {
			return 0, err
		}
		empty := false
		for _, s := range st.Streams {
			if s.Name == "EVENTS" {
				empty = s.Messages == 0
				for _, cs := range s.Consumers {
					empty = empty && cs.Pending == 0 && cs.AckPending == 0
				}
			}
		}
		if empty {
			return time.Since(start), nil
		}
		time.Sleep(time.Second)
	}
	return 0, fmt.Errorf("the queue did not drain within %s", c.drainTimeout)
}

type verification struct {
	occurrences, seen int64
	alertDocs         int64
}

func verify(ctx context.Context, c config, run string) (verification, error) {
	var v verification
	for _, idx := range []string{c.indexPrefix + "-alerts", c.indexPrefix + "-seen"} {
		if err := getJSON(ctx, c.osURL+"/"+idx+"/_refresh", map[string]any{}, &map[string]any{}); err != nil {
			return v, err
		}
	}
	var a struct {
		Hits struct {
			Total struct{ Value int64 } `json:"total"`
		} `json:"hits"`
		Aggregations struct {
			Sum struct{ Value float64 } `json:"occ"`
		} `json:"aggregations"`
	}
	q := map[string]any{
		"size": 0, "track_total_hits": true, "query": map[string]any{"term": map[string]any{"entities.hosts": "load-" + run}},
		"aggs": map[string]any{"occ": map[string]any{"sum": map[string]any{"field": "occurrence_count"}}},
	}
	if err := getJSON(ctx, c.osURL+"/"+c.indexPrefix+"-alerts/_search", q, &a); err != nil {
		return v, err
	}
	v.alertDocs, v.occurrences = a.Hits.Total.Value, int64(a.Aggregations.Sum.Value)
	var s struct {
		Count int64 `json:"count"`
	}
	sq := map[string]any{"query": map[string]any{"prefix": map[string]any{"key": c.source + ":load." + run + "."}}}
	if err := getJSON(ctx, c.osURL+"/"+c.indexPrefix+"-seen/_count", sq, &s); err != nil {
		return v, err
	}
	v.seen = s.Count
	return v, nil
}

func run(c config) int {
	ctx := context.Background()
	rb := make([]byte, 3)
	_, _ = rand.Read(rb)
	runID := hex.EncodeToString(rb)
	fmt.Printf("run %s: %d events/s for %s (%d per request, %d workers) -> %s\n", runID, c.rate, c.duration, c.batch, c.workers, c.ingestURL)

	t0 := time.Now()
	res := send(ctx, c, runID)
	elapsed := time.Since(t0)
	sort.Slice(res.latencies, func(i, j int) bool { return res.latencies[i] < res.latencies[j] })
	p50, p95, p99 := percentile(res.latencies, .50), percentile(res.latencies, .95), percentile(res.latencies, .99)
	maxLat := time.Duration(0)
	if n := len(res.latencies); n > 0 {
		maxLat = res.latencies[n-1]
	}
	var five, tooMany int64
	for code, n := range res.status {
		if code >= 500 {
			five += n
		}
		if code == 429 {
			tooMany += n
		}
	}
	fmt.Printf("sent %d events in %s (%.0f/s achieved); accepted(202) %d; filtered %d; failed records %d\n",
		res.sent, elapsed.Round(time.Millisecond), float64(res.sent)/elapsed.Seconds(), res.accepted, res.filtered, res.failedRecords)
	fmt.Printf("responses: %v; connection errors: %d\n", res.status, res.errs)
	fmt.Printf("latency (client side, to 202): p50 %s  p95 %s  p99 %s  max %s\n", p50.Round(time.Microsecond), p95.Round(time.Microsecond), p99.Round(time.Microsecond), maxLat.Round(time.Microsecond))

	if achieved := float64(res.sent) / elapsed.Seconds(); achieved < float64(c.rate)*0.9 {
		fmt.Printf("note: only %.0f events/s were achieved of %d requested. Something on the path is saturated: latency p50 x concurrency\n"+
			"      ~ workers/throughput points at a serial resource. Check NATS first (fsync on every write: `make nats-relaxed`), then CPU\n"+
			"      and disk (`docker stats`, `iostat -x 1`), then processor/OpenSearch lag (`curl localhost:8080/api/v1/queues/stats`).\n", achieved, c.rate)
	}
	failed := false
	check := func(name string, ok bool, detail string) {
		mark := "PASS"
		if !ok {
			mark, failed = "FAIL", true
		}
		fmt.Printf("  [%s] %s %s\n", mark, name, detail)
	}
	fmt.Println("gates:")
	check("no 5xx and no connection errors", five == 0 && res.errs == 0, fmt.Sprintf("(5xx=%d errors=%d)", five, res.errs))
	check("no rate-limit rejections", tooMany == 0, fmt.Sprintf("(429=%d; raise the source rate_limit if this fails)", tooMany))
	check("p99 below 100 ms", p99 < 100*time.Millisecond, fmt.Sprintf("(%s)", p99.Round(time.Microsecond)))
	check("every event accepted", res.accepted == res.sent && res.failedRecords == 0, fmt.Sprintf("(%d of %d)", res.accepted, res.sent))

	if c.verify {
		d, err := drain(ctx, c)
		if err != nil {
			check("queue drains", false, err.Error())
		} else {
			check("queue drained", true, fmt.Sprintf("(%s after the last send)", d.Round(time.Second)))
			v, err := verify(ctx, c, runID)
			if err != nil {
				check("verification query", false, err.Error())
			} else {
				fmt.Printf("stored: %d alert documents carrying %d occurrences; %d duplicate-guard keys\n", v.alertDocs, v.occurrences, v.seen)
				check("zero loss: occurrences == accepted events", v.occurrences == res.accepted, fmt.Sprintf("(%d vs %d)", v.occurrences, res.accepted))
				check("each event guarded exactly once", v.seen == res.accepted, fmt.Sprintf("(%d vs %d)", v.seen, res.accepted))
			}
		}
	}
	if failed {
		fmt.Println("RESULT: FAIL")
		return 1
	}
	fmt.Println("RESULT: PASS")
	return 0
}
