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

	// Conversation resolution fields (Phase 11).
	// When Surface and ExternalRef are set, the hub resolves (or creates) a
	// conversation before dispatching the message.  This moves conversation
	// attribution to the broker edge so every inbound message carries a
	// conversation_id.
	Surface     string `json:"surface,omitempty"`
	ExternalRef string `json:"external_ref,omitempty"`
	ParentRef   string `json:"parent_ref,omitempty"`
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

	// Authorize the sender. Identity is resolved from the sender prefix:
	// "user:" senders are looked up in the store; all other senders use the
	// broker identity, whose Type()="broker" does not match any allowed
	// sender type and is denied.
	var senderIdentity Identity
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
		senderIdentity = NewAuthenticatedUser(senderUser.ID, senderUser.Email, senderUser.DisplayName, senderUser.Role, "integration")
	} else {
		// Non-user senders use the broker identity (non-nil by the HMAC
		// check above). Its Type()="broker" is not an allowed sender type,
		// so authorizeAgentMessage denies it.
		senderIdentity = broker
	}
	allowed, reason := s.authorizeAgentMessage(r.Context(), senderIdentity, agent, false)
	if !allowed {
		log.Warn("broker inbound message authorization denied",
			"sender", req.Message.Sender, "agent_slug", agentSlug, "reason", reason)
		writeError(w, http.StatusForbidden, ErrCodeMessageDenied,
			"Message delivery denied", map[string]interface{}{
				"reason":        mapReasonToCode(reason),
				"senderMode":    senderIdentity.Type(),
				"recipientMode": agent.MessageMode,
				"sender":        req.Message.Sender,
				"agent_slug":    agentSlug,
			})
		return
	}

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

	// Validate the inbound message through the new envelope choke point.
	// This catches malformed payloads from external channels (e.g. the Teams
	// adapter emitting channel="" with a non-empty ThreadID) before dispatch
	// (Phase 7, AC-8).
	if err := messaging.ValidateLegacyMessage(req.Message); err != nil {
		writeError(w, http.StatusBadRequest, ErrCodeValidationError, err.Error(), nil)
		return
	}

	// Phase 11: Broker edge conversation resolution.
	// If the plugin provided surface + external_ref, resolve or create a
	// conversation before dispatch.  ExternalRef without Surface is rejected
	// (AC-8 regression guard: a bare thread/ref with no surface is malformed).
	if req.ExternalRef != "" && req.Surface == "" {
		writeError(w, http.StatusBadRequest, ErrCodeValidationError,
			"external_ref requires surface to be set", nil)
		return
	}
	// preDispatchConvResult is declared here so Phase 9b(ii) rendering can
	// use it after the Phase 11 block and before dispatch.
	var preDispatchConvResult *messaging.ConversationResult
	if req.Surface != "" && req.ExternalRef != "" {
		var keyOpts []messaging.ConversationByKeyOption
		keyOpts = append(keyOpts, messaging.WithSurface(req.Surface))
		if req.ParentRef != "" {
			keyOpts = append(keyOpts, messaging.WithParentRef(req.ParentRef))
		}
		if agent.ID != "" {
			agentID := agent.ID
			keyOpts = append(keyOpts, messaging.WithDefaultAgentID(&agentID))
		}
		s.mu.RLock()
		wcs := s.webChatStore
		s.mu.RUnlock()
		if wcs != nil {
			keyOpts = append(keyOpts, messaging.WithKeyTopicLookup(wcs))
		}
		convResult, convErr := messaging.ResolveOrCreateConversationByKey(
			r.Context(), s.store, log, req.ExternalRef, "group", &agent.ProjectID, keyOpts...)
		if convErr != nil {
			if s.writeDenyEnabled() {
				messaging.WriteDenialMetrics.Inc("broker.phase11")
				log.Error("conversation resolution failed", "error", convErr)
				writeError(w, http.StatusConflict, ErrCodeConversationNotResolved, "conversation resolution failed", nil)
				return
			}
			log.Warn("conversation resolution failed (write-deny OFF, continuing)", "error", convErr)
		} else {
			preDispatchConvResult = convResult
			if req.Message.Metadata == nil {
				req.Message.Metadata = make(map[string]string)
			}
			req.Message.Metadata["conversation_id"] = convResult.ConversationID
			log.Info("Resolved conversation for broker inbound",
				"conversation_id", convResult.ConversationID,
				"surface", req.Surface, "external_ref", req.ExternalRef)
		}
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
	brokerInboundMsgID := api.NewUUID()

	// DEF-135: Resolve sender and Phase 5 conversation BEFORE the render so
	// the delivery envelope carries the conversation id. Previously these ran
	// after dispatch, leaving the envelope without a conversation for Discord
	// and other native-DM messages.
	senderUserID := resolveSenderUserID(r.Context(), s.store, req.Message.SenderID, req.Message.Sender)

	// Phase 5 dual-write: resolve-or-create conversation for broker-inbound
	// messages. Skip broadcasts — they are ephemeral and do not belong to a
	// conversation. Resolution errors under write-deny now return 409 BEFORE
	// dispatch (DEF-135 consequence 1: fail-closed, retry-safe).
	var convFromPhase5 *messaging.ConversationResult
	if !req.Message.Broadcasted {
		var convErr error
		convFromPhase5, convErr = s.resolvePhase5Conversation(r.Context(), req.Message.ThreadID, agent.ProjectID, senderUserID, agent.ID)
		if convErr != nil {
			metricKey := "broker.dm"
			if req.Message.ThreadID != "" {
				metricKey = "broker.thread"
			}
			if s.writeDenyEnabled() {
				messaging.WriteDenialMetrics.Inc(metricKey)
				s.messageLog.Error("conversation resolution failed", "error", convErr)
				writeError(w, http.StatusConflict, ErrCodeConversationNotResolved, "conversation resolution failed", nil)
				return
			}
			s.messageLog.Warn("conversation resolution failed (write-deny OFF, continuing)", "error", convErr)
		}
	}

	// DEF-135 precedence rule: Phase 11 (explicit surface + external_ref)
	// wins over Phase 5 (inferred DM/thread) when both produce a result.
	// A single effectiveConv is used for both the envelope and the persisted
	// row, eliminating the prior split where they could silently disagree.
	//
	// F3 fix: broadcasts carry no conversation — not from Phase 5 (already
	// skipped above) and not from Phase 11 either. A broadcast with
	// surface + external_ref set creates the conversation row (Phase 11
	// above) but does NOT stamp it on the envelope or the persisted message.
	// This matches the documented invariant at handlers_agent_messaging.go:1898.
	effectiveConv := convFromPhase5
	if preDispatchConvResult != nil {
		if convFromPhase5 != nil && convFromPhase5.ConversationID != preDispatchConvResult.ConversationID {
			// OQ-135-2: Both resolutions produced a result and they disagree.
			// Log the divergence — this is currently unexercised (no live
			// caller sets surface + external_ref AND has a thread) but will
			// catch latent conflicts if a plugin starts doing so.
			log.Warn("Phase 11 / Phase 5 conversation divergence: Phase 11 wins",
				"phase11_conv_id", preDispatchConvResult.ConversationID,
				"phase5_conv_id", convFromPhase5.ConversationID,
			)
		}
		effectiveConv = preDispatchConvResult
	}
	if req.Message.Broadcasted {
		effectiveConv = nil
	}

	// F2 fix: validate the value that will actually be persisted, not just
	// the Phase 5 result. When Phase 11 wins, effectiveConv differs from
	// convFromPhase5, and the persisted conversation id must still pass
	// the empty-string gate.
	if effectiveConv != nil {
		if err := messaging.ValidateAttributed(effectiveConv.ConversationID); err != nil {
			if s.writeDenyEnabled() {
				messaging.WriteDenialMetrics.Inc("broker.validate")
				writeError(w, http.StatusConflict, ErrCodeConversationNotResolved, err.Error(), nil)
				return
			}
			s.messageLog.Warn("ValidateAttributed failed (write-deny OFF, continuing)", "error", err)
		}
	}

	// Phase 9b(ii): render the delivery envelope before dispatch when the
	// envelope switch is ON. The message ID is pre-generated here (same UUID
	// that will be persisted). effectiveConv may be nil for broadcasts or
	// when resolution was skipped — the renderer correctly omits the
	// conversation key (honest absence per §4.3).
	if s.writeDenyEnabled() {
		req.Message.DeliveryText = messaging.RenderDeliveryText(messaging.RenderDeliveryInput{
			MessageID:  brokerInboundMsgID,
			ConvResult: effectiveConv,
			Msg:        req.Message,
			CreatedAt:  now,
		})
	}

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

	// F5 fix (Phase 6): Persist the inbound message and publish an SSE event
	// so that messages from external channels (Discord, Telegram) appear in
	// the web chat — both live and after a refresh. This mirrors the
	// persistence + SSE pattern used by handleAgentMessage.
	storeMsg := &store.Message{
		ID:            brokerInboundMsgID,
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
	// Stamp the effectiveConv — the same value the envelope carried.
	if effectiveConv != nil {
		storeMsg.ConversationID = effectiveConv.ConversationID
	}
	// Divergence logging and consistency check.
	if !storeMsg.Broadcasted {
		oldRouting := messaging.OldRoutingFromMessage(senderUserID, agent.ID, storeMsg.ThreadID)
		convID := ""
		actualRef := ""
		if effectiveConv != nil {
			convID = effectiveConv.ConversationID
			actualRef = effectiveConv.ExternalRef
		}
		match, reason := messaging.ComputeDivergenceMatch(oldRouting, actualRef, convID)
		messaging.LogDivergence(log, messaging.DivergenceEntry{
			MessageID:  storeMsg.ID,
			OldRouting: oldRouting,
			NewRouting: messaging.NewRoutingStr(convID),
			Match:      match,
			Reason:     reason,
		})
		// DEF-3: Independent consistency check against prior messages.
		messaging.CheckConversationConsistency(r.Context(), s.store, storeMsg.ID, convID, storeMsg.ThreadID, senderUserID, agent.ID, log)
	}
	if err := s.store.CreateMessage(r.Context(), storeMsg); err != nil {
		log.Error("Failed to persist inbound broker message",
			"error", err,
			"message_id", storeMsg.ID,
			"conversation_id", storeMsg.ConversationID,
			"agent_id", agent.ID,
		)
		// Non-fatal: the dispatch already succeeded, so the agent got the
		// message. The agent now holds identifiers (message_id,
		// conversation_id) that reference an unpersisted row. Failing the
		// HTTP response here would mislead the caller into retrying —
		// which would double-deliver.
	} else {
		s.events.PublishUserMessage(r.Context(), storeMsg)
	}

	// Record reply-affinity context so that the agent's next untagged reply
	// can be routed back to the channel the user last spoke from (AC22).
	// Only record for user-identity senders with a known channel.
	s.mu.RLock()
	wcsAffinity := s.webChatStore
	s.mu.RUnlock()
	if wcsAffinity != nil && req.Message.Channel != "" && strings.HasPrefix(req.Message.Sender, "user:") {
		if senderUserID != "" {
			if err := wcsAffinity.RecordChannel(r.Context(), senderUserID, agent.ProjectID, agent.ID, req.Message.Channel, now); err != nil {
				log.Error("Failed to record conversation context for broker inbound",
					"user_id", senderUserID, "agent_id", agent.ID, "channel", req.Message.Channel, "error", err)
			}
			// Update the thread watermark so the Phase 5 thread rail reflects
			// inbound broker messages (last_activity_at / last_message_id).
			if err := wcsAffinity.TouchThread(r.Context(), senderUserID, agent.ProjectID, agent.ID, storeMsg.ID, now); err != nil {
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

// resolveSenderUserID resolves the sender's user ID from the message fields.
// If senderID is already set, it is returned as-is. Otherwise, if sender has
// a "user:" prefix, the user is looked up by email. Returns the empty string
// when the sender cannot be resolved (non-user sender, or user not found).
func resolveSenderUserID(ctx context.Context, st store.Store, senderID, sender string) string {
	if senderID != "" {
		return senderID
	}
	if strings.HasPrefix(sender, "user:") {
		senderEmail := strings.TrimPrefix(sender, "user:")
		if u, err := st.GetUserByEmail(ctx, senderEmail); err == nil && u != nil {
			return u.ID
		}
	}
	return ""
}

// resolvePhase5Conversation resolves or creates a conversation for a
// broker-inbound message using the Phase 5 dual-write path. Thread-based
// messages are resolved via ResolveOrCreateThreadConversation; non-thread
// messages resolve as DM conversations between the sender and agent.
//
// Returns (nil, nil) when resolution is inapplicable (e.g. missing
// senderUserID for a DM, or empty agentID).
// Returns (result, nil) on success.
// Returns (nil, err) when resolution fails — the caller must handle
// write-deny semantics.
func (s *Server) resolvePhase5Conversation(
	ctx context.Context,
	threadID, projectID, senderUserID, agentID string,
) (*messaging.ConversationResult, error) {
	if threadID != "" {
		var threadOpts []messaging.ThreadConversationOption
		s.mu.RLock()
		wcs := s.webChatStore
		s.mu.RUnlock()
		if wcs != nil {
			threadOpts = append(threadOpts, messaging.WithTopicLookup(wcs))
		}
		return messaging.ResolveOrCreateThreadConversation(ctx, s.store, s.messageLog, threadID, projectID, threadOpts...)
	}
	if senderUserID != "" && agentID != "" {
		return messaging.ResolveOrCreateDMConversation(ctx, s.store, s.store, s.messageLog, "user", senderUserID, "agent", agentID)
	}
	return nil, nil
}
