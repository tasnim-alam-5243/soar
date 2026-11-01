package schema

import "github.com/yourorg/soar/internal/domain"

// Kind separates mutable state indices from append-only time-series indices.
type Kind int

const (
	// State indices: single index plus alias, updated in place, read by ID.
	State Kind = iota
	// TimeSeries indices: rollover with a write alias and an ISM delete policy.
	TimeSeries
)

// Entity describes one logical index.
type Entity struct {
	Name           string // "alerts" -> index soar-alerts-v1, alias soar-alerts
	Kind           Kind
	RetentionClass string // key in retention.yaml; "" = not subject to retention
	Shards         int    // primary shards (time-series; state indices always use 1)
	Props          Props  // entity-specific fields (common fields are added automatically)
	Model          any    // zero value of the domain type, used by the drift test
}

// Version is the index generation: soar-<entity>-v<Version>.
const Version = 1

// Entities returns all indices in creation order.
func Entities() []Entity {
	return []Entity{
		{Name: "workflows", Kind: State, Model: domain.Workflow{}, Props: Props{
			"name": textKW(), "description": text(), "version": integer(), "status": kw(),
			"definition": blob(),
			"triggers":   obj(Props{"type": kw(), "kind": kw(), "match": stored()}),
			"checksum":   kw(), "created_by": kw(),
		}},
		{Name: "executions", Kind: State, RetentionClass: "executions", Model: domain.Execution{}, Props: Props{
			"workflow_id": kw(), "workflow_version": integer(), "status": kw(),
			"trigger": obj(Props{"type": kw(), "kind": kw(), "alert_id": kw(), "event_id": kw(), "user": kw()}),
			"context": blob(), "pending_nodes": kw(),
			"outbox": nested(Props{
				"key": kw(), "subject": kw(), "payload_ref": kw(),
				"published": boolean(), "created_at": date(), "published_at": date(),
			}),
			"lease":      obj(Props{"owner": kw(), "expires_at": date()}),
			"partition":  integer(),
			"error":      text(),
			"started_at": date(), "ended_at": date(), "deadline_at": date(),
		}},
		{Name: "steprun", Kind: TimeSeries, RetentionClass: "steprun", Shards: 1, Model: domain.StepRun{}, Props: Props{
			"execution_id": kw(), "node_id": kw(), "action": kw(), "attempt": integer(), "status": kw(),
			"input_ref": kw(), "output_ref": kw(),
			"error":       obj(Props{"code": kw(), "message": text(), "retryable": boolean()}),
			"duration_ms": long(), "idempotency_key": kw(), "parent_step_id": kw(), "actor_type": kw(),
			"started_at": date(), "ended_at": date(),
		}},
		{Name: "alerts", Kind: TimeSeries, RetentionClass: "alerts", Shards: 2, Model: domain.Alert{}, Props: Props{
			"event_id": kw(), "kind": kw(), "source_id": kw(), "system": kw(),
			"external_id": kw(), "external_url": stored(), "fingerprint": kw(),
			"title": textKW(), "description": text(), "summary": text(),
			"severity": integer(), "original_severity": kw(),
			"status": kw(), "category": kw(), "tags": kw(),
			"observables": obj(Props{"type": kw(), "value": kw(), "role": kw()}),
			"entities":    obj(Props{"hosts": kw(), "users": kw(), "accounts": kw()}),
			"mitre":       obj(Props{"technique_ids": kw(), "techniques": kw(), "tactics": kw()}),
			"verdict":     kw(), "ext": flat(), "raw_ref": kw(),
			"occurred_at": date(), "received_at": date(), "last_seen": date(),
			"occurrence_count": long(), "case_id": kw(),
		}},
		{Name: "cases", Kind: State, RetentionClass: "cases", Model: domain.Case{}, Props: Props{
			"title": textKW(), "summary": text(), "status": kw(), "severity": integer(),
			"assignee":  kw(),
			"sla":       obj(Props{"policy": kw(), "due_at": date(), "breached": boolean()}),
			"alert_ids": kw(), "tags": kw(), "verdict": kw(),
			"external_refs": obj(Props{
				"system": kw(), "id": kw(), "url": stored(), "last_synced_at": date(), "sync_version": long(),
			}),
			"closed_at": date(),
		}},
		{Name: "case-tasks", Kind: State, RetentionClass: "case_evidence", Model: domain.CaseTask{}, Props: Props{
			"case_id": kw(), "title": textKW(), "description": text(), "status": kw(),
			"assignee": kw(), "due_at": date(),
		}},
		{Name: "observables", Kind: State, RetentionClass: "observables", Model: domain.ObservableDoc{}, Props: Props{
			"type": kw(), "value": kw(), "first_seen": date(), "last_seen": date(),
			"reputation": obj(Props{"score": integer(), "verdict": kw(), "source": kw(), "checked_at": date()}),
			"case_ids":   kw(),
		}},
		{Name: "timeline", Kind: TimeSeries, RetentionClass: "timeline", Shards: 1, Model: domain.TimelineEvent{}, Props: Props{
			"subject_id": kw(), "event_type": kw(), "actor": kw(), "data": flat(),
		}},
		{Name: "audit", Kind: TimeSeries, RetentionClass: "audit", Shards: 1, Model: domain.AuditRecord{}, Props: Props{
			"actor": kw(), "actor_type": kw(), "action": kw(), "resource": kw(),
			"before_hash": kw(), "after_hash": kw(), "ip": ipAddr(), "request_id": kw(),
		}},
		{Name: "secrets", Kind: State, Model: domain.Secret{}, Props: Props{
			"name": kw(), "ciphertext": binary(), "key_id": kw(), "nonce": binary(), "scope": kw(),
		}},
		{Name: "principals", Kind: State, Model: domain.Principal{}, Props: Props{
			"type": kw(), "name": kw(), "roles": kw(), "key_hash": kw(), "disabled": boolean(),
		}},
		{Name: "idempotency", Kind: State, RetentionClass: "idempotency", Model: domain.Idempotency{}, Props: Props{
			"key": kw(), "status": kw(), "result_ref": kw(), "expires_at": date(),
		}},
		{Name: "timers", Kind: State, Model: domain.Timer{}, Props: Props{
			"execution_id": kw(), "node_id": kw(), "due_at": date(), "kind": kw(),
			"fired": boolean(), "fired_at": date(),
		}},
		{Name: "sources", Kind: State, Model: domain.Source{}, Props: Props{
			"name": kw(), "system": kw(), "adapter": kw(), "parser": kw(), "mapping_id": kw(),
			"auth": blob(), "schedule": kw(), "filters": kw(), "dedup": blob(), "rate_limit": blob(),
			"config": blob(), "enabled": boolean(),
			"health": obj(Props{
				"status": kw(), "last_event_at": date(), "rate_per_sec": double(),
				"error_rate": double(), "lag_seconds": double(),
			}),
		}},
		{Name: "mappings", Kind: State, Model: domain.Mapping{}, Props: Props{
			"name": kw(), "version": integer(), "kind": kw(), "definition": blob(), "checksum": kw(),
		}},
		{Name: "filters", Kind: State, Model: domain.FilterSet{}, Props: Props{
			"name": kw(), "version": integer(), "mode": kw(), "default": kw(),
			"rules": blob(), "checksum": kw(),
		}},
		{Name: "lists", Kind: State, Model: domain.List{}, Props: Props{
			"name": kw(), "items": kw(),
		}},
		{Name: "cursors", Kind: State, Model: domain.Cursor{}, Props: Props{
			"source_id": kw(), "cursor": stored(),
		}},
		{Name: "raw", Kind: TimeSeries, RetentionClass: "raw", Shards: 1, Model: domain.RawRecord{}, Props: Props{
			"source_id": kw(), "transport": kw(), "received_at": date(),
			"body": Props{"type": "text", "index": false}, "body_ref": kw(),
			"size": long(), "sha256": kw(), "parse_error": text(),
		}},
		{Name: "case-alerts", Kind: State, RetentionClass: "case_evidence", Model: domain.CaseAlert{}, Props: Props{
			"case_id": kw(), "alert_id": kw(), "snapshot": blob(), "linked_at": date(),
		}},
		{Name: "seen", Kind: State, RetentionClass: "seen", Model: domain.Seen{}, Props: Props{
			"key": kw(), "first_seen": date(),
		}},
		{Name: "connections", Kind: State, Model: domain.Connection{}, Props: Props{
			"connector_id": kw(), "name": kw(), "auth_type": kw(),
			"secret_refs": blob(), "config": blob(),
		}},
	}
}

// Fields returns the entity's full property set including common fields.
func (e Entity) Fields() Props { return merge(common(), e.Props) }
