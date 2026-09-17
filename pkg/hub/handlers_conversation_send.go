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
	"encoding/json"
	"log/slog"
	"net/http"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// conversationSendRequest is the request body for POST /api/v1/conversations/{id}/messages.
type conversationSendRequest struct {
	Msg       string `json:"msg"`
	Type      string `json:"type,omitempty"`
	Urgent    bool   `json:"urgent,omitempty"`
	Interrupt bool   `json:"interrupt,omitempty"`
}

// conversationSendResponse is the response for POST /api/v1/conversations/{id}/messages.
type conversationSendResponse struct {
	MessageID string `json:"messageId"`
	Status    string `json:"status"`
}

// handleCPMConversationSend handles POST /api/v1/conversations/{id}/messages.
// Sends a message into an existing authorized conversation.
// For two-agent DMs, derives the other endpoint and reuses normal agent delivery.
func (s *Server) handleCPMConversationSend(w http.ResponseWriter, r *http.Request, conversationID string) {
	if r.Method != http.MethodPost {
		MethodNotAllowed(w, http.MethodPost)
		return
	}

	ctx := r.Context()
	identity := GetIdentityFromContext(ctx)
	if identity == nil {
		Unauthorized(w)
		return
	}

	// Parse request body.
	var req conversationSendRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		BadRequest(w, "Invalid request body")
		return
	}

	if req.Msg == "" {
		BadRequest(w, "Message body is required")
		return
	}
	if req.Type == "" {
		req.Type = "instruction"
	}

	// Get the conversation.
	conv, err := s.store.GetConversation(ctx, conversationID)
	if err != nil {
		NotFound(w, "Conversation")
		return
	}

	// Verify caller is a canonical participant.
	if conv.Kind == "direct" && !isCanonicalDMParticipant(conv.ExternalRef, identity.Type(), identity.ID()) {
		Forbidden(w)
		return
	}

	// For direct conversations, derive the peer and send via normal delivery.
	if conv.Kind == "direct" {
		s.sendViaDirectConversation(w, r, conv, identity, &req)
		return
	}

	// Group conversation send is not yet supported for cross-project.
	writeError(w, http.StatusNotImplemented, "cross_project_groups_unsupported",
		"Sending to group conversations via this endpoint is not yet supported", nil)
}

// sendViaDirectConversation sends a message to the peer in a direct conversation.
func (s *Server) sendViaDirectConversation(
	w http.ResponseWriter, r *http.Request,
	conv *store.Conversation,
	identity Identity,
	req *conversationSendRequest,
) {
	ctx := r.Context()

	// Get participants to find the peer.
	participants, err := s.store.ListParticipants(ctx, conv.ID)
	if err != nil {
		slog.Error("handleCPMConversationSend: failed to list participants",
			"conversation_id", conv.ID, "error", err)
		writeError(w, http.StatusInternalServerError, ErrCodeInternalError,
			"Failed to resolve conversation participants", nil)
		return
	}

	// Find the peer (the participant that is not the caller).
	var peerKind, peerID string
	for _, p := range participants {
		if p.PrincipalKind != identity.Type() || p.PrincipalID != identity.ID() {
			peerKind = p.PrincipalKind
			peerID = p.PrincipalID
			break
		}
	}

	// If no peer found from participants, try parsing the external ref.
	if peerID == "" && conv.ExternalRef != "" {
		peerKind, peerID = derivePeerFromExternalRef(conv.ExternalRef, identity.Type(), identity.ID())
	}

	if peerID == "" {
		writeError(w, http.StatusBadRequest, ErrCodeInvalidRequest,
			"Cannot determine peer in this conversation", nil)
		return
	}

	// Only agent peers are supported for now.
	if peerKind != "agent" {
		writeError(w, http.StatusNotImplemented, "peer_type_unsupported",
			"Only agent-to-agent conversation sends are supported", nil)
		return
	}

	// Get the target agent.
	targetAgent, err := s.store.GetAgent(ctx, peerID)
	if err != nil {
		writeError(w, http.StatusNotFound, ErrCodeNotFound,
			"Target agent not found", nil)
		return
	}

	// Authorize the message.
	allowed, reason := s.authorizeAgentMessage(ctx, identity, targetAgent, false)
	if !allowed {
		writeError(w, http.StatusForbidden, ErrCodeMessageDenied, reason, nil)
		return
	}

	// Create and persist the message.
	msgID := generateID()
	msg := &store.Message{
		ID:             msgID,
		ProjectID:      targetAgent.ProjectID,
		Sender:         identity.Type() + ":" + identity.ID(),
		SenderID:       identity.ID(),
		Recipient:      "agent:" + targetAgent.Slug,
		RecipientID:    targetAgent.ID,
		Msg:            req.Msg,
		Type:           req.Type,
		AgentID:        targetAgent.ID,
		ConversationID: conv.ID,
		Urgent:         req.Urgent || req.Interrupt,
	}

	if err := s.store.CreateMessage(ctx, msg); err != nil {
		slog.Error("handleCPMConversationSend: failed to create message",
			"conversation_id", conv.ID, "error", err)
		writeError(w, http.StatusInternalServerError, ErrCodeInternalError,
			"Failed to create message", nil)
		return
	}

	writeJSON(w, http.StatusOK, conversationSendResponse{
		MessageID: msgID,
		Status:    "delivered",
	})
}

// derivePeerFromExternalRef extracts the peer identity from a DM external ref.
// External refs have the format "dm:kind1:id1:kind2:id2".
func derivePeerFromExternalRef(externalRef, callerKind, callerID string) (peerKind, peerID string) {
	// Parse the DM key to extract both principals.
	if len(externalRef) < 3 || externalRef[:3] != "dm:" {
		return "", ""
	}

	parts := splitDMRef(externalRef[3:])
	if len(parts) != 4 {
		return "", ""
	}

	kindA, idA, kindB, idB := parts[0], parts[1], parts[2], parts[3]

	if kindA == callerKind && idA == callerID {
		return kindB, idB
	}
	if kindB == callerKind && idB == callerID {
		return kindA, idA
	}

	return "", ""
}

// splitDMRef splits a DM ref body into its four components.
// The format is "kind1:id1:kind2:id2" where IDs may contain dashes.
func splitDMRef(body string) []string {
	// DM keys use the format "kind:uuid:kind:uuid"
	// Kinds are simple words (agent, user), UUIDs contain dashes.
	// Split into at most 4 parts by finding kind boundaries.
	var result []string
	remaining := body
	for i := 0; i < 3; i++ {
		// Find the first colon after a kind or UUID.
		idx := findKindBoundary(remaining)
		if idx < 0 {
			return nil
		}
		result = append(result, remaining[:idx])
		remaining = remaining[idx+1:]
	}
	if remaining != "" {
		result = append(result, remaining)
	}
	return result
}

// findKindBoundary finds the index of the colon separating a kind/id from the
// next component in a DM key. Kinds are short words (agent, user); UUIDs are
// 36-char dash-separated hex strings.
func findKindBoundary(s string) int {
	// For kinds: look for "agent:" or "user:" at the start.
	for _, kind := range []string{"agent", "user"} {
		if len(s) > len(kind) && s[:len(kind)] == kind && s[len(kind)] == ':' {
			return len(kind)
		}
	}
	// For UUIDs (xxxxxxxx-xxxx-xxxx-xxxx-xxxxxxxxxxxx): find the colon after 36 chars.
	if len(s) > 36 && s[36] == ':' {
		return 36
	}
	// Fallback: find first colon.
	for i, c := range s {
		if c == ':' {
			return i
		}
	}
	return -1
}
