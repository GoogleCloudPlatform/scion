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

package runtimebroker

import (
	"strings"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/messages"
)

// ============================================================================
// Health & Info Types
// ============================================================================

// HealthResponse is the response for health check endpoints.
type HealthResponse struct {
	Status  string            `json:"status"`
	Version string            `json:"version"`
	Uptime  string            `json:"uptime"`
	Checks  map[string]string `json:"checks,omitempty"`
}

// HealthStatus returns the status string from the health response.
// This enables interface-based status checking from the web handler.
func (h *HealthResponse) HealthStatus() string {
	return h.Status
}

// BrokerInfoResponse is the response for the /api/v1/info endpoint.
type BrokerInfoResponse struct {
	BrokerID     string              `json:"brokerId"`
	Name         string              `json:"name,omitempty"`
	Version      string              `json:"version"`
	Capabilities *BrokerCapabilities `json:"capabilities,omitempty"`
	Profiles     []BrokerProfile     `json:"profiles,omitempty"`
	Groves       []GroveInfo         `json:"groves,omitempty"`
}

// BrokerProfile describes a runtime profile available on a broker.
type BrokerProfile struct {
	Name      string `json:"name"`
	Type      string `json:"type"`
	Available bool   `json:"available"`
	Context   string `json:"context,omitempty"`
	Namespace string `json:"namespace,omitempty"`
}

// BrokerCapabilities describes what this runtime broker can do.
type BrokerCapabilities struct {
	WebPTY bool `json:"webPty"`
	Sync   bool `json:"sync"`
	Attach bool `json:"attach"`
	Exec   bool `json:"exec"`
}

// GroveInfo is a summary of a grove registered on this broker.
type GroveInfo struct {
	GroveID    string `json:"groveId"`
	GroveName  string `json:"groveName"`
	GitRemote  string `json:"gitRemote,omitempty"`
	AgentCount int    `json:"agentCount"`
}

// ============================================================================
// Hub Connection Status Types
// ============================================================================

// HubConnectionStatusResponse is the response for the /api/v1/hub-connections endpoint.
type HubConnectionStatusResponse struct {
	Connections []HubConnectionInfo `json:"connections"`
	Mode        string              `json:"mode"` // "single-hub" or "multi-hub"
}

// HubConnectionInfo describes the live status of a single hub connection.
type HubConnectionInfo struct {
	Name              string `json:"name"`
	HubEndpoint       string `json:"hubEndpoint"`
	BrokerID          string `json:"brokerId"`
	AuthMode          string `json:"authMode,omitempty"`
	Status            string `json:"status"` // "connected", "disconnected", "error"
	IsColocated       bool   `json:"isColocated,omitempty"`
	HasHeartbeat      bool   `json:"hasHeartbeat"`
	HasControlChannel bool   `json:"hasControlChannel"`
}

// ============================================================================
// Agent Types
// ============================================================================

// AgentResponse represents an agent in API responses.
type AgentResponse struct {
	ID            string `json:"id,omitempty"`          // Hub UUID
	Slug          string `json:"slug"`                  // URL-safe identifier
	ContainerID   string `json:"containerId,omitempty"` // Runtime container ID
	Name          string `json:"name"`
	Template      string `json:"template,omitempty"`      // Template name used
	HarnessConfig string `json:"harnessConfig,omitempty"` // Resolved harness-config name
	// HarnessConfigRevision records the harness-config bundle revision (e.g.
	// the Hub artifact ContentHash) used to provision this agent. Empty for
	// built-in or local-only configs without a tracked revision.
	HarnessConfigRevision string            `json:"harnessConfigRevision,omitempty"`
	HarnessAuth           string            `json:"harnessAuth,omitempty"` // Resolved harness auth method
	Image                 string            `json:"image,omitempty"`       // Resolved container image
	RuntimeType           string            `json:"runtime,omitempty"`     // Runtime type (docker, kubernetes, apple)
	Profile               string            `json:"profile,omitempty"`     // Settings profile used
	GroveID               string            `json:"groveId,omitempty"`
	UserID                string            `json:"userId,omitempty"`
	Status                string            `json:"status"`
	Phase                 string            `json:"phase,omitempty"`
	Activity              string            `json:"activity,omitempty"`
	StatusReason          string            `json:"statusReason,omitempty"`
	Ready                 bool              `json:"ready,omitempty"`
	ContainerStatus       string            `json:"containerStatus,omitempty"`
	Config                *AgentConfig      `json:"config,omitempty"`
	Runtime               *AgentRuntime     `json:"runtimeInfo,omitempty"` // Renamed JSON tag to avoid conflict
	Labels                map[string]string `json:"labels,omitempty"`
	CreatedAt             time.Time         `json:"createdAt,omitempty"`
	UpdatedAt             time.Time         `json:"updatedAt,omitempty"`
}

// AgentConfig contains agent configuration details.
type AgentConfig struct {
	Template  string                `json:"template,omitempty"`
	Image     string                `json:"image,omitempty"`
	HomeDir   string                `json:"homeDir,omitempty"`
	Workspace string                `json:"workspace,omitempty"`
	RepoRoot  string                `json:"repoRoot,omitempty"`
	Harness   string                `json:"harness,omitempty"`
	Env       []string              `json:"env,omitempty"`
	Volumes   []api.VolumeMount     `json:"volumes,omitempty"`
	Resources *api.K8sResources     `json:"resources,omitempty"`
	K8s       *api.KubernetesConfig `json:"kubernetes,omitempty"`
}

// AgentRuntime contains runtime information about the agent.
type AgentRuntime struct {
	ContainerID string    `json:"containerId,omitempty"`
	Node        string    `json:"node,omitempty"`
	StartedAt   time.Time `json:"startedAt,omitempty"`
	IPAddress   string    `json:"ipAddress,omitempty"`
}

// ListAgentsResponse is the response for listing agents.
type ListAgentsResponse struct {
	Agents     []AgentResponse `json:"agents"`
	NextCursor string          `json:"nextCursor,omitempty"`
	TotalCount int             `json:"totalCount"`
}

// CreateAgentRequest is the request body for creating an agent.
type CreateAgentRequest struct {
	RequestID   string             `json:"requestId,omitempty"`
	ID          string             `json:"id,omitempty"`   // Hub UUID for status reporting
	Slug        string             `json:"slug,omitempty"` // URL-safe identifier
	Name        string             `json:"name"`
	GroveID     string             `json:"groveId,omitempty"`
	UserID      string             `json:"userId,omitempty"`
	Config      *CreateAgentConfig `json:"config,omitempty"`
	HubEndpoint string             `json:"hubEndpoint,omitempty"`
	AgentToken  string             `json:"agentToken,omitempty"`

	// ResolvedEnv contains the fully merged environment variables and secrets
	// from all applicable scopes (user, grove, runtime broker). These are resolved
	// by the Hub before dispatching the agent creation request.
	// The Runtime Broker should merge these with config.Env, with config.Env
	// taking precedence over ResolvedEnv.
	ResolvedEnv map[string]string `json:"resolvedEnv,omitempty"`

	// ResolvedSecrets contains type-aware secrets resolved by the Hub.
	// These are projected into the agent container based on their type
	// (environment variable, file, or variable).
	ResolvedSecrets []api.ResolvedSecret `json:"resolvedSecrets,omitempty"`

	// CreatorName is the human-readable identity of who created this agent.
	// Injected as the SCION_CREATOR environment variable in the agent container.
	CreatorName string `json:"creatorName,omitempty"`
	// Attach indicates the agent should start in interactive attach mode (not detached).
	Attach bool `json:"attach,omitempty"`
	// ProvisionOnly indicates the agent should be provisioned (dirs, worktree, templates)
	// but not started. The container will not be launched.
	ProvisionOnly bool `json:"provisionOnly,omitempty"`
	// GrovePath is the local filesystem path to the grove on this runtime broker.
	// This is provided by the Hub from the grove provider record.
	GrovePath string `json:"grovePath,omitempty"`
	// WorkspaceStoragePath is the GCS storage path for bootstrapped workspaces.
	// When set, the broker downloads the workspace from GCS instead of using GrovePath.
	WorkspaceStoragePath string `json:"workspaceStoragePath,omitempty"`

	// GroveSlug is the grove slug for hub-native groves.
	// When set, the broker creates the workspace at ~/.scion/groves/<slug>/
	// instead of the default worktree-based path.
	GroveSlug string `json:"groveSlug,omitempty"`

	// GatherEnv indicates the broker should evaluate env completeness before starting.
	// If required keys are missing, the broker returns HTTP 202 with EnvRequirementsResponse
	// instead of starting the agent, allowing the caller to gather and submit the missing values.
	GatherEnv bool `json:"gatherEnv,omitempty"`

	// RequiredSecrets contains declared secrets from the template config.
	// Passed by the Hub so the broker can include them in env-gather requirements.
	RequiredSecrets []api.RequiredSecret `json:"requiredSecrets,omitempty"`

	// InlineConfig carries the full ScionConfig provided via the Hub API.
	// When set, the broker applies this during agent provisioning, enabling
	// inline configuration without pre-existing templates on the broker.
	InlineConfig *api.ScionConfig `json:"inlineConfig,omitempty"`

	// SharedDirs contains grove-level shared directory declarations.
	// Resolved by the Hub from the grove record and passed to the broker
	// so it can provision host-side directories and inject volume mounts.
	SharedDirs []api.SharedDir `json:"sharedDirs,omitempty"`
}

// CreateAgentConfig contains configuration for agent creation.
type CreateAgentConfig struct {
	Template      string                `json:"template,omitempty"`
	Image         string                `json:"image,omitempty"`
	HomeDir       string                `json:"homeDir,omitempty"`
	Workspace     string                `json:"workspace,omitempty"`
	RepoRoot      string                `json:"repoRoot,omitempty"`
	Env           []string              `json:"env,omitempty"`
	Volumes       []api.VolumeMount     `json:"volumes,omitempty"`
	Labels        map[string]string     `json:"labels,omitempty"`
	Annotations   map[string]string     `json:"annotations,omitempty"`
	HarnessConfig string                `json:"harnessConfig,omitempty"`
	HarnessAuth   string                `json:"harnessAuth,omitempty"` // Late-binding override for auth_selected_type
	Task          string                `json:"task,omitempty"`
	CommandArgs   []string              `json:"commandArgs,omitempty"`
	Profile       string                `json:"profile,omitempty"` // Settings profile for the runtime broker
	Branch        string                `json:"branch,omitempty"`  // Git branch name (defaults to agent slug if empty)
	Kubernetes    *api.KubernetesConfig `json:"kubernetes,omitempty"`

	// TemplateID is the Hub template ID for cache lookup.
	// When provided, the Runtime Broker can use this to look up or fetch
	// the template from the Hub and cache it locally.
	TemplateID string `json:"templateId,omitempty"`

	// TemplateHash is the content hash of the template for cache validation.
	// If the cached template's hash matches, it can be used without re-downloading.
	TemplateHash string `json:"templateHash,omitempty"`

	// GitClone specifies git clone parameters for git-anchored groves.
	// When set, the broker skips workspace mounting and injects env vars
	// so sciontool can clone the repo inside the container.
	GitClone *api.GitCloneConfig `json:"gitClone,omitempty"`

	// SharedWorkspace indicates this agent should use a shared git clone
	// workspace (git-workspace hybrid mode). When true, the broker skips
	// worktree/clone creation and configures per-agent git credentials.
	SharedWorkspace bool `json:"sharedWorkspace,omitempty"`

	// SharedDirs contains grove-level shared directory declarations.
	SharedDirs []api.SharedDir `json:"sharedDirs,omitempty"`

	// GCPIdentity holds the GCP identity assignment for the agent.
	GCPIdentity *GCPIdentityConfig `json:"gcpIdentity,omitempty"`
}

// GCPIdentityConfig holds GCP identity configuration passed from Hub to Broker.
type GCPIdentityConfig struct {
	MetadataMode string `json:"metadata_mode"`        // "block", "passthrough", "assign"
	SAEmail      string `json:"sa_email,omitempty"`   // Service account email
	ProjectID    string `json:"project_id,omitempty"` // GCP project ID
}

// CreateAgentResponse is the response for creating an agent.
type CreateAgentResponse struct {
	Agent   *AgentResponse `json:"agent"`
	Created bool           `json:"created"`
}

// EnvRequirementsResponse is returned by the broker when GatherEnv is true
// and the merged environment is missing required keys. The broker returns
// HTTP 202 with this payload instead of starting the agent.
type EnvRequirementsResponse struct {
	AgentID    string                       `json:"agentId"`
	Required   []string                     `json:"required"`
	HubHas     []string                     `json:"hubHas"`
	BrokerHas  []string                     `json:"brokerHas"` // Deprecated: always empty; kept for API compatibility
	Needs      []string                     `json:"needs"`
	SecretInfo map[string]api.SecretKeyInfo `json:"secretInfo,omitempty"`
}

// FinalizeEnvRequest is sent to the broker to supply gathered env vars
// and complete agent creation after a 202 env-gather response.
type FinalizeEnvRequest struct {
	Env map[string]string `json:"env"`
}

// ============================================================================
// Interaction Types
// ============================================================================

// MessageRequest is the request body for sending a message to an agent.
type MessageRequest struct {
	// Plain text message (legacy field, used for backwards compatibility).
	Message string `json:"message,omitempty"`

	// Structured message (new field, used by default).
	StructuredMessage *messages.StructuredMessage `json:"structured_message,omitempty"`

	// Interrupt the harness before sending.
	Interrupt bool `json:"interrupt,omitempty"`

	// GroveID is the grove ID for the target agent (used for message log labels).
	GroveID string `json:"grove_id,omitempty"`
}

// ExecRequest is the request body for executing a command in an agent.
type ExecRequest struct {
	Command []string `json:"command"`
	Timeout int      `json:"timeout,omitempty"` // Timeout in seconds
}

// ExecResponse is the response for command execution.
type ExecResponse struct {
	Output   string `json:"output"`
	ExitCode int    `json:"exitCode"`
}

// StatsResponse contains resource usage statistics for an agent.
type StatsResponse struct {
	CPUUsagePercent  float64 `json:"cpuUsagePercent"`
	MemoryUsageBytes int64   `json:"memoryUsageBytes"`
	MemoryLimitBytes int64   `json:"memoryLimitBytes,omitempty"`
	NetworkRxBytes   int64   `json:"networkRxBytes,omitempty"`
	NetworkTxBytes   int64   `json:"networkTxBytes,omitempty"`
}

// ============================================================================
// Conversion Functions
// ============================================================================

// AgentInfoToResponse converts an api.AgentInfo to an AgentResponse.
func AgentInfoToResponse(info api.AgentInfo) AgentResponse {
	phase := info.Phase
	activity := info.Activity

	// When Phase/Activity are present (new structured path), derive status
	// via DisplayStatus for backward compatibility.
	// When absent (legacy), fall back to container-status-based mapping.
	var status string
	if phase != "" {
		as := state.AgentState{
			Phase:    state.Phase(phase),
			Activity: state.Activity(activity),
		}
		status = as.DisplayStatus()
	} else if info.Phase != "" {
		// Phase already set on info, use it
		phase = info.Phase
		status = info.Phase
	} else {
		// Legacy fallback: derive phase and status from container status
		switch {
		case info.ContainerStatus == "":
			phase = string(state.PhaseCreated)
			status = string(state.PhaseCreated)
		case containsAny(info.ContainerStatus, "up", "running"):
			phase = string(state.PhaseRunning)
			status = string(state.PhaseRunning)
		case containsAny(info.ContainerStatus, "created"):
			phase = string(state.PhaseProvisioning)
			status = string(state.PhaseProvisioning)
		case containsAny(info.ContainerStatus, "exited", "stopped"):
			phase = string(state.PhaseStopped)
			status = string(state.PhaseStopped)
		default:
			status = info.ContainerStatus
		}
	}

	resp := AgentResponse{
		ID:                    info.ID,
		Slug:                  info.Slug,
		ContainerID:           info.ContainerID,
		Name:                  info.Name,
		Template:              info.Template,
		HarnessConfig:         info.HarnessConfig,
		HarnessConfigRevision: info.HarnessConfigRevision,
		HarnessAuth:           info.HarnessAuth,
		Image:                 info.Image,
		RuntimeType:           info.Runtime,
		Profile:               info.Profile,
		GroveID:               info.GroveID,
		Status:                status,
		Phase:                 phase,
		Activity:              activity,
		ContainerStatus:       info.ContainerStatus,
		Labels:                info.Labels,
		CreatedAt:             info.Created,
		Ready:                 phase == string(state.PhaseRunning),
	}

	if info.Template != "" || info.Image != "" {
		resp.Config = &AgentConfig{
			Template: info.Template,
			Image:    info.Image,
		}
	}

	if info.ContainerID != "" {
		resp.Runtime = &AgentRuntime{
			ContainerID: info.ContainerID,
		}
	}

	return resp
}

// containsAny checks if s contains any of the substrings (case-insensitive).
func containsAny(s string, substrs ...string) bool {
	s = strings.ToLower(s)
	for _, sub := range substrs {
		if strings.Contains(s, strings.ToLower(sub)) {
			return true
		}
	}
	return false
}
