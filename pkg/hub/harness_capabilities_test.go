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

	project := &store.Project{ID: tid("project-" + id), Name: "Project " + id, Slug: "project-" + id}
	require.NoError(t, s.CreateProject(ctx, project))

	agent := &store.Agent{
		ID:        tid("agent-" + id),
		Slug:      "agent-" + id,
		Name:      "Agent " + id,
		ProjectID: project.ID,
		Phase:     string(state.PhaseCreated),
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

func TestUpdateAgent_RejectsUnsupportedMaxModelCallsForGeneric(t *testing.T) {
	srv, s := testServer(t)
	agent := seedCreatedAgentForHarnessTest(t, s, "claude-update", "generic")

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
	agent := seedCreatedAgentForHarnessTest(t, s, "gemini-update", "gemini-cli")

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

func TestGetAgent_CustomHarnessTypeFromHarnessConfig(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	hc := &store.HarnessConfig{
		ID:      tid("hc-custom"),
		Name:    "custom-harness",
		Slug:    "custom-harness",
		Harness: "custom-harness",
		Scope:   store.HarnessConfigScopeGlobal,
		Status:  store.HarnessConfigStatusActive,
	}
	require.NoError(t, s.CreateHarnessConfig(ctx, hc))

	agent := seedCreatedAgentForHarnessTest(t, s, "custom-type", "custom-harness")

	rec := doRequest(t, srv, http.MethodGet, "/api/v1/agents/"+agent.ID, nil)
	require.Equal(t, http.StatusOK, rec.Code)

	var got AgentWithCapabilities
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &got))
	assert.Equal(t, "custom-harness", got.ResolvedHarness, "custom harness type should pass through from Hub DB harness config")
}

// TestResolveModelAliasForAgent_FallsBackToBuiltinTable is a regression test
// for ptone/scion#1869: on resume, the hub's stored harness config for an
// agent can lack a model_aliases map (e.g. it predates aliases being added,
// or the config entry never had one), and resolveModelAliasForAgent used to
// pass the raw size alias (e.g. "large") straight through. It must instead
// fall back to the harness's own built-in model_aliases table declared in
// harnesses/<name>/config.yaml, so a resumed agent never receives an
// unresolved alias as ANTHROPIC_MODEL (or the equivalent for other harnesses).
func TestResolveModelAliasForAgent_FallsBackToBuiltinTable(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	// Stored harness config exists (by slug) but carries no model_aliases.
	hc := &store.HarnessConfig{
		ID:      tid("hc-no-aliases"),
		Name:    "claude-no-aliases",
		Slug:    "claude-no-aliases",
		Harness: "claude",
		Scope:   store.HarnessConfigScopeGlobal,
		Status:  store.HarnessConfigStatusActive,
		Config:  &store.HarnessConfigData{Harness: "claude"},
	}
	require.NoError(t, s.CreateHarnessConfig(ctx, hc))

	agent := seedCreatedAgentForHarnessTest(t, s, "no-aliases", "claude-no-aliases")

	got := srv.resolveModelAliasForAgent(ctx, agent, "large")
	assert.NotEqual(t, "large", got, "must not pass the unresolved size alias through to the harness")
	assert.Equal(t, "claude-opus-5-5", got, "should resolve via the claude harness's built-in model_aliases table")
}

// TestResolveModelAliasForAgent_NoHarnessConfigAtAllFallsBackToBuiltinTable
// covers the more common resume/restart shape from #1869: the agent's
// applied config references no harness-config at all (e.g. an inline
// harness name), so there is nothing to look up by ID or slug. The built-in
// fallback must still resolve the alias using the resolved harness type.
func TestResolveModelAliasForAgent_NoHarnessConfigAtAllFallsBackToBuiltinTable(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	project := &store.Project{ID: tid("project-inline"), Name: "Project inline", Slug: "project-inline"}
	require.NoError(t, s.CreateProject(ctx, project))

	agent := &store.Agent{
		ID:        tid("agent-inline"),
		Slug:      "agent-inline",
		Name:      "Agent inline",
		ProjectID: project.ID,
		Phase:     string(state.PhaseCreated),
		AppliedConfig: &store.AgentAppliedConfig{
			InlineConfig: &api.ScionConfig{Harness: "claude"},
		},
	}
	require.NoError(t, s.CreateAgent(ctx, agent))

	got := srv.resolveModelAliasForAgent(ctx, agent, "large")
	assert.NotEqual(t, "large", got, "must not pass the unresolved size alias through to the harness")
	assert.Equal(t, "claude-opus-5-5", got, "should resolve via the claude harness's built-in model_aliases table")
}
