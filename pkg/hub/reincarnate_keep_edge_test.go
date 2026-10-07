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
// through the ancestry (X created by T) and through the delegation chain
// alone (X's edge re-pointed to T by an earlier role change).
func TestReincarnateRoleChangeByDescendantRefused(t *testing.T) {
	for name, link := range map[string]func(t *testing.T, f *keepEdgeFixture){
		"ancestry": func(t *testing.T, f *keepEdgeFixture) {
			ctx := context.Background()
			x := mustGetAgent(t, f.s, f.other.ID)
			x.Ancestry = []string{f.userID, f.target.ID}
			require.NoError(t, f.s.UpdateAgent(ctx, x))
			_, err := f.s.DeactivateDelegationEdgesForDelegate(ctx, store.DelegationPrincipalAgent, x.ID,
				store.Deactivation{Cause: store.EdgeDeactivationReincarnateReplaced, OpID: "fixture"})
			require.NoError(t, err)
			seedFullAgentEdge(t, f.s, store.DelegationPrincipalAgent, f.target.ID, x)
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
