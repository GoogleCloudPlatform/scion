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

// TeamsLinkService manages pending Teams account link codes.
// When a ChatLinkStore is set, codes are persisted in the database so they
// survive across Hub instances. Otherwise, an in-memory map is used (single-node only).
type TeamsLinkService struct {
	*chatLinkService
}

// NewTeamsLinkService creates a new TeamsLinkService and starts
// a background goroutine that periodically removes expired entries.
func NewTeamsLinkService() *TeamsLinkService {
	return &TeamsLinkService{
		chatLinkService: newChatLinkService(chatlinkcode.ProviderTeams, "Teams"),
	}
}

// GetStatusByTeamsUser returns the linking status for a given Teams user ID.
func (s *TeamsLinkService) GetStatusByTeamsUser(teamsUserID string) (status, userID, userEmail string) {
	return s.GetStatusByUser(teamsUserID)
}

// handleTeamsLink handles POST /api/v1/teams/link.
// This is called by the Teams plugin (broker-authenticated) to register a pending link code.
func (s *Server) handleTeamsLink(w http.ResponseWriter, r *http.Request) {
	var register func(string, string)
	if s.teamsLinkService != nil {
		register = s.teamsLinkService.RegisterCode
	}
	handleChatLinkRegistration(w, r, chatLinkRegistrationOptions{
		providerName: "Teams",
		userIDField:  "teamsUserId",
		userIDLogKey: "teams_user_id",
		decode:       decodeTeamsLinkRegistration,
		register:     register,
	})
}

// handleTeamsLinkVerify handles POST /api/v1/teams/link/verify.
// This is called by a logged-in user from the web UI to confirm a link code.
func (s *Server) handleTeamsLinkVerify(w http.ResponseWriter, r *http.Request) {
	var allowVerify func(string) bool
	var verify func(string, string, string) (string, string)
	if s.teamsLinkService != nil {
		allowVerify = s.teamsLinkService.AllowVerify
		verify = s.teamsLinkService.VerifyCode
	}
	handleChatLinkVerification(w, r, chatLinkVerificationOptions{
		providerName:      "Teams",
		userIDResponseKey: "teamsUserId",
		userIDLogKey:      "teams_user_id",
		allowVerify:       allowVerify,
		verify:            verify,
	})
}

// handleTeamsLinkStatus handles GET /api/v1/teams/link/status.
// This is called by the Teams plugin (broker-authenticated) to poll for confirmation.
func (s *Server) handleTeamsLinkStatus(w http.ResponseWriter, r *http.Request) {
	var getStatus func(string) (string, string, string)
	var consume func(string)
	if s.teamsLinkService != nil {
		getStatus = s.teamsLinkService.GetStatusByTeamsUser
		consume = s.teamsLinkService.ConsumePending
	}
	handleChatLinkStatus(w, r, chatLinkStatusOptions{
		userIDQueryParam: "teams_user_id",
		getStatus:        getStatus,
		consume:          consume,
	})
}
