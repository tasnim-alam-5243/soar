package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/yourorg/soar/internal/actions"
	"github.com/yourorg/soar/internal/domain"
	"github.com/yourorg/soar/internal/engine"
	"github.com/yourorg/soar/internal/orchestrator"
	"github.com/yourorg/soar/internal/queue"
	"github.com/yourorg/soar/internal/security"
	"github.com/yourorg/soar/internal/store"
	"github.com/yourorg/soar/internal/worker"
)

// Publisher sends messages to NATS (execution starts, cancels, approvals).
type Publisher interface {
	Publish(ctx context.Context, m queue.Msg) error
}

// WithEngine enables running workflows: the action registry (validation, connectors),
// a publisher and the number of orchestrator partitions.
func (s *Server) WithEngine(reg *actions.Registry, pub Publisher, partitions int) *Server {
	s.reg, s.pub, s.partitions = reg, pub, partitions
	return s
}

// WithSecrets enables the secrets endpoints.
func (s *Server) WithSecrets(ss *security.SecretStore) *Server { s.secrets = ss; return s }

func (s *Server) needPublisher() error {
	if s.pub == nil {
		return newProblem(http.StatusServiceUnavailable, "queue-unavailable", "Queue unavailable", "this API instance has no connection to NATS, so it cannot start or control executions")
	}
	return nil
}

func (s *Server) catalog() engine.Catalog {
	if s.reg == nil {
		return nil
	}
	return s.reg
}

// decodeOptional is decode for endpoints whose body may be empty.
func decodeOptional(r *http.Request, dst any) error {
	b, err := io.ReadAll(r.Body)
	if err != nil {
		return err
	}
	if len(bytes.TrimSpace(b)) == 0 {
		return nil
	}
	r.Body = io.NopCloser(bytes.NewReader(b))
	return decode(r, dst)
}

func toJSONMap(v any) map[string]any {
	b, _ := json.Marshal(v)
	var m map[string]any
	_ = json.Unmarshal(b, &m)
	return m
}

// ---- run, cancel, retry ----

type runIn struct {
	Inputs  map[string]any `json:"inputs"`
	AlertID string         `json:"alert_id"`
	DryRun  bool           `json:"dry_run"`
}

// startExecution stores a queued execution and publishes its start message. If the
// publish fails the execution stays queued and the scheduler's sweeper starts it.
func (s *Server) startExecution(r *http.Request, exe domain.Execution, p orchestrator.StartPayload) (domain.Execution, error) {
	exe.Context = (&engine.State{Status: engine.StatusQueued, Inputs: p.Inputs, Alert: p.Alert, DryRun: p.DryRun, Parent: p.Parent}).JSON()
	if _, err := s.st.Executions.Create(r.Context(), exe); err != nil {
		return domain.Execution{}, err
	}
	m, err := orchestrator.StartMessage(exe, p)
	if err == nil {
		err = s.pub.Publish(r.Context(), m)
	}
	if err != nil {
		s.log.WarnContext(r.Context(), "execution stored but its start message failed; the scheduler will start it", "execution_id", exe.ID, "error", err.Error())
	}
	stored, _, gerr := s.st.Executions.Get(r.Context(), exe.ID)
	if gerr != nil {
		return exe, nil
	}
	return stored, nil
}

func (s *Server) runWorkflow(w http.ResponseWriter, r *http.Request) error {
	if err := s.needPublisher(); err != nil {
		return err
	}
	var in runIn
	if err := decodeOptional(r, &in); err != nil {
		return err
	}
	wf, _, err := s.st.Workflows.Get(r.Context(), r.PathValue("id"))
	if err != nil {
		return err
	}
	switch {
	case wf.Status == "archived":
		return conflict("archived", "archived workflow versions cannot run")
	case wf.Status != "published" && !in.DryRun:
		return conflict("not-published", "only published versions run for real; set dry_run to test a draft")
	}
	if _, errs := engine.Compile(wf.Definition, s.catalog()); len(errs) > 0 {
		return unprocessable(fieldErrors(errs))
	}
	ik, err := s.idemBegin(r)
	if err != nil {
		return err
	}
	if ik.replay != "" {
		doc, ver, err := s.st.Executions.Get(r.Context(), ik.replay)
		if err != nil {
			return err
		}
		w.Header().Set("Idempotent-Replayed", "true")
		w.Header().Set("Location", Prefix+"/executions/"+doc.ID)
		return writeResource(w, http.StatusAccepted, doc, ver)
	}
	var alert map[string]any
	if in.AlertID != "" {
		a, _, err := s.st.Alerts.Get(r.Context(), in.AlertID)
		if err != nil {
			ik.abort(r)
			if errors.Is(err, store.ErrNotFound) {
				return unprocessable([]FieldError{{Path: "alert_id", Message: "no such alert"}})
			}
			return err
		}
		alert = toJSONMap(a)
	}
	exe := orchestrator.NewExecution(wf, "", domain.ExecutionTrigger{Type: "manual", User: s.actor(r), AlertID: in.AlertID}, s.partitions, in.DryRun)
	stored, err := s.startExecution(r, exe, orchestrator.StartPayload{Inputs: in.Inputs, Alert: alert, DryRun: in.DryRun})
	if err != nil {
		ik.abort(r)
		return err
	}
	ik.done(r, stored.ID)
	s.audit(r, "execution.run", "execution:"+stored.ID, nil, stored)
	w.Header().Set("Location", Prefix+"/executions/"+stored.ID)
	return writeJSON(w, http.StatusAccepted, stored)
}

type cancelIn struct {
	Reason string `json:"reason"`
}

func (s *Server) cancelExecution(w http.ResponseWriter, r *http.Request) error {
	if err := s.needPublisher(); err != nil {
		return err
	}
	var in cancelIn
	if err := decodeOptional(r, &in); err != nil {
		return err
	}
	exe, _, err := s.st.Executions.Get(r.Context(), r.PathValue("id"))
	if err != nil {
		return err
	}
	if engine.IsTerminal(exe.Status) {
		return conflict("already-ended", "the execution already ended ("+exe.Status+")")
	}
	m, err := orchestrator.ControlMessage(queue.MsgCancel, "cancel:"+exe.ID, exe, "", orchestrator.CancelPayload{Actor: s.actor(r), Reason: in.Reason})
	if err != nil {
		return err
	}
	if err := s.pub.Publish(r.Context(), m); err != nil {
		return newProblem(http.StatusServiceUnavailable, "queue-unavailable", "Queue unavailable", "the cancel request could not be queued; try again")
	}
	s.audit(r, "execution.cancel", "execution:"+exe.ID, nil, map[string]any{"reason": in.Reason})
	return writeJSON(w, http.StatusAccepted, exe)
}

func (s *Server) retryExecution(w http.ResponseWriter, r *http.Request) error {
	if err := s.needPublisher(); err != nil {
		return err
	}
	orig, _, err := s.st.Executions.Get(r.Context(), r.PathValue("id"))
	if err != nil {
		return err
	}
	switch orig.Status {
	case engine.StatusFailed, engine.StatusCancelled, engine.StatusTimedOut:
	default:
		return conflict("not-retryable", "only failed, cancelled or timed-out executions can be retried (this one is "+orig.Status+")")
	}
	wf, _, err := s.st.Workflows.Get(r.Context(), orig.WorkflowID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return conflict("workflow-gone", "the workflow version of this execution no longer exists")
		}
		return err
	}
	st, _ := engine.StateFromJSON(orig.Context)
	trig := orig.Trigger
	trig.Type, trig.RetryOf, trig.User = "retry", orig.ID, s.actor(r)
	exe := orchestrator.NewExecution(wf, "", trig, s.partitions, orig.DryRun)
	p := orchestrator.StartPayload{DryRun: orig.DryRun}
	if st != nil {
		p.Inputs, p.Alert = st.Inputs, st.Alert
	}
	stored, err := s.startExecution(r, exe, p)
	if err != nil {
		return err
	}
	s.audit(r, "execution.retry", "execution:"+stored.ID, nil, map[string]any{"retry_of": orig.ID})
	w.Header().Set("Location", Prefix+"/executions/"+stored.ID)
	return writeJSON(w, http.StatusAccepted, stored)
}

func fieldErrors(errs []engine.FieldError) []FieldError {
	out := make([]FieldError, len(errs))
	for i, e := range errs {
		out[i] = FieldError{Path: e.Path, Message: e.Message}
	}
	return out
}

// ---- approvals ----

func (s *Server) listApprovals(w http.ResponseWriter, r *http.Request) error {
	q, v, err := listQuery(r, sortSpec{fields: map[string]string{"created_at": "created_at", "due_at": "due_at"}, defaultName: "created_at", defaultDesc: true},
		"status", "execution_id", "workflow_id")
	if err != nil {
		return err
	}
	for _, f := range []string{"status", "execution_id", "workflow_id"} {
		term(&q, v, f, f)
	}
	page, err := s.st.Approvals.List(r.Context(), q)
	if err != nil {
		return err
	}
	return writePage(w, page)
}

func (s *Server) getApproval(w http.ResponseWriter, r *http.Request) error {
	return getOne(w, r, s.st.Approvals)
}

type decisionIn struct {
	Approved *bool  `json:"approved"`
	Comment  string `json:"comment"`
}

func (s *Server) decideApproval(w http.ResponseWriter, r *http.Request) error {
	if err := s.needPublisher(); err != nil {
		return err
	}
	var in decisionIn
	if err := decode(r, &in); err != nil {
		return err
	}
	var v verr
	if in.Approved == nil {
		v.add("approved", "is required (true or false)")
	}
	v.maxLen("comment", in.Comment, 4000)
	if err := v.err(); err != nil {
		return err
	}
	ctx := r.Context()
	cur, _, err := s.st.Approvals.Get(ctx, r.PathValue("id"))
	if err != nil {
		return err
	}
	if !s.canApprove(r, cur) {
		return newProblem(http.StatusForbidden, "forbidden", "Forbidden", "you are not one of the approvers of this request")
	}
	now := s.now()
	status := "rejected"
	if *in.Approved {
		status = "approved"
	}
	decided, ver, err := s.st.Approvals.Update(ctx, cur.ID, func(a *domain.Approval) error {
		if a.Status != "pending" {
			return conflict("already-decided", "this approval is already "+a.Status)
		}
		a.Status, a.DecidedBy, a.DecidedAt, a.Comment = status, s.actor(r), &now, in.Comment
		return nil
	})
	if err != nil {
		return err
	}
	exe, _, err := s.st.Executions.Get(ctx, decided.ExecutionID)
	if err == nil {
		err = s.deliverDecision(ctx, exe, decided, *in.Approved, in.Comment, s.actor(r))
	}
	if err != nil {
		// put it back so the decision can be made again
		_, _, _ = s.st.Approvals.Update(context.WithoutCancel(ctx), cur.ID, func(a *domain.Approval) error {
			a.Status, a.DecidedBy, a.DecidedAt = "pending", "", nil
			return nil
		})
		s.log.WarnContext(ctx, "approval decision not delivered", "approval", cur.ID, "error", err.Error())
		return newProblem(http.StatusServiceUnavailable, "queue-unavailable", "Decision not delivered", "the decision could not be delivered to the execution; try again")
	}
	s.audit(r, "approval."+status, "approval:"+decided.ID, nil, map[string]any{"execution_id": decided.ExecutionID, "node_id": decided.NodeID})
	return writeResource(w, http.StatusOK, decided, ver)
}

func (s *Server) deliverDecision(ctx context.Context, exe domain.Execution, a domain.Approval, approved bool, comment, actor string) error {
	var m queue.Msg
	var err error
	if len(a.Command) > 0 { // policy approval: release or fail the held-back step
		cmd, derr := orchestrator.DecodeCommand(a.Command)
		if derr != nil || cmd.Dispatch == nil {
			return errors.New("approval holds no valid command")
		}
		if approved {
			m, err = orchestrator.DispatchMessage(exe, cmd, s.reg)
		} else {
			m, err = orchestrator.StepResultMessage(exe, cmd, worker.ResultPayload{
				Status: engine.NodeFailed,
				Error:  &engine.StepError{Code: "approval_rejected", Message: "rejected by " + actor + ": " + comment},
			})
		}
	} else {
		m, err = orchestrator.ControlMessage(queue.MsgApproval, "approval:"+a.ID, exe, a.NodeID,
			orchestrator.ApprovalPayload{Approved: approved, Actor: actor, Comment: comment})
	}
	if err != nil {
		return err
	}
	return s.pub.Publish(ctx, m)
}

// ---- connectors ----

func (s *Server) listConnectors(w http.ResponseWriter, _ *http.Request) error {
	var items []actions.ConnectorInfo
	if s.reg != nil {
		items = s.reg.Connectors()
	}
	return writePage(w, store.Page[actions.ConnectorInfo]{Items: items})
}

func (s *Server) connectorActions(w http.ResponseWriter, r *http.Request) error {
	if s.reg == nil {
		return notFound("no such connector")
	}
	c, ok := s.reg.Connector(r.PathValue("id"))
	if !ok {
		return notFound("no such connector")
	}
	var items []actions.Manifest
	for _, id := range c.Actions {
		if a, ok := s.reg.Get(id); ok {
			items = append(items, a.Manifest())
		}
	}
	sort.Slice(items, func(i, j int) bool { return items[i].ID < items[j].ID })
	return writePage(w, store.Page[actions.Manifest]{Items: items})
}

// ---- secrets (values are write-only) ----

type secretIn struct {
	Value string `json:"value"`
	Scope string `json:"scope"`
}

func (s *Server) needSecrets() error {
	if s.secrets == nil || s.secrets.Keys == nil {
		return newProblem(http.StatusServiceUnavailable, "secrets-unavailable", "Secret store unavailable", "set SOAR_MASTER_KEY to store secrets")
	}
	return nil
}

func (s *Server) putSecret(w http.ResponseWriter, r *http.Request) error {
	if err := s.needSecrets(); err != nil {
		return err
	}
	var in secretIn
	if err := decode(r, &in); err != nil {
		return err
	}
	name := r.PathValue("name")
	var v verr
	if !security.ValidSecretName(name) {
		v.add("name", "starts with a letter and uses letters, digits, _ . - (max 128)")
	}
	v.required("value", in.Value)
	v.maxLen("value", in.Value, 65536)
	v.maxLen("scope", in.Scope, 200)
	if !security.ValidScope(in.Scope) {
		v.add("scope", "a comma-separated list of connector ids and/or \"workflows\" (empty: usable anywhere)")
	}
	if err := v.err(); err != nil {
		return err
	}
	meta, err := s.secrets.Put(r.Context(), name, in.Value, in.Scope)
	if err != nil {
		return err
	}
	s.audit(r, "secret.put", "secret:"+name, nil, nil)
	return writeJSON(w, http.StatusOK, meta)
}

func (s *Server) listSecrets(w http.ResponseWriter, r *http.Request) error {
	q, _, err := listQuery(r, sortSpec{fields: map[string]string{"name": "name", "updated_at": "updated_at"}, defaultName: "name"})
	if err != nil {
		return err
	}
	page, err := s.st.Secrets.List(r.Context(), q)
	if err != nil {
		return err
	}
	for i := range page.Items {
		page.Items[i] = security.Metadata(page.Items[i])
	}
	return writePage(w, page)
}

func (s *Server) deleteSecret(w http.ResponseWriter, r *http.Request) error {
	name := r.PathValue("name")
	var err error
	if s.secrets != nil {
		err = s.secrets.Delete(r.Context(), name) // also drops the cached value
	} else {
		err = s.st.Secrets.Delete(r.Context(), security.SecretID(name))
	}
	if err != nil {
		return err
	}
	s.audit(r, "secret.delete", "secret:"+name, nil, nil)
	w.WriteHeader(http.StatusNoContent)
	return nil
}

// ---- connection test ----

type connectionTestOut struct {
	OK          bool   `json:"ok"`
	Status      string `json:"status"`
	ExecutionID string `json:"execution_id"`
	Output      any    `json:"output,omitempty"`
	Error       string `json:"error,omitempty"`
}

// testConnection runs the connector's <connector>.connection.test action through the
// worker (secrets are decrypted only there) and waits briefly for the outcome.
func (s *Server) testConnection(w http.ResponseWriter, r *http.Request) error {
	if err := s.needPublisher(); err != nil {
		return err
	}
	ctx := r.Context()
	conn, _, err := s.st.Connections.Get(ctx, r.PathValue("id"))
	if err != nil {
		return err
	}
	if s.reg == nil {
		return conflict("no-test", "this API instance has no action registry")
	}
	actionID := ""
	if info, ok := s.reg.Connector(conn.ConnectorID); ok {
		for _, a := range info.Actions {
			if strings.HasSuffix(a, ".connection.test") {
				actionID = a
			}
		}
	}
	if actionID == "" {
		return unprocessable([]FieldError{{Path: "connector_id", Message: "connector " + conn.ConnectorID + " is not installed or has no connection test action"}})
	}
	wf, err := s.systemWorkflow(ctx, "_system.connection_test."+conn.ConnectorID, map[string]any{
		"inputs": []any{map[string]any{"name": "connection", "required": true}},
		"nodes":  []any{map[string]any{"id": "test", "action": actionID, "params": map[string]any{"connection": "inputs.connection"}, "retry": map[string]any{"max_attempts": 1}, "timeout": "30s"}},
	})
	if err != nil {
		return err
	}
	exe := orchestrator.NewExecution(wf, "", domain.ExecutionTrigger{Type: "manual", User: s.actor(r)}, s.partitions, false)
	stored, err := s.startExecution(r, exe, orchestrator.StartPayload{Inputs: map[string]any{"connection": conn.ID}})
	if err != nil {
		return err
	}
	out := connectionTestOut{Status: stored.Status, ExecutionID: stored.ID}
	wait := s.testWait
	if wait == 0 {
		wait = 35 * time.Second
	}
	deadline := time.Now().Add(wait)
	for time.Now().Before(deadline) {
		cur, _, err := s.st.Executions.Get(ctx, stored.ID)
		if err == nil {
			out.Status = cur.Status
			if engine.IsTerminal(cur.Status) {
				st, _ := engine.StateFromJSON(cur.Context)
				if ns := st.Nodes["test"]; ns != nil {
					out.Output = ns.Output
					if ns.Error != nil {
						out.Error = ns.Error.Code + ": " + ns.Error.Message
					}
				}
				if out.Error == "" {
					out.Error = cur.Error
				}
				out.OK = cur.Status == engine.StatusSucceeded
				break
			}
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(300 * time.Millisecond):
		}
	}
	if !engine.IsTerminal(out.Status) {
		out.Error = "the test did not finish in time; follow the execution for its result"
	}
	return writeJSON(w, http.StatusOK, out)
}

// systemWorkflow returns (creating it when missing) a published workflow the platform
// itself uses, such as connection tests. Its id is derived from its name and content.
func (s *Server) systemWorkflow(ctx context.Context, name string, def map[string]any) (domain.Workflow, error) {
	sum := checksum(def, nil)
	id := domain.DeterministicID(domain.PrefixWorkflow, name, sum)
	if wf, _, err := s.st.Workflows.Get(ctx, id); err == nil {
		return wf, nil
	} else if !errors.Is(err, store.ErrNotFound) {
		return domain.Workflow{}, err
	}
	meta := domain.NewMeta(domain.PrefixWorkflow)
	meta.ID = id
	wf := domain.Workflow{Meta: meta, Name: name, Description: "Created by the platform", Version: 1, Status: "published", Definition: def, Checksum: sum, CreatedBy: "system"}
	if _, err := s.st.Workflows.Create(ctx, wf); err != nil && !errors.Is(err, store.ErrAlreadyExists) {
		return domain.Workflow{}, err
	}
	return wf, nil
}
