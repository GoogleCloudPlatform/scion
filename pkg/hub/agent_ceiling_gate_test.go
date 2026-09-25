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

// setBrokerAgentCeiling overrides the seeded max_agents_per_broker limit
// (default 12, see seedLimitDefinitions) to a small value so tests can hit it
// without creating a dozen agents.
func setBrokerAgentCeiling(t *testing.T, s store.Store, value int64) {
	t.Helper()
	def, err := s.GetLimitDefinitionByName(context.Background(), store.LimitMaxAgentsPerBroker)
	require.NoError(t, err, "max_agents_per_broker must be seeded by New()/seedLimitDefinitions")
	def.DefaultValue = value
	_, err = s.UpdateLimitDefinition(context.Background(), def)
	require.NoError(t, err)
}

// addProjectOnBroker creates a project wired to the given broker, the same
// way setupCreateAgentServer wires project1 to its broker, so a test can put
// a second project on the SAME broker (to prove the ceiling is shared) or on
// a freshly created broker (to prove ceilings across brokers are independent).
func addProjectOnBroker(t *testing.T, s store.Store, slug string, broker *store.RuntimeBroker) *store.Project {
	t.Helper()
	ctx := context.Background()
	project := &store.Project{
		ID:   tid("project-" + slug),
		Name: "Project " + slug,
		Slug: slug,
	}
	require.NoError(t, s.CreateProject(ctx, project))
	require.NoError(t, s.AddProjectProvider(ctx, &store.ProjectProvider{
		ProjectID:  project.ID,
		BrokerID:   broker.ID,
		BrokerName: broker.Name,
		Status:     store.BrokerStatusOnline,
	}))
	project.DefaultRuntimeBrokerID = broker.ID
	require.NoError(t, s.UpdateProject(ctx, project))
	return project
}

// newTestBroker creates and registers an additional online runtime broker,
// independent of the one setupCreateAgentServer wires up by default.
func newTestBroker(t *testing.T, s store.Store, slug string) *store.RuntimeBroker {
	t.Helper()
	broker := &store.RuntimeBroker{
		ID:     tid("broker-" + slug),
		Name:   "Broker " + slug,
		Slug:   slug,
		Status: store.BrokerStatusOnline,
	}
	require.NoError(t, s.CreateRuntimeBroker(context.Background(), broker))
	return broker
}

// TestCreateAgent_BrokerCeiling_RejectsPastLimit is the core regression test
// for ptone/scion#1303: exceeding a runtime broker's agent ceiling must be
// rejected up front with a 4xx, before store.CreateAgent runs and long before
// any broker dispatch that could spawn a process/container and take down the
// host. It must never return 201 and then let the Instance crash.
func TestCreateAgent_BrokerCeiling_RejectsPastLimit(t *testing.T) {
	disp := &createAgentDispatcher{createPhase: string(state.PhaseRunning)}
	srv, s, project := setupCreateAgentServer(t, disp)
	setBrokerAgentCeiling(t, s, 1)

	// First create is within the ceiling and must succeed.
	rec1 := doRequest(t, srv, http.MethodPost, "/api/v1/agents", CreateAgentRequest{
		Name:      "ceiling-agent-1",
		ProjectID: project.ID,
	})
	require.Equal(t, http.StatusCreated, rec1.Code, "first create must succeed under the ceiling: %s", rec1.Body.String())

	// Second create — same project, same broker, would be the second
	// concurrently live agent on that broker's host — must be rejected
	// outright, not accepted and then left to crash the host.
	rec2 := doRequest(t, srv, http.MethodPost, "/api/v1/agents", CreateAgentRequest{
		Name:      "ceiling-agent-2",
		ProjectID: project.ID,
	})
	require.Equal(t, http.StatusTooManyRequests, rec2.Code,
		"create past the broker's agent ceiling must be rejected with a 4xx, not accepted: %s", rec2.Body.String())

	var errResp ErrorResponse
	require.NoError(t, json.Unmarshal(rec2.Body.Bytes(), &errResp))
	require.Equal(t, ErrCodeQuotaExceeded, errResp.Error.Code)

	// The rejected create must not have persisted an agent record.
	_, err := s.GetAgentBySlug(context.Background(), project.ID, "ceiling-agent-2")
	require.ErrorIs(t, err, store.ErrNotFound, "rejected create must not leave an agent record behind")
}

// TestCreateAgent_BrokerCeiling_SharedAcrossProjectsOnSameBroker proves the
// ceiling is scoped to the runtime broker, not to one project or one
// creator: two different projects dispatching to the SAME broker must share
// the same counter, because they land on the same host.
func TestCreateAgent_BrokerCeiling_SharedAcrossProjectsOnSameBroker(t *testing.T) {
	disp := &createAgentDispatcher{createPhase: string(state.PhaseRunning)}
	srv, s, project1 := setupCreateAgentServer(t, disp)
	setBrokerAgentCeiling(t, s, 1)

	broker, err := s.GetRuntimeBroker(context.Background(), project1.DefaultRuntimeBrokerID)
	require.NoError(t, err)
	project2 := addProjectOnBroker(t, s, "ceiling-shared-2", broker)

	rec1 := doRequest(t, srv, http.MethodPost, "/api/v1/agents", CreateAgentRequest{
		Name:      "shared-broker-agent-1",
		ProjectID: project1.ID,
	})
	require.Equal(t, http.StatusCreated, rec1.Code)

	// A different project on the SAME broker must still be blocked: the
	// broker's host, not the project, is what would run out of capacity.
	rec2 := doRequest(t, srv, http.MethodPost, "/api/v1/agents", CreateAgentRequest{
		Name:      "shared-broker-agent-2",
		ProjectID: project2.ID,
	})
	require.Equal(t, http.StatusTooManyRequests, rec2.Code,
		"the ceiling must be shared by every project on the same broker: %s", rec2.Body.String())
}

// TestCreateAgent_BrokerCeiling_IndependentAcrossBrokers proves the opposite
// of the shared-broker case: two different brokers must have independent
// counters. A hub-wide ceiling would wrongly couple a saturated small broker
// with an idle large one; per ptone's ruling on #1303, this is an
// infrastructure-scoped safety gate (per broker), distinct from the
// per-user/per-project fairness quotas.
func TestCreateAgent_BrokerCeiling_IndependentAcrossBrokers(t *testing.T) {
	disp := &createAgentDispatcher{createPhase: string(state.PhaseRunning)}
	srv, s, project1 := setupCreateAgentServer(t, disp)
	setBrokerAgentCeiling(t, s, 1)

	broker2 := newTestBroker(t, s, "ceiling-independent-2")
	project2 := addProjectOnBroker(t, s, "ceiling-independent-2", broker2)

	// Saturate broker1 (via project1).
	rec1 := doRequest(t, srv, http.MethodPost, "/api/v1/agents", CreateAgentRequest{
		Name:      "broker1-agent-1",
		ProjectID: project1.ID,
	})
	require.Equal(t, http.StatusCreated, rec1.Code)

	recBlocked := doRequest(t, srv, http.MethodPost, "/api/v1/agents", CreateAgentRequest{
		Name:      "broker1-agent-2",
		ProjectID: project1.ID,
	})
	require.Equal(t, http.StatusTooManyRequests, recBlocked.Code, "broker1 should now be at its ceiling")

	// broker2 is untouched and must still accept a create.
	rec2 := doRequest(t, srv, http.MethodPost, "/api/v1/agents", CreateAgentRequest{
		Name:      "broker2-agent-1",
		ProjectID: project2.ID,
	})
	require.Equal(t, http.StatusCreated, rec2.Code,
		"a different broker's ceiling must be independent, not shared hub-wide: %s", rec2.Body.String())
}

// TestCreateAgent_BrokerCeiling_ReleasedOnDelete confirms the per-broker
// reservation is released when its agent is deleted, the same way the
// existing per-project reservation is, so capacity becomes available again
// rather than being permanently consumed.
func TestCreateAgent_BrokerCeiling_ReleasedOnDelete(t *testing.T) {
	disp := &createAgentDispatcher{createPhase: string(state.PhaseRunning)}
	srv, s, project := setupCreateAgentServer(t, disp)
	setBrokerAgentCeiling(t, s, 1)

	rec1 := doRequest(t, srv, http.MethodPost, "/api/v1/agents", CreateAgentRequest{
		Name:      "release-agent-1",
		ProjectID: project.ID,
	})
	require.Equal(t, http.StatusCreated, rec1.Code)

	var created CreateAgentResponse
	require.NoError(t, json.Unmarshal(rec1.Body.Bytes(), &created))
	require.NotNil(t, created.Agent)

	// At the ceiling: the next create is rejected.
	recBlocked := doRequest(t, srv, http.MethodPost, "/api/v1/agents", CreateAgentRequest{
		Name:      "release-agent-2",
		ProjectID: project.ID,
	})
	require.Equal(t, http.StatusTooManyRequests, recBlocked.Code)

	// Hard-delete the first agent to release its reservation.
	recDel := doRequest(t, srv, http.MethodDelete, "/api/v1/agents/"+created.Agent.ID, nil)
	require.Equal(t, http.StatusNoContent, recDel.Code, "delete must succeed: %s", recDel.Body.String())

	// Capacity is back: a new create must now succeed.
	rec2 := doRequest(t, srv, http.MethodPost, "/api/v1/agents", CreateAgentRequest{
		Name:      "release-agent-3",
		ProjectID: project.ID,
	})
	require.Equal(t, http.StatusCreated, rec2.Code,
		"create must succeed again once the deleted agent's reservation is released: %s", rec2.Body.String())
}
