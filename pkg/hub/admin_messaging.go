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
// GET returns the current messaging switches (merged with compiled defaults).
// PUT accepts a partial update to the messaging opsettings section.
//
// Both endpoints are admin-gated (same auth check as handleAdminMaintenance).
// The section follows the maintenance pattern: DB-only, no settings.yaml
// representation, with a dedicated admin API endpoint.
func (s *Server) handleAdminMessaging(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		s.handleGetMessaging(w)
	case http.MethodPut:
		s.handlePutMessaging(w, r)
	default:
		MethodNotAllowed(w)
	}
}

// handleGetMessaging returns the current messaging switches.
// When no DB row exists, the compiled defaults are returned (both switches OFF).
func (s *Server) handleGetMessaging(w http.ResponseWriter) {
	readSwitch := false
	writeDenySwitch := false

	if ops := s.GetOperationalSettings(); ops != nil {
		readSwitch = ops.ConversationReadSwitch()
		writeDenySwitch = ops.ConversationWriteDenySwitch()
	}

	writeJSON(w, http.StatusOK, opsettings.MessagingSettings{
		ConversationReadSwitch:      &readSwitch,
		ConversationWriteDenySwitch: &writeDenySwitch,
	})
}

// handlePutMessaging accepts a presence-aware partial update to the messaging
// section. An omitted field leaves the current value unchanged; only an
// explicitly sent field updates.
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

	// Build the messaging section doc. Start from the current snapshot values
	// to preserve fields not being updated (partial update semantics).
	currentRead := ops.ConversationReadSwitch()
	currentWriteDeny := ops.ConversationWriteDenySwitch()

	ms := opsettings.MessagingSettings{
		ConversationReadSwitch:      &currentRead,
		ConversationWriteDenySwitch: &currentWriteDeny,
	}

	// Presence-aware: only update fields that were explicitly sent.
	fp, fpErr := parseFieldPresence(rawBody)
	if fpErr != nil {
		slog.Warn("parseFieldPresence failed in messaging handler, falling back to omitted-semantics", "error", fpErr)
	}

	if body.ConversationReadSwitch != nil {
		ms.ConversationReadSwitch = body.ConversationReadSwitch
	} else if fp != nil && fp.has("conversation_read_switch") {
		// Explicitly sent as null → reset to compiled default (false).
		f := false
		ms.ConversationReadSwitch = &f
	}

	if body.ConversationWriteDenySwitch != nil {
		ms.ConversationWriteDenySwitch = body.ConversationWriteDenySwitch
	} else if fp != nil && fp.has("conversation_write_deny_switch") {
		// Explicitly sent as null → reset to compiled default (false).
		f := false
		ms.ConversationWriteDenySwitch = &f
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
	readSwitch := ops.ConversationReadSwitch()
	writeDenySwitch := ops.ConversationWriteDenySwitch()
	writeJSON(w, http.StatusOK, opsettings.MessagingSettings{
		ConversationReadSwitch:      &readSwitch,
		ConversationWriteDenySwitch: &writeDenySwitch,
	})
}
