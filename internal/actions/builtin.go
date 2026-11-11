package actions

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/yourorg/soar/internal/cases"
	"github.com/yourorg/soar/internal/domain"
	"github.com/yourorg/soar/internal/store"
)

// Deps are what built-in actions need from the platform.
type Deps struct {
	Store *store.Store
	Log   *slog.Logger
	HTTP  HTTPOptions
}

// RegisterBuiltins adds every built-in action of spec section 10.
func RegisterBuiltins(r *Registry, d Deps) error {
	if d.Log == nil {
		d.Log = slog.Default()
	}
	list := []Action{NewHTTPRequest(d.HTTP), transformMap(), delaySleep(), notifyLog(d.Log)}
	if d.Store != nil {
		cs := cases.New(d.Store, d.Log)
		list = append(list, caseCreate(cs), caseUpdate(cs), caseClose(cs, d.Store), observableAdd(cs), alertUpdate(d.Store))
	}
	for _, a := range list {
		if err := r.Register(a); err != nil {
			return err
		}
	}
	return nil
}

func obj(props map[string]any, required ...string) map[string]any {
	s := map[string]any{"type": "object", "properties": props, "additionalProperties": false}
	if len(required) > 0 {
		req := make([]any, len(required))
		for i, r := range required {
			req[i] = r
		}
		s["required"] = req
	}
	return s
}

func str() map[string]any         { return map[string]any{"type": "string"} }
func strMax(n int) map[string]any { return map[string]any{"type": "string", "maxLength": n} }
func num(min, max float64) map[string]any {
	return map[string]any{"type": "number", "minimum": min, "maximum": max}
}

func intRange(min, max int) map[string]any {
	return map[string]any{"type": "integer", "minimum": min, "maximum": max}
}
func strList() map[string]any { return map[string]any{"type": "array", "items": str()} }
func enum(vals ...string) map[string]any {
	e := make([]any, len(vals))
	for i, v := range vals {
		e[i] = v
	}
	return map[string]any{"type": "string", "enum": e}
}

// ---- http.request ----

// HTTPOptions tune http.request. The SSRF guard blocks loopback, link-local (which
// includes the cloud metadata address 169.254.169.254) and unspecified addresses
// unless AllowLoopback is set (tests, or a deliberate local integration).
type HTTPOptions struct {
	AllowLoopback bool
	MaxBodyBytes  int64 // response bytes kept (default 1 MiB)
}

var metadataIPs = []net.IP{net.ParseIP("100.100.100.200"), net.ParseIP("fd00:ec2::254")}

// BlockedIP reports whether the SSRF guard refuses to connect to ip.
func BlockedIP(ip net.IP, allowLoopback bool) bool {
	if ip.IsLoopback() {
		return !allowLoopback
	}
	if ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() || ip.IsUnspecified() || ip.IsMulticast() {
		return true
	}
	for _, m := range metadataIPs {
		if ip.Equal(m) {
			return true
		}
	}
	return false
}

// SafeTransport returns an HTTP transport whose dialer re-checks every resolved
// address, including those reached through redirects and DNS rebinding. roots, when
// not nil, replaces the system CA pool (private CAs); certificates are always verified.
func SafeTransport(allowLoopback bool, roots *x509.CertPool) *http.Transport {
	dialer := &net.Dialer{
		Timeout: 10 * time.Second, KeepAlive: 30 * time.Second,
		Control: func(_, address string, _ syscall.RawConn) error {
			host, _, err := net.SplitHostPort(address)
			if err != nil {
				return err
			}
			ip := net.ParseIP(host)
			if ip == nil || BlockedIP(ip, allowLoopback) {
				return &blockedError{addr: host}
			}
			return nil
		},
	}
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.DialContext = dialer.DialContext
	tr.Proxy = nil // a proxy would bypass the address check
	if roots != nil {
		tr.TLSClientConfig = &tls.Config{RootCAs: roots, MinVersion: tls.VersionTLS12}
	}
	return tr
}

type blockedError struct{ addr string }

func (e *blockedError) Error() string {
	return "destination " + e.addr + " is blocked (loopback, link-local and metadata addresses are not allowed)"
}

// NewHTTPRequest builds the http.request action.
func NewHTTPRequest(o HTTPOptions) Action {
	if o.MaxBodyBytes <= 0 {
		o.MaxBodyBytes = 1 << 20
	}
	safe := &http.Client{Transport: SafeTransport(o.AllowLoopback, nil), CheckRedirect: limitRedirects}
	m := Manifest{
		ID: "http.request", Version: 1, Title: "HTTP request",
		Description: "Send an HTTP request; returns status, headers, body (JSON bodies are parsed). 429 and 5xx responses are retryable errors; other 4xx fail unless fail_on_status is false.",
		SideEffect:  "read", Risk: "low", SupportsDryRun: true, Class: ClassBuiltin,
		ParamsSchema: obj(map[string]any{
			"method":         enum("GET", "POST", "PUT", "PATCH", "DELETE", "HEAD"),
			"url":            map[string]any{"type": "string", "format": "uri"},
			"headers":        map[string]any{"type": "object", "additionalProperties": str()},
			"query":          map[string]any{"type": "object"},
			"body":           map[string]any{},
			"json":           map[string]any{},
			"fail_on_status": map[string]any{"type": "boolean"},
			"ca_pem":         map[string]any{"type": "string", "maxLength": 65536},
		}, "url"),
		OutputSchema: obj(map[string]any{
			"status": map[string]any{"type": "integer"}, "headers": map[string]any{"type": "object"}, "body": map[string]any{},
			"dry_run": map[string]any{"type": "boolean"}, "would_send": map[string]any{"type": "object"}, "truncated": map[string]any{"type": "boolean"},
		}),
		Secrets:         []string{"auth_token"},
		RetryableErrors: []string{"network", "timeout", "http_5xx", "http_429"},
		Limits:          Limits{CPU: 0.5, MemoryMB: 128, TimeoutS: 60, Egress: []string{"*"}},
	}
	return Func{M: m, Fn: func(ctx context.Context, in Input) (any, error) {
		p := in.Params
		method, _ := p["method"].(string)
		if method == "" {
			method = http.MethodGet
		}
		rawURL, _ := p["url"].(string)
		u, err := url.Parse(rawURL)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
			return nil, Errf("invalid_params", false, "url must be an absolute http(s) URL")
		}
		if q, ok := p["query"].(map[string]any); ok {
			vals := u.Query()
			for k, v := range q {
				vals.Set(k, fmt.Sprint(v))
			}
			u.RawQuery = vals.Encode()
		}
		if in.DryRun && method != http.MethodGet && method != http.MethodHead {
			return map[string]any{"dry_run": true, "would_send": map[string]any{"method": method, "url": redactURL(u)}}, nil
		}
		var body io.Reader
		contentType := ""
		switch {
		case p["json"] != nil:
			b, _ := json.Marshal(p["json"])
			body, contentType = bytes.NewReader(b), "application/json"
		case p["body"] != nil:
			switch b := p["body"].(type) {
			case string:
				body = strings.NewReader(b)
			default:
				enc, _ := json.Marshal(b)
				body, contentType = bytes.NewReader(enc), "application/json"
			}
		}
		req, err := http.NewRequestWithContext(ctx, method, u.String(), body)
		if err != nil {
			return nil, Errf("invalid_params", false, "%v", err)
		}
		req.Header.Set("User-Agent", "soar-http-action/1")
		if contentType != "" {
			req.Header.Set("Content-Type", contentType)
		}
		if h, ok := p["headers"].(map[string]any); ok {
			for k, v := range h {
				req.Header.Set(k, fmt.Sprint(v))
			}
		}
		client := safe
		if pem, _ := p["ca_pem"].(string); pem != "" {
			pool := x509.NewCertPool()
			if !pool.AppendCertsFromPEM([]byte(pem)) {
				return nil, Errf("invalid_params", false, "ca_pem holds no PEM certificate")
			}
			tr := SafeTransport(o.AllowLoopback, pool)
			defer tr.CloseIdleConnections()
			client = &http.Client{Transport: tr, CheckRedirect: limitRedirects}
		}
		resp, err := client.Do(req)
		if err != nil {
			var be *blockedError
			switch {
			case errors.As(err, &be):
				return nil, Errf("ssrf_blocked", false, "%s", be.Error())
			case errors.Is(err, context.DeadlineExceeded) || isTimeout(err):
				return nil, Errf("timeout", true, "request timed out")
			}
			return nil, Errf("network", true, "%s", scrubURLFromError(err.Error(), u))
		}
		defer resp.Body.Close()
		data, _ := io.ReadAll(io.LimitReader(resp.Body, o.MaxBodyBytes+1))
		truncated := int64(len(data)) > o.MaxBodyBytes
		if truncated {
			data = data[:o.MaxBodyBytes]
		}
		out := map[string]any{"status": resp.StatusCode, "headers": firstValues(resp.Header), "body": decodeBody(resp.Header.Get("Content-Type"), data)}
		if truncated {
			out["truncated"] = true
		}
		failOnStatus := true
		if v, ok := p["fail_on_status"].(bool); ok {
			failOnStatus = v
		}
		switch {
		case resp.StatusCode == http.StatusTooManyRequests:
			return nil, Errf("http_429", true, "rate limited (Retry-After: %s)", resp.Header.Get("Retry-After"))
		case resp.StatusCode >= 500:
			return nil, Errf("http_5xx", true, "server error HTTP %d", resp.StatusCode)
		case resp.StatusCode >= 400 && failOnStatus:
			return nil, Errf("http_"+strconv.Itoa(resp.StatusCode), false, "HTTP %d: %s", resp.StatusCode, snippet(data))
		}
		return out, nil
	}}
}

func limitRedirects(req *http.Request, via []*http.Request) error {
	if len(via) >= 5 {
		return errors.New("stopped after 5 redirects")
	}
	return nil
}

func isTimeout(err error) bool {
	var ne net.Error
	return errors.As(err, &ne) && ne.Timeout()
}

func redactURL(u *url.URL) string {
	c := *u
	c.User = nil
	q := c.Query()
	for k := range q {
		lk := strings.ToLower(k)
		if strings.Contains(lk, "key") || strings.Contains(lk, "token") || strings.Contains(lk, "secret") || strings.Contains(lk, "password") {
			q.Set(k, "REDACTED")
		}
	}
	c.RawQuery = q.Encode()
	return c.String()
}

func scrubURLFromError(msg string, u *url.URL) string {
	return strings.ReplaceAll(msg, u.String(), redactURL(u))
}

func firstValues(h http.Header) map[string]any {
	out := make(map[string]any, len(h))
	for k, v := range h {
		if len(v) > 0 {
			out[k] = v[0]
		}
	}
	return out
}

func decodeBody(ct string, data []byte) any {
	if strings.Contains(ct, "json") {
		var v any
		if json.Unmarshal(data, &v) == nil {
			return v
		}
	}
	return string(data)
}

func snippet(b []byte) string {
	s := strings.TrimSpace(string(b))
	if len(s) > 300 {
		s = s[:300] + "..."
	}
	return s
}

// ---- small built-ins ----

func transformMap() Action {
	return Func{
		M: Manifest{
			ID: "transform.map", Version: 1, Title: "Transform",
			Description: "Return params.value (or all params when value is absent). Use expressions in params to reshape data for later steps.",
			SideEffect:  "read", Risk: "low", SupportsDryRun: true, Class: ClassBuiltin,
			ParamsSchema: map[string]any{"type": "object"},
		},
		Fn: func(_ context.Context, in Input) (any, error) {
			if v, ok := in.Params["value"]; ok {
				return v, nil
			}
			return in.Params, nil
		},
	}
}

func delaySleep() Action {
	return Func{
		M: Manifest{
			ID: "delay.sleep", Version: 1, Title: "Sleep",
			Description: "Pause for a number of seconds inside the worker (use a wait node for long pauses).",
			SideEffect:  "read", Risk: "low", SupportsDryRun: true, Class: ClassBuiltin,
			ParamsSchema: obj(map[string]any{"seconds": num(0, 3600)}, "seconds"),
		},
		Fn: func(ctx context.Context, in Input) (any, error) {
			secs, _ := in.Params["seconds"].(float64)
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(time.Duration(secs * float64(time.Second))):
			}
			return map[string]any{"slept_seconds": secs}, nil
		},
	}
}

func notifyLog(log *slog.Logger) Action {
	return Func{
		M: Manifest{
			ID: "notify.log", Version: 1, Title: "Log message",
			Description: "Write a message to the platform log (useful for testing playbooks).",
			SideEffect:  "read", Risk: "low", SupportsDryRun: true, Class: ClassBuiltin,
			ParamsSchema: obj(map[string]any{"message": map[string]any{}, "level": enum("debug", "info", "warn", "error"), "fields": map[string]any{"type": "object"}}),
		},
		Fn: func(ctx context.Context, in Input) (any, error) {
			msg := fmt.Sprint(in.Params["message"])
			if in.Params["message"] == nil {
				msg = "notify.log"
			}
			attrs := []any{"execution_id", in.ExecutionID, "node_id", in.NodeID}
			if f, ok := in.Params["fields"].(map[string]any); ok {
				for k, v := range f {
					attrs = append(attrs, k, v)
				}
			}
			switch in.Params["level"] {
			case "debug":
				log.DebugContext(ctx, msg, attrs...)
			case "warn":
				log.WarnContext(ctx, msg, attrs...)
			case "error":
				log.ErrorContext(ctx, msg, attrs...)
			default:
				log.InfoContext(ctx, msg, attrs...)
			}
			return map[string]any{"logged": true, "message": msg}, nil
		},
	}
}

// ---- cases, observables, alerts ----

func actor(in Input) string { return "workflow:" + in.ExecutionID }

func caseCreate(cs *cases.Service) Action {
	return Func{
		M: Manifest{
			ID: "case.create", Version: 1, Title: "Create case",
			Description: "Create a case and link alerts to it (the triggering alert by default). Retries never create a second case.",
			SideEffect:  "write", Risk: "low", SupportsDryRun: true, Class: ClassBuiltin,
			ParamsSchema: obj(map[string]any{
				"title": strMax(300), "summary": strMax(20000), "severity": intRange(1, 5), "assignee": strMax(200),
				"tags": strList(), "alert_ids": strList(), "link_trigger_alert": map[string]any{"type": "boolean"},
			}, "title"),
			OutputSchema: obj(map[string]any{"case_id": str(), "created": map[string]any{"type": "boolean"}, "dry_run": map[string]any{"type": "boolean"}, "linked_alerts": strList()}),
		},
		Fn: func(ctx context.Context, in Input) (any, error) {
			p := in.Params
			if in.DryRun {
				return map[string]any{"dry_run": true, "created": false}, nil
			}
			c := domain.Case{Meta: domain.NewMeta(domain.PrefixCase), Status: "open"}
			c.ID = domain.DeterministicID(domain.PrefixCase, in.IdempotencyKey) // a retry finds the same case
			c.Title, _ = p["title"].(string)
			c.Summary, _ = p["summary"].(string)
			c.Assignee, _ = p["assignee"].(string)
			if f, ok := p["severity"].(float64); ok {
				c.Severity = int(f)
			}
			c.Tags = toStrings(p["tags"])
			alerts := toStrings(p["alert_ids"])
			if link, ok := p["link_trigger_alert"].(bool); (!ok || link) && in.AlertID != "" {
				alerts = append(alerts, in.AlertID)
			}
			doc, created, err := cs.Create(ctx, c, alerts, actor(in))
			if err != nil {
				return nil, storeErr(err)
			}
			return map[string]any{"case_id": doc.ID, "created": created, "linked_alerts": toAny(doc.AlertIDs)}, nil
		},
	}
}

func caseUpdate(cs *cases.Service) Action {
	return Func{
		M: Manifest{
			ID: "case.update", Version: 1, Title: "Update case",
			Description: "Change a case's title, summary, status, severity, assignee, tags or verdict, and optionally link more alerts.",
			SideEffect:  "write", Risk: "low", SupportsDryRun: true, Class: ClassBuiltin,
			ParamsSchema: obj(map[string]any{
				"case_id": str(), "title": strMax(300), "summary": strMax(20000), "status": enum(cases.Statuses...),
				"severity": intRange(1, 5), "assignee": strMax(200), "tags": strList(), "verdict": enum(cases.Verdicts...), "alert_ids": strList(),
			}, "case_id"),
			OutputSchema: obj(map[string]any{"case_id": str(), "changed": strList(), "dry_run": map[string]any{"type": "boolean"}}),
		},
		Fn: func(ctx context.Context, in Input) (any, error) {
			id, _ := in.Params["case_id"].(string)
			if in.DryRun {
				return map[string]any{"case_id": id, "dry_run": true, "changed": []any{}}, nil
			}
			p := patchFrom(in.Params)
			_, changed, err := cs.Update(ctx, id, p, actor(in))
			if err != nil {
				return nil, storeErr(err)
			}
			if ids := toStrings(in.Params["alert_ids"]); len(ids) > 0 {
				if _, err := cs.LinkAlerts(ctx, id, ids, actor(in)); err != nil {
					return nil, storeErr(err)
				}
				changed = append(changed, "alert_ids")
			}
			return map[string]any{"case_id": id, "changed": toAny(changed)}, nil
		},
	}
}

func caseClose(cs *cases.Service, st *store.Store) Action {
	return Func{
		M: Manifest{
			ID: "case.close", Version: 1, Title: "Close case",
			Description: "Close a case with an optional verdict and summary. Without case_id it closes the case linked to the triggering alert, if any.",
			SideEffect:  "write", Risk: "low", SupportsDryRun: true, Class: ClassBuiltin,
			ParamsSchema: obj(map[string]any{"case_id": str(), "verdict": enum(cases.Verdicts...), "summary": strMax(20000)}),
			OutputSchema: obj(map[string]any{"case_id": str(), "closed": map[string]any{"type": "boolean"}, "reason": str(), "dry_run": map[string]any{"type": "boolean"}}),
		},
		Fn: func(ctx context.Context, in Input) (any, error) {
			id, _ := in.Params["case_id"].(string)
			if id == "" && in.AlertID != "" {
				a, _, err := st.Alerts.Get(ctx, in.AlertID)
				if err != nil && !errors.Is(err, store.ErrNotFound) {
					return nil, storeErr(err)
				}
				id = a.CaseID
			}
			if id == "" {
				return map[string]any{"closed": false, "reason": "no case_id given and the triggering alert is not linked to a case"}, nil
			}
			if in.DryRun {
				return map[string]any{"case_id": id, "closed": false, "dry_run": true}, nil
			}
			closed := "closed"
			p := cases.Patch{Status: &closed}
			if v, ok := in.Params["verdict"].(string); ok {
				p.Verdict = &v
			}
			if v, ok := in.Params["summary"].(string); ok {
				p.Summary = &v
			}
			if _, _, err := cs.Update(ctx, id, p, actor(in)); err != nil {
				return nil, storeErr(err)
			}
			return map[string]any{"case_id": id, "closed": true}, nil
		},
	}
}

func observableAdd(cs *cases.Service) Action {
	return Func{
		M: Manifest{
			ID: "observable.add", Version: 1, Title: "Add observable",
			Description: "Record an indicator (ip, domain, url, hash, email, host, user, file_path), optionally linked to a case.",
			SideEffect:  "write", Risk: "low", SupportsDryRun: true, Class: ClassBuiltin,
			ParamsSchema: obj(map[string]any{"type": enum(cases.ObservableTypes...), "value": strMax(2048), "case_id": str()}, "type", "value"),
			OutputSchema: obj(map[string]any{"observable_id": str(), "created": map[string]any{"type": "boolean"}, "dry_run": map[string]any{"type": "boolean"}}),
		},
		Fn: func(ctx context.Context, in Input) (any, error) {
			typ, _ := in.Params["type"].(string)
			val, _ := in.Params["value"].(string)
			if in.DryRun {
				return map[string]any{"observable_id": domain.ObservableID(typ, val), "created": false, "dry_run": true}, nil
			}
			caseID, _ := in.Params["case_id"].(string)
			doc, created, err := cs.AddObservable(ctx, caseID, typ, val, actor(in))
			if err != nil {
				return nil, storeErr(err)
			}
			return map[string]any{"observable_id": doc.ID, "created": created}, nil
		},
	}
}

func alertUpdate(st *store.Store) Action {
	return Func{
		M: Manifest{
			ID: "alert.update", Version: 1, Title: "Update alert",
			Description: "Set an alert's verdict, status or summary (the triggering alert when alert_id is omitted).",
			SideEffect:  "write", Risk: "low", SupportsDryRun: true, Class: ClassBuiltin,
			ParamsSchema: obj(map[string]any{"alert_id": str(), "verdict": enum(cases.Verdicts...), "status": strMax(50), "summary": strMax(20000)}),
			OutputSchema: obj(map[string]any{"alert_id": str(), "dry_run": map[string]any{"type": "boolean"}}),
		},
		Fn: func(ctx context.Context, in Input) (any, error) {
			id, _ := in.Params["alert_id"].(string)
			if id == "" {
				id = in.AlertID
			}
			if id == "" {
				return nil, Errf("invalid_params", false, "alert_id is required when the workflow was not triggered by an alert")
			}
			if in.DryRun {
				return map[string]any{"alert_id": id, "dry_run": true}, nil
			}
			_, _, err := st.Alerts.Update(ctx, id, func(a *domain.Alert) error {
				if v, ok := in.Params["verdict"].(string); ok {
					a.Verdict = v
				}
				if v, ok := in.Params["status"].(string); ok {
					a.Status = v
				}
				if v, ok := in.Params["summary"].(string); ok {
					a.Summary = v
				}
				return nil
			})
			if err != nil {
				return nil, storeErr(err)
			}
			return map[string]any{"alert_id": id}, nil
		},
	}
}

func patchFrom(p map[string]any) cases.Patch {
	var out cases.Patch
	sp := func(k string) *string {
		if v, ok := p[k].(string); ok {
			return &v
		}
		return nil
	}
	out.Title, out.Summary, out.Status, out.Assignee, out.Verdict = sp("title"), sp("summary"), sp("status"), sp("assignee"), sp("verdict")
	if f, ok := p["severity"].(float64); ok {
		n := int(f)
		out.Severity = &n
	}
	if _, ok := p["tags"]; ok {
		t := toStrings(p["tags"])
		out.Tags = &t
	}
	return out
}

// storeErr maps store errors to action errors: missing documents are permanent,
// conflicts and backend trouble are retryable.
func storeErr(err error) error {
	switch {
	case errors.Is(err, store.ErrNotFound):
		return Errf("not_found", false, "%v", err)
	case errors.Is(err, store.ErrInvalid):
		return Errf("invalid_params", false, "%v", err)
	case errors.Is(err, store.ErrConflictExhausted), errors.Is(err, store.ErrVersionConflict):
		return Errf("conflict", true, "%v", err)
	}
	return Errf("storage", true, "%v", err)
}

func toStrings(v any) []string {
	list, _ := v.([]any)
	out := make([]string, 0, len(list))
	for _, x := range list {
		if s, ok := x.(string); ok && s != "" {
			out = append(out, s)
		}
	}
	return out
}

func toAny(l []string) []any {
	out := make([]any, len(l))
	for i, s := range l {
		out[i] = s
	}
	return out
}
