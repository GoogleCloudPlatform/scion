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
	"log/slog"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/hub/permissions"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// edgeLookupErrStore fails GetDelegationEdgesForDelegate for one delegate
// with an error other than store.ErrNotFound.
type edgeLookupErrStore struct {
	store.Store
	failID string
}

func (s *edgeLookupErrStore) GetDelegationEdgesForDelegate(ctx context.Context, delegateType, delegateID string) ([]*store.DelegationEdge, error) {
	if delegateID == s.failID {
		return nil, errors.New("injected delegation edge lookup fault")
	}
	return s.Store.GetDelegationEdgesForDelegate(ctx, delegateType, delegateID)
}

// ceilingStoreErrorFixture is a project with an owner user and a
// user-delegated agent (the parent), after the edge backfill.
type ceilingStoreErrorFixture struct {
	store     store.Store
	projectID string
	userID    string
	parentID  string
}

func newCeilingStoreErrorFixture(t *testing.T, name string) ceilingStoreErrorFixture {
	t.Helper()
	_, s := setupDelegationCeilingTest(t)
	f := ceilingStoreErrorFixture{
		store:     s,
		projectID: tid("cse-proj-" + name),
		userID:    tid("cse-user-" + name),
		parentID:  tid("cse-parent-" + name),
	}
	createDCProject(t, s, f.projectID, "cse-"+name)
	createDCUser(t, s, f.userID, "cse-"+name+"@test.com", f.projectID, store.ProjectRoleOwner)
	createDCAgent(t, s, f.parentID, f.projectID, f.userID, AgentRoleFull)
	createDCEdge(t, s, store.DelegationPrincipalUser, f.userID, store.DelegationPrincipalAgent, f.parentID,
		store.RoleScopeProject, f.projectID, string(AgentRoleFull))
	markEdgeBackfillComplete(t, s)
	return f
}

// decisions returns the non-sensitive read and the write decisions for the
// agent callerID under authz.
func (f ceilingStoreErrorFixture) decisions(authz *AuthzService, callerID string) map[string]Decision {
	caller := dcAgentIdentity(callerID, f.projectID, AgentRoleFull)
	return map[string]Decision{
		"non-sensitive read": decidePerm(authz, caller, Resource{Type: "project", ID: f.projectID}, ActionRead, "project.read", false),
		"write": decidePerm(authz, caller,
			Resource{Type: "agent", ID: tid("cse-new-child"), ParentType: "project", ParentID: f.projectID}, ActionCreate, "agent.create", false),
	}
}

// A failed delegation-edge lookup denies every action with the delegation
// ceiling as the denying stage, for the calling agent (depth 0) and for an
// intermediate agent in its chain (depth 1).
func TestDelegationCeiling_EdgeLookupErrorDenies(t *testing.T) {
	t.Run("depth 0", func(t *testing.T) {
		f := newCeilingStoreErrorFixture(t, "d0")
		live := NewAuthzService(f.store, slog.Default())
		for name, d := range f.decisions(live, f.parentID) {
			require.True(t, d.Allowed, "%s with a live edge store: reason %q", name, d.Reason)
		}

		faulty := NewAuthzService(&edgeLookupErrStore{Store: f.store, failID: f.parentID}, slog.Default())
		for name, d := range f.decisions(faulty, f.parentID) {
			assert.False(t, d.Allowed, "%s: reason %q", name, d.Reason)
			assert.Equal(t, DeniedByDelegationCeiling, d.DeniedBy, name)
			assert.Contains(t, d.Reason, "injected delegation edge lookup fault", name)
		}
	})

	t.Run("depth 1", func(t *testing.T) {
		f := newCeilingStoreErrorFixture(t, "d1")
		childID := tid("cse-child-d1")
		createDCAgent(t, f.store, childID, f.projectID, f.userID, AgentRoleFull)
		createDCEdge(t, f.store, store.DelegationPrincipalAgent, f.parentID, store.DelegationPrincipalAgent, childID,
			store.RoleScopeProject, f.projectID, string(AgentRoleFull))

		live := NewAuthzService(f.store, slog.Default())
		for name, d := range f.decisions(live, childID) {
			require.True(t, d.Allowed, "%s with a live edge store: reason %q", name, d.Reason)
		}

		faulty := NewAuthzService(&edgeLookupErrStore{Store: f.store, failID: f.parentID}, slog.Default())
		for name, d := range f.decisions(faulty, childID) {
			assert.False(t, d.Allowed, "%s: reason %q", name, d.Reason)
			assert.Equal(t, DeniedByDelegationCeiling, d.DeniedBy, name)
			assert.Contains(t, d.Reason, "injected delegation edge lookup fault", name)
		}
	})
}

func stepNames(steps []DecisionStep) []string {
	out := make([]string, 0, len(steps))
	for _, s := range steps {
		out = append(out, s.Step)
	}
	return out
}

// walkDelegationChain denies when no permission is named, even for an
// agent whose chain holds the permission it would otherwise evaluate.
func TestWalkDelegationChain_EmptyPermissionDenies(t *testing.T) {
	f := newCeilingStoreErrorFixture(t, "noperm")
	authz := NewAuthzService(f.store, slog.Default())
	ctx := context.Background()
	res := Resource{Type: "project", ID: f.projectID}

	allowed, _, err := authz.walkDelegationChain(ctx, res, ActionRead, "project.read", f.parentID, true,
		store.RoleScopeProject, f.projectID, nil)
	require.NoError(t, err)
	require.True(t, allowed, "the chain holds project.read")

	var steps []DecisionStep
	allowed, reason, err := authz.walkDelegationChain(ctx, res, ActionRead, "", f.parentID, true,
		store.RoleScopeProject, f.projectID, &steps)
	require.NoError(t, err)
	assert.False(t, allowed)
	assert.Equal(t, "delegation ceiling: no permission to evaluate", reason)
	assert.Equal(t, []string{"delegation_ceiling_no_permission"}, stepNames(steps))
}

// Two active edges for one delegate in the request scope deny a read, even
// when each delegator holds the permission.
func TestWalkDelegationChain_DuplicateActiveEdgesDenyRead(t *testing.T) {
	f := newCeilingStoreErrorFixture(t, "dup")
	secondUser := tid("cse-user-dup-2")
	createDCUser(t, f.store, secondUser, "cse-dup-2@test.com", f.projectID, store.ProjectRoleOwner)
	st := &extraEdgeStore{Store: f.store, delegateID: f.parentID, extra: &store.DelegationEdge{
		ID:            tid("cse-dup-extra-edge"),
		DelegatorType: store.DelegationPrincipalUser, DelegatorID: secondUser,
		DelegateType: store.DelegationPrincipalAgent, DelegateID: f.parentID,
		ScopeType: store.RoleScopeProject, ScopeID: f.projectID, Role: string(AgentRoleFull), Active: true,
	}}
	ctx := context.Background()
	res := Resource{Type: "project", ID: f.projectID}

	live := NewAuthzService(f.store, slog.Default())
	allowed, _, err := live.walkDelegationChain(ctx, res, ActionRead, "project.read", f.parentID, true,
		store.RoleScopeProject, f.projectID, nil)
	require.NoError(t, err)
	require.True(t, allowed, "a single active edge holds project.read")

	var steps []DecisionStep
	dup := NewAuthzService(st, slog.Default())
	allowed, reason, err := dup.walkDelegationChain(ctx, res, ActionRead, "project.read", f.parentID, true,
		store.RoleScopeProject, f.projectID, &steps)
	require.NoError(t, err)
	assert.False(t, allowed)
	assert.Contains(t, reason, "multiple active delegation edges")
	assert.Equal(t, []string{"delegation_ceiling_duplicate_edges"}, stepNames(steps))
}

// stubSourceResolver returns a fixed source user or error.
type stubSourceResolver struct {
	user *store.User
	err  error
}

func (r stubSourceResolver) ResolveExecutionSource(context.Context, *store.Agent) (*store.User, error) {
	return r.user, r.err
}

// bindingListErrStore fails ListRoleBindingsForPrincipals, which project
// admission uses for membership evidence.
type bindingListErrStore struct {
	store.Store
}

func (s *bindingListErrStore) ListRoleBindingsForPrincipals(context.Context, []store.PrincipalRef, []string, []string) ([]*store.RoleBinding, error) {
	return nil, errors.New("injected role binding lookup fault")
}

// executionProjectAdmission denies when the resolved source user is not
// active, even where the store's user record and the delegation path are
// live, and denies when the admission lookup fails.
func TestExecutionProjectAdmission_Direct(t *testing.T) {
	gf := newGoldenFixture(t)
	ctx := context.Background()
	id := tid("exec-direct")
	seedExecutionAgent(t, gf.store, id, gf.projectAlpha.ID, []string{gf.projectOwnerID}, []string{gf.projectOwnerID})
	principal := principalContextForIdentity(execAgent(id, gf.projectAlpha.ID, []string{gf.projectOwnerID}))
	owner, err := gf.store.GetUser(ctx, gf.projectOwnerID)
	require.NoError(t, err)
	require.Equal(t, store.UserStatusActive, owner.Status)

	t.Run("live source is admitted", func(t *testing.T) {
		authz := NewAuthzService(gf.store, slog.Default())
		ok, detail := authz.executionProjectAdmission(ctx, principal, permissionProjectSecretRead)
		assert.True(t, ok, detail)
	})

	t.Run("resolved source user is suspended", func(t *testing.T) {
		authz := NewAuthzService(gf.store, slog.Default())
		suspended := *owner
		suspended.Status = store.UserStatusSuspended
		authz.sourceResolver = stubSourceResolver{user: &suspended}
		ok, detail := authz.executionProjectAdmission(ctx, principal, permissionProjectSecretRead)
		assert.False(t, ok)
		assert.Equal(t, "execution source user is not active", detail)
	})

	t.Run("admission lookup error", func(t *testing.T) {
		authz := NewAuthzService(&bindingListErrStore{Store: gf.store}, slog.Default())
		sourcePC := PrincipalContext{Kind: PrincipalKindUser, ID: owner.ID,
			Identity: NewAuthenticatedUser(owner.ID, owner.Email, owner.DisplayName, owner.Role, "")}
		_, admErr := authz.ProjectAdmissionForClass(ctx, sourcePC, gf.projectAlpha.ID, permissionProjectSecretRead,
			executionProjectClass(permissionProjectSecretRead), nil)
		require.Error(t, admErr, "the wrapper store makes the admission lookup fail")

		ok, detail := authz.executionProjectAdmission(ctx, principal, permissionProjectSecretRead)
		assert.False(t, ok)
		assert.Equal(t, "execution project admission check failed", detail)
	})
}

// withTestProgenyPolicyRow adds a progeny/agent/<kind> policy row for
// permissionID for the duration of the test. The test must not run in
// parallel with others.
func withTestProgenyPolicyRow(t *testing.T, kind, permissionID string) {
	t.Helper()
	orig := permissions.RelationshipPolicies
	rows := append([]permissions.RelationshipPolicy(nil), orig...)
	rows = append(rows, permissions.RelationshipPolicy{
		Relationship:   string(RelationshipRuleProgeny),
		PrincipalKinds: []string{"agent"},
		ResourceType:   kind,
		PermissionIDs:  []string{permissionID},
		ReadOnly:       true,
	})
	permissions.RelationshipPolicies = rows
	t.Cleanup(func() { permissions.RelationshipPolicies = orig })
}

// ProgenyListPredicate evaluates the source-delegation clause for the bare
// kind, so it is narrower than a point read: when the source agent's user
// delegator holds the read permission only through a relationship to the
// specific record (owner of the template), the point read admits the source
// and the list omits it. The shipped policy has no progeny row or adapter
// for a kind with such a user relationship, so the test adds both for
// template.read.
func TestProgenyListPredicate_NarrowerThanPointRead(t *testing.T) {
	withTestProgenyPolicyRow(t, "template", "template.read")
	f := newGoldenFixture(t)
	ctx := context.Background()

	sourceUser := tid("n3-source-user")
	require.NoError(t, f.store.CreateUser(ctx, &store.User{ID: sourceUser, Email: "n3@golden.test", DisplayName: "n3", Role: "member", Status: "active"}))
	// A relationship-derived execution-class permission requires the delegator's admission to the agent's project; this role admits without granting template.read.
	n3Reader, err := f.store.CreateRoleDefinition(ctx, &store.RoleDefinition{Name: "n3-reader", ScopeType: store.RoleScopeProject, Permissions: []string{"project.read"}})
	require.NoError(t, err)
	_, err = f.store.CreateRoleBinding(ctx, &store.RoleBinding{RoleDefinitionID: n3Reader.ID, PrincipalType: store.RoleBindingPrincipalUser, PrincipalID: sourceUser, ScopeType: store.RoleScopeProject, ScopeID: f.projectAlpha.ID, CreatedBy: "test"})
	require.NoError(t, err)
	sourceAgent := tid("n3-source-agent")
	seedExecutionAgent(t, f.store, sourceAgent, f.projectAlpha.ID, []string{sourceUser}, []string{sourceUser})

	readerID := tid("n3-reader")
	anc := []string{f.projectOwnerID, sourceAgent}
	seedExecutionAgent(t, f.store, readerID, f.projectAlpha.ID, anc, []string{f.projectOwnerID})
	reader := execAgent(readerID, f.projectAlpha.ID, anc)

	src := SharingSource{Kind: "template", ID: tid("n3-template"), OwnerID: sourceAgent, Policy: SharingPolicyOptInRequired, OptedIn: true}
	require.NoError(t, f.authz.RegisterProgenyAdapter(fakeProgenyAdapter{kind: "template", perms: []string{"template.read"}, sources: []SharingSource{src}}))

	// The source agent's delegator holds template.read on the record it owns,
	// and not on the bare kind.
	record := Resource{Type: "template", ID: src.ID, OwnerID: sourceUser}
	holds, detail := f.authz.relationshipSourceDelegationHolds(ctx, sourceAgent, record, "template.read")
	require.True(t, holds, "record-level delegation: %s", detail)
	holds, _ = f.authz.relationshipSourceDelegationHolds(ctx, sourceAgent, Resource{Type: "template"}, "template.read")
	require.False(t, holds, "kind-level delegation")

	d := decidePerm(f.authz, reader, record, ActionRead, "template.read", true)
	r := relationshipResult(t, d, RelationshipRuleProgeny)
	assert.True(t, r.Accepted, "point read admits the source: rejected by %q (%s)", r.RejectedBy, r.Detail)

	pred := f.authz.ProgenyListPredicate(ctx, principalContextForIdentity(reader), "template")
	assert.False(t, pred.Matches(src), "list omits the source")
}
