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
	"log/slog"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Tests for ptone/scion#5605 on the hub: a change of a count limit to
// Unlimited (0) is accepted only for an agent whose broker advertises
// unlimitedCountLimits, so a disposition lists it as applied (or held) only
// when the next start applies it, on the live and the held path alike.

// setBrokerCapabilities replaces the broker's capabilities.
func setBrokerCapabilities(t *testing.T, s store.Store, brokerID string, caps *store.BrokerCapabilities) {
	t.Helper()
	ctx := context.Background()
	b, err := s.GetRuntimeBroker(ctx, brokerID)
	require.NoError(t, err)
	b.Capabilities = caps
	require.NoError(t, s.UpdateRuntimeBroker(ctx, b))
}

// TestAgentConfigPatch_UnlimitedCountByBroker pins the PATCH outcome of
// {"max_turns":0} on a stopped agent (live path) for each broker capability
// set, with the recorded cleared_fields, and checks the start dispatches
// what the disposition promised.
func TestAgentConfigPatch_UnlimitedCountByBroker(t *testing.T) {
	for _, tc := range []struct {
		name    string
		caps    *store.BrokerCapabilities
		applied bool
	}{
		{name: "no capabilities", caps: &store.BrokerCapabilities{Reprovision: true}},
		{name: "clearedFields", caps: &store.BrokerCapabilities{Reprovision: true, ClearedFields: true}},
		{name: "unlimitedCountLimits", caps: &store.BrokerCapabilities{Reprovision: true, ClearedFields: true, UnlimitedCountLimits: true}, applied: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			disp := &inlineCaptureDispatcher{reincarnateTestDispatcher: newReincarnateTestDispatcher()}
			srv, s, project, broker := setupReincarnateTestServer(t, disp)
			setBrokerCapabilities(t, s, broker.ID, tc.caps)
			agent := newEditTestAgent(t, s, project, broker, state.PhaseStopped)

			resp, code, body := patchAgentBody(t, srv, agent.ID, map[string]interface{}{
				"config": map[string]interface{}{"max_turns": 0},
			})
			stored := pendingReloadAgent(t, s, agent.ID)
			if !tc.applied {
				require.Equal(t, http.StatusUnprocessableEntity, code, body)
				assert.Contains(t, body, "unlimited_not_supported_by_broker")
				assert.Contains(t, body, "config.max_turns")
				assert.Equal(t, api.IntPtr(3), stored.AppliedConfig.InlineConfig.MaxTurns, "a refused PATCH writes nothing")
				assert.Equal(t, agent.StateVersion, stored.StateVersion)
				return
			}
			require.Equal(t, http.StatusOK, code, body)
			assert.Equal(t, []string{"config.max_turns"}, resp.Disposition.Applied)
			assert.Empty(t, resp.Disposition.HeldForReincarnate)
			assert.Empty(t, resp.Warnings)
			assert.Equal(t, api.IntPtr(0), stored.AppliedConfig.InlineConfig.MaxTurns)
			assert.NotContains(t, stored.AppliedConfig.InlineConfig.ClearedFields, "max_turns", "Unlimited is a value, not a clear")
			require.NotNil(t, stored.AppliedConfig.CreateInputs)
			require.NotNil(t, stored.AppliedConfig.CreateInputs.InlineConfig)
			assert.Equal(t, api.IntPtr(0), stored.AppliedConfig.CreateInputs.InlineConfig.MaxTurns, "a reincarnation replays Unlimited")

			// Resending the stored Unlimited is an echo: no change, no list.
			resp, code, body = patchAgentBody(t, srv, agent.ID, map[string]interface{}{
				"config": map[string]interface{}{"max_turns": 0},
			})
			require.Equal(t, http.StatusOK, code, body)
			assert.Empty(t, resp.Disposition.Applied)

			rec := doRequest(t, srv, http.MethodPost, "/api/v1/agents/"+agent.ID+"/start", nil)
			require.Less(t, rec.Code, 300, rec.Body.String())
			starts := disp.snapshot()
			require.Len(t, starts, 1)
			assert.Equal(t, api.IntPtr(0), starts[0].inline.MaxTurns, "the start dispatches Unlimited")
			assert.NotContains(t, starts[0].inline.ClearedFields, "max_turns")
		})
	}

	t.Run("a null clear keeps its meaning", func(t *testing.T) {
		disp := &inlineCaptureDispatcher{reincarnateTestDispatcher: newReincarnateTestDispatcher()}
		srv, s, project, broker := setupReincarnateTestServer(t, disp)
		setBrokerCapabilities(t, s, broker.ID, &store.BrokerCapabilities{Reprovision: true, ClearedFields: true})
		agent := newEditTestAgent(t, s, project, broker, state.PhaseStopped)
		resp, code, body := patchAgentBody(t, srv, agent.ID, map[string]interface{}{
			"config": map[string]interface{}{"max_turns": nil},
		})
		require.Equal(t, http.StatusOK, code, body)
		assert.Equal(t, []string{"config.max_turns"}, resp.Disposition.Applied)
		stored := pendingReloadAgent(t, s, agent.ID)
		assert.Nil(t, stored.AppliedConfig.InlineConfig.MaxTurns)
		assert.Contains(t, stored.AppliedConfig.InlineConfig.ClearedFields, "max_turns")
	})

	t.Run("no broker", func(t *testing.T) {
		disp := &inlineCaptureDispatcher{reincarnateTestDispatcher: newReincarnateTestDispatcher()}
		srv, s, project, broker := setupReincarnateTestServer(t, disp)
		agent := newReincarnateTestAgent(t, s, project, broker, func(a *store.Agent) {
			a.Phase = string(state.PhaseStopped)
			a.RuntimeBrokerID = ""
			a.AppliedConfig.InlineConfig = &api.ScionConfig{Harness: "claude", MaxTurns: api.IntPtr(3)}
		})
		_, code, body := patchAgentBody(t, srv, agent.ID, map[string]interface{}{
			"config": map[string]interface{}{"max_model_calls": 0},
		})
		require.Equal(t, http.StatusUnprocessableEntity, code, body)
		assert.Contains(t, body, "broker_unknown")
	})

	t.Run("negative is refused", func(t *testing.T) {
		disp := &inlineCaptureDispatcher{reincarnateTestDispatcher: newReincarnateTestDispatcher()}
		srv, s, project, broker := setupReincarnateTestServer(t, disp)
		agent := newEditTestAgent(t, s, project, broker, state.PhaseStopped)
		_, code, body := patchAgentBody(t, srv, agent.ID, map[string]interface{}{
			"config": map[string]interface{}{"max_turns": -1},
		})
		require.GreaterOrEqual(t, code, 400, body)
		require.Less(t, code, 500, body)
		assert.Contains(t, body, "max_turns")
	})
}

// TestAgentConfigPatch_HeldUnlimitedCount: with held edits on and a running
// agent, a change to Unlimited is refused on a broker without
// unlimitedCountLimits and held on one with it; the next start applies it.
func TestAgentConfigPatch_HeldUnlimitedCount(t *testing.T) {
	t.Run("old broker refuses", func(t *testing.T) {
		srv, s, _, agent := heldEditsFixture(t, state.PhaseRunning)
		_, code, body := patchAgentBody(t, srv, agent.ID, map[string]interface{}{
			"config": map[string]interface{}{"max_turns": 0},
		})
		require.Equal(t, http.StatusUnprocessableEntity, code, body)
		assert.Nil(t, pendingReloadAgent(t, s, agent.ID).PendingConfig, "a refused save holds nothing")
	})

	t.Run("capable broker holds, start applies", func(t *testing.T) {
		srv, s, disp, agent := heldEditsFixture(t, state.PhaseRunning)
		setBrokerCapabilities(t, s, agent.RuntimeBrokerID, &store.BrokerCapabilities{Reprovision: true, UnlimitedCountLimits: true})
		resp := patchPending(t, srv, agent.ID, map[string]interface{}{
			"config": map[string]interface{}{"max_turns": 0},
		})
		assert.Equal(t, []string{"config.max_turns"}, resp.Disposition.Held)
		require.NotNil(t, resp.PendingConfig)
		assert.Equal(t, "0", string(resp.PendingConfig.Config["max_turns"]))

		pendingLifecycleWarnings(t, srv, agent.ID, "stop")
		pendingLifecycleWarnings(t, srv, agent.ID, "start")
		starts := disp.snapshot()
		require.Len(t, starts, 1)
		assert.Equal(t, api.IntPtr(0), starts[0].inline.MaxTurns)
		assert.NotContains(t, starts[0].inline.ClearedFields, "max_turns", "Unlimited is a value, not a clear")
		assert.Nil(t, pendingReloadAgent(t, s, agent.ID).PendingConfig)
	})

	t.Run("clearedFields-only broker refuses", func(t *testing.T) {
		srv, s, _, agent := heldEditsFixture(t, state.PhaseRunning)
		setBrokerCapabilities(t, s, agent.RuntimeBrokerID, &store.BrokerCapabilities{Reprovision: true, ClearedFields: true})
		_, code, body := patchAgentBody(t, srv, agent.ID, map[string]interface{}{
			"config": map[string]interface{}{"max_turns": 0},
		})
		require.Equal(t, http.StatusUnprocessableEntity, code, body)
		assert.Nil(t, pendingReloadAgent(t, s, agent.ID).PendingConfig)
	})

	// A null clear held for a running agent keeps today's meaning: the
	// start records it in cleared_fields.
	t.Run("held null clear is recorded in cleared_fields", func(t *testing.T) {
		srv, s, disp, agent := heldEditsFixture(t, state.PhaseRunning)
		setBrokerCapabilities(t, s, agent.RuntimeBrokerID, &store.BrokerCapabilities{Reprovision: true, ClearedFields: true})
		resp := patchPending(t, srv, agent.ID, map[string]interface{}{
			"config": map[string]interface{}{"max_turns": nil},
		})
		assert.Equal(t, []string{"config.max_turns"}, resp.Disposition.Held)
		pendingLifecycleWarnings(t, srv, agent.ID, "stop")
		pendingLifecycleWarnings(t, srv, agent.ID, "start")
		starts := disp.snapshot()
		require.Len(t, starts, 1)
		assert.Nil(t, starts[0].inline.MaxTurns)
		assert.Contains(t, starts[0].inline.ClearedFields, "max_turns")
	})

	// Test (g): a fresh pending row (nothing pending before) stores 0 as
	// typed, so a reload still reads it as Unlimited.
	t.Run("fresh pending row keeps Unlimited across a reload", func(t *testing.T) {
		srv, s, _, agent := heldEditsFixture(t, state.PhaseRunning)
		setBrokerCapabilities(t, s, agent.RuntimeBrokerID, &store.BrokerCapabilities{Reprovision: true, UnlimitedCountLimits: true})
		patchPending(t, srv, agent.ID, map[string]interface{}{"config": map[string]interface{}{"max_turns": 0}})
		stored := pendingReloadAgent(t, s, agent.ID)
		require.NotNil(t, stored.PendingConfig)
		assert.Equal(t, "0", string(stored.PendingConfig.Config["max_turns"]))
		assert.Equal(t, store.PendingLimitsFormatTyped, stored.PendingConfig.LimitsFormat)
	})
}

// legacyPendingRow loads a pending row written before the count limits were
// typed, the way the store loads it, and stores it on the agent.
func legacyPendingRow(t *testing.T, s store.Store, agent *store.Agent, rawConfig string) *store.Agent {
	t.Helper()
	pc, err := store.UnmarshalPendingConfigFromStorage(`{"config":` + rawConfig + `,"savedAt":"2026-10-01T00:00:00Z","baseGeneration":1,"baseStateVersion":1}`)
	require.NoError(t, err)
	stored := pendingReloadAgent(t, s, agent.ID)
	_, err = s.UpdateAgentConfig(context.Background(), agent.ID, stored.StateVersion, stored.AppliedConfig, pc)
	require.NoError(t, err)
	return pendingReloadAgent(t, s, agent.ID)
}

// TestAgentPendingConfig_LegacyCountClear: a legacy pending count limit of
// 0 was a clear, and stays one through a later held save of another key
// and the next start; a PATCH of 0 over it is a change to Unlimited, not an
// echo, so an old broker refuses it.
func TestAgentPendingConfig_LegacyCountClear(t *testing.T) {
	t.Run("held save of another key keeps the clear", func(t *testing.T) {
		srv, s, disp, agent := heldEditsFixture(t, state.PhaseRunning)
		agent = legacyPendingRow(t, s, agent, `{"max_turns":0}`)
		patchPending(t, srv, agent.ID, map[string]interface{}{"config": map[string]interface{}{"model": "x"}})
		stored := pendingReloadAgent(t, s, agent.ID)
		require.NotNil(t, stored.PendingConfig)
		assert.Equal(t, "null", string(stored.PendingConfig.Config["max_turns"]))
		assert.Equal(t, store.PendingLimitsFormatTyped, stored.PendingConfig.LimitsFormat)

		pendingLifecycleWarnings(t, srv, agent.ID, "stop")
		pendingLifecycleWarnings(t, srv, agent.ID, "start")
		starts := disp.snapshot()
		require.Len(t, starts, 1)
		assert.Nil(t, starts[0].inline.MaxTurns, "the legacy 0 folds as a clear, not as Unlimited")
		assert.Equal(t, "x", starts[0].inline.Model)
	})

	t.Run("PATCH of 0 over it is a change", func(t *testing.T) {
		srv, s, _, agent := heldEditsFixture(t, state.PhaseRunning)
		agent = legacyPendingRow(t, s, agent, `{"max_turns":0}`)
		_, code, body := patchAgentBody(t, srv, agent.ID, map[string]interface{}{"config": map[string]interface{}{"max_turns": 0}})
		require.Equal(t, http.StatusUnprocessableEntity, code, body)
	})

	// With held edits off, pending rows are inert at every start, so the
	// change check compares with the applied config alone: a leftover
	// pending Unlimited does not make a change look like an echo.
	t.Run("flag off: a leftover pending Unlimited is not the base", func(t *testing.T) {
		srv, s, _, agent := heldEditsFixture(t, state.PhaseRunning)
		setBrokerCapabilities(t, s, agent.RuntimeBrokerID, &store.BrokerCapabilities{Reprovision: true, UnlimitedCountLimits: true})
		patchPending(t, srv, agent.ID, map[string]interface{}{"config": map[string]interface{}{"max_turns": 0}})
		setBrokerCapabilities(t, s, agent.RuntimeBrokerID, &store.BrokerCapabilities{Reprovision: true})
		setHeldEditsExperiment(t, srv, false)
		row := pendingReloadAgent(t, s, agent.ID)
		row.Phase = string(state.PhaseStopped)
		require.NoError(t, s.UpdateAgent(context.Background(), row))
		_, code, body := patchAgentBody(t, srv, agent.ID, map[string]interface{}{"config": map[string]interface{}{"max_turns": 0}})
		require.Equal(t, http.StatusUnprocessableEntity, code, body)
	})
}

// TestAgentPendingConfig_UnlimitedWaitsForCapableBroker: a pending
// Unlimited saved while the broker advertised unlimitedCountLimits stays
// pending when the broker no longer does, at the start fold (a start
// warning, no applied audit) and at the fold of a PATCH to an agent with no
// container (a PATCH warning); other pending keys are applied.
func TestAgentPendingConfig_UnlimitedWaitsForCapableBroker(t *testing.T) {
	// setup saves a pending Unlimited while the broker can apply it, then
	// takes the capability away. beforeRequests, when set, runs on the new
	// server before any request (to install a store wrapper race-free).
	setup := func(t *testing.T, beforeRequests func(*Server)) (*Server, store.Store, *inlineCaptureDispatcher, *store.Agent) {
		srv, s, disp, agent := heldEditsFixture(t, state.PhaseRunning)
		if beforeRequests != nil {
			beforeRequests(srv)
		}
		setBrokerCapabilities(t, s, agent.RuntimeBrokerID, &store.BrokerCapabilities{Reprovision: true, UnlimitedCountLimits: true})
		patchPending(t, srv, agent.ID, map[string]interface{}{"config": map[string]interface{}{"max_turns": 0, "model": "held-model"}})
		setBrokerCapabilities(t, s, agent.RuntimeBrokerID, &store.BrokerCapabilities{Reprovision: true})
		return srv, s, disp, agent
	}

	t.Run("start fold", func(t *testing.T) {
		// Count the fold's compare-and-set writes (UpdateAgentConfig), so the
		// test tells the early return from an idempotent write. Installed
		// before any request, so no background write races the swap.
		var writes *agentConfigWriteCounter
		srv, s, disp, agent := setup(t, func(srv *Server) {
			writes, _ = installStoreFault(t, srv, func(inner store.Store, _ *storeFaultSwitch) *agentConfigWriteCounter {
				return &agentConfigWriteCounter{Store: inner}
			})
		})

		pendingLifecycleWarnings(t, srv, agent.ID, "stop")
		before := writes.n.Load()
		warnings := pendingLifecycleWarnings(t, srv, agent.ID, "start")
		assert.Equal(t, int64(1), writes.n.Load()-before, "the first start folds model in one compare-and-set write (positive control for the counter)")
		starts := disp.snapshot()
		require.Len(t, starts, 1)
		assert.Equal(t, "held-model", starts[0].inline.Model)
		assert.Equal(t, api.IntPtr(3), starts[0].inline.MaxTurns, "the start keeps the limit it can apply")
		stored := pendingReloadAgent(t, s, agent.ID)
		require.NotNil(t, stored.PendingConfig)
		assert.Equal(t, "0", string(stored.PendingConfig.Config["max_turns"]))
		assert.NotContains(t, stored.PendingConfig.Config, "model")
		assert.True(t, containsSubstring(warnings, "config.max_turns: Unlimited waits for a runtime broker that supports it"), "%v", warnings)
		// A waiting Unlimited is not reincarnation-only: the start's
		// held-for-reincarnation warning leaves config.max_turns out (and,
		// with nothing else left pending, is not sent at all).
		assertNoHeldForReincarnate(t, warnings, "max turns")
		applied := pendingAudits(t, s, agent.ID, mutationTypeAgentPendingConfigApplied, 1)
		keys, _ := pendingAuditSummary(t, applied[0])
		assert.Equal(t, []string{"config.model"}, keys)

		// A second start with only the waiting Unlimited pending takes the
		// fold's early return: nothing is applied or written, the waiting
		// warning is repeated, and max_turns stays out of the
		// held-for-reincarnation warning.
		pendingLifecycleWarnings(t, srv, agent.ID, "stop")
		before = writes.n.Load()
		warnings = pendingLifecycleWarnings(t, srv, agent.ID, "start")
		assert.Equal(t, int64(0), writes.n.Load()-before, "the early return makes no compare-and-set write")
		starts = disp.snapshot()
		require.Len(t, starts, 2)
		assert.Equal(t, "held-model", starts[1].inline.Model)
		assert.Equal(t, api.IntPtr(3), starts[1].inline.MaxTurns, "the second start still keeps the limit it can apply")
		assert.True(t, containsSubstring(warnings, "config.max_turns: Unlimited waits for a runtime broker that supports it"), "%v", warnings)
		assertNoHeldForReincarnate(t, warnings, "max turns")
		again := pendingReloadAgent(t, s, agent.ID)
		require.NotNil(t, again.PendingConfig)
		assert.Equal(t, stored.PendingConfig.Config, again.PendingConfig.Config, "the early return writes no pending change")
		assert.Equal(t, api.IntPtr(3), again.AppliedConfig.InlineConfig.MaxTurns)
		// Audits are written asynchronously, so wait out a window in which
		// a second "applied" record would land, rather than reading once.
		assert.Never(t, func() bool {
			recs, _, err := s.ListMutationAudits(context.Background(), store.MutationAuditFilter{
				MutationType: mutationTypeAgentPendingConfigApplied, TargetID: agent.ID, Limit: 20,
			})
			return err != nil || len(recs) != 1
		}, 500*time.Millisecond, 20*time.Millisecond, "the early return records no application (exactly one applied audit)")
	})

	t.Run("no-container PATCH fold", func(t *testing.T) {
		srv, s, _, agent := setup(t, nil)
		row := pendingReloadAgent(t, s, agent.ID)
		row.Phase = string(state.PhaseStopped)
		require.NoError(t, s.UpdateAgent(context.Background(), row))
		resp := patchPending(t, srv, agent.ID, map[string]interface{}{"config": map[string]interface{}{"system_prompt": "p"}})
		assert.True(t, containsSubstring(resp.Warnings, "config.max_turns: Unlimited waits for a runtime broker that supports it"), "%v", resp.Warnings)
		stored := pendingReloadAgent(t, s, agent.ID)
		require.NotNil(t, stored.PendingConfig)
		assert.Equal(t, "0", string(stored.PendingConfig.Config["max_turns"]))
		assert.Equal(t, api.IntPtr(3), stored.AppliedConfig.InlineConfig.MaxTurns)
		assert.Equal(t, "held-model", stored.AppliedConfig.InlineConfig.Model, "the other pending keys are applied")
	})
}

// TestAgentEditability_UnlimitedUnsupported: the GET editability flags the
// count limits when the broker cannot apply Unlimited.
func TestAgentEditability_UnlimitedUnsupported(t *testing.T) {
	disp := &inlineCaptureDispatcher{reincarnateTestDispatcher: newReincarnateTestDispatcher()}
	srv, s, project, broker := setupReincarnateTestServer(t, disp)
	agent := newEditTestAgent(t, s, project, broker, state.PhaseStopped)
	ed := getAgentEditability(t, srv, nil, agent.ID)
	assert.True(t, ed.Fields["config.max_turns"].UnlimitedUnsupported)
	assert.True(t, ed.Fields["config.max_model_calls"].UnlimitedUnsupported)
	assert.False(t, ed.Fields["config.max_duration"].UnlimitedUnsupported)

	setBrokerCapabilities(t, s, broker.ID, &store.BrokerCapabilities{Reprovision: true, UnlimitedCountLimits: true})
	ed = getAgentEditability(t, srv, nil, agent.ID)
	assert.False(t, ed.Fields["config.max_turns"].UnlimitedUnsupported)
	assert.False(t, ed.Fields["config.max_model_calls"].UnlimitedUnsupported)
}

// TestAgentConfigPatch_RetiredConfigureBodyMeansUnlimited: the retired
// Configure page sent "max_turns": 0 and "max_model_calls": 0 for untouched
// limits. Since count limits are typed that body means Unlimited
// (accepted, ptone/scion#5605): refused whole on a broker that cannot apply
// it, applied on one that can, and held for a running agent with held edits
// on.
func TestAgentConfigPatch_RetiredConfigureBodyMeansUnlimited(t *testing.T) {
	for _, name := range []string{"configure-untouched-body.json", "configure-row-edit-body.json"} {
		t.Run(name, func(t *testing.T) {
			t.Run("old broker", func(t *testing.T) {
				disp := &inlineCaptureDispatcher{reincarnateTestDispatcher: newReincarnateTestDispatcher()}
				srv, s, project, broker := setupReincarnateTestServer(t, disp)
				agent := newEditTestAgent(t, s, project, broker, state.PhaseStopped)
				_, code, body := patchAgentBody(t, srv, agent.ID, map[string]interface{}{"config": configureBodyFull(t, name)})
				require.Equal(t, http.StatusUnprocessableEntity, code, body)
				assert.Contains(t, body, "reload the page")
			})
			t.Run("capable broker", func(t *testing.T) {
				disp := &inlineCaptureDispatcher{reincarnateTestDispatcher: newReincarnateTestDispatcher()}
				srv, s, project, broker := setupReincarnateTestServer(t, disp)
				setBrokerCapabilities(t, s, broker.ID, &store.BrokerCapabilities{Reprovision: true, UnlimitedCountLimits: true})
				agent := newEditTestAgent(t, s, project, broker, state.PhaseStopped)
				resp, code, body := patchAgentBody(t, srv, agent.ID, map[string]interface{}{"config": configureBodyFull(t, name)})
				require.Equal(t, http.StatusOK, code, body)
				assert.Contains(t, resp.Disposition.Applied, "config.max_turns")
				assert.Contains(t, resp.Disposition.Applied, "config.max_model_calls")
				stored := pendingReloadAgent(t, s, agent.ID)
				assert.Equal(t, api.IntPtr(0), stored.AppliedConfig.InlineConfig.MaxTurns)
				assert.Equal(t, api.IntPtr(0), stored.AppliedConfig.InlineConfig.MaxModelCalls)
			})
			t.Run("running agent, held edits", func(t *testing.T) {
				srv, s, disp, agent := heldEditsFixture(t, state.PhaseRunning)
				setBrokerCapabilities(t, s, agent.RuntimeBrokerID, &store.BrokerCapabilities{Reprovision: true, UnlimitedCountLimits: true})
				resp := patchPending(t, srv, agent.ID, map[string]interface{}{"config": configureBodyFull(t, name)})
				assert.Contains(t, resp.Disposition.Held, "config.max_turns")
				assert.Contains(t, resp.Disposition.Held, "config.max_model_calls")
				pendingLifecycleWarnings(t, srv, agent.ID, "stop")
				pendingLifecycleWarnings(t, srv, agent.ID, "start")
				starts := disp.snapshot()
				require.Len(t, starts, 1)
				assert.Equal(t, api.IntPtr(0), starts[0].inline.MaxTurns)
				assert.Equal(t, api.IntPtr(0), starts[0].inline.MaxModelCalls)
			})
		})
	}
}

// assertNoHeldForReincarnate checks that no warning is the
// held-for-reincarnation one, so none names label (the warning's form of a
// key, e.g. "max turns" for config.max_turns). The positive control builds
// that warning for label with the production formatter, so a change of its
// form fails here instead of letting the check pass vacuously.
func assertNoHeldForReincarnate(t *testing.T, warnings []string, label string) {
	t.Helper()
	require.Contains(t, heldForReincarnateWarning([]string{"config." + strings.ReplaceAll(label, " ", "_")}), heldReincarnateWarningPrefix+label)
	assert.False(t, containsSubstring(warnings, heldReincarnateWarningPrefix), "%v", warnings)
}

// agentConfigWriteCounter counts UpdateAgentConfig calls (the pending
// fold's and the discard's compare-and-set write) and changes nothing else.
type agentConfigWriteCounter struct {
	store.Store
	n atomic.Int64
}

func (c *agentConfigWriteCounter) UpdateAgentConfig(ctx context.Context, agentID string, expectedVersion int64, applied *store.AgentAppliedConfig, pending *store.AgentPendingConfig) (int64, error) {
	c.n.Add(1)
	return c.Store.UpdateAgentConfig(ctx, agentID, expectedVersion, applied, pending)
}

// containsSubstring reports whether any of list contains sub.
func containsSubstring(list []string, sub string) bool {
	for _, s := range list {
		if strings.Contains(s, sub) {
			return true
		}
	}
	return false
}

// waitingUnlimitedFixture is a stopped agent with held edits on and a
// pending max_turns of 0 saved while its broker advertised
// unlimitedCountLimits, on a broker that no longer does: the pending 0
// waits (foldKeep).
func waitingUnlimitedFixture(t *testing.T) (*Server, store.Store, *inlineCaptureDispatcher, *store.Agent) {
	t.Helper()
	srv, s, disp, agent := heldEditsFixture(t, state.PhaseRunning)
	setBrokerCapabilities(t, s, agent.RuntimeBrokerID, &store.BrokerCapabilities{Reprovision: true, UnlimitedCountLimits: true})
	patchPending(t, srv, agent.ID, map[string]interface{}{"config": map[string]interface{}{"max_turns": 0}})
	setBrokerCapabilities(t, s, agent.RuntimeBrokerID, &store.BrokerCapabilities{Reprovision: true})
	row := pendingReloadAgent(t, s, agent.ID)
	row.Phase = string(state.PhaseStopped)
	require.NoError(t, s.UpdateAgent(context.Background(), row))
	return srv, s, disp, pendingReloadAgent(t, s, agent.ID)
}

// TestAgentPendingConfig_WaitingUnlimitedSupersededByWrite: a PATCH of an
// agent with no container that writes a count limit now removes a waiting
// pending Unlimited of that key (audited superseded), so the older pending
// 0 is never applied over the newer value, even after the broker gains the
// capability.
func TestAgentPendingConfig_WaitingUnlimitedSupersededByWrite(t *testing.T) {
	srv, s, disp, agent := waitingUnlimitedFixture(t)

	resp := patchPending(t, srv, agent.ID, map[string]interface{}{"config": map[string]interface{}{"max_turns": 5}})
	assert.Equal(t, []string{"config.max_turns"}, resp.Disposition.Applied)
	assert.False(t, containsSubstring(resp.Warnings, "Unlimited waits"), "%v", resp.Warnings)
	stored := pendingReloadAgent(t, s, agent.ID)
	assert.Equal(t, api.IntPtr(5), stored.AppliedConfig.InlineConfig.MaxTurns)
	if stored.PendingConfig != nil {
		assert.NotContains(t, stored.PendingConfig.Config, "max_turns")
	}
	discarded := pendingAudits(t, s, agent.ID, mutationTypeAgentPendingConfigDiscarded, 1)
	keys, trigger := pendingAuditSummary(t, discarded[0])
	assert.Equal(t, []string{"config.max_turns"}, keys)
	assert.Equal(t, pendingDiscardTriggerSuperseded, trigger)

	setBrokerCapabilities(t, s, agent.RuntimeBrokerID, &store.BrokerCapabilities{Reprovision: true, UnlimitedCountLimits: true})
	pendingLifecycleWarnings(t, srv, agent.ID, "start")
	starts := disp.snapshot()
	require.Len(t, starts, 1)
	assert.Equal(t, api.IntPtr(5), starts[0].inline.MaxTurns, "the newer value is dispatched, not the stale pending 0")
}

// TestAgentPendingConfig_WaitingUnlimitedIsNotTheBase: a pending 0 waiting
// for a capable broker is not the next run's value, so a PATCH of 0 on a
// stopped agent is a change and an old broker refuses it, instead of the
// 0 being written to the applied config and reported applied.
func TestAgentPendingConfig_WaitingUnlimitedIsNotTheBase(t *testing.T) {
	srv, s, _, agent := waitingUnlimitedFixture(t)
	before := pendingReloadAgent(t, s, agent.ID)

	_, code, body := patchAgentBody(t, srv, agent.ID, map[string]interface{}{"config": map[string]interface{}{"max_turns": 0}})
	require.Equal(t, http.StatusUnprocessableEntity, code, body)
	assert.Contains(t, body, "unlimited_not_supported_by_broker")
	after := pendingReloadAgent(t, s, agent.ID)
	assert.Equal(t, api.IntPtr(3), after.AppliedConfig.InlineConfig.MaxTurns, "nothing written")
	assert.Equal(t, before.StateVersion, after.StateVersion)
}

// TestHTTPAgentDispatcher_UnlimitedWarningOnCreateAndStart: a create or a
// start whose inline config sets a count limit to 0, dispatched to a broker
// without unlimitedCountLimits, carries the warning in its dispatch
// warnings (the create and start responses); a capable broker gets none.
func TestHTTPAgentDispatcher_UnlimitedWarningOnCreateAndStart(t *testing.T) {
	ctx := context.Background()
	memStore := createTestStore(t)
	broker := &store.RuntimeBroker{
		ID: tid("broker-unl"), Name: "unl-broker", Slug: "unl-broker",
		Endpoint: "http://localhost:9800", Status: store.BrokerStatusOnline,
	}
	require.NoError(t, memStore.CreateRuntimeBroker(ctx, broker))
	project := &store.Project{ID: tid("project-unl"), Name: "unl-project", Slug: "unl-project"}
	require.NoError(t, memStore.CreateProject(ctx, project))
	require.NoError(t, memStore.AddProjectProvider(ctx, &store.ProjectProvider{
		ProjectID: project.ID, BrokerID: broker.ID, BrokerName: broker.Name,
		LocalPath: "/home/user/project/.scion", Status: store.BrokerStatusOnline,
	}))
	dispatcher := NewHTTPAgentDispatcherWithClient(memStore, &mockRuntimeBrokerClient{}, false, slog.Default())
	agent := &store.Agent{
		ID: "agent-unl", Name: "unl-agent", Slug: "unl-agent",
		ProjectID: project.ID, RuntimeBrokerID: broker.ID,
		AppliedConfig: &store.AgentAppliedConfig{
			HarnessConfig: "claude",
			InlineConfig:  &api.ScionConfig{MaxTurns: api.IntPtr(0)},
		},
	}
	const want = "config.max_turns: this agent's runtime broker keeps the inherited limit"

	wctx, warns := withDispatchWarnings(ctx)
	_, err := dispatcher.DispatchAgentCreate(wctx, agent)
	require.NoError(t, err)
	assert.True(t, containsSubstring(warns.Warnings(), want), "create: %v", warns.Warnings())

	wctx, warns = withDispatchWarnings(ctx)
	require.NoError(t, dispatcher.DispatchAgentStart(wctx, agent, "", false))
	assert.True(t, containsSubstring(warns.Warnings(), want), "start: %v", warns.Warnings())

	broker.Capabilities = &store.BrokerCapabilities{UnlimitedCountLimits: true}
	require.NoError(t, memStore.UpdateRuntimeBroker(ctx, broker))
	wctx, warns = withDispatchWarnings(ctx)
	require.NoError(t, dispatcher.DispatchAgentStart(wctx, agent, "", false))
	assert.False(t, containsSubstring(warns.Warnings(), want), "capable broker: %v", warns.Warnings())
}

// TestAgentConfigPatch_UnlimitedLookupErrorWritesNothing: a PATCH that
// changes a count limit to 0 while the broker record cannot be read answers
// 503 and writes nothing; an edit that sets no count limit to 0 still
// succeeds, since it needs no lookup.
func TestAgentConfigPatch_UnlimitedLookupErrorWritesNothing(t *testing.T) {
	disp := &inlineCaptureDispatcher{reincarnateTestDispatcher: newReincarnateTestDispatcher()}
	srv, s, project, broker := setupReincarnateTestServer(t, disp)
	_, fault := installBrokerReadFault(t, srv)
	agent := newEditTestAgent(t, s, project, broker, state.PhaseStopped)
	fault.Arm()

	_, code, body := patchAgentBody(t, srv, agent.ID, map[string]interface{}{"config": map[string]interface{}{"max_turns": 0}})
	require.Equal(t, http.StatusServiceUnavailable, code, body)
	after := pendingReloadAgent(t, s, agent.ID)
	assert.Equal(t, api.IntPtr(3), after.AppliedConfig.InlineConfig.MaxTurns)
	assert.Equal(t, agent.StateVersion, after.StateVersion, "nothing written")

	_, code, body = patchAgentBody(t, srv, agent.ID, map[string]interface{}{"config": map[string]interface{}{"model": "other-model"}})
	require.Equal(t, http.StatusOK, code, body)
}

// TestAgentConfigPatch_UnlimitedEchoOnOldBroker: an agent whose stored
// count limit is already 0 (from create or a template) on a broker without
// unlimitedCountLimits can still be edited: resending the 0 with another
// change is an echo, not refused.
func TestAgentConfigPatch_UnlimitedEchoOnOldBroker(t *testing.T) {
	disp := &inlineCaptureDispatcher{reincarnateTestDispatcher: newReincarnateTestDispatcher()}
	srv, s, project, broker := setupReincarnateTestServer(t, disp)
	agent := newReincarnateTestAgent(t, s, project, broker, func(a *store.Agent) {
		a.Phase = string(state.PhaseStopped)
		a.AppliedConfig.InlineConfig = &api.ScionConfig{Harness: "claude", MaxTurns: api.IntPtr(0)}
	})
	resp, code, body := patchAgentBody(t, srv, agent.ID, map[string]interface{}{
		"config": map[string]interface{}{"max_turns": 0, "max_duration": "1h"},
	})
	require.Equal(t, http.StatusOK, code, body)
	assert.Equal(t, []string{"config.max_duration"}, resp.Disposition.Applied)
	after := pendingReloadAgent(t, s, agent.ID)
	assert.Equal(t, api.IntPtr(0), after.AppliedConfig.InlineConfig.MaxTurns)
	assert.Equal(t, "1h", after.AppliedConfig.InlineConfig.MaxDuration)
}

// simulateBrokerStart is what the agent's Runtime Broker does with a
// dispatched inline config at start, by the shared rules: an older broker
// (no unlimitedCountLimits) decodes a count limit of 0 as unset; the start
// merge (config.MergeScionConfig) lays the inline config over the agent's
// persisted config; a broker with clearedFields restores each cleared key
// from the template chain (inherit); then the default limits fill what is
// still unset (config.FillDefaultLimits). It returns the effective config.
func simulateBrokerStart(persisted *api.ScionConfig, inline api.ScionConfig, caps store.BrokerCapabilities, inherit *api.ScionConfig, hub *api.HubAgentDefaults) *api.ScionConfig {
	in := inline
	if !caps.UnlimitedCountLimits {
		if api.IsUnlimitedCount(in.MaxTurns) {
			in.MaxTurns = nil
		}
		if api.IsUnlimitedCount(in.MaxModelCalls) {
			in.MaxModelCalls = nil
		}
	}
	merged := config.MergeScionConfig(persisted, &in)
	if caps.ClearedFields {
		api.ApplyClearedFields(merged, in.ClearedFields, inherit)
	}
	out := *merged
	config.FillDefaultLimits(&out, hub, 0, 0, "")
	return &out
}

// TestAgentConfigPatch_CountLimitDispositionInvariant is the design's
// invariant table for the count limits (ptone/scion#5605): for each edit
// (Unlimited, a null clear, a value) and broker capability set, on the
// live path (stopped agent) and the held path (running agent, held edits
// on), the PATCH lists config.max_turns under applied (live) or held
// (held) exactly when the next start, simulated by the shared merge and
// default rules, yields the value the edit asked for. A refused edit lists
// nothing. The agent's own limit is 3, its template's 5 and the hub
// default 9.
func TestAgentConfigPatch_CountLimitDispositionInvariant(t *testing.T) {
	inherit := &api.ScionConfig{MaxTurns: api.IntPtr(5)}
	hub := &api.HubAgentDefaults{MaxTurns: 9}
	persisted := &api.ScionConfig{MaxTurns: api.IntPtr(3)}
	edits := []struct {
		name  string
		value interface{}
		want  *int // the effective limit the edit asks for
	}{
		{name: "unlimited", value: 0, want: api.IntPtr(0)},
		{name: "null clear", value: nil, want: api.IntPtr(5)},
		{name: "value", value: 7, want: api.IntPtr(7)},
	}
	capSets := []struct {
		name string
		caps store.BrokerCapabilities
	}{
		{name: "none", caps: store.BrokerCapabilities{Reprovision: true}},
		{name: "clearedFields", caps: store.BrokerCapabilities{Reprovision: true, ClearedFields: true}},
		{name: "unlimitedCountLimits", caps: store.BrokerCapabilities{Reprovision: true, ClearedFields: true, UnlimitedCountLimits: true}},
	}
	for _, held := range []bool{false, true} {
		path := "live"
		if held {
			path = "held"
		}
		for _, e := range edits {
			for _, c := range capSets {
				t.Run(path+"/"+e.name+"/"+c.name, func(t *testing.T) {
					disp := &inlineCaptureDispatcher{reincarnateTestDispatcher: newReincarnateTestDispatcher()}
					srv, s, project, broker := setupReincarnateTestServer(t, disp)
					caps := c.caps
					setBrokerCapabilities(t, s, broker.ID, &caps)
					phase := state.PhaseStopped
					if held {
						setHeldEditsExperiment(t, srv, true)
						phase = state.PhaseRunning
					}
					agent := newEditTestAgent(t, s, project, broker, phase)

					resp, code, body := patchAgentBody(t, srv, agent.ID, map[string]interface{}{
						"config": map[string]interface{}{"max_turns": e.value},
					})
					if code == http.StatusUnprocessableEntity {
						assert.Equal(t, 0, e.value, "only Unlimited is refused: %s", body)
						assert.False(t, c.caps.UnlimitedCountLimits, "a capable broker never refuses Unlimited")
						return
					}
					require.Equal(t, http.StatusOK, code, body)
					list := resp.Disposition.Applied
					if held {
						list = resp.Disposition.Held
						pendingLifecycleWarnings(t, srv, agent.ID, "stop")
					}
					listed := false
					for _, k := range list {
						listed = listed || k == "config.max_turns"
					}
					pendingLifecycleWarnings(t, srv, agent.ID, "start")
					starts := disp.snapshot()
					require.NotEmpty(t, starts)
					effective := simulateBrokerStart(persisted, starts[len(starts)-1].inline, c.caps, inherit, hub)
					assert.Equal(t, api.IntPtrEqual(effective.MaxTurns, e.want), listed,
						"listed=%v, but the start yields %s for a requested %s", listed,
						api.FormatCountLimit(effective.MaxTurns), api.FormatCountLimit(e.want))
				})
			}
		}
	}
}

// TestCreateAgent_RefusesNegativeCountLimit: a create whose config sets a
// count limit below 0 is refused (400 validation_error naming the field)
// and creates nothing; 0 (no limit) is accepted (ptone/scion#5605).
func TestCreateAgent_RefusesNegativeCountLimit(t *testing.T) {
	for _, tc := range []struct {
		name  string
		cfg   *api.ScionConfig
		field string
	}{
		{name: "max_turns", cfg: &api.ScionConfig{MaxTurns: api.IntPtr(-1)}, field: "max_turns"},
		{name: "max_model_calls", cfg: &api.ScionConfig{MaxModelCalls: api.IntPtr(-5)}, field: "max_model_calls"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			disp := &createAgentDispatcher{createPhase: string(state.PhaseRunning)}
			srv, s, project := setupCreateAgentServer(t, disp)
			rec := doRequest(t, srv, http.MethodPost, "/api/v1/agents", CreateAgentRequest{
				Name: "negative-limit-agent", ProjectID: project.ID, Config: tc.cfg,
			})
			require.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
			assert.Contains(t, rec.Body.String(), tc.field)
			_, err := s.GetAgentBySlug(context.Background(), project.ID, "negative-limit-agent")
			assert.ErrorIs(t, err, store.ErrNotFound, "a refused create stores nothing")
		})
	}

	t.Run("zero is accepted", func(t *testing.T) {
		disp := &createAgentDispatcher{createPhase: string(state.PhaseRunning)}
		srv, _, project := setupCreateAgentServer(t, disp)
		rec := doRequest(t, srv, http.MethodPost, "/api/v1/agents", CreateAgentRequest{
			Name: "unlimited-agent", ProjectID: project.ID,
			Config: &api.ScionConfig{MaxTurns: api.IntPtr(0)},
		})
		require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	})
}
