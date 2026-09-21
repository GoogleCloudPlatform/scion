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

package hub

import (
	"log/slog"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// ---------------------------------------------------------------------------
// Body-free DM audit (#1690)
// ---------------------------------------------------------------------------
//
// DMAuditEntry is the single admission audit record for agent-to-agent DM
// delivery. It captures canonical principal, target, and conversation IDs,
// the authorization decision (allow/deny), policy revisions, and surface
// metadata — but NEVER message body or attachment content.
//
// One entry is emitted per ExecuteAgentDM call at admission time. Dispatch
// outcomes are recorded separately via LogDMDispatchOutcome so that "allowed"
// is not conflated with "delivered".

// DMAuditEntry records a body-free DM admission decision for audit.
// No message body, attachment content, or membership lists are included.
type DMAuditEntry struct {
	// Timestamp is when the admission decision was made.
	Timestamp time.Time

	// Action is the admission outcome: "allow" or "deny".
	Action string

	// SenderID is the canonical agent ID of the sender.
	SenderID string
	// SenderProjectID is the server-derived project of the sender agent.
	SenderProjectID string

	// RecipientID is the canonical agent ID of the target.
	RecipientID string
	// RecipientProjectID is the server-derived project of the target agent.
	RecipientProjectID string

	// ConversationID is the conversation context, if any.
	ConversationID string

	// Surface identifies the delivery surface (e.g. "agent_dm").
	Surface string

	// CrossProject is true when sender and recipient are in different projects.
	CrossProject bool

	// DecisionCode is the machine-readable denial code. Empty on allow.
	DecisionCode string
	// DecisionReason is the human-readable reason string.
	DecisionReason string

	// HubPolicyRevision is the hub-level policy revision at decision time.
	HubPolicyRevision int64
	// ProjectPolicyRevision is the project-level policy revision at decision time.
	ProjectPolicyRevision int64

	// CorrelationID links related audit entries (e.g. message ID once known).
	CorrelationID string
}

// LogDMAdmission emits a body-free structured log entry for a DM admission
// decision. Both allow and deny outcomes are recorded. No message body or
// attachment content is included in the log.
//
// This is called once per ExecuteAgentDM invocation, before any side effects
// for denials, and after persistence for allows (with the message ID as
// correlation).
func LogDMAdmission(entry DMAuditEntry) {
	attrs := []any{
		"action", entry.Action,
		"sender_id", entry.SenderID,
		"sender_project_id", entry.SenderProjectID,
		"recipient_id", entry.RecipientID,
		"recipient_project_id", entry.RecipientProjectID,
		"cross_project", entry.CrossProject,
		"surface", entry.Surface,
	}

	if entry.ConversationID != "" {
		attrs = append(attrs, "conversation_id", entry.ConversationID)
	}
	if entry.DecisionCode != "" {
		attrs = append(attrs, "decision_code", entry.DecisionCode)
	}
	if entry.DecisionReason != "" {
		attrs = append(attrs, "decision_reason", entry.DecisionReason)
	}
	if entry.HubPolicyRevision > 0 {
		attrs = append(attrs, "hub_policy_revision", entry.HubPolicyRevision)
	}
	if entry.ProjectPolicyRevision > 0 {
		attrs = append(attrs, "project_policy_revision", entry.ProjectPolicyRevision)
	}
	if entry.CorrelationID != "" {
		attrs = append(attrs, "correlation_id", entry.CorrelationID)
	}

	if entry.Action == "deny" {
		slog.Warn("dm admission denied", attrs...)
	} else {
		slog.Info("dm admission allowed", attrs...)
	}
}

// DMAuditEntryFromInput constructs a DMAuditEntry from the shared DM
// operation input and authorization decision. The entry is body-free:
// only canonical IDs, provenance, and decision metadata are captured.
func DMAuditEntryFromInput(input *AgentDMInput, decision *MessageDecision) DMAuditEntry {
	entry := DMAuditEntry{
		Timestamp:          time.Now(),
		SenderID:           input.SenderAgent.ID,
		SenderProjectID:    input.SenderAgent.ProjectID,
		RecipientID:        input.TargetAgent.ID,
		RecipientProjectID: input.TargetAgent.ProjectID,
		ConversationID:     input.ConversationID,
		Surface:            "agent_dm",
		CrossProject:       input.SenderAgent.ProjectID != input.TargetAgent.ProjectID,
	}

	if decision != nil {
		if decision.Allowed {
			entry.Action = "allow"
		} else {
			entry.Action = "deny"
			entry.DecisionCode = string(decision.Code)
			entry.DecisionReason = decision.Reason
		}
		entry.HubPolicyRevision = decision.HubPolicyRevision
		entry.ProjectPolicyRevision = decision.ProjectPolicyRevision
	}

	return entry
}

// DMAuditEntryForDenial constructs a deny DMAuditEntry from the shared DM
// operation input and a denial reason/code. Used for non-authorization denials
// (rate limit, validation, attachment rejection) that don't have a
// MessageDecision.
func DMAuditEntryForDenial(input *AgentDMInput, denialCode, reason string) DMAuditEntry {
	return DMAuditEntry{
		Timestamp:          time.Now(),
		Action:             "deny",
		SenderID:           input.SenderAgent.ID,
		SenderProjectID:    input.SenderAgent.ProjectID,
		RecipientID:        input.TargetAgent.ID,
		RecipientProjectID: input.TargetAgent.ProjectID,
		ConversationID:     input.ConversationID,
		Surface:            "agent_dm",
		CrossProject:       input.SenderAgent.ProjectID != input.TargetAgent.ProjectID,
		DecisionCode:       denialCode,
		DecisionReason:     reason,
	}
}

// DispatchOutcome enumerates the possible dispatch outcomes for audit.
type DispatchOutcome string

const (
	// DispatchSucceeded means the message was dispatched and acknowledged.
	DispatchSucceeded DispatchOutcome = "succeeded"
	// DispatchFailed means the dispatch attempt failed after persistence.
	DispatchFailed DispatchOutcome = "failed"
	// DispatchSkipped means no dispatcher was available.
	DispatchSkipped DispatchOutcome = "skipped"
)

// LogDMDispatchOutcome emits a separate body-free log entry for the dispatch
// result of a DM that was already admitted (persisted). This keeps admission
// (allow/deny) distinct from delivery success/failure (AC-3).
func LogDMDispatchOutcome(
	senderAgent, targetAgent *store.Agent,
	messageID string,
	outcome DispatchOutcome,
	dispatchErr error,
) {
	attrs := []any{
		"message_id", messageID,
		"sender_id", senderAgent.ID,
		"recipient_id", targetAgent.ID,
		"dispatch_outcome", string(outcome),
	}

	if dispatchErr != nil {
		attrs = append(attrs, "dispatch_error", dispatchErr.Error())
	}

	switch outcome {
	case DispatchFailed:
		slog.Warn("dm dispatch outcome", attrs...)
	default:
		slog.Info("dm dispatch outcome", attrs...)
	}
}
