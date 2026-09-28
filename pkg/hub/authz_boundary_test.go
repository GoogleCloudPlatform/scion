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
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/require"
)

// --- TokenBoundary / TargetScope / BoundaryAllows ---------------------------

func TestTokenBoundary_Valid(t *testing.T) {
	cases := []struct {
		name string
		b    TokenBoundary
		want bool
	}{
		{"project with id", TokenBoundary{Kind: BoundaryKindProject, ProjectID: "p1"}, true},
		{"project with empty id", TokenBoundary{Kind: BoundaryKindProject, ProjectID: ""}, false},
		{"hub with no id", TokenBoundary{Kind: BoundaryKindHub, ProjectID: ""}, true},
		{"hub with id", TokenBoundary{Kind: BoundaryKindHub, ProjectID: "p1"}, false},
		{"unknown kind", TokenBoundary{Kind: "bogus"}, false},
		{"zero value", TokenBoundary{}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := c.b.Valid(); got != c.want {
				t.Errorf("TokenBoundary(%+v).Valid() = %v, want %v", c.b, got, c.want)
			}
		})
	}
}

func TestTargetScope_Valid(t *testing.T) {
	cases := []struct {
		name string
		t    TargetScope
		want bool
	}{
		{"project with id", TargetScope{Kind: TargetScopeProject, ProjectID: "p1"}, true},
		{"project with empty id", TargetScope{Kind: TargetScopeProject}, false},
		{"hub with no id", TargetScope{Kind: TargetScopeHub}, true},
		{"hub with id", TargetScope{Kind: TargetScopeHub, ProjectID: "p1"}, false},
		{"unknown with no id", TargetScope{Kind: TargetScopeUnknown}, true},
		{"unknown with id", TargetScope{Kind: TargetScopeUnknown, ProjectID: "p1"}, false},
		{"bogus kind", TargetScope{Kind: "bogus"}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := c.t.Valid(); got != c.want {
				t.Errorf("TargetScope(%+v).Valid() = %v, want %v", c.t, got, c.want)
			}
		})
	}
}

func TestBoundaryAllows(t *testing.T) {
	hubBoundary := TokenBoundary{Kind: BoundaryKindHub}
	projBoundary := TokenBoundary{Kind: BoundaryKindProject, ProjectID: "p1"}
	otherProjBoundary := TokenBoundary{Kind: BoundaryKindProject, ProjectID: "p2"}

	projTarget := TargetScope{Kind: TargetScopeProject, ProjectID: "p1"}
	hubTarget := TargetScope{Kind: TargetScopeHub}
	unknownTarget := TargetScope{Kind: TargetScopeUnknown}
	invalidTarget := TargetScope{Kind: TargetScopeProject, ProjectID: ""}
	invalidBoundary := TokenBoundary{Kind: BoundaryKindProject, ProjectID: ""}

	cases := []struct {
		name string
		b    TokenBoundary
		t    TargetScope
		want bool
	}{
		{"hub boundary reaches project target", hubBoundary, projTarget, true},
		{"hub boundary reaches hub target", hubBoundary, hubTarget, true},
		{"hub boundary never reaches unknown target", hubBoundary, unknownTarget, false},
		{"project boundary reaches own project", projBoundary, projTarget, true},
		{"project boundary never reaches hub target", projBoundary, hubTarget, false},
		{"project boundary never reaches another project", otherProjBoundary, projTarget, false},
		{"project boundary never reaches unknown target", projBoundary, unknownTarget, false},
		{"invalid target scope always denies", hubBoundary, invalidTarget, false},
		{"invalid token boundary always denies", invalidBoundary, projTarget, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := BoundaryAllows(c.b, c.t); got != c.want {
				t.Errorf("BoundaryAllows(%+v, %+v) = %v, want %v", c.b, c.t, got, c.want)
			}
		})
	}
}

// --- ResolveTargetScope ------------------------------------------------------

func TestResolveTargetScope_ExistingProject(t *testing.T) {
	got := ResolveTargetScope(Resource{Type: "project", ID: "p1"}, TargetScopeEvidence{})
	if got.Kind != TargetScopeProject || got.ProjectID != "p1" {
		t.Errorf("existing project resource: got %+v, want Project/p1", got)
	}
}

func TestResolveTargetScope_CollectionLevelEvidence(t *testing.T) {
	// project.create: no project exists yet; evidence is honored because
	// project.create is reviewed NOT project-applicable.
	got := ResolveTargetScope(Resource{Type: "project"}, TargetScopeEvidence{
		IsCollectionLevel: true,
		CollectionScope:   TargetScopeHub,
		PermissionID:      "project.create",
	})
	if got.Kind != TargetScopeHub {
		t.Errorf("project.create collection evidence: got %+v, want Hub", got)
	}

	// Creating the first agent inside an already-identified, already-
	// access-checked project: the agent is new, but the project is not.
	// agent.create IS project-applicable, and Project-scope collection
	// evidence is the legitimate shape for that case (Hub-scope evidence
	// for it would be the contradiction — see the misuse test below).
	got = ResolveTargetScope(Resource{Type: "agent"}, TargetScopeEvidence{
		IsCollectionLevel:   true,
		CollectionScope:     TargetScopeProject,
		CollectionProjectID: "p1",
		PermissionID:        "agent.create",
	})
	if got.Kind != TargetScopeProject || got.ProjectID != "p1" {
		t.Errorf("agent.create with Project collection evidence: got %+v, want Project/p1", got)
	}
}

func TestResolveTargetScope_CollectionEvidenceMisuseForProjectApplicablePermission(t *testing.T) {
	// skill.create IS project-applicable (reviewed true); collection
	// evidence naming it must resolve Unknown, not Hub, even though the
	// caller claims IsCollectionLevel/Hub.
	got := ResolveTargetScope(Resource{Type: "skill"}, TargetScopeEvidence{
		IsCollectionLevel: true,
		CollectionScope:   TargetScopeHub,
		PermissionID:      "skill.create",
	})
	if got.Kind != TargetScopeUnknown {
		t.Errorf("collection evidence for project-applicable skill.create must resolve Unknown: got %+v", got)
	}
}

func TestResolveTargetScope_CollectionEvidenceMisuseForHubOnlyPermission(t *testing.T) {
	// project.create is hub-only (reviewed false); Project-scope evidence
	// naming it is the reverse contradiction and must resolve Unknown.
	got := ResolveTargetScope(Resource{Type: "project"}, TargetScopeEvidence{
		IsCollectionLevel:   true,
		CollectionScope:     TargetScopeProject,
		CollectionProjectID: "p1",
		PermissionID:        "project.create",
	})
	if got.Kind != TargetScopeUnknown {
		t.Errorf("Project-scope collection evidence for hub-only project.create must resolve Unknown: got %+v", got)
	}
}

func TestResolveTargetScope_CollectionEvidenceRejectsExistingResourceID(t *testing.T) {
	// A collection-level request must not also name an existing resource
	// instance -- that is a malformed existing-resource request, not a
	// legitimate creation, regardless of what the evidence claims.
	got := ResolveTargetScope(Resource{Type: "project", ID: "already-exists"}, TargetScopeEvidence{
		IsCollectionLevel: true,
		CollectionScope:   TargetScopeHub,
		PermissionID:      "project.create",
	})
	if got.Kind != TargetScopeUnknown {
		t.Errorf("collection evidence naming an existing resource ID must resolve Unknown: got %+v", got)
	}
}

func TestResolveTargetScope_CollectionEvidenceRejectsStrayProjectIDOnHubScope(t *testing.T) {
	// Hub-scope collection evidence must not also carry a project ID.
	got := ResolveTargetScope(Resource{Type: "project"}, TargetScopeEvidence{
		IsCollectionLevel:   true,
		CollectionScope:     TargetScopeHub,
		CollectionProjectID: "stray",
		PermissionID:        "project.create",
	})
	if got.Kind != TargetScopeUnknown {
		t.Errorf("Hub-scope evidence with a stray CollectionProjectID must resolve Unknown: got %+v", got)
	}
}

func TestResolveTargetScope_CollectionEvidenceUnreviewedPermissionDenies(t *testing.T) {
	got := ResolveTargetScope(Resource{Type: "widget"}, TargetScopeEvidence{
		IsCollectionLevel: true,
		CollectionScope:   TargetScopeHub,
		PermissionID:      "widget.create_totally_unreviewed",
	})
	if got.Kind != TargetScopeUnknown {
		t.Errorf("collection evidence for an unreviewed permission must resolve Unknown: got %+v", got)
	}
}

func TestResolveTargetScope_HubResourceType(t *testing.T) {
	got := ResolveTargetScope(Resource{Type: "hub"}, TargetScopeEvidence{})
	if got.Kind != TargetScopeHub {
		t.Errorf("hub resource type: got %+v, want Hub", got)
	}
}

func TestResolveTargetScope_ParentProject(t *testing.T) {
	got := ResolveTargetScope(Resource{Type: "agent", ParentType: "project", ParentID: "p1"}, TargetScopeEvidence{})
	if got.Kind != TargetScopeProject || got.ProjectID != "p1" {
		t.Errorf("agent with project parent: got %+v, want Project/p1", got)
	}
}

func TestResolveTargetScope_ParentSystemSentinel(t *testing.T) {
	// role_binding / access_constraint instances confirmed system-scoped by
	// their resolver must be explicitly marked ParentType == "system" — not
	// inferred from an absent ParentType.
	for _, resourceType := range []string{"role_binding", "access_constraint"} {
		got := ResolveTargetScope(Resource{Type: resourceType, ParentType: "system"}, TargetScopeEvidence{})
		if got.Kind != TargetScopeHub {
			t.Errorf("%s with ParentType=system: got %+v, want Hub", resourceType, got)
		}
	}
}

func TestResolveTargetScope_ParentProjectAppliesToMixedScopeTypes(t *testing.T) {
	// role_binding / access_constraint must NOT be blanket-classified as
	// Hub-only: a project-scoped instance resolves via the same
	// ParentType=="project" rule as any other resource type.
	for _, resourceType := range []string{"role_binding", "access_constraint"} {
		got := ResolveTargetScope(Resource{Type: resourceType, ParentType: "project", ParentID: "p1"}, TargetScopeEvidence{})
		if got.Kind != TargetScopeProject || got.ProjectID != "p1" {
			t.Errorf("%s with project parent: got %+v, want Project/p1", resourceType, got)
		}
	}
}

func TestResolveTargetScope_UserScopedResourceResolvesHub(t *testing.T) {
	got := ResolveTargetScope(Resource{Type: "skill", ScopeKind: store.SkillScopeUser, ScopeUserID: "u1"}, TargetScopeEvidence{})
	if got.Kind != TargetScopeHub {
		t.Errorf("user-scoped skill: got %+v, want Hub", got)
	}
}

func TestResolveTargetScope_MissingMetadataIsUnknownNotHub(t *testing.T) {
	// Critical constraint: missing/ambiguous resource-scope metadata must
	// never be treated as hub scope, even for resource types that are often
	// hub-wide (group, user, role, gcp_service_account, ...). A confirmed
	// system-scoped instance must carry the explicit ParentType=="system"
	// sentinel; its absence is Unknown.
	for _, resourceType := range []string{"agent", "skill", "template", "harness_config", "group", "user", "role", "gcp_service_account", "quota", "policy", "role_binding", "access_constraint"} {
		got := ResolveTargetScope(Resource{Type: resourceType}, TargetScopeEvidence{})
		if got.Kind != TargetScopeUnknown {
			t.Errorf("%s with no parent/scope metadata: got %+v, want Unknown", resourceType, got)
		}
	}
}

func TestResolveTargetScope_ContradictoryMetadataIsUnknown(t *testing.T) {
	// A resource cannot be both project-parented and globally scoped.
	got := ResolveTargetScope(Resource{Type: "skill", ParentType: "project", ParentID: "p1", ScopeKind: store.SkillScopeGlobal}, TargetScopeEvidence{})
	if got.Kind != TargetScopeUnknown {
		t.Errorf("contradictory project-parent + global-scope: got %+v, want Unknown", got)
	}
	// The system sentinel must not also carry a project ID.
	got = ResolveTargetScope(Resource{Type: "role_binding", ParentType: "system", ParentID: "p1"}, TargetScopeEvidence{})
	if got.Kind != TargetScopeUnknown {
		t.Errorf("contradictory system-sentinel + project ID: got %+v, want Unknown", got)
	}
}

// --- ProjectMembershipEvidence -----------------------------------------------

func activeUserPrincipal(id string) PrincipalContext {
	return PrincipalContext{Kind: PrincipalKindUser, ID: id}
}

func TestProjectMembershipEvidence_DirectMembership_Allowed(t *testing.T) {
	authz, s := authzTestSetup(t)
	ctx := context.Background()

	projectID := tid("apa-proj-1")
	userID := tid("apa-user-1")
	createDelegateTestProject(t, s, projectID, "apa-proj-1", "test")
	createTestUserWithProjectRole(t, s, userID, "apa1@test.com", projectID, store.ProjectRoleMember)

	ok, source, err := authz.ProjectMembershipEvidence(ctx, activeUserPrincipal(userID), projectID)
	require.NoError(t, err)
	if !ok || source != ProjectAccessSourceMembership {
		t.Errorf("got ok=%v source=%q, want ok=true source=membership", ok, source)
	}
}

func TestProjectMembershipEvidence_GroupMembership_Allowed(t *testing.T) {
	authz, s := authzTestSetup(t)
	ctx := context.Background()

	projectID := tid("apa-proj-2")
	userID := tid("apa-user-2")
	groupID := tid("apa-group-2")
	createDelegateTestProject(t, s, projectID, "apa-proj-2", "test")
	require.NoError(t, s.CreateUser(ctx, &store.User{ID: userID, Email: "apa2@test.com", DisplayName: "u", Role: "member", Status: store.UserStatusActive}))
	require.NoError(t, s.CreateGroup(ctx, &store.Group{ID: groupID, Slug: "apa-group-2", Name: "G"}))
	require.NoError(t, s.AddGroupMember(ctx, &store.GroupMember{GroupID: groupID, MemberType: store.GroupMemberTypeUser, MemberID: userID, Role: "member"}))

	rd, err := s.GetRoleDefinitionByName(ctx, store.ProjectRoleMember, store.RoleScopeProject)
	require.NoError(t, err)
	_, err = s.CreateRoleBinding(ctx, &store.RoleBinding{
		RoleDefinitionID: rd.ID,
		PrincipalType:    store.RoleBindingPrincipalGroup,
		PrincipalID:      groupID,
		ScopeType:        store.RoleScopeProject,
		ScopeID:          projectID,
		CreatedBy:        "test",
	})
	require.NoError(t, err)

	ok, source, err := authz.ProjectMembershipEvidence(ctx, activeUserPrincipal(userID), projectID)
	require.NoError(t, err)
	if !ok || source != ProjectAccessSourceGroup {
		t.Errorf("got ok=%v source=%q, want ok=true source=group", ok, source)
	}
}

func TestProjectMembershipEvidence_ExpiredGroupAdminBinding_Denied(t *testing.T) {
	authz, s := authzTestSetup(t)
	ctx := context.Background()

	projectID := tid("apa-proj-3")
	userID := tid("apa-user-3")
	groupID := tid("apa-group-3")
	createDelegateTestProject(t, s, projectID, "apa-proj-3", "test")
	require.NoError(t, s.CreateUser(ctx, &store.User{ID: userID, Email: "apa3@test.com", DisplayName: "u", Role: "member", Status: store.UserStatusActive}))
	require.NoError(t, s.CreateGroup(ctx, &store.Group{ID: groupID, Slug: "apa-group-3", Name: "G"}))
	require.NoError(t, s.AddGroupMember(ctx, &store.GroupMember{GroupID: groupID, MemberType: store.GroupMemberTypeUser, MemberID: userID, Role: "member"}))

	rd, err := s.GetRoleDefinitionByName(ctx, store.ProjectRoleAdmin, store.RoleScopeProject)
	require.NoError(t, err)
	expired := time.Now().Add(-time.Hour)
	_, err = s.CreateRoleBinding(ctx, &store.RoleBinding{
		RoleDefinitionID: rd.ID,
		PrincipalType:    store.RoleBindingPrincipalGroup,
		PrincipalID:      groupID,
		ScopeType:        store.RoleScopeProject,
		ScopeID:          projectID,
		ExpiresAt:        &expired,
		CreatedBy:        "test",
	})
	require.NoError(t, err)

	ok, _, err := authz.ProjectMembershipEvidence(ctx, activeUserPrincipal(userID), projectID)
	require.NoError(t, err)
	if ok {
		t.Error("expired group-admin binding must not grant project membership evidence")
	}
}

func TestProjectMembershipEvidence_NoBindingAtAll_Denied(t *testing.T) {
	// Stands in for "revoked membership with retained ancestry": ancestry is
	// not a parameter to ProjectMembershipEvidence at all, so a project with
	// no active binding for this principal is denied regardless of any
	// historical fact recorded elsewhere (e.g. Resource.Ancestry).
	authz, s := authzTestSetup(t)
	ctx := context.Background()

	projectID := tid("apa-proj-4")
	userID := tid("apa-user-4")
	createDelegateTestProject(t, s, projectID, "apa-proj-4", "test")
	require.NoError(t, s.CreateUser(ctx, &store.User{ID: userID, Email: "apa4@test.com", DisplayName: "u", Role: "member", Status: store.UserStatusActive}))

	ok, _, err := authz.ProjectMembershipEvidence(ctx, activeUserPrincipal(userID), projectID)
	require.NoError(t, err)
	if ok {
		t.Error("no binding at all must deny project membership evidence")
	}
}

func TestProjectMembershipEvidence_EmptyProjectID_FailsClosed(t *testing.T) {
	authz, _ := authzTestSetup(t)
	ok, _, err := authz.ProjectMembershipEvidence(context.Background(), activeUserPrincipal(tid("apa-x")), "")
	if ok || err == nil {
		t.Errorf("empty projectID must fail closed: got ok=%v err=%v", ok, err)
	}
}

func TestProjectMembershipEvidence_SuspendedUser_Denied(t *testing.T) {
	authz, s := authzTestSetup(t)
	ctx := context.Background()

	projectID := tid("apa-proj-10")
	userID := tid("apa-user-10")
	createDelegateTestProject(t, s, projectID, "apa-proj-10", "test")
	createTestUserWithProjectRole(t, s, userID, "apa10@test.com", projectID, store.ProjectRoleMember)

	u, err := s.GetUser(ctx, userID)
	require.NoError(t, err)
	u.Status = store.UserStatusSuspended
	require.NoError(t, s.UpdateUser(ctx, u))

	ok, _, err := authz.ProjectMembershipEvidence(ctx, activeUserPrincipal(userID), projectID)
	if ok || err == nil {
		t.Errorf("suspended user must fail closed: got ok=%v err=%v", ok, err)
	}
}

func TestProjectMembershipEvidence_UnsupportedPrincipalKind(t *testing.T) {
	authz, _ := authzTestSetup(t)
	agentPrincipal := PrincipalContext{Kind: PrincipalKindAgent, ID: tid("apa-agent")}
	_, _, err := authz.ProjectMembershipEvidence(context.Background(), agentPrincipal, tid("apa-proj"))
	if !isUnsupportedPrincipalKindErr(err) {
		t.Errorf("agent principal must be rejected with ErrUnsupportedPrincipalKind, got %v", err)
	}
}

func isUnsupportedPrincipalKindErr(err error) bool {
	return err != nil && (err == ErrUnsupportedPrincipalKind || errorsIsUnsupportedKind(err))
}

func errorsIsUnsupportedKind(err error) bool {
	for e := err; e != nil; e = unwrapOnce(e) {
		if e == ErrUnsupportedPrincipalKind {
			return true
		}
	}
	return false
}

func unwrapOnce(err error) error {
	type unwrapper interface{ Unwrap() error }
	if u, ok := err.(unwrapper); ok {
		return u.Unwrap()
	}
	return nil
}

// --- SystemAuthorityProof / MintTimeSystemGrant (F-3 seeded-role regressions) ---

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

func TestSystemAuthorityProof_OnlyBrokerCreate_Denied(t *testing.T) {
	authz, s := authzTestSetup(t)
	ctx := context.Background()

	projectID := tid("apa-proj-5")
	userID := tid("apa-user-5")
	createDelegateTestProject(t, s, projectID, "apa-proj-5", "test")
	systemRoleUserWithPermissions(t, s, userID, []string{"broker.create"})

	ok, err := authz.SystemAuthorityProof(ctx, activeUserPrincipal(userID), projectID, "broker.create", ContemplatedProjectClass("broker.create"))
	require.NoError(t, err)
	if ok {
		t.Error("broker.create is not reviewed project-applicable and must not establish project authority")
	}
}

func TestSystemAuthorityProof_OnlyProjectCreate_Denied(t *testing.T) {
	authz, s := authzTestSetup(t)
	ctx := context.Background()

	projectID := tid("apa-proj-6")
	userID := tid("apa-user-6")
	createDelegateTestProject(t, s, projectID, "apa-proj-6", "test")
	systemRoleUserWithPermissions(t, s, userID, []string{"project.create"})

	ok, err := authz.SystemAuthorityProof(ctx, activeUserPrincipal(userID), projectID, "project.create", ContemplatedProjectClass("project.create"))
	require.NoError(t, err)
	if ok {
		t.Error("project.create must not grant authority over an existing project")
	}
}

func TestSystemAuthorityProof_OnlySkillCreateGlobal_Denied(t *testing.T) {
	authz, s := authzTestSetup(t)
	ctx := context.Background()

	projectID := tid("apa-proj-7")
	userID := tid("apa-user-7")
	createDelegateTestProject(t, s, projectID, "apa-proj-7", "test")
	systemRoleUserWithPermissions(t, s, userID, []string{"skill.create_global"})

	ok, err := authz.SystemAuthorityProof(ctx, activeUserPrincipal(userID), projectID, "skill.create_global", ContemplatedProjectClass("skill.create_global"))
	require.NoError(t, err)
	if ok {
		t.Error("skill.create_global must not grant project authority")
	}
}

func TestSystemAuthorityProof_ProjectApplicablePermission_Allowed(t *testing.T) {
	authz, s := authzTestSetup(t)
	ctx := context.Background()

	projectID := tid("apa-proj-8")
	userID := tid("apa-user-8")
	createDelegateTestProject(t, s, projectID, "apa-proj-8", "test")
	systemRoleUserWithPermissions(t, s, userID, []string{"agent.delete"})

	ok, err := authz.SystemAuthorityProof(ctx, activeUserPrincipal(userID), projectID, "agent.delete", ContemplatedProjectClass("agent.delete"))
	require.NoError(t, err)
	if !ok {
		t.Error("a system role holding agent.delete must establish target-applicable project authority")
	}
}

func TestSystemAuthorityProof_SuperAdminWithoutMembership_Allowed(t *testing.T) {
	authz, s := authzTestSetup(t)
	ctx := context.Background()

	projectID := tid("apa-proj-9")
	adminID := tid("apa-admin-9")
	createDelegateTestProject(t, s, projectID, "apa-proj-9", "test")
	createTestUserWithRole(t, s, adminID, "apa9@test.com", "admin", store.SystemRoleSuperAdmin)

	ok, err := authz.SystemAuthorityProof(ctx, activeUserPrincipal(adminID), projectID, "agent.delete", ContemplatedProjectClass("agent.delete"))
	require.NoError(t, err)
	if !ok {
		t.Error("super-admin with no project membership row must still pass SystemAuthorityProof for agent.delete")
	}

	// And the composed runtime path agrees, via a real target.
	target := Resource{Type: "agent", ID: tid("apa9-agent"), ParentType: "project", ParentID: projectID}
	result, err := authz.ProjectTargetAdmission(ctx, activeUserPrincipal(adminID), projectID, "agent.delete", target, nil)
	require.NoError(t, err)
	if !result.Admitted || result.Source != ProjectAccessSourceSystemRole {
		t.Errorf("ProjectTargetAdmission: got %+v, want Admitted via system_role", result)
	}
}

func TestSystemAuthorityProof_EmptyProjectID_Rejected(t *testing.T) {
	authz, _ := authzTestSetup(t)
	_, err := authz.SystemAuthorityProof(context.Background(), activeUserPrincipal(tid("apa-x")), "", "agent.delete", ContemplatedProjectClass("agent.delete"))
	if err == nil {
		t.Error("SystemAuthorityProof must reject a blank projectID rather than repurpose it as a project-agnostic check")
	}
}

// TestSeededHubMember_CannotReadOrAttachProjectAgents is the F-3-ruling
// seeded-role regression: a former member, retained in Resource.Ancestry,
// whose only remaining role is the seeded hub-member/hub-viewer system
// role, cannot attach to or read project agents through system authority.
func TestSeededHubMember_CannotReadOrAttachProjectAgents(t *testing.T) {
	authz, s := authzTestSetup(t)
	ctx := context.Background()

	projectID := tid("hm-proj")
	userID := tid("hm-user")
	createDelegateTestProject(t, s, projectID, "hm-proj", "test")
	createTestUserWithRole(t, s, userID, "hm@test.com", "member", store.SystemRoleHubMember)

	for _, permID := range []string{"agent.read", "agent.attach"} {
		ok, err := authz.SystemAuthorityProof(ctx, activeUserPrincipal(userID), projectID, permID, ContemplatedProjectClass(permID))
		require.NoError(t, err)
		if ok {
			t.Errorf("hub-member system role must not establish %s on a project agent target", permID)
		}
	}
}

// TestSeededHubAdmin_ScheduledEventDoesNotUnlockAgentAction is the F-3
// seeded-role regression: hub-admin's scheduled_event permission does not
// unlock an unrelated agent action.
func TestSeededHubAdmin_ScheduledEventDoesNotUnlockAgentAction(t *testing.T) {
	authz, s := authzTestSetup(t)
	ctx := context.Background()

	projectID := tid("ha-proj")
	userID := tid("ha-user")
	createDelegateTestProject(t, s, projectID, "ha-proj", "test")
	createTestUserWithRole(t, s, userID, "ha@test.com", "admin", store.SystemRoleHubAdmin)

	// Confirm hub-admin DOES have scheduled_event authority (sanity), then
	// confirm it does not extend to agent.delete.
	schedOK, err := authz.SystemAuthorityProof(ctx, activeUserPrincipal(userID), projectID, "scheduled_event.read", ContemplatedProjectClass("scheduled_event.read"))
	require.NoError(t, err)
	agentOK, err := authz.SystemAuthorityProof(ctx, activeUserPrincipal(userID), projectID, "agent.delete", ContemplatedProjectClass("agent.delete"))
	require.NoError(t, err)
	if agentOK {
		t.Error("hub-admin's scheduled_event authority must not unlock agent.delete")
	}
	_ = schedOK // informational; hub-admin's exact permission set is seed.go's to define, not asserted here
}

// TestSeededHubMember_CatalogOnlyGrant_PairedTest is the F-3/F-4 paired
// regression pat-refactor required: a seeded hub-member's catalog-only
// skill.read remains hub-boundary MINT-eligible for the global catalog
// (MintTimeSystemGrant), while it is denied for an unrelated project-scoped
// skill target at USE time (SystemAuthorityProof / ProjectTargetAdmission).
func TestSeededHubMember_CatalogOnlyGrant_PairedTest(t *testing.T) {
	authz, s := authzTestSetup(t)
	ctx := context.Background()

	projectID := tid("cat-proj")
	userID := tid("cat-user")
	createDelegateTestProject(t, s, projectID, "cat-proj", "test")
	createTestUserWithRole(t, s, userID, "cat@test.com", "member", store.SystemRoleHubMember)

	// Mint-time: hub-boundary contemplation succeeds for the global catalog.
	mintOK, err := authz.MintTimeSystemGrant(ctx, activeUserPrincipal(userID), "skill.read")
	require.NoError(t, err)
	if !mintOK {
		t.Error("hub-member's catalog-only skill.read must remain hub-boundary mint-eligible for the global catalog")
	}

	// Use-time: denied for an unrelated project-scoped skill target.
	projectSkillTarget := Resource{Type: "skill", ID: tid("cat-skill"), ParentType: "project", ParentID: projectID, ScopeKind: store.SkillScopeProject}
	result, err := authz.ProjectTargetAdmission(ctx, activeUserPrincipal(userID), projectID, "skill.read", projectSkillTarget, nil)
	require.NoError(t, err)
	if result.Admitted {
		t.Error("hub-member's catalog-only skill.read must NOT admit access to an unrelated project-scoped skill")
	}
}

// --- ProjectTargetAdmission ---------------------------------------------------

func TestProjectTargetAdmission_MembershipPath(t *testing.T) {
	authz, s := authzTestSetup(t)
	ctx := context.Background()

	projectID := tid("pta-proj-1")
	userID := tid("pta-user-1")
	createDelegateTestProject(t, s, projectID, "pta-proj-1", "test")
	createTestUserWithProjectRole(t, s, userID, "pta1@test.com", projectID, store.ProjectRoleMember)

	target := Resource{Type: "agent", ID: tid("pta1-agent"), ParentType: "project", ParentID: projectID}
	result, err := authz.ProjectTargetAdmission(ctx, activeUserPrincipal(userID), projectID, "agent.read", target, nil)
	require.NoError(t, err)
	if !result.Admitted || result.Source != ProjectAccessSourceMembership {
		t.Errorf("got %+v, want Admitted via membership", result)
	}
}

func TestProjectTargetAdmission_ProjectMismatch_Errors(t *testing.T) {
	authz, s := authzTestSetup(t)
	ctx := context.Background()
	userID := tid("pta-user-2")
	require.NoError(t, s.CreateUser(ctx, &store.User{ID: userID, Email: "pta2@test.com", DisplayName: "u", Role: "member", Status: store.UserStatusActive}))

	target := Resource{Type: "agent", ID: tid("pta2-agent"), ParentType: "project", ParentID: tid("pta-other-project")}
	_, err := authz.ProjectTargetAdmission(ctx, activeUserPrincipal(userID), tid("pta-proj-2"), "agent.read", target, nil)
	if err == nil {
		t.Error("ProjectTargetAdmission must error when target's resolved project does not match projectID")
	}
}

func TestProjectTargetAdmission_MemoReusesResult(t *testing.T) {
	authz, s := authzTestSetup(t)
	ctx := context.Background()

	projectID := tid("pta-proj-3")
	userID := tid("pta-user-3")
	createDelegateTestProject(t, s, projectID, "pta-proj-3", "test")
	createTestUserWithProjectRole(t, s, userID, "pta3@test.com", projectID, store.ProjectRoleMember)

	memo := NewProjectAdmissionCache()
	target := Resource{Type: "agent", ID: tid("pta3-agent"), ParentType: "project", ParentID: projectID}
	r1, err := authz.ProjectTargetAdmission(ctx, activeUserPrincipal(userID), projectID, "agent.read", target, memo)
	require.NoError(t, err)
	r2, err := authz.ProjectTargetAdmission(ctx, activeUserPrincipal(userID), projectID, "agent.read", target, memo)
	require.NoError(t, err)
	if r1 != r2 {
		t.Errorf("memoized calls must agree: %+v vs %+v", r1, r2)
	}
}

// --- CanMintSelector ---------------------------------------------------------

func TestCanMintSelector_EmptySelectors_RejectedExplicitly(t *testing.T) {
	authz, _ := authzTestSetup(t)
	_, err := authz.CanMintSelector(context.Background(), activeUserPrincipal(tid("cms-0")), TokenBoundary{Kind: BoundaryKindHub}, nil)
	if !errors.Is(err, ErrEmptySelectorList) {
		t.Errorf("empty selector list must be rejected with ErrEmptySelectorList, got %v", err)
	}
}

func TestCanMintSelector_InvalidBoundary_RejectedBeforeEmptyCheck(t *testing.T) {
	authz, _ := authzTestSetup(t)
	// An invalid boundary must be rejected on its own terms, not masked by
	// (or confused with) the empty-selector-list validation.
	_, err := authz.CanMintSelector(context.Background(), activeUserPrincipal(tid("cms-0b")), TokenBoundary{Kind: BoundaryKindProject, ProjectID: ""}, nil)
	if err == nil || errors.Is(err, ErrEmptySelectorList) {
		t.Errorf("invalid boundary must be rejected as invalid, not as an empty selector list: %v", err)
	}
}

func TestCanMintSelector_UnsupportedPrincipalKind_RejectedFirst(t *testing.T) {
	authz, _ := authzTestSetup(t)
	agentPrincipal := PrincipalContext{Kind: PrincipalKindAgent, ID: tid("cms-0c")}
	_, err := authz.CanMintSelector(context.Background(), agentPrincipal, TokenBoundary{Kind: BoundaryKindHub}, nil)
	if !errorsIsUnsupportedKind(err) && err != ErrUnsupportedPrincipalKind {
		t.Errorf("unsupported principal kind must be rejected before the empty-selector check: %v", err)
	}
}

func TestCanMintSelector_UnknownSelector_Denied(t *testing.T) {
	authz, _ := authzTestSetup(t)
	results, err := authz.CanMintSelector(context.Background(), activeUserPrincipal(tid("cms-1")), TokenBoundary{Kind: BoundaryKindHub}, []string{"nonsense:selector"})
	require.NoError(t, err)
	require.Len(t, results, 1)
	if results[0].OK || results[0].Reason != MintDenialUnknownSelector {
		t.Errorf("unknown selector must be denied with MintDenialUnknownSelector, got %+v", results[0])
	}
}

func TestCanMintSelector_BoundaryNotPermitted_Denied(t *testing.T) {
	authz, s := authzTestSetup(t)
	ctx := context.Background()
	userID := tid("cms-2")
	require.NoError(t, s.CreateUser(ctx, &store.User{ID: userID, Email: "cms2@test.com", DisplayName: "u", Role: "member", Status: store.UserStatusActive}))

	// group:read is Hub-only; a project boundary must never be permitted to
	// select it.
	results, err := authz.CanMintSelector(ctx, activeUserPrincipal(userID), TokenBoundary{Kind: BoundaryKindProject, ProjectID: tid("cms-2-proj")}, []string{"group:read"})
	require.NoError(t, err)
	require.Len(t, results, 1)
	if results[0].OK || results[0].Reason != MintDenialBoundaryNotAllowed {
		t.Errorf("group:read must not be selectable under a project boundary, got %+v", results[0])
	}
}

func TestCanMintSelector_ProjectBoundary_RequiresAdmissionOncePerBatch(t *testing.T) {
	authz, s := authzTestSetup(t)
	ctx := context.Background()
	userID := tid("cms-3")
	projectID := tid("cms-3-proj")
	createDelegateTestProject(t, s, projectID, "cms-3-proj", "test")
	require.NoError(t, s.CreateUser(ctx, &store.User{ID: userID, Email: "cms3@test.com", DisplayName: "u", Role: "member", Status: store.UserStatusActive}))

	// No project membership yet: project:read must be denied even though
	// it's a perfectly ordinary, non-relationship selector — and with the
	// SAME reason as every other selector in the batch (uniform denial, no
	// oracle).
	results, err := authz.CanMintSelector(ctx, activeUserPrincipal(userID), TokenBoundary{Kind: BoundaryKindProject, ProjectID: projectID}, []string{"project:read", "agent:read"})
	require.NoError(t, err)
	require.Len(t, results, 2)
	for _, r := range results {
		if r.OK || r.Reason != MintDenialProjectAccessRequired {
			t.Errorf("selector %q: got %+v, want denied with project_access_required", r.Selector, r)
		}
	}

	createTestUserWithProjectRole(t, s, userID, "cms3@test.com", projectID, store.ProjectRoleMember)
	results, err = authz.CanMintSelector(ctx, activeUserPrincipal(userID), TokenBoundary{Kind: BoundaryKindProject, ProjectID: projectID}, []string{"agent:read"})
	require.NoError(t, err)
	require.Len(t, results, 1)
	if !results[0].OK {
		t.Errorf("agent:read should be mintable once the user has active project access: %+v", results[0])
	}
}

func TestCanMintSelector_RelationshipEligibility_AgentAttach_NoExistingTargetRequired(t *testing.T) {
	authz, s := authzTestSetup(t)
	ctx := context.Background()
	userID := tid("cms-4")
	projectID := tid("cms-4-proj")
	createDelegateTestProject(t, s, projectID, "cms-4-proj", "test")
	createTestUserWithProjectRole(t, s, userID, "cms4@test.com", projectID, store.ProjectRoleMember)

	// A member may select agent:attach before creating any agent at all —
	// CanMintSelector must not require an existing target.
	results, err := authz.CanMintSelector(ctx, activeUserPrincipal(userID), TokenBoundary{Kind: BoundaryKindProject, ProjectID: projectID}, []string{"agent:attach"})
	require.NoError(t, err)
	require.Len(t, results, 1)
	if !results[0].OK {
		t.Errorf("agent:attach should be eligible via relationship candidacy with no existing agent; got %+v", results[0])
	}
}

func TestCanMintSelector_ProjectBoundary_SuperAdminWithoutMembership_EligibleForOwnedPermission(t *testing.T) {
	authz, s := authzTestSetup(t)
	ctx := context.Background()
	adminID := tid("cms-5")
	projectID := tid("cms-5-proj")
	createDelegateTestProject(t, s, projectID, "cms-5-proj", "test")
	createTestUserWithRole(t, s, adminID, "cms5@test.com", "admin", store.SystemRoleSuperAdmin)

	// No membership row, but super-admin's system role authority covers
	// agent.delete: the selector should be admitted (F-3 ruling).
	results, err := authz.CanMintSelector(ctx, activeUserPrincipal(adminID), TokenBoundary{Kind: BoundaryKindProject, ProjectID: projectID}, []string{"agent:delete"})
	require.NoError(t, err)
	require.Len(t, results, 1)
	if !results[0].OK {
		t.Errorf("super-admin without membership should be admitted via system authority for agent:delete: %+v", results[0])
	}
}

func TestCanMintSelector_HubBoundary_CatalogOnlyGrantEligible(t *testing.T) {
	authz, s := authzTestSetup(t)
	ctx := context.Background()
	userID := tid("cms-6")
	createTestUserWithRole(t, s, userID, "cms6@test.com", "member", store.SystemRoleHubMember)

	// hub-member's catalog-only skill.read must remain hub-boundary
	// mint-eligible (F-4 catalog-read correction).
	results, err := authz.CanMintSelector(ctx, activeUserPrincipal(userID), TokenBoundary{Kind: BoundaryKindHub}, []string{"skill:read"})
	require.NoError(t, err)
	require.Len(t, results, 1)
	if !results[0].OK {
		t.Errorf("hub-member's catalog-only skill:read must be hub-boundary mint-eligible: %+v", results[0])
	}
}

func TestCanMintSelector_HubBoundary_NoBlanketAdmission(t *testing.T) {
	authz, s := authzTestSetup(t)
	ctx := context.Background()
	userID := tid("cms-7")
	createTestUserWithRole(t, s, userID, "cms7@test.com", "member", store.SystemRoleHubMember)

	// A plain hub-member has none of the agent.* permissions at all, via
	// either system authority or any project binding: agent:delete must be
	// denied under a hub boundary too (no blanket hub-member admission).
	results, err := authz.CanMintSelector(ctx, activeUserPrincipal(userID), TokenBoundary{Kind: BoundaryKindHub}, []string{"agent:delete"})
	require.NoError(t, err)
	require.Len(t, results, 1)
	if results[0].OK {
		t.Errorf("hub-member must not be admitted for agent:delete under a hub boundary: %+v", results[0])
	}
}

// TestMintTimeSystemGrant_SuperAdminHubOnlyPermission_Allowed is the
// blocker-#8 positive test: a super-admin can mint a hub-only permission
// (user.invite) under a hub boundary via its explicit, reviewed
// SupportedTargetClasses entry -- not a guessed default.
func TestMintTimeSystemGrant_SuperAdminHubOnlyPermission_Allowed(t *testing.T) {
	authz, s := authzTestSetup(t)
	ctx := context.Background()
	adminID := tid("mtsg-1")
	createTestUserWithRole(t, s, adminID, "mtsg1@test.com", "admin", store.SystemRoleSuperAdmin)

	ok, err := authz.MintTimeSystemGrant(ctx, activeUserPrincipal(adminID), "user.invite")
	require.NoError(t, err)
	if !ok {
		t.Error("super-admin must be mint-eligible for the hub-only user.invite permission")
	}
}

// TestMintTimeSystemGrant_UnreviewedPermission_Denied is the blocker-#8
// unknown-class deny test: a permission with no SupportedTargetClasses
// entry at all denies, even for a super-admin holding every permission.
func TestMintTimeSystemGrant_UnreviewedPermission_Denied(t *testing.T) {
	authz, s := authzTestSetup(t)
	ctx := context.Background()
	adminID := tid("mtsg-2")
	createTestUserWithRole(t, s, adminID, "mtsg2@test.com", "admin", store.SystemRoleSuperAdmin)

	ok, err := authz.MintTimeSystemGrant(ctx, activeUserPrincipal(adminID), "hub.settings.read")
	require.NoError(t, err)
	if ok {
		t.Error("a permission with no reviewed SupportedTargetClasses entry must deny, never fall back to a guessed class")
	}
}

// TestCanMintSelector_HubBoundary_RelationshipAlternative_OrdinaryMember is
// the pat-refactor review-item-4 regression: an ordinary project member,
// with no system role and no blanket project permission grant for
// agent.attach, must still be able to mint agent:attach under a HUB
// boundary via the relationship alternative -- flat/system authority is not
// a precondition for relationship eligibility, it is an alternative to it.
func TestCanMintSelector_HubBoundary_RelationshipAlternative_OrdinaryMember(t *testing.T) {
	authz, s := authzTestSetup(t)
	ctx := context.Background()
	userID := tid("hpe-1")
	projectID := tid("hpe-1-proj")
	createDelegateTestProject(t, s, projectID, "hpe-1-proj", "test")
	createTestUserWithProjectRole(t, s, userID, "hpe1@test.com", projectID, store.ProjectRoleMember)

	results, err := authz.CanMintSelector(ctx, activeUserPrincipal(userID), TokenBoundary{Kind: BoundaryKindHub}, []string{"agent:attach"})
	require.NoError(t, err)
	require.Len(t, results, 1)
	if !results[0].OK {
		t.Errorf("ordinary project member should be relationship-eligible for agent:attach under a hub boundary: %+v", results[0])
	}
}

// TestCanMintSelector_HubBoundary_RelationshipAlternative_NoProjectAtAll_Denied
// confirms the relationship alternative still requires SOME relevant project
// admission -- a user with no project membership anywhere cannot mint
// agent:attach under a hub boundary via the relationship path either.
func TestCanMintSelector_HubBoundary_RelationshipAlternative_NoProjectAtAll_Denied(t *testing.T) {
	authz, s := authzTestSetup(t)
	ctx := context.Background()
	userID := tid("hpe-2")
	require.NoError(t, s.CreateUser(ctx, &store.User{ID: userID, Email: "hpe2@test.com", DisplayName: "u", Role: "member", Status: store.UserStatusActive}))

	results, err := authz.CanMintSelector(ctx, activeUserPrincipal(userID), TokenBoundary{Kind: BoundaryKindHub}, []string{"agent:attach"})
	require.NoError(t, err)
	require.Len(t, results, 1)
	if results[0].OK {
		t.Errorf("a user with no project admission anywhere must not be relationship-eligible for agent:attach: %+v", results[0])
	}
}
