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

// groupSATarget builds the real, canonical Resource for a group/SA
// permission's own resource type — groupResource/gcpServiceAccountResource,
// the exact constructors the live handlers and capability computation use —
// project-parented, with its ownership field set to a DIFFERENT user than
// the test principal. Without that, an owner relationship grant could
// admit the request and masquerade as system-role authority in a
// Decide-vs-SystemAuthorityProof comparison.
func groupSATarget(resourceType, projectID, seed string) Resource {
	otherOwner := tid("gsa-diff-owner-" + seed)
	id := tid("gsa-diff-target-" + seed)
	switch resourceType {
	case "group":
		return groupResource(&store.Group{ID: id, ProjectID: projectID, OwnerID: otherOwner})
	case "gcp_service_account":
		return gcpServiceAccountResource(&store.GCPServiceAccount{ID: id, Scope: store.ScopeProject, ScopeID: projectID, CreatedBy: otherOwner})
	default:
		panic("groupSATarget: unsupported resource type " + resourceType)
	}
}

// TestGroupAndGCPServiceAccount_DecideAgreesWithSystemAuthorityProof is the
// differential proof the ruling requires: for every group.*/gcp_service_account.*
// permission reviewed ProjectTargetApplicability=true (including assign),
// Decide (via CheckAccess, exactly as production handlers and capability
// computation call it) and SystemAuthorityProof must agree on a REAL
// project-parented target built with the actual production constructors —
// not a hand-literal Resource and not an assertion against
// SystemAuthorityProof alone, which would be circular (it is the very
// function the table under test drives).
func TestGroupAndGCPServiceAccount_DecideAgreesWithSystemAuthorityProof(t *testing.T) {
	byID := make(map[string]permissions.Permission, len(permissions.Registry))
	for _, p := range permissions.Registry {
		byID[p.ID] = p
	}

	applicable := []string{
		"group.read", "group.update", "group.delete", "group.addMember", "group.removeMember",
		"gcp_service_account.read", "gcp_service_account.delete", "gcp_service_account.verify", "gcp_service_account.assign",
	}
	for _, permID := range applicable {
		permID := permID
		p, ok := byID[permID]
		if !ok {
			t.Fatalf("test assumption broken: %q is not a Registry permission", permID)
		}
		if applies, reviewed := permissions.AppliesToExistingProjectTarget(permID); !reviewed || !applies {
			t.Fatalf("test assumption broken: %q must be reviewed ProjectTargetApplicability=true", permID)
		}

		t.Run(permID+"/decide_agrees_with_system_authority_proof", func(t *testing.T) {
			authz, s := authzTestSetup(t)
			ctx := context.Background()
			projectID := tid("gsa-diff-proj-" + permID)
			userID := tid("gsa-diff-user-" + permID)
			createDelegateTestProject(t, s, projectID, "gsa-diff-"+permID, "someone-else")
			systemRoleUserWithPermissions(t, s, userID, []string{permID})

			target := groupSATarget(p.Resource, projectID, permID)
			identity := NewAuthenticatedUser(userID, userID+"@test.com", "u", store.UserRoleMember, "api")

			decision := authz.CheckAccess(ctx, identity, target, Action(p.Action))
			proof, err := authz.SystemAuthorityProof(ctx, activeUserPrincipal(userID), projectID, permID, ContemplatedProjectClass(permID))
			require.NoError(t, err)

			if !decision.Allowed {
				t.Fatalf("Decide must allow %s against its real project-parented target when the principal holds a system-scoped grant for it: %s", permID, decision.Reason)
			}
			if decision.Allowed != proof {
				t.Errorf("Decide and SystemAuthorityProof must agree for %s: Decide.Allowed=%v, SystemAuthorityProof=%v", permID, decision.Allowed, proof)
			}
		})
	}
}

// TestGroupAndGCPServiceAccount_NonApplicableRows_SystemAuthorityProofDenied
// covers the other half of the ruling's evidence requirement: group.create/
// group.list and gcp_service_account.create/list/mint are reviewed
// ProjectTargetApplicability=false, and SystemAuthorityProof must deny them
// even when the principal holds a system-scoped grant for the exact
// permission — collection-capable is never inferred from instance-capable.
func TestGroupAndGCPServiceAccount_NonApplicableRows_SystemAuthorityProofDenied(t *testing.T) {
	nonApplicable := []string{"group.create", "group.list", "gcp_service_account.create", "gcp_service_account.list", "gcp_service_account.mint"}
	for _, permID := range nonApplicable {
		permID := permID
		t.Run(permID, func(t *testing.T) {
			if applies, reviewed := permissions.AppliesToExistingProjectTarget(permID); !reviewed || applies {
				t.Fatalf("test assumption broken: %q must be reviewed ProjectTargetApplicability=false", permID)
			}

			authz, s := authzTestSetup(t)
			ctx := context.Background()
			projectID := tid("gsa-na-proj-" + permID)
			userID := tid("gsa-na-user-" + permID)
			createDelegateTestProject(t, s, projectID, "gsa-na-"+permID, "someone-else")
			systemRoleUserWithPermissions(t, s, userID, []string{permID})

			ok, err := authz.SystemAuthorityProof(ctx, activeUserPrincipal(userID), projectID, permID, ContemplatedProjectClass(permID))
			require.NoError(t, err)
			if ok {
				t.Errorf("%s is not reviewed project-applicable and must deny SystemAuthorityProof even when the principal holds a system-scoped grant for it", permID)
			}
		})
	}
}

// TestSeededRoleException_DecideAgreesWithSystemAuthorityProof is finding
// 1's differential proof for the seeded-role exceptions specifically: the
// SAME seeded system role (not a synthetic single-permission role) must
// make Decide agree with SystemAuthorityProof on a real target for each
// entry in seededRoleProjectTargetExceptions.
func TestSeededRoleException_DecideAgreesWithSystemAuthorityProof(t *testing.T) {
	byID := make(map[string]permissions.Permission, len(permissions.Registry))
	for _, p := range permissions.Registry {
		byID[p.ID] = p
	}

	for _, exc := range seededRoleProjectTargetExceptions {
		exc := exc
		p, ok := byID[exc.permissionID]
		if !ok {
			t.Fatalf("test assumption broken: %q is not a Registry permission", exc.permissionID)
		}

		t.Run(exc.role+"/"+exc.permissionID, func(t *testing.T) {
			authz, s := authzTestSetup(t)
			ctx := context.Background()
			projectID := tid("sre-diff-proj-" + exc.role + "-" + exc.permissionID)
			userID := tid("sre-diff-user-" + exc.role + "-" + exc.permissionID)
			createDelegateTestProject(t, s, projectID, "sre-diff-"+exc.role+"-"+exc.permissionID, "someone-else")
			createTestUserWithRole(t, s, userID, "sre-diff-"+exc.role+"-"+exc.permissionID+"@test.com", "member", exc.role)

			target := groupSATarget(p.Resource, projectID, exc.role+"-"+exc.permissionID)
			identity := NewAuthenticatedUser(userID, userID+"@test.com", "u", store.UserRoleMember, "api")

			decision := authz.CheckAccess(ctx, identity, target, Action(p.Action))
			proof, err := authz.SystemAuthorityProof(ctx, activeUserPrincipal(userID), projectID, exc.permissionID, ContemplatedProjectClass(exc.permissionID))
			require.NoError(t, err)

			if !decision.Allowed {
				t.Fatalf("Decide must allow the seeded %s role's %s against its real project-parented target: %s", exc.role, exc.permissionID, decision.Reason)
			}
			if decision.Allowed != proof {
				t.Errorf("Decide and SystemAuthorityProof must agree for seeded %s/%s: Decide.Allowed=%v, SystemAuthorityProof=%v", exc.role, exc.permissionID, decision.Allowed, proof)
			}
		})
	}
}

// TestSystemAuthorityProof_GroupAndGCPServiceAccount_PerPermissionCharacterization
// covers the admission-boundary facts TestGroupAndGCPServiceAccount_DecideAgreesWithSystemAuthorityProof
// does not: no-binding, wrong-permission and constraint-denied controls,
// and the composed ProjectTargetAdmission runtime path, for every
// permission reviewed ProjectTargetApplicability=true. It asserts only
// SystemAuthorityProof/ProjectTargetAdmission, not Decide/CheckAccess — the
// Decide-agreement proof lives in the differential test above.
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
		{"gcp_service_account.assign", "gcp_service_account"},
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
			// target built with the actual production constructor.
			target := groupSATarget(tc.resourceType, projectID, "pos-"+tc.permissionID)
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
				// No early return for a non-project-applicable permission:
				// the deny-by-default guarantee must be proven by actually
				// calling SystemAuthorityProof and asserting false, not
				// assumed from the table it exists to guard — a future
				// change to the gate ordering must be caught here too.
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
			ownTarget := groupSATarget(ownResourceType, projectID, "mm-own-"+exc.role+"-"+exc.permissionID)
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
