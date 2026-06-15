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
	"net/http"
	"net/url"
	"regexp"
	"strings"

	"github.com/a2aproject/a2a-go/v2/a2asrv"
)

var slugRE = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,62}$`)

// Server is the A2A HTTP server that routes requests to the SDK handler.
type Server struct {
	bridge     *Bridge
	config     *Config
	metrics    *Metrics
	log        *slog.Logger
	sdkHandler http.Handler // SDK JSON-RPC handler
}

// NewServer creates a new A2A protocol server backed by the SDK.
func NewServer(bridge *Bridge, cfg *Config, metrics *Metrics, log *slog.Logger, sdkHandler http.Handler) *Server {
	return &Server{
		bridge:     bridge,
		config:     cfg,
		metrics:    metrics,
		log:        log,
		sdkHandler: sdkHandler,
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
	if cfg.Auth.Scheme != "" && cfg.Auth.Scheme != "apiKey" && cfg.Auth.Scheme != "bearer" && cfg.Auth.Scheme != "none" {
		return fmt.Errorf("unsupported auth.scheme: %q (supported: apiKey, bearer, none)", cfg.Auth.Scheme)
	}
	if (cfg.Auth.Scheme == "apiKey" || cfg.Auth.Scheme == "bearer") && cfg.Auth.APIKey == "" {
		return fmt.Errorf("auth.api_key is required when auth.scheme is %q", cfg.Auth.Scheme)
	}
	if cfg.Auth.APIKey == "" && cfg.Auth.Scheme != "none" {
		return fmt.Errorf("auth.api_key is required (set auth.scheme: \"none\" to explicitly disable authentication)")
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
	if s.config.Auth.Scheme == "none" {
		s.log.Warn("bridge auth is explicitly DISABLED (auth.scheme: none) — all requests will be accepted without authentication")
	} else if s.config.Auth.Scheme == "" {
		s.log.Warn("auth.scheme is empty: bridge will accept credentials from both X-API-Key and Authorization headers")
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

	// Wrap with middleware chain: metrics -> rate limit -> auth.
	handler := s.authMiddleware(mux)
	handler = RateLimitMiddleware(handler, s.config.RateLimit)
	handler = InstrumentHandler(handler, s.metrics)
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
		JSONRPC string       `json:"jsonrpc"`
		ID      interface{}  `json:"id"`
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

// authMiddleware validates API key authentication on non-public endpoints.
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

		if s.config.Auth.Scheme == "none" {
			next.ServeHTTP(w, r)
			return
		}

		var apiKey string
		switch s.config.Auth.Scheme {
		case "apiKey":
			apiKey = r.Header.Get("X-API-Key")
		case "bearer":
			auth := r.Header.Get("Authorization")
			if strings.HasPrefix(auth, "Bearer ") {
				apiKey = strings.TrimPrefix(auth, "Bearer ")
			}
		default:
			// When auth.scheme is unset (empty), accept credentials from either
			// X-API-Key or Authorization: Bearer headers for convenience.
			apiKey = r.Header.Get("X-API-Key")
			if apiKey == "" {
				auth := r.Header.Get("Authorization")
				if strings.HasPrefix(auth, "Bearer ") {
					apiKey = strings.TrimPrefix(auth, "Bearer ")
				}
			}
		}

		// Compare SHA-256 hashes to avoid leaking key length via timing.
		expectedHash := sha256.Sum256([]byte(s.config.Auth.APIKey))
		providedHash := sha256.Sum256([]byte(apiKey))
		if subtle.ConstantTimeCompare(expectedHash[:], providedHash[:]) != 1 {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}

		next.ServeHTTP(w, r)
	})
}
