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

package api

import (
	"context"
	"fmt"
	"strings"
	"time"
)

// ParseDuration parses a duration string, returning 0 for empty or invalid input.
func ParseDuration(s string) time.Duration {
	if s == "" {
		return 0
	}
	d, err := time.ParseDuration(s)
	if err != nil {
		return 0
	}
	return d
}

// ServiceSpec defines a sidecar process to run alongside the main harness.
type ServiceSpec struct {
	Name       string            `json:"name" yaml:"name"`
	Command    []string          `json:"command" yaml:"command"`
	Restart    string            `json:"restart,omitempty" yaml:"restart,omitempty"`
	Env        map[string]string `json:"env,omitempty" yaml:"env,omitempty"`
	ReadyCheck *ReadyCheck       `json:"ready_check,omitempty" yaml:"ready_check,omitempty"`
}

// ReadyCheck defines a readiness gate for a service.
type ReadyCheck struct {
	Type    string `json:"type" yaml:"type"`       // "tcp", "http", "delay"
	Target  string `json:"target" yaml:"target"`   // "localhost:9222", "http://localhost:8080/health", "3s"
	Timeout string `json:"timeout" yaml:"timeout"` // max wait before giving up
}

// ValidateServices validates a slice of ServiceSpec entries.
func ValidateServices(services []ServiceSpec) error {
	seen := make(map[string]bool, len(services))
	for i, svc := range services {
		if svc.Name == "" {
			return fmt.Errorf("services[%d]: missing required field: name", i)
		}
		if seen[svc.Name] {
			return fmt.Errorf("services[%d]: duplicate service name: %q", i, svc.Name)
		}
		seen[svc.Name] = true

		if len(svc.Command) == 0 {
			return fmt.Errorf("services[%d] (%s): missing required field: command", i, svc.Name)
		}

		switch svc.Restart {
		case "", "no", "on-failure", "always":
			// valid
		default:
			return fmt.Errorf("services[%d] (%s): invalid restart policy: %q (must be \"no\", \"on-failure\", or \"always\")", i, svc.Name, svc.Restart)
		}

		if svc.ReadyCheck != nil {
			switch svc.ReadyCheck.Type {
			case "tcp", "http", "delay":
				// valid
			default:
				return fmt.Errorf("services[%d] (%s): invalid ready_check type: %q (must be \"tcp\", \"http\", or \"delay\")", i, svc.Name, svc.ReadyCheck.Type)
			}
			if svc.ReadyCheck.Target == "" {
				return fmt.Errorf("services[%d] (%s): ready_check missing required field: target", i, svc.Name)
			}
			if svc.ReadyCheck.Timeout == "" {
				return fmt.Errorf("services[%d] (%s): ready_check missing required field: timeout", i, svc.Name)
			}
		}
	}
	return nil
}

type AgentK8sMetadata struct {
	Cluster   string `json:"cluster"`
	Namespace string `json:"namespace"`
	PodName   string `json:"podName"`
	SyncedAt  string `json:"syncedAt,omitempty"`
}

// SharedDir defines a grove-level shared directory available to all agents.
type SharedDir struct {
	Name        string `json:"name" yaml:"name"`
	ReadOnly    bool   `json:"read_only,omitempty" yaml:"read_only,omitempty"`
	InWorkspace bool   `json:"in_workspace,omitempty" yaml:"in_workspace,omitempty"`
}

// ValidateSharedDirs validates a slice of SharedDir entries.
func ValidateSharedDirs(dirs []SharedDir) error {
	seen := make(map[string]bool, len(dirs))
	for i, d := range dirs {
		if d.Name == "" {
			return fmt.Errorf("shared_dirs[%d]: missing required field: name", i)
		}
		if !isValidSlug(d.Name) {
			return fmt.Errorf("shared_dirs[%d]: invalid name %q (must be lowercase alphanumeric with hyphens, e.g. \"build-cache\")", i, d.Name)
		}
		if seen[d.Name] {
			return fmt.Errorf("shared_dirs[%d]: duplicate name %q", i, d.Name)
		}
		seen[d.Name] = true
	}
	return nil
}

// isValidSlug checks that a name is a valid slug: lowercase alphanumeric with hyphens,
// starting and ending with alphanumeric, at least 1 character.
func isValidSlug(s string) bool {
	if len(s) == 0 {
		return false
	}
	for i, c := range s {
		if c >= 'a' && c <= 'z' || c >= '0' && c <= '9' {
			continue
		}
		if c == '-' && i > 0 && i < len(s)-1 {
			continue
		}
		return false
	}
	return true
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

// Validate checks that a VolumeMount has the required fields and valid values.
func (v VolumeMount) Validate() error {
	if v.Target == "" {
		return fmt.Errorf("volume mount missing required field: target")
	}

	volumeType := strings.ToLower(v.Type)
	switch volumeType {
	case "", "local":
		if v.Source == "" {
			return fmt.Errorf("local volume mount for target %q missing required field: source", v.Target)
		}
	case "gcs":
		if v.Bucket == "" {
			return fmt.Errorf("GCS volume mount for target %q missing required field: bucket", v.Target)
		}
	default:
		return fmt.Errorf("volume mount for target %q has invalid type %q (must be \"local\" or \"gcs\")", v.Target, v.Type)
	}

	return nil
}

// ValidateVolumes validates a slice of VolumeMount entries.
func ValidateVolumes(volumes []VolumeMount) error {
	for i, v := range volumes {
		if err := v.Validate(); err != nil {
			return fmt.Errorf("volumes[%d]: %w", i, err)
		}
	}
	return nil
}

type KubernetesConfig struct {
	Context               string            `json:"context,omitempty" yaml:"context,omitempty"`
	Namespace             string            `json:"namespace,omitempty" yaml:"namespace,omitempty"`
	RuntimeClassName      string            `json:"runtimeClassName,omitempty" yaml:"runtimeClassName,omitempty"`
	ServiceAccountName    string            `json:"serviceAccountName,omitempty" yaml:"serviceAccountName,omitempty"` // For Workload Identity
	Resources             *K8sResources     `json:"resources,omitempty" yaml:"resources,omitempty"`
	NodeSelector          map[string]string `json:"nodeSelector,omitempty" yaml:"nodeSelector,omitempty"`
	Tolerations           []K8sToleration   `json:"tolerations,omitempty" yaml:"tolerations,omitempty"`
	ImagePullPolicy       string            `json:"imagePullPolicy,omitempty" yaml:"imagePullPolicy,omitempty"`                   // Always, IfNotPresent, Never
	SharedDirStorageClass string            `json:"shared_dir_storage_class,omitempty" yaml:"shared_dir_storage_class,omitempty"` // Storage class for shared dir PVCs (must support RWX)
	SharedDirSize         string            `json:"shared_dir_size,omitempty" yaml:"shared_dir_size,omitempty"`                   // Default size per shared dir PVC (e.g. "10Gi")
}

// K8sToleration mirrors corev1.Toleration for use in agent configuration
// without requiring Kubernetes API imports in config consumers.
type K8sToleration struct {
	Key      string `json:"key,omitempty" yaml:"key,omitempty"`
	Operator string `json:"operator,omitempty" yaml:"operator,omitempty"` // Exists or Equal
	Value    string `json:"value,omitempty" yaml:"value,omitempty"`
	Effect   string `json:"effect,omitempty" yaml:"effect,omitempty"` // NoSchedule, PreferNoSchedule, NoExecute
}

type K8sResources struct {
	Requests map[string]string `json:"requests,omitempty" yaml:"requests,omitempty"`
	Limits   map[string]string `json:"limits,omitempty" yaml:"limits,omitempty"`
}

// ResourceSpec defines compute resource requirements for an agent container.
// It follows Kubernetes resource model conventions.
type ResourceSpec struct {
	Requests ResourceList `json:"requests,omitempty" yaml:"requests,omitempty"`
	Limits   ResourceList `json:"limits,omitempty" yaml:"limits,omitempty"`
	Disk     string       `json:"disk,omitempty" yaml:"disk,omitempty"`
}

// ResourceList is a set of resource name/quantity pairs.
type ResourceList struct {
	CPU    string `json:"cpu,omitempty" yaml:"cpu,omitempty"`
	Memory string `json:"memory,omitempty" yaml:"memory,omitempty"`
}

// AgentHubConfig holds hub connection settings that can be specified per-agent
// or per-template in scion-agent.yaml. When set, these take highest priority
// for the agent's hub endpoint, overriding grove settings and server config.
type AgentHubConfig struct {
	Endpoint string `json:"endpoint,omitempty" yaml:"endpoint,omitempty"`
}

// TelemetryConfig holds telemetry/observability settings at the agent/template level.
// These are merged with settings-level telemetry config (last write wins).
type TelemetryConfig struct {
	Enabled  *bool                  `json:"enabled,omitempty" yaml:"enabled,omitempty"`
	Cloud    *TelemetryCloudConfig  `json:"cloud,omitempty" yaml:"cloud,omitempty"`
	Hub      *TelemetryHubConfig    `json:"hub,omitempty" yaml:"hub,omitempty"`
	Local    *TelemetryLocalConfig  `json:"local,omitempty" yaml:"local,omitempty"`
	Filter   *TelemetryFilterConfig `json:"filter,omitempty" yaml:"filter,omitempty"`
	Resource map[string]string      `json:"resource,omitempty" yaml:"resource,omitempty"`
}

// TelemetryCloudConfig holds cloud OTLP forwarding settings.
type TelemetryCloudConfig struct {
	Enabled  *bool             `json:"enabled,omitempty" yaml:"enabled,omitempty"`
	Endpoint string            `json:"endpoint,omitempty" yaml:"endpoint,omitempty"`
	Protocol string            `json:"protocol,omitempty" yaml:"protocol,omitempty"`
	Headers  map[string]string `json:"headers,omitempty" yaml:"headers,omitempty"`
	TLS      *TelemetryTLS     `json:"tls,omitempty" yaml:"tls,omitempty"`
	Batch    *TelemetryBatch   `json:"batch,omitempty" yaml:"batch,omitempty"`
	Provider string            `json:"provider,omitempty" yaml:"provider,omitempty"`
}

// TelemetryTLS holds TLS settings for OTLP export.
type TelemetryTLS struct {
	Enabled            *bool  `json:"enabled,omitempty" yaml:"enabled,omitempty"`
	InsecureSkipVerify *bool  `json:"insecure_skip_verify,omitempty" yaml:"insecure_skip_verify,omitempty"`
	CAFile             string `json:"ca_file,omitempty" yaml:"ca_file,omitempty"`
}

// TelemetryBatch holds batch export settings.
type TelemetryBatch struct {
	MaxSize int    `json:"max_size,omitempty" yaml:"max_size,omitempty"`
	Timeout string `json:"timeout,omitempty" yaml:"timeout,omitempty"`
}

// TelemetryHubConfig holds Hub telemetry reporting settings.
type TelemetryHubConfig struct {
	Enabled        *bool  `json:"enabled,omitempty" yaml:"enabled,omitempty"`
	ReportInterval string `json:"report_interval,omitempty" yaml:"report_interval,omitempty"`
}

// TelemetryLocalConfig holds local debug telemetry output settings.
type TelemetryLocalConfig struct {
	Enabled *bool  `json:"enabled,omitempty" yaml:"enabled,omitempty"`
	File    string `json:"file,omitempty" yaml:"file,omitempty"`
	Console *bool  `json:"console,omitempty" yaml:"console,omitempty"`
}

// TelemetryFilterConfig holds event filtering and sampling settings.
type TelemetryFilterConfig struct {
	Enabled          *bool                      `json:"enabled,omitempty" yaml:"enabled,omitempty"`
	RespectDebugMode *bool                      `json:"respect_debug_mode,omitempty" yaml:"respect_debug_mode,omitempty"`
	Events           *TelemetryEventsConfig     `json:"events,omitempty" yaml:"events,omitempty"`
	Attributes       *TelemetryAttributesConfig `json:"attributes,omitempty" yaml:"attributes,omitempty"`
	Sampling         *TelemetrySamplingConfig   `json:"sampling,omitempty" yaml:"sampling,omitempty"`
}

// TelemetryEventsConfig holds event include/exclude lists.
type TelemetryEventsConfig struct {
	Include []string `json:"include,omitempty" yaml:"include,omitempty"`
	Exclude []string `json:"exclude,omitempty" yaml:"exclude,omitempty"`
}

// TelemetryAttributesConfig holds attribute redaction and hashing lists.
type TelemetryAttributesConfig struct {
	Redact []string `json:"redact,omitempty" yaml:"redact,omitempty"`
	Hash   []string `json:"hash,omitempty" yaml:"hash,omitempty"`
}

// TelemetrySamplingConfig holds sampling rate settings.
type TelemetrySamplingConfig struct {
	Default *float64           `json:"default,omitempty" yaml:"default,omitempty"`
	Rates   map[string]float64 `json:"rates,omitempty" yaml:"rates,omitempty"`
}

type ScionConfig struct {
	Harness          string            `json:"harness,omitempty" yaml:"harness,omitempty"`
	HarnessConfig    string            `json:"harness_config,omitempty" yaml:"harness_config,omitempty"`
	ConfigDir        string            `json:"config_dir,omitempty" yaml:"config_dir,omitempty"`
	Env              map[string]string `json:"env,omitempty" yaml:"env,omitempty"`
	Volumes          []VolumeMount     `json:"volumes,omitempty" yaml:"volumes,omitempty"`
	Detached         *bool             `json:"detached" yaml:"detached"`
	CommandArgs      []string          `json:"command_args,omitempty" yaml:"command_args,omitempty"`
	TaskFlag         string            `json:"task_flag,omitempty" yaml:"task_flag,omitempty"`
	Model            string            `json:"model,omitempty" yaml:"model,omitempty"`
	Kubernetes       *KubernetesConfig `json:"kubernetes,omitempty" yaml:"kubernetes,omitempty"`
	AuthSelectedType string            `json:"auth_selectedType,omitempty" yaml:"auth_selectedType,omitempty"`
	Resources        *ResourceSpec     `json:"resources,omitempty" yaml:"resources,omitempty"`
	Image            string            `json:"image,omitempty" yaml:"image,omitempty"`
	Services         []ServiceSpec     `json:"services,omitempty" yaml:"services,omitempty"`
	MaxTurns         int               `json:"max_turns,omitempty" yaml:"max_turns,omitempty"`
	MaxModelCalls    int               `json:"max_model_calls,omitempty" yaml:"max_model_calls,omitempty"`
	MaxDuration      string            `json:"max_duration,omitempty" yaml:"max_duration,omitempty"`
	Hub              *AgentHubConfig   `json:"hub,omitempty" yaml:"hub,omitempty"`
	Telemetry        *TelemetryConfig  `json:"telemetry,omitempty" yaml:"telemetry,omitempty"`

	Secrets []RequiredSecret `json:"secrets,omitempty" yaml:"secrets,omitempty"`

	// Agnostic template fields
	AgentInstructions    string `json:"agent_instructions,omitempty" yaml:"agent_instructions,omitempty"`
	SystemPrompt         string `json:"system_prompt,omitempty" yaml:"system_prompt,omitempty"`
	DefaultHarnessConfig string `json:"default_harness_config,omitempty" yaml:"default_harness_config,omitempty"`

	// Container user (absorbed from harness-config)
	User string `json:"user,omitempty" yaml:"user,omitempty"`

	// Agent operational parameters (creation-time record)
	Task   string `json:"task,omitempty" yaml:"task,omitempty"`
	Branch string `json:"branch,omitempty" yaml:"branch,omitempty"`

	// Info contains persisted metadata about the agent
	Info *AgentInfo `json:"-" yaml:"-"`
}

// ParseMaxDuration returns the parsed max duration, or 0 for empty/invalid values.
func (c *ScionConfig) ParseMaxDuration() time.Duration {
	return ParseDuration(c.MaxDuration)
}

func (c *ScionConfig) IsDetached() bool {
	if c.Detached == nil {
		return true
	}
	return *c.Detached
}

type AuthConfig struct {
	// Google/Gemini auth
	GeminiAPIKey         string
	GoogleAPIKey         string
	GoogleAppCredentials string
	GoogleCloudProject   string
	GoogleCloudRegion    string
	OAuthCreds           string

	// Anthropic auth
	AnthropicAPIKey  string
	ClaudeOAuthToken string // CLAUDE_CODE_OAUTH_TOKEN (long-lived, from `claude setup-token`)
	ClaudeAuthFile   string // ~/.claude/.credentials.json path (rotating refresh-token store)

	// OpenAI/Codex auth
	OpenAIAPIKey     string
	CodexAPIKey      string
	CodexAuthFile    string
	OpenCodeAuthFile string

	// GCP metadata server mode ("block", "passthrough", "assign").
	// When "assign", a GCP service account is available via the metadata
	// server and ADC file secrets are not required for vertex-ai auth.
	GCPMetadataMode string

	// Auth mode selection
	SelectedType string
}

// ResolvedAuth represents the single best auth method selected by a harness's
// ResolveAuth method. It contains everything needed to inject credentials into
// an agent container.
type ResolvedAuth struct {
	Method  string            // e.g. "anthropic-api-key", "vertex-ai", "passthrough"
	EnvVars map[string]string // env vars to inject into container
	Files   []FileMapping     // files to copy/mount into container
}

// FileMapping describes a credential file that needs to be propagated from the
// host into an agent container.
type FileMapping struct {
	SourcePath    string // absolute host path
	ContainerPath string // target path in container (~ = home placeholder)
}

// AgentInfo contains metadata about a scion agent.
// It supports both local/solo mode and hosted/distributed mode.
type AgentInfo struct {
	// Identity fields
	ID            string `json:"id,omitempty"`          // Hub UUID (database primary key, globally unique)
	Slug          string `json:"slug,omitempty"`        // URL-safe slug identifier (unique per grove)
	ContainerID   string `json:"containerId,omitempty"` // Runtime container ID (ephemeral, runtime-assigned)
	Name          string `json:"name"`                  // Human-friendly display name
	Template      string `json:"template"`
	HarnessConfig string `json:"harnessConfig,omitempty"` // Resolved harness-config name
	// HarnessConfigRevision records the harness-config bundle revision (e.g.
	// the Hub artifact's ContentHash) that this agent was provisioned from.
	// Empty when the harness-config came from a built-in or local source
	// without a tracked revision. Used by Phase 3 broker dispatch tests and
	// audit flows so operators can correlate an agent with the exact bundle
	// it ran.
	HarnessConfigRevision string `json:"harnessConfigRevision,omitempty"`
	HarnessAuth           string `json:"harnessAuth,omitempty"` // Resolved harness auth method (api-key, oauth-token, auth-file, vertex-ai)

	// Grove association
	Grove     string `json:"grove"`               // Grove name (legacy, simple string)
	GroveID   string `json:"groveId,omitempty"`   // Hosted format: <uuid>__<name>
	GrovePath string `json:"grovePath,omitempty"` // Filesystem path (solo mode)

	// Metadata
	Labels      map[string]string `json:"labels,omitempty"`
	Annotations map[string]string `json:"annotations,omitempty"`

	// Status fields
	ContainerStatus string       `json:"containerStatus,omitempty"` // Container status (e.g., Up 2 hours)
	Phase           string       `json:"phase,omitempty"`           // Lifecycle phase (created, provisioning, running, stopped, error)
	Activity        string       `json:"activity,omitempty"`        // Runtime activity (idle, thinking, executing, waiting_for_input, completed)
	Detail          *AgentDetail `json:"detail,omitempty"`          // Freeform context about the current activity

	// Runtime configuration
	Image      string            `json:"image,omitempty"`
	Detached   bool              `json:"detached,omitempty"`
	Runtime    string            `json:"runtime,omitempty"`
	Profile    string            `json:"profile,omitempty"`
	Kubernetes *AgentK8sMetadata `json:"kubernetes,omitempty"`
	Warnings   []string          `json:"warnings,omitempty"`

	// Timestamps
	Created   time.Time `json:"created,omitempty"`   // When the agent was created
	Updated   time.Time `json:"updated,omitempty"`   // Last modification timestamp
	LastSeen  time.Time `json:"lastSeen,omitempty"`  // Last heartbeat/status report
	DeletedAt time.Time `json:"deletedAt,omitempty"` // When the agent was soft-deleted

	// Ownership & access
	CreatedBy  string   `json:"createdBy,omitempty"`  // User/system that created the agent
	OwnerID    string   `json:"ownerId,omitempty"`    // Current owner user ID
	Visibility string   `json:"visibility,omitempty"` // Access level: private, team, public
	Ancestry   []string `json:"ancestry,omitempty"`   // Ordered ancestor chain [root, ..., parent] for transitive access

	// Hosted/distributed mode fields
	RuntimeBrokerID   string `json:"runtimeBrokerId,omitempty"`   // ID of the Runtime Broker managing this agent
	RuntimeBrokerName string `json:"runtimeBrokerName,omitempty"` // Name of the Runtime Broker
	RuntimeBrokerType string `json:"runtimeBrokerType,omitempty"` // Type: docker, kubernetes, apple
	RuntimeState      string `json:"runtimeState,omitempty"`      // Low-level runtime state
	HubEndpoint       string `json:"hubEndpoint,omitempty"`       // Scion Hub URL if connected
	WebPTYEnabled     bool   `json:"webPtyEnabled,omitempty"`     // Whether web terminal access is available
	TaskSummary       string `json:"taskSummary,omitempty"`       // Current task description (for dashboard)

	// Optimistic locking
	StateVersion int64 `json:"stateVersion,omitempty"` // Version for concurrent update detection
}

// AgentDetail provides freeform context about the current activity.
type AgentDetail struct {
	ToolName    string `json:"toolName,omitempty"`
	Message     string `json:"message,omitempty"`
	TaskSummary string `json:"taskSummary,omitempty"`
}

// RequiredSecret declares a secret that must be available for an agent to start.
// Declared in templates (scion-agent.yaml), settings harness configs, or settings profiles.
type RequiredSecret struct {
	Key         string `json:"key" yaml:"key"`
	Description string `json:"description,omitempty" yaml:"description,omitempty"`
	Type        string `json:"type,omitempty" yaml:"type,omitempty"`     // "environment" (default), "variable", "file"
	Target      string `json:"target,omitempty" yaml:"target,omitempty"` // Projection target (defaults to Key for env type)
	// AlternativeEnvKeys lists env var names that can satisfy this secret
	// requirement as an alternative. If any of these env vars are present,
	// the file secret is not required. For example, GOOGLE_APPLICATION_CREDENTIALS
	// can substitute for a gcloud-adc file secret.
	AlternativeEnvKeys []string `json:"alternative_env_keys,omitempty" yaml:"alternative_env_keys,omitempty"`
}

// SecretKeyInfo provides metadata about a required secret key, including
// a human-readable description and the source that declared it.
type SecretKeyInfo struct {
	Description string `json:"description,omitempty"`
	Source      string `json:"source"`         // "harness", "template", "settings"
	Type        string `json:"type,omitempty"` // "environment" (default), "variable", "file"
}

// ResolvedSecret represents a secret that has been resolved from the Hub
// and is ready for projection into an agent container.
type ResolvedSecret struct {
	Name   string `json:"name"`          // Secret key name
	Type   string `json:"type"`          // environment, variable, file
	Target string `json:"target"`        // Projection target (env var name, json key, or file path)
	Value  string `json:"value"`         // Decrypted secret value
	Source string `json:"source"`        // Scope that provided this secret (user, grove, runtime_broker)
	Ref    string `json:"ref,omitempty"` // External secret reference (e.g., "gcpsm:projects/123/secrets/name")
}

// GitCloneConfig specifies how to clone a git repository into the workspace.
// When present, the runtime skips local worktree creation and workspace
// mounting — sciontool clones the repo inside the container at startup.
type GitCloneConfig struct {
	URL    string `json:"url"`              // HTTPS clone URL (without credentials)
	Branch string `json:"branch,omitempty"` // Branch to clone (default: main)
	Depth  int    `json:"depth,omitempty"`  // Clone depth (default: 1, 0 = full)
}

type gitCloneContextKey struct{}

// ContextWithGitClone returns a new context with the GitCloneConfig attached.
func ContextWithGitClone(ctx context.Context, gc *GitCloneConfig) context.Context {
	return context.WithValue(ctx, gitCloneContextKey{}, gc)
}

// GitCloneFromContext retrieves the GitCloneConfig from the context, or nil if not set.
func GitCloneFromContext(ctx context.Context) *GitCloneConfig {
	gc, _ := ctx.Value(gitCloneContextKey{}).(*GitCloneConfig)
	return gc
}

type sharedWorkspaceContextKey struct{}

// ContextWithSharedWorkspace returns a new context with the shared workspace flag attached.
func ContextWithSharedWorkspace(ctx context.Context) context.Context {
	return context.WithValue(ctx, sharedWorkspaceContextKey{}, true)
}

// IsSharedWorkspaceFromContext returns true if the context indicates shared workspace mode.
func IsSharedWorkspaceFromContext(ctx context.Context) bool {
	v, _ := ctx.Value(sharedWorkspaceContextKey{}).(bool)
	return v
}

type githubAppContextKey struct{}

// ContextWithGitHubApp returns a new context with the GitHub App enabled flag attached.
func ContextWithGitHubApp(ctx context.Context) context.Context {
	return context.WithValue(ctx, githubAppContextKey{}, true)
}

// IsGitHubAppFromContext returns true if the context indicates GitHub App is enabled.
func IsGitHubAppFromContext(ctx context.Context) bool {
	v, _ := ctx.Value(githubAppContextKey{}).(bool)
	return v
}

type brokerModeContextKey struct{}

// ContextWithBrokerMode returns a new context with broker mode flag attached.
func ContextWithBrokerMode(ctx context.Context) context.Context {
	return context.WithValue(ctx, brokerModeContextKey{}, true)
}

// IsBrokerModeFromContext returns true if the context indicates broker mode.
func IsBrokerModeFromContext(ctx context.Context) bool {
	v, _ := ctx.Value(brokerModeContextKey{}).(bool)
	return v
}

type StartOptions struct {
	Name              string
	Task              string
	Template          string
	TemplateName      string // Human-friendly template slug (overrides Template for labels when hydration replaces Template with a cache path)
	Profile           string
	HarnessConfig     string
	HarnessAuth       string // Late-binding override for auth_selected_type (api-key, oauth-token, auth-file, vertex-ai)
	Image             string
	GrovePath         string
	Env               map[string]string
	ResolvedSecrets   []ResolvedSecret
	BrokerMode        bool // When true, auth gathering skips local sources (broker env + filesystem)
	Detached          *bool
	Resume            bool
	NoAuth            bool
	Branch            string
	Workspace         string
	GitClone          *GitCloneConfig // When set, skip workspace creation; sciontool clones inside container
	SharedWorkspace   bool            // When true, workspace is a shared git clone (git-workspace hybrid); skip worktree, configure credential helper
	TelemetryOverride *bool           // Explicit telemetry override from CLI flags (--enable-telemetry / --disable-telemetry)
	InlineConfig      *ScionConfig    // Inline config from --config flag, merged over template config
	SharedDirs        []SharedDir     // Grove-level shared directories (from Hub, merged with settings)
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
