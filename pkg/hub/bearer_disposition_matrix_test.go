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
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/hub/authzop"
	"github.com/GoogleCloudPlatform/scion/pkg/hub/permissions"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/require"
)

// bearerMatrixExclusion names why one catalog entry point is not exercised
// by TestBearerDispositionMatrix_CatalogEntryPoints, and the dedicated test
// that pins its disposition instead.
type bearerMatrixExclusion struct {
	Reason string
	Pin    string
}

// bearerMatrixExclusions lists catalog HTTP, SSE and WebSocket entry points
// whose recorded disposition the generic matrix cannot exercise. Each one
// carries a specific reason and the name of the test that pins it.
// TestBearerMatrixExclusions_NotStaleAndPinned keeps every key live and
// every Pin declared.
var bearerMatrixExclusions = map[liveInventoryKey]bearerMatrixExclusion{
	{OperationID: "broker.read", Method: "GET", Pattern: "/api/v1/runtime-brokers/{id}/conversion-plan"}: {
		Reason: "the route answers 404 while the hub.flat_runtime_brokers experiment is off, as in this fixture, and for a Runtime Broker that never reported a conversion plan, so the admitting token cannot reach a target here. The dedicated test enables the experiment, stores a plan from the broker's heartbeat and drives the route with a broker:read token and a token without it",
		Pin:    "TestConversionPlanGet_ScopedTokens",
	},
	{OperationID: "hub.githubapp.read", Method: "GET", Pattern: "/api/v1/github-app/repo-routing"}: {
		Reason: "the route answers 404 while the hub.github_triggers experiment is off, as in this fixture, so the admitting token cannot reach the handler here. The dedicated test enables the experiment and drives GET, PUT and DELETE with hub- and project-boundary tokens carrying and lacking hub_github_app:read and hub_github_app:update",
		Pin:    "TestGitHubAppRepoRouting_ScopedTokens",
	},
	{OperationID: "hub.githubapp.update", Method: "PUT", Pattern: "/api/v1/github-app/repo-routing/{owner}/{repo}"}: {
		Reason: "the route answers 404 while the hub.github_triggers experiment is off, as in this fixture, so the admitting token cannot reach the handler here. The dedicated test enables the experiment and drives GET, PUT and DELETE with hub- and project-boundary tokens carrying and lacking hub_github_app:read and hub_github_app:update",
		Pin:    "TestGitHubAppRepoRouting_ScopedTokens",
	},
	{OperationID: "hub.githubapp.update", Method: "DELETE", Pattern: "/api/v1/github-app/repo-routing/{owner}/{repo}"}: {
		Reason: "the route answers 404 while the hub.github_triggers experiment is off, as in this fixture, so the admitting token cannot reach the handler here. The dedicated test enables the experiment and drives GET, PUT and DELETE with hub- and project-boundary tokens carrying and lacking hub_github_app:read and hub_github_app:update",
		Pin:    "TestGitHubAppRepoRouting_ScopedTokens",
	},
	{OperationID: "project.triggersources.github.read", Method: "GET", Pattern: "/api/v1/projects/{id}/trigger-sources/github"}: {
		Reason: "the route answers 404 while the hub.github_triggers experiment is off, as in this fixture, so the admitting token cannot reach the handler here. The dedicated test enables the experiment and drives GET and PUT with project- and hub-boundary tokens carrying and lacking project:read and github_trigger:configure",
		Pin:    "TestProjectGitHubTriggerSettings_ScopedTokens",
	},
	{OperationID: "project.triggersources.github.update", Method: "PUT", Pattern: "/api/v1/projects/{id}/trigger-sources/github"}: {
		Reason: "the route answers 404 while the hub.github_triggers experiment is off, as in this fixture, so the admitting token cannot reach the handler here. The dedicated test enables the experiment and drives GET and PUT with project- and hub-boundary tokens carrying and lacking project:read and github_trigger:configure",
		Pin:    "TestProjectGitHubTriggerSettings_ScopedTokens",
	},
	{OperationID: "role.binding.effectivepermissions", Method: "GET", Pattern: "/api/v1/admin/effective-permissions"}: {
		Reason: "the route answers 404 while the hub.effective_permissions experiment is off, as in this fixture, so the admitting token cannot reach the handler here. The dedicated test enables the experiment and drives GET and HEAD with hub- and project-boundary tokens carrying and lacking role_binding:read",
		Pin:    "TestAdminEffectivePermissions_ScopedTokens",
	},
	{OperationID: "role.binding.effectivepermissions", Method: "HEAD", Pattern: "/api/v1/admin/effective-permissions"}: {
		Reason: "the route answers 404 while the hub.effective_permissions experiment is off, as in this fixture, so the admitting token cannot reach the handler here. The dedicated test enables the experiment and drives GET and HEAD with hub- and project-boundary tokens carrying and lacking role_binding:read",
		Pin:    "TestAdminEffectivePermissions_ScopedTokens",
	},
	{OperationID: "agent.secret.read", Method: "GET", Pattern: "/api/v1/secrets"}: {
		Reason: "the route defaults to scope=user (session-only) without scope=agent&scopeId=<agentID> and rejects scope=agent while hub.agent_secrets is off in this fixture. The dedicated test enables hub.agent_secrets and drives /api/v1/secrets?scope=agent&scopeId=... with agent:read, agent:update, unrelated-selector, and cross-project user access tokens",
		Pin:    "TestAgentSecrets_ScopedTokens",
	},
	{OperationID: "agent.secret.read", Method: "GET", Pattern: "/api/v1/secrets/{key}"}: {
		Reason: "the route defaults to scope=user (session-only) without scope=agent&scopeId=<agentID> and rejects scope=agent while hub.agent_secrets is off in this fixture. The dedicated test enables hub.agent_secrets and drives /api/v1/secrets/{key}?scope=agent&scopeId=... with agent:read, agent:update, unrelated-selector, and cross-project user access tokens",
		Pin:    "TestAgentSecrets_ScopedTokens",
	},
	{OperationID: "agent.secret.write", Method: "PUT", Pattern: "/api/v1/secrets/{key}"}: {
		Reason: "the route defaults to scope=user (session-only) without scope=agent&scopeId=<agentID> and rejects scope=agent while hub.agent_secrets is off in this fixture. The dedicated test enables hub.agent_secrets and drives PUT /api/v1/secrets/{key} with scope=agent using agent:read, agent:update, unrelated-selector, and cross-project user access tokens",
		Pin:    "TestAgentSecrets_ScopedTokens",
	},
	{OperationID: "agent.secret.write", Method: "PATCH", Pattern: "/api/v1/secrets/{key}"}: {
		Reason: "the route defaults to scope=user (session-only) without scope=agent&scopeId=<agentID> and rejects scope=agent while hub.agent_secrets is off in this fixture. The dedicated test enables hub.agent_secrets and drives PATCH /api/v1/secrets/{key}?scope=agent&scopeId=... with agent:read, agent:update, unrelated-selector, and cross-project user access tokens",
		Pin:    "TestAgentSecrets_ScopedTokens",
	},
	{OperationID: "agent.secret.write", Method: "DELETE", Pattern: "/api/v1/secrets/{key}"}: {
		Reason: "the route defaults to scope=user (session-only) without scope=agent&scopeId=<agentID> and rejects scope=agent while hub.agent_secrets is off in this fixture. The dedicated test enables hub.agent_secrets and drives DELETE /api/v1/secrets/{key}?scope=agent&scopeId=... with agent:read, agent:update, unrelated-selector, and cross-project user access tokens",
		Pin:    "TestAgentSecrets_ScopedTokens",
	},
	{OperationID: "project.membership.add", Method: "POST", Pattern: "/api/v1/projects/{id}/members"}: {
		Reason: "a token acts on membership only through its holder's project role, and the fixture super-admin holds no role in the fixture project; a grant also needs selectors for every permission of the granted role (CanDelegate). The dedicated test drives the route with real tokens on both boundaries and with the holder's session",
		Pin:    "TestMembershipToken_SelectorAndBoundaryPerRoute",
	},
	{OperationID: "project.membership.update", Method: "PATCH", Pattern: "/api/v1/projects/{id}/members/{memberId}"}: {
		Reason: "a token acts on membership only through its holder's project role, and the fixture super-admin holds no role in the fixture project; a grant also needs selectors for every permission of the granted role (CanDelegate). The dedicated test drives the route with real tokens on both boundaries and with the holder's session",
		Pin:    "TestMembershipToken_SelectorAndBoundaryPerRoute",
	},
	{OperationID: "project.membership.remove", Method: "DELETE", Pattern: "/api/v1/projects/{id}/members/{memberId}"}: {
		Reason: "a token acts on membership only through its holder's project role, and the fixture super-admin holds no role in the fixture project. The dedicated test drives the route with real tokens on both boundaries and with the holder's session",
		Pin:    "TestMembershipToken_SelectorAndBoundaryPerRoute",
	},
	{OperationID: "project.membership.transfer", Method: "POST", Pattern: "/api/v1/projects/{id}/transfer-ownership"}: {
		Reason: "a token acts on membership only through its holder's project role, and the fixture super-admin holds no role in the fixture project; a grant also needs selectors for every permission of the granted role (CanDelegate). The dedicated test drives the route with real tokens on both boundaries and with the holder's session",
		Pin:    "TestMembershipToken_SelectorAndBoundaryPerRoute",
	},
	{OperationID: "project.membership.principal.set", Method: "PUT", Pattern: "/api/v1/projects/{id}/members/principals/{type}/{principalId}"}: {
		Reason: "a token acts on membership only through its holder's project role, and the fixture super-admin holds no role in the fixture project; a grant also needs selectors for every permission of the granted role (CanDelegate). The dedicated test drives the route with real tokens on both boundaries and with the holder's session",
		Pin:    "TestMembershipToken_SelectorAndBoundaryPerRoute",
	},
	{OperationID: "project.membership.principal.remove", Method: "DELETE", Pattern: "/api/v1/projects/{id}/members/principals/{type}/{principalId}"}: {
		Reason: "a token acts on membership only through its holder's project role, and the fixture super-admin holds no role in the fixture project. The dedicated test drives the route with real tokens on both boundaries and with the holder's session",
		Pin:    "TestMembershipToken_SelectorAndBoundaryPerRoute",
	},
	{OperationID: "project.membership.rolebinding.create", Method: "POST", Pattern: "/api/v1/admin/role-bindings"}: {
		Reason: "a token acts on membership only through its holder's project role, and the fixture super-admin holds no role in the fixture project; a grant also needs selectors for every permission of the granted role (CanDelegate). The dedicated test drives the route with real tokens on both boundaries and with the holder's session",
		Pin:    "TestMembershipToken_SelectorAndBoundaryPerRoute",
	},
	{OperationID: "project.membership.rolebinding.delete", Method: "DELETE", Pattern: "/api/v1/admin/role-bindings/{id}"}: {
		Reason: "a token acts on membership only through its holder's project role, and the fixture super-admin holds no role in the fixture project. The dedicated test drives the route with real tokens on both boundaries and with the holder's session",
		Pin:    "TestMembershipToken_SelectorAndBoundaryPerRoute",
	},
	{OperationID: "project.lifecycle.create", Method: "POST", Pattern: "/api/v1/projects"}: {
		Reason: "the creator is bound to the seeded project-owner role, and a token must cover every permission of that role, some of which have no selector; the dedicated test narrows the owner role to permissions with selectors and drives the route with real tokens on both boundaries and with the holder's session",
		Pin:    "TestProjectCreateToken_RequiresCreateSelectorAndHubBoundary",
	},
	{OperationID: "project.register", Method: "POST", Pattern: "/api/v1/projects/register"}: {
		Reason: "the creator is bound to the seeded project-owner role, and a token must cover every permission of that role, some of which have no selector; the dedicated test narrows the owner role to permissions with selectors and drives the route with real tokens on both boundaries and with the holder's session",
		Pin:    "TestProjectRegisterToken_EnforcesProjectCreate",
	},
	{OperationID: "project.clone", Method: "POST", Pattern: "/api/v1/projects/{id}/clone"}: {
		Reason: "a clone needs project:update, project:clone and project:create together, the creator is bound to the seeded project-owner role (some of its permissions have no selector), and only a super-admin can mint project:clone; the dedicated test sets this up and drives the route with real tokens and with the holder's session",
		Pin:    "TestProjectCloneToken_RequiresSourceReadCloneAndCreate",
	},
	{OperationID: "project.lifecycle.delete", Method: "DELETE", Pattern: "/api/v1/projects/{id}"}: {
		Reason: "project.delete has no token selector, so a token is refused by the deletion service's base permission check (403 project_delete_forbidden) before its session-only credential step is reached; the session-only step and its IRREVERSIBLE_CASCADE reason are pinned at the service",
		Pin:    "TestRS3_ProjectDeleteScopedUATDenied",
	},
	{OperationID: "harnessconfig.list", Method: "GET", Pattern: "/api/v1/harness-configs"}: {
		Reason: "the collection list filters each row by harness_config.read instead of refusing the request, so a token without the selector gets 200 with no rows rather than 403; the unfiltered list needs harness_config:list on a hub boundary",
		Pin:    "TestCatalogWideList_UsesListSelectorThroughEvidence",
	},
	{OperationID: "template.list", Method: "GET", Pattern: "/api/v1/templates"}: {
		Reason: "the collection list filters each row by template.read instead of refusing the request, so a token without the selector gets 200 with no rows rather than 403; the unfiltered list needs template:list on a hub boundary",
		Pin:    "TestCatalogWideList_UsesListSelectorThroughEvidence",
	},
	{OperationID: "skill.list", Method: "GET", Pattern: "/api/v1/skills"}: {
		Reason: "a token without skill:list, or bounded to another project, gets 200 with no rows rather than 403: the list never refuses the request",
		Pin:    "TestSkillListToken_CountsOnlyAuthorizedRows",
	},
	{OperationID: "project.list", Method: "GET", Pattern: "/api/v1/projects"}: {
		Reason: "a token without project:list, or bounded to another project, gets 200 with no rows rather than 403: the list never refuses the request",
		Pin:    "TestProjectListToken_RequiresProjectListSelector",
	},
	{OperationID: "agent.list", Method: "GET", Pattern: "/api/v1/agents"}: {
		Reason: "a token without agent:list, or bounded to another project, gets 200 with no rows rather than 403: the list never refuses the request",
		Pin:    "TestListCaveats_ProjectBoundaryRequiresListPermissionInCeiling",
	},
	{OperationID: "inbox.message.read", Method: "GET", Pattern: "/api/v1/messages"}: {
		Reason: "the message list filters rows to the token boundary instead of refusing the request, so a project token for another project gets 200 with no rows",
		Pin:    "TestInboxToken_ProjectBoundaryFiltersMessages",
	},
	{OperationID: "inbox.message.write", Method: "POST", Pattern: "/api/v1/messages/read-all"}: {
		Reason: "mark-all-read touches only rows inside the token boundary instead of refusing the request, so a project token for another project gets 200 and marks nothing of the fixture project",
		Pin:    "TestInboxToken_MarkAllReadTouchesOnlyVisibleRows",
	},
	{OperationID: "inbox.conversation.list", Method: "GET", Pattern: "/api/v1/conversations"}: {
		Reason: "the conversation list filters rows to the token boundary instead of refusing the request, so a project token for another project gets 200 with no rows",
		Pin:    "TestConversationListToken_FilteredToBoundary",
	},
	{OperationID: "agent.message.target.resolve", Method: "GET", Pattern: "/api/v1/messaging/targets/resolve"}: {
		Reason: "target resolution answers 404 for every caller while cross-project messaging is off, which it is in the matrix server; the pin enables it",
		Pin:    "TestMessagingTargetsResolve_TokenNeedsAgentMessage",
	},
	{OperationID: "inbox.notification.read", Method: "GET", Pattern: "/api/v1/notifications"}: {
		Reason: "the notification list filters rows to the token boundary instead of refusing the request, so a project token for another project gets 200 with no rows",
		Pin:    "TestNotificationToken_RowsFilteredToBoundary",
	},
	{OperationID: "inbox.notification.ack", Method: "POST", Pattern: "/api/v1/notifications/ack-all"}: {
		Reason: "ack-all touches only rows inside the token boundary instead of refusing the request, so a project token for another project gets 200",
		Pin:    "TestNotificationToken_RowsFilteredToBoundary",
	},
	{OperationID: "inbox.notification.subscription.create", Method: "POST", Pattern: "/api/v1/notifications/subscriptions/bulk"}: {
		Reason: "bulk create takes a JSON array, which the matrix body overrides cannot express; with an object body every token gets 400 after the selector check",
		Pin:    "TestNotificationSubscription_RequiresProjectAndAgentRead",
	},
	{OperationID: "inbox.notification.subscription.read", Method: "GET", Pattern: "/api/v1/notifications/subscriptions"}: {
		Reason: "the subscription list filters rows to the token boundary instead of refusing the request, so a project token for another project gets 200 with no rows",
		Pin:    "TestNotificationToken_RowsFilteredToBoundary",
	},
	{OperationID: "inbox.notification.subscription.write", Method: "POST", Pattern: "/api/v1/notifications/subscriptions/bulk-delete"}: {
		Reason: "bulk delete skips rows outside the token boundary instead of refusing the request, so a project token for another project gets 200 with nothing deleted",
		Pin:    "TestNotificationToken_RowsFilteredToBoundary",
	},
	{OperationID: "inbox.notification.template.read", Method: "GET", Pattern: "/api/v1/notifications/templates"}: {
		Reason: "the template list filters rows to readable projects inside the token boundary instead of refusing the request, so a project token for another project gets 200 with no rows",
		Pin:    "TestNotificationTemplates_ListedOnlyForReadableProjects",
	},
	{OperationID: "project.messagingpolicy.update", Method: "PUT", Pattern: "/api/v1/projects/{id}/messaging-policy"}: {
		Reason: "the owner rule admits only an active direct project owner, and the matrix's super-admin is not one on the fixture project, so the admitting token is refused by the owner rule after the selector check",
		Pin:    "TestProjectMessagingPolicyPut_RequiresSetMessagingPolicySelector",
	},
	{OperationID: "schedule.event.read", Method: "GET", Pattern: "/api/v1/projects/{projectId}/scheduled-events/{id}/output"}: {
		Reason: "the default seeded scheduledEvent is a message event (404), and reading exec output requires both scheduled_event:read and live agent:attach on the target agent; the pin drives the full principal and UAT-scope matrix on a real capture run",
		Pin:    "TestScheduledResponses_RouteEnumerationGuard",
	},
	{OperationID: "project.systemoneinstances.read", Method: "GET", Pattern: "/api/v1/projects/{id}/system-one/instances"}: {
		Reason: "the route answers 404 while the hub.system_one experiment is off, as in this fixture; the dedicated test enables the experiment and drives the full credential and role matrix",
		Pin:    "TestProjectSystemOneInstances_AuthzMatrixAndExperimentGate",
	},
	{OperationID: "project.systemoneinstances.manage", Method: "PUT", Pattern: "/api/v1/projects/{id}/system-one/instances"}: {
		Reason: "404 while hub.system_one is off in this fixture, and management also requires active project owner/admin or unscoped local hub admin on the underlying user; the dedicated test enables the experiment and drives the full credential and role matrix",
		Pin:    "TestProjectSystemOneInstances_AuthzMatrixAndExperimentGate",
	},
	{OperationID: "project.systemoneinstances.manage", Method: "PUT", Pattern: "/api/v1/projects/{id}/system-one/instances/{name}"}: {
		Reason: "404 while hub.system_one is off in this fixture, and management also requires active project owner/admin or unscoped local hub admin on the underlying user; the dedicated test enables the experiment and drives the full credential and role matrix",
		Pin:    "TestProjectSystemOneInstances_AuthzMatrixAndExperimentGate",
	},
	{OperationID: "project.systemoneinstances.manage", Method: "DELETE", Pattern: "/api/v1/projects/{id}/system-one/instances/{name}"}: {
		Reason: "404 while hub.system_one is off in this fixture, and management also requires active project owner/admin or unscoped local hub admin on the underlying user; the dedicated test enables the experiment and drives the full credential and role matrix",
		Pin:    "TestProjectSystemOneInstances_AuthzMatrixAndExperimentGate",
	},
	{OperationID: "project.systemoneinstances.available", Method: "GET", Pattern: "/api/v1/projects/{id}/system-one/available-instances"}: {
		Reason: "404 while hub.system_one or hub.message_policies is off in this fixture, and discovery reuses authorizeProjectSystemOneManage (active project owner/admin or unscoped local hub admin); the dedicated test enables both experiments and drives the full credential and role matrix",
		Pin:    "TestProjectSystemOneAvailableInstances_AuthzAndNoSecretLeakage",
	},
	{OperationID: "project.messagepolicy.read", Method: "GET", Pattern: "/api/v1/projects/{id}/message-policies"}: {
		Reason: "the route answers 404 while the hub.message_policies experiment is off, as in this fixture; the dedicated test enables the experiment and drives the full §4 credential matrix across session, dev, project/hub/other-project UATs, member, viewer, agent, delegated/broker/hub-delivery/federation/visitor, and hub admin",
		Pin:    "TestMessagePolicyAuthz_Target7_CredentialMatrix",
	},
	{OperationID: "project.messagepolicy.read", Method: "GET", Pattern: "/api/v1/projects/{id}/message-policies/{policyId}"}: {
		Reason: "same as GET /api/v1/projects/{id}/message-policies above: 404 while hub.message_policies is off in this fixture; the dedicated test enables the experiment and drives the full §4 credential matrix",
		Pin:    "TestMessagePolicyAuthz_Target7_CredentialMatrix",
	},
	{OperationID: "project.messagepolicy.create", Method: "POST", Pattern: "/api/v1/projects/{id}/message-policies"}: {
		Reason: "404 while hub.message_policies is off in this fixture, and management also requires active project owner/admin or unscoped local hub admin on the underlying user; the dedicated test enables the experiment and drives the full §4 credential matrix",
		Pin:    "TestMessagePolicyAuthz_Target7_CredentialMatrix",
	},
	{OperationID: "project.messagepolicy.update", Method: "PATCH", Pattern: "/api/v1/projects/{id}/message-policies/{policyId}"}: {
		Reason: "404 while hub.message_policies is off in this fixture, and management also requires active project owner/admin or unscoped local hub admin on the underlying user; the dedicated test enables the experiment and drives the full §4 credential matrix",
		Pin:    "TestMessagePolicyAuthz_Target7_CredentialMatrix",
	},
	{OperationID: "project.messagepolicy.delete", Method: "DELETE", Pattern: "/api/v1/projects/{id}/message-policies/{policyId}"}: {
		Reason: "404 while hub.message_policies is off in this fixture, and management also requires active project owner/admin or unscoped local hub admin on the underlying user; the dedicated test enables the experiment and drives the full §4 credential matrix",
		Pin:    "TestMessagePolicyAuthz_Target7_CredentialMatrix",
	},
	{OperationID: "project.workspace.read", Method: "GET", Pattern: "/api/v1/projects/{id}/workspace/uploads/{sid}"}: {
		Reason: "the route answers 404 while the hub.large_uploads experiment is off, as in this fixture; the dedicated test enables the experiment and verifies owner/scope binding and agent refusal",
		Pin:    "TestWorkspaceUploads_OwnerScopeBindingAndAgentRefused",
	},
	{OperationID: "project.workspace.write", Method: "POST", Pattern: "/api/v1/projects/{id}/workspace/uploads"}: {
		Reason: "the route answers 404 while the hub.large_uploads experiment is off, as in this fixture; the dedicated test enables the experiment and verifies owner/scope binding and agent refusal",
		Pin:    "TestWorkspaceUploads_OwnerScopeBindingAndAgentRefused",
	},
	{OperationID: "project.workspace.write", Method: "PUT", Pattern: "/api/v1/projects/{id}/workspace/uploads/{sid}"}: {
		Reason: "the route answers 404 while the hub.large_uploads experiment is off, as in this fixture; the dedicated test enables the experiment and verifies owner/scope binding and agent refusal",
		Pin:    "TestWorkspaceUploads_OwnerScopeBindingAndAgentRefused",
	},
	{OperationID: "project.workspace.write", Method: "POST", Pattern: "/api/v1/projects/{id}/workspace/uploads/{sid}/complete"}: {
		Reason: "the route answers 404 while the hub.large_uploads experiment is off, as in this fixture; the dedicated test enables the experiment and verifies owner/scope binding and agent refusal",
		Pin:    "TestWorkspaceUploads_OwnerScopeBindingAndAgentRefused",
	},
	{OperationID: "project.workspace.write", Method: "DELETE", Pattern: "/api/v1/projects/{id}/workspace/uploads/{sid}"}: {
		Reason: "the route answers 404 while the hub.large_uploads experiment is off, as in this fixture; the dedicated test enables the experiment and verifies owner/scope binding and agent refusal",
		Pin:    "TestWorkspaceUploads_OwnerScopeBindingAndAgentRefused",
	},
	{OperationID: "project.shareddir.read", Method: "GET", Pattern: "/api/v1/projects/{id}/shared-dirs/{name}/uploads/{sid}"}: {
		Reason: "the route answers 404 while the hub.large_uploads experiment is off, as in this fixture; the dedicated test enables the experiment and verifies owner/scope binding and agent refusal",
		Pin:    "TestSharedDirUploads_OwnerScopeBindingAndAgentRefused",
	},
	{OperationID: "project.shareddir.write", Method: "POST", Pattern: "/api/v1/projects/{id}/shared-dirs/{name}/uploads"}: {
		Reason: "the route answers 404 while the hub.large_uploads experiment is off, as in this fixture; the dedicated test enables the experiment and verifies owner/scope binding and agent refusal",
		Pin:    "TestSharedDirUploads_OwnerScopeBindingAndAgentRefused",
	},
	{OperationID: "project.shareddir.write", Method: "PUT", Pattern: "/api/v1/projects/{id}/shared-dirs/{name}/uploads/{sid}"}: {
		Reason: "the route answers 404 while the hub.large_uploads experiment is off, as in this fixture; the dedicated test enables the experiment and verifies owner/scope binding and agent refusal",
		Pin:    "TestSharedDirUploads_OwnerScopeBindingAndAgentRefused",
	},
	{OperationID: "project.shareddir.write", Method: "POST", Pattern: "/api/v1/projects/{id}/shared-dirs/{name}/uploads/{sid}/complete"}: {
		Reason: "the route answers 404 while the hub.large_uploads experiment is off, as in this fixture; the dedicated test enables the experiment and verifies owner/scope binding and agent refusal",
		Pin:    "TestSharedDirUploads_OwnerScopeBindingAndAgentRefused",
	},
	{OperationID: "project.shareddir.write", Method: "DELETE", Pattern: "/api/v1/projects/{id}/shared-dirs/{name}/uploads/{sid}"}: {
		Reason: "the route answers 404 while the hub.large_uploads experiment is off, as in this fixture; the dedicated test enables the experiment and verifies owner/scope binding and agent refusal",
		Pin:    "TestSharedDirUploads_OwnerScopeBindingAndAgentRefused",
	},
	{OperationID: "project.messagepolicy.test", Method: "POST", Pattern: "/api/v1/projects/{id}/message-policies/test"}: {
		Reason: "404 while hub.message_policies is off in this fixture, and management also requires active project owner/admin or unscoped local hub admin on the underlying user; the dedicated test enables the experiment and drives the full §4 credential matrix",
		Pin:    "TestMessagePolicyAuthz_Target7_CredentialMatrix",
	},
	{OperationID: "project.messagepolicy.test", Method: "POST", Pattern: "/api/v1/projects/{id}/message-policies/{policyId}/test"}: {
		Reason: "404 while hub.message_policies is off in this fixture, and management also requires active project owner/admin or unscoped local hub admin on the underlying user; the dedicated test enables the experiment and drives the full §4 credential matrix",
		Pin:    "TestMessagePolicyAuthz_Target7_CredentialMatrix",
	},
	{OperationID: "project.messagepolicy.decisions", Method: "GET", Pattern: "/api/v1/projects/{id}/message-policies/decisions"}: {
		Reason: "404 while hub.message_policies is off in this fixture, and management also requires active project owner/admin or unscoped local hub admin on the underlying user; the dedicated test enables the experiment and drives the full §4 credential matrix",
		Pin:    "TestMessagePolicyAuthz_Target7_CredentialMatrix",
	},
	{OperationID: "project.messagepolicy.decisions", Method: "GET", Pattern: "/api/v1/projects/{id}/message-policies/{policyId}/decisions"}: {
		Reason: "404 while hub.message_policies is off in this fixture, and management also requires active project owner/admin or unscoped local hub admin on the underlying user; the dedicated test enables the experiment and drives the full §4 credential matrix",
		Pin:    "TestMessagePolicyAuthz_Target7_CredentialMatrix",
	},
	{OperationID: "project.template.set", Method: "POST", Pattern: "/api/v1/projects/{id}/set-template"}: {
		Reason: "the operation needs project:update and project:clone, and a token passes project.clone on a project only when its holder is a member of it; the matrix's super-admin token carries one selector and is not a member of the fixture project",
		Pin:    "TestSetTemplate_RequiresUpdateAndCloneOnTheProject",
	},
	// User-scope branches of shared routes: the branch is chosen by the
	// record's scope or by the request's scope and kind, while the matrix
	// addresses one fixture per pattern and sends no scope. Each is pinned
	// by a test that drives real tokens through the user-scope branch.
	{OperationID: "template.user.delete", Method: "DELETE", Pattern: "/api/v1/templates/{id}"}: {
		Reason: "the user-scope branch needs a user-scope template owned by the token holder, and the matrix fixture on this pattern is a global template",
		Pin:    "TestUserTemplateWrite_RequiresTemplateSelector",
	},
	{OperationID: "template.user.clone", Method: "POST", Pattern: "/api/v1/templates/{id}/clone"}: {
		Reason: "the user-scope branch is selected by scope=user in the request body; the source also needs template:read",
		Pin:    "TestCloneToUserScope_RequiresCreateOnDestination",
	},
	{OperationID: "template.user.reimport", Method: "POST", Pattern: "/api/v1/templates/{id}/reimport"}: {
		Reason: "the user-scope branch needs a user-scope template owned by the token holder and a source URL, and the read gate also needs template:read",
		Pin:    "TestTemplateUserReimport_RequiresUpdateAction",
	},
	{OperationID: "skill.user.create", Method: "POST", Pattern: "/api/v1/skills"}: {
		Reason: "the user-scope branch is selected by scope=user in the request body",
		Pin:    "TestUserScopeSkillCreate_RequiresSkillCreate",
	},
	{OperationID: "harnessconfig.user.clone", Method: "POST", Pattern: "/api/v1/harness-configs/{id}/clone"}: {
		Reason: "the user-scope branch is selected by scope=user in the request body; the route gate also needs harness_config:read on the source",
		Pin:    "TestCloneToUserScope_RequiresCreateOnDestination",
	},
	{OperationID: "harnessconfig.user.delete", Method: "DELETE", Pattern: "/api/v1/harness-configs/{id}"}: {
		Reason: "the user-scope branch needs a user-scope harness config owned by the token holder, and the route gate also needs harness_config:read",
		Pin:    "TestHarnessUserDelete_RequiresDeleteAction",
	},
	{OperationID: "harnessconfig.user.reimport", Method: "POST", Pattern: "/api/v1/harness-configs/{id}/reimport"}: {
		Reason: "the user-scope branch needs a user-scope harness config owned by the token holder and a source URL, and the route gate also needs harness_config:read",
		Pin:    "TestHarnessUserReimport_RequiresUpdateAction",
	},
	{OperationID: "template.user.import", Method: "POST", Pattern: "/api/v1/resources/import"}: {
		Reason: "the branch is selected by scope=user and kind=template in the request body, and the test server configures no resource storage",
		Pin:    "TestUserScopeImport_RequiresCreateSelector",
	},
	{OperationID: "template.user.import", Method: "POST", Pattern: "/api/v1/resources/discover"}: {
		Reason: "the branch is selected by scope=user and kind=template in the request body, and the test server configures no resource storage",
		Pin:    "TestUserScopeImport_RequiresCreateSelector",
	},
	{OperationID: "harnessconfig.user.import", Method: "POST", Pattern: "/api/v1/resources/import"}: {
		Reason: "the branch is selected by scope=user and kind=harness-config in the request body, and the test server configures no resource storage",
		Pin:    "TestUserScopeImport_RequiresCreateSelector",
	},
	{OperationID: "harnessconfig.user.import", Method: "POST", Pattern: "/api/v1/resources/discover"}: {
		Reason: "the branch is selected by scope=user and kind=harness-config in the request body, and the test server configures no resource storage",
		Pin:    "TestUserScopeImport_RequiresCreateSelector",
	},
	{OperationID: "chat.space.list", Method: "GET", Pattern: "/api/v1/chat/spaces"}: {
		Reason: "the space list filters projects by project:read instead of refusing the request, so a token without the selector gets 200 with no spaces",
		Pin:    "TestChatSpacesToken_FilteredToBoundary",
	},
	{OperationID: "chat.unreadcount.read", Method: "GET", Pattern: "/api/v1/chat/unread-count"}: {
		Reason: "the count covers only the conversations inside the token boundary instead of refusing the request, so a project token for another project gets 200",
		Pin:    "TestChatUnreadCountToken_FilteredToBoundary",
	},
	{OperationID: "chat.dm.list", Method: "GET", Pattern: "/api/v1/chat/dms"}: {
		Reason: "the DM list keeps only the DMs the token may read instead of refusing the request, so a project token for another project gets 200 with no rows",
		Pin:    "TestChatDMListToken_FilteredToBoundary",
	},
	{OperationID: "chat.search.unscoped", Method: "GET", Pattern: "/api/v1/chat/search"}: {
		Reason: "the unscoped search covers only the projects the token can read instead of refusing the request, so a token without project:read gets 200 with no rows",
		Pin:    "TestChatSearchToken_DMRowsNeedInboxRead",
	},
	{OperationID: "chat.presence.update", Method: "POST", Pattern: "/api/v1/chat/presence"}: {
		Reason: "presence leaves the named projects the caller cannot read out of its fan-out instead of refusing the request, so a project token for another project gets 200 and fans out on nothing",
		Pin:    "TestChatPresence_EveryProjectRequiresProjectRead",
	},
	{OperationID: "chat.conversation.send.dmuser", Method: "POST", Pattern: "/api/v1/chat/conversations/{id}/messages"}: {
		Reason: "a send to a user needs read access to the peer user record for every caller, and that record has no hub target for a token, so the admitting token is refused by the peer read check",
		Pin:    "TestChatHumanDirectMessage_RequiresHubBoundary",
	},
	{OperationID: "chat.attachment.upload.project", Method: "POST", Pattern: "/api/v1/chat/attachments"}: {
		Reason: "the upload takes a multipart form, which the matrix body cannot express, and the matrix server configures no attachment store",
		Pin:    "TestChatProjectUpload_RequiresInboxWriteAndProjectRead",
	},
	{OperationID: "chat.attachment.upload.noproject", Method: "POST", Pattern: "/api/v1/chat/attachments"}: {
		Reason: "the upload takes a multipart form, which the matrix body cannot express, and the matrix server configures no attachment store",
		Pin:    "TestChatProjectlessUpload_RequiresHubBoundary",
	},
	{OperationID: "chat.attachment.download.project", Method: "GET", Pattern: "/api/v1/chat/attachments/{id}"}: {
		Reason: "the matrix server configures no attachment store, so every caller gets 503 before the attachment is looked up",
		Pin:    "TestChatAttachmentDownloadToken_ProjectFileNeedsProjectRead",
	},
	{OperationID: "chat.decision.read", Method: "GET", Pattern: "/api/v1/decisions/{rowId}"}: {
		Reason: "the route answers 404 while the web.decisions experiment is off, as in this fixture, so the session-only probe gets 404 before the credential check. The dedicated test enables the experiment and drives the route with a user access token and a visitor",
		Pin:    "TestDecisionRoutes_TokenAndVisitorForbidden",
	},
	{OperationID: "chat.decisionprompt.answer", Method: "POST", Pattern: "/api/v1/decision-prompts/{messageId}/answers"}: {
		Reason: "the route answers 404 while the web.decisions experiment is off, as in this fixture, so the session-only probe gets 404 before the credential check. The dedicated test enables the experiment and drives the route with a user access token, a federated user and a visitor",
		Pin:    "TestDecisionSetAnswer_Authorization",
	},
	{OperationID: "chat.decisionprompt.dismiss", Method: "POST", Pattern: "/api/v1/decision-prompts/{messageId}/dismiss"}: {
		Reason: "the route answers 404 while the web.decisions experiment is off, as in this fixture; the dedicated test enables the experiment and drives the route across session, project and hub UATs (with and without inbox:write and across project boundaries), agent tokens (403 users_only), federated users and visitors",
		Pin:    "TestDecisionPromptDismiss_Authorization",
	},
	{OperationID: "chat.secretrequest.read", Method: "GET", Pattern: "/api/v1/secret-requests/{messageId}"}: {
		Reason: "the route answers 404 while the hub.agent_secrets experiment is off, as in this fixture, so the session-only probe gets 404 before the credential check. The dedicated test enables the experiment and drives the route with a user access token, agent token, non-recipient, and recipient without agent.attach",
		Pin:    "TestSecretRequests_EndToEnd",
	},
	{OperationID: "chat.secretrequest.fulfill", Method: "POST", Pattern: "/api/v1/secret-requests/{messageId}/fulfill"}: {
		Reason: "the route answers 404 while the hub.agent_secrets experiment is off, as in this fixture, so the session-only probe gets 404 before the credential check. The dedicated test enables the experiment and drives the route with a user access token, agent token, non-recipient, and recipient without agent.attach",
		Pin:    "TestSecretRequests_EndToEnd",
	},
	{OperationID: "chat.secretrequest.dismiss", Method: "POST", Pattern: "/api/v1/secret-requests/{messageId}/dismiss"}: {
		Reason: "the route answers 404 while the hub.agent_secrets experiment is off, as in this fixture, so the session-only probe gets 404 before the credential check. The dedicated test enables the experiment and drives the route with a user access token, agent token, non-recipient, and recipient without agent.attach",
		Pin:    "TestSecretRequests_EndToEnd",
	},
	{OperationID: "chat.usersecretrequests.list", Method: "GET", Pattern: "/api/v1/users/me/secret-requests"}: {
		Reason: "the route answers 404 while the hub.agent_secrets experiment is off, as in this fixture, so the session-only probe gets 404 before the credential check. TestUserMeSecretRequests_AuthorizationAndScope enables the experiment and drives GET with a user access token, agent token, visitor, and session users",
		Pin:    "TestUserMeSecretRequests_AuthorizationAndScope",
	},
	{OperationID: "chat.usercommandrequests.list", Method: "GET", Pattern: "/api/v1/users/me/command-requests"}: {
		Reason: "the route answers 404 while the hub.command_requests experiment is off, as in this fixture, so the session-only probe gets 404 before the credential check. TestUserMeCommandRequests_AuthorizationAndScope enables the experiment and drives GET with a user access token, agent token, visitor, federated identity, and session users",
		Pin:    "TestUserMeCommandRequests_AuthorizationAndScope",
	},
	{OperationID: "chat.commandrequest.read", Method: "GET", Pattern: "/api/v1/command-requests/{id}"}: {
		Reason: "the route answers 404 while the hub.command_requests experiment is off, as in this fixture, so the session-only probe gets 404 before the credential check. TestCommandRequests_EndToEnd enables the experiment and drives GET with a real CLI session token, real PAT through Handler(), real agent JWT through Handler(), visitor identity, federated identity, and non-recipient",
		Pin:    "TestCommandRequests_EndToEnd",
	},
	{OperationID: "chat.commandrequest.approve", Method: "POST", Pattern: "/api/v1/command-requests/{id}/approve"}: {
		Reason: "the route answers 404 while the hub.command_requests experiment is off, as in this fixture, so the session-only probe gets 404 before the credential check. TestCommandRequests_EndToEnd enables the experiment and drives POST with a real CLI session token, real PAT through Handler(), real agent JWT through Handler(), visitor identity, federated identity, and non-approver",
		Pin:    "TestCommandRequests_EndToEnd",
	},
	{OperationID: "chat.commandrequest.output", Method: "GET", Pattern: "/api/v1/command-requests/{id}/output"}: {
		Reason: "the route answers 404 while the hub.command_requests experiment is off, as in this fixture, so the session-only probe gets 404 before the credential check. TestCommandRequestOutput_AuthzAndSharing enables the experiment and drives GET with a real CLI session token, real PAT through Handler(), real agent JWT through Handler(), visitor identity, federated identity, non-approver, revoked attach, and deleted container",
		Pin:    "TestCommandRequestOutput_AuthzAndSharing",
	},
	{OperationID: "chat.commandrequest.frommessage", Method: "POST", Pattern: "/api/v1/command-requests/from-message"}: {
		Reason: "the route answers 404 while the hub.command_requests experiment is off, as in this fixture, so the session-only probe gets 404 before the credential check. TestCommandRequests_FromMessage_EndToEnd enables the experiment and drives POST with a real session, real PAT/agent JWT/visitor/federated refusals, non-recipient, and non-addressed group participant",
		Pin:    "TestCommandRequests_FromMessage_EndToEnd",
	},
	{OperationID: "chat.commandrequest.dismiss", Method: "POST", Pattern: "/api/v1/command-requests/{id}/dismiss"}: {
		Reason: "the route answers 404 while the hub.command_requests experiment is off, as in this fixture, so the session-only probe gets 404 before the credential check. TestCommandRequests_DismissAndWithdraw enables the experiment and drives POST with a real session, real PAT through Handler(), real agent JWT through Handler(), visitor identity, and non-approver",
		Pin:    "TestCommandRequests_DismissAndWithdraw",
	},
	{OperationID: "chat.decision.answer", Method: "POST", Pattern: "/api/v1/decisions/{rowId}/answer"}: {
		Reason: "the route answers 404 while the web.decisions experiment is off, as in this fixture, so the session-only probe gets 404 before the credential check. The dedicated test enables the experiment and drives the route with a user access token and a visitor",
		Pin:    "TestDecisionRoutes_TokenAndVisitorForbidden",
	},
	{OperationID: "chat.projectdecisions.list", Method: "GET", Pattern: "/api/v1/projects/{id}/decisions"}: {
		Reason: "the route answers 404 while the web.decisions experiment is off, as in this fixture; the dedicated test enables the experiment and drives the route across session, project and hub UATs (with and without inbox:read and project:read), wrong-project UATs, agent tokens, federated users and visitors",
		Pin:    "TestProjectDecisions_TokenAndCallerAuthz",
	},
	{OperationID: "chat.projectdecisions.suggestions", Method: "GET", Pattern: "/api/v1/projects/{id}/decisions/suggestions"}: {
		Reason: "the route answers 404 while web.decisions is off, as in this fixture; the dedicated test enables web.decisions and drives the route across session, UATs, agent tokens, federated users and visitors",
		Pin:    "TestProjectDecisionSuggestions_AuthzScopeAndExperiments",
	},
	{OperationID: "chat.userdecisions.list", Method: "GET", Pattern: "/api/v1/users/me/decisions"}: {
		Reason: "the route answers 404 while the web.decisions experiment is off, and when on it filters candidate member projects by project:read rather than refusing a UAT without project:read; the dedicated test enables the experiment and drives the route across session, project-bounded and hub-bounded UATs (with and without inbox:read and project:read), agent tokens, federated users and visitors",
		Pin:    "TestUserMeDecisions_TokenAndCallerAuthz",
	},
	{OperationID: "chat.userdecisions.suggestions", Method: "GET", Pattern: "/api/v1/users/me/decisions/suggestions"}: {
		Reason: "the route answers 404 while web.decisions is off, as in this fixture; the dedicated test enables web.decisions and drives the route across session, UATs, agent tokens, federated users and visitors",
		Pin:    "TestUserMeDecisionSuggestions_AuthzScopeAndValidation",
	},
	{OperationID: "chat.conversation.decisions.topic", Method: "GET", Pattern: "/api/v1/chat/conversations/{id}/decisions"}: {
		Reason: "the route answers 404 while the web.decisions experiment is off, as in this fixture; the dedicated test enables the experiment and drives the route across session users, non-readers, UATs with and without inbox:read, wrong-project UATs, agents and visitors",
		Pin:    "TestConversationDecisions_Authz",
	},
	{OperationID: "chat.conversation.decisions.dm", Method: "GET", Pattern: "/api/v1/chat/conversations/{id}/decisions"}: {
		Reason: "the route answers 404 while the web.decisions experiment is off, as in this fixture; the dedicated test enables the experiment and drives the route across DM participants, non-participants, admins, UATs with and without inbox:read, user-to-user DMs, agents and visitors",
		Pin:    "TestConversationDecisions_Authz",
	},
	{OperationID: "inbox.summary.read", Method: "GET", Pattern: "/api/v1/users/me/inbox/summary"}: {
		Reason: "the route answers 404 while the web.inbox_dashboard experiment is off, as in this fixture; the dedicated test enables the experiment and drives the route across session, hub-bounded and project-bounded UATs (with and without inbox:read), agent tokens, federated users and visitors",
		Pin:    "TestUserMeInboxSummary_AuthAndValidation",
	},
	{OperationID: "agent.screen.read", Method: "GET", Pattern: "/api/v1/agents/{id}/screen"}: {
		Reason: "the route answers 404 while the hub.suspend_screen experiment is off, as in this fixture, so the admitting token gets 404 before authz; the dedicated test enables the experiment and drives RBAC on GET",
		Pin:    "TestSuspendScreen_ManualSuspendAndRBACAndLook",
	},
	{OperationID: "agent.screen.read", Method: "GET", Pattern: "/api/v1/projects/{projectId}/agents/{id}/screen"}: {
		Reason: "the route answers 404 while the hub.suspend_screen experiment is off, as in this fixture, so the admitting token gets 404 before authz; the dedicated test enables the experiment and drives RBAC on GET",
		Pin:    "TestSuspendScreen_ManualSuspendAndRBACAndLook",
	},
	// Global catalog and project-scope branches whose refusal the generic
	// probes cannot observe. Each is pinned by a test that drives real
	// tokens through the branch.
	{OperationID: "template.clone", Method: "POST", Pattern: "/api/v1/templates/{id}/clone"}: {
		Reason: "the source needs template:read before the destination's template:create is checked, and a token without the read selector is answered 404 on the source, so a POST refusal is not 403",
		Pin:    "TestCatalogCloneToken_RequiresReadAndCreate",
	},
	{OperationID: "template.global.clone", Method: "POST", Pattern: "/api/v1/templates/{id}/clone"}: {
		Reason: "the source needs template:read before the destination's template:create_global is checked, and a token without the read selector is answered 404 on the source, so a POST refusal is not 403",
		Pin:    "TestCatalogCloneToken_RequiresReadAndCreate",
	},
	{OperationID: "harnessconfig.clone", Method: "POST", Pattern: "/api/v1/harness-configs/{id}/clone"}: {
		Reason: "the route gate needs harness_config:read on the source before the destination's harness_config:create is checked, and a token without the read selector is answered 404, so a POST refusal is not 403",
		Pin:    "TestCatalogCloneToken_RequiresReadAndCreate",
	},
	{OperationID: "template.reimport", Method: "POST", Pattern: "/api/v1/templates/{id}/reimport"}: {
		Reason: "the record needs template:read before template:create on the owning scope is checked, and a token without the read selector is answered 404, so a POST refusal is not 403",
		Pin:    "TestCatalogReimportToken_RequiresReadAndCreate",
	},
	{OperationID: "template.global.reimport", Method: "POST", Pattern: "/api/v1/templates/{id}/reimport"}: {
		Reason: "the record needs template:read before template:create_global on the global scope is checked, and a token without the read selector is answered 404, so a POST refusal is not 403",
		Pin:    "TestCatalogReimportToken_RequiresReadAndCreate",
	},
	{OperationID: "harnessconfig.reimport", Method: "POST", Pattern: "/api/v1/harness-configs/{id}/reimport"}: {
		Reason: "the route gate needs harness_config:read before harness_config:create on the owning scope is checked, and a token without the read selector is answered 404, so a POST refusal is not 403",
		Pin:    "TestCatalogReimportToken_RequiresReadAndCreate",
	},
	{OperationID: "harnessconfig.delete", Method: "DELETE", Pattern: "/api/v1/harness-configs/{id}"}: {
		Reason: "the route gate needs harness_config:read before harness_config:delete on the owning scope is checked, and a token without the read selector is answered 404, so a DELETE refusal is not 403",
		Pin:    "TestHarnessDeleteToken_RequiresReadAndDelete",
	},
	{OperationID: "harnessconfig.read", Method: "GET", Pattern: "/api/v1/harness-configs/{id}/image-status"}: {
		Reason: "an admitted read of a config with an image queries the image registry, and one without an image answers 400, so the admitting token cannot show the target exists with a 2xx",
		Pin:    "TestHarnessImageStatusToken_RequiresRead",
	},
	{OperationID: "skill.resolve.local", Method: "POST", Pattern: "/api/v1/skills/resolve"}: {
		Reason: "a reference the caller may not read answers as not found inside a 200 response, so a refused token is not answered 403",
		Pin:    "TestLocalSkillResolve_TokenNeedsSkillRead",
	},
	{OperationID: "skill.resolve.github.project", Method: "POST", Pattern: "/api/v1/skills/resolve"}: {
		Reason: "a refused reference answers forbidden inside a 200 response, so a refused token is not answered 403",
		Pin:    "TestProjectGitHubSkillResolve_TokenNeedsSkillReadOnProject",
	},
	{OperationID: "skill.resolve.github.public", Method: "POST", Pattern: "/api/v1/skills/resolve"}: {
		Reason: "a refused reference answers forbidden inside a 200 response, so a refused token is not answered 403",
		Pin:    "TestPublicGitHubSkillResolve_TokenNeedsSkillReadOnHub",
	},
	{OperationID: "template.import.global", Method: "POST", Pattern: "/api/v1/resources/import"}: {
		Reason: "an admitted import fetches its source after the check; the pin stops the fetch at a source that is not a GitHub URL",
		Pin:    "TestGlobalResourceImport_ResolvesHubThroughEvidence",
	},
	{OperationID: "template.import.global", Method: "POST", Pattern: "/api/v1/resources/discover"}: {
		Reason: "an admitted discovery fetches its source after the check; the pin stops the fetch at a source that is not a GitHub URL",
		Pin:    "TestGlobalResourceImport_ResolvesHubThroughEvidence",
	},
	{OperationID: "harnessconfig.import.global", Method: "POST", Pattern: "/api/v1/resources/import"}: {
		Reason: "an admitted import fetches its source after the check; the pin stops the fetch at a source that is not a GitHub URL",
		Pin:    "TestGlobalResourceImport_ResolvesHubThroughEvidence",
	},
	{OperationID: "harnessconfig.import.global", Method: "POST", Pattern: "/api/v1/resources/discover"}: {
		Reason: "an admitted discovery fetches its source after the check; the pin stops the fetch at a source that is not a GitHub URL",
		Pin:    "TestGlobalResourceImport_ResolvesHubThroughEvidence",
	},
	{OperationID: "template.import.project", Method: "POST", Pattern: "/api/v1/resources/import"}: {
		Reason: "an admitted import fetches its source after the check; the pin stops the fetch at a source that is not a GitHub URL",
		Pin:    "TestProjectResourceImport_TokenNeedsCreateOnTheProject",
	},
	{OperationID: "template.import.project", Method: "POST", Pattern: "/api/v1/resources/discover"}: {
		Reason: "an admitted discovery fetches its source after the check; the pin stops the fetch at a source that is not a GitHub URL",
		Pin:    "TestProjectResourceImport_TokenNeedsCreateOnTheProject",
	},
	{OperationID: "harnessconfig.import.project", Method: "POST", Pattern: "/api/v1/resources/import"}: {
		Reason: "an admitted import fetches its source after the check; the pin stops the fetch at a source that is not a GitHub URL",
		Pin:    "TestProjectResourceImport_TokenNeedsCreateOnTheProject",
	},
	{OperationID: "harnessconfig.import.project", Method: "POST", Pattern: "/api/v1/resources/discover"}: {
		Reason: "an admitted discovery fetches its source after the check; the pin stops the fetch at a source that is not a GitHub URL",
		Pin:    "TestProjectResourceImport_TokenNeedsCreateOnTheProject",
	},
	{OperationID: "artifact.update", Method: "PATCH", Pattern: "/api/v1/artifacts/{id}"}: {
		Reason: "the artifact service answers a caller that cannot read the artifact with 404, as for a missing artifact, on every method, so the generic refusal probes get 404 where the matrix expects 403 for a non-GET method; the admitting token carries only artifact:manage, not artifact:read, so the read check answers it 404 too; with artifact:read added, the super-admin is neither the fixture artifact's owner nor an admin grantee, so the manage check would answer 403",
		Pin:    "TestArtifactsManageAndVersionRoutesUserAccessTokensAreBounded",
	},
	{OperationID: "artifact.grant.list", Method: "GET", Pattern: "/api/v1/artifacts/{id}/grants"}: {
		Reason: "the admitting token carries only artifact:manage, not artifact:read, so the read check answers it 404; with artifact:read added, the super-admin is neither the fixture artifact's owner nor an admin grantee, so the manage check would answer 403",
		Pin:    "TestArtifactsManageAndVersionRoutesUserAccessTokensAreBounded",
	},
	{OperationID: "artifact.grant.put", Method: "POST", Pattern: "/api/v1/artifacts/{id}/grants"}: {
		Reason: "the artifact service answers a caller that cannot read the artifact with 404, as for a missing artifact, on every method, so the generic refusal probes get 404 where the matrix expects 403 for a non-GET method; the admitting token carries only artifact:manage, not artifact:read, so the read check answers it 404 too; with artifact:read added, the super-admin is neither the fixture artifact's owner nor an admin grantee, so the manage check would answer 403 (the review-grant path refuses it with the same 403)",
		Pin:    "TestArtifactsManageAndVersionRoutesUserAccessTokensAreBounded",
	},
	{OperationID: "artifact.grant.notify", Method: "POST", Pattern: "/api/v1/artifacts/{id}/grants"}: {
		Reason: "notify is a body field of the grant route, which admits tokens for the grant itself (artifact.grant.put); a token's notify is never refused with a status but answered notifySkipped not_permitted while the grant is still made, so the generic session-only probe (a 403 with session-only details) does not apply. The dedicated test drives the send gate with every credential kind and pins that only an interactive or dev session sends",
		Pin:    "TestGrantNotifyGate",
	},
	{OperationID: "artifact.grant.delete", Method: "DELETE", Pattern: "/api/v1/artifacts/{id}/grants/{grantId}"}: {
		Reason: "the artifact service answers a caller that cannot read the artifact with 404, as for a missing artifact, on every method, so the generic refusal probes get 404 where the matrix expects 403 for a non-GET method; the admitting token carries only artifact:manage, not artifact:read, so the read check answers it 404 too; with artifact:read added, the super-admin is neither the fixture artifact's owner nor an admin grantee, so the manage check would answer 403",
		Pin:    "TestArtifactsManageAndVersionRoutesUserAccessTokensAreBounded",
	},
	{OperationID: "artifact.link.list", Method: "GET", Pattern: "/api/v1/artifacts/{id}/links"}: {
		Reason: "the admitting token carries only artifact:manage, not artifact:read, so the read check answers it 404; with artifact:read added, the super-admin is neither the fixture artifact's owner nor an admin grantee, so the manage check would answer 403",
		Pin:    "TestArtifactsManageAndVersionRoutesUserAccessTokensAreBounded",
	},
	{OperationID: "artifact.link.create", Method: "POST", Pattern: "/api/v1/artifacts/{id}/links"}: {
		Reason: "the artifact service answers a caller that cannot read the artifact with 404, as for a missing artifact, on every method, so the generic refusal probes get 404 where the matrix expects 403 for a non-GET method; the admitting token carries only artifact:manage, not artifact:read, so the read check answers it 404 too; with artifact:read added, the super-admin is neither the fixture artifact's owner nor an admin grantee, so the manage check would answer 403",
		Pin:    "TestArtifactsManageAndVersionRoutesUserAccessTokensAreBounded",
	},
	{OperationID: "artifact.link.revoke", Method: "DELETE", Pattern: "/api/v1/artifacts/{id}/links/{linkId}"}: {
		Reason: "the artifact service answers a caller that cannot read the artifact with 404, as for a missing artifact, on every method, so the generic refusal probes get 404 where the matrix expects 403 for a non-GET method; the admitting token carries only artifact:manage, not artifact:read, so the read check answers it 404 too; with artifact:read added, the super-admin is neither the fixture artifact's owner nor an admin grantee, so the manage check would answer 403",
		Pin:    "TestArtifactsManageAndVersionRoutesUserAccessTokensAreBounded",
	},
	{OperationID: "artifact.delete", Method: "DELETE", Pattern: "/api/v1/artifacts/{id}"}: {
		Reason: "the artifact service answers a caller that cannot read the artifact with 404, as for a missing artifact, on every method, so the generic refusal probes get 404 where the matrix expects 403 for a non-GET method; the admitting token carries only artifact:manage, not artifact:read, so the read check answers it 404 and nothing is deleted; with artifact:read added, the super-admin is not the owner or an admin grantee, and the custodian clause also needs project.manage within the token's own limits, so it would answer 403",
		Pin:    "TestArtifactsManageAndVersionRoutesUserAccessTokensAreBounded",
	},
	{OperationID: "artifact.version.create", Method: "POST", Pattern: "/api/v1/artifacts/{id}/versions"}: {
		Reason: "the artifact service answers a caller that cannot read the artifact with 404, as for a missing artifact, on every method, so the generic refusal probes get 404 where the matrix expects 403 for a non-GET method; the admitting token carries only artifact:create, not artifact:read, so the read check answers it 404 too; with artifact:read added, the super-admin is neither the fixture artifact's owner nor a write, admin or review grantee, so the write check would answer 403",
		Pin:    "TestArtifactsManageAndVersionRoutesUserAccessTokensAreBounded",
	},
	{OperationID: "artifact.review.session.write", Method: "PUT", Pattern: "/api/v1/artifacts/{id}/review-sessions/{sid}"}: {
		Reason: "the artifact service answers a caller that cannot read the artifact with 404, as for a missing artifact, on every method, so the generic refusal probes get 404 where the matrix expects 403 for a non-GET method; the admitting token carries only artifact:create, not artifact:read, so the read check answers it 404 too; with artifact:read added, the super-admin is neither the fixture artifact's owner nor a write, admin or review grantee, so the write check would answer 403. TestArtifactsManageAndVersionRoutesUserAccessTokensAreBounded drives both methods with user access tokens (with the experiment on): without artifact:read or bounded to another project 404, read only 403, artifact:read and artifact:create reach the handler; TestArtifactsOpenReviewOnRoutes drives them with principals (404 for a user who cannot read the artifact, 403 for a reader without a review grant)",
		Pin:    "TestArtifactsManageAndVersionRoutesUserAccessTokensAreBounded",
	},
	{OperationID: "artifact.review.session.write", Method: "DELETE", Pattern: "/api/v1/artifacts/{id}/review-sessions/{sid}"}: {
		Reason: "same as the PUT entry above: the generic refusal probes get the artifact service's 404 for a caller that cannot read the artifact where the matrix expects 403, and the admitting super-admin is neither the owner nor a grantee; TestArtifactsManageAndVersionRoutesUserAccessTokensAreBounded drives DELETE with user access tokens and TestArtifactsOpenReviewOnRoutes with principals",
		Pin:    "TestArtifactsManageAndVersionRoutesUserAccessTokensAreBounded",
	},
	{OperationID: "artifact.review.draft.read", Method: "GET", Pattern: "/api/v1/artifacts/{id}/review-draft"}: {
		Reason: "the admitting token carries only artifact:create, not artifact:read, so readableArtifact answers it 404; with artifact:read added, the super-admin is neither the fixture artifact's owner nor a write, admin or review grantee, so canWriteErr would answer 403. TestArtifactsManageAndVersionRoutesUserAccessTokensAreBounded drives GET, PUT and DELETE with user access tokens (without artifact:read or bounded to another project 404, read only 403, artifact:read and artifact:create reach the handler) and TestArtifactsReviewDraftsAgentCreatorAndScopedTokenIsolation pins C1 scoped-token isolation",
		Pin:    "TestArtifactsManageAndVersionRoutesUserAccessTokensAreBounded",
	},
	{OperationID: "artifact.review.draft.write", Method: "PUT", Pattern: "/api/v1/artifacts/{id}/review-draft"}: {
		Reason: "the artifact service answers a caller that cannot read the artifact with 404, as for a missing artifact, on every method, so the generic refusal probes get 404 where the matrix expects 403 for a non-GET method; the admitting token carries only artifact:create, not artifact:read, so readableArtifact answers it 404 too; with artifact:read added, the super-admin is neither the fixture artifact's owner nor a write, admin or review grantee, so canWriteErr would answer 403. TestArtifactsManageAndVersionRoutesUserAccessTokensAreBounded and TestArtifactsReviewDraftsAgentCreatorAndScopedTokenIsolation drive PUT with user access tokens",
		Pin:    "TestArtifactsManageAndVersionRoutesUserAccessTokensAreBounded",
	},
	{OperationID: "artifact.review.draft.write", Method: "DELETE", Pattern: "/api/v1/artifacts/{id}/review-draft"}: {
		Reason: "same as the PUT /api/v1/artifacts/{id}/review-draft entry above: the generic refusal probes get 404 for a caller that cannot read the artifact where the matrix expects 403, and the admitting super-admin lacks artifact:read and holds no grant on the fixture artifact; TestArtifactsManageAndVersionRoutesUserAccessTokensAreBounded and TestArtifactsReviewDraftsAgentCreatorAndScopedTokenIsolation drive DELETE with user access tokens",
		Pin:    "TestArtifactsManageAndVersionRoutesUserAccessTokensAreBounded",
	},
	{OperationID: "artifact.review.request.list", Method: "GET", Pattern: "/api/v1/artifacts/{id}/review-requests"}: {
		Reason: "the route is gated by default-off hub.artifacts_review_requests (404 while off) and answers 404 for callers that cannot read the artifact; TestArtifactsReviewRequestsOnRoutes exercises publisher and self-only views with session and scoped UAT callers",
		Pin:    "TestArtifactsReviewRequestsOnRoutes",
	},
	{OperationID: "artifact.review.request.create", Method: "POST", Pattern: "/api/v1/artifacts/{id}/review-requests"}: {
		Reason: "the route is gated by default-off hub.artifacts_review_requests (404 while off), answers 404 for callers that cannot read the artifact, and refuses non-interactive user credentials at the route level with 403 interactive_session_required before charging the limiter while admitting owning agents on the bounded path; TestArtifactsReviewRequestsOnRoutes exercises interactive/dev session, scoped UAT (ceiling lacking artifact:request_review -> 403, boundary excluding project -> 404, admitting UAT -> 403 interactive_session_required) and owning/non-owning agent callers",
		Pin:    "TestArtifactsReviewRequestsOnRoutes",
	},
	{OperationID: "artifact.review.request.withdraw", Method: "POST", Pattern: "/api/v1/artifacts/{id}/review-requests/{rid}/withdraw"}: {
		Reason: "the route is gated by default-off hub.artifacts_review_requests (404 while off) and answers 404 for callers that cannot read the artifact or when the request id does not exist; TestArtifactsReviewRequestsOnRoutes exercises withdraw authorization and lifecycle",
		Pin:    "TestArtifactsReviewRequestsOnRoutes",
	},
	{OperationID: "artifact.review.request.dismiss", Method: "POST", Pattern: "/api/v1/artifacts/{id}/review-request/dismiss"}: {
		Reason: "the route is gated by default-off hub.artifacts_review_requests (404 while off) and answers uniform 404 when the caller's credential lacks artifact:read in the artifact's home scope or when no open review request exists; TestArtifactsReviewRequestsOnRoutes exercises session and scoped UAT (boundary excluding project -> 404 row unchanged, ceiling lacking artifact:read -> 404 row unchanged, admitting UAT -> 200) dismiss with the experiment on and off",
		Pin:    "TestArtifactsReviewRequestsOnRoutes",
	},
	{OperationID: "artifact.version.upload", Method: "PUT", Pattern: "/api/v1/artifacts/{id}/versions/{seq}/files/{path}"}: {
		Reason: "the artifact service answers a caller that cannot read the artifact with 404, as for a missing artifact, on every method, so the generic refusal probes get 404 where the matrix expects 403 for a non-GET method; the admitting token carries only artifact:create, not artifact:read, so the read check answers it 404 too; with artifact:read added, the super-admin is neither the fixture artifact's owner nor a write, admin or review grantee, so the write check would answer 403",
		Pin:    "TestArtifactsManageAndVersionRoutesUserAccessTokensAreBounded",
	},
	{OperationID: "artifact.version.finalize", Method: "POST", Pattern: "/api/v1/artifacts/{id}/versions/{seq}/finalize"}: {
		Reason: "the artifact service answers a caller that cannot read the artifact with 404, as for a missing artifact, on every method, so the generic refusal probes get 404 where the matrix expects 403 for a non-GET method; the admitting token carries only artifact:create, not artifact:read, so the read check answers it 404 too; with artifact:read added, the super-admin is neither the fixture artifact's owner nor a write, admin or review grantee, so the write check would answer 403",
		Pin:    "TestArtifactsManageAndVersionRoutesUserAccessTokensAreBounded",
	},
	{OperationID: "artifact.version.view", Method: "POST", Pattern: "/api/v1/artifacts/{id}/versions/{seq}/view"}: {
		Reason: "the artifact service answers a caller that cannot read the artifact with 404, as for a missing artifact, on every method, so the generic refusal probes get 404 where the matrix expects 403 for a non-GET method",
		Pin:    "TestArtifactsManageAndVersionRoutesUserAccessTokensAreBounded",
	},
	{OperationID: "broker.read", Method: "GET", Pattern: "/api/v1/runtime-brokers"}: {
		Reason: "the broker list filters each row by broker.read instead of refusing the request, so a token without the selector, or bounded to a project, gets 200 with no rows rather than 403",
		Pin:    "TestBrokerListing_TokenWithoutBrokerReadListsNoBrokers",
	},
	{OperationID: "group.list", Method: "GET", Pattern: "/api/v1/groups"}: {
		Reason: "the group list filters each row by the group.read check instead of refusing the request, so a token without the selector gets 200 with no rows rather than 403",
		Pin:    "TestExplicitGroupToken_ResolvesHubTarget",
	},
	{OperationID: "group.member.add", Method: "POST", Pattern: "/api/v1/groups/{id}/members"}: {
		Reason: "a token is held to the group role hierarchy: only the group's owner or an owner or admin member adds members, and the matrix's super-admin is none of these on the fixture group, so the admitting token is refused by the hierarchy check after the selector check",
		Pin:    "TestGroupMemberAddToken_HeldToGroupRoleHierarchy",
	},
	{OperationID: "artifact.list", Method: "GET", Pattern: "/api/v1/artifacts"}: {
		Reason: "the artifact list filters each row by the artifact.read check instead of refusing the request, so a token without the selector, or bounded to another project, gets 200 with no rows rather than 403 or 404 (no existence oracle)",
		Pin:    "TestArtifactsListUserAccessTokensAreBounded",
	},
	{OperationID: "agent.messages.read", Method: "GET", Pattern: "/api/v1/agents/{id}/messages/stream"}: {
		Reason: "the matrix server configures no event publisher, so every caller gets 501 before the agent is looked up",
		Pin:    "TestMessagesStreamToken_AdmissionByBoundary",
	},
	{OperationID: "project.conversation.gcsobject.read", Method: "GET", Pattern: "/api/v1/gcs/object"}: {
		Reason: "the matrix server enables no gs:// link experiment and configures no token generator, so every caller gets the same 404 before the message is looked up",
		Pin:    "TestGCSObjectToken_ProjectGroupNeedsProjectRead",
	},
	{OperationID: "chat.star.state", Method: "GET", Pattern: "/api/v1/chat/stars/state"}: {
		Reason: "the star state filters each message by the caller's current access to its thread instead of refusing the request, so a project token for another project gets 200 with no starred IDs rather than 403 or 404 (no existence oracle)",
		Pin:    "TestChatStars_TokenSelectors",
	},
	{OperationID: "chat.star.conversations", Method: "GET", Pattern: "/api/v1/chat/starred-conversations"}: {
		Reason: "the Starred rail filters each conversation by the caller's current access instead of refusing the request, so a project token for another project gets 200 with no rows rather than 403 or 404 (no existence oracle)",
		Pin:    "TestChatStarredConversations_TokenSelectors",
	},
	{OperationID: "chat.star.remove", Method: "DELETE", Pattern: "/api/v1/chat/stars/{messageId}"}: {
		Reason: "removing a star always answers an empty 204 (design: ownership-only, no target lookup); a project token removes a row only inside its boundary, and outside it the answer is the same empty 204 with nothing removed, so the boundary probe sees 204 rather than a refusal",
		Pin:    "TestChatStars_TokenSelectors",
	},
	{OperationID: "web.events.stream", Method: "GET", Pattern: "/events"}: {
		Reason: "the web server serves the event stream, not the hub API handler the matrix drives; the web server admits only a signed-in web session",
		Pin:    "TestWebEventStream_RefusesUserAccessToken",
	},
	{OperationID: "user.admin.provision", Method: "POST", Pattern: "/api/v1/users"}: {
		Reason: "the matrix server runs with dev authentication, and user provisioning is refused for every caller on such a hub (403 dev_auth_not_supported); the dedicated test drives the route on a hub without dev authentication with real tokens on both boundaries and with sessions",
		Pin:    "TestProvisionToken_RequiresInviteSelectorAndHubBoundary",
	},
}

// bearerMatrixKinds are the entry point kinds a request can reach.
var bearerMatrixKinds = map[authzop.EntryPointKind]bool{
	authzop.EntryPointHTTPRoute: true,
	authzop.EntryPointSSE:       true,
	authzop.EntryPointWebSocket: true,
}

// bearerMatrixEntries returns every catalog HTTP, SSE and WebSocket entry
// point, in catalog declaration order.
func bearerMatrixEntries() []bearerMatrixEntry {
	var out []bearerMatrixEntry
	for _, spec := range authzop.Catalog {
		for _, ep := range spec.EntryPoints {
			if bearerMatrixKinds[ep.Kind] {
				out = append(out, bearerMatrixEntry{Spec: spec, EntryPoint: ep})
			}
		}
	}
	return out
}

// bearerMatrixSelector returns the token selector of the operation's base
// permission, from the permission registry.
func bearerMatrixSelector(permissionID string) string {
	for _, p := range permissions.Registry {
		if p.ID == permissionID {
			return p.UATScope
		}
	}
	return ""
}

// bearerMatrixUnrelatedSelector returns a mintable selector that maps to a
// different permission than sel.
func bearerMatrixUnrelatedSelector(sel string) string {
	if sel == "project:read" {
		return "agent:read"
	}
	return "project:read"
}

// bearerMatrixGuardSelector returns the selector of the permission the
// hub-admin route guard checks on the entry point's route, or "" when the
// route has no such guard. A write behind a guard on a read permission
// needs both selectors, so the admitting token carries this one too.
func bearerMatrixGuardSelector(ep authzop.EntryPoint) string {
	best := ""
	var guard RouteMetadata
	for key, meta := range routeMetadataTable {
		_, path := splitRouteKey(key)
		if path == ep.Pattern || (strings.HasSuffix(path, "/") && strings.HasPrefix(ep.Pattern, path)) {
			if len(path) > len(best) {
				best, guard = path, meta
			}
		}
	}
	if best == "" || guard.Classification != RouteHubAdmin || guard.Permission == "" {
		return ""
	}
	return bearerMatrixSelector(guard.Permission)
}

// bearerMatrixCompanionSelectors lists, per operation, the selectors an
// admitting token carries besides the operation's own: the operation also
// checks project:read or agent:read on a target the record names, or
// broker:read on the broker whose settings it writes.
var bearerMatrixCompanionSelectors = map[authzop.OperationID][]string{
	// Binding a GitHub App installation is admin-only: besides project
	// update it requires the hub GitHub App update permission
	// (githubInstallationBindPermission), as its catalog entry states.
	"project.github.installation.bind":               {"hub_github_app:update"},
	"inbox.conversation.create":                      {"project:read"},
	"inbox.conversation.direct.read":                 {"agent:read"},
	"inbox.conversation.message.attachment.download": {"project:read"},
	"inbox.conversation.defaultagent.set":            {"project:read"},
	"inbox.conversation.participant.add":             {"project:read"},
	"inbox.conversation.resolve":                     {"project:read"},
	"inbox.notification.subscription.create":         {"project:read"},
	"inbox.notification.template.create":             {"project:read"},
	"inbox.threadgroup.create":                       {"project:read"},
	"inbox.threadgroup.update":                       {"project:read"},
	"inbox.threadgroup.delete":                       {"project:read"},
	"chat.prefs.read":                                {"agent:read"},
	"chat.prefs.update":                              {"agent:read"},
	"chat.thread.create":                             {"project:read"},
	"chat.space.markread":                            {"project:read"},
	"chat.topic.update":                              {"project:read"},
	"chat.topic.delete":                              {"project:read"},
	"chat.conversation.history.dm":                   {"agent:read"},
	"chat.conversation.send.topic":                   {"project:read"},
	"chat.conversation.send.dmagent":                 {"agent:message"},
	"chat.message.edit":                              {"project:read"},
	"chat.message.delete":                            {"project:read"},
	// Resuming or manually running a message schedule re-authorizes message
	// delivery to the target agent (ptone/scion#8588); without agent:message
	// (or agent:read), the target is masked as 404.
	"schedule.event.update":              {"agent:message"},
	"schedule.event.run":                 {"agent:message"},
	"chat.conversation.readstate.dm":     {"agent:read"},
	"chat.conversation.markread.topic":   {"project:read"},
	"chat.conversation.markread.dm":      {"agent:read"},
	"chat.conversation.markunread.topic": {"project:read"},
	"chat.conversation.markunread.dm":    {"agent:read"},
	"chat.conversation.mute.topic":       {"project:read"},
	"chat.conversation.mute.dm":          {"agent:read"},
	"chat.conversation.pin.topic":        {"project:read"},
	"chat.conversation.pin.dm":           {"agent:read"},
	"chat.conversation.typing.topic":     {"project:read"},
	"chat.conversation.typing.dm":        {"agent:read"},
	"chat.presence.update":               {"project:read"},
	"chat.search.dm":                     {"agent:read"},
	"chat.conversation.promote":          {"project:read", "agent:read"},
	"chat.star.add":                      {"project:read"},
	"chat.star.read":                     {"project:read"},
	"chat.star.state":                    {"project:read"},
	"chat.star.list":                     {"project:read"},
	"chat.conversation.history.starred":  {"project:read"},
	"chat.star.remove":                   {"project:read"},
	"chat.star.conversations":            {"project:read"},
	"quota.brokersettings.update":        {"broker:read"},
	"trigger.create":                     {"agent:message", "webhook_endpoint:read"},
	"trigger.update":                     {"agent:message", "webhook_endpoint:read"},
}

// bearerMatrixNotFoundRefusals lists the non-GET admit rows whose route
// answers every refused caller as a missing record (404), as the chat
// conversation routes do. On these rows a 404 counts as a refusal, and,
// as for a GET, only when the admitting token gets a 2xx on the same
// target.
var bearerMatrixNotFoundRefusals = map[liveInventoryKey]string{
	{"chat.conversation.send.topic", http.MethodPost, "/api/v1/chat/conversations/{id}/messages"}:     "a send the sender may not make is answered as Thread not found",
	{"chat.conversation.send.dmagent", http.MethodPost, "/api/v1/chat/conversations/{id}/messages"}:   "a send the sender may not make is answered as Thread not found",
	{"chat.conversation.markread.topic", http.MethodPost, "/api/v1/chat/conversations/{id}/read"}:     "a conversation the caller may not use is answered as Thread not found",
	{"chat.conversation.markread.dm", http.MethodPost, "/api/v1/chat/conversations/{id}/read"}:        "a conversation the caller may not use is answered as Thread not found",
	{"chat.conversation.markunread.topic", http.MethodPost, "/api/v1/chat/conversations/{id}/unread"}: "a conversation the caller may not use is answered as Thread not found",
	{"chat.conversation.markunread.dm", http.MethodPost, "/api/v1/chat/conversations/{id}/unread"}:    "a conversation the caller may not use is answered as Thread not found",
	{"chat.conversation.mute.topic", http.MethodPut, "/api/v1/chat/conversations/{id}/mute"}:          "a conversation the caller may not use is answered as Thread not found",
	{"chat.conversation.mute.dm", http.MethodPut, "/api/v1/chat/conversations/{id}/mute"}:             "a conversation the caller may not use is answered as Thread not found",
	{"chat.conversation.pin.topic", http.MethodPut, "/api/v1/chat/conversations/{id}/pin"}:            "a conversation the caller may not use is answered as Thread not found",
	{"chat.conversation.pin.dm", http.MethodPut, "/api/v1/chat/conversations/{id}/pin"}:               "a conversation the caller may not use is answered as Thread not found",
	{"chat.star.add", http.MethodPut, "/api/v1/chat/stars/{messageId}"}:                               "a message the caller may not star is answered as the shared Message not found",
}

// bearerMatrixRefusedAt reports whether status refuses a token on the
// entry point: bearerMatrixRefused, or a 404 on a row listed in
// bearerMatrixNotFoundRefusals.
func bearerMatrixRefusedAt(key liveInventoryKey, method string, status int) bool {
	if _, ok := bearerMatrixNotFoundRefusals[key]; ok && status == http.StatusNotFound {
		return true
	}
	return bearerMatrixRefused(method, status)
}

func bearerMatrixRefusalAt(key liveInventoryKey, method string) string {
	if _, ok := bearerMatrixNotFoundRefusals[key]; ok {
		return "403 or 404"
	}
	return bearerMatrixRefusal(method)
}

// TestBearerDispositionMatrix_CatalogEntryPoints drives real tokens through
// every catalogued HTTP, SSE and WebSocket entry point and checks the
// result against the operation's recorded bearer disposition:
//   - admit: a hub token with the selector (plus the route guard's
//     selector when the hub-admin guard checks a different permission, and
//     the operation's bearerMatrixCompanionSelectors) and live authority is not refused and reaches the seeded target (no 401, 403 or 404; a 5xx
//     only on a row listed in bearerMatrixPositiveServerErrors); a token
//     of the same user with an unrelated selector, and a project token for
//     another project, are refused (403, or 404 on a GET, where read
//     handlers hide the record); each row's three statuses are logged;
//   - admit_self: a token with an unrelated selector is not refused, and
//     the disposition names the test that pins its result filter;
//   - session_only: a hub token of a super-admin carrying every mintable
//     selector gets 403 with details.reason equal to the recorded reason
//     and details.credential = session_required;
//   - non_user and out_of_scope: skipped and logged;
//   - an operation with no recorded disposition yet is listed in
//     authzop.PendingBearerOperations and skipped.
//
// The refusal checks run before the admitting request, so a destructive
// entry point still has its fixture when it is refused.
func TestBearerDispositionMatrix_CatalogEntryPoints(t *testing.T) {
	m := newBearerMatrixFixture(t)
	allKey, allSelectors := m.everySelectorHubToken(t)
	t.Logf("session-only probe token carries %d selectors: %v", len(allSelectors), allSelectors)

	counts := map[string]int{}
	for _, e := range bearerMatrixEntries() {
		ep := e.EntryPoint
		label := string(e.Spec.ID) + " " + ep.Method + " " + ep.Pattern
		if ex, ok := bearerMatrixExclusions[e.key()]; ok {
			t.Logf("excluded %s: %s (pinned by %s)", label, ex.Reason, ex.Pin)
			counts["excluded"]++
			continue
		}
		d := e.Spec.Bearer
		switch d.Kind {
		case "":
			if !authzop.IsPendingBearerOperation(e.Spec.ID) {
				t.Errorf("%s has no bearer disposition and is not pending", label)
			}
			counts["pending"]++

		case authzop.BearerNonUser:
			t.Logf("skipped %s: non_user", label)
			counts["skipped"]++

		case authzop.BearerOutOfScope:
			t.Logf("skipped %s: out_of_scope, owner %s", label, d.Owner)
			counts["skipped"]++

		case authzop.BearerSessionOnly:
			rec := m.request(t, e, allKey)
			reason, credential := sessionOnlyDetailsOf(rec)
			if rec.Code != http.StatusForbidden || reason != string(d.Reason) || credential != sessionRequiredCredential {
				t.Errorf("%s: session_only %s: got %d reason=%q credential=%q, want 403 reason=%q credential=%q: %s",
					label, d.Reason, rec.Code, reason, credential, d.Reason, sessionRequiredCredential, rec.Body.String())
			}
			counts["session_only"]++

		case authzop.BearerAdmit:
			m.releaseTokens(t)
			sel := bearerMatrixSelector(e.Spec.BasePermission)
			if sel == "" {
				t.Errorf("%s: admit needs a selector for %s", label, e.Spec.BasePermission)
				continue
			}

			// (b) ceiling: the same user, an unrelated selector.
			refusedWith404 := false
			unrelated := m.mint(t, hubBoundary(), []string{bearerMatrixUnrelatedSelector(sel)})
			ceilingRec := m.request(t, e, unrelated)
			if !bearerMatrixRefusedAt(e.key(), ep.Method, ceilingRec.Code) {
				t.Errorf("%s: a token without %s got %d, want %s: %s", label, sel, ceilingRec.Code, bearerMatrixRefusalAt(e.key(), ep.Method), ceilingRec.Body.String())
			} else if ceilingRec.Code == http.StatusNotFound {
				refusedWith404 = true
			}

			// (c) boundary: a project token for another project, or for a
			// hub-only permission, a project token for the fixture project.
			// The super-admin owns the other project, so that token must
			// mint; only a hub-only selector may be refused at mint, which
			// enforces the boundary there.
			hubOnly := !bearerMatrixHasBoundary(d, authzop.BearerBoundaryProject)
			boundProject := m.otherProject
			if hubOnly {
				boundProject = m.ids.project
			}
			boundaryStatus := "not minted"
			if key := m.tryMint(projectBoundary(boundProject), []string{sel}); key != "" {
				rec := m.request(t, e, key)
				boundaryStatus = strconv.Itoa(rec.Code)
				if rec.Code == http.StatusNotFound {
					refusedWith404 = true
				}
				if !bearerMatrixRefusedAt(e.key(), ep.Method, rec.Code) {
					t.Errorf("%s: a project token for %s got %d, want %s: %s", label, boundProject, rec.Code, bearerMatrixRefusalAt(e.key(), ep.Method), rec.Body.String())
				}
			} else if hubOnly {
				t.Logf("%s: a project token for %s with %s cannot be minted; the boundary is enforced at mint", label, boundProject, sel)
			} else {
				t.Errorf("%s: a project token for %s with %s must mint for the boundary probe", label, boundProject, sel)
			}

			// (a) positive: the selector on an allowed boundary.
			boundary := hubBoundary()
			if !bearerMatrixHasBoundary(d, authzop.BearerBoundaryHub) {
				boundary = projectBoundary(m.ids.project)
			}
			positive := []string{sel}
			if guardSel := bearerMatrixGuardSelector(ep); guardSel != "" && guardSel != sel {
				positive = append(positive, guardSel)
			}
			positive = append(positive, bearerMatrixCompanionSelectors[e.Spec.ID]...)
			sort.Strings(positive)
			rec := m.request(t, e, m.mint(t, boundary, positive))
			t.Logf("admit row %s: ceiling=%d boundary=%s positive=%d with %v", label, ceilingRec.Code, boundaryStatus, rec.Code, positive)
			if rec.Code == http.StatusUnauthorized || rec.Code == http.StatusForbidden {
				t.Errorf("%s: a %s token with %s got %d, want neither 401 nor 403: %s", label, boundary.Kind, sel, rec.Code, rec.Body.String())
			}
			// The admitting token must reach the seeded target, so a 404
			// fails every row. A WebSocket upgrade cannot return 2xx to a
			// plain request, so for a WebSocket row a status other than
			// 401, 403 or 404 shows both authorization and existence; the
			// refusing tokens above got 403 or 404 on the same target.
			if rec.Code == http.StatusNotFound {
				t.Errorf("%s: the admitting token got 404, so the seeded target is not reached: %s", label, rec.Body.String())
			}
			// A row listed in bearerMatrixPositivePinned gets exactly that
			// status and error code from the admitting token.
			if want, ok := bearerMatrixPositivePinned[e.key()]; ok {
				if rec.Code != want.Status || bearerMatrixErrorCode(rec) != want.Code {
					t.Errorf("%s: the admitting token got %d %q, want %d %q (%s): %s", label, rec.Code, bearerMatrixErrorCode(rec), want.Status, want.Code, want.Reason, rec.Body.String())
				}
			}
			// A positive 5xx is expected only on the rows listed in
			// bearerMatrixPositiveServerErrors, with exactly that status,
			// and never carries an authorization or credential error code.
			if want, ok := bearerMatrixPositiveServerErrors[e.key()]; ok {
				if rec.Code != want.Status {
					t.Errorf("%s: the admitting token got %d, want %d (%s): %s", label, rec.Code, want.Status, want.Reason, rec.Body.String())
				}
			} else if rec.Code >= 500 {
				t.Errorf("%s: the admitting token got %d, and the row is not in bearerMatrixPositiveServerErrors: %s", label, rec.Code, rec.Body.String())
			}
			if rec.Code >= 500 {
				if code := bearerMatrixErrorCode(rec); bearerMatrixAuthzErrorCodes[code] {
					t.Errorf("%s: the admitting token got %d with authorization or credential code %q: %s", label, rec.Code, code, rec.Body.String())
				}
			}
			// A 404 counts as a refusal only when the admitting token
			// proves the same target exists: a 2xx, or for a WebSocket
			// entry point (which cannot answer 2xx to a plain request) any
			// status other than 404, since its handler answers 404 only
			// when the target is missing and any later status (no upgrade,
			// no runtime broker) follows the lookup.
			if refusedWith404 {
				found := rec.Code >= 200 && rec.Code < 300 ||
					(ep.Kind == authzop.EntryPointWebSocket && rec.Code != http.StatusNotFound)
				if !found {
					t.Errorf("%s: refused with 404, but the admitting token got %d, so the target is not shown to exist: %s", label, rec.Code, rec.Body.String())
				} else {
					t.Logf("404 refusal on %s: the admitting token got %d on the same target", label, rec.Code)
				}
			}
			counts["admit"]++

		case authzop.BearerAdmitSelf:
			if d.Pin == "" {
				t.Errorf("%s: admit_self names the test that pins its result filter", label)
			}
			key := m.mint(t, hubBoundary(), []string{"project:read"})
			if rec := m.request(t, e, key); rec.Code == http.StatusUnauthorized || rec.Code == http.StatusForbidden {
				t.Errorf("%s: admit_self with an unrelated selector got %d, want neither 401 nor 403: %s", label, rec.Code, rec.Body.String())
			}
			counts["admit_self"]++

		default:
			t.Errorf("%s: unknown bearer kind %q", label, d.Kind)
		}
	}

	t.Logf("bearer disposition matrix: admit=%d admit_self=%d session_only=%d skipped=%d pending=%d excluded=%d",
		counts["admit"], counts["admit_self"], counts["session_only"], counts["skipped"], counts["pending"], counts["excluded"])
	if counts["admit"] == 0 || counts["session_only"] == 0 {
		t.Fatalf("the matrix exercised no admit or no session_only entry point: %v", counts)
	}
}

// bearerMatrixPositiveServerError is the status an admitting token gets on
// a row whose handler fails after authorization in the test server.
type bearerMatrixPositiveServerError struct {
	Status int
	Reason string
}

// bearerMatrixPositivePinnedResult is the exact status and error code an
// admit row's admitting token gets, and why.
type bearerMatrixPositivePinnedResult struct {
	Status int
	Code   string
	Reason string
}

// bearerMatrixPositivePinned lists admit rows whose admitting token gets a
// fixed client error after its authorization check passes.
var bearerMatrixPositivePinned = map[liveInventoryKey]bearerMatrixPositivePinnedResult{
	{"template.update", http.MethodPut, "/api/v1/templates/{id}"}: {
		http.StatusBadRequest, "validation_error", "the empty update body has an empty name, refused before the store write"},
	{"template.global.update", http.MethodPut, "/api/v1/templates/{id}"}: {
		http.StatusBadRequest, "validation_error", "the empty update body has an empty name, refused before the store write"},
	{"harnessconfig.update", http.MethodPut, "/api/v1/harness-configs/{id}"}: {
		http.StatusBadRequest, "validation_error", "the empty update body has an empty name, refused before the store write"},
}

// bearerMatrixPositiveServerErrors lists every admit row whose admitting
// token gets a 5xx in the test server, with the status and the reason the
// handler fails after its authorization check.
var bearerMatrixPositiveServerErrors = map[liveInventoryKey]bearerMatrixPositiveServerError{
	{"agent.lifecycle.resetauth", http.MethodPost, "/api/v1/agents/{id}/reset-auth"}: {
		http.StatusInternalServerError, "the test server configures no agent dispatcher"},
	{"agent.lifecycle.resetauth", http.MethodPost, "/api/v1/projects/{projectId}/agents/{id}/reset-auth"}: {
		http.StatusInternalServerError, "the test server configures no agent dispatcher"},
	{"agent.lifecycle.exec", http.MethodPost, "/api/v1/agents/{id}/exec"}: {
		http.StatusServiceUnavailable, "the test server configures no agent dispatcher"},
	{"agent.lifecycle.exec", http.MethodPost, "/api/v1/projects/{projectId}/agents/{id}/exec"}: {
		http.StatusServiceUnavailable, "the test server configures no agent dispatcher"},
	{"project.messagelogs.read", http.MethodGet, "/api/v1/projects/{id}/message-logs"}: {
		http.StatusNotImplemented, "the test server configures no log query service"},
	// project.metrics.read (GET /api/v1/projects/{id}/metrics) and
	// hub.metrics.read (GET /api/v1/metrics/{name}, GET /api/v1/admin/metrics-dashboard)
	// are not listed: seedLiveInventoryFixtures enables tokenomics.usage_ledger,
	// so without a telemetry project they serve 200 from HubRollupSource rather
	// than 503. Experiment-off 503 behavior for all three routes is pinned by
	// TestMetricsDashboard_SourceSelectionMatrix.
	{"hub.messaging.update", http.MethodPut, "/api/v1/admin/messaging"}: {
		http.StatusNotImplemented, "the test server configures no operational settings"},
	{"hub.experiments.update", http.MethodGet, "/api/v1/admin/experiments"}: {
		http.StatusServiceUnavailable, "the test server configures no operational settings"},
	{"hub.experiments.update", http.MethodPut, "/api/v1/admin/experiments"}: {
		http.StatusServiceUnavailable, "the test server configures no operational settings"},
	{"hub.experiments.update", http.MethodDelete, "/api/v1/admin/experiments"}: {
		http.StatusServiceUnavailable, "the test server configures no operational settings"},
	{"hub.projectdefaults.update", http.MethodPut, "/api/v1/admin/project-defaults"}: {
		http.StatusNotImplemented, "the test server configures no operational settings"},
	{"hub.projectdefaults.update", http.MethodPatch, "/api/v1/admin/project-defaults"}: {
		http.StatusNotImplemented, "the test server configures no operational settings"},
	{"hub.projectdefaults.update", http.MethodPost, "/api/v1/admin/project-defaults"}: {
		http.StatusNotImplemented, "the test server configures no operational settings"},
	{"hub.diagnostics.read", http.MethodGet, "/api/v1/admin/diagnostics/logs"}: {
		http.StatusNotImplemented, "the test server configures no log query service"},
	{"hub.diagnostics.read", http.MethodGet, "/api/v1/admin/diagnostics/logs/stream"}: {
		http.StatusNotImplemented, "the test server configures no log query service"},
	{"hub.githubapp.update", http.MethodPut, "/api/v1/github-app/installations/{id}"}: {
		http.StatusInternalServerError, "the empty update body fails at the store write"},
	{"hub.githubapp.update", http.MethodPost, "/api/v1/github-app/installations/discover"}: {
		http.StatusServiceUnavailable, "the test server configures no GitHub App"},
	{"hub.githubapp.update", http.MethodPost, "/api/v1/github-app/sync-permissions"}: {
		http.StatusBadGateway, "the test server configures no GitHub App"},
	{"agent.logs.read", http.MethodGet, "/api/v1/agents/{id}/logs"}: {
		http.StatusNotImplemented, "the test server configures no agent dispatcher"},
	{"agent.logs.read", http.MethodGet, "/api/v1/projects/{projectId}/agents/{id}/logs"}: {
		http.StatusNotImplemented, "the test server configures no agent dispatcher"},
	{"agent.logs.read", http.MethodGet, "/api/v1/agents/{id}/cloud-logs"}: {
		http.StatusNotImplemented, "the test server configures no log query service"},
	{"agent.logs.read", http.MethodGet, "/api/v1/projects/{projectId}/agents/{id}/cloud-logs"}: {
		http.StatusNotImplemented, "the test server configures no log query service"},
	{"agent.messages.read", http.MethodGet, "/api/v1/agents/{id}/message-logs"}: {
		http.StatusNotImplemented, "the test server configures no log query service"},
	{"agent.messages.read", http.MethodGet, "/api/v1/projects/{projectId}/agents/{id}/message-logs"}: {
		http.StatusNotImplemented, "the test server configures no log query service"},
	{"agent.logs.read", http.MethodGet, "/api/v1/agents/{id}/cloud-logs/stream"}: {
		http.StatusNotImplemented, "the test server configures no log query service"},
	{"agent.logs.read", http.MethodGet, "/api/v1/projects/{projectId}/agents/{id}/cloud-logs/stream"}: {
		http.StatusNotImplemented, "the test server configures no log query service"},
	{"agent.messages.read", http.MethodGet, "/api/v1/agents/{id}/message-logs/stream"}: {
		http.StatusNotImplemented, "the test server configures no log query service"},
	{"agent.messages.read", http.MethodGet, "/api/v1/projects/{projectId}/agents/{id}/message-logs/stream"}: {
		http.StatusNotImplemented, "the test server configures no log query service"},
	{"project.messagelogs.read", http.MethodGet, "/api/v1/projects/{id}/message-logs/stream"}: {
		http.StatusNotImplemented, "the test server configures no log query service"},
	{"agent.workspace.sync", http.MethodPost, "/api/v1/agents/{id}/workspace/sync-from"}: {
		http.StatusBadGateway, "the fixture agent's runtime broker is not connected through the control channel"},
	{"agent.workspace.sync", http.MethodPost, "/api/v1/agents/{id}/workspace/sync-to/finalize"}: {
		http.StatusBadGateway, "the fixture agent's runtime broker is not connected through the control channel"},
	{"agent.lifecycle.keys", http.MethodPost, "/api/v1/agents/{id}/keys"}: {
		http.StatusServiceUnavailable, "the test server configures no agent dispatcher"},
	{"agent.lifecycle.keys", http.MethodPost, "/api/v1/projects/{projectId}/agents/{id}/keys"}: {
		http.StatusServiceUnavailable, "the test server configures no agent dispatcher"},
	// GET of the bare .../proxy pattern is not listed: past authorization
	// it gets the 308 to .../proxy/ (ptone/scion#5566), not a 5xx.
	{"agent.portaccess", http.MethodPost, "/api/v1/agents/{id}/ports/{port}/proxy"}: {
		http.StatusServiceUnavailable, "the fixture agent has no port tunnel"},
	{"agent.portaccess", http.MethodPut, "/api/v1/agents/{id}/ports/{port}/proxy"}: {
		http.StatusServiceUnavailable, "the fixture agent has no port tunnel"},
	{"agent.portaccess", http.MethodDelete, "/api/v1/agents/{id}/ports/{port}/proxy"}: {
		http.StatusServiceUnavailable, "the fixture agent has no port tunnel"},
	{"agent.portaccess", http.MethodGet, "/api/v1/agents/{id}/ports/{port}/proxy/{subpath}"}: {
		http.StatusServiceUnavailable, "the fixture agent has no port tunnel"},
}

// bearerMatrixAuthzErrorCodes are error codes that report an authorization
// or credential refusal; an admitting token's 5xx never carries one.
var bearerMatrixAuthzErrorCodes = map[string]bool{
	ErrCodeUnauthorized:                     true,
	ErrCodeForbidden:                        true,
	ErrCodeMembershipCredentialInsufficient: true,
	ErrCodeRoleAssignmentForbidden:          true,
	ErrCodeMutationPermissionLost:           true,
	ErrCodeSecretScopeRestricted:            true,
	ErrCodeMessageDenied:                    true,
	ErrCodePrincipalIneligible:              true,
}

// bearerMatrixErrorCode returns error.code of an error response, or "".
func bearerMatrixErrorCode(rec *httptest.ResponseRecorder) string {
	var resp ErrorResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		return ""
	}
	return resp.Error.Code
}

// bearerMatrixRefused reports whether status refuses a token. A refusal is
// 403; a GET entry point may answer 404 instead, because read handlers do
// not reveal whether a record exists to a caller that may not read it.
func bearerMatrixRefused(method string, status int) bool {
	return status == http.StatusForbidden || (method == http.MethodGet && status == http.StatusNotFound)
}

func bearerMatrixRefusal(method string) string {
	if method == http.MethodGet {
		return "403 or 404"
	}
	return "403"
}

func bearerMatrixHasBoundary(d authzop.BearerDisposition, b authzop.BearerBoundary) bool {
	for _, have := range d.Boundaries {
		if have == b {
			return true
		}
	}
	return false
}

// declaredTestFunctions returns the names of every top-level Test function
// declared in this package's test files.
func declaredTestFunctions(t *testing.T) map[string]bool {
	t.Helper()
	files, err := filepath.Glob("*_test.go")
	require.NoError(t, err)
	re := regexp.MustCompile(`(?m)^func (Test\w+)\(t \*testing\.T\)`)
	out := map[string]bool{}
	for _, f := range files {
		src, err := os.ReadFile(f)
		require.NoError(t, err)
		for _, m := range re.FindAllStringSubmatch(string(src), -1) {
			out[m[1]] = true
		}
	}
	return out
}

// TestBearerMatrixExclusions_NotStaleAndPinned requires every matrix
// exclusion to name a live catalog entry point whose disposition the matrix
// would otherwise exercise, to give a reason, and to name a declared test
// as its Pin. Every Pin recorded on a catalog disposition must also name a
// declared test. Every expected positive server error names an admit row,
// a 5xx status and a reason.
func TestBearerMatrixExclusions_NotStaleAndPinned(t *testing.T) {
	declared := declaredTestFunctions(t)
	exercised := map[liveInventoryKey]bool{}
	for _, e := range bearerMatrixEntries() {
		switch e.Spec.Bearer.Kind {
		case authzop.BearerAdmit, authzop.BearerAdmitSelf, authzop.BearerSessionOnly:
			exercised[e.key()] = true
		}
	}
	for key, ex := range bearerMatrixExclusions {
		if !exercised[key] {
			t.Errorf("stale matrix exclusion %v: no catalog entry point with an admit, admit_self or session_only disposition", key)
		}
		if strings.TrimSpace(ex.Reason) == "" {
			t.Errorf("matrix exclusion %v has no reason", key)
		}
		if ex.Pin == "" || !declared[ex.Pin] {
			t.Errorf("matrix exclusion %v: Pin %q is not a declared test in pkg/hub", key, ex.Pin)
		}
	}
	admitRows := map[liveInventoryKey]bool{}
	for _, e := range bearerMatrixEntries() {
		if e.Spec.Bearer.Kind == authzop.BearerAdmit {
			admitRows[e.key()] = true
		}
	}
	for key, want := range bearerMatrixPositiveServerErrors {
		if !admitRows[key] {
			t.Errorf("stale positive server error row %v: no catalog entry point with an admit disposition", key)
		}
		if want.Status < 500 || strings.TrimSpace(want.Reason) == "" {
			t.Errorf("positive server error row %v needs a 5xx status and a reason", key)
		}
	}
	for key, reason := range bearerMatrixNotFoundRefusals {
		if !admitRows[key] || key.Method == http.MethodGet {
			t.Errorf("stale not-found refusal row %v: no non-GET catalog entry point with an admit disposition", key)
		}
		if _, excluded := bearerMatrixExclusions[key]; excluded {
			t.Errorf("not-found refusal row %v is also excluded from the matrix", key)
		}
		if strings.TrimSpace(reason) == "" {
			t.Errorf("not-found refusal row %v needs a reason", key)
		}
	}
	for _, spec := range authzop.Catalog {
		if pin := spec.Bearer.Pin; pin != "" && !declared[pin] {
			t.Errorf("operation %s: bearer Pin %q is not a declared test in pkg/hub", spec.ID, pin)
		}
	}
}

func specAdmitsAgentJWT(spec authzop.OperationSpec) bool {
	hasPrincipal := false
	for _, p := range spec.Principals {
		if p == authzop.PrincipalAgent {
			hasPrincipal = true
			break
		}
	}
	if !hasPrincipal {
		return false
	}
	for _, c := range spec.Credentials {
		if c == authzop.CredentialAgentJWT {
			return true
		}
	}
	return false
}

// mintAgentTokenWithScopes signs and records a real agent JWT for agentID in
// projectID carrying the exact scopes given (bypassing dev-auth role upgrades
// so tests can exercise tokens lacking ScopeProjectRead).
func (m *bearerMatrixFixture) mintAgentTokenWithScopes(t *testing.T, agentID, projectID string, scopes []AgentTokenScope) string {
	t.Helper()
	tok, cred, err := m.srv.agentTokenService.SignAgentToken(AgentTokenGrant{
		AgentID:   agentID,
		ProjectID: projectID,
		Scopes:    scopes,
	}, "")
	require.NoError(t, err, "sign agent token with scopes %v", scopes)
	require.NoError(t, m.store.CreateAgentCredential(t.Context(), cred), "record agent credential")
	return tok
}

// agentProjectReadScopeExemptions lists catalogued GET entry points that admit
// PrincipalAgent + CredentialAgentJWT but by policy are NOT gated by
// ScopeProjectRead (agent self-service routes, caller-owned inbox/DM routes,
// hub-level discovery routes, or routes gated by a different scope/authority
// such as project:secret:read, project:artifact:read, or agent.attach).
//
// Routes that refuse every agent token (including routes whose handlers call
// checkAgentReadScope before a kernel or user-only refusal) belong in
// agentProjectReadRefusedForAllAgents instead, so both token dispositions are
// pinned rather than skipped.
//
// Every entry must carry a non-empty policy reason and is staleness-checked
// against both the live catalog and live enforcement behavior.
var agentProjectReadScopeExemptions = map[liveInventoryKey]string{
	{"agent.messages.read", http.MethodGet, "/api/v1/agents/{id}/messages/stream"}:                        "user-only stream endpoint (agents are refused with a stream backend installed; 501 without publisher in fixture)",
	{"agent.portaccess", http.MethodGet, "/api/v1/agents/{id}/ports/{port}/proxy"}:                        "self-only port proxy (authorizePortAccess admits only the owning agent)",
	{"agent.portaccess", http.MethodGet, "/api/v1/agents/{id}/ports/{port}/proxy/{subpath}"}:              "self-only port proxy (authorizePortAccess admits only the owning agent)",
	{"agent.port.list", http.MethodGet, "/api/v1/agents/{id}/ports"}:                                      "agent self-service route listing only the calling agent's own exposed ports",
	{"agent.port.tunnel", http.MethodGet, "/api/v1/agents/{id}/ports/tunnel"}:                             "agent self-service WebSocket tunnel gated by self-ownership and agent:port:forward",
	{"agent.usage.capabilities", http.MethodGet, "/api/v1/agents/{id}/usage/capabilities"}:                "agent self-service capabilities check (self-only; reads no project data)",
	{"agent.secrets.access", http.MethodGet, "/api/v1/agents/{id}/secrets"}:                               "agent self-service secret access gated by self-identity, not project:read",
	{"agent.secrets.access", http.MethodGet, "/api/v1/agents/{id}/secrets/{key}"}:                         "agent self-service secret access gated by self-identity, not project:read",
	{"agent.screen.read", http.MethodGet, "/api/v1/agents/{id}/screen"}:                                   "gated by agent.attach lifecycle authority rather than project:read",
	{"agent.screen.read", http.MethodGet, "/api/v1/projects/{projectId}/agents/{id}/screen"}:              "gated by agent.attach lifecycle authority rather than project:read",
	{"agent.attach", http.MethodGet, "/api/v1/agents/{id}/pty"}:                                           "gated by agent.attach lifecycle authority rather than project:read",
	{"project.secret.read", http.MethodGet, "/api/v1/projects/{id}/secrets"}:                              "gated by project:secret:read scope rather than project:read",
	{"project.secret.read", http.MethodGet, "/api/v1/projects/{id}/secrets/{key}"}:                        "gated by project:secret:read scope rather than project:read",
	{"artifact.list", http.MethodGet, "/api/v1/artifacts"}:                                                "gated per row by project:artifact:read scope (requires mine=1), not project:read",
	{"artifact.read", http.MethodGet, "/api/v1/artifacts/{id}"}:                                           "gated by project:artifact:read scope rather than project:read",
	{"artifact.read", http.MethodGet, "/api/v1/artifacts/{id}/files/{path}"}:                              "gated by project:artifact:read scope rather than project:read",
	{"artifact.read", http.MethodGet, "/api/v1/artifacts/{id}/versions/{seq}/files/{path}"}:               "gated by project:artifact:read scope rather than project:read",
	{"artifact.read", http.MethodGet, "/api/v1/artifacts/{id}/versions/{seq}/archive"}:                    "gated by project:artifact:read scope rather than project:read",
	{"artifact.review.draft.read", http.MethodGet, "/api/v1/artifacts/{id}/review-draft"}:                 "gated by project:artifact:read/create scope rather than project:read",
	{"artifact.review.request.list", http.MethodGet, "/api/v1/artifacts/{id}/review-requests"}:            "gated by project:artifact:read scope rather than project:read",
	{"artifact.version.read", http.MethodGet, "/api/v1/artifacts/{id}/versions"}:                          "gated by project:artifact:read scope rather than project:read",
	{"artifact.version.read", http.MethodGet, "/api/v1/artifacts/{id}/versions/{seq}"}:                    "gated by project:artifact:read scope rather than project:read",
	{"inbox.capabilities.read", http.MethodGet, "/api/v1/messaging/capabilities"}:                         "static self-service messaging capability discovery",
	{"inbox.conversation.list", http.MethodGet, "/api/v1/conversations"}:                                  "caller's own inbox conversations (group rows require project:read separately)",
	{"inbox.conversation.direct.read", http.MethodGet, "/api/v1/conversations/{id}"}:                      "participant-scoped direct conversation owned by the calling agent",
	{"inbox.conversation.direct.read", http.MethodGet, "/api/v1/conversations/{id}/messages"}:             "participant-scoped direct conversation messages owned by the calling agent",
	{"inbox.conversation.direct.read", http.MethodGet, "/api/v1/conversations/{id}/messages/{messageId}"}: "participant-scoped direct conversation message owned by the calling agent",
	{"inbox.conversation.direct.read", http.MethodGet, "/api/v1/conversations/{id}/watch"}:                "participant-scoped direct conversation watch owned by the calling agent",
	{"agent.message.target.resolve", http.MethodGet, "/api/v1/messaging/targets/resolve"}:                 "gated by cross-project messaging setting and authorizeAgentMessage, not project:read",
	{"chat.attachment.probe", http.MethodGet, "/api/v1/chat/attachments"}:                                 "static self-service chat attachment capability probe",
	{"user.self.profile", http.MethodGet, "/api/v1/auth/me"}:                                              "caller self-profile endpoint",
	{"hub.experiments.read", http.MethodGet, "/api/v1/experiments"}:                                       "hub-level experiment flag discovery for any signed-in identity",
	{"hub.profiling.read", http.MethodGet, "/api/v1/profiling"}:                                           "hub-level profiling status discovery for any signed-in identity",
	{"hub.injectedskills.read", http.MethodGet, "/api/v1/hub/settings/injected-skills"}:                   "hub-level injected skills settings for any signed-in identity",
	{"hub.conduitgrantkeys.read", http.MethodGet, "/api/v1/conduit/grant-keys"}:                           "hub-level conduit grant public keys for any signed-in identity",
	{"hub.triggerpresets.read", http.MethodGet, "/api/v1/trigger-presets"}:                                "hub-level trigger presets discovery, not a project read",
	{"env.hub.read", http.MethodGet, "/api/v1/env/{key}"}:                                                 "hub-level env key lookup, not a project read",
	{"secret.hub.read", http.MethodGet, "/api/v1/secrets"}:                                                "hub-level secret listing, not a project read",
	{"secret.hub.read", http.MethodGet, "/api/v1/secrets/{key}"}:                                          "hub-level secret key lookup, not a project read",
	{"agent.delegation.list", http.MethodGet, "/api/v1/agents/{id}/delegations"}:                          "agent self-service listing only its own delegations",
	{"agent.delegation.read", http.MethodGet, "/api/v1/agents/{id}/delegations/{grantId}"}:                "agent self-service reading only its own delegation grant",
}

// agentProjectReadRefusedPin describes the expected refusal behavior for a
// catalogued GET entry point that refuses all agent tokens (both with and
// without ScopeProjectRead).
type agentProjectReadRefusedPin struct {
	// RequireScopeRefusal is true when the handler calls checkAgentReadScope
	// before the downstream agent refusal, so the token lacking
	// ScopeProjectRead must specifically receive the project:read scope refusal.
	RequireScopeRefusal bool
	Reason              string
}

// agentProjectReadRefusedForAllAgents pins catalogued GET entry points that
// refuse every agent token (both without and with ScopeProjectRead). Unlike
// agentProjectReadScopeExemptions, every entry here is actively asserted for
// both token kinds and fails if either token is admitted or if a
// checkAgentReadScope-guarded route stops returning the scope refusal.
// Entries are staleness-checked against the live catalog and live responses.
var agentProjectReadRefusedForAllAgents = map[liveInventoryKey]agentProjectReadRefusedPin{
	{"agent.logs.read", http.MethodGet, "/api/v1/agents/{id}/logs"}: {
		RequireScopeRefusal: true,
		Reason:              "handleAgentLogs calls checkAgentReadScope then authorizeAgentLogsRead (kernel agent.read refuses all agent tokens)",
	},
	{"agent.logs.read", http.MethodGet, "/api/v1/projects/{projectId}/agents/{id}/logs"}: {
		RequireScopeRefusal: true,
		Reason:              "handleAgentLogs calls checkAgentReadScope then authorizeAgentLogsRead (kernel agent.read refuses all agent tokens)",
	},
	{"agent.logs.read", http.MethodGet, "/api/v1/agents/{id}/cloud-logs"}: {
		RequireScopeRefusal: true,
		Reason:              "handleAgentCloudLogs calls checkAgentReadScope then authorizeAgentLogsRead (kernel agent.read refuses all agent tokens)",
	},
	{"agent.logs.read", http.MethodGet, "/api/v1/projects/{projectId}/agents/{id}/cloud-logs"}: {
		RequireScopeRefusal: true,
		Reason:              "handleAgentCloudLogs calls checkAgentReadScope then authorizeAgentLogsRead (kernel agent.read refuses all agent tokens)",
	},
	{"agent.logs.read", http.MethodGet, "/api/v1/agents/{id}/cloud-logs/stream"}: {
		RequireScopeRefusal: true,
		Reason:              "handleAgentCloudLogsStream calls checkAgentReadScope then authorizeAgentLogsRead (kernel agent.read refuses all agent tokens)",
	},
	{"agent.logs.read", http.MethodGet, "/api/v1/projects/{projectId}/agents/{id}/cloud-logs/stream"}: {
		RequireScopeRefusal: true,
		Reason:              "handleAgentCloudLogsStream calls checkAgentReadScope then authorizeAgentLogsRead (kernel agent.read refuses all agent tokens)",
	},
	{"agent.messages.read", http.MethodGet, "/api/v1/agents/{id}/messages"}: {
		RequireScopeRefusal: true,
		Reason:              "handleAgentMessages calls checkAgentReadScope then requires a UserIdentity (refuses all agent tokens)",
	},
	{"agent.delegation.list", http.MethodGet, "/api/v1/users/me/delegations"}: {
		RequireScopeRefusal: false,
		Reason:              "handleUserMeDelegations is session-only (admitAgentDelegationCaller refuses all agent tokens)",
	},
	{"gcp.identity.read", http.MethodGet, "/api/v1/gcp-service-accounts/{id}"}: {
		RequireScopeRefusal: false,
		Reason:              "flat by-ID route requires a user identity; agent callers get 404",
	},
}

// agentProjectReadFixturePending lists catalogued GET entry points that cannot
// reach a positive control in the generic bearer-matrix fixture (for example,
// behind a default-off experiment or requiring specialized execution fixtures).
// They are NOT counted as enforced and are staleness-checked so any route that
// begins passing the positive control must be promoted out of this list.
var agentProjectReadFixturePending = map[liveInventoryKey]string{
	{"schedule.event.read", http.MethodGet, "/api/v1/projects/{projectId}/scheduled-events/{id}/output"}: "requires an exec-type scheduled event with captured output and live agent:attach on the target agent",
	{"project.messagepolicy.read", http.MethodGet, "/api/v1/projects/{id}/message-policies"}:             "answers 404 while hub.message_policies experiment is off in the bearer-matrix fixture",
	{"project.messagepolicy.read", http.MethodGet, "/api/v1/projects/{id}/message-policies/{policyId}"}:  "answers 404 while hub.message_policies experiment is off in the bearer-matrix fixture",
}

// agentProjectReadPositiveAdmitted reports whether rec represents a valid
// positive control for an agent carrying ScopeProjectRead: either 2xx, or the
// exact post-auth 5xx documented in bearerMatrixPositiveServerErrors (for
// example 501 on log routes when no dispatcher or log-query service is
// configured in the test server), never carrying an authz error code.
func agentProjectReadPositiveAdmitted(key liveInventoryKey, rec *httptest.ResponseRecorder) bool {
	if rec.Code >= 200 && rec.Code < 300 {
		return true
	}
	if want, ok := bearerMatrixPositiveServerErrors[key]; ok && rec.Code == want.Status {
		return !bearerMatrixAuthzErrorCodes[bearerMatrixErrorCode(rec)]
	}
	return false
}

// agentProjectReadRefusedByScope reports whether rec is a specific refusal
// attributable to missing ScopeProjectRead:
//   - HTTP 403 with error.code == "forbidden" and a scope/permission denial
//     message ("Missing required scope: project:read" from checkAgentReadScope,
//     or "Insufficient permissions" from the authz kernel's project.read check), or
//   - HTTP 404 on inbox.conversation.resolve, which masks denied project
//     conversation lookups as not_found (paired with a 200 positive control).
func agentProjectReadRefusedByScope(key liveInventoryKey, rec *httptest.ResponseRecorder) bool {
	var resp ErrorResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		return false
	}
	if key.OperationID == "inbox.conversation.resolve" && rec.Code == http.StatusNotFound && resp.Error.Code == ErrCodeNotFound {
		return true
	}
	if rec.Code != http.StatusForbidden || resp.Error.Code != ErrCodeForbidden {
		return false
	}
	return strings.Contains(resp.Error.Message, string(ScopeProjectRead)) ||
		strings.Contains(resp.Error.Message, "Insufficient permissions")
}

// agentProjectReadRefusedByScopeCheck reports whether rec is specifically the
// checkAgentReadScope refusal (HTTP 403 forbidden mentioning ScopeProjectRead).
func agentProjectReadRefusedByScopeCheck(rec *httptest.ResponseRecorder) bool {
	if rec.Code != http.StatusForbidden {
		return false
	}
	var resp ErrorResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		return false
	}
	return resp.Error.Code == ErrCodeForbidden && strings.Contains(resp.Error.Message, string(ScopeProjectRead))
}

// TestAgentProjectReadScopeMatrix_CatalogEntryPoints drives real agent JWTs
// through every catalogued GET entry point that admits PrincipalAgent with
// CredentialAgentJWT (deny-by-default).
//
// The admitting token carries ScopesForRole(AgentRoleFull) (which includes
// ScopeProjectRead); the refused token carries the exact same bundle minus
// ScopeProjectRead. Every non-exempt, non-refused-all, non-fixture-pending
// entry point must:
//  1. Pass a positive control with ScopeProjectRead (2xx, or its documented
//     post-auth 5xx in bearerMatrixPositiveServerErrors), and
//  2. Be refused specifically by the scope/permission check without
//     ScopeProjectRead.
//
// Routes that refuse every agent token (including checkAgentReadScope-guarded
// routes that are also refused downstream for all agents) are pinned in
// agentProjectReadRefusedForAllAgents and asserted for both token kinds.
//
// agentProjectReadScopeExemptions, agentProjectReadRefusedForAllAgents, and
// agentProjectReadFixturePending are all checked for staleness against the
// live catalog and live handler behavior.
func TestAgentProjectReadScopeMatrix_CatalogEntryPoints(t *testing.T) {
	m := newBearerMatrixFixture(t)
	ctx := t.Context()
	now := time.Now().UTC()

	// Seed agent-owned schedule, scheduled event, webhook endpoint, trigger,
	// and trigger event so row-owned read routes have real positive controls.
	agentAttribution := store.InitiatorAttribution{
		InitiatorPrincipalKind:  "agent",
		InitiatorPrincipalID:    m.ids.agent,
		InitiatorCredentialKind: store.InitiatorCredentialKindAgent,
		AttributionVersion:      1,
		AuthorizationRevision:   1,
	}

	agentSchedID := tid("aprm-sched")
	require.NoError(t, m.store.CreateSchedule(ctx, &store.Schedule{
		ID: agentSchedID, ProjectID: m.ids.project, Name: "aprm-schedule",
		CronExpr: "0 0 * * *", EventType: "message",
		Payload: `{"agentId":"` + m.ids.agent + `","message":"ping"}`,
		Status:  store.ScheduleStatusActive, CreatedBy: m.ids.agent,
		InitiatorAttribution: agentAttribution,
		CreatedAt:            now, UpdatedAt: now,
	}))
	m.ids.schedule = agentSchedID

	agentEvtID := tid("aprm-evt")
	require.NoError(t, m.store.CreateScheduledEvent(ctx, &store.ScheduledEvent{
		ID: agentEvtID, ProjectID: m.ids.project, EventType: "message",
		FireAt: now.Add(time.Hour), Payload: `{"agentId":"` + m.ids.agent + `","message":"ping"}`,
		Status: store.ScheduledEventPending, CreatedBy: m.ids.agent,
		InitiatorAttribution: agentAttribution, CreatedAt: now,
	}))
	m.ids.scheduledEvent = agentEvtID

	agentEpID := tid("aprm-wh")
	require.NoError(t, m.store.CreateWebhookEndpoint(ctx, &store.WebhookEndpoint{
		ID: agentEpID, ProjectID: m.ids.project, Name: "aprm-webhook-ep",
		Status: store.WebhookEndpointStatusActive, SignatureMode: store.WebhookSignatureModeScionV1,
		SecretVersion: 1, OwnerPrincipalKind: "agent", OwnerPrincipalID: m.ids.agent,
		OwnerCredentialKind: store.InitiatorCredentialKindAgent,
		CreatedBy:           m.ids.agent, CreatedAt: now, UpdatedAt: now,
	}))
	m.ids.webhookEndpoint = agentEpID

	agentTrigID := tid("aprm-trig")
	require.NoError(t, m.store.CreateTrigger(ctx, &store.Trigger{
		ID: agentTrigID, ProjectID: m.ids.project, Name: "aprm-trigger",
		Status: store.TriggerStatusActive, StatusReason: store.TriggerStatusReasonNone,
		SourceKind: store.TriggerSourceKindWebhook, SourceID: agentEpID,
		MatchTypes: []string{"webhook.received"}, MatchDialect: "cel",
		ActionType: "message", ActionPayload: `{"agentId":"` + m.ids.agent + `","message":"hi"}`,
		SubjectDialect: "cel", SubjectMode: store.TriggerSubjectModeNew,
		CreatedBy:            m.ids.agent,
		InitiatorAttribution: agentAttribution,
		CreatedAt:            now, UpdatedAt: now,
	}))
	m.ids.trigger = agentTrigID

	agentTrigEvtID := tid("aprm-trigevt")
	require.NoError(t, m.store.CreateTriggerEvent(ctx, &store.TriggerEvent{
		ID: agentTrigEvtID, ProjectID: m.ids.project,
		SourceKind: store.TriggerSourceKindWebhook, SourceID: agentEpID,
		Type: "webhook.received", Subject: "webhook:" + agentEpID,
		DedupKey: "aprm-trigger-event-dedup", OccurredAt: now, ReceivedAt: now,
		Attributes: map[string]string{"endpoint_name": "aprm-webhook-ep"},
		Headers:    map[string]string{"Content-Type": "application/json"},
		Payload:    `{"ok":true}`, PayloadBytes: 11,
		Status: store.TriggerEventStatusProcessed, CreatedAt: now,
	}))
	m.ids.triggerEvent = agentTrigEvtID

	// Seed project-scoped env var and GCP service account so env.read and
	// nested gcp.identity.read routes have real positive controls.
	const aprmProjectEnvKey = "APRM_PROJECT_ENV"
	_, err := m.store.UpsertEnvVar(ctx, &store.EnvVar{
		ID:      tid("aprm-env"),
		Key:     aprmProjectEnvKey,
		Value:   "aprm-val",
		Scope:   store.ScopeProject,
		ScopeID: m.ids.project,
		Created: now,
		Updated: now,
	})
	require.NoError(t, err)

	aprmProjSAID := tid("aprm-proj-sa")
	require.NoError(t, m.store.CreateGCPServiceAccount(ctx, &store.GCPServiceAccount{
		ID:        aprmProjSAID,
		Scope:     store.ScopeProject,
		ScopeID:   m.ids.project,
		Email:     "aprm-proj-sa@li-gcp-project.iam.gserviceaccount.com",
		ProjectID: "li-gcp-project",
		CreatedAt: now,
	}))

	fullScopes := ScopesForRole(AgentRoleFull)
	var withoutScopes []AgentTokenScope
	for _, sc := range fullScopes {
		if sc != ScopeProjectRead {
			withoutScopes = append(withoutScopes, sc)
		}
	}
	withoutScope := m.mintAgentTokenWithScopes(t, m.ids.agent, m.ids.project, withoutScopes)
	withScope := m.mintAgentTokenWithScopes(t, m.ids.agent, m.ids.project, fullScopes)

	// Per-route request helper supplying project scope query params and
	// project-scoped IDs so project-scoped branches are exercised with
	// positive controls.
	send := func(e bearerMatrixEntry, token string) *httptest.ResponseRecorder {
		key := e.key()
		switch {
		case key.OperationID == "gcp.identity.read" && key.Pattern == "/api/v1/gcp-service-accounts":
			return doRequestWithAgentToken(t, m.srv, http.MethodGet,
				"/api/v1/gcp-service-accounts?scope=project&scopeId="+m.ids.project, nil, token)
		case key.OperationID == "gcp.identity.read" && key.Pattern == "/api/v1/projects/{id}/gcp-service-accounts/{saId}":
			return doRequestWithAgentToken(t, m.srv, http.MethodGet,
				"/api/v1/projects/"+m.ids.project+"/gcp-service-accounts/"+aprmProjSAID, nil, token)
		case key.OperationID == "gcp.identity.read" && key.Pattern == "/api/v1/projects/{id}/gcp-service-accounts/{saId}/status":
			return doRequestWithAgentToken(t, m.srv, http.MethodGet,
				"/api/v1/projects/"+m.ids.project+"/gcp-service-accounts/"+aprmProjSAID+"/status", nil, token)
		case key.OperationID == "env.read" && key.Pattern == "/api/v1/env":
			return doRequestWithAgentToken(t, m.srv, http.MethodGet,
				"/api/v1/env?scope=project&scopeId="+m.ids.project, nil, token)
		case key.OperationID == "env.read" && key.Pattern == "/api/v1/env/{key}":
			return doRequestWithAgentToken(t, m.srv, http.MethodGet,
				"/api/v1/env/"+aprmProjectEnvKey+"?scope=project&scopeId="+m.ids.project, nil, token)
		}
		return m.request(t, e, token)
	}

	seen := map[liveInventoryKey]bool{}
	enforced := 0
	refusedAll := 0
	exempted := 0
	fixturePending := 0

	for _, e := range bearerMatrixEntries() {
		ep := e.EntryPoint
		if ep.Method != http.MethodGet || !specAdmitsAgentJWT(e.Spec) {
			continue
		}
		key := e.key()
		seen[key] = true
		label := string(e.Spec.ID) + " " + ep.Method + " " + ep.Pattern

		recWithout := send(e, withoutScope)
		recWith := send(e, withScope)

		if pin, ok := agentProjectReadRefusedForAllAgents[key]; ok {
			if strings.TrimSpace(pin.Reason) == "" {
				t.Errorf("refused-for-all-agents entry %v has an empty reason", key)
			}
			if _, dup := agentProjectReadScopeExemptions[key]; dup {
				t.Errorf("key %v appears in both agentProjectReadRefusedForAllAgents and agentProjectReadScopeExemptions", key)
			}
			if _, dup := agentProjectReadFixturePending[key]; dup {
				t.Errorf("key %v appears in both agentProjectReadRefusedForAllAgents and agentProjectReadFixturePending", key)
			}
			if pin.RequireScopeRefusal {
				if !agentProjectReadRefusedByScopeCheck(recWithout) {
					t.Errorf("%s: agent token without %s got %d (%s), want %s scope refusal: %s",
						label, ScopeProjectRead, recWithout.Code, bearerMatrixErrorCode(recWithout), ScopeProjectRead, recWithout.Body.String())
				}
			} else if !bearerMatrixRefused(ep.Method, recWithout.Code) {
				t.Errorf("%s: agent token without %s got %d, want %s: %s",
					label, ScopeProjectRead, recWithout.Code, bearerMatrixRefusal(ep.Method), recWithout.Body.String())
			}
			if agentProjectReadPositiveAdmitted(key, recWith) {
				t.Errorf("stale refused-for-all-agents entry %s: with-scope agent token was admitted (with=%d); promote to enforced",
					label, recWith.Code)
			} else if !bearerMatrixRefused(ep.Method, recWith.Code) {
				t.Errorf("%s: agent token with %s got %d, want %s: %s",
					label, ScopeProjectRead, recWith.Code, bearerMatrixRefusal(ep.Method), recWith.Body.String())
			}
			refusedAll++
			continue
		}

		if reason, ok := agentProjectReadScopeExemptions[key]; ok {
			if strings.TrimSpace(reason) == "" {
				t.Errorf("exemption %v has an empty reason", key)
			}
			if _, dup := agentProjectReadFixturePending[key]; dup {
				t.Errorf("key %v appears in both agentProjectReadScopeExemptions and agentProjectReadFixturePending", key)
			}
			if agentProjectReadRefusedByScope(key, recWithout) && agentProjectReadPositiveAdmitted(key, recWith) {
				t.Errorf("stale exemption %s: route enforces %s (without=%d, with=%d); remove from agentProjectReadScopeExemptions",
					label, ScopeProjectRead, recWithout.Code, recWith.Code)
			}
			exempted++
			continue
		}

		if reason, ok := agentProjectReadFixturePending[key]; ok {
			if strings.TrimSpace(reason) == "" {
				t.Errorf("fixture-pending entry %v has an empty reason", key)
			}
			if agentProjectReadPositiveAdmitted(key, recWith) {
				t.Errorf("stale fixture-pending %s: positive control succeeded (with=%d); promote out of agentProjectReadFixturePending",
					label, recWith.Code)
			}
			fixturePending++
			continue
		}

		// Enforced row: must pass BOTH positive control (with project:read) and
		// specific scope refusal (without project:read).
		if !agentProjectReadPositiveAdmitted(key, recWith) {
			t.Errorf("%s: positive control with %s got %d, want 2xx or documented post-auth 5xx (add fixture/override or move to agentProjectReadFixturePending): %s",
				label, ScopeProjectRead, recWith.Code, recWith.Body.String())
		}
		if !agentProjectReadRefusedByScope(key, recWithout) {
			t.Errorf("%s: agent token without %s got %d (%s), want scope/permission refusal (or add a policy-justified entry to agentProjectReadScopeExemptions): %s",
				label, ScopeProjectRead, recWithout.Code, bearerMatrixErrorCode(recWithout), recWithout.Body.String())
		}
		enforced++
	}

	for key := range agentProjectReadRefusedForAllAgents {
		if !seen[key] {
			t.Errorf("stale refused-for-all-agents entry %v: no live catalog GET entry point admits PrincipalAgent with CredentialAgentJWT", key)
		}
	}
	for key := range agentProjectReadScopeExemptions {
		if !seen[key] {
			t.Errorf("stale exemption %v: no live catalog GET entry point admits PrincipalAgent with CredentialAgentJWT", key)
		}
	}
	for key := range agentProjectReadFixturePending {
		if !seen[key] {
			t.Errorf("stale fixture-pending entry %v: no live catalog GET entry point admits PrincipalAgent with CredentialAgentJWT", key)
		}
	}

	if enforced == 0 {
		t.Fatal("matrix exercised no catalog entry points enforcing agent project:read")
	}
	t.Logf("agent project:read scope matrix: enforced=%d refused_all=%d exempted=%d fixture_pending=%d", enforced, refusedAll, exempted, fixturePending)
}

// TestProjectMetricsRoutes_AgentProjectReadScope verifies that the three
// project metrics routes refuse same-project agent tokens lacking project:read,
// continue admitting same-project agent tokens carrying project:read and
// authorized user callers, and keep refusing cross-project agent tokens.
func TestProjectMetricsRoutes_AgentProjectReadScope(t *testing.T) {
	m := newBearerMatrixFixture(t)

	withoutScope := m.mintAgentTokenWithScopes(t, m.ids.agent, m.ids.project, []AgentTokenScope{ScopeAgentStatusUpdate})
	withScope := m.mintAgentTokenWithScopes(t, m.ids.agent, m.ids.project, []AgentTokenScope{ScopeAgentStatusUpdate, ScopeProjectRead})
	otherProjectAgent := m.mintAgentTokenWithScopes(t, tid("other-proj-agent"), m.otherProject, []AgentTokenScope{ScopeAgentStatusUpdate, ScopeProjectRead})

	paths := []string{
		"/api/v1/projects/" + m.ids.project + "/metrics-summary",
		"/api/v1/projects/" + m.ids.project + "/metrics/summary",
		"/api/v1/projects/" + m.ids.project + "/metrics",
	}

	for _, path := range paths {
		t.Run(path, func(t *testing.T) {
			recNoScope := doRequestWithAgentToken(t, m.srv, http.MethodGet, path, nil, withoutScope)
			require.Equal(t, http.StatusForbidden, recNoScope.Code,
				"same-project agent without project:read must be refused: %s", recNoScope.Body.String())

			recWithScope := doRequestWithAgentToken(t, m.srv, http.MethodGet, path, nil, withScope)
			require.Equal(t, http.StatusOK, recWithScope.Code,
				"same-project agent with project:read must succeed: %s", recWithScope.Body.String())

			recOtherProj := doRequestWithAgentToken(t, m.srv, http.MethodGet, path, nil, otherProjectAgent)
			require.Equal(t, http.StatusForbidden, recOtherProj.Code,
				"cross-project agent with project:read must be refused: %s", recOtherProj.Body.String())

			recUser := doRequest(t, m.srv, http.MethodGet, path, nil)
			require.Equal(t, http.StatusOK, recUser.Code,
				"authorized user caller must remain unaffected: %s", recUser.Body.String())
		})
	}
}

// TestProjectEnvAndGCPSARoutes_AgentProjectReadScope verifies that the
// project-scoped env read routes and nested project GCP service account item
// and status routes enforce project:read for same-project agents, continue
// admitting same-project agents with project:read and authorized user callers,
// and refuse cross-project agents.
func TestProjectEnvAndGCPSARoutes_AgentProjectReadScope(t *testing.T) {
	m := newBearerMatrixFixture(t)
	ctx := t.Context()
	now := time.Now().UTC()

	const envKey = "SCOPE_TEST_PROJECT_ENV"
	_, err := m.store.UpsertEnvVar(ctx, &store.EnvVar{
		ID:      tid("scope-test-env"),
		Key:     envKey,
		Value:   "val",
		Scope:   store.ScopeProject,
		ScopeID: m.ids.project,
		Created: now,
		Updated: now,
	})
	require.NoError(t, err)

	projSAID := tid("scope-test-proj-sa")
	require.NoError(t, m.store.CreateGCPServiceAccount(ctx, &store.GCPServiceAccount{
		ID:        projSAID,
		Scope:     store.ScopeProject,
		ScopeID:   m.ids.project,
		Email:     "scope-test-proj-sa@li-gcp-project.iam.gserviceaccount.com",
		ProjectID: "li-gcp-project",
		CreatedAt: now,
	}))

	withoutScope := m.mintAgentTokenWithScopes(t, m.ids.agent, m.ids.project, []AgentTokenScope{ScopeAgentStatusUpdate})
	withScope := m.mintAgentTokenWithScopes(t, m.ids.agent, m.ids.project, []AgentTokenScope{ScopeAgentStatusUpdate, ScopeProjectRead})
	otherProjectAgent := m.mintAgentTokenWithScopes(t, tid("other-proj-agent-env-sa"), m.otherProject, []AgentTokenScope{ScopeAgentStatusUpdate, ScopeProjectRead})

	cases := []struct {
		name              string
		path              string
		crossProjectCodes []int
	}{
		{
			name:              "env list",
			path:              "/api/v1/env?scope=project&scopeId=" + m.ids.project,
			crossProjectCodes: []int{http.StatusForbidden},
		},
		{
			name:              "env get",
			path:              "/api/v1/env/" + envKey + "?scope=project&scopeId=" + m.ids.project,
			crossProjectCodes: []int{http.StatusForbidden},
		},
		{
			name:              "project gcp sa get",
			path:              "/api/v1/projects/" + m.ids.project + "/gcp-service-accounts/" + projSAID,
			crossProjectCodes: []int{http.StatusForbidden, http.StatusNotFound},
		},
		{
			name:              "project gcp sa status",
			path:              "/api/v1/projects/" + m.ids.project + "/gcp-service-accounts/" + projSAID + "/status",
			crossProjectCodes: []int{http.StatusForbidden, http.StatusNotFound},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			recNoScope := doRequestWithAgentToken(t, m.srv, http.MethodGet, tc.path, nil, withoutScope)
			require.Equal(t, http.StatusForbidden, recNoScope.Code,
				"same-project agent without project:read must be refused: %s", recNoScope.Body.String())

			recWithScope := doRequestWithAgentToken(t, m.srv, http.MethodGet, tc.path, nil, withScope)
			require.Equal(t, http.StatusOK, recWithScope.Code,
				"same-project agent with project:read must succeed: %s", recWithScope.Body.String())

			recOtherProj := doRequestWithAgentToken(t, m.srv, http.MethodGet, tc.path, nil, otherProjectAgent)
			require.Contains(t, tc.crossProjectCodes, recOtherProj.Code,
				"cross-project agent with project:read must be refused: %s", recOtherProj.Body.String())

			recUser := doRequest(t, m.srv, http.MethodGet, tc.path, nil)
			require.Equal(t, http.StatusOK, recUser.Code,
				"authorized user caller must remain unaffected: %s", recUser.Body.String())
		})
	}
}
