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
	"encoding/json"
	"fmt"
	"log/slog"
	"math/big"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/apiclient"
)

// RegistrationHandler manages the hub-verified code-based registration flow.
type RegistrationHandler struct {
	store      Store
	api        *TelegramAPIClient
	hubURL     string
	hmacKey    string
	brokerID   string
	httpClient *http.Client
	log        *slog.Logger

	mu      sync.Mutex
	pending map[string]*pendingLinkReg // telegramUserID → pending registration
}

// pendingLinkReg holds state for an in-progress hub-based linking registration.
type pendingLinkReg struct {
	Code             string
	TelegramUserID   string
	TelegramUsername string // captured at /register time for display
	ChatID           int64
	ExpiresAt        time.Time
	pollCancel       context.CancelFunc
}

// linkingCodeRequest is the JSON body sent to the hub to register a linking code.
type linkingCodeRequest struct {
	Code           string `json:"code"`
	TelegramUserID string `json:"telegramUserId"`
}

// linkingStatusResponse is the JSON response from checking a linking status.
type linkingStatusResponse struct {
	Status string       `json:"status"` // "pending", "confirmed", "expired", "not_found"
	User   *linkingUser `json:"user,omitempty"`
}

// linkingUser holds user info returned by the hub when a linking code is confirmed.
type linkingUser struct {
	ID    string `json:"id"`
	Email string `json:"email"`
}

const (
	linkingCodeExpiry   = 15 * time.Minute
	linkingPollInterval = 10 * time.Second
	linkingCodeCharset  = "ABCDEFGHJKMNPQRSTUVWXYZ23456789"
	linkingCodeLength   = 6
)

// NewRegistrationHandler creates a new RegistrationHandler.
func NewRegistrationHandler(store Store, api *TelegramAPIClient, hubURL, hmacKey, brokerID string, log *slog.Logger) *RegistrationHandler {
	if log == nil {
		log = slog.Default()
	}
	return &RegistrationHandler{
		store:      store,
		api:        api,
		hubURL:     hubURL,
		hmacKey:    hmacKey,
		brokerID:   brokerID,
		httpClient: &http.Client{Timeout: 15 * time.Second},
		log:        log,
		pending:    make(map[string]*pendingLinkReg),
	}
}

// HandleRegister handles the /register command. It generates a short
// linking code, registers it with the hub, and sends the user instructions.
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

	code, err := generateLinkingCode()
	if err != nil {
		h.log.Error("Failed to generate linking code", "error", err)
		h.sendReply(chatID, "Something went wrong. Please try again.")
		return
	}

	if err := h.registerCodeWithHub(ctx, code, telegramUserID); err != nil {
		h.log.Error("Failed to register linking code with hub", "error", err)
		h.sendReply(chatID, "Failed to start registration. Please try again later.")
		return
	}

	// Cancel any existing pending registration for this user.
	h.mu.Lock()
	h.cleanExpiredLocked()
	if old, ok := h.pending[telegramUserID]; ok && old.pollCancel != nil {
		old.pollCancel()
	}

	pollCtx, pollCancel := context.WithCancel(context.Background())
	tgUsername := ""
	if msg.From != nil {
		tgUsername = msg.From.Username
	}
	reg := &pendingLinkReg{
		Code:             code,
		TelegramUserID:   telegramUserID,
		TelegramUsername: tgUsername,
		ChatID:           chatID,
		ExpiresAt:        time.Now().Add(linkingCodeExpiry),
		pollCancel:       pollCancel,
	}
	h.pending[telegramUserID] = reg
	h.mu.Unlock()

	hubLink := fmt.Sprintf("%s/profile/telegram?code=%s", strings.TrimRight(h.hubURL, "/"), code)

	text := "To link your Telegram and Scion accounts, tap and hold the button below and choose \"Open in ...\" your authenticated browser.\n\n" +
		"(Link expires in 15 minutes.)"

	keyboard := &InlineKeyboardMarkup{
		InlineKeyboard: [][]InlineKeyboardButton{
			{
				{Text: "Link Telegram account", URL: hubLink},
			},
		},
	}

	if _, err := h.api.SendMessageWithKeyboard(ctx, chatID, text, "Markdown", keyboard, 0); err != nil {
		h.log.Error("Failed to send registration card", "error", err, "chat_id", chatID)
	}

	go h.pollForConfirmation(pollCtx, reg)
}

// HandleRegisterConfirm handles /register confirm as a manual fallback.
func (h *RegistrationHandler) HandleRegisterConfirm(msg *TGMessage) {
	if msg.From == nil {
		return
	}

	chatID := msg.Chat.ID
	telegramUserID := strconv.FormatInt(msg.From.ID, 10)

	h.mu.Lock()
	reg, ok := h.pending[telegramUserID]
	if ok && time.Now().After(reg.ExpiresAt) {
		if reg.pollCancel != nil {
			reg.pollCancel()
		}
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

	statusResp, err := h.checkLinkingStatus(ctx, telegramUserID)
	if err != nil {
		h.log.Error("Failed to check linking status", "error", err)
		h.sendReply(chatID, "Something went wrong checking your registration. Please try again.")
		return
	}

	switch statusResp.Status {
	case "pending":
		h.sendReply(chatID, "Not yet confirmed. Please open the hub and enter your code, then try again.")
		return
	case "expired", "not_found":
		h.mu.Lock()
		if old, ok := h.pending[telegramUserID]; ok && old.pollCancel != nil {
			old.pollCancel()
		}
		delete(h.pending, telegramUserID)
		h.mu.Unlock()
		h.sendReply(chatID, "Code expired. Run /register again.")
		return
	case "confirmed":
		// Continue below.
	default:
		h.log.Warn("Unknown linking status", "status", statusResp.Status)
		h.sendReply(chatID, "Unexpected status. Please try again.")
		return
	}

	h.completeRegistration(msg, reg, statusResp)
}

// HandleUnregister handles the /unregister command.
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

// pollForConfirmation polls the hub for confirmation status in the background.
func (h *RegistrationHandler) pollForConfirmation(ctx context.Context, reg *pendingLinkReg) {
	ticker := time.NewTicker(linkingPollInterval)
	defer ticker.Stop()

	deadline := reg.ExpiresAt
	for {
		select {
		case <-ctx.Done():
			return
		case t := <-ticker.C:
			if t.After(deadline) {
				h.mu.Lock()
				if cur, ok := h.pending[reg.TelegramUserID]; ok && cur.Code == reg.Code {
					delete(h.pending, reg.TelegramUserID)
				}
				h.mu.Unlock()
				return
			}

			checkCtx, checkCancel := context.WithTimeout(ctx, 10*time.Second)
			statusResp, err := h.checkLinkingStatus(checkCtx, reg.TelegramUserID)
			checkCancel()

			if err != nil {
				h.log.Debug("Poll check failed", "error", err, "telegram_user_id", reg.TelegramUserID)
				continue
			}

			if statusResp.Status == "confirmed" && statusResp.User != nil {
				h.completeRegistrationFromPoll(reg, statusResp)
				return
			}
		}
	}
}

// completeRegistration saves the user mapping and notifies the user (manual confirm path).
func (h *RegistrationHandler) completeRegistration(msg *TGMessage, reg *pendingLinkReg, statusResp *linkingStatusResponse) {
	if statusResp.User == nil {
		h.log.Error("Linking status confirmed but missing user info")
		h.sendReply(reg.ChatID, "Registration failed: could not retrieve user info. Please try again.")
		return
	}

	username := ""
	if msg.From != nil {
		if msg.From.Username != "" {
			username = msg.From.Username
		} else if msg.From.FirstName != "" {
			username = msg.From.FirstName
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	mapping := &TelegramUserMapping{
		TelegramUserID:   reg.TelegramUserID,
		TelegramUsername: username,
		ScionUserID:      statusResp.User.ID,
		ScionEmail:       statusResp.User.Email,
		LinkedAt:         time.Now(),
	}

	if err := h.store.SaveUserMapping(ctx, mapping); err != nil {
		h.log.Error("Failed to save user mapping", "error", err, "telegram_user_id", reg.TelegramUserID)
		h.sendReply(reg.ChatID, "Failed to save registration. Please try again.")
		return
	}

	h.mu.Lock()
	if reg.pollCancel != nil {
		reg.pollCancel()
	}
	delete(h.pending, reg.TelegramUserID)
	h.mu.Unlock()

	h.sendReply(reg.ChatID, fmt.Sprintf("Linked! You are %s", statusResp.User.Email))
	h.log.Info("User registered via hub linking",
		"telegram_user_id", reg.TelegramUserID,
		"scion_email", statusResp.User.Email,
		"scion_user_id", statusResp.User.ID,
	)
}

// completeRegistrationFromPoll saves the user mapping when confirmed via background polling.
func (h *RegistrationHandler) completeRegistrationFromPoll(reg *pendingLinkReg, statusResp *linkingStatusResponse) {
	if statusResp.User == nil {
		h.log.Error("Poll confirmed but missing user info", "telegram_user_id", reg.TelegramUserID)
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	mapping := &TelegramUserMapping{
		TelegramUserID:   reg.TelegramUserID,
		TelegramUsername: reg.TelegramUsername, // captured when user sent /register
		ScionUserID:      statusResp.User.ID,
		ScionEmail:       statusResp.User.Email,
		LinkedAt:         time.Now(),
	}

	if err := h.store.SaveUserMapping(ctx, mapping); err != nil {
		h.log.Error("Failed to save user mapping from poll", "error", err, "telegram_user_id", reg.TelegramUserID)
		return
	}

	h.mu.Lock()
	delete(h.pending, reg.TelegramUserID)
	h.mu.Unlock()

	h.sendReply(reg.ChatID, fmt.Sprintf("Linked! You are %s", statusResp.User.Email))
	h.log.Info("User registered via hub linking (auto-detected)",
		"telegram_user_id", reg.TelegramUserID,
		"scion_email", statusResp.User.Email,
		"scion_user_id", statusResp.User.ID,
	)
}

// registerCodeWithHub POSTs a linking code to the hub for registration.
func (h *RegistrationHandler) registerCodeWithHub(ctx context.Context, code, telegramUserID string) error {
	body, err := json.Marshal(linkingCodeRequest{
		Code:           code,
		TelegramUserID: telegramUserID,
	})
	if err != nil {
		return fmt.Errorf("marshal linking code request: %w", err)
	}

	url := h.hubURL + "/api/v1/telegram/link"
	req, err := http.NewRequestWithContext(ctx, "POST", url, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("create linking code request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if h.hmacKey != "" && h.brokerID != "" {
		if err := signBrokerRequest(req, h.brokerID, h.hmacKey); err != nil {
			return fmt.Errorf("sign linking code request: %w", err)
		}
	}

	resp, err := h.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("linking code request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated {
		return fmt.Errorf("linking code endpoint returned status %d", resp.StatusCode)
	}

	return nil
}

// checkLinkingStatus checks with the hub whether a linking code was confirmed.
func (h *RegistrationHandler) checkLinkingStatus(ctx context.Context, telegramUserID string) (*linkingStatusResponse, error) {
	url := fmt.Sprintf("%s/api/v1/telegram/link/status?telegram_user_id=%s", h.hubURL, telegramUserID)
	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return nil, fmt.Errorf("create linking status request: %w", err)
	}
	if h.hmacKey != "" && h.brokerID != "" {
		if err := signBrokerRequest(req, h.brokerID, h.hmacKey); err != nil {
			return nil, fmt.Errorf("sign linking status request: %w", err)
		}
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
			if reg.pollCancel != nil {
				reg.pollCancel()
			}
			delete(h.pending, id)
		}
	}
}

// generateLinkingCode creates a 6-character alphanumeric code using a
// charset that avoids ambiguous characters (0/O, 1/I/L).
func generateLinkingCode() (string, error) {
	result := make([]byte, linkingCodeLength)
	for i := range result {
		n, err := rand.Int(rand.Reader, big.NewInt(int64(len(linkingCodeCharset))))
		if err != nil {
			return "", fmt.Errorf("generate random char: %w", err)
		}
		result[i] = linkingCodeCharset[n.Int64()]
	}
	return string(result), nil
}

// signBrokerRequest signs an HTTP request with HMAC broker credentials.
func signBrokerRequest(req *http.Request, brokerID, hmacKey string) error {
	secretKey, err := decodeBase64(hmacKey)
	if err != nil {
		return fmt.Errorf("decode HMAC key: %w", err)
	}
	auth := &apiclient.HMACAuth{
		BrokerID:  brokerID,
		SecretKey: secretKey,
	}
	return auth.ApplyAuth(req)
}
