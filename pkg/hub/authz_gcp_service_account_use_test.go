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

// Package hub — ptone/scion#2129: the resource-aware agent-credential path
// for gcp_service_account.use. These tests exercise the kernel-level grant
// (gcpServiceAccountUseBinding) and restriction
// (agentGCPServiceAccountUseScopeMatch/agentScopeRestriction) directly
// through Decide, independent of the HTTP handlers -- see
// handlers_gcp_identity_mint_authz_test.go for the handler-level record and
// parity tests.
//
// Allow cases below run post-backfill with a system-admin delegator (via
// seedPostBackfillAdminEdge) so the delegation ceiling passes without
// depending on the pre-backfill temporary allow -- production state, not a
// carve-out. The ordinary-creator case, where the delegator is not a system
// admin and does not itself hold gcp_service_account.use, is pinned as
// denying by TestGCPServiceAccountUse_MintSucceedsWhilePermissionDecisionDenies
// (handlers_gcp_identity_mint_authz_test.go). Deny cases hold in either
// state and are unaffected by the marker.
package hub

import (
	"context"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/hub/permissions"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// gcpUseResource builds the Resource Decide sees for one gcp_service_account
// row, mirroring gcpServiceAccountResource without requiring a persisted row.
func gcpUseResource(id string) Resource {
	return Resource{Type: permissions.ResourceGCPServiceAccount, ID: id}
}

// seedPostBackfillAdminEdge marks the delegation-edge backfill migration as
// complete and records a project-scoped delegation edge from a system-admin
// user to agentID. checkUserHoldsPermission short-circuits on IsSystemAdmin,
// so the delegation ceiling passes for this agent at this project scope
// regardless of which permission is being decided -- production-like state
// (every hub-attested agent has an edge post-backfill), without seeding the
// permission itself into any role.
func seedPostBackfillAdminEdge(t *testing.T, s store.Store, agentID, projectID string) {
	t.Helper()
	setBackfillCompleted(t, s)
	adminID := tid("admin-for-" + agentID)
	createTestUserWithRole(t, s, adminID, adminID+"@test.com", "admin", store.SystemRoleSuperAdmin)
	seedRecordedDelegationEdge(t, s, store.DelegationPrincipalUser, adminID, store.DelegationPrincipalAgent, agentID,
		store.RoleScopeProject, projectID, store.ProjectRoleOwner)
}

func TestGCPServiceAccountUse_MatchingScopeAdmits(t *testing.T) {
	authz, s := authzTestSetup(t)
	ctx := context.Background()
	saX := tid("sa-x")
	agentID, projectID := tid("agent-match"), tid("project-match")
	agent := newFullAgentIdentity(agentID, projectID, nil, []AgentTokenScope{GCPTokenScopeForSA(saX)})
	seedPostBackfillAdminEdge(t, s, agentID, projectID)

	d := authz.Decide(ctx, AuthzRequest{
		Principal:  principalContextForIdentity(agent),
		Credential: credentialContextForIdentity(agent),
		Resource:   gcpUseResource(saX),
		Action:     ActionUse,
		Permission: permissions.PermissionGCPServiceAccountUse,
	})
	if !d.Allowed {
		t.Fatalf("expected admission for the exact matching SA, denied: %s", d.Reason)
	}
}

func TestGCPServiceAccountUse_AnotherAccountScopeDenied(t *testing.T) {
	authz, _ := authzTestSetup(t)
	ctx := context.Background()
	saX, saY := tid("sa-x"), tid("sa-y")
	agent := newFullAgentIdentity(tid("agent-other"), tid("project-other"), nil, []AgentTokenScope{GCPTokenScopeForSA(saX)})

	d := authz.Decide(ctx, AuthzRequest{
		Principal:  principalContextForIdentity(agent),
		Credential: credentialContextForIdentity(agent),
		Resource:   gcpUseResource(saY),
		Action:     ActionUse,
		Permission: permissions.PermissionGCPServiceAccountUse,
	})
	if d.Allowed {
		t.Fatalf("expected deny: agent's scope names a different service account")
	}
}

func TestGCPServiceAccountUse_NoScopeDenied(t *testing.T) {
	authz, _ := authzTestSetup(t)
	ctx := context.Background()
	saX := tid("sa-x")
	agent := newFullAgentIdentity(tid("agent-noscope"), tid("project-noscope"), nil, nil)

	d := authz.Decide(ctx, AuthzRequest{
		Principal:  principalContextForIdentity(agent),
		Credential: credentialContextForIdentity(agent),
		Resource:   gcpUseResource(saX),
		Action:     ActionUse,
		Permission: permissions.PermissionGCPServiceAccountUse,
	})
	if d.Allowed {
		t.Fatalf("expected deny: agent JWT has no scopes at all")
	}
}

func TestGCPServiceAccountUse_PrefixOnlyScopeDenied(t *testing.T) {
	authz, _ := authzTestSetup(t)
	ctx := context.Background()
	saX := tid("sa-x")
	// The bare prefix, with no SA ID suffix at all -- never a real scope any
	// dispatch path issues, but exactly the shape a prefix-based admission
	// mistake would accept.
	agent := newFullAgentIdentity(tid("agent-prefix"), tid("project-prefix"), nil, []AgentTokenScope{ScopeGCPTokenPrefix})

	d := authz.Decide(ctx, AuthzRequest{
		Principal:  principalContextForIdentity(agent),
		Credential: credentialContextForIdentity(agent),
		Resource:   gcpUseResource(saX),
		Action:     ActionUse,
		Permission: permissions.PermissionGCPServiceAccountUse,
	})
	if d.Allowed {
		t.Fatalf("expected deny: a bare scope prefix must not satisfy the exact per-SA match")
	}
}

func TestGCPServiceAccountUse_EmptyResourceIDDenied(t *testing.T) {
	authz, _ := authzTestSetup(t)
	ctx := context.Background()
	saX := tid("sa-x")
	agent := newFullAgentIdentity(tid("agent-emptyid"), tid("project-emptyid"), nil, []AgentTokenScope{GCPTokenScopeForSA(saX)})

	d := authz.Decide(ctx, AuthzRequest{
		Principal:  principalContextForIdentity(agent),
		Credential: credentialContextForIdentity(agent),
		Resource:   gcpUseResource(""),
		Action:     ActionUse,
		Permission: permissions.PermissionGCPServiceAccountUse,
	})
	if d.Allowed {
		t.Fatalf("expected deny: empty resource ID must fail closed")
	}

	// The guard must independently deny an empty ID even when the agent's own
	// scope is the bare prefix (GCPTokenScopeForSA("")), which is the one
	// case where the exact HasScope compare alone would not catch a missing
	// empty-ID check: the scope and the decided ID both being empty would
	// otherwise line up.
	emptyIDAgent := newFullAgentIdentity(tid("agent-emptyid-scope"), tid("project-emptyid-scope"), nil, []AgentTokenScope{GCPTokenScopeForSA("")})
	d = authz.Decide(ctx, AuthzRequest{
		Principal:  principalContextForIdentity(emptyIDAgent),
		Credential: credentialContextForIdentity(emptyIDAgent),
		Resource:   gcpUseResource(""),
		Action:     ActionUse,
		Permission: permissions.PermissionGCPServiceAccountUse,
	})
	if d.Allowed {
		t.Fatalf("expected deny: empty resource ID must fail closed even when the agent's own scope is the bare prefix")
	}
}

func TestGCPServiceAccountUse_WrongResourceTypeDenied(t *testing.T) {
	authz, _ := authzTestSetup(t)
	ctx := context.Background()
	saX := tid("sa-x")
	agent := newFullAgentIdentity(tid("agent-wrongtype"), tid("project-wrongtype"), nil, []AgentTokenScope{GCPTokenScopeForSA(saX)})

	d := authz.Decide(ctx, AuthzRequest{
		Principal:  principalContextForIdentity(agent),
		Credential: credentialContextForIdentity(agent),
		Resource:   Resource{Type: "secret", ID: saX},
		Action:     ActionUse,
		Permission: permissions.PermissionGCPServiceAccountUse,
	})
	if d.Allowed {
		t.Fatalf("expected deny: a non-gcp_service_account resource type must not satisfy this permission")
	}
}

// TestGCPServiceAccountUse_WrongPermissionUnaffected pins that the
// per-instance path is keyed on the permission ID, not merely the resource
// type: deciding a different permission over the same gcp_service_account
// resource follows the ordinary (denying, since gcp_service_account.read has
// no AgentScopes either) path rather than being accidentally admitted.
func TestGCPServiceAccountUse_WrongPermissionUnaffected(t *testing.T) {
	authz, _ := authzTestSetup(t)
	ctx := context.Background()
	saX := tid("sa-x")
	agent := newFullAgentIdentity(tid("agent-wrongperm"), tid("project-wrongperm"), nil, []AgentTokenScope{GCPTokenScopeForSA(saX)})

	d := authz.Decide(ctx, AuthzRequest{
		Principal:  principalContextForIdentity(agent),
		Credential: credentialContextForIdentity(agent),
		Resource:   gcpUseResource(saX),
		Action:     ActionRead,
		Permission: "gcp_service_account.read",
	})
	if d.Allowed {
		t.Fatalf("expected deny: gcp_service_account.read must not be admitted by a gcp_service_account.use scope")
	}
}

// TestGCPServiceAccountUse_ScopeAndParentKindUnrelated proves the per-instance
// match is decided from Resource.Type and Resource.ID alone: a
// gcp_service_account.use resource built the way gcpServiceAccountResource
// builds it for a hub-, project- or user-scoped row (which vary ParentType/
// ParentID, never Type/ID) all admit identically when the exact scope
// matches.
func TestGCPServiceAccountUse_ScopeAndParentKindUnrelated(t *testing.T) {
	authz, s := authzTestSetup(t)
	ctx := context.Background()
	saID := tid("sa-any-scope")
	agentID, projectID := tid("agent-anyscope"), tid("project-anyscope")
	agent := newFullAgentIdentity(agentID, projectID, nil, []AgentTokenScope{GCPTokenScopeForSA(saID)})
	seedPostBackfillAdminEdge(t, s, agentID, projectID)

	// The project-scoped case uses the agent's own project: the delegation
	// ceiling (step 10) derives its scope from a project-typed resource's
	// own parent, so a project-scoped SA in some OTHER project would make
	// this a cross-project request and deny on the ceiling alone -- a fact
	// about the ceiling, not about the per-instance match this test pins.
	for _, sa := range []*store.GCPServiceAccount{
		{ID: saID, Scope: store.ScopeHub, ScopeID: tid("hub")},
		{ID: saID, Scope: store.ScopeProject, ScopeID: projectID},
		{ID: saID, Scope: store.ScopeUser, ScopeID: tid("user-any")},
	} {
		d := authz.Decide(ctx, AuthzRequest{
			Principal:  principalContextForIdentity(agent),
			Credential: credentialContextForIdentity(agent),
			Resource:   gcpServiceAccountResource(sa),
			Action:     ActionUse,
			Permission: permissions.PermissionGCPServiceAccountUse,
		})
		if !d.Allowed {
			t.Errorf("scope=%s: expected admission for the exact matching SA regardless of the account's own scope, denied: %s", sa.Scope, d.Reason)
		}
	}
}

// TestGCPServiceAccountUse_ExplainNamesPermission pins that an explained
// decision over this permission names it, the same as any other permission.
func TestGCPServiceAccountUse_ExplainNamesPermission(t *testing.T) {
	authz, s := authzTestSetup(t)
	ctx := context.Background()
	saX := tid("sa-explain")
	agentID, projectID := tid("agent-explain"), tid("project-explain")
	agent := newFullAgentIdentity(agentID, projectID, nil, []AgentTokenScope{GCPTokenScopeForSA(saX)})
	seedPostBackfillAdminEdge(t, s, agentID, projectID)

	d := authz.Decide(ctx, AuthzRequest{
		Principal:  principalContextForIdentity(agent),
		Credential: credentialContextForIdentity(agent),
		Resource:   gcpUseResource(saX),
		Action:     ActionUse,
		Permission: permissions.PermissionGCPServiceAccountUse,
		Explain:    true,
	})
	if !d.Allowed {
		t.Fatalf("expected admission, denied: %s", d.Reason)
	}
	if d.PermissionID != permissions.PermissionGCPServiceAccountUse {
		t.Errorf("Decision.PermissionID = %q, want %q", d.PermissionID, permissions.PermissionGCPServiceAccountUse)
	}
	if d.Provenance == nil || d.Provenance.Permission != permissions.PermissionGCPServiceAccountUse {
		t.Errorf("explain provenance does not name the permission: %+v", d.Provenance)
	}
}

// TestGCPServiceAccountUse_IndependentGrantStillRestricted proves the
// step-7b restriction is load-bearing even against a grant that did not come
// from the request-local synthetic binding: an agent holding a REAL role
// binding for gcp_service_account.use, independent of gcpServiceAccountUseBinding
// entirely, is still denied on a service account its JWT scope does not name.
// Without agentScopeRestriction's per-instance check this would incorrectly
// allow, since ordinary restrictions never gate a role-granted permission by
// resource ID.
func TestGCPServiceAccountUse_IndependentGrantStillRestricted(t *testing.T) {
	authz, s := authzTestSetup(t)
	ctx := context.Background()

	projectID := tid("project-independent-grant")
	if err := s.CreateProject(ctx, &store.Project{
		ID: projectID, Name: "independent-grant", Slug: "independent-grant", Created: time.Now(), Updated: time.Now(),
	}); err != nil {
		t.Fatalf("CreateProject: %v", err)
	}

	agentID := tid("agent-independent-grant")
	saX, saY := tid("sa-independent-x"), tid("sa-independent-y")

	// CreateRoleBinding validates that an "agent" principal exists, so the
	// role-granted path this test exercises needs a real persisted agent, not
	// just the in-memory AgentIdentity used for the Decide call below.
	if err := s.CreateAgent(ctx, &store.Agent{
		ID: agentID, Slug: "agent-independent-grant", Name: "Agent Independent Grant",
		ProjectID: projectID, Phase: string(state.PhaseRunning), StateVersion: 1,
		Created: time.Now(), Updated: time.Now(),
	}); err != nil {
		t.Fatalf("CreateAgent: %v", err)
	}

	rd, err := s.CreateRoleDefinition(ctx, &store.RoleDefinition{
		Name:        "test-gcp-sa-use-" + agentID,
		ScopeType:   store.RoleScopeSystem,
		Permissions: []string{permissions.PermissionGCPServiceAccountUse},
	})
	if err != nil {
		t.Fatalf("CreateRoleDefinition: %v", err)
	}
	if _, err := s.CreateRoleBinding(ctx, &store.RoleBinding{
		RoleDefinitionID: rd.ID,
		PrincipalType:    store.RoleBindingPrincipalAgent,
		PrincipalID:      agentID,
		ScopeType:        store.RoleScopeSystem,
		CreatedBy:        "test",
	}); err != nil {
		t.Fatalf("CreateRoleBinding: %v", err)
	}

	// The agent's JWT names SA X, not SA Y.
	agent := newFullAgentIdentity(agentID, projectID, nil, []AgentTokenScope{GCPTokenScopeForSA(saX)})
	seedPostBackfillAdminEdge(t, s, agentID, projectID)

	d := authz.Decide(ctx, AuthzRequest{
		Principal:  principalContextForIdentity(agent),
		Credential: credentialContextForIdentity(agent),
		Resource:   gcpUseResource(saY),
		Action:     ActionUse,
		Permission: permissions.PermissionGCPServiceAccountUse,
	})
	if d.Allowed {
		t.Fatalf("expected deny: a role-granted gcp_service_account.use must still be restricted to the JWT's own SA")
	}

	// Sanity: the same role binding DOES admit the SA the JWT actually names
	// (the request-local grant is not required when a role already grants
	// it; the restriction is what narrows it, not what widens it).
	d = authz.Decide(ctx, AuthzRequest{
		Principal:  principalContextForIdentity(agent),
		Credential: credentialContextForIdentity(agent),
		Resource:   gcpUseResource(saX),
		Action:     ActionUse,
		Permission: permissions.PermissionGCPServiceAccountUse,
	})
	if !d.Allowed {
		t.Fatalf("expected admission for the SA the JWT names, denied: %s", d.Reason)
	}
}

// TestGCPServiceAccountUse_TargetAgnosticGrantNeverIncludesIt pins that
// agentScopesToPermissionIDs -- the target-agnostic function behind Decide's
// project-scoped agent binding (Step 5b) -- never grants gcp_service_account.use
// from a per-SA scope. The request-local grant in decide() (Step 5b3) is built by
// a different, resource-aware function and must never be folded into this one.
// CanDelegate's side is pinned by TestGCPServiceAccountUse_CanDelegateUnaffected.
func TestGCPServiceAccountUse_TargetAgnosticGrantNeverIncludesIt(t *testing.T) {
	scopes := []AgentTokenScope{GCPTokenScopeForSA(tid("sa-agnostic"))}
	for _, id := range agentScopesToPermissionIDs(scopes) {
		if id == permissions.PermissionGCPServiceAccountUse {
			t.Fatalf("agentScopesToPermissionIDs must never grant gcp_service_account.use target-agnostically")
		}
	}
}

// TestGCPServiceAccountUse_CanDelegateUnaffected pins that CanDelegate's
// credential-caveat intersection -- which calls agentScopeRestriction with a
// zero Resource because delegation reasons about a permission set, not one
// resource instance -- cannot be satisfied by a per-SA scope. An agent must
// not be able to delegate "use this one service account" as if it were
// general gcp_service_account.use authority.
func TestGCPServiceAccountUse_CanDelegateUnaffected(t *testing.T) {
	authz, _ := authzTestSetup(t)
	saX := tid("sa-candelegate")
	agent := newFullAgentIdentity(tid("agent-candelegate"), tid("project-candelegate"), nil, []AgentTokenScope{GCPTokenScopeForSA(saX)})

	filtered := authz.intersectCredentialCaveats(agent, []string{permissions.PermissionGCPServiceAccountUse})
	if len(filtered) != 0 {
		t.Fatalf("CanDelegate's caveat intersection must not admit gcp_service_account.use from a per-SA scope, got %v", filtered)
	}
}

// TestGCPServiceAccountUse_ScopeCapabilityNeverAdmitsUnscoped pins that a
// scope-level (instance-less) capability computation -- ComputeScopeCapabilities,
// whose Resource never carries an ID -- must never report gcp_service_account.use
// as held, no matter what per-SA scope the agent's JWT carries. No capability
// surface enumerates this permission at all: gcp_service_account.use has no
// CapabilityKind, so it is absent from both permissions.ResourceActions() and
// permissions.ScopeActions() for gcp_service_account (see
// TestGCPServiceAccountUse_CapabilitySurfaceNeverListsUse), which is the
// property this test and that one together rely on.
func TestGCPServiceAccountUse_ScopeCapabilityNeverAdmitsUnscoped(t *testing.T) {
	authz, _ := authzTestSetup(t)
	ctx := context.Background()
	saX := tid("sa-scope-cap")
	agent := newFullAgentIdentity(tid("agent-scope-cap"), tid("project-scope-cap"), nil, []AgentTokenScope{GCPTokenScopeForSA(saX)})

	caps := authz.ComputeScopeCapabilities(ctx, agent, "project", tid("project-scope-cap"), permissions.ResourceGCPServiceAccount)
	for _, a := range caps.Actions {
		if a == string(ActionUse) {
			t.Fatalf("scope-level (unscoped) capabilities must not report %q as held: %v", ActionUse, caps.Actions)
		}
	}
}

// TestGCPServiceAccountUse_OnlyPermissionOnPerInstancePath is the Registry-walk
// regression test: exactly one permission (gcp_service_account.use) is
// decided by the per-instance resource match, on BOTH the restriction side
// and the request-local grant side, and every other registered permission's
// behavior on each side is unaffected by the per-SA scope.
func TestGCPServiceAccountUse_OnlyPermissionOnPerInstancePath(t *testing.T) {
	saID := tid("sa-registry-walk")
	perSAScope := GCPTokenScopeForSA(saID)
	allScopes := append(append([]AgentTokenScope{}, allRegisteredAgentScopes()...), perSAScope)

	perSAOnlyAgent := newFullAgentIdentity(tid("agent-registry-walk-per-sa-only"), tid("project-registry-walk"), nil, []AgentTokenScope{perSAScope})
	allScopesAgent := newFullAgentIdentity(tid("agent-registry-walk-all-scopes"), tid("project-registry-walk"), nil, allScopes)

	scopeSet := make(map[string]bool, len(allScopes))
	for _, sc := range allScopes {
		scopeSet[string(sc)] = true
	}
	staticAllowed := make(map[string]bool)
	for _, p := range permissions.Registry {
		for _, sc := range p.AgentScopes {
			if scopeSet[sc] {
				staticAllowed[p.ID] = true
				break
			}
		}
	}

	// 1. Walk the restriction twice: per-SA-only must deny every permission
	// except gcp_service_account.use (fails if a second permission takes the
	// per-instance restriction path); all-scopes must match the static
	// AgentScopes result for every permission except gcp_service_account.use.
	perSAOnlyAllowedCount := 0
	var perSAOnlyAllowedID string
	for _, p := range permissions.Registry {
		resource := Resource{Type: p.Resource, ID: saID}

		perSAOnlyGot := agentScopeRestriction(perSAOnlyAgent, resource).Check(p.ID)
		if p.ID == permissions.PermissionGCPServiceAccountUse {
			if !perSAOnlyGot {
				t.Errorf("%s: expected per-instance admission for the exact matching SA with only the per-SA scope, got denied", p.ID)
			}
		} else if perSAOnlyGot {
			t.Errorf("%s: expected deny with only the per-SA scope (no static AgentScopes match), got allowed", p.ID)
		}
		if perSAOnlyGot {
			perSAOnlyAllowedCount++
			perSAOnlyAllowedID = p.ID
		}

		allScopesGot := agentScopeRestriction(allScopesAgent, resource).Check(p.ID)
		if p.ID == permissions.PermissionGCPServiceAccountUse {
			if !allScopesGot {
				t.Errorf("%s: expected per-instance admission for the exact matching SA with all scopes, got denied", p.ID)
			}
			continue
		}
		if allScopesGot != staticAllowed[p.ID] {
			t.Errorf("%s: agent-scope restriction diverged from the static AgentScopes path (got %v, want %v)", p.ID, allScopesGot, staticAllowed[p.ID])
		}
	}

	// 2. Walk the grant: gcpServiceAccountUseBinding must be nil for every
	// permission except gcp_service_account.use (fails if the synthetic role
	// grants any other permission). For that one permission, the returned
	// role must hold exactly that one permission, system-scoped.
	grantNonNilCount := 0
	var grantNonNilID string
	for _, p := range permissions.Registry {
		resource := Resource{Type: p.Resource, ID: saID}
		cb, role := gcpServiceAccountUseBinding(allScopesAgent, p.ID, resource)
		if p.ID == permissions.PermissionGCPServiceAccountUse {
			if cb == nil || role == nil {
				t.Fatalf("%s: expected a request-local grant for the exact matching SA, got nil", p.ID)
			}
			if role.ScopeType != ScopeTypeSystem {
				t.Errorf("%s: grant RolePermissions.ScopeType = %v, want %v", p.ID, role.ScopeType, ScopeTypeSystem)
			}
			if len(role.Permissions) != 1 {
				t.Errorf("%s: grant RolePermissions.Permissions = %v, want exactly {%s}", p.ID, role.Permissions, permissions.PermissionGCPServiceAccountUse)
			} else if _, ok := role.Permissions[permissions.PermissionGCPServiceAccountUse]; !ok {
				t.Errorf("%s: grant RolePermissions.Permissions = %v, want exactly {%s}", p.ID, role.Permissions, permissions.PermissionGCPServiceAccountUse)
			}
		} else if cb != nil || role != nil {
			t.Errorf("%s: expected no request-local grant, got cb=%v role=%v", p.ID, cb, role)
		}
		if cb != nil {
			grantNonNilCount++
			grantNonNilID = p.ID
		}
	}

	// 3. Exactly one permission takes the grant path and exactly one takes
	// the per-SA-only restriction path, and it is the same permission on
	// both sides. It proves which path each permission actually took.
	if perSAOnlyAllowedCount != 1 {
		t.Fatalf("expected exactly one permission allowed with only the per-SA scope, found %d", perSAOnlyAllowedCount)
	}
	if grantNonNilCount != 1 {
		t.Fatalf("expected exactly one permission with a non-nil request-local grant, found %d", grantNonNilCount)
	}
	if perSAOnlyAllowedID != grantNonNilID {
		t.Fatalf("the per-instance restriction and the request-local grant named different permissions: %q vs %q", perSAOnlyAllowedID, grantNonNilID)
	}
	if perSAOnlyAllowedID != permissions.PermissionGCPServiceAccountUse {
		t.Fatalf("expected the one per-instance permission to be %s, got %s", permissions.PermissionGCPServiceAccountUse, perSAOnlyAllowedID)
	}
}

// TestGCPServiceAccountUse_CapabilitySurfaceNeverListsUse asserts the
// structural property that ScopeCapabilityNeverAdmitsUnscoped actually
// relies on: no capability surface enumerates gcp_service_account.use at
// all, because the Registry row has no CapabilityKind. "use" is absent from
// both ResourceActions and ScopeActions for gcp_service_account.
func TestGCPServiceAccountUse_CapabilitySurfaceNeverListsUse(t *testing.T) {
	for _, action := range permissions.ResourceActions()[permissions.ResourceGCPServiceAccount] {
		if action == string(ActionUse) {
			t.Fatalf("ResourceActions()[%s] must not list %q", permissions.ResourceGCPServiceAccount, ActionUse)
		}
	}
	for _, action := range permissions.ScopeActions()[permissions.ResourceGCPServiceAccount] {
		if action == string(ActionUse) {
			t.Fatalf("ScopeActions()[%s] must not list %q", permissions.ResourceGCPServiceAccount, ActionUse)
		}
	}
}

// TestGCPServiceAccountUse_ExactIDComparison pins that
// agentGCPServiceAccountUseScopeMatch compares the exact resource ID, not
// merely the presence of some GCP token scope. This test fails if that
// comparison is weakened to "the agent holds ANY project:gcp:token:* scope".
// It also pins the helper's own permission-ID guard: the same agent and
// exact SA resource must not match for a different permission ID, and the
// request-local grant must not be built for one either.
func TestGCPServiceAccountUse_ExactIDComparison(t *testing.T) {
	saX, saY := tid("sa-exact-x"), tid("sa-exact-y")
	agent := newFullAgentIdentity(tid("agent-exact"), tid("project-exact"), nil, []AgentTokenScope{GCPTokenScopeForSA(saX)})

	if !agentGCPServiceAccountUseScopeMatch(agent, permissions.PermissionGCPServiceAccountUse, gcpUseResource(saX)) {
		t.Fatalf("expected match for the exact SA the JWT names")
	}
	if agentGCPServiceAccountUseScopeMatch(agent, permissions.PermissionGCPServiceAccountUse, gcpUseResource(saY)) {
		t.Fatalf("expected no match for a different SA ID -- the ID comparison must be exact")
	}
	if agentGCPServiceAccountUseScopeMatch(agent, "gcp_service_account.read", gcpUseResource(saX)) {
		t.Fatalf("expected no match for a different permission on the same SA")
	}
	if cb, _ := gcpServiceAccountUseBinding(agent, "gcp_service_account.read", gcpUseResource(saX)); cb != nil {
		t.Fatalf("expected no request-local binding for a different permission")
	}
}

// TestGCPServiceAccountUse_UserDecisionUnchanged pins that the per-SA
// agent-credential path is gated on isAgentPrincipal and so leaves a User
// principal's decision over gcp_service_account.use unaffected: a user with
// no role is denied, and a system-admin user (who holds every registered
// permission through the super-admin role) is admitted, exactly as for any
// other permission.
func TestGCPServiceAccountUse_UserDecisionUnchanged(t *testing.T) {
	authz, s := authzTestSetup(t)
	ctx := context.Background()
	saX := tid("sa-user-decision")

	plainUserID := tid("user-plain-decision")
	if err := s.CreateUser(ctx, &store.User{
		ID: plainUserID, Email: "plain-decision@test.com", DisplayName: "Plain", Role: "member", Status: store.UserStatusActive,
	}); err != nil {
		t.Fatalf("CreateUser: %v", err)
	}
	plainUser := NewAuthenticatedUser(plainUserID, "plain-decision@test.com", "Plain", "member", "api")

	d := authz.Decide(ctx, AuthzRequest{
		Principal:  principalContextForIdentity(plainUser),
		Credential: credentialContextForIdentity(plainUser),
		Resource:   gcpUseResource(saX),
		Action:     ActionUse,
		Permission: permissions.PermissionGCPServiceAccountUse,
	})
	if d.Allowed {
		t.Fatalf("expected deny: a user with no role must not be admitted for gcp_service_account.use")
	}

	adminID := tid("admin-decision")
	createTestUserWithRole(t, s, adminID, "admin-decision@test.com", "admin", store.SystemRoleSuperAdmin)
	admin := NewAuthenticatedUser(adminID, "admin-decision@test.com", "Admin", "admin", "api")

	d = authz.Decide(ctx, AuthzRequest{
		Principal:  principalContextForIdentity(admin),
		Credential: credentialContextForIdentity(admin),
		Resource:   gcpUseResource(saX),
		Action:     ActionUse,
		Permission: permissions.PermissionGCPServiceAccountUse,
	})
	if !d.Allowed {
		t.Fatalf("expected admission: a system-admin user holds every registered permission, denied: %s", d.Reason)
	}
}

// TestGCPServiceAccountUse_NoBuiltInRoleGrantsIt pins that no curated
// built-in role seeds gcp_service_account.use into a principal's effective
// permissions. The super-admin role is exempt by construction: it lists
// every registered permission by design, and IsSystemAdmin/checkUserHoldsPermission
// short-circuit on super-admin status rather than walking its permission
// list, so including it in this walk would not add a meaningful check. No
// other role -- hub-member, hub-viewer, project-owner, project-admin,
// project-member, or any agent role -- grants this permission, the same way
// project-owner grants gcp_service_account.assign but not this permission.
func TestGCPServiceAccountUse_NoBuiltInRoleGrantsIt(t *testing.T) {
	for _, role := range BuiltInRoles() {
		if role.Name == store.SystemRoleSuperAdmin {
			continue
		}
		for _, p := range role.Permissions {
			if p == permissions.PermissionGCPServiceAccountUse {
				t.Errorf("built-in role %q must not grant %s", role.Name, permissions.PermissionGCPServiceAccountUse)
			}
		}
	}
}
