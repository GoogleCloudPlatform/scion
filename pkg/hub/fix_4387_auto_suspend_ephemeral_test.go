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
	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ptone/scion#4387 item 1: a scheduled auto-suspend of an agent with an
// ephemeral workspace runs the pre-stop workspace check and records its
// result, as suspend does, so the next start reports the lost work.
func TestAutoSuspend_EphemeralWorkspaceRecorded(t *testing.T) {
	ctx := context.Background()
	disp := &workspaceCheckDispatcher{execOutput: workAt23}
	srv, s, broker, project := newWorkspaceCheckServer(t, disp)
	a := newWorkspaceAgent(t, s, broker, project, "ws-autosuspend", "kubernetes", api.WorkspacePlacementLocal, state.PhaseRunning)
	markAgentStalled(t, s, a.ID)
	loaded, err := s.GetAgent(ctx, a.ID)
	require.NoError(t, err)

	srv.autoSuspendStalledAgents(ctx, []store.Agent{*loaded})

	assert.Equal(t, 1, disp.calls(), "the workspace check ran")
	got, err := s.GetAgent(ctx, a.ID)
	require.NoError(t, err)
	assert.Equal(t, string(state.PhaseSuspended), got.Phase)
	rec := recordedWorkspaceAtStop(got)
	require.NotNil(t, rec, "the auto-suspend recorded the workspace check")
	assert.Equal(t, ephemeralWorkspaceRecord{Commits: 2, Files: 3}, *rec)

	resp := doRequest(t, srv, http.MethodPost, "/api/v1/agents/"+a.ID+"/start", nil)
	require.Equal(t, http.StatusOK, resp.Code, resp.Body.String())
	assert.Equal(t, []string{startWarning23}, lifecycleWarnings(t, resp.Body.Bytes()))
}

// A failed auto-suspend dispatch leaves the agent running: as for suspend,
// the record is cleared, and the running intent is put back as before.
func TestAutoSuspend_EphemeralWorkspaceFailedStopClearsRecord(t *testing.T) {
	ctx := context.Background()
	disp := &workspaceCheckDispatcher{execOutput: workAt23, stopErr: errors.New("broker failed the stop")}
	srv, s, broker, project := newWorkspaceCheckServer(t, disp)
	a := newWorkspaceAgent(t, s, broker, project, "ws-autosuspend-failed", "kubernetes", api.WorkspacePlacementLocal, state.PhaseRunning)
	_, err := s.SetRunIntent(ctx, a.ID, store.RunIntentRunning)
	require.NoError(t, err)
	markAgentStalled(t, s, a.ID)
	loaded, err := s.GetAgent(ctx, a.ID)
	require.NoError(t, err)

	srv.autoSuspendStalledAgents(ctx, []store.Agent{*loaded})

	assert.Equal(t, 1, disp.calls(), "the check ran before the failed dispatch")
	assert.Empty(t, workspaceAnnotation(t, s, a.ID), "a failed auto-suspend leaves no record")
	got := requireRunIntent(t, s, a.ID, store.RunIntentRunning)
	assert.Equal(t, string(state.PhaseRunning), got.Phase)
}
