package e2e

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"strings"
	"sync"
	"testing"

	"github.com/yourorg/soar/internal/actions"
	"github.com/yourorg/soar/internal/audit"
	"github.com/yourorg/soar/internal/domain"
	"github.com/yourorg/soar/internal/store"
)

// syncBuffer is a bytes.Buffer safe for the worker's goroutines.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}
func (s *syncBuffer) String() string { s.mu.Lock(); defer s.mu.Unlock(); return s.b.String() }

// Spec rule 3.1.7 and section 13: a secret is decrypted only in the worker, its value never
// reaches the execution context, step runs or logs (even when an action echoes it back, plain
// or base64-encoded, or puts it in an error), and every read and side effect is audited.
func TestFlowSecretsAreScrubbedAndReadsAudited(t *testing.T) {
	logs := &syncBuffer{}
	f := newFlow(t, flowOpts{workerLog: logs})
	const secret = "tok-123" // stored as intel_token by newFlow
	leak := func(_ context.Context, in actions.Input) (any, error) {
		tok, _ := in.Params["token"].(string)
		in.Log("calling the intel API with " + tok)
		if in.Params["fail"] == true {
			return nil, actions.Errf("http_401", false, "server rejected Authorization: Bearer %s", tok)
		}
		return map[string]any{
			"echo": tok, "basic": base64.StdEncoding.EncodeToString([]byte(tok)), "header": "Bearer " + tok,
			tok: "secret as a key",
		}, nil
	}
	for _, m := range []actions.Manifest{{ID: "test.leak", SideEffect: "write"}, {ID: "test.leak_read", SideEffect: "read"}} {
		m.Version, m.Class, m.Description, m.Risk = 1, actions.ClassBuiltin, m.ID, "low"
		if err := f.reg.Register(actions.Func{M: m, Fn: leak}); err != nil {
			t.Fatal(err)
		}
	}
	wf := f.workflow("secret-leak", `
nodes:
  - id: echo
    action: test.leak
    params: { token: 'secrets.intel_token' }
  - id: boom
    action: test.leak_read
    params: { token: 'secrets.intel_token', fail: true }
edges: ["echo -> boom"]
`)
	exe := f.done(f.run(wf, nil, false))

	// nothing stored or returned contains the value
	ctxJSON, _ := json.Marshal(exe)
	if strings.Contains(string(ctxJSON), secret) || strings.Contains(string(ctxJSON), base64.StdEncoding.EncodeToString([]byte(secret))) {
		t.Fatalf("the execution document contains the secret: %s", ctxJSON)
	}
	echo := nodeState(t, exe, "echo")
	out, _ := echo.Output.(map[string]any)
	if out["echo"] != "***" || out["header"] != "Bearer ***" || out["basic"] != "***" || out["***"] != "secret as a key" {
		t.Errorf("echo output not scrubbed: %v", echo.Output)
	}
	boom := nodeState(t, exe, "boom")
	if boom.Error == nil || boom.Error.Code != "http_401" || boom.Error.Message != "server rejected Authorization: Bearer ***" {
		t.Errorf("error not scrubbed: %+v", boom.Error)
	}
	runs, err := f.st.StepRuns.List(f.ctx, store.ListQuery{Limit: 50})
	if err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(runs.Items)
	if len(runs.Items) < 2 || strings.Contains(string(b), secret) {
		t.Errorf("step runs (%d) contain the secret: %s", len(runs.Items), b)
	}
	if l := logs.String(); strings.Contains(l, secret) || !strings.Contains(l, "calling the intel API with ***") {
		t.Errorf("worker log: %s", l)
	}

	// audit: two secret reads (one per step) and the write action; the read-only step's
	// failure is not a side effect
	page, err := f.st.Audit.List(f.ctx, store.ListQuery{Limit: 50})
	if err != nil {
		t.Fatal(err)
	}
	var reads, writes []domain.AuditRecord
	for _, r := range page.Items {
		if r.ActorType != audit.ActorWorkflow || r.Actor != "workflow:secret-leak" || !strings.HasPrefix(r.RequestID, exe.ID+":") {
			t.Errorf("unexpected audit record %+v", r)
			continue
		}
		switch r.Action {
		case audit.ActionSecretRead:
			if r.Resource != "secrets/intel_token" {
				t.Errorf("secret read of %q", r.Resource)
			}
			reads = append(reads, r)
		case "test.leak":
			writes = append(writes, r)
		default:
			t.Errorf("unexpected audit action %q", r.Action)
		}
	}
	if len(reads) != 2 || len(writes) != 1 || writes[0].Resource != "actions/test.leak" {
		t.Errorf("audit: %d secret reads (want 2), %d writes (want 1): %+v", len(reads), len(writes), page.Items)
	}
	if strings.Contains(func() string { b, _ := json.Marshal(page.Items); return string(b) }(), secret) {
		t.Error("an audit record contains the secret value")
	}
}

// A dry run reads no secret it does not need to and audits no side effect.
func TestFlowDryRunWritesNoActionAudit(t *testing.T) {
	f := newFlow(t, flowOpts{})
	wf := f.workflow("dry-audit", `
nodes:
  - id: c
    action: test.count
    params: { n: 1 }
`)
	if e := f.done(f.run(wf, nil, true)); e.Status != "succeeded" {
		t.Fatalf("dry run: %s", e.Status)
	}
	page, _ := f.st.Audit.List(f.ctx, store.ListQuery{Limit: 10})
	if len(page.Items) != 0 {
		t.Errorf("a dry run must not be audited as a side effect: %+v", page.Items)
	}
}
