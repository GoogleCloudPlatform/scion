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

// Package hub — the user-scope secret parent ceiling
// (EvaluateUserSecretParentCeiling) at three tiers: the evaluator, called
// directly, which pins every clause and cause from one passing base fixture
// with one change per row; the dispatch inside checkDelegationCeiling
// (called directly with a non-nil explain, because step 10 passes none);
// and Decide, where step 10 is shown to run before anything is asserted
// about it.
package hub

import (
	"context"
	"errors"
	"go/ast"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/hub/permissions"
	"github.com/GoogleCloudPlatform/scion/pkg/secret"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

var errUSPFault = errors.New("usp: injected store fault")

// uspStore wraps the fixture store: it fails the switched-on lookups, can
// override a user's kind and the progeny rows, and counts the loads the
// evaluator makes.
type uspStore struct {
	store.Store
	mu sync.Mutex

	failEdges, failAgent, failHold, failUser bool
	failBindings, failConstraints            bool
	failProgeny, failGroups                  bool
	// failGroupsAfterConstraints fails GetEffectiveGroups only once the
	// access-constraint table has been read, and groupFaults counts the
	// faults injected that way.
	failGroupsAfterConstraints bool
	groupFaults                int

	// failUserID, when set, limits failUser to GetUser(failUserID).
	failUserID string
	// userKind overrides the kind of the named users.
	userKind map[string]string
	// edgeMutate rewrites, per delegate, the edges the store returns, for
	// rows the schema does not accept.
	edgeMutate map[string]func(*store.DelegationEdge)
	// extraProgeny is appended to every ListProgenySecrets result.
	extraProgeny []store.Secret

	calls map[string]int
}

func (s *uspStore) count(name string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.calls == nil {
		s.calls = map[string]int{}
	}
	s.calls[name]++
}

func (s *uspStore) callCount(name string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls[name]
}

func (s *uspStore) totalCalls() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for _, c := range s.calls {
		n += c
	}
	return n
}

func (s *uspStore) GetDelegationEdgesForDelegate(ctx context.Context, delegateType, delegateID string) ([]*store.DelegationEdge, error) {
	s.count("GetDelegationEdgesForDelegate")
	if s.failEdges {
		return nil, errUSPFault
	}
	edges, err := s.Store.GetDelegationEdgesForDelegate(ctx, delegateType, delegateID)
	if err != nil {
		return nil, err
	}
	mut, ok := s.edgeMutate[delegateID]
	if !ok {
		return edges, nil
	}
	out := make([]*store.DelegationEdge, 0, len(edges))
	for _, e := range edges {
		c := *e
		mut(&c)
		out = append(out, &c)
	}
	return out, nil
}

func (s *uspStore) GetAgent(ctx context.Context, id string) (*store.Agent, error) {
	s.count("GetAgent")
	if s.failAgent {
		return nil, errUSPFault
	}
	return s.Store.GetAgent(ctx, id)
}

func (s *uspStore) HasActiveAgentHold(ctx context.Context, agentID string) (bool, error) {
	s.count("HasActiveAgentHold")
	if s.failHold {
		return false, errUSPFault
	}
	return s.Store.HasActiveAgentHold(ctx, agentID)
}

func (s *uspStore) GetUser(ctx context.Context, id string) (*store.User, error) {
	s.count("GetUser")
	if s.failUser && (s.failUserID == "" || s.failUserID == id) {
		return nil, errUSPFault
	}
	u, err := s.Store.GetUser(ctx, id)
	if err == nil && u != nil {
		if k, ok := s.userKind[id]; ok {
			c := *u
			c.Kind = k
			return &c, nil
		}
	}
	return u, err
}

func (s *uspStore) ListRoleBindingsForPrincipals(ctx context.Context, principals []store.PrincipalRef, scopeTypes []string, scopeIDs []string) ([]*store.RoleBinding, error) {
	s.count("ListRoleBindingsForPrincipals")
	if s.failBindings {
		return nil, errUSPFault
	}
	return s.Store.ListRoleBindingsForPrincipals(ctx, principals, scopeTypes, scopeIDs)
}

func (s *uspStore) ListAccessConstraints(ctx context.Context, limit, offset int) ([]*store.AccessConstraint, error) {
	s.count("ListAccessConstraints")
	if s.failConstraints {
		return nil, errUSPFault
	}
	return s.Store.ListAccessConstraints(ctx, limit, offset)
}

func (s *uspStore) ListProgenySecrets(ctx context.Context, ancestorIDs []string) ([]store.Secret, error) {
	s.count("ListProgenySecrets")
	if s.failProgeny {
		return nil, errUSPFault
	}
	rows, err := s.Store.ListProgenySecrets(ctx, ancestorIDs)
	if err != nil {
		return nil, err
	}
	return append(rows, s.extraProgeny...), nil
}

func (s *uspStore) GetEffectiveGroups(ctx context.Context, userID string) ([]string, error) {
	s.count("GetEffectiveGroups")
	if s.failGroups {
		return nil, errUSPFault
	}
	s.mu.Lock()
	late := s.failGroupsAfterConstraints && s.calls["ListAccessConstraints"] > 0
	if late {
		s.groupFaults++
	}
	s.mu.Unlock()
	if late {
		return nil, errUSPFault
	}
	return s.Store.GetEffectiveGroups(ctx, userID)
}

// uspFixture is base shape K: a plain project-owner user U, an agent in U's
// project whose stored ancestry is [U], one recorded edge from U to the
// agent (session source, principal ceiling, provenance version 1), and U's
// own user-scope secret shared with progeny. The evaluator runs on an
// authorization service over st, which wraps the real store.
type uspFixture struct {
	srv       *Server
	base      store.Store
	st        *uspStore
	a         *AuthzService
	projectID string
	userID    string
	agentID   string
	secretID  string
	edge      *store.DelegationEdge
}

const uspSecretKey = "USP_KEY"

func newUSPFixture(t *testing.T, name string) *uspFixture {
	t.Helper()
	srv, s := testServer(t)
	srv.SetSecretBackend(secret.NewLocalBackend(s, "test-hub-id", "test-secret"))
	f := &uspFixture{
		srv:       srv,
		base:      s,
		projectID: tid("usp-proj-" + name),
		userID:    tid("usp-user-" + name),
		agentID:   tid("usp-agent-" + name),
	}
	createDCProject(t, s, f.projectID, "usp-"+name)
	createDCUser(t, s, f.userID, "usp-"+name+"@test.com", f.projectID, store.ProjectRoleOwner)
	createDCAgent(t, s, f.agentID, f.projectID, f.userID, AgentRoleFull)
	setBackfillCompleted(t, s)
	f.edge = f.recordEdge(t, store.DelegationPrincipalUser, f.userID, f.agentID, store.EffectCeiling{Kind: store.EffectCeilingPrincipal}, nil)
	f.secretID = f.userSecret(t, f.userID, uspSecretKey, f.userID, true, store.SecretTypeEnvironment)
	f.st = &uspStore{Store: s}
	f.a = NewAuthzService(f.st, slog.Default())
	return f
}

// recordEdge records an active recorded edge from delegator to delegate in
// the fixture project. mutate, when non-nil, changes the edge before it is
// stored.
func (f *uspFixture) recordEdge(t *testing.T, delegatorType, delegatorID, delegateID string, ceiling store.EffectCeiling, mutate func(*store.DelegationEdge)) *store.DelegationEdge {
	t.Helper()
	cred := store.SourceCredentialSession
	if delegatorType == store.DelegationPrincipalAgent {
		cred = store.SourceCredentialAgent
	}
	e := &store.DelegationEdge{
		DelegatorType: delegatorType, DelegatorID: delegatorID,
		DelegateType: store.DelegationPrincipalAgent, DelegateID: delegateID,
		ScopeType: store.RoleScopeProject, ScopeID: f.projectID, Role: string(AgentRoleFull), Active: true,
		AuthorityProvenance: store.AuthorityProvenance{
			ProvenanceVersion:    store.ProvenanceVersionV1,
			SourcePrincipalKind:  delegatorType,
			SourcePrincipalID:    delegatorID,
			SourceCredentialKind: cred,
		},
		EffectCeiling: ceiling,
	}
	if mutate != nil {
		mutate(e)
	}
	require.NoError(t, f.base.CreateDelegationEdge(context.Background(), e))
	return e
}

// replaceAgentEdge deactivates every active edge of delegateID and records a
// new one built by mutate from the base recorded edge from U.
func (f *uspFixture) replaceAgentEdge(t *testing.T, delegateID string, mutate func(*store.DelegationEdge)) {
	t.Helper()
	f.deactivateEdges(t, delegateID)
	f.recordEdge(t, store.DelegationPrincipalUser, f.userID, delegateID, store.EffectCeiling{Kind: store.EffectCeilingPrincipal}, mutate)
}

func (f *uspFixture) deactivateEdges(t *testing.T, delegateID string) {
	t.Helper()
	ctx := context.Background()
	_, err := f.base.DeactivateDelegationEdgesForDelegate(ctx, store.DelegationPrincipalAgent, delegateID,
		store.Deactivation{Cause: store.EdgeDeactivationReincarnateReplaced, OpID: "usp-test"})
	require.NoError(t, err)
}

// userSecret stores a user-scope secret owned by scopeID and returns its ID.
func (f *uspFixture) userSecret(t *testing.T, scopeID, key, createdBy string, allowProgeny bool, secretType string) string {
	t.Helper()
	ctx := context.Background()
	_, _, err := f.srv.secretBackend.Set(ctx, &secret.SetSecretInput{
		Name: key, Value: "v", SecretType: secretType, Target: key,
		Scope: store.ScopeUser, ScopeID: scopeID, AllowProgeny: allowProgeny, CreatedBy: createdBy, UpdatedBy: createdBy,
	})
	require.NoError(t, err)
	meta, err := f.srv.secretBackend.GetMeta(ctx, key, store.ScopeUser, scopeID)
	require.NoError(t, err)
	require.NotNil(t, meta)
	return meta.ID
}

// agentChild stores an agent in the fixture project with the given
// ancestry and role.
func (f *uspFixture) agentChild(t *testing.T, name string, ancestry []string, role AgentRole) string {
	t.Helper()
	id := tid("usp-child-" + name)
	require.NoError(t, f.base.CreateAgent(context.Background(), &store.Agent{
		ID: id, Slug: "slug-" + id[:8], Name: "name-" + id[:8], ProjectID: f.projectID, Phase: "running",
		CreatedBy: ancestry[len(ancestry)-1], OwnerID: ancestry[0],
		AppliedConfig: &store.AgentAppliedConfig{AgentRole: string(role)}, Ancestry: ancestry,
	}))
	return id
}

func (f *uspFixture) identity(agentID string, ancestry []string) AgentIdentity {
	return newFullAgentIdentity(agentID, f.projectID, ancestry, []AgentTokenScope{ScopeProjectSecretRead})
}

func (f *uspFixture) principal() AgentIdentity {
	return f.identity(f.agentID, []string{f.userID})
}

func (f *uspFixture) resource() Resource {
	return userSecretResource(f.secretID, f.userID)
}

// eval runs the evaluator for the base principal on the base secret, with
// explain steps.
func (f *uspFixture) eval(t *testing.T) (ParentCeilingDecision, []DecisionStep) {
	t.Helper()
	return f.evalFor(t, f.principal(), f.resource())
}

func (f *uspFixture) evalFor(t *testing.T, p AgentIdentity, r Resource) (ParentCeilingDecision, []DecisionStep) {
	t.Helper()
	var explain []DecisionStep
	d, _ := f.a.evaluateUserSecretParentCeiling(context.Background(), p, r, &explain)
	// The exported form must agree with the explained one.
	exp := f.a.EvaluateUserSecretParentCeiling(context.Background(), p, r)
	require.Equal(t, d.Allowed, exp.Allowed)
	require.Equal(t, d.DenyCause, exp.DenyCause)
	return d, explain
}

func (f *uspFixture) addUserConstraint(t *testing.T, userType, userID string, perms []string) {
	t.Helper()
	_, err := f.base.CreateAccessConstraint(context.Background(), &store.AccessConstraint{
		Name:                 "usp-" + userID[:8],
		SubjectKind:          store.ConstraintSubjectPrincipal,
		SubjectPrincipalType: pvStrPtr(userType),
		SubjectPrincipalID:   pvStrPtr(userID),
		ScopeType:            store.RoleScopeSystem,
		MaximumPermissions:   perms,
		Purpose:              "usp test constraint",
		CreatedBy:            "usp-test",
	})
	require.NoError(t, err)
}

func assertUSPDeny(t *testing.T, d ParentCeilingDecision, steps []DecisionStep, cause DenyCause, step string) {
	t.Helper()
	require.False(t, d.Allowed, "reason %q", d.Reason)
	assert.Equal(t, DeniedByDelegationCeiling, d.DeniedBy, "reason %q", d.Reason)
	assert.Equal(t, cause, d.DenyCause, "reason %q steps %+v", d.Reason, steps)
	if step != "" {
		assert.True(t, hasDecisionStep(steps, "secret_parent_ceiling_"+step), "want step %s in %+v", step, steps)
	}
}

func hasSecretParentStep(steps []DecisionStep) bool {
	for _, s := range steps {
		if strings.HasPrefix(s.Step, "secret_parent_ceiling_") {
			return true
		}
	}
	return false
}

func hasGenericCeilingStep(steps []DecisionStep) bool {
	for _, s := range steps {
		if strings.HasPrefix(s.Step, "delegation_ceiling_") {
			return true
		}
	}
	return false
}

// ---- positive (T1-T4) --------------------------------------------------------

func TestUserSecretParentCeiling_BaseShapeAllows(t *testing.T) {
	f := newUSPFixture(t, "base")
	assertNotSystemAdmin(t, f.a, context.Background(), f.userID)
	d, steps := f.eval(t)
	require.True(t, d.Allowed, "reason %q steps %+v", d.Reason, steps)
	assert.Equal(t, ParentProof{
		Kind:                 ParentProofKindOwnershipFact,
		Permission:           "secret.use",
		SourcePrincipal:      ProvenancePrincipal{Kind: store.DelegationPrincipalUser, ID: f.userID},
		SourceCredentialKind: store.SourceCredentialSession,
		ResourceID:           f.secretID,
	}, d.Proof)
	assert.True(t, hasDecisionStep(steps, "secret_parent_ceiling_allowed"), "steps %+v", steps)
}

func TestUserSecretParentCeiling_BoundedAgentHopHoldingSecretUseAllows(t *testing.T) {
	f := newUSPFixture(t, "grandchild")
	child := f.agentChild(t, "gc", []string{f.userID, f.agentID}, AgentRoleFull)
	f.recordEdge(t, store.DelegationPrincipalAgent, f.agentID, child, boundedCeiling("secret.use", "project.secret_read"), nil)
	d, steps := f.evalFor(t, f.identity(child, []string{f.userID, f.agentID}), f.resource())
	require.True(t, d.Allowed, "reason %q steps %+v", d.Reason, steps)
	assert.True(t, hasDecisionStep(steps, "secret_parent_ceiling_link_allowed"), "steps %+v", steps)
}

func TestUserSecretParentCeiling_DevLocalWithDevAuthAllows(t *testing.T) {
	f := newUSPFixture(t, "devlocal")
	createDCUser(t, f.base, DevUserID, "dev@localhost", f.projectID, store.ProjectRoleOwner)
	devAgent := f.agentChild(t, "dev", []string{DevUserID}, AgentRoleFull)
	f.recordEdge(t, store.DelegationPrincipalUser, DevUserID, devAgent, store.EffectCeiling{Kind: store.EffectCeilingPrincipal}, func(e *store.DelegationEdge) {
		e.SourceCredentialKind = store.SourceCredentialDevLocal
	})
	devSecret := f.userSecret(t, DevUserID, "USP_DEV_KEY", DevUserID, true, store.SecretTypeEnvironment)
	f.a.setDevLocalAuthorityEnabled(true)
	d, steps := f.evalFor(t, f.identity(devAgent, []string{DevUserID}), userSecretResource(devSecret, DevUserID))
	require.True(t, d.Allowed, "reason %q steps %+v", d.Reason, steps)

	// T15: with dev auth off the same hop denies at S3b.
	f.a.setDevLocalAuthorityEnabled(false)
	d, steps = f.evalFor(t, f.identity(devAgent, []string{DevUserID}), userSecretResource(devSecret, DevUserID))
	assertUSPDeny(t, d, steps, DenyCauseCeilingSourceNotAllowed, "dev_local")
}

func TestUserSecretParentCeiling_SuperAdminRootTakesTheSamePath(t *testing.T) {
	f := newUSPFixture(t, "superadmin")
	admin := tid("usp-superadmin")
	createTestUserWithRole(t, f.base, admin, "usp-root-admin@test.com", "member", store.SystemRoleSuperAdmin)
	createDCUser(t, f.base, admin, "usp-root-admin@test.com", f.projectID, store.ProjectRoleOwner)
	ag := f.agentChild(t, "sa", []string{admin}, AgentRoleFull)
	f.recordEdge(t, store.DelegationPrincipalUser, admin, ag, boundedCeiling("project.read"), nil)
	sec := f.userSecret(t, admin, "USP_SA_KEY", admin, true, store.SecretTypeEnvironment)
	// No admin shortcut: the bounded hop without secret.use denies.
	d, steps := f.evalFor(t, f.identity(ag, []string{admin}), userSecretResource(sec, admin))
	assertUSPDeny(t, d, steps, DenyCauseCeilingEffectExceeded, "effect_exceeded")
	f.replaceAgentEdge(t, ag, func(e *store.DelegationEdge) {
		e.DelegatorID, e.SourcePrincipalID = admin, admin
	})
	d, steps = f.evalFor(t, f.identity(ag, []string{admin}), userSecretResource(sec, admin))
	require.True(t, d.Allowed, "reason %q steps %+v", d.Reason, steps)
}

// ---- negative, evaluator level, single fault ---------------------------------

func TestUserSecretParentCeiling_SingleFaultDenies(t *testing.T) {
	type row struct {
		name   string
		mutate func(t *testing.T, f *uspFixture) (AgentIdentity, Resource)
		cause  DenyCause
		step   string
	}
	base := func(f *uspFixture) (AgentIdentity, Resource) { return f.principal(), f.resource() }
	rows := []row{
		// T7, T11 (a UAT-shaped ceiling has no secret.use until explicit selectors exist)
		{"bounded hop without secret.use", func(t *testing.T, f *uspFixture) (AgentIdentity, Resource) {
			f.replaceAgentEdge(t, f.agentID, func(e *store.DelegationEdge) { e.EffectCeiling = boundedCeiling("project.read", "agent.read") })
			return base(f)
		}, DenyCauseCeilingEffectExceeded, "effect_exceeded"},
		// secret.use is intrinsic to a hop recorded from a user access token
		// (uatHopEffectCeiling), but a ceiling version Allows does not
		// interpret still denies every permission.
		{"uat-created child, ceiling version not interpreted", func(t *testing.T, f *uspFixture) (AgentIdentity, Resource) {
			f.replaceAgentEdge(t, f.agentID, func(e *store.DelegationEdge) {
				e.SourceCredentialKind = store.SourceCredentialUAT
				e.EffectCeiling = boundedCeiling("project.read", "project.secret_read")
				e.EffectCeiling.Version = permissions.CeilingVersion(99)
			})
			return base(f)
		}, DenyCauseCeilingEffectExceeded, "effect_exceeded"},
		// T8
		{"kind unrecorded", func(t *testing.T, f *uspFixture) (AgentIdentity, Resource) {
			f.replaceAgentEdge(t, f.agentID, func(e *store.DelegationEdge) {
				e.EffectCeiling = store.EffectCeiling{Kind: store.EffectCeilingUnrecorded}
			})
			return base(f)
		}, DenyCauseCeilingUnrecorded, "unrecorded"},
		{"provenance version 0", func(t *testing.T, f *uspFixture) (AgentIdentity, Resource) {
			f.replaceAgentEdge(t, f.agentID, func(e *store.DelegationEdge) { e.ProvenanceVersion = 0 })
			return base(f)
		}, DenyCauseCeilingUnrecorded, "unrecorded"},
		{"unknown provenance version", func(t *testing.T, f *uspFixture) (AgentIdentity, Resource) {
			f.replaceAgentEdge(t, f.agentID, func(e *store.DelegationEdge) { e.ProvenanceVersion = 2 })
			return base(f)
		}, DenyCauseCeilingUnrecorded, "unrecorded"},
		// T8s
		{"migration sentinel", func(t *testing.T, f *uspFixture) (AgentIdentity, Resource) {
			f.replaceAgentEdge(t, f.agentID, func(e *store.DelegationEdge) {
				e.DelegatorID, e.SourcePrincipalID = migrationDelegatorID, migrationDelegatorID
			})
			return base(f)
		}, DenyCauseCeilingOrphaned, "migration_edge"},
		// T9
		{"no edge", func(t *testing.T, f *uspFixture) (AgentIdentity, Resource) {
			f.deactivateEdges(t, f.agentID)
			return base(f)
		}, DenyCauseCeilingProvenanceMissing, "no_edge"},
		{"ancestor edge missing", func(t *testing.T, f *uspFixture) (AgentIdentity, Resource) {
			child := f.agentChild(t, "anc-missing", []string{f.userID, f.agentID}, AgentRoleFull)
			f.recordEdge(t, store.DelegationPrincipalAgent, f.agentID, child, store.EffectCeiling{Kind: store.EffectCeilingPrincipal}, nil)
			f.deactivateEdges(t, f.agentID)
			return f.identity(child, []string{f.userID, f.agentID}), f.resource()
		}, DenyCauseCeilingProvenanceMissing, "no_edge"},
		// T10
		{"terminal user suspended", func(t *testing.T, f *uspFixture) (AgentIdentity, Resource) {
			setUserStatus(t, f.base, f.userID, "suspended")
			return base(f)
		}, DenyCauseCeilingOrphaned, "source_not_live"},
		{"intermediate agent held", func(t *testing.T, f *uspFixture) (AgentIdentity, Resource) {
			child := f.agentChild(t, "held-parent", []string{f.userID, f.agentID}, AgentRoleFull)
			f.recordEdge(t, store.DelegationPrincipalAgent, f.agentID, child, store.EffectCeiling{Kind: store.EffectCeilingPrincipal}, nil)
			holdAgent(t, f.base, f.agentID, f.projectID, f.userID)
			return f.identity(child, []string{f.userID, f.agentID}), f.resource()
		}, DenyCauseCeilingOrphaned, "delegator_not_live"},
		{"intermediate agent deleted", func(t *testing.T, f *uspFixture) (AgentIdentity, Resource) {
			child := f.agentChild(t, "del-parent", []string{f.userID, f.agentID}, AgentRoleFull)
			f.recordEdge(t, store.DelegationPrincipalAgent, f.agentID, child, store.EffectCeiling{Kind: store.EffectCeilingPrincipal}, nil)
			softDeleteAgentRow(t, f.base, f.agentID)
			return f.identity(child, []string{f.userID, f.agentID}), f.resource()
		}, DenyCauseCeilingOrphaned, "delegator_not_live"},
		{"intermediate agent lacks the scope", func(t *testing.T, f *uspFixture) (AgentIdentity, Resource) {
			child := f.agentChild(t, "noscope-parent", []string{f.userID, f.agentID}, AgentRoleFull)
			f.recordEdge(t, store.DelegationPrincipalAgent, f.agentID, child, store.EffectCeiling{Kind: store.EffectCeilingPrincipal}, nil)
			setAgentRole(t, f.base, f.agentID, AgentRoleNone)
			return f.identity(child, []string{f.userID, f.agentID}), f.resource()
		}, DenyCauseCeilingDelegatorLacksPermission, "delegator_lacks_permission"},
		// T12
		{"secret deleted", func(t *testing.T, f *uspFixture) (AgentIdentity, Resource) {
			require.NoError(t, f.srv.secretBackend.Delete(context.Background(), uspSecretKey, store.ScopeUser, f.userID))
			return base(f)
		}, DenyCauseCeilingResourceMissing, "resource_missing"},
		{"not shared with progeny", func(t *testing.T, f *uspFixture) (AgentIdentity, Resource) {
			f.userSecret(t, f.userID, uspSecretKey, f.userID, false, store.SecretTypeEnvironment)
			return base(f)
		}, DenyCauseCeilingResourceMissing, "resource_missing"},
		{"parent is another user", func(t *testing.T, f *uspFixture) (AgentIdentity, Resource) {
			return f.principal(), userSecretResource(f.secretID, tid("usp-someone-else"))
		}, DenyCauseCeilingProvenanceStale, "resource_stale"},
		{"empty secret ID", func(t *testing.T, f *uspFixture) (AgentIdentity, Resource) {
			return f.principal(), userSecretResource("", f.userID)
		}, DenyCauseCeilingResourceMissing, "invalid_resource"},
		{"empty parent ID", func(t *testing.T, f *uspFixture) (AgentIdentity, Resource) {
			return f.principal(), userSecretResource(f.secretID, "")
		}, DenyCauseCeilingResourceMissing, "invalid_resource"},
		// T15
		{"principal not attested", func(t *testing.T, f *uspFixture) (AgentIdentity, Resource) {
			return NewFederatedAgentIdentity("https://remote.example", f.agentID, f.projectID, "fed", f.userID, []string{f.userID}, nil), f.resource()
		}, DenyCauseCeilingSourceNotAllowed, "not_attested"},
		// T20
		{"requesting agent deleted", func(t *testing.T, f *uspFixture) (AgentIdentity, Resource) {
			softDeleteAgentRow(t, f.base, f.agentID)
			return base(f)
		}, DenyCauseCeilingOrphaned, "agent_not_live"},
		{"requesting agent held", func(t *testing.T, f *uspFixture) (AgentIdentity, Resource) {
			holdAgent(t, f.base, f.agentID, f.projectID, f.userID)
			return base(f)
		}, DenyCauseCeilingOrphaned, "agent_not_live"},
		{"principal project differs", func(t *testing.T, f *uspFixture) (AgentIdentity, Resource) {
			return newFullAgentIdentity(f.agentID, tid("usp-other-proj"), []string{f.userID}, []AgentTokenScope{ScopeProjectSecretRead}), f.resource()
		}, DenyCauseCeilingOrphaned, "agent_project"},
		// T21, T24
		{"unknown delegator type", func(t *testing.T, f *uspFixture) (AgentIdentity, Resource) {
			f.st.edgeMutate = map[string]func(*store.DelegationEdge){f.agentID: func(e *store.DelegationEdge) {
				e.DelegatorType, e.SourcePrincipalKind = "group", "group"
			}}
			return base(f)
		}, DenyCauseCeilingOrphaned, "principal_mismatch"},
		{"source kind differs from delegator type", func(t *testing.T, f *uspFixture) (AgentIdentity, Resource) {
			f.replaceAgentEdge(t, f.agentID, func(e *store.DelegationEdge) { e.SourcePrincipalKind = store.DelegationPrincipalAgent })
			return base(f)
		}, DenyCauseCeilingOrphaned, "principal_mismatch"},
		{"dev_local edge with another principal", func(t *testing.T, f *uspFixture) (AgentIdentity, Resource) {
			f.replaceAgentEdge(t, f.agentID, func(e *store.DelegationEdge) { e.SourceCredentialKind = store.SourceCredentialDevLocal })
			return base(f)
		}, DenyCauseCeilingOrphaned, "principal_mismatch"},
		// T22
		{"out-of-lineage secret", func(t *testing.T, f *uspFixture) (AgentIdentity, Resource) {
			other := tid("usp-other-user")
			createDCUser(t, f.base, other, "usp-other@test.com", f.projectID, store.ProjectRoleOwner)
			sec := f.userSecret(t, other, "USP_OTHER_KEY", other, true, store.SecretTypeEnvironment)
			return f.principal(), userSecretResource(sec, other)
		}, DenyCauseCeilingResourceMissing, "resource_missing"},
		// T23
		{"terminal user loses project admission", func(t *testing.T, f *uspFixture) (AgentIdentity, Resource) {
			removeUserProjectBindings(t, f.base, f.userID, f.projectID)
			return base(f)
		}, DenyCauseCeilingDelegatorLacksPermission, "source_not_admitted"},
		// T25
		{"terminal user under an access constraint", func(t *testing.T, f *uspFixture) (AgentIdentity, Resource) {
			f.addUserConstraint(t, "user", f.userID, []string{"project.read", "project.secret_read"})
			return base(f)
		}, DenyCauseCeilingDelegatorLacksPermission, "source_restricted"},
		{"caged test-admin terminal user", func(t *testing.T, f *uspFixture) (AgentIdentity, Resource) {
			// The admin test tier is on and U reads as an admin-kind test
			// identity. Declared overlap: the test-identity clamp may also
			// withdraw U's project admission (S4a), with the same cause, so
			// the row accepts either step (cagedSteps below).
			// A fresh service, so no kind read from the base check is cached.
			f.st.userKind = map[string]string{f.userID: store.UserKindTestHubAdmin}
			f.a = NewAuthzService(f.st, slog.Default())
			setAuthzTestAdminTier(f.a, func(string) bool { return true }, time.Now)
			return base(f)
		}, DenyCauseCeilingDelegatorLacksPermission, ""},
		// T13
		{"author is not the owner", func(t *testing.T, f *uspFixture) (AgentIdentity, Resource) {
			other := tid("usp-author")
			createDCUser(t, f.base, other, "usp-author@test.com", f.projectID, store.ProjectRoleOwner)
			ag := f.agentChild(t, "author", []string{f.userID, other}, AgentRoleFull)
			f.recordEdge(t, store.DelegationPrincipalUser, f.userID, ag, store.EffectCeiling{Kind: store.EffectCeilingPrincipal}, nil)
			sec := f.userSecret(t, f.userID, "USP_AUTHOR_KEY", other, true, store.SecretTypeEnvironment)
			return f.identity(ag, []string{f.userID, other}), userSecretResource(sec, f.userID)
		}, DenyCauseCeilingSourceNotAllowed, "source_not_allowed"},
		// T14a: the terminal user is neither the author nor the head of the ancestry.
		{"terminal user is not the author", func(t *testing.T, f *uspFixture) (AgentIdentity, Resource) {
			other := tid("usp-owner-v")
			createDCUser(t, f.base, other, "usp-owner-v@test.com", f.projectID, store.ProjectRoleOwner)
			ag := f.agentChild(t, "owner-v", []string{other}, AgentRoleFull)
			f.recordEdge(t, store.DelegationPrincipalUser, f.userID, ag, store.EffectCeiling{Kind: store.EffectCeilingPrincipal}, nil)
			sec := f.userSecret(t, other, "USP_V_KEY", other, true, store.SecretTypeEnvironment)
			return f.identity(ag, []string{other}), userSecretResource(sec, other)
		}, DenyCauseCeilingSourceNotAllowed, "source_not_allowed"},
		// T14b: an agent-authored source (RQ-1b). The creating agent is in the
		// requesting agent's lineage, so S5 finds the row; S7 refuses it
		// because its recorded creator is not its owner.
		{"agent-authored source", func(t *testing.T, f *uspFixture) (AgentIdentity, Resource) {
			child := f.agentChild(t, "agent-author", []string{f.userID, f.agentID}, AgentRoleFull)
			f.recordEdge(t, store.DelegationPrincipalAgent, f.agentID, child, store.EffectCeiling{Kind: store.EffectCeilingPrincipal}, nil)
			sec := f.userSecret(t, f.userID, "USP_AGENT_AUTHOR_KEY", f.agentID, true, store.SecretTypeEnvironment)
			return f.identity(child, []string{f.userID, f.agentID}), userSecretResource(sec, f.userID)
		}, DenyCauseCeilingSourceNotAllowed, "source_not_allowed"},
		// T26: author = terminal user, but the stored ancestry's head is another user.
		{"ancestry head is not the terminal user", func(t *testing.T, f *uspFixture) (AgentIdentity, Resource) {
			other := tid("usp-head")
			createDCUser(t, f.base, other, "usp-head@test.com", f.projectID, store.ProjectRoleOwner)
			ag := f.agentChild(t, "head", []string{other, f.userID}, AgentRoleFull)
			f.recordEdge(t, store.DelegationPrincipalUser, f.userID, ag, store.EffectCeiling{Kind: store.EffectCeilingPrincipal}, nil)
			return f.identity(ag, []string{other, f.userID}), f.resource()
		}, DenyCauseCeilingSourceNotAllowed, "source_not_allowed"},
	}
	// S7 and S8 share a step name; the reason tells them apart.
	reasons := map[string]string{
		"author is not the owner":                "secret author is not its owner",
		"agent-authored source":                  "secret author is not its owner",
		"terminal user is not the author":        "recorded source is not the secret's author",
		"ancestry head is not the terminal user": "recorded source is not the secret's author",
	}
	// The caged row lands at S4c, or at S4a under the declared overlap.
	cagedSteps := []string{"secret_parent_ceiling_source_caged", "secret_parent_ceiling_source_not_admitted"}
	for _, r := range rows {
		t.Run(r.name, func(t *testing.T) {
			f := newUSPFixture(t, "sf-"+strings.ReplaceAll(r.name, " ", "-"))
			// Meta-check: the untouched base fixture allows.
			d0, steps0 := f.eval(t)
			require.True(t, d0.Allowed, "base fixture must allow: %q %+v", d0.Reason, steps0)
			p, res := r.mutate(t, f)
			d, steps := f.evalFor(t, p, res)
			assertUSPDeny(t, d, steps, r.cause, r.step)
			if want, ok := reasons[r.name]; ok {
				assert.Equal(t, want, d.Reason)
			}
			if r.name == "caged test-admin terminal user" {
				assert.True(t, hasDecisionStep(steps, cagedSteps[0]) || hasDecisionStep(steps, cagedSteps[1]),
					"want one of %v in %+v", cagedSteps, steps)
			}
		})
	}
}

// T8: the hop note is adoptable only for a version-0 unrecorded row; the
// sentinel fills none.
func TestUserSecretParentCeiling_UnrecordedHopNote(t *testing.T) {
	for _, tc := range []struct {
		name      string
		mutate    func(*store.DelegationEdge)
		cause     DenyCause
		adoptable bool
	}{
		{"version 0 unrecorded", func(e *store.DelegationEdge) {
			e.ProvenanceVersion = 0
			e.EffectCeiling = store.EffectCeiling{Kind: store.EffectCeilingUnrecorded}
		}, DenyCauseCeilingUnrecorded, true},
		{"version 2", func(e *store.DelegationEdge) { e.ProvenanceVersion = 2 }, DenyCauseCeilingUnrecorded, false},
		{"sentinel", func(e *store.DelegationEdge) {
			e.DelegatorID, e.SourcePrincipalID = migrationDelegatorID, migrationDelegatorID
			e.EffectCeiling = store.EffectCeiling{Kind: store.EffectCeilingUnrecorded}
			e.ProvenanceVersion = 0
		}, DenyCauseCeilingOrphaned, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newUSPFixture(t, "note-"+strings.ReplaceAll(tc.name, " ", "-"))
			f.replaceAgentEdge(t, f.agentID, tc.mutate)
			d, note := f.a.evaluateUserSecretParentCeiling(context.Background(), f.principal(), f.resource(), nil)
			require.False(t, d.Allowed)
			assert.Equal(t, tc.cause, d.DenyCause, "reason %q", d.Reason)
			assert.Equal(t, tc.adoptable, note.adoptable)
		})
	}
}

// T9: two active edges (the partial unique index forbids them in the store,
// so the extra edge is injected).
func TestUserSecretParentCeiling_DuplicateEdgesAmbiguous(t *testing.T) {
	f := newUSPFixture(t, "dup")
	dup := *f.edge
	dup.ID = "usp-dup-edge"
	a := NewAuthzService(&extraEdgeStore{Store: f.st, delegateID: f.agentID, extra: &dup}, slog.Default())
	var explain []DecisionStep
	d, _ := a.evaluateUserSecretParentCeiling(context.Background(), f.principal(), f.resource(), &explain)
	assertUSPDeny(t, d, explain, DenyCauseCeilingProvenanceAmbiguous, "duplicate_edges")
}

// T5 S5 >1: two progeny rows with the secret's ID.
func TestUserSecretParentCeiling_DuplicateSourceRowsAmbiguous(t *testing.T) {
	f := newUSPFixture(t, "duprow")
	rows, err := f.base.ListProgenySecrets(context.Background(), []string{f.userID})
	require.NoError(t, err)
	require.Len(t, rows, 1)
	f.st.extraProgeny = []store.Secret{rows[0]}
	d, steps := f.eval(t)
	assertUSPDeny(t, d, steps, DenyCauseCeilingProvenanceAmbiguous, "resource_ambiguous")
}

// T12: an internal-type row is not a usable source.
func TestUserSecretParentCeiling_InternalSecretMissing(t *testing.T) {
	f := newUSPFixture(t, "internal")
	rows, err := f.base.ListProgenySecrets(context.Background(), []string{f.userID})
	require.NoError(t, err)
	require.Len(t, rows, 1)
	internal := rows[0]
	internal.ID = "usp-internal-row"
	internal.SecretType = store.SecretTypeInternal
	f.st.extraProgeny = []store.Secret{internal}
	d, steps := f.evalFor(t, f.principal(), userSecretResource(internal.ID, f.userID))
	assertUSPDeny(t, d, steps, DenyCauseCeilingResourceMissing, "resource_missing")
}

// T10: a deleted terminal user. The user row cannot be removed while it is
// referenced, so the lookup reports not found.
func TestUserSecretParentCeiling_TerminalUserMissingOrphaned(t *testing.T) {
	f := newUSPFixture(t, "nouser")
	a := NewAuthzService(&userNotFoundStore{Store: f.st, id: f.userID}, slog.Default())
	var explain []DecisionStep
	d, _ := a.evaluateUserSecretParentCeiling(context.Background(), f.principal(), f.resource(), &explain)
	assertUSPDeny(t, d, explain, DenyCauseCeilingOrphaned, "source_not_live")
}

type userNotFoundStore struct {
	store.Store
	id string
}

func (s *userNotFoundStore) GetUser(ctx context.Context, id string) (*store.User, error) {
	if id == s.id {
		return nil, store.ErrNotFound
	}
	return s.Store.GetUser(ctx, id)
}

// T17: a cycle and an over-long chain.
func TestUserSecretParentCeiling_DepthAndCycle(t *testing.T) {
	t.Run("cycle", func(t *testing.T) {
		f := newUSPFixture(t, "cycle")
		other := f.agentChild(t, "cyc", []string{f.userID, f.agentID}, AgentRoleFull)
		f.replaceAgentEdge(t, f.agentID, func(e *store.DelegationEdge) {
			e.DelegatorType, e.DelegatorID = store.DelegationPrincipalAgent, other
			e.SourcePrincipalKind, e.SourcePrincipalID = store.DelegationPrincipalAgent, other
			e.SourceCredentialKind = store.SourceCredentialAgent
		})
		f.recordEdge(t, store.DelegationPrincipalAgent, f.agentID, other, store.EffectCeiling{Kind: store.EffectCeilingPrincipal}, nil)
		d, steps := f.eval(t)
		assertUSPDeny(t, d, steps, DenyCauseCeilingOrphaned, "cycle")
	})
	t.Run("depth", func(t *testing.T) {
		f := newUSPFixture(t, "depth")
		prev := f.agentID
		ancestry := []string{f.userID, f.agentID}
		for i := 0; i <= maxDelegationDepth; i++ {
			next := f.agentChild(t, "depth-"+string(rune('a'+i)), append([]string(nil), ancestry...), AgentRoleFull)
			f.recordEdge(t, store.DelegationPrincipalAgent, prev, next, store.EffectCeiling{Kind: store.EffectCeilingPrincipal}, nil)
			ancestry = append(ancestry, next)
			prev = next
		}
		d, steps := f.evalFor(t, f.identity(prev, ancestry[:len(ancestry)-1]), f.resource())
		assertUSPDeny(t, d, steps, DenyCauseCeilingOrphaned, "depth")
	})
}

// T16: every load fails closed with ceiling_error.
func TestUserSecretParentCeiling_LookupErrorsFailClosed(t *testing.T) {
	cases := []struct {
		name string
		set  func(f *uspFixture)
	}{
		{"edges", func(f *uspFixture) { f.st.failEdges = true }},
		{"agent", func(f *uspFixture) { f.st.failAgent = true }},
		{"hold", func(f *uspFixture) { f.st.failHold = true }},
		{"terminal user", func(f *uspFixture) { f.st.failUser = true; f.st.failUserID = f.userID }},
		{"project admission", func(f *uspFixture) { f.st.failBindings = true }},
		{"access constraints", func(f *uspFixture) { f.st.failConstraints = true }},
		{"terminal user groups", func(f *uspFixture) { f.st.failGroups = true }},
		{"progeny secrets", func(f *uspFixture) { f.st.failProgeny = true }},
		// A kind read error for a user who is not a test admin: the error is
		// checked before the caged flag.
		{"kind", func(f *uspFixture) { f.st.userKind = map[string]string{f.userID: "not-a-kind"} }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newUSPFixture(t, "err-"+strings.ReplaceAll(tc.name, " ", "-"))
			tc.set(f)
			d, steps := f.eval(t)
			require.False(t, d.Allowed, "a lookup error never allows")
			assert.Equal(t, DenyCauseCeilingError, d.DenyCause, "reason %q steps %+v", d.Reason, steps)
		})
	}
}

// T32: an invalid principal is a ceiling_error with no store call, and the
// evaluator reads no request memo.
func TestUserSecretParentCeiling_InvalidPrincipalAndMasking(t *testing.T) {
	f := newUSPFixture(t, "invalid")
	for _, tc := range []struct {
		name string
		p    AgentIdentity
	}{
		{"untyped nil", nil},
		{"typed nil", (*storedAgentIdentity)(nil)},
		{"empty ID", newFullAgentIdentity("", f.projectID, []string{f.userID}, nil)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			before := f.st.totalCalls()
			var d ParentCeilingDecision
			require.NotPanics(t, func() { d = f.a.EvaluateUserSecretParentCeiling(context.Background(), tc.p, f.resource()) })
			assert.False(t, d.Allowed)
			assert.Equal(t, DenyCauseCeilingError, d.DenyCause)
			assert.Equal(t, before, f.st.totalCalls(), "no store call for an invalid principal")
			chain := f.a.userSecretChainVerdict(context.Background(), tc.p, nil)
			assert.Equal(t, DenyCauseCeilingError, chain.decision.DenyCause)
			assert.Equal(t, before, f.st.totalCalls())
		})
	}

	t.Run("no memo read", func(t *testing.T) {
		ctx := withAuthzInputMemo(context.Background())
		c0, g0 := f.st.callCount("ListAccessConstraints"), f.st.callCount("GetEffectiveGroups")
		for i := 0; i < 2; i++ {
			d := f.a.EvaluateUserSecretParentCeiling(ctx, f.principal(), f.resource())
			require.True(t, d.Allowed, "reason %q", d.Reason)
		}
		assert.GreaterOrEqual(t, f.st.callCount("ListAccessConstraints")-c0, 2, "each evaluation loads the constraint table itself")
		assert.GreaterOrEqual(t, f.st.callCount("GetEffectiveGroups")-g0, 2, "each evaluation loads the source closure itself")
		a0 := f.st.callCount("GetAgent")
		for i := 0; i < 2; i++ {
			chain := f.a.userSecretChainVerdict(ctx, f.principal(), nil)
			require.True(t, chain.decision.Allowed)
		}
		assert.GreaterOrEqual(t, f.st.callCount("GetAgent")-a0, 2, "the chain verdict loads the agent itself")
	})
}

// T29: the evaluator's terminal user is resolveProvenanceChain's root user,
// and the evaluator denies whenever ResolveProvenanceRoot errors. T19: the
// chain verdict equals the fold predicate (a fold error counts as deny).
func TestUserSecretParentCeiling_ChainVerdictMatchesProvenanceAndFold(t *testing.T) {
	shapes := []struct {
		name  string
		build func(t *testing.T, f *uspFixture) (string, []string)
	}{
		{"principal", func(t *testing.T, f *uspFixture) (string, []string) { return f.agentID, []string{f.userID} }},
		{"bounded with secret.use", func(t *testing.T, f *uspFixture) (string, []string) {
			f.replaceAgentEdge(t, f.agentID, func(e *store.DelegationEdge) { e.EffectCeiling = boundedCeiling("secret.use") })
			return f.agentID, []string{f.userID}
		}},
		{"bounded without secret.use", func(t *testing.T, f *uspFixture) (string, []string) {
			f.replaceAgentEdge(t, f.agentID, func(e *store.DelegationEdge) { e.EffectCeiling = boundedCeiling("project.read") })
			return f.agentID, []string{f.userID}
		}},
		{"unrecorded", func(t *testing.T, f *uspFixture) (string, []string) {
			f.replaceAgentEdge(t, f.agentID, func(e *store.DelegationEdge) {
				e.ProvenanceVersion = 0
				e.EffectCeiling = store.EffectCeiling{Kind: store.EffectCeilingUnrecorded}
			})
			return f.agentID, []string{f.userID}
		}},
		{"agent-created mixed", func(t *testing.T, f *uspFixture) (string, []string) {
			child := f.agentChild(t, "fold-mixed", []string{f.userID, f.agentID}, AgentRoleFull)
			f.recordEdge(t, store.DelegationPrincipalAgent, f.agentID, child, boundedCeiling("secret.use", "project.read"), nil)
			return child, []string{f.userID, f.agentID}
		}},
		{"agent-created bounded without secret.use", func(t *testing.T, f *uspFixture) (string, []string) {
			child := f.agentChild(t, "fold-bounded", []string{f.userID, f.agentID}, AgentRoleFull)
			f.recordEdge(t, store.DelegationPrincipalAgent, f.agentID, child, boundedCeiling("project.read"), nil)
			return child, []string{f.userID, f.agentID}
		}},
		{"broken", func(t *testing.T, f *uspFixture) (string, []string) {
			child := f.agentChild(t, "fold-broken", []string{f.userID, f.agentID}, AgentRoleFull)
			f.recordEdge(t, store.DelegationPrincipalAgent, f.agentID, child, store.EffectCeiling{Kind: store.EffectCeilingPrincipal}, nil)
			f.deactivateEdges(t, f.agentID)
			return child, []string{f.userID, f.agentID}
		}},
	}
	for _, sh := range shapes {
		t.Run(sh.name, func(t *testing.T) {
			f := newUSPFixture(t, "fold-"+strings.ReplaceAll(sh.name, " ", "-"))
			ctx := context.Background()
			agentID, ancestry := sh.build(t, f)
			chain := f.a.userSecretChainVerdict(ctx, f.identity(agentID, ancestry), nil)

			ag, err := f.base.GetAgent(ctx, agentID)
			require.NoError(t, err)
			fold, foldErr := f.a.chainEffectCeiling(ctx, ag)
			foldAllows := foldErr == nil && fold.UnrecordedHops == 0 && fold.Ceiling.Kind != store.EffectCeilingUnrecorded &&
				EffectCeilingAllows(fold.Ceiling, "secret.use", false)
			assert.Equal(t, foldAllows, chain.decision.Allowed, "T19: chain verdict %q, fold err %v", chain.decision.Reason, foldErr)

			root, rootErr := f.a.resolveProvenanceChain(ctx, ag, false)
			if chain.decision.Allowed {
				require.NoError(t, rootErr)
				require.NotNil(t, root.RootUser)
				assert.Equal(t, root.RootUser.ID, chain.user.ID, "T29: the terminal user is the provenance root user")
			}
			if _, err := f.a.ResolveProvenanceRoot(ctx, agentID, ResolveProvenanceOptions{PermissionID: "secret.use"}); err != nil {
				assert.False(t, chain.decision.Allowed, "T29: the evaluator denies whenever the provenance root does not resolve (%v)", err)
			}
		})
	}
}

// ---- dispatch tier (T27, T27u, T18) ------------------------------------------

func (f *uspFixture) dispatch(t *testing.T, ident Identity, permissionID string, action Action, res Resource, agentIDArg string) (bool, string, DenyCause, []DecisionStep, error) {
	t.Helper()
	var explain []DecisionStep
	var cause DenyCause
	req := AuthzRequest{
		Principal:  principalContextForIdentity(ident),
		Credential: credentialContextForIdentity(ident),
		Resource:   res,
		Action:     action,
		Permission: permissionID,
	}
	ok, reason, err := f.srv.authzService.checkDelegationCeiling(context.Background(), req, permissionID, agentIDArg, &explain, &cause)
	return ok, reason, cause, explain, err
}

func TestUserSecretParentCeiling_Applicability(t *testing.T) {
	f := newUSPFixture(t, "applic")
	missing := "usp-no-such-secret"
	p0 := userSecretResource(missing, f.userID)
	jwt := f.principal()

	// P0: the arm runs; S5 finds no row.
	_, _, cause, steps, err := f.dispatch(t, jwt, "secret.use", ActionUse, p0, f.agentID)
	require.NoError(t, err)
	require.True(t, hasSecretParentStep(steps), "P0 must show the arm: %+v", steps)
	require.Equal(t, DenyCauseCeilingResourceMissing, cause)

	delivery := &hubDeliveryIdentity{agentID: f.agentID, projectID: f.projectID, ancestry: []string{f.userID}, originUserID: f.userID, boundAgentID: f.agentID}
	rows := []struct {
		name    string
		ident   Identity
		perm    string
		action  Action
		res     Resource
		generic bool // (ii) at least one generic step
	}{
		{"(a) parent type agent", jwt, "secret.use", ActionUse, Resource{Type: "secret", ID: missing, OwnerID: f.userID, ParentType: "agent", ParentID: f.userID}, true},
		{"(b) action read", jwt, "secret.use", ActionRead, p0, true},
		{"(c) secret.deliver", jwt, "secret.deliver", ActionUse, p0, true},
		{"(d) project.secret_read", jwt, permissionProjectSecretRead, ActionUse, p0, true},
		{"(e) env_var", jwt, "secret.use", ActionUse, Resource{Type: "env_var", ID: missing, OwnerID: f.userID, ParentType: "user", ParentID: f.userID}, true},
		{"(f) gcp_service_account.use", jwt, "gcp_service_account.use", ActionUse, p0, true},
		{"(g) hub delivery principal", delivery, "secret.use", ActionUse, p0, false},
		{"(a') project parent", jwt, "secret.use", ActionUse, Resource{Type: "secret", ID: missing, ParentType: "project", ParentID: f.projectID}, true},
		{"(c') secret.deliver deliver", jwt, "secret.deliver", ActionDeliver, p0, true},
		{"(d') check 7 shape", jwt, permissionProjectSecretRead, actionProjectSecretRead, Resource{Type: "project", ID: f.projectID}, true},
	}
	for _, r := range rows {
		t.Run(r.name, func(t *testing.T) {
			_, reason, cause, steps, _ := f.dispatch(t, r.ident, r.perm, r.action, r.res, f.agentID)
			assert.False(t, hasSecretParentStep(steps), "(i) the arm must not run: %+v", steps)
			if r.generic {
				assert.True(t, hasGenericCeilingStep(steps), "(ii) the generic walk runs: %+v", steps)
			}
			assert.NotEqual(t, DenyCauseCeilingResourceMissing, cause, "(iii) reason %q", reason)
			t.Logf("observed cause %q reason %q", cause, reason)
		})
	}

	t.Run("(h) agent ID differs from the principal", func(t *testing.T) {
		ok, _, _, _, err := f.dispatch(t, jwt, "secret.use", ActionUse, p0, tid("usp-someone"))
		assert.False(t, ok)
		assert.Error(t, err)
	})
	t.Run("(j) empty secret ID takes the arm", func(t *testing.T) {
		_, reason, cause, steps, err := f.dispatch(t, jwt, "secret.use", ActionUse, userSecretResource("", f.userID), f.agentID)
		require.NoError(t, err)
		assert.True(t, hasSecretParentStep(steps), "%+v", steps)
		assert.False(t, hasGenericCeilingStep(steps), "no fallback: %+v", steps)
		assert.Equal(t, DenyCauseCeilingResourceMissing, cause)
		assert.Contains(t, reason, "invalid resource")
		assert.True(t, hasDecisionStep(steps, "secret_parent_ceiling_invalid_resource"))
	})
	t.Run("(k) empty parent ID takes the arm", func(t *testing.T) {
		_, _, cause, steps, err := f.dispatch(t, jwt, "secret.use", ActionUse, userSecretResource(f.secretID, ""), f.agentID)
		require.NoError(t, err)
		assert.True(t, hasSecretParentStep(steps), "%+v", steps)
		assert.False(t, hasGenericCeilingStep(steps), "no fallback: %+v", steps)
		assert.Equal(t, DenyCauseCeilingResourceMissing, cause, "not provenance_stale")
	})
	t.Run("base shape allows through the arm", func(t *testing.T) {
		ok, reason, _, steps, err := f.dispatch(t, jwt, "secret.use", ActionUse, f.resource(), f.agentID)
		require.NoError(t, err)
		assert.True(t, ok, "reason %q steps %+v", reason, steps)
	})
}

// T27u: the predicate alone, one clause per row.
func TestUserSecretParentProofApplies_Clauses(t *testing.T) {
	jwt := newFullAgentIdentity("a1", "p1", []string{"u1"}, nil)
	pc := principalContextForIdentity(jwt)
	res := userSecretResource("s1", "u1")
	req := func(mut func(*AuthzRequest)) AuthzRequest {
		r := AuthzRequest{Principal: pc, Resource: res, Action: ActionUse, Permission: "secret.use"}
		if mut != nil {
			mut(&r)
		}
		return r
	}
	delivery := &hubDeliveryIdentity{agentID: "a1", projectID: "p1", ancestry: []string{"u1"}, originUserID: "u1", boundAgentID: "a1"}
	var typedNilDelivery *hubDeliveryIdentity
	cases := []struct {
		name string
		req  AuthzRequest
		perm string
		want bool
	}{
		{"P0", req(nil), "secret.use", true},
		{"(a) parent type agent", req(func(r *AuthzRequest) { r.Resource.ParentType = "agent" }), "secret.use", false},
		{"(b) action read", req(func(r *AuthzRequest) { r.Action = ActionRead }), "secret.use", false},
		{"(c) secret.deliver", req(func(r *AuthzRequest) { r.Permission = "secret.deliver" }), "secret.deliver", false},
		{"(d) project.secret_read", req(func(r *AuthzRequest) { r.Permission = permissionProjectSecretRead }), permissionProjectSecretRead, false},
		{"(e) env_var", req(func(r *AuthzRequest) { r.Resource.Type = "env_var" }), "secret.use", false},
		{"(f) gcp_service_account.use", req(func(r *AuthzRequest) { r.Permission = "gcp_service_account.use" }), "gcp_service_account.use", false},
		{"(g) hub delivery", req(func(r *AuthzRequest) {
			r.Principal = PrincipalContext{Kind: PrincipalKindAgent, ID: "a1", Identity: delivery}
		}), "secret.use", false},
		{"(g) typed nil hub delivery", req(func(r *AuthzRequest) {
			r.Principal = PrincipalContext{Kind: PrincipalKindAgent, ID: "a1", Identity: typedNilDelivery}
		}), "secret.use", false},
		{"(j) empty ID", req(func(r *AuthzRequest) { r.Resource.ID = "" }), "secret.use", true},
		{"(k) empty parent ID", req(func(r *AuthzRequest) { r.Resource.ParentID = "" }), "secret.use", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, userSecretParentProofApplies(tc.req, tc.perm))
		})
	}
}

// CR1: the predicate is the only applicability test. Its definition, one
// dispatch call in checkDelegationCeiling and this file are the only
// references in the package sources.
func TestUserSecretParentProofApplies_OnlyDispatchCallsIt(t *testing.T) {
	fset, files := parseHubProduction(t)
	_ = fset
	calls := 0
	defs := 0
	for _, file := range files {
		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok {
				continue
			}
			if fn.Name.Name == "userSecretParentProofApplies" && fn.Recv == nil {
				defs++
				continue
			}
			ast.Inspect(fn, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				if id, ok := call.Fun.(*ast.Ident); ok && id.Name == "userSecretParentProofApplies" {
					calls++
					assert.Equal(t, "checkDelegationCeiling", fn.Name.Name, "only the step-10 dispatch decides applicability")
				}
				return true
			})
		}
	}
	assert.Equal(t, 1, defs)
	assert.Equal(t, 1, calls)
}

// ---- Decide level (T6, T18, T28) ---------------------------------------------

func (f *uspFixture) decide(t *testing.T, ident AgentIdentity, res Resource) Decision {
	t.Helper()
	return f.srv.authzService.Decide(context.Background(), AuthzRequest{
		Principal:  principalContextForIdentity(ident),
		Credential: credentialContextForIdentity(ident),
		Resource:   res,
		Action:     ActionUse,
		Permission: "secret.use",
		Explain:    true,
	})
}

func TestUserSecretParentCeiling_DecideAllowsBaseShape(t *testing.T) {
	f := newUSPFixture(t, "decide-base")
	d := f.decide(t, f.principal(), f.resource())
	require.True(t, d.Allowed, "reason %q", d.Reason)
	r := relationshipResult(t, d, RelationshipRuleProgeny)
	assert.True(t, r.Accepted)
}

// T28: deny points. Step-10 rows carry DeniedBy delegation_ceiling and the
// evaluator's cause; rows that deny earlier do not.
func TestUserSecretParentCeiling_DecideDenyPoints(t *testing.T) {
	type row struct {
		name   string
		mutate func(t *testing.T, f *uspFixture) (AgentIdentity, Resource)
		step10 bool
		cause  DenyCause
	}
	base := func(f *uspFixture) (AgentIdentity, Resource) { return f.principal(), f.resource() }
	rows := []row{
		{"D1b own edge missing", func(t *testing.T, f *uspFixture) (AgentIdentity, Resource) {
			f.deactivateEdges(t, f.agentID)
			return base(f)
		}, false, ""},
		{"D10 sentinel", func(t *testing.T, f *uspFixture) (AgentIdentity, Resource) {
			f.replaceAgentEdge(t, f.agentID, func(e *store.DelegationEdge) {
				e.DelegatorID, e.SourcePrincipalID = migrationDelegatorID, migrationDelegatorID
			})
			return base(f)
		}, false, ""},
		{"D10 principal typing", func(t *testing.T, f *uspFixture) (AgentIdentity, Resource) {
			f.replaceAgentEdge(t, f.agentID, func(e *store.DelegationEdge) { e.SourcePrincipalKind = store.DelegationPrincipalAgent })
			return base(f)
		}, false, ""},
		{"D2 version 0", func(t *testing.T, f *uspFixture) (AgentIdentity, Resource) {
			f.replaceAgentEdge(t, f.agentID, func(e *store.DelegationEdge) { e.ProvenanceVersion = 0 })
			return base(f)
		}, true, DenyCauseCeilingUnrecorded},
		{"D2 unknown version", func(t *testing.T, f *uspFixture) (AgentIdentity, Resource) {
			f.replaceAgentEdge(t, f.agentID, func(e *store.DelegationEdge) { e.ProvenanceVersion = 2 })
			return base(f)
		}, true, DenyCauseCeilingUnrecorded},
		{"D2 kind unrecorded, known version", func(t *testing.T, f *uspFixture) (AgentIdentity, Resource) {
			f.replaceAgentEdge(t, f.agentID, func(e *store.DelegationEdge) {
				e.EffectCeiling = store.EffectCeiling{Kind: store.EffectCeilingUnrecorded}
			})
			// Precondition: the fixture keeps the source kind equal to the delegator type.
			edges, err := f.base.GetDelegationEdgesForDelegate(context.Background(), store.DelegationPrincipalAgent, f.agentID)
			require.NoError(t, err)
			for _, e := range edges {
				if e.Active {
					require.Equal(t, e.DelegatorType, e.SourcePrincipalKind)
					require.Equal(t, store.SourceCredentialSession, e.SourceCredentialKind)
				}
			}
			return base(f)
		}, true, DenyCauseCeilingUnrecorded},
		{"bounded without secret.use", func(t *testing.T, f *uspFixture) (AgentIdentity, Resource) {
			f.replaceAgentEdge(t, f.agentID, func(e *store.DelegationEdge) { e.EffectCeiling = boundedCeiling("project.read") })
			return base(f)
		}, true, DenyCauseCeilingEffectExceeded},
		{"D3 intermediate agent held", func(t *testing.T, f *uspFixture) (AgentIdentity, Resource) {
			child := f.agentChild(t, "d-held", []string{f.userID, f.agentID}, AgentRoleFull)
			f.recordEdge(t, store.DelegationPrincipalAgent, f.agentID, child, store.EffectCeiling{Kind: store.EffectCeilingPrincipal}, nil)
			holdAgent(t, f.base, f.agentID, f.projectID, f.userID)
			return f.identity(child, []string{f.userID, f.agentID}), f.resource()
		}, true, DenyCauseCeilingOrphaned},
		{"D3 intermediate agent deleted", func(t *testing.T, f *uspFixture) (AgentIdentity, Resource) {
			child := f.agentChild(t, "d-del", []string{f.userID, f.agentID}, AgentRoleFull)
			f.recordEdge(t, store.DelegationPrincipalAgent, f.agentID, child, store.EffectCeiling{Kind: store.EffectCeilingPrincipal}, nil)
			softDeleteAgentRow(t, f.base, f.agentID)
			return f.identity(child, []string{f.userID, f.agentID}), f.resource()
		}, false, ""},
		{"D4 intermediate role lacks the scope", func(t *testing.T, f *uspFixture) (AgentIdentity, Resource) {
			child := f.agentChild(t, "d-role", []string{f.userID, f.agentID}, AgentRoleFull)
			f.recordEdge(t, store.DelegationPrincipalAgent, f.agentID, child, store.EffectCeiling{Kind: store.EffectCeilingPrincipal}, nil)
			setAgentRole(t, f.base, f.agentID, AgentRoleNone)
			return f.identity(child, []string{f.userID, f.agentID}), f.resource()
		}, true, DenyCauseCeilingDelegatorLacksPermission},
		{"D5 edge source differs from the ancestry root", func(t *testing.T, f *uspFixture) (AgentIdentity, Resource) {
			// The agent's one recorded edge names another admitted user V
			// while its stored ancestry and the secret's creator are U.
			// Stage 2b admits V; step 10 refuses at S8.
			other := tid("usp-d5-root")
			createDCUser(t, f.base, other, "usp-d5-root@test.com", f.projectID, store.ProjectRoleOwner)
			f.replaceAgentEdge(t, f.agentID, func(e *store.DelegationEdge) {
				e.DelegatorID, e.SourcePrincipalID = other, other
			})
			return base(f)
		}, true, DenyCauseCeilingSourceNotAllowed},
		{"D7 dev_local with dev auth off", func(t *testing.T, f *uspFixture) (AgentIdentity, Resource) {
			createDCUser(t, f.base, DevUserID, "dev@localhost", f.projectID, store.ProjectRoleOwner)
			f.srv.authzService.setDevLocalAuthorityEnabled(false)
			dev := f.agentChild(t, "d-dev", []string{DevUserID}, AgentRoleFull)
			f.recordEdge(t, store.DelegationPrincipalUser, DevUserID, dev, store.EffectCeiling{Kind: store.EffectCeilingPrincipal}, func(e *store.DelegationEdge) {
				e.SourceCredentialKind = store.SourceCredentialDevLocal
			})
			sec := f.userSecret(t, DevUserID, "USP_D7_KEY", DevUserID, true, store.SecretTypeEnvironment)
			return f.identity(dev, []string{DevUserID}), userSecretResource(sec, DevUserID)
		}, true, DenyCauseCeilingSourceNotAllowed},
		{"D9 terminal user under an access constraint", func(t *testing.T, f *uspFixture) (AgentIdentity, Resource) {
			f.addUserConstraint(t, "user", f.userID, []string{"project.read", "project.secret_read"})
			return base(f)
		}, true, DenyCauseCeilingDelegatorLacksPermission},
		{"D8 agent under an access constraint", func(t *testing.T, f *uspFixture) (AgentIdentity, Resource) {
			f.addUserConstraint(t, "agent", f.agentID, []string{"project.read", "project.secret_read"})
			return base(f)
		}, false, ""},
	}
	for _, r := range rows {
		t.Run(r.name, func(t *testing.T) {
			f := newUSPFixture(t, "dp-"+strings.ReplaceAll(r.name, " ", "-"))
			require.True(t, f.decide(t, f.principal(), f.resource()).Allowed, "base allows")
			p, res := r.mutate(t, f)
			d := f.decide(t, p, res)
			require.False(t, d.Allowed, "reason %q", d.Reason)
			if r.step10 {
				assert.Equal(t, DeniedByDelegationCeiling, d.DeniedBy, "step 10 ran; reason %q", d.Reason)
				assert.Equal(t, r.cause, d.DenyCause, "reason %q", d.Reason)
			} else {
				assert.NotEqual(t, DeniedByDelegationCeiling, d.DeniedBy, "denied before step 10; reason %q", d.Reason)
			}
		})
	}
}

// T28 D1: on a server where the delegation-edge backfill never ran, an
// agent with no edge is refused before step 10: step 9 stage 2b finds no
// authoritative source user. The evaluator alone reports S3
// provenance_missing, which is in the unrecorded set.
func TestUserSecretParentCeiling_DecidePreBackfillNoEdgeDeniesBeforeStep10(t *testing.T) {
	f := newUSPFixture(t, "dp-d1")
	ctx := context.Background()
	require.NoError(t, f.base.DeleteHubSetting(ctx, "migration_delegation_edge_backfill_v1"))
	f.deactivateEdges(t, f.agentID)
	require.False(t, f.srv.authzService.backfillCompleted(ctx), "the server is pre-backfill")

	d := f.decide(t, f.principal(), f.resource())
	require.False(t, d.Allowed, "reason %q", d.Reason)
	assert.NotEqual(t, DeniedByDelegationCeiling, d.DeniedBy, "denied before step 10; reason %q", d.Reason)

	chain := f.a.userSecretChainVerdict(ctx, f.principal(), nil)
	require.False(t, chain.decision.Allowed)
	assert.Equal(t, DenyCauseCeilingProvenanceMissing, chain.decision.DenyCause, "reason %q", chain.decision.Reason)
	assert.True(t, chain.unrecorded)
}

// T28 D6: a user-scope secret in the root's scope whose recorded creator is
// an agent of the chain (RQ-1b). With a plain-user root, step 9 stage 4
// refuses it (the creating agent's chain does not hold secret.use), so step
// 10 is not reached. With a super-admin root stage 4 passes, and step 10
// refuses it at S7: this is the Decide-level case that reaches S7. In both
// cases the same chain reads a secret the root created (control).
func TestUserSecretParentCeiling_DecideAgentAuthoredSource(t *testing.T) {
	t.Run("plain-user root denies at stage 4", func(t *testing.T) {
		f := newUSPFixture(t, "dp-d6-plain")
		child := f.agentChild(t, "d6-plain", []string{f.userID, f.agentID}, AgentRoleFull)
		f.recordEdge(t, store.DelegationPrincipalAgent, f.agentID, child, store.EffectCeiling{Kind: store.EffectCeilingPrincipal}, nil)
		sec := f.userSecret(t, f.userID, "USP_D6_PLAIN_KEY", f.agentID, true, store.SecretTypeEnvironment)
		ident := f.identity(child, []string{f.userID, f.agentID})

		ctl := f.decide(t, ident, f.resource())
		require.True(t, ctl.Allowed, "control: the chain reads the root's own secret; reason %q", ctl.Reason)

		d := f.decide(t, ident, userSecretResource(sec, f.userID))
		require.False(t, d.Allowed, "reason %q", d.Reason)
		assert.NotEqual(t, DeniedByDelegationCeiling, d.DeniedBy, "denied before step 10; reason %q", d.Reason)
		r := relationshipResult(t, d, RelationshipRuleProgeny)
		assert.False(t, r.Accepted)
		assert.Equal(t, RelationshipRejectSourceInactive, r.RejectedBy, "stage 4; detail %q", r.Detail)

		// The evaluator alone refuses the same request at S7.
		e, steps := f.evalFor(t, ident, userSecretResource(sec, f.userID))
		assertUSPDeny(t, e, steps, DenyCauseCeilingSourceNotAllowed, "source_not_allowed")
		assert.Equal(t, "secret author is not its owner", e.Reason)
	})
	t.Run("super-admin root reaches step 10 and denies at S7", func(t *testing.T) {
		f := newUSPFixture(t, "dp-d6-admin")
		admin := tid("usp-d6-admin")
		createTestUserWithRole(t, f.base, admin, "usp-d6-admin@test.com", "member", store.SystemRoleSuperAdmin)
		createDCUser(t, f.base, admin, "usp-d6-admin@test.com", f.projectID, store.ProjectRoleOwner)
		parent := f.agentChild(t, "d6-admin-parent", []string{admin}, AgentRoleFull)
		f.recordEdge(t, store.DelegationPrincipalUser, admin, parent, store.EffectCeiling{Kind: store.EffectCeilingPrincipal}, nil)
		child := f.agentChild(t, "d6-admin-child", []string{admin, parent}, AgentRoleFull)
		f.recordEdge(t, store.DelegationPrincipalAgent, parent, child, store.EffectCeiling{Kind: store.EffectCeilingPrincipal}, nil)
		own := f.userSecret(t, admin, "USP_D6_ADMIN_OWN_KEY", admin, true, store.SecretTypeEnvironment)
		sec := f.userSecret(t, admin, "USP_D6_ADMIN_KEY", parent, true, store.SecretTypeEnvironment)
		ident := f.identity(child, []string{admin, parent})

		ctl := f.decide(t, ident, userSecretResource(own, admin))
		require.True(t, ctl.Allowed, "control: the chain reads the root's own secret; reason %q", ctl.Reason)

		d := f.decide(t, ident, userSecretResource(sec, admin))
		require.False(t, d.Allowed, "reason %q", d.Reason)
		assert.Equal(t, DeniedByDelegationCeiling, d.DeniedBy, "step 10 ran; reason %q", d.Reason)
		assert.Equal(t, DenyCauseCeilingSourceNotAllowed, d.DenyCause, "S7; reason %q", d.Reason)
	})
}

// T18: a hub_delivery principal with secret.use on a user-scope secret
// never reaches step 10 through Decide. Step 0b limits the delivery
// credential to the deliver permissions and refuses the request before any
// relationship or ceiling evaluation, so the Decide-level form of T18
// ("takes its own arm at step 10") cannot be shown. What can be shown is
// pinned here: the Decide refusal is step 0b's, and checkDelegationCeiling
// called directly routes the principal to the hub_delivery arm, never to
// the user-scope secret arm. T27 row (g) and T27u cover the arm order and
// the predicate.
func TestUserSecretParentCeiling_HubDeliveryStopsBeforeStep10(t *testing.T) {
	f := newUSPFixture(t, "hd-t18")
	withDeliveryCredentialKinds(t, CredentialKindHubDelivery)
	// A literal of the shape the constructor builds for this agent (only
	// the hub_delivery test files call the constructor,
	// TestHubDelivery_ConstructorCallSites). Step 0b and the arm's
	// permission check refuse before the stored-agent evidence is read.
	h := &hubDeliveryIdentity{agentID: f.agentID, projectID: f.projectID, ancestry: []string{f.userID}, originUserID: f.userID, boundAgentID: f.agentID}

	d := decidePerm(f.srv.authzService, h, f.resource(), ActionUse, "secret.use", true)
	assertStep0bDenied(t, d, "delivery credential is limited to deliver permissions", "secret.use")
	assert.NotEqual(t, DeniedByDelegationCeiling, d.DeniedBy)

	ok, reason, cause, steps, err := f.dispatch(t, h, "secret.use", ActionUse, f.resource(), f.agentID)
	require.NoError(t, err)
	assert.False(t, ok)
	assert.Equal(t, "delivery credential is limited to deliver permissions", reason)
	assert.Equal(t, DenyCause(""), cause)
	assert.True(t, hasDecisionStep(steps, "delegation_ceiling_delivery_denied"), "the hub_delivery arm: %+v", steps)
	assert.False(t, hasSecretParentStep(steps), "never the user-scope secret arm: %+v", steps)
}

// T13b: S7 relies on every user-scope secret writer recording the scope
// owner as the creator (CreatedBy == ScopeID). The writers are pinned by
// behaviour in TestUserSecretWriters_RecordScopeOwnerAsCreator. This test
// keeps that list complete: every secret.SetSecretInput literal in the
// pkg/hub production sources either names a fixed non-user scope or sits in
// a function reviewed here. The other modules that write secrets
// (cmd/hub_secret_migrate.go, pkg/secretmigration) copy an existing row's
// CreatedBy unchanged, as SecretMigrationExecutor.Run does.
func TestUserSecretWriters_Inventory(t *testing.T) {
	reviewed := map[string]string{
		"setEnvVar":               "user scope: scope ID and creator are both the calling user (resolveEnvSecretAccess)",
		"setSecret":               "user scope: scope ID and creator are both the calling user (resolveEnvSecretAccess)",
		"handleAgentSecrets":      "user scope: the creator is set to the scope ID (the agent's origin user)",
		"handleScopedEnvVarByKey": "called with the project and runtime broker scopes only",
		"handleScopedSecretByKey": "called with the project and runtime broker scopes only",
		"Run":                     "SecretMigrationExecutor.Run copies an existing row's scope and CreatedBy",
	}
	fixedScope := func(e ast.Expr) bool {
		sel, ok := e.(*ast.SelectorExpr)
		if !ok {
			return false
		}
		switch sel.Sel.Name {
		case "ScopeHub", "ScopeProject", "ScopeRuntimeBroker":
			return true
		}
		return false
	}
	fset, files := parseHubProduction(t)
	found := map[string]bool{}
	for _, file := range files {
		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok {
				continue
			}
			ast.Inspect(fn, func(n ast.Node) bool {
				lit, ok := n.(*ast.CompositeLit)
				if !ok {
					return true
				}
				sel, ok := lit.Type.(*ast.SelectorExpr)
				if !ok || sel.Sel.Name != "SetSecretInput" {
					return true
				}
				var scope ast.Expr
				for _, elt := range lit.Elts {
					if kv, ok := elt.(*ast.KeyValueExpr); ok {
						if k, ok := kv.Key.(*ast.Ident); ok && k.Name == "Scope" {
							scope = kv.Value
						}
					}
				}
				if scope != nil && fixedScope(scope) {
					return true
				}
				found[fn.Name.Name] = true
				_, ok = reviewed[fn.Name.Name]
				assert.True(t, ok, "%s: %s writes a secret whose scope is not a fixed non-user scope; review its CreatedBy and add it here and to TestUserSecretWriters_RecordScopeOwnerAsCreator",
					fset.Position(lit.Pos()), fn.Name.Name)
				return true
			})
		}
	}
	for _, name := range []string{"setEnvVar", "setSecret", "handleAgentSecrets"} {
		assert.True(t, found[name], "the user-scope writer %s is still found", name)
	}
}

// T6: secret.use without a user parent keeps the generic walk.
func TestUserSecretParentCeiling_GenericWalkForOtherShapes(t *testing.T) {
	f := newUSPFixture(t, "generic")
	d := f.decide(t, f.principal(), Resource{Type: "secret", ID: f.secretID})
	require.False(t, d.Allowed)
	assert.Equal(t, DeniedByDelegationCeiling, d.DeniedBy, "reason %q", d.Reason)
	assert.Contains(t, d.Reason, "does not hold secret.use", "the generic delegator check, not the parent ceiling")
}

// ---- helpers -----------------------------------------------------------------

func holdAgent(t *testing.T, s store.Store, agentID, projectID, rootUserID string) {
	t.Helper()
	_, err := s.CreateAgentHolds(context.Background(), []*store.AgentHold{{
		AgentID: agentID, ProjectID: projectID, Cause: store.AgentHoldCauseOwnerAccessEnded,
		RootPrincipalType: store.AgentHoldRootUser, RootPrincipalID: rootUserID,
		Trigger: store.MembershipLossTriggerMemberRemove, ActorKind: "system", ActorID: "hub", CorrelationID: "usp-test",
	}})
	require.NoError(t, err)
}

func setAgentRole(t *testing.T, s store.Store, agentID string, role AgentRole) {
	t.Helper()
	ag, err := s.GetAgent(context.Background(), agentID)
	require.NoError(t, err)
	if ag.AppliedConfig == nil {
		ag.AppliedConfig = &store.AgentAppliedConfig{}
	}
	ag.AppliedConfig.AgentRole = string(role)
	require.NoError(t, s.UpdateAgent(context.Background(), ag))
}

func removeUserProjectBindings(t *testing.T, s store.Store, userID, projectID string) {
	t.Helper()
	ctx := context.Background()
	bindings, err := s.ListRoleBindingsForPrincipals(ctx, []store.PrincipalRef{{Type: store.RoleBindingPrincipalUser, ID: userID}}, nil, nil)
	require.NoError(t, err)
	n := 0
	for _, b := range bindings {
		if b.ScopeType == store.RoleScopeProject && b.ScopeID == projectID {
			require.NoError(t, s.DeleteRoleBinding(ctx, b.ID))
			n++
		}
	}
	require.Positive(t, n, "the fixture user had a project binding")
}
