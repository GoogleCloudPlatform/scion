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
	"net"
	"net/http"
)

type chatLinkProvider struct {
	name             string
	userIDField      string
	userIDLogKey     string
	userIDQueryParam string
	decode           func(*http.Request) (code, userID string, err error)
	service          func(*Server) *chatLinkService
}

var (
	telegramLinkProvider = chatLinkProvider{
		name:             "Telegram",
		userIDField:      "telegramUserId",
		userIDLogKey:     "telegram_user_id",
		userIDQueryParam: "telegram_user_id",
		decode:           decodeTelegramLinkRegistration,
		service: func(s *Server) *chatLinkService {
			if s.telegramLinkService == nil {
				return nil
			}
			return s.telegramLinkService.chatLinkService
		},
	}
	discordLinkProvider = chatLinkProvider{
		name:             "Discord",
		userIDField:      "discordUserId",
		userIDLogKey:     "discord_user_id",
		userIDQueryParam: "discord_user_id",
		decode:           decodeDiscordLinkRegistration,
		service: func(s *Server) *chatLinkService {
			if s.discordLinkService == nil {
				return nil
			}
			return s.discordLinkService.chatLinkService
		},
	}
	teamsLinkProvider = chatLinkProvider{
		name:             "Teams",
		userIDField:      "teamsUserId",
		userIDLogKey:     "teams_user_id",
		userIDQueryParam: "teams_user_id",
		decode:           decodeTeamsLinkRegistration,
		service: func(s *Server) *chatLinkService {
			if s.teamsLinkService == nil {
				return nil
			}
			return s.teamsLinkService.chatLinkService
		},
	}
)

func (p chatLinkProvider) handleRegistration(s *Server, w http.ResponseWriter, r *http.Request) {
	service := p.service(s)
	var register func(string, string)
	if service != nil {
		register = service.RegisterCode
	}
	handleChatLinkRegistration(w, r, chatLinkRegistrationOptions{
		providerName: p.name,
		userIDField:  p.userIDField,
		userIDLogKey: p.userIDLogKey,
		decode:       p.decode,
		register:     register,
	})
}

func (p chatLinkProvider) handleVerification(s *Server, w http.ResponseWriter, r *http.Request) {
	service := p.service(s)
	var allowVerify func(string) bool
	var verify func(string, string, string) (string, string)
	if service != nil {
		allowVerify = service.AllowVerify
		verify = service.VerifyCode
	}
	handleChatLinkVerification(w, r, chatLinkVerificationOptions{
		providerName:      p.name,
		userIDResponseKey: p.userIDField,
		userIDLogKey:      p.userIDLogKey,
		allowVerify:       allowVerify,
		verify:            verify,
	})
}

func (p chatLinkProvider) handleStatus(s *Server, w http.ResponseWriter, r *http.Request) {
	service := p.service(s)
	var getStatus func(string) (string, string, string)
	var consume func(string)
	if service != nil {
		getStatus = service.GetStatusByUser
		consume = service.ConsumePending
	}
	handleChatLinkStatus(w, r, chatLinkStatusOptions{
		userIDQueryParam: p.userIDQueryParam,
		getStatus:        getStatus,
		consume:          consume,
	})
}

// handleTelegramLink handles POST /api/v1/telegram/link.
func (s *Server) handleTelegramLink(w http.ResponseWriter, r *http.Request) {
	telegramLinkProvider.handleRegistration(s, w, r)
}

// handleDiscordLink handles POST /api/v1/discord/link.
func (s *Server) handleDiscordLink(w http.ResponseWriter, r *http.Request) {
	discordLinkProvider.handleRegistration(s, w, r)
}

// handleTeamsLink handles POST /api/v1/teams/link.
func (s *Server) handleTeamsLink(w http.ResponseWriter, r *http.Request) {
	teamsLinkProvider.handleRegistration(s, w, r)
}

// handleTelegramLinkVerify handles POST /api/v1/telegram/link/verify.
func (s *Server) handleTelegramLinkVerify(w http.ResponseWriter, r *http.Request) {
	telegramLinkProvider.handleVerification(s, w, r)
}

// handleDiscordLinkVerify handles POST /api/v1/discord/link/verify.
func (s *Server) handleDiscordLinkVerify(w http.ResponseWriter, r *http.Request) {
	discordLinkProvider.handleVerification(s, w, r)
}

// handleTeamsLinkVerify handles POST /api/v1/teams/link/verify.
func (s *Server) handleTeamsLinkVerify(w http.ResponseWriter, r *http.Request) {
	teamsLinkProvider.handleVerification(s, w, r)
}

// handleTelegramLinkStatus handles GET /api/v1/telegram/link/status.
func (s *Server) handleTelegramLinkStatus(w http.ResponseWriter, r *http.Request) {
	telegramLinkProvider.handleStatus(s, w, r)
}

// handleDiscordLinkStatus handles GET /api/v1/discord/link/status.
func (s *Server) handleDiscordLinkStatus(w http.ResponseWriter, r *http.Request) {
	discordLinkProvider.handleStatus(s, w, r)
}

// handleTeamsLinkStatus handles GET /api/v1/teams/link/status.
func (s *Server) handleTeamsLinkStatus(w http.ResponseWriter, r *http.Request) {
	teamsLinkProvider.handleStatus(s, w, r)
}

type chatLinkRegistrationOptions struct {
	providerName string
	userIDField  string
	userIDLogKey string
	decode       func(*http.Request) (code, userID string, err error)
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

	code, userID, err := opts.decode(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, ErrCodeInvalidRequest, "invalid request body", nil)
		return
	}

	if code == "" || userID == "" {
		writeError(w, http.StatusBadRequest, ErrCodeInvalidRequest, "code and "+opts.userIDField+" are required", nil)
		return
	}

	if opts.register == nil {
		InternalError(w)
		return
	}

	opts.register(code, userID)
	slog.Info(opts.providerName+" link code registered",
		"code_prefix", maskedLinkCode(code),
		opts.userIDLogKey, userID,
		"broker_id", broker.BrokerID(),
	)

	writeJSON(w, http.StatusCreated, map[string]string{"status": "registered"})
}

func decodeTelegramLinkRegistration(r *http.Request) (string, string, error) {
	var req struct {
		Code           string `json:"code"`
		TelegramUserID string `json:"telegramUserId"`
	}
	err := readJSON(r, &req)
	return req.Code, req.TelegramUserID, err
}

func decodeDiscordLinkRegistration(r *http.Request) (string, string, error) {
	var req struct {
		Code          string `json:"code"`
		DiscordUserID string `json:"discordUserId"`
	}
	err := readJSON(r, &req)
	return req.Code, req.DiscordUserID, err
}

func decodeTeamsLinkRegistration(r *http.Request) (string, string, error) {
	var req struct {
		Code        string `json:"code"`
		TeamsUserID string `json:"teamsUserId"`
	}
	err := readJSON(r, &req)
	return req.Code, req.TeamsUserID, err
}

type chatLinkVerificationOptions struct {
	providerName      string
	userIDResponseKey string
	userIDLogKey      string
	allowVerify       func(ip string) bool
	verify            func(code, userID, userEmail string) (providerUserID, reason string)
}

func handleChatLinkVerification(w http.ResponseWriter, r *http.Request, opts chatLinkVerificationOptions) {
	if r.Method != http.MethodPost {
		MethodNotAllowed(w)
		return
	}

	user := GetUserIdentityFromContext(r.Context())
	if user == nil {
		writeError(w, http.StatusUnauthorized, ErrCodeUnauthorized, "authentication required", nil)
		return
	}

	if opts.allowVerify != nil && !opts.allowVerify(remoteIP(r.RemoteAddr)) {
		writeError(w, http.StatusTooManyRequests, ErrCodeRateLimited, "too many verify attempts, try again later", nil)
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

	if opts.verify == nil {
		InternalError(w)
		return
	}

	providerUserID, reason := opts.verify(req.Code, user.ID(), user.Email())
	if reason != "" {
		switch reason {
		case "code_not_found":
			writeError(w, http.StatusNotFound, ErrCodeNotFound, "code not found or expired", nil)
		case "code_expired":
			writeError(w, http.StatusGone, ErrCodeNotFound, "code has expired", nil)
		default:
			InternalError(w)
		}
		return
	}

	slog.Info(opts.providerName+" account linked",
		opts.userIDLogKey, providerUserID,
		"user_id", user.ID(),
		"user_email", user.Email(),
	)

	writeJSON(w, http.StatusOK, map[string]interface{}{
		"status":               "confirmed",
		opts.userIDResponseKey: providerUserID,
		"user": map[string]string{
			"id":    user.ID(),
			"email": user.Email(),
		},
	})
}

func remoteIP(remoteAddr string) string {
	ip, _, err := net.SplitHostPort(remoteAddr)
	if err != nil {
		return remoteAddr
	}
	return ip
}

type chatLinkStatusOptions struct {
	userIDQueryParam string
	getStatus        func(providerUserID string) (status, userID, userEmail string)
	consume          func(providerUserID string)
}

func handleChatLinkStatus(w http.ResponseWriter, r *http.Request, opts chatLinkStatusOptions) {
	if r.Method != http.MethodGet {
		MethodNotAllowed(w)
		return
	}

	if GetBrokerIdentityFromContext(r.Context()) == nil {
		writeError(w, http.StatusUnauthorized, ErrCodeUnauthorized, "broker authentication required", nil)
		return
	}

	providerUserID := r.URL.Query().Get(opts.userIDQueryParam)
	if providerUserID == "" {
		writeError(w, http.StatusBadRequest, ErrCodeInvalidRequest, opts.userIDQueryParam+" query parameter is required", nil)
		return
	}

	if opts.getStatus == nil {
		InternalError(w)
		return
	}

	status, userID, userEmail := opts.getStatus(providerUserID)
	response := map[string]interface{}{"status": status}
	if status == "confirmed" {
		response["user"] = map[string]string{
			"id":    userID,
			"email": userEmail,
		}
	}

	writeJSON(w, http.StatusOK, response)

	if status == "confirmed" {
		opts.consume(providerUserID)
	}
}

func maskedLinkCode(code string) string {
	if len(code) > 3 {
		code = code[:3]
	}
	return code + "***"
}
