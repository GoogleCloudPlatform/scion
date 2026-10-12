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
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/stretchr/testify/require"
)

// dialGuard bounds every wait in these tests; reaching it is a failure (a
// hang), not a timing the tests depend on.
const dialGuard = 10 * time.Second

type dialResult struct {
	conn *Connection
	err  error
}

// silentListener accepts TCP connections and never answers on them, so a
// websocket handshake to it never completes. accepted gets one value per
// accepted connection (dropped if nobody is waiting).
func silentListener(t *testing.T) (addr string, accepted <-chan struct{}) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	acc := make(chan struct{}, 1)
	var mu sync.Mutex
	var conns []net.Conn
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			mu.Lock()
			conns = append(conns, c)
			mu.Unlock()
			select {
			case acc <- struct{}{}:
			default:
			}
		}
	}()
	t.Cleanup(func() {
		_ = ln.Close()
		mu.Lock()
		defer mu.Unlock()
		for _, c := range conns {
			_ = c.Close()
		}
	})
	return ln.Addr().String(), acc
}

func waitDial(t *testing.T, ch <-chan dialResult, what string) dialResult {
	t.Helper()
	select {
	case r := <-ch:
		return r
	case <-time.After(dialGuard):
		t.Fatalf("timed out after %v waiting for %s", dialGuard, what)
		return dialResult{}
	}
}

// TestDialWithConfig_CancelEndsUnansweredHandshake: a dial whose server
// accepts the TCP connection but never answers the websocket handshake ends
// as soon as its context is cancelled.
func TestDialWithConfig_CancelEndsUnansweredHandshake(t *testing.T) {
	addr, accepted := silentListener(t)
	cfg := DefaultConnectionConfig()
	cfg.HandshakeTimeout = time.Hour // only the cancellation can end it

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan dialResult, 1)
	go func() {
		c, _, err := DialWithConfig(ctx, "ws://"+addr+"/", nil, cfg)
		done <- dialResult{c, err}
	}()
	select {
	case <-accepted:
	case <-time.After(dialGuard):
		t.Fatal("the dial never reached the server")
	}
	cancel()
	r := waitDial(t, done, "the dial to end after its context was cancelled")
	require.ErrorIs(t, r.err, context.Canceled, "a cancelled dial reports the cancellation")
	require.Nil(t, r.conn)
}

// TestDialWithConfig_UnansweredHandshakeEndsAtHandshakeTimeout: with no
// cancellation, a handshake the server never answers fails at the
// configured HandshakeTimeout, not before.
func TestDialWithConfig_UnansweredHandshakeEndsAtHandshakeTimeout(t *testing.T) {
	addr, _ := silentListener(t)
	cfg := DefaultConnectionConfig()
	cfg.HandshakeTimeout = 200 * time.Millisecond

	start := time.Now()
	done := make(chan dialResult, 1)
	go func() {
		c, _, err := DialWithConfig(context.Background(), "ws://"+addr+"/", nil, cfg)
		done <- dialResult{c, err}
	}()
	r := waitDial(t, done, "the handshake timeout")
	require.Error(t, r.err)
	require.Nil(t, r.conn)
	require.GreaterOrEqual(t, time.Since(start), cfg.HandshakeTimeout, "the dial ended before its handshake timeout")
}

// TestDialWithConfig_ConnectionOutlivesItsDialContext: a dial that
// completes returns a connection with no deadline left from dialling, so
// cancelling the dial's context afterwards does not affect it.
func TestDialWithConfig_ConnectionOutlivesItsDialContext(t *testing.T) {
	release := make(chan struct{})
	upgrader := websocket.Upgrader{CheckOrigin: func(r *http.Request) bool { return true }}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ws, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer func() { _ = ws.Close() }()
		<-release // answer only after the client cancelled its dial context
		_ = ws.WriteMessage(websocket.TextMessage, []byte("hello"))
		_, _, _ = ws.ReadMessage()
	}))
	t.Cleanup(srv.Close)

	ctx, cancel := context.WithCancel(context.Background())
	conn, _, err := DialWithConfig(ctx, "ws"+strings.TrimPrefix(srv.URL, "http")+"/", nil, DefaultConnectionConfig())
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close() })

	cancel()
	close(release)
	got := make(chan error, 1)
	go func() {
		_, data, err := conn.ReadMessage()
		if err == nil && string(data) != "hello" {
			err = errUnexpected(string(data))
		}
		got <- err
	}()
	select {
	case err := <-got:
		require.NoError(t, err, "a read after the dial context was cancelled")
	case <-time.After(dialGuard):
		t.Fatal("timed out waiting for the server's message")
	}
}

type errUnexpected string

func (e errUnexpected) Error() string { return "unexpected message " + string(e) }
