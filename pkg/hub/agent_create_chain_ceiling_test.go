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
	"runtime"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/hub/permissions"
	"github.com/GoogleCloudPlatform/scion/pkg/secret"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// chainFixture is the access-token create world with a minting dispatcher,
// for agents that create agents with their own production token.
type chainFixture struct {
	*uatCreateFixture
	client *mintBrokerClient
}

func newChainFixture(t *testing.T, name string) *chainFixture {
	t.Helper()
	f := newUATCreateFixture(t, name)
	return &chainFixture{uatCreateFixture: f, client: f.withDispatcher(t)}
}

// agentToken mints the production token for the stored agent.
func (f *chainFixture) agentToken(t *testing.T, agentID string) string {
	t.Helper()
	a, err := f.store.GetAgent(context.Background(), agentID)
	require.NoError(t, err)
	tok, err := f.srv.GenerateAgentTokenForAgent(context.Background(), a)
	require.NoError(t, err)
	return tok
}

// createAsParent posts a create in the fixture project with token.
func (f *chainFixture) createAsParent(t *testing.T, token string, body interface{}) *httptest.ResponseRecorder {
	t.Helper()
	return doRequestWithAgentToken(t, f.srv, http.MethodPost, f.path, body, token)
}

// sessionParent creates an agent with the creator's session.
func (f *chainFixture) sessionParent(t *testing.T, slug string) (*store.Agent, *store.DelegationEdge) {
	t.Helper()
	return f.createdAgent(t, f.create(t, authUser(f.creator), CreateAgentRequest{Name: slug}), slug)
}

// childOf creates slug with parent's production token and returns it.
func (f *chainFixture) childOf(t *testing.T, parent *store.Agent, slug string) (*store.Agent, *store.DelegationEdge) {
	t.Helper()
	rec := f.createAsParent(t, f.agentToken(t, parent.ID), CreateAgentRequest{Name: slug})
	return f.createdAgent(t, rec, slug)
}

// mint returns a mintFixture view of the chain fixture, for refresh.
func (f *chainFixture) mint() *mintFixture {
	return &mintFixture{srv: f.srv, store: f.store, client: f.client, projectID: f.proj.ID}
}

// rowFiveIDs is the non-delivery part of the ceiling an agent-sourced edge
// created by parent carries: the coverage of parent's ceiling-filtered mint
// scopes, bounded by parent's own edge ceiling when that edge is bounded.
func rowFiveIDs(t *testing.T, srv *Server, parent *store.Agent) []string {
	t.Helper()
	ctx := context.Background()
	a := srv.authzService
	scopes, err := a.ceilingFilteredAgentScopes(ctx, parent, a.mintCandidateScopes(parent))
	require.NoError(t, err)
	coverage := agentScopeCoverage(scopes)
	edges := activeEdgesFor(t, srv.store, parent.ID)
	if len(edges) == 1 && edges[0].Kind == store.EffectCeilingBounded {
		frozen, ok := edges[0].Frozen()
		require.True(t, ok)
		var bounded []string
		for _, id := range coverage {
			if frozen.Allows(id) {
				bounded = append(bounded, id)
			}
		}
		coverage = bounded
	}
	return sortedUniqueIDs(coverage)
}

// withoutDeliver returns ids less the hub delivery permissions, sorted.
func withoutDeliver(ids []string) []string {
	var out []string
	for _, id := range ids {
		if !hubDeliveryPermissionSet[id] {
			out = append(out, id)
		}
	}
	return sortedUniqueIDs(out)
}

// deliverOf returns the hub delivery permissions in ids, sorted.
func deliverOf(ids []string) []string {
	var out []string
	for _, id := range ids {
		if hubDeliveryPermissionSet[id] {
			out = append(out, id)
		}
	}
	return sortedUniqueIDs(out)
}

// agentDelegatorEdges returns every edge delegated by agent agentID.
func agentDelegatorEdges(t *testing.T, s store.Store, agentID string) []*store.DelegationEdge {
	t.Helper()
	edges, err := s.GetDelegationEdgesForDelegator(context.Background(), store.DelegationPrincipalAgent, agentID)
	require.NoError(t, err)
	return edges
}

// createWrites counts the rows an agent create writes after its
// transaction: agent audit records and the project's notification
// subscriptions.
type createWrites struct {
	audits        int
	subscriptions int
}

// countCreateWrites returns the createWrites present in s for projectID.
func countCreateWrites(t *testing.T, s store.Store, projectID string) createWrites {
	t.Helper()
	ctx := context.Background()
	_, audits, err := s.ListMutationAudits(ctx, store.MutationAuditFilter{TargetType: "agent"})
	require.NoError(t, err)
	subs, err := s.GetNotificationSubscriptionsByProject(ctx, projectID)
	require.NoError(t, err)
	return createWrites{audits: audits, subscriptions: len(subs)}
}

// assertAgentCreateWroteNothing asserts no agent row for slug, no edge
// delegated by agent parentID beyond the wantEdges already present, and no
// agent audit record or subscription beyond those counted in before.
func assertAgentCreateWroteNothing(t *testing.T, s store.Store, projectID, slug, parentID string, wantEdges int, before createWrites) {
	t.Helper()
	_, err := s.GetAgentBySlug(context.Background(), projectID, slug)
	assert.ErrorIs(t, err, store.ErrNotFound, "no agent row")
	assert.Len(t, agentDelegatorEdges(t, s, parentID), wantEdges, "no new delegation edge")
	after := countCreateWrites(t, s, projectID)
	assert.Equal(t, before.audits, after.audits, "no agent audit record")
	assert.Equal(t, before.subscriptions, after.subscriptions, "no subscription")
}

// A principal parent (session-created) creating a child: the child's edge
// is bounded V1 over the parent's row-five IDs plus every delivery
// permission.
func TestAgentCreateDeliverIDs_PrincipalParent(t *testing.T) {
	f := newChainFixture(t, "chain-deliver")
	parent, pEdge := f.sessionParent(t, "chain-deliver-p")
	require.Equal(t, store.EffectCeilingPrincipal, pEdge.Kind)

	_, cEdge := f.childOf(t, parent, "chain-deliver-c")
	assert.Equal(t, store.EffectCeilingBounded, cEdge.Kind)
	assert.Equal(t, permissions.CeilingVersionV1, cEdge.Version)
	assert.Equal(t, store.DelegationPrincipalAgent, cEdge.DelegatorType)
	assert.Equal(t, parent.ID, cEdge.DelegatorID)
	assertEdgeDelegatorIsSourcePrincipal(t, cEdge)
	assert.Equal(t, store.SourceCredentialAgent, cEdge.SourceCredentialKind)
	assert.Equal(t, string(permissions.BoundaryKindProject), cEdge.BoundaryKind)
	assert.Equal(t, f.proj.ID, cEdge.BoundaryProjectID)
	assert.Equal(t, sortedUniqueIDs(hubDeliveryPermissionList), deliverOf(cEdge.PermissionIDs), "every delivery permission")
	assert.Equal(t, rowFiveIDs(t, f.srv, parent), withoutDeliver(cEdge.PermissionIDs))
}

// Every non-delivery permission is in the child's ceiling exactly when the
// row-five computation puts it there; and a lookup fault at the parent's
// delivery-eligibility read fails the create with nothing written.
func TestAgentCreateDeliverIDs_NonDeliverControl(t *testing.T) {
	f := newChainFixture(t, "chain-control")
	parent, _ := f.sessionParent(t, "chain-control-p")
	_, cEdge := f.childOf(t, parent, "chain-control-c")

	inC := map[string]bool{}
	for _, id := range cEdge.PermissionIDs {
		inC[id] = true
	}
	inRow5 := map[string]bool{}
	for _, id := range rowFiveIDs(t, f.srv, parent) {
		inRow5[id] = true
	}
	for _, p := range permissions.Registry {
		assert.Equal(t, inRow5[p.ID] || hubDeliveryPermissionSet[p.ID], inC[p.ID], p.ID)
	}

	t.Run("lookup fault at delivery eligibility", func(t *testing.T) {
		token := f.agentToken(t, parent.ID)
		fs := &materialFailingStore{Store: f.store}
		var labels []string
		fs.getDelegationEdgesForDelegateHook = func(int) { labels = append(labels, edgeReadCaller()) }
		f.srv.store = fs
		f.srv.authzService.store = fs
		t.Cleanup(func() {
			f.srv.store = fs.Store
			f.srv.authzService.store = fs.Store
		})

		// Dry run: label each edge read of a successful create.
		rec := f.createAsParent(t, token, CreateAgentRequest{Name: "fault-dry"})
		require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
		t.Logf("edge reads: %v", labels)
		n := -1
		for i, l := range labels {
			if l == "parentDeliverEligibility" {
				n = i
				break
			}
		}
		require.GreaterOrEqual(t, n, 0, "parentDeliverEligibility makes its own edge read: %v", labels)
		assert.Equal(t, expectedReadsBeforeDeliverEligibility, labels[:n])
		assert.Contains(t, labels[:n], "agentSourceEffectCeiling", "the parent edge read precedes delivery eligibility")

		// Fault run: reads 1..n pass, read n+1 fails.
		edgesBefore := len(agentDelegatorEdges(t, fs.Store, parent.ID))
		labels = nil
		fs.getDelegationEdgesForDelegateCalls = 0
		fs.getDelegationEdgesForDelegateResults = nil
		fs.createAgentCalls = 0
		fs.getDelegationEdgesForDelegateErr = errors.New("edge store unavailable")
		fs.getDelegationEdgesForDelegateErrAfterCalls = n

		writesBefore := countCreateWrites(t, fs.Store, f.proj.ID)
		rec = f.createAsParent(t, token, CreateAgentRequest{Name: "fault-c"})
		assert.Contains(t, []int{http.StatusInternalServerError, http.StatusServiceUnavailable}, rec.Code, rec.Body.String())
		require.Greater(t, len(fs.getDelegationEdgesForDelegateResults), n)
		for i, err := range fs.getDelegationEdgesForDelegateResults[:n] {
			assert.NoError(t, err, "read %d (%s)", i+1, labels[i])
		}
		assert.Error(t, fs.getDelegationEdgesForDelegateResults[n])
		assert.Equal(t, "parentDeliverEligibility", labels[n])
		assert.Zero(t, fs.createAgentCalls, "no agent row written")
		assertAgentCreateWroteNothing(t, fs.Store, f.proj.ID, "fault-c", parent.ID, edgesBefore, writesBefore)
	})
}

// expectedReadsBeforeDeliverEligibility is the edge-read order of an agent
// create up to the parent's delivery-eligibility read: the create
// authorization walk, then the parent's active edge and its coverage.
var expectedReadsBeforeDeliverEligibility = []string{
	"walkDelegationChainWithCause",
	"agentSourceEffectCeiling",
	"ceilingFilteredAgentScopes",
}

// edgeReadCaller names the innermost marker function on the stack of an
// edge read.
func edgeReadCaller() string {
	markers := []string{
		"parentDeliverEligibility",
		"ceilingFilteredAgentScopes",
		"agentSourceEffectCeiling",
		"walkDelegationChainWithCause",
		"GenerateAgentTokenForAgent",
	}
	pcs := make([]uintptr, 64)
	frames := runtime.CallersFrames(pcs[:runtime.Callers(2, pcs)])
	for {
		fr, more := frames.Next()
		for _, m := range markers {
			if strings.HasSuffix(fr.Function, "."+m) {
				return m
			}
		}
		if !more {
			return "other"
		}
	}
}

// The delivery permissions on a child's edge change no token scope: the
// create mint and the refresh both equal the candidates filtered by the
// edge ceiling less the delivery permissions.
func TestDeliverIDsDoNotChangeMintScopes(t *testing.T) {
	f := newChainFixture(t, "chain-mint")
	parent, _ := f.sessionParent(t, "chain-mint-p")
	child, cEdge := f.childOf(t, parent, "chain-mint-c")
	require.NotEmpty(t, deliverOf(cEdge.PermissionIDs))

	projected := boundedCeiling(withoutDeliver(cEdge.PermissionIDs)...)
	want := filterScopes(f.srv.authzService.mintCandidateScopes(child), projected, ScopeCeilings{})
	mf := f.mint()

	assert.ElementsMatch(t, want, mf.tokenClaims(t, f.client.lastCreateReq.AgentToken).Scopes, "create mint")
	tok := refreshedToken(t, mf.refresh(t, child, child.Ancestry))
	assert.ElementsMatch(t, want, mf.tokenClaims(t, tok).Scopes, "refresh")
}

// A parent with no active edge creates a child before the edge backfill
// completes: the child's dispatch mint fails at the parent's hop, the
// create is 403 ceiling_orphaned with no agent row and an audit record, and
// the edge written for the child carries the parent's coverage only. After
// the backfill completes the create is 403 with nothing written.
func TestAgentCreateDeliverIDs_MissingParentEdge(t *testing.T) {
	t.Run("backfill not complete", func(t *testing.T) {
		f := newChainFixture(t, "chain-missing")
		parent := createFixtureAgent(t, f.bypassAgentsFixture, "chain-missing-p", []string{f.creator.ID}, AgentRoleFull)
		require.Empty(t, activeEdgesFor(t, f.store, parent.ID))

		rec := f.createAsParent(t, f.agentToken(t, parent.ID), CreateAgentRequest{Name: "chain-missing-c"})
		assert.Equal(t, agentTokenDenialMessage(DenyCauseCeilingOrphaned), assertCeilingDenial(t, rec))
		_, err := f.store.GetAgentBySlug(context.Background(), f.proj.ID, "chain-missing-c")
		assert.ErrorIs(t, err, store.ErrNotFound, "no agent row")

		assert.Empty(t, agentDelegatorEdges(t, f.store, parent.ID), "the child's edge is deactivated")
		failed, _, err := f.store.ListMutationAudits(context.Background(),
			store.MutationAuditFilter{TargetType: "agent", MutationType: mutationTypeAgentCreateDispatchFailed})
		require.NoError(t, err)
		require.Len(t, failed, 1, "the failed create is compensated")
		sum := assertCompensated(t, f.store, failed[0].TargetID)

		// Reactivate the compensated edge to inspect the ceiling it carried.
		_, err = f.store.ReactivateDelegationEdgesForDelegate(context.Background(), store.DelegationPrincipalAgent,
			failed[0].TargetID, store.EdgeDeactivationCreateCompensation, sum.OpID)
		require.NoError(t, err)
		edges := agentDelegatorEdges(t, f.store, parent.ID)
		require.Len(t, edges, 1)
		left := edges[0]
		assert.Equal(t, store.EffectCeilingBounded, left.Kind)
		assert.Empty(t, deliverOf(left.PermissionIDs), "no delivery permission")
		assert.Equal(t, rowFiveIDs(t, f.srv, parent), sortedUniqueIDs(left.PermissionIDs), "coverage only")
		assertIssueDeniedAudit(t, f.store, left.DelegateID, mintSiteCreate, string(DenyCauseCeilingOrphaned))
	})

	t.Run("backfill complete", func(t *testing.T) {
		f := newChainFixture(t, "chain-missing-bf")
		parent := createFixtureAgent(t, f.bypassAgentsFixture, "chain-missing-bf-p", []string{f.creator.ID}, AgentRoleFull)
		token := f.agentToken(t, parent.ID)
		markEdgeBackfillComplete(t, f.store)

		writesBefore := countCreateWrites(t, f.store, f.proj.ID)
		rec := f.createAsParent(t, token, CreateAgentRequest{Name: "chain-missing-bf-c"})
		assertCeilingDenial(t, rec)
		var cause DenyCause
		allowed, _, err := f.srv.authzService.walkDelegationChainWithCause(context.Background(),
			Resource{Type: "agent", ParentType: "project", ParentID: f.proj.ID}, ActionCreate, "agent.create",
			parent.ID, true, store.RoleScopeProject, f.proj.ID, nil, &cause)
		require.NoError(t, err)
		assert.False(t, allowed, "the create authorization denies first: no edge after the backfill")
		assert.Empty(t, cause, "the walk's missing-edge deny records no cause")

		claims, err := f.srv.agentTokenService.ValidateAgentToken(token)
		require.NoError(t, err)
		_, _, err = f.srv.authzService.sourceEffectCeiling(context.Background(), &agentIdentityWrapper{AgentTokenClaims: claims})
		require.ErrorIs(t, err, ErrProvenanceMissing)
		srcCause, structural := ceilingDenyCauseForError(err)
		assert.True(t, structural)
		assert.Equal(t, DenyCauseCeilingOrphaned, srcCause, "the source ceiling")
		assertAgentCreateWroteNothing(t, f.store, f.proj.ID, "chain-missing-bf-c", parent.ID, 0, writesBefore)
		assert.False(t, f.client.createCalled, "no broker request")
	})
}

// A parent whose chain carries an unrecorded hop gives its child no
// delivery permission, directly or one hop further down.
func TestAgentCreateDeliverIDs_UnrecordedOrUnknownParent(t *testing.T) {
	t.Run("unrecorded parent edge", func(t *testing.T) {
		f := newChainFixture(t, "chain-unrec")
		parent := createFixtureAgent(t, f.bypassAgentsFixture, "chain-unrec-p", []string{f.creator.ID}, AgentRoleFull)
		addProjectEdge(t, f.store, store.DelegationPrincipalUser, f.creator.ID, parent.ID, f.proj.ID)

		_, cEdge := f.childOf(t, parent, "chain-unrec-c")
		assert.Equal(t, store.EffectCeilingBounded, cEdge.Kind)
		assert.Empty(t, deliverOf(cEdge.PermissionIDs))
		assert.Equal(t, rowFiveIDs(t, f.srv, parent), sortedUniqueIDs(cEdge.PermissionIDs), "the parent's coverage")
	})

	t.Run("unrecorded grandparent edge", func(t *testing.T) {
		f := newChainFixture(t, "chain-unrec-g")
		grand := createFixtureAgent(t, f.bypassAgentsFixture, "chain-unrec-g", []string{f.creator.ID}, AgentRoleFull)
		addProjectEdge(t, f.store, store.DelegationPrincipalUser, f.creator.ID, grand.ID, f.proj.ID)

		parent, pEdge := f.childOf(t, grand, "chain-unrec-gp")
		assert.Equal(t, store.EffectCeilingBounded, pEdge.Kind)
		assert.Empty(t, deliverOf(pEdge.PermissionIDs))
		assert.Equal(t, rowFiveIDs(t, f.srv, grand), sortedUniqueIDs(pEdge.PermissionIDs))

		_, cEdge := f.childOf(t, parent, "chain-unrec-gc")
		assert.Empty(t, deliverOf(cEdge.PermissionIDs))
		assert.Equal(t, rowFiveIDs(t, f.srv, parent), sortedUniqueIDs(cEdge.PermissionIDs))
	})
}

// A three-hop chain from a session user: each agent-created edge carries
// every delivery permission, and every hop's ceiling allows each.
func TestAgentCreateDeliverIDs_MultiHop(t *testing.T) {
	f := newChainFixture(t, "chain-multi")
	p, pEdge := f.sessionParent(t, "chain-multi-p")
	c, cEdge := f.childOf(t, p, "chain-multi-c")
	d, dEdge := f.childOf(t, c, "chain-multi-d")

	all := sortedUniqueIDs(hubDeliveryPermissionList)
	assert.Equal(t, all, deliverOf(cEdge.PermissionIDs), "C")
	assert.Equal(t, all, deliverOf(dEdge.PermissionIDs), "D")
	for _, edge := range []*store.DelegationEdge{pEdge, cEdge, dEdge} {
		for _, permID := range hubDeliveryPermissionList {
			p, ok := registryPermission(permID)
			require.True(t, ok)
			res := Resource{Type: p.Resource, ParentType: "project", ParentID: f.proj.ID}
			cause, why := hopEffectCeilingDeny(edge, permID, res, d.ID, false)
			assert.Empty(t, cause, "%s on the hop to %s: %s", permID, edge.DelegateID, why)
		}
	}
	_ = c

	t.Run("dev-local parent with dev authority off", func(t *testing.T) {
		mf := newMintFixture(t, "chain-multi-dev")
		parent := devCreatedChild(t, mf, "chain-multi-dev-p")
		token, err := mf.srv.GenerateAgentTokenForAgent(context.Background(), parent)
		require.NoError(t, err)
		mf.srv.authzService.setDevLocalAuthorityEnabled(false)

		writesBefore := countCreateWrites(t, mf.store, mf.projectID)
		rec := doRequestWithAgentToken(t, mf.srv, http.MethodPost, "/api/v1/projects/"+mf.projectID+"/agents",
			CreateAgentRequest{Name: "chain-multi-dev-c"}, token)
		assertCeilingDenial(t, rec)
		assertAgentCreateWroteNothing(t, mf.store, mf.projectID, "chain-multi-dev-c", parent.ID, 0, writesBefore)

		var cause DenyCause
		allowed, _, err := mf.srv.authzService.walkDelegationChainWithCause(context.Background(),
			Resource{Type: "agent", ParentType: "project", ParentID: mf.projectID}, ActionCreate, "agent.create",
			parent.ID, true, store.RoleScopeProject, mf.projectID, nil, &cause)
		require.NoError(t, err)
		assert.False(t, allowed)
		assert.Equal(t, DenyCauseCeilingSourceNotAllowed, cause)

		assert.Equal(t, http.StatusForbidden, mf.refresh(t, parent, parent.Ancestry).Code, "refresh")
	})
}

// A dev-local parent's delivery eligibility is none while dev-local
// authority is off, and every delivery permission while it is on.
func TestParentDeliverEligibility_SourceNotAllowedGivesNone(t *testing.T) {
	mf := newMintFixture(t, "chain-dev-deliver")
	parent := devCreatedChild(t, mf, "chain-dev-deliver-p")
	ctx := context.Background()

	ids, err := mf.srv.authzService.parentDeliverEligibility(ctx, parent)
	require.NoError(t, err)
	assert.Equal(t, sortedUniqueIDs(hubDeliveryPermissionList), sortedUniqueIDs(ids), "dev-local authority on")

	mf.srv.authzService.setDevLocalAuthorityEnabled(false)
	ids, err = mf.srv.authzService.parentDeliverEligibility(ctx, parent)
	assert.NoError(t, err)
	assert.Nil(t, ids)
}

// A grandchild's non-delivery ceiling is within its parent's ceiling and
// coverage; a child of an unrecorded-edge parent is bounded by that parent's
// coverage.
func TestGrandchildCeilingSubsetOfChild(t *testing.T) {
	f := newChainFixture(t, "chain-subset")
	p, _ := f.sessionParent(t, "chain-subset-p")
	c, cEdge := f.childOf(t, p, "chain-subset-c")
	_, dEdge := f.childOf(t, c, "chain-subset-d")

	scopes, err := f.srv.authzService.ceilingFilteredAgentScopes(context.Background(), c, f.srv.authzService.mintCandidateScopes(c))
	require.NoError(t, err)
	inChild := map[string]bool{}
	for _, id := range cEdge.PermissionIDs {
		inChild[id] = true
	}
	coverage := map[string]bool{}
	for _, id := range agentScopeCoverage(scopes) {
		coverage[id] = true
	}
	for _, id := range withoutDeliver(dEdge.PermissionIDs) {
		assert.True(t, inChild[id] && coverage[id], id)
	}

	legacy := createFixtureAgent(t, f.bypassAgentsFixture, "chain-subset-l", []string{f.creator.ID}, AgentRoleFull)
	addProjectEdge(t, f.store, store.DelegationPrincipalUser, f.creator.ID, legacy.ID, f.proj.ID)
	_, lcEdge := f.childOf(t, legacy, "chain-subset-lc")
	assert.Equal(t, store.EffectCeilingBounded, lcEdge.Kind)
	assert.Equal(t, rowFiveIDs(t, f.srv, legacy), sortedUniqueIDs(lcEdge.PermissionIDs))
}

// A child created by a dev-local parent is bounded by that parent's
// coverage.
func TestDevAuthGrandchildBoundedByParent(t *testing.T) {
	mf := newMintFixture(t, "chain-dev-gc")
	parent := devCreatedChild(t, mf, "chain-dev-gc-p")
	token, err := mf.srv.GenerateAgentTokenForAgent(context.Background(), parent)
	require.NoError(t, err)

	rec := doRequestWithAgentToken(t, mf.srv, http.MethodPost, "/api/v1/projects/"+mf.projectID+"/agents",
		CreateAgentRequest{Name: "chain-dev-gc-c"}, token)
	require.Contains(t, []int{http.StatusCreated, http.StatusAccepted}, rec.Code, rec.Body.String())
	child, err := mf.store.GetAgentBySlug(context.Background(), mf.projectID, "chain-dev-gc-c")
	require.NoError(t, err)
	edges := activeEdgesFor(t, mf.store, child.ID)
	require.Len(t, edges, 1)
	assert.Equal(t, store.EffectCeilingBounded, edges[0].Kind)
	assert.Equal(t, parent.ID, edges[0].DelegatorID)
	assert.Equal(t, rowFiveIDs(t, mf.srv, parent), withoutDeliver(edges[0].PermissionIDs))
}

// Authority from a relationship is checked at use, not frozen: a child of
// an access token holding agent:attach carries exactly the token's ceiling,
// and the walk allows attach only on agents the owner descends to.
func TestRelationshipAuthorityNotFrozen(t *testing.T) {
	f := newChainFixture(t, "chain-rel")
	ctx := context.Background()
	f.srv.createProjectMembersGroup(ctx, f.proj)
	require.NoError(t, f.srv.createProjectOwnerRoleBinding(ctx, f.proj.ID, f.owner.ID))
	require.False(t, userRoleHolds(t, f.srv.authzService, f.owner.ID, "agent.attach", f.proj.ID),
		"the owner's roles do not hold attach")

	attachP, ok := registryPermission("agent.attach")
	require.True(t, ok)
	selectors := append(minimalSelectors(t), attachP.UATScope)
	c := uatCeilingFromSelectors(t, selectors...)
	uat := NewScopedUserIdentityWithCeiling(authUser(f.owner), f.proj.ID, selectors, "uat-"+f.owner.ID,
		permissions.FrozenPermissionCeiling{Version: permissions.CeilingVersionV1, PermissionIDs: c.PermissionIDs})

	child, edge := f.createdAgent(t, f.create(t, uat, CreateAgentRequest{Name: "chain-rel-c"}), "chain-rel-c")
	assert.Equal(t, sortedUniqueIDs(c.PermissionIDs), sortedUniqueIDs(edge.PermissionIDs), "the token's ceiling, unchanged")

	mine := createFixtureAgent(t, f.bypassAgentsFixture, "chain-rel-x", []string{f.owner.ID}, AgentRoleFull)
	theirs := createFixtureAgent(t, f.bypassAgentsFixture, "chain-rel-y", []string{f.creator.ID}, AgentRoleFull)
	walk := func(target *store.Agent) (bool, DenyCause) {
		var cause DenyCause
		allowed, _, err := f.srv.authzService.walkDelegationChainWithCause(ctx, agentResource(target),
			Action(attachP.Action), attachP.ID, child.ID, true, store.RoleScopeProject, f.proj.ID, nil, &cause)
		require.NoError(t, err)
		return allowed, cause
	}
	allowed, cause := walk(mine)
	assert.True(t, allowed, "attach on an agent the owner descends to: %q", cause)
	allowed, cause = walk(theirs)
	assert.False(t, allowed)
	assert.Equal(t, DenyCauseCeilingDelegatorLacksPermission, cause)
}

// legacyFixture is a chain fixture with an owner role binding and a legacy
// agent L whose only edge is unrecorded.
type legacyFixture struct {
	*chainFixture
	legacy *store.Agent
	sa     *store.GCPServiceAccount
}

func newLegacyFixture(t *testing.T, name string) *legacyFixture {
	t.Helper()
	f := newChainFixture(t, name)
	ctx := context.Background()
	f.srv.createProjectMembersGroup(ctx, f.proj)
	require.NoError(t, f.srv.createProjectOwnerRoleBinding(ctx, f.proj.ID, f.owner.ID))
	legacy := createFixtureAgent(t, f.bypassAgentsFixture, name+"-l", []string{f.owner.ID}, AgentRoleFull)
	addProjectEdge(t, f.store, store.DelegationPrincipalUser, f.owner.ID, legacy.ID, f.proj.ID)
	sa := bypassAgentsCreateSA(t, f.bypassAgentsFixture, f.proj.ID, true)
	return &legacyFixture{chainFixture: f, legacy: legacy, sa: sa}
}

func (f *legacyFixture) assignBody(slug string) map[string]interface{} {
	return map[string]interface{}{
		"name": slug,
		"gcp_identity": map[string]interface{}{
			"metadata_mode": store.GCPMetadataModeAssign, "service_account_id": f.sa.ID,
		},
	}
}

// assertSAGateDenied asserts the SA gate's generic 403.
func assertSAGateDenied(t *testing.T, rec *httptest.ResponseRecorder) {
	t.Helper()
	require.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
	assert.Equal(t, saAssignGenericForbiddenMsg, decodeTargetAPIError(t, rec).Message)
}

// agentIdentityFor returns the request identity for a production token.
func (f *chainFixture) agentIdentityFor(t *testing.T, token string) *agentIdentityWrapper {
	t.Helper()
	claims, err := f.srv.agentTokenService.ValidateAgentToken(token)
	require.NoError(t, err)
	return &agentIdentityWrapper{AgentTokenClaims: claims}
}

// assertGateUnrecorded asserts that the SA gate denies the token's agent at
// surface with the generic SA-assign message, and that the gate's
// CheckAccess denies with ceiling_unrecorded.
func (f *legacyFixture) assertGateUnrecorded(t *testing.T, token, surface string) {
	t.Helper()
	identity := f.agentIdentityFor(t, token)
	ctx := contextWithIdentity(context.Background(), identity)
	assertUnrecordedDeny(t, f.srv.authzService.CheckAccess(ctx, identity, gcpServiceAccountResource(f.sa), ActionAssign))

	rec := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodPatch, "/api/v1/agents/"+identity.ID(), nil).WithContext(ctx)
	require.False(t, f.srv.authorizeSAAssignment(rec, r, f.sa, surface))
	assertSAGateDenied(t, rec)
}

// An agent whose only edge is unrecorded: a create that takes the project
// default service account is denied by the SA gate with nothing written.
func TestLegacyAgentChildWithDefaultSADenied(t *testing.T) {
	f := newLegacyFixture(t, "legacy-dsa")
	f.setProjectAnnotation(t, projectSettingDefaultGCPIdentityMode, store.GCPMetadataModeAssign)
	f.setProjectAnnotation(t, projectSettingDefaultGCPIdentitySAID, f.sa.ID)
	token := f.agentToken(t, f.legacy.ID)
	writesBefore := countCreateWrites(t, f.store, f.proj.ID)
	assertSAGateDenied(t, f.createAsParent(t, token, CreateAgentRequest{Name: "legacy-dsa-c"}))
	assertAgentCreateWroteNothing(t, f.store, f.proj.ID, "legacy-dsa-c", f.legacy.ID, 0, writesBefore)
	f.assertGateUnrecorded(t, token, SurfaceAgentCreate)
}

// The same agent naming a service account explicitly is denied the same
// way.
func TestLegacyAgentChildWithExplicitSADenied(t *testing.T) {
	f := newLegacyFixture(t, "legacy-esa")
	token := f.agentToken(t, f.legacy.ID)
	writesBefore := countCreateWrites(t, f.store, f.proj.ID)
	assertSAGateDenied(t, f.createAsParent(t, token, f.assignBody("legacy-esa-c")))
	assertAgentCreateWroteNothing(t, f.store, f.proj.ID, "legacy-esa-c", f.legacy.ID, 0, writesBefore)
	f.assertGateUnrecorded(t, token, SurfaceAgentCreate)
}

// The same agent cannot give its child a service account by PATCH. Agent
// tokens carry no scope for agent.update, so the PATCH request is refused
// before the SA gate; the SA gate at the PATCH surface is exercised
// directly.
func TestLegacyAgentSAPatchDenied(t *testing.T) {
	f := newLegacyFixture(t, "legacy-patch")
	token := f.agentToken(t, f.legacy.ID)
	child, _ := f.createdAgent(t, f.createAsParent(t, token, CreateAgentRequest{Name: "legacy-patch-c"}), "legacy-patch-c")

	rec := doRequestWithAgentToken(t, f.srv, http.MethodPatch, "/api/v1/agents/"+child.ID,
		map[string]interface{}{"gcp_identity": f.assignBody("")["gcp_identity"]}, token)
	require.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
	stored, err := f.store.GetAgent(context.Background(), child.ID)
	require.NoError(t, err)
	assert.Nil(t, stored.AppliedConfig.GCPIdentity, "no SA written")

	f.assertGateUnrecorded(t, token, SurfaceAgentPatch)
}

// A child created by the legacy agent is denied the same three operations.
func TestLegacyParentDescendantInheritsUnrecordedDenial(t *testing.T) {
	f := newLegacyFixture(t, "legacy-desc")
	child, _ := f.childOf(t, f.legacy, "legacy-desc-c")
	token := f.agentToken(t, child.ID)

	writesBefore := countCreateWrites(t, f.store, f.proj.ID)
	assertSAGateDenied(t, f.createAsParent(t, token, f.assignBody("legacy-desc-esa")))
	assertAgentCreateWroteNothing(t, f.store, f.proj.ID, "legacy-desc-esa", child.ID, 0, writesBefore)

	grand, _ := f.createdAgent(t, f.createAsParent(t, token, CreateAgentRequest{Name: "legacy-desc-gc"}), "legacy-desc-gc")
	rec := doRequestWithAgentToken(t, f.srv, http.MethodPatch, "/api/v1/agents/"+grand.ID,
		map[string]interface{}{"gcp_identity": f.assignBody("")["gcp_identity"]}, token)
	require.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
	f.assertGateUnrecorded(t, token, SurfaceAgentPatch)

	f.setProjectAnnotation(t, projectSettingDefaultGCPIdentityMode, store.GCPMetadataModeAssign)
	f.setProjectAnnotation(t, projectSettingDefaultGCPIdentitySAID, f.sa.ID)
	writesBefore = countCreateWrites(t, f.store, f.proj.ID)
	assertSAGateDenied(t, f.createAsParent(t, token, CreateAgentRequest{Name: "legacy-desc-dsa"}))
	assertAgentCreateWroteNothing(t, f.store, f.proj.ID, "legacy-desc-dsa", child.ID, 1, writesBefore)
	f.assertGateUnrecorded(t, token, SurfaceAgentCreate)
}

// The legacy agent creates a child with no service account.
func TestLegacyAgentChildWithoutSAAllowed(t *testing.T) {
	f := newLegacyFixture(t, "legacy-nosa")
	_, edge := f.childOf(t, f.legacy, "legacy-nosa-c")
	assert.Equal(t, f.legacy.ID, edge.DelegatorID)
	assert.Equal(t, store.EffectCeilingBounded, edge.Kind)
	assert.Empty(t, deliverOf(edge.PermissionIDs))
}

// An agent whose only edge is unrecorded reads no project secret at
// runtime: both fetch endpoints answer not_found. An agent with a recorded
// edge reads it.
func TestLegacyAgentRuntimeSecretFetchNotFound(t *testing.T) {
	mf := newMaterialFixture(t, "legacy-fetch")
	ctx := context.Background()
	seedSecret(t, mf.Server.secretBackend, "LEGACY_KEY", "legacy-value", store.SecretTypeEnvironment, "LEGACY_KEY", mf.ProjectID)

	tokenFor := func(agentID string) string {
		a, err := mf.Store.GetAgent(ctx, agentID)
		require.NoError(t, err)
		a.AppliedConfig = &store.AgentAppliedConfig{AgentRole: string(AgentRoleFull)}
		require.NoError(t, mf.Store.UpdateAgent(ctx, a))
		tok, err := mf.Server.GenerateAgentTokenForAgent(ctx, a)
		require.NoError(t, err)
		return tok
	}

	createDCEdge(t, mf.Store, store.DelegationPrincipalUser, mf.UserID, store.DelegationPrincipalAgent, mf.AgentID,
		store.RoleScopeProject, mf.ProjectID, string(AgentRoleFull))
	assertProjectDenied(t, mf, mf.AgentID, tokenFor(mf.AgentID), "LEGACY_KEY")

	recordedID := tid("agent-legacy-fetch-recorded")
	createDCAgent(t, mf.Store, recordedID, mf.ProjectID, mf.UserID, AgentRoleFull)
	seedRecordedDelegationEdge(t, mf.Store, store.DelegationPrincipalUser, mf.UserID, store.DelegationPrincipalAgent, recordedID,
		store.RoleScopeProject, mf.ProjectID, string(AgentRoleFull))
	rec := doRequestWithAgentToken(t, mf.Server, http.MethodPost, "/api/v1/agent/secrets",
		secretFetchRequest{Keys: []string{"LEGACY_KEY"}}, tokenFor(recordedID))
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.Contains(t, rec.Body.String(), "legacy-value", "a recorded chain reads the secret")
}

// A child created by the legacy agent launches without the root user's
// progeny secret; a child of a recorded chain launches with it.
func TestLegacyAgentCreatedChildLaunchOmitsSecrets(t *testing.T) {
	f := newLegacyFixture(t, "legacy-launch")
	ctx := context.Background()
	backend := secret.NewLocalBackend(f.store, "test-hub-id", "test-secret")
	f.srv.SetSecretBackend(backend)
	disp := f.srv.GetDispatcher().(*HTTPAgentDispatcher)
	disp.SetSecretBackend(backend)
	disp.SetAuthzService(f.srv.authzService)
	_, _, err := backend.Set(ctx, &secret.SetSecretInput{
		Name: "PROGENY_KEY", Value: "progeny-value", SecretType: store.SecretTypeEnvironment, Target: "PROGENY_KEY",
		Scope: store.ScopeUser, ScopeID: f.owner.ID, AllowProgeny: true, InjectionMode: store.InjectionModeAlways,
		CreatedBy: f.owner.ID, UpdatedBy: f.owner.ID,
	})
	require.NoError(t, err)

	launched := func(child *store.Agent) bool {
		req := f.client.lastCreateReq
		require.NotNil(t, req)
		require.Equal(t, child.ID, req.ID)
		require.NotEqual(t, f.owner.ID, child.OwnerID, "resolved through the progeny path, not the owner's own scope")
		if _, ok := req.ResolvedEnv["PROGENY_KEY"]; ok {
			return true
		}
		for _, s := range req.ResolvedSecrets {
			if s.Name == "PROGENY_KEY" {
				return true
			}
		}
		return false
	}

	recorded, _ := f.createdAgent(t, doRequestAsUser(t, f.srv, f.owner, http.MethodPost, f.path,
		CreateAgentRequest{Name: "legacy-launch-p"}), "legacy-launch-p")
	control, _ := f.childOf(t, recorded, "legacy-launch-c2")
	assert.True(t, launched(control), "a recorded chain launches with the progeny secret")

	child, _ := f.childOf(t, f.legacy, "legacy-launch-c")
	assert.False(t, launched(child), "an unrecorded chain launches without it")
}
