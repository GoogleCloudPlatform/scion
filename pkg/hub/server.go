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
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/broker"
	"github.com/GoogleCloudPlatform/scion/pkg/hub/githubapp"
	"github.com/GoogleCloudPlatform/scion/pkg/messages"
	"github.com/GoogleCloudPlatform/scion/pkg/secret"
	"github.com/GoogleCloudPlatform/scion/pkg/storage"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/GoogleCloudPlatform/scion/pkg/util/logging"
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
	// BrokerAuthConfig holds configuration for Runtime Broker HMAC authentication.
	BrokerAuthConfig BrokerAuthConfig
	// HubEndpoint is the public endpoint URL for this Hub (used in broker join responses).
	HubEndpoint string
	// StalledThreshold is how long an agent can go without activity events
	// before being marked as stalled (default: 5 minutes). Only applies to
	// agents with a recent heartbeat (not already offline).
	StalledThreshold time.Duration
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
	// TelemetryConfig is the full hub-level telemetry config from settings.yaml.
	// Used to populate default telemetry config on new agents when no per-agent
	// or template-level telemetry config is set.
	TelemetryConfig *api.TelemetryConfig
	// MaxSubscriptionsPerUser is the maximum number of notification subscriptions
	// allowed per subscriber. Zero means unlimited (default).
	MaxSubscriptionsPerUser int
	// GitHubAppConfig holds the GitHub App configuration for agent git authentication.
	GitHubAppConfig GitHubAppServerConfig
	// HubID is the unique hub instance ID used for secret namespacing.
	// If empty, secrets are looked up/stored with an empty scope ID.
	HubID string
	// SecretBackend is the optional secret backend for signing key storage.
	// When set before New(), ensureSigningKey can load/persist keys through the
	// production secret backend (e.g., GCP Secret Manager) instead of relying
	// solely on the SQLite store.
	SecretBackend secret.SecretBackend
	// MaintenanceConfig holds configuration for routine maintenance operations.
	MaintenanceConfig MaintenanceConfig
	// GCPProjectID is the GCP project ID used for minting service accounts.
	// If empty, auto-detected from the metadata server when running on GCE/Cloud Run.
	GCPProjectID string
	// GCPMintCapPerGrove is the maximum number of minted service accounts allowed per grove.
	// Zero means unlimited (default).
	GCPMintCapPerGrove int
	// GCPMintCapGlobal is the maximum total number of minted service accounts across all groves.
	// Zero means unlimited (default).
	GCPMintCapGlobal int
}

// MaintenanceConfig holds configuration for routine maintenance operation executors.
type MaintenanceConfig struct {
	// ImageRegistry is the container image registry prefix (e.g., "ghcr.io/myorg").
	ImageRegistry string
	// ImageTag is the default image tag to pull (default: "latest").
	ImageTag string
	// Harnesses is the list of harness names whose images should be pulled (e.g., ["claude", "gemini"]).
	Harnesses []string
	// RuntimeBin overrides auto-detection of the container runtime binary (docker, podman).
	RuntimeBin string
	// RepoPath is the path to the scion source checkout for rebuild operations.
	RepoPath string
	// BinaryDest is the install path for the rebuilt binary (default: /usr/local/bin/scion).
	BinaryDest string
	// ServiceName is the systemd service name to restart (default: "scion-hub").
	ServiceName string
}

// GitHubAppServerConfig holds the GitHub App configuration for the Hub server.
type GitHubAppServerConfig struct {
	AppID           int64
	PrivateKeyPath  string
	PrivateKey      string
	WebhookSecret   string
	APIBaseURL      string
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
	DispatchAgentStart(ctx context.Context, agent *store.Agent, task string) error

	// DispatchAgentStop stops a running agent on the runtime broker.
	DispatchAgentStop(ctx context.Context, agent *store.Agent) error

	// DispatchAgentRestart restarts an agent on the runtime broker.
	DispatchAgentRestart(ctx context.Context, agent *store.Agent) error

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
	// grovePath is the local filesystem path to the grove on the broker.
	// groveSlug is the grove slug for hub-native groves (no local provider path).
	// resolvedEnv contains environment variables resolved from Hub storage (API keys, etc.).
	// harnessConfig is the harness config name to use for the agent (e.g. "claude", "gemini").
	// resolvedSecrets contains type-aware secrets (including file-type) for auth resolution.
	// sharedWorkspace indicates the grove uses a shared workspace mount
	// (hub-grove / git-workspace hybrid) so the broker must not create a
	// per-agent worktree on (re-)start.
	StartAgent(ctx context.Context, brokerID, brokerEndpoint, agentID, groveID, task, grovePath, groveSlug, harnessConfig string, resolvedEnv map[string]string, resolvedSecrets []ResolvedSecret, inlineConfig *api.ScionConfig, sharedDirs []api.SharedDir, sharedWorkspace bool) (*RemoteAgentResponse, error)

	// StopAgent stops an agent on a remote runtime broker.
	// brokerID is used for HMAC authentication lookup.
	// groveID scopes the lookup to a specific grove (required for uniqueness).
	StopAgent(ctx context.Context, brokerID, brokerEndpoint, agentID, groveID string) error

	// RestartAgent restarts an agent on a remote runtime broker.
	// brokerID is used for HMAC authentication lookup.
	// groveID scopes the lookup to a specific grove (required for uniqueness).
	// resolvedEnv carries fresh auth tokens and identity vars so the restarted
	// container retains Hub connectivity.
	RestartAgent(ctx context.Context, brokerID, brokerEndpoint, agentID, groveID string, resolvedEnv map[string]string) error

	// DeleteAgent deletes an agent from a remote runtime broker.
	// brokerID is used for HMAC authentication lookup.
	// groveID scopes the lookup to a specific grove (required for uniqueness).
	// softDelete and deletedAt are passed as query params for broker-side marking.
	DeleteAgent(ctx context.Context, brokerID, brokerEndpoint, agentID, groveID string, deleteFiles, removeBranch, softDelete bool, deletedAt time.Time) error

	// MessageAgent sends a message to an agent on a remote runtime broker.
	// brokerID is used for HMAC authentication lookup.
	// structuredMsg is optional; when non-nil it takes precedence over the plain message string.
	MessageAgent(ctx context.Context, brokerID, brokerEndpoint, agentID, groveID, message string, interrupt bool, structuredMsg *messages.StructuredMessage) error

	// CheckAgentPrompt checks if an agent has a non-empty prompt.md file.
	// brokerID is used for HMAC authentication lookup.
	// groveID scopes the lookup to a specific grove (required for uniqueness).
	CheckAgentPrompt(ctx context.Context, brokerID, brokerEndpoint, agentID, groveID string) (bool, error)

	// FinalizeEnv sends gathered env vars to a broker to complete agent creation
	// after an initial 202 env-gather response.
	FinalizeEnv(ctx context.Context, brokerID, brokerEndpoint, agentID string, env map[string]string) (*RemoteAgentResponse, error)

	// CreateAgentWithGather creates an agent and handles 202 env-gather responses.
	// Returns (response, nil, nil) on success, (nil, envReqs, nil) on 202, or (nil, nil, err) on error.
	CreateAgentWithGather(ctx context.Context, brokerID, brokerEndpoint string, req *RemoteCreateAgentRequest) (*RemoteAgentResponse, *RemoteEnvRequirementsResponse, error)

	// GetAgentLogs retrieves agent.log content from a remote runtime broker.
	// brokerID is used for HMAC authentication lookup.
	// groveID scopes the lookup to a specific grove (required for uniqueness).
	GetAgentLogs(ctx context.Context, brokerID, brokerEndpoint, agentID, groveID string, tail int) (string, error)

	// ExecAgent executes a command in an agent on a remote runtime broker.
	// brokerID is used for HMAC authentication lookup.
	// groveID scopes the lookup to a specific grove (required for uniqueness).
	// Returns the command output, exit code, and any error.
	ExecAgent(ctx context.Context, brokerID, brokerEndpoint, agentID, groveID string, command []string, timeout int) (string, int, error)

	// CleanupGrove asks a broker to remove its local hub-native grove directory.
	// brokerID is used for HMAC authentication lookup.
	// 404 responses are tolerated for idempotency.
	CleanupGrove(ctx context.Context, brokerID, brokerEndpoint, groveSlug string) error
}

// RemoteCreateAgentRequest is the request body for creating an agent on a remote runtime broker.
type RemoteCreateAgentRequest struct {
	RequestID   string             `json:"requestId,omitempty"`
	ID          string             `json:"id,omitempty"` // Hub UUID for status reporting
	Slug        string             `json:"slug"`         // URL-safe identifier for the agent
	Name        string             `json:"name"`
	GroveID     string             `json:"groveId"`
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
	// Attach indicates the agent should start in interactive attach mode (not detached).
	Attach bool `json:"attach,omitempty"`
	// ProvisionOnly indicates the agent should be provisioned (dirs, worktree, templates)
	// but not started. The container will not be launched.
	ProvisionOnly bool `json:"provisionOnly,omitempty"`
	// GrovePath is the local filesystem path to the grove on the target runtime broker.
	// This is looked up from the grove provider record for the target broker.
	GrovePath string `json:"grovePath,omitempty"`
	// WorkspaceStoragePath is the GCS storage path for bootstrapped workspaces.
	// When set, the broker downloads the workspace from GCS instead of using GrovePath.
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

	// GroveSlug is the grove slug for hub-native groves.
	// When set, the broker creates the workspace at ~/.scion/groves/<slug>/
	// instead of the default worktree-based path.
	GroveSlug string `json:"groveSlug,omitempty"`

	// InlineConfig carries the full ScionConfig provided via the Hub API's
	// config field. The broker applies this during agent provisioning,
	// enabling inline configuration without pre-existing templates.
	InlineConfig *api.ScionConfig `json:"inlineConfig,omitempty"`

	// SharedDirs contains grove-level shared directory declarations.
	// Resolved by the Hub from the grove record and passed to the broker
	// so it can provision host-side directories and inject volume mounts.
	SharedDirs []api.SharedDir `json:"sharedDirs,omitempty"`
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

	// GitClone specifies git clone parameters for git-anchored groves.
	// When set, the runtime broker skips workspace mounting and injects env vars
	// so sciontool can clone the repo inside the container.
	GitClone *api.GitCloneConfig `json:"gitClone,omitempty"`

	// SharedWorkspace indicates this agent should use a shared git clone
	// workspace (git-workspace hybrid mode). When true, the broker skips
	// worktree/clone creation and configures per-agent git credentials.
	SharedWorkspace bool `json:"sharedWorkspace,omitempty"`

	// GCPIdentity holds the GCP identity assignment for the agent.
	GCPIdentity *RemoteGCPIdentityConfig `json:"gcpIdentity,omitempty"`
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
	AgentID    string                   `json:"agentId"`
	Required   []string                 `json:"required"`
	HubHas     []string                 `json:"hubHas"`
	BrokerHas  []string                 `json:"brokerHas"`
	Needs      []string                 `json:"needs"`
	SecretInfo map[string]SecretKeyInfo `json:"secretInfo,omitempty"`
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
	oauthService           *OAuthService           // OAuth service for CLI authentication
	authConfig             AuthConfig              // Unified auth configuration
	brokerAuthService      *BrokerAuthService      // Broker HMAC authentication service
	auditLogger            AuditLogger             // Audit logger for security events
	metrics                MetricsRecorder         // Metrics recorder for broker auth
	controlChannel         *ControlChannelManager  // WebSocket control channel for runtime brokers
	authzService           *AuthzService           // Authorization service for policy evaluation
	events                 EventPublisher          // Event publisher for real-time SSE updates
	notificationDispatcher *NotificationDispatcher // Notification dispatcher for agent status events
	maintenance            *MaintenanceState       // Runtime maintenance mode state
	hubID                  string                  // Unique hub instance ID for secret namespacing
	embeddedBrokerID       string                  // Broker ID when running in hub+broker combo mode
	scheduler              *Scheduler              // Unified scheduler for recurring tasks
	cleanupOnce            sync.Once               // Ensures CleanupResources runs only once

	logQueryService *LogQueryService // Cloud Logging query service (nil = disabled)

	// Channel registry for external notification delivery (nil = disabled)
	channelRegistry *ChannelRegistry

	// WorkflowRunDispatcher manages broker dispatch for workflow runs (nil = disabled)
	workflowDispatcher *WorkflowRunDispatcher

	// GCP token generator for agent identity (nil = GCP identity disabled)
	gcpTokenGenerator GCPTokenGenerator

	// GCP IAM admin for minting service accounts (nil = minting disabled)
	gcpIAMAdmin GCPServiceAccountAdmin

	// GCP token rate limiter (nil = no rate limiting)
	gcpTokenRateLimiter *GCPTokenRateLimiter

	// GCP token metrics tracker (nil = disabled)
	gcpTokenMetrics *GCPTokenMetrics

	// Message broker proxy for pub/sub message routing (nil = disabled)
	messageBrokerProxy *MessageBrokerProxy

	// User last-seen activity tracker (nil = disabled)
	userActivity *UserActivityTracker

	// Dedicated request logger (nil = disabled)
	requestLogger *slog.Logger

	// Dedicated message logger for message audit trail (nil = uses messageLog fallback)
	dedicatedMessageLog *slog.Logger

	// Subsystem loggers for handler methods
	agentLifecycleLog *slog.Logger
	messageLog        *slog.Logger
	authLog           *slog.Logger
	envSecretLog      *slog.Logger
	templateLog       *slog.Logger
	workspaceLog      *slog.Logger
	maintenanceLog    *slog.Logger

	// Cached rate limit info from the most recent GitHub App API call
	githubAppRateLimit *githubapp.RateLimitInfo
}

// New creates a new Hub API server.
func New(cfg ServerConfig, s store.Store) (*Server, error) {
	// Apply defaults for zero-value fields that have meaningful defaults.
	defaults := DefaultServerConfig()
	if cfg.StalledThreshold == 0 {
		cfg.StalledThreshold = defaults.StalledThreshold
	}

	srv := &Server{
		config:      cfg,
		store:       s,
		mux:         http.NewServeMux(),
		startTime:   time.Now(),
		events:      noopEventPublisher{},
		maintenance: NewMaintenanceState(cfg.AdminMode, cfg.MaintenanceMessage),
		hubID:       cfg.HubID,

		// Subsystem loggers
		agentLifecycleLog: logging.Subsystem("hub.agent-lifecycle"),
		messageLog:        logging.Subsystem("hub.messages"),
		authLog:           logging.Subsystem("hub.auth"),
		envSecretLog:      logging.Subsystem("hub.env-secrets"),
		templateLog:       logging.Subsystem("hub.templates"),
		workspaceLog:      logging.Subsystem("hub.workspace"),
		maintenanceLog:    logging.Subsystem("hub.maintenance"),
	}

	// Set secret backend from config so ensureSigningKey can use it.
	// This must happen before signing key initialization below.
	if cfg.SecretBackend != nil {
		srv.secretBackend = cfg.SecretBackend
	}

	// Initialize user activity tracker (throttled to once per hour per user)
	srv.userActivity = NewUserActivityTracker(s, time.Hour)

	// Initialize GCP token metrics
	srv.gcpTokenMetrics = NewGCPTokenMetrics()

	ctx := context.Background()

	_, isGCPBackend := srv.secretBackend.(*secret.GCPBackend)

	// Initialize agent token service
	agentKey, err := srv.ensureSigningKey(ctx, SecretKeyAgentSigningKey, cfg.AgentTokenConfig.SigningKey)
	if err != nil {
		if isGCPBackend {
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
		if isGCPBackend {
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

	// Initialize OAuth service if configured
	if cfg.OAuthConfig.IsConfigured() {
		srv.oauthService = NewOAuthService(cfg.OAuthConfig)
		slog.Info("OAuth service initialized")
		// Log which providers are configured
		logOAuthProviders("Web", cfg.OAuthConfig.Web)
		logOAuthProviders("CLI", cfg.OAuthConfig.CLI)
		logOAuthProviders("Device", cfg.OAuthConfig.Device)
	} else {
		slog.Info("OAuth service NOT configured - no providers available")
		slog.Info("To enable OAuth, set environment variables SCION_SERVER_OAUTH_CLI_GOOGLE_CLIENTID, etc.")
	}

	// Log authorized domains if configured
	if len(cfg.AuthorizedDomains) > 0 {
		slog.Info("Authorized domains", "domains", strings.Join(cfg.AuthorizedDomains, ", "))
	}

	// Initialize broker auth service if enabled
	if cfg.BrokerAuthConfig.Enabled {
		srv.brokerAuthService = NewBrokerAuthService(cfg.BrokerAuthConfig, s)
		srv.auditLogger = NewLogAuditLogger("[Hub Audit]", cfg.Debug)
		srv.metrics = NewBrokerAuthMetrics()
		slog.Info("Broker HMAC authentication enabled")
	}

	// Initialize control channel manager
	srv.controlChannel = NewControlChannelManager(ControlChannelConfig{
		PingInterval:   30 * time.Second,
		PongWait:       60 * time.Second,
		WriteWait:      10 * time.Second,
		MaxMessageSize: 64 * 1024,
		RequestTimeout: 120 * time.Second,
		Debug:          cfg.Debug,
	}, logging.Subsystem("hub.control-channel"))
	// Set disconnect callback to mark broker offline when WebSocket drops
	srv.controlChannel.SetOnDisconnect(func(brokerID string) {
		ctx := context.Background()
		slog.Info("Broker disconnected, marking offline", "brokerID", brokerID)

		if err := s.UpdateRuntimeBrokerHeartbeat(ctx, brokerID, store.BrokerStatusOffline); err != nil {
			slog.Error("Failed to mark broker offline", "brokerID", brokerID, "error", err)
		}

		// Update all grove provider records for this broker
		providers, err := s.GetBrokerGroves(ctx, brokerID)
		if err != nil {
			slog.Error("Failed to get broker groves for status update", "brokerID", brokerID, "error", err)
		} else {
			for _, provider := range providers {
				if err := s.UpdateProviderStatus(ctx, provider.GroveID, brokerID, store.BrokerStatusOffline); err != nil {
					slog.Error("Failed to update provider status", "brokerID", brokerID, "grove_id", provider.GroveID, "error", err)
				}
			}

			// Publish broker disconnected event
			groveIDs := make([]string, len(providers))
			for i, p := range providers {
				groveIDs[i] = p.GroveID
			}
			srv.events.PublishBrokerDisconnected(ctx, brokerID, groveIDs)
		}
	})
	slog.Info("Control channel manager initialized")

	// Initialize workflow run dispatcher
	srv.workflowDispatcher = NewWorkflowRunDispatcher(s, srv.controlChannel, logging.Subsystem("hub.workflow-dispatcher"))
	// Wire the dispatcher into the control channel manager so broker events
	// (workflow_status, workflow_output, workflow_log) are processed.
	srv.controlChannel.SetWorkflowEventHandler(srv.workflowDispatcher)

	// Initialize authorization service
	srv.authzService = NewAuthzService(s, logging.Subsystem("hub.auth"))

	// Seed default policies and groups (idempotent)
	seedDefaultPoliciesAndGroups(ctx, s)

	// Seed the dev user when dev-auth is enabled so that Ent FK constraints
	// on owner_id are satisfied when the dev user creates groves/groups.
	if cfg.DevAuthToken != "" {
		seedDevUser(ctx, s)
	}

	// Abort any maintenance operations/migrations left in "running" state from
	// a previous server instance that was restarted mid-operation.
	if runs, migrations, err := s.AbortRunningMaintenanceOps(ctx); err != nil {
		slog.Warn("Failed to abort stalled maintenance operations", "error", err)
	} else if runs > 0 || migrations > 0 {
		slog.Info("Aborted stalled maintenance operations from previous run",
			"runs", runs, "migrations", migrations)
	}

	// Build unified auth configuration
	srv.authConfig = AuthConfig{
		Mode:           "production",
		DevAuthEnabled: cfg.DevAuthToken != "",
		DevAuthToken:   cfg.DevAuthToken,
		AgentTokenSvc:  srv.agentTokenService,
		UserTokenSvc:   srv.userTokenService,
		UATSvc:         srv.uatService,
		TrustedProxies: cfg.TrustedProxies,
		Debug:          cfg.Debug,
		Logger:         srv.authLog,
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

	// Initialize GCP token rate limiter (1 req/sec average, burst of 10)
	srv.gcpTokenRateLimiter = NewGCPTokenRateLimiter(1, 10)

	srv.registerRoutes()

	return srv, nil
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

	// Not found anywhere, generate a new one
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
func signingKeySecretID(keyName, hubID string) string {
	if hubID == "" {
		return fmt.Sprintf("hub-%s", keyName)
	}
	return fmt.Sprintf("hub-%s-%s", hubID, keyName)
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

// StartNotificationDispatcher creates and starts the notification dispatcher
// if a ChannelEventPublisher is available. It uses a lazy getter for the
// AgentDispatcher so it works even if SetDispatcher is called later.
// Safe to call multiple times; subsequent calls are no-ops.
func (s *Server) StartNotificationDispatcher() {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.notificationDispatcher != nil {
		return // already started
	}

	ep, ok := s.events.(*ChannelEventPublisher)
	if !ok {
		slog.Warn("Event publisher does not support subscriptions, notification dispatcher not started")
		return
	}

	nd := NewNotificationDispatcher(s.store, ep, s.GetDispatcher, logging.Subsystem("hub.notifications"))
	nd.messageLog = s.dedicatedMessageLog
	nd.channelRegistry = s.channelRegistry
	s.notificationDispatcher = nd
	s.notificationDispatcher.Start()
}

// StartMessageBroker creates and starts the message broker proxy if a
// ChannelEventPublisher is available. The broker enables pub/sub message
// routing with topic-based subscriptions and broadcast fan-out.
// Safe to call multiple times; subsequent calls are no-ops.
func (s *Server) StartMessageBroker(b broker.MessageBroker) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.messageBrokerProxy != nil {
		return // already started
	}

	ep, ok := s.events.(*ChannelEventPublisher)
	if !ok {
		slog.Warn("Event publisher does not support subscriptions, message broker proxy not started")
		return
	}

	proxy := NewMessageBrokerProxy(b, s.store, ep, s.GetDispatcher, logging.Subsystem("hub.broker"))
	proxy.messageLog = s.dedicatedMessageLog
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
		client = NewHybridBrokerClient(s.controlChannel, httpClient, &hmacBrokerSigner{store: s.store}, s.config.Debug)
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

	return dispatcher
}

// GenerateAgentToken generates a JWT for an agent.
// This is a convenience method that delegates to the token service.
// Additional scopes are merged with the default scopes (status update, token refresh, and notify).
func (s *Server) GenerateAgentToken(agentID, groveID string, ancestry []string, additionalScopes ...AgentTokenScope) (string, error) {
	s.mu.RLock()
	tokenService := s.agentTokenService
	s.mu.RUnlock()

	if tokenService == nil {
		return "", fmt.Errorf("agent token service not initialized")
	}

	scopes := []AgentTokenScope{ScopeAgentStatusUpdate, ScopeAgentTokenRefresh, ScopeAgentNotify}

	// In dev-auth mode, auto-grant agent creation and lifecycle scopes
	// so agents can create sub-agents without explicit template configuration.
	if s.config.DevAuthToken != "" {
		scopes = append(scopes, ScopeAgentCreate, ScopeAgentLifecycle)
	}

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

	return tokenService.GenerateAgentToken(agentID, groveID, scopes, ancestry)
}

// agentHeartbeatTimeoutHandler returns a recurring handler function that marks
// agents as offline when their last heartbeat exceeds a 2-minute threshold.
// It publishes status events for each affected agent so SSE subscribers and the
// notification system are informed.
func (s *Server) agentHeartbeatTimeoutHandler() func(ctx context.Context) {
	return func(ctx context.Context) {
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
func (s *Server) agentStalledDetectionHandler() func(ctx context.Context) {
	return func(ctx context.Context) {
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
		} else if payload.AgentName != "" && evt.GroveID != "" {
			agent, err = s.store.GetAgentBySlug(ctx, evt.GroveID, payload.AgentName)
		} else {
			return fmt.Errorf("message payload must include agentId or agentName")
		}
		if err != nil {
			if errors.Is(err, store.ErrNotFound) {
				slog.Warn("Scheduler: target agent no longer exists, dropping scheduled message",
					"eventID", evt.ID,
					"agentName", payload.AgentName,
					"agent_id", payload.AgentID,
					"groveID", evt.GroveID,
					"message", payload.Message)
				return fmt.Errorf("target agent %q no longer exists", targetName)
			}
			return fmt.Errorf("failed to resolve agent %q: %w", targetName, err)
		}

		dispatcher := s.GetDispatcher()
		if dispatcher == nil {
			return fmt.Errorf("no dispatcher available to deliver message")
		}

		// Reconstruct structured message from payload to preserve traits like Plain.
		structuredMsg := messages.NewInstruction("scheduler", "agent:"+agent.Slug, payload.Message)
		structuredMsg.SenderID = "SCHEDULER"
		structuredMsg.RecipientID = agent.ID
		structuredMsg.Plain = payload.Plain
		structuredMsg.Urgent = payload.Interrupt

		if err := dispatcher.DispatchAgentMessage(ctx, agent, payload.Message, payload.Interrupt, structuredMsg); err != nil {
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
// an agent in the grove via the AgentDispatcher.
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

		// Verify grove exists
		grove, err := s.store.GetGrove(ctx, evt.GroveID)
		if err != nil {
			if errors.Is(err, store.ErrNotFound) {
				return fmt.Errorf("grove %q no longer exists", evt.GroveID)
			}
			return fmt.Errorf("failed to resolve grove %q: %w", evt.GroveID, err)
		}

		// Resolve the runtime broker for this grove
		runtimeBrokerID := ""
		providers, provErr := s.store.GetGroveProviders(ctx, evt.GroveID)
		if provErr == nil && len(providers) > 0 {
			runtimeBrokerID = providers[0].BrokerID
		}

		// Check if an agent with this name already exists
		existingAgent, err := s.store.GetAgentBySlug(ctx, evt.GroveID, slug)
		if err == nil && existingAgent != nil {
			slog.Warn("Scheduler: agent already exists, skipping dispatch_agent",
				"eventID", evt.ID,
				"agentName", slug,
				"groveID", evt.GroveID,
				"existingPhase", existingAgent.Phase)
			return fmt.Errorf("agent %q already exists in grove", slug)
		}

		// Create the agent record
		agent := &store.Agent{
			ID:              api.NewUUID(),
			Slug:            slug,
			Name:            slug,
			Template:        payload.Template,
			GroveID:         evt.GroveID,
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

		// Apply grove-level default template if none specified
		if payload.Template == "" && grove != nil && grove.Annotations != nil {
			if dt := grove.Annotations[groveSettingDefaultTemplate]; dt != "" {
				payload.Template = dt
				agent.Template = dt
			}
		}

		// Resolve template if specified
		if payload.Template != "" {
			tmpl, tmplErr := s.resolveTemplate(ctx, payload.Template, evt.GroveID)
			if tmplErr == nil && tmpl != nil {
				if tmpl.Slug != "" {
					agent.Template = tmpl.Slug
				}
				harnessConfig := s.getHarnessConfigFromTemplate(tmpl, "")
				if harnessConfig != "" {
					agent.AppliedConfig.HarnessConfig = harnessConfig
				}
			}
		}

		// Apply grove-level defaults (harness config, limits, resources) from annotations
		applyGroveDefaults(agent.AppliedConfig, grove)

		s.populateAgentConfig(agent, grove, nil)

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
			"grove_id", evt.GroveID)
		return nil
	}
}

// workflowRunEventHandler returns an EventHandler that creates a WorkflowRun
// and hands off to the WorkflowRunDispatcher (Phase 4a).
// The run is created with status "queued" and stamped with the schedule's
// creator as created_by_user_id. Dispatch is fire-and-forget.
func (s *Server) workflowRunEventHandler() EventHandler {
	return func(ctx context.Context, evt store.ScheduledEvent) error {
		if evt.WorkflowSource == "" {
			return fmt.Errorf("workflow_run event %s has empty workflow_source", evt.ID)
		}

		// Verify the grove still exists.
		if _, err := s.store.GetGrove(ctx, evt.GroveID); err != nil {
			if errors.Is(err, store.ErrNotFound) {
				return fmt.Errorf("grove %q no longer exists", evt.GroveID)
			}
			return fmt.Errorf("failed to resolve grove %q: %w", evt.GroveID, err)
		}

		// Normalise inputs.
		inputsJSON := evt.WorkflowInputs
		if inputsJSON == "" {
			inputsJSON = "{}"
		}

		// Stamp creator from the schedule/event creator (user ID).
		var createdByUserID *string
		if evt.CreatedBy != "" {
			id := evt.CreatedBy
			createdByUserID = &id
		}

		run := &store.WorkflowRun{
			ID:              api.NewUUID(),
			GroveID:         evt.GroveID,
			SourceYaml:      evt.WorkflowSource,
			InputsJSON:      inputsJSON,
			Status:          store.WorkflowRunStatusQueued,
			CreatedByUserID: createdByUserID,
		}

		if err := s.store.CreateWorkflowRun(ctx, run); err != nil {
			return fmt.Errorf("failed to create WorkflowRun for event %s: %w", evt.ID, err)
		}

		slog.Info("Scheduler: workflow_run event created WorkflowRun",
			"eventID", evt.ID,
			"runID", run.ID,
			"groveID", evt.GroveID)

		// Fire-and-forget dispatch — does not block the scheduler.
		if s.workflowDispatcher != nil {
			s.workflowDispatcher.DispatchAsync(run.ID)
		} else {
			slog.Warn("Scheduler: no workflow dispatcher available, run queued but not dispatched",
				"eventID", evt.ID, "runID", run.ID)
		}

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
		"grove_id", sched.GroveID)

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
		ID:             api.NewUUID(),
		GroveID:        sched.GroveID,
		EventType:      sched.EventType,
		FireAt:         now,
		Payload:        sched.Payload,
		Status:         store.ScheduledEventPending,
		CreatedBy:      sched.CreatedBy,
		ScheduleID:     sched.ID,
		WorkflowSource: sched.WorkflowSource,
		WorkflowInputs: sched.WorkflowInputs,
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

	// Initialize and start the scheduler
	s.scheduler = NewScheduler(s.store, logging.Subsystem("hub.scheduler"))
	s.scheduler.RegisterRecurring("agent-heartbeat-timeout", 1, s.agentHeartbeatTimeoutHandler())
	s.scheduler.RegisterRecurring("agent-stalled-detection", 1, s.agentStalledDetectionHandler())
	if s.config.SoftDeleteRetention > 0 {
		s.scheduler.RegisterRecurring("soft-delete-purge", 60, s.purgeHandler())
	}
	s.scheduler.RegisterEventHandler("message", s.messageEventHandler())
	s.scheduler.RegisterEventHandler("dispatch_agent", s.dispatchAgentEventHandler())
	s.scheduler.RegisterEventHandler("workflow_run", s.workflowRunEventHandler())
	s.scheduler.RegisterRecurring("schedule-evaluator", 1, s.evaluateSchedulesHandler())

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
		s.scheduler.RegisterRecurring("github-app-health-check", interval, s.githubAppHealthCheckHandler())
	}

	s.scheduler.Start(ctx)

	// Start notification dispatcher (uses the current event publisher).
	// The dispatcher is resolved lazily so it works even if SetDispatcher
	// is called after Start().
	s.StartNotificationDispatcher()
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

	// Stop notification dispatcher before closing event publisher
	if s.notificationDispatcher != nil {
		s.notificationDispatcher.Stop()
	}

	// Close event publisher
	if s.events != nil {
		s.events.Close()
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
		if s.messageBrokerProxy != nil {
			s.messageBrokerProxy.Stop()
		}
		if s.events != nil {
			s.events.Close()
		}
		if s.logQueryService != nil {
			s.logQueryService.Close()
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
	s.mux.HandleFunc("/api/v1/auth/cli/authorize", s.handleCLIAuthAuthorize)
	s.mux.HandleFunc("/api/v1/auth/cli/token", s.handleCLIAuthToken)
	s.mux.HandleFunc("/api/v1/auth/cli/device", s.handleCLIDeviceAuthorize)
	s.mux.HandleFunc("/api/v1/auth/cli/device/token", s.handleCLIDeviceToken)

	// API v1 routes
	s.mux.HandleFunc("/api/v1/agents", s.handleAgents)
	s.mux.HandleFunc("/api/v1/agents/", s.handleAgentByID)

	s.mux.HandleFunc("/api/v1/groves", s.handleGroves)
	s.mux.HandleFunc("/api/v1/groves/register", s.handleGroveRegister)
	// Grove-nested routes: /api/v1/groves/{groveId}/agents, /api/v1/groves/{groveId}/env, etc.
	// This handler must come before the generic grove-by-id handler
	s.mux.HandleFunc("/api/v1/groves/", s.handleGroveRoutes)

	s.mux.HandleFunc("/api/v1/runtime-brokers", s.handleRuntimeBrokers)
	s.mux.HandleFunc("/api/v1/runtime-brokers/", s.handleRuntimeBrokerRoutes)

	s.mux.HandleFunc("/api/v1/templates", s.handleTemplatesV2)
	s.mux.HandleFunc("/api/v1/templates/", s.handleTemplateByIDV2)

	s.mux.HandleFunc("/api/v1/harness-configs", s.handleHarnessConfigs)
	s.mux.HandleFunc("/api/v1/harness-configs/", s.handleHarnessConfigByID)

	s.mux.HandleFunc("/api/v1/users", s.handleUsers)
	s.mux.HandleFunc("/api/v1/users/", s.handleUserByID)

	// Environment variables and secrets (generic endpoints)
	s.mux.HandleFunc("/api/v1/env", s.handleEnvVars)
	s.mux.HandleFunc("/api/v1/env/", s.handleEnvVarByKey)
	s.mux.HandleFunc("/api/v1/secrets", s.handleSecrets)
	s.mux.HandleFunc("/api/v1/secrets/", s.handleSecretByKey)

	// Groups and Policies (Hub Permissions System)
	s.mux.HandleFunc("/api/v1/groups", s.handleGroups)
	s.mux.HandleFunc("/api/v1/groups/", s.handleGroupRoutes)
	s.mux.HandleFunc("/api/v1/policies", s.handlePolicies)
	s.mux.HandleFunc("/api/v1/policies/", s.handlePolicyRoutes)

	// Principal resolution endpoints (Phase 4)
	s.mux.HandleFunc("/api/v1/users/me/groups", s.handleMyGroups)
	s.mux.HandleFunc("/api/v1/principals/", s.handlePrincipalRoutes)

	// Broker registration endpoints (Runtime Broker HMAC authentication)
	s.mux.HandleFunc("/api/v1/brokers", s.handleBrokersEndpoint)
	s.mux.HandleFunc("/api/v1/brokers/join", s.handleBrokerJoin)
	s.mux.HandleFunc("/api/v1/brokers/", s.handleBrokerByIDRoutes)

	// Broker plugin inbound message delivery
	s.mux.HandleFunc("/api/v1/broker/inbound", s.handleBrokerInbound)

	// Admin system endpoints
	s.mux.HandleFunc("/api/v1/admin/maintenance", s.handleAdminMaintenance)
	s.mux.HandleFunc("/api/v1/admin/maintenance/operations", s.handleAdminMaintenanceOps)
	s.mux.HandleFunc("/api/v1/admin/maintenance/operations/", s.handleAdminMaintenanceOps)
	s.mux.HandleFunc("/api/v1/admin/maintenance/migrations/", s.handleAdminMaintenanceMigrations)
	s.mux.HandleFunc("/api/v1/admin/maintenance/check-updates", s.handleCheckForUpdates)
	s.mux.HandleFunc("/api/v1/admin/scheduler", s.handleAdminScheduler)
	s.mux.HandleFunc("/api/v1/admin/server-config", s.handleAdminServerConfig)
	s.mux.HandleFunc("/api/v1/admin/gcp-quota", s.handleAdminGCPQuota)

	// Notification endpoints (user-facing)
	s.mux.HandleFunc("/api/v1/notifications", s.handleNotifications)
	s.mux.HandleFunc("/api/v1/notifications/", s.handleNotificationRoutes)

	// Message inbox endpoints (user-facing)
	s.mux.HandleFunc("/api/v1/messages", s.handleMessages)
	s.mux.HandleFunc("/api/v1/messages/", s.handleMessageRoutes)

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

	// GitHub App webhook and setup callback (unauthenticated — uses webhook signature)
	s.mux.HandleFunc("/api/v1/webhooks/github", s.handleGitHubWebhook)
	s.mux.HandleFunc("/github-app/setup", s.handleGitHubAppSetup)

	// WorkflowRun routes: individual run operations (get, cancel, logs).
	// Grove-scoped create/list are handled inside handleGroveRoutes.
	s.mux.HandleFunc("/api/v1/workflows/runs/", s.handleWorkflowRunRoutes)
}

// applyMiddleware wraps the handler with middleware.
func (s *Server) applyMiddleware(h http.Handler) http.Handler {
	// Apply middleware in reverse order (last applied runs first)
	h = s.recoveryMiddleware(h)
	if s.requestLogger != nil {
		h = logging.RequestLogMiddleware(s.requestLogger, "hub", logging.HubPathPatterns())(h)
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

		if duration > 2*time.Second {
			slog.Warn("Slow request",
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
	json.NewEncoder(w).Encode(data)
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
		if err := s.controlChannel.HandleUpgrade(w, r, brokerID); err != nil {
			slog.Error("Upgrade failed for broker", "brokerID", brokerID, "error", err)
			// Error already written by upgrader
			return
		}
		s.markBrokerOnline(brokerID)
		return
	}

	// Use authenticated broker identity
	if err := s.controlChannel.HandleUpgrade(w, r, broker.ID()); err != nil {
		slog.Error("Upgrade failed for broker", "brokerID", broker.ID(), "error", err)
		// Error already written by upgrader
		return
	}
	s.markBrokerOnline(broker.ID())
}

// markBrokerOnline updates broker and provider statuses to online after a successful WebSocket connection.
func (s *Server) markBrokerOnline(brokerID string) {
	ctx := context.Background()
	slog.Info("Broker connected, marking online", "brokerID", brokerID)

	if err := s.store.UpdateRuntimeBrokerHeartbeat(ctx, brokerID, store.BrokerStatusOnline); err != nil {
		slog.Error("Failed to mark broker online", "brokerID", brokerID, "error", err)
	}

	providers, err := s.store.GetBrokerGroves(ctx, brokerID)
	if err != nil {
		slog.Error("Failed to get broker groves for status update", "brokerID", brokerID, "error", err)
		return
	}
	for _, provider := range providers {
		if err := s.store.UpdateProviderStatus(ctx, provider.GroveID, brokerID, store.BrokerStatusOnline); err != nil {
			slog.Error("Failed to update provider status", "brokerID", brokerID, "grove_id", provider.GroveID, "error", err)
		}
	}

	// Publish broker connected event
	groveIDs := make([]string, len(providers))
	for i, p := range providers {
		groveIDs[i] = p.GroveID
	}
	broker, err := s.store.GetRuntimeBroker(ctx, brokerID)
	var brokerName string
	if err == nil {
		brokerName = broker.Name
	}
	s.events.PublishBrokerConnected(ctx, brokerID, brokerName, groveIDs)
}

// isWebSocketUpgrade checks if the request is a WebSocket upgrade request.
func isWebSocketUpgrade(r *http.Request) bool {
	return strings.ToLower(r.Header.Get("Upgrade")) == "websocket" &&
		strings.Contains(strings.ToLower(r.Header.Get("Connection")), "upgrade")
}
