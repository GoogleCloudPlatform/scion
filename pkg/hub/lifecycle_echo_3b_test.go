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

//go:build !no_sqlite

package hub

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	tc "github.com/GoogleCloudPlatform/scion/pkg/telemetrycontract"
	"github.com/GoogleCloudPlatform/scion/pkg/telemetryemit"
)

// WP1 slice 3b (ptone/scion#5927), echo side: a completed launch report
// settled by SettleLaunchedRun lets slice 3's echo apply; a settle the
// fence refuses leaves the echo refused with slice 3's label (design v9
// sections 5, 7).

// assertEchoForRunB checks the settled outcome: the row on run-b, the echo
// recorded for run-b, exactly one configured carrying it and the adoption
// provenance (run-m, cas), and no stale echo.
func assertEchoForRunB(t *testing.T, f *echoReportFixture) {
	t.Helper()
	row := mustGetAgent(t, f.raw, f.agent.ID)
	assert.Equal(t, "run-b", row.RunID)
	require.NotNil(t, row.EffectiveConfig)
	assert.Equal(t, "run-b", row.EffectiveConfig.RunID)
	evs := f.events(t)
	echoed := configuredWithEcho(evs)
	require.Len(t, echoed, 1, lcSeqFull(evs))
	assert.Equal(t, "run-b", echoed[0].LaunchID)
	assert.Equal(t, "run-m", echoed[0].AdoptedFromLaunchID)
	assert.Equal(t, store.AdoptionSourceCAS, echoed[0].AdoptionSource)
}

// TestLifecycleEvents_3bSettleRecoversAfterStoreError: the completed
// settle fails with a store error, so the echo is unsettled_adoption; the
// broker's resend settles and the echo applies.
func TestLifecycleEvents_3bSettleRecoversAfterStoreError(t *testing.T) {
	f := newEchoReportFixture(t, "lc-3b-err")
	f.runningObserved(t)
	f.wrap.settleMode = "error"
	assert.Equal(t, store.LaunchReportResultCompleted, f.report(t, "run-b", false))
	assert.Equal(t, "run-m", mustGetAgent(t, f.raw, f.agent.ID).RunID)
	assert.Equal(t, int64(1), staleEchoes(f.counters, telemetryemit.StaleEchoPathLaunchReport, store.StaleEchoUnsettledAdoption), f.counters.String())

	f.wrap.mu.Lock()
	f.wrap.settleMode = ""
	f.wrap.mu.Unlock()
	assert.Equal(t, store.LaunchReportResultCompleted, f.report(t, "run-b", false))
	assertEchoForRunB(t, f)
	assert.Equal(t, int64(1), staleEchoes(f.counters, telemetryemit.StaleEchoPathLaunchReport, store.StaleEchoUnsettledAdoption), "no second stale echo")
}

// TestLifecycleEvents_3bSettleRecoversAfterCrash: the hub committed the
// succeeded report and stopped before the settle and the echo; the resend
// is answered completed, settles, and records the echo. A further resend
// adds nothing.
func TestLifecycleEvents_3bSettleRecoversAfterCrash(t *testing.T) {
	f := newEchoReportFixture(t, "lc-3b-crash")
	info := &RemoteAgentInfo{ID: f.agent.Slug, Slug: f.agent.Slug, RunID: "run-b", Phase: string(state.PhaseRunning)}
	answer, _, err := f.raw.ApplyLaunchReport(context.Background(), f.agent.ID, f.brokerID, store.LaunchReport{
		LaunchID: f.launchID, InstanceID: "i1", Seq: 1, State: store.LaunchReportStateSucceeded,
		Phase: string(state.PhaseRunning), AgentConfig: brokerAgentConfigOf(info),
	})
	require.NoError(t, err)
	require.Equal(t, store.LaunchReportResultApplied, answer.Result)
	require.Equal(t, "run-m", mustGetAgent(t, f.raw, f.agent.ID).RunID, "nothing settled before the crash")

	assert.Equal(t, store.LaunchReportResultCompleted, f.report(t, "run-b", false))
	assertEchoForRunB(t, f)
	assertNoStaleEcho(t, f.counters)

	n := len(f.events(t))
	assert.Equal(t, store.LaunchReportResultCompleted, f.report(t, "run-b", false))
	assert.Len(t, f.events(t), n, "a resend after the settle writes nothing")
	assert.Equal(t, "run-b", mustGetAgent(t, f.raw, f.agent.ID).RunID)
	assertNoStaleEcho(t, f.counters)
}

// TestLifecycleEvents_3bSettleInterleavings: a write between the handler's
// check and the swap refuses the settle atomically. Nothing is adopted, no
// configured is emitted, and the refused echo carries slice 3's label.
func TestLifecycleEvents_3bSettleInterleavings(t *testing.T) {
	for _, tt := range []struct {
		name    string
		write   func(t *testing.T, f *echoReportFixture, ctx context.Context, agentID string)
		wantRun string
		reason  string
	}{
		{"a newer run is minted", func(t *testing.T, f *echoReportFixture, ctx context.Context, agentID string) {
			_, err := f.raw.SetAgentRunID(ctx, agentID, "run-3", nil)
			require.NoError(t, err)
		}, "run-3", store.StaleEchoRunMismatch},
		{"a new launch begins", func(t *testing.T, f *echoReportFixture, ctx context.Context, agentID string) {
			_, err := f.raw.BeginStartLaunch(ctx, agentID, store.LaunchKindRestart, 5*time.Minute, store.BeginLaunchOptions{RunID: "run-m"})
			require.NoError(t, err)
		}, "run-m", store.StaleEchoRunMismatch},
		{"the agent is reassigned to another broker", func(t *testing.T, f *echoReportFixture, ctx context.Context, agentID string) {
			other := tid("broker-lc-3b-other")
			require.NoError(t, f.raw.CreateRuntimeBroker(ctx, &store.RuntimeBroker{
				ID: other, Name: "lc-3b-other", Slug: "lc-3b-other", Endpoint: "http://localhost:9801", Status: store.BrokerStatusOnline,
			}))
			n, err := f.raw.ReassignAgentsToBroker(ctx, []*store.Agent{mustGetAgent(t, f.raw, agentID)}, other)
			require.NoError(t, err)
			require.Equal(t, 1, n)
		}, "run-m", store.StaleEchoBrokerMismatch},
		{"the generation is bumped", func(t *testing.T, f *echoReportFixture, ctx context.Context, agentID string) {
			a := mustGetAgent(t, f.raw, agentID)
			a.Generation++
			require.NoError(t, f.raw.UpdateAgent(ctx, a))
		}, "run-m", store.StaleEchoUnsettledAdoption},
	} {
		t.Run(tt.name, func(t *testing.T) {
			f := newEchoReportFixture(t, "lc-3b-race")
			f.runningObserved(t)
			f.wrap.beforeSettle = func(ctx context.Context, agentID string) { tt.write(t, f, ctx, agentID) }
			assert.Equal(t, store.LaunchReportResultCompleted, f.report(t, "run-b", false))
			assert.Equal(t, tt.wantRun, mustGetAgent(t, f.raw, f.agent.ID).RunID, "the settle is refused")
			assert.Equal(t, int64(1), staleEchoes(f.counters, telemetryemit.StaleEchoPathLaunchReport, tt.reason), f.counters.String())
			evs := f.events(t)
			assert.Zero(t, countOf(evs, tc.TransitionConfigured), lcSeqFull(evs))
			assertNoEffective(t, evs)
		})
	}
}

// TestLifecycleEvents_3bSettleRefused: completed reports that must not
// settle; the echo is refused with slice 3's label.
func TestLifecycleEvents_3bSettleRefused(t *testing.T) {
	t.Run("delete holds the row", func(t *testing.T) {
		f := newEchoReportFixture(t, "lc-3b-held")
		f.runningObserved(t)
		claimForTest(t, f.raw, f.agent.ID, store.DeletionStateDeleting, time.Minute)
		f.report(t, "run-b", false)
		assert.Equal(t, "run-m", mustGetAgent(t, f.raw, f.agent.ID).RunID)
		assert.Equal(t, int64(1), staleEchoes(f.counters, telemetryemit.StaleEchoPathLaunchReport, store.StaleEchoDeletedOrHeld), f.counters.String())
		assert.Zero(t, countOf(f.events(t), tc.TransitionConfigured))
	})
	t.Run("unbound launch (older binary)", func(t *testing.T) {
		f := newEchoReportFixture(t, "lc-3b-unbound")
		// An older replica's BeginLaunch binds nothing.
		launchID, err := f.raw.BeginLaunch(context.Background(), f.agent.ID, store.LaunchKindCreate, 5*time.Minute)
		require.NoError(t, err)
		f.launchID = launchID
		require.Empty(t, mustGetAgent(t, f.raw, f.agent.ID).LaunchRunID)
		f.runningObserved(t)
		assert.Equal(t, store.LaunchReportResultCompleted, f.report(t, "run-b", false))
		assert.Equal(t, "run-m", mustGetAgent(t, f.raw, f.agent.ID).RunID, "an unknown binding is never settled on completed")
		assert.Equal(t, int64(1), staleEchoes(f.counters, telemetryemit.StaleEchoPathLaunchReport, store.StaleEchoRunMismatch), f.counters.String())
		assert.Zero(t, f.unsettledLines())
		assert.Zero(t, countOf(f.events(t), tc.TransitionConfigured))
	})
}

// TestLifecycleEvents_3bAlreadyRunningStartSettlesWithoutEcho: a start that
// found the agent already running reports the kept entry's run and, since
// slice 3's r1 R1 fix (556965b), no effective-only field. Slice 3b still
// settles the run (applied, and completed after running was observed);
// there is no echo to write, so nothing is configured from it and nothing
// is counted stale, and the next event naming run-b carries the adoption
// (run-m, cas).
func TestLifecycleEvents_3bAlreadyRunningStartSettlesWithoutEcho(t *testing.T) {
	for _, tt := range []struct {
		name   string
		before func(t *testing.T, f *echoReportFixture)
		result string
	}{
		{"applied", func(*testing.T, *echoReportFixture) {}, store.LaunchReportResultApplied},
		{"completed after running was observed", func(t *testing.T, f *echoReportFixture) { f.runningObserved(t) }, store.LaunchReportResultCompleted},
	} {
		t.Run(tt.name, func(t *testing.T) {
			f := newEchoReportFixture(t, "lc-3b-kept")
			tt.before(t, f)
			assert.Equal(t, tt.result, f.report(t, "run-b", true))
			row := mustGetAgent(t, f.raw, f.agent.ID)
			assert.Equal(t, "run-b", row.RunID, "the kept entry's run is adopted")
			assert.Nil(t, row.EffectiveConfig, "no echo to record")
			assertNoStaleEcho(t, f.counters)
			assert.Zero(t, f.unsettledLines())

			require.NoError(t, f.raw.UpdateAgentStatus(context.Background(), f.agent.ID,
				store.AgentStatusUpdate{Phase: string(state.PhaseStopped), IfRunID: "run-b"}))
			evs := f.events(t)
			assert.Empty(t, configuredWithEcho(evs), lcSeqFull(evs))
			assertNoEffective(t, evs)
			var adopted *tc.LifecycleEvent
			for _, e := range evs {
				if e.AdoptedFromLaunchID != "" {
					adopted = e
					break
				}
			}
			require.NotNil(t, adopted, "an event carries the adoption: %s", lcSeqFull(evs))
			assert.Equal(t, "run-b", adopted.LaunchID)
			assert.Equal(t, "run-m", adopted.AdoptedFromLaunchID)
			assert.Equal(t, store.AdoptionSourceCAS, adopted.AdoptionSource)
		})
	}
}
