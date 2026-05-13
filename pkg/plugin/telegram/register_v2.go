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
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"sync"
	"time"
)

// RegistrationHandler manages the hub-verified device auth registration flow.
type RegistrationHandler struct {
	store      Store
	api        *TelegramAPIClient
	hubURL     string
	httpClient *http.Client
	log        *slog.Logger

	mu      sync.Mutex
	pending map[string]*pendingDeviceReg // telegramUserID → pending registration
}

// pendingDeviceReg holds state for an in-progress device auth registration.
type pendingDeviceReg struct {
	DeviceCode      string
	UserCode        string
	VerificationURL string
	TelegramUserID  string
	ChatID          int64
	ExpiresAt       time.Time
}

// deviceCodeRequest is the JSON body sent to the hub device code endpoint.
type deviceCodeRequest struct {
	Provider string `json:"provider,omitempty"`
}

// deviceCodeResponse is the JSON response from the hub device code endpoint.
type deviceCodeResponse struct {
	DeviceCode              string `json:"deviceCode"`
	UserCode                string `json:"userCode"`
	VerificationURL         string `json:"verificationUrl"`
	VerificationURLComplete string `json:"verificationUrlComplete,omitempty"`
	ExpiresIn               int    `json:"expiresIn"`
	Interval                int    `json:"interval"`
}

// deviceTokenRequest is the JSON body sent to the hub device token endpoint.
type deviceTokenRequest struct {
	DeviceCode string `json:"deviceCode"`
	Provider   string `json:"provider,omitempty"`
}

// deviceTokenResponse is the JSON response from the hub device token endpoint.
type deviceTokenResponse struct {
	AccessToken  string      `json:"accessToken,omitempty"`
	RefreshToken string      `json:"refreshToken,omitempty"`
	ExpiresIn    int64       `json:"expiresIn,omitempty"`
	User         *deviceUser `json:"user,omitempty"`
}

// deviceTokenErrorResponse is returned as the HTTP status code on non-200 responses.
// 202 = authorization_pending, 410 = expired_token, 429 = slow_down.
type deviceUser struct {
	ID    string `json:"id"`
	Email string `json:"email"`
}

// NewRegistrationHandler creates a new RegistrationHandler.
func NewRegistrationHandler(store Store, api *TelegramAPIClient, hubURL string, log *slog.Logger) *RegistrationHandler {
	if log == nil {
		log = slog.Default()
	}
	return &RegistrationHandler{
		store:      store,
		api:        api,
		hubURL:     hubURL,
		httpClient: &http.Client{Timeout: 15 * time.Second},
		log:        log,
		pending:    make(map[string]*pendingDeviceReg),
	}
}

// HandleRegister handles the /register command. It initiates the device auth
// flow by requesting a device code from the hub and sending the user an inline
// keyboard card with a verification URL button.
func (h *RegistrationHandler) HandleRegister(msg *TGMessage) {
	if msg.From == nil {
		return
	}

	chatID := msg.Chat.ID
	telegramUserID := strconv.FormatInt(msg.From.ID, 10)

	// Must be in DM (positive chat ID).
	if chatID < 0 {
		h.sendReply(chatID, "Please DM me to register. This command only works in a direct message.")
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	// Check if already registered.
	existing, err := h.store.GetUserMapping(ctx, telegramUserID)
	if err != nil {
		h.log.Error("Failed to check user mapping", "error", err, "telegram_user_id", telegramUserID)
		h.sendReply(chatID, "Something went wrong. Please try again.")
		return
	}
	if existing != nil {
		h.sendReply(chatID, fmt.Sprintf(
			"You are already registered as %s. Use /unregister first.",
			existing.ScionEmail,
		))
		return
	}

	// Request device code from hub.
	codeResp, err := h.requestDeviceCode(ctx)
	if err != nil {
		h.log.Error("Failed to request device code", "error", err)
		h.sendReply(chatID, "Failed to start registration. Please try again later.")
		return
	}

	// Store pending registration.
	h.mu.Lock()
	h.cleanExpiredLocked()
	h.pending[telegramUserID] = &pendingDeviceReg{
		DeviceCode:      codeResp.DeviceCode,
		UserCode:        codeResp.UserCode,
		VerificationURL: codeResp.VerificationURL,
		TelegramUserID:  telegramUserID,
		ChatID:          chatID,
		ExpiresAt:       time.Now().Add(time.Duration(codeResp.ExpiresIn) * time.Second),
	}
	h.mu.Unlock()

	// Use the complete URL if available, otherwise fall back to the base URL.
	verifyURL := codeResp.VerificationURLComplete
	if verifyURL == "" {
		verifyURL = codeResp.VerificationURL
	}

	text := fmt.Sprintf(
		"Link your scion account\n\n"+
			"1. Open the verification link below\n"+
			"2. Enter code: %s\n\n"+
			"Then send: /register confirm",
		codeResp.UserCode,
	)

	keyboard := &InlineKeyboardMarkup{
		InlineKeyboard: [][]InlineKeyboardButton{
			{
				{Text: "Open verification link", URL: verifyURL},
			},
		},
	}

	if _, err := h.api.SendMessageWithKeyboard(ctx, chatID, text, "", keyboard, 0); err != nil {
		h.log.Error("Failed to send registration card", "error", err, "chat_id", chatID)
	}
}

// HandleRegisterConfirm handles /register confirm. It polls the hub for the
// device token and, on success, stores the user mapping.
func (h *RegistrationHandler) HandleRegisterConfirm(msg *TGMessage) {
	if msg.From == nil {
		return
	}

	chatID := msg.Chat.ID
	telegramUserID := strconv.FormatInt(msg.From.ID, 10)

	h.mu.Lock()
	reg, ok := h.pending[telegramUserID]
	if ok && time.Now().After(reg.ExpiresAt) {
		delete(h.pending, telegramUserID)
		ok = false
	}
	h.mu.Unlock()

	if !ok {
		h.sendReply(chatID, "No pending registration. Run /register to start.")
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	// Poll hub for device token.
	tokenResp, status, err := h.pollDeviceToken(ctx, reg.DeviceCode)
	if err != nil {
		h.log.Error("Failed to poll device token", "error", err)
		h.sendReply(chatID, "Something went wrong checking your authorization. Please try again.")
		return
	}

	switch status {
	case "authorization_pending":
		h.sendReply(chatID, "Not yet authorized. Please complete the verification step and try again.")
		return
	case "expired_token":
		h.mu.Lock()
		delete(h.pending, telegramUserID)
		h.mu.Unlock()
		h.sendReply(chatID, "Code expired. Run /register again.")
		return
	case "slow_down":
		h.sendReply(chatID, "Too many attempts. Please wait a moment and try again.")
		return
	}

	// Success — extract user info and store mapping.
	if tokenResp.User == nil {
		h.log.Error("Device token response missing user info")
		h.sendReply(chatID, "Registration failed: could not retrieve user info. Please try again.")
		return
	}

	username := ""
	if msg.From.Username != "" {
		username = msg.From.Username
	} else if msg.From.FirstName != "" {
		username = msg.From.FirstName
	}

	mapping := &TelegramUserMapping{
		TelegramUserID:   telegramUserID,
		TelegramUsername: username,
		ScionUserID:      tokenResp.User.ID,
		ScionEmail:       tokenResp.User.Email,
		LinkedAt:         time.Now(),
	}

	if err := h.store.SaveUserMapping(ctx, mapping); err != nil {
		h.log.Error("Failed to save user mapping", "error", err, "telegram_user_id", telegramUserID)
		h.sendReply(chatID, "Failed to save registration. Please try again.")
		return
	}

	// Clean up pending registration.
	h.mu.Lock()
	delete(h.pending, telegramUserID)
	h.mu.Unlock()

	h.sendReply(chatID, fmt.Sprintf("Linked! You are %s", tokenResp.User.Email))
	h.log.Info("User registered via device auth",
		"telegram_user_id", telegramUserID,
		"scion_email", tokenResp.User.Email,
		"scion_user_id", tokenResp.User.ID,
	)
}

// HandleUnregister handles the /unregister command. It removes the user's
// Telegram-to-scion identity mapping.
func (h *RegistrationHandler) HandleUnregister(msg *TGMessage) {
	if msg.From == nil {
		return
	}

	chatID := msg.Chat.ID
	telegramUserID := strconv.FormatInt(msg.From.ID, 10)

	// Must be in DM.
	if chatID < 0 {
		h.sendReply(chatID, "Please DM me to unregister. This command only works in a direct message.")
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	existing, err := h.store.GetUserMapping(ctx, telegramUserID)
	if err != nil {
		h.log.Error("Failed to check user mapping", "error", err, "telegram_user_id", telegramUserID)
		h.sendReply(chatID, "Something went wrong. Please try again.")
		return
	}
	if existing == nil {
		h.sendReply(chatID, "You don't have a linked scion account. Send /register to link one.")
		return
	}

	if err := h.store.DeleteUserMapping(ctx, telegramUserID); err != nil {
		h.log.Error("Failed to delete user mapping", "error", err, "telegram_user_id", telegramUserID)
		h.sendReply(chatID, "Failed to unlink your account. Please try again.")
		return
	}

	h.sendReply(chatID, "Your Telegram account has been unlinked.")
	h.log.Info("User unregistered",
		"telegram_user_id", telegramUserID,
		"scion_email", existing.ScionEmail,
	)
}

// ImportV1Mappings imports v1-format user mappings (telegramID → email) into the v2 store.
func (h *RegistrationHandler) ImportV1Mappings(ctx context.Context, mappings map[string]string) error {
	var firstErr error
	imported := 0
	for telegramUserID, email := range mappings {
		existing, err := h.store.GetUserMapping(ctx, telegramUserID)
		if err != nil {
			h.log.Error("Failed to check existing mapping during v1 import",
				"error", err, "telegram_user_id", telegramUserID)
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		if existing != nil {
			h.log.Debug("Skipping v1 import for already-mapped user",
				"telegram_user_id", telegramUserID, "existing_email", existing.ScionEmail)
			continue
		}

		mapping := &TelegramUserMapping{
			TelegramUserID: telegramUserID,
			ScionEmail:     email,
			LinkedAt:       time.Now(),
		}
		if err := h.store.SaveUserMapping(ctx, mapping); err != nil {
			h.log.Error("Failed to import v1 mapping",
				"error", err, "telegram_user_id", telegramUserID, "email", email)
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		imported++
	}

	h.log.Info("V1 mapping import complete", "imported", imported, "total", len(mappings))
	return firstErr
}

// requestDeviceCode calls the hub's device authorization endpoint.
func (h *RegistrationHandler) requestDeviceCode(ctx context.Context) (*deviceCodeResponse, error) {
	body, err := json.Marshal(deviceCodeRequest{})
	if err != nil {
		return nil, fmt.Errorf("marshal device code request: %w", err)
	}

	url := h.hubURL + "/api/v1/auth/cli/device"
	req, err := http.NewRequestWithContext(ctx, "POST", url, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("create device code request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := h.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("device code request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("device code endpoint returned status %d", resp.StatusCode)
	}

	var codeResp deviceCodeResponse
	if err := json.NewDecoder(resp.Body).Decode(&codeResp); err != nil {
		return nil, fmt.Errorf("decode device code response: %w", err)
	}
	return &codeResp, nil
}

// pollDeviceToken polls the hub for the device token result.
// Returns (response, status, error). Status is empty string on success,
// or one of "authorization_pending", "expired_token", "slow_down".
func (h *RegistrationHandler) pollDeviceToken(ctx context.Context, deviceCode string) (*deviceTokenResponse, string, error) {
	body, err := json.Marshal(deviceTokenRequest{DeviceCode: deviceCode})
	if err != nil {
		return nil, "", fmt.Errorf("marshal device token request: %w", err)
	}

	url := h.hubURL + "/api/v1/auth/cli/device/token"
	req, err := http.NewRequestWithContext(ctx, "POST", url, bytes.NewReader(body))
	if err != nil {
		return nil, "", fmt.Errorf("create device token request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := h.httpClient.Do(req)
	if err != nil {
		return nil, "", fmt.Errorf("device token request failed: %w", err)
	}
	defer resp.Body.Close()

	switch resp.StatusCode {
	case http.StatusOK:
		var tokenResp deviceTokenResponse
		if err := json.NewDecoder(resp.Body).Decode(&tokenResp); err != nil {
			return nil, "", fmt.Errorf("decode device token response: %w", err)
		}
		return &tokenResp, "", nil
	case http.StatusAccepted: // 202
		return nil, "authorization_pending", nil
	case http.StatusGone: // 410
		return nil, "expired_token", nil
	case http.StatusTooManyRequests: // 429
		return nil, "slow_down", nil
	default:
		return nil, "", fmt.Errorf("device token endpoint returned status %d", resp.StatusCode)
	}
}

func (h *RegistrationHandler) sendReply(chatID int64, text string) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := h.api.SendMessage(ctx, chatID, text, ""); err != nil {
		h.log.Error("Failed to send reply", "error", err, "chat_id", chatID)
	}
}

func (h *RegistrationHandler) cleanExpiredLocked() {
	now := time.Now()
	for id, reg := range h.pending {
		if now.After(reg.ExpiresAt) {
			delete(h.pending, id)
		}
	}
}
