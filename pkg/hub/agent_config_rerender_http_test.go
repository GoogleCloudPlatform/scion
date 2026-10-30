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
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// HTTP tests for ptone/scion#3982 (see agent_config_rerender_test.go).

// setRerenderingBroker gives broker the changedFields capability.
func setRerenderingBroker(t *testing.T, s store.Store, broker *store.RuntimeBroker) {
	t.Helper()
	setBrokerCaps(t, s, broker, &store.BrokerCapabilities{Reprovision: true, ChangedFields: true})
}

// setBrokerCaps replaces broker's stored capabilities.
func setBrokerCaps(t *testing.T, s store.Store, broker *store.RuntimeBroker, caps *store.BrokerCapabilities) {
	t.Helper()
	b, err := s.GetRuntimeBroker(context.Background(), broker.ID)
	require.NoError(t, err)
	b.Capabilities = caps
	require.NoError(t, s.UpdateRuntimeBroker(context.Background(), b))
}

// t2PatchBody changes all four re-rendered keys.
func t2PatchBody() map[string]interface{} {
	return map[string]interface{}{
		"config": map[string]interface{}{
			"system_prompt":      "be brief",
			"agent_instructions": "run the tests first",
			"skills":             []map[string]interface{}{{"uri": "hub://skills/review"}},
			"services":           []map[string]interface{}{{"name": "db", "command": []string{"postgres"}}},
		},
	}
}

var t2WireKeys = []string{"config.agent_instructions", "config.services", "config.skills", "config.system_prompt"}

// TestAgentConfigPatch_RerenderingBrokerAppliesT2AtStart: on a stopped
// agent and on a created agent whose broker re-renders at start, a PATCH of
// the system prompt, agent instructions, skills and services is applied
// (no reincarnation warning), the GET reports them "now", and the next
// start dispatches the new values with changed_fields naming the four keys.
func TestAgentConfigPatch_RerenderingBrokerAppliesT2AtStart(t *testing.T) {
	for _, phase := range []state.Phase{state.PhaseStopped, state.PhaseCreated} {
		t.Run(string(phase), func(t *testing.T) {
			disp := &inlineCaptureDispatcher{reincarnateTestDispatcher: newReincarnateTestDispatcher()}
			srv, s, project, broker := setupReincarnateTestServer(t, disp)
			setRerenderingBroker(t, s, broker)
			agent := newEditTestAgent(t, s, project, broker, phase)

			resp, code, body := patchAgentBody(t, srv, agent.ID, t2PatchBody())
			require.Equal(t, http.StatusOK, code, body)
			assert.Equal(t, t2WireKeys, resp.Disposition.Applied)
			assert.Empty(t, resp.Disposition.HeldForReincarnate)
			assert.Empty(t, resp.Warnings)
			assert.Nil(t, resp.AppliedConfig.InlineConfig.ChangedFields, "changed_fields is not in the response")

			stored, err := s.GetAgent(context.Background(), agent.ID)
			require.NoError(t, err)
			assert.Equal(t, []string{"agent_instructions", "services", "skills", "system_prompt"}, stored.AppliedConfig.InlineConfig.ChangedFields)

			ed := getAgentEditability(t, srv, nil, agent.ID)
			for _, k := range t2WireKeys {
				assert.Equal(t, EditNow, ed.Fields[k].Disposition, k)
			}
			assert.Equal(t, EditReincarnate, ed.Fields["config.secrets"].Disposition, "secrets are not re-rendered at start")

			rec := doRequest(t, srv, http.MethodPost, "/api/v1/agents/"+agent.ID+"/start", nil)
			require.Less(t, rec.Code, 300, rec.Body.String())
			starts := disp.snapshot()
			require.Len(t, starts, 1)
			assert.Equal(t, "be brief", starts[0].inline.SystemPrompt)
			assert.Equal(t, "run the tests first", starts[0].inline.AgentInstructions)
			assert.Equal(t, []string{"agent_instructions", "services", "skills", "system_prompt"}, starts[0].inline.ChangedFields)
			assert.Equal(t, []string{"agent_instructions", "services", "skills", "system_prompt"}, api.RerenderConfigKeys(&starts[0].inline))
		})
	}
}

// TestAgentConfigPatch_OlderBrokerHoldsT2ForReincarnation: with a broker
// that does not re-render at start, the same PATCH is reported held for
// reincarnation with the reincarnation warning, and changed_fields is
// still recorded, so a broker upgraded later applies it.
func TestAgentConfigPatch_OlderBrokerHoldsT2ForReincarnation(t *testing.T) {
	disp := &inlineCaptureDispatcher{reincarnateTestDispatcher: newReincarnateTestDispatcher()}
	srv, s, project, broker := setupReincarnateTestServer(t, disp)
	agent := newEditTestAgent(t, s, project, broker, state.PhaseStopped)

	resp, code, body := patchAgentBody(t, srv, agent.ID, t2PatchBody())
	require.Equal(t, http.StatusOK, code, body)
	assert.Empty(t, resp.Disposition.Applied)
	assert.Equal(t, t2WireKeys, resp.Disposition.HeldForReincarnate)
	require.Len(t, resp.Warnings, 1, "%v", resp.Warnings)
	assert.Contains(t, resp.Warnings[0], "rendered at the next reincarnation")

	stored, err := s.GetAgent(context.Background(), agent.ID)
	require.NoError(t, err)
	assert.Equal(t, []string{"agent_instructions", "services", "skills", "system_prompt"}, stored.AppliedConfig.InlineConfig.ChangedFields)

	ed := getAgentEditability(t, srv, nil, agent.ID)
	for _, k := range t2WireKeys {
		assert.Equal(t, EditReincarnate, ed.Fields[k].Disposition, k)
	}
}

// TestAgentConfigPatch_ChangedFieldsNeverFromRequest: a request cannot set
// changed_fields: naming a key there is refused as a fixed key (nothing is
// written), and a model edit never enters the list.
func TestAgentConfigPatch_ChangedFieldsNeverFromRequest(t *testing.T) {
	disp := &inlineCaptureDispatcher{reincarnateTestDispatcher: newReincarnateTestDispatcher()}
	srv, s, project, broker := setupReincarnateTestServer(t, disp)
	setRerenderingBroker(t, s, broker)
	agent := newEditTestAgent(t, s, project, broker, state.PhaseStopped)

	for _, list := range [][]string{{"model"}, {"system_prompt"}} {
		_, code, body := patchAgentBody(t, srv, agent.ID, map[string]interface{}{
			"config": map[string]interface{}{"changed_fields": list, "max_turns": 8},
		})
		require.Equal(t, http.StatusBadRequest, code, body)
		assert.Contains(t, body, "config.changed_fields")
	}
	resp, code, body := patchAgentBody(t, srv, agent.ID, map[string]interface{}{
		"config": map[string]interface{}{"model": "new-model", "max_turns": 8},
	})
	require.Equal(t, http.StatusOK, code, body)
	assert.Equal(t, []string{"config.max_turns", "config.model"}, resp.Disposition.Applied)
	stored, err := s.GetAgent(context.Background(), agent.ID)
	require.NoError(t, err)
	assert.Nil(t, stored.AppliedConfig.InlineConfig.ChangedFields)
	assert.Equal(t, api.IntPtr(8), stored.AppliedConfig.InlineConfig.MaxTurns)
}

// TestWarnRerenderUnsupported_StartResponseWarning: the start dispatch adds
// the warning to the start response's warnings for an older broker, and
// nothing for a broker that re-renders.
func TestWarnRerenderUnsupported_StartResponseWarning(t *testing.T) {
	_, s, project, broker := setupReincarnateTestServer(t, newReincarnateTestDispatcher())
	agent := newEditTestAgent(t, s, project, broker, state.PhaseStopped)
	inline := &api.ScionConfig{SystemPrompt: "p", ChangedFields: []string{"system_prompt"}}
	d := &HTTPAgentDispatcher{store: s}

	ctx, warns := withDispatchWarnings(context.Background())
	d.warnRerenderUnsupported(ctx, agent, inline)
	require.Len(t, warns.Warnings(), 1)
	assert.Contains(t, warns.Warnings()[0], "system_prompt: changed since the agent was provisioned")

	setRerenderingBroker(t, s, broker)
	ctx, warns = withDispatchWarnings(context.Background())
	d.warnRerenderUnsupported(ctx, agent, inline)
	assert.Empty(t, warns.Warnings())
}

// TestAgentConfigPatch_ClearingBrokerAppliesClearsAtStart: with a broker
// that applies cleared_fields (ptone/scion#4890), clearing max_turns (null)
// and removing env keys on a stopped agent is reported applied with no
// reincarnation warning, and the start dispatches the cleared paths; a
// count limit set to 0 (Unlimited) is refused, since this broker lacks
// unlimitedCountLimits (ptone/scion#5605), and a clear the broker cannot
// apply (the model) is still held.
func TestAgentConfigPatch_ClearingBrokerAppliesClearsAtStart(t *testing.T) {
	disp := &inlineCaptureDispatcher{reincarnateTestDispatcher: newReincarnateTestDispatcher()}
	srv, s, project, broker := setupReincarnateTestServer(t, disp)
	b, err := s.GetRuntimeBroker(context.Background(), broker.ID)
	require.NoError(t, err)
	b.Capabilities = &store.BrokerCapabilities{Reprovision: true, ClearedFields: true}
	require.NoError(t, s.UpdateRuntimeBroker(context.Background(), b))
	agent := newReincarnateTestAgent(t, s, project, broker, func(a *store.Agent) {
		a.Phase = string(state.PhaseStopped)
		a.AppliedConfig.Model = "old-model"
		a.AppliedConfig.InlineConfig = &api.ScionConfig{Harness: "claude", Model: "old-model", MaxTurns: api.IntPtr(3), MaxModelCalls: api.IntPtr(7), Env: map[string]string{"KEEP": "1", "DROP": "2"}}
	})

	resp, code, body := patchAgentBody(t, srv, agent.ID, map[string]interface{}{
		"config": map[string]interface{}{"max_turns": nil, "env": map[string]string{"KEEP": "1"}},
	})
	require.Equal(t, http.StatusOK, code, body)
	assert.Equal(t, []string{"config.env", "config.max_turns"}, resp.Disposition.Applied)
	assert.Empty(t, resp.Disposition.HeldForReincarnate)
	assert.Empty(t, resp.Warnings)

	// Unlimited (0) on a count limit is refused: this broker would keep
	// the inherited limit (ptone/scion#5605).
	_, code, body = patchAgentBody(t, srv, agent.ID, map[string]interface{}{
		"config": map[string]interface{}{"max_model_calls": 0},
	})
	require.Equal(t, http.StatusUnprocessableEntity, code, body)
	assert.Contains(t, body, "unlimited_not_supported_by_broker")

	resp, code, body = patchAgentBody(t, srv, agent.ID, map[string]interface{}{
		"config": map[string]interface{}{"model": ""},
	})
	require.Equal(t, http.StatusOK, code, body)
	assert.Equal(t, []string{"config.model"}, resp.Disposition.HeldForReincarnate, "a model clear is not applied at start")
	require.Len(t, resp.Warnings, 1, "%v", resp.Warnings)

	ed := getAgentEditability(t, srv, nil, agent.ID)
	assert.False(t, ed.Fields["config.resources"].ClearNeedsReincarnate)
	assert.False(t, ed.Fields["config.thinking_level"].ClearNeedsReincarnate)
	assert.False(t, ed.Fields["config.max_turns"].ClearNeedsReincarnate, "a null clear of a count limit is applied by a clearedFields broker")
	assert.True(t, ed.Fields["config.max_turns"].UnlimitedUnsupported, "this broker cannot apply Unlimited (ptone/scion#5605)")
	assert.True(t, ed.Fields["config.model"].ClearNeedsReincarnate)

	rec := doRequest(t, srv, http.MethodPost, "/api/v1/agents/"+agent.ID+"/start", nil)
	require.Less(t, rec.Code, 300, rec.Body.String())
	starts := disp.snapshot()
	require.Len(t, starts, 1)
	assert.Equal(t, []string{"env.DROP", "max_turns"}, starts[0].inline.ClearedFields)
}

// newT2EditAgent is a stopped agent whose inline config already sets all
// four re-rendered keys, two skills among them.
func newT2EditAgent(t *testing.T, s store.Store, project *store.Project, broker *store.RuntimeBroker) *store.Agent {
	t.Helper()
	return newReincarnateTestAgent(t, s, project, broker, func(a *store.Agent) {
		a.Phase = string(state.PhaseStopped)
		a.AppliedConfig.InlineConfig = &api.ScionConfig{
			Harness:           "claude",
			SystemPrompt:      "old prompt",
			AgentInstructions: "old instructions",
			Skills:            []api.SkillReference{{URI: "hub://skills/review"}, {URI: "hub://skills/test"}},
			Services:          []api.ServiceSpec{{Name: "db", Command: []string{"postgres"}}},
		}
	})
}

// TestAgentConfigPatch_RerenderingBrokerHoldsClearsTheMergeKeeps: the
// broker renders the persisted config after merging the inline config into
// it, and that merge keeps an empty agent_instructions or services value and
// appends skill references. So on a broker that re-renders, clearing
// agent_instructions, services or skills, or removing one skill, is held
// for reincarnation with a warning; a system prompt clear is applied only
// when the broker also applies cleared_fields; adding a skill is applied.
func TestAgentConfigPatch_RerenderingBrokerHoldsClearsTheMergeKeeps(t *testing.T) {
	for _, tc := range []struct {
		name    string
		caps    *store.BrokerCapabilities
		config  map[string]interface{}
		applied []string
		held    []string
	}{
		{"clear agent_instructions", &store.BrokerCapabilities{ChangedFields: true, ClearedFields: true},
			map[string]interface{}{"agent_instructions": ""}, []string{}, []string{"config.agent_instructions"}},
		{"clear services", &store.BrokerCapabilities{ChangedFields: true, ClearedFields: true},
			map[string]interface{}{"services": []interface{}{}}, []string{}, []string{"config.services"}},
		{"clear skills", &store.BrokerCapabilities{ChangedFields: true, ClearedFields: true},
			map[string]interface{}{"skills": []interface{}{}}, []string{}, []string{"config.skills"}},
		{"remove one skill", &store.BrokerCapabilities{ChangedFields: true, ClearedFields: true},
			map[string]interface{}{"skills": []map[string]interface{}{{"uri": "hub://skills/review"}}}, []string{}, []string{"config.skills"}},
		{"add a skill", &store.BrokerCapabilities{ChangedFields: true, ClearedFields: true},
			map[string]interface{}{"skills": []map[string]interface{}{{"uri": "hub://skills/review"}, {"uri": "hub://skills/test"}, {"uri": "hub://skills/lint"}}}, []string{"config.skills"}, []string{}},
		{"clear system_prompt with clearedFields", &store.BrokerCapabilities{ChangedFields: true, ClearedFields: true},
			map[string]interface{}{"system_prompt": ""}, []string{"config.system_prompt"}, []string{}},
		{"clear system_prompt without clearedFields", &store.BrokerCapabilities{ChangedFields: true},
			map[string]interface{}{"system_prompt": ""}, []string{}, []string{"config.system_prompt"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			disp := &inlineCaptureDispatcher{reincarnateTestDispatcher: newReincarnateTestDispatcher()}
			srv, s, project, broker := setupReincarnateTestServer(t, disp)
			setBrokerCaps(t, s, broker, tc.caps)
			agent := newT2EditAgent(t, s, project, broker)

			resp, code, body := patchAgentBody(t, srv, agent.ID, map[string]interface{}{"config": tc.config})
			require.Equal(t, http.StatusOK, code, body)
			assert.Equal(t, tc.applied, resp.Disposition.Applied)
			assert.Equal(t, tc.held, resp.Disposition.HeldForReincarnate)
			if len(tc.held) > 0 {
				require.Len(t, resp.Warnings, 1, "%v", resp.Warnings)
				assert.Contains(t, resp.Warnings[0], tc.held[0]+": stored now; rendered at the next reincarnation")
			} else {
				assert.Empty(t, resp.Warnings)
			}
		})
	}
}

// TestAgentConfigPatch_SystemPromptClearThenSet: a clear of the system
// prompt is recorded in cleared_fields and changed_fields; setting it again
// takes it off cleared_fields, and changed_fields keeps it (sticky).
func TestAgentConfigPatch_SystemPromptClearThenSet(t *testing.T) {
	disp := &inlineCaptureDispatcher{reincarnateTestDispatcher: newReincarnateTestDispatcher()}
	srv, s, project, broker := setupReincarnateTestServer(t, disp)
	setBrokerCaps(t, s, broker, &store.BrokerCapabilities{ChangedFields: true, ClearedFields: true})
	agent := newT2EditAgent(t, s, project, broker)

	inline := func() *api.ScionConfig {
		got, err := s.GetAgent(context.Background(), agent.ID)
		require.NoError(t, err)
		return got.AppliedConfig.InlineConfig
	}
	_, code, body := patchAgentBody(t, srv, agent.ID, map[string]interface{}{"config": map[string]interface{}{"system_prompt": ""}})
	require.Equal(t, http.StatusOK, code, body)
	assert.Equal(t, []string{"system_prompt"}, inline().ClearedFields)
	assert.Equal(t, []string{"system_prompt"}, inline().ChangedFields)

	resp, code, body := patchAgentBody(t, srv, agent.ID, map[string]interface{}{"config": map[string]interface{}{"system_prompt": "new prompt"}})
	require.Equal(t, http.StatusOK, code, body)
	assert.Equal(t, []string{"config.system_prompt"}, resp.Disposition.Applied)
	assert.Nil(t, inline().ClearedFields, "setting the prompt again takes it off cleared_fields")
	assert.Equal(t, []string{"system_prompt"}, inline().ChangedFields, "changed_fields is sticky")
	assert.Equal(t, "new prompt", inline().SystemPrompt)
}

// TestAgentConfigPatch_EnvClearByCallerWithoutEnvView: a caller who may
// update the agent but cannot view its env records no env.KEY clears, so on
// a broker that applies cleared_fields an env edit that drops keys is still
// held for reincarnation, not reported applied.
func TestAgentConfigPatch_EnvClearByCallerWithoutEnvView(t *testing.T) {
	disp := &inlineCaptureDispatcher{reincarnateTestDispatcher: newReincarnateTestDispatcher()}
	srv, s, project, broker := setupReincarnateTestServer(t, disp)
	setBrokerCaps(t, s, broker, &store.BrokerCapabilities{ChangedFields: true, ClearedFields: true})
	agent := newReincarnateTestAgent(t, s, project, broker, func(a *store.Agent) {
		a.Phase = string(state.PhaseStopped)
		a.AppliedConfig.InlineConfig = &api.ScionConfig{Harness: "claude", Env: map[string]string{"A": "1", "B": "2"}}
	})
	user := newReincarnateAuthzUser(t, s, "edit-no-env-view")
	grantPermissionViaRoleBinding(t, s, user.ID, "agent.read", store.RoleScopeProject, project.ID)
	grantPermissionViaRoleBinding(t, s, user.ID, "agent.update", store.RoleScopeProject, project.ID)
	identity := NewAuthenticatedUser(user.ID, user.Email, user.DisplayName, user.Role, "session")

	rec := doRequestAsIdentity(t, srv, identity, http.MethodPatch, "/api/v1/agents/"+agent.ID, map[string]interface{}{
		"config": map[string]interface{}{"env": map[string]string{}},
	})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var resp agentPatchResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.NotContains(t, resp.Disposition.Applied, "config.env")
	assert.Equal(t, []string{"config.env"}, resp.Disposition.HeldForReincarnate)
	assert.NotEmpty(t, resp.Warnings)

	stored, err := s.GetAgent(context.Background(), agent.ID)
	require.NoError(t, err)
	for _, p := range stored.AppliedConfig.InlineConfig.ClearedFields {
		assert.False(t, strings.HasPrefix(p, api.ClearedEnvPathPrefix), "no env clear recorded for a caller without env view: %s", p)
	}
}

// TestDispatchAgentStart_RerenderWarningInStartResponse drives the real
// start dispatch: an agent with changed provision-rendered keys started on a
// broker without changedFields gets the warning naming them, and the
// dispatched inline config carries changed_fields; on a broker with
// changedFields there is no warning.
func TestDispatchAgentStart_RerenderWarningInStartResponse(t *testing.T) {
	f := newTiersFixture(t, tiersKnownFalse)
	agent := &store.Agent{
		ID: tid("rerender-start"), Name: "rerender-start", Slug: "rerender-start", ProjectID: f.project.ID,
		OwnerID: "dev@localhost", RuntimeBrokerID: tid("tiers-broker"),
		AppliedConfig: &store.AgentAppliedConfig{
			InlineConfig: &api.ScionConfig{SystemPrompt: "new prompt", ChangedFields: []string{"system_prompt"}},
			CreateInputs: &store.AgentCreateInputs{},
		},
	}
	ctx, warns := withDispatchWarnings(context.Background())
	require.NoError(t, f.d.DispatchAgentStart(ctx, agent, "", false))
	require.NotNil(t, f.client.lastInlineConfig)
	assert.Equal(t, []string{"system_prompt"}, f.client.lastInlineConfig.ChangedFields)
	assert.Equal(t, 1, countContaining(warns.Warnings(), "system_prompt: changed since the agent was provisioned"), "%v", warns.Warnings())

	b, err := f.s.GetRuntimeBroker(context.Background(), tid("tiers-broker"))
	require.NoError(t, err)
	b.Capabilities = &store.BrokerCapabilities{ChangedFields: true}
	require.NoError(t, f.s.UpdateRuntimeBroker(context.Background(), b))
	ctx, warns = withDispatchWarnings(context.Background())
	require.NoError(t, f.d.DispatchAgentStart(ctx, agent, "", false))
	assert.Zero(t, countContaining(warns.Warnings(), "changed since the agent was provisioned"), "%v", warns.Warnings())
}

// countContaining counts the entries of list that contain sub.
func countContaining(list []string, sub string) int {
	n := 0
	for _, s := range list {
		if strings.Contains(s, sub) {
			n++
		}
	}
	return n
}

// TestAgentConfigPatch_ResourcesSubFieldDropIsHeld: a resources edit that
// drops a stored sub-field is held for reincarnation with the cleared
// warning on every broker, and nothing is recorded in cleared_fields (that
// would revert the whole resources value) (ptone/scion#5676).
func TestAgentConfigPatch_ResourcesSubFieldDropIsHeld(t *testing.T) {
	for _, caps := range []*store.BrokerCapabilities{
		{Reprovision: true},
		{Reprovision: true, ChangedFields: true, ClearedFields: true},
	} {
		t.Run(fmt.Sprintf("clearedFields=%v", caps.ClearedFields), func(t *testing.T) {
			disp := &inlineCaptureDispatcher{reincarnateTestDispatcher: newReincarnateTestDispatcher()}
			srv, s, project, broker := setupReincarnateTestServer(t, disp)
			setBrokerCaps(t, s, broker, caps)
			agent := newReincarnateTestAgent(t, s, project, broker, func(a *store.Agent) {
				a.Phase = string(state.PhaseStopped)
				a.AppliedConfig.InlineConfig = &api.ScionConfig{Harness: "claude", Resources: &api.ResourceSpec{
					Requests: api.ResourceList{CPU: "1", Memory: "1Gi"},
				}}
			})
			resp, code, body := patchAgentBody(t, srv, agent.ID, map[string]interface{}{
				"config": map[string]interface{}{"resources": map[string]interface{}{"requests": map[string]string{"cpu": "2"}}},
			})
			require.Equal(t, http.StatusOK, code, body)
			assert.Empty(t, resp.Disposition.Applied)
			assert.Equal(t, []string{"config.resources"}, resp.Disposition.HeldForReincarnate)
			require.Len(t, resp.Warnings, 1, "%v", resp.Warnings)
			assert.Contains(t, resp.Warnings[0], "config.resources: cleared now")
			stored, err := s.GetAgent(context.Background(), agent.ID)
			require.NoError(t, err)
			assert.NotContains(t, stored.AppliedConfig.InlineConfig.ClearedFields, "resources")
			assert.Equal(t, "2", stored.AppliedConfig.InlineConfig.Resources.Requests.CPU)
		})
	}
}

// TestAgentConfigPatch_ResourcesSubFieldChangeIsApplied: a resources edit
// that changes one sub-field and sends every other stored sub-field
// unchanged is applied at the next start; only a dropped stored sub-field
// is held (ptone/scion#5676).
func TestAgentConfigPatch_ResourcesSubFieldChangeIsApplied(t *testing.T) {
	disp := &inlineCaptureDispatcher{reincarnateTestDispatcher: newReincarnateTestDispatcher()}
	srv, s, project, broker := setupReincarnateTestServer(t, disp)
	agent := newReincarnateTestAgent(t, s, project, broker, func(a *store.Agent) {
		a.Phase = string(state.PhaseStopped)
		a.AppliedConfig.InlineConfig = &api.ScionConfig{Harness: "claude", Resources: &api.ResourceSpec{
			Requests: api.ResourceList{CPU: "1", Memory: "1Gi"},
		}}
	})
	resp, code, body := patchAgentBody(t, srv, agent.ID, map[string]interface{}{
		"config": map[string]interface{}{"resources": map[string]interface{}{"requests": map[string]string{"cpu": "2", "memory": "1Gi"}}},
	})
	require.Equal(t, http.StatusOK, code, body)
	assert.Equal(t, []string{"config.resources"}, resp.Disposition.Applied)
	assert.Empty(t, resp.Disposition.HeldForReincarnate)
	assert.Empty(t, resp.Warnings)
}

// TestBuildAppliedConfig_DropsChangedFields: a new agent starts with no
// changed_fields (or cleared_fields), whatever the create request carries.
func TestBuildAppliedConfig_DropsChangedFields(t *testing.T) {
	srv, _ := testServer(t)
	ac := srv.buildAppliedConfig(CreateAgentRequest{
		Config: &api.ScionConfig{
			SystemPrompt:  "p",
			ChangedFields: []string{"system_prompt"},
			ClearedFields: []string{"max_turns"},
		},
		Workspace: "/tmp/changed-fields-ws",
	}, "dev@localhost", AgentRoleBaseline)
	require.NotNil(t, ac)
	require.NotNil(t, ac.InlineConfig)
	assert.Nil(t, ac.InlineConfig.ChangedFields)
	assert.Nil(t, ac.InlineConfig.ClearedFields)
	assert.Equal(t, "p", ac.InlineConfig.SystemPrompt)
	require.NotNil(t, ac.CreateInputs)
	require.NotNil(t, ac.CreateInputs.InlineConfig)
	assert.Nil(t, ac.CreateInputs.InlineConfig.ChangedFields)
	assert.Nil(t, ac.CreateInputs.InlineConfig.ClearedFields)
}
