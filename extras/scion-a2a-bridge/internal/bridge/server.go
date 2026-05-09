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
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"
)

var slugRE = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,62}$`)

// A2A JSON-RPC error codes.
const (
	ErrCodeParseError        = -32700
	ErrCodeInvalidRequest    = -32600
	ErrCodeMethodNotFound    = -32601
	ErrCodeInvalidParams     = -32602
	ErrCodeInternalError     = -32603
	ErrCodeTaskNotFound      = -32001
	ErrCodeTaskNotCancelable = -32002
	ErrCodeUnsupportedOp     = -32004
)

// JSONRPCRequest represents an incoming JSON-RPC 2.0 request.
type JSONRPCRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      interface{}     `json:"id"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params"`
}

// JSONRPCResponse represents an outgoing JSON-RPC 2.0 response.
type JSONRPCResponse struct {
	JSONRPC string        `json:"jsonrpc"`
	ID      interface{}   `json:"id"`
	Result  interface{}   `json:"result,omitempty"`
	Error   *JSONRPCError `json:"error,omitempty"`
}

// JSONRPCError represents a JSON-RPC 2.0 error.
type JSONRPCError struct {
	Code    int         `json:"code"`
	Message string      `json:"message"`
	Data    interface{} `json:"data,omitempty"`
}

// SendMessageParams holds parameters for the SendMessage RPC method.
type SendMessageParams struct {
	Message       Message            `json:"message"`
	Configuration *SendMessageConfig `json:"configuration,omitempty"`
	ContextID     string             `json:"contextId,omitempty"`
	TaskID        string             `json:"taskId,omitempty"`
}

// SendMessageConfig holds SendMessage configuration options.
type SendMessageConfig struct {
	AcceptedOutputModes []string `json:"acceptedOutputModes,omitempty"`
	Blocking            *bool    `json:"blocking,omitempty"`
}

// TaskQueryParams holds parameters for GetTask/ListTasks.
type TaskQueryParams struct {
	ID        string `json:"id,omitempty"`
	ContextID string `json:"contextId,omitempty"`
}

// Server is the A2A HTTP server that handles JSON-RPC requests.
type Server struct {
	bridge  *Bridge
	config  *Config
	metrics *Metrics
	log     *slog.Logger
}

// NewServer creates a new A2A protocol server.
func NewServer(bridge *Bridge, cfg *Config, metrics *Metrics, log *slog.Logger) *Server {
	return &Server{
		bridge:  bridge,
		config:  cfg,
		metrics: metrics,
		log:     log,
	}
}

// ValidateConfig checks that required configuration fields are present and consistent.
func ValidateConfig(cfg *Config) error {
	if cfg.Bridge.ExternalURL == "" {
		return fmt.Errorf("bridge.external_url is required")
	}
	for _, g := range cfg.Groves {
		if strings.Contains(g.Slug, ":") {
			return fmt.Errorf("grove slug %q must not contain ':'", g.Slug)
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

	// Per-agent routes.
	mux.HandleFunc("GET /groves/{groveSlug}/agents/{agentSlug}/.well-known/agent-card.json", s.handleAgentCard)
	mux.HandleFunc("POST /groves/{groveSlug}/agents/{agentSlug}/jsonrpc", s.handleJSONRPC)

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
			"streaming":         false,
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
	groveSlug := r.PathValue("groveSlug")
	agentSlug := r.PathValue("agentSlug")

	if !slugRE.MatchString(groveSlug) || !slugRE.MatchString(agentSlug) {
		http.Error(w, "invalid slug", http.StatusBadRequest)
		return
	}

	groveCfg := s.bridge.GetGroveConfig(groveSlug)
	if groveCfg == nil {
		http.Error(w, "grove not found", http.StatusNotFound)
		return
	}

	if len(groveCfg.ExposedAgents) > 0 {
		found := false
		for _, a := range groveCfg.ExposedAgents {
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

	card := s.bridge.GenerateAgentCard(r.Context(), groveSlug, agentSlug)

	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "public, max-age=300")
	if err := json.NewEncoder(w).Encode(card); err != nil {
		s.log.Error("failed to encode agent card response", "error", err)
	}
}

func (s *Server) handleJSONRPC(w http.ResponseWriter, r *http.Request) {
	groveSlug := r.PathValue("groveSlug")
	agentSlug := r.PathValue("agentSlug")

	if !slugRE.MatchString(groveSlug) || !slugRE.MatchString(agentSlug) {
		s.writeRPCError(w, nil, ErrCodeInvalidParams, "invalid slug format")
		return
	}

	if err := s.bridge.AuthorizeExposed(groveSlug, agentSlug); err != nil {
		s.writeRPCError(w, nil, ErrCodeInvalidParams, "agent not found")
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)

	var req JSONRPCRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		s.writeRPCError(w, nil, ErrCodeParseError, "parse error")
		return
	}

	if req.JSONRPC != "2.0" {
		s.writeRPCError(w, req.ID, ErrCodeInvalidRequest, "invalid JSON-RPC version")
		return
	}

	// JSON-RPC 2.0 §4.1: notifications (id absent/null) must not receive responses.
	if req.ID == nil {
		s.log.Debug("ignoring JSON-RPC notification", "method", req.Method)
		return
	}

	s.log.Debug("JSON-RPC request",
		"method", req.Method,
		"grove", groveSlug,
		"agent", agentSlug,
	)

	switch req.Method {
	case "message/send":
		s.handleSendMessage(w, r, req, groveSlug, agentSlug)
	case "message/stream":
		s.handleStreamMessage(w, r, req, groveSlug, agentSlug)
	case "tasks/get":
		s.handleGetTask(w, r, req, groveSlug, agentSlug)
	case "tasks/list":
		s.handleListTasks(w, r, req, groveSlug, agentSlug)
	case "tasks/cancel":
		s.handleCancelTask(w, r, req, groveSlug, agentSlug)
	case "tasks/pushNotification/set":
		s.handleSetPushNotification(w, r, req, groveSlug, agentSlug)
	case "tasks/pushNotification/get":
		s.handleGetPushNotification(w, r, req, groveSlug, agentSlug)
	case "tasks/pushNotification/delete":
		s.handleDeletePushNotification(w, r, req, groveSlug, agentSlug)
	case "tasks/resubscribe":
		s.handleResubscribe(w, r, req, groveSlug, agentSlug)
	default:
		s.writeRPCError(w, req.ID, ErrCodeMethodNotFound, fmt.Sprintf("method %q not found", req.Method))
	}
}

func (s *Server) handleSendMessage(w http.ResponseWriter, r *http.Request, req JSONRPCRequest, groveSlug, agentSlug string) {
	var params SendMessageParams
	if err := json.Unmarshal(req.Params, &params); err != nil {
		s.log.Warn("invalid SendMessage params", "error", err)
		s.writeRPCError(w, req.ID, ErrCodeInvalidParams, "invalid parameters")
		return
	}

	if len(params.Message.Parts) == 0 {
		s.writeRPCError(w, req.ID, ErrCodeInvalidParams, "message.parts must be non-empty")
		return
	}
	if params.Message.Role != "" && params.Message.Role != RoleUser {
		s.writeRPCError(w, req.ID, ErrCodeInvalidParams, "message.role must be 'user'")
		return
	}

	blocking := true
	if params.Configuration != nil && params.Configuration.Blocking != nil {
		blocking = *params.Configuration.Blocking
	}

	result, err := s.bridge.SendMessage(r.Context(), groveSlug, agentSlug, params.ContextID, params.Message.Parts, blocking)
	if err != nil {
		s.log.Error("SendMessage failed", "error", err, "grove", groveSlug, "agent", agentSlug)
		switch {
		case errors.Is(err, ErrAgentNotFound):
			s.writeRPCError(w, req.ID, ErrCodeInvalidParams, "agent not found")
		case errors.Is(err, ErrContextUnknown):
			s.writeRPCError(w, req.ID, ErrCodeInvalidParams, "unknown context ID")
		default:
			s.writeRPCError(w, req.ID, ErrCodeInternalError, "internal error")
		}
		return
	}

	s.writeRPCResult(w, req.ID, result)
}

func (s *Server) handleGetTask(w http.ResponseWriter, r *http.Request, req JSONRPCRequest, groveSlug, agentSlug string) {
	var params TaskQueryParams
	if err := json.Unmarshal(req.Params, &params); err != nil {
		s.log.Warn("invalid GetTask params", "error", err)
		s.writeRPCError(w, req.ID, ErrCodeInvalidParams, "invalid parameters")
		return
	}

	if params.ID == "" {
		s.writeRPCError(w, req.ID, ErrCodeInvalidParams, "id is required")
		return
	}

	task, err := s.bridge.AuthorizeTask(params.ID, groveSlug, agentSlug)
	if err != nil {
		s.log.Error("GetTask failed", "error", err, "taskID", params.ID)
		s.writeRPCError(w, req.ID, ErrCodeInternalError, "internal error")
		return
	}
	if task == nil {
		s.writeRPCError(w, req.ID, ErrCodeTaskNotFound, "task not found")
		return
	}

	s.writeRPCResult(w, req.ID, &TaskResult{
		ID:        task.ID,
		ContextID: task.ContextID,
		Status:    TaskStatus{State: task.State},
	})
}

func (s *Server) handleListTasks(w http.ResponseWriter, r *http.Request, req JSONRPCRequest, groveSlug, agentSlug string) {
	var params TaskQueryParams
	if err := json.Unmarshal(req.Params, &params); err != nil {
		s.log.Warn("invalid ListTasks params", "error", err)
		s.writeRPCError(w, req.ID, ErrCodeInvalidParams, "invalid parameters")
		return
	}

	if params.ContextID == "" {
		s.writeRPCError(w, req.ID, ErrCodeInvalidParams, "contextId is required")
		return
	}

	authorized, authErr := s.bridge.AuthorizeContext(params.ContextID, groveSlug, agentSlug)
	if authErr != nil {
		s.log.Error("AuthorizeContext failed", "error", authErr, "contextID", params.ContextID)
		s.writeRPCError(w, req.ID, ErrCodeInternalError, "internal error")
		return
	}
	if !authorized {
		s.writeRPCError(w, req.ID, ErrCodeTaskNotFound, "context not found")
		return
	}

	tasks, err := s.bridge.ListTasks(r.Context(), params.ContextID)
	if err != nil {
		s.log.Error("ListTasks failed", "error", err, "contextID", params.ContextID)
		s.writeRPCError(w, req.ID, ErrCodeInternalError, "internal error")
		return
	}

	s.writeRPCResult(w, req.ID, tasks)
}

func (s *Server) handleCancelTask(w http.ResponseWriter, r *http.Request, req JSONRPCRequest, groveSlug, agentSlug string) {
	var params TaskQueryParams
	if err := json.Unmarshal(req.Params, &params); err != nil {
		s.log.Warn("invalid CancelTask params", "error", err)
		s.writeRPCError(w, req.ID, ErrCodeInvalidParams, "invalid parameters")
		return
	}

	if params.ID == "" {
		s.writeRPCError(w, req.ID, ErrCodeInvalidParams, "id is required")
		return
	}

	task, err := s.bridge.AuthorizeTask(params.ID, groveSlug, agentSlug)
	if err != nil {
		s.log.Error("CancelTask auth failed", "error", err, "taskID", params.ID)
		s.writeRPCError(w, req.ID, ErrCodeInternalError, "internal error")
		return
	}
	if task == nil {
		s.writeRPCError(w, req.ID, ErrCodeTaskNotFound, "task not found")
		return
	}

	result, err := s.bridge.CancelTask(r.Context(), params.ID)
	if err != nil {
		s.log.Error("CancelTask failed", "error", err, "taskID", params.ID)
		s.writeRPCError(w, req.ID, ErrCodeTaskNotCancelable, "task cannot be canceled")
		return
	}
	if result == nil {
		s.writeRPCError(w, req.ID, ErrCodeTaskNotFound, "task not found")
		return
	}

	s.writeRPCResult(w, req.ID, result)
}

func (s *Server) handleStreamMessage(w http.ResponseWriter, r *http.Request, req JSONRPCRequest, groveSlug, agentSlug string) {
	s.log.Warn("message/stream request received — MVP limitation: streaming treats the first content message as terminal; multi-turn agents will break",
		"grove", groveSlug, "agent", agentSlug)

	var params SendMessageParams
	if err := json.Unmarshal(req.Params, &params); err != nil {
		s.log.Warn("invalid StreamMessage params", "error", err)
		s.writeRPCError(w, req.ID, ErrCodeInvalidParams, "invalid parameters")
		return
	}

	if len(params.Message.Parts) == 0 {
		s.writeRPCError(w, req.ID, ErrCodeInvalidParams, "message.parts must be non-empty")
		return
	}
	if params.Message.Role != "" && params.Message.Role != RoleUser {
		s.writeRPCError(w, req.ID, ErrCodeInvalidParams, "message.role must be 'user'")
		return
	}

	taskID, events, cleanup, err := s.bridge.SendStreamingMessage(r.Context(), groveSlug, agentSlug, params.ContextID, params.Message.Parts)
	if err != nil {
		s.log.Error("SendStreamingMessage failed", "error", err, "grove", groveSlug, "agent", agentSlug)
		s.writeRPCError(w, req.ID, ErrCodeInternalError, "internal error")
		return
	}
	defer cleanup()

	s.writeSSEStream(w, r, taskID, events)
}

func (s *Server) handleResubscribe(w http.ResponseWriter, r *http.Request, req JSONRPCRequest, groveSlug, agentSlug string) {
	var params TaskQueryParams
	if err := json.Unmarshal(req.Params, &params); err != nil {
		s.log.Warn("invalid Resubscribe params", "error", err)
		s.writeRPCError(w, req.ID, ErrCodeInvalidParams, "invalid parameters")
		return
	}

	if params.ID == "" {
		s.writeRPCError(w, req.ID, ErrCodeInvalidParams, "id is required")
		return
	}

	task, err := s.bridge.AuthorizeTask(params.ID, groveSlug, agentSlug)
	if err != nil {
		s.log.Error("Resubscribe auth failed", "error", err, "taskID", params.ID)
		s.writeRPCError(w, req.ID, ErrCodeInternalError, "internal error")
		return
	}
	if task == nil {
		s.writeRPCError(w, req.ID, ErrCodeTaskNotFound, "task not found")
		return
	}

	events, cleanup, err := s.bridge.SubscribeToTask(r.Context(), params.ID)
	if err != nil {
		s.log.Error("SubscribeToTask failed", "error", err, "taskID", params.ID)
		s.writeRPCError(w, req.ID, ErrCodeInternalError, "internal error")
		return
	}
	defer cleanup()

	s.writeSSEStream(w, r, params.ID, events)
}

func (s *Server) writeSSEStream(w http.ResponseWriter, r *http.Request, taskID string, events <-chan StreamEvent) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming not supported", http.StatusInternalServerError)
		return
	}

	// Disable the global WriteTimeout for this long-lived SSE connection.
	rc := http.NewResponseController(w)
	if err := rc.SetWriteDeadline(time.Time{}); err != nil {
		s.log.Warn("failed to disable write deadline for SSE", "error", err)
	}

	if s.metrics != nil {
		s.metrics.ActiveSSE.Inc()
		defer s.metrics.ActiveSSE.Dec()
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	keepalive := s.config.Timeouts.SSEKeepalive
	if keepalive == 0 {
		keepalive = 30 * time.Second
	}
	ticker := time.NewTicker(keepalive)
	defer ticker.Stop()

	for {
		select {
		case event, ok := <-events:
			if !ok {
				return
			}
			data, err := json.Marshal(event)
			if err != nil {
				s.log.Error("marshal SSE event", "error", err)
				continue
			}
			// SSE spec: each line of a multi-line payload must be prefixed with "data: ".
			dataStr := string(data)
			lines := strings.Split(dataStr, "\n")
			for _, line := range lines {
				fmt.Fprintf(w, "data: %s\n", line)
			}
			fmt.Fprintf(w, "\n")
			flusher.Flush()

			if event.StatusUpdate != nil && event.StatusUpdate.Final {
				return
			}
		case <-ticker.C:
			fmt.Fprintf(w, ": keepalive\n\n")
			flusher.Flush()
		case <-r.Context().Done():
			return
		}
	}
}

// PushNotificationParams holds parameters for push notification operations.
type PushNotificationParams struct {
	TaskID          string `json:"taskId"`
	ID              string `json:"id,omitempty"`
	URL             string `json:"url,omitempty"`
	Token           string `json:"token,omitempty"`
	AuthScheme      string `json:"authScheme,omitempty"`
	AuthCredentials string `json:"authCredentials,omitempty"`
}

func (s *Server) handleSetPushNotification(w http.ResponseWriter, r *http.Request, req JSONRPCRequest, groveSlug, agentSlug string) {
	var params PushNotificationParams
	if err := json.Unmarshal(req.Params, &params); err != nil {
		s.log.Warn("invalid SetPushNotification params", "error", err)
		s.writeRPCError(w, req.ID, ErrCodeInvalidParams, "invalid parameters")
		return
	}

	task, err := s.bridge.AuthorizeTask(params.TaskID, groveSlug, agentSlug)
	if err != nil {
		s.log.Error("SetPushNotification auth failed", "error", err, "taskID", params.TaskID)
		s.writeRPCError(w, req.ID, ErrCodeInternalError, "internal error")
		return
	}
	if task == nil {
		s.writeRPCError(w, req.ID, ErrCodeTaskNotFound, "task not found")
		return
	}

	parsed, err := url.Parse(params.URL)
	if err != nil || parsed.Host == "" || (parsed.Scheme != "http" && parsed.Scheme != "https") {
		s.writeRPCError(w, req.ID, ErrCodeInvalidParams, "url must be an absolute http or https URL")
		return
	}

	// SSRF validation is also enforced inside SetPushNotificationConfig (defense-in-depth).
	cfg, err := s.bridge.SetPushNotificationConfig(r.Context(), params.TaskID, params.URL, params.Token, params.AuthScheme, params.AuthCredentials)
	if err != nil {
		s.log.Error("SetPushNotificationConfig failed", "error", err, "taskID", params.TaskID)
		s.writeRPCError(w, req.ID, ErrCodeInternalError, "internal error")
		return
	}

	s.writeRPCResult(w, req.ID, cfg)
}

func (s *Server) handleGetPushNotification(w http.ResponseWriter, r *http.Request, req JSONRPCRequest, groveSlug, agentSlug string) {
	var params PushNotificationParams
	if err := json.Unmarshal(req.Params, &params); err != nil {
		s.log.Warn("invalid GetPushNotification params", "error", err)
		s.writeRPCError(w, req.ID, ErrCodeInvalidParams, "invalid parameters")
		return
	}

	task, err := s.bridge.AuthorizeTask(params.TaskID, groveSlug, agentSlug)
	if err != nil {
		s.log.Error("GetPushNotification auth failed", "error", err, "taskID", params.TaskID)
		s.writeRPCError(w, req.ID, ErrCodeInternalError, "internal error")
		return
	}
	if task == nil {
		s.writeRPCError(w, req.ID, ErrCodeTaskNotFound, "task not found")
		return
	}

	configs, err := s.bridge.GetPushNotificationConfig(r.Context(), params.TaskID)
	if err != nil {
		s.log.Error("GetPushNotificationConfig failed", "error", err, "taskID", params.TaskID)
		s.writeRPCError(w, req.ID, ErrCodeInternalError, "internal error")
		return
	}

	s.writeRPCResult(w, req.ID, configs)
}

func (s *Server) handleDeletePushNotification(w http.ResponseWriter, r *http.Request, req JSONRPCRequest, groveSlug, agentSlug string) {
	var params PushNotificationParams
	if err := json.Unmarshal(req.Params, &params); err != nil {
		s.log.Warn("invalid DeletePushNotification params", "error", err)
		s.writeRPCError(w, req.ID, ErrCodeInvalidParams, "invalid parameters")
		return
	}

	if params.TaskID == "" {
		s.writeRPCError(w, req.ID, ErrCodeInvalidParams, "taskId is required")
		return
	}

	task, err := s.bridge.AuthorizeTask(params.TaskID, groveSlug, agentSlug)
	if err != nil {
		s.log.Error("DeletePushNotification auth failed", "error", err, "taskID", params.TaskID)
		s.writeRPCError(w, req.ID, ErrCodeInternalError, "internal error")
		return
	}
	if task == nil {
		s.writeRPCError(w, req.ID, ErrCodeTaskNotFound, "task not found")
		return
	}

	if err := s.bridge.DeletePushNotificationConfig(r.Context(), params.TaskID, params.ID); err != nil {
		s.log.Error("DeletePushNotificationConfig failed", "error", err, "pushID", params.ID)
		s.writeRPCError(w, req.ID, ErrCodeInternalError, "internal error")
		return
	}

	s.writeRPCResult(w, req.ID, map[string]bool{"ok": true})
}

// normalizeJSONRPCID ensures the id conforms to JSON-RPC 2.0 (string, number, or null).
// Per §4, fractional numbers and structured values (object/array) are forbidden as IDs.
// We coerce invalid types to null rather than echoing them, accepting that this makes
// client-side correlation impossible for malformed requests.
func normalizeJSONRPCID(id interface{}) interface{} {
	switch id.(type) {
	case float64, string:
		return id
	case nil:
		return nil
	default:
		return nil
	}
}

func (s *Server) writeRPCResult(w http.ResponseWriter, id interface{}, result interface{}) {
	resp := JSONRPCResponse{
		JSONRPC: "2.0",
		ID:      normalizeJSONRPCID(id),
		Result:  result,
	}
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(resp); err != nil {
		s.log.Error("failed to encode RPC result", "error", err)
	}
}

func (s *Server) writeRPCError(w http.ResponseWriter, id interface{}, code int, message string) {
	resp := JSONRPCResponse{
		JSONRPC: "2.0",
		ID:      normalizeJSONRPCID(id),
		Error:   &JSONRPCError{Code: code, Message: message},
	}
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(resp); err != nil {
		s.log.Error("failed to encode RPC error", "error", err)
	}
}

// authMiddleware validates API key authentication on non-public endpoints.
func (s *Server) authMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Public endpoints skip auth.
		if r.URL.Path == "/.well-known/agent-card.json" || r.URL.Path == "/healthz" || r.URL.Path == "/readyz" {
			next.ServeHTTP(w, r)
			return
		}
		// Per-agent card: exactly /groves/{slug}/agents/{slug}/.well-known/agent-card.json
		segments := strings.Split(strings.TrimPrefix(r.URL.Path, "/"), "/")
		if len(segments) == 6 && segments[0] == "groves" && segments[2] == "agents" && segments[4] == ".well-known" && segments[5] == "agent-card.json" {
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
