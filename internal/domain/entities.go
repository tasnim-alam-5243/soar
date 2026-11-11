package domain

import "time"

// Meta is embedded in every stored document (spec section 5, "Common fields").
type Meta struct {
	ID            string    `json:"id"`
	CreatedAt     time.Time `json:"created_at"`
	UpdatedAt     time.Time `json:"updated_at"`
	SchemaVersion int       `json:"schema_version"`
}

// NewMeta returns Meta with a fresh ID for the given prefix and timestamps set to now.
func NewMeta(prefix string) Meta {
	now := time.Now().UTC()
	return Meta{ID: NewID(prefix), CreatedAt: now, UpdatedAt: now, SchemaVersion: SchemaVersion}
}

// Free-form JSON payloads (workflow definitions, execution context, vendor fields).
type JSON = map[string]any

// ---- Workflows and executions ----

type Trigger struct {
	Type  string `json:"type"`            // alert | schedule | manual | event
	Kind  string `json:"kind,omitempty"`  // event kind: alert, case_event, ...
	Match string `json:"match,omitempty"` // CEL condition
	Cron  string `json:"cron,omitempty"`  // schedule triggers: 5-field cron or @every
}

type Workflow struct {
	Meta
	Name        string    `json:"name"`
	Description string    `json:"description,omitempty"`
	Version     int       `json:"version"`
	Status      string    `json:"status"` // draft | published | archived
	Definition  JSON      `json:"definition"`
	Triggers    []Trigger `json:"triggers,omitempty"`
	Checksum    string    `json:"checksum,omitempty"`
	CreatedBy   string    `json:"created_by,omitempty"`
}

// OutboxEntry is one pending side effect of an execution (spec 8.4). It is saved in
// the same write as the state change that caused it and carried out afterwards.
type OutboxEntry struct {
	Key         string     `json:"key"`
	Kind        string     `json:"kind,omitempty"` // dispatch | timer | approval | start_child | notify_parent
	Subject     string     `json:"subject,omitempty"`
	Payload     JSON       `json:"payload,omitempty"` // the engine command
	PayloadRef  string     `json:"payload_ref,omitempty"`
	Published   bool       `json:"published"`
	CreatedAt   time.Time  `json:"created_at"`
	PublishedAt *time.Time `json:"published_at,omitempty"`
}

type Lease struct {
	Owner     string    `json:"owner"`
	ExpiresAt time.Time `json:"expires_at"`
}

type ExecutionTrigger struct {
	Type              string     `json:"type"` // alert | manual | schedule | subworkflow | retry | event
	Kind              string     `json:"kind,omitempty"`
	AlertID           string     `json:"alert_id,omitempty"`
	EventID           string     `json:"event_id,omitempty"`
	User              string     `json:"user,omitempty"`
	ParentExecutionID string     `json:"parent_execution_id,omitempty"`
	ParentNodeID      string     `json:"parent_node_id,omitempty"`
	RetryOf           string     `json:"retry_of,omitempty"`
	ScheduledFor      *time.Time `json:"scheduled_for,omitempty"`
}

type Execution struct {
	Meta
	WorkflowID       string           `json:"workflow_id"`
	WorkflowName     string           `json:"workflow_name,omitempty"`
	WorkflowVersion  int              `json:"workflow_version"`
	WorkflowChecksum string           `json:"workflow_checksum,omitempty"`
	Status           string           `json:"status"` // queued|running|waiting|succeeded|failed|cancelled|timed_out
	Trigger          ExecutionTrigger `json:"trigger"`
	Context          JSON             `json:"context,omitempty"` // engine state (inputs, vars, node states)
	PendingNodes     []string         `json:"pending_nodes,omitempty"`
	Outbox           []OutboxEntry    `json:"outbox,omitempty"`
	OutboxPending    bool             `json:"outbox_pending"` // any unpublished outbox entry (sweeper index)
	Lease            *Lease           `json:"lease,omitempty"`
	Partition        int              `json:"partition"`
	DryRun           bool             `json:"dry_run,omitempty"`
	Error            string           `json:"error,omitempty"`
	StartedAt        *time.Time       `json:"started_at,omitempty"`
	EndedAt          *time.Time       `json:"ended_at,omitempty"`
	DeadlineAt       *time.Time       `json:"deadline_at,omitempty"`
}

// Approval is a human decision requested by an approval node or by policy (spec 10, 20.1).
type Approval struct {
	Meta
	ExecutionID  string     `json:"execution_id"`
	NodeID       string     `json:"node_id"`
	WorkflowID   string     `json:"workflow_id,omitempty"`
	WorkflowName string     `json:"workflow_name,omitempty"`
	Partition    int        `json:"partition"`
	Approvers    []string   `json:"approvers"` // roles or user names allowed to decide
	Message      string     `json:"message,omitempty"`
	Action       string     `json:"action,omitempty"`  // action awaiting approval (policy approvals)
	Command      JSON       `json:"command,omitempty"` // the held-back dispatch (policy approvals)
	Risk         string     `json:"risk,omitempty"`
	Status       string     `json:"status"` // pending | approved | rejected | expired | cancelled
	DecidedBy    string     `json:"decided_by,omitempty"`
	DecidedAt    *time.Time `json:"decided_at,omitempty"`
	Comment      string     `json:"comment,omitempty"`
	DueAt        time.Time  `json:"due_at"`
}

type StepError struct {
	Code      string `json:"code"`
	Message   string `json:"message,omitempty"`
	Retryable bool   `json:"retryable"`
}

type StepRun struct {
	Meta
	ExecutionID    string     `json:"execution_id"`
	WorkflowID     string     `json:"workflow_id,omitempty"`
	NodeID         string     `json:"node_id"`
	Action         string     `json:"action,omitempty"`
	Attempt        int        `json:"attempt"`
	Item           int        `json:"item,omitempty"`
	Status         string     `json:"status"`
	Input          JSON       `json:"input,omitempty"`  // {"value": params}, secrets redacted (64 KB or less)
	Output         JSON       `json:"output,omitempty"` // {"value": output} (64 KB or less)
	InputRef       string     `json:"input_ref,omitempty"`
	OutputRef      string     `json:"output_ref,omitempty"`
	DryRun         bool       `json:"dry_run,omitempty"`
	Error          *StepError `json:"error,omitempty"`
	DurationMS     int64      `json:"duration_ms,omitempty"`
	IdempotencyKey string     `json:"idempotency_key,omitempty"`
	ParentStepID   string     `json:"parent_step_id,omitempty"`
	ActorType      string     `json:"actor_type,omitempty"` // user | workflow | agent | system
	StartedAt      *time.Time `json:"started_at,omitempty"`
	EndedAt        *time.Time `json:"ended_at,omitempty"`
}

// ---- Alerts, cases, observables ----

type Observable struct {
	Type  string `json:"type"` // ip, domain, url, hash, email, host, user, file_path
	Value string `json:"value"`
	Role  string `json:"role,omitempty"`
}

type Entities struct {
	Hosts    []string `json:"hosts,omitempty"`
	Users    []string `json:"users,omitempty"`
	Accounts []string `json:"accounts,omitempty"`
}

type Mitre struct {
	TechniqueIDs []string `json:"technique_ids,omitempty"`
	Techniques   []string `json:"techniques,omitempty"`
	Tactics      []string `json:"tactics,omitempty"`
}

type Alert struct {
	Meta
	EventID          string       `json:"event_id,omitempty"`
	Kind             string       `json:"kind"`
	SourceID         string       `json:"source_id"`
	System           string       `json:"system,omitempty"`
	ExternalID       string       `json:"external_id,omitempty"`
	ExternalURL      string       `json:"external_url,omitempty"`
	Fingerprint      string       `json:"fingerprint,omitempty"`
	Title            string       `json:"title"`
	Description      string       `json:"description,omitempty"`
	Summary          string       `json:"summary,omitempty"`
	Severity         int          `json:"severity"` // 1-5
	OriginalSeverity string       `json:"original_severity,omitempty"`
	Status           string       `json:"status,omitempty"`
	Category         string       `json:"category,omitempty"`
	Tags             []string     `json:"tags,omitempty"`
	Observables      []Observable `json:"observables,omitempty"`
	Entities         *Entities    `json:"entities,omitempty"`
	Mitre            *Mitre       `json:"mitre,omitempty"`
	Verdict          string       `json:"verdict,omitempty"` // true_positive|false_positive|benign|unknown
	Ext              JSON         `json:"ext,omitempty"`
	RawRef           string       `json:"raw_ref,omitempty"`
	OccurredAt       *time.Time   `json:"occurred_at,omitempty"`
	ReceivedAt       time.Time    `json:"received_at"`
	LastSeen         *time.Time   `json:"last_seen,omitempty"`
	OccurrenceCount  int64        `json:"occurrence_count"`
	CaseID           string       `json:"case_id,omitempty"`
}

type SLA struct {
	Policy   string     `json:"policy,omitempty"`
	DueAt    *time.Time `json:"due_at,omitempty"`
	Breached bool       `json:"breached"`
}

type ExternalRef struct {
	System       string     `json:"system"`
	ID           string     `json:"id"`
	URL          string     `json:"url,omitempty"`
	LastSyncedAt *time.Time `json:"last_synced_at,omitempty"`
	SyncVersion  int64      `json:"sync_version"`
}

type Case struct {
	Meta
	Title        string        `json:"title"`
	Summary      string        `json:"summary,omitempty"`
	Status       string        `json:"status"` // open | in_progress | closed
	Severity     int           `json:"severity"`
	Assignee     string        `json:"assignee,omitempty"`
	SLA          *SLA          `json:"sla,omitempty"`
	AlertIDs     []string      `json:"alert_ids,omitempty"`
	Tags         []string      `json:"tags,omitempty"`
	Verdict      string        `json:"verdict,omitempty"`
	ExternalRefs []ExternalRef `json:"external_refs,omitempty"`
	Evidence     []Evidence    `json:"evidence,omitempty"` // files in soar-evidence, deleted with the case
	ClosedAt     *time.Time    `json:"closed_at,omitempty"`
}

type CaseTask struct {
	Meta
	CaseID      string     `json:"case_id"`
	Title       string     `json:"title"`
	Description string     `json:"description,omitempty"`
	Status      string     `json:"status"`
	Assignee    string     `json:"assignee,omitempty"`
	DueAt       *time.Time `json:"due_at,omitempty"`
}

type CaseAlert struct {
	Meta
	CaseID   string    `json:"case_id"`
	AlertID  string    `json:"alert_id"`
	Snapshot JSON      `json:"snapshot"`
	LinkedAt time.Time `json:"linked_at"`
}

type Reputation struct {
	Score     int        `json:"score"`
	Verdict   string     `json:"verdict,omitempty"`
	Source    string     `json:"source,omitempty"`
	CheckedAt *time.Time `json:"checked_at,omitempty"`
}

type ObservableDoc struct {
	Meta
	Type       string      `json:"type"`
	Value      string      `json:"value"`
	FirstSeen  time.Time   `json:"first_seen"`
	LastSeen   time.Time   `json:"last_seen"`
	Reputation *Reputation `json:"reputation,omitempty"`
	CaseIDs    []string    `json:"case_ids,omitempty"`
}

type TimelineEvent struct {
	Meta
	SubjectID string `json:"subject_id"`
	EventType string `json:"event_type"`
	Actor     string `json:"actor,omitempty"`
	Data      JSON   `json:"data,omitempty"`
}

type AuditRecord struct {
	Meta
	Actor      string `json:"actor"`
	ActorType  string `json:"actor_type"`
	Action     string `json:"action"`
	Resource   string `json:"resource"`
	BeforeHash string `json:"before_hash,omitempty"`
	AfterHash  string `json:"after_hash,omitempty"`
	IP         string `json:"ip,omitempty"`
	RequestID  string `json:"request_id,omitempty"`
}

// ---- Security and runtime ----

type Secret struct {
	Meta
	Name       string `json:"name"`
	Ciphertext string `json:"ciphertext,omitempty"` // base64
	KeyID      string `json:"key_id"`
	WrappedKey string `json:"wrapped_key,omitempty"` // data key encrypted with the master key (base64)
	Nonce      string `json:"nonce,omitempty"`       // base64
	Scope      string `json:"scope,omitempty"`
}

type Principal struct {
	Meta
	Type     string   `json:"type"` // user | api_key | service
	Name     string   `json:"name"`
	Roles    []string `json:"roles"`
	KeyHash  string   `json:"key_hash,omitempty"`
	Disabled bool     `json:"disabled"`
}

type Idempotency struct {
	Meta
	Key       string    `json:"key"`
	Status    string    `json:"status"` // in_progress | done | failed
	ResultRef string    `json:"result_ref,omitempty"`
	Result    JSON      `json:"result,omitempty"` // the step result to republish on a duplicate delivery
	ExpiresAt time.Time `json:"expires_at"`
}

type Timer struct {
	Meta
	ExecutionID string     `json:"execution_id"`
	NodeID      string     `json:"node_id"`
	DueAt       time.Time  `json:"due_at"`
	Kind        string     `json:"kind"` // wait | retry | deadline | approval_timeout | event_timeout
	Attempt     int        `json:"attempt,omitempty"`
	Item        int        `json:"item,omitempty"`
	Partition   int        `json:"partition"`
	Fired       bool       `json:"fired"`
	FiredAt     *time.Time `json:"fired_at,omitempty"`
}

// ---- Ingestion configuration ----

type SourceHealth struct {
	Status      string     `json:"status,omitempty"`
	LastEventAt *time.Time `json:"last_event_at,omitempty"`
	RatePerSec  float64    `json:"rate_per_sec,omitempty"`
	ErrorRate   float64    `json:"error_rate,omitempty"`
	LagSeconds  float64    `json:"lag_seconds,omitempty"`
}

type Source struct {
	Meta
	Name      string        `json:"name"`
	System    string        `json:"system,omitempty"`
	Adapter   string        `json:"adapter"`
	Parser    string        `json:"parser"`
	MappingID string        `json:"mapping_id"`
	Auth      JSON          `json:"auth,omitempty"`
	Schedule  string        `json:"schedule,omitempty"`
	Filters   []string      `json:"filters,omitempty"`
	Dedup     JSON          `json:"dedup,omitempty"`
	RateLimit JSON          `json:"rate_limit,omitempty"`
	Config    JSON          `json:"config,omitempty"` // adapter-specific (request, pagination, cursor rules)
	Enabled   bool          `json:"enabled"`
	Health    *SourceHealth `json:"health,omitempty"`
}

type Mapping struct {
	Meta
	Name       string `json:"name"`
	Version    int    `json:"version"`
	Kind       string `json:"kind"`
	Definition JSON   `json:"definition"`
	Checksum   string `json:"checksum,omitempty"`
}

type FilterSet struct {
	Meta
	Name     string `json:"name"`
	Version  int    `json:"version"`
	Mode     string `json:"mode"`    // first_match | all
	Default  string `json:"default"` // keep | drop
	Rules    []JSON `json:"rules"`
	Checksum string `json:"checksum,omitempty"`
}

type List struct {
	Meta
	Name  string   `json:"name"`
	Items []string `json:"items"`
}

type Cursor struct {
	Meta
	SourceID       string     `json:"source_id"`
	Cursor         string     `json:"cursor"`
	LeaseOwner     string     `json:"lease_owner,omitempty"` // one collector polls a source at a time
	LeaseExpiresAt *time.Time `json:"lease_expires_at,omitempty"`
}

type RawRecord struct {
	Meta
	SourceID   string    `json:"source_id"`
	Transport  string    `json:"transport,omitempty"`
	ReceivedAt time.Time `json:"received_at"`
	Body       string    `json:"body,omitempty"`     // inline body (64 KB or less)
	BodyRef    string    `json:"body_ref,omitempty"` // object-storage reference for larger bodies
	Size       int64     `json:"size,omitempty"`
	SHA256     string    `json:"sha256,omitempty"`
	ParseError string    `json:"parse_error,omitempty"`
}

type Seen struct {
	Meta
	Key       string    `json:"key"`
	EventID   string    `json:"event_id,omitempty"` // lets a redelivered event recognise its own earlier attempt
	Applied   bool      `json:"applied,omitempty"`  // the event's alert increment is written: a redelivery must not repeat it
	FirstSeen time.Time `json:"first_seen"`
}

type Connection struct {
	Meta
	ConnectorID string `json:"connector_id"`
	Name        string `json:"name"`
	AuthType    string `json:"auth_type"`
	SecretRefs  JSON   `json:"secret_refs,omitempty"`
	Config      JSON   `json:"config,omitempty"`
}

// MetaPtr lets generic code reach the common fields of any stored document type
// (every document type embeds Meta, so the method is promoted to all of them).
func (m *Meta) MetaPtr() *Meta { return m }

// Event is the message published on events.<source>: a normalized alert plus the
// routing hints the processor needs, so it never has to look up the source.
type Event struct {
	EventID        string    `json:"event_id"`
	SourceID       string    `json:"source_id"`
	ReceivedAt     time.Time `json:"received_at"`
	DedupWindowSec int       `json:"dedup_window_sec"`
	Route          string    `json:"route,omitempty"` // workflow name chosen by a route filter
	Alert          Alert     `json:"alert"`           // ID and Meta are assigned by the processor
}

// Evidence is a file attached to a case (spec 5.1: soar-evidence bucket).
type Evidence struct {
	ID          string    `json:"id"`
	Name        string    `json:"name"`
	Ref         string    `json:"ref"` // blob://soar-evidence/...
	SHA256      string    `json:"sha256"`
	Size        int64     `json:"size"`
	ContentType string    `json:"content_type,omitempty"`
	AddedBy     string    `json:"added_by,omitempty"`
	AddedAt     time.Time `json:"added_at"`
}
