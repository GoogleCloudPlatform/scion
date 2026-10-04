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
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/conduit"
	"github.com/GoogleCloudPlatform/scion/pkg/conduit/relay"
	scionhub "github.com/GoogleCloudPlatform/scion/pkg/sciontool/hub"
	scionportforward "github.com/GoogleCloudPlatform/scion/pkg/sciontool/portforward"
	conduitv1 "github.com/GoogleCloudPlatform/scion/proto/conduit/v1"
	"github.com/gorilla/websocket"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestConduitProxyWebSocketEcho: a WebSocket upgrade through the port
// proxy reaches the agent's app over the conduit session and echoes both
// ways, on a hub server with a write timeout shorter than the exchange.
func TestConduitProxyWebSocketEcho(t *testing.T) {
	up := websocket.Upgrader{}
	f := newConduitProxyFixture(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := up.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer func() { _ = c.Close() }()
		for {
			mt, msg, err := c.ReadMessage()
			if err != nil {
				return
			}
			if err := c.WriteMessage(mt, append([]byte("echo:"), msg...)); err != nil {
				return
			}
		}
	}))
	f.withWriteTimeout(t, 300*time.Millisecond)
	f.startAgent(t)

	wsURL := "ws" + strings.TrimPrefix(f.base, "http") +
		"/api/v1/agents/" + f.launched.ID + "/ports/" + strconv.Itoa(f.app.port) + "/proxy/ws"
	c, resp, err := websocket.DefaultDialer.Dial(wsURL, http.Header{"Authorization": {"Bearer " + f.userToken}})
	require.NoError(t, err, "upgrade through the proxy")
	t.Cleanup(func() { _ = c.Close() })
	assert.Equal(t, http.StatusSwitchingProtocols, resp.StatusCode)
	assert.False(t, f.srv.portTunnels.has(f.launched.ID), "no legacy tunnel")

	exchange := func(msg string) {
		t.Helper()
		require.NoError(t, c.WriteMessage(websocket.TextMessage, []byte(msg)))
		require.NoError(t, c.SetReadDeadline(time.Now().Add(10*time.Second)))
		_, got, err := c.ReadMessage()
		require.NoError(t, err)
		assert.Equal(t, "echo:"+msg, string(got))
	}
	exchange("one")
	// Outlive the server's write timeout on the same connection.
	idle := time.NewTimer(600 * time.Millisecond)
	<-idle.C
	exchange("two")
}

// sseApp serves n events at /events, each sent after a value arrives on
// next (or every interval when next is nil), flushing after each one.
func sseApp(next <-chan struct{}, interval time.Duration, n int) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		rc := http.NewResponseController(w)
		_ = rc.Flush()
		var tick <-chan time.Time
		if next == nil {
			tk := time.NewTicker(interval)
			defer tk.Stop()
			tick = tk.C
		}
		for i := 1; i <= n; i++ {
			select {
			case <-next:
			case <-tick:
			case <-r.Context().Done():
				return
			}
			if _, err := fmt.Fprintf(w, "data: %d\n\n", i); err != nil {
				return
			}
			if err := rc.Flush(); err != nil {
				return
			}
		}
	})
}

// readEvents reads n SSE data lines from body, calling each(i) after
// event i arrives.
func readEvents(t *testing.T, body io.Reader, n int, each func(int)) {
	t.Helper()
	sc := bufio.NewScanner(body)
	for i := 1; i <= n; {
		require.True(t, sc.Scan(), "event stream ended before event %d: %v", i, sc.Err())
		line := sc.Text()
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		require.Equal(t, "data: "+strconv.Itoa(i), line)
		each(i)
		i++
	}
}

// TestConduitProxyEventStream: an event stream through the proxy is
// delivered event by event (event i arrives before the app writes event
// i+1, so nothing is buffered) and keeps flowing past the hub server's
// write timeout, which on the tunnel path would end it.
func TestConduitProxyEventStream(t *testing.T) {
	next := make(chan struct{})
	f := newConduitProxyFixture(t, sseApp(next, 0, 4))
	f.withWriteTimeout(t, 300*time.Millisecond)
	f.startAgent(t)

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		f.base+"/api/v1/agents/"+f.launched.ID+"/ports/"+strconv.Itoa(f.app.port)+"/proxy/events", nil)
	require.NoError(t, err)
	req.Header.Set("Authorization", "Bearer "+f.userToken)
	req.Header.Set("Accept", "text/event-stream")
	go func() { next <- struct{}{} }()
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	t.Cleanup(func() { _ = resp.Body.Close() })
	require.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Equal(t, "text/event-stream", resp.Header.Get("Content-Type"))

	readEvents(t, resp.Body, 4, func(i int) {
		if i == 2 {
			// Outlive the server's write timeout mid-stream.
			idle := time.NewTimer(600 * time.Millisecond)
			<-idle.C
		}
		if i < 4 {
			next <- struct{}{}
		}
	})
}

// TestConduitProxyEventStreamSoak: an event stream runs for more than the
// tunnel path's 60s timeout on a hub server with the production 60s write
// timeout. It takes over a minute, so it runs only with
// SCION_CONDUIT_SOAK=1.
func TestConduitProxyEventStreamSoak(t *testing.T) {
	if os.Getenv("SCION_CONDUIT_SOAK") != "1" {
		t.Skip("set SCION_CONDUIT_SOAK=1 to run the 65s event-stream soak")
	}
	const events = 13 // one every 5s: 65s
	f := newConduitProxyFixture(t, sseApp(nil, 5*time.Second, events))
	f.withWriteTimeout(t, portForwardTimeout)
	f.startAgent(t)

	start := time.Now()
	resp := f.get(t, "/events", http.Header{"Accept": {"text/event-stream"}})
	require.Equal(t, http.StatusOK, resp.StatusCode)
	readEvents(t, resp.Body, events, func(int) {})
	assert.Greater(t, time.Since(start), portForwardTimeout)
}

// TestConduitProxyVersionMatrix: old and new sciontool against a hub with
// hub.conduit on. A new sciontool is served over conduit; an old one
// (port-forward tunnel only) still works through the tunnel. The other
// cells: an old hub never advertises SCION_HUB_CONDUIT, so a new
// sciontool keeps the tunnel (TestPortForwardingConduitGate), and old
// against old is the unchanged tunnel path (TestAgentPortProxyThroughTunnel).
func TestConduitProxyVersionMatrix(t *testing.T) {
	for _, tc := range []struct {
		name       string
		start      func(t *testing.T, f *conduitProxyFixture)
		wantTunnel bool
	}{
		{
			name:  "new sciontool, conduit",
			start: func(t *testing.T, f *conduitProxyFixture) { f.startAgent(t) },
		},
		{
			name: "old sciontool, tunnel",
			start: func(t *testing.T, f *conduitProxyFixture) {
				guardSciontoolLog()
				ctx, cancel := context.WithCancel(context.Background())
				t.Cleanup(cancel)
				m := scionportforward.NewManager(scionhub.NewClientWithConfig(f.base, f.agentToken(t, f.launched), f.launched.ID))
				go m.Run(ctx)
				require.Eventually(t, func() bool { return f.srv.portTunnels.has(f.launched.ID) },
					10*time.Second, 20*time.Millisecond, "tunnel not established")
			},
			wantTunnel: true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newConduitProxyFixture(t, nil)
			tc.start(t, f)
			resp := f.get(t, "/matrix?v=1", nil)
			body, err := io.ReadAll(resp.Body)
			require.NoError(t, err)
			require.Equal(t, http.StatusOK, resp.StatusCode, string(body))
			assert.Equal(t, "hello /matrix?v=1", string(body))
			assert.Equal(t, tc.wantTunnel, f.srv.portTunnels.has(f.launched.ID))
			assert.Equal(t, !tc.wantTunnel, f.sessions.Load() == 1, "conduit sessions")
		})
	}
}

// TestConduitSciontoolSupersededLaunchRefused: a sciontool presenting a
// launch id the hub has superseded is refused 4409 by the real hub and
// redials only after the maximum backoff, without being admitted (it
// stops once refusals persist; see the sciontool conduit tests).
func TestConduitSciontoolSupersededLaunchRefused(t *testing.T) {
	f := newConduitProxyFixture(t, nil)
	type decision struct {
		code  uint32
		delay time.Duration
		err   error
	}
	decisions := make(chan decision, 4)
	f.onEnd = func(end conduit.End, delay time.Duration, err error) {
		decisions <- decision{end.Code(), delay, err}
	}
	_, result := f.runAgent(t, "superseded-launch", make(chan *conduitv1.Welcome, 1))
	for attempt := 1; attempt <= 2; attempt++ {
		select {
		case d := <-decisions:
			require.NoError(t, d.err, "attempt %d", attempt)
			assert.Equal(t, relay.CloseSupersededIncarnation, d.code, "attempt %d", attempt)
			assert.Equal(t, conduit.BackoffMax, d.delay, "attempt %d", attempt)
		case err := <-result:
			t.Fatalf("sciontool stopped on attempt %d: %v", attempt, err)
		case <-time.After(10 * time.Second):
			t.Fatalf("no decision for attempt %d", attempt)
		}
		// The redial timer and the key refresh are armed.
		require.True(t, f.agentClock.WaitFor(10*time.Second, func(n int) bool { return n == 2 }))
		f.agentClock.Advance(conduit.BackoffMax)
	}
	assert.Zero(t, f.sessions.Load())
}

// TestConduitSciontoolKeyRotationWithoutReconnect: after the hub rotates
// its grant signing key, a sciontool that refreshed its keys from the
// hub's grant-key route accepts grants signed with the new key on the
// same session; before the refresh it refuses them.
func TestConduitSciontoolKeyRotationWithoutReconnect(t *testing.T) {
	f := newConduitProxyFixture(t, nil)
	f.startAgent(t)
	require.Equal(t, http.StatusOK, f.get(t, "/before", nil).StatusCode)

	activation := f.srv.conduitGrantKeyActivation()
	_, err := f.srv.conduitGrants.rotate(context.Background(), activation, time.Hour)
	require.NoError(t, err)
	f.clock.Advance(activation + time.Second) // the new key now signs

	assert.NotEqual(t, http.StatusOK, f.get(t, "/stale", nil).StatusCode,
		"grant signed with a key the agent has not fetched was accepted")
	require.NoError(t, f.sciontool.RefreshKeys(context.Background()))
	resp := f.get(t, "/after", nil)
	body, _ := io.ReadAll(resp.Body)
	require.Equal(t, http.StatusOK, resp.StatusCode, string(body))
	assert.Equal(t, int64(1), f.sessions.Load(), "the session was replaced")
}
