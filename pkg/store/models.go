// Package store provides the persistence layer for the Scion Hub.
package store

import (
	"time"

	"github.com/ptone/scion-agent/pkg/api"
)

// Agent represents an agent record in the Hub database.
// This is the persistence model - for API responses, use api.AgentInfo.
type Agent struct {
	// Identity
	ID       string `json:"id"`       // UUID primary key
	AgentID  string `json:"agentId"`  // URL-safe slug identifier
	Name     string `json:"name"`     // Human-friendly display name
	Template string `json:"template"` // Template used to create this agent

	// Grove association
	GroveID string `json:"groveId"` // FK to Grove.ID

	// Metadata (stored as JSON)
	Labels      map[string]string `json:"labels,omitempty"`
	Annotations map[string]string `json:"annotations,omitempty"`

	// Status
	Status          string `json:"status"`                    // provisioning, running, stopped, error
	ConnectionState string `json:"connectionState,omitempty"` // connected, disconnected, unknown
	ContainerStatus string `json:"containerStatus,omitempty"` // Container-level status
	SessionStatus   string `json:"sessionStatus,omitempty"`   // started, waiting, completed
	RuntimeState    string `json:"runtimeState,omitempty"`    // Low-level runtime state

	// Runtime configuration
	Image         string `json:"image,omitempty"`
	Detached      bool   `json:"detached"`
	Runtime       string `json:"runtime,omitempty"`       // docker, kubernetes, apple
	RuntimeHostID string `json:"runtimeHostId,omitempty"` // FK to RuntimeHost.ID
	WebPTYEnabled bool   `json:"webPtyEnabled,omitempty"`
	TaskSummary   string `json:"taskSummary,omitempty"`

	// Applied configuration (stored as JSON)
	AppliedConfig *AgentAppliedConfig `json:"appliedConfig,omitempty"`

	// Timestamps
	Created  time.Time `json:"created"`
	Updated  time.Time `json:"updated"`
	LastSeen time.Time `json:"lastSeen,omitempty"`

	// Ownership
	CreatedBy  string `json:"createdBy,omitempty"`
	OwnerID    string `json:"ownerId,omitempty"`
	Visibility string `json:"visibility"` // private, team, public

	// Optimistic locking
	StateVersion int64 `json:"stateVersion"`
}

// AgentAppliedConfig stores the effective configuration of an agent.
type AgentAppliedConfig struct {
	Image   string            `json:"image,omitempty"`
	Harness string            `json:"harness,omitempty"`
	Env     map[string]string `json:"env,omitempty"`
	Model   string            `json:"model,omitempty"`
	Task    string            `json:"task,omitempty"` // Initial task/prompt for the agent
}

// AgentStatus constants
const (
	AgentStatusProvisioning = "provisioning"
	AgentStatusRunning      = "running"
	AgentStatusStopped      = "stopped"
	AgentStatusError        = "error"
	AgentStatusPending      = "pending"
)

// Grove represents a project/agent group in the Hub database.
type Grove struct {
	// Identity
	ID   string `json:"id"`   // UUID primary key
	Name string `json:"name"` // Human-friendly display name
	Slug string `json:"slug"` // URL-safe identifier

	// Git integration
	GitRemote string `json:"gitRemote,omitempty"` // Normalized git remote URL (unique)

	// Runtime host configuration
	// DefaultRuntimeHostID is the runtime host used when creating agents without
	// an explicit runtimeHostId. Set to the first host that registers with this grove.
	DefaultRuntimeHostID string `json:"defaultRuntimeHostId,omitempty"`

	// Metadata (stored as JSON)
	Labels      map[string]string `json:"labels,omitempty"`
	Annotations map[string]string `json:"annotations,omitempty"`

	// Timestamps
	Created time.Time `json:"created"`
	Updated time.Time `json:"updated"`

	// Ownership
	CreatedBy  string `json:"createdBy,omitempty"`
	OwnerID    string `json:"ownerId,omitempty"`
	Visibility string `json:"visibility"` // private, team, public

	// Computed fields (not stored, populated on read)
	AgentCount      int `json:"agentCount,omitempty"`
	ActiveHostCount int `json:"activeHostCount,omitempty"`
}

// RuntimeHost represents a compute node in the Hub database.
type RuntimeHost struct {
	// Identity
	ID   string `json:"id"`   // UUID primary key
	Name string `json:"name"` // Display name
	Slug string `json:"slug"` // URL-safe identifier

	// Configuration
	Type    string `json:"type"`    // docker, kubernetes, apple
	Mode    string `json:"mode"`    // connected, read-only
	Version string `json:"version"` // Scion host agent version

	// Status
	Status          string    `json:"status"`          // online, offline, degraded
	ConnectionState string    `json:"connectionState"` // connected, disconnected
	LastHeartbeat   time.Time `json:"lastHeartbeat,omitempty"`

	// Capabilities (stored as JSON)
	Capabilities       *HostCapabilities `json:"capabilities,omitempty"`
	SupportedHarnesses []string          `json:"supportedHarnesses,omitempty"`

	// Resources (stored as JSON)
	Resources *HostResources `json:"resources,omitempty"`

	// Runtimes available (stored as JSON)
	Runtimes []HostRuntime `json:"runtimes,omitempty"`

	// Metadata
	Labels      map[string]string `json:"labels,omitempty"`
	Annotations map[string]string `json:"annotations,omitempty"`

	// Network endpoint (for direct HTTP mode)
	Endpoint string `json:"endpoint,omitempty"`

	// Timestamps
	Created time.Time `json:"created"`
	Updated time.Time `json:"updated"`
}

// HostCapabilities describes what a runtime host can do.
type HostCapabilities struct {
	WebPTY bool `json:"webPty"`
	Sync   bool `json:"sync"`
	Attach bool `json:"attach"`
}

// HostResources describes resource availability on a host.
type HostResources struct {
	CPUAvailable    string `json:"cpuAvailable,omitempty"`
	MemoryAvailable string `json:"memoryAvailable,omitempty"`
	AgentsRunning   int    `json:"agentsRunning,omitempty"`
	AgentsCapacity  int    `json:"agentsCapacity,omitempty"`
}

// HostRuntime describes a container runtime available on a host.
type HostRuntime struct {
	Type      string `json:"type"`      // docker, kubernetes, apple
	Available bool   `json:"available"`
	Context   string `json:"context,omitempty"`   // K8s context
	Namespace string `json:"namespace,omitempty"` // K8s namespace
}

// GroveContributor links a runtime host to a grove.
type GroveContributor struct {
	GroveID   string    `json:"groveId"`
	HostID    string    `json:"hostId"`
	HostName  string    `json:"hostName"`
	LocalPath string    `json:"localPath,omitempty"` // Filesystem path to the grove on this host (e.g., ~/.scion or /path/to/project/.scion)
	Mode      string    `json:"mode"`                // connected, read-only
	Status    string    `json:"status"`              // online, offline
	Profiles  []string  `json:"profiles"`            // Profiles this host can execute
	LastSeen  time.Time `json:"lastSeen,omitempty"`
}

// Template represents an agent template in the Hub database.
type Template struct {
	// Identity
	ID   string `json:"id"`   // UUID primary key
	Name string `json:"name"` // Template name
	Slug string `json:"slug"` // URL-safe identifier

	// Configuration
	Harness string          `json:"harness"` // claude, gemini, opencode, codex, generic
	Image   string          `json:"image"`   // Default container image
	Config  *TemplateConfig `json:"config,omitempty"`

	// Scope
	Scope   string `json:"scope"`             // global, grove, user
	GroveID string `json:"groveId,omitempty"` // Grove association (if scope=grove)

	// Storage
	StorageURI string `json:"storageUri,omitempty"` // Remote storage URI

	// Ownership
	OwnerID    string `json:"ownerId,omitempty"`
	Visibility string `json:"visibility"` // private, team, public

	// Timestamps
	Created time.Time `json:"created"`
	Updated time.Time `json:"updated"`
}

// TemplateConfig holds template configuration details.
type TemplateConfig struct {
	Harness     string            `json:"harness,omitempty"`
	ConfigDir   string            `json:"configDir,omitempty"`
	Env         map[string]string `json:"env,omitempty"`
	Detached    bool              `json:"detached,omitempty"`
	CommandArgs []string          `json:"commandArgs,omitempty"`
	Model       string            `json:"model,omitempty"`
}

// User represents a registered user in the Hub database.
type User struct {
	// Identity
	ID          string `json:"id"` // UUID primary key
	Email       string `json:"email"`
	DisplayName string `json:"displayName"`
	AvatarURL   string `json:"avatarUrl,omitempty"`

	// Access control
	Role   string `json:"role"`   // admin, member, viewer
	Status string `json:"status"` // active, suspended

	// Preferences (stored as JSON)
	Preferences *UserPreferences `json:"preferences,omitempty"`

	// Timestamps
	Created   time.Time `json:"created"`
	LastLogin time.Time `json:"lastLogin,omitempty"`
}

// UserPreferences holds user preferences.
type UserPreferences struct {
	DefaultTemplate string `json:"defaultTemplate,omitempty"`
	DefaultProfile  string `json:"defaultProfile,omitempty"`
	Theme           string `json:"theme,omitempty"` // light, dark
}

// UserRole constants
const (
	UserRoleAdmin  = "admin"
	UserRoleMember = "member"
	UserRoleViewer = "viewer"
)

// Visibility constants - re-exported from api package for convenience.
// The api package is the canonical source for these values.
const (
	VisibilityPrivate = api.VisibilityPrivate
	VisibilityTeam    = api.VisibilityTeam
	VisibilityPublic  = api.VisibilityPublic
)

// HostMode constants
const (
	HostModeConnected = "connected"
	HostModeReadOnly  = "read-only"
)

// HostStatus constants
const (
	HostStatusOnline   = "online"
	HostStatusOffline  = "offline"
	HostStatusDegraded = "degraded"
)

// ListOptions provides pagination and filtering for list operations.
type ListOptions struct {
	Limit  int               // Maximum results
	Cursor string            // Pagination cursor (opaque string)
	Labels map[string]string // Label selectors
}

// ListResult is a generic result container for list operations.
type ListResult[T any] struct {
	Items      []T    `json:"items"`
	NextCursor string `json:"nextCursor,omitempty"`
	TotalCount int    `json:"totalCount,omitempty"`
}

// EnvVar represents an environment variable stored in the Hub database.
// Environment variables are scoped to users, groves, or runtime hosts.
type EnvVar struct {
	// Identity
	ID  string `json:"id"`  // UUID primary key
	Key string `json:"key"` // Variable name (e.g., "LOG_LEVEL")

	// Value
	Value string `json:"value"` // Variable value

	// Scope
	Scope   string `json:"scope"`   // user, grove, runtime_host
	ScopeID string `json:"scopeId"` // ID of the scoped entity

	// Metadata
	Description string `json:"description,omitempty"` // Optional description
	Sensitive   bool   `json:"sensitive,omitempty"`   // If true, value is masked in responses

	// Timestamps
	Created time.Time `json:"created"`
	Updated time.Time `json:"updated"`

	// Ownership
	CreatedBy string `json:"createdBy,omitempty"`
}

// Secret represents a secret stored in the Hub database.
// Secret values are never returned in API responses - only metadata.
type Secret struct {
	// Identity
	ID  string `json:"id"`  // UUID primary key
	Key string `json:"key"` // Secret name (e.g., "API_KEY")

	// Value (stored encrypted, never returned in API responses)
	EncryptedValue string `json:"-"` // Encrypted value (never serialized)

	// Scope
	Scope   string `json:"scope"`   // user, grove, runtime_host
	ScopeID string `json:"scopeId"` // ID of the scoped entity

	// Metadata
	Description string `json:"description,omitempty"` // Optional description
	Version     int    `json:"version"`               // Incremented on each update

	// Timestamps
	Created time.Time `json:"created"`
	Updated time.Time `json:"updated"`

	// Ownership
	CreatedBy string `json:"createdBy,omitempty"`
	UpdatedBy string `json:"updatedBy,omitempty"`
}

// Scope constants for environment variables and secrets.
const (
	ScopeUser        = "user"
	ScopeGrove       = "grove"
	ScopeRuntimeHost = "runtime_host"
)

// =============================================================================
// Conversion Functions: Store -> API
//
// These functions convert persistence models to API models for external use.
// Key ID semantics:
//   - store.Agent.ID        = UUID (database primary key)
//   - store.Agent.AgentID   = Slug (URL-safe identifier)
//   - api.AgentInfo.ID      = Container/Runtime ID (runtime-assigned, may be empty for hosted mode)
//   - api.AgentInfo.AgentID = Slug (same as store.Agent.AgentID)
// =============================================================================

// ToAPI converts a store.Agent to an api.AgentInfo for external consumption.
// Note: The api.AgentInfo.ID field is intentionally left empty because in the
// hosted context, the runtime container ID is not available at the Hub level.
// Clients should use AgentID (slug) for identification.
func (a *Agent) ToAPI() *api.AgentInfo {
	info := &api.AgentInfo{
		// Identity - Note: we do NOT set api.AgentInfo.ID here because it represents
		// a container/runtime ID which is not known at the Hub level.
		AgentID:  a.AgentID,
		Name:     a.Name,
		Template: a.Template,

		// Grove association - use the hosted format (uuid__slug)
		GroveID: a.GroveID,

		// Metadata
		Labels:      a.Labels,
		Annotations: a.Annotations,

		// Status
		Status:          a.Status,
		ContainerStatus: a.ContainerStatus,
		SessionStatus:   a.SessionStatus,
		RuntimeState:    a.RuntimeState,

		// Runtime configuration
		Image:         a.Image,
		Detached:      a.Detached,
		Runtime:       a.Runtime,
		RuntimeHostID: a.RuntimeHostID,
		WebPTYEnabled: a.WebPTYEnabled,
		TaskSummary:   a.TaskSummary,

		// Timestamps
		Created:  a.Created,
		Updated:  a.Updated,
		LastSeen: a.LastSeen,

		// Ownership
		CreatedBy:  a.CreatedBy,
		OwnerID:    a.OwnerID,
		Visibility: a.Visibility,

		// Optimistic locking
		StateVersion: a.StateVersion,
	}

	// Populate applied config fields if available
	if a.AppliedConfig != nil {
		if info.Image == "" {
			info.Image = a.AppliedConfig.Image
		}
	}

	return info
}

// ToAPI converts a store.Grove to an api.GroveInfo for external consumption.
func (g *Grove) ToAPI() *api.GroveInfo {
	return &api.GroveInfo{
		ID:   g.ID,
		Name: g.Name,
		Slug: g.Slug,

		// Timestamps
		Created: g.Created,
		Updated: g.Updated,

		// Ownership
		CreatedBy:  g.CreatedBy,
		OwnerID:    g.OwnerID,
		Visibility: g.Visibility,

		// Metadata
		Labels:      g.Labels,
		Annotations: g.Annotations,

		// Statistics
		AgentCount: g.AgentCount,
	}
}
