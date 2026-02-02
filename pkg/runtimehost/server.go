// Package runtimehost provides the Scion Runtime Host API server.
// The Runtime Host API exposes agent lifecycle management over HTTP,
// allowing the Scion Hub to remotely manage agents on this compute node.
package runtimehost

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/ptone/scion-agent/pkg/agent"
	"github.com/ptone/scion-agent/pkg/config"
	"github.com/ptone/scion-agent/pkg/hostcredentials"
	"github.com/ptone/scion-agent/pkg/hubclient"
	"github.com/ptone/scion-agent/pkg/runtime"
	"github.com/ptone/scion-agent/pkg/templatecache"
)

// ServerConfig holds configuration for the Runtime Host API server.
type ServerConfig struct {
	// Port is the HTTP port to listen on.
	Port int
	// Host is the address to bind to (e.g., "0.0.0.0" or "127.0.0.1").
	Host string
	// ReadTimeout is the maximum duration for reading the entire request.
	ReadTimeout time.Duration
	// WriteTimeout is the maximum duration before timing out writes.
	WriteTimeout time.Duration

	// Mode is the operational mode (currently only "connected" is supported).
	Mode string
	// HubEndpoint is the Hub API endpoint for reporting (optional).
	HubEndpoint string

	// HostID is a unique identifier for this runtime host.
	HostID string
	// HostName is a human-readable name for this runtime host.
	HostName string

	// CORS settings
	CORSEnabled        bool
	CORSAllowedOrigins []string
	CORSAllowedMethods []string
	CORSAllowedHeaders []string
	CORSMaxAge         int

	// Debug enables verbose debug logging.
	Debug bool

	// Hub integration settings
	// HubEnabled indicates whether this Runtime Host should connect to a Hub
	// for template hydration and other centralized services.
	HubEnabled bool
	// HubToken is the authentication token for the Hub API.
	HubToken string

	// Template cache settings
	// TemplateCacheDir is the directory for caching templates fetched from the Hub.
	// Defaults to ~/.scion/cache/templates if not specified.
	TemplateCacheDir string
	// TemplateCacheMaxSize is the maximum size of the template cache in bytes.
	// Defaults to 100MB if not specified.
	TemplateCacheMaxSize int64

	// Host credentials settings
	// HostCredentialsPath is the path to the host credentials file.
	// If set, HMAC authentication will be used instead of bearer tokens.
	// Defaults to ~/.scion/host-credentials.json if not specified.
	HostCredentialsPath string

	// HostAuthEnabled enables HMAC verification for incoming requests from the Hub.
	HostAuthEnabled bool
	// HostAuthStrictMode, when true, requires all requests to be authenticated.
	// When false (default), unauthenticated requests are allowed for transition periods.
	HostAuthStrictMode bool

	// Heartbeat settings
	// HeartbeatEnabled enables periodic heartbeats to the Hub.
	HeartbeatEnabled bool
	// HeartbeatInterval is the time between heartbeats.
	// Defaults to 30 seconds if not specified.
	HeartbeatInterval time.Duration
}

// DefaultServerConfig returns the default server configuration.
func DefaultServerConfig() ServerConfig {
	return ServerConfig{
		Port:         9800,
		Host:         "0.0.0.0",
		ReadTimeout:  30 * time.Second,
		WriteTimeout: 120 * time.Second,
		Mode:         config.RuntimeHostModeConnected,
		CORSEnabled:  true,
		CORSAllowedOrigins: []string{"*"},
		CORSAllowedMethods: []string{"GET", "POST", "PUT", "PATCH", "DELETE", "OPTIONS"},
		CORSAllowedHeaders: []string{"Authorization", "Content-Type", "X-Scion-Host-Token", "X-API-Key", "X-Scion-Host-ID", "X-Scion-Timestamp", "X-Scion-Nonce", "X-Scion-Signature", "X-Scion-Signed-Headers"},
		CORSMaxAge:         3600,
	}
}

// Server is the Runtime Host API HTTP server.
type Server struct {
	config     ServerConfig
	manager    agent.Manager
	runtime    runtime.Runtime
	httpServer *http.Server
	mux        *http.ServeMux
	mu         sync.RWMutex
	startTime  time.Time
	version    string

	// Hub integration
	hubClient hubclient.Client
	cache     *templatecache.Cache
	hydrator  *templatecache.Hydrator

	// Authentication and heartbeat
	hostAuthMiddleware *HostAuthMiddleware
	heartbeat          *HeartbeatService
	hostCredentials    *hostcredentials.HostCredentials
}

// New creates a new Runtime Host API server.
func New(cfg ServerConfig, mgr agent.Manager, rt runtime.Runtime) *Server {
	srv := &Server{
		config:    cfg,
		manager:   mgr,
		runtime:   rt,
		mux:       http.NewServeMux(),
		startTime: time.Now(),
		version:   "0.1.0", // TODO: Get from build info
	}

	// Initialize Hub integration if enabled
	if cfg.HubEnabled && cfg.HubEndpoint != "" {
		if err := srv.initHubIntegration(); err != nil {
			log.Printf("Warning: failed to initialize Hub integration: %v", err)
		}
	}

	srv.registerRoutes()

	return srv
}

// initHubIntegration initializes the Hub client, template cache, and hydrator.
func (s *Server) initHubIntegration() error {
	// Determine cache directory
	cacheDir := s.config.TemplateCacheDir
	if cacheDir == "" {
		homeDir, err := os.UserHomeDir()
		if err != nil {
			return fmt.Errorf("failed to get home directory: %w", err)
		}
		cacheDir = filepath.Join(homeDir, ".scion", "cache", "templates")
	}

	// Determine cache max size
	maxSize := s.config.TemplateCacheMaxSize
	if maxSize <= 0 {
		maxSize = templatecache.DefaultMaxSize
	}

	// Initialize template cache
	cache, err := templatecache.New(cacheDir, maxSize)
	if err != nil {
		return fmt.Errorf("failed to initialize template cache: %w", err)
	}
	s.cache = cache

	// Try to load host credentials for HMAC auth
	var secretKey []byte
	if err := s.loadHostCredentials(); err == nil && s.hostCredentials != nil {
		// Decode the secret key
		secretKey, err = base64.StdEncoding.DecodeString(s.hostCredentials.SecretKey)
		if err != nil {
			log.Printf("Warning: failed to decode host secret key: %v", err)
		}
	}

	// Initialize Hub client with appropriate auth
	opts := []hubclient.Option{}

	if len(secretKey) > 0 && s.hostCredentials != nil {
		// Use HMAC auth from credentials
		opts = append(opts, hubclient.WithHMACAuth(s.hostCredentials.HostID, secretKey))
		log.Printf("Hub client using HMAC authentication (hostID: %s)", s.hostCredentials.HostID)

		// Update HostID from credentials if not already set
		if s.config.HostID == "" {
			s.config.HostID = s.hostCredentials.HostID
		}
	} else if s.config.HubToken != "" {
		// Fall back to bearer token
		opts = append(opts, hubclient.WithBearerToken(s.config.HubToken))
		log.Printf("Hub client using bearer token authentication")
	} else {
		// Try auto dev auth
		opts = append(opts, hubclient.WithAutoDevAuth())
		log.Printf("Hub client using auto dev authentication")
	}

	client, err := hubclient.New(s.config.HubEndpoint, opts...)
	if err != nil {
		return fmt.Errorf("failed to create Hub client: %w", err)
	}
	s.hubClient = client

	// Initialize hydrator
	s.hydrator = templatecache.NewHydrator(s.cache, s.hubClient)

	// Set up host auth middleware if enabled and we have credentials
	if s.config.HostAuthEnabled && len(secretKey) > 0 {
		s.hostAuthMiddleware = NewHostAuthMiddleware(HostAuthConfig{
			Enabled:              true,
			MaxClockSkew:         5 * time.Minute,
			SecretKey:            secretKey,
			AllowUnauthenticated: !s.config.HostAuthStrictMode, // Configurable strict mode
		})
		if s.config.HostAuthStrictMode {
			log.Printf("Host auth middleware enabled (strict mode - all requests must be authenticated)")
		} else {
			log.Printf("Host auth middleware enabled (permissive mode - unauthenticated requests allowed)")
		}
	}

	log.Printf("Hub integration initialized (endpoint: %s, cache: %s, max: %d MB)",
		s.config.HubEndpoint, cacheDir, maxSize/(1024*1024))

	return nil
}

// loadHostCredentials attempts to load host credentials from the configured path.
func (s *Server) loadHostCredentials() error {
	credPath := s.config.HostCredentialsPath
	if credPath == "" {
		credPath = hostcredentials.DefaultPath()
	}

	store := hostcredentials.NewStore(credPath)
	if !store.Exists() {
		return nil // No credentials file, not an error
	}

	creds, err := store.Load()
	if err != nil {
		return fmt.Errorf("failed to load host credentials: %w", err)
	}

	s.hostCredentials = creds
	log.Printf("Host credentials loaded (hostID: %s, hub: %s)", creds.HostID, creds.HubEndpoint)
	return nil
}

// SetHubClient sets the Hub client for template hydration.
// This is useful for testing or when the client is configured externally.
func (s *Server) SetHubClient(client hubclient.Client) {
	s.hubClient = client
	if s.cache != nil {
		s.hydrator = templatecache.NewHydrator(s.cache, client)
	}
}

// SetTemplateCache sets the template cache.
// This is useful for testing or when the cache is configured externally.
func (s *Server) SetTemplateCache(cache *templatecache.Cache) {
	s.cache = cache
	if s.hubClient != nil {
		s.hydrator = templatecache.NewHydrator(cache, s.hubClient)
	}
}

// GetHydrator returns the template hydrator, if configured.
func (s *Server) GetHydrator() *templatecache.Hydrator {
	return s.hydrator
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

	log.Printf("Runtime Host API server starting on %s:%d (mode: %s)", s.config.Host, s.config.Port, s.config.Mode)

	// Start heartbeat service if enabled and we have a Hub client
	if s.config.HeartbeatEnabled && s.hubClient != nil && s.config.HostID != "" {
		interval := s.config.HeartbeatInterval
		if interval <= 0 {
			interval = DefaultHeartbeatInterval
		}

		s.heartbeat = NewHeartbeatService(
			s.hubClient.RuntimeHosts(),
			s.config.HostID,
			interval,
			s.manager,
		)
		s.heartbeat.SetVersion(s.version)
		s.heartbeat.Start(ctx)
		log.Printf("Heartbeat service started (interval: %s)", interval)
	}

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
	hb := s.heartbeat
	s.mu.RUnlock()

	// Stop heartbeat service first
	if hb != nil {
		log.Println("Stopping heartbeat service...")
		hb.Stop()
	}

	if srv == nil {
		return nil
	}

	log.Println("Runtime Host API server shutting down...")

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
	// Health endpoints
	s.mux.HandleFunc("/healthz", s.handleHealthz)
	s.mux.HandleFunc("/readyz", s.handleReadyz)

	// API v1 routes
	s.mux.HandleFunc("/api/v1/info", s.handleInfo)

	// Agent routes
	s.mux.HandleFunc("/api/v1/agents", s.handleAgents)
	s.mux.HandleFunc("/api/v1/agents/", s.handleAgentByID)
}

// applyMiddleware wraps the handler with middleware.
func (s *Server) applyMiddleware(h http.Handler) http.Handler {
	// Apply middleware in reverse order (last applied runs first)
	h = s.recoveryMiddleware(h)
	h = s.loggingMiddleware(h)
	if s.config.CORSEnabled {
		h = s.corsMiddleware(h)
	}
	// Apply host auth middleware if configured
	if s.hostAuthMiddleware != nil {
		h = s.hostAuthMiddleware.Middleware(h)
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
			log.Printf("[RuntimeHost] --> %s %s (from %s)", r.Method, r.URL.Path, r.RemoteAddr)
			if r.URL.RawQuery != "" {
				log.Printf("[RuntimeHost]     query: %s", r.URL.RawQuery)
			}
			for name, values := range r.Header {
				if name == "Authorization" {
					log.Printf("[RuntimeHost]     header: %s: [REDACTED]", name)
				} else {
					log.Printf("[RuntimeHost]     header: %s: %s", name, strings.Join(values, ", "))
				}
			}
		}

		next.ServeHTTP(wrapped, r)

		if s.config.Debug {
			log.Printf("[RuntimeHost] <-- %s %s %d (%s)",
				r.Method, r.URL.Path, wrapped.statusCode, time.Since(start))
		} else {
			log.Printf("[RuntimeHost] %s %s %d %s",
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
