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
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Tests for ptone/scion#3972: a config PATCH is accepted for an agent with
// no container (created, stopped, error, suspended), is dispatched by the
// agent's next start or resume, and the GET response reports per-field
// editability.

// dispatchedStart is what one DispatchAgentStart call sent to the broker.
type dispatchedStart struct {
	inline api.ScionConfig
	model  string
	resume bool
}

// inlineCaptureDispatcher records the InlineConfig each start dispatches:
// the real dispatcher sends agent.AppliedConfig.InlineConfig as the start
// request's InlineConfig.
type inlineCaptureDispatcher struct {
	*reincarnateTestDispatcher
	mu     sync.Mutex
	starts []dispatchedStart
}

func (d *inlineCaptureDispatcher) DispatchAgentStart(ctx context.Context, agent *store.Agent, task string, resume bool) error {
	d.mu.Lock()
	ds := dispatchedStart{resume: resume}
	if agent.AppliedConfig != nil {
		ds.model = agent.AppliedConfig.Model
		if agent.AppliedConfig.InlineConfig != nil {
			ds.inline = *agent.AppliedConfig.InlineConfig
		}
	}
	d.starts = append(d.starts, ds)
	d.mu.Unlock()
	return d.reincarnateTestDispatcher.DispatchAgentStart(ctx, agent, task, resume)
}

func (d *inlineCaptureDispatcher) snapshot() []dispatchedStart {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]dispatchedStart(nil), d.starts...)
}

// agentPatchResponse decodes the parts of the PATCH response these tests
// read.
type agentPatchResponse struct {
	StateVersion  int64                     `json:"stateVersion"`
	AppliedConfig *store.AgentAppliedConfig `json:"appliedConfig"`
	Warnings      []string                  `json:"warnings"`
	Disposition   *AgentUpdateDisposition   `json:"disposition"`
	// RawDisposition is the disposition object as sent, to check that
	// every list is present.
	RawDisposition map[string]json.RawMessage `json:"-"`
}

func newEditTestAgent(t *testing.T, s store.Store, project *store.Project, broker *store.RuntimeBroker, phase state.Phase) *store.Agent {
	t.Helper()
	a := newReincarnateTestAgent(t, s, project, broker, func(a *store.Agent) {
		a.Phase = string(phase)
		a.AppliedConfig.Model = "old-model"
		a.AppliedConfig.InlineConfig = &api.ScionConfig{
			Harness:  "claude",
			Model:    "old-model",
			MaxTurns: 3,
			Volumes:  []api.VolumeMount{{Source: "/host/data", Target: "/data"}},
		}
	})
	got, err := s.GetAgent(context.Background(), a.ID)
	require.NoError(t, err)
	return got
}

func patchAgentBody(t *testing.T, srv *Server, agentID string, body map[string]interface{}) (*agentPatchResponse, int, string) {
	t.Helper()
	rec := doRequest(t, srv, http.MethodPatch, "/api/v1/agents/"+agentID, body)
	if rec.Code != http.StatusOK {
		return nil, rec.Code, rec.Body.String()
	}
	var resp agentPatchResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	var raw struct {
		Disposition map[string]json.RawMessage `json:"disposition"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &raw))
	resp.RawDisposition = raw.Disposition
	return &resp, rec.Code, rec.Body.String()
}

// TestAgentConfigPatch_SuspendedAgentResumeDispatchesEdit: PATCH config on a
// suspended agent is accepted, and the next resume dispatches the edited
// InlineConfig (and keeps the keys the PATCH did not name).
func TestAgentConfigPatch_SuspendedAgentResumeDispatchesEdit(t *testing.T) {
	disp := &inlineCaptureDispatcher{reincarnateTestDispatcher: newReincarnateTestDispatcher()}
	srv, s, project, broker := setupReincarnateTestServer(t, disp)
	agent := newEditTestAgent(t, s, project, broker, state.PhaseSuspended)

	resp, code, body := patchAgentBody(t, srv, agent.ID, map[string]interface{}{
		"config":       map[string]interface{}{"model": "new-model", "max_turns": 7},
		"stateVersion": agent.StateVersion,
	})
	require.Equal(t, http.StatusOK, code, body)
	require.NotNil(t, resp.Disposition)
	assert.Equal(t, []string{"config.max_turns", "config.model"}, resp.Disposition.Applied)
	assert.Empty(t, resp.Warnings)
	assert.Greater(t, resp.StateVersion, agent.StateVersion)

	rec := doRequest(t, srv, http.MethodPost, "/api/v1/agents/"+agent.ID+"/start", nil)
	require.Less(t, rec.Code, 300, rec.Body.String())

	starts := disp.snapshot()
	require.Len(t, starts, 1)
	assert.True(t, starts[0].resume, "a start of a suspended agent resumes it")
	assert.Equal(t, "new-model", starts[0].inline.Model)
	assert.Equal(t, "new-model", starts[0].model)
	assert.Equal(t, 7, starts[0].inline.MaxTurns)
	assert.Equal(t, []api.VolumeMount{{Source: "/host/data", Target: "/data"}}, starts[0].inline.Volumes, "unmentioned keys are kept")
}

// TestAgentConfigPatch_ErrorAgentFreshStartDispatchesEdit: likewise for an
// agent in error, started fresh.
func TestAgentConfigPatch_ErrorAgentFreshStartDispatchesEdit(t *testing.T) {
	disp := &inlineCaptureDispatcher{reincarnateTestDispatcher: newReincarnateTestDispatcher()}
	srv, s, project, broker := setupReincarnateTestServer(t, disp)
	agent := newEditTestAgent(t, s, project, broker, state.PhaseError)

	resp, code, body := patchAgentBody(t, srv, agent.ID, map[string]interface{}{
		"config":       map[string]interface{}{"max_turns": 9, "max_duration": "2h"},
		"stateVersion": agent.StateVersion,
	})
	require.Equal(t, http.StatusOK, code, body)
	assert.Equal(t, []string{"config.max_duration", "config.max_turns"}, resp.Disposition.Applied)

	rec := doRequest(t, srv, http.MethodPost, "/api/v1/agents/"+agent.ID+"/start", nil)
	require.Less(t, rec.Code, 300, rec.Body.String())

	starts := disp.snapshot()
	require.Len(t, starts, 1)
	assert.False(t, starts[0].resume, "an error-phase agent starts fresh unless it asks to resume")
	assert.Equal(t, 9, starts[0].inline.MaxTurns)
	assert.Equal(t, "2h", starts[0].inline.MaxDuration)
	assert.Equal(t, "old-model", starts[0].inline.Model)
}

// TestAgentConfigPatch_PhaseGate: config is accepted with no container and
// refused (409, nothing written) with one, or while one is being created or
// removed.
func TestAgentConfigPatch_PhaseGate(t *testing.T) {
	accepted := map[state.Phase]bool{
		state.PhaseCreated: true, state.PhaseStopped: true, state.PhaseError: true, state.PhaseSuspended: true,
	}
	for _, phase := range state.Phases() {
		t.Run(string(phase), func(t *testing.T) {
			disp := &inlineCaptureDispatcher{reincarnateTestDispatcher: newReincarnateTestDispatcher()}
			srv, s, project, broker := setupReincarnateTestServer(t, disp)
			agent := newEditTestAgent(t, s, project, broker, phase)

			_, code, body := patchAgentBody(t, srv, agent.ID, map[string]interface{}{
				"config": map[string]interface{}{"max_turns": 11},
			})
			after, err := s.GetAgent(context.Background(), agent.ID)
			require.NoError(t, err)
			if accepted[phase] {
				require.Equal(t, http.StatusOK, code, body)
				assert.Equal(t, 11, after.AppliedConfig.InlineConfig.MaxTurns)
				return
			}
			require.Equal(t, http.StatusConflict, code, body)
			assert.Contains(t, body, "'created', 'stopped', 'error' or 'suspended'")
			assert.Equal(t, 3, after.AppliedConfig.InlineConfig.MaxTurns)
			assert.Equal(t, agent.StateVersion, after.StateVersion)
		})
	}
}

// TestAgentConfigPatch_StaleStateVersionConflicts: a save made against an
// older state version is refused with 409 and writes nothing.
func TestAgentConfigPatch_StaleStateVersionConflicts(t *testing.T) {
	disp := &inlineCaptureDispatcher{reincarnateTestDispatcher: newReincarnateTestDispatcher()}
	srv, s, project, broker := setupReincarnateTestServer(t, disp)
	agent := newEditTestAgent(t, s, project, broker, state.PhaseStopped)

	_, code, body := patchAgentBody(t, srv, agent.ID, map[string]interface{}{
		"config":       map[string]interface{}{"max_turns": 4},
		"stateVersion": agent.StateVersion,
	})
	require.Equal(t, http.StatusOK, code, body)

	_, code, body = patchAgentBody(t, srv, agent.ID, map[string]interface{}{
		"config":       map[string]interface{}{"max_turns": 5},
		"stateVersion": agent.StateVersion,
	})
	require.Equal(t, http.StatusConflict, code, body)
	after, err := s.GetAgent(context.Background(), agent.ID)
	require.NoError(t, err)
	assert.Equal(t, 4, after.AppliedConfig.InlineConfig.MaxTurns)
}

// TestAgentConfigPatch_ReincarnateOnlyWarnings: a PATCH that writes a
// provision-rendered key, or clears or zeroes a container key, says those
// edits take effect at the next reincarnation, and reports exactly the keys
// it warns about under heldForReincarnate.
func TestAgentConfigPatch_ReincarnateOnlyWarnings(t *testing.T) {
	disp := &inlineCaptureDispatcher{reincarnateTestDispatcher: newReincarnateTestDispatcher()}
	srv, s, project, broker := setupReincarnateTestServer(t, disp)
	agent := newEditTestAgent(t, s, project, broker, state.PhaseStopped)

	resp, code, body := patchAgentBody(t, srv, agent.ID, map[string]interface{}{
		"config":      map[string]interface{}{"system_prompt": "be brief", "max_turns": 0, "max_duration": nil},
		"name":        "Renamed Agent",
		"annotations": map[string]string{"k": "v"},
	})
	require.Equal(t, http.StatusOK, code, body)
	assert.Equal(t, []string{"annotations", "name"}, resp.Disposition.Applied, "max_duration was not set, so its null changes nothing")
	assert.Equal(t, []string{"config.max_turns", "config.system_prompt"}, resp.Disposition.HeldForReincarnate)
	require.Len(t, resp.Warnings, 2, "%v", resp.Warnings)
	assert.Contains(t, resp.Warnings[0], "config.system_prompt: stored now; rendered at the next reincarnation")
	// max_duration was not set, so its null changes nothing and is not named.
	assert.Contains(t, resp.Warnings[1], "config.max_turns: cleared now")
	assert.NotContains(t, resp.Warnings[1], "config.max_duration")
	assert.Equal(t, "be brief", resp.AppliedConfig.InlineConfig.SystemPrompt)
	assert.Equal(t, 0, resp.AppliedConfig.InlineConfig.MaxTurns)
}

// TestAgentConfigPatch_CreatedAgentDispositionByEffect: on a created agent,
// a provision-rendered key is stored now but reported held for
// reincarnation (a first start renders the prompt the agent was
// provisioned with), with one warning; a container key is applied.
func TestAgentConfigPatch_CreatedAgentDispositionByEffect(t *testing.T) {
	disp := &inlineCaptureDispatcher{reincarnateTestDispatcher: newReincarnateTestDispatcher()}
	srv, s, project, broker := setupReincarnateTestServer(t, disp)
	agent := newEditTestAgent(t, s, project, broker, state.PhaseCreated)

	resp, code, body := patchAgentBody(t, srv, agent.ID, map[string]interface{}{
		"config": map[string]interface{}{"system_prompt": "be brief"},
	})
	require.Equal(t, http.StatusOK, code, body)
	require.NotNil(t, resp.Disposition.Applied)
	assert.Empty(t, resp.Disposition.Applied)
	assert.Equal(t, []string{"config.system_prompt"}, resp.Disposition.HeldForReincarnate)
	require.Len(t, resp.Warnings, 1, "%v", resp.Warnings)
	assert.Contains(t, resp.Warnings[0], "config.system_prompt: stored now; rendered at the next reincarnation")
	// Storage is unchanged: the prompt is written through to the inline
	// config.
	assert.Equal(t, "be brief", resp.AppliedConfig.InlineConfig.SystemPrompt)

	resp, code, body = patchAgentBody(t, srv, agent.ID, map[string]interface{}{
		"config": map[string]interface{}{"model": "new-model"},
	})
	require.Equal(t, http.StatusOK, code, body)
	assert.Equal(t, []string{"config.model"}, resp.Disposition.Applied)
	require.NotNil(t, resp.Disposition.HeldForReincarnate)
	assert.Empty(t, resp.Disposition.HeldForReincarnate)
	assert.Empty(t, resp.Warnings)

	// An unchanged echo is not an edit: echoing the current prompt and
	// model lists nothing and warns about nothing; changing the model as
	// well lists only the model.
	resp, code, body = patchAgentBody(t, srv, agent.ID, map[string]interface{}{
		"config": map[string]interface{}{"system_prompt": "be brief", "model": "new-model"},
	})
	require.Equal(t, http.StatusOK, code, body)
	for _, k := range []string{"applied", "held", "heldForReincarnate"} {
		assert.Equal(t, "[]", string(resp.RawDisposition[k]), "disposition.%s is always an empty list when empty", k)
	}
	assert.Empty(t, resp.Warnings)

	resp, code, body = patchAgentBody(t, srv, agent.ID, map[string]interface{}{
		"config": map[string]interface{}{"system_prompt": "be brief", "model": "newer-model"},
	})
	require.Equal(t, http.StatusOK, code, body)
	assert.Equal(t, []string{"config.model"}, resp.Disposition.Applied)
	assert.Empty(t, resp.Disposition.Held)
	assert.Empty(t, resp.Disposition.HeldForReincarnate)
	assert.Empty(t, resp.Warnings)
}

// TestAgentConfigPatch_MetadataOnlyDisposition: a PATCH with no config
// reports the metadata keys it wrote, in any phase.
func TestAgentConfigPatch_MetadataOnlyDisposition(t *testing.T) {
	disp := &inlineCaptureDispatcher{reincarnateTestDispatcher: newReincarnateTestDispatcher()}
	srv, s, project, broker := setupReincarnateTestServer(t, disp)
	agent := newEditTestAgent(t, s, project, broker, state.PhaseRunning)

	resp, code, body := patchAgentBody(t, srv, agent.ID, map[string]interface{}{
		"labels": map[string]string{"team": "infra"},
	})
	require.Equal(t, http.StatusOK, code, body)
	assert.Equal(t, []string{"labels"}, resp.Disposition.Applied)
	assert.Empty(t, resp.Disposition.HeldForReincarnate)

	// The same labels again change nothing.
	resp, code, body = patchAgentBody(t, srv, agent.ID, map[string]interface{}{
		"labels": map[string]string{"team": "infra"},
	})
	require.Equal(t, http.StatusOK, code, body)
	assert.Empty(t, resp.Disposition.Applied)
}

func getAgentEditability(t *testing.T, srv *Server, identity Identity, agentID string) *AgentEditability {
	t.Helper()
	var code int
	var raw []byte
	if identity == nil {
		r := doRequest(t, srv, http.MethodGet, "/api/v1/agents/"+agentID, nil)
		code, raw = r.Code, r.Body.Bytes()
	} else {
		r := doRequestAsIdentity(t, srv, identity, http.MethodGet, "/api/v1/agents/"+agentID, nil)
		code, raw = r.Code, r.Body.Bytes()
	}
	require.Equal(t, http.StatusOK, code, string(raw))
	var got AgentWithCapabilities
	require.NoError(t, json.Unmarshal(raw, &got))
	require.NotNil(t, got.Editability, "GET /api/v1/agents/{id} carries editability")
	return got.Editability
}

// TestGetAgent_Editability: the single-agent GET reports each field's tier
// and disposition for the caller.
func TestGetAgent_Editability(t *testing.T) {
	disp := &inlineCaptureDispatcher{reincarnateTestDispatcher: newReincarnateTestDispatcher()}
	srv, s, project, broker := setupReincarnateTestServer(t, disp)

	t.Run("admin on a stopped agent", func(t *testing.T) {
		agent := newEditTestAgent(t, s, project, broker, state.PhaseStopped)
		ed := getAgentEditability(t, srv, nil, agent.ID)
		assert.Equal(t, "stopped", ed.Phase)
		assert.Equal(t, FieldEditState{Tier: EditTierContainer, Disposition: EditNow, ClearNeedsReincarnate: true}, ed.Fields["config.max_turns"])
		assert.Equal(t, EditReincarnate, ed.Fields["agentRole"].Disposition)
		assert.Equal(t, EditTierPrincipal, ed.Fields["agentRole"].Tier)
		assert.Equal(t, EditReincarnate, ed.Fields["config.system_prompt"].Disposition)
		assert.Equal(t, EditLocked, ed.Fields["template"].Disposition)
	})

	t.Run("running agent locks config edits", func(t *testing.T) {
		agent := newEditTestAgent(t, s, project, broker, state.PhaseRunning)
		ed := getAgentEditability(t, srv, nil, agent.ID)
		assert.Equal(t, EditLocked, ed.Fields["config.model"].Disposition)
		assert.Equal(t, editReasonRunning, ed.Fields["config.model"].Reason)
	})
}

// TestGetAgent_EditabilityLocksRoleForCallerWhoCannotDelegate: a user who
// may read, update and run the lifecycle of an agent, but holds no
// authority to delegate its role, sees agentRole locked; config stays
// editable for them.
func TestGetAgent_EditabilityLocksRoleForCallerWhoCannotDelegate(t *testing.T) {
	disp := &inlineCaptureDispatcher{reincarnateTestDispatcher: newReincarnateTestDispatcher()}
	srv, s, project, broker := setupReincarnateTestServer(t, disp)
	agent := newEditTestAgent(t, s, project, broker, state.PhaseStopped)

	user := newReincarnateAuthzUser(t, s, "edit-no-delegate")
	grantPermissionViaRoleBinding(t, s, user.ID, "agent.read", store.RoleScopeProject, project.ID)
	grantPermissionViaRoleBinding(t, s, user.ID, "agent.update", store.RoleScopeProject, project.ID)
	grantAgentLifecycleAtProject(t, s, user.ID, project.ID)
	identity := NewAuthenticatedUser(user.ID, user.Email, user.DisplayName, user.Role, "session")

	ed := getAgentEditability(t, srv, identity, agent.ID)
	assert.Equal(t, EditLocked, ed.Fields["agentRole"].Disposition)
	assert.Equal(t, editReasonCannotDelegate, ed.Fields["agentRole"].Reason)
	assert.Equal(t, EditNow, ed.Fields["config.max_turns"].Disposition)

	// With authority to delegate the role, the same user may change it by
	// reincarnating the agent.
	grantAgentDelegationAtProject(t, s, user.ID, project.ID)
	ed = getAgentEditability(t, srv, identity, agent.ID)
	assert.Equal(t, EditReincarnate, ed.Fields["agentRole"].Disposition)
}

// patchErrorBody decodes an error response's code and details.fields.
func patchErrorBody(t *testing.T, body string) (string, map[string]interface{}) {
	t.Helper()
	var resp struct {
		Error struct {
			Code    string                 `json:"code"`
			Details map[string]interface{} `json:"details"`
		} `json:"error"`
	}
	require.NoError(t, json.Unmarshal([]byte(body), &resp))
	fields, _ := resp.Error.Details["fields"].(map[string]interface{})
	return resp.Error.Code, fields
}

// TestAgentConfigPatch_FixedKeysRefused: a PATCH that would change a key
// the table fixes is refused with 400, naming every such key, and writes
// nothing, in every phase a config PATCH is otherwise accepted. Echoing a
// fixed key's stored value, or sending it empty where none is stored, is
// ignored.
func TestAgentConfigPatch_FixedKeysRefused(t *testing.T) {
	for _, phase := range []state.Phase{state.PhaseCreated, state.PhaseStopped, state.PhaseError, state.PhaseSuspended} {
		t.Run(string(phase), func(t *testing.T) {
			disp := &inlineCaptureDispatcher{reincarnateTestDispatcher: newReincarnateTestDispatcher()}
			srv, s, project, broker := setupReincarnateTestServer(t, disp)
			agent := newEditTestAgent(t, s, project, broker, phase)

			_, code, body := patchAgentBody(t, srv, agent.ID, map[string]interface{}{
				"name": "Renamed",
				"config": map[string]interface{}{
					"max_turns": 8, "branch": "other", "harness": "generic", "harness_config": "hc",
					"default_harness_config": "d", "clone_depth": "full", "config_dir": "/x", "detached": false,
					"hub": map[string]string{"endpoint": "https://x"}, "explicit_workspace": true,
					"empty_per_agent_workspace": true,
				},
			})
			require.Equal(t, http.StatusBadRequest, code, body)
			errCode, fields := patchErrorBody(t, body)
			assert.Equal(t, "validation_error", errCode)
			assert.Equal(t, []string{
				"config.branch", "config.clone_depth", "config.config_dir", "config.default_harness_config",
				"config.detached", "config.empty_per_agent_workspace", "config.explicit_workspace",
				"config.harness", "config.harness_config", "config.hub",
			}, sortedFieldKeys(fields))
			assert.Equal(t, editReasonFixedPatch, fields["config.harness"])

			after, err := s.GetAgent(context.Background(), agent.ID)
			require.NoError(t, err)
			assert.Equal(t, agent.StateVersion, after.StateVersion, "nothing is written")
			assert.Equal(t, agent.Name, after.Name)
			assert.Equal(t, 3, after.AppliedConfig.InlineConfig.MaxTurns)
			assert.Equal(t, "claude", after.AppliedConfig.InlineConfig.Harness)
			assert.Empty(t, after.AppliedConfig.InlineConfig.Branch)

			// The stored harness, and empty values where nothing is stored,
			// are not changes.
			resp, code, body := patchAgentBody(t, srv, agent.ID, map[string]interface{}{
				"config": map[string]interface{}{"max_turns": 8, "harness": "claude", "branch": "", "config_dir": "", "detached": nil},
			})
			require.Equal(t, http.StatusOK, code, body)
			assert.Equal(t, []string{"config.max_turns"}, resp.Disposition.Applied, "echoed fixed keys are ignored, not applied")
			assert.Empty(t, resp.Warnings, "an echo changes nothing to warn about")
			assert.Equal(t, 8, resp.AppliedConfig.InlineConfig.MaxTurns)
		})
	}
}

// TestAgentConfigPatch_TaskRefusedWhileSuspended: a resume does not send
// the task again, so a suspended agent's task is locked and a PATCH of it
// is refused with 409, nothing written.
func TestAgentConfigPatch_TaskRefusedWhileSuspended(t *testing.T) {
	disp := &inlineCaptureDispatcher{reincarnateTestDispatcher: newReincarnateTestDispatcher()}
	srv, s, project, broker := setupReincarnateTestServer(t, disp)
	agent := newEditTestAgent(t, s, project, broker, state.PhaseSuspended)

	_, code, body := patchAgentBody(t, srv, agent.ID, map[string]interface{}{
		"config": map[string]interface{}{"task": "next task", "max_turns": 8},
	})
	require.Equal(t, http.StatusConflict, code, body)
	_, fields := patchErrorBody(t, body)
	assert.Equal(t, map[string]interface{}{"config.task": editReasonNotOnResume}, fields)
	after, err := s.GetAgent(context.Background(), agent.ID)
	require.NoError(t, err)
	assert.Equal(t, agent.StateVersion, after.StateVersion)
	assert.Empty(t, after.AppliedConfig.Task)

	// A stopped agent's next fresh start sends the task, so it is accepted.
	stopped := newReincarnateTestAgent(t, s, project, broker, func(a *store.Agent) {
		a.ID = tid("stopped-" + t.Name())
		a.Slug = "stopped-" + tidSlugSafe(t.Name())
		a.Phase = string(state.PhaseStopped)
	})
	_, code, body = patchAgentBody(t, srv, stopped.ID, map[string]interface{}{
		"config": map[string]interface{}{"task": "next task"},
	})
	require.Equal(t, http.StatusOK, code, body)
}

// TestAgentPatch_DeletedAgentRefused: a soft-deleted agent cannot be
// edited at all, metadata included.
func TestAgentPatch_DeletedAgentRefused(t *testing.T) {
	disp := &inlineCaptureDispatcher{reincarnateTestDispatcher: newReincarnateTestDispatcher()}
	srv, s, project, broker := setupReincarnateTestServer(t, disp)
	agent := newReincarnateTestAgent(t, s, project, broker, func(a *store.Agent) {
		a.Phase = string(state.PhaseStopped)
		a.DeletedAt = time.Now()
	})

	_, code, body := patchAgentBody(t, srv, agent.ID, map[string]interface{}{
		"name":   "Renamed",
		"labels": map[string]string{"a": "b"},
	})
	require.Equal(t, http.StatusConflict, code, body)
	_, fields := patchErrorBody(t, body)
	assert.Equal(t, map[string]interface{}{"name": editReasonDeleted, "labels": editReasonDeleted}, fields)
	after, err := s.GetAgent(context.Background(), agent.ID)
	require.NoError(t, err)
	assert.Equal(t, agent.Name, after.Name)
}

// TestAgentConfigPatch_RemovedEnvKeysWarning: a config.env that drops keys
// of the agent's inline env warns that the removal applies only at the next
// reincarnation, naming the keys; auto-expose keys, which an absent key
// leaves untouched, are not named.
func TestAgentConfigPatch_RemovedEnvKeysWarning(t *testing.T) {
	disp := &inlineCaptureDispatcher{reincarnateTestDispatcher: newReincarnateTestDispatcher()}
	srv, s, project, broker := setupReincarnateTestServer(t, disp)
	agent := newReincarnateTestAgent(t, s, project, broker, func(a *store.Agent) {
		a.Phase = string(state.PhaseStopped)
		a.AppliedConfig.InlineConfig = &api.ScionConfig{Harness: "claude", Env: map[string]string{
			"KEEP": "1", "DROP_B": "2", "DROP_A": "3", "SCION_AUTO_EXPOSE_PORTS": "true",
		}}
	})

	resp, code, body := patchAgentBody(t, srv, agent.ID, map[string]interface{}{
		"config": map[string]interface{}{"env": map[string]string{"KEEP": "1", "NEW": "4"}},
	})
	require.Equal(t, http.StatusOK, code, body)
	require.Len(t, resp.Warnings, 1, "%v", resp.Warnings)
	assert.Contains(t, resp.Warnings[0], "config.env: removed DROP_A, DROP_B now; the agent keeps those variables until the next reincarnation")

	resp, code, body = patchAgentBody(t, srv, agent.ID, map[string]interface{}{
		"config": map[string]interface{}{"max_turns": 2},
	})
	require.Equal(t, http.StatusOK, code, body)
	assert.Empty(t, resp.Warnings, "no env in the request, no env warning")
}

// TestAgentEditAccess_RoleNeedsCeiling: changing the role needs, besides
// CanDelegate, a credential whose scopes cover the role, as a reincarnation
// does. A requester whose own role does not cover the agent's role sees the
// role locked; the same requester with a covering role does not.
func TestAgentEditAccess_RoleNeedsCeiling(t *testing.T) {
	srv, s, project, broker := setupReincarnateTestServer(t, newReincarnateTestDispatcher())
	agent := newReincarnateTestAgent(t, s, project, broker, func(a *store.Agent) {
		a.Phase = string(state.PhaseStopped)
	})
	srv.authzService.mintDevAuthOverride = false
	caps := &Capabilities{Actions: []string{string(ActionRead), string(ActionUpdate), string(ActionLifecycle)}}

	for _, tc := range []struct {
		role string
		want bool
	}{
		{role: string(AgentRoleNone), want: false},
		{role: string(AgentRoleBaseline), want: true},
	} {
		t.Run(tc.role, func(t *testing.T) {
			requester := newReincarnateTestAgent(t, s, project, broker, func(a *store.Agent) {
				a.ID = tid("requester-" + t.Name())
				a.Slug = "requester-" + tidSlugSafe(t.Name())
				a.AppliedConfig.AgentRole = tc.role
			})
			ident := delegatingRequesterFor(requester.ID, project.ID)
			ctx := contextWithIdentity(context.Background(), ident)
			access := srv.agentEditAccessFor(ctx, ident, agent, caps)
			assert.True(t, access.CanUpdate)
			assert.Equal(t, tc.want, access.CanChangeRole)
		})
	}
}

// TestAgentEditGoldens: the request bodies the web Edit page sends
// (web/src/components/pages/agent-edit.test.ts checks it still emits
// exactly these files) are accepted by the PATCH handler and store what
// they say.
func TestAgentEditGoldens(t *testing.T) {
	for _, tc := range []struct {
		name    string
		applied []string
		// heldForReincarnate: a cleared model or max_turns reaches the
		// agent only at the next reincarnation.
		heldForReincarnate []string
		check              func(t *testing.T, inline *api.ScionConfig)
		warnings           int
		// notWarned are keys no warning may name: Unlimited on Max duration
		// ("0") applies at the next start, so it is not "cleared".
		notWarned []string
	}{
		{name: "untouched", applied: []string{}, check: func(t *testing.T, inline *api.ScionConfig) {
			assert.Equal(t, "old-model", inline.Model)
			assert.Equal(t, 3, inline.MaxTurns)
		}},
		{name: "clear", applied: []string{}, heldForReincarnate: []string{"config.max_turns", "config.model"}, warnings: 1, check: func(t *testing.T, inline *api.ScionConfig) {
			assert.Empty(t, inline.Model)
			assert.Zero(t, inline.MaxTurns)
			assert.Empty(t, inline.MaxDuration)
		}},
		{name: "unlimited", applied: []string{"config.max_duration"}, heldForReincarnate: []string{"config.max_turns"}, warnings: 1, notWarned: []string{"config.max_duration"}, check: func(t *testing.T, inline *api.ScionConfig) {
			assert.Zero(t, inline.MaxTurns)
			assert.Equal(t, "0", inline.MaxDuration)
			assert.Zero(t, inline.ParseMaxDuration(), `"0" is no duration limit`)
		}},
		{name: "typed", applied: []string{"config.max_duration", "config.max_turns", "config.model"}, check: func(t *testing.T, inline *api.ScionConfig) {
			assert.Equal(t, "claude-sonnet", inline.Model)
			assert.Equal(t, 40, inline.MaxTurns)
			assert.Equal(t, "2h", inline.MaxDuration)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			raw, err := os.ReadFile(filepath.Join("testdata", "agent-edit-"+tc.name+"-body.json"))
			require.NoError(t, err)
			var body map[string]interface{}
			require.NoError(t, json.Unmarshal(raw, &body))

			disp := &inlineCaptureDispatcher{reincarnateTestDispatcher: newReincarnateTestDispatcher()}
			srv, s, project, broker := setupReincarnateTestServer(t, disp)
			agent := newEditTestAgent(t, s, project, broker, state.PhaseStopped)
			body["stateVersion"] = agent.StateVersion

			resp, code, respBody := patchAgentBody(t, srv, agent.ID, body)
			require.Equal(t, http.StatusOK, code, respBody)
			assert.Equal(t, tc.applied, resp.Disposition.Applied)
			if tc.heldForReincarnate == nil {
				tc.heldForReincarnate = []string{}
			}
			assert.Equal(t, tc.heldForReincarnate, resp.Disposition.HeldForReincarnate)
			assert.Len(t, resp.Warnings, tc.warnings, "%v", resp.Warnings)
			for _, w := range resp.Warnings {
				for _, k := range tc.notWarned {
					assert.NotContains(t, w, k)
				}
			}
			after, err := s.GetAgent(context.Background(), agent.ID)
			require.NoError(t, err)
			tc.check(t, after.AppliedConfig.InlineConfig)
		})
	}
}

// TestAgentPatch_RefusalPrecedence pins the order in which a PATCH is
// refused (lockedPatchKeys, writePatchRefusal): a deleted agent (409), then
// config in a phase that takes none, whatever the keys (409, the phase
// message), then a fixed key that would change (400), then field
// validation.
func TestAgentPatch_RefusalPrecedence(t *testing.T) {
	for _, tc := range []struct {
		name    string
		phase   state.Phase
		deleted bool
		config  map[string]interface{}
		code    int
		message string
		fields  []string
	}{
		{
			name: "deleted beats phase and fixed", phase: state.PhaseRunning, deleted: true,
			config: map[string]interface{}{"branch": "x", "max_turns": 2},
			code:   http.StatusConflict, message: "The agent is deleted and cannot be edited",
			fields: []string{"config.branch", "config.max_turns"},
		},
		{
			name: "phase beats fixed: changed fixed key only", phase: state.PhaseRunning,
			config: map[string]interface{}{"branch": "x"},
			code:   http.StatusConflict, message: configPatchPhaseMessage, fields: []string{"config.branch"},
		},
		{
			name: "phase beats fixed: echoed fixed key only", phase: state.PhaseStarting,
			config: map[string]interface{}{"harness": "claude"},
			code:   http.StatusConflict, message: configPatchPhaseMessage, fields: []string{"config.harness"},
		},
		{
			name: "fixed beats field validation", phase: state.PhaseStopped,
			config: map[string]interface{}{"harness": "generic", "thinking_level": 500},
			code:   http.StatusBadRequest, message: "Some fields are set when the agent is created and cannot be changed",
			fields: []string{"config.harness"},
		},
		{
			name: "field validation last", phase: state.PhaseStopped,
			config: map[string]interface{}{"thinking_level": 500},
			code:   http.StatusBadRequest, message: "thinking_level must be between 0 and 100",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			disp := &inlineCaptureDispatcher{reincarnateTestDispatcher: newReincarnateTestDispatcher()}
			srv, s, project, broker := setupReincarnateTestServer(t, disp)
			agent := newReincarnateTestAgent(t, s, project, broker, func(a *store.Agent) {
				a.Phase = string(tc.phase)
				if tc.deleted {
					a.DeletedAt = time.Now()
				}
				a.AppliedConfig.InlineConfig = &api.ScionConfig{Harness: "claude", MaxTurns: 3}
			})
			_, code, body := patchAgentBody(t, srv, agent.ID, map[string]interface{}{"config": tc.config})
			require.Equal(t, tc.code, code, body)
			assert.Contains(t, body, tc.message)
			_, fields := patchErrorBody(t, body)
			if tc.fields == nil {
				assert.Empty(t, fields)
			} else {
				assert.Equal(t, tc.fields, sortedFieldKeys(fields))
			}
			after, err := s.GetAgent(context.Background(), agent.ID)
			require.NoError(t, err)
			assert.Equal(t, agent.StateVersion, after.StateVersion, "nothing is written")
		})
	}
}

// TestAgentConfigPatch_FixedKeyEchoOfAppliedValue: a fixed key equal to the
// agent's applied value (what GET shows), not only its inline value, is an
// unchanged echo and is ignored; a different value is refused.
func TestAgentConfigPatch_FixedKeyEchoOfAppliedValue(t *testing.T) {
	disp := &inlineCaptureDispatcher{reincarnateTestDispatcher: newReincarnateTestDispatcher()}
	srv, s, project, broker := setupReincarnateTestServer(t, disp)
	agent := newReincarnateTestAgent(t, s, project, broker, func(a *store.Agent) {
		a.Phase = string(state.PhaseStopped)
		a.AppliedConfig.Branch = "scion/live"
		a.AppliedConfig.HarnessConfig = "hc-live"
		a.AppliedConfig.InlineConfig = &api.ScionConfig{Harness: "claude", MaxTurns: 3}
	})

	resp, code, body := patchAgentBody(t, srv, agent.ID, map[string]interface{}{
		"config": map[string]interface{}{"branch": "scion/live", "harness_config": "hc-live", "max_turns": 4},
	})
	require.Equal(t, http.StatusOK, code, body)
	assert.Equal(t, []string{"config.max_turns"}, resp.Disposition.Applied)

	// An ignored echo writes nothing: not into the inline config, and not
	// into CreateInputs, where a reincarnation would replay it as a pin.
	after, err := s.GetAgent(context.Background(), agent.ID)
	require.NoError(t, err)
	assert.Equal(t, 4, after.AppliedConfig.InlineConfig.MaxTurns)
	assert.Empty(t, after.AppliedConfig.InlineConfig.Branch)
	assert.Empty(t, after.AppliedConfig.InlineConfig.HarnessConfig)
	assert.Equal(t, "scion/live", after.AppliedConfig.Branch)
	assert.Equal(t, "hc-live", after.AppliedConfig.HarnessConfig)
	if ci := after.AppliedConfig.CreateInputs; ci != nil && ci.InlineConfig != nil {
		assert.Empty(t, ci.InlineConfig.Branch, "an echo is not an explicit edit")
		assert.Empty(t, ci.InlineConfig.HarnessConfig)
	}

	_, code, body = patchAgentBody(t, srv, agent.ID, map[string]interface{}{
		"config": map[string]interface{}{"branch": "scion/other"},
	})
	require.Equal(t, http.StatusBadRequest, code, body)
	_, fields := patchErrorBody(t, body)
	assert.Equal(t, []string{"config.branch"}, sortedFieldKeys(fields))
}

// TestAgentConfigPatch_NoWarningsForUnchangedValues: the Configure page's
// untouched body, sent to an agent whose prompts, limits and user are
// already empty, changes nothing and so warns about nothing; a real clear
// does warn.
func TestAgentConfigPatch_NoWarningsForUnchangedValues(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("testdata", "configure-untouched-body.json"))
	require.NoError(t, err)
	var cfg map[string]interface{}
	require.NoError(t, json.Unmarshal(raw, &cfg))

	disp := &inlineCaptureDispatcher{reincarnateTestDispatcher: newReincarnateTestDispatcher()}
	srv, s, project, broker := setupReincarnateTestServer(t, disp)
	agent := newReincarnateTestAgent(t, s, project, broker, func(a *store.Agent) {
		a.Phase = string(state.PhaseCreated)
		a.AppliedConfig.InlineConfig = &api.ScionConfig{Harness: "claude", Model: "golden-model"}
	})

	resp, code, body := patchAgentBody(t, srv, agent.ID, map[string]interface{}{"config": cfg})
	require.Equal(t, http.StatusOK, code, body)
	assert.Empty(t, resp.Warnings)

	resp, code, body = patchAgentBody(t, srv, agent.ID, map[string]interface{}{
		"config": map[string]interface{}{"model": nil, "system_prompt": "be brief"},
	})
	require.Equal(t, http.StatusOK, code, body)
	require.Len(t, resp.Warnings, 2, "%v", resp.Warnings)
	assert.Contains(t, resp.Warnings[0], "config.system_prompt: stored now")
	assert.Contains(t, resp.Warnings[1], "config.model: cleared now")

	// Resending the stored prompt is no change.
	resp, code, body = patchAgentBody(t, srv, agent.ID, map[string]interface{}{
		"config": map[string]interface{}{"system_prompt": "be brief"},
	})
	require.Equal(t, http.StatusOK, code, body)
	assert.Empty(t, resp.Warnings)
}

// TestAgentConfigPatch_RemovedEntriesWarnings: dropping an MCP server or a
// volume from a non-empty list warns, like an env key, because the start
// merge unites MCP servers and appends volumes; the warning names the
// removed entries (volumes by target).
func TestAgentConfigPatch_RemovedEntriesWarnings(t *testing.T) {
	disp := &inlineCaptureDispatcher{reincarnateTestDispatcher: newReincarnateTestDispatcher()}
	srv, s, project, broker := setupReincarnateTestServer(t, disp)
	agent := newReincarnateTestAgent(t, s, project, broker, func(a *store.Agent) {
		a.Phase = string(state.PhaseStopped)
		a.AppliedConfig.InlineConfig = &api.ScionConfig{
			Harness: "claude",
			MCPServers: map[string]api.MCPServerConfig{
				"docs": {Transport: "stdio", Command: "docs"}, "search": {Transport: "stdio", Command: "search"},
			},
			Volumes: []api.VolumeMount{{Source: "/h/a", Target: "/a"}, {Source: "/h/b", Target: "/b"}},
		}
	})

	resp, code, body := patchAgentBody(t, srv, agent.ID, map[string]interface{}{
		"config": map[string]interface{}{
			"mcp_servers": map[string]interface{}{"docs": map[string]string{"transport": "stdio", "command": "docs"}},
			"volumes":     []map[string]string{{"source": "/h/a", "target": "/a"}},
		},
	})
	require.Equal(t, http.StatusOK, code, body)
	require.Len(t, resp.Warnings, 2, "%v", resp.Warnings)
	assert.Contains(t, resp.Warnings[0], "config.mcp_servers: removed search now; the agent keeps those MCP servers until the next reincarnation")
	assert.Contains(t, resp.Warnings[1], "config.volumes: removed /b now; the agent keeps those volumes until the next reincarnation")
}
