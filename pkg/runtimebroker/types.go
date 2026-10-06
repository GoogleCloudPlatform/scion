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
	"encoding/json"
	"strings"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/hubclient"
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
	Projects     []ProjectInfo       `json:"projects,omitempty"`
	// WorkspaceStorage is the broker's workspace storage descriptor, the
	// same value it reports to the hub on every heartbeat.
	WorkspaceStorage *api.BrokerWorkspaceStorage `json:"workspaceStorage,omitempty"`
}

// BrokerProfile describes a runtime profile available on a broker.
type BrokerProfile struct {
	Name      string `json:"name"`
	Type      string `json:"type"`
	Available bool   `json:"available"`
	Context   string `json:"context,omitempty"`
	Namespace string `json:"namespace,omitempty"`
	// Attach reports whether this profile's runtime supports interactive
	// attach (pkg/runtime.AttachCapableRuntime, via HasAttachSupport). A
	// pointer, not a plain bool: this broker can only answer for a profile
	// backed by a runtime instance it has already built (the default
	// runtime, or an auxiliary runtime some prior request already
	// constructed) — buildInfoProfiles never builds one just to answer this
	// field. nil means unknown (no live instance to ask), which every
	// consumer must read as supported, the same missing-capability default
	// HasAttachSupport itself uses for a runtime that doesn't implement the
	// interface.
	Attach *bool `json:"attach,omitempty"`
}

// BrokerCapabilities describes what this runtime broker can do.
type BrokerCapabilities struct {
	WebPTY bool `json:"webPty"`
	Sync   bool `json:"sync"`
	Attach bool `json:"attach"`
	Exec   bool `json:"exec"`
	// Reprovision indicates this broker supports the reincarnation reprovision
	// primitive (POST .../agents/{id} with provisionOnly+reprovision, design
	// §3.4). The hub gates `scion reincarnate` on this — see
	// store.BrokerCapabilities.Reprovision and its 412 gate in pkg/hub.
	Reprovision bool `json:"reprovision"`
	// AsyncLaunch indicates this broker understands CreateAgentRequest's
	// AsyncLaunch field and the launch-report protocol (design
	// t1-async-create-v11.md §3.2, §7 P1b-1). The hub uses it only to skip
	// BeginLaunch for a broker known to lack support; the create response's
	// LaunchPending echo is authoritative either way.
	AsyncLaunch bool `json:"asyncLaunch"`
	// EmptyPerAgentWorkspace indicates this broker provisions the
	// empty-per-agent workspace sharing mode: a private, initially empty
	// directory at <projectDir>/agents/<slug>/workspace (design #2703). The
	// hub refuses to dispatch such agents to a broker without it (412).
	EmptyPerAgentWorkspace bool `json:"emptyPerAgentWorkspace"`
	// AgentMove indicates this broker can take part in moving an agent to
	// or from another broker on the same workspace export (`scion
	// reincarnate --broker`). The hub refuses a move unless both brokers
	// report it (412).
	AgentMove bool `json:"agentMove"`
	// ReprovisionEmptyPerAgent indicates this broker's reprovision reuses an
	// empty-per-agent agent's private workspace in place (same-broker
	// `scion reincarnate`, miller79/scion#167). Set on every runtime: the
	// hub checks runtime suitability separately.
	ReprovisionEmptyPerAgent bool `json:"reprovisionEmptyPerAgent,omitempty"`
	// StartsInFlight indicates the broker reports the agent starts still
	// running on it in every heartbeat (BrokerHeartbeat.StartsInFlight). Only
	// then does the hub read a start's absence from that list as "no start
	// in flight".
	StartsInFlight bool `json:"startsInFlight,omitempty"`
}

// ProjectInfo is a summary of a project registered on this broker.
type ProjectInfo struct {
	ProjectID   string `json:"projectId"`
	ProjectName string `json:"projectName"`
	GitRemote   string `json:"gitRemote,omitempty"`
	AgentCount  int    `json:"agentCount"`
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
	ProjectID             string            `json:"projectId,omitempty"`
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
	// Warnings carries only the hub-only env drop warnings (a broker-local
	// TZ value ignored for a hub-dispatched agent), so the hub can relay
	// them in its own create and start responses. Other broker-local start
	// warnings are deliberately not included.
	Warnings []string `json:"warnings,omitempty"`
	// RunID is the run identity the runtime entry carries (its scion.run_id
	// label). It usually echoes the runId the hub sent, but a start that
	// found the agent already running reports the existing run's ID, so
	// the hub can record the run that actually exists (ptone/scion#2550).
	RunID string `json:"runId,omitempty"`
	// WorkspacePlacement is where the start this response answers placed
	// the agent's workspace (api.WorkspacePlacementExport or
	// WorkspacePlacementLocal). Empty when no start resolved it.
	WorkspacePlacement string `json:"workspacePlacement,omitempty"`
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
	// workspaceAbsentAtAdmission is set by createAgent when the hub-managed
	// project path ~/.scion/projects/<slug> did not exist when the request
	// arrived. It is never read from the wire.
	workspaceAbsentAtAdmission bool

	RequestID   string             `json:"requestId,omitempty"`
	ID          string             `json:"id,omitempty"`   // Hub UUID for status reporting
	Slug        string             `json:"slug,omitempty"` // URL-safe identifier
	Name        string             `json:"name"`
	ProjectID   string             `json:"projectId,omitempty"`
	UserID      string             `json:"userId,omitempty"`
	Config      *CreateAgentConfig `json:"config,omitempty"`
	HubEndpoint string             `json:"hubEndpoint,omitempty"`
	AgentToken  string             `json:"agentToken,omitempty"`

	// ResolvedEnv contains the fully merged environment variables and secrets
	// from all applicable scopes (user, project, runtime broker). These are resolved
	// by the Hub before dispatching the agent creation request.
	// The Runtime Broker should merge these with config.Env, with config.Env
	// taking precedence over ResolvedEnv.
	ResolvedEnv map[string]string `json:"resolvedEnv,omitempty"`

	// EnvClassifications records the classification (plain, secret-fetchable,
	// secret-injected) for each key in ResolvedEnv (#127, P3a). See
	// api.EnvKind for three-state semantics (present, absent, nil).
	EnvClassifications map[string]api.EnvKind `json:"envClassifications,omitempty"`

	// ResolvedSecrets contains type-aware secrets resolved by the Hub.
	// These are projected into the agent container based on their type
	// (environment variable, file, or variable).
	ResolvedSecrets []api.ResolvedSecret `json:"resolvedSecrets,omitempty"`

	// CreatorName is the human-readable identity of who created this agent.
	// Injected as the SCION_CREATOR environment variable in the agent container.
	CreatorName string `json:"creatorName,omitempty"`
	// NoAuth indicates the agent should start without any injected credentials.
	NoAuth bool `json:"noAuth,omitempty"`
	// Attach indicates the agent should start in interactive attach mode (not detached).
	Attach bool `json:"attach,omitempty"`
	// ProvisionOnly indicates the agent should be provisioned (dirs, worktree, templates)
	// but not started. The container will not be launched.
	ProvisionOnly bool `json:"provisionOnly,omitempty"`
	// Reprovision indicates this ProvisionOnly request targets an existing
	// agent whose on-disk config should be replaced from the current
	// template/harness-config catalog, for a `scion reincarnate` request
	// (design §3.4). The hub always sets ProvisionOnly alongside this. Unlike
	// a plain ProvisionOnly call (which reuses persisted config when the
	// agent directory already exists), Reprovision forces a fresh render
	// while preserving the agent's home directory and clone-per-agent
	// workspace. Ignored when ProvisionOnly is false.
	Reprovision bool `json:"reprovision,omitempty"`
	// ExpectExistingNFSWorkspace, on a ProvisionOnly request, says the agent
	// is being moved here from another broker on the same NFS export: before
	// provisioning, the broker confirms through its own mount that the
	// workspace already exists ("agent-dir": <subPathRoot>/<projectID>/
	// agents/<name>/workspace; "project": <subPathRoot>/<projectID>/
	// workspace) and refuses with 409 when it does not, instead of
	// provisioning an empty workspace (design ptone/scion#2727 A9).
	ExpectExistingNFSWorkspace string `json:"expectExistingNfsWorkspace,omitempty"`
	// ProjectPath is the local filesystem path to the project on this runtime broker.
	// This is provided by the Hub from the project provider record.
	ProjectPath string `json:"projectPath,omitempty"`
	// WorkspaceStoragePath is the GCS storage path for bootstrapped workspaces.
	// When set, the broker downloads the workspace from GCS instead of using ProjectPath.
	WorkspaceStoragePath string `json:"workspaceStoragePath,omitempty"`
	// WorkspaceStorageBucket is the GCS bucket holding WorkspaceStoragePath,
	// sent by the hub that uploaded it. When empty the broker falls back to
	// its own StorageBucket setting (older hubs do not send it).
	WorkspaceStorageBucket string `json:"workspaceStorageBucket,omitempty"`

	// ProjectSlug is the project slug for hub-managed projects.
	// When set, the broker creates the workspace at ~/.scion.projects/<slug>/
	// instead of the default worktree-based path.
	ProjectSlug string `json:"projectSlug,omitempty"`

	// GatherEnv indicates the broker should evaluate env completeness before starting.
	// If required keys are missing, the broker returns HTTP 202 with EnvRequirementsResponse
	// instead of starting the agent, allowing the caller to gather and submit the missing values.
	GatherEnv bool `json:"gatherEnv,omitempty"`

	// AvailableAsNeededKeys lists the target key names of as_needed
	// environment-type secrets that the Hub filtered out of ResolvedSecrets
	// but could resolve in a second pass if the broker reports them as needed.
	// This lets the broker's autodetect consider these keys when selecting
	// auth type, closing the chicken-and-egg gap where autodetect only sees
	// already-resolved keys.
	AvailableAsNeededKeys []string `json:"availableAsNeededKeys,omitempty"`

	// RequiredSecrets contains declared secrets from the template config.
	// Passed by the Hub so the broker can include them in env-gather requirements.
	RequiredSecrets []api.RequiredSecret `json:"requiredSecrets,omitempty"`

	// PreResolvedSkills carries the Hub-registry skills the Hub resolved at
	// dispatch as the agent's creator (#1784). Refs covered here are installed
	// from this payload; the broker's own resolver handles the rest. Nil when
	// the Hub predates this field or had nothing to resolve.
	PreResolvedSkills *hubclient.ResolveSkillsResponse `json:"preResolvedSkills,omitempty"`

	// InlineConfig carries the full ScionConfig provided via the Hub API.
	// When set, the broker applies this during agent provisioning, enabling
	// inline configuration without pre-existing templates on the broker.
	InlineConfig *api.ScionConfig `json:"inlineConfig,omitempty"`

	// SharedDirs contains project-level shared directory declarations.
	// Resolved by the Hub from the project record and passed to the broker
	// so it can provision host-side directories and inject volume mounts.
	SharedDirs []api.SharedDir `json:"sharedDirs,omitempty"`

	// WorkspaceMode is the resolved workspace sharing mode for the project
	// (e.g. "shared", "per-agent", "worktree-per-agent"). Threaded from the
	// Hub so the broker can branch dispatch without re-deriving from labels.
	WorkspaceMode string `json:"workspaceMode,omitempty"`

	// ProvisionCredentials carries project-scope secrets for use by core provision
	// logic (skill resolution, URI variable substitution, credential helpers).
	// These are NEVER forwarded to the agent container environment or harness scripts.
	// Populated by the Hub from project-scope secrets at dispatch time.
	ProvisionCredentials map[string]string `json:"provisionCredentials,omitempty"`

	// AsyncLaunch requests the non-blocking create path (design
	// t1-async-create-v11.md §3.2, §7 P1b-1). With it absent or false,
	// createAgent's behavior is unchanged. ProvisionOnly and Reprovision
	// ignore it.
	AsyncLaunch bool `json:"asyncLaunch,omitempty"`
	// LaunchID is the Hub's launch identifier (BeginLaunch's return value),
	// echoed back on every report for this launch.
	LaunchID string `json:"launchId,omitempty"`
	// RunID is the Hub-minted identity of the run this create starts
	// (ptone/scion#2550), distinct from LaunchID. The broker labels the
	// runtime entry with it (api.LabelRunID) so a later delete carrying it
	// targets only this run. Empty from an older hub; pkg/agent then mints
	// one itself.
	RunID string `json:"runId,omitempty"`
	// LaunchTimeoutSeconds is the remaining launch budget at send time
	// (ceil(launch_deadline - send time)), not the Hub's configured
	// launchTimeout setting.
	LaunchTimeoutSeconds int `json:"launchTimeoutSeconds,omitempty"`
	// LaunchKeepaliveSeconds is the Hub's configured keepalive interval. The
	// broker defaults to 15 when absent (design §3.7).
	LaunchKeepaliveSeconds int `json:"launchKeepaliveSeconds,omitempty"`
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

	// HarnessConfigID is the Hub harness-config ID for cache lookup/hydration.
	// When set, the broker fetches the harness-config from the Hub's storage
	// backend instead of requiring it on the broker's local filesystem.
	HarnessConfigID string `json:"harnessConfigId,omitempty"`

	// HarnessConfigHash is the content hash of the harness-config for cache
	// validation, mirroring TemplateHash.
	HarnessConfigHash string `json:"harnessConfigHash,omitempty"`

	// GitClone specifies git clone parameters for git-anchored projects.
	// When set, the broker skips workspace mounting and injects env vars
	// so sciontool can clone the repo inside the container.
	GitClone *api.GitCloneConfig `json:"gitClone,omitempty"`

	// SharedWorkspace indicates this agent should use a shared git clone
	// workspace (git-workspace hybrid mode). When true, the broker skips
	// worktree/clone creation and configures per-agent git credentials.
	SharedWorkspace bool `json:"sharedWorkspace,omitempty"`

	// SharedWorkspaceClone is a shared-plain git project's workspace clone
	// settings, sent by the Hub alongside SharedWorkspace. It never turns on
	// the per-agent clone mode GitClone does; it reaches the runtime only as
	// the clone settings of the Kubernetes workspace-provision init container
	// (api.StartOptions.SharedWorkspaceClone).
	SharedWorkspaceClone *api.GitCloneConfig `json:"sharedWorkspaceClone,omitempty"`

	// SharedDirs contains project-level shared directory declarations.
	SharedDirs []api.SharedDir `json:"sharedDirs,omitempty"`

	// GCPIdentity holds the GCP identity assignment for the agent.
	GCPIdentity *GCPIdentityConfig `json:"gcpIdentity,omitempty"`

	// ProjectPreStartHookScript is the active project pre-start hook script,
	// inlined at agent-create time. The broker writes it to
	// pre-start.d/30-project-custom before the agent container starts.
	ProjectPreStartHookScript string `json:"projectPreStartHookScript,omitempty"`

	// HubAgentDefaults carries the hub's operational agent_defaults
	// (limits/resources). Applied during provisioning at a tier BELOW the
	// template and inline config and ABOVE this broker's own settings.yaml
	// defaults — deliberately not merged into InlineConfig, which is a
	// top-of-chain slot. Nil when the hub sent none: local dispatch, a
	// file-mode hub, or a hub that predates the field. See design §3.2.3.
	HubAgentDefaults *api.HubAgentDefaults `json:"hubAgentDefaults,omitempty"`
}

// GCPIdentityConfig holds GCP identity configuration passed from Hub to Broker.
type GCPIdentityConfig struct {
	MetadataMode string `json:"metadata_mode"`        // "block", "passthrough", "assign"
	SAEmail      string `json:"sa_email,omitempty"`   // Service account email
	ProjectID    string `json:"project_id,omitempty"` // GCP project ID

	// RequireLocalRuntime marks a "passthrough" mode granted by the hub's
	// hub-default identity rung, which resolves the runtime this agent will
	// use from the broker's own registration data rather than from
	// project-effective settings at dispatch time. When this is set, the
	// broker re-checks the resolved runtime once it knows it (after
	// resolveManagerForOpts) and downgrades passthrough to block itself if
	// that runtime is not a local container runtime — see buildStartContext.
	// Explicit and project-level passthrough are never flagged, so their
	// behavior is unaffected. The JSON tag must match
	// hub.RemoteGCPIdentityConfig's field of the same name.
	RequireLocalRuntime bool `json:"require_local_runtime,omitempty"`
}

// CreateAgentResponse is the response for creating an agent.
type CreateAgentResponse struct {
	Agent   *AgentResponse `json:"agent"`
	Created bool           `json:"created"`

	// Reprovisioned is set true ONLY on the branch of handleCreateAgent that
	// actually ran Manager.Reprovision (design §3.4 Amendment A2.2(a)).
	// A broker that predates the reincarnate feature has no field named
	// "reprovision" in its request handling at all, so it silently runs a
	// plain Provision for a request that set Reprovision=true — the hub
	// treats an absent echo on a reprovision dispatch as a failure, which is
	// what keeps that failure mode closed instead of a silent no-op.
	Reprovisioned bool `json:"reprovisioned,omitempty"`

	// LaunchPending is set true instead of running Manager.Start inline when
	// the broker accepted an async launch (design §3.2, §7 P1b-1): the Hub
	// writes MarkLaunchAccepted and the broker continues in runLaunch. Agent
	// is nil on this branch — the launch is not running yet.
	LaunchPending bool `json:"launchPending,omitempty"`
	// LaunchID echoes the request's LaunchID, so the sending Hub node can
	// confirm this is an answer to its own BeginLaunch before calling
	// MarkLaunchAccepted (design §3.4 dispatchLaunching).
	LaunchID string `json:"launchId,omitempty"`
	// LaunchInstanceID is this broker process's launch-owner identity,
	// generated once at broker start. The Hub stores it as launch_owner.
	LaunchInstanceID string `json:"launchInstanceId,omitempty"`
}

// EnvRequirementsResponse is returned by the broker when GatherEnv is true
// and the merged environment is missing required keys. The broker returns
// HTTP 202 with this payload instead of starting the agent.
type EnvRequirementsResponse struct {
	AgentID      string                       `json:"agentId"`
	Required     []string                     `json:"required"`
	HubHas       []string                     `json:"hubHas"`
	BrokerHas    []string                     `json:"brokerHas"` // Deprecated: always empty; kept for API compatibility
	Needs        []string                     `json:"needs"`
	SecretInfo   map[string]api.SecretKeyInfo `json:"secretInfo,omitempty"`
	Alternatives map[string][]string          `json:"alternatives,omitempty"` // Maps canonical key in Needs to alternative key names from the same any_of group
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

	// DeliveryText is the fully rendered agent-facing envelope, produced by
	// the hub (Phase 9b). When set, the broker delivers it verbatim and
	// performs no formatting. Takes precedence over StructuredMessage
	// rendering and Message. Phase 13 deletes this field.
	DeliveryText string `json:"delivery_text,omitempty"`

	// Interrupt the harness before sending.
	Interrupt bool `json:"interrupt,omitempty"`

	// ProjectID is the project ID for the target agent (used for message log labels).
	ProjectID string `json:"projectId,omitempty"`

	// MessageID is the hub's persisted message ID, when the hub wants to be
	// told about a buffered delivery that fails after acceptance (#1820).
	MessageID string `json:"message_id,omitempty"`
}

// UnmarshalJSON implements custom unmarshaling to support the legacy
// snake_case project_id field alongside the canonical projectId.
func (r *MessageRequest) UnmarshalJSON(data []byte) error {
	type Alias MessageRequest
	aux := &struct {
		LegacyProjID string `json:"project_id"`
		*Alias
	}{
		Alias: (*Alias)(r),
	}
	if err := json.Unmarshal(data, &aux); err != nil {
		return err
	}
	if r.ProjectID == "" && aux.LegacyProjID != "" {
		r.ProjectID = aux.LegacyProjID
	}
	return nil
}

// MarshalJSON implements custom marshaling to support the legacy snake_case
// project_id field alongside the canonical projectId.
func (r MessageRequest) MarshalJSON() ([]byte, error) {
	type Alias MessageRequest
	return json.Marshal(&struct {
		Alias
		LegacyProjID string `json:"project_id,omitempty"`
	}{
		Alias:        Alias(r),
		LegacyProjID: r.ProjectID,
	})
}

// ExecRequest is the request body for executing a command in an agent.
type ExecRequest struct {
	Command []string `json:"command"`
	Timeout int      `json:"timeout,omitempty"` // Timeout in seconds
}

// ResetAuthRequest is the request body for resetting auth on a running agent.
type ResetAuthRequest struct {
	Token string `json:"token"`
	// TransportToken, when set, is a fresh hub-minted transport token
	// (IAP / Cloud Run invoker). It is written to the agent's transport
	// token file alongside the agent token, so a reset also recovers an
	// agent whose transport token has expired.
	TransportToken string `json:"transportToken,omitempty"`
}

// ResetAuthResponse is the response for auth reset.
type ResetAuthResponse struct {
	Message string `json:"message"`
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
		// Legacy fallback: derive phase from ContainerStatus for runtimes
		// that don't populate Phase directly. All current runtimes populate
		// Phase as of #1257, but this remains for backward compatibility.
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
		RunID:                 info.RunID,
		Name:                  info.Name,
		Template:              info.Template,
		HarnessConfig:         info.HarnessConfig,
		HarnessConfigRevision: info.HarnessConfigRevision,
		HarnessAuth:           info.HarnessAuth,
		Image:                 info.Image,
		RuntimeType:           info.Runtime,
		Profile:               info.Profile,
		ProjectID:             info.ProjectID,
		Status:                status,
		Phase:                 phase,
		Activity:              activity,
		ContainerStatus:       info.ContainerStatus,
		Labels:                info.Labels,
		CreatedAt:             info.Created,
		Ready:                 phase == string(state.PhaseRunning),
		WorkspacePlacement:    info.WorkspacePlacement,
	}
	if len(info.HubOnlyEnvWarnings) > 0 {
		resp.Warnings = append([]string(nil), info.HubOnlyEnvWarnings...)
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

// containsAny is used by the legacy ContainerStatus-to-Phase fallback.
// It checks if s contains any of the substrings (case-insensitive).
func containsAny(s string, substrs ...string) bool {
	s = strings.ToLower(s)
	for _, sub := range substrs {
		if strings.Contains(s, strings.ToLower(sub)) {
			return true
		}
	}
	return false
}
