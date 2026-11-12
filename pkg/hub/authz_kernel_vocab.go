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

import "github.com/GoogleCloudPlatform/scion/pkg/store"

// Action represents an authorization action.
type Action string

// Action constants for authorization checks.
const (
	ActionCreate     Action = "create"
	ActionRead       Action = "read"
	ActionUpdate     Action = "update"
	ActionDelete     Action = "delete"
	ActionList       Action = "list"
	ActionManage     Action = "manage"
	ActionStart      Action = "start"
	ActionStop       Action = "stop"
	ActionMessage    Action = "message"
	ActionAttach     Action = "attach"
	ActionLifecycle  Action = "lifecycle"
	ActionPortAccess Action = "port_access"
	// ActionTunnel gates the Conduit `scion tunnel` / `scion ssh` request
	// path (not a stream kind). It is not a registered permission of its
	// own: it is granted wherever agent.port_access is (see
	// conduitAuthzFor), so every role, relationship and token scope that
	// carries port access carries it.
	ActionTunnel       Action = "tunnel"
	ActionRegister     Action = "register"
	ActionAddMember    Action = "addMember"
	ActionRemoveMember Action = "removeMember"
	ActionDispatch     Action = "dispatch"
	ActionStopAll      Action = "stop_all"
	ActionVerify       Action = "verify"
	ActionMint         Action = "mint"
	// ActionAssign covers binding a resource to a principal that will act with
	// it — currently attaching a GCP service account to an agent.
	ActionAssign         Action = "assign"
	ActionInvite         Action = "invite"
	ActionSuspend        Action = "suspend"
	ActionPromote        Action = "promote"
	ActionClone          Action = "clone"
	ActionExecute        Action = "execute"
	ActionSetMessageMode Action = "set_message_mode"
	// ActionSetMessagingPolicy covers changing a project's cross-project
	// inbound messaging policy (project.set_messaging_policy).
	ActionSetMessagingPolicy Action = "set_messaging_policy"
	// ActionManageVisitors covers granting, changing and revoking project
	// visitor access for federated accounts (project.manage_visitors).
	ActionManageVisitors Action = "manage_visitors"
	// ActionChatPost covers posting messages into a project's chat
	// threads (project.chat_post).
	ActionChatPost Action = "chat_post"
	// ActionAdmin covers access constraint administration
	// (access_constraint.admin).
	ActionAdmin Action = "admin"

	// Global-catalog write actions — see design doc §3.1.
	// These distinguish hub-catalog mutation from project-scoped CRUD,
	// so a system-scoped binding carrying only these IDs does not
	// silently grant project-level authority.
	ActionCreateGlobal Action = "create_global"
	ActionUpdateGlobal Action = "update_global"
	ActionDeleteGlobal Action = "delete_global"

	// ActionAutoProvide gates turning on a broker's auto-provide setting
	// (broker.auto_provide).
	ActionAutoProvide Action = "auto_provide"

	// ActionDeliver and ActionUse distinguish launch-time material delivery
	// from an agent's own runtime retrieval or token-mint request. Neither
	// is listed in isReadOnlyOperation: a material decision always runs the
	// delegation ceiling.
	ActionDeliver Action = "deliver"
	ActionUse     Action = "use"

	// ActionWrite covers changes to the holder's own self-scoped records
	// (permissions.ActionWrite).
	ActionWrite Action = "write"

	// ActionIssue covers issuing hub test identities
	// (permissions.ActionIssue, test_identity.issue).
	ActionIssue Action = "issue"
)

// Resource represents the target of an authorization check.
type Resource struct {
	Type       string            // e.g. "agent", "project", "policy", "group"
	ID         string            // Resource ID
	OwnerID    string            // Owner user ID
	ParentType string            // e.g. "project" for an agent
	ParentID   string            // Parent resource ID
	Labels     map[string]string // Resource labels for condition matching
	Ancestry   []string          // Ordered ancestor chain [root, ..., parent] for transitive access

	// ScopeKind is the resource's own scope classification for resource
	// types whose records are themselves partitioned by scope: "skill"
	// (store.SkillScopeGlobal/Core/Project/User), "template"
	// (store.TemplateScopeGlobal/Project/User), and "harness_config"
	// (store.HarnessConfigScopeGlobal/Project/User). It is distinct from
	// ParentType/ParentID, which describe project containment for the
	// kernel's project-scoped binding check. ScopeKind instead lets a
	// resource-type-specific check (see filterHubWideSkillGrants,
	// filterHubWideTemplateGrants, filterHubWideHarnessConfigGrants) tell a
	// genuinely hub-scoped record apart from a user- or project-scoped one
	// that merely happens to have no ParentType set. Left empty for resource
	// types that don't need it. For "skill", "template", and
	// "harness_config" resources specifically, build this through the
	// resource's canonical constructor (skillResource/skillScopeResource,
	// templateResource/templateScopeResource,
	// harnessConfigResource/harnessConfigScopeResource) rather than a
	// hand-built literal: each filter fails closed on an empty or
	// unrecognized ScopeKind (ptone/scion#1901 finding F4; ptone/scion#1916
	// applies the same rule to template and harness_config), so only those
	// constructors are guaranteed to set it correctly.
	ScopeKind string

	// ScopeUserID is the owning user of a user-scoped skill
	// (store.Skill.ScopeID when ScopeKind is store.SkillScopeUser), set only
	// by skillScopeResource/skillResource. It lets the personal-skill progeny
	// grant (skillProgenyAdapter, authz_skill_progeny.go) match the same
	// column the skill list predicate filters on. Empty otherwise.
	ScopeUserID string

	// launchedTarget is the agent record the single-agent GET handlers
	// read from the store, set only by agentStatusReadResource. The
	// launcher status-read rule (authz_launcher_read.go) decides from it
	// alone. It is unexported so no decoded request can carry it.
	launchedTarget *store.Agent

	// deliveryItem names the stored material record a deliver request is
	// for, set only by materialDeliveryResource (material_resources.go).
	// The association rules (authz_material_association.go) re-read the
	// record it names from the store and decide from that record and the
	// delivery credential's stored agent. It is unexported so no decoded
	// request can carry it.
	deliveryItem *materialDeliveryItem
}

// PrincipalKind describes the authenticated actor evaluated by an authorization request.
type PrincipalKind string

const (
	PrincipalKindUser             PrincipalKind = "user"
	PrincipalKindAgent            PrincipalKind = "agent"
	PrincipalKindFederatedUser    PrincipalKind = "federated_user"
	PrincipalKindFederatedAgent   PrincipalKind = "federated_agent"
	PrincipalKindFederatedService PrincipalKind = "federated_service"
	PrincipalKindBroker           PrincipalKind = "broker"
	PrincipalKindDev              PrincipalKind = "dev"
	// PrincipalKindAgentDelegated classifies a request made with an agent
	// delegated credential (.design/agent-delegation.md §12.1): the actor is
	// the agent the grant is bound to, and the grant issuer is the
	// authorizing user. Only *DelegatedAgentIdentity classifies to it.
	PrincipalKindAgentDelegated PrincipalKind = "agent_delegated"
	// PrincipalKindVisitor classifies a *VisitorIdentity: a user of a
	// trusted external issuer linked to an active local federated account.
	// Its ID is that account's local user ID, and NormalizePrincipalType
	// maps it to the store principal type "user" for binding lookups.
	PrincipalKindVisitor PrincipalKind = "visitor"
)

// PrincipalContext identifies the authenticated actor for an authorization request.
// Identity remains available during the migration so existing policy evaluation can
// use the established identity interfaces.
type PrincipalContext struct {
	Kind     PrincipalKind
	ID       string
	Identity Identity
}
