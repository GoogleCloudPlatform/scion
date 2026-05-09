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
	"fmt"
	"net/http"
	"strings"

	"github.com/GoogleCloudPlatform/scion/pkg/messages"
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
// The topic string is parsed to extract the project ID and agent slug using the
// standard topic format: scion.project.<projectID>.agent.<agentSlug>.messages
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
		writeErrorFromErr(w, err, "")
		return
	}

	// Dispatch directly to the agent, bypassing the broker to avoid circular delivery
	dispatcher := s.GetDispatcher()
	if dispatcher == nil {
		writeError(w, http.StatusServiceUnavailable, ErrCodeUnavailable,
			"no dispatcher available", nil)
		return
	}

	if err := dispatcher.DispatchAgentMessage(r.Context(), agent, req.Message.Msg, req.Message.Urgent, req.Message); err != nil {
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
// Expected format: scion.project.<projectID>.agent.<agentSlug>.messages
func parseAgentMessageTopic(topic string) (projectID, agentSlug string, err error) {
	parts := strings.Split(topic, ".")
	// scion.project.<projectID>.agent.<agentSlug>.messages = 6 parts
	if len(parts) != 6 {
		return "", "", fmt.Errorf("expected format scion.project.<projectId>.agent.<agentSlug>.messages, got %d segments", len(parts))
	}
	if parts[0] != "scion" || parts[1] != "project" || parts[3] != "agent" || parts[5] != "messages" {
		return "", "", fmt.Errorf("expected format scion.project.<projectId>.agent.<agentSlug>.messages")
	}
	return parts[2], parts[4], nil
}
