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
	"io"
	"net/http"
	"strings"

	triggithub "github.com/GoogleCloudPlatform/scion/pkg/hub/trigger/github"
)

const (
	maxTriggerPresetCompileBodyBytes = 64 * 1024
	maxTriggerPresetSourceBytes      = 32
)

// handleListTriggerPresets handles GET /api/v1/trigger-presets?source=github (§9.2).
//
// Both trigger-presets routes are classified RouteAuthenticated behind
// requireGitHubTriggers (404 while hub.github_triggers is off): listing static
// preset field schemas and compiling form inputs into CEL + suggested actions
// read no tenant records and mutate no server state, so any authenticated user
// or agent identity may call them.
func (s *Server) handleListTriggerPresets(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		MethodNotAllowed(w, http.MethodGet)
		return
	}

	source := strings.TrimSpace(r.URL.Query().Get("source"))
	if source != "" && !isValidTriggerPresetSourceIdent(source) {
		ValidationError(w, "invalid source query parameter", nil)
		return
	}

	writeJSON(w, http.StatusOK, triggithub.Presets(source))
}

// handleCompileTriggerPreset handles POST /api/v1/trigger-presets/compile (§9.2).
func (s *Server) handleCompileTriggerPreset(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		MethodNotAllowed(w, http.MethodPost)
		return
	}

	if r.Body == nil {
		ValidationError(w, "request body is required", nil)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxTriggerPresetCompileBodyBytes)

	var req triggithub.CompilePresetRequest
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		var maxBytesErr *http.MaxBytesError
		if errors.As(err, &maxBytesErr) {
			writeError(w, http.StatusRequestEntityTooLarge, ErrCodeInvalidRequest, "request body exceeds 64 KiB limit", nil)
			return
		}
		ValidationError(w, "invalid JSON request body", nil)
		return
	}
	var trailing any
	if err := dec.Decode(&trailing); err != io.EOF {
		var maxBytesErr *http.MaxBytesError
		if errors.As(err, &maxBytesErr) {
			writeError(w, http.StatusRequestEntityTooLarge, ErrCodeInvalidRequest, "request body exceeds 64 KiB limit", nil)
			return
		}
		ValidationError(w, "invalid JSON request body", nil)
		return
	}

	compiled, err := triggithub.CompilePreset(req)
	if err != nil {
		var cErr *triggithub.PresetCompileError
		if errors.As(err, &cErr) {
			ValidationError(w, cErr.Message, map[string]any{
				"fields": cErr.Fields,
			})
			return
		}
		ValidationError(w, "preset compilation failed", nil)
		return
	}

	writeJSON(w, http.StatusOK, compiled)
}

func isValidTriggerPresetSourceIdent(s string) bool {
	if len(s) == 0 || len(s) > maxTriggerPresetSourceBytes {
		return false
	}
	if s[0] < 'a' || s[0] > 'z' {
		return false
	}
	for i := 1; i < len(s); i++ {
		c := s[i]
		if (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') || c == '_' || c == '-' {
			continue
		}
		return false
	}
	return true
}
