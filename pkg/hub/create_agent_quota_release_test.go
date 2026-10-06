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
	"encoding/json"
	"net/http"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/require"
)

// activeReservationResources returns the resource IDs of the active
// reservations for limitName in the given scope.
func activeReservationResources(t *testing.T, s store.Store, limitName, scopeType, scopeID string) []string {
	t.Helper()
	ctx := context.Background()
	def, err := s.GetLimitDefinitionByName(ctx, limitName)
	require.NoError(t, err)
	res, err := s.ListActiveReservations(ctx, def.ID, scopeType, scopeID)
	require.NoError(t, err)
	ids := make([]string, 0, len(res))
	for _, r := range res {
		ids = append(ids, r.ResourceID)
	}
	return ids
}

// TestCreateAgent_ProjectLimitRefusal_ReleasesBrokerReservation pins the
// first releaseAgentQuotas call in createAgentInProject (ptone/scion#2018):
// the max_agents_per_broker reservation succeeds, then the
// max_agents_per_project reservation is refused. The create must answer 429
// and leave no reservation of either kind behind for the refused agent.
func TestCreateAgent_ProjectLimitRefusal_ReleasesBrokerReservation(t *testing.T) {
	disp := &createAgentDispatcher{createPhase: string(state.PhaseRunning)}
	srv, s, project := setupCreateAgentServer(t, disp)
	setBrokerAgentCeiling(t, s, 5) // room on the broker
	setProjectAgentCeiling(t, s, 1)

	rec1 := doRequest(t, srv, http.MethodPost, "/api/v1/agents", CreateAgentRequest{
		Name: "proj-limit-agent-1", ProjectID: project.ID,
	})
	require.Equal(t, http.StatusCreated, rec1.Code, rec1.Body.String())
	first, err := s.GetAgentBySlug(context.Background(), project.ID, "proj-limit-agent-1")
	require.NoError(t, err)

	brokerID := project.DefaultRuntimeBrokerID
	require.Equal(t, []string{first.ID},
		activeReservationResources(t, s, store.LimitMaxAgentsPerBroker, store.QuotaScopeBroker, brokerID))
	require.Equal(t, []string{first.ID},
		activeReservationResources(t, s, store.LimitMaxAgentsPerProject, store.QuotaScopeProject, project.ID))

	rec2 := doRequest(t, srv, http.MethodPost, "/api/v1/agents", CreateAgentRequest{
		Name: "proj-limit-agent-2", ProjectID: project.ID,
	})
	require.Equal(t, http.StatusTooManyRequests, rec2.Code, rec2.Body.String())
	var errResp ErrorResponse
	require.NoError(t, json.Unmarshal(rec2.Body.Bytes(), &errResp))
	require.Equal(t, quotaExceededMessage(store.LimitMaxAgentsPerProject), errResp.Error.Message,
		"the refusal must come from the project limit, after the broker reservation succeeded")

	require.Equal(t, []string{first.ID},
		activeReservationResources(t, s, store.LimitMaxAgentsPerBroker, store.QuotaScopeBroker, brokerID),
		"the refused create's broker reservation must be released")
	require.Equal(t, []string{first.ID},
		activeReservationResources(t, s, store.LimitMaxAgentsPerProject, store.QuotaScopeProject, project.ID),
		"the refused create must hold no project reservation")
	_, err = s.GetAgentBySlug(context.Background(), project.ID, "proj-limit-agent-2")
	require.ErrorIs(t, err, store.ErrNotFound)
}

// TestCreateAgent_RowWriteFailure_ReleasesBothReservations pins the second
// releaseAgentQuotas call in createAgentInProject (ptone/scion#2018): both
// reservations succeed, then the agent row write
// (createAgentWithIdentityKeyAndEdge) fails. Both reservations must be
// released.
//
// The row write is made to fail the way it fails in production: another
// agent in the project already holds the new slug as an identity key (a
// rename reserves the new display name's key; see
// TestCreateAgentInProject_OrderingCollisionIsRejected). The slug lookup
// finds no agent, so the create proceeds past both reservations, and the
// identity-key insert inside the row-write transaction conflicts (409).
func TestCreateAgent_RowWriteFailure_ReleasesBothReservations(t *testing.T) {
	disp := &createAgentDispatcher{createPhase: string(state.PhaseRunning)}
	srv, s, project := setupCreateAgentServer(t, disp)
	ctx := context.Background()
	setBrokerAgentCeiling(t, s, 5)
	setProjectAgentCeiling(t, s, 10)

	holder := &store.Agent{
		ID: tid("agent-key-holder"), Slug: "key-holder", Name: "key-holder",
		ProjectID: project.ID, Phase: string(state.PhaseStopped),
	}
	require.NoError(t, s.CreateAgent(ctx, holder))
	require.NoError(t, s.ReplaceAgentIdentityKeys(ctx, holder.ID, project.ID, []string{"key-holder", "widget-bot"}))

	rec := doRequest(t, srv, http.MethodPost, "/api/v1/agents", CreateAgentRequest{
		Name: "widget-bot", ProjectID: project.ID,
	})
	require.Equal(t, http.StatusConflict, rec.Code, rec.Body.String())
	_, err := s.GetAgentBySlug(ctx, project.ID, "widget-bot")
	require.ErrorIs(t, err, store.ErrNotFound, "the failed row write must leave no agent")

	require.Empty(t,
		activeReservationResources(t, s, store.LimitMaxAgentsPerBroker, store.QuotaScopeBroker, project.DefaultRuntimeBrokerID),
		"the broker reservation must be released when the row write fails")
	require.Empty(t,
		activeReservationResources(t, s, store.LimitMaxAgentsPerProject, store.QuotaScopeProject, project.ID),
		"the project reservation must be released when the row write fails")
}
