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
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/transportauth"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Tests for the resources a successful reconfigure replaces.

// validPollConfig returns a poll-mode config against the fake API.
func validPollConfig(t *testing.T, tgSrv *fakeTGServerV2) map[string]string {
	t.Helper()
	return map[string]string{
		"bot_token":    "test-token",
		"api_base_url": tgSrv.srv.URL,
		"hub_url":      "http://hub.test",
		"db_path":      filepath.Join(t.TempDir(), "test.db"),
	}
}

// storeClosed reports whether s rejects a write because it is closed.
func storeClosed(s Store) bool {
	link := &GroupLink{ChatID: -400, ProjectID: "proj-1", LinkedAt: time.Now().UTC(), Active: true}
	return s.SaveGroupLink(context.Background(), link) != nil
}

// sendQueueClosed reports whether sq has been closed.
func sendQueueClosed(sq *SendQueue) bool {
	sq.mu.Lock()
	defer sq.mu.Unlock()
	return sq.closed
}

// A successful reconfigure closes the store and send queue it replaced.
func TestV2_Reconfigure_ClosesPreviousStoreAndSendQueue(t *testing.T) {
	tgSrv := newFakeTGServerV2(t)
	b := NewV2(slog.Default())
	defer b.Close()

	require.NoError(t, b.Configure(validPollConfig(t, tgSrv)))
	oldStore, oldQueue := b.store, b.sendQueue

	require.NoError(t, b.Configure(validPollConfig(t, tgSrv)))
	require.NotSame(t, oldQueue, b.sendQueue)

	assert.Eventually(t, func() bool { return storeClosed(oldStore) }, 5*time.Second, 10*time.Millisecond,
		"the previous store must be closed after a reconfigure")
	assert.Eventually(t, func() bool { return sendQueueClosed(oldQueue) }, 5*time.Second, 10*time.Millisecond,
		"the previous send queue must be closed after a reconfigure")
	assert.False(t, storeClosed(b.store), "the current store must stay open")
	assert.False(t, sendQueueClosed(b.sendQueue), "the current send queue must stay open")
}

// A handler that started under the previous configuration keeps a working
// store and send queue until it finishes; only then are they closed. A
// message it queues on the previous send queue is sent.
func TestV2_Reconfigure_WaitsForPreviousHandlers(t *testing.T) {
	tgSrv := newFakeTGServerV2(t)
	b := NewV2(slog.Default())
	defer b.Close()

	require.NoError(t, b.Configure(validPollConfig(t, tgSrv)))
	oldStore, oldQueue := b.store, b.sendQueue

	// A handler in flight under the first config. Released at the latest
	// before b.Close, which would otherwise wait for it.
	var once sync.Once
	acquired := b.acquireGen()
	release := func() { once.Do(acquired) }
	defer release()
	require.NoError(t, b.Configure(validPollConfig(t, tgSrv)))

	assert.Never(t, func() bool { return storeClosed(oldStore) || sendQueueClosed(oldQueue) },
		300*time.Millisecond, 10*time.Millisecond,
		"the previous store and send queue must stay open while a previous handler runs")

	_, err := oldQueue.Send(context.Background(), -500, "queued by a previous handler", "", nil, 0)
	require.NoError(t, err, "a previous handler's send must be delivered")
	tgSrv.mu.Lock()
	require.NotEmpty(t, tgSrv.sentMessages)
	assert.Equal(t, "queued by a previous handler", tgSrv.sentMessages[len(tgSrv.sentMessages)-1].Text)
	tgSrv.mu.Unlock()

	release()
	b.retiring.Wait()
	assert.True(t, storeClosed(oldStore), "the previous store must be closed once its handlers finish")
	assert.True(t, sendQueueClosed(oldQueue), "the previous send queue must be closed once its handlers finish")
}

// Switching from webhook mode to poll mode stops the webhook server.
func TestV2_Reconfigure_WebhookToPollStopsServer(t *testing.T) {
	tgSrv := newFakeTGServerV2(t)
	b := NewV2(slog.Default())
	defer b.Close()

	require.NoError(t, b.Configure(validWebhookConfig(t, tgSrv)))
	require.NotNil(t, b.webhookServer)
	addr := b.webhookServer.actualAddr
	status, err := postWebhookUpdate(t, addr, "test-secret")
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, status)

	require.NoError(t, b.Configure(validPollConfig(t, tgSrv)))
	assert.Equal(t, "poll", b.inboundMode)
	assert.Nil(t, b.webhookServer, "poll mode must not keep a webhook server")
	assert.Eventually(t, func() bool {
		_, err := postWebhookUpdate(t, addr, "test-secret")
		return err != nil
	}, 5*time.Second, 10*time.Millisecond, "the webhook server must be stopped in poll mode")
}

// countingTokenSource counts Token calls; each transport-auth wrapper calls
// it once per request in serverless mode.
type countingTokenSource struct{ calls atomic.Int32 }

func (s *countingTokenSource) Token() (string, error) {
	s.calls.Add(1)
	return "test-token", nil
}

func (s *countingTokenSource) SetToken(string, time.Time) {}

func (s *countingTokenSource) Expiry() time.Time { return time.Now().Add(time.Hour) }

// The HTTP transport is wrapped with transport auth once, however many
// reconfigures happen.
func TestV2_Reconfigure_WrapsTransportOnce(t *testing.T) {
	src := &countingTokenSource{}
	orig := resolveBrokerTransport
	resolveBrokerTransport = func(_, _ string, _ transportauth.ADCSourceConstructor) (transportauth.TokenSource, transportauth.HeaderMode, error) {
		return src, transportauth.HeaderServerlessAuthorization, nil
	}
	t.Cleanup(func() { resolveBrokerTransport = orig })

	var gotHeader atomic.Value
	hub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotHeader.Store(r.Header.Get("X-Serverless-Authorization"))
	}))
	defer hub.Close()

	tgSrv := newFakeTGServerV2(t)
	b := NewV2(slog.Default())
	defer b.Close()

	cfg := validPollConfig(t, tgSrv)
	cfg["transport_mode"] = "serverless"
	cfg["transport_audience"] = "https://hub.test"
	for i := 0; i < 3; i++ {
		require.NoError(t, b.Configure(cfg))
	}

	src.calls.Store(0)
	resp, err := b.httpClient.Get(hub.URL)
	require.NoError(t, err)
	resp.Body.Close()

	assert.Equal(t, "Bearer test-token", gotHeader.Load())
	assert.Equal(t, int32(1), src.calls.Load(),
		"the transport must be wrapped once after three configures")
}

// RegisterWebhook registers the webhook URL and credential of the current
// configuration. While this instance holds the registration, a reconfigure
// registers its new settings, and a failed one changes nothing; after
// DeregisterWebhook a reconfigure does not register.
func TestV2_RegisterWebhook_UsesCurrentSettings(t *testing.T) {
	tgSrv := newFakeTGServerV2(t)
	b := NewV2(slog.Default())
	defer b.Close()

	registered := func() (string, string) {
		tgSrv.mu.Lock()
		defer tgSrv.mu.Unlock()
		return tgSrv.webhookURL, tgSrv.webhookSecret
	}

	cfg := validWebhookConfig(t, tgSrv)
	cfg["skip_set_webhook"] = "true"
	require.NoError(t, b.Configure(cfg))
	url, _ := registered()
	require.Empty(t, url, "Configure must not register with skip_set_webhook")

	require.NoError(t, b.RegisterWebhook())
	url, secret := registered()
	assert.Equal(t, "https://example.com/telegram/webhook", url)
	assert.Equal(t, "test-secret", secret)

	// The lock holder's reconfigure registers the rotated settings.
	cfg["webhook_url"] = "https://example.com/telegram/rotated"
	cfg["webhook_secret"] = "rotated-secret"
	require.NoError(t, b.Configure(cfg))
	url, secret = registered()
	assert.Equal(t, "https://example.com/telegram/rotated", url)
	assert.Equal(t, "rotated-secret", secret)

	// A reconfigure whose registration fails changes nothing.
	failing := make(map[string]string, len(cfg))
	for k, v := range cfg {
		failing[k] = v
	}
	failing["webhook_secret"] = "unregistered-secret"
	tgSrv.mu.Lock()
	tgSrv.failSetWebhook = true
	tgSrv.mu.Unlock()
	require.ErrorContains(t, b.Configure(failing), "failed to set webhook")
	tgSrv.mu.Lock()
	tgSrv.failSetWebhook = false
	tgSrv.mu.Unlock()
	assert.Equal(t, "rotated-secret", b.webhookSecret)
	status, err := postWebhookUpdate(t, b.webhookServer.actualAddr, "rotated-secret")
	require.NoError(t, err)
	assert.Equal(t, http.StatusOK, status, "a failed reconfigure must keep the registered credential")

	// After deregistering, a reconfigure does not register; RegisterWebhook
	// then registers the current settings.
	require.NoError(t, b.DeregisterWebhook())
	cfg["webhook_url"] = "https://example.com/telegram/standby"
	cfg["webhook_secret"] = "standby-secret"
	require.NoError(t, b.Configure(cfg))
	url, _ = registered()
	assert.Empty(t, url, "a reconfigure without the registration must not register")

	require.NoError(t, b.RegisterWebhook())
	url, secret = registered()
	assert.Equal(t, "https://example.com/telegram/standby", url)
	assert.Equal(t, "standby-secret", secret)
}

// Messages already queued when a send queue is closed are still sent; a
// reconfigure relies on this when it closes the previous send queue.
func TestSendQueue_CloseSendsQueuedMessages(t *testing.T) {
	tgSrv := newFakeTGServerV2(t)
	api := NewAPIClient("test-token", tgSrv.srv.URL)
	sq := NewSendQueue(api, slog.Default(), 10, 20*time.Millisecond)

	results := make([]chan *sendResult, 3)
	for i := range results {
		results[i] = make(chan *sendResult, 1)
		_, err := sq.enqueue(-600, &outboundMessage{chatID: -600, text: "queued", result: results[i]})
		require.NoError(t, err)
	}
	sq.Close()

	for i, ch := range results {
		select {
		case res := <-ch:
			assert.NoError(t, res.err, "queued message %d", i)
		default:
			t.Fatalf("queued message %d has no result after Close", i)
		}
	}
	tgSrv.mu.Lock()
	assert.Len(t, tgSrv.sentMessages, 3)
	tgSrv.mu.Unlock()
}
