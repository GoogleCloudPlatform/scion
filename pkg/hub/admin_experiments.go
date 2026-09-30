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
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/config/opsettings"
	"github.com/GoogleCloudPlatform/scion/pkg/experiments"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// handleAdminExperiments handles GET/PUT/DELETE /api/v1/admin/experiments
// (ptone/scion#2217). Route metadata (RouteHubAdmin, permission
// hub.experiments.update) authorizes the caller before the handler runs; the
// handler adds no extra super-admin check, matching handleAdminMessaging.
func (s *Server) handleAdminExperiments(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		s.handleGetAdminExperiments(w)
	case http.MethodPut:
		s.handlePutAdminExperiments(w, r)
	case http.MethodDelete:
		s.handleDeleteAdminExperiments(w, r)
	default:
		MethodNotAllowed(w, http.MethodGet, http.MethodPut, http.MethodDelete)
	}
}

// adminExperimentEntry is one row of the admin GET/PUT/DELETE response.
type adminExperimentEntry struct {
	Name          string   `json:"name"`
	Title         string   `json:"title"`
	Description   string   `json:"description"`
	Layers        []string `json:"layers"`
	Stage         string   `json:"stage"`
	Default       bool     `json:"default"`
	Override      *bool    `json:"override"`
	Enabled       bool     `json:"enabled"`
	Issue         string   `json:"issue"`
	Owner         string   `json:"owner"`
	ReviewBy      string   `json:"review_by"`
	ReviewOverdue bool     `json:"review_overdue"`
}

// adminExperimentsResponse is the GET/PUT/DELETE response shape.
type adminExperimentsResponse struct {
	Revision         int64                  `json:"revision"`
	Malformed        bool                   `json:"malformed"`
	Experiments      []adminExperimentEntry `json:"experiments"`
	UnknownOverrides map[string]bool        `json:"unknown_overrides"`
	UpdatedAt        *time.Time             `json:"updated_at"`
	UpdatedBy        *string                `json:"updated_by"`
}

// buildAdminExperimentsResponse builds the response from one consistent view
// of the section (overrides, revision, malformed flag, metadata), so
// `enabled` never mixes two refreshes and always agrees with experimentEnabled
// and GET /api/v1/experiments.
func (s *Server) buildAdminExperimentsResponse(overrides map[string]bool, revision int64, malformed, present bool, updatedAt time.Time, updatedBy string) adminExperimentsResponse {
	reg := s.experimentRegistry()
	snap := ExperimentsSnapshot{Overrides: overrides, Malformed: malformed}

	all := reg.All()
	entries := make([]adminExperimentEntry, 0, len(all))
	known := make(map[string]bool, len(all))
	now := time.Now() // one consistent instant for every entry in this response
	for _, exp := range all {
		known[exp.Name] = true

		var override *bool
		if v, ok := overrides[exp.Name]; ok {
			vv := v
			override = &vv
		}

		layers := make([]string, len(exp.Layers))
		for i, l := range exp.Layers {
			layers[i] = string(l)
		}

		entries = append(entries, adminExperimentEntry{
			Name:          exp.Name,
			Title:         exp.Title,
			Description:   exp.Description,
			Layers:        layers,
			Stage:         string(exp.Stage),
			Default:       exp.Default,
			Override:      override,
			Enabled:       s.experimentEnabledIn(snap, exp.Name),
			Issue:         exp.Issue,
			Owner:         exp.Owner,
			ReviewBy:      exp.ReviewBy,
			ReviewOverdue: exp.ReviewOverdue(now),
		})
	}

	// unknown_overrides: stored pattern-valid names this binary does not
	// know. Retired names are excluded — they are pruned on the next PUT,
	// so they are never "kept". A pattern-invalid key should never reach
	// storage (PUT drops it), but is excluded defensively if one is found.
	unknown := make(map[string]bool)
	for name, v := range overrides {
		if known[name] || reg.IsRetired(name) || !experiments.ValidName(name) {
			continue
		}
		unknown[name] = v
	}

	resp := adminExperimentsResponse{
		Revision:         revision,
		Malformed:        malformed,
		Experiments:      entries,
		UnknownOverrides: unknown,
	}
	if present {
		t := updatedAt
		b := updatedBy
		resp.UpdatedAt = &t
		resp.UpdatedBy = &b
	}
	return resp
}

// buildAdminExperimentsResponseAfterWrite builds the PUT/DELETE response.
// Update() refreshes the writing replica's cache synchronously before it
// returns, so the snapshot normally already reflects this write. If a later
// refresh landed in between (the snapshot's revision no longer matches the
// revision this write produced), attribute the response to the caller and
// now instead of a possibly different writer.
func (s *Server) buildAdminExperimentsResponseAfterWrite(overrides map[string]bool, revision int64, callerEmail string, ops *OperationalSettings) adminExperimentsResponse {
	updatedAt := time.Now()
	updatedBy := callerEmail
	if snap := ops.ExperimentsSnapshot(); snap.Present && snap.Revision == revision {
		updatedAt = snap.UpdatedAt
		updatedBy = snap.UpdatedBy
	}
	return s.buildAdminExperimentsResponse(overrides, revision, false, true, updatedAt, updatedBy)
}

// handleGetAdminExperiments returns the current experiments settings, built
// from the cache (a lagging replica is acceptable here; PUT checks
// authoritatively).
func (s *Server) handleGetAdminExperiments(w http.ResponseWriter) {
	ops := s.GetOperationalSettings()
	if ops == nil {
		writeError(w, http.StatusServiceUnavailable, "settings_unavailable", "Experiment settings are not available", nil)
		return
	}
	snap := ops.ExperimentsSnapshot()
	resp := s.buildAdminExperimentsResponse(snap.Overrides, snap.Revision, snap.Malformed, snap.Present, snap.UpdatedAt, snap.UpdatedBy)
	writeJSON(w, http.StatusOK, resp)
}

// adminExperimentsPutRequest is the PUT request body. Overrides maps a name
// to true/false (set) or JSON null (remove, decoded as a nil pointer);
// omitted names are left unchanged. ExpectedRevision is required.
type adminExperimentsPutRequest struct {
	Overrides        map[string]*bool `json:"overrides"`
	ExpectedRevision *int64           `json:"expected_revision"`
}

// handlePutAdminExperiments implements the write algorithm for
// ptone/scion#2217: an authoritative (store, not cache) compare-and-set
// against expected_revision.
func (s *Server) handlePutAdminExperiments(w http.ResponseWriter, r *http.Request) {
	ops := s.GetOperationalSettings()
	if ops == nil {
		writeError(w, http.StatusServiceUnavailable, "settings_unavailable", "Experiment settings are not available", nil)
		return
	}

	rawBody, err := readRawBody(w, r)
	if err != nil {
		writeError(w, http.StatusBadRequest, "validation_failed", "Invalid request body", nil)
		return
	}
	var body adminExperimentsPutRequest
	if err := json.Unmarshal(rawBody, &body); err != nil {
		writeError(w, http.StatusBadRequest, "validation_failed", "Invalid request body: overrides must be an object of name to true, false, or null", nil)
		return
	}
	if body.ExpectedRevision == nil {
		writeError(w, http.StatusBadRequest, "validation_failed", "expected_revision is required", nil)
		return
	}
	if len(body.Overrides) == 0 {
		writeError(w, http.StatusBadRequest, "validation_failed", "overrides must not be empty", nil)
		return
	}

	reg := s.experimentRegistry()
	for name, valPtr := range body.Overrides {
		if valPtr == nil {
			if !experiments.ValidName(name) {
				writeError(w, http.StatusBadRequest, "validation_failed",
					fmt.Sprintf("%q does not match the experiment name pattern", name),
					map[string]interface{}{"name": name})
				return
			}
			continue
		}
		if _, ok := reg.Lookup(name); !ok {
			writeError(w, http.StatusBadRequest, "validation_failed",
				fmt.Sprintf("%q is not a registered experiment", name),
				map[string]interface{}{"name": name})
			return
		}
	}

	ctx := r.Context()
	res := ops.ReadAuthoritativeExperiments(ctx)
	if res.Err != nil {
		writeError(w, http.StatusServiceUnavailable, "settings_unavailable", "Failed to read experiment settings", nil)
		return
	}
	if res.Malformed {
		writeError(w, http.StatusConflict, "experiments_malformed",
			"Stored experiment settings are unreadable; reset all experiments to defaults to continue.", nil)
		return
	}
	if *body.ExpectedRevision != res.Revision {
		writeError(w, http.StatusConflict, "revision_conflict",
			"The experiment settings were modified concurrently. Please refresh and retry.", nil)
		return
	}

	merged := mergeExperimentOverrides(res.Overrides, body.Overrides)
	pruneStoredExperimentOverrides(merged, reg)

	doc, err := json.Marshal(opsettings.ExperimentsSettings{Overrides: merged})
	if err != nil {
		slog.Error("PUT experiments: failed to marshal experiment settings", "error", err)
		writeError(w, http.StatusInternalServerError, ErrCodeInternalError, "Failed to marshal experiment settings", nil)
		return
	}
	if errs := opsettings.Validate("experiments", doc); len(errs) > 0 {
		writeError(w, http.StatusBadRequest, "validation_failed", "Invalid experiment settings", map[string]interface{}{"errors": errs})
		return
	}

	caller := GetUserIdentityFromContext(ctx)
	updatedBy := ""
	if caller != nil {
		updatedBy = caller.Email()
	}

	newRevision, err := ops.Update(ctx, "experiments", doc, updatedBy, res.Revision, "managed")
	if err != nil {
		if errors.Is(err, store.ErrRevisionConflict) {
			writeError(w, http.StatusConflict, "revision_conflict",
				"The experiment settings were modified concurrently. Please refresh and retry.", nil)
			return
		}
		slog.Error("Failed to update experiment settings", "error", err)
		writeError(w, http.StatusInternalServerError, ErrCodeInternalError, "Failed to update experiment settings", nil)
		return
	}

	logExperimentOverrideChanges(res.Overrides, merged, body.Overrides, updatedBy)

	resp := s.buildAdminExperimentsResponseAfterWrite(merged, newRevision, updatedBy, ops)
	writeJSON(w, http.StatusOK, resp)
}

// mergeExperimentOverrides applies a PUT request onto the authoritative
// overrides map (never mutating it), returning a new map. true/false sets an
// override; a nil pointer (JSON null) removes it (a no-op if absent). Names
// omitted from the request are left unchanged.
func mergeExperimentOverrides(current map[string]bool, req map[string]*bool) map[string]bool {
	merged := make(map[string]bool, len(current)+len(req))
	for k, v := range current {
		merged[k] = v
	}
	for name, valPtr := range req {
		if valPtr == nil {
			delete(merged, name)
			continue
		}
		merged[name] = *valPtr
	}
	return merged
}

// pruneStoredExperimentOverrides removes, in place, names that must never
// stay stored: retired names (permanently dead) and keys that fail the
// name pattern (never any version's experiment name; warned, not silently
// dropped). Pattern-valid names unknown to this binary's registry are
// preserved verbatim — a newer or rolled-back replica may still read them
// (ptone/scion#2217).
func pruneStoredExperimentOverrides(overrides map[string]bool, reg *experiments.Registry) {
	for name := range overrides {
		switch {
		case reg.IsRetired(name):
			delete(overrides, name)
		case !experiments.ValidName(name):
			slog.Warn("experiments: dropping stored override with an invalid name", "name", name)
			delete(overrides, name)
		}
	}
}

// logExperimentOverrideChanges writes one slog.Info per name the request
// actually changed. Names the request mentioned that produced
// no change (for example null for a name that was already absent) are not
// logged.
func logExperimentOverrideChanges(before, after map[string]bool, req map[string]*bool, actor string) {
	for name := range req {
		beforeVal, hadBefore := before[name]
		afterVal, hasAfter := after[name]
		switch {
		case hadBefore && !hasAfter:
			slog.Info("experiment override removed", "experiment", name, "from", beforeVal, "actor", actor)
		case !hadBefore && hasAfter:
			slog.Info("experiment override set", "experiment", name, "to", afterVal, "actor", actor)
		case hadBefore && hasAfter && beforeVal != afterVal:
			slog.Info("experiment override changed", "experiment", name, "from", beforeVal, "to", afterVal, "actor", actor)
		}
	}
}

// adminExperimentsDeleteRequest is the DELETE request body. A healthy
// section requires ExpectedRevision; a malformed one requires
// ConfirmResetMalformed instead, because it has no trustworthy value to
// compare against.
type adminExperimentsDeleteRequest struct {
	ExpectedRevision      *int64 `json:"expected_revision"`
	ConfirmResetMalformed bool   `json:"confirm_reset_malformed"`
}

// handleDeleteAdminExperiments resets every stored override to the registry
// defaults. It is the only reset path for the "experiments" section;
// the generic per-section reset route rejects it (see
// handleAdminServerConfigSectionReset).
func (s *Server) handleDeleteAdminExperiments(w http.ResponseWriter, r *http.Request) {
	ops := s.GetOperationalSettings()
	if ops == nil {
		writeError(w, http.StatusServiceUnavailable, "settings_unavailable", "Experiment settings are not available", nil)
		return
	}

	rawBody, err := readRawBody(w, r)
	if err != nil {
		writeError(w, http.StatusBadRequest, "validation_failed", "Invalid request body", nil)
		return
	}
	var body adminExperimentsDeleteRequest
	if len(rawBody) > 0 {
		if err := json.Unmarshal(rawBody, &body); err != nil {
			writeError(w, http.StatusBadRequest, "validation_failed", "Invalid request body", nil)
			return
		}
	}

	ctx := r.Context()
	res := ops.ReadAuthoritativeExperiments(ctx)
	if res.Err != nil {
		writeError(w, http.StatusServiceUnavailable, "settings_unavailable", "Failed to read experiment settings", nil)
		return
	}

	if res.Malformed {
		if !body.ConfirmResetMalformed {
			writeError(w, http.StatusBadRequest, "validation_failed",
				"confirm_reset_malformed is required to reset a malformed section", nil)
			return
		}
	} else {
		if body.ConfirmResetMalformed {
			// The row is healthy (for example another admin already reset
			// it): confirm_reset_malformed no longer applies. Nothing is
			// written; the caller reloads and sees the current state.
			writeError(w, http.StatusConflict, "revision_conflict",
				"The experiment settings are not malformed. Refresh and retry with expected_revision.", nil)
			return
		}
		if body.ExpectedRevision == nil {
			writeError(w, http.StatusBadRequest, "validation_failed", "expected_revision is required", nil)
			return
		}
		if *body.ExpectedRevision != res.Revision {
			writeError(w, http.StatusConflict, "revision_conflict",
				"The experiment settings were modified concurrently. Please refresh and retry.", nil)
			return
		}
	}

	doc, err := json.Marshal(opsettings.ExperimentsSettings{})
	if err != nil {
		slog.Error("DELETE experiments: failed to marshal experiment settings", "error", err)
		writeError(w, http.StatusInternalServerError, ErrCodeInternalError, "Failed to marshal experiment settings", nil)
		return
	}

	caller := GetUserIdentityFromContext(ctx)
	updatedBy := ""
	if caller != nil {
		updatedBy = caller.Email()
	}

	newRevision, err := ops.Update(ctx, "experiments", doc, updatedBy, res.Revision, "managed")
	if err != nil {
		if errors.Is(err, store.ErrRevisionConflict) {
			writeError(w, http.StatusConflict, "revision_conflict",
				"The experiment settings were modified concurrently. Please refresh and retry.", nil)
			return
		}
		slog.Error("Failed to reset experiment settings", "error", err)
		writeError(w, http.StatusInternalServerError, ErrCodeInternalError, "Failed to reset experiment settings", nil)
		return
	}

	slog.Warn("experiments: reset all overrides to defaults", "actor", updatedBy, "was_malformed", res.Malformed)

	resp := s.buildAdminExperimentsResponseAfterWrite(map[string]bool{}, newRevision, updatedBy, ops)
	writeJSON(w, http.StatusOK, resp)
}
