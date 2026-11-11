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

//go:build !no_sqlite

package hub

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"testing/iotest"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/conduit"
	"github.com/gorilla/websocket"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestConduitProxyUpstreamLost: when the conduit stream behind a proxied
// response is lost after the response started, the client sees the loss
// (a truncated event stream, a 4504 WebSocket close) rather than a stream
// that ends cleanly with an error body appended. Before the response
// starts, the error is the JSON 502 it always was.
func TestConduitProxyUpstreamLost(t *testing.T) {
	t.Run("event stream truncated, nothing appended", func(t *testing.T) {
		next := make(chan struct{}, 1)
		f := newConduitProxyFixture(t, sseApp(next, 0, 4))
		stop := f.startAgent(t)
		next <- struct{}{}
		resp := f.get(t, "/events", http.Header{"Accept": {"text/event-stream"}})
		require.Equal(t, http.StatusOK, resp.StatusCode)
		br := bufio.NewReader(resp.Body)
		for _, want := range []string{"data: 1\n", "\n"} {
			line, err := br.ReadString('\n')
			require.NoError(t, err)
			require.Equal(t, want, line)
		}

		stop()
		rest, err := io.ReadAll(br)
		require.Error(t, err, "the stream ended as if complete")
		assert.ErrorIs(t, err, io.ErrUnexpectedEOF)
		assert.Empty(t, string(rest), "bytes appended to the event stream")
	})

	t.Run("websocket closed with 4504", func(t *testing.T) {
		up := websocket.Upgrader{}
		f := newConduitProxyFixture(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			c, err := up.Upgrade(w, r, nil)
			if err != nil {
				return
			}
			defer func() { _ = c.Close() }()
			for {
				mt, msg, err := c.ReadMessage()
				if err != nil {
					return
				}
				if err := c.WriteMessage(mt, msg); err != nil {
					return
				}
			}
		}))
		stop := f.startAgent(t)
		wsURL := "ws" + strings.TrimPrefix(f.base, "http") +
			"/api/v1/agents/" + f.launched.ID + "/ports/" + strconv.Itoa(f.app.port) + "/proxy/ws"
		c, _, err := websocket.DefaultDialer.Dial(wsURL, http.Header{"Authorization": {"Bearer " + f.userToken}})
		require.NoError(t, err)
		t.Cleanup(func() { _ = c.Close() })
		require.NoError(t, c.SetReadDeadline(time.Now().Add(10*time.Second)))
		require.NoError(t, c.WriteMessage(websocket.TextMessage, []byte("one")))
		_, got, err := c.ReadMessage()
		require.NoError(t, err)
		require.Equal(t, "one", string(got))

		stop()
		_, _, err = c.ReadMessage()
		var ce *websocket.CloseError
		require.ErrorAs(t, err, &ce)
		assert.Equal(t, wsCloseUpstreamUnreachable, ce.Code)
		assert.Equal(t, "upstream_unreachable", ce.Text)
	})

	t.Run("before the response, JSON 502", func(t *testing.T) {
		f := newConduitProxyFixture(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			// Drop the connection without a response.
			if conn, _, err := http.NewResponseController(w).Hijack(); err == nil {
				_ = conn.Close()
			}
		}))
		f.startAgent(t)
		resp := f.get(t, "/", nil)
		body, err := io.ReadAll(resp.Body)
		require.NoError(t, err)
		require.Equal(t, http.StatusBadGateway, resp.StatusCode, string(body))
		var e struct {
			Error struct {
				Code    string `json:"code"`
				Message string `json:"message"`
			} `json:"error"`
		}
		require.NoError(t, json.Unmarshal(body, &e), string(body))
		assert.Equal(t, ErrCodeRuntimeError, e.Error.Code)
		assert.Equal(t, "Port proxy failed", e.Error.Message)
	})
}

// TestConduitProxyWebSocketHalfClose: a client that half-closes a proxied
// WebSocket still receives what the app sends afterwards, ending with the
// app's own close frame.
func TestConduitProxyWebSocketHalfClose(t *testing.T) {
	up := websocket.Upgrader{}
	f := newConduitProxyFixture(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := up.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer func() { _ = c.Close() }()
		// Read until the client's half-close arrives, then answer.
		for {
			if _, _, err := c.ReadMessage(); err != nil {
				break
			}
		}
		for _, m := range []string{"after-1", "after-2"} {
			if err := c.WriteMessage(websocket.TextMessage, []byte(m)); err != nil {
				return
			}
		}
		_ = c.WriteMessage(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseNormalClosure, "done"))
	}))
	f.startAgent(t)
	wsURL := "ws" + strings.TrimPrefix(f.base, "http") +
		"/api/v1/agents/" + f.launched.ID + "/ports/" + strconv.Itoa(f.app.port) + "/proxy/ws"
	c, _, err := websocket.DefaultDialer.Dial(wsURL, http.Header{"Authorization": {"Bearer " + f.userToken}})
	require.NoError(t, err)
	t.Cleanup(func() { _ = c.Close() })
	require.NoError(t, c.SetReadDeadline(time.Now().Add(10*time.Second)))
	hc, ok := c.UnderlyingConn().(interface{ CloseWrite() error })
	require.True(t, ok, "client connection cannot half-close")
	require.NoError(t, hc.CloseWrite())

	for _, want := range []string{"after-1", "after-2"} {
		_, got, err := c.ReadMessage()
		require.NoError(t, err)
		require.Equal(t, want, string(got))
	}
	_, _, err = c.ReadMessage()
	var ce *websocket.CloseError
	require.ErrorAs(t, err, &ce)
	assert.Equal(t, websocket.CloseNormalClosure, ce.Code)
	assert.Equal(t, "done", ce.Text)
}

// halfCloser records CloseWrite calls.
type halfCloser struct {
	chunkedReader
	closedWrite bool
}

func (h *halfCloser) CloseWrite() error { h.closedWrite = true; return nil }

// TestWSUpstreamBodyCloseWrite: CloseWrite reaches an upstream that can
// half-close and is a no-op otherwise.
func TestWSUpstreamBodyCloseWrite(t *testing.T) {
	hc := &halfCloser{}
	require.NoError(t, (&wsUpstreamBody{ReadWriteCloser: hc}).CloseWrite())
	assert.True(t, hc.closedWrite)
	require.NoError(t, (&wsUpstreamBody{ReadWriteCloser: &chunkedReader{}}).CloseWrite())
}

// wsTestFrame is a final frame of the given opcode carrying n payload
// bytes, with a masking key when masked.
func wsTestFrame(opcode byte, n int, masked bool) []byte {
	return wsTestFragment(true, opcode, n, masked)
}

// wsTestFragment is wsTestFrame with the FIN bit given: a message split
// into fragments is a FIN=0 frame of the message opcode, any FIN=0
// continuation frames (opcode 0), and a FIN=1 continuation frame.
func wsTestFragment(fin bool, opcode byte, n int, masked bool) []byte {
	f := []byte{opcode}
	if fin {
		f[0] |= 0x80
	}
	m := byte(0)
	if masked {
		m = 0x80
	}
	switch {
	case n < 126:
		f = append(f, m|byte(n))
	case n <= 0xffff:
		f = append(f, m|126, byte(n>>8), byte(n))
	default:
		f = append(f, m|127, 0, 0, 0, 0, byte(n>>24), byte(n>>16), byte(n>>8), byte(n))
	}
	if masked {
		f = append(f, 1, 2, 3, 4)
	}
	return append(f, bytes.Repeat([]byte{'x'}, n)...)
}

// chunkedReader returns data in reads of at most size bytes, then err.
// With errWithData, err comes with the read that returns the last bytes
// (n > 0 and an error from a single Read).
type chunkedReader struct {
	data        []byte
	size        int
	err         error
	errWithData bool
}

func (r *chunkedReader) Read(p []byte) (int, error) {
	if len(r.data) == 0 {
		return 0, r.err
	}
	n := min(min(len(p), r.size), len(r.data))
	copy(p, r.data[:n])
	r.data = r.data[n:]
	if r.errWithData && len(r.data) == 0 {
		return n, r.err
	}
	return n, nil
}

func (r *chunkedReader) Write(p []byte) (int, error) { return len(p), nil }
func (r *chunkedReader) Close() error                { return nil }

// TestWSUpstreamBody: the upstream side of a proxied WebSocket gains a
// close frame when it ends on a frame boundary without one of its own,
// and is passed through unchanged otherwise.
func TestWSUpstreamBody(t *testing.T) {
	closeFrame := func(code int, text string) []byte {
		payload := websocket.FormatCloseMessage(code, text)
		return append([]byte{0x88, byte(len(payload))}, payload...)
	}
	lost := closeFrame(wsCloseUpstreamUnreachable, "upstream_unreachable")
	text := wsTestFrame(0x1, 5, false)
	cat := func(parts ...[]byte) []byte { return bytes.Join(parts, nil) }
	first := wsTestFragment(false, 0x1, 4, false)  // FIN=0, text
	middle := wsTestFragment(false, 0x0, 3, false) // FIN=0, continuation
	last := wsTestFragment(true, 0x0, 2, false)    // FIN=1, continuation
	ping := wsTestFrame(0x9, 2, false)
	for _, tc := range []struct {
		name     string
		upstream []byte
		end      error
		want     []byte
	}{
		{name: "eof after a frame", upstream: text, end: io.EOF, want: cat(text, lost)},
		{name: "session lost after a frame", upstream: text, end: conduit.ErrSessionClosed, want: cat(text, lost)},
		{name: "no frames yet", end: conduit.ErrStreamClosed, want: lost},
		{name: "16-bit length", upstream: wsTestFrame(0x2, 300, false), end: io.EOF,
			want: cat(wsTestFrame(0x2, 300, false), lost)},
		{name: "64-bit length", upstream: wsTestFrame(0x2, 70000, false), end: io.EOF,
			want: cat(wsTestFrame(0x2, 70000, false), lost)},
		{name: "masked header", upstream: wsTestFrame(0x1, 3, true), end: io.EOF,
			want: cat(wsTestFrame(0x1, 3, true), lost)},
		{name: "lost mid-payload", upstream: text[:4], end: io.EOF, want: text[:4]},
		{name: "lost mid-header", upstream: wsTestFrame(0x2, 300, false)[:3], end: io.EOF,
			want: wsTestFrame(0x2, 300, false)[:3]},
		{name: "upstream closed first", upstream: cat(text, closeFrame(1000, "")), end: io.EOF,
			want: cat(text, closeFrame(1000, ""))},
		{name: "local limit", upstream: text, end: conduit.ErrBufferBudget,
			want: cat(text, closeFrame(wsCloseInternalError, "internal_error"))},
		// A control frame may come between the fragments of a message,
		// so the close frame is added after any whole fragment.
		{name: "fragmented message complete", upstream: cat(first, middle, last), end: io.EOF,
			want: cat(first, middle, last, lost)},
		{name: "lost between fragments", upstream: cat(first, middle), end: conduit.ErrSessionClosed,
			want: cat(first, middle, lost)},
		{name: "lost mid-fragment", upstream: cat(first, middle[:3]), end: io.EOF,
			want: cat(first, middle[:3])},
		{name: "ping between fragments", upstream: cat(first, ping, last), end: io.EOF,
			want: cat(first, ping, last, lost)},
		{name: "lost after a ping between fragments", upstream: cat(first, ping), end: io.EOF,
			want: cat(first, ping, lost)},
		{name: "close between fragments", upstream: cat(first, closeFrame(1001, "")), end: io.EOF,
			want: cat(first, closeFrame(1001, ""))},
	} {
		for _, chunk := range []int{1, 7, 1 << 20} {
			for _, oneByte := range []bool{false, true} {
				for _, errWithData := range []bool{false, true} {
					name := tc.name + "/chunk=" + strconv.Itoa(chunk) + "/onebyte=" + strconv.FormatBool(oneByte) +
						"/errwithdata=" + strconv.FormatBool(errWithData)
					t.Run(name, func(t *testing.T) {
						b := &wsUpstreamBody{ReadWriteCloser: &chunkedReader{
							data: tc.upstream, size: chunk, err: tc.end, errWithData: errWithData}}
						var r io.Reader = b
						if oneByte {
							r = iotest.OneByteReader(b)
						}
						got, err := io.ReadAll(r)
						if errors.Is(tc.end, io.EOF) {
							require.NoError(t, err)
						} else {
							require.ErrorIs(t, err, tc.end)
						}
						assert.Equal(t, tc.want, got)
					})
				}
			}
		}
	}
}

// TestRecoveryMiddlewarePassesAbort: an aborted response (panic with
// http.ErrAbortHandler) passes the recovery and request-log middleware
// and reaches net/http with nothing written after it; any other panic is
// still answered with a 500.
func TestRecoveryMiddlewarePassesAbort(t *testing.T) {
	s := &Server{}
	for _, tc := range []struct {
		name       string
		panicWith  any
		wantPanic  bool
		wantStatus int
	}{
		{name: "abort", panicWith: http.ErrAbortHandler, wantPanic: true, wantStatus: http.StatusOK},
		{name: "other panic", panicWith: "boom", wantStatus: http.StatusInternalServerError},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := s.loggingMiddleware(s.recoveryMiddleware(
				http.HandlerFunc(func(http.ResponseWriter, *http.Request) { panic(tc.panicWith) })))
			rec := httptest.NewRecorder()
			serve := func() { h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/x", nil)) }
			if tc.wantPanic {
				assert.PanicsWithValue(t, http.ErrAbortHandler, serve)
				assert.Zero(t, rec.Body.Len(), "a body was written after the abort")
			} else {
				assert.NotPanics(t, serve)
			}
			assert.Equal(t, tc.wantStatus, rec.Code)
		})
	}
}
