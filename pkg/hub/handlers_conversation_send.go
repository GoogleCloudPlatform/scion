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
	"encoding/json"
	"log/slog"
	"net/http"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/messages"
	"github.com/GoogleCloudPlatform/scion/pkg/messaging"
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

	// Derive peer from canonical DM key (immutable ExternalRef).
	// Participant rows are mutable and must not redirect delivery;
	// a forged third-participant row could otherwise divert a reply.
	// No participant-row fallback: canonical DM handler is fail-closed.
	peerKind, peerID := derivePeerFromExternalRef(conv.ExternalRef, identity.Type(), identity.ID())

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
	allowed, reason, _ := s.authorizeAgentMessage(ctx, identity, targetAgent, false)
	if !allowed {
		writeError(w, http.StatusForbidden, ErrCodeMessageDenied, reason, nil)
		return
	}

	// Build sender identity. For agents, use the slug (not UUID) to match
	// the "agent:<slug>" format used by the normal message pipeline.
	senderLabel := identity.Type() + ":" + identity.ID()
	if agentIdent := GetAgentIdentityFromContext(ctx); agentIdent != nil {
		callerAgent, lookupErr := s.store.GetAgent(ctx, agentIdent.ID())
		if lookupErr == nil && callerAgent != nil {
			senderLabel = "agent:" + callerAgent.Slug
		}
	}

	// Create and persist the message.
	msgID := generateID()
	msg := &store.Message{
		ID:             msgID,
		ProjectID:      targetAgent.ProjectID,
		Sender:         senderLabel,
		SenderID:       identity.ID(),
		Recipient:      "agent:" + targetAgent.Slug,
		RecipientID:    targetAgent.ID,
		Msg:            req.Msg,
		Type:           req.Type,
		AgentID:        targetAgent.ID,
		ConversationID: conv.ID,
		Urgent:         req.Urgent || req.Interrupt,
		DispatchState:  store.MessageDispatchPending,
	}

	if err := s.store.CreateMessage(ctx, msg); err != nil {
		slog.Error("handleCPMConversationSend: failed to create message",
			"conversation_id", conv.ID, "error", err)
		writeError(w, http.StatusInternalServerError, ErrCodeInternalError,
			"Failed to create message", nil)
		return
	}

	// Dispatch to the target agent's broker for actual delivery.
	dispatcher := s.GetDispatcher()
	if dispatcher == nil {
		// Message persisted but dispatcher unavailable — report honestly.
		writeJSON(w, http.StatusOK, conversationSendResponse{
			MessageID: msgID,
			Status:    "pending",
		})
		return
	}

	if targetAgent.RuntimeBrokerID == "" {
		// Agent has no broker yet — message is stored, delivery deferred.
		writeJSON(w, http.StatusOK, conversationSendResponse{
			MessageID: msgID,
			Status:    "pending",
		})
		return
	}

	// Build a structured message for the dispatcher. Include canonical
	// conversation context so the harness can correlate replies.
	structuredMsg := &messages.StructuredMessage{
		Type:           req.Type,
		Sender:         senderLabel,
		SenderID:       identity.ID(),
		Recipient:      msg.Recipient,
		RecipientID:    targetAgent.ID,
		Msg:            req.Msg,
		ConversationID: conv.ID,
	}

	// Render the agent-facing envelope using the same conventions as
	// the normal messaging pipeline (Phase 9b). This ensures the
	// harness receives canonical conversation context in the rendered
	// envelope, not just a populated in-memory ConversationID field.
	structuredMsg.DeliveryText = messaging.RenderDeliveryText(messaging.RenderDeliveryInput{
		MessageID: msgID,
		ConvResult: &messaging.ConversationResult{
			ConversationID: conv.ID,
			ExternalRef:    conv.ExternalRef,
			Kind:           conv.Kind,
			Surface:        conv.Surface,
			DisplayName:    conv.DisplayName,
		},
		Msg:       structuredMsg,
		CreatedAt: time.Now().UTC(),
	})

	retryCtx, retryCancel := context.WithTimeout(ctx, 30*time.Second)
	defer retryCancel()

	if err := dispatchWithBrokerRetry(retryCtx, dispatcher, targetAgent, req.Msg, req.Urgent || req.Interrupt, structuredMsg); err != nil {
		if markErr := s.store.MarkMessageFailed(ctx, msgID, err.Error()); markErr != nil {
			slog.Error("handleCPMConversationSend: failed to mark message as failed",
				"message_id", msgID, "error", markErr)
		}
		writeError(w, http.StatusBadGateway, "DISPATCH_FAILED",
			"Message stored but delivery to agent failed: "+err.Error(), nil)
		return
	}

	// CAS-flip pending→dispatched now that the broker accepted the message.
	if _, markErr := s.store.MarkMessageDispatched(ctx, msgID); markErr != nil {
		slog.Error("handleCPMConversationSend: failed to mark message dispatched",
			"message_id", msgID, "error", markErr)
	}

	writeJSON(w, http.StatusOK, conversationSendResponse{
		MessageID: msgID,
		Status:    "delivered",
	})
}

// derivePeerFromExternalRef extracts the peer identity from a DM external ref.
// External refs have the format "dm:kind1:id1:kind2:id2".
func derivePeerFromExternalRef(externalRef, callerKind, callerID string) (peerKind, peerID string) {
	kindA, idA, kindB, idB, err := messages.ParseDMKey(externalRef)
	if err != nil {
		return "", ""
	}
	if kindA == callerKind && idA == callerID {
		return kindB, idB
	}
	if kindB == callerKind && idB == callerID {
		return kindA, idA
	}
	return "", ""
}
