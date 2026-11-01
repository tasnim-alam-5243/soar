// Package migrate creates and updates index templates, indices, ISM retention
// policies and the migration ledger. Run it with `soar migrate`.
package migrate

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/yourorg/soar/internal/store/opensearch"
	"github.com/yourorg/soar/internal/store/opensearch/schema"
)

// Result is the outcome for one entity.
type Result struct {
	Entity string
	Alias  string
	Action string // created | updated | unchanged | ok | DRIFT | error
	Detail string
}

// Failed reports whether the result should make the command exit non-zero.
func (r Result) Failed() bool { return r.Action == "DRIFT" || r.Action == "error" }

// Runner applies or checks the schema.
type Runner struct {
	C    *opensearch.Client
	Opts schema.Options
	Log  *slog.Logger
}

func (r *Runner) ledgerAlias() string { return r.Opts.Prefix + "-migrations" }
func (r *Runner) ledgerIndex() string { return r.Opts.Prefix + "-migrations-v1" }

type ledgerDoc struct {
	Entity        string `json:"entity"`
	Checksum      string `json:"checksum"`
	SchemaVersion int    `json:"schema_version"`
	Action        string `json:"action"`
	AppliedAt     string `json:"applied_at"`
}

// Apply is idempotent: running it again with the same schema changes nothing.
func (r *Runner) Apply(ctx context.Context) ([]Result, error) {
	plans, err := schema.BuildAll(r.Opts)
	if err != nil {
		return nil, err
	}
	if err := r.ensureLedger(ctx); err != nil {
		return nil, err
	}
	var results []Result
	for _, p := range plans {
		res := Result{Entity: p.Entity.Name, Alias: p.Alias}
		action, err := r.applyOne(ctx, p)
		if err != nil {
			res.Action, res.Detail = "error", err.Error()
		} else {
			res.Action = action
		}
		r.Log.Info("migrate", "entity", res.Entity, "action", res.Action, "detail", res.Detail)
		results = append(results, res)
	}
	return results, nil
}

func (r *Runner) ensureLedger(ctx context.Context) error {
	status, _, err := r.C.Do(ctx, http.MethodHead, "/_alias/"+r.ledgerAlias(), nil, 404)
	if err != nil {
		return err
	}
	if status == 200 {
		return nil
	}
	body := map[string]any{
		"settings": map[string]any{"index": map[string]any{"number_of_shards": 1, "number_of_replicas": r.Opts.StateReplicas}},
		"aliases":  map[string]any{r.ledgerAlias(): map[string]any{}},
		"mappings": map[string]any{
			"dynamic": "strict",
			"properties": schema.Props{
				"entity": map[string]any{"type": "keyword"}, "checksum": map[string]any{"type": "keyword"},
				"schema_version": map[string]any{"type": "integer"}, "action": map[string]any{"type": "keyword"},
				"applied_at": map[string]any{"type": "date"},
			},
		},
	}
	_, _, err = r.C.Do(ctx, http.MethodPut, "/"+r.ledgerIndex(), body)
	return err
}

func (r *Runner) readLedger(ctx context.Context, entity string) (*ledgerDoc, error) {
	status, out, err := r.C.Do(ctx, http.MethodGet, "/"+r.ledgerAlias()+"/_source/"+entity, nil, 404)
	if err != nil {
		return nil, err
	}
	if status == 404 {
		return nil, nil
	}
	var d ledgerDoc
	if err := json.Unmarshal(out, &d); err != nil {
		return nil, err
	}
	return &d, nil
}

func (r *Runner) aliasExists(ctx context.Context, alias string) (bool, error) {
	status, _, err := r.C.Do(ctx, http.MethodHead, "/_alias/"+alias, nil, 404)
	return status == 200, err
}

func (r *Runner) applyOne(ctx context.Context, p schema.Plan) (string, error) {
	led, err := r.readLedger(ctx, p.Entity.Name)
	if err != nil {
		return "", err
	}
	exists, err := r.aliasExists(ctx, p.Alias)
	if err != nil {
		return "", err
	}
	if led != nil && led.Checksum == p.Checksum && exists {
		return "unchanged", nil
	}

	// 1. Retention policy first, so a freshly created index is attached by its ism_template.
	policyUpdated := false
	if p.Policy != nil {
		if policyUpdated, err = r.putPolicy(ctx, p); err != nil {
			return "", fmt.Errorf("ism policy: %w", err)
		}
	}
	// 2. Index template (applies to future indices, including rollover).
	if _, _, err = r.C.Do(ctx, http.MethodPut, "/_index_template/"+p.TemplateName, p.Template); err != nil {
		return "", fmt.Errorf("index template: %w", err)
	}

	action := "created"
	if !exists {
		aliasCfg := map[string]any{}
		if p.Entity.Kind == schema.TimeSeries {
			aliasCfg["is_write_index"] = true
		}
		if _, _, err = r.C.Do(ctx, http.MethodPut, "/"+p.BootstrapName, map[string]any{"aliases": map[string]any{p.Alias: aliasCfg}}); err != nil {
			return "", fmt.Errorf("create index: %w", err)
		}
	} else {
		action = "updated"
		// Additive changes only: OpenSearch rejects type changes with a clear error.
		if _, _, err = r.C.Do(ctx, http.MethodPut, "/"+p.Alias+"/_mapping", p.Mappings); err != nil {
			return "", fmt.Errorf("update mapping failed (incompatible change? create a v%d index and _reindex): %w", schema.Version+1, err)
		}
		if _, _, err = r.C.Do(ctx, http.MethodPut, "/"+p.Alias+"/_settings", p.Settings); err != nil {
			return "", fmt.Errorf("update settings: %w", err)
		}
	}

	// 3. Make sure the indices are managed by the policy (idempotent).
	if p.Policy != nil {
		pattern := p.TemplateName + "-*"
		if _, _, err = r.C.Do(ctx, http.MethodPost, "/_plugins/_ism/add/"+pattern, map[string]any{"policy_id": p.PolicyID}); err != nil {
			return "", fmt.Errorf("attach policy: %w", err)
		}
		if policyUpdated {
			// Already-managed indices keep the old policy version until told to switch.
			if _, _, err = r.C.Do(ctx, http.MethodPost, "/_plugins/_ism/change_policy/"+pattern, map[string]any{"policy_id": p.PolicyID}); err != nil {
				return "", fmt.Errorf("change policy: %w", err)
			}
		}
	}

	// 4. Record in the ledger last: a crash before this line just means the next run repeats the (idempotent) steps.
	doc := ledgerDoc{Entity: p.Entity.Name, Checksum: p.Checksum, SchemaVersion: schema.Version, Action: action,
		AppliedAt: time.Now().UTC().Format(time.RFC3339)}
	_, _, err = r.C.Do(ctx, http.MethodPut, "/"+r.ledgerAlias()+"/_doc/"+p.Entity.Name+"?refresh=true", doc)
	return action, err
}

// putPolicy creates or updates the ISM policy. It reports whether an existing policy was replaced.
func (r *Runner) putPolicy(ctx context.Context, p schema.Plan) (bool, error) {
	path := "/_plugins/_ism/policies/" + p.PolicyID
	status, out, err := r.C.Do(ctx, http.MethodGet, path, nil, 404)
	if err != nil {
		return false, err
	}
	if status == 404 {
		_, _, err = r.C.Do(ctx, http.MethodPut, path, p.Policy)
		return false, err
	}
	var cur struct {
		SeqNo       int64 `json:"_seq_no"`
		PrimaryTerm int64 `json:"_primary_term"`
	}
	if err := json.Unmarshal(out, &cur); err != nil {
		return false, err
	}
	_, _, err = r.C.Do(ctx, http.MethodPut, fmt.Sprintf("%s?if_seq_no=%d&if_primary_term=%d", path, cur.SeqNo, cur.PrimaryTerm), p.Policy)
	return true, err
}

// Check verifies the live cluster matches the schema without changing anything.
func (r *Runner) Check(ctx context.Context) ([]Result, error) {
	plans, err := schema.BuildAll(r.Opts)
	if err != nil {
		return nil, err
	}
	var results []Result
	for _, p := range plans {
		res := Result{Entity: p.Entity.Name, Alias: p.Alias, Action: "ok"}
		if problems := r.checkOne(ctx, p); len(problems) > 0 {
			res.Action, res.Detail = "DRIFT", strings.Join(problems, "; ")
		}
		results = append(results, res)
	}
	return results, nil
}

func (r *Runner) checkOne(ctx context.Context, p schema.Plan) []string {
	var problems []string
	led, err := r.readLedger(ctx, p.Entity.Name)
	switch {
	case err != nil:
		return []string{"ledger: " + err.Error()}
	case led == nil:
		problems = append(problems, "not migrated (no ledger entry)")
	case led.Checksum != p.Checksum:
		problems = append(problems, "schema changed since last migrate (run `soar migrate`)")
	}

	_, out, err := r.C.Do(ctx, http.MethodGet, "/"+p.Alias+"/_mapping", nil, 404)
	if err != nil {
		return append(problems, "mapping: "+err.Error())
	}
	var live map[string]struct {
		Mappings struct {
			Dynamic    any            `json:"dynamic"`
			Properties map[string]any `json:"properties"`
		} `json:"mappings"`
	}
	if err := json.Unmarshal(out, &live); err != nil || len(live) == 0 {
		return append(problems, "index/alias missing")
	}
	want := p.Mappings["properties"].(schema.Props)
	for idx, m := range live {
		if fmt.Sprint(m.Mappings.Dynamic) != "strict" {
			problems = append(problems, idx+": dynamic is not strict")
		}
		var missing []string
		for f := range want {
			if _, ok := m.Mappings.Properties[f]; !ok {
				missing = append(missing, f)
			}
		}
		sort.Strings(missing)
		if len(missing) > 0 {
			problems = append(problems, fmt.Sprintf("%s: missing fields %v", idx, missing))
		}
	}
	if p.Policy != nil {
		status, _, err := r.C.Do(ctx, http.MethodGet, "/_plugins/_ism/policies/"+p.PolicyID, nil, 404)
		if err != nil || status == 404 {
			problems = append(problems, "ISM policy "+p.PolicyID+" missing")
		}
	}
	return problems
}
