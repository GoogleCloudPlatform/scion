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
	"sync"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Tests for ptone/scion#2890: a re-claim of an abandoned finalizing row
// keeps it finalizing, so the row stays held by the delete even if the
// re-claiming engine dies as well, and the re-claim does not need a
// reachable broker (it skips the dispatch).

// requireCreateRollbackHeld asserts what the create rollback's held check
// (createRowHeldCheck) answers for the agent's current row.
func requireCreateRollbackHeld(t *testing.T, s store.Store, agentID string, want bool) {
	t.Helper()
	row := mustGetAgent(t, s, agentID)
	err := createRowHeldCheck(nil)(context.Background(), s, row, store.DeletionFinalizeHard)
	if want {
		assert.ErrorIs(t, err, errCreateRowDeleteHeld, "state %q lease %v: the create rollback must leave the row to the delete", row.DeletionState, row.DeletionLeaseAt)
	} else {
		assert.NoError(t, err, "state %q lease %v: the create rollback may remove the row", row.DeletionState, row.DeletionLeaseAt)
	}
}

// Pins the held check the create rollback uses: a finalizing row (live or
// lease-expired) and a live deleting row are held; a failed row or a
// lease-expired deleting row is not.
func TestCreateRollbackHeldCheck_DeleteStates(t *testing.T) {
	cases := []struct {
		seed deleteSeed
		held bool
	}{
		{seedLiveDeleting, true},
		{deleteSeed{name: "live finalizing", state: store.DeletionStateFinalizing, leaseIn: time.Minute}, true},
		{seedExpiredFinalize, true},
		{seedRevokeFailedFinl, true},
		{deleteSeed{name: "lease-expired deleting", state: store.DeletionStateDeleting, leaseIn: -time.Minute}, false},
		{deleteSeed{name: "failed", state: store.DeletionStateFailed, leaseIn: -time.Minute, code: store.DeletionCodeRuntimeError}, false},
	}
	for i, tc := range cases {
		t.Run(tc.seed.name, func(t *testing.T) {
			_, s := testServer(t)
			agent := setupBrokerAgentInPhase(t, s, "held-"+string(rune('a'+i)), state.PhaseStopped)
			seedAgentDeletion(t, s, agent.ID, tc.seed)
			requireCreateRollbackHeld(t, s, agent.ID, tc.held)
		})
	}
}

// A re-claim of an abandoned finalizing row keeps it finalizing and leaves
// the phase alone. If that engine dies too (its lease lapses), the row is
// still held: start stays refused and the create rollback leaves it to the
// delete. Before the fix the re-claim wrote deleting, so a second lapse left
// a live agent row that no delete held.
func TestDeleteReclaim_Finalizing_StaysFinalizing(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()
	agent := setupBrokerAgentInPhase(t, s, "reclaim-fin-hold", state.PhaseStopped)
	seedAgentDeletion(t, s, agent.ID, seedExpiredFinalize)

	plan, err := srv.claimAgentDeletion(ctx, agent.ID, agentDeleteParams{deleteFiles: true, removeBranch: true})
	require.NoError(t, err)
	require.NotNil(t, plan, "the abandoned finalizing row is re-claimed")
	assert.True(t, plan.skipDispatch)

	got := mustGetAgent(t, s, agent.ID)
	assert.Equal(t, store.DeletionStateFinalizing, got.DeletionState, "the re-claim keeps finalizing")
	assert.Equal(t, string(state.PhaseStopped), got.Phase, "the re-claim leaves the phase alone")
	assert.True(t, got.DeletionHoldsRow(time.Now()))
	requireCreateRollbackHeld(t, s, agent.ID, true)

	// The re-claiming engine dies: its lease lapses.
	past := time.Now().Add(-time.Minute)
	claim := plan.claim
	n, err := s.UpdateAgentDeletion(ctx, agent.ID, store.DeletionPredicate{Claim: &claim}, store.DeletionFields{LeaseAt: &past})
	require.NoError(t, err)
	require.Equal(t, 1, n)

	got = mustGetAgent(t, s, agent.ID)
	assert.True(t, got.DeletionHoldsRow(time.Now()), "an abandoned finalizing row still holds the row")
	requireCreateRollbackHeld(t, s, agent.ID, true)
	requireDeleteInProgress(t, doRequest(t, srv, http.MethodPost, "/api/v1/agents/"+agent.ID+"/start", nil))
}

// holdsRecordingStore records, after every deletion write that affected the
// row, whether the delete still holds it, and the answer of a start request
// sent at the first such write. It only records once its fault switch is
// armed; before that it is a plain pass-through.
type holdsRecordingStore struct {
	store.Store
	fault        *storeFaultSwitch
	onFirstWrite func()

	mu    sync.Mutex
	once  sync.Once
	holds []bool
}

func (s *holdsRecordingStore) UpdateAgentDeletion(ctx context.Context, id string, pred store.DeletionPredicate, set store.DeletionFields) (int, error) {
	n, err := s.Store.UpdateAgentDeletion(ctx, id, pred, set)
	if err != nil || n == 0 || !s.fault.Active() {
		return n, err
	}
	if row, gerr := s.GetAgent(ctx, id); gerr == nil && row.DeletedAt.IsZero() {
		s.mu.Lock()
		s.holds = append(s.holds, row.DeletionHoldsRow(time.Now()))
		s.mu.Unlock()
	}
	if s.onFirstWrite != nil {
		s.once.Do(s.onFirstWrite)
	}
	return n, err
}

func (s *holdsRecordingStore) recorded() []bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]bool(nil), s.holds...)
}

// A DELETE (no force) of an abandoned finalizing row whose broker is
// offline re-claims it and finishes: teardown already ran, so it needs no
// broker and is not refused with 503. The row is held by the delete
// throughout: start answers 409 before the re-claim and right after it.
func TestDeleteReclaim_Finalizing_BrokerOffline_NoForce(t *testing.T) {
	srv, base, pub, disp := engineTestServer(t)
	var path string
	startCode := 0
	rs, fault := installStoreFault(t, srv, func(inner store.Store, f *storeFaultSwitch) *holdsRecordingStore {
		rs := &holdsRecordingStore{Store: inner, fault: f}
		rs.onFirstWrite = func() {
			startCode = doRequest(t, srv, http.MethodPost, path+"/start", nil).Code
		}
		return rs
	})
	ctx := context.Background()
	agent := setupBrokerAgentInPhase(t, base, "reclaim-fin-offline", state.PhaseStopped)
	require.NoError(t, base.UpdateRuntimeBrokerHeartbeat(ctx, agent.RuntimeBrokerID, store.BrokerStatusOffline))
	require.False(t, srv.brokerReachable(ctx, agent), "the broker reads offline")
	seedAgentDeletion(t, base, agent.ID, seedExpiredFinalize)
	require.True(t, mustGetAgent(t, base, agent.ID).DeletionHoldsRow(time.Now()))

	path = "/api/v1/agents/" + agent.ID
	requireDeleteInProgress(t, doRequest(t, srv, http.MethodPost, path+"/start", nil))

	fault.Arm()

	rec := doRequest(t, srv, http.MethodDelete, path, nil)
	require.Equal(t, http.StatusNoContent, rec.Code, rec.Body.String())
	assert.True(t, agentGone(t, base, agent.ID))
	assert.Zero(t, disp.callCount(), "the finalizing re-claim skips the dispatch")
	assert.Equal(t, 1, pub.count("deleted"))

	assert.Equal(t, http.StatusConflict, startCode, "start is refused right after the re-claim")
	holds := rs.recorded()
	require.NotEmpty(t, holds)
	for i, h := range holds {
		assert.True(t, h, "deletion write %d left the row not held by the delete", i)
	}
}
