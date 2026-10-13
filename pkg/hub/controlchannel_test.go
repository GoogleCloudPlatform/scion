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
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/wsprotocol"
	"github.com/gorilla/websocket"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestControlChannelManager_OnDisconnectCallback(t *testing.T) {
	mgr := NewControlChannelManager(DefaultControlChannelConfig(), slog.Default())

	var mu sync.Mutex
	var receivedBrokerID string
	var receivedSessionID string
	done := make(chan struct{})

	mgr.SetOnDisconnect(func(brokerID, sessionID string) {
		mu.Lock()
		defer mu.Unlock()
		receivedBrokerID = brokerID
		receivedSessionID = sessionID
		close(done)
	})

	// Manually add a connection entry so removeConnection has something to remove
	mgr.mu.Lock()
	mgr.connections[tid("broker-1")] = &BrokerConnection{brokerID: tid("broker-1"), sessionID: "sess-1"}
	mgr.mu.Unlock()

	mgr.removeConnection(tid("broker-1"), "sess-1")

	// Wait for async callback
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for onDisconnect callback")
	}

	mu.Lock()
	defer mu.Unlock()
	assert.Equal(t, tid("broker-1"), receivedBrokerID)
	assert.Equal(t, "sess-1", receivedSessionID)

	// Verify connection was removed
	require.False(t, mgr.IsConnected(tid("broker-1")))
}

// TestControlChannelManager_RemoveStaleSessionNoop verifies that a teardown for
// an OLD session does not remove a NEWER connection that replaced it (flap), and
// does not fire onDisconnect for the stale session.
func TestControlChannelManager_RemoveStaleSessionNoop(t *testing.T) {
	mgr := NewControlChannelManager(DefaultControlChannelConfig(), slog.Default())

	var fired bool
	var mu sync.Mutex
	mgr.SetOnDisconnect(func(brokerID, sessionID string) {
		mu.Lock()
		defer mu.Unlock()
		fired = true
	})

	// Current live connection is session "new".
	mgr.mu.Lock()
	mgr.connections[tid("broker-1")] = &BrokerConnection{brokerID: tid("broker-1"), sessionID: "new"}
	mgr.mu.Unlock()

	// The old session's teardown must be a no-op.
	mgr.removeConnection(tid("broker-1"), "old")

	// Give any (erroneous) async callback a chance to run.
	time.Sleep(100 * time.Millisecond)

	mu.Lock()
	assert.False(t, fired, "onDisconnect must not fire for a stale session")
	mu.Unlock()
	// The live (new) connection must still be present.
	require.True(t, mgr.IsConnected(tid("broker-1")))
}

func TestControlChannelManager_OnDisconnectCallback_NilSafe(t *testing.T) {
	mgr := NewControlChannelManager(DefaultControlChannelConfig(), slog.Default())

	// Don't set any callback - verify removeConnection doesn't panic
	mgr.mu.Lock()
	mgr.connections[tid("broker-2")] = &BrokerConnection{brokerID: tid("broker-2"), sessionID: "sess-2"}
	mgr.mu.Unlock()

	// This should not panic
	mgr.removeConnection(tid("broker-2"), "sess-2")

	require.False(t, mgr.IsConnected(tid("broker-2")))
}

// TestBrokerConnection_Close_ZeroValueNoPanic proves that Close() on a
// BrokerConnection built from a bare struct literal (nil cancel, nil conn,
// nil maps) does not panic. Test fixtures across pkg/hub (e.g.
// broker_provider_selfheal_test.go) inject such literals directly into
// ControlChannelManager.connections without going through
// addConnection/AddTestConnection, so Close() must tolerate them whenever a
// server-owned cleanup path reaches these connections (ptone/scion#2433).
func TestBrokerConnection_Close_ZeroValueNoPanic(t *testing.T) {
	hc := &BrokerConnection{brokerID: tid("broker-zero"), sessionID: "sess-zero"}

	require.NotPanics(t, hc.Close)
}

// --- Stream backpressure (ptone/scion#3309) ---

// streamFrameJSON encodes a broker->Hub stream data frame.
func streamFrameJSON(t *testing.T, streamID string, data []byte) []byte {
	t.Helper()
	b, err := json.Marshal(wsprotocol.NewStreamFrame(streamID, data))
	require.NoError(t, err)
	return b
}

// handleWithin runs m.handleMessage and fails the test if it does not
// return, which is how a blocked control-channel read loop shows up.
func handleWithin(t *testing.T, m *ControlChannelManager, hc *BrokerConnection, msg []byte) {
	t.Helper()
	done := make(chan error, 1)
	go func() { done <- m.handleMessage(hc, msg) }()
	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(5 * time.Second):
		t.Fatal("control-channel read loop blocked in handleMessage")
	}
}

// patterned returns n bytes whose content depends on seed and position, so a
// lost, duplicated or reordered frame changes the reassembled output.
func patterned(seed, n int) []byte {
	b := make([]byte, n)
	for i := range b {
		b[i] = byte((seed*31 + i) % 251)
	}
	return b
}

// A stream accepts output up to StreamOutputLimit without a reader and
// returns it intact and in order; one byte more is refused without blocking.
func TestStreamProxy_OutputUpToLimitIsQueuedIntact(t *testing.T) {
	s := NewStreamProxy("s", wsprotocol.StreamTypePTY, "agent")
	const frame = 16 << 10
	var want bytes.Buffer
	for i := 0; want.Len() < StreamOutputLimit; i++ {
		data := patterned(i, frame)
		require.NoError(t, s.Write(data), "frame %d", i)
		want.Write(data)
	}
	require.Equal(t, StreamOutputLimit, want.Len())
	require.ErrorIs(t, s.Write([]byte{'x'}), errStreamOverflow)

	var got bytes.Buffer
	for got.Len() < want.Len() {
		data, err := s.Read(context.Background())
		require.NoError(t, err)
		got.Write(data)
	}
	require.True(t, bytes.Equal(want.Bytes(), got.Bytes()), "queued output must arrive intact and in order")
	require.NoError(t, s.Write([]byte{'y'}), "space is released as the reader drains")
}

// Frames queued before a close are still returned before the close error.
func TestStreamProxy_DrainsQueuedOutputBeforeClose(t *testing.T) {
	s := NewStreamProxy("s", wsprotocol.StreamTypePTY, "agent")
	require.NoError(t, s.Write([]byte("[detached]")))
	s.CloseWith(wsprotocol.ClosePTYNormal, "")
	data, err := s.Read(context.Background())
	require.NoError(t, err)
	require.Equal(t, "[detached]", string(data))
	_, err = s.Read(context.Background())
	var sce *StreamClosedError
	require.ErrorAs(t, err, &sce)
	require.Equal(t, wsprotocol.ClosePTYNormal, sce.Code)
	require.ErrorIs(t, s.Write([]byte("late")), errStreamClosed)
}

// A stream whose reader stops is closed with 1013 slow_consumer once it
// exceeds StreamOutputLimit. The control-channel read loop never blocks
// (more than the old 256-frame buffer is fed with no reader), the broker is
// told to close the stream, and another stream and a tunneled request on the
// same broker connection keep working.
func TestControlChannel_SlowConsumerClosedWithCodeOtherStreamsLive(t *testing.T) {
	hubSide, brokerSide := lifecycleWebSocketPair(t)
	m := NewControlChannelManager(DefaultControlChannelConfig(), slog.Default())
	hc := &BrokerConnection{
		brokerID:        "bp-broker",
		conn:            wsprotocol.NewConnection(hubSide, wsprotocol.ConnectionConfig{WriteWait: 5 * time.Second}),
		log:             slog.Default(),
		pendingRequests: make(map[string]chan *wsprotocol.ResponseEnvelope),
		streams:         make(map[string]*StreamProxy),
		ctx:             context.Background(),
		cancel:          func() {},
	}
	slow := NewStreamProxy("slow", wsprotocol.StreamTypePTY, "a")
	live := NewStreamProxy("live", wsprotocol.StreamTypePTY, "b")
	hc.streams["slow"], hc.streams["live"] = slow, live

	// Nobody reads "slow". 16 KiB frames: the limit is 512 frames, twice
	// the 256-frame channel that used to block this loop.
	const frame = 16 << 10
	frames := StreamOutputLimit/frame + 1
	require.Greater(t, frames, 256)
	for i := 0; i < frames; i++ {
		handleWithin(t, m, hc, streamFrameJSON(t, "slow", patterned(i, frame)))
	}

	_, err := slow.Read(context.Background())
	var sce *StreamClosedError
	require.ErrorAs(t, err, &sce)
	require.Equal(t, wsprotocol.ClosePTYTryAgainLater, sce.Code)
	require.Equal(t, closeReasonSlowConsumer, sce.Reason)
	require.Equal(t, wsprotocol.DispositionRetry, wsprotocol.ClassifyPTYClose(sce.Code))

	hc.streamsMu.RLock()
	_, stillRegistered := hc.streams["slow"]
	hc.streamsMu.RUnlock()
	require.False(t, stillRegistered, "a closed slow stream must be released")

	// The broker is told to stop sending on the slow stream.
	require.NoError(t, brokerSide.SetReadDeadline(time.Now().Add(5*time.Second)))
	var closeMsg wsprotocol.StreamCloseMessage
	require.NoError(t, brokerSide.ReadJSON(&closeMsg))
	require.Equal(t, wsprotocol.TypeStreamClose, closeMsg.Type)
	require.Equal(t, "slow", closeMsg.StreamID)
	require.Equal(t, wsprotocol.ClosePTYTryAgainLater, closeMsg.Code)

	// Late frames for the closed stream are ignored without blocking.
	handleWithin(t, m, hc, streamFrameJSON(t, "slow", []byte("late")))

	// The other stream on the same connection is still live.
	handleWithin(t, m, hc, streamFrameJSON(t, "live", []byte("still here")))
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	data, err := live.Read(ctx)
	require.NoError(t, err)
	require.Equal(t, "still here", string(data))

	// So are tunneled requests.
	respCh := make(chan *wsprotocol.ResponseEnvelope, 1)
	hc.pendingRequests["req-1"] = respCh
	resp, err := json.Marshal(wsprotocol.ResponseEnvelope{Type: wsprotocol.TypeResponse, RequestID: "req-1", StatusCode: 200})
	require.NoError(t, err)
	handleWithin(t, m, hc, resp)
	select {
	case got := <-respCh:
		require.Equal(t, 200, got.StatusCode)
	default:
		t.Fatal("tunneled response was not delivered")
	}
}

// The broker's input-overflow close (1009 input_overflow) reaches the PTY
// client unchanged, so the client can report it.
func TestPTYCloseCode_InputOverflowPassesThrough(t *testing.T) {
	f := startCloseCodeSession(t)
	require.NoError(t, f.broker.WriteJSON(wsprotocol.NewStreamCloseMessage(f.open.StreamID, "input_overflow", websocket.CloseMessageTooBig)))
	_, ce := readBrowserUntilClose(t, f.browser)
	require.Equal(t, websocket.CloseMessageTooBig, ce.Code)
	require.Equal(t, "input_overflow", ce.Text)
	require.Equal(t, wsprotocol.DispositionTerminal, wsprotocol.ClassifyPTYClose(ce.Code))
	err := f.waitDone(t)
	var sce *StreamClosedError
	require.True(t, errors.As(err, &sce))
}
