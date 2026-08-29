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

	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

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
//     self-messages, state-change notices, scheduled events). Must NEVER
//     be derived from external request data.
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

	// target.mode == project → require agent.message permission on the project
	// (goes through checkAccessForUser including UAT caveat intersection).
	if targetAgent.MessageMode == store.MessageModeProject {
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
	agentIdent, ok := senderIdentity.(AgentIdentity)
	if !ok {
		return false, "invalid agent identity"
	}

	// Fetch the sender agent's record for mode, project, ancestry.
	senderAgent, err := s.store.GetAgent(ctx, agentIdent.ID())
	if err != nil {
		slog.Warn("authorizeAgentMessage: failed to fetch sender agent",
			"sender_id", agentIdent.ID(), "error", err)
		return false, "failed to fetch sender agent record"
	}

	// Either side mode == none → DENY
	if senderAgent.MessageMode == store.MessageModeNone {
		return false, "sender agent message_mode is none"
	}
	if targetAgent.MessageMode == store.MessageModeNone {
		return false, "target agent message_mode is none"
	}

	// Cross-project → DENY
	if senderAgent.ProjectID != targetAgent.ProjectID {
		return false, "cross-project agent-to-agent messaging denied"
	}

	// Both project mode → ALLOW
	if senderAgent.MessageMode == store.MessageModeProject &&
		targetAgent.MessageMode == store.MessageModeProject {
		return true, "both agents in project mode"
	}

	// Both branch mode with parent/child relationship → ALLOW
	if senderAgent.MessageMode == store.MessageModeBranch &&
		targetAgent.MessageMode == store.MessageModeBranch {
		if isDirectParentChild(senderAgent, targetAgent) {
			return true, "branch mode parent/child relationship"
		}
		return false, "branch mode agents without direct parent/child relationship"
	}

	// All other combinations (including lineage mode, mixed modes) → DENY
	// Lineage-mode agents have NO agent-to-agent edges (D4).
	return false, fmt.Sprintf(
		"agent-to-agent messaging denied: sender mode %q, target mode %q",
		senderAgent.MessageMode, targetAgent.MessageMode,
	)
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
