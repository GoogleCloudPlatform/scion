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
	"github.com/GoogleCloudPlatform/scion/pkg/config"
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

	warnings := reincarnateOnlyEditWarnings(provision, cleared)
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
		// With nothing stored, only the new system prompt is a change: the
		// clears and the empty skills list clear nothing.
		provision, cleared = reincarnateOnlyConfigEdits(raw, &req, nil)
		assert.Equal(t, []string{"config.system_prompt"}, provision)
		assert.Empty(t, cleared)
	})

	t.Run("values the start merge applies are not reported as cleared", func(t *testing.T) {
		body := `{"max_duration":"0","thinking_level":0,"telemetry":{"enabled":false}}`
		var req api.ScionConfig
		require.NoError(t, json.Unmarshal([]byte(body), &req))
		tl := 40
		on := true
		stored := &api.ScionConfig{MaxDuration: "1h", ThinkingLevel: &tl, Telemetry: &api.TelemetryConfig{Enabled: &on}}
		provision, cleared := reincarnateOnlyConfigEdits(rawConfigOf(t, body), &req, stored)
		assert.Empty(t, provision)
		assert.Empty(t, cleared)
		assert.Empty(t, reincarnateOnlyEditWarnings(reincarnateOnlyConfigEdits(rawConfigOf(t, body), &req, stored)))
	})

	five := rawConfigOf(t, `{"model":"m","max_turns":5}`)
	assert.Empty(t, reincarnateOnlyEditWarnings(reincarnateOnlyConfigEdits(five, &api.ScionConfig{Model: "m", MaxTurns: 5}, stored)))
	assert.Empty(t, reincarnateOnlyEditWarnings(reincarnateOnlyConfigEdits(nil, nil, stored)))
}

func TestConfigEditDisposition(t *testing.T) {
	body := `{"model":"m","Max_Turns":0,"max_duration":"0","thinking_level":0,"image":"img","system_prompt":"p","skills":[],"branch":"b","not_a_key":1,"env":{"B":"2"},"volumes":[]}`
	var req api.ScionConfig
	require.NoError(t, json.Unmarshal([]byte(body), &req))
	tl := 40
	stored := &api.ScionConfig{
		Model: "old", MaxTurns: 3, MaxDuration: "1h", ThinkingLevel: &tl, Image: "img",
		Env: map[string]string{"A": "1"}, Skills: []api.SkillReference{{URI: "s"}},
	}
	removed := map[string][]string{"env": {"A"}}
	provision, cleared := reincarnateOnlyConfigEdits(rawConfigOf(t, body), &req, stored)
	applied, held := configEditDisposition(rawConfigOf(t, body), &req, stored, provision, cleared, removed)
	// max_duration "0" and thinking_level 0 apply at the next start; the
	// image and branch are echoes and volumes clears nothing.
	assert.Equal(t, []string{"config.max_duration", "config.model", "config.thinking_level"}, applied)
	assert.Equal(t, []string{"config.env", "config.max_turns", "config.skills", "config.system_prompt"}, held)

	t.Run("an env edit that removes nothing is applied", func(t *testing.T) {
		raw := rawConfigOf(t, `{"env":{"A":"1","B":"2"}}`)
		req := &api.ScionConfig{Env: map[string]string{"A": "1", "B": "2"}}
		provision, cleared := reincarnateOnlyConfigEdits(raw, req, stored)
		applied, held := configEditDisposition(raw, req, stored, provision, cleared, nil)
		assert.Equal(t, []string{"config.env"}, applied)
		assert.Empty(t, held)
	})

	t.Run("unchanged values are in neither list", func(t *testing.T) {
		echo := `{"model":"old","system_prompt":"p"}`
		var req api.ScionConfig
		require.NoError(t, json.Unmarshal([]byte(echo), &req))
		stored := &api.ScionConfig{Model: "old", SystemPrompt: "p"}
		provision, cleared := reincarnateOnlyConfigEdits(rawConfigOf(t, echo), &req, stored)
		applied, held := configEditDisposition(rawConfigOf(t, echo), &req, stored, provision, cleared, nil)
		assert.Empty(t, applied)
		assert.Empty(t, held)
	})
}

func TestAgentUpdateDisposition(t *testing.T) {
	before := agentPatchSnapshotOf(&store.Agent{
		Name: "n", Labels: map[string]string{"a": "1"}, TaskSummary: "t",
		AppliedConfig: &store.AgentAppliedConfig{GCPIdentity: &store.GCPIdentityConfig{MetadataMode: store.GCPMetadataModeBlock}},
	})

	t.Run("echoes are in no list", func(t *testing.T) {
		after := &store.Agent{
			Name: "n", Labels: map[string]string{"a": "1"}, Annotations: map[string]string{}, TaskSummary: "t",
			AppliedConfig: &store.AgentAppliedConfig{GCPIdentity: &store.GCPIdentityConfig{MetadataMode: store.GCPMetadataModeBlock}},
		}
		got := agentUpdateDisposition(before, after, nil, nil, false)
		for name, l := range map[string][]string{"applied": got.Applied, "held": got.Held, "heldForReincarnate": got.HeldForReincarnate} {
			require.NotNil(t, l, "%s is an empty list, not null", name)
			assert.Empty(t, l, name)
		}
	})

	t.Run("changes are listed by effect", func(t *testing.T) {
		after := &store.Agent{
			Name: "renamed", Labels: map[string]string{"a": "2"}, Annotations: map[string]string{"k": "v"}, TaskSummary: "t2",
			AppliedConfig: &store.AgentAppliedConfig{GCPIdentity: &store.GCPIdentityConfig{MetadataMode: store.GCPMetadataModePassthrough}},
		}
		got := agentUpdateDisposition(before, after, []string{"config.model"}, []string{"config.system_prompt"}, true)
		assert.Equal(t, []string{"annotations", "config.model", "explicitTimezone", "gcp_identity", "labels", "name", "taskSummary"}, got.Applied)
		assert.Empty(t, got.Held)
		assert.Equal(t, []string{"config.system_prompt"}, got.HeldForReincarnate)
	})
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

// TestStartMergeKeepsBase_MatchesBrokerMerge pins the "cleared now; keeps its
// previous value until the next reincarnation" warning to what the broker's
// start-time merge (config.MergeScionConfig) really does: for each request
// value, startMergeKeepsBase must say "kept" exactly when merging the
// request into a fully populated persisted config leaves that key as it was.
func TestStartMergeKeepsBase_MatchesBrokerMerge(t *testing.T) {
	enabled, disabled := true, false
	tl := 60
	base := &api.ScionConfig{
		Model: "old", Image: "img", User: "u", AuthSelectedType: "api-key", Task: "t", TaskFlag: "--task",
		MaxTurns: 9, MaxModelCalls: 9, MaxDuration: "1h", ThinkingLevel: &tl,
		Env: map[string]string{"A": "1"}, Volumes: []api.VolumeMount{{Source: "/a", Target: "/b"}},
		CommandArgs: []string{"--x"}, MCPServers: map[string]api.MCPServerConfig{"m": {Transport: "stdio"}},
		Resources:  &api.ResourceSpec{Disk: "10Gi", Requests: api.ResourceList{CPU: "1"}},
		Kubernetes: &api.KubernetesConfig{Namespace: "ns", NodeSelector: map[string]string{"pool": "a"}},
		Telemetry:  &api.TelemetryConfig{Enabled: &enabled, Cloud: &api.TelemetryCloudConfig{Endpoint: "e"}},
	}
	zero := 0
	cases := []struct {
		name string
		key  string
		req  api.ScionConfig
		kept bool
	}{
		{name: "max_turns 0 (Unlimited)", key: "max_turns", req: api.ScionConfig{MaxTurns: 0}, kept: true},
		{name: "max_model_calls 0", key: "max_model_calls", req: api.ScionConfig{}, kept: true},
		{name: `max_duration "0" (Unlimited)`, key: "max_duration", req: api.ScionConfig{MaxDuration: "0"}, kept: false},
		{name: "max_duration cleared", key: "max_duration", req: api.ScionConfig{}, kept: true},
		{name: "thinking_level 0", key: "thinking_level", req: api.ScionConfig{ThinkingLevel: &zero}, kept: false},
		{name: "thinking_level null", key: "thinking_level", req: api.ScionConfig{}, kept: true},
		{name: "model cleared", key: "model", req: api.ScionConfig{}, kept: true},
		{name: "model set", key: "model", req: api.ScionConfig{Model: "new"}, kept: false},
		{name: "image cleared", key: "image", req: api.ScionConfig{}, kept: true},
		{name: "user cleared", key: "user", req: api.ScionConfig{}, kept: true},
		{name: "env emptied", key: "env", req: api.ScionConfig{Env: map[string]string{}}, kept: true},
		{name: "volumes emptied", key: "volumes", req: api.ScionConfig{Volumes: []api.VolumeMount{}}, kept: true},
		{name: "command_args emptied", key: "command_args", req: api.ScionConfig{CommandArgs: []string{}}, kept: true},
		{name: "mcp_servers emptied", key: "mcp_servers", req: api.ScionConfig{MCPServers: map[string]api.MCPServerConfig{}}, kept: true},
		{name: "resources null", key: "resources", req: api.ScionConfig{}, kept: true},
		{name: "resources empty", key: "resources", req: api.ScionConfig{Resources: &api.ResourceSpec{}}, kept: true},
		{name: "kubernetes empty", key: "kubernetes", req: api.ScionConfig{Kubernetes: &api.KubernetesConfig{}}, kept: true},
		{name: "telemetry null", key: "telemetry", req: api.ScionConfig{}, kept: true},
		{name: "telemetry disabled", key: "telemetry", req: api.ScionConfig{Telemetry: &api.TelemetryConfig{Enabled: &disabled}}, kept: false},
		{name: "telemetry with only an empty cloud block", key: "telemetry", req: api.ScionConfig{Telemetry: &api.TelemetryConfig{Cloud: &api.TelemetryCloudConfig{}}}, kept: true},
		{name: "kubernetes with only an empty node selector", key: "kubernetes", req: api.ScionConfig{Kubernetes: &api.KubernetesConfig{NodeSelector: map[string]string{}}}, kept: true},
		{name: "kubernetes namespace set", key: "kubernetes", req: api.ScionConfig{Kubernetes: &api.KubernetesConfig{Namespace: "other"}}, kept: false},
		{name: "resources with only empty requests", key: "resources", req: api.ScionConfig{Resources: &api.ResourceSpec{Requests: api.ResourceList{}}}, kept: true},
		{name: "resources disk set", key: "resources", req: api.ScionConfig{Resources: &api.ResourceSpec{Disk: "20Gi"}}, kept: false},
		{name: "auth_selectedType cleared", key: "auth_selectedType", req: api.ScionConfig{}, kept: true},
		{name: "task cleared", key: "task", req: api.ScionConfig{}, kept: true},
		{name: "task_flag cleared", key: "task_flag", req: api.ScionConfig{}, kept: true},
		{name: "env with a new key", key: "env", req: api.ScionConfig{Env: map[string]string{"B": "2"}}, kept: false},
		{name: "volumes with an entry", key: "volumes", req: api.ScionConfig{Volumes: []api.VolumeMount{{Source: "/c", Target: "/d"}}}, kept: false},
		{name: "mcp_servers with an entry", key: "mcp_servers", req: api.ScionConfig{MCPServers: map[string]api.MCPServerConfig{"n": {Transport: "http"}}}, kept: false},
		{name: "command_args set", key: "command_args", req: api.ScionConfig{CommandArgs: []string{"--y"}}, kept: false},
	}
	covered := map[string]bool{}
	for _, tc := range cases {
		covered[tc.key] = true
	}
	// Every T1 config key needs at least one case, so a new one cannot get
	// the cleared warning wrong unnoticed.
	for _, f := range agentEditFields {
		if k, ok := strings.CutPrefix(f.Key, agentConfigKeyPrefix); ok && f.Tier == EditTierContainer {
			assert.True(t, covered[k], "T1 key %s has no case in this test", f.Key)
		}
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := tc.req
			merged := config.MergeScionConfig(base, &req)
			i := scionConfigFieldByJSONKey[tc.key]
			keptByMerge := reflect.DeepEqual(reflect.ValueOf(*merged).Field(i).Interface(), reflect.ValueOf(*base).Field(i).Interface())
			require.Equal(t, tc.kept, keptByMerge, "the broker merge itself (test expectation is stale)")
			assert.Equal(t, tc.kept, startMergeKeepsBase(tc.key, &req))
		})
	}
}

func TestDropFixedConfigKeys(t *testing.T) {
	body := `{"Branch":"b","harness":"claude","max_turns":4,"bogus":1}`
	raw := rawConfigOf(t, body)
	var cfg api.ScionConfig
	require.NoError(t, json.Unmarshal([]byte(body), &cfg))
	present := map[string]bool{"branch": true, "harness": true, "max_turns": true, "bogus": true}

	dropFixedConfigKeys(&cfg, raw, present)
	assert.Empty(t, cfg.Branch)
	assert.Empty(t, cfg.Harness)
	assert.Equal(t, 4, cfg.MaxTurns)
	assert.Equal(t, map[string]bool{"max_turns": true, "bogus": true}, present)
	assert.Len(t, raw, 2)
	assert.Contains(t, raw, "max_turns")
	assert.Contains(t, raw, "bogus")
}

func TestRemovedEntriesWarnings(t *testing.T) {
	old := &api.ScionConfig{
		Env:        map[string]string{"KEEP": "1", "GONE": "2", "SCION_AUTO_EXPOSE_PORTS": "true", "TZ": "UTC"},
		MCPServers: map[string]api.MCPServerConfig{"a": {}, "b": {}},
		Volumes:    []api.VolumeMount{{Target: "/x"}, {Target: "/y"}},
	}
	req := &api.ScionConfig{
		Env:        map[string]string{"KEEP": "1"},
		MCPServers: map[string]api.MCPServerConfig{"a": {}},
		Volumes:    []api.VolumeMount{{Target: "/y"}},
	}
	all := map[string]bool{"env": true, "mcp_servers": true, "volumes": true}

	warnings := func(old, req *api.ScionConfig, present map[string]bool, canViewEnv bool) []string {
		return removedEntriesWarnings(removedConfigEntries(old, req, present, canViewEnv))
	}
	assert.Equal(t, map[string][]string{"env": {"GONE"}, "mcp_servers": {"b"}, "volumes": {"/x"}},
		removedConfigEntries(old, req, all, true))
	got := warnings(old, req, all, true)
	require.Len(t, got, 3)
	assert.Contains(t, got[0], "config.env: removed GONE now")
	assert.Contains(t, got[1], "config.mcp_servers: removed b now")
	assert.Contains(t, got[2], "config.volumes: removed /x now")

	assert.Len(t, warnings(old, req, all, false), 2, "env key names are hidden from a caller who cannot see the env")
	assert.Empty(t, warnings(old, req, map[string]bool{}, true), "keys the request does not name are not removals")
	emptied := &api.ScionConfig{Env: map[string]string{}, MCPServers: map[string]api.MCPServerConfig{}, Volumes: []api.VolumeMount{}}
	assert.Empty(t, warnings(old, emptied, all, true), "an emptied key is the cleared warning's case")
	assert.Empty(t, warnings(nil, req, all, true))
}

// TestStartMergeKeepsBase_ValuesThatApply: a value the start merge applies is
// never reported as kept, including one equal to a placeholder of the
// populated config the check merges into (it merges into two different
// ones for that reason).
func TestStartMergeKeepsBase_ValuesThatApply(t *testing.T) {
	on := true
	turns := 7919
	for _, tc := range []struct {
		key string
		req api.ScionConfig
	}{
		{key: "telemetry", req: api.ScionConfig{Telemetry: &api.TelemetryConfig{Enabled: &on}}},
		{key: "max_turns", req: api.ScionConfig{MaxTurns: turns}},
		{key: "thinking_level", req: api.ScionConfig{ThinkingLevel: &turns}},
		{key: "max_duration", req: api.ScionConfig{MaxDuration: "0"}},
		{key: "model", req: api.ScionConfig{Model: "\x00populated-a"}},
	} {
		assert.False(t, startMergeKeepsBase(tc.key, &tc.req), "%s %+v applies at the next start", tc.key, tc.req)
	}
	assert.True(t, startMergeKeepsBase("not_a_key", &api.ScionConfig{}))
	assert.True(t, startMergeKeepsBase("model", nil))
}
