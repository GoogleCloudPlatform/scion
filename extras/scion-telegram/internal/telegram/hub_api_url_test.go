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
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/messages"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// recordingHub is a fake hub that records the paths it was asked for.
type recordingHub struct {
	srv   *httptest.Server
	mu    sync.Mutex
	paths []string
}

func newRecordingHub(t *testing.T) *recordingHub {
	t.Helper()
	h := &recordingHub{}
	h.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h.mu.Lock()
		h.paths = append(h.paths, r.Method+" "+r.URL.Path)
		h.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/v1/telegram/link":
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{}`))
		case "/api/v1/broker/projects":
			_, _ = w.Write([]byte(`{"projects":[]}`))
		default:
			_, _ = w.Write([]byte(`{}`))
		}
	}))
	t.Cleanup(h.srv.Close)
	return h
}

func (h *recordingHub) requests() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]string(nil), h.paths...)
}

// newHubAPIURLBroker configures a v2 broker against tgSrv with the given hub
// URL keys.
func newHubAPIURLBroker(t *testing.T, tgSrv *fakeTGServerV2, hubKeys map[string]string) *TelegramBrokerV2 {
	t.Helper()
	b := NewV2(slog.Default())
	t.Cleanup(func() { _ = b.Close() })
	cfg := map[string]string{
		"bot_token":      "test-token",
		"api_base_url":   tgSrv.srv.URL,
		"broker_id":      "broker-test",
		"hmac_key":       "MDEyMzQ1Njc4OWFiY2RlZjAxMjM0NTY3ODlhYmNkZWY=",
		"db_path":        filepath.Join(t.TempDir(), "test.db"),
		"downloads_path": filepath.Join(t.TempDir(), "downloads"),
	}
	for k, v := range hubKeys {
		cfg[k] = v
	}
	require.NoError(t, b.Configure(cfg))
	return b
}

// registrationLink returns the URL of the "Link Telegram account" button in
// the last message sent.
func registrationLink(t *testing.T, tgSrv *fakeTGServerV2) string {
	t.Helper()
	tgSrv.mu.Lock()
	defer tgSrv.mu.Unlock()
	require.NotEmpty(t, tgSrv.sentMessages)
	last := tgSrv.sentMessages[len(tgSrv.sentMessages)-1]
	require.NotNil(t, last.ReplyMarkup, "the registration card has a keyboard: %q", last.Text)
	require.NotEmpty(t, last.ReplyMarkup.InlineKeyboard)
	require.NotEmpty(t, last.ReplyMarkup.InlineKeyboard[0])
	return last.ReplyMarkup.InlineKeyboard[0][0].URL
}

func registerDM() *TGMessage {
	return &TGMessage{
		MessageID: 1,
		From:      &TGUser{ID: 4242, FirstName: "Alice", Username: "alice"},
		Chat:      TGChat{ID: 4242, Type: "private"},
		Text:      "/register",
	}
}

// TestV2_Register_UsesHubAPIURL pins the IAP fix: the link-code POST goes to
// hub_api_url (the hub's loopback URL), while the link people open on their
// phone is built from hub_url (the public endpoint).
func TestV2_Register_UsesHubAPIURL(t *testing.T) {
	tgSrv := newFakeTGServerV2(t)
	publicHub := newRecordingHub(t)
	apiHub := newRecordingHub(t)

	b := newHubAPIURLBroker(t, tgSrv, map[string]string{
		"hub_url":     publicHub.srv.URL,
		"hub_api_url": apiHub.srv.URL,
	})

	b.registration.HandleRegister(registerDM())

	assert.Contains(t, apiHub.requests(), "POST /api/v1/telegram/link")
	assert.Empty(t, publicHub.requests(), "no API call goes to the public endpoint")
	link := registrationLink(t, tgSrv)
	assert.True(t, strings.HasPrefix(link, publicHub.srv.URL+"/profile/telegram?code="),
		"the link people open uses hub_url, got %q", link)
}

// TestV2_Register_FallsBackToHubURL keeps the behaviour for older hubs and
// self-managed deployments, which inject no hub_api_url.
func TestV2_Register_FallsBackToHubURL(t *testing.T) {
	tgSrv := newFakeTGServerV2(t)
	hub := newRecordingHub(t)

	b := newHubAPIURLBroker(t, tgSrv, map[string]string{"hub_url": hub.srv.URL})

	b.registration.HandleRegister(registerDM())

	assert.Contains(t, hub.requests(), "POST /api/v1/telegram/link")
	link := registrationLink(t, tgSrv)
	assert.True(t, strings.HasPrefix(link, hub.srv.URL+"/profile/telegram?code="), "got %q", link)
}

// TestV2_HubClientAndInbound_UseHubAPIURL checks the hub client and inbound
// delivery call hub_api_url, while HubBaseURL (used for terminal links
// people open) stays hub_url.
func TestV2_HubClientAndInbound_UseHubAPIURL(t *testing.T) {
	tgSrv := newFakeTGServerV2(t)
	publicHub := newRecordingHub(t)
	apiHub := newRecordingHub(t)

	b := newHubAPIURLBroker(t, tgSrv, map[string]string{
		"hub_url":     publicHub.srv.URL,
		"hub_api_url": apiHub.srv.URL,
	})

	assert.Equal(t, publicHub.srv.URL, b.hubClient.HubBaseURL())
	_, err := b.hubClient.ListProjectsFresh(context.Background())
	require.NoError(t, err)
	_, err = b.hubClient.ListAgents(context.Background(), "p1", "user:alice@example.com")
	require.NoError(t, err)
	status, _ := b.deliverInboundWithFeedback(context.Background(), "scion.project.p1.user.u1.messages", &messages.StructuredMessage{Msg: "hi"})
	assert.Equal(t, http.StatusOK, status)

	reqs := apiHub.requests()
	assert.Contains(t, reqs, "GET /api/v1/broker/projects")
	assert.Contains(t, reqs, "GET /api/v1/projects/p1/agents")
	assert.Contains(t, reqs, "POST /api/v1/broker/inbound")
	assert.Empty(t, publicHub.requests(), "no API call goes to the public endpoint")

	health, err := b.HealthCheck()
	require.NoError(t, err)
	assert.Equal(t, publicHub.srv.URL, health.Details["hub_url"])
	assert.Equal(t, apiHub.srv.URL, health.Details["hub_api_url"])
}

// TestV2_Configure_HubAPIURLTravelsWithHubURL checks that a later
// configuration naming hub_url without hub_api_url drops the earlier
// hub_api_url instead of keeping a stale one.
func TestV2_Configure_HubAPIURLTravelsWithHubURL(t *testing.T) {
	tgSrv := newFakeTGServerV2(t)
	b := newHubAPIURLBroker(t, tgSrv, map[string]string{
		"hub_url":     "https://hub.example.com",
		"hub_api_url": "http://localhost:8080",
	})
	assert.Equal(t, "http://localhost:8080", b.hubAPIBase())

	require.NoError(t, b.Configure(map[string]string{
		"bot_token":    "test-token",
		"api_base_url": tgSrv.srv.URL,
		"hub_url":      "https://hub.example.com",
		"db_path":      filepath.Join(t.TempDir(), "test2.db"),
	}))
	assert.Equal(t, "https://hub.example.com", b.hubAPIBase())
}

// TestV1_Inbound_UsesHubAPIURL covers the legacy v1 broker's inbound
// delivery.
func TestV1_Inbound_UsesHubAPIURL(t *testing.T) {
	tgSrv := newFakeTelegramServer(t)
	publicHub := newRecordingHub(t)
	apiHub := newRecordingHub(t)

	b := newTestBroker(t, tgSrv)
	b.mu.Lock()
	b.hubURL = publicHub.srv.URL
	b.hubAPIURL = apiHub.srv.URL
	b.InboundHandler = nil // force HTTP delivery
	b.mu.Unlock()

	assert.Nil(t, b.deliverInbound("scion.project.p1.user.u1.messages", &messages.StructuredMessage{Msg: "hi"}))
	assert.Contains(t, apiHub.requests(), "POST /api/v1/broker/inbound")
	assert.Empty(t, publicHub.requests())
}
