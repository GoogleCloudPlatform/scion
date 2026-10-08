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

// ptone/scion#3948: a user's reincarnate that keeps the role re-records the
// agent's edge, with the user as delegator, when the agent's delegation
// chain is unrecorded (an unrecorded hop, or a missing edge), so the
// reincarnate repairs a chain that denies with ceiling_unrecorded. A fully
// recorded chain, and any self or agent-requester reincarnate that keeps the
// role, keep the edge (ptone/scion#3762).

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

// assertSAAssignAllowed asserts T may assign the service account to a child
// it creates: the gate's CheckAccess allows, and the SA-assign gate at the
// create surface admits the request.
func (f *repairFixture) assertSAAssignAllowed(t *testing.T) {
	t.Helper()
	d := f.saAssignDecision()
	require.True(t, d.Allowed, "SA assign: reason %q, cause %q", d.Reason, d.DenyCause)

	identity := dcAgentIdentity(f.target.ID, f.project.ID, AgentRoleFull)
	ctx := contextWithIdentity(context.Background(), identity)
	rec := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodPost, "/api/v1/projects/"+f.project.ID+"/agents", nil).WithContext(ctx)
	assert.True(t, f.srv.authorizeSAAssignment(rec, r, f.sa, SurfaceAgentCreate), "the SA gate admits the create: %s", rec.Body.String())
}

// assertReRecordedByUser asserts T's single active edge is a new edge from
// U with recorded session provenance and the stored (full) role, the claim
// audit reports re_recorded with replaced edges replacedIDs (deactivated
// with cause reincarnate_replaced), and T may now assign the service
// account.
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
	f.assertSAAssignAllowed(t)
}

// T's own edge is unrecorded: a user's same-role reincarnate replaces it
// with a recorded edge from the user, and T may then assign a service
// account to a child it creates.
func TestReincarnateByUserRepairsUnrecordedOwnEdge(t *testing.T) {
	f := newRepairFixture(t)
	old := seedUnrecordedEdge(t, f.s, store.DelegationPrincipalUser, f.userID, f.target)
	assertUnrecordedDeny(t, f.saAssignDecision())

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
}

// An ancestor has no edge (P has none, P -> T recorded): the reincarnate
// re-records T's edge from U.
func TestReincarnateByUserRepairsMissingAncestorEdge(t *testing.T) {
	f := newRepairFixture(t)
	markEdgeBackfillComplete(t, f.s)
	parent := f.otherAgent(t, "repair-orphan-parent")
	old := seedFullAgentEdge(t, f.s, store.DelegationPrincipalAgent, parent.ID, f.target)
	d := f.saAssignDecision()
	require.False(t, d.Allowed, "no ancestor edge: reason %q", d.Reason)

	f.reincarnateAs(t, f.session())
	f.assertReRecordedByUser(t, old.ID)
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

// A chain that is broken in another way than unrecorded (here a loop:
// P -> T and T -> P, both recorded) keeps T's edge: only an unrecorded
// chain is repaired.
func TestReincarnateByUserKeepsLoopingChain(t *testing.T) {
	f := newRepairFixture(t)
	parent := f.otherAgent(t, "repair-loop-parent")
	seedFullAgentEdge(t, f.s, store.DelegationPrincipalAgent, f.target.ID, parent)
	old := seedFullAgentEdge(t, f.s, store.DelegationPrincipalAgent, parent.ID, f.target)

	f.reincarnateAs(t, f.session())
	assertEdgeKept(t, f.s, f.target.ID, old, f.targetEdge(t))
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
