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
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/hub/permissions"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/require"
)

// TestSystemAuthorityProof_GroupAndGCPServiceAccount_PerPermissionCharacterization
// characterizes EACH group.* and gcp_service_account.* permission
// individually against its real target construction and current Decide,
// rather than inferring a whole family from a smaller sample. It covers
// every permission whose real, live authorization call site (an enforcement
// gate or a capability computation — see
// ResourceActions["group"]/["gcp_service_account"], capabilities.go)
// evaluates it against a Resource that can carry ParentType="project"
// (groupResource sets this for project_agents groups;
// gcpServiceAccountResource sets it for project-scoped service accounts).
// group.create/group.list (always parentless or a hardcoded hub Resource)
// and gcp_service_account.create/list/mint (never evaluated per-instance)
// are intentionally excluded: their real target construction never produces
// a project-parented Resource.
func TestSystemAuthorityProof_GroupAndGCPServiceAccount_PerPermissionCharacterization(t *testing.T) {
	cases := []struct {
		permissionID string
		resourceType string
	}{
		{"group.read", "group"},
		{"group.update", "group"},
		{"group.delete", "group"},
		{"group.addMember", "group"},
		{"group.removeMember", "group"},
		{"gcp_service_account.read", "gcp_service_account"},
		{"gcp_service_account.delete", "gcp_service_account"},
		{"gcp_service_account.verify", "gcp_service_account"},
	}

	for _, tc := range cases {
		tc := tc

		t.Run(tc.permissionID+"/positive_system_role_establishes_authority", func(t *testing.T) {
			authz, s := authzTestSetup(t)
			ctx := context.Background()
			projectID := tid("gsa-pos-proj-" + tc.permissionID)
			userID := tid("gsa-pos-user-" + tc.permissionID)
			createDelegateTestProject(t, s, projectID, "gsa-pos-"+tc.permissionID, "test")
			systemRoleUserWithPermissions(t, s, userID, []string{tc.permissionID})

			class := ContemplatedProjectClass(tc.permissionID)
			ok, err := authz.SystemAuthorityProof(ctx, activeUserPrincipal(userID), projectID, tc.permissionID, class)
			require.NoError(t, err)
			if !ok {
				t.Fatalf("a system role holding %s must establish target-applicable project authority", tc.permissionID)
			}

			// The composed runtime path agrees, via a real project-parented
			// target of this permission's own resource type.
			target := Resource{Type: tc.resourceType, ID: tid("gsa-pos-target-" + tc.permissionID), ParentType: "project", ParentID: projectID}
			result, err := authz.ProjectTargetAdmission(ctx, activeUserPrincipal(userID), projectID, tc.permissionID, target, nil)
			require.NoError(t, err)
			if !result.Admitted || result.Source != ProjectAccessSourceSystemRole {
				t.Errorf("ProjectTargetAdmission(%s): got %+v, want Admitted via system_role", tc.permissionID, result)
			}
		})

		t.Run(tc.permissionID+"/negative_no_binding_denied", func(t *testing.T) {
			authz, s := authzTestSetup(t)
			ctx := context.Background()
			projectID := tid("gsa-nob-proj-" + tc.permissionID)
			userID := tid("gsa-nob-user-" + tc.permissionID)
			createDelegateTestProject(t, s, projectID, "gsa-nob-"+tc.permissionID, "test")
			require.NoError(t, s.CreateUser(ctx, &store.User{ID: userID, Email: userID + "@test.com", DisplayName: "u", Role: "member", Status: store.UserStatusActive}))

			ok, err := authz.SystemAuthorityProof(ctx, activeUserPrincipal(userID), projectID, tc.permissionID, ContemplatedProjectClass(tc.permissionID))
			require.NoError(t, err)
			if ok {
				t.Errorf("a user with no role binding at all must not establish %s", tc.permissionID)
			}
		})

		t.Run(tc.permissionID+"/negative_wrong_permission_denied", func(t *testing.T) {
			authz, s := authzTestSetup(t)
			ctx := context.Background()
			projectID := tid("gsa-wp-proj-" + tc.permissionID)
			userID := tid("gsa-wp-user-" + tc.permissionID)
			createDelegateTestProject(t, s, projectID, "gsa-wp-"+tc.permissionID, "test")
			// Grant a permission unrelated to the one under test.
			systemRoleUserWithPermissions(t, s, userID, []string{"scheduled_event.read"})

			ok, err := authz.SystemAuthorityProof(ctx, activeUserPrincipal(userID), projectID, tc.permissionID, ContemplatedProjectClass(tc.permissionID))
			require.NoError(t, err)
			if ok {
				t.Errorf("holding an unrelated system permission must not establish %s", tc.permissionID)
			}
		})

		t.Run(tc.permissionID+"/negative_constraint_denied", func(t *testing.T) {
			authz, s := authzTestSetup(t)
			ctx := context.Background()
			projectID := tid("gsa-cd-proj-" + tc.permissionID)
			userID := tid("gsa-cd-user-" + tc.permissionID)
			createDelegateTestProject(t, s, projectID, "gsa-cd-"+tc.permissionID, "test")
			systemRoleUserWithPermissions(t, s, userID, []string{tc.permissionID})
			_, err := s.CreateAccessConstraint(ctx, &store.AccessConstraint{
				Name: "gsa-cd-constraint-" + tc.permissionID, SubjectKind: store.ConstraintSubjectAllPrincipals,
				ScopeType:          store.RoleScopeProject,
				ScopeID:            projectID,
				MaximumPermissions: []string{"scheduled_event.read"}, // excludes tc.permissionID
				Purpose:            "constraint-denied control",
			})
			require.NoError(t, err)

			ok, err := authz.SystemAuthorityProof(ctx, activeUserPrincipal(userID), projectID, tc.permissionID, ContemplatedProjectClass(tc.permissionID))
			require.NoError(t, err)
			if ok {
				t.Errorf("a project-scoped access constraint excluding %s must deny", tc.permissionID)
			}
		})
	}
}

// seededRoleProjectTargetException is one explicit, reviewed (system role,
// permission ID) pair where the seeded role's grant is proven, by its own
// characterization test below, to establish SystemAuthorityProof for an
// arbitrary project's real target today. A pair is added here only when a
// dedicated test proves current Decide reaches that exact target for that
// exact permission — never inferred from a sibling permission (a list
// permission is never inferred from a read permission) or from a sibling
// role.
type seededRoleProjectTargetException struct {
	role         string
	permissionID string
}

var seededRoleProjectTargetExceptions = []seededRoleProjectTargetException{
	{store.SystemRoleHubMember, "group.read"},
	{store.SystemRoleHubMember, "gcp_service_account.read"},
	{store.SystemRoleHubViewer, "group.read"},
	{store.SystemRoleHubViewer, "gcp_service_account.read"},
}

func isSeededRoleProjectTargetException(role, permissionID string) bool {
	for _, e := range seededRoleProjectTargetExceptions {
		if e.role == role && e.permissionID == permissionID {
			return true
		}
	}
	return false
}

// TestSeededRoles_DenyProjectTargetsByDefault is the generic seeded-role
// regression: every permission granted by the hub-member and hub-viewer
// system roles must deny SystemAuthorityProof for an arbitrary, unrelated
// project — the user has no membership, ownership, ancestry, or super-admin
// role on that project — UNLESS the (role, permission) pair is an explicit,
// reviewed entry in seededRoleProjectTargetExceptions. Denial is the
// default; an entry in that list is the only way a case may pass instead.
func TestSeededRoles_DenyProjectTargetsByDefault(t *testing.T) {
	roles := []struct {
		label       string
		roleName    string
		permissions []string
	}{
		{"hub-member", store.SystemRoleHubMember, hubMemberPermissionIDs()},
		{"hub-viewer", store.SystemRoleHubViewer, hubViewerPermissionIDs()},
	}

	for _, r := range roles {
		for _, permID := range r.permissions {
			r, permID := r, permID
			t.Run(r.label+"/"+permID, func(t *testing.T) {
				applies, reviewed := permissions.AppliesToExistingProjectTarget(permID)
				if !reviewed || !applies {
					// Not project-applicable at all: SystemAuthorityProof's
					// own gate denies before any role or binding evaluation
					// is reached, so there is nothing further to prove here.
					return
				}

				authz, s := authzTestSetup(t)
				ctx := context.Background()
				projectID := tid("srg-proj-" + r.label + "-" + permID)
				userID := tid("srg-user-" + r.label + "-" + permID)
				createDelegateTestProject(t, s, projectID, "srg-"+r.label+"-"+permID, "someone-else")
				createTestUserWithRole(t, s, userID, "srg-"+r.label+"-"+permID+"@test.com", "member", r.roleName)

				ok, err := authz.SystemAuthorityProof(ctx, activeUserPrincipal(userID), projectID, permID, ContemplatedProjectClass(permID))
				require.NoError(t, err)

				if isSeededRoleProjectTargetException(r.roleName, permID) {
					if !ok {
						t.Errorf("%s's %s is a reviewed exception and must establish authority for an arbitrary project", r.label, permID)
					}
					return
				}
				if ok {
					t.Errorf("%s's %s is not a reviewed exception and must NOT establish authority for an arbitrary, unrelated project", r.label, permID)
				}
			})
		}
	}
}

// TestSeededRoleException_GroupAndSARead_Controls exercises the required
// controls for each reviewed seededRoleProjectTargetExceptions entry: the
// grant is real (removing it denies), a constraint can still override it,
// and the resulting admission is permission-specific rather than
// permission-agnostic — including across a shared, reused admission cache.
func TestSeededRoleException_GroupAndSARead_Controls(t *testing.T) {
	for _, exc := range seededRoleProjectTargetExceptions {
		exc := exc

		t.Run(exc.role+"/"+exc.permissionID+"/remove_grant_denies", func(t *testing.T) {
			authz, s := authzTestSetup(t)
			ctx := context.Background()
			projectID := tid("sre-rm-proj-" + exc.role + "-" + exc.permissionID)
			userID := tid("sre-rm-user-" + exc.role + "-" + exc.permissionID)
			createDelegateTestProject(t, s, projectID, "sre-rm-"+exc.role+"-"+exc.permissionID, "someone-else")
			createTestUserWithRole(t, s, userID, "sre-rm-"+exc.role+"-"+exc.permissionID+"@test.com", "member", exc.role)

			ok, err := authz.SystemAuthorityProof(ctx, activeUserPrincipal(userID), projectID, exc.permissionID, ContemplatedProjectClass(exc.permissionID))
			require.NoError(t, err)
			require.True(t, ok, "sanity: the reviewed exception must hold before its grant is removed")

			n, err := s.DeleteRoleBindingsForPrincipal(ctx, store.RoleBindingPrincipalUser, userID)
			require.NoError(t, err)
			require.Equal(t, 1, n, "expected exactly the one seeded-role binding to be removed")

			ok, err = authz.SystemAuthorityProof(ctx, activeUserPrincipal(userID), projectID, exc.permissionID, ContemplatedProjectClass(exc.permissionID))
			require.NoError(t, err)
			if ok {
				t.Errorf("removing the user's %s role binding must deny %s", exc.role, exc.permissionID)
			}
		})

		t.Run(exc.role+"/"+exc.permissionID+"/constraint_denies", func(t *testing.T) {
			authz, s := authzTestSetup(t)
			ctx := context.Background()
			projectID := tid("sre-cn-proj-" + exc.role + "-" + exc.permissionID)
			userID := tid("sre-cn-user-" + exc.role + "-" + exc.permissionID)
			createDelegateTestProject(t, s, projectID, "sre-cn-"+exc.role+"-"+exc.permissionID, "someone-else")
			createTestUserWithRole(t, s, userID, "sre-cn-"+exc.role+"-"+exc.permissionID+"@test.com", "member", exc.role)
			_, err := s.CreateAccessConstraint(ctx, &store.AccessConstraint{
				Name: "sre-cn-constraint-" + exc.role + "-" + exc.permissionID, SubjectKind: store.ConstraintSubjectAllPrincipals,
				ScopeType:          store.RoleScopeProject,
				ScopeID:            projectID,
				MaximumPermissions: []string{"scheduled_event.read"}, // excludes exc.permissionID
				Purpose:            "constraint-denied control for a reviewed seeded-role exception",
			})
			require.NoError(t, err)

			ok, err := authz.SystemAuthorityProof(ctx, activeUserPrincipal(userID), projectID, exc.permissionID, ContemplatedProjectClass(exc.permissionID))
			require.NoError(t, err)
			if ok {
				t.Errorf("a project-scoped access constraint excluding %s must override the %s exception", exc.permissionID, exc.role)
			}
		})

		t.Run(exc.role+"/"+exc.permissionID+"/does_not_authorize_unrelated_action_under_memo_reuse", func(t *testing.T) {
			authz, s := authzTestSetup(t)
			ctx := context.Background()
			projectID := tid("sre-mm-proj-" + exc.role + "-" + exc.permissionID)
			userID := tid("sre-mm-user-" + exc.role + "-" + exc.permissionID)
			createDelegateTestProject(t, s, projectID, "sre-mm-"+exc.role+"-"+exc.permissionID, "someone-else")
			createTestUserWithRole(t, s, userID, "sre-mm-"+exc.role+"-"+exc.permissionID+"@test.com", "member", exc.role)

			memo := NewProjectAdmissionCache()
			ownResourceType := "group"
			if exc.permissionID == "gcp_service_account.read" {
				ownResourceType = "gcp_service_account"
			}
			ownTarget := Resource{Type: ownResourceType, ID: tid("sre-mm-own-" + exc.role + "-" + exc.permissionID), ParentType: "project", ParentID: projectID}
			result, err := authz.ProjectTargetAdmission(ctx, activeUserPrincipal(userID), projectID, exc.permissionID, ownTarget, memo)
			require.NoError(t, err)
			require.True(t, result.Admitted, "sanity: the reviewed exception must be admitted for its own permission and target")

			agentTarget := Resource{Type: "agent", ID: tid("sre-mm-agent-" + exc.role + "-" + exc.permissionID), ParentType: "project", ParentID: projectID}
			for _, unrelated := range []string{"agent.read", "agent.attach"} {
				agentResult, err := authz.ProjectTargetAdmission(ctx, activeUserPrincipal(userID), projectID, unrelated, agentTarget, memo)
				require.NoError(t, err)
				if agentResult.Admitted {
					t.Errorf("the same memo that admitted %s must not also admit unrelated %s", exc.permissionID, unrelated)
				}
			}
		})
	}
}
