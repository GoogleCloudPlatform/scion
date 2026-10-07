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
	mathrand "math/rand"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/go-jose/go-jose/v4/jwt"
	"go.opentelemetry.io/contrib/instrumentation/net/http/otelhttp"
	"golang.org/x/sync/singleflight"

	"github.com/GoogleCloudPlatform/scion/pkg/agent"
	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/agentkeys"
	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/artifacts"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/config/opsettings"
	"github.com/GoogleCloudPlatform/scion/pkg/ent"
	"github.com/GoogleCloudPlatform/scion/pkg/eventbus"
	"github.com/GoogleCloudPlatform/scion/pkg/experiments"
	"github.com/GoogleCloudPlatform/scion/pkg/harness"
	"github.com/GoogleCloudPlatform/scion/pkg/hub/githubapp"
	"github.com/GoogleCloudPlatform/scion/pkg/hub/imagecheck"
	"github.com/GoogleCloudPlatform/scion/pkg/lifecyclehooks"
	"github.com/GoogleCloudPlatform/scion/pkg/messages"
	"github.com/GoogleCloudPlatform/scion/pkg/messaging"
	"github.com/GoogleCloudPlatform/scion/pkg/observability/dbmetrics"
	"github.com/GoogleCloudPlatform/scion/pkg/observability/dispatchmetrics"
	"github.com/GoogleCloudPlatform/scion/pkg/observability/reapermetrics"
	"github.com/GoogleCloudPlatform/scion/pkg/secret"
	"github.com/GoogleCloudPlatform/scion/pkg/storage"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/GoogleCloudPlatform/scion/pkg/transfer"
	"github.com/GoogleCloudPlatform/scion/pkg/util/logging"
	"github.com/GoogleCloudPlatform/scion/resources"
	"github.com/google/uuid"
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

	// ConduitTCPAllowedPorts is the operator allow-list of agent-local
	// loopback ports a Conduit tcp stream grant may target in addition to
	// the agent's exposed ports. The reserved ports (9810, 18380) are always
	// refused. Only used behind the hub.conduit experiment.
	ConduitTCPAllowedPorts []int
	// ConduitGrantKeyActivation is how long a rotated-in Conduit grant key
	// is published before it signs (default 15m). It must be at least the
	// maximum interval at which grant targets refresh their key set
	// (Welcome and token refresh); otherwise a target that has not yet
	// learned the new key refuses its grants. It must also be at least the
	// hub's ring refresh interval (1m). Only used behind the hub.conduit
	// experiment.
	ConduitGrantKeyActivation time.Duration
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
	// DefaultUserRole is the role assigned to new users who are not in the
	// admin_emails list. Values: "member" (default), "viewer".
	DefaultUserRole string
	// BrokerAuthConfig holds configuration for Runtime Broker HMAC authentication.
	BrokerAuthConfig BrokerAuthConfig
	// HubEndpoint is the public endpoint URL for this Hub (used in broker join responses).
	HubEndpoint string
	// AgentEndpoint optionally overrides HubEndpoint for the sole purpose of
	// the SCION_HUB_ENDPOINT value injected into dispatched agents (see
	// HTTPAgentDispatcher.SetAgentEndpoint). Every other use of HubEndpoint
	// (invite links, chat-bridge links, the OIDC issuer default, the
	// cloudrun_invoker audience default, broker join responses) is
	// unaffected. Empty means agents receive HubEndpoint.
	AgentEndpoint string
	// SlowRequestThreshold is the duration after which an HTTP request is
	// logged as slow. Zero uses logging.DefaultSlowRequestThreshold.
	SlowRequestThreshold time.Duration
	// StalledThreshold is how long an agent can go without activity events
	// before being marked as stalled (default: 5 minutes). Only applies to
	// agents with a recent heartbeat (not already offline).
	StalledThreshold time.Duration
	// StartClaim holds the start-claim timing settings from the settings
	// file (zero fields use defaults; out-of-range values are replaced by
	// defaults with a warning). Hot-reloaded through ApplySnapshot.
	StartClaim StartClaimSettings
	// MissingAgentGrace is how long a running agent must be continuously
	// absent from its runtime broker's complete heartbeat inventory before
	// the Hub marks it phase=error with exit reason container_missing (an
	// existing preempted or evicted exit reason is kept) (default: 3 minutes,
	// minimum: 1 minute; lower values use the default).
	MissingAgentGrace time.Duration
	// AutoSuspendStalled controls whether stalled agents are automatically
	// suspended (container stopped, phase set to "suspended"). Default: false.
	AutoSuspendStalled bool
	// SoftDeleteRetention is how long soft-deleted agents are retained before purging.
	// Zero means soft-delete is disabled (hard-delete immediately).
	SoftDeleteRetention time.Duration
	// SoftDeleteRetainFiles controls whether workspace files are preserved during soft-delete.
	SoftDeleteRetainFiles bool
	// AsyncAgentLaunch is the non-blocking agent create kill switch. Off by
	// default; a launch is non-blocking only when this is on AND the request
	// opts in.
	AsyncAgentLaunch bool
	// LaunchTimeout is the whole-launch budget for an opted-in launch
	// (design §3.10). Default 5 minutes. Below minLaunchTimeout the broker's
	// fixed 20s abort margin (§3.10) would leave no time for a launch to
	// actually run, so New() rejects it and falls back to the default.
	LaunchTimeout time.Duration
	// LaunchKeepaliveSeconds is the broker keepalive interval, in seconds
	// (design §3.7). It is sent to the broker as launchKeepaliveSeconds in
	// each asynchronous create request, and sets the reaper's staleness
	// window (8x this value). Default 15.
	LaunchKeepaliveSeconds int
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
	// EnforceBrokerQuotas controls whether the per-broker agent quota cap
	// (max_agents_per_broker) is enforced on create. nil means unset — the
	// fail-safe default (enforced) applies. See brokerQuotasEnforced.
	EnforceBrokerQuotas *bool
	// AgentSecretsUserScopeOnly controls whether agents are restricted to
	// writing user (profile) scope secrets only. nil means unset — the
	// permissive default (agents may write project scope) applies. See
	// agentSecretsUserScopeOnly.
	AgentSecretsUserScopeOnly *bool
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
	// DisableCloudLogQuery skips building the Cloud Logging query service
	// even when a GCP project ID is found in the environment
	// (logging.ResolveProjectID). Tests set it so that constructing a server
	// never creates real Cloud Logging clients from ambient env.
	DisableCloudLogQuery bool
	// GCPMintCapPerProject is the maximum number of minted service accounts allowed per project.
	// Zero means unlimited (default).
	GCPMintCapPerProject int
	// GCPMintCapGlobal is the maximum total number of minted service accounts across all projects.
	// Zero means unlimited (default).
	GCPMintCapGlobal int
	// GCPMintCapPerHub is the maximum number of minted service accounts at hub scope.
	// Zero means unlimited (default). Does not affect the global cap, which counts
	// all minted SAs regardless of scope.
	GCPMintCapPerHub int
	// TransportMode is the transport-layer auth mode: "none" (default), "cloudrun_invoker", "iap".
	// Controls which transport tokens the hub issues to agents.
	TransportMode string
	// TransportAudience is the OIDC audience for transport tokens.
	// For IAP: the IAP OAuth client ID. For cloudrun_invoker: the hub URL.
	TransportAudience string
	// TransportMinter mints transport-layer OIDC tokens for agents.
	// Nil when TransportMode == "none" or unset.
	TransportMinter TransportTokenMinter
	// PlatformAuthSA is the configured platform/transport auth service
	// account email (cfg.Auth.Transport.PlatformAuthSA). The hub does not
	// create or authenticate user accounts for this identity. Empty when no
	// transport service account is configured.
	PlatformAuthSA string
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

	// ConfigPath is the --config path the server was started with ("" for
	// none). The workstation server-config PUT uses it to find the legacy
	// server.yaml sources the config loader would read.
	ConfigPath string
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

	// GEGoogleExchange holds the trust configuration for the GE Google
	// credential exchange endpoint. When Enabled, the server creates a
	// GEExchangeService and registers the exchange route.
	GEGoogleExchange GEGoogleExchangeConfig

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

	// AuditRetentionDays is the number of days to retain authorization audit records.
	// Default: 90. Used by CleanupAuditRecords for periodic retention cleanup.
	AuditRetentionDays int

	// FailedMessageRetentionDays is the number of days to retain messages in
	// dispatch_state="failed" before the failed-message-retention sweep
	// purges them. Default: 7 (see defaultFailedMessageRetentionDays). Mirrors
	// the AuditRetentionDays pattern. Zero or negative falls back to the
	// default rather than disabling the sweep.
	FailedMessageRetentionDays int

	// Experiments is the compiled experiments registry used to resolve
	// hub-wide feature flags (pkg/experiments). Production leaves this nil;
	// every reader goes through the nil-safe Server.experimentRegistry(),
	// which falls back to experiments.Default(). Tests that need a
	// server-layer experiment inject their own registry here (ptone/scion#2217).
	Experiments *experiments.Registry
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

	// DeploymentTier selects the update strategy: "source" (git-based) or "binary" (release-based).
	DeploymentTier string
	// ReleaseChannel selects which release channel to track: "stable" or "preview".
	// When empty, derived from the running binary's version string.
	ReleaseChannel string
	// UpdatePolicy controls automatic update behavior: "auto", "notify", or "disabled".
	UpdatePolicy string
	// CheckIntervalHours sets how often the scheduled update checker runs.
	// Default: 6. Minimum: 1.
	CheckIntervalHours int
	// GitHubRepo is the GitHub repository for release lookups.
	// Default: "GoogleCloudPlatform/scion".
	GitHubRepo string
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
		CORSMaxAge:             3600,
		StalledThreshold:       5 * time.Minute,
		BrokerAuthConfig:       DefaultBrokerAuthConfig(),
		LaunchTimeout:          5 * time.Minute,
		LaunchKeepaliveSeconds: 15,
		MissingAgentGrace:      DefaultMissingAgentGrace,
	}
}

// brokerQuotasEnforced reports whether the per-broker agent quota cap
// (max_agents_per_broker) is enforced on create. Fail-safe default: an
// absent (nil) switch means enforced (design P1-D4/P1-D5).
//
// Thread-safe: s.config.EnforceBrokerQuotas is written under s.mu.Lock() by
// ApplySnapshot (on the admin PUT path, and on every replica via the
// LISTEN/NOTIFY + 60s poll propagation loop in postgres mode), so it must be
// read under s.mu.RLock() here — this is called on every QuotaService.Reserve,
// for every limit, on every create/start/restart/resume/wake.
func (s *Server) brokerQuotasEnforced() bool {
	s.mu.RLock()
	v := s.config.EnforceBrokerQuotas
	s.mu.RUnlock()
	return v == nil || *v
}

// agentSecretsUserScopeOnly reports whether agents are restricted to
// writing user (profile) scope secrets only. Permissive default: an absent
// (nil) switch means agents may write project scope, as they do today
// (design ptone/scion#2291 §5).
//
// Thread-safe: s.config.AgentSecretsUserScopeOnly is written under
// s.mu.Lock() by ApplySnapshot (on the admin PUT path, and on every replica
// via the LISTEN/NOTIFY + 60s poll propagation loop in postgres mode), so it
// must be read under s.mu.RLock() here.
func (s *Server) agentSecretsUserScopeOnly() bool {
	s.mu.RLock()
	v := s.config.AgentSecretsUserScopeOnly
	s.mu.RUnlock()
	return v != nil && *v
}

// AgentDispatcher is the interface for dispatching agent operations to a runtime broker.
// Implementations may be local (co-located hub+broker) or remote (HTTP-based).
type AgentDispatcher interface {
	// DispatchAgentCreate creates and starts an agent on the runtime broker.
	// It updates agent in place from the broker's answer. A non-nil result
	// with Launch set means the broker accepted the create for asynchronous
	// launch (see CreateDispatchResult).
	DispatchAgentCreate(ctx context.Context, agent *store.Agent) (*CreateDispatchResult, error)

	// DispatchAgentProvision provisions an agent on the runtime broker without starting it.
	// This sets up directories, worktree, templates, and settings but does not launch the container.
	DispatchAgentProvision(ctx context.Context, agent *store.Agent) error

	// DispatchAgentReprovision re-renders an EXISTING agent's on-disk config
	// on the runtime broker from its current AppliedConfig, for a
	// `scion reincarnate` request (design §3.4). Unlike DispatchAgentProvision
	// it overwrites the persisted config rather than reusing it, while
	// preserving the agent's home directory and clone-per-agent workspace. It
	// does not start the container.
	DispatchAgentReprovision(ctx context.Context, agent *store.Agent) error

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
	// If the broker returns 202 with env requirements, the result carries them
	// in EnvReqs instead of an error. Launch is set when the broker accepted
	// the create for asynchronous launch.
	DispatchAgentCreateWithGather(ctx context.Context, agent *store.Agent) (*CreateDispatchResult, error)

	// DispatchFinalizeEnv sends gathered env vars to the broker to complete agent creation.
	// Launch is set in the result when the broker accepted the create for
	// asynchronous launch.
	DispatchFinalizeEnv(ctx context.Context, agent *store.Agent, env map[string]string) (*CreateDispatchResult, error)
}

// WorkspaceDispatchSpec carries the inputs a broker needs to recreate an
// agent's workspace, built once by workspaceSpecFor so create and start
// dispatch from a single source and cannot drift from each other
// (GoogleCloudPlatform/scion#1931).
//
// sciontool's own clone step is idempotent with respect to a populated
// workspace (it skips cloning when /workspace is already non-empty), and the
// broker only clears an existing workspace on a create dispatch (see
// FreshProvision), so GitClone and Branch are safe to send on start as well
// as create: where the workspace survived a stop, the clone is a no-op;
// where it didn't, it recreates it. WorkspaceMode is included so the broker
// can apply create's worktree-per-agent provisioning on start too, instead
// of always falling through to the in-container clone path GitClone
// otherwise triggers.
//
// Exported because it is a field of the exported StartExtras.
type WorkspaceDispatchSpec struct {
	// GitClone specifies git clone parameters for git-anchored projects.
	GitClone *api.GitCloneConfig
	// Branch is the git branch name (defaults to the agent slug if empty).
	Branch string
	// WorkspaceMode is the resolved workspace sharing mode (e.g.
	// "worktree-per-agent"), the same value create sends as
	// RemoteCreateAgentRequest.WorkspaceMode.
	WorkspaceMode string
	// SharedWorkspaceClone is a shared-plain git project's workspace clone
	// settings (RemoteAgentConfig.SharedWorkspaceClone); nil otherwise.
	SharedWorkspaceClone *api.GitCloneConfig
}

// StartExtras carries the dispatch-time metadata that the create path already
// sends but that the start/restart paths historically dropped (#1960): the
// Hub endpoint (for pre-resolved-skill URL rewriting), the owning user's ID,
// project-scope credentials for provision-time skill resolution, and any
// Hub-registry skills already resolved as the agent's creator. Passed to
// StartAgent/RestartAgent so the broker can attach the same skill resolver on
// every path that can reach ProvisionAgent, not just create.
//
// Workspace carries the workspace-recreation inputs
// (GoogleCloudPlatform/scion#1931). DispatchAgentStart populates it;
// DispatchAgentRestart does not send it and leaves it at its zero value.
//
// Zero value is valid and simply carries nothing extra, matching pre-#1960
// behavior for callers (e.g. local/file-mode dispatch) that have none of this.
type StartExtras struct {
	HubEndpoint          string
	UserID               string
	ProvisionCredentials map[string]string
	PreResolvedSkills    *ResolveSkillsResponse
	Workspace            WorkspaceDispatchSpec
	// RunID is the run identity the hub minted for this start or restart;
	// the broker labels the new runtime entry with it (ptone/scion#2550).
	RunID string
	// HubAgentDefaults carries the hub defaults a start applies at the
	// broker's lowest tier (see startHubAgentDefaults). Nil = none.
	HubAgentDefaults *RemoteHubAgentDefaults
	// Image is the user's explicit image (explicitDispatchImage), already
	// registry-rewritten; empty when the user chose none. The broker applies
	// it as the top-tier image (opts.Image), the same as create's
	// Config.Image, so a start or restart ranks the image exactly as the
	// create did (ptone/scion#1799). A template-derived image is never sent.
	Image string
	// SharedWorkspace is set on a restart (the start request already
	// carries it as its own field) so the broker reads and writes a
	// shared-workspace agent's state under the same broker-side agents root
	// as its start (ptone/scion#1799).
	SharedWorkspace bool
	// TemplateName is the agent's template slug. The broker uses it for
	// naming only (the scion.template label, SCION_TEMPLATE_NAME and
	// agent-info.json), never to locate or load a template. A content hash
	// is not a template name and is not sent.
	TemplateName string
}

// applyStartExtras writes extras onto payload as flat top-level wire keys.
// Both the HTTP transport (brokerHTTPTransport) and the control-channel
// transport (ControlChannelBrokerClient) call this for their StartAgent and
// RestartAgent payloads, so the two transports cannot drift from each other
// on which StartExtras fields are sent or under what key.
func applyStartExtras(payload map[string]interface{}, extras StartExtras) {
	if extras.HubEndpoint != "" {
		payload["hubEndpoint"] = extras.HubEndpoint
	}
	if extras.UserID != "" {
		payload["userId"] = extras.UserID
	}
	if len(extras.ProvisionCredentials) > 0 {
		payload["provisionCredentials"] = extras.ProvisionCredentials
	}
	if extras.PreResolvedSkills != nil {
		payload["preResolvedSkills"] = extras.PreResolvedSkills
	}
	if extras.Workspace.GitClone != nil {
		payload["gitClone"] = extras.Workspace.GitClone
	}
	if extras.Workspace.Branch != "" {
		payload["branch"] = extras.Workspace.Branch
	}
	if extras.Workspace.WorkspaceMode != "" {
		payload["workspaceMode"] = extras.Workspace.WorkspaceMode
	}
	if extras.Workspace.SharedWorkspaceClone != nil {
		payload["sharedWorkspaceClone"] = extras.Workspace.SharedWorkspaceClone
	}
	if extras.RunID != "" {
		payload["runId"] = extras.RunID
	}
	if extras.HubAgentDefaults != nil {
		payload["hubAgentDefaults"] = extras.HubAgentDefaults
	}
	if extras.Image != "" {
		payload["image"] = extras.Image
	}
	if extras.SharedWorkspace {
		payload["sharedWorkspace"] = true
	}
	if extras.TemplateName != "" && !transfer.IsContentHash(extras.TemplateName) {
		payload["templateName"] = extras.TemplateName
	}
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
	// extras carries the dispatch metadata described on StartExtras.
	StartAgent(ctx context.Context, brokerID, brokerEndpoint, agentID, projectID, task, projectPath, projectSlug, harnessConfig, harnessConfigID, harnessConfigHash string, resolvedEnv map[string]string, resolvedSecrets []ResolvedSecret, inlineConfig *api.ScionConfig, sharedDirs []api.SharedDir, sharedWorkspace, resume bool, extras StartExtras) (*RemoteAgentResponse, error)

	// StopAgent stops an agent on a remote runtime broker.
	// brokerID is used for HMAC authentication lookup.
	// projectID scopes the lookup to a specific project (required for uniqueness).
	// runID, when non-empty, names the run the stop is for
	// (ptone/scion#2550): the broker then stops only that run's entry and
	// answers a run_mismatch 404 (api.BrokerErrorCodeRunMismatch) when
	// another run holds the name; only that 404 is returned as
	// ErrStopRunNotFound, and any other 404 as a plain broker error. An
	// empty runID keeps the legacy name-scoped stop.
	StopAgent(ctx context.Context, brokerID, brokerEndpoint, agentID, projectID, runID string) error

	// RestartAgent restarts an agent on a remote runtime broker.
	// brokerID is used for HMAC authentication lookup.
	// projectID scopes the lookup to a specific project (required for uniqueness).
	// resolvedEnv carries fresh auth tokens and identity vars so the restarted
	// container retains Hub connectivity.
	// extras carries the dispatch metadata described on StartExtras.
	// RestartAgent returns the broker's response body when it sent one
	// (nil when the body is empty or undecodable; the restart still
	// succeeded), so the dispatcher can adopt the run ID it reports.
	RestartAgent(ctx context.Context, brokerID, brokerEndpoint, agentID, projectID string, resolvedEnv map[string]string, extras StartExtras) (*RemoteAgentResponse, error)

	// ResetAuthAgent injects a fresh auth token into a running agent without restarting it.
	// brokerID is used for HMAC authentication lookup.
	// projectID scopes the lookup to a specific project (required for uniqueness).
	ResetAuthAgent(ctx context.Context, brokerID, brokerEndpoint, agentID, projectID, token, transportToken string) error

	// DeleteAgent deletes an agent from a remote runtime broker.
	// brokerID is used for HMAC authentication lookup.
	// projectID scopes the lookup to a specific project (required for uniqueness).
	// opts carries the query params (see DeleteAgentOptions).
	DeleteAgent(ctx context.Context, brokerID, brokerEndpoint, agentID, projectID string, opts DeleteAgentOptions) error

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

// DeleteAgentOptions carries the optional parameters of a broker agent delete.
// SoftDelete and DeletedAt are passed as query params for broker-side
// marking. RunID, when non-empty, is sent as runId: the broker then deletes
// only the runtime entry labelled with that run and answers 404 (an
// idempotent success) when only a different run holds the name
// (ptone/scion#2550). NotAfter, when non-zero, is sent as notAfter: the
// broker refuses the delete with 409 stale_dispatch, doing nothing, if it
// arrives after that instant (ptone/scion#2906, see agent_delete_fence.go).
type DeleteAgentOptions struct {
	DeleteFiles  bool
	RemoveBranch bool
	SoftDelete   bool
	DeletedAt    time.Time
	RunID        string
	NotAfter     time.Time
	// LocalOnly removes only the broker's own state for the agent, never
	// its files on the NFS export or its branch (runtimebroker deleteAgent,
	// ?localOnly). Send it only to a broker advertising AgentMove.
	LocalOnly bool
}

// deleteAgentQuery renders opts (and the context's linked-project path) as
// the query string both broker transports send, without a leading
// separator.
func deleteAgentQuery(ctx context.Context, projectID string, opts DeleteAgentOptions) string {
	query := fmt.Sprintf("deleteFiles=%t&removeBranch=%t", opts.DeleteFiles, opts.RemoveBranch)
	if projectID != "" {
		query += "&projectId=" + url.QueryEscape(projectID)
	}
	query += deleteProjectPathQuery(ctx)
	if opts.RunID != "" {
		query += "&runId=" + url.QueryEscape(opts.RunID)
	}
	if !opts.NotAfter.IsZero() {
		query += "&notAfter=" + url.QueryEscape(opts.NotAfter.UTC().Format(time.RFC3339))
	}
	if opts.SoftDelete {
		query += fmt.Sprintf("&softDelete=true&deletedAt=%s", url.QueryEscape(opts.DeletedAt.UTC().Format(time.RFC3339)))
	}
	if opts.LocalOnly {
		query += "&localOnly=true"
	}
	// The recorded runtime (GoogleCloudPlatform/scion#2423) rides on ctx, as
	// for every other existing-agent operation, so both transports send it
	// beside runId.
	return withRecordedRuntimeQuery(ctx, query)
}

// stopAgentQuery builds the query string for a broker stop request. runId,
// when set, names the run the stop is for (ptone/scion#2550); an old broker
// ignores it and stops by name as before.
func stopAgentQuery(ctx context.Context, projectID, runID string) string {
	var query string
	if projectID != "" {
		query = "projectId=" + url.QueryEscape(projectID)
	}
	if runID != "" {
		if query != "" {
			query += "&"
		}
		query += "runId=" + url.QueryEscape(runID)
	}
	return withRecordedRuntimeQuery(ctx, query)
}

// withRunIDURL appends the runId query parameter to a start or restart
// request URL (or path) when runID is set (ptone/scion#2550). The run also
// travels in the body; the parameter lets the broker record it on the
// tracked start before reading the body, so a run-scoped stop never sees
// that start without its run. An older broker ignores it.
func withRunIDURL(endpoint, runID string) string {
	if runID == "" {
		return endpoint
	}
	if strings.Contains(endpoint, "?") {
		return endpoint + "&runId=" + url.QueryEscape(runID)
	}
	return endpoint + "?runId=" + url.QueryEscape(runID)
}

// withRunIDQuery is withRunIDURL for a bare query string.
func withRunIDQuery(query, runID string) string {
	if runID == "" {
		return query
	}
	if query == "" {
		return "runId=" + url.QueryEscape(runID)
	}
	return query + "&runId=" + url.QueryEscape(runID)
}

// ErrStopRunNotFound reports that a run-scoped stop found no entry of the
// requested run on the broker: the broker answered 404 with error code
// api.BrokerErrorCodeRunMismatch, because a different run holds the
// agent's name and was left untouched (ptone/scion#2550). Callers must not
// record the current run as stopped because of it. The error still unwraps
// to the broker's status error; brokerStopCurrentRunID reads the run the
// broker reported holding the name.
var ErrStopRunNotFound = errors.New("runtime broker has no entry for the requested run")

// stopAgentError marks the broker's run-mismatch 404 on a run-scoped stop
// as ErrStopRunNotFound, on both transports. It keys on the broker's error
// code, not the status alone, so a 404 from anything else (a proxy, an
// unknown route) is returned unchanged, as is any error on a legacy stop
// without a run ID.
func stopAgentError(err error, runID string) error {
	if err == nil || runID == "" || !isBrokerStatus(err, http.StatusNotFound) {
		return err
	}
	var se *brokerStatusError
	if !errors.As(err, &se) || se.brokerErrorCode() != api.BrokerErrorCodeRunMismatch {
		return err
	}
	if current, ok := se.brokerErrorDetails()[api.BrokerErrorDetailCurrentRunID].(string); ok && current != "" {
		return fmt.Errorf("%w (requested run %s; the broker holds run %s): %w", ErrStopRunNotFound, runID, current, err)
	}
	return fmt.Errorf("%w (requested run %s): %w", ErrStopRunNotFound, runID, err)
}

// brokerStopCurrentRunID returns the run the broker reported holding the
// agent's name when it refused a run-scoped stop (ErrStopRunNotFound), so
// a hub/broker run drift can be logged. ok is false for any other error or
// when the broker did not know the run.
func brokerStopCurrentRunID(err error) (string, bool) {
	if !errors.Is(err, ErrStopRunNotFound) {
		return "", false
	}
	var se *brokerStatusError
	if !errors.As(err, &se) {
		return "", false
	}
	current, ok := se.brokerErrorDetails()[api.BrokerErrorDetailCurrentRunID].(string)
	return current, ok && current != ""
}

// recordStopStatus writes upd, the stopped (or suspended) status a caller
// records after a stop it dispatched for run runID, only while the row
// still holds runID (store.AgentStatusUpdate.IfRunID, ptone/scion#2550).
// A broker's 202 for run X must never mark a newer run Y stopped.
//
// It reports whether the status was written. When the row has moved to
// another run it writes nothing, logs a Warn naming both runs, and returns
// false with a nil error: the caller must then skip its quota release and
// publish too. An empty runID (a row from before run IDs) writes
// unconditionally, as before.
func (s *Server) recordStopStatus(ctx context.Context, agentID, runID, action string, upd store.AgentStatusUpdate) (bool, error) {
	upd.IfRunID = runID
	err := s.store.UpdateAgentStatus(ctx, agentID, upd)
	if errors.Is(err, store.ErrRunChanged) {
		current := ""
		if a, gerr := s.store.GetAgent(ctx, agentID); gerr == nil {
			current = a.RunID
		}
		s.agentLifecycleLog.Warn("Agent "+action+": the agent moved to a newer run after the stop was dispatched; not recording the "+action,
			"agent_id", agentID, "stopped_run_id", runID, "current_run_id", current)
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, nil
}

// stopRunStillCurrent reports whether the row still holds runID, for a
// caller that has no status to write after a stop dispatched for runID but
// must not release a newer run's reservation (ptone/scion#2550). It logs a
// Warn naming both runs when the run changed. An empty runID, or a failed
// read, counts as current, as before run IDs.
func (s *Server) stopRunStillCurrent(ctx context.Context, agentID, runID, action string) bool {
	if runID == "" {
		return true
	}
	current, err := s.store.GetAgent(ctx, agentID)
	if err != nil || current.RunID == runID {
		return true
	}
	s.agentLifecycleLog.Warn("Agent "+action+": the agent moved to a newer run after the stop was dispatched; not recording the "+action,
		"agent_id", agentID, "stopped_run_id", runID, "current_run_id", current.RunID)
	return false
}

// logStopRunMismatch logs a run-scoped stop the broker refused because a
// different run holds the agent's name, naming both runs, so the operator
// can see a hub/broker run drift behind the failed stop or suspend. It is a
// no-op for any other error.
func (s *Server) logStopRunMismatch(agent *store.Agent, action string, err error) {
	if !errors.Is(err, ErrStopRunNotFound) {
		return
	}
	current, _ := brokerStopCurrentRunID(err)
	s.agentLifecycleLog.Warn("Agent "+action+": broker holds a different run than the hub recorded; nothing was stopped",
		"agent_id", agent.ID, "agent", agent.Slug, "hub_run_id", agent.RunID, "broker_run_id", current)
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
	// EnvClassifications records the classification (plain, secret-fetchable,
	// secret-injected) for each key in ResolvedEnv. Parallel structure:
	// same key names, independent lifecycle (#127, P3a).
	//
	// Three-state semantics:
	//   - Non-nil map, key present: classified as that kind.
	//   - Non-nil map, key absent: unclassified → default secret + loud error.
	//   - Nil map: classification unavailable (old hub). Distinct from "all unclassified".
	//
	// Additive wire field: old brokers ignore it (omitempty), new brokers
	// receiving from old hubs see nil and must not treat it as "all secret".
	EnvClassifications map[string]api.EnvKind `json:"envClassifications,omitempty"`
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
	// RunID is the run identity the hub minted for this create; the broker
	// labels the runtime entry with it (scion.run_id) so a later delete can
	// target exactly this run (ptone/scion#2550). Empty for provision-only
	// requests, which create no runtime entry.
	RunID string `json:"runId,omitempty"`
	// ProvisionOnly indicates the agent should be provisioned (dirs, worktree, templates)
	// but not started. The container will not be launched.
	ProvisionOnly bool `json:"provisionOnly,omitempty"`
	// Reprovision indicates this ProvisionOnly request targets an existing
	// agent whose on-disk config should be replaced from the current
	// catalog rather than reused (`scion reincarnate`, design §3.4). See
	// runtimebroker.CreateAgentRequest.Reprovision, the wire twin this maps to.
	Reprovision bool `json:"reprovision,omitempty"`
	// SharedDirBackendChanges and AllowEmptySharedDir mirror
	// runtimebroker.CreateAgentRequest's fields of the same names. Set only
	// on a reprovision for `scion reincarnate --shared-dir-backend`.
	SharedDirBackendChanges map[string]string `json:"sharedDirBackendChanges,omitempty"`
	AllowEmptySharedDir     bool              `json:"allowEmptySharedDir,omitempty"`
	// ExpectExistingNFSWorkspace mirrors
	// runtimebroker.CreateAgentRequest.ExpectExistingNFSWorkspace: on a
	// ProvisionOnly request for an agent moved from another broker, the
	// broker confirms the workspace on its mount of the export first.
	ExpectExistingNFSWorkspace string `json:"expectExistingNfsWorkspace,omitempty"`
	// AsyncLaunch, LaunchID, LaunchTimeoutSeconds and LaunchKeepaliveSeconds
	// mirror runtimebroker.CreateAgentRequest's async launch fields. They are
	// set only by dispatchLaunching. LaunchTimeoutSeconds is the remaining
	// launch budget at send time.
	AsyncLaunch            bool   `json:"asyncLaunch,omitempty"`
	LaunchID               string `json:"launchId,omitempty"`
	LaunchTimeoutSeconds   int    `json:"launchTimeoutSeconds,omitempty"`
	LaunchKeepaliveSeconds int    `json:"launchKeepaliveSeconds,omitempty"`
	// ProjectPath is the local filesystem path to the project on the target runtime broker.
	// This is looked up from the project provider record for the target broker.
	ProjectPath string `json:"projectPath,omitempty"`
	// WorkspaceStoragePath is the GCS storage path for bootstrapped workspaces.
	// When set, the broker downloads the workspace from GCS instead of using ProjectPath.
	WorkspaceStoragePath string `json:"workspaceStoragePath,omitempty"`
	// WorkspaceStorageBucket is the GCS bucket holding WorkspaceStoragePath.
	// Brokers that predate it ignore it and use their own bucket setting.
	WorkspaceStorageBucket string `json:"workspaceStorageBucket,omitempty"`

	// GatherEnv indicates the broker should evaluate env completeness before starting.
	// If required keys are missing, the broker returns HTTP 202 with env requirements.
	GatherEnv bool `json:"gatherEnv,omitempty"`

	// AvailableAsNeededKeys lists the target key names of as_needed
	// environment-type secrets that the Hub filtered out of ResolvedSecrets
	// but could resolve in a second pass if the broker reports them as needed.
	// This lets the broker's autodetect consider these keys when selecting
	// auth type, closing the chicken-and-egg gap where autodetect only sees
	// already-resolved keys.
	AvailableAsNeededKeys []string `json:"availableAsNeededKeys,omitempty"`

	// RequiredSecrets contains declared secrets from the template config.
	// Passed to the broker so it can include them in env-gather requirements.
	RequiredSecrets []api.RequiredSecret `json:"requiredSecrets,omitempty"`

	// PreResolvedSkills carries the Hub-registry skill references the Hub
	// resolved at dispatch, authorized as the agent's creator (#1784). The
	// broker installs these without calling the Hub's resolve endpoint (which
	// denies broker identities) and resolves only what is not covered here.
	PreResolvedSkills *ResolveSkillsResponse `json:"preResolvedSkills,omitempty"`

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

	// SharedWorkspaceClone carries a shared-plain git project's workspace
	// clone settings (URL, default branch, full depth). Unlike GitClone it
	// never makes the broker clone into a per-agent workspace: only the
	// Kubernetes runtime uses it, to clone into an NFS-backed shared
	// workspace from the workspace-provision init container. A broker that
	// does not know the field ignores it.
	SharedWorkspaceClone *api.GitCloneConfig `json:"sharedWorkspaceClone,omitempty"`

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
// agent_defaults, plus the auto-expose-ports default, from the hub to a
// runtime broker.
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
	MaxTurns        int               `json:"maxTurns,omitempty"`
	MaxModelCalls   int               `json:"maxModelCalls,omitempty"`
	MaxDuration     string            `json:"maxDuration,omitempty"`
	Resources       *api.ResourceSpec `json:"resources,omitempty"`
	AutoExposePorts *bool             `json:"autoExposePorts,omitempty"`
	Experiments     []string          `json:"experiments,omitempty"`
}

// RemoteGCPIdentityConfig holds GCP identity configuration sent from Hub to Broker.
type RemoteGCPIdentityConfig struct {
	MetadataMode string `json:"metadata_mode"`        // "block", "passthrough", "assign"
	SAEmail      string `json:"sa_email,omitempty"`   // Service account email
	ProjectID    string `json:"project_id,omitempty"` // GCP project ID

	// RequireLocalRuntime carries store.GCPIdentityConfig.RequireLocalRuntime
	// across the wire — see that field's doc comment. The JSON tag must stay
	// in sync with runtimebroker.GCPIdentityConfig's own field of the same
	// name; the two types are decoded independently (no shared Go type), so
	// nothing but the tag keeps them in step.
	RequireLocalRuntime bool `json:"require_local_runtime,omitempty"`
}

// RemoteAgentResponse is the response from creating an agent on a remote runtime broker.
type RemoteAgentResponse struct {
	Agent   *RemoteAgentInfo `json:"agent,omitempty"`
	Created bool             `json:"created"`

	// Reprovisioned mirrors runtimebroker.CreateAgentResponse.Reprovisioned:
	// set by the broker ONLY on the branch that actually ran
	// Manager.Reprovision (design §3.4 Amendment A2.2(a)). dispatchProvision treats a
	// reprovision dispatch whose final response lacks this as a failure —
	// an old broker has no such field and silently ran a plain Provision
	// instead, which must not be reported as reincarnate success.
	Reprovisioned bool `json:"reprovisioned,omitempty"`

	// SharedDirBackendsChanged mirrors
	// runtimebroker.CreateAgentResponse.SharedDirBackendsChanged.
	// dispatchProvision fails a reprovision that asked for a shared dir
	// backend change when the broker does not confirm it.
	SharedDirBackendsChanged bool `json:"sharedDirBackendsChanged,omitempty"`

	// LaunchPending, LaunchID and LaunchInstanceID mirror
	// runtimebroker.CreateAgentResponse's async launch echo. LaunchPending
	// with a LaunchID equal to the request's means the broker accepted the
	// create for asynchronous launch; LaunchInstanceID is the broker process
	// that owns it.
	LaunchPending    bool   `json:"launchPending,omitempty"`
	LaunchID         string `json:"launchId,omitempty"`
	LaunchInstanceID string `json:"launchInstanceId,omitempty"`
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
	// Warnings mirrors runtimebroker.AgentResponse.Warnings: the broker's
	// hub-only env drop warnings (for example a broker-local TZ that was
	// ignored). Older brokers omit it.
	Warnings []string `json:"warnings,omitempty"`
	// RunID mirrors runtimebroker.AgentResponse.RunID: the run identity of
	// the runtime entry the broker created or found (ptone/scion#2550).
	// Older brokers omit it.
	RunID string `json:"runId,omitempty"`
	// HarnessConfigSource mirrors runtimebroker.AgentResponse.HarnessConfigSource:
	// which resolution branch supplied the harness-config (hub-hydrated,
	// template-bundled, broker-local, builtin, unresolved). Provenance only
	// (ptone/scion#620). Older brokers omit it.
	HarnessConfigSource string `json:"harnessConfigSource,omitempty"`
	// WorkspacePlacement mirrors runtimebroker.AgentResponse.WorkspacePlacement:
	// where the start this answers placed the agent's workspace. Empty
	// when no start resolved it (provision-only, older brokers).
	WorkspacePlacement string `json:"workspacePlacement,omitempty"`
}

// Server is the Hub API HTTP server.
type Server struct {
	config ServerConfig
	// startupHubName is the name resolved at startup (ServerConfig.HubName,
	// from LoadGlobalConfig(serverConfigPath), else the hostname).
	// ApplySnapshot returns to it when the configured hub_name is unset.
	// See startupHubNameOrDefault. In file mode a name removed from
	// settings.yaml therefore stays in use until restart (ptone/scion#3070).
	startupHubName     string
	store              store.Store
	httpServer         *http.Server
	mux                *http.ServeMux
	mu                 sync.RWMutex
	startTime          time.Time
	dispatcher         AgentDispatcher      // Optional dispatcher for co-located runtime broker
	storage            storage.Storage      // Optional storage backend for templates
	secretBackend      secret.SecretBackend // Optional secret backend
	agentTokenService  *AgentTokenService   // Agent JWT token service
	userTokenService   *UserTokenService    // User JWT token service
	downloadSigningKey []byte               // HMAC key for skill file capability URLs (#1792)

	// chatSpacesBatch sets the GET /chat/spaces rollup batch sizes; the
	// zero value uses the defaults (handlers_chat_v2.go).
	chatSpacesBatch chatSpacesBatchSizes

	// Conduit stream grant key ring cache (conduit_grants.go); created on
	// first use behind the hub.conduit experiment.
	conduitGrantsOnce sync.Once
	conduitGrants     *conduitGrantKeys
	// conduit is the in-process conduit relay (conduit_relay.go); nil
	// unless hub.conduit was on at startup.
	conduit                atomic.Pointer[conduitRuntime]
	listCursorSealer       *listCursorSealer       // AEAD sealer for authorizedList's opaque pagination cursors (ptone/scion#2124)
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
	encryptionKey    []byte            // AES-256 key for encrypting backup secrets; nil disables encryption
	embeddedBrokerID string            // Broker ID when running in hub+broker combo mode
	// embeddedBrokerPending is non-nil while a co-located broker is expected
	// (ExpectEmbeddedBroker) but has not yet registered; it is closed when
	// registration succeeds or fails. embeddedBrokerRegErr records a failed
	// co-located registration so callers can report it distinctly.
	embeddedBrokerPending chan struct{}
	embeddedBrokerRegErr  string
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

	// userScopedDataSweepDone is closed when the startup sweep of deleted
	// users' user-scope data ends (startUserScopedDataSweep).
	userScopedDataSweepDone <-chan struct{}

	// decisionAuditWriter is the buffered decision audit writer wired into
	// authzService. CleanupResources does not close it: it runs before
	// the HTTP drain, and requests still being served then emit records.
	// Shutdown closes it after the HTTP drain, unless
	// DeferDecisionAuditClose moved that to the caller.
	decisionAuditWriter        *StoreDecisionAuditEmitter
	decisionAuditCloseDeferred atomic.Bool

	// githubWebhookNoSecretWarnOnce ensures the "no webhook secret configured"
	// rejection is logged at most once per process, so a hub being repeatedly
	// probed on the GitHub webhook endpoint does not fill its log.
	githubWebhookNoSecretWarnOnce sync.Once

	logQueryService  logQuerier               // Cloud Logging query service (nil = disabled)
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

	// Artifact store for the artifact_* tables (pkg/artifacts) — nil = artifacts unavailable.
	artifactStore artifacts.Store

	// Chat notifier for human mention + DM received notifications (W6). Nil-safe.
	chatNotifier *ChatNotifier

	// Attachment file store for chat attachments (W7). Nil = attachments disabled.
	// HA limitation: LocalDiskAttachmentStore is single-node only; see attachments.go.
	attachmentStore AttachmentStore

	// Presence manager for in-memory user presence tracking (nil = disabled).
	// Single-node only; see design §4.5 HA limitation.
	presenceManager *PresenceManager

	// Quota enforcement service (Permissions Phase 2B). Nil-safe — callers
	// must nil-check before invoking; nil means quota enforcement is disabled.
	quotaService *QuotaService

	// B3-B6 boundary services — nil-safe; nil means boundary features are disabled.
	previewService      *PreviewService      // B3 preview engine
	governanceService   *GovernanceService   // B5 transactional governance
	capabilitiesService *CapabilitiesService // B6 capabilities computation

	// RS1: Bounded domain service for project membership and ownership mutations.
	membershipService *ProjectMembershipService

	// RS3: Bounded domain service for project deletion.
	deletionService *ProjectDeletionService

	// Per-sender token-bucket limiter for the chat send paths (#1054).
	// Set once in New and read without the lock; nil-safe.
	chatSendLimiter *chatSendLimiter

	// Per-pair sliding-window limiter for agent @mention fan-out loop/storm
	// protection. Set once in New and read without the lock; nil-safe.
	mentionPairLimiter *mentionPairLimiter

	// Independent token-bucket limiters for the agent-keys operation
	// (task 2.2, .design/agent-keys-contract.md "Concrete defaults"):
	// keysPrincipalLimiter is keyed per authenticated principal+project,
	// keysTargetLimiter is keyed per target agent. Both must allow a
	// request; they are separate from chatSendLimiter's aggregate DM
	// allowance (keys must not charge or evade it) and are shared by the
	// /keys routes. Set once in New and read without the lock; nil-safe.
	// In-memory and per-Hub instance, not a distributed quota service (contract §5): N Hub
	// replicas behind a load balancer allow N times the configured rate in
	// aggregate, and a Hub restart resets both buckets to full.
	keysPrincipalLimiter *keysRateLimiter
	keysTargetLimiter    *keysRateLimiter

	// In-memory idempotency cache for chat message sends (#1055).
	// Keyed by senderID:idempotencyKey with a 5-minute TTL.
	chatIdempotency *ChatIdempotencyCache

	// Channel registry for external notification delivery (nil = disabled)
	channelRegistry *ChannelRegistry

	// Transport token minter for agent outbound auth (nil = transport auth disabled)
	transportMinter   TransportTokenMinter
	transportAudience string
	transportMode     string

	// platformAuthSA is the configured platform/transport auth service
	// account email, if any. The hub does not create or authenticate user
	// accounts for this identity (see isReservedPlatformIdentity). Empty
	// when no transport service account is configured, which leaves that
	// check inert.
	platformAuthSA string

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

	// gs:// link fetch endpoint (/api/v1/gcs/object; no per-SA client cache —
	// a fresh token and client are minted/built per request).
	// gcsLinkBaseTransport is the one shared, credential-free base transport
	// every per-request client's oauth2.Transport wraps.
	// gcsLinkRateLimiter enforces the per-viewer rate limit; gcsLinkSem is
	// the process-wide concurrency semaphore. gcsLinkSourceFactory overrides
	// gcsObjectSourceFor in tests (nil in production).
	// gcsLinkRequestDeadlineOverride shortens the whole-request deadline
	// (gcsLinkRequestDeadline, which also bounds the mint and the response
	// write — see handleGCSObject steps 10 and 13) in tests; zero in
	// production, meaning "use the constant". gcsLinkEndpointOverride points
	// gcsObjectSourceFor's *storage.Client at a fake GCS server in tests;
	// empty in production, meaning "use the real GCS endpoint". The feature
	// itself is gated on both the web.gcs_links experiment and
	// gcpTokenGenerator != nil (step 1).
	gcsLinkBaseTransport           *http.Transport
	gcsLinkRateLimiter             *GCPTokenRateLimiter
	gcsLinkSem                     chan struct{}
	gcsLinkSourceFactory           gcsSourceFactory
	gcsLinkRequestDeadlineOverride time.Duration
	gcsLinkEndpointOverride        string

	// Database connection-pool / notify metrics recorder (P0-5). Defaults to a
	// disabled no-op recorder; SetDBMetrics wires a real exporter. Drives the
	// connection-pool sampler started in StartBackgroundServices.
	dbMetrics dbmetrics.Recorder

	// Broker dispatch metrics recorder (B5-2). Defaults to a disabled no-op
	// recorder; SetDispatchMetrics wires a real exporter.
	dispatchMetrics dispatchmetrics.Recorder

	// Launch reaper metrics recorder (design §3.7): tick-outcome counter,
	// row-error counter, disarmed-time gauge. Nil until SetReaperMetrics is
	// called; the reaper tick handler nil-checks before recording, matching
	// dbMetrics/dispatchMetrics.
	reaperMetrics reapermetrics.Recorder

	// stopPoolSampler stops the DB pool-stats sampling goroutine on shutdown.
	stopPoolSampler func()

	// Message broker proxy for pub/sub message routing (nil = disabled)
	messageBrokerProxy *MessageBrokerProxy

	// User last-seen activity tracker (nil = disabled)
	userActivity *UserActivityTracker

	// operationalSettings manages Layer-1 settings from the DB on every DB
	// driver (SQLite included, since #1432). Nil only when the hub has no
	// DB-backed settings, e.g. tests that build a bare Server.
	// Uses atomic.Pointer for safe concurrent access — Phase 4/5 will add
	// request-path readers while Set is called during startup.
	operationalSettings atomic.Pointer[OperationalSettings]

	// Dedicated request logger (nil = disabled)
	requestLogger *slog.Logger

	// Dedicated message logger for message audit trail (nil = uses messageLog fallback)
	dedicatedMessageLog *slog.Logger

	// missingAgents and lifecycleOps back the heartbeat missing-container
	// reconcile (broker_heartbeat_reconcile.go). Zero values are ready to use.
	missingAgents missingAgentTracker
	lifecycleOps  lifecycleOpTracker

	// lifecycleTxHooks holds the agent lifecycle transaction hooks
	// (agent_lifecycle_tx.go). The zero value has no hooks and is ready to use.
	lifecycleTxHooks agentLifecycleHooks

	// startClaimCfg holds the current start-claim settings (see
	// start_claim_settings.go); set at New and by ApplySnapshot.
	startClaimCfg atomic.Pointer[StartClaimSettings]
	// startClaimsOn turns start claims on (start_claim.go); New sets it.
	startClaimsOn bool
	// startClaimTestHook, when set, adjusts a claim run before its renewal
	// starts (tests only).
	startClaimTestHook func(*startClaimRun)
	// claimStops records when the start-claim reaper last stopped an
	// agent's container (agent ID -> time.Time), to rate-limit it.
	claimStops sync.Map
	// intentStops records when the hub last stopped an agent that ran with
	// run intent stopped (agent ID -> time.Time), to rate-limit it.
	intentStops sync.Map
	// httpDrains marks brokers with a heartbeat-triggered drain of queued
	// stops running on this node (http_broker_drain.go).
	httpDrains sync.Map

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

	// demotionSafe is set to true when ReconcileSuperAdminBindings completes
	// with a non-empty intended admin set. Zero value (false) means "do NOT
	// demote" — every early return, error path, or "reconciler never ran" case
	// fails closed. The login path (getUserRole) checks this before demoting.
	demotionSafe atomic.Bool

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

	// ghResolveFlight coalesces concurrent resolveGitHubSkill calls that
	// share a cache key (see resolveGitHubSkill), so a burst of creates
	// hitting a cold or just-expired entry for the same ref makes one
	// mint+commits+contents+Put sequence instead of one per caller. Zero
	// value is ready to use.
	ghResolveFlight singleflight.Group

	// ghRefreshFailMu guards ghLastRefreshFailure, which records the last
	// time a background stale-refresh failed for a given cache key (see
	// refreshGitHubSkillInBackground and ghRefreshFailureBackoff).
	ghRefreshFailMu      sync.Mutex
	ghLastRefreshFailure map[string]time.Time

	// ghFailures remembers, per cache key, that GitHub recently reported a
	// gh:// ref as not found (see rememberGHNotFound and resolveGitHubSkill).
	// It is in memory only, per hub process, and never written to the store.
	ghFailures agent.FailureMemo

	// ghCooldown holds gh:// resolution requests back per credential
	// identity after a GitHub rate-limit response (see agent.GitHubCooldown).
	// Nil means the process-wide agent.SharedGitHubCooldown, the same
	// tracker the broker-side resolver uses; tests set their own.
	ghCooldown *agent.GitHubCooldown

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

	// GE Google credential exchange service (nil = exchange disabled).
	geExchangeService *GEExchangeService
	// GE exchange endpoint rate limiter (per-client-IP token bucket).
	geExchangeRateLimiter *geExchangeRateLimiter
	// GE exchange outcome counter (the ge_exchange.requests
	// logical counter). nil disables recording; see handleGEGoogleExchange
	// and SetGEExchangeMetrics.
	geExchangeMetrics GEExchangeMetricsRecorder
	// externalBearerSnapshot is the always-on, in-process recorder for the
	// three external-bearer/cache/exchange counters, wired as their default (see New) and
	// served on GET /metrics regardless of GCP export configuration. Never
	// nil after New.
	externalBearerSnapshot *ExternalBearerSnapshotMetrics
	// External-bearer path rate limiter (per-client-IP token bucket,
	// auth_external_bearer.go). Also assigned to authConfig.ExternalBearerLimiter;
	// kept here too so Start can run its cleanup goroutine, the same way
	// geExchangeRateLimiter's is started below.
	externalBearerRateLimiter *externalBearerRateLimiter

	// experiments is the compiled feature-flag registry (pkg/experiments).
	// Nil in production and in most tests; always read through the
	// nil-safe experimentRegistry() accessor, never directly.
	experiments *experiments.Registry

	// reincarnationRequesterResolveTimeout bounds
	// buildReincarnationRequesterContext's store calls (design Amendment
	// A26.14/A26.18). Zero means use defaultReincarnationRequesterResolveTimeout;
	// always read through the requesterResolveTimeout() accessor, never
	// directly. Per-Server rather than a package var so a test can shorten
	// it on its own Server instance without racing other tests' workers.
	reincarnationRequesterResolveTimeout time.Duration
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

// cloudLogQueryProjectID returns the GCP project New() builds the Cloud
// Logging query service for, or "" when that service must not be built:
// cfg.DisableCloudLogQuery is set, or no project ID is found in the
// environment (logging.ResolveProjectID).
func cloudLogQueryProjectID(cfg ServerConfig) string {
	if cfg.DisableCloudLogQuery {
		return ""
	}
	return logging.ResolveProjectID()
}

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
	const minLaunchTimeout = 30 * time.Second
	if cfg.LaunchTimeout < minLaunchTimeout {
		if cfg.LaunchTimeout != 0 {
			slog.Warn("launch_timeout below minimum 30s, using default",
				"configured", cfg.LaunchTimeout, "default", defaults.LaunchTimeout)
		}
		cfg.LaunchTimeout = defaults.LaunchTimeout
	}
	if cfg.LaunchKeepaliveSeconds <= 0 {
		cfg.LaunchKeepaliveSeconds = defaults.LaunchKeepaliveSeconds
	}
	if cfg.MissingAgentGrace < MinMissingAgentGrace {
		if cfg.MissingAgentGrace != 0 {
			slog.Warn("missing_agent_grace below minimum 1m, using default",
				"configured", cfg.MissingAgentGrace, "default", defaults.MissingAgentGrace)
		}
		cfg.MissingAgentGrace = defaults.MissingAgentGrace
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
	// The startup-resolved hub name, which ApplySnapshot returns to when
	// the configured hub_name is unset.
	srv.startupHubName = cfg.HubName
	srv.setStartClaimSettings(cfg.StartClaim)
	// Every start trigger runs under a start claim.
	srv.startClaimsOn = true

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

	// Derive AES-256 encryption key for encrypting signing key backups in
	// SQLite. This uses the same shared secret as LocalBackend so that values
	// written by backupSigningKeyToStore are encrypted at rest, closing the
	// gap where signing key material was stored in cleartext (miller79/scion#5).
	if cfg.SharedSigningSecret != "" {
		srv.encryptionKey = secret.DeriveLocalEncryptionKey(cfg.SharedSigningSecret)
	}

	// Initialize update tracker for HA integration completion detection.
	srv.updateTracker = newPendingUpdateTracker()

	// Initialize user activity tracker (throttled to once per hour per user)
	srv.userActivity = NewUserActivityTracker(s, time.Hour)

	// Initialize GCP token metrics
	srv.gcpTokenMetrics = NewGCPTokenMetrics()

	// Initialize gs:// link fetch endpoint support (shared base transport,
	// per-viewer rate limiter, global concurrency semaphore). The feature's
	// own availability gate (step 1 of handleGCSObject) checks the
	// web.gcs_links experiment and gcpTokenGenerator directly at request
	// time, so these are safe to build unconditionally even when no
	// generator is configured yet — exactly like the pre-existing GCP token
	// rate limiter below, which is likewise built regardless and simply goes
	// unused until SetGCPTokenGenerator is called.
	srv.gcsLinkBaseTransport = newGCSLinkBaseTransport()
	srv.gcsLinkRateLimiter = NewGCPTokenRateLimiter(float64(gcsLinkRateLimitPerMinute)/60.0, gcsLinkRateLimitPerMinute)
	srv.gcsLinkSem = make(chan struct{}, gcsLinkGlobalConcurrency)

	// Initialize quota enforcement service (Permissions Phase 2B).
	// limitOverride wires in the ptone/scion#2061 P2 per-broker settings
	// override (design.md §5.2): see brokerSettingLimitOverride.
	srv.quotaService = &QuotaService{
		store:  s,
		logger: slog.Default().With("component", "quota"),
		enforced: func(limitName string) bool {
			return limitName != store.LimitMaxAgentsPerBroker || srv.brokerQuotasEnforced()
		},
		limitOverride: srv.brokerSettingLimitOverride,
	}

	// Per-sender chat send rate limiter (#1054).
	srv.chatSendLimiter = newChatSendLimiter()
	srv.chatIdempotency = NewChatIdempotencyCache()

	// Per-pair agent mention loop/storm protection.
	srv.mentionPairLimiter = newMentionPairLimiter()

	// Agent-keys admission rate limiters (task 2.2, contract "Concrete
	// defaults"): 5 req/s burst 10 per principal+project, 10 req/s burst 20
	// per target.
	srv.keysPrincipalLimiter = newKeysRateLimiter(agentkeys.PrincipalProjectRateLimit, agentkeys.PrincipalProjectBurst)
	srv.keysTargetLimiter = newKeysRateLimiter(agentkeys.TargetRateLimit, agentkeys.TargetBurst)

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
		// Wire credential recorder so issued tokens are persisted for revocation.
		credAdapter := &storeCredentialRecorder{store: s}
		tokenService.SetCredentialRecorder(credAdapter)
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

	// Initialize the dedicated download-URL signing key (#1792).
	if err := srv.initDownloadSigningKey(ctx); err != nil {
		return nil, err
	}

	// Initialize the dedicated authorized-list cursor sealing key
	// (ptone/scion#2124).
	if err := srv.initListCursorSealer(ctx); err != nil {
		return nil, err
	}

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

	// Copied unconditionally (not gated on transport mode being enabled) so
	// provisionUser and the Google identity resolver can refuse the
	// configured platform/transport auth service account regardless of
	// whether transport minting itself is active. Empty when unset, which
	// isReservedPlatformIdentity treats as inert.
	srv.platformAuthSA = cfg.PlatformAuthSA

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
			EncryptionKey:           srv.encryptionKey,
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
	// See handleBrokerDisconnect for the CAS/race rationale (issue #131).
	srv.controlChannel.SetOnDisconnect(func(brokerID, sessionID string) {
		srv.handleBrokerDisconnect(context.Background(), brokerID, sessionID)
	})
	slog.Info("Control channel manager initialized")

	// Initialize authorization service
	srv.authzService = NewAuthzService(s, logging.Subsystem("hub.auth"))
	// ptone/scion#2342 (B.3 R6): the same condition that enables dev-token
	// acceptance and DevUserID seeding below (cfg.DevAuthToken != "") also
	// gates whether this server currently admits dev_local authority at
	// all. See devLocalAuthorityEnabled's doc comment (devauth.go).
	srv.authzService.setDevLocalAuthorityEnabled(cfg.DevAuthToken != "")
	srv.authzService.mintDevAuthOverride = cfg.DevAuthToken != ""

	// Wire decision audit emitter
	auditEmitter := NewStoreDecisionAuditEmitter(s, logging.Subsystem("hub.decision-audit"))
	srv.decisionAuditWriter = auditEmitter
	srv.authzService.SetDecisionAuditEmitter(auditEmitter)

	// Initialize B3-B6 boundary services (preview, governance, capabilities).
	srv.initBoundaryServices()

	// RS1: Initialize the project membership domain service.
	srv.membershipService = NewProjectMembershipService(
		s, srv.authzService,
		logging.Subsystem("hub.membership"),
	)

	// RS3: Initialize the project deletion domain service.
	srv.deletionService = NewProjectDeletionService(
		s, srv.authzService,
		logging.Subsystem("hub.project-deletion"),
	)

	// RS4: Initialize user access token service with authorization, audit, and
	// transactional support via the bounded domain service pattern.
	srv.uatService = NewUserAccessTokenService(s, srv.authzService, logging.Subsystem("hub.uat"))

	// RS1 R2-R2: Run one-binding migration before accepting traffic.
	// Idempotent — safe to re-run on every startup. Consolidates legacy
	// multi-role bindings (keeps highest authority) so the D4 one-binding
	// invariant holds before constraint enforcement begins.
	if err := srv.runMembershipMigration(ctx); err != nil {
		// Fail closed: if migration fails, the server should not start with
		// potentially inconsistent binding state.
		return nil, fmt.Errorf("membership migration failed (fail-closed): %w", err)
	}

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

	// PG1: Reconcile built-in role definitions with curated, versioned
	// permission lists. Roles are created if missing, or updated if the
	// code revision is higher than the stored revision.
	// Must run BEFORE seedDefaultGroupsAndBindings so the hub-member role exists.
	reconcileBuiltInRoles(ctx, s)

	// PG1: Seed the hub-members group and a system-scoped RoleBinding of the
	// hub-member role to that group. This replaces ~13 individual seeded policies
	// and the hub-member-create-projects policy with a single role binding.
	seedDefaultGroupsAndBindings(ctx, s)

	// Seed system limit definitions for the quota/limits subsystem (Phase 2B).
	// Shipped with unlimited defaults (DefaultValue=0) per sponsor decision OQ-2.
	seedLimitDefinitions(ctx, s)

	// Remove group memberships whose user and agent were both deleted
	// (ON DELETE SET NULL leaves the row with both IDs NULL). Such rows are
	// always orphans and would otherwise count toward group roles
	// (ptone/scion#2769). Idempotent; runs on every startup, before the
	// role-binding backfill reads group memberships. Non-fatal.
	sweepOrphanedGroupMemberships(ctx, s)

	// Backfill role bindings from existing User.Role and project group memberships.
	// Must run after reconcileBuiltInRoles so the role definitions exist.
	// Members receive hub-member permissions via the canonical Hub Members group,
	// not via direct role bindings.
	if err := BackfillRoleBindings(ctx, s); err != nil {
		slog.Warn("failed to backfill role bindings", "error", err)
	}

	// Clean up redundant direct user→hub-member system-scope bindings for users
	// who already have hub-member permissions via the canonical Hub Members group.
	// Only system-created bindings (system-backfill / system-reconcile sentinels)
	// are deleted; administrator-created direct bindings are preserved.
	if err := CleanupRedundantHubMemberBindings(ctx, s); err != nil {
		slog.Warn("failed to clean up redundant hub-member bindings", "error", err)
	}

	// Reconcile super-admin bindings: ensure User.Role == "admin" and
	// system-scoped super-admin role bindings are consistent (Phase 1F).
	// Must run after BackfillRoleBindings.
	// D11: pass AdminEmails to enable bidirectional convergence (demotion).
	// Revocation latency: takes effect on this restart; documented in commit.
	if demotionSafe, err := ReconcileSuperAdminBindings(ctx, s, cfg.AdminEmails, cfg.DefaultUserRole); err != nil {
		slog.Error("failed to reconcile super-admin bindings — revocation may be incomplete", "error", err)
	} else {
		srv.demotionSafe.Store(demotionSafe)
	}

	// Seed the dev user when dev-auth is enabled so that Ent FK constraints
	// on owner_id are satisfied when the dev user creates projects/groups.
	if cfg.DevAuthToken != "" {
		seedDevUser(ctx, s, cfg.DevUserConfig)
	}

	// Remove user-scope secrets and env vars whose user no longer exists
	// (ptone/scion#2769). Runs after seedDevUser so the dev user exists. The
	// whole sweep, lookup and removal, runs in the background under one time
	// budget and is non-fatal; see startUserScopedDataSweep.
	srv.userScopedDataSweepDone = srv.startUserScopedDataSweep(srv.ctx)

	// Seed platform skills into hub_settings["injected_skills"].system (idempotent).
	// Runs on every startup so that the system list is always in sync with the binary.
	if err := srv.seedPlatformSkillInsertions(ctx, resources.PlatformSkillsFS()); err != nil {
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

	// Same shape, for `scion reincarnate` (design §3.7): a reincarnation
	// record and its agent's reincarnation_state left non-terminal past the
	// staleness bound can only mean the hub replica running it is gone —
	// claim-then-create order means a failed request never leaves one behind
	// otherwise. Without this sweep those agents would be stuck behind a
	// permanent 409 forever (Phase 3 owns actually resuming them). Also
	// registered as a recurring singleton job below, so a restart is not the
	// only trigger.
	if n, err := srv.sweepStaleReincarnations(ctx); err != nil {
		slog.Warn("Failed to sweep stale reincarnations", "error", err)
	} else if n > 0 {
		slog.Info("Marked stale reincarnations failed after restart", "count", n)
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
		CredentialStore:    s,
		UserStore:          s,
		AuthMode:           cfg.AuthMode,
		Debug:              cfg.Debug,
		Logger:             srv.authLog,
		// Sourced from srv.platformAuthSA (set above from cfg.PlatformAuthSA),
		// not cfg.PlatformAuthSA directly, so this and Server.platformAuthSA
		// can never diverge.
		PlatformAuthSA: srv.platformAuthSA,
	}
	// Wire the proxy user provisioner (wraps provisionUser with 60s cache)
	if cfg.ProxyAuth != nil {
		srv.authConfig.ProxyUserProvisioner = MakeProxyUserProvisioner(srv)
	}

	// Initialize Cloud Logging query service (optional, gated on GCP project
	// ID and on cfg.DisableCloudLogQuery)
	if projectID := cloudLogQueryProjectID(cfg); projectID != "" {
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

	// Initialize the shared Google identity verification stack (base
	// validator + resolver) once, unconditionally. The validator is lazy — it
	// does no network I/O until ValidateIDToken/ValidateAccessToken is first
	// called — and googleTrust (auth_external_bearer.go) is the single
	// request-time source of truth for whether the external-bearer path is
	// actually reachable. Building this unconditionally means Google trust
	// added later via hot reload takes effect without a restart, and keeps
	// exactly one validator/resolver instance shared between the GE exchange
	// endpoint and the external-bearer path.
	googleValidator := NewGoogleCredentialValidator(nil)
	googleResolver := NewGoogleIdentityResolver(
		s,                    // store.Store embeds UserStore
		s,                    // store.Store embeds ExternalIdentityStore (ent-backed, durable)
		srv.isUserAuthorized, // same domain/invite/allow-registration policy as web login
		func(ctx context.Context, email string) string {
			return srv.getUserRole(ctx, email, "", "")
		},
		slog.Default(),
	)
	// See platformAuthSA above: shared by the GE exchange endpoint and the
	// external-bearer path, both of which resolve through this instance.
	googleResolver.SetPlatformAuthSA(cfg.PlatformAuthSA)
	// Give the resolver's existing-record-by-email branch the same
	// account-state handling (invited activation, role re-evaluation,
	// super-admin binding, grant sync, audit) that provisionUser's
	// existing-record branch uses — see signInPolicyDeps / SetSignInPolicyDeps.
	googleResolver.SetSignInPolicyDeps(srv.signInPolicyDeps())
	// The external-bearer path (unlike the exchange endpoint) re-validates on
	// every request, so it gets a caching decorator in front of the shared
	// base validator. The exchange endpoint below is
	// deliberately wired to the undecorated googleValidator, not this one:
	// exchange behaviour must not change, and it already
	// mints a short-lived (default 60s) Hub token per successful exchange
	// rather than re-verifying the Google credential on every downstream
	// call, so it has neither the request-per-request cost the cache exists
	// to amortize nor a need to share cache staleness characteristics with
	// this path. The resolver (identity -> Hub user, including suspension
	// enforcement, which has no cache) is still the same shared instance
	// either way.
	srv.authConfig.GoogleValidator = NewCachingGoogleCredentialValidator(googleValidator)
	srv.authConfig.GoogleResolver = googleResolver
	// The atomic.Pointer box, not a recorder, is what must exist here:
	// SetExternalBearerMetrics stores into this same box later, once an OTel
	// MeterProvider exists (see AuthConfig.ExternalBearerMetrics for why a
	// plain field would also work given today's call order, and why the box
	// is used anyway).
	srv.authConfig.ExternalBearerMetrics = &atomic.Pointer[ExternalBearerMetricsRecorder]{}

	// The in-process counters (external-bearer outcome, cache
	// result, exchange outcome) are always constructed and wired as the
	// default recorder for all three, regardless of GCP export
	// configuration: the exchange-deletion soak gate must not depend on
	// cfg.Hub.GCPProjectID being set. cmd/server_foreground.go later wires an
	// OTel-backed recorder that dual-writes into this same instance, so
	// GET /metrics keeps counting the same totals across that switch.
	srv.externalBearerSnapshot = NewExternalBearerSnapshotMetrics()
	var defaultExtBearerMetrics ExternalBearerMetricsRecorder = srv.externalBearerSnapshot
	srv.authConfig.ExternalBearerMetrics.Store(&defaultExtBearerMetrics)
	if setter, ok := srv.authConfig.GoogleValidator.(interface {
		SetMetrics(GoogleValidatorCacheMetricsRecorder)
	}); ok {
		setter.SetMetrics(srv.externalBearerSnapshot)
	}
	srv.geExchangeMetrics = srv.externalBearerSnapshot

	// Kept on Server (not just authConfig) so Start can run its cleanup
	// goroutine below, the same way geExchangeRateLimiter's is started.
	// Without a running cleanup, the bounded bucket map fills permanently
	// after maxEntries distinct client IPs and fails closed for every new
	// one.
	srv.externalBearerRateLimiter = newExternalBearerRateLimiter(cfg.TrustedProxies)
	srv.authConfig.ExternalBearerLimiter = srv.externalBearerRateLimiter

	// Initialize GE Google credential exchange service, sharing the validator
	// and resolver above so both mechanisms produce identical decisions
	// during the exchange-to-external-bearer soak.
	if cfg.GEGoogleExchange.IsValid() {
		srv.geExchangeService = NewGEExchangeService(
			cfg.GEGoogleExchange,
			googleValidator,
			srv.userTokenService,
			googleResolver,
			slog.Default(),
		)
		srv.geExchangeRateLimiter = newGEExchangeRateLimiter()
		slog.Info("GE Google exchange service initialized",
			"allowed_client_ids", len(cfg.GEGoogleExchange.AllowedClientIDs))
	}

	srv.experiments = cfg.Experiments

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
		// Hub-scope signing keys created before ptone/scion#2152 only exist
		// under the legacy (pre hub-prefix) GCP SM name. Copy the value
		// forward to the current hub-prefixed name so it is reachable once an
		// operator narrows IAM to the new prefix, without waiting for an
		// explicit `migrate-names` run — losing a signing key invalidates
		// every live session/agent token, so this can't wait on an operator's
		// schedule the way ordinary secrets can. Idempotent; the legacy
		// secret is left in place. Best-effort: a failure here just means the
		// existing (legacy-ref) resolution below is used instead.
		if gcpBackend, ok := s.secretBackend.(*secret.GCPBackend); ok {
			if copyErr := gcpBackend.CopyHubSecretForward(ctx, keyName); copyErr != nil && copyErr != store.ErrNotFound {
				slog.Warn("Failed to copy hub signing key forward to hub-prefixed GCP SM name", "key", keyName, "error", copyErr)
			}
		}

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
			// The stored value may be AES-256-GCM encrypted (enc:v1: prefix).
			// Decrypt transparently; legacy plaintext passes through as-is.
			if s.encryptionKey != nil {
				plaintext, _, decErr := secret.DecryptValue(val, s.encryptionKey)
				if decErr != nil {
					return nil, fmt.Errorf("failed to decrypt signing key %s from store: %w", keyName, decErr)
				}
				val = plaintext
			}
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
			// When a secret backend is configured, read through it so that
			// encrypted-at-rest values are decrypted transparently. Fall
			// back to the raw store for configurations without a backend.
			var val string
			if hasSecretBackend {
				sv, getErr := s.secretBackend.Get(ctx, keyName, store.ScopeHub, legacyScopeID)
				if getErr == nil {
					val = sv.Value
				}
			}
			if val == "" {
				rawVal, legacyErr := s.store.GetSecretValue(ctx, keyName, store.ScopeHub, legacyScopeID)
				if legacyErr != nil {
					continue
				}
				// Decrypt if the stored value is encrypted.
				if s.encryptionKey != nil {
					plaintext, _, decErr := secret.DecryptValue(rawVal, s.encryptionKey)
					if decErr != nil {
						slog.Warn("Failed to decrypt legacy signing key from store", "key", keyName, "legacyScopeID", legacyScopeID, "error", decErr)
						continue
					}
					rawVal = plaintext
				}
				val = rawVal
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
	// Encrypt the value before writing to SQLite so that signing key material
	// is stored at rest under AES-256-GCM, consistent with LocalBackend.Set().
	valueToStore := encodedValue
	if s.encryptionKey != nil {
		encrypted, err := secret.EncryptValue(encodedValue, s.encryptionKey)
		if err != nil {
			return fmt.Errorf("encrypting signing key backup: %w", err)
		}
		valueToStore = encrypted
	}

	existing, err := s.store.GetSecret(ctx, keyName, store.ScopeHub, hubID)
	if err == nil {
		// Record exists — update value only, preserving SecretRef and other metadata.
		existing.EncryptedValue = valueToStore
		return s.store.UpdateSecret(ctx, existing)
	}
	if err != store.ErrNotFound {
		return fmt.Errorf("checking existing secret record: %w", err)
	}
	// No existing record — create a new one.
	sec := &store.Secret{
		ID:             signingKeySecretID(keyName, hubID),
		Key:            keyName,
		EncryptedValue: valueToStore,
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
	s.resolveEmbeddedBrokerPendingLocked()
}

// ExpectEmbeddedBroker records that this process will register a co-located
// broker. Startup calls it before the Hub starts serving, because co-located
// registration (and so SetEmbeddedBrokerID) happens only after the listener
// is up. Until SetEmbeddedBrokerID or EmbeddedBrokerRegistrationFailed is
// called, waitForEmbeddedBroker blocks (bounded) instead of treating the
// broker as non-embedded. It is a no-op once an embedded broker is known.
func (s *Server) ExpectEmbeddedBroker() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.embeddedBrokerID != "" || s.embeddedBrokerPending != nil {
		return
	}
	s.embeddedBrokerPending = make(chan struct{})
}

// EmbeddedBrokerRegistrationFailed records that co-located broker
// registration failed at startup, releasing anything waiting on it. The Hub
// then has no embedded broker for the rest of the process lifetime.
func (s *Server) EmbeddedBrokerRegistrationFailed(err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err != nil {
		s.embeddedBrokerRegErr = err.Error()
	} else {
		s.embeddedBrokerRegErr = "unknown error"
	}
	s.resolveEmbeddedBrokerPendingLocked()
}

// resolveEmbeddedBrokerPendingLocked releases waiters on a pending co-located
// registration. Callers must hold s.mu.
func (s *Server) resolveEmbeddedBrokerPendingLocked() {
	if s.embeddedBrokerPending != nil {
		close(s.embeddedBrokerPending)
		s.embeddedBrokerPending = nil
	}
}

// embeddedBrokerWaitTimeout bounds how long a request waits for a pending
// co-located broker registration. A variable so tests can shorten it.
var embeddedBrokerWaitTimeout = 15 * time.Second

// embeddedBrokerState describes the Hub's embedded broker for callers that
// need to explain a negative isEmbeddedBroker result.
type embeddedBrokerState struct {
	id      string // recorded embedded broker ID, "" if none
	regErr  string // non-empty when co-located registration failed
	pending bool   // registration still outstanding after the wait
}

// waitForEmbeddedBroker returns the embedded broker state, first waiting up to
// embeddedBrokerWaitTimeout (or until ctx is done) if a co-located
// registration is still pending.
func (s *Server) waitForEmbeddedBroker(ctx context.Context) embeddedBrokerState {
	s.mu.RLock()
	pending := s.embeddedBrokerPending
	s.mu.RUnlock()
	if pending != nil {
		timer := time.NewTimer(embeddedBrokerWaitTimeout)
		defer timer.Stop()
		select {
		case <-pending:
		case <-timer.C:
		case <-ctx.Done():
		}
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	return embeddedBrokerState{
		id:      s.embeddedBrokerID,
		regErr:  s.embeddedBrokerRegErr,
		pending: s.embeddedBrokerPending != nil,
	}
}

// embeddedBrokerSnapshot returns the current embedded broker state without
// waiting for a pending co-located registration to resolve. GetHealthInfo
// (/healthz and the admin health summary) calls this instead of
// waitForEmbeddedBroker: it is polled frequently (and often with short
// client-side timeouts), so blocking up to embeddedBrokerWaitTimeout on
// every call would make a probe hitting the process during the startup race
// look like a timeout instead of the deliberate "not registered yet" status
// it should report. /readyz does not call this and is intentionally
// unaffected — see checkColocatedBrokerHealth in handlers_health.go for why
// /healthz degrades on this instead. A pending state self-corrects on the
// next poll once SetEmbeddedBrokerID or EmbeddedBrokerRegistrationFailed
// runs; a failure does not self-correct at all (no retry), so it persists
// until the broker configuration is fixed and the process is restarted.
func (s *Server) embeddedBrokerSnapshot() embeddedBrokerState {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return embeddedBrokerState{
		id:      s.embeddedBrokerID,
		regErr:  s.embeddedBrokerRegErr,
		pending: s.embeddedBrokerPending != nil,
	}
}

// SetStatelessEmbeddedBrokerID records a co-located broker whose runtime
// lifecycle operations are safe from any hub replica.
func (s *Server) SetStatelessEmbeddedBrokerID(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.embeddedBrokerID = id
	s.statelessEmbeddedBroker = id != ""
	s.resolveEmbeddedBrokerPendingLocked()
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
// server. This is called during hub startup (any DB driver) after seeding and
// initial refresh (settings-db §3.5/§3.9). Safe for concurrent use.
func (s *Server) SetOperationalSettings(ops *OperationalSettings) {
	s.operationalSettings.Store(ops)
}

// GetOperationalSettings returns the OperationalSettings service, or nil
// when the hub has no DB-backed settings. Safe for concurrent use.
func (s *Server) GetOperationalSettings() *OperationalSettings {
	return s.operationalSettings.Load()
}

// writeDenyEnabled returns whether the consolidated conversation envelope
// switch is ON (which subsumes the former write-deny behaviour).
// Safe for concurrent use. Returns false when operational settings are absent.
func (s *Server) writeDenyEnabled() bool {
	ops := s.GetOperationalSettings()
	return ops != nil && ops.ConversationEnvelopeSwitch()
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

// AdminEmails returns the current admin email list. The returned slice is a
// defensive copy — callers may iterate safely without holding s.mu.
// Implements AccessSettingsProvider.
func (s *Server) AdminEmails() []string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	result := make([]string, len(s.config.AdminEmails))
	copy(result, s.config.AdminEmails)
	return result
}

// AuthorizedDomains returns the current authorized domain list. The returned
// slice is a defensive copy. Implements AccessSettingsProvider.
func (s *Server) AuthorizedDomains() []string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	result := make([]string, len(s.config.AuthorizedDomains))
	copy(result, s.config.AuthorizedDomains)
	return result
}

// UserAccessMode returns the current user access mode. Thread-safe.
// Implements AccessSettingsProvider.
func (s *Server) UserAccessMode() string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.config.UserAccessMode
}

// DefaultUserRole returns the configured default role for new users.
// Thread-safe. Returns "member" when unconfigured.
func (s *Server) DefaultUserRole() string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.config.DefaultUserRole == "" {
		return "member"
	}
	return s.config.DefaultUserRole
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

// RS4/G13: getUATService accessor removed — no production caller exists and
// an exported accessor would be a latent bypass door around the bounded service.
// The uatService field is accessed directly within Server methods.

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

// SetReaperMetrics wires the launch reaper's metrics recorder (design §3.7).
func (s *Server) SetReaperMetrics(rec reapermetrics.Recorder) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.reaperMetrics = rec
}

// SetGCPTokenMetrics wires the GCP token metrics recorder.
func (s *Server) SetGCPTokenMetrics(m GCPTokenMetricsRecorder) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.gcpTokenMetrics = m
}

// SetDecisionAuditMetrics wires metrics into the decision audit writer. A
// nil recorder disables them. Queue depth is read separately through
// DecisionAuditQueueDepth.
func (s *Server) SetDecisionAuditMetrics(m DecisionAuditMetricsRecorder) {
	if s.decisionAuditWriter != nil {
		s.decisionAuditWriter.SetMetrics(m)
	}
}

// DecisionAuditQueueDepth reports the number of decision audit records
// queued to be written (not in-flight: records a worker is writing or
// retrying are not counted). It is the source for the queue depth gauge.
func (s *Server) DecisionAuditQueueDepth() int64 {
	if s.decisionAuditWriter == nil {
		return 0
	}
	return int64(s.decisionAuditWriter.QueueDepth())
}

// DeferDecisionAuditClose tells the Server that the caller will call
// CloseDecisionAudit itself, after every HTTP server that serves this
// Server's handler has drained. Shutdown then leaves the writer open. Use
// it when the handler is also mounted on another listener (for example
// the WebServer), so records from requests that the other listener is
// still draining are written rather than dropped.
func (s *Server) DeferDecisionAuditClose() {
	s.decisionAuditCloseDeferred.Store(true)
}

// CloseDecisionAudit drains and closes the decision audit writer. Call it
// after the HTTP servers that serve this Server have drained and before
// the store is closed. Safe to call more than once.
func (s *Server) CloseDecisionAudit(ctx context.Context) {
	if s.decisionAuditWriter != nil {
		s.decisionAuditWriter.Close(ctx)
	}
}

// SetExternalBearerMetrics wires the external-bearer authentication outcome
// counter. Unlike SetMetrics/SetDBMetrics/SetDispatchMetrics/
// SetGCPTokenMetrics above, this recorder is read from AuthConfig by the
// free-standing UnifiedAuthMiddleware closure. That closure captures a copy
// of authConfig each time applyMiddleware runs (Start(), Handler()), not
// once inside New(); cmd/server_foreground.go happens to call this setter
// before either, so a plain field would work under today's call order too.
// Storing into the *atomic.Pointer already held by
// AuthConfig.ExternalBearerMetrics — the same indirection FederationAuth
// uses for hot reload, for the identical structural reason — means every
// copy of that cfg, however many times it was captured, keeps observing the
// same box: this setter takes effect race-free even if a future caller runs
// it after Start()/Handler() already built the request-serving chain. A nil
// recorder disables counting; it never changes the external-bearer path's
// authentication outcome.
//
// If ExternalBearerMetrics is nil (a Server built without New(), which
// never allocates the box), this allocates one rather than panicking; the
// new box is only ever observed by later callers of this method, since
// UnifiedAuthMiddleware never runs on such a Server.
func (s *Server) SetExternalBearerMetrics(m ExternalBearerMetricsRecorder) {
	if s.authConfig.ExternalBearerMetrics == nil {
		s.authConfig.ExternalBearerMetrics = &atomic.Pointer[ExternalBearerMetricsRecorder]{}
	}
	s.authConfig.ExternalBearerMetrics.Store(&m)
}

// SetGoogleValidatorCacheMetrics wires the Google-credential cache counter
// into the caching decorator constructed in New(). Logs a
// warning and does nothing if the configured validator isn't (or is no
// longer) that decorator — defensive only; production always wires
// NewCachingGoogleCredentialValidator there.
func (s *Server) SetGoogleValidatorCacheMetrics(m GoogleValidatorCacheMetricsRecorder) {
	setter, ok := s.authConfig.GoogleValidator.(interface {
		SetMetrics(GoogleValidatorCacheMetricsRecorder)
	})
	if !ok {
		slog.Warn("SetGoogleValidatorCacheMetrics: configured Google validator does not support metrics wiring; cache counter stays disabled",
			"validator_type", fmt.Sprintf("%T", s.authConfig.GoogleValidator))
		return
	}
	setter.SetMetrics(m)
}

// ExternalBearerSnapshotMetrics returns the always-on, in-process recorder
// for the external-bearer/cache/exchange counters, for GET
// /metrics (handlers_health.go) and for passing into
// NewOTelExternalBearerMetrics so the OTel-backed recorder dual-writes into
// the same instance. Never nil for a Server built through New().
func (s *Server) ExternalBearerSnapshotMetrics() *ExternalBearerSnapshotMetrics {
	return s.externalBearerSnapshot
}

// SetGEExchangeMetrics wires the GE exchange outcome counter (the
// ge_exchange.requests counter; see external_bearer_metrics.go for the
// closed label set and the real exported metric name). Unlike
// ExternalBearerMetrics above, handleGEGoogleExchange reads this directly
// off *Server (it is a Server method, not a captured-by-value closure), so a
// plain field set here — the same convention SetDBMetrics/SetDispatchMetrics/
// SetGCPTokenMetrics use — needs no atomic indirection. It is still read
// without a lock while this setter writes under s.mu, so — like
// gcpTokenMetrics — callers must call this before Start, not concurrently
// with request handling; cmd/server_foreground.go does this before
// hubSrv.Start.
func (s *Server) SetGEExchangeMetrics(m GEExchangeMetricsRecorder) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.geExchangeMetrics = m
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

// GetDemotionSafe returns a pointer to the process-level demotionSafe flag so
// it can be shared with the WebServer for proxy-auth login paths.
func (s *Server) GetDemotionSafe() *atomic.Bool {
	return &s.demotionSafe
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
	nd.writeDenyEnabled = func() bool {
		ops := s.GetOperationalSettings()
		return ops != nil && ops.ConversationEnvelopeSwitch()
	}
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
	proxy.writeDenyEnabled = func() bool {
		ops := s.GetOperationalSettings()
		return ops != nil && ops.ConversationEnvelopeSwitch()
	}
	// Phase 2 D5: inject cross-project message authorization into the broker.
	// At delivery/retry time, re-evaluate authorization using the current
	// policy state (not the cached state from enqueue time).
	proxy.messageAuthorizer = func(ctx context.Context, senderID string, targetAgent *store.Agent) *MessageDecision {
		// Only reauthorize agent-to-agent messages where sender and target
		// are in different projects (cross-project). Same-project messages
		// were already authorized at enqueue time and don't need rechecking.
		senderAgent, err := s.store.GetAgent(ctx, senderID)
		if err != nil || senderAgent == nil {
			// Sender not found or nil — could be a user sender or deleted agent.
			// User messages don't need cross-project reauthorization.
			return nil
		}
		if senderAgent.ProjectID == targetAgent.ProjectID {
			// Same project — no cross-project reauthorization needed.
			return nil
		}
		// Cross-project: build an identity wrapper and evaluate.
		// We use a minimal identity from the stored agent record since the
		// original credential is not available at retry time.
		agentIdent := &storedAgentIdentity{agent: senderAgent}
		decision := s.EvaluateAgentMessage(ctx, agentIdent, targetAgent)
		return &decision
	}
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

	// Set the agent-only endpoint override, if configured. This only changes
	// what gets injected into agents as SCION_HUB_ENDPOINT; every other use
	// of s.config.HubEndpoint (invite links, chat-bridge links, OIDC issuer
	// default, cloudrun_invoker audience default) is untouched. It applies to
	// agents on every broker attached to this Hub, including remote brokers,
	// so log it at Info (not Debug-only) so operators notice it at startup.
	if s.config.AgentEndpoint != "" {
		dispatcher.SetAgentEndpoint(s.config.AgentEndpoint)
		slog.Info("server.hub.agent_endpoint is set: agents on every broker attached to this Hub, including remote brokers, will report to this URL instead of the hub's regular endpoint",
			"agent_endpoint", s.config.AgentEndpoint, "hub_endpoint", s.config.HubEndpoint)
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

	// Resolve Hub-registry skills at dispatch as the agent's creator so the
	// broker never needs to read non-public skills with its own identity (#1784).
	dispatcher.SetSkillPreResolver(s.preResolveAgentSkills)

	// Start/restart always resolve as the agent's recorded creator, so a
	// re-provision reached through either verb resolves the same set for a
	// given agent regardless of which permitted principal dispatches it
	// (ptone/scion#1994).
	dispatcher.SetCreatorSkillPreResolver(s.preResolveAgentSkillsAsCreator)

	// Wire the hub's operational agent_defaults so dispatch can carry the
	// limit/resource ones to the broker's low-precedence tier. The accessor
	// takes s.mu; it returns the zero value in file mode, where the wire field
	// is then omitted and broker behaviour is unchanged.
	dispatcher.SetHubAgentDefaultsProvider(s.hubAgentDefaults)

	// Wire the async agent launch settings. They are static after startup;
	// the accessor reads them under s.mu.
	dispatcher.SetAsyncLaunchSettingsProvider(s.asyncLaunchSettings)
	dispatcher.SetAutoExposePortsDefaultProvider(s.autoExposePortsDefault)
	dispatcher.SetDispatchExperimentsProvider(s.dispatchExperiments)

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
//
// It applies no delegation ceiling and has no production caller: every mint
// and refresh site calls GenerateAgentTokenForAgent. It serves test helpers
// (TestAllMintSitesUseCeiledHelper pins this).
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

// storeCredentialRecorder adapts store.AgentCredentialStore to CredentialRecorder.
type storeCredentialRecorder struct {
	store store.AgentCredentialStore
}

func (r *storeCredentialRecorder) RecordAgentCredential(ctx context.Context, cred *store.AgentCredential) error {
	return r.store.CreateAgentCredential(ctx, cred)
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

		// The container is stopped before phase=suspended is written; see
		// beginLifecycleOp.
		endLifecycleOp := s.beginLifecycleOp(agent.ID)
		if agent.RuntimeBrokerID != "" {
			if dispatcher == nil {
				slog.Error("Scheduler: cannot auto-suspend agent because dispatcher is nil",
					"agent_id", agent.ID, "agent_name", agent.Name)
				endLifecycleOp()
				continue
			}
		}
		supersedes := agent.StartClaimID
		priorIntent, intentAt, err := s.swapRunIntent(ctx, agent, store.RunIntentStopped)
		if err != nil {
			slog.Error("Scheduler: auto-suspend intent write failed",
				"agent_id", agent.ID, "agent_name", agent.Name, "error", err)
			endLifecycleOp()
			continue
		}
		stopRunID := agent.RunID
		if agent.RuntimeBrokerID != "" {
			s.syncWorkspaceOnStop(ctx, agent)
			if err := dispatcher.DispatchAgentStop(ctx, agent); err != nil {
				s.logStopRunMismatch(agent, "auto-suspend", err)
				slog.Error("Scheduler: auto-suspend dispatch failed",
					"agent_id", agent.ID, "agent_name", agent.Name, "error", err)
				// This stop was the system's, not the user's: if it
				// replaced a running intent, put that back unless something
				// newer replaced it. A prior stopped intent (for example a
				// user stop whose dispatch also failed) stays stopped.
				if priorIntent == store.RunIntentRunning {
					if _, rerr := s.store.RevertRunIntent(ctx, agent.ID, store.RunIntentStopped, intentAt, store.RunIntentRunning); rerr != nil {
						slog.Error("Scheduler: auto-suspend intent revert failed",
							"agent_id", agent.ID, "agent_name", agent.Name, "error", rerr)
					}
				}
				endLifecycleOp()
				continue
			}
		}
		// The superseded start claim is released last on each path below,
		// after the status write and the quota release (see suspendAgent).
		releaseClaim := func() {
			if agent.RuntimeBrokerID != "" {
				s.releaseSupersededClaim(ctx, agent.ID, supersedes, intentAt)
			}
		}

		statusUpdate := store.AgentStatusUpdate{
			Phase:           string(state.PhaseSuspended),
			ContainerStatus: "stopped",
			Activity:        "",
		}
		// Only while the row still holds the run the stop was dispatched
		// for (ptone/scion#2550).
		recorded, err := s.recordStopStatus(ctx, agent.ID, stopRunID, "auto-suspend", statusUpdate)
		endLifecycleOp()
		if err != nil {
			slog.Error("Scheduler: auto-suspend status update failed",
				"agent_id", agent.ID, "agent_name", agent.Name, "error", err)
			releaseClaim()
			continue
		}
		if !recorded {
			releaseClaim()
			continue
		}

		agent.Phase = string(state.PhaseSuspended)
		agent.ContainerStatus = "stopped"
		agent.Activity = ""
		// A suspended agent has no running container: release its
		// max_agents_per_broker reservation (ptone/scion#1963), mirroring
		// suspendAgent's HTTP-path behavior.
		s.releaseBrokerQuota(ctx, agent)
		releaseClaim()
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

// errScheduledMessageRefused is the one public refusal a scheduled message
// records when its target cannot be resolved or fire-time authorization
// refuses it. The specific cause is logged, never stored on the event.
var errScheduledMessageRefused = errors.New("scheduled_message_denied: message delivery refused")

// messageEventHandler returns an EventHandler that dispatches scheduled messages
// to agents via the AgentDispatcher.
//
// C1 containment: this handler now performs fire-time authorization via
// authorizeScheduledMessageFire before any dispatch. Scheduled messages are
// request-derived (not system-plane) and must pass the production
// authorizeAgentMessage choke point with isSystemPlane=false.
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
				"scheduledFor", evt.FireAt.UTC().Format(time.RFC3339),
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
			// The returned error is persisted as ScheduledEvent.Error, which
			// project readers can see. A missing target, a lookup failure
			// and an authorization refusal below all return the same
			// constant (errScheduledMessageRefused); the specific cause is
			// logged here.
			if errors.Is(err, store.ErrNotFound) {
				slog.Warn("Scheduler: target agent no longer exists",
					"eventID", evt.ID,
					"agentName", payload.AgentName,
					"agent_id", payload.AgentID,
					"projectID", evt.ProjectID,
					"cause", "target agent deleted")
			} else {
				slog.Warn("Scheduler: target agent lookup failed",
					"eventID", evt.ID,
					"agentName", targetName,
					"projectID", evt.ProjectID,
					"error", err)
			}
			return errScheduledMessageRefused
		}

		// ---- C1 containment: fire-time authorization ----
		// Re-resolve the creator identity and authorize the message through
		// the production choke point (authorizeAgentMessage, isSystemPlane=false).
		// Denial returns an error — the enclosing scheduler wrapper owns
		// status recording. No external effect occurs on denial.
		_, authErr := s.authorizeScheduledMessageFire(ctx, evt, agent)
		if authErr != nil {
			slog.Warn("Scheduler: scheduled message refused at fire time",
				"eventID", evt.ID,
				"agent_id", agent.ID,
				"projectID", evt.ProjectID,
				"creator", evt.CreatedBy,
				"error", authErr)
			return errScheduledMessageRefused
		}

		// Migration gate (design agent-reincarnate §3.7, R3 p2a-r1 review).
		// O-a (p2a-r2 review): checked AFTER authorization, not before —
		// same invariant deliverToAgent states explicitly: a denied creator
		// must learn nothing about the recipient's migration state. Scheduled
		// messages are not deferred (there is no sender to persist a "saved
		// to history" row for, and no request to answer 202 to) — a
		// scheduled message firing mid-`scion reincarnate` fails loudly
		// instead of dispatching into a stopped or absent container and
		// silently succeeding. The event records this as a failure so the
		// blocked-wait pairing agents rely on is not silently lost.
		if reincarnationInFlight(agent) {
			return fmt.Errorf("target agent is reincarnating")
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

		// Phase 9f: render delivery envelope for scheduler messages.
		// No persisted row and no conversation exist for scheduled
		// deliveries, so MessageID and ConvResult are honestly absent.
		if s.writeDenyEnabled() {
			var ts time.Time
			if t, err := time.Parse(time.RFC3339, structuredMsg.Timestamp); err == nil {
				ts = t
			}
			structuredMsg.DeliveryText = messaging.RenderDeliveryText(messaging.RenderDeliveryInput{
				ConvResult: nil,
				Msg:        structuredMsg,
				CreatedAt:  ts,
			})
		}

		retryCtx, retryCancel := context.WithTimeout(ctx, 30*time.Second)
		defer retryCancel()

		if err := dispatchWithBrokerRetry(retryCtx, dispatcher, agent, payload.Message, payload.Interrupt, structuredMsg); err != nil {
			return fmt.Errorf("failed to dispatch message to agent %s: %w", agent.Name, err)
		}
		// Log the recorded initiator alongside the executor context set by
		// the caller (fireEvent / executeSchedule), so a scheduled message is
		// distinguishable in logs from a live send without changing the live
		// authorization identity above (cutover rule).
		initiator := s.scheduledInitiator(evt.InitiatorAttribution)
		executor, _ := ExecutorContextFromContext(ctx)
		slog.Info("Scheduler: message delivered to agent",
			"eventID", evt.ID, "agent_id", agent.ID, "agentName", agent.Name,
			"initiator_principal_kind", initiator.PrincipalKind,
			"initiator_credential_kind", initiator.CredentialKind,
			"initiator_credential_id", initiator.CredentialID,
			"executor_kind", executor.Kind,
			"executor_id", executor.ID)
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

func (s *Server) authorizeScheduledAgentCreate(ctx context.Context, evt store.ScheduledEvent) (bool, error) {
	if evt.CreatedBy == "" {
		return false, fmt.Errorf("dispatch_agent event has no creator; cannot authorize at fire time")
	}

	if creator, err := s.store.GetAgent(ctx, evt.CreatedBy); err == nil {
		if !creator.DeletedAt.IsZero() {
			return false, fmt.Errorf("scheduled dispatch creator agent %q is deleted; cannot authorize", evt.CreatedBy)
		}
		if creator.ProjectID == "" || creator.ProjectID != evt.ProjectID {
			return false, fmt.Errorf("scheduled dispatch creator agent %q is not in project %q", evt.CreatedBy, evt.ProjectID)
		}
		if s.authzService == nil {
			return false, fmt.Errorf("scheduled dispatch cannot authorize agent creation without authz service")
		}
		role, additionalScopes := agentRoleAndScopes(creator)
		scopes := append(ScopesForRole(role), additionalScopes...)
		hasCreate := false
		for _, scope := range scopes {
			if scope == ScopeAgentCreate {
				hasCreate = true
				break
			}
		}
		if !hasCreate {
			return false, fmt.Errorf("scheduled dispatch creator agent %q missing required scope: %s", evt.CreatedBy, ScopeAgentCreate)
		}

		agentIdentity := &agentIdentityWrapper{&AgentTokenClaims{
			Claims:    jwt.Claims{Subject: creator.ID},
			ProjectID: creator.ProjectID,
			Scopes:    scopes,
		}}

		// The creator agent needs agent.create in the project through
		// Decide, including the delegation ceiling of every live ancestor.
		if decision := s.agentCreateDecision(ctx, agentIdentity, evt.ProjectID); !decision.Allowed {
			return false, fmt.Errorf("scheduled dispatch creator agent %q is not authorized to create agents in project %q: %s",
				evt.CreatedBy, evt.ProjectID, decision.Reason)
		}

		// CanDelegate check (Phase 1F): at fire time, verify the creator
		// agent still holds the scopes it would delegate to the new agent.
		grantDesc := GrantDescriptor{
			Type:      GrantTypeAgentDelegation,
			AgentRole: string(role),
			ProjectID: evt.ProjectID,
			ScopeType: store.RoleScopeProject,
			ScopeID:   evt.ProjectID,
		}
		delegateDecision := s.authzService.CanDelegate(ctx, agentIdentity, grantDesc)
		if !delegateDecision.Allowed {
			s.emitMutationAudit(ctx, &store.MutationAuditRecord{
				MutationType:       "agent_delegation",
				ActorPrincipalKind: "agent",
				ActorPrincipalID:   creator.ID,
				TargetType:         "scheduled_dispatch",
				TargetID:           evt.ID,
				CanDelegateResult:  "deny",
				CanDelegateReason:  delegateDecision.Reason,
			})
			return false, fmt.Errorf("scheduled dispatch creator agent %q failed CanDelegate: %s",
				evt.CreatedBy, delegateDecision.Reason)
		}

		return true, nil
	} else if !errors.Is(err, store.ErrNotFound) {
		return false, fmt.Errorf("failed to resolve scheduled dispatch creator agent %q: %w", evt.CreatedBy, err)
	}

	user, err := s.store.GetUser(ctx, evt.CreatedBy)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return false, fmt.Errorf("scheduled dispatch creator %q was not found", evt.CreatedBy)
		}
		return false, fmt.Errorf("failed to resolve scheduled dispatch creator user %q: %w", evt.CreatedBy, err)
	}
	if user.Status != store.UserStatusActive {
		return false, fmt.Errorf("scheduled dispatch creator user %q has status %s; cannot authorize",
			evt.CreatedBy, user.Status)
	}
	if s.authzService == nil {
		return false, fmt.Errorf("scheduled dispatch cannot authorize agent creation without authz service")
	}
	identity := NewAuthenticatedUser(user.ID, user.Email, user.DisplayName, user.Role, "scheduler")
	decision := s.agentCreateDecision(ctx, identity, evt.ProjectID)
	if !decision.Allowed {
		return false, fmt.Errorf("scheduled dispatch creator user %q is not authorized to create agents in project %q: %s",
			evt.CreatedBy, evt.ProjectID, decision.Reason)
	}

	// CanDelegate check (Phase 1F): at fire time, re-resolve the user's
	// current permissions and check they still cover the agent being dispatched.
	if s.authzService != nil {
		grantDesc := GrantDescriptor{
			Type:      GrantTypeAgentDelegation,
			AgentRole: string(AgentRoleFull), // scheduled dispatch uses the default role
			ProjectID: evt.ProjectID,
			ScopeType: store.RoleScopeProject,
			ScopeID:   evt.ProjectID,
		}
		delegateDecision := s.authzService.CanDelegate(ctx, identity, grantDesc)
		if !delegateDecision.Allowed {
			s.emitMutationAudit(ctx, &store.MutationAuditRecord{
				MutationType:       "agent_delegation",
				ActorPrincipalKind: "user",
				ActorPrincipalID:   user.ID,
				TargetType:         "scheduled_dispatch",
				TargetID:           evt.ID,
				CanDelegateResult:  "deny",
				CanDelegateReason:  delegateDecision.Reason,
			})
			return false, fmt.Errorf("scheduled dispatch creator user %q failed CanDelegate at fire time: %s",
				evt.CreatedBy, delegateDecision.Reason)
		}
	}

	return true, nil
}

// scheduledCreatorIdentity resolves a scheduled event's CreatedBy principal
// into the Identity the agent-create path would have had on its request
// context, plus the human-readable creator name that path records in
// AppliedConfig.CreatorName (#1797). Mirrors createAgent: an agent creator is
// named by its agent Name, a user creator by their email.
//
// authorizeScheduledAgentCreate has already admitted the creator by the time
// this runs; this is attribution and identity construction, not a gate.
func (s *Server) scheduledCreatorIdentity(ctx context.Context, createdBy string) (Identity, string, error) {
	if createdBy == "" {
		return nil, "", fmt.Errorf("scheduled event has no creator")
	}
	if creator, err := s.store.GetAgent(ctx, createdBy); err == nil {
		role, additionalScopes := agentRoleAndScopes(creator)
		scopes := append(ScopesForRole(role), additionalScopes...)
		identity := &agentIdentityWrapper{&AgentTokenClaims{
			Claims:    jwt.Claims{Subject: creator.ID},
			ProjectID: creator.ProjectID,
			Scopes:    scopes,
		}}
		return identity, creator.Name, nil
	} else if !errors.Is(err, store.ErrNotFound) {
		return nil, "", fmt.Errorf("failed to resolve scheduled dispatch creator agent %q: %w", createdBy, err)
	}
	user, err := s.store.GetUser(ctx, createdBy)
	if err != nil {
		return nil, "", fmt.Errorf("failed to resolve scheduled dispatch creator user %q: %w", createdBy, err)
	}
	identity := NewAuthenticatedUser(user.ID, user.Email, user.DisplayName, user.Role, "scheduler")
	return identity, user.Email, nil
}

// applyScheduledProjectDefaultGCPIdentity is the scheduler-path twin of the
// project-default/hub-default GCP identity ladder in createAgentInProject
// (handlers_agents_core.go). A scheduled dispatch carries no explicit
// gcp_identity, so the ladder here starts one rung down: the per-profile
// default for the profile the agent runs under, then the project default,
// then — when the project has no default at all — the hub default, then
// block (#1927). The same checks run in the same order at each assign rung
// (SA reachable from the project, SA verified, then the full
// evaluateSAAssignment gate against the immediate creator, whose identity
// must already be on ctx) via resolveDefaultSAAssignmentCore, the resolver
// shared with the HTTP create path's project-default and hub-default rungs
// (default_gcp_identity.go) — and, as on the create path, a default SA that
// fails any check fails the dispatch rather than silently degrading.
//
// Authorization principal: the scheduled dispatch has no interactive caller,
// so both the project-default and hub-default assign rungs authorize against
// the schedule's immediate creator — resolved by scheduledCreatorIdentity and
// placed on ctx by the caller (dispatchAgentEventHandler) before this runs.
// The hub-default rung mirrors the project-default rung's existing choice
// here; it does not introduce a new principal.
//
// When neither the project nor the hub has a default GCP identity mode
// configured at all, the applied config is left untouched (nil): unlike the
// create path's floor, this rung does not write an explicit "block" record
// for the "nothing configured" case, so the broker can apply its own
// runtime-aware default ("block" everywhere except Kubernetes, "passthrough"
// on Kubernetes — ptone/scion#2328 phase 1, since Kubernetes does not support
// "block"). An explicit "block" — at the project rung, or as the hub's own
// configured default — is different from "nothing configured" and always
// writes an explicit record, exactly as the create path does: an explicit
// choice must not be silently turned into a runtime-dependent default.
func (s *Server) applyScheduledProjectDefaultGCPIdentity(ctx context.Context, agent *store.Agent, project *store.Project) error {
	if agent.AppliedConfig == nil {
		agent.AppliedConfig = &store.AgentAppliedConfig{}
	}
	// A per-profile default for the profile this agent runs under wins over
	// the project-wide default, as on the create path.
	if profileName, profileSAID := s.projectProfileDefaultSA(ctx, agent.RuntimeBrokerID, project, agent.AppliedConfig.Profile); profileSAID != "" {
		cfg, err := s.resolveDefaultSAAssignmentCore(ctx, nil, agent.ProjectID,
			profileSAID, SurfaceProjectDefault, profileDefaultTier(profileName))
		if err != nil {
			return err
		}
		agent.AppliedConfig.GCPIdentity = cfg
		pinResolvedProfile(agent.AppliedConfig, profileName)
		slog.Debug("GCP identity chosen by default", "source", "project-profile-default",
			"project_id", agent.ProjectID, "agent", agent.Name, "profile", profileName, "sa_id", cfg.ServiceAccountID)
		return nil
	}
	projectSettings := projectSettingsFromAnnotations(project)
	switch projectSettings.DefaultGCPIdentityMode {
	case store.GCPMetadataModePassthrough:
		agent.AppliedConfig.GCPIdentity = &store.GCPIdentityConfig{
			MetadataMode: store.GCPMetadataModePassthrough,
		}
		if agent.RuntimeBrokerID != "" {
			if err := s.translatePassthroughForSandbox(ctx, agent, agent.RuntimeBrokerID); err != nil {
				return fmt.Errorf("failed to configure GCP identity for sandbox runtime: %w", err)
			}
		}
	case store.GCPMetadataModeAssign:
		if projectSettings.DefaultGCPIdentityServiceAccountID == "" {
			agent.AppliedConfig.GCPIdentity = &store.GCPIdentityConfig{
				MetadataMode: store.GCPMetadataModeBlock,
			}
			return nil
		}
		cfg, err := s.resolveDefaultSAAssignmentCore(ctx, nil, agent.ProjectID,
			projectSettings.DefaultGCPIdentityServiceAccountID, SurfaceProjectDefault, defaultTierProject)
		if err != nil {
			return err
		}
		agent.AppliedConfig.GCPIdentity = cfg
	case store.GCPMetadataModeBlock:
		// Project explicitly set "block" — stop the ladder here, matching the
		// create path's rule that explicit block does not fall through to
		// the hub default (handlers_agents_core.go). Written as an explicit
		// record (not left nil): an explicit project choice must be rejected
		// on the Kubernetes runtime, the same as an explicit per-agent
		// request or hub default, rather than silently becoming
		// "passthrough" now that nil means "apply the runtime-aware
		// default" (ptone/scion#2328 phase 1). Previously this arm left
		// AppliedConfig.GCPIdentity nil, which was indistinguishable from
		// "nothing configured" — see TestScheduledDispatch_ProjectBlockNotOverriddenByHubDefault.
		agent.AppliedConfig.GCPIdentity = &store.GCPIdentityConfig{
			MetadataMode: store.GCPMetadataModeBlock,
		}
	default:
		// No project default configured (empty string) — fall back to the
		// hub-level operational default, one rung down the ladder, mirroring
		// handlers_agents_core.go's default arm.
		hubDefaults := s.hubAgentDefaults()
		switch hubDefaults.DefaultGCPIdentityMode {
		case store.GCPMetadataModePassthrough:
			// Hub-default passthrough is confined to the embedded broker,
			// exactly as on the create path; see hubDefaultPassthroughAllowed.
			// Effective profile and pin-back mirror the create path; see
			// effectiveRuntimeProfileName.
			effectiveProfile := effectiveRuntimeProfileName(agent.AppliedConfig.Profile, project)
			// Only write an explicit record when the grant is allowed. When
			// denied, leave AppliedConfig.GCPIdentity unset instead of an
			// explicit "block" record (ptone/scion#2328) — see the create
			// path's equivalent arm (handlers_agents_core.go) for the full
			// rationale: the operator chose "passthrough", not "block", so a
			// denial here is treated like no default at all.
			if allowed, resolvedProfile := s.hubDefaultPassthroughAllowed(ctx, agent.RuntimeBrokerID, agent.ProjectID, agent.Name, effectiveProfile); allowed {
				// Pin the resolved profile onto both AppliedConfig.Profile
				// and CreateInputs.Profile — the latter is what scion
				// reincarnate replays (design §3.3 Amendment A1), and
				// CreateInputs is already built above with no Profile set,
				// so without this the pin would not survive a reincarnate.
				if agent.AppliedConfig.Profile == "" {
					agent.AppliedConfig.Profile = resolvedProfile
				}
				if agent.AppliedConfig.CreateInputs != nil && agent.AppliedConfig.CreateInputs.Profile == "" {
					agent.AppliedConfig.CreateInputs.Profile = resolvedProfile
				}
				agent.AppliedConfig.GCPIdentity = &store.GCPIdentityConfig{
					MetadataMode: store.GCPMetadataModePassthrough,
					// RequireLocalRuntime: see the create path's twin in
					// handlers_agents_core.go. Only ever set here, since this
					// whole block only runs when hubDefaultPassthroughAllowed
					// granted it.
					RequireLocalRuntime: true,
				}
				if agent.RuntimeBrokerID != "" {
					if err := s.translatePassthroughForSandbox(ctx, agent, agent.RuntimeBrokerID); err != nil {
						return fmt.Errorf("failed to configure GCP identity for sandbox runtime: %w", err)
					}
				}
			}
		case store.GCPMetadataModeAssign:
			if hubDefaults.DefaultGCPIdentityServiceAccountID == "" {
				agent.AppliedConfig.GCPIdentity = &store.GCPIdentityConfig{
					MetadataMode: store.GCPMetadataModeBlock,
				}
				return nil
			}
			cfg, err := s.resolveDefaultSAAssignmentCore(ctx, nil, agent.ProjectID,
				hubDefaults.DefaultGCPIdentityServiceAccountID, SurfaceHubDefault, defaultTierHub)
			if err != nil {
				return err
			}
			agent.AppliedConfig.GCPIdentity = cfg
		case store.GCPMetadataModeBlock:
			// Hub explicitly configured "block" as its own default — an
			// explicit choice, kept as an explicit record (rejected on the
			// Kubernetes runtime by the broker, ptone/scion#2328 phase 1),
			// mirroring the create path's equivalent case
			// (handlers_agents_core.go).
			agent.AppliedConfig.GCPIdentity = &store.GCPIdentityConfig{
				MetadataMode: store.GCPMetadataModeBlock,
			}
		default:
			// No hub default configured at all (empty) — preserve the
			// scheduler path's prior behaviour when nothing at all is
			// configured: leave AppliedConfig.GCPIdentity untouched (nil)
			// rather than writing an explicit "block" record, unlike the
			// store.GCPMetadataModeBlock case above. Pinned by
			// TestScheduledDispatch_NoProjectDefaultLeavesGCPIdentityUnchanged
			// and TestScheduledDispatch_NoHubDefaultLeavesGCPIdentityUnchanged.
			// This nil now also signals the broker to apply its own
			// runtime-aware default (ptone/scion#2328 phase 1).
		}
	}
	return nil
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

		if _, err := s.authorizeScheduledAgentCreate(ctx, evt); err != nil {
			return err
		}

		// Log staleness for late fires
		staleness := time.Since(evt.FireAt)
		if !evt.FireAt.IsZero() && staleness > 1*time.Minute {
			slog.Warn("Scheduler: firing stale dispatch_agent event",
				"eventID", evt.ID,
				"agentName", payload.AgentName,
				"scheduledFor", evt.FireAt.UTC().Format(time.RFC3339),
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
		// Scheduled dispatch has no modeled delegation context yet. Persist the
		// lowest explicit role for every scheduled child so migration/backfill
		// code can never reinterpret it as a legacy empty-role agent.
		agent.AppliedConfig.AgentRole = string(AgentRoleNone)
		agent.AppliedConfig.NoAuth = true
		// Record the creator's display name exactly as the agent-create path
		// does; the broker threads it through to the agent (#1797).
		creatorIdentity, creatorName, err := s.scheduledCreatorIdentity(ctx, evt.CreatedBy)
		if err != nil {
			return err
		}
		agent.AppliedConfig.CreatorName = creatorName
		if payload.Task != "" {
			agent.AppliedConfig.Task = payload.Task
		}
		if payload.Branch != "" {
			agent.AppliedConfig.Branch = payload.Branch
		}
		// CreateInputs (design §3.4 Amendment A3.7): without this, every scheduled agent looks
		// "legacy" to `scion reincarnate` forever — not just until its first
		// reincarnation, since the legacy fallback pins whatever HarnessConfig/
		// HarnessAuth/Profile/ThinkingLevel it resolved to into CreateInputs
		// permanently, so a template change is never picked up on a second
		// reincarnation either. Branch and NoAuth=true are the only explicit
		// inputs a scheduled agent has; everything else this path sets
		// (AgentRole, Task) is either a kept field or replaced by the
		// preamble+handoff on reincarnate.
		agent.AppliedConfig.CreateInputs = &store.AgentCreateInputs{
			Branch: payload.Branch,
			NoAuth: true,
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

		// Global fallback: try resolving a template named "default" when no
		// name arrived from the payload, the project annotation, or the hub
		// operational defaults. Twin of the rung on the agent-create path in
		// handlers_agents_core.go — design §5.2 risk (d) applies.
		templateFromImplicitDefault := false
		if payload.Template == "" {
			payload.Template = "default"
			agent.Template = "default"
			templateFromImplicitDefault = true
		}

		// Resolve template if specified. tmpl outlives the block so that
		// populateAgentConfig can stamp TemplateID/TemplateHash (and the
		// template defaults) exactly as on the agent-create path; without them
		// a broker lacking a local copy cannot hydrate the template (#1795).
		var tmpl *store.Template
		if payload.Template != "" {
			var tmplErr error
			tmpl, tmplErr = s.resolveTemplate(ctx, payload.Template, evt.ProjectID)
			// SECURITY-GATE (ptone/scion#1916): same gate as the agent-create
			// HTTP path in handlers_agents_core.go — a resolved candidate is
			// not yet known to be one the schedule's creator may read.
			// tmplErr is set to store.ErrNotFound on denial so the
			// degradation rule below (which keys off tmplErr, not tmpl)
			// treats a denial exactly like a definitive not-found, rather
			// than leaving tmplErr nil alongside a nil tmpl.
			if tmplErr == nil && tmpl != nil && !s.authorizeResolvedTemplate(ctx, creatorIdentity, tmpl) {
				tmpl = nil
				tmplErr = store.ErrNotFound
			}
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
			if templateFromImplicitDefault && tmpl == nil &&
				(tmplErr == nil || errors.Is(tmplErr, store.ErrNotFound)) {
				// The implicit "default" fallback template doesn't exist —
				// this is normal on hubs that haven't created one. Continue
				// with no template. No warning: unlike a hub default (which
				// is operator-configured and should resolve), the implicit
				// fallback is speculative.
				payload.Template = ""
				agent.Template = ""
			}
			if tmplErr == nil && tmpl != nil {
				if tmpl.Slug != "" {
					agent.Template = tmpl.Slug
				}
				// Harness-config resolution (project annotation, then this
				// template's default) happens later, in deriveAgentConfig,
				// along with the rest of create's config-resolution pipeline
				// — not here. See deriveAgentConfig's doc comment.
			}
		}

		// Project-default GCP identity, gated against the schedule creator as
		// the immediate agent creator — twin of the create path (#1797). It
		// must run before deriveAgentConfig: populateAgentConfig reads
		// AppliedConfig.GCPIdentity when checking auth credentials.
		if err := s.applyScheduledProjectDefaultGCPIdentity(
			contextWithIdentity(ctx, creatorIdentity), agent, project); err != nil {
			return fmt.Errorf("scheduled dispatch of agent %q: %w", slug, err)
		}

		// Apply project-level defaults, hub operational defaults, and the
		// template/harness-config derivation pipeline, exactly as on the
		// agent-create path. See deriveAgentConfig.
		if err := s.deriveAgentConfig(ctx, agent, project, tmpl); err != nil {
			return fmt.Errorf("scheduled dispatch of agent %q: %w", slug, err)
		}

		// Scheduled creates have no waiting client, so they opt in to
		// asynchronous launch server-side. It only takes effect when
		// hub.asyncAgentLaunch is on.
		agent.LaunchAsyncOptIn = true

		if err := s.createAgentWithIdentityKey(ctx, agent, slug); err != nil {
			return fmt.Errorf("failed to create agent %q: %w", slug, err)
		}

		// E.2b: success-path audit for scheduled dispatch. The deny-path
		// records for this same CanDelegate check are in
		// authorizeScheduledAgentCreate above. ActorPrincipalKind/ID mirror
		// that deny-audit convention (the resolved creator/execution
		// identity, required non-empty by the ent schema) — the cutover rule
		// keeps the fire-time execution/authorization identity as CreatedBy,
		// so the deny and allow audits for the same check agree on who the
		// actor is.
		//
		// The recorded initiator's credential is copied onto the audit ONLY
		// when the initiator is the same principal as the creator/executor
		// identity (initiatorMatchesExecutor, scheduled_initiator.go): after
		// an update or resume by a different user, the initiator is not the
		// creator, and this check exists specifically to prevent naming
		// principal A with principal B's credential. dev_local additionally
		// matches when both sides resolve to the well-known DevUserID —
		// initiatorMatchesExecutor's doc comment has the exact condition and
		// why it's needed (scheduledCreatorIdentity never reconstructs the
		// dev user's Type() as "dev"). When it does match, the value is
		// mapped back to hub.CredentialKind's vocabulary
		// (uat/agent_jwt/interactive/dev), since actor_credential_type is a
		// column every other writer fills from that domain, not
		// InitiatorAttribution's smaller one.
		scheduledDispatchAudit := &store.MutationAuditRecord{
			MutationType:       "agent_delegation",
			ActorPrincipalKind: creatorIdentity.Type(),
			ActorPrincipalID:   creatorIdentity.ID(),
			TargetType:         "agent",
			TargetID:           agent.ID,
			CanDelegateResult:  "allow",
		}
		initiator := s.scheduledInitiator(evt.InitiatorAttribution)
		if initiatorMatchesExecutor(initiator, creatorIdentity) {
			if hubKind := hubCredentialKindForInitiator(initiator.CredentialKind); hubKind != "" {
				scheduledDispatchAudit.ActorCredentialType = hubKind
				scheduledDispatchAudit.ActorCredentialID = initiator.CredentialID
			}
		}
		s.emitMutationAudit(ctx, scheduledDispatchAudit)

		// Record delegation edge (Phase 1G) from the schedule creator to the
		// dispatched agent. Best-effort: log errors but do not fail dispatch.
		// Determine delegator type by looking up whether the creator is an agent.
		delegatorType := store.DelegationPrincipalUser
		if _, err := s.store.GetAgent(ctx, evt.CreatedBy); err == nil {
			delegatorType = store.DelegationPrincipalAgent
		}
		// Use the agent's actual effective role from its applied config,
		// not AgentRoleNone. The edge role is used for audit and for
		// frozen-ceiling decisions on orphaned delegations.
		edgeRole := string(AgentRoleNone)
		if agent.AppliedConfig != nil && agent.AppliedConfig.AgentRole != "" {
			edgeRole = agent.AppliedConfig.AgentRole
		}
		s.recordDelegationEdgeWithType(ctx, agent.ID, evt.ProjectID, edgeRole, delegatorType, evt.CreatedBy)

		// Dispatch to runtime broker
		dispatchExecutor, _ := ExecutorContextFromContext(ctx)
		dispatcher := s.GetDispatcher()
		if dispatcher == nil {
			slog.Warn("Scheduler: no dispatcher available, agent created but not started",
				"eventID", evt.ID,
				"agent_id", agent.ID,
				"agentName", agent.Name,
				"executor_kind", dispatchExecutor.Kind,
				"executor_id", dispatchExecutor.ID)
			return nil
		}

		// The create-and-start runs under a start claim, which records run
		// intent running.
		created, err := s.createUnderClaim(ctx, agent, func(ctx context.Context) (*CreateDispatchResult, error) {
			return dispatcher.DispatchAgentCreate(ctx, agent)
		})
		var held *store.ClaimHeldError
		if errors.As(err, &held) {
			// The agent is already being started by the claim holder: the
			// event is skipped (not retried), not failed.
			slog.Info("Scheduler: agent already starting; event skipped",
				"eventID", evt.ID, "agent_id", agent.ID, "holder", string(held.Kind))
			return nil
		}
		if err != nil {
			slog.Error("Scheduler: failed to dispatch agent creation",
				"eventID", evt.ID,
				"agent_id", agent.ID,
				"agentName", agent.Name,
				"error", err,
				"executor_kind", dispatchExecutor.Kind,
				"executor_id", dispatchExecutor.ID)
			return fmt.Errorf("failed to dispatch agent %q: %w", slug, err)
		}
		if created.AcceptedLaunch() != nil {
			// The row is already provisioning; persist the non-status
			// fields from the dispatch.
			if _, err := s.persistAcceptedLaunch(ctx, agent); err != nil {
				slog.Warn("Scheduler: failed to persist agent after accepted launch",
					"eventID", evt.ID, "agent_id", agent.ID, "error", err)
			}
		}

		slog.Info("Scheduler: agent dispatched successfully",
			"eventID", evt.ID, "agent_id", agent.ID, "agentName", agent.Name,
			"project_id", evt.ProjectID,
			"executor_kind", dispatchExecutor.Kind,
			"executor_id", dispatchExecutor.ID)
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

	// Backstop for zone-prefixed expressions that reach the evaluator anyway
	// (for example a row written after the startup pass, or by an older
	// replica): pause the row instead of recording an invalid-expression
	// error on every tick.
	if hasCronZonePrefix(sched.CronExpr) {
		if err := s.store.UpdateScheduleStatus(ctx, sched.ID, store.ScheduleStatusPaused); err != nil {
			log.Error("schedule-evaluator: failed to pause schedule with unsupported zone prefix",
				"cron_expr", sched.CronExpr, "error", err)
			return
		}
		log.Warn("schedule paused: cron zone prefixes are not supported; edit the expression to UTC and resume",
			"cron_expr", sched.CronExpr)
		return
	}

	// Compute next run time
	cronSchedule, err := parseScheduleCron(sched.CronExpr)
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
		// E.2b: every recurrence copies the schedule's current initiator
		// attribution verbatim (plan §3.5) — a single struct assignment,
		// since Schedule and ScheduledEvent share the same mixin. The
		// materialized event then keeps this snapshot unchanged even if the
		// schedule is later re-attributed.
		InitiatorAttribution: sched.InitiatorAttribution,
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
		// E.2b: mark this as deferred execution of the schedule, distinct
		// from the initiator attribution just copied onto evt above.
		handlerCtx, cancel := context.WithTimeout(
			ContextWithExecutor(ctx, ExecutorContext{Kind: "schedule_evaluator", ID: "schedule:" + sched.ID}),
			30*time.Second)
		if handlerErr := handler(handlerCtx, evt); handlerErr != nil {
			errMsg = handlerErr.Error()
			log.Warn("schedule-evaluator: event handler failed", "error", handlerErr)
		} else {
			log.Info("schedule-evaluator: schedule fired successfully")
		}
		cancel()
	}

	// Update event status. If the handler returned an error, record as
	// failed rather than fired — matches fireEvent semantics (O-R3-1).
	firedAt := time.Now()
	status := store.ScheduledEventFired
	if errMsg != "" {
		status = store.ScheduledEventFailed
	}
	_ = s.store.UpdateScheduledEventStatus(ctx, evt.ID, status, &firedAt, errMsg)

	// Update schedule run state
	_ = s.store.UpdateScheduleAfterRun(ctx, sched.ID, now, nextRunAt, errMsg)
}

// registerSchedulerHandlers registers every periodic scheduler task, plus the
// "message" and "dispatch_agent" event handlers, on s.scheduler, which must
// already be initialized (StartBackgroundServices does this before calling
// in). Factored out of StartBackgroundServices so tests can inspect
// registration metadata (interval, singleton mode) without starting the
// scheduler's ticker or any other background service.
//
// Most recurring sweeps are cluster-wide-once work: under multi-replica
// Postgres they must run on a single replica per tick (gated by an advisory
// lock), otherwise every replica would publish duplicate offline/stalled
// events and race on the schedule claim. On SQLite the lock is a no-op. See
// CONCURRENCY-AUDIT.md §"Singleton / leader". broker-provider-selfheal is the
// per-instance exception: every instance must run it, not just one (see its
// registration below).
// Non-critical maintenance tasks run every 5 minutes (not every 1 minute) to
// reduce DB connection pressure. Combined with per-handler jitter in the
// scheduler, this eliminates the thundering-herd pattern that was causing
// 9-54 s API latency spikes.
func (s *Server) registerSchedulerHandlers() {
	// Async-create launch reaper (design §3.7): its own dedicated ticker,
	// registered unconditionally — see
	// registerLaunchReaper's doc comment for its per-tick cost with the
	// feature off.
	s.registerLaunchReaper()

	s.scheduler.RegisterRecurringSingleton("agent-heartbeat-timeout", 5, store.LockAgentHeartbeatTimeout, s.agentHeartbeatTimeoutHandler())
	s.scheduler.RegisterRecurringSingleton("agent-stalled-detection", 5, store.LockAgentStalledDetection, s.agentStalledDetectionHandler())
	if s.config.SoftDeleteRetention > 0 {
		s.scheduler.RegisterRecurringSingleton("soft-delete-purge", 60, store.LockSoftDeletePurge, s.purgeHandler())
	}
	s.scheduler.RegisterEventHandler("message", s.messageEventHandler())
	s.scheduler.RegisterEventHandler("dispatch_agent", s.dispatchAgentEventHandler())
	s.scheduler.RegisterRecurringSingleton("schedule-evaluator", 1, store.LockScheduleEvaluator, s.evaluateSchedulesHandler())
	s.scheduler.RegisterRecurringSingleton("broker-heartbeat-timeout", 5, store.LockBrokerHeartbeatTimeout, s.brokerHeartbeatTimeoutHandler())
	s.scheduler.RegisterRecurringSingleton("broker-affinity-reap", 5, store.LockBrokerAffinityReap, s.brokerAffinityReapHandler())
	// Hourly: an expired token is already refused at join, so this only
	// keeps the table from collecting rows.
	s.scheduler.RegisterRecurringSingleton("broker-join-token-cleanup", 60, store.LockBrokerJoinTokenCleanup, s.brokerJoinTokenCleanupHandler())
	// Not a singleton: this instance can only self-heal the providers of
	// brokers it personally holds a live local control-channel socket for
	// (see brokerProviderSelfHealHandler), so every instance must run it.
	s.scheduler.RegisterRecurring("broker-provider-selfheal", 1, s.brokerProviderSelfHealHandler())
	s.scheduler.RegisterRecurringSingleton("broker-message-sweep", 5, store.LockBrokerMessageSweep, s.brokerMessageSweepHandler())
	s.scheduler.RegisterRecurringSingleton("failed-message-retention", 60, store.LockFailedMessageRetention, s.failedMessageRetentionHandler())
	s.scheduler.RegisterRecurringSingleton("exposed-ports-sweep", 5, store.LockExposedPortsSweep, s.exposedPortsSweepHandler())
	s.scheduler.RegisterRecurringSingleton("notification-dispatch-sweep", 5, store.LockNotificationDispatchSweep, s.notificationDispatchSweepHandler())
	// Reconcile stale max_agents_per_broker reservations (ptone/scion#1963):
	// runs immediately at tick 0 (startup) and then hourly, fixing rows left
	// with released_at IS NULL by the pre-fix stop/suspend paths (or any
	// future drift) without a separate one-shot migration.
	s.scheduler.RegisterRecurringSingleton("broker-quota-reconcile", 60, store.LockBrokerQuotaReconcile, s.ReconcileStaleBrokerQuotaReservations)
	s.scheduler.RegisterRecurringSingleton("reincarnation-sweep", 5, store.LockReincarnationSweep, s.reincarnationSweepHandler())
	s.scheduler.RegisterRecurringSingleton("start-claim-reaper", 1, store.LockStartClaimReaper, s.startClaimReaperHandler())
	go func() {
		// Bounded: a store that does not answer at startup must not leave
		// this goroutine behind.
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		s.demoteOwnClaimsOnRestart(ctx)
	}()

	// A2A bridge sweep — conditional on the bridge being registered as a standalone plugin.
	if a2aExternalURL := s.getA2ABridgeExternalURL(); a2aExternalURL != "" {
		s.scheduler.RegisterRecurringSingleton(
			"a2a-bridge-sweep", 5, store.LockA2ABridgeSweep,
			s.a2aBridgeSweepHandler(a2aExternalURL),
		)
	}

	// Conduit registry maintenance (design v2.4 §3.4): only when this
	// process runs a relay (hub.conduit on at startup). One replica per tick
	// under Postgres; the registry requires the three reaps to run together
	// under one lock.
	if s.conduit.Load() != nil {
		s.scheduler.RegisterRecurringSingleton("conduit-registry-reap", conduitRegistryReapInterval, store.LockConduitRegistryReap, s.conduitRegistryReapHandler())
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

	// Register scheduled release update check for binary-tier deployments.
	// Conditional on deployment tier and update policy (design doc §6).
	mc := s.config.MaintenanceConfig
	if mc.DeploymentTier == "binary" && mc.UpdatePolicy != "disabled" {
		intervalHours := mc.CheckIntervalHours
		if intervalHours < 1 {
			intervalHours = 6 // default
		}
		intervalMinutes := intervalHours * 60
		// Add jitter: ±30 minutes to avoid thundering-herd when many hubs
		// check simultaneously (design doc recommendation).
		jitter := mathrand.Intn(61) - 30 // -30 to +30
		intervalMinutes += jitter
		if intervalMinutes < 60 {
			intervalMinutes = 60 // floor at 1 hour
		}
		s.scheduler.RegisterRecurringSingleton(
			"release-update-check",
			intervalMinutes,
			store.LockReleaseUpdateCheck,
			s.releaseUpdateCheckHandler(),
		)
	}
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
	s.registerSchedulerHandlers()

	// Report non-canonical stored timestamps (SQLite). It runs in the
	// background: on a large store the check is a full scan per time column,
	// and nothing at start depends on its result.
	s.startStoredTimestampCheck(ctx)

	// Record the workspaces this hub keeps as its own, and set the hub
	// project ID as the workspace project identity of hub-cloned projects
	// created with a locally generated one.
	s.startClonedProjectIdentityAlignment(ctx)

	// Pause schedules whose cron expression carries an unsupported zone
	// prefix before the evaluator's first tick, so it never runs them.
	s.startScheduler(ctx)

	// Start the DB connection-pool stats sampler (P3-6 -> P0-5 gauges). It is a
	// no-op unless an enabled recorder was wired via SetDBMetrics and the store
	// exposes its *sql.DB; this keeps connection-budget saturation observable
	// under multi-replica Postgres (see CONNECTION-BUDGET.md).
	if rec := s.dbMetrics; rec != nil {
		if dbp, ok := s.store.(interface{ DB() *sql.DB }); ok {
			stop := dbmetrics.StartPoolSampler(ctx, rec, dbp.DB(), 0)
			s.mu.Lock()
			s.stopPoolSampler = stop
			s.mu.Unlock()
		}
	}

	// Start rate limiter cleanup goroutines (exit when ctx is cancelled).
	if s.gcpTokenRateLimiter != nil {
		s.gcpTokenRateLimiter.StartCleanup(ctx)
	}
	if s.gcsLinkRateLimiter != nil {
		s.gcsLinkRateLimiter.StartCleanup(ctx)
	}
	if s.oidcTokenRateLimiter != nil {
		s.oidcTokenRateLimiter.StartCleanup(ctx)
	}
	if s.geExchangeRateLimiter != nil {
		s.geExchangeRateLimiter.StartCleanup(ctx)
	}
	if s.externalBearerRateLimiter != nil {
		s.externalBearerRateLimiter.StartCleanup(ctx)
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

// Shutdown gracefully shuts down the server. It is safe to call even when
// the Server was never started (e.g. New() followed directly by Shutdown()):
// the background-service teardown below always runs via CleanupResources,
// and only the final HTTP listener shutdown is skipped when there is no
// listener to shut down. It is also safe to call more than once, or
// together with CleanupResources, since CleanupResources is idempotent and
// http.Server.Shutdown tolerates repeated calls. The order is:
// CleanupResources, then the HTTP drain, then the decision audit writer
// drain (skipped if DeferDecisionAuditClose was called).
func (s *Server) Shutdown(ctx context.Context) error {
	s.mu.RLock()
	srv := s.httpServer
	s.mu.RUnlock()

	slog.Info("Hub API server shutting down...")

	// Run the shared background-service teardown (control channel, broker
	// auth, scheduler, dispatchers, preview service, link services, event
	// publisher, command bus, etc). CleanupResources is sync.Once-guarded,
	// so this is a no-op if it already ran.
	_ = s.CleanupResources(ctx)

	var err error
	if srv != nil {
		shutdownCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
		err = srv.Shutdown(shutdownCtx)
		cancel()
	}

	// Close the decision audit writer only after the HTTP drain, so
	// records from requests that finish during the drain are written.
	if !s.decisionAuditCloseDeferred.Load() {
		s.CloseDecisionAudit(ctx)
	}
	return err
}

// CleanupResources shuts down Hub-owned resources (control channel, broker auth,
// event publisher) without stopping an HTTP server. Use this in combined mode
// where the Hub API is mounted on the WebServer and has no listener of its own.
// It is also called internally by Shutdown, and is safe to call more than
// once, including after Shutdown: the teardown below runs at most once.
// It does not close the decision audit writer; in combined mode, call
// CloseDecisionAudit after the WebServer's HTTP drain.
func (s *Server) CleanupResources(ctx context.Context) error {
	s.cleanupOnce.Do(func() {
		s.mu.RLock()
		cc := s.controlChannel
		stopPoolSampler := s.stopPoolSampler
		s.mu.RUnlock()

		slog.Info("Cleaning up Hub resources...")

		// Drain the conduit relay first, while the database, scheduler and
		// server context are still up: the relay row goes draining, every
		// session gets GoAway and the relay deletes its rows (bounded by ctx).
		s.shutdownConduitRelay(ctx)

		// Stop the DB pool-stats sampler. Safe to call more than once: it
		// wraps either a context.CancelFunc or a no-op from
		// StartPoolSampler. Lives in the Once body so combined mode (which
		// only calls CleanupResources, never Shutdown) also stops it.
		//
		// Read under s.mu above rather than accessed directly here: in
		// combined mode, StartBackgroundServices (which writes this field)
		// runs in one goroutine while the CleanupResources-on-ctx.Done
		// goroutine started earlier (see cmd/server_foreground.go) can race
		// it, so the write and this read must share a lock.
		if stopPoolSampler != nil {
			stopPoolSampler()
		}

		// Cancel server-lifetime context to stop background goroutines
		if s.ctxCancel != nil {
			s.ctxCancel()
		}

		// Wait for in-flight audit goroutines.
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
		// Stop the B3 preview engine's nonce cleanup goroutine.
		if s.previewService != nil {
			s.previewService.Close()
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

// runMembershipMigration runs the RS1 one-binding migration at startup.
// R2-R2: wired into production startup, runs before route registration.
// Idempotent — safe to run on every startup. Fails closed on errors.
func (s *Server) runMembershipMigration(ctx context.Context) error {
	log := s.projectsLogger()
	results, err := s.membershipService.MigrateMultiRoleBindings(ctx)
	if err != nil {
		return fmt.Errorf("multi-role binding migration: %w", err)
	}

	var fixed, migErrors int
	for _, r := range results {
		if r.Error != nil {
			migErrors++
			log.Error("membership migration error",
				"project_id", r.ProjectID,
				"principal_id", r.PrincipalID,
				"error", r.Error)
		} else if r.DeletedCount > 0 {
			fixed++
		}
	}

	// Fail closed on any errors (R2-R2 requirement). This covers orphaned
	// role definitions (D-002) and transaction failures. Valid custom
	// coexistence (one built-in + N custom, or zero built-in + N custom) is
	// ignored by migration and never produces errors.
	if migErrors > 0 {
		return fmt.Errorf("membership migration had %d errors — resolve before accepting traffic", migErrors)
	}

	if fixed > 0 {
		log.Info("membership migration complete",
			"principals_fixed", fixed)
	} else {
		log.Debug("membership migration: no duplicates found")
	}

	// D4 membership-only unique constraint.
	//
	// The original D4 partial index blocked ALL second project bindings per
	// principal, which prevented custom project-scoped roles from coexisting
	// with built-in membership. Replace it with a narrower constraint that
	// only enforces "at most one built-in membership role per principal per
	// project" via the membership_kind column.
	//
	// Steps:
	//  1. Drop the legacy over-broad index if it exists.
	//  2. Backfill membership_kind='builtin' on existing built-in membership
	//     bindings (idempotent — only updates NULL rows that match).
	//  3. Install the new partial unique index on membership_kind IS NOT NULL.
	//
	// Fail-closed: abort startup on any DDL/DML failure.
	dbProvider, ok := s.store.(interface{ DB() *sql.DB })
	if !ok {
		return fmt.Errorf("D4 membership index: store does not expose raw DB — cannot install index (fail-closed)")
	}
	db := dbProvider.DB()
	if db == nil {
		return fmt.Errorf("D4 membership index: raw DB is nil — cannot install index (fail-closed)")
	}

	// All three steps run in a single transaction so a crash between
	// steps cannot leave the database without D4 enforcement. Both SQLite
	// and PostgreSQL support transactional DDL (DROP INDEX, CREATE INDEX)
	// and DML (UPDATE) within the same transaction.
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("D4 membership index: begin transaction failed (fail-closed): %w", err)
	}
	// Rollback on any failure; Commit below replaces it on success.
	defer func() { _ = tx.Rollback() }()

	// Step 1: Drop legacy over-broad D4 index.
	const dropLegacyIndex = `DROP INDEX IF EXISTS idx_rolebinding_one_per_principal_per_project`
	if _, err := tx.ExecContext(ctx, dropLegacyIndex); err != nil {
		return fmt.Errorf("D4 membership index: drop legacy index failed (fail-closed): %w", err)
	}

	// Step 2: Backfill membership_kind for existing built-in membership bindings.
	// Uses a correlated subquery against role_definitions to find bindings whose
	// role name is a built-in membership role. Idempotent.
	const backfillDML = `UPDATE role_bindings SET membership_kind = 'builtin' ` +
		`WHERE membership_kind IS NULL ` +
		`AND scope_type = 'project' ` +
		`AND role_definition_id IN (` +
		`  SELECT id FROM role_definitions ` +
		`  WHERE name IN ('project-owner', 'project-admin', 'project-member')` +
		`)`
	res, err := tx.ExecContext(ctx, backfillDML)
	if err != nil {
		return fmt.Errorf("D4 membership index: backfill membership_kind failed (fail-closed): %w", err)
	}
	if n, _ := res.RowsAffected(); n > 0 {
		log.Info("D4 membership index: backfilled membership_kind", "rows", n)
	}

	// Step 3: Install the narrower partial unique index.
	// Only constrains rows where membership_kind IS NOT NULL, allowing
	// unlimited custom project-scoped role bindings per principal.
	//
	// PostgreSQL concurrency note: the application-level pre-check in
	// CreateRoleBinding provides clean error messages for sequential callers
	// but cannot guard against concurrent inserts. This partial unique index
	// is the authoritative concurrency guard — PostgreSQL enforces it at the
	// MVCC level, rejecting a second built-in membership binding even when
	// two transactions race. SQLite tests prove the constraint semantics;
	// see TestCreateRoleBinding_BuiltInMembership_ConcurrentRace for the
	// closest approximation. A live PostgreSQL acceptance test exercising
	// concurrent INSERTs against this index should be run during deployment
	// QA to confirm production-equivalent behavior.
	const indexDDL = `CREATE UNIQUE INDEX IF NOT EXISTS ` +
		`idx_rolebinding_one_membership_per_principal_per_project ` +
		`ON role_bindings (principal_type, principal_id, scope_id) ` +
		`WHERE membership_kind IS NOT NULL AND scope_type = 'project'`
	if _, err := tx.ExecContext(ctx, indexDDL); err != nil {
		return fmt.Errorf("D4 membership index creation failed (fail-closed): %w", err)
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("D4 membership index: commit failed (fail-closed): %w", err)
	}
	log.Info("D4 membership-only unique index installed on role_bindings")

	return nil
}

// registerRoutes sets up all API routes.
func (s *Server) registerRoutes() {
	// Health and metrics endpoints
	s.mux.HandleFunc("/healthz", s.guarded("/healthz", s.handleHealthz))
	s.mux.HandleFunc("/readyz", s.guarded("/readyz", s.handleReadyz))
	s.mux.HandleFunc("/metrics", s.guarded("/metrics", s.handleMetrics))

	// Authentication endpoints (these routes are handled specially in middleware)
	s.mux.HandleFunc("/api/v1/auth/login", s.guarded("/api/v1/auth/login", s.handleAuthLogin))
	s.mux.HandleFunc("/api/v1/auth/token", s.guarded("/api/v1/auth/token", s.handleAuthToken))
	s.mux.HandleFunc("/api/v1/auth/refresh", s.guarded("/api/v1/auth/refresh", s.handleAuthRefresh))
	s.mux.HandleFunc("/api/v1/auth/validate", s.guarded("/api/v1/auth/validate", s.handleAuthValidate))
	s.mux.HandleFunc("/api/v1/auth/logout", s.guarded("/api/v1/auth/logout", s.handleAuthLogout))
	s.mux.HandleFunc("/api/v1/auth/me", s.guarded("/api/v1/auth/me", s.handleAuthMe))
	s.mux.HandleFunc("/api/v1/auth/admin-status", s.guarded("/api/v1/auth/admin-status", s.handleAuthAdminStatus))
	s.mux.HandleFunc("/api/v1/auth/tokens", s.guarded("/api/v1/auth/tokens", s.handleTokens))
	s.mux.HandleFunc("/api/v1/auth/tokens/", s.guarded("/api/v1/auth/tokens/", s.handleTokenByID))
	s.mux.HandleFunc("/api/v1/auth/scopes", s.guarded("/api/v1/auth/scopes", s.handleAuthScopes))
	s.mux.HandleFunc("/api/v1/auth/providers", s.guarded("/api/v1/auth/providers", s.handleCLIAuthProviders))

	// GE Google credential exchange endpoint (unauthenticated — authentication endpoint itself)
	s.mux.HandleFunc("/api/v1/auth/integrations/google/exchange", s.guarded("/api/v1/auth/integrations/google/exchange", s.handleGEGoogleExchange))

	// CLI OAuth endpoints (unauthenticated - used for login)
	s.mux.HandleFunc("/api/v1/auth/invite/redeem", s.guarded("/api/v1/auth/invite/redeem", s.handleInviteRedeem))
	s.mux.HandleFunc("/api/v1/auth/cli/authorize", s.guarded("/api/v1/auth/cli/authorize", s.handleCLIAuthAuthorize))
	s.mux.HandleFunc("/api/v1/auth/cli/token", s.guarded("/api/v1/auth/cli/token", s.handleCLIAuthToken))
	s.mux.HandleFunc("/api/v1/auth/cli/device", s.guarded("/api/v1/auth/cli/device", s.handleCLIDeviceAuthorize))
	s.mux.HandleFunc("/api/v1/auth/cli/device/token", s.guarded("/api/v1/auth/cli/device/token", s.handleCLIDeviceToken))

	// API v1 routes
	s.mux.HandleFunc("/api/v1/agents", s.guarded("/api/v1/agents", s.handleAgents))
	s.mux.HandleFunc("/api/v1/agents/", s.guarded("/api/v1/agents/", s.handleAgentByID))

	s.mux.HandleFunc("/api/v1/projects", s.guarded("/api/v1/projects", s.handleProjects))
	s.mux.HandleFunc("/api/v1/projects/register", s.guarded("/api/v1/projects/register", s.handleProjectRegister))
	// Project-nested routes: /api/v1/projects/{projectId}/agents, /api/v1/projects/{projectId}/env, etc.
	// This handler must come before the generic project-by-id handler
	s.mux.HandleFunc("/api/v1/projects/", s.guarded("/api/v1/projects/", s.handleProjectRoutes))

	s.mux.HandleFunc("/api/v1/runtime-brokers", s.guarded("/api/v1/runtime-brokers", s.handleRuntimeBrokers))
	s.mux.HandleFunc("/api/v1/runtime-brokers/", s.guarded("/api/v1/runtime-brokers/", s.handleRuntimeBrokerRoutes))

	s.mux.HandleFunc("/api/v1/templates", s.guarded("/api/v1/templates", s.handleTemplatesV2))
	s.mux.HandleFunc("/api/v1/templates/", s.guarded("/api/v1/templates/", s.handleTemplateByIDV2))

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
	s.mux.HandleFunc("/api/v1/gcp-service-accounts", s.guarded("/api/v1/gcp-service-accounts", s.handleGCPServiceAccounts))
	s.mux.HandleFunc("/api/v1/gcp-service-accounts/mint", s.guarded("/api/v1/gcp-service-accounts/mint", s.handleGCPServiceAccountsMint))
	s.mux.HandleFunc("/api/v1/gcp-service-accounts/", s.guarded("/api/v1/gcp-service-accounts/", s.handleGCPServiceAccountByID))

	s.mux.HandleFunc("/api/v1/gcs/object", s.guarded("/api/v1/gcs/object", s.handleGCSObject))
	s.mux.HandleFunc("/api/v1/conduit/grant-keys", s.guarded("/api/v1/conduit/grant-keys", s.handleConduitGrantKeys))
	s.mux.HandleFunc("/api/v1/conduit", s.guarded("/api/v1/conduit", s.handleConduit))

	// Artifact service (pkg/artifacts), behind the hub.artifacts experiment.
	// The patterns are literal here, not mounted through
	// artifacts.Service.RegisterRoutes, because the route-metadata tests and
	// the route-authz manifest lint read registrations from this file;
	// TestArtifactRoutesMatchService pins them to artifacts.RoutePatterns().
	artifactsHandler := s.artifactsHandler()
	s.mux.Handle("/api/v1/artifacts", s.artifactsGuard("/api/v1/artifacts", artifactsHandler))
	s.mux.Handle("/api/v1/artifacts/", s.artifactsGuard("/api/v1/artifacts/", artifactsHandler))
	s.mux.Handle("/api/v1/artifacts/shared/", s.artifactsGuard("/api/v1/artifacts/shared/", artifactsHandler))

	s.mux.HandleFunc("/api/v1/skills", s.guarded("/api/v1/skills", s.handleSkills))
	s.mux.HandleFunc("/api/v1/skills/", s.guarded("/api/v1/skills/", s.handleSkillByID))

	s.mux.HandleFunc("/api/v1/skill-registries", s.guarded("/api/v1/skill-registries", s.handleSkillRegistries))
	s.mux.HandleFunc("/api/v1/skill-registries/", s.guarded("/api/v1/skill-registries/", s.handleSkillRegistryByID))

	s.mux.HandleFunc("/api/v1/harness-configs", s.guarded("/api/v1/harness-configs", s.handleHarnessConfigs))
	s.mux.HandleFunc("/api/v1/harness-configs/", s.guarded("/api/v1/harness-configs/", s.handleHarnessConfigByID))

	// Hub-scoped pre-start hooks. The project-scoped equivalents live under
	// /api/v1/projects/{projectId}/pre-start-hooks (see handleProjectRoutes).
	s.mux.HandleFunc("/api/v1/pre-start-hooks", s.guarded("/api/v1/pre-start-hooks", s.handleHubPreStartHooks))
	s.mux.HandleFunc("/api/v1/pre-start-hooks/", s.guarded("/api/v1/pre-start-hooks/", s.handleHubPreStartHookByID))

	s.mux.HandleFunc("/api/v1/users", s.guarded("/api/v1/users", s.handleUsers))
	s.mux.HandleFunc("/api/v1/users/", s.guarded("/api/v1/users/", s.handleUserByID))

	// Environment variables and secrets (generic endpoints)
	s.mux.HandleFunc("/api/v1/env", s.guarded("/api/v1/env", s.handleEnvVars))
	s.mux.HandleFunc("/api/v1/env/", s.guarded("/api/v1/env/", s.handleEnvVarByKey))
	s.mux.HandleFunc("/api/v1/secrets", s.guarded("/api/v1/secrets", s.handleSecrets))
	s.mux.HandleFunc("/api/v1/secrets/", s.guarded("/api/v1/secrets/", s.handleSecretByKey))

	// Session metrics (DB-backed, distinct from Cloud Monitoring metrics dashboard)
	s.mux.HandleFunc("/api/v1/metrics/session/", s.guarded("/api/v1/metrics/session/", s.handleSessionMetrics))

	// Groups and Policies (Hub Permissions System)
	s.mux.HandleFunc("/api/v1/groups", s.guarded("/api/v1/groups", s.handleGroups))
	s.mux.HandleFunc("/api/v1/groups/", s.guarded("/api/v1/groups/", s.handleGroupRoutes))
	// CO1: Policy routes return 410 Gone. Authorization uses RoleBindings.
	s.mux.HandleFunc("/api/v1/policies", s.guarded("/api/v1/policies", s.handlePolicies))
	s.mux.HandleFunc("/api/v1/policies/", s.guarded("/api/v1/policies/", s.handlePolicyRoutes))

	// Authorization explain endpoint
	s.mux.HandleFunc("/api/v1/authz/explain", s.guarded("/api/v1/authz/explain", s.handleAuthzExplain))

	// Principal resolution endpoints (Phase 4)
	s.mux.HandleFunc("/api/v1/users/me/groups", s.guarded("/api/v1/users/me/groups", s.handleMyGroups))
	s.mux.HandleFunc("/api/v1/principals/", s.guarded("/api/v1/principals/", s.handlePrincipalRoutes))

	// User-scoped injected-skills endpoints (/users/me/...)
	s.mux.HandleFunc("/api/v1/users/me/injected-skills", s.guarded("/api/v1/users/me/injected-skills", s.handleUserMeInjectedSkills))
	s.mux.HandleFunc("/api/v1/users/me/injected-skills/", s.guarded("/api/v1/users/me/injected-skills/", func(w http.ResponseWriter, r *http.Request) {
		entryID := strings.TrimPrefix(r.URL.Path, "/api/v1/users/me/injected-skills/")
		entryID = strings.TrimSuffix(entryID, "/")
		s.handleUserMeInjectedSkillByID(w, r, entryID)
	}))

	// User-scoped terminal workspace persistence (/users/me/terminal-workspace)
	s.mux.HandleFunc("/api/v1/users/me/terminal-workspace", s.guarded("/api/v1/users/me/terminal-workspace", s.handleUserMeTerminalWorkspace))

	// User-scoped template endpoints (/users/me/templates)
	s.mux.HandleFunc("/api/v1/users/me/templates", s.guarded("/api/v1/users/me/templates", s.handleUserMeTemplates))
	s.mux.HandleFunc("/api/v1/users/me/templates/", s.guarded("/api/v1/users/me/templates/", func(w http.ResponseWriter, r *http.Request) {
		templateID := strings.TrimPrefix(r.URL.Path, "/api/v1/users/me/templates/")
		templateID = strings.TrimSuffix(templateID, "/")
		s.handleUserMeTemplateByID(w, r, templateID)
	}))

	// Hub-scope injected-skills endpoint
	s.mux.HandleFunc("/api/v1/hub/settings/injected-skills", s.guarded("/api/v1/hub/settings/injected-skills", s.handleHubInjectedSkills))

	// Broker registration endpoints (Runtime Broker HMAC authentication)
	s.mux.HandleFunc("/api/v1/brokers", s.guarded("/api/v1/brokers", s.handleBrokersEndpoint))
	s.mux.HandleFunc("/api/v1/brokers/join", s.guarded("/api/v1/brokers/join", s.handleBrokerJoin))
	s.mux.HandleFunc("/api/v1/brokers/", s.guarded("/api/v1/brokers/", s.handleBrokerByIDRoutes))

	// Broker plugin inbound message delivery
	s.mux.HandleFunc("/api/v1/broker/inbound", s.guarded("/api/v1/broker/inbound", s.handleBrokerInbound))
	s.mux.HandleFunc("/api/v1/broker/inbound/routed", s.guarded("/api/v1/broker/inbound/routed", s.handleBrokerInboundRouted))

	// Broker plugin callback delivery (interactive card responses, action acks)
	s.mux.HandleFunc("/api/v1/broker/callback", s.guarded("/api/v1/broker/callback", s.handleBrokerCallback))

	// Broker plugin project listing (fresh list for /setup flows)
	s.mux.HandleFunc("/api/v1/broker/projects", s.guarded("/api/v1/broker/projects", s.handleBrokerProjects))

	// Admin system endpoints: declarative guard enforces hub-admin; handler-local checks remain as defense-in-depth.
	s.mux.HandleFunc("/api/v1/admin/maintenance", s.guarded("/api/v1/admin/maintenance", s.handleAdminMaintenance))
	s.mux.HandleFunc("/api/v1/admin/maintenance/operations", s.guarded("/api/v1/admin/maintenance/operations", s.handleAdminMaintenanceOps))
	s.mux.HandleFunc("/api/v1/admin/maintenance/operations/", s.guarded("/api/v1/admin/maintenance/operations/", s.handleAdminMaintenanceOps))
	s.mux.HandleFunc("/api/v1/admin/maintenance/migrations/", s.guarded("/api/v1/admin/maintenance/migrations/", s.handleAdminMaintenanceMigrations))
	s.mux.HandleFunc("/api/v1/admin/maintenance/check-updates", s.guarded("/api/v1/admin/maintenance/check-updates", s.handleCheckForUpdates))
	s.mux.HandleFunc("/api/v1/admin/maintenance/update-available", s.guarded("/api/v1/admin/maintenance/update-available", s.handleUpdateAvailable))
	s.mux.HandleFunc("/api/v1/admin/maintenance/restart", s.guarded("/api/v1/admin/maintenance/restart", s.handleAdminRestart))
	s.mux.HandleFunc("/api/v1/admin/scheduler", s.guarded("/api/v1/admin/scheduler", s.handleAdminScheduler))
	s.mux.HandleFunc("/api/v1/admin/allow-list", s.guarded("/api/v1/admin/allow-list", s.handleAdminAllowList))
	s.mux.HandleFunc("/api/v1/admin/allow-list/", s.guarded("/api/v1/admin/allow-list/", s.handleAdminAllowListByEmail))
	s.mux.HandleFunc("/api/v1/admin/users/invite/bulk", s.guarded("/api/v1/admin/users/invite/bulk", s.handleAdminUserInviteBulk))
	s.mux.HandleFunc("/api/v1/admin/users/invite", s.guarded("/api/v1/admin/users/invite", s.handleAdminUserInvite))
	s.mux.HandleFunc("/api/v1/admin/invites", s.guarded("/api/v1/admin/invites", s.handleAdminInvites))
	s.mux.HandleFunc("/api/v1/admin/invites/", s.guarded("/api/v1/admin/invites/", s.handleAdminInviteByID))
	s.mux.HandleFunc("/api/v1/admin/server-config/schema", s.guarded("/api/v1/admin/server-config/schema", s.handleAdminServerConfigSchema))
	s.mux.HandleFunc("/api/v1/admin/server-config/sections/", s.guarded("/api/v1/admin/server-config/sections/", s.handleAdminServerConfigSectionReset))
	s.mux.HandleFunc("/api/v1/admin/server-config", s.guarded("/api/v1/admin/server-config", s.handleAdminServerConfig))
	s.mux.HandleFunc("/api/v1/admin/project-defaults", s.guarded("/api/v1/admin/project-defaults", s.handleAdminProjectDefaults))
	s.mux.HandleFunc("/api/v1/admin/messaging", s.guarded("/api/v1/admin/messaging", s.handleAdminMessaging))
	s.mux.HandleFunc("/api/v1/admin/experiments", s.guarded("/api/v1/admin/experiments", s.handleAdminExperiments))
	s.mux.HandleFunc("/api/v1/admin/conduit/grant-keys/rotate", s.guarded("/api/v1/admin/conduit/grant-keys/rotate", s.handleAdminConduitGrantKeyRotate))
	s.mux.HandleFunc("/api/v1/admin/agents/reset-auth-all", s.guarded("/api/v1/admin/agents/reset-auth-all", s.handleAdminResetAuthAll))
	s.mux.HandleFunc("/api/v1/admin/gcp-quota", s.guarded("/api/v1/admin/gcp-quota", s.handleAdminGCPQuota))
	s.mux.HandleFunc("/api/v1/admin/lifecycle-hooks", s.guarded("/api/v1/admin/lifecycle-hooks", s.handleAdminLifecycleHooks))
	s.mux.HandleFunc("/api/v1/admin/lifecycle-hooks/", s.guarded("/api/v1/admin/lifecycle-hooks/", s.handleAdminLifecycleHookByID))
	s.mux.HandleFunc("/api/v1/admin/validate-resources", s.guarded("/api/v1/admin/validate-resources", s.handleAdminValidateResources))
	s.mux.HandleFunc("/api/v1/admin/integrations", s.guarded("/api/v1/admin/integrations", s.handleAdminIntegrations))
	s.mux.HandleFunc("/api/v1/admin/integrations/teams/manifest", s.guarded("/api/v1/admin/integrations/teams/manifest", s.handleTeamsManifestDownload))
	s.mux.HandleFunc("/api/v1/admin/integrations/", s.guarded("/api/v1/admin/integrations/", s.handleAdminIntegrationByName))
	s.mux.HandleFunc("/api/v1/admin/diagnostics/logs/stream", s.guarded("/api/v1/admin/diagnostics/logs/stream", s.handleDiagnosticsLogsStream))
	s.mux.HandleFunc("/api/v1/admin/diagnostics/logs", s.guarded("/api/v1/admin/diagnostics/logs", s.handleDiagnosticsLogs))
	s.mux.HandleFunc("/api/v1/admin/health/summary", s.guarded("/api/v1/admin/health/summary", s.handleHealthSummary))
	s.mux.HandleFunc("/api/v1/admin/messaging/divergence", s.guarded("/api/v1/admin/messaging/divergence", s.handleAdminMessagingDivergence))
	s.mux.HandleFunc("/api/v1/metrics/", s.guarded("/api/v1/metrics/", s.handleMetricsDashboard))
	s.mux.HandleFunc("/api/v1/admin/metrics-dashboard", s.guarded("/api/v1/admin/metrics-dashboard", s.handleAdminMetricsDashboard)) // legacy backward-compat

	// Quota management (PR-B3)
	s.mux.HandleFunc("/api/v1/admin/limits", s.guarded("/api/v1/admin/limits", s.handleAdminLimits))
	s.mux.HandleFunc("/api/v1/admin/limits/", s.guarded("/api/v1/admin/limits/", s.handleAdminLimitByID))
	s.mux.HandleFunc("/api/v1/admin/entitlements/", s.guarded("/api/v1/admin/entitlements/", s.handleAdminEntitlementByID))
	s.mux.HandleFunc("/api/v1/admin/usage", s.guarded("/api/v1/admin/usage", s.handleAdminUsage))
	s.mux.HandleFunc("/api/v1/admin/usage/", s.guarded("/api/v1/admin/usage/", s.handleAdminUsageByLimit))
	s.mux.HandleFunc("/api/v1/usage/me", s.guarded("/api/v1/usage/me", s.handleUsageMe))

	// Role management (PR-C1)
	s.mux.HandleFunc("/api/v1/admin/roles", s.guarded("/api/v1/admin/roles", s.handleAdminRoles))
	s.mux.HandleFunc("/api/v1/admin/roles/export", s.guarded("/api/v1/admin/roles/export", s.handleAdminRolesExport))
	s.mux.HandleFunc("/api/v1/admin/roles/import", s.guarded("/api/v1/admin/roles/import", s.handleAdminRolesImport))
	s.mux.HandleFunc("/api/v1/admin/roles/", s.guarded("/api/v1/admin/roles/", s.handleAdminRoleByID))
	s.mux.HandleFunc("/api/v1/admin/role-bindings", s.guarded("/api/v1/admin/role-bindings", s.handleAdminRoleBindings))
	s.mux.HandleFunc("/api/v1/admin/role-bindings/", s.guarded("/api/v1/admin/role-bindings/", s.handleAdminRoleBindingByID))
	s.mux.HandleFunc("/api/v1/admin/permissions", s.guarded("/api/v1/admin/permissions", s.handleAdminPermissions))
	s.mux.HandleFunc("/api/v1/admin/access-constraints", s.guarded("/api/v1/admin/access-constraints", s.handleAdminAccessConstraints))
	s.mux.HandleFunc("GET /api/v1/admin/access-constraints/{id}/audit", s.guarded("GET /api/v1/admin/access-constraints/{id}/audit", s.handleAdminAccessConstraintAudit))
	s.mux.HandleFunc("/api/v1/admin/access-constraints/", s.guarded("/api/v1/admin/access-constraints/", s.handleAdminAccessConstraintByID))
	s.mux.HandleFunc("/api/v1/admin/access-constraint-previews", s.guarded("/api/v1/admin/access-constraint-previews", s.handleAdminAccessConstraintPreviews))
	s.mux.HandleFunc("/api/v1/admin/access-constraint-previews/", s.guarded("/api/v1/admin/access-constraint-previews/", s.handleAdminAccessConstraintPreviews))
	s.mux.HandleFunc("/api/v1/admin/effective-access", s.guarded("/api/v1/admin/effective-access", s.handleAdminEffectiveAccess))

	// Notification endpoints (user-facing)
	s.mux.HandleFunc("/api/v1/notifications", s.guarded("/api/v1/notifications", s.handleNotifications))
	s.mux.HandleFunc("/api/v1/notifications/", s.guarded("/api/v1/notifications/", s.handleNotificationRoutes))

	// Message inbox endpoints (user-facing)
	s.mux.HandleFunc("/api/v1/messages", s.guarded("/api/v1/messages", s.handleMessages))
	s.mux.HandleFunc("/api/v1/messages/", s.guarded("/api/v1/messages/", s.handleMessageRoutes))
	s.mux.HandleFunc("/api/v1/message-channels", s.guarded("/api/v1/message-channels", s.handleMessageChannels))

	// Conversation endpoints (user and agent facing)
	s.mux.HandleFunc("/api/v1/conversations", s.guarded("/api/v1/conversations", s.handleListConversations))
	s.mux.HandleFunc("/api/v1/conversations/", s.guarded("/api/v1/conversations/", s.handleConversationRoutes))
	s.mux.HandleFunc("/api/v1/conversations/resolve", s.guarded("/api/v1/conversations/resolve", s.handleConversationResolve))

	// Cross-project messaging endpoints (Phase 3)
	s.mux.HandleFunc("/api/v1/messaging/capabilities", s.guarded("/api/v1/messaging/capabilities", s.handleMessagingCapabilities))
	s.mux.HandleFunc("/api/v1/messaging/targets/resolve", s.guarded("/api/v1/messaging/targets/resolve", s.handleMessagingTargetsResolve))

	// Native chat endpoints. Registration is gated on server.native_chat.enabled,
	// so disabling the feature makes every /api/v1/chat/* path 404 rather than
	// leaving a live API behind a hidden UI. This is a startup-time gate:
	// flipping the toggle requires a hub restart (same as the message broker).
	if s.nativeChatEnabled() {
		// Chat thread prefs (Phase 3 — visibility mode persistence)
		s.mux.HandleFunc("/api/v1/chat/prefs", s.guarded("/api/v1/chat/prefs", s.handleChatPrefs))

		// Wave-2 chat endpoints (conversation REST API)
		s.mux.HandleFunc("/api/v1/chat/spaces", s.guarded("/api/v1/chat/spaces", s.handleChatSpaces))
		s.mux.HandleFunc("/api/v1/chat/spaces/", s.guarded("/api/v1/chat/spaces/", s.handleChatSpaceRoutes))
		s.mux.HandleFunc("/api/v1/chat/conversations/", s.guarded("/api/v1/chat/conversations/", s.handleChatConversationRoutes))
		s.mux.HandleFunc("/api/v1/chat/topics/", s.guarded("/api/v1/chat/topics/", s.handleChatTopicRoutes))
		s.mux.HandleFunc("/api/v1/chat/dms", s.guarded("/api/v1/chat/dms", s.handleChatDMs))
		s.mux.HandleFunc("/api/v1/chat/user-prefs", s.guarded("/api/v1/chat/user-prefs", s.handleChatUserPrefs))
		s.mux.HandleFunc("/api/v1/chat/presence", s.guarded("/api/v1/chat/presence", s.handleChatPresence))
		s.mux.HandleFunc("/api/v1/chat/search", s.guarded("/api/v1/chat/search", s.handleChatSearch))
		s.mux.HandleFunc("/api/v1/chat/attachments", s.guarded("/api/v1/chat/attachments", s.handleChatAttachments))
		s.mux.HandleFunc("/api/v1/chat/attachments/", s.guarded("/api/v1/chat/attachments/", s.handleChatAttachmentByID))
	}

	// WebSocket control channel endpoint for Runtime Brokers
	s.mux.HandleFunc("/api/v1/runtime-brokers/connect", s.guarded("/api/v1/runtime-brokers/connect", s.handleRuntimeBrokerConnect))

	// GCP identity endpoints (agent token auth)
	s.mux.HandleFunc("/api/v1/agent/gcp-token", s.guarded("/api/v1/agent/gcp-token", s.handleAgentGCPToken))
	s.mux.HandleFunc("/api/v1/agent/gcp-identity-token", s.guarded("/api/v1/agent/gcp-identity-token", s.handleAgentGCPIdentityToken))

	// Agent self-service secret fetch (agent token auth)
	s.mux.HandleFunc("POST /api/v1/agent/secrets", s.guarded("POST /api/v1/agent/secrets", s.handleAgentSecretFetch))

	// Public settings endpoint (no auth required for telemetry default, etc.)
	s.mux.HandleFunc("/api/v1/settings/public", s.guarded("/api/v1/settings/public", s.handlePublicSettings))

	// Resolved experiments map for signed-in callers (ptone/scion#2217).
	s.mux.HandleFunc("/api/v1/experiments", s.guarded("/api/v1/experiments", s.handleExperiments))

	// GitHub App integration endpoints: method-aware permission enforcement.
	// Read operations use hub.github_app.read; mutations use hub.github_app.update.
	s.mux.HandleFunc("GET /api/v1/github-app", s.guarded("GET /api/v1/github-app", s.handleGetGitHubApp))
	s.mux.HandleFunc("PUT /api/v1/github-app", s.guarded("PUT /api/v1/github-app", s.handleUpdateGitHubApp))
	s.mux.HandleFunc("GET /api/v1/github-app/installations", s.guarded("GET /api/v1/github-app/installations", s.handleListGitHubAppInstallations))
	s.mux.HandleFunc("POST /api/v1/github-app/installations", s.guarded("POST /api/v1/github-app/installations", s.handleCreateGitHubAppInstallation))
	s.mux.HandleFunc("GET /api/v1/github-app/installations/", s.guarded("GET /api/v1/github-app/installations/", s.handleGitHubAppInstallationByIDRead))
	s.mux.HandleFunc("PUT /api/v1/github-app/installations/", s.guarded("PUT /api/v1/github-app/installations/", s.handleGitHubAppInstallationByIDWrite))
	s.mux.HandleFunc("DELETE /api/v1/github-app/installations/", s.guarded("DELETE /api/v1/github-app/installations/", s.handleGitHubAppInstallationByIDWrite))
	s.mux.HandleFunc("POST /api/v1/github-app/installations/discover", s.guarded("POST /api/v1/github-app/installations/discover", s.handleGitHubAppDiscover))
	s.mux.HandleFunc("POST /api/v1/github-app/sync-permissions", s.guarded("POST /api/v1/github-app/sync-permissions", s.handleGitHubAppSyncPermissions))

	// Telegram account linking endpoints
	s.mux.HandleFunc("/api/v1/telegram/link", s.guarded("/api/v1/telegram/link", s.handleTelegramLink))
	s.mux.HandleFunc("/api/v1/telegram/link/verify", s.guarded("/api/v1/telegram/link/verify", s.handleTelegramLinkVerify))
	s.mux.HandleFunc("/api/v1/telegram/link/status", s.guarded("/api/v1/telegram/link/status", s.handleTelegramLinkStatus))

	// Discord account linking endpoints
	s.mux.HandleFunc("/api/v1/discord/link", s.guarded("/api/v1/discord/link", s.handleDiscordLink))
	s.mux.HandleFunc("/api/v1/discord/link/verify", s.guarded("/api/v1/discord/link/verify", s.handleDiscordLinkVerify))
	s.mux.HandleFunc("/api/v1/discord/link/status", s.guarded("/api/v1/discord/link/status", s.handleDiscordLinkStatus))

	// Teams account linking endpoints
	s.mux.HandleFunc("/api/v1/teams/link", s.guarded("/api/v1/teams/link", s.handleTeamsLink))
	s.mux.HandleFunc("/api/v1/teams/link/verify", s.guarded("/api/v1/teams/link/verify", s.handleTeamsLinkVerify))
	s.mux.HandleFunc("/api/v1/teams/link/status", s.guarded("/api/v1/teams/link/status", s.handleTeamsLinkStatus))

	// Unified resource import endpoint (templates + harness-configs, global + project)
	s.mux.HandleFunc("/api/v1/resources/import", s.guarded("/api/v1/resources/import", s.handleResourcesImport))
	s.mux.HandleFunc("/api/v1/resources/discover", s.guarded("/api/v1/resources/discover", s.handleResourcesDiscover))

	// Skill directory discovery (batch-add support for injected skills). Registered
	// as an exact path so it takes precedence over the /api/v1/skills/ subtree
	// handler above.
	s.mux.HandleFunc("/api/v1/skills/discover-directory", s.guarded("/api/v1/skills/discover-directory", s.handleSkillsDiscoverDirectory))

	// GitHub App webhook and setup callback (unauthenticated — uses webhook signature)
	s.mux.HandleFunc("/api/v1/webhooks/github", s.guarded("/api/v1/webhooks/github", s.handleGitHubWebhook))
	s.mux.HandleFunc("/github-app/setup", s.guarded("/github-app/setup", s.handleGitHubAppSetup))

	// Workstation-only system endpoints: declarative guard enforces workstation token.
	s.mux.HandleFunc("/api/v1/system/identity", s.guarded("/api/v1/system/identity", s.handleSystemIdentity))
	s.mux.HandleFunc("/api/v1/system/status", s.guarded("/api/v1/system/status", s.handleSystemStatus))
	s.mux.HandleFunc("/api/v1/system/check", s.guarded("/api/v1/system/check", s.handleSystemCheck))
	s.mux.HandleFunc("/api/v1/system/runtime", s.guarded("/api/v1/system/runtime", s.handleSystemRuntime))
	s.mux.HandleFunc("/api/v1/system/init", s.guarded("/api/v1/system/init", s.handleSystemInit))
	s.mux.HandleFunc("/api/v1/system/images/pull", s.guarded("/api/v1/system/images/pull", s.handleSystemImagesPull))
	s.mux.HandleFunc("/api/v1/system/images/build", s.guarded("/api/v1/system/images/build", s.handleSystemImagesBuild))
	s.mux.HandleFunc("/api/v1/system/apple-dns", s.guarded("/api/v1/system/apple-dns", s.handleAppleDNS))
	s.mux.HandleFunc("/api/v1/system/registry", s.guarded("/api/v1/system/registry", s.handleSystemRegistry))
	s.mux.HandleFunc("/api/v1/system/workstation-settings", s.guarded("/api/v1/system/workstation-settings", s.handleWorkstationSettings))

	// OIDC Identity Provider endpoints (unauthenticated — public metadata)
	if s.oidcKeyManager != nil {
		s.mux.HandleFunc("GET /.well-known/openid-configuration", s.guarded("GET /.well-known/openid-configuration", s.handleOIDCDiscovery))
		s.mux.HandleFunc("GET /.well-known/jwks.json", s.guarded("GET /.well-known/jwks.json", s.handleJWKS))
		s.mux.HandleFunc("POST /api/v1/agent/identity-token", s.guarded("POST /api/v1/agent/identity-token", s.handleAgentIdentityToken))
	}

	// Workstation-only filesystem endpoints: declarative guard enforces workstation token.
	s.mux.HandleFunc("/api/v1/system/fs/list", s.guarded("/api/v1/system/fs/list", s.handleFSList))
	s.mux.HandleFunc("/api/v1/system/fs/mkdir", s.guarded("/api/v1/system/fs/mkdir", s.handleFSMkdir))
	s.mux.HandleFunc("/api/v1/system/fs/validate-path", s.guarded("/api/v1/system/fs/validate-path", s.handleFSValidatePath))
}

// applyMiddleware wraps the handler with middleware.
func (s *Server) applyMiddleware(h http.Handler) http.Handler {
	// Apply middleware in reverse order (last applied runs first)
	h = s.recoveryMiddleware(h)

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

	// The request logger wraps UnifiedAuthMiddleware, so it is the request
	// logger's own next.ServeHTTP call that invokes auth: every request is
	// logged with its final response status, whether auth allows it through
	// or rejects it outright. auth_type and principal/credential attributes
	// come from logging.SetRequestAuth on the shared *RequestMeta this
	// middleware installs (see RequestLogMiddleware), reachable from every
	// context derived from it.
	if s.requestLogger != nil {
		h = logging.RequestLogMiddleware(s.requestLogger, "hub", logging.HubPathPatterns(), s.config.SlowRequestThreshold)(h)
	} else {
		h = s.loggingMiddleware(h)
	}

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
				slog.String("query", logging.RedactQuery(r.URL.RawQuery)),
			)
		}

		aborted := logging.ServeCatchingAbort(next, wrapped, r)

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

		attrs = append(attrs,
			slog.Int("status", wrapped.statusCode),
			slog.Duration("duration", duration),
		)
		if aborted {
			attrs = append(attrs, slog.Bool(logging.AttrAborted, true))
		}
		slog.LogAttrs(r.Context(), level, "Request completed", attrs...)
		if aborted {
			panic(http.ErrAbortHandler)
		}
	})
}

// recoveryMiddleware recovers from panics. http.ErrAbortHandler is passed
// on, not recovered: it is how a handler (the port proxy, mid-stream)
// tells net/http to cut the connection instead of completing a response
// whose headers are already out, and writing an error body after them
// would end the stream as if it had completed.
func (s *Server) recoveryMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if err := recover(); err != nil {
				if err == http.ErrAbortHandler { //nolint:errorlint // net/http compares the sentinel by identity
					panic(err)
				}
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

// Unwrap lets http.NewResponseController reach the underlying
// ResponseWriter's own optional interfaces (e.g. SetWriteDeadline), through
// this wrapper rather than stopping at it.
func (rw *responseWriter) Unwrap() http.ResponseWriter {
	return rw.ResponseWriter
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

// stampProvidersOnline sets status=online on every project-provider row linked
// to brokerID and returns only the providers it actually stamped online, so
// its only caller, markBrokerOnline, doesn't have to query the project list
// twice for event publishing and doesn't announce a project as online when
// its row was never updated. A failed UpdateProviderStatus for one provider
// is logged and excluded from the result — a partial stamp is still strictly
// better than none, but the caller must not treat an excluded provider as
// online.
func (s *Server) stampProvidersOnline(ctx context.Context, brokerID string) ([]store.ProjectProvider, error) {
	providers, err := s.store.GetBrokerProjects(ctx, brokerID)
	if err != nil {
		return nil, err
	}
	online := make([]store.ProjectProvider, 0, len(providers))
	for _, provider := range providers {
		if err := s.store.UpdateProviderStatus(ctx, provider.ProjectID, brokerID, store.BrokerStatusOnline); err != nil {
			slog.Error("Failed to update provider status", "brokerID", brokerID, "project_id", provider.ProjectID, "error", err)
			continue
		}
		online = append(online, provider)
	}
	return online, nil
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

	providers, err := s.stampProvidersOnline(ctx, brokerID)
	if err != nil {
		slog.Error("Failed to get broker projects for status update", "brokerID", brokerID, "error", err)
		return
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

	// Notification redeliver (ptone/scion#495): drain undispatched agent
	// notifications for agents on this broker. Async + CAS-gated, same
	// pattern as reconcileBroker above.
	if s.notificationDispatcher != nil {
		go s.drainUndispatchedNotifications(context.Background(), brokerID)
	}
}

// handleBrokerDisconnect runs when a broker's control-channel WebSocket drops.
// It is the OnDisconnect callback wired to srv.controlChannel in New; factored
// out to a method (rather than left as an inline closure) so tests can drive
// it directly against a lightly-constructed *Server.
//
// ReleaseAndMarkBrokerOffline atomically clears affinity AND stamps
// status=offline in a single CAS write — if a concurrent reconnect has
// already claimed the broker with a new session, the compare fails and the
// callback is a no-op. This eliminates the TOCTOU race where a separate
// ReleaseRuntimeBrokerConnection + UpdateRuntimeBrokerHeartbeat allowed
// the offline stamp to clobber a concurrent markBrokerOnline (issue #131).
func (s *Server) handleBrokerDisconnect(ctx context.Context, brokerID, sessionID string) {
	cleared, err := s.store.ReleaseAndMarkBrokerOffline(ctx, brokerID, s.instanceID, sessionID)
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
	broker, rerr := s.store.GetRuntimeBroker(ctx, brokerID)
	if rerr == nil && broker.ConnectedSessionID != nil && *broker.ConnectedSessionID != "" {
		slog.Info("broker re-claimed by new session after release; skipping provider offline stamp",
			"brokerID", brokerID, "staleSession", sessionID, "newSession", *broker.ConnectedSessionID)
		return
	}

	// Update all project provider records for this broker
	providers, err := s.store.GetBrokerProjects(ctx, brokerID)
	if err != nil {
		slog.Error("Failed to get broker projects for status update", "brokerID", brokerID, "error", err)
		return
	}
	for _, provider := range providers {
		if err := s.store.UpdateProviderStatus(ctx, provider.ProjectID, brokerID, store.BrokerStatusOffline); err != nil {
			slog.Error("Failed to update provider status", "brokerID", brokerID, "project_id", provider.ProjectID, "error", err)
		}
	}

	// Publish broker disconnected event
	projectIDs := make([]string, len(providers))
	for i, p := range providers {
		projectIDs[i] = p.ProjectID
	}
	s.events.PublishBrokerDisconnected(ctx, brokerID, projectIDs)
}

// brokerProviderSelfHealHandler returns a recurring handler that re-stamps a
// broker's project-provider rows online for every broker this hub instance
// currently holds a live control-channel connection to.
//
// issue #2090: the affinity-owning instance's disconnect callback
// (handleBrokerDisconnect) stamps every project-provider row for that
// broker_id offline. When the same broker_id is served by more than one
// instance or session — e.g. a co-located broker embedded in every replica of
// a multi-instance Hub deployment — the survivors never reconnect, so nothing
// else ever restores those rows: agent-create stays blocked with "Default
// runtime broker is unavailable" until an instance reconnects or restarts.
//
// This closes the gap from the side that can actually observe it: a live
// local socket is per-process, in-memory state, so the check must run on
// every instance rather than as a cluster-wide singleton (contrast
// brokerHeartbeatTimeoutHandler/brokerAffinityReapHandler in reaper.go, which
// are registered with RegisterRecurringSingleton because they only need one
// replica to run them).
//
// It deliberately never touches broker affinity (connected_hub_id /
// connected_session_id): reclaiming affinity from every live instance on
// every tick would fight over routing ownership between replicas that do not
// share underlying agent/container state. That reclaim isn't needed for
// correctness either — broker_routing.go's route() already prefers this
// instance's own live local socket (routeLocal) over the affinity hint, so
// dispatch to a connected instance keeps working without an affinity change.
// Restamping only the provider rows is enough to unblock agent-create, and is
// safe against a genuine broker loss: it is driven by a live local socket,
// not a cached flag, so a broker that is actually gone stops appearing in
// ListConnectedBrokers() and this handler stops touching it. From there,
// handleBrokerDisconnect is the only place that stamps this broker's
// project-provider rows offline — the heartbeat-timeout reaper
// (brokerHeartbeatTimeoutHandler → MarkStaleBrokersOffline) only ever sets
// runtime_brokers.status, never provider rows. The broker row itself is
// stamped offline by handleBrokerDisconnect's CAS (or, as a backstop, by the
// heartbeat-timeout reaper), and getAvailableBrokersForProject requires both
// rows online, so an actually-dead broker stays out of broker selection even
// while its provider rows lag behind it.
func (s *Server) brokerProviderSelfHealHandler() func(ctx context.Context) {
	return func(ctx context.Context) {
		ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
		defer cancel()
		s.selfHealBrokerProviders(ctx, s.controlChannel.ListConnectedBrokers())
	}
}

// selfHealBrokerProviders re-stamps provider rows online for every broker ID
// in snapshot that this instance still holds a live local control-channel
// connection to. snapshot is normally ListConnectedBrokers() taken by the
// caller; it is accepted as a parameter (rather than read here) so tests can
// drive the loop with a snapshot that is already stale with respect to
// s.controlChannel.connections.
//
// Every row this heals from offline to online is announced with the same
// PublishBrokerConnected event markBrokerOnline publishes on a fresh
// connect, one call per broker with exactly the project IDs healed for that
// broker — mirroring handleBrokerDisconnect's PublishBrokerDisconnected on
// the way down, so the event stream stays an accurate record of provider
// status for any consumer of project.<id>.broker.status (the in-tree web
// client does not act on this subject today; see web/src/client/state.ts).
// Nothing is published for a broker where no row changed. A duplicate
// PublishBrokerConnected from two instances healing the same row on the same
// tick is harmless: both carry an identical, stateless status="online"
// payload for the same project ID, so any consumer that treats the event as
// a state fact rather than a counted transition sees no difference from a
// single publish.
func (s *Server) selfHealBrokerProviders(ctx context.Context, snapshot []string) {
	for _, brokerID := range snapshot {
		// snapshot may be stale; re-check right before stamping to narrow the
		// window against a genuine disconnect that removed this brokerID from
		// the connections map in between. This does not close the window
		// (removeConnection can still fire after this check and before the
		// UPDATE below) — the broker-status gate in getAvailableBrokersForProject
		// is the backstop that keeps an actually-dead broker out of agent-create
		// regardless.
		if !s.controlChannel.IsConnected(brokerID) {
			continue
		}

		providers, err := s.store.GetBrokerProjects(ctx, brokerID)
		if err != nil {
			slog.Error("Scheduler: broker provider self-heal failed to list projects", "brokerID", brokerID, "error", err)
			continue
		}
		var healedProjectIDs []string
		for _, provider := range providers {
			// Skip rows already online. Unlike stampProvidersOnline (used by
			// markBrokerOnline on connect, which must keep refreshing
			// last_seen every time), this handler runs every tick on every
			// instance holding a live socket, so re-stamping an
			// already-online row on every tick is pure write load with no
			// effect on status.
			if provider.Status == store.BrokerStatusOnline {
				continue
			}
			if err := s.store.UpdateProviderStatus(ctx, provider.ProjectID, brokerID, store.BrokerStatusOnline); err != nil {
				slog.Error("Failed to update provider status", "brokerID", brokerID, "project_id", provider.ProjectID, "error", err)
				continue
			}
			healedProjectIDs = append(healedProjectIDs, provider.ProjectID)
		}
		if len(healedProjectIDs) == 0 {
			continue
		}
		broker, err := s.store.GetRuntimeBroker(ctx, brokerID)
		var brokerName string
		if err == nil {
			brokerName = broker.Name
		}
		// Only reached when at least one row actually healed; a no-op broker
		// hits the continue above without logging, keeping Info quiet.
		slog.Info("Scheduler: broker provider self-heal restamped providers online", "brokerID", brokerID, "brokerName", brokerName, "count", len(healedProjectIDs))
		s.events.PublishBrokerConnected(ctx, brokerID, brokerName, healedProjectIDs)
	}
}

// isWebSocketUpgrade checks if the request is a WebSocket upgrade request.
func isWebSocketUpgrade(r *http.Request) bool {
	return strings.ToLower(r.Header.Get("Upgrade")) == "websocket" &&
		strings.Contains(strings.ToLower(r.Header.Get("Connection")), "upgrade")
}

// githubResolutionCacheEvictionHandler returns a recurring handler function
// that purges GitHub skill resolution cache entries once they are too old to
// ever be served stale again (see GitHubResolutionStore.PurgeExpired and
// staleCutoff) — not merely once their own TTL has passed, so a branch-ref
// row survives long enough for resolveGitHubSkill's stale-serve path to still
// use it. This prevents the cache table from growing unbounded and keeps
// queries fast.
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

// brokerJoinTokenCleanupHandler returns a recurring handler, run hourly,
// that removes expired broker join tokens. Without it they would stay in
// the table until someone tried to use them or the broker was deleted.
func (s *Server) brokerJoinTokenCleanupHandler() func(ctx context.Context) {
	return func(ctx context.Context) {
		ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
		defer cancel()

		removed, err := s.store.CleanExpiredJoinTokens(ctx)
		if err != nil {
			slog.Error("Scheduler: broker join token cleanup failed", "error", err)
			return
		}
		slog.Debug("Scheduler: broker join token cleanup completed", "removed", removed)
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
	s.mu.RLock()
	mgr := s.pluginManager
	s.mu.RUnlock()
	if mgr == nil {
		return ""
	}
	cfg := mgr.GetPluginConfig("broker", "a2a-bridge")
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

// sweepOrphanedGroupMemberships deletes group memberships whose user and
// agent are both NULL and logs how many it removed. It runs on every startup
// and is idempotent. A failure is logged at Warn and startup continues: the
// rows are inert apart from role counts, and the next startup retries. The
// count is logged at Info only when rows were removed; the usual no-op run
// logs at Debug.
func sweepOrphanedGroupMemberships(ctx context.Context, s store.Store) {
	n, err := s.DeleteOrphanedGroupMemberships(ctx)
	if err != nil {
		slog.Warn("failed to delete orphaned group memberships", "error", err)
		return
	}
	if n > 0 {
		slog.Info("deleted orphaned group memberships", "count", n)
		return
	}
	slog.Debug("deleted orphaned group memberships", "count", n)
}
