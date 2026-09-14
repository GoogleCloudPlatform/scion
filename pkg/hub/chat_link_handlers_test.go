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
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestChatLinkRegistrationAcceptsShortCodes(t *testing.T) {
	srv := &Server{
		telegramLinkService: NewTelegramLinkService(),
		discordLinkService:  NewDiscordLinkService(),
		teamsLinkService:    NewTeamsLinkService(),
	}
	t.Cleanup(srv.telegramLinkService.Close)
	t.Cleanup(srv.discordLinkService.Close)
	t.Cleanup(srv.teamsLinkService.Close)

	tests := []struct {
		name    string
		body    string
		handler http.HandlerFunc
	}{
		{name: "telegram", body: `{"code":"A","telegramUserId":"telegram-user"}`, handler: srv.handleTelegramLink},
		{name: "discord", body: `{"code":"A","discordUserId":"discord-user"}`, handler: srv.handleDiscordLink},
		{name: "teams", body: `{"code":"A","teamsUserId":"teams-user"}`, handler: srv.handleTeamsLink},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(tt.body))
			req = req.WithContext(contextWithBrokerIdentity(req.Context(), NewBrokerIdentity("broker-1")))
			recorder := httptest.NewRecorder()

			require.NotPanics(t, func() { tt.handler(recorder, req) })
			assert.Equal(t, http.StatusCreated, recorder.Code)
		})
	}
}

func TestChatLinkRegistrationPreservesProviderValidation(t *testing.T) {
	srv := &Server{
		telegramLinkService: NewTelegramLinkService(),
		discordLinkService:  NewDiscordLinkService(),
		teamsLinkService:    NewTeamsLinkService(),
	}
	t.Cleanup(srv.telegramLinkService.Close)
	t.Cleanup(srv.discordLinkService.Close)
	t.Cleanup(srv.teamsLinkService.Close)

	tests := []struct {
		name      string
		body      string
		handler   http.HandlerFunc
		wantError string
	}{
		{name: "telegram", body: `{"code":"ABC123"}`, handler: srv.handleTelegramLink, wantError: "code and telegramUserId are required"},
		{name: "discord", body: `{"code":"ABC123"}`, handler: srv.handleDiscordLink, wantError: "code and discordUserId are required"},
		{name: "teams", body: `{"code":"ABC123"}`, handler: srv.handleTeamsLink, wantError: "code and teamsUserId are required"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(tt.body))
			req = req.WithContext(contextWithBrokerIdentity(req.Context(), NewBrokerIdentity("broker-1")))
			recorder := httptest.NewRecorder()

			tt.handler(recorder, req)

			assert.Equal(t, http.StatusBadRequest, recorder.Code)
			assert.Contains(t, recorder.Body.String(), tt.wantError)
		})
	}
}

func TestChatLinkRegistrationPreservesCommonGuards(t *testing.T) {
	srv := &Server{telegramLinkService: NewTelegramLinkService()}
	t.Cleanup(srv.telegramLinkService.Close)

	tests := []struct {
		name       string
		method     string
		body       string
		withBroker bool
		server     *Server
		wantStatus int
	}{
		{name: "method", method: http.MethodGet, server: srv, wantStatus: http.StatusMethodNotAllowed},
		{name: "broker authentication", method: http.MethodPost, body: `{"code":"ABC123","telegramUserId":"telegram-user"}`, server: srv, wantStatus: http.StatusUnauthorized},
		{name: "invalid body", method: http.MethodPost, body: `{`, withBroker: true, server: srv, wantStatus: http.StatusBadRequest},
		{name: "service unavailable", method: http.MethodPost, body: `{"code":"ABC123","telegramUserId":"telegram-user"}`, withBroker: true, server: &Server{}, wantStatus: http.StatusInternalServerError},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest(tt.method, "/", strings.NewReader(tt.body))
			if tt.withBroker {
				req = req.WithContext(contextWithBrokerIdentity(req.Context(), NewBrokerIdentity("broker-1")))
			}
			recorder := httptest.NewRecorder()

			tt.server.handleTelegramLink(recorder, req)

			assert.Equal(t, tt.wantStatus, recorder.Code)
		})
	}
}

func TestChatLinkRegistrationJSONCompatibility(t *testing.T) {
	tests := []struct {
		name       string
		body       string
		wantStatus int
		wantError  string
		wantUserID string
	}{
		{
			name:       "wrong-type unrelated provider field is ignored",
			body:       `{"code":"ABC123","telegramUserId":"telegram-user","discordUserId":123}`,
			wantStatus: http.StatusCreated,
			wantUserID: "telegram-user",
		},
		{
			name:       "wrong-type selected field is rejected",
			body:       `{"code":"ABC123","telegramUserId":123}`,
			wantStatus: http.StatusBadRequest,
			wantError:  "invalid request body",
		},
		{
			name:       "null selected field is missing",
			body:       `{"code":"ABC123","telegramUserId":null}`,
			wantStatus: http.StatusBadRequest,
			wantError:  "code and telegramUserId are required",
		},
		{
			name:       "duplicate selected field uses last value",
			body:       `{"code":"ABC123","telegramUserId":"first","telegramUserId":"second"}`,
			wantStatus: http.StatusCreated,
			wantUserID: "second",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			svc := NewTelegramLinkService()
			defer svc.Close()
			srv := &Server{telegramLinkService: svc}
			req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(tt.body))
			req = req.WithContext(contextWithBrokerIdentity(req.Context(), NewBrokerIdentity("broker-1")))
			recorder := httptest.NewRecorder()

			srv.handleTelegramLink(recorder, req)

			assert.Equal(t, tt.wantStatus, recorder.Code)
			if tt.wantError != "" {
				assert.Contains(t, recorder.Body.String(), tt.wantError)
			}
			if tt.wantUserID != "" {
				status, _, _ := svc.GetStatusByTelegramUser(tt.wantUserID)
				assert.Equal(t, "pending", status)
			}
		})
	}
}

func TestChatLinkVerification(t *testing.T) {
	srv := &Server{
		telegramLinkService: NewTelegramLinkService(),
		discordLinkService:  NewDiscordLinkService(),
		teamsLinkService:    NewTeamsLinkService(),
	}
	t.Cleanup(srv.telegramLinkService.Close)
	t.Cleanup(srv.discordLinkService.Close)
	t.Cleanup(srv.teamsLinkService.Close)

	tests := []struct {
		name           string
		providerUserID string
		responseKey    string
		register       func(code, providerUserID string)
		handler        http.HandlerFunc
	}{
		{name: "telegram", providerUserID: "telegram-user", responseKey: "telegramUserId", register: srv.telegramLinkService.RegisterCode, handler: srv.handleTelegramLinkVerify},
		{name: "discord", providerUserID: "discord-user", responseKey: "discordUserId", register: srv.discordLinkService.RegisterCode, handler: srv.handleDiscordLinkVerify},
		{name: "teams", providerUserID: "teams-user", responseKey: "teamsUserId", register: srv.teamsLinkService.RegisterCode, handler: srv.handleTeamsLinkVerify},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tt.register("ABC123", tt.providerUserID)
			req := authenticatedLinkRequest(http.MethodPost, `{"code":"ABC123"}`)
			recorder := httptest.NewRecorder()

			tt.handler(recorder, req)

			assert.Equal(t, http.StatusOK, recorder.Code)
			var response map[string]interface{}
			require.NoError(t, json.Unmarshal(recorder.Body.Bytes(), &response))
			assert.Equal(t, "confirmed", response["status"])
			assert.Equal(t, tt.providerUserID, response[tt.responseKey])
			assert.Equal(t, map[string]interface{}{"id": "user-1", "email": "user@example.com"}, response["user"])
		})
	}
}

func TestChatLinkVerificationPreservesCommonGuards(t *testing.T) {
	tests := []struct {
		name       string
		method     string
		body       string
		withUser   bool
		nilService bool
		setup      func(*testing.T, *TelegramLinkService)
		wantStatus int
	}{
		{name: "method", method: http.MethodGet, wantStatus: http.StatusMethodNotAllowed},
		{name: "user authentication", method: http.MethodPost, body: `{"code":"ABC123"}`, wantStatus: http.StatusUnauthorized},
		{name: "invalid body", method: http.MethodPost, body: `{`, withUser: true, wantStatus: http.StatusBadRequest},
		{name: "code required", method: http.MethodPost, body: `{}`, withUser: true, wantStatus: http.StatusBadRequest},
		{name: "service unavailable", method: http.MethodPost, body: `{"code":"ABC123"}`, withUser: true, nilService: true, wantStatus: http.StatusInternalServerError},
		{name: "code not found", method: http.MethodPost, body: `{"code":"MISSING"}`, withUser: true, wantStatus: http.StatusNotFound},
		{
			name: "code expired", method: http.MethodPost, body: `{"code":"EXPIRED"}`, withUser: true, wantStatus: http.StatusGone,
			setup: func(_ *testing.T, svc *TelegramLinkService) {
				svc.RegisterCode("EXPIRED", "telegram-user")
				svc.mu.Lock()
				svc.pending["EXPIRED"].ExpiresAt = time.Now().Add(-time.Minute)
				svc.mu.Unlock()
			},
		},
		{
			name: "rate limit precedes body validation", method: http.MethodPost, body: `{`, withUser: true, wantStatus: http.StatusTooManyRequests,
			setup: func(t *testing.T, svc *TelegramLinkService) {
				for i := 0; i < verifyBurst; i++ {
					require.True(t, svc.AllowVerify("192.0.2.1"))
				}
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			svc := NewTelegramLinkService()
			defer svc.Close()
			if tt.setup != nil {
				tt.setup(t, svc)
			}
			srv := &Server{telegramLinkService: svc}
			if tt.nilService {
				srv.telegramLinkService = nil
			}
			req := httptest.NewRequest(tt.method, "/", strings.NewReader(tt.body))
			if tt.withUser {
				req = req.WithContext(contextWithIdentity(req.Context(), NewAuthenticatedUser("user-1", "user@example.com", "User", "member", "web")))
			}
			recorder := httptest.NewRecorder()

			srv.handleTelegramLinkVerify(recorder, req)

			assert.Equal(t, tt.wantStatus, recorder.Code)
		})
	}
}

func authenticatedLinkRequest(method, body string) *http.Request {
	req := httptest.NewRequest(method, "/", strings.NewReader(body))
	return req.WithContext(contextWithIdentity(req.Context(), NewAuthenticatedUser("user-1", "user@example.com", "User", "member", "web")))
}

func TestMaskedLinkCode(t *testing.T) {
	assert.Equal(t, "A***", maskedLinkCode("A"))
	assert.Equal(t, "ABC***", maskedLinkCode("ABC"))
	assert.Equal(t, "ABC***", maskedLinkCode("ABC123"))
}
