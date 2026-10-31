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
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/config/opsettings"
	"github.com/GoogleCloudPlatform/scion/pkg/experiments"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

const (
	// chatRetentionAuditSurface is the ExecutorID of chat_retention audit
	// records (the settings save path of this endpoint).
	chatRetentionAuditSurface = "admin-chat-retention"
)

// chatRetentionSweepJSON is the sweep tuning in the admin API.
type chatRetentionSweepJSON struct {
	ChunkSize           int `json:"chunk_size"`
	PauseMS             int `json:"pause_ms"`
	TickBudgetSeconds   int `json:"tick_budget_seconds"`
	TickIntervalMinutes int `json:"tick_interval_minutes"`
}

// chatRetentionResponse is the GET/PUT response of
// /api/v1/admin/chat-retention.
type chatRetentionResponse struct {
	Enabled bool `json:"enabled"`
	// MessagesRetentionDays is null when no window is set (keep forever).
	MessagesRetentionDays *int                   `json:"messages_retention_days"`
	Sweep                 chatRetentionSweepJSON `json:"sweep"`
	// LastAppliedCutoff is the cutoff the sweep last reached (every
	// eligible message older than it is gone); null until it reaches one.
	LastAppliedCutoff *time.Time `json:"last_applied_cutoff"`
	// Revision is the section revision, for expected_revision.
	Revision int64 `json:"revision"`
}

// chatRetentionSweepPut is the sweep part of a PUT: omitted = unchanged,
// null = compiled default.
type chatRetentionSweepPut struct {
	ChunkSize           *int `json:"chunk_size,omitempty"`
	PauseMS             *int `json:"pause_ms,omitempty"`
	TickBudgetSeconds   *int `json:"tick_budget_seconds,omitempty"`
	TickIntervalMinutes *int `json:"tick_interval_minutes,omitempty"`
}

// chatRetentionPutRequest is the body of PUT /api/v1/admin/chat-retention.
type chatRetentionPutRequest struct {
	Enabled               *bool                  `json:"enabled,omitempty"`
	MessagesRetentionDays *int                   `json:"messages_retention_days,omitempty"`
	Sweep                 *chatRetentionSweepPut `json:"sweep,omitempty"`
	ExpectedRevision      *int64                 `json:"expected_revision,omitempty"`
}

// handleAdminChatRetention handles GET/PUT /api/v1/admin/chat-retention,
// the write surface of the DB-only "chat_retention" operational settings
// section (hub.chat_retention). The route guard admits hub administrators
// with a session only (route_metadata.go: admin.chatRetention); the hub
// answers 404 while the experiment is off.
//
// PUT semantics:
//   - omitted field = unchanged; explicit null = compiled default (enabled
//     false, no window, default sweep tuning; "sweep": null resets all of
//     the tuning);
//   - changing enabled or messages_retention_days (both decide what is
//     deleted) requires expected_revision; a stale one is a 409;
//   - every save is written with a compare-and-set on the read revision and
//     is recorded in the mutation audit before it is written.
func (s *Server) handleAdminChatRetention(w http.ResponseWriter, r *http.Request) {
	// The method is checked first, so an unsupported method gets 405
	// whether or not the experiment is on (like handleGCSObject).
	if r.Method != http.MethodGet && r.Method != http.MethodPut {
		MethodNotAllowed(w, http.MethodGet, http.MethodPut)
		return
	}
	if !s.experimentEnabled(experiments.ChatRetention) {
		NotFound(w, "route")
		return
	}
	if r.Method == http.MethodGet {
		s.writeChatRetentionResponse(r.Context(), w)
		return
	}
	s.handlePutChatRetention(w, r)
}

func chatRetentionResponseFor(cfg opsettings.ChatRetentionConfig, revision int64, cutoff *time.Time) chatRetentionResponse {
	resp := chatRetentionResponse{
		Enabled: cfg.Enabled,
		Sweep: chatRetentionSweepJSON{
			ChunkSize:           cfg.ChunkSize,
			PauseMS:             cfg.PauseMS,
			TickBudgetSeconds:   cfg.TickBudgetSeconds,
			TickIntervalMinutes: cfg.TickIntervalMinutes,
		},
		Revision: revision,
	}
	if cfg.HasWindow() {
		d := cfg.MessagesRetentionDays
		resp.MessagesRetentionDays = &d
	}
	if cutoff != nil {
		c := cutoff.UTC()
		resp.LastAppliedCutoff = &c
	}
	return resp
}

func (s *Server) writeChatRetentionResponse(ctx context.Context, w http.ResponseWriter) {
	cfg := opsettings.DefaultChatRetentionConfig()
	var rev int64
	if ops := s.GetOperationalSettings(); ops != nil {
		cfg = ops.ChatRetention()
		rev = ops.SectionRevision(opsettings.ChatRetentionSectionName)
	}
	writeJSON(w, http.StatusOK, chatRetentionResponseFor(cfg, rev, s.chatRetentionLastAppliedCutoff(ctx, time.Now())))
}

func (s *Server) handlePutChatRetention(w http.ResponseWriter, r *http.Request) {
	ops := s.GetOperationalSettings()
	if ops == nil {
		writeError(w, http.StatusNotImplemented, "not_implemented",
			"Updating chat retention settings requires DB-backed operational settings", nil)
		return
	}
	rawBody, err := readRawBody(w, r)
	if err != nil {
		writeError(w, http.StatusBadRequest, ErrCodeInvalidRequest, "Invalid request body", nil)
		return
	}
	var body chatRetentionPutRequest
	dec := json.NewDecoder(bytes.NewReader(rawBody))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, ErrCodeInvalidRequest, "Invalid request body: "+err.Error(), nil)
		return
	}
	fp, fpErr := parseFieldPresence(rawBody)
	if fpErr != nil {
		writeError(w, http.StatusBadRequest, ErrCodeInvalidRequest, "Invalid request body", nil)
		return
	}

	cur := ops.ChatRetention()
	curRev := ops.SectionRevision(opsettings.ChatRetentionSectionName)

	// enabled and messages_retention_days decide what is deleted: a change
	// to either must name the revision it was made against.
	if fp.has("enabled") || fp.has("messages_retention_days") {
		if body.ExpectedRevision == nil {
			writeError(w, http.StatusBadRequest, ErrCodeInvalidRequest,
				"expected_revision is required when changing enabled or messages_retention_days", nil)
			return
		}
	}
	if body.ExpectedRevision != nil && *body.ExpectedRevision != curRev {
		writeError(w, http.StatusConflict, "revision_conflict",
			"The chat retention settings were modified concurrently. Please refresh and retry.", nil)
		return
	}

	next := cur
	if body.Enabled != nil {
		next.Enabled = *body.Enabled
	} else if fp.has("enabled") {
		next.Enabled = false
	}
	if body.MessagesRetentionDays != nil {
		d := *body.MessagesRetentionDays
		if d < opsettings.ChatRetentionMinDays || d > opsettings.ChatRetentionMaxDays {
			writeError(w, http.StatusBadRequest, ErrCodeInvalidRequest,
				fmt.Sprintf("messages_retention_days must be between %d and %d, or null to keep messages forever",
					opsettings.ChatRetentionMinDays, opsettings.ChatRetentionMaxDays), nil)
			return
		}
		next.MessagesRetentionDays = d
	} else if fp.has("messages_retention_days") {
		next.MessagesRetentionDays = 0
	}
	if fp.has("sweep") {
		def := opsettings.DefaultChatRetentionConfig()
		if body.Sweep == nil {
			next.ChunkSize, next.PauseMS, next.TickBudgetSeconds, next.TickIntervalMinutes =
				def.ChunkSize, def.PauseMS, def.TickBudgetSeconds, def.TickIntervalMinutes
		} else {
			sp := fp.nestedPresence("sweep")
			applyInt := func(v *int, key string, dst *int, dflt int) {
				if v != nil {
					*dst = *v
				} else if sp.has(key) {
					*dst = dflt
				}
			}
			applyInt(body.Sweep.ChunkSize, "chunk_size", &next.ChunkSize, def.ChunkSize)
			applyInt(body.Sweep.PauseMS, "pause_ms", &next.PauseMS, def.PauseMS)
			applyInt(body.Sweep.TickBudgetSeconds, "tick_budget_seconds", &next.TickBudgetSeconds, def.TickBudgetSeconds)
			applyInt(body.Sweep.TickIntervalMinutes, "tick_interval_minutes", &next.TickIntervalMinutes, def.TickIntervalMinutes)
		}
	}

	settings := next.Settings()
	if _, err := settings.Resolve(); err != nil {
		writeError(w, http.StatusBadRequest, ErrCodeInvalidRequest, err.Error(), nil)
		return
	}
	doc, err := json.Marshal(settings)
	if err != nil {
		writeError(w, http.StatusInternalServerError, ErrCodeInternalError, "Failed to marshal chat retention settings", nil)
		return
	}
	if errs := opsettings.Validate(opsettings.ChatRetentionSectionName, doc); len(errs) > 0 {
		writeJSON(w, http.StatusBadRequest, map[string]interface{}{
			"error":  "validation_failed",
			"errors": errs,
		})
		return
	}

	changed := next != cur
	if changed {
		// Recorded before the write: a save that cannot be audited is
		// refused.
		if err := s.auditChatRetentionChange(r.Context(), gcpIAMAuditMutation, cur, next); err != nil {
			slog.Error("PUT chat retention: audit record failed; settings not changed", "error", err)
			writeError(w, http.StatusInternalServerError, ErrCodeInternalError,
				"Failed to record the chat retention change; settings not changed", nil)
			return
		}
	}

	updatedBy := ""
	if caller := GetUserIdentityFromContext(r.Context()); caller != nil {
		updatedBy = caller.Email()
	}
	if _, err := ops.Update(r.Context(), opsettings.ChatRetentionSectionName, doc, updatedBy, curRev, "managed"); err != nil {
		if changed {
			if aerr := s.auditChatRetentionChange(r.Context(), gcpIAMNotAppliedMutation, cur, next); aerr != nil {
				slog.Error("PUT chat retention: not-applied audit record failed", "error", aerr)
			}
		}
		switch {
		case errors.Is(err, store.ErrRevisionConflict):
			writeError(w, http.StatusConflict, "revision_conflict",
				"The chat retention settings were modified concurrently. Please refresh and retry.", nil)
		case errors.Is(err, ErrSectionValidation):
			writeError(w, http.StatusBadRequest, ErrCodeInvalidRequest, err.Error(), nil)
		default:
			slog.Error("Failed to update chat retention settings", "error", err)
			writeError(w, http.StatusInternalServerError, ErrCodeInternalError, "Failed to update chat retention settings", nil)
		}
		return
	}
	s.writeChatRetentionResponse(r.Context(), w)
}

// chatRetentionAuditSummary is the before/after summary of an audit record.
func chatRetentionAuditSummary(c opsettings.ChatRetentionConfig) string {
	days := "null"
	if c.HasWindow() {
		days = strconv.Itoa(c.MessagesRetentionDays)
	}
	return fmt.Sprintf("enabled=%t,messages_retention_days=%s,chunk_size=%d,pause_ms=%d,tick_budget_seconds=%d,tick_interval_minutes=%d",
		c.Enabled, days, c.ChunkSize, c.PauseMS, c.TickBudgetSeconds, c.TickIntervalMinutes)
}

// auditChatRetentionChange writes one mutation audit record for a change of
// the chat_retention section, in the same shape as the other settings
// audit records (hub_setting_update on target hub_setting).
func (s *Server) auditChatRetentionChange(ctx context.Context, mutation string, cur, next opsettings.ChatRetentionConfig) error {
	if s.store == nil {
		return errors.New("no store for the audit record")
	}
	rec := &store.MutationAuditRecord{
		MutationType:  mutation,
		TargetType:    gcpIAMAuditTarget,
		TargetID:      opsettings.ChatRetentionSectionName,
		BeforeSummary: chatRetentionAuditSummary(cur),
		AfterSummary:  chatRetentionAuditSummary(next),
		ExecutorKind:  gcpIAMSurfaceKind,
		ExecutorID:    chatRetentionAuditSurface,
	}
	auditActorFromContext(ctx).ApplyActor(rec)
	if rec.ActorPrincipalKind == "" || rec.ActorPrincipalID == "" {
		return errors.New("recording chat_retention change: no actor")
	}
	rec.Timestamp = time.Now().UTC()
	if err := s.store.CreateMutationAudit(ctx, rec); err != nil {
		return fmt.Errorf("recording chat_retention change: %w", err)
	}
	slog.Warn("chat retention setting changed",
		"before", rec.BeforeSummary, "after", rec.AfterSummary, "mutation", mutation,
		"principal_kind", rec.ActorPrincipalKind, "principal", rec.ActorPrincipalID, "audit_id", rec.ID)
	return nil
}
