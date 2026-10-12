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

// TestAdminPolicy (ptone/scion#4240, admin tier phase A2).
//
// Test identities with admin roles hold real hub-admin and super-admin
// bindings. The test-admin policy classifies every power those roles carry
// as allow or deny for the admin test identity kinds (test_hub_admin,
// test_super_admin). A deny entry is refused to those kinds even though the
// role holds it; human callers are never affected. The classification
// covers:
//
//   - every permission in the permission registry (enforced in decide,
//     before any binding is evaluated);
//   - every legacy admin route, that is every requireAdmin /
//     requireAdminFor site (enforced in requireAdminAt);
//   - every role-claim admin gate outside those routes (enforced at the
//     gate through testAdminPolicyRefuses);
//   - every operational settings section and key, every file-live key and
//     every experiment (enforced in the settings and experiment write
//     paths; every write surface is deny by default).
//
// Completeness is a startup property as well as a CI property: the test
// super-admin kind (--test-mode=super-admin) is admitted only by a binary
// whose classification is complete (TestAdminPolicyComplete), and the
// tests in test_admin_policy_test.go fail on any unclassified or stale
// entry.

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"

	"github.com/GoogleCloudPlatform/scion/pkg/config/opsettings"
	"github.com/GoogleCloudPlatform/scion/pkg/experiments"
	"github.com/GoogleCloudPlatform/scion/pkg/hub/permissions"
)

// testAdminPolicy is the classification of one power for the admin test
// identity kinds.
type testAdminPolicy string

const (
	testAdminPolicyAllow testAdminPolicy = "allow"
	testAdminPolicyDeny  testAdminPolicy = "deny"
)

// testAdminPermissionPolicy classifies every permission registry row.
// Deny covers the initial deny set of the admin tier design (§3.6.1):
// server configuration read and write, federation, the GitHub app, hub
// environment variables and secret values, maintenance, auth reset,
// experiments, test identity issuance, access constraint administration,
// user promotion, and every other runtime-writable configuration or key
// surface. Everything else the roles hold is allow.
var testAdminPermissionPolicy = map[string]testAdminPolicy{
	// Agents.
	"agent.create":              testAdminPolicyAllow,
	"agent.read":                testAdminPolicyAllow,
	"agent.list":                testAdminPolicyAllow,
	"agent.update":              testAdminPolicyAllow,
	"agent.delete":              testAdminPolicyAllow,
	"agent.attach":              testAdminPolicyAllow,
	"agent.lifecycle":           testAdminPolicyAllow,
	"agent.port_access":         testAdminPolicyAllow,
	"agent.stop_all":            testAdminPolicyAllow,
	"agent.message":             testAdminPolicyAllow,
	"agent.set_message_mode":    testAdminPolicyAllow,
	"agent.grant_hub_mode":      testAdminPolicyAllow,
	"agent.delegation.create":   testAdminPolicyAllow,
	"agent.delegation.exchange": testAdminPolicyAllow,
	// Agent-principal permissions: only agent credentials use them.
	"agent.status_update":  testAdminPolicyAllow,
	"agent.log_append":     testAdminPolicyAllow,
	"agent.notify":         testAdminPolicyAllow,
	"agent.token_refresh":  testAdminPolicyAllow,
	"agent.port_forward":   testAdminPolicyAllow,
	"agent.identity_token": testAdminPolicyAllow,

	// Projects.
	"project.create":               testAdminPolicyAllow,
	"project.read":                 testAdminPolicyAllow,
	"project.update":               testAdminPolicyAllow,
	"project.delete":               testAdminPolicyAllow,
	"project.manage":               testAdminPolicyAllow,
	"project.register":             testAdminPolicyAllow,
	"project.set_messaging_policy": testAdminPolicyAllow,
	"project.clone":                testAdminPolicyAllow,
	"project.list":                 testAdminPolicyAllow,
	// Secret value reads for agent material resolution.
	"project.secret_read": testAdminPolicyDeny,

	// Artifacts, skills, templates, harness configs.
	"artifact.read":         testAdminPolicyAllow,
	"artifact.create":       testAdminPolicyAllow,
	"artifact.update":       testAdminPolicyAllow,
	"artifact.delete":       testAdminPolicyAllow,
	"artifact.manage":       testAdminPolicyAllow,
	"skill.create":          testAdminPolicyAllow,
	"skill.create_global":   testAdminPolicyAllow,
	"skill.read":            testAdminPolicyAllow,
	"skill.update":          testAdminPolicyAllow,
	"skill.delete":          testAdminPolicyAllow,
	"skill.list":            testAdminPolicyAllow,
	"skill.register":        testAdminPolicyAllow,
	"template.create":       testAdminPolicyAllow,
	"template.read":         testAdminPolicyAllow,
	"template.update":       testAdminPolicyAllow,
	"template.delete":       testAdminPolicyAllow,
	"template.list":         testAdminPolicyAllow,
	"harness_config.create": testAdminPolicyAllow,
	"harness_config.read":   testAdminPolicyAllow,
	"harness_config.update": testAdminPolicyAllow,
	"harness_config.delete": testAdminPolicyAllow,
	"harness_config.list":   testAdminPolicyAllow,

	// Groups, users, policies.
	"group.create":       testAdminPolicyAllow,
	"group.read":         testAdminPolicyAllow,
	"group.update":       testAdminPolicyAllow,
	"group.delete":       testAdminPolicyAllow,
	"group.list":         testAdminPolicyAllow,
	"group.addMember":    testAdminPolicyAllow,
	"group.removeMember": testAdminPolicyAllow,
	"user.read":          testAdminPolicyAllow,
	"user.update":        testAdminPolicyAllow,
	"user.list":          testAdminPolicyAllow,
	"user.invite":        testAdminPolicyAllow,
	"user.suspend":       testAdminPolicyAllow,
	"user.delete":        testAdminPolicyAllow,
	// User role change and promotion.
	"user.promote":  testAdminPolicyDeny,
	"policy.create": testAdminPolicyAllow,
	"policy.read":   testAdminPolicyAllow,
	"policy.update": testAdminPolicyAllow,
	"policy.delete": testAdminPolicyAllow,
	"policy.list":   testAdminPolicyAllow,

	// Brokers (the test admin tier admits only the co-located broker).
	"broker.create":       testAdminPolicyAllow,
	"broker.read":         testAdminPolicyAllow,
	"broker.update":       testAdminPolicyAllow,
	"broker.delete":       testAdminPolicyAllow,
	"broker.list":         testAdminPolicyAllow,
	"broker.auto_provide": testAdminPolicyAllow,
	"broker.dispatch":     testAdminPolicyAllow,

	// GCP service accounts registered on this hub.
	"gcp_service_account.create": testAdminPolicyAllow,
	"gcp_service_account.read":   testAdminPolicyAllow,
	"gcp_service_account.delete": testAdminPolicyAllow,
	"gcp_service_account.list":   testAdminPolicyAllow,
	"gcp_service_account.verify": testAdminPolicyAllow,
	"gcp_service_account.mint":   testAdminPolicyAllow,
	"gcp_service_account.assign": testAdminPolicyAllow,
	"gcp_service_account.use":    testAdminPolicyAllow,

	// Hub configuration and operations.
	"hub.settings.read":   testAdminPolicyAllow,
	"hub.settings.update": testAdminPolicyDeny, // hub-wide injected skills
	"hub.config.read":     testAdminPolicyDeny,
	"hub.config.update":   testAdminPolicyDeny,
	// Maintenance operations (migrations, restart, update checks).
	"hub.maintenance.execute": testAdminPolicyDeny,
	"hub.diagnostics.read":    testAdminPolicyAllow,
	"hub.health.read":         testAdminPolicyAllow,
	"hub.admin_mode.read":     testAdminPolicyAllow,
	"hub.admin_mode.update":   testAdminPolicyDeny,
	// Integration settings carry credentials.
	"hub.integrations.read":       testAdminPolicyDeny,
	"hub.integrations.update":     testAdminPolicyDeny,
	"hub.lifecycle_hooks.read":    testAdminPolicyAllow,
	"hub.lifecycle_hooks.update":  testAdminPolicyDeny,
	"hub.allow_list.read":         testAdminPolicyAllow,
	"hub.allow_list.update":       testAdminPolicyDeny,
	"hub.project_defaults.read":   testAdminPolicyAllow,
	"hub.project_defaults.update": testAdminPolicyDeny,
	"hub.messaging.update":        testAdminPolicyDeny,
	"hub.experiments.update":      testAdminPolicyDeny,
	// Signing key rotation.
	"hub.conduit_grant_keys.execute": testAdminPolicyDeny,
	"hub.auth_reset.execute":         testAdminPolicyDeny,
	"hub.scheduler.read":             testAdminPolicyAllow,
	"hub.scheduler.update":           testAdminPolicyDeny,
	"hub.federation.read":            testAdminPolicyDeny,
	"hub.federation.update":          testAdminPolicyDeny,
	"hub.teams_manifest.read":        testAdminPolicyAllow,
	"hub.teams_manifest.update":      testAdminPolicyDeny,
	"hub.validate.execute":           testAdminPolicyAllow,
	"hub.github_app.read":            testAdminPolicyDeny,
	"hub.github_app.update":          testAdminPolicyDeny,
	"hub.metrics.read":               testAdminPolicyAllow,
	"hub.env_vars.read":              testAdminPolicyDeny,
	// Explaining other principals' decisions is a read.
	"hub.audit.read": testAdminPolicyAllow,

	// Test identity issuance: test identities never issue.
	"test_identity.issue":             testAdminPolicyDeny,
	"test_identity.issue_admin":       testAdminPolicyDeny,
	"test_identity.issue_super_admin": testAdminPolicyDeny,

	// Quotas, roles, bindings, access constraints, schedules.
	"quota.read":              testAdminPolicyAllow,
	"quota.create":            testAdminPolicyAllow,
	"quota.update":            testAdminPolicyAllow,
	"quota.delete":            testAdminPolicyAllow,
	"role.read":               testAdminPolicyAllow,
	"role.create":             testAdminPolicyAllow,
	"role.update":             testAdminPolicyAllow,
	"role.delete":             testAdminPolicyAllow,
	"role_binding.read":       testAdminPolicyAllow,
	"role_binding.create":     testAdminPolicyAllow,
	"role_binding.delete":     testAdminPolicyAllow,
	"access_constraint.admin": testAdminPolicyDeny,
	"access_constraint.read":  testAdminPolicyAllow,
	"scheduled_event.read":    testAdminPolicyAllow,
	"scheduled_event.list":    testAdminPolicyAllow,
	"scheduled_event.create":  testAdminPolicyAllow,
	"scheduled_event.delete":  testAdminPolicyAllow,
	"scheduled_event.update":  testAdminPolicyAllow,

	// Material delivery and runtime retrieval of secret values.
	"secret.deliver":          testAdminPolicyDeny,
	"env_var.deliver":         testAdminPolicyDeny,
	"skill_injection.deliver": testAdminPolicyDeny,
	"secret.use":              testAdminPolicyDeny,

	// Self-scoped records.
	"inbox.read":                  testAdminPolicyAllow,
	"inbox.write":                 testAdminPolicyAllow,
	"user_skill_injection.update": testAdminPolicyAllow,
}

// testAdminMustDenyPermissions is the initial deny set of the admin tier
// design (§3.6.1), pinned: each must be classified deny. Every
// hub.federation.* and hub.github_app.* permission must be deny as well
// (checked by prefix).
var testAdminMustDenyPermissions = []string{
	"hub.config.update",
	"hub.config.read",
	"hub.env_vars.read",
	"hub.maintenance.execute",
	"hub.auth_reset.execute",
	"hub.experiments.update",
	"test_identity.issue",
	"test_identity.issue_admin",
	"test_identity.issue_super_admin",
	"access_constraint.admin",
	"user.promote",
	"project.secret_read",
	"secret.use",
}

// testAdminMustDenyPermissionPrefixes are permission families that are
// deny as a whole.
var testAdminMustDenyPermissionPrefixes = []string{"hub.federation.", "hub.github_app."}

// testAdminLegacyRoutePolicy classifies every legacy admin route: each
// RouteHubAdmin route without a declared permission (the route guard falls
// back to requireAdmin; keyed by RouteID) and each handler that calls
// requireAdminAt directly (keyed by its site name).
var testAdminLegacyRoutePolicy = map[string]testAdminPolicy{
	// Delegation adoption commits agent authority migrations.
	"admin.delegationAdoption":          testAdminPolicyDeny,
	"admin.delegationAdoption.previews": testAdminPolicyDeny,
	"admin.delegationAdoption.commits":  testAdminPolicyDeny,
	// Lifting a governance hold on one agent.
	"agents.hold.lift": testAdminPolicyAllow,
	// Revoking one user's web sessions.
	"users.sessions.revoke": testAdminPolicyAllow,
}

// testAdminRoleGate is the classification of one role-claim admin gate: a
// function that admits an unscoped local platform admin (or a stored or
// claimed admin role) outside the legacy admin routes.
type testAdminRoleGate struct {
	Policy testAdminPolicy
	// EnforcedIn names the function that calls testAdminPolicyRefuses for
	// this gate (deny entries only; empty means the gate's own function).
	EnforcedIn string
	Reason     string
}

// testAdminRoleGatePolicy classifies every role-claim admin gate, keyed
// "file:function" (the census in test_admin_policy_census_test.go finds
// them: the callers of IsUnscopedLocalPlatformAdmin, IsSystemAdmin and
// IsHubAdmin, and the role gates of testAdminRoleAdminCensus).
var testAdminRoleGatePolicy = map[string]testAdminRoleGate{
	"authorize.go:requireAdminAt":                                 {Policy: testAdminPolicyAllow, Reason: "legacy admin routes: classified per route in testAdminLegacyRoutePolicy and enforced in requireAdminAt"},
	"admin_delegation_adoption.go:isDelegationAdoptionAdmin":      {Policy: testAdminPolicyDeny, Reason: "agent authority migration"},
	"admin_delegation_adoption.go:recheckDelegationAdoptionAdmin": {Policy: testAdminPolicyDeny, Reason: "agent authority migration"},
	"admin_mode.go:adminModeMiddleware":                           {Policy: testAdminPolicyAllow, Reason: "admins pass while admin mode is on (the mode itself is hub.admin_mode.update, deny)"},
	"admin_mode.go:adminModeWebMiddleware":                        {Policy: testAdminPolicyAllow, Reason: "admins pass while admin mode is on (web)"},
	"gcp_iam_settings.go:gcpIAMHubAdmin":                          {Policy: testAdminPolicyDeny, EnforcedIn: "requireGCPIAMHubAdmin", Reason: "GCP permission check settings are runtime configuration"},
	"handlers_env_secrets.go:authorizeHubEnvVarList":              {Policy: testAdminPolicyDeny, Reason: "hub environment variables (hub.env_vars.read)"},
	"handlers_env_secrets.go:resolveEnvSecretAccess":              {Policy: testAdminPolicyDeny, Reason: "hub-scope environment variables and secrets"},
	"web.go:authorizeSSESubjects":                                 {Policy: testAdminPolicyAllow, Reason: "event subscriptions (read)"},
	"passthrough_gate.go:authorizePassthroughIdentity":            {Policy: testAdminPolicyDeny, Reason: "exposes the co-located broker's host GCP identity to agents"},
	"handlers_auth.go:handleAuthAdminStatus":                      {Policy: testAdminPolicyAllow, Reason: "display of the caller's own admin status"},
	"perftrace_middleware.go:perfHeadersAllowed":                  {Policy: testAdminPolicyAllow, Reason: "request timing headers"},
	"project_messaging_policy.go:messagingPolicyOwnerRule":        {Policy: testAdminPolicyAllow, Reason: "project messaging policy (project.set_messaging_policy is allow)"},
	"authorize_message.go:authorizeAgentMessage":                  {Policy: testAdminPolicyAllow, Reason: "messaging agents"},
	"handlers_agent_message_mode.go:handleSetMessageMode":         {Policy: testAdminPolicyAllow, Reason: "agent message mode"},
	"deletion_redact.go:callerSeesDeletionDetail":                 {Policy: testAdminPolicyAllow, Reason: "deletion detail display"},
	"authz_candelegate.go:CanDelegate":                            {Policy: testAdminPolicyAllow, Reason: "delegation by an admin; grant shaping for test admins is phase A3"},
	// System-admin binding checks outside decide (IsSystemAdmin, IsHubAdmin).
	"handlers_agent_create_helpers.go:brokerProviderHasOwnerConsent": {Policy: testAdminPolicyAllow, Reason: "reads whether the broker provider's linker (not the caller) is a system admin"},
	"handlers_brokers.go:brokerRemintTargetAuthorized":               {Policy: testAdminPolicyDeny, Reason: "runtime broker credential remint (key material)"},
	"handlers_brokers.go:authorizedForBrokerRotate":                  {Policy: testAdminPolicyDeny, Reason: "runtime broker secret rotation (key material)"},
	"authz_delegation_ceiling.go:evaluateUserDelegatorAuthority":     {Policy: testAdminPolicyAllow, Reason: "delegation ceiling of a system-admin delegator; grant shaping for test admins is phase A3"},
	"project_deletion_service.go:Delete":                             {Policy: testAdminPolicyAllow, Reason: "super-admin project deletion governance"},
	"project_deletion_service.go:checkDeletionGovernance":            {Policy: testAdminPolicyAllow, Reason: "super-admin project deletion governance"},
}

// testAdminSettingsSectionPolicy classifies every operational settings
// section. All are deny: test identities get no settings writes.
var testAdminSettingsSectionPolicy = map[string]testAdminPolicy{
	"access":            testAdminPolicyDeny,
	"lifecycle":         testAdminPolicyDeny,
	"maintenance":       testAdminPolicyDeny,
	"messaging":         testAdminPolicyDeny,
	"telemetry":         testAdminPolicyDeny,
	"auto_expose_ports": testAdminPolicyDeny,
	"quotas":            testAdminPolicyDeny,
	"gcp_iam":           testAdminPolicyDeny,
	"agent_secrets":     testAdminPolicyDeny,
	"agent_defaults":    testAdminPolicyDeny,
	"endpoints":         testAdminPolicyDeny,
	"github_app":        testAdminPolicyDeny,
	"notifications":     testAdminPolicyDeny,
	"project_defaults":  testAdminPolicyDeny,
	"federation":        testAdminPolicyDeny,
	"runtimes":          testAdminPolicyDeny,
	"profiles":          testAdminPolicyDeny,
	"harness_configs":   testAdminPolicyDeny,
	"experiments":       testAdminPolicyDeny,
	"artifacts":         testAdminPolicyDeny,
	"profiling":         testAdminPolicyDeny,
}

// testAdminSettingsKeyPolicy classifies every operational settings key
// (each section's KoanfPaths). All are deny.
var testAdminSettingsKeyPolicy = map[string]testAdminPolicy{
	"server.hub.admin_emails":                  testAdminPolicyDeny,
	"server.auth.user_access_mode":             testAdminPolicyDeny,
	"server.auth.default_user_role":            testAdminPolicyDeny,
	"server.auth.authorized_domains":           testAdminPolicyDeny,
	"server.hub.auto_suspend_stalled":          testAdminPolicyDeny,
	"server.hub.stalled_threshold":             testAdminPolicyDeny,
	"server.hub.soft_delete_retention":         testAdminPolicyDeny,
	"server.hub.soft_delete_retain_files":      testAdminPolicyDeny,
	"server.hub.start_claim_lease_ttl":         testAdminPolicyDeny,
	"server.hub.start_max_duration":            testAdminPolicyDeny,
	"server.hub.start_unconfirmed_hold":        testAdminPolicyDeny,
	"server.hub.start_create_unconfirmed_hold": testAdminPolicyDeny,
	"telemetry.enabled":                        testAdminPolicyDeny,
	"telemetry.cloud":                          testAdminPolicyDeny,
	"telemetry.cloud.enabled":                  testAdminPolicyDeny,
	"telemetry.cloud.endpoint":                 testAdminPolicyDeny,
	"telemetry.cloud.protocol":                 testAdminPolicyDeny,
	"telemetry.cloud.headers":                  testAdminPolicyDeny,
	"telemetry.cloud.provider":                 testAdminPolicyDeny,
	"telemetry.cloud.tls":                      testAdminPolicyDeny,
	"telemetry.cloud.tls.enabled":              testAdminPolicyDeny,
	"telemetry.cloud.tls.insecure_skip_verify": testAdminPolicyDeny,
	"telemetry.cloud.tls.ca_file":              testAdminPolicyDeny,
	"telemetry.cloud.batch":                    testAdminPolicyDeny,
	"telemetry.cloud.batch.max_size":           testAdminPolicyDeny,
	"telemetry.cloud.batch.timeout":            testAdminPolicyDeny,
	"telemetry.hub":                            testAdminPolicyDeny,
	"telemetry.hub.enabled":                    testAdminPolicyDeny,
	"telemetry.hub.report_interval":            testAdminPolicyDeny,
	"telemetry.local":                          testAdminPolicyDeny,
	"telemetry.local.enabled":                  testAdminPolicyDeny,
	"telemetry.local.file":                     testAdminPolicyDeny,
	"telemetry.local.console":                  testAdminPolicyDeny,
	"telemetry.filter":                         testAdminPolicyDeny,
	"telemetry.filter.enabled":                 testAdminPolicyDeny,
	"telemetry.filter.respect_debug_mode":      testAdminPolicyDeny,
	"telemetry.filter.events":                  testAdminPolicyDeny,
	"telemetry.filter.events.include":          testAdminPolicyDeny,
	"telemetry.filter.events.exclude":          testAdminPolicyDeny,
	"telemetry.filter.attributes":              testAdminPolicyDeny,
	"telemetry.filter.attributes.redact":       testAdminPolicyDeny,
	"telemetry.filter.attributes.hash":         testAdminPolicyDeny,
	"telemetry.filter.sampling":                testAdminPolicyDeny,
	"telemetry.filter.sampling.default":        testAdminPolicyDeny,
	"telemetry.filter.sampling.rates":          testAdminPolicyDeny,
	"telemetry.resource":                       testAdminPolicyDeny,
	"auto_expose_ports.enabled":                testAdminPolicyDeny,
	"quotas.enforce_broker_quotas":             testAdminPolicyDeny,
	"server.hub.gcp_iam_check_mode":            testAdminPolicyDeny,
	"server.hub.gcp_iam_deny_unknown_policy":   testAdminPolicyDeny,
	"agent_secrets.user_scope_only":            testAdminPolicyDeny,
	"default_template":                         testAdminPolicyDeny,
	"default_harness_config":                   testAdminPolicyDeny,
	"default_max_turns":                        testAdminPolicyDeny,
	"default_max_model_calls":                  testAdminPolicyDeny,
	"default_max_duration":                     testAdminPolicyDeny,
	"default_resources":                        testAdminPolicyDeny,
	"default_model":                            testAdminPolicyDeny,
	"default_thinking_level":                   testAdminPolicyDeny,
	"default_max_agent_role":                   testAdminPolicyDeny,
	"default_agent_role":                       testAdminPolicyDeny,
	"default_runtime_broker":                   testAdminPolicyDeny,
	"default_timezone":                         testAdminPolicyDeny,
	"default_gcp_identity_mode":                testAdminPolicyDeny,
	"default_gcp_identity_service_account_id":  testAdminPolicyDeny,
	"server.hub.public_url":                    testAdminPolicyDeny,
	"server.hub.hub_name":                      testAdminPolicyDeny,
	"image_registry":                           testAdminPolicyDeny,
	"server.hub.monitoring_dashboard_url":      testAdminPolicyDeny,
	"server.github_app":                        testAdminPolicyDeny,
	"server.github_app.app_id":                 testAdminPolicyDeny,
	"server.github_app.api_base_url":           testAdminPolicyDeny,
	"server.github_app.webhooks_enabled":       testAdminPolicyDeny,
	"server.github_app.installation_url":       testAdminPolicyDeny,
	"server.github_app.private_key_path":       testAdminPolicyDeny,
	"server.notification_channels":             testAdminPolicyDeny,
	"project_defaults.default_scratchpad":      testAdminPolicyDeny,
	"server.federation.enabled":                testAdminPolicyDeny,
	"server.federation.trusted_issuers":        testAdminPolicyDeny,
	"server.federation.algorithms":             testAdminPolicyDeny,
	"server.federation.refresh_interval":       testAdminPolicyDeny,
	"server.federation.debounce_interval":      testAdminPolicyDeny,
	"runtimes":                                 testAdminPolicyDeny,
	"profiles":                                 testAdminPolicyDeny,
	"harness_configs":                          testAdminPolicyDeny,
}

// testAdminFileLiveKeyPolicy classifies every file-live key (fileLiveKeys:
// file-routed keys applied without a restart). All are deny; they are
// writable only on workstation hubs, which the test admin tier excludes.
var testAdminFileLiveKeyPolicy = map[string]testAdminPolicy{
	"server.log_level":       testAdminPolicyDeny,
	"auto_inject_gcloud_adc": testAdminPolicyDeny,
	"schema_version":         testAdminPolicyDeny,
}

// testAdminExperimentPolicy classifies every registered experiment. All
// are deny: test identities never change an experiment.
var testAdminExperimentPolicy = map[string]testAdminPolicy{
	experiments.AgentDelegation:              testAdminPolicyDeny,
	experiments.AuthorizationDecisionAuditV2: testAdminPolicyDeny,
	"web.terminal_workspace":                 testAdminPolicyDeny,
	"web.gcs_links":                          testAdminPolicyDeny,
	experiments.K8sNFSHome:                   testAdminPolicyDeny,
	experiments.FlatRuntimeBrokers:           testAdminPolicyDeny,
	experiments.Artifacts:                    testAdminPolicyDeny,
	"hub.conduit":                            testAdminPolicyDeny,
	experiments.ChatScheduledSend:            testAdminPolicyDeny,
	experiments.AgentEdit:                    testAdminPolicyDeny,
	experiments.ChatThreadGroups:             testAdminPolicyDeny,
}

// testAdminPolicyInputs are the vocabularies the classification must cover.
// testAdminPolicyProblems checks a set of inputs, so a test can add an
// unclassified entry and see it reported.
type testAdminPolicyInputs struct {
	Permissions      []string
	LegacyRoutes     []string
	SettingsSections []string
	SettingsKeys     []string
	FileLiveKeys     []string
	Experiments      []string
}

// currentTestAdminPolicyInputs reads the vocabularies of this binary: the
// permission registry, the legacy admin routes of the route table (plus
// the requireAdminAt sites), the operational settings registry, the
// file-live keys and the experiment registry.
func currentTestAdminPolicyInputs() testAdminPolicyInputs {
	var in testAdminPolicyInputs
	for _, p := range permissions.Registry {
		in.Permissions = append(in.Permissions, p.ID)
	}
	in.LegacyRoutes = append(in.LegacyRoutes, legacyAdminRouteIDs()...)
	in.LegacyRoutes = append(in.LegacyRoutes, testAdminRequireAdminAtSites...)
	for _, sec := range opsettings.Registry {
		in.SettingsSections = append(in.SettingsSections, sec.Name)
		in.SettingsKeys = append(in.SettingsKeys, sec.KoanfPaths...)
	}
	for k := range fileLiveKeys {
		in.FileLiveKeys = append(in.FileLiveKeys, k)
	}
	for _, e := range experiments.Default().All() {
		in.Experiments = append(in.Experiments, e.Name)
	}
	return in
}

// testAdminRequireAdminAtSites are the site names handlers pass to
// requireAdminAt (the census pins this list against the code).
var testAdminRequireAdminAtSites = []string{"agents.hold.lift", "users.sessions.revoke"}

// legacyAdminRouteIDs returns the RouteID of every RouteHubAdmin route
// without a declared permission: the routes whose guard falls back to
// requireAdmin.
func legacyAdminRouteIDs() []string {
	var out []string
	for _, meta := range routeMetadataTable {
		if meta.Classification == RouteHubAdmin && meta.Permission == "" {
			out = append(out, meta.RouteID)
		}
	}
	sort.Strings(out)
	return out
}

// testAdminPolicyProblems returns one line per unclassified entry, per
// entry with a value other than allow or deny, and per stale entry (a
// classification for something that no longer exists). An empty result
// means the classification is complete.
func testAdminPolicyProblems(in testAdminPolicyInputs) []string {
	var problems []string
	check := func(kind string, names []string, table map[string]testAdminPolicy) {
		seen := map[string]bool{}
		for _, n := range names {
			seen[n] = true
			p, ok := table[n]
			switch {
			case !ok:
				problems = append(problems, fmt.Sprintf("%s %q has no TestAdminPolicy classification", kind, n))
			case p != testAdminPolicyAllow && p != testAdminPolicyDeny:
				problems = append(problems, fmt.Sprintf("%s %q has an invalid TestAdminPolicy %q", kind, n, p))
			}
		}
		for n := range table {
			if !seen[n] {
				problems = append(problems, fmt.Sprintf("%s %q is classified but does not exist (stale TestAdminPolicy entry)", kind, n))
			}
		}
	}
	check("permission", in.Permissions, testAdminPermissionPolicy)
	check("legacy admin route", in.LegacyRoutes, testAdminLegacyRoutePolicy)
	check("settings section", in.SettingsSections, testAdminSettingsSectionPolicy)
	check("settings key", in.SettingsKeys, testAdminSettingsKeyPolicy)
	check("file-live key", in.FileLiveKeys, testAdminFileLiveKeyPolicy)
	check("experiment", in.Experiments, testAdminExperimentPolicy)
	for _, id := range testAdminMustDenyPermissions {
		if testAdminPermissionPolicy[id] != testAdminPolicyDeny {
			problems = append(problems, fmt.Sprintf("permission %q is in the initial deny set but is not classified deny", id))
		}
	}
	for id, p := range testAdminPermissionPolicy {
		for _, prefix := range testAdminMustDenyPermissionPrefixes {
			if strings.HasPrefix(id, prefix) && p != testAdminPolicyDeny {
				problems = append(problems, fmt.Sprintf("permission %q (%s*) must be classified deny", id, prefix))
			}
		}
	}
	for key, g := range testAdminRoleGatePolicy {
		if g.Policy != testAdminPolicyAllow && g.Policy != testAdminPolicyDeny {
			problems = append(problems, fmt.Sprintf("role gate %q has an invalid TestAdminPolicy %q", key, g.Policy))
		}
	}
	sort.Strings(problems)
	return problems
}

// ErrTestAdminPolicyIncomplete reports an incomplete test-admin policy
// classification.
var ErrTestAdminPolicyIncomplete = errors.New("the test-admin policy classification is incomplete")

// TestAdminPolicyComplete returns nil when every permission, legacy admin
// route, settings section and key, file-live key and experiment of this
// binary carries a TestAdminPolicy classification, and an error naming
// the gaps otherwise. The test super-admin kind (--test-mode=super-admin)
// is admitted only while it returns nil.
func TestAdminPolicyComplete() error {
	if problems := testAdminPolicyProblems(currentTestAdminPolicyInputs()); len(problems) > 0 {
		return fmt.Errorf("%w: %s", ErrTestAdminPolicyIncomplete, strings.Join(problems, "; "))
	}
	return nil
}

// testAdminPermissionDenied reports whether the test-admin policy denies
// permissionID to the admin test identity kinds. An unclassified
// permission is denied.
func testAdminPermissionDenied(permissionID string) bool {
	return testAdminPermissionPolicy[permissionID] != testAdminPolicyAllow
}

// testAdminCallerIsAdminKind reports whether the caller identified in ctx
// is an admin-kind test identity, through kindForPrincipal. It is false
// while the test admin tier is off (no admin-kind identity authenticates
// then) and for every non-user caller. A kind read error reports true, so
// callers deny (fail closed).
func (s *Server) testAdminCallerIsAdminKind(ctx context.Context) (string, bool) {
	if !s.testAdminTierOn() {
		return "", false
	}
	user := GetUserIdentityFromContext(ctx)
	if isNilIdentity(user) {
		return "", false
	}
	k, err := s.kindForPrincipal(ctx, user.ID())
	if err != nil {
		return user.ID(), true
	}
	return user.ID(), k.isTestAdmin()
}

// testAdminPolicyRefuses reports whether the role-claim admin gate key
// refuses the caller in ctx: the caller is an admin-kind test identity and
// the gate is not classified allow in testAdminRoleGatePolicy. A refusal
// writes a test_admin_policy_denial line to the test admin audit sink.
func (s *Server) testAdminPolicyRefuses(ctx context.Context, key string) bool {
	uid, adminKind := s.testAdminCallerIsAdminKind(ctx)
	if !adminKind {
		return false
	}
	return s.testAdminRoleGateRefused(ctx, uid, key)
}

// testAdminPolicyRefusesUser is testAdminPolicyRefuses for an explicit user
// ID (a gate that re-reads its actor, for example inside a transaction).
func (s *Server) testAdminPolicyRefusesUser(ctx context.Context, userID, key string) bool {
	if !s.testAdminTierOn() || userID == "" {
		return false
	}
	k, err := s.kindForPrincipal(ctx, userID)
	if err == nil && !k.isTestAdmin() {
		return false
	}
	return s.testAdminRoleGateRefused(ctx, userID, key)
}

func (s *Server) testAdminRoleGateRefused(ctx context.Context, userID, key string) bool {
	if g, ok := testAdminRoleGatePolicy[key]; ok && g.Policy == testAdminPolicyAllow {
		return false
	}
	s.recordTestAdminPolicyDenial(ctx, userID, "role_gate", key)
	return true
}

// testAdminRouteRefused reports whether the legacy admin route site
// refuses the caller in ctx (an admin-kind test identity, and the site not
// classified allow). A refusal writes a sink line.
func (s *Server) testAdminRouteRefused(ctx context.Context, site string) bool {
	uid, adminKind := s.testAdminCallerIsAdminKind(ctx)
	if !adminKind {
		return false
	}
	if testAdminLegacyRoutePolicy[site] == testAdminPolicyAllow {
		return false
	}
	if site == "" {
		site = "(unclassified)"
	}
	s.recordTestAdminPolicyDenial(ctx, uid, "legacy_route", site)
	return true
}

// testAdminSettingsKeyAllowed reports whether a settings write that
// touches key may proceed for an admin-kind test identity. key is a body
// path in the settings layout; it is matched against the classified
// settings keys, the file-live keys and the section names (a key matches
// a classified key it equals or nests under). Unmatched keys are denied.
func testAdminSettingsKeyAllowed(key string) bool {
	best, bestPolicy := "", testAdminPolicyDeny
	consider := func(table map[string]testAdminPolicy) {
		for k, p := range table {
			if (key == k || strings.HasPrefix(key, k+".")) && len(k) > len(best) {
				best, bestPolicy = k, p
			}
		}
	}
	consider(testAdminSettingsKeyPolicy)
	consider(testAdminFileLiveKeyPolicy)
	consider(testAdminSettingsSectionPolicy)
	return best != "" && bestPolicy == testAdminPolicyAllow
}

// testAdminExperimentAllowed reports whether an admin-kind test identity
// may change the named experiment. Unclassified names are denied.
func testAdminExperimentAllowed(name string) bool {
	return testAdminExperimentPolicy[name] == testAdminPolicyAllow
}

// rejectTestAdminSettingsWrite answers 403 and returns true when the
// caller is an admin-kind test identity and the write touches any settings
// key, section or experiment the test-admin policy does not allow. keys
// are settings keys or section names; names are experiment names. An
// empty write by such a caller is refused as well (nothing is classified
// allow for an unnamed key). Every settings and experiment write path
// calls it before anything is applied.
func (s *Server) rejectTestAdminSettingsWrite(w http.ResponseWriter, r *http.Request, keys, names []string) bool {
	uid, adminKind := s.testAdminCallerIsAdminKind(r.Context())
	if !adminKind {
		return false
	}
	var refused []string
	for _, k := range keys {
		if !testAdminSettingsKeyAllowed(k) {
			refused = append(refused, k)
		}
	}
	for _, n := range names {
		if !testAdminExperimentAllowed(n) {
			refused = append(refused, "experiment:"+n)
		}
	}
	if len(keys) == 0 && len(names) == 0 {
		refused = append(refused, "(empty)")
	}
	if len(refused) == 0 {
		return false
	}
	sort.Strings(refused)
	s.recordTestAdminPolicyDenial(r.Context(), uid, "settings", strings.Join(refused, ","))
	writeError(w, http.StatusForbidden, ErrCodeForbidden,
		"test identities with admin roles cannot change hub settings or experiments",
		map[string]interface{}{"reason": testAdminReasonPolicyDenied})
	return true
}

// testAdminReasonPolicyDenied is the details.reason of a test-admin policy
// refusal written by a handler.
const testAdminReasonPolicyDenied = "test_admin_policy"

// settingsBodyKeys returns the dotted path of every object member a JSON
// settings write body carries. A path that matches no classified key is
// denied by testAdminSettingsKeyAllowed, so the request layout need not
// equal the settings layout for the refusal to hold.
func settingsBodyKeys(rawBody []byte) []string {
	var out []string
	for _, p := range settingsBodyKeyPaths(rawBody) {
		out = append(out, strings.Join(p, "."))
	}
	sort.Strings(out)
	return out
}

// errTestAdminSettingsRefused is returned by testAdminSettingsWriteGuard.
var errTestAdminSettingsRefused = errors.New("test identities with admin roles cannot change hub settings")

// testAdminSettingsWriteGuard is the operational settings write guard: an
// admin-kind test identity may write a section only when the section is
// classified allow. Writes made without an admin-kind caller in ctx (human
// requests, startup and background reconciliation) pass.
func (s *Server) testAdminSettingsWriteGuard(ctx context.Context, section string) error {
	uid, adminKind := s.testAdminCallerIsAdminKind(ctx)
	if !adminKind {
		return nil
	}
	if testAdminSettingsSectionPolicy[section] == testAdminPolicyAllow {
		return nil
	}
	s.recordTestAdminPolicyDenial(ctx, uid, "settings", "section:"+section)
	return fmt.Errorf("%w: section %s", errTestAdminSettingsRefused, section)
}
