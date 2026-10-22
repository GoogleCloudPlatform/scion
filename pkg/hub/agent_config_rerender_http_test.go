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
	"net/http"
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
	b, err := s.GetRuntimeBroker(context.Background(), broker.ID)
	require.NoError(t, err)
	b.Capabilities = &store.BrokerCapabilities{Reprovision: true, ChangedFields: true}
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
	assert.Equal(t, 8, stored.AppliedConfig.InlineConfig.MaxTurns)
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
