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

// Trigger and webhook endpoint resource caps (Stage 1 §4.1, §4.2, F5; Stage 3 §4.3).
const (
	MaxWebhookEndpointsPerProject = 20
	MaxActiveTriggersPerSource    = 50
	MaxTriggersPerProject         = 200
	MaxTriggerEventListsBytes     = 8 * 1024
)

// WebhookEndpoint signature modes and statuses (§4.1, §8.1a).
const (
	WebhookSignatureModeScionV1  = "scion_v1"
	WebhookSignatureModeBodyOnly = "body_only"

	WebhookEndpointStatusActive   = "active"
	WebhookEndpointStatusDisabled = "disabled"
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

	TriggerSubjectModeNew  = "new"
	TriggerSubjectModeSkip = "skip"

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

// TriggerFire statuses (§4.4, §5.5).
const (
	TriggerFireStatusFiring  = "firing"
	TriggerFireStatusFired   = "fired"
	TriggerFireStatusFailed  = "failed"
	TriggerFireStatusRefused = "refused"
	TriggerFireStatusSkipped = "skipped"
)

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

// WebhookEndpoint represents a project-owned receiving webhook address (§4.1).
// Its signing secret is stored out of band as a hub-scoped SecretTypeInternal
// row in secrets, never on this struct.
type WebhookEndpoint struct {
	ID                  string            `json:"id"`
	ProjectID           string            `json:"projectId"`
	Name                string            `json:"name"`
	Description         string            `json:"description,omitempty"`
	OwnerPrincipalKind  string            `json:"ownerPrincipalKind"`
	OwnerPrincipalID    string            `json:"ownerPrincipalId"`
	OwnerCredentialKind string            `json:"ownerCredentialKind"`
	SecretVersion       int               `json:"secretVersion"`
	SignatureMode       string            `json:"signatureMode"`
	Status              string            `json:"status"`
	CreatedBy           string            `json:"createdBy,omitempty"`
	LastReceivedAt      *time.Time        `json:"lastReceivedAt,omitempty"`
	ReceiveCount        int               `json:"receiveCount"`
	RejectCount         int               `json:"rejectCount"`
	CreatedAt           time.Time         `json:"createdAt"`
	UpdatedAt           time.Time         `json:"updatedAt"`
	Fixture             FixtureProvenance `json:"-"`
}

// WebhookEndpointFilter selects webhook endpoints in ListWebhookEndpoints.
type WebhookEndpointFilter struct {
	ProjectID          string
	Name               string
	Status             string
	OwnerPrincipalKind string
	OwnerPrincipalID   string
}

// TriggerMatchRule is one ordered element of a trigger's match_rules list (§4.2, mixed-rule-arrays §2.1).
type TriggerMatchRule struct {
	Dialect string `json:"dialect"`
	Expr    string `json:"expr"`
}

// Trigger represents a project-scoped event trigger definition (§4.2).
type Trigger struct {
	ID             string             `json:"id"`
	ProjectID      string             `json:"projectId"`
	Name           string             `json:"name"`
	Description    string             `json:"description,omitempty"`
	SourceKind     string             `json:"sourceKind"`
	SourceID       string             `json:"sourceId"`
	MatchTypes     []string           `json:"matchTypes"`
	MatchDialect   string             `json:"matchDialect,omitempty"`
	MatchRule      string             `json:"matchRule,omitempty"`
	MatchRules     []TriggerMatchRule `json:"matchRules"`
	ActionType     string             `json:"actionType"`
	ActionPayload  string             `json:"actionPayload"`
	SubjectDialect string             `json:"subjectDialect"`
	SubjectRule    string             `json:"subjectRule,omitempty"`
	SubjectMode    string             `json:"subjectMode"`
	Status         string             `json:"status"`
	StatusReason   string             `json:"statusReason,omitempty"`
	LastFireAt     *time.Time         `json:"lastFireAt,omitempty"`
	LastFireStatus string             `json:"lastFireStatus,omitempty"`
	LastFireError  string             `json:"lastFireError,omitempty"`
	FireCount      int                `json:"fireCount"`
	ErrorCount     int                `json:"errorCount"`
	CreatedBy      string             `json:"createdBy,omitempty"`
	CreatedAt      time.Time          `json:"createdAt"`
	UpdatedAt      time.Time          `json:"updatedAt"`

	InitiatorAttribution `json:"-"`
	AuthorityCeiling     EffectCeiling     `json:"-"`
	Fixture              FixtureProvenance `json:"-"`
	// MatchRulesCorrupt is true when the stored match_rules column contains
	// unparseable JSON. Callers must fail closed rather than treating an empty
	// MatchRules slice as a Types-only trigger.
	MatchRulesCorrupt bool `json:"-"`
}

// TriggerFilter selects triggers in ListTriggers and CountTriggers.
type TriggerFilter struct {
	ProjectID          string
	SourceKind         string
	SourceID           string
	Status             string
	Name               string
	InitiatorAgentID   string
	AuthorityPrincipal ScheduleAuthorityPrincipal
	SystemPausedOnly   bool
}

// TriggerFieldMask names the mutable columns an UpdateTrigger call intends to
// write. Status writes both status and status_reason together, matching
// ScheduleFieldMask.
type TriggerFieldMask struct {
	Name           bool
	Description    bool
	SourceKind     bool
	SourceID       bool
	MatchTypes     bool
	MatchDialect   bool
	MatchRule      bool
	MatchRules     bool
	ActionType     bool
	ActionPayload  bool
	SubjectDialect bool
	SubjectRule    bool
	SubjectMode    bool
	Status         bool
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
	Status         string     `json:"status"`
	Error          string     `json:"error,omitempty"`
	ResultRef      string     `json:"resultRef,omitempty"`
	MatchedThrough string     `json:"matchedThrough,omitempty"`
	DecidedDialect string     `json:"decidedDialect,omitempty"`
	RuleTrace      string     `json:"ruleTrace,omitempty"`
	ExecutorKind   string     `json:"executorKind,omitempty"`
	ExecutorID     string     `json:"executorId,omitempty"`
	LeaseUntil     time.Time  `json:"leaseUntil"`
	StartedAt      time.Time  `json:"startedAt"`
	FinishedAt     *time.Time `json:"finishedAt,omitempty"`
	CreatedAt      time.Time  `json:"createdAt"`

	InitiatorAttribution `json:"-"`
}

// TriggerFireFilter selects trigger fire rows in ListTriggerFires.
type TriggerFireFilter struct {
	ProjectID string
	TriggerID string
	EventID   string
	Status    string
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
	FlushWebhookEndpointCounters(ctx context.Context, id string, receiveDelta, rejectDelta int, lastReceivedAt *time.Time) error
	// DeleteWebhookEndpoint pauses dependent triggers in projectID with
	// status_reason = "source_deleted", deletes the endpoint's hub-scoped
	// internal secret rows, and deletes the endpoint row in one transaction.
	DeleteWebhookEndpoint(ctx context.Context, projectID, id string) (pausedTriggerIDs []string, deletedSecretKeys []string, err error)

	// Triggers
	CreateTrigger(ctx context.Context, tr *Trigger) error
	GetTrigger(ctx context.Context, id string) (*Trigger, error)
	GetTriggerByName(ctx context.Context, projectID, name string) (*Trigger, error)
	ListTriggers(ctx context.Context, filter TriggerFilter, opts ListOptions) (*ListResult[Trigger], error)
	CountTriggers(ctx context.Context, filter TriggerFilter) (int, error)
	UpdateTrigger(ctx context.Context, tr *Trigger, fields TriggerFieldMask, prevRevision int, prevRevisionKnown bool, attribution *InitiatorAttribution) error
	UpdateTriggerStatus(ctx context.Context, id, status, reason string) error
	RecordTriggerFire(ctx context.Context, id string, firedAt time.Time, status, errMsg string) error
	DeleteTrigger(ctx context.Context, id string) error
	DeleteTriggersByProject(ctx context.Context, projectID string) (int, error)

	// Trigger events
	CreateTriggerEvent(ctx context.Context, evt *TriggerEvent) error
	GetTriggerEvent(ctx context.Context, id string) (*TriggerEvent, error)
	GetTriggerEventByDedupKey(ctx context.Context, sourceKind, sourceID, dedupKey string) (*TriggerEvent, error)
	GetTriggerEventByBodySHA256(ctx context.Context, projectID, sourceKind, bodySHA256 string) (*TriggerEvent, error)
	ListTriggerEvents(ctx context.Context, filter TriggerEventFilter, opts ListOptions) (*ListResult[TriggerEvent], error)
	CountPendingTriggerEvents(ctx context.Context, projectID string) (int, error)
	MarkTriggerEventProcessed(ctx context.Context, id string, processedAt time.Time, matchCount, fireCount int) error
	MarkTriggerEventFailed(ctx context.Context, id string, processedAt time.Time, errMsg string) error
	ReconcileOrphanedPendingTriggerEvents(ctx context.Context, queueKind string, olderThan, now time.Time, errMsg string, limit int) (int, error)

	// Trigger fires
	ClaimTriggerFire(ctx context.Context, fire *TriggerFire) (claimed bool, existing *TriggerFire, err error)
	FinishTriggerFire(ctx context.Context, id, status, resultRef, errMsg string, finishedAt time.Time) (bool, error)
	GetTriggerFire(ctx context.Context, id string) (*TriggerFire, error)
	ListTriggerFires(ctx context.Context, filter TriggerFireFilter, opts ListOptions) (*ListResult[TriggerFire], error)
	HasLiveTriggerFireForSubject(ctx context.Context, triggerID, subject string, now time.Time) (bool, error)
	AgentHasFiredTriggerForEvent(ctx context.Context, eventID, agentID string) (bool, error)
	ReapExpiredTriggerFires(ctx context.Context, now time.Time, limit int) (int, error)
	PurgeOldTriggerHistory(ctx context.Context, olderThan time.Time) (eventsDeleted int, firesDeleted int, err error)
}
