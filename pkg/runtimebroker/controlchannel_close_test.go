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
	"context"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

// closeTestGuard bounds every wait in these tests; reaching it is a failure
// (a hang), not a timing the tests depend on.
const closeTestGuard = 10 * time.Second

func waitChan[T any](t *testing.T, ch <-chan T, what string) T {
	t.Helper()
	select {
	case v := <-ch:
		return v
	case <-time.After(closeTestGuard):
		t.Fatalf("timed out after %v waiting for %s", closeTestGuard, what)
		var zero T
		return zero
	}
}

func newCloseTestClient(endpoint string) *ControlChannelClient {
	cfg := DefaultControlChannelConfig()
	cfg.HubEndpoint = endpoint
	cfg.BrokerID = "b1"
	// One attempt per test: a retry would only come after the test ends.
	cfg.ReconnectInitial = time.Hour
	cfg.ReconnectMax = time.Hour
	return NewControlChannelClient(cfg, http.NotFoundHandler(), nil, "", slog.Default())
}

// TestControlChannelClient_ConnectCompletingAfterCloseRegistersNoSession: a
// dial that has completed when Close runs, but whose connection is not yet
// stored, never sends the connect message, so the Hub registers no session
// for the closed client. The dial is held at the afterDial hook while Close
// runs, then released.
func TestControlChannelClient_ConnectCompletingAfterCloseRegistersNoSession(t *testing.T) {
	var gotConnect atomic.Int32
	ended := make(chan struct{}, 1)
	upgrader := websocket.Upgrader{CheckOrigin: func(r *http.Request) bool { return true }}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ws, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer func() { _ = ws.Close() }()
		if _, _, err := ws.ReadMessage(); err == nil {
			gotConnect.Add(1)
		}
		ended <- struct{}{}
	}))
	t.Cleanup(srv.Close)

	c := newCloseTestClient(srv.URL)
	dialed := make(chan struct{})
	proceed := make(chan struct{})
	var once sync.Once
	c.afterDial = func() {
		once.Do(func() { close(dialed) })
		<-proceed
	}

	connectDone := make(chan error, 1)
	go func() { connectDone <- c.Connect(context.Background()) }()
	waitChan(t, dialed, "the dial to complete")

	closeDone := make(chan error, 1)
	go func() { closeDone <- c.Close() }()
	if err := waitChan(t, closeDone, "Close to return while the dial is held"); err != nil {
		t.Errorf("Close = %v, want nil", err)
	}
	close(proceed)

	if err := waitChan(t, connectDone, "Connect to return after Close"); !errors.Is(err, context.Canceled) {
		t.Errorf("Connect after Close = %v, want context.Canceled", err)
	}
	waitChan(t, ended, "the Hub side of the connection to end")
	if n := gotConnect.Load(); n != 0 {
		t.Errorf("the Hub received %d connect message(s) from a closed client, want none", n)
	}
	if c.IsConnected() {
		t.Error("the closed client reports itself connected")
	}
}

// TestControlChannelClient_CloseWhileDialBlockedReturnsPromptly: Close
// returns, and the connect loop ends, while the dial is still waiting on a
// Hub that never answers; the dial ends only because Close cancels it.
func TestControlChannelClient_CloseWhileDialBlockedReturnsPromptly(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	accepted := make(chan struct{}, 1)
	var held sync.Mutex
	var conns []net.Conn
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			held.Lock()
			conns = append(conns, conn) // never answered
			held.Unlock()
			select {
			case accepted <- struct{}{}:
			default:
			}
		}
	}()
	t.Cleanup(func() {
		held.Lock()
		defer held.Unlock()
		for _, conn := range conns {
			_ = conn.Close()
		}
	})

	c := newCloseTestClient("http://" + ln.Addr().String())
	connectDone := make(chan error, 1)
	go func() { connectDone <- c.Connect(context.Background()) }()
	waitChan(t, accepted, "the dial to reach the Hub")

	closeDone := make(chan error, 1)
	go func() { closeDone <- c.Close() }()
	if err := waitChan(t, closeDone, "Close to return while the dial is blocked"); err != nil {
		t.Errorf("Close = %v, want nil", err)
	}
	if err := waitChan(t, connectDone, "the connect loop to end after Close"); !errors.Is(err, context.Canceled) {
		t.Errorf("Connect after Close = %v, want context.Canceled", err)
	}
}

// TestControlChannelClient_CloseDuringBackoffReturnsPromptly: Close returns,
// and the connect loop ends, while the loop waits out its backoff after a
// failed connect.
func TestControlChannelClient_CloseDuringBackoffReturnsPromptly(t *testing.T) {
	attempted := make(chan struct{}, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case attempted <- struct{}{}:
		default:
		}
		http.Error(w, "unavailable", http.StatusServiceUnavailable)
	}))
	t.Cleanup(srv.Close)

	c := newCloseTestClient(srv.URL)
	connectDone := make(chan error, 1)
	go func() { connectDone <- c.Connect(context.Background()) }()
	waitChan(t, attempted, "the first connect attempt")

	closeDone := make(chan error, 1)
	go func() { closeDone <- c.Close() }()
	if err := waitChan(t, closeDone, "Close to return during the backoff"); err != nil {
		t.Errorf("Close = %v, want nil", err)
	}
	if err := waitChan(t, connectDone, "the connect loop to end after Close"); !errors.Is(err, context.Canceled) {
		t.Errorf("Connect after Close = %v, want context.Canceled", err)
	}
}

// TestControlChannelClient_CloseWithoutConnect: Close on a client that never
// connected returns at once without error.
func TestControlChannelClient_CloseWithoutConnect(t *testing.T) {
	c := newCloseTestClient("http://127.0.0.1:1")
	closeDone := make(chan error, 1)
	go func() { closeDone <- c.Close() }()
	if err := waitChan(t, closeDone, "Close without Connect"); err != nil {
		t.Errorf("Close without Connect = %v, want nil", err)
	}
}
