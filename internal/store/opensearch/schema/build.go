package schema

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
)

// Options controls how templates are rendered for an environment.
type Options struct {
	Prefix             string // index prefix, "soar" in production
	StateReplicas      int    // replicas for state indices (0 on a single dev node, 1 in production)
	TimeSeriesReplicas int
	Retention          Retention
}

// Plan is everything needed to create or update one entity.
type Plan struct {
	Entity        Entity
	Alias         string         // soar-alerts
	BootstrapName string         // soar-alerts-v1 (state) or soar-alerts-v1-000001 (time-series)
	TemplateName  string         // soar-alerts-v1
	Template      map[string]any // composable index template body
	Mappings      map[string]any // the mappings block alone (for PUT _mapping on existing indices)
	Settings      map[string]any // dynamic settings re-applied to existing indices
	PolicyID      string         // ISM policy id, "" for state indices
	Policy        map[string]any // ISM policy body, nil for state indices
	Checksum      string         // sha256 over template + policy
}

// RolloverMinSize and RolloverMaxAge apply to every time-series index.
const (
	RolloverMinSize = "30gb"
	RolloverMaxAge  = "1d"
)

// Build renders the plan for one entity.
func Build(e Entity, o Options) (Plan, error) {
	if o.Prefix == "" {
		return Plan{}, fmt.Errorf("schema: empty prefix")
	}
	base := fmt.Sprintf("%s-%s-v%d", o.Prefix, e.Name, Version)
	alias := fmt.Sprintf("%s-%s", o.Prefix, e.Name)

	mappings := map[string]any{
		"dynamic":    "strict",
		"_meta":      map[string]any{"entity": e.Name, "schema_version": Version},
		"properties": e.Fields(),
	}

	settings := map[string]any{}
	dynamic := map[string]any{}
	refresh := e.Refresh
	switch e.Kind {
	case State:
		if refresh == "" {
			refresh = "1s"
		}
		settings["index"] = map[string]any{"number_of_shards": 1, "number_of_replicas": o.StateReplicas, "refresh_interval": refresh}
		dynamic = map[string]any{"index": map[string]any{"number_of_replicas": o.StateReplicas, "refresh_interval": refresh}}
	case TimeSeries:
		shards := e.Shards
		if shards < 1 {
			shards = 1
		}
		if refresh == "" {
			refresh = "5s"
		}
		settings["index"] = map[string]any{
			"number_of_shards": shards, "number_of_replicas": o.TimeSeriesReplicas,
			"refresh_interval": refresh, "codec": "best_compression",
		}
		settings["plugins.index_state_management.rollover_alias"] = alias
		dynamic = map[string]any{"index": map[string]any{"number_of_replicas": o.TimeSeriesReplicas, "refresh_interval": refresh}}
	}

	p := Plan{
		Entity: e, Alias: alias, TemplateName: base, Mappings: mappings, Settings: dynamic,
		BootstrapName: base,
		Template: map[string]any{
			"index_patterns": []string{base + "*"},
			"priority":       200,
			"version":        Version,
			"_meta":          map[string]any{"managed_by": "soar migrate", "entity": e.Name},
			"template":       map[string]any{"settings": settings, "mappings": mappings},
		},
	}

	if e.Kind == TimeSeries {
		p.BootstrapName = base + "-000001"
		p.PolicyID = fmt.Sprintf("%s-%s-retention", o.Prefix, e.Name)
		age := o.Retention.For(e.RetentionClass)
		if age == TTL {
			return Plan{}, fmt.Errorf("schema: time-series entity %q cannot use ttl retention", e.Name)
		}
		p.Policy = map[string]any{"policy": map[string]any{
			"description":   fmt.Sprintf("SOAR %s: rollover at %s or %s, delete after %s", e.Name, RolloverMinSize, RolloverMaxAge, age),
			"default_state": "hot",
			"states": []any{
				map[string]any{
					"name":        "hot",
					"actions":     []any{map[string]any{"rollover": map[string]any{"min_size": RolloverMinSize, "min_index_age": RolloverMaxAge}}},
					"transitions": []any{map[string]any{"state_name": "delete", "conditions": map[string]any{"min_index_age": age}}},
				},
				map[string]any{
					"name":        "delete",
					"actions":     []any{map[string]any{"delete": map[string]any{}}},
					"transitions": []any{},
				},
			},
			"ism_template": []any{map[string]any{"index_patterns": []string{base + "-*"}, "priority": 100}},
		}}
	}

	sum := sha256.New()
	for _, v := range []any{p.Template, p.Policy, p.BootstrapName} {
		b, err := json.Marshal(v) // map keys are sorted, so this is deterministic
		if err != nil {
			return Plan{}, err
		}
		sum.Write(b)
	}
	p.Checksum = hex.EncodeToString(sum.Sum(nil))
	return p, nil
}

// BuildAll renders every entity.
func BuildAll(o Options) ([]Plan, error) {
	var out []Plan
	for _, e := range Entities() {
		p, err := Build(e, o)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, nil
}
