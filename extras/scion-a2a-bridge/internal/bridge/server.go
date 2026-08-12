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

package bridge

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2asrv"
	"github.com/prometheus/client_golang/prometheus"

	"github.com/GoogleCloudPlatform/scion/pkg/util/logging"
)

var slugRE = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,62}$`)

// BridgePathPatterns returns URL patterns for the A2A bridge API.
// These patterns are used by RequestLogMiddleware to extract project and agent
// IDs from the request path for structured log enrichment.
func BridgePathPatterns() []logging.PathPattern {
	return []logging.PathPattern{
		{Prefix: "/projects/", ProjectIdx: 0, AgentIdx: 2}, // /projects/{slug}/agents/{slug}/...
		{Prefix: "/groves/", ProjectIdx: 0, AgentIdx: 2},   // /groves/{slug}/agents/{slug}/...
	}
}

// Server is the A2A HTTP server that routes requests to the SDK handler.
type Server struct {
	bridge     *Bridge
	config     *Config         // base config (kept for backward compat in non-snapshot paths)
	snapshot   *SnapshotHolder // atomic snapshot of effective config (hot-apply)
	metrics    *Metrics
	log        *slog.Logger
	sdkHandler http.Handler // SDK JSON-RPC handler
	// Legacy validators — used only when snapshot is nil (tests, backward compat).
	uatValidator *UATValidator
	jwtValidator *JWTValidator
}

// NewServer creates a new A2A protocol server backed by the SDK.
func NewServer(bridge *Bridge, cfg *Config, metrics *Metrics, log *slog.Logger, sdkHandler http.Handler) *Server {
	s := &Server{
		bridge:     bridge,
		config:     cfg,
		metrics:    metrics,
		log:        log,
		sdkHandler: sdkHandler,
	}
	// Initialize per-user auth validators based on the configured scheme.
	switch cfg.Auth.Scheme {
	case "hubUAT":
		s.uatValidator = NewUATValidator(cfg.Hub.Endpoint, cfg.Auth.UATCacheTTL)
	case "hubJWT":
		// JWTValidator is initialized later via SetJWTValidator once the
		// signing key is loaded (it may come from Secret Manager).
	}
	return s
}

// SetSnapshot wires the atomic config snapshot for hot-apply support.
// When set, auth middleware and rate limiting read from the snapshot
// instead of the static config pointer.
func (s *Server) SetSnapshot(snap *SnapshotHolder) {
	s.snapshot = snap
}

// SetJWTValidator sets the JWT validator for hubJWT mode. Called after the
// signing key is loaded (which may require Secret Manager access).
// Also updates the snapshot if one is wired.
//
// NOTE: not safe for concurrent use with Configure(). Currently only called
// once during boot before the HTTP server starts.
func (s *Server) SetJWTValidator(v *JWTValidator) {
	s.jwtValidator = v
	if s.snapshot != nil {
		snap := s.snapshot.Load()
		if snap != nil && snap.Auth.Scheme == "hubJWT" {
			// Create a new snapshot with the validator set.
			newSnap := *snap
			newSnap.Auth.JWTValidator = v
			s.snapshot.Store(&newSnap)
		}
	}
}

// ValidateConfig checks that required configuration fields are present and consistent.
func ValidateConfig(cfg *Config) error {
	if cfg.Bridge.ExternalURL == "" {
		return fmt.Errorf("bridge.external_url is required")
	}
	for _, g := range cfg.Projects {
		if strings.Contains(g.Slug, ":") {
			return fmt.Errorf("project slug %q must not contain ':'", g.Slug)
		}
		for _, a := range g.ExposedAgents {
			if strings.Contains(a, ":") {
				return fmt.Errorf("agent slug %q must not contain ':'", a)
			}
		}
	}
	if cfg.Hub.Endpoint == "" {
		return fmt.Errorf("hub.endpoint is required")
	}
	if cfg.Hub.User == "" {
		return fmt.Errorf("hub.user is required")
	}
	switch cfg.Auth.Scheme {
	case "", "apiKey", "bearer", "none", "hubUAT", "hubJWT", "federation":
		// valid
	default:
		return fmt.Errorf("unsupported auth.scheme: %q (supported: apiKey, bearer, none, hubUAT, hubJWT, federation)", cfg.Auth.Scheme)
	}
	if (cfg.Auth.Scheme == "apiKey" || cfg.Auth.Scheme == "bearer") && cfg.Auth.APIKey == "" {
		return fmt.Errorf("auth.api_key is required when auth.scheme is %q", cfg.Auth.Scheme)
	}
	// api_key is required for legacy schemes and the default (empty) scheme.
	// hubUAT, hubJWT, and federation do not use api_key — they validate per-user/agent credentials instead.
	if cfg.Auth.APIKey == "" && cfg.Auth.Scheme != "none" && cfg.Auth.Scheme != "hubUAT" && cfg.Auth.Scheme != "hubJWT" && cfg.Auth.Scheme != "federation" {
		return fmt.Errorf("auth.api_key is required (set auth.scheme: \"none\" to explicitly disable authentication)")
	}
	if cfg.Auth.Scheme == "hubJWT" && cfg.Hub.SigningKey == "" && cfg.Hub.SigningKeySecret == "" {
		return fmt.Errorf("hub.signing_key or hub.signing_key_secret is required when auth.scheme is hubJWT")
	}
	if cfg.Auth.UATCacheTTL < 0 {
		return fmt.Errorf("auth.uat_cache_ttl must not be negative")
	}
	if cfg.Auth.UATCacheTTL > 300*time.Second {
		return fmt.Errorf("auth.uat_cache_ttl must not exceed 300s")
	}
	if cfg.Bridge.Provider.URL != "" {
		if _, err := url.Parse(cfg.Bridge.Provider.URL); err != nil {
			return fmt.Errorf("bridge.provider.url is invalid: %w", err)
		}
	}
	return nil
}

// WarnOnOpenAuth logs a warning if the auth configuration leaves the bridge open.
func (s *Server) WarnOnOpenAuth() {
	cfg := s.effectiveConfig()
	switch cfg.Auth.Scheme {
	case "none":
		s.log.Warn("bridge auth is explicitly DISABLED (auth.scheme: none) — all requests will be accepted without authentication")
	case "":
		s.log.Warn("auth.scheme is empty: bridge will accept credentials from both X-API-Key and Authorization headers")
	case "hubUAT":
		s.log.Info("bridge auth: hubUAT — per-user Scion UAT authentication enabled")
	case "hubJWT":
		s.log.Info("bridge auth: hubJWT — per-user Scion JWT authentication enabled")
	case "federation":
		s.log.Info("bridge auth: federation — pass-through OIDC federation authentication enabled (hub validates tokens)")
	}
	if cfg.RateLimit.TrustProxy {
		s.log.Warn("rate_limit.trust_proxy is enabled — X-Forwarded-For is trusted unconditionally, which allows clients to spoof their IP and bypass per-IP rate limits; consider adding network-level proxy restrictions")
	}
}

// Handler returns an http.Handler for the A2A server routes.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()

	// Top-level well-known agent card (registry).
	mux.HandleFunc("GET /.well-known/agent-card.json", s.handleWellKnownAgentCard)

	// OIDC discovery proxy — publicly exposes the hub's IAP-protected OIDC endpoints.
	mux.HandleFunc("GET /.well-known/openid-configuration", s.handleOIDCDiscoveryProxy)
	mux.HandleFunc("GET /.well-known/jwks.json", s.handleJWKSProxy)

	// Per-agent routes — the SDK handler handles JSON-RPC protocol.
	mux.HandleFunc("GET /projects/{projectSlug}/agents/{agentSlug}/.well-known/agent-card.json", s.handleAgentCard)
	mux.HandleFunc("POST /projects/{projectSlug}/agents/{agentSlug}/jsonrpc", s.handleJSONRPC)

	// Legacy per-agent routes (backward compatibility for "grove" naming).
	mux.HandleFunc("GET /groves/{projectSlug}/agents/{agentSlug}/.well-known/agent-card.json", s.handleAgentCard)
	mux.HandleFunc("POST /groves/{projectSlug}/agents/{agentSlug}/jsonrpc", s.handleJSONRPC)

	// Health, readiness, and metrics.
	mux.HandleFunc("GET /healthz", s.handleHealthz)
	mux.HandleFunc("GET /readyz", s.handleReadyz)
	mux.Handle("GET /metrics", MetricsHandler(prometheus.DefaultGatherer))

	// Wrap with middleware chain: request-log -> metrics -> rate limit -> auth.
	handler := s.authMiddleware(mux)
	if s.snapshot != nil {
		handler = s.snapshotRateLimitMiddleware(handler)
	} else {
		handler = RateLimitMiddleware(handler, s.config.RateLimit)
	}
	handler = InstrumentHandler(handler, s.metrics)
	handler = logging.RequestLogMiddleware(
		s.log, "scion-a2a-bridge", BridgePathPatterns(), 0,
	)(handler)

	// Internal endpoints — behind Hub-JWT auth with service claim pin,
	// NOT the external authMiddleware (which enforces the A2A auth_scheme
	// and would leave the endpoint open when scheme is "none").
	internalMux := http.NewServeMux()
	internalMux.HandleFunc("POST /internal/sweep", s.handleInternalSweep)

	// Combine: internal routes (with their own auth) + external routes.
	combinedMux := http.NewServeMux()
	combinedMux.Handle("/internal/", s.hubJWTAuthMiddleware(internalMux))
	combinedMux.Handle("/", handler) // handler is the existing middleware chain

	return combinedMux
}

// SDKRequestHandler returns the a2asrv.RequestHandler for use with other transports (gRPC, REST).
// Returns nil if the server was created without an SDK handler.
func (s *Server) SDKRequestHandler() a2asrv.RequestHandler {
	// The SDK handler is stored as http.Handler but we also need the RequestHandler
	// for gRPC/REST transports. This is set via SetSDKRequestHandler.
	return s.bridge.sdkRequestHandler
}

func (s *Server) handleHealthz(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(map[string]string{"status": "ok"}); err != nil {
		s.log.Error("failed to encode healthz response", "error", err)
	}
}

func (s *Server) handleReadyz(w http.ResponseWriter, r *http.Request) {
	checks := map[string]string{}
	ready := true

	if err := s.bridge.store.Ping(r.Context()); err != nil {
		s.log.Error("readiness check: database ping failed", "error", err)
		checks["database"] = "error"
		ready = false
	} else {
		checks["database"] = "ok"
	}

	if s.bridge.broker != nil {
		checks["broker"] = "connected"
	} else {
		checks["broker"] = "not configured"
	}

	checks["status"] = "ready"
	if !ready {
		checks["status"] = "not ready"
		w.WriteHeader(http.StatusServiceUnavailable)
	}

	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(checks); err != nil {
		s.log.Error("failed to encode readyz response", "error", err)
	}
}

func (s *Server) handleWellKnownAgentCard(w http.ResponseWriter, r *http.Request) {
	cfg := s.effectiveConfig()
	registry := map[string]interface{}{
		"name":        "scion-a2a-bridge",
		"description": "Scion A2A Protocol Bridge — exposes Scion agents as A2A endpoints",
		"url":         cfg.Bridge.ExternalURL,
		"version":     "1.0.0",
		"capabilities": map[string]bool{
			"streaming":         true,
			"pushNotifications": false,
		},
	}

	if cfg.Bridge.Provider.Organization != "" {
		registry["provider"] = map[string]string{
			"organization": cfg.Bridge.Provider.Organization,
			"url":          cfg.Bridge.Provider.URL,
		}
	}

	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "public, max-age=300")
	if err := json.NewEncoder(w).Encode(registry); err != nil {
		s.log.Error("failed to encode well-known agent card response", "error", err)
	}
}

func (s *Server) handleAgentCard(w http.ResponseWriter, r *http.Request) {
	projectSlug := r.PathValue("projectSlug")
	agentSlug := r.PathValue("agentSlug")

	if !slugRE.MatchString(projectSlug) || !slugRE.MatchString(agentSlug) {
		http.Error(w, "invalid slug", http.StatusBadRequest)
		return
	}

	projectCfg := s.bridge.GetProjectConfig(projectSlug)
	if projectCfg == nil {
		http.Error(w, "project not found", http.StatusNotFound)
		return
	}

	if len(projectCfg.ExposedAgents) > 0 {
		found := false
		for _, a := range projectCfg.ExposedAgents {
			if a == agentSlug {
				found = true
				break
			}
		}
		if !found {
			http.Error(w, "agent not exposed", http.StatusNotFound)
			return
		}
	}

	card := s.bridge.GenerateAgentCard(r.Context(), projectSlug, agentSlug)

	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "public, max-age=300")
	if err := json.NewEncoder(w).Encode(card); err != nil {
		s.log.Error("failed to encode agent card response", "error", err)
	}
}

// handleJSONRPC validates the project/agent routing and delegates to the SDK handler.
func (s *Server) handleJSONRPC(w http.ResponseWriter, r *http.Request) {
	projectSlug := r.PathValue("projectSlug")
	agentSlug := r.PathValue("agentSlug")

	if !slugRE.MatchString(projectSlug) || !slugRE.MatchString(agentSlug) {
		writeJSONRPCError(w, nil, -32602, "invalid slug format")
		return
	}

	if err := s.bridge.AuthorizeExposed(projectSlug, agentSlug); err != nil {
		writeJSONRPCError(w, nil, -32602, "agent not found")
		return
	}

	// Opportunistic sweep: fire at most once per interval per instance.
	s.bridge.maybeOpportunisticSweep(r.Context())

	// Enforce request body size limit to prevent memory exhaustion.
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20) // 1 MB

	// Inject routing info into context for the executor.
	ctx := WithRouteInfo(r.Context(), RouteInfo{
		ProjectSlug: projectSlug,
		AgentSlug:   agentSlug,
	})
	r = r.WithContext(ctx)

	// Delegate to SDK JSON-RPC handler.
	s.sdkHandler.ServeHTTP(w, r)
}

// writeJSONRPCError writes a minimal JSON-RPC error response.
func writeJSONRPCError(w http.ResponseWriter, id interface{}, code int, message string) {
	type jsonrpcError struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	}
	type jsonrpcResponse struct {
		JSONRPC string        `json:"jsonrpc"`
		ID      interface{}   `json:"id"`
		Error   *jsonrpcError `json:"error,omitempty"`
	}
	resp := jsonrpcResponse{
		JSONRPC: "2.0",
		ID:      id,
		Error:   &jsonrpcError{Code: code, Message: message},
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(resp)
}

// authMiddleware validates authentication on non-public endpoints.
// When a snapshot is available, auth scheme and validators are read from the
// snapshot for hot-apply support. Each request loads the snapshot once for
// consistency even if a config swap happens mid-flight.
func (s *Server) authMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Public/operational endpoints skip auth.
		if r.URL.Path == "/.well-known/agent-card.json" ||
			r.URL.Path == "/.well-known/openid-configuration" ||
			r.URL.Path == "/.well-known/jwks.json" ||
			r.URL.Path == "/healthz" || r.URL.Path == "/readyz" || r.URL.Path == "/metrics" {
			next.ServeHTTP(w, r)
			return
		}
		// Per-agent card: exactly /projects/{slug}/agents/{slug}/.well-known/agent-card.json
		// or legacy /groves/{slug}/agents/{slug}/.well-known/agent-card.json
		segments := strings.Split(strings.TrimPrefix(r.URL.Path, "/"), "/")
		if len(segments) == 6 && (segments[0] == "projects" || segments[0] == "groves") && segments[2] == "agents" && segments[4] == ".well-known" && segments[5] == "agent-card.json" {
			next.ServeHTTP(w, r)
			return
		}

		// Resolve auth scheme and validators from snapshot (hot-apply) or static config.
		var scheme string
		var uatV *UATValidator
		var jwtV *JWTValidator
		var configAPIKey string

		if s.snapshot != nil {
			snap := s.snapshot.Load()
			scheme = snap.Auth.Scheme
			uatV = snap.Auth.UATValidator
			jwtV = snap.Auth.JWTValidator
			if jwtV == nil {
				jwtV = s.jwtValidator
			}
			configAPIKey = snap.Auth.APIKey
		} else {
			scheme = s.config.Auth.Scheme
			uatV = s.uatValidator
			jwtV = s.jwtValidator
			configAPIKey = s.config.Auth.APIKey
		}

		switch scheme {
		case "none":
			next.ServeHTTP(w, r)
			return

		case "hubUAT":
			token := extractBearerOrAPIKey(r)
			if !strings.HasPrefix(token, "scion_pat_") {
				http.Error(w, "unauthorized: expected scion_pat_* token", http.StatusUnauthorized)
				return
			}
			if uatV == nil {
				s.log.Error("hubUAT scheme configured but UAT validator not initialized")
				http.Error(w, "internal server error", http.StatusInternalServerError)
				return
			}
			caller, err := uatV.Validate(r.Context(), token)
			if err != nil {
				s.log.Debug("UAT validation failed", "error", err)
				http.Error(w, "unauthorized", http.StatusUnauthorized)
				return
			}
			ctx := withCallerIdentity(r.Context(), caller)
			next.ServeHTTP(w, r.WithContext(ctx))
			return

		case "hubJWT":
			token := extractBearerToken(r)
			if token == "" {
				http.Error(w, "unauthorized: missing bearer token", http.StatusUnauthorized)
				return
			}
			if jwtV == nil {
				s.log.Error("hubJWT scheme configured but JWT validator not initialized")
				http.Error(w, "internal server error", http.StatusInternalServerError)
				return
			}
			caller, err := jwtV.Validate(token)
			if err != nil {
				s.log.Debug("JWT validation failed", "error", err)
				http.Error(w, "unauthorized", http.StatusUnauthorized)
				return
			}
			ctx := withCallerIdentity(r.Context(), caller)
			next.ServeHTTP(w, r.WithContext(ctx))
			return

		case "federation":
			// Federation pass-through: decode the JWT WITHOUT verification
			// for bridge-local bookkeeping (task isolation, logging).
			// The hub validates the token via X-Scion-Federation-Token.
			//
			// NOTE: The bridge decodes federation tokens WITHOUT signature verification.
			// This is by design: the hub validates the token via its FederationAuthenticator
			// when the bridge passes it in the X-Scion-Federation-Token header.
			// Bridge-level signature verification requires public OIDC discovery
			// endpoints, which is tracked as issue #930.
			token := extractBearerToken(r)
			if token == "" {
				http.Error(w, "unauthorized: missing bearer token", http.StatusUnauthorized)
				return
			}
			caller, err := decodeFederationToken(token)
			if err != nil {
				s.log.Debug("federation token decode failed", "error", err)
				http.Error(w, "unauthorized: malformed token", http.StatusUnauthorized)
				return
			}
			ctx := withCallerIdentity(r.Context(), caller)
			next.ServeHTTP(w, r.WithContext(ctx))
			return

		default:
			// Legacy schemes: "apiKey", "bearer", or "" (accept either header).
			// No CallerIdentity is injected.
			var apiKey string
			switch scheme {
			case "apiKey":
				apiKey = r.Header.Get("X-API-Key")
			case "bearer":
				apiKey = extractBearerToken(r)
			default:
				// When auth.scheme is unset (empty), accept credentials from either
				// X-API-Key or Authorization: Bearer headers for convenience.
				apiKey = r.Header.Get("X-API-Key")
				if apiKey == "" {
					apiKey = extractBearerToken(r)
				}
			}

			// Compare SHA-256 hashes to avoid leaking key length via timing.
			expectedHash := sha256.Sum256([]byte(configAPIKey))
			providedHash := sha256.Sum256([]byte(apiKey))
			if subtle.ConstantTimeCompare(expectedHash[:], providedHash[:]) != 1 {
				http.Error(w, "unauthorized", http.StatusUnauthorized)
				return
			}

			next.ServeHTTP(w, r)
		}
	})
}

// snapshotRateLimitMiddleware uses the snapshot's pre-built rate limiter.
// A nil limiter (rate limiting disabled) passes through.
func (s *Server) snapshotRateLimitMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Skip rate limiting for operational endpoints.
		if r.URL.Path == "/healthz" || r.URL.Path == "/readyz" || r.URL.Path == "/metrics" {
			next.ServeHTTP(w, r)
			return
		}

		snap := s.snapshot.Load()
		if snap.Limiter == nil {
			next.ServeHTTP(w, r)
			return
		}

		key := hashKey(extractBearerOrAPIKey(r))
		if key == "" {
			host, _, err := net.SplitHostPort(r.RemoteAddr)
			if err != nil {
				host = r.RemoteAddr
			}
			if snap.Config.RateLimit.TrustProxy {
				if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
					if i := strings.Index(xff, ","); i >= 0 {
						host = strings.TrimSpace(xff[:i])
					} else {
						host = strings.TrimSpace(xff)
					}
				}
			}
			key = host
		}

		if !snap.Limiter.Allow(key) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusTooManyRequests)
			w.Write([]byte(`{"error":"rate limit exceeded"}`))
			return
		}

		next.ServeHTTP(w, r)
	})
}

// effectiveConfig returns the effective config from the snapshot if available,
// or the static config as fallback.
func (s *Server) effectiveConfig() *Config {
	if s.snapshot != nil {
		snap := s.snapshot.Load()
		return &snap.Config
	}
	return s.config
}

// hubJWTAuthMiddleware validates Hub-issued JWTs and pins the service claim.
// This protects internal endpoints (like /internal/sweep) that must only be
// callable by the Hub scheduler, not by arbitrary Hub users.
func (s *Server) hubJWTAuthMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.jwtValidator == nil {
			http.Error(w, "sweep endpoint not configured", http.StatusServiceUnavailable)
			return
		}

		token := extractBearerToken(r)
		if token == "" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}

		identity, err := s.jwtValidator.Validate(token)
		if err != nil {
			http.Error(w, "invalid token", http.StatusUnauthorized)
			return
		}

		// PIN THE SERVICE CLAIM — this is the security-critical detail.
		// Without this check, any valid Hub user token can trigger sweeps.
		if identity.Role != "service" {
			http.Error(w, "forbidden: service role required", http.StatusForbidden)
			return
		}

		next.ServeHTTP(w, r)
	})
}

// handleInternalSweep runs the bridge sweep: reap stale tasks + purge old events.
func (s *Server) handleInternalSweep(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	s.bridge.RunSweep(ctx)

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	w.Write([]byte(`{"status":"ok"}`))
}

// extractBearerToken extracts the token from an Authorization: Bearer header.
func extractBearerToken(r *http.Request) string {
	auth := r.Header.Get("Authorization")
	if strings.HasPrefix(auth, "Bearer ") {
		return strings.TrimPrefix(auth, "Bearer ")
	}
	return ""
}

// extractBearerOrAPIKey extracts a token from Authorization: Bearer or X-API-Key headers.
func extractBearerOrAPIKey(r *http.Request) string {
	if token := extractBearerToken(r); token != "" {
		return token
	}
	return r.Header.Get("X-API-Key")
}
