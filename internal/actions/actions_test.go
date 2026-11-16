package actions

import (
	"context"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/yourorg/soar/internal/store/memory"
)

func registry(t *testing.T, allowLoopback bool) *Registry {
	t.Helper()
	r := NewRegistry()
	if err := RegisterBuiltins(r, Deps{Store: memory.NewStore("soar"), Log: slog.New(slog.NewTextHandler(io.Discard, nil)), HTTP: HTTPOptions{AllowLoopback: allowLoopback}}); err != nil {
		t.Fatal(err)
	}
	return r
}

func run(t *testing.T, r *Registry, id string, in Input) (any, *Error) {
	t.Helper()
	a, ok := r.Get(id)
	if !ok {
		t.Fatalf("no action %s", id)
	}
	if e := CheckParams(a.Manifest(), in.Params); e != nil {
		return nil, e
	}
	out, err := a.Execute(context.Background(), in)
	if err != nil {
		return nil, AsError(err)
	}
	if e := CheckOutput(a.Manifest(), out); e != nil {
		t.Fatalf("%s output breaks its schema: %v", id, e)
	}
	return out, nil
}

func TestHTTPRequest(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		switch r.URL.Path {
		case "/ok":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"score": 91}`))
		case "/busy":
			w.WriteHeader(503)
		case "/slow-down":
			w.WriteHeader(429)
		default:
			w.WriteHeader(404)
		}
	}))
	defer srv.Close()
	r := registry(t, true)
	out, e := run(t, r, "http.request", Input{Params: map[string]any{"url": srv.URL + "/ok", "query": map[string]any{"q": "x"}}})
	if e != nil || out.(map[string]any)["body"].(map[string]any)["score"] != float64(91) || out.(map[string]any)["status"] != 200 {
		t.Fatalf("ok: %v %v", out, e)
	}
	for path, want := range map[string]struct {
		code      string
		retryable bool
	}{"/busy": {"http_5xx", true}, "/slow-down": {"http_429", true}, "/missing": {"http_404", false}} {
		if _, e := run(t, r, "http.request", Input{Params: map[string]any{"url": srv.URL + path}}); e == nil || e.Code != want.code || e.Retryable != want.retryable {
			t.Errorf("%s: %+v", path, e)
		}
	}
	if out, e := run(t, r, "http.request", Input{Params: map[string]any{"url": srv.URL + "/missing", "fail_on_status": false}}); e != nil || out.(map[string]any)["status"] != 404 {
		t.Errorf("fail_on_status false: %v %v", out, e)
	}
	before := hits.Load()
	out, e = run(t, r, "http.request", Input{DryRun: true, Params: map[string]any{"url": srv.URL + "/ok?token=abc", "method": "POST", "json": map[string]any{"a": 1}}})
	if e != nil || out.(map[string]any)["dry_run"] != true || hits.Load() != before {
		t.Fatalf("dry run must not send: %v %v", out, e)
	}
	if u := out.(map[string]any)["would_send"].(map[string]any)["url"].(string); u != srv.URL+"/ok?token=REDACTED" {
		t.Errorf("credentials in URLs must be redacted: %s", u)
	}
	if _, e := run(t, r, "http.request", Input{Params: map[string]any{"url": srv.URL, "method": "TRACE"}}); e == nil || e.Code != "invalid_params" {
		t.Errorf("schema check: %+v", e)
	}
	if _, e := run(t, r, "http.request", Input{Params: map[string]any{"url": srv.URL, "ca_pem": "not a cert"}}); e == nil || e.Code != "invalid_params" {
		t.Errorf("bad CA: %+v", e)
	}
}

func TestSSRFGuard(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(200) }))
	defer srv.Close()
	r := registry(t, false)
	if _, e := run(t, r, "http.request", Input{Params: map[string]any{"url": srv.URL}}); e == nil || e.Code != "ssrf_blocked" {
		t.Fatalf("loopback must be blocked: %+v", e)
	}
	for _, ip := range []string{"169.254.169.254", "127.0.0.1", "::1", "0.0.0.0", "fe80::1", "fd00:ec2::254", "100.100.100.200"} {
		if !BlockedIP(net.ParseIP(ip), false) {
			t.Errorf("%s must be blocked", ip)
		}
	}
	for _, ip := range []string{"10.0.0.5", "192.168.1.10", "8.8.8.8"} {
		if BlockedIP(net.ParseIP(ip), false) {
			t.Errorf("%s must be allowed (internal services are reachable)", ip)
		}
	}
}

func TestCaseActionsAreIdempotent(t *testing.T) {
	r := registry(t, false)
	in := Input{ExecutionID: "exe_1", IdempotencyKey: "exe_1:open:1", Params: map[string]any{"title": "Phish", "severity": float64(4)}}
	out1, e := run(t, r, "case.create", in)
	if e != nil || out1.(map[string]any)["created"] != true {
		t.Fatalf("create: %v %v", out1, e)
	}
	out2, e := run(t, r, "case.create", in) // a redelivery after a lost ack
	if e != nil || out2.(map[string]any)["created"] != false || out2.(map[string]any)["case_id"] != out1.(map[string]any)["case_id"] {
		t.Fatalf("retry must find the same case: %v %v", out2, e)
	}
	id := out1.(map[string]any)["case_id"].(string)
	if out, e := run(t, r, "case.close", Input{Params: map[string]any{"case_id": id, "verdict": "true_positive"}}); e != nil || out.(map[string]any)["closed"] != true {
		t.Fatalf("close: %v %v", out, e)
	}
	if out, e := run(t, r, "case.close", Input{Params: map[string]any{}}); e != nil || out.(map[string]any)["closed"] != false {
		t.Fatalf("close without a case must explain, not fail: %v %v", out, e)
	}
	if _, e := run(t, r, "case.update", Input{Params: map[string]any{"case_id": "cas_missing", "status": "open"}}); e == nil || e.Code != "not_found" || e.Retryable {
		t.Fatalf("missing case: %+v", e)
	}
	if out, e := run(t, r, "observable.add", Input{Params: map[string]any{"type": "ip", "value": "10.0.0.1", "case_id": id}}); e != nil || out.(map[string]any)["created"] != true {
		t.Fatalf("observable: %v %v", out, e)
	}
}

func TestManifestValidation(t *testing.T) {
	bad := []Manifest{
		{ID: "x.y", Description: "", SideEffect: "read", Risk: "low", Class: ClassBuiltin},
		{ID: "x.y", Description: "d", SideEffect: "maybe", Risk: "low", Class: ClassBuiltin},
		{ID: "x.y", Description: "d", SideEffect: "read", Risk: "low", Class: ClassPython},
	}
	for i, m := range bad {
		if m.Validate() == nil {
			t.Errorf("manifest %d should be rejected", i)
		}
	}
	m := Manifest{RetryableErrors: []string{"http_5xx", "timeout"}}
	if !m.Retryable("http_503") || !m.Retryable("timeout") || m.Retryable("http_404") {
		t.Error("retryable codes")
	}
}
