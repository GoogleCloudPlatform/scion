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
	"sync"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/hub/permissions"
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

// TestResolveTargetScope_TemplateAndHarnessConfigCreate_EveryScope_ViaRealConstructors
// builds the Resource through the actual production constructors
// (templateScopeResource/templateUserScopeResource/harnessConfigScopeResource)
// rather than hand literals, covering every scope those constructors
// produce for template.create/harness_config.create: unlike skill, a single
// permission ID covers project, global and user scope for these two
// resource types (there is no separate *_create_global permission), so all
// three must resolve correctly through collection evidence.
func TestResolveTargetScope_TemplateAndHarnessConfigCreate_EveryScope_ViaRealConstructors(t *testing.T) {
	user := NewAuthenticatedUser(tid("ttc-user"), "ttc@test.com", "u", store.UserRoleMember, "api")

	got := ResolveTargetScope(templateScopeResource(store.TemplateScopeGlobal, ""), TargetScopeEvidence{
		IsCollectionLevel: true, CollectionScope: TargetScopeHub, PermissionID: "template.create",
	})
	if got.Kind != TargetScopeHub {
		t.Errorf("template.create via templateScopeResource(global): got %+v, want Hub", got)
	}

	got = ResolveTargetScope(templateUserScopeResource(user), TargetScopeEvidence{
		IsCollectionLevel: true, CollectionScope: TargetScopeHub, PermissionID: "template.create",
	})
	if got.Kind != TargetScopeHub {
		t.Errorf("template.create via templateUserScopeResource: got %+v, want Hub", got)
	}

	got = ResolveTargetScope(templateScopeResource(store.TemplateScopeProject, "p1"), TargetScopeEvidence{
		IsCollectionLevel: true, CollectionScope: TargetScopeProject, CollectionProjectID: "p1", PermissionID: "template.create",
	})
	if got.Kind != TargetScopeProject || got.ProjectID != "p1" {
		t.Errorf("template.create via templateScopeResource(project): got %+v, want Project/p1", got)
	}

	got = ResolveTargetScope(harnessConfigScopeResource(store.HarnessConfigScopeGlobal, ""), TargetScopeEvidence{
		IsCollectionLevel: true, CollectionScope: TargetScopeHub, PermissionID: "harness_config.create",
	})
	if got.Kind != TargetScopeHub {
		t.Errorf("harness_config.create via harnessConfigScopeResource(global): got %+v, want Hub", got)
	}

	got = ResolveTargetScope(harnessConfigScopeResource(store.HarnessConfigScopeProject, "p1"), TargetScopeEvidence{
		IsCollectionLevel: true, CollectionScope: TargetScopeProject, CollectionProjectID: "p1", PermissionID: "harness_config.create",
	})
	if got.Kind != TargetScopeProject || got.ProjectID != "p1" {
		t.Errorf("harness_config.create via harnessConfigScopeResource(project): got %+v, want Project/p1", got)
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

// TestResolveTargetScope_SkillListSupportsBothCollectionClasses is the
// paired global/project regression: skill.list is reviewed
// ProjectTargetApplicability=true AND separately, legitimately, resolves
// Hub-scope collection evidence for the global catalog -- a single boolean
// cannot represent both, and this must not regress to Unknown for the Hub
// case.
func TestResolveTargetScope_SkillListSupportsBothCollectionClasses(t *testing.T) {
	hub := ResolveTargetScope(Resource{Type: "skill"}, TargetScopeEvidence{
		IsCollectionLevel: true,
		CollectionScope:   TargetScopeHub,
		PermissionID:      "skill.list",
	})
	if hub.Kind != TargetScopeHub {
		t.Errorf("skill.list Hub collection evidence: got %+v, want Hub", hub)
	}

	proj := ResolveTargetScope(Resource{Type: "skill"}, TargetScopeEvidence{
		IsCollectionLevel:   true,
		CollectionScope:     TargetScopeProject,
		CollectionProjectID: "p1",
		PermissionID:        "skill.list",
	})
	if proj.Kind != TargetScopeProject || proj.ProjectID != "p1" {
		t.Errorf("skill.list Project collection evidence: got %+v, want Project/p1", proj)
	}
}

// TestResolveTargetScope_CollectionEvidenceRejectsMismatchedResourceParent
// is the exact contradiction case: a resource that already independently
// names a project parent (project A) cannot be paired with collection
// evidence naming a DIFFERENT project (B) -- the evidence does not win over
// a contradictory resource fact.
func TestResolveTargetScope_CollectionEvidenceRejectsMismatchedResourceParent(t *testing.T) {
	got := ResolveTargetScope(Resource{Type: "agent", ParentType: "project", ParentID: "project-A"}, TargetScopeEvidence{
		IsCollectionLevel:   true,
		CollectionScope:     TargetScopeProject,
		CollectionProjectID: "project-B",
		PermissionID:        "agent.create",
	})
	if got.Kind != TargetScopeUnknown {
		t.Errorf("mismatched resource parent (A) vs evidence project (B) must resolve Unknown: got %+v", got)
	}
}

// TestResolveTargetScope_CollectionEvidenceRejectsResourceHubMismatch mirrors
// the above for the Hub-evidence direction: a resource that already
// independently names a project parent cannot be paired with Hub-scope
// collection evidence.
func TestResolveTargetScope_CollectionEvidenceRejectsResourceHubMismatch(t *testing.T) {
	got := ResolveTargetScope(Resource{Type: "skill", ParentType: "project", ParentID: "project-A"}, TargetScopeEvidence{
		IsCollectionLevel: true,
		CollectionScope:   TargetScopeHub,
		PermissionID:      "skill.list",
	})
	if got.Kind != TargetScopeUnknown {
		t.Errorf("resource with a project parent paired with Hub collection evidence must resolve Unknown: got %+v", got)
	}
}

// TestResolveTargetScope_ArbitraryTypeWithUserScopeKindIsUnknown is the
// "restrict scope-kind classification to reviewed resource types"
// regression: an arbitrary/unexpected resource type that happens to carry
// ScopeKind="user" (e.g. from an unrelated resource-building bug) must NOT
// be classified Hub on that basis -- only skill/template/harness_config
// carry that ScopeKind concept.
func TestResolveTargetScope_ArbitraryTypeWithUserScopeKindIsUnknown(t *testing.T) {
	got := ResolveTargetScope(Resource{Type: "widget", ScopeKind: store.SkillScopeUser}, TargetScopeEvidence{})
	if got.Kind != TargetScopeUnknown {
		t.Errorf("arbitrary type with ScopeKind=user must resolve Unknown, not Hub: got %+v", got)
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
	// ProjectMembershipEvidence takes no Resource argument at all, so it
	// cannot consult Ancestry or any other historical fact: a project with
	// no active binding for this principal is denied outright. The
	// binding-removal and retained-ancestry cases are covered end to end
	// through ProjectTargetAdmission by the TestProjectTargetAdmission_*
	// RetainedAncestryStillDenies tests.
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

// --- SystemAuthorityProof / MintTimeSystemGrant (seeded-role regressions) ---

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

// TestSeededHubMember_CannotReadOrAttachProjectAgents is the seeded-role
// regression: a user whose only role is the seeded hub-member system role
// cannot attach to or read project agents through system authority — the
// hub-member catalog grant does not extend to agent.read/agent.attach.
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

// TestProjectTargetAdmission_DirectBindingRemoved_RetainedAncestryStillDenies
// is the AC3 regression: a direct project role binding admits access; once
// that binding is deleted, the user is denied even though the target's
// Ancestry chain still names them (ancestry alone is never evidence).
func TestProjectTargetAdmission_DirectBindingRemoved_RetainedAncestryStillDenies(t *testing.T) {
	authz, s := authzTestSetup(t)
	ctx := context.Background()

	projectID := tid("rm-proj-1")
	userID := tid("rm-user-1")
	createDelegateTestProject(t, s, projectID, "rm-proj-1", "test")
	createTestUserWithProjectRole(t, s, userID, "rm1@test.com", projectID, store.ProjectRoleMember)

	target := Resource{Type: "agent", ID: tid("rm-agent-1"), ParentType: "project", ParentID: projectID, Ancestry: []string{userID}}

	result, err := authz.ProjectTargetAdmission(ctx, activeUserPrincipal(userID), projectID, "agent.read", target, nil)
	require.NoError(t, err)
	require.True(t, result.Admitted, "direct project membership must admit agent.read before removal")

	n, err := s.DeleteRoleBindingsForPrincipal(ctx, store.RoleBindingPrincipalUser, userID)
	require.NoError(t, err)
	require.Equal(t, 1, n, "expected exactly the one direct project role binding to be removed")

	result, err = authz.ProjectTargetAdmission(ctx, activeUserPrincipal(userID), projectID, "agent.read", target, nil)
	require.NoError(t, err)
	if result.Admitted {
		t.Error("removing the direct role binding must deny agent.read even though Resource.Ancestry still names the user")
	}
}

// TestProjectTargetAdmission_GroupMembershipRemoved_RetainedAncestryStillDenies
// is the group-binding half of AC3: group membership admits access; once
// the user is removed from the group, they are denied even though the
// target's Ancestry chain still names them.
func TestProjectTargetAdmission_GroupMembershipRemoved_RetainedAncestryStillDenies(t *testing.T) {
	authz, s := authzTestSetup(t)
	ctx := context.Background()

	projectID := tid("rm-proj-2")
	userID := tid("rm-user-2")
	groupID := tid("rm-group-2")
	createDelegateTestProject(t, s, projectID, "rm-proj-2", "test")
	require.NoError(t, s.CreateUser(ctx, &store.User{ID: userID, Email: "rm2@test.com", DisplayName: "u", Role: "member", Status: store.UserStatusActive}))
	require.NoError(t, s.CreateGroup(ctx, &store.Group{ID: groupID, Slug: "rm-group-2", Name: "G"}))
	require.NoError(t, s.AddGroupMember(ctx, &store.GroupMember{GroupID: groupID, MemberType: store.GroupMemberTypeUser, MemberID: userID, Role: "member"}))

	rd, err := s.GetRoleDefinitionByName(ctx, store.ProjectRoleMember, store.RoleScopeProject)
	require.NoError(t, err)
	_, err = s.CreateRoleBinding(ctx, &store.RoleBinding{
		RoleDefinitionID: rd.ID, PrincipalType: store.RoleBindingPrincipalGroup, PrincipalID: groupID,
		ScopeType: store.RoleScopeProject, ScopeID: projectID, CreatedBy: "test",
	})
	require.NoError(t, err)

	target := Resource{Type: "agent", ID: tid("rm-agent-2"), ParentType: "project", ParentID: projectID, Ancestry: []string{userID}}

	result, err := authz.ProjectTargetAdmission(ctx, activeUserPrincipal(userID), projectID, "agent.read", target, nil)
	require.NoError(t, err)
	require.True(t, result.Admitted, "group membership must admit agent.read before removal")

	require.NoError(t, s.RemoveGroupMember(ctx, groupID, store.GroupMemberTypeUser, userID))

	result, err = authz.ProjectTargetAdmission(ctx, activeUserPrincipal(userID), projectID, "agent.read", target, nil)
	require.NoError(t, err)
	if result.Admitted {
		t.Error("removing group membership must deny agent.read even though Resource.Ancestry still names the user")
	}
}

// TestProjectTargetAdmission_FormerMemberWithOnlyHubMemberRole_RetainedAncestryStillDenies
// closes AC3's third case: a user who lost their direct project membership,
// and whose only remaining role is the seeded hub-member system role, is
// still denied agent.read/agent.attach on a target whose Ancestry chain
// still names them.
func TestProjectTargetAdmission_FormerMemberWithOnlyHubMemberRole_RetainedAncestryStillDenies(t *testing.T) {
	authz, s := authzTestSetup(t)
	ctx := context.Background()

	projectID := tid("rm-proj-3")
	userID := tid("rm-user-3")
	createDelegateTestProject(t, s, projectID, "rm-proj-3", "test")
	createTestUserWithProjectRole(t, s, userID, "rm3@test.com", projectID, store.ProjectRoleMember)

	target := Resource{Type: "agent", ID: tid("rm-agent-3"), ParentType: "project", ParentID: projectID, Ancestry: []string{userID}}

	result, err := authz.ProjectTargetAdmission(ctx, activeUserPrincipal(userID), projectID, "agent.read", target, nil)
	require.NoError(t, err)
	require.True(t, result.Admitted, "direct project membership must admit agent.read before removal")

	n, err := s.DeleteRoleBindingsForPrincipal(ctx, store.RoleBindingPrincipalUser, userID)
	require.NoError(t, err)
	require.Equal(t, 1, n, "expected exactly the one direct project role binding to be removed")

	rd, err := s.GetRoleDefinitionByName(ctx, store.SystemRoleHubMember, store.RoleScopeSystem)
	require.NoError(t, err)
	_, err = s.CreateRoleBinding(ctx, &store.RoleBinding{
		RoleDefinitionID: rd.ID, PrincipalType: store.RoleBindingPrincipalUser, PrincipalID: userID,
		ScopeType: store.RoleScopeSystem, CreatedBy: "test",
	})
	require.NoError(t, err)

	for _, permID := range []string{"agent.read", "agent.attach"} {
		result, err := authz.ProjectTargetAdmission(ctx, activeUserPrincipal(userID), projectID, permID, target, nil)
		require.NoError(t, err)
		if result.Admitted {
			t.Errorf("a former member holding only the seeded hub-member role must be denied %s, even though Resource.Ancestry still names them", permID)
		}
	}
}

// TestSeededHubAdmin_ScheduledEventDoesNotUnlockAgentAction is the
// seeded-role regression: hub-admin's scheduled_event permission does not
// unlock an unrelated agent action.
func TestSeededHubAdmin_ScheduledEventDoesNotUnlockAgentAction(t *testing.T) {
	authz, s := authzTestSetup(t)
	ctx := context.Background()

	projectID := tid("ha-proj")
	userID := tid("ha-user")
	createDelegateTestProject(t, s, projectID, "ha-proj", "test")
	createTestUserWithRole(t, s, userID, "ha@test.com", "admin", store.SystemRoleHubAdmin)

	// Confirm hub-admin DOES have scheduled_event authority (positive
	// control — without this, the negative assertion below would also pass
	// if SystemAuthorityProof denied everything), then confirm it does not
	// extend to agent.delete.
	schedOK, err := authz.SystemAuthorityProof(ctx, activeUserPrincipal(userID), projectID, "scheduled_event.read", ContemplatedProjectClass("scheduled_event.read"))
	require.NoError(t, err)
	require.True(t, schedOK, "sanity: hub-admin must hold scheduled_event.read for this test to prove exact-permission semantics")

	agentOK, err := authz.SystemAuthorityProof(ctx, activeUserPrincipal(userID), projectID, "agent.delete", ContemplatedProjectClass("agent.delete"))
	require.NoError(t, err)
	if agentOK {
		t.Error("hub-admin's scheduled_event authority must not unlock agent.delete")
	}
}

// TestSeededHubMember_CatalogOnlyGrant_PairedTest is the paired
// regression: a seeded hub-member's catalog-only
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

// TestProjectTargetAdmission_MemoReusesResult proves the memo actually
// caches, rather than merely agreeing because nothing changed between calls:
// the underlying role binding is removed between the two calls, so only a
// real cache hit can explain the second call still returning Admitted.
func TestProjectTargetAdmission_MemoReusesResult(t *testing.T) {
	authz, s := authzTestSetup(t)
	ctx := context.Background()

	projectID := tid("pta-proj-3")
	userID := tid("pta-user-3")
	createDelegateTestProject(t, s, projectID, "pta-proj-3", "test")
	createTestUserWithProjectRole(t, s, userID, "pta3@test.com", projectID, store.ProjectRoleMember)

	target := Resource{Type: "agent", ID: tid("pta3-agent"), ParentType: "project", ParentID: projectID}

	memo := NewProjectAdmissionCache()
	r1, err := authz.ProjectTargetAdmission(ctx, activeUserPrincipal(userID), projectID, "agent.read", target, memo)
	require.NoError(t, err)
	require.True(t, r1.Admitted, "sanity: direct project membership must admit before removal")

	n, err := s.DeleteRoleBindingsForPrincipal(ctx, store.RoleBindingPrincipalUser, userID)
	require.NoError(t, err)
	require.Equal(t, 1, n)

	// Same memo: the cached result from before removal is returned.
	r2, err := authz.ProjectTargetAdmission(ctx, activeUserPrincipal(userID), projectID, "agent.read", target, memo)
	require.NoError(t, err)
	if !r2.Admitted {
		t.Error("a memoized result must be returned even though the underlying binding was removed after the first call")
	}

	// Nil memo: no caching, so the removal is reflected immediately.
	r3, err := authz.ProjectTargetAdmission(ctx, activeUserPrincipal(userID), projectID, "agent.read", target, nil)
	require.NoError(t, err)
	if r3.Admitted {
		t.Error("without a memo, the removed binding must be reflected immediately (denied)")
	}
}

// TestProjectAdmissionCache_KeyIncludesClass is a white-box test of the
// cache's own keying: two classes that differ only in ScopeKind (for
// example a real project-scoped skill vs the same permission contemplated
// for the global catalog) must not collide in the cache.
func TestProjectAdmissionCache_KeyIncludesClass(t *testing.T) {
	memo := NewProjectAdmissionCache()
	base := projectAdmissionCacheKey{
		principalKind: PrincipalKindUser, principalID: "u1", projectID: "p1", permissionID: "skill.read",
		class: ProjectTargetClass{ResourceType: permissions.ResourceSkill, ScopeKind: store.SkillScopeProject},
	}
	other := base
	other.class = ProjectTargetClass{ResourceType: permissions.ResourceSkill, ScopeKind: store.SkillScopeGlobal}

	memo.put(base, ProjectAdmissionResult{Admitted: true, Source: ProjectAccessSourceMembership})

	if _, ok := memo.get(other); ok {
		t.Error("a cache entry seeded for one class must not be visible under a different class")
	}
	if cached, ok := memo.get(base); !ok || !cached.Admitted {
		t.Errorf("the exact key must still hit: got %+v, ok=%v", cached, ok)
	}
}

// TestProjectAdmissionCache_ZeroValueUsable proves a zero-value
// ProjectAdmissionCache{} (as opposed to one built via
// NewProjectAdmissionCache) is ready to use: put followed by get on the same
// key returns the stored value, and get on a key that was never put misses
// cleanly rather than panicking.
func TestProjectAdmissionCache_ZeroValueUsable(t *testing.T) {
	var memo ProjectAdmissionCache
	key := projectAdmissionCacheKey{
		principalKind: PrincipalKindUser, principalID: "u1", projectID: "p1", permissionID: "agent.read",
	}

	if _, ok := memo.get(key); ok {
		t.Error("get on a zero-value cache before any put must miss, not hit")
	}

	memo.put(key, ProjectAdmissionResult{Admitted: true, Source: ProjectAccessSourceMembership})

	got, ok := memo.get(key)
	if !ok || !got.Admitted {
		t.Errorf("get after put on a zero-value cache = %+v, ok=%v, want the stored value", got, ok)
	}
}

// TestProjectAdmissionCache_LiteralWithoutMapUsable is
// TestProjectAdmissionCache_ZeroValueUsable's variant for a cache built as an
// explicit literal that omits the map field — the same shape a caller gets
// from &ProjectAdmissionCache{} rather than var declaration or
// NewProjectAdmissionCache.
func TestProjectAdmissionCache_LiteralWithoutMapUsable(t *testing.T) {
	memo := &ProjectAdmissionCache{}
	key := projectAdmissionCacheKey{
		principalKind: PrincipalKindUser, principalID: "u2", projectID: "p2", permissionID: "agent.create",
	}

	memo.put(key, ProjectAdmissionResult{Admitted: false})

	got, ok := memo.get(key)
	if !ok || got.Admitted {
		t.Errorf("get after put on a literal cache without a map = %+v, ok=%v, want the stored (non-admitted) value", got, ok)
	}
}

// TestProjectAdmissionCache_ZeroValueConcurrentSafe proves the lazy map
// initialization in put is safe under concurrent first use: many goroutines
// racing to put into the same zero-value cache must not lose any write, and
// must not race on the map itself.
func TestProjectAdmissionCache_ZeroValueConcurrentSafe(t *testing.T) {
	var memo ProjectAdmissionCache
	const n = 50
	var wg sync.WaitGroup
	wg.Add(n)
	for i := 0; i < n; i++ {
		go func() {
			defer wg.Done()
			key := projectAdmissionCacheKey{
				principalKind: PrincipalKindUser,
				principalID:   "u",
				projectID:     "p",
				permissionID:  string(rune('a' + i%26)),
			}
			memo.put(key, ProjectAdmissionResult{Admitted: true})
			memo.get(key)
		}()
	}
	wg.Wait()

	for i := 0; i < 26; i++ {
		key := projectAdmissionCacheKey{
			principalKind: PrincipalKindUser,
			principalID:   "u",
			projectID:     "p",
			permissionID:  string(rune('a' + i)),
		}
		got, ok := memo.get(key)
		if !ok || !got.Admitted {
			t.Errorf("get(%q) after concurrent puts = %+v, ok=%v, want the stored value", key.permissionID, got, ok)
		}
	}
}

func TestProjectAdmissionForClass_MembershipPath(t *testing.T) {
	authz, s := authzTestSetup(t)
	ctx := context.Background()

	projectID := tid("pafc-proj-1")
	userID := tid("pafc-user-1")
	createDelegateTestProject(t, s, projectID, "pafc-proj-1", "test")
	createTestUserWithProjectRole(t, s, userID, "pafc1@test.com", projectID, store.ProjectRoleMember)

	class := ProjectTargetClass{ResourceType: "agent"}
	result, err := authz.ProjectAdmissionForClass(ctx, activeUserPrincipal(userID), projectID, "agent.read", class, nil)
	require.NoError(t, err)
	if !result.Admitted || result.Source != ProjectAccessSourceMembership {
		t.Errorf("got %+v, want Admitted via membership", result)
	}
}

// TestProjectAdmissionForClass_MaterialScopeDiffersFromExecutionProject is
// F.2's motivating case: the material's own class can differ from the
// execution project's resource type/scope (e.g. checking a user-scoped
// material's project-scoped delivery class explicitly, rather than deriving
// it from a Resource that describes the executing agent).
func TestProjectAdmissionForClass_MaterialScopeDiffersFromExecutionProject(t *testing.T) {
	authz, s := authzTestSetup(t)
	ctx := context.Background()

	projectID := tid("pafc-proj-2")
	userID := tid("pafc-user-2")
	createDelegateTestProject(t, s, projectID, "pafc-proj-2", "test")
	systemRoleUserWithPermissions(t, s, userID, []string{"skill.read"})

	// A class naming "skill" explicitly, independent of any agent Resource.
	class := ProjectTargetClass{ResourceType: "skill", ScopeKind: store.SkillScopeProject}
	result, err := authz.ProjectAdmissionForClass(ctx, activeUserPrincipal(userID), projectID, "skill.read", class, nil)
	require.NoError(t, err)
	if !result.Admitted || result.Source != ProjectAccessSourceSystemRole {
		t.Errorf("got %+v, want Admitted via system_role for the explicit skill class", result)
	}
}

// onceFailingBindingsStore wraps a store.Store and fails the FIRST call to
// ListRoleBindingsForPrincipals with failErr, then delegates to the real
// store for every subsequent call.
type onceFailingBindingsStore struct {
	store.Store
	failed  bool
	failErr error
}

func (s *onceFailingBindingsStore) ListRoleBindingsForPrincipals(ctx context.Context, principals []store.PrincipalRef, scopeTypes []string, scopeIDs []string) ([]*store.RoleBinding, error) {
	if !s.failed {
		s.failed = true
		return nil, s.failErr
	}
	return s.Store.ListRoleBindingsForPrincipals(ctx, principals, scopeTypes, scopeIDs)
}

// TestProjectAdmissionForClass_ErrorsNeverMemoized proves an error result is
// never cached: the underlying store fails exactly once, so the first call
// must error and the second call — using the SAME memo — must recompute
// (via the now-succeeding store) rather than replay the first call's error
// or a cached denial.
func TestProjectAdmissionForClass_ErrorsNeverMemoized(t *testing.T) {
	_, s := authzTestSetup(t)
	ctx := context.Background()

	projectID := tid("pafc-err-proj")
	userID := tid("pafc-err-user")
	createDelegateTestProject(t, s, projectID, "pafc-err-proj", "test")
	createTestUserWithProjectRole(t, s, userID, "pafcerr@test.com", projectID, store.ProjectRoleMember)

	failing := &onceFailingBindingsStore{Store: s, failErr: errors.New("injected transient binding-list failure")}
	authz := NewAuthzService(failing, slog.Default())

	class := ProjectTargetClass{ResourceType: "agent"}
	memo := NewProjectAdmissionCache()

	_, err1 := authz.ProjectAdmissionForClass(ctx, activeUserPrincipal(userID), projectID, "agent.read", class, memo)
	if err1 == nil {
		t.Fatal("expected the first call to fail via the injected store error")
	}

	result2, err2 := authz.ProjectAdmissionForClass(ctx, activeUserPrincipal(userID), projectID, "agent.read", class, memo)
	require.NoError(t, err2)
	if !result2.Admitted {
		t.Error("the second call must recompute and succeed, not return a cached denial from the first call's error")
	}
}

// onceFailingConstraintStore wraps a store.Store and fails the FIRST call to
// ListAccessConstraints with failErr, then delegates to the real store for
// every subsequent call.
type onceFailingConstraintStore struct {
	store.Store
	failed  bool
	failErr error
}

func (s *onceFailingConstraintStore) ListAccessConstraints(ctx context.Context, limit, offset int) ([]*store.AccessConstraint, error) {
	if !s.failed {
		s.failed = true
		return nil, s.failErr
	}
	return s.Store.ListAccessConstraints(ctx, limit, offset)
}

// TestProjectAdmissionForClass_ConstraintLoadErrorNotMemoized proves that a
// transient access-constraint-table load error on the SYSTEM-AUTHORITY path
// (SystemAuthorityProof, which loads the constraint table for its
// project-scoped reduction) denies without being memoized: the first call
// must error, and a second call on the SAME memo must recompute via the
// now-succeeding store and admit, rather than replay a cached deny-all
// restriction from the first call's load failure.
func TestProjectAdmissionForClass_ConstraintLoadErrorNotMemoized(t *testing.T) {
	_, s := authzTestSetup(t)
	ctx := context.Background()

	projectID := tid("pafc-cle-proj")
	userID := tid("pafc-cle-user")
	createDelegateTestProject(t, s, projectID, "pafc-cle-proj", "test")
	// A non-member with a target-applicable SYSTEM grant only, so
	// ProjectMembershipEvidence denies and SystemAuthorityProof — the path
	// that loads the access-constraint table — is the one exercised.
	systemRoleUserWithPermissions(t, s, userID, []string{"skill.read"})

	failing := &onceFailingConstraintStore{Store: s, failErr: errors.New("injected transient constraint-list failure")}
	authz := NewAuthzService(failing, slog.Default())

	class := ProjectTargetClass{ResourceType: "skill", ScopeKind: store.SkillScopeProject}
	memo := NewProjectAdmissionCache()

	_, err1 := authz.ProjectAdmissionForClass(ctx, activeUserPrincipal(userID), projectID, "skill.read", class, memo)
	if err1 == nil {
		t.Fatal("expected the first call to fail via the injected constraint-table load error")
	}
	require.ErrorIs(t, err1, ErrProjectAccessDenied)

	result2, err2 := authz.ProjectAdmissionForClass(ctx, activeUserPrincipal(userID), projectID, "skill.read", class, memo)
	require.NoError(t, err2)
	if !result2.Admitted {
		t.Error("the second call must recompute and succeed, not return a cached denial from the first call's constraint-load error")
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

// TestCanMintSelector_ProjectBoundary_SuperAdminWithoutMembership_AdmittedButFlatIneligible
// pins the rule: flat project mint eligibility remains
// project-binding-only, even for a super-admin. System authority
// establishes ADMISSION (the selector's per-permission SystemAuthorityProof
// check passes -- confirmed indirectly by agent:attach succeeding below,
// and directly here by the flat agent:delete selector reaching the
// eligibility stage at all rather than being denied with
// MintDenialProjectAccessRequired), but a FLAT selector like agent:delete
// still requires the project's OWN role to carry that permission -- a
// super-admin's system-scoped role is not a project-scoped role, so
// hasProjectRoleFlatPermission finds nothing and denies with
// MintDenialFlatRoleInsufficient, distinct from an admission denial.
func TestCanMintSelector_ProjectBoundary_SuperAdminWithoutMembership_AdmittedButFlatIneligible(t *testing.T) {
	authz, s := authzTestSetup(t)
	ctx := context.Background()
	adminID := tid("cms-5")
	projectID := tid("cms-5-proj")
	createDelegateTestProject(t, s, projectID, "cms-5-proj", "test")
	createTestUserWithRole(t, s, adminID, "cms5@test.com", "admin", store.SystemRoleSuperAdmin)

	results, err := authz.CanMintSelector(ctx, activeUserPrincipal(adminID), TokenBoundary{Kind: BoundaryKindProject, ProjectID: projectID}, []string{"agent:delete"})
	require.NoError(t, err)
	require.Len(t, results, 1)
	if results[0].OK {
		t.Errorf("flat agent:delete must stay project-binding-only, even for a super-admin with no project membership: %+v", results[0])
	}
	if results[0].Reason != MintDenialFlatRoleInsufficient {
		t.Errorf("must be denied as flat-role-insufficient (admission passed, eligibility did not), got reason=%q", results[0].Reason)
	}
}

// TestCanMintSelector_ProjectBoundary_SuperAdminWithoutMembership_RelationshipEligible
// confirms the OTHER half of the flat/relationship split: a reviewed
// RELATIONSHIP-eligible selector
// (agent:attach) IS mintable for a super-admin with no project membership,
// via system-authority admission plus RelationshipPolicyMintEligible --
// which does not require a matching project role.
func TestCanMintSelector_ProjectBoundary_SuperAdminWithoutMembership_RelationshipEligible(t *testing.T) {
	authz, s := authzTestSetup(t)
	ctx := context.Background()
	adminID := tid("cms-5b")
	projectID := tid("cms-5b-proj")
	createDelegateTestProject(t, s, projectID, "cms-5b-proj", "test")
	createTestUserWithRole(t, s, adminID, "cms5b@test.com", "admin", store.SystemRoleSuperAdmin)

	results, err := authz.CanMintSelector(ctx, activeUserPrincipal(adminID), TokenBoundary{Kind: BoundaryKindProject, ProjectID: projectID}, []string{"agent:attach"})
	require.NoError(t, err)
	require.Len(t, results, 1)
	if !results[0].OK {
		t.Errorf("reviewed relationship-eligible agent:attach should be mintable for a super-admin without membership: %+v", results[0])
	}
}

func TestCanMintSelector_HubBoundary_CatalogOnlyGrantEligible(t *testing.T) {
	authz, s := authzTestSetup(t)
	ctx := context.Background()
	userID := tid("cms-6")
	createTestUserWithRole(t, s, userID, "cms6@test.com", "member", store.SystemRoleHubMember)

	// hub-member's catalog-only skill.read must remain hub-boundary
	// mint-eligible.
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

// TestMintTimeSystemGrant_UnknownClassValue_Denied is the regression: an
// actual UNKNOWN TargetClassKind enum value (not merely an
// absent SupportedTargetClasses entry) must deny rather than silently fall
// through to an under-specified class and potentially grant on it.
func TestMintTimeSystemGrant_UnknownClassValue_Denied(t *testing.T) {
	authz, s := authzTestSetup(t)
	ctx := context.Background()
	adminID := tid("mtsg-3")
	createTestUserWithRole(t, s, adminID, "mtsg3@test.com", "admin", store.SystemRoleSuperAdmin)

	const testPermID = "test.unknown_target_class_permission"
	permissions.SupportedTargetClasses[testPermID] = []permissions.TargetClassKind{"totally_bogus_class_value"}
	defer delete(permissions.SupportedTargetClasses, testPermID)

	ok, err := authz.MintTimeSystemGrant(ctx, activeUserPrincipal(adminID), testPermID)
	require.NoError(t, err)
	if ok {
		t.Error("an unrecognized TargetClassKind value must deny, not fall through to an under-specified class")
	}
}

// TestHasAnyProjectBinding_SoleProjectConstrained_Denied is the
// regression: a project-scoped access constraint governing the ONLY
// project a permission is granted in must deny -- constraint reduction must
// be evaluated per-project, not merged/diluted across projects or checked
// against a system-wide ResourceContext{}.
func TestHasAnyProjectBinding_SoleProjectConstrained_Denied(t *testing.T) {
	authz, s := authzTestSetup(t)
	ctx := context.Background()
	userID := tid("hapb-1")
	projectID := tid("hapb-1-proj")
	createDelegateTestProject(t, s, projectID, "hapb-1-proj", "test")
	require.NoError(t, s.CreateUser(ctx, &store.User{ID: userID, Email: "hapb1@test.com", DisplayName: "u", Role: "member", Status: store.UserStatusActive}))

	rd := createTestRoleDefinition(t, s, "hapb-1-role", store.RoleScopeProject, []string{"agent.read"})
	_, err := s.CreateRoleBinding(ctx, &store.RoleBinding{
		RoleDefinitionID: rd.ID, PrincipalType: store.RoleBindingPrincipalUser, PrincipalID: userID,
		ScopeType: store.RoleScopeProject, ScopeID: projectID, CreatedBy: "test",
	})
	require.NoError(t, err)
	_, err = s.CreateAccessConstraint(ctx, &store.AccessConstraint{
		Name: "hapb-1-constraint", SubjectKind: store.ConstraintSubjectAllPrincipals,
		ScopeType: store.RoleScopeProject, ScopeID: projectID,
		MaximumPermissions: []string{"agent.list"}, // excludes agent.read
		Purpose:            "test: sole project constrained",
	})
	require.NoError(t, err)

	ok, err := authz.hasAnyProjectBinding(ctx, activeUserPrincipal(userID), "agent.read")
	require.NoError(t, err)
	if ok {
		t.Error("the sole project's access constraint excludes agent.read; hasAnyProjectBinding must deny")
	}
}

// TestHasAnyProjectBinding_ConstraintLoadErrorReturnsError proves a
// transient access-constraint-table load failure surfaces as an error from
// hasAnyProjectBinding, wrapping ErrProjectAccessDenied, rather than a
// deny-all-derived false. This path is not reachable through CanMintSelector
// under a persistently failing store: MintTimeSystemGrant's own unconditional
// constraint load runs first and errors, and the per-call cache then replays
// that same error for every later helper in the batch, so this test calls
// hasAnyProjectBinding directly.
func TestHasAnyProjectBinding_ConstraintLoadErrorReturnsError(t *testing.T) {
	_, s := authzTestSetup(t)
	ctx := context.Background()
	userID := tid("hapb-cle")
	projectID := tid("hapb-cle-proj")
	createDelegateTestProject(t, s, projectID, "hapb-cle-proj", "test")
	require.NoError(t, s.CreateUser(ctx, &store.User{ID: userID, Email: "hapbcle@test.com", DisplayName: "u", Role: "member", Status: store.UserStatusActive}))

	rd := createTestRoleDefinition(t, s, "hapb-cle-role", store.RoleScopeProject, []string{"agent.read"})
	_, err := s.CreateRoleBinding(ctx, &store.RoleBinding{
		RoleDefinitionID: rd.ID, PrincipalType: store.RoleBindingPrincipalUser, PrincipalID: userID,
		ScopeType: store.RoleScopeProject, ScopeID: projectID, CreatedBy: "test",
	})
	require.NoError(t, err)

	failing := &r2FailingStore{Store: s, failListConstraints: errors.New("injected: constraint load failure")}
	authz := NewAuthzService(failing, slog.Default())

	ok, err := authz.hasAnyProjectBinding(ctx, activeUserPrincipal(userID), "agent.read")
	require.Error(t, err)
	require.ErrorIs(t, err, ErrProjectAccessDenied)
	if ok {
		t.Error("a constraint-table load failure must deny, not admit")
	}
}

// TestHasRelevantProjectAdmission_ConstraintLoadErrorReturnsError is
// hasAnyProjectBinding's counterpart for the Hub-boundary relationship
// alternative: hasRelevantProjectAdmission calls
// permissionSurvivesProjectConstraints per project, so this pins that a
// transient constraint-table load failure surfaces as an error there too,
// called directly for the same reason as the test above.
func TestHasRelevantProjectAdmission_ConstraintLoadErrorReturnsError(t *testing.T) {
	_, s := authzTestSetup(t)
	ctx := context.Background()
	userID := tid("hrpa-cle")
	projectID := tid("hrpa-cle-proj")
	createDelegateTestProject(t, s, projectID, "hrpa-cle-proj", "test")
	createTestUserWithProjectRole(t, s, userID, "hrpacle@test.com", projectID, store.ProjectRoleMember)

	failing := &r2FailingStore{Store: s, failListConstraints: errors.New("injected: constraint load failure")}
	authz := NewAuthzService(failing, slog.Default())

	ok, err := authz.hasRelevantProjectAdmission(ctx, activeUserPrincipal(userID), "agent.attach")
	require.Error(t, err)
	require.ErrorIs(t, err, ErrProjectAccessDenied)
	if ok {
		t.Error("a constraint-table load failure must deny, not admit")
	}
}

// TestHasAnyProjectBinding_SecondUnconstrainedProject_Allowed confirms the
// other half of the rule: a second, unconstrained project's grant still succeeds
// even though a first project's grant is constrained away -- each project
// is evaluated independently.
func TestHasAnyProjectBinding_SecondUnconstrainedProject_Allowed(t *testing.T) {
	authz, s := authzTestSetup(t)
	ctx := context.Background()
	userID := tid("hapb-2")
	constrainedProject := tid("hapb-2-proj-a")
	openProject := tid("hapb-2-proj-b")
	createDelegateTestProject(t, s, constrainedProject, "hapb-2-proj-a", "test")
	createDelegateTestProject(t, s, openProject, "hapb-2-proj-b", "test")
	require.NoError(t, s.CreateUser(ctx, &store.User{ID: userID, Email: "hapb2@test.com", DisplayName: "u", Role: "member", Status: store.UserStatusActive}))

	rd := createTestRoleDefinition(t, s, "hapb-2-role", store.RoleScopeProject, []string{"agent.read"})
	for _, pid := range []string{constrainedProject, openProject} {
		_, err := s.CreateRoleBinding(ctx, &store.RoleBinding{
			RoleDefinitionID: rd.ID, PrincipalType: store.RoleBindingPrincipalUser, PrincipalID: userID,
			ScopeType: store.RoleScopeProject, ScopeID: pid, CreatedBy: "test",
		})
		require.NoError(t, err)
	}
	_, err := s.CreateAccessConstraint(ctx, &store.AccessConstraint{
		Name: "hapb-2-constraint", SubjectKind: store.ConstraintSubjectAllPrincipals,
		ScopeType: store.RoleScopeProject, ScopeID: constrainedProject,
		MaximumPermissions: []string{"agent.list"}, // excludes agent.read, only for constrainedProject
		Purpose:            "test: second project unconstrained",
	})
	require.NoError(t, err)

	ok, err := authz.hasAnyProjectBinding(ctx, activeUserPrincipal(userID), "agent.read")
	require.NoError(t, err)
	if !ok {
		t.Error("the second, unconstrained project's grant should still succeed even though the first project is constrained")
	}
}

// TestCanMintSelector_HubBoundary_RelationshipAlternative_OrdinaryMember is
// the regression: an ordinary project member,
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

// TestCanMintSelector_HubBoundary_RelationshipAlternative_DevPrincipal pins
// that a dev/local-user principal reaches the relationship mint-eligibility
// path the same way an ordinary user principal does: RelationshipPolicies
// rows are authored against the canonical "user" kind, so the raw runtime
// kind ("dev") must be normalized through permissions.RelationshipPrincipalKind
// before matching, or a dev principal would silently fail to match a row
// scoped to "user" despite requireLocalUserPrincipal treating dev and user
// as equally valid local-user principals everywhere else.
func TestCanMintSelector_HubBoundary_RelationshipAlternative_DevPrincipal(t *testing.T) {
	authz, s := authzTestSetup(t)
	ctx := context.Background()
	userID := tid("hpe-dev-1")
	projectID := tid("hpe-dev-1-proj")
	createDelegateTestProject(t, s, projectID, "hpe-dev-1-proj", "test")
	createTestUserWithProjectRole(t, s, userID, "hpedev1@test.com", projectID, store.ProjectRoleMember)

	devPrincipal := PrincipalContext{Kind: PrincipalKindDev, ID: userID}
	results, err := authz.CanMintSelector(ctx, devPrincipal, TokenBoundary{Kind: BoundaryKindHub}, []string{"agent:attach"})
	require.NoError(t, err)
	require.Len(t, results, 1)
	if !results[0].OK {
		t.Errorf("a dev principal with ordinary project membership should be relationship-eligible for agent:attach under a hub boundary, the same as a user principal: %+v", results[0])
	}
}

// TestNormalizePrincipalType_AgreesWithRelationshipPrincipalKind pins that
// hub.NormalizePrincipalType (the flat mint path and Decide's constraint
// matching) and permissions.RelationshipPrincipalKind (the relationship
// mint path) never diverge, even though they are two independent
// implementations of the same mapping (permissions cannot import hub, so
// there is no single shared function to call instead) — every PrincipalKind
// constant, plus an unrecognized value, must map identically through both.
func TestNormalizePrincipalType_AgreesWithRelationshipPrincipalKind(t *testing.T) {
	kinds := []string{
		string(PrincipalKindUser),
		string(PrincipalKindAgent),
		string(PrincipalKindFederatedUser),
		string(PrincipalKindFederatedAgent),
		string(PrincipalKindFederatedService),
		string(PrincipalKindBroker),
		string(PrincipalKindDev),
		"totally-unrecognized-kind",
	}
	for _, k := range kinds {
		got := NormalizePrincipalType(k)
		want := permissions.RelationshipPrincipalKind(k)
		if got != want {
			t.Errorf("NormalizePrincipalType(%q) = %q but permissions.RelationshipPrincipalKind(%q) = %q -- the flat and relationship mint paths would diverge on this principal kind", k, got, k, want)
		}
	}
}

// TestMintEligibilityCache_PrincipalMismatchSkipsCache proves the
// principal-key guard: a cache populated for one principal must not hand a
// second, different principal the first principal's closure. CanMintSelector
// never actually reuses one cache across two principals (it installs a
// fresh cache per call), but a future caller that did must be safe.
func TestMintEligibilityCache_PrincipalMismatchSkipsCache(t *testing.T) {
	authz, s := authzTestSetup(t)
	ctx := context.Background()
	userA := tid("mec-user-a")
	userB := tid("mec-user-b")
	require.NoError(t, s.CreateUser(ctx, &store.User{ID: userA, Email: "meca@test.com", DisplayName: "a", Role: "member", Status: store.UserStatusActive}))
	require.NoError(t, s.CreateUser(ctx, &store.User{ID: userB, Email: "mecb@test.com", DisplayName: "b", Role: "member", Status: store.UserStatusActive}))

	cache := &mintEligibilityCache{}
	cachedCtx := withMintEligibilityCache(ctx, cache)

	refsA, _, _, err := authz.principalClosure(cachedCtx, activeUserPrincipal(userA))
	require.NoError(t, err)
	refsB, _, _, err := authz.principalClosure(cachedCtx, activeUserPrincipal(userB))
	require.NoError(t, err)

	if len(refsA) != 1 || refsA[0].ID != userA {
		t.Fatalf("sanity: refsA must resolve to userA, got %+v", refsA)
	}
	if len(refsB) != 1 || refsB[0].ID != userB {
		t.Errorf("a second principal sharing one cache must get its OWN closure, not the first principal's: got %+v, want ID %q", refsB, userB)
	}

	// Same guard, for activeSystemScopeCandidates's system-scope slot: two
	// principals with DIFFERENT system-scoped grants, sharing one cache,
	// must each see their own candidates.
	sysUserA := tid("mec-sys-user-a")
	sysUserB := tid("mec-sys-user-b")
	systemRoleUserWithPermissions(t, s, sysUserA, []string{"quota.read"})
	systemRoleUserWithPermissions(t, s, sysUserB, []string{"scheduled_event.read"})

	sysCache := &mintEligibilityCache{}
	sysCachedCtx := withMintEligibilityCache(ctx, sysCache)

	candidatesA, roleDefsA, _, err := authz.activeSystemScopeCandidates(sysCachedCtx, activeUserPrincipal(sysUserA))
	require.NoError(t, err)
	candidatesB, roleDefsB, _, err := authz.activeSystemScopeCandidates(sysCachedCtx, activeUserPrincipal(sysUserB))
	require.NoError(t, err)

	if !candidateSetHasPermission(candidatesA, roleDefsA, "quota.read") {
		t.Fatal("sanity: sysUserA's candidates must include quota.read")
	}
	if !candidateSetHasPermission(candidatesB, roleDefsB, "scheduled_event.read") {
		t.Error("a second principal sharing one cache must get its OWN system-scope candidates, not the first principal's")
	}
	if candidateSetHasPermission(candidatesB, roleDefsB, "quota.read") {
		t.Error("sysUserB's candidates must not include sysUserA's quota.read grant")
	}
}

// countingConstraintStore wraps a store.Store and counts calls to
// ListAccessConstraints, so a test can assert the mint-eligibility cache
// actually prevents the access-constraint table from being reloaded once
// per (permission, project) pair.
type countingConstraintStore struct {
	store.Store
	listAccessConstraintCalls int
}

func (s *countingConstraintStore) ListAccessConstraints(ctx context.Context, limit, offset int) ([]*store.AccessConstraint, error) {
	s.listAccessConstraintCalls++
	return s.Store.ListAccessConstraints(ctx, limit, offset)
}

// TestCanMintSelector_HubBoundary_ConstraintTableLoadedOnceForBatch is the
// regression: evaluating a relationship-eligible
// selector across multiple permissions and multiple candidate projects must
// load the access-constraint table once per CanMintSelector call, not once
// per (permission, project) pair.
func TestCanMintSelector_HubBoundary_ConstraintTableLoadedOnceForBatch(t *testing.T) {
	_, s := authzTestSetup(t)
	ctx := context.Background()
	userID := tid("perf-user")
	projectA := tid("perf-proj-a")
	projectB := tid("perf-proj-b")
	createDelegateTestProject(t, s, projectA, "perf-proj-a", "test")
	createDelegateTestProject(t, s, projectB, "perf-proj-b", "test")
	createTestUserWithProjectRole(t, s, userID, "perf@test.com", projectA, store.ProjectRoleMember)
	createTestUserWithProjectRole(t, s, userID, "perf@test.com", projectB, store.ProjectRoleMember)

	counting := &countingConstraintStore{Store: s}
	authz := NewAuthzService(counting, nil)

	// Two permissions (agent:attach, agent:port_access), each relationship-
	// eligible via hasRelevantProjectAdmission, each of which would
	// otherwise re-check both projects' constraint tables independently:
	// without the cache this is 2 permissions x 2 projects = 4 reloads.
	results, err := authz.CanMintSelector(ctx, activeUserPrincipal(userID), TokenBoundary{Kind: BoundaryKindHub}, []string{"agent:attach", "agent:port_access"})
	require.NoError(t, err)
	require.Len(t, results, 2)
	for _, r := range results {
		if !r.OK {
			t.Errorf("expected relationship eligibility for %+v", r)
		}
	}

	if counting.listAccessConstraintCalls > 1 {
		t.Errorf("ListAccessConstraints called %d times for one CanMintSelector batch (2 permissions x 2 projects); want at most 1 (cached)", counting.listAccessConstraintCalls)
	}
}

// TestCanMintSelector_ConstraintLoadErrorReturnsError proves a transient
// access-constraint-table load failure surfaces as an error from
// CanMintSelector on three of its paths: the Hub-boundary flat/system path
// (hubPermissionEligible -> MintTimeSystemGrant), the Project-boundary
// relationship path (selectorMintEligible -> permissionSurvivesProjectConstraints),
// and the Project-boundary flat-role path (selectorMintEligible ->
// hasProjectRoleFlatPermission -> projectScopedPermissionsStrict). See
// TestHasAnyProjectBinding_ConstraintLoadErrorReturnsError and
// TestHasRelevantProjectAdmission_ConstraintLoadErrorReturnsError for the two
// remaining CanMintSelector paths, exercised via a direct call to each
// helper: through CanMintSelector itself, MintTimeSystemGrant's unconditional
// constraint load and the per-call cache both return before either path is
// tried. None of the three subtests below should return a SelectorEligibility
// slice.
func TestCanMintSelector_ConstraintLoadErrorReturnsError(t *testing.T) {
	t.Run("hub_boundary", func(t *testing.T) {
		_, s := authzTestSetup(t)
		ctx := context.Background()
		userID := tid("cms-cle-hub")
		createTestUserWithRole(t, s, userID, "cmsclehub@test.com", "member", store.SystemRoleHubMember)

		failing := &r2FailingStore{Store: s, failListConstraints: errors.New("injected: constraint load failure")}
		authz := NewAuthzService(failing, slog.Default())

		// Mirrors TestCanMintSelector_HubBoundary_CatalogOnlyGrantEligible:
		// a hub-member's catalog-only skill:read reaches MintTimeSystemGrant,
		// which loads the constraint table via the error-returning form.
		results, err := authz.CanMintSelector(ctx, activeUserPrincipal(userID), TokenBoundary{Kind: BoundaryKindHub}, []string{"skill:read"})
		require.Error(t, err)
		require.ErrorIs(t, err, ErrProjectAccessDenied)
		require.Nil(t, results)
	})

	t.Run("project_boundary_relationship_selector", func(t *testing.T) {
		_, s := authzTestSetup(t)
		ctx := context.Background()
		userID := tid("cms-cle-proj")
		projectID := tid("cms-cle-proj-p")
		createDelegateTestProject(t, s, projectID, "cms-cle-proj-p", "test")
		createTestUserWithProjectRole(t, s, userID, "cmscleproj@test.com", projectID, store.ProjectRoleMember)

		failing := &r2FailingStore{Store: s, failListConstraints: errors.New("injected: constraint load failure")}
		authz := NewAuthzService(failing, slog.Default())

		// Mirrors TestCanMintSelector_RelationshipEligibility_AgentAttach_NoExistingTargetRequired:
		// an ordinary member's agent:attach under a Project boundary reaches
		// permissionSurvivesProjectConstraints directly via selectorMintEligible.
		results, err := authz.CanMintSelector(ctx, activeUserPrincipal(userID), TokenBoundary{Kind: BoundaryKindProject, ProjectID: projectID}, []string{"agent:attach"})
		require.Error(t, err)
		require.ErrorIs(t, err, ErrProjectAccessDenied)
		require.Nil(t, results)
	})

	t.Run("project_boundary_flat_role_descriptor", func(t *testing.T) {
		_, s := authzTestSetup(t)
		ctx := context.Background()
		userID := tid("cms-cle-flat")
		projectID := tid("cms-cle-flat-p")
		createDelegateTestProject(t, s, projectID, "cms-cle-flat-p", "test")
		// project-admin carries agent.delete, a permission with no
		// MintEligibilityRegistry descriptor (flat-role default).
		createTestUserWithProjectRole(t, s, userID, "cmscleflat@test.com", projectID, store.ProjectRoleAdmin)

		failing := &r2FailingStore{Store: s, failListConstraints: errors.New("injected: constraint load failure")}
		authz := NewAuthzService(failing, slog.Default())

		// Mirrors TestCanMintSelector_ProjectBoundary_SuperAdminWithoutMembership_AdmittedButFlatIneligible's
		// flat-descriptor selector: agent:delete has no MintEligibilityRegistry
		// entry, so selectorMintEligible's default branch reaches
		// hasProjectRoleFlatPermission -> projectScopedPermissionsStrict.
		results, err := authz.CanMintSelector(ctx, activeUserPrincipal(userID), TokenBoundary{Kind: BoundaryKindProject, ProjectID: projectID}, []string{"agent:delete"})
		require.Error(t, err)
		require.ErrorIs(t, err, ErrProjectAccessDenied)
		require.Nil(t, results)
	})
}

// TestHasProjectRoleFlatPermission_BindingListErrorWrapsSentinel proves that
// a store failure on the flat mint path's OWN group/binding resolution
// (projectScopedGrants) is classified with ErrProjectAccessDenied, the same
// as a constraint-table load failure on this path — matching every other
// CanMintSelector path's error classification. getProjectScopedPermissions's
// non-strict form keeps its unwrapped error, for useraccesstoken.go.
func TestHasProjectRoleFlatPermission_BindingListErrorWrapsSentinel(t *testing.T) {
	_, s := authzTestSetup(t)
	ctx := context.Background()
	userID := tid("hprfp-ble")
	projectID := tid("hprfp-ble-proj")
	createDelegateTestProject(t, s, projectID, "hprfp-ble-proj", "test")
	createTestUserWithProjectRole(t, s, userID, "hprfpble@test.com", projectID, store.ProjectRoleAdmin)

	failing := &r2FailingStore{Store: s, failListBindings: errors.New("injected: binding list failure")}
	authz := NewAuthzService(failing, slog.Default())

	ok, err := authz.hasProjectRoleFlatPermission(ctx, activeUserPrincipal(userID), projectID, "agent.delete")
	require.Error(t, err)
	require.ErrorIs(t, err, ErrProjectAccessDenied)
	if ok {
		t.Error("a binding-list failure must deny, not admit")
	}
}

// TestGetProjectScopedPermissions_GroupResolutionFailureLogsAndErrors proves
// getProjectScopedPermissions's own behavior on a group-resolution failure is
// independent of projectScopedPermissionsStrict, despite both sharing
// projectScopedGrants: it emits a Warn log and returns an unwrapped error,
// not ErrProjectAccessDenied -- useraccesstoken.go's caller does not expect
// that sentinel.
func TestGetProjectScopedPermissions_GroupResolutionFailureLogsAndErrors(t *testing.T) {
	_, s := authzTestSetup(t)
	ctx := context.Background()
	userID := tid("gpsp-grf")
	projectID := tid("gpsp-grf-proj")
	createDelegateTestProject(t, s, projectID, "gpsp-grf-proj", "test")

	failing := &r2FailingStore{Store: s, failGetEffectiveGroups: errors.New("injected: group resolution failure")}
	logger, buf := federationAuthCaptureBuffer()
	authz := NewAuthzService(failing, logger)

	_, err := authz.getProjectScopedPermissions(ctx, store.RoleBindingPrincipalUser, userID, projectID)
	require.Error(t, err)
	if errors.Is(err, ErrProjectAccessDenied) {
		t.Error("getProjectScopedPermissions must keep its unwrapped group-resolution error, not ErrProjectAccessDenied")
	}
	if got := countWarnLines(t, buf, "failed to get effective groups for project-scoped permission resolution (fail-closed)"); got != 1 {
		t.Errorf("expected exactly 1 Warn log line for the group-resolution failure, got %d", got)
	}
}

// TestGetProjectScopedPermissions_ConstraintLoadFailureDeniesAllWithoutError
// pins getProjectScopedPermissions's other half of the parity with
// projectScopedPermissionsStrict: on a constraint-table load failure,
// getProjectScopedPermissions returns an empty permission set with a nil
// error (the deny-all restriction from loadAccessConstraintRestrictions),
// which is the contract useraccesstoken.go's caller relies on --
// projectScopedPermissionsStrict, on the identical failure, returns an error
// wrapping ErrProjectAccessDenied instead.
func TestGetProjectScopedPermissions_ConstraintLoadFailureDeniesAllWithoutError(t *testing.T) {
	_, s := authzTestSetup(t)
	ctx := context.Background()
	userID := tid("gpsp-cle")
	projectID := tid("gpsp-cle-proj")
	createDelegateTestProject(t, s, projectID, "gpsp-cle-proj", "test")
	require.NoError(t, s.CreateUser(ctx, &store.User{ID: userID, Email: "gpspcle@test.com", DisplayName: "u", Role: "member", Status: store.UserStatusActive}))

	rd := createTestRoleDefinition(t, s, "gpsp-cle-role", store.RoleScopeProject, []string{"agent.read"})
	_, err := s.CreateRoleBinding(ctx, &store.RoleBinding{
		RoleDefinitionID: rd.ID, PrincipalType: store.RoleBindingPrincipalUser, PrincipalID: userID,
		ScopeType: store.RoleScopeProject, ScopeID: projectID, CreatedBy: "test",
	})
	require.NoError(t, err)

	failing := &r2FailingStore{Store: s, failListConstraints: errors.New("injected: constraint load failure")}
	authz := NewAuthzService(failing, slog.Default())

	perms, err := authz.getProjectScopedPermissions(ctx, store.RoleBindingPrincipalUser, userID, projectID)
	require.NoError(t, err)
	require.Empty(t, perms)

	// The other side of the same fixture: projectScopedPermissionsStrict
	// denies the identical failure with an error instead.
	_, strictErr := authz.projectScopedPermissionsStrict(ctx, store.RoleBindingPrincipalUser, userID, projectID)
	require.Error(t, strictErr)
	require.ErrorIs(t, strictErr, ErrProjectAccessDenied)
}

// --- Full contradiction matrix ----------------------------------------------

func TestResolveTargetScope_InstanceOnlyPermission_CollectionEvidenceDenied(t *testing.T) {
	// agent.attach is CapabilityResource (instance-only): its
	// CollectionTargetClasses entry is reviewed EMPTY. A malformed
	// missing-instance request for it must never be accepted as a declared
	// collection target, regardless of resource-family reasoning.
	got := ResolveTargetScope(Resource{Type: "agent"}, TargetScopeEvidence{
		IsCollectionLevel:   true,
		CollectionScope:     TargetScopeProject,
		CollectionProjectID: "p1",
		PermissionID:        "agent.attach",
	})
	if got.Kind != TargetScopeUnknown {
		t.Errorf("collection evidence for instance-only agent.attach must resolve Unknown: got %+v", got)
	}
}

func TestResolveTargetScope_SystemParentWithProjectEvidence_Denied(t *testing.T) {
	// Resource{Type:skill, ParentType:system} paired with Project evidence
	// for skill.list is a contradiction: the resource independently claims
	// system/hub scope.
	got := ResolveTargetScope(Resource{Type: "skill", ParentType: "system"}, TargetScopeEvidence{
		IsCollectionLevel:   true,
		CollectionScope:     TargetScopeProject,
		CollectionProjectID: "p1",
		PermissionID:        "skill.list",
	})
	if got.Kind != TargetScopeUnknown {
		t.Errorf("system-parented resource with Project evidence must resolve Unknown: got %+v", got)
	}
}

func TestResolveTargetScope_HubEvidenceWithExplicitProjectScopeKind_Denied(t *testing.T) {
	// Hub evidence for skill.list, but the resource's own ScopeKind
	// explicitly says "project" -- a contradiction.
	got := ResolveTargetScope(Resource{Type: "skill", ScopeKind: store.SkillScopeProject}, TargetScopeEvidence{
		IsCollectionLevel: true,
		CollectionScope:   TargetScopeHub,
		PermissionID:      "skill.list",
	})
	if got.Kind != TargetScopeUnknown {
		t.Errorf("Hub evidence with an explicit project ScopeKind must resolve Unknown: got %+v", got)
	}
}

func TestResolveTargetScope_ResourceTypeMismatchesPermission_Denied(t *testing.T) {
	// Resource.Type ("agent") does not match evidence.PermissionID's own
	// resource type ("skill" for skill.create).
	got := ResolveTargetScope(Resource{Type: "agent"}, TargetScopeEvidence{
		IsCollectionLevel:   true,
		CollectionScope:     TargetScopeProject,
		CollectionProjectID: "p1",
		PermissionID:        "skill.create",
	})
	if got.Kind != TargetScopeUnknown {
		t.Errorf("mismatched Resource.Type vs evidence.PermissionID must resolve Unknown: got %+v", got)
	}
}

// --- Explicit FlatRole descriptor must call hasProjectRoleFlatPermission,
// and OR behavior when the relationship alternative legitimately succeeds ---

// testFlatRoleOnlyPermissionID is an injected test-only MintEligibilityRegistry
// entry (no such descriptor exists in production data today, since
// agent.attach/port_access are Relationship-only) — needed to exercise the
// FlatRole branch at all. testFlatAndRelationshipPermRoleID reuses the real
// agent.attach permission (temporarily overriding its descriptor, restored
// via withTestFlatRoleDescriptor's cleanup) to exercise OR behavior against
// a real relationship-eligible permission.
const (
	testFlatRoleOnlyPermissionID      = "test.flat_role_only_permission"
	testFlatAndRelationshipPermRoleID = "agent.attach"
)

func withTestFlatRoleDescriptor(t *testing.T, permissionID string, sources []permissions.MintEligibilitySource) {
	t.Helper()
	original, hadOriginal := permissions.MintEligibilityRegistry[permissionID]
	permissions.MintEligibilityRegistry[permissionID] = permissions.MintEligibilityDescriptor{
		PermissionID: permissionID,
		Sources:      sources,
	}
	t.Cleanup(func() {
		if hadOriginal {
			permissions.MintEligibilityRegistry[permissionID] = original
		} else {
			delete(permissions.MintEligibilityRegistry, permissionID)
		}
	})
}

func TestSelectorMintEligible_ExplicitFlatRoleSource_ProvesProjectRole(t *testing.T) {
	authz, s := authzTestSetup(t)
	ctx := context.Background()
	userID := tid("smef-1")
	projectID := tid("smef-1-proj")
	createDelegateTestProject(t, s, projectID, "smef-1-proj", "test")
	createTestUserWithProjectRole(t, s, userID, "smef1@test.com", projectID, store.ProjectRoleMember)

	withTestFlatRoleDescriptor(t, testFlatRoleOnlyPermissionID, []permissions.MintEligibilitySource{
		{Kind: permissions.MintEligibilityFlatRole},
	})

	// The test permission is not actually in the project-member role's
	// permission set, so an explicit FlatRole descriptor must still deny:
	// an explicit FlatRole source is proven via hasProjectRoleFlatPermission,
	// never assumed.
	ok, reason, err := authz.selectorMintEligible(ctx, activeUserPrincipal(userID), TokenBoundary{Kind: BoundaryKindProject, ProjectID: projectID}, []string{testFlatRoleOnlyPermissionID})
	require.NoError(t, err)
	if ok {
		t.Error("an explicit FlatRole descriptor must still require the project role to actually carry the permission")
	}
	if reason != MintDenialFlatRoleInsufficient {
		t.Errorf("a descriptor with no Relationship source must return MintDenialFlatRoleInsufficient, not a relationship-flavored reason; got %q", reason)
	}
}

func TestSelectorMintEligible_FlatRoleAndRelationship_ORBehavior(t *testing.T) {
	authz, s := authzTestSetup(t)
	ctx := context.Background()
	userID := tid("smef-2")
	projectID := tid("smef-2-proj")
	createDelegateTestProject(t, s, projectID, "smef-2-proj", "test")
	createTestUserWithProjectRole(t, s, userID, "smef2@test.com", projectID, store.ProjectRoleMember)

	// agent.attach's real descriptor is Relationship-only; temporarily add
	// a FlatRole source alongside it (which will fail, since the member's
	// role does not carry it as a flat permission) to prove the
	// Relationship alternative still succeeds via OR.
	withTestFlatRoleDescriptor(t, testFlatAndRelationshipPermRoleID, []permissions.MintEligibilitySource{
		{Kind: permissions.MintEligibilityFlatRole},
		{Kind: permissions.MintEligibilityRelationship, RelationshipTypes: []string{"owner", "ancestor"}},
	})

	ok, _, err := authz.selectorMintEligible(ctx, activeUserPrincipal(userID), TokenBoundary{Kind: BoundaryKindProject, ProjectID: projectID}, []string{testFlatAndRelationshipPermRoleID})
	require.NoError(t, err)
	if !ok {
		t.Error("FlatRole failing must not prevent the Relationship alternative from succeeding (OR semantics)")
	}
}

// TestSelectorMintEligible_FlatRoleAndRelationship_BothFail_DenialReason
// pins the MintDenialReason chosen for a mixed-source descriptor when
// NEITHER source succeeds: a Relationship source that was actually tried
// and failed keeps MintDenialNoRelationshipCandidacy, even though a
// FlatRole source was also present and also failed.
func TestSelectorMintEligible_FlatRoleAndRelationship_BothFail_DenialReason(t *testing.T) {
	authz, s := authzTestSetup(t)
	ctx := context.Background()
	userID := tid("smef-4")
	projectID := tid("smef-4-proj")
	createDelegateTestProject(t, s, projectID, "smef-4-proj", "test")
	createTestUserWithProjectRole(t, s, userID, "smef4@test.com", projectID, store.ProjectRoleMember)
	_, err := s.CreateAccessConstraint(ctx, &store.AccessConstraint{
		Name: "smef-4-constraint", SubjectKind: store.ConstraintSubjectAllPrincipals,
		ScopeType: store.RoleScopeProject, ScopeID: projectID,
		MaximumPermissions: []string{"agent.read"}, // excludes agent.attach
		Purpose:            "test: mixed-source denial reason when both sources fail",
	})
	require.NoError(t, err)

	withTestFlatRoleDescriptor(t, testFlatAndRelationshipPermRoleID, []permissions.MintEligibilitySource{
		{Kind: permissions.MintEligibilityFlatRole},
		{Kind: permissions.MintEligibilityRelationship, RelationshipTypes: []string{"owner", "ancestor"}},
	})

	ok, reason, err := authz.selectorMintEligible(ctx, activeUserPrincipal(userID), TokenBoundary{Kind: BoundaryKindProject, ProjectID: projectID}, []string{testFlatAndRelationshipPermRoleID})
	require.NoError(t, err)
	if ok {
		t.Fatal("both FlatRole and Relationship failing must deny")
	}
	if reason != MintDenialNoRelationshipCandidacy {
		t.Errorf("a descriptor with a Relationship source that was tried and failed must return MintDenialNoRelationshipCandidacy even though a FlatRole source was also present and failed; got %q", reason)
	}
}

func TestSelectorMintEligible_RelationshipDeniedByProjectConstraint(t *testing.T) {
	authz, s := authzTestSetup(t)
	ctx := context.Background()
	userID := tid("smef-3")
	projectID := tid("smef-3-proj")
	createDelegateTestProject(t, s, projectID, "smef-3-proj", "test")
	createTestUserWithProjectRole(t, s, userID, "smef3@test.com", projectID, store.ProjectRoleMember)
	_, err := s.CreateAccessConstraint(ctx, &store.AccessConstraint{
		Name: "smef-3-constraint", SubjectKind: store.ConstraintSubjectAllPrincipals,
		ScopeType: store.RoleScopeProject, ScopeID: projectID,
		MaximumPermissions: []string{"agent.read"}, // excludes agent.attach
		Purpose:            "test: relationship denied by project constraint",
	})
	require.NoError(t, err)

	// agent:attach is relationship-eligible for an ordinary member, but the
	// project's access constraint specifically excludes agent.attach --
	// relationship candidacy must not override that.
	results, err := authz.CanMintSelector(ctx, activeUserPrincipal(userID), TokenBoundary{Kind: BoundaryKindProject, ProjectID: projectID}, []string{"agent:attach"})
	require.NoError(t, err)
	require.Len(t, results, 1)
	if results[0].OK {
		t.Errorf("a project constraint excluding agent.attach must deny relationship-based mint eligibility: %+v", results[0])
	}
}

func TestHubPermissionEligible_RelationshipDeniedWhenAllProjectsConstrained(t *testing.T) {
	authz, s := authzTestSetup(t)
	ctx := context.Background()
	userID := tid("smef-4")
	projectID := tid("smef-4-proj")
	createDelegateTestProject(t, s, projectID, "smef-4-proj", "test")
	createTestUserWithProjectRole(t, s, userID, "smef4@test.com", projectID, store.ProjectRoleMember)
	_, err := s.CreateAccessConstraint(ctx, &store.AccessConstraint{
		Name: "smef-4-constraint", SubjectKind: store.ConstraintSubjectAllPrincipals,
		ScopeType: store.RoleScopeProject, ScopeID: projectID,
		MaximumPermissions: []string{"agent.read"}, // excludes agent.attach
		Purpose:            "test: hub relationship path respects constraints",
	})
	require.NoError(t, err)

	// Under a Hub boundary, the relationship alternative must also respect
	// the constraint on the user's only project.
	results, err := authz.CanMintSelector(ctx, activeUserPrincipal(userID), TokenBoundary{Kind: BoundaryKindHub}, []string{"agent:attach"})
	require.NoError(t, err)
	require.Len(t, results, 1)
	if results[0].OK {
		t.Errorf("hub-boundary relationship eligibility must respect a constraint excluding agent.attach from the user's only project: %+v", results[0])
	}
}

// --- Explicit reviewed ScopeKind allowlist -----------------------------------

func TestValidateRealProjectClass_UncuratedTypeRejectsNonEmptyScopeKind(t *testing.T) {
	authz, s := authzTestSetup(t)
	ctx := context.Background()
	projectID := tid("vrpc-1-proj")
	userID := tid("vrpc-1")
	createDelegateTestProject(t, s, projectID, "vrpc-1-proj", "test")
	systemRoleUserWithPermissions(t, s, userID, []string{"agent.delete"})

	// "agent" has no reviewed scope-kind semantics: a non-empty ScopeKind
	// must be rejected outright, not silently accepted as "any string."
	_, err := authz.SystemAuthorityProof(ctx, activeUserPrincipal(userID), projectID, "agent.delete", ProjectTargetClass{ResourceType: "agent", ScopeKind: "bogus"})
	if err == nil {
		t.Error("a non-empty ScopeKind for a resource type with no reviewed scope semantics must be rejected")
	}
}

func TestValidateRealProjectClass_MaterialTypeAcceptsRegisteredValues(t *testing.T) {
	// "secret" is pre-registered for F.2 with explicit allowed values.
	// validateRealProjectClass itself (not the full SystemAuthorityProof
	// pipeline, which also needs a Registry-backed permission ID) proves
	// the allowlist accepts a registered value and rejects an unregistered
	// one.
	if err := validateRealProjectClass("does.not.exist.in.registry", ProjectTargetClass{ResourceType: "secret", ScopeKind: "hub"}); err == nil {
		t.Fatal("expected an error for a permission ID absent from Registry (resource-type mismatch check must still fire)")
	}
}

func TestValidRealProjectScopeKinds_MaterialTypesRegistered(t *testing.T) {
	for _, rt := range []string{"secret", "env_var", "skill_injection"} {
		allowed, ok := validRealProjectScopeKinds[rt]
		if !ok || len(allowed) == 0 {
			t.Errorf("resource type %q must have explicit registered ScopeKind values for F.2", rt)
		}
	}
}

// TestValidRealProjectScopeKinds_MatchesReviewedScopeKindResourceTypes ties
// validRealProjectScopeKinds to isReviewedScopeKindResourceType: every
// resource type computeTargetFacts treats as having reviewed ScopeKind
// semantics (skill/template/harness_config) must have an entry here, so the
// two functions cannot silently diverge on which types carry a ScopeKind
// concept at all.
func TestValidRealProjectScopeKinds_MatchesReviewedScopeKindResourceTypes(t *testing.T) {
	for _, rt := range []string{permissions.ResourceSkill, permissions.ResourceTemplate, permissions.ResourceHarnessConfig} {
		if !isReviewedScopeKindResourceType(rt) {
			t.Fatalf("test assumption broken: %q is expected to be a reviewed-scope-kind resource type", rt)
		}
		if _, ok := validRealProjectScopeKinds[rt]; !ok {
			t.Errorf("resource type %q is reviewed-scope-kind but has no validRealProjectScopeKinds entry", rt)
		}
	}
}

// TestValidRealProjectScopeKinds_MaterialRowsNowLive pins that the three
// material resource types resolve to live Registry permissions, so
// validateRealProjectClass accepts a registered scope kind for them and
// still rejects a mismatched resource type. A real material permission
// paired with one of its registered ScopeKind values validates; a class
// whose ResourceType does not match the permission's own resource type is
// rejected.
func TestValidRealProjectScopeKinds_MaterialRowsNowLive(t *testing.T) {
	for _, rt := range []string{permissions.ResourceSecret, permissions.ResourceEnvVar, permissions.ResourceSkillInjection} {
		found := false
		for _, p := range permissions.Registry {
			if p.Resource == rt {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("resource type %q has no live Registry permission", rt)
		}
	}
	if err := validateRealProjectClass("secret.deliver", ProjectTargetClass{ResourceType: permissions.ResourceSecret, ScopeKind: store.ScopeProject}); err != nil {
		t.Errorf("secret.deliver with a registered scope kind should validate: %v", err)
	}
	if err := validateRealProjectClass("secret.deliver", ProjectTargetClass{ResourceType: permissions.ResourceEnvVar, ScopeKind: store.ScopeProject}); err == nil {
		t.Error("a class resource type mismatched with the permission's own resource type must still be rejected")
	}
}

// --- Real-route review corrections + supplied-unknown-metadata coherence ---

func TestResolveTargetScope_RoleBindingReadIsHubOnly(t *testing.T) {
	// handleAdminRoleBindings authorizes GET (list) against a hard-coded
	// Resource{Type:"role_binding", ID:"hub"} regardless of the bindings'
	// own scope -- role_binding.read is Hub-only for collection purposes.
	hub := ResolveTargetScope(Resource{Type: "role_binding"}, TargetScopeEvidence{
		IsCollectionLevel: true, CollectionScope: TargetScopeHub, PermissionID: "role_binding.read",
	})
	if hub.Kind != TargetScopeHub {
		t.Errorf("role_binding.read Hub collection evidence: got %+v, want Hub", hub)
	}
	proj := ResolveTargetScope(Resource{Type: "role_binding"}, TargetScopeEvidence{
		IsCollectionLevel: true, CollectionScope: TargetScopeProject, CollectionProjectID: "p1", PermissionID: "role_binding.read",
	})
	if proj.Kind != TargetScopeUnknown {
		t.Errorf("role_binding.read Project collection evidence must resolve Unknown (Hub-only): got %+v", proj)
	}
}

func TestResolveTargetScope_AccessConstraintAdminIsHubOnly(t *testing.T) {
	// requireConstraintAdminPermission authorizes against a hard-coded
	// Resource{Type:"access_constraint", ID:"hub"} regardless of the
	// constraint record's own ScopeType.
	hub := ResolveTargetScope(Resource{Type: "access_constraint"}, TargetScopeEvidence{
		IsCollectionLevel: true, CollectionScope: TargetScopeHub, PermissionID: "access_constraint.admin",
	})
	if hub.Kind != TargetScopeHub {
		t.Errorf("access_constraint.admin Hub collection evidence: got %+v, want Hub", hub)
	}
}

func TestResolveTargetScope_GCPServiceAccountAssignIsInstanceOnly(t *testing.T) {
	// gcpServiceAccountResource(sa) always builds
	// a Resource for the EXISTING gcp_service_account being assigned, whose
	// ParentType/ParentID (when set) is that SA's own scope -- never the
	// new agent being created/patched. assign is instance-only: collection
	// evidence for it must deny.
	got := ResolveTargetScope(Resource{Type: "gcp_service_account"}, TargetScopeEvidence{
		IsCollectionLevel: true, CollectionScope: TargetScopeProject, CollectionProjectID: "p1", PermissionID: "gcp_service_account.assign",
	})
	if got.Kind != TargetScopeUnknown {
		t.Errorf("gcp_service_account.assign collection evidence must resolve Unknown (instance-only): got %+v", got)
	}
}

func TestResolveTargetScope_UnrecognizedParentTypeIsUnknown(t *testing.T) {
	got := ResolveTargetScope(Resource{Type: "agent", ParentType: "bogus", ParentID: "p1"}, TargetScopeEvidence{})
	if got.Kind != TargetScopeUnknown {
		t.Errorf("unrecognized ParentType must resolve Unknown (contradiction, not absent): got %+v", got)
	}
}

func TestResolveTargetScope_OrphanParentIDIsUnknown(t *testing.T) {
	// ParentID set with no declared ParentType is a malformed fact, not "no
	// parent claim."
	got := ResolveTargetScope(Resource{Type: "agent", ParentID: "p1"}, TargetScopeEvidence{})
	if got.Kind != TargetScopeUnknown {
		t.Errorf("orphan ParentID with blank ParentType must resolve Unknown: got %+v", got)
	}
}

func TestResolveTargetScope_UnrecognizedScopeKindIsUnknown(t *testing.T) {
	got := ResolveTargetScope(Resource{Type: "skill", ScopeKind: "bogus"}, TargetScopeEvidence{})
	if got.Kind != TargetScopeUnknown {
		t.Errorf("unrecognized ScopeKind on a reviewed-scope-kind type must resolve Unknown: got %+v", got)
	}
}

func TestResolveTargetScope_ExistingProjectStillCoherentAfterValidation(t *testing.T) {
	// The simple, coherent existing-project case must still work after the
	// supplied-fact coherence check runs first.
	got := ResolveTargetScope(Resource{Type: "project", ID: "p1"}, TargetScopeEvidence{})
	if got.Kind != TargetScopeProject || got.ProjectID != "p1" {
		t.Errorf("coherent existing project must still resolve Project/p1: got %+v", got)
	}
}

func TestResolveTargetScope_CollectionEvidenceUnrecognizedParentTypeIsUnknown(t *testing.T) {
	got := ResolveTargetScope(Resource{Type: "agent", ParentType: "bogus"}, TargetScopeEvidence{
		IsCollectionLevel: true, CollectionScope: TargetScopeProject, CollectionProjectID: "p1", PermissionID: "agent.create",
	})
	if got.Kind != TargetScopeUnknown {
		t.Errorf("collection evidence with an unrecognized ParentType must resolve Unknown: got %+v", got)
	}
}

// --- Exhaustive fast-path contradiction matrix ------------------------------

func TestResolveTargetScope_ExistingProjectWithProjectParent_Denied(t *testing.T) {
	// A project resource has no "project parent" concept at all; claiming
	// one is a contradiction, not something the existing-project fast path
	// may ignore.
	got := ResolveTargetScope(Resource{Type: "project", ID: "A", ParentType: "project", ParentID: "B"}, TargetScopeEvidence{})
	if got.Kind != TargetScopeUnknown {
		t.Errorf("project A with a project parent B must resolve Unknown, not Project/A: got %+v", got)
	}
}

func TestResolveTargetScope_HubTypeWithProjectParent_Denied(t *testing.T) {
	// The Hub singleton has no parent concept at all.
	got := ResolveTargetScope(Resource{Type: "hub", ParentType: "project", ParentID: "X"}, TargetScopeEvidence{})
	if got.Kind != TargetScopeUnknown {
		t.Errorf("hub with a project parent must resolve Unknown, not Hub: got %+v", got)
	}
}

func TestResolveTargetScope_ExistingProjectWithMalformedSystemParent_Denied(t *testing.T) {
	// A system parent must never carry a ParentID; this must be caught
	// BEFORE the existing-project fast path returns Project(A).
	got := ResolveTargetScope(Resource{Type: "project", ID: "A", ParentType: "system", ParentID: "X"}, TargetScopeEvidence{})
	if got.Kind != TargetScopeUnknown {
		t.Errorf("project A with a malformed system parent must resolve Unknown, not Project/A: got %+v", got)
	}
}

func TestResolveTargetScope_SkillWithSystemParentAndExplicitProjectScope_Denied(t *testing.T) {
	// Plain (non-collection) instance path: a skill claiming both a system
	// parent AND an explicit "project" ScopeKind is internally
	// contradictory.
	got := ResolveTargetScope(Resource{Type: "skill", ParentType: "system", ScopeKind: store.SkillScopeProject}, TargetScopeEvidence{})
	if got.Kind != TargetScopeUnknown {
		t.Errorf("skill with system parent + explicit project ScopeKind must resolve Unknown: got %+v", got)
	}
}

func TestResolveTargetScope_CollectionEmptyTypeWithBogusScopeKind_Denied(t *testing.T) {
	// Resource.Type is empty; the effective type only becomes "skill" via
	// the permission's own registry type. The bogus ScopeKind must still be
	// caught once the effective type is resolved, rather than escaping
	// review because Resource.Type itself was empty at validation time.
	got := ResolveTargetScope(Resource{ScopeKind: "bogus"}, TargetScopeEvidence{
		IsCollectionLevel: true, CollectionScope: TargetScopeHub, PermissionID: "skill.list",
	})
	if got.Kind != TargetScopeUnknown {
		t.Errorf("collection evidence with empty Resource.Type + bogus ScopeKind must resolve Unknown: got %+v", got)
	}
}
