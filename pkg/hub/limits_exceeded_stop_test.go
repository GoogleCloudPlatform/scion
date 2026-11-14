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

// limitsStops (ptone/scion#7928) waits for the fixture's limits_exceeded stops and returns how
// many stops the broker received.
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

func TestLimitsExceededStop_SelfReportStopsOnce(t *testing.T) {
	f := newHaltFixture(t)
	require.Equal(t, http.StatusOK, f.selfReport(t, "run-1", limitsReport()))
	assert.Equal(t, 1, f.limitsStops(t))

	got := f.get(t)
	assert.Equal(t, string(state.PhaseStopped), got.Phase)
	assert.Equal(t, string(state.ActivityLimitsExceeded), got.Activity, "the reported activity is kept")
	assert.Equal(t, limitsMsg, got.Message, "the reported message is kept")
	assert.Equal(t, store.RunIntentRunning, got.RunIntent, "intent stays as after the container's own exit")

	// Repeat reports of the run, from the agent and the broker, do not
	// stop it again.
	require.Equal(t, http.StatusOK, f.selfReport(t, "run-1", limitsReport()))
	f.heartbeat(t, brokerAgentHeartbeat{Phase: "stopped", Activity: "limits_exceeded", ExitReason: "limits_exceeded", RunID: "run-1"})
	assert.Equal(t, 1, f.limitsStops(t))
	got = f.get(t)
	assert.Equal(t, string(state.PhaseStopped), got.Phase)
	assert.Equal(t, string(state.ActivityLimitsExceeded), got.Activity)
}

func TestLimitsExceededStop_HeartbeatStopsOnce(t *testing.T) {
	f := newHaltFixture(t)
	hb := brokerAgentHeartbeat{Phase: "running", Activity: "limits_exceeded", Message: limitsMsg, RunID: "run-1"}
	f.heartbeat(t, hb)
	f.heartbeat(t, hb)
	assert.Equal(t, 1, f.limitsStops(t))
	got := f.get(t)
	assert.Equal(t, string(state.PhaseStopped), got.Phase)
	assert.Equal(t, string(state.ActivityLimitsExceeded), got.Activity)
}

func TestLimitsExceededStop_StaleRunNotStopped(t *testing.T) {
	f := newHaltFixture(t)
	require.Equal(t, http.StatusOK, f.selfReport(t, "run-0", limitsReport()))
	f.heartbeat(t, brokerAgentHeartbeat{Phase: "running", Activity: "limits_exceeded", RunID: "run-0"})
	assert.Equal(t, 0, f.limitsStops(t))
	assert.Equal(t, string(state.PhaseRunning), f.get(t).Phase)
}

func TestLimitsExceededStop_AlreadyStoppedNotStopped(t *testing.T) {
	f := newHaltFixture(t)
	require.NoError(t, f.s.UpdateAgentStatus(context.Background(), f.agent.ID,
		store.AgentStatusUpdate{Phase: string(state.PhaseStopped), Activity: string(state.ActivityCompleted)}))
	// A late heartbeat still records the terminal activity, but the run
	// is already stopped.
	f.heartbeat(t, brokerAgentHeartbeat{Phase: "stopped", Activity: "limits_exceeded", RunID: "run-1"})
	assert.Equal(t, 0, f.limitsStops(t))
	assert.Equal(t, string(state.ActivityLimitsExceeded), f.get(t).Activity)
}

func TestLimitsExceededStop_FailedDispatchRetries(t *testing.T) {
	f := newHaltFixture(t)
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
