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
	"encoding/json"
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
	all := []string{"config.agent_instructions", "config.secrets", "config.services", "config.skills", "config.system_prompt"}
	set := &api.ScionConfig{
		AgentInstructions: "i", SystemPrompt: "p",
		Skills:   []api.SkillReference{{URI: "hub://skills/a"}},
		Services: []api.ServiceSpec{{Name: "db", Command: []string{"postgres"}}},
		Secrets:  []api.RequiredSecret{{Key: "K"}},
	}
	assert.Equal(t, []string{"config.secrets"}, with.heldProvisionEdits(all, nil, set, nil), "set values reach the persisted config")
	assert.Equal(t, all, without.heldProvisionEdits(all, nil, set, nil))

	// Clears the start merge keeps are held: the broker would render the
	// previous value. A system prompt clear is applied only when the broker
	// also applies cleared_fields and the clear was recorded.
	both := brokerStartSupportFrom(&store.BrokerCapabilities{ChangedFields: true, ClearedFields: true})
	cleared := &api.ScionConfig{}
	clears := []string{"config.agent_instructions", "config.services", "config.skills", "config.system_prompt"}
	assert.Equal(t, clears, with.heldProvisionEdits(clears, set, cleared, []string{"system_prompt"}))
	assert.Equal(t, []string{"config.agent_instructions", "config.services", "config.skills"}, both.heldProvisionEdits(clears, set, cleared, []string{"system_prompt"}))
	assert.Equal(t, clears, both.heldProvisionEdits(clears, set, cleared, nil), "an unrecorded clear is not applied")

	// A skill removal is held: the merge appends references and a re-render
	// uninstalls nothing. Adding a skill is applied.
	two := &api.ScionConfig{Skills: []api.SkillReference{{URI: "hub://skills/a"}, {URI: "hub://skills/b", As: "bee"}}}
	one := &api.ScionConfig{Skills: []api.SkillReference{{URI: "hub://skills/a"}}}
	renamed := &api.ScionConfig{Skills: []api.SkillReference{{URI: "hub://skills/a"}, {URI: "hub://skills/b"}}}
	assert.Equal(t, []string{"config.skills"}, with.heldProvisionEdits([]string{"config.skills"}, two, one, nil))
	assert.Equal(t, []string{"config.skills"}, with.heldProvisionEdits([]string{"config.skills"}, two, renamed, nil), "a changed install name removes the old one")
	pinned := &api.ScionConfig{Skills: []api.SkillReference{{URI: "hub://skills/a@v1"}}}
	repinned := &api.ScionConfig{Skills: []api.SkillReference{{URI: "hub://skills/a@v2"}}}
	assert.Empty(t, with.heldProvisionEdits([]string{"config.skills"}, pinned, repinned, nil), "a version change replaces the skill at the same destination")
	assert.Empty(t, with.heldProvisionEdits([]string{"config.skills"}, one, two, nil))
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
				assert.False(t, want.ClearNeedsReincarnate, "older broker: the whole edit waits, so no separate clear note")
				want.Disposition = EditNow
				// A clear of a re-rendered key is kept by the start merge
				// (the system prompt's needs clearedFields, absent here).
				want.ClearNeedsReincarnate = true
				// A resume continues the conversation.
				if phase == state.PhaseSuspended {
					want.Note = "session"
				}
			}
			assert.Equal(t, want, newer.Fields[f.Key], "%s in %s", f.Key, phase)
		}
	}
	// With clearedFields too, a system prompt clear is applied at start;
	// the other re-rendered keys' clears are not.
	bothAccess := full
	bothAccess.Broker = brokerStartSupport{ChangedFields: true, ClearedFields: true}
	both := buildAgentEditability(&store.Agent{Phase: string(state.PhaseStopped)}, bothAccess)
	assert.False(t, both.Fields["config.system_prompt"].ClearNeedsReincarnate)
	for _, k := range []string{"config.agent_instructions", "config.skills", "config.services"} {
		assert.True(t, both.Fields[k].ClearNeedsReincarnate, k)
		assert.Equal(t, EditNow, both.Fields[k].Disposition, k)
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

// wantClearAppliedAtStart are the container keys whose clear a broker with
// clearedFields applies at the next start (ptone/scion#4890): the keys
// api.ClearableConfigPath accepts, and env (removed keys travel as
// env.KEY entries). A count limit's clear is its null; its 0 (Unlimited) is
// a value, not a clear (ptone/scion#5605).
var wantClearAppliedAtStart = map[string]bool{
	"config.max_turns":       true,
	"config.max_model_calls": true,
	"config.max_duration":    true,
	"config.thinking_level":  true,
	"config.resources":       true,
	"config.system_prompt":   true,
	"config.env":             true,
}

func TestBrokerStartSupport_ClearAppliesAtStart(t *testing.T) {
	with := brokerStartSupportFrom(&store.BrokerCapabilities{ClearedFields: true})
	without := brokerStartSupportFrom(&store.BrokerCapabilities{ChangedFields: true})
	for _, f := range agentEditFields {
		assert.Equal(t, wantClearAppliedAtStart[f.Key], with.clearAppliesAtStart(f.Key), "with clearedFields: %s", f.Key)
		assert.False(t, without.clearAppliesAtStart(f.Key), "without clearedFields: %s", f.Key)
	}
	cleared := []string{"config.max_turns", "config.model", "config.resources"}
	recorded := []string{"max_turns", "resources"}
	assert.Equal(t, []string{"config.model"}, with.heldClearEdits(cleared, recorded, nil, nil), "a null count limit is a clear the broker applies")
	assert.Equal(t, cleared, without.heldClearEdits(cleared, recorded, nil, nil))
	assert.Equal(t, cleared, with.heldClearEdits(cleared, nil, nil, nil), "a clear that was not recorded is not applied")
	// A recorded null clear of either count limit is applied by a broker
	// with clearedFields. Their 0 (Unlimited) never reaches heldClearEdits:
	// the start merge applies it, so it is not reported as cleared
	// (ptone/scion#5605).
	for _, key := range []string{"max_turns", "max_model_calls"} {
		assert.Empty(t, with.heldClearEdits([]string{"config." + key}, []string{key}, nil, nil), key)
		assert.Equal(t, []string{"config." + key}, without.heldClearEdits([]string{"config." + key}, []string{key}, nil, nil), key)
	}
	// env is applied only when every dropped key was recorded: a caller who
	// cannot view env records none, and auto-expose and TZ keys are never
	// recorded.
	env := []string{"config.env"}
	assert.Empty(t, with.heldClearEdits(env, []string{"env.A", "env.B"}, []string{"A", "B"}, nil))
	assert.Equal(t, env, with.heldClearEdits(env, []string{"env.A"}, []string{"A", "B"}, nil))
	assert.Equal(t, env, with.heldClearEdits(env, nil, []string{"A"}, nil))
	assert.Equal(t, env, with.heldClearEdits(env, nil, []string{"SCION_AUTO_EXPOSE_PORTS"}, nil))
	assert.Equal(t, env, without.heldClearEdits(env, []string{"env.A"}, []string{"A"}, nil))

	// A resources edit that drops sub-fields is applied only by a broker
	// with clearedFields and clearedResourceSubfields, and only when every
	// dropped sub-path was recorded (ptone/scion#5676).
	res := []string{"config.resources"}
	dropped := []string{"resources.disk", "resources.limits.memory"}
	subfields := brokerStartSupportFrom(&store.BrokerCapabilities{ClearedFields: true, ClearedResourceSubfields: true})
	subfieldsOnly := brokerStartSupportFrom(&store.BrokerCapabilities{ClearedResourceSubfields: true})
	assert.Empty(t, subfields.heldClearEdits(res, dropped, nil, dropped))
	assert.Equal(t, res, subfields.heldClearEdits(res, dropped[:1], nil, dropped), "a dropped sub-path that was not recorded")
	assert.Equal(t, res, with.heldClearEdits(res, dropped, nil, dropped), "clearedFields alone does not apply sub-paths")
	assert.Equal(t, res, without.heldClearEdits(res, dropped, nil, dropped))
	assert.Equal(t, res, subfieldsOnly.heldClearEdits(res, dropped, nil, dropped), "sub-paths need clearedFields too")
	assert.Empty(t, with.heldClearEdits(res, []string{"resources"}, nil, nil), "a recorded whole clear follows the general rule")

	removed := map[string][]string{"env": {"A"}, "volumes": {"/data"}}
	assert.Equal(t, map[string][]string{"volumes": {"/data"}}, with.heldRemovals(removed, []string{"env.A"}))
	assert.Equal(t, removed, with.heldRemovals(removed, nil), "unrecorded env removals stay held")
	assert.Equal(t, removed, without.heldRemovals(removed, []string{"env.A"}))
}

func TestDroppedEnvKeys(t *testing.T) {
	old := &api.ScionConfig{Env: map[string]string{"A": "1", "B": "2", "C": "3"}}
	present := map[string]bool{"env": true}
	assert.Equal(t, []string{"A", "C"}, droppedEnvKeys(old, &api.ScionConfig{Env: map[string]string{"B": "2", "D": "4"}}, present))
	assert.Equal(t, []string{"A", "B", "C"}, droppedEnvKeys(old, &api.ScionConfig{}, present))
	assert.Nil(t, droppedEnvKeys(old, &api.ScionConfig{}, map[string]bool{"model": true}), "env not in the request drops nothing")
	assert.Nil(t, droppedEnvKeys(nil, &api.ScionConfig{}, present))
	withDerived := &api.ScionConfig{Env: map[string]string{"A": "1", "SCION_AUTO_EXPOSE_PORTS": "true", "TZ": "UTC"}}
	assert.Equal(t, []string{"A"}, droppedEnvKeys(withDerived, &api.ScionConfig{}, present), "auto-expose keys and TZ are derived per start, not dropped")
}

func TestBrokerStartSupport_RerenderWarnings(t *testing.T) {
	with := brokerStartSupport{ChangedFields: true}
	got := with.reincarnateOnlyEditWarnings([]string{"config.services"}, []string{"config.model"})
	require.Len(t, got, 2)
	assert.Equal(t, "config.services: stored now; rendered at the next reincarnation (a start does not render secrets, and does not apply a clear the broker cannot apply or a removed skill)", got[0])
	assert.Contains(t, got[1], "config.model: cleared now")
	assert.Equal(t, reincarnateOnlyEditWarnings([]string{"config.services"}, nil), brokerStartSupport{}.reincarnateOnlyEditWarnings([]string{"config.services"}, nil))
	assert.Equal(t, reincarnateOnlyEditWarnings(nil, []string{"config.model"}), with.reincarnateOnlyEditWarnings(nil, []string{"config.model"}))
}

// TestAgentEditability_ClearNeedsReincarnateByBroker: with clearedFields
// a clear of a clearable container key needs no reincarnation; every other
// container key still flags it, and an older broker flags them all.
func TestAgentEditability_ClearNeedsReincarnateByBroker(t *testing.T) {
	full := agentEditAccess{CanUpdate: true, CanChangeRole: true}
	clearing := full
	clearing.Broker = brokerStartSupport{ClearedFields: true}
	for _, phase := range state.Phases() {
		agent := &store.Agent{Phase: string(phase)}
		older := buildAgentEditability(agent, full)
		newer := buildAgentEditability(agent, clearing)
		for _, f := range agentEditFields {
			want := older.Fields[f.Key]
			if wantClearAppliedAtStart[f.Key] && f.Tier == EditTierContainer {
				assert.True(t, want.ClearNeedsReincarnate, "older broker: clearing %s needs a reincarnation", f.Key)
				want.ClearNeedsReincarnate = false
			}
			assert.Equal(t, want, newer.Fields[f.Key], "%s in %s", f.Key, phase)
		}
	}
}

// TestAgentEditability_SubfieldClearUnsupported: config.resources carries
// subfieldClearUnsupported unless the agent's broker applies resources
// sub-field clears (clearedFields and clearedResourceSubfields); no other
// field ever carries it (ptone/scion#5676).
func TestAgentEditability_SubfieldClearUnsupported(t *testing.T) {
	for _, tc := range []struct {
		name string
		sup  brokerStartSupport
		want bool
	}{
		{"no broker", brokerStartSupport{}, true},
		{"clearedFields only", brokerStartSupport{ClearedFields: true}, true},
		{"clearedResourceSubfields only", brokerStartSupport{ClearedResourceSubfields: true}, true},
		{"capable", brokerStartSupport{ClearedFields: true, ClearedResourceSubfields: true}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			access := agentEditAccess{CanUpdate: true, CanChangeRole: true, Broker: tc.sup}
			for _, phase := range state.Phases() {
				ed := buildAgentEditability(&store.Agent{Phase: string(phase)}, access)
				for _, f := range agentEditFields {
					want := tc.want && f.Key == "config.resources"
					assert.Equal(t, want, ed.Fields[f.Key].SubfieldClearUnsupported, "%s in %s", f.Key, phase)
				}
			}
		})
	}
	raw, err := json.Marshal(FieldEditState{SubfieldClearUnsupported: true})
	require.NoError(t, err)
	assert.Contains(t, string(raw), `"subfieldClearUnsupported":true`)
	raw, err = json.Marshal(FieldEditState{})
	require.NoError(t, err)
	assert.NotContains(t, string(raw), "subfieldClearUnsupported")
}

// TestCapabilitiesFromStrings_ClearedResourceSubfields: the registration
// name sets the stored capability, and brokerStartSupportFrom carries it.
func TestCapabilitiesFromStrings_ClearedResourceSubfields(t *testing.T) {
	for _, name := range []string{"clearedResourceSubfields", "cleared_resource_subfields"} {
		caps := capabilitiesFromStrings([]string{name})
		assert.True(t, caps.ClearedResourceSubfields, name)
		assert.True(t, brokerStartSupportFrom(caps).ClearedResourceSubfields, name)
	}
	assert.False(t, capabilitiesFromStrings([]string{"clearedFields"}).ClearedResourceSubfields)
	assert.False(t, brokerStartSupportFrom(nil).ClearedResourceSubfields)
}

// TestCapabilitiesFromStrings_ClearedFields: the registration name sets
// the stored capability.
func TestCapabilitiesFromStrings_ClearedFields(t *testing.T) {
	for _, name := range []string{"clearedFields", "cleared_fields"} {
		assert.True(t, capabilitiesFromStrings([]string{name}).ClearedFields, name)
	}
	assert.False(t, capabilitiesFromStrings([]string{"changedFields"}).ClearedFields)
}

// TestWithDroppedResourcesSubField: a resources edit that drops a stored
// sub-field is counted as a clear of config.resources for reporting
// (ptone/scion#5676), and droppedResourceSubPaths names the dropped
// sub-paths; one that keeps or replaces every stored sub-field, or an
// empty whole value (already a clear), is not added again.
func TestWithDroppedResourcesSubField(t *testing.T) {
	old := &api.ScionConfig{Resources: &api.ResourceSpec{
		Requests: api.ResourceList{CPU: "1", Memory: "1Gi"},
		Limits:   api.ResourceList{Memory: "2Gi"},
		Disk:     "10Gi",
	}}
	present := map[string]bool{"resources": true}
	for _, tc := range []struct {
		name      string
		req       *api.ResourceSpec
		want      []string
		wantPaths []string
	}{
		{"drops requests.memory", &api.ResourceSpec{Requests: api.ResourceList{CPU: "2"}, Limits: api.ResourceList{Memory: "2Gi"}, Disk: "10Gi"}, []string{"config.max_turns", "config.resources"}, []string{"resources.requests.memory"}},
		{"drops limits.memory", &api.ResourceSpec{Requests: api.ResourceList{CPU: "1", Memory: "1Gi"}, Disk: "10Gi"}, []string{"config.max_turns", "config.resources"}, []string{"resources.limits.memory"}},
		{"drops disk", &api.ResourceSpec{Requests: api.ResourceList{CPU: "1", Memory: "1Gi"}, Limits: api.ResourceList{Memory: "2Gi"}}, []string{"config.max_turns", "config.resources"}, []string{"resources.disk"}},
		{"drops all but one", &api.ResourceSpec{Limits: api.ResourceList{CPU: "4"}}, []string{"config.max_turns", "config.resources"}, []string{"resources.disk", "resources.limits.memory", "resources.requests.cpu", "resources.requests.memory"}},
		{"changes cpu, keeps every other stored sub-field", &api.ResourceSpec{Requests: api.ResourceList{CPU: "2", Memory: "1Gi"}, Limits: api.ResourceList{Memory: "2Gi"}, Disk: "10Gi"}, []string{"config.max_turns"}, nil},
		{"replaces every stored sub-field", &api.ResourceSpec{Requests: api.ResourceList{CPU: "2", Memory: "2Gi"}, Limits: api.ResourceList{Memory: "4Gi", CPU: "1"}, Disk: "20Gi"}, []string{"config.max_turns"}, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := withDroppedResourcesSubField([]string{"config.max_turns"}, old, &api.ScionConfig{Resources: tc.req}, present)
			assert.Equal(t, tc.want, got)
			assert.Equal(t, tc.wantPaths, droppedResourceSubPaths(old, &api.ScionConfig{Resources: tc.req}, present))
		})
	}
	already := []string{"config.resources"}
	assert.Equal(t, already, withDroppedResourcesSubField(already, old, &api.ScionConfig{Resources: &api.ResourceSpec{}}, present), "a whole clear is already counted")
	assert.Nil(t, droppedResourceSubPaths(old, &api.ScionConfig{Resources: &api.ResourceSpec{}}, present), "a whole clear drops no sub-path")
	assert.Nil(t, withDroppedResourcesSubField(nil, old, &api.ScionConfig{Resources: &api.ResourceSpec{Requests: api.ResourceList{CPU: "2"}}}, map[string]bool{"model": true}), "resources not in the request")
	assert.Nil(t, withDroppedResourcesSubField(nil, &api.ScionConfig{}, &api.ScionConfig{Resources: &api.ResourceSpec{Requests: api.ResourceList{CPU: "2"}}}, present), "nothing stored")
}
