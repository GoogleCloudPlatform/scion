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

// agentSecretAccessDeniedMessage and agentSecretAccessErrorMessage are the
// fixed neutral messages for a whole-request precheck denial (checks 1-6).
// No backend error string ever reaches the caller.
const (
	agentSecretAccessDeniedMessage = "agent is not authorized for secret access"
	agentSecretAccessErrorMessage  = "failed to verify agent access"
)

// handleAgentSecretFetch handles POST /api/v1/agent/secrets.
// Called by the agent client (FetchSecrets) to retrieve secret values by key.
// Authenticates via X-Scion-Agent-Token and returns secrets scoped to the
// agent's project.
//
// Every key goes through one check sequence (material_runtime.go) before
// any value is read: the whole-request precheck (checks 1-6), then per-item
// project authorization (check 7) and the record-race rule (check 9).
// Unless the agent's applied config allows GitHub credentials, a GitHub
// credential key is reported not_found without any read
// (gitCredentialKeyDenied), and so is a secret whose target names a GitHub
// credential, refused after its metadata is read and before its value is
// read (gitCredentialSecretDenied); both checks are in selectRuntimeMaterial.
func (s *Server) handleAgentSecretFetch(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		MethodNotAllowed(w, http.MethodPost)
		return
	}

	// ident == nil keeps today's status for this endpoint (403), decided
	// before the precheck runs.
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

	slog.Info("agent secret fetch",
		"agent_id", agent.Subject,
		"keys", req.Keys,
	)

	if s.secretBackend == nil {
		writeError(w, http.StatusServiceUnavailable, ErrCodeUnavailable,
			"secret storage is not configured on this Hub", nil)
		return
	}

	ctx := r.Context()
	ident := GetAgentIdentityFromContext(ctx)
	correlationID := newMaterialCorrelationID()

	facts, reason, status := s.materialRuntimePrecheck(ctx, ident)
	if status != 0 {
		event := s.buildMaterialSelectionEvent(ctx, "fetch", correlationID, nil, reason, nil)
		s.logMaterialSelection(ctx, event)

		if status == http.StatusInternalServerError {
			writeError(w, status, ErrCodeRuntimeError, agentSecretAccessErrorMessage, nil)
			return
		}
		writeError(w, status, ErrCodeForbidden, agentSecretAccessDeniedMessage, nil)
		return
	}

	// A nil or absent authz service fails the whole request closed (a
	// per-item entitled_but_unavailable inside a 200 would not match that).
	if s.authzService == nil {
		event := s.buildMaterialSelectionEvent(ctx, "fetch", correlationID, facts, ReasonBackendError, nil)
		s.logMaterialSelection(ctx, event)
		writeError(w, http.StatusInternalServerError, ErrCodeRuntimeError, agentSecretAccessErrorMessage, nil)
		return
	}

	results := make([]secretFetchResult, 0, len(req.Keys))
	items := make([]MaterialSelectionEventItem, 0, len(req.Keys))
	var decisionCache projectDecisionCache

	for _, key := range req.Keys {
		item, sv, permission, detail := s.selectRuntimeMaterial(ctx, ident, facts, store.ScopeProject, key, &decisionCache)
		items = append(items, materialSelectionItem(item, permission, detail))

		switch {
		case item.Selected:
			results = append(results, secretFetchResult{Key: key, Value: sv.Value, Status: "ok"})
			s.logAgentSecretReadCompat(ctx, agent.Subject, facts.ProjectID, store.ScopeProject, facts.ProjectID, key, true, "", true, correlationID)
		case item.Allowed || item.Reason == ReasonBackendError:
			// The project-level decision allowed this item (or check 7 hit an
			// infrastructure error before deciding). Either way a backend
			// fault or a check-9 record race confirms nothing about the key,
			// so it is reported unavailable rather than not found.
			results = append(results, secretFetchResult{Key: key, Status: "entitled_but_unavailable", Error: "secret unavailable"})
			s.logAgentSecretReadCompat(ctx, agent.Subject, facts.ProjectID, store.ScopeProject, facts.ProjectID, key, false, item.Reason, true, correlationID)
		default:
			results = append(results, secretFetchResult{Key: key, Status: "not_found", Error: "secret not found"})
			s.logAgentSecretReadCompat(ctx, agent.Subject, facts.ProjectID, store.ScopeProject, facts.ProjectID, key, false, item.Reason, true, correlationID)
		}
	}

	event := s.buildMaterialSelectionEvent(ctx, "fetch", correlationID, facts, "", items)
	s.logMaterialSelection(ctx, event)

	writeJSON(w, http.StatusOK, secretFetchResponse{
		Secrets: results,
	})
}
