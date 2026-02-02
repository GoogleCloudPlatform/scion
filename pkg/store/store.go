package store

import (
	"context"
	"errors"
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

	// RuntimeHost operations
	RuntimeHostStore

	// Template operations
	TemplateStore

	// User operations
	UserStore

	// GroveContributor operations
	GroveContributorStore

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

	// Host Secret operations (Runtime Host authentication)
	HostSecretStore
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
}

// AgentFilter defines criteria for filtering agents.
type AgentFilter struct {
	GroveID       string
	RuntimeHostID string
	Status        string
	OwnerID       string
}

// AgentStatusUpdate contains fields for status-only updates.
type AgentStatusUpdate struct {
	Status          string `json:"status,omitempty"`
	Message         string `json:"message,omitempty"`
	ConnectionState string `json:"connectionState,omitempty"`
	ContainerStatus string `json:"containerStatus,omitempty"`
	SessionStatus   string `json:"sessionStatus,omitempty"`
	RuntimeState    string `json:"runtimeState,omitempty"`
	TaskSummary     string `json:"taskSummary,omitempty"`
	Heartbeat       bool   `json:"heartbeat,omitempty"`
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
	HostID          string // Filter by contributing host
	Name            string // Filter by exact name (case-insensitive)
}

// RuntimeHostStore defines runtime host persistence operations.
type RuntimeHostStore interface {
	// CreateRuntimeHost creates a new runtime host record.
	CreateRuntimeHost(ctx context.Context, host *RuntimeHost) error

	// GetRuntimeHost retrieves a runtime host by ID.
	// Returns ErrNotFound if the host doesn't exist.
	GetRuntimeHost(ctx context.Context, id string) (*RuntimeHost, error)

	// GetRuntimeHostByName retrieves a runtime host by its name (case-insensitive).
	// This is used to prevent duplicate hosts with the same name.
	// Returns ErrNotFound if the host doesn't exist.
	GetRuntimeHostByName(ctx context.Context, name string) (*RuntimeHost, error)

	// UpdateRuntimeHost updates an existing runtime host.
	// Returns ErrNotFound if the host doesn't exist.
	UpdateRuntimeHost(ctx context.Context, host *RuntimeHost) error

	// DeleteRuntimeHost removes a runtime host by ID.
	// Returns ErrNotFound if the host doesn't exist.
	DeleteRuntimeHost(ctx context.Context, id string) error

	// ListRuntimeHosts returns runtime hosts matching the filter criteria.
	ListRuntimeHosts(ctx context.Context, filter RuntimeHostFilter, opts ListOptions) (*ListResult[RuntimeHost], error)

	// UpdateRuntimeHostHeartbeat updates the last heartbeat and status.
	UpdateRuntimeHostHeartbeat(ctx context.Context, id string, status string) error
}

// RuntimeHostFilter defines criteria for filtering runtime hosts.
type RuntimeHostFilter struct {
	Status  string
	Mode    string
	GroveID string
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

	// ListTemplates returns templates matching the filter criteria.
	ListTemplates(ctx context.Context, filter TemplateFilter, opts ListOptions) (*ListResult[Template], error)
}

// TemplateFilter defines criteria for filtering templates.
type TemplateFilter struct {
	Name    string // Exact match on template name
	Scope   string
	ScopeID string
	GroveID string // Deprecated: use ScopeID
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
}

// GroveContributorStore defines grove-host relationship operations.
type GroveContributorStore interface {
	// AddGroveContributor adds a host as a contributor to a grove.
	AddGroveContributor(ctx context.Context, contrib *GroveContributor) error

	// RemoveGroveContributor removes a host from a grove's contributors.
	RemoveGroveContributor(ctx context.Context, groveID, hostID string) error

	// GetGroveContributor returns a specific contributor by grove and host ID.
	// Returns ErrNotFound if the contributor relationship doesn't exist.
	GetGroveContributor(ctx context.Context, groveID, hostID string) (*GroveContributor, error)

	// GetGroveContributors returns all contributors to a grove.
	GetGroveContributors(ctx context.Context, groveID string) ([]GroveContributor, error)

	// GetHostGroves returns all groves a host contributes to.
	GetHostGroves(ctx context.Context, hostID string) ([]GroveContributor, error)

	// UpdateContributorStatus updates a contributor's status and last seen time.
	UpdateContributorStatus(ctx context.Context, groveID, hostID, status string) error
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

	// ListEnvVars returns environment variables matching the filter criteria.
	ListEnvVars(ctx context.Context, filter EnvVarFilter) ([]EnvVar, error)
}

// EnvVarFilter defines criteria for filtering environment variables.
type EnvVarFilter struct {
	Scope   string // Required: user, grove, runtime_host
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
	Scope   string // Required: user, grove, runtime_host
	ScopeID string // Required: ID of the scoped entity
	Key     string // Optional: filter by specific key
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

	// GetEffectiveGroups returns all groups a user belongs to, including
	// transitive memberships through nested groups.
	GetEffectiveGroups(ctx context.Context, userID string) ([]string, error)
}

// GroupFilter defines criteria for filtering groups.
type GroupFilter struct {
	OwnerID  string // Filter by owner
	ParentID string // Filter by parent group
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

	// AddPolicyBinding binds a principal (user or group) to a policy.
	// Returns ErrAlreadyExists if the binding already exists.
	AddPolicyBinding(ctx context.Context, binding *PolicyBinding) error

	// RemovePolicyBinding removes a binding from a policy.
	// Returns ErrNotFound if the binding doesn't exist.
	RemovePolicyBinding(ctx context.Context, policyID, principalType, principalID string) error

	// GetPolicyBindings returns all bindings for a policy.
	GetPolicyBindings(ctx context.Context, policyID string) ([]PolicyBinding, error)

	// GetPoliciesForPrincipal returns all policies bound to a specific principal.
	GetPoliciesForPrincipal(ctx context.Context, principalType, principalID string) ([]Policy, error)
}

// PolicyFilter defines criteria for filtering policies.
type PolicyFilter struct {
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
// Host Secrets (Runtime Host Authentication)
// =============================================================================

// HostSecretStore defines host secret persistence operations.
type HostSecretStore interface {
	// CreateHostSecret creates a new host secret record.
	// Returns ErrAlreadyExists if a secret for this host already exists.
	CreateHostSecret(ctx context.Context, secret *HostSecret) error

	// GetHostSecret retrieves a host secret by host ID.
	// Returns ErrNotFound if the secret doesn't exist.
	GetHostSecret(ctx context.Context, hostID string) (*HostSecret, error)

	// GetActiveSecrets retrieves all active and deprecated (within grace period) secrets for a host.
	// This is used during secret rotation to support dual-secret validation.
	// Returns an empty slice if no secrets exist.
	GetActiveSecrets(ctx context.Context, hostID string) ([]*HostSecret, error)

	// UpdateHostSecret updates an existing host secret.
	// Returns ErrNotFound if the secret doesn't exist.
	UpdateHostSecret(ctx context.Context, secret *HostSecret) error

	// DeleteHostSecret removes a host secret.
	// Returns ErrNotFound if the secret doesn't exist.
	DeleteHostSecret(ctx context.Context, hostID string) error

	// CreateJoinToken creates a new join token for host registration.
	// Returns ErrAlreadyExists if a token for this host already exists.
	CreateJoinToken(ctx context.Context, token *HostJoinToken) error

	// GetJoinToken retrieves a join token by token hash.
	// Returns ErrNotFound if the token doesn't exist.
	GetJoinToken(ctx context.Context, tokenHash string) (*HostJoinToken, error)

	// GetJoinTokenByHostID retrieves a join token by host ID.
	// Returns ErrNotFound if the token doesn't exist.
	GetJoinTokenByHostID(ctx context.Context, hostID string) (*HostJoinToken, error)

	// DeleteJoinToken removes a join token by host ID.
	// Returns ErrNotFound if the token doesn't exist.
	DeleteJoinToken(ctx context.Context, hostID string) error

	// CleanExpiredJoinTokens removes all expired join tokens.
	CleanExpiredJoinTokens(ctx context.Context) error
}
