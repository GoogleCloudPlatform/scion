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
	"errors"
	"net/http"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const limitsMsg = "max_duration of 2m exceeded"

// restartingRuntime makes the fixture's broker list the agent's runtime in
// RestartingRuntimes. With gated, the armed stops wait until release.
func (f haltFixture) restartingRuntime(t *testing.T, gated bool) (release func()) {
	t.Helper()
	b, err := f.s.GetRuntimeBroker(context.Background(), f.broker.ID)
	require.NoError(t, err)
	b.Capabilities = &store.BrokerCapabilities{RestartingRuntimes: []string{f.agent.Runtime}}
	require.NoError(t, f.s.UpdateRuntimeBroker(context.Background(), b))
	gate := make(chan struct{})
	f.srv.limitsStopWait = func() {
		if gated {
			<-gate
		}
	}
	return func() { close(gate) }
}

// limitsStops (ptone/scion#7928) waits for the fixture's limits_exceeded
// stops and returns how many stops the broker received.
func (f haltFixture) limitsStops(t *testing.T) int {
	t.Helper()
	f.srv.limitsStopsInFlight.Wait()
	d := f.srv.GetDispatcher().(*reincarnateTestDispatcher)
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.stopCalls
}

func limitsReport() map[string]any {
	return map[string]any{"activity": "limits_exceeded", "message": limitsMsg}
}

func TestLimitsExceededStop_SelfReportStopsOnceAfterShutdown(t *testing.T) {
	f := newHaltFixture(t)
	release := f.restartingRuntime(t, true)
	require.Equal(t, http.StatusOK, f.selfReport(t, "run-1", limitsReport()))
	// sciontool's shutdown during the grace: its final report says
	// stopped, which does not cancel the armed stop. Repeat reports of the
	// run do not arm another.
	require.Equal(t, http.StatusOK, f.selfReport(t, "run-1", map[string]any{
		"phase": "stopped", "activity": "limits_exceeded", "exitReason": "limits_exceeded", "exitCode": 10,
	}))
	f.heartbeat(t, brokerAgentHeartbeat{Phase: "stopped", Activity: "limits_exceeded", RunID: "run-1"})
	release()
	assert.Equal(t, 1, f.limitsStops(t))

	got := f.get(t)
	assert.Equal(t, string(state.PhaseStopped), got.Phase)
	assert.Equal(t, string(state.ActivityLimitsExceeded), got.Activity, "the reported activity is kept")
	assert.Equal(t, "limits_exceeded", got.ExitReason, "the reported exit reason is kept")
	assert.Equal(t, store.RunIntentRunning, got.RunIntent, "intent stays as after the container's own exit")
	_, held := f.srv.limitsStops.Load(f.agent.ID)
	assert.False(t, held, "the run is forgotten once its stop ends")
	assert.False(t, f.srv.lifecycleOps.active(f.agent.ID), "the stop's lifecycle op has ended")
}

func TestLimitsExceededStop_HeartbeatStopsOnce(t *testing.T) {
	f := newHaltFixture(t)
	f.restartingRuntime(t, false)
	hb := brokerAgentHeartbeat{Phase: "running", Activity: "limits_exceeded", Message: limitsMsg, RunID: "run-1"}
	f.heartbeat(t, hb)
	f.heartbeat(t, hb)
	assert.Equal(t, 1, f.limitsStops(t))
	got := f.get(t)
	assert.Equal(t, string(state.PhaseStopped), got.Phase)
	assert.Equal(t, string(state.ActivityLimitsExceeded), got.Activity)
	assert.Equal(t, limitsMsg, got.Message, "the reported message is kept")
}

func TestLimitsExceededStop_LostEarlyReportFinalStoppedStops(t *testing.T) {
	// The first limits_exceeded the hub sees is the final self-report,
	// phase stopped: the run is still stopped on its runtime.
	f := newHaltFixture(t)
	f.restartingRuntime(t, false)
	require.Equal(t, http.StatusOK, f.selfReport(t, "run-1", map[string]any{
		"phase": "stopped", "activity": "limits_exceeded", "exitReason": "limits_exceeded", "exitCode": 10,
	}))
	assert.Equal(t, 1, f.limitsStops(t))
	assert.Equal(t, string(state.PhaseStopped), f.get(t).Phase)
}

func TestLimitsExceededStop_NonRestartingRuntimeNotStopped(t *testing.T) {
	// Docker, Podman and Kubernetes containers stay exited: a broker that
	// does not list the agent's runtime (or predates the capability) gets
	// no stop, and the agent's own reports end the run as before.
	f := newHaltFixture(t)
	require.Equal(t, http.StatusOK, f.selfReport(t, "run-1", limitsReport()))
	f.heartbeat(t, brokerAgentHeartbeat{Phase: "running", Activity: "limits_exceeded", RunID: "run-1"})
	assert.Equal(t, 0, f.limitsStops(t))
	assert.Equal(t, string(state.PhaseRunning), f.get(t).Phase)
}

func TestLimitsExceededStop_StaleRunNotStopped(t *testing.T) {
	f := newHaltFixture(t)
	f.restartingRuntime(t, false)
	require.Equal(t, http.StatusOK, f.selfReport(t, "run-0", limitsReport()))
	f.heartbeat(t, brokerAgentHeartbeat{Phase: "running", Activity: "limits_exceeded", RunID: "run-0"})
	assert.Equal(t, 0, f.limitsStops(t))
	assert.Equal(t, string(state.PhaseRunning), f.get(t).Phase)
}

func TestLimitsExceededStop_UserStoppedNotStopped(t *testing.T) {
	f := newHaltFixture(t)
	f.restartingRuntime(t, false)
	ctx := context.Background()
	_, err := f.s.SetRunIntent(ctx, f.agent.ID, store.RunIntentStopped)
	require.NoError(t, err)
	require.NoError(t, f.s.UpdateAgentStatus(ctx, f.agent.ID,
		store.AgentStatusUpdate{Phase: string(state.PhaseStopped), Activity: string(state.ActivityCompleted)}))
	// A late heartbeat still records the terminal activity, but the user
	// already stopped the run.
	f.heartbeat(t, brokerAgentHeartbeat{Phase: "stopped", Activity: "limits_exceeded", RunID: "run-1"})
	assert.Equal(t, 0, f.limitsStops(t))
	assert.Equal(t, string(state.ActivityLimitsExceeded), f.get(t).Activity)
}

func TestLimitsExceededStop_UserStopDuringGraceWins(t *testing.T) {
	f := newHaltFixture(t)
	release := f.restartingRuntime(t, true)
	f.heartbeat(t, brokerAgentHeartbeat{Phase: "running", Activity: "limits_exceeded", RunID: "run-1"})
	_, err := f.s.SetRunIntent(context.Background(), f.agent.ID, store.RunIntentStopped)
	require.NoError(t, err)
	release()
	assert.Equal(t, 0, f.limitsStops(t))
}

func TestLimitsExceededStop_FailedDispatchRetries(t *testing.T) {
	f := newHaltFixture(t)
	f.restartingRuntime(t, false)
	d := f.srv.GetDispatcher().(*reincarnateTestDispatcher)
	d.mu.Lock()
	d.stopErr = errors.New("broker unreachable")
	d.mu.Unlock()
	f.heartbeat(t, brokerAgentHeartbeat{Phase: "running", Activity: "limits_exceeded", RunID: "run-1"})
	assert.Equal(t, 1, f.limitsStops(t))
	assert.Equal(t, string(state.PhaseRunning), f.get(t).Phase)

	// The run is forgotten: the next limits_exceeded report of it (from
	// a restarted container) stops it.
	d.mu.Lock()
	d.stopErr = nil
	d.mu.Unlock()
	f.heartbeat(t, brokerAgentHeartbeat{Phase: "running", Activity: "working", RunID: "run-1"})
	f.heartbeat(t, brokerAgentHeartbeat{Phase: "running", Activity: "limits_exceeded", RunID: "run-1"})
	assert.Equal(t, 2, f.limitsStops(t))
	assert.Equal(t, string(state.PhaseStopped), f.get(t).Phase)
}

func TestLimitsExceededStop_ArmedStopNotRepeated(t *testing.T) {
	// The run flips limits_exceeded -> working -> limits_exceeded while its
	// stop is armed: the second transition does not arm another.
	f := newHaltFixture(t)
	release := f.restartingRuntime(t, true)
	f.heartbeat(t, brokerAgentHeartbeat{Phase: "running", Activity: "limits_exceeded", RunID: "run-1"})
	f.heartbeat(t, brokerAgentHeartbeat{Phase: "running", Activity: "working", RunID: "run-1"})
	f.heartbeat(t, brokerAgentHeartbeat{Phase: "running", Activity: "limits_exceeded", RunID: "run-1"})
	release()
	assert.Equal(t, 1, f.limitsStops(t))
	assert.Equal(t, string(state.PhaseStopped), f.get(t).Phase)
}
