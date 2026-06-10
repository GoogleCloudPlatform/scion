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

	"github.com/GoogleCloudPlatform/scion/pkg/projectcompat"
)

const legacyGroveRouteSunset = "Sun, 01 Nov 2026 00:00:00 GMT"

// handleLegacyGroveRoute marks legacy /api/v1/groves endpoints as deprecated
// while preserving their existing behavior through the canonical project
// handlers.
func (s *Server) handleLegacyGroveRoute(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if projectcompat.DeprecatedGroveRoute(r.URL.Path) {
			w.Header().Set("Deprecation", "true")
			w.Header().Set("Sunset", legacyGroveRouteSunset)
			w.Header().Set("Link", `</api/v1/projects/>; rel="successor-version"`)
		}
		h(w, r)
	}
}

// legacyProjectIDFromJSON returns the project ID supplied through legacy grove
// JSON fields. Canonical fields remain decoded by the request-specific struct.
func legacyProjectIDFromJSON(data []byte) (string, error) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		return "", err
	}

	for _, key := range []string{"grove_id", "groveId"} {
		raw, ok := fields[key]
		if !ok {
			continue
		}
		var value string
		if err := json.Unmarshal(raw, &value); err != nil {
			return "", err
		}
		if value != "" {
			return value, nil
		}
	}
	return "", nil
}
