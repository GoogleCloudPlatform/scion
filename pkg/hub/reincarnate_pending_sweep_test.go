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
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Tests for the pending-only staleness bound of the reincarnation sweep
// (ptone/scion#3986): the patched config lives only in the worker's memory,
// so a claim whose worker never ran (the hub restarted between the claim and
// the worker) must fail as retryable, with nothing applied.

// claimReincarnationWithoutWorker commits a reincarnation claim exactly as
// startReincarnation does (claim and record in one transaction) but starts
// no worker, as when the hub restarts right after the claim. The claim and
// the record are dated age ago.
func claimReincarnationWithoutWorker(t *testing.T, srv *Server, s store.Store, agentID string, age time.Duration) *store.AgentReincarnation {
	t.Helper()
	ctx := context.Background()
	a, err := s.GetAgent(ctx, agentID)
	require.NoError(t, err)
	claimedAt := time.Now().Add(-age).Truncate(time.Microsecond)
	a.ReincarnationState = store.ReincarnationStatePending
	a.ReincarnationUpdatedAt = &claimedAt
	rec := &store.AgentReincarnation{
		AgentID:               a.ID,
		FromGeneration:        a.Generation,
		ToGeneration:          a.Generation + 1,
		State:                 store.AgentReincarnationStatePending,
		PreviousAppliedConfig: a.AppliedConfig,
	}
	require.NoError(t, srv.reincarnateClaimTx(ctx, a, rec, nil, AuditActor{}))
	if age > 0 {
		// The record's updated_at defaults to the commit time; date it back
		// to the claim time without changing its state.
		got, err := s.GetAgentReincarnation(ctx, rec.ID)
		require.NoError(t, err)
		got.UpdatedAt = claimedAt
		ok, err := s.TryAdvanceAgentReincarnation(ctx, got, store.AgentReincarnationStatePending, time.Time{})
		require.NoError(t, err)
		require.True(t, ok)
	}
	got, err := s.GetAgentReincarnation(ctx, rec.ID)
	require.NoError(t, err)
	return got
}

func dispatcherCalls(d *reincarnateTestDispatcher) (stops, reprovisions, starts int) {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.stopCalls, d.reprovisionCalls, d.startCalls
}

// TestSweepStaleReincarnations_PendingWithoutWorkerFailsRetryable: a claim
// left pending by a restart, past the pending bound but well inside the
// general 30-minute bound, is failed by the production sweep with a clear
// reason, nothing applied; a re-submitted reincarnation then succeeds with
// its patch.
func TestSweepStaleReincarnations_PendingWithoutWorkerFailsRetryable(t *testing.T) {
	ctx := context.Background()
	disp := newReincarnateTestDispatcher()
	srv, s, project, broker := setupReincarnateTestServer(t, disp)
	agent := newReincarnateTestAgent(t, s, project, broker, nil)
	before := snapshotAgent(t, s, agent.ID)

	claimed := claimReincarnationWithoutWorker(t, srv, s, agent.ID, 5*time.Minute)
	require.Less(t, reincarnationPendingStaleAfter, 5*time.Minute)
	require.Greater(t, reincarnationStaleAfter, 5*time.Minute)

	// While the abandoned claim stands, a new request is refused.
	rec := reincarnateAsDev(t, srv, agent.ID, ReincarnateAgentRequest{Image: "patched-image:v9"})
	require.Equal(t, http.StatusConflict, rec.Code, rec.Body.String())

	n, err := srv.sweepStaleReincarnations(ctx)
	require.NoError(t, err)
	assert.Equal(t, 1, n)

	after, err := s.GetAgent(ctx, agent.ID)
	require.NoError(t, err)
	assert.Equal(t, store.ReincarnationStateFailed, after.ReincarnationState)
	assert.Equal(t, "error", after.Phase)
	assert.Equal(t, "reincarnation failed: "+reincarnationDidNotStartReason, after.Message)
	assert.Equal(t, before.generation, after.Generation, "generation must not change")
	afterSnap := snapshotAgent(t, s, agent.ID)
	assert.JSONEq(t, string(before.applied), string(afterSnap.applied), "no part of the patch may be applied")
	stops, reprovisions, starts := dispatcherCalls(disp)
	assert.Zero(t, stops, "the agent must not be stopped")
	assert.Zero(t, reprovisions)
	assert.Zero(t, starts)

	got, err := s.GetAgentReincarnation(ctx, claimed.ID)
	require.NoError(t, err)
	assert.Equal(t, store.AgentReincarnationStateFailed, got.State)
	assert.Equal(t, reincarnationDidNotStartReason, got.Error)

	// Retryable: the same request is accepted now and applies its patch.
	rec = reincarnateAsDev(t, srv, agent.ID, ReincarnateAgentRequest{Image: "patched-image:v9"})
	require.Equal(t, http.StatusAccepted, rec.Code, rec.Body.String())
	settled := waitForReincarnationSettled(t, s, agent.ID)
	assert.Equal(t, store.AgentReincarnationStateCompleted, settled.State, settled.Error)
	final, err := s.GetAgent(ctx, agent.ID)
	require.NoError(t, err)
	assert.Equal(t, store.ReincarnationStateNone, final.ReincarnationState)
	assert.Equal(t, "patched-image:v9", final.AppliedConfig.Image)
	assert.Equal(t, before.generation+1, final.Generation)
}

// TestSweepStaleReincarnations_FreshPendingUntouched: a pending claim
// younger than the pending bound is left alone by the production sweep.
func TestSweepStaleReincarnations_FreshPendingUntouched(t *testing.T) {
	ctx := context.Background()
	disp := newReincarnateTestDispatcher()
	srv, s, project, broker := setupReincarnateTestServer(t, disp)
	agent := newReincarnateTestAgent(t, s, project, broker, nil)

	claimed := claimReincarnationWithoutWorker(t, srv, s, agent.ID, 0)

	n, err := srv.sweepStaleReincarnations(ctx)
	require.NoError(t, err)
	assert.Zero(t, n)

	after, err := s.GetAgent(ctx, agent.ID)
	require.NoError(t, err)
	assert.Equal(t, store.ReincarnationStatePending, after.ReincarnationState)
	got, err := s.GetAgentReincarnation(ctx, claimed.ID)
	require.NoError(t, err)
	assert.Equal(t, store.AgentReincarnationStatePending, got.State)
}

// TestSweepStaleReincarnations_PendingBoundSparesWorkerPastPending: a live
// worker that has taken its first step (pending->stopping) is not touched
// by the pending bound, even with a pending cutoff that treats every
// pending row as stale, and goes on to apply its patch.
func TestSweepStaleReincarnations_PendingBoundSparesWorkerPastPending(t *testing.T) {
	ctx := context.Background()
	base := newReincarnateTestDispatcher()
	disp := &blockingStopDispatcher{reincarnateTestDispatcher: base, entered: make(chan struct{}), release: make(chan struct{})}
	srv, s, project, broker := setupReincarnateTestServer(t, disp)
	agent := newReincarnateTestAgent(t, s, project, broker, nil)

	rec := reincarnateAsDev(t, srv, agent.ID, ReincarnateAgentRequest{Image: "patched-image:v9"})
	require.Equal(t, http.StatusAccepted, rec.Code, rec.Body.String())
	<-disp.entered // the worker won pending->stopping and is inside Stop

	now := time.Now()
	n, err := srv.sweepStaleReincarnationsWithCutoffs(ctx, now.Add(-reincarnationStaleAfter), now.Add(time.Hour))
	close(disp.release)
	require.NoError(t, err)
	assert.Zero(t, n, "a record past pending must be held to the general bound")

	settled := waitForReincarnationSettled(t, s, agent.ID)
	assert.Equal(t, store.AgentReincarnationStateCompleted, settled.State, settled.Error)
	final, err := s.GetAgent(ctx, agent.ID)
	require.NoError(t, err)
	assert.Equal(t, "patched-image:v9", final.AppliedConfig.Image)
}

// TestSweepStaleReincarnations_WorkerLosingPendingCASWritesNothing: when
// the sweep fails a pending record before its worker takes its first step
// (a false positive on a slow worker), the worker loses its pending->stopping
// CAS and returns without touching the agent, the record or the broker.
func TestSweepStaleReincarnations_WorkerLosingPendingCASWritesNothing(t *testing.T) {
	ctx := context.Background()
	disp := newReincarnateTestDispatcher()
	srv, s, project, broker := setupReincarnateTestServer(t, disp)
	agent := newReincarnateTestAgent(t, s, project, broker, nil)

	claimed := claimReincarnationWithoutWorker(t, srv, s, agent.ID, 0)
	now := time.Now()
	n, err := srv.sweepStaleReincarnationsWithCutoffs(ctx, now.Add(-reincarnationStaleAfter), now.Add(time.Hour))
	require.NoError(t, err)
	require.Equal(t, 1, n)

	swept := snapshotAgent(t, s, agent.ID)
	sweptAgent, err := s.GetAgent(ctx, agent.ID)
	require.NoError(t, err)
	sweptRec, err := s.GetAgentReincarnation(ctx, claimed.ID)
	require.NoError(t, err)
	require.Equal(t, store.AgentReincarnationStateFailed, sweptRec.State)

	fresh := *claimed.PreviousAppliedConfig
	fresh.Image = "patched-image:v9"
	srv.runReincarnationWorker(ctx, agent.ID, claimed.ID, claimed.PreviousAppliedConfig, &fresh, "",
		time.Now(), "", &ReincarnationPlan{}, claimed.ToGeneration, 0, nil)

	after := snapshotAgent(t, s, agent.ID)
	assert.Equal(t, swept, after, "the losing worker must not write the agent")
	afterAgent, err := s.GetAgent(ctx, agent.ID)
	require.NoError(t, err)
	assert.Equal(t, sweptAgent.Message, afterAgent.Message)
	afterRec, err := s.GetAgentReincarnation(ctx, claimed.ID)
	require.NoError(t, err)
	assert.Equal(t, sweptRec.State, afterRec.State)
	assert.Equal(t, sweptRec.Error, afterRec.Error)
	assert.True(t, sweptRec.UpdatedAt.Equal(afterRec.UpdatedAt), "the losing worker must not write the record")
	stops, reprovisions, starts := dispatcherCalls(disp)
	assert.Zero(t, stops)
	assert.Zero(t, reprovisions)
	assert.Zero(t, starts)
}

// TestSweepStaleReincarnations_OrphanPendingUsesPendingBound: an agent left
// pending with no matching record is reset under the pending bound too.
func TestSweepStaleReincarnations_OrphanPendingUsesPendingBound(t *testing.T) {
	ctx := context.Background()
	disp := newReincarnateTestDispatcher()
	srv, s, project, broker := setupReincarnateTestServer(t, disp)
	agent := newReincarnateTestAgent(t, s, project, broker, nil)
	agent.ReincarnationState = store.ReincarnationStatePending
	claimedAt := time.Now()
	agent.ReincarnationUpdatedAt = &claimedAt
	require.NoError(t, s.UpdateAgent(ctx, agent))

	n, err := srv.sweepStaleReincarnations(ctx)
	require.NoError(t, err)
	assert.Zero(t, n, "a fresh orphan must not be swept")

	agent, err = s.GetAgent(ctx, agent.ID)
	require.NoError(t, err)
	claimedAt = time.Now().Add(-5 * time.Minute)
	agent.ReincarnationUpdatedAt = &claimedAt
	require.NoError(t, s.UpdateAgent(ctx, agent))

	n, err = srv.sweepStaleReincarnations(ctx)
	require.NoError(t, err)
	assert.Equal(t, 1, n)
	after, err := s.GetAgent(ctx, agent.ID)
	require.NoError(t, err)
	assert.Equal(t, store.ReincarnationStateFailed, after.ReincarnationState)
	assert.Contains(t, after.Message, reincarnationDidNotStartReason)
}
