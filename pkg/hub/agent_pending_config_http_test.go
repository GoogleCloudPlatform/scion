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
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/experiments"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Tests for ptone/scion#3976: with hub.agent_edit_held on, config edits of
// an agent with a live or transitional container are held in its pending
// config and applied at its next start; provision-rendered keys stay
// pending until a reincarnation; pending edits can be discarded.

// setHeldEditsExperiment turns hub.agent_edit_held on or off on srv by
// replacing its registry; every other experiment keeps its default.
func setHeldEditsExperiment(t *testing.T, srv *Server, on bool) {
	t.Helper()
	var active []experiments.Experiment
	for _, e := range experiments.Default().All() {
		if e.Name == experiments.AgentEditHeld {
			e.Default = on
		}
		active = append(active, e)
	}
	reg, err := experiments.NewRegistry(active, nil)
	require.NoError(t, err)
	srv.experiments = reg
	require.Equal(t, on, srv.heldEditsEnabled())
}

// heldEditsFixture is a reincarnate-style test server with the experiment
// on, and one agent in phase.
func heldEditsFixture(t *testing.T, phase state.Phase) (*Server, store.Store, *inlineCaptureDispatcher, *store.Agent) {
	t.Helper()
	disp := &inlineCaptureDispatcher{reincarnateTestDispatcher: newReincarnateTestDispatcher()}
	srv, s, project, broker := setupReincarnateTestServer(t, disp)
	setHeldEditsExperiment(t, srv, true)
	agent := newEditTestAgent(t, s, project, broker, phase)
	return srv, s, disp, agent
}

// pendingPatchResponse decodes the PATCH response with its pending config.
type pendingPatchResponse struct {
	StateVersion  int64                     `json:"stateVersion"`
	AppliedConfig *store.AgentAppliedConfig `json:"appliedConfig"`
	PendingConfig *store.AgentPendingConfig `json:"pendingConfig"`
	Warnings      []string                  `json:"warnings"`
	Disposition   AgentUpdateDisposition    `json:"disposition"`
}

func patchPending(t *testing.T, srv *Server, agentID string, body map[string]interface{}) pendingPatchResponse {
	t.Helper()
	rec := doRequest(t, srv, http.MethodPatch, "/api/v1/agents/"+agentID, body)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var resp pendingPatchResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	return resp
}

// pendingLifecycleWarnings posts a lifecycle action and returns its warnings.
func pendingLifecycleWarnings(t *testing.T, srv *Server, agentID, action string) []string {
	t.Helper()
	rec := doRequest(t, srv, http.MethodPost, "/api/v1/agents/"+agentID+"/"+action, nil)
	require.Less(t, rec.Code, 300, "%s: %s", action, rec.Body.String())
	var resp struct {
		Warnings []string `json:"warnings"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	return resp.Warnings
}

func pendingReloadAgent(t *testing.T, s store.Store, id string) *store.Agent {
	t.Helper()
	a, err := s.GetAgent(context.Background(), id)
	require.NoError(t, err)
	return a
}

// pendingAudits returns the agent's audit records of mutationType, waiting
// up to a few seconds for the asynchronous writes to reach want records.
func pendingAudits(t *testing.T, s store.Store, agentID, mutationType string, want int) []*store.MutationAuditRecord {
	t.Helper()
	var got []*store.MutationAuditRecord
	require.Eventually(t, func() bool {
		recs, _, err := s.ListMutationAudits(context.Background(), store.MutationAuditFilter{
			MutationType: mutationType, TargetID: agentID, Limit: 20,
		})
		if err != nil {
			return false
		}
		got = recs
		return len(recs) >= want
	}, 5*time.Second, 20*time.Millisecond, "want %d %s audit record(s)", want, mutationType)
	return got
}

// pendingAuditSummary decodes a pending config audit record's after summary.
func pendingAuditSummary(t *testing.T, rec *store.MutationAuditRecord) (keys []string, trigger string) {
	t.Helper()
	var sum struct {
		Keys    []string `json:"keys"`
		Trigger string   `json:"trigger"`
	}
	require.NoError(t, json.Unmarshal([]byte(rec.AfterSummary), &sum))
	return sum.Keys, sum.Trigger
}

// TestAgentPendingConfig_RunningModelHeldThenAppliedAtStart: a model edit
// of a running agent is held, leaves the applied model unchanged, and the
// next start after a stop launches with it and clears the pending config.
func TestAgentPendingConfig_RunningModelHeldThenAppliedAtStart(t *testing.T) {
	srv, s, disp, agent := heldEditsFixture(t, state.PhaseRunning)

	resp := patchPending(t, srv, agent.ID, map[string]interface{}{
		"config":       map[string]interface{}{"model": "new-model"},
		"stateVersion": agent.StateVersion,
	})
	assert.Equal(t, []string{"config.model"}, resp.Disposition.Held)
	assert.Empty(t, resp.Disposition.Applied)
	assert.Empty(t, resp.Disposition.HeldForReincarnate)
	assert.Equal(t, "old-model", resp.AppliedConfig.Model, "the applied model is unchanged")
	assert.Equal(t, "old-model", resp.AppliedConfig.InlineConfig.Model)
	require.NotNil(t, resp.PendingConfig)
	assert.JSONEq(t, `"new-model"`, string(resp.PendingConfig.Config["model"]))
	assert.Greater(t, resp.StateVersion, agent.StateVersion, "a held save bumps the state version")

	stored := pendingReloadAgent(t, s, agent.ID)
	assert.Equal(t, "old-model", stored.AppliedConfig.Model)
	require.NotNil(t, stored.PendingConfig)
	assert.True(t, strings.HasPrefix(stored.PendingConfig.SavedBy, "user:"), stored.PendingConfig.SavedBy)
	assert.Equal(t, "Development User", stored.PendingConfig.SavedByName)
	assert.Equal(t, agent.StateVersion, stored.PendingConfig.BaseStateVersion)
	assert.Equal(t, agent.Generation, stored.PendingConfig.BaseGeneration)
	assert.False(t, stored.PendingConfig.SavedAt.IsZero())

	pendingLifecycleWarnings(t, srv, agent.ID, "stop")
	pendingLifecycleWarnings(t, srv, agent.ID, "start")

	starts := disp.snapshot()
	require.Len(t, starts, 1)
	assert.Equal(t, "new-model", starts[0].model, "the start launches with the held model")
	assert.Equal(t, "new-model", starts[0].inline.Model)
	assert.Equal(t, 3, starts[0].inline.MaxTurns, "keys the edit did not name are kept")

	after := pendingReloadAgent(t, s, agent.ID)
	assert.Nil(t, after.PendingConfig, "the pending config is cleared once applied")
	assert.Equal(t, "new-model", after.AppliedConfig.Model)

	saved := pendingAudits(t, s, agent.ID, mutationTypeAgentPendingConfigSaved, 1)
	keys, _ := pendingAuditSummary(t, saved[0])
	assert.Equal(t, []string{"config.model"}, keys)
	applied := pendingAudits(t, s, agent.ID, mutationTypeAgentPendingConfigApplied, 1)
	keys, trigger := pendingAuditSummary(t, applied[0])
	assert.Equal(t, []string{"config.model"}, keys)
	assert.Equal(t, pendingFoldTriggerStart, trigger)
}

// TestAgentPendingConfig_SuspendThenWakeAppliesHeldEdit: a held edit is
// applied by the resume a message-triggered wake starts.
func TestAgentPendingConfig_SuspendThenWakeAppliesHeldEdit(t *testing.T) {
	srv, s, disp, agent := heldEditsFixture(t, state.PhaseRunning)

	resp := patchPending(t, srv, agent.ID, map[string]interface{}{
		"config": map[string]interface{}{"model": "woken-model", "max_turns": 12},
	})
	assert.Equal(t, []string{"config.max_turns", "config.model"}, resp.Disposition.Held)

	pendingLifecycleWarnings(t, srv, agent.ID, "suspend")
	suspended := pendingReloadAgent(t, s, agent.ID)
	require.Equal(t, string(state.PhaseSuspended), suspended.Phase)
	require.NotNil(t, suspended.PendingConfig, "a suspend keeps the pending config")

	// The resumed container reports in, which ends the wake's readiness
	// wait.
	go func() {
		time.Sleep(200 * time.Millisecond)
		_ = s.UpdateAgentStatus(context.Background(), agent.ID, store.AgentStatusUpdate{
			Phase: string(state.PhaseRunning), Activity: "idle",
		})
	}()
	result, dmErr := srv.wakeAgentForDM(context.Background(), suspended)
	require.Nil(t, dmErr)
	require.NotNil(t, result)
	assert.Equal(t, WakeResumed, result.Outcome)

	starts := disp.snapshot()
	require.Len(t, starts, 1)
	assert.True(t, starts[0].resume)
	assert.Equal(t, "woken-model", starts[0].model)
	assert.Equal(t, 12, starts[0].inline.MaxTurns)
	assert.Nil(t, pendingReloadAgent(t, s, agent.ID).PendingConfig)

	applied := pendingAudits(t, s, agent.ID, mutationTypeAgentPendingConfigApplied, 1)
	_, trigger := pendingAuditSummary(t, applied[0])
	assert.Equal(t, pendingFoldTriggerWake, trigger)
}

// gatedCaptureDispatcher blocks DispatchAgentStart until released, so a
// test can act while a start holds its claim.
type gatedCaptureDispatcher struct {
	*inlineCaptureDispatcher
	entered chan struct{}
	release chan struct{}
}

func (d *gatedCaptureDispatcher) DispatchAgentStart(ctx context.Context, agent *store.Agent, task string, resume bool) error {
	select {
	case d.entered <- struct{}{}:
	default:
	}
	<-d.release
	return d.inlineCaptureDispatcher.DispatchAgentStart(ctx, agent, task, resume)
}

// TestAgentPendingConfig_SaveDuringStartIsHeld: a config save that arrives
// while a start holds its claim (the agent reads starting) is held, never
// written through: the start in flight dispatches the config it began
// with, and the next start dispatches the saved edit.
func TestAgentPendingConfig_SaveDuringStartIsHeld(t *testing.T) {
	inner := &inlineCaptureDispatcher{reincarnateTestDispatcher: newReincarnateTestDispatcher()}
	disp := &gatedCaptureDispatcher{inlineCaptureDispatcher: inner, entered: make(chan struct{}, 1), release: make(chan struct{})}
	srv, s, project, broker := setupReincarnateTestServer(t, disp)
	setHeldEditsExperiment(t, srv, true)
	agent := newEditTestAgent(t, s, project, broker, state.PhaseStopped)

	startDone := make(chan int, 1)
	go func() {
		rec := doRequest(t, srv, http.MethodPost, "/api/v1/agents/"+agent.ID+"/start", nil)
		startDone <- rec.Code
	}()
	select {
	case <-disp.entered:
	case <-time.After(10 * time.Second):
		t.Fatal("the start never reached its dispatch")
	}

	during := pendingReloadAgent(t, s, agent.ID)
	require.Equal(t, string(state.PhaseStarting), during.Phase, "the start holds its claim with the agent starting")
	require.NotEmpty(t, during.StartClaimID)
	resp := patchPending(t, srv, agent.ID, map[string]interface{}{
		"config": map[string]interface{}{"model": "mid-start-model"},
	})
	assert.Equal(t, []string{"config.model"}, resp.Disposition.Held)
	assert.Empty(t, resp.Disposition.Applied)
	assert.Equal(t, "old-model", resp.AppliedConfig.Model, "never written through")

	close(disp.release)
	select {
	case code := <-startDone:
		require.Less(t, code, 300)
	case <-time.After(10 * time.Second):
		t.Fatal("the start did not finish")
	}

	starts := inner.snapshot()
	require.Len(t, starts, 1)
	assert.Equal(t, "old-model", starts[0].model, "the start in flight sends the config it began with")
	held := pendingReloadAgent(t, s, agent.ID)
	require.NotNil(t, held.PendingConfig)
	assert.JSONEq(t, `"mid-start-model"`, string(held.PendingConfig.Config["model"]))
	assert.Equal(t, "old-model", held.AppliedConfig.Model)

	pendingLifecycleWarnings(t, srv, agent.ID, "stop")
	pendingLifecycleWarnings(t, srv, agent.ID, "start")
	starts = inner.snapshot()
	require.Len(t, starts, 2)
	assert.Equal(t, "mid-start-model", starts[1].model)
	assert.Nil(t, pendingReloadAgent(t, s, agent.ID).PendingConfig)
}

// TestAgentPendingConfig_SystemPromptHeldForReincarnate: a system prompt
// edit of a stopped agent is held for a reincarnation; a start leaves it
// pending and warns, in the response and the agent's status message.
func TestAgentPendingConfig_SystemPromptHeldForReincarnate(t *testing.T) {
	srv, s, disp, agent := heldEditsFixture(t, state.PhaseStopped)

	resp := patchPending(t, srv, agent.ID, map[string]interface{}{
		"config": map[string]interface{}{"system_prompt": "be brief"},
	})
	assert.Equal(t, []string{"config.system_prompt"}, resp.Disposition.HeldForReincarnate)
	assert.Empty(t, resp.Disposition.Applied)
	assert.Empty(t, resp.Disposition.Held)
	assert.Empty(t, resp.AppliedConfig.InlineConfig.SystemPrompt, "not written to the applied config")
	require.NotNil(t, resp.PendingConfig)
	assert.JSONEq(t, `"be brief"`, string(resp.PendingConfig.Config["system_prompt"]))
	require.Len(t, resp.Warnings, 1, "%v", resp.Warnings)
	assert.Contains(t, resp.Warnings[0], "config.system_prompt: held until the next reincarnation")

	warnings := pendingLifecycleWarnings(t, srv, agent.ID, "start")
	assert.Contains(t, warnings, "Held for reincarnation: system prompt")

	starts := disp.snapshot()
	require.Len(t, starts, 1)
	assert.Empty(t, starts[0].inline.SystemPrompt, "a start does not apply a provision-rendered key")
	after := pendingReloadAgent(t, s, agent.ID)
	require.NotNil(t, after.PendingConfig, "the system prompt stays pending")
	assert.Equal(t, []string{"config.system_prompt"}, pendingConfigKeys(after.PendingConfig))
	assert.Equal(t, "Held for reincarnation: system prompt", after.Message)

	recs, _, err := s.ListMutationAudits(context.Background(), store.MutationAuditFilter{
		MutationType: mutationTypeAgentPendingConfigApplied, TargetID: agent.ID, Limit: 5,
	})
	require.NoError(t, err)
	assert.Empty(t, recs, "nothing was applied")
}

// TestAgentPendingConfig_NowPhasePatchFoldsPendingFirst: a config PATCH of
// an agent with no container first applies its pending container keys,
// then the request (whose keys win), so only provision-rendered keys stay
// pending.
func TestAgentPendingConfig_NowPhasePatchFoldsPendingFirst(t *testing.T) {
	srv, s, _, agent := heldEditsFixture(t, state.PhaseRunning)

	resp := patchPending(t, srv, agent.ID, map[string]interface{}{
		"config": map[string]interface{}{"model": "held-model", "max_duration": "2h", "system_prompt": "be brief"},
	})
	assert.Equal(t, []string{"config.max_duration", "config.model"}, resp.Disposition.Held)
	assert.Equal(t, []string{"config.system_prompt"}, resp.Disposition.HeldForReincarnate)

	pendingLifecycleWarnings(t, srv, agent.ID, "stop")
	stopped := pendingReloadAgent(t, s, agent.ID)
	require.Equal(t, string(state.PhaseStopped), stopped.Phase)
	require.Len(t, stopped.PendingConfig.Config, 3, "a stop does not apply pending edits")

	resp = patchPending(t, srv, agent.ID, map[string]interface{}{
		"config": map[string]interface{}{"max_turns": 9, "model": "request-model"},
	})
	assert.Equal(t, []string{"config.max_turns", "config.model"}, resp.Disposition.Applied)
	assert.Empty(t, resp.Disposition.Held)
	assert.Equal(t, "request-model", resp.AppliedConfig.Model, "the request's key wins over the pending one")
	assert.Equal(t, 9, resp.AppliedConfig.InlineConfig.MaxTurns)
	assert.Equal(t, "2h", resp.AppliedConfig.InlineConfig.MaxDuration, "the pending key was applied first")
	require.NotNil(t, resp.PendingConfig)
	assert.Equal(t, []string{"config.system_prompt"}, pendingConfigKeys(resp.PendingConfig), "only the provision-rendered key stays pending")

	applied := pendingAudits(t, s, agent.ID, mutationTypeAgentPendingConfigApplied, 1)
	keys, trigger := pendingAuditSummary(t, applied[0])
	assert.Equal(t, []string{"config.max_duration", "config.model"}, keys)
	assert.Equal(t, pendingFoldTriggerUpdate, trigger)
}

// TestAgentPendingConfig_RestartAppliesHeldEdit: a restart's start leg
// applies held edits too.
func TestAgentPendingConfig_RestartAppliesHeldEdit(t *testing.T) {
	srv, s, disp, agent := heldEditsFixture(t, state.PhaseRunning)
	patchPending(t, srv, agent.ID, map[string]interface{}{
		"config": map[string]interface{}{"max_turns": 21},
	})
	pendingLifecycleWarnings(t, srv, agent.ID, "restart")
	starts := disp.snapshot()
	require.Len(t, starts, 1)
	assert.Equal(t, 21, starts[0].inline.MaxTurns)
	assert.Nil(t, pendingReloadAgent(t, s, agent.ID).PendingConfig)
	applied := pendingAudits(t, s, agent.ID, mutationTypeAgentPendingConfigApplied, 1)
	_, trigger := pendingAuditSummary(t, applied[0])
	assert.Equal(t, pendingFoldTriggerRestart, trigger)
}

// TestAgentPendingConfig_ReconcileRestartKeepsPending: a reconcile-driven
// restart (the broker's /restart, which carries no inline config) neither
// applies nor clears pending edits.
func TestAgentPendingConfig_ReconcileRestartKeepsPending(t *testing.T) {
	srv, s, disp, agent := heldEditsFixture(t, state.PhaseRunning)
	patchPending(t, srv, agent.ID, map[string]interface{}{
		"config": map[string]interface{}{"model": "held-model"},
	})
	before := pendingReloadAgent(t, s, agent.ID)

	_, err := srv.execDispatchRestart(context.Background(), store.BrokerDispatch{
		AgentID: agent.ID, BrokerID: agent.RuntimeBrokerID, Op: "restart",
	})
	require.NoError(t, err)

	after := pendingReloadAgent(t, s, agent.ID)
	require.NotNil(t, after.PendingConfig, "the reconcile restart keeps the pending config")
	assert.JSONEq(t, `"held-model"`, string(after.PendingConfig.Config["model"]))
	assert.Equal(t, "old-model", after.AppliedConfig.Model)
	assert.Equal(t, before.StateVersion, after.StateVersion)
	assert.Empty(t, disp.snapshot(), "no start was dispatched")
}

// TestAgentPendingConfig_DiscardClearsAndAudits: DELETE pending-config
// clears the pending config, bumps the state version and audits; a second
// discard finds nothing; a stale stateVersion is refused.
func TestAgentPendingConfig_DiscardClearsAndAudits(t *testing.T) {
	srv, s, _, agent := heldEditsFixture(t, state.PhaseRunning)
	resp := patchPending(t, srv, agent.ID, map[string]interface{}{
		"config": map[string]interface{}{"model": "held-model", "system_prompt": "be brief"},
	})
	require.NotNil(t, resp.PendingConfig)

	stale := doRequest(t, srv, http.MethodDelete, "/api/v1/agents/"+agent.ID+"/pending-config?stateVersion="+strconv.FormatInt(agent.StateVersion, 10), nil)
	assert.Equal(t, http.StatusConflict, stale.Code, stale.Body.String())

	rec := doRequest(t, srv, http.MethodDelete, "/api/v1/agents/"+agent.ID+"/pending-config?stateVersion="+strconv.FormatInt(resp.StateVersion, 10), nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var out struct {
		StateVersion  int64                     `json:"stateVersion"`
		PendingConfig *store.AgentPendingConfig `json:"pendingConfig"`
		Discarded     []string                  `json:"discarded"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &out))
	assert.Equal(t, []string{"config.model", "config.system_prompt"}, out.Discarded)
	assert.Nil(t, out.PendingConfig)
	assert.Greater(t, out.StateVersion, resp.StateVersion)

	after := pendingReloadAgent(t, s, agent.ID)
	assert.Nil(t, after.PendingConfig)
	assert.Equal(t, "old-model", after.AppliedConfig.Model, "discarding applies nothing")

	discarded := pendingAudits(t, s, agent.ID, mutationTypeAgentPendingConfigDiscarded, 1)
	keys, _ := pendingAuditSummary(t, discarded[0])
	assert.Equal(t, []string{"config.model", "config.system_prompt"}, keys)

	again := doRequest(t, srv, http.MethodDelete, "/api/v1/agents/"+agent.ID+"/pending-config", nil)
	require.Equal(t, http.StatusOK, again.Code, again.Body.String())
	require.NoError(t, json.Unmarshal(again.Body.Bytes(), &out))
	assert.Equal(t, []string{}, out.Discarded)
	assert.Equal(t, after.StateVersion, pendingReloadAgent(t, s, agent.ID).StateVersion, "an empty discard writes nothing")

	wrong := doRequest(t, srv, http.MethodPost, "/api/v1/agents/"+agent.ID+"/pending-config", nil)
	assert.Equal(t, http.StatusMethodNotAllowed, wrong.Code)
}

// TestAgentPendingConfig_FlagOffKeepsTodayBehaviour: with
// hub.agent_edit_held off, a config PATCH of a running agent is refused
// (409), a provision-rendered key of a stopped agent is written through,
// an agentRole key is ignored, the discard route answers 404, and a start
// leaves stored pending edits alone.
func TestAgentPendingConfig_FlagOffKeepsTodayBehaviour(t *testing.T) {
	off := func(t *testing.T, phase state.Phase) (*Server, store.Store, *inlineCaptureDispatcher, *store.Agent) {
		srv, s, disp, agent := heldEditsFixture(t, phase)
		setHeldEditsExperiment(t, srv, false)
		return srv, s, disp, agent
	}

	t.Run("running config edit is refused", func(t *testing.T) {
		srv, s, _, running := off(t, state.PhaseRunning)
		rec := doRequest(t, srv, http.MethodPatch, "/api/v1/agents/"+running.ID, map[string]interface{}{
			"config": map[string]interface{}{"model": "new-model"},
		})
		require.Equal(t, http.StatusConflict, rec.Code, rec.Body.String())
		assert.Contains(t, rec.Body.String(), configPatchPhaseMessage)
		assert.Nil(t, pendingReloadAgent(t, s, running.ID).PendingConfig)
		ed := getAgentEditability(t, srv, nil, running.ID)
		assert.Equal(t, EditLocked, ed.Fields["config.model"].Disposition)
	})

	t.Run("discard route answers 404", func(t *testing.T) {
		srv, _, _, running := off(t, state.PhaseRunning)
		del := doRequest(t, srv, http.MethodDelete, "/api/v1/agents/"+running.ID+"/pending-config", nil)
		assert.Equal(t, http.StatusNotFound, del.Code, del.Body.String())
	})

	t.Run("agentRole is ignored", func(t *testing.T) {
		srv, s, _, running := off(t, state.PhaseRunning)
		rec := doRequest(t, srv, http.MethodPatch, "/api/v1/agents/"+running.ID, map[string]interface{}{
			"agentRole": "readonly", "name": "renamed",
		})
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		assert.Equal(t, "renamed", pendingReloadAgent(t, s, running.ID).Name)
	})

	t.Run("provision-rendered key is written through", func(t *testing.T) {
		srv, _, _, stopped := off(t, state.PhaseStopped)
		resp := patchPending(t, srv, stopped.ID, map[string]interface{}{
			"config": map[string]interface{}{"system_prompt": "be brief"},
		})
		assert.Equal(t, []string{"config.system_prompt"}, resp.Disposition.HeldForReincarnate)
		assert.Equal(t, "be brief", resp.AppliedConfig.InlineConfig.SystemPrompt, "written through, as before")
		assert.Nil(t, resp.PendingConfig)
	})

	t.Run("stored pending config stays inert at start", func(t *testing.T) {
		srv, s, disp, stopped := off(t, state.PhaseStopped)
		cur := pendingReloadAgent(t, s, stopped.ID)
		cur.PendingConfig = &store.AgentPendingConfig{Config: map[string]json.RawMessage{"model": json.RawMessage(`"pending-model"`)}}
		require.NoError(t, s.UpdateAgent(context.Background(), cur))
		pendingLifecycleWarnings(t, srv, stopped.ID, "start")
		starts := disp.snapshot()
		require.Len(t, starts, 1)
		assert.Equal(t, "old-model", starts[0].model)
		assert.NotNil(t, pendingReloadAgent(t, s, stopped.ID).PendingConfig)
	})
}

// TestAgentPendingConfig_AgentRoleRefusedWithHeldEdits: with held edits on,
// a PATCH naming agentRole is refused (409) and writes nothing.
func TestAgentPendingConfig_AgentRoleRefusedWithHeldEdits(t *testing.T) {
	srv, s, _, agent := heldEditsFixture(t, state.PhaseRunning)
	rec := doRequest(t, srv, http.MethodPatch, "/api/v1/agents/"+agent.ID, map[string]interface{}{
		"agentRole": "readonly", "name": "renamed",
	})
	require.Equal(t, http.StatusConflict, rec.Code, rec.Body.String())
	assert.Contains(t, rec.Body.String(), "reincarnating")
	after := pendingReloadAgent(t, s, agent.ID)
	assert.Equal(t, agent.Name, after.Name)
	assert.Equal(t, agent.StateVersion, after.StateVersion)
}

// TestAgentPendingConfig_SaveValidation: a held save runs the live PATCH's
// validation (a bad thinking level is refused and nothing is written), TZ
// is stripped from env with a warning, an unchanged key is not stored, and
// a stale stateVersion is refused.
func TestAgentPendingConfig_SaveValidation(t *testing.T) {
	srv, s, _, agent := heldEditsFixture(t, state.PhaseRunning)

	rec := doRequest(t, srv, http.MethodPatch, "/api/v1/agents/"+agent.ID, map[string]interface{}{
		"config": map[string]interface{}{"thinking_level": 150},
	})
	require.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
	unchanged := pendingReloadAgent(t, s, agent.ID)
	assert.Nil(t, unchanged.PendingConfig)
	assert.Equal(t, agent.StateVersion, unchanged.StateVersion)

	resp := patchPending(t, srv, agent.ID, map[string]interface{}{
		"config": map[string]interface{}{"env": map[string]string{"FOO": "bar", "TZ": "Europe/Paris"}, "model": "old-model"},
	})
	assert.Contains(t, resp.Warnings, configEnvTZIgnoredWarning)
	assert.Equal(t, []string{"config.env"}, resp.Disposition.Held, "the model equals its current value, so it is not an edit")
	stored := pendingReloadAgent(t, s, agent.ID)
	require.NotNil(t, stored.PendingConfig)
	assert.JSONEq(t, `{"FOO":"bar"}`, string(stored.PendingConfig.Config["env"]), "TZ is not stored")
	assert.NotContains(t, stored.PendingConfig.Config, "model")

	stale := doRequest(t, srv, http.MethodPatch, "/api/v1/agents/"+agent.ID, map[string]interface{}{
		"config": map[string]interface{}{"model": "x"}, "stateVersion": agent.StateVersion,
	})
	assert.Equal(t, http.StatusConflict, stale.Code, stale.Body.String())

	// Sending the pending value again is not an edit either.
	again := patchPending(t, srv, agent.ID, map[string]interface{}{
		"config": map[string]interface{}{"env": map[string]string{"FOO": "bar"}},
	})
	assert.Empty(t, again.Disposition.Held)
}

// TestAgentPendingConfig_GetShowsPendingAndEffectiveNextRun: the agent GET
// carries the pending config and held editability; ?view=edit adds
// effectiveNextRun, the applied config with the pending keys applied.
func TestAgentPendingConfig_GetShowsPendingAndEffectiveNextRun(t *testing.T) {
	srv, _, _, agent := heldEditsFixture(t, state.PhaseRunning)
	patchPending(t, srv, agent.ID, map[string]interface{}{
		"config": map[string]interface{}{"model": "next-model", "system_prompt": "be brief"},
	})

	rec := doRequest(t, srv, http.MethodGet, "/api/v1/agents/"+agent.ID, nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var plain AgentWithCapabilities
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &plain))
	require.NotNil(t, plain.PendingConfig)
	assert.Equal(t, []string{"config.model", "config.system_prompt"}, pendingConfigKeys(plain.PendingConfig))
	assert.Nil(t, plain.EffectiveNextRun, "effectiveNextRun only on ?view=edit")
	require.NotNil(t, plain.Editability)
	assert.Equal(t, EditHeld, plain.Editability.Fields["config.model"].Disposition)
	assert.Equal(t, EditReincarnate, plain.Editability.Fields["config.system_prompt"].Disposition)

	rec = doRequest(t, srv, http.MethodGet, "/api/v1/agents/"+agent.ID+"?view=edit", nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var edit AgentWithCapabilities
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &edit))
	require.NotNil(t, edit.EffectiveNextRun)
	assert.Equal(t, "next-model", edit.EffectiveNextRun.Model)
	assert.Equal(t, "be brief", edit.EffectiveNextRun.InlineConfig.SystemPrompt)
	assert.Nil(t, edit.EffectiveNextRun.CreateInputs)
	assert.Equal(t, "old-model", edit.AppliedConfig.Model, "the applied config itself is unchanged")
}
