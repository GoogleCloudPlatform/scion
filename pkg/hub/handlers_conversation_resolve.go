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
	"net/http"

	"github.com/GoogleCloudPlatform/scion/pkg/messaging"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// conversationResolveResponse is the response for GET /api/v1/conversations/resolve.
type conversationResolveResponse struct {
	Exists       bool                `json:"exists"`
	Conversation *conversationResponse `json:"conversation,omitempty"`
	PeerAgent    *targetAgentInfo    `json:"peerAgent,omitempty"`
}

// handleConversationResolve handles GET /api/v1/conversations/resolve.
// Read-only canonical resolution shared by CLI. Returns exists: false for an
// authorized peer with no conversation. Never creates rows.
func (s *Server) handleConversationResolve(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		MethodNotAllowed(w, http.MethodGet)
		return
	}

	ctx := r.Context()
	identity := GetIdentityFromContext(ctx)
	if identity == nil {
		Unauthorized(w)
		return
	}

	q := r.URL.Query()
	reference := q.Get("reference")
	projectID := q.Get("project_id")

	if reference == "" {
		BadRequest(w, "'reference' query parameter is required")
		return
	}

	// Parse the reference.
	ref, err := messaging.ParseReference(reference)
	if err != nil {
		writeError(w, http.StatusBadRequest, ErrCodeInvalidRequest,
			"Invalid conversation reference", nil)
		return
	}

	switch ref.Kind {
	case messaging.RefConversation:
		// conv:<uuid> — direct lookup.
		conv, err := s.store.GetConversation(ctx, ref.Value)
		if err != nil {
			NotFound(w, "Conversation")
			return
		}

		// Verify caller is a canonical participant.
		if conv.Kind == "direct" && !isCanonicalDMParticipant(conv.ExternalRef, identity.Type(), identity.ID()) {
			NotFound(w, "Conversation")
			return
		}

		// If project_id is specified, verify the conversation matches.
		if projectID != "" && conv.ProjectID != nil && *conv.ProjectID != projectID {
			// For direct conversations, check if either endpoint is in the project.
			if conv.Kind == "direct" {
				if !s.conversationInvolvesProject(ctx, conv, projectID) {
					writeError(w, http.StatusConflict, "project_mismatch",
						"Conversation does not match the specified project", nil)
					return
				}
			} else {
				writeError(w, http.StatusConflict, "project_mismatch",
					"Conversation does not match the specified project", nil)
				return
			}
		}

		participants, _ := s.store.ListParticipants(ctx, conv.ID)
		writeJSON(w, http.StatusOK, conversationResolveResponse{
			Exists: true,
			Conversation: &conversationResponse{
				Conversation: *conv,
				Participants: participants,
			},
		})

	case messaging.RefAgent:
		// @<agent-slug> — resolve the agent, then look up the DM.
		s.resolveAgentConversation(w, r, identity, ref.Value, projectID)

	case messaging.RefThread:
		// #<thread-name> — resolve the named thread.
		s.resolveThreadConversation(w, r, identity, ref.Value, projectID)

	default:
		writeError(w, http.StatusBadRequest, ErrCodeInvalidRequest,
			"Unsupported conversation reference kind", nil)
	}
}

// resolveAgentConversation resolves an @agent reference to a DM conversation
// without creating one. Returns exists: false if no DM exists.
func (s *Server) resolveAgentConversation(
	w http.ResponseWriter, r *http.Request,
	identity Identity, agentSlug string, projectID string,
) {
	ctx := r.Context()

	// Determine the project to search in.
	searchProjectID := projectID
	if searchProjectID == "" {
		// Try to infer from caller's identity.
		if agentIdent, ok := identity.(AgentIdentity); ok {
			searchProjectID = agentIdent.ProjectID()
		}
	}

	if searchProjectID == "" {
		BadRequest(w, "Project ID is required when resolving @agent references")
		return
	}

	// Look up the target agent.
	agentResult, err := s.store.ListAgents(ctx, store.AgentFilter{
		ProjectID: searchProjectID,
	}, store.ListOptions{})
	if err != nil {
		NotFound(w, "Agent")
		return
	}

	var targetAgent *store.Agent
	for i := range agentResult.Items {
		if agentResult.Items[i].Slug == agentSlug || agentResult.Items[i].Name == agentSlug {
			targetAgent = &agentResult.Items[i]
			break
		}
	}

	if targetAgent == nil {
		// Privacy-preserving: return exists: false instead of an error.
		writeJSON(w, http.StatusOK, conversationResolveResponse{Exists: false})
		return
	}

	// Look up existing DM — do NOT create one.
	callerKind := identity.Type()
	callerID := identity.ID()

	// Compute the DM key.
	key, _, _, keyErr := messaging.DeriveConversationKey(messaging.KeyInputs{
		SenderKind:    callerKind,
		SenderID:      callerID,
		RecipientKind: "agent",
		RecipientID:   targetAgent.ID,
	})
	if keyErr != nil {
		writeJSON(w, http.StatusOK, conversationResolveResponse{
			Exists: false,
			PeerAgent: &targetAgentInfo{
				ID:        targetAgent.ID,
				Slug:      targetAgent.Slug,
				ProjectID: targetAgent.ProjectID,
			},
		})
		return
	}

	conv, err := s.store.GetConversationByExternalRef(ctx, "native", key)
	if err != nil || conv == nil {
		// No conversation exists yet — return exists: false.
		writeJSON(w, http.StatusOK, conversationResolveResponse{
			Exists: false,
			PeerAgent: &targetAgentInfo{
				ID:        targetAgent.ID,
				Slug:      targetAgent.Slug,
				ProjectID: targetAgent.ProjectID,
			},
		})
		return
	}

	participants, _ := s.store.ListParticipants(ctx, conv.ID)
	writeJSON(w, http.StatusOK, conversationResolveResponse{
		Exists: true,
		Conversation: &conversationResponse{
			Conversation: *conv,
			Participants: participants,
		},
		PeerAgent: &targetAgentInfo{
			ID:        targetAgent.ID,
			Slug:      targetAgent.Slug,
			ProjectID: targetAgent.ProjectID,
		},
	})
}

// resolveThreadConversation resolves a #thread reference to a group conversation.
func (s *Server) resolveThreadConversation(
	w http.ResponseWriter, r *http.Request,
	identity Identity, threadName string, projectID string,
) {
	ctx := r.Context()

	// List group conversations accessible to the caller.
	convs, err := s.store.GetConversationsForPrincipal(ctx, identity.Type(), identity.ID())
	if err != nil {
		NotFound(w, "Conversation")
		return
	}

	var matches []store.Conversation
	for _, conv := range convs {
		if conv.Kind != "group" {
			continue
		}
		if conv.DisplayName != threadName {
			continue
		}
		if projectID != "" && conv.ProjectID != nil && *conv.ProjectID != projectID {
			continue
		}
		matches = append(matches, conv)
	}

	switch len(matches) {
	case 0:
		writeJSON(w, http.StatusOK, conversationResolveResponse{Exists: false})
	case 1:
		participants, _ := s.store.ListParticipants(ctx, matches[0].ID)
		writeJSON(w, http.StatusOK, conversationResolveResponse{
			Exists: true,
			Conversation: &conversationResponse{
				Conversation: matches[0],
				Participants: participants,
			},
		})
	default:
		writeError(w, http.StatusConflict, "ambiguous_reference",
			"Multiple conversations match this reference", nil)
	}
}

// conversationInvolvesProject checks if a direct conversation involves an agent
// in the specified project.
func (s *Server) conversationInvolvesProject(ctx context.Context, conv *store.Conversation, projectID string) bool {
	participants, err := s.store.ListParticipants(ctx, conv.ID)
	if err != nil {
		return false
	}
	for _, p := range participants {
		if p.PrincipalKind == "agent" {
			agent, err := s.store.GetAgent(ctx, p.PrincipalID)
			if err == nil && agent != nil && agent.ProjectID == projectID {
				return true
			}
		}
	}
	return false
}
