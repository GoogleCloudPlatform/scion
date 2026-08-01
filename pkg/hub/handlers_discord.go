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
		writeError(w, http.StatusInternalServerError, "internal", err.Error(), nil)
	}
}
