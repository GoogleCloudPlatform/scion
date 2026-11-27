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
		status, unmeasured, stale := deriveScopeCoverageSummary(cov, 10, now, true, resolveHosted)
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
		status, unmeasured, stale := deriveScopeCoverageSummary(cov, 5, now, true, resolveHosted)
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
		status, unmeasured, stale := deriveScopeCoverageSummary(cov, 5, now, true, resolveHosted)
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
		status, unmeasured, stale := deriveScopeCoverageSummary(cov, 7, now, true, resolveHosted)
		assert.Equal(t, MeasurementStatusUnmeasured, status)
		assert.Equal(t, 2, unmeasured)
		assert.Equal(t, 0, stale)
	})

	t.Run("deleted-agent historical events fallback reads measured when zero agent buckets exist", func(t *testing.T) {
		cov := &store.UsageScopeCoverage{}
		status, unmeasured, stale := deriveScopeCoverageSummary(cov, 12, now, true, resolveHosted)
		assert.Equal(t, MeasurementStatusMeasured, status)
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
		status, unmeasured, stale := deriveScopeCoverageSummary(cov, 0, now, true, resolveWorkstation)
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
		status, unmeasured, stale := deriveScopeCoverageSummary(cov, 0, now, false, resolveHosted)
		assert.Equal(t, MeasurementStatusUnmeasured, status)
		assert.Equal(t, 1, unmeasured)
		assert.Equal(t, 0, stale)
	})

	t.Run("legacy_client per-agent and aggregate semantics", func(t *testing.T) {
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
		// With Track 1 meter_health present, Track 1 derivation wins (not legacy_client).
		assert.Equal(t, MeasurementStatusMeasured, deriveMeasurementStatus(measurementStatusInput{
			Usage:              globalCaps["claude"],
			MeterHealth:        []*store.MeterHealth{{FinalState: store.MeterHealthFinalStateOpen, LastReceiptAt: now}},
			HasRunUsageEvents:  true,
			HasLegacyUsageOnly: true,
			DBNow:              now,
		}))

		// Aggregate: all legacy_client (+ unmeasured) -> legacy_client (never measured).
		covLegacyOnly := &store.UsageScopeCoverage{
			Buckets: []store.UsageCoverageBucket{
				{HarnessConfig: "claude", HasUsageData: true, HasLegacyOnly: true, AgentCount: 2},
				{HarnessConfig: "hermes", AgentCount: 1},
			},
		}
		status, unmeasured, stale := deriveScopeCoverageSummary(covLegacyOnly, 10, now, true, resolveHosted)
		assert.Equal(t, MeasurementStatusLegacyClient, status)
		assert.Equal(t, 1, unmeasured)
		assert.Equal(t, 0, stale)

		// Aggregate: measured + legacy_client -> partial (never measured).
		covMixedMeasured := &store.UsageScopeCoverage{
			Buckets: []store.UsageCoverageBucket{
				{HarnessConfig: "claude", HasMeterHealth: true, LatestFinalState: store.MeterHealthFinalStateOpen, HasUsageData: true, AgentCount: 1},
				{HarnessConfig: "claude", HasUsageData: true, HasLegacyOnly: true, AgentCount: 1},
			},
		}
		status, unmeasured, stale = deriveScopeCoverageSummary(covMixedMeasured, 10, now, true, resolveHosted)
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
		status, _, _ = deriveScopeCoverageSummary(covMixedCallsOnly, 10, now, true, resolveHosted)
		assert.Equal(t, MeasurementStatusPartial, status)

		// Aggregate: stale + legacy_client -> stale.
		covStaleLegacy := &store.UsageScopeCoverage{
			Buckets: []store.UsageCoverageBucket{
				{HarnessConfig: "claude", IsRunning: true, IsPastGrace: true, AgentCount: 1},
				{HarnessConfig: "claude", HasUsageData: true, HasLegacyOnly: true, AgentCount: 1},
			},
		}
		status, _, stale = deriveScopeCoverageSummary(covStaleLegacy, 10, now, true, resolveHosted)
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
