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
	"net/http"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Tests for ptone/scion#5864: with hub.agent_edit_held on, a held edit
// stays pending until it takes effect. A start applies only the pending
// keys it brings into effect; a key only a reincarnation brings into
// effect stays pending, whole, and is not written into the applied config
// or the create inputs.

// effectAssertNoAppliedAudit waits out a window in which an asynchronous
// agent_pending_config_applied record for agentID would land, and fails if
// one does.
func effectAssertNoAppliedAudit(t *testing.T, s store.Store, agentID string) {
	t.Helper()
	assert.Never(t, func() bool {
		recs, _, err := s.ListMutationAudits(context.Background(), store.MutationAuditFilter{
			MutationType: mutationTypeAgentPendingConfigApplied, TargetID: agentID, Limit: 5,
		})
		return err != nil || len(recs) > 0
	}, 500*time.Millisecond, 20*time.Millisecond, "no agent_pending_config_applied record")
}

// effectSetCreateInputsMaxTurns records an explicit max_turns of n in the
// agent's create inputs, so a test can tell whether an edit was recorded
// there.
func effectSetCreateInputsMaxTurns(t *testing.T, s store.Store, agentID string, n int) {
	t.Helper()
	a, err := s.GetAgent(context.Background(), agentID)
	require.NoError(t, err)
	a.AppliedConfig.CreateInputs = &store.AgentCreateInputs{InlineConfig: &api.ScionConfig{MaxTurns: api.IntPtr(n)}}
	require.NoError(t, s.UpdateAgent(context.Background(), a))
}

// effectStopStart stops and starts the agent and returns the start's
// warnings.
func effectStopStart(t *testing.T, srv *Server, agentID string) []string {
	t.Helper()
	pendingLifecycleWarnings(t, srv, agentID, "stop")
	return pendingLifecycleWarnings(t, srv, agentID, "start")
}

// effectSetPhase writes the agent's phase directly.
func effectSetPhase(t *testing.T, s store.Store, agentID string, phase state.Phase) {
	t.Helper()
	a, err := s.GetAgent(context.Background(), agentID)
	require.NoError(t, err)
	a.Phase = string(phase)
	require.NoError(t, s.UpdateAgent(context.Background(), a))
}

// TestPendingFold_CountClearOldBrokerStaysPending (C1): a held count-limit
// clear on a broker without clearedFields stays pending through a start:
// the start keeps the limit, records no application and names the key.
func TestPendingFold_CountClearOldBrokerStaysPending(t *testing.T) {
	srv, s, disp, agent := heldEditsFixture(t, state.PhaseRunning)
	resp := patchPending(t, srv, agent.ID, map[string]interface{}{"config": map[string]interface{}{"max_turns": nil}})
	assert.Equal(t, []string{"config.max_turns"}, resp.Disposition.HeldForReincarnate)
	require.Len(t, resp.Warnings, 1, "%v", resp.Warnings)
	assert.Equal(t, "config.max_turns: the clear is kept pending until a reincarnation applies pending edits; the agent keeps its current value (a start does not apply a cleared value)", resp.Warnings[0])

	warnings := effectStopStart(t, srv, agent.ID)
	starts := disp.snapshot()
	require.Len(t, starts, 1)
	assert.Equal(t, api.IntPtr(3), starts[0].inline.MaxTurns, "the start keeps the limit")
	after := pendingReloadAgent(t, s, agent.ID)
	require.NotNil(t, after.PendingConfig)
	assert.Equal(t, []string{"config.max_turns"}, pendingConfigKeys(after.PendingConfig))
	assert.Equal(t, api.IntPtr(3), after.AppliedConfig.InlineConfig.MaxTurns)
	assert.Contains(t, warnings, heldForReincarnateWarning([]string{"config.max_turns"}))
	effectAssertNoAppliedAudit(t, s, agent.ID)
}

// TestPendingFold_DroppedResourcesSubFieldStaysPendingWhole (C2): a held
// resources edit that drops a stored sub-field stays pending as a whole:
// none of it is dispatched, the changed cpu included.
func TestPendingFold_DroppedResourcesSubFieldStaysPendingWhole(t *testing.T) {
	srv, s, disp, agent := heldFilterFixture(t, &store.BrokerCapabilities{Reprovision: true, ClearedFields: true})
	resp := patchPending(t, srv, agent.ID, map[string]interface{}{"config": map[string]interface{}{
		"resources": map[string]interface{}{"requests": map[string]string{"cpu": "2"}},
	}})
	assert.Equal(t, []string{"config.resources"}, resp.Disposition.HeldForReincarnate)

	warnings := effectStopStart(t, srv, agent.ID)
	starts := disp.snapshot()
	require.Len(t, starts, 1)
	require.NotNil(t, starts[0].inline.Resources)
	assert.Equal(t, api.ResourceList{CPU: "1", Memory: "1Gi"}, starts[0].inline.Resources.Requests, "no part of the resources edit is dispatched")
	after := pendingReloadAgent(t, s, agent.ID)
	require.NotNil(t, after.PendingConfig)
	assert.Equal(t, []string{"config.resources"}, pendingConfigKeys(after.PendingConfig))
	assert.Equal(t, api.ResourceList{CPU: "1", Memory: "1Gi"}, after.AppliedConfig.InlineConfig.Resources.Requests)
	assert.Contains(t, warnings, heldForReincarnateWarning([]string{"config.resources"}))
	effectAssertNoAppliedAudit(t, s, agent.ID)
}

// TestPendingFold_PlainT1FoldsAndDrains (C3, positive control): a held
// model edit is applied at the start, drained from the pending config and
// audited as applied.
func TestPendingFold_PlainT1FoldsAndDrains(t *testing.T) {
	srv, s, disp, agent := heldEditsFixture(t, state.PhaseRunning)
	resp := patchPending(t, srv, agent.ID, map[string]interface{}{"config": map[string]interface{}{"model": "held-model"}})
	assert.Equal(t, []string{"config.model"}, resp.Disposition.Held)

	effectStopStart(t, srv, agent.ID)
	starts := disp.snapshot()
	require.Len(t, starts, 1)
	assert.Equal(t, "held-model", starts[0].model)
	assert.Nil(t, pendingReloadAgent(t, s, agent.ID).PendingConfig)
	applied := pendingAudits(t, s, agent.ID, mutationTypeAgentPendingConfigApplied, 1)
	keys, trigger := pendingAuditSummary(t, applied[0])
	assert.Equal(t, []string{"config.model"}, keys)
	assert.Equal(t, pendingFoldTriggerStart, trigger)
}

// TestPendingFold_WaitingAndReincarnateAreDisjoint (C4): a waiting
// Unlimited and a reincarnation-only clear on one start both stay pending,
// each named by its own warning only.
func TestPendingFold_WaitingAndReincarnateAreDisjoint(t *testing.T) {
	srv, s, disp, agent := heldFilterFixture(t, &store.BrokerCapabilities{Reprovision: true, UnlimitedCountLimits: true})
	patchPending(t, srv, agent.ID, map[string]interface{}{"config": map[string]interface{}{
		"max_turns": 0, "max_duration": nil,
	}})
	setBrokerCapabilities(t, s, agent.RuntimeBrokerID, &store.BrokerCapabilities{Reprovision: true})

	warnings := effectStopStart(t, srv, agent.ID)
	starts := disp.snapshot()
	require.Len(t, starts, 1)
	assert.Equal(t, "2h", starts[0].inline.MaxDuration)
	assert.Nil(t, starts[0].inline.MaxTurns)
	after := pendingReloadAgent(t, s, agent.ID)
	require.NotNil(t, after.PendingConfig)
	assert.Equal(t, []string{"config.max_duration", "config.max_turns"}, pendingConfigKeys(after.PendingConfig))
	assert.Contains(t, warnings, unlimitedWaitsWarning([]string{"config.max_turns"}), "the waiting warning names only max_turns")
	assert.Contains(t, warnings, heldForReincarnateWarning([]string{"config.max_duration"}), "the held-for-reincarnation warning names only max_duration")
	assertNoHeldForReincarnate(t, warnings, "max turns")
	effectAssertNoAppliedAudit(t, s, agent.ID)
}

// TestPendingFold_LegacyClearFoldsOnClearingBroker (C5a): a legacy pending
// count limit of 0 is a clear, and folds as one on a broker that applies
// cleared_fields.
func TestPendingFold_LegacyClearFoldsOnClearingBroker(t *testing.T) {
	srv, s, disp, agent := heldEditsFixture(t, state.PhaseStopped)
	setBrokerCapabilities(t, s, agent.RuntimeBrokerID, &store.BrokerCapabilities{Reprovision: true, ClearedFields: true})
	legacyPendingRow(t, s, agent, `{"max_turns":0}`)

	pendingLifecycleWarnings(t, srv, agent.ID, "start")
	starts := disp.snapshot()
	require.Len(t, starts, 1)
	assert.Nil(t, starts[0].inline.MaxTurns, "folded as a clear, not as Unlimited")
	assert.Contains(t, starts[0].inline.ClearedFields, "max_turns")
	assert.Nil(t, pendingReloadAgent(t, s, agent.ID).PendingConfig)
}

// TestPendingFold_LegacyClearOldBrokerStaysPending (C5b): on a broker
// without clearedFields a legacy pending 0 (a clear) stays pending as null
// through a start that folds another key, and the rewritten row keeps the
// typed limits stamp.
func TestPendingFold_LegacyClearOldBrokerStaysPending(t *testing.T) {
	srv, s, disp, agent := heldEditsFixture(t, state.PhaseStopped)
	legacyPendingRow(t, s, agent, `{"max_turns":0,"model":"legacy-model"}`)

	warnings := pendingLifecycleWarnings(t, srv, agent.ID, "start")
	starts := disp.snapshot()
	require.Len(t, starts, 1)
	assert.Equal(t, "legacy-model", starts[0].model)
	assert.Equal(t, api.IntPtr(3), starts[0].inline.MaxTurns, "the start keeps the limit")
	after := pendingReloadAgent(t, s, agent.ID)
	require.NotNil(t, after.PendingConfig)
	assert.Equal(t, []string{"config.max_turns"}, pendingConfigKeys(after.PendingConfig))
	assert.Equal(t, "null", string(after.PendingConfig.Config["max_turns"]))
	assert.Equal(t, store.PendingLimitsFormatTyped, after.PendingConfig.LimitsFormat)
	assert.Contains(t, warnings, heldForReincarnateWarning([]string{"config.max_turns"}))
}

// TestPendingFold_ReclassifiesAgainstCurrentBroker (C6): a clear saved as
// reincarnation-only folds at a start once the broker applies
// cleared_fields: the fold classifies against the current broker.
func TestPendingFold_ReclassifiesAgainstCurrentBroker(t *testing.T) {
	srv, s, disp, agent := heldEditsFixture(t, state.PhaseRunning)
	resp := patchPending(t, srv, agent.ID, map[string]interface{}{"config": map[string]interface{}{"max_turns": nil}})
	assert.Equal(t, []string{"config.max_turns"}, resp.Disposition.HeldForReincarnate)
	setBrokerCapabilities(t, s, agent.RuntimeBrokerID, &store.BrokerCapabilities{Reprovision: true, ClearedFields: true})

	effectStopStart(t, srv, agent.ID)
	starts := disp.snapshot()
	require.Len(t, starts, 1)
	assert.Nil(t, starts[0].inline.MaxTurns)
	assert.Contains(t, starts[0].inline.ClearedFields, "max_turns")
	assert.Nil(t, pendingReloadAgent(t, s, agent.ID).PendingConfig)
	applied := pendingAudits(t, s, agent.ID, mutationTypeAgentPendingConfigApplied, 1)
	keys, _ := pendingAuditSummary(t, applied[0])
	assert.Equal(t, []string{"config.max_turns"}, keys)
}

// TestPendingFold_HeldUnlimitedFoldsOnCapableBroker (C7): on a broker that
// applies Unlimited, a held 0 is a set value: held, not a clear, and it
// folds.
func TestPendingFold_HeldUnlimitedFoldsOnCapableBroker(t *testing.T) {
	srv, s, disp, agent := heldEditsFixture(t, state.PhaseRunning)
	setBrokerCapabilities(t, s, agent.RuntimeBrokerID, &store.BrokerCapabilities{Reprovision: true, UnlimitedCountLimits: true})
	resp := patchPending(t, srv, agent.ID, map[string]interface{}{"config": map[string]interface{}{"max_turns": 0}})
	assert.Equal(t, []string{"config.max_turns"}, resp.Disposition.Held)
	assert.Empty(t, resp.Disposition.HeldForReincarnate)

	effectStopStart(t, srv, agent.ID)
	starts := disp.snapshot()
	require.Len(t, starts, 1)
	assert.Equal(t, api.IntPtr(0), starts[0].inline.MaxTurns)
	assert.Nil(t, pendingReloadAgent(t, s, agent.ID).PendingConfig)
	applied := pendingAudits(t, s, agent.ID, mutationTypeAgentPendingConfigApplied, 1)
	keys, _ := pendingAuditSummary(t, applied[0])
	assert.Equal(t, []string{"config.max_turns"}, keys)
}

// TestPendingFold_ReincarnateOnlyKeyNotWrittenAfterReincarnation (RF1): a
// held reincarnation-only clear stays pending through a reincarnation and
// the next start, and is written neither into the applied config nor into
// the create inputs.
func TestPendingFold_ReincarnateOnlyKeyNotWrittenAfterReincarnation(t *testing.T) {
	srv, s, disp, agent := heldEditsFixture(t, state.PhaseRunning)
	effectSetCreateInputsMaxTurns(t, s, agent.ID, 3)
	resp := patchPending(t, srv, agent.ID, map[string]interface{}{"config": map[string]interface{}{"max_turns": nil}})
	assert.Equal(t, []string{"config.max_turns"}, resp.Disposition.HeldForReincarnate)

	rec := reincarnateAsDev(t, srv, agent.ID, ReincarnateAgentRequest{Model: "reincarnated-model"})
	require.Equal(t, http.StatusAccepted, rec.Code, rec.Body.String())
	waitForReincarnationSettled(t, s, agent.ID)
	before := pendingReloadAgent(t, s, agent.ID)
	require.NotNil(t, before.PendingConfig)
	require.NotNil(t, before.AppliedConfig.InlineConfig.MaxTurns, "the reincarnation keeps a limit")
	require.NotNil(t, before.AppliedConfig.CreateInputs)
	require.NotNil(t, before.AppliedConfig.CreateInputs.InlineConfig)
	require.NotNil(t, before.AppliedConfig.CreateInputs.InlineConfig.MaxTurns)
	wantTurns := *before.AppliedConfig.InlineConfig.MaxTurns
	wantInputTurns := *before.AppliedConfig.CreateInputs.InlineConfig.MaxTurns

	n := len(disp.snapshot())
	warnings := effectStopStart(t, srv, agent.ID)
	starts := disp.snapshot()
	require.Len(t, starts, n+1)
	assert.Equal(t, api.IntPtr(wantTurns), starts[n].inline.MaxTurns, "the start keeps the limit")
	after := pendingReloadAgent(t, s, agent.ID)
	require.NotNil(t, after.PendingConfig)
	assert.Equal(t, []string{"config.max_turns"}, pendingConfigKeys(after.PendingConfig))
	assert.Equal(t, api.IntPtr(wantTurns), after.AppliedConfig.InlineConfig.MaxTurns)
	require.NotNil(t, after.AppliedConfig.CreateInputs)
	require.NotNil(t, after.AppliedConfig.CreateInputs.InlineConfig)
	assert.Equal(t, api.IntPtr(wantInputTurns), after.AppliedConfig.CreateInputs.InlineConfig.MaxTurns, "the clear is not recorded as an explicit edit")
	assert.Contains(t, warnings, heldForReincarnateWarning([]string{"config.max_turns"}))
	effectAssertNoAppliedAudit(t, s, agent.ID)
}

// TestPendingFold_MixedEnvEditStaysPendingWhole (M2): a held env edit that
// adds one variable and removes another, on a broker without
// clearedFields, stays pending whole: the addition is not dispatched
// either, and the key is reported once.
func TestPendingFold_MixedEnvEditStaysPendingWhole(t *testing.T) {
	srv, s, disp, agent := heldFilterFixture(t, &store.BrokerCapabilities{Reprovision: true})
	resp := patchPending(t, srv, agent.ID, map[string]interface{}{"config": map[string]interface{}{
		"env": map[string]string{"KEEP": "1", "NEW": "1"},
	}})
	assert.Equal(t, []string{"config.env"}, resp.Disposition.HeldForReincarnate)
	assert.Contains(t, resp.Warnings, "config.env: the edit removes DROP, so the whole edit, additions included, is kept pending until a reincarnation applies pending edits; the agent keeps its current variables (a start does not remove them)")

	warnings := effectStopStart(t, srv, agent.ID)
	starts := disp.snapshot()
	require.Len(t, starts, 1)
	assert.NotContains(t, starts[0].inline.Env, "NEW", "the addition waits with the removal")
	assert.Equal(t, "2", starts[0].inline.Env["DROP"])
	after := pendingReloadAgent(t, s, agent.ID)
	assert.Equal(t, map[string]string{"KEEP": "1", "DROP": "2"}, after.AppliedConfig.InlineConfig.Env)
	require.NotNil(t, after.PendingConfig)
	assert.Equal(t, []string{"config.env"}, pendingConfigKeys(after.PendingConfig))
	assert.Contains(t, warnings, heldForReincarnateWarning([]string{"config.env"}), "env is reported once")
	effectAssertNoAppliedAudit(t, s, agent.ID)
}

// TestPendingFold_MixedEditFlagOffUnchanged (M3): with held edits off, a
// mixed skills and env edit behaves as before: refused for a running
// agent, written through for a stopped one (additions applied, removals
// reported held for reincarnation), and a stored pending row is not
// applied.
func TestPendingFold_MixedEditFlagOffUnchanged(t *testing.T) {
	body := map[string]interface{}{"config": map[string]interface{}{
		"skills": []map[string]interface{}{{"uri": "hub://skills/review"}, {"uri": "hub://skills/other"}},
		"env":    map[string]string{"KEEP": "1", "NEW": "1"},
	}}

	t.Run("running: refused", func(t *testing.T) {
		srv, s, _, agent := heldFilterFixture(t, &store.BrokerCapabilities{Reprovision: true, ChangedFields: true})
		setHeldEditsExperiment(t, srv, false)
		rec := doRequest(t, srv, http.MethodPatch, "/api/v1/agents/"+agent.ID, body)
		require.Equal(t, http.StatusConflict, rec.Code, rec.Body.String())
		assert.Nil(t, pendingReloadAgent(t, s, agent.ID).PendingConfig)
	})

	t.Run("stopped: written through as before", func(t *testing.T) {
		srv, s, _, agent := heldFilterFixture(t, &store.BrokerCapabilities{Reprovision: true, ChangedFields: true})
		setHeldEditsExperiment(t, srv, false)
		effectSetPhase(t, s, agent.ID, state.PhaseStopped)
		resp := patchPending(t, srv, agent.ID, body)
		assert.Equal(t, []string{"config.env", "config.skills"}, resp.Disposition.HeldForReincarnate)
		assert.Nil(t, resp.PendingConfig)
		after := pendingReloadAgent(t, s, agent.ID)
		assert.Equal(t, "1", after.AppliedConfig.InlineConfig.Env["NEW"], "the addition is applied")
		assert.Contains(t, after.AppliedConfig.InlineConfig.Skills, api.SkillReference{URI: "hub://skills/other"})
	})

	t.Run("stored pending row is not applied", func(t *testing.T) {
		srv, s, disp, agent := heldFilterFixture(t, &store.BrokerCapabilities{Reprovision: true, ChangedFields: true})
		patchPending(t, srv, agent.ID, body)
		setHeldEditsExperiment(t, srv, false)
		effectStopStart(t, srv, agent.ID)
		starts := disp.snapshot()
		require.Len(t, starts, 1)
		assert.NotContains(t, starts[0].inline.Env, "NEW")
		assert.NotContains(t, starts[0].inline.Skills, api.SkillReference{URI: "hub://skills/other"})
	})
}
