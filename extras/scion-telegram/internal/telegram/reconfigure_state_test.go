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
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/messages"
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

// waitRetired waits until the broker's background retirements have finished,
// failing the test rather than hanging if they do not finish in time.
func waitRetired(t *testing.T, b *TelegramBrokerV2) {
	t.Helper()
	done := make(chan struct{})
	go func() {
		b.retiring.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		assert.Fail(t, "retirements did not finish within 10s")
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
	waitRetired(t, b)
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
	assert.True(t, b.webhookRegistered, "a failed reconfigure must keep the registration")
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

// countingStore counts Close calls on the store it wraps.
type countingStore struct {
	Store
	closes atomic.Int32
}

func (c *countingStore) Close() error {
	c.closes.Add(1)
	return c.Store.Close()
}

// captureCountedStores wraps the SQLite stores Configure opens from now until
// the test ends, so their Close calls can be counted. Stores are appended by
// Configure; read the slice only when no Configure is running.
func captureCountedStores(t *testing.T) *[]*countingStore {
	t.Helper()
	var opened []*countingStore
	orig := openSQLiteStore
	openSQLiteStore = func(dbPath string) (Store, error) {
		s, err := orig(dbPath)
		if err != nil {
			return nil, err
		}
		c := &countingStore{Store: s}
		opened = append(opened, c)
		return c, nil
	}
	t.Cleanup(func() { openSQLiteStore = orig })
	return &opened
}

// Configure after Close fails without opening anything, and the store Close
// closed is not closed again.
func TestV2_Configure_AfterCloseFails(t *testing.T) {
	tgSrv := newFakeTGServerV2(t)
	stores := captureCountedStores(t)
	b := NewV2(slog.Default())

	require.NoError(t, b.Configure(validPollConfig(t, tgSrv)))
	require.Len(t, *stores, 1)
	require.NoError(t, b.Close())

	require.ErrorContains(t, b.Configure(validPollConfig(t, tgSrv)), "closed")
	waitRetired(t, b)
	assert.Len(t, *stores, 1, "Configure after Close must not open a store")
	assert.Equal(t, int32(1), (*stores)[0].closes.Load(), "the store must be closed exactly once")
}

// A Configure racing Close either commits before Close (and Close closes its
// store) or fails; every store is closed exactly once.
func TestV2_Configure_RacingCloseClosesEachStoreOnce(t *testing.T) {
	tgSrv := newFakeTGServerV2(t)
	stores := captureCountedStores(t)
	for i := 0; i < 20; i++ {
		b := NewV2(slog.Default())
		require.NoError(t, b.Configure(validPollConfig(t, tgSrv)))
		cfg := validPollConfig(t, tgSrv)

		configured := make(chan struct{})
		go func() {
			defer close(configured)
			_ = b.Configure(cfg) // an error is expected when Close wins
		}()
		require.NoError(t, b.Close())
		<-configured
		waitRetired(t, b)
	}
	for i, s := range *stores {
		assert.Equal(t, int32(1), s.closes.Load(), "store %d must be closed exactly once", i)
	}
}

// Two reconfigures while one handler of the first configuration runs: the
// handler reads the broker's current store, so neither the first nor the
// second configuration's store may close until it finishes.
func TestV2_Reconfigure_TwiceDuringOneHandler(t *testing.T) {
	tgSrv := newFakeTGServerV2(t)
	b := NewV2(slog.Default())
	defer b.Close()

	require.NoError(t, b.Configure(validPollConfig(t, tgSrv)))
	firstStore := b.store

	var once sync.Once
	acquired := b.acquireGen()
	release := func() { once.Do(acquired) }
	defer release()

	require.NoError(t, b.Configure(validPollConfig(t, tgSrv)))
	secondStore := b.store
	require.NoError(t, b.Configure(validPollConfig(t, tgSrv)))

	assert.Never(t, func() bool { return storeClosed(firstStore) || storeClosed(secondStore) },
		300*time.Millisecond, 10*time.Millisecond,
		"no store the running handler may read can close while it runs")

	release()
	waitRetired(t, b)
	assert.True(t, storeClosed(firstStore), "the first store must be closed once the handler finishes")
	assert.True(t, storeClosed(secondStore), "the second store must be closed once the handler finishes")
	assert.False(t, storeClosed(b.store), "the current store must stay open")
}

// gatedTelegram forwards to a fake Telegram API, except that it holds the
// first sendMessage call after arm until unblock, and hands a pending update
// to the next getUpdates call.
type gatedTelegram struct {
	srv         *httptest.Server
	armed       atomic.Bool
	entered     chan struct{}
	release     chan struct{}
	releaseOnce sync.Once
	pending     atomic.Pointer[Update]
}

func newGatedTelegram(t *testing.T, tgSrv *fakeTGServerV2) *gatedTelegram {
	t.Helper()
	target, err := url.Parse(tgSrv.srv.URL)
	require.NoError(t, err)
	proxy := httputil.NewSingleHostReverseProxy(target)

	g := &gatedTelegram{entered: make(chan struct{}), release: make(chan struct{})}
	g.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/sendMessage") && g.armed.CompareAndSwap(true, false):
			close(g.entered)
			select {
			case <-g.release:
			case <-r.Context().Done():
				return
			}
		case strings.HasSuffix(r.URL.Path, "/getUpdates"):
			updates := []Update{}
			if u := g.pending.Swap(nil); u != nil {
				updates = append(updates, *u)
			} else {
				select {
				case <-time.After(20 * time.Millisecond):
				case <-r.Context().Done():
					return
				}
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(apiResponse{OK: true, Result: mustJSONRawV2(t, updates)})
			return
		}
		proxy.ServeHTTP(w, r)
	}))
	t.Cleanup(func() {
		g.unblock()
		g.srv.Close()
	})
	return g
}

// unblock releases a held sendMessage call; it may be called more than once.
func (g *gatedTelegram) unblock() { g.releaseOnce.Do(func() { close(g.release) }) }

// waitHeld waits until a sendMessage call is being held.
func (g *gatedTelegram) waitHeld(t *testing.T) {
	t.Helper()
	select {
	case <-g.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("no sendMessage call reached the gate")
	}
}

// dmUpdate is a direct message, which the broker answers with sendMessage.
func dmUpdate() Update {
	return Update{UpdateID: 7, Message: &TGMessage{
		MessageID: 1,
		From:      &TGUser{ID: 42, FirstName: "Alice"},
		Chat:      TGChat{ID: 42, Type: "private"},
		Text:      "hello",
	}}
}

// assertKeptUntilReleased checks that the store and send queue stay open
// while the gated call is held and close after it is released and finish
// has returned.
func assertKeptUntilReleased(t *testing.T, g *gatedTelegram, store Store, sq *SendQueue, finish func()) {
	t.Helper()
	assert.Never(t, func() bool { return storeClosed(store) || sendQueueClosed(sq) },
		300*time.Millisecond, 10*time.Millisecond,
		"the previous store and send queue must stay open while the handler runs")
	g.unblock()
	finish()
	assert.Eventually(t, func() bool { return storeClosed(store) && sendQueueClosed(sq) },
		5*time.Second, 10*time.Millisecond,
		"the previous store and send queue must close once the handler finishes")
}

// A webhook update being handled during a reconfigure keeps the previous
// store and send queue open until it finishes.
func TestV2_Reconfigure_WaitsForWebhookHandler(t *testing.T) {
	tgSrv := newFakeTGServerV2(t)
	g := newGatedTelegram(t, tgSrv)
	b := NewV2(slog.Default())
	defer b.Close()
	defer g.unblock()

	cfg := validWebhookConfig(t, tgSrv)
	cfg["api_base_url"] = g.srv.URL
	require.NoError(t, b.Configure(cfg))
	oldStore, oldQueue := b.store, b.sendQueue
	addr := b.webhookServer.actualAddr

	body, err := json.Marshal(dmUpdate())
	require.NoError(t, err)
	g.armed.Store(true)
	posted := make(chan struct{})
	go func() {
		defer close(posted)
		req, err := http.NewRequest(http.MethodPost, "http://"+addr+webhookPath, strings.NewReader(string(body)))
		if err != nil {
			return
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set(secretTokenHeader, "test-secret")
		if resp, err := http.DefaultClient.Do(req); err == nil {
			resp.Body.Close()
		}
	}()
	g.waitHeld(t)

	require.NoError(t, b.Configure(cfg)) // same listen address: the server is kept
	assertKeptUntilReleased(t, g, oldStore, oldQueue, func() { <-posted })
}

// A polled update being handled during a reconfigure keeps the previous
// store and send queue open until it finishes.
func TestV2_Reconfigure_WaitsForPolledHandler(t *testing.T) {
	tgSrv := newFakeTGServerV2(t)
	g := newGatedTelegram(t, tgSrv)
	b := NewV2(slog.Default())
	defer b.Close()
	defer g.unblock()

	cfg := validPollConfig(t, tgSrv)
	cfg["api_base_url"] = g.srv.URL
	require.NoError(t, b.Configure(cfg))
	oldStore, oldQueue := b.store, b.sendQueue

	u := dmUpdate()
	g.pending.Store(&u)
	g.armed.Store(true)
	require.NoError(t, b.Subscribe("scion.project.proj-1.>"))
	g.waitHeld(t)

	require.NoError(t, b.Configure(cfg))
	assertKeptUntilReleased(t, g, oldStore, oldQueue, func() {})
}

// A Publish in progress during a reconfigure keeps the previous store and
// send queue open until it finishes, and its message is sent.
func TestV2_Reconfigure_WaitsForPublish(t *testing.T) {
	tgSrv := newFakeTGServerV2(t)
	g := newGatedTelegram(t, tgSrv)
	b := NewV2(slog.Default())
	defer b.Close()
	defer g.unblock()

	cfg := validPollConfig(t, tgSrv)
	cfg["api_base_url"] = g.srv.URL
	require.NoError(t, b.Configure(cfg))
	oldStore, oldQueue := b.store, b.sendQueue

	msg := messages.NewInstruction("user:alice", "agent:coder", "hello from hub")
	msg.Metadata = map[string]string{"telegram_chat_id": "-300"}
	g.armed.Store(true)
	published := make(chan error, 1)
	go func() {
		published <- b.Publish(context.Background(), "scion.project.proj-1.agent.coder.messages", msg)
	}()
	g.waitHeld(t)

	require.NoError(t, b.Configure(cfg))
	assertKeptUntilReleased(t, g, oldStore, oldQueue, func() {
		select {
		case err := <-published:
			assert.NoError(t, err)
		case <-time.After(5 * time.Second):
			t.Error("Publish did not return after the gate was released")
		}
	})
	sent := tgSrv.getSentMessages()
	require.Len(t, sent, 1)
	assert.Contains(t, sent[0].Text, "hello from hub")
}

// syncBuffer is a bytes.Buffer safe for concurrent writes and reads.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.String()
}

// A retirement that waits for a long-running handler logs a warning and
// keeps waiting; it closes nothing until the handler finishes.
func TestV2_Reconfigure_WarnsWhileWaitingForHandlers(t *testing.T) {
	orig := handlerWaitWarnAfter
	handlerWaitWarnAfter = 20 * time.Millisecond
	t.Cleanup(func() { handlerWaitWarnAfter = orig })

	logs := &syncBuffer{}
	tgSrv := newFakeTGServerV2(t)
	b := NewV2(slog.New(slog.NewTextHandler(logs, nil)))
	defer b.Close()

	require.NoError(t, b.Configure(validPollConfig(t, tgSrv)))
	oldStore := b.store

	var once sync.Once
	acquired := b.acquireGen()
	release := func() { once.Do(acquired) }
	defer release()

	require.NoError(t, b.Configure(validPollConfig(t, tgSrv)))
	assert.Eventually(t, func() bool {
		return strings.Contains(logs.String(), "Still waiting for running handlers")
	}, 5*time.Second, 10*time.Millisecond, "a long wait for handlers must log a warning")
	assert.False(t, storeClosed(oldStore), "the wait must not close the store early")

	release()
	waitRetired(t, b)
	assert.True(t, storeClosed(oldStore))
}
