package opensearch

import (
	"github.com/yourorg/soar/internal/domain"
	"github.com/yourorg/soar/internal/store"
	"github.com/yourorg/soar/internal/store/opensearch/schema"
)

// NewStore wires one repository per index in spec section 5. prefix is the
// index prefix the migration used (config SOAR_INDEX_PREFIX).
//
// Configuration objects (workflows, sources, mappings, filter sets, lists, connections)
// are written with refresh=wait_for unless o.Refresh says otherwise. They change rarely,
// and other roles and the API read them through searches (registry reloads, trigger
// index, filter tests), so a write must be visible to the next search: an analyst who
// edits a managed list and then runs POST /filters/test sees the new list.
func NewStore(c *Client, prefix string, o Options) *store.Store {
	n := store.Names{Prefix: prefix}
	cfg := o
	if cfg.Refresh == "" {
		cfg.Refresh = "wait_for"
	}
	return &store.Store{
		Names:       n,
		Workflows:   NewRepo[domain.Workflow](c, n.Index("workflows"), schema.State, cfg),
		Executions:  NewRepo[domain.Execution](c, n.Index("executions"), schema.State, o),
		StepRuns:    NewRepo[domain.StepRun](c, n.Index("steprun"), schema.TimeSeries, o),
		Alerts:      NewRepo[domain.Alert](c, n.Index("alerts"), schema.TimeSeries, o),
		Cases:       NewRepo[domain.Case](c, n.Index("cases"), schema.State, o),
		CaseTasks:   NewRepo[domain.CaseTask](c, n.Index("case-tasks"), schema.State, o),
		CaseAlerts:  NewRepo[domain.CaseAlert](c, n.Index("case-alerts"), schema.State, o),
		Observables: NewRepo[domain.ObservableDoc](c, n.Index("observables"), schema.State, o),
		Timeline:    NewRepo[domain.TimelineEvent](c, n.Index("timeline"), schema.TimeSeries, o),
		Audit:       NewRepo[domain.AuditRecord](c, n.Index("audit"), schema.TimeSeries, o),
		Secrets:     NewRepo[domain.Secret](c, n.Index("secrets"), schema.State, o),
		Principals:  NewRepo[domain.Principal](c, n.Index("principals"), schema.State, o),
		Idempotency: NewRepo[domain.Idempotency](c, n.Index("idempotency"), schema.State, o),
		Timers:      NewRepo[domain.Timer](c, n.Index("timers"), schema.State, o),
		Approvals:   NewRepo[domain.Approval](c, n.Index("approvals"), schema.State, o),
		Sources:     NewRepo[domain.Source](c, n.Index("sources"), schema.State, cfg),
		Mappings:    NewRepo[domain.Mapping](c, n.Index("mappings"), schema.State, cfg),
		Filters:     NewRepo[domain.FilterSet](c, n.Index("filters"), schema.State, cfg),
		Lists:       NewRepo[domain.List](c, n.Index("lists"), schema.State, cfg),
		Cursors:     NewRepo[domain.Cursor](c, n.Index("cursors"), schema.State, o),
		Raw:         NewRepo[domain.RawRecord](c, n.Index("raw"), schema.TimeSeries, o),
		Seen:        NewRepo[domain.Seen](c, n.Index("seen"), schema.State, o),
		Connections: NewRepo[domain.Connection](c, n.Index("connections"), schema.State, cfg),
	}
}
