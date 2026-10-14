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
	"sync"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The purge job asks each expired soft-deleted agent's broker to remove the
// files the soft delete retained, and purges the record only once that is
// settled (ptone/scion#4744).

const retainedPurgeTestRetention = time.Hour

// retainedPurgeFixture is a hub with a 1h soft-delete retention and one
// agent on an online broker, soft-deleted deletedAgo ago.
func retainedPurgeFixture(t *testing.T, suffix string, deletedAgo time.Duration) (*Server, store.Store, *engineStubDispatcher, *store.Agent) {
	t.Helper()
	srv, s, _, disp := engineTestServer(t)
	srv.mu.Lock()
	srv.config.SoftDeleteRetention = retainedPurgeTestRetention
	srv.mu.Unlock()
	a := addRetainedPurgeAgent(t, s, suffix, deletedAgo)
	return srv, s, disp, a
}

// addRetainedPurgeAgent adds an agent of run "run-1" on its own online
// broker, soft-deleted deletedAgo ago.
func addRetainedPurgeAgent(t *testing.T, s store.Store, suffix string, deletedAgo time.Duration) *store.Agent {
	t.Helper()
	ctx := context.Background()
	a := setupBrokerAgentInPhase(t, s, "rp-"+suffix, state.PhaseStopped)
	_, err := s.SetAgentRunID(ctx, a.ID, "run-1", nil)
	require.NoError(t, err)
	a = mustGetAgent(t, s, a.ID)
	a.DeletedAt = time.Now().Add(-deletedAgo)
	require.NoError(t, s.UpdateAgent(ctx, a))
	got := mustGetAgent(t, s, a.ID)
	require.Equal(t, "run-1", got.RunID)
	return got
}

// setBrokerStatus sets the agent's broker status.
func setBrokerStatus(t *testing.T, s store.Store, brokerID, status string) {
	t.Helper()
	b, err := s.GetRuntimeBroker(context.Background(), brokerID)
	require.NoError(t, err)
	b.Status = status
	require.NoError(t, s.UpdateRuntimeBroker(context.Background(), b))
}

// assertUnclaimed checks the row carries no purge claim.
func assertUnclaimed(t *testing.T, s store.Store, id string) {
	t.Helper()
	got := mustGetAgent(t, s, id)
	assert.Equal(t, store.DeletionStateNone, got.DeletionState, "the purge claim was released")
	assert.Nil(t, got.DeletionLeaseAt)
}

// An expired agent's broker gets a purgeRetained delete scoped to the
// agent's project; while it runs, the row is claimed and a restore is
// refused; once the broker answers, the record is purged.
func TestPurgeExpiredAgents_RemovesRetainedFilesThenPurges(t *testing.T) {
	srv, s, disp, a := retainedPurgeFixture(t, "ok", 2*time.Hour)
	var mu sync.Mutex
	var sawPurgeRetained, sawProject, heldDuringDispatch bool
	var restoreCode int
	disp.setFn(func(ctx context.Context, got *store.Agent) error {
		mu.Lock()
		defer mu.Unlock()
		sawPurgeRetained = purgeRetainedFrom(ctx)
		sawProject = got.ProjectID == a.ProjectID && got.ProjectID != ""
		row := mustGetAgent(t, s, a.ID)
		heldDuringDispatch = row.DeletionHoldsRow(time.Now())
		restoreCode = restoreForTest(t, srv, a.ID).Code
		return nil
	})

	srv.purgeHandler()(context.Background())

	mu.Lock()
	defer mu.Unlock()
	assert.Equal(t, 1, disp.callCount(), "the broker got the purge dispatch")
	assert.True(t, sawPurgeRetained, "the dispatch is marked purgeRetained")
	assert.True(t, sawProject, "the dispatch carries the agent's project")
	assert.True(t, heldDuringDispatch, "the row is claimed while the broker removes the files")
	assert.Equal(t, http.StatusConflict, restoreCode, "a restore is refused during the dispatch")
	assert.True(t, agentGone(t, s, a.ID), "the record is purged after the broker answered")
	deleteFiles, removeBranch := disp.deleteFlags()
	assert.Equal(t, []bool{true}, deleteFiles, "the purge is a files delete")
	assert.Equal(t, []bool{false}, removeBranch, "the purge keeps the agent's branch")
}

// Each record is purged right after its broker answers, while its claim is
// live, not after the rest of the page: a claim that lapses later (here,
// during the next agent's dispatch) can no longer let a restore bring back
// an agent whose files were removed.
func TestPurgeExpiredAgents_PurgesEachRecordWhileClaimed(t *testing.T) {
	srv, s, disp, first := retainedPurgeFixture(t, "page-a", 2*time.Hour)
	second := addRetainedPurgeAgent(t, s, "page-b", 2*time.Hour)
	if second.ID < first.ID {
		first, second = second, first // the purge settles agents in ID order
	}
	var mu sync.Mutex
	var firstGoneDuringSecond, restoredWithoutFiles bool
	disp.setFn(func(ctx context.Context, got *store.Agent) error {
		if got.ID != second.ID {
			return nil
		}
		mu.Lock()
		defer mu.Unlock()
		// Lapse the first agent's claim, if its row is still there, and try
		// to restore it.
		past := time.Now().Add(-time.Minute)
		_, err := s.UpdateAgentDeletion(context.Background(), first.ID,
			store.DeletionPredicate{States: []string{store.DeletionStateDeleting}},
			store.DeletionFields{LeaseAt: &past, KeepUpdated: true})
		require.NoError(t, err)
		firstGoneDuringSecond = agentGone(t, s, first.ID)
		rec := restoreForTest(t, srv, first.ID)
		restoredWithoutFiles = rec.Code < 300
		return nil
	})

	srv.purgeHandler()(context.Background())

	mu.Lock()
	defer mu.Unlock()
	assert.Equal(t, 2, disp.callCount())
	assert.True(t, firstGoneDuringSecond, "the first record is purged before the next dispatch")
	assert.False(t, restoredWithoutFiles, "no agent is restored after its files were removed")
	assert.True(t, agentGone(t, s, first.ID))
	assert.True(t, agentGone(t, s, second.ID))
}

// A row with no run ID (created before run IDs) is purged without a
// dispatch: its delete could not be fenced to its run.
func TestPurgeExpiredAgents_NoRunIDPurgesWithoutDispatch(t *testing.T) {
	srv, s, disp, _ := retainedPurgeFixture(t, "norun-x", 2*time.Hour)
	legacy := setupBrokerAgentInPhase(t, s, "rp-norun", state.PhaseStopped)
	legacy.DeletedAt = time.Now().Add(-2 * time.Hour)
	require.NoError(t, s.UpdateAgent(context.Background(), legacy))
	require.Empty(t, mustGetAgent(t, s, legacy.ID).RunID)

	var dispatched []string
	var mu sync.Mutex
	disp.setFn(func(_ context.Context, got *store.Agent) error {
		mu.Lock()
		dispatched = append(dispatched, got.ID)
		mu.Unlock()
		return nil
	})
	srv.purgeHandler()(context.Background())
	mu.Lock()
	defer mu.Unlock()
	assert.NotContains(t, dispatched, legacy.ID, "a row with no run ID gets no purge dispatch")
	assert.True(t, agentGone(t, s, legacy.ID), "and is purged")
}

// A soft-deleted row left finalizing (a permanent delete that stopped after
// its teardown) cannot be claimed; it is purged without a dispatch, as
// before, rather than kept forever.
func TestPurgeExpiredAgents_FinalizingRowPurgesWithoutDispatch(t *testing.T) {
	srv, s, disp, a := retainedPurgeFixture(t, "finalizing", 2*time.Hour)
	finalizing := store.DeletionStateFinalizing
	n, err := s.UpdateAgentDeletion(context.Background(), a.ID,
		store.DeletionPredicate{States: []string{store.DeletionStateNone}, DeletedAtNotNull: true},
		store.DeletionFields{State: &finalizing, BumpClaim: true})
	require.NoError(t, err)
	require.Equal(t, 1, n)

	srv.purgeHandler()(context.Background())
	assert.Equal(t, 0, disp.callCount())
	assert.True(t, agentGone(t, s, a.ID))
}

// An agent still inside the retention window is neither dispatched nor
// purged.
func TestPurgeExpiredAgents_KeepsAgentInsideWindow(t *testing.T) {
	srv, s, disp, a := retainedPurgeFixture(t, "window", 10*time.Minute)
	srv.purgeHandler()(context.Background())
	assert.Equal(t, 0, disp.callCount())
	assert.False(t, agentGone(t, s, a.ID))
}

// A broker that is registered but offline keeps the record, with nothing
// dispatched and no claim left; once it is back, the next run settles it.
func TestPurgeExpiredAgents_OfflineBrokerRetries(t *testing.T) {
	srv, s, disp, a := retainedPurgeFixture(t, "offline", 2*time.Hour)
	setBrokerStatus(t, s, a.RuntimeBrokerID, store.BrokerStatusOffline)

	srv.purgeHandler()(context.Background())
	assert.Equal(t, 0, disp.callCount(), "nothing is dispatched to an offline broker")
	assert.False(t, agentGone(t, s, a.ID), "the record is kept for a later run")
	assertUnclaimed(t, s, a.ID)

	setBrokerStatus(t, s, a.RuntimeBrokerID, store.BrokerStatusOnline)
	srv.purgeHandler()(context.Background())
	assert.Equal(t, 1, disp.callCount())
	assert.True(t, agentGone(t, s, a.ID))
}

// A failed dispatch (here the broker unreachable) keeps the record and
// releases the claim, so a restore is possible again; the next run retries.
func TestPurgeExpiredAgents_FailedDispatchKeepsRecordAndReleasesClaim(t *testing.T) {
	srv, s, disp, a := retainedPurgeFixture(t, "fail", 2*time.Hour)
	disp.setFn(func(context.Context, *store.Agent) error {
		return errors.New("failed to send request: connection refused")
	})

	srv.purgeHandler()(context.Background())
	assert.Equal(t, 1, disp.callCount())
	assert.False(t, agentGone(t, s, a.ID))
	assertUnclaimed(t, s, a.ID)
	blocked, err := srv.deleteBlocksStart(context.Background(), mustGetAgent(t, s, a.ID))
	require.NoError(t, err)
	assert.False(t, blocked, "a restore is not blocked after a failed dispatch")

	disp.setFn(nil)
	srv.purgeHandler()(context.Background())
	assert.Equal(t, 2, disp.callCount())
	assert.True(t, agentGone(t, s, a.ID))
}

// A broker that no longer exists cannot be asked: the record is purged and
// the retained files are reported left.
func TestPurgeExpiredAgents_DeletedBrokerPurges(t *testing.T) {
	srv, s, disp, a := retainedPurgeFixture(t, "gone", 2*time.Hour)
	require.NoError(t, s.DeleteRuntimeBroker(context.Background(), a.RuntimeBrokerID))

	srv.purgeHandler()(context.Background())
	assert.Equal(t, 0, disp.callCount())
	assert.True(t, agentGone(t, s, a.ID))
}

// An agent with no broker has no retained files to remove.
func TestPurgeExpiredAgents_NoBrokerPurges(t *testing.T) {
	srv, s, disp, a := retainedPurgeFixture(t, "nobroker", 2*time.Hour)
	a.RuntimeBrokerID = ""
	require.NoError(t, s.UpdateAgent(context.Background(), a))

	srv.purgeHandler()(context.Background())
	assert.Equal(t, 0, disp.callCount())
	assert.True(t, agentGone(t, s, a.ID))
}

// The bound: a record whose broker stays unreachable is kept until
// retainedPurgeGiveUp past the window, then purged.
func TestPurgeExpiredAgents_GiveUpBound(t *testing.T) {
	for _, tc := range []struct {
		name  string
		ago   time.Duration
		purge bool
	}{
		{name: "inside the bound", ago: retainedPurgeTestRetention + retainedPurgeGiveUp - time.Hour, purge: false},
		{name: "past the bound", ago: retainedPurgeTestRetention + retainedPurgeGiveUp + time.Hour, purge: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv, s, disp, a := retainedPurgeFixture(t, "bound-"+map[bool]string{true: "past", false: "in"}[tc.purge], tc.ago)
			setBrokerStatus(t, s, a.RuntimeBrokerID, store.BrokerStatusOffline)
			srv.purgeHandler()(context.Background())
			assert.Equal(t, 0, disp.callCount())
			assert.Equal(t, tc.purge, agentGone(t, s, a.ID))
		})
	}
}

// Broker answers that settle the record although nothing was removed.
func TestPurgeExpiredAgents_BrokerRefusalsPurge(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
	}{
		{name: "retained files refused", err: &brokerStatusError{StatusCode: http.StatusConflict,
			Body: `{"error":{"code":"retained_files_refused","message":"Retained files not removed: the agent's files carry no soft-delete mark","details":{"reason":"the agent's files carry no soft-delete mark"}}}`}},
		{name: "invalid request", err: &brokerStatusError{StatusCode: http.StatusBadRequest,
			Body: `{"error":{"code":"validation_error","message":"purgeRetained requires projectId"}}`}},
		{name: "another run holds the name", err: &DeleteRunMismatchError{RequestedRunID: "run-a", CurrentRunID: "run-b"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv, s, disp, a := retainedPurgeFixture(t, "refused-"+tc.name[:7], 2*time.Hour)
			disp.setFn(func(context.Context, *store.Agent) error { return tc.err })
			srv.purgeHandler()(context.Background())
			assert.Equal(t, 1, disp.callCount())
			assert.True(t, agentGone(t, s, a.ID))
		})
	}
}

// A restore that lands before the purge claims the row wins: the claim
// matches nothing and the agent is neither dispatched nor purged.
func TestPurgeExpiredAgents_RestoreBeforeClaimWins(t *testing.T) {
	srv, s, disp, a := retainedPurgeFixture(t, "restore", 2*time.Hour)
	rec := restoreForTest(t, srv, a.ID)
	require.Less(t, rec.Code, 300, rec.Body.String())

	_, ok := srv.claimForRetainedPurge(context.Background(), a.ID)
	assert.False(t, ok, "a restored row cannot be claimed")
	srv.purgeHandler()(context.Background())
	assert.Equal(t, 0, disp.callCount())
	assert.False(t, agentGone(t, s, a.ID))
	assert.True(t, mustGetAgent(t, s, a.ID).DeletedAt.IsZero())
}

// A hub that stops mid-dispatch leaves a claim that lapses with its lease:
// it then blocks no restore, the restore clears it, and an unrestored row
// is re-claimed and purged by the next run.
func TestPurgeExpiredAgents_LapsedClaim(t *testing.T) {
	lapse := func(t *testing.T, s store.Store, id string) {
		t.Helper()
		past := time.Now().Add(-time.Minute)
		deleting := store.DeletionStateDeleting
		n, err := s.UpdateAgentDeletion(context.Background(), id,
			store.DeletionPredicate{States: []string{store.DeletionStateNone}, DeletedAtNotNull: true},
			store.DeletionFields{State: &deleting, BumpClaim: true, LeaseAt: &past})
		require.NoError(t, err)
		require.Equal(t, 1, n)
	}

	t.Run("restore is allowed and clears it", func(t *testing.T) {
		srv, s, _, a := retainedPurgeFixture(t, "lapsed-restore", 2*time.Hour)
		lapse(t, s, a.ID)
		blocked, err := srv.deleteBlocksStart(context.Background(), mustGetAgent(t, s, a.ID))
		require.NoError(t, err)
		assert.False(t, blocked, "a lapsed claim does not block a restore")
		rec := restoreForTest(t, srv, a.ID)
		require.Less(t, rec.Code, 300, rec.Body.String())
		got := mustGetAgent(t, s, a.ID)
		assert.True(t, got.DeletedAt.IsZero())
		assert.Equal(t, store.DeletionStateNone, got.DeletionState, "the restored agent carries no lapsed claim")
	})
	t.Run("next run re-claims and purges", func(t *testing.T) {
		srv, s, disp, a := retainedPurgeFixture(t, "lapsed-rerun", 2*time.Hour)
		lapse(t, s, a.ID)
		srv.purgeHandler()(context.Background())
		assert.Equal(t, 1, disp.callCount())
		assert.True(t, agentGone(t, s, a.ID))
	})
}
