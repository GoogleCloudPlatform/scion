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
	"log/slog"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// conversationParticipantView is a conversation participant as the
// single-conversation GET returns it: the stored participant row plus,
// for an agent participant, that agent's message mode (ptone/scion#4411).
// MessageMode is response-only and never persisted.
type conversationParticipantView struct {
	store.ConversationParticipant
	// MessageMode is set only for an agent participant in the
	// conversation's own project (see conversationModeProject). It is
	// empty for users, for agents in other projects, and when the agent
	// could not be looked up.
	MessageMode string `json:"messageMode,omitempty"`
}

// conversationGetResponse is the body of GET /api/v1/conversations/{id}.
// It matches conversationResponse on the wire, with participants carrying
// the message mode of in-project agents.
type conversationGetResponse struct {
	store.Conversation
	Participants []conversationParticipantView `json:"participants,omitempty"`
}

// conversationModeProject returns the project whose agents' message modes
// the conversation GET may show, or "" to show none. It is the
// conversation's project when it has one. A direct (or legacy projectless)
// conversation has none, so it is the calling agent's project for an agent
// caller, and otherwise the one project all agent participants share.
// Agents outside that project never have their mode shown, so the response
// discloses nothing new about other projects.
func conversationModeProject(conv *store.Conversation, callerAgentProjectID string, agents map[string]*store.Agent) string {
	if conv.ProjectID != nil && *conv.ProjectID != "" {
		return *conv.ProjectID
	}
	if callerAgentProjectID != "" {
		return callerAgentProjectID
	}
	shared := ""
	for _, a := range agents {
		if a.ProjectID == "" {
			return ""
		}
		if shared == "" {
			shared = a.ProjectID
		} else if shared != a.ProjectID {
			return ""
		}
	}
	return shared
}

// conversationParticipantViews attaches message modes to participants
// with one batched agent lookup. It is display data only: a failed lookup
// is logged at debug and leaves every mode empty, never failing the GET.
func (s *Server) conversationParticipantViews(ctx context.Context, conv *store.Conversation, participants []store.ConversationParticipant) []conversationParticipantView {
	views := make([]conversationParticipantView, len(participants))
	var agentIDs []string
	for i, p := range participants {
		views[i] = conversationParticipantView{ConversationParticipant: p}
		if p.PrincipalKind == "agent" && p.PrincipalID != "" {
			agentIDs = append(agentIDs, p.PrincipalID)
		}
	}
	if len(agentIDs) == 0 {
		return views
	}

	agents, err := s.store.GetAgentsByIDs(ctx, agentIDs)
	if err != nil {
		slog.DebugContext(ctx, "conversation get: participant message mode lookup failed",
			"conversation_id", conv.ID, "error", err)
		return views
	}

	callerProject := ""
	if agentIdent := GetAgentIdentityFromContext(ctx); agentIdent != nil {
		callerProject = agentIdent.ProjectID()
	}
	project := conversationModeProject(conv, callerProject, agents)
	if project == "" {
		return views
	}

	for i := range views {
		if views[i].PrincipalKind != "agent" {
			continue
		}
		a, ok := agents[views[i].PrincipalID]
		if !ok || a == nil {
			slog.DebugContext(ctx, "conversation get: participant agent not found for message mode",
				"conversation_id", conv.ID, "agent_id", views[i].PrincipalID)
			continue
		}
		if a.ProjectID != project {
			continue
		}
		views[i].MessageMode = a.MessageMode
	}
	return views
}
