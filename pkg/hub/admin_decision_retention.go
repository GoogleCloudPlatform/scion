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
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/config/opsettings"
	"github.com/GoogleCloudPlatform/scion/pkg/experiments"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

const (
	// decisionRetentionAuditSurface is the ExecutorID of decision_retention
	// mutation audit records (design-8984 §3.1).
	decisionRetentionAuditSurface = "admin-decision-retention"
	decisionRetentionHistoryRuns  = 10
)

type decisionRetentionSweepJSON struct {
	PageSize            int `json:"page_size"`
	TickBudgetSeconds   int `json:"tick_budget_seconds"`
	TickIntervalMinutes int `json:"tick_interval_minutes"`
}

type decisionRetentionRunJSON struct {
	ID            string                                   `json:"id"`
	Trigger       string                                   `json:"trigger"`
	DryRun        bool                                     `json:"dry_run"`
	StartedAt     time.Time                                `json:"started_at"`
	FinishedAt    *time.Time                               `json:"finished_at"`
	Status        string                                   `json:"status"`
	Actor         string                                   `json:"actor,omitempty"`
	Error         string                                   `json:"error,omitempty"`
	Projects      int                                      `json:"projects"`
	Totals        store.DecisionRetentionCounts            `json:"totals"`
	Counts        map[string]store.DecisionRetentionCounts `json:"counts,omitempty"`
	RetentionDays int                                      `json:"retention_days"`
}

type decisionRetentionResponse struct {
	Enabled          bool                                            `json:"enabled"`
	RetentionDays    *int                                            `json:"retention_days"`
	ProjectOverrides map[string]opsettings.DecisionRetentionOverride `json:"project_overrides"`
	Sweep            decisionRetentionSweepJSON                      `json:"sweep"`
	Revision         int64                                           `json:"revision"`
	Runs             []decisionRetentionRunJSON                      `json:"runs"`
}

type decisionRetentionSweepPut struct {
	PageSize            *int `json:"page_size,omitempty"`
	TickBudgetSeconds   *int `json:"tick_budget_seconds,omitempty"`
	TickIntervalMinutes *int `json:"tick_interval_minutes,omitempty"`
}

type decisionRetentionPutRequest struct {
	Enabled          *bool                                           `json:"enabled,omitempty"`
	RetentionDays    *int                                            `json:"retention_days,omitempty"`
	ProjectOverrides map[string]opsettings.DecisionRetentionOverride `json:"project_overrides,omitempty"`
	Sweep            *decisionRetentionSweepPut                      `json:"sweep,omitempty"`
	ExpectedRevision *int64                                          `json:"expected_revision,omitempty"`
}

// handleAdminDecisionRetention handles GET/PUT /api/v1/admin/decision-retention,
// the write surface of the DB-only "decision_retention" operational settings
// section (hub.decision_retention, design-8984 §3.1). Hub admins with a session
// only (route_metadata.go: admin.decisionRetention); answers 404 while the
// experiment is off.
func (s *Server) handleAdminDecisionRetention(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodPut {
		MethodNotAllowed(w, http.MethodGet, http.MethodPut)
		return
	}
	if !s.experimentEnabled(experiments.DecisionRetention) {
		NotFound(w, "route")
		return
	}
	if r.Method == http.MethodGet {
		s.writeDecisionRetentionResponse(r.Context(), w)
		return
	}
	s.handlePutDecisionRetention(w, r)
}

func decisionRetentionRunJSONFor(r store.DecisionRetentionRun) decisionRetentionRunJSON {
	out := decisionRetentionRunJSON{
		ID:            r.ID,
		Trigger:       r.Trigger,
		DryRun:        r.DryRun,
		StartedAt:     r.StartedAt.UTC(),
		Status:        r.Status,
		Actor:         r.Actor,
		Error:         r.Error,
		RetentionDays: r.Policy.RetentionDays,
	}
	if r.FinishedAt != nil {
		t := r.FinishedAt.UTC()
		out.FinishedAt = &t
	}
	for pid, c := range r.Counts {
		out.Totals.Add(c)
		if c.Decisions > 0 || c.Answers > 0 {
			out.Projects++
			if out.Counts == nil {
				out.Counts = make(map[string]store.DecisionRetentionCounts)
			}
			out.Counts[pid] = c
		}
	}
	return out
}

func (s *Server) writeDecisionRetentionResponse(ctx context.Context, w http.ResponseWriter) {
	cfg := opsettings.DefaultDecisionRetentionConfig()
	var rev int64
	if ops := s.GetOperationalSettings(); ops != nil {
		cfg = ops.DecisionRetention()
		rev = ops.SectionRevision(opsettings.DecisionRetentionSectionName)
	}
	overrides := make(map[string]opsettings.DecisionRetentionOverride)
	if len(cfg.ProjectOverrides) > 0 {
		live, err := s.filterLiveProjectOverrides(ctx, cfg.ProjectOverrides)
		if err != nil {
			slog.Warn("decision retention: filter live project overrides", "error", err)
			for k, v := range cfg.ProjectOverrides {
				overrides[k] = v.Clone()
			}
		} else if live != nil {
			overrides = live
		}
	}
	resp := decisionRetentionResponse{
		Enabled:          cfg.Enabled,
		ProjectOverrides: overrides,
		Sweep: decisionRetentionSweepJSON{
			PageSize:            cfg.PageSize,
			TickBudgetSeconds:   cfg.TickBudgetSeconds,
			TickIntervalMinutes: cfg.TickIntervalMinutes,
		},
		Revision: rev,
		Runs:     []decisionRetentionRunJSON{},
	}
	if cfg.HasWindow() {
		d := cfg.RetentionDays
		resp.RetentionDays = &d
	}
	if st := s.decisionRetentionStore(); st != nil {
		runs, err := st.ListDecisionRetentionRuns(ctx, store.DecisionRetentionRunFilter{Limit: decisionRetentionHistoryRuns})
		if err != nil {
			slog.Warn("decision retention: read run history", "error", err)
		} else {
			for _, r := range runs {
				resp.Runs = append(resp.Runs, decisionRetentionRunJSONFor(r))
			}
		}
	}
	writeJSON(w, http.StatusOK, resp)
}

func (s *Server) handlePutDecisionRetention(w http.ResponseWriter, r *http.Request) {
	ops := s.GetOperationalSettings()
	if ops == nil {
		writeError(w, http.StatusNotImplemented, "not_implemented",
			"Updating decision retention settings requires DB-backed operational settings", nil)
		return
	}
	rawBody, err := readRawBody(w, r)
	if err != nil {
		writeError(w, http.StatusBadRequest, ErrCodeInvalidRequest, "Invalid request body", nil)
		return
	}
	var body decisionRetentionPutRequest
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

	cur := ops.DecisionRetention()
	curRev := ops.SectionRevision(opsettings.DecisionRetentionSectionName)

	if fp.has("enabled") || fp.has("retention_days") || fp.has("project_overrides") {
		if body.ExpectedRevision == nil {
			writeError(w, http.StatusBadRequest, ErrCodeInvalidRequest,
				"expected_revision is required when changing enabled, retention_days, or project_overrides", nil)
			return
		}
	}
	if body.ExpectedRevision != nil && *body.ExpectedRevision != curRev {
		writeError(w, http.StatusConflict, "revision_conflict",
			"The decision retention settings were modified concurrently. Please refresh and retry.", nil)
		return
	}

	next := cur.Clone()
	if body.Enabled != nil {
		next.Enabled = *body.Enabled
	} else if fp.has("enabled") {
		next.Enabled = false
	}
	if body.RetentionDays != nil {
		d := *body.RetentionDays
		if d < opsettings.DecisionRetentionMinDays || d > opsettings.DecisionRetentionMaxDays {
			writeError(w, http.StatusBadRequest, ErrCodeInvalidRequest,
				fmt.Sprintf("retention_days must be between %d and %d, or null to keep decisions forever",
					opsettings.DecisionRetentionMinDays, opsettings.DecisionRetentionMaxDays), nil)
			return
		}
		next.RetentionDays = d
	} else if fp.has("retention_days") {
		next.RetentionDays = 0
	}
	if fp.has("project_overrides") {
		if len(body.ProjectOverrides) == 0 {
			next.ProjectOverrides = nil
		} else {
			next.ProjectOverrides = make(map[string]opsettings.DecisionRetentionOverride, len(body.ProjectOverrides))
			for k, v := range body.ProjectOverrides {
				next.ProjectOverrides[k] = v.Clone()
			}
		}
	}
	if fp.has("sweep") {
		def := opsettings.DefaultDecisionRetentionConfig()
		if body.Sweep == nil {
			next.PageSize, next.TickBudgetSeconds, next.TickIntervalMinutes =
				def.PageSize, def.TickBudgetSeconds, def.TickIntervalMinutes
		} else {
			sp := fp.nestedPresence("sweep")
			applyInt := func(v *int, key string, dst *int, dflt int) {
				if v != nil {
					*dst = *v
				} else if sp.has(key) {
					*dst = dflt
				}
			}
			applyInt(body.Sweep.PageSize, "page_size", &next.PageSize, def.PageSize)
			applyInt(body.Sweep.TickBudgetSeconds, "tick_budget_seconds", &next.TickBudgetSeconds, def.TickBudgetSeconds)
			applyInt(body.Sweep.TickIntervalMinutes, "tick_interval_minutes", &next.TickIntervalMinutes, def.TickIntervalMinutes)
		}
	}

	// Validate all fields (including UUID syntax and override bounds) before
	// pruning overrides for deleted projects.
	settings := next.Settings()
	if _, err := settings.Resolve(); err != nil {
		writeError(w, http.StatusBadRequest, ErrCodeInvalidRequest, err.Error(), nil)
		return
	}
	if len(next.ProjectOverrides) > 0 {
		pruned, err := s.filterLiveProjectOverrides(r.Context(), next.ProjectOverrides)
		if err != nil {
			slog.Error("PUT decision retention: failed to verify projects for overrides", "error", err)
			writeError(w, http.StatusInternalServerError, ErrCodeInternalError, "Failed to verify projects for retention overrides", nil)
			return
		}
		// Reject unknown project IDs newly submitted in this request so a
		// mistyped keep_forever hold fails visibly instead of vanishing, while
		// still silently pruning previously stored keys for deleted projects.
		if fp.has("project_overrides") {
			missing := make([]string, 0)
			for pid := range next.ProjectOverrides {
				if _, isLive := pruned[pid]; isLive {
					continue
				}
				if _, wasStored := cur.ProjectOverrides[pid]; wasStored {
					continue
				}
				missing = append(missing, pid)
			}
			if len(missing) > 0 {
				sort.Strings(missing)
				fieldErrs := make([]config.ValidationError, len(missing))
				for i, pid := range missing {
					fieldErrs[i] = config.ValidationError{
						Path:    "project_overrides." + pid,
						Message: "project not found",
					}
				}
				writeJSON(w, http.StatusBadRequest, map[string]interface{}{
					"error":  "validation_failed",
					"errors": fieldErrs,
				})
				return
			}
		}
		next.ProjectOverrides = pruned
		settings = next.Settings()
	}

	doc, err := json.Marshal(settings)
	if err != nil {
		writeError(w, http.StatusInternalServerError, ErrCodeInternalError, "Failed to marshal decision retention settings", nil)
		return
	}
	if errs := opsettings.Validate(opsettings.DecisionRetentionSectionName, doc); len(errs) > 0 {
		writeJSON(w, http.StatusBadRequest, map[string]interface{}{
			"error":  "validation_failed",
			"errors": errs,
		})
		return
	}

	changed := !next.Equal(cur)
	if changed {
		if err := s.auditDecisionRetentionChange(r.Context(), gcpIAMAuditMutation, cur, next); err != nil {
			slog.Error("PUT decision retention: audit record failed; settings not changed", "error", err)
			writeError(w, http.StatusInternalServerError, ErrCodeInternalError,
				"Failed to record the decision retention change; settings not changed", nil)
			return
		}
	}

	updatedBy := ""
	if caller := GetUserIdentityFromContext(r.Context()); caller != nil {
		updatedBy = caller.Email()
	}
	if _, err := ops.Update(r.Context(), opsettings.DecisionRetentionSectionName, doc, updatedBy, curRev, "managed"); err != nil {
		if changed {
			if aerr := s.auditDecisionRetentionChange(r.Context(), gcpIAMNotAppliedMutation, cur, next); aerr != nil {
				slog.Error("PUT decision retention: not-applied audit record failed", "error", aerr)
			}
		}
		switch {
		case errors.Is(err, store.ErrRevisionConflict):
			writeError(w, http.StatusConflict, "revision_conflict",
				"The decision retention settings were modified concurrently. Please refresh and retry.", nil)
		case errors.Is(err, ErrSectionValidation):
			writeError(w, http.StatusBadRequest, ErrCodeInvalidRequest, err.Error(), nil)
		default:
			slog.Error("Failed to update decision retention settings", "error", err)
			writeError(w, http.StatusInternalServerError, ErrCodeInternalError, "Failed to update decision retention settings", nil)
		}
		return
	}
	s.writeDecisionRetentionResponse(r.Context(), w)
}

// filterLiveProjectOverrides returns a copy of overrides containing only
// projects that still exist in s.store, dropping overrides for deleted
// projects (design-8984 §3.1).
func (s *Server) filterLiveProjectOverrides(
	ctx context.Context,
	overrides map[string]opsettings.DecisionRetentionOverride,
) (map[string]opsettings.DecisionRetentionOverride, error) {
	if len(overrides) == 0 || s.store == nil {
		return nil, nil
	}
	ids := make([]string, 0, len(overrides))
	for pid := range overrides {
		ids = append(ids, pid)
	}
	sort.Strings(ids)
	live := make(map[string]bool, len(ids))
	for start := 0; start < len(ids); start += profileProjectSummariesBatchSize {
		end := start + profileProjectSummariesBatchSize
		if end > len(ids) {
			end = len(ids)
		}
		batch := ids[start:end]
		res, err := s.store.ListProjectSummaries(ctx, store.ProjectFilter{MemberProjectIDs: batch}, store.ListOptions{Limit: len(batch), SkipTotalCount: true})
		if err != nil {
			return nil, err
		}
		for _, p := range res.Items {
			live[p.ID] = true
		}
	}
	out := make(map[string]opsettings.DecisionRetentionOverride, len(live))
	for pid, ov := range overrides {
		if live[pid] {
			out[pid] = ov.Clone()
		}
	}
	if len(out) == 0 {
		return nil, nil
	}
	return out, nil
}

func decisionRetentionAuditSummary(c opsettings.DecisionRetentionConfig) string {
	days := "null"
	if c.HasWindow() {
		days = strconv.Itoa(c.RetentionDays)
	}
	overrides := "none"
	if len(c.ProjectOverrides) > 0 {
		pids := make([]string, 0, len(c.ProjectOverrides))
		for pid := range c.ProjectOverrides {
			pids = append(pids, pid)
		}
		sort.Strings(pids)
		parts := make([]string, 0, len(pids))
		for _, pid := range pids {
			ov := c.ProjectOverrides[pid]
			if ov.Mode == opsettings.DecisionRetentionOverrideModeDays && ov.Days != nil {
				parts = append(parts, fmt.Sprintf("%s:%dd", pid, *ov.Days))
			} else {
				parts = append(parts, fmt.Sprintf("%s:%s", pid, ov.Mode))
			}
		}
		overrides = strings.Join(parts, ";")
	}
	return fmt.Sprintf("enabled=%t,retention_days=%s,project_overrides=%s,page_size=%d,tick_budget_seconds=%d,tick_interval_minutes=%d",
		c.Enabled, days, overrides, c.PageSize, c.TickBudgetSeconds, c.TickIntervalMinutes)
}

func (s *Server) auditDecisionRetentionChange(ctx context.Context, mutation string, cur, next opsettings.DecisionRetentionConfig) error {
	if s.store == nil {
		return errors.New("no store for the audit record")
	}
	rec := &store.MutationAuditRecord{
		MutationType:  mutation,
		TargetType:    gcpIAMAuditTarget,
		TargetID:      opsettings.DecisionRetentionSectionName,
		BeforeSummary: decisionRetentionAuditSummary(cur),
		AfterSummary:  decisionRetentionAuditSummary(next),
		ExecutorKind:  gcpIAMSurfaceKind,
		ExecutorID:    decisionRetentionAuditSurface,
	}
	auditActorFromContext(ctx).ApplyActor(rec)
	if rec.ActorPrincipalKind == "" || rec.ActorPrincipalID == "" {
		return errors.New("recording decision_retention change: no actor")
	}
	rec.Timestamp = time.Now().UTC()
	if err := s.store.CreateMutationAudit(ctx, rec); err != nil {
		return fmt.Errorf("recording decision_retention change: %w", err)
	}
	slog.Warn("decision retention setting changed",
		"before", rec.BeforeSummary, "after", rec.AfterSummary, "mutation", mutation,
		"principal_kind", rec.ActorPrincipalKind, "principal", rec.ActorPrincipalID, "audit_id", rec.ID)
	return nil
}
