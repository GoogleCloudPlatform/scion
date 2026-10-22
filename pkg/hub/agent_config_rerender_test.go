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

package hub

import (
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Tests for ptone/scion#3982: an edit of a provision-rendered key is
// recorded in the inline config's changed_fields, dispatched with every
// start, and reported as taking effect at the next start when the agent's
// broker re-renders it (the changedFields capability).

// wantRerenderedAtStart are the keys a broker with changedFields applies at
// the next start, written out independently of api.RerenderableConfigKeys.
var wantRerenderedAtStart = map[string]bool{
	"config.system_prompt":      true,
	"config.agent_instructions": true,
	"config.skills":             true,
	"config.services":           true,
}

// noContainerPhases are the phases with no live or transitional container.
var noContainerPhases = map[state.Phase]bool{
	state.PhaseCreated: true, state.PhaseStopped: true, state.PhaseError: true,
	state.PhaseHalted: true, state.PhaseSuspended: true,
}

func TestNextChangedFields(t *testing.T) {
	for _, tc := range []struct {
		name      string
		old       *api.ScionConfig
		provision []string
		want      []string
	}{
		{"nothing", nil, nil, nil},
		{"first change", nil, []string{"config.system_prompt"}, []string{"system_prompt"}},
		{"sticky union", &api.ScionConfig{ChangedFields: []string{"skills"}}, []string{"config.services", "config.system_prompt"}, []string{"services", "skills", "system_prompt"}},
		{"kept without a new change", &api.ScionConfig{ChangedFields: []string{"agent_instructions"}}, nil, []string{"agent_instructions"}},
		// secrets is provision-rendered but not re-rendered at start, and a
		// stale entry naming any other key is dropped.
		{"only rerenderable keys", &api.ScionConfig{ChangedFields: []string{"model", "env.FOO"}}, []string{"config.secrets", "config.model"}, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, nextChangedFields(tc.old, tc.provision))
		})
	}
}

func TestBrokerStartSupport_AppliesAtStart(t *testing.T) {
	with := brokerStartSupportFrom(&store.BrokerCapabilities{ChangedFields: true})
	without := brokerStartSupportFrom(&store.BrokerCapabilities{Reprovision: true})
	assert.Equal(t, brokerStartSupport{}, brokerStartSupportFrom(nil))
	for _, f := range agentEditFields {
		assert.Equal(t, wantRerenderedAtStart[f.Key], with.appliesAtStart(f.Key), "with changedFields: %s", f.Key)
		assert.False(t, without.appliesAtStart(f.Key), "without changedFields: %s", f.Key)
	}
	assert.Equal(t, []string{"config.secrets"}, with.heldProvisionEdits([]string{"config.secrets", "config.skills", "config.system_prompt"}))
	assert.Equal(t, []string{"config.secrets", "config.skills"}, without.heldProvisionEdits([]string{"config.secrets", "config.skills"}))
}

// TestAgentEditability_EveryKeyEveryPhaseByBroker enumerates every key,
// every phase and both broker kinds: with changedFields, the four
// re-rendered keys are "now" in every phase with no container, and every
// other cell is what the same agent reports on an older broker.
func TestAgentEditability_EveryKeyEveryPhaseByBroker(t *testing.T) {
	full := agentEditAccess{CanUpdate: true, CanChangeRole: true}
	rerendering := full
	rerendering.Broker = brokerStartSupport{ChangedFields: true}
	for _, phase := range state.Phases() {
		agent := &store.Agent{Phase: string(phase)}
		older := buildAgentEditability(agent, full)
		newer := buildAgentEditability(agent, rerendering)
		require.Len(t, newer.Fields, len(agentEditFields))
		for _, f := range agentEditFields {
			want := older.Fields[f.Key]
			if wantRerenderedAtStart[f.Key] && noContainerPhases[phase] {
				assert.Equal(t, EditReincarnate, want.Disposition, "older broker: %s in %s waits for a reincarnation", f.Key, phase)
				want.Disposition = EditNow
			}
			assert.Equal(t, want, newer.Fields[f.Key], "%s in %s", f.Key, phase)
		}
	}
	// A caller without update gets nothing, whatever the broker.
	noUpdate := buildAgentEditability(&store.Agent{Phase: string(state.PhaseStopped)}, agentEditAccess{Broker: brokerStartSupport{ChangedFields: true}})
	assert.Equal(t, EditLocked, noUpdate.Fields["config.system_prompt"].Disposition)
}

// TestRerenderUnsupportedWarning: a start whose inline config lists keys to
// re-render warns, naming them, only when the broker lacks changedFields.
func TestRerenderUnsupportedWarning(t *testing.T) {
	inline := &api.ScionConfig{ChangedFields: []string{"system_prompt", "skills"}, ClearedFields: []string{"services", "max_turns"}}
	msg := rerenderUnsupportedWarning(inline, &store.BrokerCapabilities{Reprovision: true})
	assert.Equal(t, "services, skills, system_prompt: changed since the agent was provisioned; this agent's runtime broker does not re-render them at start, so the change takes effect at the next reincarnation", msg)
	assert.NotEmpty(t, rerenderUnsupportedWarning(inline, nil), "a broker with no reported capabilities is an older broker")
	assert.Empty(t, rerenderUnsupportedWarning(inline, &store.BrokerCapabilities{ChangedFields: true}))
	assert.Empty(t, rerenderUnsupportedWarning(&api.ScionConfig{ClearedFields: []string{"max_turns"}}, nil))
	assert.Empty(t, rerenderUnsupportedWarning(nil, nil))
}

// TestCapabilitiesFromStrings_ChangedFields: the registration name sets
// the stored capability.
func TestCapabilitiesFromStrings_ChangedFields(t *testing.T) {
	for _, name := range []string{"changedFields", "changed_fields", "CHANGEDFIELDS"} {
		assert.True(t, capabilitiesFromStrings([]string{name}).ChangedFields, name)
	}
	assert.False(t, capabilitiesFromStrings([]string{"reprovision"}).ChangedFields)
}
