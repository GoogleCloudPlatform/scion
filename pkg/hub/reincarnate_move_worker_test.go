//go:build !no_sqlite

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
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// prepareMoveWorkerFixture gives the fixture agent a run on the source, a
// broker reservation there and an exposed port, as a running agent has.
func prepareMoveWorkerFixture(t *testing.T, f *moveFixture) {
	t.Helper()
	ctx := context.Background()
	_, err := f.s.SetAgentRunID(ctx, f.agent.ID, "run-src")
	require.NoError(t, err)
	reserveBrokerSlot(t, f.s, f.src, f.agent.ID)
	require.NoError(t, f.s.UpdateAgentExposedPorts(ctx, f.agent.ID, []store.ExposedPort{{Port: 8080, Label: "web", Mode: "http"}}))
}

// startMove sends a real move as the agent itself and waits for the worker.
func startMove(t *testing.T, f *moveFixture) (*store.AgentReincarnation, *store.Agent) {
	t.Helper()
	rec := f.reincarnate(t, ReincarnateAgentRequest{Handoff: "moving on", TargetBroker: f.dst.ID})
	require.Equal(t, http.StatusAccepted, rec.Code, rec.Body.String())
	var resp ReincarnateAgentResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.Equal(t, f.src.ID, resp.SourceBrokerID)
	assert.Equal(t, f.dst.ID, resp.TargetBrokerID)
	r := waitForReincarnationSettled(t, f.s, f.agent.ID)
	a, err := f.s.GetAgent(context.Background(), f.agent.ID)
	require.NoError(t, err)
	return r, a
}

// A completed move: the agent runs on the target with the next
// generation, provisioned there with the expected workspace and started
// with the preamble; the reservation moved to the target; the source's
// exposed ports are cleared and its local state removed (sourceCleanup
// done) with a localOnly delete aimed at the source's run.
func TestReincarnateMove_CompletesOnTarget(t *testing.T) {
	f := setupMoveFixture(t, true, nil)
	prepareMoveWorkerFixture(t, f)

	r, a := startMove(t, f)
	require.Equal(t, store.AgentReincarnationStateCompleted, r.State, r.Error)
	assert.Equal(t, f.src.ID, r.SourceBrokerID)
	assert.Equal(t, f.dst.ID, r.TargetBrokerID)
	assert.Equal(t, store.SourceCleanupDone, r.SourceCleanup)

	assert.Equal(t, f.dst.ID, a.RuntimeBrokerID)
	assert.Equal(t, 2, a.Generation)
	assert.Empty(t, a.ExposedPorts, "the source's exposed ports are cleared")

	provisions, deletes, starts := f.disp.moveSnapshot()
	assert.Equal(t, []string{f.dst.ID + "|agent-dir"}, provisions)
	assert.Equal(t, []string{f.dst.ID}, starts)
	assert.Equal(t, []string{f.src.ID + "|run-src"}, deletes, "only the source's run is cleaned up, localOnly")
	f.disp.mu.Lock()
	assert.Equal(t, 1, f.disp.stopCalls)
	assert.Zero(t, f.disp.reprovisionCalls, "a move provisions fresh on the target, never reprovisions")
	require.NotNil(t, f.disp.lastStartResume)
	assert.False(t, *f.disp.lastStartResume)
	assert.Contains(t, f.disp.lastStartTask, "moving on", "the handoff reaches the new generation")
	f.disp.mu.Unlock()

	// Release before reserve: the slot is counted against the target only.
	assert.EqualValues(t, 0, brokerReservationCount(t, f.s, f.src.ID))
	assert.EqualValues(t, 1, brokerReservationCount(t, f.s, f.dst.ID))
}

// A failed source cleanup is recorded and never fails the move.
func TestReincarnateMove_SourceCleanupFailureRecorded(t *testing.T) {
	f := setupMoveFixture(t, true, nil)
	prepareMoveWorkerFixture(t, f)
	f.disp.localOnlyDeleteErr = map[string]error{f.src.ID: errors.New("source broker offline")}

	r, a := startMove(t, f)
	require.Equal(t, store.AgentReincarnationStateCompleted, r.State, r.Error)
	assert.Equal(t, f.dst.ID, a.RuntimeBrokerID)
	assert.True(t, strings.HasPrefix(r.SourceCleanup, "failed:"), "sourceCleanup = %q", r.SourceCleanup)
	assert.Contains(t, r.SourceCleanup, "source broker offline")
}

// assertRolledBackToSource checks a failed move left the agent stopped on
// the source exactly as before: broker, runtime, placement (A12) and
// applied config restored, the reservation back on the source, and the
// target's state removed with a localOnly delete.
func assertRolledBackToSource(t *testing.T, f *moveFixture, r *store.AgentReincarnation, a *store.Agent) {
	t.Helper()
	require.Equal(t, store.AgentReincarnationStateFailed, r.State)
	assert.Equal(t, f.src.ID, a.RuntimeBrokerID, "the agent stays on the source")
	assert.Equal(t, "kubernetes", a.Runtime, "the source runtime is restored")
	assert.Equal(t, api.WorkspacePlacementExport, a.WorkspacePlacement, "placement reflects the source after rollback")
	assert.Equal(t, 1, a.Generation)
	require.NotNil(t, a.AppliedConfig)
	assert.Equal(t, "old-image:v1", a.AppliedConfig.Image, "the previous applied config is restored")
	assert.Equal(t, store.ReincarnationStateFailed, a.ReincarnationState)

	_, deletes, _ := f.disp.moveSnapshot()
	require.NotEmpty(t, deletes)
	assert.True(t, strings.HasPrefix(deletes[0], f.dst.ID+"|"), "the target's state is removed: %q", deletes)
	for _, d := range deletes {
		assert.False(t, strings.HasPrefix(d, f.src.ID+"|"), "the source is never cleaned up after a failed move: %q", deletes)
	}
	assert.EqualValues(t, 1, brokerReservationCount(t, f.s, f.src.ID), "the reservation is back on the source")
	assert.EqualValues(t, 0, brokerReservationCount(t, f.s, f.dst.ID))
	assert.Empty(t, r.SourceCleanup)
}

// A9: the target finds no workspace on its mount (409 on the move's
// provision), so the move rolls back and the agent is never started.
func TestReincarnateMove_TargetWorkspaceMissingRollsBack(t *testing.T) {
	f := setupMoveFixture(t, true, nil)
	prepareMoveWorkerFixture(t, f)
	f.disp.moveProvisionErr = errors.New("broker returned 409: the agent's workspace is not on this broker's NFS export")

	r, a := startMove(t, f)
	assertRolledBackToSource(t, f, r, a)
	assert.Contains(t, r.Error, "workspace is not on this broker's NFS export")
	_, _, starts := f.disp.moveSnapshot()
	assert.Empty(t, starts, "the agent is not started anywhere")
}

// A failed start on the target rolls back too.
func TestReincarnateMove_TargetStartFailureRollsBack(t *testing.T) {
	f := setupMoveFixture(t, true, nil)
	prepareMoveWorkerFixture(t, f)
	f.disp.startErr = errors.New("start refused by broker")

	r, a := startMove(t, f)
	assertRolledBackToSource(t, f, r, a)
	assert.Contains(t, r.Error, "start failed")
}

// A user moving an agent to a broker that does not yet serve the project
// links it as a provider (agent-move) once the move proceeds.
func TestReincarnateMove_UserMoveLinksTargetProvider(t *testing.T) {
	f := setupMoveFixture(t, false, func(dst *store.RuntimeBroker) { dst.AutoProvide = true })
	prepareMoveWorkerFixture(t, f)

	rec := doRequest(t, f.srv, http.MethodPost, "/api/v1/agents/"+f.agent.ID+"/reincarnate",
		ReincarnateAgentRequest{TargetBroker: f.dst.ID})
	require.Equal(t, http.StatusAccepted, rec.Code, rec.Body.String())
	r := waitForReincarnationSettled(t, f.s, f.agent.ID)
	require.Equal(t, store.AgentReincarnationStateCompleted, r.State, r.Error)
	p, err := f.s.GetProjectProvider(context.Background(), f.project.ID, f.dst.ID)
	require.NoError(t, err)
	assert.Equal(t, "agent-move", p.LinkedBy)
}

// A self-move to a broker not serving the project is refused with 409 on
// a real request too: no record, no provider link, nothing dispatched.
func TestReincarnateMove_SelfMoveRealToNonServingRefused(t *testing.T) {
	f := setupMoveFixture(t, false, func(dst *store.RuntimeBroker) { dst.AutoProvide = true })
	count := f.agentCount(t)
	rec := f.reincarnate(t, ReincarnateAgentRequest{Handoff: "h", TargetBroker: f.dst.ID})
	require.Equal(t, http.StatusConflict, rec.Code, rec.Body.String())
	_, err := f.s.GetProjectProvider(context.Background(), f.project.ID, f.dst.ID)
	assert.ErrorIs(t, err, store.ErrNotFound, "no provider link")
	f.assertNoMoveSideEffects(t, count)
	provisions, deletes, starts := f.disp.moveSnapshot()
	assert.Empty(t, provisions)
	assert.Empty(t, deletes)
	assert.Empty(t, starts)
}

// A passthrough agent's real self-move is refused with 403.
func TestReincarnateMove_SelfMoveRealPassthroughRefused(t *testing.T) {
	f := setupMoveFixture(t, true, nil)
	ctx := context.Background()
	a, err := f.s.GetAgent(ctx, f.agent.ID)
	require.NoError(t, err)
	a.AppliedConfig.GCPIdentity = &store.GCPIdentityConfig{MetadataMode: store.GCPMetadataModePassthrough}
	require.NoError(t, f.s.UpdateAgent(ctx, a))
	f.agent = a
	count := f.agentCount(t)

	rec := f.reincarnate(t, ReincarnateAgentRequest{Handoff: "h", TargetBroker: f.dst.ID})
	require.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
	_, msg, _ := decodeMoveRefusal(t, rec)
	assert.Equal(t, "this agent cannot move itself; ask a user to move you", msg)
	f.assertNoMoveSideEffects(t, count)
}

// moveBrokerQuota: when the target has no room, the source reservation is
// restored and the error returned.
func TestMoveBrokerQuota_TargetFullRestoresSource(t *testing.T) {
	f := setupMoveFixture(t, true, nil)
	setBrokerAgentCeiling(t, f.s, 1)
	reserveBrokerSlot(t, f.s, f.src, f.agent.ID)
	reserveBrokerSlot(t, f.s, f.dst, tid("occupant"))

	err := f.srv.moveBrokerQuota(context.Background(), f.agent, &reincarnationMove{SourceBrokerID: f.src.ID, TargetBrokerID: f.dst.ID})
	require.Error(t, err)
	assert.EqualValues(t, 1, brokerReservationCount(t, f.s, f.src.ID), "the source reservation is restored")
	assert.EqualValues(t, 1, brokerReservationCount(t, f.s, f.dst.ID), "only the occupant")
}

// The worker re-checks eligibility before its first side effect: a target
// that stopped advertising AgentMove since the request is refused.
func TestRecheckMoveEligibility_RefusesChangedFacts(t *testing.T) {
	f := setupMoveFixture(t, true, nil)
	mv := &reincarnationMove{SourceBrokerID: f.src.ID, TargetBrokerID: f.dst.ID, ProjectID: f.project.ID, SelfMove: true, AgentDirWorkspace: true}
	require.NoError(t, f.srv.recheckMoveEligibility(context.Background(), f.agent.ID, mv))

	f.dst.Capabilities = &store.BrokerCapabilities{Reprovision: true}
	require.NoError(t, f.s.UpdateRuntimeBroker(context.Background(), f.dst))
	err := f.srv.recheckMoveEligibility(context.Background(), f.agent.ID, mv)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "does not support agent move")
}

// A4: an empty-per-agent agent with placement export on the same export
// may move (its own agent directory is expected on the target); one whose
// placement is local keeps the 400.
func TestReincarnateMove_EmptyPerAgent(t *testing.T) {
	makeEmptyPerAgent := func(t *testing.T, f *moveFixture) {
		t.Helper()
		ctx := context.Background()
		p, err := f.s.GetProject(ctx, f.project.ID)
		require.NoError(t, err)
		p.GitRemote = ""
		if p.Labels == nil {
			p.Labels = map[string]string{}
		}
		p.Labels[store.LabelWorkspaceMode] = string(store.SharingModeEmptyPerAgent)
		require.NoError(t, f.s.UpdateProject(ctx, p))
		require.True(t, p.IsEmptyPerAgent())
		a, err := f.s.GetAgent(ctx, f.agent.ID)
		require.NoError(t, err)
		a.AppliedConfig.GitClone = nil
		a.AppliedConfig.Workspace = ""
		require.NoError(t, f.s.UpdateAgent(ctx, a))
		f.agent = a
	}

	t.Run("placement export moves", func(t *testing.T) {
		f := setupMoveFixture(t, true, nil)
		makeEmptyPerAgent(t, f)
		prepareMoveWorkerFixture(t, f)
		r, a := startMove(t, f)
		require.Equal(t, store.AgentReincarnationStateCompleted, r.State, r.Error)
		assert.Equal(t, f.dst.ID, a.RuntimeBrokerID)
		provisions, _, _ := f.disp.moveSnapshot()
		assert.Equal(t, []string{f.dst.ID + "|agent-dir"}, provisions)
	})
	t.Run("different export identity keeps the 400", func(t *testing.T) {
		f := setupMoveFixture(t, true, func(dst *store.RuntimeBroker) {
			dst.WorkspaceStorage.NFS.ExportID = "0a0a0a0a-0000-4000-8000-000000000000"
		})
		makeEmptyPerAgent(t, f)
		count := f.agentCount(t)
		rec := f.reincarnate(t, ReincarnateAgentRequest{Handoff: "h", TargetBroker: f.dst.ID})
		require.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
		_, msg, v := decodeMoveRefusal(t, rec)
		assert.Contains(t, msg, "empty-per-agent")
		assertVerdictFailedAt(t, v, moveCheckWorkspaceMode)
		f.assertNoMoveSideEffects(t, count)
	})
	t.Run("placement local keeps the 400", func(t *testing.T) {
		f := setupMoveFixture(t, true, nil)
		makeEmptyPerAgent(t, f)
		require.NoError(t, f.s.SetAgentWorkspacePlacement(context.Background(), f.agent.ID, api.WorkspacePlacementLocal))
		count := f.agentCount(t)
		rec := f.reincarnate(t, ReincarnateAgentRequest{Handoff: "h", TargetBroker: f.dst.ID})
		require.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
		_, msg, v := decodeMoveRefusal(t, rec)
		assert.Contains(t, msg, "empty-per-agent")
		assertVerdictFailedAt(t, v, moveCheckWorkspaceMode)
		f.assertNoMoveSideEffects(t, count)
	})
}

// A self-move never links a provider, even should its target not serve the
// project by the time the worker runs (defense in depth behind the A8
// check).
func TestLinkMoveTargetProvider_SelfMoveNeverLinks(t *testing.T) {
	f := setupMoveFixture(t, false, nil)
	ctx := context.Background()
	mv := &reincarnationMove{SourceBrokerID: f.src.ID, TargetBrokerID: f.dst.ID, ProjectID: f.project.ID, SelfMove: true}
	require.NoError(t, f.srv.linkMoveTargetProvider(ctx, mv))
	_, err := f.s.GetProjectProvider(ctx, f.project.ID, f.dst.ID)
	assert.ErrorIs(t, err, store.ErrNotFound, "a self-move must not link the target")

	mv.SelfMove = false
	require.NoError(t, f.srv.linkMoveTargetProvider(ctx, mv))
	p, err := f.s.GetProjectProvider(ctx, f.project.ID, f.dst.ID)
	require.NoError(t, err)
	assert.Equal(t, "agent-move", p.LinkedBy)
}

// The worker re-checks eligibility before its first side effect: when the
// facts changed after the request was accepted (here the target stopped
// advertising AgentMove), the move fails from pending without stopping or
// touching the agent anywhere.
func TestReincarnationWorker_MoveRecheckFailsBeforeAnySideEffect(t *testing.T) {
	f := setupMoveFixture(t, true, nil)
	prepareMoveWorkerFixture(t, f)
	ctx := context.Background()

	f.dst.Capabilities = &store.BrokerCapabilities{Reprovision: true}
	require.NoError(t, f.s.UpdateRuntimeBroker(ctx, f.dst))
	a, err := f.s.GetAgent(ctx, f.agent.ID)
	require.NoError(t, err)
	a.ReincarnationState = store.ReincarnationStatePending
	require.NoError(t, f.s.UpdateAgent(ctx, a))
	rec := &store.AgentReincarnation{
		AgentID: a.ID, FromGeneration: 1, ToGeneration: 2, State: store.AgentReincarnationStatePending,
		PreviousAppliedConfig: a.AppliedConfig, SourceBrokerID: f.src.ID, TargetBrokerID: f.dst.ID,
	}
	require.NoError(t, f.s.CreateAgentReincarnation(ctx, rec))
	fresh := *a.AppliedConfig
	mv := &reincarnationMove{SourceBrokerID: f.src.ID, TargetBrokerID: f.dst.ID, ProjectID: f.project.ID, SelfMove: true, AgentDirWorkspace: true}

	f.srv.runReincarnationWorker(ctx, a.ID, rec.ID, a.AppliedConfig, &fresh, "h", time.Now(), "", &ReincarnationPlan{}, 2, a.DeletionClaim, mv)

	got, err := f.s.GetAgentReincarnation(ctx, rec.ID)
	require.NoError(t, err)
	assert.Equal(t, store.AgentReincarnationStateFailed, got.State)
	assert.Contains(t, got.Error, "does not support agent move")
	f.disp.mu.Lock()
	assert.Zero(t, f.disp.stopCalls, "nothing is stopped")
	f.disp.mu.Unlock()
	provisions, deletes, starts := f.disp.moveSnapshot()
	assert.Empty(t, provisions)
	assert.Empty(t, deletes)
	assert.Empty(t, starts)
	after, err := f.s.GetAgent(ctx, a.ID)
	require.NoError(t, err)
	assert.Equal(t, f.src.ID, after.RuntimeBrokerID)
	assert.EqualValues(t, 1, brokerReservationCount(t, f.s, f.src.ID))
}
