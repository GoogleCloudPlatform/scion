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
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/experiments"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/GoogleCloudPlatform/scion/pkg/telemetrycontract"
)

// MeasurementStatus values for Track 1 usage read responses (Tokenomics design §3.3).
const (
	MeasurementStatusMeasured     = "measured"
	MeasurementStatusPartial      = "partial"
	MeasurementStatusCallsOnly    = "calls_only"
	MeasurementStatusUnmeasured   = "unmeasured"
	MeasurementStatusLegacyClient = "legacy_client"
	MeasurementStatusStale        = "stale"

	defaultUsageWindowDays    = 30
	defaultUsageRunSummaryCap = 20
	defaultHeartbeatInterval  = time.Duration(telemetrycontract.DefaultUsageHeartbeatIntervalSec) * time.Second
	staleHeartbeatMultiplier  = 3
	meterStartupGrace         = 60 * time.Second
)

// AgentUsageBucketUnknownEvents reports per-bucket counts of events in the
// window where that bucket was NULL while other events in the same group
// reported known values (mixed known+unknown). Omitted when all buckets are
// uniformly known or uniformly unknown.
type AgentUsageBucketUnknownEvents struct {
	Input        int64 `json:"input,omitempty"`
	Output       int64 `json:"output,omitempty"`
	CacheRead    int64 `json:"cache_read,omitempty"`
	CacheWrite   int64 `json:"cache_write,omitempty"`
	CacheWrite1h int64 `json:"cache_write_1h,omitempty"`
	Reasoning    int64 `json:"reasoning,omitempty"`
}

// AgentUsageTokens holds nullable canonical Track 1 token buckets (§3.1).
// A nil pointer serializes as JSON null (unknown) and is never fabricated as 0.
// SummableTokens is the sum of non-nil {Input, Output, CacheRead, CacheWrite},
// or nil when all four summable buckets are unknown.
type AgentUsageTokens struct {
	Input          *int64                         `json:"input"`
	Output         *int64                         `json:"output"`
	CacheRead      *int64                         `json:"cache_read"`
	CacheWrite     *int64                         `json:"cache_write"`
	CacheWrite1h   *int64                         `json:"cache_write_1h"`
	Reasoning      *int64                         `json:"reasoning"`
	SummableTokens *int64                         `json:"summable_tokens"`
	UnknownEvents  *AgentUsageBucketUnknownEvents `json:"unknown_events,omitempty"`
}

// AgentUsageModelBreakdown reports per-model usage totals over the query window.
type AgentUsageModelBreakdown struct {
	Model           string           `json:"model"`
	Harness         string           `json:"harness,omitempty"`
	UsageSource     string           `json:"usage_source,omitempty"`
	Calls           int64            `json:"calls"`
	ErrorCalls      int64            `json:"error_calls"`
	Events          int64            `json:"events"`
	NullTokenEvents int64            `json:"null_token_events"`
	Tokens          AgentUsageTokens `json:"tokens"`
}

// AgentUsageRunSummary is a compact attribution summary for recent runs of an agent.
type AgentUsageRunSummary struct {
	RunID         string    `json:"run_id"`
	Generation    int       `json:"generation"`
	Harness       string    `json:"harness,omitempty"`
	HarnessConfig string    `json:"harness_config,omitempty"`
	Template      string    `json:"template,omitempty"`
	CreatedAt     time.Time `json:"created_at"`
}

// AgentUsageWindow describes the effective day-aligned UTC [since, until]
// window evaluated for a usage read response.
type AgentUsageWindow struct {
	Since time.Time `json:"since"`
	Until time.Time `json:"until"`
}

// AgentUsageResponse is returned by GET /api/v1/agents/{id}/usage (§4.6).
type AgentUsageResponse struct {
	AgentID           string                     `json:"agent_id"`
	ProjectID         string                     `json:"project_id"`
	Window            AgentUsageWindow           `json:"window"`
	Calls             int64                      `json:"calls"`
	ErrorCalls        int64                      `json:"error_calls"`
	Events            int64                      `json:"events"`
	NullTokenEvents   int64                      `json:"null_token_events"`
	Tokens            AgentUsageTokens           `json:"tokens"`
	ByModel           []AgentUsageModelBreakdown `json:"by_model"`
	Runs              []AgentUsageRunSummary     `json:"runs,omitempty"`
	MeasurementStatus string                     `json:"measurement_status"`
	AsOf              time.Time                  `json:"as_of"`
	LastReceiptAt     *time.Time                 `json:"last_receipt_at"`
}

// handleAgentUsage serves GET /api/v1/agents/{id}/usage.
// Gated behind tokenomics.usage_ledger (returns 404 when off).
func (s *Server) handleAgentUsage(w http.ResponseWriter, r *http.Request, agentID string) {
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
	agent, err := s.store.GetAgent(ctx, agentID)
	if err != nil {
		writeErrorFromErr(w, err, "")
		return
	}
	if !s.authorizeAgentUsageRead(w, r, agent) {
		return
	}

	dbNow, err := s.store.DBNow(ctx)
	if err != nil {
		writeErrorFromErr(w, err, "")
		return
	}

	since, until, err := parseUsageWindow(r.URL.Query().Get("since"), r.URL.Query().Get("until"), dbNow)
	if err != nil {
		ValidationError(w, err.Error(), nil)
		return
	}

	rollups, err := s.store.ListUsageRollupsDailyByAgent(ctx, agent.ID, since, until)
	if err != nil {
		writeErrorFromErr(w, err, "")
		return
	}

	presence, err := s.store.GetUsageTokenPresenceByAgent(ctx, agent.ID, since, until)
	if err != nil {
		writeErrorFromErr(w, err, "")
		return
	}

	runs, err := s.store.ListRunAttributionsByAgent(ctx, agent.ID, defaultUsageRunSummaryCap)
	if err != nil {
		writeErrorFromErr(w, err, "")
		return
	}

	meterRows, err := s.store.ListMeterHealthByAgent(ctx, agent.ID)
	if err != nil {
		writeErrorFromErr(w, err, "")
		return
	}

	targetRunID := agent.RunID
	if targetRunID == "" && len(runs) > 0 && runs[0] != nil {
		targetRunID = runs[0].RunID
	}
	var currentRunHasEvents bool
	if targetRunID != "" {
		currentRunHasEvents, err = s.store.HasUsageEventsForRun(ctx, agent.ID, targetRunID)
		if err != nil {
			writeErrorFromErr(w, err, "")
			return
		}
	}

	resp := buildAgentUsageResponse(agent, since, until, dbNow, rollups, presence, runs, meterRows, currentRunHasEvents)
	writeJSON(w, http.StatusOK, resp)
}

// authorizeAgentUsageRead enforces agent-scoped read authz consistent with
// other agent read endpoints:
//   - UserIdentity: requires ActionRead on the agent resource (owner, project
//     member with agent.read, hub admin); unrelated users get 403.
//   - AgentIdentity: only the agent itself (agentIdent.ID() == agent.ID) is
//     allowed; another agent's token gets 403.
func (s *Server) authorizeAgentUsageRead(w http.ResponseWriter, r *http.Request, agent *store.Agent) bool {
	if agent == nil {
		NotFound(w, "Agent")
		return false
	}
	ctx := r.Context()
	identity := GetIdentityFromContext(ctx)
	if identity == nil {
		Unauthorized(w)
		return false
	}
	switch ident := identity.(type) {
	case UserIdentity:
		decision := s.authzService.CheckAccess(ctx, ident, agentResource(agent), ActionRead)
		if !decision.Allowed {
			Forbidden(w)
			return false
		}
		return true
	case AgentIdentity:
		if ident.ID() != agent.ID {
			Forbidden(w)
			return false
		}
		return true
	default:
		Forbidden(w)
		return false
	}
}

// parseUsageWindow parses optional since and until query parameters relative
// to now (UTC) and snaps the effective window to inclusive UTC day boundaries
// ([00:00:00Z, 23:59:59.999999Z]) so daily rollups and event presence queries
// cover the exact same time span and the response reports the effective window.
func parseUsageWindow(sinceRaw, untilRaw string, now time.Time) (time.Time, time.Time, error) {
	now = now.UTC().Truncate(time.Microsecond)
	until := now
	if strings.TrimSpace(untilRaw) != "" {
		parsed, err := parseUsageTimeParam(untilRaw, now, true)
		if err != nil {
			return time.Time{}, time.Time{}, fmt.Errorf("invalid until parameter: %w", err)
		}
		until = parsed
	}

	since := until.Add(-time.Duration(defaultUsageWindowDays) * 24 * time.Hour)
	if strings.TrimSpace(sinceRaw) != "" {
		parsed, err := parseUsageTimeParam(sinceRaw, until, false)
		if err != nil {
			return time.Time{}, time.Time{}, fmt.Errorf("invalid since parameter: %w", err)
		}
		since = parsed
	}

	if since.After(until) {
		return time.Time{}, time.Time{}, fmt.Errorf("since (%s) must not be after until (%s)", since.Format(time.RFC3339), until.Format(time.RFC3339))
	}

	// Snap to inclusive UTC day boundaries to match usage_rollups_daily grain.
	snappedSince := time.Date(since.Year(), since.Month(), since.Day(), 0, 0, 0, 0, time.UTC)
	snappedUntil := time.Date(until.Year(), until.Month(), until.Day(), 0, 0, 0, 0, time.UTC).
		Add(24*time.Hour - time.Microsecond)
	return snappedSince, snappedUntil, nil
}

func parseUsageTimeParam(raw string, ref time.Time, endOfDay bool) (time.Time, error) {
	s := strings.TrimSpace(raw)
	if t, err := time.Parse(time.RFC3339Nano, s); err == nil {
		return t.UTC().Truncate(time.Microsecond), nil
	}
	if d, err := time.Parse("2006-01-02", s); err == nil {
		d = d.UTC()
		if endOfDay {
			return d.Add(24*time.Hour - time.Microsecond), nil
		}
		return d, nil
	}
	if !endOfDay {
		// Support relative day durations like "7d", "30d" and Go durations like "24h".
		if strings.HasSuffix(s, "d") && len(s) > 1 {
			days, err := strconv.Atoi(strings.TrimSuffix(s, "d"))
			if err == nil && days > 0 {
				return ref.Add(-time.Duration(days) * 24 * time.Hour).UTC().Truncate(time.Microsecond), nil
			}
		}
		if dur, err := time.ParseDuration(s); err == nil && dur > 0 {
			return ref.Add(-dur).UTC().Truncate(time.Microsecond), nil
		}
	}
	return time.Time{}, fmt.Errorf("expected RFC 3339 timestamp, YYYY-MM-DD date, or positive duration (e.g. 7d, 24h), got %q", raw)
}

type usageBucketPresence struct {
	eventCount        int64
	inputCount        int64
	outputCount       int64
	cacheReadCount    int64
	cacheWriteCount   int64
	cacheWrite1hCount int64
	reasoningCount    int64
}

func (p *usageBucketPresence) add(src *store.UsageTokenPresence) {
	p.eventCount += src.EventCount
	p.inputCount += src.InputCount
	p.outputCount += src.OutputCount
	p.cacheReadCount += src.CacheReadCount
	p.cacheWriteCount += src.CacheWriteCount
	p.cacheWrite1hCount += src.CacheWrite1hCount
	p.reasoningCount += src.ReasoningCount
}

type modelRollupAgg struct {
	model           string
	harness         string
	usageSource     string
	calls           int64
	errorCalls      int64
	input           int64
	output          int64
	cacheRead       int64
	cacheWrite      int64
	cacheWrite1h    int64
	reasoning       int64
	events          int64
	nullTokenEvents int64
	presence        usageBucketPresence
}

func buildAgentUsageResponse(
	agent *store.Agent,
	since, until, dbNow time.Time,
	rollups []*store.UsageRollupDaily,
	presence []*store.UsageTokenPresence,
	runs []*store.RunAttribution,
	meterRows []*store.MeterHealth,
	currentRunHasEvents bool,
) AgentUsageResponse {
	type modelKey struct {
		model       string
		harness     string
		usageSource string
	}

	presenceByKey := make(map[modelKey]*usageBucketPresence, len(presence))
	var totalPresence usageBucketPresence
	for _, p := range presence {
		if p == nil {
			continue
		}
		k := modelKey{model: p.Model, harness: p.Harness, usageSource: p.UsageSource}
		dst, ok := presenceByKey[k]
		if !ok {
			dst = &usageBucketPresence{}
			presenceByKey[k] = dst
		}
		dst.add(p)
		totalPresence.add(p)
	}

	byModelMap := make(map[modelKey]*modelRollupAgg)
	var (
		totalCalls           int64
		totalErrorCalls      int64
		totalEvents          int64
		totalNullTokenEvents int64
		totalInput           int64
		totalOutput          int64
		totalCacheRead       int64
		totalCacheWrite      int64
		totalCacheWrite1h    int64
		totalReasoning       int64
	)

	for _, r := range rollups {
		if r == nil {
			continue
		}
		totalCalls += r.Calls
		totalErrorCalls += r.ErrorCalls
		totalEvents += r.Events
		totalNullTokenEvents += r.NullTokenEvents
		totalInput += r.Input
		totalOutput += r.Output
		totalCacheRead += r.CacheRead
		totalCacheWrite += r.CacheWrite
		totalCacheWrite1h += r.CacheWrite1h
		totalReasoning += r.Reasoning

		k := modelKey{model: r.Model, harness: r.Harness, usageSource: r.UsageSource}
		agg, ok := byModelMap[k]
		if !ok {
			agg = &modelRollupAgg{
				model:       r.Model,
				harness:     r.Harness,
				usageSource: r.UsageSource,
			}
			if p, hasP := presenceByKey[k]; hasP {
				agg.presence = *p
			}
			byModelMap[k] = agg
		}
		agg.calls += r.Calls
		agg.errorCalls += r.ErrorCalls
		agg.input += r.Input
		agg.output += r.Output
		agg.cacheRead += r.CacheRead
		agg.cacheWrite += r.CacheWrite
		agg.cacheWrite1h += r.CacheWrite1h
		agg.reasoning += r.Reasoning
		agg.events += r.Events
		agg.nullTokenEvents += r.NullTokenEvents
	}

	byModel := make([]AgentUsageModelBreakdown, 0, len(byModelMap))
	for _, agg := range byModelMap {
		tokens := resolveNullableTokens(
			agg.events, agg.nullTokenEvents,
			agg.input, agg.output, agg.cacheRead, agg.cacheWrite, agg.cacheWrite1h, agg.reasoning,
			agg.presence,
		)
		byModel = append(byModel, AgentUsageModelBreakdown{
			Model:           agg.model,
			Harness:         agg.harness,
			UsageSource:     agg.usageSource,
			Calls:           agg.calls,
			ErrorCalls:      agg.errorCalls,
			Events:          agg.events,
			NullTokenEvents: agg.nullTokenEvents,
			Tokens:          tokens,
		})
	}
	sort.Slice(byModel, func(i, j int) bool {
		si := int64(-1)
		if byModel[i].Tokens.SummableTokens != nil {
			si = *byModel[i].Tokens.SummableTokens
		}
		sj := int64(-1)
		if byModel[j].Tokens.SummableTokens != nil {
			sj = *byModel[j].Tokens.SummableTokens
		}
		if si != sj {
			return si > sj
		}
		if byModel[i].Calls != byModel[j].Calls {
			return byModel[i].Calls > byModel[j].Calls
		}
		return byModel[i].Model < byModel[j].Model
	})

	totalTokens := resolveNullableTokens(
		totalEvents, totalNullTokenEvents,
		totalInput, totalOutput, totalCacheRead, totalCacheWrite, totalCacheWrite1h, totalReasoning,
		totalPresence,
	)

	runSummaries := make([]AgentUsageRunSummary, 0, len(runs))
	var currentRunAttr *store.RunAttribution
	for _, ra := range runs {
		if ra == nil {
			continue
		}
		if agent.RunID != "" && ra.RunID == agent.RunID {
			currentRunAttr = ra
		}
		runSummaries = append(runSummaries, AgentUsageRunSummary{
			RunID:         ra.RunID,
			Generation:    ra.Generation,
			Harness:       ra.Harness,
			HarnessConfig: ra.HarnessConfig,
			Template:      ra.Template,
			CreatedAt:     ra.CreatedAt,
		})
	}
	if currentRunAttr == nil && len(runs) > 0 {
		currentRunAttr = runs[0]
	}

	// Select meter_health rows for the current run for status derivation,
	// and compute the latest last_receipt_at across all rows.
	var lastReceiptAt *time.Time
	var currentRunMeterRows []*store.MeterHealth
	targetRunID := agent.RunID
	if targetRunID == "" && currentRunAttr != nil {
		targetRunID = currentRunAttr.RunID
	}
	for _, mh := range meterRows {
		if mh == nil {
			continue
		}
		if lastReceiptAt == nil || mh.LastReceiptAt.After(*lastReceiptAt) {
			t := mh.LastReceiptAt.UTC()
			lastReceiptAt = &t
		}
		if targetRunID != "" && mh.RunID == targetRunID {
			currentRunMeterRows = append(currentRunMeterRows, mh)
		}
	}

	// If no active/attributed run exists at all (e.g. legacy/backfill-free
	// agent with historical rollups only), fall back to window events;
	// otherwise strictly evaluate the current run's events and meter_health.
	hasRunData := currentRunHasEvents
	if targetRunID == "" {
		hasRunData = totalEvents > 0
	}

	status := deriveMeasurementStatus(measurementStatusInput{
		RunAttribution:    currentRunAttr,
		MeterHealth:       currentRunMeterRows,
		HasRunUsageEvents: hasRunData,
		DBNow:             dbNow,
	})

	return AgentUsageResponse{
		AgentID:           agent.ID,
		ProjectID:         agent.ProjectID,
		Window:            AgentUsageWindow{Since: since, Until: until},
		Calls:             totalCalls,
		ErrorCalls:        totalErrorCalls,
		Events:            totalEvents,
		NullTokenEvents:   totalNullTokenEvents,
		Tokens:            totalTokens,
		ByModel:           byModel,
		Runs:              runSummaries,
		MeasurementStatus: status,
		AsOf:              dbNow,
		LastReceiptAt:     lastReceiptAt,
	}
}

// resolveNullableTokens converts non-nullable daily rollup sums into canonical
// three-state nullable token buckets (§3.1). A bucket with zero non-null
// contributions — or whose presence cannot be verified (for example, events
// past ledger retention) — surfaces as nil (JSON null), never 0.
func resolveNullableTokens(
	events, nullTokenEvents int64,
	input, output, cacheRead, cacheWrite, cacheWrite1h, reasoning int64,
	presence usageBucketPresence,
) AgentUsageTokens {
	if events <= 0 || nullTokenEvents >= events {
		return AgentUsageTokens{}
	}

	resolveBucket := func(sum int64, nonNullCount int64) (*int64, int64) {
		var unknownCount int64
		if presence.eventCount > 0 && nonNullCount > 0 && nonNullCount < presence.eventCount {
			unknownCount = presence.eventCount - nonNullCount
		}
		if sum > 0 {
			v := sum
			return &v, unknownCount
		}
		// Sum is 0. Only emit measured 0 when usage_events presence confirms at
		// least one non-null event for this bucket; otherwise surface nil
		// (unknown) — never fabricate 0 (§3.1).
		if presence.eventCount > 0 && nonNullCount > 0 {
			var zero int64
			return &zero, unknownCount
		}
		return nil, 0
	}

	inVal, inUnk := resolveBucket(input, presence.inputCount)
	outVal, outUnk := resolveBucket(output, presence.outputCount)
	crVal, crUnk := resolveBucket(cacheRead, presence.cacheReadCount)
	cwVal, cwUnk := resolveBucket(cacheWrite, presence.cacheWriteCount)
	cw1hVal, cw1hUnk := resolveBucket(cacheWrite1h, presence.cacheWrite1hCount)
	reasVal, reasUnk := resolveBucket(reasoning, presence.reasoningCount)

	out := AgentUsageTokens{
		Input:        inVal,
		Output:       outVal,
		CacheRead:    crVal,
		CacheWrite:   cwVal,
		CacheWrite1h: cw1hVal,
		Reasoning:    reasVal,
	}

	if inUnk > 0 || outUnk > 0 || crUnk > 0 || cwUnk > 0 || cw1hUnk > 0 || reasUnk > 0 {
		out.UnknownEvents = &AgentUsageBucketUnknownEvents{
			Input:        inUnk,
			Output:       outUnk,
			CacheRead:    crUnk,
			CacheWrite:   cwUnk,
			CacheWrite1h: cw1hUnk,
			Reasoning:    reasUnk,
		}
	}

	// SummableTokens = sum of known {input, output, cache_read, cache_write} (§3.1).
	// Reasoning and cache_write_1h are subsets and are never added to SummableTokens.
	var summable int64
	hasAnySummable := false
	for _, p := range []*int64{out.Input, out.Output, out.CacheRead, out.CacheWrite} {
		if p != nil {
			summable += *p
			hasAnySummable = true
		}
	}
	if hasAnySummable {
		out.SummableTokens = &summable
	}
	return out
}

// measurementStatusInput holds the run-scoped inputs to deriveMeasurementStatus.
// Kept in a single struct + single function so P2 can extend it with usage.yaml
// harness capability metadata without changing call sites.
type measurementStatusInput struct {
	RunAttribution    *store.RunAttribution
	MeterHealth       []*store.MeterHealth
	HasRunUsageEvents bool
	DBNow             time.Time
}

// deriveMeasurementStatus computes the P1 measurement_status for an agent's
// current run:
//   - "stale" when an open meter_health row for the run is past 3 heartbeat
//     intervals (180s) after the 60s startup grace, evaluated at read time from
//     DBNow (§4.2 item 7);
//   - "measured" when the current run has received Track 1 usage events and its
//     meter is fresh (or cleanly closed with final_state="final");
//   - "unmeasured" when no Track 1 usage data has been received for the current
//     run.
//
// P2 extends this function with usage.yaml capability metadata (partial,
// calls_only, legacy_client).
func deriveMeasurementStatus(in measurementStatusInput) string {
	var latest *store.MeterHealth
	for _, mh := range in.MeterHealth {
		if mh == nil {
			continue
		}
		if latest == nil || mh.LastReceiptAt.After(latest.LastReceiptAt) {
			latest = mh
		}
	}

	if latest != nil {
		pastGrace := true
		if in.RunAttribution != nil && !in.RunAttribution.CreatedAt.IsZero() {
			pastGrace = in.DBNow.Sub(in.RunAttribution.CreatedAt) > meterStartupGrace
		}
		staleAfter := staleHeartbeatMultiplier * defaultHeartbeatInterval
		if latest.FinalState == store.MeterHealthFinalStateOpen &&
			pastGrace &&
			in.DBNow.Sub(latest.LastReceiptAt) > staleAfter {
			return MeasurementStatusStale
		}
		// TODO(P2): map latest.FinalState == store.MeterHealthFinalStateIncomplete
		// to MeasurementStatusPartial once P2 adds partial-coverage handling.
	}

	if in.HasRunUsageEvents {
		return MeasurementStatusMeasured
	}
	return MeasurementStatusUnmeasured
}
