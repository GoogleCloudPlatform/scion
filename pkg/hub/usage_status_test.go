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
	"errors"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
)

func makeUsageCap(source api.UsageLiveSource, tokenTypes, naTypes []string, countsFailed bool) *api.HarnessUsageCapabilities {
	return &api.HarnessUsageCapabilities{
		SchemaVersion: 1,
		Live: api.HarnessUsageLiveCapabilities{
			Source:                  source,
			TokenTypes:              tokenTypes,
			NotApplicableTokenTypes: naTypes,
			CountsFailedCalls:       countsFailed,
		},
	}
}

func TestClassifyUsageLiveCapability(t *testing.T) {
	tests := []struct {
		name string
		cap  *api.HarnessUsageCapabilities
		want usageCapabilityKind
	}{
		{
			name: "nil capability is unknown",
			cap:  nil,
			want: usageCapUnknown,
		},
		{
			name: "empty source is unknown",
			cap:  &api.HarnessUsageCapabilities{},
			want: usageCapUnknown,
		},
		{
			name: "source none is unmeasured",
			cap:  makeUsageCap(api.UsageLiveSourceNone, nil, nil, false),
			want: usageCapUnmeasured,
		},
		{
			name: "hooks with no token types is calls_only (antigravity)",
			cap:  makeUsageCap(api.UsageLiveSourceHooks, nil, nil, false),
			want: usageCapCallsOnly,
		},
		{
			name: "claude all four summable types is full",
			cap:  makeUsageCap(api.UsageLiveSourceNative, []string{"input", "output", "cache_read", "cache_write"}, nil, true),
			want: usageCapFull,
		},
		{
			name: "gemini-cli with cache_write not_applicable is full",
			cap:  makeUsageCap(api.UsageLiveSourceNative, []string{"input", "output", "cache_read", "reasoning"}, []string{"cache_write"}, true),
			want: usageCapFull,
		},
		{
			name: "copilot counts_failed_calls=false stays full",
			cap:  makeUsageCap(api.UsageLiveSourceNative, []string{"input", "output", "cache_read", "cache_write", "reasoning"}, nil, false),
			want: usageCapFull,
		},
		{
			name: "missing cache_read without not_applicable is partial",
			cap:  makeUsageCap(api.UsageLiveSourceHooks, []string{"input", "output", "cache_write"}, nil, false),
			want: usageCapPartial,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, classifyUsageLiveCapability(tc.cap))
		})
	}
}

func TestDeriveMeasurementStatus_TruthTable(t *testing.T) {
	now := time.Date(2026, 11, 27, 12, 0, 0, 0, time.UTC)

	capNone := makeUsageCap(api.UsageLiveSourceNone, nil, nil, false)
	capCallsOnly := makeUsageCap(api.UsageLiveSourceHooks, nil, nil, false)
	capPartial := makeUsageCap(api.UsageLiveSourceHooks, []string{"input", "output"}, nil, false)
	capFull := makeUsageCap(api.UsageLiveSourceNative, []string{"input", "output", "cache_read"}, []string{"cache_write"}, true)

	freshOpen := []*store.MeterHealth{{
		FinalState:    store.MeterHealthFinalStateOpen,
		LastReceiptAt: now.Add(-30 * time.Second),
	}}
	exactBoundaryOpen := []*store.MeterHealth{{
		FinalState:    store.MeterHealthFinalStateOpen,
		LastReceiptAt: now.Add(-180 * time.Second), // == staleAfter -> NOT stale
	}}
	staleOpen := []*store.MeterHealth{{
		FinalState:    store.MeterHealthFinalStateOpen,
		LastReceiptAt: now.Add(-181 * time.Second), // > staleAfter -> stale
	}}
	closedFinal := []*store.MeterHealth{{
		FinalState:    store.MeterHealthFinalStateFinal,
		LastReceiptAt: now.Add(-10 * time.Minute),
	}}
	incompleteMH := []*store.MeterHealth{{
		FinalState:    store.MeterHealthFinalStateIncomplete,
		LastReceiptAt: now.Add(-30 * time.Second),
	}}
	driftMH := []*store.MeterHealth{{
		FinalState:          store.MeterHealthFinalStateOpen,
		LastReceiptAt:       now.Add(-30 * time.Second),
		SchemaDriftDetected: true,
	}}

	running := string(state.PhaseRunning)
	stopped := string(state.PhaseStopped)

	tests := []struct {
		name string
		in   measurementStatusInput
		want string
	}{
		{
			name: "row1: source=none running past grace no receipt -> unmeasured (not stale)",
			in: measurementStatusInput{
				Usage:        capNone,
				AgentPhase:   running,
				RunStartedAt: now.Add(-5 * time.Minute),
				DBNow:        now,
			},
			want: MeasurementStatusUnmeasured,
		},
		{
			name: "row1b: source=none even with stale open meter -> unmeasured",
			in: measurementStatusInput{
				Usage:       capNone,
				AgentPhase:  running,
				MeterHealth: staleOpen,
				DBNow:       now,
			},
			want: MeasurementStatusUnmeasured,
		},
		{
			name: "row2: running open meter >180s -> stale",
			in: measurementStatusInput{
				Usage:             capFull,
				AgentPhase:        running,
				MeterHealth:       staleOpen,
				HasRunUsageEvents: true,
				DBNow:             now,
			},
			want: MeasurementStatusStale,
		},
		{
			name: "row2b: running open meter ==180s boundary -> measured (not stale)",
			in: measurementStatusInput{
				Usage:             capFull,
				AgentPhase:        running,
				MeterHealth:       exactBoundaryOpen,
				HasRunUsageEvents: true,
				DBNow:             now,
			},
			want: MeasurementStatusMeasured,
		},
		{
			name: "row2c: stopped agent with old open meter -> measured (not stale)",
			in: measurementStatusInput{
				Usage:             capFull,
				AgentPhase:        stopped,
				MeterHealth:       staleOpen,
				HasRunUsageEvents: true,
				DBNow:             now,
			},
			want: MeasurementStatusMeasured,
		},
		{
			name: "row2d: historical window (WindowIncludesNow=false) with stale open meter -> measured (D7a)",
			in: measurementStatusInput{
				Usage:             capFull,
				AgentPhase:        running,
				WindowIncludesNow: boolPtr(false),
				MeterHealth:       staleOpen,
				HasRunUsageEvents: true,
				DBNow:             now,
			},
			want: MeasurementStatusMeasured,
		},
		{
			name: "row3: P1 deferral (a) running known live harness no receipt past 60s grace -> stale",
			in: measurementStatusInput{
				Usage:        capFull,
				AgentPhase:   running,
				RunStartedAt: now.Add(-61 * time.Second),
				DBNow:        now,
			},
			want: MeasurementStatusStale,
		},
		{
			name: "row3b: running known live harness within 60s grace boundary (==60s) -> unmeasured",
			in: measurementStatusInput{
				Usage:        capFull,
				AgentPhase:   running,
				RunStartedAt: now.Add(-60 * time.Second),
				DBNow:        now,
			},
			want: MeasurementStatusUnmeasured,
		},
		{
			name: "row3c: historical window (WindowIncludesNow=false) past 60s grace -> unmeasured (not stale)",
			in: measurementStatusInput{
				Usage:             capFull,
				AgentPhase:        running,
				WindowIncludesNow: boolPtr(false),
				RunStartedAt:      now.Add(-5 * time.Minute),
				DBNow:             now,
			},
			want: MeasurementStatusUnmeasured,
		},
		{
			name: "row4: unknown capability (nil usage.yaml) running past 60s grace no receipt -> unmeasured (P1 compat)",
			in: measurementStatusInput{
				Usage:        nil,
				AgentPhase:   running,
				RunStartedAt: now.Add(-5 * time.Minute),
				DBNow:        now,
			},
			want: MeasurementStatusUnmeasured,
		},
		{
			name: "row5: stopped known live harness no receipt no events -> unmeasured",
			in: measurementStatusInput{
				Usage:        capFull,
				AgentPhase:   stopped,
				RunStartedAt: now.Add(-5 * time.Minute),
				DBNow:        now,
			},
			want: MeasurementStatusUnmeasured,
		},
		{
			name: "row6: P1 deferral (b) heartbeat-only fresh meter with full capability -> measured",
			in: measurementStatusInput{
				Usage:             capFull,
				AgentPhase:        running,
				MeterHealth:       freshOpen,
				HasRunUsageEvents: false,
				DBNow:             now,
			},
			want: MeasurementStatusMeasured,
		},
		{
			name: "row7: heartbeat-only fresh meter with partial capability -> partial",
			in: measurementStatusInput{
				Usage:             capPartial,
				AgentPhase:        running,
				MeterHealth:       freshOpen,
				HasRunUsageEvents: false,
				DBNow:             now,
			},
			want: MeasurementStatusPartial,
		},
		{
			name: "row8: heartbeat-only fresh meter with calls_only capability -> calls_only",
			in: measurementStatusInput{
				Usage:             capCallsOnly,
				AgentPhase:        running,
				MeterHealth:       freshOpen,
				HasRunUsageEvents: false,
				DBNow:             now,
			},
			want: MeasurementStatusCallsOnly,
		},
		{
			name: "row9: heartbeat-only fresh meter with unknown capability -> unmeasured (P1 compat)",
			in: measurementStatusInput{
				Usage:             nil,
				AgentPhase:        running,
				MeterHealth:       freshOpen,
				HasRunUsageEvents: false,
				DBNow:             now,
			},
			want: MeasurementStatusUnmeasured,
		},
		{
			name: "row10: final_state=incomplete with full capability -> partial",
			in: measurementStatusInput{
				Usage:             capFull,
				AgentPhase:        stopped,
				MeterHealth:       incompleteMH,
				HasRunUsageEvents: true,
				DBNow:             now,
			},
			want: MeasurementStatusPartial,
		},
		{
			name: "row10b: final_state=incomplete with calls_only capability -> calls_only",
			in: measurementStatusInput{
				Usage:             capCallsOnly,
				AgentPhase:        stopped,
				MeterHealth:       incompleteMH,
				HasRunUsageEvents: true,
				DBNow:             now,
			},
			want: MeasurementStatusCallsOnly,
		},
		{
			name: "drift: schema_drift_detected on latest meter downgrades full to calls_only",
			in: measurementStatusInput{
				Usage:             capFull,
				AgentPhase:        running,
				MeterHealth:       driftMH,
				HasRunUsageEvents: true,
				DBNow:             now,
			},
			want: MeasurementStatusCallsOnly,
		},
		{
			name: "drift-unknown: schema_drift_detected with unknown capability keeps prior measured behavior (O1)",
			in: measurementStatusInput{
				Usage:             nil,
				AgentPhase:        running,
				MeterHealth:       driftMH,
				HasRunUsageEvents: true,
				DBNow:             now,
			},
			want: MeasurementStatusMeasured,
		},
		{
			name: "row11: calls_only capability with events -> calls_only",
			in: measurementStatusInput{
				Usage:             capCallsOnly,
				AgentPhase:        running,
				MeterHealth:       freshOpen,
				HasRunUsageEvents: true,
				DBNow:             now,
			},
			want: MeasurementStatusCallsOnly,
		},
		{
			name: "row12: partial capability with events -> partial",
			in: measurementStatusInput{
				Usage:             capPartial,
				AgentPhase:        stopped,
				MeterHealth:       closedFinal,
				HasRunUsageEvents: true,
				DBNow:             now,
			},
			want: MeasurementStatusPartial,
		},
		{
			name: "row13: full capability with events and closed final meter -> measured",
			in: measurementStatusInput{
				Usage:             capFull,
				AgentPhase:        stopped,
				MeterHealth:       closedFinal,
				HasRunUsageEvents: true,
				DBNow:             now,
			},
			want: MeasurementStatusMeasured,
		},
		{
			name: "row13b: unknown capability with events -> measured (P1 compat)",
			in: measurementStatusInput{
				Usage:             nil,
				AgentPhase:        running,
				HasRunUsageEvents: true,
				DBNow:             now,
			},
			want: MeasurementStatusMeasured,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, deriveMeasurementStatus(tc.in))
		})
	}
}

func TestDeriveScopeCoverageSummary_AggregatePrecedenceAndMixedCapabilities(t *testing.T) {
	now := time.Date(2026, 11, 27, 12, 0, 0, 0, time.UTC)
	globalCaps := map[string]*api.HarnessUsageCapabilities{
		"claude":      makeUsageCap(api.UsageLiveSourceNative, []string{"input", "output", "cache_read", "cache_write"}, nil, true),
		"antigravity": makeUsageCap(api.UsageLiveSourceHooks, nil, nil, false),
		"hermes":      makeUsageCap(api.UsageLiveSourceNone, nil, nil, false),
		"partial-h":   makeUsageCap(api.UsageLiveSourceHooks, []string{"input", "output"}, nil, false),
	}
	projectCaps := map[harnessRefKey]*api.HarnessUsageCapabilities{
		{projectID: "proj-override", ref: "hermes"}: makeUsageCap(api.UsageLiveSourceHooks, []string{"input", "output", "cache_read", "cache_write"}, nil, false),
	}
	resolveHosted := func(projectID, harnessConfig string) *api.HarnessUsageCapabilities {
		if cap, ok := globalCaps[harnessConfig]; ok {
			return cap
		}
		if projectID != "" {
			if cap, ok := projectCaps[harnessRefKey{projectID: projectID, ref: harnessConfig}]; ok {
				return cap
			}
		}
		return nil
	}
	resolveWorkstation := func(projectID, harnessConfig string) *api.HarnessUsageCapabilities {
		if projectID != "" {
			if cap, ok := projectCaps[harnessRefKey{projectID: projectID, ref: harnessConfig}]; ok {
				return cap
			}
		}
		if cap, ok := globalCaps[harnessConfig]; ok {
			return cap
		}
		return nil
	}

	t.Run("stale wins over partial, measured, calls_only, unmeasured", func(t *testing.T) {
		cov := &store.UsageScopeCoverage{
			Buckets: []store.UsageCoverageBucket{
				{HarnessConfig: "claude", IsRunning: true, IsPastGrace: true, AgentCount: 1},                                  // stale (past grace, no receipt)
				{HarnessConfig: "partial-h", HasUsageData: true, AgentCount: 2},                                               // partial
				{HarnessConfig: "claude", HasMeterHealth: true, LatestFinalState: "open", HasUsageData: false, AgentCount: 3}, // measured (heartbeat-only)
				{HarnessConfig: "antigravity", HasUsageData: true, AgentCount: 1},                                             // calls_only
				{HarnessConfig: "hermes", IsRunning: true, IsPastGrace: true, AgentCount: 4},                                  // unmeasured (source:none)
			},
		}
		status, unmeasured, stale := deriveScopeCoverageSummary(cov, 10, false, true, now, true, resolveHosted)
		assert.Equal(t, MeasurementStatusStale, status)
		assert.Equal(t, 1, stale)
		assert.Equal(t, 4, unmeasured)
	})

	t.Run("mixed measured + calls_only with events reads partial", func(t *testing.T) {
		cov := &store.UsageScopeCoverage{
			Buckets: []store.UsageCoverageBucket{
				{HarnessConfig: "claude", HasUsageData: true, AgentCount: 1},
				{HarnessConfig: "antigravity", HasUsageData: true, AgentCount: 1},
			},
		}
		status, unmeasured, stale := deriveScopeCoverageSummary(cov, 5, false, true, now, true, resolveHosted)
		assert.Equal(t, MeasurementStatusPartial, status)
		assert.Equal(t, 0, unmeasured)
		assert.Equal(t, 0, stale)
	})

	t.Run("all calls_only with events reads calls_only (R2)", func(t *testing.T) {
		cov := &store.UsageScopeCoverage{
			Buckets: []store.UsageCoverageBucket{
				{HarnessConfig: "antigravity", HasUsageData: true, AgentCount: 2},
			},
		}
		status, unmeasured, stale := deriveScopeCoverageSummary(cov, 5, false, true, now, true, resolveHosted)
		assert.Equal(t, MeasurementStatusCallsOnly, status)
		assert.Equal(t, 0, unmeasured)
		assert.Equal(t, 0, stale)
	})

	t.Run("all unmeasured (source:none) with events reads unmeasured (R2)", func(t *testing.T) {
		cov := &store.UsageScopeCoverage{
			Buckets: []store.UsageCoverageBucket{
				{HarnessConfig: "hermes", HasUsageData: true, AgentCount: 2},
			},
		}
		status, unmeasured, stale := deriveScopeCoverageSummary(cov, 7, false, true, now, true, resolveHosted)
		assert.Equal(t, MeasurementStatusUnmeasured, status)
		assert.Equal(t, 2, unmeasured)
		assert.Equal(t, 0, stale)
	})

	t.Run("deleted-agent zero-bucket fallback distinguishes track1-only, mixed, and legacy-only (R1)", func(t *testing.T) {
		cov := &store.UsageScopeCoverage{}
		// Track-1-only rollups -> measured.
		status, unmeasured, stale := deriveScopeCoverageSummary(cov, 12, false, true, now, true, resolveHosted)
		assert.Equal(t, MeasurementStatusMeasured, status)
		assert.Equal(t, 0, unmeasured)
		assert.Equal(t, 0, stale)

		// Mixed legacy + Track-1 rollups -> partial.
		status, unmeasured, stale = deriveScopeCoverageSummary(cov, 12, true, true, now, true, resolveHosted)
		assert.Equal(t, MeasurementStatusPartial, status)
		assert.Equal(t, 0, unmeasured)
		assert.Equal(t, 0, stale)

		// Legacy-only rollups -> legacy_client (never measured).
		status, unmeasured, stale = deriveScopeCoverageSummary(cov, 12, true, false, now, true, resolveHosted)
		assert.Equal(t, MeasurementStatusLegacyClient, status)
		assert.Equal(t, 0, unmeasured)
		assert.Equal(t, 0, stale)
	})

	t.Run("workstation lookup uses project-scoped override over global", func(t *testing.T) {
		cov := &store.UsageScopeCoverage{
			Buckets: []store.UsageCoverageBucket{
				{
					HarnessConfig:       "hermes",
					CapabilityProjectID: "proj-override",
					HasMeterHealth:      true,
					LatestFinalState:    store.MeterHealthFinalStateOpen,
					HasUsageData:        false,
					AgentCount:          1,
				},
			},
		}
		status, unmeasured, stale := deriveScopeCoverageSummary(cov, 0, false, false, now, true, resolveWorkstation)
		assert.Equal(t, MeasurementStatusMeasured, status)
		assert.Equal(t, 0, unmeasured)
		assert.Equal(t, 0, stale)
	})

	t.Run("historical window (windowIncludesNow=false) suppresses past-grace staleness", func(t *testing.T) {
		cov := &store.UsageScopeCoverage{
			Buckets: []store.UsageCoverageBucket{
				{HarnessConfig: "claude", IsRunning: true, IsPastGrace: true, AgentCount: 1},
			},
		}
		status, unmeasured, stale := deriveScopeCoverageSummary(cov, 0, false, false, now, false, resolveHosted)
		assert.Equal(t, MeasurementStatusUnmeasured, status)
		assert.Equal(t, 1, unmeasured)
		assert.Equal(t, 0, stale)
	})

	t.Run("legacy_client per-agent and aggregate semantics (D13 / D13a)", func(t *testing.T) {
		// Per-agent: legacy-only with no meter_health stays legacy_client
		// across full, source:none, unknown, and running-past-grace cases.
		assert.Equal(t, MeasurementStatusLegacyClient, deriveMeasurementStatus(measurementStatusInput{
			Usage:              globalCaps["claude"],
			AgentPhase:         string(state.PhaseRunning),
			RunStartedAt:       now.Add(-5 * time.Minute),
			HasRunUsageEvents:  true,
			HasLegacyUsageOnly: true,
			DBNow:              now,
		}))
		assert.Equal(t, MeasurementStatusLegacyClient, deriveMeasurementStatus(measurementStatusInput{
			Usage:              globalCaps["hermes"],
			HasRunUsageEvents:  true,
			HasLegacyUsageOnly: true,
			DBNow:              now,
		}))
		// D13a: legacy-only with a fresh meter_health still reads legacy_client,
		// while a stale open meter still wins as stale.
		assert.Equal(t, MeasurementStatusLegacyClient, deriveMeasurementStatus(measurementStatusInput{
			Usage:              globalCaps["claude"],
			AgentPhase:         string(state.PhaseRunning),
			MeterHealth:        []*store.MeterHealth{{FinalState: store.MeterHealthFinalStateOpen, LastReceiptAt: now}},
			HasRunUsageEvents:  true,
			HasLegacyUsageOnly: true,
			DBNow:              now,
		}))
		assert.Equal(t, MeasurementStatusStale, deriveMeasurementStatus(measurementStatusInput{
			Usage:              globalCaps["claude"],
			AgentPhase:         string(state.PhaseRunning),
			MeterHealth:        []*store.MeterHealth{{FinalState: store.MeterHealthFinalStateOpen, LastReceiptAt: now.Add(-5 * time.Minute)}},
			HasRunUsageEvents:  true,
			HasLegacyUsageOnly: true,
			DBNow:              now,
		}))

		// Aggregate: all legacy_client (+ unmeasured), including one with a fresh meter (D13a) -> legacy_client.
		covLegacyOnly := &store.UsageScopeCoverage{
			Buckets: []store.UsageCoverageBucket{
				{HarnessConfig: "claude", HasUsageData: true, HasLegacyOnly: true, AgentCount: 1},
				{HarnessConfig: "claude", HasMeterHealth: true, LatestFinalState: store.MeterHealthFinalStateOpen, HasUsageData: true, HasLegacyOnly: true, AgentCount: 1},
				{HarnessConfig: "hermes", AgentCount: 1},
			},
		}
		status, unmeasured, stale := deriveScopeCoverageSummary(covLegacyOnly, 10, true, false, now, true, resolveHosted)
		assert.Equal(t, MeasurementStatusLegacyClient, status)
		assert.Equal(t, 1, unmeasured)
		assert.Equal(t, 0, stale)

		// Aggregate D13a: legacy-only with stale open meter -> stale wins.
		covLegacyStaleMeter := &store.UsageScopeCoverage{
			Buckets: []store.UsageCoverageBucket{
				{HarnessConfig: "claude", IsRunning: true, HasMeterHealth: true, LatestFinalState: store.MeterHealthFinalStateOpen, IsReceiptStale: true, HasUsageData: true, HasLegacyOnly: true, AgentCount: 1},
			},
		}
		status, _, stale = deriveScopeCoverageSummary(covLegacyStaleMeter, 10, true, false, now, true, resolveHosted)
		assert.Equal(t, MeasurementStatusStale, status)
		assert.Equal(t, 1, stale)

		// Aggregate: measured + legacy_client -> partial (never measured).
		covMixedMeasured := &store.UsageScopeCoverage{
			Buckets: []store.UsageCoverageBucket{
				{HarnessConfig: "claude", HasMeterHealth: true, LatestFinalState: store.MeterHealthFinalStateOpen, HasUsageData: true, AgentCount: 1},
				{HarnessConfig: "claude", HasUsageData: true, HasLegacyOnly: true, AgentCount: 1},
			},
		}
		status, unmeasured, stale = deriveScopeCoverageSummary(covMixedMeasured, 10, true, true, now, true, resolveHosted)
		assert.Equal(t, MeasurementStatusPartial, status)
		assert.Equal(t, 0, unmeasured)
		assert.Equal(t, 0, stale)

		// Aggregate: calls_only + legacy_client -> partial.
		covMixedCallsOnly := &store.UsageScopeCoverage{
			Buckets: []store.UsageCoverageBucket{
				{HarnessConfig: "antigravity", HasMeterHealth: true, LatestFinalState: store.MeterHealthFinalStateOpen, HasUsageData: true, AgentCount: 1},
				{HarnessConfig: "claude", HasUsageData: true, HasLegacyOnly: true, AgentCount: 1},
			},
		}
		status, _, _ = deriveScopeCoverageSummary(covMixedCallsOnly, 10, true, true, now, true, resolveHosted)
		assert.Equal(t, MeasurementStatusPartial, status)

		// Aggregate: stale + legacy_client -> stale.
		covStaleLegacy := &store.UsageScopeCoverage{
			Buckets: []store.UsageCoverageBucket{
				{HarnessConfig: "claude", IsRunning: true, IsPastGrace: true, AgentCount: 1},
				{HarnessConfig: "claude", HasUsageData: true, HasLegacyOnly: true, AgentCount: 1},
			},
		}
		status, _, stale = deriveScopeCoverageSummary(covStaleLegacy, 10, true, false, now, true, resolveHosted)
		assert.Equal(t, MeasurementStatusStale, status)
		assert.Equal(t, 1, stale)
	})
}

type usageCapErrorStubStore struct {
	store.Store
	globalErr      error
	projectCfg     *store.HarnessConfig
	projectQueried bool
}

func (s *usageCapErrorStubStore) GetHarnessConfigBySlug(_ context.Context, slug, scope, _ string) (*store.HarnessConfig, error) {
	if scope == store.HarnessConfigScopeGlobal {
		return nil, s.globalErr
	}
	s.projectQueried = true
	if s.projectCfg != nil && s.projectCfg.Slug == slug {
		return s.projectCfg, nil
	}
	return nil, store.ErrNotFound
}

func TestUsageCapabilityMemo_HostedGlobalStoreErrorDoesNotConsultProjectScope(t *testing.T) {
	stub := &usageCapErrorStubStore{
		globalErr: errors.New("db temporarily unavailable"),
		projectCfg: &store.HarnessConfig{
			Slug:    "err-cfg",
			Harness: "antigravity",
			Scope:   store.HarnessConfigScopeProject,
			ScopeID: "proj-1",
			Config: &store.HarnessConfigData{
				Harness: "antigravity",
				Usage:   makeUsageCap(api.UsageLiveSourceHooks, nil, nil, false),
			},
		},
	}
	srv := &Server{store: stub, workstation: false}
	memo := newUsageCapabilityMemo(context.Background(), srv)

	got := memo.resolve("proj-1", "err-cfg")
	assert.Nil(t, got, "hosted global store error must yield unknown capability (nil)")
	assert.False(t, stub.projectQueried, "hosted hub must never consult project scope after non-NotFound global store error")
}

type scopeWalkQuery struct {
	slug    string
	scope   string
	scopeID string
}

type harnessConfigScopeWalkStubStore struct {
	store.Store
	configs map[string]*store.HarnessConfig
	errs    map[string]error
	queries []scopeWalkQuery
}

func scopeWalkConfigKey(scope, scopeID, slug string) string {
	return scope + ":" + scopeID + ":" + slug
}

func scopeWalkErrKey(scope, scopeID string) string {
	return scope + ":" + scopeID
}

func (s *harnessConfigScopeWalkStubStore) GetHarnessConfigBySlug(_ context.Context, slug, scope, scopeID string) (*store.HarnessConfig, error) {
	s.queries = append(s.queries, scopeWalkQuery{slug: slug, scope: scope, scopeID: scopeID})
	if err, ok := s.errs[scopeWalkErrKey(scope, scopeID)]; ok {
		return nil, err
	}
	if hc, ok := s.configs[scopeWalkConfigKey(scope, scopeID, slug)]; ok {
		return hc, nil
	}
	return nil, store.ErrNotFound
}

func TestResolveHarnessConfigBySlug_DispatchAndUsageMemoParity(t *testing.T) {
	const (
		projectID = "proj-1"
		userID    = "user-1"
		slug      = "shared-cfg"
	)
	globalHC := &store.HarnessConfig{ID: "hc-global", Slug: slug, Scope: store.HarnessConfigScopeGlobal, Status: store.HarnessConfigStatusActive}
	projectHC := &store.HarnessConfig{ID: "hc-project", Slug: slug, Scope: store.HarnessConfigScopeProject, ScopeID: projectID, Status: store.HarnessConfigStatusActive}
	userHC := &store.HarnessConfig{ID: "hc-user", Slug: slug, Scope: store.HarnessConfigScopeUser, ScopeID: userID, Status: store.HarnessConfigStatusActive}

	errGlobal := errors.New("global store unavailable")
	errProject := errors.New("project store unavailable")

	qGlobal := scopeWalkQuery{slug: slug, scope: store.HarnessConfigScopeGlobal, scopeID: ""}
	qProject := scopeWalkQuery{slug: slug, scope: store.HarnessConfigScopeProject, scopeID: projectID}

	cases := []struct {
		name        string
		workstation bool
		projectID   string
		slug        string
		configs     map[string]*store.HarnessConfig
		errs        map[string]error
		wantID      string
		wantErr     error
		wantQueries []scopeWalkQuery
	}{
		{
			name:        "hosted/project-only",
			workstation: false,
			projectID:   projectID,
			slug:        slug,
			configs: map[string]*store.HarnessConfig{
				scopeWalkConfigKey(store.HarnessConfigScopeProject, projectID, slug): projectHC,
			},
			wantID:      projectHC.ID,
			wantQueries: []scopeWalkQuery{qGlobal, qProject},
		},
		{
			name:        "workstation/project-only",
			workstation: true,
			projectID:   projectID,
			slug:        slug,
			configs: map[string]*store.HarnessConfig{
				scopeWalkConfigKey(store.HarnessConfigScopeProject, projectID, slug): projectHC,
			},
			wantID:      projectHC.ID,
			wantQueries: []scopeWalkQuery{qProject},
		},
		{
			name:        "hosted/global-only",
			workstation: false,
			projectID:   projectID,
			slug:        slug,
			configs: map[string]*store.HarnessConfig{
				scopeWalkConfigKey(store.HarnessConfigScopeGlobal, "", slug): globalHC,
			},
			wantID:      globalHC.ID,
			wantQueries: []scopeWalkQuery{qGlobal},
		},
		{
			name:        "workstation/global-fallback",
			workstation: true,
			projectID:   projectID,
			slug:        slug,
			configs: map[string]*store.HarnessConfig{
				scopeWalkConfigKey(store.HarnessConfigScopeGlobal, "", slug): globalHC,
			},
			wantID:      globalHC.ID,
			wantQueries: []scopeWalkQuery{qProject, qGlobal},
		},
		{
			name:        "hosted/global-wins-over-project",
			workstation: false,
			projectID:   projectID,
			slug:        slug,
			configs: map[string]*store.HarnessConfig{
				scopeWalkConfigKey(store.HarnessConfigScopeGlobal, "", slug):         globalHC,
				scopeWalkConfigKey(store.HarnessConfigScopeProject, projectID, slug): projectHC,
			},
			wantID:      globalHC.ID,
			wantQueries: []scopeWalkQuery{qGlobal},
		},
		{
			name:        "workstation/project-wins-over-global",
			workstation: true,
			projectID:   projectID,
			slug:        slug,
			configs: map[string]*store.HarnessConfig{
				scopeWalkConfigKey(store.HarnessConfigScopeGlobal, "", slug):         globalHC,
				scopeWalkConfigKey(store.HarnessConfigScopeProject, projectID, slug): projectHC,
			},
			wantID:      projectHC.ID,
			wantQueries: []scopeWalkQuery{qProject},
		},
		{
			name:        "hosted/user-scope-ignored",
			workstation: false,
			projectID:   projectID,
			slug:        slug,
			configs: map[string]*store.HarnessConfig{
				scopeWalkConfigKey(store.HarnessConfigScopeUser, userID, slug): userHC,
			},
			wantID:      "",
			wantQueries: []scopeWalkQuery{qGlobal, qProject},
		},
		{
			name:        "workstation/user-scope-ignored",
			workstation: true,
			projectID:   projectID,
			slug:        slug,
			configs: map[string]*store.HarnessConfig{
				scopeWalkConfigKey(store.HarnessConfigScopeUser, userID, slug): userHC,
			},
			wantID:      "",
			wantQueries: []scopeWalkQuery{qProject, qGlobal},
		},
		{
			name:        "hosted/user-ignored-resolves-global",
			workstation: false,
			projectID:   projectID,
			slug:        slug,
			configs: map[string]*store.HarnessConfig{
				scopeWalkConfigKey(store.HarnessConfigScopeUser, userID, slug): userHC,
				scopeWalkConfigKey(store.HarnessConfigScopeGlobal, "", slug):   globalHC,
			},
			wantID:      globalHC.ID,
			wantQueries: []scopeWalkQuery{qGlobal},
		},
		{
			name:        "workstation/user-ignored-resolves-project",
			workstation: true,
			projectID:   projectID,
			slug:        slug,
			configs: map[string]*store.HarnessConfig{
				scopeWalkConfigKey(store.HarnessConfigScopeUser, userID, slug):       userHC,
				scopeWalkConfigKey(store.HarnessConfigScopeProject, projectID, slug): projectHC,
			},
			wantID:      projectHC.ID,
			wantQueries: []scopeWalkQuery{qProject},
		},
		{
			name:        "hosted/empty-project-id-global-fallback",
			workstation: false,
			projectID:   "",
			slug:        slug,
			configs: map[string]*store.HarnessConfig{
				scopeWalkConfigKey(store.HarnessConfigScopeProject, projectID, slug): projectHC,
				scopeWalkConfigKey(store.HarnessConfigScopeGlobal, "", slug):         globalHC,
			},
			wantID:      globalHC.ID,
			wantQueries: []scopeWalkQuery{qGlobal},
		},
		{
			name:        "workstation/empty-project-id-global-fallback",
			workstation: true,
			projectID:   "",
			slug:        slug,
			configs: map[string]*store.HarnessConfig{
				scopeWalkConfigKey(store.HarnessConfigScopeProject, projectID, slug): projectHC,
				scopeWalkConfigKey(store.HarnessConfigScopeGlobal, "", slug):         globalHC,
			},
			wantID:      globalHC.ID,
			wantQueries: []scopeWalkQuery{qGlobal},
		},
		{
			name:        "hosted/global-error-fails-closed",
			workstation: false,
			projectID:   projectID,
			slug:        slug,
			configs: map[string]*store.HarnessConfig{
				scopeWalkConfigKey(store.HarnessConfigScopeProject, projectID, slug): projectHC,
			},
			errs: map[string]error{
				scopeWalkErrKey(store.HarnessConfigScopeGlobal, ""): errGlobal,
			},
			wantErr:     errGlobal,
			wantQueries: []scopeWalkQuery{qGlobal},
		},
		{
			name:        "workstation/project-error-falls-back-to-global",
			workstation: true,
			projectID:   projectID,
			slug:        slug,
			configs: map[string]*store.HarnessConfig{
				scopeWalkConfigKey(store.HarnessConfigScopeGlobal, "", slug): globalHC,
			},
			errs: map[string]error{
				scopeWalkErrKey(store.HarnessConfigScopeProject, projectID): errProject,
			},
			wantID:      globalHC.ID,
			wantQueries: []scopeWalkQuery{qProject, qGlobal},
		},
		{
			name:        "workstation/project-error-retains-err-on-global-not-found",
			workstation: true,
			projectID:   projectID,
			slug:        slug,
			errs: map[string]error{
				scopeWalkErrKey(store.HarnessConfigScopeProject, projectID): errProject,
			},
			wantErr:     errProject,
			wantQueries: []scopeWalkQuery{qProject, qGlobal},
		},
		{
			name:        "hosted/global-not-found-project-error-returns-err",
			workstation: false,
			projectID:   projectID,
			slug:        slug,
			errs: map[string]error{
				scopeWalkErrKey(store.HarnessConfigScopeProject, projectID): errProject,
			},
			wantErr:     errProject,
			wantQueries: []scopeWalkQuery{qGlobal, qProject},
		},
		{
			name:        "hosted/empty-slug-no-queries",
			workstation: false,
			projectID:   projectID,
			slug:        "",
			configs: map[string]*store.HarnessConfig{
				scopeWalkConfigKey(store.HarnessConfigScopeGlobal, "", slug): globalHC,
			},
			wantID:      "",
			wantQueries: nil,
		},
		{
			name:        "workstation/empty-slug-no-queries",
			workstation: true,
			projectID:   projectID,
			slug:        "",
			configs: map[string]*store.HarnessConfig{
				scopeWalkConfigKey(store.HarnessConfigScopeProject, projectID, slug): projectHC,
			},
			wantID:      "",
			wantQueries: nil,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()

			dispatchStub := &harnessConfigScopeWalkStubStore{configs: tc.configs, errs: tc.errs}
			dispatchSrv := &Server{store: dispatchStub, workstation: tc.workstation}
			dispatchHC, dispatchErr := dispatchSrv.resolveHarnessConfigBySlug(ctx, tc.projectID, tc.slug)

			memoStub := &harnessConfigScopeWalkStubStore{configs: tc.configs, errs: tc.errs}
			memoSrv := &Server{store: memoStub, workstation: tc.workstation}
			memoHC, memoErr := newUsageCapabilityMemo(ctx, memoSrv).resolveConfigBySlug(tc.projectID, tc.slug)

			if tc.wantErr != nil {
				assert.ErrorIs(t, dispatchErr, tc.wantErr)
				assert.ErrorIs(t, memoErr, tc.wantErr)
			} else {
				assert.NoError(t, dispatchErr)
				assert.NoError(t, memoErr)
			}

			if tc.wantID == "" {
				assert.Nil(t, dispatchHC)
				assert.Nil(t, memoHC)
			} else {
				if assert.NotNil(t, dispatchHC) {
					assert.Equal(t, tc.wantID, dispatchHC.ID)
				}
				if assert.NotNil(t, memoHC) {
					assert.Equal(t, tc.wantID, memoHC.ID)
				}
			}

			assert.Equal(t, tc.wantQueries, dispatchStub.queries, "dispatch scope walk queries")
			assert.Equal(t, tc.wantQueries, memoStub.queries, "usageCapabilityMemo scope walk queries")
			assert.Equal(t, dispatchStub.queries, memoStub.queries, "dispatch and usageCapabilityMemo must walk identical scopes")
		})
	}
}
