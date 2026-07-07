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

	"github.com/GoogleCloudPlatform/scion/pkg/messages"
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

	// Enforce ActionAttach permission for user-identity senders. Agent-identity
	// and system senders (scheduled events, internal) skip this check — they
	// use broker HMAC trust which is infrastructure-level authorization.
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
		userIdent := NewAuthenticatedUser(senderUser.ID, senderUser.Email, senderUser.DisplayName, senderUser.Role, "integration")
		decision := s.authzService.CheckAccess(r.Context(), userIdent, agentResource(agent), ActionAttach)
		if !decision.Allowed {
			log.Warn("User lacks permission to message agent via integration",
				"sender", req.Message.Sender, "agent_slug", agentSlug, "reason", decision.Reason)
			writeError(w, http.StatusForbidden, ErrCodeForbidden,
				"user does not have permission to message this agent", map[string]interface{}{
					"sender":     req.Message.Sender,
					"agent_slug": agentSlug,
				})
			return
		}
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
