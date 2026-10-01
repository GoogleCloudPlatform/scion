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
	data, err := os.ReadFile(filepath.Join("testdata", "configure-untouched-body.json"))
	require.NoError(t, err)
	var body map[string]interface{}
	require.NoError(t, json.Unmarshal(data, &body))
	return body
}

// TestApplyAgentUpdate_UntouchedSaveLeavesHubTelemetryAndEnvAlone is R1-1's
// hub-side regression test, tightened per review round 2 (R2-2): a live
// config with a REALISTIC hub-stamped telemetry config (Cloud.Endpoint set,
// not just Enabled) and live Env/InlineConfig populated the way create
// leaves them, PATCHed with configureUntouchedBody (the real buildConfig
// output for an untouched form, not a hand-written approximation), must
// leave CreateInputs and live Env untouched, and must never record telemetry
// into CreateInputs -- the one thing Option C (recordExplicitEdits) actually
// controls.
//
// It does NOT assert that live AppliedConfig.InlineConfig.Telemetry
// survives: every config PATCH (even this "untouched" one) replaces
// InlineConfig wholesale with exactly what the request decoded to
// (options.md §7.2, pre-existing, out of scope for this PR), so a request
// body that never mentions "telemetry" drops it from the LIVE InlineConfig
// on the very first PATCH, independent of recordExplicitEdits. Both
// `assert.Nil(...InlineConfig.Telemetry)` calls below document that known,
// unrelated effect rather than warn about a regression.
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
	assert.Nil(t, updated.AppliedConfig.InlineConfig.Env,
		"documents the pre-existing wholesale-InlineConfig-replace side effect (options.md §7.2, not fixed by this PR): "+
			"InlineConfig.Env does go nil on an untouched save. The web-side R2-1 fix is what keeps populateForm reading "+
			"the real auto-expose value back from the live AppliedConfig.Env regardless, not this.")

	// Step 2: a reload-shaped PATCH (as the FIXED buildConfig would send
	// after re-loading the now-ic.Env-nil agent and editing the one custom
	// row) must not synthesize a SCION_AUTO_EXPOSE_* key that was never live
	// -- R2-1 facet (a).
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
	// The live InlineConfig.Telemetry is, separately, ALSO nil at this point
	// -- but that is the pre-existing wholesale-InlineConfig-replace effect
	// (options.md §7.2: every config PATCH overwrites InlineConfig with
	// exactly what the request decoded to, dropping any field the request
	// didn't send, including on step 1's "untouched" PATCH). That loss is
	// unrelated to recordExplicitEdits/CreateInputs and out of scope for
	// this PR; asserted here only so a future §7.2 fix has a test that
	// notices the behavior changing.
	assert.Nil(t, final.AppliedConfig.InlineConfig.Telemetry)
}

// TestApplyAgentUpdate_ReloadAfterUntouchedSavePreservesLiveAutoExposeValue
// is R2-1 facet (b)'s dedicated regression: an agent whose live env DOES
// have an explicit auto-expose key (true) must keep it true across an
// untouched Save (step 1) followed by an unrelated custom-row edit (step 2,
// sent as the FIXED buildConfig would after re-loading from the now-nil
// InlineConfig.Env and reading the real value back from AppliedConfig.Env).
// Before the fix, step 2 would have sent the global default (false) instead
// of the real live value (true), silently flipping the live setting off and
// recording the flip into CreateInputs.
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
	assert.Nil(t, mid.AppliedConfig.InlineConfig.Env, "sanity check: InlineConfig.Env does go nil after the untouched save")

	// Step 2: the FIXED page reloads, reads SCION_AUTO_EXPOSE_PORTS=true back
	// from the live AppliedConfig.Env (not the now-nil InlineConfig.Env), and
	// re-sends that same value while the user edits the unrelated K row.
	rec2 := patchAgentConfig(t, srv, agent.ID, map[string]interface{}{
		"env": map[string]interface{}{"K": "v2", "SCION_AUTO_EXPOSE_PORTS": "true"},
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
		"re-sending the unchanged real value must not be misrecorded or lost")
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
