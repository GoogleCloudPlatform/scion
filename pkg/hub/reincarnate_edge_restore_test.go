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
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

func activeEdgesForAgent(t *testing.T, s store.Store, agentID string) []*store.DelegationEdge {
	t.Helper()
	edges, err := s.GetDelegationEdgesForDelegate(context.Background(), store.DelegationPrincipalAgent, agentID)
	require.NoError(t, err)
	return edges
}

func assertEdgeRestoredToPrior(t *testing.T, s store.Store, agentID, recID string, priorEdge *store.DelegationEdge, cause store.EdgeDeactivationCause) {
	t.Helper()
	ctx := context.Background()
	edges := activeEdgesForAgent(t, s, agentID)
	require.Len(t, edges, 1, "exactly one active delegation edge")
	assert.Equal(t, priorEdge.ID, edges[0].ID, "prior delegation edge is restored")
	assert.Equal(t, priorEdge.Role, edges[0].Role, "delegation edge matches reverted config")
	assert.Equal(t, priorEdge.DelegatorType, edges[0].DelegatorType)
	assert.Equal(t, priorEdge.DelegatorID, edges[0].DelegatorID)
	assert.Equal(t, priorEdge.AuthorityProvenance, edges[0].AuthorityProvenance)
	assert.Equal(t, priorEdge.EffectCeiling, edges[0].EffectCeiling)

	deactivated, err := s.GetDeactivatedDelegationEdgesForDelegate(ctx, store.DelegationPrincipalAgent, agentID, cause, recID)
	require.NoError(t, err)
	require.Len(t, deactivated, 1, "replacement edge deactivated with failure cause")
	assert.Equal(t, recID, deactivated[0].ID)

	sum := auditSummary(t, s, mutationTypeAgentReincarnateEdgeRestore, agentID)
	assert.Equal(t, recID, sum["reincarnation_id"])
	assert.Equal(t, string(cause), sum["cause"])
	assert.EqualValues(t, 1, sum["deactivated"])
	assert.EqualValues(t, 1, sum["reactivated"])
}

func TestReincarnateRolePatch_PlainFailureRestoresEdge(t *testing.T) {
	newFixture := func(t *testing.T, reprovErr error) (*Server, store.Store, *store.Agent, *store.DelegationEdge, *store.Agent) {
		t.Helper()
		disp := newReincarnateTestDispatcher()
		disp.reprovisionErr = reprovErr
		srv, s, project, broker := setupReincarnateTestServer(t, disp)
		userID := tid("edge-restore-user-" + t.Name())
		createDCUser(t, s, userID, tidSlugSafe(t.Name())+"@edge-restore.test", project.ID, store.ProjectRoleOwner)
		target := newReincarnateTestAgent(t, s, project, broker, func(a *store.Agent) {
			a.CreatedBy = userID
			a.OwnerID = userID
			a.Ancestry = []string{userID}
			a.AppliedConfig.AgentRole = string(AgentRoleFull)
		})
		other := newReincarnateTestAgent(t, s, project, broker, func(a *store.Agent) {
			a.ID = tid("edge-restore-other-" + t.Name())
			a.Slug = "edge-restore-other-" + tidSlugSafe(t.Name())
			a.CreatedBy = userID
			a.OwnerID = userID
			a.Ancestry = []string{userID}
			a.AppliedConfig.AgentRole = string(AgentRoleFull)
			a.Phase = "stopped"
		})
		priorEdge := seedFullAgentEdge(t, s, store.DelegationPrincipalUser, userID, target)
		seedFullAgentEdge(t, s, store.DelegationPrincipalUser, userID, other)
		return srv, s, target, priorEdge, other
	}

	t.Run("failure restores prior edge", func(t *testing.T) {
		srv, s, target, priorEdge, other := newFixture(t, errors.New("injected reprovision failure"))
		rec := httptest.NewRecorder()
		req := reincarnateRequest(t, target.ID, fullRequesterFor(other.ID, target.ProjectID), ReincarnateAgentRequest{
			Handoff: "h",
			Role:    string(AgentRoleBaseline),
		})
		srv.handleReincarnateAgent(rec, req, target.ID)
		require.Equal(t, http.StatusAccepted, rec.Code, rec.Body.String())

		r := waitForReincarnationSettled(t, s, target.ID)
		require.Equal(t, store.AgentReincarnationStateFailed, r.State)

		gotAgent := mustGetAgent(t, s, target.ID)
		assert.Equal(t, string(AgentRoleFull), gotAgent.AppliedConfig.AgentRole, "config reverted to prior role")
		assertEdgeRestoredToPrior(t, s, target.ID, r.ID, priorEdge, store.EdgeDeactivationReincarnateFailed)
	})

	t.Run("success keeps patched edge", func(t *testing.T) {
		srv, s, target, priorEdge, other := newFixture(t, nil)
		rec := httptest.NewRecorder()
		req := reincarnateRequest(t, target.ID, fullRequesterFor(other.ID, target.ProjectID), ReincarnateAgentRequest{
			Handoff: "h",
			Role:    string(AgentRoleBaseline),
		})
		srv.handleReincarnateAgent(rec, req, target.ID)
		require.Equal(t, http.StatusAccepted, rec.Code, rec.Body.String())

		r := waitForReincarnationSettled(t, s, target.ID)
		require.Equal(t, store.AgentReincarnationStateCompleted, r.State, r.Error)

		gotAgent := mustGetAgent(t, s, target.ID)
		assert.Equal(t, string(AgentRoleBaseline), gotAgent.AppliedConfig.AgentRole, "config has patched role")

		edges := activeEdgesForAgent(t, s, target.ID)
		require.Len(t, edges, 1)
		assert.NotEqual(t, priorEdge.ID, edges[0].ID)
		assert.Equal(t, r.ID, edges[0].ID)
		assert.Equal(t, string(AgentRoleBaseline), edges[0].Role, "delegation edge matches patched role")
		assert.Equal(t, store.DelegationPrincipalAgent, edges[0].DelegatorType)
		assert.Equal(t, other.ID, edges[0].DelegatorID)
		assert.Empty(t, agentAudits(t, s, mutationTypeAgentReincarnateEdgeRestore, target.ID))
	})
}

func TestReincarnateRolePatch_MoveFailureRestoresEdge(t *testing.T) {
	newMoveRoleFixture := func(t *testing.T, provErr error) (*moveFixture, *store.DelegationEdge, *store.Agent) {
		t.Helper()
		f := setupMoveFixture(t, true, nil)
		prepareMoveWorkerFixture(t, f)
		f.disp.moveProvisionErr = provErr

		userID := tid("move-edge-user-" + t.Name())
		createDCUser(t, f.s, userID, tidSlugSafe(t.Name())+"@move-edge.test", f.project.ID, store.ProjectRoleOwner)

		ctx := context.Background()
		f.agent = mustGetAgent(t, f.s, f.agent.ID)
		f.agent.CreatedBy = userID
		f.agent.OwnerID = userID
		f.agent.Ancestry = []string{userID}
		f.agent.AppliedConfig.AgentRole = string(AgentRoleFull)
		require.NoError(t, f.s.UpdateAgent(ctx, f.agent))

		other := newReincarnateTestAgent(t, f.s, f.project, f.src, func(a *store.Agent) {
			a.ID = tid("move-edge-other-" + t.Name())
			a.Slug = "move-edge-other-" + tidSlugSafe(t.Name())
			a.CreatedBy = userID
			a.OwnerID = userID
			a.Ancestry = []string{userID}
			a.AppliedConfig.AgentRole = string(AgentRoleFull)
			a.Phase = "stopped"
		})
		priorEdge := seedFullAgentEdge(t, f.s, store.DelegationPrincipalUser, userID, f.agent)
		seedFullAgentEdge(t, f.s, store.DelegationPrincipalUser, userID, other)
		return f, priorEdge, other
	}

	t.Run("failure restores prior edge", func(t *testing.T) {
		f, priorEdge, other := newMoveRoleFixture(t, errors.New("injected target provision failure"))
		rec := httptest.NewRecorder()
		req := reincarnateRequest(t, f.agent.ID, fullRequesterFor(other.ID, f.project.ID), ReincarnateAgentRequest{
			Handoff:      "moving",
			TargetBroker: f.dst.ID,
			Role:         string(AgentRoleBaseline),
		})
		f.srv.handleReincarnateAgent(rec, req, f.agent.ID)
		require.Equal(t, http.StatusAccepted, rec.Code, rec.Body.String())

		r := waitForMoveSettled(t, f.s, f.agent.ID)
		require.Equal(t, store.AgentReincarnationStateFailed, r.State)

		gotAgent := mustGetAgent(t, f.s, f.agent.ID)
		assert.Equal(t, f.src.ID, gotAgent.RuntimeBrokerID, "agent rolled back to source broker")
		assert.Equal(t, string(AgentRoleFull), gotAgent.AppliedConfig.AgentRole, "config reverted to prior role")
		assertEdgeRestoredToPrior(t, f.s, f.agent.ID, r.ID, priorEdge, store.EdgeDeactivationReincarnateFailed)
	})

	t.Run("success keeps patched edge", func(t *testing.T) {
		f, priorEdge, other := newMoveRoleFixture(t, nil)
		rec := httptest.NewRecorder()
		req := reincarnateRequest(t, f.agent.ID, fullRequesterFor(other.ID, f.project.ID), ReincarnateAgentRequest{
			Handoff:      "moving",
			TargetBroker: f.dst.ID,
			Role:         string(AgentRoleBaseline),
		})
		f.srv.handleReincarnateAgent(rec, req, f.agent.ID)
		require.Equal(t, http.StatusAccepted, rec.Code, rec.Body.String())

		r := waitForMoveSettled(t, f.s, f.agent.ID)
		require.Equal(t, store.AgentReincarnationStateCompleted, r.State, r.Error)

		gotAgent := mustGetAgent(t, f.s, f.agent.ID)
		assert.Equal(t, f.dst.ID, gotAgent.RuntimeBrokerID, "agent moved to target broker")
		assert.Equal(t, string(AgentRoleBaseline), gotAgent.AppliedConfig.AgentRole, "config has patched role")

		edges := activeEdgesForAgent(t, f.s, f.agent.ID)
		require.Len(t, edges, 1)
		assert.NotEqual(t, priorEdge.ID, edges[0].ID)
		assert.Equal(t, r.ID, edges[0].ID)
		assert.Equal(t, string(AgentRoleBaseline), edges[0].Role, "delegation edge matches patched role")
		assert.Equal(t, store.DelegationPrincipalAgent, edges[0].DelegatorType)
		assert.Equal(t, other.ID, edges[0].DelegatorID)
		assert.Empty(t, agentAudits(t, f.s, mutationTypeAgentReincarnateEdgeRestore, f.agent.ID))
	})
}

type claimedEdgeFixture struct {
	srv       *Server
	s         store.Store
	agent     *store.Agent
	priorEdge *store.DelegationEdge
	rec       *store.AgentReincarnation
	userID    string
	otherID   string
}

func newClaimedEdgeFixture(t *testing.T, withPriorEdge, roleChanged bool) *claimedEdgeFixture {
	t.Helper()
	srv, s, project, broker := setupReincarnateTestServer(t, newReincarnateTestDispatcher())
	userID := tid("claimed-edge-user-" + t.Name())
	createDCUser(t, s, userID, tidSlugSafe(t.Name())+"@claimed-edge.test", project.ID, store.ProjectRoleOwner)
	target := newReincarnateTestAgent(t, s, project, broker, func(a *store.Agent) {
		a.CreatedBy = userID
		a.OwnerID = userID
		a.Ancestry = []string{userID}
		a.AppliedConfig.AgentRole = string(AgentRoleFull)
	})
	var priorEdge *store.DelegationEdge
	if withPriorEdge {
		priorEdge = seedFullAgentEdge(t, s, store.DelegationPrincipalUser, userID, target)
	}
	otherID := tid("claimed-edge-other-" + t.Name())
	now := time.Now()
	target.ReincarnationUpdatedAt = &now
	rec := &store.AgentReincarnation{
		AgentID:               target.ID,
		FromGeneration:        target.Generation,
		ToGeneration:          target.Generation + 1,
		State:                 store.AgentReincarnationStatePending,
		PreviousAppliedConfig: target.AppliedConfig,
	}
	newRole := string(AgentRoleFull)
	if roleChanged {
		newRole = string(AgentRoleBaseline)
	}
	auth := &reincarnateAuthority{
		DelegatorType: store.DelegationPrincipalUser,
		DelegatorID:   otherID,
		Role:          newRole,
		Ceiling:       store.EffectCeiling{Kind: store.EffectCeilingPrincipal},
		Provenance:    recordedProv(store.DelegationPrincipalUser, otherID, store.SourceCredentialSession),
		RoleChanged:   roleChanged,
	}
	require.NoError(t, srv.reincarnateClaimTx(context.Background(), target, rec, auth, nil, AuditActor{}))
	return &claimedEdgeFixture{
		srv: srv, s: s, agent: target, priorEdge: priorEdge, rec: rec, userID: userID, otherID: otherID,
	}
}

func (f *claimedEdgeFixture) advance(t *testing.T, states ...string) {
	t.Helper()
	from := store.AgentReincarnationStatePending
	for _, to := range states {
		_, ok, err := f.srv.tryAdvanceReincarnation(context.Background(), f.rec.ID, from, to, reincarnationStepMaxAttempts, nil)
		require.NoError(t, err)
		require.True(t, ok, "advance %s -> %s", from, to)
		from = to
	}
}

func TestReincarnateRolePatch_FailureWithoutRestoreKeepsEdge(t *testing.T) {
	f := newClaimedEdgeFixture(t, true, true)
	f.advance(t, store.AgentReincarnationStateStopping, store.AgentReincarnationStateProvisioning, store.AgentReincarnationStateStarting)

	fresh := *f.rec.PreviousAppliedConfig
	fresh.AgentRole = string(AgentRoleBaseline)
	_, err := f.srv.updateReincarnationStep(context.Background(), f.agent.ID, reincarnationStepUpdate{
		reincarnationState: store.ReincarnationStateStarting,
		appliedConfig:      &fresh,
	}, reincarnationStepMaxAttempts)
	require.NoError(t, err)

	f.srv.failReincarnation(context.Background(), f.agent.ID, f.rec.ID, store.AgentReincarnationStateStarting, "start failed", nil)

	got := mustGetAgent(t, f.s, f.agent.ID)
	assert.Equal(t, string(AgentRoleBaseline), got.AppliedConfig.AgentRole)
	edges := activeEdgesForAgent(t, f.s, f.agent.ID)
	require.Len(t, edges, 1)
	assert.Equal(t, f.rec.ID, edges[0].ID, "patched edge stays active when config is not restored")
	assert.Equal(t, string(AgentRoleBaseline), edges[0].Role)
	assert.Empty(t, agentAudits(t, f.s, mutationTypeAgentReincarnateEdgeRestore, f.agent.ID))
}

func TestReincarnateRolePatch_FailureWithoutPriorEdgeLeavesNone(t *testing.T) {
	f := newClaimedEdgeFixture(t, false, true)
	f.srv.failReincarnation(context.Background(), f.agent.ID, f.rec.ID, store.AgentReincarnationStatePending,
		"no dispatcher available", f.rec.PreviousAppliedConfig)

	assert.Equal(t, store.ReincarnationStateFailed, mustGetAgent(t, f.s, f.agent.ID).ReincarnationState)
	assert.Empty(t, activeEdgesForAgent(t, f.s, f.agent.ID), "no active edge when none existed before claim")

	deactivated, err := f.s.GetDeactivatedDelegationEdgesForDelegate(context.Background(), store.DelegationPrincipalAgent, f.agent.ID,
		store.EdgeDeactivationReincarnateFailed, f.rec.ID)
	require.NoError(t, err)
	require.Len(t, deactivated, 1)
	assert.Equal(t, f.rec.ID, deactivated[0].ID)

	sum := auditSummary(t, f.s, mutationTypeAgentReincarnateEdgeRestore, f.agent.ID)
	assert.EqualValues(t, 1, sum["deactivated"])
	assert.EqualValues(t, 0, sum["reactivated"])
}

func TestReincarnateRolePatch_SweepRestoresEdge(t *testing.T) {
	t.Run("pending swept restores edge", func(t *testing.T) {
		f := newClaimedEdgeFixture(t, true, true)
		n, err := f.srv.sweepStaleReincarnationsOlderThan(context.Background(), time.Now().Add(time.Hour))
		require.NoError(t, err)
		require.Equal(t, 1, n)
		assertEdgeRestoredToPrior(t, f.s, f.agent.ID, f.rec.ID, f.priorEdge, store.EdgeDeactivationReincarnatePendingSwept)
	})

	t.Run("provisioning swept restores edge", func(t *testing.T) {
		f := newClaimedEdgeFixture(t, true, true)
		f.advance(t, store.AgentReincarnationStateStopping, store.AgentReincarnationStateProvisioning)
		n, err := f.srv.sweepStaleReincarnationsOlderThan(context.Background(), time.Now().Add(time.Hour))
		require.NoError(t, err)
		require.Equal(t, 1, n)
		assertEdgeRestoredToPrior(t, f.s, f.agent.ID, f.rec.ID, f.priorEdge, store.EdgeDeactivationReincarnateStaleSwept)
	})

	t.Run("starting swept keeps patched edge", func(t *testing.T) {
		f := newClaimedEdgeFixture(t, true, true)
		f.advance(t, store.AgentReincarnationStateStopping, store.AgentReincarnationStateProvisioning, store.AgentReincarnationStateStarting)
		n, err := f.srv.sweepStaleReincarnationsOlderThan(context.Background(), time.Now().Add(time.Hour))
		require.NoError(t, err)
		require.Equal(t, 1, n)
		edges := activeEdgesForAgent(t, f.s, f.agent.ID)
		require.Len(t, edges, 1)
		assert.Equal(t, f.rec.ID, edges[0].ID)
		assert.Empty(t, agentAudits(t, f.s, mutationTypeAgentReincarnateEdgeRestore, f.agent.ID))
	})
}

func TestReincarnateRolePatch_SoftDeleteRemarksEdges(t *testing.T) {
	ctx := context.Background()
	f := newClaimedEdgeFixture(t, true, true)
	f.srv.SetDispatcher(&engineStubDispatcher{})
	f.srv.config.SoftDeleteRetention = time.Hour

	softDeleteForTest(t, f.srv, f.agent.ID)
	deleted := mustGetAgent(t, f.s, f.agent.ID)
	require.False(t, deleted.DeletedAt.IsZero())
	require.NotEmpty(t, deleted.SoftDeleteOpID)
	assert.Empty(t, activeEdgesForAgent(t, f.s, f.agent.ID))

	// Sweep fails the pending reincarnation, re-marking the edges for the soft-deleted agent.
	n, err := f.srv.sweepStaleReincarnationsOlderThan(ctx, time.Now().Add(time.Hour))
	require.NoError(t, err)
	require.Equal(t, 1, n)

	assert.Empty(t, activeEdgesForAgent(t, f.s, f.agent.ID), "soft-deleted agent stays without active edges")
	deactivatedFailed, err := f.s.GetDeactivatedDelegationEdgesForDelegate(ctx, store.DelegationPrincipalAgent, f.agent.ID,
		store.EdgeDeactivationReincarnatePendingSwept, f.rec.ID)
	require.NoError(t, err)
	require.Len(t, deactivatedFailed, 1)
	assert.Equal(t, f.rec.ID, deactivatedFailed[0].ID, "replacement edge re-marked as reincarnate_pending_swept")

	deactivatedSoft, err := f.s.GetDeactivatedDelegationEdgesForDelegate(ctx, store.DelegationPrincipalAgent, f.agent.ID,
		store.EdgeDeactivationAgentSoftDelete, deleted.SoftDeleteOpID)
	require.NoError(t, err)
	require.Len(t, deactivatedSoft, 1)
	assert.Equal(t, f.priorEdge.ID, deactivatedSoft[0].ID, "prior edge re-marked as agent_soft_delete for restore")

	sum := auditSummary(t, f.s, mutationTypeAgentReincarnateEdgeRestore, f.agent.ID)
	assert.EqualValues(t, 2, sum["remarked"])
	assert.EqualValues(t, 0, sum["reactivated"])

	// Restoring the soft-deleted agent reactivates the original edge.
	rec := restoreForTest(t, f.srv, f.agent.ID)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	edges := activeEdgesForAgent(t, f.s, f.agent.ID)
	require.Len(t, edges, 1, "exactly one active edge after restore")
	assert.Equal(t, f.priorEdge.ID, edges[0].ID)
	assert.Equal(t, f.priorEdge.Role, edges[0].Role)
}

func TestReincarnateSameRoleClaimEdgeIsNotRestored(t *testing.T) {
	f := newClaimedEdgeFixture(t, true, false)
	edgesBefore := activeEdgesForAgent(t, f.s, f.agent.ID)
	require.Len(t, edgesBefore, 1)
	assert.NotEqual(t, f.rec.ID, edgesBefore[0].ID, "same-role re-record is not marked restorable")
	assert.NotContains(t, auditSummary(t, f.s, mutationTypeAgentReincarnateClaim, f.agent.ID), "edge_restorable")

	f.srv.failReincarnation(context.Background(), f.agent.ID, f.rec.ID, store.AgentReincarnationStatePending,
		"reprovision failed", f.rec.PreviousAppliedConfig)

	edgesAfter := activeEdgesForAgent(t, f.s, f.agent.ID)
	require.Len(t, edgesAfter, 1)
	assert.Equal(t, edgesBefore[0].ID, edgesAfter[0].ID, "same-role re-recorded edge stays active after failure")
	assert.Empty(t, agentAudits(t, f.s, mutationTypeAgentReincarnateEdgeRestore, f.agent.ID))
}
