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
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/GoogleCloudPlatform/scion/pkg/plugin"
)

// handleProjectDiscord routes /api/v1/projects/{projectId}/discord/... requests.
func (s *Server) handleProjectDiscord(w http.ResponseWriter, r *http.Request, projectID, discordPath string) {
	mgr := s.pluginManager
	if mgr == nil {
		writeError(w, http.StatusServiceUnavailable, "plugin_unavailable", "integration manager not available", nil)
		return
	}

	switch {
	case discordPath == "channels" && r.Method == http.MethodGet:
		s.handleDiscordChannels(w, r, projectID, mgr)
	case discordPath == "threads" && r.Method == http.MethodGet:
		s.handleDiscordThreads(w, r, projectID, mgr)
	case discordPath == "default" && r.Method == http.MethodPut:
		s.handleDiscordSetDefault(w, r, projectID, mgr)
	case strings.HasPrefix(discordPath, "channels/") && strings.HasSuffix(discordPath, "/history") && r.Method == http.MethodGet:
		// Extract channelId from "channels/{channelId}/history"
		trimmed := strings.TrimPrefix(discordPath, "channels/")
		channelID := strings.TrimSuffix(trimmed, "/history")
		if channelID == "" {
			writeError(w, http.StatusBadRequest, "missing_channel_id", "channel_id is required in the URL path", nil)
			return
		}
		s.handleDiscordHistory(w, r, projectID, channelID, mgr)
	case discordPath == "dm" && r.Method == http.MethodPost:
		s.handleDiscordDM(w, r, projectID, mgr)
	default:
		NotFound(w, "discord endpoint")
	}
}

func (s *Server) handleDiscordChannels(w http.ResponseWriter, r *http.Request, projectID string, mgr IntegrationManager) {
	params, _ := json.Marshal(map[string]string{"project_id": projectID})

	result, err := mgr.BrokerQuery(r.Context(), "discord", "list-channels", params)
	if err != nil {
		s.writeDiscordError(w, err)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	w.Write(result)
}

func (s *Server) handleDiscordThreads(w http.ResponseWriter, r *http.Request, projectID string, mgr IntegrationManager) {
	channelID := r.URL.Query().Get("channel_id")

	reqMap := map[string]string{"project_id": projectID}
	if channelID != "" {
		reqMap["channel_id"] = channelID
	}
	params, _ := json.Marshal(reqMap)

	result, err := mgr.BrokerQuery(r.Context(), "discord", "list-threads", params)
	if err != nil {
		s.writeDiscordError(w, err)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	w.Write(result)
}

func (s *Server) handleDiscordSetDefault(w http.ResponseWriter, r *http.Request, projectID string, mgr IntegrationManager) {
	var body struct {
		ChannelID string `json:"channel_id"`
		ThreadID  string `json:"thread_id,omitempty"`
		AgentSlug string `json:"agent_slug"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_body", "invalid request body", nil)
		return
	}
	if body.ChannelID == "" || body.AgentSlug == "" {
		writeError(w, http.StatusBadRequest, "missing_fields", "channel_id and agent_slug are required", nil)
		return
	}

	reqMap := map[string]string{
		"project_id": projectID,
		"channel_id": body.ChannelID,
		"agent_slug": body.AgentSlug,
	}
	if body.ThreadID != "" {
		reqMap["thread_id"] = body.ThreadID
	}
	params, _ := json.Marshal(reqMap)

	result, err := mgr.BrokerQuery(r.Context(), "discord", "set-default", params)
	if err != nil {
		s.writeDiscordError(w, err)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	w.Write(result)
}

func (s *Server) handleDiscordHistory(w http.ResponseWriter, r *http.Request, projectID, channelID string, mgr IntegrationManager) {
	q := r.URL.Query()
	reqMap := map[string]interface{}{
		"project_id": projectID,
		"channel_id": channelID,
	}
	if v := q.Get("limit"); v != "" {
		limit, err := strconv.Atoi(v)
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid_limit", "limit must be a valid integer", nil)
			return
		}
		reqMap["limit"] = limit
	}
	if v := q.Get("before"); v != "" {
		reqMap["before"] = v
	}
	if v := q.Get("after"); v != "" {
		reqMap["after"] = v
	}
	if q.Get("humans_only") == "true" {
		reqMap["humans_only"] = true
	}
	params, _ := json.Marshal(reqMap)

	result, err := mgr.BrokerQuery(r.Context(), "discord", "channel-history", params)
	if err != nil {
		s.writeDiscordError(w, err)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	w.Write(result)
}

func (s *Server) handleDiscordDM(w http.ResponseWriter, r *http.Request, projectID string, mgr IntegrationManager) {
	var body struct {
		RecipientEmail string `json:"recipient_email"`
		Message        string `json:"message"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_body", "invalid request body", nil)
		return
	}
	if body.RecipientEmail == "" || body.Message == "" {
		writeError(w, http.StatusBadRequest, "missing_fields", "recipient_email and message are required", nil)
		return
	}

	params, _ := json.Marshal(map[string]string{
		"project_id":      projectID,
		"recipient_email": body.RecipientEmail,
		"message":         body.Message,
	})

	result, err := mgr.BrokerQuery(r.Context(), "discord", "send-dm", params)
	if err != nil {
		s.writeDiscordError(w, err)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	w.Write(result)
}

// writeDiscordError maps plugin errors to HTTP status codes.
func (s *Server) writeDiscordError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, plugin.ErrUnsupportedOperation):
		writeError(w, http.StatusBadRequest, "unsupported_operation", err.Error(), nil)
	case errors.Is(err, plugin.ErrPluginUnavailable):
		writeError(w, http.StatusServiceUnavailable, "plugin_unavailable", err.Error(), nil)
	case errors.Is(err, plugin.ErrRateLimited):
		writeError(w, http.StatusTooManyRequests, "rate_limited", err.Error(), nil)
	case errors.Is(err, plugin.ErrNotFound):
		writeError(w, http.StatusNotFound, "not_found", err.Error(), nil)
	case errors.Is(err, plugin.ErrForbidden):
		writeError(w, http.StatusForbidden, "forbidden", err.Error(), nil)
	default:
		writeError(w, http.StatusInternalServerError, "internal", "internal server error", nil)
	}
}
