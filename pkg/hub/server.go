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

	// DispatchAgentStart resumes a stopped agent on the runtime broker.
	DispatchAgentStart(ctx context.Context, agent *store.Agent) error

	// DispatchAgentStop stops a running agent on the runtime broker.
	DispatchAgentStop(ctx context.Context, agent *store.Agent) error

	// DispatchAgentRestart restarts an agent on the runtime broker.
	DispatchAgentRestart(ctx context.Context, agent *store.Agent) error

	// DispatchAgentDelete removes an agent from the runtime broker.
	// deleteFiles indicates whether to delete workspace files.
	// removeBranch indicates whether to remove the git branch.
	DispatchAgentDelete(ctx context.Context, agent *store.Agent, deleteFiles, removeBranch bool) error

	// DispatchAgentMessage sends a message to an agent on the runtime broker.
	DispatchAgentMessage(ctx context.Context, agent *store.Agent, message string, interrupt bool) error

	// DispatchCheckAgentPrompt checks if an agent has a non-empty prompt.md file.
	DispatchCheckAgentPrompt(ctx context.Context, agent *store.Agent) (bool, error)
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
	StartAgent(ctx context.Context, brokerID, brokerEndpoint, agentID string) error

	// StopAgent stops an agent on a remote runtime broker.
	// brokerID is used for HMAC authentication lookup.
	StopAgent(ctx context.Context, brokerID, brokerEndpoint, agentID string) error

	// RestartAgent restarts an agent on a remote runtime broker.
	// brokerID is used for HMAC authentication lookup.
	RestartAgent(ctx context.Context, brokerID, brokerEndpoint, agentID string) error

	// DeleteAgent deletes an agent from a remote runtime broker.
	// brokerID is used for HMAC authentication lookup.
	DeleteAgent(ctx context.Context, brokerID, brokerEndpoint, agentID string, deleteFiles, removeBranch bool) error

	// MessageAgent sends a message to an agent on a remote runtime broker.
	// brokerID is used for HMAC authentication lookup.
	MessageAgent(ctx context.Context, brokerID, brokerEndpoint, agentID, message string, interrupt bool) error

	// CheckAgentPrompt checks if an agent has a non-empty prompt.md file.
	// brokerID is used for HMAC authentication lookup.
	CheckAgentPrompt(ctx context.Context, brokerID, brokerEndpoint, agentID string) (bool, error)
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
	HubEndpoint string             `json:"hubEndpoint,omitempty"`
	AgentToken  string             `json:"agentToken,omitempty"`
	// GrovePath is the local filesystem path to the grove on the target runtime broker.
	// This is looked up from the grove provider record for the target broker.
	GrovePath string `json:"grovePath,omitempty"`
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

	// TemplateID is the Hub template ID for cache lookup on the Runtime Broker.
	// When provided, the Runtime Broker can use this to fetch the template
	// from the Hub and cache it locally.
	TemplateID string `json:"templateId,omitempty"`

	// TemplateHash is the content hash of the template for cache validation.
	// If the cached template's hash matches, it can be used without re-downloading.
	TemplateHash string `json:"templateHash,omitempty"`
}

// RemoteAgentResponse is the response from creating an agent on a remote runtime broker.
type RemoteAgentResponse struct {
	Agent   *RemoteAgentInfo `json:"agent,omitempty"`
	Created bool             `json:"created"`
}

// RemoteAgentInfo contains agent information from a remote runtime broker.
type RemoteAgentInfo struct {
	ID              string `json:"id"`              // Hub UUID
	Slug            string `json:"slug"`            // URL-safe identifier
	ContainerID     string `json:"containerId"`     // Runtime container ID
	Name            string `json:"name"`
	Template        string `json:"template,omitempty"`
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
	agentTokenService *AgentTokenService  // Agent JWT token service
	userTokenService  *UserTokenService   // User JWT token service
	apiKeyService     *APIKeyService      // API key service
	oauthService      *OAuthService       // OAuth service for CLI authentication
	authConfig        AuthConfig          // Unified auth configuration
	brokerAuthService   *BrokerAuthService    // Broker HMAC authentication service
	auditLogger       AuditLogger         // Audit logger for security events
	metrics           MetricsRecorder     // Metrics recorder for broker auth
	controlChannel    *ControlChannelManager // WebSocket control channel for runtime brokers
}

// New creates a new Hub API server.
func New(cfg ServerConfig, s store.Store) *Server {
	srv := &Server{
		config:    cfg,
		store:     s,
		mux:       http.NewServeMux(),
		startTime: time.Now(),
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
		}
	})
	slog.Info("Control channel manager initialized")

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

// ensureSigningKey ensures a signing key exists in the store, loading it if it does
// or generating and saving it if it doesn't.
// TODO: This should ultimately be replaced with a proper secret management backing service.
func (s *Server) ensureSigningKey(ctx context.Context, keyName string, existingKey []byte) ([]byte, error) {
	if len(existingKey) > 0 {
		return existingKey, nil
	}

	// Try to load from store
	val, err := s.store.GetSecretValue(ctx, keyName, store.ScopeHub, "hub")
	if err == nil {
		slog.Info("Loaded existing signing key from store", "key", keyName)
		return base64.StdEncoding.DecodeString(val)
	}

	if err != store.ErrNotFound {
		return nil, fmt.Errorf("failed to load signing key %s from store: %w", keyName, err)
	}

	// Not found, generate a new one
	newKey := make([]byte, 32)
	if _, err := rand.Read(newKey); err != nil {
		return nil, fmt.Errorf("failed to generate random signing key: %w", err)
	}

	// Save to store
	secret := &store.Secret{
		ID:             fmt.Sprintf("hub-%s", keyName),
		Key:            keyName,
		EncryptedValue: base64.StdEncoding.EncodeToString(newKey),
		Scope:          store.ScopeHub,
		ScopeID:        "hub",
		Description:    fmt.Sprintf("Hub signing key for %s", keyName),
	}

	if _, err := s.store.UpsertSecret(ctx, secret); err != nil {
		// Log warning but continue - we will have a random key for this session
		slog.Warn("Failed to persist signing key", "key", keyName, "error", err)
	} else {
		slog.Info("Persisted new signing key", "key", keyName)
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

	return dispatcher
}

// GenerateAgentToken generates a JWT for an agent.
// This is a convenience method that delegates to the token service.
func (s *Server) GenerateAgentToken(agentID, groveID string) (string, error) {
	s.mu.RLock()
	tokenService := s.agentTokenService
	s.mu.RUnlock()

	if tokenService == nil {
		return "", fmt.Errorf("agent token service not initialized")
	}

	return tokenService.GenerateAgentToken(agentID, groveID, []AgentTokenScope{ScopeAgentStatusUpdate})
}

// Start starts the HTTP server.
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

	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	return srv.Shutdown(ctx)
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

	// Broker registration endpoints (Runtime Broker HMAC authentication)
	s.mux.HandleFunc("/api/v1/brokers", s.handleBrokersEndpoint)
	s.mux.HandleFunc("/api/v1/brokers/join", s.handleBrokerJoin)
	s.mux.HandleFunc("/api/v1/brokers/", s.handleBrokerByIDRoutes)

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
}

// isWebSocketUpgrade checks if the request is a WebSocket upgrade request.
func isWebSocketUpgrade(r *http.Request) bool {
	return strings.ToLower(r.Header.Get("Upgrade")) == "websocket" &&
		strings.Contains(strings.ToLower(r.Header.Get("Connection")), "upgrade")
}
