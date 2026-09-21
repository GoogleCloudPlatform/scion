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
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/wsprotocol"
	"github.com/gorilla/websocket"
	"github.com/stretchr/testify/require"
)

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

// Authorisation and agent lookup are deliberately outside this isolated session
// test. Both browser and broker sockets are loopback peers, never real agents.
func TestPTYLifecycle_ClientCloseReleasesHubStreamOnce(t *testing.T) {
	local, browser := lifecycleWebSocketPair(t)
	brokerLocal, broker := lifecycleWebSocketPair(t)
	manager := NewControlChannelManager(DefaultControlChannelConfig(), slog.Default())
	connection := &BrokerConnection{
		brokerID: "lifecycle-broker",
		conn:     wsprotocol.NewConnection(brokerLocal, wsprotocol.ConnectionConfig{WriteWait: time.Second}),
		streams:  make(map[string]*StreamProxy),
	}
	manager.connections[connection.brokerID] = connection
	session := newPTYSession(context.Background(), "fixture-agent", "fixture-project", connection.brokerID, local, manager, 80, 24)
	done := make(chan error, 1)
	go func() { done <- session.Run() }()
	// Close the peer on failure too, releasing Run without racing stream setup.
	t.Cleanup(func() { _ = browser.Close() })
	require.NoError(t, broker.SetReadDeadline(time.Now().Add(5*time.Second)))
	var open wsprotocol.StreamOpenMessage
	require.NoError(t, broker.ReadJSON(&open))
	require.Equal(t, wsprotocol.TypeStreamOpen, open.Type)
	require.Equal(t, 80, open.Cols)
	require.Equal(t, 24, open.Rows)

	// Existing browser close protocol: send prefix+d, then close the socket.
	require.NoError(t, browser.WriteJSON(wsprotocol.NewPTYDataMessage([]byte{2, 'd'})))
	var data wsprotocol.StreamFrame
	require.NoError(t, broker.ReadJSON(&data))
	require.Equal(t, open.StreamID, data.StreamID)
	require.Equal(t, []byte{2, 'd'}, data.Data)
	require.NoError(t, browser.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseNormalClosure, "detach"), time.Now().Add(time.Second)))
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Hub PTY session did not exit after client close")
	}
	var closed wsprotocol.StreamCloseMessage
	require.NoError(t, broker.ReadJSON(&closed))
	require.Equal(t, wsprotocol.TypeStreamClose, closed.Type)
	require.Equal(t, open.StreamID, closed.StreamID)
	require.ErrorIs(t, session.ctx.Err(), context.Canceled)
	connection.streamsMu.RLock()
	remaining := len(connection.streams)
	connection.streamsMu.RUnlock()
	require.Zero(t, remaining)
	require.True(t, manager.IsConnected(connection.brokerID), "closing one attach must preserve broker connection")
	_, err := session.stream.Read(context.Background())
	require.Error(t, err, "stream readers must unblock")
	require.NoError(t, browser.SetReadDeadline(time.Now().Add(time.Second)))
	_, _, err = browser.ReadMessage()
	require.True(t, websocket.IsCloseError(err, websocket.CloseNormalClosure), "browser should receive normal close: %v", err)

	session.Close()
	session.Close()
	require.NoError(t, broker.SetReadDeadline(time.Now().Add(100*time.Millisecond)))
	_, _, err = broker.ReadMessage()
	var timeout net.Error
	require.ErrorAs(t, err, &timeout)
	require.True(t, timeout.Timeout(), "repeated cleanup must not emit another stream-close")
}
