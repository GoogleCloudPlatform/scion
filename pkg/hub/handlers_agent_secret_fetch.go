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
	"errors"
	"log/slog"
	"net/http"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// secretFetchRequest is the request body for POST /api/v1/agent/secrets.
// The client sends a list of secret key names to retrieve.
type secretFetchRequest struct {
	Keys []string `json:"keys"`
}

// secretFetchResponse is the response body for POST /api/v1/agent/secrets.
type secretFetchResponse struct {
	Secrets []secretFetchResult `json:"secrets"`
}

// secretFetchResult represents the resolution status of a single secret key.
type secretFetchResult struct {
	Key    string `json:"key"`
	Value  string `json:"value,omitempty"`
	Status string `json:"status"`
	Error  string `json:"error,omitempty"`
}

// handleAgentSecretFetch handles POST /api/v1/agent/secrets.
// Called by the agent client (FetchSecrets) to retrieve secret values by key.
// Authenticates via X-Scion-Agent-Token and returns secrets scoped to the
// agent's project.
func (s *Server) handleAgentSecretFetch(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		MethodNotAllowed(w)
		return
	}

	agent := GetAgentFromContext(r.Context())
	if agent == nil {
		writeError(w, http.StatusForbidden, ErrCodeForbidden, "agent authentication required", nil)
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, 64*1024) // 64 KB payload limit

	var req secretFetchRequest
	if err := readJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, ErrCodeInvalidRequest, "invalid request body: "+err.Error(), nil)
		return
	}

	if len(req.Keys) == 0 {
		writeError(w, http.StatusBadRequest, ErrCodeInvalidRequest, "keys must not be empty", nil)
		return
	}

	if len(req.Keys) > 100 {
		writeError(w, http.StatusBadRequest, ErrCodeInvalidRequest, "too many keys requested (max 100)", nil)
		return
	}

	// Look up the agent record to get the project ID.
	agentRecord, err := s.store.GetAgent(r.Context(), agent.Subject)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeError(w, http.StatusForbidden, ErrCodeForbidden, "agent not found", nil)
			return
		}
		writeError(w, http.StatusInternalServerError, ErrCodeRuntimeError, "failed to look up agent", nil)
		return
	}
	if agentRecord == nil {
		writeError(w, http.StatusForbidden, ErrCodeForbidden, "agent not found", nil)
		return
	}

	slog.Info("agent secret fetch",
		"agent_id", agent.Subject,
		"project_id", agentRecord.ProjectID,
		"keys", req.Keys,
	)

	if s.secretBackend == nil {
		writeError(w, http.StatusServiceUnavailable, ErrCodeUnavailable,
			"secret storage is not configured on this Hub", nil)
		return
	}

	results := make([]secretFetchResult, 0, len(req.Keys))
	for _, key := range req.Keys {
		sv, err := s.secretBackend.Get(r.Context(), key, store.ScopeProject, agentRecord.ProjectID)
		if err != nil {
			if errors.Is(err, store.ErrNotFound) {
				results = append(results, secretFetchResult{
					Key:    key,
					Status: "not_found",
					Error:  "secret not found",
				})
			} else {
				results = append(results, secretFetchResult{
					Key:    key,
					Status: "entitled_but_unavailable",
					Error:  err.Error(),
				})
			}
			continue
		}
		results = append(results, secretFetchResult{
			Key:    key,
			Value:  sv.Value,
			Status: "ok",
		})
	}

	writeJSON(w, http.StatusOK, secretFetchResponse{
		Secrets: results,
	})
}
