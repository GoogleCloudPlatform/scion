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

// This file covers design §6 H-4: `launch` (with `deadline` and
// `remainingSeconds`) is required on GET and List agent responses for an
// agent with an active launch, and must be absent for an agent with none.
package hub

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestGetAgent_ActiveLaunchIncludesLaunchField(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	project := &store.Project{ID: tid("launch-api-get-project"), Slug: "launch-api-get-project", Name: "Launch API Get Project", Created: time.Now(), Updated: time.Now()}
	require.NoError(t, s.CreateProject(ctx, project))
	agent := &store.Agent{
		ID: tid("launch-api-get-agent"), Slug: "launch-api-get-agent", Name: "Launch API Get Agent",
		ProjectID: project.ID, Phase: string(state.PhaseCreated), StateVersion: 1,
		Created: time.Now(), Updated: time.Now(),
	}
	require.NoError(t, s.CreateAgent(ctx, agent))
	_, err := s.BeginLaunch(ctx, agent.ID, store.LaunchKindCreate, 5*time.Minute)
	require.NoError(t, err)

	rec := doRequest(t, srv, http.MethodGet, "/api/v1/agents/"+agent.ID, nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	var raw map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &raw))
	launchRaw, ok := raw["launch"]
	require.True(t, ok, "expected a \"launch\" key in the GET response for an agent with an active launch")

	var launch store.AgentLaunch
	require.NoError(t, json.Unmarshal(launchRaw, &launch))
	assert.True(t, launch.Active)
	assert.Equal(t, "active", launch.State)
	require.NotNil(t, launch.Deadline)
	require.NotNil(t, launch.RemainingSeconds)
	assert.Greater(t, *launch.RemainingSeconds, 0)
}

func TestGetAgent_NoLaunchOmitsLaunchField(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	project := &store.Project{ID: tid("launch-api-noget-project"), Slug: "launch-api-noget-project", Name: "Launch API NoGet Project", Created: time.Now(), Updated: time.Now()}
	require.NoError(t, s.CreateProject(ctx, project))
	agent := &store.Agent{
		ID: tid("launch-api-noget-agent"), Slug: "launch-api-noget-agent", Name: "Launch API NoGet Agent",
		ProjectID: project.ID, Phase: string(state.PhaseRunning), StateVersion: 1,
		Created: time.Now(), Updated: time.Now(),
	}
	require.NoError(t, s.CreateAgent(ctx, agent))

	rec := doRequest(t, srv, http.MethodGet, "/api/v1/agents/"+agent.ID, nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	var raw map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &raw))
	_, ok := raw["launch"]
	assert.False(t, ok, "expected no \"launch\" key in the GET response for an agent with no launch")
}

func TestListAgents_ActiveLaunchIncludesLaunchField(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	project := &store.Project{ID: tid("launch-api-list-project"), Slug: "launch-api-list-project", Name: "Launch API List Project", Created: time.Now(), Updated: time.Now()}
	require.NoError(t, s.CreateProject(ctx, project))
	withLaunch := &store.Agent{
		ID: tid("launch-api-list-with"), Slug: "launch-api-list-with", Name: "With Launch",
		ProjectID: project.ID, Phase: string(state.PhaseCreated), StateVersion: 1,
		Created: time.Now(), Updated: time.Now(),
	}
	require.NoError(t, s.CreateAgent(ctx, withLaunch))
	_, err := s.BeginLaunch(ctx, withLaunch.ID, store.LaunchKindCreate, 5*time.Minute)
	require.NoError(t, err)

	without := &store.Agent{
		ID: tid("launch-api-list-without"), Slug: "launch-api-list-without", Name: "Without Launch",
		ProjectID: project.ID, Phase: string(state.PhaseRunning), StateVersion: 1,
		Created: time.Now(), Updated: time.Now(),
	}
	require.NoError(t, s.CreateAgent(ctx, without))

	rec := doRequest(t, srv, http.MethodGet, "/api/v1/agents?projectId="+project.ID, nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	var raw struct {
		Agents []map[string]json.RawMessage `json:"agents"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &raw))
	require.Len(t, raw.Agents, 2)

	byID := map[string]map[string]json.RawMessage{}
	for _, a := range raw.Agents {
		var idOnly struct {
			ID string `json:"id"`
		}
		require.NoError(t, json.Unmarshal(a["id"], &idOnly.ID))
		byID[idOnly.ID] = a
	}

	launchRaw, ok := byID[withLaunch.ID]["launch"]
	require.True(t, ok, "expected a \"launch\" key for the agent with an active launch")
	var launch store.AgentLaunch
	require.NoError(t, json.Unmarshal(launchRaw, &launch))
	assert.True(t, launch.Active)
	require.NotNil(t, launch.RemainingSeconds)

	_, ok = byID[without.ID]["launch"]
	assert.False(t, ok, "expected no \"launch\" key for the agent with no launch")
}
