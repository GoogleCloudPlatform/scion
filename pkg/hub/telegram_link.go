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

package hub

import (
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"
)

const telegramLinkCodeTTL = 15 * time.Minute

// telegramPendingLink holds state for a pending Telegram account linking.
type telegramPendingLink struct {
	Code           string
	TelegramUserID string
	ExpiresAt      time.Time
	Status         string // "pending", "confirmed"
	UserID         string
	UserEmail      string
}

// TelegramLinkService manages pending Telegram account link codes.
type TelegramLinkService struct {
	mu      sync.Mutex
	pending map[string]*telegramPendingLink // code → pending link
	done    chan struct{}
}

// NewTelegramLinkService creates a new TelegramLinkService and starts
// a background goroutine that periodically removes expired entries.
func NewTelegramLinkService() *TelegramLinkService {
	s := &TelegramLinkService{
		pending: make(map[string]*telegramPendingLink),
		done:    make(chan struct{}),
	}
	go s.cleanupLoop()
	return s
}

// RegisterCode stores a pending link code from the Telegram plugin.
func (s *TelegramLinkService) RegisterCode(code, telegramUserID string) {
	s.mu.Lock()
	defer s.mu.Unlock()

	// Remove any existing pending code for this telegram user.
	for c, p := range s.pending {
		if p.TelegramUserID == telegramUserID {
			delete(s.pending, c)
		}
	}

	s.pending[strings.ToUpper(code)] = &telegramPendingLink{
		Code:           strings.ToUpper(code),
		TelegramUserID: telegramUserID,
		ExpiresAt:      time.Now().Add(telegramLinkCodeTTL),
		Status:         "pending",
	}
}

// VerifyCode attempts to confirm a pending link code with the given user.
// Returns the telegramUserID on success, or empty string with a reason.
func (s *TelegramLinkService) VerifyCode(code, userID, userEmail string) (telegramUserID string, err string) {
	s.mu.Lock()
	defer s.mu.Unlock()

	p, ok := s.pending[strings.ToUpper(code)]
	if !ok {
		return "", "code_not_found"
	}
	if time.Now().After(p.ExpiresAt) {
		delete(s.pending, strings.ToUpper(code))
		return "", "code_expired"
	}
	if p.Status == "confirmed" {
		return p.TelegramUserID, ""
	}

	p.Status = "confirmed"
	p.UserID = userID
	p.UserEmail = userEmail
	return p.TelegramUserID, ""
}

// GetStatusByTelegramUser returns the linking status for a given Telegram user ID.
func (s *TelegramLinkService) GetStatusByTelegramUser(telegramUserID string) (status, userID, userEmail string) {
	s.mu.Lock()
	defer s.mu.Unlock()

	for _, p := range s.pending {
		if p.TelegramUserID == telegramUserID {
			if time.Now().After(p.ExpiresAt) {
				return "expired", "", ""
			}
			return p.Status, p.UserID, p.UserEmail
		}
	}
	return "not_found", "", ""
}

// ConsumePending removes a confirmed entry so it isn't returned again.
func (s *TelegramLinkService) ConsumePending(telegramUserID string) {
	s.mu.Lock()
	defer s.mu.Unlock()

	for code, p := range s.pending {
		if p.TelegramUserID == telegramUserID {
			delete(s.pending, code)
			return
		}
	}
}

// Close stops the background cleanup goroutine.
func (s *TelegramLinkService) Close() {
	close(s.done)
}

func (s *TelegramLinkService) cleanupLoop() {
	ticker := time.NewTicker(1 * time.Minute)
	defer ticker.Stop()

	for {
		select {
		case <-s.done:
			return
		case <-ticker.C:
			s.mu.Lock()
			now := time.Now()
			for code, p := range s.pending {
				if now.After(p.ExpiresAt) {
					delete(s.pending, code)
				}
			}
			s.mu.Unlock()
		}
	}
}

// handleTelegramLink handles POST /api/v1/telegram/link.
// This is called by the Telegram plugin (broker-authenticated) to register a pending link code.
func (s *Server) handleTelegramLink(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		MethodNotAllowed(w)
		return
	}

	broker := GetBrokerIdentityFromContext(r.Context())
	if broker == nil {
		writeError(w, http.StatusUnauthorized, ErrCodeUnauthorized, "broker authentication required", nil)
		return
	}

	var req struct {
		Code           string `json:"code"`
		TelegramUserID string `json:"telegramUserId"`
	}
	if err := readJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, ErrCodeInvalidRequest, "invalid request body", nil)
		return
	}

	if req.Code == "" || req.TelegramUserID == "" {
		writeError(w, http.StatusBadRequest, ErrCodeInvalidRequest, "code and telegramUserId are required", nil)
		return
	}

	if s.telegramLinkService == nil {
		InternalError(w)
		return
	}

	s.telegramLinkService.RegisterCode(req.Code, req.TelegramUserID)

	slog.Info("Telegram link code registered",
		"code_prefix", req.Code[:3]+"***",
		"telegram_user_id", req.TelegramUserID,
		"broker_id", broker.BrokerID(),
	)

	writeJSON(w, http.StatusCreated, map[string]string{"status": "registered"})
}

// handleTelegramLinkVerify handles POST /api/v1/telegram/link/verify.
// This is called by a logged-in user from the web UI to confirm a link code.
func (s *Server) handleTelegramLinkVerify(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		MethodNotAllowed(w)
		return
	}

	user := GetUserIdentityFromContext(r.Context())
	if user == nil {
		writeError(w, http.StatusUnauthorized, ErrCodeUnauthorized, "authentication required", nil)
		return
	}

	var req struct {
		Code string `json:"code"`
	}
	if err := readJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, ErrCodeInvalidRequest, "invalid request body", nil)
		return
	}

	if req.Code == "" {
		writeError(w, http.StatusBadRequest, ErrCodeInvalidRequest, "code is required", nil)
		return
	}

	if s.telegramLinkService == nil {
		InternalError(w)
		return
	}

	telegramUserID, errReason := s.telegramLinkService.VerifyCode(req.Code, user.ID(), user.Email())
	if errReason != "" {
		switch errReason {
		case "code_not_found":
			writeError(w, http.StatusNotFound, ErrCodeNotFound, "code not found or expired", nil)
		case "code_expired":
			writeError(w, http.StatusGone, ErrCodeNotFound, "code has expired", nil)
		default:
			InternalError(w)
		}
		return
	}

	slog.Info("Telegram account linked",
		"telegram_user_id", telegramUserID,
		"user_id", user.ID(),
		"user_email", user.Email(),
	)

	writeJSON(w, http.StatusOK, map[string]interface{}{
		"status":         "confirmed",
		"telegramUserId": telegramUserID,
		"user": map[string]string{
			"id":    user.ID(),
			"email": user.Email(),
		},
	})
}

// handleTelegramLinkStatus handles GET /api/v1/telegram/link/status.
// This is called by the Telegram plugin (broker-authenticated) to poll for confirmation.
func (s *Server) handleTelegramLinkStatus(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		MethodNotAllowed(w)
		return
	}

	broker := GetBrokerIdentityFromContext(r.Context())
	if broker == nil {
		writeError(w, http.StatusUnauthorized, ErrCodeUnauthorized, "broker authentication required", nil)
		return
	}

	telegramUserID := r.URL.Query().Get("telegram_user_id")
	if telegramUserID == "" {
		writeError(w, http.StatusBadRequest, ErrCodeInvalidRequest, "telegram_user_id query parameter is required", nil)
		return
	}

	if s.telegramLinkService == nil {
		InternalError(w)
		return
	}

	status, userID, userEmail := s.telegramLinkService.GetStatusByTelegramUser(telegramUserID)

	resp := map[string]interface{}{
		"status": status,
	}
	if status == "confirmed" {
		resp["user"] = map[string]string{
			"id":    userID,
			"email": userEmail,
		}
	}

	writeJSON(w, http.StatusOK, resp)
}
