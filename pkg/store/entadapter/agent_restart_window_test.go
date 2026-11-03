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

// Restart-window store tests: the run check (UpdateAgentStatus, the
// ReportRunID filter) across a restart. A restart's start leg records the
// new run with SetAgentRunID before it calls the broker (the hub's
// HTTPAgentDispatcher.beginRun makes exactly that store call), so these
// tests model "restart accepted and minted" with the same call.

package entadapter

import (
	"context"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// restartWindowAgent is a running kubernetes agent of run "run-1" whose
// restart has recorded the new run "run-2".
func restartWindowAgent(t *testing.T, ctx context.Context, slug string) (*AgentStore, string) {
	t.Helper()
	s, projectID := newTestAgentStore(t)
	id := newHaltAgent(t, ctx, s, projectID, slug, defaultHaltRow())
	prior, err := s.SetAgentRunID(ctx, id, "run-2", nil)
	require.NoError(t, err)
	require.Equal(t, "run-1", prior)
	return s, id
}

// restartWindowHeartbeat is a broker heartbeat of run runID that would move
// the row's container status and message if it applied.
func restartWindowHeartbeat(runID, containerStatus, message string) store.AgentStatusUpdate {
	return store.AgentStatusUpdate{Phase: "running", ContainerStatus: containerStatus, Message: message, Heartbeat: true,
		ReportSource: store.StatusReportSourceRuntime, ReportRunID: runID}
}

func TestAgentRestartWindow_OldRunDroppedAfterSetRunID(t *testing.T) {
	ctx := context.Background()
	s, id := restartWindowAgent(t, ctx, "rw-old")
	before, err := s.GetAgent(ctx, id)
	require.NoError(t, err)
	// newHaltAgent writes last-seen an hour in the past, so the advance
	// below is checked against the write time, not at clock resolution.
	require.True(t, before.LastSeen.Before(time.Now().Add(-30*time.Minute)), "fixture last-seen is in the past")

	apply(t, ctx, s, id, restartWindowHeartbeat("run-1", "exited (143)", "old run"))
	oldSelf := selfReport("error", "")
	oldSelf.ReportRunID = "run-1"
	oldSelf.ExitReason = "oom_killed"
	apply(t, ctx, s, id, oldSelf)

	got, err := s.GetAgent(ctx, id)
	require.NoError(t, err)
	assert.Equal(t, "run-2", got.RunID)
	assert.Equal(t, before.Phase, got.Phase)
	assert.Equal(t, before.Activity, got.Activity)
	assert.Equal(t, before.ContainerStatus, got.ContainerStatus)
	assert.Equal(t, before.Message, got.Message)
	assert.Equal(t, before.ExitReason, got.ExitReason)
	assert.WithinDuration(t, time.Now(), got.LastSeen, time.Minute, "last-seen still advances to the report time")
}

func TestAgentRestartWindow_NewRunApplied(t *testing.T) {
	ctx := context.Background()
	s, id := restartWindowAgent(t, ctx, "rw-new")

	apply(t, ctx, s, id, restartWindowHeartbeat("run-2", "running (new)", "new run"))
	got, err := s.GetAgent(ctx, id)
	require.NoError(t, err)
	assert.Equal(t, "running (new)", got.ContainerStatus)
	assert.Equal(t, "new run", got.Message)

	newSelf := selfReport("running", "idle")
	newSelf.ReportRunID = "run-2"
	apply(t, ctx, s, id, newSelf)
	got, err = s.GetAgent(ctx, id)
	require.NoError(t, err)
	assert.Equal(t, "idle", got.Activity)
}

// TestAgentRestartWindow_CharacterizesRunlessReportAccepted documents
// CURRENT behaviour; it is not an endorsement. The run check skips a
// report that names no run (ReportRunID ""), so after a restart has
// recorded the new run, a run-less report (a pod without the run label,
// or an older broker) still changes the row. See gap (1) in
// notes/kr-restart-window-gaps.md for the analysis and the proposed fix.
func TestAgentRestartWindow_CharacterizesRunlessReportAccepted(t *testing.T) {
	ctx := context.Background()
	s, id := restartWindowAgent(t, ctx, "rw-runless")

	apply(t, ctx, s, id, restartWindowHeartbeat("", "exited (1)", "run-less report"))
	got, err := s.GetAgent(ctx, id)
	require.NoError(t, err)
	assert.Equal(t, "run-2", got.RunID)
	assert.Equal(t, "exited (1)", got.ContainerStatus, "current behaviour: a run-less report applies")
	assert.Equal(t, "run-less report", got.Message, "current behaviour: a run-less report applies")
}
