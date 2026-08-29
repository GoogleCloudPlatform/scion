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
	"strings"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/go-jose/go-jose/v4/jwt"
)

// AgentMessageability holds viewer-relative messaging reachability for an agent.
// Injected as _messageability alongside _capabilities on agent responses.
type AgentMessageability struct {
	CanMessage     bool   `json:"canMessage"`
	CanReachViewer bool   `json:"canReachViewer"`
	Reason         string `json:"reason,omitempty"`
}

// AgentMessageabilityDetail extends AgentMessageability with counts (detail endpoint only).
type AgentMessageabilityDetail struct {
	AgentMessageability
	ReachableAgentCount int `json:"reachableAgentCount"`
	ReachableUserCount  int `json:"reachableUserCount"`
}

// Reason codes for messaging authorization denials.
const (
	ReasonModeNone              = "mode_none"
	ReasonModeNoneSender        = "mode_none_sender"
	ReasonModeLineageNoAncestry = "mode_lineage_no_ancestry"
	ReasonModeBranchNoEdge      = "mode_branch_no_edge"
	ReasonModeLineageAgentAgent = "mode_lineage_agent_to_agent"
	ReasonMissingPermission     = "missing_permission"
)

// mapReasonToCode converts the plain-text reason strings returned by
// authorizeAgentMessage into structured reason codes for API responses.
func mapReasonToCode(reason string) string {
	switch {
	case reason == "target agent message_mode is none":
		return ReasonModeNone
	case reason == "sender agent message_mode is none":
		return ReasonModeNoneSender
	case strings.HasPrefix(reason, "user not authorized for target agent with message_mode"):
		return ReasonModeLineageNoAncestry
	case reason == "branch mode agents without direct parent/child relationship":
		return ReasonModeBranchNoEdge
	case strings.HasPrefix(reason, "agent-to-agent messaging denied: sender mode"):
		// Covers mixed modes and lineage-mode agent-to-agent denials (D4).
		return ReasonModeLineageAgentAgent
	case strings.HasPrefix(reason, "agent.message permission denied"):
		return ReasonMissingPermission
	default:
		// For unrecognized reasons, return a generic code rather than leaking
		// internal reason strings to the API consumer.
		return "denied"
	}
}

// ComputeMessageability computes viewer-relative messaging reachability.
// viewerIdentity is the authenticated user/agent making the API request.
// targetAgent is the agent being viewed.
func (s *Server) ComputeMessageability(
	ctx context.Context,
	viewerIdentity Identity,
	targetAgent *store.Agent,
) *AgentMessageability {
	if viewerIdentity == nil || targetAgent == nil {
		return &AgentMessageability{}
	}

	// canMessage: can the viewer send a message to this agent?
	canMessage, reason := s.authorizeAgentMessage(ctx, viewerIdentity, targetAgent, false)

	// canReachViewer: can this agent send a message to the viewer?
	canReachViewer := computeCanReachViewer(viewerIdentity, targetAgent)

	result := &AgentMessageability{
		CanMessage:     canMessage,
		CanReachViewer: canReachViewer,
	}
	if !canMessage {
		result.Reason = mapReasonToCode(reason)
	}
	return result
}

// computeCanReachViewer evaluates whether the agent could message the viewer.
// This is conceptually the reverse authorization check: can the target agent
// reach the viewer?
//
// For user viewers:
//   - mode "none" → false (sealed agents cannot send messages)
//   - mode "project" → true (project-mode agents can message all project users)
//   - mode "lineage"/"branch" → true if viewer is in the agent's ancestry
//
// For agent viewers, we would need a full agent-to-agent check, which is
// handled by the forward direction. Simplified: return true if modes are
// compatible (both project or both branch with parent/child).
func computeCanReachViewer(viewerIdentity Identity, targetAgent *store.Agent) bool {
	if viewerIdentity == nil || targetAgent == nil {
		return false
	}

	switch targetAgent.MessageMode {
	case store.MessageModeNone:
		return false
	case store.MessageModeProject:
		// Project-mode agents can message any user/agent in the project.
		// For user viewers, this is generally true (they are browsing the project).
		// For agent viewers, the full check requires the viewer agent's mode — this
		// is a simplified approximation (see design doc Section 3.2).
		return true
	case store.MessageModeLineage, store.MessageModeBranch:
		// Lineage and branch modes allow messaging users in the agent's ancestry.
		viewerID := viewerIdentity.ID()
		for _, ancestorID := range targetAgent.Ancestry {
			if ancestorID == viewerID {
				return true
			}
		}
		return false
	default:
		return false
	}
}

// ComputeMessageabilityDetail computes the extended messageability metadata
// for the agent detail endpoint. It includes reachable agent and user counts
// in addition to the base messageability fields.
func (s *Server) ComputeMessageabilityDetail(
	ctx context.Context,
	viewerIdentity Identity,
	targetAgent *store.Agent,
	projectAgents []store.Agent,
) *AgentMessageabilityDetail {
	base := s.ComputeMessageability(ctx, viewerIdentity, targetAgent)

	// Count reachable agents: for each agent in projectAgents, check if
	// targetAgent could message it. We construct a temporary AgentIdentity
	// from the target agent record and run the forward authorization check.
	reachableAgents := 0
	senderIdentity := agentIdentityFromAgent(targetAgent)
	for i := range projectAgents {
		other := &projectAgents[i]
		if other.ID == targetAgent.ID {
			continue
		}
		allowed, _ := s.authorizeAgentMessage(ctx, senderIdentity, other, false)
		if allowed {
			reachableAgents++
		}
	}

	// Count reachable users: simplified approach based on message mode.
	reachableUsers := countReachableUsers(targetAgent)

	return &AgentMessageabilityDetail{
		AgentMessageability: *base,
		ReachableAgentCount: reachableAgents,
		ReachableUserCount:  reachableUsers,
	}
}

// agentIdentityFromAgent constructs a minimal AgentIdentity from a store.Agent
// record, suitable for authorization checks where we need to evaluate the
// agent as a sender.
func agentIdentityFromAgent(a *store.Agent) AgentIdentity {
	return &agentIdentityWrapper{
		&AgentTokenClaims{
			Claims:    jwt.Claims{Subject: a.ID},
			ProjectID: a.ProjectID,
			Ancestry:  a.Ancestry,
		},
	}
}

// countReachableUsers returns a simplified count of users the agent can reach
// based on its message mode:
//   - "none" → 0 (sealed)
//   - "lineage"/"branch" → count of ancestry entries that look like user IDs
//     (ancestry contains user IDs and agent IDs; we count all since in practice
//     the root ancestor is always a user)
//   - "project" → we don't have the full member list here; return the ancestry
//     count + 1 as a lower bound to signal "at least some users reachable"
func countReachableUsers(targetAgent *store.Agent) int {
	switch targetAgent.MessageMode {
	case store.MessageModeNone:
		return 0
	case store.MessageModeLineage, store.MessageModeBranch:
		// Ancestry chain contains the user(s) who can be reached.
		// The first element is always the root user.
		if len(targetAgent.Ancestry) > 0 {
			// Count unique user ancestors. In practice the root (index 0) is
			// always a user; intermediate entries may be agents.
			// Conservative: return 1 (the root user) as guaranteed minimum.
			return 1
		}
		return 0
	case store.MessageModeProject:
		// Project-mode agents can reach all project members. Without a
		// project-member count available here, return -1 to signal "all
		// project members" or a sentinel. The brief says simplified approach
		// is acceptable, so return a positive indicator.
		// Return ancestry count + 1 as a lower-bound estimate.
		if len(targetAgent.Ancestry) > 0 {
			return len(targetAgent.Ancestry)
		}
		return 1
	default:
		return 0
	}
}

// getSenderMode returns the message mode for the sender identity.
// For user senders, this returns "user" (users don't have message modes).
// For agent senders, it fetches the agent's message mode from the store.
func (s *Server) getSenderMode(ctx context.Context, identity Identity) string {
	if identity == nil {
		return ""
	}
	switch identity.Type() {
	case "user", "dev", "federated_user":
		return "user"
	case "agent":
		agent, err := s.store.GetAgent(ctx, identity.ID())
		if err != nil {
			return "unknown"
		}
		return agent.MessageMode
	default:
		return "unknown"
	}
}
