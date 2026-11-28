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
	"log/slog"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// UsageBudgetEntry represents one applicable raw-token budget for the active
// UTC day or month window. JSON field names use snake_case to match the Track 1
// usage read APIs (GET /api/v1/{projects,users}/{id}/usage, /api/v1/me/usage).
type UsageBudgetEntry struct {
	ID                 string     `json:"id,omitempty"`
	LimitDefinitionID  string     `json:"limit_definition_id"`
	LimitName          string     `json:"limit_name"`
	ScopeKind          string     `json:"scope_kind"`
	ScopeID            string     `json:"scope_id"`
	ProjectScopeID     string     `json:"project_scope_id,omitempty"`
	WindowStartUTC     time.Time  `json:"window_start_utc"`
	WindowEndUTC       time.Time  `json:"window_end_utc"`
	Cap                *int64     `json:"cap"`
	TokenAction        string     `json:"token_action"`
	Consumed           int64      `json:"consumed"`
	State              string     `json:"state"`
	ThresholdsNotified int64      `json:"thresholds_notified"`
	OverrideUntil      *time.Time `json:"override_until"`
	StateVersion       int64      `json:"state_version"`
	AsOf               time.Time  `json:"as_of"`
}

// UsageBudgetsResponse is returned by:
//   - GET /api/v1/projects/{id}/usage/budgets
//   - GET /api/v1/users/{id}/usage/budgets
//   - GET /api/v1/me/usage/budgets
type UsageBudgetsResponse struct {
	Scope   UsageScopeRef      `json:"scope"`
	AsOf    time.Time          `json:"as_of"`
	Budgets []UsageBudgetEntry `json:"budgets"`
}

// overrideBudgetRequest is the payload for:
//   - POST /api/v1/projects/{id}/usage/budgets/{budgetId}:override
//   - POST /api/v1/users/{id}/usage/budgets/{budgetId}:override
//
// OverrideUntil is decoded as json.RawMessage so the handler can distinguish
// an omitted key (400 Bad Request) from an explicit JSON null (clear override).
type overrideBudgetRequest struct {
	OverrideUntil json.RawMessage `json:"override_until"`
	StateVersion  *int64          `json:"state_version,omitempty"`
	Reason        string          `json:"reason,omitempty"`
}

// parseUsageBudgetOverrideSubpath parses "usage/budgets/{budgetId}:override"
// into budgetID. Returns ("", false) for any other path shape.
func parseUsageBudgetOverrideSubpath(subPath string) (string, bool) {
	const prefix = "usage/budgets/"
	const suffix = ":override"
	if !strings.HasPrefix(subPath, prefix) || !strings.HasSuffix(subPath, suffix) {
		return "", false
	}
	budgetID := strings.TrimSuffix(strings.TrimPrefix(subPath, prefix), suffix)
	if budgetID == "" || strings.Contains(budgetID, "/") {
		return "", false
	}
	return budgetID, true
}

// handleProjectUsageBudgets serves GET /api/v1/projects/{id}/usage/budgets.
// Gated behind tokenomics.usage_ledger AND tokenomics.token_budgets (404 when
// either is off). Authorized for project members with project.read and
// same-project agent tokens, matching GET /api/v1/projects/{id}/usage.
func (s *Server) handleProjectUsageBudgets(w http.ResponseWriter, r *http.Request, projectID string) {
	if !s.tokenBudgetsEnabled() {
		NotFound(w, "route")
		return
	}
	if r.Method != http.MethodGet {
		MethodNotAllowed(w, http.MethodGet)
		return
	}
	if !checkAgentReadScope(w, r) {
		return
	}

	ctx := r.Context()
	identity := GetIdentityFromContext(ctx)
	if identity == nil {
		Unauthorized(w)
		return
	}

	project, err := s.store.GetProject(ctx, projectID)
	if err != nil {
		writeErrorFromErr(w, err, "")
		return
	}
	if project == nil {
		NotFound(w, "Project")
		return
	}

	switch ident := identity.(type) {
	case UserIdentity:
		decision := s.authzService.CheckAccess(ctx, ident, Resource{
			Type:    "project",
			ID:      project.ID,
			OwnerID: project.OwnerID,
		}, ActionRead)
		if !decision.Allowed {
			Forbidden(w)
			return
		}
	case AgentIdentity:
		if ident.ProjectID() != project.ID {
			Forbidden(w)
			return
		}
	default:
		Forbidden(w)
		return
	}

	asOf, err := s.store.DBNow(ctx)
	if err != nil {
		writeErrorFromErr(w, err, "")
		return
	}
	asOf = asOf.UTC()
	dayStart, dayEnd := utcDayWindow(asOf)
	monthStart, monthEnd := utcMonthWindow(asOf)

	specs := []budgetEvalSpec{
		{
			limitName:      store.LimitLLMTokensPerProjectDay,
			scopeKind:      store.UsageScopeKindProject,
			scopeID:        project.ID,
			counterScopeID: project.ID,
			bindingScope:   store.QuotaScopeProject,
			bindingScopeID: project.ID,
			windowStartUTC: dayStart,
			windowEndUTC:   dayEnd,
		},
		{
			limitName:      store.LimitLLMTokensPerProjectMonth,
			scopeKind:      store.UsageScopeKindProject,
			scopeID:        project.ID,
			counterScopeID: project.ID,
			bindingScope:   store.QuotaScopeProject,
			bindingScopeID: project.ID,
			windowStartUTC: monthStart,
			windowEndUTC:   monthEnd,
		},
	}

	entries, err := s.buildBudgetEntries(ctx, newTokenLimitEvalCache(s.quotaService), nil, nil, specs, asOf, true)
	if err != nil {
		writeErrorFromErr(w, err, "")
		return
	}

	writeJSON(w, http.StatusOK, UsageBudgetsResponse{
		Scope:   UsageScopeRef{Kind: store.UsageScopeKindProject, ID: project.ID},
		AsOf:    asOf,
		Budgets: entries,
	})
}

// handleMeUsageBudgets serves GET /api/v1/me/usage/budgets.
// Gated behind tokenomics.usage_ledger AND tokenomics.token_budgets (404 when
// either is off). Authorized for any authenticated UserIdentity (session JWT or
// scoped UAT); agents receive 403. A project-boundary UAT restricts user_project
// rows to its boundary project.
func (s *Server) handleMeUsageBudgets(w http.ResponseWriter, r *http.Request) {
	if !s.tokenBudgetsEnabled() {
		NotFound(w, "route")
		return
	}
	if r.Method != http.MethodGet {
		MethodNotAllowed(w, http.MethodGet)
		return
	}

	ctx := r.Context()
	identity := GetIdentityFromContext(ctx)
	if identity == nil {
		Unauthorized(w)
		return
	}
	userIdent, ok := identity.(UserIdentity)
	if !ok {
		Forbidden(w)
		return
	}
	token, admitted := selfReadCredential(ctx, userIdent, "hub.usage.budgets.self.read")
	if !admitted {
		refuseSelfReadCredential(w, r, userIdent, "usage budgets")
		return
	}
	boundaryProjectID := ""
	if token != nil {
		boundaryProjectID = tokenBoundaryProject(token)
	}

	s.serveUserUsageBudgets(w, r, userIdent, userIdent.ID(), boundaryProjectID)
}

// handleUserUsageBudgets serves GET /api/v1/users/{id}/usage/budgets.
// Gated behind tokenomics.usage_ledger AND tokenomics.token_budgets (404 when
// either is off). Authorized for the user themselves (session JWT or scoped UAT;
// project-boundary UATs restrict user_project rows to their boundary project) or
// a hub admin with hub.metrics.read; unrelated users and agents receive 403.
func (s *Server) handleUserUsageBudgets(w http.ResponseWriter, r *http.Request, targetUserID string) {
	if !s.tokenBudgetsEnabled() {
		NotFound(w, "route")
		return
	}
	if r.Method != http.MethodGet {
		MethodNotAllowed(w, http.MethodGet)
		return
	}

	ctx := r.Context()
	identity := GetIdentityFromContext(ctx)
	if identity == nil {
		Unauthorized(w)
		return
	}
	userIdent, ok := identity.(UserIdentity)
	if !ok {
		Forbidden(w)
		return
	}

	boundaryProjectID := ""
	if userIdent.ID() == targetUserID {
		token, admitted := selfReadCredential(ctx, userIdent, "user.usage.budgets.self.read")
		if !admitted {
			refuseSelfReadCredential(w, r, userIdent, "usage budgets")
			return
		}
		if token != nil {
			boundaryProjectID = tokenBoundaryProject(token)
		}
	} else {
		decision := s.authzService.Decide(ctx, AuthzRequest{
			Principal:  principalContextForIdentity(userIdent),
			Credential: credentialContextForIdentity(userIdent),
			Resource:   Resource{Type: "hub", ID: "hub"},
			Action:     ActionRead,
			Permission: "hub.metrics.read",
		})
		if !decision.Allowed {
			Forbidden(w)
			return
		}
	}

	u, err := s.store.GetUser(ctx, targetUserID)
	if err != nil {
		writeErrorFromErr(w, err, "")
		return
	}
	if u == nil {
		NotFound(w, "User")
		return
	}

	s.serveUserUsageBudgets(w, r, userIdent, u.ID, boundaryProjectID)
}

func (s *Server) serveUserUsageBudgets(w http.ResponseWriter, r *http.Request, caller UserIdentity, targetUserID, boundaryProjectID string) {
	ctx := r.Context()
	asOf, err := s.store.DBNow(ctx)
	if err != nil {
		writeErrorFromErr(w, err, "")
		return
	}
	asOf = asOf.UTC()
	dayStart, dayEnd := utcDayWindow(asOf)
	monthStart, monthEnd := utcMonthWindow(asOf)

	evalCache := newTokenLimitEvalCache(s.quotaService)

	// Load all open state rows for this user (both user and user_project scopes)
	// in a single query, and load all user_project counter sums for the active
	// day and month windows in two grouped prefix queries so store calls stay
	// O(1) regardless of how many projects exist on the Hub (R2).
	openRows, err := s.store.ListOpenUsageBudgetStatesForUser(ctx, targetUserID, asOf)
	if err != nil {
		writeErrorFromErr(w, err, "")
		return
	}
	stateByKey := make(map[store.UsageBudgetStateKey]*store.UsageBudgetState, len(openRows))
	for _, row := range openRows {
		k := store.UsageBudgetStateKey{
			LimitDefinitionID: row.LimitDefinitionID,
			ScopeKind:         row.ScopeKind,
			ScopeID:           row.ScopeID,
			ProjectScopeID:    row.ProjectScopeID,
			WindowStartUTC:    row.WindowStartUTC,
		}
		stateByKey[k] = row
	}

	userPrefix := targetUserID + ":"
	monthUPConsumed, err := s.store.SumUsageScopeCounterTokensByPrefix(ctx, store.UsageScopeKindUserProject, userPrefix, monthStart, monthEnd)
	if err != nil {
		writeErrorFromErr(w, err, "")
		return
	}
	dayUPConsumed, err := s.store.SumUsageScopeCounterTokensByPrefix(ctx, store.UsageScopeKindUserProject, userPrefix, dayStart, dayEnd)
	if err != nil {
		writeErrorFromErr(w, err, "")
		return
	}

	type consumedLookupKey struct {
		scopeKind      string
		counterScopeID string
		windowStartUTC time.Time
	}
	preloadedConsumed := make(map[consumedLookupKey]int64, len(monthUPConsumed)+len(dayUPConsumed))
	for counterID, sum := range monthUPConsumed {
		preloadedConsumed[consumedLookupKey{
			scopeKind:      store.UsageScopeKindUserProject,
			counterScopeID: counterID,
			windowStartUTC: monthStart,
		}] = sum
	}
	for counterID, sum := range dayUPConsumed {
		preloadedConsumed[consumedLookupKey{
			scopeKind:      store.UsageScopeKindUserProject,
			counterScopeID: counterID,
			windowStartUTC: dayStart,
		}] = sum
	}

	entries := make([]UsageBudgetEntry, 0, 4)

	// Project-boundary UATs are clamped strictly to their boundary project
	// (matching P2 D7): omit hub-wide user-scope aggregate rows and return only
	// that boundary project's user_project rows.
	if boundaryProjectID == "" {
		userSpecs := []budgetEvalSpec{
			{
				limitName:      store.LimitLLMTokensPerUserDay,
				scopeKind:      store.UsageScopeKindUser,
				scopeID:        targetUserID,
				counterScopeID: targetUserID,
				bindingSubject: targetUserID,
				bindingScope:   store.QuotaScopeSystem,
				bindingScopeID: "",
				windowStartUTC: dayStart,
				windowEndUTC:   dayEnd,
			},
			{
				limitName:      store.LimitLLMTokensPerUserMonth,
				scopeKind:      store.UsageScopeKindUser,
				scopeID:        targetUserID,
				counterScopeID: targetUserID,
				bindingSubject: targetUserID,
				bindingScope:   store.QuotaScopeSystem,
				bindingScopeID: "",
				windowStartUTC: monthStart,
				windowEndUTC:   monthEnd,
			},
		}
		userEntries, err := s.buildBudgetEntries(ctx, evalCache, stateByKey, nil, userSpecs, asOf, true)
		if err != nil {
			writeErrorFromErr(w, err, "")
			return
		}
		entries = append(entries, userEntries...)
	}

	visibleProjects, err := s.listCandidateUserProjectsForCaller(ctx, caller, targetUserID, boundaryProjectID, evalCache, openRows, monthUPConsumed)
	if err != nil {
		writeErrorFromErr(w, err, "")
		return
	}

	if len(visibleProjects) > 0 {
		upSpecs := make([]budgetEvalSpec, 0, len(visibleProjects)*2)
		for _, pid := range visibleProjects {
			upCounterID := userPrefix + pid
			upSpecs = append(upSpecs,
				budgetEvalSpec{
					limitName:      store.LimitLLMTokensPerUserProjectDay,
					scopeKind:      store.UsageScopeKindUserProject,
					scopeID:        targetUserID,
					projectScopeID: pid,
					counterScopeID: upCounterID,
					bindingSubject: targetUserID,
					bindingScope:   store.QuotaScopeProject,
					bindingScopeID: pid,
					windowStartUTC: dayStart,
					windowEndUTC:   dayEnd,
				},
				budgetEvalSpec{
					limitName:      store.LimitLLMTokensPerUserProjectMonth,
					scopeKind:      store.UsageScopeKindUserProject,
					scopeID:        targetUserID,
					projectScopeID: pid,
					counterScopeID: upCounterID,
					bindingSubject: targetUserID,
					bindingScope:   store.QuotaScopeProject,
					bindingScopeID: pid,
					windowStartUTC: monthStart,
					windowEndUTC:   monthEnd,
				},
			)
		}
		// Only include user_project rows where a finite cap applies OR an active
		// state row already exists (avoiding empty unlimited rows).
		preloadedFn := func(spec budgetEvalSpec) (int64, bool) {
			return preloadedConsumed[consumedLookupKey{
				scopeKind:      spec.scopeKind,
				counterScopeID: spec.counterScopeID,
				windowStartUTC: spec.windowStartUTC,
			}], true
		}
		upEntries, err := s.buildBudgetEntries(ctx, evalCache, stateByKey, preloadedFn, upSpecs, asOf, false)
		if err != nil {
			writeErrorFromErr(w, err, "")
			return
		}
		entries = append(entries, upEntries...)
	}

	writeJSON(w, http.StatusOK, UsageBudgetsResponse{
		Scope:   UsageScopeRef{Kind: store.UsageScopeKindUser, ID: targetUserID},
		AsOf:    asOf,
		Budgets: entries,
	})
}

// listCandidateUserProjectsForCaller returns the sorted project IDs for which
// targetUserID has an active user_project budget row, an explicit
// project-scoped user_project binding, or (under a hub-wide user_project
// default > 0) actual counter usage in the active month window — intersected
// with projects the caller is authorized to see.
//
// Never enumerates all projects on the Hub via ListProjects, keeping query
// count O(1) even on Hubs with thousands of projects (R2).
func (s *Server) listCandidateUserProjectsForCaller(
	ctx context.Context,
	caller UserIdentity,
	targetUserID string,
	boundaryProjectID string,
	evalCache *tokenLimitEvalCache,
	openRows []*store.UsageBudgetState,
	monthUPConsumed map[string]int64,
) ([]string, error) {
	if boundaryProjectID != "" {
		// Verify the caller can actually read the boundary project.
		proj, err := s.store.GetProject(ctx, boundaryProjectID)
		if err != nil {
			if errors.Is(err, store.ErrNotFound) {
				return nil, nil
			}
			return nil, err
		}
		if proj == nil {
			return nil, nil
		}
		decision := s.authzService.CheckAccess(ctx, caller, Resource{
			Type:    "project",
			ID:      proj.ID,
			OwnerID: proj.OwnerID,
		}, ActionRead)
		if !decision.Allowed {
			return nil, nil
		}
		return []string{proj.ID}, nil
	}

	scopeResult, err := s.authzService.ResolveListScopes(ctx, caller, "project.list")
	if err != nil {
		return nil, err
	}
	if scopeResult.Scopes.IsNone() {
		return nil, nil
	}

	candidates := make(map[string]struct{})
	for _, row := range openRows {
		if row.ScopeKind == store.UsageScopeKindUserProject && row.ProjectScopeID != "" {
			candidates[row.ProjectScopeID] = struct{}{}
		}
	}

	// Collect project-scoped bindings for llm_tokens_per_user_project_{day,month}
	// and detect whether a hub-wide default (>0) applies across projects.
	upDefIDs := make(map[string]bool, 2)
	hasHubWideUPDefault := false
	for _, limitName := range []string{store.LimitLLMTokensPerUserProjectDay, store.LimitLLMTokensPerUserProjectMonth} {
		def, defErr := evalCache.lookupLimitDef(ctx, limitName)
		if defErr != nil {
			return nil, defErr
		}
		if def == nil {
			continue
		}
		upDefIDs[def.ID] = true
		if def.DefaultValue > 0 {
			hasHubWideUPDefault = true
		}
	}

	if len(upDefIDs) > 0 {
		subjBindings, bErr := evalCache.loadSubjectBindings(ctx, targetUserID)
		if bErr != nil {
			return nil, bErr
		}
		for _, b := range subjBindings {
			if !upDefIDs[b.LimitDefinitionID] || b.Value <= 0 {
				continue
			}
			if b.ScopeType == store.QuotaScopeProject && b.ScopeID != "" {
				candidates[b.ScopeID] = struct{}{}
			} else if b.ScopeType == store.QuotaScopeSystem {
				hasHubWideUPDefault = true
			}
		}
		sysBindings, err := evalCache.loadSystemBindings(ctx)
		if err != nil {
			return nil, err
		}
		for _, b := range sysBindings {
			if !upDefIDs[b.LimitDefinitionID] || b.Value <= 0 {
				continue
			}
			if b.ScopeType == store.QuotaScopeProject && b.ScopeID != "" {
				candidates[b.ScopeID] = struct{}{}
			} else if b.ScopeType == store.QuotaScopeSystem {
				hasHubWideUPDefault = true
			}
		}
	}

	// Under a hub-wide user_project default (>0), include projects where
	// targetUserID actually has counter usage in the active month window
	// (discovered in one grouped prefix query, never scanning all hub projects).
	if hasHubWideUPDefault {
		prefix := targetUserID + ":"
		for counterID := range monthUPConsumed {
			if pid, ok := strings.CutPrefix(counterID, prefix); ok && pid != "" {
				candidates[pid] = struct{}{}
			}
		}
	}

	if len(candidates) == 0 {
		return nil, nil
	}

	excluded := make(map[string]struct{}, len(scopeResult.ExcludedProjectIDs))
	for _, id := range scopeResult.ExcludedProjectIDs {
		excluded[id] = struct{}{}
	}

	var allowed map[string]struct{}
	if !scopeResult.Scopes.IsAll() {
		allowedIDs := scopeResult.Scopes.ProjectIDs()
		allowed = make(map[string]struct{}, len(allowedIDs))
		for _, id := range allowedIDs {
			allowed[id] = struct{}{}
		}
	}

	ids := make([]string, 0, len(candidates))
	for pid := range candidates {
		if _, isExcl := excluded[pid]; isExcl {
			continue
		}
		if allowed != nil {
			if _, isAllowed := allowed[pid]; !isAllowed {
				continue
			}
		}
		ids = append(ids, pid)
	}
	sort.Strings(ids)
	return ids, nil
}

func (s *Server) buildBudgetEntries(
	ctx context.Context,
	evalCache *tokenLimitEvalCache,
	preloadedStates map[store.UsageBudgetStateKey]*store.UsageBudgetState,
	preloadedConsumed func(spec budgetEvalSpec) (int64, bool),
	specs []budgetEvalSpec,
	asOf time.Time,
	includeUnlimitedWithoutRow bool,
) ([]UsageBudgetEntry, error) {
	if evalCache == nil {
		evalCache = newTokenLimitEvalCache(s.quotaService)
	}
	entries := make([]UsageBudgetEntry, 0, len(specs))

	for _, spec := range specs {
		def, err := evalCache.lookupLimitDef(ctx, spec.limitName)
		if err != nil {
			return nil, err
		}
		if def == nil {
			continue
		}

		capVal, action, err := evalCache.resolveForDef(
			ctx, def, spec.bindingSubject, spec.bindingScope, spec.bindingScopeID,
		)
		if err != nil {
			return nil, err
		}

		key := store.UsageBudgetStateKey{
			LimitDefinitionID: def.ID,
			ScopeKind:         spec.scopeKind,
			ScopeID:           spec.scopeID,
			ProjectScopeID:    spec.projectScopeID,
			WindowStartUTC:    spec.windowStartUTC,
		}
		var row *store.UsageBudgetState
		if preloadedStates != nil {
			row = preloadedStates[key]
		} else {
			row, err = s.store.GetUsageBudgetState(ctx, key)
			if err != nil && !errors.Is(err, store.ErrNotFound) {
				return nil, err
			}
		}

		if capVal <= 0 && row == nil && !includeUnlimitedWithoutRow {
			continue
		}

		var consumed int64
		if preloadedConsumed != nil {
			if c, ok := preloadedConsumed(spec); ok {
				consumed = c
			} else {
				consumed, err = s.store.SumUsageScopeCounterTokens(
					ctx, spec.scopeKind, spec.counterScopeID, spec.windowStartUTC, spec.windowEndUTC,
				)
				if err != nil {
					return nil, err
				}
			}
		} else {
			consumed, err = s.store.SumUsageScopeCounterTokens(
				ctx, spec.scopeKind, spec.counterScopeID, spec.windowStartUTC, spec.windowEndUTC,
			)
			if err != nil {
				return nil, err
			}
		}

		var capPtr *int64
		if capVal > 0 {
			c := capVal
			capPtr = &c
		}

		entry := UsageBudgetEntry{
			LimitDefinitionID: def.ID,
			LimitName:         def.Name,
			ScopeKind:         spec.scopeKind,
			ScopeID:           spec.scopeID,
			ProjectScopeID:    spec.projectScopeID,
			WindowStartUTC:    spec.windowStartUTC,
			WindowEndUTC:      spec.windowEndUTC,
			Cap:               capPtr,
			TokenAction:       action,
			Consumed:          consumed,
			AsOf:              asOf,
		}

		if row != nil {
			entry.ID = row.ID
			entry.State = row.State
			entry.ThresholdsNotified = row.ThresholdsNotified
			entry.OverrideUntil = row.OverrideUntil
			entry.StateVersion = row.StateVersion
			// If an override has already expired (override_until <= as_of),
			// surface max(stored, computed) so UI/CLI do not display a stale ok
			// before the next ingest or admission evaluation persists the rise.
			if row.OverrideUntil != nil && !row.OverrideUntil.After(asOf) && capVal > 0 {
				entry.State = maxBudgetState(row.State, computeBudgetState(consumed, capVal))
			}
		} else {
			if capVal <= 0 {
				entry.State = store.UsageBudgetStateOK
			} else {
				entry.State = computeBudgetState(consumed, capVal)
			}
		}

		entries = append(entries, entry)
	}
	return entries, nil
}

// handleProjectUsageBudgetOverride serves:
// POST /api/v1/projects/{id}/usage/budgets/{budgetId}:override.
// Authorized for project admins (project.manage on project {id}, held by
// project_owner, project_admin, and hub admins; denied to project_member and
// agents).
func (s *Server) handleProjectUsageBudgetOverride(w http.ResponseWriter, r *http.Request, projectID, budgetID string) {
	if !s.tokenBudgetsEnabled() {
		NotFound(w, "route")
		return
	}
	if r.Method != http.MethodPost {
		MethodNotAllowed(w, http.MethodPost)
		return
	}

	ctx := r.Context()
	identity := GetIdentityFromContext(ctx)
	if identity == nil {
		Unauthorized(w)
		return
	}
	userIdent, ok := identity.(UserIdentity)
	if !ok {
		Forbidden(w)
		return
	}

	project, err := s.store.GetProject(ctx, projectID)
	if err != nil {
		writeErrorFromErr(w, err, "")
		return
	}
	if project == nil {
		NotFound(w, "Project")
		return
	}

	decision := s.authzService.Decide(ctx, AuthzRequest{
		Principal:  principalContextForIdentity(userIdent),
		Credential: credentialContextForIdentity(userIdent),
		Resource:   projectResource(project),
		Action:     ActionManage,
		Permission: "project.manage",
	})
	if !decision.Allowed {
		Forbidden(w)
		return
	}

	row, err := s.store.GetUsageBudgetStateByID(ctx, budgetID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			NotFound(w, "Usage Budget")
			return
		}
		writeErrorFromErr(w, err, "")
		return
	}

	// Verify the budget row belongs to this project.
	belongs := (row.ScopeKind == store.UsageScopeKindProject && row.ScopeID == project.ID) ||
		(row.ScopeKind == store.UsageScopeKindUserProject && row.ProjectScopeID == project.ID)
	if !belongs {
		NotFound(w, "Usage Budget")
		return
	}

	s.executeUsageBudgetOverride(w, r, row)
}

// handleUserUsageBudgetOverride serves:
// POST /api/v1/users/{id}/usage/budgets/{budgetId}:override.
// Authorized for hub admins with quota.update on hub (regular users including
// self and agents receive 403).
func (s *Server) handleUserUsageBudgetOverride(w http.ResponseWriter, r *http.Request, targetUserID, budgetID string) {
	if !s.tokenBudgetsEnabled() {
		NotFound(w, "route")
		return
	}
	if r.Method != http.MethodPost {
		MethodNotAllowed(w, http.MethodPost)
		return
	}

	ctx := r.Context()
	identity := GetIdentityFromContext(ctx)
	if identity == nil {
		Unauthorized(w)
		return
	}
	userIdent, ok := identity.(UserIdentity)
	if !ok {
		Forbidden(w)
		return
	}

	decision := s.authzService.Decide(ctx, AuthzRequest{
		Principal:  principalContextForIdentity(userIdent),
		Credential: credentialContextForIdentity(userIdent),
		Resource:   hubScopedResource("quota", "hub"),
		Action:     ActionUpdate,
		Permission: "quota.update",
	})
	if !decision.Allowed {
		Forbidden(w)
		return
	}

	u, err := s.store.GetUser(ctx, targetUserID)
	if err != nil {
		writeErrorFromErr(w, err, "")
		return
	}
	if u == nil {
		NotFound(w, "User")
		return
	}

	row, err := s.store.GetUsageBudgetStateByID(ctx, budgetID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			NotFound(w, "Usage Budget")
			return
		}
		writeErrorFromErr(w, err, "")
		return
	}

	// Verify the budget row belongs to this user.
	belongs := (row.ScopeKind == store.UsageScopeKindUser && row.ScopeID == u.ID) ||
		(row.ScopeKind == store.UsageScopeKindUserProject && row.ScopeID == u.ID)
	if !belongs {
		NotFound(w, "Usage Budget")
		return
	}

	s.executeUsageBudgetOverride(w, r, row)
}

func (s *Server) executeUsageBudgetOverride(w http.ResponseWriter, r *http.Request, row *store.UsageBudgetState) {
	var req overrideBudgetRequest
	if err := readJSON(r, &req); err != nil {
		BadRequest(w, "invalid request body: "+err.Error())
		return
	}

	rawOverride := bytes.TrimSpace(req.OverrideUntil)
	if len(rawOverride) == 0 {
		BadRequest(w, "override_until is required (pass null to clear an active override)")
		return
	}
	var overrideUntil *time.Time
	if !bytes.Equal(rawOverride, []byte("null")) {
		var parsed time.Time
		if err := json.Unmarshal(rawOverride, &parsed); err != nil {
			BadRequest(w, "invalid override_until timestamp: "+err.Error())
			return
		}
		overrideUntil = &parsed
	}

	// Resolve expected state_version from If-Match header and/or body.
	var hasIfMatch bool
	var ifMatchVer int64
	if rawIfMatch := strings.TrimSpace(r.Header.Get("If-Match")); rawIfMatch != "" {
		v, err := parseIfMatchRevision(rawIfMatch)
		if err != nil {
			BadRequest(w, "invalid If-Match header: "+err.Error())
			return
		}
		hasIfMatch = true
		ifMatchVer = v
	}

	var expectedVersion int64
	switch {
	case hasIfMatch && req.StateVersion != nil:
		if *req.StateVersion != ifMatchVer {
			BadRequest(w, "If-Match header and state_version body field disagree")
			return
		}
		expectedVersion = ifMatchVer
	case hasIfMatch:
		expectedVersion = ifMatchVer
	case req.StateVersion != nil:
		if *req.StateVersion <= 0 {
			BadRequest(w, "state_version must be a positive integer")
			return
		}
		expectedVersion = *req.StateVersion
	default:
		writeError(w, http.StatusPreconditionRequired, ErrCodeRevisionConflict,
			"If-Match header or state_version is required for budget overrides", nil)
		return
	}

	ctx := r.Context()
	eval := s.TokenBudgetEvaluator()
	res, err := eval.OverrideBudgetState(ctx, row, overrideUntil, expectedVersion)
	if err != nil {
		switch {
		case errors.Is(err, store.ErrBudgetStateConflict):
			writeError(w, http.StatusPreconditionFailed, ErrCodeRevisionConflict,
				"budget state_version mismatch; refresh and retry", nil)
		case errors.Is(err, ErrBudgetWindowClosed), errors.Is(err, ErrBudgetOverrideInvalidRange):
			ValidationError(w, err.Error(), nil)
		case errors.Is(err, store.ErrNotFound):
			NotFound(w, "Usage Budget")
		default:
			writeErrorFromErr(w, err, "")
		}
		return
	}

	s.writeUsageBudgetOverrideAudit(ctx, res, strings.TrimSpace(req.Reason))

	asOf, err := s.store.DBNow(ctx)
	if err != nil {
		asOf = time.Now().UTC()
	} else {
		asOf = asOf.UTC()
	}

	var capPtr *int64
	if res.Cap > 0 {
		c := res.Cap
		capPtr = &c
	}

	writeJSON(w, http.StatusOK, UsageBudgetEntry{
		ID:                 res.After.ID,
		LimitDefinitionID:  res.After.LimitDefinitionID,
		LimitName:          res.LimitName,
		ScopeKind:          res.After.ScopeKind,
		ScopeID:            res.After.ScopeID,
		ProjectScopeID:     res.After.ProjectScopeID,
		WindowStartUTC:     res.After.WindowStartUTC,
		WindowEndUTC:       res.After.WindowEndUTC,
		Cap:                capPtr,
		TokenAction:        res.Action,
		Consumed:           res.Consumed,
		State:              res.After.State,
		ThresholdsNotified: res.After.ThresholdsNotified,
		OverrideUntil:      res.After.OverrideUntil,
		StateVersion:       res.After.StateVersion,
		AsOf:               asOf,
	})
}

func (s *Server) writeUsageBudgetOverrideAudit(ctx context.Context, res *OverrideBudgetResult, reason string) {
	if s.store == nil || res == nil || res.Before == nil || res.After == nil {
		return
	}
	beforePayload := map[string]any{
		"limit_name":     res.LimitName,
		"scope_kind":     res.Before.ScopeKind,
		"scope_id":       res.Before.ScopeID,
		"state":          res.Before.State,
		"override_until": res.Before.OverrideUntil,
		"state_version":  res.Before.StateVersion,
	}
	afterPayload := map[string]any{
		"limit_name":     res.LimitName,
		"scope_kind":     res.After.ScopeKind,
		"scope_id":       res.After.ScopeID,
		"state":          res.After.State,
		"override_until": res.After.OverrideUntil,
		"state_version":  res.After.StateVersion,
	}
	if reason != "" {
		afterPayload["reason"] = reason
	}
	beforeJSON, _ := json.Marshal(beforePayload)
	afterJSON, _ := json.Marshal(afterPayload)

	rec := &store.MutationAuditRecord{
		Timestamp:     time.Now().UTC(),
		MutationType:  "usage_budget_override",
		TargetType:    "usage_budget_state",
		TargetID:      res.After.ID,
		BeforeSummary: string(beforeJSON),
		AfterSummary:  string(afterJSON),
	}
	auditActorFromContext(ctx).ApplyActor(rec)
	if err := s.store.CreateMutationAudit(ctx, rec); err != nil {
		slog.Warn("failed to write mutation audit for usage budget override",
			"budget_id", res.After.ID, "error", err)
	}
}
