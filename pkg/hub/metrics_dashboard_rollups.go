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
	"context"
	"fmt"
	"math"
	"sort"
	"strings"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

const dashboardRollupGroupLimit = 5000

// HubRollupSource implements MetricsDashboardSource backed by the Hub's Track 1
// operational usage rollups (usage_rollups_hourly / usage_rollups_daily) and
// Pipeline C session metadata (agent_session_metrics).
type HubRollupSource struct {
	store store.Store
	// hourlyRetentionDays optionally supplies the configured hourly rollup
	// retention horizon (in days; 0 = unlimited). When nil, defaults to
	// config.DefaultResolvedTelemetryRetention().HourlyRollupsDays.
	hourlyRetentionDays func() int
	// newCapabilityLookup optionally constructs a per-request usage.yaml
	// capability lookup memo (wired from *Server so dashboard and scoped usage
	// API statuses agree). When nil, capability is treated as unknown.
	newCapabilityLookup func(ctx context.Context) usageCapabilityLookup
	// groupLimit optionally overrides dashboardRollupGroupLimit in tests to
	// exercise truncation and daily degradation deterministically.
	groupLimit int
}

// NewHubRollupSource constructs a HubRollupSource backed by s.
func NewHubRollupSource(s store.Store) *HubRollupSource {
	return &HubRollupSource{store: s}
}

func (s *HubRollupSource) effectiveHourlyRetentionDays() int {
	if s.hourlyRetentionDays != nil {
		return s.hourlyRetentionDays()
	}
	return config.DefaultResolvedTelemetryRetention().HourlyRollupsDays
}

func (s *HubRollupSource) readHourlyHorizonInputs(ctx context.Context) (hourlyCoverageStart time.Time, hasHourlyCoverage bool, hourlyRetentionDays int, hourlySweptThrough time.Time, err error) {
	hourlyCoverageStart, hasHourlyCoverage, err = s.store.GetUsageHourlyCoverageStart(ctx)
	if err != nil {
		return time.Time{}, false, 0, time.Time{}, fmt.Errorf("read hourly coverage marker: %w", err)
	}
	retentionState, err := s.store.GetUsageRetentionState(ctx)
	if err != nil {
		return time.Time{}, false, 0, time.Time{}, fmt.Errorf("read usage retention state: %w", err)
	}
	if retentionState != nil && retentionState.HourlySweptThrough != nil {
		hourlySweptThrough = *retentionState.HourlySweptThrough
	}
	return hourlyCoverageStart, hasHourlyCoverage, s.effectiveHourlyRetentionDays(), hourlySweptThrough, nil
}

func (s *HubRollupSource) effectiveGroupLimit() int {
	if s.groupLimit > 0 {
		return s.groupLimit
	}
	return dashboardRollupGroupLimit
}

// Name returns the data source identifier for Hub rollups.
func (s *HubRollupSource) Name() string {
	return MetricsSourceHubRollups
}

// resolvedRollupWindow captures the effective query window, granularity, and
// reporting timezone for a HubRollupSource query.
type resolvedRollupWindow struct {
	dbNow       time.Time
	since       time.Time // query window start (UTC)
	until       time.Time // query window end (UTC)
	granularity string    // store.UsageGranularityHour or store.UsageGranularityDay
	reportLoc   *time.Location
	startDayKey string // earliest local day key in reportLoc
}

func dailyFallbackWindow(dbNow time.Time, periodDays int, projectID string) resolvedRollupWindow {
	utcWindow := metricsQueryWindowFor(dbNow, periodDays, &queryConfig{ProjectID: projectID})
	sinceDay := time.Date(utcWindow.start.Year(), utcWindow.start.Month(), utcWindow.start.Day(), 0, 0, 0, 0, time.UTC)
	untilDay := time.Date(utcWindow.end.Year(), utcWindow.end.Month(), utcWindow.end.Day(), 0, 0, 0, 0, time.UTC)
	return resolvedRollupWindow{
		dbNow:       dbNow,
		since:       sinceDay,
		until:       untilDay.Add(24*time.Hour - time.Microsecond),
		granularity: store.UsageGranularityDay,
		reportLoc:   time.UTC,
		startDayKey: dayKey(utcWindow.start, time.UTC),
	}
}

// resolveWindow computes the dashboard window and delegates hourly-vs-daily
// granularity selection to P2.1a's shared resolveUsageWindowAndGranularity and
// resolveConsistentUsageWindow (D7b). When hourly rollups are eligible and
// consistent, UTC hour buckets are re-bucketed into the viewer's requested
// timezone; when falling back to daily rollups, the window and reported
// timeZone are UTC so day keys match the actual bucket boundaries.
func (s *HubRollupSource) resolveWindow(ctx context.Context, periodDays int, cfg *queryConfig) (resolvedRollupWindow, error) {
	dbNow, err := s.store.DBNow(ctx)
	if err != nil {
		return resolvedRollupWindow{}, fmt.Errorf("read DB clock: %w", err)
	}
	hourlyCoverageStart, hasHourlyCoverage, hourlyRetentionDays, hourlySweptThrough, err := s.readHourlyHorizonInputs(ctx)
	if err != nil {
		return resolvedRollupWindow{}, err
	}

	reqWindow := metricsQueryWindowFor(dbNow, periodDays, cfg)
	win, err := resolveUsageWindowAndGranularity(
		reqWindow.start.UTC().Format(time.RFC3339Nano),
		reqWindow.end.UTC().Format(time.RFC3339Nano),
		dbNow,
		hourlyCoverageStart,
		hasHourlyCoverage,
		false,
		hourlyRetentionDays,
		hourlySweptThrough,
	)
	if err != nil {
		return resolvedRollupWindow{}, fmt.Errorf("resolve rollup granularity: %w", err)
	}

	win, _, err = resolveConsistentUsageWindow(ctx, s.store, win, store.UsageRollupQuery{ProjectID: cfg.ProjectID}, false)
	if err != nil {
		return resolvedRollupWindow{}, fmt.Errorf("check hourly rollup consistency: %w", err)
	}

	if win.Granularity == store.UsageGranularityHour {
		loc := cfg.location()
		return resolvedRollupWindow{
			dbNow:       dbNow,
			since:       win.Since,
			until:       win.Until,
			granularity: store.UsageGranularityHour,
			reportLoc:   loc,
			startDayKey: dayKey(reqWindow.start, loc),
		}, nil
	}

	// Daily fallback: usage_rollups_daily is keyed by UTC calendar day, so
	// report timeZone="UTC" and compute the periodDays window in UTC.
	return dailyFallbackWindow(dbNow, periodDays, cfg.ProjectID), nil
}

// resolve24hWindow resolves the rolling 24-hour window for QueryProjectSummary
// through P2.1a's shared resolveUsageWindowAndGranularity and
// resolveConsistentUsageWindow (D7b). When falling back to daily rollups,
// since/until are snapped to full UTC day boundaries so rollups, sessions,
// active agents, and coverage all evaluate the exact same window.
func (s *HubRollupSource) resolve24hWindow(ctx context.Context, projectID string) (resolvedRollupWindow, error) {
	dbNow, err := s.store.DBNow(ctx)
	if err != nil {
		return resolvedRollupWindow{}, fmt.Errorf("read DB clock: %w", err)
	}
	hourlyCoverageStart, hasHourlyCoverage, hourlyRetentionDays, hourlySweptThrough, err := s.readHourlyHorizonInputs(ctx)
	if err != nil {
		return resolvedRollupWindow{}, err
	}

	start := dbNow.UTC().Add(-24 * time.Hour)
	win, err := resolveUsageWindowAndGranularity(
		start.Format(time.RFC3339Nano),
		dbNow.UTC().Format(time.RFC3339Nano),
		dbNow,
		hourlyCoverageStart,
		hasHourlyCoverage,
		false,
		hourlyRetentionDays,
		hourlySweptThrough,
	)
	if err != nil {
		return resolvedRollupWindow{}, fmt.Errorf("resolve 24h rollup granularity: %w", err)
	}
	win, _, err = resolveConsistentUsageWindow(ctx, s.store, win, store.UsageRollupQuery{ProjectID: projectID}, false)
	if err != nil {
		return resolvedRollupWindow{}, fmt.Errorf("check 24h hourly rollup consistency: %w", err)
	}
	return resolvedRollupWindow{
		dbNow:       dbNow,
		since:       win.Since,
		until:       win.Until,
		granularity: win.Granularity,
		reportLoc:   time.UTC,
		startDayKey: dayKey(win.Since, time.UTC),
	}, nil
}

// tokenBearingHitRateUnknowns computes conservative per-field counts of
// presence-tracked token-bearing events whose input, cache_read, or cache_write
// bucket was NULL, plus whether any pre-presence (legacy) events exist in the
// scope (D11b):
//
//	untracked      = max(0, events - presence_tracked_events)
//	trackedNullLB  = max(0, null_token_events - untracked)
//	tokenBearing   = max(0, presence_tracked_events - trackedNullLB)
//	unknown(F)     = max(0, tokenBearing - F_known_events)
//
// Subtracting only trackedNullLB prevents pre-P2.1a daily rows (which carry
// null_token_events > 0 with presence_tracked_events == 0) from masking real
// unknowns in presence-tracked rows. When untracked > 0, hasUntracked is true
// so the caller flags any non-nil hit rate as partial.
func tokenBearingHitRateUnknowns(t store.UsageRollupTotals) (unknownInput, unknownCacheRead, unknownCacheWrite int64, hasUntracked bool) {
	untracked := t.Events - t.PresenceTrackedEvents
	if untracked < 0 {
		untracked = 0
	}
	hasUntracked = untracked > 0

	trackedNullLB := t.NullTokenEvents - untracked
	if trackedNullLB < 0 {
		trackedNullLB = 0
	}
	tokenBearing := t.PresenceTrackedEvents - trackedNullLB
	if tokenBearing <= 0 {
		return 0, 0, 0, hasUntracked
	}
	if d := tokenBearing - t.InputKnownEvents; d > 0 {
		unknownInput = d
	}
	if d := tokenBearing - t.CacheReadKnownEvents; d > 0 {
		unknownCacheRead = d
	}
	if d := tokenBearing - t.CacheWriteKnownEvents; d > 0 {
		unknownCacheWrite = d
	}
	return unknownInput, unknownCacheRead, unknownCacheWrite, hasUntracked
}

// computeCacheHitRate returns cache_read / (input + cache_read + known_cache_write)
// and whether the rate is partial (a true upper bound when only input or
// cache_write has unknowns, or when pre-presence untracked rows are present).
//
// Per D11b:
//   - Token-less (calls-only) events are excluded via trackedNullLB and do not
//     affect the hit rate.
//   - If input or cacheRead is nil, or unknownCacheRead > 0 (numerator would
//     be understated), or the denominator is <= 0, returns (nil, false).
//   - Otherwise computes from known values and sets partial=true whenever
//     cacheWrite is nil, unknownInput > 0, unknownCacheWrite > 0, or
//     hasUntracked is true (events > presence_tracked_events).
func computeCacheHitRate(input, cacheRead, cacheWrite *int64, unknownInput, unknownCacheRead, unknownCacheWrite int64, hasUntracked bool) (*float64, bool) {
	if input == nil || cacheRead == nil || unknownCacheRead > 0 {
		return nil, false
	}
	var cw float64
	partial := hasUntracked || unknownInput > 0 || unknownCacheWrite > 0
	if cacheWrite != nil {
		cw = float64(*cacheWrite)
	} else {
		// cache_write is unknown for all token-bearing events in this scope/bucket
		// (e.g. legacy session-metrics rows or harnesses that do not report cache_write).
		partial = true
	}
	denom := float64(*input) + float64(*cacheRead) + cw
	if denom <= 0 {
		return nil, false
	}
	rate := float64(*cacheRead) / denom
	return &rate, partial
}

func dashboardUnknownEvents(u *AgentUsageBucketUnknownEvents) *DashboardTokenUnknownEvents {
	if u == nil {
		return nil
	}
	return &DashboardTokenUnknownEvents{
		Input:        u.Input,
		Output:       u.Output,
		CacheRead:    u.CacheRead,
		CacheWrite:   u.CacheWrite,
		CacheWrite1h: u.CacheWrite1h,
		Reasoning:    u.Reasoning,
	}
}

func dashboardTokenTotalsFromRollup(t store.UsageRollupTotals) *DashboardTokenTotals {
	resolved := resolveNullableTokens(
		t.Events, t.NullTokenEvents,
		t.Input, t.Output, t.CacheRead, t.CacheWrite, t.CacheWrite1h, t.Reasoning,
		presenceFromTotals(t),
	)
	if t.Saturated {
		resolved.Saturated = true
	}
	unkIn, unkCR, unkCW, hasUntracked := tokenBearingHitRateUnknowns(t)
	rate, partial := computeCacheHitRate(resolved.Input, resolved.CacheRead, resolved.CacheWrite, unkIn, unkCR, unkCW, hasUntracked)
	return &DashboardTokenTotals{
		Input:               resolved.Input,
		Output:              resolved.Output,
		CacheRead:           resolved.CacheRead,
		CacheWrite:          resolved.CacheWrite,
		CacheWrite1h:        resolved.CacheWrite1h,
		Reasoning:           resolved.Reasoning,
		SummableTokens:      resolved.SummableTokens,
		CacheHitRate:        rate,
		CacheHitRatePartial: partial,
		UnknownEvents:       dashboardUnknownEvents(resolved.UnknownEvents),
		Saturated:           resolved.Saturated,
	}
}

func knownSummableTotal(t *DashboardTokenTotals) int64 {
	if t == nil || t.SummableTokens == nil {
		return 0
	}
	return *t.SummableTokens
}

func (s *HubRollupSource) queryMeasurement(
	ctx context.Context,
	projectID string,
	w resolvedRollupWindow,
	totalEvents int64,
	hasLegacyRollups bool,
	hasNonLegacyRollups bool,
) (*DashboardMeasurement, error) {
	cov, err := s.store.GetUsageScopeCoverage(ctx, store.UsageCoverageFilter{
		ProjectID:    projectID,
		Since:        w.since,
		Until:        w.until,
		StaleAfter:   staleHeartbeatMultiplier * defaultHeartbeatInterval,
		StartupGrace: defaultUsageStartupGrace,
		DBNow:        w.dbNow,
	})
	if err != nil {
		return nil, err
	}
	var resolveCap usageCapabilityLookup
	if s.newCapabilityLookup != nil {
		resolveCap = s.newCapabilityLookup(ctx)
	}
	windowIncludesNow := usageWindowIncludesNow(w.until, w.dbNow)
	status, unmeasuredAgents, staleAgents := deriveScopeCoverageSummary(
		cov,
		totalEvents,
		hasLegacyRollups,
		hasNonLegacyRollups,
		w.dbNow,
		windowIncludesNow,
		resolveCap,
	)
	return &DashboardMeasurement{
		Status:           status,
		UnmeasuredAgents: unmeasuredAgents,
		StaleAgents:      staleAgents,
		LastReceiptAt:    cov.LastReceiptAt,
	}, nil
}

// QuerySummary returns aggregate metric counts for the given period from Hub
// rollups and Pipeline C session metadata.
func (s *HubRollupSource) QuerySummary(ctx context.Context, periodDays int, opts ...QueryOption) (*DashboardSummary, error) {
	cfg := applyQueryOptions(opts)
	w, err := s.resolveWindow(ctx, periodDays, cfg)
	if err != nil {
		return nil, err
	}

	rollupRes, err := s.store.QueryUsageRollups(ctx, store.UsageRollupQuery{
		Granularity: w.granularity,
		Since:       w.since,
		Until:       w.until,
		ProjectID:   cfg.ProjectID,
	})
	if err != nil {
		return nil, fmt.Errorf("query usage rollups summary: %w", err)
	}

	tokens := dashboardTokenTotalsFromRollup(rollupRes.Totals)
	summary := &DashboardSummary{
		PeriodDays:          periodDays,
		TimeZone:            w.reportLoc.String(),
		Source:              MetricsSourceHubRollups,
		Granularity:         w.granularity,
		TotalAPICalls:       rollupRes.Totals.Calls,
		TotalTokens:         knownSummableTotal(tokens),
		Tokens:              tokens,
		CacheHitRate:        tokens.CacheHitRate,
		CacheHitRatePartial: tokens.CacheHitRatePartial,
	}

	var queryErrors []string

	agentsRes, err := s.store.CountUsageActiveAgents(ctx, store.UsageAgentCountQuery{
		Granularity:    w.granularity,
		Since:          w.since,
		Until:          w.until,
		ProjectID:      cfg.ProjectID,
		IncludeBuckets: false,
	})
	if err != nil {
		queryErrors = append(queryErrors, fmt.Sprintf("unique agents: %v", err))
	} else {
		summary.UniqueAgents = agentsRes.TotalUniqueAgents
	}

	sessionsRes, err := s.store.CountSessionMetrics(ctx, store.SessionMetricsCountQuery{
		Since:         w.since,
		Until:         w.until,
		ProjectID:     cfg.ProjectID,
		IncludeEvents: false,
	})
	if err != nil {
		summary.totalSessionsFailed = true
		queryErrors = append(queryErrors, fmt.Sprintf("sessions: %v", err))
	} else {
		summary.TotalSessions = sessionsRes.TotalSessions
	}

	measurement, err := s.queryMeasurement(ctx, cfg.ProjectID, w, rollupRes.Totals.Events, rollupRes.HasLegacyRollups, rollupRes.HasNonLegacyRollups)
	if err != nil {
		queryErrors = append(queryErrors, fmt.Sprintf("measurement: %v", err))
	} else {
		summary.Measurement = measurement
	}

	if len(queryErrors) > 0 {
		summary.PartialFailure = true
		summary.PartialErrors = queryErrors
		return summary, fmt.Errorf("partial query failures: %s", strings.Join(queryErrors, "; "))
	}
	return summary, nil
}

// QuerySessions returns daily session counts (from agent_session_metrics,
// deduplicated per calendar day) and daily active agent counts (from usage
// rollups, deduplicated per calendar day).
func (s *HubRollupSource) QuerySessions(ctx context.Context, periodDays int, opts ...QueryOption) (*SessionsView, error) {
	cfg := applyQueryOptions(opts)
	w, err := s.resolveWindow(ctx, periodDays, cfg)
	if err != nil {
		return nil, err
	}

	limit := s.effectiveGroupLimit()

	// Query active agents first so if hourly (hour, agent) pairs truncate we can
	// degrade cleanly to daily granularity before querying sessions/measurement
	// on the exact same window and timezone.
	agentsRes, agentsErr := s.store.CountUsageActiveAgents(ctx, store.UsageAgentCountQuery{
		Granularity:    w.granularity,
		Since:          w.since,
		Until:          w.until,
		ProjectID:      cfg.ProjectID,
		IncludeBuckets: true,
		Limit:          limit,
	})
	if agentsErr == nil && agentsRes.Truncated && w.granularity == store.UsageGranularityHour {
		w = dailyFallbackWindow(w.dbNow, periodDays, cfg.ProjectID)
		agentsRes, agentsErr = s.store.CountUsageActiveAgents(ctx, store.UsageAgentCountQuery{
			Granularity:    w.granularity,
			Since:          w.since,
			Until:          w.until,
			ProjectID:      cfg.ProjectID,
			IncludeBuckets: true,
			Limit:          limit,
		})
	}

	view := &SessionsView{
		PeriodDays:   periodDays,
		TimeZone:     w.reportLoc.String(),
		Source:       MetricsSourceHubRollups,
		Granularity:  w.granularity,
		DailyCounts:  []TimeSeriesPoint{},
		ActiveAgents: []TimeSeriesPoint{},
	}

	var queryErrors []string

	if agentsErr != nil {
		queryErrors = append(queryErrors, fmt.Sprintf("active agents: %v", agentsErr))
	} else {
		if agentsRes.Truncated {
			view.Truncated = true
		}
		if w.granularity == store.UsageGranularityHour {
			dayAgents := make(map[string]map[string]struct{})
			for _, pair := range agentsRes.BucketAgents {
				dk := dayKey(pair.BucketStart, w.reportLoc)
				if dk < w.startDayKey {
					continue
				}
				if dayAgents[dk] == nil {
					dayAgents[dk] = make(map[string]struct{})
				}
				dayAgents[dk][pair.AgentID] = struct{}{}
			}
			pts := mapCountToPoints(dayAgents)
			if agentsRes.Truncated && len(pts) > 0 {
				pts = pts[1:]
			}
			view.ActiveAgents = pts
		} else {
			pts := make([]TimeSeriesPoint, 0, len(agentsRes.BucketCounts))
			for _, bc := range agentsRes.BucketCounts {
				dk := dayKey(bc.BucketStart, time.UTC)
				if dk < w.startDayKey {
					continue
				}
				pts = append(pts, TimeSeriesPoint{Timestamp: dk, Value: bc.Count})
			}
			sort.Slice(pts, func(i, j int) bool { return pts[i].Timestamp < pts[j].Timestamp })
			if agentsRes.Truncated && len(pts) > 0 {
				pts = pts[1:]
			}
			view.ActiveAgents = pts
		}
	}

	sessionsRes, err := s.store.CountSessionMetrics(ctx, store.SessionMetricsCountQuery{
		Since:         w.since,
		Until:         w.until,
		ProjectID:     cfg.ProjectID,
		IncludeEvents: true,
		Limit:         limit,
	})
	if err != nil {
		queryErrors = append(queryErrors, fmt.Sprintf("daily sessions: %v", err))
	} else {
		// Deduplicate (agent_id, session_id) per resolved calendar day.
		type sessionKey struct {
			agentID   string
			sessionID string
		}
		daySessions := make(map[string]map[sessionKey]struct{})
		for _, ref := range sessionsRes.Sessions {
			dk := dayKey(ref.StartedAt, w.reportLoc)
			if dk < w.startDayKey {
				continue
			}
			if daySessions[dk] == nil {
				daySessions[dk] = make(map[sessionKey]struct{})
			}
			daySessions[dk][sessionKey{agentID: ref.AgentID, sessionID: ref.SessionID}] = struct{}{}
		}
		pts := mapCountToPoints(daySessions)
		if sessionsRes.Truncated {
			view.Truncated = true
			// Drop the earliest boundary day so no day appears with a partial count.
			if len(pts) > 0 {
				pts = pts[1:]
			}
		}
		view.DailyCounts = pts
	}

	var totalEvents int64
	var hasLegacyRollups, hasNonLegacyRollups bool
	if rollupRes, rErr := s.store.QueryUsageRollups(ctx, store.UsageRollupQuery{
		Granularity: w.granularity,
		Since:       w.since,
		Until:       w.until,
		ProjectID:   cfg.ProjectID,
	}); rErr != nil {
		queryErrors = append(queryErrors, fmt.Sprintf("rollup totals: %v", rErr))
	} else {
		totalEvents = rollupRes.Totals.Events
		hasLegacyRollups = rollupRes.HasLegacyRollups
		hasNonLegacyRollups = rollupRes.HasNonLegacyRollups
	}

	measurement, err := s.queryMeasurement(ctx, cfg.ProjectID, w, totalEvents, hasLegacyRollups, hasNonLegacyRollups)
	if err != nil {
		queryErrors = append(queryErrors, fmt.Sprintf("measurement: %v", err))
	} else {
		view.Measurement = measurement
	}

	if len(queryErrors) > 0 {
		view.PartialFailure = true
		view.PartialErrors = queryErrors
		return view, fmt.Errorf("partial query failures: %s", strings.Join(queryErrors, "; "))
	}
	return view, nil
}

type modelCallsQuerySet struct {
	byModel   *store.UsageRollupQueryResult
	byHarness *store.UsageRollupQueryResult
	byUser    *store.UsageRollupQueryResult
	errors    []string
}

func (s *HubRollupSource) fetchModelCalls(ctx context.Context, w resolvedRollupWindow, cfg *queryConfig, limit int) modelCallsQuerySet {
	timeDim := timeGroupDim(w.granularity)
	var out modelCallsQuerySet

	res, err := s.store.QueryUsageRollups(ctx, store.UsageRollupQuery{
		Granularity: w.granularity,
		Since:       w.since,
		Until:       w.until,
		ProjectID:   cfg.ProjectID,
		GroupBy:     []string{timeDim, store.UsageGroupByModel},
		Limit:       limit,
	})
	if err != nil {
		out.errors = append(out.errors, fmt.Sprintf("by model: %v", err))
	} else {
		out.byModel = res
	}

	res, err = s.store.QueryUsageRollups(ctx, store.UsageRollupQuery{
		Granularity: w.granularity,
		Since:       w.since,
		Until:       w.until,
		ProjectID:   cfg.ProjectID,
		GroupBy:     []string{timeDim, store.UsageGroupByHarness},
		Limit:       limit,
	})
	if err != nil {
		out.errors = append(out.errors, fmt.Sprintf("by harness: %v", err))
	} else {
		out.byHarness = res
	}

	if cfg.GroupBy == store.UsageGroupByUser {
		res, err = s.store.QueryUsageRollups(ctx, store.UsageRollupQuery{
			Granularity: w.granularity,
			Since:       w.since,
			Until:       w.until,
			ProjectID:   cfg.ProjectID,
			GroupBy:     []string{timeDim, store.UsageGroupByUser},
			Limit:       limit,
		})
		if err != nil {
			out.errors = append(out.errors, fmt.Sprintf("by user: %v", err))
		} else {
			out.byUser = res
		}
	}
	return out
}

func (q modelCallsQuerySet) anyTruncated() bool {
	return (q.byModel != nil && q.byModel.Truncated) ||
		(q.byHarness != nil && q.byHarness.Truncated) ||
		(q.byUser != nil && q.byUser.Truncated)
}

// QueryModelCalls returns API call time series grouped by model, harness, and
// (when groupBy=user) by billing user. If an hourly grouped query hits the
// internal row limit, it degrades cleanly to daily granularity (timeZone="UTC");
// if daily still truncates, Truncated is set and the boundary day is dropped so
// no day ever appears with partial totals.
func (s *HubRollupSource) QueryModelCalls(ctx context.Context, periodDays int, opts ...QueryOption) (*ModelCallsView, error) {
	cfg := applyQueryOptions(opts)
	w, err := s.resolveWindow(ctx, periodDays, cfg)
	if err != nil {
		return nil, err
	}

	limit := s.effectiveGroupLimit()
	qs := s.fetchModelCalls(ctx, w, cfg, limit)
	if w.granularity == store.UsageGranularityHour && qs.anyTruncated() {
		w = dailyFallbackWindow(w.dbNow, periodDays, cfg.ProjectID)
		qs = s.fetchModelCalls(ctx, w, cfg, limit)
	}

	view := &ModelCallsView{
		PeriodDays:  periodDays,
		TimeZone:    w.reportLoc.String(),
		Source:      MetricsSourceHubRollups,
		Granularity: w.granularity,
		GroupBy:     cfg.GroupBy,
		Truncated:   qs.anyTruncated(),
	}

	var totalEvents int64
	var hasLegacyRollups, hasNonLegacyRollups bool
	if qs.byModel != nil {
		totalEvents = qs.byModel.Totals.Events
		hasLegacyRollups = qs.byModel.HasLegacyRollups
		hasNonLegacyRollups = qs.byModel.HasNonLegacyRollups
		view.ByModel = buildCallsSeries(qs.byModel.Groups, w, qs.byModel.Truncated, func(g *store.UsageRollupGroup) string {
			return g.Model
		})
	}
	if qs.byHarness != nil {
		if totalEvents == 0 {
			totalEvents = qs.byHarness.Totals.Events
			hasLegacyRollups = qs.byHarness.HasLegacyRollups
			hasNonLegacyRollups = qs.byHarness.HasNonLegacyRollups
		}
		view.ByHarness = buildCallsSeries(qs.byHarness.Groups, w, qs.byHarness.Truncated, func(g *store.UsageRollupGroup) string {
			return g.Harness
		})
	}
	if qs.byUser != nil {
		if totalEvents == 0 {
			totalEvents = qs.byUser.Totals.Events
			hasLegacyRollups = qs.byUser.HasLegacyRollups
			hasNonLegacyRollups = qs.byUser.HasNonLegacyRollups
		}
		view.ByUser = buildCallsSeries(qs.byUser.Groups, w, qs.byUser.Truncated, func(g *store.UsageRollupGroup) string {
			return g.BillingUserID
		})
	}

	queryErrors := qs.errors
	measurement, err := s.queryMeasurement(ctx, cfg.ProjectID, w, totalEvents, hasLegacyRollups, hasNonLegacyRollups)
	if err != nil {
		queryErrors = append(queryErrors, fmt.Sprintf("measurement: %v", err))
	} else {
		view.Measurement = measurement
	}

	if len(queryErrors) > 0 {
		view.PartialFailure = true
		view.PartialErrors = queryErrors
		return view, fmt.Errorf("partial query failures: %s", strings.Join(queryErrors, "; "))
	}
	return view, nil
}

// QueryTokens returns token usage series (input, output, cacheRead, cacheWrite)
// grouped by model (or harness/user when requested via WithGroupBy), plus
// nullable totals and daily cache hit rate. Unknown buckets are never
// fabricated as 0. If an hourly grouped query hits the internal row limit, it
// degrades cleanly to daily granularity (timeZone="UTC"); if daily still
// truncates, Truncated is set and the boundary day is dropped so no day ever
// appears with partial totals.
func (s *HubRollupSource) QueryTokens(ctx context.Context, periodDays int, opts ...QueryOption) (*TokensView, error) {
	cfg := applyQueryOptions(opts)
	w, err := s.resolveWindow(ctx, periodDays, cfg)
	if err != nil {
		return nil, err
	}

	dim := cfg.GroupBy
	if dim == "" {
		dim = store.UsageGroupByModel
	}
	limit := s.effectiveGroupLimit()

	rollupRes, err := s.store.QueryUsageRollups(ctx, store.UsageRollupQuery{
		Granularity: w.granularity,
		Since:       w.since,
		Until:       w.until,
		ProjectID:   cfg.ProjectID,
		GroupBy:     []string{timeGroupDim(w.granularity), dim},
		Limit:       limit,
	})
	if err != nil {
		return nil, fmt.Errorf("query token rollups: %w", err)
	}
	if w.granularity == store.UsageGranularityHour && rollupRes.Truncated {
		w = dailyFallbackWindow(w.dbNow, periodDays, cfg.ProjectID)
		rollupRes, err = s.store.QueryUsageRollups(ctx, store.UsageRollupQuery{
			Granularity: w.granularity,
			Since:       w.since,
			Until:       w.until,
			ProjectID:   cfg.ProjectID,
			GroupBy:     []string{timeGroupDim(w.granularity), dim},
			Limit:       limit,
		})
		if err != nil {
			return nil, fmt.Errorf("query daily token rollups after truncation: %w", err)
		}
	}

	// Accumulate raw UsageRollupTotals per (label, localDay) and per localDay
	// before calling resolveNullableTokens so presence counters remain exact
	// across multiple UTC hours mapped to the same local calendar day.
	labelDayTotals := make(map[string]map[string]*store.UsageRollupTotals)
	dayTotals := make(map[string]*store.UsageRollupTotals)

	dropDay := ""
	if rollupRes.Truncated {
		dropDay = earliestDayKeyInGroups(rollupRes.Groups, w)
	}

	for _, g := range rollupRes.Groups {
		if g == nil || g.BucketStart == nil {
			continue
		}
		dk := dayKey(*g.BucketStart, w.reportLoc)
		if dk < w.startDayKey || (dropDay != "" && dk == dropDay) {
			continue
		}
		label := normalizeDashboardLabel(labelForDim(g, dim))
		if labelDayTotals[label] == nil {
			labelDayTotals[label] = make(map[string]*store.UsageRollupTotals)
		}
		acc := labelDayTotals[label][dk]
		if acc == nil {
			acc = &store.UsageRollupTotals{}
			labelDayTotals[label][dk] = acc
		}
		addRollupTotals(acc, g.UsageRollupTotals)

		dayAcc := dayTotals[dk]
		if dayAcc == nil {
			dayAcc = &store.UsageRollupTotals{}
			dayTotals[dk] = dayAcc
		}
		addRollupTotals(dayAcc, g.UsageRollupTotals)
	}

	inputMap := make(map[string][]TimeSeriesPoint)
	outputMap := make(map[string][]TimeSeriesPoint)
	cacheReadMap := make(map[string][]TimeSeriesPoint)
	cacheWriteMap := make(map[string][]TimeSeriesPoint)

	for label, byDay := range labelDayTotals {
		for dk, acc := range byDay {
			tok := resolveNullableTokens(
				acc.Events, acc.NullTokenEvents,
				acc.Input, acc.Output, acc.CacheRead, acc.CacheWrite, acc.CacheWrite1h, acc.Reasoning,
				presenceFromTotals(*acc),
			)
			// Only emit points for known (non-nil) buckets; unknown is never 0.
			if tok.Input != nil {
				inputMap[label] = append(inputMap[label], TimeSeriesPoint{Timestamp: dk, Value: *tok.Input})
			}
			if tok.Output != nil {
				outputMap[label] = append(outputMap[label], TimeSeriesPoint{Timestamp: dk, Value: *tok.Output})
			}
			if tok.CacheRead != nil {
				cacheReadMap[label] = append(cacheReadMap[label], TimeSeriesPoint{Timestamp: dk, Value: *tok.CacheRead})
			}
			if tok.CacheWrite != nil {
				cacheWriteMap[label] = append(cacheWriteMap[label], TimeSeriesPoint{Timestamp: dk, Value: *tok.CacheWrite})
			}
		}
	}

	dailyHitRate := make([]NullableFloatPoint, 0, len(dayTotals))
	for dk, acc := range dayTotals {
		tok := resolveNullableTokens(
			acc.Events, acc.NullTokenEvents,
			acc.Input, acc.Output, acc.CacheRead, acc.CacheWrite, acc.CacheWrite1h, acc.Reasoning,
			presenceFromTotals(*acc),
		)
		unkIn, unkCR, unkCW, hasUntracked := tokenBearingHitRateUnknowns(*acc)
		rate, partial := computeCacheHitRate(tok.Input, tok.CacheRead, tok.CacheWrite, unkIn, unkCR, unkCW, hasUntracked)
		dailyHitRate = append(dailyHitRate, NullableFloatPoint{
			Timestamp: dk,
			Value:     rate,
			Partial:   partial,
		})
	}
	sort.Slice(dailyHitRate, func(i, j int) bool {
		return dailyHitRate[i].Timestamp < dailyHitRate[j].Timestamp
	})

	view := &TokensView{
		PeriodDays:        periodDays,
		TimeZone:          w.reportLoc.String(),
		Source:            MetricsSourceHubRollups,
		Granularity:       w.granularity,
		GroupBy:           dim,
		Input:             sortedLabeledSeries(inputMap),
		Output:            sortedLabeledSeries(outputMap),
		CacheRead:         sortedLabeledSeries(cacheReadMap),
		CacheWrite:        sortedLabeledSeries(cacheWriteMap),
		Totals:            dashboardTokenTotalsFromRollup(rollupRes.Totals),
		DailyCacheHitRate: dailyHitRate,
		Truncated:         rollupRes.Truncated,
	}

	measurement, err := s.queryMeasurement(ctx, cfg.ProjectID, w, rollupRes.Totals.Events, rollupRes.HasLegacyRollups, rollupRes.HasNonLegacyRollups)
	if err != nil {
		view.PartialFailure = true
		view.PartialErrors = []string{fmt.Sprintf("measurement: %v", err)}
		return view, fmt.Errorf("partial query failures: measurement: %v", err)
	}
	view.Measurement = measurement
	return view, nil
}

// QueryProjectSummary returns lightweight scalar metrics for a project over
// the last 24 hours from Hub rollups and Pipeline C session metadata. When
// falling back to daily rollups, all fields evaluate the same UTC daily window
// and PeriodLabel states the UTC daily start date.
func (s *HubRollupSource) QueryProjectSummary(ctx context.Context, projectID string) (*ProjectMetricsSummary, error) {
	w, err := s.resolve24hWindow(ctx, projectID)
	if err != nil {
		return nil, err
	}

	rollupRes, err := s.store.QueryUsageRollups(ctx, store.UsageRollupQuery{
		Granularity: w.granularity,
		Since:       w.since,
		Until:       w.until,
		ProjectID:   projectID,
	})
	if err != nil {
		return nil, fmt.Errorf("query project 24h usage rollups: %w", err)
	}

	periodLabel := "Last 24h (hourly buckets)"
	if w.granularity == store.UsageGranularityDay {
		periodLabel = fmt.Sprintf("Since %s (UTC daily)", w.since.UTC().Format(dayKeyLayout))
	}

	tokens := dashboardTokenTotalsFromRollup(rollupRes.Totals)
	summary := &ProjectMetricsSummary{
		Source:              MetricsSourceHubRollups,
		Granularity:         w.granularity,
		APICalls24h:         rollupRes.Totals.Calls,
		TokenUsage24h:       knownSummableTotal(tokens),
		PeriodLabel:         periodLabel,
		Tokens:              tokens,
		CacheHitRate:        tokens.CacheHitRate,
		CacheHitRatePartial: tokens.CacheHitRatePartial,
	}

	var queryErrors []string

	agentsRes, err := s.store.CountUsageActiveAgents(ctx, store.UsageAgentCountQuery{
		Granularity:    w.granularity,
		Since:          w.since,
		Until:          w.until,
		ProjectID:      projectID,
		IncludeBuckets: false,
	})
	if err != nil {
		queryErrors = append(queryErrors, fmt.Sprintf("active agents: %v", err))
	} else {
		summary.ActiveAgents24h = agentsRes.TotalUniqueAgents
	}

	sessionsRes, err := s.store.CountSessionMetrics(ctx, store.SessionMetricsCountQuery{
		Since:         w.since,
		Until:         w.until,
		ProjectID:     projectID,
		IncludeEvents: false,
	})
	if err != nil {
		summary.sessionsFailed = true
		queryErrors = append(queryErrors, fmt.Sprintf("sessions: %v", err))
	} else {
		summary.SessionsCount24h = sessionsRes.TotalSessions
	}

	measurement, err := s.queryMeasurement(ctx, projectID, w, rollupRes.Totals.Events, rollupRes.HasLegacyRollups, rollupRes.HasNonLegacyRollups)
	if err != nil {
		queryErrors = append(queryErrors, fmt.Sprintf("measurement: %v", err))
	} else {
		summary.Measurement = measurement
	}

	if len(queryErrors) > 0 {
		summary.PartialFailure = true
		summary.PartialErrors = queryErrors
		return summary, fmt.Errorf("partial query failures: %s", strings.Join(queryErrors, "; "))
	}
	return summary, nil
}

func timeGroupDim(granularity string) string {
	if granularity == store.UsageGranularityHour {
		return store.UsageGroupByHour
	}
	return store.UsageGroupByDay
}

func normalizeDashboardLabel(raw string) string {
	if strings.TrimSpace(raw) == "" {
		return "(unknown)"
	}
	return raw
}

func labelForDim(g *store.UsageRollupGroup, dim string) string {
	switch dim {
	case store.UsageGroupByHarness:
		return g.Harness
	case store.UsageGroupByUser:
		return g.BillingUserID
	default:
		return g.Model
	}
}

// earliestDayKeyInGroups returns the lexicographically smallest dayKey among
// groups inside w. Since QueryUsageRollups orders newest-first before LIMIT,
// only this earliest boundary day can have partially returned labels.
func earliestDayKeyInGroups(groups []*store.UsageRollupGroup, w resolvedRollupWindow) string {
	earliest := ""
	for _, g := range groups {
		if g == nil || g.BucketStart == nil {
			continue
		}
		dk := dayKey(*g.BucketStart, w.reportLoc)
		if dk < w.startDayKey {
			continue
		}
		if earliest == "" || dk < earliest {
			earliest = dk
		}
	}
	return earliest
}

func buildCallsSeries(groups []*store.UsageRollupGroup, w resolvedRollupWindow, truncated bool, labelFn func(*store.UsageRollupGroup) string) []LabeledTimeSeries {
	dropDay := ""
	if truncated {
		dropDay = earliestDayKeyInGroups(groups, w)
	}
	byLabel := make(map[string]map[string]int64)
	for _, g := range groups {
		if g == nil || g.BucketStart == nil {
			continue
		}
		dk := dayKey(*g.BucketStart, w.reportLoc)
		if dk < w.startDayKey || (dropDay != "" && dk == dropDay) {
			continue
		}
		label := normalizeDashboardLabel(labelFn(g))
		if byLabel[label] == nil {
			byLabel[label] = make(map[string]int64)
		}
		byLabel[label][dk] = satAddInt64(byLabel[label][dk], g.Calls)
	}
	var out []LabeledTimeSeries
	for label, dayTotals := range byLabel {
		pts := make([]TimeSeriesPoint, 0, len(dayTotals))
		for dk, total := range dayTotals {
			pts = append(pts, TimeSeriesPoint{Timestamp: dk, Value: total})
		}
		sort.Slice(pts, func(i, j int) bool { return pts[i].Timestamp < pts[j].Timestamp })
		out = append(out, LabeledTimeSeries{Label: label, Points: pts})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Label < out[j].Label })
	return out
}

func sortedLabeledSeries(m map[string][]TimeSeriesPoint) []LabeledTimeSeries {
	out := make([]LabeledTimeSeries, 0, len(m))
	for label, pts := range m {
		sort.Slice(pts, func(i, j int) bool { return pts[i].Timestamp < pts[j].Timestamp })
		out = append(out, LabeledTimeSeries{Label: label, Points: pts})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Label < out[j].Label })
	return out
}

func mapCountToPoints[K comparable](byDay map[string]map[K]struct{}) []TimeSeriesPoint {
	pts := make([]TimeSeriesPoint, 0, len(byDay))
	for dk, set := range byDay {
		pts = append(pts, TimeSeriesPoint{Timestamp: dk, Value: int64(len(set))})
	}
	sort.Slice(pts, func(i, j int) bool { return pts[i].Timestamp < pts[j].Timestamp })
	return pts
}

func addRollupTotals(dst *store.UsageRollupTotals, src store.UsageRollupTotals) {
	dst.Calls = satAddInt64(dst.Calls, src.Calls)
	dst.ErrorCalls = satAddInt64(dst.ErrorCalls, src.ErrorCalls)
	dst.Input = satAddInt64(dst.Input, src.Input)
	dst.Output = satAddInt64(dst.Output, src.Output)
	dst.CacheRead = satAddInt64(dst.CacheRead, src.CacheRead)
	dst.CacheWrite = satAddInt64(dst.CacheWrite, src.CacheWrite)
	dst.CacheWrite1h = satAddInt64(dst.CacheWrite1h, src.CacheWrite1h)
	dst.Reasoning = satAddInt64(dst.Reasoning, src.Reasoning)
	dst.ReportedCostMicros = satAddInt64(dst.ReportedCostMicros, src.ReportedCostMicros)
	dst.Events = satAddInt64(dst.Events, src.Events)
	dst.NullTokenEvents = satAddInt64(dst.NullTokenEvents, src.NullTokenEvents)
	dst.InputKnownEvents = satAddInt64(dst.InputKnownEvents, src.InputKnownEvents)
	dst.OutputKnownEvents = satAddInt64(dst.OutputKnownEvents, src.OutputKnownEvents)
	dst.CacheReadKnownEvents = satAddInt64(dst.CacheReadKnownEvents, src.CacheReadKnownEvents)
	dst.CacheWriteKnownEvents = satAddInt64(dst.CacheWriteKnownEvents, src.CacheWriteKnownEvents)
	dst.CacheWrite1hKnownEvents = satAddInt64(dst.CacheWrite1hKnownEvents, src.CacheWrite1hKnownEvents)
	dst.ReasoningKnownEvents = satAddInt64(dst.ReasoningKnownEvents, src.ReasoningKnownEvents)
	dst.PresenceTrackedEvents = satAddInt64(dst.PresenceTrackedEvents, src.PresenceTrackedEvents)
	if src.Saturated {
		dst.Saturated = true
	}
}

func satAddInt64(a, b int64) int64 {
	if b > 0 && a > math.MaxInt64-b {
		return math.MaxInt64
	}
	return a + b
}
