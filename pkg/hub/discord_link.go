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

// DiscordLinkService manages pending Discord account link codes.
// When a ChatLinkStore is set, codes are persisted in the database so they
// survive across Hub instances. Otherwise, an in-memory map is used (single-node only).
type DiscordLinkService struct {
	*chatLinkService
}

// NewDiscordLinkService creates a new DiscordLinkService and starts
// a background goroutine that periodically removes expired entries.
func NewDiscordLinkService() *DiscordLinkService {
	return &DiscordLinkService{
		chatLinkService: newChatLinkService(chatlinkcode.ProviderDiscord, "Discord"),
	}
}

// GetStatusByDiscordUser returns the linking status for a given Discord user ID.
func (s *DiscordLinkService) GetStatusByDiscordUser(discordUserID string) (status, userID, userEmail string) {
	return s.GetStatusByUser(discordUserID)
}

// handleDiscordLink handles POST /api/v1/discord/link.
// This is called by the Discord plugin (broker-authenticated) to register a pending link code.
func (s *Server) handleDiscordLink(w http.ResponseWriter, r *http.Request) {
	var register func(string, string)
	if s.discordLinkService != nil {
		register = s.discordLinkService.RegisterCode
	}
	handleChatLinkRegistration(w, r, chatLinkRegistrationOptions{
		providerName: "Discord",
		userIDField:  "discordUserId",
		userIDLogKey: "discord_user_id",
		decode:       decodeDiscordLinkRegistration,
		register:     register,
	})
}

// handleDiscordLinkVerify handles POST /api/v1/discord/link/verify.
// This is called by a logged-in user from the web UI to confirm a link code.
func (s *Server) handleDiscordLinkVerify(w http.ResponseWriter, r *http.Request) {
	var allowVerify func(string) bool
	var verify func(string, string, string) (string, string)
	if s.discordLinkService != nil {
		allowVerify = s.discordLinkService.AllowVerify
		verify = s.discordLinkService.VerifyCode
	}
	handleChatLinkVerification(w, r, chatLinkVerificationOptions{
		providerName:      "Discord",
		userIDResponseKey: "discordUserId",
		userIDLogKey:      "discord_user_id",
		allowVerify:       allowVerify,
		verify:            verify,
	})
}

// handleDiscordLinkStatus handles GET /api/v1/discord/link/status.
// This is called by the Discord plugin (broker-authenticated) to poll for confirmation.
func (s *Server) handleDiscordLinkStatus(w http.ResponseWriter, r *http.Request) {
	var getStatus func(string) (string, string, string)
	var consume func(string)
	if s.discordLinkService != nil {
		getStatus = s.discordLinkService.GetStatusByDiscordUser
		consume = s.discordLinkService.ConsumePending
	}
	handleChatLinkStatus(w, r, chatLinkStatusOptions{
		userIDQueryParam: "discord_user_id",
		getStatus:        getStatus,
		consume:          consume,
	})
}
