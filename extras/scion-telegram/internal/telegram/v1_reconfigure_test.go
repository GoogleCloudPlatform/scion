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
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/messages"
	"github.com/GoogleCloudPlatform/scion/pkg/transportauth"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Tests for reconfiguring the v1 broker and restarting its poll loop.

// v1PollServer is a fake Telegram Bot API for the v1 poll loop. The first
// getUpdates returns one text message; every later one long-polls until the
// client cancels the request, and is reported on polls.
type v1PollServer struct {
	srv   *httptest.Server
	polls chan struct{}

	mu    sync.Mutex
	calls int
}

func newV1PollServer(t *testing.T) *v1PollServer {
	t.Helper()
	f := &v1PollServer{polls: make(chan struct{}, 16)}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/bottest-token/getMe":
			_ = json.NewEncoder(w).Encode(apiResponse{OK: true, Result: mustJSONRaw(t, BotUser{
				ID: 100, IsBot: true, FirstName: "TestBot", Username: "test_bot",
			})})
		case "/bottest-token/getUpdates":
			f.mu.Lock()
			f.calls++
			first := f.calls == 1
			f.mu.Unlock()
			if first {
				_ = json.NewEncoder(w).Encode(apiResponse{OK: true, Result: mustJSONRaw(t, []Update{{
					UpdateID: 1,
					Message: &TGMessage{
						MessageID: 1,
						From:      &TGUser{ID: 42, Username: "alice"},
						Chat:      TGChat{ID: 7, Type: "private"},
						Text:      "hello",
					},
				}})})
				return
			}
			select {
			case f.polls <- struct{}{}:
			default:
			}
			<-r.Context().Done()
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(f.srv.Close)
	return f
}

// The HTTP transport is wrapped with transport auth once, however many
// reconfigures happen.
func TestV1_Reconfigure_WrapsTransportOnce(t *testing.T) {
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

	tgSrv := newFakeTelegramServer(t)
	b := New(slog.Default())
	defer b.Close()

	cfg := map[string]string{
		"bot_token":          "test-token",
		"api_base_url":       tgSrv.srv.URL,
		"transport_mode":     "serverless",
		"transport_audience": "https://hub.test",
	}
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

// A stop followed by an immediate restart, while the old loop is still
// finishing a message, leaves the new loop running with its own open done
// channel; the stop only waits for the old loop, which has exited.
func TestV1_StopThenRestart_KeepsNewLoop(t *testing.T) {
	const pattern = "test.>"
	tgSrv := newV1PollServer(t)

	entered := make(chan struct{})
	release := make(chan struct{})
	var handled atomic.Int32

	b := New(slog.Default())
	t.Cleanup(func() { b.Close() })
	require.NoError(t, b.Configure(map[string]string{
		"bot_token":    "test-token",
		"api_base_url": tgSrv.srv.URL,
	}))
	b.InboundHandler = func(string, *messages.StructuredMessage) {
		if handled.Add(1) == 1 {
			close(entered)
			<-release
		}
	}

	// Start the old loop and hold it in its message handler.
	require.NoError(t, b.Subscribe(pattern))
	<-entered

	// Learn when the stop has cancelled the old loop.
	cancelled := make(chan struct{})
	b.mu.Lock()
	oldDone := b.pollDone
	cancelOld := b.pollCancel
	b.pollCancel = func() { cancelOld(); close(cancelled) }
	b.mu.Unlock()

	stopped := make(chan struct{})
	go func() {
		defer close(stopped)
		_ = b.Unsubscribe(pattern)
	}()
	<-cancelled

	// Restart while the stop still waits for the old loop.
	require.NoError(t, b.Subscribe(pattern))
	close(release)
	<-stopped

	b.mu.RLock()
	running := b.pollCancel != nil
	newDone := b.pollDone
	b.mu.RUnlock()

	require.True(t, running, "the restarted loop must still be registered after the stop returns")
	require.NotEqual(t, oldDone, newDone, "the restarted loop must own a new done channel")
	select {
	case <-oldDone:
	default:
		t.Fatal("the old loop must have exited when the stop returns")
	}
	select {
	case <-newDone:
		t.Fatal("the restarted loop's done channel must stay open")
	default:
	}

	// The restarted loop polls. The timeout only bounds a failing run.
	select {
	case <-tgSrv.polls:
	case <-time.After(10 * time.Second):
		t.Fatal("the restarted loop must poll for updates")
	}
}
