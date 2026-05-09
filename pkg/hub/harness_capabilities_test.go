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
	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func seedCreatedAgentForHarnessTest(t *testing.T, s store.Store, id, harnessConfig string) *store.Agent {
	t.Helper()
	ctx := context.Background()

	project := &store.Project{ID: "project-" + id, Name: "Project " + id, Slug: "project-" + id}
	require.NoError(t, s.CreateProject(ctx, project))

	agent := &store.Agent{
		ID:      "agent-" + id,
		Slug:    "agent-" + id,
		Name:    "Agent " + id,
		ProjectID: project.ID,
		Phase:   string(state.PhaseCreated),
		AppliedConfig: &store.AgentAppliedConfig{
			HarnessConfig: harnessConfig,
		},
	}
	require.NoError(t, s.CreateAgent(ctx, agent))
	return agent
}

func TestGetAgent_ExposesHarnessCapabilities(t *testing.T) {
	srv, s := testServer(t)
	agent := seedCreatedAgentForHarnessTest(t, s, "caps", "claude")

	rec := doRequest(t, srv, http.MethodGet, "/api/v1/agents/"+agent.ID, nil)
	require.Equal(t, http.StatusOK, rec.Code)

	var got AgentWithCapabilities
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &got))
	require.NotNil(t, got.HarnessCapabilities)
	assert.Equal(t, "claude", got.ResolvedHarness)
	assert.Equal(t, "claude", got.HarnessCapabilities.Harness)
	assert.Equal(t, api.SupportNo, got.HarnessCapabilities.Limits.MaxModelCalls.Support)
}

func TestUpdateAgent_RejectsUnsupportedMaxModelCallsForClaude(t *testing.T) {
	srv, s := testServer(t)
	agent := seedCreatedAgentForHarnessTest(t, s, "claude-update", "claude")

	rec := doRequest(t, srv, http.MethodPatch, "/api/v1/agents/"+agent.ID, map[string]interface{}{
		"config": map[string]interface{}{
			"max_model_calls": 2,
		},
	})
	require.Equal(t, http.StatusBadRequest, rec.Code)

	var errResp ErrorResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &errResp))
	assert.Equal(t, ErrCodeValidationError, errResp.Error.Code)
	require.NotNil(t, errResp.Error.Details)
	fields, ok := errResp.Error.Details["fields"].(map[string]interface{})
	require.True(t, ok)
	_, has := fields["max_model_calls"]
	assert.True(t, has)
}

func TestUpdateAgent_AllowsGeminiMaxModelCalls(t *testing.T) {
	srv, s := testServer(t)
	agent := seedCreatedAgentForHarnessTest(t, s, "gemini-update", "gemini")

	rec := doRequest(t, srv, http.MethodPatch, "/api/v1/agents/"+agent.ID, map[string]interface{}{
		"config": map[string]interface{}{
			"max_model_calls": 3,
		},
	})
	require.Equal(t, http.StatusOK, rec.Code)

	updated, err := s.GetAgent(context.Background(), agent.ID)
	require.NoError(t, err)
	require.NotNil(t, updated.AppliedConfig)
	require.NotNil(t, updated.AppliedConfig.InlineConfig)
	assert.Equal(t, 3, updated.AppliedConfig.InlineConfig.MaxModelCalls)
}

func TestUpdateAgent_AllowsMaxDurationForAllHarnesses(t *testing.T) {
	srv, s := testServer(t)
	agent := seedCreatedAgentForHarnessTest(t, s, "duration-update", "gemini")

	rec := doRequest(t, srv, http.MethodPatch, "/api/v1/agents/"+agent.ID, map[string]interface{}{
		"config": map[string]interface{}{
			"max_duration": "10m",
		},
	})
	require.Equal(t, http.StatusOK, rec.Code)

	updated, err := s.GetAgent(context.Background(), agent.ID)
	require.NoError(t, err)
	require.NotNil(t, updated.AppliedConfig)
	require.NotNil(t, updated.AppliedConfig.InlineConfig)
	assert.Equal(t, "10m", updated.AppliedConfig.InlineConfig.MaxDuration)
}
