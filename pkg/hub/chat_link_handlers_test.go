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

func TestMaskedLinkCode(t *testing.T) {
	assert.Equal(t, "A***", maskedLinkCode("A"))
	assert.Equal(t, "ABC***", maskedLinkCode("ABC"))
	assert.Equal(t, "ABC***", maskedLinkCode("ABC123"))
}
