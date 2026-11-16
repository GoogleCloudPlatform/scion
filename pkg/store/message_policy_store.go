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
	"encoding/json"
	"slices"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
)

// MessagePolicyMode enumerates enforcement modes on a project message policy
// (consumer design §5.3). New policies default to MessagePolicyModeShadow.
const (
	MessagePolicyModeShadow  = "shadow"
	MessagePolicyModeEnforce = "enforce"
)

// MessagePolicyOutcome enumerates configured fallback outcomes for uncertain or
// failed evaluations (consumer design §§4.4, 5.3).
const (
	MessagePolicyOutcomeAdmit  = "admit"
	MessagePolicyOutcomeReject = "reject"
)

// MessagePolicyClass enumerates evaluation classes recorded on a
// MessagePolicyDecision (consumer design §§5.5, 5.8).
const (
	MessagePolicyClassPass      = "pass"
	MessagePolicyClassViolation = "violation"
	MessagePolicyClassUncertain = "uncertain"
	MessagePolicyClassFailed    = "failed"
)

// MessagePolicyDecisionAction enumerates actions recorded on a
// MessagePolicyDecision (consumer design §5.8).
const (
	MessagePolicyActionAdmitted = "admitted"
	MessagePolicyActionRejected = "rejected"
	MessagePolicyActionShadow   = "shadow"
)

// MessagePolicySurface constants identify outbound agent message hook surfaces
// (consumer design §§5.2, 5.3). Only agent_to_user and agent_to_conversation
// have hooks in M1 (H2); the remaining surfaces are reserved for M3 (H1/H3).
const (
	MessagePolicySurfaceAgentToUser         = "agent_to_user"
	MessagePolicySurfaceAgentToConversation = "agent_to_conversation"
	MessagePolicySurfaceAgentDM             = "agent_dm"
	MessagePolicySurfaceGroupPost           = "group_post"
	MessagePolicySurfaceGroupSet            = "group_set"
	MessagePolicySurfaceMentionFanout       = "mention_fanout"
	MessagePolicySurfaceMentionDelivery     = "mention_delivery"
)

// Operational caps and retention defaults for message policies and decisions
// (consumer design §§5.3, 5.8).
const (
	MaxMessagePoliciesPerProject               = 50
	MaxActiveMatchingMessagePolicies           = 10
	MaxMessagePolicyNameLen                    = 63
	MaxMessagePolicyViolationRuleBytes         = 4096
	MaxMessagePolicySystemOneRuleBytes         = 16384
	MaxMessagePolicyTemplateBytes              = 1024
	DefaultMessagePolicyDecisionMaxAge         = 7 * 24 * time.Hour
	DefaultMaxMessagePolicyDecisionsPerProject = 100_000
)

// IsValidMessagePolicyName reports whether name is a canonical slug suitable
// for a project message policy (1..63 lowercase ASCII letters/digits separated
// by single hyphens, e.g. "no-prose-choices").
func IsValidMessagePolicyName(name string) bool {
	return name != "" && len(name) <= MaxMessagePolicyNameLen && api.Slugify(name) == name
}

// IsValidMessagePolicyMode reports whether mode is a valid policy enforcement
// mode ("shadow" or "enforce").
func IsValidMessagePolicyMode(mode string) bool {
	return mode == MessagePolicyModeShadow || mode == MessagePolicyModeEnforce
}

// IsValidMessagePolicyOutcome reports whether outcome is a valid fallback
// outcome ("admit" or "reject").
func IsValidMessagePolicyOutcome(outcome string) bool {
	return outcome == MessagePolicyOutcomeAdmit || outcome == MessagePolicyOutcomeReject
}

// IsValidMessagePolicyClass reports whether class is a valid evaluation class.
func IsValidMessagePolicyClass(class string) bool {
	switch class {
	case MessagePolicyClassPass,
		MessagePolicyClassViolation,
		MessagePolicyClassUncertain,
		MessagePolicyClassFailed:
		return true
	default:
		return false
	}
}

// IsValidMessagePolicyDecisionAction reports whether action is a valid
// recorded decision action ("admitted", "rejected", or "shadow").
func IsValidMessagePolicyDecisionAction(action string) bool {
	switch action {
	case MessagePolicyActionAdmitted,
		MessagePolicyActionRejected,
		MessagePolicyActionShadow:
		return true
	default:
		return false
	}
}

// MessagePolicySenderSelector restricts which agent senders a policy governs.
// When AllAgents is true or AgentIDs is empty, all agents in the project match.
type MessagePolicySenderSelector struct {
	AgentIDs  []string `json:"agent_ids,omitempty"`
	AllAgents bool     `json:"all_agents,omitempty"`
}

// Clone returns a deep copy of s.
func (s MessagePolicySenderSelector) Clone() MessagePolicySenderSelector {
	var ids []string
	if len(s.AgentIDs) > 0 {
		ids = slices.Clone(s.AgentIDs)
	}
	return MessagePolicySenderSelector{
		AgentIDs:  ids,
		AllAgents: s.AllAgents,
	}
}

// MessagePolicyRecipientSelector restricts which recipients a policy governs.
// Kinds may include "agent", "user", or "conversation" (M1 authoring refuses
// "agent" and non-empty AgentIDs because agent deliveries are M3 territory).
type MessagePolicyRecipientSelector struct {
	Kinds    []string `json:"kinds,omitempty"`
	AgentIDs []string `json:"agent_ids,omitempty"`
}

// Clone returns a deep copy of r.
func (r MessagePolicyRecipientSelector) Clone() MessagePolicyRecipientSelector {
	var kinds, ids []string
	if len(r.Kinds) > 0 {
		kinds = slices.Clone(r.Kinds)
	}
	if len(r.AgentIDs) > 0 {
		ids = slices.Clone(r.AgentIDs)
	}
	return MessagePolicyRecipientSelector{
		Kinds:    kinds,
		AgentIDs: ids,
	}
}

// MessagePolicyScope defines the surfaces and sender/recipient selectors
// governed by a MessagePolicy (consumer design §5.3). Contains no time.Time
// fields so it serializes cleanly in Ent JSON columns across SQLite and
// PostgreSQL.
type MessagePolicyScope struct {
	Surfaces  []string                       `json:"surfaces"`
	Sender    MessagePolicySenderSelector    `json:"sender,omitempty"`
	Recipient MessagePolicyRecipientSelector `json:"recipient,omitempty"`
}

// Clone returns a deep copy of sc.
func (sc MessagePolicyScope) Clone() MessagePolicyScope {
	var surfaces []string
	if len(sc.Surfaces) > 0 {
		surfaces = slices.Clone(sc.Surfaces)
	}
	return MessagePolicyScope{
		Surfaces:  surfaces,
		Sender:    sc.Sender.Clone(),
		Recipient: sc.Recipient.Clone(),
	}
}

// Matches reports whether sc covers a send on surface from senderAgentID to a
// recipient of recipientKind (and recipientAgentID when recipientKind == "agent").
func (sc MessagePolicyScope) Matches(surface, senderAgentID, recipientKind, recipientAgentID string) bool {
	if !slices.Contains(sc.Surfaces, surface) {
		return false
	}
	if !sc.Sender.AllAgents && len(sc.Sender.AgentIDs) > 0 {
		if !slices.Contains(sc.Sender.AgentIDs, senderAgentID) {
			return false
		}
	}
	if len(sc.Recipient.Kinds) > 0 {
		if !slices.Contains(sc.Recipient.Kinds, recipientKind) {
			return false
		}
	}
	if len(sc.Recipient.AgentIDs) > 0 {
		if recipientKind != "agent" || !slices.Contains(sc.Recipient.AgentIDs, recipientAgentID) {
			return false
		}
	}
	return true
}

// MessagePolicy is a project-scoped message admission policy record
// (consumer design §5.3).
type MessagePolicy struct {
	ID                string             `json:"id"`
	ProjectID         string             `json:"project_id"`
	Name              string             `json:"name"`
	Description       string             `json:"description,omitempty"`
	Enabled           bool               `json:"enabled"`
	Mode              string             `json:"mode"`
	Scope             MessagePolicyScope `json:"scope"`
	ViolationRule     string             `json:"violation_rule"`
	SystemOneRule     string             `json:"system_one_rule,omitempty"`
	SystemOneRefs     []string           `json:"system_one_refs,omitempty"`
	OnUncertain       string             `json:"on_uncertain,omitempty"`
	OnFailed          string             `json:"on_failed"`
	OnFailedReasons   map[string]string  `json:"on_failed_reasons,omitempty"`
	RejectReason      string             `json:"reject_reason,omitempty"`
	RejectRemediation string             `json:"reject_remediation,omitempty"`
	Revision          int64              `json:"revision"`
	CreatedBy         string             `json:"created_by,omitempty"`
	UpdatedBy         string             `json:"updated_by,omitempty"`
	CreatedAt         time.Time          `json:"created_at"`
	UpdatedAt         time.Time          `json:"updated_at"`
}

// Clone returns a deep copy of p.
func (p *MessagePolicy) Clone() *MessagePolicy {
	if p == nil {
		return nil
	}
	cp := *p
	cp.Scope = p.Scope.Clone()
	if len(p.SystemOneRefs) > 0 {
		cp.SystemOneRefs = slices.Clone(p.SystemOneRefs)
	} else {
		cp.SystemOneRefs = nil
	}
	if len(p.OnFailedReasons) > 0 {
		cp.OnFailedReasons = make(map[string]string, len(p.OnFailedReasons))
		for k, v := range p.OnFailedReasons {
			cp.OnFailedReasons[k] = v
		}
	} else {
		cp.OnFailedReasons = nil
	}
	return &cp
}

// Matches reports whether p is enabled and its scope matches the given send.
func (p *MessagePolicy) Matches(surface, senderAgentID, recipientKind, recipientAgentID string) bool {
	if p == nil || !p.Enabled {
		return false
	}
	return p.Scope.Matches(surface, senderAgentID, recipientKind, recipientAgentID)
}

// MessagePolicyDecision is a content-free audit/telemetry record of one policy
// evaluation against a message send (consumer design §5.8). It intentionally
// carries NO message body or attachment content — only MessageID (empty when
// rejected before persistence).
type MessagePolicyDecision struct {
	ID             string          `json:"id"`
	ProjectID      string          `json:"project_id"`
	PolicyID       string          `json:"policy_id"`
	PolicyName     string          `json:"policy_name,omitempty"`
	PolicyRevision int64           `json:"policy_revision"`
	Mode           string          `json:"mode"`
	Surface        string          `json:"surface"`
	SenderKind     string          `json:"sender_kind,omitempty"`
	SenderID       string          `json:"sender_id,omitempty"`
	RecipientKind  string          `json:"recipient_kind,omitempty"`
	RecipientID    string          `json:"recipient_id,omitempty"`
	MessageID      string          `json:"message_id,omitempty"`
	Class          string          `json:"class"`
	Action         string          `json:"action"`
	Reason         string          `json:"reason,omitempty"`
	CELErrorReason string          `json:"cel_error_reason,omitempty"`
	Evaluation     json.RawMessage `json:"evaluation,omitempty"`
	ElapsedMs      int64           `json:"elapsed_ms"`
	CreatedAt      time.Time       `json:"created_at"`
}

// MessagePolicyFilter filters ListMessagePolicies queries.
type MessagePolicyFilter struct {
	ProjectID string
	Enabled   *bool
	Mode      string
}

// MessagePolicyDecisionFilter filters ListMessagePolicyDecisions queries.
type MessagePolicyDecisionFilter struct {
	ProjectID string
	PolicyID  string
	Mode      string
	Surface   string
	Class     string
	Action    string
}

// MessagePolicyDecisionTrim reports the result of one bounded
// TrimMessagePolicyDecisions pass.
type MessagePolicyDecisionTrim struct {
	// AgedDeleted is the number of rows deleted because CreatedAt < now - maxAge.
	AgedDeleted int
	// OverflowDeleted is the number of oldest rows deleted to enforce
	// maxRowsPerProject after the age pass.
	OverflowDeleted int
	// More reports whether eligible rows remain beyond this pass's candidate cap.
	More bool
}

// TotalDeleted returns AgedDeleted + OverflowDeleted.
func (t MessagePolicyDecisionTrim) TotalDeleted() int {
	return t.AgedDeleted + t.OverflowDeleted
}

// MessagePolicyStore defines persistence operations for project-scoped message
// policies and their content-free decision records (ptone/scion#8047).
type MessagePolicyStore interface {
	// CreateMessagePolicy creates a new message policy at Revision=1.
	// Mode defaults to "shadow" when empty. Enforces MaxMessagePoliciesPerProject
	// (50) per project, returning ErrQuotaExceeded when at cap, ErrAlreadyExists
	// on (ProjectID, Name) or ID collision, and ErrInvalidInput for invalid
	// fields. Populates ID (if empty), Mode, Revision, CreatedAt, and UpdatedAt.
	CreateMessagePolicy(ctx context.Context, policy *MessagePolicy) error

	// GetMessagePolicy retrieves a message policy by UUID.
	// Returns ErrNotFound if not found or id is not a valid UUID.
	GetMessagePolicy(ctx context.Context, id string) (*MessagePolicy, error)

	// GetMessagePolicyByName retrieves a message policy by (projectID, name).
	// Returns ErrNotFound if not found or projectID is not a valid UUID.
	GetMessagePolicyByName(ctx context.Context, projectID, name string) (*MessagePolicy, error)

	// ListMessagePolicies lists message policies ordered by (name ASC, id ASC).
	ListMessagePolicies(ctx context.Context, filter MessagePolicyFilter, opts ListOptions) (*ListResult[MessagePolicy], error)

	// UpdateMessagePolicy updates an existing message policy (resolved by ID when
	// non-empty, otherwise by (ProjectID, Name)) and increments Revision by 1 on
	// every edit (consumer design §5.3). When policy.Revision > 0, it must match
	// the stored Revision or ErrRevisionConflict is returned.
	UpdateMessagePolicy(ctx context.Context, policy *MessagePolicy) error

	// DeleteMessagePolicy deletes a message policy by UUID.
	// Returns ErrNotFound if no row with that ID exists.
	DeleteMessagePolicy(ctx context.Context, id string) error

	// CreateMessagePolicyDecision records one content-free policy evaluation
	// decision. MessageID may be empty (stored as NULL) when the send was
	// rejected before persistence. Populates ID (if empty) and CreatedAt (if
	// zero).
	CreateMessagePolicyDecision(ctx context.Context, decision *MessagePolicyDecision) error

	// ListMessagePolicyDecisions lists decision records ordered newest first
	// (created_at DESC, id DESC), with keyset pagination via opts.Cursor.
	ListMessagePolicyDecisions(ctx context.Context, filter MessagePolicyDecisionFilter, opts ListOptions) (*ListResult[MessagePolicyDecision], error)

	// TrimMessagePolicyDecisions trims decision records older than maxAge
	// (when maxAge > 0) and then trims each project's oldest remaining rows
	// above maxRowsPerProject (when maxRowsPerProject > 0), in bounded batches
	// safe on both SQLite and PostgreSQL.
	TrimMessagePolicyDecisions(ctx context.Context, now time.Time, maxAge time.Duration, maxRowsPerProject int) (MessagePolicyDecisionTrim, error)
}
