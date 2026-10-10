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
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const oomMessage137 = "Agent container was killed for exceeding its memory limit, exit code 137"

// TestHeartbeatStopReason_OOMKilledWhileRunning: the broker reports the agent
// container terminated with OOMKilled (exit 137). It is recorded as
// oom_killed with its own message, and the phase is error (a crash).
func TestHeartbeatStopReason_OOMKilledWhileRunning(t *testing.T) {
	srv, s, brokerID, projectID, agentSlug := setupHeartbeatExitCodeTest(t)

	ec := 137
	code := sendHeartbeat(t, srv, brokerID, projectID, brokerAgentHeartbeat{
		Slug:       agentSlug,
		Phase:      "error",
		ExitCode:   &ec,
		ExitReason: "oom_killed",
	})
	assert.Equal(t, http.StatusOK, code)

	got := getAgentState(t, s, agentSlug, projectID)
	assert.Equal(t, "error", got.Phase)
	assert.Equal(t, "oom_killed", got.ExitReason)
	assert.Equal(t, oomMessage137, got.Message)
	require.NotNil(t, got.ExitCode)
	assert.Equal(t, 137, *got.ExitCode)
}

// TestHeartbeatStopReason_OOMKilledAfterCrashReport: the agent is already in
// error with a plain crash recorded (its own report, or an earlier
// heartbeat). A heartbeat carrying oom_killed records the more specific
// reason and replaces only a message that says nothing more.
func TestHeartbeatStopReason_OOMKilledAfterCrashReport(t *testing.T) {
	for _, tc := range []struct {
		name        string
		storedRsn   string
		storedMsg   string
		wantReason  string
		wantMessage string
	}{
		{"empty reason, crash message", "", "Agent crashed with exit code 137", "oom_killed", oomMessage137},
		{"crashed reason, crash message", "crashed", "Agent crashed with exit code 137", "oom_killed", oomMessage137},
		{"crashed reason, generic message", "crashed", "", "oom_killed", oomMessage137},
		{"crashed reason, specific message kept", "crashed", "harness: out of memory in tool X", "oom_killed", "harness: out of memory in tool X"},
		{"preempted reason kept", "preempted", "Agent pod was preempted", "preempted", "Agent pod was preempted"},
		{"limits_exceeded reason kept", "limits_exceeded", "Agent crashed with exit code 137", "limits_exceeded", "Agent crashed with exit code 137"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv, s, brokerID, projectID, agentSlug := setupHeartbeatExitCodeTest(t)

			agent := getAgentState(t, s, agentSlug, projectID)
			agent.Phase = "error"
			agent.ExitReason = tc.storedRsn
			agent.Message = tc.storedMsg
			stored := 137
			agent.ExitCode = &stored
			require.NoError(t, s.UpdateAgent(context.Background(), agent))

			ec := 137
			code := sendHeartbeat(t, srv, brokerID, projectID, brokerAgentHeartbeat{
				Slug:       agentSlug,
				Phase:      "error",
				ExitCode:   &ec,
				ExitReason: "oom_killed",
			})
			assert.Equal(t, http.StatusOK, code)

			got := getAgentState(t, s, agentSlug, projectID)
			assert.Equal(t, "error", got.Phase, "phase is kept")
			assert.Equal(t, tc.wantReason, got.ExitReason)
			assert.Equal(t, tc.wantMessage, got.Message)
		})
	}
}

// TestHeartbeatStopReason_OOMKilledDoesNotMoveStopped: oom_killed is a crash,
// not a disruption: the terminal backfill never moves a stopped agent's
// phase for it.
func TestHeartbeatStopReason_OOMKilledDoesNotMoveStopped(t *testing.T) {
	srv, s, brokerID, projectID, agentSlug := setupHeartbeatExitCodeTest(t)

	agent := getAgentState(t, s, agentSlug, projectID)
	agent.Phase = "stopped"
	agent.Message = "Agent stopped"
	require.NoError(t, s.UpdateAgent(context.Background(), agent))

	ec := 137
	code := sendHeartbeat(t, srv, brokerID, projectID, brokerAgentHeartbeat{
		Slug:       agentSlug,
		Phase:      "error",
		ExitCode:   &ec,
		ExitReason: "oom_killed",
	})
	assert.Equal(t, http.StatusOK, code)

	got := getAgentState(t, s, agentSlug, projectID)
	assert.Equal(t, "stopped", got.Phase)
	assert.Equal(t, "oom_killed", got.ExitReason)
	assert.Equal(t, oomMessage137, got.Message)
}

// TestHeartbeatStopReason_TombstoneAfterCleanStop: the early-preemption case.
// The agent's own clean stop on SIGTERM reached the hub first (stopped, no
// reason), and the pod was removed before any heartbeat listed it terminal.
// The broker's runtime then reports a tombstone for the vanished pod: phase
// stopped or error, reason preempted, no exit code, container status
// "deleted (Preempted)". The hub records the reason.
func TestHeartbeatStopReason_TombstoneAfterCleanStop(t *testing.T) {
	for _, tc := range []struct {
		name      string
		hbPhase   string
		wantPhase string
	}{
		{"persistent workspace", "stopped", "stopped"},
		{"emptydir workspace", "error", "error"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv, s, brokerID, projectID, agentSlug := setupHeartbeatExitCodeTest(t)

			agent := getAgentState(t, s, agentSlug, projectID)
			agent.Phase = "stopped"
			agent.Message = "Agent stopped"
			zero := 0
			agent.ExitCode = &zero
			require.NoError(t, s.UpdateAgent(context.Background(), agent))

			code := sendHeartbeat(t, srv, brokerID, projectID, brokerAgentHeartbeat{
				Slug:            agentSlug,
				Phase:           tc.hbPhase,
				ContainerStatus: "deleted (Preempted)",
				ExitReason:      "preempted",
			})
			assert.Equal(t, http.StatusOK, code)

			got := getAgentState(t, s, agentSlug, projectID)
			assert.Equal(t, tc.wantPhase, got.Phase)
			assert.Equal(t, "preempted", got.ExitReason)
			assert.Equal(t, "Agent pod was preempted", got.Message)
		})
	}
}
