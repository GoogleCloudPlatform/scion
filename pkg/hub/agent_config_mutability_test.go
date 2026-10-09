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
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// wantEditTier is the expected tier of every key in the mutability table.
// It is written out key by key, independently of agentEditFields, so a
// change of tier fails here and has to be made deliberately.
var wantEditTier = map[string]EditTier{
	"name":        EditTierMetadata,
	"labels":      EditTierMetadata,
	"annotations": EditTierMetadata,
	"taskSummary": EditTierMetadata,
	"messageMode": EditTierMetadata,

	"config.model":             EditTierContainer,
	"config.thinking_level":    EditTierContainer,
	"config.auth_selectedType": EditTierContainer,
	"config.image":             EditTierContainer,
	"config.user":              EditTierContainer,
	"config.max_turns":         EditTierContainer,
	"config.max_model_calls":   EditTierContainer,
	"config.max_duration":      EditTierContainer,
	"config.resources":         EditTierContainer,
	"config.env":               EditTierContainer,
	"config.telemetry":         EditTierContainer,
	"config.mcp_servers":       EditTierContainer,
	"config.volumes":           EditTierContainer,
	"config.kubernetes":        EditTierContainer,
	"config.command_args":      EditTierContainer,
	"config.task_flag":         EditTierContainer,
	"config.task":              EditTierContainer,
	"explicitTimezone":         EditTierContainer,

	"config.system_prompt":      EditTierProvision,
	"config.agent_instructions": EditTierProvision,
	"config.skills":             EditTierProvision,
	"config.services":           EditTierProvision,
	"config.secrets":            EditTierProvision,

	"agentRole":    EditTierPrincipal,
	"gcp_identity": EditTierPrincipal,

	"projectId":                        EditTierImmutable,
	"template":                         EditTierImmutable,
	"harnessConfig":                    EditTierImmutable,
	"runtimeBrokerId":                  EditTierImmutable,
	"profile":                          EditTierImmutable,
	"branch":                           EditTierImmutable,
	"config.branch":                    EditTierImmutable,
	"config.clone_depth":               EditTierImmutable,
	"config.explicit_workspace":        EditTierImmutable,
	"config.empty_per_agent_workspace": EditTierImmutable,
	"config.harness":                   EditTierImmutable,
	"config.harness_config":            EditTierImmutable,
	"config.default_harness_config":    EditTierImmutable,
	"config.config_dir":                EditTierImmutable,
	"config.detached":                  EditTierImmutable,
	"config.hub":                       EditTierImmutable,
}

// wantSessionSensitive lists the keys a resume applies but whose effect on
// a continued conversation depends on the harness.
var wantSessionSensitive = map[string]bool{
	"config.model":             true,
	"config.thinking_level":    true,
	"config.auth_selectedType": true,
}

// wantTierDisposition is the design's phase rule per tier (design s4.1),
// before the per-key exceptions in wantDispositionException.
var wantTierDisposition = map[EditTier]map[state.Phase]EditDisposition{
	EditTierMetadata: {
		state.PhaseCreated: EditImmediate, state.PhaseStopped: EditImmediate, state.PhaseError: EditImmediate,
		state.PhaseSuspended: EditImmediate, state.PhaseRunning: EditImmediate, state.PhaseProvisioning: EditImmediate,
		state.PhaseCloning: EditImmediate, state.PhaseStarting: EditImmediate, state.PhaseStopping: EditImmediate,
	},
	EditTierContainer: {
		state.PhaseCreated: EditNow, state.PhaseStopped: EditNow, state.PhaseError: EditNow,
		state.PhaseSuspended: EditNow, state.PhaseRunning: EditHeld, state.PhaseProvisioning: EditHeld,
		state.PhaseCloning: EditHeld, state.PhaseStarting: EditHeld, state.PhaseStopping: EditHeld,
	},
	EditTierProvision: {
		state.PhaseCreated: EditReincarnate, state.PhaseStopped: EditReincarnate, state.PhaseError: EditReincarnate,
		state.PhaseSuspended: EditReincarnate, state.PhaseRunning: EditReincarnate, state.PhaseProvisioning: EditReincarnate,
		state.PhaseCloning: EditReincarnate, state.PhaseStarting: EditReincarnate, state.PhaseStopping: EditReincarnate,
	},
	EditTierPrincipal: {
		state.PhaseCreated: EditReincarnate, state.PhaseStopped: EditReincarnate, state.PhaseError: EditReincarnate,
		state.PhaseSuspended: EditReincarnate, state.PhaseRunning: EditReincarnate, state.PhaseProvisioning: EditReincarnate,
		state.PhaseCloning: EditReincarnate, state.PhaseStarting: EditReincarnate, state.PhaseStopping: EditReincarnate,
	},
	EditTierImmutable: {
		state.PhaseCreated: EditLocked, state.PhaseStopped: EditLocked, state.PhaseError: EditLocked,
		state.PhaseSuspended: EditLocked, state.PhaseRunning: EditLocked, state.PhaseProvisioning: EditLocked,
		state.PhaseCloning: EditLocked, state.PhaseStarting: EditLocked, state.PhaseStopping: EditLocked,
	},
}

// wantDispositionException lists the cells where a key departs from its
// tier's rule (design s4.2).
var wantDispositionException = map[string]map[state.Phase]EditDisposition{
	// The hub already stores the timezone pin in every phase and applies it
	// at the next start.
	"explicitTimezone": {
		state.PhaseRunning: EditNow, state.PhaseProvisioning: EditNow, state.PhaseCloning: EditNow,
		state.PhaseStarting: EditNow, state.PhaseStopping: EditNow,
	},
	// A resume does not send the task again.
	"config.task": {state.PhaseSuspended: EditLocked},
	// GCP identity is written through before the first start.
	"gcp_identity": {state.PhaseCreated: EditNow},
	// The role cannot change before the first start.
	"agentRole": {state.PhaseCreated: EditLocked},
}

// TestAgentEditTable_CoversEveryScionConfigKey fails when an api.ScionConfig
// key has no row in the mutability table, and when a config row names a key
// api.ScionConfig no longer has. An exported field with no json tag is
// encoded under its Go name, so it needs a row under that name.
func TestAgentEditTable_CoversEveryScionConfigKey(t *testing.T) {
	typ := reflect.TypeOf(api.ScionConfig{})
	jsonKeys := map[string]bool{}
	for i := 0; i < typ.NumField(); i++ {
		key := scionConfigJSONKey(typ.Field(i))
		if key == "" {
			continue
		}
		jsonKeys[key] = true
		_, ok := agentEditFieldByKey[agentConfigKeyPrefix+key]
		assert.True(t, ok, "api.ScionConfig key %q has no row in agentEditFields (agent_config_mutability.go); give it a tier", agentConfigKeyPrefix+key)
	}
	for _, f := range agentEditFields {
		if k, ok := strings.CutPrefix(f.Key, agentConfigKeyPrefix); ok {
			assert.True(t, jsonKeys[k], "table row %q names no api.ScionConfig key", f.Key)
		}
	}
}

func TestScionConfigJSONKey(t *testing.T) {
	typ := reflect.TypeOf(struct {
		Tagged   string `json:"tagged_key,omitempty"`
		Untagged string
		Skipped  string `json:"-"`
		Info     string `json:"-" yaml:"-"`
		hidden   string //nolint:unused // exercises the unexported case
	}{})
	got := []string{}
	for i := 0; i < typ.NumField(); i++ {
		got = append(got, scionConfigJSONKey(typ.Field(i)))
	}
	assert.Equal(t, []string{"tagged_key", "Untagged", "", "", ""}, got)
}

// TestAgentEditTable_EveryKeyEveryPhase enumerates every key and every
// phase and asserts the tier, the session-sensitivity flag and the
// disposition of each cell, and that a deleted agent locks every key.
func TestAgentEditTable_EveryKeyEveryPhase(t *testing.T) {
	require.Len(t, agentEditFields, len(wantEditTier), "every table row must have an expected tier, and the reverse")
	require.Len(t, agentEditFieldByKey, len(agentEditFields), "table keys must be unique")
	require.Len(t, state.Phases(), 9, "a new phase needs a column in wantTierDisposition")

	for _, f := range agentEditFields {
		want, ok := wantEditTier[f.Key]
		require.True(t, ok, "unexpected table key %q", f.Key)
		assert.Equal(t, want, f.Tier, "tier of %s", f.Key)
		assert.Equal(t, wantSessionSensitive[f.Key], f.SessionSensitive, "session sensitivity of %s", f.Key)

		for _, phase := range state.Phases() {
			wantD, ok := wantDispositionException[f.Key][phase]
			if !ok {
				wantD, ok = wantTierDisposition[f.Tier][phase]
				require.True(t, ok, "no expected disposition for tier %s in phase %s", f.Tier, phase)
			}
			got, reason := editDisposition(f, string(phase), false)
			assert.Equal(t, wantD, got, "disposition of %s in %s", f.Key, phase)
			if got == EditLocked {
				assert.NotEmpty(t, reason, "a locked cell needs a reason: %s in %s", f.Key, phase)
			}

			got, reason = editDisposition(f, string(phase), true)
			assert.Equal(t, EditLocked, got, "a deleted agent locks %s in %s", f.Key, phase)
			assert.Equal(t, editReasonDeleted, reason)
		}

		got, _ := editDisposition(f, "", false)
		assert.Equal(t, EditLocked, got, "an unknown phase locks %s", f.Key)
	}
}

// TestBuildAgentEditability_HeldIsNotAvailableYet: a held edit of a
// running or transitional agent is not available yet, so the editability
// served to the UI reports those cells locked with a reason, while the
// cells the design marks now or immediate in those phases stay so.
func TestBuildAgentEditability_HeldIsNotAvailableYet(t *testing.T) {
	full := agentEditAccess{CanUpdate: true, CanChangeRole: true}
	for _, phase := range []state.Phase{state.PhaseRunning, state.PhaseProvisioning, state.PhaseCloning, state.PhaseStarting, state.PhaseStopping} {
		ed := buildAgentEditability(&store.Agent{Phase: string(phase)}, full)
		for _, key := range []string{"config.model", "config.max_turns", "config.max_duration"} {
			assert.Equal(t, FieldEditState{
				Tier:                  EditTierContainer,
				Disposition:           EditLocked,
				SessionSensitive:      wantSessionSensitive[key],
				ClearNeedsReincarnate: true,
				Reason:                editReasonRunning,
			}, ed.Fields[key], "%s in %s", key, phase)
		}
		assert.Equal(t, EditNow, ed.Fields["explicitTimezone"].Disposition, "timezone in %s", phase)
		assert.Equal(t, EditImmediate, ed.Fields["name"].Disposition, "name in %s", phase)
		// Provision-rendered keys too: no endpoint takes them for a running
		// agent yet.
		assert.Equal(t, EditLocked, ed.Fields["config.system_prompt"].Disposition, "system prompt in %s", phase)
		assert.Equal(t, editReasonRunning, ed.Fields["config.system_prompt"].Reason)
		assert.Equal(t, EditLocked, ed.Fields["config.harness"].Disposition, "a fixed key in %s", phase)
		assert.Equal(t, editReasonImmutable, ed.Fields["config.harness"].Reason, "a fixed key keeps its own reason")
		// The role and GCP identity change through the reincarnate endpoint.
		assert.Equal(t, EditReincarnate, ed.Fields["agentRole"].Disposition, "role in %s", phase)
		assert.Equal(t, EditReincarnate, ed.Fields["gcp_identity"].Disposition, "GCP identity in %s", phase)
	}
}

func TestBuildAgentEditability_NoContainerPhases(t *testing.T) {
	full := agentEditAccess{CanUpdate: true, CanChangeRole: true}
	for _, phase := range []state.Phase{state.PhaseCreated, state.PhaseStopped, state.PhaseError, state.PhaseSuspended} {
		ed := buildAgentEditability(&store.Agent{Phase: string(phase)}, full)
		require.Equal(t, string(phase), ed.Phase)
		require.Len(t, ed.Fields, len(agentEditFields))
		assert.Equal(t, EditNow, ed.Fields["config.max_turns"].Disposition, phase)
		assert.True(t, ed.Fields["config.max_turns"].ClearNeedsReincarnate, phase)
		assert.False(t, ed.Fields["config.system_prompt"].ClearNeedsReincarnate, "a T2 key is reincarnate-only anyway")
		// T2 keys are written through in these phases today, but they only
		// take effect at a reincarnation, which is their disposition.
		assert.Equal(t, EditReincarnate, ed.Fields["config.system_prompt"].Disposition, phase)
		assert.Equal(t, EditTierImmutable, ed.Fields["template"].Tier)
		assert.Equal(t, EditLocked, ed.Fields["template"].Disposition)
		assert.NotEmpty(t, ed.Fields["template"].Reason)
	}

	suspended := buildAgentEditability(&store.Agent{Phase: string(state.PhaseSuspended)}, full)
	assert.Equal(t, "session", suspended.Fields["config.model"].Note)
	assert.Equal(t, "session", suspended.Fields["config.thinking_level"].Note)
	assert.Empty(t, suspended.Fields["config.max_turns"].Note)
	assert.Equal(t, EditLocked, suspended.Fields["config.task"].Disposition)
	assert.Equal(t, editReasonNotOnResume, suspended.Fields["config.task"].Reason)

	stopped := buildAgentEditability(&store.Agent{Phase: string(state.PhaseStopped)}, full)
	assert.Empty(t, stopped.Fields["config.model"].Note, "a fresh start begins a new conversation")
	assert.True(t, stopped.Fields["config.model"].SessionSensitive)

	created := buildAgentEditability(&store.Agent{Phase: string(state.PhaseCreated)}, full)
	assert.Equal(t, EditNow, created.Fields["gcp_identity"].Disposition)
	assert.Equal(t, EditLocked, created.Fields["agentRole"].Disposition)
	assert.Equal(t, editReasonRoleCreated, created.Fields["agentRole"].Reason)
}

func TestBuildAgentEditability_CallerAccess(t *testing.T) {
	agent := &store.Agent{Phase: string(state.PhaseStopped)}

	t.Run("cannot delegate locks the role only", func(t *testing.T) {
		ed := buildAgentEditability(agent, agentEditAccess{CanUpdate: true})
		assert.Equal(t, EditLocked, ed.Fields["agentRole"].Disposition)
		assert.Equal(t, editReasonCannotDelegate, ed.Fields["agentRole"].Reason)
		assert.Equal(t, EditNow, ed.Fields["config.model"].Disposition)
		assert.Equal(t, EditReincarnate, ed.Fields["gcp_identity"].Disposition)
	})

	t.Run("no update locks everything", func(t *testing.T) {
		ed := buildAgentEditability(agent, agentEditAccess{})
		for key, st := range ed.Fields {
			assert.Equal(t, EditLocked, st.Disposition, key)
			assert.NotEmpty(t, st.Reason, key)
		}
		assert.Equal(t, editReasonNoUpdate, ed.Fields["config.model"].Reason)
		assert.Equal(t, editReasonImmutable, ed.Fields["template"].Reason, "a fixed field keeps its own reason")
	})

	t.Run("deleted agent locks everything", func(t *testing.T) {
		deleted := &store.Agent{Phase: string(state.PhaseStopped), DeletedAt: time.Now()}
		ed := buildAgentEditability(deleted, agentEditAccess{CanUpdate: true, CanChangeRole: true})
		for key, st := range ed.Fields {
			assert.Equal(t, EditLocked, st.Disposition, key)
			assert.Equal(t, editReasonDeleted, st.Reason, key)
		}
	})

	assert.Nil(t, buildAgentEditability(nil, agentEditAccess{}))
}

func rawConfigOf(t *testing.T, body string) map[string]json.RawMessage {
	t.Helper()
	var m map[string]json.RawMessage
	require.NoError(t, json.Unmarshal([]byte(body), &m))
	return m
}

func TestReincarnateOnlyConfigEdits(t *testing.T) {
	body := `{"model":"m","Max_Turns":0,"max_duration":null,"thinking_level":50,"image":"","system_prompt":"p","skills":[],"not_a_key":1,"env":{}}`
	var req api.ScionConfig
	require.NoError(t, json.Unmarshal([]byte(body), &req))
	raw := rawConfigOf(t, body)
	stored := &api.ScionConfig{
		MaxTurns: 3, MaxDuration: "1h", Image: "img", Env: map[string]string{"A": "1"},
		Skills: []api.SkillReference{{URI: "s"}},
	}
	provision, cleared := reincarnateOnlyConfigEdits(raw, &req, stored)
	assert.Equal(t, []string{"config.skills", "config.system_prompt"}, provision)
	assert.Equal(t, []string{"config.env", "config.image", "config.max_duration", "config.max_turns"}, cleared)

	warnings := reincarnateOnlyEditWarnings(raw, &req, stored)
	require.Len(t, warnings, 2)
	assert.Contains(t, warnings[0], "config.skills, config.system_prompt: stored now; rendered at the next reincarnation")
	assert.Contains(t, warnings[1], "config.env, config.image, config.max_duration, config.max_turns: cleared now")

	t.Run("unchanged values are not reported", func(t *testing.T) {
		// Clearing what is already empty, and resending a stored prompt,
		// change nothing.
		same := &api.ScionConfig{SystemPrompt: "p"}
		provision, cleared := reincarnateOnlyConfigEdits(raw, &req, same)
		assert.Empty(t, provision)
		assert.Empty(t, cleared)
		assert.Empty(t, reincarnateOnlyEditWarnings(raw, &req, nil), "nothing stored: only the new system prompt would count")
	})

	five := rawConfigOf(t, `{"model":"m","max_turns":5}`)
	assert.Empty(t, reincarnateOnlyEditWarnings(five, &api.ScionConfig{Model: "m", MaxTurns: 5}, stored))
	assert.Empty(t, reincarnateOnlyEditWarnings(nil, nil, stored))
}

func TestRawJSONIsEmpty(t *testing.T) {
	for _, v := range []string{``, `null`, `""`, `0`, `0.0`, `false`, `[]`, `{}`, ` [ ] `, `{ }`} {
		assert.True(t, rawJSONIsEmpty(json.RawMessage(v)), v)
	}
	for _, v := range []string{`1`, `"x"`, `true`, `[1]`, `{"a":1}`, `-1`} {
		assert.False(t, rawJSONIsEmpty(json.RawMessage(v)), v)
	}
}

func TestAgentUpdateAppliedKeys(t *testing.T) {
	got := agentUpdateAppliedKeys("n", map[string]string{}, nil, "", true,
		rawConfigOf(t, `{"max_turns":3,"Model":"m","bogus":1}`), false, true)
	assert.Equal(t, []string{"config.max_turns", "config.model", "explicitTimezone", "labels", "name"}, got.Applied)

	empty := agentUpdateAppliedKeys("", nil, nil, "", false, nil, false, false)
	require.NotNil(t, empty.Applied, "applied is an empty list, not null")
	assert.Empty(t, empty.Applied)
}

func TestConfigFieldUnchanged(t *testing.T) {
	tl := 1
	stored := &api.ScionConfig{Harness: "claude", Volumes: []api.VolumeMount{{Source: "/a", Target: "/b"}}, ThinkingLevel: &tl}
	assert.True(t, configFieldUnchanged("harness", &api.ScionConfig{Harness: "claude"}, stored))
	assert.False(t, configFieldUnchanged("harness", &api.ScionConfig{Harness: "generic"}, stored))
	assert.False(t, configFieldUnchanged("harness", &api.ScionConfig{}, stored), "clearing a stored value is a change")
	assert.True(t, configFieldUnchanged("branch", &api.ScionConfig{}, stored), "empty where nothing is stored is no change")
	assert.True(t, configFieldUnchanged("branch", &api.ScionConfig{}, nil))
	assert.False(t, configFieldUnchanged("branch", &api.ScionConfig{Branch: "x"}, nil))
	assert.True(t, configFieldUnchanged("volumes", &api.ScionConfig{Volumes: []api.VolumeMount{{Source: "/a", Target: "/b"}}}, stored))
	assert.True(t, configFieldUnchanged("command_args", &api.ScionConfig{CommandArgs: []string{}}, stored), "an empty list where none is stored")
	assert.True(t, configFieldUnchanged("not_a_key", &api.ScionConfig{}, stored))
}

func TestLockedPatchKeys(t *testing.T) {
	raw := func(body string) map[string]json.RawMessage { return rawConfigOf(t, body) }
	decode := func(body string) *api.ScionConfig {
		var c api.ScionConfig
		require.NoError(t, json.Unmarshal([]byte(body), &c))
		return &c
	}
	stopped := &store.Agent{Phase: string(state.PhaseStopped), AppliedConfig: &store.AgentAppliedConfig{
		InlineConfig: &api.ScionConfig{Harness: "claude", Branch: "main"},
	}}

	t.Run("container and provision keys are writable with no container", func(t *testing.T) {
		body := `{"model":"m","max_turns":0,"system_prompt":"p","bogus_key":1}`
		assert.Nil(t, lockedPatchKeys(stopped, []string{"name", "labels", "explicitTimezone"}, raw(body), decode(body), nil))
	})

	t.Run("a fixed key that would change is refused as fixed", func(t *testing.T) {
		body := `{"harness":"generic","branch":"main","clone_depth":"full","model":"m"}`
		ref := lockedPatchKeys(stopped, nil, raw(body), decode(body), nil)
		require.NotNil(t, ref)
		assert.False(t, ref.Conflict)
		assert.Equal(t, map[string]string{
			"config.harness":     editReasonFixedPatch,
			"config.clone_depth": editReasonFixedPatch,
		}, ref.Fields, "branch echoes the stored value and is ignored")
	})

	t.Run("an empty fixed key where none is stored is ignored", func(t *testing.T) {
		body := `{"config_dir":"","detached":null,"hub":null,"user":""}`
		assert.Nil(t, lockedPatchKeys(stopped, nil, raw(body), decode(body), nil))
	})

	t.Run("the task of a suspended agent is a phase conflict", func(t *testing.T) {
		suspended := &store.Agent{Phase: string(state.PhaseSuspended)}
		body := `{"task":"next","max_turns":3}`
		ref := lockedPatchKeys(suspended, nil, raw(body), decode(body), nil)
		require.NotNil(t, ref)
		assert.True(t, ref.Conflict)
		assert.Equal(t, map[string]string{"config.task": editReasonNotOnResume}, ref.Fields)
	})

	t.Run("config of a running agent is a phase conflict", func(t *testing.T) {
		running := &store.Agent{Phase: string(state.PhaseRunning)}
		body := `{"max_turns":3}`
		ref := lockedPatchKeys(running, []string{"name"}, raw(body), decode(body), nil)
		require.NotNil(t, ref)
		assert.True(t, ref.Conflict)
		assert.Equal(t, map[string]string{"config.max_turns": editReasonRunning}, ref.Fields, "a rename stays allowed")
	})

	t.Run("a fixed key of a running agent is locked by the phase, echo or not", func(t *testing.T) {
		running := &store.Agent{Phase: string(state.PhaseRunning), AppliedConfig: &store.AgentAppliedConfig{
			InlineConfig: &api.ScionConfig{Harness: "claude"},
		}}
		for _, body := range []string{`{"branch":"x"}`, `{"harness":"claude"}`} {
			ref := lockedPatchKeys(running, nil, raw(body), decode(body), nil)
			require.NotNil(t, ref, body)
			assert.True(t, ref.Conflict, body)
			for _, reason := range ref.Fields {
				assert.Equal(t, editReasonRunning, reason, body)
			}
		}
	})

	t.Run("a fixed key equal to the applied value is unchanged", func(t *testing.T) {
		live := &store.Agent{Phase: string(state.PhaseStopped), AppliedConfig: &store.AgentAppliedConfig{
			Branch: "scion/live", HarnessConfig: "hc-live", InlineConfig: &api.ScionConfig{CloneDepth: "full"},
		}}
		applied := appliedFixedValues(live, "claude")
		echo := `{"branch":"scion/live","harness_config":"hc-live","harness":"claude"}`
		assert.Nil(t, lockedPatchKeys(live, nil, raw(echo), decode(echo), applied))

		changed := `{"branch":"other","harness_config":"hc-other","harness":"generic"}`
		ref := lockedPatchKeys(live, nil, raw(changed), decode(changed), applied)
		require.NotNil(t, ref)
		assert.False(t, ref.Conflict)
		assert.Len(t, ref.Fields, 3)

		// An empty value clears a stored inline value even though no applied
		// value is set for the key: that is a change.
		clear := `{"clone_depth":""}`
		ref = lockedPatchKeys(live, nil, raw(clear), decode(clear), applied)
		require.NotNil(t, ref)
		assert.Equal(t, map[string]string{"config.clone_depth": editReasonFixedPatch}, ref.Fields)
	})

	t.Run("anything on a deleted agent is a conflict", func(t *testing.T) {
		deleted := &store.Agent{Phase: string(state.PhaseStopped), DeletedAt: time.Now()}
		ref := lockedPatchKeys(deleted, []string{"name", "annotations"}, nil, nil, nil)
		require.NotNil(t, ref)
		assert.True(t, ref.Conflict)
		assert.Equal(t, map[string]string{"name": editReasonDeleted, "annotations": editReasonDeleted}, ref.Fields)
	})
}
