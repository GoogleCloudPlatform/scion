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
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"
)

// A2A JSON-RPC error codes.
const (
	ErrCodeParseError       = -32700
	ErrCodeInvalidRequest   = -32600
	ErrCodeMethodNotFound   = -32601
	ErrCodeInvalidParams    = -32602
	ErrCodeInternalError    = -32603
	ErrCodeTaskNotFound     = -32001
	ErrCodeTaskNotCancelable = -32002
	ErrCodeUnsupportedOp    = -32004
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

	// Wrap with middleware chain: metrics → rate limit → auth.
	handler := s.authMiddleware(mux)
	handler = RateLimitMiddleware(handler, s.config.RateLimit)
	handler = InstrumentHandler(handler, s.metrics)
	return handler
}

func (s *Server) handleHealthz(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
}

func (s *Server) handleReadyz(w http.ResponseWriter, r *http.Request) {
	checks := map[string]string{}
	ready := true

	if err := s.bridge.store.Ping(); err != nil {
		checks["database"] = "error: " + err.Error()
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
	json.NewEncoder(w).Encode(checks)
}

func (s *Server) handleWellKnownAgentCard(w http.ResponseWriter, r *http.Request) {
	// Return a registry card listing all configured groves/agents.
	registry := map[string]interface{}{
		"name":        "scion-a2a-bridge",
		"description": "Scion A2A Protocol Bridge — exposes Scion agents as A2A endpoints",
		"url":         s.config.Bridge.ExternalURL,
		"version":     "1.0.0",
		"capabilities": map[string]bool{
			"streaming":         true,
			"pushNotifications": true,
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
	json.NewEncoder(w).Encode(registry)
}

func (s *Server) handleAgentCard(w http.ResponseWriter, r *http.Request) {
	groveSlug := r.PathValue("groveSlug")
	agentSlug := r.PathValue("agentSlug")

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
	json.NewEncoder(w).Encode(card)
}

func (s *Server) handleJSONRPC(w http.ResponseWriter, r *http.Request) {
	groveSlug := r.PathValue("groveSlug")
	agentSlug := r.PathValue("agentSlug")

	var req JSONRPCRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		s.writeRPCError(w, nil, ErrCodeParseError, "parse error")
		return
	}

	if req.JSONRPC != "2.0" {
		s.writeRPCError(w, req.ID, ErrCodeInvalidRequest, "invalid JSON-RPC version")
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
		s.handleGetTask(w, r, req)
	case "tasks/list":
		s.handleListTasks(w, r, req)
	case "tasks/cancel":
		s.handleCancelTask(w, r, req)
	case "tasks/pushNotification/set":
		s.handleSetPushNotification(w, r, req)
	case "tasks/pushNotification/get":
		s.handleGetPushNotification(w, r, req)
	case "tasks/pushNotification/delete":
		s.handleDeletePushNotification(w, r, req)
	case "tasks/resubscribe":
		s.handleResubscribe(w, r, req)
	default:
		s.writeRPCError(w, req.ID, ErrCodeMethodNotFound, fmt.Sprintf("method %q not found", req.Method))
	}
}

func (s *Server) handleSendMessage(w http.ResponseWriter, r *http.Request, req JSONRPCRequest, groveSlug, agentSlug string) {
	var params SendMessageParams
	if err := json.Unmarshal(req.Params, &params); err != nil {
		s.writeRPCError(w, req.ID, ErrCodeInvalidParams, "invalid params: "+err.Error())
		return
	}

	blocking := true
	if params.Configuration != nil && params.Configuration.Blocking != nil {
		blocking = *params.Configuration.Blocking
	}

	result, err := s.bridge.SendMessage(r.Context(), groveSlug, agentSlug, params.ContextID, params.Message.Parts, blocking)
	if err != nil {
		s.log.Error("SendMessage failed", "error", err, "grove", groveSlug, "agent", agentSlug)
		s.writeRPCError(w, req.ID, ErrCodeInternalError, err.Error())
		return
	}

	s.writeRPCResult(w, req.ID, result)
}

func (s *Server) handleGetTask(w http.ResponseWriter, r *http.Request, req JSONRPCRequest) {
	var params TaskQueryParams
	if err := json.Unmarshal(req.Params, &params); err != nil {
		s.writeRPCError(w, req.ID, ErrCodeInvalidParams, "invalid params: "+err.Error())
		return
	}

	task, err := s.bridge.GetTask(r.Context(), params.ID)
	if err != nil {
		s.writeRPCError(w, req.ID, ErrCodeInternalError, err.Error())
		return
	}
	if task == nil {
		s.writeRPCError(w, req.ID, ErrCodeTaskNotFound, "task not found")
		return
	}

	s.writeRPCResult(w, req.ID, task)
}

func (s *Server) handleListTasks(w http.ResponseWriter, r *http.Request, req JSONRPCRequest) {
	var params TaskQueryParams
	if err := json.Unmarshal(req.Params, &params); err != nil {
		s.writeRPCError(w, req.ID, ErrCodeInvalidParams, "invalid params: "+err.Error())
		return
	}

	if params.ContextID == "" {
		s.writeRPCError(w, req.ID, ErrCodeInvalidParams, "contextId is required")
		return
	}

	tasks, err := s.bridge.ListTasks(r.Context(), params.ContextID)
	if err != nil {
		s.writeRPCError(w, req.ID, ErrCodeInternalError, err.Error())
		return
	}

	s.writeRPCResult(w, req.ID, tasks)
}

func (s *Server) handleCancelTask(w http.ResponseWriter, r *http.Request, req JSONRPCRequest) {
	var params TaskQueryParams
	if err := json.Unmarshal(req.Params, &params); err != nil {
		s.writeRPCError(w, req.ID, ErrCodeInvalidParams, "invalid params: "+err.Error())
		return
	}

	result, err := s.bridge.CancelTask(r.Context(), params.ID)
	if err != nil {
		s.writeRPCError(w, req.ID, ErrCodeTaskNotCancelable, err.Error())
		return
	}
	if result == nil {
		s.writeRPCError(w, req.ID, ErrCodeTaskNotFound, "task not found")
		return
	}

	s.writeRPCResult(w, req.ID, result)
}

func (s *Server) handleStreamMessage(w http.ResponseWriter, r *http.Request, req JSONRPCRequest, groveSlug, agentSlug string) {
	var params SendMessageParams
	if err := json.Unmarshal(req.Params, &params); err != nil {
		s.writeRPCError(w, req.ID, ErrCodeInvalidParams, "invalid params: "+err.Error())
		return
	}

	taskID, events, cleanup, err := s.bridge.SendStreamingMessage(r.Context(), groveSlug, agentSlug, params.ContextID, params.Message.Parts)
	if err != nil {
		s.log.Error("SendStreamingMessage failed", "error", err)
		s.writeRPCError(w, req.ID, ErrCodeInternalError, err.Error())
		return
	}
	defer cleanup()

	s.writeSSEStream(w, r, taskID, events)
}

func (s *Server) handleResubscribe(w http.ResponseWriter, r *http.Request, req JSONRPCRequest) {
	var params TaskQueryParams
	if err := json.Unmarshal(req.Params, &params); err != nil {
		s.writeRPCError(w, req.ID, ErrCodeInvalidParams, "invalid params: "+err.Error())
		return
	}

	events, cleanup, err := s.bridge.SubscribeToTask(r.Context(), params.ID)
	if err != nil {
		s.writeRPCError(w, req.ID, ErrCodeInternalError, err.Error())
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
			fmt.Fprintf(w, "data: %s\n\n", data)
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

func (s *Server) handleSetPushNotification(w http.ResponseWriter, r *http.Request, req JSONRPCRequest) {
	var params PushNotificationParams
	if err := json.Unmarshal(req.Params, &params); err != nil {
		s.writeRPCError(w, req.ID, ErrCodeInvalidParams, "invalid params: "+err.Error())
		return
	}

	cfg, err := s.bridge.SetPushNotificationConfig(r.Context(), params.TaskID, params.URL, params.Token, params.AuthScheme, params.AuthCredentials)
	if err != nil {
		s.writeRPCError(w, req.ID, ErrCodeInternalError, err.Error())
		return
	}

	s.writeRPCResult(w, req.ID, cfg)
}

func (s *Server) handleGetPushNotification(w http.ResponseWriter, r *http.Request, req JSONRPCRequest) {
	var params PushNotificationParams
	if err := json.Unmarshal(req.Params, &params); err != nil {
		s.writeRPCError(w, req.ID, ErrCodeInvalidParams, "invalid params: "+err.Error())
		return
	}

	configs, err := s.bridge.GetPushNotificationConfig(r.Context(), params.TaskID)
	if err != nil {
		s.writeRPCError(w, req.ID, ErrCodeInternalError, err.Error())
		return
	}

	s.writeRPCResult(w, req.ID, configs)
}

func (s *Server) handleDeletePushNotification(w http.ResponseWriter, r *http.Request, req JSONRPCRequest) {
	var params PushNotificationParams
	if err := json.Unmarshal(req.Params, &params); err != nil {
		s.writeRPCError(w, req.ID, ErrCodeInvalidParams, "invalid params: "+err.Error())
		return
	}

	if err := s.bridge.DeletePushNotificationConfig(r.Context(), params.ID); err != nil {
		s.writeRPCError(w, req.ID, ErrCodeInternalError, err.Error())
		return
	}

	s.writeRPCResult(w, req.ID, map[string]bool{"ok": true})
}

func (s *Server) writeRPCResult(w http.ResponseWriter, id interface{}, result interface{}) {
	resp := JSONRPCResponse{
		JSONRPC: "2.0",
		ID:      id,
		Result:  result,
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(resp)
}

func (s *Server) writeRPCError(w http.ResponseWriter, id interface{}, code int, message string) {
	resp := JSONRPCResponse{
		JSONRPC: "2.0",
		ID:      id,
		Error:   &JSONRPCError{Code: code, Message: message},
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(resp)
}

// authMiddleware validates API key authentication on non-public endpoints.
func (s *Server) authMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Public endpoints skip auth.
		if strings.HasSuffix(r.URL.Path, "agent-card.json") || r.URL.Path == "/healthz" || r.URL.Path == "/readyz" || r.URL.Path == "/metrics" {
			next.ServeHTTP(w, r)
			return
		}

		if s.config.Auth.APIKey == "" {
			next.ServeHTTP(w, r)
			return
		}

		apiKey := r.Header.Get("X-API-Key")
		if apiKey == "" {
			apiKey = r.Header.Get("Authorization")
			if strings.HasPrefix(apiKey, "Bearer ") {
				apiKey = strings.TrimPrefix(apiKey, "Bearer ")
			}
		}

		if apiKey != s.config.Auth.APIKey {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}

		next.ServeHTTP(w, r)
	})
}
