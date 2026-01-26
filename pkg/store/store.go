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
	Status          string
	ConnectionState string
	ContainerStatus string
	SessionStatus   string
	RuntimeState    string
	TaskSummary     string
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
	OwnerID       string
	Visibility    string
	GitRemotePrefix string
	HostID        string // Filter by contributing host
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
	UpdateRuntimeHostHeartbeat(ctx context.Context, id string, status string, resources *HostResources) error
}

// RuntimeHostFilter defines criteria for filtering runtime hosts.
type RuntimeHostFilter struct {
	Type    string
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
	Scope   string
	GroveID string
	Harness string
	OwnerID string
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
