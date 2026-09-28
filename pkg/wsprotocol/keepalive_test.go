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

package wsprotocol

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/stretchr/testify/require"
)

// newRawKeepaliveWSPair sets up a real client/server WebSocket pair over a
// local httptest server, for exercising StartKeepalive against an actual
// connection rather than a mock.
func newRawKeepaliveWSPair(t *testing.T) (client, server *websocket.Conn, cleanup func()) {
	t.Helper()
	upgrader := websocket.Upgrader{CheckOrigin: func(r *http.Request) bool { return true }}
	ready := make(chan *websocket.Conn, 1)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ws, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			t.Errorf("upgrade: %v", err)
			return
		}
		ready <- ws
	}))

	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http")
	c, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
	require.NoError(t, err)
	s := <-ready

	return c, s, func() {
		_ = c.Close()
		_ = s.Close()
		srv.Close()
	}
}

// TestStartKeepalive_PongsExtendTheReadDeadline proves the full keepalive
// loop end to end: StartKeepalive's ping ticker actually writes pings, the
// peer's default gorilla/websocket behavior answers each with a pong (as
// long as it is pumping reads), and the pong handler StartKeepalive installs
// extends the read deadline enough that a read on the keepalive side never
// times out, well past what a single, un-extended deadline would allow.
func TestStartKeepalive_PongsExtendTheReadDeadline(t *testing.T) {
	client, server, cleanup := newRawKeepaliveWSPair(t)
	defer cleanup()

	// The client only needs to keep pumping reads for gorilla's default ping
	// handler to fire (which answers with a pong automatically); it never
	// sends anything itself.
	clientDone := make(chan struct{})
	go func() {
		defer close(clientDone)
		for {
			if _, _, err := client.ReadMessage(); err != nil {
				return
			}
		}
	}()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var mu sync.Mutex
	cfg := ConnectionConfig{PingInterval: 20 * time.Millisecond, PongWait: 80 * time.Millisecond, WriteWait: 200 * time.Millisecond}
	require.NoError(t, StartKeepalive(ctx, server, &mu, cfg))

	// Read on the server side for well over 3x PongWait. If pongs were not
	// extending the deadline, this would time out at ~1x PongWait.
	readErrCh := make(chan error, 1)
	go func() {
		_, _, err := server.ReadMessage()
		readErrCh <- err
	}()

	select {
	case err := <-readErrCh:
		t.Fatalf("read ended too early (deadline was not extended): %v", err)
	case <-time.After(300 * time.Millisecond):
		// Still alive well past a single PongWait: the pongs are extending
		// the deadline as intended.
	}

	// Stop the client from reading (and therefore from ever pong-ing again),
	// and confirm the server's read now does eventually fail once the last
	// extended deadline lapses.
	cancel()
	_ = client.Close()
	<-clientDone

	select {
	case err := <-readErrCh:
		require.Error(t, err)
	case <-time.After(2 * time.Second):
		t.Fatal("server read did not fail after the peer stopped answering")
	}
}

// TestStartKeepalive_NoPongsExpireTheReadDeadline is the negative control for
// TestStartKeepalive_PongsExtendTheReadDeadline: with the peer never pumping
// reads (so gorilla's default ping handler never runs and no pong is ever
// sent), a read on the keepalive side must time out at approximately
// PongWait, ruling out a false pass where the deadline was never really
// armed in the first place.
func TestStartKeepalive_NoPongsExpireTheReadDeadline(t *testing.T) {
	_, server, cleanup := newRawKeepaliveWSPair(t)
	defer cleanup()
	// The client deliberately never reads, so it never answers a ping with a
	// pong.

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var mu sync.Mutex
	cfg := ConnectionConfig{PingInterval: 20 * time.Millisecond, PongWait: 100 * time.Millisecond, WriteWait: 200 * time.Millisecond}
	require.NoError(t, StartKeepalive(ctx, server, &mu, cfg))

	start := time.Now()
	_, _, err := server.ReadMessage()
	elapsed := time.Since(start)

	require.Error(t, err)
	require.Less(t, elapsed, 500*time.Millisecond, "deadline should expire at about PongWait, not hang")
}

// TestStartKeepalive_ZeroConfigFallsBackToDefaults proves that a zero-valued
// ConnectionConfig does not crash the ping loop and does not fail the very
// next read immediately: a zero PingInterval panics inside time.NewTicker,
// and a zero PongWait would arm the read deadline at time.Now(), the same
// as having no keepalive at all.
func TestStartKeepalive_ZeroConfigFallsBackToDefaults(t *testing.T) {
	_, server, cleanup := newRawKeepaliveWSPair(t)
	defer cleanup()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var mu sync.Mutex
	require.NoError(t, StartKeepalive(ctx, server, &mu, ConnectionConfig{}))

	// Give the ping loop's goroutine a moment to actually run: a zero
	// PingInterval panics inside time.NewTicker almost immediately, which
	// would crash the whole test binary rather than merely failing this
	// test.
	time.Sleep(50 * time.Millisecond)

	// A zero PongWait would have armed the read deadline at time.Now(),
	// failing this read immediately. The real default is 60s, so surviving
	// comfortably past 200ms proves the fallback took effect.
	readErrCh := make(chan error, 1)
	go func() {
		_, _, err := server.ReadMessage()
		readErrCh <- err
	}()
	select {
	case err := <-readErrCh:
		t.Fatalf("read failed immediately; the PongWait fallback did not take effect: %v", err)
	case <-time.After(200 * time.Millisecond):
		// Still blocked well past what a zero PongWait would have allowed.
	}
}

// TestStartKeepalive_ZeroWriteWaitStillPings proves the WriteWait fallback:
// left at zero, every ping write's deadline would otherwise be
// time.Now(), failing the very first ping write and ending the keepalive
// loop silently — after which the connection would only die once the
// already-armed PongWait elapses on its own, not because pongs kept
// extending it.
func TestStartKeepalive_ZeroWriteWaitStillPings(t *testing.T) {
	client, server, cleanup := newRawKeepaliveWSPair(t)
	defer cleanup()

	clientDone := make(chan struct{})
	go func() {
		defer close(clientDone)
		for {
			if _, _, err := client.ReadMessage(); err != nil {
				return
			}
		}
	}()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var mu sync.Mutex
	// WriteWait deliberately left at zero.
	cfg := ConnectionConfig{PingInterval: 20 * time.Millisecond, PongWait: 100 * time.Millisecond}
	require.NoError(t, StartKeepalive(ctx, server, &mu, cfg))

	readErrCh := make(chan error, 1)
	go func() {
		_, _, err := server.ReadMessage()
		readErrCh <- err
	}()

	select {
	case err := <-readErrCh:
		t.Fatalf("read ended too early; the WriteWait fallback did not take effect: %v", err)
	case <-time.After(300 * time.Millisecond):
		// Still alive well past a single PongWait: pings kept being sent
		// (each with a working write deadline) and answered.
	}

	cancel()
	_ = client.Close()
	<-clientDone
}

// TestStartKeepalive_PingIntervalClampedBelowPongWait proves that a partial
// config leaving PingInterval at its 30s default alongside a much smaller
// caller-supplied PongWait does not let the read deadline expire before the
// first ping is ever sent — which would otherwise drop the connection
// repeatedly instead of keeping it alive.
func TestStartKeepalive_PingIntervalClampedBelowPongWait(t *testing.T) {
	require.Equal(t, 50*time.Millisecond, pingIntervalFor(DefaultPingInterval, 100*time.Millisecond),
		"a PongWait of 100ms should give a PingInterval of half that, 50ms")

	client, server, cleanup := newRawKeepaliveWSPair(t)
	defer cleanup()

	clientDone := make(chan struct{})
	go func() {
		defer close(clientDone)
		for {
			if _, _, err := client.ReadMessage(); err != nil {
				return
			}
		}
	}()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var mu sync.Mutex
	// Only PongWait is set; PingInterval would otherwise fall back to the
	// 30s default, which is larger than this PongWait.
	cfg := ConnectionConfig{PongWait: 100 * time.Millisecond}
	require.NoError(t, StartKeepalive(ctx, server, &mu, cfg))

	readErrCh := make(chan error, 1)
	go func() {
		_, _, err := server.ReadMessage()
		readErrCh <- err
	}()

	select {
	case err := <-readErrCh:
		t.Fatalf("read ended too early; PingInterval was not clamped below PongWait: %v", err)
	case <-time.After(300 * time.Millisecond):
		// A ping must have gone out, and been answered, well within the
		// 100ms PongWait for the read to still be alive here.
	}

	cancel()
	_ = client.Close()
	<-clientDone
}

// TestStartKeepalive_TinyPongWaitDoesNotPanic covers the clamp's own edge
// case: a PongWait of 1ns is the smallest positive duration the fallbacks
// let through unchanged, and a clamp expression that can truncate to zero
// for a small PongWait (integer division rounds down) would set
// PingInterval to zero, panicking inside time.NewTicker. The clamp must
// never produce zero for any positive PongWait: at 1ns it cannot go any
// lower than PongWait itself, so it stays at 1ns rather than truncating to
// zero.
func TestStartKeepalive_TinyPongWaitDoesNotPanic(t *testing.T) {
	require.Equal(t, time.Nanosecond, pingIntervalFor(DefaultPingInterval, time.Nanosecond),
		"a 1ns PongWait should floor the PingInterval at 1ns, not truncate it to 0")

	_, server, cleanup := newRawKeepaliveWSPair(t)
	defer cleanup()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var mu sync.Mutex
	require.NoError(t, StartKeepalive(ctx, server, &mu, ConnectionConfig{PongWait: time.Nanosecond}))

	// Give the ping loop's goroutine a moment to actually run: a PingInterval
	// that the clamp truncated to zero would panic inside time.NewTicker
	// almost immediately, crashing the whole test binary rather than merely
	// failing this test.
	time.Sleep(50 * time.Millisecond)
}

// TestPingIntervalFor covers pingIntervalFor's own decision boundary
// directly: an interval strictly below the wait is left alone, but an
// interval equal to (not just above) the wait must still fall back to half
// — a boundary the behavioral tests above don't exercise on their own,
// since they only ever pass a default PingInterval that is either clearly
// below or clearly at/above PongWait, never exactly equal to it.
func TestPingIntervalFor(t *testing.T) {
	tests := []struct {
		name         string
		pingInterval time.Duration
		pongWait     time.Duration
		want         time.Duration
	}{
		{
			name:         "interval below the wait is left unchanged",
			pingInterval: 20 * time.Millisecond,
			pongWait:     100 * time.Millisecond,
			want:         20 * time.Millisecond,
		},
		{
			name:         "interval equal to the wait falls back to half",
			pingInterval: 100 * time.Millisecond,
			pongWait:     100 * time.Millisecond,
			want:         50 * time.Millisecond,
		},
		{
			name:         "interval above the wait falls back to half",
			pingInterval: 30 * time.Second,
			pongWait:     100 * time.Millisecond,
			want:         50 * time.Millisecond,
		},
		{
			name:         "a 2ns wait gives 1ns, strictly below the wait",
			pingInterval: 30 * time.Second,
			pongWait:     2 * time.Nanosecond,
			want:         time.Nanosecond,
		},
		{
			name:         "a 1ns wait floors at 1ns instead of truncating to 0",
			pingInterval: 30 * time.Second,
			pongWait:     time.Nanosecond,
			want:         time.Nanosecond,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, tc.want, pingIntervalFor(tc.pingInterval, tc.pongWait))
		})
	}
}
