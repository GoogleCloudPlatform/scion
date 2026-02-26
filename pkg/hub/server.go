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
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/ptone/scion-agent/pkg/api"
	"github.com/ptone/scion-agent/pkg/secret"
	"github.com/ptone/scion-agent/pkg/storage"
	"github.com/ptone/scion-agent/pkg/store"
	"github.com/ptone/scion-agent/pkg/util/logging"
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
	// EnableAPIKeys enables API key authentication.
	EnableAPIKeys bool
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
	// SoftDeleteRetention is how long soft-deleted agents are retained before purging.
	// Zero means soft-delete is disabled (hard-delete immediately).
	SoftDeleteRetention time.Duration
	// SoftDeleteRetainFiles controls whether workspace files are preserved during soft-delete.
	SoftDeleteRetainFiles bool
	// AdminMode restricts access to admin users only (maintenance mode).
	AdminMode bool
	// MaintenanceMessage is the custom message shown during admin mode.
	MaintenanceMessage string
}

// DefaultServerConfig returns the default server configuration.
func DefaultServerConfig() ServerConfig {
	return ServerConfig{
		Port:         9810,
		Host:         "0.0.0.0",
		ReadTimeout:  30 * time.Second,
		WriteTimeout: 60 * time.Second,
		CORSEnabled:  true,
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
		BrokerAuthConfig:   DefaultBrokerAuthConfig(),
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
	DispatchAgentMessage(ctx context.Context, agent *store.Agent, message string, interrupt bool) error

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
	StartAgent(ctx context.Context, brokerID, brokerEndpoint, agentID, task, grovePath, groveSlug string) (*RemoteAgentResponse, error)

	// StopAgent stops an agent on a remote runtime broker.
	// brokerID is used for HMAC authentication lookup.
	StopAgent(ctx context.Context, brokerID, brokerEndpoint, agentID string) error

	// RestartAgent restarts an agent on a remote runtime broker.
	// brokerID is used for HMAC authentication lookup.
	RestartAgent(ctx context.Context, brokerID, brokerEndpoint, agentID string) error

	// DeleteAgent deletes an agent from a remote runtime broker.
	// brokerID is used for HMAC authentication lookup.
	// softDelete and deletedAt are passed as query params for broker-side marking.
	DeleteAgent(ctx context.Context, brokerID, brokerEndpoint, agentID string, deleteFiles, removeBranch, softDelete bool, deletedAt time.Time) error

	// MessageAgent sends a message to an agent on a remote runtime broker.
	// brokerID is used for HMAC authentication lookup.
	MessageAgent(ctx context.Context, brokerID, brokerEndpoint, agentID, message string, interrupt bool) error

	// CheckAgentPrompt checks if an agent has a non-empty prompt.md file.
	// brokerID is used for HMAC authentication lookup.
	CheckAgentPrompt(ctx context.Context, brokerID, brokerEndpoint, agentID string) (bool, error)

	// FinalizeEnv sends gathered env vars to a broker to complete agent creation
	// after an initial 202 env-gather response.
	FinalizeEnv(ctx context.Context, brokerID, brokerEndpoint, agentID string, env map[string]string) (*RemoteAgentResponse, error)

	// CreateAgentWithGather creates an agent and handles 202 env-gather responses.
	// Returns (response, nil, nil) on success, (nil, envReqs, nil) on 202, or (nil, nil, err) on error.
	CreateAgentWithGather(ctx context.Context, brokerID, brokerEndpoint string, req *RemoteCreateAgentRequest) (*RemoteAgentResponse, *RemoteEnvRequirementsResponse, error)
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
	HubEndpoint string             `json:"hubEndpoint,omitempty"`
	AgentToken  string             `json:"agentToken,omitempty"`
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
}

// ResolvedSecret represents a secret resolved by the Hub for projection into an agent container.
type ResolvedSecret struct {
	Name   string `json:"name"`             // Secret key name
	Type   string `json:"type"`             // environment, variable, file
	Target string `json:"target"`           // Projection target
	Value  string `json:"value"`            // Decrypted secret value
	Source string `json:"source"`           // Scope that provided this secret
	Ref    string `json:"ref,omitempty"`    // External secret reference (e.g., "gcpsm:projects/123/secrets/name")
}

// RemoteAgentConfig contains agent configuration for remote creation.
type RemoteAgentConfig struct {
	Template    string   `json:"template,omitempty"`
	Image       string   `json:"image,omitempty"`
	HomeDir     string   `json:"homeDir,omitempty"`
	Workspace   string   `json:"workspace,omitempty"`
	Env         []string `json:"env,omitempty"`
	Task        string   `json:"task,omitempty"`
	CommandArgs []string `json:"commandArgs,omitempty"`
	Profile     string   `json:"profile,omitempty"` // Settings profile for the runtime broker

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
	Source      string `json:"source"`                // "harness", "template", "settings"
	Type        string `json:"type,omitempty"`         // "environment" (default), "variable", "file"
}

type RemoteEnvRequirementsResponse struct {
	AgentID    string                  `json:"agentId"`
	Required   []string                `json:"required"`
	HubHas     []string                `json:"hubHas"`
	BrokerHas  []string                `json:"brokerHas"`
	Needs      []string                `json:"needs"`
	SecretInfo map[string]SecretKeyInfo `json:"secretInfo,omitempty"`
}

// RemoteAgentInfo contains agent information from a remote runtime broker.
type RemoteAgentInfo struct {
	ID              string `json:"id"`              // Hub UUID
	Slug            string `json:"slug"`            // URL-safe identifier
	ContainerID     string `json:"containerId"`     // Runtime container ID
	Name            string `json:"name"`
	Template        string `json:"template,omitempty"`
	HarnessConfig   string `json:"harnessConfig,omitempty"`
	Runtime         string `json:"runtime,omitempty"`
	Status          string `json:"status"`
	ContainerStatus string `json:"containerStatus,omitempty"`
}

// Server is the Hub API HTTP server.
type Server struct {
	config            ServerConfig
	store             store.Store
	httpServer        *http.Server
	mux               *http.ServeMux
	mu                sync.RWMutex
	startTime         time.Time
	dispatcher        AgentDispatcher     // Optional dispatcher for co-located runtime broker
	storage           storage.Storage     // Optional storage backend for templates
	secretBackend     secret.SecretBackend // Optional secret backend
	agentTokenService *AgentTokenService  // Agent JWT token service
	userTokenService  *UserTokenService   // User JWT token service
	apiKeyService     *APIKeyService      // API key service
	oauthService      *OAuthService       // OAuth service for CLI authentication
	authConfig        AuthConfig          // Unified auth configuration
	brokerAuthService   *BrokerAuthService    // Broker HMAC authentication service
	auditLogger       AuditLogger         // Audit logger for security events
	metrics           MetricsRecorder     // Metrics recorder for broker auth
	controlChannel            *ControlChannelManager // WebSocket control channel for runtime brokers
	authzService              *AuthzService       // Authorization service for policy evaluation
	events                    EventPublisher      // Event publisher for real-time SSE updates
	notificationDispatcher    *NotificationDispatcher // Notification dispatcher for agent status events
}

// New creates a new Hub API server.
func New(cfg ServerConfig, s store.Store) *Server {
	srv := &Server{
		config:    cfg,
		store:     s,
		mux:       http.NewServeMux(),
		startTime: time.Now(),
		events:    noopEventPublisher{},
	}

	ctx := context.Background()

	// Initialize agent token service
	agentKey, err := srv.ensureSigningKey(ctx, SecretKeyAgentSigningKey, cfg.AgentTokenConfig.SigningKey)
	if err == nil {
		cfg.AgentTokenConfig.SigningKey = agentKey
	}
	tokenService, err := NewAgentTokenService(cfg.AgentTokenConfig)
	if err != nil {
		slog.Warn("Failed to initialize agent token service", "error", err)
	} else {
		srv.agentTokenService = tokenService
	}

	// Initialize user token service
	userKey, err := srv.ensureSigningKey(ctx, SecretKeyUserSigningKey, cfg.UserTokenConfig.SigningKey)
	if err == nil {
		cfg.UserTokenConfig.SigningKey = userKey
	}
	userTokenService, err := NewUserTokenService(cfg.UserTokenConfig)
	if err != nil {
		slog.Warn("Failed to initialize user token service", "error", err)
	} else {
		srv.userTokenService = userTokenService
	}

	// Initialize API key service if enabled
	if cfg.EnableAPIKeys {
		srv.apiKeyService = NewAPIKeyService(s, s)
	}

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
	})
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
					slog.Error("Failed to update provider status", "brokerID", brokerID, "groveID", provider.GroveID, "error", err)
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

	// Initialize authorization service
	srv.authzService = NewAuthzService(s, slog.Default())

	// Seed default policies and groups (idempotent)
	seedDefaultPoliciesAndGroups(ctx, s)

	// Build unified auth configuration
	srv.authConfig = AuthConfig{
		Mode:           "production",
		DevAuthEnabled: cfg.DevAuthToken != "",
		DevAuthToken:   cfg.DevAuthToken,
		AgentTokenSvc:  srv.agentTokenService,
		UserTokenSvc:   srv.userTokenService,
		APIKeySvc:      srv.apiKeyService,
		TrustedProxies: cfg.TrustedProxies,
		Debug:          cfg.Debug,
	}

	srv.registerRoutes()

	return srv
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
		return existingKey, nil
	}

	// Try to load from the secret backend if configured
	if s.secretBackend != nil {
		sv, err := s.secretBackend.Get(ctx, keyName, store.ScopeHub, "hub")
		if err == nil {
			slog.Info("Loaded existing signing key from secret backend", "key", keyName)
			return base64.StdEncoding.DecodeString(sv.Value)
		}
		if err != store.ErrNotFound {
			slog.Warn("Failed to load signing key from secret backend, trying store", "key", keyName, "error", err)
		}
	}

	// Fallback: try loading from the store directly (for migration/local dev)
	val, err := s.store.GetSecretValue(ctx, keyName, store.ScopeHub, "hub")
	if err == nil {
		slog.Info("Loaded existing signing key from store", "key", keyName)
		return base64.StdEncoding.DecodeString(val)
	}
	if err != store.ErrNotFound {
		return nil, fmt.Errorf("failed to load signing key %s from store: %w", keyName, err)
	}

	// Not found anywhere, generate a new one
	newKey := make([]byte, 32)
	if _, err := rand.Read(newKey); err != nil {
		return nil, fmt.Errorf("failed to generate random signing key: %w", err)
	}

	encodedKey := base64.StdEncoding.EncodeToString(newKey)

	// Try saving through the secret backend first
	if s.secretBackend != nil {
		input := &secret.SetSecretInput{
			Name:        keyName,
			Value:       encodedKey,
			Scope:       store.ScopeHub,
			ScopeID:     "hub",
			Description: fmt.Sprintf("Hub signing key for %s", keyName),
		}
		if _, _, err := s.secretBackend.Set(ctx, input); err == nil {
			slog.Info("Persisted new signing key via secret backend", "key", keyName)
			return newKey, nil
		} else {
			slog.Warn("Secret backend unavailable for signing key, falling back to store", "key", keyName, "error", err)
		}
	}

	// Fallback: save directly to the store (hub-internal key, acceptable for local dev)
	sec := &store.Secret{
		ID:             fmt.Sprintf("hub-%s", keyName),
		Key:            keyName,
		EncryptedValue: encodedKey,
		Scope:          store.ScopeHub,
		ScopeID:        "hub",
		Description:    fmt.Sprintf("Hub signing key for %s", keyName),
	}
	if _, err := s.store.UpsertSecret(ctx, sec); err != nil {
		slog.Warn("Failed to persist signing key", "key", keyName, "error", err)
	} else {
		slog.Info("Persisted new signing key to store", "key", keyName)
	}

	return newKey, nil
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

// SetStorage sets the storage backend for template files.
func (s *Server) SetStorage(stor storage.Storage) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.storage = stor
}

// GetStorage returns the current storage backend.
func (s *Server) GetStorage() storage.Storage {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.storage
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

// GetAPIKeyService returns the API key service.
func (s *Server) GetAPIKeyService() *APIKeyService {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.apiKeyService
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

// SetEventPublisher sets the event publisher for real-time SSE updates.
func (s *Server) SetEventPublisher(ep EventPublisher) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.events = ep
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
		client = NewHybridBrokerClient(s.controlChannel, httpClient, s.config.Debug)
	} else {
		client = httpClient
	}

	dispatcher := NewHTTPAgentDispatcherWithClient(s.store, client, s.config.Debug)

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

	// Pass secret backend to dispatcher if configured
	if s.secretBackend != nil {
		dispatcher.SetSecretBackend(s.secretBackend)
	}

	// In dev-auth mode, pass the dev token so agents get it for fallback auth
	if s.config.DevAuthToken != "" {
		dispatcher.SetDevAuthToken(s.config.DevAuthToken)
	}

	return dispatcher
}

// GenerateAgentToken generates a JWT for an agent.
// This is a convenience method that delegates to the token service.
// Additional scopes are merged with the default ScopeAgentStatusUpdate scope.
func (s *Server) GenerateAgentToken(agentID, groveID string, additionalScopes ...AgentTokenScope) (string, error) {
	s.mu.RLock()
	tokenService := s.agentTokenService
	s.mu.RUnlock()

	if tokenService == nil {
		return "", fmt.Errorf("agent token service not initialized")
	}

	scopes := []AgentTokenScope{ScopeAgentStatusUpdate}

	// In dev-auth mode, auto-grant agent creation and lifecycle scopes
	// so agents can create sub-agents without explicit template configuration.
	if s.config.DevAuthToken != "" {
		scopes = append(scopes, ScopeAgentCreate, ScopeAgentLifecycle, ScopeAgentNotify)
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

	return tokenService.GenerateAgentToken(agentID, groveID, scopes)
}

// Start starts the HTTP server.
// startPurgeLoop starts a background goroutine that periodically purges expired
// soft-deleted agents. It's a no-op if soft-delete retention is not configured.
func (s *Server) startPurgeLoop(ctx context.Context) {
	retention := s.config.SoftDeleteRetention
	if retention <= 0 {
		return
	}

	slog.Info("Starting soft-delete purge loop", "retention", retention, "interval", "1h")

	go func() {
		// Run immediately on startup
		s.purgeExpiredAgents(ctx)

		ticker := time.NewTicker(1 * time.Hour)
		defer ticker.Stop()

		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				s.purgeExpiredAgents(ctx)
			}
		}
	}()
}

// purgeExpiredAgents permanently removes soft-deleted agents that have exceeded
// the retention period.
func (s *Server) purgeExpiredAgents(ctx context.Context) {
	cutoff := time.Now().Add(-s.config.SoftDeleteRetention)
	purged, err := s.store.PurgeDeletedAgents(ctx, cutoff)
	if err != nil {
		slog.Error("Failed to purge deleted agents", "error", err)
		return
	}
	if purged > 0 {
		slog.Info("Purged expired soft-deleted agents", "count", purged, "cutoff", cutoff)
	}
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

	// Start the purge loop for soft-deleted agents
	s.startPurgeLoop(ctx)

	// Start notification dispatcher if event publisher supports subscriptions
	if ep, ok := s.events.(*ChannelEventPublisher); ok {
		s.notificationDispatcher = NewNotificationDispatcher(s.store, ep, s.GetDispatcher())
		s.notificationDispatcher.Start()
	}

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
	if s.notificationDispatcher != nil {
		s.notificationDispatcher.Stop()
	}
	if s.events != nil {
		s.events.Close()
	}
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
	s.mux.HandleFunc("/api/v1/auth/api-keys", s.handleAPIKeys)
	s.mux.HandleFunc("/api/v1/auth/api-keys/", s.handleAPIKeyByID)

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

	// Notification endpoints (user-facing)
	s.mux.HandleFunc("/api/v1/notifications", s.handleNotifications)
	s.mux.HandleFunc("/api/v1/notifications/", s.handleNotificationRoutes)

	// WebSocket control channel endpoint for Runtime Brokers
	s.mux.HandleFunc("/api/v1/runtime-brokers/connect", s.handleRuntimeBrokerConnect)
}

// applyMiddleware wraps the handler with middleware.
func (s *Server) applyMiddleware(h http.Handler) http.Handler {
	// Apply middleware in reverse order (last applied runs first)
	h = s.recoveryMiddleware(h)
	h = s.loggingMiddleware(h)

	// Apply broker auth middleware (checks X-Scion-Broker-ID header for HMAC auth)
	// This runs after unified auth but before the handler, allowing hosts to authenticate
	if s.brokerAuthService != nil {
		if s.auditLogger != nil {
			h = AuditableBrokerAuthMiddleware(s.brokerAuthService, s.auditLogger)(h)
		} else {
			h = BrokerAuthMiddleware(s.brokerAuthService)(h)
		}
	}

	// Apply admin mode middleware (after auth, so identity is available)
	if s.config.AdminMode {
		h = adminModeMiddleware(s.config.MaintenanceMessage)(h)
	}

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

		// Extract contextual metadata for logging
		// In the future, this could extract trace IDs from headers
		traceID := r.Header.Get("X-Cloud-Trace-Context")
		if traceID == "" {
			traceID = r.Header.Get("X-Trace-ID")
		}

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
			slog.Error("Failed to update provider status", "brokerID", brokerID, "groveID", provider.GroveID, "error", err)
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
