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

// Deleting an agent that is still in the "created" phase: a start may
// already be running on its broker, so the delete is dispatched when the
// broker is reachable, and a broker problem never blocks removing the hub
// record (ptone/scion#2635).
package hub

import (
	"context"
	"fmt"
	"net/http"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// markCreatedPhase moves a fixture agent back to the "created" phase.
func markCreatedPhase(t *testing.T, s store.Store, agent *store.Agent) {
	t.Helper()
	ctx := context.Background()
	current, err := s.GetAgent(ctx, agent.ID)
	require.NoError(t, err)
	current.Phase = string(state.PhaseCreated)
	require.NoError(t, s.UpdateAgent(ctx, current))
}

func TestDeleteAgent_CreatedPhase_OnlineBroker_DispatchesDelete(t *testing.T) {
	srv, s := testServer(t)
	disp := &deleteDispatcher{}
	srv.SetDispatcher(disp)

	_, _, agent := setupOnlineBrokerAgent(t, s, "created-online")
	markCreatedPhase(t, s, agent)

	rec := doRequest(t, srv, http.MethodDelete, "/api/v1/agents/"+agent.ID, nil)
	require.Equal(t, http.StatusNoContent, rec.Code, rec.Body.String())

	assert.Equal(t, 1, disp.deleteCalls,
		"a created-phase agent may have a start in flight on its broker; the delete must reach the broker")
	_, err := s.GetAgent(context.Background(), agent.ID)
	assert.ErrorIs(t, err, store.ErrNotFound)
}

func TestDeleteAgent_CreatedPhase_DispatchFailure_StillDeletes(t *testing.T) {
	srv, s := testServer(t)
	disp := &deleteDispatcher{deleteErr: fmt.Errorf("broker connection refused")}
	srv.SetDispatcher(disp)

	_, _, agent := setupOnlineBrokerAgent(t, s, "created-fail")
	markCreatedPhase(t, s, agent)

	rec := doRequest(t, srv, http.MethodDelete, "/api/v1/agents/"+agent.ID, nil)
	require.Equal(t, http.StatusNoContent, rec.Code, rec.Body.String())

	assert.Equal(t, 1, disp.deleteCalls)
	_, err := s.GetAgent(context.Background(), agent.ID)
	assert.ErrorIs(t, err, store.ErrNotFound,
		"a broker error must not block deleting a created-phase agent")
}

func TestDeleteAgent_CreatedPhase_OfflineBroker_SkipsDispatch(t *testing.T) {
	srv, s := testServer(t)
	disp := &deleteDispatcher{}
	srv.SetDispatcher(disp)

	_, _, agent := setupOfflineBrokerAgent(t, s, "created-offline")
	markCreatedPhase(t, s, agent)

	rec := doRequest(t, srv, http.MethodDelete, "/api/v1/agents/"+agent.ID, nil)
	require.Equal(t, http.StatusNoContent, rec.Code, rec.Body.String())

	assert.Equal(t, 0, disp.deleteCalls, "an unreachable broker must not be contacted")
	_, err := s.GetAgent(context.Background(), agent.ID)
	assert.ErrorIs(t, err, store.ErrNotFound)
}
