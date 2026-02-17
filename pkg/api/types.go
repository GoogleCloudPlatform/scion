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

type GeminiConfig struct {
	AuthSelectedType string `json:"auth_selectedType,omitempty" yaml:"auth_selectedType,omitempty"`
}

// AgentHubConfig holds hub connection settings that can be specified per-agent
// or per-template in scion-agent.yaml. When set, these take highest priority
// for the agent's hub endpoint, overriding grove settings and server config.
type AgentHubConfig struct {
	Endpoint string `json:"endpoint,omitempty" yaml:"endpoint,omitempty"`
}

type ScionConfig struct {
	Harness       string            `json:"harness,omitempty" yaml:"harness,omitempty"`
	HarnessConfig string            `json:"harness_config,omitempty" yaml:"harness_config,omitempty"`
	ConfigDir     string            `json:"config_dir,omitempty" yaml:"config_dir,omitempty"`
	Env         map[string]string `json:"env,omitempty" yaml:"env,omitempty"`
	Volumes     []VolumeMount     `json:"volumes,omitempty" yaml:"volumes,omitempty"`
	Detached    *bool             `json:"detached" yaml:"detached"`
	CommandArgs []string          `json:"command_args,omitempty" yaml:"command_args,omitempty"`
	Model       string            `json:"model,omitempty" yaml:"model,omitempty"`
	Kubernetes  *KubernetesConfig `json:"kubernetes,omitempty" yaml:"kubernetes,omitempty"`
	Gemini      *GeminiConfig     `json:"gemini,omitempty" yaml:"gemini,omitempty"`
	Resources   *ResourceSpec     `json:"resources,omitempty" yaml:"resources,omitempty"`
	Image       string            `json:"image,omitempty" yaml:"image,omitempty"`
	Services    []ServiceSpec     `json:"services,omitempty" yaml:"services,omitempty"`
	MaxTurns    int               `json:"max_turns,omitempty" yaml:"max_turns,omitempty"`
	MaxDuration string            `json:"max_duration,omitempty" yaml:"max_duration,omitempty"`
	Hub         *AgentHubConfig   `json:"hub,omitempty" yaml:"hub,omitempty"`

	// Agnostic template fields (Phase 2)
	AgentInstructions  string `json:"agent_instructions,omitempty" yaml:"agent_instructions,omitempty"`
	SystemPrompt       string `json:"system_prompt,omitempty" yaml:"system_prompt,omitempty"`
	DefaultHarnessConfig string `json:"default_harness_config,omitempty" yaml:"default_harness_config,omitempty"`

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
	ID          string `json:"id,omitempty"`          // Hub UUID (database primary key, globally unique)
	Slug        string `json:"slug,omitempty"`        // URL-safe slug identifier (unique per grove)
	ContainerID string `json:"containerId,omitempty"` // Runtime container ID (ephemeral, runtime-assigned)
	Name          string `json:"name"`                  // Human-friendly display name
	Template      string `json:"template"`
	HarnessConfig string `json:"harnessConfig,omitempty"` // Resolved harness-config name

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
	RuntimeBrokerID   string `json:"runtimeBrokerId,omitempty"`   // ID of the Runtime Broker managing this agent
	RuntimeBrokerName string `json:"runtimeBrokerName,omitempty"` // Name of the Runtime Broker
	RuntimeBrokerType string `json:"runtimeBrokerType,omitempty"` // Type: docker, kubernetes, apple
	RuntimeState    string `json:"runtimeState,omitempty"`    // Low-level runtime state
	HubEndpoint     string `json:"hubEndpoint,omitempty"`     // Scion Hub URL if connected
	WebPTYEnabled   bool   `json:"webPtyEnabled,omitempty"`   // Whether web terminal access is available
	TaskSummary     string `json:"taskSummary,omitempty"`     // Current task description (for dashboard)

	// Optimistic locking
	StateVersion int64 `json:"stateVersion,omitempty"` // Version for concurrent update detection
}

// ResolvedSecret represents a secret that has been resolved from the Hub
// and is ready for projection into an agent container.
type ResolvedSecret struct {
	Name   string `json:"name"`           // Secret key name
	Type   string `json:"type"`           // environment, variable, file
	Target string `json:"target"`         // Projection target (env var name, json key, or file path)
	Value  string `json:"value"`          // Decrypted secret value
	Source string `json:"source"`         // Scope that provided this secret (user, grove, runtime_broker)
}

type StartOptions struct {
	Name            string
	Task            string
	Template        string
	Profile         string
	HarnessConfig   string
	Image           string
	GrovePath       string
	Env             map[string]string
	ResolvedSecrets []ResolvedSecret
	Detached        *bool
	Resume          bool
	Auth            AuthProvider
	NoAuth          bool
	Branch          string
	Workspace       string
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
