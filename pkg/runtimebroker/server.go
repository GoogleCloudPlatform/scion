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

// Package runtimebroker provides the Scion Runtime Broker API server.
// The Runtime Broker API exposes agent lifecycle management over HTTP,
// allowing the Scion Hub to remotely manage agents on this compute node.
package runtimebroker

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/ptone/scion-agent/pkg/agent"
	"github.com/ptone/scion-agent/pkg/brokercredentials"
	"github.com/ptone/scion-agent/pkg/hubclient"
	"github.com/ptone/scion-agent/pkg/runtime"
	"github.com/ptone/scion-agent/pkg/templatecache"
	"github.com/ptone/scion-agent/pkg/util/logging"
)

// ServerConfig holds configuration for the Runtime Broker API server.
type ServerConfig struct {
	// Port is the HTTP port to listen on.
	Port int
	// Host is the address to bind to (e.g., "0.0.0.0" or "127.0.0.1").
	Host string
	// ReadTimeout is the maximum duration for reading the entire request.
	ReadTimeout time.Duration
	// WriteTimeout is the maximum duration before timing out writes.
	WriteTimeout time.Duration

	// HubEndpoint is the Hub API endpoint for reporting (optional).
	HubEndpoint string

	// BrokerID is a unique identifier for this runtime broker.
	BrokerID string
	// BrokerName is a human-readable name for this runtime broker.
	BrokerName string

	// CORS settings
	CORSEnabled        bool
	CORSAllowedOrigins []string
	CORSAllowedMethods []string
	CORSAllowedHeaders []string
	CORSMaxAge         int

	// Debug enables verbose debug logging.
	Debug bool

	// Hub integration settings
	// HubEnabled indicates whether this Runtime Broker should connect to a Hub
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

	// Broker credentials settings
	// BrokerCredentialsPath is the path to the broker credentials file.
	// If set, HMAC authentication will be used instead of bearer tokens.
	// Defaults to ~/.scion/broker-credentials.json if not specified.
	BrokerCredentialsPath string

	// InMemoryCredentials allows injecting credentials directly without a file.
	// Used for co-located Hub+RuntimeBroker mode where credentials are generated
	// in-memory and shared between the Hub and RuntimeBroker in the same process.
	// Takes precedence over BrokerCredentialsPath if set.
	InMemoryCredentials *brokercredentials.BrokerCredentials

	// BrokerAuthEnabled enables HMAC verification for incoming requests from the Hub.
	BrokerAuthEnabled bool
	// BrokerAuthStrictMode, when true, requires all requests to be authenticated.
	// When false (default), unauthenticated requests are allowed for transition periods.
	BrokerAuthStrictMode bool

	// Heartbeat settings
	// HeartbeatEnabled enables periodic heartbeats to the Hub.
	HeartbeatEnabled bool
	// HeartbeatInterval is the time between heartbeats.
	// Defaults to 30 seconds if not specified.
	HeartbeatInterval time.Duration

	// Control channel settings
	// ControlChannelEnabled enables the WebSocket control channel to the Hub.
	// This allows NAT traversal for brokers behind firewalls.
	ControlChannelEnabled bool

	// Workspace sync settings
	// StorageBucket is the GCS bucket name for workspace storage.
	// Used when workspace sync requests don't specify a bucket.
	StorageBucket string
	// WorktreeBase is the base directory for agent worktrees.
	// Used as a fallback when resolving workspace paths.
	WorktreeBase string
}

// DefaultServerConfig returns the default server configuration.
func DefaultServerConfig() ServerConfig {
	return ServerConfig{
		Port:               9800,
		Host:               "0.0.0.0",
		ReadTimeout:        30 * time.Second,
		WriteTimeout:       120 * time.Second,
		CORSEnabled:        true,
		CORSAllowedOrigins: []string{"*"},
		CORSAllowedMethods: []string{"GET", "POST", "PUT", "PATCH", "DELETE", "OPTIONS"},
		CORSAllowedHeaders: []string{"Authorization", "Content-Type", "X-Scion-Broker-Token", "X-API-Key", "X-Scion-Broker-ID", "X-Scion-Timestamp", "X-Scion-Nonce", "X-Scion-Signature", "X-Scion-Signed-Headers"},
		CORSMaxAge:         3600,
	}
}

// Server is the Runtime Broker API HTTP server.
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
	brokerAuthMiddleware *BrokerAuthMiddleware
	heartbeat          *HeartbeatService
	brokerCredentials    *brokercredentials.BrokerCredentials

	// Credential watching
	credentialsStore   *brokercredentials.Store
	credentialsModTime time.Time
	credWatcherStop    chan struct{}

	// Control channel
	controlChannel *ControlChannelClient
}

// New creates a new Runtime Broker API server.
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
			slog.Warn("Failed to initialize Hub integration", "error", err)
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

	// Try to load broker credentials for HMAC auth
	var secretKey []byte
	if err := s.loadBrokerCredentials(); err == nil && s.brokerCredentials != nil {
		// Decode the secret key
		secretKey, err = base64.StdEncoding.DecodeString(s.brokerCredentials.SecretKey)
		if err != nil {
			slog.Warn("Failed to decode broker secret key", "error", err)
		}
	}

	// Initialize Hub client with appropriate auth
	opts := []hubclient.Option{}

	if len(secretKey) > 0 && s.brokerCredentials != nil {
		// Use HMAC auth from credentials
		opts = append(opts, hubclient.WithHMACAuth(s.brokerCredentials.BrokerID, secretKey))
		slog.Info("Hub client using HMAC authentication", "brokerID", s.brokerCredentials.BrokerID)

		// Update BrokerID from credentials if not already set
		if s.config.BrokerID == "" {
			s.config.BrokerID = s.brokerCredentials.BrokerID
		}
	} else if s.config.HubToken != "" {
		// Fall back to bearer token
		opts = append(opts, hubclient.WithBearerToken(s.config.HubToken))
		slog.Info("Hub client using bearer token authentication")
	} else {
		// Try auto dev auth
		opts = append(opts, hubclient.WithAutoDevAuth())
		slog.Info("Hub client using auto dev authentication")
	}

	client, err := hubclient.New(s.config.HubEndpoint, opts...)
	if err != nil {
		return fmt.Errorf("failed to create Hub client: %w", err)
	}
	s.hubClient = client

	// Initialize hydrator
	s.hydrator = templatecache.NewHydrator(s.cache, s.hubClient)

	// Set up broker auth middleware if enabled and we have credentials
	if s.config.BrokerAuthEnabled && len(secretKey) > 0 {
		s.brokerAuthMiddleware = NewBrokerAuthMiddleware(BrokerAuthConfig{
			Enabled:              true,
			MaxClockSkew:         5 * time.Minute,
			SecretKey:            secretKey,
			AllowUnauthenticated: !s.config.BrokerAuthStrictMode, // Configurable strict mode
		})
		if s.config.BrokerAuthStrictMode {
			slog.Info("Broker auth middleware enabled (strict mode)")
		} else {
			slog.Info("Broker auth middleware enabled (permissive mode)")
		}
	}

	slog.Info("Hub integration initialized",
		"endpoint", s.config.HubEndpoint,
		"cache", cacheDir,
		"max_size_mb", maxSize/(1024*1024),
	)

	return nil
}

// loadBrokerCredentials attempts to load broker credentials.
// Priority: InMemoryCredentials (co-located mode) > BrokerCredentialsPath > default path
func (s *Server) loadBrokerCredentials() error {
	// Check for in-memory credentials first (used in co-located Hub+RuntimeBroker mode)
	if s.config.InMemoryCredentials != nil {
		s.brokerCredentials = s.config.InMemoryCredentials
		slog.Info("Using in-memory broker credentials (co-located mode)", "brokerID", s.brokerCredentials.BrokerID)
		return nil
	}

	// Fall back to file-based credentials
	credPath := s.config.BrokerCredentialsPath
	if credPath == "" {
		credPath = brokercredentials.DefaultPath()
	}

	s.credentialsStore = brokercredentials.NewStore(credPath)
	if !s.credentialsStore.Exists() {
		return nil // No credentials file, not an error
	}

	creds, err := s.credentialsStore.Load()
	if err != nil {
		return fmt.Errorf("failed to load broker credentials: %w", err)
	}

	s.brokerCredentials = creds
	s.credentialsModTime = s.credentialsStore.ModTime()
	slog.Info("Broker credentials loaded", "brokerID", creds.BrokerID, "hub", creds.HubEndpoint)
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

	slog.Info("Runtime Broker API server starting",
		"host", s.config.Host,
		"port", s.config.Port,
	)
	if s.config.Debug {
		slog.Debug("Broker details",
			"brokerID", s.config.BrokerID,
			"brokerName", s.config.BrokerName,
			"hub_endpoint", s.config.HubEndpoint,
		)
	}

	// Check if we have valid broker credentials for Hub communication
	hasValidCredentials := s.brokerCredentials != nil && s.brokerCredentials.SecretKey != ""

	// Start heartbeat service if enabled and we have valid credentials
	if s.config.HeartbeatEnabled && s.hubClient != nil && s.config.BrokerID != "" {
		if !hasValidCredentials {
			slog.Warn("Skipping heartbeat: no valid broker credentials (run 'scion hub link' first)")
		} else {
			interval := s.config.HeartbeatInterval
			if interval <= 0 {
				interval = DefaultHeartbeatInterval
			}

			s.heartbeat = NewHeartbeatService(
				s.hubClient.RuntimeBrokers(),
				s.config.BrokerID,
				interval,
				s.manager,
			)
			s.heartbeat.SetVersion(s.version)
			s.heartbeat.Start(ctx)
			slog.Info("Heartbeat service started", "interval", interval)
		}
	}

	// Start control channel if enabled and we have valid credentials
	if s.config.ControlChannelEnabled && s.config.HubEndpoint != "" && s.config.BrokerID != "" {
		if !hasValidCredentials {
			slog.Warn("Skipping control channel: no valid broker credentials (run 'scion hub link' first)")
		} else {
			secretKey, err := base64.StdEncoding.DecodeString(s.brokerCredentials.SecretKey)
			if err != nil {
				slog.Error("Failed to decode secret key", "error", err)
			} else {
				ccConfig := ControlChannelConfig{
					HubEndpoint:         s.config.HubEndpoint,
					BrokerID:              s.config.BrokerID,
					SecretKey:           secretKey,
					Version:             s.version,
					ReconnectInitial:    1 * time.Second,
					ReconnectMax:        60 * time.Second,
					ReconnectMultiplier: 2.0,
					PingInterval:        30 * time.Second,
					PongWait:            60 * time.Second,
					WriteWait:           10 * time.Second,
					Debug:               s.config.Debug,
				}

				s.controlChannel = NewControlChannelClient(ccConfig, s.Handler(), s)
				go func() {
					if err := s.controlChannel.Connect(ctx); err != nil {
						slog.Error("Control channel error", "error", err)
					}
				}()
				slog.Info("Connecting to Hub control channel", "endpoint", s.config.HubEndpoint)
			}
		}
	}

	// Start credential watcher for dynamic reload
	if s.config.HubEnabled && s.credentialsStore != nil {
		s.startCredentialWatcher(ctx)
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
	cc := s.controlChannel
	credWatcherStop := s.credWatcherStop
	s.mu.RUnlock()

	// Stop credential watcher
	if credWatcherStop != nil {
		slog.Info("Stopping credential watcher...")
		close(credWatcherStop)
	}

	// Stop control channel first
	if cc != nil {
		slog.Info("Stopping control channel...")
		cc.Close()
	}

	// Stop heartbeat service
	if hb != nil {
		slog.Info("Stopping heartbeat service...")
		hb.Stop()
	}

	if srv == nil {
		return nil
	}

	slog.Info("Runtime Broker API server shutting down...")

	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	return srv.Shutdown(ctx)
}

// Handler returns the HTTP handler for the server.
// This is useful for testing without starting a listener.
func (s *Server) Handler() http.Handler {
	return s.applyMiddleware(s.mux)
}

// LookupContainerID implements AgentLookup interface.
// It looks up an agent by slug and returns its container ID.
func (s *Server) LookupContainerID(ctx context.Context, slug string) (string, error) {
	if s.manager == nil {
		return "", fmt.Errorf("agent manager not available")
	}

	// Look up agent using List with filter by name
	agents, err := s.manager.List(ctx, map[string]string{"scion.name": slug})
	if err != nil {
		return "", fmt.Errorf("failed to list agents: %w", err)
	}

	if len(agents) == 0 {
		return "", fmt.Errorf("agent '%s' not found", slug)
	}

	agent := agents[0]

	// Check if agent supports attach (has tmux)
	if agent.Labels == nil || agent.Labels["scion.tmux"] != "true" {
		return "", fmt.Errorf("agent '%s' does not support attach", slug)
	}

	// Get container ID - prefer label, then ContainerID from runtime, then ID
	containerID := agent.Labels["scion.container.id"]
	if containerID == "" {
		containerID = agent.ContainerID
	}
	if containerID == "" {
		containerID = agent.ID
	}
	if containerID == "" {
		return "", fmt.Errorf("agent '%s' has no container ID", slug)
	}

	return containerID, nil
}

// RuntimeCommand implements AgentLookup interface.
// It returns the container runtime command (e.g., "docker", "container").
func (s *Server) RuntimeCommand() string {
	if s.runtime == nil {
		return "docker" // Default fallback
	}
	return s.runtime.Name()
}

// startCredentialWatcher starts a goroutine that watches for credential file changes.
// When credentials change, it reinitializes the Hub client and restarts services.
func (s *Server) startCredentialWatcher(ctx context.Context) {
	if s.credentialsStore == nil {
		slog.Warn("No credentials store configured, skipping watcher")
		return
	}

	s.credWatcherStop = make(chan struct{})
	go s.credentialWatchLoop(ctx)
	slog.Info("Credential watcher started", "interval", "10s")
}

// credentialWatchLoop is the main credential watching loop.
func (s *Server) credentialWatchLoop(ctx context.Context) {
	ticker := time.NewTicker(10 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-s.credWatcherStop:
			return
		case <-ticker.C:
			if err := s.checkAndReloadCredentials(ctx); err != nil {
				slog.Error("Error checking credentials", "error", err)
			}
		}
	}
}

// checkAndReloadCredentials checks if credentials have changed and reloads if necessary.
func (s *Server) checkAndReloadCredentials(ctx context.Context) error {
	if s.credentialsStore == nil {
		return nil
	}

	creds, modTime, err := s.credentialsStore.LoadIfChanged(s.credentialsModTime)
	if err != nil {
		return fmt.Errorf("failed to check credentials: %w", err)
	}

	if creds == nil {
		return nil // No change
	}

	slog.Info("Credentials changed, reloading", "brokerID", creds.BrokerID)

	s.mu.Lock()
	oldCredentials := s.brokerCredentials
	s.brokerCredentials = creds
	s.credentialsModTime = modTime
	s.mu.Unlock()

	// Check if broker ID or secret key changed (requiring service restart)
	brokerIDChanged := oldCredentials == nil || oldCredentials.BrokerID != creds.BrokerID
	secretKeyChanged := oldCredentials == nil || oldCredentials.SecretKey != creds.SecretKey

	if brokerIDChanged || secretKeyChanged {
		if err := s.reinitializeHubServices(ctx, creds); err != nil {
			slog.Error("Failed to reinitialize services", "error", err)
			return err
		}
		slog.Info("Services reinitialized with new credentials")
	}

	return nil
}

// reinitializeHubServices stops and restarts the Hub client, heartbeat, and control channel.
func (s *Server) reinitializeHubServices(ctx context.Context, creds *brokercredentials.BrokerCredentials) error {
	// Decode the secret key
	secretKey, err := base64.StdEncoding.DecodeString(creds.SecretKey)
	if err != nil {
		return fmt.Errorf("failed to decode secret key: %w", err)
	}

	// Stop existing services
	s.mu.Lock()
	if s.controlChannel != nil {
		slog.Info("Stopping control channel for reload")
		s.controlChannel.Close()
		s.controlChannel = nil
	}
	if s.heartbeat != nil {
		slog.Info("Stopping heartbeat for reload")
		s.heartbeat.Stop()
		s.heartbeat = nil
	}
	s.mu.Unlock()

	// Create new Hub client with updated credentials
	opts := []hubclient.Option{
		hubclient.WithHMACAuth(creds.BrokerID, secretKey),
	}
	client, err := hubclient.New(s.config.HubEndpoint, opts...)
	if err != nil {
		return fmt.Errorf("failed to create Hub client: %w", err)
	}

	s.mu.Lock()
	s.hubClient = client
	s.config.BrokerID = creds.BrokerID // Update BrokerID in config
	if s.cache != nil {
		s.hydrator = templatecache.NewHydrator(s.cache, client)
	}
	slog.Info("Hub client reinitialized with HMAC auth", "brokerID", creds.BrokerID)
	s.mu.Unlock()

	// Restart heartbeat if enabled
	if s.config.HeartbeatEnabled && s.config.BrokerID != "" {
		interval := s.config.HeartbeatInterval
		if interval <= 0 {
			interval = DefaultHeartbeatInterval
		}

		s.mu.Lock()
		s.heartbeat = NewHeartbeatService(
			client.RuntimeBrokers(),
			creds.BrokerID,
			interval,
			s.manager,
		)
		s.heartbeat.SetVersion(s.version)
		s.heartbeat.Start(ctx)
		s.mu.Unlock()
		slog.Info("Heartbeat restarted", "interval", interval)
	}

	// Restart control channel if enabled
	if s.config.ControlChannelEnabled && s.config.HubEndpoint != "" {
		ccConfig := ControlChannelConfig{
			HubEndpoint:         s.config.HubEndpoint,
			BrokerID:              creds.BrokerID,
			SecretKey:           secretKey,
			Version:             s.version,
			ReconnectInitial:    1 * time.Second,
			ReconnectMax:        60 * time.Second,
			ReconnectMultiplier: 2.0,
			PingInterval:        30 * time.Second,
			PongWait:            60 * time.Second,
			WriteWait:           10 * time.Second,
			Debug:               s.config.Debug,
		}

		s.mu.Lock()
		s.controlChannel = NewControlChannelClient(ccConfig, s.Handler(), s)
		s.mu.Unlock()

		go func() {
			if err := s.controlChannel.Connect(ctx); err != nil {
				slog.Error("Control channel error after reload", "error", err)
			}
		}()
		slog.Info("Control channel restarted")
	}

	return nil
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

	// Workspace sync routes (for Hub-initiated sync via control channel)
	s.mux.HandleFunc("/api/v1/workspace/upload", s.handleWorkspaceUpload)
	s.mux.HandleFunc("/api/v1/workspace/apply", s.handleWorkspaceApply)
}

// applyMiddleware wraps the handler with middleware.
func (s *Server) applyMiddleware(h http.Handler) http.Handler {
	// Apply middleware in reverse order (last applied runs first)
	h = s.recoveryMiddleware(h)
	h = s.loggingMiddleware(h)
	if s.config.CORSEnabled {
		h = s.corsMiddleware(h)
	}
	// Apply broker auth middleware if configured
	if s.brokerAuthMiddleware != nil {
		h = s.brokerAuthMiddleware.Middleware(h)
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
