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
	"sync"
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
	handler    func(update Update)
	server     *http.Server
	log        *slog.Logger
	// listener is bound by Listen and served by Serve.
	listener net.Listener
	// actualAddr is populated by Listen with the resolved listen address.
	actualAddr string

	secretMu    sync.RWMutex
	secretToken string
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

// Start binds the listen address and serves in a background goroutine.
// It returns the resolved listen address (useful when port 0 is used).
func (ws *WebhookServer) Start() (string, error) {
	addr, err := ws.Listen()
	if err != nil {
		return "", err
	}
	ws.Serve()
	return addr, nil
}

// Listen binds the listen address without serving it yet, so a caller can
// find out the address is unavailable before committing to the server.
// It returns the resolved listen address.
func (ws *WebhookServer) Listen() (string, error) {
	ln, err := net.Listen("tcp", ws.listenAddr)
	if err != nil {
		return "", err
	}
	ws.listener = ln
	ws.actualAddr = ln.Addr().String()
	return ws.actualAddr, nil
}

// Serve serves the listener bound by Listen in a background goroutine.
func (ws *WebhookServer) Serve() {
	mux := http.NewServeMux()
	mux.HandleFunc(webhookPath, ws.handleWebhook)

	ws.server = &http.Server{
		Addr:    ws.listenAddr,
		Handler: mux,
	}

	ln := ws.listener
	go func() {
		if err := ws.server.Serve(ln); err != nil && err != http.ErrServerClosed {
			ws.log.Error("Webhook server error", "error", err)
		}
	}()

	ws.log.Info("Webhook server started", "addr", ws.actualAddr)
}

// SetSecretToken replaces the secret token that inbound requests must carry.
func (ws *WebhookServer) SetSecretToken(token string) {
	ws.secretMu.Lock()
	defer ws.secretMu.Unlock()
	ws.secretToken = token
}

func (ws *WebhookServer) currentSecretToken() string {
	ws.secretMu.RLock()
	defer ws.secretMu.RUnlock()
	return ws.secretToken
}

// Stop gracefully shuts down the webhook server. A listener that was bound
// but never served is closed.
func (ws *WebhookServer) Stop(ctx context.Context) error {
	if ws.server == nil {
		if ws.listener != nil {
			return ws.listener.Close()
		}
		return nil
	}
	ws.log.Info("Stopping webhook server")
	return ws.server.Shutdown(ctx)
}

// handleWebhook processes incoming webhook requests from Telegram.
func (ws *WebhookServer) handleWebhook(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	// Validate secret token.
	if secretToken := ws.currentSecretToken(); secretToken != "" {
		token := r.Header.Get(secretTokenHeader)
		if subtle.ConstantTimeCompare([]byte(token), []byte(secretToken)) != 1 {
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
