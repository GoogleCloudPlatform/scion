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
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/secret"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// This file is the hub-side test plan for Option C (ptone/scion#2493, see
// options.md §5): PATCH /api/v1/agents/{id} must keep AppliedConfig.CreateInputs
// in sync with any config edit that actually changes a field's live value
// (invariant E), and leave it alone for an edit that merely echoes the live
// value back -- which is what the configure page's Save and Start both do on
// every call. All eight hub-side test-plan items live here; the ninth
// (web/src/components/pages/agent-configure.ts's buildConfig) is a Vitest
// test alongside that file.
//
// newReincarnateTestAgent and setupReincarnateTestServer (defined in
// handlers_agent_reincarnate_test.go) are reused throughout: they build a
// ready-to-go agent/project/broker trio, and the agent's default Phase
// ("running") is overridden to "created" wherever a test needs to PATCH
// config (applyAgentUpdate only allows config edits in 'created' or
// 'stopped', handlers_agents_core.go).

// patchAgentConfig issues a PATCH /api/v1/agents/{id} with a raw (map-typed)
// config body, so that an explicit empty/zero value in rawConfig actually
// reaches the wire as a present JSON key -- marshaling a *api.ScionConfig
// directly would silently omit it (every ScionConfig field is `omitempty`),
// which is exactly the ambiguity recordExplicitEdits' "present keys only"
// rule exists to resolve on the read side.
func patchAgentConfig(t *testing.T, srv *Server, agentID string, rawConfig map[string]interface{}) *httptest.ResponseRecorder {
	t.Helper()
	return doRequest(t, srv, http.MethodPatch, "/api/v1/agents/"+agentID, map[string]interface{}{
		"config": rawConfig,
	})
}

// TestApplyAgentUpdate_ExplicitEditsSurviveReincarnate is test-plan item 1:
// a provisionOnly-shaped agent (one with CreateInputs), PATCHed with a
// changed model, a new env key and a system prompt, must have all three
// survive `scion reincarnate` -- the exact bug ptone/scion#2493 reports.
func TestApplyAgentUpdate_ExplicitEditsSurviveReincarnate(t *testing.T) {
	disp := newReincarnateTestDispatcher()
	srv, s, project, broker := setupReincarnateTestServer(t, disp)
	ctx := context.Background()

	agent := newReincarnateTestAgent(t, s, project, broker, func(a *store.Agent) {
		a.Phase = string(state.PhaseCreated)
		a.AppliedConfig.Model = "old-model"
		a.AppliedConfig.CreateInputs = &store.AgentCreateInputs{}
	})

	rec := patchAgentConfig(t, srv, agent.ID, map[string]interface{}{
		"model":         "new-model",
		"env":           map[string]interface{}{"FOO": "bar"},
		"system_prompt": "be helpful",
	})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	updated, err := s.GetAgent(ctx, agent.ID)
	require.NoError(t, err)
	require.NotNil(t, updated.AppliedConfig.CreateInputs)
	require.NotNil(t, updated.AppliedConfig.CreateInputs.InlineConfig)
	assert.Equal(t, "new-model", updated.AppliedConfig.CreateInputs.InlineConfig.Model)
	assert.Equal(t, "bar", updated.AppliedConfig.CreateInputs.InlineConfig.Env["FOO"])
	assert.Equal(t, "be helpful", updated.AppliedConfig.CreateInputs.InlineConfig.SystemPrompt)

	fresh, _, err := srv.buildFreshAppliedConfig(ctx, updated, project, "")
	require.NoError(t, err)
	assert.Equal(t, "new-model", fresh.Model, "the PATCHed model must survive reincarnate")
	assert.Equal(t, "bar", fresh.Env["FOO"], "the PATCHed env key must survive reincarnate")
	require.NotNil(t, fresh.InlineConfig)
	assert.Equal(t, "be helpful", fresh.InlineConfig.SystemPrompt, "the PATCHed system prompt must survive reincarnate")
}

// TestApplyAgentUpdate_EchoPatchLeavesCreateInputsByteIdentical is test-plan
// item 2: a configure-shaped PATCH that echoes every live value back
// unchanged -- including a registry-qualified image sent in bare form, which
// canonicalizes to the same value -- must leave CreateInputs untouched. This
// is the common case: the configure page reloads the live, derived config
// and PATCHes the whole thing back on every Save and every Start.
func TestApplyAgentUpdate_EchoPatchLeavesCreateInputsByteIdentical(t *testing.T) {
	disp := newReincarnateTestDispatcher()
	disp.imageRegistry = "registry.example.com"
	srv, s, project, broker := setupReincarnateTestServer(t, disp)
	ctx := context.Background()

	agent := newReincarnateTestAgent(t, s, project, broker, func(a *store.Agent) {
		a.Phase = string(state.PhaseCreated)
		a.AppliedConfig.Image = "registry.example.com/explicit-image:v1"
		a.AppliedConfig.Model = "explicit-model"
		a.AppliedConfig.HarnessAuth = "api-key"
		tl := 3
		a.AppliedConfig.ThinkingLevel = &tl
		a.AppliedConfig.Env = map[string]string{"EXPLICIT_KEY": "explicit-value"}
		a.AppliedConfig.InlineConfig = &api.ScionConfig{
			Image:            "registry.example.com/explicit-image:v1",
			Model:            "explicit-model",
			AuthSelectedType: "api-key",
			ThinkingLevel:    &tl,
			Env:              map[string]string{"EXPLICIT_KEY": "explicit-value"},
			SystemPrompt:     "existing prompt",
		}
		a.AppliedConfig.CreateInputs = &store.AgentCreateInputs{
			HarnessAuth:   "api-key",
			ThinkingLevel: &tl,
			InlineConfig: &api.ScionConfig{
				Image:            "registry.example.com/explicit-image:v1",
				Model:            "explicit-model",
				AuthSelectedType: "api-key",
				ThinkingLevel:    &tl,
				Env:              map[string]string{"EXPLICIT_KEY": "explicit-value"},
				SystemPrompt:     "existing prompt",
			},
		}
	})

	before, err := json.Marshal(agent.AppliedConfig.CreateInputs)
	require.NoError(t, err)

	// Echo every live value back, exactly as agent-configure.ts's
	// populateForm/buildConfig round trip does -- including the image in its
	// BARE form (as a user could type, or as a stale page echoes it): the
	// registry-qualified comparison must still see this as unchanged.
	rec := patchAgentConfig(t, srv, agent.ID, map[string]interface{}{
		"image":             "explicit-image:v1",
		"model":             "explicit-model",
		"auth_selectedType": "api-key",
		"thinking_level":    3,
		"env":               map[string]interface{}{"EXPLICIT_KEY": "explicit-value"},
		"system_prompt":     "existing prompt",
	})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	updated, err := s.GetAgent(ctx, agent.ID)
	require.NoError(t, err)
	after, err := json.Marshal(updated.AppliedConfig.CreateInputs)
	require.NoError(t, err)
	assert.JSONEq(t, string(before), string(after),
		"an all-echo PATCH must leave CreateInputs byte-identical (invariant E)")
}

// TestApplyAgentUpdate_TemplateEnvRefreshesAfterEchoPatch is test-plan item
// 3: an env key the template originally supplied, echoed back unchanged by a
// PATCH, must not freeze into CreateInputs -- so when the template's default
// later changes, reincarnate picks up the NEW template value, not the one
// the echo would otherwise have pinned.
func TestApplyAgentUpdate_TemplateEnvRefreshesAfterEchoPatch(t *testing.T) {
	disp := newReincarnateTestDispatcher()
	srv, s, project, broker := setupReincarnateTestServer(t, disp)
	ctx := context.Background()

	template := &store.Template{
		ID:          tid("tmpl-env-refresh-" + t.Name()),
		Name:        "t",
		Slug:        "explicit-edits-template-" + tidSlugSafe(t.Name()),
		Harness:     "claude",
		Scope:       store.TemplateScopeGlobal,
		Status:      store.TemplateStatusActive,
		ContentHash: "template-hash-v1",
		Config:      &store.TemplateConfig{Env: map[string]string{"K": "a"}},
	}
	require.NoError(t, s.CreateTemplate(ctx, template))

	agent := newReincarnateTestAgent(t, s, project, broker, func(a *store.Agent) {
		a.Phase = string(state.PhaseCreated)
		a.Template = template.Slug
		a.AppliedConfig.Env = map[string]string{"K": "a"}
		a.AppliedConfig.InlineConfig = &api.ScionConfig{Env: map[string]string{"K": "a"}}
		a.AppliedConfig.CreateInputs = &store.AgentCreateInputs{}
	})

	// Echo PATCH: the configure page sends back the live K=a it loaded,
	// untouched.
	rec := patchAgentConfig(t, srv, agent.ID, map[string]interface{}{
		"env": map[string]interface{}{"K": "a"},
	})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	updated, err := s.GetAgent(ctx, agent.ID)
	require.NoError(t, err)
	if updated.AppliedConfig.CreateInputs.InlineConfig != nil {
		assert.NotContains(t, updated.AppliedConfig.CreateInputs.InlineConfig.Env, "K",
			"an echoed template-sourced env key must not be recorded as explicit")
	}

	// Now the template's default changes.
	template.Config.Env["K"] = "b"
	require.NoError(t, s.UpdateTemplate(ctx, template))

	fresh, _, err := srv.buildFreshAppliedConfig(ctx, updated, project, "")
	require.NoError(t, err)
	assert.Equal(t, "b", fresh.Env["K"], "reincarnate must pick up the template's new default, not freeze the echoed value")
}

// TestApplyAgentUpdate_DeletesEnvKeyAndThinkingLevel is test-plan item 4: a
// PATCH that removes a previously-explicit env key and sets thinking_level
// to null must remove both from CreateInputs.
func TestApplyAgentUpdate_DeletesEnvKeyAndThinkingLevel(t *testing.T) {
	disp := newReincarnateTestDispatcher()
	srv, s, project, broker := setupReincarnateTestServer(t, disp)
	ctx := context.Background()

	tl := 5
	agent := newReincarnateTestAgent(t, s, project, broker, func(a *store.Agent) {
		a.Phase = string(state.PhaseCreated)
		a.AppliedConfig.Env = map[string]string{"FOO": "bar"}
		a.AppliedConfig.ThinkingLevel = &tl
		a.AppliedConfig.InlineConfig = &api.ScionConfig{Env: map[string]string{"FOO": "bar"}, ThinkingLevel: &tl}
		a.AppliedConfig.CreateInputs = &store.AgentCreateInputs{
			ThinkingLevel: &tl,
			InlineConfig:  &api.ScionConfig{Env: map[string]string{"FOO": "bar"}, ThinkingLevel: &tl},
		}
	})

	// env without FOO -> deleted. thinking_level: null -> unset.
	rec := patchAgentConfig(t, srv, agent.ID, map[string]interface{}{
		"env":            map[string]interface{}{},
		"thinking_level": nil,
	})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	updated, err := s.GetAgent(ctx, agent.ID)
	require.NoError(t, err)
	ci := updated.AppliedConfig.CreateInputs
	require.NotNil(t, ci)
	assert.Nil(t, ci.ThinkingLevel, "thinking_level:null must clear CreateInputs.ThinkingLevel")
	if ci.InlineConfig != nil {
		assert.NotContains(t, ci.InlineConfig.Env, "FOO", "a removed env key must be deleted from CreateInputs")
		assert.Nil(t, ci.InlineConfig.ThinkingLevel, "thinking_level:null must clear the InlineConfig mirror too")
	}
}

// TestApplyAgentUpdate_AbsentVolumesKeptPresentEmptySystemPromptCleared is
// test-plan item 5: a PATCH whose raw config object never mentions "volumes"
// must leave CreateInputs.InlineConfig.Volumes alone (absent is never
// "cleared"), while a PATCH that explicitly sends an empty "system_prompt"
// must clear it (present-and-empty is a real, intentional edit).
func TestApplyAgentUpdate_AbsentVolumesKeptPresentEmptySystemPromptCleared(t *testing.T) {
	disp := newReincarnateTestDispatcher()
	srv, s, project, broker := setupReincarnateTestServer(t, disp)
	ctx := context.Background()

	agent := newReincarnateTestAgent(t, s, project, broker, func(a *store.Agent) {
		a.Phase = string(state.PhaseCreated)
		a.AppliedConfig.InlineConfig = &api.ScionConfig{
			SystemPrompt: "old explicit prompt",
			Volumes:      []api.VolumeMount{{Source: "/host/path", Target: "/container/path"}},
		}
		a.AppliedConfig.CreateInputs = &store.AgentCreateInputs{
			InlineConfig: &api.ScionConfig{
				SystemPrompt: "old explicit prompt",
				Volumes:      []api.VolumeMount{{Source: "/host/path", Target: "/container/path"}},
			},
		}
	})

	// The raw config object below has no "volumes" key at all (this page
	// does not render volumes), and an explicit empty "system_prompt".
	rec := patchAgentConfig(t, srv, agent.ID, map[string]interface{}{
		"system_prompt": "",
	})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	updated, err := s.GetAgent(ctx, agent.ID)
	require.NoError(t, err)
	ci := updated.AppliedConfig.CreateInputs
	require.NotNil(t, ci)
	require.NotNil(t, ci.InlineConfig)
	assert.Equal(t, "", ci.InlineConfig.SystemPrompt, "a present empty system_prompt must clear the explicit value")
	require.Len(t, ci.InlineConfig.Volumes, 1, "an absent 'volumes' key must never clear CreateInputs volumes")
	assert.Equal(t, "/host/path", ci.InlineConfig.Volumes[0].Source)
}

// TestApplyAgentUpdate_SecretNamedEnvKeyStrippedByCleanup is test-plan item
// 6: a secret-named key that a PATCH records into
// CreateInputs.InlineConfig.Env needs no new cleanup surface -- the existing
// AppliedConfigEnvCleanupExecutor narrow rule (applied_config_env_cleanup.go)
// already sweeps CreateInputs.InlineConfig.Env and strips it.
func TestApplyAgentUpdate_SecretNamedEnvKeyStrippedByCleanup(t *testing.T) {
	disp := newReincarnateTestDispatcher()
	srv, s, project, broker := setupReincarnateTestServer(t, disp)
	ctx := context.Background()

	agent := newReincarnateTestAgent(t, s, project, broker, func(a *store.Agent) {
		a.Phase = string(state.PhaseCreated)
		a.AppliedConfig.CreateInputs = &store.AgentCreateInputs{}
	})

	rec := patchAgentConfig(t, srv, agent.ID, map[string]interface{}{
		"env": map[string]interface{}{"OWNER_SECRET": "typed-into-env-row"},
	})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	updated, err := s.GetAgent(ctx, agent.ID)
	require.NoError(t, err)
	require.NotNil(t, updated.AppliedConfig.CreateInputs.InlineConfig)
	require.Equal(t, "typed-into-env-row", updated.AppliedConfig.CreateInputs.InlineConfig.Env["OWNER_SECRET"],
		"sanity check: the PATCH must have recorded the key before cleanup runs")

	secretBackend := &cleanupTestSecretBackend{
		byScope: map[string][]secret.SecretMeta{
			"user/" + updated.OwnerID: {
				{Name: "OWNER_SECRET", SecretType: "variable"},
			},
		},
	}
	exec := &AppliedConfigEnvCleanupExecutor{Store: s, SecretBackend: secretBackend}
	var buf bytes.Buffer
	require.NoError(t, exec.Run(ctx, &buf, nil))

	cleaned, err := s.GetAgent(ctx, agent.ID)
	require.NoError(t, err)
	require.NotNil(t, cleaned.AppliedConfig.CreateInputs.InlineConfig)
	assert.NotContains(t, cleaned.AppliedConfig.CreateInputs.InlineConfig.Env, "OWNER_SECRET",
		"the existing applied-config-env-cleanup sweep must strip a secret-named key recorded by recordExplicitEdits")
}

// TestApplyAgentUpdate_NilCreateInputsStaysNil is test-plan item 7: an agent
// with no CreateInputs (predates the field, or was never captured) must have
// a config PATCH leave it nil -- the reincarnate fallback
// (legacyCreateInputsFromAppliedConfig) already reads the live config
// directly for these agents, so recordExplicitEdits has nothing to seed.
func TestApplyAgentUpdate_NilCreateInputsStaysNil(t *testing.T) {
	disp := newReincarnateTestDispatcher()
	srv, s, project, broker := setupReincarnateTestServer(t, disp)
	ctx := context.Background()

	agent := newReincarnateTestAgent(t, s, project, broker, func(a *store.Agent) {
		a.Phase = string(state.PhaseCreated)
		a.AppliedConfig.CreateInputs = nil
	})

	rec := patchAgentConfig(t, srv, agent.ID, map[string]interface{}{
		"model":         "new-model",
		"env":           map[string]interface{}{"FOO": "bar"},
		"system_prompt": "be helpful",
	})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	updated, err := s.GetAgent(ctx, agent.ID)
	require.NoError(t, err)
	assert.Nil(t, updated.AppliedConfig.CreateInputs, "a PATCH must never create CreateInputs for an agent that never had it")
	// The live config must still have applied normally.
	assert.Equal(t, "new-model", updated.AppliedConfig.Model)
}

// TestApplyAgentUpdate_HarnessConfigNeverReachesCreateInputs is test-plan
// item 8: a PATCH that changes harness_config must never let that reach
// CreateInputs -- a harness switch is not a validated PATCH operation today,
// and reincarnate must not pick one up unvalidated against whatever harness
// is current at that later point.
func TestApplyAgentUpdate_HarnessConfigNeverReachesCreateInputs(t *testing.T) {
	disp := newReincarnateTestDispatcher()
	srv, s, project, broker := setupReincarnateTestServer(t, disp)
	ctx := context.Background()

	agent := newReincarnateTestAgent(t, s, project, broker, func(a *store.Agent) {
		a.Phase = string(state.PhaseCreated)
		a.AppliedConfig.CreateInputs = &store.AgentCreateInputs{
			HarnessConfig: "original-harness-config",
			InlineConfig:  &api.ScionConfig{HarnessConfig: "original-harness-config"},
		}
	})

	rec := patchAgentConfig(t, srv, agent.ID, map[string]interface{}{
		"harness_config": "new-harness-config",
	})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	updated, err := s.GetAgent(ctx, agent.ID)
	require.NoError(t, err)
	ci := updated.AppliedConfig.CreateInputs
	require.NotNil(t, ci)
	assert.Equal(t, "original-harness-config", ci.HarnessConfig,
		"a PATCHed harness_config must never reach CreateInputs.HarnessConfig")
	require.NotNil(t, ci.InlineConfig)
	assert.Equal(t, "original-harness-config", ci.InlineConfig.HarnessConfig,
		"a PATCHed harness_config must never reach CreateInputs.InlineConfig.HarnessConfig either")
}
