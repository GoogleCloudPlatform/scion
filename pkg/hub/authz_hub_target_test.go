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
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/hub/permissions"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// hubLevelTargetIDs are the instance-level hub resource types
// hubScopedResource serves, each with a representative ID.
var hubLevelTargetIDs = []struct {
	resourceType string
	id           string
}{
	{"role", "role-x"},
	{"role_binding", "binding-x"},
	{"access_constraint", "constraint-x"},
	{"quota", "hub"},
	{"user", "user-x"},
	{"group", "group-x"},
	{"gcp_service_account", "sa-x"},
	{"skill", "registry-x"},
	{"agent", "hub"},
	{"secret", "secret-x"},
}

// TestHubScopedResource_ResolvesHubForEveryHubLevelType pins that the
// adapter is what makes a hub-level instance resolve to the hub scope: the
// adapter's target resolves Hub, and the same type and ID with no parent
// resolves Unknown.
func TestHubScopedResource_ResolvesHubForEveryHubLevelType(t *testing.T) {
	for _, tc := range hubLevelTargetIDs {
		t.Run(tc.resourceType, func(t *testing.T) {
			adapted := hubScopedResource(tc.resourceType, tc.id)
			assert.Equal(t, Resource{Type: tc.resourceType, ID: tc.id, ParentType: "system"}, adapted)
			assert.Equal(t, TargetScope{Kind: TargetScopeHub}, ResolveTargetScope(adapted, TargetScopeEvidence{}))
			assert.Equal(t, TargetScopeUnknown, ResolveTargetScope(Resource{Type: tc.resourceType, ID: tc.id}, TargetScopeEvidence{}).Kind,
				"a parentless %s must not resolve to the hub scope without the adapter", tc.resourceType)

			assert.True(t, isHubScopedResource(adapted))
			assert.True(t, isHubScopedResource(Resource{Type: tc.resourceType, ID: tc.id}))
			assert.False(t, isHubScopedResource(Resource{Type: tc.resourceType, ID: tc.id, ParentType: "project", ParentID: "p"}))
		})
	}
	assert.False(t, isHubScopedResource(Resource{Type: "group", ID: "g", ParentType: "system", ParentID: "x"}),
		"a system parent with a parent ID is malformed, not hub-scoped")
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

// TestHubScopedResource_KernelScopeMatchesParentless pins that session
// decisions do not depend on which hub-level shape a caller builds: for
// every hub-level type and every registry action on it, a session
// decision on {T, ID} equals the decision on hubScopedResource(T, ID), for
// a user holding each seeded system role, a project owner and a user with
// no role.
func TestHubScopedResource_KernelScopeMatchesParentless(t *testing.T) {
	f := newBearerFixture(t, "hubkernel")
	s := f.store
	users := map[string]*AuthenticatedUser{
		"super-admin": seedRoleUser(t, s, tid("hubkernel-super"), store.SystemRoleSuperAdmin, true),
		"hub-admin":   seedRoleUser(t, s, tid("hubkernel-hubadmin"), store.SystemRoleHubAdmin, true),
		"hub-member":  seedRoleUser(t, s, tid("hubkernel-member"), store.SystemRoleHubMember, true),
		"hub-viewer":  seedRoleUser(t, s, tid("hubkernel-viewer"), store.SystemRoleHubViewer, false),
		"no-role":     seedRoleUser(t, s, tid("hubkernel-none"), "", false),
		"proj-owner":  bearerUser(f.ownerA),
	}

	ctx := context.Background()
	allowed, denied := 0, 0
	for _, tc := range hubLevelTargetIDs {
		var actions []string
		for _, p := range permissions.Registry {
			if p.Resource == tc.resourceType {
				actions = append(actions, p.Action)
			}
		}
		require.NotEmpty(t, actions, "no registry actions for %s", tc.resourceType)
		for name, user := range users {
			for _, action := range actions {
				parentless := f.srv.authzService.CheckAccess(ctx, user, Resource{Type: tc.resourceType, ID: tc.id}, Action(action))
				adapted := f.srv.authzService.CheckAccess(ctx, user, hubScopedResource(tc.resourceType, tc.id), Action(action))
				assert.Equal(t, parentless.Allowed, adapted.Allowed, "%s %s:%s: allowed differs (parentless %q, adapted %q)", name, tc.resourceType, action, parentless.Reason, adapted.Reason)
				assert.Equal(t, parentless.Reason, adapted.Reason, "%s %s:%s: reason differs", name, tc.resourceType, action)
				if adapted.Allowed {
					allowed++
				} else {
					denied++
				}
			}
		}
	}
	assert.Positive(t, allowed, "the comparison must include allowed decisions")
	assert.Positive(t, denied, "the comparison must include denied decisions")
}

// TestHubCollectionEvidence_DeniesWhenPermissionDiffers pins that hub
// collection evidence classifies a request only for the permission it
// names: evidence for another permission leaves the target unknown, and
// the bearer gate denies.
func TestHubCollectionEvidence_DeniesWhenPermissionDiffers(t *testing.T) {
	f := newBearerFixture(t, "hubevid-diff")
	ctx := context.Background()
	admin := seedRoleUser(t, f.store, tid("hubevid-diff-super"), store.SystemRoleSuperAdmin, true)
	ceiling := bearerCeiling(t, "group:list", "skill:list")

	mismatched := f.srv.authzService.EvaluateBearerCeiling(ctx, principalContextForIdentity(admin), hubBoundary(), ceiling, "group.list", Resource{Type: "group"}, BearerOptions{Evidence: hubCollectionEvidence("skill.list")})
	assert.False(t, mismatched.Decision.Allowed)
	assert.Equal(t, BearerStageTargetUnknown, mismatched.Stage)
	assert.Equal(t, bearerReasonTargetUnknown, mismatched.Decision.Reason)

	matched := f.srv.authzService.EvaluateBearerCeiling(ctx, principalContextForIdentity(admin), hubBoundary(), ceiling, "group.list", Resource{Type: "group"}, BearerOptions{Evidence: hubCollectionEvidence("group.list")})
	assert.True(t, matched.Decision.Allowed, "matching evidence admits the request: stage %q reason %q", matched.Stage, matched.Decision.Reason)
	assert.Equal(t, TargetScope{Kind: TargetScopeHub}, matched.TargetScope)

	assert.Equal(t, TargetScopeEvidence{IsCollectionLevel: true, CollectionScope: TargetScopeProject, CollectionProjectID: "p1", PermissionID: "agent.list"},
		projectCollectionEvidence("agent.list", "p1"))
}

// TestHubCollectionEvidence_DeniesUnlistedPermission pins that hub
// collection evidence resolves the hub scope only for a permission whose
// collection classes include a hub class: an instance-only
// permission, a self permission and an unregistered ID resolve Unknown.
func TestHubCollectionEvidence_DeniesUnlistedPermission(t *testing.T) {
	for _, permissionID := range []string{"agent.attach", "inbox.read", "user_skill_injection.update", "no.such.permission"} {
		assert.Equal(t, TargetScopeUnknown, ResolveTargetScope(Resource{}, hubCollectionEvidence(permissionID)).Kind, permissionID)
	}
	assert.Equal(t, TargetScopeHub, ResolveTargetScope(Resource{}, hubCollectionEvidence("group.list")).Kind,
		"a permission with a hub collection class resolves Hub")

	f := newBearerFixture(t, "hubevid-unlisted")
	admin := seedRoleUser(t, f.store, tid("hubevid-unlisted-super"), store.SystemRoleSuperAdmin, true)
	eval := f.srv.authzService.EvaluateBearerCeiling(context.Background(), principalContextForIdentity(admin), hubBoundary(), bearerCeiling(t, "agent:attach"), "agent.attach", Resource{}, BearerOptions{Evidence: hubCollectionEvidence("agent.attach")})
	assert.False(t, eval.Decision.Allowed)
	assert.Equal(t, BearerStageTargetUnknown, eval.Stage)
}

// TestBearerGate_UnknownTargetDeniesWithoutEvidence pins that a hub-level
// target built without the adapter is unresolvable and denied for a hub
// token, even one whose holder has full authority, and that the adapter's
// target passes the target stage.
func TestBearerGate_UnknownTargetDeniesWithoutEvidence(t *testing.T) {
	f := newBearerFixture(t, "hubunknown")
	ctx := context.Background()
	admin := seedRoleUser(t, f.store, tid("hubunknown-super"), store.SystemRoleSuperAdmin, true)
	ceiling := bearerCeiling(t, "agent:read", "group:read", "user:read")

	cases := []struct {
		name         string
		target       Resource
		permissionID string
	}{
		{"parentless agent", Resource{Type: "agent", ID: "hub"}, "agent.read"},
		{"unresolved secret", Resource{Type: "secret", ID: "secret-x"}, "secret.use"},
		{"hub quota", Resource{Type: "quota", ID: "hub"}, "quota.read"},
		{"parentless group", Resource{Type: "group", ID: "group-x"}, "group.read"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			eval := f.srv.authzService.EvaluateBearerCeiling(ctx, principalContextForIdentity(admin), hubBoundary(), ceiling, tc.permissionID, tc.target, BearerOptions{})
			assert.False(t, eval.Decision.Allowed)
			assert.Equal(t, BearerStageTargetUnknown, eval.Stage)
			assert.Equal(t, TargetScopeUnknown, eval.TargetScope.Kind)

			adapted := f.srv.authzService.EvaluateBearerCeiling(ctx, principalContextForIdentity(admin), hubBoundary(), ceiling, tc.permissionID, hubScopedResource(tc.target.Type, tc.target.ID), BearerOptions{})
			assert.NotEqual(t, BearerStageTargetUnknown, adapted.Stage, "the adapter's target resolves: reason %q", adapted.Decision.Reason)
			assert.Equal(t, TargetScope{Kind: TargetScopeHub}, adapted.TargetScope)
		})
	}
}

// legacyCeiling returns a frozen ceiling with no version, normalized from
// its stored scopes.
func legacyCeiling(scopes ...string) permissions.FrozenPermissionCeiling {
	return permissions.FrozenPermissionCeiling{Version: permissions.CeilingVersionUnspecified, PermissionIDs: permissions.NormalizeLegacyUATScopes(scopes)}
}

// TestBearerGate_PermissionBoundaryEligibilityAppliesToLegacyCeilings pins
// stage 3b: a permission is usable only on a boundary kind its
// allowed boundaries list. A project-boundary legacy ceiling that carries
// group, hub or scheduled-event permissions is denied on a project target
// for each, with a stable reason, while a permission eligible for the
// project boundary passes the stage.
func TestBearerGate_PermissionBoundaryEligibilityAppliesToLegacyCeilings(t *testing.T) {
	f := newBearerFixture(t, "legacyelig")
	ctx := context.Background()
	owner := bearerUser(f.ownerA)
	scopes := []string{
		"group:read", "group:update", "group:delete", "group:addMember", "group:removeMember", "group:create", "group:list",
		"hub:manage",
		"scheduled_event:read", "scheduled_event:list", "scheduled_event:create", "scheduled_event:update", "scheduled_event:delete",
		"agent:read",
	}
	ceiling := legacyCeiling(scopes...)
	projectGroup := Resource{Type: "group", ID: "group-x", ParentType: "project", ParentID: f.projectA}
	projectEvent := Resource{Type: "scheduled_event", ID: "event-x", ParentType: "project", ParentID: f.projectA}
	projectTarget := Resource{Type: "project", ID: f.projectA}

	cases := []struct {
		permissionID string
		target       Resource
	}{
		{"group.read", projectGroup},
		{"group.update", projectGroup},
		{"group.delete", projectGroup},
		{"group.addMember", projectGroup},
		{"group.removeMember", projectGroup},
		{"group.create", projectGroup},
		{"group.list", projectGroup},
		{"hub.audit.read", projectTarget},
		{"scheduled_event.read", projectEvent},
		{"scheduled_event.list", projectEvent},
		{"scheduled_event.create", projectEvent},
		{"scheduled_event.update", projectEvent},
		{"scheduled_event.delete", projectEvent},
	}
	for _, tc := range cases {
		t.Run(tc.permissionID, func(t *testing.T) {
			require.True(t, ceiling.Allows(tc.permissionID), "the legacy ceiling must carry %s", tc.permissionID)
			eval := f.srv.authzService.EvaluateBearerCeiling(ctx, principalContextForIdentity(owner), projectBoundary(f.projectA), ceiling, tc.permissionID, tc.target, BearerOptions{})
			assert.False(t, eval.Decision.Allowed)
			assert.Equal(t, BearerStageBoundaryEligibility, eval.Stage)
			assert.Equal(t, "permission is not eligible for this token boundary", eval.Decision.Reason)

			// The same rule holds for a real token identity.
			token := NewScopedUserIdentityWithBoundaryAndDecoration(owner, projectBoundary(f.projectA), scopes, tid("legacyelig-cred"), ceiling, nil)
			action, ok := registryActionFor(tc.permissionID)
			require.True(t, ok)
			decision := f.srv.authzService.Decide(ctx, AuthzRequest{
				Principal:  principalContextForIdentity(token),
				Credential: credentialContextForIdentity(token),
				Resource:   tc.target,
				Action:     action,
				Permission: tc.permissionID,
			})
			assert.False(t, decision.Allowed)
			assert.Equal(t, bearerReasonBoundaryIneligible, decision.Reason)
		})
	}

	// A permission eligible for the project boundary passes stage 3b.
	agentTarget := Resource{Type: "agent", ID: f.agentA.ID, ParentType: "project", ParentID: f.projectA}
	eligible := f.srv.authzService.EvaluateBearerCeiling(ctx, principalContextForIdentity(owner), projectBoundary(f.projectA), ceiling, "agent.read", agentTarget, BearerOptions{})
	assert.True(t, eligible.Decision.Allowed, "stage %q reason %q", eligible.Stage, eligible.Decision.Reason)

	// A hub-only permission is eligible on a hub boundary.
	hub := f.srv.authzService.EvaluateBearerCeiling(ctx, principalContextForIdentity(owner), hubBoundary(), ceiling, "group.read", projectGroup, BearerOptions{})
	assert.NotEqual(t, BearerStageBoundaryEligibility, hub.Stage, "reason %q", hub.Decision.Reason)

	// A permission with no boundary entry is eligible on no boundary.
	unlisted := f.srv.authzService.EvaluateBearerCeiling(ctx, principalContextForIdentity(owner), hubBoundary(), ceiling, "scheduled_event.read", projectEvent, BearerOptions{})
	assert.Equal(t, BearerStageBoundaryEligibility, unlisted.Stage)
}

// TestHubSARelationshipRule_AcceptsSystemParentShape pins that the
// hub-member assign rule and the owner rule's hub-scoped exclusion apply to
// a hub-scoped service account in both shapes: no parent, and an explicit
// system parent.
func TestHubSARelationshipRule_AcceptsSystemParentShape(t *testing.T) {
	f := setupHubScopedAssignTest(t)
	ctx := context.Background()
	member := NewAuthenticatedUser(f.member.ID, f.member.Email, f.member.DisplayName, "member", "api")

	parentless := Resource{Type: "gcp_service_account", ID: "sa-hub"}
	systemParent := hubScopedResource("gcp_service_account", "sa-hub")
	for name, r := range map[string]Resource{"parentless": parentless, "system parent": systemParent} {
		t.Run(name, func(t *testing.T) {
			assert.True(t, isHubScopedServiceAccount(r))
			decision := f.srv.authzService.CheckAccess(ctx, member, r, ActionAssign)
			assert.True(t, decision.Allowed, "reason %q", decision.Reason)
			assert.Equal(t, "relationship grant: hub member hub-scoped assign", decision.Reason)

			owned := r
			owned.OwnerID = f.member.ID
			var rules []string
			for _, c := range f.srv.authzService.relationshipCandidates(principalContextForIdentity(member), owned, ActionAssign, "gcp_service_account.assign") {
				rules = append(rules, string(c.rule))
			}
			assert.NotContains(t, rules, string(RelationshipRuleOwner), "assign on a hub-scoped account is governed by hub membership, not ownership")
			assert.Contains(t, rules, string(RelationshipRuleHubMemberSAAssign))
		})
	}
	assert.False(t, isHubScopedServiceAccount(Resource{Type: "gcp_service_account", ID: "sa-p", ParentType: "project", ParentID: f.project.ID}))
}

// TestCheckAccessWithEvidence_EvidenceClassifiesWithoutWidening pins that
// CheckAccessWithEvidence passes server-built evidence to the bearer gate:
// with matching evidence a hub token is decided by its ceiling and the
// holder's live authority, and evidence never admits a request that the
// ceiling, live authority or the token boundary denies.
func TestCheckAccessWithEvidence_EvidenceClassifiesWithoutWidening(t *testing.T) {
	f := newBearerFixture(t, "evidcheck")
	ctx := context.Background()
	authz := f.srv.authzService
	admin := seedRoleUser(t, f.store, tid("evidcheck-super"), store.SystemRoleSuperAdmin, true)
	norole := seedRoleUser(t, f.store, tid("evidcheck-none"), "", false)
	groups := Resource{Type: "group"}
	evidence := hubCollectionEvidence("group.list")

	adminToken := NewScopedUserIdentityWithBoundaryAndDecoration(admin, hubBoundary(), []string{"group:list"}, tid("evidcheck-admin-cred"), bearerCeiling(t, "group:list"), nil)

	t.Run("live authority and the selector allow", func(t *testing.T) {
		d := authz.CheckAccessWithEvidence(ctx, adminToken, groups, ActionList, evidence)
		assert.True(t, d.Allowed, d.Reason)
	})

	t.Run("the evidence reaches the gate", func(t *testing.T) {
		d := authz.CheckAccessWithEvidence(ctx, adminToken, groups, ActionList, TargetScopeEvidence{})
		assert.False(t, d.Allowed)
		assert.Equal(t, unresolvedTargetReason(groups), d.Reason)
		eval := authz.EvaluateBearerCeiling(ctx, principalContextForIdentity(admin), hubBoundary(), adminToken.Ceiling(), "group.list", groups, BearerOptions{})
		assert.Equal(t, BearerStageTargetUnknown, eval.Stage, eval.Decision.Reason)
	})

	t.Run("no live authority denies", func(t *testing.T) {
		token := NewScopedUserIdentityWithBoundaryAndDecoration(norole, hubBoundary(), []string{"group:list"}, tid("evidcheck-none-cred"), bearerCeiling(t, "group:list"), nil)
		d := authz.CheckAccessWithEvidence(ctx, token, groups, ActionList, evidence)
		assert.False(t, d.Allowed)
		eval := authz.EvaluateBearerCeiling(ctx, principalContextForIdentity(norole), hubBoundary(), token.Ceiling(), "group.list", groups, BearerOptions{Evidence: evidence})
		assert.Equal(t, BearerStageAuthority, eval.Stage, eval.Decision.Reason)
	})

	t.Run("a ceiling without the permission denies", func(t *testing.T) {
		token := NewScopedUserIdentityWithBoundaryAndDecoration(admin, hubBoundary(), []string{"group:read"}, tid("evidcheck-read-cred"), bearerCeiling(t, "group:read"), nil)
		d := authz.CheckAccessWithEvidence(ctx, token, groups, ActionList, evidence)
		assert.False(t, d.Allowed)
		eval := authz.EvaluateBearerCeiling(ctx, principalContextForIdentity(admin), hubBoundary(), token.Ceiling(), "group.list", groups, BearerOptions{Evidence: evidence})
		assert.Equal(t, BearerStageCeiling, eval.Stage, eval.Decision.Reason)
	})

	t.Run("evidence does not widen a session decision", func(t *testing.T) {
		withEvidence := authz.CheckAccessWithEvidence(ctx, norole, groups, ActionList, evidence)
		without := authz.CheckAccess(ctx, norole, groups, ActionList)
		assert.False(t, withEvidence.Allowed)
		assert.Equal(t, without.Allowed, withEvidence.Allowed)
	})

	t.Run("evidence naming another project does not widen the boundary", func(t *testing.T) {
		owner := bearerUser(f.ownerA)
		token := NewScopedUserIdentityWithBoundaryAndDecoration(owner, projectBoundary(f.projectA), []string{"agent:list"}, tid("evidcheck-owner-cred"), bearerCeiling(t, "agent:list"), nil)
		own := authz.CheckAccessWithEvidence(ctx, token, Resource{Type: "agent", ParentType: "project", ParentID: f.projectA}, ActionList, projectCollectionEvidence("agent.list", f.projectA))
		assert.True(t, own.Allowed, own.Reason)
		other := authz.CheckAccessWithEvidence(ctx, token, Resource{Type: "agent", ParentType: "project", ParentID: f.projectB}, ActionList, projectCollectionEvidence("agent.list", f.projectB))
		assert.False(t, other.Allowed)
		assert.Equal(t, bearerReasonOutsideProject, other.Reason)
	})
}

// TestAuthorizeWithEvidence_WritesStatusForDecision pins that
// authorizeWithEvidence admits an allowed request, writes 401 when the
// request carries no identity, and writes 403 when the decision denies.
func TestAuthorizeWithEvidence_WritesStatusForDecision(t *testing.T) {
	f := newBearerFixture(t, "evidauthz")
	admin := seedRoleUser(t, f.store, tid("evidauthz-super"), store.SystemRoleSuperAdmin, true)
	evidence := hubCollectionEvidence("group.list")

	run := func(identity Identity) (int, bool) {
		req := httptest.NewRequest(http.MethodGet, "/api/v1/groups", nil)
		if identity != nil {
			req = req.WithContext(contextWithIdentity(req.Context(), identity))
		}
		rec := httptest.NewRecorder()
		ok := f.srv.authorizeWithEvidence(rec, req, Resource{Type: "group"}, ActionList, evidence)
		return rec.Code, ok
	}

	allowed := NewScopedUserIdentityWithBoundaryAndDecoration(admin, hubBoundary(), []string{"group:list"}, tid("evidauthz-list-cred"), bearerCeiling(t, "group:list"), nil)
	code, ok := run(allowed)
	assert.True(t, ok)
	assert.Equal(t, http.StatusOK, code, "nothing is written on allow")

	code, ok = run(nil)
	assert.False(t, ok)
	assert.Equal(t, http.StatusUnauthorized, code)

	denied := NewScopedUserIdentityWithBoundaryAndDecoration(admin, hubBoundary(), []string{"group:read"}, tid("evidauthz-read-cred"), bearerCeiling(t, "group:read"), nil)
	code, ok = run(denied)
	assert.False(t, ok)
	assert.Equal(t, http.StatusForbidden, code)
}
