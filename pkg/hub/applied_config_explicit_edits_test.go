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
	"os"
	"path/filepath"
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

// TestApplyAgentUpdate_TaskNeverReachesCreateInputs fills a gap in the
// CreateInputs entry-path enumeration (round 6, tz-lead requirement, see the
// PR body): Task is excluded from CreateInputs by design
// (AgentCreateInputs' doc comment, pkg/store/models.go -- reincarnate's
// hub-built preamble plus handoff always replaces it), but until now nothing
// PATCHed a changed task and asserted it never shows up in CI.
func TestApplyAgentUpdate_TaskNeverReachesCreateInputs(t *testing.T) {
	disp := newReincarnateTestDispatcher()
	srv, s, project, broker := setupReincarnateTestServer(t, disp)
	ctx := context.Background()

	agent := newReincarnateTestAgent(t, s, project, broker, func(a *store.Agent) {
		a.Phase = string(state.PhaseCreated)
		a.AppliedConfig.Task = "original task"
		a.AppliedConfig.CreateInputs = &store.AgentCreateInputs{}
	})

	rec := patchAgentConfig(t, srv, agent.ID, map[string]interface{}{
		"task": "a brand new task",
	})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	updated, err := s.GetAgent(ctx, agent.ID)
	require.NoError(t, err)
	assert.Equal(t, "a brand new task", updated.AppliedConfig.Task, "the live write must still apply the new task")
	ci := updated.AppliedConfig.CreateInputs
	require.NotNil(t, ci)
	assert.Nil(t, ci.InlineConfig, "a PATCHed task must never reach CreateInputs at all -- AgentCreateInputs has no Task field to record it in")
}

// TestApplyAgentUpdate_HarnessAuthChangeIsRecorded fills a gap in the
// CreateInputs entry-path enumeration (round 6, tz-lead requirement, see the
// PR body): TestApplyAgentUpdate_EchoPatchLeavesCreateInputsByteIdentical
// covers an unchanged auth_selectedType echo, but until now nothing proved a
// genuine HarnessAuth CHANGE is recorded into CreateInputs (special-cased in
// recordExplicitEdits against old.HarnessAuth, "" meaning unchanged).
func TestApplyAgentUpdate_HarnessAuthChangeIsRecorded(t *testing.T) {
	disp := newReincarnateTestDispatcher()
	srv, s, project, broker := setupReincarnateTestServer(t, disp)
	ctx := context.Background()

	agent := newReincarnateTestAgent(t, s, project, broker, func(a *store.Agent) {
		a.Phase = string(state.PhaseCreated)
		a.AppliedConfig.HarnessAuth = "api-key"
		a.AppliedConfig.CreateInputs = &store.AgentCreateInputs{}
	})

	rec := patchAgentConfig(t, srv, agent.ID, map[string]interface{}{
		"auth_selectedType": "vertex-ai",
	})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	updated, err := s.GetAgent(ctx, agent.ID)
	require.NoError(t, err)
	assert.Equal(t, "vertex-ai", updated.AppliedConfig.HarnessAuth)
	ci := updated.AppliedConfig.CreateInputs
	require.NotNil(t, ci)
	assert.Equal(t, "vertex-ai", ci.HarnessAuth, "a genuine HarnessAuth change must be recorded into CreateInputs.HarnessAuth")
	require.NotNil(t, ci.InlineConfig)
	assert.Equal(t, "vertex-ai", ci.InlineConfig.AuthSelectedType, "...and mirrored into CreateInputs.InlineConfig.AuthSelectedType")
}

// TestApplyAgentUpdate_ImageChangeIsRecorded fills a gap in the CreateInputs
// entry-path enumeration (round 6, tz-lead requirement, see the PR body):
// TestApplyAgentUpdate_ImageCompareCanonicalizesBothSides and
// TestApplyAgentUpdate_EchoPatchLeavesCreateInputsByteIdentical both cover an
// unchanged image echo, but until now nothing proved a genuine Image CHANGE
// is recorded (special-cased in recordExplicitEdits against old.Image,
// canonicalized via RewriteImageRegistry, "" meaning unchanged).
func TestApplyAgentUpdate_ImageChangeIsRecorded(t *testing.T) {
	disp := newReincarnateTestDispatcher()
	srv, s, project, broker := setupReincarnateTestServer(t, disp)
	ctx := context.Background()

	agent := newReincarnateTestAgent(t, s, project, broker, func(a *store.Agent) {
		a.Phase = string(state.PhaseCreated)
		a.AppliedConfig.Image = "old-image:v1"
		a.AppliedConfig.CreateInputs = &store.AgentCreateInputs{}
	})

	rec := patchAgentConfig(t, srv, agent.ID, map[string]interface{}{
		"image": "new-image:v2",
	})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	updated, err := s.GetAgent(ctx, agent.ID)
	require.NoError(t, err)
	assert.Equal(t, "new-image:v2", updated.AppliedConfig.Image)
	ci := updated.AppliedConfig.CreateInputs
	require.NotNil(t, ci)
	require.NotNil(t, ci.InlineConfig)
	assert.Equal(t, "new-image:v2", ci.InlineConfig.Image, "a genuine Image change must be recorded into CreateInputs.InlineConfig.Image")
}

// ============================================================================
// Review round 1 (gs://scion-xproject-exchange/tz-refactor/out/2493/review-1.md)
// ============================================================================

// configureUntouchedBody loads the golden fixture shared with
// agent-configure-build-config.test.ts's "R2-2" vitest case
// (web/src/components/pages/agent-configure-build-config.test.ts): the exact
// JSON body the real, fixed buildConfig emits for a fully untouched form
// loaded from a live config with model "golden-model" and nothing else set.
// Loading the SAME file in both places means a future buildConfig change
// that stops matching it breaks the vitest case directly, instead of
// leaving this Go test to silently test a body nobody's buildConfig
// actually produces anymore (ptone/scion#2493 R2-2).
func configureUntouchedBody(t *testing.T) map[string]interface{} {
	t.Helper()
	return loadTestdataJSONBody(t, "configure-untouched-body.json")
}

// configureRowEditBody loads the golden fixture shared with
// agent-configure-build-config.test.ts's row-edit vitest case: the exact body
// the real buildConfig emits when the user adds one custom env row (FOO) on
// an agent whose AppliedConfig.Env has an unrelated template key
// (TEMPLATE_KEY) and whose auto-expose control is untouched, so no
// SCION_AUTO_EXPOSE_* key is sent. Loading the SAME file in both places means
// a future buildConfig change that stops matching it breaks the vitest case
// directly.
func configureRowEditBody(t *testing.T) map[string]interface{} {
	t.Helper()
	return loadTestdataJSONBody(t, "configure-row-edit-body.json")
}

func loadTestdataJSONBody(t *testing.T, name string) map[string]interface{} {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", name))
	require.NoError(t, err)
	var body map[string]interface{}
	require.NoError(t, json.Unmarshal(data, &body))
	return body
}

// TestApplyAgentUpdate_UntouchedSaveLeavesHubTelemetryAndEnvAlone is R1-1's
// hub-side regression test, tightened per review round 2 (R2-2), round 3
// (R3-1) and round 4 (R4-1): a live config with a REALISTIC hub-stamped
// telemetry config (Cloud.Endpoint set, not just Enabled) and live
// Env/InlineConfig populated the way create leaves them, PATCHed with
// configureUntouchedBody (the real buildConfig output for an untouched
// form, not a hand-written approximation), must leave CreateInputs and live
// Env untouched, must never record telemetry OR env into CreateInputs (the
// one thing Option C / recordExplicitEdits controls), AND must leave the
// LIVE AppliedConfig.InlineConfig.Telemetry and .Env themselves untouched
// too.
//
// That last part is carryForwardAbsentPageOwnedFields' job
// (applied_config_explicit_edits.go), not recordExplicitEdits: once
// buildConfig stopped echoing an untouched telemetry control (R1-1) or an
// untouched env (R2-1), the unconditional wholesale InlineConfig replace in
// applyAgentUpdate would otherwise wipe both live fields -- including an
// explicit telemetry opt-out, and (for a legacy agent with no CreateInputs)
// every explicit env key `scion reincarnate` has no other record of at all
// (R4-1) -- on a plain Start with no Save, since the request never mentions
// either key at all. The carve-out (right before `agent.AppliedConfig.
// InlineConfig = cfg`) copies both forward from the pre-PATCH InlineConfig
// whenever their key was absent from the request; this runs AFTER
// recordExplicitEdits, so CreateInputs still correctly never sees either as
// explicit. Earlier versions of this test asserted the opposite for each
// field in turn (`assert.Nil(...InlineConfig.Telemetry)` then
// `assert.Nil(...InlineConfig.Env)`, both calling the loss "pre-existing
// §7.2") -- that was wrong both times; §7.2 is the general wholesale
// InlineConfig replace, but both fields had always survived a configure-page
// round trip before R1-1/R2-1 made them (correctly) stop being echoed, so
// their being wiped was this PR's own regression, not a pre-existing one.
//
// Also covers R2-1 facet (a)'s two-step sequence: this fixture's live env
// never had a SCION_AUTO_EXPOSE_* key, so after the untouched Save (step 1)
// a second PATCH that only edits the one custom env key (step 2, as the
// FIXED buildConfig would send after a reload -- see
// agent-configure-build-config.test.ts's matching "facet (a)" vitest case)
// must not synthesize one.
func TestApplyAgentUpdate_UntouchedSaveLeavesHubTelemetryAndEnvAlone(t *testing.T) {
	disp := newReincarnateTestDispatcher()
	srv, s, project, broker := setupReincarnateTestServer(t, disp)
	ctx := context.Background()

	enabled := true
	agent := newReincarnateTestAgent(t, s, project, broker, func(a *store.Agent) {
		a.Phase = string(state.PhaseCreated)
		a.AppliedConfig.Model = "golden-model"
		// "As create leaves them": InlineConfig.Env mirrors live Env (the
		// create path aliases the two; see handlers_agent_create_helpers.go),
		// and InlineConfig.Model matches the live Model.
		a.AppliedConfig.Env = map[string]string{"EXPLICIT_KEY": "explicit-value"}
		a.AppliedConfig.InlineConfig = &api.ScionConfig{
			Model: "golden-model",
			Env:   map[string]string{"EXPLICIT_KEY": "explicit-value"},
			Telemetry: &api.TelemetryConfig{
				Enabled: &enabled,
				Cloud:   &api.TelemetryCloudConfig{Endpoint: "https://telemetry.example.com"},
			},
		}
		a.AppliedConfig.CreateInputs = &store.AgentCreateInputs{}
	})

	before, err := json.Marshal(agent.AppliedConfig.CreateInputs)
	require.NoError(t, err)
	beforeEnv, err := json.Marshal(agent.AppliedConfig.Env)
	require.NoError(t, err)

	// Step 1: the untouched-form body, byte-for-byte what the real buildConfig
	// emits (loaded from the shared golden fixture).
	rec := patchAgentConfig(t, srv, agent.ID, configureUntouchedBody(t))
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	updated, err := s.GetAgent(ctx, agent.ID)
	require.NoError(t, err)
	after, err := json.Marshal(updated.AppliedConfig.CreateInputs)
	require.NoError(t, err)
	assert.JSONEq(t, string(before), string(after),
		"an untouched Save must leave CreateInputs (including hub telemetry) alone")
	afterEnv, err := json.Marshal(updated.AppliedConfig.Env)
	require.NoError(t, err)
	assert.JSONEq(t, string(beforeEnv), string(afterEnv), "an untouched Save must leave live Env alone")
	require.NotNil(t, updated.AppliedConfig.InlineConfig)
	// R4-1: live InlineConfig.Env must survive an untouched Save/Start --
	// carryForwardAbsentPageOwnedFields copies it forward from the pre-PATCH
	// InlineConfig whenever the request omits "env", specifically so a
	// legacy agent (no CreateInputs) doesn't lose every explicit env key the
	// next time it's reincarnated (legacyCreateInputsFromAppliedConfig reads
	// exactly this field).
	require.NotNil(t, updated.AppliedConfig.InlineConfig.Env, "live InlineConfig.Env must survive an untouched Save/Start")
	assert.Equal(t, "explicit-value", updated.AppliedConfig.InlineConfig.Env["EXPLICIT_KEY"])
	// R3-1: live Telemetry must survive too -- applyAgentUpdate's narrow
	// carve-out copies it forward whenever the request never mentions
	// "telemetry", specifically so a plain Start never silently undoes an
	// explicit opt-out or a project's TelemetryEnabled stamp.
	require.NotNil(t, updated.AppliedConfig.InlineConfig.Telemetry, "live hub telemetry must survive an untouched Save/Start")
	require.NotNil(t, updated.AppliedConfig.InlineConfig.Telemetry.Enabled)
	assert.True(t, *updated.AppliedConfig.InlineConfig.Telemetry.Enabled)
	require.NotNil(t, updated.AppliedConfig.InlineConfig.Telemetry.Cloud)
	assert.Equal(t, "https://telemetry.example.com", updated.AppliedConfig.InlineConfig.Telemetry.Cloud.Endpoint)

	// Step 2: a reload-shaped PATCH (as the FIXED buildConfig would send
	// after re-loading the agent -- whose InlineConfig.Env the R4-1
	// carve-out kept equal to EXPLICIT_KEY, same as its live AppliedConfig.Env
	// -- and editing the one custom row) must not synthesize a
	// SCION_AUTO_EXPOSE_* key that was never live -- R2-1 facet (a).
	step2 := configureUntouchedBody(t)
	step2["env"] = map[string]interface{}{"EXPLICIT_KEY": "changed-value"}
	rec2 := patchAgentConfig(t, srv, agent.ID, step2)
	require.Equal(t, http.StatusOK, rec2.Code, rec2.Body.String())

	final, err := s.GetAgent(ctx, agent.ID)
	require.NoError(t, err)
	assert.Equal(t, "changed-value", final.AppliedConfig.Env["EXPLICIT_KEY"])
	assert.NotContains(t, final.AppliedConfig.Env, "SCION_AUTO_EXPOSE_PORTS",
		"a fixture whose live env never had an auto-expose key must never gain one just because an unrelated row changed")
	require.NotNil(t, final.AppliedConfig.CreateInputs.InlineConfig)
	assert.Equal(t, "changed-value", final.AppliedConfig.CreateInputs.InlineConfig.Env["EXPLICIT_KEY"])
	assert.NotContains(t, final.AppliedConfig.CreateInputs.InlineConfig.Env, "SCION_AUTO_EXPOSE_PORTS")
	// recordExplicitEdits never saw a "telemetry" key in either step's body
	// (buildConfig only sends it when the user actually toggles it), so
	// CreateInputs never picks it up as explicit -- the one thing Option C
	// controls here.
	assert.Nil(t, final.AppliedConfig.CreateInputs.InlineConfig.Telemetry,
		"telemetry was never present in either PATCH body, so CreateInputs must never record it")
	// R3-1: the live InlineConfig.Telemetry must ALSO still be the real hub
	// config after step 2, not just step 1 -- the carve-out runs on every
	// config PATCH that omits "telemetry", not just the first one.
	require.NotNil(t, final.AppliedConfig.InlineConfig.Telemetry, "live hub telemetry must survive the untouched-then-edited sequence")
	require.NotNil(t, final.AppliedConfig.InlineConfig.Telemetry.Enabled)
	assert.True(t, *final.AppliedConfig.InlineConfig.Telemetry.Enabled)
	require.NotNil(t, final.AppliedConfig.InlineConfig.Telemetry.Cloud)
	assert.Equal(t, "https://telemetry.example.com", final.AppliedConfig.InlineConfig.Telemetry.Cloud.Endpoint)
}

// TestApplyAgentUpdate_UntouchedSavePreservesExplicitTelemetryOptOut is R3-1's
// dedicated regression for the opt-out case the review called out
// specifically: live telemetry already explicitly disabled
// (Enabled: false) must still read false after an untouched Save, not
// silently revert to whatever the broker settings or template says once
// InlineConfig.Telemetry would otherwise go nil.
func TestApplyAgentUpdate_UntouchedSavePreservesExplicitTelemetryOptOut(t *testing.T) {
	disp := newReincarnateTestDispatcher()
	srv, s, project, broker := setupReincarnateTestServer(t, disp)
	ctx := context.Background()

	disabled := false
	agent := newReincarnateTestAgent(t, s, project, broker, func(a *store.Agent) {
		a.Phase = string(state.PhaseCreated)
		a.AppliedConfig.Model = "golden-model"
		a.AppliedConfig.InlineConfig = &api.ScionConfig{
			Model:     "golden-model",
			Telemetry: &api.TelemetryConfig{Enabled: &disabled},
		}
		a.AppliedConfig.CreateInputs = &store.AgentCreateInputs{}
	})

	before, err := json.Marshal(agent.AppliedConfig.CreateInputs)
	require.NoError(t, err)

	rec := patchAgentConfig(t, srv, agent.ID, configureUntouchedBody(t))
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	updated, err := s.GetAgent(ctx, agent.ID)
	require.NoError(t, err)
	after, err := json.Marshal(updated.AppliedConfig.CreateInputs)
	require.NoError(t, err)
	assert.JSONEq(t, string(before), string(after), "an untouched Save must leave CreateInputs alone")

	require.NotNil(t, updated.AppliedConfig.InlineConfig)
	require.NotNil(t, updated.AppliedConfig.InlineConfig.Telemetry)
	require.NotNil(t, updated.AppliedConfig.InlineConfig.Telemetry.Enabled)
	assert.False(t, *updated.AppliedConfig.InlineConfig.Telemetry.Enabled,
		"an explicit telemetry opt-out must survive an untouched Save, not silently fall back to broker settings/template")
}

// TestApplyAgentUpdate_UntouchedSaveThenReincarnateKeepsLegacyAgentExplicitEnv
// is R4-1's dedicated regression, the exact scenario options.md §5's test
// plan and the "Legacy agent (no CI): No-op" edge case both depend on: an
// agent with NO CreateInputs (predates the field, or was created before it
// existed) relies entirely on legacyCreateInputsFromAppliedConfig
// (reincarnate_config.go) reading its LIVE InlineConfig.Env back at
// reincarnate time to recover its explicit env -- there is no CreateInputs
// record to fall back on. Before the R4-1 carve-out, an untouched Save/Start
// would wipe InlineConfig.Env via the wholesale replace, and reincarnate
// would then silently lose every one of this agent's explicit env keys with
// no warning.
func TestApplyAgentUpdate_UntouchedSaveThenReincarnateKeepsLegacyAgentExplicitEnv(t *testing.T) {
	disp := newReincarnateTestDispatcher()
	srv, s, project, broker := setupReincarnateTestServer(t, disp)
	ctx := context.Background()

	agent := newReincarnateTestAgent(t, s, project, broker, func(a *store.Agent) {
		a.Phase = string(state.PhaseCreated)
		a.AppliedConfig.Model = "golden-model"
		a.AppliedConfig.Env = map[string]string{"FOO": "bar"}
		a.AppliedConfig.InlineConfig = &api.ScionConfig{
			Model: "golden-model",
			Env:   map[string]string{"FOO": "bar"},
		}
		// The defining condition for this test: no CreateInputs at all.
		a.AppliedConfig.CreateInputs = nil
	})

	rec := patchAgentConfig(t, srv, agent.ID, configureUntouchedBody(t))
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	updated, err := s.GetAgent(ctx, agent.ID)
	require.NoError(t, err)
	assert.Nil(t, updated.AppliedConfig.CreateInputs, "an untouched Save must never create CreateInputs for a legacy agent")
	require.NotNil(t, updated.AppliedConfig.InlineConfig)
	assert.Equal(t, "bar", updated.AppliedConfig.InlineConfig.Env["FOO"],
		"live InlineConfig.Env must survive the untouched Save -- it is the ONLY record a legacy agent has of this explicit env key")

	fresh, _, err := srv.buildFreshAppliedConfig(ctx, updated, project, "")
	require.NoError(t, err)
	assert.Equal(t, "bar", fresh.Env["FOO"], "reincarnate must still recover the legacy agent's explicit env key after an untouched Save")
}

// TestApplyAgentUpdate_ReloadAfterUntouchedSavePreservesLiveAutoExposeValue
// pins that an explicit auto-expose value (true, in all three maps) survives
// an untouched Save (step 1, no env key) followed by an unrelated custom-row
// edit (step 2, env without auto-expose keys, as buildConfig sends it when
// the control is untouched): live, InlineConfig and CreateInputs all keep it.
func TestApplyAgentUpdate_ReloadAfterUntouchedSavePreservesLiveAutoExposeValue(t *testing.T) {
	disp := newReincarnateTestDispatcher()
	srv, s, project, broker := setupReincarnateTestServer(t, disp)
	ctx := context.Background()

	agent := newReincarnateTestAgent(t, s, project, broker, func(a *store.Agent) {
		a.Phase = string(state.PhaseCreated)
		a.AppliedConfig.Env = map[string]string{"K": "v", "SCION_AUTO_EXPOSE_PORTS": "true"}
		a.AppliedConfig.InlineConfig = &api.ScionConfig{
			Env: map[string]string{"K": "v", "SCION_AUTO_EXPOSE_PORTS": "true"},
		}
		a.AppliedConfig.CreateInputs = &store.AgentCreateInputs{
			InlineConfig: &api.ScionConfig{Env: map[string]string{"K": "v", "SCION_AUTO_EXPOSE_PORTS": "true"}},
		}
	})

	// Step 1: untouched Save (no env key at all).
	rec := patchAgentConfig(t, srv, agent.ID, map[string]interface{}{
		"thinking_level":     nil,
		"branch":             "",
		"user":               "",
		"agent_instructions": "",
		"system_prompt":      "",
	})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	mid, err := s.GetAgent(ctx, agent.ID)
	require.NoError(t, err)
	assert.Equal(t, "true", mid.AppliedConfig.Env["SCION_AUTO_EXPOSE_PORTS"], "step 1 must leave live auto-expose unchanged")
	require.NotNil(t, mid.AppliedConfig.InlineConfig)
	// R4-1: InlineConfig.Env must now ALSO survive the untouched save,
	// carried forward by carryForwardAbsentPageOwnedFields.
	require.NotNil(t, mid.AppliedConfig.InlineConfig.Env, "InlineConfig.Env must survive the untouched save")
	assert.Equal(t, "true", mid.AppliedConfig.InlineConfig.Env["SCION_AUTO_EXPOSE_PORTS"])

	// Step 2: the page reloads and the user edits the unrelated K row; the
	// untouched auto-expose control sends nothing.
	rec2 := patchAgentConfig(t, srv, agent.ID, map[string]interface{}{
		"env": map[string]interface{}{"K": "v2"},
	})
	require.Equal(t, http.StatusOK, rec2.Code, rec2.Body.String())

	final, err := s.GetAgent(ctx, agent.ID)
	require.NoError(t, err)
	assert.Equal(t, "true", final.AppliedConfig.Env["SCION_AUTO_EXPOSE_PORTS"],
		"live auto-expose must still be true, never silently flipped to the global default")
	assert.Equal(t, "v2", final.AppliedConfig.Env["K"])
	require.NotNil(t, final.AppliedConfig.CreateInputs.InlineConfig)
	assert.Equal(t, "v2", final.AppliedConfig.CreateInputs.InlineConfig.Env["K"], "the real edit must still be recorded")
	assert.Equal(t, "true", final.AppliedConfig.CreateInputs.InlineConfig.Env["SCION_AUTO_EXPOSE_PORTS"],
		"an untouched explicit auto-expose value must not be lost")
}

// TestApplyAgentUpdate_ImageCompareCanonicalizesBothSides is R1-2: old.Image
// is registry-qualified only after a broker echo; a `created`-phase agent
// whose image came bare from a template still has a bare old.Image. A bare
// echo of that bare image must not be recorded as a diff just because the
// request side gets canonicalised and the live side doesn't.
func TestApplyAgentUpdate_ImageCompareCanonicalizesBothSides(t *testing.T) {
	disp := newReincarnateTestDispatcher()
	disp.imageRegistry = "registry.example.com"
	srv, s, project, broker := setupReincarnateTestServer(t, disp)
	ctx := context.Background()

	agent := newReincarnateTestAgent(t, s, project, broker, func(a *store.Agent) {
		a.Phase = string(state.PhaseCreated)
		a.AppliedConfig.Image = "old-image:v1" // bare: never broker-echoed
		a.AppliedConfig.CreateInputs = &store.AgentCreateInputs{}
	})

	rec := patchAgentConfig(t, srv, agent.ID, map[string]interface{}{
		"image": "old-image:v1", // echoed bare, exactly as loaded
	})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	updated, err := s.GetAgent(ctx, agent.ID)
	require.NoError(t, err)
	if ci := updated.AppliedConfig.CreateInputs; ci.InlineConfig != nil {
		assert.Empty(t, ci.InlineConfig.Image,
			"a bare echo of a bare live image (both canonicalising to the same registry-qualified form) must not be recorded")
	}
}

// TestApplyAgentUpdate_EnvRemovalSkippedWithoutAttach is R1-3's first case: a
// caller with agent.update but not agent.attach (a project-owner/admin who
// is not the agent's creator, per projectOwnerPermissionIDs/
// projectAdminPermissionIDs excluding agent.attach, miller79/scion#88) gets
// an empty Env from the GET response (ResponseView/canViewAgentEnv), so
// their client's echo cannot be trusted to list every key that still exists
// live. An empty env PATCH from that caller must not wipe the agent's
// existing explicit CreateInputs env.
func TestApplyAgentUpdate_EnvRemovalSkippedWithoutAttach(t *testing.T) {
	disp := newReincarnateTestDispatcher()
	srv, s, project, broker := setupReincarnateTestServer(t, disp)
	ctx := context.Background()
	srv.createProjectMembersGroup(ctx, project)

	owner := makeProjectMemberUser(t, s, project, tid("project-owner-no-attach"), "Owner", store.GroupMemberRoleOwner)

	agent := newReincarnateTestAgent(t, s, project, broker, func(a *store.Agent) {
		a.Phase = string(state.PhaseCreated)
		// Owned/created by someone else, so the caller gets no attach via
		// the resource-owner/ancestor relationship either.
		a.OwnerID = tid("agent-creator")
		a.CreatedBy = tid("agent-creator")
		a.Ancestry = []string{tid("agent-creator")}
		a.AppliedConfig.Env = map[string]string{"FOO": "bar"}
		a.AppliedConfig.InlineConfig = &api.ScionConfig{Env: map[string]string{"FOO": "bar"}}
		a.AppliedConfig.CreateInputs = &store.AgentCreateInputs{
			InlineConfig: &api.ScionConfig{Env: map[string]string{"FOO": "bar"}},
		}
	})

	// The caller's GET would have seen an empty Env (no attach), so their
	// client echoes env back empty -- but cfg.Env is still non-nil (an empty
	// object, not an absent key), which is what makes this scenario distinct
	// from "didn't touch env at all".
	rec := doRequestAsUser(t, srv, owner, http.MethodPatch, "/api/v1/agents/"+agent.ID,
		map[string]interface{}{"config": map[string]interface{}{"env": map[string]interface{}{}}})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	updated, err := s.GetAgent(ctx, agent.ID)
	require.NoError(t, err)
	ci := updated.AppliedConfig.CreateInputs
	require.NotNil(t, ci)
	require.NotNil(t, ci.InlineConfig)
	assert.Equal(t, "bar", ci.InlineConfig.Env["FOO"],
		"an empty env echo from a caller without attach must not be read as the user removing every env key")
}

// TestApplyAgentUpdate_GitHubTokenNeverTreatedAsRemoved is R1-3's second
// case: GITHUB_TOKEN is stripped from EVERY API response unconditionally
// (store.AgentAppliedConfig's MarshalJSON / ResponseView doc comment), even
// for an attach-capable caller, so its absence from any request's env map is
// never evidence that the user removed it -- regardless of attach status.
func TestApplyAgentUpdate_GitHubTokenNeverTreatedAsRemoved(t *testing.T) {
	disp := newReincarnateTestDispatcher()
	srv, s, project, broker := setupReincarnateTestServer(t, disp)
	ctx := context.Background()

	agent := newReincarnateTestAgent(t, s, project, broker, func(a *store.Agent) {
		a.Phase = string(state.PhaseCreated)
		a.AppliedConfig.Env = map[string]string{"GITHUB_TOKEN": "ghp_secret", "FOO": "bar"}
		a.AppliedConfig.InlineConfig = &api.ScionConfig{Env: map[string]string{"GITHUB_TOKEN": "ghp_secret", "FOO": "bar"}}
		a.AppliedConfig.CreateInputs = &store.AgentCreateInputs{
			InlineConfig: &api.ScionConfig{Env: map[string]string{"GITHUB_TOKEN": "ghp_secret", "FOO": "bar"}},
		}
	})

	// doRequest uses the dev-admin token (attach-capable), echoing the
	// visible FOO key but -- as every caller must, since the GET response
	// never includes it -- omitting GITHUB_TOKEN.
	rec := patchAgentConfig(t, srv, agent.ID, map[string]interface{}{
		"env": map[string]interface{}{"FOO": "bar"},
	})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	updated, err := s.GetAgent(ctx, agent.ID)
	require.NoError(t, err)
	ci := updated.AppliedConfig.CreateInputs
	require.NotNil(t, ci)
	require.NotNil(t, ci.InlineConfig)
	assert.Equal(t, "ghp_secret", ci.InlineConfig.Env["GITHUB_TOKEN"],
		"GITHUB_TOKEN must never be treated as removed, since no caller's response ever includes it to echo back")
}

// TestApplyAgentUpdate_PresenceDetectionIsCaseInsensitive is R1-5:
// encoding/json matches struct field names case-insensitively when there is
// no exact match, so a non-canonical-case request key must still count as
// "present" -- otherwise the field decodes and changes the live value, but
// recordExplicitEdits silently drops it from CreateInputs because its
// lower-cased presence check misses the differently-cased raw key.
func TestApplyAgentUpdate_PresenceDetectionIsCaseInsensitive(t *testing.T) {
	disp := newReincarnateTestDispatcher()
	srv, s, project, broker := setupReincarnateTestServer(t, disp)
	ctx := context.Background()

	agent := newReincarnateTestAgent(t, s, project, broker, func(a *store.Agent) {
		a.Phase = string(state.PhaseCreated)
		a.AppliedConfig.CreateInputs = &store.AgentCreateInputs{}
	})

	rec := patchAgentConfig(t, srv, agent.ID, map[string]interface{}{
		"System_Prompt": "be helpful", // non-canonical case
	})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	updated, err := s.GetAgent(ctx, agent.ID)
	require.NoError(t, err)
	ci := updated.AppliedConfig.CreateInputs
	require.NotNil(t, ci)
	require.NotNil(t, ci.InlineConfig)
	assert.Equal(t, "be helpful", ci.InlineConfig.SystemPrompt,
		"a non-canonical-case JSON key must still count as present, matching encoding/json's own case-insensitive field match")
}

// ============================================================================
// Review round 6 (FINAL) (gs://scion-xproject-exchange/tz-refactor/out/2493/review-6.md)
// ============================================================================

// TestApplyAgentUpdate_ModelAliasComparedAfterResolution is R6-2: the
// enumeration's Model row states the compare runs after alias resolution
// (cfg.Model is reassigned to the resolved value in applyAgentUpdate before
// the `old` snapshot and the recordExplicitEdits call), but nothing had
// pinned that directly -- every existing Model test used a literal model ID
// on both sides. If resolution were ever moved to run after the diff
// instead, re-picking a size tier that already resolves to the live model
// would wrongly be recorded as the raw alias string (e.g. "medium") instead
// of being recognised as unchanged, and no test would have failed.
func TestApplyAgentUpdate_ModelAliasComparedAfterResolution(t *testing.T) {
	disp := newReincarnateTestDispatcher()
	srv, s, project, broker := setupReincarnateTestServer(t, disp)
	ctx := context.Background()

	hc := &store.HarnessConfig{
		ID:          tid("hc-model-alias-" + t.Name()),
		Name:        "hc",
		Slug:        "model-alias-hc-" + tidSlugSafe(t.Name()),
		Harness:     "claude",
		Scope:       store.HarnessConfigScopeGlobal,
		Status:      store.HarnessConfigStatusActive,
		ContentHash: "hc-hash-v1",
		Config: &store.HarnessConfigData{
			ModelAliases: map[string]string{"medium": "model-m", "large": "model-l"},
		},
	}
	require.NoError(t, s.CreateHarnessConfig(ctx, hc))

	agent := newReincarnateTestAgent(t, s, project, broker, func(a *store.Agent) {
		a.Phase = string(state.PhaseCreated)
		a.AppliedConfig.Model = "model-m" // the ALREADY-RESOLVED live model
		a.AppliedConfig.HarnessConfigID = hc.ID
		a.AppliedConfig.CreateInputs = &store.AgentCreateInputs{}
	})

	// Re-picking the tier that already resolves to the live model must be a
	// no-op: "medium" resolves to "model-m", which equals old.Model.
	rec := patchAgentConfig(t, srv, agent.ID, map[string]interface{}{
		"model": "medium",
	})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	updated, err := s.GetAgent(ctx, agent.ID)
	require.NoError(t, err)
	ci := updated.AppliedConfig.CreateInputs
	require.NotNil(t, ci)
	assert.Nil(t, ci.InlineConfig, "an alias that resolves to the already-live model must not be recorded")

	// Picking a DIFFERENT tier must record the RESOLVED model, not the raw
	// alias string.
	rec2 := patchAgentConfig(t, srv, agent.ID, map[string]interface{}{
		"model": "large",
	})
	require.Equal(t, http.StatusOK, rec2.Code, rec2.Body.String())

	final, err := s.GetAgent(ctx, agent.ID)
	require.NoError(t, err)
	assert.Equal(t, "model-l", final.AppliedConfig.Model, "the live write must apply the resolved model")
	ci2 := final.AppliedConfig.CreateInputs
	require.NotNil(t, ci2)
	require.NotNil(t, ci2.InlineConfig)
	assert.Equal(t, "model-l", ci2.InlineConfig.Model,
		"CreateInputs must record the RESOLVED model id, not the raw alias string")
}
