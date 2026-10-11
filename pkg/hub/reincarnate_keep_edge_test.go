// Copyright 2026 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//      http://www.apache.org/licenses/LICENSE-2.0
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

	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ptone/scion#3762: when agent X reincarnates another agent T without
// changing its role, T keeps its existing delegation edge, so T's authority
// does not start depending on X's chain.

// keepEdgeFixture is a project owned by user U, with a full-role agent T and
// a full-role agent X, each delegated by U through a recorded session edge.
type keepEdgeFixture struct {
	srv     *Server
	s       store.Store
	project *store.Project
	userID  string
	target  *store.Agent
	other   *store.Agent
	edge    *store.DelegationEdge // T's edge, U -> T
}

func newKeepEdgeFixture(t *testing.T) *keepEdgeFixture {
	t.Helper()
	srv, s, project, broker := setupReincarnateTestServer(t, newReincarnateTestDispatcher())
	userID := tid("keep-edge-user-" + t.Name())
	createDCUser(t, s, userID, tidSlugSafe(t.Name())+"@keep-edge.test", project.ID, store.ProjectRoleOwner)
	byUser := func(a *store.Agent) {
		a.CreatedBy = userID
		a.OwnerID = userID
		a.Ancestry = []string{userID}
		a.AppliedConfig.AgentRole = string(AgentRoleFull)
	}
	target := newReincarnateTestAgent(t, s, project, broker, byUser)
	other := newReincarnateTestAgent(t, s, project, broker, func(a *store.Agent) {
		byUser(a)
		a.ID = tid("keep-edge-other-" + t.Name())
		a.Slug = "keep-edge-other-" + tidSlugSafe(t.Name())
		a.Phase = "stopped"
	})
	edge := seedFullAgentEdge(t, s, store.DelegationPrincipalUser, userID, target)
	seedFullAgentEdge(t, s, store.DelegationPrincipalUser, userID, other)
	return &keepEdgeFixture{srv: srv, s: s, project: project, userID: userID, target: target, other: other, edge: edge}
}

// seedFullAgentEdge records an active, recorded, principal-ceiling project
// edge from delegator to agent with the full role, and returns it.
func seedFullAgentEdge(t *testing.T, s store.Store, delegatorType, delegatorID string, agent *store.Agent) *store.DelegationEdge {
	t.Helper()
	kind := store.SourceCredentialSession
	if delegatorType == store.DelegationPrincipalAgent {
		kind = store.SourceCredentialAgent
	}
	e := &store.DelegationEdge{
		DelegatorType: delegatorType,
		DelegatorID:   delegatorID,
		DelegateType:  store.DelegationPrincipalAgent,
		DelegateID:    agent.ID,
		ScopeType:     store.RoleScopeProject,
		ScopeID:       agent.ProjectID,
		Role:          string(AgentRoleFull),
		Active:        true,
		AuthorityProvenance: store.AuthorityProvenance{
			ProvenanceVersion:    store.ProvenanceVersionV1,
			SourcePrincipalKind:  delegatorType,
			SourcePrincipalID:    delegatorID,
			SourceCredentialKind: kind,
		},
		EffectCeiling: store.EffectCeiling{Kind: store.EffectCeilingPrincipal},
	}
	require.NoError(t, s.CreateDelegationEdge(context.Background(), e))
	return e
}

// fullRequesterFor is an agent identity that may reincarnate a full-role
// agent: the lifecycle scope plus every scope of the full role.
func fullRequesterFor(requesterID, projectID string) AgentIdentity {
	return agentIdentityFor(requesterID, projectID, append(ScopesForRole(AgentRoleFull), ScopeAgentLifecycle)...)
}

// reincarnate runs a real reincarnation of T by X with body and requires it
// to be accepted and settled.
func (f *keepEdgeFixture) reincarnate(t *testing.T, body ReincarnateAgentRequest) {
	t.Helper()
	rec := httptest.NewRecorder()
	f.srv.handleReincarnateAgent(rec, reincarnateRequest(t, f.target.ID, fullRequesterFor(f.other.ID, f.project.ID), body), f.target.ID)
	require.Equal(t, http.StatusAccepted, rec.Code, rec.Body.String())
	waitForReincarnationSettled(t, f.s, f.target.ID)
}

// targetEdge returns T's single active edge.
func (f *keepEdgeFixture) targetEdge(t *testing.T) *store.DelegationEdge {
	t.Helper()
	edges, err := f.s.GetDelegationEdgesForDelegate(context.Background(), store.DelegationPrincipalAgent, f.target.ID)
	require.NoError(t, err)
	var active []*store.DelegationEdge
	for _, e := range edges {
		if e.Active {
			active = append(active, e)
		}
	}
	require.Len(t, active, 1)
	return active[0]
}

// assertTargetCanCreateAgents mints T's token, then asserts the mint and a
// refresh issue the agent-create scope and that Decide allows agent.create
// in the project for T's identity.
func (f *keepEdgeFixture) assertTargetCanCreateAgents(t *testing.T) {
	t.Helper()
	ctx := context.Background()
	target := mustGetAgent(t, f.s, f.target.ID)

	token, err := f.srv.issueAgentTokenForTest(ctx, target)
	require.NoError(t, err, "T's token mint")
	claims, err := f.srv.agentTokenService.ValidateAgentToken(token)
	require.NoError(t, err)
	assert.Contains(t, claims.Scopes, ScopeAgentCreate, "the mint issues the agent-create scope")

	rec := httptest.NewRecorder()
	f.srv.handleAgentTokenRefresh(rec, buildAgentRefreshRequest(target.ID, claims, "", false), target.ID)
	refreshed := refreshedToken(t, rec)
	refreshedClaims, err := f.srv.agentTokenService.ValidateAgentToken(refreshed)
	require.NoError(t, err)
	assert.Contains(t, refreshedClaims.Scopes, ScopeAgentCreate, "the refresh issues the agent-create scope")

	identity := &agentIdentityWrapper{AgentTokenClaims: claims}
	actx := contextWithIdentity(ctx, identity)
	// CheckAccess builds the request and runs Decide.
	decision := f.srv.authzService.CheckAccess(actx, identity,
		Resource{Type: "agent", ParentType: "project", ParentID: f.project.ID}, ActionCreate)
	assert.True(t, decision.Allowed, "T may create agents: reason %q", decision.Reason)
}

// Regression: X reincarnates T without changing its role. T keeps its edge
// U -> T (no reincarnate_replaced deactivation, self-reincarnate audit
// shape), and T may still create agents.
func TestReincarnateByOtherAgentKeepsEdge(t *testing.T) {
	f := newKeepEdgeFixture(t)
	f.reincarnate(t, ReincarnateAgentRequest{Handoff: "h"})

	got := f.targetEdge(t)
	assert.Equal(t, store.DelegationPrincipalUser, got.DelegatorType)
	assert.Equal(t, f.userID, got.DelegatorID, "T -> U, not T -> X")
	assertEdgeKept(t, f.s, f.target.ID, f.edge, got)
	f.assertTargetCanCreateAgents(t)
}

// The same, then X is deleted: T's authority never depended on X, so T may
// still create agents, and its token mint and refresh still issue scopes.
func TestReincarnateByOtherAgentThenRequesterDeleted(t *testing.T) {
	f := newKeepEdgeFixture(t)
	f.reincarnate(t, ReincarnateAgentRequest{Handoff: "h"})

	rec := doRequest(t, f.srv, http.MethodDelete, "/api/v1/agents/"+f.other.ID, nil)
	require.Equal(t, http.StatusNoContent, rec.Code, rec.Body.String())
	_, err := f.s.GetAgent(context.Background(), f.other.ID)
	require.ErrorIs(t, err, store.ErrNotFound, "X is deleted")

	assert.Equal(t, f.edge.ID, f.targetEdge(t).ID, "T keeps U -> T")
	f.assertTargetCanCreateAgents(t)
}

// A requester that descends from T may not change T's role: it would become
// T's delegator and close a loop in the delegation chain. Refused with 403,
// on a dry run and a real run, with nothing written. Descent is found
// through the ancestry alone (X created by T, its edge since re-pointed to
// U: refused conservatively) and through the delegation chain alone (X's
// edge re-pointed to T by an earlier role change).
func TestReincarnateRoleChangeByDescendantRefused(t *testing.T) {
	for name, link := range map[string]func(t *testing.T, f *keepEdgeFixture){
		"ancestry": func(t *testing.T, f *keepEdgeFixture) {
			// Ancestry is immutable after creation, so X is created with T
			// in it. Its edge points at U: only the ancestry links X to T.
			f.other = newReincarnateTestAgent(t, f.s, f.project, &store.RuntimeBroker{ID: f.target.RuntimeBrokerID}, func(a *store.Agent) {
				a.ID = tid("keep-edge-child-" + t.Name())
				a.Slug = "keep-edge-child-" + tidSlugSafe(t.Name())
				a.CreatedBy = f.target.ID
				a.OwnerID = f.userID
				a.Ancestry = []string{f.userID, f.target.ID}
				a.AppliedConfig.AgentRole = string(AgentRoleFull)
				a.Phase = "stopped"
			})
			seedFullAgentEdge(t, f.s, store.DelegationPrincipalUser, f.userID, f.other)
		},
		"chain": func(t *testing.T, f *keepEdgeFixture) {
			ctx := context.Background()
			_, err := f.s.DeactivateDelegationEdgesForDelegate(ctx, store.DelegationPrincipalAgent, f.other.ID,
				store.Deactivation{Cause: store.EdgeDeactivationReincarnateReplaced, OpID: "fixture"})
			require.NoError(t, err)
			seedFullAgentEdge(t, f.s, store.DelegationPrincipalAgent, f.target.ID, f.other)
		},
	} {
		t.Run(name, func(t *testing.T) {
			f := newKeepEdgeFixture(t)
			link(t, f)
			target := mustGetAgent(t, f.s, f.target.ID)
			for _, dryRun := range []bool{true, false} {
				rec := httptest.NewRecorder()
				body := ReincarnateAgentRequest{Handoff: "h", DryRun: dryRun, Role: string(AgentRoleBaseline)}
				f.srv.handleReincarnateAgent(rec, reincarnateRequest(t, f.target.ID, fullRequesterFor(f.other.ID, f.project.ID), body), f.target.ID)
				require.Equal(t, http.StatusForbidden, rec.Code, "dryRun=%v: %s", dryRun, rec.Body.String())
				assert.Contains(t, rec.Body.String(), "closes a loop in the delegation chain", "dryRun=%v", dryRun)
			}
			assertNothingClaimed(t, f.s, target, f.edge)
			assert.Equal(t, string(AgentRoleFull), mustGetAgent(t, f.s, f.target.ID).AppliedConfig.AgentRole, "the role is unchanged")
		})
	}
}

// A requester that does not descend from T may still change T's role, and
// then becomes T's recorded delegator (the cycle check does not refuse it).
func TestReincarnateRoleChangeByNonDescendantReRecords(t *testing.T) {
	f := newKeepEdgeFixture(t)
	f.reincarnate(t, ReincarnateAgentRequest{Handoff: "h", Role: string(AgentRoleBaseline)})

	got := f.targetEdge(t)
	assert.NotEqual(t, f.edge.ID, got.ID)
	assert.Equal(t, store.DelegationPrincipalAgent, got.DelegatorType)
	assert.Equal(t, f.other.ID, got.DelegatorID, "T -> X after a role change")
	assert.Equal(t, string(AgentRoleBaseline), got.Role)
	assertReincarnateReplacedEdge(t, f.s, f.target.ID, f.edge.ID)
}

// --role naming the role the agent already has is not a role change: the
// edge is kept.
func TestReincarnateSameRoleKeepsEdge(t *testing.T) {
	f := newKeepEdgeFixture(t)
	f.reincarnate(t, ReincarnateAgentRequest{Handoff: "h", Role: string(AgentRoleFull)})
	assertEdgeKept(t, f.s, f.target.ID, f.edge, f.targetEdge(t))
}

// requesterGetAgentErrStore fails GetAgent for one agent ID.
type requesterGetAgentErrStore struct {
	store.Store
	failID string
}

func (s *requesterGetAgentErrStore) GetAgent(ctx context.Context, id string) (*store.Agent, error) {
	if id == s.failID {
		return nil, errors.New("injected requester agent lookup fault")
	}
	return s.Store.GetAgent(ctx, id)
}

// A lookup fault in the cycle check answers 503 on a dry run and a real run,
// with nothing written. The fault is on the requester's row in the server
// store: an edge read fault on the chain surfaces earlier, in the
// requester's own authority checks, which read the same edges.
func TestReincarnateCycleCheckLookupFault503(t *testing.T) {
	f := newKeepEdgeFixture(t)
	f.srv.store = &requesterGetAgentErrStore{Store: f.srv.store, failID: f.other.ID}
	target := mustGetAgent(t, f.s, f.target.ID)
	for _, dryRun := range []bool{true, false} {
		rec := httptest.NewRecorder()
		body := ReincarnateAgentRequest{Handoff: "h", DryRun: dryRun, Role: string(AgentRoleBaseline)}
		f.srv.handleReincarnateAgent(rec, reincarnateRequest(t, f.target.ID, fullRequesterFor(f.other.ID, f.project.ID), body), f.target.ID)
		require.Equal(t, http.StatusServiceUnavailable, rec.Code, "dryRun=%v: %s", dryRun, rec.Body.String())
		assert.Contains(t, rec.Body.String(), "delegation chain", "dryRun=%v: the cycle check's 503", dryRun)
	}
	assertNothingClaimed(t, f.s, target, f.edge)
}

// A self --role naming the stored role is not a role change: it is accepted
// and keeps the edge even when the agent's token scopes would fail
// CanDelegate for that role, as a plain self-reincarnate is. The role
// lattice and the lifecycle check still run on it, and a self --role that
// does change the role still runs CanDelegate.
func TestReincarnateSelfSameRole(t *testing.T) {
	selfReincarnate := func(t *testing.T, f *keepEdgeFixture, identity AgentIdentity, role AgentRole) *httptest.ResponseRecorder {
		t.Helper()
		rec := httptest.NewRecorder()
		f.srv.handleReincarnateAgent(rec, reincarnateRequest(t, f.target.ID, identity, ReincarnateAgentRequest{Handoff: "h", Role: string(role)}), f.target.ID)
		return rec
	}
	lifecycleOnly := func(f *keepEdgeFixture) AgentIdentity {
		return agentIdentityFor(f.target.ID, f.project.ID, ScopeAgentLifecycle)
	}

	t.Run("accepted without CanDelegate, edge kept", func(t *testing.T) {
		f := newKeepEdgeFixture(t)
		rec := selfReincarnate(t, f, lifecycleOnly(f), AgentRoleFull)
		require.Equal(t, http.StatusAccepted, rec.Code, rec.Body.String())
		waitForReincarnationSettled(t, f.s, f.target.ID)
		assertEdgeKept(t, f.s, f.target.ID, f.edge, f.targetEdge(t))
		assert.Equal(t, string(AgentRoleFull), mustGetAgent(t, f.s, f.target.ID).AppliedConfig.AgentRole)
	})

	t.Run("a real role change still runs CanDelegate", func(t *testing.T) {
		f := newKeepEdgeFixture(t)
		target := mustGetAgent(t, f.s, f.target.ID)
		rec := selfReincarnate(t, f, lifecycleOnly(f), AgentRoleBaseline)
		require.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
		assert.Contains(t, rec.Body.String(), "Cannot delegate agent authority")
		assertNothingClaimed(t, f.s, target, f.edge)
	})

	t.Run("the role lattice still runs", func(t *testing.T) {
		f := newKeepEdgeFixture(t)
		f.project.Annotations = map[string]string{projectSettingMaxAgentRole: string(AgentRoleBaseline)}
		require.NoError(t, f.s.UpdateProject(context.Background(), f.project))
		target := mustGetAgent(t, f.s, f.target.ID)
		rec := selfReincarnate(t, f, lifecycleOnly(f), AgentRoleFull)
		require.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
		assert.Contains(t, rec.Body.String(), "project maximum")
		assertNothingClaimed(t, f.s, target, f.edge)
	})

	t.Run("the lifecycle check still runs", func(t *testing.T) {
		f := newKeepEdgeFixture(t)
		target := mustGetAgent(t, f.s, f.target.ID)
		noLifecycle := agentIdentityFor(f.target.ID, f.project.ID) // no scopes
		rec := selfReincarnate(t, f, noLifecycle, AgentRoleFull)
		require.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
		assertNothingClaimed(t, f.s, target, f.edge)
	})
}
