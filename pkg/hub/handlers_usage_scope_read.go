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
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/experiments"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

const (
	defaultUsageGroupsLimit = 100
	maxUsageGroupsLimit     = 500
)

// UsageScopeRef identifies the scope evaluated by a scoped usage summary API.
type UsageScopeRef struct {
	Kind string `json:"kind"`
	ID   string `json:"id,omitempty"`
}

// UsageWindow reports the effective UTC window and rollup granularity ("hour"
// or "day") evaluated for a usage query.
type UsageWindow struct {
	Since       time.Time `json:"since"`
	Until       time.Time `json:"until"`
	Granularity string    `json:"granularity"`
}

// UsageGroupRow is one grouped aggregation row in UsageSummaryResponse.
// Grouped dimension fields are pointers so a grouped dimension with an empty
// value (e.g., unknown harness/model or NULL billing_user_id) is always
// emitted as "" while ungrouped dimensions are omitted.
type UsageGroupRow struct {
	BucketStart     *time.Time       `json:"bucket_start,omitempty"`
	ProjectID       *string          `json:"project_id,omitempty"`
	UserID          *string          `json:"user_id,omitempty"`
	AgentID         *string          `json:"agent_id,omitempty"`
	Harness         *string          `json:"harness,omitempty"`
	Model           *string          `json:"model,omitempty"`
	Calls           int64            `json:"calls"`
	ErrorCalls      int64            `json:"error_calls"`
	Events          int64            `json:"events"`
	NullTokenEvents int64            `json:"null_token_events"`
	Tokens          AgentUsageTokens `json:"tokens"`
}

// UsageSummaryResponse is returned by:
//   - GET /api/v1/projects/{id}/usage
//   - GET /api/v1/users/{id}/usage
//   - GET /api/v1/me/usage
//   - GET /api/v1/usage
type UsageSummaryResponse struct {
	Scope             UsageScopeRef    `json:"scope"`
	Window            UsageWindow      `json:"window"`
	GroupBy           []string         `json:"group_by,omitempty"`
	Calls             int64            `json:"calls"`
	ErrorCalls        int64            `json:"error_calls"`
	Events            int64            `json:"events"`
	NullTokenEvents   int64            `json:"null_token_events"`
	Tokens            AgentUsageTokens `json:"tokens"`
	Groups            []UsageGroupRow  `json:"groups,omitempty"`
	Truncated         bool             `json:"truncated"`
	MeasurementStatus string           `json:"measurement_status"`
	UnmeasuredAgents  int              `json:"unmeasured_agents"`
	StaleAgents       int              `json:"stale_agents"`
	AsOf              time.Time        `json:"as_of"`
	LastReceiptAt     *time.Time       `json:"last_receipt_at"`
}

// handleProjectUsage serves GET /api/v1/projects/{id}/usage.
// Gated behind tokenomics.usage_ledger (404 when off).
// Authorized for project members with project.read (session JWT or scoped UAT)
// and agents belonging to the same project.
func (s *Server) handleProjectUsage(w http.ResponseWriter, r *http.Request, projectID string) {
	if !s.experimentEnabled(experiments.TokenomicsUsageLedger) {
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

	s.serveScopedUsage(w, r, UsageScopeRef{Kind: store.UsageScopeKindProject, ID: project.ID}, project.ID, "", "")
}

// handleMeUsage serves GET /api/v1/me/usage (caller's own Track 1 token usage,
// distinct from GET /api/v1/usage/me which serves quota usage).
// Gated behind tokenomics.usage_ledger (404 when off).
// Authorized for any authenticated UserIdentity (session JWT or scoped UAT);
// agents receive 403. A project-boundary UAT is restricted to its boundary
// project.
func (s *Server) handleMeUsage(w http.ResponseWriter, r *http.Request) {
	if !s.experimentEnabled(experiments.TokenomicsUsageLedger) {
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
	token, admitted := selfReadCredential(ctx, userIdent, "hub.usage.self.read")
	if !admitted {
		refuseSelfReadCredential(w, r, userIdent, "usage")
		return
	}
	boundaryProjectID := ""
	if token != nil {
		boundaryProjectID = tokenBoundaryProject(token)
	}

	s.serveScopedUsage(w, r, UsageScopeRef{Kind: store.UsageScopeKindUser, ID: userIdent.ID()}, "", userIdent.ID(), boundaryProjectID)
}

// handleUserUsage serves GET /api/v1/users/{id}/usage (billing_user_id = id).
// Gated behind tokenomics.usage_ledger (404 when off).
// Authorized for the user themselves (session JWT or scoped UAT; project-
// boundary UATs are restricted to their boundary project) or a hub admin with
// hub.metrics.read; unrelated users and agents receive 403.
func (s *Server) handleUserUsage(w http.ResponseWriter, r *http.Request, targetUserID string) {
	if !s.experimentEnabled(experiments.TokenomicsUsageLedger) {
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
		token, admitted := selfReadCredential(ctx, userIdent, "user.usage.self.read")
		if !admitted {
			refuseSelfReadCredential(w, r, userIdent, "usage")
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

	s.serveScopedUsage(w, r, UsageScopeRef{Kind: store.UsageScopeKindUser, ID: u.ID}, "", u.ID, boundaryProjectID)
}

// handleGlobalUsage serves GET /api/v1/usage (hub-wide Track 1 token usage).
// Registered as an exact path so it never shadows GET /api/v1/usage/me (quota
// usage). Gated behind tokenomics.usage_ledger (404 when off) and RouteHubAdmin
// (hub.metrics.read on hub).
func (s *Server) handleGlobalUsage(w http.ResponseWriter, r *http.Request) {
	if !s.experimentEnabled(experiments.TokenomicsUsageLedger) {
		NotFound(w, "route")
		return
	}
	if r.Method != http.MethodGet {
		MethodNotAllowed(w, http.MethodGet)
		return
	}

	if GetUserIdentityFromContext(r.Context()) == nil {
		Unauthorized(w)
		return
	}

	s.serveScopedUsage(w, r, UsageScopeRef{Kind: store.UsageScopeKindGlobal}, "", "", "")
}

func (s *Server) serveScopedUsage(w http.ResponseWriter, r *http.Request, scope UsageScopeRef, fixedProjectID, fixedUserID, boundaryProjectID string) {
	ctx := r.Context()
	qParams := r.URL.Query()

	groupBy, hasHourGroup, err := parseUsageGroupBy(qParams["groupBy"])
	if err != nil {
		ValidationError(w, err.Error(), nil)
		return
	}

	hourlyRetentionDays := s.usageRetentionConfig().HourlyRollupsDays

	limit, err := parseUsageLimit(qParams.Get("limit"), hasHourGroup, hourlyRetentionDays)
	if err != nil {
		ValidationError(w, err.Error(), nil)
		return
	}

	// Optional filters; reject conflicting scope filters and enforce any UAT
	// project boundary.
	filterProjectID := strings.TrimSpace(qParams.Get("project_id"))
	if fixedProjectID != "" {
		if filterProjectID != "" && filterProjectID != fixedProjectID {
			ValidationError(w, "project_id filter conflicts with route project scope", nil)
			return
		}
		filterProjectID = fixedProjectID
	}
	if boundaryProjectID != "" {
		if filterProjectID != "" && filterProjectID != boundaryProjectID {
			Forbidden(w)
			return
		}
		filterProjectID = boundaryProjectID
	}

	filterUserID := strings.TrimSpace(qParams.Get("user_id"))
	if fixedUserID != "" {
		if filterUserID != "" && filterUserID != fixedUserID {
			ValidationError(w, "user_id filter conflicts with route user scope", nil)
			return
		}
		filterUserID = fixedUserID
	}

	filterAgentID := strings.TrimSpace(qParams.Get("agent_id"))
	filterHarness := strings.TrimSpace(qParams.Get("harness"))
	filterModel := strings.TrimSpace(qParams.Get("model"))
	filterUsageSource := strings.TrimSpace(qParams.Get("usage_source"))

	dbNow, err := s.store.DBNow(ctx)
	if err != nil {
		writeErrorFromErr(w, err, "")
		return
	}

	hourlyCoverageStart, hasHourlyCoverage, err := s.store.GetUsageHourlyCoverageStart(ctx)
	if err != nil {
		writeErrorFromErr(w, err, "")
		return
	}

	retentionState, err := s.store.GetUsageRetentionState(ctx)
	if err != nil {
		writeErrorFromErr(w, err, "")
		return
	}
	var hourlySweptThrough time.Time
	if retentionState != nil && retentionState.HourlySweptThrough != nil {
		hourlySweptThrough = *retentionState.HourlySweptThrough
	}

	win, err := resolveUsageWindowAndGranularity(
		qParams.Get("since"),
		qParams.Get("until"),
		dbNow,
		hourlyCoverageStart,
		hasHourlyCoverage,
		hasHourGroup,
		hourlyRetentionDays,
		hourlySweptThrough,
	)
	if err != nil {
		ValidationError(w, err.Error(), nil)
		return
	}

	win, validationMsg, err := resolveConsistentUsageWindow(
		ctx,
		s.store,
		win,
		store.UsageRollupQuery{
			ProjectID:     filterProjectID,
			BillingUserID: filterUserID,
			AgentID:       filterAgentID,
			Harness:       filterHarness,
			Model:         filterModel,
			UsageSource:   filterUsageSource,
		},
		hasHourGroup,
	)
	if err != nil {
		writeErrorFromErr(w, err, "")
		return
	}
	if validationMsg != "" {
		ValidationError(w, validationMsg, nil)
		return
	}

	rollupRes, err := s.store.QueryUsageRollups(ctx, store.UsageRollupQuery{
		Granularity:   win.Granularity,
		Since:         win.Since,
		Until:         win.Until,
		ProjectID:     filterProjectID,
		BillingUserID: filterUserID,
		AgentID:       filterAgentID,
		Harness:       filterHarness,
		Model:         filterModel,
		UsageSource:   filterUsageSource,
		GroupBy:       groupBy,
		Limit:         limit,
	})
	if err != nil {
		writeErrorFromErr(w, err, "")
		return
	}

	// Coverage is scoped to project, billing user, agent_id, and [since, until].
	// Note: harness and model filters narrow token totals/groups but do not
	// narrow agent-level coverage counts (O1d).
	cov, err := s.store.GetUsageScopeCoverage(ctx, store.UsageCoverageFilter{
		ProjectID:     filterProjectID,
		BillingUserID: filterUserID,
		AgentID:       filterAgentID,
		Since:         win.Since,
		Until:         win.Until,
		StaleAfter:    staleHeartbeatMultiplier * defaultHeartbeatInterval,
		StartupGrace:  defaultUsageStartupGrace,
		DBNow:         dbNow,
	})
	if err != nil {
		writeErrorFromErr(w, err, "")
		return
	}

	capMemo := newUsageCapabilityMemo(ctx, s)
	resp := buildScopedUsageResponse(scope, win, groupBy, dbNow, rollupRes, cov, capMemo.resolve)
	writeJSON(w, http.StatusOK, resp)
}

func parseUsageGroupBy(rawValues []string) ([]string, bool, error) {
	if len(rawValues) == 0 {
		return nil, false, nil
	}
	allowed := map[string]bool{
		store.UsageGroupByModel:   true,
		store.UsageGroupByHarness: true,
		store.UsageGroupByAgent:   true,
		store.UsageGroupByUser:    true,
		store.UsageGroupByProject: true,
		store.UsageGroupByDay:     true,
		store.UsageGroupByHour:    true,
	}
	seen := make(map[string]bool)
	out := make([]string, 0, len(rawValues))
	hasDay, hasHour := false, false
	for _, raw := range rawValues {
		for _, part := range strings.Split(raw, ",") {
			d := strings.TrimSpace(part)
			if d == "" {
				continue
			}
			if !allowed[d] {
				return nil, false, fmt.Errorf("invalid groupBy dimension %q (allowed: model, harness, agent, user, project, day, hour)", d)
			}
			if seen[d] {
				continue
			}
			seen[d] = true
			if d == store.UsageGroupByDay {
				hasDay = true
			}
			if d == store.UsageGroupByHour {
				hasHour = true
			}
			out = append(out, d)
		}
	}
	if hasDay && hasHour {
		return nil, false, fmt.Errorf("groupBy cannot specify both day and hour")
	}
	return out, hasHour, nil
}

func defaultHourlyLimitForRetention(hourlyRetentionDays int) int {
	if hourlyRetentionDays <= 0 {
		return maxUsageGroupsLimit
	}
	n := hourlyRetentionDays*24 + 1
	if n > maxUsageGroupsLimit {
		return maxUsageGroupsLimit
	}
	if n < defaultUsageGroupsLimit {
		return defaultUsageGroupsLimit
	}
	return n
}

func parseUsageLimit(raw string, hasHourGroup bool, hourlyRetentionDays int) (int, error) {
	s := strings.TrimSpace(raw)
	if s == "" {
		if hasHourGroup {
			return defaultHourlyLimitForRetention(hourlyRetentionDays), nil
		}
		return defaultUsageGroupsLimit, nil
	}
	n, err := strconv.Atoi(s)
	if err != nil || n <= 0 || n > maxUsageGroupsLimit {
		return 0, fmt.Errorf("limit must be an integer between 1 and %d, got %q", maxUsageGroupsLimit, raw)
	}
	return n, nil
}

// resolveUsageWindowAndGranularity parses since/until in UTC and chooses
// "hour" granularity only when the window starts at or after the effective
// hourly horizon: max(hourlyCoverageStart, hourlySweptThrough,
// now - hourlyRetentionDays) (preventing silent under-reporting of
// pre-migration or previously swept periods even after hourly_rollups_days is
// raised); otherwise it selects "day". An explicit groupBy=hour outside that
// horizon returns an error.
func resolveUsageWindowAndGranularity(
	sinceRaw, untilRaw string,
	dbNow time.Time,
	hourlyCoverageStart time.Time,
	hasHourlyCoverage bool,
	requireHourly bool,
	hourlyRetentionDays int,
	hourlySweptThrough time.Time,
) (UsageWindow, error) {
	now := dbNow.UTC().Truncate(time.Microsecond)
	until := now
	if strings.TrimSpace(untilRaw) != "" {
		parsed, err := parseUsageTimeParam(untilRaw, now, true)
		if err != nil {
			return UsageWindow{}, fmt.Errorf("invalid until parameter: %w", err)
		}
		until = parsed
	}

	var since time.Time
	if strings.TrimSpace(sinceRaw) == "" {
		untilDay := time.Date(until.Year(), until.Month(), until.Day(), 0, 0, 0, 0, time.UTC)
		since = untilDay.AddDate(0, 0, -(defaultUsageWindowDays - 1))
	} else {
		parsed, err := parseUsageTimeParam(sinceRaw, until, false)
		if err != nil {
			return UsageWindow{}, fmt.Errorf("invalid since parameter: %w", err)
		}
		since = parsed
	}

	if since.After(until) {
		return UsageWindow{}, fmt.Errorf("since (%s) must not be after until (%s)",
			since.UTC().Format(time.RFC3339), until.UTC().Format(time.RFC3339))
	}

	sinceHour := time.Date(since.Year(), since.Month(), since.Day(), since.Hour(), 0, 0, 0, time.UTC)
	coverageStartHour := time.Date(
		hourlyCoverageStart.Year(), hourlyCoverageStart.Month(), hourlyCoverageStart.Day(), hourlyCoverageStart.Hour(),
		0, 0, 0, time.UTC,
	)

	var retentionCutoff time.Time
	withinRetention := true
	if hourlyRetentionDays > 0 {
		retentionCutoff = hourlyRollupsRetentionCutoff(now, hourlyRetentionDays)
		withinRetention = !sinceHour.Before(retentionCutoff)
	}
	var sweptThroughHour time.Time
	withinSwept := true
	if !hourlySweptThrough.IsZero() {
		sweptThroughHour = time.Date(
			hourlySweptThrough.Year(), hourlySweptThrough.Month(), hourlySweptThrough.Day(), hourlySweptThrough.Hour(),
			0, 0, 0, time.UTC,
		)
		withinSwept = !sinceHour.Before(sweptThroughHour)
	}
	withinCoverage := hasHourlyCoverage && !sinceHour.Before(coverageStartHour)

	if requireHourly {
		if !hasHourlyCoverage {
			return UsageWindow{}, fmt.Errorf("groupBy=hour is unavailable: hourly rollup coverage has not started yet")
		}
		if !withinCoverage {
			return UsageWindow{}, fmt.Errorf("groupBy=hour is unavailable before %s (hourly rollup coverage start)",
				coverageStartHour.UTC().Format(time.RFC3339))
		}
		if !withinSwept {
			return UsageWindow{}, fmt.Errorf("groupBy=hour is unavailable before %s (hourly rollups already swept)",
				sweptThroughHour.UTC().Format(time.RFC3339))
		}
		if !withinRetention {
			return UsageWindow{}, fmt.Errorf("groupBy=hour is only available within the %d-day hourly retention window (since >= %s)",
				hourlyRetentionDays, retentionCutoff.UTC().Format(time.RFC3339))
		}
	}

	if withinRetention && withinCoverage && withinSwept {
		untilHour := time.Date(until.Year(), until.Month(), until.Day(), until.Hour(), 0, 0, 0, time.UTC)
		return UsageWindow{
			Since:       sinceHour,
			Until:       untilHour.Add(time.Hour - time.Microsecond),
			Granularity: store.UsageGranularityHour,
		}, nil
	}

	sinceDay := time.Date(since.Year(), since.Month(), since.Day(), 0, 0, 0, 0, time.UTC)
	untilDay := time.Date(until.Year(), until.Month(), until.Day(), 0, 0, 0, 0, time.UTC)
	return UsageWindow{
		Since:       sinceDay,
		Until:       untilDay.Add(24*time.Hour - time.Microsecond),
		Granularity: store.UsageGranularityDay,
	}, nil
}

// resolveConsistentUsageWindow applies the D7b read-time hourly-vs-daily
// consistency cross-check to an already-resolved UsageWindow. When win is
// hourly, it compares full-day SUM(events) between usage_rollups_hourly and
// usage_rollups_daily across EVERY UTC day the window overlaps (including
// partial edge days and the coverage-marker day) using the same scope/dimension
// filters. On mismatch, it returns a validation message when requireHourly is
// true, or falls back to a UTC day-snapped daily UsageWindow otherwise.
// Shared by scoped /usage handlers and HubRollupSource so all surfaces agree.
func resolveConsistentUsageWindow(
	ctx context.Context,
	st store.UsageLedgerStore,
	win UsageWindow,
	filter store.UsageRollupQuery,
	requireHourly bool,
) (UsageWindow, string, error) {
	if win.Granularity != store.UsageGranularityHour {
		return win, "", nil
	}
	sinceDay := time.Date(win.Since.Year(), win.Since.Month(), win.Since.Day(), 0, 0, 0, 0, time.UTC)
	untilDay := time.Date(win.Until.Year(), win.Until.Month(), win.Until.Day(), 0, 0, 0, 0, time.UTC)
	filter.Since = sinceDay
	filter.Until = untilDay
	consistent, err := st.CheckUsageHourlyConsistency(ctx, filter)
	if err != nil {
		return UsageWindow{}, "", err
	}
	if consistent {
		return win, "", nil
	}
	if requireHourly {
		return UsageWindow{}, "groupBy=hour is unavailable for this window: hourly rollups are incomplete compared to daily rollups", nil
	}
	return UsageWindow{
		Since:       sinceDay,
		Until:       untilDay.Add(24*time.Hour - time.Microsecond),
		Granularity: store.UsageGranularityDay,
	}, "", nil
}

func buildScopedUsageResponse(
	scope UsageScopeRef,
	win UsageWindow,
	groupBy []string,
	dbNow time.Time,
	rollupRes *store.UsageRollupQueryResult,
	cov *store.UsageScopeCoverage,
	resolveCap usageCapabilityLookup,
) UsageSummaryResponse {
	if rollupRes == nil {
		rollupRes = &store.UsageRollupQueryResult{}
	}
	if cov == nil {
		cov = &store.UsageScopeCoverage{}
	}

	t := rollupRes.Totals
	totalTokens := resolveNullableTokens(
		t.Events, t.NullTokenEvents,
		t.Input, t.Output, t.CacheRead, t.CacheWrite, t.CacheWrite1h, t.Reasoning,
		presenceFromTotals(t),
	)
	if t.Saturated {
		totalTokens.Saturated = true
	}

	grouped := make(map[string]bool, len(groupBy))
	for _, d := range groupBy {
		grouped[d] = true
	}

	var groups []UsageGroupRow
	if len(groupBy) > 0 {
		groups = make([]UsageGroupRow, 0, len(rollupRes.Groups))
		for _, g := range rollupRes.Groups {
			if g == nil {
				continue
			}
			gt := resolveNullableTokens(
				g.Events, g.NullTokenEvents,
				g.Input, g.Output, g.CacheRead, g.CacheWrite, g.CacheWrite1h, g.Reasoning,
				presenceFromTotals(g.UsageRollupTotals),
			)
			if g.Saturated {
				gt.Saturated = true
			}
			row := UsageGroupRow{
				BucketStart:     g.BucketStart,
				Calls:           g.Calls,
				ErrorCalls:      g.ErrorCalls,
				Events:          g.Events,
				NullTokenEvents: g.NullTokenEvents,
				Tokens:          gt,
			}
			if grouped[store.UsageGroupByProject] {
				v := g.ProjectID
				row.ProjectID = &v
			}
			if grouped[store.UsageGroupByUser] {
				v := g.BillingUserID
				row.UserID = &v
			}
			if grouped[store.UsageGroupByAgent] {
				v := g.AgentID
				row.AgentID = &v
			}
			if grouped[store.UsageGroupByHarness] {
				v := g.Harness
				row.Harness = &v
			}
			if grouped[store.UsageGroupByModel] {
				v := g.Model
				row.Model = &v
			}
			groups = append(groups, row)
		}
	}

	windowIncludesNow := usageWindowIncludesNow(win.Until, dbNow)
	status, unmeasuredAgents, staleAgents := deriveScopeCoverageSummary(
		cov,
		t.Events,
		rollupRes.HasLegacyRollups,
		rollupRes.HasNonLegacyRollups,
		dbNow,
		windowIncludesNow,
		resolveCap,
	)

	return UsageSummaryResponse{
		Scope:             scope,
		Window:            win,
		GroupBy:           groupBy,
		Calls:             t.Calls,
		ErrorCalls:        t.ErrorCalls,
		Events:            t.Events,
		NullTokenEvents:   t.NullTokenEvents,
		Tokens:            totalTokens,
		Groups:            groups,
		Truncated:         rollupRes.Truncated,
		MeasurementStatus: status,
		UnmeasuredAgents:  unmeasuredAgents,
		StaleAgents:       staleAgents,
		AsOf:              dbNow,
		LastReceiptAt:     cov.LastReceiptAt,
	}
}

// deriveScopeCoverageSummary maps each SQL-grouped coverage bucket through the
// single deriveMeasurementStatus function in Go (O1c) and computes aggregate
// status using precedence stale > partial > measured > calls_only >
// legacy_client > unmeasured (with mixed measured/calls_only/legacy_client
// scopes reading partial so legacy_client never reads as measured), along with
// unmeasured_agents and stale_agents counts. The totalEvents fallback applies
// ONLY when zero agent buckets were classified (e.g., historical rollups from
// deleted agents no longer in scope).
func deriveScopeCoverageSummary(
	cov *store.UsageScopeCoverage,
	totalEvents int64,
	hasLegacyRollups bool,
	hasNonLegacyRollups bool,
	dbNow time.Time,
	windowIncludesNow bool,
	resolveCap usageCapabilityLookup,
) (status string, unmeasuredAgents, staleAgents int) {
	var (
		partialAgents      int
		measuredAgents     int
		callsOnlyAgents    int
		legacyClientAgents int
	)
	staleAfter := staleHeartbeatMultiplier * defaultHeartbeatInterval
	if cov != nil {
		for _, b := range cov.Buckets {
			if b.AgentCount <= 0 {
				continue
			}
			phase := string(state.PhaseStopped)
			if b.IsRunning {
				phase = string(state.PhaseRunning)
			}
			var mhRows []*store.MeterHealth
			if b.HasMeterHealth {
				// Synthetic timestamp encodes only the SQL is_receipt_stale flag so
				// status derivation stays inside the single deriveMeasurementStatus function.
				receiptAt := dbNow
				if b.IsReceiptStale {
					receiptAt = dbNow.Add(-(staleAfter + time.Second))
				}
				mhRows = []*store.MeterHealth{{
					FinalState:          b.LatestFinalState,
					LastReceiptAt:       receiptAt,
					SchemaDriftDetected: b.HasSchemaDrift,
				}}
			}
			var runStartedAt time.Time
			if b.IsPastGrace {
				runStartedAt = dbNow.Add(-(defaultUsageStartupGrace + time.Second))
			}
			var usageCap *api.HarnessUsageCapabilities
			if resolveCap != nil {
				usageCap = resolveCap(b.CapabilityProjectID, b.HarnessConfig)
			}
			s := deriveMeasurementStatus(measurementStatusInput{
				MeterHealth:        mhRows,
				HasRunUsageEvents:  b.HasUsageData,
				HasLegacyUsageOnly: b.HasLegacyOnly,
				RunStartedAt:       runStartedAt,
				DBNow:              dbNow,
				AgentPhase:         phase,
				WindowIncludesNow:  &windowIncludesNow,
				Usage:              usageCap,
			})
			switch s {
			case MeasurementStatusStale:
				staleAgents += b.AgentCount
			case MeasurementStatusPartial:
				partialAgents += b.AgentCount
			case MeasurementStatusMeasured:
				measuredAgents += b.AgentCount
			case MeasurementStatusCallsOnly:
				callsOnlyAgents += b.AgentCount
			case MeasurementStatusLegacyClient:
				legacyClientAgents += b.AgentCount
			default:
				unmeasuredAgents += b.AgentCount
			}
		}
	}

	totalClassified := staleAgents + partialAgents + measuredAgents + callsOnlyAgents + legacyClientAgents + unmeasuredAgents
	switch {
	case staleAgents > 0:
		status = MeasurementStatusStale
	case partialAgents > 0:
		status = MeasurementStatusPartial
	case measuredAgents > 0 && (callsOnlyAgents > 0 || legacyClientAgents > 0):
		status = MeasurementStatusPartial
	case measuredAgents > 0:
		status = MeasurementStatusMeasured
	case callsOnlyAgents > 0 && legacyClientAgents > 0:
		status = MeasurementStatusPartial
	case callsOnlyAgents > 0:
		status = MeasurementStatusCallsOnly
	case legacyClientAgents > 0:
		status = MeasurementStatusLegacyClient
	case totalClassified == 0 && totalEvents > 0:
		switch {
		case hasLegacyRollups && hasNonLegacyRollups:
			status = MeasurementStatusPartial
		case hasLegacyRollups:
			status = MeasurementStatusLegacyClient
		default:
			status = MeasurementStatusMeasured
		}
	default:
		status = MeasurementStatusUnmeasured
	}
	return status, unmeasuredAgents, staleAgents
}
