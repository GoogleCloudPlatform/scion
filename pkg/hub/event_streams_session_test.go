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

//go:build !no_sqlite

package hub

import (
	"bufio"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// openTIEventStream opens an events stream with a test identity web
// session's cookies and returns the response and a channel closed when the
// server ends the stream.
func openTIEventStream(t *testing.T, e *tiWebEnv, jar []*http.Cookie) (*http.Response, <-chan struct{}) {
	t.Helper()
	server := httptest.NewServer(e.ws.Handler())
	t.Cleanup(server.Close)
	req, err := http.NewRequest(http.MethodGet, server.URL+"/events?sub=notification.ti", nil)
	require.NoError(t, err)
	req.Header.Set("Accept", "text/event-stream")
	for _, c := range jar {
		req.AddCookie(c)
	}
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	t.Cleanup(func() { _ = resp.Body.Close() })
	require.Equal(t, http.StatusOK, resp.StatusCode)
	closed := make(chan struct{})
	go func() {
		r := bufio.NewReader(resp.Body)
		for {
			if _, err := r.ReadString('\n'); err != nil {
				close(closed)
				return
			}
		}
	}()
	return resp, closed
}

// The two session_ended close paths of handleSSE (phase 0 review of
// ptone/scion#5628): the heartbeat re-check of a test identity web session
// whose identity was deleted, and the deadline at the identity's expiry.
// Both close the stream with reason session_ended and leave the registry.
func TestEventStream_TestIdentitySessionEndedReasons(t *testing.T) {
	t.Run("heartbeat re-check", func(t *testing.T) {
		e := newTIWebEnv(t)
		logs := &streamLogCapture{}
		e.ws.log = slog.New(slog.NewJSONHandler(logs, &slog.HandlerOptions{Level: slog.LevelInfo}))
		e.ws.events = NewChannelEventPublisher()
		e.ws.config.SSEHeartbeatInterval = 100 * time.Millisecond
		fx := e.issue()
		jar := e.login(fx.Identity.ID)
		_, closed := openTIEventStream(t, e, jar)
		waitForStreamCount(t, e.ws.EventStreams(), 1)

		rec := doRequestWithToken(t, e.srv, e.issuerTok, http.MethodDelete, "/api/v1/test-identities/"+fx.Identity.ID, nil)
		require.Equal(t, http.StatusNoContent, rec.Code)
		select {
		case <-closed:
		case <-time.After(5 * time.Second):
			t.Fatal("stream still open after the identity was deleted")
		}
		waitForStreamCount(t, e.ws.EventStreams(), 0)
		require.Eventually(t, func() bool { return len(logs.closeLines(t)) == 1 }, 5*time.Second, 10*time.Millisecond)
		assert.Equal(t, map[string]int{"session_ended": 1}, closeReasons(logs.closeLines(t)))
	})

	t.Run("identity expiry deadline", func(t *testing.T) {
		e := newTIWebEnv(t)
		logs := &streamLogCapture{}
		e.ws.log = slog.New(slog.NewJSONHandler(logs, &slog.HandlerOptions{Level: slog.LevelInfo}))
		e.ws.events = NewChannelEventPublisher()
		// No heartbeat re-check fires before the deadline does.
		e.ws.config.SSEHeartbeatInterval = time.Minute
		fx := e.issue()
		jar := e.login(fx.Identity.ID)
		// Move the hub clock to shortly before the identity's expiry, so
		// the stream's deadline timer fires in about a second of real time.
		e.clock.Advance(time.Until(fx.Identity.ExpiresAt) - 1500*time.Millisecond)
		start := time.Now()
		_, closed := openTIEventStream(t, e, jar)
		select {
		case <-closed:
		case <-time.After(10 * time.Second):
			t.Fatal("stream still open after the identity's expiry")
		}
		assert.Less(t, time.Since(start), 10*time.Second)
		waitForStreamCount(t, e.ws.EventStreams(), 0)
		require.Eventually(t, func() bool { return len(logs.closeLines(t)) == 1 }, 5*time.Second, 10*time.Millisecond)
		assert.Equal(t, map[string]int{"session_ended": 1}, closeReasons(logs.closeLines(t)))
	})
}
