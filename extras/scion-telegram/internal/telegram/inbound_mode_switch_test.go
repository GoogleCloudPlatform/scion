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
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Tests for switching the inbound mode (poll / webhook) at runtime.

// pollState returns whether a poll loop is running and the channel that is
// closed when the most recent poll loop has exited.
func pollState(b *TelegramBrokerV2) (running bool, done <-chan struct{}) {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return b.pollCancel != nil, b.pollDone
}

// getUpdatesCount returns how many getUpdates requests the fake received.
func (f *fakeTGServerV2) getUpdatesCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.getUpdatesCalls
}

// waitPolling waits until the broker has polled the fake at least once.
func waitPolling(t *testing.T, tgSrv *fakeTGServerV2) {
	t.Helper()
	require.Eventually(t, func() bool { return tgSrv.getUpdatesCount() > 0 },
		5*time.Second, 10*time.Millisecond, "the broker must poll for updates")
}

// assertClosed checks that done is closed now, without waiting.
func assertClosed(t *testing.T, done <-chan struct{}, msg string) {
	t.Helper()
	require.NotNil(t, done, "a poll loop must have been started")
	select {
	case <-done:
	default:
		assert.Fail(t, msg)
	}
}

// Switching from webhook mode to poll mode removes the webhook registration
// and starts polling for the subscriptions made in webhook mode.
func TestV2_Reconfigure_WebhookToPollDeletesWebhookAndPolls(t *testing.T) {
	tgSrv := newFakeTGServerV2(t)
	b := NewV2(slog.Default())
	defer b.Close()

	require.NoError(t, b.Configure(validWebhookConfig(t, tgSrv)))
	require.NoError(t, b.Subscribe("scion.project.proj-1.>"))
	running, _ := pollState(b)
	require.False(t, running, "webhook mode must not poll")

	require.NoError(t, b.Configure(validPollConfig(t, tgSrv)))

	tgSrv.mu.Lock()
	deleteCalls, registeredURL := tgSrv.deleteWebhookCalls, tgSrv.webhookURL
	tgSrv.mu.Unlock()
	assert.Equal(t, 1, deleteCalls, "leaving webhook mode must call deleteWebhook")
	assert.Empty(t, registeredURL, "the webhook must no longer be registered")

	running, _ = pollState(b)
	assert.True(t, running, "poll mode must poll for the existing subscriptions")
	waitPolling(t, tgSrv)
}

// Switching from poll mode to webhook mode stops the poll loop and waits for
// it: while a polled update's handler is still running, Configure does not
// return; once the handler finishes, the loop has exited when Configure
// returns.
func TestV2_Reconfigure_PollToWebhookStopsPollLoop(t *testing.T) {
	tgSrv := newFakeTGServerV2(t)
	g := newGatedTelegram(t, tgSrv)
	b := NewV2(slog.Default())
	defer b.Close()
	defer g.unblock()

	pollCfg := validPollConfig(t, tgSrv)
	pollCfg["api_base_url"] = g.srv.URL
	require.NoError(t, b.Configure(pollCfg))

	u := dmUpdate()
	g.pending.Store(&u)
	g.armed.Store(true)
	require.NoError(t, b.Subscribe("scion.project.proj-1.>"))
	g.waitHeld(t) // the polled update's handler is now held
	_, done := pollState(b)

	webhookCfg := validWebhookConfig(t, tgSrv)
	webhookCfg["api_base_url"] = g.srv.URL
	configured := make(chan error, 1)
	go func() { configured <- b.Configure(webhookCfg) }()

	assert.Never(t, func() bool { return len(configured) > 0 }, 300*time.Millisecond, 10*time.Millisecond,
		"the switch to webhook mode must wait for the poll loop's running handler")

	g.unblock()
	select {
	case err := <-configured:
		require.NoError(t, err)
	case <-time.After(10 * time.Second):
		require.FailNow(t, "Configure did not return after the handler finished")
	}
	assertClosed(t, done, "the poll loop must have exited when the switch to webhook mode returns")
	running, _ := pollState(b)
	assert.False(t, running, "webhook mode must not keep a poll loop")
}

// Close after a switch from poll mode to webhook mode leaves no poll loop
// (the issue's webhook-mode Close case).
func TestV2_Close_AfterPollToWebhookLeavesNoPollLoop(t *testing.T) {
	tgSrv := newFakeTGServerV2(t)
	b := NewV2(slog.Default())
	defer b.Close() // idempotent; releases the broker if a require fails

	require.NoError(t, b.Configure(validPollConfig(t, tgSrv)))
	require.NoError(t, b.Subscribe("scion.project.proj-1.>"))
	waitPolling(t, tgSrv)
	_, done := pollState(b)

	require.NoError(t, b.Configure(validWebhookConfig(t, tgSrv)))
	require.NoError(t, b.Close())

	assertClosed(t, done, "no poll loop may be left after Close in webhook mode")
	running, _ := pollState(b)
	assert.False(t, running, "no poll loop may be left after Close")
}

// Close stops the poll loop; it has exited by the time Close returns. Close
// in poll mode already stopped polling before the inbound-mode fix, so this
// test passes without it; it guards the stopPolling call in Close, which now
// runs in either mode.
func TestV2_Close_StopsPollLoop(t *testing.T) {
	tgSrv := newFakeTGServerV2(t)
	b := NewV2(slog.Default())

	require.NoError(t, b.Configure(validPollConfig(t, tgSrv)))
	require.NoError(t, b.Subscribe("scion.project.proj-1.>"))
	waitPolling(t, tgSrv)
	_, done := pollState(b)

	require.NoError(t, b.Close())

	assertClosed(t, done, "the poll loop must have exited when Close returns")
	running, _ := pollState(b)
	assert.False(t, running, "no poll loop may be left after Close")
}

// multiBotTelegram is a fake Telegram API that answers for several bot
// credentials. Each credential maps to a bot ID (several credentials may
// belong to one bot); a rejected credential gets 401 for every method. It
// counts deleteWebhook calls per credential, including rejected ones.
type multiBotTelegram struct {
	srv *httptest.Server

	mu            sync.Mutex
	botIDs        map[string]int64
	rejected      map[string]bool
	deleteWebhook map[string]int
}

func newMultiBotTelegram(t *testing.T, botIDs map[string]int64) *multiBotTelegram {
	t.Helper()
	f := &multiBotTelegram{botIDs: botIDs, rejected: make(map[string]bool), deleteWebhook: make(map[string]int)}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		token, method, ok := strings.Cut(strings.TrimPrefix(r.URL.Path, "/bot"), "/")
		f.mu.Lock()
		botID, known := f.botIDs[token]
		rejected := f.rejected[token]
		if ok && method == "deleteWebhook" {
			f.deleteWebhook[token]++
		}
		f.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		if !ok || !known || rejected {
			w.WriteHeader(http.StatusUnauthorized)
			_ = json.NewEncoder(w).Encode(apiResponse{OK: false, ErrorCode: 401, Description: "Unauthorized"})
			return
		}
		var result any = true
		switch method {
		case "getMe":
			result = BotUser{ID: botID, IsBot: true, FirstName: "TestBot", Username: "test_bot"}
		case "getUpdates":
			select {
			case <-time.After(20 * time.Millisecond):
			case <-r.Context().Done():
				return
			}
			result = []Update{}
		}
		_ = json.NewEncoder(w).Encode(apiResponse{OK: true, Result: mustJSONRawV2(t, result)})
	}))
	t.Cleanup(f.srv.Close)
	return f
}

// reject makes every later call with token fail with 401.
func (f *multiBotTelegram) reject(token string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.rejected[token] = true
}

// deleteWebhookCalls returns the deleteWebhook calls made with token.
func (f *multiBotTelegram) deleteWebhookCalls(token string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.deleteWebhook[token]
}

// switchBotAndMode configures webhook mode with oldToken, calls between,
// then reconfigures to poll mode with newToken and returns that error.
func switchBotAndMode(t *testing.T, b *TelegramBrokerV2, f *multiBotTelegram, oldToken, newToken string, between func()) error {
	t.Helper()
	require.NoError(t, b.Configure(map[string]string{
		"bot_token":      oldToken,
		"api_base_url":   f.srv.URL,
		"hub_url":        "http://hub.test",
		"db_path":        filepath.Join(t.TempDir(), "test.db"),
		"inbound_mode":   "webhook",
		"webhook_url":    "https://example.com/telegram/webhook",
		"webhook_listen": "127.0.0.1:0",
		"webhook_secret": "test-secret",
	}))
	require.NoError(t, b.Subscribe("scion.project.proj-1.>"))
	between()
	return b.Configure(map[string]string{
		"bot_token":    newToken,
		"api_base_url": f.srv.URL,
		"hub_url":      "http://hub.test",
		"db_path":      filepath.Join(t.TempDir(), "test.db"),
	})
}

// A reconfigure that switches to a different bot and from webhook to poll
// mode removes the old bot's webhook through the old bot's client.
func TestV2_Reconfigure_WebhookToPollWithOtherBotDeletesOldWebhook(t *testing.T) {
	f := newMultiBotTelegram(t, map[string]int64{"bot-a-token": 100, "bot-b-token": 200})
	b := NewV2(slog.Default())
	defer b.Close()

	require.NoError(t, switchBotAndMode(t, b, f, "bot-a-token", "bot-b-token", func() {}))

	assert.Equal(t, 1, f.deleteWebhookCalls("bot-a-token"),
		"the old bot's webhook must be removed through the old bot's client")
	assert.Equal(t, 0, f.deleteWebhookCalls("bot-b-token"),
		"the new bot has no webhook registration to remove")
	running, _ := pollState(b)
	assert.True(t, running, "poll mode must poll for the existing subscriptions")
}

// A reconfigure that rotates the credential of the same bot (the old one is
// revoked) and switches from webhook to poll mode removes the webhook
// through the new credential and succeeds.
func TestV2_Reconfigure_WebhookToPollWithRotatedCredentialDeletesViaNewClient(t *testing.T) {
	f := newMultiBotTelegram(t, map[string]int64{"old-token": 100, "new-token": 100})
	b := NewV2(slog.Default())
	defer b.Close()

	err := switchBotAndMode(t, b, f, "old-token", "new-token", func() { f.reject("old-token") })
	require.NoError(t, err, "a revoked old credential must not fail the reconfigure")

	assert.Equal(t, 1, f.deleteWebhookCalls("new-token"),
		"the same bot's webhook must be removed through the new credential")
	assert.Equal(t, 0, f.deleteWebhookCalls("old-token"),
		"the revoked credential must not be used")
	assert.Equal(t, "poll", b.inboundMode)
	running, _ := pollState(b)
	assert.True(t, running, "poll mode must poll for the existing subscriptions")
}

// When the old bot's deleteWebhook fails on a switch to a different bot, the
// reconfigure still succeeds and logs a warning: the old bot's webhook does
// not block the new bot's polling.
func TestV2_Reconfigure_WebhookToPollWithOtherBotDeleteFailureWarns(t *testing.T) {
	f := newMultiBotTelegram(t, map[string]int64{"bot-a-token": 100, "bot-b-token": 200})
	logs := &syncBuffer{}
	b := NewV2(slog.New(slog.NewTextHandler(logs, nil)))
	defer b.Close()

	err := switchBotAndMode(t, b, f, "bot-a-token", "bot-b-token", func() { f.reject("bot-a-token") })
	require.NoError(t, err, "the old bot's deleteWebhook failure must not fail the reconfigure")

	assert.Equal(t, 1, f.deleteWebhookCalls("bot-a-token"), "the old bot's webhook removal must be attempted")
	assert.Equal(t, 0, f.deleteWebhookCalls("bot-b-token"))
	assert.Equal(t, "poll", b.inboundMode)
	assert.Contains(t, logs.String(), "Failed to delete the previous bot's webhook")
	running, _ := pollState(b)
	assert.True(t, running, "poll mode must poll for the existing subscriptions")
}
