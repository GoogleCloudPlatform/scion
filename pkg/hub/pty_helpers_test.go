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

package hub

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/wsprotocol"
	"github.com/gorilla/websocket"
	"github.com/stretchr/testify/require"
)

// startCloseCodeSession starts a PTY session against a loopback broker and
// waits for the stream_open to reach the broker.
func startCloseCodeSession(t *testing.T) *closeCodeFixture {
	t.Helper()
	manager := NewControlChannelManager(DefaultControlChannelConfig(), slog.Default())
	t.Cleanup(manager.Shutdown)
	const brokerID = "close-code-broker"
	broker := closeCodeBroker(t, manager, brokerID)
	local, browser := lifecycleWebSocketPair(t)
	f := &closeCodeFixture{
		manager: manager,
		broker:  broker,
		browser: browser,
		session: newPTYSession(context.Background(), "close-code-agent", "fixture-project", brokerID, local, manager, 80, 24),
		done:    make(chan error, 1),
	}
	go func() { f.done <- f.session.Run() }()
	t.Cleanup(func() { _ = browser.Close() })
	require.NoError(t, broker.SetReadDeadline(time.Now().Add(5*time.Second)))
	require.NoError(t, broker.ReadJSON(&f.open))
	require.Equal(t, wsprotocol.TypeStreamOpen, f.open.Type)
	return f
}

// readBrowserUntilClose returns the PTY data frames the browser received, in
// order, and the close frame that ended the connection.
func readBrowserUntilClose(t *testing.T, browser *websocket.Conn) ([]string, *websocket.CloseError) {
	t.Helper()
	require.NoError(t, browser.SetReadDeadline(time.Now().Add(5*time.Second)))
	var frames []string
	for {
		var msg wsprotocol.PTYDataMessage
		err := browser.ReadJSON(&msg)
		if err != nil {
			var ce *websocket.CloseError
			require.True(t, errors.As(err, &ce), "browser connection ended without a close frame: %v", err)
			return frames, ce
		}
		if msg.Type == wsprotocol.TypeData {
			frames = append(frames, string(msg.Data))
		}
	}
}

func lifecycleWebSocketPair(t *testing.T) (*websocket.Conn, *websocket.Conn) {
	t.Helper()
	accepted := make(chan *websocket.Conn, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upgrader := websocket.Upgrader{}
		conn, err := upgrader.Upgrade(w, r, nil)
		if err == nil {
			accepted <- conn
		}
	}))
	t.Cleanup(server.Close)
	peer, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(server.URL, "http"), nil)
	require.NoError(t, err)
	t.Cleanup(func() { _ = peer.Close() })
	select {
	case local := <-accepted:
		t.Cleanup(func() { _ = local.Close() })
		return local, peer
	case <-time.After(5 * time.Second):
		t.Fatal("local websocket upgrade did not complete")
		return nil, nil
	}
}

// closeCodeBroker registers a loopback broker control channel with manager
// and returns the broker's side of it.
func closeCodeBroker(t *testing.T, manager *ControlChannelManager, brokerID string) *websocket.Conn {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = manager.HandleUpgrade(w, r, brokerID)
	}))
	t.Cleanup(server.Close)
	broker, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(server.URL, "http"), nil)
	require.NoError(t, err)
	t.Cleanup(func() { _ = broker.Close() })
	require.NoError(t, broker.SetReadDeadline(time.Now().Add(5*time.Second)))
	var connected wsprotocol.ConnectedMessage
	require.NoError(t, broker.ReadJSON(&connected))
	require.Equal(t, wsprotocol.TypeConnected, connected.Type)
	require.Eventually(t, func() bool { return manager.IsConnected(brokerID) }, 5*time.Second, 10*time.Millisecond)
	return broker
}

type closeCodeFixture struct {
	manager *ControlChannelManager
	broker  *websocket.Conn
	browser *websocket.Conn
	session *PTYSession
	done    chan error
	open    wsprotocol.StreamOpenMessage
}

func (f *closeCodeFixture) waitDone(t *testing.T) error {
	t.Helper()
	select {
	case err := <-f.done:
		return err
	case <-time.After(5 * time.Second):
		t.Fatal("Hub PTY session did not exit")
		return nil
	}
}
