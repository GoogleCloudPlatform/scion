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
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/artifacts"
	"github.com/GoogleCloudPlatform/scion/pkg/hub/authzop"
	"github.com/GoogleCloudPlatform/scion/pkg/storage"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/google/uuid"
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
// effect — never merely because a fixture would be inconvenient. Because the
// side effect risk applies to any real dispatch of the declared method, not
// just the un-suffixed one, the suffix check is also skipped for every key
// here (logged with this same reason): both checks that send the declared
// method are skipped, and only the control check (which never sends it)
// still runs.
// TestLiveInventoryExclusionsNotStale asserts every key here still names a
// real, currently-declared catalog HTTP entry point.
var positiveCheckExclusions = map[liveInventoryKey]string{
	{OperationID: "hub.maintenance.execute", Method: "POST", Pattern: "/api/v1/admin/maintenance/restart"}:       "handleAdminRestart (admin_maintenance.go) invokes a real systemd restart subprocess; nothing before it short-circuits for a fake or real target, so there is no safe way to dispatch the declared method",
	{OperationID: "hub.maintenance.execute", Method: "POST", Pattern: "/api/v1/admin/maintenance/check-updates"}: "handleCheckForUpdates (admin_maintenance.go:662-690) calls the GitHub release channel when MaintenanceConfig.DeploymentTier == \"binary\"; excluded so this test cannot depend on, or accidentally call out based on, server config",
}

// controlCheckExclusions lists HTTP catalog entry points for which
// TestCatalogHTTPEntryPoints_LiveMethodCheck does not require "an
// unsupported method must return 405". Each reason names the specific
// dispatch shape that makes a 405 control meaningless for that entry — not
// merely that the control happens to fail today.
var controlCheckExclusions = map[liveInventoryKey]string{
	{OperationID: "agent.portaccess", Method: "GET", Pattern: "/api/v1/agents/{id}/ports/{port}/proxy"}:                      "proxyAgentPort forwards every HTTP method to the tunnel with no method-based routing at all (see the entry's own comment in catalog.go); there is no unsupported method to control against",
	{OperationID: "agent.portaccess", Method: "POST", Pattern: "/api/v1/agents/{id}/ports/{port}/proxy"}:                     "same as the GET .../proxy entry above: proxyAgentPort accepts every method by design",
	{OperationID: "agent.portaccess", Method: "PUT", Pattern: "/api/v1/agents/{id}/ports/{port}/proxy"}:                      "same as the GET .../proxy entry above: proxyAgentPort accepts every method by design",
	{OperationID: "agent.portaccess", Method: "DELETE", Pattern: "/api/v1/agents/{id}/ports/{port}/proxy"}:                   "same as the GET .../proxy entry above: proxyAgentPort accepts every method by design",
	{OperationID: "agent.portaccess", Method: "GET", Pattern: "/api/v1/agents/{id}/ports/{port}/proxy/{subpath}"}:            "same as the GET .../proxy entry above: proxyAgentPort accepts every method by design",
	{OperationID: "agent.secrets.access", Method: "GET", Pattern: "/api/v1/agents/{id}/secrets"}:                             "handleAgentSecrets (handlers_env_secrets.go) answers 501 when no secrets backend is configured, before it looks at the method or the key; testServer configures no secrets backend, so every method gets the same 501",
	{OperationID: "agent.secrets.access", Method: "GET", Pattern: "/api/v1/agents/{id}/secrets/{key}"}:                       "same as the GET .../secrets control entry above",
	{OperationID: "agent.secrets.access", Method: "PUT", Pattern: "/api/v1/agents/{id}/secrets/{key}"}:                       "same as the GET .../secrets control entry above",
	{OperationID: "hub.policies.removed", Method: "GET", Pattern: "/api/v1/policies"}:                                        "handlePolicies (handlers_policies.go) answers 410 Gone for every method by design; there is no unsupported method to control against",
	{OperationID: "hub.policies.removed", Method: "GET", Pattern: "/api/v1/policies/{id}"}:                                   "handlePolicyRoutes (handlers_policies.go) answers 410 Gone for every method by design, the same as the GET /api/v1/policies entry above",
	{OperationID: "user.session.logout", Method: "POST", Pattern: "/api/v1/auth/logout"}:                                     "every method on /api/v1/auth/logout gets the same route-guard answer, so there is no unsupported method to control against",
	{OperationID: "broker.messagefailures.report", Method: "POST", Pattern: "/api/v1/runtime-brokers/{id}/message-failures"}: "handleRuntimeBrokerByIDInternal (handlers_runtime_brokers.go) matches subPath==\"message-failures\" && method==POST as a single condition; any other method falls through to the \"RuntimeBroker resource\" 404, never a 405",
	{OperationID: "gcp.identity.verify", Method: "POST", Pattern: "/api/v1/gcp-service-accounts/{id}/verify"}:                "handleGCPServiceAccountByID (handlers_gcp_identity_scoped.go:297-304) matches on action==\"verify\" && method==POST as a single condition; any other method on the same action falls through to the generic \"action not found\" 404, never a 405",
}

// suffixCheckExclusions lists HTTP catalog entry points for which
// TestCatalogHTTPEntryPoints_LiveMethodCheck does not require "the declared
// method at pattern + '/<bogus-segment>' returns 404 or 405" (the third,
// path-suffix check — see the doc comment on the test). Each entry names the
// live handler branch that accepts, or ignores, a trailing path segment the
// catalog pattern does not declare, discovered by actually probing it, not
// by assumption. This check is intentionally narrower than "no route should
// ever accept an undeclared suffix" — see the test's doc comment for what it
// does and does not prove.
var suffixCheckExclusions = map[liveInventoryKey]string{
	{OperationID: "user.read", Method: "GET", Pattern: "/api/v1/users/{id}"}:                                               "handleUserByID (handlers_users_core.go) only special-cases the \"revoke-sessions\" suffix; any other suffix, including this check's bogus one, falls through to the same GET handling as the bare ID",
	{OperationID: "user.update", Method: "PATCH", Pattern: "/api/v1/users/{id}"}:                                           "same as user.read above: handleUserByID ignores an unrecognized suffix rather than 404ing on it",
	{OperationID: "user.admin.delete", Method: "DELETE", Pattern: "/api/v1/users/{id}"}:                                    "same as user.read above: handleUserByID ignores an unrecognized suffix rather than 404ing on it",
	{OperationID: "secret.read", Method: "GET", Pattern: "/api/v1/secrets/{key}"}:                                          "handleSecretByKey (handlers_env_secrets.go) extracts the key with extractID, which truncates at the first '/' after the prefix and discards the suffix, so the request proceeds on the bare key; with no secret backend configured in testServer, getSecret then panics on a nil backend, recovered as a 500 — not a 404/405, but unrelated to the suffix, which the truncation already removed",
	{OperationID: "secret.write", Method: "PUT", Pattern: "/api/v1/secrets/{key}"}:                                         "extractID truncates the suffix the same way as secret.read above, but setSecret checks the request body's value field before ever touching the secret backend; this test's generic empty PUT body always fails that check with 400 \"value is required\", the same 400 the bare path gets",
	{OperationID: "secret.write", Method: "DELETE", Pattern: "/api/v1/secrets/{key}"}:                                      "extractID truncates the suffix the same way as secret.read above; deleteSecret then panics on a nil secret backend the same way getSecret does, recovered as a 500",
	{OperationID: "hub.githubapp.update", Method: "PUT", Pattern: "/api/v1/github-app/installations/{id}"}:                 "parseInstallationIDFromPath (handlers_github_app.go) runs strconv.ParseInt on the entire remainder of the path, not just a leading segment, so a suffix makes parsing fail; the handler answers 400 \"invalid installation ID\", rejecting the suffix rather than ignoring it",
	{OperationID: "hub.githubapp.update", Method: "DELETE", Pattern: "/api/v1/github-app/installations/{id}"}:              "same as the PUT installations/{id} entry above",
	{OperationID: "hub.githubapp.read", Method: "GET", Pattern: "/api/v1/github-app/installations/{id}"}:                   "same as the PUT installations/{id} entry above",
	{OperationID: "quota.read", Method: "GET", Pattern: "/api/v1/admin/entitlements/{id}"}:                                 "handleAdminEntitlementByID (handlers_quota.go) extracts the ID with extractID, which truncates at the first '/' after the prefix and discards the suffix, so the request proceeds on the real, un-suffixed ID and returns the entitlement (200) exactly as if the suffix were absent",
	{OperationID: "quota.update", Method: "PUT", Pattern: "/api/v1/admin/entitlements/{id}"}:                               "same truncation as the GET entitlements/{id} entry above; this test's generic empty PUT body then fails updateEntitlement's validation with 400 before the (correctly extracted) ID is used for anything — the same 400 the bare path gets",
	{OperationID: "quota.delete", Method: "DELETE", Pattern: "/api/v1/admin/entitlements/{id}"}:                            "same truncation as the GET entitlements/{id} entry above; deleteEntitlement then deletes the real, un-suffixed entitlement (204) exactly as if the suffix were absent",
	{OperationID: "quota.read", Method: "GET", Pattern: "/api/v1/admin/limits/{id}"}:                                       "handleAdminLimitByID (handlers_quota.go) splits on the first '/' and only special-cases a second segment of exactly \"entitlements\"; any other suffix is silently discarded, and the request proceeds on the real, un-suffixed ID",
	{OperationID: "quota.update", Method: "PUT", Pattern: "/api/v1/admin/limits/{id}"}:                                     "handleAdminLimitByID discards the suffix the same way as quota.read above; with a valid update body (see bodyOverrides) updateLimitDefinition succeeds on the real, un-suffixed ID and returns 200 exactly as if the suffix were absent",
	{OperationID: "quota.delete", Method: "DELETE", Pattern: "/api/v1/admin/limits/{id}"}:                                  "handleAdminLimitByID discards the suffix the same way as quota.read above; deleteLimitDefinition then fails deleting a limit this test's fixture entitlement still references, mapped to 400 — the same 400 happens on the bare path, independent of the suffix",
	{OperationID: "secret.read", Method: "GET", Pattern: "/api/v1/secrets"}:                                                "the by-key route \"/api/v1/secrets/\" registers as a prefix, so a suffix on this bare collection route falls through to handleSecretByKey with the suffix as the key; getSecret then panics on a nil secret backend, recovered as a 500",
	{OperationID: "hub.lifecyclehooks.read", Method: "GET", Pattern: "/api/v1/admin/lifecycle-hooks"}:                      "the by-ID route \"/api/v1/admin/lifecycle-hooks/\" registers as a prefix, so a suffix on this bare collection route falls through to handleAdminLifecycleHookByID with the suffix as the ID; getLifecycleHook then 400s failing to parse it as a UUID",
	{OperationID: "hub.githubapp.read", Method: "GET", Pattern: "/api/v1/github-app/installations"}:                        "the by-ID route \"/api/v1/github-app/installations/\" registers as a prefix, so a suffix on this bare collection route falls through to handleGitHubAppInstallationByIDRead with the suffix as the ID; parseInstallationIDFromPath then 400s \"invalid installation ID\" failing to parse it as an integer",
	{OperationID: "agent.stopall", Method: "POST", Pattern: "/api/v1/agents/stop-all"}:                                     "handleAgentByID checks id == \"stop-all\" before it looks at anything after that segment, so a suffix is silently ignored and stop-all runs exactly as it would on the bare path",
	{OperationID: "quota.read", Method: "GET", Pattern: "/api/v1/admin/usage/{limit}"}:                                     "handleAdminUsageByLimit (handlers_quota.go) extracts limitID with extractID, which truncates at the first '/' and discards everything after it, so the suffix never reaches the lookup",
	{OperationID: "role.binding.read", Method: "GET", Pattern: "/api/v1/admin/role-bindings/user/{userId}"}:                "handleAdminRoleBindingByID's \"user/\" branch (handlers_roles.go) takes the entire remaining path as the user ID with no further splitting; a nonexistent literal ID, suffixed or not, just returns an empty binding list (200), never a 404",
	{OperationID: "env.read", Method: "GET", Pattern: "/api/v1/env/{key}"}:                                                 "handleEnvVarByKey (handlers_env_secrets.go) extracts the key with extractID, which truncates at the first '/' and discards everything after it, so the suffix never reaches the lookup",
	{OperationID: "hub.lifecyclehooks.read", Method: "GET", Pattern: "/api/v1/admin/lifecycle-hooks/{id}"}:                 "handleAdminLifecycleHookByID (handlers_lifecycle_hooks.go) extracts the ID with extractID, which truncates at the first '/' and discards everything after it, so the suffix never reaches getLifecycleHook's lookup",
	{OperationID: "group.member.remove", Method: "DELETE", Pattern: "/api/v1/groups/{id}/members/{memberType}/{memberId}"}: "handleGroupMemberByID (handlers_groups.go) splits memberPath into at most two parts, so a trailing suffix is appended onto memberID as one string rather than forming a separate segment; the resulting lookup fails with 400, not a routing 404",
	{OperationID: "hub.config.update", Method: "DELETE", Pattern: "/api/v1/admin/server-config/sections/{id}"}:             "handleAdminServerConfigSectionReset (admin_settings.go) requires OperationalSettings and 400s \"Section reset requires DB-backed operational settings\" before it ever parses the section name from the path; testServer wires no OperationalSettings, so the same 400 happens on the bare path, independent of the suffix",
	{OperationID: "hub.maintenance.execute", Method: "POST", Pattern: "/api/v1/admin/maintenance/operations/{id}/run"}:     "handleAdminMaintenanceOps (admin_maintenance.go) splits the sub-path into at most three parts, so a fourth segment is absorbed into the \"run\" branch's own remainder rather than changing dispatch; combined with this entry's deliberate cross-category key (see patternOverrides), the resulting 400 is the same category-mismatch rejection as the bare path",
	{OperationID: "hub.metrics.read", Method: "GET", Pattern: "/api/v1/metrics/{name}"}:                                    "the metrics dashboard is not configured in testServer (no telemetry project ID); the resulting pre-dispatch 503 fires before path structure is examined, the same limitation the positive check documents in the test's doc comment",
	{OperationID: "chat.access", Method: "GET", Pattern: "/api/v1/chat/attachments/{id}"}:                                  "handleAttachmentDownload needs an attachment storage backend testServer does not configure; the resulting pre-dispatch 503 fires regardless of the suffix",
	{OperationID: "agent.portaccess", Method: "GET", Pattern: "/api/v1/agents/{id}/ports/{port}/proxy"}:                    "proxyAgentPort forwards the entire remaining path as part of the proxied request, with no path-based routing at all — the same design already excluded from the control check above",
	{OperationID: "agent.portaccess", Method: "POST", Pattern: "/api/v1/agents/{id}/ports/{port}/proxy"}:                   "same as the GET .../proxy suffix entry above",
	{OperationID: "agent.portaccess", Method: "PUT", Pattern: "/api/v1/agents/{id}/ports/{port}/proxy"}:                    "same as the GET .../proxy suffix entry above",
	{OperationID: "agent.portaccess", Method: "DELETE", Pattern: "/api/v1/agents/{id}/ports/{port}/proxy"}:                 "same as the GET .../proxy suffix entry above",
	{OperationID: "agent.portaccess", Method: "GET", Pattern: "/api/v1/agents/{id}/ports/{port}/proxy/{subpath}"}:          "same as the GET .../proxy suffix entry above",
	{OperationID: "agent.secrets.access", Method: "GET", Pattern: "/api/v1/agents/{id}/secrets"}:                           "handleAgentSecrets (handlers_env_secrets.go) answers 501 when no secrets backend is configured, before it looks at the key; testServer configures no secrets backend, so a suffixed path gets the same 501 as the bare one",
	{OperationID: "agent.secrets.access", Method: "GET", Pattern: "/api/v1/agents/{id}/secrets/{key}"}:                     "same as the GET .../secrets suffix entry above",
	{OperationID: "agent.secrets.access", Method: "PUT", Pattern: "/api/v1/agents/{id}/secrets/{key}"}:                     "same as the GET .../secrets suffix entry above",
	{OperationID: "hub.policies.removed", Method: "GET", Pattern: "/api/v1/policies"}:                                      "the by-ID route \"/api/v1/policies/\" registers as a prefix, so a suffix on this bare collection route reaches handlePolicyRoutes, which answers 410 Gone for every sub-path by design",
	{OperationID: "hub.policies.removed", Method: "GET", Pattern: "/api/v1/policies/{id}"}:                                 "handlePolicyRoutes (handlers_policies.go) answers 410 Gone for every sub-path by design, so a suffixed path gets the same 410 as the bare one",
	{OperationID: "user.admin.invite", Method: "GET", Pattern: "/api/v1/admin/invites/{id}"}:                               "handleAdminInviteByID (admin_invites.go:65-96) special-cases only a second segment of \"revoke\"; any other suffix, including this check's bogus one, falls through to the same GET handling as the bare ID",
	{OperationID: "user.admin.invite", Method: "DELETE", Pattern: "/api/v1/admin/invites/{id}"}:                            "same as the GET invites/{id} suffix entry above — and because the suffix is silently ignored, a DELETE with a bogus suffix would delete the real fixture, so this exclusion also protects the positive check that runs after it",
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
	projectMembershipUpdate string
	projectMemberRoleID     string
	allowListEmail          string
	maintenanceOpKey        string
	maintenanceMigrationKey string
	integrationName         string
	lifecycleHook           string
	chatTopic               string
	agentLifecycle          string
	agentRestore            string
	agentRestoreProject     string
	agentProvisioning       string
	artifact                string
}

// seedLiveInventoryFixtures creates one real store row per resource family
// (plus a second, disposable instance for families with a destructive
// catalog operation, so a DELETE entry tested earlier in catalog
// declaration order cannot remove the fixture a later read/update entry
// still needs) and returns their IDs. It never mints a real credential and
// never calls out to any external service — everything it creates lives
// only in the test's in-memory SQLite store.
func seedLiveInventoryFixtures(t *testing.T, ctx context.Context, srv *Server, s store.Store) idFixtures {
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
		integrationName:      "telegram",
		lifecycleHook:        tid("li-lifecycle-hook"),
		chatTopic:            tid("li-chat-topic"),
		agentLifecycle:       tid("li-agent-lifecycle"),
		agentRestore:         tid("li-agent-restore"),
		agentRestoreProject:  tid("li-agent-restore-project"),
		agentProvisioning:    tid("li-agent-provisioning"),
	}

	require.NoError(t, s.CreateProject(ctx, &store.Project{ID: f.project, Name: "LI Project", Slug: "li-project"}))
	require.NoError(t, s.CreateProject(ctx, &store.Project{ID: f.projectDel, Name: "LI Project Del", Slug: "li-project-del"}))

	require.NoError(t, s.CreateAgent(ctx, &store.Agent{ID: f.agent, Slug: "li-agent", Name: "LI Agent", ProjectID: f.project, Phase: string(state.PhaseRunning)}))
	require.NoError(t, s.CreateAgent(ctx, &store.Agent{ID: f.agentDel, Slug: "li-agent-del", Name: "LI Agent Del", ProjectID: f.project, Phase: string(state.PhaseRunning)}))
	require.NoError(t, s.UpdateAgentExposedPorts(ctx, f.agent, []store.ExposedPort{{Port: 18080, Host: "127.0.0.1", Label: "li", Mode: "rw", ExposedAt: now, ExposedBy: "agent"}}))

	// agent.lifecycle.control dispatches start/stop/suspend/restart through
	// handleAgentLifecycle, which persists a real phase change to the store
	// (checkBrokerAvailability lets an agent with no RuntimeBrokerID
	// through, and GetDispatcher() is nil in testServer, so start/stop/
	// restart never reach a broker). A dedicated fixture, not f.agent, keeps
	// that phase churn (and stop's port clearing) off the agent every other
	// family's checks (e.g. agent.portaccess's proxy entries) still depend
	// on.
	//
	// Expected per-form sequence (start, stop, suspend, restart, in catalog
	// declaration order): start/stop/restart each 200. suspend runs third,
	// after stop already set phase=stopped, so its own phase=running guard
	// 400s before it would reach suspendAgent — still a real, non-404/405
	// result read from the seeded row, not a placeholder artifact.
	require.NoError(t, s.CreateAgent(ctx, &store.Agent{ID: f.agentLifecycle, Slug: "li-agent-lifecycle", Name: "LI Agent Lifecycle", ProjectID: f.project, Phase: string(state.PhaseRunning)}))

	// agent.lifecycle.restore requires the target to already be
	// soft-deleted (restoreAgent 400s "Agent is not in deleted state"
	// otherwise); seeded pre-deleted here so the positive check exercises a
	// genuine restore (200) rather than that guard. Two separate instances,
	// one per path form, so the by-id form's restore consuming its fixture's
	// DeletedAt does not turn the project-scoped form's restore into that
	// same 400.
	require.NoError(t, s.CreateAgent(ctx, &store.Agent{ID: f.agentRestore, Slug: "li-agent-restore", Name: "LI Agent Restore", ProjectID: f.project, Phase: string(state.PhaseStopped), DeletedAt: now.Add(-time.Hour)}))
	require.NoError(t, s.CreateAgent(ctx, &store.Agent{ID: f.agentRestoreProject, Slug: "li-agent-restore-project", Name: "LI Agent Restore Project", ProjectID: f.project, Phase: string(state.PhaseStopped), DeletedAt: now.Add(-time.Hour)}))

	// agent.lifecycle.env 409s unless the agent is phase=provisioning
	// or phase=created (submitAgentEnv); shared by both path forms since
	// GetDispatcher() is nil in testServer, so submitAgentEnv always 400s
	// "no runtime broker available" before persisting anything, for either
	// form, leaving this fixture's phase untouched.
	require.NoError(t, s.CreateAgent(ctx, &store.Agent{ID: f.agentProvisioning, Slug: "li-agent-provisioning", Name: "LI Agent Provisioning", ProjectID: f.project, Phase: string(state.PhaseProvisioning)}))

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
	f.projectMemberRoleID = projectMemberRole.ID
	// A separate binding for project.membership.update: UpdateMemberRole
	// (project_membership_service.go) replaces the binding atomically
	// (create new, delete old) even when the role doesn't change, so
	// probing update with a real body must not use the same binding
	// project.membership.remove still needs.
	pmUpdate, err := s.CreateRoleBinding(ctx, &store.RoleBinding{RoleDefinitionID: projectMemberRole.ID, PrincipalType: store.RoleBindingPrincipalUser, PrincipalID: f.userDel, ScopeType: store.RoleScopeProject, ScopeID: f.project, CreatedBy: f.user, CreatedAt: now})
	require.NoError(t, err)
	f.projectMembershipUpdate = pmUpdate.ID

	// Maintenance operations and migrations are seeded once, by key, during
	// Migrate() (SeedMaintenanceOperations) — they are a fixed built-in
	// registry, not something a test creates.
	f.maintenanceOpKey = "pull-images"
	f.maintenanceMigrationKey = "secret-hub-id-migration"

	// hub.integrations.read needs a plugin manager that actually has a
	// plugin loaded: handleGetIntegration (handlers_integrations.go) 404s
	// for any name the manager doesn't have, and testServer configures no
	// plugin manager at all. mockIntegrationManager is already defined in
	// this package's test sources (handlers_integrations_test.go).
	mgr := newMockIntegrationManager()
	mgr.plugins[f.integrationName] = map[string]string{}
	srv.pluginManager = mgr

	// chat.access's /chat/prefs entries need a real WebChatStore:
	// handleChatPrefs (handlers_chat_prefs.go) checks webChatStore for nil
	// before its method switch, so with no store configured every method —
	// including the bogus control method — hits the same pre-dispatch 503.
	dbProvider, ok := s.(interface{ DB() *sql.DB })
	require.True(t, ok, "store does not expose DB() *sql.DB; cannot wire a WebChatStore")
	wcs := NewWebChatStore(dbProvider.DB(), "sqlite3")
	require.NoError(t, wcs.Init())
	srv.SetWebChatStore(wcs)

	// Wiring a real WebChatStore above (for /chat/prefs) also changes
	// chat.access's /chat/topics/{id} and /chat/conversations/{id}/messages
	// entries: both call WebChatStore.GetTopic(key) and 404 for a key it
	// doesn't have (handleTopicGet, handleConversationHistory), where a nil
	// store previously short-circuited to a graceful empty/200 response. A
	// bare topic row is enough for both GET entries to resolve.
	require.NoError(t, wcs.CreateTopic(ctx, WebChatTopic{
		ID:        f.chatTopic,
		ProjectID: f.project,
		Name:      "li-chat-topic",
		CreatedBy: f.user,
		CreatedAt: now,
	}))

	require.NoError(t, s.CreateLifecycleHook(ctx, &store.LifecycleHook{
		ID:        f.lifecycleHook,
		Name:      "li-lifecycle-hook",
		ScopeType: store.LifecycleHookScopeHub,
		Trigger:   store.LifecycleHookTriggerRunning,
		Action: &store.LifecycleHookAction{
			Type:    store.LifecycleHookActionHTTP,
			Method:  "POST",
			URL:     "https://example.invalid/li-lifecycle-hook",
			OnError: store.LifecycleHookOnErrorLog,
		},
		Enabled: true,
		Created: now,
		Updated: now,
	}))

	// Artifact service: on (experiment, store, blob storage) with one
	// single-file artifact owned by the dev user, homed in f.project.
	artStore, artBlobs := enableArtifactsForTest(t, srv)
	f.artifact = seedLiveInventoryArtifact(t, ctx, artStore, artBlobs, f.project)

	return f
}

// seedLiveInventoryArtifact publishes a one-file artifact directly through
// the store and blob storage, the way the single-file publish endpoint
// writes it, and returns its id.
func seedLiveInventoryArtifact(t *testing.T, ctx context.Context, st artifacts.Store, blobs storage.Storage, projectID string) string {
	t.Helper()
	content := []byte("# live inventory\n")
	sum := sha256.Sum256(content)
	digest := hex.EncodeToString(sum[:])
	_, err := blobs.Upload(ctx, artifacts.BlobPath("test-hub-id", digest), bytes.NewReader(content), storage.UploadOptions{ContentType: "text/markdown"})
	require.NoError(t, err)
	now := time.Now()
	a := &artifacts.Artifact{ID: uuid.NewString(), ScopeKind: artifacts.ScopeKindProject, ScopeRef: projectID,
		OwnerKind: artifacts.PrincipalKindUser, OwnerRef: DevUserID, Title: "live", CreatedAt: now, UpdatedAt: now}
	v := &artifacts.Version{ID: uuid.NewString(), ArtifactID: a.ID, Seq: 1, Kind: artifacts.VersionKindPublish,
		EntryPath: "live.md", TotalBytes: int64(len(content)), FileCount: 1, CreatedAt: now, State: artifacts.VersionStateReady}
	files := []artifacts.File{{VersionID: v.ID, Path: "live.md", Size: int64(len(content)), SHA256: digest, MediaType: "text/markdown"}}
	require.NoError(t, st.CreatePublished(ctx, a, v, files, nil))
	return a.ID
}

// overrideKey identifies one (operation, pattern) pair for parameter
// override lookup. Some patterns are declared by more than one operation
// (e.g. GET, PATCH and DELETE all address "/api/v1/agents/{id}"); an entry
// here overrides the pattern-only fallback in patternOverrides for that
// specific operation only — typically to re-point a destructive verb at a
// disposable fixture instance instead of the one longer-lived reads and
// updates on the same pattern still need.
type overrideKey struct {
	OperationID string
	Pattern     string
}

// opPatternOverrides holds every parameter override that must be resolved
// by (operation, pattern) rather than by pattern alone, because the pattern
// is shared with a different operation that needs a different fixture
// instance. This is the only place a destructive verb is re-pointed at a
// disposable twin; an operation not listed here uses whatever
// patternOverrides gives its pattern.
func opPatternOverrides(f idFixtures) map[overrideKey]map[string]string {
	return map[overrideKey]map[string]string{
		{"agent.lifecycle.delete", "/api/v1/agents/{id}"}:                         {"id": f.agentDel},
		{"project.lifecycle.delete", "/api/v1/projects/{id}"}:                     {"id": f.projectDel},
		{"group.delete", "/api/v1/groups/{id}"}:                                   {"id": f.groupDel},
		{"user.admin.delete", "/api/v1/users/{id}"}:                               {"id": f.userDel},
		{"skill.delete", "/api/v1/skills/{id}"}:                                   {"id": f.skillDel},
		{"template.delete", "/api/v1/templates/{id}"}:                             {"id": f.templateDel},
		{"harnessconfig.delete", "/api/v1/harness-configs/{id}"}:                  {"id": f.harnessConfigDel},
		{"gcp.identity.delete", "/api/v1/gcp-service-accounts/{id}"}:              {"id": f.gcpSADel},
		{"role.definition.delete", "/api/v1/admin/roles/{id}"}:                    {"id": f.roleDefinitionDel},
		{"quota.update", "/api/v1/admin/limits/{id}"}:                             {"id": f.limitUD},
		{"quota.delete", "/api/v1/admin/limits/{id}"}:                             {"id": f.limitUD},
		{"quota.update", "/api/v1/admin/entitlements/{id}"}:                       {"id": f.entitlementUD},
		{"quota.delete", "/api/v1/admin/entitlements/{id}"}:                       {"id": f.entitlementUD},
		{"access.constraint.update", "/api/v1/admin/access-constraints/{id}"}:     {"id": f.accessConstraintUD},
		{"access.constraint.delete", "/api/v1/admin/access-constraints/{id}"}:     {"id": f.accessConstraintUD},
		{"project.membership.update", "/api/v1/projects/{id}/members/{memberId}"}: {"id": f.project, "memberId": f.projectMembershipUpdate},
	}
}

// patternOverrides maps a literal EntryPoint Pattern to the placeholder
// values it needs, built once real fixture IDs are known. A pattern absent
// from this map, and from opPatternOverrides, is substituted with the
// generic placeholder for every "{name}" segment, which is correct for
// every entry whose live handler dispatches on method (and, where
// applicable, resolves its target) without an existence check that runs
// before the method switch.
func patternOverrides(f idFixtures) map[string]map[string]string {
	return map[string]map[string]string{
		// --- artifact family ---
		"/api/v1/artifacts/{id}":                             {"id": f.artifact},
		"/api/v1/artifacts/{id}/files/{path}":                {"id": f.artifact, "path": "live.md"},
		"/api/v1/artifacts/{id}/versions/{seq}/files/{path}": {"id": f.artifact, "seq": "1", "path": "live.md"},

		// --- agent family ---
		"/api/v1/agents/{id}":                                       {"id": f.agent},
		"/api/v1/agents/{id}/ports":                                 {"id": f.agent},
		"/api/v1/agents/{id}/ports/{port}/proxy":                    {"id": f.agent, "port": f.agentPort},
		"/api/v1/agents/{id}/ports/{port}/proxy/{subpath}":          {"id": f.agent, "port": f.agentPort, "subpath": "x"},
		"/api/v1/agents/{id}/set_message_mode":                      {"id": f.agent},
		"/api/v1/projects/{projectId}/agents/{id}/set_message_mode": {"projectId": f.project, "id": f.agent},

		// --- agent self-access family (agent JWT routes) ---
		"/api/v1/agents/{id}/token/refresh":                         {"id": f.agent},
		"/api/v1/agents/{id}/refresh-token":                         {"id": f.agent},
		"/api/v1/agents/{id}/outbound-message":                      {"id": f.agent},
		"/api/v1/agents/{id}/metrics":                               {"id": f.agent},
		"/api/v1/projects/{projectId}/agents/{id}/outbound-message": {"projectId": f.project, "id": f.agent},

		// --- agent lifecycle family ---
		"/api/v1/agents/{id}/start":                            {"id": f.agentLifecycle},
		"/api/v1/agents/{id}/stop":                             {"id": f.agentLifecycle},
		"/api/v1/agents/{id}/suspend":                          {"id": f.agentLifecycle},
		"/api/v1/agents/{id}/restart":                          {"id": f.agentLifecycle},
		"/api/v1/projects/{projectId}/agents/{id}/start":       {"projectId": f.project, "id": f.agentLifecycle},
		"/api/v1/projects/{projectId}/agents/{id}/stop":        {"projectId": f.project, "id": f.agentLifecycle},
		"/api/v1/projects/{projectId}/agents/{id}/suspend":     {"projectId": f.project, "id": f.agentLifecycle},
		"/api/v1/projects/{projectId}/agents/{id}/restart":     {"projectId": f.project, "id": f.agentLifecycle},
		"/api/v1/agents/{id}/restore":                          {"id": f.agentRestore},
		"/api/v1/projects/{projectId}/agents/{id}/restore":     {"projectId": f.project, "id": f.agentRestoreProject},
		"/api/v1/agents/{id}/exec":                             {"id": f.agent},
		"/api/v1/projects/{projectId}/agents/{id}/exec":        {"projectId": f.project, "id": f.agent},
		"/api/v1/agents/{id}/env":                              {"id": f.agentProvisioning},
		"/api/v1/projects/{projectId}/agents/{id}/env":         {"projectId": f.project, "id": f.agentProvisioning},
		"/api/v1/agents/{id}/reset-auth":                       {"id": f.agent},
		"/api/v1/projects/{projectId}/agents/{id}/reset-auth":  {"projectId": f.project, "id": f.agent},
		"/api/v1/agents/{id}/reincarnate":                      {"id": f.agent},
		"/api/v1/projects/{projectId}/agents/{id}/reincarnate": {"projectId": f.project, "id": f.agent},

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

		// --- quota family (read defaults; quota.update/.delete are
		// re-pointed at the disposable UD instances by opPatternOverrides) ---
		"/api/v1/admin/limits/{id}":              {"id": f.limit},
		"/api/v1/admin/limits/{id}/entitlements": {"id": f.limit},
		"/api/v1/admin/entitlements/{id}":        {"id": f.entitlement},
		"/api/v1/admin/usage/{limit}":            {"limit": f.limit},

		// --- GCP identity family (gcp.identity.delete is re-pointed at the
		// disposable instance by opPatternOverrides) ---
		"/api/v1/gcp-service-accounts/{id}":        {"id": f.gcpSA},
		"/api/v1/gcp-service-accounts/{id}/verify": {"id": f.gcpSA},

		// --- access constraint family (read default; update/.delete are
		// re-pointed at the disposable UD instance by opPatternOverrides) ---
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
		"/api/v1/runtime-brokers/{id}":                         {"id": f.runtimeBroker},
		"/api/v1/runtime-brokers/{id}/settings":                {"id": f.runtimeBroker},
		"/api/v1/runtime-brokers/{id}/message-failures":        {"id": f.runtimeBroker},
		"/api/v1/runtime-brokers/{id}/agents/{agentId}/launch": {"id": f.runtimeBroker, "agentId": f.agent},

		// --- github app family ---
		"/api/v1/github-app/installations/{id}": {"id": f.githubInstallationID},

		// --- role family (role.definition.delete is re-pointed at the
		// disposable instance by opPatternOverrides) ---
		"/api/v1/admin/roles/{id}":           {"id": f.roleDefinition},
		"/api/v1/admin/roles/{id}/export":    {"id": f.roleDefinition},
		"/api/v1/admin/roles/{id}/duplicate": {"id": f.roleDefinition},
		"/api/v1/admin/role-bindings/{id}":   {"id": f.roleBindingDel},

		// --- maintenance family: run entries deliberately use the OTHER
		// category's key, so executeOperation/executeMigration's own
		// category check rejects the request with 400 before anything
		// executes (admin_maintenance.go:345-350, :138-142) — never a fake
		// key (which would just 404, telling us nothing) and never a
		// same-category real key (which would actually run it).
		"/api/v1/admin/maintenance/operations/{id}":     {"id": f.maintenanceOpKey},
		"/api/v1/admin/maintenance/operations/{id}/run": {"id": f.maintenanceMigrationKey},
		"/api/v1/admin/maintenance/migrations/{id}/run": {"id": f.maintenanceOpKey},

		// --- chat family ---
		"/api/v1/chat/spaces/{id}/threads":         {"id": f.project},
		"/api/v1/chat/topics/{id}":                 {"id": f.chatTopic},
		"/api/v1/chat/conversations/{id}/messages": {"id": f.chatTopic},

		// --- integrations family ---
		"/api/v1/admin/integrations/{name}": {"name": f.integrationName},

		// --- lifecycle hooks family ---
		"/api/v1/admin/lifecycle-hooks/{id}": {"id": f.lifecycleHook},
	}
}

// queryOverrides holds a literal query string to append to a pattern's
// substituted path. Only /chat/prefs needs one today: handleChatPrefs
// (handlers_chat_prefs.go) requires an agentId query parameter to resolve
// which agent's thread preferences to read or write, before it does
// anything else (including its method switch).
func queryOverrides(f idFixtures) map[string]string {
	return map[string]string{
		"/api/v1/chat/prefs": "agentId=" + f.agent,
		// The single-file publish is selected by ?name= (pkg/artifacts).
		"/api/v1/artifacts": "name=live-inventory.txt&scope=" + f.project,
	}
}

// bodyOverrides holds a real request body for a (operation, pattern) pair
// whose handler validates the body before using the path's ID for anything,
// so this test's default generic body (an empty JSON object) would 400
// before the suffix or control check could observe path- or method-related
// behavior at all. An entry not listed here uses the empty object.
func bodyOverrides(f idFixtures) map[overrideKey]map[string]interface{} {
	return map[overrideKey]map[string]interface{}{
		// updateProjectMemberRole (handlers_project_members.go) 400s
		// "roleDefinitionId is required" on an empty body before the
		// binding ID is used for anything; with a real role ID, the
		// handler actually looks up the binding, which is what both the
		// control and the suffix check need to observe.
		{"project.membership.update", "/api/v1/projects/{id}/members/{memberId}"}: {"roleDefinitionId": f.projectMemberRoleID},
		// updateLimitDefinition (handlers_quota.go) overwrites
		// resourceType and unit from the request unconditionally
		// (handlers_quota.go:370-371), so an override missing either
		// field 500s on the store update, on the bare path as much as the
		// suffixed one. All four fields make the positive check exercise
		// a real update (200); the suffix check stays excluded because
		// the suffixed PUT also returns 200 — the suffix is discarded by
		// handleAdminLimitByID's parts[0] extraction (see the exclusion
		// reason above), not rejected.
		{"quota.update", "/api/v1/admin/limits/{id}"}: {"name": "li-limit-ud-updated", "resourceType": "agent", "unit": "count", "defaultValue": 10},
		// handleAgentExec (handlers_agents_core.go) 400s "command is
		// required" on the generic empty body before its own agent lookup
		// and dispatcher check; a real command lets the positive check
		// reach GetDispatcher()'s nil check (503) instead. Both callers
		// (handleAgentAction, handleProjectAgentAction) resolve the agent
		// before calling handleAgentExec, so this override is about the
		// body check, not the agent lookup.
		{"agent.lifecycle.exec", "/api/v1/agents/{id}/exec"}:                      {"command": []interface{}{"echo", "li-inventory-probe"}},
		{"agent.lifecycle.exec", "/api/v1/projects/{projectId}/agents/{id}/exec"}: {"command": []interface{}{"echo", "li-inventory-probe"}},
		// submitAgentEnv (handlers_agents_core.go) 400s "env map is required"
		// on the generic empty body before its own agent lookup (both
		// callers have already resolved the agent); a real env map lets the
		// positive check reach the phase gate and then GetDispatcher()'s nil
		// check (400 "no runtime broker available").
		{"agent.lifecycle.env", "/api/v1/agents/{id}/env"}:                      {"env": map[string]interface{}{"LI_ENV_VAR": "1"}},
		{"agent.lifecycle.env", "/api/v1/projects/{projectId}/agents/{id}/env"}: {"env": map[string]interface{}{"LI_ENV_VAR": "1"}},
	}
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
// accepts, used as the control check (check 2 below): if a route dispatches
// on method the way its handler is supposed to, this method must be
// rejected with 405.
const bogusMethod = "PROPFIND"

// bogusSegment is a path segment no catalog pattern declares, appended
// after the substituted path for the suffix check: a handler that rejects
// undeclared structure should 404 or 405 it, not silently accept it as part
// of an ID or fall through to the same handling as the bare path.
const bogusSegment = "live-inventory-bogus-suffix"

// TestCatalogHTTPEntryPoints_LiveMethodCheck probes the real server mux for
// every declared HTTP entry point in authzop.Catalog (skipping the reviewed
// exclusion maps above) and makes three assertions per entry:
//
//  1. Positive check: sending the catalog's declared method at a path built
//     from the declared pattern, with real fixture IDs substituted wherever
//     the live handler needs one to exist, must not return 404 or 405.
//  2. Control check (runs first — see the ordering note below): sending an
//     unsupported method (bogusMethod) at the same path must return 405.
//     This proves the positive check actually reached the handler's method
//     dispatch — without it, a handler that 404s or errors before ever
//     looking at r.Method would make the positive check pass vacuously
//     regardless of what method the catalog declares.
//  3. Suffix check: sending the declared method at path + "/" + bogusSegment
//     must return 404 or 405. This catches a pattern that is missing a
//     trailing segment a live route actually requires (declared too short),
//     as opposed to declaring the wrong segment (caught by check 1, since a
//     substituted-but-wrong segment reaches a different, real 404). It runs
//     for every entry, parameterised or not: a static collection route can
//     share a mux prefix with a by-ID route, so a missing segment is just as
//     meaningful a question for it.
//
// What these three checks together prove: for an entry not excluded from
// any of them, the catalog's declared method is the one the live route
// accepts, and the declared pattern (with its parameters substituted) is
// long enough to reach that route's own dispatch rather than a shorter
// prefix's. That is #2227's AC2 for the shape of drift the original catalog
// actually had — a wrong method, or a wrong/incomplete path segment.
//
// What these checks do NOT prove, even for an included entry:
//   - That the pattern is not too LONG. A handler that ignores or discards
//     an extra trailing segment returns the same result as it does for the
//     bare path, so the positive check cannot tell a too-long pattern from
//     a correct one. Such a handler also FAILS the suffix check (it
//     answers something other than 404/405), which is why it is listed in
//     suffixCheckExclusions with the specific reason: e.g. handleUserByID
//     falls through for any unrecognized suffix; extractID-based handlers
//     truncate at the first '/' and discard the rest rather than folding
//     it into the ID. This is a real, named limitation, not an oversight.
//   - Full path correctness when the positive check's non-404/405 status
//     comes from a pre-dispatch condition unrelated to routing — most
//     commonly a 5xx or 503 for a backend this test server does not
//     configure (the secret backend, the metrics/telemetry project,
//     attachment storage), or a body-validation 400 before the path ID is
//     used. For those entries the positive check confirms only that the
//     route and method exist. The control and suffix checks still run
//     unless the entry is excluded from them — several of these are
//     excluded from the suffix check, and gcp.identity.verify from the
//     control check (see the maps above).
func TestCatalogHTTPEntryPoints_LiveMethodCheck(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()

	f := seedLiveInventoryFixtures(t, ctx, srv, s)
	opOverrides := opPatternOverrides(f)
	patternFallback := patternOverrides(f)
	queryFor := queryOverrides(f)
	bodyFor := bodyOverrides(f)

	tested, controlChecked, suffixChecked, suffixSkipped := 0, 0, 0, 0
	for _, entry := range catalogHTTPEntryPoints() {
		ep := entry.EntryPoint
		key := liveInventoryKey{OperationID: entry.OperationID, Method: ep.Method, Pattern: ep.Pattern}

		params := opOverrides[overrideKey{entry.OperationID, ep.Pattern}]
		if params == nil {
			params = patternFallback[ep.Pattern]
		}

		path := substituteLiveInventoryParams(ep.Pattern, params)
		if q, ok := queryFor[ep.Pattern]; ok {
			path += "?" + q
		}

		var body interface{}
		switch ep.Method {
		case http.MethodPost, http.MethodPut, http.MethodPatch:
			if override, ok := bodyFor[overrideKey{entry.OperationID, ep.Pattern}]; ok {
				body = override
			} else {
				body = map[string]interface{}{}
			}
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

		// Suffix check also runs before the positive check, for the same
		// destructive-entry reason as the control check above. It applies
		// to every entry, parameterised or not: a static collection route
		// can sit under the same mux prefix as a by-ID route (e.g.
		// "/api/v1/secrets" and "/api/v1/secrets/{key}" both register under
		// "/api/v1/secrets/"), so an undeclared suffix on the static route
		// can fall through to that other handler exactly the way a missing
		// segment on a parameterised route would.
		if reason, excluded := positiveCheckExclusions[key]; excluded {
			// A key excluded from the positive check is excluded here too:
			// the side effect risk that rules out sending the declared
			// method to the bare target applies just as much to sending it
			// to the same target plus a bogus suffix.
			t.Logf("suffix check skipped for %s %s (operation %s): %s", ep.Method, path, entry.OperationID, reason)
			suffixSkipped++
		} else if reason, excluded := suffixCheckExclusions[key]; excluded {
			t.Logf("suffix check skipped for %s %s (operation %s): %s", ep.Method, path, entry.OperationID, reason)
			suffixSkipped++
		} else {
			suffixPath := path
			if q, ok := queryFor[ep.Pattern]; ok {
				suffixPath = strings.TrimSuffix(path, "?"+q) + "/" + bogusSegment + "?" + q
			} else {
				suffixPath += "/" + bogusSegment
			}
			suffixRec := doRequest(t, srv, ep.Method, suffixPath, body)
			suffixChecked++
			if suffixRec.Code != http.StatusNotFound && suffixRec.Code != http.StatusMethodNotAllowed {
				t.Errorf("%s %s (operation %s): got %d for an undeclared trailing segment, want 404 or 405 — the catalog pattern may be missing a segment the live route requires",
					ep.Method, suffixPath, entry.OperationID, suffixRec.Code)
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
	if suffixChecked == 0 {
		t.Fatal("no HTTP catalog entry points were exercised by the suffix check — this test is broken")
	}
	t.Logf("live method/path check: %d positive checks (%d excluded), %d control checks (%d excluded), %d suffix checks (%d excluded)",
		tested, len(positiveCheckExclusions), controlChecked, len(controlCheckExclusions), suffixChecked, suffixSkipped)
}

// TestLiveInventoryExclusionsNotStale asserts every entry in
// positiveCheckExclusions, controlCheckExclusions and suffixCheckExclusions
// still names a real, currently declared catalog HTTP entry point. A stale
// exclusion — left behind after a catalog correction changes or removes the
// entry point it names — would silently stop meaning anything and hide the
// entry from live-inventory coverage for no reason.
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
	for key := range suffixCheckExclusions {
		if !live[key] {
			t.Errorf("stale suffix-check exclusion: operation %q method %q pattern %q does not match any current catalog HTTP entry point",
				key.OperationID, key.Method, key.Pattern)
		}
	}
}
