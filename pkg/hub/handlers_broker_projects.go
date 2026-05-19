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

	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// brokerProject is a lightweight project representation returned to broker plugins.
type brokerProject struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	Slug string `json:"slug"`
}

// handleBrokerProjects handles GET /api/v1/broker/projects.
// Returns all projects as a lightweight list for broker plugins (e.g. Telegram /setup).
//
// Authentication: Requires broker HMAC authentication (X-Scion-Broker-ID header
// validated by BrokerAuthMiddleware).
func (s *Server) handleBrokerProjects(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		MethodNotAllowed(w)
		return
	}

	broker := GetBrokerIdentityFromContext(r.Context())
	if broker == nil {
		writeError(w, http.StatusUnauthorized, ErrCodeBrokerAuthFailed,
			"broker HMAC authentication required", nil)
		return
	}

	result, err := s.store.ListProjects(r.Context(), store.ProjectFilter{}, store.ListOptions{Limit: 500})
	if err != nil {
		writeErrorFromErr(w, err, "")
		return
	}

	projects := make([]brokerProject, len(result.Items))
	for i, p := range result.Items {
		slug := p.Slug
		if slug == "" {
			slug = p.Name
		}
		projects[i] = brokerProject{ID: p.ID, Name: p.Name, Slug: slug}
	}

	writeJSON(w, http.StatusOK, map[string]interface{}{
		"projects": projects,
	})
}
