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

// handleAdminMessaging handles GET/PUT /api/v1/admin/messaging.
//
// GET returns the current messaging switch (merged with compiled default).
// PUT accepts a partial update to the messaging opsettings section.
//
// Both endpoints are admin-gated (same auth check as handleAdminMaintenance).
// The section follows the maintenance pattern: DB-only, no settings.yaml
// representation, with a dedicated admin API endpoint.
//
// Phase 9a: the two former switches (conversation_read_switch and
// conversation_write_deny_switch) are consolidated into a single
// conversation_envelope_switch that defaults ON. Stale keys self-clean
// on first PUT.
func (s *Server) handleAdminMessaging(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		s.handleGetMessaging(w)
	case http.MethodPut:
		s.handlePutMessaging(w, r)
	default:
		MethodNotAllowed(w, http.MethodGet, http.MethodPut)
	}
}

// messagingResponse is the GET/PUT response shape for the admin messaging endpoint.
type messagingResponse struct {
	ConversationEnvelopeSwitch   *bool `json:"conversation_envelope_switch"`
	CrossProjectMessagingEnabled *bool `json:"cross_project_messaging_enabled"`
	// Revision is the settings section revision, used for CAS on security-
	// critical flag changes.
	Revision int64 `json:"revision"`
}

// messagingPutRequest is the typed request for PUT /admin/messaging.
// All fields are optional pointers; omitted = unchanged.
type messagingPutRequest struct {
	ConversationEnvelopeSwitch   *bool  `json:"conversation_envelope_switch,omitempty"`
	CrossProjectMessagingEnabled *bool  `json:"cross_project_messaging_enabled,omitempty"`
	ExpectedRevision             *int64 `json:"expected_revision,omitempty"`
}

// handleGetMessaging returns the current messaging settings.
// Reports what enforcement sites would actually do: when OperationalSettings
// is nil (init failed), enforcement reads false, so GET reports false.
func (s *Server) handleGetMessaging(w http.ResponseWriter) {
	envelopeSwitch := false
	crossProjectEnabled := false
	var revision int64

	if ops := s.GetOperationalSettings(); ops != nil {
		envelopeSwitch = ops.ConversationEnvelopeSwitch()
		crossProjectEnabled = ops.CrossProjectMessagingEnabled()
		revision = ops.SectionRevision("messaging")
	}

	writeJSON(w, http.StatusOK, messagingResponse{
		ConversationEnvelopeSwitch:   &envelopeSwitch,
		CrossProjectMessagingEnabled: &crossProjectEnabled,
		Revision:                     revision,
	})
}

// handlePutMessaging accepts a field-preserving partial update to the
// messaging section.
//
// Semantics:
//   - Omitted field = unchanged
//   - Explicit null = reset ONLY that field to its compiled default
//   - Resetting conversation_envelope_switch does NOT reset cross_project_messaging_enabled
//   - Changing cross_project_messaging_enabled requires expected_revision for CAS
//
// The handler merges the request with current state, preserving any
// fields not mentioned in the request body.
func (s *Server) handlePutMessaging(w http.ResponseWriter, r *http.Request) {
	ops := s.GetOperationalSettings()
	if ops == nil {
		writeError(w, http.StatusNotImplemented, "not_implemented",
			"Updating messaging settings is not supported in file/SQLite mode", nil)
		return
	}

	rawBody, err := readRawBody(w, r)
	if err != nil {
		writeError(w, http.StatusBadRequest, ErrCodeInvalidRequest, "Invalid request body", nil)
		return
	}

	var body messagingPutRequest
	if err := json.Unmarshal(rawBody, &body); err != nil {
		writeError(w, http.StatusBadRequest, ErrCodeInvalidRequest, "Invalid request body", nil)
		return
	}

	// Presence-aware: detect explicit null for per-field reset.
	fp, fpErr := parseFieldPresence(rawBody)
	if fpErr != nil {
		slog.Warn("parseFieldPresence failed in messaging handler, falling back to omitted-semantics", "error", fpErr)
	}

	// Build the merged messaging section from current values.
	currentEnvelope := ops.ConversationEnvelopeSwitch()
	currentCrossProject := ops.CrossProjectMessagingEnabled()
	currentRevision := ops.SectionRevision("messaging")

	ms := opsettings.MessagingSettings{
		ConversationEnvelopeSwitch:   &currentEnvelope,
		CrossProjectMessagingEnabled: &currentCrossProject,
	}

	// Apply per-field updates or resets.

	// conversation_envelope_switch:
	if body.ConversationEnvelopeSwitch != nil {
		ms.ConversationEnvelopeSwitch = body.ConversationEnvelopeSwitch
	} else if fp != nil && fp.has("conversation_envelope_switch") {
		// Explicit null → reset to compiled default (ON).
		defaultVal := true
		ms.ConversationEnvelopeSwitch = &defaultVal
	}

	// cross_project_messaging_enabled:
	if body.CrossProjectMessagingEnabled != nil {
		// Security-critical flag: require expected_revision for CAS.
		if body.ExpectedRevision == nil {
			writeError(w, http.StatusBadRequest, ErrCodeInvalidRequest,
				"expected_revision is required when changing cross_project_messaging_enabled", nil)
			return
		}
		if *body.ExpectedRevision != currentRevision {
			writeError(w, http.StatusConflict, "revision_conflict",
				"The messaging settings were modified concurrently. Please refresh and retry.", nil)
			return
		}
		ms.CrossProjectMessagingEnabled = body.CrossProjectMessagingEnabled
	} else if fp != nil && fp.has("cross_project_messaging_enabled") {
		// Explicit null → reset to compiled default (OFF).
		if body.ExpectedRevision == nil {
			writeError(w, http.StatusBadRequest, ErrCodeInvalidRequest,
				"expected_revision is required when changing cross_project_messaging_enabled", nil)
			return
		}
		if *body.ExpectedRevision != currentRevision {
			writeError(w, http.StatusConflict, "revision_conflict",
				"The messaging settings were modified concurrently. Please refresh and retry.", nil)
			return
		}
		defaultVal := false
		ms.CrossProjectMessagingEnabled = &defaultVal
	}

	doc, err := json.Marshal(ms)
	if err != nil {
		slog.Error("PUT messaging: failed to marshal messaging settings", "error", err)
		writeError(w, http.StatusInternalServerError, ErrCodeInternalError,
			"Failed to marshal messaging settings", nil)
		return
	}

	// Validate the document against the section schema.
	if errs := opsettings.Validate("messaging", doc); len(errs) > 0 {
		writeJSON(w, http.StatusBadRequest, map[string]interface{}{
			"error":  "validation_failed",
			"errors": errs,
		})
		return
	}

	caller := GetUserIdentityFromContext(r.Context())
	updatedBy := ""
	if caller != nil {
		updatedBy = caller.Email()
	}

	// Use CAS with the current revision to prevent concurrent overwrites.
	casRevision := currentRevision
	if _, err := ops.Update(r.Context(), "messaging", doc, updatedBy, casRevision, "managed"); err != nil {
		slog.Error("Failed to update messaging settings", "error", err)
		writeError(w, http.StatusInternalServerError, ErrCodeInternalError,
			"Failed to update messaging settings", nil)
		return
	}

	// Read back the applied state.
	resultEnvelope := ops.ConversationEnvelopeSwitch()
	resultCrossProject := ops.CrossProjectMessagingEnabled()
	resultRevision := ops.SectionRevision("messaging")
	writeJSON(w, http.StatusOK, messagingResponse{
		ConversationEnvelopeSwitch:   &resultEnvelope,
		CrossProjectMessagingEnabled: &resultCrossProject,
		Revision:                     resultRevision,
	})
}
