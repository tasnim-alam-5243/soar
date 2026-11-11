// Package worker runs actions (spec section 10, "Worker algorithm"):
//
//	pull step -> idempotency check (done? republish the stored result, ack)
//	-> mark in_progress -> resolve secrets and connection -> execute with limits
//	-> write step run + idempotency result -> publish result -> ack
//
// Secrets are decrypted here and nowhere else (spec rule 3.1.7); their values are
// scrubbed from outputs, errors and log lines, and every read is audited.
package worker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/nats-io/nats.go/jetstream"

	"github.com/yourorg/soar/internal/actions"
	"github.com/yourorg/soar/internal/audit"
	"github.com/yourorg/soar/internal/blob"
	"github.com/yourorg/soar/internal/domain"
	"github.com/yourorg/soar/internal/engine"
	"github.com/yourorg/soar/internal/obs"
	"github.com/yourorg/soar/internal/queue"
	"github.com/yourorg/soar/internal/store"
)

// StepPayload is the payload of a "step" envelope: the engine's dispatch plus the
// context the orchestrator adds.
type StepPayload struct {
	engine.Dispatch
	WorkflowID   string `json:"workflow_id,omitempty"`
	WorkflowName string `json:"workflow_name,omitempty"`
	AlertID      string `json:"alert_id,omitempty"`
}

// ResultPayload is the payload of a "step_result" envelope.
type ResultPayload struct {
	Status     string            `json:"status"` // succeeded | failed
	Output     any               `json:"output,omitempty"`
	OutputRef  string            `json:"output_ref,omitempty"`
	Error      *engine.StepError `json:"error,omitempty"`
	DurationMS int64             `json:"duration_ms,omitempty"`
}

// Secrets decrypts named secrets (implemented by the security package).
type Secrets interface {
	Resolve(ctx context.Context, name string) (string, error)
}

// Publisher is the part of the queue client the worker needs.
type Publisher interface {
	Publish(ctx context.Context, m queue.Msg) error
}

// Config tunes the worker.
type Config struct {
	Priorities     []string       // default all
	Classes        []string       // default all
	Concurrency    map[string]int // per priority; default critical 8, normal 16, bulk 4
	AckWait        time.Duration  // heartbeat deadline (default 60s); heartbeats every AckWait/3
	IdempotencyTTL time.Duration  // how long a finished step's result is remembered (default 7d)
	NakDelay       time.Duration
	HoldMax        time.Duration // longest pause between attempts while OpenSearch is unavailable (default 15s)
}

func (c *Config) defaults() {
	if len(c.Priorities) == 0 {
		c.Priorities = queue.Priorities
	}
	if len(c.Classes) == 0 {
		c.Classes = queue.Classes
	}
	if c.Concurrency == nil {
		c.Concurrency = map[string]int{}
	}
	for p, n := range map[string]int{"critical": 8, "normal": 16, "bulk": 4} {
		if c.Concurrency[p] <= 0 {
			c.Concurrency[p] = n
		}
	}
	if c.AckWait <= 0 {
		c.AckWait = 60 * time.Second
	}
	if c.IdempotencyTTL <= 0 {
		c.IdempotencyTTL = 7 * 24 * time.Hour
	}
	if c.NakDelay <= 0 {
		c.NakDelay = 5 * time.Second
	}
	if c.HoldMax <= 0 {
		c.HoldMax = 15 * time.Second
	}
}

// Counters for health output and tests.
type Counters struct {
	Executed, Succeeded, Failed, Replayed, Retried atomic.Int64
	Held                                           atomic.Int64 // steps held while OpenSearch was unavailable
}

// Worker executes steps.
type Worker struct {
	Q        Publisher
	St       *store.Store
	Registry *actions.Registry
	Secrets  Secrets
	Blob     blob.Blob      // nil: large outputs stay inline
	Audit    *audit.Auditor // nil: nothing is audited (tests)
	Log      *slog.Logger
	Cfg      Config
	Stats    Counters
	Now      func() time.Time
}

func (w *Worker) now() time.Time {
	if w.Now != nil {
		return w.Now()
	}
	return time.Now().UTC()
}

// Consumers lists the (priority, class) pairs this worker serves.
func (w *Worker) Consumers() [][2]string {
	w.Cfg.defaults()
	var out [][2]string
	for _, p := range w.Cfg.Priorities {
		for _, c := range w.Cfg.Classes {
			out = append(out, [2]string{p, c})
		}
	}
	return out
}

// Run pulls from every consumer until ctx ends. Each priority has its own capacity,
// so bulk enrichment cannot starve critical response actions (spec 8.7).
func (w *Worker) Run(ctx context.Context, consumers map[[2]string]jetstream.Consumer) error {
	w.Cfg.defaults()
	sems := map[string]chan struct{}{}
	for _, p := range w.Cfg.Priorities {
		sems[p] = make(chan struct{}, w.Cfg.Concurrency[p])
	}
	var wg sync.WaitGroup
	for key, cons := range consumers {
		wg.Add(1)
		go func(prio string, cons jetstream.Consumer) {
			defer wg.Done()
			sem := sems[prio]
			for ctx.Err() == nil {
				free := cap(sem) - len(sem)
				if free <= 0 {
					sleep(ctx, 50*time.Millisecond)
					continue
				}
				batch, err := cons.Fetch(free, jetstream.FetchMaxWait(time.Second))
				if err != nil {
					if ctx.Err() == nil {
						w.Log.Warn("fetch failed", "error", err.Error())
						sleep(ctx, time.Second)
					}
					continue
				}
				for m := range batch.Messages() {
					sem <- struct{}{}
					wg.Add(1)
					go func(m jetstream.Msg) {
						defer func() { <-sem; wg.Done() }()
						w.Handle(ctx, m)
					}(m)
				}
			}
		}(key[0], cons)
	}
	wg.Wait()
	return nil
}

func sleep(ctx context.Context, d time.Duration) {
	select {
	case <-ctx.Done():
	case <-time.After(d):
	}
}

// Msg is the part of jetstream.Msg the worker uses (tests pass fakes).
type Msg interface {
	Data() []byte
	Subject() string
	InProgress() error
	NakWithDelay(time.Duration) error
	Term() error
	DoubleAck(context.Context) error
}

// Handle processes one step message. It always settles the message.
func (w *Worker) Handle(ctx context.Context, m Msg) {
	w.Cfg.defaults()
	var env queue.Envelope
	var sp StepPayload
	if err := json.Unmarshal(m.Data(), &env); err != nil || env.Type != queue.MsgStep || json.Unmarshal(env.Payload, &sp) != nil || sp.Action == "" || sp.ExecutionID == "" {
		w.Log.Error("invalid step message; terminating it", "subject", m.Subject())
		_ = m.Term()
		return
	}
	log := w.Log.With("execution_id", sp.ExecutionID, "node_id", sp.NodeID, "action", sp.Action, "attempt", sp.Attempt, "item", sp.Item)

	// keep the message while the step runs
	hbCtx, stopHB := context.WithCancel(ctx)
	defer stopHB()
	go func() {
		t := time.NewTicker(w.Cfg.AckWait / 3)
		defer t.Stop()
		for {
			select {
			case <-hbCtx.Done():
				return
			case <-t.C:
				_ = m.InProgress()
			}
		}
	}()

	res, retryLater, err := w.process(ctx, env, sp, log)
	// OpenSearch unavailable (idempotency lookup or mark, i.e. before the action ran):
	// hold the step here, kept alive by the heartbeat above, instead of nak-ing it. A step
	// has 3 deliveries, so a nak per attempt dead-lettered it after ~10 s of outage and
	// failed its execution. Retrying here is exactly a redelivery that costs nothing.
	if err != nil && retryLater && store.Unavailable(err) && ctx.Err() == nil {
		w.Stats.Held.Add(1)
		obs.StoreHolds.WithLabelValues("worker").Inc()
		log.Warn("OpenSearch is unavailable: holding the step until it answers again", "error", err.Error())
		held := time.Now()
		wait := store.Waiter{Max: w.Cfg.HoldMax}
		for err != nil && retryLater && store.Unavailable(err) && wait.Wait(ctx) == nil {
			res, retryLater, err = w.process(ctx, env, sp, log)
		}
		if err == nil {
			log.Info("OpenSearch answers again: step resumed", "held_for", time.Since(held).Round(time.Second).String())
		}
	}
	if err != nil {
		if retryLater {
			log.Warn("step could not be processed; it will be redelivered", "error", err.Error())
			w.Stats.Retried.Add(1)
			obs.StepRetries.WithLabelValues(sp.Action).Inc()
			_ = m.NakWithDelay(w.Cfg.NakDelay)
			return
		}
		log.Error("step failed permanently before running", "error", err.Error())
	}
	if err := w.publishResult(ctx, env, sp, res); err != nil {
		log.Warn("could not publish the result; the step will be redelivered and answered from the idempotency store", "error", err.Error())
		_ = m.NakWithDelay(w.Cfg.NakDelay)
		return
	}
	actx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := m.DoubleAck(actx); err != nil {
		log.Warn("ack not confirmed; a redelivery will be answered from the idempotency store", "error", err.Error())
	}
}

// process returns the result to publish. retryLater with an error means "nak, try again".
func (w *Worker) process(ctx context.Context, env queue.Envelope, sp StepPayload, log *slog.Logger) (ResultPayload, bool, error) {
	act, ok := w.Registry.Get(sp.Action)
	if !ok {
		return failure("unknown_action", "this worker has no action "+sp.Action, false), false, nil
	}
	man := act.Manifest()
	idemID := domain.DeterministicID("idm", sp.IdempotencyKey)

	// 1. idempotency: a finished step is answered from the store, never re-executed
	rec, _, err := w.St.Idempotency.Get(ctx, idemID)
	switch {
	case err == nil && rec.Status == "done":
		w.Stats.Replayed.Add(1)
		obs.StepReplays.WithLabelValues(sp.Action).Inc()
		var r ResultPayload
		if b, _ := json.Marshal(rec.Result); json.Unmarshal(b, &r) == nil && r.Status != "" {
			log.Info("step already done; republishing its result")
			return r, false, nil
		}
	case err == nil && rec.Status == "in_progress" && man.AtMostOnce:
		// an earlier delivery started this side effect and its outcome is unknown
		return failure("at_most_once_interrupted", "an earlier attempt was interrupted; the action is at-most-once, so it is not repeated automatically", false), false, nil
	case err != nil && !errors.Is(err, store.ErrNotFound):
		return ResultPayload{}, true, fmt.Errorf("idempotency lookup: %w", err)
	}
	now := w.now()
	mark := domain.Idempotency{Meta: domain.Meta{ID: idemID, CreatedAt: now, UpdatedAt: now}, Key: sp.IdempotencyKey, Status: "in_progress", ExpiresAt: now.Add(w.Cfg.IdempotencyTTL)}
	if man.AtMostOnce {
		if _, err := w.St.Idempotency.Create(ctx, mark); err != nil {
			if errors.Is(err, store.ErrAlreadyExists) {
				return failure("at_most_once_interrupted", "another attempt of this at-most-once action already started", false), false, nil
			}
			return ResultPayload{}, true, fmt.Errorf("idempotency mark: %w", err)
		}
	} else if _, err := w.St.Idempotency.Put(ctx, mark); err != nil {
		return ResultPayload{}, true, fmt.Errorf("idempotency mark: %w", err)
	}

	// 2. run
	started := w.now()
	out, aerr := w.execute(ctx, act, man, sp, log)
	res := ResultPayload{DurationMS: time.Since(started).Milliseconds()}
	if aerr != nil {
		retryable := (aerr.Retryable || man.Retryable(aerr.Code)) && !man.AtMostOnce
		res.Status, res.Error = engine.NodeFailed, &engine.StepError{Code: aerr.Code, Message: aerr.Message, Retryable: retryable}
		w.Stats.Failed.Add(1)
	} else {
		res.Status = engine.NodeSucceeded
		inline, ref, err := blob.Offload(ctx, w.Blob, blob.BucketPayloads, out)
		if err != nil {
			return ResultPayload{}, true, fmt.Errorf("store large output: %w", err)
		}
		res.Output, res.OutputRef = inline, ref
		w.Stats.Succeeded.Add(1)
	}
	w.Stats.Executed.Add(1)
	obs.StepDuration.WithLabelValues(sp.Action, res.Status).Observe(float64(res.DurationMS) / 1000)

	// 3. record: step run (history) and idempotency result (replay source)
	w.writeStepRun(ctx, env, sp, man, res, started, log)
	status := "done"
	if res.Status != engine.NodeSucceeded {
		status = "failed"
	}
	var resultJSON domain.JSON
	b, _ := json.Marshal(res)
	_ = json.Unmarshal(b, &resultJSON)
	if len(b) > blob.InlineLimit {
		resultJSON = map[string]any{"status": res.Status, "output_ref": res.OutputRef}
	}
	mark.Status, mark.Result, mark.UpdatedAt = status, resultJSON, w.now()
	if _, err := w.St.Idempotency.Put(ctx, mark); err != nil {
		log.Warn("could not store the idempotency result; a redelivery would run the action again", "error", err.Error())
	}
	return res, false, nil
}

func failure(code, msg string, retryable bool) ResultPayload {
	return ResultPayload{Status: engine.NodeFailed, Error: &engine.StepError{Code: code, Message: msg, Retryable: retryable}}
}

// execute runs one action. Secret values it used are scrubbed from its output, its
// error and its log lines; secret reads and completed side effects are audited.
func (w *Worker) execute(ctx context.Context, act actions.Action, man actions.Manifest, sp StepPayload, log *slog.Logger) (any, *actions.Error) {
	used := &secretSet{}
	out, aerr := w.run(ctx, act, man, sp, log, used)
	if !used.empty() {
		var hit bool
		if aerr != nil {
			scrubbed := *aerr
			scrubbed.Message, hit = used.text(aerr.Message)
			aerr = &scrubbed
		} else {
			out, hit = used.value(out)
		}
		if hit {
			obs.SecretsScrubbed.WithLabelValues(sp.Action).Inc()
			log.Warn("a secret value appeared in the action's result and was replaced with ***")
		}
	}
	if aerr == nil && !sp.DryRun && man.SideEffect != "read" {
		w.Audit.Record(ctx, w.auditRecord(sp, sp.Action, actionResource(man, sp, out)))
	}
	return out, aerr
}

func (w *Worker) auditRecord(sp StepPayload, action, resource string) domain.AuditRecord {
	return domain.AuditRecord{
		Actor: audit.WorkflowActor(sp.WorkflowName, sp.WorkflowID), ActorType: audit.ActorWorkflow,
		Action: action, Resource: resource, RequestID: sp.IdempotencyKey,
	}
}

// actionResource names what a completed action changed, for its audit record.
func actionResource(man actions.Manifest, sp StepPayload, out any) string {
	m, _ := out.(map[string]any)
	prefix := ""
	if man.Connector != "" {
		prefix = man.Connector + "/"
	}
	for _, k := range []string{"case_id", "alert_id"} {
		if id := m[k]; id != nil && fmt.Sprint(id) != "" {
			return prefix + strings.TrimSuffix(k, "_id") + "s/" + fmt.Sprint(id)
		}
		if id, ok := sp.Params[k].(string); ok && id != "" {
			return prefix + strings.TrimSuffix(k, "_id") + "s/" + id
		}
	}
	return "actions/" + man.ID
}

func (w *Worker) run(ctx context.Context, act actions.Action, man actions.Manifest, sp StepPayload, log *slog.Logger, used *secretSet) (any, *actions.Error) {
	params := sp.Params
	if params == nil {
		params = map[string]any{}
	}
	connName, _ := params["connection"].(string)
	if e := actions.CheckParams(man, params); e != nil {
		return nil, e
	}
	if sp.DryRun && man.SideEffect != "read" && !man.SupportsDryRun {
		return map[string]any{"dry_run": true, "skipped": true, "reason": "this action has side effects and does not support dry runs"}, nil
	}
	// secrets referenced by expressions (secrets.<name>) are filled in only now
	if names := engine.SecretNames(params); len(names) > 0 {
		if w.Secrets == nil {
			return nil, actions.Errf("missing_secret", false, "no secret store is configured")
		}
		resolved, err := engine.ResolveSecrets(params, func(n string) (string, error) {
			v, err := w.Secrets.Resolve(ctx, n)
			if err == nil {
				used.add(v)
				w.Audit.Record(ctx, w.auditRecord(sp, audit.ActionSecretRead, "secrets/"+n))
			}
			return v, err
		})
		if err != nil {
			return nil, actions.Errf("missing_secret", false, "%v", err)
		}
		params = resolved.(map[string]any)
	}
	in := actions.Input{
		ExecutionID: sp.ExecutionID, WorkflowID: sp.WorkflowID, NodeID: sp.NodeID, AlertID: sp.AlertID,
		Attempt: sp.Attempt, Item: sp.Item, IdempotencyKey: sp.IdempotencyKey, DryRun: sp.DryRun,
		Log: func(s string) {
			if clean, hit := used.text(s); hit {
				s = clean
				obs.SecretsScrubbed.WithLabelValues(sp.Action).Inc()
			}
			log.Info("action log", "line", s)
		},
	}
	if man.NeedsConnection || man.Connector != "" {
		conn, err := w.connection(ctx, man, connName)
		if err != nil {
			return nil, err
		}
		if len(conn.Secrets) > 0 {
			for _, v := range conn.Secrets {
				used.add(v)
			}
			w.Audit.Record(ctx, w.auditRecord(sp, audit.ActionSecretRead, "connections/"+conn.Name))
		}
		in.Connection = conn
		delete(params, "connection")
	}
	in.Params = params

	timeout := time.Duration(sp.TimeoutMS) * time.Millisecond
	if timeout <= 0 {
		timeout = engine.DefaultStepTimeout
	}
	if timeout > engine.MaxStepTimeout {
		timeout = engine.MaxStepTimeout
	}
	tctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	out, err := safeExecute(tctx, act, in)
	if err != nil {
		if errors.Is(tctx.Err(), context.DeadlineExceeded) {
			return nil, actions.Errf("timeout", true, "the action did not finish within %s", timeout)
		}
		return nil, actions.AsError(err)
	}
	if e := actions.CheckOutput(man, out); e != nil {
		return nil, e
	}
	return out, nil
}

// safeExecute turns a panic in action code into a failure instead of killing the worker.
func safeExecute(ctx context.Context, act actions.Action, in actions.Input) (out any, err error) {
	defer func() {
		if p := recover(); p != nil {
			err = actions.Errf("action_panic", false, "%v", p)
		}
	}()
	return act.Execute(ctx, in)
}

// connection resolves the connection a connector action uses: the one named in the
// "connection" parameter, or the only connection of that connector.
func (w *Worker) connection(ctx context.Context, man actions.Manifest, name string) (*actions.Connection, *actions.Error) {
	var c domain.Connection
	switch {
	case name != "":
		doc, _, err := w.St.Connections.Get(ctx, name)
		if err == nil {
			c = doc
			break
		}
		if !errors.Is(err, store.ErrNotFound) {
			return nil, actions.Errf("storage", true, "load connection: %v", err)
		}
		page, err := w.St.Connections.List(ctx, store.ListQuery{Terms: map[string][]string{"name": {name}}, Limit: 2})
		if err != nil {
			return nil, actions.Errf("storage", true, "load connection: %v", err)
		}
		if len(page.Items) == 0 {
			return nil, actions.Errf("invalid_connection", false, "no connection named %q", name)
		}
		c = page.Items[0]
	default:
		page, err := w.St.Connections.List(ctx, store.ListQuery{Terms: map[string][]string{"connector_id": {man.Connector}}, Limit: 2})
		if err != nil {
			return nil, actions.Errf("storage", true, "load connection: %v", err)
		}
		switch len(page.Items) {
		case 0:
			return nil, actions.Errf("invalid_connection", false, "no connection is configured for %s", man.Connector)
		case 1:
			c = page.Items[0]
		default:
			return nil, actions.Errf("invalid_connection", false, "several %s connections exist; name one with the connection parameter", man.Connector)
		}
	}
	if man.Connector != "" && c.ConnectorID != man.Connector {
		return nil, actions.Errf("invalid_connection", false, "connection %q belongs to %s, not %s", c.Name, c.ConnectorID, man.Connector)
	}
	conn := &actions.Connection{ID: c.ID, Name: c.Name, Config: c.Config, Secrets: map[string]string{}}
	for key, ref := range c.SecretRefs {
		r, _ := ref.(string)
		if r == "" {
			continue
		}
		if w.Secrets == nil {
			return nil, actions.Errf("missing_secret", false, "no secret store is configured")
		}
		v, err := w.Secrets.Resolve(ctx, r)
		if err != nil {
			return nil, actions.Errf("missing_secret", false, "connection %q secret %q: %v", c.Name, key, err)
		}
		conn.Secrets[key] = v
	}
	return conn, nil
}

func (w *Worker) writeStepRun(ctx context.Context, env queue.Envelope, sp StepPayload, man actions.Manifest, res ResultPayload, started time.Time, log *slog.Logger) {
	ended := w.now()
	id := domain.DeterministicID(domain.PrefixStepRun, env.ID)
	sr := domain.StepRun{
		Meta:        domain.Meta{ID: id, CreatedAt: ended, UpdatedAt: ended, SchemaVersion: domain.SchemaVersion},
		ExecutionID: sp.ExecutionID, WorkflowID: sp.WorkflowID, NodeID: sp.NodeID, Action: sp.Action, Attempt: sp.Attempt, Item: sp.Item,
		Status: res.Status, IdempotencyKey: sp.IdempotencyKey, ActorType: "workflow", DryRun: sp.DryRun,
		DurationMS: res.DurationMS, StartedAt: &started, EndedAt: &ended, OutputRef: res.OutputRef,
	}
	if in, _ := json.Marshal(engine.RedactSecrets(map[string]any(sp.Params))); len(in) <= blob.InlineLimit {
		sr.Input = map[string]any{"value": engine.RedactSecrets(map[string]any(sp.Params))}
	}
	if res.Output != nil {
		sr.Output = map[string]any{"value": res.Output}
	}
	if res.Error != nil {
		sr.Error = &domain.StepError{Code: res.Error.Code, Message: res.Error.Message, Retryable: res.Error.Retryable}
	}
	if _, err := w.St.StepRuns.Create(ctx, sr); err != nil && !errors.Is(err, store.ErrAlreadyExists) {
		log.Warn("could not write step run", "error", err.Error())
	}
	_ = man
}

func (w *Worker) publishResult(ctx context.Context, env queue.Envelope, sp StepPayload, res ResultPayload) error {
	out, err := queue.NewEnvelope(queue.MsgStepResult, "res:"+env.ID, sp.ExecutionID, env.Partition, res)
	if err != nil {
		return err
	}
	out.NodeID, out.Attempt, out.Item, out.IdempotencyKey, out.Trace = sp.NodeID, sp.Attempt, sp.Item, sp.IdempotencyKey, env.Trace
	msg, err := out.Msg(queue.ResultSubject(env.Partition))
	if err != nil {
		return err
	}
	return w.Q.Publish(ctx, msg)
}

// OnDeadLetter reports a step whose deliveries were exhausted (the worker kept
// crashing on it) as failed, so its execution can take its error path instead of
// waiting for the deadline.
func (w *Worker) OnDeadLetter(ctx context.Context, dl queue.DeadLetter) {
	var env queue.Envelope
	var sp StepPayload
	if json.Unmarshal(dl.Data, &env) != nil || json.Unmarshal(env.Payload, &sp) != nil || sp.ExecutionID == "" {
		return
	}
	obs.DLQMessages.WithLabelValues("steps").Inc()
	res := failure("delivery_failed", fmt.Sprintf("the step could not be delivered after %d attempts (see the DLQ)", dl.Deliveries), false)
	if err := w.publishResult(ctx, env, sp, res); err != nil {
		w.Log.Error("could not report a dead-lettered step", "execution_id", sp.ExecutionID, "node_id", sp.NodeID, "error", err.Error())
	}
}

// ClassOf maps a manifest class to the NATS subject class.
func ClassOf(m actions.Manifest) string {
	if strings.EqualFold(m.Class, actions.ClassPython) {
		return actions.ClassPython
	}
	return actions.ClassBuiltin
}
