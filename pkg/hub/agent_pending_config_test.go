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
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Unit tests of the held-edit building blocks (ptone/scion#3976) that need
// no store.

func decodeConfigPatch(t *testing.T, body string) configPatch {
	t.Helper()
	var cfg api.ScionConfig
	require.NoError(t, json.Unmarshal([]byte(body), &cfg))
	var raw map[string]json.RawMessage
	require.NoError(t, json.Unmarshal([]byte(body), &raw))
	present := map[string]bool{}
	for k := range raw {
		present[strings.ToLower(k)] = true
	}
	return configPatch{cfg: &cfg, raw: raw, present: present}
}

func TestConfigPatchSubset(t *testing.T) {
	p := decodeConfigPatch(t, `{"model":"m","System_Prompt":"be brief","env":{},"thinking_level":null,"not_a_key":1}`)
	t1 := p.subset(isContainerConfigField)
	assert.Equal(t, map[string]bool{"model": true, "env": true, "thinking_level": true}, t1.present)
	assert.Equal(t, "m", t1.cfg.Model)
	assert.Empty(t, t1.cfg.SystemPrompt, "a key outside the subset is zeroed")
	require.NotNil(t, t1.cfg.Env, "an explicit empty env stays an empty map, not absent")
	assert.Empty(t, t1.cfg.Env)
	assert.NotContains(t, t1.raw, "not_a_key", "keys the table does not know are dropped")

	t2 := p.subset(func(f agentEditField) bool { return f.Tier == EditTierProvision })
	assert.Equal(t, map[string]bool{"system_prompt": true}, t2.present)
	assert.Equal(t, "be brief", t2.cfg.SystemPrompt)
	assert.Empty(t, t2.cfg.Model)
	assert.Nil(t, t2.cfg.Env)
	assert.Contains(t, t2.raw, "System_Prompt", "the raw key keeps the request's spelling")

	assert.Equal(t, "m", p.cfg.Model, "the original is left as it was")
	assert.Equal(t, "be brief", p.cfg.SystemPrompt)
	assert.True(t, configPatch{}.empty())
	assert.True(t, p.subset(func(agentEditField) bool { return false }).empty())
}

func TestCanonicalConfigRaw(t *testing.T) {
	p := decodeConfigPatch(t, `{"Model":"m","env":{"A":"1","TZ":"UTC"},"max_turns":null,"thinking_level":null}`)
	stripConfigEnvTZ(p.cfg)
	raw := canonicalConfigRaw(p)
	assert.JSONEq(t, `"m"`, string(raw["model"]), "keys are stored by their json key")
	assert.JSONEq(t, `{"A":"1"}`, string(raw["env"]), "the stripped TZ is not stored")
	assert.Equal(t, "null", string(raw["max_turns"]), "a null count is stored as null (a clear), apart from 0 (Unlimited, ptone/scion#5605)")
	assert.Equal(t, "null", string(raw["thinking_level"]), "a null pointer key is kept as null")
	assert.NotContains(t, raw, "Model")

	nullEnv := decodeConfigPatch(t, `{"env":null}`)
	assert.Equal(t, "null", string(canonicalConfigRaw(nullEnv)["env"]))
}

// TestCanonicalConfigRaw_NestedKeysAreCanonical: a nested key spelled in
// another case (which encoding/json accepts) is stored under its json key,
// so the response redaction of telemetry cloud headers applies to it.
func TestCanonicalConfigRaw_NestedKeysAreCanonical(t *testing.T) {
	p := decodeConfigPatch(t, `{"Telemetry":{"Cloud":{"Endpoint":"https://otel","Headers":{"Authorization":"Bearer secret"}}}}`)
	raw := canonicalConfigRaw(p)
	require.Contains(t, raw, "telemetry")
	var tel map[string]map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(raw["telemetry"], &tel))
	require.Contains(t, tel, "cloud")
	assert.Contains(t, tel["cloud"], "headers")
	assert.NotContains(t, string(raw["telemetry"]), "Cloud")
	assert.NotContains(t, string(raw["telemetry"]), "Headers")

	pc := &store.AgentPendingConfig{Config: raw}
	b, err := json.Marshal(pc.ResponseView(false))
	require.NoError(t, err)
	assert.NotContains(t, string(b), "Bearer secret", "a viewer without attach never sees the headers")
	b, err = json.Marshal(pc.ResponseView(true))
	require.NoError(t, err)
	assert.Contains(t, string(b), "Bearer secret")
}

func TestPendingConfigPatch_DecodesStoredKeys(t *testing.T) {
	pc := &store.AgentPendingConfig{Config: map[string]json.RawMessage{
		"model":          json.RawMessage(`"m"`),
		"thinking_level": json.RawMessage(`null`),
		"max_turns":      json.RawMessage(`"not a number"`),
	}}
	p := pendingConfigPatch(pc)
	assert.Equal(t, "m", p.cfg.Model)
	assert.Nil(t, p.cfg.ThinkingLevel)
	assert.True(t, p.present["thinking_level"], "a stored null is a present key (clear at apply)")
	assert.False(t, p.present["max_turns"], "a value that no longer decodes is dropped alone")
	assert.NotContains(t, p.raw, "max_turns")
	assert.True(t, pendingConfigPatch(nil).empty())
}

func TestEffectiveNextRunInline(t *testing.T) {
	agent := &store.Agent{
		AppliedConfig: &store.AgentAppliedConfig{InlineConfig: &api.ScionConfig{Model: "a", MaxTurns: api.IntPtr(3), SystemPrompt: "old"}},
		PendingConfig: &store.AgentPendingConfig{Config: map[string]json.RawMessage{
			"model":         json.RawMessage(`"b"`),
			"system_prompt": json.RawMessage(`null`),
		}},
	}
	got := effectiveNextRunInline(agent)
	assert.Equal(t, "b", got.Model)
	assert.Equal(t, api.IntPtr(3), got.MaxTurns)
	assert.Empty(t, got.SystemPrompt, "a pending null clears the key")
	assert.Equal(t, "a", agent.AppliedConfig.InlineConfig.Model, "the applied config is not changed")

	agent.PendingConfig = nil
	assert.Equal(t, "a", effectiveNextRunInline(agent).Model)
}

func TestHeldForReincarnateWarning(t *testing.T) {
	assert.Empty(t, heldForReincarnateWarning(nil))
	assert.Equal(t, "Held for reincarnation: agent instructions, system prompt. A reincarnation does not apply pending edits yet; they stay pending.",
		heldForReincarnateWarning([]string{"config.agent_instructions", "config.system_prompt"}))
}

func TestStartFoldTrigger(t *testing.T) {
	assert.Equal(t, pendingFoldTriggerStart, startFoldTrigger(StartOpts{Kind: store.StartClaimUser}))
	assert.Equal(t, pendingFoldTriggerResume, startFoldTrigger(StartOpts{Kind: store.StartClaimUser, Resume: true}))
	assert.Equal(t, pendingFoldTriggerRestart, startFoldTrigger(StartOpts{Kind: store.StartClaimRestart}))
	assert.Equal(t, pendingFoldTriggerWake, startFoldTrigger(StartOpts{Kind: store.StartClaimWake, Resume: true}))
}

func TestPendingConfigKeys(t *testing.T) {
	assert.Nil(t, pendingConfigKeys(nil))
	assert.Equal(t, []string{"config.model", "config.skills"}, pendingConfigKeys(&store.AgentPendingConfig{
		Config: map[string]json.RawMessage{"skills": json.RawMessage(`[]`), "model": json.RawMessage(`"m"`)},
	}))
}

func TestCloneAppliedConfigCopiesEnvDeeply(t *testing.T) {
	ac := &store.AgentAppliedConfig{
		Env:          map[string]string{"A": "1"},
		InlineConfig: &api.ScionConfig{Model: "m"},
		CreateInputs: &store.AgentCreateInputs{},
	}
	cp := cloneAppliedConfig(ac)
	require.NotNil(t, cp)
	assert.Equal(t, map[string]string{"A": "1"}, cp.Env, "Env is copied, not redacted")
	cp.Env["A"] = "2"
	cp.InlineConfig.Model = "x"
	assert.Equal(t, "1", ac.Env["A"])
	assert.Equal(t, "m", ac.InlineConfig.Model)
	assert.Nil(t, cloneAppliedConfig(nil))
}

// TestAgentEditability_HeldEdits: with held edits on, a running or
// transitional agent's container keys are held and, with a broker that
// does not re-render at start (the zero broker here), its provision-rendered
// keys reincarnate, instead of locked; phases with no container are
// unchanged.
func TestAgentEditability_HeldEdits(t *testing.T) {
	on := agentEditAccess{CanUpdate: true, CanChangeRole: true, HeldEdits: true}
	off := agentEditAccess{CanUpdate: true, CanChangeRole: true}
	for _, phase := range []state.Phase{state.PhaseProvisioning, state.PhaseCloning, state.PhaseStarting, state.PhaseRunning, state.PhaseStopping} {
		agent := &store.Agent{Phase: string(phase)}
		ed := buildAgentEditability(agent, on)
		assert.Equal(t, EditHeld, ed.Fields["config.model"].Disposition, phase)
		assert.Empty(t, ed.Fields["config.model"].Reason, phase)
		assert.Equal(t, EditReincarnate, ed.Fields["config.system_prompt"].Disposition, phase)
		assert.Equal(t, EditLocked, ed.Fields["config.branch"].Disposition, phase)
		assert.Equal(t, EditNow, ed.Fields["explicitTimezone"].Disposition, phase)

		edOff := buildAgentEditability(agent, off)
		assert.Equal(t, EditLocked, edOff.Fields["config.model"].Disposition, phase)
		assert.Equal(t, editReasonRunning, edOff.Fields["config.model"].Reason, phase)
	}
	for _, phase := range []state.Phase{state.PhaseCreated, state.PhaseStopped, state.PhaseError, state.PhaseSuspended} {
		agent := &store.Agent{Phase: string(phase)}
		assert.Equal(t, buildAgentEditability(agent, off), buildAgentEditability(agent, on), phase)
	}
}

// TestLockedPatchKeysFor_HeldEdits: with held edits on, config keys of a
// running agent are not refused; a fixed key that would change is still
// refused (400), and an unchanged echo is not.
func TestLockedPatchKeysFor_HeldEdits(t *testing.T) {
	running := &store.Agent{Phase: string(state.PhaseRunning), AppliedConfig: &store.AgentAppliedConfig{
		InlineConfig: &api.ScionConfig{Branch: "main"},
	}}
	ok := decodeConfigPatch(t, `{"model":"m","system_prompt":"p"}`)
	assert.Nil(t, lockedPatchKeysFor(running, true, nil, ok.raw, ok.cfg, nil))
	ref := lockedPatchKeysFor(running, false, nil, ok.raw, ok.cfg, nil)
	require.NotNil(t, ref)
	assert.True(t, ref.Conflict)

	fixed := decodeConfigPatch(t, `{"branch":"other"}`)
	ref = lockedPatchKeysFor(running, true, nil, fixed.raw, fixed.cfg, nil)
	require.NotNil(t, ref)
	assert.False(t, ref.Conflict, "a changed fixed key is a 400, not a phase conflict")
	assert.Contains(t, ref.Fields, "config.branch")

	echo := decodeConfigPatch(t, `{"branch":"main"}`)
	assert.Nil(t, lockedPatchKeysFor(running, true, nil, echo.raw, echo.cfg, nil))
}

// TestFoldPendingConfig_RemovesUnappliableKeepsProvisionKeys: folding only
// container keys, with none pending, applies nothing, removes the stored
// keys that can never be applied, and keeps a valid provision-rendered key.
func TestFoldPendingConfig_RemovesUnappliableKeepsProvisionKeys(t *testing.T) {
	agent := &store.Agent{
		AppliedConfig: &store.AgentAppliedConfig{InlineConfig: &api.ScionConfig{Model: "m"}},
		PendingConfig: &store.AgentPendingConfig{Config: map[string]json.RawMessage{
			"system_prompt": json.RawMessage(`"be brief"`),
			"max_turns":     json.RawMessage(`"not a number"`),
			"no_such_key":   json.RawMessage(`1`),
		}},
	}
	folded, unappliable := (&Server{}).foldPendingConfig(context.Background(), agent, pendingFoldPlan{keep: isContainerConfigField})
	assert.Empty(t, folded)
	assert.Equal(t, []string{"config.max_turns", "config.no_such_key"}, unappliable)
	require.NotNil(t, agent.PendingConfig)
	assert.Equal(t, []string{"config.system_prompt"}, pendingConfigKeys(agent.PendingConfig))
	assert.Equal(t, "m", agent.AppliedConfig.InlineConfig.Model, "nothing was applied")
}
