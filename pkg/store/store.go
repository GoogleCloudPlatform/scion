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

package store

import (
	"context"
	"errors"
	"time"
)

// Common errors returned by store implementations.
var (
	ErrNotFound        = errors.New("not found")
	ErrAlreadyExists   = errors.New("already exists")
	ErrVersionConflict = errors.New("version conflict")
	ErrInvalidInput    = errors.New("invalid input")
)

// Store defines the interface for Hub data persistence.
// Implementations may use SQLite, PostgreSQL, Firestore, or other backends.
type Store interface {
	// Close releases any resources held by the store.
	Close() error

	// Ping checks connectivity to the underlying database.
	Ping(ctx context.Context) error

	// Migrate applies any pending database migrations.
	Migrate(ctx context.Context) error

	// Agent operations
	AgentStore

	// Project operations
	ProjectStore

	// RuntimeBroker operations
	RuntimeBrokerStore

	// Template operations
	TemplateStore

	// HarnessConfig operations
	HarnessConfigStore

	// User operations
	UserStore

	// ProjectProvider operations
	ProjectProviderStore

	// EnvVar operations
	EnvVarStore

	// Secret operations
	SecretStore

	// Group operations (Hub Permissions System)
	GroupStore

	// Policy operations (Hub Permissions System)
	PolicyStore

	// User Access Token operations
	UserAccessTokenStore

	// Broker Secret operations (Runtime Broker authentication)
	BrokerSecretStore

	// Notification operations (Agent Status Notification System)
	NotificationStore

	// ScheduledEvent operations (One-Shot Timers)
	ScheduledEventStore

	// Schedule operations (Recurring Schedules)
	ScheduleStore

	// GCP Service Account operations (GCP Identity for Agents)
	GCPServiceAccountStore

	// GitHub App Installation operations
	GitHubInstallationStore

	// Message operations (Bidirectional Human-Agent Messaging)
	MessageStore

	// Maintenance operations (Admin Maintenance Panel)
	MaintenanceStore

	// Project Sync State operations (Workspace Sync Metadata)
	ProjectSyncStateStore

	// Allow List operations (User Access Control)
	AllowListStore

	// Invite Code operations (User Invitation System)
	InviteCodeStore

}

// AgentStore defines agent-related persistence operations.
type AgentStore interface {
	// CreateAgent creates a new agent record.
	// Returns ErrAlreadyExists if an agent with the same ID exists.
	CreateAgent(ctx context.Context, agent *Agent) error

	// GetAgent retrieves an agent by ID.
	// Returns ErrNotFound if the agent doesn't exist.
	GetAgent(ctx context.Context, id string) (*Agent, error)

	// GetAgentBySlug retrieves an agent by its slug within a project.
	// Returns ErrNotFound if the agent doesn't exist.
	GetAgentBySlug(ctx context.Context, projectID, slug string) (*Agent, error)

	// UpdateAgent updates an existing agent.
	// Uses optimistic locking via StateVersion.
	// Returns ErrNotFound if agent doesn't exist.
	// Returns ErrVersionConflict if the version doesn't match.
	UpdateAgent(ctx context.Context, agent *Agent) error

	// DeleteAgent removes an agent by ID.
	// Returns ErrNotFound if the agent doesn't exist.
	DeleteAgent(ctx context.Context, id string) error

	// ListAgents returns agents matching the filter criteria.
	ListAgents(ctx context.Context, filter AgentFilter, opts ListOptions) (*ListResult[Agent], error)

	// UpdateAgentStatus updates only status-related fields.
	// This is a partial update that doesn't require version checking.
	UpdateAgentStatus(ctx context.Context, id string, status AgentStatusUpdate) error

	// PurgeDeletedAgents permanently removes soft-deleted agents older than cutoff.
	// Returns the number of agents purged.
	PurgeDeletedAgents(ctx context.Context, cutoff time.Time) (int, error)

	// MarkStaleAgentsOffline marks agents whose last heartbeat is older
	// than threshold as offline. Only affects agents with phase=running whose
	// activity is not a terminal sticky state (completed, limits_exceeded).
	// Returns the updated agent records for event publishing.
	MarkStaleAgentsOffline(ctx context.Context, threshold time.Time) ([]Agent, error)

	// MarkStalledAgents marks running agents as stalled when their last activity
	// event is older than activityThreshold but they have a recent heartbeat
	// (last_seen >= heartbeatRecency). Only affects agents with phase=running
	// whose activity is not a terminal sticky state or already stalled/offline.
	// Returns the updated agent records for event publishing.
	MarkStalledAgents(ctx context.Context, activityThreshold, heartbeatRecency time.Time) ([]Agent, error)
}

// AgentFilter defines criteria for filtering agents.
type AgentFilter struct {
	ProjectID       string
	RuntimeBrokerID string
	Phase           string
	OwnerID         string
	IncludeDeleted  bool // If true, include soft-deleted agents in results

	// MemberOrOwnerProjectIDs, when non-empty, restricts results to agents
	// whose project_id is in this set OR whose owner_id matches OwnerID.
	// OwnerID and this field are combined with OR (not AND) when both are set.
	MemberOrOwnerProjectIDs []string

	// MemberProjectIDs, when non-empty, restricts results to agents whose
	// project_id is in this set. Unlike MemberOrOwnerProjectIDs, this is NOT
	// combined with OwnerID — it filters strictly by project ID membership.
	MemberProjectIDs []string

	// ExcludeOwnerID, when non-empty, excludes agents whose owner_id matches
	// this value. Used with MemberProjectIDs to return "shared" agents (in a
	// member project but not personally created).
	ExcludeOwnerID string

	// AncestorID, when non-empty, restricts results to agents whose ancestry
	// chain contains the given principal ID (transitive access via creation lineage).
	AncestorID string
}

// AgentStatusUpdate contains fields for status-only updates.
type AgentStatusUpdate struct {
	Phase           string            `json:"phase,omitempty"`
	Activity        string            `json:"activity,omitempty"`
	ToolName        string            `json:"toolName,omitempty"`
	Message         string            `json:"message,omitempty"`
	ConnectionState string            `json:"connectionState,omitempty"`
	ContainerStatus string            `json:"containerStatus,omitempty"`
	RuntimeState    string            `json:"runtimeState,omitempty"`
	TaskSummary     string            `json:"taskSummary,omitempty"`
	Heartbeat       bool              `json:"heartbeat,omitempty"`
	Metadata        map[string]string `json:"metadata,omitempty"`

	// Limits tracking (reported by sciontool)
	CurrentTurns      *int   `json:"currentTurns,omitempty"`
	CurrentModelCalls *int   `json:"currentModelCalls,omitempty"`
	StartedAt         string `json:"startedAt,omitempty"`

	// Exit tracking
	ExitCode *int `json:"exitCode,omitempty"`
}

// ProjectStore defines project-related persistence operations.
type ProjectStore interface {
	// CreateProject creates a new project record.
	// Returns ErrAlreadyExists if a project with the same slug exists.
	CreateProject(ctx context.Context, project *Project) error

	// GetProject retrieves a project by ID.
	// Returns ErrNotFound if the project doesn't exist.
	GetProject(ctx context.Context, id string) (*Project, error)

	// GetProjectBySlug retrieves a project by its slug.
	// Returns ErrNotFound if the project doesn't exist.
	GetProjectBySlug(ctx context.Context, slug string) (*Project, error)

	// GetProjectBySlugCaseInsensitive retrieves a project by its slug, ignoring case.
	// This is useful for matching projects without git remotes (like global projects).
	// Returns ErrNotFound if the project doesn't exist.
	GetProjectBySlugCaseInsensitive(ctx context.Context, slug string) (*Project, error)

	// GetProjectsByGitRemote returns all projects matching the normalized git remote URL.
	// Returns an empty slice (not error) if none found.
	GetProjectsByGitRemote(ctx context.Context, gitRemote string) ([]*Project, error)

	// NextAvailableSlug returns the next available slug given a base slug.
	// If baseSlug is available, it is returned as-is. Otherwise, serial
	// suffixes are tried: baseSlug-1, baseSlug-2, etc.
	NextAvailableSlug(ctx context.Context, baseSlug string) (string, error)

	// UpdateProject updates an existing project.
	// Returns ErrNotFound if the project doesn't exist.
	UpdateProject(ctx context.Context, project *Project) error

	// DeleteProject removes a project by ID.
	// Returns ErrNotFound if the project doesn't exist.
	DeleteProject(ctx context.Context, id string) error

	// ListProjects returns projects matching the filter criteria.
	ListProjects(ctx context.Context, filter ProjectFilter, opts ListOptions) (*ListResult[Project], error)
}

// ProjectFilter defines criteria for filtering projects.
type ProjectFilter struct {
	OwnerID         string
	Visibility      string
	GitRemotePrefix string
	GitRemote       string // Filter by exact git remote (case-sensitive)
	BrokerID        string // Filter by contributing broker
	Name            string // Filter by exact name (case-insensitive)
	Slug            string // Filter by exact slug (case-insensitive)

	// MemberOrOwnerIDs, when non-empty, restricts results to projects whose ID
	// is in this set OR whose owner_id matches OwnerID. OwnerID and this
	// field are combined with OR (not AND) when both are set.
	MemberOrOwnerIDs []string

	// MemberProjectIDs, when non-empty, restricts results to projects whose ID
	// is in this set. Unlike MemberOrOwnerIDs, this is NOT combined with
	// OwnerID — it filters strictly by project ID membership.
	MemberProjectIDs []string

	// ExcludeOwnerID, when non-empty, excludes projects whose owner_id matches
	// this value. Used with MemberProjectIDs to return "shared" projects (member
	// but not owner).
	ExcludeOwnerID string
}

// RuntimeBrokerStore defines runtime broker persistence operations.
type RuntimeBrokerStore interface {
	// CreateRuntimeBroker creates a new runtime broker record.
	CreateRuntimeBroker(ctx context.Context, broker *RuntimeBroker) error

	// GetRuntimeBroker retrieves a runtime broker by ID.
	// Returns ErrNotFound if the broker doesn't exist.
	GetRuntimeBroker(ctx context.Context, id string) (*RuntimeBroker, error)

	// GetRuntimeBrokerByName retrieves a runtime broker by its name (case-insensitive).
	// This is used to prevent duplicate brokers with the same name.
	// Returns ErrNotFound if the broker doesn't exist.
	GetRuntimeBrokerByName(ctx context.Context, name string) (*RuntimeBroker, error)

	// UpdateRuntimeBroker updates an existing runtime broker.
	// Returns ErrNotFound if the broker doesn't exist.
	UpdateRuntimeBroker(ctx context.Context, broker *RuntimeBroker) error

	// DeleteRuntimeBroker removes a runtime broker by ID.
	// Returns ErrNotFound if the broker doesn't exist.
	DeleteRuntimeBroker(ctx context.Context, id string) error

	// ListRuntimeBrokers returns runtime brokers matching the filter criteria.
	ListRuntimeBrokers(ctx context.Context, filter RuntimeBrokerFilter, opts ListOptions) (*ListResult[RuntimeBroker], error)

	// UpdateRuntimeBrokerHeartbeat updates the last heartbeat and status.
	UpdateRuntimeBrokerHeartbeat(ctx context.Context, id string, status string) error
}

// RuntimeBrokerFilter defines criteria for filtering runtime brokers.
type RuntimeBrokerFilter struct {
	Status      string
	ProjectID   string
	Name        string // Exact match on broker name (case-insensitive)
	AutoProvide *bool  // Filter by auto-provide flag (nil = no filter)
}

// TemplateStore defines template persistence operations.
type TemplateStore interface {
	// CreateTemplate creates a new template record.
	CreateTemplate(ctx context.Context, template *Template) error

	// GetTemplate retrieves a template by ID.
	// Returns ErrNotFound if the template doesn't exist.
	GetTemplate(ctx context.Context, id string) (*Template, error)

	// GetTemplateBySlug retrieves a template by its slug and scope.
	// Returns ErrNotFound if the template doesn't exist.
	GetTemplateBySlug(ctx context.Context, slug, scope, projectID string) (*Template, error)

	// UpdateTemplate updates an existing template.
	// Returns ErrNotFound if the template doesn't exist.
	UpdateTemplate(ctx context.Context, template *Template) error

	// DeleteTemplate removes a template by ID.
	// Returns ErrNotFound if the template doesn't exist.
	DeleteTemplate(ctx context.Context, id string) error

	// DeleteTemplatesByScope removes all templates for a given scope.
	// Returns the number of deleted records. No error if zero rows affected.
	DeleteTemplatesByScope(ctx context.Context, scope, scopeID string) (int, error)

	// ListTemplates returns templates matching the filter criteria.
	ListTemplates(ctx context.Context, filter TemplateFilter, opts ListOptions) (*ListResult[Template], error)
}

// TemplateFilter defines criteria for filtering templates.
type TemplateFilter struct {
	Name      string // Exact match on template name
	Scope     string
	ScopeID   string
	ProjectID string // When set without Scope, returns global + project-scoped templates for this project
	Harness   string
	OwnerID   string
	Status    string
	Search    string // Full-text search on name/description
}

// HarnessConfigStore defines harness config persistence operations.
type HarnessConfigStore interface {
	// CreateHarnessConfig creates a new harness config record.
	CreateHarnessConfig(ctx context.Context, hc *HarnessConfig) error

	// GetHarnessConfig retrieves a harness config by ID.
	// Returns ErrNotFound if the harness config doesn't exist.
	GetHarnessConfig(ctx context.Context, id string) (*HarnessConfig, error)

	// GetHarnessConfigBySlug retrieves a harness config by its slug and scope.
	// Returns ErrNotFound if the harness config doesn't exist.
	GetHarnessConfigBySlug(ctx context.Context, slug, scope, scopeID string) (*HarnessConfig, error)

	// UpdateHarnessConfig updates an existing harness config.
	// Returns ErrNotFound if the harness config doesn't exist.
	UpdateHarnessConfig(ctx context.Context, hc *HarnessConfig) error

	// DeleteHarnessConfig removes a harness config by ID.
	// Returns ErrNotFound if the harness config doesn't exist.
	DeleteHarnessConfig(ctx context.Context, id string) error

	// DeleteHarnessConfigsByScope removes all harness configs for a given scope.
	// Returns the number of deleted records. No error if zero rows affected.
	DeleteHarnessConfigsByScope(ctx context.Context, scope, scopeID string) (int, error)

	// ListHarnessConfigs returns harness configs matching the filter criteria.
	ListHarnessConfigs(ctx context.Context, filter HarnessConfigFilter, opts ListOptions) (*ListResult[HarnessConfig], error)
}

// HarnessConfigFilter defines criteria for filtering harness configs.
type HarnessConfigFilter struct {
	Name      string // Exact match on name
	Scope     string
	ScopeID   string
	ProjectID string // When set without Scope, returns global + project-scoped configs for this project
	Harness   string
	OwnerID   string
	Status    string
	Search    string // Full-text search on name/description
}

// UserStore defines user persistence operations.
type UserStore interface {
	// CreateUser creates a new user record.
	CreateUser(ctx context.Context, user *User) error

	// GetUser retrieves a user by ID.
	// Returns ErrNotFound if the user doesn't exist.
	GetUser(ctx context.Context, id string) (*User, error)

	// GetUserByEmail retrieves a user by email.
	// Returns ErrNotFound if the user doesn't exist.
	GetUserByEmail(ctx context.Context, email string) (*User, error)

	// UpdateUser updates an existing user.
	// Returns ErrNotFound if the user doesn't exist.
	UpdateUser(ctx context.Context, user *User) error

	// UpdateUserLastSeen sets only the last_seen timestamp for a user.
	UpdateUserLastSeen(ctx context.Context, id string, t time.Time) error

	// DeleteUser removes a user by ID.
	// Returns ErrNotFound if the user doesn't exist.
	DeleteUser(ctx context.Context, id string) error

	// ListUsers returns users matching the filter criteria.
	ListUsers(ctx context.Context, filter UserFilter, opts ListOptions) (*ListResult[User], error)
}

// UserFilter defines criteria for filtering users.
type UserFilter struct {
	Role   string
	Status string
	Search string // fuzzy match on email and display_name
}

// AllowListStore defines operations for the email allow list used in invite_only mode.
type AllowListStore interface {
	AddAllowListEntry(ctx context.Context, entry *AllowListEntry) error
	RemoveAllowListEntry(ctx context.Context, email string) error
	GetAllowListEntry(ctx context.Context, email string) (*AllowListEntry, error)
	ListAllowListEntries(ctx context.Context, opts ListOptions) (*ListResult[AllowListEntry], error)
	IsEmailAllowListed(ctx context.Context, email string) (bool, error)
	BulkAddAllowListEntries(ctx context.Context, entries []*AllowListEntry) (added int, skipped int, err error)
	ListEmailDomains(ctx context.Context) ([]string, error)
}

// InviteCodeStore defines operations for invite code management.
type InviteCodeStore interface {
	CreateInviteCode(ctx context.Context, invite *InviteCode) error
	GetInviteCodeByHash(ctx context.Context, codeHash string) (*InviteCode, error)
	GetInviteCode(ctx context.Context, id string) (*InviteCode, error)
	ListInviteCodes(ctx context.Context, opts ListOptions) (*ListResult[InviteCode], error)
	IncrementInviteUseCount(ctx context.Context, id string) error
	RevokeInviteCode(ctx context.Context, id string) error
	DeleteInviteCode(ctx context.Context, id string) error
	GetInviteStats(ctx context.Context) (*InviteStats, error)
}

// ProjectProviderStore defines project-broker relationship operations.
type ProjectProviderStore interface {
	// AddProjectProvider adds a broker as a provider to a project.
	AddProjectProvider(ctx context.Context, provider *ProjectProvider) error


	// RemoveProjectProvider removes a broker from a project's providers.
	RemoveProjectProvider(ctx context.Context, projectID, brokerID string) error

	// GetProjectProvider returns a specific provider by project and broker ID.
	// Returns ErrNotFound if the provider relationship doesn't exist.
	GetProjectProvider(ctx context.Context, projectID, brokerID string) (*ProjectProvider, error)

	// GetProjectProviders returns all providers to a project.
	GetProjectProviders(ctx context.Context, projectID string) ([]ProjectProvider, error)

	// GetBrokerProjects returns all projects a broker provides for.
	GetBrokerProjects(ctx context.Context, brokerID string) ([]ProjectProvider, error)

	// UpdateProviderStatus updates a provider's status and last seen time.
	UpdateProviderStatus(ctx context.Context, projectID, brokerID, status string) error
}

// EnvVarStore defines environment variable persistence operations.
type EnvVarStore interface {
	// CreateEnvVar creates a new environment variable.
	// Returns ErrAlreadyExists if an env var with the same key+scope+scopeId exists.
	CreateEnvVar(ctx context.Context, envVar *EnvVar) error

	// GetEnvVar retrieves an environment variable by key, scope, and scopeId.
	// Returns ErrNotFound if the env var doesn't exist.
	GetEnvVar(ctx context.Context, key, scope, scopeID string) (*EnvVar, error)

	// UpdateEnvVar updates an existing environment variable.
	// Returns ErrNotFound if the env var doesn't exist.
	UpdateEnvVar(ctx context.Context, envVar *EnvVar) error

	// UpsertEnvVar creates or updates an environment variable.
	// Uses key+scope+scopeId as the unique identifier.
	UpsertEnvVar(ctx context.Context, envVar *EnvVar) (created bool, err error)

	// DeleteEnvVar removes an environment variable.
	// Returns ErrNotFound if the env var doesn't exist.
	DeleteEnvVar(ctx context.Context, key, scope, scopeID string) error

	// DeleteEnvVarsByScope removes all environment variables for a given scope.
	// Returns the number of deleted records. No error if zero rows affected.
	DeleteEnvVarsByScope(ctx context.Context, scope, scopeID string) (int, error)

	// ListEnvVars returns environment variables matching the filter criteria.
	ListEnvVars(ctx context.Context, filter EnvVarFilter) ([]EnvVar, error)
}

// EnvVarFilter defines criteria for filtering environment variables.
type EnvVarFilter struct {
	Scope   string // Required: user, project, runtime_broker
	ScopeID string // Required: ID of the scoped entity
	Key     string // Optional: filter by specific key
}

// SecretStore defines secret persistence operations.
type SecretStore interface {
	// CreateSecret creates a new secret.
	// Returns ErrAlreadyExists if a secret with the same key+scope+scopeId exists.
	CreateSecret(ctx context.Context, secret *Secret) error

	// GetSecret retrieves secret metadata by key, scope, and scopeId.
	// Returns ErrNotFound if the secret doesn't exist.
	// Note: The EncryptedValue is populated but should not be exposed via API.
	GetSecret(ctx context.Context, key, scope, scopeID string) (*Secret, error)

	// UpdateSecret updates an existing secret.
	// Increments the version automatically.
	// Returns ErrNotFound if the secret doesn't exist.
	UpdateSecret(ctx context.Context, secret *Secret) error

	// UpsertSecret creates or updates a secret.
	// Uses key+scope+scopeId as the unique identifier.
	UpsertSecret(ctx context.Context, secret *Secret) (created bool, err error)

	// DeleteSecret removes a secret.
	// Returns ErrNotFound if the secret doesn't exist.
	DeleteSecret(ctx context.Context, key, scope, scopeID string) error

	// DeleteSecretsByScope removes all secrets for a given scope.
	// Returns the number of deleted records. No error if zero rows affected.
	DeleteSecretsByScope(ctx context.Context, scope, scopeID string) (int, error)

	// ListSecrets returns secret metadata matching the filter criteria.
	// Note: EncryptedValue is NOT populated in the returned secrets.
	ListSecrets(ctx context.Context, filter SecretFilter) ([]Secret, error)

	// GetSecretValue retrieves the encrypted value of a secret.
	// This is used internally for environment resolution.
	// Returns ErrNotFound if the secret doesn't exist.
	GetSecretValue(ctx context.Context, key, scope, scopeID string) (encryptedValue string, err error)

	// ListProgenySecrets returns user-scoped secrets with allowProgeny=true
	// whose createdBy is in the given set of ancestor IDs.
	// Note: EncryptedValue is NOT populated in the returned secrets.
	ListProgenySecrets(ctx context.Context, ancestorIDs []string) ([]Secret, error)
}

// SecretFilter defines criteria for filtering secrets.
type SecretFilter struct {
	Scope   string // Required: user, project, runtime_broker
	ScopeID string // Required: ID of the scoped entity
	Key     string // Optional: filter by specific key
	Type    string // Optional: filter by secret type (environment, variable, file)
}

// =============================================================================
// Groups and Policies (Hub Permissions System)
// =============================================================================

// GroupStore defines group-related persistence operations.
type GroupStore interface {
	// CreateGroup creates a new group record.
	// Returns ErrAlreadyExists if a group with the same slug exists.
	CreateGroup(ctx context.Context, group *Group) error

	// GetGroup retrieves a group by ID.
	// Returns ErrNotFound if the group doesn't exist.
	GetGroup(ctx context.Context, id string) (*Group, error)

	// GetGroupBySlug retrieves a group by its slug.
	// Returns ErrNotFound if the group doesn't exist.
	GetGroupBySlug(ctx context.Context, slug string) (*Group, error)

	// UpdateGroup updates an existing group.
	// Returns ErrNotFound if the group doesn't exist.
	UpdateGroup(ctx context.Context, group *Group) error

	// DeleteGroup removes a group by ID.
	// Also removes all group memberships (both as parent and as member).
	// Returns ErrNotFound if the group doesn't exist.
	DeleteGroup(ctx context.Context, id string) error

	// ListGroups returns groups matching the filter criteria.
	ListGroups(ctx context.Context, filter GroupFilter, opts ListOptions) (*ListResult[Group], error)

	// AddGroupMember adds a user or group as a member of a group.
	// Returns ErrAlreadyExists if the membership already exists.
	AddGroupMember(ctx context.Context, member *GroupMember) error

	// RemoveGroupMember removes a member from a group.
	// Returns ErrNotFound if the membership doesn't exist.
	RemoveGroupMember(ctx context.Context, groupID, memberType, memberID string) error

	// UpdateGroupMemberRole updates the role of an existing group member.
	// Returns ErrNotFound if the membership doesn't exist.
	UpdateGroupMemberRole(ctx context.Context, groupID, memberType, memberID, newRole string) error

	// GetGroupMembers returns all members of a group.
	GetGroupMembers(ctx context.Context, groupID string) ([]GroupMember, error)

	// GetUserGroups returns all groups a user is a direct member of.
	GetUserGroups(ctx context.Context, userID string) ([]GroupMember, error)

	// GetGroupMembership returns a specific membership record.
	// Returns ErrNotFound if the membership doesn't exist.
	GetGroupMembership(ctx context.Context, groupID, memberType, memberID string) (*GroupMember, error)

	// WouldCreateCycle checks if adding memberGroupID to groupID would create a cycle.
	// Returns true if a cycle would be created.
	WouldCreateCycle(ctx context.Context, groupID, memberGroupID string) (bool, error)

	// GetGroupByProjectID retrieves the project_agents group associated with a project.
	// Returns ErrNotFound if no project group exists for this project.
	GetGroupByProjectID(ctx context.Context, projectID string) (*Group, error)

	// GetEffectiveGroups returns all groups a user belongs to, including
	// transitive memberships through nested groups.
	GetEffectiveGroups(ctx context.Context, userID string) ([]string, error)

	// GetEffectiveGroupsForAgent returns all groups an agent belongs to,
	// including the implicit project_agents group and transitive parent groups.
	GetEffectiveGroupsForAgent(ctx context.Context, agentID string) ([]string, error)

	// CheckDelegatedAccess checks whether an agent's delegation relationship
	// satisfies the given policy conditions. Returns true if the agent has
	// delegation enabled, its creator is active, and the conditions match.
	CheckDelegatedAccess(ctx context.Context, agentID string, conditions *PolicyConditions) (bool, error)

	// CountGroupMembersByRole counts how many members of a group have the given role.
	CountGroupMembersByRole(ctx context.Context, groupID, role string) (int, error)

	// GetGroupsByIDs retrieves groups by a list of IDs.
	// Returns only groups that exist; missing IDs are silently skipped.
	GetGroupsByIDs(ctx context.Context, ids []string) ([]Group, error)
}

// GroupFilter defines criteria for filtering groups.
type GroupFilter struct {
	OwnerID   string // Filter by owner
	ParentID  string // Filter by parent group
	GroupType string // Filter by group type ("explicit" or "project_agents")
	ProjectID string // Filter by project ID (for project_agents groups)
}

// PrincipalRef identifies a principal by type and ID.
// Used for bulk policy lookups across multiple principals.
type PrincipalRef struct {
	Type string // "user", "group", or "agent"
	ID   string // Principal UUID
}

// PolicyStore defines policy-related persistence operations.
type PolicyStore interface {
	// CreatePolicy creates a new policy record.
	CreatePolicy(ctx context.Context, policy *Policy) error

	// GetPolicy retrieves a policy by ID.
	// Returns ErrNotFound if the policy doesn't exist.
	GetPolicy(ctx context.Context, id string) (*Policy, error)

	// UpdatePolicy updates an existing policy.
	// Returns ErrNotFound if the policy doesn't exist.
	UpdatePolicy(ctx context.Context, policy *Policy) error

	// DeletePolicy removes a policy by ID.
	// Also removes all policy bindings.
	// Returns ErrNotFound if the policy doesn't exist.
	DeletePolicy(ctx context.Context, id string) error

	// ListPolicies returns policies matching the filter criteria.
	ListPolicies(ctx context.Context, filter PolicyFilter, opts ListOptions) (*ListResult[Policy], error)

	// AddPolicyBinding binds a principal (user, group, or agent) to a policy.
	// Returns ErrAlreadyExists if the binding already exists.
	AddPolicyBinding(ctx context.Context, binding *PolicyBinding) error

	// RemovePolicyBinding removes a binding from a policy.
	// Returns ErrNotFound if the binding doesn't exist.
	RemovePolicyBinding(ctx context.Context, policyID, principalType, principalID string) error

	// GetPolicyBindings returns all bindings for a policy.
	GetPolicyBindings(ctx context.Context, policyID string) ([]PolicyBinding, error)

	// GetPoliciesForPrincipal returns all policies bound to a specific principal.
	GetPoliciesForPrincipal(ctx context.Context, principalType, principalID string) ([]Policy, error)

	// GetPoliciesForPrincipals returns all policies bound to any of the given principals.
	// Results are ordered by scope_type then priority ASC.
	GetPoliciesForPrincipals(ctx context.Context, principals []PrincipalRef) ([]Policy, error)
}

// PolicyFilter defines criteria for filtering policies.
type PolicyFilter struct {
	Name         string // Filter by policy name
	ScopeType    string // Filter by scope type (hub, project, resource)
	ScopeID      string // Filter by scope ID
	ResourceType string // Filter by resource type
	Effect       string // Filter by effect (allow, deny)
}

// =============================================================================
// User Access Tokens (UATs)
// =============================================================================

// UserAccessTokenStore defines user access token persistence operations.
type UserAccessTokenStore interface {
	// CreateUserAccessToken creates a new user access token record.
	CreateUserAccessToken(ctx context.Context, token *UserAccessToken) error

	// GetUserAccessToken retrieves a user access token by ID.
	// Returns ErrNotFound if the token doesn't exist.
	GetUserAccessToken(ctx context.Context, id string) (*UserAccessToken, error)

	// GetUserAccessTokenByHash retrieves a user access token by its key hash.
	// Returns ErrNotFound if the token doesn't exist.
	GetUserAccessTokenByHash(ctx context.Context, hash string) (*UserAccessToken, error)

	// UpdateUserAccessTokenLastUsed updates the last used timestamp.
	UpdateUserAccessTokenLastUsed(ctx context.Context, id string) error

	// RevokeUserAccessToken marks a token as revoked.
	// Returns ErrNotFound if the token doesn't exist.
	RevokeUserAccessToken(ctx context.Context, id string) error

	// DeleteUserAccessToken permanently removes a token by ID.
	// Returns ErrNotFound if the token doesn't exist.
	DeleteUserAccessToken(ctx context.Context, id string) error

	// ListUserAccessTokens returns all non-deleted tokens for a user.
	ListUserAccessTokens(ctx context.Context, userID string) ([]UserAccessToken, error)

	// CountUserAccessTokens returns the number of active (non-revoked) tokens for a user.
	CountUserAccessTokens(ctx context.Context, userID string) (int, error)
}

// =============================================================================
// Broker Secrets (Runtime Broker Authentication)
// =============================================================================

// BrokerSecretStore defines broker secret persistence operations.
type BrokerSecretStore interface {
	// CreateBrokerSecret creates a new broker secret record.
	// Returns ErrAlreadyExists if a secret for this broker already exists.
	CreateBrokerSecret(ctx context.Context, secret *BrokerSecret) error

	// GetBrokerSecret retrieves a broker secret by broker ID.
	// Returns ErrNotFound if the secret doesn't exist.
	GetBrokerSecret(ctx context.Context, brokerID string) (*BrokerSecret, error)

	// GetActiveSecrets retrieves all active and deprecated (within grace period) secrets for a broker.
	// This is used during secret rotation to support dual-secret validation.
	// Returns an empty slice if no secrets exist.
	GetActiveSecrets(ctx context.Context, brokerID string) ([]*BrokerSecret, error)

	// UpdateBrokerSecret updates an existing broker secret.
	// Returns ErrNotFound if the secret doesn't exist.
	UpdateBrokerSecret(ctx context.Context, secret *BrokerSecret) error

	// DeleteBrokerSecret removes a broker secret.
	// Returns ErrNotFound if the secret doesn't exist.
	DeleteBrokerSecret(ctx context.Context, brokerID string) error

	// CreateJoinToken creates a new join token for broker registration.
	// Returns ErrAlreadyExists if a token for this broker already exists.
	CreateJoinToken(ctx context.Context, token *BrokerJoinToken) error

	// GetJoinToken retrieves a join token by token hash.
	// Returns ErrNotFound if the token doesn't exist.
	GetJoinToken(ctx context.Context, tokenHash string) (*BrokerJoinToken, error)

	// GetJoinTokenByBrokerID retrieves a join token by broker ID.
	// Returns ErrNotFound if the token doesn't exist.
	GetJoinTokenByBrokerID(ctx context.Context, brokerID string) (*BrokerJoinToken, error)

	// DeleteJoinToken removes a join token by broker ID.
	// Returns ErrNotFound if the token doesn't exist.
	DeleteJoinToken(ctx context.Context, brokerID string) error

	// CleanExpiredJoinTokens removes all expired join tokens.
	CleanExpiredJoinTokens(ctx context.Context) error
}

// =============================================================================
// Notifications (Agent Status Notification System)
// =============================================================================

// NotificationStore manages notification subscriptions and notification records.
type NotificationStore interface {
	// CreateNotificationSubscription creates a new notification subscription.
	CreateNotificationSubscription(ctx context.Context, sub *NotificationSubscription) error

	// GetNotificationSubscription returns a single subscription by ID.
	// Returns ErrNotFound if the subscription doesn't exist.
	GetNotificationSubscription(ctx context.Context, id string) (*NotificationSubscription, error)

	// GetNotificationSubscriptions returns all agent-scoped subscriptions for a watched agent.
	GetNotificationSubscriptions(ctx context.Context, agentID string) ([]NotificationSubscription, error)

	// GetNotificationSubscriptionsByProject returns all subscriptions within a project (any scope).
	GetNotificationSubscriptionsByProject(ctx context.Context, projectID string) ([]NotificationSubscription, error)

	// GetNotificationSubscriptionsByProjectScope returns project-scoped subscriptions
	// (scope='project') for a given project.
	GetNotificationSubscriptionsByProjectScope(ctx context.Context, projectID string) ([]NotificationSubscription, error)

	// GetSubscriptionsForSubscriber returns all subscriptions owned by a subscriber.
	GetSubscriptionsForSubscriber(ctx context.Context, subscriberType, subscriberID string) ([]NotificationSubscription, error)

	// UpdateNotificationSubscriptionTriggers updates the trigger activities of a subscription.
	// Returns ErrNotFound if the subscription doesn't exist.
	UpdateNotificationSubscriptionTriggers(ctx context.Context, id string, triggerActivities []string) error

	// DeleteNotificationSubscription deletes a subscription by ID.
	// Returns ErrNotFound if the subscription doesn't exist.
	DeleteNotificationSubscription(ctx context.Context, id string) error

	// DeleteNotificationSubscriptionsForAgent deletes all subscriptions for a watched agent.
	// No error on zero rows affected.
	DeleteNotificationSubscriptionsForAgent(ctx context.Context, agentID string) error

	// CreateNotification creates a new notification record.
	CreateNotification(ctx context.Context, notif *Notification) error

	// GetNotifications returns notifications for a subscriber.
	// If onlyUnacknowledged is true, only unacknowledged notifications are returned.
	// Results are ordered by created_at DESC.
	GetNotifications(ctx context.Context, subscriberType, subscriberID string, onlyUnacknowledged bool) ([]Notification, error)

	// GetNotificationsByAgent returns notifications for a subscriber filtered by agent ID.
	// If onlyUnacknowledged is true, only unacknowledged notifications are returned.
	// Results are ordered by created_at DESC.
	GetNotificationsByAgent(ctx context.Context, agentID, subscriberType, subscriberID string, onlyUnacknowledged bool) ([]Notification, error)

	// AcknowledgeNotification marks a notification as acknowledged.
	// Returns ErrNotFound if the notification doesn't exist.
	AcknowledgeNotification(ctx context.Context, id string) error

	// AcknowledgeAllNotifications marks all notifications for a subscriber as acknowledged.
	// No error on zero rows affected.
	AcknowledgeAllNotifications(ctx context.Context, subscriberType, subscriberID string) error

	// MarkNotificationDispatched marks a notification as dispatched.
	// Returns ErrNotFound if the notification doesn't exist.
	MarkNotificationDispatched(ctx context.Context, id string) error

	// GetLastNotificationStatus returns the status of the most recent notification
	// for a given subscription. Returns ("", nil) if no notifications exist.
	GetLastNotificationStatus(ctx context.Context, subscriptionID string) (string, error)

	// CreateSubscriptionTemplate creates a new subscription template.
	CreateSubscriptionTemplate(ctx context.Context, tmpl *SubscriptionTemplate) error

	// GetSubscriptionTemplate returns a template by ID.
	// Returns ErrNotFound if the template doesn't exist.
	GetSubscriptionTemplate(ctx context.Context, id string) (*SubscriptionTemplate, error)

	// ListSubscriptionTemplates returns all templates, optionally filtered by project.
	// Pass empty projectID to include global templates only, or a specific projectID
	// to include both global and project-specific templates.
	ListSubscriptionTemplates(ctx context.Context, projectID string) ([]SubscriptionTemplate, error)

	// DeleteSubscriptionTemplate deletes a template by ID.
	// Returns ErrNotFound if the template doesn't exist.
	DeleteSubscriptionTemplate(ctx context.Context, id string) error
}

// =============================================================================
// Scheduled Events (One-Shot Timers)
// =============================================================================

// ScheduledEventStore manages one-shot scheduled events.
type ScheduledEventStore interface {
	// CreateScheduledEvent creates a new scheduled event.
	CreateScheduledEvent(ctx context.Context, event *ScheduledEvent) error

	// GetScheduledEvent retrieves a scheduled event by ID.
	// Returns ErrNotFound if the event doesn't exist.
	GetScheduledEvent(ctx context.Context, id string) (*ScheduledEvent, error)

	// ListPendingScheduledEvents returns all events with status "pending".
	// Used on startup to load timers into memory.
	ListPendingScheduledEvents(ctx context.Context) ([]ScheduledEvent, error)

	// UpdateScheduledEventStatus updates the status and optional error for an event.
	UpdateScheduledEventStatus(ctx context.Context, id string, status string, firedAt *time.Time, errMsg string) error

	// CancelScheduledEvent marks an event as cancelled.
	// Returns ErrNotFound if the event doesn't exist or is not pending.
	CancelScheduledEvent(ctx context.Context, id string) error

	// ListScheduledEvents returns events matching the filter criteria.
	ListScheduledEvents(ctx context.Context, filter ScheduledEventFilter, opts ListOptions) (*ListResult[ScheduledEvent], error)

	// PurgeOldScheduledEvents removes non-pending events older than cutoff.
	PurgeOldScheduledEvents(ctx context.Context, cutoff time.Time) (int, error)
}

// =============================================================================
// Recurring Schedules (Cron-Based)
// =============================================================================

// ScheduleStore manages user-defined recurring schedules.
type ScheduleStore interface {
	// CreateSchedule creates a new recurring schedule.
	// Returns ErrAlreadyExists if a schedule with the same project_id+name exists.
	CreateSchedule(ctx context.Context, schedule *Schedule) error

	// GetSchedule retrieves a schedule by ID.
	// Returns ErrNotFound if the schedule doesn't exist.
	GetSchedule(ctx context.Context, id string) (*Schedule, error)

	// ListSchedules returns schedules matching the filter criteria.
	ListSchedules(ctx context.Context, filter ScheduleFilter, opts ListOptions) (*ListResult[Schedule], error)

	// UpdateSchedule updates an existing schedule (name, cron_expr, payload, status).
	// Returns ErrNotFound if the schedule doesn't exist.
	UpdateSchedule(ctx context.Context, schedule *Schedule) error

	// UpdateScheduleStatus updates only the status of a schedule.
	// Returns ErrNotFound if the schedule doesn't exist.
	UpdateScheduleStatus(ctx context.Context, id string, status string) error

	// UpdateScheduleAfterRun updates a schedule after a run completes.
	// Sets last_run_at, next_run_at, run counters, and error status.
	UpdateScheduleAfterRun(ctx context.Context, id string, ranAt time.Time, nextRunAt time.Time, errMsg string) error

	// DeleteSchedule removes a schedule by ID.
	// Returns ErrNotFound if the schedule doesn't exist.
	DeleteSchedule(ctx context.Context, id string) error

	// ListDueSchedules returns active schedules whose next_run_at has passed.
	ListDueSchedules(ctx context.Context, now time.Time) ([]Schedule, error)
}

// =============================================================================
// GCP Service Accounts (GCP Identity for Agents)
// =============================================================================

// GCPServiceAccountStore defines GCP service account persistence operations.
type GCPServiceAccountStore interface {
	// CreateGCPServiceAccount registers a new GCP service account.
	// Returns ErrAlreadyExists if a SA with the same email+scope+scopeID exists.
	CreateGCPServiceAccount(ctx context.Context, sa *GCPServiceAccount) error

	// GetGCPServiceAccount retrieves a GCP service account by ID.
	// Returns ErrNotFound if the SA doesn't exist.
	GetGCPServiceAccount(ctx context.Context, id string) (*GCPServiceAccount, error)

	// UpdateGCPServiceAccount updates a GCP service account record.
	// Returns ErrNotFound if the SA doesn't exist.
	UpdateGCPServiceAccount(ctx context.Context, sa *GCPServiceAccount) error

	// DeleteGCPServiceAccount removes a GCP service account by ID.
	// Returns ErrNotFound if the SA doesn't exist.
	DeleteGCPServiceAccount(ctx context.Context, id string) error

	// ListGCPServiceAccounts returns GCP service accounts matching the filter.
	ListGCPServiceAccounts(ctx context.Context, filter GCPServiceAccountFilter) ([]GCPServiceAccount, error)

	// CountGCPServiceAccounts returns the number of GCP service accounts matching the filter.
	CountGCPServiceAccounts(ctx context.Context, filter GCPServiceAccountFilter) (int, error)
}

// GCPServiceAccountFilter defines criteria for filtering GCP service accounts.
type GCPServiceAccountFilter struct {
	Scope   string // Filter by scope (hub, project, user)
	ScopeID string // Filter by scope ID
	Email   string // Filter by SA email
	Managed *bool  // Filter by managed status (nil = no filter)
}

// =============================================================================
// GitHub App Installations
// =============================================================================

// GitHubInstallationStore defines GitHub App installation persistence operations.
type GitHubInstallationStore interface {
	// CreateGitHubInstallation creates a new GitHub App installation record.
	// Uses installation_id as the natural key — creating an existing one is idempotent (no-op).
	CreateGitHubInstallation(ctx context.Context, installation *GitHubInstallation) error

	// GetGitHubInstallation retrieves a GitHub App installation by installation ID.
	// Returns ErrNotFound if the installation doesn't exist.
	GetGitHubInstallation(ctx context.Context, installationID int64) (*GitHubInstallation, error)

	// UpdateGitHubInstallation updates an existing GitHub App installation.
	// Returns ErrNotFound if the installation doesn't exist.
	UpdateGitHubInstallation(ctx context.Context, installation *GitHubInstallation) error

	// DeleteGitHubInstallation removes a GitHub App installation by installation ID.
	// Returns ErrNotFound if the installation doesn't exist.
	DeleteGitHubInstallation(ctx context.Context, installationID int64) error

	// ListGitHubInstallations returns all GitHub App installations matching the filter.
	ListGitHubInstallations(ctx context.Context, filter GitHubInstallationFilter) ([]GitHubInstallation, error)

	// GetInstallationForRepository returns an active GitHub App installation
	// that covers the given repository (owner/repo format).
	// Returns ErrNotFound if no matching active installation exists.
	GetInstallationForRepository(ctx context.Context, repoFullName string) (*GitHubInstallation, error)
}

// GitHubInstallationFilter defines criteria for filtering GitHub App installations.
type GitHubInstallationFilter struct {
	AccountLogin string // Filter by GitHub account login
	Status       string // Filter by status (active, suspended, deleted)
	AppID        int64  // Filter by app ID
}

// =============================================================================
// Messages (Bidirectional Human-Agent Messaging)
// =============================================================================

// MessageStore manages persisted structured messages.
type MessageStore interface {
	// CreateMessage persists a new message.
	CreateMessage(ctx context.Context, msg *Message) error

	// GetMessage returns a single message by ID.
	// Returns ErrNotFound if the message doesn't exist.
	GetMessage(ctx context.Context, id string) (*Message, error)

	// ListMessages returns messages matching the given filter.
	// Results are ordered by created_at DESC.
	ListMessages(ctx context.Context, filter MessageFilter, opts ListOptions) (*ListResult[Message], error)

	// MarkMessageRead marks a message as read.
	// Returns ErrNotFound if the message doesn't exist.
	MarkMessageRead(ctx context.Context, id string) error

	// MarkAllMessagesRead marks all messages for a recipient as read.
	MarkAllMessagesRead(ctx context.Context, recipientID string) error

	// PurgeOldMessages removes read messages older than readCutoff and
	// unread messages older than unreadCutoff. Returns count removed.
	PurgeOldMessages(ctx context.Context, readCutoff time.Time, unreadCutoff time.Time) (int, error)
}

// =============================================================================
// Maintenance Operations (Admin Maintenance Panel)
// =============================================================================

// MaintenanceStore defines storage operations for maintenance tasks.
type MaintenanceStore interface {
	// ListMaintenanceOperations returns all registered operations and migrations.
	ListMaintenanceOperations(ctx context.Context) ([]MaintenanceOperation, error)

	// GetMaintenanceOperation returns a single operation by key.
	GetMaintenanceOperation(ctx context.Context, key string) (*MaintenanceOperation, error)

	// UpdateMaintenanceOperation updates an operation's status and result fields.
	UpdateMaintenanceOperation(ctx context.Context, op *MaintenanceOperation) error

	// CreateMaintenanceRun inserts a new run record.
	CreateMaintenanceRun(ctx context.Context, run *MaintenanceOperationRun) error

	// UpdateMaintenanceRun updates a run's status, result, and log.
	UpdateMaintenanceRun(ctx context.Context, run *MaintenanceOperationRun) error

	// GetMaintenanceRun returns a single run by ID.
	GetMaintenanceRun(ctx context.Context, id string) (*MaintenanceOperationRun, error)

	// ListMaintenanceRuns returns runs for a given operation key, ordered by started_at DESC.
	ListMaintenanceRuns(ctx context.Context, operationKey string, limit int) ([]MaintenanceOperationRun, error)

	// AbortRunningMaintenanceOps transitions any "running" operation runs and
	// migrations to "failed". Called at server startup to clean up operations
	// that were interrupted by a restart.
	AbortRunningMaintenanceOps(ctx context.Context) (runs int64, migrations int64, err error)
}

// =============================================================================
// Project Sync State (Workspace Sync Metadata)
// =============================================================================

// ProjectSyncStateStore manages sync metadata for project workspace synchronization.
type ProjectSyncStateStore interface {
	// UpsertProjectSyncState creates or updates sync state for a project (optionally per broker).
	UpsertProjectSyncState(ctx context.Context, state *ProjectSyncState) error

	// GetProjectSyncState retrieves sync state for a project and optional broker.
	// Pass empty brokerID for hub-native project state.
	// Returns ErrNotFound if no sync state exists.
	GetProjectSyncState(ctx context.Context, projectID, brokerID string) (*ProjectSyncState, error)

	// ListProjectSyncStates returns all sync states for a project (across all brokers).
	ListProjectSyncStates(ctx context.Context, projectID string) ([]ProjectSyncState, error)

	// DeleteProjectSyncState removes sync state for a project and optional broker.
	// Returns ErrNotFound if the state doesn't exist.
	DeleteProjectSyncState(ctx context.Context, projectID, brokerID string) error
}
