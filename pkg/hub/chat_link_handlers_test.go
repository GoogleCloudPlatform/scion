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
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

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

func TestMaskedLinkCode(t *testing.T) {
	assert.Equal(t, "A***", maskedLinkCode("A"))
	assert.Equal(t, "ABC***", maskedLinkCode("ABC"))
	assert.Equal(t, "ABC***", maskedLinkCode("ABC123"))
}
