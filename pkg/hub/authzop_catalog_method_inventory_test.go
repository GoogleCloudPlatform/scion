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

	"github.com/GoogleCloudPlatform/scion/pkg/hub/authzop"
)

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
	{OperationID: "inbox.conversation.participant.add", Method: "POST", Pattern: "/api/v1/conversations/{id}/participants"}: "handleAddParticipant answers a caller who is not a participant exactly as an unknown conversation (404); this test calls with the dev token, whose principal kind is \"dev\", and conversation participants are users or agents only, so no fixture can make the caller a participant; TestConversationAddParticipant_RequiresProjectReadAndMemberPrincipals and the TestConversationAddParticipant_* tests drive the route with real participants",
	{OperationID: "hub.maintenance.execute", Method: "POST", Pattern: "/api/v1/admin/maintenance/restart"}:                  "handleAdminRestart (admin_maintenance.go) invokes a real systemd restart subprocess; nothing before it short-circuits for a fake or real target, so there is no safe way to dispatch the declared method",
	{OperationID: "hub.maintenance.execute", Method: "POST", Pattern: "/api/v1/admin/maintenance/check-updates"}:            "handleCheckForUpdates (admin_maintenance.go:662-690) calls the GitHub release channel when MaintenanceConfig.DeploymentTier == \"binary\"; excluded so this test cannot depend on, or accidentally call out based on, server config",
	{OperationID: "agent.hold.lift", Method: "POST", Pattern: "/api/v1/agents/{id}/hold/lift"}:                              "handleAgentHoldLift (agent_hold_lift.go) clears an agent's holds and writes an audit record: a live POST mutates state, so it is not dispatched against a real target here; TestAgentHoldLift drives it against a real held agent",
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
	{OperationID: "project.workspace.read", Method: "GET", Pattern: "/api/v1/projects/{id}/dav/{path}"}:                      "the WebDAV handler (project_webdav.go) serves PROPFIND, this test's control method, as a real WebDAV method, so there is no unsupported method to control against",
	{OperationID: "project.workspace.read", Method: "HEAD", Pattern: "/api/v1/projects/{id}/dav/{path}"}:                     "same as the WebDAV GET control entry above",
	{OperationID: "project.workspace.read", Method: "OPTIONS", Pattern: "/api/v1/projects/{id}/dav/{path}"}:                  "same as the WebDAV GET control entry above",
	{OperationID: "project.workspace.write", Method: "PUT", Pattern: "/api/v1/projects/{id}/dav/{path}"}:                     "same as the WebDAV GET control entry above",
	{OperationID: "project.workspace.write", Method: "PROPPATCH", Pattern: "/api/v1/projects/{id}/dav/{path}"}:               "same as the WebDAV GET control entry above",
	{OperationID: "project.workspace.write", Method: "LOCK", Pattern: "/api/v1/projects/{id}/dav/{path}"}:                    "same as the WebDAV GET control entry above",
	{OperationID: "project.workspace.write", Method: "UNLOCK", Pattern: "/api/v1/projects/{id}/dav/{path}"}:                  "same as the WebDAV GET control entry above",
	{OperationID: "project.workspace.write", Method: "COPY", Pattern: "/api/v1/projects/{id}/dav/{path}"}:                    "same as the WebDAV GET control entry above",
	{OperationID: "project.workspace.write", Method: "MOVE", Pattern: "/api/v1/projects/{id}/dav/{path}"}:                    "same as the WebDAV GET control entry above",
	{OperationID: "project.workspace.write", Method: "POST", Pattern: "/api/v1/projects/{id}/dav/{path}"}:                    "same as the WebDAV GET control entry above",
	{OperationID: "project.workspace.write", Method: "PROPFIND", Pattern: "/api/v1/projects/{id}/dav/{path}"}:                "same as the WebDAV GET control entry above",
	{OperationID: "project.workspace.write", Method: "DELETE", Pattern: "/api/v1/projects/{id}/dav/{path}"}:                  "same as the WebDAV GET control entry above",
	{OperationID: "project.workspace.write", Method: "MKCOL", Pattern: "/api/v1/projects/{id}/dav/{path}"}:                   "same as the WebDAV GET control entry above",
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
	{OperationID: "user.read", Method: "GET", Pattern: "/api/v1/users/{id}"}:                                                     "handleUserByID (handlers_users_core.go) only special-cases the \"revoke-sessions\" suffix; any other suffix, including this check's bogus one, falls through to the same GET handling as the bare ID",
	{OperationID: "user.update", Method: "PATCH", Pattern: "/api/v1/users/{id}"}:                                                 "same as user.read above: handleUserByID ignores an unrecognized suffix rather than 404ing on it",
	{OperationID: "user.admin.delete", Method: "DELETE", Pattern: "/api/v1/users/{id}"}:                                          "same as user.read above: handleUserByID ignores an unrecognized suffix rather than 404ing on it",
	{OperationID: "secret.read", Method: "GET", Pattern: "/api/v1/secrets/{key}"}:                                                "handleSecretByKey (handlers_env_secrets.go) extracts the key with extractID, which truncates at the first '/' after the prefix and discards the suffix, so the request proceeds on the bare key; with no secret backend configured in testServer, getSecret then panics on a nil backend, recovered as a 500 — not a 404/405, but unrelated to the suffix, which the truncation already removed",
	{OperationID: "secret.write", Method: "PUT", Pattern: "/api/v1/secrets/{key}"}:                                               "extractID truncates the suffix the same way as secret.read above, but setSecret checks the request body's value field before ever touching the secret backend; this test's generic empty PUT body always fails that check with 400 \"value is required\", the same 400 the bare path gets",
	{OperationID: "secret.write", Method: "DELETE", Pattern: "/api/v1/secrets/{key}"}:                                            "extractID truncates the suffix the same way as secret.read above; deleteSecret then panics on a nil secret backend the same way getSecret does, recovered as a 500",
	{OperationID: "hub.githubapp.update", Method: "PUT", Pattern: "/api/v1/github-app/installations/{id}"}:                       "parseInstallationIDFromPath (handlers_github_app.go) runs strconv.ParseInt on the entire remainder of the path, not just a leading segment, so a suffix makes parsing fail; the handler answers 400 \"invalid installation ID\", rejecting the suffix rather than ignoring it",
	{OperationID: "hub.githubapp.update", Method: "DELETE", Pattern: "/api/v1/github-app/installations/{id}"}:                    "same as the PUT installations/{id} entry above",
	{OperationID: "hub.githubapp.read", Method: "GET", Pattern: "/api/v1/github-app/installations/{id}"}:                         "same as the PUT installations/{id} entry above",
	{OperationID: "quota.read", Method: "GET", Pattern: "/api/v1/admin/entitlements/{id}"}:                                       "handleAdminEntitlementByID (handlers_quota.go) extracts the ID with extractID, which truncates at the first '/' after the prefix and discards the suffix, so the request proceeds on the real, un-suffixed ID and returns the entitlement (200) exactly as if the suffix were absent",
	{OperationID: "quota.update", Method: "PUT", Pattern: "/api/v1/admin/entitlements/{id}"}:                                     "same truncation as the GET entitlements/{id} entry above; this test's generic empty PUT body then fails updateEntitlement's validation with 400 before the (correctly extracted) ID is used for anything — the same 400 the bare path gets",
	{OperationID: "quota.delete", Method: "DELETE", Pattern: "/api/v1/admin/entitlements/{id}"}:                                  "same truncation as the GET entitlements/{id} entry above; deleteEntitlement then deletes the real, un-suffixed entitlement (204) exactly as if the suffix were absent",
	{OperationID: "quota.read", Method: "GET", Pattern: "/api/v1/admin/limits/{id}"}:                                             "handleAdminLimitByID (handlers_quota.go) splits on the first '/' and only special-cases a second segment of exactly \"entitlements\"; any other suffix is silently discarded, and the request proceeds on the real, un-suffixed ID",
	{OperationID: "quota.update", Method: "PUT", Pattern: "/api/v1/admin/limits/{id}"}:                                           "handleAdminLimitByID discards the suffix the same way as quota.read above; with a valid update body (see bodyOverrides) updateLimitDefinition succeeds on the real, un-suffixed ID and returns 200 exactly as if the suffix were absent",
	{OperationID: "quota.delete", Method: "DELETE", Pattern: "/api/v1/admin/limits/{id}"}:                                        "handleAdminLimitByID discards the suffix the same way as quota.read above; deleteLimitDefinition then fails deleting a limit this test's fixture entitlement still references, mapped to 400 — the same 400 happens on the bare path, independent of the suffix",
	{OperationID: "secret.read", Method: "GET", Pattern: "/api/v1/secrets"}:                                                      "the by-key route \"/api/v1/secrets/\" registers as a prefix, so a suffix on this bare collection route falls through to handleSecretByKey with the suffix as the key; getSecret then panics on a nil secret backend, recovered as a 500",
	{OperationID: "hub.lifecyclehooks.read", Method: "GET", Pattern: "/api/v1/admin/lifecycle-hooks"}:                            "the by-ID route \"/api/v1/admin/lifecycle-hooks/\" registers as a prefix, so a suffix on this bare collection route falls through to handleAdminLifecycleHookByID with the suffix as the ID; getLifecycleHook then 400s failing to parse it as a UUID",
	{OperationID: "hub.githubapp.read", Method: "GET", Pattern: "/api/v1/github-app/installations"}:                              "the by-ID route \"/api/v1/github-app/installations/\" registers as a prefix, so a suffix on this bare collection route falls through to handleGitHubAppInstallationByIDRead with the suffix as the ID; parseInstallationIDFromPath then 400s \"invalid installation ID\" failing to parse it as an integer",
	{OperationID: "agent.stopall", Method: "POST", Pattern: "/api/v1/agents/stop-all"}:                                           "handleAgentByID checks id == \"stop-all\" before it looks at anything after that segment, so a suffix is silently ignored and stop-all runs exactly as it would on the bare path",
	{OperationID: "quota.read", Method: "GET", Pattern: "/api/v1/admin/usage/{limit}"}:                                           "handleAdminUsageByLimit (handlers_quota.go) extracts limitID with extractID, which truncates at the first '/' and discards everything after it, so the suffix never reaches the lookup",
	{OperationID: "role.binding.read", Method: "GET", Pattern: "/api/v1/admin/role-bindings/user/{userId}"}:                      "handleAdminRoleBindingByID's \"user/\" branch (handlers_roles.go) takes the entire remaining path as the user ID with no further splitting; a nonexistent literal ID, suffixed or not, just returns an empty binding list (200), never a 404",
	{OperationID: "env.read", Method: "GET", Pattern: "/api/v1/env/{key}"}:                                                       "handleEnvVarByKey (handlers_env_secrets.go) extracts the key with extractID, which truncates at the first '/' and discards everything after it, so the suffix never reaches the lookup",
	{OperationID: "hub.lifecyclehooks.read", Method: "GET", Pattern: "/api/v1/admin/lifecycle-hooks/{id}"}:                       "handleAdminLifecycleHookByID (handlers_lifecycle_hooks.go) extracts the ID with extractID, which truncates at the first '/' and discards everything after it, so the suffix never reaches getLifecycleHook's lookup",
	{OperationID: "hub.lifecyclehooks.update", Method: "PUT", Pattern: "/api/v1/admin/lifecycle-hooks/{id}"}:                     "handleAdminLifecycleHookByID truncates the suffix with extractID the same way as the GET entry above, so the update runs on the real ID; its result (409 for the empty body's version check) is the same as on the bare path",
	{OperationID: "hub.lifecyclehooks.update", Method: "DELETE", Pattern: "/api/v1/admin/lifecycle-hooks/{id}"}:                  "handleAdminLifecycleHookByID truncates the suffix with extractID the same way as the GET entry above, so a suffixed DELETE would delete the real hook (204) exactly as the bare path does, and leave the positive check nothing to delete",
	{OperationID: "group.member.remove", Method: "DELETE", Pattern: "/api/v1/groups/{id}/members/{memberType}/{memberId}"}:       "handleGroupMemberByID (handlers_groups.go) splits memberPath into at most two parts, so a trailing suffix is appended onto memberID as one string rather than forming a separate segment; the resulting lookup fails with 400, not a routing 404",
	{OperationID: "hub.config.update", Method: "DELETE", Pattern: "/api/v1/admin/server-config/sections/{id}"}:                   "handleAdminServerConfigSectionReset (admin_settings.go) requires OperationalSettings and 400s \"Section reset requires DB-backed operational settings\" before it ever parses the section name from the path; testServer wires no OperationalSettings, so the same 400 happens on the bare path, independent of the suffix",
	{OperationID: "hub.maintenance.execute", Method: "POST", Pattern: "/api/v1/admin/maintenance/operations/{id}/run"}:           "handleAdminMaintenanceOps (admin_maintenance.go) splits the sub-path into at most three parts, so a fourth segment is absorbed into the \"run\" branch's own remainder rather than changing dispatch; combined with this entry's deliberate cross-category key (see patternOverrides), the resulting 400 is the same category-mismatch rejection as the bare path",
	{OperationID: "hub.metrics.read", Method: "GET", Pattern: "/api/v1/metrics/{name}"}:                                          "the metrics dashboard is not configured in testServer (no telemetry project ID); the resulting pre-dispatch 503 fires before path structure is examined, the same limitation the positive check documents in the test's doc comment",
	{OperationID: "chat.access", Method: "GET", Pattern: "/api/v1/chat/attachments/{id}"}:                                        "handleAttachmentDownload needs an attachment storage backend testServer does not configure; the resulting pre-dispatch 503 fires regardless of the suffix",
	{OperationID: "agent.portaccess", Method: "GET", Pattern: "/api/v1/agents/{id}/ports/{port}/proxy"}:                          "proxyAgentPort forwards the entire remaining path as part of the proxied request, with no path-based routing at all — the same design already excluded from the control check above",
	{OperationID: "agent.portaccess", Method: "POST", Pattern: "/api/v1/agents/{id}/ports/{port}/proxy"}:                         "same as the GET .../proxy suffix entry above",
	{OperationID: "agent.portaccess", Method: "PUT", Pattern: "/api/v1/agents/{id}/ports/{port}/proxy"}:                          "same as the GET .../proxy suffix entry above",
	{OperationID: "agent.portaccess", Method: "DELETE", Pattern: "/api/v1/agents/{id}/ports/{port}/proxy"}:                       "same as the GET .../proxy suffix entry above",
	{OperationID: "agent.portaccess", Method: "GET", Pattern: "/api/v1/agents/{id}/ports/{port}/proxy/{subpath}"}:                "same as the GET .../proxy suffix entry above",
	{OperationID: "agent.secrets.access", Method: "GET", Pattern: "/api/v1/agents/{id}/secrets"}:                                 "handleAgentSecrets (handlers_env_secrets.go) answers 501 when no secrets backend is configured, before it looks at the key; testServer configures no secrets backend, so a suffixed path gets the same 501 as the bare one",
	{OperationID: "agent.secrets.access", Method: "GET", Pattern: "/api/v1/agents/{id}/secrets/{key}"}:                           "same as the GET .../secrets suffix entry above",
	{OperationID: "agent.secrets.access", Method: "PUT", Pattern: "/api/v1/agents/{id}/secrets/{key}"}:                           "same as the GET .../secrets suffix entry above",
	{OperationID: "hub.policies.removed", Method: "GET", Pattern: "/api/v1/policies"}:                                            "the by-ID route \"/api/v1/policies/\" registers as a prefix, so a suffix on this bare collection route reaches handlePolicyRoutes, which answers 410 Gone for every sub-path by design",
	{OperationID: "hub.policies.removed", Method: "GET", Pattern: "/api/v1/policies/{id}"}:                                       "handlePolicyRoutes (handlers_policies.go) answers 410 Gone for every sub-path by design, so a suffixed path gets the same 410 as the bare one",
	{OperationID: "user.admin.invite", Method: "GET", Pattern: "/api/v1/admin/invites/{id}"}:                                     "handleAdminInviteByID (admin_invites.go:65-96) special-cases only a second segment of \"revoke\"; any other suffix, including this check's bogus one, falls through to the same GET handling as the bare ID",
	{OperationID: "inbox.conversation.create", Method: "POST", Pattern: "/api/v1/conversations/"}:                                "the pattern ends in a slash, so the suffixed path has an empty segment (\"//\"), which the server mux answers with a 307 redirect to the cleaned path before any handler runs",
	{OperationID: "inbox.conversation.direct.read", Method: "GET", Pattern: "/api/v1/conversations/{id}/messages"}:               "a suffix on the messages collection is the messages/{messageId} route; handleGetConversationMessage checks the direct conversation's key before it looks the message up, and the dev caller is not named in the key, so it answers 403 on any message ID",
	{OperationID: "user.admin.invite", Method: "DELETE", Pattern: "/api/v1/admin/invites/{id}"}:                                  "same as the GET invites/{id} suffix entry above — and because the suffix is silently ignored, a DELETE with a bogus suffix would delete the real fixture, so this exclusion also protects the positive check that runs after it",
	{OperationID: "user.read", Method: "GET", Pattern: "/api/v1/users/{id}"}:                                                     "handleUserByID (handlers_users_core.go) only special-cases the \"revoke-sessions\" suffix; any other suffix, including this check's bogus one, falls through to the same GET handling as the bare ID",
	{OperationID: "user.update", Method: "PATCH", Pattern: "/api/v1/users/{id}"}:                                                 "same as user.read above: handleUserByID ignores an unrecognized suffix rather than 404ing on it",
	{OperationID: "user.admin.delete", Method: "DELETE", Pattern: "/api/v1/users/{id}"}:                                          "same as user.read above: handleUserByID ignores an unrecognized suffix rather than 404ing on it",
	{OperationID: "secret.read", Method: "GET", Pattern: "/api/v1/secrets/{key}"}:                                                "handleSecretByKey (handlers_env_secrets.go) extracts the key with extractID, which truncates at the first '/' after the prefix and discards the suffix, so the request proceeds on the bare key; with no secret backend configured in testServer, getSecret then panics on a nil backend, recovered as a 500 — not a 404/405, but unrelated to the suffix, which the truncation already removed",
	{OperationID: "secret.write", Method: "PUT", Pattern: "/api/v1/secrets/{key}"}:                                               "extractID truncates the suffix the same way as secret.read above, but setSecret checks the request body's value field before ever touching the secret backend; this test's generic empty PUT body always fails that check with 400 \"value is required\", the same 400 the bare path gets",
	{OperationID: "secret.write", Method: "DELETE", Pattern: "/api/v1/secrets/{key}"}:                                            "extractID truncates the suffix the same way as secret.read above; deleteSecret then panics on a nil secret backend the same way getSecret does, recovered as a 500",
	{OperationID: "hub.githubapp.update", Method: "PUT", Pattern: "/api/v1/github-app/installations/{id}"}:                       "parseInstallationIDFromPath (handlers_github_app.go) runs strconv.ParseInt on the entire remainder of the path, not just a leading segment, so a suffix makes parsing fail; the handler answers 400 \"invalid installation ID\", rejecting the suffix rather than ignoring it",
	{OperationID: "hub.githubapp.update", Method: "DELETE", Pattern: "/api/v1/github-app/installations/{id}"}:                    "same as the PUT installations/{id} entry above",
	{OperationID: "hub.githubapp.read", Method: "GET", Pattern: "/api/v1/github-app/installations/{id}"}:                         "same as the PUT installations/{id} entry above",
	{OperationID: "quota.read", Method: "GET", Pattern: "/api/v1/admin/entitlements/{id}"}:                                       "handleAdminEntitlementByID (handlers_quota.go) extracts the ID with extractID, which truncates at the first '/' after the prefix and discards the suffix, so the request proceeds on the real, un-suffixed ID and returns the entitlement (200) exactly as if the suffix were absent",
	{OperationID: "quota.update", Method: "PUT", Pattern: "/api/v1/admin/entitlements/{id}"}:                                     "same truncation as the GET entitlements/{id} entry above; this test's generic empty PUT body then fails updateEntitlement's validation with 400 before the (correctly extracted) ID is used for anything — the same 400 the bare path gets",
	{OperationID: "quota.delete", Method: "DELETE", Pattern: "/api/v1/admin/entitlements/{id}"}:                                  "same truncation as the GET entitlements/{id} entry above; deleteEntitlement then deletes the real, un-suffixed entitlement (204) exactly as if the suffix were absent",
	{OperationID: "quota.read", Method: "GET", Pattern: "/api/v1/admin/limits/{id}"}:                                             "handleAdminLimitByID (handlers_quota.go) splits on the first '/' and only special-cases a second segment of exactly \"entitlements\"; any other suffix is silently discarded, and the request proceeds on the real, un-suffixed ID",
	{OperationID: "quota.update", Method: "PUT", Pattern: "/api/v1/admin/limits/{id}"}:                                           "handleAdminLimitByID discards the suffix the same way as quota.read above; with a valid update body (see bodyOverrides) updateLimitDefinition succeeds on the real, un-suffixed ID and returns 200 exactly as if the suffix were absent",
	{OperationID: "quota.delete", Method: "DELETE", Pattern: "/api/v1/admin/limits/{id}"}:                                        "handleAdminLimitByID discards the suffix the same way as quota.read above; deleteLimitDefinition then fails deleting a limit this test's fixture entitlement still references, mapped to 400 — the same 400 happens on the bare path, independent of the suffix",
	{OperationID: "secret.read", Method: "GET", Pattern: "/api/v1/secrets"}:                                                      "the by-key route \"/api/v1/secrets/\" registers as a prefix, so a suffix on this bare collection route falls through to handleSecretByKey with the suffix as the key; getSecret then panics on a nil secret backend, recovered as a 500",
	{OperationID: "hub.lifecyclehooks.read", Method: "GET", Pattern: "/api/v1/admin/lifecycle-hooks"}:                            "the by-ID route \"/api/v1/admin/lifecycle-hooks/\" registers as a prefix, so a suffix on this bare collection route falls through to handleAdminLifecycleHookByID with the suffix as the ID; getLifecycleHook then 400s failing to parse it as a UUID",
	{OperationID: "hub.githubapp.read", Method: "GET", Pattern: "/api/v1/github-app/installations"}:                              "the by-ID route \"/api/v1/github-app/installations/\" registers as a prefix, so a suffix on this bare collection route falls through to handleGitHubAppInstallationByIDRead with the suffix as the ID; parseInstallationIDFromPath then 400s \"invalid installation ID\" failing to parse it as an integer",
	{OperationID: "agent.stopall", Method: "POST", Pattern: "/api/v1/agents/stop-all"}:                                           "handleAgentByID checks id == \"stop-all\" before it looks at anything after that segment, so a suffix is silently ignored and stop-all runs exactly as it would on the bare path",
	{OperationID: "quota.read", Method: "GET", Pattern: "/api/v1/admin/usage/{limit}"}:                                           "handleAdminUsageByLimit (handlers_quota.go) extracts limitID with extractID, which truncates at the first '/' and discards everything after it, so the suffix never reaches the lookup",
	{OperationID: "role.binding.read", Method: "GET", Pattern: "/api/v1/admin/role-bindings/user/{userId}"}:                      "handleAdminRoleBindingByID's \"user/\" branch (handlers_roles.go) takes the entire remaining path as the user ID with no further splitting; a nonexistent literal ID, suffixed or not, just returns an empty binding list (200), never a 404",
	{OperationID: "env.read", Method: "GET", Pattern: "/api/v1/env/{key}"}:                                                       "handleEnvVarByKey (handlers_env_secrets.go) extracts the key with extractID, which truncates at the first '/' and discards everything after it, so the suffix never reaches the lookup",
	{OperationID: "hub.lifecyclehooks.read", Method: "GET", Pattern: "/api/v1/admin/lifecycle-hooks/{id}"}:                       "handleAdminLifecycleHookByID (handlers_lifecycle_hooks.go) extracts the ID with extractID, which truncates at the first '/' and discards everything after it, so the suffix never reaches getLifecycleHook's lookup",
	{OperationID: "group.member.remove", Method: "DELETE", Pattern: "/api/v1/groups/{id}/members/{memberType}/{memberId}"}:       "handleGroupMemberByID (handlers_groups.go) splits memberPath into at most two parts, so a trailing suffix is appended onto memberID as one string rather than forming a separate segment; the resulting lookup fails with 400, not a routing 404",
	{OperationID: "hub.config.update", Method: "DELETE", Pattern: "/api/v1/admin/server-config/sections/{id}"}:                   "handleAdminServerConfigSectionReset (admin_settings.go) requires OperationalSettings and 400s \"Section reset requires DB-backed operational settings\" before it ever parses the section name from the path; testServer wires no OperationalSettings, so the same 400 happens on the bare path, independent of the suffix",
	{OperationID: "hub.maintenance.execute", Method: "POST", Pattern: "/api/v1/admin/maintenance/operations/{id}/run"}:           "handleAdminMaintenanceOps (admin_maintenance.go) splits the sub-path into at most three parts, so a fourth segment is absorbed into the \"run\" branch's own remainder rather than changing dispatch; combined with this entry's deliberate cross-category key (see patternOverrides), the resulting 400 is the same category-mismatch rejection as the bare path",
	{OperationID: "hub.metrics.read", Method: "GET", Pattern: "/api/v1/metrics/{name}"}:                                          "the metrics dashboard is not configured in testServer (no telemetry project ID); the resulting pre-dispatch 503 fires before path structure is examined, the same limitation the positive check documents in the test's doc comment",
	{OperationID: "chat.access", Method: "GET", Pattern: "/api/v1/chat/attachments/{id}"}:                                        "handleAttachmentDownload needs an attachment storage backend testServer does not configure; the resulting pre-dispatch 503 fires regardless of the suffix",
	{OperationID: "agent.portaccess", Method: "GET", Pattern: "/api/v1/agents/{id}/ports/{port}/proxy"}:                          "proxyAgentPort forwards the entire remaining path as part of the proxied request, with no path-based routing at all — the same design already excluded from the control check above",
	{OperationID: "agent.portaccess", Method: "POST", Pattern: "/api/v1/agents/{id}/ports/{port}/proxy"}:                         "same as the GET .../proxy suffix entry above",
	{OperationID: "agent.portaccess", Method: "PUT", Pattern: "/api/v1/agents/{id}/ports/{port}/proxy"}:                          "same as the GET .../proxy suffix entry above",
	{OperationID: "agent.portaccess", Method: "DELETE", Pattern: "/api/v1/agents/{id}/ports/{port}/proxy"}:                       "same as the GET .../proxy suffix entry above",
	{OperationID: "agent.portaccess", Method: "GET", Pattern: "/api/v1/agents/{id}/ports/{port}/proxy/{subpath}"}:                "same as the GET .../proxy suffix entry above",
	{OperationID: "agent.secrets.access", Method: "GET", Pattern: "/api/v1/agents/{id}/secrets"}:                                 "handleAgentSecrets (handlers_env_secrets.go) answers 501 when no secrets backend is configured, before it looks at the key; testServer configures no secrets backend, so a suffixed path gets the same 501 as the bare one",
	{OperationID: "agent.secrets.access", Method: "GET", Pattern: "/api/v1/agents/{id}/secrets/{key}"}:                           "same as the GET .../secrets suffix entry above",
	{OperationID: "agent.secrets.access", Method: "PUT", Pattern: "/api/v1/agents/{id}/secrets/{key}"}:                           "same as the GET .../secrets suffix entry above",
	{OperationID: "hub.policies.removed", Method: "GET", Pattern: "/api/v1/policies"}:                                            "the by-ID route \"/api/v1/policies/\" registers as a prefix, so a suffix on this bare collection route reaches handlePolicyRoutes, which answers 410 Gone for every sub-path by design",
	{OperationID: "hub.policies.removed", Method: "GET", Pattern: "/api/v1/policies/{id}"}:                                       "handlePolicyRoutes (handlers_policies.go) answers 410 Gone for every sub-path by design, so a suffixed path gets the same 410 as the bare one",
	{OperationID: "user.admin.invite", Method: "GET", Pattern: "/api/v1/admin/invites/{id}"}:                                     "handleAdminInviteByID (admin_invites.go:65-96) special-cases only a second segment of \"revoke\"; any other suffix, including this check's bogus one, falls through to the same GET handling as the bare ID",
	{OperationID: "project.workspace.read", Method: "GET", Pattern: "/api/v1/projects/{id}/dav/{path}"}:                          "the WebDAV handler takes the whole remaining path as the workspace resource path, so a trailing segment names a deeper resource rather than an undeclared route",
	{OperationID: "project.workspace.read", Method: "HEAD", Pattern: "/api/v1/projects/{id}/dav/{path}"}:                         "same as the WebDAV GET suffix entry above",
	{OperationID: "project.workspace.read", Method: "OPTIONS", Pattern: "/api/v1/projects/{id}/dav/{path}"}:                      "same as the WebDAV GET suffix entry above",
	{OperationID: "project.workspace.write", Method: "PUT", Pattern: "/api/v1/projects/{id}/dav/{path}"}:                         "same as the WebDAV GET suffix entry above",
	{OperationID: "project.workspace.write", Method: "PROPPATCH", Pattern: "/api/v1/projects/{id}/dav/{path}"}:                   "same as the WebDAV GET suffix entry above",
	{OperationID: "project.workspace.write", Method: "LOCK", Pattern: "/api/v1/projects/{id}/dav/{path}"}:                        "same as the WebDAV GET suffix entry above",
	{OperationID: "project.workspace.write", Method: "UNLOCK", Pattern: "/api/v1/projects/{id}/dav/{path}"}:                      "same as the WebDAV GET suffix entry above",
	{OperationID: "project.workspace.write", Method: "COPY", Pattern: "/api/v1/projects/{id}/dav/{path}"}:                        "same as the WebDAV GET suffix entry above",
	{OperationID: "project.workspace.write", Method: "MOVE", Pattern: "/api/v1/projects/{id}/dav/{path}"}:                        "same as the WebDAV GET suffix entry above",
	{OperationID: "project.workspace.write", Method: "POST", Pattern: "/api/v1/projects/{id}/dav/{path}"}:                        "same as the WebDAV GET suffix entry above",
	{OperationID: "project.workspace.write", Method: "PROPFIND", Pattern: "/api/v1/projects/{id}/dav/{path}"}:                    "same as the WebDAV GET suffix entry above",
	{OperationID: "project.workspace.write", Method: "DELETE", Pattern: "/api/v1/projects/{id}/dav/{path}"}:                      "same as the WebDAV GET suffix entry above",
	{OperationID: "project.workspace.write", Method: "MKCOL", Pattern: "/api/v1/projects/{id}/dav/{path}"}:                       "same as the WebDAV GET suffix entry above",
	{OperationID: "project.workspace.read", Method: "GET", Pattern: "/api/v1/projects/{id}/workspace/files/{path}"}:              "handleProjectWorkspace takes the whole remaining path as the file path; a trailing segment below the seeded regular file is a path through a non-directory, answered 400",
	{OperationID: "project.shareddir.read", Method: "GET", Pattern: "/api/v1/projects/{id}/shared-dirs/{name}/files/{path}"}:     "handleSharedDirFiles takes the whole remaining path as the file path; a trailing segment below the seeded regular file is a path through a non-directory, answered 400",
	{OperationID: "project.workspace.write", Method: "PUT", Pattern: "/api/v1/projects/{id}/workspace/files/{path}"}:             "handleProjectWorkspace takes the whole remaining path as the file path, so a trailing segment names a deeper file (written, 200) rather than an undeclared route",
	{OperationID: "project.workspace.write", Method: "DELETE", Pattern: "/api/v1/projects/{id}/workspace/files/{path}"}:          "same as the workspace files PUT entry above; the suffixed DELETE removes the deeper file the suffixed PUT wrote",
	{OperationID: "project.shareddir.write", Method: "PUT", Pattern: "/api/v1/projects/{id}/shared-dirs/{name}/files/{path}"}:    "handleSharedDirFiles takes the whole remaining path as the file path, so a trailing segment names a deeper file (written, 200) rather than an undeclared route",
	{OperationID: "project.shareddir.write", Method: "DELETE", Pattern: "/api/v1/projects/{id}/shared-dirs/{name}/files/{path}"}: "same as the shared-dir files PUT entry above; the suffixed DELETE removes the deeper file the suffixed PUT wrote",
	{OperationID: "project.env.write", Method: "PUT", Pattern: "/api/v1/projects/{id}/env/{key}"}:                                "handleProjectEnvVarByKey takes the whole remaining path as the key, and the key validation rejects the '/' with 400 before any lookup",
	{OperationID: "project.secret.read", Method: "GET", Pattern: "/api/v1/projects/{id}/secrets"}:                                "the by-key branch takes a suffix on the collection route as a key; with no secret backend configured in testServer the read panics on the nil backend, recovered as a 500",
	{OperationID: "project.secret.read", Method: "GET", Pattern: "/api/v1/projects/{id}/secrets/{key}"}:                          "handleProjectSecretByKey takes the whole remaining path as the key; with no secret backend configured in testServer the read panics on the nil backend, recovered as a 500",
	{OperationID: "project.secret.write", Method: "PUT", Pattern: "/api/v1/projects/{id}/secrets/{key}"}:                         "handleProjectSecretByKey takes the whole remaining path as the key, and this test's empty PUT body fails the value check with 400, the same 400 the bare path gets",
	{OperationID: "project.secret.write", Method: "PATCH", Pattern: "/api/v1/projects/{id}/secrets/{key}"}:                       "same as the secrets GET by-key suffix entry above: the nil secret backend panics, recovered as a 500",
	{OperationID: "project.secret.write", Method: "DELETE", Pattern: "/api/v1/projects/{id}/secrets/{key}"}:                      "same as the secrets GET by-key suffix entry above: the nil secret backend panics, recovered as a 500",
	{OperationID: "project.metrics.read", Method: "GET", Pattern: "/api/v1/projects/{id}/metrics/summary"}:                       "a suffix on metrics/summary falls through to the metrics dashboard branch, which accepts any metrics/... path and answers 503 because testServer configures no telemetry project",
	{OperationID: "project.metrics.read", Method: "GET", Pattern: "/api/v1/projects/{id}/metrics"}:                               "handleProjectMetricsDashboard accepts any metrics/... path and answers 503 because testServer configures no telemetry project, before path structure is examined",
	{OperationID: "user.admin.invite", Method: "DELETE", Pattern: "/api/v1/admin/invites/{id}"}:                                  "same as the GET invites/{id} suffix entry above — and because the suffix is silently ignored, a DELETE with a bogus suffix would delete the real fixture, so this exclusion also protects the positive check that runs after it",
	{OperationID: "hub.lifecyclehooks.update", Method: "PUT", Pattern: "/api/v1/admin/lifecycle-hooks/{id}"}:                     "handleAdminLifecycleHookByID truncates the suffix with extractID the same way as the GET entry above, so the update runs on the real ID; its result (409 for the empty body's version check) is the same as on the bare path",
	{OperationID: "hub.lifecyclehooks.update", Method: "DELETE", Pattern: "/api/v1/admin/lifecycle-hooks/{id}"}:                  "handleAdminLifecycleHookByID truncates the suffix with extractID the same way as the GET entry above, so a suffixed DELETE would delete the real hook (204) exactly as the bare path does, and leave the positive check nothing to delete",
	{OperationID: "hub.integrations.read", Method: "GET", Pattern: "/api/v1/admin/integrations/{name}/health"}:                   "handleAdminIntegrationByName (handlers_integrations.go) splits the sub-path into at most three parts and the health branch ignores the third, so a suffix reaches the same health check as the bare path",
	{OperationID: "hub.integrations.read", Method: "GET", Pattern: "/api/v1/admin/integrations/{name}/update/{id}"}:              "handleAdminIntegrationByName splits the sub-path into at most three parts, so the suffix joins the update ID as one string and the status lookup answers for that ID instead of a routing 404",
	{OperationID: "hub.integrations.update", Method: "PUT", Pattern: "/api/v1/admin/integrations/{name}/config"}:                 "handleAdminIntegrationByName splits the sub-path into at most three parts and the config branch ignores the third, so a suffix reaches the same config update as the bare path",
	{OperationID: "hub.integrations.update", Method: "POST", Pattern: "/api/v1/admin/integrations/{name}/restart"}:               "handleAdminIntegrationByName splits the sub-path into at most three parts and the restart branch ignores the third, so a suffix restarts the integration as the bare path does",
	{OperationID: "hub.integrations.install", Method: "POST", Pattern: "/api/v1/admin/integrations/{name}/install"}:              "handleAdminIntegrationByName splits the sub-path into at most three parts and the install branch ignores the third, so a suffix reaches the same install handling as the bare path",
	{OperationID: "hub.integrations.install", Method: "POST", Pattern: "/api/v1/admin/integrations/{name}/update"}:               "handleAdminIntegrationByName splits the sub-path into at most three parts and a POST to update with a third part starts the same update as the bare path",
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
