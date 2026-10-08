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

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ptone/scion#3948: a user's reincarnate that keeps the role re-records the
// agent's edge, with the user as delegator, when the agent's delegation
// chain has an unrecorded hop or its own edge is missing, so the reincarnate
// repairs a chain that denies with ceiling_unrecorded. A fully recorded
// chain, and any self or agent-requester reincarnate that keeps the role,
// keep the edge (ptone/scion#3762). A chain with a missing ancestor edge or a
// loop is refused by the agent standing gate before any of this runs.

// repairFixture is a project owned by user U with a full-role agent T
// created by U, an assign-mode service account, and no delegation edges:
// each test seeds the chain it needs.
type repairFixture struct {
	srv     *Server
	s       store.Store
	project *store.Project
	broker  *store.RuntimeBroker
	userID  string
	email   string
	target  *store.Agent
	sa      *store.GCPServiceAccount
}

func newRepairFixture(t *testing.T) *repairFixture {
	t.Helper()
	srv, s, project, broker := setupReincarnateTestServer(t, newReincarnateTestDispatcher())
	userID := tid("repair-user-" + t.Name())
	email := tidSlugSafe(t.Name()) + "@repair.test"
	createDCUser(t, s, userID, email, project.ID, store.ProjectRoleOwner)
	f := &repairFixture{srv: srv, s: s, project: project, broker: broker, userID: userID, email: email}
	f.target = newReincarnateTestAgent(t, s, project, broker, f.byUser(nil))
	f.sa = scaCreateSA(t, s, project.ID)
	return f
}

// byUser returns an agent mutation that makes the agent a full-role agent
// created by U, then applies extra.
func (f *repairFixture) byUser(extra func(a *store.Agent)) func(a *store.Agent) {
	return func(a *store.Agent) {
		a.CreatedBy = f.userID
		a.OwnerID = f.userID
		a.Ancestry = []string{f.userID}
		a.AppliedConfig.AgentRole = string(AgentRoleFull)
		if extra != nil {
			extra(a)
		}
	}
}

// otherAgent creates a stopped full-role agent named slug, created by U.
func (f *repairFixture) otherAgent(t *testing.T, slug string, ancestry ...string) *store.Agent {
	t.Helper()
	return newReincarnateTestAgent(t, f.s, f.project, f.broker, f.byUser(func(a *store.Agent) {
		a.ID = tid(slug + "-" + t.Name())
		a.Slug = slug + "-" + tidSlugSafe(t.Name())
		a.Phase = "stopped"
		if len(ancestry) > 0 {
			a.Ancestry = ancestry
		}
	}))
}

// seedUnrecordedEdge records an active project edge from delegator to agent
// with the full role and no recorded provenance (provenance version 0, the
// unrecorded ceiling kind), and returns it.
func seedUnrecordedEdge(t *testing.T, s store.Store, delegatorType, delegatorID string, agent *store.Agent) *store.DelegationEdge {
	t.Helper()
	e := &store.DelegationEdge{
		DelegatorType: delegatorType,
		DelegatorID:   delegatorID,
		DelegateType:  store.DelegationPrincipalAgent,
		DelegateID:    agent.ID,
		ScopeType:     store.RoleScopeProject,
		ScopeID:       agent.ProjectID,
		Role:          string(AgentRoleFull),
		Active:        true,
	}
	require.NoError(t, s.CreateDelegationEdge(context.Background(), e))
	require.Zero(t, e.ProvenanceVersion)
	require.Equal(t, store.EffectCeilingUnrecorded, e.Kind)
	return e
}

// session is U's signed-in session identity.
func (f *repairFixture) session() Identity {
	return NewAuthenticatedUser(f.userID, f.email, f.email, store.UserRoleMember, "web")
}

// reincarnateAs runs a real reincarnation of T by identity with an empty
// patch (the web button's body) and requires it to be accepted and settled.
func (f *repairFixture) reincarnateAs(t *testing.T, identity Identity) {
	t.Helper()
	rec := httptest.NewRecorder()
	f.srv.handleReincarnateAgent(rec, reincarnateRequest(t, f.target.ID, identity, ReincarnateAgentRequest{Handoff: "h"}), f.target.ID)
	require.Equal(t, http.StatusAccepted, rec.Code, rec.Body.String())
	waitForReincarnationSettled(t, f.s, f.target.ID)
}

// activeEdges returns T's active edges.
func (f *repairFixture) activeEdges(t *testing.T) []*store.DelegationEdge {
	t.Helper()
	edges, err := f.s.GetDelegationEdgesForDelegate(context.Background(), store.DelegationPrincipalAgent, f.target.ID)
	require.NoError(t, err)
	var active []*store.DelegationEdge
	for _, e := range edges {
		if e.Active {
			active = append(active, e)
		}
	}
	return active
}

// targetEdge returns T's single active edge.
func (f *repairFixture) targetEdge(t *testing.T) *store.DelegationEdge {
	t.Helper()
	active := f.activeEdges(t)
	require.Len(t, active, 1)
	return active[0]
}

// saAssignDecision is the SA-assign gate's Layer 1 decision for T assigning
// the fixture's service account, as T's full-role token.
func (f *repairFixture) saAssignDecision() Decision {
	identity := dcAgentIdentity(f.target.ID, f.project.ID, AgentRoleFull)
	ctx := contextWithIdentity(context.Background(), identity)
	return f.srv.authzService.CheckAccess(ctx, identity, gcpServiceAccountResource(f.sa), ActionAssign)
}

// assertSAAssignCeilingCleared asserts the delegation ceiling no longer
// stands between T and assigning the service account: the SA-assign gate's
// Layer 1 CheckAccess allows, so it does not deny with ceiling_unrecorded.
// It does not run Layer 2 (GCP actAs); assertRealSAAssignCreate does.
func (f *repairFixture) assertSAAssignCeilingCleared(t *testing.T) {
	t.Helper()
	d := f.saAssignDecision()
	assert.NotEqual(t, DenyCauseCeilingUnrecorded, d.DenyCause, "reason %q", d.Reason)
	assert.True(t, d.Allowed, "SA assign Layer 1: reason %q, cause %q", d.Reason, d.DenyCause)
}

// assertRealSAAssignCreate drives a real create of a child with an
// assign-mode service account, over HTTP, with T's own production token, and
// requires it to succeed through both layers of the SA-assign gate. T is given
// a GCP identity of its own (an assign-mode account other than the target),
// and the caller-permission checker is a stub that allows the target, so
// Layer 2 consults it once (the pattern of sa_assign_gate_wiring_test.go).
func (f *repairFixture) assertRealSAAssignCreate(t *testing.T) {
	t.Helper()
	ctx := context.Background()
	sa := wiringSA(t, f.s, store.ScopeProject, f.project.ID, "repair-child@p.iam.gserviceaccount.com")

	target := mustGetAgent(t, f.s, f.target.ID)
	target.AppliedConfig.GCPIdentity = &store.GCPIdentityConfig{
		MetadataMode:        store.GCPMetadataModeAssign,
		ServiceAccountID:    tid("repair-own-sa"),
		ServiceAccountEmail: "repair-own@p.iam.gserviceaccount.com",
	}
	require.NoError(t, f.s.UpdateAgent(ctx, target))
	token, err := f.srv.issueAgentTokenForTest(ctx, mustGetAgent(t, f.s, f.target.ID))
	require.NoError(t, err, "the repaired agent's token mint")

	f.srv.SetDispatcher(&createAgentDispatcher{createPhase: string(state.PhaseRunning)})
	checker := store.NewFakeCallerPermissionChecker().AllowTarget(sa.Email)
	enforceSAAssign(f.srv, checker)

	slug := "repair-child-" + tidSlugSafe(t.Name())
	rec := doRequestWithAgentToken(t, f.srv, http.MethodPost, "/api/v1/projects/"+f.project.ID+"/agents", CreateAgentRequest{
		Name: slug,
		Task: "do something",
		GCPIdentity: &GCPIdentityAssignment{
			MetadataMode:     store.GCPMetadataModeAssign,
			ServiceAccountID: sa.ID,
		},
	}, token)
	require.Equal(t, http.StatusCreated, rec.Code, "body: %s", rec.Body.String())
	require.Equal(t, 1, checker.CallCount(), "the actAs checker is consulted")
	assert.Equal(t, sa.ID, checker.Calls()[0].TargetSAID)

	child, err := f.s.GetAgentBySlug(ctx, f.project.ID, slug)
	require.NoError(t, err)
	require.NotNil(t, child.AppliedConfig)
	require.NotNil(t, child.AppliedConfig.GCPIdentity)
	assert.Equal(t, sa.ID, child.AppliedConfig.GCPIdentity.ServiceAccountID)
}

// assertReRecordedByUser asserts T's single active edge is a new edge from
// U with recorded session provenance and the stored (full) role, the claim
// audit reports re_recorded with replaced edges replacedIDs (deactivated
// with cause reincarnate_replaced), and the delegation ceiling no longer
// denies T the service-account assignment.
func (f *repairFixture) assertReRecordedByUser(t *testing.T, replacedIDs ...string) {
	t.Helper()
	got := f.targetEdge(t)
	for _, id := range replacedIDs {
		assert.NotEqual(t, id, got.ID, "the edge is re-recorded")
	}
	assert.Equal(t, store.DelegationPrincipalUser, got.DelegatorType)
	assert.Equal(t, f.userID, got.DelegatorID, "U is the recorded delegator")
	assert.Equal(t, string(AgentRoleFull), got.Role, "the stored role")
	assert.Equal(t, store.ProvenanceVersionV1, got.ProvenanceVersion, "recorded provenance")
	assert.Equal(t, store.DelegationPrincipalUser, got.SourcePrincipalKind)
	assert.Equal(t, f.userID, got.SourcePrincipalID)
	assert.Equal(t, store.SourceCredentialSession, got.SourceCredentialKind)
	assert.Equal(t, store.EffectCeilingPrincipal, got.Kind)
	assertCeilingFromSource(t, f.srv, f.session(), got.EffectCeiling)

	sum := auditSummary(t, f.s, mutationTypeAgentReincarnateClaim, f.target.ID)
	assert.Equal(t, true, sum["re_recorded"])
	assert.EqualValues(t, len(replacedIDs), sum["edges_replaced"])
	if len(replacedIDs) == 1 {
		assertReincarnateReplacedEdge(t, f.s, f.target.ID, replacedIDs[0])
	}
	assert.Equal(t, string(AgentRoleFull), mustGetAgent(t, f.s, f.target.ID).AppliedConfig.AgentRole, "the role is unchanged")
	f.assertSAAssignCeilingCleared(t)
}

// T's own edge is unrecorded: a user's same-role reincarnate replaces it
// with a recorded edge from the user, and T may then create a child with an
// assign-mode service account.
func TestReincarnateByUserRepairsUnrecordedOwnEdge(t *testing.T) {
	f := newRepairFixture(t)
	old := seedUnrecordedEdge(t, f.s, store.DelegationPrincipalUser, f.userID, f.target)
	assertUnrecordedDeny(t, f.saAssignDecision())

	f.reincarnateAs(t, f.session())
	f.assertReRecordedByUser(t, old.ID)
	f.assertRealSAAssignCreate(t)
}

// T's own edge is unrecorded and the hop above it is recorded with local
// development provenance on a server without dev auth, so the whole chain
// fails chainEffectCeiling as a source that is not accepted. The own edge is
// unrecorded, so the repair still runs: the user-rooted edge replaces the
// chain.
func TestReincarnateByUserRepairsUnrecordedOwnEdgeUnderDevLocalHop(t *testing.T) {
	f := newRepairFixture(t)
	ctx := context.Background()
	createDCUser(t, f.s, DevUserID, "dev-root@repair.test", f.project.ID, store.ProjectRoleMember)
	parent := f.otherAgent(t, "repair-devlocal-parent", DevUserID)
	require.NoError(t, f.s.CreateDelegationEdge(ctx, &store.DelegationEdge{
		DelegatorType: store.DelegationPrincipalUser,
		DelegatorID:   DevUserID,
		DelegateType:  store.DelegationPrincipalAgent,
		DelegateID:    parent.ID,
		ScopeType:     store.RoleScopeProject,
		ScopeID:       f.project.ID,
		Role:          string(AgentRoleFull),
		Active:        true,
		AuthorityProvenance: store.AuthorityProvenance{
			ProvenanceVersion:    store.ProvenanceVersionV1,
			SourcePrincipalKind:  store.DelegationPrincipalUser,
			SourcePrincipalID:    DevUserID,
			SourceCredentialKind: store.SourceCredentialDevLocal,
		},
		EffectCeiling: store.EffectCeiling{Kind: store.EffectCeilingPrincipal},
	}))
	old := seedUnrecordedEdge(t, f.s, store.DelegationPrincipalAgent, parent.ID, f.target)
	require.False(t, f.srv.authzService.devLocalAuthorityEnabled(), "fixture: dev auth is off")
	_, err := f.srv.authzService.chainEffectCeiling(ctx, mustGetAgent(t, f.s, f.target.ID))
	require.ErrorIs(t, err, errSourceNotAllowed, "fixture: the chain fold refuses the dev_local hop")

	f.reincarnateAs(t, f.session())
	f.assertReRecordedByUser(t, old.ID)
}

// Only an ancestor hop is unrecorded (U -> P unrecorded, P -> T recorded):
// the reincarnate re-records T's edge from U, which cuts the unrecorded hop
// out of T's chain.
func TestReincarnateByUserRepairsUnrecordedAncestor(t *testing.T) {
	f := newRepairFixture(t)
	parent := f.otherAgent(t, "repair-parent")
	seedUnrecordedEdge(t, f.s, store.DelegationPrincipalUser, f.userID, parent)
	old := seedFullAgentEdge(t, f.s, store.DelegationPrincipalAgent, parent.ID, f.target)
	assertUnrecordedDeny(t, f.saAssignDecision())

	f.reincarnateAs(t, f.session())
	f.assertReRecordedByUser(t, old.ID)
}

// T's own edge and its parent's are both unrecorded (U -> P and P -> T
// unrecorded): the reincarnate passes the standing gate and re-records T's
// edge from U.
func TestReincarnateByUserRepairsUnrecordedOwnAndAncestor(t *testing.T) {
	f := newRepairFixture(t)
	parent := f.otherAgent(t, "repair-unrec-both-parent")
	seedUnrecordedEdge(t, f.s, store.DelegationPrincipalUser, f.userID, parent)
	old := seedUnrecordedEdge(t, f.s, store.DelegationPrincipalAgent, parent.ID, f.target)
	assertUnrecordedDeny(t, f.saAssignDecision())

	f.reincarnateAs(t, f.session())
	f.assertReRecordedByUser(t, old.ID)
}

// T has no edge at all after the edge backfill completed: the reincarnate
// records one from U, deactivating nothing.
func TestReincarnateByUserRepairsMissingOwnEdge(t *testing.T) {
	f := newRepairFixture(t)
	markEdgeBackfillComplete(t, f.s)
	require.Empty(t, f.activeEdges(t))
	d := f.saAssignDecision()
	require.False(t, d.Allowed, "no edge: reason %q", d.Reason)

	f.reincarnateAs(t, f.session())
	f.assertReRecordedByUser(t)
	f.assertRealSAAssignCreate(t)
}

// assertRefusedByStanding asserts a user's same-role reincarnate of T is
// refused with 409 by the agent standing gate, on a dry run and a real run,
// with nothing written: T keeps its single edge old.
func (f *repairFixture) assertRefusedByStanding(t *testing.T, old *store.DelegationEdge) {
	t.Helper()
	target := mustGetAgent(t, f.s, f.target.ID)
	for _, dryRun := range []bool{true, false} {
		rec := httptest.NewRecorder()
		body := ReincarnateAgentRequest{Handoff: "h", DryRun: dryRun}
		f.srv.handleReincarnateAgent(rec, reincarnateRequest(t, f.target.ID, f.session(), body), f.target.ID)
		require.Equal(t, http.StatusConflict, rec.Code, "dryRun=%v: %s", dryRun, rec.Body.String())
		assert.Contains(t, rec.Body.String(), "suspended", "dryRun=%v", dryRun)
	}
	assertNothingClaimed(t, f.s, target, old)
}

// An ancestor has no edge (P has none, P -> T recorded): the chain is broken
// above its first link, so the standing gate refuses the reincarnate before
// the repair can run.
func TestReincarnateByUserMissingAncestorEdgeRefusedByStanding(t *testing.T) {
	f := newRepairFixture(t)
	markEdgeBackfillComplete(t, f.s)
	parent := f.otherAgent(t, "repair-orphan-parent")
	old := seedFullAgentEdge(t, f.s, store.DelegationPrincipalAgent, parent.ID, f.target)
	f.assertRefusedByStanding(t, old)
}

// A fully recorded chain (U -> P -> T, both recorded) keeps T's edge on a
// user's same-role reincarnate: T keeps P as its delegator.
func TestReincarnateByUserKeepsRecordedChain(t *testing.T) {
	f := newRepairFixture(t)
	parent := f.otherAgent(t, "repair-recorded-parent")
	seedFullAgentEdge(t, f.s, store.DelegationPrincipalUser, f.userID, parent)
	old := seedFullAgentEdge(t, f.s, store.DelegationPrincipalAgent, parent.ID, f.target)

	f.reincarnateAs(t, f.session())
	assertEdgeKept(t, f.s, f.target.ID, old, f.targetEdge(t))
}

// A looping chain (P -> T and T -> P, both recorded) is refused by the
// standing gate before the repair can run.
func TestReincarnateByUserLoopingChainRefusedByStanding(t *testing.T) {
	f := newRepairFixture(t)
	parent := f.otherAgent(t, "repair-loop-parent")
	seedFullAgentEdge(t, f.s, store.DelegationPrincipalAgent, f.target.ID, parent)
	old := seedFullAgentEdge(t, f.s, store.DelegationPrincipalAgent, parent.ID, f.target)
	f.assertRefusedByStanding(t, old)
}

// A self or agent-requester same-role reincarnate keeps the edge whether
// T's own edge or only an ancestor hop is unrecorded.
func TestReincarnateByAgentKeepsUnrecordedChain(t *testing.T) {
	type seed func(t *testing.T, f *repairFixture) *store.DelegationEdge
	chains := map[string]seed{
		"own edge unrecorded": func(t *testing.T, f *repairFixture) *store.DelegationEdge {
			return seedUnrecordedEdge(t, f.s, store.DelegationPrincipalUser, f.userID, f.target)
		},
		"ancestor unrecorded": func(t *testing.T, f *repairFixture) *store.DelegationEdge {
			parent := f.otherAgent(t, "repair-unrec-parent")
			seedUnrecordedEdge(t, f.s, store.DelegationPrincipalUser, f.userID, parent)
			return seedFullAgentEdge(t, f.s, store.DelegationPrincipalAgent, parent.ID, f.target)
		},
	}
	requesters := map[string]func(t *testing.T, f *repairFixture) Identity{
		"self": func(t *testing.T, f *repairFixture) Identity {
			return agentIdentityFor(f.target.ID, f.project.ID, ScopeAgentLifecycle)
		},
		"agent": func(t *testing.T, f *repairFixture) Identity {
			other := f.otherAgent(t, "repair-requester")
			seedFullAgentEdge(t, f.s, store.DelegationPrincipalUser, f.userID, other)
			return fullRequesterFor(other.ID, f.project.ID)
		},
	}
	for chainName, seedChain := range chains {
		for requesterName, requester := range requesters {
			t.Run(chainName+"/"+requesterName, func(t *testing.T) {
				f := newRepairFixture(t)
				old := seedChain(t, f)
				f.reincarnateAs(t, requester(t, f))
				assertEdgeKept(t, f.s, f.target.ID, old, f.targetEdge(t))
				assertUnrecordedDeny(t, f.saAssignDecision())
			})
		}
	}
}

// targetEdgeReadErrStore fails every delegation edge read for one delegate.
type targetEdgeReadErrStore struct {
	store.Store
	failID string
}

func (s *targetEdgeReadErrStore) GetDelegationEdgesForDelegate(ctx context.Context, delegateType, delegateID string) ([]*store.DelegationEdge, error) {
	if delegateID == s.failID {
		return nil, errInjectedLookup
	}
	return s.Store.GetDelegationEdgesForDelegate(ctx, delegateType, delegateID)
}

// A lookup fault while reading T's chain answers 503 on a dry run and a
// real run, with nothing written.
func TestReincarnateByUserChainLookupFault503(t *testing.T) {
	f := newRepairFixture(t)
	old := seedUnrecordedEdge(t, f.s, store.DelegationPrincipalUser, f.userID, f.target)
	target := mustGetAgent(t, f.s, f.target.ID)
	f.srv.authzService.store = &targetEdgeReadErrStore{Store: f.s, failID: f.target.ID}
	for _, dryRun := range []bool{true, false} {
		rec := httptest.NewRecorder()
		body := ReincarnateAgentRequest{Handoff: "h", DryRun: dryRun}
		f.srv.handleReincarnateAgent(rec, reincarnateRequest(t, f.target.ID, f.session(), body), f.target.ID)
		require.Equal(t, http.StatusServiceUnavailable, rec.Code, "dryRun=%v: %s", dryRun, rec.Body.String())
		assert.Contains(t, rec.Body.String(), "agent's delegation chain", "dryRun=%v", dryRun)
	}
	assertNothingClaimed(t, f.s, target, old)
}

// The mint's denial for the agent's own missing edge names a user's
// reincarnate beside a direct recreate; every other structural cause names a
// direct recreate only.
func TestAgentTokenDenialMessages(t *testing.T) {
	own := agentTokenDenialMessage(DenyCauseCeilingOrphaned, true)
	assert.Equal(t, agentTokenOwnEdgeMissingMessage, own)
	assert.Contains(t, own, "reincarnate this agent")
	assert.Contains(t, own, "recreate it directly")
	for _, cause := range []DenyCause{DenyCauseCeilingOrphaned, DenyCauseCeilingUnrecorded, DenyCauseCeilingError, ""} {
		msg := agentTokenDenialMessage(cause, false)
		assert.Contains(t, msg, "delegation record is missing or inconsistent", "cause %q", cause)
		assert.Contains(t, msg, "recreate it directly", "cause %q", cause)
		assert.NotContains(t, msg, "reincarnate", "cause %q: recreate only", cause)
	}
	src := agentTokenDenialMessage(DenyCauseCeilingSourceNotAllowed, true)
	assert.Equal(t, "The agent's delegation record names a source that is not accepted on this server", src)
}

// A mint for an agent with no edge of its own (after the edge backfill) is
// denied with the own-edge message; a mint for an agent whose parent has no
// edge is denied with the recreate-only message.
func TestAgentTokenMintOwnEdgeMissingMessage(t *testing.T) {
	mint := func(t *testing.T, f *repairFixture) (*agentTokenIssueError, *httptest.ResponseRecorder) {
		t.Helper()
		_, err := authorizeAgentTokenAt(context.Background(), f.srv, f.s, mustGetAgent(t, f.s, f.target.ID), mintSiteStart)
		var e *agentTokenIssueError
		require.ErrorAs(t, err, &e)
		rec := httptest.NewRecorder()
		require.True(t, writeAgentTokenIssueError(rec, e))
		require.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
		return e, rec
	}

	t.Run("own edge missing", func(t *testing.T) {
		f := newRepairFixture(t)
		markEdgeBackfillComplete(t, f.s)
		e, rec := mint(t, f)
		assert.Equal(t, DenyCauseCeilingOrphaned, e.Cause)
		assert.True(t, e.OwnEdgeMissing)
		assert.Equal(t, agentTokenOwnEdgeMissingMessage, decodeTargetAPIError(t, rec).Message)
	})

	t.Run("parent edge missing", func(t *testing.T) {
		f := newRepairFixture(t)
		markEdgeBackfillComplete(t, f.s)
		parent := f.otherAgent(t, "repair-mint-parent")
		seedFullAgentEdge(t, f.s, store.DelegationPrincipalAgent, parent.ID, f.target)
		e, rec := mint(t, f)
		assert.Equal(t, DenyCauseCeilingOrphaned, e.Cause)
		assert.False(t, e.OwnEdgeMissing)
		assert.Equal(t, agentTokenDenialMessage(DenyCauseCeilingOrphaned, false), decodeTargetAPIError(t, rec).Message)
	})
}
