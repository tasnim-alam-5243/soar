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

type OutboxEntry struct {
	Key         string     `json:"key"`
	Subject     string     `json:"subject"`
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
	Type    string `json:"type"`
	Kind    string `json:"kind,omitempty"`
	AlertID string `json:"alert_id,omitempty"`
	EventID string `json:"event_id,omitempty"`
	User    string `json:"user,omitempty"`
}

type Execution struct {
	Meta
	WorkflowID      string           `json:"workflow_id"`
	WorkflowVersion int              `json:"workflow_version"`
	Status          string           `json:"status"` // queued|running|waiting|succeeded|failed|cancelled|timed_out
	Trigger         ExecutionTrigger `json:"trigger"`
	Context         JSON             `json:"context,omitempty"`
	PendingNodes    []string         `json:"pending_nodes,omitempty"`
	Outbox          []OutboxEntry    `json:"outbox,omitempty"`
	Lease           *Lease           `json:"lease,omitempty"`
	Partition       int              `json:"partition"`
	Error           string           `json:"error,omitempty"`
	StartedAt       *time.Time       `json:"started_at,omitempty"`
	EndedAt         *time.Time       `json:"ended_at,omitempty"`
	DeadlineAt      *time.Time       `json:"deadline_at,omitempty"`
}

type StepError struct {
	Code      string `json:"code"`
	Message   string `json:"message,omitempty"`
	Retryable bool   `json:"retryable"`
}

type StepRun struct {
	Meta
	ExecutionID    string     `json:"execution_id"`
	NodeID         string     `json:"node_id"`
	Action         string     `json:"action,omitempty"`
	Attempt        int        `json:"attempt"`
	Status         string     `json:"status"`
	InputRef       string     `json:"input_ref,omitempty"`
	OutputRef      string     `json:"output_ref,omitempty"`
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
	Ciphertext string `json:"ciphertext"` // base64
	KeyID      string `json:"key_id"`
	Nonce      string `json:"nonce"` // base64
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
	ExpiresAt time.Time `json:"expires_at"`
}

type Timer struct {
	Meta
	ExecutionID string     `json:"execution_id"`
	NodeID      string     `json:"node_id"`
	DueAt       time.Time  `json:"due_at"`
	Kind        string     `json:"kind"` // wait | retry | approval_timeout | event_timeout
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
	SourceID string `json:"source_id"`
	Cursor   string `json:"cursor"`
}

type RawRecord struct {
	Meta
	SourceID   string    `json:"source_id"`
	Transport  string    `json:"transport,omitempty"`
	ReceivedAt time.Time `json:"received_at"`
	Body       string    `json:"body,omitempty"`     // inline body (64 KB or less)
	BodyRef    string    `json:"body_ref,omitempty"` // MinIO reference for larger bodies
	Size       int64     `json:"size,omitempty"`
	SHA256     string    `json:"sha256,omitempty"`
	ParseError string    `json:"parse_error,omitempty"`
}

type Seen struct {
	Meta
	Key       string    `json:"key"`
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
