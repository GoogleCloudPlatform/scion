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
	"slices"
	"sort"
	"strings"
	"sync"

	"github.com/GoogleCloudPlatform/scion/pkg/credentialmeta"
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
	ResourceAccessConstraint  = "access_constraint"
	// ResourceSecret, ResourceEnvVar and ResourceSkillInjection are the
	// material-delivery resource types (ptone/scion#2129): the typed target
	// of an agent secret, stored environment variable or stored skill
	// reference, as distinct from ResourceProject/ResourceSkill.
	ResourceSecret         = "secret"
	ResourceEnvVar         = "env_var"
	ResourceSkillInjection = "skill_injection"
	// ResourceArtifact is the artifact service's resource type
	// (pkg/artifacts). Artifact permissions are checked in the hub's
	// artifacts.Host adapter against the artifact's home project.
	ResourceArtifact = "artifact"

	// ResourceInbox and ResourceUserSkillInjection are self-scoped resource
	// types: their records belong to one user and carry no project or hub
	// target that a role binding could authorize. See TargetClassKindSelf.
	ResourceInbox              = "inbox"
	ResourceUserSkillInjection = "user_skill_injection"

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
	ActionLifecycle      = "lifecycle"
	ActionCreateGlobal   = "create_global"
	// ActionDeliver and ActionUse distinguish launch-time material delivery
	// from an agent's own runtime retrieval or token-mint request over the
	// same or a related resource (ptone/scion#2129). Neither is a read-only
	// action.
	ActionDeliver = "deliver"
	ActionUse     = "use"
	// ActionWrite covers creating, changing and removing the holder's own
	// self-scoped records.
	ActionWrite = "write"

	// PermissionGCPServiceAccountUse is the gcp_service_account.use
	// permission ID. Named so pkg/hub/authz.go's agent-scope handling for
	// this one permission keys off a constant rather than a literal string.
	// The Registry row below keeps its ID as the literal string, not this
	// constant: pkg/hub/authzop/catalog_test.go reads registry.go as text
	// and takes the first quoted string on each line starting with "{ID:"
	// as that row's ID, so an identifier there would make it read the
	// row's Description as the ID. TestMaterialPermissions_Registered pins
	// the constant against the row.
	PermissionGCPServiceAccountUse = "gcp_service_account.use"

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
	// ExcludeFromManageAlias keeps this permission's UAT scope out of the
	// resource's "<resource>:manage" convenience alias. Used for observation
	// permissions (agent.attach, agent.port_access) that some project roles
	// do not hold (owners/admins lack attach; members lack
	// port_access), so that holders of those roles can still mint
	// agent:manage tokens. The scope remains available for explicit selection.
	ExcludeFromManageAlias bool
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
	{ID: "agent.attach", Resource: ResourceAgent, Action: ActionAttach, CapabilityKind: CapabilityResource, UATScope: "agent:attach", AgentScopes: []string{"project:agent:lifecycle"}, Description: "Attach to agent sessions (terminal, exec, env, reset-auth)", Enforcement: []string{"pkg/hub/authorize.go:authorizeAgentLifecycle", "pkg/hub/pty_handlers.go", "pkg/hub/authorize_agentkeys.go:authorizeAgentKeys"}, ExcludeFromManageAlias: true},
	{ID: "agent.lifecycle", Resource: ResourceAgent, Action: ActionLifecycle, CapabilityKind: CapabilityResource, UATScope: "agent:lifecycle", AgentScopes: []string{"project:agent:lifecycle"}, Description: "Start, stop, suspend, restart, restore, and reincarnate agents", Enforcement: []string{"pkg/hub/authorize.go:authorizeAgentLifecycle", "pkg/hub/handlers_agents_core.go:handleAgentAction", "pkg/hub/handlers_agent_reincarnate.go:authorizeAgentReincarnate"}},
	{ID: "agent.port_access", Resource: ResourceAgent, Action: ActionPortAccess, CapabilityKind: CapabilityResource, UATScope: "agent:port_access", Description: "Access agent forwarded ports", Enforcement: []string{"pkg/hub/port_forward_handlers.go"}, ExcludeFromManageAlias: true},
	{ID: "agent.stop_all", Resource: ResourceAgent, Action: ActionStopAll, CapabilityKind: CapabilityScope, Description: "Stop all agents", Enforcement: []string{"pkg/hub/handlers_agents_core.go"}},
	{ID: "agent.message", Resource: ResourceAgent, Action: ActionMessage, CapabilityKind: CapabilityScope, UATScope: "agent:message", Description: "Send messages to agents", NonRouteUse: []string{"Phase 2: pkg/hub/authorize.go:authorizeAgentMessage"}},
	{ID: "agent.set_message_mode", Resource: ResourceAgent, Action: ActionSetMessageMode, CapabilityKind: CapabilityResource, AgentScopes: []string{"project:agent:set_message_mode"}, Description: "Change agent message mode", Enforcement: []string{"pkg/hub/handlers_agents_core.go"}},
	{ID: "agent.grant_hub_mode", Resource: ResourceAgent, Action: "grant_hub_mode", CapabilityKind: CapabilityResource, Description: "Grant hub message mode (requires full role + hub mode for agent callers)", Enforcement: []string{"pkg/hub/authorize_message_mode_grant.go"}},

	{ID: "project.create", Resource: ResourceProject, Action: ActionCreate, CapabilityKind: CapabilityScope, Description: "Create projects", Enforcement: []string{"pkg/hub/handlers_projects_core.go"}},
	{ID: "project.read", Resource: ResourceProject, Action: ActionRead, CapabilityKind: CapabilityResource, UATScope: "project:read", AgentScopes: []string{"project:read"}, Description: "Read project metadata", Enforcement: []string{"pkg/hub/handlers_projects_core.go", "pkg/hub/authz.go"}},
	{ID: "project.update", Resource: ResourceProject, Action: ActionUpdate, CapabilityKind: CapabilityResource, UATScope: "project:update", Description: "Update projects", Enforcement: []string{"pkg/hub/handlers_projects_core.go", "pkg/hub/authz.go"}},
	{ID: "project.delete", Resource: ResourceProject, Action: ActionDelete, CapabilityKind: CapabilityResource, Description: "Delete projects", Enforcement: []string{"pkg/hub/handlers_projects_core.go"}},
	{ID: "project.manage", Resource: ResourceProject, Action: ActionManage, CapabilityKind: CapabilityResource, UATScope: "project:manage", Description: "Manage project administration (RS1 membership operations)", Enforcement: []string{"pkg/hub/handlers_projects_core.go"}},
	{ID: "project.register", Resource: ResourceProject, Action: ActionRegister, CapabilityKind: CapabilityResource, Description: "Register projects", Enforcement: []string{"pkg/hub/handlers_projects_core.go"}},
	{ID: "project.set_messaging_policy", Resource: ResourceProject, Action: "set_messaging_policy", CapabilityKind: CapabilityResource, Description: "Set project cross-project messaging policy (owner/admin only)", Enforcement: []string{"pkg/hub/project_messaging_policy.go"}},

	{ID: "artifact.read", Resource: ResourceArtifact, Action: ActionRead, CapabilityKind: CapabilityResource, UATScope: "artifact:read", AgentScopes: []string{"project:artifact:read"}, Description: "Read artifacts", Enforcement: []string{"pkg/hub/artifacts_host.go:func (h *artifactHost) Authorize"}},
	{ID: "artifact.create", Resource: ResourceArtifact, Action: ActionCreate, CapabilityKind: CapabilityScope, UATScope: "artifact:create", AgentScopes: []string{"project:artifact:write"}, Description: "Publish artifacts", Enforcement: []string{"pkg/hub/artifacts_host.go:func (h *artifactHost) Authorize"}},
	{ID: "artifact.update", Resource: ResourceArtifact, Action: ActionUpdate, CapabilityKind: CapabilityResource, UATScope: "artifact:update", AgentScopes: []string{"project:artifact:write"}, Description: "Publish new versions of artifacts", Enforcement: []string{"pkg/hub/artifacts_host.go:func (h *artifactHost) Authorize"}},
	{ID: "artifact.delete", Resource: ResourceArtifact, Action: ActionDelete, CapabilityKind: CapabilityResource, UATScope: "artifact:delete", Description: "Delete artifacts", Enforcement: []string{"pkg/hub/artifacts_host.go:func (h *artifactHost) Authorize"}},
	{ID: "artifact.manage", Resource: ResourceArtifact, Action: ActionManage, CapabilityKind: CapabilityResource, UATScope: "artifact:manage", Description: "Manage artifact grants and share links", Enforcement: []string{"pkg/hub/artifacts_host.go:func (h *artifactHost) Authorize"}},

	{ID: "skill.create", Resource: ResourceSkill, Action: ActionCreate, CapabilityKind: CapabilityScope, UATScope: "skill:create", Description: "Create skills", Enforcement: []string{"pkg/hub/skill_handlers.go"}},
	{ID: "skill.create_global", Resource: ResourceSkill, Action: ActionCreateGlobal, CapabilityKind: CapabilityScope, Description: "Create skills in the global (hub) catalog", Enforcement: []string{"pkg/hub/skill_handlers.go"}},
	{ID: "skill.read", Resource: ResourceSkill, Action: ActionRead, CapabilityKind: CapabilityResource, UATScope: "skill:read", AgentScopes: []string{"project:read"}, Description: "Read skills", Enforcement: []string{"pkg/hub/skill_handlers.go"}},
	{ID: "skill.update", Resource: ResourceSkill, Action: ActionUpdate, CapabilityKind: CapabilityResource, UATScope: "skill:update", Description: "Update skills", Enforcement: []string{"pkg/hub/skill_handlers.go"}},
	{ID: "skill.delete", Resource: ResourceSkill, Action: ActionDelete, CapabilityKind: CapabilityResource, UATScope: "skill:delete", Description: "Delete skills", Enforcement: []string{"pkg/hub/skill_handlers.go"}},
	{ID: "skill.list", Resource: ResourceSkill, Action: ActionList, CapabilityKind: CapabilityScope, UATScope: "skill:list", AgentScopes: []string{"project:read"}, Description: "List skills", Enforcement: []string{"pkg/hub/skill_handlers.go"}},

	{ID: "template.create", Resource: ResourceTemplate, Action: ActionCreate, CapabilityKind: CapabilityScope, UATScope: "template:create", AgentScopes: []string{"project:template:write"}, Description: "Create templates", Enforcement: []string{"pkg/hub/template_handlers.go"}},
	{ID: "template.read", Resource: ResourceTemplate, Action: ActionRead, CapabilityKind: CapabilityResource, UATScope: "template:read", AgentScopes: []string{"project:read"}, Description: "Read templates", Enforcement: []string{"pkg/hub/template_handlers.go"}},
	{ID: "template.update", Resource: ResourceTemplate, Action: ActionUpdate, CapabilityKind: CapabilityResource, UATScope: "template:update", AgentScopes: []string{"project:template:write"}, Description: "Update templates", Enforcement: []string{"pkg/hub/template_handlers.go"}},
	{ID: "template.delete", Resource: ResourceTemplate, Action: ActionDelete, CapabilityKind: CapabilityResource, UATScope: "template:delete", Description: "Delete templates", Enforcement: []string{"pkg/hub/template_handlers.go"}},
	{ID: "template.list", Resource: ResourceTemplate, Action: ActionList, CapabilityKind: CapabilityScope, UATScope: "template:list", AgentScopes: []string{"project:read"}, Description: "List templates", Enforcement: []string{"pkg/hub/template_handlers.go"}},

	{ID: "harness_config.create", Resource: ResourceHarnessConfig, Action: ActionCreate, CapabilityKind: CapabilityScope, UATScope: "harness_config:create", Description: "Create harness configs", Enforcement: []string{"pkg/hub/harness_config_handlers.go"}},
	{ID: "harness_config.read", Resource: ResourceHarnessConfig, Action: ActionRead, CapabilityKind: CapabilityResource, UATScope: "harness_config:read", AgentScopes: []string{"project:read"}, Description: "Read harness configs", Enforcement: []string{"pkg/hub/harness_config_handlers.go"}},
	{ID: "harness_config.update", Resource: ResourceHarnessConfig, Action: ActionUpdate, CapabilityKind: CapabilityResource, UATScope: "harness_config:update", Description: "Update harness configs", Enforcement: []string{"pkg/hub/harness_config_handlers.go"}},
	{ID: "harness_config.delete", Resource: ResourceHarnessConfig, Action: ActionDelete, CapabilityKind: CapabilityResource, UATScope: "harness_config:delete", Description: "Delete harness configs", Enforcement: []string{"pkg/hub/harness_config_handlers.go"}},
	{ID: "harness_config.list", Resource: ResourceHarnessConfig, Action: ActionList, CapabilityKind: CapabilityScope, UATScope: "harness_config:list", AgentScopes: []string{"project:read"}, Description: "List harness configs", Enforcement: []string{"pkg/hub/harness_config_handlers.go"}},

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

	// broker.create is a hub-level permission: registration is gated by an
	// explicit hub-member role grant (seed.go hubMemberPermissionIDs), not by
	// mere authentication. Its UAT selector "broker:create" is mintable only
	// on a hub-boundary token (PermissionAllowedBoundaries). Broker creation
	// does not admit bearer credentials (authorizeBrokerCreate).
	{ID: "broker.create", Resource: ResourceBroker, Action: ActionCreate, CapabilityKind: CapabilityScope, UATScope: "broker:create", Description: "Create brokers", Enforcement: []string{"pkg/hub/handlers_brokers.go:authorizeBrokerCreate", "pkg/hub/handlers_projects_core.go"}},
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
	// AgentScopes names project:agent:sa_assign only, its own agent scope
	// distinct from project:agent:create (ptone/scion#2339): a ceiling that
	// covers agent.create does not thereby cover this permission, and vice
	// versa. authz.go's effectiveAgentScopes adds project:agent:sa_assign for
	// a verified agent JWT signed before the split that holds
	// project:agent:create, so such a token keeps authorizing this
	// permission.
	{ID: "gcp_service_account.assign", Resource: ResourceGCPServiceAccount, Action: ActionAssign, CapabilityKind: CapabilityResource, UATScope: "gcp_service_account:assign", AgentScopes: []string{"project:agent:sa_assign"}, Description: "Assign GCP service accounts to agents", Enforcement: []string{"pkg/hub/handlers_gcp_identity.go", "pkg/hub/authz.go"}},

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
	{ID: "hub.experiments.update", Resource: ResourceHub, Action: ActionUpdate, CapabilityKind: CapabilityScope, Description: "Read and update hub-wide experiment overrides", Enforcement: []string{"pkg/hub/route_metadata.go:admin.experiments", "pkg/hub/admin_experiments.go:handleAdminExperiments"}},
	{ID: "hub.auth_reset.execute", Resource: ResourceHub, Action: ActionExecute, CapabilityKind: CapabilityScope, Description: "Reset all auth", NonRouteUse: []string{"Phase 2 D4 route guard conversion"}},
	{ID: "hub.scheduler.read", Resource: ResourceHub, Action: ActionRead, CapabilityKind: CapabilityScope, Description: "Read scheduler", NonRouteUse: []string{"Phase 2 D4 route guard conversion"}},
	{ID: "hub.scheduler.update", Resource: ResourceHub, Action: ActionUpdate, CapabilityKind: CapabilityScope, Description: "Update scheduler", NonRouteUse: []string{"Phase 2 D4 route guard conversion"}},
	{ID: "hub.federation.read", Resource: ResourceHub, Action: ActionRead, CapabilityKind: CapabilityScope, Description: "Read federation config", NonRouteUse: []string{"Phase 2 D4 route guard conversion"}},
	{ID: "hub.federation.update", Resource: ResourceHub, Action: ActionUpdate, CapabilityKind: CapabilityScope, Description: "Update federation config", NonRouteUse: []string{"Phase 2 D4 route guard conversion"}},
	{ID: "hub.teams_manifest.read", Resource: ResourceHub, Action: ActionRead, CapabilityKind: CapabilityScope, Description: "Read teams manifest", NonRouteUse: []string{"Phase 2 D4 route guard conversion"}},
	{ID: "hub.teams_manifest.update", Resource: ResourceHub, Action: ActionUpdate, CapabilityKind: CapabilityScope, Description: "Update teams manifest", NonRouteUse: []string{"Phase 2 D4 route guard conversion"}},
	{ID: "hub.validate.execute", Resource: ResourceHub, Action: ActionExecute, CapabilityKind: CapabilityScope, Description: "Validate resources", NonRouteUse: []string{"Phase 2 D4 route guard conversion"}},
	{ID: "hub.github_app.read", Resource: ResourceHub, Action: ActionRead, CapabilityKind: CapabilityScope, Description: "Read GitHub app configuration", Enforcement: []string{"pkg/hub/route_metadata.go"}},
	{ID: "hub.github_app.update", Resource: ResourceHub, Action: ActionUpdate, CapabilityKind: CapabilityScope, Description: "Update GitHub app configuration", Enforcement: []string{"pkg/hub/route_metadata.go"}},
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

	// Access constraint management (AC1 — Operator Access Constraint Backend)
	{ID: "access_constraint.admin", Resource: ResourceAccessConstraint, Action: ActionManage, CapabilityKind: CapabilityScope, Description: "Administer access constraints (create, update, delete)", Enforcement: []string{"pkg/hub/handlers_access_constraints.go"}},
	{ID: "access_constraint.read", Resource: ResourceAccessConstraint, Action: ActionRead, CapabilityKind: CapabilityScope, Description: "Read access constraints", Enforcement: []string{"pkg/hub/handlers_access_constraints.go"}},

	// Scheduled event / recurring schedule permissions (project-scoped)
	{ID: "scheduled_event.read", Resource: ResourceScheduledEvent, Action: ActionRead, CapabilityKind: CapabilityResource, Description: "Read a scheduled event", Enforcement: []string{"pkg/hub/handlers_scheduled_events.go", "pkg/hub/handlers_schedules.go"}},
	{ID: "scheduled_event.list", Resource: ResourceScheduledEvent, Action: ActionList, CapabilityKind: CapabilityScope, Description: "List scheduled events", Enforcement: []string{"pkg/hub/handlers_scheduled_events.go", "pkg/hub/handlers_schedules.go"}},
	{ID: "scheduled_event.create", Resource: ResourceScheduledEvent, Action: ActionCreate, CapabilityKind: CapabilityScope, Description: "Create a scheduled event", Enforcement: []string{"pkg/hub/handlers_scheduled_events.go", "pkg/hub/handlers_schedules.go"}},
	{ID: "scheduled_event.delete", Resource: ResourceScheduledEvent, Action: ActionDelete, CapabilityKind: CapabilityResource, Description: "Cancel a scheduled event or delete a schedule", Enforcement: []string{"pkg/hub/handlers_scheduled_events.go", "pkg/hub/handlers_schedules.go"}},
	{ID: "scheduled_event.update", Resource: ResourceScheduledEvent, Action: ActionUpdate, CapabilityKind: CapabilityResource, Description: "Update a recurring schedule", Enforcement: []string{"pkg/hub/handlers_schedules.go"}},

	// Extensions to existing resource types (Phase 2 D4 resolution)
	{ID: "user.invite", Resource: ResourceUser, Action: ActionInvite, CapabilityKind: CapabilityScope, UATScope: "user:invite", Description: "Invite users", NonRouteUse: []string{"Phase 2 D4 route guard conversion"}},
	{ID: "user.suspend", Resource: ResourceUser, Action: ActionSuspend, CapabilityKind: CapabilityResource, Description: "Suspend users", Enforcement: []string{"pkg/hub/handlers_users_core.go"}},
	{ID: "user.promote", Resource: ResourceUser, Action: ActionPromote, CapabilityKind: CapabilityResource, Description: "Promote or demote users", Enforcement: []string{"pkg/hub/handlers_users_core.go"}},
	{ID: "user.delete", Resource: ResourceUser, Action: ActionDelete, CapabilityKind: CapabilityResource, Description: "Delete users", Enforcement: []string{"pkg/hub/handlers_users_core.go"}},
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

	// Material delivery and runtime-use permissions (ptone/scion#2129).
	// *.deliver governs launch-time delivery of a secret, stored environment
	// variable or stored skill reference to a target agent. It carries no
	// AgentScopes and is intended for the internal hub-delivery credential
	// only (ptone/scion#2228), never an agent JWT. Holding *.deliver through a
	// role never substitutes for the association, progeny or skill-default
	// grant required for the selected item.
	// secret.use governs an agent's own runtime retrieval and is admitted
	// under an agent JWT via the AgentScopes mapping below. gcp_service_account.use
	// keeps AgentScopes nil: the GCP token scope names one service account
	// instance (project:gcp:token:<sa-id>), so its agent-credential admission
	// is decided per resource instead of from this permission's static
	// scope list. See pkg/hub/authz.go's agent-scope restriction and its
	// request-local synthetic grant, both keyed on PermissionGCPServiceAccountUse.
	{ID: "secret.deliver", Resource: ResourceSecret, Action: ActionDeliver, Description: "Deliver a secret to an agent at launch", NonRouteUse: []string{"material delivery grant evaluation"}},
	{ID: "env_var.deliver", Resource: ResourceEnvVar, Action: ActionDeliver, Description: "Deliver a stored environment variable to an agent at launch", NonRouteUse: []string{"material delivery grant evaluation"}},
	{ID: "skill_injection.deliver", Resource: ResourceSkillInjection, Action: ActionDeliver, Description: "Deliver a stored skill reference to an agent at launch", NonRouteUse: []string{"material delivery grant evaluation"}},
	{ID: "secret.use", Resource: ResourceSecret, Action: ActionUse, AgentScopes: []string{"project:secret:read"}, Description: "Retrieve a secret value at runtime by key", Enforcement: []string{"pkg/hub/material_runtime.go"}},
	{ID: "gcp_service_account.use", Resource: ResourceGCPServiceAccount, Action: ActionUse, Description: "Mint a token as an assigned GCP service account", NonRouteUse: []string{"GCP token mint request"}},

	// Self-scoped permissions act only on the holder's own records. A user
	// access token needs the exact selector, and the target checks in
	// Server.authorizeSelfScoped apply. Mint eligibility is "the issuer is
	// an active user"; see permissions.IsSelfPermission.
	{ID: "inbox.read", Resource: ResourceInbox, Action: ActionRead, UATScope: "inbox:read", Description: "Read your own inbox, notifications and direct messages", NonRouteUse: []string{"pkg/hub/authorize.go:authorizeSelfScoped"}},
	{ID: "inbox.write", Resource: ResourceInbox, Action: ActionWrite, UATScope: "inbox:write", Description: "Send, change and remove your own inbox items and direct messages", NonRouteUse: []string{"pkg/hub/authorize.go:authorizeSelfScoped"}},
	{ID: "user_skill_injection.update", Resource: ResourceUserSkillInjection, Action: ActionUpdate, UATScope: "user_skill_injection:update", Description: "Change the skills injected into your own agents", NonRouteUse: []string{"pkg/hub/authorize.go:authorizeSelfScoped"}},
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
		if permission.Resource == resource && permission.UATScope != "" && !permission.ExcludeFromManageAlias {
			out = append(out, permission.UATScope)
		}
	}
	return out
}

// BoundaryKind identifies the credential-side boundary a UAT is issued
// under: confined to one project, or spanning the hub (including
// cross-project use, subject to the holder's live authority on each
// resolved target — see pkg/hub/authz_boundary.go). The dependency-neutral
// credentialmeta package owns the canonical enum so permissions and audit
// metadata cannot drift. pkg/hub aliases this type through permissions.
type BoundaryKind = credentialmeta.BoundaryKind

const (
	BoundaryKindProject = credentialmeta.BoundaryProject
	BoundaryKindHub     = credentialmeta.BoundaryHub
)

// ValidBoundary is defined in project_applicability.go (shared with
// pkg/store, which cannot import pkg/hub).

// SelectorMapping is the resolved, explicit mapping from one published
// token selector (an ordinary UAT scope or a manage alias such as
// "agent:manage") to the canonical permission ID(s) it carries and the
// boundary kinds it may be issued under. PermissionIDs has exactly one
// entry for an ordinary selector and one entry per expanded scope for a
// manage alias. AllowedBoundaries is the intersection of
// SelectorAllowedBoundaries (project_applicability.go) across every ID in
// PermissionIDs — a separate, independently hand-reviewed table, not
// derived from Permission.Resource/Action.
//
// This table is derived from the existing Permission.UATScope field and
// UATManageAliases/UATManageScopesFor, not a second hand-maintained
// selector vocabulary: a resource:action reconstruction would silently
// collapse two permissions sharing a resource/action pair (e.g.
// hub.settings.read and hub.config.read, both {hub, read}) into one
// selector once either becomes UAT-selectable. Mint resolves selectors
// through this table, and runtime authorization and delegation enforce the
// ceiling persisted from that resolution.
type SelectorMapping struct {
	Selector          string
	PermissionIDs     []string
	AllowedBoundaries []BoundaryKind
}

var (
	selectorRegistry     map[string]SelectorMapping
	selectorRegistryOnce sync.Once
)

// buildSelectorRegistry constructs the selector table from Registry and
// UATManageAliases. A selector is only added to the table when every
// expanded permission ID has a reviewed SelectorAllowedBoundaries entry and
// the intersection of those boundary sets is non-empty; a selector missing
// that review, or whose reviewed boundaries never agree, is simply absent
// from the map (ResolveSelector then reports ok=false) rather than silently
// getting an empty or partially-reviewed AllowedBoundaries. It is
// deterministic and side-effect free; callers must not mutate the returned
// map (ResolveSelector caches it).
func buildSelectorRegistry() map[string]SelectorMapping {
	type pending struct {
		selector string
		ids      []string
	}
	var candidates []pending

	for _, p := range Registry {
		if p.UATScope == "" {
			continue
		}
		candidates = append(candidates, pending{selector: p.UATScope, ids: []string{p.ID}})
	}

	// Manage aliases expand to their concrete scopes via the existing
	// UATManageScopesFor, which already excludes ExcludeFromManageAlias
	// scopes (e.g. agent:attach, agent:port_access stay outside
	// agent:manage).
	byScope := make(map[string][]string, len(candidates))
	for _, c := range candidates {
		byScope[c.selector] = c.ids
	}
	aliases := make([]string, 0, len(UATManageAliases))
	for alias := range UATManageAliases {
		aliases = append(aliases, alias)
	}
	sort.Strings(aliases)
	for _, alias := range aliases {
		resource := UATManageAliases[alias]
		var ids []string
		for _, scope := range UATManageScopesFor(resource) {
			ids = append(ids, byScope[scope]...)
		}
		if len(ids) == 0 {
			continue
		}
		candidates = append(candidates, pending{selector: alias, ids: ids})
	}

	out := make(map[string]SelectorMapping, len(candidates))
	for _, c := range candidates {
		boundaries, ok := intersectAllowedBoundaries(c.ids)
		if !ok || len(boundaries) == 0 {
			// Unreviewed permission ID, or the reviewed boundary sets for
			// this alias's members never agree: fail closed by omitting
			// the selector entirely rather than guessing.
			continue
		}
		out[c.selector] = SelectorMapping{
			Selector:          c.selector,
			PermissionIDs:     c.ids,
			AllowedBoundaries: boundaries,
		}
	}
	return out
}

// intersectAllowedBoundaries returns the intersection of
// SelectorAllowedBoundaries across every ID in ids. ok is false if any ID
// lacks a reviewed entry. An empty ids yields no boundaries and ok=false:
// there is nothing to intersect, so the function has no basis for allowing
// any boundary.
func intersectAllowedBoundaries(ids []string) (boundaries []BoundaryKind, ok bool) {
	if len(ids) == 0 {
		return nil, false
	}
	counts := make(map[BoundaryKind]int)
	for _, id := range ids {
		kinds, reviewed := SelectorAllowedBoundaries(id)
		if !reviewed {
			return nil, false
		}
		for _, k := range kinds {
			counts[k]++
		}
	}
	for _, k := range []BoundaryKind{BoundaryKindProject, BoundaryKindHub} {
		if counts[k] == len(ids) {
			boundaries = append(boundaries, k)
		}
	}
	return boundaries, true
}

// ResolveSelector is the single lookup point from a published token selector
// to its canonical permission ID(s) and allowed boundaries. Unknown,
// unmapped, unreviewed, or (defensively) empty-after-expansion selectors
// return ok=false — callers must fail closed rather than reconstruct a
// selector from resource/action.
//
// ResolveSelector returns a COPY of the derived mapping: PermissionIDs and
// AllowedBoundaries are cloned so a caller mutating the returned slices
// cannot corrupt the process-wide cached selectorRegistry.
func ResolveSelector(selector string) (SelectorMapping, bool) {
	selectorRegistryOnce.Do(func() {
		selectorRegistry = buildSelectorRegistry()
	})
	m, ok := selectorRegistry[selector]
	if !ok || len(m.PermissionIDs) == 0 {
		return SelectorMapping{}, false
	}
	m.PermissionIDs = slices.Clone(m.PermissionIDs)
	m.AllowedBoundaries = slices.Clone(m.AllowedBoundaries)
	return m, true
}

// ValidateSelectorRegistry compares the live, derived selector table against
// a caller-supplied pinned expected snapshot (selector -> sorted permission
// IDs). This is the actual "every selector maps explicitly" enforcement:
// pure derivation from Permission.UATScope is safe only because UATScope
// itself is a hand-authored literal per Registry entry (never derived from
// Resource/Action), and this pin forces a human to update the test's
// expected table — an explicit review act — whenever a Registry change
// adds, removes, or retargets a UATScope or alias member. It also re-checks
// the no-duplicate-UATScope invariant directly against Registry, independent
// of the pinned snapshot.
func ValidateSelectorRegistry(expected map[string][]string) error {
	seenScopes := make(map[string]string) // UATScope -> Permission.ID
	for _, p := range Registry {
		if p.UATScope == "" {
			continue
		}
		if owner, dup := seenScopes[p.UATScope]; dup {
			return fmt.Errorf("duplicate UATScope %q claimed by both %q and %q", p.UATScope, owner, p.ID)
		}
		seenScopes[p.UATScope] = p.ID
	}

	selectorRegistryOnce.Do(func() {
		selectorRegistry = buildSelectorRegistry()
	})

	if len(selectorRegistry) != len(expected) {
		return fmt.Errorf("selector registry has %d entries, expected snapshot has %d", len(selectorRegistry), len(expected))
	}
	for selector, wantIDs := range expected {
		m, ok := selectorRegistry[selector]
		if !ok {
			return fmt.Errorf("expected selector %q not present in derived registry", selector)
		}
		gotIDs := append([]string(nil), m.PermissionIDs...)
		sort.Strings(gotIDs)
		wantSorted := append([]string(nil), wantIDs...)
		sort.Strings(wantSorted)
		if len(gotIDs) != len(wantSorted) {
			return fmt.Errorf("selector %q resolved to %v, want %v", selector, gotIDs, wantSorted)
		}
		for i := range gotIDs {
			if gotIDs[i] != wantSorted[i] {
				return fmt.Errorf("selector %q resolved to %v, want %v", selector, gotIDs, wantSorted)
			}
		}
	}
	for selector := range selectorRegistry {
		if _, ok := expected[selector]; !ok {
			return fmt.Errorf("derived registry has unexpected selector %q not present in pinned snapshot", selector)
		}
	}
	return nil
}

// MintEligibilityKind names one way a permission can become eligible for
// selection when minting a UAT restriction.
type MintEligibilityKind string

const (
	// MintEligibilityFlatRole is the existing rule: the principal's current
	// role grant includes this permission as a flat subset. Unchanged by
	// A.1.
	MintEligibilityFlatRole MintEligibilityKind = "flat_role"
	// MintEligibilityRelationship means a relationship rule of a declared
	// type (see MintEligibilitySource.RelationshipTypes) could justify this
	// permission for this principal kind — not that a specific target
	// already exists or has been checked.
	MintEligibilityRelationship MintEligibilityKind = "relationship"
)

// MintEligibilitySource is one way a selector can become mintable. A
// permission may declare more than one source; Sources are OR'd — any one
// satisfied source makes the selector eligible for selection.
type MintEligibilitySource struct {
	Kind MintEligibilityKind
	// RelationshipTypes names candidate relationship rule types (e.g.
	// "owner", "ancestor"), matching the RelationshipType values B.1's
	// resolver evaluates. Set only when Kind == MintEligibilityRelationship.
	RelationshipTypes []string
}

// MintEligibilityDescriptor is what a UAT mint path and CLI/UI eligibility
// display consume for one permission. It answers "may this authenticated
// user select this restriction for this boundary," never "does a target
// already exist" and never "is this a grant" — every later request against
// a specific target still goes through full request-time authorization.
//
// It does NOT carry its own AllowedBoundaries or ActionAllowlist: those
// would duplicate PermissionAllowedBoundaries/RelationshipPolicy — callers
// derive boundaries via SelectorAllowedBoundaries(PermissionID) and derive
// the action limit for a
// MintEligibilityRelationship source via RelationshipPolicyAllows against
// RelationshipPolicies (relationship_policy.go), the ONE shared authoring
// surface B.1's runtime relationship-grant evaluator also reads — so there
// is never a second, drifting allowlist.
type MintEligibilityDescriptor struct {
	PermissionID string
	Sources      []MintEligibilitySource
	// RequiresExistingTarget is false for resource-relative permissions
	// such as agent.attach/agent.port_access: a member may mint the
	// restriction before creating their first agent. The token gains no
	// grant either way — every later request is independently authorized.
	RequiresExistingTarget bool
}

// MintEligibilityRegistry declares mint eligibility for resource-relative
// permissions reviewed for #2092 (creator/ancestor attach and port access).
// Scope-wide/durable-authority permissions are not listed here; they keep
// the existing flat role-permission-subset mint rule by default (callers
// treat a PermissionID absent from this map as MintEligibilityFlatRole
// only). Every MintEligibilityRelationship source's RelationshipTypes must
// resolve to at least one MintEligible==true RelationshipPolicies row with a
// matching ResourceType/PermissionID (consistency test in
// relationship_policy_test.go).
var MintEligibilityRegistry = map[string]MintEligibilityDescriptor{
	"agent.attach": {
		PermissionID: "agent.attach",
		Sources: []MintEligibilitySource{
			{Kind: MintEligibilityRelationship, RelationshipTypes: []string{"owner", "ancestor"}},
		},
		RequiresExistingTarget: false,
	},
	"agent.port_access": {
		PermissionID: "agent.port_access",
		Sources: []MintEligibilitySource{
			{Kind: MintEligibilityRelationship, RelationshipTypes: []string{"owner", "ancestor"}},
		},
		RequiresExistingTarget: false,
	},
}
