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

	ActionCreate       = "create"
	ActionRead         = "read"
	ActionUpdate       = "update"
	ActionDelete       = "delete"
	ActionList         = "list"
	ActionManage       = "manage"
	ActionAttach       = "attach"
	ActionPortAccess   = "port_access"
	ActionRegister     = "register"
	ActionAddMember    = "addMember"
	ActionRemoveMember = "removeMember"
	ActionDispatch     = "dispatch"
	ActionStopAll      = "stop_all"
	ActionVerify       = "verify"
	ActionMint         = "mint"
	ActionAssign       = "assign"

	UATScopeAgentManage = "agent:manage"
)

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

	{ID: "project.create", Resource: ResourceProject, Action: ActionCreate, CapabilityKind: CapabilityScope, Description: "Create projects", Enforcement: []string{"pkg/hub/handlers_projects_core.go"}},
	{ID: "project.read", Resource: ResourceProject, Action: ActionRead, CapabilityKind: CapabilityResource, UATScope: "project:read", AgentScopes: []string{"project:read"}, Description: "Read project metadata", Enforcement: []string{"pkg/hub/handlers_projects_core.go", "pkg/hub/authz.go"}},
	{ID: "project.update", Resource: ResourceProject, Action: ActionUpdate, CapabilityKind: CapabilityResource, UATScope: "project:update", Description: "Update projects", Enforcement: []string{"pkg/hub/handlers_projects_core.go", "pkg/hub/authz.go"}},
	{ID: "project.delete", Resource: ResourceProject, Action: ActionDelete, CapabilityKind: CapabilityResource, Description: "Delete projects", Enforcement: []string{"pkg/hub/handlers_projects_core.go"}},
	{ID: "project.manage", Resource: ResourceProject, Action: ActionManage, CapabilityKind: CapabilityResource, Description: "Manage project administration", Enforcement: []string{"pkg/hub/handlers_projects_core.go"}},
	{ID: "project.register", Resource: ResourceProject, Action: ActionRegister, CapabilityKind: CapabilityResource, Description: "Register projects", Enforcement: []string{"pkg/hub/handlers_projects_core.go"}},

	{ID: "skill.create", Resource: ResourceSkill, Action: ActionCreate, CapabilityKind: CapabilityScope, Description: "Create skills", Enforcement: []string{"pkg/hub/skill_handlers.go"}},
	{ID: "skill.read", Resource: ResourceSkill, Action: ActionRead, CapabilityKind: CapabilityResource, Description: "Read skills", Enforcement: []string{"pkg/hub/skill_handlers.go"}},
	{ID: "skill.update", Resource: ResourceSkill, Action: ActionUpdate, CapabilityKind: CapabilityResource, Description: "Update skills", Enforcement: []string{"pkg/hub/skill_handlers.go"}},
	{ID: "skill.delete", Resource: ResourceSkill, Action: ActionDelete, CapabilityKind: CapabilityResource, Description: "Delete skills", Enforcement: []string{"pkg/hub/skill_handlers.go"}},
	{ID: "skill.list", Resource: ResourceSkill, Action: ActionList, CapabilityKind: CapabilityScope, Description: "List skills", Enforcement: []string{"pkg/hub/skill_handlers.go"}},

	{ID: "template.create", Resource: ResourceTemplate, Action: ActionCreate, CapabilityKind: CapabilityScope, Description: "Create templates", Enforcement: []string{"pkg/hub/template_handlers.go"}},
	{ID: "template.read", Resource: ResourceTemplate, Action: ActionRead, CapabilityKind: CapabilityResource, Description: "Read templates", Enforcement: []string{"pkg/hub/template_handlers.go"}},
	{ID: "template.update", Resource: ResourceTemplate, Action: ActionUpdate, CapabilityKind: CapabilityResource, Description: "Update templates", Enforcement: []string{"pkg/hub/template_handlers.go"}},
	{ID: "template.delete", Resource: ResourceTemplate, Action: ActionDelete, CapabilityKind: CapabilityResource, Description: "Delete templates", Enforcement: []string{"pkg/hub/template_handlers.go"}},
	{ID: "template.list", Resource: ResourceTemplate, Action: ActionList, CapabilityKind: CapabilityScope, Description: "List templates", Enforcement: []string{"pkg/hub/template_handlers.go"}},

	{ID: "harness_config.create", Resource: ResourceHarnessConfig, Action: ActionCreate, CapabilityKind: CapabilityScope, Description: "Create harness configs", Enforcement: []string{"pkg/hub/harness_config_handlers.go"}},
	{ID: "harness_config.read", Resource: ResourceHarnessConfig, Action: ActionRead, CapabilityKind: CapabilityResource, Description: "Read harness configs", Enforcement: []string{"pkg/hub/harness_config_handlers.go"}},
	{ID: "harness_config.update", Resource: ResourceHarnessConfig, Action: ActionUpdate, CapabilityKind: CapabilityResource, Description: "Update harness configs", Enforcement: []string{"pkg/hub/harness_config_handlers.go"}},
	{ID: "harness_config.delete", Resource: ResourceHarnessConfig, Action: ActionDelete, CapabilityKind: CapabilityResource, Description: "Delete harness configs", Enforcement: []string{"pkg/hub/harness_config_handlers.go"}},
	{ID: "harness_config.list", Resource: ResourceHarnessConfig, Action: ActionList, CapabilityKind: CapabilityScope, Description: "List harness configs", Enforcement: []string{"pkg/hub/harness_config_handlers.go"}},

	{ID: "group.create", Resource: ResourceGroup, Action: ActionCreate, CapabilityKind: CapabilityScope, Description: "Create groups", Enforcement: []string{"pkg/hub/handlers_groups.go"}},
	{ID: "group.read", Resource: ResourceGroup, Action: ActionRead, CapabilityKind: CapabilityResource, Description: "Read groups", Enforcement: []string{"pkg/hub/handlers_groups.go"}},
	{ID: "group.update", Resource: ResourceGroup, Action: ActionUpdate, CapabilityKind: CapabilityResource, Description: "Update groups", Enforcement: []string{"pkg/hub/handlers_groups.go"}},
	{ID: "group.delete", Resource: ResourceGroup, Action: ActionDelete, CapabilityKind: CapabilityResource, Description: "Delete groups", Enforcement: []string{"pkg/hub/handlers_groups.go"}},
	{ID: "group.list", Resource: ResourceGroup, Action: ActionList, CapabilityKind: CapabilityScope, Description: "List groups", Enforcement: []string{"pkg/hub/handlers_groups.go"}},
	{ID: "group.addMember", Resource: ResourceGroup, Action: ActionAddMember, CapabilityKind: CapabilityResource, Description: "Add group members", Enforcement: []string{"pkg/hub/handlers_groups.go"}},
	{ID: "group.removeMember", Resource: ResourceGroup, Action: ActionRemoveMember, CapabilityKind: CapabilityResource, Description: "Remove group members", Enforcement: []string{"pkg/hub/handlers_groups.go"}},

	{ID: "user.read", Resource: ResourceUser, Action: ActionRead, CapabilityKind: CapabilityResource, Description: "Read users", Enforcement: []string{"pkg/hub/handlers_users_core.go"}},
	{ID: "user.update", Resource: ResourceUser, Action: ActionUpdate, CapabilityKind: CapabilityResource, Description: "Update users", Enforcement: []string{"pkg/hub/handlers_users_core.go"}},

	{ID: "policy.create", Resource: ResourcePolicy, Action: ActionCreate, CapabilityKind: CapabilityScope, Description: "Create policies", Enforcement: []string{"pkg/hub/handlers_policies.go", "pkg/hub/route_metadata.go:requireAdmin"}},
	{ID: "policy.read", Resource: ResourcePolicy, Action: ActionRead, CapabilityKind: CapabilityResource, Description: "Read policies", Enforcement: []string{"pkg/hub/handlers_policies.go", "pkg/hub/route_metadata.go:requireAdmin"}},
	{ID: "policy.update", Resource: ResourcePolicy, Action: ActionUpdate, CapabilityKind: CapabilityResource, Description: "Update policies", Enforcement: []string{"pkg/hub/handlers_policies.go", "pkg/hub/route_metadata.go:requireAdmin"}},
	{ID: "policy.delete", Resource: ResourcePolicy, Action: ActionDelete, CapabilityKind: CapabilityResource, Description: "Delete policies", Enforcement: []string{"pkg/hub/handlers_policies.go", "pkg/hub/route_metadata.go:requireAdmin"}},
	{ID: "policy.list", Resource: ResourcePolicy, Action: ActionList, CapabilityKind: CapabilityScope, Description: "List policies", Enforcement: []string{"pkg/hub/handlers_policies.go", "pkg/hub/route_metadata.go:requireAdmin"}},

	{ID: "broker.create", Resource: ResourceBroker, Action: ActionCreate, CapabilityKind: CapabilityScope, Description: "Create brokers", Enforcement: []string{"pkg/hub/handlers_brokers.go"}},
	{ID: "broker.read", Resource: ResourceBroker, Action: ActionRead, CapabilityKind: CapabilityResource, Description: "Read brokers", Enforcement: []string{"pkg/hub/handlers_brokers.go"}},
	{ID: "broker.update", Resource: ResourceBroker, Action: ActionUpdate, CapabilityKind: CapabilityResource, Description: "Update brokers", Enforcement: []string{"pkg/hub/handlers_brokers.go"}},
	{ID: "broker.delete", Resource: ResourceBroker, Action: ActionDelete, CapabilityKind: CapabilityResource, Description: "Delete brokers", Enforcement: []string{"pkg/hub/handlers_brokers.go"}},
	{ID: "broker.list", Resource: ResourceBroker, Action: ActionList, CapabilityKind: CapabilityScope, Description: "List brokers", Enforcement: []string{"pkg/hub/handlers_brokers.go"}},
	{ID: "broker.dispatch", Resource: ResourceBroker, Action: ActionDispatch, CapabilityKind: CapabilityResource, Description: "Dispatch through brokers", Enforcement: []string{"pkg/hub/handlers_brokers.go"}},

	{ID: "gcp_service_account.create", Resource: ResourceGCPServiceAccount, Action: ActionCreate, CapabilityKind: CapabilityScope, Description: "Create GCP service accounts", Enforcement: []string{"pkg/hub/handlers_gcp_identity.go"}},
	{ID: "gcp_service_account.read", Resource: ResourceGCPServiceAccount, Action: ActionRead, CapabilityKind: CapabilityResource, Description: "Read GCP service accounts", Enforcement: []string{"pkg/hub/handlers_gcp_identity.go"}},
	{ID: "gcp_service_account.delete", Resource: ResourceGCPServiceAccount, Action: ActionDelete, CapabilityKind: CapabilityResource, Description: "Delete GCP service accounts", Enforcement: []string{"pkg/hub/handlers_gcp_identity.go"}},
	{ID: "gcp_service_account.list", Resource: ResourceGCPServiceAccount, Action: ActionList, CapabilityKind: CapabilityScope, Description: "List GCP service accounts", Enforcement: []string{"pkg/hub/handlers_gcp_identity.go"}},
	{ID: "gcp_service_account.verify", Resource: ResourceGCPServiceAccount, Action: ActionVerify, CapabilityKind: CapabilityResource, Description: "Verify GCP service accounts", Enforcement: []string{"pkg/hub/handlers_gcp_identity.go"}},
	{ID: "gcp_service_account.mint", Resource: ResourceGCPServiceAccount, Action: ActionMint, CapabilityKind: CapabilityScope, Description: "Mint GCP service account tokens", Enforcement: []string{"pkg/hub/handlers_gcp_identity.go"}},
	{ID: "gcp_service_account.assign", Resource: ResourceGCPServiceAccount, Action: ActionAssign, CapabilityKind: CapabilityResource, Description: "Assign GCP service accounts to agents", Enforcement: []string{"pkg/hub/handlers_gcp_identity.go", "pkg/hub/authz.go"}},

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
	out := map[string]bool{UATScopeAgentManage: true}
	for _, permission := range Registry {
		if permission.UATScope != "" {
			out[permission.UATScope] = true
		}
	}
	return out
}

// UATManageScopes returns the concrete scopes expanded from agent:manage.
func UATManageScopes() []string {
	scopes := uatScopesForResource(ResourceAgent)
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
		out = append(out, Permission{
			ID:          "agent.manage",
			Resource:    ResourceAgent,
			Action:      "manage",
			UATScope:    UATScopeAgentManage,
			Description: "All agent scopes (convenience alias)",
			NonRouteUse: []string{"UAT scope expansion alias"},
		})
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
