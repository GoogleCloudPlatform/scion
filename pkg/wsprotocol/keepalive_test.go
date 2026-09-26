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
