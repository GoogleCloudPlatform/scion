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
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/secret"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// HTTP tests for relaying start-leg dispatch warnings: the broker's
// hub-only env drop warnings and the hub's TZ-targeted secret warning reach
// the user-facing start, restart and create-on-existing-agent responses.

const brokerTZDropWarning = "broker dropped TZ: the hub is the only TZ source"

// newTZWarningsServer returns a server whose real HTTP dispatcher talks to
// a fake broker that returns brokerTZDropWarning on start, with a user
// secret targeting TZ, and a stopped agent assigned to that broker.
func newTZWarningsServer(t *testing.T) (*Server, store.Store, *store.Agent, *mockRuntimeBrokerClient) {
	t.Helper()
	ctx := context.Background()
	srv, s := testServer(t)

	broker := &store.RuntimeBroker{
		ID:       tid("tz-warn-broker-" + t.Name()),
		Name:     "tz-warn-broker",
		Slug:     "tz-warn-broker-" + tidSlugSafe(t.Name()),
		Endpoint: "http://localhost:9800",
		Status:   store.BrokerStatusOnline,
	}
	require.NoError(t, s.CreateRuntimeBroker(ctx, broker))
	project := &store.Project{
		ID:                     tid("tz-warn-project-" + t.Name()),
		Name:                   "tz-warn-project",
		Slug:                   "tz-warn-project-" + tidSlugSafe(t.Name()),
		DefaultRuntimeBrokerID: broker.ID,
	}
	require.NoError(t, s.CreateProject(ctx, project))
	require.NoError(t, s.AddProjectProvider(ctx, &store.ProjectProvider{
		ProjectID: project.ID, BrokerID: broker.ID, BrokerName: broker.Name, Status: broker.Status,
	}))

	// The owner is a project member, so the agent is in good standing
	// (ptone/scion#3433).
	ensureStandingRoot(t, s, project.ID, tid("tz-warn-user"))
	agent := &store.Agent{
		ID:              tid("tz-warn-agent-" + t.Name()),
		Name:            "tz-warn-agent",
		Slug:            "tz-warn-agent",
		ProjectID:       project.ID,
		OwnerID:         tid("tz-warn-user"),
		RuntimeBrokerID: broker.ID,
		Phase:           string(state.PhaseStopped),
		AppliedConfig:   &store.AgentAppliedConfig{HarnessConfig: "claude"},
	}
	require.NoError(t, s.CreateAgent(ctx, agent))

	client := &mockRuntimeBrokerClient{
		startReturnResp: &RemoteAgentResponse{Agent: &RemoteAgentInfo{
			ID: agent.ID, Phase: string(state.PhaseRunning), Warnings: []string{brokerTZDropWarning},
		}},
	}
	d := NewHTTPAgentDispatcherWithClient(s, client, false, slog.Default())
	d.SetSecretBackend(&mockSecretBackend{secrets: []secret.SecretWithValue{{
		SecretMeta: secret.SecretMeta{Name: "tz-secret", SecretType: "environment", Target: "TZ", Scope: "user", ScopeID: agent.OwnerID, InjectionMode: "always"},
		Value:      "America/Denver",
	}}})
	srv.SetDispatcher(d)
	return srv, s, agent, client
}

// assertTZStartWarnings checks both start-leg warnings are present: the
// broker's, and the hub's TZ-secret warning (which never carries the value).
func assertTZStartWarnings(t *testing.T, warnings []string) {
	t.Helper()
	assert.Contains(t, warnings, brokerTZDropWarning)
	var secretWarning string
	for _, w := range warnings {
		if strings.Contains(w, `"tz-secret"`) {
			secretWarning = w
		}
	}
	require.NotEmpty(t, secretWarning, "the TZ-targeted secret warning must be relayed: %v", warnings)
	assert.NotContains(t, secretWarning, "America/Denver", "the warning never carries the secret value")
}

func TestAgentLifecycle_RelaysStartWarnings(t *testing.T) {
	for _, action := range []string{"start", "restart"} {
		t.Run(action, func(t *testing.T) {
			srv, _, agent, client := newTZWarningsServer(t)
			rec := doRequest(t, srv, http.MethodPost, "/api/v1/agents/"+agent.ID+"/"+action, nil)
			require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
			assert.Empty(t, client.lastResolvedEnv["TZ"], "the TZ secret never reaches the broker")

			var resp struct {
				ID       string   `json:"id"`
				Phase    string   `json:"phase"`
				Warnings []string `json:"warnings"`
			}
			require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
			assert.Equal(t, agent.ID, resp.ID, "the agent stays the body of the response")
			assert.Equal(t, string(state.PhaseRunning), resp.Phase)
			assertTZStartWarnings(t, resp.Warnings)
		})
	}
}

// TestAgentLifecycle_NoWarningsKeyWhenEmpty checks the lifecycle response is
// unchanged (no warnings key) when the dispatch raised none.
func TestAgentLifecycle_NoWarningsKeyWhenEmpty(t *testing.T) {
	srv, _, agent, client := newTZWarningsServer(t)
	client.startReturnResp.Agent.Warnings = nil
	srv.GetDispatcher().(*HTTPAgentDispatcher).SetSecretBackend(&mockSecretBackend{})
	rec := doRequest(t, srv, http.MethodPost, "/api/v1/agents/"+agent.ID+"/start", nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var raw map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &raw))
	assert.NotContains(t, raw, "warnings")
	assert.Contains(t, raw, "id")
}

// TestCreateAgent_ExistingStoppedAgentRelaysStartWarnings covers the create
// call that resumes an existing stopped agent in place (handleExistingAgent).
func TestCreateAgent_ExistingStoppedAgentRelaysStartWarnings(t *testing.T) {
	srv, s, agent, _ := newTZWarningsServer(t)
	rec := doRequest(t, srv, http.MethodPost, "/api/v1/agents", CreateAgentRequest{
		Name: agent.Name, ProjectID: agent.ProjectID, Task: "x", Resume: true,
	})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var resp CreateAgentResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	require.NotNil(t, resp.Agent)
	assert.Equal(t, agent.ID, resp.Agent.ID, "the existing agent was resumed, not recreated")
	assertTZStartWarnings(t, resp.Warnings)

	stored, err := s.GetAgent(context.Background(), agent.ID)
	require.NoError(t, err)
	assert.Equal(t, string(state.PhaseRunning), stored.Phase)
}
