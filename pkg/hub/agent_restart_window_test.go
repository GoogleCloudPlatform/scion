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

// Restart-window tests: the run check across a restart, through the real
// broker heartbeat handler and the agent status endpoint. A restart's start
// leg records the new run with store.SetAgentRunID before it calls the
// broker (HTTPAgentDispatcher.beginRun makes exactly that call), so
// restartMinted models "restart accepted and the new run recorded" with the
// same store call. The fixture (haltFixture) is a running kubernetes agent
// of run "run-1".

import (
	"context"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// restartMinted records run "run-2" on the fixture's agent, the write a
// restart's beginRun makes before the broker call.
func (f haltFixture) restartMinted(t *testing.T) {
	t.Helper()
	prior, err := f.s.SetAgentRunID(context.Background(), f.agent.ID, "run-2", nil)
	require.NoError(t, err)
	require.Equal(t, "run-1", prior)
}

func TestRestartWindow_HeartbeatOldRunIgnoredAfterRestart(t *testing.T) {
	f := newHaltFixture(t)
	f.restartMinted(t)
	before := f.get(t)
	code := 143
	f.heartbeat(t, brokerAgentHeartbeat{Phase: "stopped", ExitReason: "preempted", ExitCode: &code,
		ContainerStatus: "exited (143)", Message: "old run", RunID: "run-1"})
	got := f.get(t)
	assert.Equal(t, "run-2", got.RunID)
	assert.Equal(t, before.Phase, got.Phase)
	assert.Equal(t, before.Activity, got.Activity)
	assert.Equal(t, before.ContainerStatus, got.ContainerStatus)
	assert.Equal(t, before.Message, got.Message)
	assert.Empty(t, got.ExitReason)
}

func TestRestartWindow_SelfReportOldRunIgnoredAfterRestart(t *testing.T) {
	f := newHaltFixture(t)
	f.restartMinted(t)
	before := f.get(t)
	require.Equal(t, http.StatusOK, f.selfReport(t, "run-1", map[string]any{"phase": "stopped", "exitReason": "oom_killed"}))
	got := f.get(t)
	assert.Equal(t, before.Phase, got.Phase)
	assert.Equal(t, before.Activity, got.Activity)
	assert.Empty(t, got.ExitReason)
}

func TestRestartWindow_HeartbeatNewRunApplied(t *testing.T) {
	f := newHaltFixture(t)
	f.restartMinted(t)
	f.heartbeat(t, brokerAgentHeartbeat{Phase: "running", ContainerStatus: "running (new)", Message: "new run", RunID: "run-2"})
	got := f.get(t)
	assert.Equal(t, "running (new)", got.ContainerStatus)
	assert.Equal(t, "new run", got.Message)
}

func TestRestartWindow_SelfReportNewRunApplied(t *testing.T) {
	f := newHaltFixture(t)
	f.restartMinted(t)
	require.Equal(t, http.StatusOK, f.selfReport(t, "run-2", map[string]any{"phase": "running", "activity": "idle"}))
	assert.Equal(t, "idle", f.get(t).Activity)
}

// TestRestartWindow_CharacterizesRunlessHeartbeatAccepted documents CURRENT
// behaviour; it is not an endorsement. The store's run check skips a report
// that names no run, so after a restart has recorded the new run, a
// heartbeat entry without a run ID (a pod without the scion.run_id label,
// or an older broker) still changes the agent. See gap (1) in
// notes/kr-restart-window-gaps.md for the analysis and the proposed fix.
func TestRestartWindow_CharacterizesRunlessHeartbeatAccepted(t *testing.T) {
	f := newHaltFixture(t)
	f.restartMinted(t)
	f.heartbeat(t, brokerAgentHeartbeat{Phase: "running", ContainerStatus: "exited (1)", Message: "run-less report"})
	got := f.get(t)
	assert.Equal(t, "run-2", got.RunID)
	assert.Equal(t, "exited (1)", got.ContainerStatus, "current behaviour: a run-less heartbeat applies")
	assert.Equal(t, "run-less report", got.Message, "current behaviour: a run-less heartbeat applies")
}

// TestRestartWindow_CharacterizesRunlessSelfReportAccepted documents CURRENT
// behaviour; it is not an endorsement. An agent token without a run claim
// presents no run (presentedAgentTokenRunID ""), so after a restart has
// recorded the new run its report still changes the agent. With no start
// launch active, Guard 0f does not apply. See gap (1) in
// notes/kr-restart-window-gaps.md.
func TestRestartWindow_CharacterizesRunlessSelfReportAccepted(t *testing.T) {
	f := newHaltFixture(t)
	f.restartMinted(t)
	require.Equal(t, http.StatusOK, f.selfReport(t, "", map[string]any{"phase": "running", "activity": "idle"}))
	assert.Equal(t, "idle", f.get(t).Activity, "current behaviour: a run-less self-report applies")
}

// TestRestartWindow_CharacterizesOldRunReportAcceptedBeforeMint documents
// CURRENT behaviour; it is not an endorsement. A restart mints the new run
// only in its start leg, after the stop leg, so between accepting the
// restart and that mint the row still holds the old run, and the old pod's
// reports pass the run check. Here the restart is accepted but not yet
// minted (the row is still "run-1"). See gap (2) in
// notes/kr-restart-window-gaps.md.
func TestRestartWindow_CharacterizesOldRunReportAcceptedBeforeMint(t *testing.T) {
	f := newHaltFixture(t)
	f.heartbeat(t, brokerAgentHeartbeat{Phase: "running", ContainerStatus: "exited (143)", Message: "old pod", RunID: "run-1"})
	got := f.get(t)
	assert.Equal(t, "run-1", got.RunID)
	assert.Equal(t, "exited (143)", got.ContainerStatus, "current behaviour: the old run's report applies before the mint")
	assert.Equal(t, "old pod", got.Message, "current behaviour: the old run's report applies before the mint")
}
