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
	"net/http"
	"net/http/httptest"
	"runtime"
	"sync/atomic"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/wsprotocol"
	"github.com/gorilla/websocket"
)

// fakeControlHub is a minimal Hub control-channel endpoint: it completes the
// connect handshake, then reads until the broker's connection ends.
type fakeControlHub struct {
	srv      *httptest.Server
	connects atomic.Int32
	ended    atomic.Int32 // connections the hub saw end
}

func newFakeControlHub(t *testing.T) *fakeControlHub {
	t.Helper()
	h := &fakeControlHub{}
	upgrader := websocket.Upgrader{CheckOrigin: func(r *http.Request) bool { return true }}
	h.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ws, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer func() { _ = ws.Close() }()
		h.connects.Add(1)
		if _, _, err := ws.ReadMessage(); err != nil { // connect message
			h.ended.Add(1)
			return
		}
		// A long ping interval from the hub would override the test's; send
		// none so the client keeps its configured interval.
		if err := ws.WriteJSON(wsprotocol.NewConnectedMessage("b1", "session", 0)); err != nil {
			h.ended.Add(1)
			return
		}
		for {
			if _, _, err := ws.ReadMessage(); err != nil {
				h.ended.Add(1)
				return
			}
		}
	}))
	t.Cleanup(h.srv.Close)
	return h
}

// newPingTestClient returns a control-channel client for hub whose read
// deadline (PongWait) is far longer than the test's timeouts, so a reconnect
// within those timeouts can only come from the ping write failure.
func newPingTestClient(hub *fakeControlHub, writePing func(*wsprotocol.Connection) error) *ControlChannelClient {
	cfg := DefaultControlChannelConfig()
	cfg.HubEndpoint = hub.srv.URL
	cfg.BrokerID = "b1"
	cfg.PingInterval = 20 * time.Millisecond
	cfg.PongWait = time.Minute
	cfg.WriteWait = time.Second
	cfg.ReconnectInitial = 10 * time.Millisecond
	cfg.ReconnectMax = 20 * time.Millisecond
	c := NewControlChannelClient(cfg, http.NotFoundHandler(), nil, "", slog.Default())
	c.writePing = writePing
	return c
}

func waitFor(t *testing.T, timeout time.Duration, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out after %v waiting for %s", timeout, what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// A failed ping write closes the connection at once and the client
// reconnects, without waiting for the read deadline.
func TestControlChannelPing_WriteErrorClosesAndReconnects(t *testing.T) {
	hub := newFakeControlHub(t)
	var pings atomic.Int32
	c := newPingTestClient(hub, func(conn *wsprotocol.Connection) error {
		if pings.Add(1) == 1 {
			return errors.New("websocket write timeout")
		}
		return conn.WritePing()
	})
	var transitions []bool
	stateCh := make(chan bool, 16)
	c.config.OnConnectionStateChange = func(connected bool) { stateCh <- connected }

	connectDone := make(chan error, 1)
	go func() { connectDone <- c.Connect(context.Background()) }()

	waitFor(t, 3*time.Second, "the hub to see the first connection end", func() bool { return hub.ended.Load() >= 1 })
	waitFor(t, 3*time.Second, "a second connection", func() bool { return hub.connects.Load() >= 2 })
	waitFor(t, 3*time.Second, "the client to be connected again", c.IsConnected)

	for len(transitions) < 3 {
		select {
		case s := <-stateCh:
			transitions = append(transitions, s)
		case <-time.After(3 * time.Second):
			t.Fatalf("connection state transitions = %v, want connected, disconnected, connected", transitions)
		}
	}
	if transitions[0] != true || transitions[1] != false || transitions[2] != true {
		t.Errorf("connection state transitions = %v, want [true false true]", transitions)
	}

	if err := c.Close(); err != nil {
		t.Logf("Close: %v", err)
	}
	select {
	case <-connectDone:
	case <-time.After(3 * time.Second):
		t.Fatal("Connect did not return after Close")
	}
}

// Repeated ping write failures each close their connection and reconnect;
// every replaced connection is closed, and no ping loop or reader goroutine
// is left behind once the client is closed.
func TestControlChannelPing_RepeatedFailuresDoNotLeak(t *testing.T) {
	hub := newFakeControlHub(t)
	baseline := runtime.NumGoroutine()

	c := newPingTestClient(hub, func(conn *wsprotocol.Connection) error {
		return errors.New("websocket write timeout")
	})
	connectDone := make(chan error, 1)
	go func() { connectDone <- c.Connect(context.Background()) }()

	const rounds = 8
	waitFor(t, 5*time.Second, "repeated reconnects", func() bool { return hub.connects.Load() >= rounds })
	// Every connection but possibly the current one has ended on the hub.
	waitFor(t, 3*time.Second, "replaced connections to be closed", func() bool {
		return hub.ended.Load() >= hub.connects.Load()-1
	})

	closeDone := make(chan struct{})
	go func() {
		_ = c.Close()
		close(closeDone)
	}()
	select {
	case <-closeDone:
	case <-time.After(3 * time.Second):
		t.Fatal("Close did not return")
	}
	select {
	case <-connectDone:
	case <-time.After(3 * time.Second):
		t.Fatal("Connect did not return after Close")
	}
	hub.srv.Close()

	waitFor(t, 3*time.Second, "goroutines to return to baseline", func() bool {
		return runtime.NumGoroutine() <= baseline+2
	})
}
