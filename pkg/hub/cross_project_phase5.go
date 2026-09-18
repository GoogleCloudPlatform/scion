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
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// ---------------------------------------------------------------------------
// Phase 5 D1: Multi-target DM fan-out with cross-project support
// ---------------------------------------------------------------------------

// MaxFanOutTargets is the maximum number of targets in a single fan-out DM.
// This prevents abuse and limits server-side resource consumption.
const MaxFanOutTargets = 50

// QualifiedAgentRef represents a project-qualified agent reference in the
// form @<project-slug>/<agent-slug>. When ProjectSlug is empty, the agent
// is resolved within the sender's project (same-project reference).
type QualifiedAgentRef struct {
	ProjectSlug string
	AgentSlug   string
}

// ParseQualifiedAgentRef parses a qualified agent reference string.
// Accepted forms:
//   - "@project/agent" or "project/agent" → cross-project reference
//   - "@agent" or "agent" → same-project reference (ProjectSlug is "")
//
// Returns an error for clearly malformed refs (empty input, empty project
// or agent slug after splitting).
func ParseQualifiedAgentRef(s string) (QualifiedAgentRef, error) {
	s = strings.TrimPrefix(s, "@")
	if s == "" {
		return QualifiedAgentRef{}, fmt.Errorf("empty agent reference")
	}
	if idx := strings.Index(s, "/"); idx >= 0 {
		project := s[:idx]
		agent := s[idx+1:]
		if project == "" {
			return QualifiedAgentRef{}, fmt.Errorf("empty project slug in qualified ref %q", "@"+s)
		}
		if agent == "" {
			return QualifiedAgentRef{}, fmt.Errorf("empty agent slug in qualified ref %q", "@"+s)
		}
		return QualifiedAgentRef{
			ProjectSlug: project,
			AgentSlug:   agent,
		}, nil
	}
	return QualifiedAgentRef{AgentSlug: s}, nil
}

// FanOutTarget represents a resolved target in a multi-target DM fan-out.
type FanOutTarget struct {
	Agent      *store.Agent
	ProjectID  string
	AgentSlug  string
	Decision   MessageDecision
	Delivered  bool
	MessageID  string
	DenialCode MessageDenialCode
}

// FanOutResult summarizes the outcome of a multi-target DM fan-out.
type FanOutResult struct {
	Targets   []FanOutTarget
	Delivered int
	Denied    int
	Failed    int
}

// ResolveFanOutTargets resolves a list of qualified agent refs to store.Agent
// records, deduplicating by canonical agent ID.
func (s *Server) ResolveFanOutTargets(
	ctx context.Context,
	refs []QualifiedAgentRef,
	senderProjectID string,
) ([]FanOutTarget, error) {
	if len(refs) > MaxFanOutTargets {
		return nil, fmt.Errorf("fan-out target count %d exceeds maximum %d", len(refs), MaxFanOutTargets)
	}

	seen := make(map[string]bool, len(refs))
	var targets []FanOutTarget

	for _, ref := range refs {
		projectID := senderProjectID
		if ref.ProjectSlug != "" {
			// Cross-project reference: resolve project by slug.
			project, err := s.store.GetProjectBySlug(ctx, ref.ProjectSlug)
			if err != nil || project == nil {
				slog.Warn("ResolveFanOutTargets: project lookup failed",
					"project_slug", ref.ProjectSlug, "error", err)
				targets = append(targets, FanOutTarget{
					AgentSlug:  ref.AgentSlug,
					ProjectID:  "",
					DenialCode: MessageDenialCrossProjectUnsupported,
					Decision: MessageDecision{
						Reason: fmt.Sprintf("project %q not found", ref.ProjectSlug),
					},
				})
				continue
			}
			projectID = project.ID
		}

		agent, err := s.store.GetAgentBySlug(ctx, projectID, ref.AgentSlug)
		if err != nil {
			slog.Warn("ResolveFanOutTargets: agent lookup failed",
				"project_id", projectID, "agent_slug", ref.AgentSlug, "error", err)
			targets = append(targets, FanOutTarget{
				AgentSlug:  ref.AgentSlug,
				ProjectID:  projectID,
				DenialCode: MessageDenialCrossProjectUnsupported,
				Decision: MessageDecision{
					Reason: fmt.Sprintf("agent %q not found", ref.AgentSlug),
				},
			})
			continue
		}

		// Deduplicate by canonical agent ID.
		if seen[agent.ID] {
			continue
		}
		seen[agent.ID] = true

		targets = append(targets, FanOutTarget{
			Agent:     agent,
			ProjectID: projectID,
			AgentSlug: ref.AgentSlug,
		})
	}

	return targets, nil
}

// EvaluateFanOutTargets evaluates messaging authorization for each target
// independently. Returns per-recipient decisions without short-circuiting:
// authorized targets get deliveries even when others are denied.
func (s *Server) EvaluateFanOutTargets(
	ctx context.Context,
	senderIdentity Identity,
	targets []FanOutTarget,
) FanOutResult {
	result := FanOutResult{
		Targets: make([]FanOutTarget, len(targets)),
	}
	copy(result.Targets, targets)

	for i := range result.Targets {
		t := &result.Targets[i]
		if t.Agent == nil {
			// Already failed during resolution.
			result.Failed++
			continue
		}

		// Evaluate per-recipient policy using the shared evaluator.
		allowed, reason, decision := s.authorizeAgentMessage(ctx, senderIdentity, t.Agent, false)
		if decision != nil {
			t.Decision = *decision
		} else {
			t.Decision = MessageDecision{
				Allowed: allowed,
				Reason:  reason,
			}
		}

		if !allowed {
			t.DenialCode = t.Decision.Code
			if t.DenialCode == "" {
				t.DenialCode = MessageDenialCrossProjectUnsupported
			}
			result.Denied++
		} else {
			t.Delivered = true
			result.Delivered++
		}
	}

	return result
}

// ---------------------------------------------------------------------------
// Phase 5 D2: Group/broadcast/plugin boundary enforcement
// ---------------------------------------------------------------------------

// ValidateCrossProjectGroupBoundary rejects cross-project operations on
// group conversations, broadcasts, and plugin channels BEFORE any effects.
// Returns a MessageDecision with denial code cross_project_groups_unsupported
// when the operation would cross project boundaries.
//
// Per design Section 11: "Project-owned group conversations, native topics,
// project broadcasts, and plugin channels retain their current project
// boundaries."
func ValidateCrossProjectGroupBoundary(
	senderProjectID string,
	targetProjectID string,
	operationType string,
) *MessageDecision {
	if senderProjectID == targetProjectID {
		return nil // Same project — no boundary violation.
	}
	return &MessageDecision{
		Code:   MessageDenialCrossProjectGroupsUnsupported,
		Reason: fmt.Sprintf("cross-project %s is not supported; group/broadcast/plugin channels retain project boundaries", operationType),
	}
}

// ValidateCrossProjectRoomJoin rejects attempts to join a foreign project's
// room/group/broadcast. This is checked before any admission effects.
func ValidateCrossProjectRoomJoin(
	agentProjectID string,
	roomProjectID string,
) *MessageDecision {
	if agentProjectID == roomProjectID {
		return nil
	}
	return &MessageDecision{
		Code:   MessageDenialCrossProjectGroupsUnsupported,
		Reason: "joining a room in a different project is not supported",
	}
}

// ---------------------------------------------------------------------------
// Phase 5 D3: Scheduled message cross-project support
// ---------------------------------------------------------------------------

// ScheduledMessageCrossProjectContext stores the cross-project authority and
// caveats captured at schedule time. This is persisted with the scheduled event
// so that fire-time reauthorization uses the original context.
//
// TODO: wire into scheduled event creation/fire paths once the persistence
// layer supports storing cross-project context alongside scheduled events.
type ScheduledMessageCrossProjectContext struct {
	// SenderProjectID is the project where the scheduled message was authored.
	SenderProjectID string `json:"senderProjectId"`
	// TargetProjectID is the target agent's project at schedule time.
	TargetProjectID string `json:"targetProjectId,omitempty"`
	// TargetAgentID is the immutable ID of the target agent.
	TargetAgentID string `json:"targetAgentId"`
	// OriginUserID is the root human principal from the sender's ancestry.
	OriginUserID string `json:"originUserId,omitempty"`
	// AuthoredAt is when the scheduled message was created.
	AuthoredAt time.Time `json:"authoredAt"`
	// CrossProject is true when target and sender are in different projects.
	CrossProject bool `json:"crossProject"`
	// SenderMode is the sender agent's message mode at authoring time.
	SenderMode string `json:"senderMode,omitempty"`
}

// authorizeScheduledMessageCrossProject extends fire-time authorization for
// cross-project scheduled messages. It re-evaluates all gates that may have
// changed since authoring:
//   - Hub cross_project_messaging_enabled
//   - Sender and target agent modes
//   - Target project inbound policy
//   - Origin user membership and status
//   - Target agent existence and deletion status
//
// Returns nil on success, or an error with a typed denial code on failure.
func (s *Server) authorizeScheduledMessageCrossProject(
	ctx context.Context,
	evt store.ScheduledEvent,
	agent *store.Agent,
	creatorIdentity Identity,
) error {
	if agent == nil {
		return fmt.Errorf("%s: target agent is nil", MessageDenialScheduledTargetDeleted)
	}

	// Check if the target has been soft-deleted since scheduling.
	if !agent.DeletedAt.IsZero() {
		return fmt.Errorf("%s: target agent %q has been deleted since scheduling",
			MessageDenialScheduledTargetDeleted, agent.ID)
	}

	// For cross-project scheduled messages, the event project is the sender's
	// project. The target agent may be in a different project.
	if agent.ProjectID == evt.ProjectID {
		// Same project — use the standard authorization path.
		return nil
	}

	// Cross-project: re-evaluate all gates at fire time.
	agentIdent, ok := creatorIdentity.(AgentIdentity)
	if !ok {
		// Creator is a user — use full cross-project evaluation via
		// the user-to-agent path which is already handled by the caller.
		return nil
	}

	decision := s.EvaluateAgentMessage(ctx, agentIdent, agent)
	if !decision.Allowed {
		code := decision.Code
		if code == "" {
			code = MessageDenialCrossProjectScheduledDenied
		}
		return fmt.Errorf("%s: %s", code, decision.Reason)
	}

	return nil
}

// ---------------------------------------------------------------------------
// Phase 5 D4: Cross-project attachment transfer authorization
// ---------------------------------------------------------------------------

// AttachmentTransferDecision captures whether a cross-project attachment
// transfer is authorized.
type AttachmentTransferDecision struct {
	Allowed        bool
	Code           MessageDenialCode
	Reason         string
	AttachmentID   string
	ConversationID string
}

// AuthorizeAttachmentDownload checks whether an agent is authorized to
// download an attachment. Authorization requires:
//  1. The attachment is associated with a message in a conversation.
//  2. The requesting agent is a participant in that conversation.
//  3. For cross-project attachments, the cross-project messaging feature is enabled.
//
// Path links are non-transferring — they provide no download authorization.
func (s *Server) AuthorizeAttachmentDownload(
	ctx context.Context,
	requesterIdentity Identity,
	attachmentID string,
	conversationID string,
) AttachmentTransferDecision {
	if attachmentID == "" {
		return AttachmentTransferDecision{
			Code:   MessageDenialAttachmentNotFound,
			Reason: "attachment ID is required",
		}
	}

	if conversationID == "" {
		return AttachmentTransferDecision{
			Code:   MessageDenialAttachmentUnauthorized,
			Reason: "conversation ID is required for attachment authorization",
		}
	}

	// Fail closed if requester identity is missing from context.
	if requesterIdentity == nil {
		return AttachmentTransferDecision{
			Code:   MessageDenialAttachmentUnauthorized,
			Reason: "requester identity is missing; cannot authorize attachment download",
		}
	}

	// Verify the requester is a conversation participant using participant rows.
	participants, err := s.store.ListParticipants(ctx, conversationID)
	if err != nil {
		return AttachmentTransferDecision{
			Code:   MessageDenialAttachmentUnauthorized,
			Reason: "failed to list conversation participants",
		}
	}

	requesterID := requesterIdentity.ID()
	isParticipant := false
	for _, p := range participants {
		if p.PrincipalID == requesterID {
			isParticipant = true
			break
		}
	}

	if !isParticipant {
		return AttachmentTransferDecision{
			Code:         MessageDenialAttachmentUnauthorized,
			Reason:       "requester is not a participant in the attachment's conversation",
			AttachmentID: attachmentID,
		}
	}

	// For cross-project conversations, verify the Hub feature is enabled.
	// Reuse the already-fetched participants to avoid a duplicate ListParticipants query.
	if isCrossProjectConversationFromParticipants(ctx, s, participants) {
		ops := s.GetOperationalSettings()
		if ops == nil || !ops.CrossProjectMessagingEnabled() {
			return AttachmentTransferDecision{
				Code:   MessageDenialCrossProjectAttachUnsupported,
				Reason: "cross-project messaging is disabled; attachment download denied",
			}
		}
	}

	return AttachmentTransferDecision{
		Allowed:        true,
		AttachmentID:   attachmentID,
		ConversationID: conversationID,
	}
}

// isCrossProjectConversation checks whether a conversation contains
// participants from different projects by examining stored messages.
func isCrossProjectConversation(ctx context.Context, s *Server, conversationID string) bool {
	participants, err := s.store.ListParticipants(ctx, conversationID)
	if err != nil || len(participants) < 2 {
		return false
	}
	return isCrossProjectConversationFromParticipants(ctx, s, participants)
}

// isCrossProjectConversationFromParticipants checks whether a pre-fetched
// participant list contains agents from different projects. This avoids a
// duplicate ListParticipants query when the caller already has the list.
func isCrossProjectConversationFromParticipants(ctx context.Context, s *Server, participants []store.ConversationParticipant) bool {
	if len(participants) < 2 {
		return false
	}

	var projectIDs []string
	for _, p := range participants {
		if p.PrincipalKind == "agent" {
			agent, agErr := s.store.GetAgent(ctx, p.PrincipalID)
			if agErr == nil && agent != nil {
				projectIDs = append(projectIDs, agent.ProjectID)
			}
		}
	}
	if len(projectIDs) < 2 {
		return false
	}
	// Check if any two agents are from different projects.
	first := projectIDs[0]
	for _, pid := range projectIDs[1:] {
		if pid != first {
			return true
		}
	}
	return false
}

// RejectCrossProjectFileTransport returns an explicit denial when an
// unsupported file transport (e.g., path links, plugin channels) is
// attempted across project boundaries.
func RejectCrossProjectFileTransport(transport string) AttachmentTransferDecision {
	return AttachmentTransferDecision{
		Code:   MessageDenialCrossProjectAttachUnsupported,
		Reason: fmt.Sprintf("cross-project file transport %q is not supported; use Hub-managed attachments", transport),
	}
}

// ---------------------------------------------------------------------------
// Phase 5 D5: Content surface authorization
// ---------------------------------------------------------------------------

// ContentSurfaceType identifies the surface being accessed.
type ContentSurfaceType string

const (
	ContentSurfaceStreamEvents ContentSurfaceType = "stream_events"
	ContentSurfaceSearch       ContentSurfaceType = "search"
	ContentSurfacePreview      ContentSurfaceType = "preview"
	ContentSurfaceNotification ContentSurfaceType = "notification"
	ContentSurfaceUnread       ContentSurfaceType = "unread"
	ContentSurfaceEditDelete   ContentSurfaceType = "edit_delete"
	ContentSurfaceReceipt      ContentSurfaceType = "receipt"
	ContentSurfaceTyping       ContentSurfaceType = "typing"
	ContentSurfaceAttachment   ContentSurfaceType = "attachment"
)

// ContentAccessDecision captures the outcome of a content surface
// authorization check.
type ContentAccessDecision struct {
	Allowed        bool
	Surface        ContentSurfaceType
	Code           MessageDenialCode
	Reason         string
	ConversationID string
}

// AuthorizeCrossProjectContentAccess checks whether an identity is authorized
// to access cross-project content on a given surface. Non-participants receive
// no DM payload, preview, attachment metadata, unread signal, typing event,
// or search result.
//
// This is the shared guard applied to ALL content surfaces per design
// Section 11: "Cross-project DM payloads must not be published on either
// project's broadly subscribed message/chat stream."
func (s *Server) AuthorizeCrossProjectContentAccess(
	ctx context.Context,
	requesterID string,
	conversationID string,
	surface ContentSurfaceType,
) ContentAccessDecision {
	if conversationID == "" {
		return ContentAccessDecision{
			Allowed: true, // No conversation context — not a cross-project surface.
			Surface: surface,
		}
	}

	// Check if this is a cross-project conversation.
	if !isCrossProjectConversation(ctx, s, conversationID) {
		return ContentAccessDecision{
			Allowed:        true,
			Surface:        surface,
			ConversationID: conversationID,
		}
	}

	// Cross-project conversation: only participants may access content.
	participants, err := s.store.ListParticipants(ctx, conversationID)
	if err != nil {
		return ContentAccessDecision{
			Surface: surface,
			Code:    MessageDenialCrossProjectContentUnauthorized,
			Reason:  "failed to list conversation participants",
		}
	}

	isParticipant := false
	for _, p := range participants {
		if p.PrincipalID == requesterID {
			isParticipant = true
			break
		}
	}

	if isParticipant {
		// Verify the Hub feature is still enabled.
		ops := s.GetOperationalSettings()
		if ops == nil || !ops.CrossProjectMessagingEnabled() {
			return ContentAccessDecision{
				Surface:        surface,
				Code:           MessageDenialCrossProjectDisabled,
				Reason:         "cross-project messaging is disabled",
				ConversationID: conversationID,
			}
		}
		return ContentAccessDecision{
			Allowed:        true,
			Surface:        surface,
			ConversationID: conversationID,
		}
	}

	return ContentAccessDecision{
		Surface:        surface,
		Code:           MessageDenialCrossProjectContentUnauthorized,
		Reason:         fmt.Sprintf("non-participant cannot access cross-project %s content", surface),
		ConversationID: conversationID,
	}
}

// ReauthorizeSubscription checks whether an existing subscription should
// continue receiving cross-project content after a permission change.
// Called when policy is revoked, mode changes, or the Hub feature is disabled.
func (s *Server) ReauthorizeSubscription(
	ctx context.Context,
	subscriberID string,
	conversationID string,
) bool {
	decision := s.AuthorizeCrossProjectContentAccess(
		ctx, subscriberID, conversationID, ContentSurfaceStreamEvents,
	)
	return decision.Allowed
}

// ---------------------------------------------------------------------------
// Phase 5 D6: Legacy view compatibility
// ---------------------------------------------------------------------------

// LegacyViewQueryMode controls how legacy per-agent/interagent views query
// cross-project messages.
type LegacyViewQueryMode string

const (
	// LegacyViewCanonical queries using canonical endpoint/message IDs.
	// This is the required mode for new cross-project rows.
	LegacyViewCanonical LegacyViewQueryMode = "canonical"
	// LegacyViewLegacy uses the old project-filtered query path.
	// Only for pre-existing same-project rows.
	LegacyViewLegacy LegacyViewQueryMode = "legacy"
)

// ClassifyLegacyViewQuery determines whether a legacy view query for a
// message should use canonical or legacy query mode.
//
// Per design: "Make legacy per-agent/interagent views query canonical
// endpoint/message IDs for new rows without broad project-filter removal
// or sender-slug matching."
func ClassifyLegacyViewQuery(msg *store.Message) LegacyViewQueryMode {
	if msg == nil {
		return LegacyViewLegacy
	}
	// Cross-project messages always use canonical query mode.
	if msg.SenderProjectID != nil && *msg.SenderProjectID != "" &&
		msg.RecipientProjectID != nil && *msg.RecipientProjectID != "" &&
		*msg.SenderProjectID != *msg.RecipientProjectID {
		return LegacyViewCanonical
	}
	// Messages with canonical conversation IDs use the canonical path.
	if msg.ConversationID != "" {
		return LegacyViewCanonical
	}
	return LegacyViewLegacy
}

// ValidateLegacyViewClaims rejects attempts by Message Broker/plugin metadata
// to claim local sender or system-plane exemption on cross-project content.
//
// Per design: "Message Broker/plugin metadata cannot claim local sender or
// system-plane exemption."
func ValidateLegacyViewClaims(
	senderProjectID string,
	claimedLocalSender bool,
	claimedSystemPlane bool,
) *MessageDecision {
	if senderProjectID == "" {
		return nil // No project context — legacy behavior.
	}
	if claimedLocalSender {
		return &MessageDecision{
			Code:   MessageDenialCrossProjectUnsupported,
			Reason: "cross-project messages cannot claim local sender status",
		}
	}
	if claimedSystemPlane {
		return &MessageDecision{
			Code:   MessageDenialCrossProjectUnsupported,
			Reason: "cross-project messages cannot claim system-plane exemption",
		}
	}
	return nil
}

// ---------------------------------------------------------------------------
// Phase 5 D7: Audit and metrics
// ---------------------------------------------------------------------------

// CrossProjectAuditEntry records a cross-project messaging decision for audit.
// Body-free: normal logs omit message bodies and membership lists.
type CrossProjectAuditEntry struct {
	Timestamp        time.Time         `json:"timestamp"`
	Action           string            `json:"action"` // "deliver", "deny", "scheduled_fire", "attachment_download"
	SenderID         string            `json:"senderId"`
	SenderProjectID  string            `json:"senderProjectId"`
	RecipientID      string            `json:"recipientId"`
	RecipientProject string            `json:"recipientProjectId"`
	DecisionCode     MessageDenialCode `json:"decisionCode,omitempty"`
	Reason           string            `json:"reason,omitempty"`
	CrossProject     bool              `json:"crossProject"`
	HubRevision      int64             `json:"hubRevision,omitempty"`
	ProjectRevision  int64             `json:"projectRevision,omitempty"`
	CorrelationID    string            `json:"correlationId,omitempty"`
	Surface          string            `json:"surface,omitempty"`
	ConversationID   string            `json:"conversationId,omitempty"`
}

// LogCrossProjectDecision emits a body-free audit log entry for a cross-project
// messaging decision. Uses structured logging for operator diagnostics.
func LogCrossProjectDecision(entry CrossProjectAuditEntry) {
	attrs := []any{
		"action", entry.Action,
		"sender_id", entry.SenderID,
		"sender_project", entry.SenderProjectID,
		"recipient_id", entry.RecipientID,
		"recipient_project", entry.RecipientProject,
		"cross_project", entry.CrossProject,
	}
	if entry.DecisionCode != "" {
		attrs = append(attrs, "decision_code", string(entry.DecisionCode))
	}
	if entry.Reason != "" {
		attrs = append(attrs, "reason", entry.Reason)
	}
	if entry.HubRevision > 0 {
		attrs = append(attrs, "hub_revision", entry.HubRevision)
	}
	if entry.ProjectRevision > 0 {
		attrs = append(attrs, "project_revision", entry.ProjectRevision)
	}
	if entry.CorrelationID != "" {
		attrs = append(attrs, "correlation_id", entry.CorrelationID)
	}
	if entry.Surface != "" {
		attrs = append(attrs, "surface", entry.Surface)
	}
	if entry.ConversationID != "" {
		attrs = append(attrs, "conversation_id", entry.ConversationID)
	}

	if entry.DecisionCode != "" {
		slog.Warn("cross-project messaging denied", attrs...)
	} else {
		slog.Info("cross-project messaging authorized", attrs...)
	}
}

// DeliveryDeduplicationCheck prevents delivery retries from duplicating
// persisted messages/notifications by checking if a message ID already exists.
func (s *Server) DeliveryDeduplicationCheck(
	ctx context.Context,
	messageID string,
) bool {
	if messageID == "" {
		return false // No ID to check — not a duplicate.
	}
	msg, err := s.store.GetMessage(ctx, messageID)
	if err != nil {
		return false // Error or not found — not a duplicate.
	}
	return msg != nil
}
