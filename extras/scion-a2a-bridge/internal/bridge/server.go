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
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/a2aproject/a2a-go/v2/a2asrv"
)

var slugRE = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,62}$`)

// Server is the A2A HTTP server that routes requests to the SDK handler.
type Server struct {
	bridge       *Bridge
	config       *Config
	metrics      *Metrics
	log          *slog.Logger
	sdkHandler   http.Handler // SDK JSON-RPC handler
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

// SetJWTValidator sets the JWT validator for hubJWT mode. Called after the
// signing key is loaded (which may require Secret Manager access).
func (s *Server) SetJWTValidator(v *JWTValidator) {
	s.jwtValidator = v
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
	case "", "apiKey", "bearer", "none", "hubUAT", "hubJWT":
		// valid
	default:
		return fmt.Errorf("unsupported auth.scheme: %q (supported: apiKey, bearer, none, hubUAT, hubJWT)", cfg.Auth.Scheme)
	}
	if (cfg.Auth.Scheme == "apiKey" || cfg.Auth.Scheme == "bearer") && cfg.Auth.APIKey == "" {
		return fmt.Errorf("auth.api_key is required when auth.scheme is %q", cfg.Auth.Scheme)
	}
	// api_key is required for legacy schemes and the default (empty) scheme.
	// hubUAT and hubJWT do not use api_key — they validate per-user credentials instead.
	if cfg.Auth.APIKey == "" && cfg.Auth.Scheme != "none" && cfg.Auth.Scheme != "hubUAT" && cfg.Auth.Scheme != "hubJWT" {
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
	// Require explicit opt-in for unauthenticated gRPC/REST transports.
	if cfg.Bridge.GRPCListenAddress != "" && !cfg.Bridge.GRPCInsecure && cfg.Auth.Scheme == "none" {
		return fmt.Errorf("gRPC transport is configured but auth.scheme is \"none\"; set bridge.grpc_insecure: true to acknowledge unauthenticated gRPC access, or configure auth")
	}
	if cfg.Bridge.RESTListenAddress != "" && !cfg.Bridge.RESTInsecure && cfg.Auth.Scheme == "none" {
		return fmt.Errorf("REST transport is configured but auth.scheme is \"none\"; set bridge.rest_insecure: true to acknowledge unauthenticated REST access, or configure auth")
	}
	return nil
}

// WarnOnOpenAuth logs a warning if the auth configuration leaves the bridge open.
func (s *Server) WarnOnOpenAuth() {
	switch s.config.Auth.Scheme {
	case "none":
		s.log.Warn("bridge auth is explicitly DISABLED (auth.scheme: none) — all requests will be accepted without authentication")
	case "":
		s.log.Warn("auth.scheme is empty: bridge will accept credentials from both X-API-Key and Authorization headers")
	case "hubUAT":
		s.log.Info("bridge auth: hubUAT — per-user Scion UAT authentication enabled")
	case "hubJWT":
		s.log.Info("bridge auth: hubJWT — per-user Scion JWT authentication enabled")
	}
	if s.config.RateLimit.TrustProxy {
		s.log.Warn("rate_limit.trust_proxy is enabled — X-Forwarded-For is trusted unconditionally, which allows clients to spoof their IP and bypass per-IP rate limits; consider adding network-level proxy restrictions")
	}
}

// Handler returns an http.Handler for the A2A server routes.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()

	// Top-level well-known agent card (registry).
	mux.HandleFunc("GET /.well-known/agent-card.json", s.handleWellKnownAgentCard)

	// Per-agent routes — the SDK handler handles JSON-RPC protocol.
	mux.HandleFunc("GET /projects/{projectSlug}/agents/{agentSlug}/.well-known/agent-card.json", s.handleAgentCard)
	mux.HandleFunc("POST /projects/{projectSlug}/agents/{agentSlug}/jsonrpc", s.handleJSONRPC)

	// Legacy per-agent routes (backward compatibility for "grove" naming).
	mux.HandleFunc("GET /groves/{projectSlug}/agents/{agentSlug}/.well-known/agent-card.json", s.handleAgentCard)
	mux.HandleFunc("POST /groves/{projectSlug}/agents/{agentSlug}/jsonrpc", s.handleJSONRPC)

	// Health, readiness, and metrics.
	mux.HandleFunc("GET /healthz", s.handleHealthz)
	mux.HandleFunc("GET /readyz", s.handleReadyz)
	mux.Handle("GET /metrics", MetricsHandler())

	// Wrap with middleware chain: SSE deadlines -> metrics -> rate limit -> auth.
	// SSEWriteDeadlineMiddleware is outermost so it wraps the raw
	// http.ResponseWriter: it replaces the server's fixed WriteTimeout with a
	// rolling per-write deadline for text/event-stream responses, which keeps
	// long-lived streams alive without disabling write deadlines globally.
	handler := s.authMiddleware(mux)
	handler = RateLimitMiddleware(handler, s.config.RateLimit)
	handler = InstrumentHandler(handler, s.metrics)
	handler = SSEWriteDeadlineMiddleware(handler)
	return handler
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

	if err := s.bridge.store.Ping(); err != nil {
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
	registry := map[string]interface{}{
		"name":        "scion-a2a-bridge",
		"description": "Scion A2A Protocol Bridge — exposes Scion agents as A2A endpoints",
		"url":         s.config.Bridge.ExternalURL,
		"version":     "1.0.0",
		"capabilities": map[string]bool{
			"streaming":         true,
			"pushNotifications": false,
		},
	}

	if s.config.Bridge.Provider.Organization != "" {
		registry["provider"] = map[string]string{
			"organization": s.config.Bridge.Provider.Organization,
			"url":          s.config.Bridge.Provider.URL,
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

	// Limit request body to 1 MB to prevent memory exhaustion from oversized payloads.
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)

	// Inject routing info into context for the executor.
	ctx := WithRouteInfo(r.Context(), RouteInfo{
		ProjectSlug: projectSlug,
		AgentSlug:   agentSlug,
	})
	r = r.WithContext(ctx)

	// Delegate to SDK JSON-RPC handler.
	s.sdkHandler.ServeHTTP(w, r)
}

// normalizeJSONRPCID ensures only valid JSON-RPC 2.0 ID types (string, number,
// null) are echoed back. Arrays, objects, and booleans are replaced with null
// per JSON-RPC 2.0 §4.1.
func normalizeJSONRPCID(id interface{}) interface{} {
	switch id.(type) {
	case nil, string, float64, int, int64:
		return id
	case json.Number:
		return id
	default:
		return nil
	}
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
		ID:      normalizeJSONRPCID(id),
		Error:   &jsonrpcError{Code: code, Message: message},
	}
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(resp); err != nil {
		slog.Default().Error("failed to encode JSON-RPC error response", "error", err)
	}
}

// verifyCredential checks that the provided credential matches the configured API key.
func verifyCredential(provided, expected string) bool {
	expectedHash := sha256.Sum256([]byte(expected))
	providedHash := sha256.Sum256([]byte(provided))
	return subtle.ConstantTimeCompare(expectedHash[:], providedHash[:]) == 1
}

// authError describes an authentication failure in a transport-neutral way so
// that HTTP, REST, and gRPC callers can map it onto their own error model.
type authError struct {
	msg string
	// internal marks a server-side misconfiguration (500) rather than a
	// caller error (401).
	internal bool
}

func (e *authError) Error() string { return e.msg }

// headerLookup returns the value of a request header/metadata entry. Keys are
// the canonical HTTP names ("Authorization", "X-API-Key"); implementations are
// responsible for any transport-specific casing (gRPC metadata is lowercase).
type headerLookup func(name string) string

// authenticate validates caller credentials against the configured auth scheme.
// It is transport-neutral: the JSON-RPC HTTP middleware, the REST middleware,
// and the gRPC interceptors all go through it so every transport supports the
// same schemes. For the per-user schemes (hubUAT/hubJWT) the returned context
// carries the resolved CallerIdentity; legacy schemes return ctx unchanged.
func (s *Server) authenticate(ctx context.Context, header headerLookup) (context.Context, *authError) {
	switch s.config.Auth.Scheme {
	case "none":
		return ctx, nil

	case "hubUAT":
		token := bearerOrAPIKeyFrom(header)
		if !strings.HasPrefix(token, "scion_pat_") {
			return nil, &authError{msg: "unauthorized: expected scion_pat_* token"}
		}
		if s.uatValidator == nil {
			s.log.Error("hubUAT scheme configured but UAT validator not initialized")
			return nil, &authError{msg: "internal server error", internal: true}
		}
		caller, err := s.uatValidator.Validate(ctx, token)
		if err != nil {
			s.log.Debug("UAT validation failed", "error", err)
			return nil, &authError{msg: "unauthorized"}
		}
		return withCallerIdentity(ctx, caller), nil

	case "hubJWT":
		token := bearerFrom(header)
		if token == "" {
			return nil, &authError{msg: "unauthorized: missing bearer token"}
		}
		if s.jwtValidator == nil {
			s.log.Error("hubJWT scheme configured but JWT validator not initialized")
			return nil, &authError{msg: "internal server error", internal: true}
		}
		caller, err := s.jwtValidator.Validate(token)
		if err != nil {
			s.log.Debug("JWT validation failed", "error", err)
			return nil, &authError{msg: "unauthorized"}
		}
		return withCallerIdentity(ctx, caller), nil

	default:
		// Legacy schemes: "apiKey", "bearer", or "" (accept either header).
		// No CallerIdentity is injected.
		var apiKey string
		switch s.config.Auth.Scheme {
		case "apiKey":
			apiKey = header("X-API-Key")
		case "bearer":
			apiKey = bearerFrom(header)
		default:
			// When auth.scheme is unset (empty), accept credentials from either
			// X-API-Key or Authorization: Bearer headers for convenience.
			apiKey = header("X-API-Key")
			if apiKey == "" {
				apiKey = bearerFrom(header)
			}
		}
		if !verifyCredential(apiKey, s.config.Auth.APIKey) {
			return nil, &authError{msg: "unauthorized"}
		}
		return ctx, nil
	}
}

// authMiddleware validates authentication on non-public endpoints.
func (s *Server) authMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Public endpoints skip auth.
		if r.URL.Path == "/.well-known/agent-card.json" || r.URL.Path == "/healthz" || r.URL.Path == "/readyz" {
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

		ctx, authErr := s.authenticate(r.Context(), r.Header.Get)
		if authErr != nil {
			writeAuthError(w, authErr)
			return
		}
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// AuthHTTPMiddleware wraps an http.Handler with the configured authentication,
// for transports served outside the main JSON-RPC mux (REST). It supports the
// same schemes as authMiddleware, including per-user hubUAT/hubJWT. Auth is
// skipped only when the operator explicitly opted out via bridge.rest_insecure
// or auth.scheme: "none".
func (s *Server) AuthHTTPMiddleware(next http.Handler) http.Handler {
	if s.config.Auth.Scheme == "none" || s.config.Bridge.RESTInsecure {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx, authErr := s.authenticate(r.Context(), r.Header.Get)
		if authErr != nil {
			writeAuthError(w, authErr)
			return
		}
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// writeAuthError maps an authError onto an HTTP response.
func writeAuthError(w http.ResponseWriter, err *authError) {
	if err.internal {
		http.Error(w, err.msg, http.StatusInternalServerError)
		return
	}
	http.Error(w, err.msg, http.StatusUnauthorized)
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

// bearerFrom extracts the token from an Authorization: Bearer header using a
// transport-neutral lookup.
func bearerFrom(header headerLookup) string {
	auth := header("Authorization")
	if strings.HasPrefix(auth, "Bearer ") {
		return strings.TrimPrefix(auth, "Bearer ")
	}
	return ""
}

// bearerOrAPIKeyFrom extracts a token from Authorization: Bearer or X-API-Key
// using a transport-neutral lookup.
func bearerOrAPIKeyFrom(header headerLookup) string {
	if token := bearerFrom(header); token != "" {
		return token
	}
	return header("X-API-Key")
}
