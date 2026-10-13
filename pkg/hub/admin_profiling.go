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
	"log/slog"
	"net/http"

	"github.com/GoogleCloudPlatform/scion/pkg/config/opsettings"
)

// profilingResponse is the GET/PUT response of /api/v1/admin/profiling.
type profilingResponse struct {
	ReadinessMarks bool  `json:"readiness_marks"`
	Revision       int64 `json:"revision"`
}

// profilingPutRequest is the typed request for PUT /api/v1/admin/profiling.
// Omitted = unchanged; an explicit null resets the key to its default (off).
type profilingPutRequest struct {
	ReadinessMarks *bool `json:"readiness_marks,omitempty"`
}

// ReadinessMarksEnabled reports the profiling readiness_marks setting; false
// without DB-backed operational settings. It is the single source for GET
// /api/v1/profiling and the web shell's initial data.
func (s *Server) ReadinessMarksEnabled() bool {
	if ops := s.GetOperationalSettings(); ops != nil {
		return ops.ReadinessMarks()
	}
	return false
}

// handleAdminProfiling handles GET/PUT /api/v1/admin/profiling, the write
// surface of the DB-only "profiling" operational settings section. The route
// guard admits hub administrators only (route_metadata.go: admin.profiling).
func (s *Server) handleAdminProfiling(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		s.writeProfilingResponse(w)
	case http.MethodPut:
		s.handlePutProfiling(w, r)
	default:
		MethodNotAllowed(w, http.MethodGet, http.MethodPut)
	}
}

func (s *Server) writeProfilingResponse(w http.ResponseWriter) {
	resp := profilingResponse{}
	if ops := s.GetOperationalSettings(); ops != nil {
		resp.ReadinessMarks = ops.ReadinessMarks()
		resp.Revision = ops.SectionRevision("profiling")
	}
	writeJSON(w, http.StatusOK, resp)
}

func (s *Server) handlePutProfiling(w http.ResponseWriter, r *http.Request) {
	ops := s.GetOperationalSettings()
	if ops == nil {
		writeError(w, http.StatusNotImplemented, "not_implemented",
			"Updating profiling settings requires DB-backed operational settings", nil)
		return
	}

	rawBody, err := readRawBody(w, r)
	if err != nil {
		writeError(w, http.StatusBadRequest, ErrCodeInvalidRequest, "Invalid request body", nil)
		return
	}
	var body profilingPutRequest
	if err := json.Unmarshal(rawBody, &body); err != nil {
		writeError(w, http.StatusBadRequest, ErrCodeInvalidRequest, "Invalid request body", nil)
		return
	}
	fp, fpErr := parseFieldPresence(rawBody)
	if fpErr != nil {
		slog.Warn("parseFieldPresence failed in profiling handler, falling back to omitted-semantics", "error", fpErr)
	}

	current := ops.ReadinessMarks()
	ps := opsettings.ProfilingSettings{ReadinessMarks: &current}
	if body.ReadinessMarks != nil {
		ps.ReadinessMarks = body.ReadinessMarks
	} else if fp != nil && fp.has("readiness_marks") {
		off := false
		ps.ReadinessMarks = &off
	}

	doc, err := json.Marshal(ps)
	if err != nil {
		writeError(w, http.StatusInternalServerError, ErrCodeInternalError, "Failed to marshal profiling settings", nil)
		return
	}
	if errs := opsettings.Validate("profiling", doc); len(errs) > 0 {
		writeJSON(w, http.StatusBadRequest, map[string]interface{}{
			"error":  "validation_failed",
			"errors": errs,
		})
		return
	}

	updatedBy := ""
	if caller := GetUserIdentityFromContext(r.Context()); caller != nil {
		updatedBy = caller.Email()
	}
	if _, err := ops.Update(r.Context(), "profiling", doc, updatedBy, ops.SectionRevision("profiling"), "managed"); err != nil {
		slog.Error("Failed to update profiling settings", "error", err)
		writeError(w, http.StatusInternalServerError, ErrCodeInternalError, "Failed to update profiling settings", nil)
		return
	}
	s.writeProfilingResponse(w)
}

// clientProfilingResponse is the response of GET /api/v1/profiling.
type clientProfilingResponse struct {
	ReadinessMarks bool `json:"readinessMarks"`
}

// handleProfiling handles GET /api/v1/profiling: the profiling switches the
// web client acts on, for any signed-in caller. It carries only the
// readiness marks switch.
func (s *Server) handleProfiling(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		MethodNotAllowed(w, http.MethodGet)
		return
	}
	writeJSON(w, http.StatusOK, clientProfilingResponse{ReadinessMarks: s.ReadinessMarksEnabled()})
}
