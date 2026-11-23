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

import "net/http"

// AgentHubInfoResponse is the response body for GET /api/v1/agent/hub.
type AgentHubInfoResponse struct {
	HubID string `json:"hub_id"`
}

// handleAgentHubInfo handles GET /api/v1/agent/hub.
// Returns non-secret hub identity metadata (hub_id) for the authenticated
// agent. Refuses non-agent callers (users, sessions, UATs, brokers) with
// 403 agents_only.
func (s *Server) handleAgentHubInfo(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		MethodNotAllowed(w, http.MethodGet)
		return
	}

	ctx := r.Context()
	agentIdent := GetAgentIdentityFromContext(ctx)
	if isNilIdentity(agentIdent) || GetUserIdentityFromContext(ctx) != nil {
		writeError(w, http.StatusForbidden, ErrCodeAgentsOnly, "this endpoint is only available to agents", nil)
		return
	}

	writeJSON(w, http.StatusOK, AgentHubInfoResponse{
		HubID: s.HubID(),
	})
}
