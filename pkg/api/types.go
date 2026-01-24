package api

import (
	"context"
	"time"
)

type AgentK8sMetadata struct {
	Cluster   string `json:"cluster"`
	Namespace string `json:"namespace"`
	PodName   string `json:"podName"`
	SyncedAt  string `json:"syncedAt,omitempty"`
}

type VolumeMount struct {
	Source   string `json:"source" yaml:"source"`
	Target   string `json:"target" yaml:"target"`
	ReadOnly bool   `json:"read_only,omitempty" yaml:"read_only,omitempty"`
	Type     string `json:"type,omitempty" yaml:"type,omitempty"`     // "local" (default) or "gcs"
	Bucket   string `json:"bucket,omitempty" yaml:"bucket,omitempty"` // For GCS
	Prefix   string `json:"prefix,omitempty" yaml:"prefix,omitempty"` // For GCS
	Mode     string `json:"mode,omitempty" yaml:"mode,omitempty"`     // Mount options
}

type KubernetesConfig struct {
	Context            string        `json:"context,omitempty" yaml:"context,omitempty"`
	Namespace          string        `json:"namespace,omitempty" yaml:"namespace,omitempty"`
	RuntimeClassName   string        `json:"runtimeClassName,omitempty" yaml:"runtimeClassName,omitempty"`
	ServiceAccountName string        `json:"serviceAccountName,omitempty" yaml:"serviceAccountName,omitempty"` // For Workload Identity
	Resources          *K8sResources `json:"resources,omitempty" yaml:"resources,omitempty"`
}

type K8sResources struct {
	Requests map[string]string `json:"requests,omitempty" yaml:"requests,omitempty"`
	Limits   map[string]string `json:"limits,omitempty" yaml:"limits,omitempty"`
}

type GeminiConfig struct {
	AuthSelectedType string `json:"auth_selectedType,omitempty" yaml:"auth_selectedType,omitempty"`
}

type ScionConfig struct {
	Harness     string            `json:"harness,omitempty" yaml:"harness,omitempty"`
	ConfigDir   string            `json:"config_dir,omitempty" yaml:"config_dir,omitempty"`
	Env         map[string]string `json:"env,omitempty" yaml:"env,omitempty"`
	Volumes     []VolumeMount     `json:"volumes,omitempty" yaml:"volumes,omitempty"`
	Detached    *bool             `json:"detached" yaml:"detached"`
	CommandArgs []string          `json:"command_args,omitempty" yaml:"command_args,omitempty"`
	Model       string            `json:"model,omitempty" yaml:"model,omitempty"`
	Kubernetes  *KubernetesConfig `json:"kubernetes,omitempty" yaml:"kubernetes,omitempty"`
	Gemini      *GeminiConfig     `json:"gemini,omitempty" yaml:"gemini,omitempty"`
	Image       string            `json:"image,omitempty" yaml:"image,omitempty"`

	// Info contains persisted metadata about the agent
	Info *AgentInfo `json:"-" yaml:"-"`
}

func (c *ScionConfig) IsDetached() bool {
	if c.Detached == nil {
		return true
	}
	return *c.Detached
}

type AuthConfig struct {
	GeminiAPIKey         string
	GoogleAPIKey         string
	VertexAPIKey         string
	GoogleAppCredentials string
	GoogleCloudProject   string
	OAuthCreds           string
	AnthropicAPIKey      string
	OpenCodeAuthFile     string
	CodexAuthFile        string
	SelectedType         string
}

type AuthProvider interface {
	GetAuthConfig(context.Context) (AuthConfig, error)
}

// AgentInfo contains metadata about a scion agent.
// It supports both local/solo mode and hosted/distributed mode.
type AgentInfo struct {
	// Identity fields
	ID       string `json:"id,omitempty"`       // Container/runtime ID (legacy, runtime-assigned)
	AgentID  string `json:"agentId,omitempty"`  // URL-safe slug identifier for the agent
	Name     string `json:"name"`               // Human-friendly display name
	Template string `json:"template"`

	// Grove association
	Grove     string `json:"grove"`               // Grove name (legacy, simple string)
	GroveID   string `json:"groveId,omitempty"`   // Hosted format: <uuid>__<name>
	GrovePath string `json:"grovePath,omitempty"` // Filesystem path (solo mode)

	// Metadata
	Labels      map[string]string `json:"labels,omitempty"`
	Annotations map[string]string `json:"annotations,omitempty"`

	// Status fields
	ContainerStatus string `json:"containerStatus,omitempty"` // Container status (e.g., Up 2 hours)
	Status          string `json:"status,omitempty"`          // Scion agent high-level status (e.g., running, stopped)
	SessionStatus   string `json:"sessionStatus,omitempty"`   // Agent session status (e.g., started, waiting, completed)

	// Runtime configuration
	Image      string            `json:"image,omitempty"`
	Detached   bool              `json:"detached,omitempty"`
	Runtime    string            `json:"runtime,omitempty"`
	Profile    string            `json:"profile,omitempty"`
	Kubernetes *AgentK8sMetadata `json:"kubernetes,omitempty"`
	Warnings   []string          `json:"warnings,omitempty"`

	// Timestamps
	Created  time.Time `json:"created,omitempty"`  // When the agent was created
	Updated  time.Time `json:"updated,omitempty"`  // Last modification timestamp
	LastSeen time.Time `json:"lastSeen,omitempty"` // Last heartbeat/status report

	// Ownership & access
	CreatedBy  string `json:"createdBy,omitempty"`  // User/system that created the agent
	OwnerID    string `json:"ownerId,omitempty"`    // Current owner user ID
	Visibility string `json:"visibility,omitempty"` // Access level: private, team, public

	// Hosted/distributed mode fields
	RuntimeHostID   string `json:"runtimeHostId,omitempty"`   // ID of the Runtime Host managing this agent
	RuntimeHostType string `json:"runtimeHostType,omitempty"` // Type: docker, kubernetes, apple
	RuntimeState    string `json:"runtimeState,omitempty"`    // Low-level runtime state
	HubEndpoint     string `json:"hubEndpoint,omitempty"`     // Scion Hub URL if connected
	WebPTYEnabled   bool   `json:"webPtyEnabled,omitempty"`   // Whether web terminal access is available
	TaskSummary     string `json:"taskSummary,omitempty"`     // Current task description (for dashboard)

	// Optimistic locking
	StateVersion int64 `json:"stateVersion,omitempty"` // Version for concurrent update detection
}

type StartOptions struct {
	Name      string
	Task      string
	Template  string
	Profile   string
	Image     string
	GrovePath string
	Env       map[string]string
	Detached  *bool
	Resume    bool
	Auth      AuthProvider
	NoAuth    bool
	Branch    string
	Workspace string
}

type StatusEvent struct {
	AgentID   string `json:"agent_id"`
	Status    string `json:"status"`
	Message   string `json:"message,omitempty"`
	Timestamp string `json:"timestamp"`
}

// Visibility constants for agent and grove access control.
const (
	VisibilityPrivate = "private" // Only the owner can access
	VisibilityTeam    = "team"    // Team members can access
	VisibilityPublic  = "public"  // Anyone can access (read-only)
)

// GroveInfo contains metadata about a grove (project/agent group).
// It supports both local/solo mode and hosted/distributed mode.
type GroveInfo struct {
	// Identity fields
	ID   string `json:"id,omitempty"` // UUID (hosted) or empty (solo)
	Name string `json:"name"`         // Human-friendly display name
	Slug string `json:"slug"`         // URL-safe identifier

	// Location
	Path string `json:"path,omitempty"` // Filesystem path (solo mode)

	// Timestamps
	Created time.Time `json:"created,omitempty"` // When the grove was created
	Updated time.Time `json:"updated,omitempty"` // Last modification timestamp

	// Ownership
	CreatedBy  string `json:"createdBy,omitempty"`  // User/system that created the grove
	OwnerID    string `json:"ownerId,omitempty"`    // Current owner user ID
	Visibility string `json:"visibility,omitempty"` // Access level: private, team, public

	// Metadata
	Labels      map[string]string `json:"labels,omitempty"`
	Annotations map[string]string `json:"annotations,omitempty"`

	// Hosted mode fields
	HubEndpoint string `json:"hubEndpoint,omitempty"` // Scion Hub URL if registered

	// Statistics (computed, not persisted)
	AgentCount int `json:"agentCount,omitempty"` // Number of agents in this grove
}

// GroveID returns the hosted-format grove ID (<uuid>__<slug>) if available,
// otherwise returns the Name or Slug as a fallback.
func (g *GroveInfo) GroveID() string {
	if g.ID != "" && g.Slug != "" {
		return g.ID + GroveIDSeparator + g.Slug
	}
	if g.Slug != "" {
		return g.Slug
	}
	return g.Name
}
