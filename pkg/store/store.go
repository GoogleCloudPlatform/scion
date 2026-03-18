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

	// Grove operations
	GroveStore

	// RuntimeBroker operations
	RuntimeBrokerStore

	// Template operations
	TemplateStore

	// HarnessConfig operations
	HarnessConfigStore

	// User operations
	UserStore

	// GroveProvider operations
	GroveProviderStore

	// EnvVar operations
	EnvVarStore

	// Secret operations
	SecretStore

	// Group operations (Hub Permissions System)
	GroupStore

	// Policy operations (Hub Permissions System)
	PolicyStore

	// API Key operations
	APIKeyStore

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
}

// AgentStore defines agent-related persistence operations.
type AgentStore interface {
	// CreateAgent creates a new agent record.
	// Returns ErrAlreadyExists if an agent with the same ID exists.
	CreateAgent(ctx context.Context, agent *Agent) error

	// GetAgent retrieves an agent by ID.
	// Returns ErrNotFound if the agent doesn't exist.
	GetAgent(ctx context.Context, id string) (*Agent, error)

	// GetAgentBySlug retrieves an agent by its slug within a grove.
	// Returns ErrNotFound if the agent doesn't exist.
	GetAgentBySlug(ctx context.Context, groveID, slug string) (*Agent, error)

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
	GroveID         string
	RuntimeBrokerID string
	Phase           string
	OwnerID         string
	IncludeDeleted  bool // If true, include soft-deleted agents in results
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
}

// GroveStore defines grove-related persistence operations.
type GroveStore interface {
	// CreateGrove creates a new grove record.
	// Returns ErrAlreadyExists if a grove with the same git remote exists.
	CreateGrove(ctx context.Context, grove *Grove) error

	// GetGrove retrieves a grove by ID.
	// Returns ErrNotFound if the grove doesn't exist.
	GetGrove(ctx context.Context, id string) (*Grove, error)

	// GetGroveBySlug retrieves a grove by its slug.
	// Returns ErrNotFound if the grove doesn't exist.
	GetGroveBySlug(ctx context.Context, slug string) (*Grove, error)

	// GetGroveBySlugCaseInsensitive retrieves a grove by its slug, ignoring case.
	// This is useful for matching groves without git remotes (like global groves).
	// Returns ErrNotFound if the grove doesn't exist.
	GetGroveBySlugCaseInsensitive(ctx context.Context, slug string) (*Grove, error)

	// GetGroveByGitRemote retrieves a grove by its normalized git remote URL.
	// Returns ErrNotFound if the grove doesn't exist.
	GetGroveByGitRemote(ctx context.Context, gitRemote string) (*Grove, error)

	// UpdateGrove updates an existing grove.
	// Returns ErrNotFound if the grove doesn't exist.
	UpdateGrove(ctx context.Context, grove *Grove) error

	// DeleteGrove removes a grove by ID.
	// Returns ErrNotFound if the grove doesn't exist.
	DeleteGrove(ctx context.Context, id string) error

	// ListGroves returns groves matching the filter criteria.
	ListGroves(ctx context.Context, filter GroveFilter, opts ListOptions) (*ListResult[Grove], error)
}

// GroveFilter defines criteria for filtering groves.
type GroveFilter struct {
	OwnerID         string
	Visibility      string
	GitRemotePrefix string
	BrokerID string // Filter by contributing broker
	Name            string // Filter by exact name (case-insensitive)
	Slug            string // Filter by exact slug (case-insensitive)
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
	GroveID     string
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
	GetTemplateBySlug(ctx context.Context, slug, scope, groveID string) (*Template, error)

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
	Name    string // Exact match on template name
	Scope   string
	ScopeID string
	GroveID string // When set without Scope, returns global + grove-scoped templates for this grove
	Harness string
	OwnerID string
	Status  string
	Search  string // Full-text search on name/description
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
	Name    string // Exact match on name
	Scope   string
	ScopeID string
	GroveID string // When set without Scope, returns global + grove-scoped configs for this grove
	Harness string
	OwnerID string
	Status  string
	Search  string // Full-text search on name/description
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

// GroveProviderStore defines grove-broker relationship operations.
type GroveProviderStore interface {
	// AddGroveProvider adds a broker as a provider to a grove.
	AddGroveProvider(ctx context.Context, provider *GroveProvider) error

	// RemoveGroveProvider removes a broker from a grove's providers.
	RemoveGroveProvider(ctx context.Context, groveID, brokerID string) error

	// GetGroveProvider returns a specific provider by grove and broker ID.
	// Returns ErrNotFound if the provider relationship doesn't exist.
	GetGroveProvider(ctx context.Context, groveID, brokerID string) (*GroveProvider, error)

	// GetGroveProviders returns all providers to a grove.
	GetGroveProviders(ctx context.Context, groveID string) ([]GroveProvider, error)

	// GetBrokerGroves returns all groves a broker provides for.
	GetBrokerGroves(ctx context.Context, brokerID string) ([]GroveProvider, error)

	// UpdateProviderStatus updates a provider's status and last seen time.
	UpdateProviderStatus(ctx context.Context, groveID, brokerID, status string) error
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
	Scope   string // Required: user, grove, runtime_broker
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
}

// SecretFilter defines criteria for filtering secrets.
type SecretFilter struct {
	Scope   string // Required: user, grove, runtime_broker
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

	// GetGroupByGroveID retrieves the grove_agents group associated with a grove.
	// Returns ErrNotFound if no grove group exists for this grove.
	GetGroupByGroveID(ctx context.Context, groveID string) (*Group, error)

	// GetEffectiveGroups returns all groups a user belongs to, including
	// transitive memberships through nested groups.
	GetEffectiveGroups(ctx context.Context, userID string) ([]string, error)

	// GetEffectiveGroupsForAgent returns all groups an agent belongs to,
	// including the implicit grove_agents group and transitive parent groups.
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
	GroupType string // Filter by group type ("explicit" or "grove_agents")
	GroveID   string // Filter by grove ID (for grove_agents groups)
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
	ScopeType    string // Filter by scope type (hub, grove, resource)
	ScopeID      string // Filter by scope ID
	ResourceType string // Filter by resource type
	Effect       string // Filter by effect (allow, deny)
}

// =============================================================================
// API Keys
// =============================================================================

// APIKeyStore defines API key persistence operations.
type APIKeyStore interface {
	// CreateAPIKey creates a new API key record.
	// Returns the created key (with ID set).
	CreateAPIKey(ctx context.Context, key *APIKey) error

	// GetAPIKey retrieves an API key by ID.
	// Returns ErrNotFound if the key doesn't exist.
	GetAPIKey(ctx context.Context, id string) (*APIKey, error)

	// GetAPIKeyByHash retrieves an API key by its hash.
	// Returns ErrNotFound if the key doesn't exist.
	GetAPIKeyByHash(ctx context.Context, hash string) (*APIKey, error)

	// GetAPIKeyByPrefix retrieves an API key by its prefix.
	// Returns ErrNotFound if the key doesn't exist.
	GetAPIKeyByPrefix(ctx context.Context, prefix string) (*APIKey, error)

	// UpdateAPIKey updates an existing API key.
	// Returns ErrNotFound if the key doesn't exist.
	UpdateAPIKey(ctx context.Context, key *APIKey) error

	// UpdateAPIKeyLastUsed updates the last used timestamp.
	UpdateAPIKeyLastUsed(ctx context.Context, id string) error

	// DeleteAPIKey removes an API key by ID.
	// Returns ErrNotFound if the key doesn't exist.
	DeleteAPIKey(ctx context.Context, id string) error

	// ListAPIKeys returns API keys for a user.
	ListAPIKeys(ctx context.Context, userID string) ([]APIKey, error)

	// RevokeUserAPIKeys revokes all API keys for a user.
	RevokeUserAPIKeys(ctx context.Context, userID string) error
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

	// GetNotificationSubscriptionsByGrove returns all subscriptions within a grove (any scope).
	GetNotificationSubscriptionsByGrove(ctx context.Context, groveID string) ([]NotificationSubscription, error)

	// GetNotificationSubscriptionsByGroveScope returns grove-scoped subscriptions
	// (scope='grove') for a given grove.
	GetNotificationSubscriptionsByGroveScope(ctx context.Context, groveID string) ([]NotificationSubscription, error)

	// GetSubscriptionsForSubscriber returns all subscriptions owned by a subscriber.
	GetSubscriptionsForSubscriber(ctx context.Context, subscriberType, subscriberID string) ([]NotificationSubscription, error)

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
	// Returns ErrAlreadyExists if a schedule with the same grove_id+name exists.
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
}

// GCPServiceAccountFilter defines criteria for filtering GCP service accounts.
type GCPServiceAccountFilter struct {
	Scope   string // Filter by scope (hub, grove, user)
	ScopeID string // Filter by scope ID
	Email   string // Filter by SA email
}
