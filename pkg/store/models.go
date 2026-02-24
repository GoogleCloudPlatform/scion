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

// Package store provides the persistence layer for the Scion Hub.
package store

import (
	"strings"
	"time"

	"github.com/ptone/scion-agent/pkg/api"
)

// Agent represents an agent record in the Hub database.
// This is the persistence model - for API responses, use api.AgentInfo.
type Agent struct {
	// Identity
	ID       string `json:"id"`       // UUID primary key
	Slug     string `json:"slug"`     // URL-safe slug identifier (unique per grove)
	Name     string `json:"name"`     // Human-friendly display name
	Template string `json:"template"` // Template used to create this agent

	// Grove association
	GroveID string `json:"groveId"` // FK to Grove.ID

	// Metadata (stored as JSON)
	Labels      map[string]string `json:"labels,omitempty"`
	Annotations map[string]string `json:"annotations,omitempty"`

	// Status
	Status          string `json:"status"`                    // provisioning, running, stopped, error, busy, idle, waiting_for_input, completed
	ConnectionState string `json:"connectionState,omitempty"` // connected, disconnected, unknown
	ContainerStatus string `json:"containerStatus,omitempty"` // Container-level status
	RuntimeState    string `json:"runtimeState,omitempty"`    // Low-level runtime state

	// Runtime configuration
	Image           string `json:"image,omitempty"`
	Detached        bool   `json:"detached"`
	Runtime         string `json:"runtime,omitempty"`           // docker, kubernetes, apple
	RuntimeBrokerID string `json:"runtimeBrokerId,omitempty"`   // FK to RuntimeBroker.ID
	WebPTYEnabled   bool   `json:"webPtyEnabled,omitempty"`
	TaskSummary     string `json:"taskSummary,omitempty"`
	Message         string `json:"message,omitempty"`

	// Enriched fields (populated by Hub when returning data, not persisted)
	Grove             string `json:"grove,omitempty"`             // Grove name (resolved from GroveID)
	RuntimeBrokerName string `json:"runtimeBrokerName,omitempty"` // Broker name (resolved from RuntimeBrokerID)
	HarnessConfig     string `json:"harnessConfig,omitempty"`     // Harness type (resolved from AppliedConfig.Harness)

	// Applied configuration (stored as JSON)
	AppliedConfig *AgentAppliedConfig `json:"appliedConfig,omitempty"`

	// Timestamps
	Created   time.Time `json:"created"`
	Updated   time.Time `json:"updated"`
	LastSeen  time.Time `json:"lastSeen,omitempty"`
	DeletedAt time.Time `json:"deletedAt,omitempty"`

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
	Profile string            `json:"profile,omitempty"`   // Settings profile for the runtime broker
	Task    string            `json:"task,omitempty"`      // Initial task/prompt for the agent
	Attach  bool              `json:"attach,omitempty"`    // If true, signals interactive attach mode to the broker/harness
	Workspace string          `json:"workspace,omitempty"` // Host path to mount as /workspace (overrides default grove root)
	GitClone  *api.GitCloneConfig `json:"gitClone,omitempty"`

	// Template info for Runtime Broker hydration
	TemplateID   string `json:"templateId,omitempty"`   // Hub template ID for fetching
	TemplateHash string `json:"templateHash,omitempty"` // Content hash for cache validation

	// CreatorName is the human-readable identity of who created this agent.
	// For user-created agents, this is the user's email.
	// For agent-created sub-agents, this is the creating agent's name.
	CreatorName string `json:"creatorName,omitempty"`

	// Hub access scopes granted to the agent (from template HubAccess config)
	HubAccessScopes []string `json:"hubAccessScopes,omitempty"`

	// WorkspaceStoragePath is the GCS storage path for bootstrapped workspaces.
	// Set during workspace bootstrap for non-git groves.
	WorkspaceStoragePath string `json:"workspaceStoragePath,omitempty"`
}

// AgentStatus constants
const (
	AgentStatusCreated         = "created"
	AgentStatusCloning         = "cloning"
	AgentStatusProvisioning    = "provisioning"
	AgentStatusRunning         = "running"
	AgentStatusStopped         = "stopped"
	AgentStatusError           = "error"
	AgentStatusPending         = "pending"
	AgentStatusBusy            = "busy"
	AgentStatusIdle            = "idle"
	AgentStatusWaitingForInput = "waiting_for_input"
	AgentStatusCompleted       = "completed"
	AgentStatusDeleted         = "deleted"
	AgentStatusRestored        = "restored"
)

// Grove represents a project/agent group in the Hub database.
type Grove struct {
	// Identity
	ID   string `json:"id"`   // UUID primary key
	Name string `json:"name"` // Human-friendly display name
	Slug string `json:"slug"` // URL-safe identifier

	// Git integration
	GitRemote string `json:"gitRemote,omitempty"` // Normalized git remote URL (unique)

	// Runtime broker configuration
	// DefaultRuntimeBrokerID is the runtime broker used when creating agents without
	// an explicit runtimeBrokerId. Set to the first broker that registers with this grove.
	DefaultRuntimeBrokerID string `json:"defaultRuntimeBrokerId,omitempty"`

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
	ActiveBrokerCount int `json:"activeBrokerCount,omitempty"`
}

// RuntimeBroker represents a compute node in the Hub database.
type RuntimeBroker struct {
	// Identity
	ID   string `json:"id"`   // UUID primary key
	Name string `json:"name"` // Display name
	Slug string `json:"slug"` // URL-safe identifier

	// Configuration
	Version string `json:"version"` // Scion broker agent version

	// Status
	Status          string    `json:"status"`          // online, offline, degraded
	ConnectionState string    `json:"connectionState"` // connected, disconnected
	LastHeartbeat   time.Time `json:"lastHeartbeat,omitempty"`

	// Capabilities (stored as JSON)
	Capabilities *BrokerCapabilities `json:"capabilities,omitempty"`

	// Profiles available (stored as JSON)
	Profiles []BrokerProfile `json:"profiles,omitempty"`

	// Metadata
	Labels      map[string]string `json:"labels,omitempty"`
	Annotations map[string]string `json:"annotations,omitempty"`

	// Network endpoint (for direct HTTP mode)
	Endpoint string `json:"endpoint,omitempty"`

	// Auto-provide configuration
	// When true, this broker is automatically added as a provider for new groves
	AutoProvide bool `json:"autoProvide,omitempty"`

	// Timestamps
	Created time.Time `json:"created"`
	Updated time.Time `json:"updated"`

	// Ownership - tracks who registered this broker
	CreatedBy string `json:"createdBy,omitempty"`
}

// BrokerCapabilities describes what a runtime broker can do.
type BrokerCapabilities struct {
	WebPTY bool `json:"webPty"`
	Sync   bool `json:"sync"`
	Attach bool `json:"attach"`
}

// BrokerProfile describes a runtime profile available on a broker.
type BrokerProfile struct {
	Name      string `json:"name"`      // Profile name (e.g., "docker-default", "k8s-prod")
	Type      string `json:"type"`      // docker, kubernetes, apple
	Available bool   `json:"available"`
	Context   string `json:"context,omitempty"`   // K8s context
	Namespace string `json:"namespace,omitempty"` // K8s namespace
}

// GroveProvider links a runtime broker to a grove.
type GroveProvider struct {
	GroveID    string    `json:"groveId"`
	BrokerID   string    `json:"brokerId"`
	BrokerName string    `json:"brokerName"`
	LocalPath  string    `json:"localPath,omitempty"` // Filesystem path to the grove on this broker (e.g., ~/.scion or /path/to/project/.scion)
	Status     string    `json:"status"`              // online, offline
	LastSeen   time.Time `json:"lastSeen,omitempty"`

	// Ownership - tracks who linked this broker to the grove
	LinkedBy string    `json:"linkedBy,omitempty"` // User ID who performed the link
	LinkedAt time.Time `json:"linkedAt,omitempty"` // Timestamp when the link was created
}

// Template represents an agent template in the Hub database.
type Template struct {
	// Identity
	ID          string `json:"id"`                    // UUID primary key
	Name        string `json:"name"`                  // Template name (e.g., "claude", "custom-gemini")
	Slug        string `json:"slug"`                  // URL-safe identifier
	DisplayName string `json:"displayName,omitempty"` // Human-friendly name
	Description string `json:"description,omitempty"` // Optional description

	// Configuration
	Harness string          `json:"harness"` // claude, gemini, opencode, codex, generic
	Image   string          `json:"image"`   // Default container image
	Config  *TemplateConfig `json:"config,omitempty"`

	// Content tracking
	ContentHash string `json:"contentHash,omitempty"` // SHA-256 hash of template contents

	// Scope
	Scope   string `json:"scope"`             // global, grove, user
	ScopeID string `json:"scopeId,omitempty"` // groveId or userId (null for global)
	GroveID string `json:"groveId,omitempty"` // Grove association (if scope=grove) - deprecated, use ScopeID

	// Storage
	StorageURI    string `json:"storageUri,omitempty"`    // Full bucket URI (e.g., "gs://bucket/templates/path/")
	StorageBucket string `json:"storageBucket,omitempty"` // Bucket name
	StoragePath   string `json:"storagePath,omitempty"`   // Path within bucket

	// File manifest
	Files []TemplateFile `json:"files,omitempty"` // Manifest of template files

	// Inheritance
	BaseTemplate string `json:"baseTemplate,omitempty"` // Parent template ID (for inheritance)

	// Protection
	Locked bool   `json:"locked,omitempty"` // Prevent modifications (global templates)
	Status string `json:"status"`           // pending, active, archived

	// Ownership
	OwnerID    string `json:"ownerId,omitempty"`
	CreatedBy  string `json:"createdBy,omitempty"`
	UpdatedBy  string `json:"updatedBy,omitempty"`
	Visibility string `json:"visibility"` // private, grove, public

	// Timestamps
	Created time.Time `json:"created"`
	Updated time.Time `json:"updated"`
}

// TemplateFile represents a file within a template.
type TemplateFile struct {
	Path string `json:"path"`           // Relative path (e.g., "home/.bashrc")
	Size int64  `json:"size"`           // File size in bytes
	Hash string `json:"hash"`           // SHA-256 hash of file
	Mode string `json:"mode,omitempty"` // File permissions (e.g., "0644")
}

// TemplateStatus constants
const (
	TemplateStatusPending  = "pending"
	TemplateStatusActive   = "active"
	TemplateStatusArchived = "archived"
)

// TemplateScope constants
const (
	TemplateScopeGlobal = "global"
	TemplateScopeGrove  = "grove"
	TemplateScopeUser   = "user"
)

// HarnessConfig represents a harness configuration in the Hub database.
type HarnessConfig struct {
	// Identity
	ID          string `json:"id"`                    // UUID primary key
	Name        string `json:"name"`                  // Harness config name (e.g., "claude", "gemini-experimental")
	Slug        string `json:"slug"`                  // URL-safe identifier
	DisplayName string `json:"displayName,omitempty"` // Human-friendly name
	Description string `json:"description,omitempty"` // Optional description

	// Configuration
	Harness string             `json:"harness"` // claude, gemini, opencode, codex, generic
	Config  *HarnessConfigData `json:"config,omitempty"`

	// Content tracking
	ContentHash string `json:"contentHash,omitempty"` // SHA-256 hash of harness config contents

	// Scope
	Scope   string `json:"scope"`             // global, grove, user
	ScopeID string `json:"scopeId,omitempty"` // groveId or userId (null for global)

	// Storage
	StorageURI    string `json:"storageUri,omitempty"`    // Full bucket URI (e.g., "gs://bucket/harness-configs/path/")
	StorageBucket string `json:"storageBucket,omitempty"` // Bucket name
	StoragePath   string `json:"storagePath,omitempty"`   // Path within bucket

	// File manifest
	Files []TemplateFile `json:"files,omitempty"` // Manifest of harness config files (reuses TemplateFile)

	// Protection
	Locked bool   `json:"locked,omitempty"` // Prevent modifications
	Status string `json:"status"`           // pending, active, archived

	// Ownership
	OwnerID    string `json:"ownerId,omitempty"`
	CreatedBy  string `json:"createdBy,omitempty"`
	UpdatedBy  string `json:"updatedBy,omitempty"`
	Visibility string `json:"visibility"` // private, grove, public

	// Timestamps
	Created time.Time `json:"created"`
	Updated time.Time `json:"updated"`
}

// HarnessConfigData holds the harness-specific configuration details.
type HarnessConfigData struct {
	Harness          string            `json:"harness,omitempty"`
	Image            string            `json:"image,omitempty"`
	User             string            `json:"user,omitempty"`
	Model            string            `json:"model,omitempty"`
	Args             []string          `json:"args,omitempty"`
	Env              map[string]string `json:"env,omitempty"`
	AuthSelectedType string            `json:"authSelectedType,omitempty"`
	Secrets          []api.RequiredSecret `json:"secrets,omitempty"`
}

// HarnessConfigStatus constants
const (
	HarnessConfigStatusPending  = "pending"
	HarnessConfigStatusActive   = "active"
	HarnessConfigStatusArchived = "archived"
)

// HarnessConfigScope constants
const (
	HarnessConfigScopeGlobal = "global"
	HarnessConfigScopeGrove  = "grove"
	HarnessConfigScopeUser   = "user"
)

// TemplateConfig holds template configuration details.
type TemplateConfig struct {
	Harness     string            `json:"harness,omitempty"`
	Image       string            `json:"image,omitempty"`
	ConfigDir   string            `json:"configDir,omitempty"`
	Env         map[string]string `json:"env,omitempty"`
	Detached    bool              `json:"detached,omitempty"`
	CommandArgs []string          `json:"commandArgs,omitempty"`
	Model       string            `json:"model,omitempty"`
	Kubernetes  *KubernetesConfig `json:"kubernetes,omitempty"`
	HubAccess   *HubAccessConfig  `json:"hubAccess,omitempty"`
	Secrets     []api.RequiredSecret `json:"secrets,omitempty"`
}

// HubAccessConfig defines what Hub API scopes an agent created from this template receives.
type HubAccessConfig struct {
	Scopes []string `json:"scopes,omitempty"`
}

// KubernetesConfig holds Kubernetes-specific configuration for templates.
type KubernetesConfig struct {
	Resources *ResourceRequirements `json:"resources,omitempty"`
	NodeSelector map[string]string  `json:"nodeSelector,omitempty"`
}

// ResourceRequirements defines compute resource requirements.
type ResourceRequirements struct {
	Limits   map[string]string `json:"limits,omitempty"`
	Requests map[string]string `json:"requests,omitempty"`
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

// =============================================================================
// Broker Authentication (Runtime Broker HMAC Authentication)
// =============================================================================

// BrokerSecret stores the HMAC shared secret for a Runtime Broker.
type BrokerSecret struct {
	BrokerID string    `json:"brokerId"`
	SecretKey []byte    `json:"-"` // Never serialize - stored encrypted at rest
	Algorithm string    `json:"algorithm"` // "hmac-sha256"
	CreatedAt time.Time `json:"createdAt"`
	RotatedAt time.Time `json:"rotatedAt,omitempty"`
	ExpiresAt time.Time `json:"expiresAt,omitempty"`
	Status    string    `json:"status"` // active, deprecated, revoked
}

// BrokerSecretStatus constants
const (
	BrokerSecretStatusActive     = "active"
	BrokerSecretStatusDeprecated = "deprecated"
	BrokerSecretStatusRevoked    = "revoked"
)

// BrokerSecretAlgorithm constants
const (
	BrokerSecretAlgorithmHMACSHA256 = "hmac-sha256"
)

// BrokerJoinToken is a short-lived token for broker registration.
type BrokerJoinToken struct {
	BrokerID string    `json:"brokerId"`
	TokenHash string    `json:"-"` // SHA-256 hash of token (never exposed)
	ExpiresAt time.Time `json:"expiresAt"`
	CreatedAt time.Time `json:"createdAt"`
	CreatedBy string    `json:"createdBy"` // User ID who created the token
}

// BrokerStatus constants
const (
	BrokerStatusOnline   = "online"
	BrokerStatusOffline  = "offline"
	BrokerStatusDegraded = "degraded"
)

// =============================================================================
// Notifications (Agent Status Notification System)
// =============================================================================

// SubscriberType constants define what kind of entity receives notifications.
const (
	SubscriberTypeAgent = "agent"
	SubscriberTypeUser  = "user"
)

// NotificationSubscription represents a subscription to agent status changes.
type NotificationSubscription struct {
	ID             string   `json:"id"`             // UUID primary key
	AgentID        string   `json:"agentId"`        // Agent being watched
	SubscriberType string   `json:"subscriberType"` // "agent" or "user"
	SubscriberID   string   `json:"subscriberId"`   // Slug or ID of the subscriber
	GroveID        string   `json:"groveId"`        // Grove scope
	TriggerStatuses []string `json:"triggerStatuses"` // e.g. ["COMPLETED", "WAITING_FOR_INPUT"]
	CreatedAt      time.Time `json:"createdAt"`
	CreatedBy      string   `json:"createdBy"`
}

// MatchesStatus returns true if the given status matches any of the subscription's
// trigger statuses. Comparison is case-insensitive.
func (s *NotificationSubscription) MatchesStatus(status string) bool {
	normalized := strings.ToUpper(status)
	for _, trigger := range s.TriggerStatuses {
		if strings.ToUpper(trigger) == normalized {
			return true
		}
	}
	return false
}

// Notification represents a notification record generated from a subscription match.
type Notification struct {
	ID             string    `json:"id"`             // UUID primary key
	SubscriptionID string    `json:"subscriptionId"` // FK to NotificationSubscription
	AgentID        string    `json:"agentId"`        // Agent that triggered the notification
	GroveID        string    `json:"groveId"`
	SubscriberType string    `json:"subscriberType"` // "agent" or "user"
	SubscriberID   string    `json:"subscriberId"`
	Status         string    `json:"status"`         // Trigger status (UPPER CASE)
	Message        string    `json:"message"`
	Dispatched     bool      `json:"dispatched"`     // Whether dispatch was attempted
	Acknowledged   bool      `json:"acknowledged"`   // Whether acknowledged (for human targets)
	CreatedAt      time.Time `json:"createdAt"`
}

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
// Environment variables are scoped to users, groves, or runtime brokers.
type EnvVar struct {
	// Identity
	ID  string `json:"id"`  // UUID primary key
	Key string `json:"key"` // Variable name (e.g., "LOG_LEVEL")

	// Value
	Value string `json:"value"` // Variable value

	// Scope
	Scope   string `json:"scope"`   // user, grove, runtime_broker
	ScopeID string `json:"scopeId"` // ID of the scoped entity

	// Metadata
	Description   string `json:"description,omitempty"`   // Optional description
	Sensitive     bool   `json:"sensitive,omitempty"`     // If true, value is masked in responses
	InjectionMode string `json:"injectionMode,omitempty"` // "always" or "as_needed" (default: "as_needed")
	Secret        bool   `json:"secret,omitempty"`        // If true, value is encrypted and never returned

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

	// External reference (e.g., "gcpsm:projects/123/secrets/name" for GCP SM backend)
	SecretRef string `json:"secretRef,omitempty"` // External secret reference

	// Type and Target
	SecretType string `json:"type"`             // environment, variable, file (default: environment)
	Target     string `json:"target,omitempty"` // Projection target: env var name, json key, or file path

	// Scope
	Scope   string `json:"scope"`   // user, grove, runtime_broker
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

// SecretType constants define how a secret is projected into the agent container.
const (
	SecretTypeEnvironment = "environment" // Injected as environment variable (default)
	SecretTypeVariable    = "variable"    // Written to ~/.scion/secrets.json for programmatic access
	SecretTypeFile        = "file"        // Written to a file at the specified Target path
)

// Scope constants for environment variables and secrets.
const (
	ScopeHub         = "hub"
	ScopeUser        = "user"
	ScopeGrove       = "grove"
	ScopeRuntimeBroker = "runtime_broker"
)

// InjectionMode constants for environment variables.
const (
	InjectionModeAlways   = "always"
	InjectionModeAsNeeded = "as_needed"
)

// =============================================================================
// Groups and Policies (Hub Permissions System)
// =============================================================================

// Group represents a user group in the Hub database.
// Groups support hierarchical membership through nested groups.
type Group struct {
	// Identity
	ID          string `json:"id"`          // UUID primary key
	Name        string `json:"name"`        // Human-friendly display name
	Slug        string `json:"slug"`        // URL-safe identifier
	Description string `json:"description,omitempty"`
	GroupType   string `json:"groupType,omitempty"` // "explicit" or "grove_agents"
	GroveID     string `json:"groveId,omitempty"`   // FK to Grove.ID (for grove_agents groups)

	// Hierarchy
	ParentID string `json:"parentId,omitempty"` // Optional parent group for hierarchy

	// Metadata (stored as JSON)
	Labels      map[string]string `json:"labels,omitempty"`
	Annotations map[string]string `json:"annotations,omitempty"`

	// Timestamps
	Created time.Time `json:"created"`
	Updated time.Time `json:"updated"`

	// Ownership
	CreatedBy string `json:"createdBy,omitempty"`
	OwnerID   string `json:"ownerId,omitempty"`
}

// GroupMember represents membership in a group.
// Members can be either users or other groups (for nested group support).
type GroupMember struct {
	GroupID    string    `json:"groupId"`    // The group this membership belongs to
	MemberType string    `json:"memberType"` // "user" or "group"
	MemberID   string    `json:"memberId"`   // User ID or Group ID
	Role       string    `json:"role"`       // "member", "admin", "owner"
	AddedAt    time.Time `json:"addedAt"`
	AddedBy    string    `json:"addedBy,omitempty"`
}

// GroupMemberType constants
const (
	GroupMemberTypeUser  = "user"
	GroupMemberTypeGroup = "group"
	GroupMemberTypeAgent = "agent"
)

// GroupType constants
const (
	GroupTypeExplicit   = "explicit"
	GroupTypeGroveAgents = "grove_agents"
)

// PolicyPrincipalType agent constant
const (
	PolicyPrincipalTypeAgent = "agent"
)

// GroupMemberRole constants
const (
	GroupMemberRoleMember = "member"
	GroupMemberRoleAdmin  = "admin"
	GroupMemberRoleOwner  = "owner"
)

// Policy defines access control rules in the Hub.
// Policies specify what actions are allowed or denied on resources.
type Policy struct {
	// Identity
	ID          string `json:"id"`                    // UUID primary key
	Name        string `json:"name"`                  // Human-friendly name
	Description string `json:"description,omitempty"` // Detailed description

	// Scope
	ScopeType string `json:"scopeType"` // "hub", "grove", "resource"
	ScopeID   string `json:"scopeId"`   // ID of the scoped entity (empty for hub scope)

	// Resource targeting
	ResourceType string `json:"resourceType"`         // "*" for all, or specific type (agent, grove, etc.)
	ResourceID   string `json:"resourceId,omitempty"` // Specific resource ID (optional)

	// Permissions
	Actions []string `json:"actions"` // Actions like "read", "write", "delete", "*"
	Effect  string   `json:"effect"`  // "allow" or "deny"

	// Conditions (stored as JSON)
	Conditions *PolicyConditions `json:"conditions,omitempty"`

	// Priority for conflict resolution (higher = evaluated first)
	Priority int `json:"priority"`

	// Metadata (stored as JSON)
	Labels      map[string]string `json:"labels,omitempty"`
	Annotations map[string]string `json:"annotations,omitempty"`

	// Timestamps
	Created time.Time `json:"created"`
	Updated time.Time `json:"updated"`

	// Ownership
	CreatedBy string `json:"createdBy,omitempty"`
}

// DelegatedFromCondition specifies a delegation source for policy matching.
// When set on a policy, the policy applies to agents whose creator matches.
type DelegatedFromCondition struct {
	PrincipalType string `json:"principalType"` // "user"
	PrincipalID   string `json:"principalId"`   // User UUID
}

// PolicyConditions provides optional conditional logic for policies.
type PolicyConditions struct {
	Labels             map[string]string        `json:"labels,omitempty"`             // Resource must have these labels
	ValidFrom          *time.Time               `json:"validFrom,omitempty"`          // Policy valid from this time
	ValidUntil         *time.Time               `json:"validUntil,omitempty"`         // Policy valid until this time
	SourceIPs          []string                 `json:"sourceIps,omitempty"`          // Allowed source IP ranges (CIDR)
	DelegatedFrom      *DelegatedFromCondition  `json:"delegatedFrom,omitempty"`      // Match agents delegated from a specific principal
	DelegatedFromGroup string                   `json:"delegatedFromGroup,omitempty"` // Match agents whose creator is in this group
}

// PolicyEffect constants
const (
	PolicyEffectAllow = "allow"
	PolicyEffectDeny  = "deny"
)

// PolicyScopeType constants
const (
	PolicyScopeHub      = "hub"
	PolicyScopeGrove    = "grove"
	PolicyScopeResource = "resource"
)

// PolicyBinding links a principal (user or group) to a policy.
type PolicyBinding struct {
	PolicyID      string `json:"policyId"`
	PrincipalType string `json:"principalType"` // "user" or "group"
	PrincipalID   string `json:"principalId"`
}

// PolicyPrincipalType constants
const (
	PolicyPrincipalTypeUser  = "user"
	PolicyPrincipalTypeGroup = "group"
)

// =============================================================================
// API Keys
// =============================================================================

// APIKey represents an API key stored in the Hub database.
// API keys are used for programmatic access to the Hub API.
type APIKey struct {
	// Identity
	ID     string `json:"id"`     // UUID primary key
	UserID string `json:"userId"` // FK to User.ID

	// Key metadata
	Name   string `json:"name"`   // User-provided name for the key
	Prefix string `json:"prefix"` // First 8 chars for identification (sk_live_XXXXXXXX)

	// Security - the hash is never exposed in API responses
	KeyHash string `json:"-"` // SHA-256 hash of the full key

	// Scopes limit what the key can access
	Scopes []string `json:"scopes,omitempty"`

	// Status
	Revoked   bool       `json:"revoked"`
	ExpiresAt *time.Time `json:"expiresAt,omitempty"`
	LastUsed  *time.Time `json:"lastUsed,omitempty"`

	// Timestamps
	Created time.Time `json:"created"`
}

// APIKeyPrefix constants
const (
	APIKeyPrefixLive = "sk_live_"
	APIKeyPrefixTest = "sk_test_"
)

// =============================================================================
// Conversion Functions: Store -> API
//
// These functions convert persistence models to API models for external use.
// Key ID semantics:
//   - store.Agent.ID   = UUID (database primary key, globally unique)
//   - store.Agent.Slug = URL-safe identifier (unique per grove)
//   - api.AgentInfo.ID   = Hub UUID (same as store.Agent.ID)
//   - api.AgentInfo.Slug = URL-safe identifier (same as store.Agent.Slug)
//   - api.AgentInfo.ContainerID = Runtime container ID (ephemeral, runtime-assigned)
// =============================================================================

// ToAPI converts a store.Agent to an api.AgentInfo for external consumption.
func (a *Agent) ToAPI() *api.AgentInfo {
	info := &api.AgentInfo{
		// Identity
		ID:       a.ID,
		Slug:     a.Slug,
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
		RuntimeState:    a.RuntimeState,

		// Runtime configuration
		Image:         a.Image,
		Detached:      a.Detached,
		Runtime:       a.Runtime,
		RuntimeBrokerID: a.RuntimeBrokerID,
		WebPTYEnabled: a.WebPTYEnabled,
		TaskSummary:   a.TaskSummary,

		// Timestamps
		Created:   a.Created,
		Updated:   a.Updated,
		LastSeen:  a.LastSeen,
		DeletedAt: a.DeletedAt,

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
		if info.HarnessConfig == "" && a.AppliedConfig.Harness != "" {
			info.HarnessConfig = a.AppliedConfig.Harness
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
