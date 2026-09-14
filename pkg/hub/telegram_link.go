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
	"net/http"

	"github.com/GoogleCloudPlatform/scion/pkg/ent/chatlinkcode"
)

// TelegramLinkService manages pending Telegram account link codes.
// When a ChatLinkStore is set, codes are persisted in the database so they
// survive across Hub instances. Otherwise, an in-memory map is used (single-node only).
type TelegramLinkService struct {
	*chatLinkService
}

// NewTelegramLinkService creates a new TelegramLinkService and starts
// a background goroutine that periodically removes expired entries.
func NewTelegramLinkService() *TelegramLinkService {
	return &TelegramLinkService{
		chatLinkService: newChatLinkService(chatlinkcode.ProviderTelegram, "Telegram"),
	}
}

// GetStatusByTelegramUser returns the linking status for a given Telegram user ID.
func (s *TelegramLinkService) GetStatusByTelegramUser(telegramUserID string) (status, userID, userEmail string) {
	return s.GetStatusByUser(telegramUserID)
}

// handleTelegramLink handles POST /api/v1/telegram/link.
// This is called by the Telegram plugin (broker-authenticated) to register a pending link code.
func (s *Server) handleTelegramLink(w http.ResponseWriter, r *http.Request) {
	var register func(string, string)
	if s.telegramLinkService != nil {
		register = s.telegramLinkService.RegisterCode
	}
	handleChatLinkRegistration(w, r, chatLinkRegistrationOptions{
		providerName: "Telegram",
		userIDField:  "telegramUserId",
		userIDLogKey: "telegram_user_id",
		decode:       decodeTelegramLinkRegistration,
		register:     register,
	})
}

// handleTelegramLinkVerify handles POST /api/v1/telegram/link/verify.
// This is called by a logged-in user from the web UI to confirm a link code.
func (s *Server) handleTelegramLinkVerify(w http.ResponseWriter, r *http.Request) {
	var allowVerify func(string) bool
	var verify func(string, string, string) (string, string)
	if s.telegramLinkService != nil {
		allowVerify = s.telegramLinkService.AllowVerify
		verify = s.telegramLinkService.VerifyCode
	}
	handleChatLinkVerification(w, r, chatLinkVerificationOptions{
		providerName:      "Telegram",
		userIDResponseKey: "telegramUserId",
		userIDLogKey:      "telegram_user_id",
		allowVerify:       allowVerify,
		verify:            verify,
	})
}

// handleTelegramLinkStatus handles GET /api/v1/telegram/link/status.
// This is called by the Telegram plugin (broker-authenticated) to poll for confirmation.
func (s *Server) handleTelegramLinkStatus(w http.ResponseWriter, r *http.Request) {
	var getStatus func(string) (string, string, string)
	var consume func(string)
	if s.telegramLinkService != nil {
		getStatus = s.telegramLinkService.GetStatusByTelegramUser
		consume = s.telegramLinkService.ConsumePending
	}
	handleChatLinkStatus(w, r, chatLinkStatusOptions{
		userIDQueryParam: "telegram_user_id",
		getStatus:        getStatus,
		consume:          consume,
	})
}
