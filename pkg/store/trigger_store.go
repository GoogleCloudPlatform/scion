// Copyright 2026 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package store

import (
	"context"
	"fmt"
	"time"
)

// Trigger and webhook endpoint resource caps (Stage 1 §4.1, §4.2, F5; Stage 2 §4.3; Stage 3 §4.3).
const (
	MaxWebhookEndpointsPerProject    = 20
	MaxBoundWebhookEndpointsPerAgent = 5
	MaxActiveTriggersPerSource       = 50
	MaxTriggersPerProject            = 200
	MaxTriggerEventListsBytes        = 8 * 1024
	MaxTriggerActions                = 8
	MaxTriggerActionsBytes           = 64 * 1024
)

// WebhookEndpoint auth modes, signature modes, statuses, bound lifetimes,
// rotation grace window, and bounds (Stage 1 §4.1, §8.1a; Stage 2 §4.1, §4.4, §7.1, §7.3, §7.4).
const (
	WebhookAuthModeHMAC     = "hmac"
	WebhookAuthModeHubToken = "hub_token"

	WebhookSignatureModeScionV1  = "scion_v1"
	WebhookSignatureModeGitHubV1 = "github_v1"
	WebhookSignatureModeStripeV1 = "stripe_v1"
	WebhookSignatureModeBodyOnly = "body_only"

	WebhookEndpointStatusActive   = "active"
	WebhookEndpointStatusDisabled = "disabled"

	WebhookBoundLifetimeNone       = ""
	WebhookBoundLifetimeAgent      = "agent"
	WebhookBoundLifetimePersistent = "persistent"

	WebhookSecretRotationGraceWindow = 24 * time.Hour

	// DefaultWebhookMaxBodyBytes is the default per-endpoint body limit (64 KiB).
	DefaultWebhookMaxBodyBytes = 64 * 1024
	// DefaultWebhookReplayWindowSeconds is the default timestamp tolerance (300 s).
	DefaultWebhookReplayWindowSeconds = 300
	// MinWebhookReplayWindowSeconds and MaxWebhookReplayWindowSeconds bound replay_window_seconds (§7.3).
	MinWebhookReplayWindowSeconds = 30
	MaxWebhookReplayWindowSeconds = 900

	// Trigger fire and dispatch limits (§7.4).
	MaxTriggerFiresPerMinute              = 600
	DefaultTriggerMaxConcurrentDispatches = 5
	MaxTriggerConcurrentDispatches        = 50
	MaxLiveTriggerAgentsPerProject        = 25
)

// Trigger source kinds, rule dialects, action types, subject modes, statuses,
// and system pause reasons (§4.2, §5.6).
const (
	TriggerSourceKindWebhook = "webhook"
	TriggerSourceKindGitHub  = "github"

	TriggerDialectCEL = "cel"

	TriggerActionMessage       = "message"
	TriggerActionDispatchAgent = "dispatch_agent"
	TriggerActionExec          = "exec"
	TriggerActionGitHubComment = "github.comment"
	// TriggerActionNotify marks a hub-authored failure-notification fire row
	// recorded in trigger_fires (Stage 2 §10). Notify rows never re-notify and
	// are excluded from per-trigger fire-rate limits.
	TriggerActionNotify = "notify"

	// TriggerFireActionIndexNotify is the reserved action_index stored on
	// action_type = "notify" trigger_fires rows for the agent-message sink.
	// TriggerFireActionIndexNotifyUser is the reserved action_index stored on
	// action_type = "notify" trigger_fires rows for the user-notice sink.
	// Real actions always use non-negative indices (0..MaxTriggerActions-1), so
	// negative indices can never collide with real actions on the unique
	// (event_id, trigger_id, action_index) key.
	TriggerFireActionIndexNotify     = -1
	TriggerFireActionIndexNotifyUser = -2

	TriggerSubjectModeNew   = "new"
	TriggerSubjectModeSkip  = "skip"
	TriggerSubjectModeReuse = "reuse"

	// MaxTriggerCoalesceWindowSeconds is the maximum allowed subject
	// coalesce_window duration in seconds (10 minutes, Stage 3 §6.3).
	MaxTriggerCoalesceWindowSeconds = 600

	TriggerStatusActive = "active"
	TriggerStatusPaused = "paused"

	TriggerStatusReasonNone           = ""
	TriggerStatusReasonCreatorDeleted = "creator_deleted"
	TriggerStatusReasonTargetDeleted  = "target_deleted"
	TriggerStatusReasonSourceDeleted  = "source_deleted"
)

// TriggerEvent statuses (§4.3, §6.3).
const (
	TriggerEventStatusPending   = "pending"
	TriggerEventStatusProcessed = "processed"
	TriggerEventStatusFailed    = "failed"
)

// TriggerFire statuses (§4.4, §5.5; Stage 2 §6.2; Stage 3 §6.2, §6.3).
const (
	TriggerFireStatusCoalescing = "coalescing"
	TriggerFireStatusDeferred   = "deferred"
	TriggerFireStatusFiring     = "firing"
	TriggerFireStatusWaiting    = "waiting"
	TriggerFireStatusFired      = "fired"
	TriggerFireStatusFailed     = "failed"
	TriggerFireStatusRefused    = "refused"
	TriggerFireStatusSkipped    = "skipped"
	TriggerFireStatusExpired    = "expired"
)

// TriggerFire rate-limit decision markers (action-arrays §3.5).
// Persisted once per (trigger_id, event_id) inside LockTriggerQuota so all
// sibling actions across dispatcher passes share the exact same rate decision
// without inferring from outcome status or error text.
const (
	TriggerFireRateDecisionNone     = ""
	TriggerFireRateDecisionAdmitted = "admitted"
	TriggerFireRateDecisionLimited  = "limited"
)

// TriggerSubjectClaim states (Stage 2 §5.6).
const (
	TriggerSubjectClaimStateActive   = "active"
	TriggerSubjectClaimStateReleased = "released"
)

// TriggerSubjectClaim release reasons (Stage 3 §10.3).
const (
	TriggerSubjectClaimReleaseReasonAssociationLost = "association_lost"
)

// MaxTriggerSubjectBytes is the maximum byte length of a trigger subject claim key (§5.6).
const MaxTriggerSubjectBytes = 256

// Message action offline queue TTL bounds in seconds (Stage 2 §6.1).
const (
	DefaultMessageQueueTTLSeconds = 86_400
	MinMessageQueueTTLSeconds     = 60
	MaxMessageQueueTTLSeconds     = 172_800
)

// TriggerSubjectAbandonedClaimTTL is the age after which an unbound active
// claim (agent_id IS NULL) is considered abandoned and eligible for takeover
// or maintenance release (Stage 2 §5.6, §18 S16).
const TriggerSubjectAbandonedClaimTTL = 5 * time.Minute

// WebhookEndpointSecretKeyPrefix returns the hub-scoped internal secret key
// prefix for an endpoint's versioned HMAC secrets (§4.1, R1).
func WebhookEndpointSecretKeyPrefix(projectID, endpointID string) string {
	return fmt.Sprintf("webhook_endpoint.%s.%s.v", projectID, endpointID)
}

// WebhookEndpointSecretKey returns the hub-scoped internal secret key for a
// specific version of an endpoint's HMAC secret (§4.1, R1).
func WebhookEndpointSecretKey(projectID, endpointID string, version int) string {
	return fmt.Sprintf("webhook_endpoint.%s.%s.v%d", projectID, endpointID, version)
}

// WebhookEndpointProjectSecretKeyPrefix returns the hub-scoped internal secret
// key prefix covering every webhook endpoint in projectID.
func WebhookEndpointProjectSecretKeyPrefix(projectID string) string {
	return fmt.Sprintf("webhook_endpoint.%s.", projectID)
}

// WebhookRoutingHints configures optional RFC 6901 JSON pointers (<= 128 bytes
// each) that extract event type, subject, and dedup key from a verified webhook
// JSON body (Stage 2 §7.2, §18 S20).
type WebhookRoutingHints struct {
	TypePath    string `json:"typePath,omitempty"`
	SubjectPath string `json:"subjectPath,omitempty"`
	DedupPath   string `json:"dedupPath,omitempty"`
}

// IsEmpty reports whether all routing hint paths are unset.
func (h *WebhookRoutingHints) IsEmpty() bool {
	return h == nil || (h.TypePath == "" && h.SubjectPath == "" && h.DedupPath == "")
}

// WebhookEndpoint represents a project-owned receiving webhook address (§4.1).
// Its signing secret is stored out of band as a hub-scoped SecretTypeInternal
// row in secrets, never on this struct.
type WebhookEndpoint struct {
	ID                  string               `json:"id"`
	ProjectID           string               `json:"projectId"`
	Name                string               `json:"name"`
	Description         string               `json:"description,omitempty"`
	OwnerPrincipalKind  string               `json:"ownerPrincipalKind"`
	OwnerPrincipalID    string               `json:"ownerPrincipalId"`
	OwnerCredentialKind string               `json:"ownerCredentialKind"`
	AuthMode            string               `json:"authMode"`
	AllowProjectAgents  bool                 `json:"allowProjectAgents"`
	MaxBodyBytes        int                  `json:"maxBodyBytes"`
	RatePerSecond       int                  `json:"ratePerSecond"`
	RateBurst           int                  `json:"rateBurst"`
	ReplayWindowSeconds int                  `json:"replayWindowSeconds"`
	SecretVersion       int                  `json:"secretVersion"`
	SecretRotatedAt     *time.Time           `json:"secretRotatedAt,omitempty"`
	ConfigVersion       int                  `json:"configVersion"`
	SignatureMode       string               `json:"signatureMode"`
	RoutingHints        *WebhookRoutingHints `json:"routingHints,omitempty"`
	Status              string               `json:"status"`
	BoundAgentID        string               `json:"boundAgentId,omitempty"`
	BoundLifetime       string               `json:"boundLifetime,omitempty"`
	CreatedBy           string               `json:"createdBy,omitempty"`
	LastReceivedAt      *time.Time           `json:"lastReceivedAt,omitempty"`
	ReceiveCount        int                  `json:"receiveCount"`
	RejectCount         int                  `json:"rejectCount"`
	CreatedAt           time.Time            `json:"createdAt"`
	UpdatedAt           time.Time            `json:"updatedAt"`
	Fixture             FixtureProvenance    `json:"-"`
}

// WebhookEndpointFilter selects webhook endpoints in ListWebhookEndpoints.
type WebhookEndpointFilter struct {
	ProjectID          string
	Name               string
	Status             string
	OwnerPrincipalKind string
	OwnerPrincipalID   string
	BoundAgentID       string
	// VisibleToAgentID matches endpoints either owned by this agent OR bound
	// to this agent (Stage 2 §4.1). Mutually exclusive with OwnerPrincipal* filters.
	VisibleToAgentID string
}

// WebhookEndpointFieldMask names the mutable columns an UpdateWebhookEndpoint
// call intends to write (Stage 2 §4.4, §7.1, §7.3, §7.4).
type WebhookEndpointFieldMask struct {
	Description bool
	Status      bool
	// Binding writes both bound_agent_id and bound_lifetime together.
	Binding             bool
	AuthMode            bool
	AllowProjectAgents  bool
	SignatureMode       bool
	RoutingHints        bool
	MaxBodyBytes        bool
	RatePerSecond       bool
	RateBurst           bool
	ReplayWindowSeconds bool
}

// TriggerMatchRule is one ordered element of a trigger's match_rules list (§4.2, mixed-rule-arrays §2.1).
type TriggerMatchRule struct {
	Dialect string `json:"dialect"`
	Expr    string `json:"expr"`
}

// TriggerAction is one ordered action element of a trigger's actions list
// (action-arrays-design §1). Payload holds the canonical JSON object string
// for the action type.
type TriggerAction struct {
	Type    string `json:"type"`
	Payload string `json:"payload"`
}

// TriggerNotifyPolicy configures failure notifications on a trigger definition
// (Stage 2 §10, §12). Stored as JSON text in triggers.notify.
type TriggerNotifyPolicy struct {
	OnOutcomes []string `json:"onOutcomes,omitempty"`
	UserNotice *bool    `json:"userNotice,omitempty"`
	AgentID    string   `json:"agentId,omitempty"`
	AgentName  string   `json:"agentName,omitempty"`
}

// IsEmpty reports whether all notify policy fields are unset.
func (p *TriggerNotifyPolicy) IsEmpty() bool {
	return p == nil || (len(p.OnOutcomes) == 0 && p.UserNotice == nil && p.AgentID == "" && p.AgentName == "")
}

// Trigger represents a project-scoped event trigger definition (§4.2).
type Trigger struct {
	ID                      string               `json:"id"`
	ProjectID               string               `json:"projectId"`
	Name                    string               `json:"name"`
	Description             string               `json:"description,omitempty"`
	SourceKind              string               `json:"sourceKind"`
	SourceID                string               `json:"sourceId"`
	MatchTypes              []string             `json:"matchTypes"`
	MatchDialect            string               `json:"matchDialect,omitempty"`
	MatchRule               string               `json:"matchRule,omitempty"`
	MatchRules              []TriggerMatchRule   `json:"matchRules"`
	MatchForm               string               `json:"matchForm,omitempty"`
	ActionType              string               `json:"actionType"`
	ActionPayload           string               `json:"actionPayload"`
	Actions                 []TriggerAction      `json:"actions"`
	SubjectDialect          string               `json:"subjectDialect"`
	SubjectRule             string               `json:"subjectRule,omitempty"`
	SubjectMode             string               `json:"subjectMode"`
	CoalesceWindowSeconds   int                  `json:"coalesceWindowSeconds,omitempty"`
	MaxFiresPerMinute       int                  `json:"maxFiresPerMinute"`
	MaxConcurrentDispatches int                  `json:"maxConcurrentDispatches"`
	Notify                  *TriggerNotifyPolicy `json:"notify,omitempty"`
	Status                  string               `json:"status"`
	StatusReason            string               `json:"statusReason,omitempty"`
	LastFireAt              *time.Time           `json:"lastFireAt,omitempty"`
	LastFireStatus          string               `json:"lastFireStatus,omitempty"`
	LastFireError           string               `json:"lastFireError,omitempty"`
	FireCount               int                  `json:"fireCount"`
	ErrorCount              int                  `json:"errorCount"`
	CreatedBy               string               `json:"createdBy,omitempty"`
	CreatedAt               time.Time            `json:"createdAt"`
	UpdatedAt               time.Time            `json:"updatedAt"`

	InitiatorAttribution `json:"-"`
	AuthorityCeiling     EffectCeiling     `json:"-"`
	Fixture              FixtureProvenance `json:"-"`
	// MatchRulesCorrupt is true when the stored match_rules column contains
	// unparseable JSON. Callers must fail closed rather than treating an empty
	// MatchRules slice as a Types-only trigger.
	MatchRulesCorrupt bool `json:"-"`
	// ActionsCorrupt is true when the stored actions column contains
	// unparseable or invalid JSON. Callers must fail closed rather than
	// silently dropping actions or falling back to legacy scalar columns.
	ActionsCorrupt bool `json:"-"`
}

// TriggerFilter selects triggers in ListTriggers and CountTriggers.
type TriggerFilter struct {
	ProjectID          string
	SourceKind         string
	SourceID           string
	SourceIDs          []string
	Status             string
	Name               string
	ActionTypes        []string
	TargetAgentID      string
	InitiatorAgentID   string
	AuthorityPrincipal ScheduleAuthorityPrincipal
	SystemPausedOnly   bool
}

// TriggerFieldMask names the mutable columns an UpdateTrigger call intends to
// write. Status writes both status and status_reason together, matching
// ScheduleFieldMask.
type TriggerFieldMask struct {
	Name                    bool
	Description             bool
	SourceKind              bool
	SourceID                bool
	MatchTypes              bool
	MatchDialect            bool
	MatchRule               bool
	MatchRules              bool
	MatchForm               bool
	ActionType              bool
	ActionPayload           bool
	Actions                 bool
	SubjectDialect          bool
	SubjectRule             bool
	SubjectMode             bool
	CoalesceWindowSeconds   bool
	MaxFiresPerMinute       bool
	MaxConcurrentDispatches bool
	Notify                  bool
	Status                  bool
}

// TriggerEvent represents a durable inbox event record (§4.3).
type TriggerEvent struct {
	ID           string              `json:"id"`
	ProjectID    string              `json:"projectId"`
	SourceKind   string              `json:"sourceKind"`
	SourceID     string              `json:"sourceId"`
	Type         string              `json:"type"`
	Subject      string              `json:"subject"`
	DedupKey     string              `json:"dedupKey"`
	BodySHA256   string              `json:"bodySha256,omitempty"`
	OccurredAt   time.Time           `json:"occurredAt"`
	ReceivedAt   time.Time           `json:"receivedAt"`
	Attributes   map[string]string   `json:"attributes"`
	Lists        map[string][]string `json:"lists,omitempty"`
	Headers      map[string]string   `json:"headers"`
	Payload      string              `json:"payload,omitempty"`
	PayloadRef   string              `json:"payloadRef,omitempty"`
	PayloadBytes int                 `json:"payloadBytes"`
	Status       string              `json:"status"`
	MatchCount   int                 `json:"matchCount"`
	FireCount    int                 `json:"fireCount"`
	Error        string              `json:"error,omitempty"`
	ProcessedAt  *time.Time          `json:"processedAt,omitempty"`
	CreatedAt    time.Time           `json:"createdAt"`
}

// TriggerEventFilter selects trigger events in ListTriggerEvents.
type TriggerEventFilter struct {
	ProjectID        string
	SourceKind       string
	SourceID         string
	Status           string
	Since            *time.Time
	VisibleToAgentID string
}

// TriggerFire represents one per-(event, trigger) claim and history row (§4.4).
type TriggerFire struct {
	ID             string     `json:"id"`
	ProjectID      string     `json:"projectId"`
	TriggerID      string     `json:"triggerId"`
	EventID        string     `json:"eventId"`
	Subject        string     `json:"subject"`
	ActionType     string     `json:"actionType"`
	ActionIndex    int        `json:"actionIndex"`
	Status         string     `json:"status"`
	RateDecision   string     `json:"-"`
	CounterFlushed bool       `json:"-"`
	Error          string     `json:"error,omitempty"`
	ResultRef      string     `json:"resultRef,omitempty"`
	TargetAgentID  string     `json:"targetAgentId,omitempty"`
	WaitUntil      *time.Time `json:"waitUntil,omitempty"`
	Attempts       int        `json:"attempts,omitempty"`
	MatchedThrough string     `json:"matchedThrough,omitempty"`
	DecidedDialect string     `json:"decidedDialect,omitempty"`
	RuleTrace      string     `json:"ruleTrace,omitempty"`
	ExecutorKind   string     `json:"executorKind,omitempty"`
	ExecutorID     string     `json:"executorId,omitempty"`
	LeaseUntil     time.Time  `json:"leaseUntil"`
	NotBefore      *time.Time `json:"notBefore,omitempty"`
	DeferredAt     *time.Time `json:"deferredAt,omitempty"`
	StartedAt      time.Time  `json:"startedAt"`
	FinishedAt     *time.Time `json:"finishedAt,omitempty"`
	CreatedAt      time.Time  `json:"createdAt"`

	InitiatorAttribution `json:"-"`
}

// TriggerFireFilter selects trigger fire rows in ListTriggerFires.
type TriggerFireFilter struct {
	ProjectID        string
	TriggerID        string
	EventID          string
	Status           string
	Since            *time.Time
	VisibleToAgentID string
}

// TriggerSubjectClaim represents the per-(trigger, action_index, subject)
// live-subject claim row used by dispatch_agent triggers in mode "skip"
// (Stage 2 §5.6) and Stage 3 "reuse", and exec triggers in mode "skip" (P8).
type TriggerSubjectClaim struct {
	ID            string     `json:"id"`
	TriggerID     string     `json:"triggerId"`
	ActionIndex   int        `json:"actionIndex"`
	Subject       string     `json:"subject"`
	State         string     `json:"state"`
	AgentID       string     `json:"agentId,omitempty"`
	EventID       string     `json:"eventId"`
	FireID        string     `json:"fireId"`
	ClaimedAt     time.Time  `json:"claimedAt"`
	BoundAt       *time.Time `json:"boundAt,omitempty"`
	ReleasedAt    *time.Time `json:"releasedAt,omitempty"`
	ReleaseReason string     `json:"releaseReason,omitempty"`

	// Agent is populated in-memory by SubjectClaims.Live from the GetAgent
	// lookup it already performs, so callers can inspect phase/launch state
	// without a second store lookup or type assertion. Never persisted.
	Agent *Agent `json:"-"`
}

// TriggerStore manages webhook endpoints, triggers, trigger events, and
// trigger fires (ptone/scion#7961).
type TriggerStore interface {
	// LockTriggerQuota acquires the transaction-scoped per-project trigger and
	// webhook-endpoint quota lock (LockTriggerQuota) on PostgreSQL. Must be
	// called inside WithTx before reading endpoint or trigger counts for cap
	// enforcement; no-op on SQLite where write transactions already serialize.
	LockTriggerQuota(ctx context.Context, projectID string) error

	// Webhook endpoints
	CreateWebhookEndpoint(ctx context.Context, ep *WebhookEndpoint) error
	GetWebhookEndpoint(ctx context.Context, id string) (*WebhookEndpoint, error)
	GetWebhookEndpointByName(ctx context.Context, projectID, name string) (*WebhookEndpoint, error)
	ListWebhookEndpoints(ctx context.Context, filter WebhookEndpointFilter, opts ListOptions) (*ListResult[WebhookEndpoint], error)
	CountWebhookEndpoints(ctx context.Context, projectID string) (int, error)
	CountBoundWebhookEndpoints(ctx context.Context, projectID, agentID string) (int, error)
	UpdateWebhookEndpoint(ctx context.Context, ep *WebhookEndpoint, fields WebhookEndpointFieldMask) error
	RotateWebhookEndpointSecret(ctx context.Context, projectID, id string, nextVersion int, rotatedAt time.Time) (*WebhookEndpoint, error)
	FlushWebhookEndpointCounters(ctx context.Context, id string, receiveDelta, rejectDelta int, lastReceivedAt *time.Time) error
	// DeleteWebhookEndpoint pauses dependent triggers in projectID with
	// status_reason = "source_deleted", deletes the endpoint's hub-scoped
	// internal secret rows, and deletes the endpoint row in one transaction.
	DeleteWebhookEndpoint(ctx context.Context, projectID, id string) (pausedTriggerIDs []string, deletedSecretKeys []string, err error)
	// DeleteWebhookEndpointForBoundAgent atomically deletes an agent-lifetime
	// endpoint bound to boundAgentID, its internal secret rows, and any triggers
	// on it authored by boundAgentID; triggers on it authored by others are
	// paused with status_reason = "source_deleted" (Stage 2 §4.2). Returns
	// ErrNotFound if the endpoint is absent, no longer bound with lifetime
	// "agent" to boundAgentID, or if a live (non-deleted) agent with
	// id = boundAgentID still exists.
	DeleteWebhookEndpointForBoundAgent(ctx context.Context, projectID, id, boundAgentID string) (deletedTriggerIDs []string, pausedTriggerIDs []string, deletedSecretKeys []string, err error)
	// UnbindWebhookEndpointForAgent atomically clears bound_agent_id and
	// bound_lifetime on a persistent endpoint currently bound to boundAgentID
	// when no live (non-deleted) agent with id = boundAgentID exists (Stage 2 §4.2).
	// Returns false when the endpoint is absent, no longer bound, or the agent is live.
	UnbindWebhookEndpointForAgent(ctx context.Context, projectID, id, boundAgentID string) (unbound bool, err error)
	// PurgeExpiredRotatedWebhookSecrets deletes hub-scoped internal secret rows
	// for webhook endpoints whose secret version is retired and past graceWindow
	// (or superseded by a newer rotation, Stage 2 §4.4).
	PurgeExpiredRotatedWebhookSecrets(ctx context.Context, now time.Time, graceWindow time.Duration) (int, error)
	// ListOrphanedBoundWebhookEndpoints returns up to limit webhook endpoints
	// with bound_lifetime in ("agent", "persistent") whose bound_agent_id
	// references a soft-deleted or missing agent row (hourly maintenance cascade backstop).
	ListOrphanedBoundWebhookEndpoints(ctx context.Context, limit int) ([]WebhookEndpoint, error)

	// Triggers
	CreateTrigger(ctx context.Context, tr *Trigger) error
	GetTrigger(ctx context.Context, id string) (*Trigger, error)
	GetTriggerByName(ctx context.Context, projectID, name string) (*Trigger, error)
	ListTriggers(ctx context.Context, filter TriggerFilter, opts ListOptions) (*ListResult[Trigger], error)
	CountTriggers(ctx context.Context, filter TriggerFilter) (int, error)
	ProjectsWithTriggersBySourceKind(ctx context.Context, projectIDs []string, sourceKind string) (map[string]bool, error)
	UpdateTrigger(ctx context.Context, tr *Trigger, fields TriggerFieldMask, prevRevision int, prevRevisionKnown bool, attribution *InitiatorAttribution) error
	UpdateTriggerStatus(ctx context.Context, id, status, reason string) error
	RecordTriggerFire(ctx context.Context, id string, firedAt time.Time, status, errMsg string) error
	// FlushTriggerEventCounters atomically updates a trigger's fire_count,
	// error_count, and last_fire_* once per matched (trigger_id, event_id)
	// using the durable counter_flushed marker on trigger_fires rows, and
	// flushes a single error_count delta when a later pass records the event's
	// first error outcome (action-arrays §3.5).
	FlushTriggerEventCounters(ctx context.Context, triggerID, eventID string, firedAt time.Time, status, errMsg string) error
	DeleteTrigger(ctx context.Context, id string) error
	DeleteTriggersByProject(ctx context.Context, projectID string) (int, error)

	// Trigger events
	CreateTriggerEvent(ctx context.Context, evt *TriggerEvent) error
	GetTriggerEvent(ctx context.Context, id string) (*TriggerEvent, error)
	GetTriggerEventByDedupKey(ctx context.Context, sourceKind, sourceID, dedupKey string) (*TriggerEvent, error)
	GetTriggerEventByBodySHA256(ctx context.Context, projectID, sourceKind, bodySHA256 string) (*TriggerEvent, error)
	ListTriggerEvents(ctx context.Context, filter TriggerEventFilter, opts ListOptions) (*ListResult[TriggerEvent], error)
	CountPendingTriggerEvents(ctx context.Context, projectID string) (int, error)
	// SumTriggerEventOverflowBytes returns the total payload_bytes of trigger_events
	// in projectID with non-empty payload_ref created at or after since (Stage 2 §8).
	SumTriggerEventOverflowBytes(ctx context.Context, projectID string, since time.Time) (int64, error)
	// ExistingTriggerEventPayloadRefs returns the subset of refs belonging to
	// projectID that currently exist in trigger_events.payload_ref (uses the
	// (project_id, payload_ref) index during the hourly orphan sweep, Stage 2 §8).
	ExistingTriggerEventPayloadRefs(ctx context.Context, projectID string, refs []string) (map[string]bool, error)
	// UpdateTriggerEventAttributes is enrichment-only (Stage 3 §5.5): it updates
	// attributes and lists on a pending event row before rule evaluation and
	// never modifies payload, type, subject, headers, dedup_key, or body_sha256.
	// It is a no-op once the event is no longer pending.
	UpdateTriggerEventAttributes(ctx context.Context, id string, attributes map[string]string, lists map[string][]string) error
	MarkTriggerEventProcessed(ctx context.Context, id string, processedAt time.Time, matchCount, fireCount int) error
	MarkTriggerEventFailed(ctx context.Context, id string, processedAt time.Time, errMsg string) error
	ReconcileOrphanedPendingTriggerEvents(ctx context.Context, queueKind string, olderThan, now time.Time, errMsg string, limit int) (int, error)

	// Trigger fires
	ClaimTriggerFire(ctx context.Context, fire *TriggerFire) (claimed bool, existing *TriggerFire, err error)
	ClaimTriggerFireSubjectSkip(ctx context.Context, fire *TriggerFire, skipError string, now time.Time) (claimed bool, skippedBusy bool, existing *TriggerFire, err error)
	ClaimTriggerFireCoalescing(ctx context.Context, fire *TriggerFire, coalesceError string, now, notBefore time.Time) (claimed bool, promoted bool, superseded []TriggerFire, existing *TriggerFire, err error)
	DeferTriggerFire(ctx context.Context, id string, deferredAt, notBefore, leaseUntil time.Time) (bool, error)
	FinishPendingTriggerFire(ctx context.Context, id, status, errMsg, matchedThrough, decidedDialect, ruleTrace string, finishedAt time.Time) (bool, error)
	FinishTriggerFire(ctx context.Context, id, status, resultRef, targetAgentID, errMsg string, finishedAt time.Time) (bool, error)
	ParkTriggerFireWaiting(ctx context.Context, id, projectID, targetAgentID, errMsg string, waitUntil time.Time, maxPerTarget, maxPerProject int) (parked bool, capExceeded bool, err error)
	ClaimWaitingTriggerFire(ctx context.Context, id string, attempts int, leaseUntil time.Time) (bool, error)
	ReturnFiringTriggerFireToWaiting(ctx context.Context, id string, errMsg string) (bool, error)
	FinishWaitingTriggerFire(ctx context.Context, id, status, resultRef, targetAgentID, errMsg string, finishedAt time.Time, attempts int) (bool, error)
	ListWaitingFiresForTarget(ctx context.Context, targetAgentID string, limit int) ([]string, error)
	GetTriggerFire(ctx context.Context, id string) (*TriggerFire, error)
	ListTriggerFires(ctx context.Context, filter TriggerFireFilter, opts ListOptions) (*ListResult[TriggerFire], error)
	CountTriggerFiresSince(ctx context.Context, triggerID string, since time.Time, excludeStatuses ...string) (int, error)
	// CountTriggerActionFiresSince counts trigger_fires rows for triggerID with
	// action_type == actionType (when non-empty), event_id != excludeEventID
	// (when non-empty), created >= since, and status not in excludeStatuses.
	CountTriggerActionFiresSince(ctx context.Context, triggerID, actionType, excludeEventID string, since time.Time, excludeStatuses ...string) (int, error)
	// CountRecentTriggerFires counts distinct matched events in trigger_fires
	// for triggerID with created > since and status <> 'skipped', excluding
	// the caller's own event (derived from excludeFireID when non-empty) so
	// sibling actions of the same matched event never count against each other
	// and an in-flight firing row does not count against its own
	// max_fires_per_minute budget (Stage 2 §7.4, action-arrays §3.5).
	// When any sibling row for (trigger_id, event_id) already has an explicit
	// rate_decision marker ("admitted" or "limited"), returns 0 or
	// MaxTriggerFiresPerMinute respectively without inspecting status or error text.
	// A decision is visible to siblings and later passes only once the caller
	// persists it with RecordTriggerFireRateDecision in the same LockTriggerQuota
	// transaction.
	CountRecentTriggerFires(ctx context.Context, triggerID string, since time.Time, excludeFireID string) (int, error)
	// RecordTriggerFireRateDecision persists the per-(trigger, event) rate-limit
	// decision ("admitted" or "limited") on fireID if not already set. Must be
	// called inside the same LockTriggerQuota transaction as CountRecentTriggerFires.
	RecordTriggerFireRateDecision(ctx context.Context, fireID, decision string) error
	HasLiveTriggerFireForSubject(ctx context.Context, triggerID string, actionIndex int, subject string, now time.Time) (bool, error)
	HasLiveTriggerFireForSubjectExcluding(ctx context.Context, triggerID string, actionIndex int, subject, excludeFireID string, includeFiring bool, now time.Time) (bool, error)
	HasPendingTriggerFiresForEvent(ctx context.Context, eventID string) (bool, error)
	AgentHasFiredTriggerForEvent(ctx context.Context, eventID, agentID string) (bool, error)
	EventHasTriggerFireForResultRef(ctx context.Context, eventID, resultRef string) (bool, error)
	ReapExpiredTriggerFires(ctx context.Context, now time.Time, limit int) (int, error)
	PurgeOldTriggerHistory(ctx context.Context, olderThan time.Time) (eventsDeleted int, firesDeleted int, err error)
	// PurgeOldTriggerHistoryWithPayloadRefs purges terminal trigger_fires and
	// trigger_events older than olderThan and returns the non-empty payload_ref
	// values of the purged event rows so the caller can delete their overflow objects.
	PurgeOldTriggerHistoryWithPayloadRefs(ctx context.Context, olderThan time.Time) (eventsDeleted int, firesDeleted int, payloadRefs []string, err error)

	// Trigger subject claims (Stage 2 §5.6, action-arrays §1)
	ClaimTriggerSubject(ctx context.Context, triggerID string, actionIndex int, subject, eventID, fireID string, now, abandonedBefore time.Time) (claimed *TriggerSubjectClaim, existing *TriggerSubjectClaim, err error)
	BindTriggerSubjectClaim(ctx context.Context, claimID, fireID, agentID string, boundAt time.Time) (bool, error)
	ReleaseTriggerSubjectClaim(ctx context.Context, claimID, fireID string, releasedAt time.Time) (bool, error)
	GetActiveTriggerSubjectClaim(ctx context.Context, triggerID string, actionIndex int, subject string) (*TriggerSubjectClaim, error)
	ReleaseTriggerSubjectClaimsForAgent(ctx context.Context, agentID string, releasedAt time.Time) (int, error)
	ReleaseTriggerSubjectClaimsForProjectBySourceKind(ctx context.Context, projectID, sourceKind, reason string, releasedAt time.Time) (int, error)
	ReleaseTerminalTriggerSubjectClaims(ctx context.Context, now time.Time) (int, error)
	PurgeOldTriggerSubjectClaims(ctx context.Context, olderThan time.Time) (int, error)
}
