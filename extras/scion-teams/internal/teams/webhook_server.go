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

package teams

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"time"
)

// ActivityHandler processes Bot Framework Activities received by the webhook server.
type ActivityHandler interface {
	HandleActivity(ctx context.Context, activity *Activity) (*InvokeResponse, error)
}

// WebhookServer receives HTTP POST requests from the Bot Framework Service.
type WebhookServer struct {
	server    *http.Server
	validator *JWTValidator
	handler   ActivityHandler
	log       *slog.Logger
	addr      string
	listener  net.Listener
}

// NewWebhookServer creates a new WebhookServer that validates JWT tokens
// and dispatches Activities to the handler.
func NewWebhookServer(addr string, validator *JWTValidator, handler ActivityHandler, log *slog.Logger) *WebhookServer {
	ws := &WebhookServer{
		validator: validator,
		handler:   handler,
		log:       log,
		addr:      addr,
	}

	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/messages", ws.handleMessages)
	// Health check endpoint.
	mux.HandleFunc("GET /health", ws.handleHealth)

	ws.server = &http.Server{
		Addr:         addr,
		Handler:      mux,
		ReadTimeout:  10 * time.Second,
		WriteTimeout: 10 * time.Second,
		IdleTimeout:  120 * time.Second,
	}

	return ws
}

// Start begins listening for incoming Activities. It blocks until the server
// is stopped or encounters a fatal error.
func (ws *WebhookServer) Start() error {
	var err error
	ws.listener, err = net.Listen("tcp", ws.addr)
	if err != nil {
		return fmt.Errorf("listen on %s: %w", ws.addr, err)
	}

	ws.log.Info("Webhook server listening", "addr", ws.listener.Addr().String())

	if err := ws.server.Serve(ws.listener); err != nil && err != http.ErrServerClosed {
		return fmt.Errorf("webhook server error: %w", err)
	}
	return nil
}

// Stop gracefully shuts down the webhook server.
func (ws *WebhookServer) Stop(ctx context.Context) error {
	ws.log.Info("Stopping webhook server")
	return ws.server.Shutdown(ctx)
}

// Addr returns the listener address, useful for tests using port 0.
func (ws *WebhookServer) Addr() string {
	if ws.listener != nil {
		return ws.listener.Addr().String()
	}
	return ws.addr
}

// handleMessages processes incoming Activity POST requests.
func (ws *WebhookServer) handleMessages(w http.ResponseWriter, r *http.Request) {
	// 1. Extract and validate Bearer token.
	authHeader := r.Header.Get("Authorization")
	if authHeader == "" {
		ws.log.Warn("Missing Authorization header")
		http.Error(w, "missing Authorization header", http.StatusUnauthorized)
		return
	}

	if !strings.HasPrefix(authHeader, "Bearer ") {
		ws.log.Warn("Invalid Authorization header format")
		http.Error(w, "invalid Authorization header", http.StatusUnauthorized)
		return
	}

	tokenString := strings.TrimPrefix(authHeader, "Bearer ")

	_, err := ws.validator.ValidateToken(r.Context(), tokenString)
	if err != nil {
		ws.log.Warn("JWT validation failed", "error", err)
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}

	// 2. Read and deserialize Activity.
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 1<<20)) // 1MB limit
	if err != nil {
		ws.log.Error("Failed to read request body", "error", err)
		http.Error(w, "failed to read body", http.StatusBadRequest)
		return
	}

	var activity Activity
	if err := json.Unmarshal(body, &activity); err != nil {
		ws.log.Warn("Failed to parse Activity JSON", "error", err)
		http.Error(w, "invalid Activity JSON", http.StatusBadRequest)
		return
	}

	ws.log.Debug("Received Activity",
		"type", activity.Type,
		"id", activity.ID,
		"from", activity.From.Name,
		"conversation", activity.Conversation.ID,
	)

	// 3. Dispatch to handler.
	// For non-invoke activities, acknowledge immediately and dispatch async.
	// This avoids blocking the HTTP response on hub delivery (design doc §4.1).
	if activity.Type != "invoke" {
		w.WriteHeader(http.StatusOK)
		go func() {
			defer func() {
				if r := recover(); r != nil {
					ws.log.Error("Panic in async activity handler",
						"panic", r,
						"type", activity.Type,
						"activity_id", activity.ID,
					)
				}
			}()
			if _, err := ws.handler.HandleActivity(context.Background(), &activity); err != nil {
				ws.log.Error("Async activity handler error", "error", err, "type", activity.Type)
			}
		}()
		return
	}

	// Invoke activities need synchronous processing to return InvokeResponse.
	invokeResp, err := ws.handler.HandleActivity(r.Context(), &activity)
	if err != nil {
		ws.log.Error("Activity handler error", "error", err, "type", activity.Type)
		http.Error(w, "handler error", http.StatusInternalServerError)
		return
	}

	// Return the InvokeResponse body.
	if invokeResp != nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(invokeResp.Status)
		json.NewEncoder(w).Encode(invokeResp.Body)
		return
	}

	w.WriteHeader(http.StatusOK)
}

// handleHealth returns a simple health check response.
func (ws *WebhookServer) handleHealth(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
}
