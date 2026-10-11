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
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"log/slog"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/hub/permissions"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/go-jose/go-jose/v4/jwt"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// projectSA builds a project-scoped service account resource in the given
// project. Project-scoped is what gives it a project parent, which is what the
// baseline keys on.
func projectSA(t *testing.T, id, projectID string) Resource {
	t.Helper()
	r := gcpServiceAccountResource(&store.GCPServiceAccount{
		ID:      id,
		Scope:   store.ScopeProject,
		ScopeID: projectID,
		Email:   id + "@example.iam.gserviceaccount.com",
	})
	require.Equal(t, projectID, projectIDForResource(r),
		"fixture must carry a project parent for the test to mean anything")
	return r
}

func newAgentBaselineFixture(t *testing.T) *agentBaselineFixture {
	t.Helper()
	authz, s := authzTestSetup(t)
	ctx := context.Background()

	own := &store.Project{
		ID: tid("baseline-project-own"), Name: "Own Project", Slug: "baseline-own",
	}
	other := &store.Project{
		ID: tid("baseline-project-other"), Name: "Other Project", Slug: "baseline-other",
	}
	require.NoError(t, s.CreateProject(ctx, own))
	require.NoError(t, s.CreateProject(ctx, other))

	// The implicit project_agents group. Created by createProjectGroup in
	// production; the agent is a member of it by virtue of its project ID, with
	// no membership row.
	agentsGroup := &store.Group{
		ID:        api.NewUUID(),
		Name:      "Own Project Agents",
		Slug:      "project:baseline-own:agents",
		GroupType: store.GroupTypeProjectAgents,
		ProjectID: own.ID,
	}
	require.NoError(t, s.CreateGroup(ctx, agentsGroup))

	agent := &store.Agent{
		ID: tid("baseline-agent"), Slug: tid("baseline-agent"), Name: "Baseline Agent",
		ProjectID: own.ID, Phase: string(state.PhaseRunning),
	}
	require.NoError(t, s.CreateAgent(ctx, agent))

	// CO1: Create a project-scoped role definition and binding that grants the
	// agent read+list on agents and projects. This replaces the old implicit
	// agent project read baseline with an explicit role binding.
	readRoleDef := createTestRoleDefinition(t, s, "agent-project-read-baseline",
		store.RoleScopeProject, []string{
			"agent.read", "agent.list",
			"project.read", "project.list",
		})
	_, err := s.CreateRoleBinding(ctx, &store.RoleBinding{
		RoleDefinitionID: readRoleDef.ID,
		PrincipalType:    store.RoleBindingPrincipalAgent,
		PrincipalID:      agent.ID,
		ScopeType:        store.RoleScopeProject,
		ScopeID:          own.ID,
		CreatedBy:        "test",
	})
	require.NoError(t, err)

	return &agentBaselineFixture{
		authz:        authz,
		store:        s,
		ownProject:   own,
		otherProject: other,
		agent:        agent,
		agentsGroup:  agentsGroup,
		readRoleDef:  readRoleDef,
		// CO1: Agent identity carries baseline scopes. The scopes include
		// project:read which maps to project.read in the permissions registry.
		// The agent scope restriction only allows permissions that map to
		// declared scopes, so only registry-mapped permissions pass through.
		identity: &agentIdentityWrapper{&AgentTokenClaims{
			Claims:    jwt.Claims{Subject: agent.ID},
			ProjectID: own.ID,
			Scopes:    ScopesForRole(AgentRoleBaseline),
		}},
	}
}

func newBearerFixture(t *testing.T, name string) bearerFixture {
	t.Helper()
	srv, s := testServer(t)
	f := bearerFixture{
		srv:      srv,
		store:    s,
		projectA: tid("bearer-" + name + "-project-a"),
		projectB: tid("bearer-" + name + "-project-b"),
		ownerA:   tid("bearer-" + name + "-owner-a"),
		ownerB:   tid("bearer-" + name + "-owner-b"),
	}
	createRS1Project(t, s, f.projectA, f.ownerA)
	createRS1Project(t, s, f.projectB, f.ownerB)
	f.agentA = uatpAgent(t, s, f.projectA, f.ownerA, name+"-a", f.ownerA)
	f.agentB = uatpAgent(t, s, f.projectB, f.ownerB, name+"-b", f.ownerB)
	return f
}

func bearerUser(id string) *AuthenticatedUser {
	return NewAuthenticatedUser(id, id+"@test.com", "User", "member", "api")
}

func bearerCeiling(t *testing.T, selectors ...string) permissions.FrozenPermissionCeiling {
	t.Helper()
	ceiling, ok := permissions.BuildCeilingFromSelectors(selectors)
	require.True(t, ok, "selectors must resolve: %v", selectors)
	return ceiling
}

func hubBoundary() TokenBoundary { return TokenBoundary{Kind: BoundaryKindHub} }

func projectBoundary(projectID string) TokenBoundary {
	return TokenBoundary{Kind: BoundaryKindProject, ProjectID: projectID}
}

func activeUserPrincipal(id string) PrincipalContext {
	return PrincipalContext{Kind: PrincipalKindUser, ID: id}
}

// systemRoleUserWithPermissions creates a user with a custom system-scoped
// role definition carrying exactly permissionIDs, and returns the user ID.
func systemRoleUserWithPermissions(t *testing.T, s store.Store, userID string, permissionIDs []string) {
	t.Helper()
	ctx := context.Background()
	require.NoError(t, s.CreateUser(ctx, &store.User{ID: userID, Email: userID + "@test.com", DisplayName: "u", Role: "member", Status: store.UserStatusActive}))
	rd, err := s.CreateRoleDefinition(ctx, &store.RoleDefinition{
		Name:        "apa-custom-" + userID,
		ScopeType:   store.RoleScopeSystem,
		Permissions: permissionIDs,
	})
	require.NoError(t, err)
	_, err = s.CreateRoleBinding(ctx, &store.RoleBinding{
		RoleDefinitionID: rd.ID,
		PrincipalType:    store.RoleBindingPrincipalUser,
		PrincipalID:      userID,
		ScopeType:        store.RoleScopeSystem,
		CreatedBy:        "test",
	})
	require.NoError(t, err)
}

// bypassAgentsFixture is the world these tests reason about, mirroring the
// narrated scenario in design §5.3:
//
//	project (P1), owned by owner
//	  caller   — the agent presenting the token in most tests
//	  sibling  — a project peer of caller; NOT a descendant of it
//	  child    — a descendant of caller (caller.ID in its ancestry)
//	other (P2)
//	  stranger — an agent in a project the caller has nothing to do with
//
// The sibling/child distinction is load-bearing: the ancestry bypass grants
// caller full access to child, so a denial test written against child would
// pass for the wrong reason.
type bypassAgentsFixture struct {
	srv      *Server
	store    store.Store
	owner    *store.User
	proj     *store.Project
	other    *store.Project
	caller   *store.Agent
	sibling  *store.Agent
	child    *store.Agent
	stranger *store.Agent
	broker   *store.RuntimeBroker
	// brokerSecret is the HMAC key for broker-authenticated requests.
	brokerSecret []byte
	brokerAuthID string
}

// bypassAgentsServer builds a server that accepts all three identity kinds:
// the dev user token, agent JWTs, and HMAC-signed broker requests. The stock
// testServer has no broker auth, and the #591 bypass admitted brokers as well
// as agents, so the tests need a server where a broker caller can actually
// reach a handler.
func bypassAgentsServer(t *testing.T) (*Server, store.Store) {
	t.Helper()
	s, err := newTestStore(t, ":memory:")
	if err != nil {
		t.Skipf("skipping: test store unavailable (%v)", err)
	}
	// Remove backfill marker — see testServer comment for rationale.
	_ = s.DeleteHubSetting(context.Background(), "migration_delegation_edge_backfill_v1")

	cfg := DefaultServerConfig()
	cfg.DevAuthToken = testDevToken
	cfg.DevUserConfig = DevUserConfig{
		Username:    "dev",
		DisplayName: "Development User",
		Email:       "dev@localhost",
	}
	cfg.BrokerAuthConfig = DefaultBrokerAuthConfig()
	srv, err := newTestHubServer(t, cfg, s)
	require.NoError(t, err)
	srv.SetHubID("test-hub-id")
	waitUserScopedDataSweep(t, srv)
	return srv, s
}

// bindFixtureOwner gives f.owner the project-owner binding on f.proj, for
// tests whose intent is project-owner access. Project.OwnerID alone grants
// nothing (ptone/scion#2586). It is not part of bypassAgentsSetup because
// many tests bind f.owner to a narrower project role themselves, and a
// principal holds at most one built-in membership per project.
func bindFixtureOwner(t *testing.T, f *bypassAgentsFixture) {
	t.Helper()
	require.NoError(t, f.srv.createProjectOwnerRoleBinding(context.Background(), f.proj.ID, f.owner.ID))
}

func bypassAgentsSetup(t *testing.T) *bypassAgentsFixture {
	t.Helper()
	srv, s := bypassAgentsServer(t)
	ctx := context.Background()
	f := &bypassAgentsFixture{srv: srv, store: s}

	f.owner = &store.User{
		ID:          tid("bypass-owner"),
		Email:       "bypass-owner@example.com",
		DisplayName: "Bypass Owner",
		Role:        store.UserRoleMember,
		Status:      "active",
		Created:     time.Now(),
	}
	require.NoError(t, s.CreateUser(ctx, f.owner))

	f.proj = &store.Project{
		ID:      tid("bypass-p1"),
		Name:    "Bypass P1",
		Slug:    "bypass-p1",
		OwnerID: f.owner.ID,
	}
	require.NoError(t, s.CreateProject(ctx, f.proj))

	f.other = &store.Project{
		ID:      tid("bypass-p2"),
		Name:    "Bypass P2",
		Slug:    "bypass-p2",
		OwnerID: f.owner.ID,
	}
	require.NoError(t, s.CreateProject(ctx, f.other))
	// The owner relationship on a project agent requires active project
	// access (ptone/scion#2141); the binding grants no permissions itself.
	grantProjectAccessOnly(t, s, f.owner.ID, f.proj.ID)
	grantProjectAccessOnly(t, s, f.owner.ID, f.other.ID)

	// An auto-provide broker, so that agent creation can resolve a broker and
	// the create tests exercise the authorization gate rather than dying at
	// broker selection.
	f.brokerSecret = []byte("bypass-secret-key-32-bytes-ok!!")
	f.brokerAuthID = uuid.New().String()
	f.broker = &store.RuntimeBroker{
		ID:          f.brokerAuthID,
		Name:        "bypass-broker",
		Slug:        "bypass-broker",
		Status:      store.BrokerStatusOnline,
		AutoProvide: true,
		Created:     time.Now(),
		Updated:     time.Now(),
	}
	require.NoError(t, s.CreateRuntimeBroker(ctx, f.broker))
	require.NoError(t, s.CreateBrokerSecret(ctx, &store.BrokerSecret{
		BrokerID:  f.broker.ID,
		SecretKey: f.brokerSecret,
		Algorithm: store.BrokerSecretAlgorithmHMACSHA256,
		Status:    store.BrokerSecretStatusActive,
	}))
	for _, p := range []*store.Project{f.proj, f.other} {
		require.NoError(t, s.AddProjectProvider(ctx, &store.ProjectProvider{
			ProjectID:  p.ID,
			BrokerID:   f.broker.ID,
			BrokerName: f.broker.Name,
			Status:     store.BrokerStatusOnline,
		}))
		p.DefaultRuntimeBrokerID = f.broker.ID
		require.NoError(t, s.UpdateProject(ctx, p))
	}

	mk := func(name, projectID string, ancestry []string) *store.Agent {
		a := &store.Agent{
			ID:        tid(name),
			Slug:      tid(name),
			Name:      name,
			ProjectID: projectID,
			Phase:     string(state.PhaseRunning),
			CreatedBy: f.owner.ID,
			OwnerID:   f.owner.ID,
			Ancestry:  ancestry,
		}
		require.NoError(t, s.CreateAgent(ctx, a))
		return a
	}
	f.caller = mk("bypass-caller", f.proj.ID, []string{f.owner.ID})
	f.sibling = mk("bypass-sibling", f.proj.ID, []string{f.owner.ID})
	f.child = mk("bypass-child", f.proj.ID, []string{f.owner.ID, tid("bypass-caller")})
	f.stranger = mk("bypass-stranger", f.other.ID, []string{f.owner.ID})

	return f
}

// bypassAgentsCreateSA registers a project-scoped GCP service account.
func bypassAgentsCreateSA(t *testing.T, f *bypassAgentsFixture, scopeID string, verified bool) *store.GCPServiceAccount {
	t.Helper()
	sa := &store.GCPServiceAccount{
		ID:        uuid.New().String(),
		Scope:     store.ScopeProject,
		ScopeID:   scopeID,
		Email:     fmt.Sprintf("sa-%s@proj.iam.gserviceaccount.com", uuid.New().String()[:8]),
		ProjectID: "gcp-proj",
		CreatedBy: f.owner.ID,
		Verified:  verified,
		CreatedAt: time.Now(),
	}
	require.NoError(t, f.store.CreateGCPServiceAccount(context.Background(), sa))
	return sa
}

// setupCanDelegateTest creates a test AuthzService with role definitions seeded.
func setupCanDelegateTest(t *testing.T) (*AuthzService, store.Store) {
	t.Helper()
	authz, s := authzTestSetup(t)
	return authz, s
}

// createTestUserWithRole creates a user and assigns them a system-scoped role binding.
// For super-admin bindings, CreatedBy is set to the system reconciler sentinel
// because the D10 store-level guard rejects non-reconciler callers.
func createTestUserWithRole(t *testing.T, s store.Store, userID, email, role, roleName string) {
	t.Helper()
	ctx := context.Background()

	require.NoError(t, s.CreateUser(ctx, &store.User{
		ID: userID, Email: email, DisplayName: email, Role: role, Status: "active",
	}))

	rd, err := s.GetRoleDefinitionByName(ctx, roleName, store.RoleScopeSystem)
	require.NoError(t, err, "role definition %q not found", roleName)

	createdBy := "test"
	if roleName == store.SystemRoleSuperAdmin {
		createdBy = store.SystemReconcileCreatedBy
	}

	_, err = s.CreateRoleBinding(ctx, &store.RoleBinding{
		RoleDefinitionID: rd.ID,
		PrincipalType:    store.RoleBindingPrincipalUser,
		PrincipalID:      userID,
		ScopeType:        store.RoleScopeSystem,
		ScopeID:          "",
		CreatedBy:        createdBy,
	})
	if err != nil && err != store.ErrAlreadyExists {
		t.Fatalf("failed to create role binding: %v", err)
	}
}

// createTestUserWithProjectRole creates a user and assigns them a project-scoped role binding.
func createTestUserWithProjectRole(t *testing.T, s store.Store, userID, email, projectID, roleName string) {
	t.Helper()
	ctx := context.Background()

	// Create user if not exists
	if _, err := s.GetUser(ctx, userID); err != nil {
		require.NoError(t, s.CreateUser(ctx, &store.User{
			ID: userID, Email: email, DisplayName: email, Role: "member", Status: "active",
		}))
	}

	rd, err := s.GetRoleDefinitionByName(ctx, roleName, store.RoleScopeProject)
	require.NoError(t, err, "role definition %q not found", roleName)

	_, err = s.CreateRoleBinding(ctx, &store.RoleBinding{
		RoleDefinitionID: rd.ID,
		PrincipalType:    store.RoleBindingPrincipalUser,
		PrincipalID:      userID,
		ScopeType:        store.RoleScopeProject,
		ScopeID:          projectID,
		CreatedBy:        "test",
	})
	if err != nil && err != store.ErrAlreadyExists {
		t.Fatalf("failed to create project role binding: %v", err)
	}
}

// createDelegateTestProject creates a project in the store.
func createDelegateTestProject(t *testing.T, s store.Store, projectID, slug, createdBy string) {
	t.Helper()
	ctx := context.Background()
	require.NoError(t, s.CreateProject(ctx, &store.Project{
		ID:        projectID,
		Name:      slug,
		Slug:      slug,
		CreatedBy: createdBy,
	}))
}

// edgeLookupErrStore fails GetDelegationEdgesForDelegate for one delegate
// with an error other than store.ErrNotFound.
type edgeLookupErrStore struct {
	store.Store
	failID string
}

// stubSourceResolver returns a fixed source user or error.
type stubSourceResolver struct {
	user *store.User
	err  error
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

// newHubDeliveryNoItemGrantIdentity builds a hub_delivery identity for the
// role-does-not-substitute fixture: agent C's ancestry names an alpha
// project member with no opted-in secret, env var or skill injection, so no association, progeny
// or skill-default grant exists for it on any golden fixture resource. A
// role binding naming a deliver permission is therefore the only grant the
// kernel could match for it.
func newHubDeliveryNoItemGrantIdentity(t *testing.T, f *goldenFixture, agentID string) *hubDeliveryIdentity {
	t.Helper()
	newHubDeliveryTestAgent(t, f.store, agentID, f.projectAlpha.ID, f.memberAlphaID)
	h, err := f.authz.newHubDeliveryIdentity(context.Background(), agentID)
	require.NoError(t, err)
	return h
}

// bindDeliverRoleToAgent gives agentID a system-scope role binding,
// naming only secret.deliver, directly.
func bindDeliverRoleToAgent(t *testing.T, s store.Store, agentID string) {
	t.Helper()
	ctx := context.Background()
	rd := newDeliverRoleDefinition(t, s)
	_, err := s.CreateRoleBinding(ctx, &store.RoleBinding{
		RoleDefinitionID: rd.ID,
		PrincipalType:    store.RoleBindingPrincipalAgent,
		PrincipalID:      agentID,
		ScopeType:        store.RoleScopeSystem,
		CreatedBy:        store.SystemReconcileCreatedBy,
	})
	require.NoError(t, err)
}

// bindDeliverRoleToAgentGroup gives agentID the same role indirectly,
// through membership in a group holding the system-scope role binding
// (authorizationPrincipals adds GetEffectiveGroupsForAgent to the principal
// closure, authz.go).
func bindDeliverRoleToAgentGroup(t *testing.T, s store.Store, groupID, agentID string) {
	t.Helper()
	ctx := context.Background()
	require.NoError(t, s.CreateGroup(ctx, &store.Group{
		ID: groupID, Name: "hub delivery role-only group", Slug: "hd-role-only-" + groupID,
		GroupType: store.GroupTypeExplicit,
	}))
	require.NoError(t, s.AddGroupMember(ctx, &store.GroupMember{
		GroupID: groupID, MemberType: store.GroupMemberTypeAgent, MemberID: agentID, Role: store.GroupMemberRoleMember,
	}))
	rd := newDeliverRoleDefinition(t, s)
	_, err := s.CreateRoleBinding(ctx, &store.RoleBinding{
		RoleDefinitionID: rd.ID,
		PrincipalType:    store.RoleBindingPrincipalGroup,
		PrincipalID:      groupID,
		ScopeType:        store.RoleScopeSystem,
		CreatedBy:        store.SystemReconcileCreatedBy,
	})
	require.NoError(t, err)
}

// withDeliveryCredentialKinds replaces the delivery credential set for the
// duration of a test. Callers must not use t.Parallel.
func withDeliveryCredentialKinds(t *testing.T, kinds ...CredentialKind) {
	t.Helper()
	set := map[CredentialKind]struct{}{}
	for _, k := range kinds {
		set[k] = struct{}{}
	}
	deliveryCredentialKindsMu.Lock()
	saved := deliveryCredentialKinds
	deliveryCredentialKinds = set
	deliveryCredentialKindsMu.Unlock()
	t.Cleanup(func() {
		deliveryCredentialKindsMu.Lock()
		deliveryCredentialKinds = saved
		deliveryCredentialKindsMu.Unlock()
	})
}

func deliveryGateRequest(identity Identity, kind CredentialKind, res Resource, perm string) AuthzRequest {
	return AuthzRequest{
		Principal:  principalContextForIdentity(identity),
		Credential: CredentialContext{Kind: kind},
		Resource:   res,
		Action:     ActionDeliver,
		Permission: perm,
		Explain:    true,
	}
}

// assertRequestNotAdmitted asserts the request was denied, and that the
// reason is one Decide can actually produce for it (notAdmittedReasons),
// without pinning which of entry classification or the delivery gate denied
// it. Both leave the request unadmitted, which is the invariant this
// asserts.
func assertRequestNotAdmitted(t *testing.T, d Decision, msg string) {
	t.Helper()
	assert.False(t, d.Allowed, "%s: reason %q", msg, d.Reason)
	assert.Contains(t, notAdmittedReasons, d.Reason, "%s: unexpected deny reason %q", msg, d.Reason)
}

func boundedCeiling(ids ...string) store.EffectCeiling {
	if ids == nil {
		ids = []string{}
	}
	return store.EffectCeiling{Kind: store.EffectCeilingBounded, Version: permissions.CeilingVersionV1, PermissionIDs: ids}
}

func allRegistryIDs() []string {
	ids := make([]string, 0, len(permissions.Registry))
	for _, p := range permissions.Registry {
		ids = append(ids, p.ID)
	}
	return ids
}

// uatCeilingFromSelectors returns the bounded ceiling a V1 UAT with the given
// selectors carries.
func uatCeilingFromSelectors(t *testing.T, selectors ...string) store.EffectCeiling {
	t.Helper()
	var ids []string
	for _, sel := range selectors {
		found := false
		for _, p := range permissions.Registry {
			if p.UATScope == sel {
				ids = append(ids, p.ID)
				found = true
			}
		}
		require.True(t, found, "unknown UAT selector %q", sel)
	}
	return boundedCeiling(sortedUniqueIDs(ids)...)
}

// readonlyRoleUATSelectors returns the UAT selectors that cover the
// readonly role's required scopes: the seven read selectors of the worked
// example. Ceiling-optional role scopes (ceilingOptionalRoleScopes) are left
// out: they never decide whether a ceiling fits the role.
func readonlyRoleUATSelectors(t *testing.T) []string {
	t.Helper()
	readSelectors := []string{}
	for _, scope := range ScopesForRole(AgentRoleReadOnly) {
		if ceilingOptionalRoleScopes[scope] {
			continue
		}
		for _, permID := range agentScopeCoverage([]AgentTokenScope{scope}) {
			p, _ := registryPermission(permID)
			if p.UATScope != "" {
				readSelectors = append(readSelectors, p.UATScope)
			}
		}
	}
	readSelectors = sortedUniqueIDs(readSelectors)
	require.Len(t, readSelectors, 7, "the worked example uses seven read selectors")
	return readSelectors
}

// ambiguousEdgeStore returns every edge of dupID twice.
type ambiguousEdgeStore struct {
	store.Store
	dupID string
}

type ceilingFixture struct {
	store     store.Store
	projectID string
	userID    string
}

func newCeilingFixture(t *testing.T, name string) ceilingFixture {
	t.Helper()
	_, s := authzTestSetup(t)
	f := ceilingFixture{store: s, projectID: tid("ec-proj-" + name), userID: tid("ec-user-" + name)}
	createDCProject(t, s, f.projectID, "ec-"+name)
	createDCUser(t, s, f.userID, "ec-"+name+"@test.com", f.projectID, store.ProjectRoleOwner)
	return f
}

var (
	provSession  = store.AuthorityProvenance{ProvenanceVersion: 1, SourceCredentialKind: store.SourceCredentialSession}
	provAgent    = store.AuthorityProvenance{ProvenanceVersion: 1, SourceCredentialKind: store.SourceCredentialAgent}
	provDevLocal = store.AuthorityProvenance{ProvenanceVersion: 1, SourceCredentialKind: store.SourceCredentialDevLocal}
	ceilPrincip  = store.EffectCeiling{Kind: store.EffectCeilingPrincipal}
)

// seedExecutionAgent stores an agent row in projectID with the given
// ancestry and records the typed delegation edges of its chain in that
// project: chain[0] is the source user, chain[1:] are intermediate agents
// (stored if absent), and each link delegates to the next, the last to the
// agent.
func seedExecutionAgent(t *testing.T, s store.Store, agentID, projectID string, ancestry, chain []string) {
	t.Helper()
	ctx := context.Background()
	require.NotEmpty(t, chain, "chain names the source user")
	storeAgentIfAbsent := func(id string, anc []string) {
		if _, err := s.GetAgent(ctx, id); err == nil {
			return
		}
		require.NoError(t, s.CreateAgent(ctx, &store.Agent{
			ID: id, Slug: "exec-" + id[:8], Name: "exec-" + id[:8],
			ProjectID: projectID, Phase: "running",
			OwnerID: chain[0], CreatedBy: chain[0], Ancestry: anc,
			AppliedConfig: &store.AgentAppliedConfig{AgentRole: string(AgentRoleFull)},
		}))
	}
	for i := 1; i < len(chain); i++ {
		storeAgentIfAbsent(chain[i], append([]string(nil), chain[:i]...))
	}
	storeAgentIfAbsent(agentID, ancestry)

	for i := 0; i < len(chain); i++ {
		delegatorType := store.DelegationPrincipalAgent
		if i == 0 {
			delegatorType = store.DelegationPrincipalUser
		}
		delegate := agentID
		if i+1 < len(chain) {
			delegate = chain[i+1]
		}
		seedRecordedDelegationEdge(t, s, delegatorType, chain[i], store.DelegationPrincipalAgent, delegate,
			store.RoleScopeProject, projectID, string(AgentRoleFull))
	}
}

// execAgent is a local agent identity in projectID whose ancestry is
// rooted at the golden fixture's secret owner.
func execAgent(id, projectID string, ancestry []string) AgentIdentity {
	return &agentIdentityWrapper{&AgentTokenClaims{
		Claims:    jwt.Claims{Subject: id},
		ProjectID: projectID,
		Ancestry:  ancestry,
		Scopes:    allRegisteredAgentScopes(),
	}}
}

// extraEdgeStore adds one more delegation edge for delegateID.
type extraEdgeStore struct {
	store.Store
	delegateID string
	extra      *store.DelegationEdge
}

// gcpUseResource builds the Resource Decide sees for one gcp_service_account
// row, mirroring gcpServiceAccountResource without requiring a persisted row.
func gcpUseResource(id string) Resource {
	return Resource{Type: permissions.ResourceGCPServiceAccount, ID: id}
}

// goldenFixture sets up a shared world for the golden decision tests:
//
//   - Two projects (alpha, beta) with members groups and agents
//   - A super-admin user, a hub-admin user
//   - A hub-member user who is a member of project alpha only
//   - A hub-member user with NO project memberships
//   - A project-owner user for alpha
//   - A project-admin user for alpha
//   - Agents in both projects, with ancestry chains for progeny tests
//   - Progeny policies for secrets, env vars, and skill injections
type goldenFixture struct {
	authz *AuthzService
	store store.Store

	projectAlpha *store.Project
	projectBeta  *store.Project

	// Groups
	hubMembersGroup   *store.Group
	alphaMembersGroup *store.Group
	betaMembersGroup  *store.Group
	alphaAgentsGroup  *store.Group
	betaAgentsGroup   *store.Group

	// Users
	superAdminID   string
	hubAdminID     string
	memberAlphaID  string // hub member AND alpha project member
	memberNoneID   string // hub member with NO project memberships
	projectOwnerID string // owner of project alpha
	projectAdminID string // admin of project alpha

	// Agents
	agentAlpha *store.Agent // agent in project alpha
	agentBeta  *store.Agent // agent in project beta

	// Progeny test resources
	secretID         string
	envVarID         string
	skillInjectionID string
}

func newGoldenFixture(t *testing.T) *goldenFixture {
	t.Helper()
	authz, s := authzTestSetup(t)
	ctx := context.Background()

	f := &goldenFixture{
		authz:            authz,
		store:            s,
		superAdminID:     tid("golden-superadmin"),
		hubAdminID:       tid("golden-hubadmin"),
		memberAlphaID:    tid("golden-member-alpha"),
		memberNoneID:     tid("golden-member-none"),
		projectOwnerID:   tid("golden-proj-owner"),
		projectAdminID:   tid("golden-proj-admin"),
		secretID:         tid("golden-secret"),
		envVarID:         tid("golden-envvar"),
		skillInjectionID: tid("golden-skill-inj"),
	}

	// --- Projects ---
	f.projectAlpha = &store.Project{
		ID: tid("golden-project-alpha"), Name: "Alpha", Slug: "golden-alpha",
		OwnerID: f.projectOwnerID,
	}
	f.projectBeta = &store.Project{
		ID: tid("golden-project-beta"), Name: "Beta", Slug: "golden-beta",
		OwnerID: tid("golden-beta-owner"),
	}
	require.NoError(t, s.CreateProject(ctx, f.projectAlpha))
	require.NoError(t, s.CreateProject(ctx, f.projectBeta))

	// --- Groups ---
	// hub-members group (the seeded one may already exist from testServer)
	hmGroup, err := s.GetGroupBySlug(ctx, "hub-members")
	if err != nil {
		hmGroup = &store.Group{
			ID: api.NewUUID(), Name: "Hub Members", Slug: "hub-members",
			GroupType: store.GroupTypeExplicit,
		}
		require.NoError(t, s.CreateGroup(ctx, hmGroup))
	}
	f.hubMembersGroup = hmGroup

	f.alphaMembersGroup = &store.Group{
		ID: api.NewUUID(), Name: "Alpha Members",
		Slug: "project:golden-alpha:members", GroupType: store.GroupTypeExplicit,
		ProjectID: f.projectAlpha.ID,
	}
	f.betaMembersGroup = &store.Group{
		ID: api.NewUUID(), Name: "Beta Members",
		Slug: "project:golden-beta:members", GroupType: store.GroupTypeExplicit,
		ProjectID: f.projectBeta.ID,
	}
	f.alphaAgentsGroup = &store.Group{
		ID: api.NewUUID(), Name: "Alpha Agents",
		Slug: "project:golden-alpha:agents", GroupType: store.GroupTypeProjectAgents,
		ProjectID: f.projectAlpha.ID,
	}
	f.betaAgentsGroup = &store.Group{
		ID: api.NewUUID(), Name: "Beta Agents",
		Slug: "project:golden-beta:agents", GroupType: store.GroupTypeProjectAgents,
		ProjectID: f.projectBeta.ID,
	}
	require.NoError(t, s.CreateGroup(ctx, f.alphaMembersGroup))
	require.NoError(t, s.CreateGroup(ctx, f.betaMembersGroup))
	require.NoError(t, s.CreateGroup(ctx, f.alphaAgentsGroup))
	require.NoError(t, s.CreateGroup(ctx, f.betaAgentsGroup))

	// --- Users ---
	// Super-admin
	createTestUserWithRole(t, s, f.superAdminID, "superadmin@golden.test", "admin", store.SystemRoleSuperAdmin)
	// Hub admin
	createTestUserWithRole(t, s, f.hubAdminID, "hubadmin@golden.test", "member", store.SystemRoleHubAdmin)
	// Hub member who is also alpha project member
	createTestUserWithRole(t, s, f.memberAlphaID, "member-alpha@golden.test", "member", store.SystemRoleHubMember)
	// Hub member with NO project memberships
	createTestUserWithRole(t, s, f.memberNoneID, "member-none@golden.test", "member", store.SystemRoleHubMember)
	// Project owner of alpha (also a hub member)
	require.NoError(t, s.CreateUser(ctx, &store.User{
		ID: f.projectOwnerID, Email: "proj-owner@golden.test",
		DisplayName: "Project Owner", Role: "member", Status: "active",
	}))
	// Project admin of alpha (also a hub member)
	require.NoError(t, s.CreateUser(ctx, &store.User{
		ID: f.projectAdminID, Email: "proj-admin@golden.test",
		DisplayName: "Project Admin", Role: "member", Status: "active",
	}))

	// --- Hub Members group memberships ---
	for _, uid := range []string{f.memberAlphaID, f.memberNoneID, f.projectOwnerID, f.projectAdminID} {
		_ = s.AddGroupMember(ctx, &store.GroupMember{
			GroupID: f.hubMembersGroup.ID, MemberID: uid,
			MemberType: store.GroupMemberTypeUser, Role: store.GroupMemberRoleMember,
		})
	}

	// --- Alpha project memberships ---
	require.NoError(t, s.AddGroupMember(ctx, &store.GroupMember{
		GroupID: f.alphaMembersGroup.ID, MemberID: f.projectOwnerID,
		MemberType: store.GroupMemberTypeUser, Role: store.GroupMemberRoleOwner,
	}))
	require.NoError(t, s.AddGroupMember(ctx, &store.GroupMember{
		GroupID: f.alphaMembersGroup.ID, MemberID: f.projectAdminID,
		MemberType: store.GroupMemberTypeUser, Role: store.GroupMemberRoleAdmin,
	}))
	require.NoError(t, s.AddGroupMember(ctx, &store.GroupMember{
		GroupID: f.alphaMembersGroup.ID, MemberID: f.memberAlphaID,
		MemberType: store.GroupMemberTypeUser, Role: store.GroupMemberRoleMember,
	}))

	// --- Project role bindings for alpha members ---
	createTestUserWithProjectRole(t, s, f.projectOwnerID, "proj-owner@golden.test",
		f.projectAlpha.ID, store.ProjectRoleOwner)
	createTestUserWithProjectRole(t, s, f.projectAdminID, "proj-admin@golden.test",
		f.projectAlpha.ID, store.ProjectRoleAdmin)
	createTestUserWithProjectRole(t, s, f.memberAlphaID, "member-alpha@golden.test",
		f.projectAlpha.ID, store.ProjectRoleMember)

	// --- Agents ---
	f.agentAlpha = &store.Agent{
		ID: tid("golden-agent-alpha"), Slug: "golden-agent-alpha",
		Name: "Alpha Agent", ProjectID: f.projectAlpha.ID,
		Phase:    string(state.PhaseRunning),
		OwnerID:  f.projectOwnerID,
		Ancestry: []string{f.projectOwnerID},
	}
	f.agentBeta = &store.Agent{
		ID: tid("golden-agent-beta"), Slug: "golden-agent-beta",
		Name: "Beta Agent", ProjectID: f.projectBeta.ID,
		Phase:   string(state.PhaseRunning),
		OwnerID: tid("golden-beta-owner"),
	}
	require.NoError(t, s.CreateAgent(ctx, f.agentAlpha))
	require.NoError(t, s.CreateAgent(ctx, f.agentBeta))

	// CO1: Legacy per-project member policies removed. Authorization is
	// handled exclusively by RoleBindings and the AK1 kernel. The
	// createTestUserWithProjectRole helper above creates the necessary
	// project-scoped RoleBindings.

	// --- Progeny resources (CO1: relationship grants replace DelegatedFrom policies) ---
	// The RelationshipGrantResolver uses store.ListProgenySecrets/EnvVars/SkillInjections
	// to check progeny access. These store methods filter for AllowProgeny=true and
	// CreatedBy IN ancestry.

	// Secret with progeny access
	require.NoError(t, s.CreateSecret(ctx, &store.Secret{
		ID:           f.secretID,
		Key:          "golden-secret",
		Scope:        "user",
		ScopeID:      f.projectOwnerID,
		AllowProgeny: true,
		CreatedBy:    f.projectOwnerID,
	}))

	// EnvVar with progeny access
	require.NoError(t, s.CreateEnvVar(ctx, &store.EnvVar{
		ID:           f.envVarID,
		Key:          "golden-envvar",
		Value:        "test-value",
		Scope:        "user",
		ScopeID:      f.projectOwnerID,
		AllowProgeny: true,
		CreatedBy:    f.projectOwnerID,
	}))

	// Skill injection with progeny access
	require.NoError(t, s.AddSkillInjection(ctx, &store.SkillInjection{
		ID:           f.skillInjectionID,
		Scope:        "user",
		ScopeID:      f.projectOwnerID,
		SkillURI:     "test://golden-skill",
		AllowProgeny: true,
		CreatedBy:    f.projectOwnerID,
	}))

	return f
}

// seedRoleUser creates an active user who is a hub member and, when
// roleName is not empty and not hub-member, also holds that system role.
func seedRoleUser(t *testing.T, s store.Store, id, roleName string, hubMember bool) *AuthenticatedUser {
	t.Helper()
	ctx := context.Background()
	require.NoError(t, s.CreateUser(ctx, &store.User{ID: id, Email: id + "@test.com", DisplayName: id, Role: store.UserRoleMember, Status: "active"}))
	if hubMember {
		ensureHubMembership(ctx, s, id)
	}
	if roleName != "" && roleName != store.SystemRoleHubMember {
		rd, err := s.GetRoleDefinitionByName(ctx, roleName, store.RoleScopeSystem)
		require.NoError(t, err)
		createdBy := "test-setup"
		if roleName == store.SystemRoleSuperAdmin {
			createdBy = store.SystemReconcileCreatedBy
		}
		_, err = s.CreateRoleBinding(ctx, &store.RoleBinding{
			RoleDefinitionID: rd.ID,
			PrincipalType:    store.RoleBindingPrincipalUser,
			PrincipalID:      id,
			ScopeType:        store.RoleScopeSystem,
			CreatedBy:        createdBy,
		})
		require.NoError(t, err)
	}
	return bearerUser(id)
}

func newParentCeilingFixture(t *testing.T, name string) parentCeilingFixture {
	t.Helper()
	authz, s := setupDelegationCeilingTest(t)
	f := parentCeilingFixture{
		authz:     authz,
		store:     s,
		projectID: tid("pc-proj-" + name),
		userID:    tid("pc-user-" + name),
	}
	createDCProject(t, s, f.projectID, "pc-"+name)
	createDCUser(t, s, f.userID, name+"@pc.test", f.projectID, store.ProjectRoleOwner)
	return f
}

// userRoleHolds reports whether userID holds perm through its roles in the
// project (a project target, so no named relationship applies).
func userRoleHolds(t *testing.T, a *AuthzService, userID, perm, projectID string) bool {
	t.Helper()
	ok, _, err := a.evaluateUserDelegatorAuthority(context.Background(), userID,
		Resource{Type: "project", ID: projectID}, ActionRead, perm, store.RoleScopeProject, projectID)
	require.NoError(t, err)
	return ok
}

func softDeleteStoredAgent(t *testing.T, s store.Store, id string) {
	t.Helper()
	a, err := s.GetAgent(context.Background(), id)
	require.NoError(t, err)
	a.DeletedAt = time.Now()
	require.NoError(t, s.UpdateAgent(context.Background(), a))
}

func assertCeilingDeny(t *testing.T, d Decision, msg string) {
	t.Helper()
	assert.False(t, d.Allowed, "%s: reason %q", msg, d.Reason)
	assert.Equal(t, DeniedByDelegationCeiling, d.DeniedBy, "%s: reason %q", msg, d.Reason)
}

// addProjectMemberWithRole is a small helper that adds the given user to the
// project's members group with the requested role.
func addProjectMemberWithRole(t *testing.T, s store.Store, project *store.Project, userID, role string) {
	t.Helper()
	ctx := context.Background()
	membersGroup, err := s.GetGroupBySlug(ctx, "project:"+project.Slug+":members")
	require.NoError(t, err)
	require.NoError(t, s.AddGroupMember(ctx, &store.GroupMember{
		GroupID:    membersGroup.ID,
		MemberType: store.GroupMemberTypeUser,
		MemberID:   userID,
		Role:       role,
	}))

	// Phase 1F: also create the corresponding project role binding, since
	// isProjectOwnerOrAdmin now uses role bindings as the sole source of truth.
	groupRoleMap := map[string]string{
		store.GroupMemberRoleOwner:  store.ProjectRoleOwner,
		store.GroupMemberRoleAdmin:  store.ProjectRoleAdmin,
		store.GroupMemberRoleMember: store.ProjectRoleMember,
	}
	if roleName, ok := groupRoleMap[role]; ok {
		rd, err := s.GetRoleDefinitionByName(ctx, roleName, store.RoleScopeProject)
		require.NoError(t, err, "project role definition %q not found", roleName)
		_, err = s.CreateRoleBinding(ctx, &store.RoleBinding{
			RoleDefinitionID: rd.ID,
			PrincipalType:    store.RoleBindingPrincipalUser,
			PrincipalID:      userID,
			ScopeType:        store.RoleScopeProject,
			ScopeID:          project.ID,
			CreatedBy:        "test",
		})
		if err != nil && err != store.ErrAlreadyExists {
			t.Fatalf("failed to create project role binding: %v", err)
		}
	}
}

// makeProjectMemberUser creates a user, adds them to hub-members, and adds them
// to the project's members group with the given role.
func makeProjectMemberUser(t *testing.T, s store.Store, project *store.Project, id, name, role string) *store.User {
	t.Helper()
	ctx := context.Background()
	u := &store.User{
		ID:          id,
		Email:       id + "@test.com",
		DisplayName: name,
		Role:        store.UserRoleMember,
		Status:      "active",
		Created:     time.Now(),
	}
	require.NoError(t, s.CreateUser(ctx, u))
	ensureHubMembership(ctx, s, u.ID)
	addProjectMemberWithRole(t, s, project, u.ID, role)
	return u
}

// recordedProv returns version-1 provenance whose source is the edge's
// delegator, as every recording write produces.
func recordedProv(kind string, id string, cred store.SourceCredentialKind) store.AuthorityProvenance {
	return store.AuthorityProvenance{
		ProvenanceVersion:    store.ProvenanceVersionV1,
		SourcePrincipalKind:  kind,
		SourcePrincipalID:    id,
		SourceCredentialKind: cred,
	}
}

// provenanceFixture is a project with an admitted owner user, and an
// AuthzService with local development authority as requested.
type provenanceFixture struct {
	ceilingFixture
	a *AuthzService
}

func newProvenanceFixture(t *testing.T, name string, devLocal bool) provenanceFixture {
	t.Helper()
	f := newCeilingFixture(t, "pr-"+name)
	return provenanceFixture{ceilingFixture: f, a: f.authz(f.store, devLocal, false)}
}

// faultStore fails the named lookups.
type faultStore struct {
	store.Store
	edges, users, agents bool
	userID               string // when set, only GetUser(userID) fails
}

// parseHubProduction parses the non-test Go files of this package.
func parseHubProduction(t *testing.T) (*token.FileSet, []*ast.File) {
	t.Helper()
	fset := token.NewFileSet()
	paths, err := filepath.Glob("*.go")
	require.NoError(t, err)
	var files []*ast.File
	for _, p := range paths {
		if strings.HasSuffix(p, "_test.go") {
			continue
		}
		src, err := os.ReadFile(p)
		require.NoError(t, err)
		f, err := parser.ParseFile(fset, p, src, 0)
		require.NoError(t, err)
		files = append(files, f)
	}
	return fset, files
}

// declReceiverName returns "Recv.Name" or "Name" for decl.
func declReceiverName(decl *ast.FuncDecl) string {
	if decl.Recv == nil || len(decl.Recv.List) == 0 {
		return decl.Name.Name
	}
	typ := decl.Recv.List[0].Type
	if star, ok := typ.(*ast.StarExpr); ok {
		typ = star.X
	}
	if id, ok := typ.(*ast.Ident); ok {
		return id.Name + "." + decl.Name.Name
	}
	return decl.Name.Name
}

// callsIn returns, per enclosing function, the calls to a function or
// method named name.
func callsIn(files []*ast.File, name string) map[string][]*ast.CallExpr {
	out := map[string][]*ast.CallExpr{}
	for _, f := range files {
		for _, d := range f.Decls {
			fn, ok := d.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				switch fun := call.Fun.(type) {
				case *ast.Ident:
					if fun.Name == name {
						out[declReceiverName(fn)] = append(out[declReceiverName(fn)], call)
					}
				case *ast.SelectorExpr:
					if fun.Sel.Name == name {
						out[declReceiverName(fn)] = append(out[declReceiverName(fn)], call)
					}
				}
				return true
			})
		}
	}
	return out
}

// relationshipAllowKey identifies one (relationship, principal kind,
// resource type) cell of the characterization table. PrincipalKind uses the
// relationship-policy vocabulary ("user", "agent").
type relationshipAllowKey struct {
	Relationship  string
	PrincipalKind string
	ResourceType  string
}

// relationshipUnregisteredPermissions lists permission IDs that a production
// call site derives from a (resource type, action) pair with no registry
// entry, and that a relationship admits. Each entry names its call sites.
// These IDs are accepted by the permission resolver and by the relationship
// policy consistency test only through this list.
var relationshipUnregisteredPermissions = map[string][]string{}

// relationshipCrossTypePermissions lists relationship cells whose resource
// type differs from the registry resource of the permission they admit. The
// value is the production call site that makes that request.
var relationshipCrossTypePermissions = map[relationshipAllowKey]map[string]string{
	{Relationship: "progeny", PrincipalKind: "agent", ResourceType: "secret"}: {
		"project.secret_read": "httpdispatcher.go (agent secret resolution: Resource{Type: \"secret\"}, Permission: project.secret_read)",
	},
}

// relationshipCharacterizedAllowlist is the frozen per-cell permission list.
var relationshipCharacterizedAllowlist = map[relationshipAllowKey][]string{
	// Resource owner (user principals). Every same-type registry permission.
	// Hub-scoped gcp_service_account assign is excluded by the rule shape
	// (see TestRelationshipCharacterization_Owner) and is covered by
	// hub_member_sa_assign instead.
	{"owner", "user", "agent"}: {
		"agent.create", "agent.read", "agent.list", "agent.update", "agent.delete",
		"agent.attach", "agent.lifecycle", "agent.port_access", "agent.stop_all",
		"agent.message", "agent.set_message_mode", "agent.grant_hub_mode",
		"agent.status_update", "agent.notify",
		"agent.token_refresh", "agent.port_forward", "agent.identity_token",
	},
	// No {"owner", "user", "project"} cell: Project.OwnerID grants nothing
	// (ptone/scion#2586); see TestRelationshipCharacterization_OwnerProjectGrantsNothing.
	{"owner", "user", "template"}: {
		"template.create", "template.read", "template.update", "template.delete", "template.list",
	},
	{"owner", "user", "harness_config"}: {
		"harness_config.create", "harness_config.read", "harness_config.update",
		"harness_config.delete", "harness_config.list",
	},
	{"owner", "user", "group"}: {
		"group.create", "group.read", "group.update", "group.delete", "group.list",
		"group.addMember", "group.removeMember",
	},
	{"owner", "user", "broker"}: {
		"broker.create", "broker.read", "broker.update", "broker.delete", "broker.list", "broker.dispatch",
	},
	{"owner", "user", "gcp_service_account"}: {
		"gcp_service_account.create", "gcp_service_account.read", "gcp_service_account.delete",
		"gcp_service_account.list", "gcp_service_account.verify", "gcp_service_account.mint",
		"gcp_service_account.assign",
	},
	{"owner", "user", "skill"}: {
		"skill.create", "skill.create_global", "skill.read", "skill.update", "skill.delete",
		"skill.list", "skill.register",
	},

	// Ancestor (a principal in the agent resource's creation chain).
	{"ancestor", "user", "agent"}: {
		"agent.create", "agent.read", "agent.list", "agent.update", "agent.delete",
		"agent.attach", "agent.lifecycle", "agent.port_access", "agent.stop_all",
		"agent.message", "agent.set_message_mode", "agent.grant_hub_mode",
		"agent.status_update", "agent.notify",
		"agent.token_refresh", "agent.port_forward", "agent.identity_token",
	},
	// Agent ancestors are further limited by their JWT scopes; this cell is
	// the set reachable when the agent holds every registered agent scope.
	{"ancestor", "agent", "agent"}: {
		"agent.create", "agent.delete", "agent.attach", "agent.lifecycle",
		"agent.set_message_mode", "agent.status_update", "agent.notify",
		"agent.token_refresh", "agent.port_forward", "agent.identity_token",
	},

	// An agent reads the status of an agent it directly launched, in the
	// same project, on the single-agent GET routes only
	// (TestLauncherRead_OnlySingleAgentReadWidened).
	{"launcher", "agent", "agent"}: {"agent.read"},

	// Progeny read of an ancestor's opted-in user-scoped secret, plus the
	// reviewed exact pairs (reviewedProgenyExactPairs): runtime use and launch
	// delivery of opted-in user-scope secrets and env vars.
	{"progeny", "agent", "secret"}:  {"project.secret_read", "secret.use", "secret.deliver"},
	{"progeny", "agent", "env_var"}: {"env_var.deliver"},

	// Current hub members may assign hub-scoped service accounts.
	{"hub_member_sa_assign", "user", "gcp_service_account"}: {"gcp_service_account.assign"},

	// An agent may read its origin user's personal (user-scoped) skills,
	// through the same progeny relationship as opted-in secrets, keyed on
	// the skill's owning bucket rather than a per-record creator field.
	{"progeny", "agent", "skill"}: {"skill.read"},
}

// sameTypeRegistryPermissions returns every registry permission ID whose
// Resource is resourceType, in registry order.
func sameTypeRegistryPermissions(resourceType string) []permissions.Permission {
	var out []permissions.Permission
	for _, p := range permissions.Registry {
		if p.Resource == resourceType {
			out = append(out, p)
		}
	}
	return out
}

func characterizedSet(key relationshipAllowKey) map[string]bool {
	set := map[string]bool{}
	for _, id := range relationshipCharacterizedAllowlist[key] {
		set[id] = true
	}
	return set
}

// allRegisteredAgentScopes returns every agent JWT scope referenced by the
// registry, so an agent carrying them is limited only by the relationship.
func allRegisteredAgentScopes() []AgentTokenScope {
	seen := map[string]bool{}
	var out []AgentTokenScope
	for _, p := range permissions.Registry {
		for _, s := range p.AgentScopes {
			if !seen[s] {
				seen[s] = true
				out = append(out, AgentTokenScope(s))
			}
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

func decideExplicit(t *testing.T, authz *AuthzService, identity Identity, resource Resource, p permissions.Permission) Decision {
	t.Helper()
	return authz.Decide(context.Background(), AuthzRequest{
		Principal:  principalContextForIdentity(identity),
		Credential: credentialContextForIdentity(identity),
		Resource:   resource,
		Action:     Action(p.Action),
		Permission: p.ID,
	})
}

func createCharacterizationUser(t *testing.T, s store.Store, id string) UserIdentity {
	t.Helper()
	require.NoError(t, s.CreateUser(context.Background(), &store.User{
		ID: id, Email: id + "@relchar.test", DisplayName: id, Role: "member", Status: "active",
	}))
	return NewAuthenticatedUser(id, id+"@relchar.test", id, "member", "api")
}

// grantProjectAccessOnly binds userID in projectID to a project-scoped role
// with no permissions. The binding is project membership (active project
// access, which owner and ancestor relationships on project targets
// require; ptone/scion#2141) without granting any permission through the
// role, so the relationship stays the only grant source under test.
func grantProjectAccessOnly(t *testing.T, s store.Store, userID, projectID string) {
	t.Helper()
	ctx := context.Background()
	name := "relchar-access-only"
	rd, err := s.GetRoleDefinitionByName(ctx, name, store.RoleScopeProject)
	if err != nil {
		require.ErrorIs(t, err, store.ErrNotFound)
		rd, err = s.CreateRoleDefinition(ctx, &store.RoleDefinition{
			ID: api.NewUUID(), Name: name, Description: "project access, no permissions",
			ScopeType: store.RoleScopeProject, Permissions: []string{},
		})
		require.NoError(t, err)
	}
	_, err = s.CreateRoleBinding(ctx, &store.RoleBinding{
		RoleDefinitionID: rd.ID, PrincipalType: store.RoleBindingPrincipalUser, PrincipalID: userID,
		ScopeType: store.RoleScopeProject, ScopeID: projectID, CreatedBy: "test",
	})
	require.NoError(t, err)
}

// knownRelationshipNames lists every relationship name a row may use.
// Association relationships are typed but have no rows until their
// permissions are registered (TestRelationshipPolicy_AssociationRowsRequireRegistryIDs).
var knownRelationshipNames = map[string]bool{
	"owner":                true,
	"ancestor":             true,
	"progeny":              true,
	"hub_member_sa_assign": true,
	"launcher":             true,
	"project_association":  true,
	"hub_association":      true,
	"broker_association":   true,
}

// relationshipPolicyPrincipalKinds is the principal-kind vocabulary rows use.
var relationshipPolicyPrincipalKinds = map[string]bool{"user": true, "agent": true}

func progenyPairAgent(subject, projectID string, ancestry []string, scopes []AgentTokenScope) *agentIdentityWrapper {
	return &agentIdentityWrapper{&AgentTokenClaims{
		Claims:    jwt.Claims{Subject: subject},
		ProjectID: projectID,
		Ancestry:  ancestry,
		Scopes:    scopes,
	}}
}

func fedPrincipalID(sub string) string { return fedTestIssuer + ":" + sub }

// federatedBindingStore is a test store wrapper that records role bindings,
// group membership and users rows keyed to federated principal IDs
// (<issuer>:<sub>). The persistent store cannot hold these records today:
// CreateRoleBinding requires a principal ID that names an existing users
// row, and GetEffectiveGroups rejects an ID that is not a UUID (see
// ptone/scion#3427). The wrapper serves them so the authorization rule can
// be exercised end to end. Every other read passes through to the wrapped
// store. It can also inject a fault for each federated read and serve extra
// access-constraint rows.
type federatedBindingStore struct {
	store.Store
	mu               sync.Mutex
	bindings         []*store.RoleBinding
	groups           map[string][]string
	users            map[string]*store.User
	extraConstraints []*store.AccessConstraint
	failUser         bool
	failBindings     bool
	failGroups       bool
	failConstraints  bool
}

func newFedFixture(t *testing.T, name string) *fedFixture {
	t.Helper()
	f := newRPAFixture(t, "fed"+name)
	owners := installAgentOwnerOverride(t, f.srv)
	fs := &federatedBindingStore{Store: owners, groups: map[string][]string{}, users: map[string]*store.User{}}
	f.srv.authzService.store = fs
	return &fedFixture{rpaFixture: f, fed: fs, owners: owners}
}

func fedIdentity(sub string) *FederatedUserIdentity {
	return NewFederatedUserIdentity(fedTestIssuer, sub, sub+"@idp.example", "Fed "+sub, "viewer", nil)
}

type rpaFixture struct {
	srv       *Server
	store     store.Store // the real store, for fixture writes
	counting  *rpaStore   // the store the authz service reads through
	projectID string
	ownerID   string // the project owner (not a principal under test)
}

func newRPAFixture(t *testing.T, name string) *rpaFixture {
	t.Helper()
	srv, s := testServer(t)
	f := &rpaFixture{
		srv:       srv,
		store:     s,
		projectID: tid("rpa-" + name + "-project"),
		ownerID:   tid("rpa-" + name + "-powner"),
	}
	createRS1Project(t, s, f.projectID, f.ownerID)
	f.counting = &rpaStore{Store: s, getUserCalls: map[string]int{}}
	orig := srv.authzService.store
	srv.authzService.store = f.counting
	t.Cleanup(func() { srv.authzService.store = orig })
	return f
}

func decidePerm(authz *AuthzService, identity Identity, resource Resource, action Action, permissionID string, explain bool) Decision {
	return authz.Decide(context.Background(), AuthzRequest{
		Principal:  principalContextForIdentity(identity),
		Credential: credentialContextForIdentity(identity),
		Resource:   resource,
		Action:     action,
		Permission: permissionID,
		Explain:    explain,
	})
}

func relationshipResult(t *testing.T, d Decision, rule RelationshipRuleID) RelationshipCandidateResult {
	t.Helper()
	require.NotNil(t, d.Provenance, "explain provenance")
	for _, r := range d.Provenance.Relationships {
		if r.Rule == rule {
			return r
		}
	}
	t.Fatalf("no %s candidate in provenance: %+v", rule, d.Provenance.Relationships)
	return RelationshipCandidateResult{}
}

func setUserStatus(t *testing.T, s store.Store, id, status string) {
	t.Helper()
	u, err := s.GetUser(context.Background(), id)
	require.NoError(t, err)
	u.Status = status
	require.NoError(t, s.UpdateUser(context.Background(), u))
}

type fakeProgenyAdapter struct {
	kind    string
	perms   []string
	sources []SharingSource
	err     error
}

// releaseBuiltinProgenyAdapter lets a test register its own adapter for a
// kind served by the built-in store adapter.
func releaseBuiltinProgenyAdapter(t *testing.T, a *AuthzService, kind string) {
	t.Helper()
	require.True(t, progenyOptInKinds[kind], "kind %q has no built-in adapter", kind)
	a.progenyAdapters.mu.Lock()
	defer a.progenyAdapters.mu.Unlock()
	if a.progenyAdapters.builtinReleased == nil {
		a.progenyAdapters.builtinReleased = map[string]bool{}
	}
	a.progenyAdapters.builtinReleased[kind] = true
}

// callerFuncNames returns the fully qualified function names on the
// calling goroutine's stack, innermost first.
func callerFuncNames() []string {
	pcs := make([]uintptr, 512)
	n := runtime.Callers(2, pcs)
	frames := runtime.CallersFrames(pcs[:n])
	var out []string
	for {
		f, more := frames.Next()
		out = append(out, f.Function)
		if !more {
			break
		}
	}
	return out
}

// memoTestStore wraps a real store.Store and intercepts the methods the
// memo serves or that its tests need to fault: the five memoized
// loaders (GetEffectiveGroups[ForAgent], ListRoleBindingsForPrincipals,
// GetRoleDefinitionsByIDs, ListAccessConstraints, GetDelegationEdgesForDelegate)
// plus the ceiling's own direct calls (GetRoleDefinition, GetUser, GetAgent,
// GetHubSetting), which the memo never serves (H1).
//
// fault, when non-nil, is consulted after the call is counted and (if
// recordCalls) recorded, and before delegating to the real store. Returning
// a non-nil error short-circuits the real call, exactly modeling a store
// fault; the wrapper never mutates or fabricates a success.
type memoTestStore struct {
	store.Store

	mu          sync.Mutex
	counts      map[string]int
	records     []storeCallRecord
	recordCalls bool
	fault       func(method string, n int, ctx context.Context) error

	// ignoreCancel implements H4 cancellation store variant (ii): the real
	// delegate call is made on a ctx with cancellation stripped, so the real
	// store answers as if the ctx were live even after the caller's ctx was
	// cancelled. Variant (i) needs no special handling here — a fault func
	// that checks ctx.Err() and returns a wrapped error is sufficient.
	ignoreCancel bool
}

func newMemoTestStore(s store.Store) *memoTestStore {
	return &memoTestStore{Store: s, counts: make(map[string]int)}
}

// errInjected is the sentinel fault error every row injects. Its identity is
// never asserted on directly — callers compare Error() strings or
// errors.Is(context.Canceled) — but tests use errors.Is against this
// sentinel to confirm the deny actually came from the injected fault and not
// some unrelated error.
var errInjected = errors.New("authz_request_inputs_parity_test: injected store fault")

// callNFault returns a fault func that fails a method's n-th call only.
func callNFault(method string, n int) func(string, int, context.Context) error {
	return func(m string, callN int, _ context.Context) error {
		if m == method && callN == n {
			return fmt.Errorf("injected fault on %s call #%d: %w", m, callN, errInjected)
		}
		return nil
	}
}

type parityRecordingAuditEmitter struct {
	mu      sync.Mutex
	records []*store.DecisionAuditRecord
}

// newRecordingAuthz builds a fresh AuthzService (H3) over s (typically a
// *memoTestStore) with a synchronous recording emitter and
// DecisionAuditSampleRate = 1.0, so every decision is audited and captured
// in call order.
func newRecordingAuthz(s store.Store) (*AuthzService, *parityRecordingAuditEmitter) {
	authz := NewAuthzService(s, slog.Default())
	emitter := &parityRecordingAuditEmitter{}
	authz.SetDecisionAuditEmitter(emitter)
	authz.DecisionAuditSampleRate = 1.0
	return authz, emitter
}

// assertAuditSequenceEqual compares two audit-record sequences field by
// field apart from Timestamp.
// extraMsg, when provided (a single, ALREADY-formatted string — callers
// that need their own format+args must fmt.Sprintf it themselves before
// calling), is appended to each per-record failure message.
//
// An earlier version took msgAndArgs ...interface{}, type-asserted the
// first element as a format string, and re-Sprintf'd it internally. That
// pattern passed an already-formatted string as testify's format string,
// with the caller's own msgAndArgs (itself a format string plus args) as
// the %-substitution values for it — producing garbled output like
// "audit record 3 mismatch%!(EXTRA string=...)" whenever a caller passed
// its own msgAndArgs. It also made go vet's printf analysis infer this
// function as a Printf-style wrapper over an unverifiable format string,
// flagging every call site that passed its own format+args. A plain
// extraMsg ...string parameter sidesteps both problems.
func assertAuditSequenceEqual(t *testing.T, ref, cand []*store.DecisionAuditRecord, extraMsg ...string) {
	t.Helper()
	if !assert.Equal(t, len(ref), len(cand), "audit record count mismatch") {
		return
	}
	var suffix string
	if len(extraMsg) > 0 && extraMsg[0] != "" {
		suffix = ": " + extraMsg[0]
	}
	for i := range ref {
		r, c := *ref[i], *cand[i]
		r.Timestamp, c.Timestamp = time.Time{}, time.Time{}
		assert.Equal(t, r, c, fmt.Sprintf("audit record %d mismatch%s", i, suffix))
	}
}

// assertDecisionsEqual is reflect.DeepEqual on the whole Decision,
// with a readable failure message.
func assertDecisionsEqual(t *testing.T, ref, cand Decision, msgAndArgs ...interface{}) {
	t.Helper()
	if !reflect.DeepEqual(ref, cand) {
		assert.Fail(t, fmt.Sprintf("decisions differ:\n  reference: %+v\n  candidate: %+v", ref, cand), msgAndArgs...)
	}
}

// newA1Fixture creates the fixture on s using name as a per-test ID salt, so
// distinct tests sharing one store (rare in this file; most build their own
// via authzTestSetup) do not collide.
func newA1Fixture(t *testing.T, s store.Store, name string) *a1Fixture {
	t.Helper()
	projectID := tid(name + "-project")
	otherProjID := tid(name + "-other-project")
	delegatorID := tid(name + "-delegator")
	agentID := tid(name + "-agent")
	targetID := tid(name + "-target")

	createDCProject(t, s, projectID, name+"-proj")
	createDCProject(t, s, otherProjID, name+"-other-proj")
	// A non-admin delegator with an in-scope ACTIVE project binding.
	createDCUser(t, s, delegatorID, name+"-delegator@test.com", projectID, store.ProjectRoleAdmin)
	createDCAgent(t, s, agentID, projectID, delegatorID, AgentRoleFull)
	createDCAgent(t, s, targetID, projectID, delegatorID, AgentRoleFull)
	createDCEdge(t, s, store.DelegationPrincipalUser, delegatorID, store.DelegationPrincipalAgent, agentID,
		store.RoleScopeProject, projectID, string(AgentRoleFull))

	return &a1Fixture{
		store:       s,
		projectID:   projectID,
		otherProjID: otherProjID,
		delegatorID: delegatorID,
		agentID:     agentID,
		targetID:    targetID,
		agent:       dcAgentIdentity(agentID, projectID, AgentRoleFull),
		resource:    Resource{Type: "agent", ID: targetID, ParentType: "project", ParentID: projectID, OwnerID: delegatorID},
	}
}

func newP1Fixture(t *testing.T, s store.Store, name string) *p1Fixture {
	t.Helper()
	ctx := context.Background()
	projectID := tid(name + "-project")
	userID := tid(name + "-user")
	ownerID := tid(name + "-owner")
	agentID := tid(name + "-agent")

	require.NoError(t, s.CreateProject(ctx, &store.Project{ID: projectID, Slug: name + "-proj", Name: name, OwnerID: ownerID}))
	require.NoError(t, s.CreateUser(ctx, &store.User{ID: userID, Email: name + "@test.com", DisplayName: name, Role: "member", Status: "active"}))

	group := &store.Group{ID: tid(name + "-group"), Name: name + " members", Slug: "project:" + projectID + ":members", GroupType: store.GroupTypeExplicit, ProjectID: projectID}
	require.NoError(t, s.CreateGroup(ctx, group))
	require.NoError(t, s.AddGroupMember(ctx, &store.GroupMember{GroupID: group.ID, MemberID: userID, MemberType: store.GroupMemberTypeUser, Role: store.GroupMemberRoleMember}))

	rd, err := s.GetRoleDefinitionByName(ctx, store.ProjectRoleMember, store.RoleScopeProject)
	require.NoError(t, err)
	_, err = s.CreateRoleBinding(ctx, &store.RoleBinding{
		RoleDefinitionID: rd.ID,
		PrincipalType:    "group",
		PrincipalID:      group.ID,
		ScopeType:        store.RoleScopeProject,
		ScopeID:          projectID,
		CreatedBy:        "test",
	})
	require.NoError(t, err)

	require.NoError(t, s.CreateAgent(ctx, &store.Agent{ID: agentID, Slug: name + "-agent", Name: name + "-agent", ProjectID: projectID, Phase: "running", OwnerID: ownerID, Ancestry: []string{ownerID}}))

	return &p1Fixture{
		store:     s,
		projectID: projectID,
		userID:    userID,
		groupID:   group.ID,
		user:      NewAuthenticatedUser(userID, name+"@test.com", name, "member", "api"),
		agentRes:  Resource{Type: "agent", ID: agentID, ParentType: "project", ParentID: projectID, OwnerID: ownerID},
	}
}

func newProjectPrincipalFixture(t *testing.T, s store.Store, name, roleName string) *projectPrincipalFixture {
	t.Helper()
	ctx := context.Background()
	projectID := tid(name + "-project")
	userID := tid(name + "-user")
	ownerID := tid(name + "-owner")
	agentID := tid(name + "-agent")

	require.NoError(t, s.CreateProject(ctx, &store.Project{ID: projectID, Slug: name + "-proj", Name: name, OwnerID: ownerID}))
	createDCUser(t, s, userID, name+"@test.com", projectID, roleName)
	require.NoError(t, s.CreateAgent(ctx, &store.Agent{ID: agentID, Slug: name + "-agent", Name: name + "-agent", ProjectID: projectID, Phase: "running", OwnerID: ownerID, Ancestry: []string{ownerID}}))

	return &projectPrincipalFixture{
		store:     s,
		projectID: projectID,
		userID:    userID,
		user:      NewAuthenticatedUser(userID, name+"@test.com", name, "member", "api"),
		agentRes:  Resource{Type: "agent", ID: agentID, ParentType: "project", ParentID: projectID, OwnerID: ownerID},
	}
}

// agentResourceTuples builds one tuple per ResourceActions["agent"] against
// res, in registry order.
func agentResourceTuples(res Resource) []rawTuple {
	out := make([]rawTuple, 0, len(ResourceActions["agent"]))
	for _, a := range ResourceActions["agent"] {
		out = append(out, rawTuple{res, a})
	}
	return out
}

// agentGrantableTuples returns a small, deliberately-chosen tuple set that a
// full-lifecycle agent (AgentRoleFull) actually passes the AK1 kernel for,
// so every decision reaches step 10 and the ceiling result is what
// determines Allowed. This matters because agent.read/agent.list/
// agent.update/agent.port_access/agent.stop_all/agent.message/
// agent.grant_hub_mode have no AgentScopes entry in the permissions
// registry at all (agents cannot pass the kernel for them via JWT scope,
// exactly as TestDelegationCeiling_UserAgentChain's comment documents for
// agent.read), and this fixture's target is owned by the delegator, not the
// agent, so no owner/ancestor relationship grant covers them either.
// project.read (AgentScopes: ["project:read"]) is the read-only permission
// AgentRoleFull actually holds; agent.delete/attach/lifecycle (AgentScopes:
// ["project:agent:lifecycle"]) are the non-read-only ones.
func agentGrantableTuples(f *a1Fixture) []rawTuple {
	// Deliberately unowned (no OwnerID) on every tuple's resource, both
	// here and below. A relationship-grant fallback for a user delegator
	// (userRelationshipAuthority) means an "owner" relationship on a
	// resource the delegator itself owns grants the permission
	// independent of any role-binding/role-definition resolution. Every
	// caller of this helper (A6, A7, A7', A8) targets the delegator's
	// ROLE-BASED permission check specifically (faulting GetRoleDefinition,
	// ListAccessConstraints or the edges lookup); an owned resource would
	// let the owner-relationship fallback silently paper over those
	// faults, making the row pass without the fault having been reached.
	// No caller relies on ownership, so every resource here is unowned —
	// f.resource itself carries OwnerID: f.delegatorID (newA1Fixture), so
	// tuples 1-3 use an explicit unowned copy, not f.resource directly.
	//
	// The fixture's non-admin ProjectRoleAdmin delegator holds project.read
	// and agent.lifecycle through its role binding, but NOT discrete
	// agent.delete/agent.attach permissions — those two are only reachable
	// here through the owner-relationship fallback, which the unowned
	// resource disables. So delete/attach deny via
	// DenyCauseCeilingDelegatorLacksPermission even with no fault at all;
	// read/lifecycle allow with no fault and only deny that way once the
	// fault is injected. See TestParity_A6_DelegatorRoleDefinitionFails for
	// why both shapes still belong in the same row.
	projectRes := Resource{Type: "project", ID: f.projectID}
	unownedResource := f.resource
	unownedResource.OwnerID = ""
	return []rawTuple{
		{projectRes, ActionRead},
		{unownedResource, ActionDelete},
		{unownedResource, ActionAttach},
		{unownedResource, ActionLifecycle},
	}
}

// failEffectiveGroupsStore fails GetEffectiveGroups (Step 2: principal
// resolution).
type failEffectiveGroupsStore struct {
	store.Store
	failErr error
}

// failBindingsStore fails ListRoleBindingsForPrincipals (Step 3: role-binding
// resolution).
type failBindingsStore struct {
	store.Store
	failErr error
}

// failRoleDefsStore fails GetRoleDefinitionsByIDs (Step 4: role-definition
// resolution). It only fires when there is at least one role-definition ID to
// load, matching loadRoleDefinitions's short-circuit on an empty ID list.
type failRoleDefsStore struct {
	store.Store
	failErr error
}

// failConstraintsStore fails ListAccessConstraints (Step 7c: access-constraint
// load), while principal and binding resolution succeed normally.
type failConstraintsStore struct {
	store.Store
	failErr error
}

// selfToken builds a token identity with the given boundary and selectors.
func selfToken(t *testing.T, userID string, boundary TokenBoundary, selectors ...string) *ScopedUserIdentity {
	t.Helper()
	return NewScopedUserIdentityWithBoundaryAndDecoration(bearerUser(userID), boundary, selectors, tid("self-cred-"+userID), bearerCeiling(t, selectors...), nil)
}

// authzTestSetup creates a test server with the authz service and pre-populated data.
// Note: testServer() removes the delegation edge backfill marker so that
// agents created directly via the store (without delegation edges) are
// not denied by the post-backfill no-edge check. Tests that specifically
// exercise post-backfill behavior re-create the marker explicitly.
func authzTestSetup(t *testing.T) (*AuthzService, store.Store) {
	t.Helper()
	srv, s := testServer(t)
	return srv.authzService, s
}

// createTestRoleDefinition creates a custom role definition for tests.
func createTestRoleDefinition(t *testing.T, s store.Store, name, scopeType string, permissions []string) *store.RoleDefinition {
	t.Helper()
	ctx := context.Background()
	rd, err := s.CreateRoleDefinition(ctx, &store.RoleDefinition{
		Name:        name,
		ScopeType:   scopeType,
		Permissions: permissions,
	})
	require.NoError(t, err)
	return rd
}

// errorInjectingStore wraps a real store and allows injecting errors into
// specific methods. All other methods delegate to the embedded store.
type errorInjectingStore struct {
	store.Store
	fault                         *storeFaultSwitch // nil: always active
	getEffectiveGroupsErr         error
	getEffectiveGroupsForAgentErr error
	getGroupMembersErr            error
}

func assertUnrecordedDeny(t *testing.T, d Decision) {
	t.Helper()
	require.False(t, d.Allowed, "reason %q", d.Reason)
	assert.Equal(t, DeniedByDelegationCeiling, d.DeniedBy, "reason %q", d.Reason)
	assert.Equal(t, DenyCauseCeilingUnrecorded, d.DenyCause, "reason %q", d.Reason)
}

// agentBaselineFixture is the shared world for the baseline tests: two
// projects, an agent in the first, and the implicit project_agents group for
// the first project (so that role bindings to that group resolve).
//
// CO1: The agent identity now carries baseline JWT scopes so the agent scope
// restriction has a real Check function. A project-scoped role binding
// grants the agent read+list permissions on agents and projects in its own
// project, replacing the old implicit read baseline.
type agentBaselineFixture struct {
	authz        *AuthzService
	store        store.Store
	ownProject   *store.Project
	otherProject *store.Project
	agent        *store.Agent
	agentsGroup  *store.Group
	identity     AgentIdentity
	readRoleDef  *store.RoleDefinition
}

// bearerFixture is two projects, each with its own owner, and an agent in
// each project owned by that project's owner.
type bearerFixture struct {
	srv      *Server
	store    store.Store
	projectA string
	projectB string
	ownerA   string
	ownerB   string
	agentA   *store.Agent
	agentB   *store.Agent
}

// token mints an agent JWT for the calling agent with the given scopes.
// ScopeProjectRead is always included because the baseline role grants it to
// every agent; without it, checkAgentReadScope rejects read requests before
// the handler-level authorization these tests are designed to exercise.
func (f *bypassAgentsFixture) token(t *testing.T, scopes ...AgentTokenScope) string {
	t.Helper()
	svc := f.srv.GetAgentTokenService()
	require.NotNil(t, svc)
	allScopes := append([]AgentTokenScope{ScopeProjectRead}, scopes...)
	tok, err := svc.GenerateAgentToken(f.caller.ID, f.caller.ProjectID, allScopes, nil)
	require.NoError(t, err)
	return tok
}

// asAgent issues a request carrying the calling agent's token.
func (f *bypassAgentsFixture) asAgent(t *testing.T, method, path string, body interface{}, scopes ...AgentTokenScope) *httptest.ResponseRecorder {
	t.Helper()
	return doRequestWithAgentToken(t, f.srv, method, path, body, f.token(t, scopes...))
}

// asAgentWithHeaders issues a request carrying the calling agent's token with additional headers.
func (f *bypassAgentsFixture) asAgentWithHeaders(t *testing.T, method, path string, body interface{}, headers map[string]string, scopes ...AgentTokenScope) *httptest.ResponseRecorder {
	t.Helper()
	var bodyBytes []byte
	if body != nil {
		var err error
		bodyBytes, err = json.Marshal(body)
		require.NoError(t, err)
	}
	req := httptest.NewRequest(method, path, bytes.NewReader(bodyBytes))
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("X-Scion-Agent-Token", f.token(t, scopes...))
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	f.srv.Handler().ServeHTTP(rec, req)
	return rec
}

// asBroker issues an HMAC-signed broker request. Brokers implement neither
// UserIdentity nor AgentIdentity, so before #591 they slipped through every one
// of these guards exactly as agents did.
func (f *bypassAgentsFixture) asBroker(t *testing.T, method, path string, body interface{}) *httptest.ResponseRecorder {
	t.Helper()
	var raw []byte
	if body != nil {
		var err error
		raw, err = json.Marshal(body)
		require.NoError(t, err)
	}
	req := httptest.NewRequest(method, path, bytes.NewReader(raw))
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	timestamp := strconv.FormatInt(time.Now().Unix(), 10)
	nonce := "bypass-nonce-" + uuid.New().String()
	req.Header.Set(HeaderBrokerID, f.broker.ID)
	req.Header.Set(HeaderTimestamp, timestamp)
	req.Header.Set(HeaderNonce, nonce)

	svc := f.srv.brokerAuthService
	require.NotNil(t, svc, "broker auth service must be configured for broker tests")
	mac := hmac.New(sha256.New, f.brokerSecret)
	mac.Write(svc.buildCanonicalString(req, timestamp, nonce))
	req.Header.Set(HeaderSignature, base64.StdEncoding.EncodeToString(mac.Sum(nil)))

	rec := httptest.NewRecorder()
	f.srv.Handler().ServeHTTP(rec, req)
	return rec
}

func (s *edgeLookupErrStore) GetDelegationEdgesForDelegate(ctx context.Context, delegateType, delegateID string) ([]*store.DelegationEdge, error) {
	if delegateID == s.failID {
		return nil, errors.New("injected delegation edge lookup fault")
	}
	return s.Store.GetDelegationEdgesForDelegate(ctx, delegateType, delegateID)
}

func (r stubSourceResolver) ResolveExecutionSource(context.Context, *store.Agent) (*store.User, error) {
	return r.user, r.err
}

// newDeliverRoleDefinition creates a minimal system-scope custom role
// naming only secret.deliver. The built-in super-admin role also holds it
// (through allPermissionIDs), but super-admin is direct-user-only
// (store/entadapter's directUserOnlyRoles) and cannot be bound to an agent
// or a group, so a role binding onto agent:<C> or a group needs its own
// role instead.
func newDeliverRoleDefinition(t *testing.T, s store.Store) *store.RoleDefinition {
	t.Helper()
	rd, err := s.CreateRoleDefinition(context.Background(), &store.RoleDefinition{
		Name:        "hd-role-only-deliver-" + tid(t.Name())[:8],
		Description: "holds secret.deliver only, for the role-does-not-substitute fixture",
		ScopeType:   store.RoleScopeSystem,
		Permissions: []string{"secret.deliver"},
	})
	require.NoError(t, err)
	return rd
}

// notAdmittedReasons are the deny reasons a request denied without ever
// being admitted can carry: the two Decide's entry classification check
// (ptone/scion#2123) produces for a supplied credential kind — one when the
// kind does not match the identity's own derived kind, the other when the
// kind is not recognized at all — plus deliveryGateReason, for a kind that
// matches the identity but sits outside the delivery set and so denies at
// the gate itself instead.
var notAdmittedReasons = []string{
	"credential kind does not match identity",
	"unrecognized credential kind",
	deliveryGateReason,
}

func (s *ambiguousEdgeStore) GetDelegationEdgesForDelegate(ctx context.Context, delegateType, delegateID string) ([]*store.DelegationEdge, error) {
	edges, err := s.Store.GetDelegationEdgesForDelegate(ctx, delegateType, delegateID)
	if err != nil || delegateID != s.dupID {
		return edges, err
	}
	out := make([]*store.DelegationEdge, 0, 2*len(edges))
	for _, e := range edges {
		cp := *e
		cp.ID = e.ID + "-dup"
		out = append(out, e, &cp)
	}
	return out, nil
}

// authz returns an AuthzService on store s with explicit flags.
func (f ceilingFixture) authz(s store.Store, devLocal, mintOverride bool) *AuthzService {
	a := NewAuthzService(s, slog.Default())
	a.setDevLocalAuthorityEnabled(devLocal)
	a.mintDevAuthOverride = mintOverride
	return a
}

func (f ceilingFixture) agent(t *testing.T, name string, role AgentRole) *store.Agent {
	t.Helper()
	id := tid("ec-agent-" + name)
	createDCAgent(t, f.store, id, f.projectID, f.userID, role)
	a, err := f.store.GetAgent(context.Background(), id)
	require.NoError(t, err)
	return a
}

func (f ceilingFixture) edge(t *testing.T, delegatorType, delegatorID, delegateID string, c store.EffectCeiling, p store.AuthorityProvenance) {
	t.Helper()
	require.NoError(t, f.store.CreateDelegationEdge(context.Background(), &store.DelegationEdge{
		DelegatorType:       delegatorType,
		DelegatorID:         delegatorID,
		DelegateType:        store.DelegationPrincipalAgent,
		DelegateID:          delegateID,
		ScopeType:           store.RoleScopeProject,
		ScopeID:             f.projectID,
		Role:                string(AgentRoleFull),
		Active:              true,
		AuthorityProvenance: p,
		EffectCeiling:       c,
	}))
}

// walkHop runs the step-10 walk for agentID in f's project scope and returns
// the outcome and cause.
func (f ceilingFixture) walkHop(t *testing.T, authz *AuthzService, agentID, permissionID string, resource Resource, action Action) (bool, DenyCause) {
	t.Helper()
	var cause DenyCause
	allowed, _, err := authz.walkDelegationChainWithCause(context.Background(), resource, action, permissionID, agentID,
		true, store.RoleScopeProject, f.projectID, nil, &cause)
	require.NoError(t, err)
	return allowed, cause
}

func (s *extraEdgeStore) GetDelegationEdgesForDelegate(ctx context.Context, delegateType, delegateID string) ([]*store.DelegationEdge, error) {
	edges, err := s.Store.GetDelegationEdgesForDelegate(ctx, delegateType, delegateID)
	if err != nil || delegateID != s.delegateID {
		return edges, err
	}
	return append(edges, s.extra), nil
}

// userChild creates an agent whose single edge is a recorded session edge
// from the fixture user.
func (f provenanceFixture) userChild(t *testing.T, name string, c store.EffectCeiling) *store.Agent {
	t.Helper()
	ag := f.agent(t, name, AgentRoleFull)
	f.edge(t, store.DelegationPrincipalUser, f.userID, ag.ID, c, recordedProv(store.DelegationPrincipalUser, f.userID, store.SourceCredentialSession))
	return ag
}

// agentChild creates an agent whose single edge is a recorded agent edge
// from parent.
func (f provenanceFixture) agentChild(t *testing.T, name string, parent *store.Agent, c store.EffectCeiling) *store.Agent {
	t.Helper()
	ag := f.agent(t, name, AgentRoleFull)
	f.edge(t, store.DelegationPrincipalAgent, parent.ID, ag.ID, c, recordedProv(store.DelegationPrincipalAgent, parent.ID, store.SourceCredentialAgent))
	return ag
}

func (f provenanceFixture) resolve(t *testing.T, agentID string) (RecordedProvenanceRoot, error) {
	t.Helper()
	return f.a.ResolveProvenanceRoot(context.Background(), agentID, ResolveProvenanceOptions{PermissionID: "agent.create"})
}

// devUserInProject creates the local development user as an owner of the
// fixture project with the given status.
func (f provenanceFixture) devUserInProject(t *testing.T, status string) {
	t.Helper()
	createDCUser(t, f.store, DevUserID, "dev@localhost", f.projectID, store.ProjectRoleOwner)
	setUserStatus(t, f.store, DevUserID, status)
}

func (s *faultStore) GetDelegationEdgesForDelegate(ctx context.Context, delegateType, delegateID string) ([]*store.DelegationEdge, error) {
	if s.edges {
		return nil, errProvenanceStoreFault
	}
	return s.Store.GetDelegationEdgesForDelegate(ctx, delegateType, delegateID)
}

func (s *faultStore) GetUser(ctx context.Context, id string) (*store.User, error) {
	if s.users && (s.userID == "" || s.userID == id) {
		return nil, errProvenanceStoreFault
	}
	return s.Store.GetUser(ctx, id)
}

func (s *faultStore) GetAgent(ctx context.Context, id string) (*store.Agent, error) {
	if s.agents {
		return nil, errProvenanceStoreFault
	}
	return s.Store.GetAgent(ctx, id)
}

func (s *federatedBindingStore) addBinding(rb *store.RoleBinding) *store.RoleBinding {
	s.mu.Lock()
	defer s.mu.Unlock()
	rb.ID = api.NewUUID()
	s.bindings = append(s.bindings, rb)
	return rb
}

func (s *federatedBindingStore) removeBinding(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	kept := s.bindings[:0]
	for _, b := range s.bindings {
		if b.ID != id {
			kept = append(kept, b)
		}
	}
	s.bindings = kept
}

func (s *federatedBindingStore) set(fn func(*federatedBindingStore)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	fn(s)
}

func (s *federatedBindingStore) GetUser(ctx context.Context, id string) (*store.User, error) {
	if !isFedTestPrincipal(id) {
		return s.Store.GetUser(ctx, id)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.failUser {
		return nil, errors.New("injected: user lookup failure")
	}
	if u, ok := s.users[id]; ok {
		cp := *u
		return &cp, nil
	}
	return nil, store.ErrNotFound
}

func (s *federatedBindingStore) GetEffectiveGroups(ctx context.Context, userID string) ([]string, error) {
	if !isFedTestPrincipal(userID) {
		return s.Store.GetEffectiveGroups(ctx, userID)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.failGroups {
		return nil, errors.New("injected: group resolution failure")
	}
	return append([]string(nil), s.groups[userID]...), nil
}

func (s *federatedBindingStore) ListRoleBindingsForPrincipals(ctx context.Context, principals []store.PrincipalRef, scopeTypes []string, scopeIDs []string) ([]*store.RoleBinding, error) {
	s.mu.Lock()
	fail := s.failBindings && len(principals) > 0 && isFedTestPrincipal(principals[0].ID)
	s.mu.Unlock()
	if fail {
		return nil, errors.New("injected: binding lookup failure")
	}
	var real []store.PrincipalRef
	wanted := map[string]bool{}
	for _, p := range principals {
		wanted[p.Type+":"+p.ID] = true
		if !isFedTestPrincipal(p.ID) {
			real = append(real, p)
		}
	}
	var out []*store.RoleBinding
	if len(real) > 0 {
		got, err := s.Store.ListRoleBindingsForPrincipals(ctx, real, scopeTypes, scopeIDs)
		if err != nil {
			return nil, err
		}
		out = append(out, got...)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, b := range s.bindings {
		if !wanted[b.PrincipalType+":"+b.PrincipalID] {
			continue
		}
		if len(scopeTypes) > 0 && !containsString(scopeTypes, b.ScopeType) {
			continue
		}
		if len(scopeIDs) > 0 && !containsString(scopeIDs, b.ScopeID) && b.ScopeType != store.RoleScopeSystem {
			continue
		}
		cp := *b
		out = append(out, &cp)
	}
	return out, nil
}

func (s *federatedBindingStore) ListAccessConstraints(ctx context.Context, limit, offset int) ([]*store.AccessConstraint, error) {
	s.mu.Lock()
	fail := s.failConstraints
	extra := append([]*store.AccessConstraint(nil), s.extraConstraints...)
	s.mu.Unlock()
	if fail {
		return nil, errors.New("injected: access constraint load failure")
	}
	page, err := s.Store.ListAccessConstraints(ctx, limit, offset)
	if err != nil {
		return nil, err
	}
	if offset == 0 {
		page = append(page, extra...)
	}
	return page, nil
}

// hubUser creates an active user with hub membership and no project binding.
func (f *rpaFixture) hubUser(t *testing.T, id string) {
	t.Helper()
	ctx := context.Background()
	require.NoError(t, f.store.CreateUser(ctx, &store.User{
		ID: id, Email: id + "@test.com", DisplayName: "User", Role: "member", Status: store.UserStatusActive,
	}))
	ensureHubMembership(ctx, f.store, id)
}

// expiringMember creates an active user whose only project binding is a
// project-member binding that expires at expiresAt.
func (f *rpaFixture) expiringMember(t *testing.T, id string, expiresAt time.Time) {
	t.Helper()
	ctx := context.Background()
	f.hubUser(t, id)
	rd, err := f.store.GetRoleDefinitionByName(ctx, store.ProjectRoleMember, store.RoleScopeProject)
	require.NoError(t, err)
	_, err = f.store.CreateRoleBinding(ctx, &store.RoleBinding{
		RoleDefinitionID: rd.ID, PrincipalType: store.RoleBindingPrincipalUser, PrincipalID: id,
		ScopeType: store.RoleScopeProject, ScopeID: f.projectID, ExpiresAt: &expiresAt, CreatedBy: "test",
	})
	require.NoError(t, err)
}

// clearBindings removes every role binding held directly by userID (the
// test server seeds system bindings for the dev user), so project access
// comes only from what the test grants.
func (f *rpaFixture) clearBindings(t *testing.T, userID string) {
	t.Helper()
	ctx := context.Background()
	bindings, err := f.store.ListRoleBindingsForPrincipal(ctx, store.RoleBindingPrincipalUser, userID)
	require.NoError(t, err)
	for _, b := range bindings {
		require.NoError(t, f.store.DeleteRoleBinding(ctx, b.ID))
	}
}

// evaluate runs Decide (with Explain) for the principal and, for rule, the
// stage directly. For a UAT it also runs the Decide step-1 bearer gate
// admission and asserts that the gate and the stage each read the store
// (fresh memos) and agree.
func (f *rpaFixture) evaluate(t *testing.T, kind rpaPrincipal, userID string, res Resource, perm string, rule RelationshipRuleID) rpaOutcome {
	t.Helper()
	ctx := context.Background()
	authz := f.srv.authzService
	ident := rpaIdentity(kind, userID, f.projectID)
	principal := principalContextForIdentity(ident)
	action, ok := registryActionFor(perm)
	require.True(t, ok)

	var out rpaOutcome
	out.decision = decidePerm(authz, ident, res, action, perm, true)

	before := f.counting.calls(userID)
	out.stageKind, _ = authz.relationshipProjectAccessStage(ctx, principal, res, perm, rule, &ProjectAdmissionCache{})
	stageReads := f.counting.calls(userID) - before

	if kind == rpaUAT {
		in, gated := bearerGateInputsFor(principal, credentialContextForIdentity(ident))
		require.True(t, gated, "UAT request must reach the bearer gate")
		before = f.counting.calls(userID)
		var trace bearerGateTrace
		out.gateDecision = authz.evaluateBearerGate(ctx, principal, in, res, TargetScopeEvidence{}, action, perm, &ProjectAdmissionCache{}, &trace)
		gateReads := f.counting.calls(userID) - before
		out.gateAdmitted = out.gateDecision == nil

		// Both checks ran (each made its own store read) and agree.
		assert.Positive(t, gateReads, "the step-1 admission must run")
		assert.Positive(t, stageReads, "the relationship stage must run")
		assert.Equal(t, out.gateAdmitted, out.stageKind == "",
			"step-1 admission (%v) and relationship stage (%q) must agree", out.gateDecision, out.stageKind)
	}
	return out
}

// messageAgent creates an agent in the fixture project with the given
// message mode, owned by the project owner, with ancestry.
func (f *rpaFixture) messageAgent(t *testing.T, suffix, mode string, ancestry ...string) *store.Agent {
	t.Helper()
	agent := &store.Agent{
		ID:          tid("rpa-msg-agent-" + suffix),
		Slug:        "rpa-msg-agent-" + suffix,
		Name:        "RPA Message Agent " + suffix,
		ProjectID:   f.projectID,
		OwnerID:     f.ownerID,
		Phase:       "stopped",
		Ancestry:    ancestry,
		MessageMode: mode,
	}
	require.NoError(t, f.store.CreateAgent(context.Background(), agent))
	return agent
}

func (f fakeProgenyAdapter) Kind() string              { return f.kind }
func (f fakeProgenyAdapter) ReadPermissions() []string { return f.perms }
func (f fakeProgenyAdapter) Sources(_ context.Context, q ProgenyQuery) ([]SharingSource, error) {
	if f.err != nil {
		return nil, f.err
	}
	var out []SharingSource
	for _, s := range f.sources {
		if q.ResourceID == "" || s.ID == q.ResourceID {
			out = append(out, s)
		}
	}
	return out, nil
}

// delegateCtx returns the ctx to use for the real store call: unchanged for
// H4 variant (i) (and for every non-cancellation row, where ignoreCancel is
// false), or with cancellation stripped for variant (ii).
func (m *memoTestStore) delegateCtx(ctx context.Context) context.Context {
	if m.ignoreCancel {
		return context.WithoutCancel(ctx)
	}
	return ctx
}

func (m *memoTestStore) call(ctx context.Context, method string) error {
	m.mu.Lock()
	m.counts[method]++
	n := m.counts[method]
	if m.recordCalls {
		m.records = append(m.records, storeCallRecord{
			method:        method,
			n:             n,
			hasInputMemo:  authzInputMemoFromContext(ctx) != nil,
			hasEdgesMemo:  delegationEdgesMemoFromContext(ctx) != nil,
			ceilingActive: getDelegationCeilingCache(ctx) != nil,
			stack:         callerFuncNames(),
		})
	}
	fault := m.fault
	m.mu.Unlock()
	if fault != nil {
		return fault(method, n, ctx)
	}
	return nil
}

func (m *memoTestStore) countOf(method string) int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.counts[method]
}

// snapshotCounts returns a point-in-time copy of every method's call count,
// for callers that need to diff counts across two points in a test (e.g.
// E6's post-cancellation per-method call-count comparison).
func (m *memoTestStore) snapshotCounts() map[string]int {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make(map[string]int, len(m.counts))
	for k, v := range m.counts {
		out[k] = v
	}
	return out
}

func (m *memoTestStore) recordedCalls() []storeCallRecord {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]storeCallRecord, len(m.records))
	copy(out, m.records)
	return out
}

func (m *memoTestStore) GetEffectiveGroups(ctx context.Context, userID string) ([]string, error) {
	if err := m.call(ctx, "GetEffectiveGroups"); err != nil {
		return nil, err
	}
	return m.Store.GetEffectiveGroups(m.delegateCtx(ctx), userID)
}

func (m *memoTestStore) GetEffectiveGroupsForAgent(ctx context.Context, agentID string) ([]string, error) {
	if err := m.call(ctx, "GetEffectiveGroupsForAgent"); err != nil {
		return nil, err
	}
	return m.Store.GetEffectiveGroupsForAgent(m.delegateCtx(ctx), agentID)
}

func (m *memoTestStore) ListRoleBindingsForPrincipals(ctx context.Context, principals []store.PrincipalRef, scopeTypes, scopeIDs []string) ([]*store.RoleBinding, error) {
	if err := m.call(ctx, "ListRoleBindingsForPrincipals"); err != nil {
		return nil, err
	}
	return m.Store.ListRoleBindingsForPrincipals(m.delegateCtx(ctx), principals, scopeTypes, scopeIDs)
}

func (m *memoTestStore) GetRoleDefinitionsByIDs(ctx context.Context, ids []string) (map[string]*store.RoleDefinition, error) {
	if err := m.call(ctx, "GetRoleDefinitionsByIDs"); err != nil {
		return nil, err
	}
	return m.Store.GetRoleDefinitionsByIDs(m.delegateCtx(ctx), ids)
}

func (m *memoTestStore) GetRoleDefinition(ctx context.Context, id string) (*store.RoleDefinition, error) {
	if err := m.call(ctx, "GetRoleDefinition"); err != nil {
		return nil, err
	}
	return m.Store.GetRoleDefinition(m.delegateCtx(ctx), id)
}

func (m *memoTestStore) ListAccessConstraints(ctx context.Context, limit, offset int) ([]*store.AccessConstraint, error) {
	if err := m.call(ctx, "ListAccessConstraints"); err != nil {
		return nil, err
	}
	return m.Store.ListAccessConstraints(m.delegateCtx(ctx), limit, offset)
}

func (m *memoTestStore) GetDelegationEdgesForDelegate(ctx context.Context, delegateType, delegateID string) ([]*store.DelegationEdge, error) {
	if err := m.call(ctx, "GetDelegationEdgesForDelegate"); err != nil {
		return nil, err
	}
	return m.Store.GetDelegationEdgesForDelegate(m.delegateCtx(ctx), delegateType, delegateID)
}

func (m *memoTestStore) GetUser(ctx context.Context, id string) (*store.User, error) {
	if err := m.call(ctx, "GetUser"); err != nil {
		return nil, err
	}
	return m.Store.GetUser(m.delegateCtx(ctx), id)
}

func (m *memoTestStore) GetAgent(ctx context.Context, id string) (*store.Agent, error) {
	if err := m.call(ctx, "GetAgent"); err != nil {
		return nil, err
	}
	return m.Store.GetAgent(m.delegateCtx(ctx), id)
}

func (m *memoTestStore) GetHubSetting(ctx context.Context, section string) (*store.HubSetting, error) {
	if err := m.call(ctx, "GetHubSetting"); err != nil {
		return nil, err
	}
	return m.Store.GetHubSetting(m.delegateCtx(ctx), section)
}

func (r *parityRecordingAuditEmitter) EmitDecisionAudit(_ context.Context, record *store.DecisionAuditRecord) {
	r.mu.Lock()
	defer r.mu.Unlock()
	cp := *record
	r.records = append(r.records, &cp)
}

func (r *parityRecordingAuditEmitter) snapshot() []*store.DecisionAuditRecord {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]*store.DecisionAuditRecord, len(r.records))
	copy(out, r.records)
	return out
}

// projectPrincipalFixture is a project-scoped principal reached through a
// direct (non-group) role binding, for P2 (member), P4 (admin) and similar
// rows that don't need the group-closure shape P1 exercises.
type projectPrincipalFixture struct {
	store     store.Store
	projectID string
	userID    string
	user      UserIdentity
	agentRes  Resource
}

func (s *failEffectiveGroupsStore) GetEffectiveGroups(ctx context.Context, userID string) ([]string, error) {
	return nil, s.failErr
}

func (s *failBindingsStore) ListRoleBindingsForPrincipals(ctx context.Context, principals []store.PrincipalRef, scopeTypes []string, scopeIDs []string) ([]*store.RoleBinding, error) {
	return nil, s.failErr
}

func (s *failRoleDefsStore) GetRoleDefinitionsByIDs(ctx context.Context, ids []string) (map[string]*store.RoleDefinition, error) {
	return nil, s.failErr
}

func (s *failConstraintsStore) ListAccessConstraints(ctx context.Context, limit, offset int) ([]*store.AccessConstraint, error) {
	return nil, s.failErr
}

func (s *errorInjectingStore) GetEffectiveGroups(ctx context.Context, userID string) ([]string, error) {
	if s.getEffectiveGroupsErr != nil && s.fault.Active() {
		return nil, s.getEffectiveGroupsErr
	}
	return s.Store.GetEffectiveGroups(ctx, userID)
}

func (s *errorInjectingStore) GetEffectiveGroupsForAgent(ctx context.Context, agentID string) ([]string, error) {
	if s.getEffectiveGroupsForAgentErr != nil && s.fault.Active() {
		return nil, s.getEffectiveGroupsForAgentErr
	}
	return s.Store.GetEffectiveGroupsForAgent(ctx, agentID)
}

func (s *errorInjectingStore) GetGroupMembers(ctx context.Context, groupID string) ([]store.GroupMember, error) {
	if s.getGroupMembersErr != nil && s.fault.Active() {
		return nil, s.getGroupMembersErr
	}
	return s.Store.GetGroupMembers(ctx, groupID)
}

func isFedTestPrincipal(id string) bool { return strings.HasPrefix(id, fedTestIssuer+":") }

// newHubDeliveryTestAgent creates a store.Agent in projectID, descending
// from ownerID, for a hubDeliveryIdentity built through the real
// constructor. It also records the active user-to-agent delegation edge in
// projectID, so the execution-project relationship stage resolves ownerID
// as the agent's authoritative source user and later stages decide the
// outcome. ownerID must be a stored user admitted to projectID.
func newHubDeliveryTestAgent(t *testing.T, s store.Store, agentID, projectID, ownerID string) {
	t.Helper()
	require.NoError(t, s.CreateAgent(context.Background(), &store.Agent{
		ID: agentID, Slug: "slug-" + agentID[:8], Name: "name-" + agentID[:8],
		ProjectID: projectID, Phase: string(state.PhaseRunning),
		OwnerID: ownerID, CreatedBy: ownerID, Ancestry: []string{ownerID},
	}))
	// A recorded edge (session provenance, principal ceiling): delivery
	// permissions require recorded provenance on every hop, so an edge
	// without provenance would deny the ordinary-proof controls below.
	seedRecordedDelegationEdge(t, s, store.DelegationPrincipalUser, ownerID, store.DelegationPrincipalAgent, agentID,
		store.RoleScopeProject, projectID, string(AgentRoleFull))
}

type parentCeilingFixture struct {
	authz     *AuthzService
	store     store.Store
	projectID string
	userID    string
}

var errProvenanceStoreFault = errors.New("store fault")

const fedTestIssuer = "https://idp.example"

// installAgentOwnerOverride wraps the server store and the authorization
// store's base with one agentOwnerOverrideStore map, so handlers and
// authorization agree on the agent's owner. base is the store the
// authorization service should read through underneath the override.
func installAgentOwnerOverride(t *testing.T, srv *Server) *agentOwnerOverrideStore {
	t.Helper()
	mu := &sync.Mutex{}
	owners := map[string]string{}
	origSrv := srv.store
	srv.store = &agentOwnerOverrideStore{Store: origSrv, mu: mu, owners: owners}
	t.Cleanup(func() { srv.store = origSrv })
	origAuthz := srv.authzService.store
	override := &agentOwnerOverrideStore{Store: origAuthz, mu: mu, owners: owners}
	srv.authzService.store = override
	t.Cleanup(func() { srv.authzService.store = origAuthz })
	return override
}

// fedFixture is an rpaFixture whose authorization service reads through a
// federatedBindingStore, and whose stored agents can report a federated
// owner.
type fedFixture struct {
	*rpaFixture
	fed    *federatedBindingStore
	owners *agentOwnerOverrideStore
}

// rpaStore wraps the server store, counts GetUser calls per user ID, and can
// inject a GetUser fault for one user ID. GetUser is the first store read of
// ProjectMembershipEvidence (requireActiveUser), so a fault there is a real
// store fault inside the admission lookup.
type rpaStore struct {
	store.Store
	mu              sync.Mutex
	getUserCalls    map[string]int
	failGetUserFor  string
	failBindingsFor string
}

// rpaPrincipal names the two principal classes every row runs for.
type rpaPrincipal string

const (
	rpaInteractive rpaPrincipal = "interactive"
	rpaUAT         rpaPrincipal = "uat"
)

func rpaIdentity(kind rpaPrincipal, userID, projectID string) Identity {
	switch kind {
	case rpaUAT:
		return NewScopedUserIdentity(NewAuthenticatedUser(userID, userID+"@test.com", "User", "member", "api"), projectID, rpaUATScopes)
	default:
		return NewAuthenticatedUser(userID, userID+"@test.com", "User", "member", "web")
	}
}

// rpaOutcome is what one row evaluation observed.
type rpaOutcome struct {
	decision Decision
	// stageKind is the direct stage result for rule ("" = passes).
	stageKind string
	// gateAdmitted is the Decide step-1 bearer gate result (UAT only).
	gateAdmitted bool
	gateDecision *Decision
}

// storeCallRecord captures one intercepted call for X6/X7-style assertions
// that a call did or did not observe an input/edges memo, or run under the
// per-decision delegation ceiling cache.
type storeCallRecord struct {
	method        string
	n             int // 1-based call number for this method on this wrapper
	hasInputMemo  bool
	hasEdgesMemo  bool
	ceilingActive bool
	// stack holds the fully qualified function names on the calling
	// goroutine's stack at the time of the call, innermost first. It is
	// captured only when recordCalls is set.
	stack []string
}

// a1Fixture builds the A1 row's fixture and is reused, unmodified, by every
// A-row and by X7: an agent principal in its own project, with lifecycle
// scopes (AgentRoleFull), and a single-level delegation edge to a
// **non-admin** user delegator who holds an **in-scope active project
// binding** (ProjectRoleAdmin, not a system role) — required so that
// IsSystemAdmin does not short-circuit (match by name, not line number,
// in authz_delegation_ceiling.go) and getEffectivePermissions loads
// constraints, which A7/A7'/X7 all
// depend on.
type a1Fixture struct {
	store       store.Store
	projectID   string
	otherProjID string
	delegatorID string
	agentID     string
	targetID    string // an agent resource in the agent's own project
	agent       AgentIdentity
	resource    Resource
}

// p1Fixture is an ordinary hub member reached only through a group binding
// (P1): a "members" group holding a project role binding, with the user a
// member of that group and nothing else. Also serves E1-E5b, whose rows
// pin the principal to "P1, a user, so there is no delegation ceiling."
type p1Fixture struct {
	store     store.Store
	projectID string
	userID    string
	groupID   string
	user      UserIdentity
	agentRes  Resource // an agent resource in the member's project
}

// rawTuple is one (resource, action) pair evaluated through CheckAccess.
type rawTuple struct {
	resource Resource
	action   Action
}

// chain stores agents ids[0..n-1] in the fixture project: the user
// delegates to ids[0] and each agent delegates to the next.
func (f parentCeilingFixture) chain(t *testing.T, ids ...string) {
	t.Helper()
	prev, prevType := f.userID, store.DelegationPrincipalUser
	for _, id := range ids {
		createDCAgent(t, f.store, id, f.projectID, prev, AgentRoleFull)
		seedRecordedDelegationEdge(t, f.store, prevType, prev, store.DelegationPrincipalAgent, id,
			store.RoleScopeProject, f.projectID, string(AgentRoleFull))
		prev, prevType = id, store.DelegationPrincipalAgent
	}
}

func (f parentCeilingFixture) agent(id string) AgentIdentity {
	return dcAgentIdentity(id, f.projectID, AgentRoleFull)
}

func (f parentCeilingFixture) projectRead(t *testing.T, id string) Decision {
	t.Helper()
	return decidePerm(f.authz, f.agent(id), Resource{Type: "project", ID: f.projectID}, ActionRead, "project.read", false)
}

func (f parentCeilingFixture) agentCreate(t *testing.T, id string) Decision {
	t.Helper()
	return decidePerm(f.authz, f.agent(id), Resource{Type: "agent", ParentType: "project", ParentID: f.projectID}, ActionCreate, "agent.create", false)
}

// agent stores a project agent reported as owned by ownerID.
func (f *fedFixture) agent(t *testing.T, suffix, ownerID string) *store.Agent {
	t.Helper()
	return storeFedAgent(t, f.store, f.owners, f.projectID, f.ownerID, suffix, ownerID, "")
}

// accessOnlyRoleID returns the ID of a project role with no permissions, so
// a binding to it is project access without granting anything itself.
func (f *fedFixture) accessOnlyRoleID(t *testing.T) string {
	t.Helper()
	ctx := context.Background()
	name := "relchar-access-only"
	rd, err := f.store.GetRoleDefinitionByName(ctx, name, store.RoleScopeProject)
	if err != nil {
		require.ErrorIs(t, err, store.ErrNotFound)
		rd, err = f.store.CreateRoleDefinition(ctx, &store.RoleDefinition{
			ID: api.NewUUID(), Name: name, Description: "project access, no permissions",
			ScopeType: store.RoleScopeProject, Permissions: []string{},
		})
		require.NoError(t, err)
	}
	return rd.ID
}

// grantFed records an access-only project binding keyed to
// user:<issuer>:<sub> on projectID.
func (f *fedFixture) grantFed(t *testing.T, principalID, projectID string, mutate func(*store.RoleBinding)) *store.RoleBinding {
	t.Helper()
	rb := &store.RoleBinding{
		RoleDefinitionID: f.accessOnlyRoleID(t), PrincipalType: store.RoleBindingPrincipalUser, PrincipalID: principalID,
		ScopeType: store.RoleScopeProject, ScopeID: projectID, CreatedBy: "test",
	}
	if mutate != nil {
		mutate(rb)
	}
	return f.fed.addBinding(rb)
}

// grantFedSuperAdmin records a system-scope super-admin binding keyed to
// user:<issuer>:<sub>.
func (f *fedFixture) grantFedSuperAdmin(t *testing.T, principalID string) *store.RoleBinding {
	t.Helper()
	rd, err := f.store.GetRoleDefinitionByName(context.Background(), store.SystemRoleSuperAdmin, store.RoleScopeSystem)
	require.NoError(t, err)
	return f.fed.addBinding(&store.RoleBinding{
		RoleDefinitionID: rd.ID, PrincipalType: store.RoleBindingPrincipalUser, PrincipalID: principalID,
		ScopeType: store.RoleScopeSystem, CreatedBy: store.SystemReconcileCreatedBy,
	})
}

// evaluate runs Decide (with Explain), the stage directly for rule, and
// ProjectTargetAdmission, each with a fresh memo.
func (f *fedFixture) evaluate(t *testing.T, ident Identity, res Resource, perm string, rule RelationshipRuleID) fedOutcome {
	t.Helper()
	ctx := context.Background()
	authz := f.srv.authzService
	principal := principalContextForIdentity(ident)
	require.Equal(t, PrincipalKindFederatedUser, principal.Kind)
	action, ok := registryActionFor(perm)
	require.True(t, ok)

	var out fedOutcome
	out.decision = decidePerm(authz, ident, res, action, perm, true)
	out.stageKind, _ = authz.relationshipProjectAccessStage(ctx, principal, res, perm, rule, &ProjectAdmissionCache{})
	out.admission, out.admErr = authz.ProjectTargetAdmission(ctx, principal, resourceProjectScope(res), perm, res, &ProjectAdmissionCache{})
	return out
}

// ListRoleBindingsForPrincipals fails when the closure's first (direct)
// principal is failBindingsFor: a binding lookup fault, a fault class
// distinct from the GetUser fault.
func (s *rpaStore) ListRoleBindingsForPrincipals(ctx context.Context, principals []store.PrincipalRef, scopeTypes []string, scopeIDs []string) ([]*store.RoleBinding, error) {
	s.mu.Lock()
	fail := s.failBindingsFor != "" && len(principals) > 0 && principals[0].ID == s.failBindingsFor
	s.mu.Unlock()
	if fail {
		return nil, errors.New("injected: binding lookup failure")
	}
	return s.Store.ListRoleBindingsForPrincipals(ctx, principals, scopeTypes, scopeIDs)
}

func (s *rpaStore) setBindingFault(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.failBindingsFor = id
}

func (s *rpaStore) GetUser(ctx context.Context, id string) (*store.User, error) {
	s.mu.Lock()
	s.getUserCalls[id]++
	fail := s.failGetUserFor != "" && s.failGetUserFor == id
	s.mu.Unlock()
	if fail {
		return nil, errors.New("injected: user lookup failure")
	}
	return s.Store.GetUser(ctx, id)
}

func (s *rpaStore) calls(id string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.getUserCalls[id]
}

func (s *rpaStore) setFault(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.failGetUserFor = id
}

// rpaUATScopes is the ceiling of the UAT used by every row: it covers the
// permissions the rows evaluate, so only live authority decides.
var rpaUATScopes = []string{"agent:attach", "agent:read"}

// agentOwnerOverrideStore is a test store wrapper that reports a federated
// principal as the owner of a stored agent. The persistent store keeps an
// agent's owner as a users row ID, so an agent owned by <issuer>:<sub>
// cannot be stored today (see ptone/scion#3427). The agent is stored with a
// local owner and GetAgent reports the federated owner instead.
type agentOwnerOverrideStore struct {
	store.Store
	mu     *sync.Mutex
	owners map[string]string // agent ID -> owner principal ID
}

// storeFedAgent stores an agent in projectID with a local owner and records
// ownerID (a federated principal) as its reported owner. It returns the
// agent as the handlers and authorization see it.
func storeFedAgent(t *testing.T, s store.Store, o *agentOwnerOverrideStore, projectID, localOwnerID, suffix, ownerID, mode string) *store.Agent {
	t.Helper()
	agent := &store.Agent{
		ID: tid("fed-agent-" + suffix), Slug: "fed-agent-" + suffix, Name: "Fed Agent " + suffix,
		ProjectID: projectID, OwnerID: localOwnerID, Phase: "stopped", MessageMode: mode,
	}
	require.NoError(t, s.CreateAgent(context.Background(), agent))
	o.mu.Lock()
	o.owners[agent.ID] = ownerID
	o.mu.Unlock()
	cp := *agent
	cp.OwnerID = ownerID
	return &cp
}

// fedOutcome is what one federated evaluation observed.
type fedOutcome struct {
	decision  Decision
	stageKind string
	admission ProjectAdmissionResult
	admErr    error
}

func (s *agentOwnerOverrideStore) GetAgent(ctx context.Context, id string) (*store.Agent, error) {
	a, err := s.Store.GetAgent(ctx, id)
	if err != nil || a == nil {
		return a, err
	}
	s.mu.Lock()
	owner, ok := s.owners[id]
	s.mu.Unlock()
	if ok {
		cp := *a
		cp.OwnerID = owner
		return &cp, nil
	}
	return a, nil
}
