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
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sort"
	"sync/atomic"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/hub/permissions"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// reissueFixture is a mint fixture holding a three-level chain in one
// project: user -> R (principal, session) -> P (bounded) -> A (bounded).
// P and A carry a bounded ceiling recorded before the artifact
// permissions existed, so neither is issued the artifact scopes.
type reissueFixture struct {
	*mintFixture
	// faults is installed as the server's store (and the authorization
	// service's) when the fixture is built; it is transparent until a test
	// arms it.
	faults              *reissueFaultStore
	root, parent, child *store.Agent
	operator            reissueOperator
}

// legacyBoundedCeiling is a bounded ceiling over the full role's coverage
// without the artifact permissions: the shape of a ceiling frozen before
// those permissions were added.
func legacyBoundedCeiling() store.EffectCeiling {
	var scopes []AgentTokenScope
	for _, s := range ScopesForRole(AgentRoleFull) {
		if !ceilingOptionalRoleScopes[s] {
			scopes = append(scopes, s)
		}
	}
	return boundedCeiling(agentScopeCoverage(scopes)...)
}

func agentProv(parentID string) store.AuthorityProvenance {
	return store.AuthorityProvenance{
		ProvenanceVersion:    store.ProvenanceVersionV1,
		SourcePrincipalKind:  store.DelegationPrincipalAgent,
		SourcePrincipalID:    parentID,
		SourceCredentialKind: store.SourceCredentialAgent,
		SourceCredentialID:   "jti-" + parentID,
	}
}

func newReissueFixture(t *testing.T, name string, topRole string) *reissueFixture {
	t.Helper()
	f, faults := newReissueMintFixture(t, name)
	setBackfillCompleted(t, f.store)
	// The test server enables dev auth, which raises every mint to the
	// full role; the re-issue is tested against production minting.
	f.srv.authzService.mintDevAuthOverride = false
	if topRole != store.ProjectRoleOwner {
		// Replace the owner with a user holding topRole.
		userID := tid(name + "-top")
		createDCUser(t, f.store, userID, name+"-top@test.com", f.projectID, topRole)
		f.userID = userID
	}
	r := f.agent(t, name+"-root", AgentRoleFull, state.PhaseRunning)
	f.edge(t, store.DelegationPrincipalUser, f.userID, r.ID, store.EffectCeiling{Kind: store.EffectCeilingPrincipal},
		store.AuthorityProvenance{ProvenanceVersion: 1, SourcePrincipalKind: store.DelegationPrincipalUser, SourcePrincipalID: f.userID, SourceCredentialKind: store.SourceCredentialSession})
	p := f.childAgent(t, name+"-parent", r, AgentRoleFull)
	a := f.childAgent(t, name+"-child", p, AgentRoleFull)
	return &reissueFixture{
		mintFixture: f, faults: faults, root: r, parent: p, child: a,
		operator: reissueOperator{UserID: DevUserID, CredentialKind: store.InitiatorCredentialKindSession},
	}
}

// newReissueMintFixture is newMintFixture with a reissueFaultStore installed
// on the server (and its authorization service) right after the server is
// built, before any audited setup (installStoreFault).
func newReissueMintFixture(t *testing.T, name string) (*mintFixture, *reissueFaultStore) {
	t.Helper()
	srv, s := testServer(t)
	faults, sw := installStoreFault(t, srv, func(inner store.Store, fault *storeFaultSwitch) *reissueFaultStore {
		return &reissueFaultStore{Store: inner, fault: fault}
	})
	faults.sw = sw
	srv.authzService.store = faults
	project := setupProjectWithBroker(t, s, name, name)
	client := &mintBrokerClient{mockRuntimeBrokerClient: &mockRuntimeBrokerClient{}}
	disp := NewHTTPAgentDispatcherWithClient(s, client, false, slog.Default())
	disp.SetTokenGenerator(srv)
	srv.SetDispatcher(disp)
	userID := tid(name + "-user")
	createDCUser(t, s, userID, name+"-user@test.com", project.ID, store.ProjectRoleOwner)
	return &mintFixture{
		srv: srv, store: s, disp: disp, client: client,
		projectID: project.ID, brokerID: tid("broker-" + name), userID: userID,
	}, faults
}

// reissueFaultStore injects the re-issue tests' store faults. It delegates
// everything until its switch is armed; then each configured fault applies:
//   - failParentOnce: the next GetAgent for failParentID fails (one shot,
//     re-armed by the test before each call);
//   - dupEdgeAgentID: that agent's active edge is reported twice;
//   - edgeReadErr: every agent-delegate edge read fails;
//   - auditFailInTx: the agent_scopes_reissued audit write inside a
//     transaction fails.
type reissueFaultStore struct {
	store.Store
	fault *storeFaultSwitch
	sw    *storeFaultSwitch

	failParentID   string
	failParentOnce atomic.Bool
	parentFired    atomic.Int32
	dupEdgeAgentID string
	edgeReadErr    bool
	auditFailInTx  bool
	auditFired     atomic.Int32
	// nilAgentInTxID: inside a transaction, GetAgent for this ID answers
	// no row and no error.
	nilAgentInTxID string
	// nilAgentAfterCommitID: once a transaction has committed, GetAgent for
	// this ID answers no row and no error.
	nilAgentAfterCommitID string
	committed             atomic.Bool
}

// arm turns the configured faults on.
func (s *reissueFaultStore) arm() { s.sw.Arm() }

func (s *reissueFaultStore) GetAgent(ctx context.Context, id string) (*store.Agent, error) {
	if s.fault.Active() && id == s.failParentID && s.failParentOnce.CompareAndSwap(true, false) {
		s.parentFired.Add(1)
		return nil, errors.New("injected agent read fault")
	}
	if s.fault.Active() && s.committed.Load() && id == s.nilAgentAfterCommitID {
		return nil, nil
	}
	return s.Store.GetAgent(ctx, id)
}

func (s *reissueFaultStore) GetDelegationEdgesForDelegate(ctx context.Context, delegateType, delegateID string) ([]*store.DelegationEdge, error) {
	if s.fault.Active() && s.edgeReadErr && delegateType == store.DelegationPrincipalAgent {
		return nil, errors.New("injected delegation edge read fault")
	}
	edges, err := s.Store.GetDelegationEdgesForDelegate(ctx, delegateType, delegateID)
	if err != nil || !s.fault.Active() || delegateID != s.dupEdgeAgentID || len(edges) == 0 {
		return edges, err
	}
	dup := *edges[0]
	dup.ID = "duplicate"
	return append(edges, &dup), nil
}

func (s *reissueFaultStore) WithTx(ctx context.Context, fn func(tx store.Store) error) error {
	if !s.fault.Active() {
		return s.Store.WithTx(ctx, fn)
	}
	err := s.Store.WithTx(ctx, func(tx store.Store) error {
		if s.auditFailInTx {
			tx = &auditFailStore{Store: tx, fired: &s.auditFired}
		}
		if s.nilAgentInTxID != "" {
			tx = &reissueNilAgentStore{Store: tx, id: s.nilAgentInTxID}
		}
		return fn(tx)
	})
	if err == nil {
		s.committed.Store(true)
	}
	return err
}

// reissueNilAgentStore answers GetAgent for id with no row and no error.
type reissueNilAgentStore struct {
	store.Store
	id string
}

func (s *reissueNilAgentStore) GetAgent(ctx context.Context, id string) (*store.Agent, error) {
	if id == s.id {
		return nil, nil
	}
	return s.Store.GetAgent(ctx, id)
}

// A store answering no row and no error for the agent inside the commit
// refuses the re-issue: nothing is written, revoked or pushed.
func TestScopeReissue_NilAgentInCommitRefuses(t *testing.T) {
	f := newReissueFixture(t, "rs-nil-tx", store.ProjectRoleOwner)
	f.run(t, f.parent, false)
	jti := "rs-nil-tx-jti"
	insertTestAgentCredential(t, f.store, f.child.ID, f.projectID, jti)
	credBefore := getTestAgentCredential(t, f.store, jti)
	edges := f.allEdges(t, f.child)
	child := f.reload(t, f.child)
	f.faults.nilAgentInTxID = f.child.ID
	f.faults.arm()
	f.client.resetAuthCalled = false

	resp, err := f.srv.runScopeReissue(context.Background(), child, f.operator, false)
	require.ErrorIs(t, err, errReissueConflict)
	assert.Nil(t, resp)
	rec := httptest.NewRecorder()
	writeScopeReissueError(rec, err)
	assert.Equal(t, http.StatusConflict, rec.Code)
	assert.Equal(t, edges, f.allEdges(t, f.child), "no edge change")
	assertCredentialUnrevoked(t, f.store, jti, credBefore)
	assert.False(t, f.client.resetAuthCalled)
	assert.Empty(t, reissueAudits(t, f.store, f.child.ID, mutationTypeAgentScopesReissued))
}

// A store answering no row and no error when the agent is re-read after
// the commit: the re-issue stands, no token is pushed, and the dispatch
// record says why.
func TestScopeReissue_NilAgentAfterCommitNotPushed(t *testing.T) {
	f := newReissueFixture(t, "rs-nil-after", store.ProjectRoleOwner)
	f.run(t, f.parent, false)
	child := f.reload(t, f.child)
	f.faults.nilAgentAfterCommitID = f.child.ID
	f.faults.arm()
	f.client.resetAuthCalled = false

	resp, err := f.srv.runScopeReissue(context.Background(), child, f.operator, false)
	require.NoError(t, err)
	assert.NotEmpty(t, resp.EdgeNew, "the commit stands")
	assert.False(t, resp.Dispatched)
	assert.NotEmpty(t, resp.DispatchError)
	assert.False(t, f.client.resetAuthCalled, "nothing pushed")
	recs := reissueAudits(t, f.store, f.child.ID, mutationTypeAgentScopesReissueDispatch)
	require.Len(t, recs, 1)
	assert.Contains(t, recs[0].AfterSummary, `"error_class":"lookup_error"`)
}

// childAgent stores a running child of parent with a legacy bounded edge.
func (f *mintFixture) childAgent(t *testing.T, slug string, parent *store.Agent, role AgentRole) *store.Agent {
	t.Helper()
	a := &store.Agent{
		ID: tid(slug), Slug: slug, Name: slug, ProjectID: f.projectID, OwnerID: f.userID,
		RuntimeBrokerID: f.brokerID, Phase: string(state.PhaseRunning), StateVersion: 1,
		Ancestry:      append(append([]string{}, parent.Ancestry...), parent.ID),
		AppliedConfig: &store.AgentAppliedConfig{AgentRole: string(role)},
		Created:       time.Now(), Updated: time.Now(),
	}
	require.NoError(t, f.store.CreateAgent(context.Background(), a))
	f.edge(t, store.DelegationPrincipalAgent, parent.ID, a.ID, legacyBoundedCeiling(), agentProv(parent.ID))
	return a
}

func (f *reissueFixture) reload(t *testing.T, a *store.Agent) *store.Agent {
	t.Helper()
	got, err := f.store.GetAgent(context.Background(), a.ID)
	require.NoError(t, err)
	return got
}

func (f *reissueFixture) grant(t *testing.T, a *store.Agent) []AgentTokenScope {
	t.Helper()
	g, err := f.srv.AuthorizeAgentToken(context.Background(), f.reload(t, a))
	require.NoError(t, err)
	return g.Scopes
}

func (f *reissueFixture) run(t *testing.T, a *store.Agent, dryRun bool) *ScopeReissueResponse {
	t.Helper()
	resp, err := f.srv.runScopeReissue(context.Background(), f.reload(t, a), f.operator, dryRun)
	require.NoError(t, err)
	return resp
}

// allEdges returns every edge of a, active or not.
func (f *reissueFixture) allEdges(t *testing.T, a *store.Agent) []*store.DelegationEdge {
	t.Helper()
	edges, err := f.store.ListAllDelegationEdgesForDelegate(context.Background(), store.DelegationPrincipalAgent, a.ID)
	require.NoError(t, err)
	return edges
}

func (f *reissueFixture) activeEdge(t *testing.T, a *store.Agent) *store.DelegationEdge {
	t.Helper()
	active, err := f.srv.authzService.activeProjectEdges(context.Background(), a.ID, f.projectID)
	require.NoError(t, err)
	require.Len(t, active, 1)
	return active[0]
}

func reissueAudits(t *testing.T, s store.Store, agentID, mutationType string) []*store.MutationAuditRecord {
	t.Helper()
	recs, _, err := s.ListMutationAudits(context.Background(), store.MutationAuditFilter{
		MutationType: mutationType, TargetType: "agent", TargetID: agentID,
	})
	require.NoError(t, err)
	return recs
}

func decodeReissueSummary(t *testing.T, rec *store.MutationAuditRecord) reissueAuditSummary {
	t.Helper()
	var s reissueAuditSummary
	require.NoError(t, json.Unmarshal([]byte(rec.AfterSummary), &s))
	return s
}

func artifactScopeStrings() []string {
	return []string{string(ScopeProjectArtifactRead), string(ScopeProjectArtifactWrite)}
}

func reissueSorted(in []string) []string {
	out := append([]string{}, in...)
	sort.Strings(out)
	return out
}

// walkAllows runs the step-10 chain walk for agent a on perm.
func (f *reissueFixture) walkAllows(t *testing.T, a *store.Agent, perm string) bool {
	t.Helper()
	resource, action, ok := reissuePermissionTarget(a, perm)
	require.True(t, ok)
	allowed, _, err := f.srv.authzService.walkDelegationChain(context.Background(), resource, action, perm, a.ID, true,
		store.RoleScopeProject, f.projectID, nil)
	require.NoError(t, err)
	return allowed
}

// T1: P, then A, re-issued top-down: both gain the artifact scopes, the
// step-10 walk allows the artifact permissions for A, A keeps its ID, and
// the audit lists the scopes as added.
func TestScopeReissue_T1_LegacyChainGainsArtifactScopes(t *testing.T) {
	f := newReissueFixture(t, "rs-t1", store.ProjectRoleOwner)

	for _, a := range []*store.Agent{f.parent, f.child} {
		got := scopeStrings(f.grant(t, a))
		for _, s := range artifactScopeStrings() {
			assert.NotContains(t, got, s, "before: %s lacks %s", a.Slug, s)
		}
	}
	assert.False(t, f.walkAllows(t, f.child, "artifact.read"), "before: walk denies artifact.read")

	// A first: P's own authority still lacks the artifact permissions, so
	// A gains nothing yet (top-down order matters).
	early := f.run(t, f.child, true)
	assert.Empty(t, early.Added, "A computed against P's unchanged edge gains nothing")

	oldParentEdge := f.activeEdge(t, f.parent)
	pResp := f.run(t, f.parent, false)
	assert.Equal(t, artifactScopeStrings(), reissueSorted(pResp.Added))
	assert.Empty(t, pResp.Removed)
	assert.False(t, pResp.Noop)
	assert.Equal(t, store.DelegationPrincipalAgent, pResp.CeilingSource.DelegatorKind)
	assert.Equal(t, f.root.ID, pResp.CeilingSource.DelegatorID)

	oldChildEdge := f.activeEdge(t, f.child)
	f.client.resetAuthCalled = false
	aResp := f.run(t, f.child, false)
	assert.Equal(t, artifactScopeStrings(), reissueSorted(aResp.Added))
	assert.Empty(t, aResp.Removed)
	assert.Equal(t, string(AgentRoleFull), aResp.RoleBefore)
	assert.Equal(t, string(AgentRoleFull), aResp.RoleAfter)
	assert.True(t, aResp.Dispatched, "running agent receives the new token")
	assert.True(t, f.client.resetAuthCalled)

	// The pushed token carries exactly the re-issued set.
	after := f.grant(t, f.child)
	assert.ElementsMatch(t, after, f.tokenClaims(t, f.client.resetAuthToken).Scopes)
	for _, s := range artifactScopeStrings() {
		assert.Contains(t, scopeStrings(after), s)
	}
	assert.True(t, f.walkAllows(t, f.child, "artifact.read"), "after: walk allows artifact.read")
	assert.True(t, f.walkAllows(t, f.child, "artifact.create"), "after: walk allows artifact.create")

	// A is not recreated; E is kept, inactive, with the re-issue cause;
	// E' copies E's source and names the operator as initiator.
	assert.Equal(t, f.child.ID, f.reload(t, f.child).ID)
	edges := f.allEdges(t, f.child)
	require.Len(t, edges, 2)
	var oldRow *store.DelegationEdge
	for _, e := range edges {
		if e.ID == oldChildEdge.ID {
			oldRow = e
		}
	}
	require.NotNil(t, oldRow)
	assert.False(t, oldRow.Active)
	assert.Equal(t, store.EdgeDeactivationScopeReissueReplaced, oldRow.Cause)
	newEdge := f.activeEdge(t, f.child)
	assert.Equal(t, aResp.EdgeNew, newEdge.ID)
	assert.Equal(t, f.parent.ID, newEdge.DelegatorID)
	assert.Equal(t, oldChildEdge.SourcePrincipalKind, newEdge.SourcePrincipalKind)
	assert.Equal(t, oldChildEdge.SourcePrincipalID, newEdge.SourcePrincipalID)
	assert.Equal(t, oldChildEdge.SourceCredentialKind, newEdge.SourceCredentialKind)
	assert.Equal(t, oldChildEdge.SourceCredentialID, newEdge.SourceCredentialID)
	assert.Equal(t, store.DelegationPrincipalUser, newEdge.InitiatorPrincipalKind)
	assert.Equal(t, f.operator.UserID, newEdge.InitiatorPrincipalID)
	assert.Equal(t, store.InitiatorCredentialKindSession, newEdge.InitiatorCredentialKind)
	assert.Equal(t, store.EffectCeilingBounded, newEdge.Kind)
	assert.Contains(t, newEdge.PermissionIDs, "artifact.read")
	assert.NotEqual(t, oldParentEdge.ID, f.activeEdge(t, f.parent).ID)

	// The audit record lists the diff and the edges, in the commit.
	recs := reissueAudits(t, f.store, f.child.ID, mutationTypeAgentScopesReissued)
	var applied *reissueAuditSummary
	for _, r := range recs {
		s := decodeReissueSummary(t, r)
		if !s.DryRun {
			applied = &s
		}
	}
	require.NotNil(t, applied)
	assert.Equal(t, aResp.OpID, applied.OpID)
	assert.Equal(t, artifactScopeStrings(), reissueSorted(applied.ScopesAdded))
	assert.Empty(t, applied.ScopesRemoved)
	assert.Equal(t, oldChildEdge.ID, applied.EdgeReplaced)
	assert.Equal(t, newEdge.ID, applied.EdgeNew)
	assert.Equal(t, "full", applied.RoleBefore)
	assert.Equal(t, "full", applied.RoleAfter)
	assert.Equal(t, reissueCeilingSource{
		DelegatorKind: "agent", DelegatorID: f.parent.ID,
		SourceCredentialKind: string(store.SourceCredentialAgent), SourceCredentialID: "jti-" + f.parent.ID,
		CeilingKind: string(store.EffectCeilingBounded),
	}, applied.CeilingSource)

	disp := reissueAudits(t, f.store, f.child.ID, mutationTypeAgentScopesReissueDispatch)
	require.Len(t, disp, 1)
	assert.JSONEq(t, `{"op_id":"`+aResp.OpID+`","credentials_revoked":0,"dispatched":true}`, disp[0].AfterSummary)
	assert.NotContains(t, disp[0].AfterSummary, "scope")
}

// T2: lowering the project maximum and re-issuing lowers the stored role,
// drops the scopes, and revokes the agent's credentials at once.
func TestScopeReissue_T2_Clawback(t *testing.T) {
	f := newReissueFixture(t, "rs-t2", store.ProjectRoleOwner)
	ctx := context.Background()
	jti := "rs-t2-jti"
	insertTestAgentCredential(t, f.store, f.child.ID, f.projectID, jti)

	project, err := f.store.GetProject(ctx, f.projectID)
	require.NoError(t, err)
	if project.Annotations == nil {
		project.Annotations = map[string]string{}
	}
	project.Annotations[projectSettingMaxAgentRole] = string(AgentRoleReadOnly)
	require.NoError(t, f.store.UpdateProject(ctx, project))

	before := f.grant(t, f.child)
	resp := f.run(t, f.child, false)
	assert.Equal(t, "full", resp.RoleBefore)
	assert.Equal(t, "readonly", resp.RoleAfter)
	assert.NotEmpty(t, resp.Removed)
	assert.Contains(t, resp.Removed, string(ScopeAgentCreate))
	assert.Equal(t, 1, resp.CredentialsRevoked)

	role, _ := agentRoleAndScopes(f.reload(t, f.child))
	assert.Equal(t, AgentRoleReadOnly, role, "stored role lowered")
	assert.Equal(t, string(AgentRoleReadOnly), f.activeEdge(t, f.child).Role)

	cred := getTestAgentCredential(t, f.store, jti)
	require.NotNil(t, cred.RevokedAt, "the prior credential is revoked in the commit")
	require.NotNil(t, cred.RevokeReason)
	assert.Equal(t, agentCredentialRevokeReasonScopesReissued, *cred.RevokeReason)

	after := f.grant(t, f.child)
	added, removed, _ := diffScopes(before, after)
	assert.Empty(t, added)
	assert.ElementsMatch(t, resp.Removed, scopeStrings(removed))
}

// T3: every whole-chain failure writes no edge and revokes nothing, and
// records a scope-free denial.
func TestScopeReissue_T3_FailClosed(t *testing.T) {
	cases := []struct {
		name  string
		setup func(t *testing.T, f *reissueFixture)
		fault func(f *reissueFixture)
		code  int
		cause string
	}{
		{
			name: "deleted parent",
			setup: func(t *testing.T, f *reissueFixture) {
				require.NoError(t, f.store.DeleteAgent(context.Background(), f.parent.ID))
			},
			code: http.StatusForbidden, cause: string(DenyCauseCeilingOrphaned),
		},
		{
			name:  "two active edges",
			fault: func(f *reissueFixture) { f.faults.dupEdgeAgentID = f.child.ID },
			code:  http.StatusForbidden, cause: string(DenyCauseCeilingOrphaned),
		},
		{
			name: "migration sentinel",
			setup: func(t *testing.T, f *reissueFixture) {
				replaceActiveEdge(t, f, store.DelegationPrincipalUser, migrationDelegatorID, store.EffectCeiling{}, store.AuthorityProvenance{})
			},
			code: http.StatusForbidden, cause: string(DenyCauseCeilingUnrecorded),
		},
		{
			name: "unrecorded edge",
			setup: func(t *testing.T, f *reissueFixture) {
				replaceActiveEdge(t, f, store.DelegationPrincipalAgent, f.parent.ID, store.EffectCeiling{}, store.AuthorityProvenance{})
			},
			code: http.StatusForbidden, cause: string(DenyCauseCeilingUnrecorded),
		},
		{
			name:  "injected edge read error",
			fault: func(f *reissueFixture) { f.faults.edgeReadErr = true },
			code:  http.StatusServiceUnavailable, cause: mintErrorClassLookup,
		},
	}
	for i, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newReissueFixture(t, "rs-t3-"+itoa(i), store.ProjectRoleOwner)
			jti := "rs-t3-jti-" + itoa(i)
			insertTestAgentCredential(t, f.store, f.child.ID, f.projectID, jti)
			if tc.setup != nil {
				tc.setup(t, f)
			}
			edgesBefore := f.allEdges(t, f.child)
			credBefore := getTestAgentCredential(t, f.store, jti)
			child := f.reload(t, f.child)
			if tc.fault != nil {
				tc.fault(f)
				f.faults.arm()
			}

			_, err := f.srv.runScopeReissue(context.Background(), child, f.operator, false)
			require.Error(t, err)

			rec := httptest.NewRecorder()
			writeScopeReissueError(rec, err)
			assert.Equal(t, tc.code, rec.Code, rec.Body.String())
			if tc.code == http.StatusForbidden {
				assert.Equal(t, string(DeniedByDelegationCeiling), decodeTargetAPIError(t, rec).Details["denied_by"])
			}

			assert.Equal(t, edgesBefore, f.allEdges(t, f.child), "no edge change")
			assertCredentialUnrevoked(t, f.store, jti, credBefore)
			assert.False(t, f.client.resetAuthCalled, "no token pushed")
			assert.Empty(t, reissueAudits(t, f.store, f.child.ID, mutationTypeAgentScopesReissued))
			assertIssueDeniedAudit(t, f.store, f.child.ID, mintSiteReissue, tc.cause)
		})
	}
}

// replaceActiveEdge swaps the child's active edge for one with the given
// delegator, ceiling and provenance.
func replaceActiveEdge(t *testing.T, f *reissueFixture, delegatorType, delegatorID string, c store.EffectCeiling, p store.AuthorityProvenance) {
	t.Helper()
	ctx := context.Background()
	_, err := f.store.DeactivateDelegationEdgesForDelegate(ctx, store.DelegationPrincipalAgent, f.child.ID, store.Deactivation{
		Cause: store.EdgeDeactivationReincarnateReplaced, OpID: "test-op",
	})
	require.NoError(t, err)
	f.edge(t, delegatorType, delegatorID, f.child.ID, c, p)
}

// T3a (CR1): a parent lookup error refuses the operation with a lookup
// fault; it never computes from a default role.
func TestScopeReissue_T3a_ParentLookupErrorRefuses(t *testing.T) {
	f := newReissueFixture(t, "rs-t3a", store.ProjectRoleOwner)
	jti := "rs-t3a-jti"
	insertTestAgentCredential(t, f.store, f.child.ID, f.projectID, jti)
	credBefore := getTestAgentCredential(t, f.store, jti)
	edgesBefore := f.allEdges(t, f.child)
	child := f.reload(t, f.child)
	// A one-shot fault on the first parent read: code that swallowed it
	// (for example by falling back to a default role) would go on to
	// compute a result from the later, successful reads.
	w := f.faults
	w.failParentID = f.parent.ID
	w.arm()

	w.failParentOnce.Store(true)
	_, err := f.srv.computeScopeReissue(context.Background(), child)
	require.Equal(t, int32(1), w.parentFired.Load(), "the fault hit the parent read")
	require.Error(t, err, "a parent lookup error refuses; it is never computed past")
	var issueErr *agentTokenIssueError
	require.ErrorAs(t, err, &issueErr)
	assert.True(t, issueErr.Lookup, "lookup fault, not a computed result")
	assert.Equal(t, mintSiteReissue, issueErr.Site)

	// The real run: the fault hits only the first parent read again.
	w.failParentOnce.Store(true)
	resp, err := f.srv.runScopeReissue(context.Background(), child, f.operator, false)
	require.Equal(t, int32(2), w.parentFired.Load())
	require.Error(t, err)
	assert.Nil(t, resp, "no result: never a default-role computation")

	rec := httptest.NewRecorder()
	writeScopeReissueError(rec, err)
	assert.Equal(t, http.StatusServiceUnavailable, rec.Code)
	assert.Equal(t, edgesBefore, f.allEdges(t, f.child), "no edge change")
	assertCredentialUnrevoked(t, f.store, jti, credBefore)
	recs := issueDeniedAudits(t, f.store, f.child.ID)
	require.Len(t, recs, 1, "the refused run records one scope-free denial")
	for _, r := range recs {
		assert.JSONEq(t, `{"site":"reissue","deny_cause":"lookup_error"}`, r.AfterSummary)
	}
	role, _ := agentRoleAndScopes(f.reload(t, f.child))
	assert.Equal(t, AgentRoleFull, role, "stored role unchanged")
	assert.Empty(t, reissueAudits(t, f.store, f.child.ID, mutationTypeAgentScopesReissued))
}

// auditFailStore fails the agent_scopes_reissued audit write; the
// reissueFaultStore wraps a transaction's store with it.
type auditFailStore struct {
	store.Store
	fired *atomic.Int32
}

func (s *auditFailStore) CreateMutationAudit(ctx context.Context, r *store.MutationAuditRecord) error {
	if r.MutationType == mutationTypeAgentScopesReissued {
		s.fired.Add(1)
		return errors.New("injected audit write fault")
	}
	return s.Store.CreateMutationAudit(ctx, r)
}

// CR2: the record, the role, the revocation and the audit row commit
// together. When the audit row cannot be written, nothing else is kept.
func TestScopeReissue_AuditFailureRollsBack(t *testing.T) {
	f := newReissueFixture(t, "rs-cr2", store.ProjectRoleOwner)
	ctx := context.Background()
	jti := "rs-cr2-jti"
	insertTestAgentCredential(t, f.store, f.child.ID, f.projectID, jti)
	credBefore := getTestAgentCredential(t, f.store, jti)
	// Lower the project maximum so the run would change the role too.
	project, err := f.store.GetProject(ctx, f.projectID)
	require.NoError(t, err)
	if project.Annotations == nil {
		project.Annotations = map[string]string{}
	}
	project.Annotations[projectSettingMaxAgentRole] = string(AgentRoleReadOnly)
	require.NoError(t, f.store.UpdateProject(ctx, project))
	edgesBefore := f.allEdges(t, f.child)

	child := f.reload(t, f.child)
	f.faults.auditFailInTx = true
	f.faults.arm()
	f.client.resetAuthCalled = false
	resp, err := f.srv.runScopeReissue(ctx, child, f.operator, false)
	require.Equal(t, int32(1), f.faults.auditFired.Load(), "the audit write inside the transaction was reached and failed")
	require.ErrorContains(t, err, "injected audit write fault", "the run failed because of the audit write")
	assert.Nil(t, resp)

	assert.Equal(t, edgesBefore, f.allEdges(t, f.child), "edge change rolled back")
	role, _ := agentRoleAndScopes(f.reload(t, f.child))
	assert.Equal(t, AgentRoleFull, role, "role change rolled back")
	assertCredentialUnrevoked(t, f.store, jti, credBefore)
	assert.False(t, f.client.resetAuthCalled, "nothing pushed")
	assert.Empty(t, reissueAudits(t, f.store, f.child.ID, mutationTypeAgentScopesReissued))
	assert.Empty(t, reissueAudits(t, f.store, f.child.ID, mutationTypeAgentScopesReissueDispatch))
}

// T4: a lookup fault for one permission withholds only the scopes covering
// it, with cause lookup, and E' leaves the permission out so refresh agrees.
func TestScopeReissue_T4_PerScopeWithhold(t *testing.T) {
	f := newReissueFixture(t, "rs-t4", store.ProjectRoleOwner)
	f.run(t, f.parent, false)

	prev := reissueLiveCheckFault
	reissueLiveCheckFault = func(perm string) error {
		if perm == "artifact.create" {
			return errors.New("injected permission lookup fault")
		}
		return nil
	}
	t.Cleanup(func() { reissueLiveCheckFault = prev })

	resp := f.run(t, f.child, false)
	assert.Equal(t, []string{string(ScopeProjectArtifactRead)}, resp.Added)
	assert.Contains(t, resp.Withheld, reissueWithheldScope{Scope: string(ScopeProjectArtifactWrite), Cause: reissueWithheldLookup})
	assert.NotContains(t, f.activeEdge(t, f.child).PermissionIDs, "artifact.create")

	after := f.grant(t, f.child)
	assert.Contains(t, scopeStrings(after), string(ScopeProjectArtifactRead))
	assert.NotContains(t, scopeStrings(after), string(ScopeProjectArtifactWrite), "refresh issues the re-issued set")
}

// T5: the operator is never an input. The result for a chain topped by a
// project member is the same under two different operators, and is no
// larger than the result for a chain topped by an owner.
func TestScopeReissue_T5_OperatorNeverRaises(t *testing.T) {
	member := newReissueFixture(t, "rs-t5m", store.ProjectRoleMember)
	member.run(t, member.parent, false)
	first, err := member.srv.computeScopeReissue(context.Background(), member.reload(t, member.child))
	require.NoError(t, err)

	member.operator = reissueOperator{UserID: "another-admin", CredentialKind: store.InitiatorCredentialKindSession}
	dry := member.run(t, member.child, true)
	assert.ElementsMatch(t, scopeStrings(first.added), dry.Added)
	assert.ElementsMatch(t, scopeStrings(first.removed), dry.Removed)
	assert.Equal(t, string(first.roleAfter), dry.RoleAfter)

	owner := newReissueFixture(t, "rs-t5o", store.ProjectRoleOwner)
	owner.run(t, owner.parent, false)
	ownerPlan, err := owner.srv.computeScopeReissue(context.Background(), owner.reload(t, owner.child))
	require.NoError(t, err)
	ownerSet := scopeSet(ownerPlan.after)
	for _, s := range first.after {
		assert.True(t, ownerSet[s], "member-derived scope %s is within the owner-derived set", s)
	}
}

// T6: no automatic path. Refresh after a re-issue issues the re-issued
// set; refresh without one on a legacy bounded chain still withholds the
// artifact scopes.
func TestScopeReissue_T6_RefreshUnchanged(t *testing.T) {
	f := newReissueFixture(t, "rs-t6", store.ProjectRoleOwner)

	tok := refreshedToken(t, f.refresh(t, f.reload(t, f.child), f.child.Ancestry))
	for _, s := range artifactScopeStrings() {
		assert.NotContains(t, scopeStrings(f.tokenClaims(t, tok).Scopes), s, "refresh never adds the scopes on its own")
	}
	assert.Len(t, f.allEdges(t, f.child), 1, "refresh records no edge")

	f.run(t, f.parent, false)
	resp := f.run(t, f.child, false)
	tok = refreshedToken(t, f.refresh(t, f.reload(t, f.child), f.child.Ancestry))
	assert.ElementsMatch(t, f.grant(t, f.child), f.tokenClaims(t, tok).Scopes)
	for _, s := range resp.Added {
		assert.Contains(t, scopeStrings(f.tokenClaims(t, tok).Scopes), s)
	}
}

// reissueHTTP calls the reset-auth handler with identity and cred.
func reissueHTTP(t *testing.T, srv *Server, agentID string, body any, identity Identity, cred CredentialContext) *httptest.ResponseRecorder {
	t.Helper()
	data, err := json.Marshal(body)
	require.NoError(t, err)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/agents/"+agentID+"/reset-auth", bytes.NewReader(data))
	req.Header.Set("Content-Type", "application/json")
	ctx := contextWithIdentity(req.Context(), identity)
	ctx = contextWithCredentialContext(ctx, cred)
	req = req.WithContext(ctx)
	rec := httptest.NewRecorder()
	srv.handleAgentResetAuth(rec, req, agentID)
	return rec
}

// T6 (operators): an agent token (including the agent itself), a user
// access token of a super-admin, and a non-super-admin session are all
// refused, and nothing is written.
func TestScopeReissue_OperatorRefusals(t *testing.T) {
	f := newReissueFixture(t, "rs-op", store.ProjectRoleOwner)
	body := ScopeReissueRequest{ReissueScopes: true}
	require.NoError(t, f.store.CreateUser(context.Background(), &store.User{
		ID: tid("rs-op-admin"), Email: "admin@test.com", DisplayName: "Admin", Role: "admin", Status: "active",
	}))
	grantSuperAdmin(t, f.store, tid("rs-op-admin"))
	admin := NewAuthenticatedUser(tid("rs-op-admin"), "admin@test.com", "Admin", "admin", "")
	member := NewAuthenticatedUser(f.userID, "owner@test.com", "Owner", "member", "")
	hubAdminID := tid("rs-op-hub-admin")
	require.NoError(t, f.store.CreateUser(context.Background(), &store.User{
		ID: hubAdminID, Email: "hub-admin@test.com", DisplayName: "Hub Admin", Role: "member", Status: "active",
	}))
	grantSystemRole(t, f.store, hubAdminID, store.SystemRoleHubAdmin)
	require.False(t, f.srv.authzService.IsSystemAdmin(context.Background(), hubAdminID), "hub-admin is not super-admin")
	hubAdmin := NewAuthenticatedUser(hubAdminID, "hub-admin@test.com", "Hub Admin", "member", "")
	selfClaims := &AgentTokenClaims{ProjectID: f.projectID, Scopes: ScopesForRole(AgentRoleFull), Ancestry: f.child.Ancestry}
	selfClaims.Subject = f.child.ID
	parentClaims := &AgentTokenClaims{ProjectID: f.projectID, Scopes: ScopesForRole(AgentRoleFull), Ancestry: f.parent.Ancestry}
	parentClaims.Subject = f.parent.ID
	uat := NewScopedUserIdentityWithCeiling(admin, f.projectID, []string{"project:agent:manage"}, "uat-rs-op",
		permissions.FrozenPermissionCeiling{Version: permissions.CeilingVersionV1, PermissionIDs: allRegistryIDs()})

	refused := []struct {
		name     string
		identity Identity
		cred     CredentialContext
	}{
		{"agent itself", &agentIdentityWrapper{selfClaims}, CredentialContext{Kind: CredentialKindAgentJWT}},
		{"parent agent", &agentIdentityWrapper{parentClaims}, CredentialContext{Kind: CredentialKindAgentJWT}},
		{"super-admin user access token", uat, credentialContextForIdentity(uat)},
		{"project owner session", member, CredentialContext{Kind: CredentialKindInteractive}},
		{"hub-admin session without super-admin", hubAdmin, CredentialContext{Kind: CredentialKindInteractive}},
	}
	for _, tc := range refused {
		t.Run(tc.name, func(t *testing.T) {
			rec := reissueHTTP(t, f.srv, f.child.ID, body, tc.identity, tc.cred)
			assert.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
		})
	}
	assert.Len(t, f.allEdges(t, f.child), 1, "nothing written")
	assert.Empty(t, reissueAudits(t, f.store, f.child.ID, mutationTypeAgentScopesReissued))

	// A super-admin session is admitted (dry run).
	rec := reissueHTTP(t, f.srv, f.child.ID, ScopeReissueRequest{ReissueScopes: true, DryRun: true}, admin, CredentialContext{Kind: CredentialKindInteractive})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var resp ScopeReissueResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.True(t, resp.DryRun)

	// dry_run without reissue_scopes is a malformed request.
	rec = reissueHTTP(t, f.srv, f.child.ID, ScopeReissueRequest{DryRun: true}, admin, CredentialContext{Kind: CredentialKindInteractive})
	assert.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
}

// The route end to end with the dev super-admin: the reset-auth route with
// the body re-issues; without it, it is a plain reset-auth.
func TestScopeReissue_Route(t *testing.T) {
	f := newReissueFixture(t, "rs-route", store.ProjectRoleOwner)
	f.run(t, f.parent, false)

	rec := doRequest(t, f.srv, http.MethodPost, "/api/v1/agents/"+f.child.ID+"/reset-auth", ScopeReissueRequest{ReissueScopes: true, DryRun: true})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var resp ScopeReissueResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.True(t, resp.DryRun)
	assert.Equal(t, artifactScopeStrings(), reissueSorted(resp.Added))
	assert.Len(t, f.allEdges(t, f.child), 1)

	rec = doRequest(t, f.srv, http.MethodPost, "/api/v1/projects/"+f.projectID+"/agents/"+f.child.ID+"/reset-auth", ScopeReissueRequest{ReissueScopes: true})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.False(t, resp.DryRun)
	assert.NotEmpty(t, resp.EdgeNew)

	// Plain reset-auth is unchanged: no edge written.
	f.client.resetAuthCalled = false
	rec = doRequest(t, f.srv, http.MethodPost, "/api/v1/agents/"+f.child.ID+"/reset-auth", nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	assert.True(t, f.client.resetAuthCalled)
	assert.Len(t, f.allEdges(t, f.child), 2)
}

// T9: a dry run writes no edge and no credential change, records a
// dry_run=true row, and its diff equals the real run's.
func TestScopeReissue_T9_DryRunMatchesRealRun(t *testing.T) {
	f := newReissueFixture(t, "rs-t9", store.ProjectRoleOwner)
	f.run(t, f.parent, false)
	jti := "rs-t9-jti"
	insertTestAgentCredential(t, f.store, f.child.ID, f.projectID, jti)
	credBefore := getTestAgentCredential(t, f.store, jti)
	edgesBefore := f.allEdges(t, f.child)

	f.client.resetAuthCalled = false
	dry := f.run(t, f.child, true)
	assert.True(t, dry.DryRun)
	assert.Empty(t, dry.EdgeNew)
	assert.Equal(t, edgesBefore, f.allEdges(t, f.child), "dry run writes no edge")
	assertCredentialUnrevoked(t, f.store, jti, credBefore)
	assert.False(t, f.client.resetAuthCalled, "dry run pushes nothing")
	recs := reissueAudits(t, f.store, f.child.ID, mutationTypeAgentScopesReissued)
	require.Len(t, recs, 1)
	drySummary := decodeReissueSummary(t, recs[0])
	assert.True(t, drySummary.DryRun)
	assert.Empty(t, drySummary.EdgeNew)

	real := f.run(t, f.child, false)
	assert.Equal(t, dry.Added, real.Added)
	assert.Equal(t, dry.Removed, real.Removed)
	assert.Equal(t, dry.Kept, real.Kept)
	assert.Equal(t, dry.Withheld, real.Withheld)
	assert.Equal(t, dry.RoleAfter, real.RoleAfter)
	assert.Equal(t, 1, real.CredentialsRevoked)
}

// A second re-issue is a no-op: nothing written, revoked or pushed.
func TestScopeReissue_NoopWritesNothing(t *testing.T) {
	f := newReissueFixture(t, "rs-noop", store.ProjectRoleOwner)
	f.run(t, f.parent, false)
	f.run(t, f.child, false)
	edges := f.allEdges(t, f.child)
	audits := len(reissueAudits(t, f.store, f.child.ID, mutationTypeAgentScopesReissued))
	jti := "rs-noop-jti"
	insertTestAgentCredential(t, f.store, f.child.ID, f.projectID, jti)
	credBefore := getTestAgentCredential(t, f.store, jti)

	f.client.resetAuthCalled = false
	resp := f.run(t, f.child, false)
	assert.True(t, resp.Noop)
	assert.Empty(t, resp.Added)
	assert.Empty(t, resp.Removed)
	assert.Equal(t, edges, f.allEdges(t, f.child))
	assertCredentialUnrevoked(t, f.store, jti, credBefore)
	assert.False(t, f.client.resetAuthCalled)
	assert.Len(t, reissueAudits(t, f.store, f.child.ID, mutationTypeAgentScopesReissued), audits)
}

// failingResetAuthClient fails every reset-auth push.
type failingResetAuthClient struct {
	*mintBrokerClient
}

func (m *failingResetAuthClient) ResetAuthAgent(_ context.Context, _, _, _, _, _, _ string) error {
	return errors.New("injected push failure")
}

// CR2: when the push fails after the commit, a follow-up record attributes
// the revoked credentials and the failure; the re-issue itself stands.
func TestScopeReissue_PushFailureIsAudited(t *testing.T) {
	f := newReissueFixture(t, "rs-push", store.ProjectRoleOwner)
	f.run(t, f.parent, false)
	insertTestAgentCredential(t, f.store, f.child.ID, f.projectID, "rs-push-jti")
	disp := NewHTTPAgentDispatcherWithClient(f.store, &failingResetAuthClient{f.client}, false, nil)
	disp.SetTokenGenerator(f.srv)
	f.srv.SetDispatcher(disp)

	resp := f.run(t, f.child, false)
	assert.False(t, resp.Dispatched)
	assert.NotEmpty(t, resp.DispatchError)
	assert.NotEmpty(t, resp.EdgeNew, "the commit stands")
	assert.Equal(t, 1, resp.CredentialsRevoked)

	recs := reissueAudits(t, f.store, f.child.ID, mutationTypeAgentScopesReissueDispatch)
	require.Len(t, recs, 1)
	var summary reissueDispatchSummary
	require.NoError(t, json.Unmarshal([]byte(recs[0].AfterSummary), &summary))
	assert.Equal(t, resp.OpID, summary.OpID)
	assert.Equal(t, 1, summary.CredentialsRevoked)
	assert.False(t, summary.Dispatched)
	assert.Equal(t, "dispatch_failed", summary.ErrorClass)
	assert.NotContains(t, recs[0].AfterSummary, "scope")
}

// A stopped agent gets E' and loses its credentials; nothing is pushed.
func TestScopeReissue_StoppedAgentNotPushed(t *testing.T) {
	f := newReissueFixture(t, "rs-stopped", store.ProjectRoleOwner)
	f.run(t, f.parent, false)
	child := f.reload(t, f.child)
	child.Phase = string(state.PhaseStopped)
	require.NoError(t, f.store.UpdateAgent(context.Background(), child))

	f.client.resetAuthCalled = false
	resp := f.run(t, f.child, false)
	assert.NotEmpty(t, resp.EdgeNew)
	assert.False(t, resp.Dispatched)
	assert.Empty(t, resp.DispatchError)
	assert.False(t, f.client.resetAuthCalled)
	recs := reissueAudits(t, f.store, f.child.ID, mutationTypeAgentScopesReissueDispatch)
	require.Len(t, recs, 1)
	assert.Contains(t, recs[0].AfterSummary, `"skipped":"agent_not_running"`)
}

// Phase 1 refuses a user-delegated agent without writing anything.
func TestScopeReissue_UserDelegatorNotYetSupported(t *testing.T) {
	f := newReissueFixture(t, "rs-user", store.ProjectRoleOwner)
	_, err := f.srv.runScopeReissue(context.Background(), f.reload(t, f.root), f.operator, false)
	require.ErrorIs(t, err, errReissueUnsupportedDelegator)
	rec := httptest.NewRecorder()
	writeScopeReissueError(rec, err)
	assert.Equal(t, http.StatusUnprocessableEntity, rec.Code)
	assert.Len(t, f.allEdges(t, f.root), 1)
}

// A dispatch during a re-issue records a mint denial against the reissue
// site, and plain reset-auth keeps its own site.
func TestMintSiteFromContext(t *testing.T) {
	assert.Equal(t, mintSiteResetAuth, mintSiteFromContext(context.Background(), mintSiteResetAuth))
	assert.Equal(t, mintSiteReissue, mintSiteFromContext(withMintSite(context.Background(), mintSiteReissue), mintSiteResetAuth))
}

// The re-issued set equals what agent creation by the same delegator
// issues today, for a user-rooted chain: it includes project:artifact:write
// (which also covers a permission no project role holds) and
// project:secret:read.
func TestScopeReissue_EqualsCreateToday(t *testing.T) {
	f := newReissueFixture(t, "rs-eq", store.ProjectRoleOwner)
	f.run(t, f.parent, false)
	resp := f.run(t, f.child, false)
	require.False(t, resp.Noop)
	reissued := f.grant(t, f.child)

	// Create a sibling of the child today, through the create route, as
	// the parent agent with its current token.
	parent := f.reload(t, f.parent)
	parentGrant, err := f.srv.AuthorizeAgentToken(context.Background(), parent)
	require.NoError(t, err)
	token, err := f.srv.agentTokenService.GenerateAgentToken(parent.ID, f.projectID, parentGrant.Scopes, append(append([]string{}, parent.Ancestry...), parent.ID))
	require.NoError(t, err)
	rec := doRequestWithAgentToken(t, f.srv, http.MethodPost, "/api/v1/projects/"+f.projectID+"/agents",
		CreateAgentRequest{Name: "rs-eq-sibling", AgentRole: string(AgentRoleFull)}, token)
	require.Less(t, rec.Code, 300, rec.Body.String())
	sibling, err := f.store.GetAgentBySlug(context.Background(), f.projectID, "rs-eq-sibling")
	require.NoError(t, err)
	created := f.grant(t, sibling)

	assert.ElementsMatch(t, created, reissued, "re-issue equals create-today")
	assert.Contains(t, scopeStrings(reissued), string(ScopeProjectArtifactWrite))
	assert.Contains(t, scopeStrings(reissued), string(ScopeProjectSecretRead))
	siblingCeiling := f.activeEdge(t, sibling).EffectCeiling
	childCeiling := f.activeEdge(t, f.child).EffectCeiling
	assert.True(t, effectCeilingsEqual(siblingCeiling, childCeiling),
		"same ceiling as creation (kind, version, permissions, boundary): created %+v, re-issued %+v", siblingCeiling, childCeiling)
	assert.Equal(t, store.EffectCeilingBounded, childCeiling.Kind)
	assert.Equal(t, f.projectID, childCeiling.BoundaryProjectID)
}

// grantSuperAdmin binds the system super-admin role to userID, as the
// system reconciler does (only it may create super-admin bindings).
func grantSuperAdmin(t *testing.T, s store.Store, userID string) {
	t.Helper()
	ctx := context.Background()
	rd, err := s.GetRoleDefinitionByName(ctx, store.SystemRoleSuperAdmin, store.RoleScopeSystem)
	require.NoError(t, err)
	_, err = s.CreateRoleBinding(ctx, &store.RoleBinding{
		RoleDefinitionID: rd.ID, PrincipalType: store.RoleBindingPrincipalUser, PrincipalID: userID,
		ScopeType: store.RoleScopeSystem, CreatedBy: store.SystemReconcileCreatedBy,
	})
	if err != nil && !errors.Is(err, store.ErrAlreadyExists) {
		require.NoError(t, err)
	}
}
