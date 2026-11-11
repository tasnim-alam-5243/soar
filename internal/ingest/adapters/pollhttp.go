package adapters

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/yourorg/soar/internal/domain"
	"github.com/yourorg/soar/internal/expr"
)

func init() { Register("poll_http", NewHTTPPoller) }

// pollConfig is the poll_http part of Source.Config (see integrations/wazuh/sources/wazuh-pull.yaml).
type pollConfig struct {
	method, path        string
	indexPrefix         string
	indexDays           int
	body                map[string]any
	itemsPath, itemPath string
	keyPath             string // per-hit sort key (OpenSearch: "sort"), enables keyset paging
	cursorField         string
	idField             string
	pageSize            int
}

// HTTPPoller polls a JSON search API with a time cursor. For the Wazuh indexer it lists
// the last few daily indices instead of a wildcard, which would touch every shard.
//
// String values in request.body that start with "cel:" are CEL expressions over
// cursor.start, cursor.end (RFC 3339), cursor.order ("asc" or "desc") and cursor.after
// (the previous page's sort key, or null). A map entry whose expression yields null is
// left out, so `search_after: 'cel:cursor.after'` disappears on the first page.
type HTTPPoller struct {
	name   string
	cfg    pollConfig
	base   string
	user   string
	pass   string
	client *http.Client
	eng    *expr.Engine

	mu    sync.Mutex
	progs map[string]*expr.Program
	// primarySortOnly is set when the server rejected the secondary sort key (spec 10.1:
	// Wazuh's id may not be sortable). Paging then uses the cursor field alone and ties
	// at page boundaries are covered by the overlap and the duplicate guard.
	primarySortOnly atomic.Bool
}

func asMap(v any) map[string]any { m, _ := v.(map[string]any); return m }

func asInt(v any, def int) int {
	switch t := v.(type) {
	case float64:
		return int(t)
	case int:
		return t
	}
	return def
}

// NewHTTPPoller builds the poll_http adapter.
func NewHTTPPoller(src domain.Source, conn domain.Connection, resolve func(string) (string, error)) (Poller, error) {
	c := src.Config
	req := asMap(c["request"])
	if req == nil {
		return nil, fmt.Errorf("source %s: config.request is required", src.Name)
	}
	cfg := pollConfig{method: http.MethodGet, body: asMap(req["body"])}
	if m, _ := req["method"].(string); m != "" {
		cfg.method = strings.ToUpper(m)
	}
	cfg.path, _ = req["path"].(string)
	if cfg.path == "" {
		return nil, fmt.Errorf("source %s: config.request.path is required", src.Name)
	}
	if iw := asMap(req["index_window"]); iw != nil {
		cfg.indexPrefix, _ = iw["prefix"].(string)
		cfg.indexDays = asInt(iw["days"], 2)
	}
	cfg.itemsPath, _ = c["items_path"].(string)
	cfg.itemPath, _ = c["item_path"].(string)
	cfg.keyPath, _ = c["key_path"].(string)
	cur := asMap(c["cursor"])
	cfg.cursorField, _ = cur["field"].(string)
	if cfg.cursorField == "" {
		return nil, fmt.Errorf("source %s: config.cursor.field is required", src.Name)
	}
	cfg.idField, _ = cur["id_field"].(string)
	if cfg.idField == "" {
		cfg.idField = "id"
	}
	cfg.pageSize = asInt(cfg.body["size"], 500)

	base, _ := conn.Config["base_url"].(string)
	if base == "" {
		return nil, fmt.Errorf("connection %s: config.base_url is required", conn.Name)
	}
	p := &HTTPPoller{name: src.Name, cfg: cfg, base: strings.TrimRight(base, "/"), progs: map[string]*expr.Program{}}
	for _, k := range []string{"username", "password"} {
		if ref, ok := conn.SecretRefs[k].(string); ok && ref != "" {
			v, err := resolve(ref)
			if err != nil {
				return nil, fmt.Errorf("connection %s: %w", conn.Name, err)
			}
			if k == "username" {
				p.user = v
			} else {
				p.pass = v
			}
		}
	}

	tr := &http.Transport{MaxIdleConnsPerHost: 2, TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS12}}
	if ca, _ := conn.Config["ca_file"].(string); ca != "" {
		pem, err := os.ReadFile(ca)
		if err != nil {
			return nil, fmt.Errorf("connection %s: read CA file: %w", conn.Name, err)
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(pem) {
			return nil, fmt.Errorf("connection %s: CA file has no valid certificates", conn.Name)
		}
		tr.TLSClientConfig.RootCAs = pool
	}
	if insecure, _ := conn.Config["insecure_skip_verify"].(bool); insecure {
		slog.Warn("TLS certificate verification is DISABLED for this connection; use a CA file outside development", "connection", conn.Name)
		tr.TLSClientConfig.InsecureSkipVerify = true //nolint:gosec // explicit dev-only setting, logged above
	}
	p.client = &http.Client{Transport: tr, Timeout: 60 * time.Second}
	eng, err := expr.New(nil, "cursor")
	if err != nil {
		return nil, err
	}
	p.eng = eng
	return p, nil
}

func (p *HTTPPoller) program(src string) (*expr.Program, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if prg := p.progs[src]; prg != nil {
		return prg, nil
	}
	prg, err := p.eng.Compile(src)
	if err != nil {
		return nil, fmt.Errorf("request body expression %q: %w", src, err)
	}
	p.progs[src] = prg
	return prg, nil
}

// render deep-copies the body, evaluating strings that start with "cel:". A map entry
// whose expression evaluates to null is dropped.
func (p *HTTPPoller) render(v any, vars map[string]any) (any, error) {
	switch t := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(t))
		for k, x := range t {
			r, err := p.render(x, vars)
			if err != nil {
				return nil, err
			}
			if s, isStr := x.(string); r == nil && isStr && strings.HasPrefix(s, "cel:") {
				continue
			}
			out[k] = r
		}
		return out, nil
	case []any:
		out := make([]any, len(t))
		for i, x := range t {
			r, err := p.render(x, vars)
			if err != nil {
				return nil, err
			}
			out[i] = r
		}
		return out, nil
	case string:
		src, ok := strings.CutPrefix(t, "cel:")
		if !ok {
			return t, nil
		}
		prg, err := p.program(src)
		if err != nil {
			return nil, err
		}
		return prg.Eval(vars)
	}
	return v, nil
}

func (p *HTTPPoller) indices(start, now time.Time) string {
	if p.cfg.indexPrefix == "" {
		return ""
	}
	days := p.cfg.indexDays
	if need := int(now.Sub(start).Hours()/24) + 2; need > days {
		days = min(need, 31) // catching up after downtime must still reach the right indices
	}
	names := make([]string, 0, days)
	for d := 0; d < days; d++ {
		names = append(names, p.cfg.indexPrefix+now.UTC().AddDate(0, 0, -d).Format("2006.01.02"))
	}
	return strings.Join(names, ",")
}

func (p *HTTPPoller) sortKeys() int {
	s, _ := p.cfg.body["sort"].([]any)
	return len(s)
}

// Page implements Poller.
func (p *HTTPPoller) Page(ctx context.Context, q Query) (Page, error) {
	trim := p.primarySortOnly.Load()
	pg, status, err := p.fetch(ctx, q, trim)
	if err != nil && status == http.StatusBadRequest && !trim && p.sortKeys() > 1 {
		// The secondary sort key may not be sortable on this cluster (spec 10.1). Retry
		// with the cursor field alone; remember it only if that works.
		if pg2, _, err2 := p.fetch(ctx, q, true); err2 == nil {
			p.primarySortOnly.Store(true)
			slog.Warn("the source rejected the secondary sort key; paging by the cursor field only (the overlap and the duplicate guard cover ties)",
				"source", p.name, "error", err.Error())
			return pg2, nil
		}
	}
	return pg, err
}

func (p *HTTPPoller) fetch(ctx context.Context, q Query, primaryOnly bool) (Page, int, error) {
	now := q.Now
	if now.IsZero() {
		now = time.Now()
	}
	end := q.To
	if end.IsZero() {
		end = now.Add(24 * time.Hour) // open upper bound, generous for clock skew between hosts
	}
	order := "asc"
	if q.Newest {
		order = "desc"
	}
	var after any
	if len(q.After) > 0 {
		a := q.After
		if primaryOnly && len(a) > 1 {
			a = a[:1]
		}
		after = a
	}
	vars := map[string]any{"cursor": map[string]any{
		"start": q.From.UTC().Format(time.RFC3339Nano), "end": end.UTC().Format(time.RFC3339Nano), "order": order, "after": after,
	}}
	body, err := p.render(p.cfg.body, vars)
	if err != nil {
		return Page{}, 0, err
	}
	if m, ok := body.(map[string]any); ok && primaryOnly {
		if s, ok := m["sort"].([]any); ok && len(s) > 1 {
			m["sort"] = s[:1]
		}
	}
	var rd io.Reader
	if body != nil && p.cfg.method != http.MethodGet {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	path := strings.ReplaceAll(p.cfg.path, "{index}", p.indices(q.From, now))
	req, err := http.NewRequestWithContext(ctx, p.cfg.method, p.base+path, rd)
	if err != nil {
		return Page{}, 0, err
	}
	req.Header.Set("Content-Type", "application/json")
	if p.user != "" {
		req.SetBasicAuth(p.user, p.pass)
	}
	resp, err := p.client.Do(req)
	if err != nil {
		return Page{}, 0, fmt.Errorf("poll: %w", err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
	if err != nil {
		return Page{}, resp.StatusCode, err
	}
	if resp.StatusCode != http.StatusOK {
		msg := string(raw)
		if len(msg) > 300 {
			msg = msg[:300]
		}
		return Page{}, resp.StatusCode, fmt.Errorf("poll: HTTP %d: %s", resp.StatusCode, msg)
	}
	var doc any
	if err := json.Unmarshal(raw, &doc); err != nil {
		return Page{}, resp.StatusCode, fmt.Errorf("poll: response is not JSON: %w", err)
	}
	list, _ := expr.GetPath(doc, p.cfg.itemsPath).([]any)
	pg := Page{Full: len(list) >= p.cfg.pageSize && p.cfg.pageSize > 0}
	for i, hit := range list {
		if p.cfg.keyPath != "" && i == len(list)-1 {
			if k, ok := expr.GetPath(hit, p.cfg.keyPath).([]any); ok && len(k) > 0 {
				pg.LastKey = k
			}
		}
		it := hit
		if p.cfg.itemPath != "" {
			it = expr.GetPath(hit, p.cfg.itemPath)
		}
		m, ok := it.(map[string]any)
		if !ok {
			continue
		}
		if ts, ok := expr.GetPath(m, p.cfg.cursorField).(string); ok {
			if t, err := time.Parse(time.RFC3339Nano, ts); err == nil && t.After(pg.Last) {
				pg.Last = t
			}
		}
		pg.Items = append(pg.Items, m)
		pg.IDs = append(pg.IDs, idOf(m[p.cfg.idField]))
	}
	return pg, resp.StatusCode, nil
}

func idOf(v any) string {
	switch t := v.(type) {
	case nil:
		return ""
	case string:
		return t
	default:
		return fmt.Sprint(t)
	}
}
