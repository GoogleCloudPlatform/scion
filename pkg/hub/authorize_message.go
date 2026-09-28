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
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// ---------------------------------------------------------------------------
// Phase 2: Cross-project messaging types and evaluator
// ---------------------------------------------------------------------------

// MessageDenialCode is a stable, machine-readable denial code for messaging
// authorization decisions. Codes are lower_snake_case and must not be reused
// across semantically different denials.
type MessageDenialCode string

const (
	MessageDenialNone                            MessageDenialCode = ""
	MessageDenialCrossProjectDisabled            MessageDenialCode = "cross_project_disabled"
	MessageDenialCrossProjectSenderMode          MessageDenialCode = "cross_project_sender_mode"
	MessageDenialCrossProjectTargetMode          MessageDenialCode = "cross_project_target_mode"
	MessageDenialCrossProjectInboundNone         MessageDenialCode = "cross_project_inbound_none"
	MessageDenialCrossProjectNotMember           MessageDenialCode = "cross_project_origin_not_member"
	MessageDenialCrossProjectUntrusted           MessageDenialCode = "cross_project_untrusted_origin"
	MessageDenialCrossProjectUnsupported         MessageDenialCode = "cross_project_surface_unsupported"
	MessageDenialCrossProjectGroupsUnsupported   MessageDenialCode = "cross_project_groups_unsupported"
	MessageDenialCrossProjectAttachUnsupported   MessageDenialCode = "cross_project_attachment_unsupported"
	MessageDenialCrossProjectScheduledDenied     MessageDenialCode = "cross_project_scheduled_denied"
	MessageDenialCrossProjectScheduledDisabled   MessageDenialCode = "cross_project_scheduled_disabled"
	MessageDenialCrossProjectScheduledTarget     MessageDenialCode = "cross_project_scheduled_target" // reserved: scheduled message target validation
	MessageDenialCrossProjectContentUnauthorized MessageDenialCode = "cross_project_content_unauthorized"
	MessageDenialScheduledCreatorDeleted         MessageDenialCode = "scheduled_message_creator_deleted"  // reserved: creator lifecycle checks
	MessageDenialScheduledCreatorInactive        MessageDenialCode = "scheduled_message_creator_inactive" // reserved: creator lifecycle checks
	MessageDenialScheduledTargetDeleted          MessageDenialCode = "scheduled_message_target_deleted"
	MessageDenialScheduledModeChanged            MessageDenialCode = "scheduled_message_mode_changed" // reserved: mode-change detection at fire time
	MessageDenialAttachmentNotFound              MessageDenialCode = "attachment_not_found"
	MessageDenialAttachmentUnauthorized          MessageDenialCode = "attachment_unauthorized"
	MessageDenialDeliveryDuplicate               MessageDenialCode = "delivery_duplicate" // reserved: delivery deduplication guard
)

// MessageDecision captures the outcome of an agent message authorization
// evaluation. It replaces the (bool, string) return of authorizeAgentMessage
// with a typed, machine-readable result per design Section 5.
type MessageDecision struct {
	Allowed               bool              `json:"allowed"`
	Code                  MessageDenialCode `json:"code,omitempty"`
	Reason                string            `json:"reason,omitempty"`
	CrossProject          bool              `json:"crossProject,omitempty"`
	HubPolicyRevision     int64             `json:"hubPolicyRevision,omitempty"`
	ProjectPolicyRevision int64             `json:"projectPolicyRevision,omitempty"`
}

// ---------------------------------------------------------------------------
// D1: Effective membership reader
// ---------------------------------------------------------------------------

// EffectiveMembershipResult distinguishes a negative membership result from a store
// error. Both refuse delivery, but infrastructure failure should be retryable.
type EffectiveMembershipResult struct {
	IsMember bool
	Role     string // highest built-in role found: owner, admin, member, or ""
	Err      error  // non-nil only for infrastructure/store errors
}

// CheckEffectiveMembership checks whether a user is an active member of a
// project by examining direct and effective-group role bindings with
// active-time checks.
//
// Membership means holding a built-in member, admin, or owner binding
// (including valid group-derived member/admin). An owner counts as a member;
// ownership does not permit piercing a target's mode.
//
// Ignored: expired, not-yet-active, revoked, custom additive role bindings.
// NOT membership: public project visibility, generic read grant, shared
// conversation, or Hub-admin status.
//
// Returns EffectiveMembershipResult with IsMember=true and the highest role if the
// user is a member, IsMember=false with Err=nil for a definite non-member,
// or IsMember=false with Err!=nil for infrastructure errors.
func (s *Server) CheckEffectiveMembership(ctx context.Context, userID, projectID string) EffectiveMembershipResult {
	now := time.Now()

	// 1. Direct user bindings.
	directBindings, err := s.store.ListRoleBindingsForPrincipal(ctx, store.RoleBindingPrincipalUser, userID)
	if err != nil {
		return EffectiveMembershipResult{Err: fmt.Errorf("list direct bindings for user %s: %w", userID, err)}
	}

	rdCache := make(map[string]*store.RoleDefinition)
	getRoleDef := func(rdID string) (*store.RoleDefinition, error) {
		if rd, ok := rdCache[rdID]; ok {
			return rd, nil
		}
		rd, err := s.store.GetRoleDefinition(ctx, rdID)
		if err != nil {
			return nil, fmt.Errorf("get role definition %s: %w", rdID, err)
		}
		if rd == nil {
			return nil, fmt.Errorf("role definition %s is nil", rdID)
		}
		rdCache[rdID] = rd
		return rd, nil
	}

	bestRole := ""
	for _, rb := range directBindings {
		if rb.ScopeType != store.RoleScopeProject || rb.ScopeID != projectID {
			continue
		}
		if !isBindingActive(rb, now) {
			continue
		}
		rd, rdErr := getRoleDef(rb.RoleDefinitionID)
		if rdErr != nil {
			return EffectiveMembershipResult{Err: rdErr}
		}
		// Only built-in membership roles count.
		if !store.IsBuiltInProjectMembershipRole(rd.Name) {
			continue
		}
		bestRole = higherProjectRole(bestRole, rd.Name)
	}

	// 2. Group-derived bindings.
	groupIDs, err := s.store.GetEffectiveGroups(ctx, userID)
	if err != nil {
		return EffectiveMembershipResult{Err: fmt.Errorf("get effective groups for user %s: %w", userID, err)}
	}
	if len(groupIDs) > 0 {
		var principals []store.PrincipalRef
		for _, gid := range groupIDs {
			principals = append(principals, store.PrincipalRef{Type: store.RoleBindingPrincipalGroup, ID: gid})
		}
		groupBindings, err := s.store.ListRoleBindingsForPrincipals(ctx, principals, nil, nil)
		if err != nil {
			return EffectiveMembershipResult{Err: fmt.Errorf("list group bindings: %w", err)}
		}
		for _, rb := range groupBindings {
			if rb.ScopeType != store.RoleScopeProject || rb.ScopeID != projectID {
				continue
			}
			if !isBindingActive(rb, now) {
				continue
			}
			rd, rdErr := getRoleDef(rb.RoleDefinitionID)
			if rdErr != nil {
				return EffectiveMembershipResult{Err: rdErr}
			}
			// Groups can only confer admin or member, never owner.
			if rd.Name == store.ProjectRoleOwner {
				continue
			}
			if !store.IsBuiltInProjectMembershipRole(rd.Name) {
				continue
			}
			bestRole = higherProjectRole(bestRole, rd.Name)
		}
	}

	if bestRole == "" {
		return EffectiveMembershipResult{IsMember: false}
	}
	return EffectiveMembershipResult{IsMember: true, Role: bestRole}
}

// authorizeAgentMessage is the single choke point for ALL messaging
// authorization. It implements the decision logic from design doc Section 5
// (D1-D10). Every ingress (direct API, chat v2, broadcast, broker inbound)
// must call this function before delivering a message.
//
// The decision table evaluates in this order:
//
//  1. System-plane messages bypass all checks (D8).
//  2. Agent self-messages are allowed (harness integration).
//  3. Super-admin users pierce everything including mode=none (D6).
//  4. User senders: ancestry and project-owner piercing for lineage/branch;
//     agent.message permission check for project mode; none always denied.
//  5. Agent senders: both endpoints must be in the same project; both must
//     be project mode (project cell) or both branch mode with a direct
//     parent/child relationship (branch cell); lineage-mode agents have
//     zero agent-to-agent edges (D4).
//
// Parameters:
//   - senderIdentity: the authenticated caller (user or agent).
//   - targetAgent: the target agent record (freshly read from the store;
//     mode is evaluated live per D10).
//   - isSystemPlane: true ONLY for hub-internal system messages (sciontool
//     self-messages, state-change notices). Must NEVER be derived from
//     external request data. Scheduled events are NOT system-plane: they
//     are request-derived (authored by a user or agent) and must be
//     authorized with isSystemPlane=false at both authoring and fire time.
//
// Returns (allowed, reason, decision). When allowed is false, reason
// describes why. For agent-to-agent paths, decision carries a typed
// MessageDecision with stable denial codes; for user paths it is nil.
//
// See docs/messaging-authorization.md for the full decision table and
// piercing rules.
func (s *Server) authorizeAgentMessage(
	ctx context.Context,
	senderIdentity Identity,
	targetAgent *store.Agent,
	isSystemPlane bool,
) (allowed bool, reason string, decision *MessageDecision) {
	if senderIdentity == nil {
		return false, "no authenticated identity", nil
	}
	if targetAgent == nil {
		return false, "nil target agent", nil
	}

	// ---- D8: system-plane messages bypass all mode checks ----
	if isSystemPlane {
		return true, "system plane bypass", nil
	}

	// Agent self-message: allow an agent to deliver to itself regardless of mode.
	// This is NOT system-plane (D8); it is a self-access exemption for harness
	// integration (sciontool port-expose, etc.).
	if agentIdent, ok := senderIdentity.(AgentIdentity); ok && agentIdent.ID() == targetAgent.ID {
		return true, "agent self-message", nil
	}

	// ---- D6: super-admin user pierces everything, including none ----
	if user, ok := senderIdentity.(UserIdentity); ok {
		if IsUnscopedLocalPlatformAdmin(user) {
			return true, "super-admin bypass", nil
		}
	}

	// Branch by sender type.
	switch senderIdentity.Type() {
	case "user", "dev", "federated_user":
		allowed, reason := s.authorizeUserToAgent(ctx, senderIdentity, targetAgent)
		return allowed, reason, nil
	case "agent":
		d := s.authorizeAgentToAgent(ctx, senderIdentity, targetAgent)
		return d.Allowed, d.Reason, &d
	default:
		return false, fmt.Sprintf("identity type %q may not send messages", senderIdentity.Type()), nil
	}
}

// authorizeUserToAgent implements the user-sender path of the messaging
// decision logic. Piercing lives here — user-identity only, never inherited
// by an owner's agents (D6).
func (s *Server) authorizeUserToAgent(
	ctx context.Context,
	senderIdentity Identity,
	targetAgent *store.Agent,
) (bool, string) {
	userIdent, ok := senderIdentity.(UserIdentity)
	if !ok {
		return false, "invalid user identity"
	}

	// target.mode == none → DENY (super-admin already handled above)
	if targetAgent.MessageMode == store.MessageModeNone {
		return false, "target agent message_mode is none"
	}

	targetResource := agentResource(targetAgent)

	// D6 UAT caveat: piercing applies only when the token carries agent:message.
	// Full-session users (non-UAT) always have piercing ability.
	uatDeniesMessage := false
	if scoped, ok := userIdent.(*ScopedUserIdentity); ok {
		if !scoped.HasScope("agent:message") {
			uatDeniesMessage = true
		}
	}

	// Ancestry check: U in target.Ancestry → ALLOW (lineage/branch/project)
	// Only trust ancestry when hub-attested (not federated).
	if !uatDeniesMessage && AncestryIsHubAttested(senderIdentity) {
		if canAccessAsAncestor(userIdent.ID(), targetResource) {
			return true, "user in target ancestry"
		}
	}

	// Project owner pierces lineage/branch/project (D6).
	// Owner only — NOT admin. Admin piercing would let admins unseal agents.
	if !uatDeniesMessage && s.isProjectOwner(ctx, userIdent.ID(), targetAgent.ProjectID) {
		return true, "project owner piercing"
	}

	// target.mode == project or hub → require agent.message permission on the project
	// (evaluated via AK1 kernel including UAT credential caveat intersection).
	// For user delivery, hub behaves like project under existing authorization.
	if targetAgent.MessageMode == store.MessageModeProject || targetAgent.MessageMode == store.MessageModeHub {
		decision := s.authzService.CheckAccess(ctx, userIdent, targetResource, ActionMessage)
		if decision.Allowed {
			return true, "agent.message permission granted"
		}
		return false, "agent.message permission denied: " + decision.Reason
	}

	// target.mode is lineage or branch, and sender is not in ancestry and
	// not project owner → DENY.
	return false, fmt.Sprintf("user not authorized for target agent with message_mode %q", targetAgent.MessageMode)
}

// authorizeAgentToAgent implements the agent-sender path of the messaging
// decision logic. Agents NEVER pierce mode restrictions, even if their origin
// user is a super-admin or project owner (D6 pinning rule).
//
// Returns a MessageDecision with typed denial codes, eliminating the need for
// callers to re-evaluate via EvaluateAgentMessage.
func (s *Server) authorizeAgentToAgent(
	ctx context.Context,
	senderIdentity Identity,
	targetAgent *store.Agent,
) MessageDecision {
	return s.EvaluateAgentMessage(ctx, senderIdentity, targetAgent)
}

// EvaluateAgentMessage is the typed cross-project message evaluator (Phase 2,
// D2). It implements the full decision logic from design Section 5:
//
//  1. Authenticated local agent + current valid sender/receiver records
//  2. Same project? → existing mode matrix (with hub/project compatibility)
//  3. Different project? → ALL of the following must pass:
//     a. Hub cross_project_messaging_enabled is true (authoritative store read)
//     b. Sender mode == hub
//     c. Receiver mode is project OR hub
//     d. Destination project's crossProjectInbound policy allows sender
//     e. Ancestry is hub-attested (reject federated identities)
//     f. Root human principal is valid, not disabled/deleted
//
// Returns a MessageDecision with stable denial codes.
func (s *Server) EvaluateAgentMessage(
	ctx context.Context,
	senderIdentity Identity,
	targetAgent *store.Agent,
) MessageDecision {
	if targetAgent == nil {
		return MessageDecision{Reason: "nil target agent"}
	}

	agentIdent, ok := senderIdentity.(AgentIdentity)
	if !ok {
		return MessageDecision{Reason: "invalid agent identity"}
	}

	// Fetch the sender agent's record for mode, project, ancestry.
	senderAgent, err := s.store.GetAgent(ctx, agentIdent.ID())
	if err != nil {
		slog.Warn("EvaluateAgentMessage: failed to fetch sender agent",
			"sender_id", agentIdent.ID(), "error", err)
		return MessageDecision{Reason: "failed to fetch sender agent record"}
	}
	if senderAgent == nil {
		return MessageDecision{Reason: "sender agent record is nil"}
	}

	// Either side mode == none → DENY
	if senderAgent.MessageMode == store.MessageModeNone {
		return MessageDecision{Reason: "sender agent message_mode is none"}
	}
	if targetAgent.MessageMode == store.MessageModeNone {
		return MessageDecision{Reason: "target agent message_mode is none"}
	}

	// Same-project path: use existing mode matrix.
	if senderAgent.ProjectID == targetAgent.ProjectID {
		return s.evaluateSameProjectModes(senderAgent, targetAgent)
	}

	// Cross-project path: evaluate all required gates.
	return s.evaluateCrossProject(ctx, agentIdent, senderAgent, targetAgent)
}

// evaluateSameProjectModes implements the same-project mode compatibility
// matrix from design Section 3.
func (s *Server) evaluateSameProjectModes(sender, target *store.Agent) MessageDecision {
	sMode := sender.MessageMode
	tMode := target.MessageMode

	// Both in project cell (project or hub) → ALLOW
	if (sMode == store.MessageModeProject || sMode == store.MessageModeHub) &&
		(tMode == store.MessageModeProject || tMode == store.MessageModeHub) {
		return MessageDecision{Allowed: true, Reason: "both agents in project/hub communication cell"}
	}

	// Both branch mode with parent/child relationship → ALLOW
	if sMode == store.MessageModeBranch && tMode == store.MessageModeBranch {
		if isDirectParentChild(sender, target) {
			return MessageDecision{Allowed: true, Reason: "branch mode parent/child relationship"}
		}
		return MessageDecision{Reason: "branch mode agents without direct parent/child relationship"}
	}

	// All other combinations (including lineage mode, mixed modes) → DENY
	return MessageDecision{
		Reason: fmt.Sprintf("agent-to-agent messaging denied: sender mode %q, target mode %q", sMode, tMode),
	}
}

// evaluateCrossProject implements the cross-project authorization gates from
// design Section 5. All gates must pass for the delivery to be authorized.
func (s *Server) evaluateCrossProject(
	ctx context.Context,
	agentIdent AgentIdentity,
	senderAgent, targetAgent *store.Agent,
) MessageDecision {
	decision := MessageDecision{CrossProject: true}

	// Gate (a): Hub cross_project_messaging_enabled must be true.
	// Authoritative store read — bypasses the replica-local cache so that the
	// very next send after a policy change enforces the current setting,
	// regardless of notification/poll propagation state (#1686).
	ops := s.GetOperationalSettings()
	if ops == nil {
		decision.Code = MessageDenialCrossProjectDisabled
		decision.Reason = "cross-project messaging is not enabled on this Hub (no operational settings)"
		return decision
	}
	cpmSetting := ops.ReadAuthoritativeCrossProjectEnabled(ctx)
	if cpmSetting.Err != nil {
		decision.Code = MessageDenialCrossProjectDisabled
		decision.Reason = "cross-project messaging setting read failed (failing closed)"
		return decision
	}
	decision.HubPolicyRevision = cpmSetting.Revision
	if !cpmSetting.Enabled {
		decision.Code = MessageDenialCrossProjectDisabled
		decision.Reason = "cross-project messaging is not enabled on this Hub"
		return decision
	}

	// Gate (b): Sender mode must be hub.
	if senderAgent.MessageMode != store.MessageModeHub {
		decision.Code = MessageDenialCrossProjectSenderMode
		decision.Reason = fmt.Sprintf("cross-project messaging requires sender mode hub, got %q", senderAgent.MessageMode)
		return decision
	}

	// Gate (c): Receiver mode must be project OR hub.
	if targetAgent.MessageMode != store.MessageModeProject && targetAgent.MessageMode != store.MessageModeHub {
		decision.Code = MessageDenialCrossProjectTargetMode
		decision.Reason = fmt.Sprintf("cross-project target must be in project or hub mode, got %q", targetAgent.MessageMode)
		return decision
	}

	// Gate (d): Destination project's crossProjectInbound policy.
	destProject, err := s.store.GetProject(ctx, targetAgent.ProjectID)
	if err != nil {
		slog.Warn("EvaluateAgentMessage: failed to fetch destination project",
			"project_id", targetAgent.ProjectID, "error", err)
		decision.Reason = "failed to fetch destination project"
		return decision
	}
	if destProject == nil {
		decision.Reason = "destination project record is nil"
		return decision
	}

	inboundPolicy := destProject.CrossProjectInbound
	if inboundPolicy == "" {
		inboundPolicy = store.CrossProjectInboundNone
	}
	decision.ProjectPolicyRevision = destProject.CrossProjectInboundRevision

	switch inboundPolicy {
	case store.CrossProjectInboundNone:
		decision.Code = MessageDenialCrossProjectInboundNone
		decision.Reason = "destination project does not accept external agent messages"
		return decision

	case store.CrossProjectInboundAny:
		// Any local agent from any project is allowed after origin validation.

	case store.CrossProjectInboundMembers:
		// Members policy requires origin validation AND membership check.

	default:
		// Unknown policy value → fail closed.
		decision.Code = MessageDenialCrossProjectInboundNone
		decision.Reason = fmt.Sprintf("unknown inbound policy %q, failing closed", inboundPolicy)
		return decision
	}

	// Gates (e, f): validate ancestry attestation and origin user for all
	// cross-project sends (both "any" and "members" policies).
	originUserID, originDenial := s.validateCrossProjectOrigin(ctx, agentIdent)
	if originDenial != nil {
		return *originDenial
	}

	// "members" policy additionally requires membership in the destination project.
	if inboundPolicy == store.CrossProjectInboundMembers {
		memberResult := s.CheckEffectiveMembership(ctx, originUserID, targetAgent.ProjectID)
		if memberResult.Err != nil {
			slog.Warn("EvaluateAgentMessage: membership check failed",
				"user_id", originUserID, "project_id", targetAgent.ProjectID,
				"error", memberResult.Err)
			// Infrastructure error → retryable denial, not a membership denial.
			decision.Reason = "membership check failed: " + memberResult.Err.Error()
			return decision
		}
		if !memberResult.IsMember {
			decision.Code = MessageDenialCrossProjectNotMember
			decision.Reason = "origin user is not an active member of the destination project"
			return decision
		}
	}

	// All gates passed — cross-project delivery is authorized.
	decision.Allowed = true
	decision.Reason = "cross-project messaging authorized"
	return decision
}

// validateCrossProjectOrigin implements gates (e) and (f) for cross-project
// messaging: ancestry must be hub-attested and the root human principal must
// be valid and active. Called once for both "members" and "any" inbound
// policies — eliminates the duplication that previously existed between the
// two branches.
//
// Returns (originUserID, nil) on success, or ("", *MessageDecision) with a
// typed denial on failure.
func (s *Server) validateCrossProjectOrigin(ctx context.Context, agentIdent AgentIdentity) (string, *MessageDecision) {
	// Gate (e): Ancestry must be hub-attested (reject federated identities).
	if !AncestryIsHubAttested(agentIdent) {
		return "", &MessageDecision{
			CrossProject: true,
			Code:         MessageDenialCrossProjectUntrusted,
			Reason:       "cross-project messaging requires hub-attested ancestry",
		}
	}

	// Gate (f): Root human principal must be valid, not disabled/deleted.
	originUserID := agentIdent.OriginUserID()
	if originUserID == "" {
		return "", &MessageDecision{
			CrossProject: true,
			Code:         MessageDenialCrossProjectUntrusted,
			Reason:       "sender agent has no root human principal in ancestry",
		}
	}

	originUser, err := s.store.GetUser(ctx, originUserID)
	if err != nil {
		slog.Warn("validateCrossProjectOrigin: failed to fetch origin user",
			"user_id", originUserID, "error", err)
		return "", &MessageDecision{
			CrossProject: true,
			Code:         MessageDenialCrossProjectUntrusted,
			Reason:       "failed to fetch origin user",
		}
	}
	if originUser == nil {
		return "", &MessageDecision{
			CrossProject: true,
			Code:         MessageDenialCrossProjectUntrusted,
			Reason:       "origin user record is nil",
		}
	}
	if originUser.Status != "active" {
		return "", &MessageDecision{
			CrossProject: true,
			Code:         MessageDenialCrossProjectUntrusted,
			Reason:       fmt.Sprintf("origin user is %s, not active", originUser.Status),
		}
	}

	return originUserID, nil
}

// ---------------------------------------------------------------------------
// storedAgentIdentity adapts a persisted store.Agent record to the
// AgentIdentity interface. Used by the Message Broker retry path (D5) where
// the original JWT is not available. The identity is reconstructed from
// trusted stored data — the persisted ancestry built by the Hub.
// ---------------------------------------------------------------------------

// Compile-time interface assertion.
var _ AgentIdentity = (*storedAgentIdentity)(nil)

type storedAgentIdentity struct {
	agent *store.Agent
}

func (s *storedAgentIdentity) ID() string                      { return s.agent.ID }
func (s *storedAgentIdentity) Type() string                    { return "agent" }
func (s *storedAgentIdentity) ProjectID() string               { return s.agent.ProjectID }
func (s *storedAgentIdentity) Scopes() []AgentTokenScope       { return nil }
func (s *storedAgentIdentity) HasScope(_ AgentTokenScope) bool { return false }
func (s *storedAgentIdentity) Ancestry() []string              { return s.agent.Ancestry }
func (s *storedAgentIdentity) TokenID() string                 { return "" }

func (s *storedAgentIdentity) OriginUserID() string {
	if len(s.agent.Ancestry) > 0 {
		return s.agent.Ancestry[0]
	}
	return ""
}

// authorizeCrossProjectAgentMessage implements the cross-project path of the
// agent-to-agent messaging decision logic (design §2, §3, §5).
//
// Decision order:
//  1. Sender must be hub mode.
//  2. Recipient must be project or hub mode.
//  3. Hub cross_project_messaging_enabled must be true.
//  4. Destination project's crossProjectInbound policy must permit the sender.
//  5. Both agent and project records must be valid (not deleted).
//
// prefetchedCPM, when non-nil, provides a pre-fetched authoritative
// cross-project setting read so that callers who have already performed the
// store read (e.g. EvaluateCrossProjectReadAccess) avoid redundant queries.
// When nil, the method performs its own authoritative read.
func (s *Server) authorizeCrossProjectAgentMessage(
	ctx context.Context,
	senderAgent, targetAgent *store.Agent,
	sMode, tMode string,
	prefetchedCPM *CrossProjectSettingResult,
) (bool, string) {
	// 1. Sender must be hub mode — project-mode agents cannot send cross-project.
	if sMode != store.MessageModeHub {
		return false, "cross_project_sender_mode"
	}

	// 2. Recipient must be project or hub mode. branch/lineage/none cannot
	//    receive external messages.
	if tMode != store.MessageModeProject && tMode != store.MessageModeHub {
		return false, "cross_project_target_mode"
	}

	// 3. Hub-level kill switch — authoritative store read (#1686).
	if prefetchedCPM != nil {
		if prefetchedCPM.Err != nil || !prefetchedCPM.Enabled {
			return false, "cross_project_disabled"
		}
	} else {
		ops := s.GetOperationalSettings()
		if ops == nil {
			return false, "cross_project_disabled"
		}
		cpmSetting := ops.ReadAuthoritativeCrossProjectEnabled(ctx)
		if cpmSetting.Err != nil || !cpmSetting.Enabled {
			return false, "cross_project_disabled"
		}
	}

	// 4. Destination project's inbound policy.
	targetProject, err := s.store.GetProject(ctx, targetAgent.ProjectID)
	if err != nil || targetProject == nil {
		slog.Warn("authorizeCrossProjectAgentMessage: failed to fetch target project",
			"project_id", targetAgent.ProjectID, "error", err)
		return false, "cross_project_target_project_unavailable"
	}

	switch targetProject.CrossProjectInbound {
	case store.CrossProjectInboundNone, "":
		return false, "cross_project_inbound_none"

	case store.CrossProjectInboundMembers:
		// Sender's origin user must be a current member of the target project.
		originUserID := resolveOriginUserID(senderAgent)
		if originUserID == "" {
			return false, "cross_project_untrusted_origin"
		}
		if !s.isActiveMember(ctx, originUserID, targetAgent.ProjectID) {
			return false, "cross_project_origin_not_member"
		}

	case store.CrossProjectInboundAny:
		// Accept from any eligible agent on this Hub — no origin check needed.

	default:
		return false, fmt.Sprintf("cross_project_inbound_unknown: %q", targetProject.CrossProjectInbound)
	}

	// 5. Verify sender project is also valid.
	senderProject, err := s.store.GetProject(ctx, senderAgent.ProjectID)
	if err != nil || senderProject == nil {
		return false, "cross_project_sender_project_unavailable"
	}

	return true, "cross_project_authorized"
}

// resolveOriginUserID extracts the root human principal from an agent's
// hub-attested ancestry chain. The first element of the ancestry is the
// originating user.
func resolveOriginUserID(agent *store.Agent) string {
	if agent == nil {
		return ""
	}
	if len(agent.Ancestry) > 0 {
		return agent.Ancestry[0]
	}
	return agent.CreatedBy
}

// isActiveMember checks whether a user has active membership in the given project.
// Counts built-in member/admin/owner bindings. Does not count custom-only roles
// or public visibility.
func (s *Server) isActiveMember(ctx context.Context, userID, projectID string) bool {
	if userID == "" || projectID == "" {
		return false
	}
	membership, err := s.store.GetProjectMembership(ctx, projectID, userID)
	if err != nil || membership == nil {
		return false
	}
	// Owner, admin, and member roles all count as active membership.
	switch membership.Role {
	case store.ProjectRoleOwner, store.ProjectRoleAdmin, store.ProjectRoleMember:
		return true
	default:
		return false
	}
}

// EvaluateCrossProjectReadAccess checks whether an agent can read cross-project
// conversation history. Requires canonical participation, Hub enabled, valid
// endpoint/project records, and at least one currently permitted direction.
func (s *Server) EvaluateCrossProjectReadAccess(
	ctx context.Context,
	readerAgent *store.Agent,
	peerAgent *store.Agent,
) (allowed bool, reason string) {
	if readerAgent == nil || peerAgent == nil {
		return false, "invalid_agent"
	}

	// Both must be in project or hub mode.
	rMode := readerAgent.MessageMode
	pMode := peerAgent.MessageMode
	if rMode != store.MessageModeProject && rMode != store.MessageModeHub {
		return false, "reader_mode_insufficient"
	}
	if pMode != store.MessageModeProject && pMode != store.MessageModeHub {
		return false, "peer_mode_insufficient"
	}

	// At least one must be hub mode.
	if rMode != store.MessageModeHub && pMode != store.MessageModeHub {
		return false, "no_hub_mode_endpoint"
	}

	// Hub switch must be enabled — authoritative store read (#1686).
	ops := s.GetOperationalSettings()
	if ops == nil {
		return false, "cross_project_disabled"
	}
	cpmSetting := ops.ReadAuthoritativeCrossProjectEnabled(ctx)
	if cpmSetting.Err != nil || !cpmSetting.Enabled {
		return false, "cross_project_disabled"
	}

	// Check at least one permitted direction. Pass the pre-fetched
	// authoritative setting to avoid redundant store reads (#1686).
	// Forward: reader→peer
	forwardAllowed, _ := s.authorizeCrossProjectAgentMessage(ctx, readerAgent, peerAgent, rMode, pMode, &cpmSetting)

	// Reverse: peer→reader
	reverseAllowed, _ := s.authorizeCrossProjectAgentMessage(ctx, peerAgent, readerAgent, pMode, rMode, &cpmSetting)

	if !forwardAllowed && !reverseAllowed {
		return false, "no_permitted_direction"
	}

	return true, "cross_project_read_authorized"
}

// isDirectParentChild reports whether two agents have a direct parent/child
// relationship. The last element of an agent's Ancestry array is its parent
// (which may be a user or an agent).
func isDirectParentChild(a, b *store.Agent) bool {
	// a is b's parent: b's last ancestry entry is a.ID
	if len(b.Ancestry) > 0 && b.Ancestry[len(b.Ancestry)-1] == a.ID {
		return true
	}
	// b is a's parent: a's last ancestry entry is b.ID
	if len(a.Ancestry) > 0 && a.Ancestry[len(a.Ancestry)-1] == b.ID {
		return true
	}
	return false
}

// isProjectOwner reports whether the user has the project-owner role (and ONLY
// the owner role, not admin) in the given project. This is stricter than
// isProjectOwnerOrAdmin: for messaging piercing (D6), only the owner role
// confers the ability to message lineage/branch agents.
func (s *Server) isProjectOwner(ctx context.Context, userID, projectID string) bool {
	if userID == "" || projectID == "" {
		return false
	}
	membership, err := s.store.GetProjectMembership(ctx, projectID, userID)
	if err != nil || membership == nil {
		return false
	}
	return membership.Role == store.ProjectRoleOwner
}
