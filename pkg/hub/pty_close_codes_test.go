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
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/GoogleCloudPlatform/scion/pkg/wsprotocol"
	"github.com/gorilla/websocket"
	"github.com/stretchr/testify/require"
)

// Hub loopback tests for the PTY close-code contract (ptone/scion#1970). The
// broker peer is a real control-channel WebSocket accepted by
// ControlChannelManager.HandleUpgrade, so broker messages go through the
// production read loop. The browser peer is a loopback WebSocket; no real
// agent is involved.

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

// AC-C3: broker close codes pass through, legacy codes are mapped, and data
// written before the close reaches the browser before the close frame.
func TestPTYCloseCode_BrokerStreamClose(t *testing.T) {
	cases := []struct {
		name       string
		brokerCode int
		reason     string
		wantCode   int
	}{
		{"session gone passes through", 4410, "session_ended", 4410},
		{"upstream unavailable passes through", 4503, "runtime_stream_dropped", 4503},
		{"normal closure passes through", 1000, "", 1000},
		{"legacy 0 maps to 1000", 0, "", 1000},
		{"legacy 404 maps to 4404", 404, "agent not found", 4404},
		{"legacy 500 maps to 1011", 500, "internal error", 1011},
		{"unknown code maps to 1011", 42, "", 1011},
		{"reserved local-only code is not put on the wire", 1006, "", 1011},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := startCloseCodeSession(t)
			final := []string{"line one\r\n", "line two\r\n", "[detached (from session scion)]\r\n"}
			for _, chunk := range final {
				require.NoError(t, f.broker.WriteJSON(wsprotocol.NewStreamFrame(f.open.StreamID, []byte(chunk))))
			}
			require.NoError(t, f.broker.WriteJSON(wsprotocol.NewStreamCloseMessage(f.open.StreamID, tc.reason, tc.brokerCode)))

			frames, ce := readBrowserUntilClose(t, f.browser)
			require.Equal(t, final, frames, "every frame written before the close must arrive, in order, before it")
			require.Equal(t, tc.wantCode, ce.Code)
			if tc.wantCode == tc.brokerCode {
				require.Equal(t, tc.reason, ce.Text, "reason passes through with the code")
			}
			err := f.waitDone(t)
			var sce *StreamClosedError
			require.ErrorAs(t, err, &sce)
			code, _ := f.session.CloseCause()
			require.Equal(t, tc.wantCode, code)
			require.True(t, f.manager.IsConnected("close-code-broker"), "a stream close must not drop the broker")
		})
	}
}

// A broker reason that is not valid UTF-8 must not cost the client the real
// code: browsers fail the connection (1006) on an invalid close reason. The
// raw bytes are written so the invalid sequence reaches the Hub on the wire.
// On this path encoding/json already replaces invalid bytes with U+FFFD
// before TruncateCloseReason sees them; the test guards the end-to-end
// property, and TestTruncateCloseReason_InvalidUTF8 covers the sanitizer.
func TestPTYCloseCode_InvalidUTF8ReasonKeepsCode(t *testing.T) {
	f := startCloseCodeSession(t)
	raw := []byte(`{"type":"` + wsprotocol.TypeStreamClose + `","streamId":"` + f.open.StreamID +
		`","reason":"session` + "\xff\xfe" + `_ended","code":4410}`)
	require.NoError(t, f.broker.WriteMessage(websocket.TextMessage, raw))

	_, ce := readBrowserUntilClose(t, f.browser)
	require.Equal(t, wsprotocol.ClosePTYSessionGone, ce.Code)
	require.True(t, utf8.ValidString(ce.Text), "close reason must be valid UTF-8: %q", ce.Text)
	require.LessOrEqual(t, len(ce.Text), wsprotocol.MaxCloseReasonBytes)
	_ = f.waitDone(t)
}

// AC-C3: losing the broker control channel mid-session closes the browser
// with 4503.
func TestPTYCloseCode_BrokerDisconnectSends4503(t *testing.T) {
	f := startCloseCodeSession(t)
	require.NoError(t, f.broker.WriteJSON(wsprotocol.NewStreamFrame(f.open.StreamID, []byte("before loss"))))
	// Drop the broker's control channel without a close handshake.
	_ = f.broker.UnderlyingConn().(*net.TCPConn).SetLinger(0)
	_ = f.broker.Close()

	frames, ce := readBrowserUntilClose(t, f.browser)
	require.Equal(t, []string{"before loss"}, frames)
	require.Equal(t, wsprotocol.ClosePTYUpstreamUnavailable, ce.Code)
	require.Equal(t, wsprotocol.CloseReasonBrokerDisconnected, ce.Text)
	_ = f.waitDone(t)
}

// BrokerConnection.Close (the path taken on control-channel loss and Hub
// control-channel shutdown) records 4503 on every open stream.
func TestPTYCloseCode_BrokerConnectionCloseMarksStreams(t *testing.T) {
	brokerLocal, _ := lifecycleWebSocketPair(t)
	connection := &BrokerConnection{
		brokerID: "bc-broker",
		conn:     wsprotocol.NewConnection(brokerLocal, wsprotocol.ConnectionConfig{WriteWait: time.Second}),
		streams:  make(map[string]*StreamProxy),
		ctx:      context.Background(),
		cancel:   func() {},
	}
	a, b := NewStreamProxy("a", wsprotocol.StreamTypePTY, "x"), NewStreamProxy("b", wsprotocol.StreamTypePTY, "y")
	connection.streams["a"], connection.streams["b"] = a, b
	connection.Close()
	for _, s := range []*StreamProxy{a, b} {
		_, err := s.Read(context.Background())
		var sce *StreamClosedError
		require.ErrorAs(t, err, &sce)
		require.Equal(t, wsprotocol.ClosePTYUpstreamUnavailable, sce.Code)
		require.Equal(t, wsprotocol.CloseReasonBrokerDisconnected, sce.Reason)
	}
}

// AC-C3: a stream-open failure closes the browser with 4503, not 1000.
func TestPTYCloseCode_StreamOpenFailureSends4503(t *testing.T) {
	t.Run("broker not connected", func(t *testing.T) {
		manager := NewControlChannelManager(DefaultControlChannelConfig(), slog.Default())
		local, browser := lifecycleWebSocketPair(t)
		session := newPTYSession(context.Background(), "agent", "project", "absent-broker", local, manager, 80, 24)
		err := session.Run()
		require.Error(t, err)
		session.Close() // the handler's deferred Close must not override the code
		_, ce := readBrowserUntilClose(t, browser)
		require.Equal(t, wsprotocol.ClosePTYUpstreamUnavailable, ce.Code)
		require.Equal(t, wsprotocol.CloseReasonStreamOpenFailed, ce.Text)
	})
	t.Run("stream_open write fails", func(t *testing.T) {
		brokerLocal, _ := lifecycleWebSocketPair(t)
		manager := NewControlChannelManager(DefaultControlChannelConfig(), slog.Default())
		connection := &BrokerConnection{
			brokerID: "broken-broker",
			conn:     wsprotocol.NewConnection(brokerLocal, wsprotocol.ConnectionConfig{WriteWait: time.Second}),
			streams:  make(map[string]*StreamProxy),
		}
		manager.connections[connection.brokerID] = connection
		require.NoError(t, brokerLocal.Close())
		local, browser := lifecycleWebSocketPair(t)
		session := newPTYSession(context.Background(), "agent", "project", connection.brokerID, local, manager, 80, 24)
		require.Error(t, session.Run())
		_, ce := readBrowserUntilClose(t, browser)
		require.Equal(t, wsprotocol.ClosePTYUpstreamUnavailable, ce.Code)
		require.Equal(t, wsprotocol.CloseReasonStreamOpenFailed, ce.Text)
	})
}

// A client ping frame is answered with a pong (P3 liveness capability).
func TestPTYCloseCode_ClientPingGetsPong(t *testing.T) {
	f := startCloseCodeSession(t)
	require.NoError(t, f.browser.WriteJSON(wsprotocol.NewPingMessage()))
	require.NoError(t, f.browser.SetReadDeadline(time.Now().Add(5*time.Second)))
	var pong wsprotocol.PongMessage
	require.NoError(t, f.browser.ReadJSON(&pong))
	require.Equal(t, wsprotocol.TypePong, pong.Type)

	// The ping is answered by the Hub and is not forwarded to the broker.
	require.NoError(t, f.broker.SetReadDeadline(time.Now().Add(200*time.Millisecond)))
	_, _, err := f.broker.ReadMessage()
	var timeout net.Error
	require.ErrorAs(t, err, &timeout)
	require.True(t, timeout.Timeout(), "ping must not reach the broker")
}

// A client that disappears without a close frame is not recorded as a clean
// 1000 detach.
func TestPTYCloseCode_ClientLossIsNotNormalClosure(t *testing.T) {
	f := startCloseCodeSession(t)
	_ = f.browser.UnderlyingConn().(*net.TCPConn).SetLinger(0)
	_ = f.browser.Close()
	_ = f.waitDone(t)
	code, reason := f.session.CloseCause()
	require.NotEqual(t, websocket.CloseNormalClosure, code)
	require.Equal(t, wsprotocol.ClosePTYInternalError, code)
	require.True(t, reason == wsprotocol.CloseReasonClientReadFailed || reason == wsprotocol.CloseReasonClientWriteFailed, reason)
}

// A deliberate client close is recorded as 1000.
func TestPTYCloseCode_ClientCloseIsNormalClosure(t *testing.T) {
	f := startCloseCodeSession(t)
	require.NoError(t, f.browser.WriteControl(websocket.CloseMessage,
		websocket.FormatCloseMessage(websocket.CloseNormalClosure, "detach"), time.Now().Add(time.Second)))
	_ = f.waitDone(t)
	code, _ := f.session.CloseCause()
	require.Equal(t, websocket.CloseNormalClosure, code)
}

// StreamProxy.Read returns every frame queued before the close, then the
// close cause. Repeated to exercise select's random choice between ready
// channels.
func TestStreamProxy_ReadDrainsBeforeCloseError(t *testing.T) {
	for i := 0; i < 200; i++ {
		proxy := NewStreamProxy("s", wsprotocol.StreamTypePTY, "a")
		for j := 0; j < 5; j++ {
			require.NoError(t, proxy.Write([]byte(fmt.Sprintf("frame-%d", j))))
		}
		proxy.CloseWith(4410, "session_ended")
		proxy.CloseWith(1000, "") // first close wins
		for j := 0; j < 5; j++ {
			data, err := proxy.Read(context.Background())
			require.NoError(t, err, "iteration %d frame %d", i, j)
			require.Equal(t, fmt.Sprintf("frame-%d", j), string(data))
		}
		_, err := proxy.Read(context.Background())
		var sce *StreamClosedError
		require.ErrorAs(t, err, &sce)
		require.Equal(t, 4410, sce.Code)
		require.Equal(t, "session_ended", sce.Reason)
		require.Contains(t, sce.Error(), "4410")
	}
}

// TestStreamProxy_CloseErrorNilBeforeClose pins that closeError returns a
// true nil error, not an error interface wrapping a nil *StreamClosedError,
// when the stream has not been closed yet. A typed-nil pointer returned
// through an error-typed value would make err != nil true even though
// nothing failed; require.NoError below fails exactly that case, since it
// compares err against nil the same way a real caller would.
func TestStreamProxy_CloseErrorNilBeforeClose(t *testing.T) {
	proxy := NewStreamProxy("s", wsprotocol.StreamTypePTY, "a")
	require.NoError(t, proxy.closeError())
}

func TestPTYCloseCause_Mapping(t *testing.T) {
	cases := []struct {
		name       string
		err        error
		wantCode   int
		wantReason string
	}{
		{"stream closed", &StreamClosedError{Code: 4410, Reason: "agent_stopped"}, 4410, "agent_stopped"},
		{"stream open", &ptyStreamOpenError{err: errors.New("x")}, 4503, wsprotocol.CloseReasonStreamOpenFailed},
		{"broker write", &ptyBrokerWriteError{err: errors.New("x")}, 4503, wsprotocol.CloseReasonBrokerWriteFailed},
		{"client close frame", &ptyClientReadError{err: &websocket.CloseError{Code: 1001}}, 1000, ""},
		{"client read error", &ptyClientReadError{err: errors.New("i/o timeout")}, 1011, wsprotocol.CloseReasonClientReadFailed},
		{"client write error", &ptyClientWriteError{err: errors.New("broken pipe")}, 1011, wsprotocol.CloseReasonClientWriteFailed},
		{"context cancelled", context.Canceled, 1011, wsprotocol.CloseReasonInternalError},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			code, reason := ptyCloseCause(tc.err)
			require.Equal(t, tc.wantCode, code)
			require.Equal(t, tc.wantReason, reason)
		})
	}
}

// TestIsExpectedPTYEnd_Mapping covers what counts as an ordinary end of a PTY
// session rather than a Hub-side failure worth logging as an error,
// including a canceled session context: closeWith's own s.cancel(), or an
// external Close() (the handler's defer, or a test) racing a still-blocked
// read. This does not change the close code sent to the client:
// TestPTYCloseCause_Mapping's "context cancelled" case above pins that
// ptyCloseCause(context.Canceled) is unaffected by this function and still
// maps to 1011/internal_error.
func TestIsExpectedPTYEnd_Mapping(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"eof", io.EOF, true},
		{"context canceled", context.Canceled, true},
		{"wrapped context canceled", fmt.Errorf("read: %w", context.Canceled), true},
		{"stream closed", &StreamClosedError{Code: 4410, Reason: "agent_stopped"}, true},
		{"client close frame", &websocket.CloseError{Code: 1001}, true},
		{"unexpected error", errors.New("boom"), false},
		{"deadline exceeded is not cancellation", context.DeadlineExceeded, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, tc.want, isExpectedPTYEnd(tc.err))
		})
	}
}
