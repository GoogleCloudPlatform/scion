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
	"strings"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/hub/authzop"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/require"
)

// liveInventoryKey identifies one HTTP entry point declared in
// authzop.Catalog, by the operation that declares it and the declared
// method/pattern.
type liveInventoryKey struct {
	OperationID string
	Method      string
	Pattern     string
}

// positiveCheckExclusions lists HTTP catalog entry points for which
// TestCatalogHTTPEntryPoints_LiveMethodCheck does not require "declared
// method + a real target must not 404/405". Each reason must be true and
// specific. This check is skipped only when dispatching the declared method
// for real would itself be the problem — a real external or host-level side
// effect — never merely because a fixture would be inconvenient.
// TestLiveInventoryExclusionsNotStale asserts every key here still names a
// real, currently-declared catalog HTTP entry point.
var positiveCheckExclusions = map[liveInventoryKey]string{
	{OperationID: "hub.maintenance.execute", Method: "POST", Pattern: "/api/v1/admin/maintenance/restart"}:             "handleAdminRestart (admin_maintenance.go) invokes a real systemd restart subprocess; nothing before it short-circuits for a fake or real target, so there is no safe way to dispatch the declared method",
	{OperationID: "hub.maintenance.execute", Method: "POST", Pattern: "/api/v1/admin/maintenance/check-updates"}:       "handleCheckForUpdates (admin_maintenance.go:662-690) calls the GitHub release channel when MaintenanceConfig.DeploymentTier == \"binary\"; excluded so this test cannot depend on, or accidentally call out based on, server config",
	{OperationID: "hub.maintenance.execute", Method: "POST", Pattern: "/api/v1/admin/maintenance/operations/{id}/run"}: "executeOperation (admin_maintenance.go) resolves a real executor and runs it (e.g. pulling container images) for any operation key that exists; a fake key 404s, so there is no fixture that both exists and is safe to actually run",
	{OperationID: "hub.maintenance.execute", Method: "POST", Pattern: "/api/v1/admin/maintenance/migrations/{id}/run"}: "executeMigration (admin_maintenance.go) runs a real migration for any migration key that exists; same reasoning as the operations/{id}/run exclusion above",
	{OperationID: "hub.integrations.read", Method: "GET", Pattern: "/api/v1/admin/integrations/{name}"}:                "handleGetIntegration (handlers_integrations.go:359-382) 404s for any name unless a plugin manager has it loaded or a global on-disk plugin-settings file lists it; testServer configures neither, and reading or writing that global config file from this test is out of scope",
}

// controlCheckExclusions lists HTTP catalog entry points for which
// TestCatalogHTTPEntryPoints_LiveMethodCheck does not require "an
// unsupported method must return 405". Each reason names the specific
// dispatch shape that makes a 405 control meaningless for that entry — not
// merely that the control happens to fail today.
var controlCheckExclusions = map[liveInventoryKey]string{
	{OperationID: "agent.portaccess", Method: "GET", Pattern: "/api/v1/agents/{id}/ports/{port}/proxy"}:           "proxyAgentPort forwards every HTTP method to the tunnel with no method-based routing at all (see the entry's own comment in catalog.go); there is no unsupported method to control against",
	{OperationID: "agent.portaccess", Method: "POST", Pattern: "/api/v1/agents/{id}/ports/{port}/proxy"}:          "same as the GET .../proxy entry above: proxyAgentPort accepts every method by design",
	{OperationID: "agent.portaccess", Method: "PUT", Pattern: "/api/v1/agents/{id}/ports/{port}/proxy"}:           "same as the GET .../proxy entry above: proxyAgentPort accepts every method by design",
	{OperationID: "agent.portaccess", Method: "DELETE", Pattern: "/api/v1/agents/{id}/ports/{port}/proxy"}:        "same as the GET .../proxy entry above: proxyAgentPort accepts every method by design",
	{OperationID: "agent.portaccess", Method: "GET", Pattern: "/api/v1/agents/{id}/ports/{port}/proxy/{subpath}"}: "same as the GET .../proxy entry above: proxyAgentPort accepts every method by design",
	{OperationID: "gcp.identity.verify", Method: "POST", Pattern: "/api/v1/gcp-service-accounts/{id}/verify"}:     "handleGCPServiceAccountByID (handlers_gcp_identity_scoped.go:297-304) matches on action==\"verify\" && method==POST as a single condition; any other method on the same action falls through to the generic \"action not found\" 404, never a 405",
	{OperationID: "chat.access", Method: "GET", Pattern: "/api/v1/chat/prefs"}:                                    "handleChatPrefs (handlers_chat_prefs.go) checks webChatStore for nil and returns 503 before it ever looks at r.Method; testServer does not configure a webChatStore",
	{OperationID: "chat.access", Method: "PUT", Pattern: "/api/v1/chat/prefs"}:                                    "same as the GET /chat/prefs entry above: the nil-webChatStore check precedes the method switch",
	{OperationID: "hub.integrations.read", Method: "GET", Pattern: "/api/v1/admin/integrations/{name}"}:           "handleAdminIntegrationByName (handlers_integrations.go:219-224) requires hub.integrations.update for PUT/POST/DELETE inline before the name/action switch; an unrecognized method other than GET still reaches the action switch and 404s there (unknown action), not 405, for the bare-name shape this catalog entry declares",
}

// idFixtures holds the real, store-seeded entity IDs this test substitutes
// into catalog patterns so a correctly-dispatched request reaches live
// business logic instead of a placeholder-driven 404. Populated by
// seedLiveInventoryFixtures.
type idFixtures struct {
	project                 string
	projectDel              string
	agent                   string
	agentDel                string
	agentPort               string
	group                   string
	groupDel                string
	user                    string
	userDel                 string
	member                  string
	skill                   string
	skillDel                string
	skillRegistry           string
	template                string
	templateDel             string
	harnessConfig           string
	harnessConfigDel        string
	schedule                string
	scheduledEvent          string
	limit                   string
	limitUD                 string
	entitlement             string
	entitlementUD           string
	gcpSA                   string
	gcpSADel                string
	accessConstraint        string
	accessConstraintUD      string
	invite                  string
	envVarKey               string
	uatRevokePost           string
	uatRevokeDelete         string
	runtimeBroker           string
	githubInstallationID    string
	roleDefinition          string
	roleDefinitionDel       string
	roleBindingDel          string
	projectMembership       string
	allowListEmail          string
	maintenanceOpKey        string
	maintenanceMigrationKey string
}

// seedLiveInventoryFixtures creates one real store row per resource family
// (plus a second, disposable instance for families with a destructive
// catalog operation, so a DELETE entry tested earlier in catalog
// declaration order cannot remove the fixture a later read/update entry
// still needs) and returns their IDs. It never mints a real credential and
// never calls out to any external service — everything it creates lives
// only in the test's in-memory SQLite store.
func seedLiveInventoryFixtures(t *testing.T, ctx context.Context, s store.Store) idFixtures {
	t.Helper()
	now := time.Now()

	f := idFixtures{
		project:              tid("li-project"),
		projectDel:           tid("li-project-del"),
		agent:                tid("li-agent"),
		agentDel:             tid("li-agent-del"),
		agentPort:            "18080",
		group:                tid("li-group"),
		groupDel:             tid("li-group-del"),
		user:                 tid("li-user"),
		userDel:              tid("li-user-del"),
		member:               tid("li-member"),
		skill:                tid("li-skill"),
		skillDel:             tid("li-skill-del"),
		skillRegistry:        tid("li-skill-registry"),
		template:             tid("li-template"),
		templateDel:          tid("li-template-del"),
		harnessConfig:        tid("li-hc"),
		harnessConfigDel:     tid("li-hc-del"),
		schedule:             tid("li-schedule"),
		scheduledEvent:       tid("li-sched-event"),
		limit:                tid("li-limit"),
		limitUD:              tid("li-limit-ud"),
		entitlement:          tid("li-entitlement"),
		entitlementUD:        tid("li-entitlement-ud"),
		gcpSA:                tid("li-gcp-sa"),
		gcpSADel:             tid("li-gcp-sa-del"),
		accessConstraint:     tid("li-ac"),
		accessConstraintUD:   tid("li-ac-ud"),
		invite:               tid("li-invite"),
		allowListEmail:       "li-allowlist@test.com",
		envVarKey:            "LI_INVENTORY_TEST_VAR",
		uatRevokePost:        tid("li-uat-revoke-post"),
		uatRevokeDelete:      tid("li-uat-revoke-delete"),
		runtimeBroker:        tid("li-broker"),
		githubInstallationID: "900000001",
		roleDefinition:       tid("li-role"),
		roleDefinitionDel:    tid("li-role-del"),
		roleBindingDel:       tid("li-role-binding-del"),
	}

	require.NoError(t, s.CreateProject(ctx, &store.Project{ID: f.project, Name: "LI Project", Slug: "li-project"}))
	require.NoError(t, s.CreateProject(ctx, &store.Project{ID: f.projectDel, Name: "LI Project Del", Slug: "li-project-del"}))

	require.NoError(t, s.CreateAgent(ctx, &store.Agent{ID: f.agent, Slug: "li-agent", Name: "LI Agent", ProjectID: f.project, Phase: string(state.PhaseRunning)}))
	require.NoError(t, s.CreateAgent(ctx, &store.Agent{ID: f.agentDel, Slug: "li-agent-del", Name: "LI Agent Del", ProjectID: f.project, Phase: string(state.PhaseRunning)}))
	require.NoError(t, s.UpdateAgentExposedPorts(ctx, f.agent, []store.ExposedPort{{Port: 18080, Host: "127.0.0.1", Label: "li", Mode: "rw", ExposedAt: now, ExposedBy: "agent"}}))

	require.NoError(t, s.CreateGroup(ctx, &store.Group{ID: f.group, Slug: "li-group", Name: "LI Group"}))
	require.NoError(t, s.CreateGroup(ctx, &store.Group{ID: f.groupDel, Slug: "li-group-del", Name: "LI Group Del"}))

	require.NoError(t, s.CreateUser(ctx, &store.User{ID: f.user, Email: "li-user@test.com", DisplayName: "LI User", Role: "member", Status: "active"}))
	require.NoError(t, s.CreateUser(ctx, &store.User{ID: f.userDel, Email: "li-user-del@test.com", DisplayName: "LI User Del", Role: "member", Status: "active"}))
	require.NoError(t, s.CreateUser(ctx, &store.User{ID: f.member, Email: "li-member@test.com", DisplayName: "LI Member", Role: "member", Status: "active"}))
	require.NoError(t, s.AddGroupMember(ctx, &store.GroupMember{GroupID: f.group, MemberID: f.member, MemberType: store.GroupMemberTypeUser, Role: store.GroupMemberRoleMember}))

	require.NoError(t, s.CreateSkill(ctx, &store.Skill{ID: f.skill, Name: "li-skill", Slug: "li-skill", Scope: store.SkillScopeGlobal, Status: "active", Created: now, Updated: now}))
	require.NoError(t, s.CreateSkill(ctx, &store.Skill{ID: f.skillDel, Name: "li-skill-del", Slug: "li-skill-del", Scope: store.SkillScopeGlobal, Status: "active", Created: now, Updated: now}))
	// CreateSkillRegistry (skill_registry_store.go) ignores a caller-supplied
	// ID and mints its own, writing it back into the passed struct.
	skillRegistry := &store.SkillRegistry{Name: "li-skill-registry", Endpoint: "https://example.invalid/registry", Type: store.SkillRegistryTypeHub, TrustLevel: store.SkillRegistryTrustTrusted, Status: store.SkillRegistryStatusActive, Created: now, Updated: now}
	require.NoError(t, s.CreateSkillRegistry(ctx, skillRegistry))
	f.skillRegistry = skillRegistry.ID

	require.NoError(t, s.CreateTemplate(ctx, &store.Template{ID: f.template, Name: "li-template", Slug: "li-template", Harness: "claude", Scope: store.TemplateScopeGlobal, Created: now, Updated: now}))
	require.NoError(t, s.CreateTemplate(ctx, &store.Template{ID: f.templateDel, Name: "li-template-del", Slug: "li-template-del", Harness: "claude", Scope: store.TemplateScopeGlobal, Created: now, Updated: now}))

	require.NoError(t, s.CreateHarnessConfig(ctx, &store.HarnessConfig{ID: f.harnessConfig, Name: "li-hc", Slug: "li-hc", Harness: "claude", Scope: store.HarnessConfigScopeGlobal, Status: store.HarnessConfigStatusActive, Created: now, Updated: now}))
	require.NoError(t, s.CreateHarnessConfig(ctx, &store.HarnessConfig{ID: f.harnessConfigDel, Name: "li-hc-del", Slug: "li-hc-del", Harness: "claude", Scope: store.HarnessConfigScopeGlobal, Status: store.HarnessConfigStatusActive, Created: now, Updated: now}))

	require.NoError(t, s.CreateSchedule(ctx, &store.Schedule{ID: f.schedule, ProjectID: f.project, Name: "li-schedule", CronExpr: "0 0 * * *", EventType: "message", Payload: "{}", Status: store.ScheduleStatusActive, CreatedAt: now, UpdatedAt: now}))
	require.NoError(t, s.CreateScheduledEvent(ctx, &store.ScheduledEvent{ID: f.scheduledEvent, ProjectID: f.project, EventType: "message", FireAt: now.Add(time.Hour), Payload: "{}", Status: store.ScheduledEventPending, CreatedAt: now}))

	limitDef, err := s.CreateLimitDefinition(ctx, &store.LimitDefinition{ID: f.limit, Name: "li-limit", ResourceType: "agent", Unit: "count", DefaultValue: 10, CreatedAt: now, UpdatedAt: now})
	require.NoError(t, err)
	_, err = s.CreateLimitDefinition(ctx, &store.LimitDefinition{ID: f.limitUD, Name: "li-limit-ud", ResourceType: "agent", Unit: "count", DefaultValue: 10, CreatedAt: now, UpdatedAt: now})
	require.NoError(t, err)
	_, err = s.CreateEntitlementBinding(ctx, &store.EntitlementBinding{ID: f.entitlement, LimitDefinitionID: limitDef.ID, SubjectType: "user", SubjectID: f.user, ScopeType: "system", Value: 5, CreatedBy: f.user, CreatedAt: now, UpdatedAt: now})
	require.NoError(t, err)
	_, err = s.CreateEntitlementBinding(ctx, &store.EntitlementBinding{ID: f.entitlementUD, LimitDefinitionID: f.limitUD, SubjectType: "user", SubjectID: f.user, ScopeType: "system", Value: 5, CreatedBy: f.user, CreatedAt: now, UpdatedAt: now})
	require.NoError(t, err)

	require.NoError(t, s.CreateGCPServiceAccount(ctx, &store.GCPServiceAccount{ID: f.gcpSA, Scope: store.ScopeHub, ScopeID: "li-hub", Email: "li-gcp-sa@li.iam.gserviceaccount.com", ProjectID: "li-gcp-project", CreatedBy: f.user, CreatedAt: now}))
	require.NoError(t, s.CreateGCPServiceAccount(ctx, &store.GCPServiceAccount{ID: f.gcpSADel, Scope: store.ScopeHub, ScopeID: "li-hub", Email: "li-gcp-sa-del@li.iam.gserviceaccount.com", ProjectID: "li-gcp-project", CreatedBy: f.user, CreatedAt: now}))

	// Scoped to a specific, otherwise-unused principal (never "all_principals"
	// or a group the dev super-admin belongs to): an access constraint is a
	// live authorization restriction, and one that applied hub-wide would
	// poison every other probe in this test, not just the constraint's own
	// entry points.
	// CreateAccessConstraint (access_constraint_store.go) does not honor a
	// caller-supplied ID; the store always mints its own. Capture the
	// returned ID rather than the one this test asked for.
	acSubjectType := "user"
	acSubjectID := f.userDel
	ac, err := s.CreateAccessConstraint(ctx, &store.AccessConstraint{Name: "li-ac", SubjectKind: "principal", SubjectPrincipalType: &acSubjectType, SubjectPrincipalID: &acSubjectID, ScopeType: "system", MaximumPermissions: []string{"agent.read"}, Purpose: "live-inventory test fixture", CreatedBy: f.user, CreatedAt: now, UpdatedAt: now})
	require.NoError(t, err)
	f.accessConstraint = ac.ID
	acUD, err := s.CreateAccessConstraint(ctx, &store.AccessConstraint{Name: "li-ac-ud", SubjectKind: "principal", SubjectPrincipalType: &acSubjectType, SubjectPrincipalID: &acSubjectID, ScopeType: "system", MaximumPermissions: []string{"agent.read"}, Purpose: "live-inventory test fixture (update+delete)", CreatedBy: f.user, CreatedAt: now, UpdatedAt: now})
	require.NoError(t, err)
	f.accessConstraintUD = acUD.ID

	require.NoError(t, s.CreateInviteCode(ctx, &store.InviteCode{ID: f.invite, CodeHash: "li-invite-hash", CodePrefix: "li-invite", MaxUses: 1, ExpiresAt: now.Add(24 * time.Hour), CreatedBy: f.user, Created: now}))

	require.NoError(t, s.AddAllowListEntry(ctx, &store.AllowListEntry{ID: tid("li-allowlist"), Email: f.allowListEmail, AddedBy: f.user, Created: now}))
	// DELETE /admin/allow-list/{email} (deprecated) actually deletes the
	// User(invited) record for that email (admin_allow_list.go:118-126), not
	// the AllowListEntry row above — it needs both to exist.
	require.NoError(t, s.CreateUser(ctx, &store.User{ID: tid("li-allowlist-user"), Email: f.allowListEmail, DisplayName: "LI Allowlist Invitee", Role: "member", Status: store.UserStatusInvited}))

	// scope=user with no explicit scopeId query param resolves to the
	// caller's own user ID (resolveEnvSecretAccess, handlers_env_secrets.go),
	// not an arbitrary user — the caller here is always the dev super-admin.
	require.NoError(t, s.CreateEnvVar(ctx, &store.EnvVar{ID: tid("li-envvar"), Key: f.envVarKey, Value: "1", Scope: store.ScopeUser, ScopeID: DevUserID, Created: now, Updated: now}))

	expiry := now.Add(24 * time.Hour)
	// UserID must be the dev caller (DevUserID), not f.user: RevokeToken and
	// DeleteToken (handlers_auth.go) look up the token by (caller's user ID,
	// token ID), so a token owned by any other user 404s regardless of
	// method or path correctness.
	require.NoError(t, s.CreateUserAccessToken(ctx, &store.UserAccessToken{ID: f.uatRevokePost, UserID: DevUserID, Name: "li-uat-revoke-post", Prefix: "li_p", KeyHash: "li-uat-revoke-post-key-hash", ProjectID: f.project, Scopes: []string{"project:read"}, ExpiresAt: &expiry, Created: now}))
	require.NoError(t, s.CreateUserAccessToken(ctx, &store.UserAccessToken{ID: f.uatRevokeDelete, UserID: DevUserID, Name: "li-uat-revoke-delete", Prefix: "li_d", KeyHash: "li-uat-revoke-delete-key-hash", ProjectID: f.project, Scopes: []string{"project:read"}, ExpiresAt: &expiry, Created: now}))

	require.NoError(t, s.CreateRuntimeBroker(ctx, &store.RuntimeBroker{ID: f.runtimeBroker, Name: "li-broker", Slug: "li-broker", Status: "offline", ConnectionState: "disconnected"}))

	require.NoError(t, s.CreateGitHubInstallation(ctx, &store.GitHubInstallation{InstallationID: 900000001, AccountLogin: "li-account", AccountType: "Organization", AppID: 1, Status: store.GitHubInstallationStatusActive, CreatedAt: now, UpdatedAt: now}))

	_, err = s.CreateRoleDefinition(ctx, &store.RoleDefinition{ID: f.roleDefinition, Name: "li-role", Description: "live inventory test role", ScopeType: store.RoleScopeSystem, Permissions: []string{"agent.read"}, CreatedAt: now, UpdatedAt: now})
	require.NoError(t, err)
	rdDel, err := s.CreateRoleDefinition(ctx, &store.RoleDefinition{ID: f.roleDefinitionDel, Name: "li-role-del", Description: "live inventory test role (delete)", ScopeType: store.RoleScopeSystem, Permissions: []string{"agent.read"}, CreatedAt: now, UpdatedAt: now})
	require.NoError(t, err)
	_, err = s.CreateRoleBinding(ctx, &store.RoleBinding{ID: f.roleBindingDel, RoleDefinitionID: rdDel.ID, PrincipalType: store.RoleBindingPrincipalUser, PrincipalID: f.userDel, ScopeType: store.RoleScopeSystem, CreatedBy: f.user, CreatedAt: now})
	require.NoError(t, err)

	// project.membership.update / .remove address a role binding by ID, so
	// they need a real project-scoped binding rather than a fake ID: a
	// binding on the seeded "project-member" role, scoped to f.project.
	projectMemberRole, err := s.GetRoleDefinitionByName(ctx, "project-member", store.RoleScopeProject)
	require.NoError(t, err)
	pm, err := s.CreateRoleBinding(ctx, &store.RoleBinding{RoleDefinitionID: projectMemberRole.ID, PrincipalType: store.RoleBindingPrincipalUser, PrincipalID: f.member, ScopeType: store.RoleScopeProject, ScopeID: f.project, CreatedBy: f.user, CreatedAt: now})
	require.NoError(t, err)
	f.projectMembership = pm.ID

	// Maintenance operations and migrations are seeded once, by key, during
	// Migrate() (SeedMaintenanceOperations) — they are a fixed built-in
	// registry, not something a test creates. These are real, currently
	// seeded keys used only for the GET-by-key entry (never for /run, which
	// is excluded above).
	f.maintenanceOpKey = "pull-images"
	f.maintenanceMigrationKey = "secret-hub-id-migration"

	return f
}

// patternOverrides maps a literal EntryPoint Pattern to the placeholder
// values it needs, built once real fixture IDs are known. A pattern absent
// from this map is substituted with the generic placeholder for every
// "{name}" segment, which is correct for every entry whose live handler
// dispatches on method (and, where applicable, resolves its target) without
// an existence check that runs before the method switch.
func patternOverrides(f idFixtures) map[string]map[string]string {
	return map[string]map[string]string{
		// --- agent family ---
		"/api/v1/agents/{id}":                                       {"id": f.agent},
		"/api/v1/agents/{id}/ports":                                 {"id": f.agent},
		"/api/v1/agents/{id}/ports/{port}/proxy":                    {"id": f.agent, "port": f.agentPort},
		"/api/v1/agents/{id}/ports/{port}/proxy/{subpath}":          {"id": f.agent, "port": f.agentPort, "subpath": "x"},
		"/api/v1/agents/{id}/set_message_mode":                      {"id": f.agent},
		"/api/v1/projects/{projectId}/agents/{id}/set_message_mode": {"projectId": f.project, "id": f.agent},

		// --- project family ---
		"/api/v1/projects/{id}":                    {"id": f.project},
		"/api/v1/projects/{id}/members":            {"id": f.project},
		"/api/v1/projects/{id}/members/{memberId}": {"id": f.project, "memberId": f.projectMembership},
		"/api/v1/projects/{id}/transfer-ownership": {"id": f.project},

		// --- group family ---
		"/api/v1/groups/{id}":                                 {"id": f.group},
		"/api/v1/groups/{id}/members":                         {"id": f.group},
		"/api/v1/groups/{id}/members/{memberType}/{memberId}": {"id": f.group, "memberType": "user", "memberId": f.member},

		// --- user family ---
		"/api/v1/users/{id}": {"id": f.user},

		// --- skill family ---
		"/api/v1/skills/{id}": {"id": f.skill},

		// --- skill registry family ---
		"/api/v1/skill-registries/{id}": {"id": f.skillRegistry},

		// --- template family ---
		"/api/v1/templates/{id}": {"id": f.template},

		// --- harness config family ---
		"/api/v1/harness-configs/{id}": {"id": f.harnessConfig},

		// --- schedule family ---
		"/api/v1/projects/{projectId}/scheduled-events/{id}": {"projectId": f.project, "id": f.scheduledEvent},
		"/api/v1/projects/{projectId}/schedules/{id}":        {"projectId": f.project, "id": f.schedule},
		"/api/v1/projects/{projectId}/scheduled-events":      {"projectId": f.project},
		"/api/v1/projects/{projectId}/schedules":             {"projectId": f.project},

		// --- quota family ---
		"/api/v1/admin/limits/{id}":              {"id": f.limit},
		"/api/v1/admin/limits/{id}/entitlements": {"id": f.limit},
		"/api/v1/admin/entitlements/{id}":        {"id": f.entitlement},
		"/api/v1/admin/usage/{limit}":            {"limit": f.limit},

		// --- GCP identity family ---
		"/api/v1/gcp-service-accounts/{id}":        {"id": f.gcpSA},
		"/api/v1/gcp-service-accounts/{id}/verify": {"id": f.gcpSA},

		// --- access constraint family ---
		"/api/v1/admin/access-constraints/{id}": {"id": f.accessConstraint},

		// --- credential token family ---
		"/api/v1/auth/tokens/{id}/revoke": {"id": f.uatRevokePost},
		"/api/v1/auth/tokens/{id}":        {"id": f.uatRevokeDelete},

		// --- invite family ---
		"/api/v1/admin/invites/{id}":       {"id": f.invite},
		"/api/v1/admin/allow-list/{email}": {"email": f.allowListEmail},

		// --- env var family ---
		"/api/v1/env/{key}": {"key": f.envVarKey},

		// --- runtime broker family ---
		"/api/v1/runtime-brokers/{id}": {"id": f.runtimeBroker},

		// --- github app family ---
		"/api/v1/github-app/installations/{id}": {"id": f.githubInstallationID},

		// --- role family ---
		"/api/v1/admin/roles/{id}":           {"id": f.roleDefinition},
		"/api/v1/admin/roles/{id}/export":    {"id": f.roleDefinition},
		"/api/v1/admin/roles/{id}/duplicate": {"id": f.roleDefinition},
		"/api/v1/admin/role-bindings/{id}":   {"id": f.roleBindingDel},

		// --- maintenance family (read-by-key only; /run is excluded above) ---
		"/api/v1/admin/maintenance/operations/{id}":     {"id": f.maintenanceOpKey},
		"/api/v1/admin/maintenance/migrations/{id}/run": {"id": f.maintenanceMigrationKey},

		// --- chat family ---
		"/api/v1/chat/spaces/{id}/threads": {"id": f.project},
	}
}

// deleteVerbFixups re-points the delete-shaped catalog entries at the
// disposable instance of their resource family, so exercising the real
// DELETE (or, for role.definition.update, a real update-then-delete
// sequence) cannot remove the fixture an earlier- or later-declared
// non-destructive entry for the same family still needs. Keyed by
// (OperationID, Pattern) since some operations share a pattern across
// methods (all of which should target the same disposable instance).
func deleteVerbFixups(f idFixtures) map[string]map[string]string {
	return map[string]map[string]string{
		"agent.lifecycle.delete":   {"id": f.agentDel},
		"project.lifecycle.delete": {"id": f.projectDel},
		"group.delete":             {"id": f.groupDel},
		"user.admin.delete":        {"id": f.userDel},
		"skill.delete":             {"id": f.skillDel},
		"template.delete":          {"id": f.templateDel},
		"harnessconfig.delete":     {"id": f.harnessConfigDel},
		"gcp.identity.delete":      {"id": f.gcpSADel},
		"role.definition.update":   {"id": f.roleDefinition},
		"role.definition.delete":   {"id": f.roleDefinitionDel},
		"role.binding.delete":      {"id": f.roleBindingDel},
	}
}

func quotaUpdateDeleteFixups(f idFixtures) map[string]map[string]string {
	return map[string]map[string]string{
		"/api/v1/admin/limits/{id}":       {"id": f.limitUD},
		"/api/v1/admin/entitlements/{id}": {"id": f.entitlementUD},
	}
}

func accessConstraintUpdateDeleteFixup(f idFixtures) map[string]string {
	return map[string]string{"id": f.accessConstraintUD}
}

// substituteLiveInventoryParams replaces every "{name}" placeholder in an
// authzop EntryPoint pattern with a caller-supplied override for that name,
// or a fixed, syntactically valid generic placeholder otherwise, so the
// resulting path can be dispatched through the real server mux.
func substituteLiveInventoryParams(pattern string, overrides map[string]string) string {
	var b strings.Builder
	for i := 0; i < len(pattern); {
		if pattern[i] == '{' {
			end := strings.IndexByte(pattern[i:], '}')
			if end < 0 {
				b.WriteString(pattern[i:])
				break
			}
			name := pattern[i+1 : i+end]
			if v, ok := overrides[name]; ok {
				b.WriteString(v)
			} else {
				b.WriteString("live-inventory-placeholder")
			}
			i += end + 1
			continue
		}
		b.WriteByte(pattern[i])
		i++
	}
	return b.String()
}

// catalogHTTPEntryPoints returns every (operationID, EntryPoint) pair in
// authzop.Catalog whose Kind is EntryPointHTTPRoute, in catalog declaration
// order. Non-HTTP kinds (WebSocket, SSE, BrokerCall, SchedulerJob,
// CLICommand, BackgroundJob, InternalDispatch) are out of scope for a live
// HTTP method/path check by construction — they either have no HTTP surface
// at all, or (SSE/WebSocket) are already pinned by dedicated tests in
// authzop/drift_test.go.
func catalogHTTPEntryPoints() []struct {
	OperationID string
	EntryPoint  authzop.EntryPoint
} {
	var out []struct {
		OperationID string
		EntryPoint  authzop.EntryPoint
	}
	for _, spec := range authzop.Catalog {
		for _, ep := range spec.EntryPoints {
			if ep.Kind != authzop.EntryPointHTTPRoute {
				continue
			}
			out = append(out, struct {
				OperationID string
				EntryPoint  authzop.EntryPoint
			}{OperationID: string(spec.ID), EntryPoint: ep})
		}
	}
	return out
}

// bogusMethod is an HTTP method no route in this codebase declares or
// accepts, used as the R2 control: if a route dispatches on method the way
// its handler is supposed to, this method must be rejected with 405.
const bogusMethod = "PROPFIND"

// TestCatalogHTTPEntryPoints_LiveMethodCheck probes the real server mux for
// every declared HTTP entry point in authzop.Catalog (skipping the small,
// reviewed exclusion maps above) and makes two assertions per entry:
//
//  1. Positive check: sending the catalog's declared method at a path built
//     from the declared pattern, with real fixture IDs substituted wherever
//     the live handler needs one to exist, must not return 404 or 405. 404
//     means the declared path does not reach the resource the operation
//     addresses; 405 means the declared method is wrong.
//  2. Control check: sending an unsupported method (bogusMethod) at the same
//     path must return 405. This proves the positive check actually
//     reached the handler's method dispatch — without it, a handler that
//     404s or errors before ever looking at r.Method would make the
//     positive check pass vacuously regardless of what method the catalog
//     declares.
//
// Together, an entry can only pass both checks if changing its declared
// method (positive check fails: no longer routes) or its declared path
// (control check would have already flagged an unreachable dispatch, or the
// positive check 404s once the path stops matching the fixture) breaks it —
// which is #2227's AC2 ("a catalog entry whose method or path differs from
// its route fails a test").
func TestCatalogHTTPEntryPoints_LiveMethodCheck(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	f := seedLiveInventoryFixtures(t, ctx, s)
	overrides := patternOverrides(f)
	deleteFixups := deleteVerbFixups(f)
	quotaFixups := quotaUpdateDeleteFixups(f)
	acFixup := accessConstraintUpdateDeleteFixup(f)

	tested, controlChecked := 0, 0
	for _, entry := range catalogHTTPEntryPoints() {
		ep := entry.EntryPoint
		key := liveInventoryKey{OperationID: entry.OperationID, Method: ep.Method, Pattern: ep.Pattern}

		params := overrides[ep.Pattern]
		if fixup, ok := deleteFixups[entry.OperationID]; ok {
			params = fixup
		}
		// access.constraint.update / .delete and quota.update / .delete
		// share their pattern with a longer-lived read entry, so they are
		// re-pointed by operation+pattern rather than by pattern alone.
		if entry.OperationID == "access.constraint.update" || entry.OperationID == "access.constraint.delete" {
			params = acFixup
		}
		if entry.OperationID == "quota.update" || entry.OperationID == "quota.delete" || entry.OperationID == "quota.create" {
			if fixup, ok := quotaFixups[ep.Pattern]; ok {
				params = fixup
			}
		}

		path := substituteLiveInventoryParams(ep.Pattern, params)

		var body interface{}
		switch ep.Method {
		case http.MethodPost, http.MethodPut, http.MethodPatch:
			body = map[string]interface{}{}
		}

		// Control check runs before the positive check: for a single-use
		// destructive entry (a DELETE that only has one real fixture
		// instance), running the positive check first would consume the
		// fixture and make the control check's follow-up request 404
		// regardless of method, defeating the control. Order doesn't matter
		// for non-destructive entries, so running control first uniformly
		// is safe.
		if reason, excluded := controlCheckExclusions[key]; excluded {
			t.Logf("control check skipped for %s %s (operation %s): %s", ep.Method, path, entry.OperationID, reason)
		} else {
			controlRec := doRequest(t, srv, bogusMethod, path, nil)
			controlChecked++
			if controlRec.Code != http.StatusMethodNotAllowed {
				t.Errorf("%s %s (operation %s): sending %s got %d, want 405 — the probe for this entry never reaches method dispatch, so its declared-method result proves nothing",
					ep.Method, path, entry.OperationID, bogusMethod, controlRec.Code)
			}
		}

		if reason, excluded := positiveCheckExclusions[key]; excluded {
			t.Logf("positive check skipped for %s %s (operation %s): %s", ep.Method, path, entry.OperationID, reason)
			continue
		}
		rec := doRequest(t, srv, ep.Method, path, body)
		tested++
		switch rec.Code {
		case http.StatusMethodNotAllowed:
			t.Errorf("%s %s (operation %s): got 405 Method Not Allowed — the catalog declares a method the live route does not accept",
				ep.Method, path, entry.OperationID)
		case http.StatusNotFound:
			t.Errorf("%s %s (operation %s): got 404 Not Found — the catalog's declared path does not reach a live route for this operation",
				ep.Method, path, entry.OperationID)
		}
	}

	if tested == 0 {
		t.Fatal("no HTTP catalog entry points were exercised by the positive check — this test is broken")
	}
	if controlChecked == 0 {
		t.Fatal("no HTTP catalog entry points were exercised by the control check — this test is broken")
	}
	t.Logf("live method/path check: %d positive checks, %d control checks, %d positive exclusions, %d control exclusions",
		tested, controlChecked, len(positiveCheckExclusions), len(controlCheckExclusions))
}

// TestLiveInventoryExclusionsNotStale asserts every entry in
// positiveCheckExclusions and controlCheckExclusions still names a real,
// currently declared catalog HTTP entry point. A stale exclusion — left
// behind after a catalog correction changes or removes the entry point it
// names — would silently stop meaning anything and hide the entry from
// live-inventory coverage for no reason.
func TestLiveInventoryExclusionsNotStale(t *testing.T) {
	live := make(map[liveInventoryKey]bool)
	for _, entry := range catalogHTTPEntryPoints() {
		live[liveInventoryKey{OperationID: entry.OperationID, Method: entry.EntryPoint.Method, Pattern: entry.EntryPoint.Pattern}] = true
	}
	for key := range positiveCheckExclusions {
		if !live[key] {
			t.Errorf("stale positive-check exclusion: operation %q method %q pattern %q does not match any current catalog HTTP entry point",
				key.OperationID, key.Method, key.Pattern)
		}
	}
	for key := range controlCheckExclusions {
		if !live[key] {
			t.Errorf("stale control-check exclusion: operation %q method %q pattern %q does not match any current catalog HTTP entry point",
				key.OperationID, key.Method, key.Pattern)
		}
	}
}
