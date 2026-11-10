//go:build !no_sqlite

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
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const forceMoveTestRemote = "https://example.com/force-move-repo.git"

// setupForceMoveFixture is setupMoveFixture for a clone-per-agent project
// with a git remote, whose agent's workspace is local to the source broker
// (so the export cannot carry it), and a target that supports forced moves.
func setupForceMoveFixture(t *testing.T, dstIsProvider bool, mutate func(dst *store.RuntimeBroker)) *moveFixture {
	t.Helper()
	f := setupMoveFixture(t, dstIsProvider, func(dst *store.RuntimeBroker) {
		dst.Capabilities = &store.BrokerCapabilities{Reprovision: true, AgentMove: true, AgentMoveRecreate: true}
		if mutate != nil {
			mutate(dst)
		}
	})
	ctx := context.Background()
	p, err := f.s.GetProject(ctx, f.project.ID)
	require.NoError(t, err)
	p.GitRemote = forceMoveTestRemote
	require.NoError(t, f.s.UpdateProject(ctx, p))
	f.project = p
	require.NoError(t, f.s.SetAgentWorkspacePlacement(ctx, f.agent.ID, api.WorkspacePlacementLocal))
	a, err := f.s.GetAgent(ctx, f.agent.ID)
	require.NoError(t, err)
	f.agent = a
	return f
}

// forceWouldApplyDetail decodes error.details.forceWouldApply.
func forceWouldApplyDetail(t *testing.T, rec *httptest.ResponseRecorder) bool {
	t.Helper()
	var body struct {
		Error struct {
			Details map[string]interface{} `json:"details"`
		} `json:"error"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body), rec.Body.String())
	v, _ := body.Error.Details["forceWouldApply"].(bool)
	return v
}

// A forced dry run of a local-workspace agent is a 200 recreate
// verdict, and writes nothing: no provider link, no quota, no record, no
// agent row change, no dispatch.
func TestReincarnateMove_ForceDryRun_RecreateVerdictNoSideEffects(t *testing.T) {
	f := setupForceMoveFixture(t, false, func(dst *store.RuntimeBroker) { dst.AutoProvide = true })
	ctx := context.Background()
	reserveBrokerSlot(t, f.s, f.src, f.agent.ID)
	count := f.agentCount(t)

	// The dev user may link brokers, so the access check passes without the
	// link existing.
	rec := doRequest(t, f.srv, http.MethodPost, "/api/v1/agents/"+f.agent.ID+"/reincarnate",
		ReincarnateAgentRequest{DryRun: true, TargetBroker: f.dst.ID, ForceMove: true})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var resp ReincarnateAgentResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	require.NotNil(t, resp.MoveVerdict)
	v := resp.MoveVerdict
	assert.True(t, v.Eligible)
	assert.Equal(t, MoveTransferModeRecreate, v.TransferMode)
	got := checkResults(*v)
	for _, name := range []string{moveCheckWorkspaceStorage, moveCheckSameExport, moveCheckWorkspaceOnExport} {
		assert.Equal(t, MoveCheckWaived, got[name], name)
	}
	assert.Equal(t, MoveCheckPassed, got[moveCheckWorkspaceRecreatable])
	keys := consequenceKeys(*v)
	for _, item := range []string{"identity", "relationships", "config", "handoff", "workspace", "home", "harness-session", "credentials", "local-work", "process-state"} {
		assert.Contains(t, keys, item)
	}
	assert.Equal(t, "lost", keys["local-work"])

	_, err := f.s.GetProjectProvider(ctx, f.project.ID, f.dst.ID)
	assert.ErrorIs(t, err, store.ErrNotFound, "a forced dry run must not link the target")
	assert.EqualValues(t, 1, brokerReservationCount(t, f.s, f.src.ID), "the reservation stays on the source")
	assert.EqualValues(t, 0, brokerReservationCount(t, f.s, f.dst.ID))
	f.assertNoMoveSideEffects(t, count)
}

// End to end: --force on an export-eligible agent is an export move.
func TestReincarnateMove_ForceDryRun_ExportEligibleStaysExport(t *testing.T) {
	f := setupMoveFixture(t, true, nil)
	count := f.agentCount(t)
	rec := f.reincarnate(t, ReincarnateAgentRequest{DryRun: true, TargetBroker: f.dst.ID, ForceMove: true})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var resp ReincarnateAgentResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	require.NotNil(t, resp.MoveVerdict)
	assert.Equal(t, MoveTransferModeExport, resp.MoveVerdict.TransferMode)
	for _, c := range resp.MoveVerdict.Checks {
		assert.Equal(t, MoveCheckPassed, c.Result, c.Name)
	}
	f.assertNoMoveSideEffects(t, count)
}

// Without --force the refusal is unchanged, and the details say
// --force would apply.
func TestReincarnateMove_WithoutForce_RefusalCarriesForceWouldApply(t *testing.T) {
	f := setupForceMoveFixture(t, true, nil)
	count := f.agentCount(t)
	for _, dryRun := range []bool{true, false} {
		rec := f.reincarnate(t, ReincarnateAgentRequest{DryRun: dryRun, Handoff: "h", TargetBroker: f.dst.ID})
		require.Equal(t, http.StatusConflict, rec.Code, rec.Body.String())
		code, msg, v := decodeMoveRefusal(t, rec)
		assert.Equal(t, ErrCodeConflict, code)
		assert.Contains(t, msg, "not on the shared NFS export")
		assertVerdictFailedAt(t, v, moveCheckWorkspaceOnExport)
		assert.Empty(t, v.TransferMode)
		assert.True(t, forceWouldApplyDetail(t, rec))
	}
	f.assertNoMoveSideEffects(t, count)
}

// A forced move onto a target without the capability is a 412 at the
// capability check.
func TestReincarnateMove_ForceDryRun_TargetWithoutCapability(t *testing.T) {
	f := setupForceMoveFixture(t, true, func(dst *store.RuntimeBroker) {
		dst.Capabilities.AgentMoveRecreate = false
	})
	rec := f.reincarnate(t, ReincarnateAgentRequest{DryRun: true, TargetBroker: f.dst.ID, ForceMove: true})
	require.Equal(t, http.StatusPreconditionFailed, rec.Code, rec.Body.String())
	_, msg, v := decodeMoveRefusal(t, rec)
	assert.Contains(t, msg, "does not support forced moves")
	assert.Equal(t, MoveCheckFailed, checkResults(v)[moveCheckCapability])
	assert.False(t, forceWouldApplyDetail(t, rec))
}

// forceMove without a target broker is a 400 before anything else.
func TestReincarnateMove_ForceWithoutTarget_Returns400(t *testing.T) {
	f := setupForceMoveFixture(t, true, nil)
	count := f.agentCount(t)
	rec := f.reincarnate(t, ReincarnateAgentRequest{DryRun: true, ForceMove: true})
	require.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
	assert.Contains(t, rec.Body.String(), "forceMove needs targetBroker")
	f.assertNoMoveSideEffects(t, count)
}

// forceMove with a target that resolves to the agent's current
// broker is a 400 before any plan, on a dry run too.
func TestReincarnateMove_ForceToCurrentBroker_Returns400(t *testing.T) {
	f := setupForceMoveFixture(t, true, nil)
	count := f.agentCount(t)
	for _, dryRun := range []bool{true, false} {
		for _, target := range []string{f.src.ID, f.src.Slug} {
			rec := f.reincarnate(t, ReincarnateAgentRequest{DryRun: dryRun, Handoff: "h", TargetBroker: target, ForceMove: true})
			require.Equal(t, http.StatusBadRequest, rec.Code, "dryRun=%v target=%s: %s", dryRun, target, rec.Body.String())
			assert.Contains(t, rec.Body.String(), "--force applies only to a move to another broker")
			assert.NotContains(t, rec.Body.String(), `"plan"`)
		}
	}
	f.assertNoMoveSideEffects(t, count)
}

// An empty-per-agent agent whose workspace the export cannot carry passes
// the handler's empty-per-agent gate under --force, and is recreated.
// Without --force it keeps the workspace_mode refusal.
func TestReincarnateMove_ForceEmptyPerAgent(t *testing.T) {
	f := setupMoveFixture(t, true, func(dst *store.RuntimeBroker) {
		dst.Capabilities = &store.BrokerCapabilities{Reprovision: true, AgentMove: true, AgentMoveRecreate: true}
	})
	ctx := context.Background()
	p, err := f.s.GetProject(ctx, f.project.ID)
	require.NoError(t, err)
	p.GitRemote = ""
	if p.Labels == nil {
		p.Labels = map[string]string{}
	}
	p.Labels[store.LabelWorkspaceMode] = string(store.SharingModeEmptyPerAgent)
	require.NoError(t, f.s.UpdateProject(ctx, p))
	require.True(t, p.IsEmptyPerAgent())
	a, err := f.s.GetAgent(ctx, f.agent.ID)
	require.NoError(t, err)
	a.AppliedConfig.GitClone = nil
	a.AppliedConfig.Workspace = ""
	require.NoError(t, f.s.UpdateAgent(ctx, a))
	require.NoError(t, f.s.SetAgentWorkspacePlacement(ctx, a.ID, api.WorkspacePlacementLocal))
	a, err = f.s.GetAgent(ctx, a.ID)
	require.NoError(t, err)
	f.agent = a
	count := f.agentCount(t)

	rec := f.reincarnate(t, ReincarnateAgentRequest{DryRun: true, TargetBroker: f.dst.ID})
	require.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
	_, _, v := decodeMoveRefusal(t, rec)
	assertVerdictFailedAt(t, v, moveCheckWorkspaceMode)

	rec = f.reincarnate(t, ReincarnateAgentRequest{DryRun: true, TargetBroker: f.dst.ID, ForceMove: true})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var resp ReincarnateAgentResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	require.NotNil(t, resp.MoveVerdict)
	assert.Equal(t, MoveTransferModeRecreate, resp.MoveVerdict.TransferMode)
	assert.Equal(t, MoveCheckPassed, checkResults(*resp.MoveVerdict)[moveCheckWorkspaceRecreatable])
	f.assertNoMoveSideEffects(t, count)
}
