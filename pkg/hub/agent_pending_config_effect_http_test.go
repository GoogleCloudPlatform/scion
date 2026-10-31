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
	"database/sql"
	"encoding/json"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
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

// effectUser creates an active member user with the project-admin role
// on projectID (agent.update, no attach) and hub membership, as a login
// would.
func effectUser(t *testing.T, s store.Store, projectID, name string) *store.User {
	t.Helper()
	id := tid("ae6-" + name + "-" + t.Name())
	createTestUserWithProjectRole(t, s, id, name+"@example.test", projectID, store.ProjectRoleAdmin)
	ensureHubMembership(context.Background(), s, id)
	u, err := s.GetUser(context.Background(), id)
	require.NoError(t, err)
	return u
}

// effectRevokeGrants deletes every role binding of userID: the user can no
// longer edit the agent.
func effectRevokeGrants(t *testing.T, s store.Store, userID string) {
	t.Helper()
	_, err := s.DeleteRoleBindingsForPrincipal(context.Background(), store.RoleBindingPrincipalUser, userID)
	require.NoError(t, err)
}

// effectSetUserStatus writes userID's account status.
func effectSetUserStatus(t *testing.T, s store.Store, userID, status string) {
	t.Helper()
	u, err := s.GetUser(context.Background(), userID)
	require.NoError(t, err)
	u.Status = status
	require.NoError(t, s.UpdateUser(context.Background(), u))
}

// effectPatchAs PATCHes the agent as user and decodes the 200 response.
func effectPatchAs(t *testing.T, srv *Server, user *store.User, agentID string, body map[string]interface{}) pendingPatchResponse {
	t.Helper()
	rec := doRequestAsUser(t, srv, user, http.MethodPatch, "/api/v1/agents/"+agentID, body)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var resp pendingPatchResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	return resp
}

// effectSeedPending stores a pending config on the agent directly: config
// holds raw JSON values, keySavers the per-key savers (nil for a legacy
// row) and savedBy the last saver.
func effectSeedPending(t *testing.T, s store.Store, agentID string, config map[string]string, keySavers map[string]string, savedBy string) {
	t.Helper()
	cur := pendingReloadAgent(t, s, agentID)
	pc := &store.AgentPendingConfig{Config: map[string]json.RawMessage{}, KeySavers: keySavers, SavedBy: savedBy, SavedAt: time.Now().UTC()}
	for k, v := range config {
		pc.Config[k] = json.RawMessage(v)
	}
	_, err := s.UpdateAgentConfig(context.Background(), agentID, cur.StateVersion, cur.AppliedConfig, pc)
	require.NoError(t, err)
}

// effectDiscards waits for want agent_pending_config_discarded records and
// returns their keys by trigger.
func effectDiscards(t *testing.T, s store.Store, agentID string, want int) map[string][]string {
	t.Helper()
	out := map[string][]string{}
	for _, rec := range pendingAudits(t, s, agentID, mutationTypeAgentPendingConfigDiscarded, want) {
		keys, trigger := pendingAuditSummary(t, rec)
		out[trigger] = append(out[trigger], keys...)
	}
	return out
}

// effectAssertNoDiscard waits out a window in which an asynchronous
// agent_pending_config_discarded record for agentID would land, and fails
// if one does.
func effectAssertNoDiscard(t *testing.T, s store.Store, agentID string) {
	t.Helper()
	assert.Never(t, func() bool {
		recs, _, err := s.ListMutationAudits(context.Background(), store.MutationAuditFilter{
			MutationType: mutationTypeAgentPendingConfigDiscarded, TargetID: agentID, Limit: 5,
		})
		return err != nil || len(recs) > 0
	}, 500*time.Millisecond, 20*time.Millisecond, "no agent_pending_config_discarded record")
}

// effectStoredPending returns the agent's stored pending config as storage
// encodes it, for byte-identity checks.
func effectStoredPending(t *testing.T, s store.Store, agentID string) (string, int64) {
	t.Helper()
	a := pendingReloadAgent(t, s, agentID)
	raw, err := store.MarshalPendingConfigForStorage(a.PendingConfig)
	require.NoError(t, err)
	return raw, a.StateVersion
}

// effectDropWarning is the warning a fold gives for keys dropped with
// trigger.
func effectDropWarning(trigger string, keys ...string) string {
	return pendingFoldWarnings(map[string][]string{trigger: keys}, nil)[0]
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
	for _, w := range warnings {
		if strings.HasPrefix(w, heldReincarnateWarningPrefix) {
			assert.NotContains(t, w, "max turns", "a waiting Unlimited is never also held for reincarnation")
		}
	}
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

	t.Run("stored pending row is not applied, and is cleared", func(t *testing.T) {
		srv, s, disp, agent := heldFilterFixture(t, &store.BrokerCapabilities{Reprovision: true, ChangedFields: true})
		patchPending(t, srv, agent.ID, body)
		setHeldEditsExperiment(t, srv, false)
		effectStopStart(t, srv, agent.ID)
		starts := disp.snapshot()
		require.Len(t, starts, 1)
		assert.NotContains(t, starts[0].inline.Env, "NEW")
		assert.NotContains(t, starts[0].inline.Skills, api.SkillReference{URI: "hub://skills/other"})
		assert.Nil(t, pendingReloadAgent(t, s, agent.ID).PendingConfig, "and it is cleared (I3)")
	})
}

// TestPendingReauth_PerKeySaver (I1-a): each key is re-checked against its
// own saver. A's model is dropped once A loses agent.update (audited,
// warned); B's env is applied.
func TestPendingReauth_PerKeySaver(t *testing.T) {
	srv, s, disp, agent := heldEditsFixture(t, state.PhaseRunning)
	a := effectUser(t, s, agent.ProjectID, "a")
	b := effectUser(t, s, agent.ProjectID, "b")
	effectPatchAs(t, srv, a, agent.ID, map[string]interface{}{"config": map[string]interface{}{"model": "a-model"}})
	effectPatchAs(t, srv, b, agent.ID, map[string]interface{}{"config": map[string]interface{}{"env": map[string]string{"FROM_B": "1"}}})
	stored := pendingReloadAgent(t, s, agent.ID)
	require.NotNil(t, stored.PendingConfig)
	assert.Equal(t, map[string]string{"model": "user:" + a.ID, "env": "user:" + b.ID}, stored.PendingConfig.KeySavers)
	effectRevokeGrants(t, s, a.ID)

	warnings := effectStopStart(t, srv, agent.ID)
	starts := disp.snapshot()
	require.Len(t, starts, 1)
	assert.Equal(t, "old-model", starts[0].model, "A's edit is not applied")
	assert.Equal(t, "1", starts[0].inline.Env["FROM_B"], "B's edit is applied")
	assert.Nil(t, pendingReloadAgent(t, s, agent.ID).PendingConfig)
	assert.Contains(t, warnings, effectDropWarning(pendingDiscardTriggerSaverUnauthorized, "config.model"))
	assert.Equal(t, map[string][]string{pendingDiscardTriggerSaverUnauthorized: {"config.model"}}, effectDiscards(t, s, agent.ID, 1))
	applied := pendingAudits(t, s, agent.ID, mutationTypeAgentPendingConfigApplied, 1)
	keys, _ := pendingAuditSummary(t, applied[0])
	assert.Equal(t, []string{"config.env"}, keys)
}

// TestPendingReauth_EnvViewReevaluated (I1-b): the env-removal rules run
// with the env saver's CURRENT env-view decision: a saver who could view
// env when saving a removal, and can no longer, gets the removal left out
// at the start (the edit is applied on its additions alone).
func TestPendingReauth_EnvViewReevaluated(t *testing.T) {
	srv, s, disp, agent := heldFilterFixture(t, &store.BrokerCapabilities{Reprovision: true, ClearedFields: true})
	a := effectUser(t, s, agent.ProjectID, "a")
	// The agent's owner may attach (view env); project-admin alone may
	// update but not attach.
	setOwner := func(owner string) {
		cur := pendingReloadAgent(t, s, agent.ID)
		cur.OwnerID = owner
		require.NoError(t, s.UpdateAgent(context.Background(), cur))
	}
	setOwner(a.ID)
	require.True(t, canViewAgentEnv(contextWithIdentity(context.Background(), NewAuthenticatedUser(a.ID, a.Email, a.DisplayName, a.Role, "")), srv, pendingReloadAgent(t, s, agent.ID)),
		"positive control: the owner may view env")
	resp := effectPatchAs(t, srv, a, agent.ID, map[string]interface{}{"config": map[string]interface{}{
		"env": map[string]string{"KEEP": "1", "NEW": "1"},
	}})
	assert.Equal(t, []string{"config.env"}, resp.Disposition.Held, "a recorded removal applies at the next start on this broker")
	// Another user becomes the owner: A keeps project-admin (update) but
	// can no longer attach.
	setOwner(effectUser(t, s, agent.ProjectID, "new-owner").ID)

	effectStopStart(t, srv, agent.ID)
	starts := disp.snapshot()
	require.Len(t, starts, 1)
	assert.Equal(t, "1", starts[0].inline.Env["NEW"], "the addition is applied")
	// The removal is not recorded, so the broker's merge keeps DROP: the
	// saver can no longer view env.
	assert.NotContains(t, starts[0].inline.ClearedFields, api.ClearedEnvPathPrefix+"DROP")
}

// TestPendingReauth_UnresolvableSaverDropped (I1-d): an unknown saver kind
// and an empty saver are dropped with saver_unresolvable and a warning;
// the start continues and applies the rest.
func TestPendingReauth_UnresolvableSaverDropped(t *testing.T) {
	srv, s, disp, agent := heldEditsFixture(t, state.PhaseStopped)
	effectSeedPending(t, s, agent.ID,
		map[string]string{"model": `"bogus-model"`, "max_turns": `7`, "thinking_level": `3`},
		map[string]string{"model": "bogus:x", "max_turns": "", "thinking_level": "dev:" + DevUserID}, "")

	warnings := pendingLifecycleWarnings(t, srv, agent.ID, "start")
	starts := disp.snapshot()
	require.Len(t, starts, 1)
	assert.Equal(t, "old-model", starts[0].model)
	assert.Equal(t, api.IntPtr(3), starts[0].inline.MaxTurns)
	assert.Equal(t, api.IntPtr(3), starts[0].inline.ThinkingLevel, "the dev saver's key folds")
	assert.Contains(t, warnings, effectDropWarning(pendingDiscardTriggerSaverUnresolvable, "config.max_turns", "config.model"))
	assert.Equal(t, map[string][]string{pendingDiscardTriggerSaverUnresolvable: {"config.max_turns", "config.model"}}, effectDiscards(t, s, agent.ID, 1))
}

// TestPendingReauth_LegacyRowFallsBackToSavedBy (I1-e): a row written
// before KeySavers existed (with the retired envSaverCanViewEnv) loads,
// and its keys are re-checked against SavedBy.
func TestPendingReauth_LegacyRowFallsBackToSavedBy(t *testing.T) {
	srv, s, disp, agent := heldEditsFixture(t, state.PhaseStopped)
	a := effectUser(t, s, agent.ProjectID, "a")
	pc, err := store.UnmarshalPendingConfigFromStorage(`{"config":{"model":"legacy-model","env":{"L":"1"}},"savedBy":"user:` + a.ID + `","envSaverCanViewEnv":true,"savedAt":"2026-10-01T00:00:00Z","limitsFormat":2}`)
	require.NoError(t, err)
	require.Nil(t, pc.KeySavers)
	cur := pendingReloadAgent(t, s, agent.ID)
	_, err = s.UpdateAgentConfig(context.Background(), agent.ID, cur.StateVersion, cur.AppliedConfig, pc)
	require.NoError(t, err)

	pendingLifecycleWarnings(t, srv, agent.ID, "start")
	starts := disp.snapshot()
	require.Len(t, starts, 1)
	assert.Equal(t, "legacy-model", starts[0].model)
	assert.Equal(t, "1", starts[0].inline.Env["L"])
	assert.Nil(t, pendingReloadAgent(t, s, agent.ID).PendingConfig)

	// The same row with a saver who has lost the grant drops both keys.
	srv2, s2, disp2, agent2 := heldEditsFixture(t, state.PhaseStopped)
	b := effectUser(t, s2, agent2.ProjectID, "b")
	effectSeedPending(t, s2, agent2.ID, map[string]string{"model": `"legacy-model"`}, nil, "user:"+b.ID)
	effectRevokeGrants(t, s2, b.ID)
	pendingLifecycleWarnings(t, srv2, agent2.ID, "start")
	require.Len(t, disp2.snapshot(), 1)
	assert.Equal(t, "old-model", disp2.snapshot()[0].model)
	assert.Equal(t, map[string][]string{pendingDiscardTriggerSaverUnauthorized: {"config.model"}}, effectDiscards(t, s2, agent2.ID, 1))
}

// TestPendingReauth_DelegatedSaverDroppedAuditedAndWarned (I1-f): a key
// saved with an agent delegated credential is never applied: it is
// audited saver_delegated and named in the start warning (and in the
// warning of a no-container PATCH that folds it); other savers' keys
// fold and the start succeeds.
func TestPendingReauth_DelegatedSaverDroppedAuditedAndWarned(t *testing.T) {
	seed := func(t *testing.T, s store.Store, agentID string) {
		effectSeedPending(t, s, agentID,
			map[string]string{"model": `"delegated-model"`, "max_turns": `9`},
			map[string]string{"model": "agent_delegated:some-agent", "max_turns": "dev:" + DevUserID}, "")
	}

	t.Run("start", func(t *testing.T) {
		srv, s, disp, agent := heldEditsFixture(t, state.PhaseStopped)
		seed(t, s, agent.ID)
		warnings := pendingLifecycleWarnings(t, srv, agent.ID, "start")
		starts := disp.snapshot()
		require.Len(t, starts, 1)
		assert.Equal(t, "old-model", starts[0].model)
		assert.Equal(t, api.IntPtr(9), starts[0].inline.MaxTurns)
		assert.Contains(t, warnings, effectDropWarning(pendingDiscardTriggerSaverDelegated, "config.model"))
		assert.Equal(t, map[string][]string{pendingDiscardTriggerSaverDelegated: {"config.model"}}, effectDiscards(t, s, agent.ID, 1))
		assert.Nil(t, pendingReloadAgent(t, s, agent.ID).PendingConfig)
	})

	t.Run("no-container PATCH fold", func(t *testing.T) {
		srv, s, _, agent := heldEditsFixture(t, state.PhaseStopped)
		seed(t, s, agent.ID)
		resp := patchPending(t, srv, agent.ID, map[string]interface{}{"config": map[string]interface{}{"thinking_level": 2}})
		assert.Contains(t, resp.Warnings, effectDropWarning(pendingDiscardTriggerSaverDelegated, "config.model"))
		after := pendingReloadAgent(t, s, agent.ID)
		assert.Equal(t, "old-model", after.AppliedConfig.Model)
		assert.Equal(t, api.IntPtr(9), after.AppliedConfig.InlineConfig.MaxTurns)
		assert.Equal(t, map[string][]string{pendingDiscardTriggerSaverDelegated: {"config.model"}}, effectDiscards(t, s, agent.ID, 1))
	})
}

// TestPendingReauth_WholeKeyDecision (I1-g): one decision per key: a
// resources value with several sub-fields is dropped whole when its saver
// lost agent.update, and folds whole when the saver still holds it.
func TestPendingReauth_WholeKeyDecision(t *testing.T) {
	resources := map[string]interface{}{
		"requests": map[string]string{"cpu": "2", "memory": "4Gi"},
		"limits":   map[string]string{"cpu": "4", "memory": "8Gi"},
	}
	for _, revoke := range []bool{true, false} {
		name := map[bool]string{true: "saver lost the grant: dropped whole", false: "saver allowed: folds whole"}[revoke]
		t.Run(name, func(t *testing.T) {
			srv, s, disp, agent := heldEditsFixture(t, state.PhaseRunning)
			a := effectUser(t, s, agent.ProjectID, "a")
			resp := effectPatchAs(t, srv, a, agent.ID, map[string]interface{}{"config": map[string]interface{}{"resources": resources}})
			assert.Equal(t, []string{"config.resources"}, resp.Disposition.Held)
			if revoke {
				effectRevokeGrants(t, s, a.ID)
			}
			effectStopStart(t, srv, agent.ID)
			starts := disp.snapshot()
			require.Len(t, starts, 1)
			if revoke {
				assert.Nil(t, starts[0].inline.Resources, "no sub-field is applied")
				assert.Equal(t, map[string][]string{pendingDiscardTriggerSaverUnauthorized: {"config.resources"}}, effectDiscards(t, s, agent.ID, 1))
				return
			}
			require.NotNil(t, starts[0].inline.Resources)
			assert.Equal(t, api.ResourceList{CPU: "2", Memory: "4Gi"}, starts[0].inline.Resources.Requests)
			assert.Equal(t, api.ResourceList{CPU: "4", Memory: "8Gi"}, starts[0].inline.Resources.Limits)
			effectAssertNoDiscard(t, s, agent.ID)
		})
	}
}

// TestPendingReauth_KeySaversOnlyFromAuthorizedSave (I1-h): a caller
// without agent.update is refused (403) and the stored row, KeySavers
// included, is byte-identical; an allowed caller's save records its
// precise-kind reference for each key it stores.
func TestPendingReauth_KeySaversOnlyFromAuthorizedSave(t *testing.T) {
	srv, s, _, agent := heldEditsFixture(t, state.PhaseRunning)
	patchPending(t, srv, agent.ID, map[string]interface{}{"config": map[string]interface{}{"model": "dev-model"}})
	before, beforeVersion := effectStoredPending(t, s, agent.ID)

	outsider := effectUser(t, s, agent.ProjectID, "outsider")
	effectRevokeGrants(t, s, outsider.ID)
	rec := doRequestAsUser(t, srv, outsider, http.MethodPatch, "/api/v1/agents/"+agent.ID, map[string]interface{}{
		"config": map[string]interface{}{"model": "outsider-model", "max_turns": 5},
	})
	require.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
	after, afterVersion := effectStoredPending(t, s, agent.ID)
	assert.Equal(t, before, after, "the refused save writes nothing")
	assert.Equal(t, beforeVersion, afterVersion)

	a := effectUser(t, s, agent.ProjectID, "a")
	effectPatchAs(t, srv, a, agent.ID, map[string]interface{}{"config": map[string]interface{}{"model": "a-model", "max_turns": 5}})
	stored := pendingReloadAgent(t, s, agent.ID)
	require.NotNil(t, stored.PendingConfig)
	assert.Equal(t, map[string]string{"model": "user:" + a.ID, "max_turns": "user:" + a.ID}, stored.PendingConfig.KeySavers)
}

// TestPendingReauth_DevSaveStoresDevRef (V2): a save as the workstation dev
// user stores exactly dev:<DevUserID>, not user:. (No visitor-caller
// fixture issues a PATCH as a visitor; the visitor ref is covered by the
// saver-kind tests.)
func TestPendingReauth_DevSaveStoresDevRef(t *testing.T) {
	srv, s, _, agent := heldEditsFixture(t, state.PhaseRunning)
	patchPending(t, srv, agent.ID, map[string]interface{}{"config": map[string]interface{}{"model": "dev-model"}})
	stored := pendingReloadAgent(t, s, agent.ID)
	require.NotNil(t, stored.PendingConfig)
	assert.Equal(t, map[string]string{"model": "dev:" + DevUserID}, stored.PendingConfig.KeySavers)
	assert.Equal(t, "user:"+DevUserID, stored.PendingConfig.SavedBy, "SavedBy keeps the API format")
}

// TestPendingFold_PatchFoldKeepsReincarnateAndSupersedes (C8): the fold of
// a PATCH to an agent with no container keeps a reincarnation-only key
// pending, and a write of that key now supersedes it (audited).
func TestPendingFold_PatchFoldKeepsReincarnateAndSupersedes(t *testing.T) {
	srv, s, _, agent := heldEditsFixture(t, state.PhaseRunning)
	patchPending(t, srv, agent.ID, map[string]interface{}{"config": map[string]interface{}{"max_turns": nil, "model": "held-model"}})
	effectSetPhase(t, s, agent.ID, state.PhaseStopped)

	resp := patchPending(t, srv, agent.ID, map[string]interface{}{"config": map[string]interface{}{"thinking_level": 2}})
	assert.Equal(t, []string{"config.max_turns"}, pendingConfigKeys(resp.PendingConfig), "the reincarnation-only clear stays pending")
	after := pendingReloadAgent(t, s, agent.ID)
	assert.Equal(t, "held-model", after.AppliedConfig.Model, "the other key folds")
	assert.Equal(t, api.IntPtr(3), after.AppliedConfig.InlineConfig.MaxTurns)

	resp = patchPending(t, srv, agent.ID, map[string]interface{}{"config": map[string]interface{}{"max_turns": 8}})
	assert.Nil(t, resp.PendingConfig, "the write supersedes the pending clear")
	assert.Equal(t, api.IntPtr(8), pendingReloadAgent(t, s, agent.ID).AppliedConfig.InlineConfig.MaxTurns)
	assert.Equal(t, map[string][]string{pendingDiscardTriggerSuperseded: {"config.max_turns"}}, effectDiscards(t, s, agent.ID, 1))
}

// TestPendingFlagOff_StartClears (I3-a): with held edits off, a start of an
// agent with leftover pending rows completes end to end, clears them
// (audited experiment_off) and hands the new state version to the caller's
// copy; a clear that keeps failing leaves the row and the start completes.
func TestPendingFlagOff_StartClears(t *testing.T) {
	t.Run("cleared", func(t *testing.T) {
		srv, s, disp, agent := heldEditsFixture(t, state.PhaseStopped)
		effectSeedPending(t, s, agent.ID, map[string]string{"model": `"leftover"`}, map[string]string{"model": "dev:" + DevUserID}, "")
		setHeldEditsExperiment(t, srv, false)
		pendingLifecycleWarnings(t, srv, agent.ID, "start")
		starts := disp.snapshot()
		require.Len(t, starts, 1)
		assert.Equal(t, "old-model", starts[0].model)
		after := pendingReloadAgent(t, s, agent.ID)
		assert.Nil(t, after.PendingConfig)
		assert.Equal(t, string(state.PhaseRunning), after.Phase, "the started-status write succeeded after the clear")
		assert.Equal(t, map[string][]string{pendingDiscardTriggerExperimentOff: {"config.model"}}, effectDiscards(t, s, agent.ID, 1))
	})

	t.Run("clear keeps conflicting", func(t *testing.T) {
		disp := &inlineCaptureDispatcher{reincarnateTestDispatcher: newReincarnateTestDispatcher()}
		srv, s, project, broker := setupReincarnateTestServer(t, disp)
		_, fault := installStoreFault(t, srv, func(inner store.Store, f *storeFaultSwitch) *effectFailingConfigWrites {
			return &effectFailingConfigWrites{Store: inner, fault: f}
		})
		setHeldEditsExperiment(t, srv, false)
		agent := newEditTestAgent(t, s, project, broker, state.PhaseStopped)
		effectSeedPending(t, s, agent.ID, map[string]string{"model": `"leftover"`}, nil, "user:"+DevUserID)
		fault.Arm()
		pendingLifecycleWarnings(t, srv, agent.ID, "start")
		require.Len(t, disp.snapshot(), 1, "the start completes")
		assert.NotNil(t, pendingReloadAgent(t, s, agent.ID).PendingConfig, "the row stays")
		effectAssertNoDiscard(t, s, agent.ID)
	})
}

// effectFailingConfigWrites fails every UpdateAgentConfig with a version
// conflict while its switch is armed.
type effectFailingConfigWrites struct {
	store.Store
	fault *storeFaultSwitch
}

func (f *effectFailingConfigWrites) UpdateAgentConfig(ctx context.Context, agentID string, expectedVersion int64, applied *store.AgentAppliedConfig, pending *store.AgentPendingConfig) (int64, error) {
	if f.fault.Active() {
		return 0, store.ErrVersionConflict
	}
	return f.Store.UpdateAgentConfig(ctx, agentID, expectedVersion, applied, pending)
}

// TestPendingFlagOff_PatchClearsAll (I3-b): with held edits off, a PATCH
// clears every pending key: the ones it names as superseded, the rest as
// experiment_off. (A PATCH without config clears too.)
func TestPendingFlagOff_PatchClearsAll(t *testing.T) {
	srv, s, _, agent := heldEditsFixture(t, state.PhaseStopped)
	effectSeedPending(t, s, agent.ID, map[string]string{"model": `"m"`, "max_turns": `4`}, nil, "user:"+DevUserID)
	setHeldEditsExperiment(t, srv, false)
	resp := patchPending(t, srv, agent.ID, map[string]interface{}{"config": map[string]interface{}{"model": "now"}})
	assert.Nil(t, resp.PendingConfig)
	assert.Nil(t, pendingReloadAgent(t, s, agent.ID).PendingConfig)
	assert.Equal(t, map[string][]string{
		pendingDiscardTriggerSuperseded:    {"config.model"},
		pendingDiscardTriggerExperimentOff: {"config.max_turns"},
	}, effectDiscards(t, s, agent.ID, 2))

	srv2, s2, _, agent2 := heldEditsFixture(t, state.PhaseRunning)
	effectSeedPending(t, s2, agent2.ID, map[string]string{"model": `"m"`}, nil, "user:"+DevUserID)
	setHeldEditsExperiment(t, srv2, false)
	patchPending(t, srv2, agent2.ID, map[string]interface{}{"name": "renamed"})
	assert.Nil(t, pendingReloadAgent(t, s2, agent2.ID).PendingConfig)
	assert.Equal(t, map[string][]string{pendingDiscardTriggerExperimentOff: {"config.model"}}, effectDiscards(t, s2, agent2.ID, 1))
}

// TestPendingFlagOff_OnOffOnReauthorizes (I3-c): saved on, turned off and
// on again without a start or edit in between, the pending edit is still
// re-checked against its saver at the next start.
func TestPendingFlagOff_OnOffOnReauthorizes(t *testing.T) {
	srv, s, disp, agent := heldEditsFixture(t, state.PhaseRunning)
	a := effectUser(t, s, agent.ProjectID, "a")
	effectPatchAs(t, srv, a, agent.ID, map[string]interface{}{"config": map[string]interface{}{"model": "a-model"}})
	setHeldEditsExperiment(t, srv, false)
	setHeldEditsExperiment(t, srv, true)
	effectRevokeGrants(t, s, a.ID)
	effectStopStart(t, srv, agent.ID)
	require.Len(t, disp.snapshot(), 1)
	assert.Equal(t, "old-model", disp.snapshot()[0].model)
	assert.Equal(t, map[string][]string{pendingDiscardTriggerSaverUnauthorized: {"config.model"}}, effectDiscards(t, s, agent.ID, 1))
}

// TestPendingFlagOff_ResponsesHidePending (I3-d): with held edits off, the
// GET, list and lifecycle responses carry no pendingConfig, and ?view=edit
// no effectiveNextRun.
func TestPendingFlagOff_ResponsesHidePending(t *testing.T) {
	srv, s, _, agent := heldEditsFixture(t, state.PhaseStopped)
	effectSeedPending(t, s, agent.ID, map[string]string{"model": `"m"`}, nil, "user:"+DevUserID)
	// Positive control: with the experiment on, the GET shows it.
	rec := doRequest(t, srv, http.MethodGet, "/api/v1/agents/"+agent.ID+"?view=edit", nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.Contains(t, rec.Body.String(), `"pendingConfig"`)
	assert.Contains(t, rec.Body.String(), `"effectiveNextRun"`)

	setHeldEditsExperiment(t, srv, false)
	for _, path := range []string{
		"/api/v1/agents/" + agent.ID,
		"/api/v1/agents/" + agent.ID + "?view=edit",
		"/api/v1/agents",
		"/api/v1/projects/" + agent.ProjectID + "/agents",
	} {
		rec := doRequest(t, srv, http.MethodGet, path, nil)
		require.Equal(t, http.StatusOK, rec.Code, "%s: %s", path, rec.Body.String())
		assert.NotContains(t, rec.Body.String(), `"pendingConfig"`, path)
		assert.NotContains(t, rec.Body.String(), `"effectiveNextRun"`, path)
	}
	rec = doRequest(t, srv, http.MethodPost, "/api/v1/agents/"+agent.ID+"/start", nil)
	require.Less(t, rec.Code, 300, rec.Body.String())
	assert.NotContains(t, rec.Body.String(), `"pendingConfig"`)
}

// TestEffectiveNextRun_IncludesReincarnateKey (Q4, O-b): the preview
// includes a key held for reincarnation, does not apply a pending env
// removal, and records no authorization decision about the saver.
func TestEffectiveNextRun_IncludesReincarnateKey(t *testing.T) {
	srv, s, _, agent := heldFilterFixture(t, &store.BrokerCapabilities{Reprovision: true, ClearedFields: true})
	y := effectUser(t, s, agent.ProjectID, "y")
	effectSeedPending(t, s, agent.ID,
		map[string]string{"max_duration": `null`, "env": `{"KEEP":"1"}`},
		map[string]string{"max_duration": "dev:" + DevUserID, "env": "user:" + y.ID}, "")
	effectRevokeGrants(t, s, y.ID)
	before, _, err := s.ListMutationAudits(context.Background(), store.MutationAuditFilter{TargetID: agent.ID, Limit: 50})
	require.NoError(t, err)

	rec := doRequest(t, srv, http.MethodGet, "/api/v1/agents/"+agent.ID+"?view=edit", nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var edit AgentWithCapabilities
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &edit))
	require.NotNil(t, edit.EffectiveNextRun)
	assert.Empty(t, edit.EffectiveNextRun.InlineConfig.MaxDuration, "the reincarnation-held clear is shown")
	// The pending env map is shown as stored (present-key overlay); the
	// preview records no clear for it and asks nothing about its saver.
	assert.Equal(t, "1", edit.EffectiveNextRun.InlineConfig.Env["KEEP"])
	assert.Equal(t, "2h", pendingReloadAgent(t, s, agent.ID).AppliedConfig.InlineConfig.MaxDuration)
	// Nothing about the saver was decided or written.
	assert.Never(t, func() bool {
		after, _, err := s.ListMutationAudits(context.Background(), store.MutationAuditFilter{TargetID: agent.ID, Limit: 50})
		return err != nil || len(after) != len(before)
	}, 300*time.Millisecond, 20*time.Millisecond)
	assert.NotNil(t, pendingReloadAgent(t, s, agent.ID).PendingConfig, "the preview drops nothing")
}

// TestPendingFoldRefusal_ProvisionedStartRevertsIntent (F): a first start
// of a provisioned agent refused by its pending config fold reverts its
// run intent (startDidNotHappen), and answers 503.
func TestPendingFoldRefusal_ProvisionedStartRevertsIntent(t *testing.T) {
	disp := &inlineCaptureDispatcher{reincarnateTestDispatcher: newReincarnateTestDispatcher()}
	srv, s, project, broker := setupReincarnateTestServer(t, disp)
	_, fault := installStoreFault(t, srv, func(inner store.Store, f *storeFaultSwitch) *effectFailingConfigWrites {
		return &effectFailingConfigWrites{Store: inner, fault: f}
	})
	setHeldEditsExperiment(t, srv, true)
	agent := newEditTestAgent(t, s, project, broker, state.PhaseCreated)
	effectSeedPending(t, s, agent.ID, map[string]string{"model": `"m"`}, map[string]string{"model": "dev:" + DevUserID}, "")
	fault.Arm()
	rec := doRequest(t, srv, http.MethodPost, "/api/v1/agents/"+agent.ID+"/start", nil)
	assert.GreaterOrEqual(t, rec.Code, 400, rec.Body.String())
	assert.Empty(t, disp.snapshot(), "nothing was dispatched")
	after := pendingReloadAgent(t, s, agent.ID)
	assert.Equal(t, store.RunIntentStopped, after.RunIntent, "the refused first start reverts its run intent")
	assert.NotNil(t, after.PendingConfig)
}

// saverBSeededStart seeds a stopped agent's pending config with one held
// model edit saved by ref (as a KeySavers entry, or, when legacy, only as
// SavedBy), starts it as the dev user, and returns the start's warnings.
func saverBSeededStart(t *testing.T, ref string, legacy bool) (*Server, store.Store, *inlineCaptureDispatcher, *store.Agent, []string) {
	t.Helper()
	srv, s, disp, agent := heldEditsFixture(t, state.PhaseStopped)
	keySavers := map[string]string{"model": ref}
	savedBy := ref
	if legacy {
		keySavers = nil
	}
	effectSeedPending(t, s, agent.ID, map[string]string{"model": `"seeded-model"`}, keySavers, savedBy)
	warnings := pendingLifecycleWarnings(t, srv, agent.ID, "start")
	return srv, s, disp, agent, warnings
}

// saverBAssertDropped checks a start that dropped the seeded model edit:
// the start dispatched the old model, the key is gone, one discarded audit
// carries trigger, and the start warned with it.
func saverBAssertDropped(t *testing.T, s store.Store, disp *inlineCaptureDispatcher, agentID string, warnings []string, trigger string) {
	t.Helper()
	starts := disp.snapshot()
	require.Len(t, starts, 1)
	assert.Equal(t, "old-model", starts[0].model, "the dropped edit is not applied")
	assert.Nil(t, pendingReloadAgent(t, s, agentID).PendingConfig, "the dropped key is removed")
	assert.Equal(t, map[string][]string{trigger: {"config.model"}}, effectDiscards(t, s, agentID, 1))
	assert.Contains(t, warnings, effectDropWarning(trigger, "config.model"))
	effectAssertNoAppliedAudit(t, s, agentID)
}

// saverBAssertFolded checks a start that applied the seeded (or saved)
// model edit with no discard.
func saverBAssertFolded(t *testing.T, s store.Store, disp *inlineCaptureDispatcher, agentID, model string) {
	t.Helper()
	starts := disp.snapshot()
	require.NotEmpty(t, starts)
	assert.Equal(t, model, starts[len(starts)-1].model)
	assert.Nil(t, pendingReloadAgent(t, s, agentID).PendingConfig)
	applied := pendingAudits(t, s, agentID, mutationTypeAgentPendingConfigApplied, 1)
	keys, _ := pendingAuditSummary(t, applied[0])
	assert.Contains(t, keys, "config.model")
	effectAssertNoDiscard(t, s, agentID)
}

// saverBFederatedUser creates an active federated account row through the
// store (the only path that writes the federated kind).
func saverBFederatedUser(t *testing.T, s store.Store, email string) *store.User {
	t.Helper()
	issuer := "https://ae6-issuer.example.com"
	acct := &store.User{DisplayName: "AE6 Federated", Kind: store.UserKindFederated, Status: store.UserStatusActive, HomeIssuer: &issuer, FederatedEmail: &email}
	require.NoError(t, s.CreateFederatedUser(context.Background(), acct))
	return acct
}

// saverBReportRunning reports the woken container running after a short
// delay, which ends a wake's readiness wait.
func saverBReportRunning(s store.Store, agentID string) {
	go func() {
		time.Sleep(200 * time.Millisecond)
		_ = s.UpdateAgentStatus(context.Background(), agentID, store.AgentStatusUpdate{
			Phase: string(state.PhaseRunning), Activity: "idle",
		})
	}()
}

// saverBWake suspends the agent (as the dev user) and wakes it with no
// caller identity, as a hub-internal wake does.
func saverBWake(t *testing.T, srv *Server, s store.Store, agentID string) {
	t.Helper()
	pendingLifecycleWarnings(t, srv, agentID, "suspend")
	suspended := pendingReloadAgent(t, s, agentID)
	require.Equal(t, string(state.PhaseSuspended), suspended.Phase)
	saverBReportRunning(s, agentID)
	result, dmErr := srv.wakeAgentForDM(context.Background(), suspended)
	require.Nil(t, dmErr)
	require.NotNil(t, result)
	require.Equal(t, WakeResumed, result.Outcome)
}

// saverBGetUserCounter records every GetUser id it serves.
type saverBGetUserCounter struct {
	store.Store
	mu  sync.Mutex
	ids []string
}

func (c *saverBGetUserCounter) GetUser(ctx context.Context, id string) (*store.User, error) {
	c.mu.Lock()
	c.ids = append(c.ids, id)
	c.mu.Unlock()
	return c.Store.GetUser(ctx, id)
}

func (c *saverBGetUserCounter) seen(id string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, got := range c.ids {
		if got == id {
			return true
		}
	}
	return false
}

// TestPendingFold_MaintenanceDefersThenFolds (R-1): while maintenance mode
// is on, a non-admin saver's held edit is deferred (kept pending, neither
// applied nor discarded, warned with the maintenance notice and never as
// held for reincarnation), is superseded by an admin's write-through, and
// folds once maintenance ends.
func TestPendingFold_MaintenanceDefersThenFolds(t *testing.T) {
	maintenanceWarning := pendingFoldWarnings(nil, []string{"config.model"})[0]

	t.Run("(i) deferred on the write and early-return paths, (ii) folds after", func(t *testing.T) {
		srv, s, disp, agent := heldEditsFixture(t, state.PhaseRunning)
		saver := effectUser(t, s, agent.ProjectID, "maint-saver")
		effectPatchAs(t, srv, saver, agent.ID, map[string]interface{}{"config": map[string]interface{}{"model": "user-model"}})
		// An admin's (dev) held edit of another key folds on the same
		// start, so the first start takes the write path.
		patchPending(t, srv, agent.ID, map[string]interface{}{"config": map[string]interface{}{"max_turns": 7}})
		srv.maintenance.SetEnabled(true)
		t.Cleanup(func() { srv.maintenance.SetEnabled(false) })

		warnings := effectStopStart(t, srv, agent.ID)
		starts := disp.snapshot()
		require.Len(t, starts, 1)
		assert.Equal(t, "old-model", starts[0].model, "the deferred model is not applied")
		assert.Equal(t, api.IntPtr(7), starts[0].inline.MaxTurns, "the admin's edit folds")
		after := pendingReloadAgent(t, s, agent.ID)
		require.NotNil(t, after.PendingConfig)
		assert.Equal(t, []string{"config.model"}, pendingConfigKeys(after.PendingConfig))
		assert.Contains(t, warnings, maintenanceWarning)
		assertNoHeldForReincarnate(t, warnings, "model")
		applied := pendingAudits(t, s, agent.ID, mutationTypeAgentPendingConfigApplied, 1)
		keys, _ := pendingAuditSummary(t, applied[0])
		assert.Equal(t, []string{"config.max_turns"}, keys, "the deferred key is not audited as applied")
		effectAssertNoDiscard(t, s, agent.ID)

		// Early-return path: model is the only pending key.
		warnings = effectStopStart(t, srv, agent.ID)
		starts = disp.snapshot()
		require.Len(t, starts, 2)
		assert.Equal(t, "old-model", starts[1].model)
		assert.Contains(t, warnings, maintenanceWarning)
		assertNoHeldForReincarnate(t, warnings, "model")
		require.NotNil(t, pendingReloadAgent(t, s, agent.ID).PendingConfig)
		effectAssertNoDiscard(t, s, agent.ID)

		// (ii) Maintenance ends: the next start folds model.
		srv.maintenance.SetEnabled(false)
		effectStopStart(t, srv, agent.ID)
		starts = disp.snapshot()
		require.Len(t, starts, 3)
		assert.Equal(t, "user-model", starts[2].model)
		assert.Nil(t, pendingReloadAgent(t, s, agent.ID).PendingConfig)
		applied = pendingAudits(t, s, agent.ID, mutationTypeAgentPendingConfigApplied, 2)
		var appliedKeys []string
		for _, rec := range applied {
			k, _ := pendingAuditSummary(t, rec)
			appliedKeys = append(appliedKeys, k...)
		}
		assert.Contains(t, appliedKeys, "config.model")
	})

	t.Run("(iii) an admin write-through supersedes the deferred value", func(t *testing.T) {
		srv, s, disp, agent := heldEditsFixture(t, state.PhaseRunning)
		saver := effectUser(t, s, agent.ProjectID, "maint-saver")
		effectPatchAs(t, srv, saver, agent.ID, map[string]interface{}{"config": map[string]interface{}{"model": "stale-model"}})
		pendingLifecycleWarnings(t, srv, agent.ID, "stop")
		srv.maintenance.SetEnabled(true)
		t.Cleanup(func() { srv.maintenance.SetEnabled(false) })

		resp := patchPending(t, srv, agent.ID, map[string]interface{}{"config": map[string]interface{}{"model": "admin-model"}})
		assert.Equal(t, []string{"config.model"}, resp.Disposition.Applied)
		assert.Nil(t, pendingReloadAgent(t, s, agent.ID).PendingConfig, "the deferred value is superseded")
		assert.Equal(t, map[string][]string{pendingDiscardTriggerSuperseded: {"config.model"}}, effectDiscards(t, s, agent.ID, 1))

		srv.maintenance.SetEnabled(false)
		pendingLifecycleWarnings(t, srv, agent.ID, "start")
		starts := disp.snapshot()
		require.Len(t, starts, 1)
		assert.Equal(t, "admin-model", starts[0].model, "never the stale value")
	})

	t.Run("(iii-b) an admin edit of another key warns about the deferred key", func(t *testing.T) {
		srv, s, _, agent := heldEditsFixture(t, state.PhaseRunning)
		saver := effectUser(t, s, agent.ProjectID, "maint-saver")
		effectPatchAs(t, srv, saver, agent.ID, map[string]interface{}{"config": map[string]interface{}{"model": "user-model"}})
		pendingLifecycleWarnings(t, srv, agent.ID, "stop")
		srv.maintenance.SetEnabled(true)
		t.Cleanup(func() { srv.maintenance.SetEnabled(false) })

		resp := patchPending(t, srv, agent.ID, map[string]interface{}{"config": map[string]interface{}{"max_turns": 9}})
		assert.Contains(t, resp.Warnings, maintenanceWarning)
		after := pendingReloadAgent(t, s, agent.ID)
		require.NotNil(t, after.PendingConfig)
		assert.Equal(t, []string{"config.model"}, pendingConfigKeys(after.PendingConfig))
		effectAssertNoDiscard(t, s, agent.ID)
	})

	t.Run("(iv) a hub-internal wake defers and records the notice in the status message", func(t *testing.T) {
		srv, s, disp, agent := heldEditsFixture(t, state.PhaseRunning)
		saver := effectUser(t, s, agent.ProjectID, "maint-saver")
		effectPatchAs(t, srv, saver, agent.ID, map[string]interface{}{"config": map[string]interface{}{"model": "user-model"}})
		pendingLifecycleWarnings(t, srv, agent.ID, "suspend")
		suspended := pendingReloadAgent(t, s, agent.ID)
		require.Equal(t, string(state.PhaseSuspended), suspended.Phase)
		srv.maintenance.SetEnabled(true)
		t.Cleanup(func() { srv.maintenance.SetEnabled(false) })

		saverBReportRunning(s, agent.ID)
		result, dmErr := srv.wakeAgentForDM(context.Background(), suspended)
		require.Nil(t, dmErr)
		require.NotNil(t, result)
		assert.Equal(t, WakeResumed, result.Outcome)

		starts := disp.snapshot()
		require.Len(t, starts, 1)
		assert.Equal(t, "old-model", starts[0].model)
		after := pendingReloadAgent(t, s, agent.ID)
		require.NotNil(t, after.PendingConfig)
		assert.Equal(t, []string{"config.model"}, pendingConfigKeys(after.PendingConfig))
		assert.Contains(t, after.Message, pendingDeferredNote)
		assert.Contains(t, after.Message, "config.model")
		assert.NotContains(t, after.Message, heldReincarnateWarningPrefix)
		effectAssertNoDiscard(t, s, agent.ID)
		effectAssertNoAppliedAudit(t, s, agent.ID)
	})
}

// TestPendingSaverKind_UserAllowedFolds: a user saver who still holds
// agent.update has the edit applied, under a precise user: ref.
func TestPendingSaverKind_UserAllowedFolds(t *testing.T) {
	srv, s, disp, agent := heldEditsFixture(t, state.PhaseRunning)
	saver := effectUser(t, s, agent.ProjectID, "allowed")
	effectPatchAs(t, srv, saver, agent.ID, map[string]interface{}{"config": map[string]interface{}{"model": "user-model"}})
	stored := pendingReloadAgent(t, s, agent.ID)
	require.NotNil(t, stored.PendingConfig)
	assert.Equal(t, map[string]string{"model": "user:" + saver.ID}, stored.PendingConfig.KeySavers)

	effectStopStart(t, srv, agent.ID)
	saverBAssertFolded(t, s, disp, agent.ID, "user-model")
}

// TestPendingSaverKind_UserDeniedDropped: a user saver who lost agent.update
// has the edit dropped with saver_unauthorized, audited and warned.
func TestPendingSaverKind_UserDeniedDropped(t *testing.T) {
	srv, s, disp, agent := heldEditsFixture(t, state.PhaseRunning)
	saver := effectUser(t, s, agent.ProjectID, "denied")
	effectPatchAs(t, srv, saver, agent.ID, map[string]interface{}{"config": map[string]interface{}{"model": "user-model"}})
	effectRevokeGrants(t, s, saver.ID)

	warnings := effectStopStart(t, srv, agent.ID)
	saverBAssertDropped(t, s, disp, agent.ID, warnings, pendingDiscardTriggerSaverUnauthorized)
}

// TestPendingSaverKind_UserInactiveDropped (ruling A): a suspended saver's
// edit is dropped with saver_account_inactive.
func TestPendingSaverKind_UserInactiveDropped(t *testing.T) {
	srv, s, disp, agent := heldEditsFixture(t, state.PhaseRunning)
	saver := effectUser(t, s, agent.ProjectID, "inactive")
	effectPatchAs(t, srv, saver, agent.ID, map[string]interface{}{"config": map[string]interface{}{"model": "user-model"}})
	effectSetUserStatus(t, s, saver.ID, store.UserStatusSuspended)

	warnings := effectStopStart(t, srv, agent.ID)
	saverBAssertDropped(t, s, disp, agent.ID, warnings, pendingDiscardTriggerSaverAccountInactive)
}

// TestPendingSaverKind_UserRowGates: a user: ref whose row no live user arm
// admits is dropped with saver_unresolvable: a federated row, a transport
// identity's email. (Test-fixture and admin-kind rows are covered by
// TestPendingReauth_MirrorsLivePatchGates and
// TestPendingSaverKind_TestAdminRowMapping; a reserved-domain non-fixture
// row cannot be created through the store, which refuses it.)
func TestPendingSaverKind_UserRowGates(t *testing.T) {
	t.Run("federated row", func(t *testing.T) {
		srv, s, disp, agent := heldEditsFixture(t, state.PhaseStopped)
		fed := saverBFederatedUser(t, s, "fed-row@corp.example")
		effectSeedPending(t, s, agent.ID, map[string]string{"model": `"seeded-model"`}, map[string]string{"model": "user:" + fed.ID}, "user:"+fed.ID)
		warnings := pendingLifecycleWarnings(t, srv, agent.ID, "start")
		saverBAssertDropped(t, s, disp, agent.ID, warnings, pendingDiscardTriggerSaverUnresolvable)
	})

	t.Run("transport identity email", func(t *testing.T) {
		srv, s, disp, agent := heldEditsFixture(t, state.PhaseStopped)
		saver := effectUser(t, s, agent.ProjectID, "transport")
		// The row's email is now the hub's platform transport identity.
		srv.platformAuthSA = saver.Email
		effectSeedPending(t, s, agent.ID, map[string]string{"model": `"seeded-model"`}, map[string]string{"model": "user:" + saver.ID}, "user:"+saver.ID)
		warnings := pendingLifecycleWarnings(t, srv, agent.ID, "start")
		saverBAssertDropped(t, s, disp, agent.ID, warnings, pendingDiscardTriggerSaverUnresolvable)
	})
}

// TestPendingSaverKind_DevWorkstationFolds: the workstation dev user's save
// is stored as dev:<DevUserID> and re-checked through the same CheckAccess,
// and a legacy user:<DevUserID> row maps to the dev saver.
func TestPendingSaverKind_DevWorkstationFolds(t *testing.T) {
	t.Run("precise dev ref", func(t *testing.T) {
		srv, s, disp, agent := heldEditsFixture(t, state.PhaseRunning)
		patchPending(t, srv, agent.ID, map[string]interface{}{"config": map[string]interface{}{"model": "dev-model"}})
		stored := pendingReloadAgent(t, s, agent.ID)
		require.NotNil(t, stored.PendingConfig)
		assert.Equal(t, "dev:"+DevUserID, stored.PendingConfig.KeySavers["model"])
		effectStopStart(t, srv, agent.ID)
		saverBAssertFolded(t, s, disp, agent.ID, "dev-model")
	})

	t.Run("legacy user:<DevUserID>", func(t *testing.T) {
		_, s, disp, agent, _ := saverBSeededStart(t, "user:"+DevUserID, true)
		saverBAssertFolded(t, s, disp, agent.ID, "seeded-model")
	})
}

// TestPendingSaverKind_AgentDropped: an agent saver is not rebuilt in v1:
// its edit is dropped with saver_unresolvable.
func TestPendingSaverKind_AgentDropped(t *testing.T) {
	_, s, disp, agent, warnings := saverBSeededStart(t, "agent:"+tid("ae6-saver-agent"), false)
	saverBAssertDropped(t, s, disp, agent.ID, warnings, pendingDiscardTriggerSaverUnresolvable)
}

// TestPendingSaverKind_FederatedUserDropped: a federated user saver, by its
// precise ref or a legacy user:<issuer>:<subject>, is dropped with
// saver_unresolvable.
func TestPendingSaverKind_FederatedUserDropped(t *testing.T) {
	for name, tc := range map[string]struct {
		ref    string
		legacy bool
	}{
		"precise ref": {"federated_user:https://iss.example.com:sub-1", false},
		"legacy ref":  {"user:https://iss.example.com:sub-1", true},
	} {
		t.Run(name, func(t *testing.T) {
			_, s, disp, agent, warnings := saverBSeededStart(t, tc.ref, tc.legacy)
			saverBAssertDropped(t, s, disp, agent.ID, warnings, pendingDiscardTriggerSaverUnresolvable)
		})
	}
}

// TestPendingSaverKind_UnsupportedKindsDropped (note a): every kind the
// ruling does not rebuild, an unknown kind and an empty ref are dropped
// with saver_unresolvable.
func TestPendingSaverKind_UnsupportedKindsDropped(t *testing.T) {
	for _, ref := range []string{
		"federated_agent:https://iss.example.com:agent-1",
		"visitor:" + tid("ae6-visitor"),
		"broker:" + tid("ae6-broker"),
		"federated_service:https://iss.example.com:svc",
		"dev_untrusted:" + DevUserID,
		"bogus:x",
		"",
	} {
		t.Run(ref, func(t *testing.T) {
			_, s, disp, agent, warnings := saverBSeededStart(t, ref, false)
			saverBAssertDropped(t, s, disp, agent.ID, warnings, pendingDiscardTriggerSaverUnresolvable)
		})
	}
}

// TestPendingSaverLegacy_VisitorRefDropped (ruling B): a legacy user:<id>
// whose row is a federated (visitor) account is dropped with
// saver_unresolvable, never re-checked as a native user. (No seam records
// CheckAccess calls by principal, so the absence of a re-check is shown by
// the trigger: a re-checked user with no grant would be saver_unauthorized.)
func TestPendingSaverLegacy_VisitorRefDropped(t *testing.T) {
	srv, s, disp, agent := heldEditsFixture(t, state.PhaseStopped)
	visitor := saverBFederatedUser(t, s, "legacy-visitor@corp.example")
	effectSeedPending(t, s, agent.ID, map[string]string{"model": `"seeded-model"`}, nil, "user:"+visitor.ID)
	warnings := pendingLifecycleWarnings(t, srv, agent.ID, "start")
	saverBAssertDropped(t, s, disp, agent.ID, warnings, pendingDiscardTriggerSaverUnresolvable)
}

// TestPendingSaverLegacy_NativeRefRechecked (ruling B): a legacy user:<id>
// of a native row gets the full re-check.
func TestPendingSaverLegacy_NativeRefRechecked(t *testing.T) {
	setup := func(t *testing.T) (*Server, store.Store, *inlineCaptureDispatcher, *store.Agent, *store.User) {
		srv, s, disp, agent := heldEditsFixture(t, state.PhaseStopped)
		saver := effectUser(t, s, agent.ProjectID, "legacy-native")
		effectSeedPending(t, s, agent.ID, map[string]string{"model": `"seeded-model"`}, nil, "user:"+saver.ID)
		return srv, s, disp, agent, saver
	}

	t.Run("active and allowed folds", func(t *testing.T) {
		srv, s, disp, agent, _ := setup(t)
		pendingLifecycleWarnings(t, srv, agent.ID, "start")
		saverBAssertFolded(t, s, disp, agent.ID, "seeded-model")
	})
	t.Run("deleted is saver_unauthorized", func(t *testing.T) {
		srv, s, disp, agent, saver := setup(t)
		effectRevokeGrants(t, s, saver.ID)
		require.NoError(t, s.DeleteUser(context.Background(), saver.ID))
		warnings := pendingLifecycleWarnings(t, srv, agent.ID, "start")
		saverBAssertDropped(t, s, disp, agent.ID, warnings, pendingDiscardTriggerSaverUnauthorized)
	})
	t.Run("not active is saver_account_inactive", func(t *testing.T) {
		srv, s, disp, agent, saver := setup(t)
		effectSetUserStatus(t, s, saver.ID, store.UserStatusInvited)
		warnings := pendingLifecycleWarnings(t, srv, agent.ID, "start")
		saverBAssertDropped(t, s, disp, agent.ID, warnings, pendingDiscardTriggerSaverAccountInactive)
	})
	t.Run("grant removed is saver_unauthorized", func(t *testing.T) {
		srv, s, disp, agent, saver := setup(t)
		effectRevokeGrants(t, s, saver.ID)
		warnings := pendingLifecycleWarnings(t, srv, agent.ID, "start")
		saverBAssertDropped(t, s, disp, agent.ID, warnings, pendingDiscardTriggerSaverUnauthorized)
	})
}

// TestPendingSaverLegacy_FederatedUserRefNoRowRead (F2): a legacy
// user:<issuer>:<subject> is a federated user, dropped with
// saver_unresolvable before any row read.
func TestPendingSaverLegacy_FederatedUserRefNoRowRead(t *testing.T) {
	srv, s, disp, agent := heldEditsFixture(t, state.PhaseStopped)
	const id = "https://iss.example.com:sub-legacy"
	effectSeedPending(t, s, agent.ID, map[string]string{"model": `"seeded-model"`}, nil, "user:"+id)
	counter, _ := installStoreFault(t, srv, func(inner store.Store, _ *storeFaultSwitch) *saverBGetUserCounter {
		return &saverBGetUserCounter{Store: inner}
	})
	warnings := pendingLifecycleWarnings(t, srv, agent.ID, "start")
	saverBAssertDropped(t, s, disp, agent.ID, warnings, pendingDiscardTriggerSaverUnresolvable)
	assert.False(t, counter.seen(id), "no row is read for a federated user ref")
}

// TestPendingReauth_SystemActorWake (I1-c): a hub-internal wake re-checks
// each key against its saver; a drop is audited with the system actor and
// named in the stored status message, also when nothing is held (N-3).
func TestPendingReauth_SystemActorWake(t *testing.T) {
	t.Run("drop beside a held key", func(t *testing.T) {
		srv, s, disp, agent := heldEditsFixture(t, state.PhaseRunning)
		saver := effectUser(t, s, agent.ProjectID, "wake-saver")
		effectPatchAs(t, srv, saver, agent.ID, map[string]interface{}{"config": map[string]interface{}{"model": "user-model"}})
		patchPending(t, srv, agent.ID, map[string]interface{}{"config": map[string]interface{}{"system_prompt": "held prompt"}})
		effectRevokeGrants(t, s, saver.ID)

		saverBWake(t, srv, s, agent.ID)
		starts := disp.snapshot()
		require.Len(t, starts, 1)
		assert.Equal(t, "old-model", starts[0].model)
		recs := pendingAudits(t, s, agent.ID, mutationTypeAgentPendingConfigDiscarded, 1)
		keys, trigger := pendingAuditSummary(t, recs[0])
		assert.Equal(t, []string{"config.model"}, keys)
		assert.Equal(t, pendingDiscardTriggerSaverUnauthorized, trigger)
		assert.Equal(t, "system", recs[0].ActorPrincipalKind)
		assert.Equal(t, pendingConfigSystemActor, recs[0].ActorPrincipalID)
		after := pendingReloadAgent(t, s, agent.ID)
		assert.Contains(t, after.Message, "config.model: "+pendingSaverUnauthorizedNote)
		assert.Contains(t, after.Message, heldReincarnateWarningPrefix, "the held key is noted in the same message")
	})

	t.Run("only drops still writes the notice", func(t *testing.T) {
		srv, s, _, agent := heldEditsFixture(t, state.PhaseRunning)
		saver := effectUser(t, s, agent.ProjectID, "wake-saver")
		effectPatchAs(t, srv, saver, agent.ID, map[string]interface{}{"config": map[string]interface{}{"model": "user-model"}})
		effectRevokeGrants(t, s, saver.ID)

		saverBWake(t, srv, s, agent.ID)
		assert.Equal(t, map[string][]string{pendingDiscardTriggerSaverUnauthorized: {"config.model"}}, effectDiscards(t, s, agent.ID, 1))
		after := pendingReloadAgent(t, s, agent.ID)
		assert.Nil(t, after.PendingConfig)
		assert.Contains(t, after.Message, "config.model: "+pendingSaverUnauthorizedNote)
		assert.False(t, strings.Contains(after.Message, heldReincarnateWarningPrefix))
	})
}

// ---- Group A (ptone/scion#5864): the §4.0 gate map, the M3 test-admin
// mapping, confirm-before-drop, the F3 faults and the distinct triggers.

// errGateAInjected is the store fault the group A seams inject.
var errGateAInjected = errors.New("injected authz store fault")

// gateAAuthzFaultStore wraps the authorization service's store. While
// armed, the role-binding loads of one user (ListRoleBindingsForPrincipals
// with user:userID in the closure) follow a script: the first matching
// call answers first, every later one answers rest. "empty" returns no
// bindings and no error (an untagged lookup fault: decide denies with no
// cause), "error" fails the load (decide tags it DenyCauseResolutionError)
// and "pass" delegates.
type gateAAuthzFaultStore struct {
	store.Store
	mu     sync.Mutex
	userID string
	armed  bool
	calls  int
	first  string
	rest   string
}

func (w *gateAAuthzFaultStore) ListRoleBindingsForPrincipals(ctx context.Context, principals []store.PrincipalRef, scopeTypes []string, scopeIDs []string) ([]*store.RoleBinding, error) {
	mode := "pass"
	w.mu.Lock()
	if w.armed {
		for _, p := range principals {
			if p.Type == store.RoleBindingPrincipalUser && p.ID == w.userID {
				w.calls++
				if w.calls == 1 {
					mode = w.first
				} else {
					mode = w.rest
				}
				break
			}
		}
	}
	w.mu.Unlock()
	switch mode {
	case "empty":
		return nil, nil
	case "error":
		return nil, errGateAInjected
	}
	return w.Store.ListRoleBindingsForPrincipals(ctx, principals, scopeTypes, scopeIDs)
}

// arm starts the script for userID.
func (w *gateAAuthzFaultStore) arm(userID, first, rest string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.userID, w.first, w.rest, w.armed, w.calls = userID, first, rest, true, 0
}

// gateAInstallAuthzFault installs a disarmed gateAAuthzFaultStore as the
// authorization service's store (the services New builds keep the raw
// store, so installStoreFault on srv.store does not reach CheckAccess).
// Installed right after the fixture, before any request; restored on
// cleanup.
func gateAInstallAuthzFault(t *testing.T, srv *Server) *gateAAuthzFaultStore {
	t.Helper()
	inner := srv.authzService.store
	w := &gateAAuthzFaultStore{Store: inner}
	srv.authzService.store = w
	t.Cleanup(func() { srv.authzService.store = inner })
	return w
}

// gateAStart posts a start as the dev user and returns the recorder.
func gateAStart(t *testing.T, srv *Server, agentID string) *httptest.ResponseRecorder {
	t.Helper()
	return doRequest(t, srv, http.MethodPost, "/api/v1/agents/"+agentID+"/start", nil)
}

// gateARowSnapshot is the stored pending config (as storage encodes it)
// and applied config of the agent, for byte-identity checks across a
// refused start (the start claim's own intent columns are not compared).
func gateARowSnapshot(t *testing.T, s store.Store, agentID string) (string, string) {
	t.Helper()
	pending, _ := effectStoredPending(t, s, agentID)
	a := pendingReloadAgent(t, s, agentID)
	type raw store.AgentAppliedConfig
	applied, err := json.Marshal((*raw)(a.AppliedConfig))
	require.NoError(t, err)
	return pending, string(applied)
}

// gateALivePatch PATCHes the agent's model as user and returns the status.
func gateALivePatch(t *testing.T, srv *Server, user *store.User, agentID string) int {
	t.Helper()
	return doRequestAsUser(t, srv, user, http.MethodPatch, "/api/v1/agents/"+agentID,
		map[string]interface{}{"config": map[string]interface{}{"model": "live-model"}}).Code
}

// gateASeedModel stores a pending model edit saved by ref on the agent.
func gateASeedModel(t *testing.T, s store.Store, agentID, ref string) {
	t.Helper()
	effectSeedPending(t, s, agentID, map[string]string{"model": `"held-model"`}, map[string]string{"model": ref}, ref)
}

// gateAStrPtr returns a pointer to v.
func gateAStrPtr(v string) *string { return &v }

// gateAFixtureUser creates a member-tier test fixture row expiring at
// expires.
func gateAFixtureUser(t *testing.T, s store.Store, name string, expires time.Time) *store.User {
	t.Helper()
	id := tid("ae6-fx-" + name + "-" + t.Name())
	u := &store.User{
		ID: id, Email: "fx-" + id[:8] + "@" + store.TestFixtureEmailDomain, DisplayName: "fixture " + name,
		Role: store.UserRoleMember, Status: store.UserStatusActive, Kind: store.UserKindTestFixture,
		ExpiresAt: &expires, IssuedBy: gateAStrPtr(DevUserID),
	}
	require.NoError(t, s.CreateTestFixtureUser(context.Background(), u))
	return u
}

// gateAAdminUser creates a valid test_hub_admin row (its kind's domain and
// role) expiring at expires, through the store's only admin-kind path.
func gateAAdminUser(t *testing.T, s store.Store, name string, expires time.Time) *store.User {
	t.Helper()
	ctx := context.Background()
	rd, err := s.GetRoleDefinitionByName(ctx, store.SystemRoleHubAdmin, store.RoleScopeSystem)
	require.NoError(t, err)
	id := tid("ae6-ta-" + name + "-" + t.Name())
	u := &store.User{
		ID: id, Email: "ta-" + id[:8] + "@" + store.TestHubAdminEmailDomain, DisplayName: "test admin " + name,
		Role: store.TestAdminRole(store.UserKindTestHubAdmin), Status: store.UserStatusActive, Kind: store.UserKindTestHubAdmin,
		ExpiresAt: &expires, IssuedBy: gateAStrPtr(DevUserID), IssuanceID: gateAStrPtr(generateID()),
	}
	_, err = s.CreateTestAdminUser(ctx, u, rd.ID)
	require.NoError(t, err)
	return u
}

// gateARawAdminRow inserts a test_hub_admin row with any email and role
// directly in the database: no store path writes an admin-kind row out of
// its domain or with another role (O-3), but the row gate must still map
// one.
func gateARawAdminRow(t *testing.T, s store.Store, email, role string, expires time.Time) string {
	t.Helper()
	raw, ok := s.(interface {
		DB() *sql.DB
		Dialect() string
	})
	require.True(t, ok, "the test store exposes its database")
	id := generateID()
	q := "INSERT INTO users (id, email, display_name, role, status, created, session_generation, kind, expires_at, issued_by, issuance_id) VALUES (?, ?, 'raw test admin', ?, 'active', ?, 0, 'test_hub_admin', ?, ?, ?)"
	_, err := raw.DB().ExecContext(context.Background(), taRebind(raw.Dialect(), q), id, email, role, time.Now().UTC(), expires.UTC(), DevUserID, generateID())
	require.NoError(t, err)
	return id
}

// gateAAdvanceClocks moves the clocks of the fold's and the JWT arm's row
// gates forward by d.
func gateAAdvanceClocks(srv *Server, d time.Duration) {
	now := func() time.Time { return time.Now().Add(d) }
	srv.testIdentities.now = now
	srv.authConfig.Now = now
}

// TestPendingReauth_DenyCauseMapExhaustive: every DenyCause constant in the
// package, found by parsing its non-test sources, has a fold mapping, and
// every cause Decision.IsIndeterminate reports is mapped as a fault.
func TestPendingReauth_DenyCauseMapExhaustive(t *testing.T) {
	files, err := filepath.Glob("*.go")
	require.NoError(t, err)
	fset := token.NewFileSet()
	var found []DenyCause
	for _, name := range files {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, name, nil, 0)
		require.NoError(t, err, name)
		for _, decl := range f.Decls {
			gd, ok := decl.(*ast.GenDecl)
			if !ok || gd.Tok != token.CONST {
				continue
			}
			for _, spec := range gd.Specs {
				vs := spec.(*ast.ValueSpec)
				typ, ok := vs.Type.(*ast.Ident)
				if !ok || typ.Name != "DenyCause" {
					continue
				}
				for i := range vs.Names {
					require.Less(t, i, len(vs.Values), "%s: DenyCause const %s has no value", name, vs.Names[i].Name)
					lit, ok := vs.Values[i].(*ast.BasicLit)
					require.True(t, ok, "%s: DenyCause const %s is not a string literal", name, vs.Names[i].Name)
					v, err := strconv.Unquote(lit.Value)
					require.NoError(t, err)
					found = append(found, DenyCause(v))
				}
			}
		}
	}
	// Non-empty control: the package declares the 11 named causes the
	// plan mapped (authz.go; the 12th mapping is the empty cause), so a
	// parse that finds nothing fails here.
	require.GreaterOrEqual(t, len(found), 11, "found DenyCause constants: %v", found)
	for _, c := range found {
		_, ok := pendingFoldDenyCauses[c]
		assert.True(t, ok, "DenyCause %q has no mapping in pendingFoldDenyCauses", c)
	}
	_, ok := pendingFoldDenyCauses[""]
	assert.True(t, ok, "the empty cause (a policy deny) is mapped")
	for c, res := range pendingFoldDenyCauses {
		if (Decision{DenyCause: c}).IsIndeterminate() {
			assert.Equal(t, pendingFoldDenyFault, res, "IsIndeterminate cause %q must refuse the fold", c)
		}
		assert.Equal(t, res == pendingFoldDenyFault, pendingFoldDenyIndeterminate(Decision{DenyCause: c}), "cause %q", c)
	}
}

// TestPendingReauth_MirrorsLivePatchGates: for each principal gate on the
// live PATCH path (§4.0(a)), a live PATCH as such a principal is refused,
// and the fold of a key that principal saved drops it with the mapped
// trigger (or, for maintenance, defers it).
func TestPendingReauth_MirrorsLivePatchGates(t *testing.T) {
	// dropCase starts the agent as the dev user with a pending model saved
	// by ref, and asserts the drop: not dispatched, removed, one discarded
	// audit with trigger, and the warning.
	dropCase := func(t *testing.T, srv *Server, s store.Store, disp *inlineCaptureDispatcher, agentID, ref, trigger string) {
		t.Helper()
		gateASeedModel(t, s, agentID, ref)
		warnings := pendingLifecycleWarnings(t, srv, agentID, "start")
		starts := disp.snapshot()
		require.Len(t, starts, 1)
		assert.Equal(t, "old-model", starts[0].model, "the dropped edit is not applied")
		assert.Nil(t, pendingReloadAgent(t, s, agentID).PendingConfig)
		assert.Equal(t, map[string][]string{trigger: {"config.model"}}, effectDiscards(t, s, agentID, 1))
		assert.Contains(t, warnings, effectDropWarning(trigger, "config.model"))
		effectAssertNoAppliedAudit(t, s, agentID)
	}

	t.Run("account suspended", func(t *testing.T) {
		srv, s, disp, agent := heldEditsFixture(t, state.PhaseStopped)
		u := effectUser(t, s, agent.ProjectID, "susp")
		effectSetUserStatus(t, s, u.ID, store.UserStatusSuspended)
		assert.Equal(t, http.StatusForbidden, gateALivePatch(t, srv, u, agent.ID), "live: user_suspended")
		dropCase(t, srv, s, disp, agent.ID, "user:"+u.ID, pendingDiscardTriggerSaverAccountInactive)
	})

	t.Run("account row missing", func(t *testing.T) {
		srv, s, disp, agent := heldEditsFixture(t, state.PhaseStopped)
		u := effectUser(t, s, agent.ProjectID, "gone")
		require.NoError(t, s.DeleteUser(context.Background(), u.ID))
		assert.Equal(t, http.StatusUnauthorized, gateALivePatch(t, srv, u, agent.ID), "live: no user record")
		dropCase(t, srv, s, disp, agent.ID, "user:"+u.ID, pendingDiscardTriggerSaverUnauthorized)
	})

	t.Run("federated row", func(t *testing.T) {
		srv, s, disp, agent := heldEditsFixture(t, state.PhaseStopped)
		issuer, email := "https://hub-b.example.com", "fed-"+tid("x")[:6]+"@corp.example"
		u := &store.User{DisplayName: "federated", Kind: store.UserKindFederated, Status: store.UserStatusActive, HomeIssuer: &issuer, FederatedEmail: &email}
		require.NoError(t, s.CreateFederatedUser(context.Background(), u))
		require.NotEmpty(t, u.ID)
		assert.Equal(t, http.StatusUnauthorized, gateALivePatch(t, srv, u, agent.ID), "live: a hub token never admits a federated row")
		dropCase(t, srv, s, disp, agent.ID, "user:"+u.ID, pendingDiscardTriggerSaverUnresolvable)
	})

	t.Run("transport identity email", func(t *testing.T) {
		srv, s, disp, agent := heldEditsFixture(t, state.PhaseStopped)
		u := effectUser(t, s, agent.ProjectID, "transport")
		srv.platformAuthSA, srv.authConfig.PlatformAuthSA = u.Email, u.Email
		assert.Equal(t, http.StatusUnauthorized, gateALivePatch(t, srv, u, agent.ID), "live: transport identity")
		dropCase(t, srv, s, disp, agent.ID, "user:"+u.ID, pendingDiscardTriggerSaverUnresolvable)
	})

	t.Run("test fixture with the feature off", func(t *testing.T) {
		srv, s, disp, agent := heldEditsFixture(t, state.PhaseStopped)
		require.False(t, srv.testIdentities.enabled)
		u := gateAFixtureUser(t, s, "off", time.Now().Add(time.Hour))
		assert.Equal(t, http.StatusUnauthorized, gateALivePatch(t, srv, u, agent.ID), "live: test_identities_disabled")
		dropCase(t, srv, s, disp, agent.ID, "user:"+u.ID, pendingDiscardTriggerSaverAccountInactive)
	})

	t.Run("test fixture expired", func(t *testing.T) {
		srv, s, disp, agent := heldEditsFixture(t, state.PhaseStopped)
		enableTestIdentitiesForTest(srv)
		u := gateAFixtureUser(t, s, "exp", time.Now().Add(time.Hour))
		gateAAdvanceClocks(srv, 2*time.Hour)
		assert.Equal(t, http.StatusUnauthorized, gateALivePatch(t, srv, u, agent.ID), "live: test_identity_expired")
		dropCase(t, srv, s, disp, agent.ID, "user:"+u.ID, pendingDiscardTriggerSaverAccountInactive)
	})

	t.Run("admin-kind test row with its tier off", func(t *testing.T) {
		srv, s, disp, agent := heldEditsFixture(t, state.PhaseStopped)
		u := gateAAdminUser(t, s, "tieroff", time.Now().Add(time.Hour))
		assert.Equal(t, http.StatusUnauthorized, gateALivePatch(t, srv, u, agent.ID), "live: test_admin_tier_disabled")
		dropCase(t, srv, s, disp, agent.ID, "user:"+u.ID, pendingDiscardTriggerSaverAccountInactive)
	})

	t.Run("admin-kind test row expired", func(t *testing.T) {
		srv, s, disp, agent := heldEditsFixture(t, state.PhaseStopped)
		taEnableGate(srv)
		u := gateAAdminUser(t, s, "exp", time.Now().Add(time.Hour))
		gateAAdvanceClocks(srv, 2*time.Hour)
		assert.Equal(t, http.StatusUnauthorized, gateALivePatch(t, srv, u, agent.ID), "live: test_admin_identity_expired")
		dropCase(t, srv, s, disp, agent.ID, "user:"+u.ID, pendingDiscardTriggerSaverAccountInactive)
	})

	t.Run("maintenance mode: a non-admin saver is deferred", func(t *testing.T) {
		srv, s, disp, agent := heldEditsFixture(t, state.PhaseStopped)
		u := effectUser(t, s, agent.ProjectID, "maint")
		require.NotNil(t, srv.maintenance)
		srv.maintenance.SetEnabled(true)
		t.Cleanup(func() { srv.maintenance.SetEnabled(false) })
		assert.Equal(t, http.StatusServiceUnavailable, gateALivePatch(t, srv, u, agent.ID), "live: maintenance refuses non-admin users")

		gateASeedModel(t, s, agent.ID, "user:"+u.ID)
		warnings := pendingLifecycleWarnings(t, srv, agent.ID, "start")
		starts := disp.snapshot()
		require.Len(t, starts, 1)
		assert.Equal(t, "old-model", starts[0].model, "deferred, not applied")
		after := pendingReloadAgent(t, s, agent.ID)
		require.NotNil(t, after.PendingConfig)
		assert.Equal(t, []string{"config.model"}, pendingConfigKeys(after.PendingConfig), "deferred, not dropped")
		assert.Contains(t, warnings, "config.model: "+pendingDeferredNote)
		assert.NotContains(t, warnings, heldForReincarnateWarning([]string{"config.model"}))
		effectAssertNoDiscard(t, s, agent.ID)
		effectAssertNoAppliedAudit(t, s, agent.ID)
	})

	t.Run("policy deny: the saver lost agent.update", func(t *testing.T) {
		srv, s, disp, agent := heldEditsFixture(t, state.PhaseStopped)
		u := effectUser(t, s, agent.ProjectID, "denied")
		effectRevokeGrants(t, s, u.ID)
		assert.Equal(t, http.StatusForbidden, gateALivePatch(t, srv, u, agent.ID), "live: agent.update denied")
		dropCase(t, srv, s, disp, agent.ID, "user:"+u.ID, pendingDiscardTriggerSaverUnauthorized)
	})
}

// TestPendingSaverKind_TestAdminRowMapping (M3): an admin-kind test saver's
// row refusals map to the fold's own triggers, each a drop with exactly one
// discarded audit and a warning, and nothing reaches the test-admin
// security sink. A kind-read error is not a drop: the start answers 503
// with nothing written or audited. The error is injected at
// srv.authConfig.KindForPrincipal, keyed on the saver, so the different
// principal starting the agent (the dev user) is unaffected (R-A).
func TestPendingSaverKind_TestAdminRowMapping(t *testing.T) {
	// setup is a stopped agent on a server with a recording test-admin
	// sink; tierOn turns the hub-admin kind on.
	setup := func(t *testing.T, tierOn bool) (*Server, store.Store, *inlineCaptureDispatcher, *store.Agent, *taSinkBuffer) {
		srv, s, disp, agent := heldEditsFixture(t, state.PhaseStopped)
		if tierOn {
			taEnableGate(srv)
		}
		buf := &taSinkBuffer{}
		srv.testAdminAudit = newTestAdminAuditSink(buf)
		return srv, s, disp, agent, buf
	}
	dropRow := func(t *testing.T, srv *Server, s store.Store, disp *inlineCaptureDispatcher, agentID, userID, trigger string, buf *taSinkBuffer) {
		t.Helper()
		gateASeedModel(t, s, agentID, "user:"+userID)
		warnings := pendingLifecycleWarnings(t, srv, agentID, "start")
		require.Len(t, disp.snapshot(), 1)
		assert.Nil(t, pendingReloadAgent(t, s, agentID).PendingConfig)
		discards := pendingAudits(t, s, agentID, mutationTypeAgentPendingConfigDiscarded, 1)
		require.Len(t, discards, 1, "exactly one discarded audit")
		keys, got := pendingAuditSummary(t, discards[0])
		assert.Equal(t, []string{"config.model"}, keys)
		assert.Equal(t, trigger, got)
		assert.Contains(t, warnings, effectDropWarning(trigger, "config.model"))
		assert.Empty(t, buf.String(), "the fold writes nothing to the test-admin sink")
	}
	domain := "@" + store.TestHubAdminEmailDomain

	t.Run("tier off: saver_account_inactive", func(t *testing.T) {
		srv, s, disp, agent, buf := setup(t, false)
		u := gateAAdminUser(t, s, "tier", time.Now().Add(time.Hour))
		dropRow(t, srv, s, disp, agent.ID, u.ID, pendingDiscardTriggerSaverAccountInactive, buf)
	})

	t.Run("expired: saver_account_inactive", func(t *testing.T) {
		srv, s, disp, agent, buf := setup(t, true)
		u := gateAAdminUser(t, s, "exp", time.Now().Add(time.Hour))
		gateAAdvanceClocks(srv, 2*time.Hour)
		dropRow(t, srv, s, disp, agent.ID, u.ID, pendingDiscardTriggerSaverAccountInactive, buf)
	})

	t.Run("domain mismatch: saver_unresolvable", func(t *testing.T) {
		srv, s, disp, agent, buf := setup(t, true)
		id := gateARawAdminRow(t, s, "ta-mismatch-"+tid("d")[:6]+"@"+store.TestSuperAdminEmailDomain, store.TestAdminRole(store.UserKindTestHubAdmin), time.Now().Add(time.Hour))
		dropRow(t, srv, s, disp, agent.ID, id, pendingDiscardTriggerSaverUnresolvable, buf)
	})

	t.Run("role mismatch: saver_unresolvable", func(t *testing.T) {
		srv, s, disp, agent, buf := setup(t, true)
		id := gateARawAdminRow(t, s, "ta-role-"+tid("r")[:6]+domain, store.UserRoleAdmin, time.Now().Add(time.Hour))
		dropRow(t, srv, s, disp, agent.ID, id, pendingDiscardTriggerSaverUnresolvable, buf)
	})

	// The kind-read error and its positive control share a fixture: an
	// admin-kind saver whose tier is off. testAdminRowRejection reads the
	// kind before the tier, so with the stub the fold fails at the kind
	// read; without it the row reaches its own mapping.
	t.Run("kind-read error: 503, nothing dropped or written", func(t *testing.T) {
		srv, s, disp, agent, buf := setup(t, false)
		u := gateAAdminUser(t, s, "kinderr", time.Now().Add(time.Hour))
		gateASeedModel(t, s, agent.ID, "user:"+u.ID)
		orig := srv.authConfig.KindForPrincipal
		t.Cleanup(func() { srv.authConfig.KindForPrincipal = orig })
		srv.authConfig.KindForPrincipal = func(ctx context.Context, userID string) (principalKind, error) {
			if userID == u.ID {
				return principalKind{}, errGateAInjected
			}
			return orig(ctx, userID)
		}
		pendingBefore, appliedBefore := gateARowSnapshot(t, s, agent.ID)

		rec := gateAStart(t, srv, agent.ID)
		require.Equal(t, http.StatusServiceUnavailable, rec.Code, rec.Body.String())
		assert.Contains(t, rec.Body.String(), pendingReauthUnavailableMessage)
		assert.Empty(t, disp.snapshot(), "no dispatch")
		pendingAfter, appliedAfter := gateARowSnapshot(t, s, agent.ID)
		assert.Equal(t, pendingBefore, pendingAfter, "the pending config is byte-identical")
		assert.Equal(t, appliedBefore, appliedAfter, "the applied config is byte-identical")
		effectAssertNoDiscard(t, s, agent.ID)
		assert.Empty(t, buf.String(), "the fold writes nothing to the test-admin sink")
	})

	t.Run("positive control: without the stub the same row reaches its own mapping: saver_account_inactive", func(t *testing.T) {
		srv, s, disp, agent, buf := setup(t, false)
		u := gateAAdminUser(t, s, "kindok", time.Now().Add(time.Hour))
		orig := srv.authConfig.KindForPrincipal
		t.Cleanup(func() { srv.authConfig.KindForPrincipal = orig })
		dropRow(t, srv, s, disp, agent.ID, u.ID, pendingDiscardTriggerSaverAccountInactive, buf)
	})
}

// TestPendingReauth_ConfirmBeforeDrop: a deny with no cause is confirmed by
// one fresh evaluation before a key is dropped (§4.0(b)). The seam is a
// role-binding load of the saver that silently returns nothing once (an
// untagged fault: decide denies with an empty DenyCause). No agent.update
// grant in this fixture depends on isCurrentHubMember (it gates only the
// hub-scoped service-account assign), so the plan's GetGroupMembership
// seam cannot reach this check; the binding load is the saver's real
// agent.update path (project-admin binding).
func TestPendingReauth_ConfirmBeforeDrop(t *testing.T) {
	// setup: a running agent, a project-admin saver holding a model edit,
	// then stopped; the authz fault wrapper is installed before any
	// request and armed just before the start.
	setup := func(t *testing.T) (*Server, store.Store, *inlineCaptureDispatcher, *store.Agent, *store.User, *gateAAuthzFaultStore) {
		srv, s, disp, agent := heldEditsFixture(t, state.PhaseRunning)
		fault := gateAInstallAuthzFault(t, srv)
		u := effectUser(t, s, agent.ProjectID, "confirm")
		resp := effectPatchAs(t, srv, u, agent.ID, map[string]interface{}{"config": map[string]interface{}{"model": "held-model"}})
		require.Equal(t, []string{"config.model"}, resp.Disposition.Held)
		pendingLifecycleWarnings(t, srv, agent.ID, "stop")
		return srv, s, disp, agent, u, fault
	}

	t.Run("deny (untagged fault) then allow: the key folds", func(t *testing.T) {
		srv, s, disp, agent, u, fault := setup(t)
		fault.arm(u.ID, "empty", "pass")
		pendingLifecycleWarnings(t, srv, agent.ID, "start")
		starts := disp.snapshot()
		require.Len(t, starts, 1)
		assert.Equal(t, "held-model", starts[0].model)
		assert.Nil(t, pendingReloadAgent(t, s, agent.ID).PendingConfig)
		pendingAudits(t, s, agent.ID, mutationTypeAgentPendingConfigApplied, 1)
		effectAssertNoDiscard(t, s, agent.ID)
	})

	t.Run("deny then deny: saver_unauthorized", func(t *testing.T) {
		srv, s, disp, agent, u, fault := setup(t)
		fault.arm(u.ID, "empty", "empty")
		warnings := pendingLifecycleWarnings(t, srv, agent.ID, "start")
		starts := disp.snapshot()
		require.Len(t, starts, 1)
		assert.Equal(t, "old-model", starts[0].model)
		assert.Equal(t, map[string][]string{pendingDiscardTriggerSaverUnauthorized: {"config.model"}}, effectDiscards(t, s, agent.ID, 1))
		assert.Contains(t, warnings, effectDropWarning(pendingDiscardTriggerSaverUnauthorized, "config.model"))
	})

	t.Run("deny then indeterminate: 503, nothing dropped, row byte-identical", func(t *testing.T) {
		srv, s, disp, agent, u, fault := setup(t)
		pendingBefore, appliedBefore := gateARowSnapshot(t, s, agent.ID)
		fault.arm(u.ID, "empty", "error")
		rec := gateAStart(t, srv, agent.ID)
		require.Equal(t, http.StatusServiceUnavailable, rec.Code, rec.Body.String())
		assert.Empty(t, disp.snapshot())
		pendingAfter, appliedAfter := gateARowSnapshot(t, s, agent.ID)
		assert.Equal(t, pendingBefore, pendingAfter)
		assert.Equal(t, appliedBefore, appliedAfter)
		effectAssertNoDiscard(t, s, agent.ID)
	})
}

// TestPendingFoldFault_StartRefusedRowUnchanged (F3): a binding-load fault
// on the saver's re-check refuses the start with a 503 before dispatch:
// nothing is dropped, written or audited.
func TestPendingFoldFault_StartRefusedRowUnchanged(t *testing.T) {
	srv, s, disp, agent := heldEditsFixture(t, state.PhaseRunning)
	fault := gateAInstallAuthzFault(t, srv)
	u := effectUser(t, s, agent.ProjectID, "fault")
	effectPatchAs(t, srv, u, agent.ID, map[string]interface{}{"config": map[string]interface{}{"model": "held-model"}})
	pendingLifecycleWarnings(t, srv, agent.ID, "stop")
	pendingBefore, appliedBefore := gateARowSnapshot(t, s, agent.ID)

	fault.arm(u.ID, "error", "error")
	rec := gateAStart(t, srv, agent.ID)
	require.Equal(t, http.StatusServiceUnavailable, rec.Code, rec.Body.String())
	assert.Contains(t, rec.Body.String(), pendingReauthUnavailableMessage)
	assert.Empty(t, disp.snapshot(), "no dispatch")
	pendingAfter, appliedAfter := gateARowSnapshot(t, s, agent.ID)
	assert.Equal(t, pendingBefore, pendingAfter)
	assert.Equal(t, appliedBefore, appliedAfter)
	effectAssertNoDiscard(t, s, agent.ID)
	effectAssertNoAppliedAudit(t, s, agent.ID)
}

// TestPendingFoldFault_PatchRefusedNothingWritten (F3): the same fault at
// the fold of a PATCH to an agent with no container answers 503 and writes
// nothing, the request's own key included.
func TestPendingFoldFault_PatchRefusedNothingWritten(t *testing.T) {
	srv, s, _, agent := heldEditsFixture(t, state.PhaseRunning)
	fault := gateAInstallAuthzFault(t, srv)
	u := effectUser(t, s, agent.ProjectID, "fault")
	effectPatchAs(t, srv, u, agent.ID, map[string]interface{}{"config": map[string]interface{}{"model": "held-model"}})
	pendingLifecycleWarnings(t, srv, agent.ID, "stop")
	pendingBefore, versionBefore := effectStoredPending(t, s, agent.ID)

	fault.arm(u.ID, "error", "error")
	rec := doRequest(t, srv, http.MethodPatch, "/api/v1/agents/"+agent.ID, map[string]interface{}{
		"config": map[string]interface{}{"max_turns": 9},
	})
	require.Equal(t, http.StatusServiceUnavailable, rec.Code, rec.Body.String())
	assert.Contains(t, rec.Body.String(), pendingReauthUnavailableMessage)
	pendingAfter, versionAfter := effectStoredPending(t, s, agent.ID)
	assert.Equal(t, pendingBefore, pendingAfter)
	assert.Equal(t, versionBefore, versionAfter, "nothing was written")
	assert.Equal(t, api.IntPtr(3), pendingReloadAgent(t, s, agent.ID).AppliedConfig.InlineConfig.MaxTurns, "the request's own key is not written")
	effectAssertNoDiscard(t, s, agent.ID)
}

// TestPendingFoldFault_EnvViewIndeterminate (F3): a fault on the env
// saver's env-view check (taken before any key is re-checked) refuses the
// start: a partial env edit is never folded on a fault.
func TestPendingFoldFault_EnvViewIndeterminate(t *testing.T) {
	srv, s, disp, agent := heldEditsFixture(t, state.PhaseRunning)
	fault := gateAInstallAuthzFault(t, srv)
	u := effectUser(t, s, agent.ProjectID, "envfault")
	resp := effectPatchAs(t, srv, u, agent.ID, map[string]interface{}{"config": map[string]interface{}{"env": map[string]string{"NEW": "1"}}})
	require.Equal(t, []string{"config.env"}, resp.Disposition.Held)
	pendingLifecycleWarnings(t, srv, agent.ID, "stop")
	pendingBefore, appliedBefore := gateARowSnapshot(t, s, agent.ID)

	fault.arm(u.ID, "error", "error")
	rec := gateAStart(t, srv, agent.ID)
	require.Equal(t, http.StatusServiceUnavailable, rec.Code, rec.Body.String())
	assert.Empty(t, disp.snapshot(), "no dispatch")
	pendingAfter, appliedAfter := gateARowSnapshot(t, s, agent.ID)
	assert.Equal(t, pendingBefore, pendingAfter, "env is not folded")
	assert.Equal(t, appliedBefore, appliedAfter)
	effectAssertNoDiscard(t, s, agent.ID)
}

// TestPendingSaverDrop_TriggersAreDistinct: four drops on one start (a user
// denied, a user suspended, an agent saver, a delegated saver) give four
// audits with four distinct triggers and four warnings; the start
// succeeds.
func TestPendingSaverDrop_TriggersAreDistinct(t *testing.T) {
	srv, s, disp, agent := heldEditsFixture(t, state.PhaseStopped)
	denied := effectUser(t, s, agent.ProjectID, "denied")
	effectRevokeGrants(t, s, denied.ID)
	suspended := effectUser(t, s, agent.ProjectID, "suspended")
	effectSetUserStatus(t, s, suspended.ID, store.UserStatusSuspended)
	effectSeedPending(t, s, agent.ID, map[string]string{
		"model":           `"denied-model"`,
		"max_turns":       `7`,
		"max_duration":    `"3h"`,
		"max_model_calls": `5`,
	}, map[string]string{
		"model":           "user:" + denied.ID,
		"max_turns":       "user:" + suspended.ID,
		"max_duration":    "agent:" + tid("saver-agent"),
		"max_model_calls": "agent_delegated:" + tid("saver-agent"),
	}, "user:"+denied.ID)

	warnings := pendingLifecycleWarnings(t, srv, agent.ID, "start")
	starts := disp.snapshot()
	require.Len(t, starts, 1)
	assert.Equal(t, "old-model", starts[0].model)
	assert.Equal(t, api.IntPtr(3), starts[0].inline.MaxTurns)
	assert.Empty(t, starts[0].inline.MaxDuration)
	assert.Nil(t, pendingReloadAgent(t, s, agent.ID).PendingConfig)
	assert.Equal(t, map[string][]string{
		pendingDiscardTriggerSaverUnauthorized:    {"config.model"},
		pendingDiscardTriggerSaverAccountInactive: {"config.max_turns"},
		pendingDiscardTriggerSaverUnresolvable:    {"config.max_duration"},
		pendingDiscardTriggerSaverDelegated:       {"config.max_model_calls"},
	}, effectDiscards(t, s, agent.ID, 4))
	for trigger, key := range map[string]string{
		pendingDiscardTriggerSaverUnauthorized:    "config.model",
		pendingDiscardTriggerSaverAccountInactive: "config.max_turns",
		pendingDiscardTriggerSaverUnresolvable:    "config.max_duration",
		pendingDiscardTriggerSaverDelegated:       "config.max_model_calls",
	} {
		assert.Contains(t, warnings, effectDropWarning(trigger, key))
	}
	effectAssertNoAppliedAudit(t, s, agent.ID)
}
