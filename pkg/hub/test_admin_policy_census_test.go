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
	"bytes"
	"errors"
	"go/ast"
	"go/token"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/config/opsettings"
	"github.com/GoogleCloudPlatform/scion/pkg/experiments"
	"github.com/GoogleCloudPlatform/scion/pkg/hub/permissions"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Admin tier A2 (ptone/scion#4240): the TestAdminPolicy classification is
// complete, an unclassified or stale entry fails, the initial deny set is
// pinned, and every legacy admin route and role-claim admin gate in pkg/hub
// is classified (go/ast census, like TestBypassCensus and the A1 census).
// No database: these tests compile in every build.

// Every permission, legacy admin route, settings section and key,
// file-live key and experiment of this binary is classified, and nothing
// classified is stale. TestAdminPolicyComplete (the G3 startup check)
// agrees.
func TestTestAdminPolicy_Complete(t *testing.T) {
	in := currentTestAdminPolicyInputs()
	assert.Empty(t, testAdminPolicyProblems(in))
	require.NoError(t, TestAdminPolicyComplete())

	// The inputs are read from the live vocabularies, not from the tables.
	assert.Len(t, in.Permissions, len(permissions.Registry))
	assert.Len(t, in.SettingsSections, len(opsettings.Registry))
	assert.Len(t, in.Experiments, len(experiments.Default().All()))
	assert.Len(t, in.FileLiveKeys, len(fileLiveKeys))
	assert.NotEmpty(t, in.LegacyRoutes)
	for _, id := range legacyAdminRouteIDs() {
		assert.Contains(t, in.LegacyRoutes, id)
	}
}

// A fake unclassified entry in each vocabulary is reported, and so is an
// entry classified but no longer present (stale) and an invalid value.
func TestTestAdminPolicy_FakeUnclassifiedEntryFails(t *testing.T) {
	base := currentTestAdminPolicyInputs()
	clone := func() testAdminPolicyInputs {
		c := base
		c.Permissions = append([]string(nil), base.Permissions...)
		c.LegacyRoutes = append([]string(nil), base.LegacyRoutes...)
		c.SettingsSections = append([]string(nil), base.SettingsSections...)
		c.SettingsKeys = append([]string(nil), base.SettingsKeys...)
		c.FileLiveKeys = append([]string(nil), base.FileLiveKeys...)
		c.Experiments = append([]string(nil), base.Experiments...)
		return c
	}
	cases := []struct {
		name string
		add  func(*testAdminPolicyInputs)
		want string
	}{
		{"permission", func(in *testAdminPolicyInputs) { in.Permissions = append(in.Permissions, "hub.fake_power.update") }, `permission "hub.fake_power.update" has no TestAdminPolicy classification`},
		{"legacy route", func(in *testAdminPolicyInputs) { in.LegacyRoutes = append(in.LegacyRoutes, "admin.fakeRoute") }, `legacy admin route "admin.fakeRoute" has no TestAdminPolicy classification`},
		{"settings section", func(in *testAdminPolicyInputs) { in.SettingsSections = append(in.SettingsSections, "fake_section") }, `settings section "fake_section" has no TestAdminPolicy classification`},
		{"settings key", func(in *testAdminPolicyInputs) { in.SettingsKeys = append(in.SettingsKeys, "server.hub.fake_key") }, `settings key "server.hub.fake_key" has no TestAdminPolicy classification`},
		{"file-live key", func(in *testAdminPolicyInputs) { in.FileLiveKeys = append(in.FileLiveKeys, "fake_live_key") }, `file-live key "fake_live_key" has no TestAdminPolicy classification`},
		{"experiment", func(in *testAdminPolicyInputs) { in.Experiments = append(in.Experiments, "hub.fake_experiment") }, `experiment "hub.fake_experiment" has no TestAdminPolicy classification`},
		{"stale permission", func(in *testAdminPolicyInputs) { in.Permissions = in.Permissions[1:] }, "(stale TestAdminPolicy entry)"},
		{"stale experiment", func(in *testAdminPolicyInputs) { in.Experiments = in.Experiments[1:] }, "(stale TestAdminPolicy entry)"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			in := clone()
			tc.add(&in)
			problems := testAdminPolicyProblems(in)
			require.NotEmpty(t, problems)
			assert.Contains(t, strings.Join(problems, "\n"), tc.want)
		})
	}

	// An invalid classification value is reported.
	prev, had := testAdminExperimentPolicy[experiments.AgentEdit]
	testAdminExperimentPolicy[experiments.AgentEdit] = "maybe"
	t.Cleanup(func() {
		if had {
			testAdminExperimentPolicy[experiments.AgentEdit] = prev
		}
	})
	assert.Contains(t, strings.Join(testAdminPolicyProblems(clone()), "\n"), `has an invalid TestAdminPolicy "maybe"`)
	require.ErrorIs(t, TestAdminPolicyComplete(), ErrTestAdminPolicyIncomplete, "the G3 startup check sees the same gap")
}

// The initial deny set (admin tier design §3.6.1) is deny, every settings
// section and key, file-live key and experiment is deny, and only allow
// and deny are used.
func TestTestAdminPolicy_InitialDenySet(t *testing.T) {
	for _, id := range testAdminMustDenyPermissions {
		assert.Equal(t, testAdminPolicyDeny, testAdminPermissionPolicy[id], id)
		assert.True(t, testAdminPermissionDenied(id), id)
	}
	families := 0
	for _, p := range permissions.Registry {
		for _, prefix := range testAdminMustDenyPermissionPrefixes {
			if strings.HasPrefix(p.ID, prefix) {
				families++
				assert.Equal(t, testAdminPolicyDeny, testAdminPermissionPolicy[p.ID], p.ID)
			}
		}
	}
	assert.GreaterOrEqual(t, families, 4, "hub.federation.* and hub.github_app.* exist")
	assert.True(t, testAdminPermissionDenied("hub.not_a_permission"), "an unclassified permission is denied")
	for name, tbl := range map[string]map[string]testAdminPolicy{
		"settings section": testAdminSettingsSectionPolicy,
		"settings key":     testAdminSettingsKeyPolicy,
		"file-live key":    testAdminFileLiveKeyPolicy,
		"experiment":       testAdminExperimentPolicy,
	} {
		for k, p := range tbl {
			assert.Equal(t, testAdminPolicyDeny, p, "%s %s: test identities get no settings or experiment writes", name, k)
		}
	}
	for k, p := range testAdminPermissionPolicy {
		assert.Contains(t, []testAdminPolicy{testAdminPolicyAllow, testAdminPolicyDeny}, p, k)
	}
	// The settings key matcher denies every classified key, nested keys,
	// file-live keys, sections, request-layout paths and unknown keys.
	for _, key := range []string{"server.hub.admin_emails", "telemetry.cloud.tls.ca_file", "runtimes.docker.type", "server.log_level",
		"auto_inject_gcloud_adc", "maintenance", "hub.public_url", "federation.enabled", "not.a.key", ""} {
		assert.False(t, testAdminSettingsKeyAllowed(key), key)
	}
	for _, e := range experiments.Default().All() {
		assert.False(t, testAdminExperimentAllowed(e.Name), e.Name)
	}
	assert.False(t, testAdminExperimentAllowed("hub.unknown_experiment"))
}

// testAdminGrantableAtProjectScope exempts the agent delegation grant
// management permissions from the grant check at project scope only, so a
// test admin can still grant project-owner and project-admin ("project
// roles stay grantable"). A test admin cannot hold a project binding
// itself (refuseTestAdminMemberGrantee, refuseTestAdminGrantee), and it
// never exercises these permissions (decide step 0c stays deny). Every
// member is deny-classified (the exemption only widens what may be
// granted, never what is exercised) and held by a built-in project role
// (so the exemption has a project-scope grant to apply to). It applies at
// no other scope and to no other deny-classified permission.
func TestTestAdminPolicy_GrantableAtProjectScopeCensus(t *testing.T) {
	projectRole := map[string]bool{}
	for _, id := range append(projectOwnerPermissionIDs(), projectAdminPermissionIDs()...) {
		projectRole[id] = true
	}
	require.NotEmpty(t, testAdminGrantableAtProjectScope)
	for id, ok := range testAdminGrantableAtProjectScope {
		assert.True(t, ok, id)
		assert.Equal(t, testAdminPolicyDeny, testAdminPermissionPolicy[id], "%s must be deny-classified", id)
		assert.True(t, projectRole[id], "%s must be held by a built-in project role", id)
		assert.False(t, testAdminGrantRefuses(id, store.RoleScopeProject), "%s grantable at project scope", id)
		assert.True(t, testAdminGrantRefuses(id, store.RoleScopeSystem), "%s refused at system scope", id)
		assert.True(t, testAdminGrantRefuses(id, "hub"), "%s refused at hub scope", id)
		assert.True(t, testAdminGrantRefuses(id, ""), "%s refused without a grant scope", id)
	}
	for id := range testAdminPermissionPolicy {
		if testAdminPermissionDenied(id) && !testAdminGrantableAtProjectScope[id] {
			assert.True(t, testAdminGrantRefuses(id, store.RoleScopeProject), "%s stays refused at project scope", id)
		}
	}
	assert.True(t, testAdminGrantRefuses("secret.use", store.RoleScopeProject), "secret.use stays refused at project scope")
	assert.False(t, testAdminGrantRefuses("agent.read", store.RoleScopeSystem), "an allow-classified permission is grantable")
}

// requireAdminAt sites: every handler call passes a literal site name, the
// names are exactly testAdminRequireAdminAtSites, and requireAdmin /
// requireAdminFor are called only by the route guard and by each other.
func TestTestAdminPolicy_LegacyRouteCensus(t *testing.T) {
	var sites []string
	walkPkgHubFuncs(t, func(file string, fn *ast.FuncDecl, _ *token.FileSet) {
		ast.Inspect(fn.Body, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			key := file + ":" + fn.Name.Name
			switch sel.Sel.Name {
			case "requireAdminAt":
				if key == "authorize.go:requireAdminFor" {
					return true
				}
				require.Len(t, call.Args, 4, key)
				lit, ok := call.Args[3].(*ast.BasicLit)
				require.True(t, ok && lit.Kind == token.STRING, "%s: requireAdminAt needs a literal site name", key)
				site, err := strconv.Unquote(lit.Value)
				require.NoError(t, err)
				sites = append(sites, site)
			case "requireAdminFor":
				assert.Equal(t, "authorize.go:requireAdmin", key, "requireAdminFor is called only by requireAdmin; handlers use requireAdminAt")
			case "requireAdmin":
				assert.Equal(t, "route_metadata.go:routeGuard", key, "requireAdmin is the route guard's fallback only")
			}
			return true
		})
	})
	sort.Strings(sites)
	want := append([]string(nil), testAdminRequireAdminAtSites...)
	sort.Strings(want)
	assert.Equal(t, want, sites)
}

// Role-claim admin gates: every pkg/hub function that calls
// IsUnscopedLocalPlatformAdmin, IsSystemAdmin or IsHubAdmin (outside their
// definitions), and every "role gate" of the A1
// role==admin census, is classified in testAdminRoleGatePolicy; no entry is
// stale; and each deny entry's enforcing function calls
// testAdminPolicyRefuses (or testAdminPolicyRefusesUser) with that key.
func TestTestAdminPolicy_RoleGateCensus(t *testing.T) {
	found := map[string]bool{}
	refusals := map[string]map[string]bool{} // function name -> keys it refuses
	walkPkgHubFuncs(t, func(file string, fn *ast.FuncDecl, _ *token.FileSet) {
		ast.Inspect(fn.Body, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			name := ""
			switch f := call.Fun.(type) {
			case *ast.Ident:
				name = f.Name
			case *ast.SelectorExpr:
				name = f.Sel.Name
			}
			switch name {
			case "IsUnscopedLocalPlatformAdmin":
				found[file+":"+fn.Name.Name] = true
			case "IsSystemAdmin", "IsHubAdmin":
				// Binding-based system-admin checks outside decide; the
				// definitions in authz.go are not gates.
				if file != "authz.go" {
					found[file+":"+fn.Name.Name] = true
				}
			case "testAdminPolicyRefuses", "testAdminPolicyRefusesUser":
				lit, ok := call.Args[len(call.Args)-1].(*ast.BasicLit)
				require.True(t, ok && lit.Kind == token.STRING, "%s:%s: %s needs a literal gate key", file, fn.Name.Name, name)
				key, err := strconv.Unquote(lit.Value)
				require.NoError(t, err)
				if refusals[fn.Name.Name] == nil {
					refusals[fn.Name.Name] = map[string]bool{}
				}
				refusals[fn.Name.Name][key] = true
				_, classified := testAdminRoleGatePolicy[key]
				assert.True(t, classified, "%s:%s refuses unclassified gate %q", file, fn.Name.Name, key)
				// A function that refuses under its own "file:function"
				// key is a gate in its own right, whether or not it also
				// compares a role.
				if key == file+":"+fn.Name.Name {
					found[key] = true
				}
			}
			return true
		})
	})
	for key, class := range testAdminRoleAdminCensus {
		if strings.HasPrefix(class, "role gate") {
			found[key] = true
		}
	}
	require.NotEmpty(t, found)
	for key := range found {
		_, ok := testAdminRoleGatePolicy[key]
		assert.True(t, ok, "role-claim admin gate %s has no TestAdminPolicy classification", key)
	}
	for key, g := range testAdminRoleGatePolicy {
		assert.True(t, found[key], "stale TestAdminPolicy role gate %s: no such gate", key)
		assert.NotEmpty(t, g.Reason, key)
		assertValidViewAsMemberGate(t, "testAdminRoleGatePolicy", key, g.ViewAsMember)
		if g.Policy != testAdminPolicyDeny {
			assert.Empty(t, g.EnforcedIn, "%s: only deny entries name an enforcing function", key)
			continue
		}
		fn := g.EnforcedIn
		if fn == "" {
			fn = key[strings.Index(key, ":")+1:]
		}
		assert.True(t, refusals[fn][key], "deny gate %s is not enforced: %s does not call testAdminPolicyRefuses(..., %q)", key, fn, key)
	}
}

const (
	// viewAsMemberDeniedFirst means the site is strictly preceded by a
	// denying identity/Decide/RouteHubAdmin gate. Requires By.
	viewAsMemberDeniedFirst viewAsMemberGateKind = "denied_first"
	// viewAsMemberNotARequestGate means the site is not a request-path caller
	// authorization gate (e.g., login/provisioning/seed/background/non-web
	// credential authentication). Requires Reason.
	viewAsMemberNotARequestGate viewAsMemberGateKind = "not_a_request_gate"
)

func vamDeniedFirst(by string) viewAsMemberGate {
	return viewAsMemberGate{Kind: viewAsMemberDeniedFirst, By: by}
}

func vamNotARequestGate(reason string) viewAsMemberGate {
	return viewAsMemberGate{Kind: viewAsMemberNotARequestGate, Reason: reason}
}

// viewAsMemberDBRoleReadPolicy classifies every function in pkg/hub that
// compares a struct's .Role field with "admin" / store.UserRoleAdmin and is
// not already classified in testAdminRoleGatePolicy (ptone/scion#8648,
// design §4.2.4).
var viewAsMemberDBRoleReadPolicy = map[string]viewAsMemberGate{
	"google_identity_resolver.go:provisionNewUser": vamNotARequestGate("sign-in user provisioning: syncs super-admin binding for newly provisioned user"),
	"handlers_auth.go:provisionUser":               vamNotARequestGate("sign-in user provisioning: syncs super-admin binding at login"),
	"handlers_test_login.go:handleTestLogin":       vamNotARequestGate("dev/test sign-in: syncs super-admin binding and clears view-as-member"),
	"handlers_users_core.go:updateUser":            vamNotARequestGate("target user role mutation: inspects requested *updates.Role, not caller role; gated by RouteHubAdmin"),
	"seed.go:backfillUserRoleBindings":             vamNotARequestGate("startup migration: backfills super-admin role bindings"),
	"seed.go:reconcileSuperAdminBindings":          vamNotARequestGate("startup reconciliation: reconciles User.Role with super-admin bindings"),
	"sign_in_policy.go:applyLiveSignInPolicy":      vamNotARequestGate("sign-in policy: syncs super-admin binding on login"),
	"web.go:handleOAuthCallback":                   vamNotARequestGate("OAuth callback sign-in: syncs super-admin binding and clears view-as-member"),
	"web.go:proxyAuthMiddleware":                   vamNotARequestGate("proxy sign-in/reconcile: syncs super-admin binding and stored sessKeyUserRole"),
}

// viewAsMemberIdentityRebuildPolicy classifies every function in pkg/hub that
// constructs an *AuthenticatedUser via NewAuthenticatedUser (ptone/scion#8648).
// Request-path helpers that rebuild the caller's identity from a *store.User
// row must honour view-as-member via applyViewAsMemberFromContext (Effective)
// or be explicitly classified here.
var viewAsMemberIdentityRebuildPolicy = map[string]viewAsMemberGate{
	// Sanctioned entry points and request-path caller rebuilders (Effective).
	"auth.go:UnifiedAuthMiddleware":               vamEffective(),
	"decisions_agent.go:loadSourceAuthorIdentity": vamEffective(),
	"port_view.go:portViewIdentity":               vamEffective(),
	"web.go:authorizeSSESubjects":                 vamEffective(),
	"web.go:expandProjectWildcard":                vamEffective(),
	"web.go:newSSEMessageViewer":                  vamEffective(),
	// Admin-only routes denied before the rebuild runs.
	"agent_scope_reissue.go:planUserDelegatorReissue": vamDeniedFirst("hub.auth_reset.execute (authorizeScopeReissue)"),
	"agent_scope_reissue.go:reissueUATIdentity":       vamDeniedFirst("hub.auth_reset.execute (authorizeScopeReissue)"),
	"audit_authz.go:handleAuthzExplain":               vamDeniedFirst("RouteHubAdmin (hub.authz.explain)"),
	"audit_authz.go:resolveExplainPrincipal":          vamDeniedFirst("RouteHubAdmin (hub.authz.explain)"),
	// RealByDesign: intentionally checks real stored role.
	"view_as_member.go:AuthorizeViewAsMemberChange": vamRealByDesign("evaluates real (unclamped) admin eligibility to enter view-as-member"),
	// Non-caller, background, audit, or non-web-session credential builders.
	"agent_delegation.go:issuerPrincipal":                               vamNotARequestGate("builds PrincipalContext for an agent delegation issuer, not a web session caller"),
	"agent_pending_config_authz.go:resolvePendingUserSaver":             vamNotARequestGate("background held-edit fold rebuilds the saver from DB; maintenance deferral is RealByDesign (D4)"),
	"agent_standing.go:userAdmissionAs":                                 vamNotARequestGate("evaluates an agent's standing root/delegator user, not a web session caller"),
	"artifacts_host.go:recordedDelegator":                               vamNotARequestGate("evaluates an artifact's recorded delegator, not a web session caller"),
	"artifacts_host.go:sponsorReadsHomeThroughRole":                     vamNotARequestGate("evaluates an artifact sponsor's role via clamped AuthzService"),
	"artifacts_recipient.go:recipientIdentity":                          vamNotARequestGate("evaluates a target share recipient user, not a caller admin gate"),
	"artifacts_subject_user.go:namedUserSubject":                        vamNotARequestGate("resolves a named target subject user for an artifact access grant"),
	"auth.go:MakeProxyUserProvisioner":                                  vamNotARequestGate("proxy direct API auth (no scion_sess cookie; §4.4 unaffected)"),
	"auth.go:extractProxyUser":                                          vamNotARequestGate("proxy direct API auth (no scion_sess cookie; §4.4 unaffected)"),
	"auth_external_bearer.go:authenticateExternalBearer":                vamNotARequestGate("external Google bearer auth (no scion_sess cookie; §4.4 unaffected)"),
	"authz_delegation_ceiling.go:userRelationshipAuthority":             vamNotARequestGate("evaluates a delegator's relationship authority via clamped AuthzService"),
	"authz_execution_project.go:sourceUserExecutionAdmission":           vamNotARequestGate("evaluates a trigger/agent source user's project admission, not a web session caller"),
	"authz_provenance_root.go:ResolveProvenanceRoot":                    vamNotARequestGate("evaluates an agent's provenance root user, not a web session caller"),
	"authz_resource_parent_ceiling.go:loadUserSecretSourceInputs":       vamNotARequestGate("evaluates a secret source user's bindings via clamped AuthzService"),
	"authz_resource_parent_ceiling.go:parentCeilingSourceIdentity":      vamNotARequestGate("evaluates a parent-ceiling source user, not a web session caller"),
	"broker_inbound_sender.go:resolveBrokerInboundSender":               vamNotARequestGate("broker-authenticated inbound message sender (no web session)"),
	"brokerauth.go:resolveOnBehalfOf":                                   vamNotARequestGate("broker-authenticated on-behalf-of user (no web session)"),
	"chat_member_fanout.go:fanOutThreadMessageToMembers":                vamNotARequestGate("chat member fanout checks each member via clamped AuthzService.CheckAccess"),
	"chat_scheduled_send.go:checkScheduledFire":                         vamNotARequestGate("background worker; durable state must use real authority"),
	"conduit_authz.go:conduitRenewalIdentity":                           vamNotARequestGate("background conduit renewal identity (no web session)"),
	"conduit_relay_ops.go:relayUserIdentity":                            vamNotARequestGate("conduit relay credential identity (no web session)"),
	"decisions_bell.go:expandDecisionBellTopic":                         vamNotARequestGate("background worker; durable state must use real authority"),
	"decisions_profile.go:waitingOnYouTotal":                            vamNotARequestGate("background worker; durable state must use real authority"),
	"exec_container.go:ResolveExecContainer":                            vamNotARequestGate("resolves exec container owner via clamped AuthzService, not a caller admin gate"),
	"firecore_authority.go:resolveFireUser":                             vamNotARequestGate("background trigger/schedule fire authority (no web session)"),
	"github_app_association_authz.go:AuthorizeGitHubTokenSourceProject": vamNotARequestGate("evaluates GitHub token source user via clamped AuthzService"),
	"github_app_association_authz.go:githubBindingUserIsAdmin":          vamNotARequestGate("evaluates GitHub binding user via clamped AuthzService.Decide"),
	"lifecycle_hook_fire_authority.go:authorizeHookFire":                vamNotARequestGate("background lifecycle hook fire authority (no web session)"),
	"message_hold_redrive.go:revalidateDelegatedHeldMessage":            vamNotARequestGate("background/redrive revalidation of held delegated message issuer"),
	"message_hold_redrive.go:revalidateUserHeldMessage":                 vamNotARequestGate("background/redrive revalidation of held message sender"),
	"passthrough_translation_review.go:reviewTranslatedAgent":           vamNotARequestGate("evaluates an agent's launching user for passthrough translation review"),
	"skill_dispatch_resolve.go:creatorIdentityForAgent":                 vamNotARequestGate("evaluates an agent's creator for skill dispatch, not a web session caller"),
	"test_identity_workload_issuer.go:authenticate":                     vamNotARequestGate("workload delegation grantor identity (workload auth, no web session)"),
	"thread_group_migration.go:tgmAllowedGroups":                        vamNotARequestGate("background thread-group migration (no web session)"),
	"useraccesstoken.go:ValidateToken":                                  vamNotARequestGate("UAT (scion_pat_*) authentication (no scion_sess cookie; §4.4 unaffected)"),
}

// validateViewAsMemberGate returns a diagnostic string if g is missing or
// invalid for its kind, or "" if valid (ptone/scion#8648, design §4.2.4, §11 item 9).
func validateViewAsMemberGate(table, key string, g viewAsMemberGate) string {
	switch g.Kind {
	case viewAsMemberEffective:
		if g.By != "" {
			return table + "[" + key + "]: Effective must not set By"
		}
		return ""
	case viewAsMemberRealByDesign:
		if strings.TrimSpace(g.Reason) == "" {
			return table + "[" + key + "]: RealByDesign requires a Reason"
		}
		if g.By != "" {
			return table + "[" + key + "]: RealByDesign must not set By"
		}
		return ""
	case viewAsMemberDeniedFirst:
		if strings.TrimSpace(g.By) == "" {
			return table + "[" + key + "]: DeniedFirst requires By naming the preceding denying gate"
		}
		return ""
	case viewAsMemberNotARequestGate:
		if strings.TrimSpace(g.Reason) == "" {
			return table + "[" + key + "]: NotARequestGate requires a Reason"
		}
		if g.By != "" {
			return table + "[" + key + "]: NotARequestGate must not set By"
		}
		return ""
	default:
		return table + "[" + key + "]: ViewAsMember.Kind=" + strconv.Quote(string(g.Kind)) + " is missing or invalid (must be effective, real_by_design, denied_first, or not_a_request_gate)"
	}
}

// assertValidViewAsMemberGate validates that a viewAsMemberGate classification
// is present and carries the required fields for its kind (ptone/scion#8648,
// design §4.2.4, §11 item 9).
func assertValidViewAsMemberGate(t *testing.T, table, key string, g viewAsMemberGate) {
	t.Helper()
	if msg := validateViewAsMemberGate(table, key, g); msg != "" {
		assert.Fail(t, msg)
	}
}

// TestViewAsMember_MissingClassificationFails verifies design §11 item 9:
// omitting or malforming a ViewAsMember classification is rejected.
func TestViewAsMember_MissingClassificationFails(t *testing.T) {
	assert.NotEmpty(t, validateViewAsMemberGate("tbl", "k", viewAsMemberGate{}), "zero-value ViewAsMember must fail")
	assert.NotEmpty(t, validateViewAsMemberGate("tbl", "k", viewAsMemberGate{Kind: "bogus"}))
	assert.NotEmpty(t, validateViewAsMemberGate("tbl", "k", viewAsMemberGate{Kind: viewAsMemberRealByDesign}))
	assert.NotEmpty(t, validateViewAsMemberGate("tbl", "k", viewAsMemberGate{Kind: viewAsMemberDeniedFirst}))
	assert.NotEmpty(t, validateViewAsMemberGate("tbl", "k", viewAsMemberGate{Kind: viewAsMemberNotARequestGate}))
	assert.Empty(t, validateViewAsMemberGate("tbl", "k", vamEffective()))
	assert.Empty(t, validateViewAsMemberGate("tbl", "k", vamRealByDesign("D4")))
	assert.Empty(t, validateViewAsMemberGate("tbl", "k", vamDeniedFirst("RouteHubAdmin")))
	assert.Empty(t, validateViewAsMemberGate("tbl", "k", vamNotARequestGate("seed")))
}

// isRoleFieldExpr matches struct field accesses like u.Role, user.Role,
// sessionUser.Role, and *updates.Role (not Role() method calls).
func isRoleFieldExpr(e ast.Expr) bool {
	switch x := e.(type) {
	case *ast.SelectorExpr:
		return x.Sel.Name == "Role"
	case *ast.StarExpr:
		return isRoleFieldExpr(x.X)
	}
	return false
}

// TestViewAsMember_DBRoleReadCensus scans every pkg/hub function comparing a
// struct .Role field with "admin" / store.UserRoleAdmin and requires it to be
// classified for view-as-member in either testAdminRoleGatePolicy or
// viewAsMemberDBRoleReadPolicy (ptone/scion#8648, design §4.2.4).
func TestViewAsMember_DBRoleReadCensus(t *testing.T) {
	found := map[string]bool{}
	walkPkgHubFuncs(t, func(file string, fn *ast.FuncDecl, _ *token.FileSet) {
		ast.Inspect(fn.Body, func(n ast.Node) bool {
			bin, ok := n.(*ast.BinaryExpr)
			if !ok || (bin.Op != token.EQL && bin.Op != token.NEQ) {
				return true
			}
			if (isRoleFieldExpr(bin.X) && isAdminRoleLiteral(bin.Y)) ||
				(isRoleFieldExpr(bin.Y) && isAdminRoleLiteral(bin.X)) {
				found[file+":"+fn.Name.Name] = true
			}
			return true
		})
	})
	require.NotEmpty(t, found)

	for key := range found {
		if g, ok := testAdminRoleGatePolicy[key]; ok {
			assertValidViewAsMemberGate(t, "testAdminRoleGatePolicy", key, g.ViewAsMember)
			assert.NotContains(t, viewAsMemberDBRoleReadPolicy, key,
				"%s is already classified in testAdminRoleGatePolicy; do not duplicate in viewAsMemberDBRoleReadPolicy", key)
			continue
		}
		g, ok := viewAsMemberDBRoleReadPolicy[key]
		if !assert.True(t, ok, "DB-row .Role admin comparison %s has no ViewAsMember classification in testAdminRoleGatePolicy or viewAsMemberDBRoleReadPolicy", key) {
			continue
		}
		assertValidViewAsMemberGate(t, "viewAsMemberDBRoleReadPolicy", key, g)
	}
	for key, g := range viewAsMemberDBRoleReadPolicy {
		assert.True(t, found[key], "stale viewAsMemberDBRoleReadPolicy entry %s: no .Role admin comparison in function", key)
		assertValidViewAsMemberGate(t, "viewAsMemberDBRoleReadPolicy", key, g)
	}
}

// TestViewAsMember_IdentityRebuildCensus scans every pkg/hub function calling
// NewAuthenticatedUser (outside its definition) and requires a ViewAsMember
// classification in viewAsMemberIdentityRebuildPolicy. Effective rebuilders
// outside UnifiedAuthMiddleware and webSessionUser-based SSE helpers must call
// applyViewAsMemberFromContext so rebuilt caller identities cannot bypass the
// effective-role lever (ptone/scion#8648, design §4.2.4).
func TestViewAsMember_IdentityRebuildCensus(t *testing.T) {
	// Sanctioned Effective sites that do not rebuild from a DB *store.User row:
	// UnifiedAuthMiddleware applies WithViewAsMember directly; the web.go SSE
	// helpers construct from webSessionUser.Role (already effective "member").
	directEffectiveSites := map[string]bool{
		"auth.go:UnifiedAuthMiddleware": true,
		"web.go:authorizeSSESubjects":   true,
		"web.go:expandProjectWildcard":  true,
		"web.go:newSSEMessageViewer":    true,
	}

	found := map[string]*ast.FuncDecl{}
	walkPkgHubFuncs(t, func(file string, fn *ast.FuncDecl, _ *token.FileSet) {
		key := file + ":" + fn.Name.Name
		if key == "internal/principal/identity.go:NewAuthenticatedUser" {
			return
		}
		ast.Inspect(fn.Body, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			name := ""
			switch f := call.Fun.(type) {
			case *ast.Ident:
				name = f.Name
			case *ast.SelectorExpr:
				name = f.Sel.Name
			}
			if name == "NewAuthenticatedUser" {
				found[key] = fn
			}
			return true
		})
	})
	require.NotEmpty(t, found)

	for key, fn := range found {
		g, ok := viewAsMemberIdentityRebuildPolicy[key]
		if !assert.True(t, ok, "NewAuthenticatedUser caller %s has no ViewAsMember classification in viewAsMemberIdentityRebuildPolicy", key) {
			continue
		}
		assertValidViewAsMemberGate(t, "viewAsMemberIdentityRebuildPolicy", key, g)
		if g.Kind == viewAsMemberEffective && !directEffectiveSites[key] {
			refs := identRefs(fn)
			assert.True(t, refs["applyViewAsMemberFromContext"],
				"viewAsMemberIdentityRebuildPolicy[%s] is classified Effective but does not call applyViewAsMemberFromContext", key)
		}
	}
	for key, g := range viewAsMemberIdentityRebuildPolicy {
		_, ok := found[key]
		assert.True(t, ok, "stale viewAsMemberIdentityRebuildPolicy entry %s: function does not call NewAuthenticatedUser", key)
		assertValidViewAsMemberGate(t, "viewAsMemberIdentityRebuildPolicy", key, g)
	}
}

// The sink chains every line to the previous one; the verifier accepts
// the chain (with unrelated output interleaved and across process starts)
// and detects a deleted, edited, inserted or reordered line.
func TestTestAdminAuditSink_ChainAndVerifier(t *testing.T) {
	var buf bytes.Buffer
	sink := newTestAdminAuditSink(&buf)
	for i := 0; i < 5; i++ {
		require.NoError(t, sink.emit("ev", map[string]any{"i": i}))
		buf.WriteString("unrelated process output\n")
	}
	n, err := verifyTestAdminAuditChain(bytes.NewReader(buf.Bytes()))
	require.NoError(t, err)
	assert.Equal(t, 5, n)

	// A second process start opens a new chain.
	second := newTestAdminAuditSink(&buf)
	require.NoError(t, second.emit("ev", nil))
	require.NoError(t, second.emit("ev", nil))
	n, err = verifyTestAdminAuditChain(bytes.NewReader(buf.Bytes()))
	require.NoError(t, err)
	assert.Equal(t, 7, n)

	tagged := func() []string {
		var out []string
		for _, l := range strings.Split(strings.TrimRight(buf.String(), "\n"), "\n") {
			if strings.HasPrefix(l, testAdminAuditTag) {
				out = append(out, l)
			}
		}
		return out
	}
	join := func(lines []string) string { return strings.Join(lines, "\n") + "\n" }
	lines := tagged()
	require.Len(t, lines, 7)

	mutations := map[string][]string{
		"deleted line":   append(append([]string(nil), lines[:2]...), lines[3:]...),
		"deleted first":  lines[1:],
		"edited line":    append(append(append([]string(nil), lines[:2]...), strings.Replace(lines[2], `"i":2`, `"i":9`, 1)), lines[3:]...),
		"swapped lines":  append(append(append([]string(nil), lines[:1]...), lines[2], lines[1]), lines[3:]...),
		"inserted line":  append(append(append([]string(nil), lines[:3]...), lines[1]), lines[3:]...),
		"untagged chain": append(append(append([]string(nil), lines[:3]...), testAdminAuditTag+` {"event":"forged"}`), lines[3:]...),
	}
	for name, ls := range mutations {
		_, err := verifyTestAdminAuditChain(strings.NewReader(join(ls)))
		assert.ErrorIs(t, err, errTestAdminAuditChainBroken, name)
	}

	// A failed write does not advance the chain: the next line links to
	// the last line written.
	var ok bytes.Buffer
	w := &taFlakyWriter{out: &ok}
	s := newTestAdminAuditSink(w)
	require.NoError(t, s.emit("a", nil))
	w.fail = true
	require.Error(t, s.emit("b", nil))
	w.fail = false
	require.NoError(t, s.emit("c", nil))
	n, err = verifyTestAdminAuditChain(bytes.NewReader(ok.Bytes()))
	require.NoError(t, err)
	assert.Equal(t, 2, n)
}

// The sink sets event, ts, seq and prev itself; a caller field of the same
// name does not override them.
func TestTestAdminAuditSink_ReservedFields(t *testing.T) {
	var buf bytes.Buffer
	sink := newTestAdminAuditSink(&buf)
	require.NoError(t, sink.emit("real", map[string]any{"event": "forged", "seq": 99, "prev": "x", "ts": "y"}))
	line := strings.TrimSpace(buf.String())
	assert.Contains(t, line, `"event":"real"`)
	assert.Contains(t, line, `"seq":0`)
	assert.Contains(t, line, `"prev":"`+testAdminAuditChainSeed+`"`)
	assert.NotContains(t, line, `"forged"`)
	_, err := verifyTestAdminAuditChain(strings.NewReader(buf.String()))
	require.NoError(t, err)
	var nilSink *testAdminAuditSink
	require.Error(t, nilSink.emit("x", nil), "an unconfigured sink fails, so callers fail closed")
}

type taFlakyWriter struct {
	out  *bytes.Buffer
	fail bool
}

func (w *taFlakyWriter) Write(p []byte) (int, error) {
	if w.fail {
		return 0, errors.New("sink unavailable")
	}
	return w.out.Write(p)
}

// Review C1: CanDelegate runs the test-admin delegation check before the
// role-claim super-admin shortcut and before any type-specific
// non-amplification check, so neither admits a deny-classified grant by an
// admin kind.
func TestTestAdminPolicy_CanDelegateChecksPolicyFirst(t *testing.T) {
	var refusePos, shortcutPos token.Pos
	var typeChecks []token.Pos
	found := false
	walkPkgHubFuncs(t, func(file string, fn *ast.FuncDecl, _ *token.FileSet) {
		if file != "authz_candelegate.go" || fn.Name.Name != "CanDelegate" || fn.Recv == nil {
			return
		}
		found = true
		ast.Inspect(fn.Body, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			name := ""
			switch f := call.Fun.(type) {
			case *ast.Ident:
				name = f.Name
			case *ast.SelectorExpr:
				name = f.Sel.Name
			}
			switch {
			case name == "refuseTestAdminDeniedGrant" && refusePos == 0:
				refusePos = call.Pos()
			case name == "IsUnscopedLocalPlatformAdmin" && shortcutPos == 0:
				shortcutPos = call.Pos()
			case strings.HasPrefix(name, "canDelegate"):
				typeChecks = append(typeChecks, call.Pos())
			}
			return true
		})
	})
	require.True(t, found, "CanDelegate exists")
	require.NotZero(t, refusePos, "CanDelegate calls refuseTestAdminDeniedGrant")
	require.NotZero(t, shortcutPos)
	assert.Less(t, refusePos, shortcutPos, "the policy check runs before the super-admin shortcut")
	require.NotEmpty(t, typeChecks)
	for _, p := range typeChecks {
		assert.Less(t, refusePos, p, "the policy check runs before the type-specific checks")
	}
}

// Review RN3: the agent-delegation exclusion in CanDelegate relies on no
// deny-classified permission other than secret.use being an agent scope.
// Giving another deny-classified permission AgentScopes fails here and
// forces a re-review of that exclusion.
func TestTestAdminPolicy_DenyPermissionsHaveNoAgentScopes(t *testing.T) {
	for _, p := range permissions.Registry {
		if testAdminPermissionDenied(p.ID) && p.ID != "secret.use" {
			assert.Empty(t, p.AgentScopes, "deny-classified %s has AgentScopes; re-review the CanDelegate agent-delegation exclusion", p.ID)
		}
	}
}
