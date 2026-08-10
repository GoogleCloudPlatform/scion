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

	"github.com/GoogleCloudPlatform/scion/pkg/config/opsettings"
)

// handleAdminProjectDefaults handles GET/PUT /api/v1/admin/project-defaults.
//
// GET returns the current project defaults (merged with compiled defaults).
// PUT accepts a partial update to the project_defaults opsettings section.
//
// Both endpoints are admin-gated (same auth check as handleAdminMaintenance).
// The section follows the maintenance pattern: DB-only, no settings.yaml
// representation, with a dedicated admin API endpoint.
func (s *Server) handleAdminProjectDefaults(w http.ResponseWriter, r *http.Request) {
	// Require admin user.
	user := GetUserIdentityFromContext(r.Context())
	if user == nil || user.Role() != "admin" {
		Forbidden(w)
		return
	}

	switch r.Method {
	case http.MethodGet:
		s.handleGetProjectDefaults(w)
	case http.MethodPut:
		s.handlePutProjectDefaults(w, r)
	default:
		MethodNotAllowed(w)
	}
}

// handleGetProjectDefaults returns the current project defaults, merging
// the DB row with the compiled default. When no DB row exists, the compiled
// default is returned (default_scratchpad: true).
func (s *Server) handleGetProjectDefaults(w http.ResponseWriter) {
	enabled := true // compiled default

	if ops := s.GetOperationalSettings(); ops != nil {
		enabled = ops.ProjectDefaultScratchpad()
	} else {
		s.mu.RLock()
		if s.config.DefaultScratchpad != nil {
			// File/SQLite mode: read from settings.yaml via ApplySnapshot.
			enabled = *s.config.DefaultScratchpad
		}
		s.mu.RUnlock()
	}

	writeJSON(w, http.StatusOK, opsettings.ProjectDefaultsSettings{
		DefaultScratchpad: &enabled,
	})
}

// handlePutProjectDefaults accepts a partial update to the project_defaults
// section. It writes the section via OperationalSettings.Update() (which
// handles validation, persistence, and cross-replica propagation) or falls
// back to a simple validation-only response in file/SQLite mode.
func (s *Server) handlePutProjectDefaults(w http.ResponseWriter, r *http.Request) {
	var body opsettings.ProjectDefaultsSettings
	if err := readJSON(r, &body); err != nil {
		BadRequest(w, "Invalid request body: "+err.Error())
		return
	}

	doc, err := json.Marshal(body)
	if err != nil {
		writeError(w, http.StatusInternalServerError, ErrCodeInternalError,
			"Failed to marshal settings", nil)
		return
	}

	// Validate the document against the section schema.
	if errs := opsettings.Validate("project_defaults", doc); len(errs) > 0 {
		writeJSON(w, http.StatusBadRequest, map[string]interface{}{
			"error":  "validation_failed",
			"errors": errs,
		})
		return
	}

	// In postgres mode, persist via OperationalSettings.
	if ops := s.GetOperationalSettings(); ops != nil {
		caller := GetUserIdentityFromContext(r.Context())
		updatedBy := ""
		if caller != nil {
			updatedBy = caller.Email()
		}

		// last-writer-wins (-1) — no CAS needed for this endpoint.
		if _, err := ops.Update(r.Context(), "project_defaults", doc, updatedBy, -1, "managed"); err != nil {
			writeError(w, http.StatusInternalServerError, ErrCodeInternalError,
				"Failed to update project defaults: "+err.Error(), nil)
			return
		}

		// Read back the applied state.
		enabled := ops.ProjectDefaultScratchpad()
		writeJSON(w, http.StatusOK, opsettings.ProjectDefaultsSettings{
			DefaultScratchpad: &enabled,
		})
		return
	}

	// File/SQLite mode: no persistent storage for this section.
	// Return 501 to signal that writes are not supported.
	writeError(w, http.StatusNotImplemented, "not_implemented",
		"Updating project defaults is not supported in file/SQLite mode", nil)
}
