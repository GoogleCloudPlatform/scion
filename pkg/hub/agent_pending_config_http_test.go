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
	"github.com/GoogleCloudPlatform/scion/pkg/api"
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
	assert.Equal(t, api.IntPtr(3), starts[0].inline.MaxTurns, "keys the edit did not name are kept")

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
	assert.Equal(t, api.IntPtr(12), starts[0].inline.MaxTurns)
	assert.Nil(t, pendingReloadAgent(t, s, agent.ID).PendingConfig)

	applied := pendingAudits(t, s, agent.ID, mutationTypeAgentPendingConfigApplied, 1)
	_, trigger := pendingAuditSummary(t, applied[0])
	assert.Equal(t, pendingFoldTriggerWake, trigger)
	// The wake here carries no caller identity: the hub is the actor.
	assert.Equal(t, "system", applied[0].ActorPrincipalKind)
	assert.Equal(t, pendingConfigSystemActor, applied[0].ActorPrincipalID)
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
	assert.Equal(t, "config.system_prompt: "+heldProvisionNote, resp.Warnings[0])

	warnings := pendingLifecycleWarnings(t, srv, agent.ID, "start")
	assert.Contains(t, warnings, heldForReincarnateWarning([]string{"config.system_prompt"}))

	starts := disp.snapshot()
	require.Len(t, starts, 1)
	assert.Empty(t, starts[0].inline.SystemPrompt, "a start does not apply a provision-rendered key")
	after := pendingReloadAgent(t, s, agent.ID)
	require.NotNil(t, after.PendingConfig, "the system prompt stays pending")
	assert.Equal(t, []string{"config.system_prompt"}, pendingConfigKeys(after.PendingConfig))
	assert.Equal(t, heldForReincarnateWarning([]string{"config.system_prompt"}), after.Message)

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
	assert.Equal(t, api.IntPtr(9), resp.AppliedConfig.InlineConfig.MaxTurns)
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
	assert.Equal(t, api.IntPtr(21), starts[0].inline.MaxTurns)
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

	t.Run("an edit written through removes the pending keys it names", func(t *testing.T) {
		srv, s, disp, stopped := off(t, state.PhaseStopped)
		cur := pendingReloadAgent(t, s, stopped.ID)
		cur.PendingConfig = &store.AgentPendingConfig{Config: map[string]json.RawMessage{
			"model":         json.RawMessage(`"stale-model"`),
			"system_prompt": json.RawMessage(`"pending prompt"`),
		}}
		require.NoError(t, s.UpdateAgent(context.Background(), cur))
		resp := patchPending(t, srv, stopped.ID, map[string]interface{}{
			"config": map[string]interface{}{"model": "newer-model"},
		})
		assert.Equal(t, []string{"config.model"}, resp.Disposition.Applied)
		require.NotNil(t, resp.PendingConfig)
		assert.Equal(t, []string{"config.system_prompt"}, pendingConfigKeys(resp.PendingConfig), "only the key the edit named is removed")
		discarded := pendingAudits(t, s, stopped.ID, mutationTypeAgentPendingConfigDiscarded, 1)
		keys, trigger := pendingAuditSummary(t, discarded[0])
		assert.Equal(t, []string{"config.model"}, keys)
		assert.Equal(t, pendingDiscardTriggerSuperseded, trigger)

		// Turned back on, the next start runs the newer value.
		setHeldEditsExperiment(t, srv, true)
		pendingLifecycleWarnings(t, srv, stopped.ID, "start")
		starts := disp.snapshot()
		require.Len(t, starts, 1)
		assert.Equal(t, "newer-model", starts[0].model)
	})

	// The start fold's whole-key keep (ptone/scion#5864) runs only with
	// held edits on: a clear written through with them off still reaches
	// the applied config and the create inputs now.
	t.Run("a clear written through still reaches the applied config", func(t *testing.T) {
		srv, s, _, stopped := off(t, state.PhaseStopped)
		effectSetCreateInputsMaxTurns(t, s, stopped.ID, 3)
		resp := patchPending(t, srv, stopped.ID, map[string]interface{}{
			"config": map[string]interface{}{"max_turns": nil},
		})
		assert.Equal(t, []string{"config.max_turns"}, resp.Disposition.HeldForReincarnate)
		assert.Nil(t, resp.PendingConfig)
		after := pendingReloadAgent(t, s, stopped.ID)
		assert.Nil(t, after.AppliedConfig.InlineConfig.MaxTurns, "written through, as before")
		require.NotNil(t, after.AppliedConfig.CreateInputs)
		require.NotNil(t, after.AppliedConfig.CreateInputs.InlineConfig)
		assert.Nil(t, after.AppliedConfig.CreateInputs.InlineConfig.MaxTurns, "recorded as an explicit clear, as before")
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

// TestAgentPendingConfig_ResponsesRedactPendingSecretsForNonAttachViewer:
// pending env values, telemetry cloud headers, MCP server env and headers
// and service env, some saved with keys spelled in another case, are
// returned to the agent's owner (who may attach) and
// never to a project member who may not attach: on GET, the project list
// and that member's PATCH response.
func TestAgentPendingConfig_ResponsesRedactPendingSecretsForNonAttachViewer(t *testing.T) {
	srv, s, alice, _, project := setupDemoPolicyTest(t)
	setHeldEditsExperiment(t, srv, true)
	ctx := context.Background()
	agent := &store.Agent{
		ID:        tid("agent-pending-redaction"),
		Slug:      "agent-pending-redaction",
		Name:      "Pending Redaction Agent",
		ProjectID: project.ID,
		OwnerID:   alice.ID,
		Phase:     string(state.PhaseRunning),
		AppliedConfig: &store.AgentAppliedConfig{
			InlineConfig: &api.ScionConfig{Harness: "claude"},
		},
	}
	require.NoError(t, s.CreateAgent(ctx, agent))
	bob := makeProjectMemberUser(t, s, project, tid("user-bob-pending-redaction"), "Bob", store.GroupMemberRoleOwner)
	createTestUserWithProjectRole(t, s, bob.ID, bob.Email, project.ID, store.ProjectRoleOwner)

	rec := doRequestAsUser(t, srv, alice, http.MethodPatch, "/api/v1/agents/"+agent.ID, map[string]any{
		"config": map[string]any{
			"Telemetry": map[string]any{"Cloud": map[string]any{
				"Endpoint": "https://otel.example",
				"Headers":  map[string]string{"Authorization": "Bearer pending-secret"},
			}},
			"ENV": map[string]string{"PENDING_KEY": "pending-env-secret"},
			"mcp_servers": map[string]any{"docs": map[string]any{
				"transport": "http", "url": "https://mcp.example",
				"Headers": map[string]string{"Authorization": "Bearer mcp-pending-secret"},
				"env":     map[string]string{"MCP_TOKEN": "mcp-env-pending-secret"},
			}},
			"services": []map[string]any{{
				"name": "db", "command": []string{"run"},
				"env": map[string]string{"DB_PASSWORD": "svc-pending-secret"},
			}},
		},
	})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	stored := pendingReloadAgent(t, s, agent.ID)
	require.NotNil(t, stored.PendingConfig)
	assert.Contains(t, stored.PendingConfig.Config, "telemetry", "stored under the json key")
	assert.Contains(t, stored.PendingConfig.Config, "env")
	assert.NotContains(t, stored.PendingConfig.Config, "Telemetry")

	t.Run("owner GET sees the pending secrets", func(t *testing.T) {
		rec := doRequestAsUser(t, srv, alice, http.MethodGet, "/api/v1/agents/"+agent.ID, nil)
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		assert.Contains(t, rec.Body.String(), "Bearer pending-secret")
		assert.Contains(t, rec.Body.String(), "pending-env-secret")
		assert.Contains(t, rec.Body.String(), "mcp-pending-secret")
		assert.Contains(t, rec.Body.String(), "mcp-env-pending-secret")
		assert.Contains(t, rec.Body.String(), "svc-pending-secret")
	})
	t.Run("non-attach member GET sees neither", func(t *testing.T) {
		rec := doRequestAsUser(t, srv, bob, http.MethodGet, "/api/v1/agents/"+agent.ID+"?view=edit", nil)
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		assert.NotContains(t, rec.Body.String(), "pending-secret")
		assert.NotContains(t, rec.Body.String(), "pending-env-secret")
		assert.Contains(t, rec.Body.String(), "https://otel.example", "the rest of the pending telemetry is shown")
		assert.Contains(t, rec.Body.String(), "https://mcp.example", "the pending MCP server is still listed")
	})
	t.Run("non-attach member list sees neither", func(t *testing.T) {
		rec := doRequestAsUser(t, srv, bob, http.MethodGet, "/api/v1/agents?projectId="+project.ID, nil)
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		assert.NotContains(t, rec.Body.String(), "pending-secret")
		assert.NotContains(t, rec.Body.String(), "pending-env-secret")
	})
	t.Run("non-attach member PATCH response sees neither", func(t *testing.T) {
		rec := doRequestAsUser(t, srv, bob, http.MethodPatch, "/api/v1/agents/"+agent.ID, map[string]any{
			"taskSummary": "updated by project owner",
		})
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		assert.NotContains(t, rec.Body.String(), "pending-secret")
		assert.NotContains(t, rec.Body.String(), "pending-env-secret")
	})
}

// TestAgentPendingConfig_ReincarnateDoesNotApplyPendingYet pins the interim
// behaviour until reincarnation applies pending edits (ptone/scion#3978): a
// reincarnation neither applies nor clears the pending config, and a pending
// container key is applied at the next start after it, over the value the
// reincarnation set. ptone/scion#3978 changes this deliberately.
func TestAgentPendingConfig_ReincarnateDoesNotApplyPendingYet(t *testing.T) {
	srv, s, disp, agent := heldEditsFixture(t, state.PhaseRunning)
	patchPending(t, srv, agent.ID, map[string]interface{}{
		"config": map[string]interface{}{"model": "held-model", "system_prompt": "held prompt"},
	})

	rec := reincarnateAsDev(t, srv, agent.ID, ReincarnateAgentRequest{Model: "reincarnated-model"})
	require.Equal(t, http.StatusAccepted, rec.Code, rec.Body.String())
	waitForReincarnationSettled(t, s, agent.ID)

	after := pendingReloadAgent(t, s, agent.ID)
	assert.Equal(t, "reincarnated-model", after.AppliedConfig.Model, "the reincarnation applies its own patch")
	require.NotNil(t, after.PendingConfig, "the reincarnation leaves the pending config in place")
	assert.Equal(t, []string{"config.model", "config.system_prompt"}, pendingConfigKeys(after.PendingConfig))
	recs, _, err := s.ListMutationAudits(context.Background(), store.MutationAuditFilter{
		MutationType: mutationTypeAgentPendingConfigApplied, TargetID: agent.ID, Limit: 5,
	})
	require.NoError(t, err)
	assert.Empty(t, recs, "nothing pending was applied by the reincarnation")

	before := len(disp.snapshot())
	pendingLifecycleWarnings(t, srv, agent.ID, "stop")
	pendingLifecycleWarnings(t, srv, agent.ID, "start")
	starts := disp.snapshot()
	require.Len(t, starts, before+1)
	assert.Equal(t, "held-model", starts[len(starts)-1].model, "the pending model is applied at the next start, over the reincarnation's")
	final := pendingReloadAgent(t, s, agent.ID)
	require.NotNil(t, final.PendingConfig)
	assert.Equal(t, []string{"config.system_prompt"}, pendingConfigKeys(final.PendingConfig))
}

// TestAgentPendingConfig_StartOfRunningAgentKeepsHeldEdits: a start of an
// agent that is already running keeps its running container, so held
// edits stay held: nothing is applied, the pending config is kept and no
// applied audit is written. The next container creation applies them.
func TestAgentPendingConfig_StartOfRunningAgentKeepsHeldEdits(t *testing.T) {
	srv, s, disp, agent := heldEditsFixture(t, state.PhaseRunning)
	patchPending(t, srv, agent.ID, map[string]interface{}{
		"config": map[string]interface{}{"model": "held-model"},
	})

	rec := doRequest(t, srv, http.MethodPost, "/api/v1/agents/"+agent.ID+"/start", nil)
	require.Less(t, rec.Code, 300, rec.Body.String())
	after := pendingReloadAgent(t, s, agent.ID)
	assert.Equal(t, "old-model", after.AppliedConfig.Model, "the running container was not recreated")
	require.NotNil(t, after.PendingConfig, "the held edit stays held")
	assert.Equal(t, []string{"config.model"}, pendingConfigKeys(after.PendingConfig))
	for _, st := range disp.snapshot() {
		assert.Equal(t, "old-model", st.model)
	}
	recs, _, err := s.ListMutationAudits(context.Background(), store.MutationAuditFilter{
		MutationType: mutationTypeAgentPendingConfigApplied, TargetID: agent.ID, Limit: 5,
	})
	require.NoError(t, err)
	assert.Empty(t, recs, "nothing was applied")

	pendingLifecycleWarnings(t, srv, agent.ID, "stop")
	pendingLifecycleWarnings(t, srv, agent.ID, "start")
	starts := disp.snapshot()
	require.NotEmpty(t, starts)
	assert.Equal(t, "held-model", starts[len(starts)-1].model, "the next container creation applies the held edit")
	assert.Nil(t, pendingReloadAgent(t, s, agent.ID).PendingConfig)
}

// TestAgentPendingConfig_UndecodableStoredKeyIsRemovedAtStart: a stored
// pending key that no longer decodes can never be applied; the start
// removes it (and applies the rest), so it does not stay pending or warn.
func TestAgentPendingConfig_UndecodableStoredKeyIsRemovedAtStart(t *testing.T) {
	srv, s, disp, agent := heldEditsFixture(t, state.PhaseStopped)
	cur := pendingReloadAgent(t, s, agent.ID)
	cur.PendingConfig = &store.AgentPendingConfig{Config: map[string]json.RawMessage{
		"model":     json.RawMessage(`"good-model"`),
		"max_turns": json.RawMessage(`"not a number"`),
	}}
	require.NoError(t, s.UpdateAgent(context.Background(), cur))

	warnings := pendingLifecycleWarnings(t, srv, agent.ID, "start")
	for _, w := range warnings {
		assert.NotContains(t, w, heldReincarnateWarningPrefix)
	}
	starts := disp.snapshot()
	require.Len(t, starts, 1)
	assert.Equal(t, "good-model", starts[0].model)
	assert.Equal(t, api.IntPtr(3), starts[0].inline.MaxTurns, "the undecodable value is not applied")
	assert.Nil(t, pendingReloadAgent(t, s, agent.ID).PendingConfig, "the undecodable key is removed")
	discarded := pendingAudits(t, s, agent.ID, mutationTypeAgentPendingConfigDiscarded, 1)
	keys, trigger := pendingAuditSummary(t, discarded[0])
	assert.Equal(t, []string{"config.max_turns"}, keys)
	assert.Equal(t, pendingDiscardTriggerUnappliable, trigger)
}

// TestAgentPendingConfig_StartRemovesUnappliableKeysWhenNothingFolds: with
// no container key pending, a start still removes the stored keys that can
// never be applied (one that no longer decodes, one the edit table does not
// know) and persists that, keeps the valid provision-rendered key, warns
// about it alone and writes no applied audit.
func TestAgentPendingConfig_StartRemovesUnappliableKeysWhenNothingFolds(t *testing.T) {
	srv, s, _, agent := heldEditsFixture(t, state.PhaseStopped)
	cur := pendingReloadAgent(t, s, agent.ID)
	cur.PendingConfig = &store.AgentPendingConfig{Config: map[string]json.RawMessage{
		"system_prompt": json.RawMessage(`"be brief"`),
		"max_turns":     json.RawMessage(`"not a number"`),
		"no_such_key":   json.RawMessage(`1`),
	}}
	require.NoError(t, s.UpdateAgent(context.Background(), cur))
	before := pendingReloadAgent(t, s, agent.ID)

	warnings := pendingLifecycleWarnings(t, srv, agent.ID, "start")
	assert.Contains(t, warnings, heldForReincarnateWarning([]string{"config.system_prompt"}))

	after := pendingReloadAgent(t, s, agent.ID)
	require.NotNil(t, after.PendingConfig)
	assert.Equal(t, []string{"config.system_prompt"}, pendingConfigKeys(after.PendingConfig), "the valid provision-rendered key stays pending")
	assert.Greater(t, after.StateVersion, before.StateVersion, "the removal is persisted")
	recs, _, err := s.ListMutationAudits(context.Background(), store.MutationAuditFilter{
		MutationType: mutationTypeAgentPendingConfigApplied, TargetID: agent.ID, Limit: 5,
	})
	require.NoError(t, err)
	assert.Empty(t, recs, "nothing was applied")
	discarded := pendingAudits(t, s, agent.ID, mutationTypeAgentPendingConfigDiscarded, 1)
	keys, trigger := pendingAuditSummary(t, discarded[0])
	assert.Equal(t, []string{"config.max_turns", "config.no_such_key"}, keys)
	assert.Equal(t, pendingDiscardTriggerUnappliable, trigger)
}

// TestAgentPendingConfig_PatchRemovesUnappliableKeysAndAudits: a config
// PATCH of an agent with no container folds its pending config first; a
// stored key that can never be applied is removed there and audited once
// as discarded with trigger unappliable, and a valid provision-rendered key
// stays pending.
func TestAgentPendingConfig_PatchRemovesUnappliableKeysAndAudits(t *testing.T) {
	srv, s, _, agent := heldEditsFixture(t, state.PhaseStopped)
	cur := pendingReloadAgent(t, s, agent.ID)
	cur.PendingConfig = &store.AgentPendingConfig{Config: map[string]json.RawMessage{
		"system_prompt": json.RawMessage(`"be brief"`),
		"max_turns":     json.RawMessage(`"not a number"`),
	}}
	require.NoError(t, s.UpdateAgent(context.Background(), cur))

	resp := patchPending(t, srv, agent.ID, map[string]interface{}{
		"config": map[string]interface{}{"model": "patched-model"},
	})
	assert.Equal(t, []string{"config.model"}, resp.Disposition.Applied)
	require.NotNil(t, resp.PendingConfig)
	assert.Equal(t, []string{"config.system_prompt"}, pendingConfigKeys(resp.PendingConfig))
	stored := pendingReloadAgent(t, s, agent.ID)
	require.NotNil(t, stored.PendingConfig)
	assert.Equal(t, []string{"config.system_prompt"}, pendingConfigKeys(stored.PendingConfig), "the removal is persisted")
	assert.Equal(t, api.IntPtr(3), stored.AppliedConfig.InlineConfig.MaxTurns, "the undecodable value is not applied")

	discarded := pendingAudits(t, s, agent.ID, mutationTypeAgentPendingConfigDiscarded, 1)
	require.Len(t, discarded, 1, "exactly one discarded audit")
	keys, trigger := pendingAuditSummary(t, discarded[0])
	assert.Equal(t, []string{"config.max_turns"}, keys)
	assert.Equal(t, pendingDiscardTriggerUnappliable, trigger)
	// No second record arrives later.
	time.Sleep(200 * time.Millisecond)
	recs, _, err := s.ListMutationAudits(context.Background(), store.MutationAuditFilter{
		MutationType: mutationTypeAgentPendingConfigDiscarded, TargetID: agent.ID, Limit: 5,
	})
	require.NoError(t, err)
	assert.Len(t, recs, 1)
}

// TestAgentPendingConfig_AsyncStartAppliesHeldEdits: an asynchronous start
// (hub.async_agent_start, accepted as a launch) goes through the same fold:
// the agent the dispatcher receives already carries the held edit, which is
// persisted and audited.
func TestAgentPendingConfig_AsyncStartAppliesHeldEdits(t *testing.T) {
	f, d := newAsyncStartFixture(t)
	var active []experiments.Experiment
	for _, e := range f.srv.experimentRegistry().All() {
		if e.Name == experiments.AgentEditHeld || e.Name == experiments.AsyncAgentStart {
			e.Default = true
		}
		active = append(active, e)
	}
	reg, err := experiments.NewRegistry(active, nil)
	require.NoError(t, err)
	f.srv.experiments = reg
	require.True(t, f.srv.heldEditsEnabled())
	require.True(t, f.srv.experimentEnabled(experiments.AsyncAgentStart))

	a := f.addAgent("as-held", string(state.PhaseStopped), "")
	cur := pendingReloadAgent(t, f.s, a.ID)
	cur.PendingConfig = &store.AgentPendingConfig{Config: map[string]json.RawMessage{"model": json.RawMessage(`"async-held-model"`)}}
	require.NoError(t, f.s.UpdateAgent(context.Background(), cur))

	// The agent the dispatcher received: the model it was dispatched with.
	var dispatchedModel string
	dispatched := false
	d.onAccept = func(_ context.Context, ag *store.Agent) {
		dispatched = true
		if ag.AppliedConfig != nil {
			dispatchedModel = ag.AppliedConfig.Model
		}
	}

	code, body := lifecycleWithBody(t, f, a.ID, "start", map[string]bool{"acceptAsyncLaunch": true})
	require.Equal(t, http.StatusOK, code, body)
	opts, _, _ := d.calls()
	require.Len(t, opts, 1)
	assert.True(t, opts[0].Async, "the start was dispatched asynchronously")
	require.True(t, dispatched, "the asynchronous dispatch was accepted")
	assert.Equal(t, "async-held-model", dispatchedModel, "the dispatch carried the held edit: the fold ran before it")

	after := pendingReloadAgent(t, f.s, a.ID)
	require.NotNil(t, after.AppliedConfig)
	assert.Equal(t, "async-held-model", after.AppliedConfig.Model, "the held edit was applied before the dispatch")
	assert.Nil(t, after.PendingConfig)
	applied := pendingAudits(t, f.s, a.ID, mutationTypeAgentPendingConfigApplied, 1)
	keys, trigger := pendingAuditSummary(t, applied[0])
	assert.Equal(t, []string{"config.model"}, keys)
	assert.Equal(t, pendingFoldTriggerStart, trigger)
}

// TestAgentPendingConfig_HeldSystemPromptOnRerenderingBroker: on a broker
// that re-renders provision-rendered keys at start (changedFields,
// ptone/scion#3982), a system prompt edit saved while the agent runs is
// held, and the next start applies it: the dispatch carries it and lists
// it in changed_fields, and the pending config is cleared. On a broker
// without changedFields it stays pending, with the start warning.
func TestAgentPendingConfig_HeldSystemPromptOnRerenderingBroker(t *testing.T) {
	t.Run("broker re-renders at start", func(t *testing.T) {
		srv, s, disp, agent := heldEditsFixture(t, state.PhaseRunning)
		broker, err := s.GetRuntimeBroker(context.Background(), agent.RuntimeBrokerID)
		require.NoError(t, err)
		setRerenderingBroker(t, s, broker)

		resp := patchPending(t, srv, agent.ID, map[string]interface{}{
			"config": map[string]interface{}{"system_prompt": "be brief"},
		})
		assert.Equal(t, []string{"config.system_prompt"}, resp.Disposition.Held, "applied by the next start")
		assert.Empty(t, resp.Disposition.HeldForReincarnate)
		assert.Empty(t, resp.Warnings)
		require.NotNil(t, resp.PendingConfig)
		assert.Equal(t, []string{"config.system_prompt"}, pendingConfigKeys(resp.PendingConfig))
		ed := getAgentEditability(t, srv, nil, agent.ID)
		assert.Equal(t, EditHeld, ed.Fields["config.system_prompt"].Disposition)
		assert.Equal(t, EditReincarnate, ed.Fields["config.secrets"].Disposition, "secrets are not re-rendered at start")

		pendingLifecycleWarnings(t, srv, agent.ID, "stop")
		warnings := pendingLifecycleWarnings(t, srv, agent.ID, "start")
		for _, w := range warnings {
			assert.NotContains(t, w, heldReincarnateWarningPrefix)
		}
		starts := disp.snapshot()
		require.Len(t, starts, 1)
		assert.Equal(t, "be brief", starts[0].inline.SystemPrompt)
		assert.Contains(t, starts[0].inline.ChangedFields, "system_prompt", "the start asks the broker to re-render it")
		assert.Nil(t, pendingReloadAgent(t, s, agent.ID).PendingConfig, "the pending config is cleared")
		applied := pendingAudits(t, s, agent.ID, mutationTypeAgentPendingConfigApplied, 1)
		keys, _ := pendingAuditSummary(t, applied[0])
		assert.Equal(t, []string{"config.system_prompt"}, keys)
	})

	t.Run("broker without changedFields", func(t *testing.T) {
		srv, s, disp, agent := heldEditsFixture(t, state.PhaseRunning)
		resp := patchPending(t, srv, agent.ID, map[string]interface{}{
			"config": map[string]interface{}{"system_prompt": "be brief"},
		})
		assert.Empty(t, resp.Disposition.Held)
		assert.Equal(t, []string{"config.system_prompt"}, resp.Disposition.HeldForReincarnate)

		pendingLifecycleWarnings(t, srv, agent.ID, "stop")
		warnings := pendingLifecycleWarnings(t, srv, agent.ID, "start")
		assert.Contains(t, warnings, heldForReincarnateWarning([]string{"config.system_prompt"}))
		starts := disp.snapshot()
		require.Len(t, starts, 1)
		assert.Empty(t, starts[0].inline.SystemPrompt)
		assert.NotContains(t, starts[0].inline.ChangedFields, "system_prompt")
		after := pendingReloadAgent(t, s, agent.ID)
		require.NotNil(t, after.PendingConfig)
		assert.Equal(t, []string{"config.system_prompt"}, pendingConfigKeys(after.PendingConfig), "it stays pending")
	})
}

// agentUpdateOutcome is what a config PATCH reported and stored, for
// comparing the held-edits flag on and off.
type agentUpdateOutcome struct {
	Disposition   AgentUpdateDisposition
	Warnings      []string
	ChangedFields []string
	ClearedFields []string
	Pending       []string
}

// patchOutcome runs a PATCH on a fresh stopped agent (with the broker's
// caps set by caps and held edits on or off) and returns what it reported
// and stored.
func patchOutcome(t *testing.T, heldOn bool, caps *store.BrokerCapabilities, body map[string]interface{}) agentUpdateOutcome {
	t.Helper()
	disp := &inlineCaptureDispatcher{reincarnateTestDispatcher: newReincarnateTestDispatcher()}
	srv, s, project, broker := setupReincarnateTestServer(t, disp)
	setHeldEditsExperiment(t, srv, heldOn)
	setBrokerCaps(t, s, broker, caps)
	agent := newT2EditAgent(t, s, project, broker)
	resp := patchPending(t, srv, agent.ID, body)
	stored := pendingReloadAgent(t, s, agent.ID)
	return agentUpdateOutcome{
		Disposition:   resp.Disposition,
		Warnings:      resp.Warnings,
		ChangedFields: stored.AppliedConfig.InlineConfig.ChangedFields,
		ClearedFields: stored.AppliedConfig.InlineConfig.ClearedFields,
		Pending:       pendingConfigKeys(stored.PendingConfig),
	}
}

// TestAgentPendingConfig_NoContainerRerenderEditsMatchFlagOff: for an
// agent with no container on a broker that re-renders at start
// (changedFields) and applies clears (clearedFields), a provision-rendered
// edit is written now with held edits on exactly as with them off: the
// same disposition, warnings, changed_fields and cleared_fields, and
// nothing pending. Cases: non-empty values, a system prompt clear (applied
// through cleared_fields), an agent instructions clear and a skill removal
// (both held for reincarnation with AE-11's warning). With held edits off
// the outcome is AE-11's own (its tests in agent_config_rerender*_test.go
// cover the rest); on an older broker, held edits off records the
// reincarnation hold and changed_fields as AE-11 does.
func TestAgentPendingConfig_NoContainerRerenderEditsMatchFlagOff(t *testing.T) {
	capable := &store.BrokerCapabilities{Reprovision: true, ChangedFields: true, ClearedFields: true}
	cases := []struct {
		name string
		cfg  map[string]interface{}
	}{
		{"values", map[string]interface{}{"system_prompt": "be brief", "agent_instructions": "run the tests first"}},
		{"system prompt clear", map[string]interface{}{"system_prompt": nil}},
		{"instructions clear", map[string]interface{}{"agent_instructions": ""}},
		{"skill removal", map[string]interface{}{"skills": []map[string]interface{}{{"uri": "hub://skills/review"}}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			body := map[string]interface{}{"config": tc.cfg}
			var off, on agentUpdateOutcome
			t.Run("off", func(t *testing.T) { off = patchOutcome(t, false, capable, body) })
			t.Run("on", func(t *testing.T) { on = patchOutcome(t, true, capable, body) })
			assert.Equal(t, off, on, "held edits on: the no-container edit is written now, exactly as with the flag off")
			assert.Empty(t, on.Pending)
		})
	}

	older := &store.BrokerCapabilities{Reprovision: true}
	offOlder := patchOutcome(t, false, older, map[string]interface{}{"config": map[string]interface{}{
		"system_prompt": "be brief", "agent_instructions": "run the tests first",
	}})
	assert.Empty(t, offOlder.Disposition.Applied)
	assert.Equal(t, []string{"config.agent_instructions", "config.system_prompt"}, offOlder.Disposition.HeldForReincarnate)
	require.Len(t, offOlder.Warnings, 1, "%v", offOlder.Warnings)
	assert.Contains(t, offOlder.Warnings[0], "rendered at the next reincarnation")
	assert.Contains(t, offOlder.ChangedFields, "system_prompt", "recorded for a broker upgraded later")
	assert.Empty(t, offOlder.Pending)
}

// heldFilterFixture is a running agent, with held edits on and its broker's
// caps set to caps, whose inline config sets the provision-rendered keys,
// two skills, a duration limit, two env keys and resource requests.
func heldFilterFixture(t *testing.T, caps *store.BrokerCapabilities) (*Server, store.Store, *inlineCaptureDispatcher, *store.Agent) {
	t.Helper()
	disp := &inlineCaptureDispatcher{reincarnateTestDispatcher: newReincarnateTestDispatcher()}
	srv, s, project, broker := setupReincarnateTestServer(t, disp)
	setHeldEditsExperiment(t, srv, true)
	setBrokerCaps(t, s, broker, caps)
	agent := newReincarnateTestAgent(t, s, project, broker, func(a *store.Agent) {
		a.Phase = string(state.PhaseRunning)
		a.AppliedConfig.Model = "old-model"
		a.AppliedConfig.InlineConfig = &api.ScionConfig{
			Harness:           "claude",
			Model:             "old-model",
			SystemPrompt:      "old prompt",
			AgentInstructions: "old instructions",
			Skills:            []api.SkillReference{{URI: "hub://skills/review"}, {URI: "hub://skills/test"}},
			Services:          []api.ServiceSpec{{Name: "db", Command: []string{"postgres"}}},
			MaxDuration:       "2h",
			Env:               map[string]string{"KEEP": "1", "DROP": "2"},
			Resources:         &api.ResourceSpec{Requests: api.ResourceList{CPU: "1", Memory: "1Gi"}},
		}
	})
	return srv, s, disp, pendingReloadAgent(t, s, agent.ID)
}

// TestAgentPendingConfig_HeldSaveUsesTheStartRules: a held save reports
// each edit by what the next start will do with it on the agent's broker,
// with the rules a written-through edit gets (ptone/scion#3982, #4890,
// #5676).
func TestAgentPendingConfig_HeldSaveUsesTheStartRules(t *testing.T) {
	t.Run("re-rendering broker: a clear or skill removal is kept pending whole until a reincarnation", func(t *testing.T) {
		srv, _, _, agent := heldFilterFixture(t, &store.BrokerCapabilities{Reprovision: true, ChangedFields: true})
		resp := patchPending(t, srv, agent.ID, map[string]interface{}{"config": map[string]interface{}{
			"agent_instructions": "",
			"skills":             []map[string]interface{}{{"uri": "hub://skills/review"}},
		}})
		assert.Empty(t, resp.Disposition.Held)
		assert.Equal(t, []string{"config.agent_instructions", "config.skills"}, resp.Disposition.HeldForReincarnate)
		require.Len(t, resp.Warnings, 1, "%v", resp.Warnings)
		assert.Equal(t, "config.agent_instructions, config.skills: "+heldWholeKeyNote, resp.Warnings[0])
	})

	t.Run("clearing broker: a clear and an env removal are held and reach cleared_fields at the next start", func(t *testing.T) {
		srv, s, disp, agent := heldFilterFixture(t, &store.BrokerCapabilities{Reprovision: true, ClearedFields: true})
		resp := patchPending(t, srv, agent.ID, map[string]interface{}{"config": map[string]interface{}{
			"max_duration": nil,
			"env":          map[string]string{"KEEP": "1"},
		}})
		assert.Equal(t, []string{"config.env", "config.max_duration"}, resp.Disposition.Held)
		assert.Empty(t, resp.Disposition.HeldForReincarnate)
		assert.Empty(t, resp.Warnings)

		pendingLifecycleWarnings(t, srv, agent.ID, "stop")
		pendingLifecycleWarnings(t, srv, agent.ID, "start")
		starts := disp.snapshot()
		require.Len(t, starts, 1)
		assert.Contains(t, starts[0].inline.ClearedFields, "max_duration")
		assert.Contains(t, starts[0].inline.ClearedFields, api.ClearedEnvPathPrefix+"DROP")
		assert.Empty(t, starts[0].inline.MaxDuration)
		assert.NotContains(t, starts[0].inline.Env, "DROP")
		assert.Nil(t, pendingReloadAgent(t, s, agent.ID).PendingConfig)
	})

	t.Run("a resources edit that drops a sub-field needs a reincarnation", func(t *testing.T) {
		srv, _, _, agent := heldFilterFixture(t, &store.BrokerCapabilities{Reprovision: true, ClearedFields: true})
		resp := patchPending(t, srv, agent.ID, map[string]interface{}{"config": map[string]interface{}{
			"resources": map[string]interface{}{"requests": map[string]string{"cpu": "1"}},
		}})
		assert.Empty(t, resp.Disposition.Held)
		assert.Equal(t, []string{"config.resources"}, resp.Disposition.HeldForReincarnate)
	})

	t.Run("re-rendering broker: secrets stay pending through a start and never reach changed_fields", func(t *testing.T) {
		srv, s, disp, agent := heldFilterFixture(t, &store.BrokerCapabilities{Reprovision: true, ChangedFields: true})
		resp := patchPending(t, srv, agent.ID, map[string]interface{}{"config": map[string]interface{}{
			"secrets": []map[string]interface{}{{"key": "API_TOKEN"}},
		}})
		assert.Empty(t, resp.Disposition.Held)
		assert.Equal(t, []string{"config.secrets"}, resp.Disposition.HeldForReincarnate)
		require.Len(t, resp.Warnings, 1, "%v", resp.Warnings)
		assert.Equal(t, "config.secrets: "+heldNotRerenderedNote, resp.Warnings[0])

		pendingLifecycleWarnings(t, srv, agent.ID, "stop")
		warnings := pendingLifecycleWarnings(t, srv, agent.ID, "start")
		assert.Contains(t, warnings, heldForReincarnateWarning([]string{"config.secrets"}))
		starts := disp.snapshot()
		require.Len(t, starts, 1)
		assert.NotContains(t, starts[0].inline.ChangedFields, "secrets")
		assert.Empty(t, starts[0].inline.Secrets)
		after := pendingReloadAgent(t, s, agent.ID)
		require.NotNil(t, after.PendingConfig)
		assert.Equal(t, []string{"config.secrets"}, pendingConfigKeys(after.PendingConfig))
	})

	t.Run("a key a start never applies stays held for reincarnation even when set back to its applied value", func(t *testing.T) {
		// A broker without changedFields: no provision-rendered key is
		// applied at a start.
		srv, s, _, agent := heldFilterFixture(t, &store.BrokerCapabilities{Reprovision: true})
		first := patchPending(t, srv, agent.ID, map[string]interface{}{"config": map[string]interface{}{"system_prompt": "new"}})
		assert.Equal(t, []string{"config.system_prompt"}, first.Disposition.HeldForReincarnate)
		back := patchPending(t, srv, agent.ID, map[string]interface{}{"config": map[string]interface{}{"system_prompt": "old prompt"}})
		assert.Empty(t, back.Disposition.Held, "a start does not apply the system prompt on this broker")
		assert.Equal(t, []string{"config.system_prompt"}, back.Disposition.HeldForReincarnate)
		require.Len(t, back.Warnings, 1, "%v", back.Warnings)
		assert.Equal(t, "config.system_prompt: "+heldProvisionNote, back.Warnings[0])

		pendingLifecycleWarnings(t, srv, agent.ID, "stop")
		warnings := pendingLifecycleWarnings(t, srv, agent.ID, "start")
		assert.Contains(t, warnings, heldForReincarnateWarning([]string{"config.system_prompt"}))
		after := pendingReloadAgent(t, s, agent.ID)
		require.NotNil(t, after.PendingConfig)
		assert.Equal(t, []string{"config.system_prompt"}, pendingConfigKeys(after.PendingConfig))
	})

	t.Run("secrets set back to the applied value stay held for reincarnation on a re-rendering broker", func(t *testing.T) {
		srv, _, _, agent := heldFilterFixture(t, &store.BrokerCapabilities{Reprovision: true, ChangedFields: true})
		first := patchPending(t, srv, agent.ID, map[string]interface{}{"config": map[string]interface{}{
			"secrets": []map[string]interface{}{{"key": "API_TOKEN"}},
		}})
		assert.Equal(t, []string{"config.secrets"}, first.Disposition.HeldForReincarnate)
		// The applied config has no secrets: this save sets them back.
		back := patchPending(t, srv, agent.ID, map[string]interface{}{"config": map[string]interface{}{"secrets": nil}})
		assert.Empty(t, back.Disposition.Held, "a start never applies secrets")
		assert.Equal(t, []string{"config.secrets"}, back.Disposition.HeldForReincarnate)
		require.Len(t, back.Warnings, 1, "%v", back.Warnings)
		assert.Equal(t, "config.secrets: "+heldNotRerenderedNote, back.Warnings[0])
	})

	t.Run("re-sending a pending removal is not an edit and warns about nothing", func(t *testing.T) {
		srv, _, _, agent := heldFilterFixture(t, &store.BrokerCapabilities{Reprovision: true, ChangedFields: true})
		body := map[string]interface{}{"config": map[string]interface{}{
			"skills": []map[string]interface{}{{"uri": "hub://skills/review"}},
		}}
		first := patchPending(t, srv, agent.ID, body)
		assert.Equal(t, []string{"config.skills"}, first.Disposition.HeldForReincarnate)
		again := patchPending(t, srv, agent.ID, body)
		assert.Empty(t, again.Disposition.Held)
		assert.Empty(t, again.Disposition.HeldForReincarnate)
		assert.Empty(t, again.Warnings, "the value equals the next-run value: nothing to report")
	})

	t.Run("a skill removal pending from an earlier save is still reported against the applied config", func(t *testing.T) {
		srv, s, disp, agent := heldFilterFixture(t, &store.BrokerCapabilities{Reprovision: true, ChangedFields: true})
		first := patchPending(t, srv, agent.ID, map[string]interface{}{"config": map[string]interface{}{
			"skills": []map[string]interface{}{{"uri": "hub://skills/review"}},
		}})
		assert.Equal(t, []string{"config.skills"}, first.Disposition.HeldForReincarnate)
		// The next-run value is [review]; this save adds a skill, but the
		// start applies [review, other] to the applied [review, test],
		// which still removes test.
		second := patchPending(t, srv, agent.ID, map[string]interface{}{"config": map[string]interface{}{
			"skills": []map[string]interface{}{{"uri": "hub://skills/review"}, {"uri": "hub://skills/other"}},
		}})
		assert.Empty(t, second.Disposition.Held, "the start will not apply the removal")
		assert.Equal(t, []string{"config.skills"}, second.Disposition.HeldForReincarnate)
		require.Len(t, second.Warnings, 1, "%v", second.Warnings)
		assert.Contains(t, second.Warnings[0], "the whole key's change is kept pending, including any part a start could apply")

		// The start keeps the whole skills edit pending, the added skill
		// included (ptone/scion#5864): the applied skills are unchanged and
		// the start names config.skills once.
		pendingLifecycleWarnings(t, srv, agent.ID, "stop")
		warnings := pendingLifecycleWarnings(t, srv, agent.ID, "start")
		assert.Contains(t, warnings, heldForReincarnateWarning([]string{"config.skills"}))
		starts := disp.snapshot()
		require.Len(t, starts, 1)
		assert.NotContains(t, starts[0].inline.ChangedFields, "skills")
		after := pendingReloadAgent(t, s, agent.ID)
		assert.Equal(t, []api.SkillReference{{URI: "hub://skills/review"}, {URI: "hub://skills/test"}}, after.AppliedConfig.InlineConfig.Skills)
		require.NotNil(t, after.PendingConfig)
		assert.Equal(t, []string{"config.skills"}, pendingConfigKeys(after.PendingConfig))
		effectAssertNoAppliedAudit(t, s, agent.ID)
	})
}
