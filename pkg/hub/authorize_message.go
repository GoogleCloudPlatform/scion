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
	MessageDenialNone                    MessageDenialCode = ""
	MessageDenialCrossProjectDisabled    MessageDenialCode = "cross_project_disabled"
	MessageDenialCrossProjectSenderMode  MessageDenialCode = "cross_project_sender_mode"
	MessageDenialCrossProjectTargetMode  MessageDenialCode = "cross_project_target_mode"
	MessageDenialCrossProjectInboundNone MessageDenialCode = "cross_project_inbound_none"
	MessageDenialCrossProjectNotMember   MessageDenialCode = "cross_project_origin_not_member"
	MessageDenialCrossProjectUntrusted   MessageDenialCode = "cross_project_untrusted_origin"
	MessageDenialCrossProjectUnsupported MessageDenialCode = "cross_project_surface_unsupported"
)

// MessageDecision captures the outcome of an agent message authorization
// evaluation. It replaces the (bool, string) return of authorizeAgentMessage
// with a typed, machine-readable result per design Section 5.
type MessageDecision struct {
	Allowed              bool              `json:"allowed"`
	Code                 MessageDenialCode `json:"code,omitempty"`
	Reason               string            `json:"reason,omitempty"`
	CrossProject         bool              `json:"crossProject,omitempty"`
	HubPolicyRevision    int64             `json:"hubPolicyRevision,omitempty"`
	ProjectPolicyRevision int64            `json:"projectPolicyRevision,omitempty"`
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
// Returns (allowed, reason). When allowed is false, reason describes why.
//
// See docs/messaging-authorization.md for the full decision table and
// piercing rules.
func (s *Server) authorizeAgentMessage(
	ctx context.Context,
	senderIdentity Identity,
	targetAgent *store.Agent,
	isSystemPlane bool,
) (allowed bool, reason string) {
	if senderIdentity == nil {
		return false, "no authenticated identity"
	}
	if targetAgent == nil {
		return false, "nil target agent"
	}

	// ---- D8: system-plane messages bypass all mode checks ----
	if isSystemPlane {
		return true, "system plane bypass"
	}

	// Agent self-message: allow an agent to deliver to itself regardless of mode.
	// This is NOT system-plane (D8); it is a self-access exemption for harness
	// integration (sciontool port-expose, etc.).
	if agentIdent, ok := senderIdentity.(AgentIdentity); ok && agentIdent.ID() == targetAgent.ID {
		return true, "agent self-message"
	}

	// ---- D6: super-admin user pierces everything, including none ----
	if user, ok := senderIdentity.(UserIdentity); ok {
		if IsUnscopedLocalPlatformAdmin(user) {
			return true, "super-admin bypass"
		}
	}

	// Branch by sender type.
	switch senderIdentity.Type() {
	case "user", "dev", "federated_user":
		return s.authorizeUserToAgent(ctx, senderIdentity, targetAgent)
	case "agent":
		return s.authorizeAgentToAgent(ctx, senderIdentity, targetAgent)
	default:
		return false, fmt.Sprintf("identity type %q may not send messages", senderIdentity.Type())
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
func (s *Server) authorizeAgentToAgent(
	ctx context.Context,
	senderIdentity Identity,
	targetAgent *store.Agent,
) (bool, string) {
	decision := s.EvaluateAgentMessage(ctx, senderIdentity, targetAgent)
	return decision.Allowed, decision.Reason
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
	// Read authoritatively from the store, not from cache.
	ops := s.GetOperationalSettings()
	if ops == nil || !ops.CrossProjectMessagingEnabled() {
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
		// Any local agent from any project is allowed. Continue to remaining gates.

	case store.CrossProjectInboundMembers:
		// Gate (e): Ancestry must be hub-attested.
		if !AncestryIsHubAttested(agentIdent) {
			decision.Code = MessageDenialCrossProjectUntrusted
			decision.Reason = "cross-project messaging requires hub-attested ancestry"
			return decision
		}

		// Gate (f): Root human principal must be valid, not disabled/deleted.
		originUserID := agentIdent.OriginUserID()
		if originUserID == "" {
			decision.Code = MessageDenialCrossProjectUntrusted
			decision.Reason = "sender agent has no root human principal in ancestry"
			return decision
		}

		originUser, err := s.store.GetUser(ctx, originUserID)
		if err != nil {
			slog.Warn("EvaluateAgentMessage: failed to fetch origin user",
				"user_id", originUserID, "error", err)
			decision.Code = MessageDenialCrossProjectUntrusted
			decision.Reason = "failed to fetch origin user"
			return decision
		}
		if originUser.Status != "active" {
			decision.Code = MessageDenialCrossProjectUntrusted
			decision.Reason = fmt.Sprintf("origin user is %s, not active", originUser.Status)
			return decision
		}

		// Check membership of origin user in destination project.
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

	default:
		// Unknown policy value → fail closed.
		decision.Code = MessageDenialCrossProjectInboundNone
		decision.Reason = fmt.Sprintf("unknown inbound policy %q, failing closed", inboundPolicy)
		return decision
	}

	// For "any" policy, still validate ancestry and origin user gates
	// (gates e, f apply to all cross-project sends per the design).
	if inboundPolicy == store.CrossProjectInboundAny {
		if !AncestryIsHubAttested(agentIdent) {
			decision.Code = MessageDenialCrossProjectUntrusted
			decision.Reason = "cross-project messaging requires hub-attested ancestry"
			return decision
		}

		originUserID := agentIdent.OriginUserID()
		if originUserID == "" {
			decision.Code = MessageDenialCrossProjectUntrusted
			decision.Reason = "sender agent has no root human principal in ancestry"
			return decision
		}

		originUser, err := s.store.GetUser(ctx, originUserID)
		if err != nil {
			decision.Code = MessageDenialCrossProjectUntrusted
			decision.Reason = "failed to fetch origin user"
			return decision
		}
		if originUser.Status != "active" {
			decision.Code = MessageDenialCrossProjectUntrusted
			decision.Reason = fmt.Sprintf("origin user is %s, not active", originUser.Status)
			return decision
		}
	}

	// All gates passed — cross-project delivery is authorized.
	decision.Allowed = true
	decision.Reason = "cross-project messaging authorized"
	return decision
}

// ---------------------------------------------------------------------------
// storedAgentIdentity adapts a persisted store.Agent record to the
// AgentIdentity interface. Used by the Message Broker retry path (D5) where
// the original JWT is not available. The identity is reconstructed from
// trusted stored data — the persisted ancestry built by the Hub.
// ---------------------------------------------------------------------------

type storedAgentIdentity struct {
	agent *store.Agent
}

func (s *storedAgentIdentity) ID() string          { return s.agent.ID }
func (s *storedAgentIdentity) Type() string         { return "agent" }
func (s *storedAgentIdentity) ProjectID() string    { return s.agent.ProjectID }
func (s *storedAgentIdentity) Scopes() []AgentTokenScope { return nil }
func (s *storedAgentIdentity) HasScope(_ AgentTokenScope) bool { return false }
func (s *storedAgentIdentity) Ancestry() []string   { return s.agent.Ancestry }
func (s *storedAgentIdentity) TokenID() string       { return "" }

func (s *storedAgentIdentity) OriginUserID() string {
	if len(s.agent.Ancestry) > 0 {
		return s.agent.Ancestry[0]
	}
	return ""
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
