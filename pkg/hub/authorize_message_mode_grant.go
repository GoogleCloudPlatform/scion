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

// MessageModeGrantDecision is the result of AuthorizeMessageModeGrant.
type MessageModeGrantDecision struct {
	Allowed bool
	Reason  string
}

// AuthorizeMessageModeGrant checks whether the authenticated actor may grant
// the resolved message mode to the target agent. This is the shared guard
// called from all effective-mode mutation paths: explicit creation, template
// resolution, parent inheritance, default resolution, existing-agent mode
// changes, and cascades.
//
// Contract (from 04-decisions-and-mode-ceiling.md):
//   - If the resolved mode is not "hub", retain existing mode rules (no new check).
//   - If human caller: retain existing action-specific human authorization
//     (humans can always grant hub mode if they pass existing authorization).
//   - If agent caller:
//   - Load caller's current stored record; lookup failure denies the grant.
//   - Require caller's stored role == full AND stored message mode == hub.
//   - Intersect with authenticated action scopes (require set_message_mode scope).
//   - Require caller's project == target project.
//   - Otherwise deny.
//
// This function does NOT check whether the actor has existing action-specific
// authorization (e.g. set_message_mode permission, project ownership, lineage
// ownership). That check must happen before calling this function. This guard
// adds the hub-specific non-escalation ceiling on top of existing authorization.
//
// A no-op (target already has hub and mode is not changing) is not a new grant.
// Callers must check isNewHubGrant before calling this function.
func (s *Server) AuthorizeMessageModeGrant(
	ctx context.Context,
	actor Identity,
	targetProjectID string,
	resolvedMode string,
) MessageModeGrantDecision {
	// Non-hub modes: no additional check needed.
	if resolvedMode != store.MessageModeHub {
		return MessageModeGrantDecision{Allowed: true, Reason: "non-hub mode"}
	}

	if actor == nil {
		return MessageModeGrantDecision{Allowed: false, Reason: "no authenticated identity"}
	}

	switch actor.Type() {
	case "user", "dev", "federated_user":
		// Human callers retain existing action-specific authorization.
		// If they passed the existing authorization checks (project owner,
		// lineage owner, super-admin), they may grant hub mode.
		return MessageModeGrantDecision{Allowed: true, Reason: "human caller authorized"}

	case "agent":
		return s.authorizeAgentHubGrant(ctx, actor, targetProjectID)

	default:
		return MessageModeGrantDecision{
			Allowed: false,
			Reason:  fmt.Sprintf("identity type %q cannot grant hub mode", actor.Type()),
		}
	}
}

// authorizeAgentHubGrant implements the agent-caller path of the hub mode
// grant guard. An agent may grant hub only when it is currently full-role
// and already hub-mode, with the required authenticated action scopes
// and same-project target.
func (s *Server) authorizeAgentHubGrant(
	ctx context.Context,
	actor Identity,
	targetProjectID string,
) MessageModeGrantDecision {
	agentIdent, ok := actor.(AgentIdentity)
	if !ok {
		return MessageModeGrantDecision{
			Allowed: false,
			Reason:  "invalid agent identity",
		}
	}

	// Load the caller's current stored record. Lookup failure denies the grant.
	callerAgent, err := s.store.GetAgent(ctx, agentIdent.ID())
	if err != nil {
		slog.Warn("AuthorizeMessageModeGrant: failed to load caller agent record",
			"caller_id", agentIdent.ID(), "error", err)
		return MessageModeGrantDecision{
			Allowed: false,
			Reason:  "failed to load caller agent record",
		}
	}

	// Require caller's stored role == full.
	callerRole, _ := agentRoleAndScopes(callerAgent)
	if callerRole != AgentRoleFull {
		return MessageModeGrantDecision{
			Allowed: false,
			Reason:  fmt.Sprintf("agent caller role is %q, must be %q to grant hub mode", callerRole, AgentRoleFull),
		}
	}

	// Require caller's stored message mode == hub.
	if callerAgent.MessageMode != store.MessageModeHub {
		return MessageModeGrantDecision{
			Allowed: false,
			Reason: fmt.Sprintf(
				"agent caller message mode is %q, must be %q to grant hub mode",
				callerAgent.MessageMode, store.MessageModeHub),
		}
	}

	// Intersect with authenticated action scopes: require set_message_mode.
	if !agentIdent.HasScope(ScopeAgentSetMessageMode) {
		return MessageModeGrantDecision{
			Allowed: false,
			Reason:  "agent lacks project:agent:set_message_mode scope",
		}
	}

	// Require caller's project == target project.
	if agentIdent.ProjectID() != targetProjectID {
		return MessageModeGrantDecision{
			Allowed: false,
			Reason:  "agent caller is in a different project than the target",
		}
	}

	return MessageModeGrantDecision{Allowed: true, Reason: "full/hub agent authorized"}
}

// isNewHubGrant returns true if setting resolvedMode on an agent with
// currentMode constitutes a new hub grant (requiring the grant guard).
// Restart or unrelated update retaining hub is NOT a new grant.
func isNewHubGrant(currentMode, resolvedMode string) bool {
	if resolvedMode != store.MessageModeHub {
		return false
	}
	// If the agent already has hub mode, this is not a new grant.
	return currentMode != store.MessageModeHub
}
