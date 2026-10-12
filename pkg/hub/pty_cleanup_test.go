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
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/wsprotocol"
	"github.com/gorilla/websocket"
	"github.com/stretchr/testify/require"
)

// TestPTYCleanup_SocketLossReleasesHubStream tests that an unexpected WebSocket
// disconnect (browser crash, network loss) properly releases the hub-side PTY
// session and cleans up the control channel stream to the broker.
func TestPTYCleanup_SocketLossReleasesHubStream(t *testing.T) {
	local, browser := lifecycleWebSocketPair(t)
	brokerLocal, broker := lifecycleWebSocketPair(t)
	manager := NewControlChannelManager(DefaultControlChannelConfig(), slog.Default())
	connection := &BrokerConnection{
		brokerID: "socketloss-broker",
		conn:     wsprotocol.NewConnection(brokerLocal, wsprotocol.ConnectionConfig{WriteWait: time.Second}),
		streams:  make(map[string]*StreamProxy),
	}
	manager.connections[connection.brokerID] = connection

	session := newPTYSession(context.Background(), "socketloss-agent", "fixture-project", connection.brokerID, local, manager, 80, 24)
	done := make(chan error, 1)
	go func() { done <- session.Run() }()
	t.Cleanup(func() { _ = browser.Close() })

	// Wait for stream open to reach broker
	require.NoError(t, broker.SetReadDeadline(time.Now().Add(5*time.Second)))
	var open wsprotocol.StreamOpenMessage
	require.NoError(t, broker.ReadJSON(&open))
	require.Equal(t, wsprotocol.TypeStreamOpen, open.Type)

	// Simulate socket loss by abruptly closing the browser connection
	// (no close frame, just TCP reset)
	_ = browser.UnderlyingConn().(*net.TCPConn).SetLinger(0)
	_ = browser.Close()

	// Session should exit
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Hub PTY session did not exit after socket loss")
	}

	// Verify: session context canceled
	require.ErrorIs(t, session.ctx.Err(), context.Canceled)

	// Verify: stream close sent to broker
	var closed wsprotocol.StreamCloseMessage
	require.NoError(t, broker.SetReadDeadline(time.Now().Add(5*time.Second)))
	require.NoError(t, broker.ReadJSON(&closed))
	require.Equal(t, wsprotocol.TypeStreamClose, closed.Type)
	require.Equal(t, open.StreamID, closed.StreamID)

	// Verify: broker connection preserved
	require.True(t, manager.IsConnected(connection.brokerID),
		"broker connection must survive browser socket loss")

	// Verify: stream cleaned up in connection
	connection.streamsMu.RLock()
	remaining := len(connection.streams)
	connection.streamsMu.RUnlock()
	require.Zero(t, remaining, "stream must be removed from broker connection")
}

// TestPTYCleanup_BrokerDisconnectCleansUpHubSessions tests that when a broker's
// control channel drops, all pending PTY sessions for that broker are cleaned up
// and their streams unblocked.
func TestPTYCleanup_BrokerDisconnectCleansUpHubSessions(t *testing.T) {
	local, browser := lifecycleWebSocketPair(t)
	brokerLocal, broker := lifecycleWebSocketPair(t)
	manager := NewControlChannelManager(DefaultControlChannelConfig(), slog.Default())
	connection := &BrokerConnection{
		brokerID: "disconnect-broker",
		conn:     wsprotocol.NewConnection(brokerLocal, wsprotocol.ConnectionConfig{WriteWait: time.Second}),
		streams:  make(map[string]*StreamProxy),
		ctx:      context.Background(),
		cancel:   func() {},
	}
	manager.connections[connection.brokerID] = connection

	session := newPTYSession(context.Background(), "disconnect-agent", "fixture-project", connection.brokerID, local, manager, 80, 24)
	done := make(chan error, 1)
	go func() { done <- session.Run() }()
	t.Cleanup(func() { _ = browser.Close() })

	// Wait for stream open
	require.NoError(t, broker.SetReadDeadline(time.Now().Add(5*time.Second)))
	var open wsprotocol.StreamOpenMessage
	require.NoError(t, broker.ReadJSON(&open))

	// Simulate broker disconnect by closing the broker connection
	// This is what happens when the broker process crashes or network drops
	connection.Close()

	// The stream proxy should be closed, causing the session to exit
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Hub PTY session did not exit after broker disconnect")
	}

	// Verify: all streams cleaned up
	connection.streamsMu.RLock()
	remaining := len(connection.streams)
	connection.streamsMu.RUnlock()
	require.Zero(t, remaining, "all streams must be cleaned up on broker disconnect")
}

// TestPTYCleanup_HubStreamProxyCloseIdempotent verifies that closing a
// StreamProxy multiple times does not panic.
func TestPTYCleanup_HubStreamProxyCloseIdempotent(t *testing.T) {
	proxy := NewStreamProxy("test-stream", wsprotocol.StreamTypePTY, "test-agent")

	// Multiple Close() calls must not panic
	proxy.Close()
	proxy.Close()
	proxy.Close()

	// Read should return EOF after close
	_, err := proxy.Read(context.Background())
	require.Error(t, err, "Read should return error after Close")
}

// TestPTYCleanup_HubStreamProxyWriteAfterClose verifies that writing to a
// closed StreamProxy returns an error.
func TestPTYCleanup_HubStreamProxyWriteAfterClose(t *testing.T) {
	proxy := NewStreamProxy("test-stream", wsprotocol.StreamTypePTY, "test-agent")
	proxy.Close()

	err := proxy.Write([]byte("data"))
	require.Error(t, err, "Write should return error after Close")
}

// TestPTYCleanup_HubPTYSessionCloseIdempotent verifies that calling Close()
// on a PTY session multiple times does not panic and does not send duplicate
// stream-close messages.
func TestPTYCleanup_HubPTYSessionCloseIdempotent(t *testing.T) {
	local, browser := lifecycleWebSocketPair(t)
	brokerLocal, broker := lifecycleWebSocketPair(t)
	manager := NewControlChannelManager(DefaultControlChannelConfig(), slog.Default())
	connection := &BrokerConnection{
		brokerID: "idem-broker",
		conn:     wsprotocol.NewConnection(brokerLocal, wsprotocol.ConnectionConfig{WriteWait: time.Second}),
		streams:  make(map[string]*StreamProxy),
	}
	manager.connections[connection.brokerID] = connection
	session := newPTYSession(context.Background(), "idem-agent", "fixture-project", connection.brokerID, local, manager, 80, 24)

	done := make(chan error, 1)
	go func() { done <- session.Run() }()
	t.Cleanup(func() { _ = browser.Close() })

	require.NoError(t, broker.SetReadDeadline(time.Now().Add(5*time.Second)))
	var open wsprotocol.StreamOpenMessage
	require.NoError(t, broker.ReadJSON(&open))

	// Close browser with normal close
	require.NoError(t, browser.WriteControl(websocket.CloseMessage,
		websocket.FormatCloseMessage(websocket.CloseNormalClosure, "detach"),
		time.Now().Add(time.Second)))

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("session did not exit")
	}

	// Read the single stream-close
	var closed wsprotocol.StreamCloseMessage
	require.NoError(t, broker.SetReadDeadline(time.Now().Add(2*time.Second)))
	require.NoError(t, broker.ReadJSON(&closed))
	require.Equal(t, open.StreamID, closed.StreamID)

	// Multiple Close() calls must not panic and must not produce extra messages
	session.Close()
	session.Close()

	// No additional stream-close should appear
	require.NoError(t, broker.SetReadDeadline(time.Now().Add(200*time.Millisecond)))
	_, _, err := broker.ReadMessage()
	var timeout net.Error
	require.ErrorAs(t, err, &timeout)
	require.True(t, timeout.Timeout(), "repeated Close() must not emit another stream-close")
}
