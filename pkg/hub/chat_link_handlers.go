// Copyright 2026 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     https://www.apache.org/licenses/LICENSE-2.0
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
)

type chatLinkRegistrationRequest struct {
	Code           string `json:"code"`
	TelegramUserID string `json:"telegramUserId"`
	DiscordUserID  string `json:"discordUserId"`
	TeamsUserID    string `json:"teamsUserId"`
}

func (r chatLinkRegistrationRequest) userID(field string) string {
	switch field {
	case "telegramUserId":
		return r.TelegramUserID
	case "discordUserId":
		return r.DiscordUserID
	case "teamsUserId":
		return r.TeamsUserID
	default:
		return ""
	}
}

type chatLinkRegistrationOptions struct {
	providerName string
	userIDField  string
	userIDLogKey string
	register     func(code, userID string)
}

func handleChatLinkRegistration(w http.ResponseWriter, r *http.Request, opts chatLinkRegistrationOptions) {
	if r.Method != http.MethodPost {
		MethodNotAllowed(w)
		return
	}

	broker := GetBrokerIdentityFromContext(r.Context())
	if broker == nil {
		writeError(w, http.StatusUnauthorized, ErrCodeUnauthorized, "broker authentication required", nil)
		return
	}

	var req chatLinkRegistrationRequest
	if err := readJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, ErrCodeInvalidRequest, "invalid request body", nil)
		return
	}

	userID := req.userID(opts.userIDField)
	if req.Code == "" || userID == "" {
		writeError(w, http.StatusBadRequest, ErrCodeInvalidRequest, "code and "+opts.userIDField+" are required", nil)
		return
	}

	if opts.register == nil {
		InternalError(w)
		return
	}

	opts.register(req.Code, userID)
	slog.Info(opts.providerName+" link code registered",
		"code_prefix", maskedLinkCode(req.Code),
		opts.userIDLogKey, userID,
		"broker_id", broker.BrokerID(),
	)

	writeJSON(w, http.StatusCreated, map[string]string{"status": "registered"})
}

func maskedLinkCode(code string) string {
	if len(code) > 3 {
		code = code[:3]
	}
	return code + "***"
}
