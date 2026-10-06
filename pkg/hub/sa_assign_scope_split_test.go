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

// preSplitScopesForRole returns the literal scope list each AgentRole
// carried before this split — a fixed snapshot, not derived from the
// current ScopesForRole, so a later addition to a role's bundle cannot
// silently change what "legacy" means here. Every role's list is a frozen
// literal: AgentRoleFull's pre-split list carried no ScopeAgentSAAssign, and
// no legacy list carries the artifact read scope added later.
func preSplitScopesForRole(role AgentRole) []AgentTokenScope {
	if role == AgentRoleFull {
		return []AgentTokenScope{
			ScopeProjectRead,
			ScopeAgentStatusUpdate,
			ScopeAgentTokenRefresh,
			ScopeAgentNotify,
			ScopeAgentPortForward,
			ScopeAgentCreate,
			ScopeAgentLifecycle,
			ScopeProjectSecretRead,
			ScopeProjectTemplateWrite,
			ScopeAgentSetMessageMode,
		}
	}
	// Frozen literal lists: a scope a role gains later (the artifact read
	// scope) must never appear in a "legacy" token.
	switch role {
	case AgentRoleReadOnly:
		return []AgentTokenScope{ScopeProjectRead}
	case AgentRoleBaseline:
		return []AgentTokenScope{
			ScopeProjectRead,
			ScopeAgentStatusUpdate,
			ScopeAgentTokenRefresh,
			ScopeAgentNotify,
			ScopeAgentPortForward,
		}
	default:
		return nil
	}
}

// TestAgentScopesToPermissionIDs_CreateAndAssignAreDisjoint is the
// registry-level pin: agentScopesToPermissionIDs is a pure scope->permission
// lookup with no compatibility logic of its own, so project:agent:create and
// project:agent:sa_assign now grant disjoint permission sets.
func TestAgentScopesToPermissionIDs_CreateAndAssignAreDisjoint(t *testing.T) {
	const (
		createPerm = "agent.create"
		assignPerm = "gcp_service_account.assign"
	)

	tests := []struct {
		name       string
		scopes     []AgentTokenScope
		wantCreate bool
		wantAssign bool
	}{
		{"neither scope grants neither permission", []AgentTokenScope{ScopeProjectRead}, false, false},
		{"create scope alone grants create, not assign", []AgentTokenScope{ScopeAgentCreate}, true, false},
		{"assign scope alone grants assign, not create", []AgentTokenScope{ScopeAgentSAAssign}, false, true},
		{"both scopes grant both permissions", []AgentTokenScope{ScopeAgentCreate, ScopeAgentSAAssign}, true, true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ids := agentScopesToPermissionIDs(tt.scopes)
			assert.Equal(t, tt.wantCreate, containsID(ids, createPerm), "%s grant", createPerm)
			assert.Equal(t, tt.wantAssign, containsID(ids, assignPerm), "%s grant", assignPerm)
		})
	}
}

func containsID(ids []string, id string) bool {
	for _, got := range ids {
		if got == id {
			return true
		}
	}
	return false
}

// TestEffectiveAgentScopes_LegacyPreSplitTokenGainsAssignScope pins
// effectiveAgentScopes directly: only an identity with legacyScopeSchema set
// (constructed directly here to simulate what ValidateAgentToken produces
// for a verified, pre-split hub JWT) that holds ScopeAgentCreate without
// ScopeAgentSAAssign gets the scope appended. Every other combination passes
// through unchanged.
func TestEffectiveAgentScopes_LegacyPreSplitTokenGainsAssignScope(t *testing.T) {
	tests := []struct {
		name   string
		scopes []AgentTokenScope
		legacy bool
		want   []AgentTokenScope
	}{
		{
			name:   "legacy token with create gains sa_assign",
			scopes: []AgentTokenScope{ScopeAgentCreate},
			legacy: true,
			want:   []AgentTokenScope{ScopeAgentCreate, ScopeAgentSAAssign},
		},
		{
			name:   "legacy token without create is unchanged",
			scopes: []AgentTokenScope{ScopeProjectRead},
			legacy: true,
			want:   []AgentTokenScope{ScopeProjectRead},
		},
		{
			name:   "legacy token already holding both scopes is unchanged (no duplicate)",
			scopes: []AgentTokenScope{ScopeAgentCreate, ScopeAgentSAAssign},
			legacy: true,
			want:   []AgentTokenScope{ScopeAgentCreate, ScopeAgentSAAssign},
		},
		{
			name:   "current-schema token with create alone is unchanged (zero-valued wrapper, not legacy)",
			scopes: []AgentTokenScope{ScopeAgentCreate},
			legacy: false,
			want:   []AgentTokenScope{ScopeAgentCreate},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			identity := &agentIdentityWrapper{&AgentTokenClaims{
				Scopes:            tt.scopes,
				legacyScopeSchema: tt.legacy,
			}}
			got := effectiveAgentScopes(identity)
			assert.ElementsMatch(t, tt.want, got)
		})
	}
}

// TestEffectiveAgentScopes_NonHubJWTIdentityNeverLegacy pins the
// discriminator's edge case: an identity this hub's own signer never issued
// (a federated agent, here) can never be read as a legacy pre-split token —
// isLegacyPreSplitAgentJWT requires a *agentIdentityWrapper with
// legacyScopeSchema set, which only ValidateAgentToken ever sets, so a
// federated identity's scopes pass through effectiveAgentScopes unchanged no
// matter what literal scope strings it carries.
//
// This is additionally moot in today's Decide pipeline: buildAgentSyntheticBindings
// is only reached when agent.ProjectID() != "" (authz.go step 5b), and
// FederatedAgentIdentity.ProjectID() always returns "", so a federated agent
// can never reach gcp_service_account.assign this way regardless. This test
// pins the discriminator function directly so the rule holds independently
// of that other gate.
func TestEffectiveAgentScopes_NonHubJWTIdentityNeverLegacy(t *testing.T) {
	federated := NewFederatedAgentIdentity(
		"https://issuer.example", "remote-agent", "proj-x", "Remote Agent", "root-user",
		nil, []AgentTokenScope{ScopeAgentCreate})

	assert.False(t, isLegacyPreSplitAgentJWT(federated),
		"a non-hub-JWT identity must never read as a legacy pre-split token")

	got := effectiveAgentScopes(federated)
	assert.Equal(t, []AgentTokenScope{ScopeAgentCreate}, got,
		"a non-hub-JWT identity's scopes must pass through unchanged")

	ids := agentScopesToPermissionIDs(got)
	assert.NotContains(t, ids, "gcp_service_account.assign",
		"a non-hub-JWT identity holding only the combined create scope must not receive the compatibility assign grant")
	assert.Contains(t, ids, "agent.create")
}

// TestAgentScopeSplit_GoldenPermissionSets pins, for every AgentRole, the
// exact permission set it authorizes — both reading its current
// ScopesForRole bundle directly, and reading its pre-split bundle through
// effectiveAgentScopes as a legacy token — and asserts the two are always
// identical, so a stray extra grant (a change to effectiveAgentScopes, or a
// Registry change that accidentally widens a role) is caught immediately.
// It also pins that no reading, for any role, ever includes
// gcp_service_account.use: the compatibility rule must never cross into a
// different per-SA-scoped permission family (ptone/scion#2129).
func TestAgentScopeSplit_GoldenPermissionSets(t *testing.T) {
	golden := map[AgentRole][]string{
		AgentRoleNone: {},
		AgentRoleReadOnly: {
			"project.read", "artifact.read", "skill.read", "skill.list",
			"template.read", "template.list",
			"harness_config.read", "harness_config.list",
		},
		AgentRoleBaseline: {
			"project.read", "artifact.read", "skill.read", "skill.list",
			"template.read", "template.list",
			"harness_config.read", "harness_config.list",
			"agent.status_update", "agent.token_refresh", "agent.notify", "agent.port_forward",
			"artifact.create", "artifact.update",
		},
		AgentRoleFull: {
			"project.read", "artifact.read", "skill.read", "skill.list",
			"template.read", "template.list",
			"harness_config.read", "harness_config.list",
			"agent.status_update", "agent.token_refresh", "agent.notify", "agent.port_forward",
			"agent.create", "gcp_service_account.assign",
			"agent.delete", "agent.attach", "agent.lifecycle",
			"project.secret_read", "secret.use",
			"template.create", "template.update",
			"agent.set_message_mode",
			"artifact.create", "artifact.update",
		},
	}

	// Iterate the explicit set of roles ValidAgentRole accepts, not
	// range(golden): ranging the map would silently skip a role missing its
	// own golden row instead of failing for it.
	for _, role := range []AgentRole{AgentRoleNone, AgentRoleReadOnly, AgentRoleBaseline, AgentRoleFull} {
		require.True(t, ValidAgentRole(role), "test bug: %s is not a valid AgentRole", role)
		want, ok := golden[role]
		require.True(t, ok, "role %s has no golden permission set in this table", role)

		t.Run(string(role)+"/current", func(t *testing.T) {
			got := agentScopesToPermissionIDs(ScopesForRole(role))
			assert.ElementsMatch(t, want, got)
			assert.NotContains(t, got, "gcp_service_account.use")
		})

		t.Run(string(role)+"/legacy", func(t *testing.T) {
			legacyIdentity := &agentIdentityWrapper{&AgentTokenClaims{
				Scopes:            preSplitScopesForRole(role),
				legacyScopeSchema: true,
			}}
			got := agentScopesToPermissionIDs(effectiveAgentScopes(legacyIdentity))
			// Legacy tokens predate project:artifact:read and
			// project:artifact:write, so they lack exactly the artifact
			// permissions.
			wantLegacy := slices.DeleteFunc(slices.Clone(want), func(p string) bool {
				return p == "artifact.read" || p == "artifact.create" || p == "artifact.update"
			})
			assert.ElementsMatch(t, wantLegacy, got,
				"a legacy pre-split token for role %s must authorize what a current token does, except for scopes added after it was minted", role)
			assert.NotContains(t, got, "gcp_service_account.use")
		})
	}

	assert.Len(t, golden, 4, "golden must have exactly one row per role ValidAgentRole accepts")
	assert.False(t, ValidAgentRole(AgentRole("unknown-role-for-test")),
		"test bug: the sentinel used to confirm ValidAgentRole rejects unknown roles must not itself be valid")
}

// TestCanAgentDelegateToAgent_LegacyFullCanDelegateFull pins the rule that
// keeps a genuine pre-split Full-role token (legacyScopeSchema set, carrying
// the old 10-scope list) able to delegate the full role to a sub-agent,
// because its effective scopes include every scope ScopesForRole(Full)
// requires: canAgentDelegateToAgent must compare against
// effectiveAgentScopes, which applies the same compatibility rule
// buildAgentSyntheticBindings and agentScopeRestriction apply, rather than
// against the actor's raw, pre-split scope list.
func TestCanAgentDelegateToAgent_LegacyFullCanDelegateFull(t *testing.T) {
	authz, _ := authzTestSetup(t)

	actor := &agentIdentityWrapper{&AgentTokenClaims{
		Claims:            jwt.Claims{Subject: tid("candelegate-legacy-full")},
		Scopes:            preSplitScopesForRole(AgentRoleFull),
		legacyScopeSchema: true,
	}}
	grant := GrantDescriptor{
		Type:      GrantTypeAgentDelegation,
		AgentRole: string(AgentRoleFull),
		ProjectID: tid("candelegate-legacy-full-project"),
	}

	decision := authz.CanDelegate(context.Background(), actor, grant)
	assert.True(t, decision.Allowed,
		"a legacy pre-split Full token must still be able to delegate full: %q", decision.Reason)
}

// TestCanAgentDelegateToAgent_CurrentCreateOnlyCannotDelegateFull is the
// mirror: an actor holding the pre-split 10-scope list (project:agent:create
// but not project:agent:sa_assign) that is NOT a verified legacy token
// (legacyScopeSchema left at its zero value) must not be able to delegate
// full — it is genuinely missing a scope full requires, and nothing here may
// widen it back.
func TestCanAgentDelegateToAgent_CurrentCreateOnlyCannotDelegateFull(t *testing.T) {
	authz, _ := authzTestSetup(t)

	actor := &agentIdentityWrapper{&AgentTokenClaims{
		Claims: jwt.Claims{Subject: tid("candelegate-current-createonly")},
		Scopes: preSplitScopesForRole(AgentRoleFull),
		// legacyScopeSchema deliberately left unset: this actor is not a
		// verified pre-split JWT, just a current-schema identity that
		// doesn't hold project:agent:sa_assign.
	}}
	grant := GrantDescriptor{
		Type:      GrantTypeAgentDelegation,
		AgentRole: string(AgentRoleFull),
		ProjectID: tid("candelegate-current-createonly-project"),
	}

	decision := authz.CanDelegate(context.Background(), actor, grant)
	assert.False(t, decision.Allowed,
		"a current-schema token without project:agent:sa_assign must not be able to delegate full")
	assert.Contains(t, decision.Reason, string(ScopeAgentSAAssign),
		"the denial must name the missing scope, not an unrelated one")
}

// TestCanAgentDelegateToAgent_LegacyParentCanDelegateExplicitAssignScope
// covers the case a legacy pre-split parent's effective authority includes
// project:agent:sa_assign even though its raw scopes never literally name
// it: a legacy actor holding only project:agent:create can delegate an
// explicit project:agent:sa_assign grant (requested independently of any
// role bundle, via GrantDescriptor.AgentScopes with AgentRole none) to a
// child. This is the intended consequence of judging delegation by the
// parent's effective authority rather than its literal scope list: a token
// minted under the pre-split vocabulary held gcp_service_account.assign
// through project:agent:create, so a parent holding that scope could already
// confer assign on a child; this pins that the same parent can still confer
// it explicitly.
func TestCanAgentDelegateToAgent_LegacyParentCanDelegateExplicitAssignScope(t *testing.T) {
	authz, _ := authzTestSetup(t)

	actor := &agentIdentityWrapper{&AgentTokenClaims{
		Claims:            jwt.Claims{Subject: tid("candelegate-legacy-explicit-assign")},
		Scopes:            []AgentTokenScope{ScopeAgentCreate},
		legacyScopeSchema: true,
	}}
	grant := GrantDescriptor{
		Type:        GrantTypeAgentDelegation,
		AgentRole:   string(AgentRoleNone),
		AgentScopes: []AgentTokenScope{ScopeAgentSAAssign},
		ProjectID:   tid("candelegate-legacy-explicit-assign-project"),
	}

	decision := authz.CanDelegate(context.Background(), actor, grant)
	assert.True(t, decision.Allowed,
		"a legacy parent holding project:agent:create must be able to delegate an explicit project:agent:sa_assign grant: %q",
		decision.Reason)
}

// TestCanAgentDelegateToAgent_CurrentParentCannotDelegateAssignItLacks is the
// mirror: a non-legacy actor holding only project:agent:create (not flagged
// as a legacy pre-split token) must NOT be able to delegate an explicit
// project:agent:sa_assign grant it does not itself hold.
func TestCanAgentDelegateToAgent_CurrentParentCannotDelegateAssignItLacks(t *testing.T) {
	authz, _ := authzTestSetup(t)

	actor := &agentIdentityWrapper{&AgentTokenClaims{
		Claims: jwt.Claims{Subject: tid("candelegate-current-explicit-assign")},
		Scopes: []AgentTokenScope{ScopeAgentCreate},
		// legacyScopeSchema deliberately left unset.
	}}
	grant := GrantDescriptor{
		Type:        GrantTypeAgentDelegation,
		AgentRole:   string(AgentRoleNone),
		AgentScopes: []AgentTokenScope{ScopeAgentSAAssign},
		ProjectID:   tid("candelegate-current-explicit-assign-project"),
	}

	decision := authz.CanDelegate(context.Background(), actor, grant)
	assert.False(t, decision.Allowed,
		"a non-legacy parent must not be able to delegate project:agent:sa_assign it does not hold: %q",
		decision.Reason)
	assert.Contains(t, decision.Reason, string(ScopeAgentSAAssign),
		"the denial must name the missing scope, not an unrelated one")
}

// scopeSplitFixture is the shared world for the CheckAccess-level scope-split
// tests: one project and one service account inside it, so a test only has
// to vary the calling agent's scopes and legacy status.
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
// the given scopes and legacy flag — never derived from ScopesForRole, so
// every scope combination is reachable regardless of which real AgentRole
// would produce it. legacy=false leaves legacyScopeSchema at its Go zero
// value, the same shape every in-process, non-ValidateAgentToken identity
// has.
func (f *scopeSplitFixture) identity(agentID string, scopes []AgentTokenScope, legacy bool) AgentIdentity {
	return &agentIdentityWrapper{&AgentTokenClaims{
		Claims:            jwt.Claims{Subject: agentID},
		ProjectID:         f.projectID,
		Scopes:            scopes,
		legacyScopeSchema: legacy,
	}}
}

func (f *scopeSplitFixture) agentCreateResource() Resource {
	return Resource{Type: "agent", ParentType: "project", ParentID: f.projectID}
}

// TestAuthz_AgentScopeSplit_CreateWithoutAssign covers a current (non-legacy)
// token holding only project:agent:create: agent.create is granted,
// gcp_service_account.assign is not.
func TestAuthz_AgentScopeSplit_CreateWithoutAssign(t *testing.T) {
	f := newScopeSplitFixture(t)
	ctx := context.Background()
	identity := f.identity(tid("scopesplit-create-only"), []AgentTokenScope{ScopeAgentCreate}, false)

	createDecision := f.authz.CheckAccess(ctx, identity, f.agentCreateResource(), ActionCreate)
	assert.True(t, createDecision.Allowed, "agent.create must be granted: %q", createDecision.Reason)

	assignDecision := f.authz.CheckAccess(ctx, identity, f.sa, ActionAssign)
	assert.False(t, assignDecision.Allowed,
		"gcp_service_account.assign must not be granted by project:agent:create alone on a non-legacy token")
}

// TestAuthz_AgentScopeSplit_AssignWithoutCreate covers a current token
// holding only project:agent:sa_assign: gcp_service_account.assign is
// granted, agent.create is not.
func TestAuthz_AgentScopeSplit_AssignWithoutCreate(t *testing.T) {
	f := newScopeSplitFixture(t)
	ctx := context.Background()
	identity := f.identity(tid("scopesplit-assign-only"), []AgentTokenScope{ScopeAgentSAAssign}, false)

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
		[]AgentTokenScope{ScopeAgentCreate, ScopeAgentSAAssign}, false)

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
	identity := f.identity(tid("scopesplit-neither"), []AgentTokenScope{ScopeProjectRead}, false)

	createDecision := f.authz.CheckAccess(ctx, identity, f.agentCreateResource(), ActionCreate)
	assert.False(t, createDecision.Allowed, "agent.create must not be granted")

	assignDecision := f.authz.CheckAccess(ctx, identity, f.sa, ActionAssign)
	assert.False(t, assignDecision.Allowed, "gcp_service_account.assign must not be granted")
}

// TestAuthz_AgentScopeSplit_PreSplitTokenKeepsAssign pins the compatibility
// rule: a genuine legacy pre-split agent JWT that holds only the combined
// project:agent:create scope keeps authorizing gcp_service_account.assign,
// exactly as it did when it was minted.
func TestAuthz_AgentScopeSplit_PreSplitTokenKeepsAssign(t *testing.T) {
	f := newScopeSplitFixture(t)
	ctx := context.Background()
	identity := f.identity(tid("scopesplit-presplit"), []AgentTokenScope{ScopeAgentCreate}, true)

	decision := f.authz.CheckAccess(ctx, identity, f.sa, ActionAssign)
	assert.True(t, decision.Allowed,
		"a legacy pre-split token holding project:agent:create must keep authorizing gcp_service_account.assign: %q",
		decision.Reason)
}

// TestAuthz_AgentScopeSplit_PostSplitTokenDoesNotGainAssign is
// TestAuthz_AgentScopeSplit_PreSplitTokenKeepsAssign's mirror, and the
// property that makes the split real rather than cosmetic: a zero-valued
// identity wrapper (legacyScopeSchema at its Go zero value, false — the same
// shape every in-process, non-ValidateAgentToken identity has) holding only
// project:agent:create must not gain gcp_service_account.assign.
func TestAuthz_AgentScopeSplit_PostSplitTokenDoesNotGainAssign(t *testing.T) {
	f := newScopeSplitFixture(t)
	ctx := context.Background()
	identity := f.identity(tid("scopesplit-postsplit"), []AgentTokenScope{ScopeAgentCreate}, false)

	decision := f.authz.CheckAccess(ctx, identity, f.sa, ActionAssign)
	assert.False(t, decision.Allowed,
		"a zero-valued identity holding only project:agent:create must not gain gcp_service_account.assign")
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
// identity with exactly the given scopes and legacy flag. The agent record
// carries its own GCP identity in assign mode so callerPrincipal populates a
// ServiceAccountEmail — otherwise Layer 2 denies with "no caller identity"
// before Layer 1's scope check (the thing these tests exercise) is ever
// reached, and with saAssignCheckMode off (the fixture's default), that
// identity is never actually verified against GCP, only required to be
// present.
func (f *scopeSplitGateFixture) agentContext(t *testing.T, agentID string, scopes []AgentTokenScope, legacy bool) context.Context {
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
		Claims:            jwt.Claims{Subject: agentID},
		ProjectID:         f.projectID,
		Scopes:            scopes,
		legacyScopeSchema: legacy,
	}}
	return contextWithIdentity(context.Background(), agent)
}

// TestEvaluateSAAssignment_ScopeSplit_PreSplitCreateOnlyAllowed covers a
// genuine legacy pre-split token holding only project:agent:create through
// the actual SA-assign gate: allowed, via effectiveAgentScopes.
func TestEvaluateSAAssignment_ScopeSplit_PreSplitCreateOnlyAllowed(t *testing.T) {
	f := newScopeSplitGateFixture(t)
	ctx := f.agentContext(t, tid("scopesplit-gate-presplit"), []AgentTokenScope{ScopeAgentCreate}, true)

	denial := f.srv.evaluateSAAssignment(ctx, nil, f.sa, SurfaceAgentCreate)
	assert.Nil(t, denial, "a legacy pre-split token holding project:agent:create must still be allowed to assign")
}

// TestEvaluateSAAssignment_ScopeSplit_PostSplitCreateOnlyDenied covers the
// mirror: a non-legacy (zero-valued) token holding only
// project:agent:create is denied through the actual SA-assign gate.
func TestEvaluateSAAssignment_ScopeSplit_PostSplitCreateOnlyDenied(t *testing.T) {
	f := newScopeSplitGateFixture(t)
	ctx := f.agentContext(t, tid("scopesplit-gate-postsplit-create"), []AgentTokenScope{ScopeAgentCreate}, false)

	denial := f.srv.evaluateSAAssignment(ctx, nil, f.sa, SurfaceAgentCreate)
	require.NotNil(t, denial, "a non-legacy token holding only project:agent:create must be denied")
	assert.Equal(t, saAssignDenyForbiddenStructured, denial.kind)
}

// TestEvaluateSAAssignment_ScopeSplit_PostSplitAssignScopeAllowed covers a
// non-legacy token holding project:agent:sa_assign (without
// project:agent:create) through the actual SA-assign gate: allowed.
func TestEvaluateSAAssignment_ScopeSplit_PostSplitAssignScopeAllowed(t *testing.T) {
	f := newScopeSplitGateFixture(t)
	ctx := f.agentContext(t, tid("scopesplit-gate-postsplit-assign"), []AgentTokenScope{ScopeAgentSAAssign}, false)

	denial := f.srv.evaluateSAAssignment(ctx, nil, f.sa, SurfaceAgentCreate)
	assert.Nil(t, denial, "a non-legacy token holding project:agent:sa_assign must be allowed to assign")
}
