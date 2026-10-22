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

package hub

import (
	"go/ast"
	"go/parser"
	"go/token"
	"go/types"
	"path/filepath"
	"reflect"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Admin tier A1 census guards (ptone/scion#4240). They walk every non-test
// file of pkg/hub and of the packages moved out of it (guardScanSubdirs:
// code that decided hub authority before a move still does; other packages
// do not decide hub authority) with go/ast, like TestBypassCensus
// and TestSigningKeySource_EveryKeySourceClassified: every site that reads
// authority outside the authorization service's clamped store must be
// classified, decision sites must apply the test identity clamps and the
// activation filter, and no classification may go stale.

// Census classes.
const (
	// censusClamped: the function applies the test identity clamps to the
	// bindings it reads (clampBindingsForUser or clampTestIdentityBindings),
	// at least once per role-binding read, and filters by activation
	// (isBindingActive, bindingActivationOK or evaluateActivation).
	// Verified.
	censusClamped = "decision: clamped"
	// censusExcludesTestIdentities: a holder-counting guard that counts
	// only positively human rows (IsHumanKind or
	// reconcileSkipsUser). Verified.
	censusExcludesTestIdentities = "guard: excludes test identities"
)

// censusVerifiers maps the verified classes to the identifiers the
// function must reference.
var censusVerifiers = map[string][]string{
	censusClamped:                {"clampBindingsForUser", "clampTestIdentityBindings"},
	censusExcludesTestIdentities: {"IsHumanKind", "reconcileSkipsUser"},
}

// censusClampedStoreMethods are the store methods the authorization
// service's clamped store wraps (testFixtureGrantClamp). Only these reads
// through a.store / h.a.store pass the census without a classification.
var censusClampedStoreMethods = map[string]bool{
	"ListRoleBindingsForPrincipals": true,
	"ListRoleBindingsForPrincipal":  true,
}

// censusMutatorPrefix matches store method names that write rather than
// read.
var censusMutatorPrefix = regexp.MustCompile(`^(Create|Update|Delete|Add|Remove|Set|Lock|Replace|Upsert|Migrate|Backfill|Reconcile|Purge|Increment|Touch|Mark|Release|Ensure|Record|Claim)`)

// censusReadFamilyName matches the authority-read methods identified by
// name: group closures and membership predicates.
var censusReadFamilyName = regexp.MustCompile(`^Get(Effective|User|Parent)Groups|RoleBinding|GroupMember|ProjectMember`)

// censusAuthorityReadMethods returns the authority-read methods the census
// tracks, derived from the store.Store method set by reflection: every
// non-mutating method that returns role bindings, project memberships or
// group memberships, or whose name names a group closure or membership
// read. A new store read in any of these families enters the census by
// itself; the required list below proves the derivation covers the known
// family (a read-family coverage assertion).
func censusAuthorityReadMethods(t *testing.T) map[string]bool {
	t.Helper()
	st := reflect.TypeOf((*store.Store)(nil)).Elem()
	returnsAuthority := regexp.MustCompile(`RoleBinding|ProjectMembership|GroupMember`)
	out := map[string]bool{}
	for i := 0; i < st.NumMethod(); i++ {
		m := st.Method(i)
		if censusMutatorPrefix.MatchString(m.Name) {
			continue
		}
		hit := censusReadFamilyName.MatchString(m.Name)
		for j := 0; j < m.Type.NumOut() && !hit; j++ {
			hit = returnsAuthority.MatchString(m.Type.Out(j).String())
		}
		if hit {
			out[m.Name] = true
		}
	}
	for _, name := range []string{
		"ListRoleBindingsForPrincipals", "ListRoleBindingsForPrincipal", "ListRoleBindingsForScope",
		"ListAllRoleBindings", "GetRoleBinding", "CountAllRoleBindings",
		"GetEffectiveGroups", "GetEffectiveGroupsForAgent", "GetUserGroups", "GetParentGroups",
		"GetGroupMembers", "GetGroupMembership", "CountGroupMembersByRole",
		"GetProjectMembership", "ListProjectMembers", "IsProjectMember",
	} {
		_, ok := st.MethodByName(name)
		require.True(t, ok, "store.Store has no method %s", name)
		require.True(t, out[name], "the read-family derivation misses %s", name)
	}
	return out
}

// isRoleBindingRead reports whether a census method reads role bindings
// themselves (the reads the clamps and the activation filter apply to).
func isRoleBindingRead(method string) bool {
	return strings.Contains(method, "RoleBinding")
}

// testAdminAuthorityReadCensus classifies every pkg/hub call of an
// authority-read method that is not made through the authorization
// service's clamped store (receiver a.store / h.a.store inside the
// service), keyed "file:function:method". Reasons say why an unverified
// class needs no clamp.
var testAdminAuthorityReadCensus = map[string]string{
	// The clamps and the perf-trace decorator themselves.
	"authz_test_fixture_clamp.go:ListRoleBindingsForPrincipal:ListRoleBindingsForPrincipal":   "wrapper: the clamp's own read",
	"authz_test_fixture_clamp.go:ListRoleBindingsForPrincipals:ListRoleBindingsForPrincipals": "wrapper: the clamp's own read",
	"perftrace_store.go:GetEffectiveGroups:GetEffectiveGroups":                                "wrapper: perf-trace decorator inside the clamp",
	"perftrace_store.go:ListRoleBindingsForPrincipal:ListRoleBindingsForPrincipal":            "wrapper: perf-trace decorator inside the clamp",
	"perftrace_store.go:ListRoleBindingsForPrincipals:ListRoleBindingsForPrincipals":          "wrapper: perf-trace decorator inside the clamp",

	// Decision sites outside the authorization service: clamped.
	"access_constraint_governance.go:isConstraintAdmin:GetEffectiveGroups":                          censusClamped,
	"access_constraint_governance.go:isConstraintAdmin:ListRoleBindingsForPrincipals":               censusClamped,
	"access_constraint_preview.go:getBasePermissions:ListRoleBindingsForPrincipals":                 censusClamped,
	"authorize_message.go:checkEffectiveMembership:GetEffectiveGroups":                              censusClamped,
	"authorize_message.go:checkEffectiveMembership:ListRoleBindingsForPrincipal":                    censusClamped,
	"authorize_message.go:checkEffectiveMembership:ListRoleBindingsForPrincipals":                   censusClamped,
	"authz_list_helpers.go:resolveUserOwnerProjectIDsOrError:ListRoleBindingsForPrincipal":          censusClamped,
	"chat_membership.go:chatMemberProjectIDs:GetEffectiveGroups":                                    censusClamped,
	"chat_membership.go:chatMemberProjectIDs:ListRoleBindingsForPrincipal":                          censusClamped,
	"chat_membership.go:chatMemberProjectIDs:ListRoleBindingsForPrincipals":                         censusClamped,
	"project_deletion_service.go:checkDeletionGovernanceFromStore:ListRoleBindingsForPrincipal":     censusClamped,
	"project_membership_service.go:actorHasHubRoleBindingAuthorityTx:GetEffectiveGroups":            censusClamped,
	"project_membership_service.go:actorHasHubRoleBindingAuthorityTx:ListRoleBindingsForPrincipals": censusClamped,
	"project_membership_service.go:isActorDirectOwnerFromStore:ListRoleBindingsForPrincipal":        censusClamped,
	"project_membership_service.go:projectEffectiveRole:GetEffectiveGroups":                         censusClamped,
	"project_membership_service.go:projectEffectiveRole:ListRoleBindingsForPrincipal":               censusClamped,
	"project_membership_service.go:projectEffectiveRole:ListRoleBindingsForPrincipals":              censusClamped,
	"project_membership_service.go:projectEffectiveRoleFromStore:GetEffectiveGroups":                censusClamped,
	"project_membership_service.go:projectEffectiveRoleFromStore:ListRoleBindingsForPrincipal":      censusClamped,
	"project_membership_service.go:projectEffectiveRoleFromStore:ListRoleBindingsForPrincipals":     censusClamped,

	"authz.go:activeDirectProjectRoles:ListRoleBindingsForPrincipal": censusClamped,

	// Inside the authorization service: group closures that feed the
	// clamped binding read with the user principal in the same list (the
	// clamps' invariant). Group membership itself carries no authority and
	// no expiry in A1 (group membership expiry is A3).
	"authz.go:authorizationPrincipals:GetEffectiveGroups":               "authz: group closure fed to the clamped binding read with the user principal",
	"authz.go:authorizationPrincipals:GetEffectiveGroupsForAgent":       "authz: agent group closure fed to the clamped binding read",
	"authz.go:getEffectivePermissions:GetEffectiveGroups":               "authz: group closure fed to the clamped binding read with the user principal",
	"authz.go:getEffectivePermissions:GetEffectiveGroupsForAgent":       "authz: agent group closure fed to the clamped binding read",
	"authz.go:hasActiveSystemRole:GetEffectiveGroups":                   "authz: group closure fed to the clamped binding read with the user principal",
	"authz.go:isProjectOwnerOrAdmin:GetEffectiveGroups":                 "authz: group closure fed to the clamped binding read with the user principal",
	"authz_boundary.go:loadPrincipalClosure:GetEffectiveGroups":         "authz: group closure fed to the clamped binding read with the user principal",
	"authz_boundary.go:loadPrincipalClosure:GetEffectiveGroupsForAgent": "authz: agent group closure fed to the clamped binding read",
	"authz_boundary.go:projectScopedGrants:GetEffectiveGroups":          "authz: group closure fed to the clamped binding read with the user principal",
	"authz_boundary.go:projectScopedGrants:GetEffectiveGroupsForAgent":  "authz: agent group closure fed to the clamped binding read",
	"authz_candelegate.go:canDelegateGroupMembership:GetParentGroups":   "delegation: the target group's closure, whose bindings the actor must already hold",
	"authz_candelegate.go:grantPermissionSet:GetParentGroups":           "delegation: the group closure whose role permissions a test admin's grant would confer (test-admin policy check; counts every binding, so it only refuses more)",
	"authz.go:buildMembershipPathChains:GetGroupMembership":             "display: decision explain membership paths",
	"authz.go:buildMembershipPathChains:GetParentGroups":                "display: decision explain membership paths",
	"authz.go:buildMembershipPathChains:GetUserGroups":                  "display: decision explain membership paths",
	"authz.go:isCurrentHubMember:GetGroupMembership":                    "decision: hub-members membership (relationship grant); admin kinds count only while live and enabled (testAdminPrincipalLive)",

	// Holder-counting guards: test identities never count.
	"access_constraint_preview.go:resolveConstraintAdmins:GetEffectiveGroups":                    censusExcludesTestIdentities,
	"access_constraint_preview.go:resolveConstraintAdmins:GetGroupMembers":                       censusExcludesTestIdentities,
	"access_constraint_preview.go:resolveConstraintAdmins:ListRoleBindingsForScope":              censusExcludesTestIdentities,
	"handlers_users_core.go:checkLastSuperAdminTx:ListRoleBindingsForScope":                      censusExcludesTestIdentities,
	"seed.go:ReconcileSuperAdminBindings:ListRoleBindingsForPrincipal":                           censusExcludesTestIdentities,
	"access_constraint_governance.go:checkConstraintAdminLockout:ListRoleBindingsForScope":       "guard: counts survivors among resolveConstraintAdmins' admin set, which counts only positively human rows (IsHumanKind); a scope binding keeps an admin only when active (isBindingActive) at the check or activation time and it names that admin or one of its groups",
	"access_constraint_governance.go:checkUserRemovalAdminLockout:GetEffectiveGroups":            "guard: scopes to lockout-check for a user removal; an extra scope only adds a check, never admits",
	"access_constraint_governance.go:checkUserRemovalAdminLockout:ListRoleBindingsForPrincipals": "guard: scopes to lockout-check for a user removal (clamped, unwired: admin kinds never constraint admins); an extra, even expired, binding only adds a check, never admits",

	// Narrowing only: access-constraint restriction and preview resolution
	// can only remove authority, never grant it.
	"access_constraint_capabilities.go:computeRestrictions:GetEffectiveGroups":               "restriction: finds constraints that narrow the principal",
	"access_constraint_preview.go:resolveAllPrincipals:GetEffectiveGroups":                   "preview: selects principals a constraint would narrow",
	"access_constraint_preview.go:resolveGroupClosure:GetEffectiveGroups":                    "preview: selects principals a constraint would narrow",
	"access_constraint_preview.go:resolveGroupClosure:GetGroupMembers":                       "preview: selects principals a constraint would narrow",
	"access_constraint_preview.go:resolveSinglePrincipal:GetEffectiveGroups":                 "preview: selects principals a constraint would narrow",
	"handlers_access_constraints.go:buildConstraintPreview:GetGroupMembers":                  "preview: selects principals a constraint would narrow",
	"membership_loss.go:transitiveGroupMemberUsers:GetGroupMembers":                          "revocation: membership-loss processing only removes access",
	"handlers_agents_core.go:addAgentCreateIfAnyProjectAllows:GetEffectiveGroups":            "advisory: UI create-button hint; the create path decides",
	"handlers_agents_core.go:addAgentCreateIfAnyProjectAllows:ListRoleBindingsForPrincipals": "advisory: UI create-button hint; the create path decides",

	// Narrowing and governance reads outside the authorization service.
	"access_constraint_capabilities.go:computeRestrictions:GetEffectiveGroupsForAgent": "restriction: finds constraints that narrow the principal",
	"access_constraint_preview.go:resolveAllPrincipals:GetEffectiveGroupsForAgent":     "preview: selects principals a constraint would narrow",
	"access_constraint_preview.go:resolveAllPrincipals:ListProjectMembers":             "preview: selects principals a constraint would narrow",
	"access_constraint_preview.go:resolveGroupClosure:GetEffectiveGroupsForAgent":      "preview: selects principals a constraint would narrow",
	"access_constraint_preview.go:resolveSinglePrincipal:GetEffectiveGroupsForAgent":   "preview: selects principals a constraint would narrow",
	"handlers_agent_message_mode.go:handleSetMessageMode:GetProjectMembership":         "narrowing: denies project admins (D7); can only refuse, never grant",
	"quota.go:resolveEffectiveLimitWithSource:GetUserGroups":                           "restriction: quota limits by group",
	"membership_loss.go:membershipExpiryScan:ListAllRoleBindings":                      "revocation: finds expired bindings to process membership loss",
	"handlers_groups.go:authorizeGroupMemberGrant:GetGroupMembership":                  "group governance: the caller's group role (not a role binding); the grant itself goes through CanDelegate with the grantee",
	"handlers_groups.go:removeGroupMember:GetGroupMembership":                          "group governance: the caller's group role for a removal",
	"handlers_groups.go:removeGroupMember:CountGroupMembersByRole":                     "guard: last group owner on removal",
	"handlers_groups.go:groupClosureAuthority:GetParentGroups":                         "group: the bindings a group closure carries, read to confirm a token's delegation check in the membership transaction; not a user's authority",
	"chat_v2_no_recipient.go:projectHumanMembersStrict:ListProjectMembers":             "routing: chat recipients of a project; grants nothing",
	"handlers_chat_v2.go:resolveProjectHumanMembers:ListProjectMembers":                "routing: chat recipients of a project; grants nothing",

	// Display: returns stored records to an authorized caller; grants nothing.
	"handlers_admin_effective_access.go:handleAdminEffectiveAccess:GetEffectiveGroups":            "display: effective-access report (bindings read through the clamped authz store)",
	"handlers_admin_effective_access.go:handleAdminEffectiveAccess:ListRoleBindingsForPrincipals": "display: effective-access report, read through the authorization service's clamped store",
	"handlers_groups.go:listGroupMembers:GetGroupMembers":                                         "display: group member listing",
	"handlers_groups.go:getGroupMember:GetGroupMembership":                                        "display: one group membership",
	"handlers_chat_v2.go:handleSpaceMembers:ListProjectMembers":                                   "display: chat space member listing",
	"handlers_principals.go:handleAgentGroups:GetEffectiveGroupsForAgent":                         "display: an agent's groups",
	"handlers_principals.go:handlePrincipalRoutes:GetEffectiveGroupsForAgent":                     "display: a principal's groups",
	"handlers_principals.go:handlePrincipalRoutes:GetUserGroups":                                  "display: a principal's groups",
	"handlers_roles.go:listRoleBindings:CountAllRoleBindings":                                     "display: admin role-binding listing",
	"handlers_roles.go:listRoleBindings:ListAllRoleBindings":                                      "display: admin role-binding listing",
	"handlers_roles.go:listRoleBindingsForPrincipal:GetEffectiveGroupsForAgent":                   "display: stored bindings of a principal",
	"handlers_admin_effective_access.go:handleAdminEffectiveAccess:GetEffectiveGroupsForAgent":    "display: effective-access report",
	"perftrace_store.go:GetEffectiveGroupsForAgent:GetEffectiveGroupsForAgent":                    "wrapper: perf-trace decorator inside the clamp",
	"perftrace_store.go:GetGroupMembership:GetGroupMembership":                                    "wrapper: perf-trace decorator inside the clamp",
	"perftrace_store.go:GetParentGroups:GetParentGroups":                                          "wrapper: perf-trace decorator inside the clamp",
	"perftrace_store.go:GetProjectMembership:GetProjectMembership":                                "wrapper: perf-trace decorator inside the clamp",
	"perftrace_store.go:GetUserGroups:GetUserGroups":                                              "wrapper: perf-trace decorator inside the clamp",
	"handlers_principals.go:handleMyGroups:GetEffectiveGroups":                                    "display: the caller's groups",
	"handlers_principals.go:handlePrincipalRoutes:GetEffectiveGroups":                             "display: a principal's groups",
	"handlers_project_members.go:listProjectMembers:ListRoleBindingsForScope":                     "display: project member listing",
	"handlers_roles.go:listBindingsForUser:ListRoleBindingsForPrincipal":                          "display: stored bindings of a user",
	"handlers_roles.go:listRoleBindingsForPrincipal:GetEffectiveGroups":                           "display: stored bindings of a principal",
	"handlers_roles.go:listRoleBindingsForPrincipal:ListRoleBindingsForPrincipal":                 "display: stored bindings of a principal",
	"handlers_roles.go:listRoleBindingsForPrincipal:ListRoleBindingsForPrincipals":                "display: stored bindings of a principal",
	"handlers_project_visitors.go:handleListProjectVisitors:ListVisitorRoleBindings":              "display: project visitor grant listing for a holder of project.manage_visitors; federated accounts only",

	// Mutation inputs: the bindings being replaced, deleted or migrated.
	"access_constraint_governance.go:ReplaceRoleBinding:GetRoleBinding":                              "mutation: the binding being replaced",
	"handlers_auth.go:deleteSuperAdminRoleBinding:ListRoleBindingsForPrincipal":                      "mutation: bindings being deleted",
	"handlers_roles.go:deleteRoleBinding:GetRoleBinding":                                             "mutation: the binding being deleted",
	"handlers_roles.go:authorizeRoleBindingDelete:GetRoleBinding":                                    "routing: the binding's scope selects the project check; the authorization service decides",
	"handlers_users_core.go:deleteSuperAdminBindingTx:ListRoleBindingsForPrincipal":                  "mutation: bindings being deleted",
	"handlers_users_core.go:guardAndCascadeUserRoleBindingsTx:GetRoleBinding":                        "mutation: user-delete binding cascade",
	"handlers_users_core.go:guardAndCascadeUserRoleBindingsTx:ListRoleBindingsForPrincipal":          "mutation: user-delete binding cascade",
	"handlers_users_core.go:superAdminBindingStateForUser:ListRoleBindingsForPrincipal":              "mutation: role-change target's own binding state; test identity targets are refused first",
	"project_membership_service.go:RemoveMember:GetRoleBinding":                                      "mutation: the binding being removed",
	"project_membership_service.go:UpdateMemberRole:GetRoleBinding":                                  "mutation: the binding being changed",
	"project_membership_service.go:findExistingDirectBindingsFromStore:ListRoleBindingsForPrincipal": "mutation: the target's existing bindings to replace",
	"project_membership_set.go:loadProjectPrincipalBindings:ListRoleBindingsForPrincipal":            "mutation: the target's existing bindings to plan against",
	"handlers_project_visitors.go:projectVisitorBinding:ListVisitorRoleBindings":                     "mutation: the visitor grant being changed or revoked; federated accounts only",
	"federation_visitor.go:missingVisitorRules:ListRoleBindingsForPrincipals":                        "mutation: the linked federated account's own project bindings, to skip a rule's project where it already has a visitor binding or to find the inactive visitor grant by email the rule binding replaces; federated accounts only",
	"project_fixture_purge.go:purgeFixtureProject:ListRoleBindingsForScope":                          "mutation: test identity purge, the stamped project's bindings to delete; any other owner keeps the project (project scope)",
	"project_owner_predicates.go:projectOwnerBindingCount:ListRoleBindingsForScope":                  "guard: last-project-owner count (project scope; project bindings are not clamped for any kind)",
	"project_owner_predicates.go:usableOwnerPrincipals:ListRoleBindingsForScope":                     "guard: last-project-owner count (project scope; project bindings are not clamped for any kind)",
	"handlers_groups.go:groupClosureAuthority:ListRoleBindingsForPrincipals":                         "group: the bindings a group closure carries, read to confirm a token's delegation check in the membership transaction; not a user's authority",

	// Visitor admission (federated accounts only). Every caller passes the
	// ID of a *VisitorIdentity, and resolveFederatedVisitor builds one only
	// for a kind federated account (federation_visitor.go:98, before the one
	// NewVisitorIdentity call at :118). A user's kind never changes and
	// federated is no test identity kind, so the clamps would remove nothing.
	"federation_visitor_agents.go:visitorGrantBindings:ListRoleBindingsForPrincipal": "visitor admission: the caller's own project-visitor grants (project scope only) for the visitor agent list and read routes and its own grant list; reached only with a *VisitorIdentity, built only for a kind federated account (federation_visitor.go:98), never a test identity; federated accounts only",

	// Sign-in (every sign-in path refuses test identities).
	"handlers_auth.go:hasUIPromotedBinding:ListRoleBindingsForPrincipal": "sign-in: admin_emails demotion protection; test identities never sign in",

	// Startup maintenance.
	"project_membership_service.go:MigrateMultiRoleBindings:ListRoleBindingsForScope": "startup: binding migration",
	"seed.go:CleanupRedundantHubMemberBindings:GetGroupMembers":                       "startup: redundant hub-member binding cleanup",
	"seed.go:CleanupRedundantHubMemberBindings:ListRoleBindingsForPrincipal":          "startup: redundant hub-member binding cleanup",
	"seed.go:hubViewerBindingsForUser:ListRoleBindingsForPrincipal":                   "mutation: hub grant sync",
	"seed.go:projectHasOwnerBinding:ListRoleBindingsForScope":                         "startup: legacy owner backfill",
	"seed.go:removeProjectMembersGroupRoleBindings:ListRoleBindingsForPrincipal":      "startup: members-group binding cleanup",
	"seed.go:warnOwnerOnlyLegacyProject:ListRoleBindingsForScope":                     "startup: legacy owner warning",
}

// testAdminAdminEmailsCensus classifies every function that reads the
// admin_emails setting (selector AdminEmails), keyed "file:function".
var testAdminAdminEmailsCensus = map[string]string{
	"admin_settings_db.go:accessNoRowBase":                "config: settings plumbing",
	"admin_settings_db.go:applySnapshotToResponse":        "config: settings plumbing",
	"admin_settings_db.go:extractKoanfKeysFromRequest":    "config: settings plumbing",
	"admin_settings_db.go:overlayAccessRequest":           "config: settings plumbing",
	"handlers_auth.go:getUserRole":                        "sign-in: role at sign-in; test identities never sign in",
	"handlers_auth.go:signInDenial":                       "sign-in: test identities never sign in",
	"handlers_auth.go:signInPolicyDeps":                   "sign-in: test identities never sign in",
	"handlers_users_provision.go:provisionWarnings":       "config: provisioning warning",
	"operational_settings.go:ApplySnapshot":               "config: settings plumbing",
	"operational_settings.go:BuildLayer1SnapshotFromFile": "config: settings plumbing",
	"operational_settings.go:buildSnapshotFromKoanf":      "config: settings plumbing",
	"server.go:AdminEmails":                               "config: accessor; ReconcileSuperAdminBindings skips test identities",
	"server.go:New":                                       "config: passed to ReconcileSuperAdminBindings, which skips test identities",
	"web.go:adminEmails":                                  "sign-in: web sign-in; test identities never sign in",
}

// The legacy admin fast path itself (IsUnscopedLocalPlatformAdmin) lives in
// pkg/hub/internal/principal since ptone/scion#3316. The census scans that
// directory too (guardScanSubdirs), so its role check is classified below;
// TestBypassCensus tracks its pkg/hub callers.
//
// testAdminRoleAdminCensus classifies every function that compares a
// user's role (stored or claim) with admin, keyed "file:function". A
// test_hub_admin's stored role is member and the JWT row block refuses a
// token whose role claim is not its kind's role, so no role gate admits
// it. A test_super_admin holds the admin role: every "role gate" entry here
// is classified allow or deny for it in testAdminRoleGatePolicy (admin tier
// A2, TestTestAdminPolicy_RoleGateCensus).
var testAdminRoleAdminCensus = map[string]string{
	"admin_delegation_adoption.go:recheckDelegationAdoptionAdmin":         "role gate: admin only",
	"admin_mode.go:adminModeWebMiddleware":                                "role gate: admin only",
	"gcp_iam_settings.go:gcpIAMHubAdmin":                                  "role gate: admin only",
	"handlers_env_secrets.go:authorizeHubEnvVarList":                      "role gate: admin only",
	"handlers_env_secrets.go:resolveEnvSecretAccess":                      "role gate: admin only",
	"web.go:authorizeSSESubjects":                                         "role gate: admin only",
	"federation_auth.go:extractUserClaims":                                "federation: a federated identity is never admin (coerced to viewer); federation is off while the tier is on",
	"handlers_test_admin_identities.go:canIssueTestAdmin":                 "issuer check: the issuer's stored role, read from its row",
	"handlers_test_admin_identities.go:recheckTestAdminDelegationGrantor": "issuer check: a workload delegation grantor's stored role, read from its row through the issuance transaction (X4); its kind must be human first",
	"handlers_auth.go:determineUserRole":                                  "sign-in: test identities never sign in",
	"handlers_auth.go:getUserRole":                                        "sign-in: test identities never sign in",
	"handlers_auth.go:handleAuthRefresh":                                  "sign-in: test identities get no refresh token",
	"handlers_auth.go:provisionUser":                                      "sign-in: test identities never sign in",
	"handlers_auth.go:signInPolicyDeps":                                   "sign-in: test identities never sign in",
	"sign_in_policy.go:applyLiveSignInPolicy":                             "sign-in: test identities never sign in",
	"web.go:handleOAuthCallback":                                          "sign-in: test identities never sign in",
	"web.go:proxyAuthMiddleware":                                          "sign-in: test identities never sign in",
	"handlers_users_core.go:checkUserPromotePermission":                   "mutation: requested new role; test identity targets are refused first",
	"handlers_users_core.go:executeRoleTransition":                        "mutation: requested new role; test identity targets are refused",
	"handlers_users_core.go:updateUser":                                   "mutation: requested new role; test identity targets are refused",
	"seed.go:ReconcileSuperAdminBindings":                                 censusExcludesTestIdentities,
	"seed.go:backfillUserRoleBindings":                                    "startup: a super-admin backfill on an admin-kind row is refused by the store (system bindings on admin kinds are issuance-only)",
	"internal/principal/identity.go:IsUnscopedLocalPlatformAdmin":         "role gate: admin only (the legacy unscoped admin fast path); a test_super_admin passes it, so each caller is classified as its own gate in testAdminRoleGatePolicy",
}

// censusFuncSite is one parsed function of pkg/hub.
type censusFuncSite struct {
	refs  map[string]bool // identifiers referenced in the body
	calls map[string]int  // calls in the body, by called name
	// bindingReads counts the unclamped role-binding reads in the body.
	bindingReads int
}

// clampCalls is the number of test identity clamp calls in the function.
func (c *censusFuncSite) clampCalls() int {
	return c.calls["clampBindingsForUser"] + c.calls["clampTestIdentityBindings"]
}

// activationFiltered reports whether the function applies the activation
// filter.
func (c *censusFuncSite) activationFiltered() bool {
	return c.refs["isBindingActive"] || c.refs["bindingActivationOK"] || c.refs["evaluateActivation"]
}

// identCalls counts the calls in fn by called name (identifier or
// selector name).
func identCalls(fn *ast.FuncDecl) map[string]int {
	calls := map[string]int{}
	ast.Inspect(fn.Body, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		switch f := call.Fun.(type) {
		case *ast.Ident:
			calls[f.Name]++
		case *ast.SelectorExpr:
			calls[f.Sel.Name]++
		}
		return true
	})
	return calls
}

// walkPkgHubFuncs calls visit for every function declaration in the
// non-test files of pkg/hub and of its existing guardScanSubdirs (files there
// are named by their relative path); other subdirectories are not walked.
func walkPkgHubFuncs(t *testing.T, visit func(file string, fn *ast.FuncDecl, fset *token.FileSet)) {
	t.Helper()
	fset := token.NewFileSet()
	// pkgmove:scan-covers pkg/hub/apierr
	// scanned through guardScanSubdirs; guards test-admin authority census sites.
	// pkgmove:scan-covers pkg/hub/internal/principal
	// scanned through guardScanSubdirs; guards test-admin authority census sites.
	// pkgmove:scan-covers pkg/hub/maintenance
	// scanned through guardScanSubdirs once it exists; guards test-admin authority census sites.
	entries, err := readGuardScanDirs(".")
	require.NoError(t, err)
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, filepath.Join(".", name), nil, 0)
		require.NoError(t, err)
		for _, decl := range f.Decls {
			if fn, ok := decl.(*ast.FuncDecl); ok && fn.Body != nil {
				visit(name, fn, fset)
			}
		}
	}
}

// identRefs returns the identifiers (including selector names) fn uses.
func identRefs(fn *ast.FuncDecl) map[string]bool {
	refs := map[string]bool{}
	ast.Inspect(fn.Body, func(n ast.Node) bool {
		switch x := n.(type) {
		case *ast.Ident:
			refs[x.Name] = true
		case *ast.SelectorExpr:
			refs[x.Sel.Name] = true
		}
		return true
	})
	return refs
}

// isAuthzServiceStoreRead reports whether a call's receiver is the
// authorization service's clamped store: a.store inside an AuthzService
// method, or h.a.store inside a principalInputs method.
func isAuthzServiceStoreRead(fn *ast.FuncDecl, recv string) bool {
	if fn.Recv == nil || len(fn.Recv.List) == 0 {
		return false
	}
	recvType := types.ExprString(fn.Recv.List[0].Type)
	switch {
	case recvType == "*AuthzService" && recv == "a.store":
		return true
	case recvType == "*principalInputs" && recv == "h.a.store":
		return true
	}
	return false
}

func checkCensus(t *testing.T, kind string, seen map[string]*censusFuncSite, census map[string]string) {
	t.Helper()
	keys := make([]string, 0, len(seen))
	for k := range seen {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, key := range keys {
		class, ok := census[key]
		if !assert.True(t, ok, "%s: unclassified site %s: classify it, and apply the test identity clamps if it decides authority", kind, key) {
			continue
		}
		assert.NotEmpty(t, strings.TrimSpace(class), "%s: %s has an empty classification", kind, key)
		if want, verified := censusVerifiers[class]; verified {
			found := false
			for _, id := range want {
				if seen[key].refs[id] {
					found = true
				}
			}
			assert.True(t, found, "%s: %s is classified %q but its function references none of %v", kind, key, class, want)
		}
		if class == censusClamped && isRoleBindingRead(key[strings.LastIndex(key, ":")+1:]) {
			// Occurrence level: one clamp call per role-binding read in
			// the function, and the activation filter (DEC-4 C-b).
			site := seen[key]
			assert.GreaterOrEqual(t, site.clampCalls(), site.bindingReads,
				"%s: %s has %d role-binding reads but %d clamp calls", kind, key, site.bindingReads, site.clampCalls())
			assert.True(t, site.activationFiltered(), "%s: %s reads role bindings without an activation filter", kind, key)
		}
	}
	for key := range census {
		_, ok := seen[key]
		assert.True(t, ok, "%s: stale classification %s: no such site", kind, key)
	}
}

// TestTestAdminAuthorityReadCensus: every pkg/hub caller of an
// authority read outside the authorization service's clamped store is
// classified; decision sites apply the clamps; guards exclude test
// identities; no entry is stale. The same holds for admin_emails reads and
// role==admin comparisons.
func TestTestAdminAuthorityReadCensus(t *testing.T) {
	methods := censusAuthorityReadMethods(t)
	reads := map[string]*censusFuncSite{}
	adminEmails := map[string]*censusFuncSite{}
	roleAdmin := map[string]*censusFuncSite{}
	clampedReads := 0
	walkPkgHubFuncs(t, func(file string, fn *ast.FuncDecl, fset *token.FileSet) {
		var shared *censusFuncSite
		site := func() *censusFuncSite {
			if shared == nil {
				shared = &censusFuncSite{refs: identRefs(fn), calls: identCalls(fn)}
			}
			return shared
		}
		ast.Inspect(fn.Body, func(n ast.Node) bool {
			switch x := n.(type) {
			case *ast.SelectorExpr:
				name := x.Sel.Name
				if methods[name] {
					recv := types.ExprString(x.X)
					// Only the methods the clamped store wraps pass
					// through it; other reads through a.store are
					// unclamped and need a classification.
					if censusClampedStoreMethods[name] && isAuthzServiceStoreRead(fn, recv) {
						clampedReads++
						return true
					}
					fs := site()
					if isRoleBindingRead(name) {
						fs.bindingReads++
					}
					reads[file+":"+fn.Name.Name+":"+name] = fs
				}
				if name == "AdminEmails" {
					adminEmails[file+":"+fn.Name.Name] = site()
				}
			case *ast.BinaryExpr:
				if x.Op != token.EQL && x.Op != token.NEQ {
					return true
				}
				if isRoleExpr(x.X) && isAdminRoleLiteral(x.Y) || isRoleExpr(x.Y) && isAdminRoleLiteral(x.X) {
					roleAdmin[file+":"+fn.Name.Name] = site()
				}
			}
			return true
		})
	})
	assert.Greater(t, clampedReads, 10, "the authorization service's own reads were found")
	checkCensus(t, "authority read", reads, testAdminAuthorityReadCensus)
	checkCensus(t, "admin_emails", adminEmails, testAdminAdminEmailsCensus)
	checkCensus(t, "role==admin", roleAdmin, testAdminRoleAdminCensus)
}

// isRoleExpr matches x.Role, x.Role(), and the role variables role
// comparisons use (oldRole, newRole, currentRole, storedRole).
func isRoleExpr(e ast.Expr) bool {
	switch x := e.(type) {
	case *ast.SelectorExpr:
		return x.Sel.Name == "Role"
	case *ast.CallExpr:
		if sel, ok := x.Fun.(*ast.SelectorExpr); ok {
			return sel.Sel.Name == "Role"
		}
	case *ast.StarExpr:
		return isRoleExpr(x.X)
	case *ast.Ident:
		return strings.HasSuffix(x.Name, "Role") || x.Name == "role"
	}
	return false
}

// isAdminRoleLiteral matches "admin" and store.UserRoleAdmin.
func isAdminRoleLiteral(e ast.Expr) bool {
	switch x := e.(type) {
	case *ast.BasicLit:
		return x.Value == `"admin"`
	case *ast.SelectorExpr:
		return x.Sel.Name == "UserRoleAdmin"
	}
	return false
}

// testAdminActivationCensus classifies the authorization service's role
// binding loaders that hand their bindings to a caller that filters them,
// keyed "function". Every other authorization-service function that reads
// role bindings must itself apply the activation filter.
var testAdminActivationCensus = map[string]string{
	"Bindings":                   "loader: decide's kernel evaluates activation per candidate (evaluateActivation)",
	"projectMembershipInputs":    "loader: ProjectMembershipEvidence filters with bindingActivationOK",
	"projectScopedGrants":        "loader: unionScopedRoleBindingPermissions filters with bindingActivationOK",
	"canDelegateGroupMembership": "delegation: the group's bindings are what the actor must already hold; an inactive one only makes the check stricter",
	"grantPermissionSet":         "delegation: permissions a test admin's group grant would confer; inactive bindings are counted on purpose (one becoming active later must not slip past the test-admin policy check), so it only refuses more",
}

// TestTestAdminDecidePathsApplyActivation (DEC-4 C-b): every binding read
// of the authorization service (role-binding family, through its clamped
// store) is in a function that applies the activation filter
// (isBindingActive, bindingActivationOK or evaluateActivation), or is a
// classified loader whose caller does. Together with the clamp, which
// every such read passes through, no decide path evaluates a binding
// without the activation filter.
func TestTestAdminDecidePathsApplyActivation(t *testing.T) {
	methods := censusAuthorityReadMethods(t)
	seen := map[string]bool{}
	walkPkgHubFuncs(t, func(file string, fn *ast.FuncDecl, fset *token.FileSet) {
		refs := identRefs(fn)
		ast.Inspect(fn.Body, func(n ast.Node) bool {
			sel, ok := n.(*ast.SelectorExpr)
			if !ok || !methods[sel.Sel.Name] || !strings.Contains(sel.Sel.Name, "RoleBinding") {
				return true
			}
			if !isAuthzServiceStoreRead(fn, types.ExprString(sel.X)) {
				return true
			}
			name := fn.Name.Name
			if _, classified := testAdminActivationCensus[name]; classified {
				seen[name] = true
				return true
			}
			assert.True(t, refs["isBindingActive"] || refs["bindingActivationOK"] || refs["evaluateActivation"],
				"%s: %s reads role bindings without an activation filter", fset.Position(sel.Pos()), name)
			return true
		})
	})
	for name := range testAdminActivationCensus {
		assert.True(t, seen[name], "stale activation classification %s", name)
	}
}
