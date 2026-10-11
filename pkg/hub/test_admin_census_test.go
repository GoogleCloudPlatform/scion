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
	"os"
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
// file of pkg/hub with go/ast, like TestBypassCensus and
// TestSigningKeySource_EveryKeySourceClassified: every site that reads
// authority outside the authorization service's clamped store must be
// classified, decision sites must apply the test identity clamps, and no
// classification may go stale.

// Census classes.
const (
	// censusClamped: the function applies the test identity clamps to the
	// bindings it reads (clampBindingsForUser, clampTestIdentityBindings or
	// clampTestFixtureBindings). Verified.
	censusClamped = "decision: clamped"
	// censusExcludesTestIdentities: a holder-counting guard that leaves
	// every test identity kind out (IsTestIdentity, isTestFixtureUser or
	// reconcileSkipsUser). Verified.
	censusExcludesTestIdentities = "guard: excludes test identities"
)

// censusVerifiers maps the verified classes to the identifiers the
// function must reference.
var censusVerifiers = map[string][]string{
	censusClamped:                {"clampBindingsForUser", "clampTestIdentityBindings", "clampTestFixtureBindings"},
	censusExcludesTestIdentities: {"IsTestIdentity", "isTestFixtureUser", "reconcileSkipsUser"},
}

// censusAuthorityReadMethods returns the authority-read methods the census
// tracks, anchored on the store.Store method set: every role-binding read
// (List*/Get*RoleBinding*), GetEffectiveGroups and GetGroupMembers. A new
// store method in the role-binding read family enters the census by
// itself.
func censusAuthorityReadMethods(t *testing.T) map[string]bool {
	t.Helper()
	st := reflect.TypeOf((*store.Store)(nil)).Elem()
	family := regexp.MustCompile(`^(List|Get)RoleBinding`)
	out := map[string]bool{}
	for i := 0; i < st.NumMethod(); i++ {
		if name := st.Method(i).Name; family.MatchString(name) {
			out[name] = true
		}
	}
	for _, name := range []string{"GetEffectiveGroups", "GetGroupMembers"} {
		_, ok := st.MethodByName(name)
		require.True(t, ok, "store.Store has no method %s", name)
		out[name] = true
	}
	require.True(t, out["ListRoleBindingsForPrincipals"] && out["ListRoleBindingsForScope"] && out["GetRoleBinding"],
		"the census must anchor on the role-binding read methods")
	return out
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
	"authorize_message.go:CheckEffectiveMembership:GetEffectiveGroups":                              censusClamped,
	"authorize_message.go:CheckEffectiveMembership:ListRoleBindingsForPrincipal":                    censusClamped,
	"authorize_message.go:CheckEffectiveMembership:ListRoleBindingsForPrincipals":                   censusClamped,
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

	// Holder-counting guards: test identities never count.
	"access_constraint_preview.go:resolveAdminUsers:GetEffectiveGroups":                       censusExcludesTestIdentities,
	"access_constraint_preview.go:resolveAdminUsers:GetGroupMembers":                          censusExcludesTestIdentities,
	"access_constraint_preview.go:resolveAdminUsers:ListRoleBindingsForScope":                 censusExcludesTestIdentities,
	"handlers_users_core.go:checkLastSuperAdminTx:ListRoleBindingsForScope":                   censusExcludesTestIdentities,
	"seed.go:ReconcileSuperAdminBindings:ListRoleBindingsForPrincipal":                        censusExcludesTestIdentities,
	"access_constraint_governance.go:enforceRoleBindingLockout:ListRoleBindingsForPrincipals": "guard: counts survivors among resolveAdminUsers' admin set, which excludes test identities (isTestFixtureUser)",

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

	// Display: returns stored records to an authorized caller; grants nothing.
	"handlers_admin_effective_access.go:handleAdminEffectiveAccess:GetEffectiveGroups":            "display: effective-access report (bindings read through the clamped authz store)",
	"handlers_admin_effective_access.go:handleAdminEffectiveAccess:ListRoleBindingsForPrincipals": "display: effective-access report, read through the authorization service's clamped store",
	"handlers_groups.go:listGroupMembers:GetGroupMembers":                                         "display: group member listing",
	"handlers_principals.go:handleMyGroups:GetEffectiveGroups":                                    "display: the caller's groups",
	"handlers_principals.go:handlePrincipalRoutes:GetEffectiveGroups":                             "display: a principal's groups",
	"handlers_project_members.go:listProjectMembers:ListRoleBindingsForScope":                     "display: project member listing",
	"handlers_roles.go:listBindingsForUser:ListRoleBindingsForPrincipal":                          "display: stored bindings of a user",
	"handlers_roles.go:listRoleBindingsForPrincipal:GetEffectiveGroups":                           "display: stored bindings of a principal",
	"handlers_roles.go:listRoleBindingsForPrincipal:ListRoleBindingsForPrincipal":                 "display: stored bindings of a principal",
	"handlers_roles.go:listRoleBindingsForPrincipal:ListRoleBindingsForPrincipals":                "display: stored bindings of a principal",

	// Mutation inputs: the bindings being replaced, deleted or migrated.
	"access_constraint_governance.go:ReplaceRoleBinding:GetRoleBinding":                              "mutation: the binding being replaced",
	"handlers_auth.go:deleteSuperAdminRoleBinding:ListRoleBindingsForPrincipal":                      "mutation: bindings being deleted",
	"handlers_roles.go:deleteRoleBinding:GetRoleBinding":                                             "mutation: the binding being deleted",
	"handlers_users_core.go:deleteSuperAdminBindingTx:ListRoleBindingsForPrincipal":                  "mutation: bindings being deleted",
	"handlers_users_core.go:guardAndCascadeUserRoleBindingsTx:GetRoleBinding":                        "mutation: user-delete binding cascade",
	"handlers_users_core.go:guardAndCascadeUserRoleBindingsTx:ListRoleBindingsForPrincipal":          "mutation: user-delete binding cascade",
	"handlers_users_core.go:superAdminBindingStateForUser:ListRoleBindingsForPrincipal":              "mutation: role-change target's own binding state; test identity targets are refused first",
	"project_membership_service.go:RemoveMember:GetRoleBinding":                                      "mutation: the binding being removed",
	"project_membership_service.go:UpdateMemberRole:GetRoleBinding":                                  "mutation: the binding being changed",
	"project_membership_service.go:findExistingDirectBindingsFromStore:ListRoleBindingsForPrincipal": "mutation: the target's existing bindings to replace",
	"project_membership_set.go:loadProjectPrincipalBindings:ListRoleBindingsForPrincipal":            "mutation: the target's existing bindings to plan against",
	"project_owner_predicates.go:projectOwnerBindingCount:ListRoleBindingsForScope":                  "guard: last-project-owner count (project scope; project bindings are not clamped for any kind)",
	"project_owner_predicates.go:usableOwnerPrincipals:ListRoleBindingsForScope":                     "guard: last-project-owner count (project scope; project bindings are not clamped for any kind)",
	"handlers_groups.go:groupClosureHasRoleBinding:ListRoleBindingsForPrincipals":                    "group: whether a group carries bindings (session requirement), not a user's authority",

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

// testAdminRoleAdminCensus classifies every function that compares a
// user's role (stored or claim) with admin, keyed "file:function". A
// test_hub_admin's stored role is member and the JWT row block refuses a
// token whose role claim is not its kind's role, so no role gate admits
// it; the test_super_admin kind is refused until admin tier A2 classifies
// these gates.
var testAdminRoleAdminCensus = map[string]string{
	"admin_delegation_adoption.go:recheckDelegationAdoptionAdmin": "role gate: admin only",
	"admin_mode.go:adminModeWebMiddleware":                        "role gate: admin only",
	"gcp_iam_settings.go:gcpIAMHubAdmin":                          "role gate: admin only",
	"handlers_env_secrets.go:authorizeHubEnvVarList":              "role gate: admin only",
	"handlers_env_secrets.go:resolveEnvSecretAccess":              "role gate: admin only",
	"identity.go:IsUnscopedLocalPlatformAdmin":                    "role gate: the legacy admin fast path (claim role admin)",
	"web.go:authorizeSSESubjects":                                 "role gate: admin only",
	"federation_auth.go:extractUserClaims":                        "federation: a federated identity is never admin (coerced to viewer); federation is off while the tier is on",
	"handlers_test_admin_identities.go:canIssueTestAdmin":         "issuer check: the issuer's stored role, read from its row",
	"handlers_auth.go:determineUserRole":                          "sign-in: test identities never sign in",
	"handlers_auth.go:getUserRole":                                "sign-in: test identities never sign in",
	"handlers_auth.go:handleAuthRefresh":                          "sign-in: test identities get no refresh token",
	"handlers_auth.go:provisionUser":                              "sign-in: test identities never sign in",
	"handlers_auth.go:signInPolicyDeps":                           "sign-in: test identities never sign in",
	"sign_in_policy.go:applyLiveSignInPolicy":                     "sign-in: test identities never sign in",
	"web.go:handleOAuthCallback":                                  "sign-in: test identities never sign in",
	"web.go:proxyAuthMiddleware":                                  "sign-in: test identities never sign in",
	"handlers_users_core.go:checkUserPromotePermission":           "mutation: requested new role; test identity targets are refused first",
	"handlers_users_core.go:executeRoleTransition":                "mutation: requested new role; test identity targets are refused",
	"handlers_users_core.go:updateUser":                           "mutation: requested new role; test identity targets are refused",
	"seed.go:ReconcileSuperAdminBindings":                         censusExcludesTestIdentities,
	"seed.go:backfillUserRoleBindings":                            "startup: a super-admin backfill on an admin-kind row is refused by the store (system bindings on admin kinds are issuance-only)",
}

// censusFuncSite is one parsed function of pkg/hub.
type censusFuncSite struct {
	refs map[string]bool // identifiers referenced in the body
}

// walkPkgHubFuncs calls visit for every function declaration in the
// non-test files of pkg/hub (this directory only).
func walkPkgHubFuncs(t *testing.T, visit func(file string, fn *ast.FuncDecl, fset *token.FileSet)) {
	t.Helper()
	fset := token.NewFileSet()
	entries, err := os.ReadDir(".")
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
		var refs map[string]bool
		site := func() *censusFuncSite {
			if refs == nil {
				refs = identRefs(fn)
			}
			return &censusFuncSite{refs: refs}
		}
		ast.Inspect(fn.Body, func(n ast.Node) bool {
			switch x := n.(type) {
			case *ast.SelectorExpr:
				name := x.Sel.Name
				if methods[name] {
					recv := types.ExprString(x.X)
					if isAuthzServiceStoreRead(fn, recv) {
						clampedReads++
						return true
					}
					reads[file+":"+fn.Name.Name+":"+name] = site()
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
