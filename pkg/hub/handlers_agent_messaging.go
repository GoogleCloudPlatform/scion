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
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/hub/githubapp"
	"github.com/GoogleCloudPlatform/scion/pkg/messages"
	"github.com/GoogleCloudPlatform/scion/pkg/messaging"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// OutboundMessageRequest is the request body for POST /api/v1/agents/{id}/outbound-message.
type OutboundMessageRequest struct {
	Recipient   string            `json:"recipient,omitempty"`
	RecipientID string            `json:"recipient_id,omitempty"`
	Msg         string            `json:"msg"`
	Type        string            `json:"type,omitempty"`
	Urgent      bool              `json:"urgent,omitempty"`
	Attachments []string          `json:"attachments,omitempty"`
	Channel     string            `json:"channel,omitempty"`
	ThreadID    string            `json:"thread_id,omitempty"`
	Metadata    map[string]string `json:"metadata,omitempty"`
	// Visibility controls which consumers see this message.
	// One of "normal", "verbose", "full". Empty defaults to "normal".
	Visibility string `json:"visibility,omitempty"`
}

// handleAgentOutboundMessage handles POST /api/v1/agents/{id}/outbound-message.
// Agents use this to send messages to human inboxes. Authenticated via agent
// token (self-access only). The recipient defaults to the agent's creator when
// not explicitly specified.
func (s *Server) handleAgentOutboundMessage(w http.ResponseWriter, r *http.Request, id string) {
	ctx := r.Context()

	agentIdent := GetAgentIdentityFromContext(ctx)
	if agentIdent == nil {
		Unauthorized(w)
		return
	}
	if agentIdent.ID() != id {
		writeError(w, http.StatusForbidden, ErrCodeForbidden, "Agents can only send outbound messages as themselves", nil)
		return
	}

	var req OutboundMessageRequest
	if err := readJSON(r, &req); err != nil {
		BadRequest(w, "Invalid request body: "+err.Error())
		return
	}

	if req.Type == "" {
		req.Type = "input-needed"
	}

	// Per-sender send limit (#1054). This is the path a looping agent floods a
	// thread through, so the limit has to live here and not only on the
	// browser send path. The response is an explicit 429 with Retry-After
	// rather than a silent drop, so a caller can back off and resend.
	//
	// The traffic class is derived from the (now defaulted) message type so the
	// automatic assistant-reply transcript mirror — posted by the agent hook,
	// not written by the agent — cannot spend the whole allowance the agent
	// needs for a completion report or an escalation. The class only ever
	// selects a reservation inside the agent's single aggregate ceiling, so a
	// caller cannot buy extra allowance by relabelling its traffic. A type
	// this build does not recognise is classified as ordinary agent traffic
	// and still accepted, exactly as before: tightening the type contract on
	// the wire is a compatibility change and is tracked separately.
	//
	// Charged before the payload is validated: a flood of malformed sends is
	// still a flood.
	if !s.allowChatSend(w, agentIdent.ID(), chatSenderClassForMessageType(req.Type)) {
		return
	}

	if req.Msg == "" {
		ValidationError(w, "msg is required", nil)
		return
	}
	if msgLen := utf8.RuneCountInString(req.Msg); msgLen > messages.MaxMessageLength {
		ValidationError(w, fmt.Sprintf("message exceeds %d character limit (current: %d chars). Consider splitting into multiple messages using multiple scion message invocations", messages.MaxMessageLength, msgLen), nil)
		return
	}

	// Validate and default visibility.
	switch req.Visibility {
	case "":
		req.Visibility = messages.VisibilityNormal
	case messages.VisibilityNormal, messages.VisibilityVerbose, messages.VisibilityFull:
		// valid
	default:
		ValidationError(w, fmt.Sprintf("invalid visibility %q; must be one of: normal, verbose, full", req.Visibility), nil)
		return
	}

	// Validate DM key format when the thread_id looks like a DM key.
	// Non-DM thread IDs (topic UUIDs, etc.) pass through as-is.
	if req.ThreadID != "" && strings.HasPrefix(req.ThreadID, "dm:") && !validDMKey(req.ThreadID) {
		BadRequest(w, "invalid DM key format")
		return
	}

	agent, err := s.store.GetAgent(ctx, id)
	if err != nil {
		writeErrorFromErr(w, err, "")
		return
	}

	// Resolve recipient: explicit takes precedence; implicit defaults to agent creator.
	recipientID := req.RecipientID
	recipient := req.Recipient

	if recipientID == "" && recipient != "" {
		// Explicit recipient string provided without an ID — resolve the user.
		// Accept "user:<identifier>" or bare "<identifier>".
		identifier := strings.TrimPrefix(recipient, "user:")

		// Try email lookup first (identifier contains @).
		if strings.Contains(identifier, "@") {
			if u, err := s.store.GetUserByEmail(ctx, identifier); err == nil {
				recipientID = u.ID
				name := u.DisplayName
				if name == "" {
					name = u.Email
				}
				recipient = "user:" + name
			}
		}

		// Fall back to display-name search if email lookup didn't match.
		if recipientID == "" {
			result, err := s.store.ListUsers(ctx, store.UserFilter{Search: identifier}, store.ListOptions{Limit: 1})
			if err == nil && len(result.Items) == 1 {
				u := result.Items[0]
				recipientID = u.ID
				name := u.DisplayName
				if name == "" {
					name = u.Email
				}
				recipient = "user:" + name
			}
		}

		if recipientID == "" {
			ValidationError(w, fmt.Sprintf("recipient %q could not be resolved to a known user", req.Recipient), nil)
			return
		}
	}

	if recipientID == "" && recipient == "" {
		ValidationError(w, "recipient is required — specify a user with 'user:<name>' or 'user:<email>'", nil)
		return
	}

	// Ownership check: verify the DM key IDs match the actual sender (agent)
	// and recipient (user). The early format check above rejects malformed keys;
	// this catches well-formed keys with wrong participant IDs.
	if req.ThreadID != "" && strings.HasPrefix(req.ThreadID, "dm:") {
		dmAgentID, dmUserID := parseDMKeyIDs(req.ThreadID)
		if dmAgentID != agent.ID || dmUserID != recipientID {
			BadRequest(w, "DM thread_id does not match the sender and recipient")
			return
		}
	}

	// Reply affinity (Phase 6, AC22): when the agent sends an untagged reply
	// (no explicit channel), check webchat_conversation_context for the
	// (recipient, project, agent) triple. If a row exists, route to the
	// channel the user last spoke from. If no row exists, leave channel
	// empty so the message fans out to all spokes (today's default behavior).
	if req.Channel == "" && recipientID != "" && s.webChatStore != nil && s.GetMessageBrokerProxy() != nil {
		if lastCh, err := s.webChatStore.GetLastChannel(ctx, recipientID, agent.ProjectID, agent.ID); err != nil {
			s.messageLog.Error("Failed to look up reply affinity",
				"recipient_id", recipientID, "agent_id", agent.ID, "error", err)
			// Non-fatal: fall through to fan-out-to-all behavior.
		} else if lastCh != "" {
			req.Channel = lastCh
		}
	}

	// Validate channel against registered channels.
	// Fail closed: if broker proxy is unavailable, reject the message rather than
	// silently skipping validation.
	if req.Channel != "" {
		bp := s.GetMessageBrokerProxy()
		if bp == nil {
			writeError(w, http.StatusServiceUnavailable, "broker_unavailable",
				"cannot validate channel: message broker is not available", nil)
			return
		}
		channels := bp.ListChannels()
		found := false
		for _, ch := range channels {
			if ch.Name == req.Channel {
				found = true
				break
			}
		}
		if !found {
			available := make([]string, len(channels))
			for i, ch := range channels {
				available[i] = ch.Name
			}
			if len(available) == 0 {
				ValidationError(w, fmt.Sprintf("channel %q is not registered; no channels are currently available", req.Channel), nil)
			} else {
				ValidationError(w, fmt.Sprintf("channel %q is not registered; available channels: %s", req.Channel, strings.Join(available, ", ")), nil)
			}
			return
		}
	}

	storeMsg := &store.Message{
		ID:          api.NewUUID(),
		ProjectID:   agent.ProjectID,
		Sender:      "agent:" + agent.Slug,
		SenderID:    agent.ID,
		Recipient:   recipient,
		RecipientID: recipientID,
		Msg:         req.Msg,
		Type:        req.Type,
		Urgent:      req.Urgent,
		AgentID:     agent.ID,
		Channel:     req.Channel,
		ThreadID:    req.ThreadID,
		Visibility:  req.Visibility,
		CreatedAt:   time.Now(),
	}

	// Phase 5 dual-write: resolve-or-create conversation, stamp conversation_id.
	var convResult *messaging.ConversationResult
	if req.ThreadID != "" {
		convResult = messaging.ResolveOrCreateThreadConversation(ctx, s.store, s.messageLog, req.ThreadID, agent.ProjectID)
	} else if agent.ID != "" && recipientID != "" {
		convResult = messaging.ResolveOrCreateDMConversation(ctx, s.store, s.store, s.messageLog, "agent", agent.ID, "user", recipientID)
	}
	if convResult != nil {
		storeMsg.ConversationID = convResult.ConversationID
	}
	// Always log divergence — even when convResult is nil, that is a divergence signal.
	oldRouting := messaging.OldRoutingFromMessage(agent.ID, recipientID, req.ThreadID)
	convID := ""
	actualRef := ""
	if convResult != nil {
		convID = convResult.ConversationID
		actualRef = convResult.ExternalRef
	}
	match, reason := messaging.ComputeDivergenceMatch(oldRouting, actualRef, convID)
	messaging.LogDivergence(s.messageLog, messaging.DivergenceEntry{
		MessageID:  storeMsg.ID,
		OldRouting: oldRouting,
		NewRouting: messaging.NewRoutingStr(convID),
		Match:      match,
		Reason:     reason,
	})

	// Build a structured message for external dispatch paths.
	structuredMsg := &messages.StructuredMessage{
		Sender:      storeMsg.Sender,
		SenderID:    storeMsg.SenderID,
		Recipient:   storeMsg.Recipient,
		RecipientID: storeMsg.RecipientID,
		Msg:         storeMsg.Msg,
		Type:        storeMsg.Type,
		Urgent:      storeMsg.Urgent,
		Attachments: req.Attachments,
		Channel:     req.Channel,
		ThreadID:    req.ThreadID,
		Visibility:  req.Visibility,
		Metadata:    req.Metadata,
	}
	// Propagate recipients and group_id from metadata for group-set messages.
	if req.Metadata != nil {
		if r, ok := req.Metadata["recipients"]; ok {
			structuredMsg.Recipients = r
		}
		if gid, ok := req.Metadata["group_id"]; ok {
			storeMsg.GroupID = gid
		}
	}

	// W7: Record the attached files as chat attachments so they render in web
	// chat. The refs ride along in the message metadata because the linkage row
	// needs a message ID, which only exists once the message is persisted —
	// below on the direct path, or in the broker's deliverToUser.
	attachmentRefs := s.ingestAgentAttachments(ctx, agent.ProjectID, agent.ID, req.Attachments)
	if encoded, ok := attachmentRefsMetadata(attachmentRefs); ok {
		if structuredMsg.Metadata == nil {
			structuredMsg.Metadata = make(map[string]string, 1)
		}
		structuredMsg.Metadata[attachmentsMetadataKey] = encoded
	}

	// Route through broker when available; otherwise persist and publish
	// directly. The broker's deliverToUser callback handles persistence
	// and SSE, so doing both here would create duplicate messages.
	if bp := s.GetMessageBrokerProxy(); bp != nil {
		if err := bp.PublishUserMessage(ctx, agent.ProjectID, recipientID, structuredMsg); err != nil {
			s.messageLog.Error("Failed to dispatch outbound message through broker",
				"agent_id", agent.ID, "recipient_id", recipientID, "error", err)
			writeError(w, http.StatusBadGateway, ErrCodeDeliveryFailed,
				"Message delivery failed: "+err.Error(), nil)
			return
		}
		s.messageLog.Info("Outbound message dispatched through broker",
			"agent_id", agent.ID, "recipient_id", recipientID, "project_id", agent.ProjectID)
	} else {
		if err := s.store.CreateMessage(ctx, storeMsg); err != nil {
			s.messageLog.Error("Failed to persist outbound message", "error", err)
			writeError(w, http.StatusInternalServerError, ErrCodeInternalError,
				"Failed to persist message", nil)
			return
		}
		// W7: Link before publishing so a client that refetches on the SSE
		// event already sees the attachments.
		s.mu.RLock()
		wcs := s.webChatStore
		s.mu.RUnlock()
		linkAttachmentRefs(ctx, wcs, storeMsg.ID, attachmentRefs, s.messageLog)
		s.events.PublishUserMessage(ctx, storeMsg)
		if s.channelRegistry != nil && s.channelRegistry.Len() > 0 {
			s.channelRegistry.Dispatch(ctx, structuredMsg)
		}
	}

	// W6: DM notification for agent → human replies (non-broker path only).
	// The broker path fires notifications from deliverToUser in messagebroker.go.
	if bp := s.GetMessageBrokerProxy(); bp == nil {
		if cn := s.getChatNotifier(); cn != nil && req.ThreadID != "" && strings.HasPrefix(req.ThreadID, "dm:") && recipientID != "" {
			senderName := agent.Name
			if senderName == "" {
				senderName = agent.Slug
			}
			go cn.NotifyDMReceived(context.Background(), recipientID, ChatMessageContext{
				SenderID:        agent.ID,
				SenderName:      senderName,
				ConversationKey: req.ThreadID,
				Preview:         req.Msg,
				ProjectID:       agent.ProjectID,
			})
		}
	}

	s.logMessage("outbound message sent",
		"agent_id", agent.ID,
		"agent_name", agent.Name,
		"project_id", agent.ProjectID,
		"recipient_id", recipientID,
		"msg_type", req.Type,
	)

	writeJSON(w, http.StatusOK, map[string]interface{}{
		"message_id":   storeMsg.ID,
		"status":       "sent",
		"recipient":    recipient,
		"recipient_id": recipientID,
	})
}

// handleAgentGitHubTokenRefresh handles POST /api/v1/agents/{id}/refresh-token.
// An agent can request a fresh GitHub App installation token when its current
// token is nearing expiry. This is a self-access operation: the agent must
// present a valid Hub auth token whose subject matches the target agent ID.
func (s *Server) handleAgentGitHubTokenRefresh(w http.ResponseWriter, r *http.Request, id string) {
	agentIdent := GetAgentIdentityFromContext(r.Context())
	if agentIdent == nil {
		writeError(w, http.StatusUnauthorized, ErrCodeUnauthorized,
			"agent authentication required for GitHub token refresh", nil)
		return
	}

	// Enforce self-access: agents can only refresh their own GitHub token
	if agentIdent.ID() != id {
		writeError(w, http.StatusForbidden, ErrCodeForbidden,
			"agents can only refresh their own GitHub token", nil)
		return
	}

	// Require the token refresh scope
	if !agentIdent.HasScope(ScopeAgentTokenRefresh) {
		writeError(w, http.StatusForbidden, ErrCodeForbidden,
			"missing required scope: agent:token:refresh", nil)
		return
	}

	ctx := r.Context()

	// Look up the agent to get its project
	agent, err := s.store.GetAgent(ctx, id)
	if err != nil {
		writeErrorFromErr(w, err, "")
		return
	}

	if agent.ProjectID == "" {
		writeError(w, http.StatusBadRequest, ErrCodeInvalidRequest,
			"agent has no project associated", nil)
		return
	}

	project, err := s.store.GetProject(ctx, agent.ProjectID)
	if err != nil {
		writeErrorFromErr(w, err, "")
		return
	}

	if project.GitHubInstallationID == nil {
		writeError(w, http.StatusBadRequest, ErrCodeInvalidRequest,
			"project has no GitHub App installation", nil)
		return
	}

	token, expiry, err := s.MintGitHubAppTokenForProject(ctx, project)
	if err != nil {
		// Classify the error to return an appropriate status code.
		// Configuration errors (bad key, wrong app_id) are 502 (upstream auth failed),
		// not 500 (our server is broken).
		statusCode := http.StatusBadGateway
		errCode := ErrCodeRuntimeError
		if mintErr, ok := err.(*githubapp.TokenMintError); ok {
			switch mintErr.ErrorCode {
			case githubapp.ErrCodePrivateKeyInvalid, githubapp.ErrCodeAppNotFound:
				statusCode = http.StatusBadGateway
				errCode = ErrCodeRuntimeError
			case githubapp.ErrCodeInstallationRevoked, githubapp.ErrCodeInstallationSuspended:
				statusCode = http.StatusUnprocessableEntity
				errCode = ErrCodeUnprocessable
			case githubapp.ErrCodePermissionDenied, githubapp.ErrCodeRepoNotAccessible:
				statusCode = http.StatusForbidden
				errCode = ErrCodeForbidden
			}
		}
		writeError(w, statusCode, errCode,
			"failed to mint GitHub token: "+err.Error(), nil)
		return
	}

	if token == "" {
		writeError(w, http.StatusServiceUnavailable, ErrCodeUnavailable,
			"GitHub App not configured on Hub", nil)
		return
	}

	writeJSON(w, http.StatusOK, map[string]interface{}{
		"token":      token,
		"expires_at": expiry,
	})
}

// restoreAgent restores a soft-deleted agent.
func (s *Server) restoreAgent(w http.ResponseWriter, r *http.Request, id string) {
	ctx := r.Context()

	agent, err := s.store.GetAgent(ctx, id)
	if err != nil {
		writeErrorFromErr(w, err, "")
		return
	}

	if agent.DeletedAt.IsZero() {
		BadRequest(w, "Agent is not in deleted state")
		return
	}

	agent.DeletedAt = time.Time{}
	agent.Updated = time.Now()

	if err := s.store.UpdateAgent(ctx, agent); err != nil {
		writeErrorFromErr(w, err, "")
		return
	}

	s.events.PublishAgentCreated(ctx, agent)

	writeJSON(w, http.StatusOK, agent.ToAPI())
}

// MessageRequest is the request body for sending a message to an agent.
type MessageRequest struct {
	// Plain text message (legacy field, used for backwards compatibility).
	Message string `json:"message,omitempty"`

	// Structured message (new field, used by default).
	StructuredMessage *messages.StructuredMessage `json:"structured_message,omitempty"`

	// Interrupt the harness before sending.
	Interrupt bool `json:"interrupt,omitempty"`

	// Notify subscribes the sender to status notifications for this agent
	// (COMPLETED, WAITING_FOR_INPUT, LIMITS_EXCEEDED, STALLED, ERROR).
	Notify bool `json:"notify,omitempty"`

	// Wake resumes a suspended agent before delivering the message.
	Wake bool `json:"wake,omitempty"`

	// Mentions lists agent slugs to receive mention notifications (max 10).
	// The primary recipient is automatically excluded from mention fan-out.
	Mentions []string `json:"mentions,omitempty"`
}

func (s *Server) handleAgentMessage(w http.ResponseWriter, r *http.Request, id string) {
	ctx := r.Context()

	var req MessageRequest
	if err := readJSON(r, &req); err != nil {
		BadRequest(w, "Invalid request body: "+err.Error())
		return
	}

	// Determine the message content and structured message to forward
	var plainMessage string
	var structuredMsg *messages.StructuredMessage

	if req.StructuredMessage != nil {
		structuredMsg = req.StructuredMessage
		plainMessage = req.StructuredMessage.Msg
		// B5 SECURITY FIX: ALWAYS derive sender identity from the
		// authenticated context. Client-supplied Sender and SenderID are
		// untrusted inputs that must never be used as conversation key
		// inputs — the DM key IS the access authority for direct
		// conversations and there is no second check to catch a wrong one.
		//
		// This also fixes the downstream broker path: the broker receives
		// the published message and inherits these (now auth-derived) fields.
		structuredMsg.Sender = "user:unknown"
		structuredMsg.SenderID = ""
		if user := GetUserIdentityFromContext(ctx); user != nil {
			structuredMsg.SenderID = user.ID()
			if name := user.DisplayName(); name != "" {
				structuredMsg.Sender = "user:" + name
			} else if email := user.Email(); email != "" {
				structuredMsg.Sender = "user:" + email
			}
		} else if agentIdent := GetAgentIdentityFromContext(ctx); agentIdent != nil {
			structuredMsg.SenderID = agentIdent.ID()
			structuredMsg.Sender = "agent:" + agentIdent.ID()
		}
		// Default version, timestamp and type when the client omits them
		// (e.g. the web UI sends a minimal structured_message).
		if structuredMsg.Version == 0 {
			structuredMsg.Version = messages.Version
		}
		if structuredMsg.Timestamp == "" {
			structuredMsg.Timestamp = time.Now().UTC().Format(time.RFC3339)
		}
		if structuredMsg.Type == "" {
			structuredMsg.Type = messages.TypeInstruction
		}
	} else if req.Message != "" {
		plainMessage = req.Message
		// Build a structured message from the plain text so that downstream
		// logging and the broker receive a fully-populated payload.
		sender := "user:unknown"
		senderID := ""
		if user := GetUserIdentityFromContext(ctx); user != nil {
			senderID = user.ID()
			if name := user.DisplayName(); name != "" {
				sender = "user:" + name
			} else if email := user.Email(); email != "" {
				sender = "user:" + email
			}
		}
		structuredMsg = messages.NewInstruction(sender, "agent:"+id, plainMessage)
		structuredMsg.SenderID = senderID
	} else {
		ValidationError(w, "message or structured_message is required", nil)
		return
	}

	// Validate DM key format when the thread_id looks like a DM key.
	if structuredMsg != nil && structuredMsg.ThreadID != "" &&
		strings.HasPrefix(structuredMsg.ThreadID, "dm:") && !validDMKey(structuredMsg.ThreadID) {
		BadRequest(w, "invalid DM key format")
		return
	}

	// Cap mentions to avoid oversized responses and wasted server resources (R1).
	if len(req.Mentions) > messages.MaxMentionRecipients {
		req.Mentions = req.Mentions[:messages.MaxMentionRecipients]
	}

	// Detect group[] recipient for multi-target fan-out.
	if structuredMsg != nil && messages.IsGroupRecipient(structuredMsg.Recipient) {
		s.handleGroupMessage(w, r, id, structuredMsg, plainMessage, req.Interrupt)
		return
	}

	agent, err := s.store.GetAgent(ctx, id)
	if err != nil {
		writeErrorFromErr(w, err, "")
		return
	}

	// Ownership check: verify the DM key IDs match the actual participants.
	// The agent in the DM key must match the target agent; the user must match
	// the AUTHENTICATED identity (not the client-supplied SenderID, which can
	// be spoofed).
	if structuredMsg != nil && structuredMsg.ThreadID != "" &&
		strings.HasPrefix(structuredMsg.ThreadID, "dm:") {
		dmAgentID, dmUserID := parseDMKeyIDs(structuredMsg.ThreadID)
		var authenticatedUserID string
		if user := GetUserIdentityFromContext(ctx); user != nil {
			authenticatedUserID = user.ID()
		} else if agentIdent := GetAgentIdentityFromContext(ctx); agentIdent != nil {
			authenticatedUserID = agentIdent.ID()
		}
		if dmAgentID != agent.ID || dmUserID != authenticatedUserID {
			BadRequest(w, "DM thread_id does not match the sender and recipient")
			return
		}
	}

	// Wake handling: if requested, resume a suspended agent before message delivery.
	if req.Wake {
		switch state.Phase(agent.Phase) {
		case state.PhaseSuspended:
			if !s.checkBrokerAvailability(w, r, agent) {
				return
			}
			dispatcher := s.GetDispatcher()
			if dispatcher == nil {
				ServiceNotReady(w, "Dispatch not available — server may still be starting up")
				return
			}
			if agent.RuntimeBrokerID == "" {
				ServiceNotReady(w, "Agent has no runtime broker assigned")
				return
			}

			// Wake always resumes a suspended agent, so the harness must
			// continue its prior session.
			if err := dispatcher.DispatchAgentStart(ctx, agent, "", true); err != nil {
				RuntimeError(w, "Failed to wake agent: "+err.Error())
				return
			}

			// Set phase to 'starting' while we wait for readiness.
			statusUpdate := store.AgentStatusUpdate{Phase: string(state.PhaseStarting)}
			if err := s.store.UpdateAgentStatus(ctx, id, statusUpdate); err != nil {
				writeErrorFromErr(w, err, "")
				return
			}
			agent.Phase = string(state.PhaseStarting)
			s.events.PublishAgentStatus(ctx, agent)

			if err := s.waitForAgentReady(ctx, id, 30*time.Second); err != nil {
				// On failure, set agent to an error state for clarity.
				_ = s.store.UpdateAgentStatus(ctx, id, store.AgentStatusUpdate{Phase: string(state.PhaseError), Message: "Failed to become ready after wake"})
				RuntimeError(w, "Agent resumed but did not become ready: "+err.Error())
				return
			}

			// Agent is ready, set phase to 'running'.
			statusUpdate = store.AgentStatusUpdate{Phase: string(state.PhaseRunning)}
			if err := s.store.UpdateAgentStatus(ctx, id, statusUpdate); err != nil {
				writeErrorFromErr(w, err, "")
				return
			}
			agent.Phase = string(state.PhaseRunning)
			s.events.PublishAgentStatus(ctx, agent)

		case state.PhaseRunning:
			// no-op

		case state.PhaseStopped:
			writeError(w, http.StatusBadRequest, ErrCodeValidationError,
				"Agent is stopped, not suspended — use 'scion resume' to restart it with its previous state", nil)
			return

		case state.PhaseError:
			writeError(w, http.StatusBadRequest, ErrCodeValidationError,
				"Agent is in error state — use 'scion resume' to restart", nil)
			return

		default:
			writeError(w, http.StatusBadRequest, ErrCodeValidationError,
				fmt.Sprintf("Agent is not yet running (phase: %s) — wait for it to reach running state", agent.Phase), nil)
			return
		}
	}

	// Reject messages to non-running agents when --wake is not set.
	if !req.Wake {
		switch state.Phase(agent.Phase) {
		case state.PhaseRunning:
			// OK — proceed to deliver
		case state.PhaseSuspended:
			writeError(w, http.StatusConflict, ErrCodeAgentNotRunning,
				fmt.Sprintf("Agent %q is suspended. Use --wake to resume and deliver.", agent.Slug), nil)
			return
		case state.PhaseStopped:
			writeError(w, http.StatusConflict, ErrCodeAgentNotRunning,
				fmt.Sprintf("Agent %q is stopped. Use 'scion resume' to restart it with its previous state.", agent.Slug), nil)
			return
		case state.PhaseError:
			writeError(w, http.StatusConflict, ErrCodeAgentNotRunning,
				fmt.Sprintf("Agent %q is in error state. Use 'scion resume' to restart.", agent.Slug), nil)
			return
		default:
			writeError(w, http.StatusConflict, ErrCodeAgentNotRunning,
				fmt.Sprintf("Agent %q is not yet running (phase: %s). Wait for it to reach running state.", agent.Slug, agent.Phase), nil)
			return
		}
	}

	// Populate recipient slug and ID from the resolved agent.
	structuredMsg.Recipient = "agent:" + agent.Slug
	structuredMsg.RecipientID = agent.ID

	// Default the channel to "web" for messages sent through the web UI.
	// Only tag as "web" when the authenticated user's client type is
	// actually "web" — CLI and API callers should not be tagged.
	if structuredMsg.Channel == "" {
		if user := GetUserIdentityFromContext(ctx); user != nil {
			if au, ok := user.(*AuthenticatedUser); ok && au.ClientType() == "web" {
				structuredMsg.Channel = "web"
			}
		}
	}

	if !s.checkBrokerAvailability(w, r, agent) {
		return
	}

	// Log the message dispatch to dedicated message log
	logAttrs := []any{
		"agent_id", agent.ID,
		"agent_name", agent.Name,
		"project_id", agent.ProjectID,
	}
	if structuredMsg != nil {
		logAttrs = append(logAttrs, structuredMsg.LogAttrs()...)
	}
	s.logMessage("message dispatched", logAttrs...)

	// Persist to message store before delivery attempt. Set dispatch_state
	// to "dispatched" (no new pending rows per delivery policy).
	var persistedMsgID string
	if structuredMsg != nil {
		storeMsg := &store.Message{
			ID:            api.NewUUID(),
			ProjectID:     agent.ProjectID,
			Sender:        structuredMsg.Sender,
			SenderID:      structuredMsg.SenderID,
			Recipient:     structuredMsg.Recipient,
			RecipientID:   structuredMsg.RecipientID,
			Msg:           structuredMsg.Msg,
			Type:          structuredMsg.Type,
			Urgent:        structuredMsg.Urgent,
			Broadcasted:   structuredMsg.Broadcasted,
			AgentID:       agent.ID,
			Channel:       structuredMsg.Channel,
			ThreadID:      structuredMsg.ThreadID,
			DispatchState: store.MessageDispatchDispatched,
			CreatedAt:     time.Now(),
		}
		// Phase 5 dual-write: resolve-or-create conversation for user/agent → agent messages.
		var convResult *messaging.ConversationResult
		if structuredMsg.ThreadID != "" {
			convResult = messaging.ResolveOrCreateThreadConversation(ctx, s.store, s.messageLog, structuredMsg.ThreadID, agent.ProjectID)
		} else if agent.ID != "" {
			// B5 SECURITY: derive sender identity for the DM key from the
			// authenticated context, never from the message payload.
			if authKind, authID := authenticatedSender(ctx); authID != "" {
				convResult = messaging.ResolveOrCreateDMConversation(ctx, s.store, s.store, s.messageLog, authKind, authID, "agent", agent.ID)
			}
		}
		if convResult != nil {
			storeMsg.ConversationID = convResult.ConversationID
		}
		// Always log divergence — even when convResult is nil, that is a divergence signal.
		oldRouting := messaging.OldRoutingFromMessage(structuredMsg.SenderID, agent.ID, structuredMsg.ThreadID)
		convID := ""
		actualRef := ""
		if convResult != nil {
			convID = convResult.ConversationID
			actualRef = convResult.ExternalRef
		}
		match, reason := messaging.ComputeDivergenceMatch(oldRouting, actualRef, convID)
		messaging.LogDivergence(s.messageLog, messaging.DivergenceEntry{
			MessageID:  storeMsg.ID,
			OldRouting: oldRouting,
			NewRouting: messaging.NewRoutingStr(convID),
			Match:      match,
			Reason:     reason,
		})
		// Propagate GroupID from metadata so CLI-originated group[] messages
		// preserve correlation in the store.
		if structuredMsg.Metadata != nil {
			if gid, ok := structuredMsg.Metadata["group_id"]; ok {
				storeMsg.GroupID = gid
			}
		}
		if err := s.store.CreateMessage(ctx, storeMsg); err != nil {
			s.messageLog.Error("Failed to persist message", "error", err)
		} else {
			persistedMsgID = storeMsg.ID
		}
		// B11/B13: only publish when persistence succeeded — publishing an
		// unpersisted message is not legal.
		if persistedMsgID != "" {
			// Publish SSE event so connected browser clients can update the
			// per-agent conversation view in real time — mirrors the agent→user
			// publish path in handleAgentOutboundMessage.
			s.events.PublishUserMessage(ctx, storeMsg)
		}
	}

	// Managed agent path: deliver message directly via backend, bypass broker.
	if isManagedAgentRuntime(agent.Runtime) {
		if err := s.managedAgentMessage(ctx, agent, plainMessage, req.Interrupt); err != nil {
			if persistedMsgID != "" {
				if markErr := s.store.MarkMessageFailed(ctx, persistedMsgID, err.Error()); markErr != nil {
					s.messageLog.Error("Failed to mark message as failed", "id", persistedMsgID, "error", markErr)
				}
			}
			RuntimeError(w, "Failed to send message to managed agent: "+err.Error())
			return
		}

		agent.Phase = string(state.PhaseRunning)
		agent.Activity = "working"
		_ = s.store.UpdateAgentStatus(ctx, agent.ID, store.AgentStatusUpdate{
			Phase:    agent.Phase,
			Activity: agent.Activity,
		})
		s.events.PublishAgentStatus(ctx, agent)

		// Process @mentions for managed agents too.
		var managedMentionResults []messages.MentionResult
		if len(req.Mentions) > 0 && structuredMsg != nil {
			managedMentionResults = s.processMentions(ctx, req.Mentions, agent, structuredMsg)
		}

		// B11/B13: reflect persistence failure in the response status.
		// The request still succeeds (dispatch worked), but the caller
		// should know the message was not persisted.
		managedStatus := "delivered"
		if persistedMsgID == "" {
			managedStatus = "delivered_not_persisted"
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(MessageDeliveryResponse{
			MessageID:      persistedMsgID,
			Status:         managedStatus,
			Agent:          agent.Slug,
			AgentPhase:     agent.Phase,
			MentionResults: managedMentionResults,
		})
		return
	}

	// If a dispatcher is available, dispatch the message to the runtime broker
	dispatcher := s.GetDispatcher()
	if dispatcher == nil {
		ServiceNotReady(w, "Message dispatch is not available yet — the server may still be starting up")
		return
	}
	if agent.RuntimeBrokerID == "" {
		ServiceNotReady(w, "Agent has no runtime broker assigned — the server may still be starting up")
		return
	}

	// Synchronous delivery with 30s retry deadline for transient broker failures.
	retryCtx, retryCancel := context.WithTimeout(ctx, 30*time.Second)
	defer retryCancel()

	if err := dispatchWithBrokerRetry(retryCtx, dispatcher, agent, plainMessage, req.Interrupt, structuredMsg); err != nil {
		if persistedMsgID != "" {
			if markErr := s.store.MarkMessageFailed(ctx, persistedMsgID, err.Error()); markErr != nil {
				s.messageLog.Error("Failed to mark message as failed", "id", persistedMsgID, "error", markErr)
			}
		}
		if errors.Is(err, ErrBrokerTimeout) {
			GatewayTimeout(w, "Broker unreachable after 30s deadline")
		} else if req.Wake {
			RuntimeError(w, "Agent resumed successfully but message delivery failed: "+err.Error())
		} else {
			RuntimeError(w, "Failed to send message to runtime broker: "+err.Error())
		}
		return
	}

	// Publish agent-to-agent messages through the broker so plugin observers
	// (Telegram, broker-log) can see them. ObserverOnly prevents the hub's own
	// subscription from re-dispatching.
	if strings.HasPrefix(structuredMsg.Sender, "agent:") &&
		strings.HasPrefix(structuredMsg.Recipient, "agent:") {
		if bp := s.GetMessageBrokerProxy(); bp != nil {
			observerMsg := *structuredMsg
			observerMsg.ObserverOnly = true
			if err := bp.PublishMessage(ctx, agent.ProjectID, &observerMsg); err != nil {
				s.messageLog.Error("Failed to publish agent-to-agent observer message",
					"agent_id", agent.ID, "error", err)
			}
		}
	}

	// Create notification subscription if requested
	if req.Notify {
		var notifySubscriberType, notifySubscriberID, createdBy string
		if agentIdent := GetAgentIdentityFromContext(ctx); agentIdent != nil {
			createdBy = agentIdent.ID()
			if creatorAgent, err := s.store.GetAgent(ctx, agentIdent.ID()); err == nil {
				notifySubscriberType = store.SubscriberTypeAgent
				notifySubscriberID = creatorAgent.Slug
			}
		} else if userIdent := GetUserIdentityFromContext(ctx); userIdent != nil {
			createdBy = userIdent.ID()
			notifySubscriberType = store.SubscriberTypeUser
			notifySubscriberID = userIdent.ID()
		}
		s.createNotifySubscription(ctx, agent.ID, agent.ProjectID, notifySubscriberType, notifySubscriberID, createdBy)
	}

	// Process @mentions: validate slugs, fan out mention messages to resolved agents.
	var mentionResults []messages.MentionResult
	if len(req.Mentions) > 0 && structuredMsg != nil {
		mentionResults = s.processMentions(ctx, req.Mentions, agent, structuredMsg)
	}

	// B11/B13: reflect persistence failure in the response status.
	deliveryStatus := "delivered"
	if persistedMsgID == "" {
		deliveryStatus = "delivered_not_persisted"
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(MessageDeliveryResponse{
		MessageID:      persistedMsgID,
		Status:         deliveryStatus,
		Agent:          agent.Slug,
		AgentPhase:     agent.Phase,
		MentionResults: mentionResults,
	})
}

// MessageDeliveryResponse is the JSON response for a successful agent message delivery.
type MessageDeliveryResponse struct {
	MessageID      string                   `json:"message_id"`
	Status         string                   `json:"status"`
	Agent          string                   `json:"agent"`
	AgentPhase     string                   `json:"agent_phase"`
	MentionResults []messages.MentionResult `json:"mention_results,omitempty"`
}

// GroupMessageRecipientResult represents the delivery status for one recipient in a group[] delivery.
type GroupMessageRecipientResult struct {
	Recipient string `json:"recipient"`
	Status    string `json:"status"`
	Error     string `json:"error,omitempty"`
}

// GroupMessageResponse is the JSON response for a group[] message delivery.
type GroupMessageResponse struct {
	GroupID   string                        `json:"group_id"`
	Delivered int                           `json:"delivered"`
	Failed    int                           `json:"failed"`
	Results   []GroupMessageRecipientResult `json:"results"`
}

// handleGroupMessage fans out a structured message to multiple recipients parsed from group[].
func (s *Server) handleGroupMessage(w http.ResponseWriter, r *http.Request, anchorID string, msg *messages.StructuredMessage, plainMessage string, interrupt bool) {
	ctx := r.Context()

	recipients, err := messages.ParseGroupRecipient(msg.Recipient)
	if err != nil {
		ValidationError(w, "invalid group[] recipient: "+err.Error(), nil)
		return
	}

	// Resolve the anchor agent for project context.
	anchorAgent, err := s.store.GetAgent(ctx, anchorID)
	if err != nil {
		writeErrorFromErr(w, err, "")
		return
	}
	projectID := anchorAgent.ProjectID

	recipientStrs := make([]string, len(recipients))
	for i, r := range recipients {
		recipientStrs[i] = r.String()
	}
	recipientsSet := messages.FormatGroupRecipients(msg.Sender, recipientStrs)

	groupID := api.NewUUID()
	results := make([]GroupMessageRecipientResult, len(recipients))
	delivered := 0

	dispatcher := s.GetDispatcher()

	// Note: retries are sequential — large groups with unreachable members
	// may block for up to N × 30s. Future work: parallel dispatch.
	for i, recip := range recipients {
		recipStr := recip.String()

		switch recip.Kind {
		case messages.RecipientAgent:
			agent, err := s.store.GetAgentBySlug(ctx, projectID, api.Slugify(recip.Name))
			if err != nil {
				results[i] = GroupMessageRecipientResult{Recipient: recipStr, Status: "failed", Error: "agent not found: " + recip.Name}
				continue
			}

			agentMsg := *msg
			agentMsg.Type = messages.TypeGroupSet
			agentMsg.Recipient = "agent:" + agent.Slug
			agentMsg.RecipientID = agent.ID
			agentMsg.Recipients = recipientsSet

			storeMsg := &store.Message{
				ID:            api.NewUUID(),
				ProjectID:     projectID,
				Sender:        agentMsg.Sender,
				SenderID:      agentMsg.SenderID,
				Recipient:     agentMsg.Recipient,
				RecipientID:   agentMsg.RecipientID,
				Msg:           agentMsg.Msg,
				Type:          agentMsg.Type,
				Urgent:        agentMsg.Urgent,
				AgentID:       agent.ID,
				GroupID:       groupID,
				DispatchState: store.MessageDispatchDispatched,
				CreatedAt:     time.Now(),
			}
			// Phase 5 dual-write: resolve-or-create conversation for group set message.
			// B5 SECURITY: derive sender from authenticated context, never payload.
			var convResult *messaging.ConversationResult
			if agent.ID != "" {
				if authKind, authID := authenticatedSender(ctx); authID != "" {
					convResult = messaging.ResolveOrCreateDMConversation(ctx, s.store, s.store, s.messageLog, authKind, authID, "agent", agent.ID)
				}
			}
			if convResult != nil {
				storeMsg.ConversationID = convResult.ConversationID
			}
			// Always log divergence — even when convResult is nil, that is a divergence signal.
			oldRouting := messaging.OldRoutingFromMessage(agentMsg.SenderID, agent.ID, "")
			convID := ""
			actualRef := ""
			if convResult != nil {
				convID = convResult.ConversationID
				actualRef = convResult.ExternalRef
			}
			match, reason := messaging.ComputeDivergenceMatch(oldRouting, actualRef, convID)
			messaging.LogDivergence(s.messageLog, messaging.DivergenceEntry{
				MessageID:  storeMsg.ID,
				OldRouting: oldRouting,
				NewRouting: messaging.NewRoutingStr(convID),
				Match:      match,
				Reason:     reason,
			})
			if err := s.store.CreateMessage(ctx, storeMsg); err != nil {
				s.messageLog.Error("Failed to persist set message", "recipient", recipStr, "error", err)
			} else {
				// B11/B13: only publish when persistence succeeded.
				s.events.PublishUserMessage(ctx, storeMsg)
			}

			if dispatcher == nil {
				results[i] = GroupMessageRecipientResult{Recipient: recipStr, Status: "failed", Error: "dispatcher not available"}
				continue
			}
			if agent.RuntimeBrokerID == "" {
				results[i] = GroupMessageRecipientResult{Recipient: recipStr, Status: "failed", Error: "agent has no runtime broker"}
				continue
			}

			retryCtx, retryCancel := context.WithTimeout(ctx, 30*time.Second)
			if err := dispatchWithBrokerRetry(retryCtx, dispatcher, agent, plainMessage, interrupt, &agentMsg); err != nil {
				retryCancel()
				if markErr := s.store.MarkMessageFailed(ctx, storeMsg.ID, err.Error()); markErr != nil {
					s.messageLog.Error("Failed to mark set message as failed", "id", storeMsg.ID, "error", markErr)
				}
				results[i] = GroupMessageRecipientResult{Recipient: recipStr, Status: "failed", Error: err.Error()}
				continue
			}
			retryCancel()

			// Publish agent-to-agent messages through the broker for plugin observers.
			if strings.HasPrefix(agentMsg.Sender, "agent:") {
				if bp := s.GetMessageBrokerProxy(); bp != nil {
					observerMsg := agentMsg
					observerMsg.ObserverOnly = true
					if err := bp.PublishMessage(ctx, projectID, &observerMsg); err != nil {
						s.messageLog.Error("Failed to publish group[] observer message",
							"recipient", recipStr, "error", err)
					}
				}
			}

			results[i] = GroupMessageRecipientResult{Recipient: recipStr, Status: "delivered"}
			delivered++

		case messages.RecipientUser:
			userRecip := "user:" + recip.Name
			userID := ""

			// Try to resolve user by email or display name.
			identifier := recip.Name
			if strings.Contains(identifier, "@") {
				if u, err := s.store.GetUserByEmail(ctx, identifier); err == nil {
					userID = u.ID
					name := u.DisplayName
					if name == "" {
						name = u.Email
					}
					userRecip = "user:" + name
				}
			}
			if userID == "" {
				result, lookupErr := s.store.ListUsers(ctx, store.UserFilter{Search: identifier}, store.ListOptions{Limit: 1})
				if lookupErr == nil && len(result.Items) == 1 {
					u := result.Items[0]
					userID = u.ID
					name := u.DisplayName
					if name == "" {
						name = u.Email
					}
					userRecip = "user:" + name
				}
			}

			if userID == "" {
				results[i] = GroupMessageRecipientResult{Recipient: recipStr, Status: "failed", Error: "user not found: " + recip.Name}
				continue
			}

			userMsg := *msg
			userMsg.Type = messages.TypeGroupSet
			userMsg.Recipient = userRecip
			userMsg.RecipientID = userID
			userMsg.Recipients = recipientsSet

			storeMsg := &store.Message{
				ID:          api.NewUUID(),
				ProjectID:   projectID,
				Sender:      userMsg.Sender,
				SenderID:    userMsg.SenderID,
				Recipient:   userMsg.Recipient,
				RecipientID: userMsg.RecipientID,
				Msg:         userMsg.Msg,
				Type:        userMsg.Type,
				Urgent:      userMsg.Urgent,
				AgentID:     anchorAgent.ID,
				GroupID:     groupID,
				CreatedAt:   time.Now(),
			}
			// Phase 5 dual-write: resolve-or-create conversation for group set message to user.
			// B5 SECURITY: derive sender from authenticated context, never payload.
			var convResult *messaging.ConversationResult
			if userID != "" {
				if authKind, authID := authenticatedSender(ctx); authID != "" {
					convResult = messaging.ResolveOrCreateDMConversation(ctx, s.store, s.store, s.messageLog, authKind, authID, "user", userID)
				}
			}
			if convResult != nil {
				storeMsg.ConversationID = convResult.ConversationID
			}
			// Always log divergence — even when convResult is nil, that is a divergence signal.
			oldRouting := messaging.OldRoutingFromMessage(userMsg.SenderID, userID, "")
			convID := ""
			actualRef := ""
			if convResult != nil {
				convID = convResult.ConversationID
				actualRef = convResult.ExternalRef
			}
			match, reason := messaging.ComputeDivergenceMatch(oldRouting, actualRef, convID)
			messaging.LogDivergence(s.messageLog, messaging.DivergenceEntry{
				MessageID:  storeMsg.ID,
				OldRouting: oldRouting,
				NewRouting: messaging.NewRoutingStr(convID),
				Match:      match,
				Reason:     reason,
			})
			if err := s.store.CreateMessage(ctx, storeMsg); err != nil {
				s.messageLog.Error("Failed to persist set message", "recipient", recipStr, "error", err)
			} else {
				// B11/B13: only publish when persistence succeeded.
				s.events.PublishUserMessage(ctx, storeMsg)
			}

			results[i] = GroupMessageRecipientResult{Recipient: recipStr, Status: "delivered"}
			delivered++
		}
	}

	s.logMessage("set message dispatched",
		"project_id", projectID,
		"group_id", groupID,
		"total", len(recipients),
		"delivered", delivered,
		"failed", len(recipients)-delivered,
	)

	resp := GroupMessageResponse{
		GroupID:   groupID,
		Delivered: delivered,
		Failed:    len(recipients) - delivered,
		Results:   results,
	}
	writeJSON(w, http.StatusOK, resp)
}

// BroadcastMessageRequest is the request body for broadcasting a message via the broker.
type BroadcastMessageRequest struct {
	StructuredMessage *messages.StructuredMessage `json:"structured_message"`
	Interrupt         bool                        `json:"interrupt,omitempty"`
}

// handleProjectBroadcast handles POST /api/v1/projects/{projectId}/broadcast.
// It publishes a broadcast message to the project's message broker topic,
// which fans out to all running agents in the project.
func (s *Server) handleProjectBroadcast(w http.ResponseWriter, r *http.Request, projectID string) {
	if r.Method != http.MethodPost {
		MethodNotAllowed(w)
		return
	}

	// Require user or agent authentication
	ctx := r.Context()
	userIdent := GetUserIdentityFromContext(ctx)
	agentIdent := GetAgentIdentityFromContext(ctx)
	if userIdent == nil && agentIdent == nil {
		writeError(w, http.StatusForbidden, ErrCodeForbidden, "Broadcast requires user or agent authentication", nil)
		return
	}

	// Phase 3 msg-authz: Agent callers must be in the same project.
	// ScopeAgentLifecycle no longer required — messaging is a first-class axis (D1).
	// Per-recipient authorization happens below via authorizeAgentMessage.
	if agentIdent != nil && userIdent == nil {
		if agentIdent.ProjectID() != projectID {
			writeError(w, http.StatusForbidden, ErrCodeForbidden, "Agents can only broadcast within their own project", nil)
			return
		}
	}

	// Phase 3 msg-authz: User callers — verify project exists and user has
	// basic project read access. This prevents outsiders from broadcasting.
	// The actual per-agent authorization happens per-recipient below via
	// authorizeAgentMessage. The project-level ActionAttach check is replaced
	// with ActionRead as a fast-fail gate (D1: messaging separated from attach).
	if userIdent != nil {
		project, err := s.store.GetProject(ctx, projectID)
		if err != nil {
			if errors.Is(err, store.ErrNotFound) {
				NotFound(w, "Project")
			} else {
				writeErrorFromErr(w, err, "")
			}
			return
		}
		if !s.authorize(w, r, projectResource(project), ActionRead) {
			return // authorize writes 403
		}
	}

	var req BroadcastMessageRequest
	if err := readJSON(r, &req); err != nil {
		BadRequest(w, "Invalid request body: "+err.Error())
		return
	}

	if req.StructuredMessage == nil {
		ValidationError(w, "structured_message is required", nil)
		return
	}

	// B5 SECURITY FIX: ALWAYS derive sender identity from the
	// authenticated context, same as handleAgentMessage. Client-supplied
	// Sender and SenderID are untrusted and must not be used as DM key
	// inputs or for routing decisions (self-skip).
	req.StructuredMessage.Sender = "user:unknown"
	req.StructuredMessage.SenderID = ""
	if userIdent != nil {
		req.StructuredMessage.SenderID = userIdent.ID()
		if name := userIdent.DisplayName(); name != "" {
			req.StructuredMessage.Sender = "user:" + name
		} else if email := userIdent.Email(); email != "" {
			req.StructuredMessage.Sender = "user:" + email
		}
	} else if agentIdent != nil {
		req.StructuredMessage.SenderID = agentIdent.ID()
		req.StructuredMessage.Sender = "agent:" + agentIdent.ID()
	}

	// B5 SECURITY FIX: force Broadcasted = true server-side. The client
	// must not control whether its message is treated as a broadcast —
	// that is a routing fact the server knows. Without this, a client
	// setting Broadcasted=false walks the message through the DM
	// dual-write in deliverToAgent, creating a DM conversation per
	// running agent.
	req.StructuredMessage.Broadcasted = true

	// Use authenticated identity for self-skip, not the Sender field.
	// The Sender field is a display label; the auth identity is the
	// security-relevant identity. A forged Sender could change which
	// agents are targeted.
	authKind, authID := authenticatedSender(ctx)

	// Compute broadcast targeting: list all agents, classify by phase.
	allResult, err := s.store.ListAgents(ctx, store.AgentFilter{
		ProjectID: projectID,
	}, store.ListOptions{})
	if err != nil {
		writeErrorFromErr(w, err, "")
		return
	}

	var targeted int
	skippedBreakdown := make(map[string]int)
	for _, agent := range allResult.Items {
		// Skip the sending agent to avoid self-delivery.
		if authKind == "agent" && agent.ID == authID {
			continue
		}
		if agent.Phase == string(state.PhaseRunning) {
			targeted++
		} else {
			skippedBreakdown[agent.Phase]++
		}
	}
	skipped := 0
	for _, c := range skippedBreakdown {
		skipped += c
	}

	// Collect running agents from the already-fetched list for direct fan-out.
	var runningAgents []store.Agent
	for _, agent := range allResult.Items {
		// Skip the sending agent to avoid self-delivery.
		if authKind == "agent" && agent.ID == authID {
			continue
		}
		if agent.Phase == string(state.PhaseRunning) {
			runningAgents = append(runningAgents, agent)
		}
	}

	// Phase 3 msg-authz: Pre-filter recipients through authorizeAgentMessage.
	// A project owner's broadcast reaches lineage/branch agents; a member's
	// reaches only project-mode agents; none-mode agents never reached
	// (except by super-admin). Per D4 constraint in the design doc.
	senderIdentity := GetIdentityFromContext(ctx)
	var authorizedAgents []store.Agent
	for i := range runningAgents {
		a := &runningAgents[i]
		allowed, _ := s.authorizeAgentMessage(ctx, senderIdentity, a, false)
		if allowed {
			authorizedAgents = append(authorizedAgents, runningAgents[i])
		}
	}
	// Update targeted count to reflect filtering.
	filtered := len(runningAgents) - len(authorizedAgents)
	targeted = len(authorizedAgents)
	if filtered > 0 {
		skipped += filtered
		skippedBreakdown["unauthorized"] = filtered
	}
	runningAgents = authorizedAgents

	// Log the broadcast
	logAttrs := []any{"project_id", projectID}
	logAttrs = append(logAttrs, req.StructuredMessage.LogAttrs()...)
	s.logMessage("broadcast message published", logAttrs...)

	proxy := s.GetMessageBrokerProxy()
	if proxy == nil {
		// No broker configured — use direct fan-out with pre-filtered list.
		if !s.broadcastDirect(w, r, projectID, req.StructuredMessage, req.Interrupt, runningAgents) {
			return
		}
	} else {
		// Phase 3 msg-authz: Broker is available. PublishBroadcast fans out to
		// ALL subscribed agents, which bypasses our per-recipient filter. Instead,
		// publish per-agent messages through the broker for each authorized agent.
		for _, agent := range runningAgents {
			agentMsg := *req.StructuredMessage
			agentMsg.Recipient = "agent:" + agent.Slug
			agentMsg.RecipientID = agent.ID
			if err := proxy.PublishMessage(ctx, projectID, &agentMsg); err != nil {
				s.messageLog.Error("Failed to publish filtered broadcast to agent",
					"agent_id", agent.ID, "agent_slug", agent.Slug, "error", err)
			}
		}
	}

	s.writeBroadcastResponse(w, targeted+skipped, targeted, skipped, skippedBreakdown)
}

// BroadcastAcceptedResponse is the JSON response for a broadcast message.
type BroadcastAcceptedResponse struct {
	Status           string         `json:"status"`
	Total            int            `json:"total"`
	Targeted         int            `json:"targeted"`
	Skipped          int            `json:"skipped"`
	SkippedBreakdown map[string]int `json:"skipped_breakdown,omitempty"`
}

func (s *Server) writeBroadcastResponse(w http.ResponseWriter, total, targeted, skipped int, skippedBreakdown map[string]int) {
	resp := BroadcastAcceptedResponse{
		Status:   "accepted",
		Total:    total,
		Targeted: targeted,
		Skipped:  skipped,
	}
	if len(skippedBreakdown) > 0 {
		resp.SkippedBreakdown = skippedBreakdown
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusAccepted)
	_ = json.NewEncoder(w).Encode(resp)
}

// broadcastDirect fans out a broadcast message directly to the given running agents
// without using the message broker. The caller provides pre-filtered running agents
// (already excluding the sender) from the same ListAgents query used for targeting counts.
// Returns true on success (caller writes 202 response), false if an error response was written.
func (s *Server) broadcastDirect(w http.ResponseWriter, r *http.Request, projectID string, msg *messages.StructuredMessage, interrupt bool, runningAgents []store.Agent) bool {
	ctx := r.Context()
	dispatcher := s.GetDispatcher()
	if dispatcher == nil {
		ServiceNotReady(w, "Message dispatch is not available yet — the server may still be starting up")
		return false
	}

	for _, agent := range runningAgents {
		agentMsg := *msg
		agentMsg.Recipient = "agent:" + agent.Slug
		agentMsg.RecipientID = agent.ID

		storeMsg := &store.Message{
			ID:            api.NewUUID(),
			ProjectID:     projectID,
			Sender:        agentMsg.Sender,
			SenderID:      agentMsg.SenderID,
			Recipient:     agentMsg.Recipient,
			RecipientID:   agentMsg.RecipientID,
			Msg:           agentMsg.Msg,
			Type:          agentMsg.Type,
			Urgent:        agentMsg.Urgent,
			Broadcasted:   true,
			AgentID:       agent.ID,
			DispatchState: store.MessageDispatchDispatched,
			CreatedAt:     time.Now(),
		}
		if err := s.store.CreateMessage(ctx, storeMsg); err != nil {
			s.messageLog.Error("Failed to persist broadcast message", "agent_id", agent.ID, "error", err)
		}

		retryCtx, retryCancel := context.WithTimeout(ctx, 30*time.Second)
		dispatchErr := dispatchWithBrokerRetry(retryCtx, dispatcher, &agent, agentMsg.Msg, interrupt, &agentMsg)
		retryCancel()

		if dispatchErr != nil {
			s.messageLog.Error("Failed to deliver broadcast message to agent",
				"agent_id", agent.ID,
				"agentSlug", agent.Slug, "error", dispatchErr)
			if markErr := s.store.MarkMessageFailed(ctx, storeMsg.ID, dispatchErr.Error()); markErr != nil {
				s.messageLog.Error("Failed to mark broadcast message as failed", "id", storeMsg.ID, "error", markErr)
			}
			s.publishBroadcastDeliveryFailed(ctx, &agent, &agentMsg, dispatchErr)
		}
	}
	return true
}

// publishBroadcastDeliveryFailed publishes a DELIVERY_FAILED notification to the
// message sender when a per-agent broadcast delivery fails.
func (s *Server) publishBroadcastDeliveryFailed(ctx context.Context, targetAgent *store.Agent, msg *messages.StructuredMessage, deliveryErr error) {
	if !strings.HasPrefix(msg.Sender, "agent:") || msg.SenderID == "" {
		return
	}
	senderAgent, err := s.store.GetAgent(ctx, msg.SenderID)
	if err != nil {
		return
	}

	failMsg := fmt.Sprintf("Broadcast delivery failed to agent %q: %v", targetAgent.Slug, deliveryErr)
	structuredMsg := &messages.StructuredMessage{
		Sender:      "system",
		Recipient:   msg.Sender,
		RecipientID: senderAgent.ID,
		Msg:         failMsg,
		Type:        messages.TypeSystem,
		Status:      "DELIVERY_FAILED",
		Metadata:    map[string]string{"system_category": messages.SystemCategoryDeliveryFailed},
	}

	dispatcher := s.GetDispatcher()
	if dispatcher == nil {
		return
	}
	if err := dispatcher.DispatchAgentMessage(ctx, senderAgent, failMsg, false, structuredMsg); err != nil {
		s.messageLog.Error("Failed to dispatch broadcast DELIVERY_FAILED notification",
			"sender_id", msg.SenderID, "target_agent", targetAgent.Slug, "error", err)
	}
}

// processMentions validates mention slugs against project agents, fans out
// NewMention messages to each valid recipient, and returns per-slug results.
// The primary recipient (the agent the message was sent to) is excluded.
func (s *Server) processMentions(ctx context.Context, mentionSlugs []string, primaryAgent *store.Agent, originalMsg *messages.StructuredMessage) []messages.MentionResult {
	if len(mentionSlugs) == 0 {
		return nil
	}

	// List project agents for resolution.
	agentList, err := s.store.ListAgents(ctx, store.AgentFilter{ProjectID: primaryAgent.ProjectID}, store.ListOptions{Limit: 200})
	if err != nil {
		s.messageLog.Error("Failed to list project agents for mention resolution", "error", err)
		return nil
	}

	// Build the AgentInfo slice and a slug-to-agent map for dispatch.
	agentInfos := make([]messages.AgentInfo, 0, len(agentList.Items))
	agentBySlug := make(map[string]*store.Agent, len(agentList.Items))
	for i := range agentList.Items {
		a := &agentList.Items[i]
		agentInfos = append(agentInfos, messages.AgentInfo{Slug: a.Slug, Name: a.Name})
		agentBySlug[strings.ToLower(a.Slug)] = a
	}

	// Resolve mentions using the shared package.
	results := messages.ResolveMentions(mentionSlugs, agentInfos, primaryAgent.Slug)

	// Aggregate timeout for all mention dispatches (O1): 30s total to avoid
	// blocking the HTTP response for up to N × 10s in the worst case.
	aggregateCtx, aggregateCancel := context.WithTimeout(ctx, 30*time.Second)
	defer aggregateCancel()

	// Fan out mention messages for each delivered slug.
	for i, r := range results {
		if r.Status != "delivered" {
			continue
		}

		// Check aggregate timeout before starting each dispatch.
		if aggregateCtx.Err() != nil {
			results[i].Status = "timeout"
			results[i].Error = "aggregate mention dispatch timeout exceeded"
			continue
		}

		mentionAgent, ok := agentBySlug[strings.ToLower(r.Slug)]
		if !ok {
			results[i].Status = "error"
			results[i].Error = "agent resolved but not found for dispatch"
			continue
		}

		mentionMsg := messages.NewMention(originalMsg.Sender, "agent:"+r.Slug, originalMsg.Msg, originalMsg.Recipient)
		mentionMsg.SenderID = originalMsg.SenderID
		mentionMsg.RecipientID = mentionAgent.ID
		mentionMsg.Channel = originalMsg.Channel
		mentionMsg.ThreadID = originalMsg.ThreadID

		// Persist the mention message.
		storeMsg := &store.Message{
			ID:            api.NewUUID(),
			ProjectID:     primaryAgent.ProjectID,
			Sender:        mentionMsg.Sender,
			SenderID:      mentionMsg.SenderID,
			Recipient:     mentionMsg.Recipient,
			RecipientID:   mentionMsg.RecipientID,
			Msg:           mentionMsg.Msg,
			Type:          mentionMsg.Type,
			AgentID:       mentionAgent.ID,
			Channel:       mentionMsg.Channel,
			ThreadID:      mentionMsg.ThreadID,
			DispatchState: store.MessageDispatchDispatched,
			CreatedAt:     time.Now(),
		}
		if mentionMsg.Metadata != nil {
			storeMsg.GroupID = mentionMsg.Metadata["group_id"]
		}
		// B15 dual-write: resolve-or-create conversation for agent mention dispatch.
		{
			var convResult *messaging.ConversationResult
			if mentionMsg.ThreadID != "" {
				convResult = messaging.ResolveOrCreateThreadConversation(ctx, s.store, s.messageLog, mentionMsg.ThreadID, primaryAgent.ProjectID)
			} else if mentionMsg.SenderID != "" && mentionAgent.ID != "" {
				senderKind, sOK := messages.PrincipalKindFromAddress(mentionMsg.Sender)
				if sOK {
					convResult = messaging.ResolveOrCreateDMConversation(ctx, s.store, s.store, s.messageLog, senderKind, mentionMsg.SenderID, "agent", mentionAgent.ID)
				}
			}
			if convResult != nil {
				storeMsg.ConversationID = convResult.ConversationID
			}
		}
		var persisted bool
		if createErr := s.store.CreateMessage(ctx, storeMsg); createErr != nil {
			s.messageLog.Error("Failed to persist mention message", "slug", r.Slug, "error", createErr)
		} else {
			persisted = true
		}
		// B11/B13: only publish when persistence succeeded.
		if persisted {
			s.events.PublishUserMessage(ctx, storeMsg)
		}

		// Dispatch to the mentioned agent's runtime.
		dispatcher := s.GetDispatcher()
		if dispatcher == nil {
			results[i].Status = "error"
			results[i].Error = "dispatch not available"
			continue
		}
		if mentionAgent.RuntimeBrokerID == "" {
			results[i].Status = "error"
			results[i].Error = "agent has no runtime broker"
			continue
		}

		// Per-dispatch timeout is the lesser of 10s or the remaining aggregate budget.
		dispatchCtx, cancel := context.WithTimeout(aggregateCtx, 10*time.Second)
		if dispatchErr := dispatchWithBrokerRetry(dispatchCtx, dispatcher, mentionAgent, mentionMsg.Msg, false, mentionMsg); dispatchErr != nil {
			cancel()
			if aggregateCtx.Err() != nil {
				results[i].Status = "timeout"
				results[i].Error = "aggregate mention dispatch timeout exceeded"
			} else {
				results[i].Status = "error"
				results[i].Error = "dispatch failed: " + dispatchErr.Error()
			}
			if persisted {
				if markErr := s.store.MarkMessageFailed(ctx, storeMsg.ID, dispatchErr.Error()); markErr != nil {
					s.messageLog.Error("Failed to mark mention message as failed", "id", storeMsg.ID, "error", markErr)
				}
			}
			continue
		}
		cancel()
	}

	return results
}

// authenticatedSender returns the principal kind ("user" or "agent") and ID
// from the request's authenticated context. Returns ("", "") when no
// authenticated identity is present.
//
// B5 SECURITY: DM conversation key inputs MUST come from the authenticated
// context, never from the client-supplied message payload. The key IS the
// access control list for direct conversations — any guess on any input to
// the key derivation is a guess on the ACL.
func authenticatedSender(ctx context.Context) (kind, id string) {
	if user := GetUserIdentityFromContext(ctx); user != nil {
		return "user", user.ID()
	}
	if agent := GetAgentIdentityFromContext(ctx); agent != nil {
		return "agent", agent.ID()
	}
	return "", ""
}
