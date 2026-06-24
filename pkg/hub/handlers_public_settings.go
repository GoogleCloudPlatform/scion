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

// PublicSettingsResponse contains non-sensitive server settings for the web UI.
type PublicSettingsResponse struct {
	TelemetryEnabled bool `json:"telemetryEnabled"`
}

func (s *Server) handlePublicSettings(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		MethodNotAllowed(w)
		return
	}

	enabled := false
	if s.config.TelemetryDefault != nil {
		enabled = *s.config.TelemetryDefault
	}

	writeJSON(w, http.StatusOK, PublicSettingsResponse{
		TelemetryEnabled: enabled,
	})
}
