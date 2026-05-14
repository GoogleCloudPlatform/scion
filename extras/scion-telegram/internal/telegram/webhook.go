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

package telegram

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"net/http"
)

const (
	// webhookPath is the URL path the webhook server listens on.
	webhookPath = "/telegram/webhook"

	// secretTokenHeader is the HTTP header Telegram uses to send the secret token.
	secretTokenHeader = "X-Telegram-Bot-Api-Secret-Token"
)

// WebhookServer is an HTTP server that receives webhook callbacks from Telegram.
// It validates the secret token, parses updates, and dispatches them to the
// provided handler function.
type WebhookServer struct {
	listenAddr string
	secretToken string
	handler     func(update Update)
	server      *http.Server
	log         *slog.Logger
	// actualAddr is populated after Start() with the resolved listen address.
	actualAddr string
}

// NewWebhookServer creates a new WebhookServer.
func NewWebhookServer(listenAddr, secretToken string, handler func(Update), log *slog.Logger) *WebhookServer {
	if log == nil {
		log = slog.Default()
	}
	return &WebhookServer{
		listenAddr:  listenAddr,
		secretToken: secretToken,
		handler:     handler,
		log:         log,
	}
}

// Start starts the HTTP server in a background goroutine.
// It returns the resolved listen address (useful when port 0 is used).
func (ws *WebhookServer) Start() (string, error) {
	mux := http.NewServeMux()
	mux.HandleFunc(webhookPath, ws.handleWebhook)

	ws.server = &http.Server{
		Addr:    ws.listenAddr,
		Handler: mux,
	}

	ln, err := net.Listen("tcp", ws.listenAddr)
	if err != nil {
		return "", err
	}
	ws.actualAddr = ln.Addr().String()

	go func() {
		if err := ws.server.Serve(ln); err != nil && err != http.ErrServerClosed {
			ws.log.Error("Webhook server error", "error", err)
		}
	}()

	ws.log.Info("Webhook server started", "addr", ws.actualAddr)
	return ws.actualAddr, nil
}

// Stop gracefully shuts down the webhook server.
func (ws *WebhookServer) Stop(ctx context.Context) error {
	if ws.server == nil {
		return nil
	}
	ws.log.Info("Stopping webhook server")
	return ws.server.Shutdown(ctx)
}

// handleWebhook processes incoming webhook requests from Telegram.
func (ws *WebhookServer) handleWebhook(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// Validate secret token.
	if ws.secretToken != "" {
		token := r.Header.Get(secretTokenHeader)
		if subtle.ConstantTimeCompare([]byte(token), []byte(ws.secretToken)) != 1 {
			ws.log.Warn("Webhook request with invalid secret token")
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
	}

	// Read and parse the update.
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20)) // 1MB limit
	if err != nil {
		ws.log.Warn("Failed to read webhook body", "error", err)
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}

	var update Update
	if err := json.Unmarshal(body, &update); err != nil {
		ws.log.Warn("Failed to parse webhook update", "error", err)
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}

	// Telegram expects a fast 200 OK response; process asynchronously if needed.
	w.WriteHeader(http.StatusOK)

	// Dispatch the update to the handler.
	if ws.handler != nil {
		ws.handler(update)
	}
}
