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

// TestResolveModelAliasForAgent_UsesSyncedAliasesOverBuiltinTable is the
// end-to-end regression test for ptone/scion#2365: once a harness-config's
// config.yaml is synced into the hub (via BootstrapHarnessConfigsFromDir,
// which routes through resource_store.go's Create/Update -> extractModelConfig),
// resolveModelAliasForAgent must resolve using the *synced* model_aliases,
// not the table compiled into the hub binary from harnesses/<name>/config.yaml
// at build time. Before the fix, the stored record never carried
// model_aliases at all, so this always fell through to the built-in table.
func TestResolveModelAliasForAgent_UsesSyncedAliasesOverBuiltinTable(t *testing.T) {
	srv, s, _ := testTemplateBootstrapServer(t)
	ctx := context.Background()

	dir := makeHarnessConfigDir(t, "codex-custom", map[string]string{
		"config.yaml": "harness: codex\n" +
			"model_aliases:\n  small: tiny-model\n  large: totally-custom-large-model\n",
	})
	require.NoError(t, srv.BootstrapHarnessConfigsFromDir(ctx, dir))

	hc, err := s.GetHarnessConfigBySlug(ctx, "codex-custom", store.HarnessConfigScopeGlobal, "")
	require.NoError(t, err)
	require.NotNil(t, hc.Config)
	require.Equal(t, "totally-custom-large-model", hc.Config.ModelAliases["large"], "sync must have persisted the synced aliases")

	agent := seedCreatedAgentForHarnessTest(t, s, "synced-aliases", "codex-custom")
	agent.AppliedConfig.HarnessConfigID = hc.ID
	require.NoError(t, s.UpdateAgent(ctx, agent))

	got := srv.resolveModelAliasForAgent(ctx, agent, "large")
	assert.Equal(t, "totally-custom-large-model", got, "must resolve using the synced model_aliases, not the built-in codex table")
}

// TestResolveModelAliasForAgent_BackfillsFromStorageWhenRecordPredatesSync
// covers a harness-config record persisted before model_aliases extraction
// existed (or otherwise never re-synced since): the DB record's Config has
// no ModelAliases, but its config.yaml is still present in storage.
// resolveModelAliasForAgent must re-derive the aliases from that stored
// config.yaml at read time rather than immediately falling back to the
// built-in table — this is the "backfill" behavior described in the fix
// (self-heals without waiting for the next sync/migration).
func TestResolveModelAliasForAgent_BackfillsFromStorageWhenRecordPredatesSync(t *testing.T) {
	srv, s, _ := testTemplateBootstrapServer(t)
	ctx := context.Background()

	dir := makeHarnessConfigDir(t, "codex-legacy", map[string]string{
		"config.yaml": "harness: codex\n" +
			"model_aliases:\n  small: tiny-model\n  large: stored-large-model\n",
	})
	require.NoError(t, srv.BootstrapHarnessConfigsFromDir(ctx, dir))

	hc, err := s.GetHarnessConfigBySlug(ctx, "codex-legacy", store.HarnessConfigScopeGlobal, "")
	require.NoError(t, err)
	require.NotNil(t, hc.Config)
	require.NotEmpty(t, hc.Config.ModelAliases)

	// Simulate a record that predates this fix: config.yaml is still in
	// storage (untouched), but the DB record's Config carries no aliases.
	hc.Config = &store.HarnessConfigData{Harness: "codex"}
	require.NoError(t, s.UpdateHarnessConfig(ctx, hc))

	agent := seedCreatedAgentForHarnessTest(t, s, "backfill-aliases", "codex-legacy")
	agent.AppliedConfig.HarnessConfigID = hc.ID
	require.NoError(t, s.UpdateAgent(ctx, agent))

	got := srv.resolveModelAliasForAgent(ctx, agent, "large")
	assert.Equal(t, "stored-large-model", got, "must backfill aliases from the record's own stored config.yaml rather than the built-in codex table")
}

func TestUpdateAgent_AllowsConfigUpdateWhenStoppedAndRejectsWhenRunning(t *testing.T) {
	t.Run("stopped agent allows config model update", func(t *testing.T) {
		srv, s := testServer(t)
		ctx := context.Background()

		agent := seedCreatedAgentForHarnessTest(t, s, "stopped-model-update", "claude")
		agent.Phase = string(state.PhaseStopped)
		require.NoError(t, s.UpdateAgent(ctx, agent))

		rec := doRequest(t, srv, http.MethodPatch, "/api/v1/agents/"+agent.ID, map[string]interface{}{
			"config": map[string]interface{}{
				"model": "claude-opus-4-8",
			},
		})
		require.Equal(t, http.StatusOK, rec.Code, "body: %s", rec.Body.String())

		updated, err := s.GetAgent(ctx, agent.ID)
		require.NoError(t, err)
		require.NotNil(t, updated.AppliedConfig)
		assert.Equal(t, "claude-opus-4-8", updated.AppliedConfig.Model)
		require.NotNil(t, updated.AppliedConfig.InlineConfig)
		assert.Equal(t, "claude-opus-4-8", updated.AppliedConfig.InlineConfig.Model)
	})

	t.Run("running agent rejects config update with 409 Conflict", func(t *testing.T) {
		srv, s := testServer(t)
		ctx := context.Background()

		agent := seedCreatedAgentForHarnessTest(t, s, "running-model-update", "claude")
		agent.Phase = string(state.PhaseRunning)
		require.NoError(t, s.UpdateAgent(ctx, agent))

		rec := doRequest(t, srv, http.MethodPatch, "/api/v1/agents/"+agent.ID, map[string]interface{}{
			"config": map[string]interface{}{
				"model": "claude-opus-4-8",
			},
		})
		require.Equal(t, http.StatusConflict, rec.Code, "body: %s", rec.Body.String())
	})

	t.Run("soft-deleted agent rejects config update with 409 Conflict", func(t *testing.T) {
		srv, s := testServer(t)
		ctx := context.Background()

		agent := seedCreatedAgentForHarnessTest(t, s, "deleted-model-update", "claude")
		dbAgent, err := s.GetAgent(ctx, agent.ID)
		require.NoError(t, err)
		dbAgent.Phase = string(state.PhaseStopped)
		dbAgent.DeletedAt = dbAgent.Created
		require.NoError(t, s.UpdateAgent(ctx, dbAgent))

		rec := doRequest(t, srv, http.MethodPatch, "/api/v1/agents/"+agent.ID, map[string]interface{}{
			"config": map[string]interface{}{
				"model": "claude-opus-4-8",
			},
		})
		require.Equal(t, http.StatusConflict, rec.Code, "body: %s", rec.Body.String())
	})
}
