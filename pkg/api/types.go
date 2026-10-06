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
	"strconv"
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

// MCPTransport identifies the transport protocol an MCP server uses.
type MCPTransport string

const (
	MCPTransportStdio          MCPTransport = "stdio"
	MCPTransportSSE            MCPTransport = "sse"
	MCPTransportStreamableHTTP MCPTransport = "streamable-http"
)

// MCPScope identifies whether an MCP server is registered globally or per-project
// in the harness's native config. Harnesses that do not distinguish project
// scope (Gemini, OpenCode) treat "project" as "global".
type MCPScope string

const (
	MCPScopeGlobal  MCPScope = "global"
	MCPScopeProject MCPScope = "project"
)

// MCPServerConfig is the universal, harness-agnostic MCP server description.
// Template authors define MCP servers once in scion-agent.yaml's mcp_servers
// block; the container-side provisioner translates this into each harness's
// native format (.claude.json, .gemini/settings.json, opencode.json, etc.).
type MCPServerConfig struct {
	Transport MCPTransport      `json:"transport" yaml:"transport"`
	Command   string            `json:"command,omitempty" yaml:"command,omitempty"`
	Args      []string          `json:"args,omitempty" yaml:"args,omitempty"`
	Env       map[string]string `json:"env,omitempty" yaml:"env,omitempty"`
	URL       string            `json:"url,omitempty" yaml:"url,omitempty"`
	Headers   map[string]string `json:"headers,omitempty" yaml:"headers,omitempty"`
	Scope     MCPScope          `json:"scope,omitempty" yaml:"scope,omitempty"`
}

// ValidateMCPServers validates an MCP server map per the rules in
// .design/template-mcp-servers.md.
func ValidateMCPServers(servers map[string]MCPServerConfig) error {
	for name, srv := range servers {
		if name == "" {
			return fmt.Errorf("mcp_servers: empty server name")
		}
		if !isValidMCPName(name) {
			return fmt.Errorf("mcp_servers[%q]: invalid name (must be alphanumeric with hyphens or underscores)", name)
		}
		switch srv.Transport {
		case MCPTransportStdio:
			if srv.Command == "" {
				return fmt.Errorf("mcp_servers[%q]: transport %q requires command", name, srv.Transport)
			}
			if srv.URL != "" {
				return fmt.Errorf("mcp_servers[%q]: transport %q does not allow url", name, srv.Transport)
			}
			if len(srv.Headers) > 0 {
				return fmt.Errorf("mcp_servers[%q]: transport %q does not allow headers", name, srv.Transport)
			}
		case MCPTransportSSE, MCPTransportStreamableHTTP:
			if srv.URL == "" {
				return fmt.Errorf("mcp_servers[%q]: transport %q requires url", name, srv.Transport)
			}
			if srv.Command != "" {
				return fmt.Errorf("mcp_servers[%q]: transport %q does not allow command", name, srv.Transport)
			}
			if len(srv.Args) > 0 {
				return fmt.Errorf("mcp_servers[%q]: transport %q does not allow args", name, srv.Transport)
			}
			if len(srv.Env) > 0 {
				return fmt.Errorf("mcp_servers[%q]: transport %q does not allow env", name, srv.Transport)
			}
		case "":
			return fmt.Errorf("mcp_servers[%q]: missing required field: transport", name)
		default:
			return fmt.Errorf("mcp_servers[%q]: invalid transport %q (must be %q, %q, or %q)",
				name, srv.Transport, MCPTransportStdio, MCPTransportSSE, MCPTransportStreamableHTTP)
		}
		switch srv.Scope {
		case "", MCPScopeGlobal, MCPScopeProject:
			// valid (empty defaults to global at provisioning time)
		default:
			return fmt.Errorf("mcp_servers[%q]: invalid scope %q (must be %q or %q)",
				name, srv.Scope, MCPScopeGlobal, MCPScopeProject)
		}
	}
	return nil
}

// isValidMCPName accepts alphanumeric names with optional hyphens or underscores
// (no leading/trailing punctuation). Mirrors slug rules used elsewhere but
// permits underscores too, since MCP server names commonly use them.
func isValidMCPName(s string) bool {
	if s == "" {
		return false
	}
	for i, c := range s {
		switch {
		case c >= 'a' && c <= 'z':
		case c >= 'A' && c <= 'Z':
		case c >= '0' && c <= '9':
		case (c == '-' || c == '_') && i > 0 && i < len(s)-1:
		default:
			return false
		}
	}
	return true
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
	// UID is the pod's Kubernetes-assigned unique identifier, distinct from
	// PodName. Populated by KubernetesRuntime.List when available; empty for
	// any other backend, or for a Kubernetes pod fetched through a path
	// that does not set it.
	UID string `json:"uid,omitempty"`
}

// SharedDir defines a project-level shared directory available to all agents.
type SharedDir struct {
	Name        string `json:"name" yaml:"name" koanf:"name"`
	ReadOnly    bool   `json:"read_only,omitempty" yaml:"read_only,omitempty" koanf:"read_only"`
	InWorkspace bool   `json:"in_workspace,omitempty" yaml:"in_workspace,omitempty" koanf:"in_workspace"`
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
	Source   string `json:"source" yaml:"source" koanf:"source"`
	Target   string `json:"target" yaml:"target" koanf:"target"`
	ReadOnly bool   `json:"read_only,omitempty" yaml:"read_only,omitempty" koanf:"read_only"`
	// Type discriminates the volume kind:
	//   "local" (default) — host bind mount; requires Source.
	//   "gcs"             — GCS FUSE mount; requires Bucket.
	//   "nfs"             — literal NFS protocol mount; requires Server, Source.
	//   "cloudrun-volume" — Cloud Run managed volume; requires VolumeName.
	//   "gke-shared-volume" — GKE-provided shared volume (e.g. Filestore CSI PVC); requires VolumeName.
	Type       string `json:"type,omitempty" yaml:"type,omitempty" koanf:"type"`
	Bucket     string `json:"bucket,omitempty" yaml:"bucket,omitempty" koanf:"bucket"`                // GCS bucket name
	Prefix     string `json:"prefix,omitempty" yaml:"prefix,omitempty" koanf:"prefix"`                // GCS object prefix
	Mode       string `json:"mode,omitempty" yaml:"mode,omitempty" koanf:"mode"`                      // Mount options
	Server     string `json:"server,omitempty" yaml:"server,omitempty" koanf:"server"`                // NFS: server host/IP
	VolumeName string `json:"volume_name,omitempty" yaml:"volume_name,omitempty" koanf:"volume_name"` // Cloud Run / GKE volume name
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
	case "nfs":
		if v.Server == "" {
			return fmt.Errorf("NFS volume mount for target %q missing required field: server", v.Target)
		}
		if v.Source == "" {
			return fmt.Errorf("NFS volume mount for target %q missing required field: source (server export path)", v.Target)
		}
	case "cloudrun-volume":
		if v.VolumeName == "" {
			return fmt.Errorf("cloudrun-volume mount for target %q missing required field: volume_name", v.Target)
		}
	case "gke-shared-volume":
		if v.VolumeName == "" {
			return fmt.Errorf("gke-shared-volume mount for target %q missing required field: volume_name", v.Target)
		}
	default:
		return fmt.Errorf("volume mount for target %q has invalid type %q (must be \"local\", \"gcs\", \"nfs\", \"cloudrun-volume\", or \"gke-shared-volume\")", v.Target, v.Type)
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
	Context            string `json:"context,omitempty" yaml:"context,omitempty"`
	Namespace          string `json:"namespace,omitempty" yaml:"namespace,omitempty"`
	RuntimeClassName   string `json:"runtimeClassName,omitempty" yaml:"runtimeClassName,omitempty"`
	ServiceAccountName string `json:"serviceAccountName,omitempty" yaml:"serviceAccountName,omitempty"` // For Workload Identity
	// PriorityClassName sets spec.priorityClassName on the agent pod. Must
	// name a PriorityClass that already exists on the cluster — Scion does
	// not create one. Validated as a DNS-1123 subdomain. Unset means today's
	// behaviour (no priorityClassName, so the pod schedules at priority 0
	// and is an ordinary preemption target). Overrides the runtime-level
	// default set by runtimes.<name>.priority_class_name, if any.
	PriorityClassName     string            `json:"priorityClassName,omitempty" yaml:"priorityClassName,omitempty"`
	Resources             *K8sResources     `json:"resources,omitempty" yaml:"resources,omitempty"`
	NodeSelector          map[string]string `json:"nodeSelector,omitempty" yaml:"nodeSelector,omitempty"`
	Tolerations           []K8sToleration   `json:"tolerations,omitempty" yaml:"tolerations,omitempty"`
	ImagePullPolicy       string            `json:"imagePullPolicy,omitempty" yaml:"imagePullPolicy,omitempty"`                   // Always, IfNotPresent, Never
	SharedDirStorageClass string            `json:"shared_dir_storage_class,omitempty" yaml:"shared_dir_storage_class,omitempty"` // Storage class for shared dir PVCs (must support RWX)
	SharedDirSize         string            `json:"shared_dir_size,omitempty" yaml:"shared_dir_size,omitempty"`                   // Default size per shared dir PVC (e.g. "10Gi")
	// SafeToEvict, when explicitly false, adds the
	// cluster-autoscaler.kubernetes.io/safe-to-evict: "false" annotation to
	// the agent pod. On GKE Autopilot this requests extended run duration;
	// on other clusters it stops the cluster autoscaler from scaling the
	// node down while the pod runs. Only false has an effect: nil and true
	// both leave the pod unannotated. Overrides the profile's and the
	// runtime entry's safe_to_evict, if any. Ignored on non-Kubernetes
	// runtimes.
	SafeToEvict *bool `json:"safeToEvict,omitempty" yaml:"safeToEvict,omitempty"`
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
	Requests ResourceList `json:"requests,omitempty" yaml:"requests,omitempty" koanf:"requests"`
	Limits   ResourceList `json:"limits,omitempty" yaml:"limits,omitempty" koanf:"limits"`
	Disk     string       `json:"disk,omitempty" yaml:"disk,omitempty" koanf:"disk"`
}

// ResourceList is a set of resource name/quantity pairs.
type ResourceList struct {
	CPU    string `json:"cpu,omitempty" yaml:"cpu,omitempty" koanf:"cpu"`
	Memory string `json:"memory,omitempty" yaml:"memory,omitempty" koanf:"memory"`
}

// AgentHubConfig holds hub connection settings that can be specified per-agent
// or per-template in scion-agent.yaml. When set, these take highest priority
// for the agent's hub endpoint, overriding project settings and server config.
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
	Enabled      *bool             `json:"enabled,omitempty" yaml:"enabled,omitempty"`
	Endpoint     string            `json:"endpoint,omitempty" yaml:"endpoint,omitempty"`
	Protocol     string            `json:"protocol,omitempty" yaml:"protocol,omitempty"`
	Headers      map[string]string `json:"headers,omitempty" yaml:"headers,omitempty"`
	TLS          *TelemetryTLS     `json:"tls,omitempty" yaml:"tls,omitempty"`
	Batch        *TelemetryBatch   `json:"batch,omitempty" yaml:"batch,omitempty"`
	Provider     string            `json:"provider,omitempty" yaml:"provider,omitempty"`
	GCPProjectID *string           `json:"gcp_project_id,omitempty" yaml:"gcp_project_id,omitempty"`
	CloudLogging *bool             `json:"cloud_logging,omitempty" yaml:"cloud_logging,omitempty"`
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
	ThinkingLevel    *int              `json:"thinking_level,omitempty" yaml:"thinking_level,omitempty"`
	Kubernetes       *KubernetesConfig `json:"kubernetes,omitempty" yaml:"kubernetes,omitempty"`
	AuthSelectedType string            `json:"auth_selectedType,omitempty" yaml:"auth_selectedType,omitempty"`
	Resources        *ResourceSpec     `json:"resources,omitempty" yaml:"resources,omitempty"`
	Image            string            `json:"image,omitempty" yaml:"image,omitempty"`
	Services         []ServiceSpec     `json:"services,omitempty" yaml:"services,omitempty"`
	// MCPServers is the universal MCP server map. Keys are server names; values
	// are the transport-agnostic config translated by each harness's
	// container-side provisioner into native format.
	MCPServers    map[string]MCPServerConfig `json:"mcp_servers,omitempty" yaml:"mcp_servers,omitempty"`
	MaxTurns      int                        `json:"max_turns,omitempty" yaml:"max_turns,omitempty"`
	MaxModelCalls int                        `json:"max_model_calls,omitempty" yaml:"max_model_calls,omitempty"`
	MaxDuration   string                     `json:"max_duration,omitempty" yaml:"max_duration,omitempty"`
	Hub           *AgentHubConfig            `json:"hub,omitempty" yaml:"hub,omitempty"`
	Telemetry     *TelemetryConfig           `json:"telemetry,omitempty" yaml:"telemetry,omitempty"`

	Secrets []RequiredSecret `json:"secrets,omitempty" yaml:"secrets,omitempty"`

	// Skills declares skill references to resolve at provision time.
	Skills []SkillReference `json:"skills,omitempty" yaml:"skills,omitempty" koanf:"skills"`

	// Agnostic template fields
	AgentInstructions    string `json:"agent_instructions,omitempty" yaml:"agent_instructions,omitempty"`
	SystemPrompt         string `json:"system_prompt,omitempty" yaml:"system_prompt,omitempty"`
	DefaultHarnessConfig string `json:"default_harness_config,omitempty" yaml:"default_harness_config,omitempty"`

	// Container user (absorbed from harness-config)
	User string `json:"user,omitempty" yaml:"user,omitempty"`

	// Agent operational parameters (creation-time record)
	Task   string `json:"task,omitempty" yaml:"task,omitempty"`
	Branch string `json:"branch,omitempty" yaml:"branch,omitempty"`

	// ExplicitWorkspace records that /workspace is a user-provided --workspace
	// path, bind-mounted directly with no git worktree/branch, even when inside a
	// repo. Persisted so resume/restart honors the same contract as first start.
	//
	// Also true for a broker-provisioned worktree-per-agent workspace
	// (tryProvisionWorktree sets opts.Workspace too, so it takes the same
	// branch in ProvisionAgent): both cases mount from a Volumes-derived path
	// rather than GetAgent's managed <agentDir>/workspace recovery. RepoRoot
	// resolution, not this flag, is what tells the two cases apart — see
	// pkg/agent's readProvisionedWorktreeRepoRoot. That value is deliberately
	// not a field on ScionConfig (populated by unmarshaling templates, hub
	// inline config, and --config files) nor on AgentInfo (persisted in
	// agentHome, which the container can write) — see
	// ContextWithProvisionedWorktreeRepoRoot below.
	ExplicitWorkspace bool `json:"explicit_workspace,omitempty" yaml:"explicit_workspace,omitempty"`

	// EmptyPerAgentWorkspace records that the agent's workspace is its
	// private, non-git agents/<slug>/workspace directory (design #2703).
	// Persisted so a restart or resume that arrives without the mode (e.g.
	// a dropped or undecodable request body) can never fall back to legacy
	// workspace resolution or an enclosing repo root, and so delete skips
	// worktree/branch cleanup for it.
	EmptyPerAgentWorkspace bool `json:"empty_per_agent_workspace,omitempty" yaml:"empty_per_agent_workspace,omitempty"`

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
	// GCP shared fields — used across vertex-ai harnesses and have
	// special handling. These remain as first-class fields because they
	// participate in multi-source fallback resolution (e.g.
	// GOOGLE_CLOUD_PROJECT | GCP_PROJECT | ANTHROPIC_VERTEX_PROJECT_ID)
	// and are referenced by ResolveAuth for vertex-ai credential translation.
	GoogleAppCredentials string
	GoogleCloudProject   string
	GoogleCloudRegion    string

	// GCP metadata server mode ("block", "passthrough", "assign").
	// When "assign", a GCP service account is available via the metadata
	// server and ADC file secrets are not required for vertex-ai auth.
	GCPMetadataMode string

	// Auth mode selection
	SelectedType string

	// EnvVars holds config-driven auth env vars gathered from harness
	// config metadata (auth.types[*].required_env). These flow through
	// the auth pipeline as the sole source of per-provider credentials,
	// enabling harnesses to declare auth requirements without Go code changes.
	EnvVars map[string]string

	// Files holds config-driven file-based auth credentials gathered from
	// harness config metadata (auth.types[*].required_files). Each entry
	// maps a field name (e.g. "ClaudeAuthFile") to the host path of the
	// credential file. These replace the former per-provider file fields.
	Files map[string]string
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
	Slug          string `json:"slug,omitempty"`        // URL-safe slug identifier (unique per project)
	ContainerID   string `json:"containerId,omitempty"` // Runtime container ID (ephemeral, runtime-assigned)
	RunID         string `json:"runId,omitempty"`       // Per-run identity from the LabelRunID label; empty for pre-run-ID entries (ptone/scion#2550). In agent-info.json: the run that owns the agent's files (ptone/scion#2675); List never fills RunID from agent-info.json, so on the wire it is always the label
	Name          string `json:"name"`                  // Human-friendly display name
	Template      string `json:"template"`
	TemplateHash  string `json:"templateHash,omitempty"`  // Content hash of a template loaded from a content-addressed cache dir; when set, Template is a display name only and is never looked up by name
	HarnessConfig string `json:"harnessConfig,omitempty"` // Resolved harness-config name
	// HarnessConfigRevision records the harness-config bundle revision (e.g.
	// the Hub artifact's ContentHash) that this agent was provisioned from.
	// Empty when the harness-config came from a built-in or local source
	// without a tracked revision. Used by Phase 3 broker dispatch tests and
	// audit flows so operators can correlate an agent with the exact bundle
	// it ran.
	HarnessConfigRevision string `json:"harnessConfigRevision,omitempty"`
	HarnessAuth           string `json:"harnessAuth,omitempty"` // Resolved harness auth method (api-key, oauth-token, auth-file, vertex-ai)

	// Project association
	Project     string `json:"project"`               // Project name (standard field)
	ProjectID   string `json:"projectId,omitempty"`   // Hosted format: <uuid>__<name>
	ProjectPath string `json:"projectPath,omitempty"` // Filesystem path (solo mode)

	// Metadata
	Labels      map[string]string `json:"labels,omitempty"`
	Annotations map[string]string `json:"annotations,omitempty"`

	// Status fields
	ContainerStatus string       `json:"containerStatus,omitempty"` // Container status (e.g., Up 2 hours)
	Phase           string       `json:"phase,omitempty"`           // Lifecycle phase (created, provisioning, running, stopped, error)
	Activity        string       `json:"activity,omitempty"`        // Runtime activity (working, thinking, executing, waiting_for_input, completed)
	ExitCode        *int         `json:"exitCode,omitempty"`        // Structured exit code from runtime (nil = unknown)
	ExitReason      string       `json:"exitReason,omitempty"`      // Terminal reason: "crashed" or "limits_exceeded"
	Detail          *AgentDetail `json:"detail,omitempty"`          // Freeform context about the current activity

	// Runtime configuration
	Image      string            `json:"image,omitempty"`
	Detached   bool              `json:"detached,omitempty"`
	Runtime    string            `json:"runtime,omitempty"`
	Profile    string            `json:"profile,omitempty"`
	Kubernetes *AgentK8sMetadata `json:"kubernetes,omitempty"`
	// WorkspacePlacement is where the start that produced this info placed
	// the agent's workspace: WorkspacePlacementExport or
	// WorkspacePlacementLocal. Empty when this info did not come from a
	// start that resolved the workspace (a listing, or an agent found
	// already running).
	WorkspacePlacement string   `json:"workspacePlacement,omitempty"`
	Warnings           []string `json:"warnings,omitempty"`
	// HubOnlyEnvWarnings carries only the warnings for broker-local values
	// of hub-only env keys (TZ) that the broker dropped for a hub-dispatched
	// agent. They are also included in Warnings; this field lets the broker
	// relay just these to the hub without leaking its other local warnings.
	HubOnlyEnvWarnings []string `json:"hubOnlyEnvWarnings,omitempty"`

	// ExplicitImage and ExplicitImagePullPolicy record the image /
	// kubernetes.imagePullPolicy that the INLINE config (--config), not a
	// template, explicitly set at provision time — deliberately not a
	// template's contribution, since a template is re-read live on every
	// Start and so needs no persisted copy; only the inline config, which
	// has no live source to re-derive from on a later restart, does. A
	// restart whose own request supplies no inline image/pull-policy of its
	// own — including one that passes --harness-auth or an unrelated
	// --config field — falls back to these fields, so a value pinned via
	// --config at create time is not silently replaced by a Hub settings or
	// file-default value that was only ever meant to apply when nothing more
	// specific was set (ptone/scion#2156).
	ExplicitImage           string `json:"explicitImage,omitempty"`
	ExplicitImagePullPolicy string `json:"explicitImagePullPolicy,omitempty"`

	// Timestamps
	Created           time.Time `json:"created,omitempty"`           // When the agent was created
	Updated           time.Time `json:"updated,omitempty"`           // Last modification timestamp
	LastSeen          time.Time `json:"lastSeen,omitempty"`          // Last heartbeat/status report
	LastActivityEvent time.Time `json:"lastActivityEvent,omitempty"` // Last meaningful activity change
	DeletedAt         time.Time `json:"deletedAt,omitempty"`         // When the agent was soft-deleted

	// Ownership & access
	CreatedBy string   `json:"createdBy,omitempty"` // User/system that created the agent
	OwnerID   string   `json:"ownerId,omitempty"`   // Current owner user ID
	Ancestry  []string `json:"ancestry,omitempty"`  // Ordered ancestor chain [root, ..., parent] for transitive access

	// Hosted/distributed mode fields
	RuntimeBrokerID   string `json:"runtimeBrokerId,omitempty"`   // ID of the Runtime Broker managing this agent
	RuntimeBrokerName string `json:"runtimeBrokerName,omitempty"` // Name of the Runtime Broker
	RuntimeBrokerType string `json:"runtimeBrokerType,omitempty"` // Type: docker, kubernetes, apple
	RuntimeState      string `json:"runtimeState,omitempty"`      // Low-level runtime state
	HubEndpoint       string `json:"hubEndpoint,omitempty"`       // Scion Hub URL if connected
	WebPTYEnabled     bool   `json:"webPtyEnabled,omitempty"`     // Whether web terminal access is available
	TaskSummary       string `json:"taskSummary,omitempty"`       // Current task description (for dashboard)
	// ProvisionedOnly: the Hub reports the agent provisioned but not
	// started (ptone/scion#2929). No omitempty: an explicit false lets a
	// client that merges responses clear a previously seen true.
	ProvisionedOnly bool `json:"provisionedOnly"`

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
	Key         string `json:"key" yaml:"key" koanf:"key"`
	Description string `json:"description,omitempty" yaml:"description,omitempty" koanf:"description"`
	Type        string `json:"type,omitempty" yaml:"type,omitempty" koanf:"type"`       // "environment" (default), "variable", "file"
	Target      string `json:"target,omitempty" yaml:"target,omitempty" koanf:"target"` // Projection target (defaults to Key for env type)
	// AlternativeEnvKeys lists env var names that can satisfy this secret
	// requirement as an alternative. If any of these env vars are present,
	// the file secret is not required. For example, GOOGLE_APPLICATION_CREDENTIALS
	// can substitute for a gcloud-adc file secret.
	AlternativeEnvKeys []string `json:"alternative_env_keys,omitempty" yaml:"alternative_env_keys,omitempty" koanf:"alternative_env_keys"`
}

// SkillReference declares a skill dependency in a template's scion-agent.yaml.
type SkillReference struct {
	URI      string `json:"uri" yaml:"uri" koanf:"uri"`
	As       string `json:"as,omitempty" yaml:"as,omitempty" koanf:"as"`
	Optional bool   `json:"optional,omitempty" yaml:"optional,omitempty" koanf:"optional"`
	// Scope indicates the injection source of this skill reference.
	// Valid values: "hub", "user", "project", "template", "platform", "" (empty = lowest precedence).
	// Used for destination-name collision resolution: higher-scope skills win.
	Scope string `json:"scope,omitempty" yaml:"scope,omitempty" koanf:"scope"`
}

// SkillInjectionEntry is the API representation of one injected-skills list entry.
type SkillInjectionEntry struct {
	ID           string `json:"id,omitempty"` // set on read, not required on write
	SkillURI     string `json:"skillUri"`
	SkillAs      string `json:"skillAs,omitempty"`
	Optional     bool   `json:"optional,omitempty"`
	AllowProgeny bool   `json:"allowProgeny,omitempty"` // Allow creator's progeny agents to access (user scope only)
	SortOrder    int    `json:"sortOrder,omitempty"`
	// Enriched fields (populated when URI maps to a known skill bank entry):
	SkillName string `json:"skillName,omitempty"`
	SkillSlug string `json:"skillSlug,omitempty"`
}

// SkillInjectionList is the list response wrapper for injected-skills endpoints.
type SkillInjectionList struct {
	Entries []SkillInjectionEntry `json:"entries"`
}

// HubSkillInjectionSetting is the value stored in hub_settings["injected_skills"].
type HubSkillInjectionSetting struct {
	System      []SkillReference `json:"system"`       // seeded from binary, immutable
	UserDefined []SkillReference `json:"user_defined"` // admin-managed
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
	Source string `json:"source"`        // Scope that provided this secret (user, project, runtime_broker)
	Ref    string `json:"ref,omitempty"` // External secret reference (e.g., "gcpsm:projects/123/secrets/name")
}

// EnvKind classifies the origin and delivery channel of an environment
// variable in the dispatch request. Used by the env classification system
// (#127, P3a) to determine how each value should be delivered to the
// sandbox (plain env var, fetched secret, or dispatch-injected secret).
//
// Three states govern classification lookup:
//  1. Key present in a non-nil EnvClassifications map → that kind.
//  2. Key absent from a non-nil map → unclassified → secret (fail-closed + loud).
//  3. Map entirely nil → classification unavailable (version skew). Distinct
//     from state 2 — must NOT be treated as "all secret".
type EnvKind string

const (
	// EnvKindPlain is a non-sensitive operational value delivered via
	// --env KEY=VALUE. Examples: SCION_MODEL, SCION_HUB_NAME, SCION_DEBUG.
	EnvKindPlain EnvKind = "plain"

	// EnvKindSecretFetchable is a value stored in the hub's secret store,
	// deliverable via SCION_SECRET_KEYS and the P2 fetch endpoint.
	// Examples: user-stored API keys, project-scoped GITHUB_TOKEN.
	EnvKindSecretFetchable EnvKind = "secret-fetchable"

	// EnvKindSecretInjected is a value produced at dispatch time that
	// exists in no store. Delivery channel is TBD (P3b).
	// Examples: GITHUB_TOKEN from GitHub App, SCION_DEV_TOKEN,
	// SCION_GIT_CLONE_URL.
	EnvKindSecretInjected EnvKind = "secret-injected"

	// EnvKindSecretBootstrap is a credential that bootstraps the secret
	// delivery channel itself, so it cannot be delivered through that
	// channel. P3b performs no routing for these keys — they are already
	// handled by their own delivery mechanism. This kind says nothing
	// about whether the value reaches argv; see the per-key notes at
	// each classification site.
	//
	// Exactly two values today:
	//
	//   SCION_AUTH_TOKEN — authorises the secret fetch (X-Scion-Agent-Token).
	//     NOT in argv: diverted to ~/.scion/scion-token by
	//     pkg/agent/run.go:761-777 (temp+rename, 0600), then deleted from
	//     opts.Env at :777 before buildAgentEnv at :870. Read by
	//     pkg/hubsync/sync.go:1329. See §3.4.
	//
	//   SCION_TRANSPORT_TOKEN — Google-signed OIDC token for IAP transport.
	//     IN argv: no diversion exists. Accepted exposure. 1h lifetime,
	//     NOT boundable (GenerateIdTokenRequest has no Lifetime field,
	//     unlike GenerateAccessTokenRequest which sets 300s). See §3.4.1.
	//
	EnvKindSecretBootstrap EnvKind = "secret-bootstrap"
)

// ClassifyEnvKey looks up the classification for a key in a classifications
// map, implementing the three-state lookup described on EnvKind:
//
//   - Non-nil map, key present: returns (kind, true).
//   - Non-nil map, key absent: returns ("", false) — caller must treat as
//     unclassified (default secret + loud error).
//   - Nil map: returns ("", false) — caller must distinguish this from the
//     absent-key case via a separate nil check on the map. The nil case
//     means classification data is unavailable (e.g. old hub), not that
//     every key is unclassified.
//
// This function does NOT implement the fail-closed default or the loud
// path — those are the caller's responsibility and belong in P3b's
// consumption logic. P3a only records and tests.
func ClassifyEnvKey(classifications map[string]EnvKind, key string) (EnvKind, bool) {
	if classifications == nil {
		return "", false
	}
	kind, ok := classifications[key]
	return kind, ok
}

// GitCloneConfig specifies how to clone a git repository into the workspace.
// When present, the runtime skips local worktree creation and workspace
// mounting — sciontool clones the repo inside the container at startup.
type GitCloneConfig struct {
	URL    string `json:"url"`              // HTTPS clone URL (without credentials)
	Branch string `json:"branch,omitempty"` // Branch to clone (default: main)
	Depth  *int   `json:"depth,omitempty"`  // Clone depth (nil/omitted = shallow depth 1, 0 = full clone, >0 = that depth)
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

type freshProvisionContextKey struct{}

// ContextWithFreshProvision returns a new context marking this dispatch as a
// fresh provision: an agent directory left over from a same-named agent may
// be wiped and re-cloned. Set only for a create dispatch
// (GoogleCloudPlatform/scion#1931) — start and restart never carry this, so
// GetAgent preserves an existing populated workspace on those paths.
func ContextWithFreshProvision(ctx context.Context) context.Context {
	return context.WithValue(ctx, freshProvisionContextKey{}, true)
}

// IsFreshProvisionFromContext returns true if the context marks this
// dispatch as a fresh provision.
func IsFreshProvisionFromContext(ctx context.Context) bool {
	v, _ := ctx.Value(freshProvisionContextKey{}).(bool)
	return v
}

type runIDContextKey struct{}

// ContextWithRunID returns a new context carrying the run ID of the start
// that is provisioning the agent (ptone/scion#2550), so provisioning can
// record it in agent-info.json as the run that owns the agent's files
// (ptone/scion#2675). An empty runID returns ctx unchanged.
func ContextWithRunID(ctx context.Context, runID string) context.Context {
	if runID == "" {
		return ctx
	}
	return context.WithValue(ctx, runIDContextKey{}, runID)
}

// RunIDFromContext returns the run ID set by ContextWithRunID, or "".
func RunIDFromContext(ctx context.Context) string {
	v, _ := ctx.Value(runIDContextKey{}).(string)
	return v
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

type emptyPerAgentWorkspaceContextKey struct{}

// ContextWithEmptyPerAgentWorkspace returns a new context marking the agent's
// workspace as empty-per-agent (design #2703): a private, initially empty,
// non-git directory at <projectDir>/agents/<slug>/workspace.
func ContextWithEmptyPerAgentWorkspace(ctx context.Context) context.Context {
	return context.WithValue(ctx, emptyPerAgentWorkspaceContextKey{}, true)
}

// IsEmptyPerAgentWorkspaceFromContext returns true if the context marks the
// agent's workspace as empty-per-agent.
func IsEmptyPerAgentWorkspaceFromContext(ctx context.Context) bool {
	v, _ := ctx.Value(emptyPerAgentWorkspaceContextKey{}).(bool)
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

type reprovisionContextKey struct{}

// ContextWithReprovision returns a new context flagged as a reincarnation
// reprovision (design: /scion-volumes/scratchpad/projects/agent-migrate/design.md
// §3.4). Provisioning uses this to force-overwrite content that a normal
// provision call leaves alone once present (e.g. a platform skill directory
// whose embedded content changed in a newer broker binary), while still
// preserving the agent's home directory and workspace/worktree as a whole.
func ContextWithReprovision(ctx context.Context) context.Context {
	return context.WithValue(ctx, reprovisionContextKey{}, true)
}

// IsReprovisionFromContext returns true if the context indicates a
// reincarnation reprovision rather than a normal (first-time or restart)
// provision.
func IsReprovisionFromContext(ctx context.Context) bool {
	v, _ := ctx.Value(reprovisionContextKey{}).(bool)
	return v
}

// ReincarnateEligible implements the eligibility predicate of design §3.4
// Amendment A23, extended for empty-per-agent (miller79/scion#167): a
// `scion reincarnate` reprovision is safe to attempt for a workspace that is
// clone-per-agent (a real GitClone), an explicit mount (no GitClone, but a
// non-empty Workspace), or empty-per-agent (the agent's private
// <agentDir>/workspace directory, reused in place). Both the Hub (deciding
// whether to accept a reincarnate request, using AppliedConfig.GitClone,
// AppliedConfig.Workspace and the project's empty-per-agent mode) and the
// broker (deciding whether Manager.Reprovision may run, using the equivalent
// StartOptions fields) call this one helper so the two gates cannot drift
// apart.
//
// Neither side treats this as sufficient on its own: the Hub additionally
// excludes worktree-per-agent projects (which it can detect from the project
// record but this helper cannot, since GitClone is also set for
// worktree-per-agent agents — see design §3.4 Amendment A4) and gates
// empty-per-agent on a local-disk runtime and the broker's
// ReprovisionEmptyPerAgent capability; the broker additionally requires the
// workspace to already exist on disk — Reprovision never creates, clones,
// pulls, resets, or removes a workspace.
func ReincarnateEligible(hasGitClone bool, workspace string, emptyPerAgent bool) bool {
	return hasGitClone || workspace != "" || emptyPerAgent
}

// IsLocalDiskRuntime reports whether a runtime keeps an empty-per-agent
// workspace as a directory on the broker's own disk, so same-broker
// reincarnation can reuse it in place (miller79/scion#167). It is the one
// allow-list behind both the Hub's reincarnate gate and the broker's
// Manager.Reprovision guard. The values are runtime Name()s ("docker",
// "podman", "container"), plus "apple", which some brokers record as a
// profile Type for the Apple container runtime. Kubernetes, cloud runtimes,
// and empty or unknown values are refused (fail closed).
func IsLocalDiskRuntime(name string) bool {
	switch strings.ToLower(strings.TrimSpace(name)) {
	case "docker", "podman", "container", "apple":
		return true
	default:
		return false
	}
}

type provisionedWorktreeRepoRootContextKey struct{}

// ContextWithProvisionedWorktreeRepoRoot records that the workspace at hand
// (opts.Workspace) is a broker-provisioned worktree-per-agent checkout —
// created by runtimebroker's tryProvisionWorktree, not by a user's
// --workspace override — whose git repo root is repoRoot. Modelled on
// ContextWithGitClone / ContextWithSharedWorkspace: broker-local, never on
// the wire.
//
// pkg/agent/run.go's Start consumes it (ProvisionedWorktreeRepoRootFromContext)
// to set RunConfig.RepoRoot directly, skipping detectRepoRoot's "an explicit
// workspace skips git detection" rule — that rule exists for a user's own
// --workspace override and must not swallow the broker's own
// provisioning. This doc is the canonical statement of that rationale; other
// call sites point back here.
//
// The value is never a ScionConfig field (populated by unmarshaling
// templates, hub inline config, and --config files) or an AgentInfo field
// (agent-info.json lives in agentHome, which the container can write). The
// broker persists it in a broker-owned file under agentDir instead — see
// pkg/agent's writeProvisionedWorktreeRepoRoot/readProvisionedWorktreeRepoRoot
// — and run.go independently validates it against the real filesystem
// (provision.ValidateWorktreeForBase) before trusting it either way.
func ContextWithProvisionedWorktreeRepoRoot(ctx context.Context, repoRoot string) context.Context {
	return context.WithValue(ctx, provisionedWorktreeRepoRootContextKey{}, repoRoot)
}

// ProvisionedWorktreeRepoRootFromContext returns the repo root recorded by
// ContextWithProvisionedWorktreeRepoRoot, or "" if none was set (including
// every non-broker dispatch and every broker dispatch that is not a
// provisioned worktree — e.g. clone-per-agent or a user --workspace
// override).
func ProvisionedWorktreeRepoRootFromContext(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	v, _ := ctx.Value(provisionedWorktreeRepoRootContextKey{}).(string)
	return v
}

type harnessConfigPathContextKey struct{}

// ContextWithHarnessConfigPath records a pre-resolved local directory for the
// agent's harness-config (e.g. one hydrated from the Hub's storage backend).
// Provisioning uses it directly instead of searching the local filesystem.
func ContextWithHarnessConfigPath(ctx context.Context, path string) context.Context {
	return context.WithValue(ctx, harnessConfigPathContextKey{}, path)
}

// HarnessConfigPathFromContext returns the pre-resolved harness-config directory
// from the context, or "" if none was set.
func HarnessConfigPathFromContext(ctx context.Context) string {
	v, _ := ctx.Value(harnessConfigPathContextKey{}).(string)
	return v
}

type templateNameContextKey struct{}

// ContextWithTemplateName records the human-friendly template slug for the
// agent being provisioned. It is used for naming only (agent-info.json and
// labels) and never to locate or load a template.
func ContextWithTemplateName(ctx context.Context, name string) context.Context {
	return context.WithValue(ctx, templateNameContextKey{}, name)
}

// TemplateNameFromContext returns the template slug recorded by
// ContextWithTemplateName, or "" if none was set.
func TemplateNameFromContext(ctx context.Context) string {
	v, _ := ctx.Value(templateNameContextKey{}).(string)
	return v
}

// HubAgentDefaults carries the Hub's operational agent_defaults limit/resource
// fields to a runtime broker, for application at the broker's LOW-precedence
// defaults tier.
//
// These four fields need no Hub-side resolution (unlike default_template and
// default_harness_config, which the Hub must resolve so it can stamp IDs and
// hashes), so they deliberately do NOT travel in AppliedConfig / InlineConfig.
// Anything the Hub stamps into InlineConfig arrives at the broker as
// top-of-chain and would outrank a template that sets max_turns explicitly —
// the exact precedence inversion this channel exists to avoid.
//
// The zero value means "the Hub supplied no defaults"; callers leave the
// pointer nil in that case so an unset field is indistinguishable on the wire
// from a Hub that predates the field. See design §3.2.3.
//
// AutoExposePorts is the Hub's auto-expose-ports default
// (SCION_AUTO_EXPOSE_PORTS). It is the lowest env tier: buildAgentEnv applies
// it only when no higher tier (hub-resolved env, template, harness-config
// env, scion-agent.json) left the key set. Unlike the four limit fields it is
// sent on start and restart as well as create, so a change to the Hub default
// reaches an agent at its next start.
type HubAgentDefaults struct {
	MaxTurns        int           `json:"maxTurns,omitempty"`
	MaxModelCalls   int           `json:"maxModelCalls,omitempty"`
	MaxDuration     string        `json:"maxDuration,omitempty"`
	Resources       *ResourceSpec `json:"resources,omitempty"`
	AutoExposePorts *bool         `json:"autoExposePorts,omitempty"`
	// Experiments lists the hub experiments that change what a broker does
	// for this dispatch and are enabled on the hub (for example
	// experiments.K8sNFSHome). The hub sends only names that are on; an
	// absent name is off.
	Experiments []string `json:"experiments,omitempty"`
}

// ExperimentEnabled reports whether the hub sent name as enabled.
func (d *HubAgentDefaults) ExperimentEnabled(name string) bool {
	if d == nil {
		return false
	}
	for _, e := range d.Experiments {
		if e == name {
			return true
		}
	}
	return false
}

// IsEmpty reports whether no default carries a value. An empty set is not put
// on the wire, which is what keeps file-mode dispatch byte-identical to the
// pre-change behaviour (the file-mode snapshot leaves these fields zero).
func (d *HubAgentDefaults) IsEmpty() bool {
	if d == nil {
		return true
	}
	return d.MaxTurns == 0 && d.MaxModelCalls == 0 && d.MaxDuration == "" && d.Resources == nil &&
		d.AutoExposePorts == nil && len(d.Experiments) == 0
}

// EnvAutoExposePorts is the env key that enables in-container port
// auto-exposure (read by sciontool's autoexpose.ConfigFromEnv).
const EnvAutoExposePorts = "SCION_AUTO_EXPOSE_PORTS"

// DefaultEnv returns the env entries the Hub defaults contribute at the
// lowest env tier, or nil when there are none. Callers apply each entry only
// when the key is otherwise unset.
func (d *HubAgentDefaults) DefaultEnv() map[string]string {
	if d == nil || d.AutoExposePorts == nil {
		return nil
	}
	return map[string]string{EnvAutoExposePorts: strconv.FormatBool(*d.AutoExposePorts)}
}

type hubAgentDefaultsContextKey struct{}

// ContextWithHubAgentDefaults records the Hub's operational agent_defaults on
// the context so provisioning can apply them at its own low-precedence tier.
// Modelled on ContextWithBrokerMode / ContextWithHarnessConfigPath: the value
// rides the context rather than growing ProvisionAgent's already-long
// parameter list.
func ContextWithHubAgentDefaults(ctx context.Context, d *HubAgentDefaults) context.Context {
	return context.WithValue(ctx, hubAgentDefaultsContextKey{}, d)
}

// HubAgentDefaultsFromContext returns the Hub's operational agent_defaults from
// the context, or nil if the Hub sent none (local mode, file-mode Hub, or a Hub
// that predates the field).
func HubAgentDefaultsFromContext(ctx context.Context) *HubAgentDefaults {
	if ctx == nil {
		return nil
	}
	v, _ := ctx.Value(hubAgentDefaultsContextKey{}).(*HubAgentDefaults)
	return v
}

type StartOptions struct {
	Name              string
	Task              string
	Template          string
	TemplateName      string // Human-friendly template slug (overrides Template for labels when hydration replaces Template with a cache path)
	Profile           string
	HarnessConfig     string
	HarnessConfigPath string // Resolved local dir for the harness-config (set when hydrated from the Hub); bypasses on-disk FindHarnessConfigDir lookup
	HarnessAuth       string // Late-binding override for auth_selected_type (api-key, oauth-token, auth-file, vertex-ai)
	Image             string
	ProjectPath       string
	Env               map[string]string
	ResolvedSecrets   []ResolvedSecret
	BrokerMode        bool // When true, auth gathering skips local sources (broker env + filesystem)
	// TrustedHubEndpoint is the broker's own operator-derived resolution of
	// the hub endpoint — set only in BrokerMode, only from the request
	// HubEndpoint, the hub connection endpoint, or this broker's configured
	// HubEndpoint (never from ResolvedEnv/Config.Env, which a project or
	// template creator controls). It is empty when none
	// of those operator tiers produced a value, even if Env's own
	// SCION_HUB_ENDPOINT is non-empty. Runtime.Run's substrate egress
	// allowlist is the one consumer that must read this field instead of
	// Env["SCION_HUB_ENDPOINT"] — see pkg/agent/run.go and
	// pkg/runtime/substrate_egress.go.
	TrustedHubEndpoint string
	Detached           *bool
	Resume             bool
	NoAuth             bool
	Branch             string
	Workspace          string
	GitClone           *GitCloneConfig // When set, skip workspace creation; sciontool clones inside container
	SharedWorkspace    bool            // When true, workspace is a shared git clone (git-workspace hybrid); skip worktree, configure credential helper
	// SharedWorkspaceClone holds the clone settings of a shared-plain git
	// project's workspace. Set only with SharedWorkspace and without
	// GitClone. It does not change how the workspace is mounted or created:
	// only the Kubernetes runtime uses it, through RunConfig.GitCloneForInit,
	// so the workspace-provision init container clones into an NFS-backed
	// shared workspace that has not been cloned yet.
	SharedWorkspaceClone *GitCloneConfig
	// FreshProvision marks this dispatch as a create, not a start or restart:
	// GetAgent wipes and re-clones an existing populated workspace only when
	// this is set, so a same-named leftover agent directory is not confused
	// with a start/restart that must preserve un-pushed work
	// (GoogleCloudPlatform/scion#1931).
	FreshProvision    bool
	TelemetryOverride *bool        // Explicit telemetry override from CLI flags (--enable-telemetry / --disable-telemetry)
	InlineConfig      *ScionConfig // Inline config from --config flag, merged over template config
	SharedDirs        []SharedDir  // Project-level shared directories (from Hub, merged with settings)
	ExtraHosts        []string     // Extra --add-host entries for container networking (e.g. "example.com:host-gateway")

	// EmptyPerAgentWorkspace gives the agent a private, initially empty,
	// non-git workspace at <projectDir>/agents/<slug>/workspace (design
	// #2703). Mutually exclusive with Workspace, GitClone and SharedWorkspace.
	EmptyPerAgentWorkspace bool

	// ProjectPreStartHookScript is the project-owner-supplied shell script
	// inlined from the project's active ProjectPreStartHook at agent-create
	// time. If non-empty, the broker writes it to pre-start.d/30-project-custom
	// before the agent container starts.
	ProjectPreStartHookScript string

	// Checkpoint, when set, is called by the runtime immediately before each
	// resource-creating call (an async launch's pre-create gate, design
	// t1-async-create-v11.md §3.8.3). A non-nil error stops the launch
	// before that resource is created, and the runtime returns it (wrapped).
	// step names the resource about to be created (e.g. "secrets",
	// "pod_create"). Nil (the synchronous path) means no gate.
	Checkpoint func(ctx context.Context, step string) error
	// OnResourceCreated, when set, is called by the runtime after each true
	// create of a launch-owned runtime resource, with the created object's
	// identity (design §3.8.4), so an aborted launch can delete exactly what
	// it created. Nil (the synchronous path) means nothing is recorded.
	// Setting it also makes the caller the owner of a failed or cancelled
	// start's cleanup: the runtime then skips its own start cleanup and
	// leaves the reported resources to the caller. Set both hooks together.
	OnResourceCreated func(ResourceHandle)

	// RunID is the per-run identity minted by the hub for this create/start
	// dispatch (ptone/scion#2550). It is applied to the runtime entry as the
	// LabelRunID label. When empty (local/CLI mode, or an older hub) the
	// agent manager mints a UUID itself, so every new entry carries one.
	RunID string

	// ResolvedKubernetesServiceAccountName is the Kubernetes ServiceAccount
	// resolved by the broker from the operator-configured GSA mapping;
	// applied over the template and persisted config at start, when
	// non-empty. It is not part of InlineConfig because it must also apply
	// when starting or restarting an existing agent, whose Kubernetes config
	// otherwise comes only from the template chain and the persisted config,
	// not from InlineConfig.
	ResolvedKubernetesServiceAccountName string
}

// ResourceHandle identifies one runtime resource created during a launch
// (design t1-async-create-v11.md §3.8.4), reported by the runtime via
// StartOptions.OnResourceCreated after each true create. UID is the
// identity a cleanup deletes by: the Kubernetes object UID, or the
// container ID for container runtimes. Namespace is empty for runtimes
// that have no namespaces.
type ResourceHandle struct {
	Kind      string // one of the ResourceKind* constants
	Namespace string
	Name      string
	UID       string
}

// LabelRunID is the runtime label carrying an entry's per-run identity
// (StartOptions.RunID, AgentInfo.RunID). A delete carrying a run ID only
// targets the entry with that label (ptone/scion#2550).
const LabelRunID = "scion.run_id"

// Error-detail keys a runtime broker sets on a start or restart failure
// that happened inside Manager.Start (ptone/scion#2550). By then the broker
// has acted: Start may already have removed the previous same-name entry
// (and restart has stopped it) and may have created a new entry labelled
// with the requested run. The hub must therefore not revert its run ID to
// the previous one. An error without DetailStartAttempted is a rejection
// from before Manager.Start, or from a broker that predates the marker.
const (
	// BrokerErrorDetailStartAttempted is true when the failure came from
	// Manager.Start.
	BrokerErrorDetailStartAttempted = "startAttempted"
	// BrokerErrorDetailRunID carries the run ID the failed start used,
	// when it had one.
	BrokerErrorDetailRunID = "runId"
	// BrokerErrorDetailCurrentRunID carries the run the runtime holds for
	// the agent after the failed start, from one re-list of every runtime
	// on the broker (scoped to the request's project) on the failure path:
	// the scion.run_id of the agent's single container entry, or "" when it
	// is unlabelled or entries of several runs exist (so a delete falls back
	// to the by-name resolution). It is absent when no container entry is
	// left on any runtime, or when the re-list failed; the hub then keeps
	// the run it minted. When present, the hub records it in place of the
	// run it minted, so its next delete targets what actually exists.
	BrokerErrorDetailCurrentRunID = "currentRunId"
)

// ResourceHandle.Kind values.
const (
	ResourceKindSecret              = "secret"
	ResourceKindSecretProviderClass = "secretproviderclass"
	ResourceKindPod                 = "pod"
	ResourceKindContainer           = "container"
)

type StatusEvent struct {
	AgentID   string `json:"agent_id"`
	Status    string `json:"status"`
	Message   string `json:"message,omitempty"`
	Timestamp string `json:"timestamp"`
}

// Visibility constants for resource access control (skills, templates, harness configs).
const (
	VisibilityPrivate = "private" // Only the owner can access
	VisibilityTeam    = "team"    // Team members can access
	VisibilityPublic  = "public"  // Anyone can access (read-only)
)

// ProjectInfo contains metadata about a project (project/agent group).
// It supports both local/solo mode and hosted/distributed mode.
type ProjectInfo struct {
	// Identity fields
	ID   string `json:"id,omitempty"` // UUID (hosted) or empty (solo)
	Name string `json:"name"`         // Human-friendly display name
	Slug string `json:"slug"`         // URL-safe identifier

	// Location
	Path string `json:"path,omitempty"` // Filesystem path (solo mode)

	// Timestamps
	Created time.Time `json:"created,omitempty"` // When the project was created
	Updated time.Time `json:"updated,omitempty"` // Last modification timestamp

	// Ownership
	CreatedBy string `json:"createdBy,omitempty"` // User/system that created the project
	OwnerID   string `json:"ownerId,omitempty"`   // Current owner user ID

	// Metadata
	Labels      map[string]string `json:"labels,omitempty"`
	Annotations map[string]string `json:"annotations,omitempty"`

	// Hosted mode fields
	HubEndpoint string `json:"hubEndpoint,omitempty"` // Scion Hub URL if registered

	// Statistics (computed, not persisted)
	AgentCount int `json:"agentCount,omitempty"` // Number of agents in this project
}

// ProjectID returns the hosted-format project ID (<uuid>__<slug>) if available,
// otherwise returns the Name or Slug as a fallback.
func (g *ProjectInfo) ProjectID() string {
	if g.ID != "" && g.Slug != "" {
		return g.ID + ProjectIDSeparator + g.Slug
	}
	if g.Slug != "" {
		return g.Slug
	}
	return g.Name
}
