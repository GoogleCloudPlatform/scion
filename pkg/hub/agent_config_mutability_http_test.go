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
	"sync"
	"testing"

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
// edits take effect at the next reincarnation, and still reports them
// applied.
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
	assert.Equal(t, []string{"annotations", "config.max_duration", "config.max_turns", "config.system_prompt", "name"}, resp.Disposition.Applied)
	require.Len(t, resp.Warnings, 2, "%v", resp.Warnings)
	assert.Contains(t, resp.Warnings[0], "config.system_prompt: stored now; rendered at the next reincarnation")
	assert.Contains(t, resp.Warnings[1], "config.max_duration, config.max_turns: cleared now")
	assert.Equal(t, "be brief", resp.AppliedConfig.InlineConfig.SystemPrompt)
	assert.Equal(t, 0, resp.AppliedConfig.InlineConfig.MaxTurns)
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
