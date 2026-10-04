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

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// preClaimReadStore returns agent rows as they read before a delete claim
// (deletion columns cleared), so the start gate passes and the delete claim
// is met only by the start's own writes: the race in which a delete claims
// the row between the gate and the running-intent write.
type preClaimReadStore struct {
	store.Store
}

func (s preClaimReadStore) GetAgent(ctx context.Context, id string) (*store.Agent, error) {
	a, err := s.Store.GetAgent(ctx, id)
	if err != nil || a == nil {
		return a, err
	}
	c := *a
	c.DeletionState = ""
	c.DeletionLeaseAt = nil
	return &c, nil
}

// A delete that claims the row after the start gate passed refuses the
// start's running-intent write: start, restart and create-on-existing answer
// 409 delete_in_progress, nothing is dispatched (restart's stop leg
// included), and the stored intent stays as the delete left it
// (ptone/scion#2550, round 5 N2).
func TestRunIntent_DeleteClaimAfterGateRefusesStart(t *testing.T) {
	for _, action := range []string{api.AgentActionStart, api.AgentActionRestart} {
		t.Run(action, func(t *testing.T) {
			srv, base := testServer(t)
			disp := &deleteGuardDispatcher{}
			srv.SetDispatcher(disp)
			agent := setupBrokerAgentInPhase(t, base, "ri-claim-"+action, state.PhaseStopped)
			srv.store = preClaimReadStore{Store: base}
			ctx := context.Background()
			_, err := base.SetRunIntent(ctx, agent.ID, store.RunIntentStopped)
			require.NoError(t, err)
			seedAgentDeletion(t, base, agent.ID, seedLiveDeleting)

			rec := doRequest(t, srv, http.MethodPost, "/api/v1/agents/"+agent.ID+"/"+action, nil)
			requireDeleteInProgress(t, rec)
			assert.Zero(t, disp.starts, "no start dispatch")
			assert.Zero(t, disp.stops, "no stop dispatch")

			got, err := base.GetAgent(ctx, agent.ID)
			require.NoError(t, err)
			assert.Equal(t, store.RunIntentStopped, got.RunIntent, "intent is not left running")
			assert.Equal(t, string(state.PhaseStopped), got.Phase)
		})
	}
}

// The DM wake path maps the refused running intent to delete_in_progress
// too, not to a runtime error (round 5 N2).
func TestRunIntent_DeleteClaimAfterGateRefusesWake(t *testing.T) {
	srv, s := testServer(t)
	disp := &deleteGuardDispatcher{}
	srv.SetDispatcher(disp)
	agent := setupBrokerAgentInPhase(t, s, "ri-claim-wake", state.PhaseSuspended)
	ctx := context.Background()
	_, err := s.SetRunIntent(ctx, agent.ID, store.RunIntentStopped)
	require.NoError(t, err)
	// The wake's caller read the row before the claim.
	stale, err := s.GetAgent(ctx, agent.ID)
	require.NoError(t, err)
	seedAgentDeletion(t, s, agent.ID, seedLiveDeleting)

	_, dmErr := srv.wakeAgentForDM(ctx, stale)
	require.NotNil(t, dmErr)
	assert.Equal(t, ErrCodeDeleteInProgress, dmErr.Code)
	assert.Equal(t, http.StatusConflict, dmErr.HTTPStatus)
	assert.Zero(t, disp.starts, "no start dispatch")
	got, err := s.GetAgent(ctx, agent.ID)
	require.NoError(t, err)
	assert.Equal(t, store.RunIntentStopped, got.RunIntent)
}
