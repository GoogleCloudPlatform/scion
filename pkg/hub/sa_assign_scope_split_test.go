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
	"slices"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/go-jose/go-jose/v4/jwt"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Tests for ptone/scion#2339: gcp_service_account.assign has its own agent
// scope, project:agent:sa_assign, decided independently of agent.create's
// project:agent:create. These tests name the rule the split establishes, not
// a historical gap.

// TestAgentScopesToPermissionIDs_CreateAndAssignDecidedIndependently is the
// unit-level pin on agentScopesToPermissionIDs: each of the two permissions
// is granted only by its own scope, except for a scope-schema-0 token, where
// the combined project:agent:create scope keeps granting both (the
// compatibility rule for a token minted before the split).
func TestAgentScopesToPermissionIDs_CreateAndAssignDecidedIndependently(t *testing.T) {
	const (
		createPerm = "agent.create"
		assignPerm = "gcp_service_account.assign"
	)

	tests := []struct {
		name        string
		scopes      []AgentTokenScope
		scopeSchema int
		wantCreate  bool
		wantAssign  bool
	}{
		{
			name:        "neither scope grants neither permission",
			scopes:      []AgentTokenScope{ScopeProjectRead},
			scopeSchema: CurrentAgentScopeSchema,
		},
		{
			name:        "create scope alone grants create, not assign",
			scopes:      []AgentTokenScope{ScopeAgentCreate},
			scopeSchema: CurrentAgentScopeSchema,
			wantCreate:  true,
		},
		{
			name:        "assign scope alone grants assign, not create",
			scopes:      []AgentTokenScope{ScopeAgentSAAssign},
			scopeSchema: CurrentAgentScopeSchema,
			wantAssign:  true,
		},
		{
			name:        "both scopes grant both permissions",
			scopes:      []AgentTokenScope{ScopeAgentCreate, ScopeAgentSAAssign},
			scopeSchema: CurrentAgentScopeSchema,
			wantCreate:  true,
			wantAssign:  true,
		},
		{
			name:        "scope-schema-0 token: create scope alone still grants assign",
			scopes:      []AgentTokenScope{ScopeAgentCreate},
			scopeSchema: 0,
			wantCreate:  true,
			wantAssign:  true,
		},
		{
			name:        "scope-schema-0 token with neither scope grants neither permission",
			scopes:      []AgentTokenScope{ScopeProjectRead},
			scopeSchema: 0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ids := agentScopesToPermissionIDs(tt.scopes, tt.scopeSchema)
			assert.Equal(t, tt.wantCreate, slices.Contains(ids, createPerm), "%s grant", createPerm)
			assert.Equal(t, tt.wantAssign, slices.Contains(ids, assignPerm), "%s grant", assignPerm)
		})
	}
}

// scopeSplitFixture is the shared world for the CheckAccess-level scope-split
// tests: one project and one service account inside it, so a test only has
// to vary the calling agent's scopes and scope schema.
type scopeSplitFixture struct {
	authz     *AuthzService
	projectID string
	sa        Resource
}

func newScopeSplitFixture(t *testing.T) *scopeSplitFixture {
	t.Helper()
	authz, s := authzTestSetup(t)
	ctx := context.Background()

	projectID := tid("scopesplit-project")
	require.NoError(t, s.CreateProject(ctx, &store.Project{
		ID: projectID, Name: "Scope Split Project", Slug: "scopesplit-project",
	}))

	return &scopeSplitFixture{
		authz:     authz,
		projectID: projectID,
		sa:        projectSA(t, tid("scopesplit-sa"), projectID),
	}
}

// identity builds an agent identity in the fixture's project with exactly
// the given scopes and scope schema — never derived from ScopesForRole, so
// the four scope combinations and both schema values are all reachable
// regardless of which real AgentRole would produce them.
func (f *scopeSplitFixture) identity(agentID string, scopes []AgentTokenScope, scopeSchema int) AgentIdentity {
	return &agentIdentityWrapper{&AgentTokenClaims{
		Claims:      jwt.Claims{Subject: agentID},
		ProjectID:   f.projectID,
		Scopes:      scopes,
		ScopeSchema: scopeSchema,
	}}
}

func (f *scopeSplitFixture) agentCreateResource() Resource {
	return Resource{Type: "agent", ParentType: "project", ParentID: f.projectID}
}

// TestAuthz_AgentScopeSplit_CreateWithoutAssign covers a post-split token
// (current scope schema) holding only project:agent:create: agent.create is
// granted, gcp_service_account.assign is not.
func TestAuthz_AgentScopeSplit_CreateWithoutAssign(t *testing.T) {
	f := newScopeSplitFixture(t)
	ctx := context.Background()
	identity := f.identity(tid("scopesplit-create-only"), []AgentTokenScope{ScopeAgentCreate}, CurrentAgentScopeSchema)

	createDecision := f.authz.CheckAccess(ctx, identity, f.agentCreateResource(), ActionCreate)
	assert.True(t, createDecision.Allowed, "agent.create must be granted: %q", createDecision.Reason)

	assignDecision := f.authz.CheckAccess(ctx, identity, f.sa, ActionAssign)
	assert.False(t, assignDecision.Allowed,
		"gcp_service_account.assign must not be granted by project:agent:create alone on a post-split token")
}

// TestAuthz_AgentScopeSplit_AssignWithoutCreate covers a post-split token
// holding only project:agent:sa_assign: gcp_service_account.assign is
// granted, agent.create is not.
func TestAuthz_AgentScopeSplit_AssignWithoutCreate(t *testing.T) {
	f := newScopeSplitFixture(t)
	ctx := context.Background()
	identity := f.identity(tid("scopesplit-assign-only"), []AgentTokenScope{ScopeAgentSAAssign}, CurrentAgentScopeSchema)

	assignDecision := f.authz.CheckAccess(ctx, identity, f.sa, ActionAssign)
	assert.True(t, assignDecision.Allowed, "gcp_service_account.assign must be granted: %q", assignDecision.Reason)

	createDecision := f.authz.CheckAccess(ctx, identity, f.agentCreateResource(), ActionCreate)
	assert.False(t, createDecision.Allowed,
		"agent.create must not be granted by project:agent:sa_assign alone")
}

// TestAuthz_AgentScopeSplit_Both covers a token holding both scopes: both
// permissions are granted.
func TestAuthz_AgentScopeSplit_Both(t *testing.T) {
	f := newScopeSplitFixture(t)
	ctx := context.Background()
	identity := f.identity(tid("scopesplit-both"),
		[]AgentTokenScope{ScopeAgentCreate, ScopeAgentSAAssign}, CurrentAgentScopeSchema)

	createDecision := f.authz.CheckAccess(ctx, identity, f.agentCreateResource(), ActionCreate)
	assert.True(t, createDecision.Allowed, "agent.create must be granted: %q", createDecision.Reason)

	assignDecision := f.authz.CheckAccess(ctx, identity, f.sa, ActionAssign)
	assert.True(t, assignDecision.Allowed, "gcp_service_account.assign must be granted: %q", assignDecision.Reason)
}

// TestAuthz_AgentScopeSplit_Neither covers a token holding neither scope:
// neither permission is granted.
func TestAuthz_AgentScopeSplit_Neither(t *testing.T) {
	f := newScopeSplitFixture(t)
	ctx := context.Background()
	identity := f.identity(tid("scopesplit-neither"), []AgentTokenScope{ScopeProjectRead}, CurrentAgentScopeSchema)

	createDecision := f.authz.CheckAccess(ctx, identity, f.agentCreateResource(), ActionCreate)
	assert.False(t, createDecision.Allowed, "agent.create must not be granted")

	assignDecision := f.authz.CheckAccess(ctx, identity, f.sa, ActionAssign)
	assert.False(t, assignDecision.Allowed, "gcp_service_account.assign must not be granted")
}

// TestAuthz_AgentScopeSplit_PreSplitTokenKeepsAssign pins the compatibility
// rule: an agent JWT minted before the split (scope schema 0, the Go zero
// value for a token that predates AgentTokenClaims.ScopeSchema) that holds
// only the combined project:agent:create scope keeps authorizing
// gcp_service_account.assign, exactly as it did when it was minted.
func TestAuthz_AgentScopeSplit_PreSplitTokenKeepsAssign(t *testing.T) {
	f := newScopeSplitFixture(t)
	ctx := context.Background()
	identity := f.identity(tid("scopesplit-presplit"), []AgentTokenScope{ScopeAgentCreate}, 0)

	decision := f.authz.CheckAccess(ctx, identity, f.sa, ActionAssign)
	assert.True(t, decision.Allowed,
		"a scope-schema-0 token holding project:agent:create must keep authorizing gcp_service_account.assign: %q",
		decision.Reason)
}

// TestAuthz_AgentScopeSplit_PostSplitTokenDoesNotGainAssign is
// TestAuthz_AgentScopeSplit_PreSplitTokenKeepsAssign's mirror, and the
// property that makes the split real rather than cosmetic: once a token
// carries the current scope schema, holding project:agent:create alone is
// no longer enough for gcp_service_account.assign.
func TestAuthz_AgentScopeSplit_PostSplitTokenDoesNotGainAssign(t *testing.T) {
	f := newScopeSplitFixture(t)
	ctx := context.Background()
	identity := f.identity(tid("scopesplit-postsplit"), []AgentTokenScope{ScopeAgentCreate}, CurrentAgentScopeSchema)

	decision := f.authz.CheckAccess(ctx, identity, f.sa, ActionAssign)
	assert.False(t, decision.Allowed,
		"a current-scope-schema token holding only project:agent:create must not gain gcp_service_account.assign")
}

// TestAgentScopeSchema_NonHubJWTIdentityGetsNoLegacyGrant pins the
// discriminator's edge case: an identity this hub's own signer never issued
// (a federated agent, here) must not receive legacyAgentScopeGrants's
// compatibility grant merely for holding the combined scope literal. It
// resolves to CurrentAgentScopeSchema, never to schema 0 — schema 0 is
// reserved for an actual pre-split token this hub signed.
//
// In today's Decide pipeline this is additionally moot in practice:
// buildAgentSyntheticBindings is only reached when agent.ProjectID() != ""
// (authz.go step 5b), and FederatedAgentIdentity.ProjectID() always returns
// "", so a federated agent can never reach gcp_service_account.assign this
// way regardless of scope or schema. This test pins the discriminator
// function directly so the rule holds independently of that other gate.
func TestAgentScopeSchema_NonHubJWTIdentityGetsNoLegacyGrant(t *testing.T) {
	federated := NewFederatedAgentIdentity(
		"https://issuer.example", "remote-agent", "proj-x", "Remote Agent", "root-user",
		nil, []AgentTokenScope{ScopeAgentCreate})

	schema := agentScopeSchema(federated)
	assert.Equal(t, CurrentAgentScopeSchema, schema,
		"a non-hub-JWT identity must resolve to the current scope schema, never schema 0")

	ids := agentScopesToPermissionIDs(federated.Scopes(), schema)
	assert.NotContains(t, ids, "gcp_service_account.assign",
		"a non-hub-JWT identity holding only the combined create scope must not receive the compatibility assign grant")
	assert.Contains(t, ids, "agent.create")
}

// --- evaluateSAAssignment-level tests (the actual SA-assign gate entry
// point, not just CheckAccess) -----------------------------------------

// scopeSplitGateFixture is the shared world for evaluateSAAssignment-level
// scope-split tests: a project and a verified, project-scoped service
// account inside it, on a server with SA-assign checking off (Layer 2's
// actAs check is disabled and always allows), so a test result reflects
// Layer 1 (Hub policy / agent scope) alone. The delegation-edge backfill
// marker is absent by default (testServer), so Step 10's delegation ceiling
// takes its pre-backfill "allow" branch for these hub-attested agents rather
// than requiring a delegation edge — the same setup
// authz_agent_assign_baseline_test.go's fixture relies on.
type scopeSplitGateFixture struct {
	srv       *Server
	store     store.Store
	projectID string
	sa        *store.GCPServiceAccount
}

func newScopeSplitGateFixture(t *testing.T) *scopeSplitGateFixture {
	t.Helper()
	srv, s := testServer(t)

	projectID := tid("scopesplit-gate-project")
	createDCProject(t, s, projectID, "scopesplit-gate-project")

	return &scopeSplitGateFixture{
		srv:       srv,
		store:     s,
		projectID: projectID,
		sa:        scaCreateSA(t, s, projectID),
	}
}

// agentContext creates a store.Agent record for agentID (callerPrincipal,
// which evaluateSAAssignment's Layer 2 calls unconditionally, resolves the
// caller by loading this record) and returns a context carrying an agent
// identity with exactly the given scopes and scope schema. The agent record
// carries its own GCP identity in assign mode so callerPrincipal populates a
// ServiceAccountEmail — otherwise Layer 2 denies with "no caller identity"
// before Layer 1's scope check (the thing these tests exercise) is ever
// reached, and with saAssignCheckMode off (the fixture's default), that
// identity is never actually verified against GCP, only required to be
// present.
func (f *scopeSplitGateFixture) agentContext(t *testing.T, agentID string, scopes []AgentTokenScope, scopeSchema int) context.Context {
	t.Helper()
	require.NoError(t, f.store.CreateAgent(context.Background(), &store.Agent{
		ID:        agentID,
		Slug:      "slug-" + agentID,
		Name:      "name-" + agentID,
		ProjectID: f.projectID,
		Phase:     "running",
		AppliedConfig: &store.AgentAppliedConfig{
			GCPIdentity: &store.GCPIdentityConfig{
				MetadataMode:        store.GCPMetadataModeAssign,
				ServiceAccountID:    f.sa.ID,
				ServiceAccountEmail: f.sa.Email,
			},
		},
	}))
	agent := &agentIdentityWrapper{&AgentTokenClaims{
		Claims:      jwt.Claims{Subject: agentID},
		ProjectID:   f.projectID,
		Scopes:      scopes,
		ScopeSchema: scopeSchema,
	}}
	return contextWithIdentity(context.Background(), agent)
}

// TestEvaluateSAAssignment_ScopeSplit_PreSplitCreateOnlyAllowed covers a
// schema-0 (pre-split) token holding only project:agent:create through the
// actual SA-assign gate: allowed, via legacyAgentScopeGrants.
func TestEvaluateSAAssignment_ScopeSplit_PreSplitCreateOnlyAllowed(t *testing.T) {
	f := newScopeSplitGateFixture(t)
	ctx := f.agentContext(t, tid("scopesplit-gate-presplit"), []AgentTokenScope{ScopeAgentCreate}, 0)

	denial := f.srv.evaluateSAAssignment(ctx, nil, f.sa, SurfaceAgentCreate)
	assert.Nil(t, denial, "a schema-0 token holding project:agent:create must still be allowed to assign")
}

// TestEvaluateSAAssignment_ScopeSplit_PostSplitCreateOnlyDenied covers the
// mirror: a current-scope-schema token holding only project:agent:create is
// denied through the actual SA-assign gate.
func TestEvaluateSAAssignment_ScopeSplit_PostSplitCreateOnlyDenied(t *testing.T) {
	f := newScopeSplitGateFixture(t)
	ctx := f.agentContext(t, tid("scopesplit-gate-postsplit-create"), []AgentTokenScope{ScopeAgentCreate}, CurrentAgentScopeSchema)

	denial := f.srv.evaluateSAAssignment(ctx, nil, f.sa, SurfaceAgentCreate)
	require.NotNil(t, denial, "a current-scope-schema token holding only project:agent:create must be denied")
	assert.Equal(t, saAssignDenyForbiddenStructured, denial.kind)
}

// TestEvaluateSAAssignment_ScopeSplit_PostSplitAssignScopeAllowed covers a
// current-scope-schema token holding project:agent:sa_assign (without
// project:agent:create) through the actual SA-assign gate: allowed.
func TestEvaluateSAAssignment_ScopeSplit_PostSplitAssignScopeAllowed(t *testing.T) {
	f := newScopeSplitGateFixture(t)
	ctx := f.agentContext(t, tid("scopesplit-gate-postsplit-assign"), []AgentTokenScope{ScopeAgentSAAssign}, CurrentAgentScopeSchema)

	denial := f.srv.evaluateSAAssignment(ctx, nil, f.sa, SurfaceAgentCreate)
	assert.Nil(t, denial, "a current-scope-schema token holding project:agent:sa_assign must be allowed to assign")
}
