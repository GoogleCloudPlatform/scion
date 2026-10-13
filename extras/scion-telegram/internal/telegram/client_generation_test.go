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
	"encoding/json"
	"errors"
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

// Tests for the API client and update offset across reconfigures: handlers
// keep the client of the configuration they started under, and the update
// offset is kept per bot.

// genTelegram is a fake Telegram API for several bot credentials. Each
// credential maps to a bot ID; several credentials may belong to one bot.
// Pending updates are kept per bot and confirmed like Telegram does: a
// getUpdates call with offset N drops the bot's updates with an ID below N.
// It records, per credential, the sendMessage chat IDs and the getUpdates
// offsets.
type genTelegram struct {
	srv *httptest.Server

	mu      sync.Mutex
	botIDs  map[string]int64
	pending map[int64][]Update // bot ID → unconfirmed updates, in ID order
	sent    map[string][]int64 // credential → chat ID of each sendMessage
	offsets map[string][]int64 // credential → offset of each getUpdates
}

func newGenTelegram(t *testing.T, botIDs map[string]int64) *genTelegram {
	t.Helper()
	f := &genTelegram{botIDs: botIDs, pending: make(map[int64][]Update),
		sent: make(map[string][]int64), offsets: make(map[string][]int64)}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		token, method, ok := strings.Cut(strings.TrimPrefix(r.URL.Path, "/bot"), "/")
		f.mu.Lock()
		botID, known := f.botIDs[token]
		f.mu.Unlock()
		if !ok || !known {
			w.WriteHeader(http.StatusUnauthorized)
			_ = json.NewEncoder(w).Encode(apiResponse{OK: false, ErrorCode: 401, Description: "Unauthorized"})
			return
		}
		var result any = true
		switch method {
		case "getMe":
			result = BotUser{ID: botID, IsBot: true, FirstName: "TestBot", Username: "test_bot"}
		case "sendMessage":
			var req struct {
				ChatID int64 `json:"chat_id"`
			}
			_ = json.NewDecoder(r.Body).Decode(&req)
			f.mu.Lock()
			f.sent[token] = append(f.sent[token], req.ChatID)
			f.mu.Unlock()
			result = TGMessage{MessageID: 1, Chat: TGChat{ID: req.ChatID}}
		case "getUpdates":
			var req getUpdatesRequest
			_ = json.NewDecoder(r.Body).Decode(&req)
			f.mu.Lock()
			f.offsets[token] = append(f.offsets[token], req.Offset)
			var kept []Update
			for _, u := range f.pending[botID] {
				if u.UpdateID >= req.Offset {
					kept = append(kept, u)
				}
			}
			f.pending[botID] = kept
			f.mu.Unlock()
			if len(kept) == 0 {
				select {
				case <-time.After(20 * time.Millisecond):
				case <-r.Context().Done():
					return
				}
				kept = []Update{}
			}
			result = kept
		}
		_ = json.NewEncoder(w).Encode(apiResponse{OK: true, Result: mustJSONRawV2(t, result)})
	}))
	t.Cleanup(f.srv.Close)
	return f
}

// queue adds an unconfirmed update for botID.
func (f *genTelegram) queue(botID int64, u Update) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.pending[botID] = append(f.pending[botID], u)
}

// sentTo returns the chat IDs of the sendMessage calls made with token.
func (f *genTelegram) sentTo(token string) []int64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]int64(nil), f.sent[token]...)
}

// offsetsOf returns the offsets of the getUpdates calls made with token.
func (f *genTelegram) offsetsOf(token string) []int64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]int64(nil), f.offsets[token]...)
}

func genPollConfig(t *testing.T, f *genTelegram, token string) map[string]string {
	return map[string]string{
		"bot_token":    token,
		"api_base_url": f.srv.URL,
		"hub_url":      "http://hub.test",
		"db_path":      filepath.Join(t.TempDir(), "test.db"),
	}
}

func genWebhookConfig(t *testing.T, f *genTelegram, token string) map[string]string {
	c := genPollConfig(t, f, token)
	c["inbound_mode"] = "webhook"
	c["webhook_url"] = "https://example.com/telegram/webhook"
	c["webhook_listen"] = "127.0.0.1:0"
	c["webhook_secret"] = "test-secret"
	return c
}

// gatedHub is a fake hub whose ListAgents signals entered, then waits for
// gate and fails, so a group message handler is held before it replies.
type gatedHub struct {
	*fakeHubClient
	entered chan struct{}
	gate    chan struct{}
}

func (h *gatedHub) ListAgents(ctx context.Context, _, _ string) ([]AgentInfo, error) {
	h.entered <- struct{}{}
	select {
	case <-h.gate:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	return nil, errors.New("list agents returned status 500")
}

// holdGroupHandler prepares b so that a message from user 456 in group -200
// is held in the hub call until the returned gate is closed; the handler
// then replies "Couldn't fetch the agent list" through its API client.
func holdGroupHandler(t *testing.T, b *TelegramBrokerV2) (entered, gate chan struct{}) {
	t.Helper()
	hub := &gatedHub{fakeHubClient: newFakeHubClient(),
		entered: make(chan struct{}, 1), gate: make(chan struct{})}
	b.mu.Lock()
	b.hubClient = hub
	store := b.store
	b.mu.Unlock()
	saveTestGroupLink(t, store, -200, "proj-1", "my-project", "coder")
	linkTestUser(t, store, 456, "alice@example.com")
	return hub.entered, hub.gate
}

func waitSignal(t *testing.T, ch <-chan struct{}, what string) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(5 * time.Second):
		require.FailNow(t, "timed out waiting for "+what)
	}
}

// A webhook handler that starts under one configuration replies through
// that configuration's client after a reconfigure to another bot.
func TestV2_Reconfigure_WebhookHandlerKeepsItsClient(t *testing.T) {
	f := newGenTelegram(t, map[string]int64{"bot-a-token": 100, "bot-b-token": 200})
	b := NewV2(slog.Default())
	defer b.Close()
	require.NoError(t, b.Configure(genWebhookConfig(t, f, "bot-a-token")))
	entered, gate := holdGroupHandler(t, b)

	handled := make(chan struct{})
	go func() {
		defer close(handled)
		b.handleWebhookUpdate(Update{UpdateID: 1, Message: plainGroupMessage(456, "hi")})
	}()
	waitSignal(t, entered, "the webhook handler to reach the hub")

	require.NoError(t, b.Configure(genWebhookConfig(t, f, "bot-b-token")))
	close(gate)
	waitSignal(t, handled, "the webhook handler to finish")

	assert.Equal(t, []int64{-200}, f.sentTo("bot-a-token"),
		"the handler must reply through the client of the configuration it started under")
	assert.Empty(t, f.sentTo("bot-b-token"),
		"the handler must not switch to the new configuration's client")
}

// A polled update's handler that starts under one configuration replies
// through that configuration's client after a reconfigure to another bot.
func TestV2_Reconfigure_PolledHandlerKeepsItsClient(t *testing.T) {
	f := newGenTelegram(t, map[string]int64{"bot-a-token": 100, "bot-b-token": 200})
	b := NewV2(slog.Default())
	defer b.Close()
	require.NoError(t, b.Configure(genPollConfig(t, f, "bot-a-token")))
	entered, gate := holdGroupHandler(t, b)
	f.queue(100, Update{UpdateID: 10, Message: plainGroupMessage(456, "hi")})
	require.NoError(t, b.Subscribe("scion.project.proj-1.>"))
	waitSignal(t, entered, "the polled handler to reach the hub")
	_, oldDone := pollState(b)

	require.NoError(t, b.Configure(genPollConfig(t, f, "bot-b-token")))
	close(gate)
	waitSignal(t, oldDone, "the old poll loop to exit")

	assert.Equal(t, []int64{-200}, f.sentTo("bot-a-token"),
		"the handler must reply through the client that polled the update")
	assert.Empty(t, f.sentTo("bot-b-token"),
		"the handler must not switch to the new configuration's client")
}

// dmUpdateFrom is a private-chat message; the broker answers it with a help
// text through the client of the configuration handling it.
func dmUpdateFrom(id, chatID int64) Update {
	return Update{UpdateID: id, Message: &TGMessage{MessageID: id, From: &TGUser{ID: chatID},
		Chat: TGChat{ID: chatID, Type: "private"}, Text: "hello"}}
}

// A reconfigure to a bot not polled before starts that bot's update offset
// from the beginning: its first getUpdates does not carry the old bot's
// offset, and its pending update (with an ID below that offset) is handled.
func TestV2_Reconfigure_OtherBotResetsUpdateOffset(t *testing.T) {
	f := newGenTelegram(t, map[string]int64{"bot-a-token": 100, "bot-b-token": 200})
	b := NewV2(slog.Default())
	defer b.Close()
	require.NoError(t, b.Configure(genPollConfig(t, f, "bot-a-token")))
	f.queue(100, dmUpdateFrom(500, 41))
	require.NoError(t, b.Subscribe("scion.project.proj-1.>"))
	require.Eventually(t, func() bool { return len(f.sentTo("bot-a-token")) == 1 },
		5*time.Second, 10*time.Millisecond, "bot A's update must be handled")

	f.queue(200, dmUpdateFrom(7, 42))
	require.NoError(t, b.Configure(genPollConfig(t, f, "bot-b-token")))

	require.Eventually(t, func() bool { return len(f.offsetsOf("bot-b-token")) > 0 },
		5*time.Second, 10*time.Millisecond, "the broker must poll bot B")
	assert.Equal(t, int64(1), f.offsetsOf("bot-b-token")[0],
		"bot B's first getUpdates must not carry bot A's offset")
	require.Eventually(t, func() bool { return len(f.sentTo("bot-b-token")) == 1 },
		5*time.Second, 10*time.Millisecond, "bot B's pending update must not be skipped")
	assert.Equal(t, []int64{42}, f.sentTo("bot-b-token"))
	assert.Equal(t, []int64{41}, f.sentTo("bot-a-token"))
}

// A reconfigure that rotates the same bot's credential keeps the update
// offset: the handled update is not fetched again, and the next one is
// handled once.
func TestV2_Reconfigure_SameBotKeepsUpdateOffset(t *testing.T) {
	f := newGenTelegram(t, map[string]int64{"old-token": 100, "new-token": 100})
	b := NewV2(slog.Default())
	defer b.Close()
	require.NoError(t, b.Configure(genPollConfig(t, f, "old-token")))
	f.queue(100, dmUpdateFrom(500, 41))
	require.NoError(t, b.Subscribe("scion.project.proj-1.>"))
	require.Eventually(t, func() bool { return len(f.sentTo("old-token")) == 1 },
		5*time.Second, 10*time.Millisecond, "the update must be handled")

	require.NoError(t, b.Configure(genPollConfig(t, f, "new-token")))
	require.Eventually(t, func() bool { return len(f.offsetsOf("new-token")) > 0 },
		5*time.Second, 10*time.Millisecond, "the broker must poll with the new credential")
	assert.Equal(t, int64(501), f.offsetsOf("new-token")[0],
		"the same bot's offset must carry over to the new credential")

	f.queue(100, dmUpdateFrom(501, 43))
	require.Eventually(t, func() bool { return len(f.sentTo("new-token")) == 1 },
		5*time.Second, 10*time.Millisecond, "the next update must be handled")
	assert.Equal(t, []int64{43}, f.sentTo("new-token"),
		"the handled update must not be fetched again")
	assert.Equal(t, []int64{41}, f.sentTo("old-token"))
}

// Switching from bot A to bot B and back resumes A from A's own offset. A's
// last handled update, which the switch left unconfirmed at Telegram, is not
// handled again.
func TestV2_Reconfigure_BackToEarlierBotResumesItsOffset(t *testing.T) {
	f := newGenTelegram(t, map[string]int64{"bot-a-token": 100, "bot-b-token": 200})
	b := NewV2(slog.Default())
	defer b.Close()
	require.NoError(t, b.Configure(genPollConfig(t, f, "bot-a-token")))
	entered, gate := holdGroupHandler(t, b)
	f.queue(100, Update{UpdateID: 500, Message: plainGroupMessage(456, "hi")})
	require.NoError(t, b.Subscribe("scion.project.proj-1.>"))
	waitSignal(t, entered, "bot A's handler to reach the hub")
	_, aDone := pollState(b)

	// Switch to bot B while update 500 is being handled. A's loop exits
	// after the handler without polling again, so Telegram still holds 500
	// as unconfirmed.
	require.NoError(t, b.Configure(genPollConfig(t, f, "bot-b-token")))
	close(gate)
	waitSignal(t, aDone, "bot A's poll loop to exit")
	require.Equal(t, []int64{-200}, f.sentTo("bot-a-token"), "update 500 must be handled once")
	aPolls := len(f.offsetsOf("bot-a-token"))
	require.Eventually(t, func() bool { return len(f.offsetsOf("bot-b-token")) > 0 },
		5*time.Second, 10*time.Millisecond, "the broker must poll bot B")

	require.NoError(t, b.Configure(genPollConfig(t, f, "bot-a-token")))
	require.Eventually(t, func() bool { return len(f.offsetsOf("bot-a-token")) > aPolls },
		5*time.Second, 10*time.Millisecond, "the broker must poll bot A again")
	assert.Equal(t, int64(501), f.offsetsOf("bot-a-token")[aPolls],
		"bot A must resume from its own offset")

	f.queue(100, dmUpdateFrom(501, 43))
	require.Eventually(t, func() bool { return len(f.sentTo("bot-a-token")) >= 2 },
		5*time.Second, 10*time.Millisecond, "bot A's next update must be handled")
	assert.Equal(t, []int64{-200, 43}, f.sentTo("bot-a-token"),
		"update 500 must not be handled again")
	assert.Empty(t, f.sentTo("bot-b-token"))
}
