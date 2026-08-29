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
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/messages"
	"github.com/GoogleCloudPlatform/scion/pkg/messaging"
	"github.com/GoogleCloudPlatform/scion/pkg/projectcompat"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// inboundMessageRequest is the JSON body sent by broker plugins to deliver
// inbound messages to the hub.
type inboundMessageRequest struct {
	Topic   string                      `json:"topic"`
	Message *messages.StructuredMessage `json:"message"`
}

// handleBrokerInbound handles POST /api/v1/broker/inbound.
// This is the callback endpoint that broker plugins use to deliver inbound
// messages from external systems to the hub for dispatch to agents.
//
// Authentication: Requires broker HMAC authentication (X-Scion-Broker-ID header
// validated by BrokerAuthMiddleware).
//
// The topic string is parsed to extract the project ID and agent slug. Canonical
// broker topics use scion.project; legacy scion.grove topics are accepted here
// as an external compatibility adapter.
func (s *Server) handleBrokerInbound(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		MethodNotAllowed(w)
		return
	}

	// Require broker HMAC authentication
	broker := GetBrokerIdentityFromContext(r.Context())
	if broker == nil {
		writeError(w, http.StatusUnauthorized, ErrCodeBrokerAuthFailed,
			"broker HMAC authentication required", nil)
		return
	}

	// Log plugin name for observability
	pluginName := r.Header.Get("X-Scion-Plugin-Name")
	log := s.messageLog.With(
		"broker_id", broker.ID(),
		"plugin_name", pluginName,
	)

	// Parse request body
	var req inboundMessageRequest
	if err := readJSON(r, &req); err != nil {
		BadRequest(w, "invalid request body: "+err.Error())
		return
	}

	if req.Topic == "" {
		ValidationError(w, "topic is required", map[string]interface{}{
			"field": "topic",
		})
		return
	}
	if req.Message == nil {
		ValidationError(w, "message is required", map[string]interface{}{
			"field": "message",
		})
		return
	}

	// Parse topic to extract project ID and agent slug
	projectID, agentSlug, err := parseAgentMessageTopic(req.Topic)
	if err != nil {
		BadRequest(w, "invalid topic: "+err.Error())
		return
	}

	// Validate DM key format when the thread_id looks like a DM key.
	if req.Message.ThreadID != "" && strings.HasPrefix(req.Message.ThreadID, "dm:") && !validDMKey(req.Message.ThreadID) {
		BadRequest(w, "invalid DM key format")
		return
	}

	// Look up the agent
	agent, err := s.store.GetAgentBySlug(r.Context(), projectID, agentSlug)
	if err != nil {
		log.Warn("Agent not found for inbound message",
			"project_id", projectID, "agent_slug", agentSlug, "error", err)
		if errors.Is(err, store.ErrNotFound) {
			writeError(w, http.StatusNotFound, ErrCodeAgentNotFound,
				fmt.Sprintf("Agent %q not found in project", agentSlug),
				map[string]interface{}{
					"agent_slug":  agentSlug,
					"project_id":  projectID,
					"remediation": "Use /agents to see available agents, or /default to change the default.",
				})
		} else {
			writeErrorFromErr(w, err, "")
		}
		return
	}

	// Phase 3 msg-authz: Enforce authorizeAgentMessage for user-identity senders.
	// System/infrastructure senders (not user: or agent:) are system-plane
	// messages (D8) — scheduled events, internal hub triggers relayed through
	// broker transport. These bypass mode checks unconditionally; broker HMAC
	// trust covers the infrastructure transport layer.
	// Agent-to-agent via broker: documented as a known gap for follow-up — the
	// broker path for agent-to-agent is less common than the direct API, and
	// constructing an AgentIdentity from the broker request is non-trivial.
	if strings.HasPrefix(req.Message.Sender, "user:") {
		senderEmail := strings.TrimPrefix(req.Message.Sender, "user:")
		senderUser, err := s.store.GetUserByEmail(r.Context(), senderEmail)
		if err != nil {
			log.Warn("Could not resolve sender identity for permission check",
				"sender", req.Message.Sender, "error", err)
			if errors.Is(err, store.ErrNotFound) {
				writeError(w, http.StatusForbidden, ErrCodeForbidden,
					"sender identity could not be resolved", map[string]interface{}{
						"sender": req.Message.Sender,
					})
			} else {
				writeError(w, http.StatusInternalServerError, ErrCodeInternalError,
					"internal error resolving sender identity", nil)
			}
			return
		}
		// Cache the resolved user ID so downstream DM-ownership and
		// persistence blocks can reuse it without redundant DB lookups.
		req.Message.SenderID = senderUser.ID

		userIdent := NewAuthenticatedUser(senderUser.ID, senderUser.Email, senderUser.DisplayName, senderUser.Role, "integration")
		allowed, reason := s.authorizeAgentMessage(r.Context(), userIdent, agent, false)
		if !allowed {
			log.Warn("broker inbound message authorization denied",
				"sender", req.Message.Sender, "agent_slug", agentSlug, "reason", reason)
			writeError(w, http.StatusForbidden, ErrCodeForbidden,
				"Message delivery denied", map[string]interface{}{
					"sender":     req.Message.Sender,
					"agent_slug": agentSlug,
				})
			return
		}
	}
	// else: system-plane (D8) or agent-sender — no authorizeAgentMessage check.
	// System messages are unconditionally exempt. Agent-to-agent via broker
	// relies on broker HMAC trust (known gap documented above).

	// Ownership check: verify the DM key IDs match the actual participants.
	// The agent in the DM key must match the resolved agent; the user must
	// match the sender.
	if req.Message.ThreadID != "" && strings.HasPrefix(req.Message.ThreadID, "dm:") {
		dmAgentID, dmUserID := parseDMKeyIDs(req.Message.ThreadID)
		// SenderID was cached by the upstream permission check for "user:"
		// senders, so no additional DB lookup is needed here.
		senderID := req.Message.SenderID
		if dmAgentID != agent.ID || dmUserID != senderID {
			BadRequest(w, "DM thread_id does not match the sender and recipient")
			return
		}
	}

	// Reject messages to non-running agents.
	if phase := state.Phase(agent.Phase); phase != state.PhaseRunning {
		var msg string
		switch phase {
		case state.PhaseSuspended:
			msg = fmt.Sprintf("Agent %q is suspended.", agent.Slug)
		case state.PhaseStopped:
			msg = fmt.Sprintf("Agent %q is stopped.", agent.Slug)
		case state.PhaseError:
			msg = fmt.Sprintf("Agent %q is in error state.", agent.Slug)
		default:
			msg = fmt.Sprintf("Agent %q is not yet running (phase: %s).", agent.Slug, agent.Phase)
		}
		writeError(w, http.StatusConflict, ErrCodeAgentNotRunning, msg, nil)
		return
	}

	// A leading "!" in the message body acts as an inline interrupt signal:
	// strip the prefix and promote to urgent so the harness is interrupted
	// before delivery — equivalent to --interrupt on the CLI.
	if trimmed := strings.TrimSpace(req.Message.Msg); strings.HasPrefix(trimmed, "!") {
		content := strings.TrimSpace(trimmed[1:])
		if content == "" {
			content = "interrupt"
		}
		req.Message.Msg = content
		req.Message.Urgent = true
	}

	// Dispatch directly to the agent, bypassing the broker to avoid circular delivery
	dispatcher := s.GetDispatcher()
	if dispatcher == nil {
		writeError(w, http.StatusServiceUnavailable, ErrCodeUnavailable,
			"no dispatcher available", nil)
		return
	}

	// Capture arrival time before dispatch so the persisted CreatedAt reflects
	// when the hub received the message, not when dispatch completed (which can
	// be up to 30s later under retry).
	now := time.Now().UTC()

	retryCtx, retryCancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer retryCancel()

	if err := dispatchWithBrokerRetry(retryCtx, dispatcher, agent, req.Message.Msg, req.Message.Urgent, req.Message); errors.Is(err, ErrBrokerTimeout) {
		GatewayTimeout(w, "Broker unreachable after 30s deadline")
		return
	} else if err != nil {
		log.Error("Failed to dispatch inbound message",
			"agent_id", agent.ID, "agent_slug", agentSlug, "error", err)
		writeError(w, http.StatusBadGateway, ErrCodeRuntimeError,
			"failed to deliver message to agent: "+err.Error(), nil)
		return
	}

	log.Info("Inbound message delivered",
		"project_id", projectID,
		"agent_id", agent.ID,
		"agent_slug", agentSlug,
		"sender", req.Message.Sender,
		"type", req.Message.Type,
	)

	// The sender user ID was resolved and cached in req.Message.SenderID
	// during the upstream permission check for "user:" senders.
	senderUserID := req.Message.SenderID

	// F5 fix (Phase 6): Persist the inbound message and publish an SSE event
	// so that messages from external channels (Discord, Telegram) appear in
	// the web chat — both live and after a refresh. This mirrors the
	// persistence + SSE pattern used by handleAgentMessage.
	storeMsg := &store.Message{
		ID:            api.NewUUID(),
		ProjectID:     agent.ProjectID,
		Sender:        req.Message.Sender,
		SenderID:      senderUserID,
		Recipient:     "agent:" + agent.Slug,
		RecipientID:   agent.ID,
		Msg:           req.Message.Msg,
		Type:          req.Message.Type,
		Urgent:        req.Message.Urgent,
		AgentID:       agent.ID,
		Channel:       req.Message.Channel,
		ThreadID:      req.Message.ThreadID,
		Visibility:    req.Message.Visibility,
		Broadcasted:   req.Message.Broadcasted,
		DispatchState: store.MessageDispatchDispatched,
		CreatedAt:     now,
	}
	if req.Message.Metadata != nil {
		if gid, ok := req.Message.Metadata["group_id"]; ok {
			storeMsg.GroupID = gid
		}
	}
	// B15 dual-write: resolve-or-create conversation for broker inbound messages.
	{
		var convResult *messaging.ConversationResult
		if req.Message.ThreadID != "" {
			convResult = messaging.ResolveOrCreateThreadConversation(r.Context(), s.store, log, req.Message.ThreadID, agent.ProjectID)
		} else if senderUserID != "" && agent.ID != "" {
			senderKind, sOK := messages.PrincipalKindFromAddress(req.Message.Sender)
			if sOK {
				convResult = messaging.ResolveOrCreateDMConversation(r.Context(), s.store, s.store, log, senderKind, senderUserID, "agent", agent.ID)
			}
		}
		if convResult != nil {
			storeMsg.ConversationID = convResult.ConversationID
		}
	}
	if err := s.store.CreateMessage(r.Context(), storeMsg); err != nil {
		log.Error("Failed to persist inbound broker message", "error", err)
		// Non-fatal: the dispatch already succeeded, so the agent got the
		// message. Failing the HTTP response here would mislead the caller
		// into retrying — which would double-deliver.
	} else {
		s.events.PublishUserMessage(r.Context(), storeMsg)
	}

	// Record reply-affinity context so that the agent's next untagged reply
	// can be routed back to the channel the user last spoke from (AC22).
	// Only record for user-identity senders with a known channel.
	if s.webChatStore != nil && req.Message.Channel != "" && strings.HasPrefix(req.Message.Sender, "user:") {
		if senderUserID != "" {
			if err := s.webChatStore.RecordChannel(r.Context(), senderUserID, agent.ProjectID, agent.ID, req.Message.Channel, now); err != nil {
				log.Error("Failed to record conversation context for broker inbound",
					"user_id", senderUserID, "agent_id", agent.ID, "channel", req.Message.Channel, "error", err)
			}
			// Update the thread watermark so the Phase 5 thread rail reflects
			// inbound broker messages (last_activity_at / last_message_id).
			if err := s.webChatStore.TouchThread(r.Context(), senderUserID, agent.ProjectID, agent.ID, storeMsg.ID, now); err != nil {
				log.Error("Failed to update thread watermark for broker inbound",
					"user_id", senderUserID, "agent_id", agent.ID, "error", err)
			}
		}
	}

	// Log to dedicated message audit log
	if s.dedicatedMessageLog != nil {
		logAttrs := []any{
			"agent_id", agent.ID,
			"agent_name", agent.Name,
			"project_id", agent.ProjectID,
			"source", "broker-inbound",
			"broker_id", broker.ID(),
			"plugin_name", pluginName,
		}
		logAttrs = append(logAttrs, req.Message.LogAttrs()...)
		s.dedicatedMessageLog.Info("inbound broker message delivered", logAttrs...)
	}

	writeJSON(w, http.StatusOK, map[string]interface{}{
		"delivered": true,
		"agentId":   agent.ID,
	})
}

// parseAgentMessageTopic extracts the project ID and agent slug from a topic string.
// Expected canonical format: scion.project.<projectID>.agent.<agentSlug>.messages.
// Legacy scion.grove topics are accepted at this adapter boundary.
func parseAgentMessageTopic(topic string) (projectID, agentSlug string, err error) {
	parsed, err := projectcompat.ParseTopic(topic)
	if err != nil {
		return "", "", err
	}
	if parsed.Kind != projectcompat.TopicKindAgent {
		return "", "", fmt.Errorf("expected format scion.project.<projectId>.agent.<agentSlug>.messages")
	}
	return parsed.ProjectID, parsed.Actor, nil
}
