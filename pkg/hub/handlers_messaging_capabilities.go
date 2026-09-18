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
	"net/http"
)

// messagingCapabilitiesResponse is the response for GET /api/v1/messaging/capabilities.
type messagingCapabilitiesResponse struct {
	HubEnabled                     bool     `json:"hubEnabled"`
	CrossProjectConversationKinds  []string `json:"crossProjectConversationKinds"`
	SupportedModes                 []string `json:"supportedModes"`
}

// handleMessagingCapabilities handles GET /api/v1/messaging/capabilities.
// Returns minimal supported modes, Hub enabled state, and conversation kinds.
// Authenticated but does not require admin access.
func (s *Server) handleMessagingCapabilities(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		MethodNotAllowed(w, http.MethodGet)
		return
	}

	identity := GetIdentityFromContext(r.Context())
	if identity == nil {
		Unauthorized(w)
		return
	}

	hubEnabled := false
	ops := s.GetOperationalSettings()
	if ops != nil {
		hubEnabled = ops.CrossProjectMessagingEnabled()
	}

	resp := messagingCapabilitiesResponse{
		HubEnabled:                    hubEnabled,
		CrossProjectConversationKinds: []string{"direct"},
		SupportedModes:                []string{"none", "lineage", "branch", "project", "hub"},
	}

	writeJSON(w, http.StatusOK, resp)
}
