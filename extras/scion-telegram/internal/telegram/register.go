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
	"crypto/rand"
	"encoding/json"
	"fmt"
	"html/template"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	defaultRegisterAddr = ":9093"
	tokenTTL            = 10 * time.Minute
)

// pendingRegistration holds state for an in-progress /register flow.
type pendingRegistration struct {
	Token            string
	TelegramUserID   string
	TelegramUsername string
	ChatID           int64
	ExpiresAt        time.Time
}

// registrationServer handles the HTTP registration flow.
type registrationServer struct {
	mu       sync.Mutex
	pending  map[string]*pendingRegistration // token -> registration
	broker   *TelegramBroker
	srv      *http.Server
	listener net.Listener
}

func newRegistrationServer(broker *TelegramBroker) *registrationServer {
	return &registrationServer{
		pending: make(map[string]*pendingRegistration),
		broker:  broker,
	}
}

// start begins listening on the given address. It returns the actual
// address the server is listening on (useful when port 0 is specified).
func (rs *registrationServer) start(addr string) (string, error) {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return "", fmt.Errorf("listen on %s: %w", addr, err)
	}
	rs.listener = ln

	mux := http.NewServeMux()
	mux.HandleFunc("/register", rs.handleRegister)
	mux.HandleFunc("/health", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		fmt.Fprintln(w, "ok")
	})

	rs.srv = &http.Server{Handler: mux}
	go rs.srv.Serve(ln)

	return ln.Addr().String(), nil
}

func (rs *registrationServer) stop() {
	if rs.srv != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		rs.srv.Shutdown(ctx)
	}
}

// createToken generates a pending registration token for the given Telegram user.
func (rs *registrationServer) createToken(telegramUserID string, telegramUsername string, chatID int64) string {
	token := generateToken()

	rs.mu.Lock()
	defer rs.mu.Unlock()

	rs.cleanExpired()

	rs.pending[token] = &pendingRegistration{
		Token:            token,
		TelegramUserID:   telegramUserID,
		TelegramUsername: telegramUsername,
		ChatID:           chatID,
		ExpiresAt:        time.Now().Add(tokenTTL),
	}

	return token
}

// consumeToken validates and removes a pending registration, returning it if valid.
func (rs *registrationServer) consumeToken(token string) *pendingRegistration {
	rs.mu.Lock()
	defer rs.mu.Unlock()

	reg, ok := rs.pending[token]
	if !ok {
		return nil
	}
	if time.Now().After(reg.ExpiresAt) {
		delete(rs.pending, token)
		return nil
	}
	delete(rs.pending, token)
	return reg
}

// peekToken checks if a token is valid without consuming it.
func (rs *registrationServer) peekToken(token string) *pendingRegistration {
	rs.mu.Lock()
	defer rs.mu.Unlock()

	reg, ok := rs.pending[token]
	if !ok {
		return nil
	}
	if time.Now().After(reg.ExpiresAt) {
		delete(rs.pending, token)
		return nil
	}
	return reg
}

func (rs *registrationServer) cleanExpired() {
	now := time.Now()
	for token, reg := range rs.pending {
		if now.After(reg.ExpiresAt) {
			delete(rs.pending, token)
		}
	}
}

func (rs *registrationServer) handleRegister(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		rs.handleRegisterGet(w, r)
	case http.MethodPost:
		rs.handleRegisterPost(w, r)
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func (rs *registrationServer) handleRegisterGet(w http.ResponseWriter, r *http.Request) {
	token := r.URL.Query().Get("token")
	if token == "" {
		http.Error(w, "missing token parameter", http.StatusBadRequest)
		return
	}

	reg := rs.peekToken(token)
	if reg == nil {
		w.WriteHeader(http.StatusBadRequest)
		fmt.Fprintln(w, "Invalid or expired registration token. Please send /register again in Telegram.")
		return
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	registerFormTemplate.Execute(w, map[string]string{
		"Token":    token,
		"Username": reg.TelegramUsername,
	})
}

func (rs *registrationServer) handleRegisterPost(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form data", http.StatusBadRequest)
		return
	}

	token := r.FormValue("token")
	email := strings.TrimSpace(r.FormValue("email"))

	if token == "" || email == "" {
		http.Error(w, "missing token or email", http.StatusBadRequest)
		return
	}

	reg := rs.consumeToken(token)
	if reg == nil {
		w.WriteHeader(http.StatusBadRequest)
		fmt.Fprintln(w, "Invalid or expired registration token. Please send /register again in Telegram.")
		return
	}

	rs.broker.setUserMapping(reg.TelegramUserID, email)

	rs.broker.mu.RLock()
	mappingsFile := rs.broker.mappingsFile
	rs.broker.mu.RUnlock()
	if mappingsFile != "" {
		if err := rs.broker.saveUserMappings(mappingsFile); err != nil {
			rs.broker.log.Error("Failed to persist user mappings", "error", err)
		}
	}

	// Send confirmation to the Telegram chat
	go rs.sendConfirmation(reg, email)

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	registerSuccessTemplate.Execute(w, map[string]string{
		"Email": email,
	})
}

func (rs *registrationServer) sendConfirmation(reg *pendingRegistration, email string) {
	rs.broker.mu.RLock()
	api := rs.broker.api
	rs.broker.mu.RUnlock()

	if api == nil {
		return
	}

	text := fmt.Sprintf("Registration complete! Your Telegram account is now linked to scion user: %s", email)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := api.SendMessage(ctx, reg.ChatID, text, ""); err != nil {
		rs.broker.log.Error("Failed to send registration confirmation", "error", err, "chat_id", reg.ChatID)
	}
}

// setUserMapping stores a Telegram user ID → scion email mapping.
func (b *TelegramBroker) setUserMapping(telegramUserID, email string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.userMappings == nil {
		b.userMappings = make(map[string]string)
	}
	b.userMappings[telegramUserID] = email
}

// removeUserMapping deletes a Telegram user ID mapping.
func (b *TelegramBroker) removeUserMapping(telegramUserID string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	delete(b.userMappings, telegramUserID)
}

// getUserMapping returns the scion email for a Telegram user ID, if mapped.
func (b *TelegramBroker) getUserMapping(telegramUserID string) (string, bool) {
	b.mu.RLock()
	defer b.mu.RUnlock()
	if b.userMappings == nil {
		return "", false
	}
	email, ok := b.userMappings[telegramUserID]
	return email, ok
}

// saveUserMappings persists the current user mappings to a JSON file.
func (b *TelegramBroker) saveUserMappings(path string) error {
	b.mu.RLock()
	mappings := make(map[string]string, len(b.userMappings))
	for k, v := range b.userMappings {
		mappings[k] = v
	}
	b.mu.RUnlock()

	data, err := json.MarshalIndent(mappings, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal user mappings: %w", err)
	}
	if err := os.WriteFile(path, data, 0600); err != nil {
		return fmt.Errorf("write user mappings: %w", err)
	}
	return nil
}

// loadUserMappings loads user mappings from a JSON file, merging with existing mappings.
func (b *TelegramBroker) loadUserMappings(path string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("read user mappings: %w", err)
	}

	var loaded map[string]string
	if err := json.Unmarshal(data, &loaded); err != nil {
		return fmt.Errorf("parse user mappings file: %w", err)
	}

	b.mu.Lock()
	defer b.mu.Unlock()
	if b.userMappings == nil {
		b.userMappings = make(map[string]string)
	}
	for k, v := range loaded {
		if _, exists := b.userMappings[k]; !exists {
			b.userMappings[k] = v
		}
	}
	return nil
}

// handleBotCommand processes /register and /unregister commands. Returns true
// if the message was a recognized command and was handled.
func (b *TelegramBroker) handleBotCommand(tgMsg *TGMessage) bool {
	text := strings.TrimSpace(tgMsg.Text)

	// Strip bot mention suffix (e.g., "/register@mybot")
	cmd := text
	if idx := strings.Index(cmd, " "); idx != -1 {
		cmd = cmd[:idx]
	}
	if idx := strings.Index(cmd, "@"); idx != -1 {
		cmd = cmd[:idx]
	}

	switch cmd {
	case "/register":
		b.handleRegisterCommand(tgMsg)
		return true
	case "/unregister":
		b.handleUnregisterCommand(tgMsg)
		return true
	default:
		return false
	}
}

func (b *TelegramBroker) handleRegisterCommand(tgMsg *TGMessage) {
	if tgMsg.From == nil {
		return
	}

	senderID := strconv.FormatInt(tgMsg.From.ID, 10)

	// Check if already registered
	if email, ok := b.getUserMapping(senderID); ok {
		b.replyToChat(tgMsg.Chat.ID, fmt.Sprintf("You are already linked to scion user: %s\nTo unlink, send /unregister", email))
		return
	}

	b.mu.RLock()
	regServer := b.regServer
	registerURL := b.registerURL
	b.mu.RUnlock()

	if regServer == nil {
		b.replyToChat(tgMsg.Chat.ID, "Registration is not available. The registration server is not configured.")
		return
	}

	username := tgMsg.From.Username
	if username == "" {
		username = tgMsg.From.FirstName
	}

	token := regServer.createToken(senderID, username, tgMsg.Chat.ID)
	link := fmt.Sprintf("%s/register?token=%s", registerURL, token)

	b.replyToChat(tgMsg.Chat.ID,
		fmt.Sprintf("Click here to link your Telegram account to scion:\n%s\n\nThis link expires in 10 minutes.", link))
}

func (b *TelegramBroker) handleUnregisterCommand(tgMsg *TGMessage) {
	if tgMsg.From == nil {
		return
	}

	senderID := strconv.FormatInt(tgMsg.From.ID, 10)

	email, ok := b.getUserMapping(senderID)
	if !ok {
		b.replyToChat(tgMsg.Chat.ID, "You don't have a linked scion account. Send /register to link one.")
		return
	}

	b.removeUserMapping(senderID)

	b.mu.RLock()
	mappingsFile := b.mappingsFile
	b.mu.RUnlock()
	if mappingsFile != "" {
		if err := b.saveUserMappings(mappingsFile); err != nil {
			b.log.Error("Failed to persist user mappings after unregister", "error", err)
		}
	}

	b.replyToChat(tgMsg.Chat.ID, fmt.Sprintf("Your account has been unlinked from scion user: %s", email))
}

// replyToChat sends a text message to a Telegram chat.
func (b *TelegramBroker) replyToChat(chatID int64, text string) {
	b.mu.RLock()
	api := b.api
	b.mu.RUnlock()

	if api == nil {
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := api.SendMessage(ctx, chatID, text, ""); err != nil {
		b.log.Error("Failed to send reply", "error", err, "chat_id", chatID)
	}
}

func generateToken() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		panic(fmt.Sprintf("crypto/rand failed: %v", err))
	}
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

var registerFormTemplate = template.Must(template.New("form").Parse(`<!DOCTYPE html>
<html>
<head><title>Link Telegram to Scion</title>
<style>
body { font-family: sans-serif; max-width: 480px; margin: 40px auto; padding: 0 20px; }
h1 { color: #333; }
label { display: block; margin-top: 16px; font-weight: bold; }
input[type=email] { width: 100%; padding: 8px; margin-top: 4px; box-sizing: border-box; }
button { margin-top: 16px; padding: 10px 24px; background: #0088cc; color: white; border: none; border-radius: 4px; cursor: pointer; font-size: 16px; }
button:hover { background: #006da3; }
.info { color: #666; margin-top: 8px; font-size: 14px; }
</style>
</head>
<body>
<h1>Link Telegram to Scion</h1>
<p>Linking Telegram user <strong>{{.Username}}</strong> to your scion account.</p>
<form method="POST" action="/register">
<input type="hidden" name="token" value="{{.Token}}">
<label for="email">Scion account email:</label>
<input type="email" id="email" name="email" required placeholder="you@example.com">
<p class="info">Enter the email address associated with your scion hub account.</p>
<button type="submit">Link Account</button>
</form>
</body>
</html>`))

var registerSuccessTemplate = template.Must(template.New("success").Parse(`<!DOCTYPE html>
<html>
<head><title>Registration Complete</title>
<style>
body { font-family: sans-serif; max-width: 480px; margin: 40px auto; padding: 0 20px; }
h1 { color: #2e7d32; }
</style>
</head>
<body>
<h1>Registration Complete</h1>
<p>Your Telegram account is now linked to scion user: <strong>{{.Email}}</strong></p>
<p>You can close this page and return to Telegram.</p>
</body>
</html>`))
