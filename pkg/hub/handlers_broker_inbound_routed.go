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
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// routedInboundRequest is the JSON body for POST /api/v1/broker/inbound/routed.
type routedInboundRequest struct {
	ProjectID    string                      `json:"project_id"`
	DefaultAgent string                      `json:"default_agent,omitempty"` // slug, not UUID
	Surface      string                      `json:"surface,omitempty"`
	ExternalRef  string                      `json:"external_ref,omitempty"`
	ParentRef    string                      `json:"parent_ref,omitempty"`
	Message      *messages.StructuredMessage `json:"message"`
}

// routedInboundResponse is returned from POST /api/v1/broker/inbound/routed.
type routedInboundResponse struct {
	Delivered          bool                   `json:"delivered"`
	PrimaryAgent       string                 `json:"primary_agent"`
	Results            []routedDeliveryResult `json:"results"`
	UnresolvedMentions []string               `json:"unresolved_mentions,omitempty"`
	MentionErrors      []routedMentionError   `json:"mention_errors,omitempty"`
}

type routedDeliveryResult struct {
	AgentSlug          string `json:"agent_slug"`
	Type               string `json:"type"`   // "message" or "mention"
	Status             string `json:"status"` // delivered, unauthorized, not_running, conversation_not_resolved, error, not_attempted
	MessageID          string `json:"message_id,omitempty"`
	Error              string `json:"error,omitempty"`
	PersistenceWarning string `json:"persistence_warning,omitempty"`
}

type routedMentionError struct {
	Slug  string `json:"slug"`
	Error string `json:"error"`
}

// handleBrokerInboundRouted handles POST /api/v1/broker/inbound/routed.
// This endpoint uses the shared recipient planner to resolve routing from
// message content, then dispatches to each recipient with per-agent
// authorization, conversation resolution, and structured results.
func (s *Server) handleBrokerInboundRouted(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		MethodNotAllowed(w, http.MethodPost)
		return
	}

	// Require broker HMAC authentication.
	broker := GetBrokerIdentityFromContext(r.Context())
	if broker == nil {
		writeError(w, http.StatusUnauthorized, ErrCodeBrokerAuthFailed,
			"broker HMAC authentication required", nil)
		return
	}

	pluginName := r.Header.Get("X-Scion-Plugin-Name")
	log := s.messageLog.With(
		"broker_id", broker.ID(),
		"plugin_name", pluginName,
		"endpoint", "broker.inbound.routed",
	)

	// Parse request body.
	var req routedInboundRequest
	if err := readJSON(r, &req); err != nil {
		BadRequest(w, "invalid request body: "+err.Error())
		return
	}

	// --- Input validation ---
	if req.ProjectID == "" {
		ValidationError(w, "project_id is required", map[string]interface{}{"field": "project_id"})
		return
	}
	if req.Message == nil {
		ValidationError(w, "message is required", map[string]interface{}{"field": "message"})
		return
	}
	if !strings.HasPrefix(req.Message.Sender, "user:") {
		writeError(w, http.StatusBadRequest, ErrCodeValidationError,
			"sender must use user: prefix for mapped identity", nil)
		return
	}
	// Reject broadcasts and dm:-prefixed thread IDs on this endpoint.
	if req.Message.Broadcasted {
		writeError(w, http.StatusBadRequest, ErrCodeValidationError,
			"broadcasted messages must use legacy inbound", nil)
		return
	}
	if req.Message.ThreadID != "" && strings.HasPrefix(req.Message.ThreadID, "dm:") {
		writeError(w, http.StatusBadRequest, ErrCodeValidationError,
			"dm:-prefixed thread IDs must use legacy inbound", nil)
		return
	}
	// external_ref requires surface; parent_ref requires external_ref.
	if req.ExternalRef != "" && req.Surface == "" {
		writeError(w, http.StatusBadRequest, ErrCodeValidationError,
			"external_ref requires surface to be set", nil)
		return
	}
	if req.ParentRef != "" && req.ExternalRef == "" {
		writeError(w, http.StatusBadRequest, ErrCodeValidationError,
			"parent_ref requires external_ref to be set", nil)
		return
	}

	// --- Resolve sender identity ---
	senderEmail := strings.TrimPrefix(req.Message.Sender, "user:")
	senderUser, err := s.store.GetUserByEmail(r.Context(), senderEmail)
	if err != nil {
		log.Warn("Could not resolve sender identity",
			"sender", req.Message.Sender, "error", err)
		if errors.Is(err, store.ErrNotFound) {
			writeError(w, http.StatusForbidden, ErrCodeForbidden,
				"sender identity could not be resolved",
				map[string]interface{}{"sender": req.Message.Sender})
		} else {
			writeError(w, http.StatusInternalServerError, ErrCodeInternalError,
				"internal error resolving sender identity", nil)
		}
		return
	}
	// C2a containment: require active status.
	if senderUser.Status != store.UserStatusActive {
		log.Warn("routed inbound sender is not active",
			"sender", req.Message.Sender, "status", senderUser.Status)
		writeError(w, http.StatusForbidden, ErrCodeForbidden,
			"sender identity is not active",
			map[string]interface{}{"sender": req.Message.Sender, "status": senderUser.Status})
		return
	}
	req.Message.SenderID = senderUser.ID
	senderIdentity := NewAuthenticatedUser(senderUser.ID, senderUser.Email, senderUser.DisplayName, senderUser.Role, "integration")

	// --- Strip leading "!" interrupt prefix ---
	if trimmed := strings.TrimSpace(req.Message.Msg); strings.HasPrefix(trimmed, "!") {
		content := strings.TrimSpace(trimmed[1:])
		if content == "" {
			content = "interrupt"
		}
		req.Message.Msg = content
		req.Message.Urgent = true
	}

	// --- Resolve default agent ---
	var defaultAgent *store.Agent
	if req.DefaultAgent != "" {
		da, daErr := s.store.GetAgentBySlug(r.Context(), req.ProjectID, req.DefaultAgent)
		if daErr != nil || da == nil {
			// Missing configured default is treated as absent, with a diagnostic.
			log.Warn("configured default_agent not found",
				"slug", req.DefaultAgent, "project_id", req.ProjectID)
		} else if !da.DeletedAt.IsZero() {
			log.Warn("configured default_agent is deleted",
				"slug", req.DefaultAgent, "project_id", req.ProjectID)
		} else {
			defaultAgent = da
		}
	}

	// --- Resolve routing via shared planner ---
	plan, planErr := resolveRoutingAgents(r.Context(), s.store, req.ProjectID, req.Message.Msg, defaultAgent)
	if planErr != nil {
		log.Error("agent routing resolution failed", "error", planErr)
		writeError(w, http.StatusInternalServerError, ErrCodeInternalError,
			"failed to resolve routing agents", nil)
		return
	}
	if len(plan.Agents) == 0 {
		writeError(w, http.StatusUnprocessableEntity, "no_routing_recipient",
			"no routing recipient could be resolved", nil)
		return
	}

	// --- Validate message body/attachments ---
	// Build a synthetic message for validation (channel, thread_id are from the
	// adapter payload). The recipient will be set per-agent below.
	if req.Message.Msg == "" && len(req.Message.Attachments) == 0 {
		ValidationError(w, "message body or attachments required", nil)
		return
	}

	now := time.Now().UTC()
	primaryAgent := plan.Agents[0]

	// Build response structures.
	resp := routedInboundResponse{
		PrimaryAgent:       primaryAgent.Slug,
		UnresolvedMentions: plan.UnresolvedMentions,
	}
	results := make([]routedDeliveryResult, 0, len(plan.Agents))

	// Collect mention errors from resolution.
	for _, mr := range plan.MentionResults {
		if mr.Status == "error" {
			resp.MentionErrors = append(resp.MentionErrors, routedMentionError{
				Slug:  mr.Slug,
				Error: mr.Error,
			})
		}
	}

	// --- Remove incoming mention routing metadata ---
	// The hub owns mention_co_addressees and group recipient fields; remove
	// any client-supplied values before creating deliveries.
	if req.Message.Metadata != nil {
		delete(req.Message.Metadata, "mention_co_addressees")
		delete(req.Message.Metadata, "group_id")
		delete(req.Message.Metadata, "mention_source")
		delete(req.Message.Metadata, "mention_position")
	}

	// Build co-addressees for envelope rendering (when multiple recipients).
	var coAddressees []messaging.Addressee
	if len(plan.Agents) > 1 {
		coAddressees = groupCoAddressees(plan.Agents)
	}

	// --- Primary agent: authorize, resolve conversation, dispatch, persist ---
	primaryResult := s.dispatchRoutedRecipient(r.Context(), dispatchRoutedParams{
		agent:        primaryAgent,
		isPrimary:    true,
		sender:       senderIdentity,
		req:          &req,
		plan:         &plan,
		coAddressees: coAddressees,
		now:          now,
	})
	results = append(results, primaryResult)

	if primaryResult.Status != "delivered" {
		// Primary dispatch failed; no secondaries.
		for _, agent := range plan.Agents[1:] {
			results = append(results, routedDeliveryResult{
				AgentSlug: agent.Slug,
				Type:      "mention",
				Status:    "not_attempted",
			})
		}
		resp.Delivered = false
		resp.Results = results

		// Determine error code from primary failure.
		switch primaryResult.Status {
		case "unauthorized":
			writeError(w, http.StatusForbidden, ErrCodeMessageDenied,
				"primary agent message authorization denied",
				map[string]interface{}{"agent_slug": primaryAgent.Slug, "results": results})
			return
		case "not_running":
			writeError(w, http.StatusConflict, ErrCodeAgentNotRunning,
				fmt.Sprintf("primary agent %q is not running", primaryAgent.Slug),
				map[string]interface{}{"results": results})
			return
		case "conversation_not_resolved":
			writeError(w, http.StatusConflict, ErrCodeConversationNotResolved,
				"conversation resolution failed for primary agent",
				map[string]interface{}{"results": results})
			return
		default:
			// Dispatch error.
			status := http.StatusBadGateway
			code := ErrCodeRuntimeError
			if primaryResult.Error == "no dispatcher available" {
				status = http.StatusServiceUnavailable
				code = ErrCodeUnavailable
			} else if primaryResult.Error != "" && strings.Contains(primaryResult.Error, "30s deadline") {
				status = http.StatusGatewayTimeout
				code = ErrCodeBrokerTimeout
			}
			writeError(w, status, code,
				"primary agent dispatch failed: "+primaryResult.Error,
				map[string]interface{}{"results": results})
			return
		}
	}

	// --- Secondary agents: authorize, resolve, dispatch, persist each ---
	for _, agent := range plan.Agents[1:] {
		result := s.dispatchRoutedRecipient(r.Context(), dispatchRoutedParams{
			agent:            agent,
			isPrimary:        false,
			sender:           senderIdentity,
			req:              &req,
			plan:             &plan,
			coAddressees:     coAddressees,
			now:              now,
			primaryRecipient: "agent:" + primaryAgent.Slug,
		})
		results = append(results, result)
	}

	resp.Delivered = true
	resp.Results = results
	writeJSON(w, http.StatusOK, resp)
}

// dispatchRoutedParams holds parameters for a single recipient dispatch.
type dispatchRoutedParams struct {
	agent            *store.Agent
	isPrimary        bool
	sender           Identity
	req              *routedInboundRequest
	plan             *RoutingPlan
	coAddressees     []messaging.Addressee
	now              time.Time
	primaryRecipient string // "agent:<slug>" of primary, for mention metadata
}

// dispatchRoutedRecipient handles authorization, conversation resolution,
// message construction, dispatch, and persistence for a single recipient
// in the routed inbound flow.
func (s *Server) dispatchRoutedRecipient(
	ctx context.Context,
	params dispatchRoutedParams,
) routedDeliveryResult {
	agent := params.agent
	msgType := "mention"
	if params.isPrimary {
		msgType = "message"
	}

	result := routedDeliveryResult{
		AgentSlug: agent.Slug,
		Type:      msgType,
	}

	// --- Authorization ---
	allowed, reason := s.authorizeAgentMessage(ctx, params.sender, agent, false)
	if !allowed {
		s.messageLog.Warn("routed inbound authorization denied",
			"agent_slug", agent.Slug, "reason", reason)
		result.Status = "unauthorized"
		result.Error = reason
		return result
	}

	// --- Phase check ---
	if phase := state.Phase(agent.Phase); phase != state.PhaseRunning {
		result.Status = "not_running"
		result.Error = fmt.Sprintf("agent is %s", agent.Phase)
		return result
	}

	// --- Dispatcher availability ---
	// Check before conversation resolution to avoid creating orphan
	// conversation rows when no dispatcher is available.
	dispatcher := s.GetDispatcher()
	if dispatcher == nil {
		result.Status = "error"
		result.Error = "no dispatcher available"
		return result
	}

	// --- Build structured message ---
	var msg *messages.StructuredMessage
	if params.isPrimary {
		msg = &messages.StructuredMessage{
			Version:     messages.Version,
			Timestamp:   params.now.Format(time.RFC3339),
			Sender:      params.req.Message.Sender,
			SenderID:    params.req.Message.SenderID,
			Recipient:   "agent:" + agent.Slug,
			RecipientID: agent.ID,
			Msg:         params.req.Message.Msg,
			Type:        messages.TypeInstruction,
			Urgent:      params.req.Message.Urgent,
			Channel:     params.req.Message.Channel,
			ThreadID:    params.req.Message.ThreadID,
		}
	} else {
		msg = messages.NewMention(
			params.req.Message.Sender,
			"agent:"+agent.Slug,
			params.req.Message.Msg,
			params.primaryRecipient,
		)
		// Override NewMention's time.Now() with the shared arrival timestamp
		// so all recipients see one consistent arrival time (design step 6).
		msg.Timestamp = params.now.Format(time.RFC3339)
		msg.SenderID = params.req.Message.SenderID
		msg.RecipientID = agent.ID
		msg.Urgent = params.req.Message.Urgent
		msg.Channel = params.req.Message.Channel
		msg.ThreadID = params.req.Message.ThreadID
	}

	// Copy visibility, transport flags, and supported attachments.
	// Deep-copy the slice so recipients don't share the underlying array.
	if len(params.req.Message.Attachments) > 0 {
		msg.Attachments = make([]string, len(params.req.Message.Attachments))
		copy(msg.Attachments, params.req.Message.Attachments)
	}
	// Copy metadata but ensure hub-owned fields are excluded.
	if params.req.Message.Metadata != nil {
		if msg.Metadata == nil {
			msg.Metadata = make(map[string]string)
		}
		for k, v := range params.req.Message.Metadata {
			switch k {
			case "mention_co_addressees", "group_id", "mention_source", "mention_position":
				continue
			}
			msg.Metadata[k] = v
		}
	}

	// --- Validate through envelope choke point ---
	if err := messaging.ValidateLegacyMessage(msg); err != nil {
		result.Status = "error"
		result.Error = "validation failed: " + err.Error()
		return result
	}

	// --- Allocate message UUID ---
	msgID := api.NewUUID()
	result.MessageID = msgID

	// --- Conversation resolution ---
	// Phase 11 (explicit surface + external_ref) takes precedence over Phase 5.
	var effectiveConv *messaging.ConversationResult

	if params.req.Surface != "" && params.req.ExternalRef != "" {
		var keyOpts []messaging.ConversationByKeyOption
		keyOpts = append(keyOpts, messaging.WithSurface(params.req.Surface))
		if params.req.ParentRef != "" {
			keyOpts = append(keyOpts, messaging.WithParentRef(params.req.ParentRef))
		}
		agentID := agent.ID
		keyOpts = append(keyOpts, messaging.WithDefaultAgentID(&agentID))
		s.mu.RLock()
		wcs := s.webChatStore
		s.mu.RUnlock()
		if wcs != nil {
			keyOpts = append(keyOpts, messaging.WithKeyTopicLookup(wcs))
		}
		convResult, convErr := messaging.ResolveOrCreateConversationByKey(
			ctx, s.store, s.messageLog, params.req.ExternalRef, "group", &agent.ProjectID, keyOpts...)
		if convErr != nil {
			if s.writeDenyEnabled() {
				messaging.WriteDenialMetrics.Inc("broker.routed.phase11")
				result.Status = "conversation_not_resolved"
				result.Error = "conversation resolution failed"
				return result
			}
			s.messageLog.Warn("conversation resolution failed (write-deny OFF, continuing)",
				"agent_slug", agent.Slug, "error", convErr)
		} else {
			effectiveConv = convResult
		}
	}

	// Phase 5: inferred DM/thread conversation.
	senderUserID := params.req.Message.SenderID
	if effectiveConv == nil {
		convFromPhase5, convErr := s.resolvePhase5Conversation(
			ctx, params.req.Message.ThreadID, params.req.ProjectID, senderUserID, agent.ID, params.req.Message.Channel)
		if convErr != nil {
			metricKey := "broker.routed.dm"
			if params.req.Message.ThreadID != "" {
				metricKey = "broker.routed.thread"
			}
			if s.writeDenyEnabled() {
				messaging.WriteDenialMetrics.Inc(metricKey)
				result.Status = "conversation_not_resolved"
				result.Error = "conversation resolution failed"
				return result
			}
			s.messageLog.Warn("Phase 5 conversation resolution failed (write-deny OFF, continuing)",
				"agent_slug", agent.Slug, "error", convErr)
		} else {
			effectiveConv = convFromPhase5
		}
	}

	// Validate attributed conversation.
	if effectiveConv != nil {
		if err := messaging.ValidateAttributed(effectiveConv.ConversationID); err != nil {
			if s.writeDenyEnabled() {
				messaging.WriteDenialMetrics.Inc("broker.routed.validate")
				result.Status = "conversation_not_resolved"
				result.Error = err.Error()
				return result
			}
			s.messageLog.Warn("ValidateAttributed failed (write-deny OFF, continuing)",
				"agent_slug", agent.Slug, "error", err)
		}
	}

	// --- Render delivery envelope ---
	if s.writeDenyEnabled() {
		renderInput := messaging.RenderDeliveryInput{
			MessageID:  msgID,
			ConvResult: effectiveConv,
			Msg:        msg,
			CreatedAt:  params.now,
			IsMention:  !params.isPrimary,
		}
		if len(params.coAddressees) > 0 {
			renderInput.CoAddressees = params.coAddressees
		}
		msg.DeliveryText = messaging.RenderDeliveryText(renderInput)
	}

	// --- Dispatch ---
	retryCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	if err := dispatchWithBrokerRetry(retryCtx, dispatcher, agent, msg.Msg, msg.Urgent, msg); errors.Is(err, ErrBrokerTimeout) {
		result.Status = "error"
		result.Error = "broker unreachable after 30s deadline"
		return result
	} else if err != nil {
		s.messageLog.Error("routed inbound dispatch failed",
			"agent_slug", agent.Slug, "error", err)
		result.Status = "error"
		result.Error = "dispatch failed: " + err.Error()
		return result
	}

	// --- Persist ---
	storeMsg := &store.Message{
		ID:            msgID,
		ProjectID:     params.req.ProjectID,
		Sender:        msg.Sender,
		SenderID:      senderUserID,
		Recipient:     msg.Recipient,
		RecipientID:   agent.ID,
		Msg:           msg.Msg,
		Type:          msg.Type,
		Urgent:        msg.Urgent,
		AgentID:       agent.ID,
		Channel:       msg.Channel,
		ThreadID:      msg.ThreadID,
		DispatchState: store.MessageDispatchDispatched,
		CreatedAt:     params.now,
	}
	if effectiveConv != nil {
		storeMsg.ConversationID = effectiveConv.ConversationID
	}
	if err := s.store.CreateMessage(ctx, storeMsg); err != nil {
		s.messageLog.Error("Failed to persist routed inbound message",
			"error", err, "message_id", msgID, "agent_slug", agent.Slug)
		// Persistence failure after dispatch is nonfatal — dispatch succeeded.
		result.PersistenceWarning = "message dispatched but persistence failed: " + err.Error()
	} else {
		s.events.PublishUserMessage(ctx, storeMsg)
	}

	// --- Reply affinity ---
	s.mu.RLock()
	wcsAffinity := s.webChatStore
	s.mu.RUnlock()
	if wcsAffinity != nil && msg.Channel != "" && senderUserID != "" {
		if err := wcsAffinity.RecordChannel(ctx, senderUserID, params.req.ProjectID, agent.ID, msg.Channel, params.now); err != nil {
			s.messageLog.Error("Failed to record channel for routed inbound",
				"user_id", senderUserID, "agent_id", agent.ID, "error", err)
		}
		if err := wcsAffinity.TouchThread(ctx, senderUserID, params.req.ProjectID, agent.ID, storeMsg.ID, params.now); err != nil {
			s.messageLog.Error("Failed to update thread watermark for routed inbound",
				"user_id", senderUserID, "agent_id", agent.ID, "error", err)
		}
	}

	// --- Audit log ---
	if s.dedicatedMessageLog != nil {
		s.dedicatedMessageLog.Info("routed inbound message delivered",
			"agent_id", agent.ID,
			"agent_slug", agent.Slug,
			"project_id", params.req.ProjectID,
			"message_id", msgID,
			"source", "broker-inbound-routed",
			"type", msg.Type,
		)
	}

	result.Status = "delivered"
	return result
}
