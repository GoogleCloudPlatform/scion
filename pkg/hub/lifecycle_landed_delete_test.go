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
	"log/slog"
	"net/http"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// A synchronous start or restart whose broker start lands after a delete
// won answers 409 delete_in_progress, like the mid-dispatch case, instead
// of 200 with the agent body (row delete-claimed or soft-deleted) or 404
// (row hard-deleted). The compensating delete of the landed run still runs,
// once, and its warning is carried in the 409 details (ptone/scion#3255).

const landedRunRemovedWarning = "agent was deleted while it was starting; its container was removed"

// newLandedDeleteServer returns a server whose real HTTP dispatcher talks to
// a landingClient, and a stopped agent assigned to that broker.
func newLandedDeleteServer(t *testing.T) (*Server, store.Store, *store.Agent, *landingClient) {
	t.Helper()
	ctx := context.Background()
	srv, s := testServer(t)

	broker := &store.RuntimeBroker{
		ID:       tid("landed-del-broker-" + t.Name()),
		Name:     "landed-del-broker",
		Slug:     "landed-del-broker-" + tidSlugSafe(t.Name()),
		Endpoint: "http://localhost:9800",
		Status:   store.BrokerStatusOnline,
	}
	require.NoError(t, s.CreateRuntimeBroker(ctx, broker))
	project := &store.Project{
		ID:                     tid("landed-del-project-" + t.Name()),
		Name:                   "landed-del-project",
		Slug:                   "landed-del-project-" + tidSlugSafe(t.Name()),
		DefaultRuntimeBrokerID: broker.ID,
	}
	require.NoError(t, s.CreateProject(ctx, project))
	require.NoError(t, s.AddProjectProvider(ctx, &store.ProjectProvider{
		ProjectID: project.ID, BrokerID: broker.ID, BrokerName: broker.Name, Status: broker.Status,
	}))
	agent := &store.Agent{
		ID:              tid("landed-del-agent-" + t.Name()),
		Name:            "landed-del-agent",
		Slug:            "landed-del-agent",
		ProjectID:       project.ID,
		OwnerID:         tid("landed-del-user"),
		RuntimeBrokerID: broker.ID,
		Phase:           string(state.PhaseStopped),
		AppliedConfig:   &store.AgentAppliedConfig{HarnessConfig: "claude"},
	}
	require.NoError(t, s.CreateAgent(ctx, agent))

	client := &landingClient{mockRuntimeBrokerClient: &mockRuntimeBrokerClient{}, reportRunID: true}
	srv.SetDispatcher(NewHTTPAgentDispatcherWithClient(s, client, false, slog.Default()))
	return srv, s, agent, client
}

func TestLifecycle_DeleteWinsAfterLanding(t *testing.T) {
	for _, action := range []string{api.AgentActionStart, api.AgentActionRestart} {
		for _, del := range landingDeletes {
			t.Run(action+"/"+del.name, func(t *testing.T) {
				srv, s, agent, client := newLandedDeleteServer(t)
				client.onLand = func() { del.apply(t, s, agent.ID) }

				rec := doRequest(t, srv, http.MethodPost, "/api/v1/agents/"+agent.ID+"/"+action, nil)
				sent := client.lastStartExtras.RunID
				require.NotEmpty(t, sent, "the start leg reached the broker")

				if !del.compensate {
					// The agent is live: 200 with the agent body, unchanged.
					require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
					var resp struct {
						ID    string `json:"id"`
						Phase string `json:"phase"`
					}
					require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
					assert.Equal(t, agent.ID, resp.ID)
					assert.Equal(t, string(state.PhaseRunning), resp.Phase)
					assert.Empty(t, client.deleteRuns, "no compensating delete")
					return
				}

				requireIntentDeleteInProgress(t, rec, agent.ID)
				var body ErrorResponse
				require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
				assert.Contains(t, body.Error.Details["warnings"], landedRunRemovedWarning,
					"the compensation warning is carried in the details")
				var raw map[string]json.RawMessage
				require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &raw))
				assert.NotContains(t, raw, "id", "no agent body")
				assert.NotContains(t, raw, "agent", "no agent body")
				assert.Equal(t, []string{sent}, client.deleteRuns,
					"the landed run is deleted once, scoped to its run ID")

				// The delete's state is left alone: the phase write is
				// skipped, not merely dropped by the guard.
				if got, err := s.GetAgent(context.Background(), agent.ID); err == nil {
					assert.NotEqual(t, string(state.PhaseRunning), got.Phase)
				}
			})
		}
	}
}
