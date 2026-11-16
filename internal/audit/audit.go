package audit

import (
	"context"
	"log/slog"
	"time"

	"github.com/yourorg/soar/internal/domain"
	"github.com/yourorg/soar/internal/obs"
	"github.com/yourorg/soar/internal/store"
)

// Actor types (spec section 20.1).
const (
	ActorUser     = "user"
	ActorWorkflow = "workflow"
	ActorAgent    = "agent"
	ActorSystem   = "system"
)

// Actions recorded outside the API.
const (
	ActionSecretRead = "secret.read"
)

// Sink stores records (store.Repo[domain.AuditRecord] satisfies it).
type Sink interface {
	Create(ctx context.Context, doc domain.AuditRecord) (store.Version, error)
}

// Auditor writes audit records. The zero value and a nil *Auditor record nothing.
type Auditor struct {
	Sink Sink
	Log  *slog.Logger
}

// Record stores rec, filling in its id and time. A failure is logged and counted
// (soar_audit_failures_total) but not returned: the audited operation has already
// happened, and failing it afterwards would only hide that it did.
func (a *Auditor) Record(ctx context.Context, rec domain.AuditRecord) {
	if a == nil || a.Sink == nil {
		return
	}
	if rec.ID == "" {
		rec.Meta = domain.NewMeta("aud")
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	if _, err := a.Sink.Create(ctx, rec); err != nil {
		obs.AuditFailures.WithLabelValues(rec.ActorType).Inc()
		if a.Log != nil {
			a.Log.ErrorContext(ctx, "audit record not written", "actor", rec.Actor, "action", rec.Action,
				"resource", rec.Resource, "error", err.Error())
		}
	}
}

// WorkflowActor names a workflow as an audit actor.
func WorkflowActor(name, id string) string {
	if name == "" {
		name = id
	}
	return "workflow:" + name
}
