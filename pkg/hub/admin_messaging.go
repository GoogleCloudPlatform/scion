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

// messagingResponse is the GET/PUT response shape for the admin messaging
// endpoint. It exposes only the consolidated switch.
type messagingResponse struct {
	ConversationEnvelopeSwitch *bool `json:"conversation_envelope_switch"`
}

// handleGetMessaging returns the current messaging switch.
// When no DB row exists (or OperationalSettings is nil), the compiled
// default is returned (ON).
func (s *Server) handleGetMessaging(w http.ResponseWriter) {
	envelopeSwitch := true // compiled default: ON

	if ops := s.GetOperationalSettings(); ops != nil {
		envelopeSwitch = ops.ConversationEnvelopeSwitch()
	}

	writeJSON(w, http.StatusOK, messagingResponse{
		ConversationEnvelopeSwitch: &envelopeSwitch,
	})
}

// handlePutMessaging accepts a presence-aware partial update to the messaging
// section. An omitted field leaves the current value unchanged; only an
// explicitly sent field updates. An explicit null resets to the compiled
// default (ON) by deleting the section so the absent→default path runs.
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

	var body opsettings.MessagingSettings
	if err := json.Unmarshal(rawBody, &body); err != nil {
		writeError(w, http.StatusBadRequest, ErrCodeInvalidRequest, "Invalid request body", nil)
		return
	}

	// Presence-aware: detect explicit null for the reset path.
	fp, fpErr := parseFieldPresence(rawBody)
	if fpErr != nil {
		slog.Warn("parseFieldPresence failed in messaging handler, falling back to omitted-semantics", "error", fpErr)
	}

	// Check for explicit-null reset: if the key was sent as null, delete the
	// section entirely so the absent→compiled-default (ON) path runs.
	if body.ConversationEnvelopeSwitch == nil && fp != nil && fp.has("conversation_envelope_switch") {
		if err := ops.DeleteSection(r.Context(), "messaging"); err != nil {
			slog.Error("PUT messaging: failed to delete section for null reset", "error", err)
			writeError(w, http.StatusInternalServerError, ErrCodeInternalError,
				"Failed to reset messaging settings", nil)
			return
		}
		// Read back the compiled default (ON).
		result := ops.ConversationEnvelopeSwitch()
		writeJSON(w, http.StatusOK, messagingResponse{
			ConversationEnvelopeSwitch: &result,
		})
		return
	}

	// Build the messaging section doc from the current value.
	current := ops.ConversationEnvelopeSwitch()
	ms := opsettings.MessagingSettings{
		ConversationEnvelopeSwitch: &current,
	}

	// Apply the explicit value if sent.
	if body.ConversationEnvelopeSwitch != nil {
		ms.ConversationEnvelopeSwitch = body.ConversationEnvelopeSwitch
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

	// last-writer-wins (-1) for messaging — no CAS needed for this endpoint.
	if _, err := ops.Update(r.Context(), "messaging", doc, updatedBy, -1, "managed"); err != nil {
		slog.Error("Failed to update messaging settings", "error", err)
		writeError(w, http.StatusInternalServerError, ErrCodeInternalError,
			"Failed to update messaging settings", nil)
		return
	}

	// Read back the applied state.
	result := ops.ConversationEnvelopeSwitch()
	writeJSON(w, http.StatusOK, messagingResponse{
		ConversationEnvelopeSwitch: &result,
	})
}
