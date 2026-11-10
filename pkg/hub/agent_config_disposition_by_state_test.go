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
	"errors"
	"net/http"
	"runtime"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestAgentConfigPatch_SystemPromptDispositionByProvisionState pins when a
// system prompt edit takes effect, as the PATCH disposition and the GET
// editability report it, by the agent's state and whether its broker
// re-renders provision-rendered keys at start (changedFields, as the
// embedded broker advertises), with held edits off and on
// (ptone/scion#7544).
//
//   - provisioned, never started: a provision-only create rendered the
//     prompt on the broker. The first start keeps it unless the broker
//     re-renders, so the edit waits for a reincarnation, or is applied on
//     a re-rendering broker.
//   - running: with held edits the edit is saved, held for the next start
//     on a re-rendering broker and for a reincarnation otherwise; without
//     them a config PATCH of a live agent is refused.
//   - stopped with a container: a start reuses the provisioned files, as
//     for a provisioned agent.
func TestAgentConfigPatch_SystemPromptDispositionByProvisionState(t *testing.T) {
	const prompt = "be brief"
	sp := []string{"config.system_prompt"}
	tests := []struct {
		name  string
		phase state.Phase
		// rerendering: the broker advertises changedFields.
		rerendering bool
		heldEdits   bool
		// wantCode is the PATCH status; on 200 the lists below apply.
		wantCode    int
		wantApplied []string
		wantHeld    []string
		wantHFR     []string
		// wantPending is whether the prompt is saved in the pending
		// config rather than written to the inline config.
		wantPending bool
		wantEdit    EditDisposition
	}{
		{name: "provisioned, never started", phase: state.PhaseCreated,
			wantCode: http.StatusOK, wantHFR: sp, wantEdit: EditReincarnate},
		{name: "provisioned, never started, held edits", phase: state.PhaseCreated, heldEdits: true,
			wantCode: http.StatusOK, wantHFR: sp, wantPending: true, wantEdit: EditReincarnate},
		{name: "provisioned, never started, re-rendering broker", phase: state.PhaseCreated, rerendering: true,
			wantCode: http.StatusOK, wantApplied: sp, wantEdit: EditNow},
		{name: "provisioned, never started, re-rendering broker, held edits", phase: state.PhaseCreated, rerendering: true, heldEdits: true,
			wantCode: http.StatusOK, wantApplied: sp, wantEdit: EditNow},
		{name: "running", phase: state.PhaseRunning,
			wantCode: http.StatusConflict, wantEdit: EditLocked},
		{name: "running, held edits", phase: state.PhaseRunning, heldEdits: true,
			wantCode: http.StatusOK, wantHFR: sp, wantPending: true, wantEdit: EditReincarnate},
		{name: "running, re-rendering broker", phase: state.PhaseRunning, rerendering: true,
			wantCode: http.StatusConflict, wantEdit: EditLocked},
		{name: "running, re-rendering broker, held edits", phase: state.PhaseRunning, rerendering: true, heldEdits: true,
			wantCode: http.StatusOK, wantHeld: sp, wantPending: true, wantEdit: EditHeld},
		{name: "stopped with container", phase: state.PhaseStopped,
			wantCode: http.StatusOK, wantHFR: sp, wantEdit: EditReincarnate},
		{name: "stopped with container, held edits", phase: state.PhaseStopped, heldEdits: true,
			wantCode: http.StatusOK, wantHFR: sp, wantPending: true, wantEdit: EditReincarnate},
		{name: "stopped with container, re-rendering broker", phase: state.PhaseStopped, rerendering: true,
			wantCode: http.StatusOK, wantApplied: sp, wantEdit: EditNow},
		{name: "stopped with container, re-rendering broker, held edits", phase: state.PhaseStopped, rerendering: true, heldEdits: true,
			wantCode: http.StatusOK, wantApplied: sp, wantEdit: EditNow},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			disp := &inlineCaptureDispatcher{reincarnateTestDispatcher: newReincarnateTestDispatcher()}
			srv, s, project, broker := setupReincarnateTestServer(t, disp)
			setHeldEditsExperiment(t, srv, tc.heldEdits)
			if tc.rerendering {
				setRerenderingBroker(t, s, broker)
			}
			agent := newPromptEditTestAgent(t, s, project, broker, tc.phase)

			assert.Equal(t, tc.wantEdit, getAgentEditability(t, srv, nil, agent.ID).Fields["config.system_prompt"].Disposition,
				"the editability agrees with the PATCH disposition")

			resp, code, body := patchAgentBody(t, srv, agent.ID, map[string]interface{}{
				"config": map[string]interface{}{"system_prompt": prompt},
			})
			require.Equal(t, tc.wantCode, code, body)
			if code != http.StatusOK {
				return
			}
			assert.Equal(t, nonNilStrings(tc.wantApplied), resp.Disposition.Applied)
			assert.Equal(t, nonNilStrings(tc.wantHeld), resp.Disposition.Held)
			assert.Equal(t, nonNilStrings(tc.wantHFR), resp.Disposition.HeldForReincarnate)
			if len(tc.wantHFR) == 0 {
				assert.Empty(t, resp.Warnings, "an edit the next start applies carries no reincarnation warning")
			}

			after := pendingReloadAgent(t, s, agent.ID)
			if tc.wantPending {
				require.NotNil(t, after.PendingConfig)
				assert.Equal(t, sp, pendingConfigKeys(after.PendingConfig))
			} else {
				assert.True(t, after.PendingConfig.IsEmpty(), "nothing is left pending")
				require.NotNil(t, after.AppliedConfig)
				require.NotNil(t, after.AppliedConfig.InlineConfig)
				assert.Equal(t, prompt, after.AppliedConfig.InlineConfig.SystemPrompt, "written to the inline config the next start sends")
			}
		})
	}
}

// TestAgentConfigPatch_BrokerLookupErrorWritesNothing: when the agent's
// broker record cannot be read, a config PATCH whose disposition depends on
// it answers 503 and writes nothing, instead of reporting an edit the
// broker applies at the next start as held for reincarnation
// (ptone/scion#7544). Without held edits, an edit whose disposition does
// not depend on the broker (a model change) needs no lookup and succeeds.
// With held edits the broker decides how the request and the pending keys
// are split, so every config PATCH needs it.
func TestAgentConfigPatch_BrokerLookupErrorWritesNothing(t *testing.T) {
	for _, heldEdits := range []bool{false, true} {
		name := "held edits off"
		if heldEdits {
			name = "held edits on"
		}
		t.Run(name, func(t *testing.T) {
			disp := &inlineCaptureDispatcher{reincarnateTestDispatcher: newReincarnateTestDispatcher()}
			srv, s, project, broker := setupReincarnateTestServer(t, disp)
			_, fault := installBrokerReadFault(t, srv)
			setHeldEditsExperiment(t, srv, heldEdits)
			setRerenderingBroker(t, s, broker)
			agent := newPromptEditTestAgent(t, s, project, broker, state.PhaseCreated)
			fault.Arm()

			_, code, body := patchAgentBody(t, srv, agent.ID, map[string]interface{}{
				"config": map[string]interface{}{"system_prompt": "be brief"},
			})
			require.Equal(t, http.StatusServiceUnavailable, code, body)
			assert.Contains(t, body, brokerUnavailableMessage)
			after := pendingReloadAgent(t, s, agent.ID)
			assert.Equal(t, agent.StateVersion, after.StateVersion, "nothing written")
			assert.Empty(t, after.AppliedConfig.InlineConfig.SystemPrompt)
			assert.True(t, after.PendingConfig.IsEmpty())

			_, code, body = patchAgentBody(t, srv, agent.ID, map[string]interface{}{
				"config": map[string]interface{}{"model": "other-model"},
			})
			if heldEdits {
				require.Equal(t, http.StatusServiceUnavailable, code, body)
				assert.Equal(t, agent.StateVersion, pendingReloadAgent(t, s, agent.ID).StateVersion, "nothing written")
			} else {
				require.Equal(t, http.StatusOK, code, body)
			}
		})
	}
}

// TestAgentPendingConfig_StartRefusedOnBrokerLookupError: a start that
// folds held edits while the agent's broker record cannot be read is
// refused before its dispatch, with a 503, and writes nothing
// (ptone/scion#7544). Without the refusal the fold would treat the broker
// as not re-rendering: the held system prompt would stay pending as
// reincarnation-only and the start would be dispatched without it.
func TestAgentPendingConfig_StartRefusedOnBrokerLookupError(t *testing.T) {
	disp := &inlineCaptureDispatcher{reincarnateTestDispatcher: newReincarnateTestDispatcher()}
	srv, s, project, broker := setupReincarnateTestServer(t, disp)
	_, fault := installStoreFault(t, srv, func(inner store.Store, f *storeFaultSwitch) *startSupportReadFaultStore {
		return &startSupportReadFaultStore{Store: inner, fault: f}
	})
	setHeldEditsExperiment(t, srv, true)
	setRerenderingBroker(t, s, broker)
	agent := newPromptEditTestAgent(t, s, project, broker, state.PhaseRunning)

	resp := patchPending(t, srv, agent.ID, map[string]interface{}{
		"config": map[string]interface{}{"system_prompt": "be brief"},
	})
	require.Equal(t, []string{"config.system_prompt"}, resp.Disposition.Held, "applied by the next start on this broker")
	pendingLifecycleWarnings(t, srv, agent.ID, "stop")
	before := pendingReloadAgent(t, s, agent.ID)
	require.Equal(t, string(state.PhaseStopped), before.Phase)
	pendingBefore, appliedBefore := gateARowSnapshot(t, s, agent.ID)

	fault.Arm()
	rec := gateAStart(t, srv, agent.ID)
	require.Equal(t, http.StatusServiceUnavailable, rec.Code, rec.Body.String())
	assert.Contains(t, rec.Body.String(), pendingFoldBrokerUnavailableMessage)
	assert.Empty(t, disp.snapshot(), "no dispatch")
	after := pendingReloadAgent(t, s, agent.ID)
	assert.Equal(t, before.StateVersion, after.StateVersion, "nothing written")
	require.NotNil(t, after.PendingConfig)
	assert.Equal(t, []string{"config.system_prompt"}, pendingConfigKeys(after.PendingConfig))
	pendingAfter, appliedAfter := gateARowSnapshot(t, s, agent.ID)
	assert.Equal(t, pendingBefore, pendingAfter)
	assert.Equal(t, appliedBefore, appliedAfter)
}

// startSupportReadFaultStore fails, once armed, the runtime broker reads
// made by agentBrokerStartSupport (and the strict read under it), and lets
// every other read through: the start's own placement check reads the same
// record before the fold, so a fault on every read would refuse the start
// before the fold runs.
type startSupportReadFaultStore struct {
	store.Store
	fault *storeFaultSwitch
}

func (f *startSupportReadFaultStore) GetRuntimeBroker(ctx context.Context, id string) (*store.RuntimeBroker, error) {
	if f.fault.Active() && calledFromAgentBrokerStartSupport() {
		return nil, errors.New("store unavailable")
	}
	return f.Store.GetRuntimeBroker(ctx, id)
}

// calledFromAgentBrokerStartSupport reports whether the caller's stack
// includes Server.agentBrokerStartSupport or Server.agentBrokerStartSupportStrict.
func calledFromAgentBrokerStartSupport() bool {
	pc := make([]uintptr, 64)
	frames := runtime.CallersFrames(pc[:runtime.Callers(2, pc)])
	for {
		fr, more := frames.Next()
		if strings.Contains(fr.Function, ".(*Server).agentBrokerStartSupport") {
			return true
		}
		if !more {
			return false
		}
	}
}

// newPromptEditTestAgent is an agent in phase with a stored inline config
// and no system prompt, on broker.
func newPromptEditTestAgent(t *testing.T, s store.Store, project *store.Project, broker *store.RuntimeBroker, phase state.Phase) *store.Agent {
	t.Helper()
	a := newReincarnateTestAgent(t, s, project, broker, func(a *store.Agent) {
		a.Phase = string(phase)
		a.AppliedConfig.Model = "old-model"
		a.AppliedConfig.InlineConfig = &api.ScionConfig{
			Harness:  "claude",
			Model:    "old-model",
			MaxTurns: api.IntPtr(3),
		}
	})
	return pendingReloadAgent(t, s, a.ID)
}

// nonNilStrings returns v, or an empty non-nil list for nil, matching the
// disposition's lists, which are never null.
func nonNilStrings(v []string) []string {
	if v == nil {
		return []string{}
	}
	return v
}
