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
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

// RegistrationHandler manages the hub-verified token-based registration flow.
type RegistrationHandler struct {
	store      Store
	api        *TelegramAPIClient
	hubURL     string
	httpClient *http.Client
	log        *slog.Logger

	mu      sync.Mutex
	pending map[string]*pendingLinkReg // telegramUserID → pending registration
}

// pendingLinkReg holds state for an in-progress hub-based linking registration.
type pendingLinkReg struct {
	Token          string
	TelegramUserID string
	ChatID         int64
	ExpiresAt      time.Time
}

// linkingTokenRequest is the JSON body sent to the hub to register a linking token.
type linkingTokenRequest struct {
	Token          string `json:"token"`
	TelegramUserID string `json:"telegramUserId"`
}

// linkingStatusResponse is the JSON response from checking a linking token's status.
type linkingStatusResponse struct {
	Status string       `json:"status"` // "pending", "confirmed", "expired"
	User   *linkingUser `json:"user,omitempty"`
}

// linkingUser holds user info returned by the hub when a linking token is confirmed.
type linkingUser struct {
	ID    string `json:"id"`
	Email string `json:"email"`
}

const linkingTokenExpiry = 10 * time.Minute

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
		pending:    make(map[string]*pendingLinkReg),
	}
}

// HandleRegister handles the /register command. It generates a one-time
// linking token, registers it with the hub, and sends the user a link to
// the hub where they can confirm their identity.
func (h *RegistrationHandler) HandleRegister(msg *TGMessage) {
	if msg.From == nil {
		return
	}

	chatID := msg.Chat.ID
	telegramUserID := strconv.FormatInt(msg.From.ID, 10)

	if chatID < 0 {
		h.sendReply(chatID, "Please DM me to register. This command only works in a direct message.")
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

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

	token := generateLinkingToken()

	if err := h.registerTokenWithHub(ctx, token, telegramUserID); err != nil {
		h.log.Error("Failed to register linking token with hub", "error", err)
		h.sendReply(chatID, "Failed to start registration. Please try again later.")
		return
	}

	h.mu.Lock()
	h.cleanExpiredLocked()
	h.pending[telegramUserID] = &pendingLinkReg{
		Token:          token,
		TelegramUserID: telegramUserID,
		ChatID:         chatID,
		ExpiresAt:      time.Now().Add(linkingTokenExpiry),
	}
	h.mu.Unlock()

	linkURL := fmt.Sprintf("%s/telegram/register?token=%s", strings.TrimRight(h.hubURL, "/"), token)

	text := fmt.Sprintf(
		"Link your scion account\n\n"+
			"1. Open the link below (you must be logged into the hub)\n"+
			"2. Confirm the linking on the hub page\n\n"+
			"Then send: /register confirm",
	)

	keyboard := &InlineKeyboardMarkup{
		InlineKeyboard: [][]InlineKeyboardButton{
			{
				{Text: "Link account on hub", URL: linkURL},
			},
		},
	}

	if _, err := h.api.SendMessageWithKeyboard(ctx, chatID, text, "", keyboard, 0); err != nil {
		h.log.Error("Failed to send registration card", "error", err, "chat_id", chatID)
	}
}

// HandleRegisterConfirm handles /register confirm. It checks with the hub
// whether the linking token was confirmed and, on success, stores the user mapping.
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

	statusResp, err := h.checkLinkingStatus(ctx, reg.Token)
	if err != nil {
		h.log.Error("Failed to check linking status", "error", err)
		h.sendReply(chatID, "Something went wrong checking your registration. Please try again.")
		return
	}

	switch statusResp.Status {
	case "pending":
		h.sendReply(chatID, "Not yet confirmed. Please open the link and confirm on the hub, then try again.")
		return
	case "expired":
		h.mu.Lock()
		delete(h.pending, telegramUserID)
		h.mu.Unlock()
		h.sendReply(chatID, "Token expired. Run /register again.")
		return
	case "confirmed":
		// Continue below.
	default:
		h.log.Warn("Unknown linking status", "status", statusResp.Status)
		h.sendReply(chatID, "Unexpected status. Please try again.")
		return
	}

	if statusResp.User == nil {
		h.log.Error("Linking status confirmed but missing user info")
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
		ScionUserID:      statusResp.User.ID,
		ScionEmail:       statusResp.User.Email,
		LinkedAt:         time.Now(),
	}

	if err := h.store.SaveUserMapping(ctx, mapping); err != nil {
		h.log.Error("Failed to save user mapping", "error", err, "telegram_user_id", telegramUserID)
		h.sendReply(chatID, "Failed to save registration. Please try again.")
		return
	}

	h.mu.Lock()
	delete(h.pending, telegramUserID)
	h.mu.Unlock()

	h.sendReply(chatID, fmt.Sprintf("Linked! You are %s", statusResp.User.Email))
	h.log.Info("User registered via hub linking",
		"telegram_user_id", telegramUserID,
		"scion_email", statusResp.User.Email,
		"scion_user_id", statusResp.User.ID,
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

// registerTokenWithHub POSTs a linking token to the hub for registration.
func (h *RegistrationHandler) registerTokenWithHub(ctx context.Context, token, telegramUserID string) error {
	body, err := json.Marshal(linkingTokenRequest{
		Token:          token,
		TelegramUserID: telegramUserID,
	})
	if err != nil {
		return fmt.Errorf("marshal linking token request: %w", err)
	}

	url := h.hubURL + "/api/v1/telegram/link"
	req, err := http.NewRequestWithContext(ctx, "POST", url, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("create linking token request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := h.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("linking token request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated {
		return fmt.Errorf("linking token endpoint returned status %d", resp.StatusCode)
	}

	return nil
}

// checkLinkingStatus checks with the hub whether a linking token was confirmed.
func (h *RegistrationHandler) checkLinkingStatus(ctx context.Context, token string) (*linkingStatusResponse, error) {
	url := fmt.Sprintf("%s/api/v1/telegram/link/%s", h.hubURL, token)
	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return nil, fmt.Errorf("create linking status request: %w", err)
	}

	resp, err := h.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("linking status request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("linking status endpoint returned status %d", resp.StatusCode)
	}

	var statusResp linkingStatusResponse
	if err := json.NewDecoder(resp.Body).Decode(&statusResp); err != nil {
		return nil, fmt.Errorf("decode linking status response: %w", err)
	}
	return &statusResp, nil
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

// generateLinkingToken creates a cryptographically random hex token.
func generateLinkingToken() string {
	b := make([]byte, 16)
	rand.Read(b)
	return hex.EncodeToString(b)
}
