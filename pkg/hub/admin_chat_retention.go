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
	MessagesRetentionDays *int `json:"messages_retention_days"`
	// DeletedConversationGraceDays is how long after a conversation or
	// topic is deleted its messages are purged (while enabled), whatever
	// the window.
	DeletedConversationGraceDays int                    `json:"deleted_conversation_grace_days"`
	Sweep                        chatRetentionSweepJSON `json:"sweep"`
	// LastAppliedCutoff is the cutoff the sweep last reached (every
	// eligible message older than it is gone); null until it reaches one.
	LastAppliedCutoff *time.Time `json:"last_applied_cutoff"`
	// Revision is the section revision, for expected_revision.
	Revision int64 `json:"revision"`
	// Runs are the most recent chat_retention_runs rows, newest first
	// (sweep, manual and preview runs).
	Runs []chatRetentionRunJSON `json:"runs"`
	// LastPreview is the most recent preview, or null when none was run
	// (or none is kept).
	LastPreview *chatRetentionRunJSON `json:"last_preview"`
	// PurgedSincePreview is how many messages the sweep and manual runs
	// that finished after the last preview started deleted; the panel's
	// "backlog draining" estimate is the preview's messages minus this.
	// 0 without a preview.
	PurgedSincePreview int64 `json:"purged_since_preview"`
}

// chatRetentionRunJSON is one chat_retention_runs row in the admin API: its
// totals, never per-project detail, a message ID or content.
type chatRetentionRunJSON struct {
	ID         string     `json:"id"`
	Trigger    string     `json:"trigger"`
	DryRun     bool       `json:"dry_run"`
	StartedAt  time.Time  `json:"started_at"`
	FinishedAt *time.Time `json:"finished_at"`
	Status     string     `json:"status"`
	// Actor is the user ID of the administrator who started a manual run
	// or a preview; "" for a sweep.
	Actor string `json:"actor,omitempty"`
	Error string `json:"error,omitempty"`
	// Messages is the messages deleted (or, for a preview, that would be
	// deleted): by the window plus of deleted conversations, plus failed
	// agent messages for a sweep run.
	Messages int64 `json:"messages"`
	// Projects counts the projects with at least one such message.
	Projects int `json:"projects"`
	// Totals are the run's counts summed over every project.
	Totals store.ChatRetentionCounts `json:"totals"`
	// MessagesRetentionDays is the window the run applied (0 = none) and
	// DeletedConversationGraceDays its grace.
	MessagesRetentionDays        int `json:"messages_retention_days"`
	DeletedConversationGraceDays int `json:"deleted_conversation_grace_days"`
}

// chatRetentionHistoryRuns is how many runs the admin response lists.
const chatRetentionHistoryRuns = 10

// chatRetentionRunMessages is the messages a run counted: by the window,
// of deleted conversations and failed agent messages.
func chatRetentionRunMessages(c store.ChatRetentionCounts) int64 {
	return c.Main + c.DeletedConv + c.Failed
}

func chatRetentionRunJSONFor(r store.ChatRetentionRun) chatRetentionRunJSON {
	out := chatRetentionRunJSON{
		ID: r.ID, Trigger: r.Trigger, DryRun: r.DryRun, StartedAt: r.StartedAt.UTC(), Status: r.Status,
		Actor: r.Actor, Error: r.Error,
		MessagesRetentionDays:        r.Policy.MessagesRetentionDays,
		DeletedConversationGraceDays: r.Policy.DeletedConversationGraceDays,
	}
	if r.FinishedAt != nil {
		t := r.FinishedAt.UTC()
		out.FinishedAt = &t
	}
	for _, c := range r.Counts {
		out.Totals.Add(c)
		if chatRetentionRunMessages(c) > 0 {
			out.Projects++
		}
	}
	out.Messages = chatRetentionRunMessages(out.Totals)
	return out
}

// chatRetentionHistory fills the run history, the last preview and the
// messages purged since it. It reads at most a few small indexed pages of
// chat_retention_runs (about one row a day), never the messages. A failed
// read is logged and leaves the history empty.
func (s *Server) chatRetentionHistory(ctx context.Context, resp *chatRetentionResponse) {
	resp.Runs = []chatRetentionRunJSON{}
	st := s.chatRetentionStore()
	if st == nil {
		return
	}
	runs, err := st.ListChatRetentionRuns(ctx, store.ChatRetentionRunFilter{Limit: chatRetentionHistoryRuns})
	if err != nil {
		slog.Warn("chat retention: read run history", "error", err)
		return
	}
	for _, r := range runs {
		resp.Runs = append(resp.Runs, chatRetentionRunJSONFor(r))
	}
	previews, err := st.ListChatRetentionRuns(ctx, store.ChatRetentionRunFilter{Trigger: store.ChatRetentionTriggerPreview, Limit: 1})
	if err != nil {
		slog.Warn("chat retention: read last preview", "error", err)
		return
	}
	if len(previews) == 0 {
		return
	}
	last := chatRetentionRunJSONFor(previews[0])
	resp.LastPreview = &last
	// Runs that finished after the preview started: a sweep run open
	// since before it counts whole, so the estimate errs towards a
	// smaller backlog.
	since, err := st.ListChatRetentionRuns(ctx, store.ChatRetentionRunFilter{FinishedAfter: previews[0].StartedAt})
	if err != nil {
		slog.Warn("chat retention: read runs since the last preview", "error", err)
		return
	}
	for _, r := range since {
		if r.DryRun {
			continue
		}
		var t store.ChatRetentionCounts
		for _, c := range r.Counts {
			t.Add(c)
		}
		resp.PurgedSincePreview += t.Main + t.DeletedConv
	}
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
	Enabled                      *bool                  `json:"enabled,omitempty"`
	MessagesRetentionDays        *int                   `json:"messages_retention_days,omitempty"`
	DeletedConversationGraceDays *int                   `json:"deleted_conversation_grace_days,omitempty"`
	Sweep                        *chatRetentionSweepPut `json:"sweep,omitempty"`
	ExpectedRevision             *int64                 `json:"expected_revision,omitempty"`
}

// handleAdminChatRetention handles GET/PUT /api/v1/admin/chat-retention,
// the write surface of the DB-only "chat_retention" operational settings
// section (hub.chat_retention). The route guard admits hub administrators
// with a session only (route_metadata.go: admin.chatRetention); the hub
// answers 404 while the experiment is off.
//
// PUT semantics:
//   - omitted field = unchanged; explicit null = compiled default (enabled
//     false, no window, a 30-day grace, default sweep tuning; "sweep": null
//     resets all of the tuning);
//   - changing enabled, messages_retention_days or
//     deleted_conversation_grace_days (each decides what is deleted)
//     requires expected_revision; a stale one is a 409;
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
		Enabled:                      cfg.Enabled,
		DeletedConversationGraceDays: cfg.DeletedConversationGraceDays,
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
	resp := chatRetentionResponseFor(cfg, rev, s.chatRetentionLastAppliedCutoff(ctx, time.Now()))
	s.chatRetentionHistory(ctx, &resp)
	writeJSON(w, http.StatusOK, resp)
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

	// enabled, messages_retention_days and deleted_conversation_grace_days
	// decide what is deleted: a change to any of them must name the
	// revision it was made against.
	if fp.has("enabled") || fp.has("messages_retention_days") || fp.has("deleted_conversation_grace_days") {
		if body.ExpectedRevision == nil {
			writeError(w, http.StatusBadRequest, ErrCodeInvalidRequest,
				"expected_revision is required when changing enabled, messages_retention_days or deleted_conversation_grace_days", nil)
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
	if body.DeletedConversationGraceDays != nil {
		g := *body.DeletedConversationGraceDays
		if g < opsettings.ChatRetentionMinGraceDays || g > opsettings.ChatRetentionMaxDays {
			writeError(w, http.StatusBadRequest, ErrCodeInvalidRequest,
				fmt.Sprintf("deleted_conversation_grace_days must be between %d and %d",
					opsettings.ChatRetentionMinGraceDays, opsettings.ChatRetentionMaxDays), nil)
			return
		}
		next.DeletedConversationGraceDays = g
	} else if fp.has("deleted_conversation_grace_days") {
		next.DeletedConversationGraceDays = opsettings.ChatRetentionDefaultGraceDays
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
	return fmt.Sprintf("enabled=%t,messages_retention_days=%s,deleted_conversation_grace_days=%d,chunk_size=%d,pause_ms=%d,tick_budget_seconds=%d,tick_interval_minutes=%d",
		c.Enabled, days, c.DeletedConversationGraceDays, c.ChunkSize, c.PauseMS, c.TickBudgetSeconds, c.TickIntervalMinutes)
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
