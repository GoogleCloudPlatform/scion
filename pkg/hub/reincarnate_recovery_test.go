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
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Tests for ptone/scion#5511: a failed reincarnation carries the new
// handoff forward, behind a recovery addendum, instead of restoring the
// last generation's Task.

const (
	recoveryTestGenNTask   = "generation 1 preamble and its stale handoff"
	recoveryTestNewHandoff = "## Immediate active work\nthe handoff written for generation 2"
)

func withRecoveryTestGenNTask(a *store.Agent) { a.AppliedConfig.Task = recoveryTestGenNTask }

// runFailedRecoveryReincarnation runs a self-reincarnation with handoff
// against disp, which must make it fail, and returns the settled record.
func runFailedRecoveryReincarnation(t *testing.T, disp *reincarnateTestDispatcher, handoff string) (*Server, store.Store, *store.Agent, *store.RuntimeBroker, *store.AgentReincarnation) {
	t.Helper()
	srv, s, project, broker := setupReincarnateTestServer(t, disp)
	agent := newReincarnateTestAgent(t, s, project, broker, withRecoveryTestGenNTask)
	self := agentIdentityFor(agent.ID, project.ID)

	rec := httptest.NewRecorder()
	srv.handleReincarnateAgent(rec, reincarnateRequest(t, agent.ID, self, ReincarnateAgentRequest{Handoff: handoff}), agent.ID)
	require.Equal(t, http.StatusAccepted, rec.Code, rec.Body.String())
	r := waitForReincarnationSettled(t, s, agent.ID)
	require.Equal(t, store.AgentReincarnationStateFailed, r.State)
	return srv, s, agent, broker, r
}

// (1) A failed reprovision, whose re-render of the previous config is
// refused too (the ptone/scion#5510 409 case), leaves the agent row's Task
// as the recovery Task: the addendum plus the new handoff. The record's
// PreviousAppliedConfig, the audit copy, keeps the generation N Task.
func TestReincarnateRecovery_FailedReprovisionLeavesRecoveryTaskOnRow(t *testing.T) {
	disp := newReincarnateTestDispatcher()
	disp.reprovisionErr = fmt.Errorf("broker refused: reprovision refused: container is still running")
	disp.rerenderErr = fmt.Errorf("broker refused: reprovision refused: container is still running")
	_, s, agent, _, r := runFailedRecoveryReincarnation(t, disp, recoveryTestNewHandoff)

	final, err := s.GetAgent(context.Background(), agent.ID)
	require.NoError(t, err)
	assert.Equal(t, store.ReincarnationStateFailed, final.ReincarnationState)
	assert.Equal(t, r.ToGeneration-1, final.Generation, "the agent stays at generation N")
	require.NotNil(t, final.AppliedConfig)
	assert.Equal(t, reincarnationRecoveryTask(r.ToGeneration, recoveryTestNewHandoff, store.AgentReincarnationStateProvisioning), final.AppliedConfig.Task)
	assert.NotContains(t, final.AppliedConfig.Task, recoveryTestGenNTask, "the stale generation N Task must not be restored")
	assert.Equal(t, "old-image:v1", final.AppliedConfig.Image, "the rest of the previous config is restored")

	require.NotNil(t, r.PreviousAppliedConfig)
	assert.Equal(t, recoveryTestGenNTask, r.PreviousAppliedConfig.Task, "the record's PreviousAppliedConfig is not changed")
	assert.Equal(t, recoveryTestNewHandoff, r.Handoff)
}

// (2) The best-effort re-render of the previous config after a failed
// reprovision dispatches the recovery Task, so a successful re-render
// stages it in the broker's prompt.md too.
func TestReincarnateRecovery_RerenderDispatchesRecoveryTask(t *testing.T) {
	disp := newReincarnateTestDispatcher()
	disp.reprovisionErr = fmt.Errorf("request timeout after 2m0s")
	_, _, _, _, r := runFailedRecoveryReincarnation(t, disp, recoveryTestNewHandoff)

	calls, cfgs := disp.reprovisionSnapshot()
	require.Equal(t, 2, calls, "the failed reprovision plus one re-render")
	require.Len(t, cfgs, 2)
	assert.Equal(t, reincarnationRecoveryTask(r.ToGeneration, recoveryTestNewHandoff, store.AgentReincarnationStateProvisioning), cfgs[1].Task)
	assert.Equal(t, "old-image:v1", cfgs[1].Image, "the re-render is otherwise the previous config")
}

// (3) With no handoff there is nothing new to carry forward: the previous
// config is restored and re-rendered unchanged, with no addendum around
// the stale Task.
func TestReincarnateRecovery_EmptyHandoffRestoresPreviousUnchanged(t *testing.T) {
	disp := newReincarnateTestDispatcher()
	disp.reprovisionErr = fmt.Errorf("request timeout after 2m0s")
	srv, s, project, broker := setupReincarnateTestServer(t, disp)
	agent := newReincarnateTestAgent(t, s, project, broker, withRecoveryTestGenNTask)

	rec := reincarnateAsDev(t, srv, agent.ID, ReincarnateAgentRequest{})
	require.Equal(t, http.StatusAccepted, rec.Code, rec.Body.String())
	r := waitForReincarnationSettled(t, s, agent.ID)
	require.Equal(t, store.AgentReincarnationStateFailed, r.State)
	require.Empty(t, r.Handoff)

	final, err := s.GetAgent(context.Background(), agent.ID)
	require.NoError(t, err)
	assert.Equal(t, recoveryTestGenNTask, final.AppliedConfig.Task)
	_, cfgs := disp.reprovisionSnapshot()
	require.Len(t, cfgs, 2)
	assert.Equal(t, recoveryTestGenNTask, cfgs[1].Task)
	assert.Equal(t, *r.PreviousAppliedConfig, cfgs[1], "the re-render dispatches exactly the previous config")
}

// (4) A failure before any state change (a refused stop) restores the
// recovery config too, so the next start delivers the new handoff.
func TestReincarnateRecovery_StopFailureRestoresRecoveryTask(t *testing.T) {
	disp := newReincarnateTestDispatcher()
	disp.stopErr = fmt.Errorf("broker unreachable")
	_, s, agent, _, r := runFailedRecoveryReincarnation(t, disp, recoveryTestNewHandoff)

	stops, reprovisions, starts := dispatcherCalls(disp)
	require.Equal(t, 1, stops)
	require.Zero(t, reprovisions)
	require.Zero(t, starts)

	final, err := s.GetAgent(context.Background(), agent.ID)
	require.NoError(t, err)
	assert.Equal(t, reincarnationRecoveryTask(r.ToGeneration, recoveryTestNewHandoff, store.AgentReincarnationStateStopping), final.AppliedConfig.Task)
	assert.Equal(t, recoveryTestGenNTask, r.PreviousAppliedConfig.Task, "the record's PreviousAppliedConfig is not changed")
}

// (5) The next fresh start after the failure delivers the recovery Task.
// This is the real delivery path: a task-less, non-resume start falls back
// to the row's AppliedConfig.Task in HTTPAgentDispatcher.DispatchAgentStart
// and sends it to the broker as the explicit task, which wins over (and
// overwrites) a prompt.md the refused re-render left stale. A resume sends
// no task at all, by design.
func TestReincarnateRecovery_NextStartDeliversRecoveryTask(t *testing.T) {
	disp := newReincarnateTestDispatcher()
	disp.reprovisionErr = fmt.Errorf("broker refused: reprovision refused: container is still running")
	disp.rerenderErr = fmt.Errorf("broker refused: reprovision refused: container is still running")
	_, s, agent, broker, r := runFailedRecoveryReincarnation(t, disp, recoveryTestNewHandoff)
	ctx := context.Background()

	broker.Endpoint = "http://localhost:9800"
	require.NoError(t, s.UpdateRuntimeBroker(ctx, broker))

	want := reincarnationRecoveryTask(r.ToGeneration, recoveryTestNewHandoff, store.AgentReincarnationStateProvisioning)

	row, err := s.GetAgent(ctx, agent.ID)
	require.NoError(t, err)
	client := &mockRuntimeBrokerClient{}
	httpDisp := NewHTTPAgentDispatcherWithClient(s, client, false, slog.Default())
	require.NoError(t, httpDisp.DispatchAgentStart(ctx, row, "", false))
	require.True(t, client.startCalled)
	assert.Equal(t, want, client.lastTask, "the start delivers the recovery Task")
	assert.NotContains(t, client.lastTask, recoveryTestGenNTask)

	row, err = s.GetAgent(ctx, agent.ID)
	require.NoError(t, err)
	resumeClient := &mockRuntimeBrokerClient{}
	resumeDisp := NewHTTPAgentDispatcherWithClient(s, resumeClient, false, slog.Default())
	require.NoError(t, resumeDisp.DispatchAgentStart(ctx, row, "", true))
	require.True(t, resumeClient.startCalled)
	assert.Empty(t, resumeClient.lastTask, "a resume continues the session and is handed no task")
}

// The stale sweep's restore carries the handoff forward the same way, for
// a record still pending (its worker never ran) and for one that failed
// mid-flight before reprovision succeeded. The record's
// PreviousAppliedConfig is not changed.
func TestReincarnateRecovery_SweepRestoresRecoveryTask(t *testing.T) {
	for _, state := range []string{store.AgentReincarnationStatePending, store.AgentReincarnationStateStopping, store.AgentReincarnationStateProvisioning} {
		t.Run(state, func(t *testing.T) {
			ctx := context.Background()
			disp := newReincarnateTestDispatcher()
			srv, s, project, broker := setupReincarnateTestServer(t, disp)
			agent := newReincarnateTestAgent(t, s, project, broker, withRecoveryTestGenNTask)
			previous, err := s.GetAgent(ctx, agent.ID)
			require.NoError(t, err)

			rec := &store.AgentReincarnation{
				AgentID:               agent.ID,
				FromGeneration:        previous.Generation,
				ToGeneration:          previous.Generation + 1,
				State:                 state,
				PreviousAppliedConfig: previous.AppliedConfig,
				Handoff:               recoveryTestNewHandoff,
			}
			require.NoError(t, s.CreateAgentReincarnation(ctx, rec))

			agent, err = s.GetAgent(ctx, agent.ID)
			require.NoError(t, err)
			agent.ReincarnationState = state
			if state == store.AgentReincarnationStateProvisioning {
				// Mid-migration the row holds the fresh config.
				agent.Phase = "provisioning"
				agent.AppliedConfig = &store.AgentAppliedConfig{Image: "fresh-image:v2", Task: "[SCION REINCARNATION] fresh preamble"}
			}
			require.NoError(t, s.UpdateAgent(ctx, agent))

			future := time.Now().Add(time.Hour)
			n, err := srv.sweepStaleReincarnationsWithCutoffs(ctx, future, future)
			require.NoError(t, err)
			require.Equal(t, 1, n)

			after, err := s.GetAgent(ctx, agent.ID)
			require.NoError(t, err)
			assert.Equal(t, store.ReincarnationStateFailed, after.ReincarnationState)
			require.NotNil(t, after.AppliedConfig)
			assert.Equal(t, reincarnationRecoveryTask(rec.ToGeneration, recoveryTestNewHandoff, state), after.AppliedConfig.Task)
			assert.Equal(t, "old-image:v1", after.AppliedConfig.Image)

			got, err := s.GetAgentReincarnation(ctx, rec.ID)
			require.NoError(t, err)
			assert.Equal(t, store.AgentReincarnationStateFailed, got.State)
			require.NotNil(t, got.PreviousAppliedConfig)
			assert.Equal(t, recoveryTestGenNTask, got.PreviousAppliedConfig.Task, "the record's PreviousAppliedConfig is not changed")
		})
	}
}
