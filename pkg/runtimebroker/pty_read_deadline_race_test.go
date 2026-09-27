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

package runtimebroker

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/wsprotocol"
	"github.com/gorilla/websocket"
	"github.com/stretchr/testify/require"
)

// newRawBrokerWSPair dials a bare WebSocket connection against an
// httptest server and returns both ends, with no PTY or tmux involved: this
// test only needs two real gorilla connections to exercise the read-deadline
// mechanics directly.
func newRawBrokerWSPair(t *testing.T) (server, client *websocket.Conn) {
	t.Helper()
	upgrader := websocket.Upgrader{}
	connCh := make(chan *websocket.Conn, 1)
	httpSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			t.Errorf("upgrade failed: %v", err)
			return
		}
		connCh <- c
	}))
	t.Cleanup(httpSrv.Close)

	wsURL := "ws" + strings.TrimPrefix(httpSrv.URL, "http")
	c, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
	require.NoError(t, err)
	t.Cleanup(func() { _ = c.Close() })

	s := <-connCh
	t.Cleanup(func() { _ = s.Close() })
	return s, c
}

// TestPokeReadDeadlineUntilDone_WinsRaceAgainstLatePongHandler regression-
// tests the teardown race a single SetReadDeadline(now) call has against the
// keepalive's pong handler: the handler runs on the same reader goroutine,
// inside ReadMessage, so a pong whose payload was already read but whose
// handler had not yet fired when the deadline was forced into the past can
// still push it back out by PongWait right afterward — stalling the reader
// (and everything joined on it) for up to PongWait instead of returning
// promptly.
//
// The interleaving is forced deterministically rather than left to a
// wall-clock race: the pong handler blocks, after consuming the pong's
// payload, until the test releases it — exactly the "payload read, handler
// not yet run" window the real race depends on. pokeReadDeadlineUntilDone
// must reclaim the deadline on its next tick regardless of when the handler
// wins that race.
func TestPokeReadDeadlineUntilDone_WinsRaceAgainstLatePongHandler(t *testing.T) {
	server, client := newRawBrokerWSPair(t)

	const pongWait = 300 * time.Millisecond

	handlerEntered := make(chan struct{})
	releaseHandler := make(chan struct{})
	var enteredOnce sync.Once
	server.SetPongHandler(func(appData string) error {
		enteredOnce.Do(func() { close(handlerEntered) })
		<-releaseHandler
		return server.SetReadDeadline(time.Now().Add(pongWait))
	})

	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			if _, _, err := server.ReadMessage(); err != nil {
				return
			}
		}
	}()

	// Send one pong and wait until the server's reader has consumed its
	// payload and is parked inside the pong handler, blocked on
	// releaseHandler.
	require.NoError(t, client.WriteControl(websocket.PongMessage, []byte{}, time.Now().Add(time.Second)))
	select {
	case <-handlerEntered:
	case <-time.After(2 * time.Second):
		t.Fatal("server never entered the pong handler")
	}

	// Release the blocked handler shortly after pokeReadDeadlineUntilDone's
	// first, immediate SetReadDeadline(now) call below, landing squarely in
	// the race window: the handler's SetReadDeadline(now+PongWait) runs
	// right after teardown's own SetReadDeadline(now).
	go func() {
		time.Sleep(10 * time.Millisecond)
		close(releaseHandler)
	}()

	start := time.Now()
	pokeReadDeadlineUntilDone(server, done)
	elapsed := time.Since(start)

	// pokeReadDeadlineUntilDone re-asserts the past deadline every
	// readDeadlinePokeInterval (50ms), so even though the pong handler wins
	// the first race and pushes the deadline back out to PongWait, the next
	// tick reclaims it well before PongWait elapses. A single
	// SetReadDeadline call — the bug this guards against — has nothing to
	// re-assert the deadline once the handler overwrites it, and would
	// instead stall until the full PongWait elapses.
	require.Less(t, elapsed, pongWait/2,
		"pokeReadDeadlineUntilDone lost the race against the pong handler; the reader stalled for close to the full PongWait")
}

// TestLocalPTYSessionRun_TeardownCallsPokeReadDeadlineFn proves Run's
// teardown actually goes through pokeReadDeadlineFn, rather than only
// proving the helper itself behaves correctly in isolation:
// TestPokeReadDeadlineUntilDone_WinsRaceAgainstLatePongHandler calls
// pokeReadDeadlineUntilDone directly, so it cannot tell whether Run's call
// site still uses it or was quietly reverted to a bare SetReadDeadline call
// — reproducing the underlying pong/teardown race end-to-end through a real
// Run() call would need control over the timing of the production pong
// handler installed deep inside wsprotocol.StartKeepalive, which isn't
// practical from a test. Overriding the seam directly sidesteps that: it
// proves the wiring without needing to win the race itself.
func TestLocalPTYSessionRun_TeardownCallsPokeReadDeadlineFn(t *testing.T) {
	dial, tmuxCmd := newDirectAttachFixture(t)

	orig := pokeReadDeadlineFn
	called := make(chan struct{}, 1)
	pokeReadDeadlineFn = func(conn *websocket.Conn, done <-chan struct{}) {
		select {
		case called <- struct{}{}:
		default:
		}
		orig(conn, done) // still do the real work so the session tears down normally.
	}
	t.Cleanup(func() { pokeReadDeadlineFn = orig })

	conn, _, err := dial()
	require.NoError(t, err)
	defer func() { _ = conn.Close() }()

	require.Eventually(t, func() bool {
		out, err := tmuxCmd("list-clients", "-t", "scion", "-F", "#{client_width}x#{client_height}")
		return err == nil && out == "80x24"
	}, 5*time.Second, 20*time.Millisecond, "one real tmux client should attach")

	// C-b d: the tmux detach keystroke, the same trigger
	// TestHandleAgentAttach_NormalEndSendsClassifiedCloseFrame uses to end
	// the session normally.
	require.NoError(t, conn.WriteJSON(wsprotocol.NewPTYDataMessage([]byte{2, 'd'})))

	select {
	case <-called:
	case <-time.After(5 * time.Second):
		t.Fatal("Run's teardown never called pokeReadDeadlineFn")
	}
}
