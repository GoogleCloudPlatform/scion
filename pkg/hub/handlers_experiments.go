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

// experimentsResponse is the response shape for GET /api/v1/experiments.
type experimentsResponse struct {
	Experiments map[string]bool `json:"experiments"`
}

// handleExperiments handles GET /api/v1/experiments (ptone/scion#2217).
//
// It is the client's source of truth for the hub-wide experiment map,
// fetched at boot in parallel with GET /api/v1/settings/public. The route is
// classified RouteAuthenticated: any signed-in identity (user, agent, or
// broker token) receives the map; there is no per-experiment authorization.
//
// The response holds every registered web-layer experiment, resolved from
// one snapshot so it never mixes two refreshes, and matches the value every
// other reader (experimentEnabled, the admin endpoints) would report right
// now.
func (s *Server) handleExperiments(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		MethodNotAllowed(w, http.MethodGet)
		return
	}
	writeJSON(w, http.StatusOK, experimentsResponse{Experiments: s.resolvedExperiments()})
}
