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
	"net/http"
)

// validVisibilityModes is the set of valid visibility mode values.
var validVisibilityModes = map[string]bool{
	"conversation": true,
	"verbose":      true,
	"full":         true,
}

// DEPRECATED(wave-1): Remove after v2 is stable and flag is permanently ON.
//
// handleChatPrefs handles GET and PUT /api/v1/chat/prefs.
//
// Query parameters (required):
//   - agentId: the agent ID for the thread
//
// The project ID is derived from the agent, so the client only needs to
// supply the agent ID. The user ID comes from the session.
//
// GET returns the current prefs (or defaults if none saved).
// PUT accepts {"visibility_mode": "conversation"|"verbose"|"full"} and
// upserts the pref row.
//
// Writes to webchat_thread_prefs (wave-1 table). V2 does not call this
// endpoint — visibility mode is not used in the v2 conversation view.
func (s *Server) handleChatPrefs(w http.ResponseWriter, r *http.Request) {
	user := GetUserIdentityFromContext(r.Context())
	if user == nil {
		Forbidden(w)
		return
	}

	// Read webChatStore under a read lock to avoid a data race with
	// SetWebChatStore, which writes under a write lock.
	s.mu.RLock()
	webChatStore := s.webChatStore
	s.mu.RUnlock()

	if webChatStore == nil {
		writeError(w, http.StatusServiceUnavailable, "SERVICE_UNAVAILABLE", "Chat preferences not available", nil)
		return
	}

	agentID := r.URL.Query().Get("agentId")
	if agentID == "" {
		writeError(w, http.StatusBadRequest, "INVALID_ARGUMENT", "agentId query parameter is required", nil)
		return
	}

	// Look up the agent to get projectId and verify access.
	ctx := r.Context()
	agent, err := s.store.GetAgent(ctx, agentID)
	if err != nil {
		writeErrorFromErr(w, err, "Agent")
		return
	}

	// Require at least read access to the agent.
	res := agentResource(agent)
	decision := s.authzService.CheckAccess(ctx, user, res, ActionRead)
	if !decision.Allowed {
		writeError(w, http.StatusForbidden, ErrCodeForbidden, "Access denied", nil)
		return
	}

	switch r.Method {
	case http.MethodGet:
		prefs, err := webChatStore.GetThreadPrefs(ctx, user.ID(), agent.ProjectID, agentID)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "INTERNAL", "Failed to read preferences", nil)
			return
		}
		writeJSON(w, http.StatusOK, prefs)

	case http.MethodPut:
		// Limit request body size to prevent DoS via oversized payloads.
		r.Body = http.MaxBytesReader(w, r.Body, 1048576) // 1MB limit

		var body struct {
			VisibilityMode string `json:"visibility_mode"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			writeError(w, http.StatusBadRequest, "INVALID_ARGUMENT", "Invalid JSON body", nil)
			return
		}
		if !validVisibilityModes[body.VisibilityMode] {
			writeError(w, http.StatusBadRequest, "INVALID_ARGUMENT", "visibility_mode must be conversation, verbose, or full", nil)
			return
		}

		prefs := ThreadPrefs{VisibilityMode: body.VisibilityMode}
		if err := webChatStore.SetThreadPrefs(ctx, user.ID(), agent.ProjectID, agentID, prefs); err != nil {
			writeError(w, http.StatusInternalServerError, "INTERNAL", "Failed to save preferences", nil)
			return
		}
		writeJSON(w, http.StatusOK, prefs)

	default:
		MethodNotAllowed(w)
	}
}
