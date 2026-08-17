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

package hub

import (
	"bufio"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/config/opsettings"
	"github.com/GoogleCloudPlatform/scion/pkg/ent"
	"github.com/GoogleCloudPlatform/scion/pkg/eventbus"
	"github.com/GoogleCloudPlatform/scion/pkg/harness"
	"github.com/GoogleCloudPlatform/scion/pkg/hub/githubapp"
	"github.com/GoogleCloudPlatform/scion/pkg/hub/imagecheck"
	"github.com/GoogleCloudPlatform/scion/pkg/lifecyclehooks"
	"github.com/GoogleCloudPlatform/scion/pkg/messages"
	"github.com/GoogleCloudPlatform/scion/pkg/observability/dbmetrics"
	"github.com/GoogleCloudPlatform/scion/pkg/observability/dispatchmetrics"
	"github.com/GoogleCloudPlatform/scion/pkg/secret"
	"github.com/GoogleCloudPlatform/scion/pkg/storage"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/GoogleCloudPlatform/scion/pkg/util/logging"
	"github.com/google/uuid"
	"github.com/robfig/cron/v3"
)

const (
	// SecretKeyAgentSigningKey is the secret key for the agent token signing key.
	SecretKeyAgentSigningKey = "agent_signing_key"
	// SecretKeyUserSigningKey is the secret key for the user token signing key.
	SecretKeyUserSigningKey = "user_signing_key"
)

// ServerConfig holds configuration for the Hub API server.
type ServerConfig struct {
	// Port is the HTTP port to listen on.
	Port int
	// Host is the address to bind to (e.g., "0.0.0.0" or "127.0.0.1").
	Host string
	// ReadTimeout is the maximum duration for reading the entire request.
	ReadTimeout time.Duration
	// WriteTimeout is the maximum duration before timing out writes.
	WriteTimeout time.Duration
	// CORS settings
	CORSEnabled        bool
	CORSAllowedOrigins []string
	CORSAllowedMethods []string
	CORSAllowedHeaders []string
	CORSMaxAge         int
	// DevAuthToken is the development authentication token.
	// If non-empty, development auth middleware is enabled.
	DevAuthToken string
	// AgentTokenConfig holds configuration for agent JWT tokens.
	// If SigningKey is empty, a random key is generated.
	AgentTokenConfig AgentTokenConfig
	// UserTokenConfig holds configuration for user JWT tokens.
	// If SigningKey is empty, a random key is generated.
	UserTokenConfig UserTokenConfig
	// SharedSigningSecret is the deployment-wide secret (the same value every
	// replica receives via --session-secret / SESSION_SECRET) from which the
	// agent and user JWT signing keys are derived deterministically. When set,
	// every replica derives identical signing keys regardless of its
	// host-derived HubID, so a JWT minted by one replica validates on any
	// other replica behind the load balancer. When empty, signing keys fall
	// back to per-hub storage in the secret backend / store.
	SharedSigningSecret string
	// RequireStableSigningKey makes hub startup fail rather than silently
	// generate a brand-new signing key when no existing key can be resolved.
	// Generating a new key invalidates every token previously issued by this
	// hub — agents get crypto verification errors and cannot self-refresh. After
	// a restart that changed the hub identity (e.g. a new pod hostname -> new
	// HubID) without a SharedSigningSecret, that silently orphans every live
	// agent. Enabling this turns that silent outage into a loud fail-fast.
	// Operators enabling it must provide a SharedSigningSecret or pre-provision
	// the signing keys; otherwise first boot will (correctly) refuse to start.
	RequireStableSigningKey bool
	// AuthMode is the exclusive human auth mode: "oauth" (default), "proxy", "dev".
	AuthMode string
	// ProxyAuthenticator is the configured proxy authenticator (when AuthMode == "proxy").
	ProxyAuth ProxyAuthenticator
	// TrustedProxies is a list of trusted proxy IPs/CIDRs for forwarded headers.
	TrustedProxies []string
	// Debug enables verbose debug logging.
	Debug bool
	// OAuthConfig holds OAuth provider credentials for CLI authentication.
	OAuthConfig OAuthConfig
	// AuthorizedDomains is a list of email domains allowed to authenticate.
	// If empty, all domains are allowed.
	AuthorizedDomains []string
	// AdminEmails is a list of email addresses that should be auto-promoted to admin role.
	// Useful for bootstrapping the first admin user.
	AdminEmails []string
	// UserAccessMode controls how user access is evaluated at login time.
	// Values: "open" (default), "domain_restricted", "invite_only".
	UserAccessMode string
	// BrokerAuthConfig holds configuration for Runtime Broker HMAC authentication.
	BrokerAuthConfig BrokerAuthConfig
	// HubEndpoint is the public endpoint URL for this Hub (used in broker join responses).
	HubEndpoint string
	// SlowRequestThreshold is the duration after which an HTTP request is
	// logged as slow. Zero uses logging.DefaultSlowRequestThreshold.
	SlowRequestThreshold time.Duration
	// StalledThreshold is how long an agent can go without activity events
	// before being marked as stalled (default: 5 minutes). Only applies to
	// agents with a recent heartbeat (not already offline).
	StalledThreshold time.Duration
	// AutoSuspendStalled controls whether stalled agents are automatically
	// suspended (container stopped, phase set to "suspended"). Default: false.
	AutoSuspendStalled bool
	// SoftDeleteRetention is how long soft-deleted agents are retained before purging.
	// Zero means soft-delete is disabled (hard-delete immediately).
	SoftDeleteRetention time.Duration
	// SoftDeleteRetainFiles controls whether workspace files are preserved during soft-delete.
	SoftDeleteRetainFiles bool
	// AdminMode restricts access to admin users only (maintenance mode).
	AdminMode bool
	// MaintenanceMessage is the custom message shown during admin mode.
	MaintenanceMessage string
	// TelemetryDefault is the default telemetry enabled state for new agents.
	// Exposed via GET /api/v1/settings/public so the web UI can pre-populate the checkbox.
	TelemetryDefault *bool
	// AutoExposePortsDefault is the default auto-expose-ports enabled state for new agents.
	// Exposed via GET /api/v1/settings/public so the web UI can pre-populate the checkbox.
	AutoExposePortsDefault *bool
	// DefaultScratchpad controls whether new projects automatically get a
	// "scratchpad" shared directory. When nil, the compiled default (true) applies.
	DefaultScratchpad *bool
	// TelemetryConfig is the full hub-level telemetry config from settings.yaml.
	// Used to populate default telemetry config on new agents when no per-agent
	// or template-level telemetry config is set.
	TelemetryConfig *api.TelemetryConfig
	// AgentDefaults holds the hub operational agent_defaults section
	// (Layer-1 settings, koanf keys default_template, default_harness_config,
	// default_max_turns, default_max_model_calls, default_max_duration,
	// default_resources).
	//
	// Written only by ApplySnapshot, under s.mu. Read only through
	// s.hubAgentDefaults(), which also takes s.mu — never read this field
	// directly from a request path (see operational_settings.go, the
	// propagation goroutine writes it concurrently with request handling).
	//
	// In file mode this stays at its zero value: BuildLayer1SnapshotFromFile
	// deliberately leaves the agent-defaults fields empty because a co-located
	// broker reads the same settings.yaml and applies them itself at the
	// BOTTOM of its own chain. Populating them hub-side as well would promote
	// them to the hub tier and silently outrank broker profile resources and
	// template limits. See the design's §3.2.4 and alternative A7.
	AgentDefaults opsettings.AgentDefaultsSettings
	// MaxSubscriptionsPerUser is the maximum number of notification subscriptions
	// allowed per subscriber. Zero means unlimited (default).
	MaxSubscriptionsPerUser int
	// GitHubAppConfig holds the GitHub App configuration for agent git authentication.
	GitHubAppConfig GitHubAppServerConfig
	// HubID is the unique hub instance ID used for secret namespacing.
	// If empty, secrets are looked up/stored with an empty scope ID.
	HubID string
	// HubName is the human-readable hub display name for HA deployments.
	HubName string
	// DisableLegacyStorageFallback disables the legacy un-namespaced storage
	// path fallback. When true, only hub-scoped paths are checked.
	DisableLegacyStorageFallback bool
	// SecretBackend is the optional secret backend for signing key storage.
	// When set before New(), ensureSigningKey can load/persist keys through the
	// production secret backend (e.g., GCP Secret Manager) instead of relying
	// solely on the SQLite store.
	SecretBackend secret.SecretBackend
	// MaintenanceConfig holds configuration for routine maintenance operations.
	MaintenanceConfig MaintenanceConfig
	// GCPIAMCheckMode controls whether IAM actAs permission is checked when
	// binding a GCP service account to an agent.
	// "off" (default) or "enforce". See sa_assign_gate.go for the constants.
	GCPIAMCheckMode string
	// GCPIAMDenyUnknownPolicy controls behavior when the Policy Troubleshooter
	// cannot evaluate deny policies (e.g., Hub SA lacks org-level permissions).
	// "fail-open" (default): if allow is granted and deny is unknown, treat as
	// allowed. "fail-closed": treat as indeterminate (denied).
	GCPIAMDenyUnknownPolicy string
	// GCPProjectID is the GCP project ID used for minting service accounts.
	// If empty, auto-detected from the metadata server when running on GCE/Cloud Run.
	GCPProjectID string
	// TelemetryProjectID is the GCP project where telemetry metrics are stored.
	// Used by the metrics dashboard to query Cloud Monitoring.
	// Falls back to GCPProjectID if empty.
	TelemetryProjectID string
	// GCPMintCapPerProject is the maximum number of minted service accounts allowed per project.
	// Zero means unlimited (default).
	GCPMintCapPerProject int
	// GCPMintCapGlobal is the maximum total number of minted service accounts across all projects.
	// Zero means unlimited (default).
	GCPMintCapGlobal int
	// TransportMode is the transport-layer auth mode: "none" (default), "cloudrun_invoker", "iap".
	// Controls which transport tokens the hub issues to agents.
	TransportMode string
	// TransportAudience is the OIDC audience for transport tokens.
	// For IAP: the IAP OAuth client ID. For cloudrun_invoker: the hub URL.
	TransportAudience string
	// TransportMinter mints transport-layer OIDC tokens for agents.
	// Nil when TransportMode == "none" or unset.
	TransportMinter TransportTokenMinter
	// SchedulerIntervalSeconds is the root ticker interval for the background
	// scheduler, in seconds. Default: 60. Increasing this reduces DB connection
	// pressure on small deployments.
	SchedulerIntervalSeconds int
	// SchedulerMaxConcurrency limits the number of recurring handlers that may
	// run simultaneously in a single tick. When nil (unset), the scheduler
	// uses its built-in default of 2 so the fix for issue #367 is active
	// out-of-the-box. Pointer-to-0 explicitly means unlimited.
	SchedulerMaxConcurrency *int

	// Workstation indicates non-production, single-user mode (e.g. local laptop).
	// When true, /api/v1/system/* and other workstation-only endpoints are enabled.
	Workstation bool
	// DevUserConfig holds optional identity overrides for the development user.
	DevUserConfig DevUserConfig

	// OIDCLogin holds configuration for an external OIDC provider for web login.
	OIDCLogin config.OIDCLoginConfig
	// OIDCConfig holds configuration for the OIDC Identity Provider feature.
	// When Enabled, the hub initializes an OIDCKeyManager and exposes OIDC endpoints.
	OIDCConfig config.OIDCProviderConfig

	// Federation holds configuration for hub-hub federation authentication.
	// When Federation.Enabled is true, the server initializes a FederationAuthenticator
	// and injects it into the auth middleware.
	Federation config.FederationConfig

	// Mode is the server mode (e.g. "workstation", "dev", "hosted").
	// Used by the federation authenticator to enforce HTTPS on issuer URLs
	// in non-dev/non-workstation modes.
	Mode string

	// WorkspaceStorageConfig selects the workspace storage backend for
	// hub-managed project workspaces. When Backend is "nfs",
	// "cloudrun-volume" or "gke-shared-volume", hubManagedProjectPath returns
	// a path on the configured durable mount instead of the node-local home
	// directory.
	// Nil or Backend=="" / "local" preserves the legacy ephemeral behavior.
	WorkspaceStorageConfig *config.V1WorkspaceStorageConfig

	// NativeChatEnabled controls whether the built-in chat feature is active:
	// the /api/v1/chat/* routes are registered and the web UI is told to show
	// the chat interface. Nil means enabled — chat shipped default-on, so an
	// operator must opt out explicitly via server.native_chat.enabled.
	// Read through Server.nativeChatEnabled(), never directly.
	NativeChatEnabled *bool
}

// MaintenanceConfig holds configuration for routine maintenance operation executors.
type MaintenanceConfig struct {
	// ImageRegistry is the container image registry prefix (e.g., "ghcr.io/myorg").
	ImageRegistry string
	// ImageTag is the default image tag to pull (default: "latest").
	ImageTag string
	// Harnesses is the list of harness names whose images should be pulled (e.g., ["claude", "antigravity", "opencode", "codex"]).
	Harnesses []string
	// RuntimeBin overrides auto-detection of the container runtime binary (docker, podman).
	RuntimeBin string
	// RepoPath is the path to the scion source checkout for rebuild operations.
	RepoPath string
	// RepoBranch is the git branch to checkout before building. When empty,
	// the repo stays on whatever branch is currently checked out.
	RepoBranch string
	// BinaryDest is the install path for the rebuilt binary (default: /usr/local/bin/scion).
	BinaryDest string
	// ServiceName is the systemd service name to restart (default: "scion-hub").
	ServiceName string
}

// GitHubAppServerConfig holds the GitHub App configuration for the Hub server.
type GitHubAppServerConfig struct {
	AppID          int64
	PrivateKeyPath string
	PrivateKey     string
	WebhookSecret  string
	APIBaseURL     string
	// RawBaseURL overrides the origin used to build raw file-content URLs
	// (default https://raw.githubusercontent.com). Set alongside APIBaseURL
	// for GitHub Enterprise or for tests that serve fixture content.
	RawBaseURL      string
	WebhooksEnabled bool
	InstallationURL string
}

// DefaultServerConfig returns the default server configuration.
func DefaultServerConfig() ServerConfig {
	return ServerConfig{
		Port:               9810,
		Host:               "0.0.0.0",
		ReadTimeout:        30 * time.Second,
		WriteTimeout:       60 * time.Second,
		CORSEnabled:        true,
		CORSAllowedOrigins: []string{"*"},
		CORSAllowedMethods: []string{"GET", "POST", "PUT", "PATCH", "DELETE", "OPTIONS"},
		CORSAllowedHeaders: []string{
			"Authorization", "Content-Type",
			"X-Scion-Broker-Token", "X-Scion-Agent-Token", "X-API-Key",
			// Broker HMAC authentication headers
			"X-Scion-Broker-ID", "X-Scion-Timestamp", "X-Scion-Nonce",
			"X-Scion-Signature", "X-Scion-Signed-Headers",
		},
		CORSMaxAge:       3600,
		StalledThreshold: 5 * time.Minute,
		BrokerAuthConfig: DefaultBrokerAuthConfig(),
	}
}

// AgentDispatcher is the interface for dispatching agent operations to a runtime broker.
// Implementations may be local (co-located hub+broker) or remote (HTTP-based).
type AgentDispatcher interface {
	// DispatchAgentCreate creates and starts an agent on the runtime broker.
	// Returns the updated agent info after creation/start.
	DispatchAgentCreate(ctx context.Context, agent *store.Agent) error

	// DispatchAgentProvision provisions an agent on the runtime broker without starting it.
	// This sets up directories, worktree, templates, and settings but does not launch the container.
	DispatchAgentProvision(ctx context.Context, agent *store.Agent) error

	// DispatchAgentStart resumes a stopped agent on the runtime broker.
	// task is an optional task string to pass to the agent on start.
	// resume requests harness session continuation (e.g. Claude --continue);
	// callers compute it from the agent's stored phase (suspended → resume).
	DispatchAgentStart(ctx context.Context, agent *store.Agent, task string, resume bool) error

	// DispatchAgentStop stops a running agent on the runtime broker.
	DispatchAgentStop(ctx context.Context, agent *store.Agent) error

	// DispatchAgentRestart restarts an agent on the runtime broker.
	DispatchAgentRestart(ctx context.Context, agent *store.Agent) error

	// DispatchAgentResetAuth injects a fresh token into a running agent without restarting it.
	DispatchAgentResetAuth(ctx context.Context, agent *store.Agent) error

	// DispatchAgentDelete removes an agent from the runtime broker.
	// deleteFiles indicates whether to delete workspace files.
	// removeBranch indicates whether to remove the git branch.
	// softDelete indicates this is a soft-delete (broker should mark agent-info.json).
	// deletedAt is the soft-deletion timestamp (zero for hard delete).
	DispatchAgentDelete(ctx context.Context, agent *store.Agent, deleteFiles, removeBranch, softDelete bool, deletedAt time.Time) error

	// DispatchAgentMessage sends a message to an agent on the runtime broker.
	// The structuredMsg parameter is optional; when nil, the plain message string is used.
	DispatchAgentMessage(ctx context.Context, agent *store.Agent, message string, interrupt bool, structuredMsg *messages.StructuredMessage) error

	// DispatchAgentLogs retrieves agent.log content from the runtime broker.
	DispatchAgentLogs(ctx context.Context, agent *store.Agent, tail int) (string, error)

	// DispatchAgentExec executes a command in an agent on the runtime broker.
	// Returns the command output, exit code, and any error.
	DispatchAgentExec(ctx context.Context, agent *store.Agent, command []string, timeout int) (string, int, error)

	// DispatchCheckAgentPrompt checks if an agent has a non-empty prompt.md file.
	DispatchCheckAgentPrompt(ctx context.Context, agent *store.Agent) (bool, error)

	// DispatchAgentCreateWithGather creates an agent with env-gather support.
	// If the broker returns 202 with env requirements, it returns the requirements
	// instead of an error. The second return value is non-nil when gather is needed.
	DispatchAgentCreateWithGather(ctx context.Context, agent *store.Agent) (*RemoteEnvRequirementsResponse, error)

	// DispatchFinalizeEnv sends gathered env vars to the broker to complete agent creation.
	DispatchFinalizeEnv(ctx context.Context, agent *store.Agent, env map[string]string) error
}

// RuntimeBrokerClient is an interface for communicating with runtime brokers over HTTP.
// This allows the hub to dispatch operations to remote runtime brokers.
// All methods take a brokerID parameter which is used for HMAC authentication when
// the client supports it (AuthenticatedBrokerClient).
type RuntimeBrokerClient interface {
	// CreateAgent creates an agent on a remote runtime broker.
	// brokerID is used for HMAC authentication lookup.
	CreateAgent(ctx context.Context, brokerID, brokerEndpoint string, req *RemoteCreateAgentRequest) (*RemoteAgentResponse, error)

	// StartAgent starts an agent on a remote runtime broker.
	// brokerID is used for HMAC authentication lookup.
	// task is an optional task string to pass to the agent on start.
	// projectPath is the local filesystem path to the project on the broker.
	// projectSlug is the project slug for hub-native projects (no local provider path).
	// resolvedEnv contains environment variables resolved from Hub storage (API keys, etc.).
	// harnessConfig is the harness config name to use for the agent (e.g. "claude", "gemini").
	// resolvedSecrets contains type-aware secrets (including file-type) for auth resolution.
	// sharedWorkspace indicates the project uses a shared workspace mount
	// (hub-project / git-workspace hybrid) so the broker must not create a
	// per-agent worktree on (re-)start.
	StartAgent(ctx context.Context, brokerID, brokerEndpoint, agentID, projectID, task, projectPath, projectSlug, harnessConfig string, resolvedEnv map[string]string, resolvedSecrets []ResolvedSecret, inlineConfig *api.ScionConfig, sharedDirs []api.SharedDir, sharedWorkspace, resume bool) (*RemoteAgentResponse, error)

	// StopAgent stops an agent on a remote runtime broker.
	// brokerID is used for HMAC authentication lookup.
	// projectID scopes the lookup to a specific project (required for uniqueness).
	StopAgent(ctx context.Context, brokerID, brokerEndpoint, agentID, projectID string) error

	// RestartAgent restarts an agent on a remote runtime broker.
	// brokerID is used for HMAC authentication lookup.
	// projectID scopes the lookup to a specific project (required for uniqueness).
	// resolvedEnv carries fresh auth tokens and identity vars so the restarted
	// container retains Hub connectivity.
	RestartAgent(ctx context.Context, brokerID, brokerEndpoint, agentID, projectID string, resolvedEnv map[string]string) error

	// ResetAuthAgent injects a fresh auth token into a running agent without restarting it.
	// brokerID is used for HMAC authentication lookup.
	// projectID scopes the lookup to a specific project (required for uniqueness).
	ResetAuthAgent(ctx context.Context, brokerID, brokerEndpoint, agentID, projectID, token string) error

	// DeleteAgent deletes an agent from a remote runtime broker.
	// brokerID is used for HMAC authentication lookup.
	// projectID scopes the lookup to a specific project (required for uniqueness).
	// softDelete and deletedAt are passed as query params for broker-side marking.
	DeleteAgent(ctx context.Context, brokerID, brokerEndpoint, agentID, projectID string, deleteFiles, removeBranch, softDelete bool, deletedAt time.Time) error

	// MessageAgent sends a message to an agent on a remote runtime broker.
	// brokerID is used for HMAC authentication lookup.
	// structuredMsg is optional; when non-nil it takes precedence over the plain message string.
	MessageAgent(ctx context.Context, brokerID, brokerEndpoint, agentID, projectID, message string, interrupt bool, structuredMsg *messages.StructuredMessage) error

	// CheckAgentPrompt checks if an agent has a non-empty prompt.md file.
	// brokerID is used for HMAC authentication lookup.
	// projectID scopes the lookup to a specific project (required for uniqueness).
	CheckAgentPrompt(ctx context.Context, brokerID, brokerEndpoint, agentID, projectID string) (bool, error)

	// CreateAgentWithGather creates an agent and handles 202 env-gather responses.
	// Returns (response, nil, nil) on success, (nil, envReqs, nil) on 202, or (nil, nil, err) on error.
	CreateAgentWithGather(ctx context.Context, brokerID, brokerEndpoint string, req *RemoteCreateAgentRequest) (*RemoteAgentResponse, *RemoteEnvRequirementsResponse, error)

	// GetAgentLogs retrieves agent.log content from a remote runtime broker.
	// brokerID is used for HMAC authentication lookup.
	// projectID scopes the lookup to a specific project (required for uniqueness).
	GetAgentLogs(ctx context.Context, brokerID, brokerEndpoint, agentID, projectID string, tail int) (string, error)

	// ExecAgent executes a command in an agent on a remote runtime broker.
	// brokerID is used for HMAC authentication lookup.
	// projectID scopes the lookup to a specific project (required for uniqueness).
	// Returns the command output, exit code, and any error.
	ExecAgent(ctx context.Context, brokerID, brokerEndpoint, agentID, projectID string, command []string, timeout int) (string, int, error)

	// CleanupProject asks a broker to remove its local hub-native project directory.
	// brokerID is used for HMAC authentication lookup.
	// projectID is passed to enable NFS subtree cleanup (keyed by project ID).
	// 404 responses are tolerated for idempotency.
	CleanupProject(ctx context.Context, brokerID, brokerEndpoint, projectSlug, projectID string) error
}

// RemoteCreateAgentRequest is the request body for creating an agent on a remote runtime broker.
type RemoteCreateAgentRequest struct {
	RequestID   string             `json:"requestId,omitempty"`
	ID          string             `json:"id,omitempty"` // Hub UUID for status reporting
	Slug        string             `json:"slug"`         // URL-safe identifier for the agent
	Name        string             `json:"name"`
	ProjectID   string             `json:"projectId"`
	UserID      string             `json:"userId,omitempty"`
	Config      *RemoteAgentConfig `json:"config,omitempty"`
	ResolvedEnv map[string]string  `json:"resolvedEnv,omitempty"`
	// ResolvedSecrets contains type-aware secrets resolved by the Hub.
	// These are projected into the agent container based on their type.
	ResolvedSecrets []ResolvedSecret `json:"resolvedSecrets,omitempty"`
	HubEndpoint     string           `json:"hubEndpoint,omitempty"`
	AgentToken      string           `json:"agentToken,omitempty"`
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
	// ProjectPath is the local filesystem path to the project on the target runtime broker.
	// This is looked up from the project provider record for the target broker.
	ProjectPath string `json:"projectPath,omitempty"`
	// WorkspaceStoragePath is the GCS storage path for bootstrapped workspaces.
	// When set, the broker downloads the workspace from GCS instead of using ProjectPath.
	WorkspaceStoragePath string `json:"workspaceStoragePath,omitempty"`

	// GatherEnv indicates the broker should evaluate env completeness before starting.
	// If required keys are missing, the broker returns HTTP 202 with env requirements.
	GatherEnv bool `json:"gatherEnv,omitempty"`

	// RequiredSecrets contains declared secrets from the template config.
	// Passed to the broker so it can include them in env-gather requirements.
	RequiredSecrets []api.RequiredSecret `json:"requiredSecrets,omitempty"`

	// EnvSources tracks which scope provided each env var key (for reporting to CLI).
	// Only populated when GatherEnv is true.
	EnvSources map[string]string `json:"envSources,omitempty"`

	// ProjectSlug is the project slug for hub-native projects.
	// When set, the broker creates the workspace at ~/.scion/projects/<slug>/
	// instead of the default worktree-based path.
	ProjectSlug string `json:"projectSlug,omitempty"`

	// InlineConfig carries the full ScionConfig provided via the Hub API's
	// config field. The broker applies this during agent provisioning,
	// enabling inline configuration without pre-existing templates.
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
}

// ResolvedSecret represents a secret resolved by the Hub for projection into an agent container.
type ResolvedSecret struct {
	Name   string `json:"name"`          // Secret key name
	Type   string `json:"type"`          // environment, variable, file
	Target string `json:"target"`        // Projection target
	Value  string `json:"value"`         // Decrypted secret value
	Source string `json:"source"`        // Scope that provided this secret
	Ref    string `json:"ref,omitempty"` // External secret reference (e.g., "gcpsm:projects/123/secrets/name")
}

// RemoteAgentConfig contains agent configuration for remote creation.
type RemoteAgentConfig struct {
	Template      string   `json:"template,omitempty"`
	Image         string   `json:"image,omitempty"`
	HomeDir       string   `json:"homeDir,omitempty"`
	Workspace     string   `json:"workspace,omitempty"`
	Env           []string `json:"env,omitempty"`
	Task          string   `json:"task,omitempty"`
	CommandArgs   []string `json:"commandArgs,omitempty"`
	HarnessConfig string   `json:"harnessConfig,omitempty"` // Resolved harness config name for env-gather
	HarnessAuth   string   `json:"harnessAuth,omitempty"`   // Late-binding override for auth_selected_type
	Profile       string   `json:"profile,omitempty"`       // Settings profile for the runtime broker
	Branch        string   `json:"branch,omitempty"`        // Git branch name (defaults to agent slug if empty)

	// TemplateID is the Hub template ID for cache lookup on the Runtime Broker.
	// When provided, the Runtime Broker can use this to fetch the template
	// from the Hub and cache it locally.
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
	// When set, the runtime broker skips workspace mounting and injects env vars
	// so sciontool can clone the repo inside the container.
	GitClone *api.GitCloneConfig `json:"gitClone,omitempty"`

	// SharedWorkspace indicates this agent should use a shared git clone
	// workspace (git-workspace hybrid mode). When true, the broker skips
	// worktree/clone creation and configures per-agent git credentials.
	SharedWorkspace bool `json:"sharedWorkspace,omitempty"`

	// GCPIdentity holds the GCP identity assignment for the agent.
	GCPIdentity *RemoteGCPIdentityConfig `json:"gcpIdentity,omitempty"`

	// ProjectPreStartHookScript is the active project pre-start hook script,
	// inlined at agent-create time. The broker writes it to
	// pre-start.d/30-project-custom before the agent starts.
	ProjectPreStartHookScript string `json:"projectPreStartHookScript,omitempty"`

	// HubAgentDefaults carries the hub's operational agent_defaults for
	// application at the broker's LOW-precedence defaults tier — deliberately
	// not folded into InlineConfig, which is a top-of-chain slot.
	HubAgentDefaults *RemoteHubAgentDefaults `json:"hubAgentDefaults,omitempty"`
}

// RemoteHubAgentDefaults carries the four limit/resource operational
// agent_defaults from the hub to a runtime broker.
//
// Only the fields that need no hub-side resolution travel here.
// default_template and default_harness_config are absent by design: the hub
// must resolve those itself so it can stamp TemplateID/TemplateHash and
// HarnessConfigID/HarnessConfigHash, and they therefore ride the existing
// AppliedConfig ladder instead.
//
// Version skew is safe by construction: a broker that predates this field
// ignores the unknown JSON key, so hub defaults simply do not apply — which is
// exactly today's behaviour. No capability negotiation is needed.
//
// Field-for-field JSON-compatible with api.HubAgentDefaults, which is the type
// the broker decodes into; TestRemoteHubAgentDefaults_WireCompatibleWithBroker
// pins that.
type RemoteHubAgentDefaults struct {
	MaxTurns      int               `json:"maxTurns,omitempty"`
	MaxModelCalls int               `json:"maxModelCalls,omitempty"`
	MaxDuration   string            `json:"maxDuration,omitempty"`
	Resources     *api.ResourceSpec `json:"resources,omitempty"`
}

// RemoteGCPIdentityConfig holds GCP identity configuration sent from Hub to Broker.
type RemoteGCPIdentityConfig struct {
	MetadataMode string `json:"metadata_mode"`        // "block", "passthrough", "assign"
	SAEmail      string `json:"sa_email,omitempty"`   // Service account email
	ProjectID    string `json:"project_id,omitempty"` // GCP project ID
}

// RemoteAgentResponse is the response from creating an agent on a remote runtime broker.
type RemoteAgentResponse struct {
	Agent   *RemoteAgentInfo `json:"agent,omitempty"`
	Created bool             `json:"created"`
}

// RemoteEnvRequirementsResponse is returned by the broker when env gather is needed.
// The Hub uses this to relay env requirements back to the CLI.
// SecretKeyInfo provides metadata about a required secret key.
type SecretKeyInfo struct {
	Description string `json:"description,omitempty"`
	Source      string `json:"source"`         // "harness", "template", "settings"
	Type        string `json:"type,omitempty"` // "environment" (default), "variable", "file"
}

type RemoteEnvRequirementsResponse struct {
	AgentID      string                   `json:"agentId"`
	Required     []string                 `json:"required"`
	HubHas       []string                 `json:"hubHas"`
	BrokerHas    []string                 `json:"brokerHas"`
	Needs        []string                 `json:"needs"`
	SecretInfo   map[string]SecretKeyInfo `json:"secretInfo,omitempty"`
	Alternatives map[string][]string      `json:"alternatives,omitempty"` // Maps canonical key in Needs to alternative key names from the same any_of group
}

// RemoteAgentInfo contains agent information from a remote runtime broker.
type RemoteAgentInfo struct {
	ID              string `json:"id"`          // Hub UUID
	Slug            string `json:"slug"`        // URL-safe identifier
	ContainerID     string `json:"containerId"` // Runtime container ID
	Name            string `json:"name"`
	Template        string `json:"template,omitempty"`
	HarnessConfig   string `json:"harnessConfig,omitempty"`
	HarnessAuth     string `json:"harnessAuth,omitempty"`
	Image           string `json:"image,omitempty"` // Resolved container image
	Runtime         string `json:"runtime,omitempty"`
	Profile         string `json:"profile,omitempty"`  // Settings profile used
	Phase           string `json:"phase,omitempty"`    // Lifecycle phase
	Activity        string `json:"activity,omitempty"` // Runtime activity
	Status          string `json:"status"`             // Legacy: kept for backward compat with older brokers
	ContainerStatus string `json:"containerStatus,omitempty"`
}

// Server is the Hub API HTTP server.
type Server struct {
	config                 ServerConfig
	store                  store.Store
	httpServer             *http.Server
	mux                    *http.ServeMux
	mu                     sync.RWMutex
	startTime              time.Time
	dispatcher             AgentDispatcher         // Optional dispatcher for co-located runtime broker
	storage                storage.Storage         // Optional storage backend for templates
	secretBackend          secret.SecretBackend    // Optional secret backend
	agentTokenService      *AgentTokenService      // Agent JWT token service
	userTokenService       *UserTokenService       // User JWT token service
	uatService             *UserAccessTokenService // User access token service
	inviteService          *InviteService          // Invite code service
	oauthService           *OAuthService           // OAuth service for CLI authentication
	authConfig             AuthConfig              // Unified auth configuration
	brokerAuthService      *BrokerAuthService      // Broker HMAC authentication service
	auditLogger            AuditLogger             // Audit logger for security events
	metrics                MetricsRecorder         // Metrics recorder for broker auth
	controlChannel         *ControlChannelManager  // WebSocket control channel for runtime brokers
	portTunnels            *PortTunnelManager      // Agent-held port-forward tunnels
	authzService           *AuthzService           // Authorization service for policy evaluation
	events                 EventPublisher          // Event publisher for real-time SSE updates
	commandBus             CommandBus              // Inter-node dispatch signal bus (nil-safe; nil = no-op)
	notificationDispatcher *NotificationDispatcher // Notification dispatcher for agent status events
	lifecycleHookEvaluator *LifecycleHookEvaluator // Lifecycle hook evaluator for agent phase transitions
	// reconcile op executors (seams): default to executeDispatch/deliverMessage;
	// Phase 3/4 supply the real local-tunnel ops; tests override for exactly-once.
	execDispatch     func(ctx context.Context, d store.BrokerDispatch) (string, error)
	deliverMsg       func(ctx context.Context, m *store.Message) error
	maintenance      *MaintenanceState // Runtime maintenance mode state
	hubID            string            // Unique hub instance ID for secret namespacing
	instanceID       string            // Unique per-process ID (uuid); affinity key for broker dispatch
	embeddedBrokerID string            // Broker ID when running in hub+broker combo mode
	// statelessEmbeddedBroker is true when the embedded broker identity is a
	// replica-independent API adapter rather than a process-owned control channel.
	statelessEmbeddedBroker bool
	runtimeReloadFunc       func() bool // Callback to reload the co-located broker runtime; returns true if swapped
	workstation             bool        // True when running in workstation (non-production) mode
	// webdavLocks stores per-project WebDAV lock systems keyed by project ID.
	// This replaces the per-request webdav.NewMemLS() so that locks survive
	// across HTTP requests within a single instance.
	webdavLocks sync.Map           // map[string]webdav.LockSystem
	scheduler   *Scheduler         // Unified scheduler for recurring tasks
	cleanupOnce sync.Once          // Ensures CleanupResources runs only once
	ctx         context.Context    // Server-lifetime context; cancelled on Shutdown
	ctxCancel   context.CancelFunc // Cancels ctx

	logQueryService  *LogQueryService         // Cloud Logging query service (nil = disabled)
	metricsDashboard *MetricsDashboardService // Cloud Monitoring metrics dashboard (nil = disabled)

	// Telegram link service for code-based account linking (nil = disabled)
	telegramLinkService *TelegramLinkService

	// Discord link service for code-based account linking (nil = disabled)
	discordLinkService *DiscordLinkService

	// Teams link service for code-based account linking (nil = disabled)
	teamsLinkService *TeamsLinkService

	// Plugin manager for broker integration admin API (nil = no integrations)
	pluginManager IntegrationManager

	// Web chat store for webchat_* tables (thread prefs, chat threads, etc.) — nil = disabled.
	webChatStore WebChatStore

	// Chat notifier for human mention + DM received notifications (W6). Nil-safe.
	chatNotifier *ChatNotifier

	// Attachment file store for chat attachments (W7). Nil = attachments disabled.
	// HA limitation: LocalDiskAttachmentStore is single-node only; see attachments.go.
	attachmentStore AttachmentStore

	// Presence manager for in-memory user presence tracking (nil = disabled).
	// Single-node only; see design §4.5 HA limitation.
	presenceManager *PresenceManager

	// Per-sender token-bucket limiter for the chat send paths (#1054).
	// Set once in New and read without the lock; nil-safe.
	chatSendLimiter *chatSendLimiter

	// In-memory idempotency cache for chat message sends (#1055).
	// Keyed by senderID:idempotencyKey with a 5-minute TTL.
	chatIdempotency *ChatIdempotencyCache

	// Channel registry for external notification delivery (nil = disabled)
	channelRegistry *ChannelRegistry

	// Transport token minter for agent outbound auth (nil = transport auth disabled)
	transportMinter   TransportTokenMinter
	transportAudience string
	transportMode     string

	// OIDC identity provider (nil = OIDC IdP disabled)
	oidcKeyManager       *OIDCKeyManager
	oidcIssuerURL        string
	oidcTokenRateLimiter *GCPTokenRateLimiter // per-agent rate limiter for OIDC identity token requests
	oidcTokenLifetime    time.Duration        // validity duration for OIDC identity tokens

	// GCP token generator for agent identity (nil = GCP identity disabled)
	gcpTokenGenerator GCPTokenGenerator

	// GCP IAM admin for minting service accounts (nil = minting disabled)
	gcpIAMAdmin GCPServiceAccountAdmin

	// Caller-permission checker for the agent service-account assignment
	// surface, and the mode gating whether it is consulted. Both are ALWAYS
	// set explicitly in NewServer — nil is a wiring bug and denies, it is not
	// a way to switch the check off. Turning the check off is done by
	// installing store.NewDisabledCallerPermissionChecker, which is a value
	// somebody has to construct and pass. See saAssignCheckerFor.
	saAssignChecker     store.CallerPermissionChecker
	saAssignCheckMode   string
	denyUnknownFailOpen bool

	// The same pair for the lifecycle-hook execution-identity surface. A
	// SEPARATE field rather than a shared one, deliberately: the two surfaces
	// degrade differently when the check is off (agent assign falls back to
	// policy-gated, hook identity falls back to ungated), so an operator must
	// be able to reason about — and eventually enable — them independently.
	// Sharing one field would make that impossible and would hide the
	// difference behind a single innocuous-looking setting.
	hookIdentityChecker   store.CallerPermissionChecker
	hookIdentityCheckMode string

	// GCP token rate limiter (nil = no rate limiting)
	gcpTokenRateLimiter *GCPTokenRateLimiter

	// GCP token metrics tracker (nil = disabled)
	gcpTokenMetrics GCPTokenMetricsRecorder

	// Database connection-pool / notify metrics recorder (P0-5). Defaults to a
	// disabled no-op recorder; SetDBMetrics wires a real exporter. Drives the
	// connection-pool sampler started in StartBackgroundServices.
	dbMetrics dbmetrics.Recorder

	// Broker dispatch metrics recorder (B5-2). Defaults to a disabled no-op
	// recorder; SetDispatchMetrics wires a real exporter.
	dispatchMetrics dispatchmetrics.Recorder

	// stopPoolSampler stops the DB pool-stats sampling goroutine on shutdown.
	stopPoolSampler func()

	// Message broker proxy for pub/sub message routing (nil = disabled)
	messageBrokerProxy *MessageBrokerProxy

	// User last-seen activity tracker (nil = disabled)
	userActivity *UserActivityTracker

	// operationalSettings manages Layer-1 settings from the DB in postgres mode.
	// Nil (zero value) in file/SQLite mode (settings-db §3.7).
	// Uses atomic.Pointer for safe concurrent access — Phase 4/5 will add
	// request-path readers while Set is called during startup.
	operationalSettings atomic.Pointer[OperationalSettings]

	// Dedicated request logger (nil = disabled)
	requestLogger *slog.Logger

	// Dedicated message logger for message audit trail (nil = uses messageLog fallback)
	dedicatedMessageLog *slog.Logger

	// Subsystem loggers for handler methods
	agentLifecycleLog *slog.Logger
	authLog           *slog.Logger
	envSecretLog      *slog.Logger
	groupsLog         *slog.Logger
	maintenanceLog    *slog.Logger
	messageLog        *slog.Logger
	projectsLog       *slog.Logger
	resourceLog       *slog.Logger
	templateLog       *slog.Logger
	workspaceLog      *slog.Logger
	agentMetricsLog   *slog.Logger

	// Cached rate limit info from the most recent GitHub App API call
	githubAppRateLimit *githubapp.RateLimitInfo

	// Shared HTTP client for federation proxy calls (no redirect following).
	federationClient *http.Client

	// federationAuth holds the current FederationAuthenticator.
	// Swapped atomically by ApplySnapshot; read by the auth middleware.
	federationAuth atomic.Pointer[FederationAuthenticator]

	imageBuildActive atomic.Bool
	imagePullActive  atomic.Bool

	imageChecker *imagecheck.Checker
	imageManager imageManager
	brokerClient *HybridBrokerClient

	// Mode 3 (HA) integration support fields.
	// dbDriver records the database backend ("sqlite" or "postgres") for
	// feature-gating Mode 3 endpoints that require Postgres.
	dbDriver string
	// entClient is the Ent ORM client for direct queries on
	// integration_configs and integration_updates tables (nil when HA
	// integration features are not configured).
	entClient *ent.Client
	// databaseDSN is the Postgres connection string, needed to open the
	// admin signal listener connection and for PublishAdminSignal calls
	// outside a transaction (nil/empty when database is not Postgres).
	databaseDSN string
	// updateTracker manages pending HA update timeouts and reconnect-based
	// completion detection.
	updateTracker *pendingUpdateTracker

	// ghResolutionStore is the DB-backed GitHub skill resolution cache (nil when entClient is nil).
	ghResolutionStore *GitHubResolutionStore

	// nonceCacheStore is the DB-backed HMAC nonce replay cache (nil when entClient is nil).
	// When set, it replaces the in-memory NonceCache in BrokerAuthService for
	// cross-instance replay protection.
	nonceCacheStore *NonceCacheStore

	// chatLinkStore is the DB-backed chat link code store (nil when entClient is nil).
	// When non-nil, Telegram/Discord/Teams link services delegate to it.
	chatLinkStore *ChatLinkStore

	// warnedEphemeralProjects holds the project slugs already reported as being
	// served from ephemeral local storage, keeping that warning to one line per
	// slug on a request path. See warnEphemeralProjectPath.
	warnedEphemeralProjects sync.Map
}

// groupsLogger returns the groups subsystem logger, falling back to
// slog.Default() when the field is nil (e.g. in tests that construct Server
// directly without the constructor).
func (s *Server) groupsLogger() *slog.Logger {
	if s.groupsLog != nil {
		return s.groupsLog
	}
	return slog.Default()
}

// projectsLogger returns the projects subsystem logger, falling back to
// slog.Default() when the field is nil.
func (s *Server) projectsLogger() *slog.Logger {
	if s.projectsLog != nil {
		return s.projectsLog
	}
	return slog.Default()
}

func newInstanceID() string {
	if podName := os.Getenv("POD_NAME"); podName != "" {
		return podName + "-" + uuid.NewString()
	}
	return uuid.NewString()
}

// InstanceID returns the per-process unique identifier for this hub instance.
func (s *Server) InstanceID() string { return s.instanceID }

// New creates a new Hub API server.
func New(cfg ServerConfig, s store.Store) (*Server, error) {
	// Apply defaults for zero-value fields that have meaningful defaults.
	defaults := DefaultServerConfig()
	if cfg.StalledThreshold == 0 || cfg.StalledThreshold < 2*time.Minute {
		if cfg.StalledThreshold != 0 {
			slog.Warn("stalled_threshold below minimum 2m, using default",
				"configured", cfg.StalledThreshold, "default", defaults.StalledThreshold)
		}
		cfg.StalledThreshold = defaults.StalledThreshold
	}

	srvCtx, srvCancel := context.WithCancel(context.Background())

	srv := &Server{
		config:      cfg,
		store:       s,
		mux:         http.NewServeMux(),
		startTime:   time.Now(),
		events:      noopEventPublisher{},
		maintenance: NewMaintenanceState(cfg.AdminMode, cfg.MaintenanceMessage),
		hubID:       cfg.HubID,
		instanceID:  newInstanceID(),
		workstation: cfg.Workstation,
		ctx:         srvCtx,
		ctxCancel:   srvCancel,
		portTunnels: NewPortTunnelManager(),

		// Subsystem loggers
		agentLifecycleLog: logging.Subsystem("hub.agent-lifecycle"),
		authLog:           logging.Subsystem("hub.auth"),
		envSecretLog:      logging.Subsystem("hub.env-secrets"),
		groupsLog:         logging.Subsystem("hub.groups"),
		maintenanceLog:    logging.Subsystem("hub.maintenance"),
		messageLog:        logging.Subsystem("hub.messages"),
		projectsLog:       logging.Subsystem("hub.projects"),
		resourceLog:       logging.Subsystem("hub.resources"),
		templateLog:       logging.Subsystem("hub.templates"),
		workspaceLog:      logging.Subsystem("hub.workspace"),
		agentMetricsLog:   logging.Subsystem("hub.agent-metrics"),
	}

	// Wire tunnel disconnect handler: when an agent's port-forward tunnel
	// closes (readLoop exits), clear its exposed port registrations so stale
	// ports are not advertised.
	srv.portTunnels.onDisconnect = func(agentID string) {
		srv.clearExposedPortsForAgent(context.Background(), agentID)
	}

	// Shared federation HTTP client: no redirect following to prevent
	// credential leakage via Authorization header on cross-origin redirects.
	srv.federationClient = &http.Client{
		Timeout: federationTimeout,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}

	// Set secret backend from config so ensureSigningKey can use it.
	// This must happen before signing key initialization below.
	if cfg.SecretBackend != nil {
		srv.secretBackend = cfg.SecretBackend
	}

	// Initialize update tracker for HA integration completion detection.
	srv.updateTracker = newPendingUpdateTracker()

	// Initialize user activity tracker (throttled to once per hour per user)
	srv.userActivity = NewUserActivityTracker(s, time.Hour)

	// Initialize GCP token metrics
	srv.gcpTokenMetrics = NewGCPTokenMetrics()

	// Per-sender chat send rate limiter (#1054).
	srv.chatSendLimiter = newChatSendLimiter()
	srv.chatIdempotency = NewChatIdempotencyCache()

	ctx := context.Background()

	_, isGCPBackend := srv.secretBackend.(*secret.GCPBackend)

	// Initialize agent token service
	agentKey, err := srv.ensureSigningKey(ctx, SecretKeyAgentSigningKey, cfg.AgentTokenConfig.SigningKey)
	if err != nil {
		// Fail-fast for a GCP backend (production) or when stable keys are
		// required. Otherwise a non-fatal error would fall through to
		// NewAgentTokenService generating an ephemeral random key, reintroducing
		// the silent token-invalidation this guard exists to prevent.
		if isGCPBackend || cfg.RequireStableSigningKey {
			return nil, fmt.Errorf("agent signing key: %w", err)
		}
		logSigningKeyFailure("agent", err)
	} else {
		cfg.AgentTokenConfig.SigningKey = agentKey
	}
	tokenService, err := NewAgentTokenService(cfg.AgentTokenConfig)
	if err != nil {
		slog.Warn("Failed to initialize agent token service", "error", err)
	} else {
		srv.agentTokenService = tokenService
		fp := sha256.Sum256(tokenService.config.SigningKey)
		slog.Info("Agent token service initialized", "key_fingerprint", hex.EncodeToString(fp[:8]))
	}

	// Initialize user token service
	userKey, err := srv.ensureSigningKey(ctx, SecretKeyUserSigningKey, cfg.UserTokenConfig.SigningKey)
	if err != nil {
		if isGCPBackend || cfg.RequireStableSigningKey {
			return nil, fmt.Errorf("user signing key: %w", err)
		}
		logSigningKeyFailure("user", err)
	} else {
		cfg.UserTokenConfig.SigningKey = userKey
	}
	userTokenService, err := NewUserTokenService(cfg.UserTokenConfig)
	if err != nil {
		slog.Warn("Failed to initialize user token service", "error", err)
	} else {
		srv.userTokenService = userTokenService
		fp := sha256.Sum256(userTokenService.config.SigningKey)
		slog.Info("User token service initialized", "key_fingerprint", hex.EncodeToString(fp[:8]))
	}

	// Initialize user access token service
	srv.uatService = NewUserAccessTokenService(s, s, s)

	// Initialize invite code service
	srv.inviteService = NewInviteService(s)

	// Initialize Telegram link service
	srv.telegramLinkService = NewTelegramLinkService()

	// Initialize Discord link service
	srv.discordLinkService = NewDiscordLinkService()

	// Initialize Teams link service
	srv.teamsLinkService = NewTeamsLinkService()

	// Validate OIDC login configuration at startup (fail fast).
	if cfg.OIDCLogin.Enabled {
		if err := validateOIDCLoginConfig(&cfg.OIDCLogin); err != nil {
			return nil, fmt.Errorf("invalid OIDC login configuration: %w", err)
		}
	}

	// Initialize OAuth service if configured (traditional OAuth or OIDC login)
	oidcLoginCfg := &cfg.OIDCLogin // may be zero-value (Enabled=false)
	if cfg.OAuthConfig.IsConfigured() || cfg.OIDCLogin.Enabled {
		srv.oauthService = NewOAuthService(cfg.OAuthConfig, oidcLoginCfg)
		slog.Info("OAuth service initialized")
		// Log which providers are configured
		logOAuthProviders("Web", cfg.OAuthConfig.Web)
		logOAuthProviders("CLI", cfg.OAuthConfig.CLI)
		logOAuthProviders("Device", cfg.OAuthConfig.Device)
		if cfg.OIDCLogin.Enabled {
			slog.Info("OIDC login provider configured",
				"displayName", cfg.OIDCLogin.DisplayName,
				"issuerUrl", cfg.OIDCLogin.IssuerURL)
		}
	} else {
		slog.Info("OAuth service NOT configured - no providers available")
		slog.Info("To enable OAuth, set environment variables SCION_SERVER_OAUTH_CLI_GOOGLE_CLIENTID, etc.")
	}

	// Log authorized domains if configured
	if len(cfg.AuthorizedDomains) > 0 {
		slog.Info("Authorized domains", "domains", strings.Join(cfg.AuthorizedDomains, ", "))
	}

	// Initialize audit logger (used by broker auth and invite system)
	// Default reconcile-drain op executors (Phase 3/4 supply the real local ops).
	srv.execDispatch = srv.executeDispatch
	srv.deliverMsg = srv.deliverMessage

	srv.auditLogger = NewLogAuditLogger("[Hub Audit]", cfg.Debug)

	// Initialize broker auth service if enabled
	if cfg.BrokerAuthConfig.Enabled {
		srv.brokerAuthService = NewBrokerAuthService(cfg.BrokerAuthConfig, s)
		srv.metrics = NewBrokerAuthMetrics()
		slog.Info("Broker HMAC authentication enabled")
	}

	// Store transport token minter if configured
	if cfg.TransportMinter != nil {
		srv.transportMinter = cfg.TransportMinter
		srv.transportAudience = cfg.TransportAudience
		srv.transportMode = cfg.TransportMode
		slog.Info("Transport token minter configured",
			"mode", cfg.TransportMode,
			"audience", cfg.TransportAudience)
	}

	// Initialize OIDC Identity Provider key manager if enabled
	if cfg.OIDCConfig.Enabled {
		oidcIssuerURL := cfg.OIDCConfig.IssuerURL
		if oidcIssuerURL == "" {
			oidcIssuerURL = cfg.HubEndpoint
		}
		if oidcIssuerURL == "" {
			return nil, fmt.Errorf("OIDC is enabled but no issuer URL configured (set oidc.issuer_url or hub.endpoint)")
		}
		oidcIssuerURL = strings.TrimRight(oidcIssuerURL, "/")

		oidcMgr, err := NewOIDCKeyManager(ctx, OIDCKeyManagerConfig{
			Store:                   s,
			Backend:                 srv.secretBackend,
			HubID:                   srv.hubID,
			IssuerURL:               oidcIssuerURL,
			RequireStableSigningKey: cfg.RequireStableSigningKey,
			Log:                     logging.Subsystem("hub.oidc"),
		})
		if err != nil {
			if isGCPBackend || cfg.RequireStableSigningKey {
				return nil, fmt.Errorf("OIDC key manager: %w", err)
			}
			slog.Warn("Failed to initialize OIDC key manager", "error", err)
		} else {
			srv.oidcKeyManager = oidcMgr
			srv.oidcIssuerURL = oidcIssuerURL

			// Start background loops for key cleanup and cross-instance refresh.
			oidcMgr.StartCleanupLoop(ctx)
			oidcMgr.StartRefreshLoop(ctx)

			// OIDC identity token lifetime: use config if set, else default 15m
			srv.oidcTokenLifetime = 15 * time.Minute
			if cfg.OIDCConfig.TokenLifetime > 0 {
				srv.oidcTokenLifetime = cfg.OIDCConfig.TokenLifetime
			}

			// Per-agent rate limiter for OIDC identity token requests (0.5 req/sec avg, burst 30)
			srv.oidcTokenRateLimiter = NewGCPTokenRateLimiter(0.5, 30)

			slog.Info("OIDC Identity Provider enabled", "issuer_url", oidcIssuerURL)
		}
	}

	// Initialize control channel manager
	srv.controlChannel = NewControlChannelManager(ControlChannelConfig{
		PingInterval:   30 * time.Second,
		PongWait:       60 * time.Second,
		WriteWait:      10 * time.Second,
		MaxMessageSize: 1024 * 1024, // 1MB — see issue #165
		RequestTimeout: 120 * time.Second,
		Debug:          cfg.Debug,
	}, logging.Subsystem("hub.control-channel"))
	// Set disconnect callback to mark broker offline when WebSocket drops.
	// ReleaseAndMarkBrokerOffline atomically clears affinity AND stamps
	// status=offline in a single CAS write — if a concurrent reconnect has
	// already claimed the broker with a new session, the compare fails and the
	// callback is a no-op. This eliminates the TOCTOU race where a separate
	// ReleaseRuntimeBrokerConnection + UpdateRuntimeBrokerHeartbeat allowed
	// the offline stamp to clobber a concurrent markBrokerOnline (issue #131).
	srv.controlChannel.SetOnDisconnect(func(brokerID, sessionID string) {
		ctx := context.Background()

		cleared, err := s.ReleaseAndMarkBrokerOffline(ctx, brokerID, srv.instanceID, sessionID)
		if err != nil {
			slog.Error("Failed to release broker affinity on disconnect", "brokerID", brokerID, "sessionID", sessionID, "error", err)
			return
		}
		if !cleared {
			slog.Info("broker reconnected elsewhere; skipping offline stamp", "brokerID", brokerID, "staleSession", sessionID)
			return
		}

		slog.Info("Broker disconnected, marking offline", "brokerID", brokerID, "sessionID", sessionID)

		// Guard: re-read the broker before updating provider statuses. A
		// concurrent markBrokerOnline may have already re-claimed the broker
		// between our atomic release+offline and now. If so, skip provider
		// updates to avoid clobbering the new session's online providers.
		broker, rerr := s.GetRuntimeBroker(ctx, brokerID)
		if rerr == nil && broker.ConnectedSessionID != nil && *broker.ConnectedSessionID != "" {
			slog.Info("broker re-claimed by new session after release; skipping provider offline stamp",
				"brokerID", brokerID, "staleSession", sessionID, "newSession", *broker.ConnectedSessionID)
			return
		}

		// Update all project provider records for this broker
		providers, err := s.GetBrokerProjects(ctx, brokerID)
		if err != nil {
			slog.Error("Failed to get broker projects for status update", "brokerID", brokerID, "error", err)
		} else {
			for _, provider := range providers {
				if err := s.UpdateProviderStatus(ctx, provider.ProjectID, brokerID, store.BrokerStatusOffline); err != nil {
					slog.Error("Failed to update provider status", "brokerID", brokerID, "project_id", provider.ProjectID, "error", err)
				}
			}

			// Publish broker disconnected event
			projectIDs := make([]string, len(providers))
			for i, p := range providers {
				projectIDs[i] = p.ProjectID
			}
			srv.events.PublishBrokerDisconnected(ctx, brokerID, projectIDs)
		}
	})
	slog.Info("Control channel manager initialized")

	// Initialize authorization service
	srv.authzService = NewAuthzService(s, logging.Subsystem("hub.auth"))

	// Wire the caller-permission checker for agent service-account assignment.
	//
	// GCP IAM check mode: read from config, default to "off" (Q1 ruling).
	// When "enforce", the PT checker (wired later in server_foreground.go)
	// gates SA assignment. The disabled checker installed here is the default
	// until a real checker replaces it via SetSAAssignChecker.
	//
	// Installed explicitly rather than left nil on purpose: a nil checker
	// denies, so "forgot to wire it" and "chose to switch it off" cannot be
	// confused for one another. See NewDisabledCallerPermissionChecker.
	gcpIAMMode := cfg.GCPIAMCheckMode
	switch gcpIAMMode {
	case SAAssignCheckEnforce:
		srv.saAssignCheckMode = SAAssignCheckEnforce
		srv.hookIdentityCheckMode = SAAssignCheckEnforce
	case SAAssignCheckOff, "":
		srv.saAssignCheckMode = SAAssignCheckOff
		srv.hookIdentityCheckMode = SAAssignCheckOff
	default:
		slog.Warn("unrecognised gcpIamCheckMode value, defaulting to off",
			"value", gcpIAMMode)
		srv.saAssignCheckMode = SAAssignCheckOff
		srv.hookIdentityCheckMode = SAAssignCheckOff
	}

	// Parse deny-unknown fallback policy (default: fail-open).
	srv.denyUnknownFailOpen = true
	switch cfg.GCPIAMDenyUnknownPolicy {
	case "fail-closed":
		srv.denyUnknownFailOpen = false
	case "fail-open", "":
		srv.denyUnknownFailOpen = true
	default:
		slog.Warn("unrecognised gcpIamDenyUnknownPolicy value, defaulting to fail-open",
			"value", cfg.GCPIAMDenyUnknownPolicy)
	}
	slog.Info("GCP deny-unknown fallback policy",
		"policy", cfg.GCPIAMDenyUnknownPolicy,
		"failOpen", srv.denyUnknownFailOpen)

	srv.saAssignChecker = store.NewDisabledCallerPermissionChecker()
	if srv.saAssignCheckMode == SAAssignCheckOff {
		// Names the SURFACE and what it degrades to, not the feature. The same
		// disabled checker means "policy-gated only" here and "ungated"
		// elsewhere; a message about "the IAM check" would mislead about the
		// other one.
		slog.Warn("GCP caller-permission checking is OFF for agent service-account assignment: "+
			"assignment is gated by Hub policy only, and no caller is checked for "+
			store.PermissionActAs+" on the target account",
			"surface", SurfaceAgentAssign, "mode", srv.saAssignCheckMode)
	} else {
		slog.Info("GCP caller-permission checking is ENFORCE for agent service-account assignment",
			"surface", SurfaceAgentAssign, "mode", srv.saAssignCheckMode)
	}

	// Same wiring for the lifecycle-hook execution-identity surface, installed
	// separately because it degrades to something strictly worse. See the
	// field comment and SurfaceHookExecutionIdentity.
	srv.hookIdentityChecker = store.NewDisabledCallerPermissionChecker()
	if srv.hookIdentityCheckMode == SAAssignCheckOff {
		slog.Warn("GCP caller-permission checking is OFF for lifecycle-hook execution identity: "+
			"any caller who may write a hook may run it as any in-scope verified service account, "+
			"and no caller is checked for "+store.PermissionActAs+" on it. Unlike agent "+
			"service-account assignment, this surface has NO second policy layer to fall back on",
			"surface", lifecyclehooks.SurfaceHookExecutionIdentity,
			"mode", srv.hookIdentityCheckMode)
	} else {
		slog.Info("GCP caller-permission checking is ENFORCE for lifecycle-hook execution identity",
			"surface", lifecyclehooks.SurfaceHookExecutionIdentity,
			"mode", srv.hookIdentityCheckMode)
	}

	// Seed default policies and groups (idempotent)
	seedDefaultPoliciesAndGroups(ctx, s)

	// Backfill the per-project service-account assign policy onto projects
	// that predate it (idempotent). Projects nobody touches never run
	// createProjectMembersGroupAndPolicy again, so without this their members
	// would silently lose assign when the gate moves to ActionAssign.
	backfillProjectAssignPolicies(ctx, s)

	// Seed the dev user when dev-auth is enabled so that Ent FK constraints
	// on owner_id are satisfied when the dev user creates projects/groups.
	if cfg.DevAuthToken != "" {
		seedDevUser(ctx, s, cfg.DevUserConfig)
	}

	// Seed platform skills into hub_settings["injected_skills"].system (idempotent).
	// Runs on every startup so that the system list is always in sync with the binary.
	if err := srv.seedPlatformSkillInsertions(ctx); err != nil {
		slog.Warn("Failed to seed platform skill insertions", "error", err)
	}

	// Seed GitHub resolution cache settings into hub_settings["github_resolution_cache"] (idempotent).
	if err := srv.seedGitHubResolutionCacheSettings(ctx); err != nil {
		slog.Warn("Failed to seed github_resolution_cache settings", "error", err)
	}

	// Abort any maintenance operations/migrations left in "running" state from
	// a previous server instance that was restarted mid-operation.
	if runs, migrations, err := s.AbortRunningMaintenanceOps(ctx); err != nil {
		slog.Warn("Failed to abort stalled maintenance operations", "error", err)
	} else if runs > 0 || migrations > 0 {
		slog.Info("Aborted stalled maintenance operations from previous run",
			"runs", runs, "migrations", migrations)
	}

	// Initialize federation authenticator if enabled.
	if cfg.Federation.Enabled {
		// Derive mode for HTTPS enforcement.
		federationMode := cfg.Mode
		if federationMode == "" {
			if cfg.Workstation {
				federationMode = "workstation"
			} else {
				federationMode = "hosted"
			}
		}
		// Use the OIDC issuer URL as the default expected audience.
		federationAudience := srv.oidcIssuerURL
		if federationAudience == "" {
			federationAudience = cfg.OIDCConfig.IssuerURL
		}

		fa, err := NewFederationAuthenticator(
			cfg.Federation,
			federationAudience,
			srv.federationClient,
			federationMode,
			logging.Subsystem("hub.federation"),
		)
		if err != nil {
			return nil, fmt.Errorf("federation authenticator init: %w", err)
		}
		srv.federationAuth.Store(fa)
		slog.Info("Federation authenticator enabled",
			"trusted_issuers", len(cfg.Federation.TrustedIssuers))
	}

	// Build unified auth configuration
	srv.authConfig = AuthConfig{
		Mode:               "production",
		DevAuthEnabled:     cfg.DevAuthToken != "",
		DevAuthToken:       cfg.DevAuthToken,
		DevUserCfg:         cfg.DevUserConfig,
		AgentTokenSvc:      srv.agentTokenService,
		UserTokenSvc:       srv.userTokenService,
		UATSvc:             srv.uatService,
		BrokerAuthSvc:      srv.brokerAuthService,
		TrustedProxies:     cfg.TrustedProxies,
		ProxyAuthenticator: cfg.ProxyAuth,
		FederationAuth:     &srv.federationAuth,
		AuthMode:           cfg.AuthMode,
		Debug:              cfg.Debug,
		Logger:             srv.authLog,
	}
	// Wire the proxy user provisioner (wraps provisionUser with 60s cache)
	if cfg.ProxyAuth != nil {
		srv.authConfig.ProxyUserProvisioner = MakeProxyUserProvisioner(srv)
	}

	// Initialize Cloud Logging query service (optional, gated on GCP project ID)
	if projectID := logging.ResolveProjectID(); projectID != "" {
		logQuerySvc, err := NewLogQueryService(ctx, projectID)
		if err != nil {
			slog.Warn("Failed to initialize Cloud Logging query service", "error", err)
		} else {
			srv.logQueryService = logQuerySvc
			slog.Info("Cloud Logging query service initialized", "project", projectID)
		}
	}

	// Initialize metrics dashboard service (optional, gated on telemetry project ID)
	if telemetryProject := cfg.TelemetryProjectID; telemetryProject != "" {
		metricsSvc, err := NewMetricsDashboardService(ctx, telemetryProject)
		if err != nil {
			slog.Warn("Failed to initialize metrics dashboard service", "error", err)
		} else {
			srv.metricsDashboard = metricsSvc
			slog.Info("Metrics dashboard service initialized", "project", telemetryProject)
		}
	} else if projectID := cfg.GCPProjectID; projectID != "" {
		metricsSvc, err := NewMetricsDashboardService(ctx, projectID)
		if err != nil {
			slog.Warn("Failed to initialize metrics dashboard service", "error", err)
		} else {
			srv.metricsDashboard = metricsSvc
			slog.Info("Metrics dashboard service initialized (from GCPProjectID)", "project", projectID)
		}
	}

	// Initialize GCP token rate limiter (1 req/sec average, burst of 10)
	srv.gcpTokenRateLimiter = NewGCPTokenRateLimiter(1, 10)

	// Initialize image checker for harness config image status verification
	srv.imageChecker = imagecheck.NewChecker()

	srv.registerRoutes()

	return srv, nil
}

// deriveSharedSigningKey deterministically derives a 32-byte HS256 signing key
// from the deployment's shared signing secret and the logical key name. The key
// name (e.g. "user_signing_key", "agent_signing_key") provides domain
// separation so the user and agent keys differ even though both originate from
// the same shared secret. Every replica configured with the same shared secret
// derives identical keys, which is what lets a JWT minted by one replica be
// validated by another.
func deriveSharedSigningKey(secret, keyName string) []byte {
	sum := sha256.Sum256([]byte("scion-hub-signing-key:" + keyName + ":" + secret))
	return sum[:]
}

// ensureSigningKey ensures a signing key exists, loading it if it does
// or generating and saving it if it doesn't.
//
// When a secret backend (e.g., GCP Secret Manager) is configured, signing keys
// are stored and retrieved through it. Otherwise, signing keys fall back to
// direct database storage. This is acceptable for hub-internal infrastructure
// keys, unlike user-managed secrets which always require a production backend.
func (s *Server) ensureSigningKey(ctx context.Context, keyName string, existingKey []byte) ([]byte, error) {
	if len(existingKey) > 0 {
		fp := sha256.Sum256(existingKey)
		slog.Info("ensureSigningKey: using pre-configured key",
			"key", keyName,
			"source", "config",
			"key_len", len(existingKey),
			"sha256_prefix", hex.EncodeToString(fp[:8]),
		)
		return existingKey, nil
	}

	// When a deployment-wide shared signing secret is configured (the same
	// secret every replica receives via --session-secret / SESSION_SECRET),
	// derive the signing key deterministically from it. This makes the key
	// identical on every replica regardless of the host-derived hub ID, so a
	// JWT minted by one replica validates on any other. It mirrors the web
	// session cookie store (commit 0515e2a8), whose keys are derived from the
	// same shared secret, and is what lets the hub scale horizontally behind a
	// load balancer without operators having to pin a matching HubID on each
	// replica. Per-host secret-backend storage (below) is bypassed entirely.
	if s.config.SharedSigningSecret != "" {
		key := deriveSharedSigningKey(s.config.SharedSigningSecret, keyName)
		fp := sha256.Sum256(key)
		slog.Info("ensureSigningKey: derived from shared signing secret",
			"key", keyName,
			"source", "shared_secret",
			"key_len", len(key),
			"sha256_prefix", hex.EncodeToString(fp[:8]),
		)
		// Sync the derived key to the secret backend so that external consumers
		// (e.g. scion-chat-app) that discover signing keys via label-based
		// auto-discovery in GCP Secret Manager can still find them.
		encodedKey := base64.StdEncoding.EncodeToString(key)
		_, isGCPBackend := s.secretBackend.(*secret.GCPBackend)
		if err := s.syncSigningKeyToBackend(ctx, keyName, encodedKey, s.hubID, isGCPBackend); err != nil {
			slog.Warn("Failed to sync shared-secret-derived key to secret backend",
				"key", keyName, "error", err)
		}
		return key, nil
	}

	hubID := s.hubID
	hasSecretBackend := s.secretBackend != nil
	_, isGCPBackend := s.secretBackend.(*secret.GCPBackend)

	slog.Info("ensureSigningKey: resolving key",
		"key", keyName,
		"hub_id", hubID,
		"has_secret_backend", hasSecretBackend,
		"is_gcp_backend", isGCPBackend,
	)

	// Try to load from the secret backend if configured
	if hasSecretBackend {
		sv, err := s.secretBackend.Get(ctx, keyName, store.ScopeHub, hubID)
		if err == nil {
			slog.Info("Loaded existing signing key from secret backend", "key", keyName)
			key, decErr := base64.StdEncoding.DecodeString(sv.Value)
			if decErr != nil {
				return nil, fmt.Errorf("failed to decode signing key %s from secret backend: %w", keyName, decErr)
			}
			fp := sha256.Sum256(key)
			slog.Info("ensureSigningKey: resolved from secret backend",
				"key", keyName,
				"source", "secret_backend",
				"scope", store.ScopeHub,
				"scope_id", hubID,
				"key_len", len(key),
				"sha256_prefix", hex.EncodeToString(fp[:8]),
				"secret_ref", sv.SecretRef,
			)
			// Backfill the SQLite record as a local backup so the key survives
			// even if the secret backend becomes unavailable. This also covers
			// the case where GCPBackend.Get recovered the key directly from
			// GCP SM without a pre-existing SQLite metadata record.
			if persistErr := s.backupSigningKeyToStore(ctx, keyName, sv.Value, hubID); persistErr != nil {
				slog.Warn("Failed to persist signing key backup to store after loading from backend", "key", keyName, "error", persistErr)
			}
			return key, nil
		}
		if err != store.ErrNotFound {
			slog.Warn("Failed to load signing key from secret backend, trying store", "key", keyName, "error", err)
		}
	}

	// Fallback: try loading from the store directly (for migration/local dev)
	val, err := s.store.GetSecretValue(ctx, keyName, store.ScopeHub, hubID)
	if err == nil {
		if val == "" {
			// The GCP secret backend stores EncryptedValue="" in SQLite (using a
			// SecretRef instead). If GCP SM later loses the secret, this fallback
			// finds the empty row. Treat it as not-found so we continue to legacy
			// migration or generate a new key rather than silently returning nil.
			slog.Warn("Store contains empty signing key value (secret backend reference row); treating as not found", "key", keyName)
		} else {
			slog.Info("Loaded existing signing key from store", "key", keyName)
			key, decErr := base64.StdEncoding.DecodeString(val)
			if decErr != nil {
				return nil, fmt.Errorf("failed to decode signing key %s from store: %w", keyName, decErr)
			}
			if len(key) == 0 {
				return nil, fmt.Errorf("signing key %s decoded to empty value", keyName)
			}
			fp := sha256.Sum256(key)
			slog.Info("ensureSigningKey: resolved from store",
				"key", keyName,
				"source", "store",
				"scope", store.ScopeHub,
				"scope_id", hubID,
				"key_len", len(key),
				"sha256_prefix", hex.EncodeToString(fp[:8]),
			)
			// Sync to secret backend so future restarts load from the authoritative source.
			if err := s.syncSigningKeyToBackend(ctx, keyName, val, hubID, isGCPBackend); err != nil {
				return nil, err
			}
			return key, nil
		}
	} else if err != store.ErrNotFound {
		return nil, fmt.Errorf("failed to load signing key %s from store: %w", keyName, err)
	}

	// Migration fallback: try legacy scope IDs used before hub-instance-ID namespacing.
	// Keys may exist under ScopeID="hub" (pre-refactor) or ScopeID="" (window between
	// the refactor and the fix that passes HubID into ServerConfig).
	if hubID != "" {
		for _, legacyScopeID := range []string{"hub", ""} {
			if legacyScopeID == hubID {
				continue
			}
			val, legacyErr := s.store.GetSecretValue(ctx, keyName, store.ScopeHub, legacyScopeID)
			if legacyErr != nil {
				continue
			}
			slog.Info("Loaded signing key from legacy scope ID, will migrate", "key", keyName, "legacyScopeID", legacyScopeID)
			key, decErr := base64.StdEncoding.DecodeString(val)
			if decErr != nil {
				return nil, fmt.Errorf("failed to decode legacy signing key %s: %w", keyName, decErr)
			}
			fp := sha256.Sum256(key)
			slog.Info("ensureSigningKey: resolved from legacy migration",
				"key", keyName,
				"source", "legacy_store",
				"legacy_scope_id", legacyScopeID,
				"target_scope_id", hubID,
				"key_len", len(key),
				"sha256_prefix", hex.EncodeToString(fp[:8]),
			)
			// Delete the old secret from the secret backend (e.g. GCP SM) first
			// so stale secrets don't confuse label-based auto-discovery by
			// external consumers like scion-chat-app.
			if hasSecretBackend {
				if delErr := s.secretBackend.Delete(ctx, keyName, store.ScopeHub, legacyScopeID); delErr != nil {
					slog.Warn("Failed to delete legacy signing key from secret backend", "key", keyName, "legacyScopeID", legacyScopeID, "error", delErr)
				} else {
					slog.Info("Deleted legacy signing key from secret backend", "key", keyName, "legacyScopeID", legacyScopeID)
				}
			}
			// Delete the old DB record — it may share the same primary key ID
			// so an INSERT with the new scope_id would collide on the PK.
			if delErr := s.store.DeleteSecret(ctx, keyName, store.ScopeHub, legacyScopeID); delErr != nil {
				slog.Warn("Failed to delete legacy signing key record", "key", keyName, "legacyScopeID", legacyScopeID, "error", delErr)
			}
			// Sync to secret backend and persist to store under current hub ID.
			if err := s.syncSigningKeyToBackend(ctx, keyName, val, hubID, isGCPBackend); err != nil {
				return nil, err
			}
			// Always persist the local backup — syncSigningKeyToBackend is a
			// no-op when there is no secret backend, so the key would be lost
			// on restart without this explicit save.
			if persistErr := s.backupSigningKeyToStore(ctx, keyName, val, hubID); persistErr != nil {
				slog.Warn("Failed to persist migrated signing key backup to store", "key", keyName, "error", persistErr)
			}
			return key, nil
		}
	}

	// Not found anywhere — we must generate a new key. Generating a new signing
	// key invalidates EVERY token previously issued by this hub: live agents see
	// "failed to verify token" crypto errors and, because the self-service
	// refresh endpoint authenticates with the (now-invalid) token, cannot
	// recover on their own. This is expected on genuine first boot, but after a
	// restart that changed the hub identity (e.g. a new pod hostname -> new
	// HubID) without a SharedSigningSecret it silently orphans every live agent.
	//
	// Fail-fast when the operator has opted into stable-key enforcement, and
	// otherwise make the token-invalidating event loud (error-level) so it is
	// alertable rather than buried in a warning.
	if s.config.RequireStableSigningKey {
		return nil, fmt.Errorf("refusing to generate a new signing key %q: RequireStableSigningKey is set and no existing key was found "+
			"(generating one would invalidate all live agent/user tokens); provide a SharedSigningSecret or pre-provision the key", keyName)
	}
	if hasSecretBackend {
		slog.Error("ensureSigningKey: no existing signing key found despite a configured secret backend; generating a NEW key — ALL previously issued tokens are now INVALID",
			"key", keyName,
			"hub_id", hubID,
			"hint", "set a SharedSigningSecret (SESSION_SECRET) or pin a stable HubID so signing keys persist across restarts/redeploys",
		)
	}

	slog.Warn("Signing key not found in any source, generating new key", "key", keyName, "hub_id", hubID)
	newKey := make([]byte, 32)
	if _, err := rand.Read(newKey); err != nil {
		return nil, fmt.Errorf("failed to generate random signing key: %w", err)
	}

	encodedKey := base64.StdEncoding.EncodeToString(newKey)
	fp := sha256.Sum256(newKey)
	slog.Info("ensureSigningKey: generated new key",
		"key", keyName,
		"source", "generated",
		"scope_id", hubID,
		"key_len", len(newKey),
		"sha256_prefix", hex.EncodeToString(fp[:8]),
	)

	// Save through the secret backend first
	if hasSecretBackend {
		input := &secret.SetSecretInput{
			Name:        keyName,
			Value:       encodedKey,
			SecretType:  store.SecretTypeInternal,
			Scope:       store.ScopeHub,
			ScopeID:     hubID,
			Description: fmt.Sprintf("Hub signing key for %s", keyName),
		}
		if _, _, err := s.secretBackend.Set(ctx, input); err != nil {
			if isGCPBackend {
				return nil, fmt.Errorf("failed to persist signing key %s to Secret Manager: %w", keyName, err)
			}
			slog.Warn("Secret backend unavailable for signing key, falling back to store", "key", keyName, "error", err)
		} else {
			slog.Info("Persisted new signing key via secret backend", "key", keyName)
			// Also persist to SQLite as backup (value only, preserving SecretRef from Set).
			if persistErr := s.backupSigningKeyToStore(ctx, keyName, encodedKey, hubID); persistErr != nil {
				slog.Warn("Failed to persist signing key backup to store", "key", keyName, "error", persistErr)
			}
			return newKey, nil
		}
	}

	// Fallback: save directly to the store (acceptable only for local dev without SM)
	if err := s.backupSigningKeyToStore(ctx, keyName, encodedKey, hubID); err != nil {
		slog.Warn("Failed to persist signing key", "key", keyName, "error", err)
	} else {
		slog.Info("Persisted new signing key to store", "key", keyName)
	}

	return newKey, nil
}

// syncSigningKeyToBackend syncs a signing key (found in SQLite) to the secret backend
// and maintains a local SQLite backup. When isGCPBackend is true, a sync failure is
// treated as a fatal error since the key would not survive a database reset.
func (s *Server) syncSigningKeyToBackend(ctx context.Context, keyName, encodedValue, hubID string, isGCPBackend bool) error {
	if s.secretBackend == nil {
		return nil
	}
	input := &secret.SetSecretInput{
		Name:        keyName,
		Value:       encodedValue,
		SecretType:  store.SecretTypeInternal,
		Scope:       store.ScopeHub,
		ScopeID:     hubID,
		Description: fmt.Sprintf("Hub signing key for %s (synced from store)", keyName),
	}
	if _, _, syncErr := s.secretBackend.Set(ctx, input); syncErr != nil {
		if isGCPBackend {
			return fmt.Errorf("failed to sync signing key %s to Secret Manager: %w", keyName, syncErr)
		}
		slog.Warn("Failed to sync signing key to secret backend", "key", keyName, "error", syncErr)
	} else {
		slog.Info("Synced signing key to secret backend", "key", keyName)
	}
	// Re-persist the actual value to SQLite as backup. The backend's Set() stores
	// EncryptedValue="" (using a SecretRef), so without this the key material
	// would be lost if the secret backend becomes unavailable.
	if persistErr := s.backupSigningKeyToStore(ctx, keyName, encodedValue, hubID); persistErr != nil {
		slog.Warn("Failed to re-persist signing key backup to store after sync", "key", keyName, "error", persistErr)
	}
	return nil
}

// logSigningKeyFailure logs a signing key loading failure for non-production
// (local/dev) deployments. When GCPBackend is configured, signing key failures
// are fatal and returned as errors from New() instead of reaching this function.
func logSigningKeyFailure(keyType string, err error) {
	slog.Warn("Failed to load signing key, will use ephemeral key (local dev only)", "key_type", keyType, "error", err)
}

// backupSigningKeyToStore saves a signing key value to SQLite as a local backup.
// If a record already exists (e.g. with a SecretRef from GCPBackend.Set), only the
// EncryptedValue is updated — the SecretRef is preserved so the UI and other consumers
// can see that the secret is backed by Secret Manager.
func (s *Server) backupSigningKeyToStore(ctx context.Context, keyName, encodedValue, hubID string) error {
	existing, err := s.store.GetSecret(ctx, keyName, store.ScopeHub, hubID)
	if err == nil {
		// Record exists — update value only, preserving SecretRef and other metadata.
		existing.EncryptedValue = encodedValue
		return s.store.UpdateSecret(ctx, existing)
	}
	if err != store.ErrNotFound {
		return fmt.Errorf("checking existing secret record: %w", err)
	}
	// No existing record — create a new one.
	sec := &store.Secret{
		ID:             signingKeySecretID(keyName, hubID),
		Key:            keyName,
		EncryptedValue: encodedValue,
		Scope:          store.ScopeHub,
		ScopeID:        hubID,
		SecretType:     store.SecretTypeInternal,
		Description:    fmt.Sprintf("Hub signing key for %s", keyName),
	}
	_, err = s.store.UpsertSecret(ctx, sec)
	return err
}

// signingKeySecretID returns a deterministic primary key for a signing key record,
// scoped to the hub instance to avoid PK collisions during migration.
// signingKeySecretID derives a stable surrogate primary key for the signing-key
// backup secret. The store keys secrets by the (key, scope, scope_id) triple, so
// the ID is only a surrogate; it is generated deterministically as a UUIDv5 so
// the value is valid for the UUID-typed primary key while remaining stable
// across restarts.
func signingKeySecretID(keyName, hubID string) string {
	return uuid.NewSHA1(uuid.NameSpaceOID, []byte("hub-signing-key:"+hubID+":"+keyName)).String()
}

// SetDispatcher sets the agent dispatcher for co-located runtime broker operations.
func (s *Server) SetDispatcher(d AgentDispatcher) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.dispatcher = d
}

// GetDispatcher returns the current agent dispatcher.
func (s *Server) GetDispatcher() AgentDispatcher {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.dispatcher
}

// SetEmbeddedBrokerID records the broker ID for a co-located runtime broker
// running in the same process as the hub. This allows the hub to skip GCS
// sync operations when the broker already has filesystem access.
func (s *Server) SetEmbeddedBrokerID(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.embeddedBrokerID = id
}

// SetStatelessEmbeddedBrokerID records a co-located broker whose runtime
// lifecycle operations are safe from any hub replica.
func (s *Server) SetStatelessEmbeddedBrokerID(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.embeddedBrokerID = id
	s.statelessEmbeddedBroker = id != ""
}

// SetRuntimeReloadFunc registers a callback that reloads the co-located
// broker's container runtime. Called during startup wiring so that
// reloadSettings can trigger a runtime swap without a full restart.
// The callback returns true if the runtime was actually swapped.
func (s *Server) SetRuntimeReloadFunc(fn func() bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.runtimeReloadFunc = fn
}

// GetEmbeddedBrokerID returns the co-located broker ID, if any.
func (s *Server) GetEmbeddedBrokerID() string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.embeddedBrokerID
}

// GetStatelessEmbeddedBrokerID returns the embedded stateless broker ID, if any.
func (s *Server) GetStatelessEmbeddedBrokerID() string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if !s.statelessEmbeddedBroker {
		return ""
	}
	return s.embeddedBrokerID
}

// isEmbeddedBroker returns true if brokerID matches the co-located broker
// running in the same process as the hub.
func (s *Server) isEmbeddedBroker(brokerID string) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.embeddedBrokerID != "" && s.embeddedBrokerID == brokerID
}

// SetStorage sets the storage backend for template files.
func (s *Server) SetStorage(stor storage.Storage) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.storage = stor
}

// SetRequestLogger sets the dedicated request logger.
func (s *Server) SetRequestLogger(l *slog.Logger) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.requestLogger = l
}

// SetMessageLogger sets the dedicated message audit logger.
// When set, message dispatch events are logged to this logger in addition
// to the standard subsystem logger, enabling a separate "scion-messages"
// log stream in Cloud Logging.
func (s *Server) SetMessageLogger(l *slog.Logger) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.dedicatedMessageLog = l
}

// SetChannelRegistry sets the notification channel registry for external delivery.
// When set, user notifications are also dispatched to configured external channels
// (webhook, Slack, etc.) in addition to the standard SSE pipeline.
func (s *Server) SetChannelRegistry(r *ChannelRegistry) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.channelRegistry = r
}

// SetMessageBrokerProxy sets the message broker proxy for pub/sub message routing.
// When set, messages can be routed through the broker instead of direct dispatch.
func (s *Server) SetMessageBrokerProxy(p *MessageBrokerProxy) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.messageBrokerProxy = p
}

// GetMessageBrokerProxy returns the current message broker proxy (nil if disabled).
func (s *Server) GetMessageBrokerProxy() *MessageBrokerProxy {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.messageBrokerProxy
}

// SetWebChatStore sets the webchat store for thread prefs and chat threads API.
// It also initializes the ChatNotifier for human-mention and DM notifications (W6).
func (s *Server) SetWebChatStore(wcs WebChatStore) {
	s.mu.Lock()
	s.webChatStore = wcs
	// Initialize ChatNotifier with the store. Presence is resolved lazily
	// through the server (see serverPresenceChecker): the presence manager is
	// created by InitPresenceManager, which runs after this on the current
	// startup path, and a snapshot taken here would pin a nil checker.
	s.chatNotifier = NewChatNotifier(s.store, s.events, wcs, serverPresenceChecker{s}, s.messageLog)
	// Wire into existing broker proxy if already started (startup order varies).
	if s.messageBrokerProxy != nil {
		s.messageBrokerProxy.chatNotifier = s.chatNotifier
		s.messageBrokerProxy.webChatStore = wcs
	}
	s.mu.Unlock()
}

// SetAttachmentStore sets the attachment file store for upload/download (W7).
func (s *Server) SetAttachmentStore(as AttachmentStore) {
	s.mu.Lock()
	s.attachmentStore = as
	s.mu.Unlock()
}

// getChatNotifier returns the chat notifier, or nil if not initialized.
func (s *Server) getChatNotifier() *ChatNotifier {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.chatNotifier
}

// serverPresenceChecker adapts the server's presence manager to the
// PresenceChecker interface, resolving it at call time rather than at
// construction time. Startup wires the webchat store (and with it the
// ChatNotifier) before InitPresenceManager runs, so a checker captured up
// front would be permanently absent-reporting — the defect this replaces.
type serverPresenceChecker struct {
	srv *Server
}

// IsUserActive reports whether the user is currently present, or false while
// no presence manager exists (before InitPresenceManager, or in deployments
// that never start one).
func (c serverPresenceChecker) IsUserActive(userID string) bool {
	if c.srv == nil {
		return false
	}
	c.srv.mu.RLock()
	pm := c.srv.presenceManager
	c.srv.mu.RUnlock()
	return pm.IsUserActive(userID)
}

// InitPresenceManager creates and starts the presence manager for real-time
// user presence tracking. It seeds the in-memory map from User.last_seen.
// Call this after the event publisher is wired.
func (s *Server) InitPresenceManager() {
	pm := NewPresenceManager(s.events, s.store)
	pm.SeedFromStore(s.ctx, s.store)
	s.mu.Lock()
	s.presenceManager = pm
	s.mu.Unlock()
}

// StopPresenceManager shuts down the presence manager's background goroutine.
func (s *Server) StopPresenceManager() {
	s.mu.RLock()
	pm := s.presenceManager
	s.mu.RUnlock()
	if pm != nil {
		pm.Stop()
	}
}

// SetPluginManager sets the plugin manager for broker integration admin API.
func (s *Server) SetPluginManager(m IntegrationManager) {
	s.mu.Lock()
	s.pluginManager = m
	s.mu.Unlock()

	s.registerReconnectCallbacks(m)
}

// SetIntegrationHA configures Mode 3 (HA) integration support on the server.
// It sets the database driver, Ent client, and DSN needed for Postgres-backed
// integration config/update endpoints and the admin signal listener.
func (s *Server) SetIntegrationHA(dbDriver string, client *ent.Client, dsn string) {
	s.mu.Lock()
	s.dbDriver = dbDriver
	s.entClient = client
	s.databaseDSN = dsn

	// Initialize GitHub resolution store when ent client is available
	if client != nil {
		s.ghResolutionStore = NewGitHubResolutionStore(client)
	}

	// Initialize DB-backed nonce cache store and wire it into the broker auth
	// service for cross-instance HMAC replay protection.
	if client != nil && s.brokerAuthService != nil {
		s.nonceCacheStore = NewNonceCacheStore(client)
		s.brokerAuthService.SetNonceCacheStore(s.nonceCacheStore)
	}

	// Initialize DB-backed chat link code store and wire it into link services
	// so codes are shared across Hub instances.
	if client != nil {
		s.chatLinkStore = NewChatLinkStore(client)
		if s.telegramLinkService != nil {
			s.telegramLinkService.SetStore(s.chatLinkStore)
		}
		if s.discordLinkService != nil {
			s.discordLinkService.SetStore(s.chatLinkStore)
		}
		if s.teamsLinkService != nil {
			s.teamsLinkService.SetStore(s.chatLinkStore)
		}
	}

	s.mu.Unlock()

	if client != nil {
		s.sweepOrphanedUpdates()
	}
}

// IsPostgres reports whether the hub is running on a Postgres backend.
func (s *Server) IsPostgres() bool {
	return strings.EqualFold(s.dbDriver, "postgres")
}

// SetOperationalSettings attaches the OperationalSettings service to the
// server. This is called during postgres-mode startup after seeding and
// initial refresh (settings-db §3.5/§3.9). Safe for concurrent use.
func (s *Server) SetOperationalSettings(ops *OperationalSettings) {
	s.operationalSettings.Store(ops)
}

// GetOperationalSettings returns the OperationalSettings service, or nil
// in file/SQLite mode. Safe for concurrent use.
func (s *Server) GetOperationalSettings() *OperationalSettings {
	return s.operationalSettings.Load()
}

// logMessage logs a message dispatch event to the dedicated message logger
// if configured, otherwise falls back to the standard subsystem message logger.
func (s *Server) logMessage(msg string, attrs ...any) {
	if s.dedicatedMessageLog != nil {
		s.dedicatedMessageLog.Info(msg, attrs...)
	} else {
		s.messageLog.Info(msg, attrs...)
	}
}

// GetStorage returns the current storage backend.
func (s *Server) GetStorage() storage.Storage {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.storage
}

// SetHubID sets the unique hub instance ID for secret namespacing.
func (s *Server) SetHubID(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.hubID = id
}

// HubID returns the hub instance ID. Thread-safe.
func (s *Server) HubID() string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.hubID
}

// LegacyFallbackEnabled returns true when legacy un-namespaced storage path
// fallback is active (the default). Returns false when the operator has
// explicitly disabled it after completing migration.
func (s *Server) LegacyFallbackEnabled() bool {
	return !s.config.DisableLegacyStorageFallback
}

// HubName returns the human-readable hub display name. Thread-safe.
func (s *Server) HubName() string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.config.HubName
}

// SetSecretBackend sets the secret backend for pluggable secret storage.
func (s *Server) SetSecretBackend(b secret.SecretBackend) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.secretBackend = b
}

// GetSecretBackend returns the current secret backend.
func (s *Server) GetSecretBackend() secret.SecretBackend {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.secretBackend
}

// GetAgentTokenService returns the agent token service.
func (s *Server) GetAgentTokenService() *AgentTokenService {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.agentTokenService
}

// GetUserTokenService returns the user token service.
func (s *Server) GetUserTokenService() *UserTokenService {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.userTokenService
}

// GetUATService returns the user access token service.
func (s *Server) GetUATService() *UserAccessTokenService {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.uatService
}

// GetOAuthService returns the OAuth service.
func (s *Server) GetOAuthService() *OAuthService {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.oauthService
}

// GetStore returns the data store.
func (s *Server) GetStore() store.Store {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.store
}

// GetAuthzService returns the authorization service.
func (s *Server) GetAuthzService() *AuthzService {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.authzService
}

// GetBrokerAuthService returns the broker authentication service.
func (s *Server) GetBrokerAuthService() *BrokerAuthService {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.brokerAuthService
}

// GetAuditLogger returns the audit logger.
func (s *Server) GetAuditLogger() AuditLogger {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.auditLogger
}

// SetAuditLogger sets a custom audit logger.
func (s *Server) SetAuditLogger(logger AuditLogger) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.auditLogger = logger
}

// GetMetrics returns the metrics recorder.
func (s *Server) GetMetrics() MetricsRecorder {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.metrics
}

// SetMetrics sets a custom metrics recorder.
func (s *Server) SetMetrics(m MetricsRecorder) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.metrics = m
}

// SetDBMetrics wires the database connection-pool / notify metrics recorder
// (P0-5). When set to an enabled recorder before StartBackgroundServices, the
// hub starts sampling the DB connection pool into the pool gauges. Passing a
// disabled recorder (or never calling this) leaves pool sampling off.
func (s *Server) SetDBMetrics(rec dbmetrics.Recorder) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.dbMetrics = rec
}

// SetDispatchMetrics wires the broker-dispatch metrics recorder (B5-2).
func (s *Server) SetDispatchMetrics(rec dispatchmetrics.Recorder) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.dispatchMetrics = rec
}

// SetGCPTokenMetrics wires the GCP token metrics recorder.
func (s *Server) SetGCPTokenMetrics(m GCPTokenMetricsRecorder) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.gcpTokenMetrics = m
}

// SetLocalImageChecker wires a local container runtime into the image
// checker so it can verify images via the local Docker/Podman daemon.
func (s *Server) SetLocalImageChecker(l imagecheck.LocalImageExister) {
	s.imageChecker.SetLocal(l)
	if mgr, ok := l.(imageManager); ok {
		s.imageManager = mgr
	}
}

// GetMaintenanceState returns the runtime maintenance state.
func (s *Server) GetMaintenanceState() *MaintenanceState {
	return s.maintenance
}

// SetEventPublisher sets the event publisher for real-time SSE updates.
// SetGCPTokenGenerator sets the GCP token generator for agent identity.
func (s *Server) SetGCPTokenGenerator(g GCPTokenGenerator) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.gcpTokenGenerator = g
}

// SetGCPServiceAccountAdmin sets the GCP IAM admin client for minting service accounts.
func (s *Server) SetGCPServiceAccountAdmin(a GCPServiceAccountAdmin) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.gcpIAMAdmin = a
}

// DenyUnknownFailOpen returns the configured deny-unknown fallback policy.
// Used by server_foreground.go to pass the setting to the PT checker constructor.
func (s *Server) DenyUnknownFailOpen() bool {
	return s.denyUnknownFailOpen
}

// SetSAAssignChecker replaces the caller-permission checker for the agent
// service-account assignment surface. Both SetSAAssignChecker and
// SetHookIdentityChecker should typically be called with the same cached
// checker instance so that the two surfaces share one cache.
func (s *Server) SetSAAssignChecker(c store.CallerPermissionChecker) {
	s.mu.Lock()
	old := s.saAssignChecker
	s.saAssignChecker = c
	s.mu.Unlock()

	// Drain stale entries from the outgoing checker. Entries cached under the
	// old checker's inner would produce decisions against the wrong backend
	// if the reference leaked (it shouldn't, but belt-and-suspenders).
	if cc, ok := old.(*CachedCallerPermissionChecker); ok {
		cc.InvalidateAll()
	}
}

// SetHookIdentityChecker replaces the caller-permission checker for the
// lifecycle-hook execution-identity surface.
func (s *Server) SetHookIdentityChecker(c store.CallerPermissionChecker) {
	s.mu.Lock()
	old := s.hookIdentityChecker
	s.hookIdentityChecker = c
	s.mu.Unlock()

	// Drain stale entries — same rationale as SetSAAssignChecker.
	if cc, ok := old.(*CachedCallerPermissionChecker); ok {
		cc.InvalidateAll()
	}
}

// invalidateActAsCache removes cached actAs decisions for a specific SA.
// Called after SA deletion and Hub-initiated IAM mutations (mint path).
// No-op if the configured checkers do not support invalidation (e.g.
// DisabledCallerPermissionChecker when gcpIamCheckMode=off).
func (s *Server) invalidateActAsCache(saEmail string) {
	s.mu.RLock()
	assignChecker := s.saAssignChecker
	hookChecker := s.hookIdentityChecker
	s.mu.RUnlock()

	if c, ok := assignChecker.(*CachedCallerPermissionChecker); ok {
		c.InvalidateForSA(saEmail)
	}
	if c, ok := hookChecker.(*CachedCallerPermissionChecker); ok {
		c.InvalidateForSA(saEmail)
	}
}

// SetGCPProjectID sets the GCP project ID used for minting service accounts.
func (s *Server) SetGCPProjectID(projectID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.config.GCPProjectID = projectID
}

func (s *Server) SetEventPublisher(ep EventPublisher) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.events = ep
}

// SetCommandBus sets the inter-node dispatch signal bus. Nil is safe (treated
// as no-op). Called from the server-foreground init path after backend selection.
func (s *Server) SetCommandBus(cb CommandBus) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.commandBus = cb
	if pgBus, ok := cb.(*PostgresCommandBus); ok {
		pgBus.SetOnReconnect(func() {
			if rec := s.dispatchMetrics; rec != nil {
				rec.IncCmdBusReconnects(context.Background(), 1)
			}
		})
	}
}

// CommandBus returns the configured command bus, or nil.
func (s *Server) CommandBus() CommandBus { return s.commandBus }

// StartNotificationDispatcher creates and starts the notification dispatcher
// if a subscription-capable EventPublisher is available. It uses a lazy getter for the
// AgentDispatcher so it works even if SetDispatcher is called later.
// Safe to call multiple times; subsequent calls are no-ops.
func (s *Server) StartNotificationDispatcher() {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.notificationDispatcher != nil {
		return // already started
	}

	if _, isNoop := s.events.(noopEventPublisher); isNoop || s.events == nil {
		slog.Warn("Event publisher does not support subscriptions, notification dispatcher not started")
		return
	}

	nd := NewNotificationDispatcher(s.store, s.events, s.GetDispatcher, logging.Subsystem("hub.notifications"))
	nd.messageLog = s.dedicatedMessageLog
	nd.channelRegistry = s.channelRegistry
	s.notificationDispatcher = nd
	s.notificationDispatcher.Start()
}

// StartLifecycleHookEvaluator creates and starts the lifecycle hook evaluator
// if a subscription-capable EventPublisher is available. The evaluator listens
// for authoritative agent phase transitions and fires matching lifecycle hooks
// asynchronously — it never blocks or aborts a transition.
// Safe to call multiple times; subsequent calls are no-ops.
func (s *Server) StartLifecycleHookEvaluator(opts ...EvaluatorOption) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.lifecycleHookEvaluator != nil {
		return // already started
	}

	if _, isNoop := s.events.(noopEventPublisher); isNoop || s.events == nil {
		slog.Warn("Event publisher does not support subscriptions, lifecycle hook evaluator not started")
		return
	}

	// In multi-instance HA the active publisher is *PostgresEventPublisher,
	// which broadcasts every transition to ALL hub instances. With the in-memory
	// deduper each instance would fire the hook independently (duplicate
	// register/deregister), so the broadcast publisher MUST use the durable
	// store-backed CAS deduper. Select it from the publisher type; explicit
	// caller opts still take precedence (they are applied last).
	allOpts := opts
	if driver := deduperDriverForPublisher(s.events); driver != "" {
		allOpts = append([]EvaluatorOption{WithDBDriver(driver)}, opts...)
	}

	executor := NewHTTPExecutor(s.store, s.gcpTokenGenerator, s.auditLogger, logging.Subsystem("hub.lifecycle-hooks.executor"))
	ev := NewLifecycleHookEvaluator(s.store, s.events, executor, logging.Subsystem("hub.lifecycle-hooks"), allOpts...)
	s.lifecycleHookEvaluator = ev
	s.lifecycleHookEvaluator.Start()
}

// StartMessageBroker creates and starts the message broker proxy if a
// subscription-capable EventPublisher is available. The broker enables pub/sub message
// routing with topic-based subscriptions and broadcast fan-out.
// Safe to call multiple times; subsequent calls are no-ops.
func (s *Server) StartMessageBroker(b eventbus.EventBus) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.messageBrokerProxy != nil {
		return // already started
	}

	if _, isNoop := s.events.(noopEventPublisher); isNoop || s.events == nil {
		slog.Warn("Event publisher does not support subscriptions, message broker proxy not started")
		return
	}

	proxy := NewMessageBrokerProxy(b, s.store, s.events, s.GetDispatcher, logging.Subsystem("hub.broker"))
	proxy.messageLog = s.dedicatedMessageLog
	proxy.chatNotifier = s.chatNotifier // W6: wire DM notification trigger
	proxy.webChatStore = s.webChatStore // DM watermark stamping after persist
	s.messageBrokerProxy = proxy
	proxy.Start()

	// Wire broker proxy to notification dispatcher so user notifications
	// flow through the broker plugin instead of the channel registry.
	if s.notificationDispatcher != nil {
		s.notificationDispatcher.SetBrokerProxy(proxy)
	}
}

// GetControlChannelManager returns the control channel manager.
func (s *Server) GetControlChannelManager() *ControlChannelManager {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.controlChannel
}

// CreateAuthenticatedDispatcher creates an HTTPAgentDispatcher with authenticated
// broker communication. This dispatcher signs outgoing requests to Runtime Brokers
// using HMAC authentication based on shared secrets stored in the database.
// It also supports control channel fallback for NAT traversal.
func (s *Server) CreateAuthenticatedDispatcher() *HTTPAgentDispatcher {
	// Create authenticated HTTP client
	httpClient := NewAuthenticatedBrokerClient(s.store, s.config.Debug)

	// Wrap with hybrid client that prefers control channel
	var client RuntimeBrokerClient
	if s.controlChannel != nil {
		hbc := NewHybridBrokerClient(s.controlChannel, httpClient, &hmacBrokerSigner{store: s.store}, s.config.Debug)
		hbc.SetAffinityLookup(StoreAffinityLookup(s.store, 0))
		if statelessBrokerID := s.GetStatelessEmbeddedBrokerID(); statelessBrokerID != "" {
			hbc.SetStatelessLocalBrokers([]string{statelessBrokerID})
		}
		s.brokerClient = hbc
		client = hbc
	} else {
		client = httpClient
	}

	dispatcher := NewHTTPAgentDispatcherWithClient(s.store, client, s.config.Debug, logging.Subsystem("hub.dispatcher"))

	// Configure token generator if available
	if s.agentTokenService != nil {
		dispatcher.SetTokenGenerator(s)
	} else if s.config.Debug {
		slog.Warn("No agent token service configured - agents won't have Hub credentials")
	}

	// Set Hub endpoint if configured
	if s.config.HubEndpoint != "" {
		dispatcher.SetHubEndpoint(s.config.HubEndpoint)
		if s.config.Debug {
			slog.Debug("Dispatcher hub endpoint configured", "endpoint", s.config.HubEndpoint)
		}
	} else if s.config.Debug {
		slog.Warn("No hub.endpoint configured - agents won't know how to reach Hub")
		slog.Info("Configure via: hub.endpoint in server.yaml or SCION_SERVER_HUB_ENDPOINT env var")
	}

	// Set Hub name so agent log entries carry the hub label.
	if s.config.HubName != "" {
		dispatcher.SetHubName(s.config.HubName)
	}

	// Pass hub ID and secret backend to dispatcher if configured
	dispatcher.SetHubID(s.hubID)
	if s.secretBackend != nil {
		dispatcher.SetSecretBackend(s.secretBackend)
	}
	if s.authzService != nil {
		dispatcher.SetAuthzService(s.authzService)
	}

	// In dev-auth mode, pass the dev token so agents get it for fallback auth
	if s.config.DevAuthToken != "" {
		dispatcher.SetDevAuthToken(s.config.DevAuthToken)
	}

	// Configure GitHub App token minter if the app is configured
	if s.config.GitHubAppConfig.AppID != 0 {
		dispatcher.SetGitHubAppMinter(s)
	}

	// Wire cross-node lifecycle dispatch deps (B4-2) so the dispatcher
	// can handle ErrLifecycleDeferred from route-gated Start/Stop/Restart
	// by writing durable intent, signaling the owning node, and waiting
	// for the terminal phase. In SQLite mode events/commandBus are no-ops,
	// and route() always returns routeLocal, so this never triggers.
	dispatcher.SetCrossNodeDeps(s.events, s.commandBus)
	if s.dispatchMetrics != nil {
		dispatcher.SetDispatchMetrics(s.dispatchMetrics)
	}

	// Configure transport token minter if available
	if s.transportMinter != nil && s.transportAudience != "" {
		dispatcher.SetTransportMinter(s.transportMinter, s.transportAudience, s.transportMode)
	}

	// Wire resource hash repair so the dispatcher can auto-fix stale DB
	// manifests when the shared GCS bucket was updated by another hub.
	dispatcher.SetHarnessConfigRepairer(s.syncHarnessConfigFromStorage)
	dispatcher.SetTemplateRepairer(s.syncTemplateFromStorage)

	// Wire the hub's operational agent_defaults so dispatch can carry the
	// limit/resource ones to the broker's low-precedence tier. The accessor
	// takes s.mu; it returns the zero value in file mode, where the wire field
	// is then omitted and broker behaviour is unchanged.
	dispatcher.SetHubAgentDefaultsProvider(s.hubAgentDefaults)

	// Set image registry so bare image names are rewritten before dispatch
	dispatcher.SetImageRegistry(s.resolveImageRegistry())

	return dispatcher
}

// GenerateAgentToken generates a JWT for an agent.
// This is a convenience method that delegates to the token service.
// Base scopes are determined by the passed role.
// Dev-auth mode overrides to full if the role would be more restrictive,
// preserving dev-mode behavior where all agents get full access.
// Additional scopes are merged with the role-based defaults, deduplicated.
func (s *Server) GenerateAgentToken(agentID, projectID string, ancestry []string, role AgentRole, additionalScopes []AgentTokenScope) (string, error) {
	s.mu.RLock()
	tokenService := s.agentTokenService
	s.mu.RUnlock()

	if tokenService == nil {
		return "", fmt.Errorf("agent token service not initialized")
	}

	// Use the specified role for base scopes.
	// Dev-auth mode overrides to full if the role would be more restrictive,
	// preserving dev-mode behavior where all agents get full access.
	effectiveRole := role
	if s.config.DevAuthToken != "" && CompareRoles(role, AgentRoleFull) < 0 {
		effectiveRole = AgentRoleFull
	}
	scopes := ScopesForRole(effectiveRole)

	// Merge additional scopes, deduplicating
	seen := make(map[AgentTokenScope]bool, len(scopes))
	for _, sc := range scopes {
		seen[sc] = true
	}
	for _, scope := range additionalScopes {
		if !seen[scope] {
			scopes = append(scopes, scope)
			seen[scope] = true
		}
	}

	return tokenService.GenerateAgentToken(agentID, projectID, scopes, ancestry)
}

// agentHeartbeatTimeoutHandler returns a recurring handler function that marks
// agents as offline when their last heartbeat exceeds a 2-minute threshold.
// It publishes status events for each affected agent so SSE subscribers and the
// notification system are informed.
func (s *Server) agentHeartbeatTimeoutHandler() func(ctx context.Context) {
	return func(ctx context.Context) {
		// Tight timeout: fail fast if DB connections are saturated rather than
		// holding a connection while waiting, which worsens the thundering herd.
		ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
		defer cancel()

		threshold := time.Now().Add(-2 * time.Minute)

		agents, err := s.store.MarkStaleAgentsOffline(ctx, threshold)
		if err != nil {
			slog.Error("Scheduler: heartbeat timeout check failed", "error", err)
			return
		}

		for i := range agents {
			s.events.PublishAgentStatus(ctx, &agents[i])
		}

		if len(agents) > 0 {
			slog.Info("Scheduler: marked stale agents as offline",
				"count", len(agents), "threshold", threshold)
		}
	}
}

// agentStalledDetectionHandler returns a recurring handler function that marks
// agents as stalled when their last activity event exceeds the stalled threshold
// but they still have a recent heartbeat (process alive but hung).
// It publishes status events for each affected agent so SSE subscribers and the
// notification system are informed.
// When AutoSuspendStalled is enabled, stalled agents are additionally suspended
// (container stopped, phase set to "suspended").
func (s *Server) agentStalledDetectionHandler() func(ctx context.Context) {
	return func(ctx context.Context) {
		// Tight timeout: fail fast if DB connections are saturated rather than
		// holding a connection while waiting, which worsens the thundering herd.
		ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
		defer cancel()

		activityThreshold := time.Now().Add(-s.config.StalledThreshold)
		heartbeatRecency := time.Now().Add(-2 * time.Minute)

		agents, err := s.store.MarkStalledAgents(ctx, activityThreshold, heartbeatRecency)
		if err != nil {
			slog.Error("Scheduler: stalled detection check failed", "error", err)
			return
		}

		for i := range agents {
			s.events.PublishAgentStatus(ctx, &agents[i])
		}

		if len(agents) > 0 {
			slog.Info("Scheduler: marked stalled agents",
				"count", len(agents), "threshold", s.config.StalledThreshold)
		}

		// Auto-suspend stalled agents if enabled.
		s.mu.RLock()
		autoSuspend := s.config.AutoSuspendStalled
		s.mu.RUnlock()

		if autoSuspend && len(agents) > 0 {
			s.autoSuspendStalledAgents(ctx, agents)
		}
	}
}

// autoSuspendStalledAgents suspends agents that were just marked stalled.
// It stops the container via the dispatcher and transitions the phase to suspended.
// Agents whose harness does not support resume are skipped.
func (s *Server) autoSuspendStalledAgents(ctx context.Context, agents []store.Agent) {
	dispatcher := s.GetDispatcher()
	suspended := 0

	for i := range agents {
		agent := &agents[i]

		// Skip agents whose harness does not support resume — suspending
		// them would imply resumability that doesn't exist.
		if agent.AppliedConfig != nil && agent.AppliedConfig.HarnessConfig != "" {
			h := harness.New(agent.AppliedConfig.HarnessConfig)
			if h.AdvancedCapabilities().Resume.Support == api.SupportNo {
				slog.Debug("Scheduler: skipping auto-suspend for non-resumable harness",
					"agent_id", agent.ID, "harness", agent.AppliedConfig.HarnessConfig)
				continue
			}
		}

		if agent.RuntimeBrokerID != "" {
			if dispatcher == nil {
				slog.Error("Scheduler: cannot auto-suspend agent because dispatcher is nil",
					"agent_id", agent.ID, "agent_name", agent.Name)
				continue
			}
			s.syncWorkspaceOnStop(ctx, agent)
			if err := dispatcher.DispatchAgentStop(ctx, agent); err != nil {
				slog.Error("Scheduler: auto-suspend dispatch failed",
					"agent_id", agent.ID, "agent_name", agent.Name, "error", err)
				continue
			}
		}

		statusUpdate := store.AgentStatusUpdate{
			Phase:           string(state.PhaseSuspended),
			ContainerStatus: "stopped",
			Activity:        "",
		}
		if err := s.store.UpdateAgentStatus(ctx, agent.ID, statusUpdate); err != nil {
			slog.Error("Scheduler: auto-suspend status update failed",
				"agent_id", agent.ID, "agent_name", agent.Name, "error", err)
			continue
		}

		agent.Phase = string(state.PhaseSuspended)
		agent.ContainerStatus = "stopped"
		agent.Activity = ""
		s.events.PublishAgentStatus(ctx, agent)
		suspended++
	}

	if suspended > 0 {
		slog.Info("Scheduler: auto-suspended stalled agents", "count", suspended)
	}
}

// purgeHandler returns a recurring handler function that permanently removes
// soft-deleted agents that have exceeded the retention period.
func (s *Server) purgeHandler() func(ctx context.Context) {
	return func(ctx context.Context) {
		// Purge soft-deleted agents
		cutoff := time.Now().Add(-s.config.SoftDeleteRetention)
		purged, err := s.store.PurgeDeletedAgents(ctx, cutoff)
		if err != nil {
			slog.Error("Scheduler: agent purge failed", "error", err)
		} else if purged > 0 {
			slog.Info("Scheduler: purged soft-deleted agents", "count", purged, "cutoff", cutoff)
		}

		// Purge old scheduled events (non-pending, older than 7 days)
		eventCutoff := time.Now().Add(-7 * 24 * time.Hour)
		purgedEvents, err := s.store.PurgeOldScheduledEvents(ctx, eventCutoff)
		if err != nil {
			slog.Error("Scheduler: scheduled event purge failed", "error", err)
		} else if purgedEvents > 0 {
			slog.Info("Scheduler: purged old scheduled events", "count", purgedEvents)
		}
	}
}

// MessageEventPayload is the JSON payload for "message" type scheduled events.
type MessageEventPayload struct {
	AgentID   string `json:"agentId,omitempty"`
	AgentName string `json:"agentName,omitempty"`
	Message   string `json:"message"`
	Interrupt bool   `json:"interrupt,omitempty"`
	Plain     bool   `json:"plain,omitempty"`
}

// messageEventHandler returns an EventHandler that dispatches scheduled messages
// to agents via the AgentDispatcher.
func (s *Server) messageEventHandler() EventHandler {
	return func(ctx context.Context, evt store.ScheduledEvent) error {
		var payload MessageEventPayload
		if err := json.Unmarshal([]byte(evt.Payload), &payload); err != nil {
			return fmt.Errorf("invalid message payload: %w", err)
		}

		if payload.Message == "" {
			return fmt.Errorf("message payload is empty")
		}

		// Log staleness for events that fired late (e.g. after server downtime)
		staleness := time.Since(evt.FireAt)
		if !evt.FireAt.IsZero() && staleness > 1*time.Minute {
			slog.Warn("Scheduler: firing stale message event",
				"eventID", evt.ID,
				"agentName", payload.AgentName,
				"agent_id", payload.AgentID,
				"scheduledFor", evt.FireAt.Format(time.RFC3339),
				"staleness", staleness.Truncate(time.Second).String())
		}

		// Resolve the target agent name for logging
		targetName := payload.AgentName
		if targetName == "" {
			targetName = payload.AgentID
		}

		// Resolve the agent
		var agent *store.Agent
		var err error
		if payload.AgentID != "" {
			agent, err = s.store.GetAgent(ctx, payload.AgentID)
		} else if payload.AgentName != "" && evt.ProjectID != "" {
			agent, err = s.store.GetAgentBySlug(ctx, evt.ProjectID, payload.AgentName)
		} else {
			return fmt.Errorf("message payload must include agentId or agentName")
		}
		if err != nil {
			if errors.Is(err, store.ErrNotFound) {
				slog.Warn("Scheduler: target agent no longer exists, marking event as failed",
					"eventID", evt.ID,
					"agentName", payload.AgentName,
					"agent_id", payload.AgentID,
					"projectID", evt.ProjectID,
					"message", payload.Message)
				now := time.Now()
				_ = s.store.UpdateScheduledEventStatus(ctx, evt.ID, store.ScheduledEventFailed, &now, "target agent deleted")
				return nil
			}
			return fmt.Errorf("failed to resolve agent %q: %w", targetName, err)
		}

		dispatcher := s.GetDispatcher()
		if dispatcher == nil {
			return fmt.Errorf("no dispatcher available to deliver message")
		}

		// Reconstruct structured message from payload to preserve traits like Plain.
		structuredMsg := messages.NewSystemMessage("scheduler", "agent:"+agent.Slug, payload.Message, messages.SystemCategoryScheduler)
		structuredMsg.SenderID = "SCHEDULER"
		structuredMsg.RecipientID = agent.ID
		structuredMsg.Plain = payload.Plain
		structuredMsg.Urgent = payload.Interrupt

		retryCtx, retryCancel := context.WithTimeout(ctx, 30*time.Second)
		defer retryCancel()

		if err := dispatchWithBrokerRetry(retryCtx, dispatcher, agent, payload.Message, payload.Interrupt, structuredMsg); err != nil {
			return fmt.Errorf("failed to dispatch message to agent %s: %w", agent.Name, err)
		}
		slog.Info("Scheduler: message delivered to agent",
			"eventID", evt.ID, "agent_id", agent.ID, "agentName", agent.Name)
		return nil
	}
}

// DispatchAgentEventPayload is the JSON payload for "dispatch_agent" type scheduled events.
type DispatchAgentEventPayload struct {
	AgentName string `json:"agentName"`
	Template  string `json:"template,omitempty"`
	Task      string `json:"task,omitempty"`
	Branch    string `json:"branch,omitempty"`
}

// dispatchAgentEventHandler returns an EventHandler that creates and starts
// an agent in the project via the AgentDispatcher.
func (s *Server) dispatchAgentEventHandler() EventHandler {
	return func(ctx context.Context, evt store.ScheduledEvent) error {
		var payload DispatchAgentEventPayload
		if err := json.Unmarshal([]byte(evt.Payload), &payload); err != nil {
			return fmt.Errorf("invalid dispatch_agent payload: %w", err)
		}

		if payload.AgentName == "" {
			return fmt.Errorf("dispatch_agent payload: agentName is required")
		}

		// Log staleness for late fires
		staleness := time.Since(evt.FireAt)
		if !evt.FireAt.IsZero() && staleness > 1*time.Minute {
			slog.Warn("Scheduler: firing stale dispatch_agent event",
				"eventID", evt.ID,
				"agentName", payload.AgentName,
				"scheduledFor", evt.FireAt.Format(time.RFC3339),
				"staleness", staleness.Truncate(time.Second).String())
		}

		// Validate agent name
		slug, err := api.ValidateAgentName(payload.AgentName)
		if err != nil {
			return fmt.Errorf("invalid agent name %q: %w", payload.AgentName, err)
		}

		// Verify project exists
		project, err := s.store.GetProject(ctx, evt.ProjectID)
		if err != nil {
			if errors.Is(err, store.ErrNotFound) {
				return fmt.Errorf("project %q no longer exists", evt.ProjectID)
			}
			return fmt.Errorf("failed to resolve project %q: %w", evt.ProjectID, err)
		}

		// Resolve the runtime broker for this project
		runtimeBrokerID := ""
		providers, provErr := s.store.GetProjectProviders(ctx, evt.ProjectID)
		if provErr == nil && len(providers) > 0 {
			runtimeBrokerID = providers[0].BrokerID
		}

		// Check if an agent with this name already exists
		existingAgent, err := s.store.GetAgentBySlug(ctx, evt.ProjectID, slug)
		if err == nil && existingAgent != nil {
			slog.Warn("Scheduler: agent already exists, skipping dispatch_agent",
				"eventID", evt.ID,
				"agentName", slug,
				"projectID", evt.ProjectID,
				"existingPhase", existingAgent.Phase)
			return fmt.Errorf("agent %q already exists in project", slug)
		}

		// Create the agent record
		agent := &store.Agent{
			ID:              api.NewUUID(),
			Slug:            slug,
			Name:            slug,
			Template:        payload.Template,
			ProjectID:       evt.ProjectID,
			RuntimeBrokerID: runtimeBrokerID,
			Phase:           "created",
			Detached:        true,
			CreatedBy:       evt.CreatedBy,
		}

		// Build applied config with task
		agent.AppliedConfig = &store.AgentAppliedConfig{}
		if payload.Task != "" {
			agent.AppliedConfig.Task = payload.Task
		}
		if payload.Branch != "" {
			agent.AppliedConfig.Branch = payload.Branch
		}

		// Apply project-level default template if none specified
		if payload.Template == "" && project != nil && project.Annotations != nil {
			if dt := project.Annotations[projectSettingDefaultTemplate]; dt != "" {
				payload.Template = dt
				agent.Template = dt
			}
		}

		// Hub operational default template — lowest tier. Below the scheduled
		// payload and below the project annotation, both of which have already
		// had their chance above. Sets payload.Template and agent.Template
		// together, matching the annotation rung's shape. In file mode
		// hubAgentDefaults() is always the zero value so this never fires
		// (design §3.2.4). This is the twin of the rung on the agent-create
		// path in handlers_agents_core.go — design §5.2 risk (d) is these two
		// diverging again, so both paths get both rungs, no exceptions.
		templateFromHubDefault := false
		if payload.Template == "" {
			if d := s.hubAgentDefaults(); d.DefaultTemplate != "" {
				payload.Template = d.DefaultTemplate
				agent.Template = d.DefaultTemplate
				templateFromHubDefault = true
			}
		}

		// Resolve template if specified
		if payload.Template != "" {
			tmpl, tmplErr := s.resolveTemplate(ctx, payload.Template, evt.ProjectID)
			// DEGRADATION RULE (design §3.2.2), the scheduler-path equivalent of
			// the create path's. A resolve failure never fails a scheduled
			// dispatch on this path, so there is no 404 to suppress — but a name
			// that nothing can resolve must not be left on the agent record
			// either, because the broker would then try to hydrate a template
			// that does not exist. Clear it and say so. Gated on the
			// templateFromHubDefault flag, never inferred from the setting.
			//
			// A genuine not-found only. A transient store error is deliberately
			// excluded, for the reason spelled out at length on the create path
			// in handlers_agents_core.go: a DB blip is not evidence that the
			// setting is stale, and clearing on one would make some scheduled
			// dispatches silently lose their template and others keep it
			// depending on store weather. On a store error this path keeps its
			// pre-existing behaviour — leave the name alone and let the broker
			// try to resolve it locally.
			//
			// On the errors.Is limb: resolveTemplate collapses a miss to
			// (nil, nil) — it swallows store.ErrNotFound at each of its three
			// lookups — so with today's stores that limb does not fire, and
			// tmpl == nil is doing all the work. It is kept deliberately, and it
			// is not quite dead code: resolveTemplate swallows by equality
			// (err != store.ErrNotFound), so a store that ever WRAPPED
			// ErrNotFound would escape the swallow and arrive here, and this
			// limb would correctly read it as a definitive miss rather than as
			// an ambiguous failure. Checked at the time of writing: the ent
			// adapter returns bare store.ErrNotFound from mapError and
			// parseGetID, and nothing in pkg/store wraps it.
			if templateFromHubDefault && tmpl == nil &&
				(tmplErr == nil || errors.Is(tmplErr, store.ErrNotFound)) {
				s.warnHubDefaultTemplateUnusable(ctx, payload.Template, evt.ProjectID, "not found")
				payload.Template = ""
				agent.Template = ""
			}
			if tmplErr == nil && tmpl != nil {
				if tmpl.Slug != "" {
					agent.Template = tmpl.Slug
				}
				// Only fall back to the template's harness config when the
				// project has no default-harness-config annotation — the
				// project setting outranks the template, matching the
				// default-template block above and the agent-create path.
				//
				// MECHANISM NOTE — leaving AppliedConfig.HarnessConfig empty
				// when the annotation is present is deliberate:
				// applyProjectDefaults below is what actually applies the
				// project value on this path. The agent-create path in
				// handlers_agents_core.go reads the same annotation inline
				// instead, which makes applyProjectDefaults' harness-config
				// branch unreachable there. Behaviourally identical today —
				// keep the two in sync, and if you change
				// applyProjectDefaults' harness handling, check BOTH paths.
				projectHarnessConfig := ""
				if project != nil && project.Annotations != nil {
					projectHarnessConfig = project.Annotations[projectSettingDefaultHarnessConfig]
				}
				if projectHarnessConfig == "" {
					if harnessConfig := s.getHarnessConfigFromTemplate(tmpl, ""); harnessConfig != "" {
						agent.AppliedConfig.HarnessConfig = harnessConfig
					}
				}
			}
		}

		// Apply project-level defaults (harness config, limits, resources) from annotations
		applyProjectDefaults(agent.AppliedConfig, project)

		// Hub operational agent_defaults — strictly between applyProjectDefaults
		// and populateAgentConfig, exactly as on the agent-create path. See
		// applyHubAgentDefaults for why that placement is the whole point.
		if applyHubAgentDefaults(agent.AppliedConfig, s.hubAgentDefaults()) {
			ctx = withHubDefaultHarnessConfig(ctx)
		}

		s.populateAgentConfig(ctx, agent, project, nil)

		if err := s.store.CreateAgent(ctx, agent); err != nil {
			return fmt.Errorf("failed to create agent %q: %w", slug, err)
		}

		// Dispatch to runtime broker
		dispatcher := s.GetDispatcher()
		if dispatcher == nil {
			slog.Warn("Scheduler: no dispatcher available, agent created but not started",
				"eventID", evt.ID,
				"agent_id", agent.ID,
				"agentName", agent.Name)
			return nil
		}

		if err := dispatcher.DispatchAgentCreate(ctx, agent); err != nil {
			slog.Error("Scheduler: failed to dispatch agent creation",
				"eventID", evt.ID,
				"agent_id", agent.ID,
				"agentName", agent.Name,
				"error", err)
			return fmt.Errorf("failed to dispatch agent %q: %w", slug, err)
		}

		slog.Info("Scheduler: agent dispatched successfully",
			"eventID", evt.ID, "agent_id", agent.ID, "agentName", agent.Name,
			"project_id", evt.ProjectID)
		return nil
	}
}

// evaluateSchedulesHandler returns a recurring handler that evaluates due
// recurring schedules and fires their events. It queries active schedules
// whose next_run_at has passed, executes the action, and updates next_run_at.
func (s *Server) evaluateSchedulesHandler() func(ctx context.Context) {
	return func(ctx context.Context) {
		now := time.Now().UTC()
		dueSchedules, err := s.store.ListDueSchedules(ctx, now)
		if err != nil {
			slog.Error("schedule-evaluator: failed to list due schedules",
				"subsystem", "scheduler", "error", err)
			return
		}

		if len(dueSchedules) == 0 {
			return
		}

		slog.Debug("schedule-evaluator: evaluating due schedules",
			"subsystem", "scheduler", "count", len(dueSchedules))

		for _, sched := range dueSchedules {
			s.executeSchedule(ctx, sched, now)
		}
	}
}

// executeSchedule fires a single recurring schedule and updates its state.
func (s *Server) executeSchedule(ctx context.Context, sched store.Schedule, now time.Time) {
	log := slog.With("subsystem", "scheduler",
		"schedule_id", sched.ID, "schedule_name", sched.Name,
		"project_id", sched.ProjectID)

	// Compute next run time
	parser := cron.NewParser(cron.Minute | cron.Hour | cron.Dom | cron.Month | cron.Dow)
	cronSchedule, err := parser.Parse(sched.CronExpr)
	if err != nil {
		log.Error("schedule-evaluator: invalid cron expression",
			"cron_expr", sched.CronExpr, "error", err)
		_ = s.store.UpdateScheduleAfterRun(ctx, sched.ID, now, time.Time{},
			fmt.Sprintf("invalid cron expression: %v", err))
		return
	}
	nextRunAt := cronSchedule.Next(now)

	// Create a one-shot event from the schedule
	evt := store.ScheduledEvent{
		ID:         api.NewUUID(),
		ProjectID:  sched.ProjectID,
		EventType:  sched.EventType,
		FireAt:     now,
		Payload:    sched.Payload,
		Status:     store.ScheduledEventPending,
		CreatedBy:  sched.CreatedBy,
		ScheduleID: sched.ID,
	}

	if err := s.store.CreateScheduledEvent(ctx, &evt); err != nil {
		log.Error("schedule-evaluator: failed to create event", "error", err)
		_ = s.store.UpdateScheduleAfterRun(ctx, sched.ID, now, nextRunAt,
			fmt.Sprintf("failed to create event: %v", err))
		return
	}

	// Execute the event immediately
	var errMsg string
	handler, ok := s.scheduler.GetEventHandler(sched.EventType)
	if !ok {
		errMsg = fmt.Sprintf("unknown event type: %s", sched.EventType)
		log.Error("schedule-evaluator: unknown event type", "event_type", sched.EventType)
	} else {
		handlerCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
		if handlerErr := handler(handlerCtx, evt); handlerErr != nil {
			errMsg = handlerErr.Error()
			log.Warn("schedule-evaluator: event handler failed", "error", handlerErr)
		} else {
			log.Info("schedule-evaluator: schedule fired successfully")
		}
		cancel()
	}

	// Update event status
	firedAt := time.Now()
	status := store.ScheduledEventFired
	_ = s.store.UpdateScheduledEventStatus(ctx, evt.ID, status, &firedAt, errMsg)

	// Update schedule run state
	_ = s.store.UpdateScheduleAfterRun(ctx, sched.ID, now, nextRunAt, errMsg)
}

// StartBackgroundServices initializes and starts the scheduler and notification
// dispatcher. It is called by Start() for standalone mode and must be called
// explicitly in combined mode (Hub mounted on WebServer) since Start() is
// not invoked in that case.
func (s *Server) StartBackgroundServices(ctx context.Context) {
	s.mu.Lock()
	if s.startTime.IsZero() {
		s.startTime = time.Now()
	}
	s.mu.Unlock()

	// Initialize and start the scheduler. Interval and concurrency are
	// configurable via server.scheduler in settings.yaml to let operators
	// tune background load to match their DB capacity (see issue #367).
	var schedOpts []SchedulerOption
	if s.config.SchedulerIntervalSeconds > 0 {
		schedOpts = append(schedOpts, WithTickInterval(time.Duration(s.config.SchedulerIntervalSeconds)*time.Second))
	}
	if s.config.SchedulerMaxConcurrency != nil {
		schedOpts = append(schedOpts, WithMaxConcurrency(*s.config.SchedulerMaxConcurrency))
	}
	s.scheduler = NewScheduler(s.store, logging.Subsystem("hub.scheduler"), schedOpts...)
	// Recurring sweeps are cluster-wide-once work: under multi-replica Postgres
	// they must run on a single replica per tick (gated by an advisory lock),
	// otherwise every replica would publish duplicate offline/stalled events and
	// race on the schedule claim. On SQLite the lock is a no-op. See
	// CONCURRENCY-AUDIT.md §"Singleton / leader".
	// Non-critical maintenance tasks run every 5 minutes (not every 1 minute) to
	// reduce DB connection pressure. Combined with per-handler jitter in the
	// scheduler, this eliminates the thundering-herd pattern that was causing
	// 9-54 s API latency spikes.
	s.scheduler.RegisterRecurringSingleton("agent-heartbeat-timeout", 5, store.LockAgentHeartbeatTimeout, s.agentHeartbeatTimeoutHandler())
	s.scheduler.RegisterRecurringSingleton("agent-stalled-detection", 5, store.LockAgentStalledDetection, s.agentStalledDetectionHandler())
	if s.config.SoftDeleteRetention > 0 {
		s.scheduler.RegisterRecurringSingleton("soft-delete-purge", 60, store.LockSoftDeletePurge, s.purgeHandler())
	}
	s.scheduler.RegisterEventHandler("message", s.messageEventHandler())
	s.scheduler.RegisterEventHandler("dispatch_agent", s.dispatchAgentEventHandler())
	s.scheduler.RegisterRecurringSingleton("schedule-evaluator", 1, store.LockScheduleEvaluator, s.evaluateSchedulesHandler())
	s.scheduler.RegisterRecurringSingleton("broker-affinity-reap", 5, store.LockBrokerAffinityReap, s.brokerAffinityReapHandler())
	s.scheduler.RegisterRecurringSingleton("broker-message-sweep", 5, store.LockBrokerMessageSweep, s.brokerMessageSweepHandler())
	s.scheduler.RegisterRecurringSingleton("exposed-ports-sweep", 5, store.LockExposedPortsSweep, s.exposedPortsSweepHandler())

	// A2A bridge sweep — conditional on the bridge being registered as a standalone plugin.
	if a2aExternalURL := s.getA2ABridgeExternalURL(); a2aExternalURL != "" {
		s.scheduler.RegisterRecurringSingleton(
			"a2a-bridge-sweep", 5, store.LockA2ABridgeSweep,
			s.a2aBridgeSweepHandler(a2aExternalURL),
		)
	}

	// Register GitHub resolution cache TTL eviction (every 10 minutes)
	if s.ghResolutionStore != nil {
		s.scheduler.RegisterRecurringSingleton("github-resolution-cache-eviction", 10, store.LockGitHubResolutionCacheEviction, s.githubResolutionCacheEvictionHandler())
	}

	// Register HMAC nonce cache TTL eviction (every 5 minutes)
	if s.nonceCacheStore != nil {
		s.scheduler.RegisterRecurringSingleton("nonce-cache-eviction", 5, store.LockNonceCacheEviction, s.nonceCacheEvictionHandler())
	}

	// Register chat link code TTL eviction (every 5 minutes)
	if s.chatLinkStore != nil {
		s.scheduler.RegisterRecurringSingleton("chat-link-code-eviction", 5, store.LockChatLinkCodeEviction, s.chatLinkCodeEvictionHandler())
	}

	// Register GitHub App health check if the app is configured
	s.mu.RLock()
	ghAppConfigured := s.config.GitHubAppConfig.AppID != 0
	ghWebhooksEnabled := s.config.GitHubAppConfig.WebhooksEnabled
	s.mu.RUnlock()
	if ghAppConfigured {
		interval := 360 // 6 hours in minutes when webhooks are disabled
		if ghWebhooksEnabled {
			interval = 1440 // 24 hours when webhooks are enabled
		}
		s.scheduler.RegisterRecurringSingleton("github-app-health-check", interval, store.LockGitHubAppHealthCheck, s.githubAppHealthCheckHandler())
	}

	s.scheduler.Start(ctx)

	// Start the DB connection-pool stats sampler (P3-6 -> P0-5 gauges). It is a
	// no-op unless an enabled recorder was wired via SetDBMetrics and the store
	// exposes its *sql.DB; this keeps connection-budget saturation observable
	// under multi-replica Postgres (see CONNECTION-BUDGET.md).
	if rec := s.dbMetrics; rec != nil {
		if dbp, ok := s.store.(interface{ DB() *sql.DB }); ok {
			s.stopPoolSampler = dbmetrics.StartPoolSampler(ctx, rec, dbp.DB(), 0)
		}
	}

	// Start rate limiter cleanup goroutines (exit when ctx is cancelled).
	if s.gcpTokenRateLimiter != nil {
		s.gcpTokenRateLimiter.StartCleanup(ctx)
	}
	if s.oidcTokenRateLimiter != nil {
		s.oidcTokenRateLimiter.StartCleanup(ctx)
	}

	// Start OIDC key cleanup loop to remove expired rotated keys from JWKS.
	if s.oidcKeyManager != nil {
		s.oidcKeyManager.StartCleanupLoop(ctx)
	}

	// Start notification dispatcher (uses the current event publisher).
	// The dispatcher is resolved lazily so it works even if SetDispatcher
	// is called after Start().
	s.StartNotificationDispatcher()

	// Start lifecycle hook evaluator (uses the current event publisher).
	// The evaluator detects postgres from the EventPublisher type for
	// backend-aware deduplication; callers may also pass WithDBDriver.
	s.StartLifecycleHookEvaluator()
}

func (s *Server) Start(ctx context.Context) error {
	s.mu.Lock()
	s.startTime = time.Now()

	handler := s.applyMiddleware(s.mux)

	s.httpServer = &http.Server{
		Addr:         fmt.Sprintf("%s:%d", s.config.Host, s.config.Port),
		Handler:      handler,
		ReadTimeout:  s.config.ReadTimeout,
		WriteTimeout: s.config.WriteTimeout,
	}
	s.mu.Unlock()

	s.StartBackgroundServices(ctx)

	slog.Info("Hub API server starting", "host", s.config.Host, "port", s.config.Port)

	errCh := make(chan error, 1)
	go func() {
		if err := s.httpServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			errCh <- err
		}
		close(errCh)
	}()

	select {
	case err := <-errCh:
		return err
	case <-ctx.Done():
		return s.Shutdown(context.Background())
	}
}

// Shutdown gracefully shuts down the server.
func (s *Server) Shutdown(ctx context.Context) error {
	s.mu.RLock()
	srv := s.httpServer
	cc := s.controlChannel
	s.mu.RUnlock()

	if srv == nil {
		return nil
	}

	slog.Info("Hub API server shutting down...")

	// Cancel server-lifetime context to stop background goroutines
	if s.ctxCancel != nil {
		s.ctxCancel()
	}

	// Shutdown control channel first
	if cc != nil {
		cc.Shutdown()
	}

	// Stop the nonce cache cleanup goroutine
	if s.brokerAuthService != nil {
		s.brokerAuthService.Close()
	}

	// Stop scheduler
	if s.scheduler != nil {
		s.scheduler.Stop()
	}

	// Stop the DB pool-stats sampler.
	if s.stopPoolSampler != nil {
		s.stopPoolSampler()
	}

	// Stop notification dispatcher before closing event publisher
	if s.notificationDispatcher != nil {
		s.notificationDispatcher.Stop()
	}

	// Stop lifecycle hook evaluator before closing event publisher
	if s.lifecycleHookEvaluator != nil {
		s.lifecycleHookEvaluator.Stop()
	}

	// Stop presence manager before closing event publisher
	if s.presenceManager != nil {
		s.presenceManager.Stop()
	}

	// Close event publisher
	if s.events != nil {
		s.events.Close()
	}
	if s.commandBus != nil {
		s.commandBus.Close()
	}

	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	return srv.Shutdown(ctx)
}

// CleanupResources shuts down Hub-owned resources (control channel, broker auth,
// event publisher) without stopping an HTTP server. Use this in combined mode
// where the Hub API is mounted on the WebServer and has no listener of its own.
func (s *Server) CleanupResources(ctx context.Context) error {
	s.cleanupOnce.Do(func() {
		s.mu.RLock()
		cc := s.controlChannel
		s.mu.RUnlock()

		slog.Info("Cleaning up Hub resources...")

		// Cancel server-lifetime context to stop background goroutines
		if s.ctxCancel != nil {
			s.ctxCancel()
		}

		if cc != nil {
			cc.Shutdown()
		}
		if s.brokerAuthService != nil {
			s.brokerAuthService.Close()
		}
		if s.scheduler != nil {
			s.scheduler.Stop()
		}
		if s.notificationDispatcher != nil {
			s.notificationDispatcher.Stop()
		}
		if s.lifecycleHookEvaluator != nil {
			s.lifecycleHookEvaluator.Stop()
		}
		if s.messageBrokerProxy != nil {
			s.messageBrokerProxy.Stop()
		}
		if s.telegramLinkService != nil {
			s.telegramLinkService.Close()
		}
		if s.discordLinkService != nil {
			s.discordLinkService.Close()
		}
		if s.teamsLinkService != nil {
			s.teamsLinkService.Close()
		}
		// Stop presence manager before closing event publisher
		if s.presenceManager != nil {
			s.presenceManager.Stop()
		}
		if s.events != nil {
			s.events.Close()
		}
		if s.commandBus != nil {
			s.commandBus.Close()
		}
		if s.logQueryService != nil {
			_ = s.logQueryService.Close()
		}
		if s.metricsDashboard != nil {
			if err := s.metricsDashboard.Close(); err != nil {
				slog.Warn("Failed to close metrics dashboard", "error", err)
			}
		}
	})
	return nil
}

// Handler returns the HTTP handler for the server.
// This is useful for testing without starting a listener.
func (s *Server) Handler() http.Handler {
	return s.applyMiddleware(s.mux)
}

// registerRoutes sets up all API routes.
func (s *Server) registerRoutes() {
	// Health and metrics endpoints
	s.mux.HandleFunc("/healthz", s.handleHealthz)
	s.mux.HandleFunc("/readyz", s.handleReadyz)
	s.mux.HandleFunc("/metrics", s.handleMetrics)

	// Authentication endpoints (these routes are handled specially in middleware)
	s.mux.HandleFunc("/api/v1/auth/login", s.handleAuthLogin)
	s.mux.HandleFunc("/api/v1/auth/token", s.handleAuthToken)
	s.mux.HandleFunc("/api/v1/auth/refresh", s.handleAuthRefresh)
	s.mux.HandleFunc("/api/v1/auth/validate", s.handleAuthValidate)
	s.mux.HandleFunc("/api/v1/auth/logout", s.handleAuthLogout)
	s.mux.HandleFunc("/api/v1/auth/me", s.handleAuthMe)
	s.mux.HandleFunc("/api/v1/auth/tokens", s.handleTokens)
	s.mux.HandleFunc("/api/v1/auth/tokens/", s.handleTokenByID)
	s.mux.HandleFunc("/api/v1/auth/providers", s.handleCLIAuthProviders)

	// CLI OAuth endpoints (unauthenticated - used for login)
	s.mux.HandleFunc("/api/v1/auth/invite/redeem", s.handleInviteRedeem)
	s.mux.HandleFunc("/api/v1/auth/cli/authorize", s.handleCLIAuthAuthorize)
	s.mux.HandleFunc("/api/v1/auth/cli/token", s.handleCLIAuthToken)
	s.mux.HandleFunc("/api/v1/auth/cli/device", s.handleCLIDeviceAuthorize)
	s.mux.HandleFunc("/api/v1/auth/cli/device/token", s.handleCLIDeviceToken)

	// API v1 routes
	s.mux.HandleFunc("/api/v1/agents", s.handleAgents)
	s.mux.HandleFunc("/api/v1/agents/", s.handleAgentByID)

	s.mux.HandleFunc("/api/v1/projects", s.handleProjects)
	s.mux.HandleFunc("/api/v1/projects/register", s.handleProjectRegister)
	// Project-nested routes: /api/v1/projects/{projectId}/agents, /api/v1/projects/{projectId}/env, etc.
	// This handler must come before the generic project-by-id handler
	s.mux.HandleFunc("/api/v1/projects/", s.handleProjectRoutes)

	// Legacy /api/v1/groves aliases are external compatibility adapters for
	// the canonical /api/v1/projects handlers.
	s.mux.HandleFunc("/api/v1/groves", s.handleLegacyGroveRoute(s.handleProjects))
	s.mux.HandleFunc("/api/v1/groves/register", s.handleLegacyGroveRoute(s.handleProjectRegister))
	s.mux.HandleFunc("/api/v1/groves/", s.handleLegacyGroveRoute(s.handleProjectRoutes))

	s.mux.HandleFunc("/api/v1/runtime-brokers", s.handleRuntimeBrokers)
	s.mux.HandleFunc("/api/v1/runtime-brokers/", s.handleRuntimeBrokerRoutes)

	s.mux.HandleFunc("/api/v1/templates", s.handleTemplatesV2)
	s.mux.HandleFunc("/api/v1/templates/", s.handleTemplateByIDV2)

	// Scope-addressed service accounts. The project-nested routes under
	// /api/v1/projects/{id}/gcp-service-accounts remain registered and
	// unchanged; these routes exist for scopes that have no project to nest
	// under.
	//
	// The by-id subtree serves PARENTLESS accounts only -- hub and user scope.
	// A project-scoped account is 404 there, so this is not a second address
	// for accounts that already have one; it is the only address for accounts
	// that have none. P4 registered the collection alone because it only
	// needed to list; P5 needs to view, re-verify and delete a hub-scoped
	// account from a UI and a CLI that are not inside any project.
	s.mux.HandleFunc("/api/v1/gcp-service-accounts", s.handleGCPServiceAccounts)
	s.mux.HandleFunc("/api/v1/gcp-service-accounts/", s.handleGCPServiceAccountByID)

	s.mux.HandleFunc("/api/v1/skills", s.handleSkills)
	s.mux.HandleFunc("/api/v1/skills/", s.handleSkillByID)

	s.mux.HandleFunc("/api/v1/skill-registries", s.handleSkillRegistries)
	s.mux.HandleFunc("/api/v1/skill-registries/", s.handleSkillRegistryByID)

	s.mux.HandleFunc("/api/v1/harness-configs", s.handleHarnessConfigs)
	s.mux.HandleFunc("/api/v1/harness-configs/", s.handleHarnessConfigByID)

	// Hub-scoped pre-start hooks. The project-scoped equivalents live under
	// /api/v1/projects/{projectId}/pre-start-hooks (see handleProjectRoutes).
	s.mux.HandleFunc("/api/v1/pre-start-hooks", s.handleHubPreStartHooks)
	s.mux.HandleFunc("/api/v1/pre-start-hooks/", s.handleHubPreStartHookByID)

	s.mux.HandleFunc("/api/v1/users", s.handleUsers)
	s.mux.HandleFunc("/api/v1/users/", s.handleUserByID)

	// Environment variables and secrets (generic endpoints)
	s.mux.HandleFunc("/api/v1/env", s.handleEnvVars)
	s.mux.HandleFunc("/api/v1/env/", s.handleEnvVarByKey)
	s.mux.HandleFunc("/api/v1/secrets", s.handleSecrets)
	s.mux.HandleFunc("/api/v1/secrets/", s.handleSecretByKey)

	// Session metrics (DB-backed, distinct from Cloud Monitoring metrics dashboard)
	s.mux.HandleFunc("/api/v1/metrics/session/", s.handleSessionMetrics)

	// Groups and Policies (Hub Permissions System)
	s.mux.HandleFunc("/api/v1/groups", s.handleGroups)
	s.mux.HandleFunc("/api/v1/groups/", s.handleGroupRoutes)
	s.mux.HandleFunc("/api/v1/policies", s.handlePolicies)
	s.mux.HandleFunc("/api/v1/policies/", s.handlePolicyRoutes)

	// Principal resolution endpoints (Phase 4)
	s.mux.HandleFunc("/api/v1/users/me/groups", s.handleMyGroups)
	s.mux.HandleFunc("/api/v1/principals/", s.handlePrincipalRoutes)

	// User-scoped injected-skills endpoints (/users/me/...)
	s.mux.HandleFunc("/api/v1/users/me/injected-skills", s.handleUserMeInjectedSkills)
	s.mux.HandleFunc("/api/v1/users/me/injected-skills/", func(w http.ResponseWriter, r *http.Request) {
		entryID := strings.TrimPrefix(r.URL.Path, "/api/v1/users/me/injected-skills/")
		entryID = strings.TrimSuffix(entryID, "/")
		s.handleUserMeInjectedSkillByID(w, r, entryID)
	})

	// Hub-scope injected-skills endpoint
	s.mux.HandleFunc("/api/v1/hub/settings/injected-skills", s.handleHubInjectedSkills)

	// Broker registration endpoints (Runtime Broker HMAC authentication)
	s.mux.HandleFunc("/api/v1/brokers", s.handleBrokersEndpoint)
	s.mux.HandleFunc("/api/v1/brokers/join", s.handleBrokerJoin)
	s.mux.HandleFunc("/api/v1/brokers/", s.handleBrokerByIDRoutes)

	// Broker plugin inbound message delivery
	s.mux.HandleFunc("/api/v1/broker/inbound", s.handleBrokerInbound)

	// Broker plugin project listing (fresh list for /setup flows)
	s.mux.HandleFunc("/api/v1/broker/projects", s.handleBrokerProjects)

	// Admin system endpoints
	s.mux.HandleFunc("/api/v1/admin/maintenance", s.handleAdminMaintenance)
	s.mux.HandleFunc("/api/v1/admin/maintenance/operations", s.handleAdminMaintenanceOps)
	s.mux.HandleFunc("/api/v1/admin/maintenance/operations/", s.handleAdminMaintenanceOps)
	s.mux.HandleFunc("/api/v1/admin/maintenance/migrations/", s.handleAdminMaintenanceMigrations)
	s.mux.HandleFunc("/api/v1/admin/maintenance/check-updates", s.handleCheckForUpdates)
	s.mux.HandleFunc("/api/v1/admin/maintenance/restart", s.handleAdminRestart)
	s.mux.HandleFunc("/api/v1/admin/scheduler", s.handleAdminScheduler)
	s.mux.HandleFunc("/api/v1/admin/allow-list", s.handleAdminAllowList)
	s.mux.HandleFunc("/api/v1/admin/allow-list/", s.handleAdminAllowListByEmail)
	s.mux.HandleFunc("/api/v1/admin/users/invite/bulk", s.handleAdminUserInviteBulk)
	s.mux.HandleFunc("/api/v1/admin/users/invite", s.handleAdminUserInvite)
	s.mux.HandleFunc("/api/v1/admin/invites", s.handleAdminInvites)
	s.mux.HandleFunc("/api/v1/admin/invites/", s.handleAdminInviteByID)
	s.mux.HandleFunc("/api/v1/admin/server-config/schema", s.handleAdminServerConfigSchema)
	s.mux.HandleFunc("/api/v1/admin/server-config/sections/", s.handleAdminServerConfigSectionReset)
	s.mux.HandleFunc("/api/v1/admin/server-config", s.handleAdminServerConfig)
	s.mux.HandleFunc("/api/v1/admin/project-defaults", s.handleAdminProjectDefaults)
	s.mux.HandleFunc("/api/v1/admin/agents/reset-auth-all", s.handleAdminResetAuthAll)
	s.mux.HandleFunc("/api/v1/admin/gcp-quota", s.handleAdminGCPQuota)
	s.mux.HandleFunc("/api/v1/admin/lifecycle-hooks", s.handleAdminLifecycleHooks)
	s.mux.HandleFunc("/api/v1/admin/lifecycle-hooks/", s.handleAdminLifecycleHookByID)
	s.mux.HandleFunc("/api/v1/admin/validate-resources", s.handleAdminValidateResources)
	s.mux.HandleFunc("/api/v1/admin/integrations", s.handleAdminIntegrations)
	s.mux.HandleFunc("/api/v1/admin/integrations/teams/manifest", s.handleTeamsManifestDownload)
	s.mux.HandleFunc("/api/v1/admin/integrations/", s.handleAdminIntegrationByName)
	s.mux.HandleFunc("/api/v1/admin/diagnostics/logs/stream", s.handleDiagnosticsLogsStream)
	s.mux.HandleFunc("/api/v1/admin/diagnostics/logs", s.handleDiagnosticsLogs)
	s.mux.HandleFunc("/api/v1/admin/health/summary", s.handleHealthSummary)
	s.mux.HandleFunc("/api/v1/metrics/", s.handleMetricsDashboard)
	s.mux.HandleFunc("/api/v1/admin/metrics-dashboard", s.handleAdminMetricsDashboard) // legacy backward-compat

	// Notification endpoints (user-facing)
	s.mux.HandleFunc("/api/v1/notifications", s.handleNotifications)
	s.mux.HandleFunc("/api/v1/notifications/", s.handleNotificationRoutes)

	// Message inbox endpoints (user-facing)
	s.mux.HandleFunc("/api/v1/messages", s.handleMessages)
	s.mux.HandleFunc("/api/v1/messages/", s.handleMessageRoutes)
	s.mux.HandleFunc("/api/v1/message-channels", s.handleMessageChannels)

	// Native chat endpoints. Registration is gated on server.native_chat.enabled,
	// so disabling the feature makes every /api/v1/chat/* path 404 rather than
	// leaving a live API behind a hidden UI. This is a startup-time gate:
	// flipping the toggle requires a hub restart (same as the message broker).
	if s.nativeChatEnabled() {
		// Chat thread prefs (Phase 3 — visibility mode persistence)
		s.mux.HandleFunc("/api/v1/chat/prefs", s.handleChatPrefs)

		// Chat thread endpoints (Phase 5 — thread rail, legacy)
		s.mux.HandleFunc("/api/v1/chat/threads", s.handleChatThreads)
		s.mux.HandleFunc("/api/v1/chat/threads/", s.handleChatThreadRoutes)

		// Wave-2 chat endpoints (conversation REST API)
		s.mux.HandleFunc("/api/v1/chat/spaces", s.handleChatSpaces)
		s.mux.HandleFunc("/api/v1/chat/spaces/", s.handleChatSpaceRoutes)
		s.mux.HandleFunc("/api/v1/chat/conversations/", s.handleChatConversationRoutes)
		s.mux.HandleFunc("/api/v1/chat/topics/", s.handleChatTopicRoutes)
		s.mux.HandleFunc("/api/v1/chat/dms", s.handleChatDMs)
		s.mux.HandleFunc("/api/v1/chat/user-prefs", s.handleChatUserPrefs)
		s.mux.HandleFunc("/api/v1/chat/presence", s.handleChatPresence)
		s.mux.HandleFunc("/api/v1/chat/search", s.handleChatSearch)
		s.mux.HandleFunc("/api/v1/chat/attachments", s.handleChatAttachments)
		s.mux.HandleFunc("/api/v1/chat/attachments/", s.handleChatAttachmentByID)
	}

	// WebSocket control channel endpoint for Runtime Brokers
	s.mux.HandleFunc("/api/v1/runtime-brokers/connect", s.handleRuntimeBrokerConnect)

	// GCP identity endpoints (agent token auth)
	s.mux.HandleFunc("/api/v1/agent/gcp-token", s.handleAgentGCPToken)
	s.mux.HandleFunc("/api/v1/agent/gcp-identity-token", s.handleAgentGCPIdentityToken)

	// Public settings endpoint (no auth required for telemetry default, etc.)
	s.mux.HandleFunc("/api/v1/settings/public", s.handlePublicSettings)

	// GitHub App integration endpoints
	s.mux.HandleFunc("/api/v1/github-app", s.handleGitHubApp)
	s.mux.HandleFunc("/api/v1/github-app/installations", s.handleGitHubAppInstallations)
	s.mux.HandleFunc("/api/v1/github-app/installations/", s.handleGitHubAppInstallations)
	s.mux.HandleFunc("/api/v1/github-app/installations/discover", s.handleGitHubAppDiscover)
	s.mux.HandleFunc("/api/v1/github-app/sync-permissions", s.handleGitHubAppSyncPermissions)

	// Telegram account linking endpoints
	s.mux.HandleFunc("/api/v1/telegram/link", s.handleTelegramLink)
	s.mux.HandleFunc("/api/v1/telegram/link/verify", s.handleTelegramLinkVerify)
	s.mux.HandleFunc("/api/v1/telegram/link/status", s.handleTelegramLinkStatus)

	// Discord account linking endpoints
	s.mux.HandleFunc("/api/v1/discord/link", s.handleDiscordLink)
	s.mux.HandleFunc("/api/v1/discord/link/verify", s.handleDiscordLinkVerify)
	s.mux.HandleFunc("/api/v1/discord/link/status", s.handleDiscordLinkStatus)

	// Teams account linking endpoints
	s.mux.HandleFunc("/api/v1/teams/link", s.handleTeamsLink)
	s.mux.HandleFunc("/api/v1/teams/link/verify", s.handleTeamsLinkVerify)
	s.mux.HandleFunc("/api/v1/teams/link/status", s.handleTeamsLinkStatus)

	// Unified resource import endpoint (templates + harness-configs, global + project)
	s.mux.HandleFunc("/api/v1/resources/import", s.handleResourcesImport)
	s.mux.HandleFunc("/api/v1/resources/discover", s.handleResourcesDiscover)

	// Skill directory discovery (batch-add support for injected skills). Registered
	// as an exact path so it takes precedence over the /api/v1/skills/ subtree
	// handler above.
	s.mux.HandleFunc("/api/v1/skills/discover-directory", s.handleSkillsDiscoverDirectory)

	// GitHub App webhook and setup callback (unauthenticated — uses webhook signature)
	s.mux.HandleFunc("/api/v1/webhooks/github", s.handleGitHubWebhook)
	s.mux.HandleFunc("/github-app/setup", s.handleGitHubAppSetup)

	// Workstation-only system endpoints
	s.mux.Handle("/api/v1/system/identity", s.requireWorkstation(http.HandlerFunc(s.handleSystemIdentity)))
	s.mux.Handle("/api/v1/system/status", s.requireWorkstation(http.HandlerFunc(s.handleSystemStatus)))
	s.mux.Handle("/api/v1/system/check", s.requireWorkstation(http.HandlerFunc(s.handleSystemCheck)))
	s.mux.Handle("/api/v1/system/runtime", s.requireWorkstation(http.HandlerFunc(s.handleSystemRuntime)))
	s.mux.Handle("/api/v1/system/init", s.requireWorkstation(http.HandlerFunc(s.handleSystemInit)))
	s.mux.Handle("/api/v1/system/images/pull", s.requireWorkstation(http.HandlerFunc(s.handleSystemImagesPull)))
	s.mux.Handle("/api/v1/system/images/build", s.requireWorkstation(http.HandlerFunc(s.handleSystemImagesBuild)))
	s.mux.Handle("/api/v1/system/apple-dns", s.requireWorkstation(http.HandlerFunc(s.handleAppleDNS)))
	s.mux.Handle("/api/v1/system/registry", s.requireWorkstation(http.HandlerFunc(s.handleSystemRegistry)))
	s.mux.Handle("/api/v1/system/workstation-settings", s.requireWorkstation(http.HandlerFunc(s.handleWorkstationSettings)))

	// OIDC Identity Provider endpoints (unauthenticated — public metadata)
	if s.oidcKeyManager != nil {
		s.mux.HandleFunc("GET /.well-known/openid-configuration", s.handleOIDCDiscovery)
		s.mux.HandleFunc("GET /.well-known/jwks.json", s.handleJWKS)
		s.mux.HandleFunc("POST /api/v1/agent/identity-token", s.handleAgentIdentityToken)
	}

	// Workstation-only filesystem endpoints
	s.mux.Handle("/api/v1/system/fs/list", s.requireWorkstation(http.HandlerFunc(s.handleFSList)))
	s.mux.Handle("/api/v1/system/fs/mkdir", s.requireWorkstation(http.HandlerFunc(s.handleFSMkdir)))
	s.mux.Handle("/api/v1/system/fs/validate-path", s.requireWorkstation(http.HandlerFunc(s.handleFSValidatePath)))
}

// applyMiddleware wraps the handler with middleware.
func (s *Server) applyMiddleware(h http.Handler) http.Handler {
	// Apply middleware in reverse order (last applied runs first)
	h = s.recoveryMiddleware(h)
	if s.requestLogger != nil {
		h = logging.RequestLogMiddleware(s.requestLogger, "hub", logging.HubPathPatterns(), s.config.SlowRequestThreshold)(h)
	} else {
		h = s.loggingMiddleware(h)
	}

	// Apply broker auth middleware (checks X-Scion-Broker-ID header for HMAC auth)
	// This runs after unified auth but before the handler, allowing hosts to authenticate
	if s.brokerAuthService != nil {
		if s.auditLogger != nil {
			h = AuditableBrokerAuthMiddleware(s.brokerAuthService, s.auditLogger)(h)
		} else {
			h = BrokerAuthMiddleware(s.brokerAuthService)(h)
		}
	}

	// Record user last-seen activity (after auth, so identity is available).
	if s.userActivity != nil {
		h = userActivityMiddleware(s.userActivity)(h)
	}

	// Apply admin mode middleware (after auth, so identity is available).
	// Always applied — checks runtime MaintenanceState on each request.
	h = adminModeMiddleware(s.maintenance)(h)

	// Apply unified auth middleware
	// This handles all authentication types: agent tokens, user tokens, API keys, dev tokens
	h = UnifiedAuthMiddleware(s.authConfig)(h)

	if s.config.CORSEnabled {
		h = s.corsMiddleware(h)
	}

	// OTel HTTP tracing (outermost - wraps all middleware for full request lifecycle)
	h = otelhttp.NewHandler(h, "hub")

	return h
}

// corsMiddleware adds CORS headers.
func (s *Server) corsMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		origin := r.Header.Get("Origin")
		if origin == "" {
			next.ServeHTTP(w, r)
			return
		}

		// Check if origin is allowed
		allowed := false
		for _, o := range s.config.CORSAllowedOrigins {
			if o == "*" || o == origin {
				allowed = true
				break
			}
		}

		if allowed {
			w.Header().Set("Access-Control-Allow-Origin", origin)
			w.Header().Set("Access-Control-Allow-Methods", strings.Join(s.config.CORSAllowedMethods, ", "))
			w.Header().Set("Access-Control-Allow-Headers", strings.Join(s.config.CORSAllowedHeaders, ", "))
			w.Header().Set("Access-Control-Max-Age", fmt.Sprintf("%d", s.config.CORSMaxAge))
		}

		// Handle preflight
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}

		next.ServeHTTP(w, r)
	})
}

// requireWorkstation returns middleware that gates endpoints behind workstation mode.
// Returns 404 when the server is not running in workstation mode.
func (s *Server) requireWorkstation(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !s.workstation {
			http.NotFound(w, r)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// assertLoopback checks that the request originates from a loopback address.
func assertLoopback(r *http.Request) error {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	ip := net.ParseIP(host)
	if ip == nil || !ip.IsLoopback() {
		return fmt.Errorf("non-loopback request from %s", r.RemoteAddr)
	}
	return nil
}

// loggingMiddleware logs requests.
func (s *Server) loggingMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		wrapped := &responseWriter{ResponseWriter: w, statusCode: http.StatusOK}

		// Extract contextual metadata for logging.
		traceID := logging.ExtractTraceIDFromHeaders(r)

		attrs := []slog.Attr{
			slog.String("method", r.Method),
			slog.String("path", r.URL.Path),
			slog.String("remote_addr", r.RemoteAddr),
		}
		if traceID != "" {
			attrs = append(attrs, slog.String(logging.AttrTraceID, traceID))
		}

		if s.config.Debug {
			slog.Debug("Incoming request",
				slog.String("method", r.Method),
				slog.String("path", r.URL.Path),
				slog.String("remote_addr", r.RemoteAddr),
				slog.String("query", r.URL.RawQuery),
			)
		}

		next.ServeHTTP(wrapped, r)

		duration := time.Since(start)
		level := slog.LevelInfo
		if wrapped.statusCode >= 500 {
			level = slog.LevelError
		} else if wrapped.statusCode >= 400 {
			level = slog.LevelWarn
		}

		// Log slow requests, exempting streaming responses.
		contentType := strings.ToLower(wrapped.Header().Get("Content-Type"))
		isStreaming := strings.HasPrefix(contentType, "text/event-stream")
		isUpgrade := r.Header.Get("Upgrade") != ""
		slowThreshold := s.config.SlowRequestThreshold
		if slowThreshold <= 0 {
			slowThreshold = logging.DefaultSlowRequestThreshold
		}
		if !isStreaming && !isUpgrade && duration > slowThreshold {
			slog.Info("Slow request",
				slog.String("method", r.Method),
				slog.String("path", r.URL.Path),
				slog.Duration("elapsed", duration),
				slog.Int("status", wrapped.statusCode),
			)
		}

		slog.LogAttrs(r.Context(), level, "Request completed",
			append(attrs,
				slog.Int("status", wrapped.statusCode),
				slog.Duration("duration", duration),
			)...,
		)
	})
}

// recoveryMiddleware recovers from panics.
func (s *Server) recoveryMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if err := recover(); err != nil {
				slog.Error("Panic recovered",
					slog.Any("error", err),
					slog.String("path", r.URL.Path),
				)
				InternalError(w)
			}
		}()
		next.ServeHTTP(w, r)
	})
}

// responseWriter wraps http.ResponseWriter to capture status code.
// It implements http.Hijacker to support WebSocket upgrades.
type responseWriter struct {
	http.ResponseWriter
	statusCode int
}

func (rw *responseWriter) WriteHeader(code int) {
	rw.statusCode = code
	rw.ResponseWriter.WriteHeader(code)
}

// Hijack implements http.Hijacker for WebSocket support.
func (rw *responseWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	if hj, ok := rw.ResponseWriter.(http.Hijacker); ok {
		return hj.Hijack()
	}
	return nil, nil, fmt.Errorf("hijack not supported")
}

// Flush implements http.Flusher for streaming support.
func (rw *responseWriter) Flush() {
	if f, ok := rw.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// logOAuthProviders logs which OAuth providers are configured for a client type.
func logOAuthProviders(clientType string, cfg OAuthClientConfig) {
	googleConfigured := cfg.Google.ClientID != "" && cfg.Google.ClientSecret != ""
	githubConfigured := cfg.GitHub.ClientID != "" && cfg.GitHub.ClientSecret != ""

	if googleConfigured || githubConfigured {
		var providers []string
		if googleConfigured {
			providers = append(providers, "Google")
		}
		if githubConfigured {
			providers = append(providers, "GitHub")
		}
		slog.Info("OAuth providers configured", "client", clientType, "providers", providers)
	} else {
		slog.Info("No OAuth providers configured", "client", clientType)
	}
}

// Helper functions

// writeJSON writes a JSON response.
func writeJSON(w http.ResponseWriter, statusCode int, data interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(statusCode)
	_ = json.NewEncoder(w).Encode(data)
}

// readJSON reads JSON from request body.
func readJSON(r *http.Request, v interface{}) error {
	if r.Body == nil {
		return fmt.Errorf("empty request body")
	}
	return json.NewDecoder(r.Body).Decode(v)
}

// extractID extracts the ID from a URL path like "/api/v1/agents/{id}".
func extractID(r *http.Request, prefix string) string {
	path := strings.TrimPrefix(r.URL.Path, prefix)
	path = strings.TrimPrefix(path, "/")
	// Remove any trailing path segments
	if idx := strings.Index(path, "/"); idx != -1 {
		path = path[:idx]
	}
	return path
}

// extractAction extracts the action from a URL path like "/api/v1/agents/{id}/start".
func extractAction(r *http.Request, prefix string) (id, action string) {
	path := strings.TrimPrefix(r.URL.Path, prefix)
	path = strings.TrimPrefix(path, "/")
	parts := strings.SplitN(path, "/", 2)
	if len(parts) == 0 {
		return "", ""
	}
	id = parts[0]
	if len(parts) > 1 {
		action = parts[1]
	}
	return
}

// handleRuntimeBrokerConnect handles WebSocket upgrade for Runtime Broker control channel.
func (s *Server) handleRuntimeBrokerConnect(w http.ResponseWriter, r *http.Request) {
	// Verify this is a WebSocket upgrade request
	if !isWebSocketUpgrade(r) {
		writeError(w, 400, ErrCodeInvalidRequest, "WebSocket upgrade required", nil)
		return
	}

	// Get broker identity from context (set by BrokerAuthMiddleware)
	broker := GetBrokerIdentityFromContext(r.Context())
	if broker == nil {
		// Try to get broker ID from header if not authenticated yet
		brokerID := r.Header.Get("X-Scion-Broker-ID")
		if brokerID == "" {
			writeError(w, 401, ErrCodeUnauthorized, "Broker authentication required", nil)
			return
		}

		// Validate broker exists and is authorized
		if s.brokerAuthService == nil {
			writeError(w, 401, ErrCodeUnauthorized, "Broker authentication not enabled", nil)
			return
		}

		// For WebSocket, we need to verify HMAC on the upgrade request
		_, err := s.brokerAuthService.ValidateBrokerSignature(r.Context(), r)
		if err != nil {
			slog.Error("HMAC validation failed for broker", "brokerID", brokerID, "error", err)
			writeError(w, 401, ErrCodeBrokerAuthFailed, "Invalid broker signature", nil)
			return
		}

		// Use the broker ID from header
		sessionID, err := s.controlChannel.HandleUpgrade(w, r, brokerID)
		if err != nil {
			slog.Error("Upgrade failed for broker", "brokerID", brokerID, "error", err)
			// Error already written by upgrader
			return
		}
		s.markBrokerOnline(brokerID, sessionID)
		return
	}

	// Use authenticated broker identity
	sessionID, err := s.controlChannel.HandleUpgrade(w, r, broker.ID())
	if err != nil {
		slog.Error("Upgrade failed for broker", "brokerID", broker.ID(), "error", err)
		// Error already written by upgrader
		return
	}
	s.markBrokerOnline(broker.ID(), sessionID)
}

// markBrokerOnline updates broker and provider statuses to online after a successful WebSocket connection.
// It claims broker affinity for this hub instance + the connection's sessionID,
// which also bumps status->online and refreshes the heartbeat in one CAS write.
func (s *Server) markBrokerOnline(brokerID, sessionID string) {
	ctx := context.Background()
	slog.Info("Broker connected, marking online", "brokerID", brokerID, "sessionID", sessionID, "instanceID", s.instanceID)

	if err := s.store.ClaimRuntimeBrokerConnection(ctx, brokerID, s.instanceID, sessionID); err != nil {
		slog.Error("Failed to claim broker connection", "brokerID", brokerID, "error", err)
	}

	providers, err := s.store.GetBrokerProjects(ctx, brokerID)
	if err != nil {
		slog.Error("Failed to get broker projects for status update", "brokerID", brokerID, "error", err)
		return
	}
	for _, provider := range providers {
		if err := s.store.UpdateProviderStatus(ctx, provider.ProjectID, brokerID, store.BrokerStatusOnline); err != nil {
			slog.Error("Failed to update provider status", "brokerID", brokerID, "project_id", provider.ProjectID, "error", err)
		}
	}

	// Publish broker connected event
	projectIDs := make([]string, len(providers))
	for i, p := range providers {
		projectIDs[i] = p.ProjectID
	}
	broker, err := s.store.GetRuntimeBroker(ctx, brokerID)
	var brokerName string
	if err == nil {
		brokerName = broker.Name
	}
	s.events.PublishBrokerConnected(ctx, brokerID, brokerName, projectIDs)

	// Durability backstop (design §5.3): the moment this node owns the socket,
	// drain any durable dispatch intent that accumulated while the broker was
	// offline or owned elsewhere. Async so it never blocks the connect path;
	// idempotent + CAS-gated so concurrent drains execute each item once.
	go s.reconcileBroker(context.Background(), brokerID)
}

// isWebSocketUpgrade checks if the request is a WebSocket upgrade request.
func isWebSocketUpgrade(r *http.Request) bool {
	return strings.ToLower(r.Header.Get("Upgrade")) == "websocket" &&
		strings.Contains(strings.ToLower(r.Header.Get("Connection")), "upgrade")
}

// githubResolutionCacheEvictionHandler returns a recurring handler function that
// purges expired GitHub skill resolution cache entries. This prevents the cache
// table from growing unbounded and keeps queries fast.
func (s *Server) githubResolutionCacheEvictionHandler() func(ctx context.Context) {
	return func(ctx context.Context) {
		ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
		defer cancel()

		if s.ghResolutionStore == nil {
			return
		}

		if err := s.ghResolutionStore.PurgeExpired(ctx); err != nil {
			slog.Error("Scheduler: github resolution cache eviction failed", "error", err)
		}
	}
}

// nonceCacheEvictionHandler returns a recurring handler function that purges
// expired HMAC nonce cache entries from the database. This prevents the nonce
// table from growing unbounded and reclaims storage for nonces whose TTL has
// passed.
func (s *Server) nonceCacheEvictionHandler() func(ctx context.Context) {
	return func(ctx context.Context) {
		ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
		defer cancel()

		if s.nonceCacheStore == nil {
			return
		}

		purged, err := s.nonceCacheStore.PurgeExpired(ctx)
		if err != nil {
			slog.Error("Scheduler: nonce cache eviction failed", "error", err)
			return
		}
		if purged > 0 {
			slog.Info("Scheduler: nonce cache eviction completed", "purged", purged)
		}
	}
}

// chatLinkCodeEvictionHandler returns a recurring handler function that
// purges expired chat link code entries. This prevents the chat_link_codes
// table from growing unbounded.
func (s *Server) chatLinkCodeEvictionHandler() func(ctx context.Context) {
	return func(ctx context.Context) {
		ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
		defer cancel()

		if s.chatLinkStore == nil {
			return
		}

		if err := s.chatLinkStore.PurgeExpired(ctx); err != nil {
			slog.Error("Scheduler: chat link code eviction failed", "error", err)
		}
	}
}

// seedGitHubResolutionCacheSettings seeds the github_resolution_cache section
// in hub_settings with default TTL values. This runs on every startup so operators
// can see and adjust the cache behavior via the settings UI.
// Idempotent: calling more than once produces the same result.
func (s *Server) seedGitHubResolutionCacheSettings(ctx context.Context) error {
	settingValue := map[string]interface{}{
		"branch_ref_ttl_minutes": 30,
		"sha_ref_ttl_hours":      24,
		"enabled":                true,
	}

	value, err := json.Marshal(settingValue)
	if err != nil {
		return fmt.Errorf("seedGitHubResolutionCacheSettings: marshal: %w", err)
	}

	// Check if the setting already exists to avoid spurious revision bumps
	_, getErr := s.store.GetHubSetting(ctx, "github_resolution_cache")
	if getErr == nil {
		// Setting exists; skip upsert to avoid revision churn
		slog.Debug("seedGitHubResolutionCacheSettings: setting already exists; skipping")
		return nil
	}
	if !errors.Is(getErr, store.ErrNotFound) {
		return fmt.Errorf("seedGitHubResolutionCacheSettings: read existing setting: %w", getErr)
	}

	// Setting doesn't exist; create it with expectedRevision=-1 (upsert pattern)
	if _, upsertErr := s.store.UpsertHubSetting(ctx, "github_resolution_cache", value, "seed", -1, "seeded"); upsertErr != nil {
		return fmt.Errorf("seedGitHubResolutionCacheSettings: upsert: %w", upsertErr)
	}

	slog.Info("Seeded github_resolution_cache settings into hub_settings")
	return nil
}

// getA2ABridgeExternalURL returns the external URL of the A2A bridge if it is
// registered as a standalone plugin, or "" if not configured. Used to
// conditionally register the Hub-driven sweep scheduler job.
func (s *Server) getA2ABridgeExternalURL() string {
	if s.pluginManager == nil {
		return ""
	}
	cfg := s.pluginManager.GetPluginConfig("broker", "a2a-bridge")
	return cfg["external_url"]
}

// a2aBridgeSweepHandler returns a recurring handler that POSTs to the bridge's
// /internal/sweep endpoint with a Hub-minted service JWT. The bridge validates
// the token and runs the sweep (reap stale tasks + purge old events).
func (s *Server) a2aBridgeSweepHandler(externalURL string) func(ctx context.Context) {
	return func(ctx context.Context) {
		if s.userTokenService == nil {
			slog.Warn("a2a-bridge-sweep: user token service not available, skipping")
			return
		}

		// Mint a short-lived JWT with a service claim. The bridge's
		// hubJWTAuthMiddleware pins on role=="service" — without this
		// claim, any valid user token could trigger sweeps.
		token, _, err := s.userTokenService.GenerateAccessToken(
			"hub-scheduler",        // userID — synthetic service identity
			"hub-scheduler@system", // email
			"Hub Scheduler",        // displayName
			"service",              // role — the pinned service claim
			ClientTypeAPI,          // clientType
		)
		if err != nil {
			slog.Error("a2a-bridge-sweep: failed to mint sweep token", "error", err)
			return
		}

		req, err := http.NewRequestWithContext(ctx, "POST", externalURL+"/internal/sweep", nil)
		if err != nil {
			slog.Error("a2a-bridge-sweep: failed to create request", "error", err)
			return
		}
		req.Header.Set("Authorization", "Bearer "+token)

		sweepClient := &http.Client{Timeout: 30 * time.Second}
		resp, err := sweepClient.Do(req)
		if err != nil {
			slog.Error("a2a-bridge-sweep: request failed", "error", err)
			return
		}
		defer func() { _ = resp.Body.Close() }()

		if resp.StatusCode != http.StatusOK {
			slog.Warn("a2a-bridge-sweep: non-200 response",
				"status", resp.StatusCode, "url", externalURL)
		}
	}
}
