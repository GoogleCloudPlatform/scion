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

package permissions

import (
	"fmt"
	"sort"
	"strings"
)

const (
	ResourceAgent             = "agent"
	ResourceProject           = "project"
	ResourceSkill             = "skill"
	ResourceTemplate          = "template"
	ResourceHarnessConfig     = "harness_config"
	ResourceGroup             = "group"
	ResourceUser              = "user"
	ResourcePolicy            = "policy"
	ResourceBroker            = "broker"
	ResourceGCPServiceAccount = "gcp_service_account"
	ResourceHub               = "hub"
	ResourceQuota             = "quota"
	ResourceRole              = "role"
	ResourceRoleBinding       = "role_binding"
	ResourceScheduledEvent    = "scheduled_event"

	ActionCreate         = "create"
	ActionRead           = "read"
	ActionUpdate         = "update"
	ActionDelete         = "delete"
	ActionList           = "list"
	ActionManage         = "manage"
	ActionAttach         = "attach"
	ActionPortAccess     = "port_access"
	ActionRegister       = "register"
	ActionAddMember      = "addMember"
	ActionRemoveMember   = "removeMember"
	ActionDispatch       = "dispatch"
	ActionStopAll        = "stop_all"
	ActionVerify         = "verify"
	ActionMint           = "mint"
	ActionAssign         = "assign"
	ActionInvite         = "invite"
	ActionSuspend        = "suspend"
	ActionPromote        = "promote"
	ActionClone          = "clone"
	ActionExecute        = "execute"
	ActionMessage        = "message"
	ActionSetMessageMode = "set_message_mode"

	UATScopeAgentManage         = "agent:manage"
	UATScopeSkillManage         = "skill:manage"
	UATScopeTemplateManage      = "template:manage"
	UATScopeHarnessConfigManage = "harness_config:manage"
	UATScopeGroupManage         = "group:manage"
)

// UATManageAliases maps each manage-alias scope to its resource type.
// Only resource types with 5+ UAT scopes get aliases — types with fewer
// scopes (broker, user, gcp_service_account, project) are not worth aliasing.
var UATManageAliases = map[string]string{
	UATScopeAgentManage:         ResourceAgent,
	UATScopeSkillManage:         ResourceSkill,
	UATScopeTemplateManage:      ResourceTemplate,
	UATScopeHarnessConfigManage: ResourceHarnessConfig,
	UATScopeGroupManage:         ResourceGroup,
}

// CapabilityKind says whether a permission applies to an individual resource or
// to a collection/scope. It drives Hub capability projections.
type CapabilityKind string

const (
	CapabilityNone     CapabilityKind = ""
	CapabilityResource CapabilityKind = "resource"
	CapabilityScope    CapabilityKind = "scope"
)

// Permission describes one canonical resource/action pair. Scope strings and
// capability projections are metadata on the permission, not separate lists.
type Permission struct {
	ID             string
	Resource       string
	Action         string
	CapabilityKind CapabilityKind
	UATScope       string
	AgentScopes    []string
	Description    string
	Enforcement    []string
	NonRouteUse    []string
}

// Registry is the canonical permission/resource vocabulary for Hub authz.
//
// Phase 1A keeps existing handler-local enforcement; the Enforcement and
// NonRouteUse fields record where each permission is currently consumed so drift
// tests can fail when a public scope has no corresponding use.
var Registry = []Permission{
	{ID: "agent.create", Resource: ResourceAgent, Action: ActionCreate, CapabilityKind: CapabilityScope, UATScope: "agent:create", AgentScopes: []string{"project:agent:create"}, Description: "Create agents", Enforcement: []string{"pkg/hub/authorize.go:authorizeAgentCreate", "pkg/hub/handlers_agents_core.go"}},
	{ID: "agent.read", Resource: ResourceAgent, Action: ActionRead, CapabilityKind: CapabilityResource, UATScope: "agent:read", Description: "Read agent status and metadata", Enforcement: []string{"pkg/hub/handlers_agents_core.go", "pkg/hub/authz.go"}},
	{ID: "agent.list", Resource: ResourceAgent, Action: ActionList, CapabilityKind: CapabilityScope, UATScope: "agent:list", Description: "List agents in the project", Enforcement: []string{"pkg/hub/handlers_agents_core.go", "pkg/hub/authz.go"}},
	{ID: "agent.update", Resource: ResourceAgent, Action: ActionUpdate, CapabilityKind: CapabilityResource, Description: "Update agents", Enforcement: []string{"pkg/hub/handlers_agents_core.go"}},
	{ID: "agent.delete", Resource: ResourceAgent, Action: ActionDelete, CapabilityKind: CapabilityResource, UATScope: "agent:delete", AgentScopes: []string{"project:agent:lifecycle"}, Description: "Delete agents", Enforcement: []string{"pkg/hub/handlers_agents_core.go", "pkg/hub/handlers_agent_delete_authz_test.go"}},
	{ID: "agent.attach", Resource: ResourceAgent, Action: ActionAttach, CapabilityKind: CapabilityResource, UATScope: "agent:attach", AgentScopes: []string{"project:agent:lifecycle"}, Description: "Attach to agent sessions", Enforcement: []string{"pkg/hub/authorize.go:authorizeAgentLifecycle", "pkg/hub/pty_handlers.go"}},
	{ID: "agent.port_access", Resource: ResourceAgent, Action: ActionPortAccess, CapabilityKind: CapabilityResource, UATScope: "agent:port_access", Description: "Access agent forwarded ports", Enforcement: []string{"pkg/hub/port_forward_handlers.go"}},
	{ID: "agent.stop_all", Resource: ResourceAgent, Action: ActionStopAll, CapabilityKind: CapabilityScope, Description: "Stop all agents", Enforcement: []string{"pkg/hub/handlers_agents_core.go"}},
	{ID: "agent.message", Resource: ResourceAgent, Action: ActionMessage, CapabilityKind: CapabilityScope, UATScope: "agent:message", Description: "Send messages to agents", NonRouteUse: []string{"Phase 2: pkg/hub/authorize.go:authorizeAgentMessage"}},
	{ID: "agent.set_message_mode", Resource: ResourceAgent, Action: ActionSetMessageMode, CapabilityKind: CapabilityResource, Description: "Change agent message mode", Enforcement: []string{"pkg/hub/handlers_agents_core.go"}},

	{ID: "project.create", Resource: ResourceProject, Action: ActionCreate, CapabilityKind: CapabilityScope, Description: "Create projects", Enforcement: []string{"pkg/hub/handlers_projects_core.go"}},
	{ID: "project.read", Resource: ResourceProject, Action: ActionRead, CapabilityKind: CapabilityResource, UATScope: "project:read", AgentScopes: []string{"project:read"}, Description: "Read project metadata", Enforcement: []string{"pkg/hub/handlers_projects_core.go", "pkg/hub/authz.go"}},
	{ID: "project.update", Resource: ResourceProject, Action: ActionUpdate, CapabilityKind: CapabilityResource, UATScope: "project:update", Description: "Update projects", Enforcement: []string{"pkg/hub/handlers_projects_core.go", "pkg/hub/authz.go"}},
	{ID: "project.delete", Resource: ResourceProject, Action: ActionDelete, CapabilityKind: CapabilityResource, Description: "Delete projects", Enforcement: []string{"pkg/hub/handlers_projects_core.go"}},
	{ID: "project.manage", Resource: ResourceProject, Action: ActionManage, CapabilityKind: CapabilityResource, Description: "Manage project administration", Enforcement: []string{"pkg/hub/handlers_projects_core.go"}},
	{ID: "project.register", Resource: ResourceProject, Action: ActionRegister, CapabilityKind: CapabilityResource, Description: "Register projects", Enforcement: []string{"pkg/hub/handlers_projects_core.go"}},

	{ID: "skill.create", Resource: ResourceSkill, Action: ActionCreate, CapabilityKind: CapabilityScope, UATScope: "skill:create", Description: "Create skills", Enforcement: []string{"pkg/hub/skill_handlers.go"}},
	{ID: "skill.read", Resource: ResourceSkill, Action: ActionRead, CapabilityKind: CapabilityResource, UATScope: "skill:read", Description: "Read skills", Enforcement: []string{"pkg/hub/skill_handlers.go"}},
	{ID: "skill.update", Resource: ResourceSkill, Action: ActionUpdate, CapabilityKind: CapabilityResource, UATScope: "skill:update", Description: "Update skills", Enforcement: []string{"pkg/hub/skill_handlers.go"}},
	{ID: "skill.delete", Resource: ResourceSkill, Action: ActionDelete, CapabilityKind: CapabilityResource, UATScope: "skill:delete", Description: "Delete skills", Enforcement: []string{"pkg/hub/skill_handlers.go"}},
	{ID: "skill.list", Resource: ResourceSkill, Action: ActionList, CapabilityKind: CapabilityScope, UATScope: "skill:list", Description: "List skills", Enforcement: []string{"pkg/hub/skill_handlers.go"}},

	{ID: "template.create", Resource: ResourceTemplate, Action: ActionCreate, CapabilityKind: CapabilityScope, UATScope: "template:create", Description: "Create templates", Enforcement: []string{"pkg/hub/template_handlers.go"}},
	{ID: "template.read", Resource: ResourceTemplate, Action: ActionRead, CapabilityKind: CapabilityResource, UATScope: "template:read", Description: "Read templates", Enforcement: []string{"pkg/hub/template_handlers.go"}},
	{ID: "template.update", Resource: ResourceTemplate, Action: ActionUpdate, CapabilityKind: CapabilityResource, UATScope: "template:update", Description: "Update templates", Enforcement: []string{"pkg/hub/template_handlers.go"}},
	{ID: "template.delete", Resource: ResourceTemplate, Action: ActionDelete, CapabilityKind: CapabilityResource, UATScope: "template:delete", Description: "Delete templates", Enforcement: []string{"pkg/hub/template_handlers.go"}},
	{ID: "template.list", Resource: ResourceTemplate, Action: ActionList, CapabilityKind: CapabilityScope, UATScope: "template:list", Description: "List templates", Enforcement: []string{"pkg/hub/template_handlers.go"}},

	{ID: "harness_config.create", Resource: ResourceHarnessConfig, Action: ActionCreate, CapabilityKind: CapabilityScope, UATScope: "harness_config:create", Description: "Create harness configs", Enforcement: []string{"pkg/hub/harness_config_handlers.go"}},
	{ID: "harness_config.read", Resource: ResourceHarnessConfig, Action: ActionRead, CapabilityKind: CapabilityResource, UATScope: "harness_config:read", Description: "Read harness configs", Enforcement: []string{"pkg/hub/harness_config_handlers.go"}},
	{ID: "harness_config.update", Resource: ResourceHarnessConfig, Action: ActionUpdate, CapabilityKind: CapabilityResource, UATScope: "harness_config:update", Description: "Update harness configs", Enforcement: []string{"pkg/hub/harness_config_handlers.go"}},
	{ID: "harness_config.delete", Resource: ResourceHarnessConfig, Action: ActionDelete, CapabilityKind: CapabilityResource, UATScope: "harness_config:delete", Description: "Delete harness configs", Enforcement: []string{"pkg/hub/harness_config_handlers.go"}},
	{ID: "harness_config.list", Resource: ResourceHarnessConfig, Action: ActionList, CapabilityKind: CapabilityScope, UATScope: "harness_config:list", Description: "List harness configs", Enforcement: []string{"pkg/hub/harness_config_handlers.go"}},

	{ID: "group.create", Resource: ResourceGroup, Action: ActionCreate, CapabilityKind: CapabilityScope, UATScope: "group:create", Description: "Create groups", Enforcement: []string{"pkg/hub/handlers_groups.go"}},
	{ID: "group.read", Resource: ResourceGroup, Action: ActionRead, CapabilityKind: CapabilityResource, UATScope: "group:read", Description: "Read groups", Enforcement: []string{"pkg/hub/handlers_groups.go"}},
	{ID: "group.update", Resource: ResourceGroup, Action: ActionUpdate, CapabilityKind: CapabilityResource, UATScope: "group:update", Description: "Update groups", Enforcement: []string{"pkg/hub/handlers_groups.go"}},
	{ID: "group.delete", Resource: ResourceGroup, Action: ActionDelete, CapabilityKind: CapabilityResource, UATScope: "group:delete", Description: "Delete groups", Enforcement: []string{"pkg/hub/handlers_groups.go"}},
	{ID: "group.list", Resource: ResourceGroup, Action: ActionList, CapabilityKind: CapabilityScope, UATScope: "group:list", Description: "List groups", Enforcement: []string{"pkg/hub/handlers_groups.go"}},
	{ID: "group.addMember", Resource: ResourceGroup, Action: ActionAddMember, CapabilityKind: CapabilityResource, UATScope: "group:addMember", Description: "Add group members", Enforcement: []string{"pkg/hub/handlers_groups.go"}},
	{ID: "group.removeMember", Resource: ResourceGroup, Action: ActionRemoveMember, CapabilityKind: CapabilityResource, UATScope: "group:removeMember", Description: "Remove group members", Enforcement: []string{"pkg/hub/handlers_groups.go"}},

	{ID: "user.read", Resource: ResourceUser, Action: ActionRead, CapabilityKind: CapabilityResource, UATScope: "user:read", Description: "Read users", Enforcement: []string{"pkg/hub/handlers_users_core.go"}},
	{ID: "user.update", Resource: ResourceUser, Action: ActionUpdate, CapabilityKind: CapabilityResource, Description: "Update users", Enforcement: []string{"pkg/hub/handlers_users_core.go"}},

	{ID: "policy.create", Resource: ResourcePolicy, Action: ActionCreate, CapabilityKind: CapabilityScope, Description: "Create policies", Enforcement: []string{"pkg/hub/handlers_policies.go", "pkg/hub/route_metadata.go:requireAdmin"}},
	{ID: "policy.read", Resource: ResourcePolicy, Action: ActionRead, CapabilityKind: CapabilityResource, Description: "Read policies", Enforcement: []string{"pkg/hub/handlers_policies.go", "pkg/hub/route_metadata.go:requireAdmin"}},
	{ID: "policy.update", Resource: ResourcePolicy, Action: ActionUpdate, CapabilityKind: CapabilityResource, Description: "Update policies", Enforcement: []string{"pkg/hub/handlers_policies.go", "pkg/hub/route_metadata.go:requireAdmin"}},
	{ID: "policy.delete", Resource: ResourcePolicy, Action: ActionDelete, CapabilityKind: CapabilityResource, Description: "Delete policies", Enforcement: []string{"pkg/hub/handlers_policies.go", "pkg/hub/route_metadata.go:requireAdmin"}},
	{ID: "policy.list", Resource: ResourcePolicy, Action: ActionList, CapabilityKind: CapabilityScope, Description: "List policies", Enforcement: []string{"pkg/hub/handlers_policies.go", "pkg/hub/route_metadata.go:requireAdmin"}},

	{ID: "broker.create", Resource: ResourceBroker, Action: ActionCreate, CapabilityKind: CapabilityScope, Description: "Create brokers", Enforcement: []string{"pkg/hub/handlers_brokers.go"}},
	{ID: "broker.read", Resource: ResourceBroker, Action: ActionRead, CapabilityKind: CapabilityResource, UATScope: "broker:read", Description: "Read brokers", Enforcement: []string{"pkg/hub/handlers_brokers.go"}},
	{ID: "broker.update", Resource: ResourceBroker, Action: ActionUpdate, CapabilityKind: CapabilityResource, Description: "Update brokers", Enforcement: []string{"pkg/hub/handlers_brokers.go"}},
	{ID: "broker.delete", Resource: ResourceBroker, Action: ActionDelete, CapabilityKind: CapabilityResource, Description: "Delete brokers", Enforcement: []string{"pkg/hub/handlers_brokers.go"}},
	{ID: "broker.list", Resource: ResourceBroker, Action: ActionList, CapabilityKind: CapabilityScope, UATScope: "broker:list", Description: "List brokers", Enforcement: []string{"pkg/hub/handlers_brokers.go"}},
	{ID: "broker.dispatch", Resource: ResourceBroker, Action: ActionDispatch, CapabilityKind: CapabilityResource, Description: "Dispatch through brokers", Enforcement: []string{"pkg/hub/handlers_brokers.go"}},

	{ID: "gcp_service_account.create", Resource: ResourceGCPServiceAccount, Action: ActionCreate, CapabilityKind: CapabilityScope, Description: "Create GCP service accounts", Enforcement: []string{"pkg/hub/handlers_gcp_identity.go"}},
	{ID: "gcp_service_account.read", Resource: ResourceGCPServiceAccount, Action: ActionRead, CapabilityKind: CapabilityResource, UATScope: "gcp_service_account:read", Description: "Read GCP service accounts", Enforcement: []string{"pkg/hub/handlers_gcp_identity.go"}},
	{ID: "gcp_service_account.delete", Resource: ResourceGCPServiceAccount, Action: ActionDelete, CapabilityKind: CapabilityResource, Description: "Delete GCP service accounts", Enforcement: []string{"pkg/hub/handlers_gcp_identity.go"}},
	{ID: "gcp_service_account.list", Resource: ResourceGCPServiceAccount, Action: ActionList, CapabilityKind: CapabilityScope, UATScope: "gcp_service_account:list", Description: "List GCP service accounts", Enforcement: []string{"pkg/hub/handlers_gcp_identity.go"}},
	{ID: "gcp_service_account.verify", Resource: ResourceGCPServiceAccount, Action: ActionVerify, CapabilityKind: CapabilityResource, UATScope: "gcp_service_account:verify", Description: "Verify GCP service accounts", Enforcement: []string{"pkg/hub/handlers_gcp_identity.go"}},
	{ID: "gcp_service_account.mint", Resource: ResourceGCPServiceAccount, Action: ActionMint, CapabilityKind: CapabilityScope, Description: "Mint GCP service account tokens", Enforcement: []string{"pkg/hub/handlers_gcp_identity.go"}},
	{ID: "gcp_service_account.assign", Resource: ResourceGCPServiceAccount, Action: ActionAssign, CapabilityKind: CapabilityResource, UATScope: "gcp_service_account:assign", Description: "Assign GCP service accounts to agents", Enforcement: []string{"pkg/hub/handlers_gcp_identity.go", "pkg/hub/authz.go"}},

	// Hub resource type — hub-level administrative operations (Phase 2 D4 resolution)
	{ID: "hub.settings.read", Resource: ResourceHub, Action: ActionRead, CapabilityKind: CapabilityScope, Description: "Read hub settings", NonRouteUse: []string{"Phase 2 D4 route guard conversion"}},
	{ID: "hub.settings.update", Resource: ResourceHub, Action: ActionUpdate, CapabilityKind: CapabilityScope, Description: "Update hub settings", NonRouteUse: []string{"Phase 2 D4 route guard conversion"}},
	{ID: "hub.config.read", Resource: ResourceHub, Action: ActionRead, CapabilityKind: CapabilityScope, Description: "Read server configuration", NonRouteUse: []string{"Phase 2 D4 route guard conversion"}},
	{ID: "hub.config.update", Resource: ResourceHub, Action: ActionUpdate, CapabilityKind: CapabilityScope, Description: "Update server configuration", NonRouteUse: []string{"Phase 2 D4 route guard conversion"}},
	{ID: "hub.maintenance.execute", Resource: ResourceHub, Action: ActionExecute, CapabilityKind: CapabilityScope, Description: "Execute maintenance operations", NonRouteUse: []string{"Phase 2 D4 route guard conversion"}},
	{ID: "hub.diagnostics.read", Resource: ResourceHub, Action: ActionRead, CapabilityKind: CapabilityScope, Description: "Read diagnostics and logs", NonRouteUse: []string{"Phase 2 D4 route guard conversion"}},
	{ID: "hub.health.read", Resource: ResourceHub, Action: ActionRead, CapabilityKind: CapabilityScope, Description: "Read health summary", NonRouteUse: []string{"Phase 2 D4 route guard conversion"}},
	{ID: "hub.admin_mode.read", Resource: ResourceHub, Action: ActionRead, CapabilityKind: CapabilityScope, Description: "Read admin mode state", NonRouteUse: []string{"Phase 2 D4 route guard conversion"}},
	{ID: "hub.admin_mode.update", Resource: ResourceHub, Action: ActionUpdate, CapabilityKind: CapabilityScope, Description: "Update admin mode", NonRouteUse: []string{"Phase 2 D4 route guard conversion"}},
	{ID: "hub.integrations.read", Resource: ResourceHub, Action: ActionRead, CapabilityKind: CapabilityScope, Description: "Read integrations", NonRouteUse: []string{"Phase 2 D4 route guard conversion"}},
	{ID: "hub.integrations.update", Resource: ResourceHub, Action: ActionUpdate, CapabilityKind: CapabilityScope, Description: "Update integrations", NonRouteUse: []string{"Phase 2 D4 route guard conversion"}},
	{ID: "hub.lifecycle_hooks.read", Resource: ResourceHub, Action: ActionRead, CapabilityKind: CapabilityScope, Description: "Read lifecycle hooks", NonRouteUse: []string{"Phase 2 D4 route guard conversion"}},
	{ID: "hub.lifecycle_hooks.update", Resource: ResourceHub, Action: ActionUpdate, CapabilityKind: CapabilityScope, Description: "Update lifecycle hooks", NonRouteUse: []string{"Phase 2 D4 route guard conversion"}},
	{ID: "hub.allow_list.read", Resource: ResourceHub, Action: ActionRead, CapabilityKind: CapabilityScope, Description: "Read allow list", NonRouteUse: []string{"Phase 2 D4 route guard conversion"}},
	{ID: "hub.allow_list.update", Resource: ResourceHub, Action: ActionUpdate, CapabilityKind: CapabilityScope, Description: "Update allow list", NonRouteUse: []string{"Phase 2 D4 route guard conversion"}},
	{ID: "hub.project_defaults.read", Resource: ResourceHub, Action: ActionRead, CapabilityKind: CapabilityScope, Description: "Read project defaults", NonRouteUse: []string{"Phase 2 D4 route guard conversion"}},
	{ID: "hub.project_defaults.update", Resource: ResourceHub, Action: ActionUpdate, CapabilityKind: CapabilityScope, Description: "Update project defaults", NonRouteUse: []string{"Phase 2 D4 route guard conversion"}},
	{ID: "hub.messaging.update", Resource: ResourceHub, Action: ActionUpdate, CapabilityKind: CapabilityScope, Description: "Update messaging switches", Enforcement: []string{"pkg/hub/route_metadata.go:admin.messaging", "pkg/hub/admin_messaging.go:handleAdminMessaging"}},
	{ID: "hub.auth_reset.execute", Resource: ResourceHub, Action: ActionExecute, CapabilityKind: CapabilityScope, Description: "Reset all auth", NonRouteUse: []string{"Phase 2 D4 route guard conversion"}},
	{ID: "hub.scheduler.read", Resource: ResourceHub, Action: ActionRead, CapabilityKind: CapabilityScope, Description: "Read scheduler", NonRouteUse: []string{"Phase 2 D4 route guard conversion"}},
	{ID: "hub.scheduler.update", Resource: ResourceHub, Action: ActionUpdate, CapabilityKind: CapabilityScope, Description: "Update scheduler", NonRouteUse: []string{"Phase 2 D4 route guard conversion"}},
	{ID: "hub.federation.read", Resource: ResourceHub, Action: ActionRead, CapabilityKind: CapabilityScope, Description: "Read federation config", NonRouteUse: []string{"Phase 2 D4 route guard conversion"}},
	{ID: "hub.federation.update", Resource: ResourceHub, Action: ActionUpdate, CapabilityKind: CapabilityScope, Description: "Update federation config", NonRouteUse: []string{"Phase 2 D4 route guard conversion"}},
	{ID: "hub.teams_manifest.read", Resource: ResourceHub, Action: ActionRead, CapabilityKind: CapabilityScope, Description: "Read teams manifest", NonRouteUse: []string{"Phase 2 D4 route guard conversion"}},
	{ID: "hub.teams_manifest.update", Resource: ResourceHub, Action: ActionUpdate, CapabilityKind: CapabilityScope, Description: "Update teams manifest", NonRouteUse: []string{"Phase 2 D4 route guard conversion"}},
	{ID: "hub.validate.execute", Resource: ResourceHub, Action: ActionExecute, CapabilityKind: CapabilityScope, Description: "Validate resources", NonRouteUse: []string{"Phase 2 D4 route guard conversion"}},
	{ID: "hub.github_app.read", Resource: ResourceHub, Action: ActionRead, CapabilityKind: CapabilityScope, Description: "Read GitHub app configuration", NonRouteUse: []string{"Phase 2 D4 route guard conversion"}},
	{ID: "hub.github_app.update", Resource: ResourceHub, Action: ActionUpdate, CapabilityKind: CapabilityScope, Description: "Update GitHub app configuration", NonRouteUse: []string{"Phase 2 D4 route guard conversion"}},
	{ID: "hub.metrics.read", Resource: ResourceHub, Action: ActionRead, CapabilityKind: CapabilityScope, Description: "Read metrics dashboard", NonRouteUse: []string{"Phase 2 D4 route guard conversion"}},
	{ID: "hub.audit.read", Resource: ResourceHub, Action: ActionManage, CapabilityKind: CapabilityNone, Description: "Explain authorization decisions for other principals (super-admin only)", NonRouteUse: []string{"audit_authz.go explain-for-other-principal gate"}},

	// Quota management (Phase 2B — Limits/Quotas)
	{ID: "quota.read", Resource: ResourceQuota, Action: ActionRead, CapabilityKind: CapabilityScope, Description: "Read limit definitions, entitlements, and usage", Enforcement: []string{"pkg/hub/handlers_quota.go"}},
	{ID: "quota.create", Resource: ResourceQuota, Action: ActionCreate, CapabilityKind: CapabilityScope, Description: "Create limit definitions and entitlement bindings", Enforcement: []string{"pkg/hub/handlers_quota.go"}},
	{ID: "quota.update", Resource: ResourceQuota, Action: ActionUpdate, CapabilityKind: CapabilityScope, Description: "Update limit definitions and entitlement bindings", Enforcement: []string{"pkg/hub/handlers_quota.go"}},
	{ID: "quota.delete", Resource: ResourceQuota, Action: ActionDelete, CapabilityKind: CapabilityScope, Description: "Delete limit definitions and entitlement bindings", Enforcement: []string{"pkg/hub/handlers_quota.go"}},

	// Role management (Phase 2 PR-C1)
	{ID: "role.read", Resource: ResourceRole, Action: ActionRead, CapabilityKind: CapabilityScope, Description: "Read role definitions", Enforcement: []string{"pkg/hub/handlers_roles.go"}},
	{ID: "role.create", Resource: ResourceRole, Action: ActionCreate, CapabilityKind: CapabilityScope, Description: "Create custom role definitions", Enforcement: []string{"pkg/hub/handlers_roles.go"}},
	{ID: "role.update", Resource: ResourceRole, Action: ActionUpdate, CapabilityKind: CapabilityScope, Description: "Update custom role definitions", Enforcement: []string{"pkg/hub/handlers_roles.go"}},
	{ID: "role.delete", Resource: ResourceRole, Action: ActionDelete, CapabilityKind: CapabilityScope, Description: "Delete custom role definitions", Enforcement: []string{"pkg/hub/handlers_roles.go"}},
	{ID: "role_binding.read", Resource: ResourceRoleBinding, Action: ActionRead, CapabilityKind: CapabilityScope, Description: "Read role bindings", Enforcement: []string{"pkg/hub/handlers_roles.go"}},
	{ID: "role_binding.create", Resource: ResourceRoleBinding, Action: ActionCreate, CapabilityKind: CapabilityScope, Description: "Create role bindings", Enforcement: []string{"pkg/hub/handlers_roles.go"}},
	{ID: "role_binding.delete", Resource: ResourceRoleBinding, Action: ActionDelete, CapabilityKind: CapabilityScope, Description: "Delete role bindings", Enforcement: []string{"pkg/hub/handlers_roles.go"}},

	// Scheduled event / recurring schedule permissions (project-scoped)
	{ID: "scheduled_event.read", Resource: ResourceScheduledEvent, Action: ActionRead, CapabilityKind: CapabilityResource, Description: "Read a scheduled event", Enforcement: []string{"pkg/hub/handlers_scheduled_events.go", "pkg/hub/handlers_schedules.go"}},
	{ID: "scheduled_event.list", Resource: ResourceScheduledEvent, Action: ActionList, CapabilityKind: CapabilityScope, Description: "List scheduled events", Enforcement: []string{"pkg/hub/handlers_scheduled_events.go", "pkg/hub/handlers_schedules.go"}},
	{ID: "scheduled_event.create", Resource: ResourceScheduledEvent, Action: ActionCreate, CapabilityKind: CapabilityScope, Description: "Create a scheduled event", Enforcement: []string{"pkg/hub/handlers_scheduled_events.go", "pkg/hub/handlers_schedules.go"}},
	{ID: "scheduled_event.delete", Resource: ResourceScheduledEvent, Action: ActionDelete, CapabilityKind: CapabilityResource, Description: "Cancel a scheduled event or delete a schedule", Enforcement: []string{"pkg/hub/handlers_scheduled_events.go", "pkg/hub/handlers_schedules.go"}},
	{ID: "scheduled_event.update", Resource: ResourceScheduledEvent, Action: ActionUpdate, CapabilityKind: CapabilityResource, Description: "Update a recurring schedule", Enforcement: []string{"pkg/hub/handlers_schedules.go"}},

	// Extensions to existing resource types (Phase 2 D4 resolution)
	{ID: "user.invite", Resource: ResourceUser, Action: ActionInvite, CapabilityKind: CapabilityScope, UATScope: "user:invite", Description: "Invite users", NonRouteUse: []string{"Phase 2 D4 route guard conversion"}},
	{ID: "user.suspend", Resource: ResourceUser, Action: ActionSuspend, CapabilityKind: CapabilityResource, Description: "Suspend users", NonRouteUse: []string{"Phase 2 D4 route guard conversion"}},
	{ID: "user.promote", Resource: ResourceUser, Action: ActionPromote, CapabilityKind: CapabilityResource, Description: "Promote or demote users", NonRouteUse: []string{"Phase 2 D4 route guard conversion"}},
	{ID: "user.list", Resource: ResourceUser, Action: ActionList, CapabilityKind: CapabilityScope, UATScope: "user:list", Description: "List users", NonRouteUse: []string{"Phase 2 D4 route guard conversion"}},
	{ID: "project.clone", Resource: ResourceProject, Action: ActionClone, CapabilityKind: CapabilityResource, UATScope: "project:clone", Description: "Clone projects", NonRouteUse: []string{"Phase 2 D4 route guard conversion"}},
	{ID: "project.list", Resource: ResourceProject, Action: ActionList, CapabilityKind: CapabilityScope, Description: "List projects", NonRouteUse: []string{"Phase 2 D4 route guard conversion"}},
	{ID: "skill.register", Resource: ResourceSkill, Action: ActionRegister, CapabilityKind: CapabilityScope, UATScope: "skill:register", Description: "Register skills in registries", NonRouteUse: []string{"Phase 2 D4 route guard conversion"}},

	{ID: "agent.status_update", Resource: ResourceAgent, Action: "status_update", AgentScopes: []string{"agent:status:update"}, Description: "Update own agent status", NonRouteUse: []string{"agent token self-status endpoint"}},
	{ID: "agent.log_append", Resource: ResourceAgent, Action: "log_append", AgentScopes: []string{"agent:log:append"}, Description: "Append own agent logs", NonRouteUse: []string{"agent token log append endpoint"}},
	{ID: "project.secret_read", Resource: ResourceProject, Action: "secret_read", AgentScopes: []string{"project:secret:read"}, Description: "Read project secrets", NonRouteUse: []string{"agent secret/env resolution"}},
	{ID: "agent.notify", Resource: ResourceAgent, Action: "notify", AgentScopes: []string{"project:agent:notify"}, Description: "Manage own notification subscriptions", NonRouteUse: []string{"agent notification endpoints"}},
	{ID: "agent.token_refresh", Resource: ResourceAgent, Action: "token_refresh", AgentScopes: []string{"agent:token:refresh"}, Description: "Refresh own agent token", NonRouteUse: []string{"agent token refresh endpoint"}},
	{ID: "agent.port_forward", Resource: ResourceAgent, Action: "port_forward", AgentScopes: []string{"agent:port:forward"}, Description: "Register and hold forwarded ports", NonRouteUse: []string{"agent port tunnel endpoints"}},
	{ID: "agent.identity_token", Resource: ResourceAgent, Action: "identity_token", AgentScopes: []string{"agent:identity:token"}, Description: "Request OIDC identity tokens", NonRouteUse: []string{"agent identity token endpoint"}},
}

// ResourceActions returns item-level capability actions keyed by resource type.
func ResourceActions() map[string][]string {
	return actionsByKind(CapabilityResource)
}

// ScopeActions returns collection/scope-level capability actions keyed by resource type.
func ScopeActions() map[string][]string {
	return actionsByKind(CapabilityScope)
}

func actionsByKind(kind CapabilityKind) map[string][]string {
	out := map[string][]string{}
	for _, permission := range Registry {
		if permission.CapabilityKind != kind {
			continue
		}
		out[permission.Resource] = append(out[permission.Resource], permission.Action)
	}
	return out
}

// UATValidScopes returns the set of scopes valid for newly-created UATs,
// including aliases.
func UATValidScopes() map[string]bool {
	out := make(map[string]bool)
	for alias := range UATManageAliases {
		out[alias] = true
	}
	for _, permission := range Registry {
		if permission.UATScope != "" {
			out[permission.UATScope] = true
		}
	}
	return out
}

// UATManageScopes returns the concrete scopes expanded from agent:manage.
func UATManageScopes() []string {
	return UATManageScopesFor(ResourceAgent)
}

// UATManageScopesFor returns the concrete scopes expanded from a manage alias
// for the given resource type.
func UATManageScopesFor(resource string) []string {
	scopes := uatScopesForResource(resource)
	sort.Strings(scopes)
	return scopes
}

// UATScopeOptions returns UAT scopes with display metadata for CLI/UI surfaces.
func UATScopeOptions(includeAliases bool) []Permission {
	var out []Permission
	for _, permission := range Registry {
		if permission.UATScope != "" {
			out = append(out, permission)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].UATScope < out[j].UATScope
	})
	if includeAliases {
		// Sort alias scopes for stable output order.
		aliases := make([]string, 0, len(UATManageAliases))
		for alias := range UATManageAliases {
			aliases = append(aliases, alias)
		}
		sort.Strings(aliases)
		for _, alias := range aliases {
			resource := UATManageAliases[alias]
			out = append(out, Permission{
				ID:          resource + ".manage",
				Resource:    resource,
				Action:      "manage",
				UATScope:    alias,
				Description: fmt.Sprintf("All %s scopes (convenience alias)", resource),
				NonRouteUse: []string{"UAT scope expansion alias"},
			})
		}
	}
	return out
}

// UATScopeHelp formats the canonical UAT scope list for command help text.
func UATScopeHelp() string {
	options := UATScopeOptions(true)
	width := 0
	for _, option := range options {
		if len(option.UATScope) > width {
			width = len(option.UATScope)
		}
	}
	lines := make([]string, 0, len(options))
	for _, option := range options {
		lines = append(lines, fmt.Sprintf("  %-*s  %s", width, option.UATScope, option.Description))
	}
	return strings.Join(lines, "\n")
}

func uatScopesForResource(resource string) []string {
	var out []string
	for _, permission := range Registry {
		if permission.Resource == resource && permission.UATScope != "" {
			out = append(out, permission.UATScope)
		}
	}
	return out
}
