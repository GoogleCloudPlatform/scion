package hub

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/ptone/scion-agent/pkg/storage"
	"github.com/ptone/scion-agent/pkg/store"
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
	// HostAuthConfig holds configuration for Runtime Host HMAC authentication.
	HostAuthConfig HostAuthConfig
	// HubEndpoint is the public endpoint URL for this Hub (used in host join responses).
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
			"X-Scion-Host-Token", "X-Scion-Agent-Token", "X-API-Key",
			// Host HMAC authentication headers
			"X-Scion-Host-ID", "X-Scion-Timestamp", "X-Scion-Nonce",
			"X-Scion-Signature", "X-Scion-Signed-Headers",
		},
		CORSMaxAge:       3600,
		HostAuthConfig:   DefaultHostAuthConfig(),
	}
}

// AgentDispatcher is the interface for dispatching agent operations to a runtime host.
// Implementations may be local (co-located hub+host) or remote (HTTP-based).
type AgentDispatcher interface {
	// DispatchAgentCreate creates and starts an agent on the runtime host.
	// Returns the updated agent info after creation/start.
	DispatchAgentCreate(ctx context.Context, agent *store.Agent) error

	// DispatchAgentStart resumes a stopped agent on the runtime host.
	DispatchAgentStart(ctx context.Context, agent *store.Agent) error

	// DispatchAgentStop stops a running agent on the runtime host.
	DispatchAgentStop(ctx context.Context, agent *store.Agent) error

	// DispatchAgentRestart restarts an agent on the runtime host.
	DispatchAgentRestart(ctx context.Context, agent *store.Agent) error

	// DispatchAgentDelete removes an agent from the runtime host.
	// deleteFiles indicates whether to delete workspace files.
	// removeBranch indicates whether to remove the git branch.
	DispatchAgentDelete(ctx context.Context, agent *store.Agent, deleteFiles, removeBranch bool) error

	// DispatchAgentMessage sends a message to an agent on the runtime host.
	DispatchAgentMessage(ctx context.Context, agent *store.Agent, message string, interrupt bool) error
}

// RuntimeHostClient is an interface for communicating with runtime hosts over HTTP.
// This allows the hub to dispatch operations to remote runtime hosts.
// All methods take a hostID parameter which is used for HMAC authentication when
// the client supports it (AuthenticatedHostClient).
type RuntimeHostClient interface {
	// CreateAgent creates an agent on a remote runtime host.
	// hostID is used for HMAC authentication lookup.
	CreateAgent(ctx context.Context, hostID, hostEndpoint string, req *RemoteCreateAgentRequest) (*RemoteAgentResponse, error)

	// StartAgent starts an agent on a remote runtime host.
	// hostID is used for HMAC authentication lookup.
	StartAgent(ctx context.Context, hostID, hostEndpoint, agentID string) error

	// StopAgent stops an agent on a remote runtime host.
	// hostID is used for HMAC authentication lookup.
	StopAgent(ctx context.Context, hostID, hostEndpoint, agentID string) error

	// RestartAgent restarts an agent on a remote runtime host.
	// hostID is used for HMAC authentication lookup.
	RestartAgent(ctx context.Context, hostID, hostEndpoint, agentID string) error

	// DeleteAgent deletes an agent from a remote runtime host.
	// hostID is used for HMAC authentication lookup.
	DeleteAgent(ctx context.Context, hostID, hostEndpoint, agentID string, deleteFiles, removeBranch bool) error

	// MessageAgent sends a message to an agent on a remote runtime host.
	// hostID is used for HMAC authentication lookup.
	MessageAgent(ctx context.Context, hostID, hostEndpoint, agentID, message string, interrupt bool) error
}

// RemoteCreateAgentRequest is the request body for creating an agent on a remote runtime host.
type RemoteCreateAgentRequest struct {
	RequestID   string            `json:"requestId,omitempty"`
	AgentID     string            `json:"agentId"`
	Name        string            `json:"name"`
	GroveID     string            `json:"groveId"`
	UserID      string            `json:"userId,omitempty"`
	Config      *RemoteAgentConfig `json:"config,omitempty"`
	ResolvedEnv map[string]string `json:"resolvedEnv,omitempty"`
	HubEndpoint string            `json:"hubEndpoint,omitempty"`
	AgentToken  string            `json:"agentToken,omitempty"`
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

	// TemplateID is the Hub template ID for cache lookup on the Runtime Host.
	// When provided, the Runtime Host can use this to fetch the template
	// from the Hub and cache it locally.
	TemplateID string `json:"templateId,omitempty"`

	// TemplateHash is the content hash of the template for cache validation.
	// If the cached template's hash matches, it can be used without re-downloading.
	TemplateHash string `json:"templateHash,omitempty"`
}

// RemoteAgentResponse is the response from creating an agent on a remote runtime host.
type RemoteAgentResponse struct {
	Agent   *RemoteAgentInfo `json:"agent,omitempty"`
	Created bool             `json:"created"`
}

// RemoteAgentInfo contains agent information from a remote runtime host.
type RemoteAgentInfo struct {
	ID              string `json:"id"`
	AgentID         string `json:"agentId"`
	Name            string `json:"name"`
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
	dispatcher        AgentDispatcher     // Optional dispatcher for co-located runtime host
	storage           storage.Storage     // Optional storage backend for templates
	agentTokenService *AgentTokenService  // Agent JWT token service
	userTokenService  *UserTokenService   // User JWT token service
	apiKeyService     *APIKeyService      // API key service
	oauthService      *OAuthService       // OAuth service for CLI authentication
	authConfig        AuthConfig          // Unified auth configuration
	hostAuthService   *HostAuthService    // Host HMAC authentication service
	auditLogger       AuditLogger         // Audit logger for security events
	metrics           MetricsRecorder     // Metrics recorder for host auth
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
		log.Printf("[Hub] Warning: failed to initialize agent token service: %v", err)
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
		log.Printf("[Hub] Warning: failed to initialize user token service: %v", err)
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
		log.Printf("[Hub] OAuth service initialized")
		// Log which providers are configured
		logOAuthProviders("Web", cfg.OAuthConfig.Web)
		logOAuthProviders("CLI", cfg.OAuthConfig.CLI)
	} else {
		log.Printf("[Hub] OAuth service NOT configured - no providers available")
		log.Printf("[Hub] To enable OAuth, set environment variables:")
		log.Printf("[Hub]   SCION_SERVER_OAUTH_CLI_GOOGLE_CLIENTID")
		log.Printf("[Hub]   SCION_SERVER_OAUTH_CLI_GOOGLE_CLIENTSECRET")
		log.Printf("[Hub]   (or use server.yaml configuration)")
	}

	// Log authorized domains if configured
	if len(cfg.AuthorizedDomains) > 0 {
		log.Printf("[Hub] Authorized domains: %s", strings.Join(cfg.AuthorizedDomains, ", "))
	}

	// Initialize host auth service if enabled
	if cfg.HostAuthConfig.Enabled {
		srv.hostAuthService = NewHostAuthService(cfg.HostAuthConfig, s)
		srv.auditLogger = NewLogAuditLogger("[Hub Audit]", cfg.Debug)
		srv.metrics = NewHostAuthMetrics()
		log.Printf("[Hub] Host HMAC authentication enabled")
	}

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
		log.Printf("[Hub] Loaded existing signing key from store: %s", keyName)
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
		log.Printf("[Hub] Warning: failed to persist signing key %s: %v", keyName, err)
	} else {
		log.Printf("[Hub] Persisted new signing key: %s", keyName)
	}

	return newKey, nil
}

// SetDispatcher sets the agent dispatcher for co-located runtime host operations.
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

// GetHostAuthService returns the host authentication service.
func (s *Server) GetHostAuthService() *HostAuthService {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.hostAuthService
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

// CreateAuthenticatedDispatcher creates an HTTPAgentDispatcher with authenticated
// host communication. This dispatcher signs outgoing requests to Runtime Hosts
// using HMAC authentication based on shared secrets stored in the database.
func (s *Server) CreateAuthenticatedDispatcher() *HTTPAgentDispatcher {
	client := NewAuthenticatedHostClient(s.store, s.config.Debug)
	dispatcher := NewHTTPAgentDispatcherWithClient(s.store, client, s.config.Debug)

	// Configure token generator if available
	if s.agentTokenService != nil {
		dispatcher.SetTokenGenerator(s)
	}

	// Set Hub endpoint if configured
	if s.config.HubEndpoint != "" {
		dispatcher.SetHubEndpoint(s.config.HubEndpoint)
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

	log.Printf("Hub API server starting on %s:%d", s.config.Host, s.config.Port)

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
	s.mu.RUnlock()

	if srv == nil {
		return nil
	}

	log.Println("Hub API server shutting down...")

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

	s.mux.HandleFunc("/api/v1/runtime-hosts", s.handleRuntimeHosts)
	s.mux.HandleFunc("/api/v1/runtime-hosts/", s.handleRuntimeHostRoutes)

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

	// Host registration endpoints (Runtime Host HMAC authentication)
	s.mux.HandleFunc("/api/v1/hosts", s.handleHostsEndpoint)
	s.mux.HandleFunc("/api/v1/hosts/join", s.handleHostJoin)
	s.mux.HandleFunc("/api/v1/hosts/", s.handleHostByIDRoutes)
}

// applyMiddleware wraps the handler with middleware.
func (s *Server) applyMiddleware(h http.Handler) http.Handler {
	// Apply middleware in reverse order (last applied runs first)
	h = s.recoveryMiddleware(h)
	h = s.loggingMiddleware(h)

	// Apply host auth middleware (checks X-Scion-Host-ID header for HMAC auth)
	// This runs after unified auth but before the handler, allowing hosts to authenticate
	if s.hostAuthService != nil {
		if s.auditLogger != nil {
			h = AuditableHostAuthMiddleware(s.hostAuthService, s.auditLogger)(h)
		} else {
			h = HostAuthMiddleware(s.hostAuthService)(h)
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

		if s.config.Debug {
			log.Printf("[Hub] --> %s %s (from %s)", r.Method, r.URL.Path, r.RemoteAddr)
			if r.URL.RawQuery != "" {
				log.Printf("[Hub]     query: %s", r.URL.RawQuery)
			}
			for name, values := range r.Header {
				if name == "Authorization" {
					log.Printf("[Hub]     header: %s: [REDACTED]", name)
				} else {
					log.Printf("[Hub]     header: %s: %s", name, strings.Join(values, ", "))
				}
			}
		}

		next.ServeHTTP(wrapped, r)

		if s.config.Debug {
			log.Printf("[Hub] <-- %s %s %d (%s)",
				r.Method, r.URL.Path, wrapped.statusCode, time.Since(start))
		} else {
			log.Printf("%s %s %d %s",
				r.Method, r.URL.Path, wrapped.statusCode, time.Since(start))
		}
	})
}

// recoveryMiddleware recovers from panics.
func (s *Server) recoveryMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if err := recover(); err != nil {
				log.Printf("panic recovered: %v", err)
				InternalError(w)
			}
		}()
		next.ServeHTTP(w, r)
	})
}

// responseWriter wraps http.ResponseWriter to capture status code.
type responseWriter struct {
	http.ResponseWriter
	statusCode int
}

func (rw *responseWriter) WriteHeader(code int) {
	rw.statusCode = code
	rw.ResponseWriter.WriteHeader(code)
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
		log.Printf("[Hub]   %s OAuth: %s", clientType, strings.Join(providers, ", "))
	} else {
		log.Printf("[Hub]   %s OAuth: none configured", clientType)
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
