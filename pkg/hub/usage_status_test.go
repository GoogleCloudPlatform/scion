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
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
)

func boolPtr(v bool) *bool { return &v }

func TestClassifyUsageLiveCapability(t *testing.T) {
	tests := []struct {
		name string
		cap  *usageLiveCapability
		want usageCapabilityKind
	}{
		{
			name: "nil capability is unknown",
			cap:  nil,
			want: usageCapUnknown,
		},
		{
			name: "empty source is unknown",
			cap:  &usageLiveCapability{},
			want: usageCapUnknown,
		},
		{
			name: "source none is unmeasured",
			cap:  &usageLiveCapability{Source: "none"},
			want: usageCapUnmeasured,
		},
		{
			name: "hooks with no token types is calls_only (antigravity)",
			cap:  &usageLiveCapability{Source: "hooks"},
			want: usageCapCallsOnly,
		},
		{
			name: "claude all four summable types is full",
			cap: &usageLiveCapability{
				Source:            "native",
				TokenTypes:        []string{"input", "output", "cache_read", "cache_write"},
				CountsFailedCalls: true,
			},
			want: usageCapFull,
		},
		{
			name: "gemini-cli with cache_write not_applicable is full",
			cap: &usageLiveCapability{
				Source:                  "native",
				TokenTypes:              []string{"input", "output", "cache_read", "reasoning"},
				NotApplicableTokenTypes: []string{"cache_write"},
				CountsFailedCalls:       true,
			},
			want: usageCapFull,
		},
		{
			name: "copilot counts_failed_calls=false stays full",
			cap: &usageLiveCapability{
				Source:            "native",
				TokenTypes:        []string{"input", "output", "cache_read", "cache_write", "reasoning"},
				CountsFailedCalls: false,
			},
			want: usageCapFull,
		},
		{
			name: "missing cache_read without not_applicable is partial",
			cap: &usageLiveCapability{
				Source:     "hooks",
				TokenTypes: []string{"input", "output", "cache_write"},
			},
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

	capNone := &usageLiveCapability{Source: "none"}
	capCallsOnly := &usageLiveCapability{Source: "hooks"}
	capPartial := &usageLiveCapability{Source: "hooks", TokenTypes: []string{"input", "output"}}
	capFull := &usageLiveCapability{
		Source:                  "native",
		TokenTypes:              []string{"input", "output", "cache_read"},
		NotApplicableTokenTypes: []string{"cache_write"},
	}

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
				UsageLive:    capNone,
				AgentPhase:   running,
				RunStartedAt: now.Add(-5 * time.Minute),
				DBNow:        now,
			},
			want: MeasurementStatusUnmeasured,
		},
		{
			name: "row1b: source=none even with stale open meter -> unmeasured",
			in: measurementStatusInput{
				UsageLive:   capNone,
				AgentPhase:  running,
				MeterHealth: staleOpen,
				DBNow:       now,
			},
			want: MeasurementStatusUnmeasured,
		},
		{
			name: "row2: running open meter >180s -> stale",
			in: measurementStatusInput{
				UsageLive:         capFull,
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
				UsageLive:         capFull,
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
				UsageLive:         capFull,
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
				UsageLive:         capFull,
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
				UsageLive:    capFull,
				AgentPhase:   running,
				RunStartedAt: now.Add(-61 * time.Second),
				DBNow:        now,
			},
			want: MeasurementStatusStale,
		},
		{
			name: "row3b: running known live harness within 60s grace boundary (==60s) -> unmeasured",
			in: measurementStatusInput{
				UsageLive:    capFull,
				AgentPhase:   running,
				RunStartedAt: now.Add(-60 * time.Second),
				DBNow:        now,
			},
			want: MeasurementStatusUnmeasured,
		},
		{
			name: "row3c: historical window (WindowIncludesNow=false) past 60s grace -> unmeasured (not stale)",
			in: measurementStatusInput{
				UsageLive:         capFull,
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
				UsageLive:    nil,
				AgentPhase:   running,
				RunStartedAt: now.Add(-5 * time.Minute),
				DBNow:        now,
			},
			want: MeasurementStatusUnmeasured,
		},
		{
			name: "row5: stopped known live harness no receipt no events -> unmeasured",
			in: measurementStatusInput{
				UsageLive:    capFull,
				AgentPhase:   stopped,
				RunStartedAt: now.Add(-5 * time.Minute),
				DBNow:        now,
			},
			want: MeasurementStatusUnmeasured,
		},
		{
			name: "row6: P1 deferral (b) heartbeat-only fresh meter with full capability -> measured",
			in: measurementStatusInput{
				UsageLive:         capFull,
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
				UsageLive:         capPartial,
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
				UsageLive:         capCallsOnly,
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
				UsageLive:         nil,
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
				UsageLive:         capFull,
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
				UsageLive:         capCallsOnly,
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
				UsageLive:         capFull,
				AgentPhase:        running,
				MeterHealth:       driftMH,
				HasRunUsageEvents: true,
				DBNow:             now,
			},
			want: MeasurementStatusCallsOnly,
		},
		{
			name: "row11: calls_only capability with events -> calls_only",
			in: measurementStatusInput{
				UsageLive:         capCallsOnly,
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
				UsageLive:         capPartial,
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
				UsageLive:         capFull,
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
				UsageLive:         nil,
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
	resolver := &usageCapabilityResolver{
		global: map[string]*usageLiveCapability{
			"claude": {
				Source:     "native",
				TokenTypes: []string{"input", "output", "cache_read", "cache_write"},
			},
			"antigravity": {
				Source: "hooks",
			},
			"hermes": {
				Source: "none",
			},
			"partial-h": {
				Source:     "hooks",
				TokenTypes: []string{"input", "output"},
			},
		},
		project: map[harnessRefKey]*usageLiveCapability{
			{projectID: "proj-override", ref: "hermes"}: {
				Source:     "hooks",
				TokenTypes: []string{"input", "output", "cache_read", "cache_write"},
			},
		},
	}

	t.Run("stale wins over partial, measured, calls_only, unmeasured", func(t *testing.T) {
		cov := &store.UsageScopeCoverage{
			Buckets: []store.UsageCoverageBucket{
				{HarnessConfig: "claude", IsRunning: true, IsPastGrace: true, AgentCount: 1},                              // stale (past grace, no receipt)
				{HarnessConfig: "partial-h", HasUsageData: true, AgentCount: 2},                                           // partial
				{HarnessConfig: "claude", HasMeterHealth: true, LatestFinalState: "open", HasUsageData: false, AgentCount: 3}, // measured (heartbeat-only)
				{HarnessConfig: "antigravity", HasUsageData: true, AgentCount: 1},                                         // calls_only
				{HarnessConfig: "hermes", IsRunning: true, IsPastGrace: true, AgentCount: 4},                              // unmeasured (source:none)
			},
		}
		status, unmeasured, stale := deriveScopeCoverageSummary(cov, 10, now, true, resolver)
		assert.Equal(t, MeasurementStatusStale, status)
		assert.Equal(t, 1, stale)
		assert.Equal(t, 4, unmeasured)
	})

	t.Run("mixed measured + calls_only reads partial", func(t *testing.T) {
		cov := &store.UsageScopeCoverage{
			Buckets: []store.UsageCoverageBucket{
				{HarnessConfig: "claude", HasUsageData: true, AgentCount: 1},
				{HarnessConfig: "antigravity", HasUsageData: true, AgentCount: 1},
			},
		}
		status, unmeasured, stale := deriveScopeCoverageSummary(cov, 5, now, true, resolver)
		assert.Equal(t, MeasurementStatusPartial, status)
		assert.Equal(t, 0, unmeasured)
		assert.Equal(t, 0, stale)
	})

	t.Run("all calls_only reads calls_only", func(t *testing.T) {
		cov := &store.UsageScopeCoverage{
			Buckets: []store.UsageCoverageBucket{
				{HarnessConfig: "antigravity", HasUsageData: true, AgentCount: 2},
			},
		}
		status, unmeasured, stale := deriveScopeCoverageSummary(cov, 0, now, true, resolver)
		assert.Equal(t, MeasurementStatusCallsOnly, status)
		assert.Equal(t, 0, unmeasured)
		assert.Equal(t, 0, stale)
	})

	t.Run("project-scoped harness config override takes precedence over global", func(t *testing.T) {
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
		status, unmeasured, stale := deriveScopeCoverageSummary(cov, 0, now, true, resolver)
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
		status, unmeasured, stale := deriveScopeCoverageSummary(cov, 0, now, false, resolver)
		assert.Equal(t, MeasurementStatusUnmeasured, status)
		assert.Equal(t, 1, unmeasured)
		assert.Equal(t, 0, stale)
	})
}
