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
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/conduit"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/gorilla/websocket"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
)

// headerTimeoutTestBound is the short response header bound the tests
// configure in place of the 60s default.
const headerTimeoutTestBound = time.Second

// withHeaderTimeout configures the port proxy's response header bound and
// wires a port proxy counter, returning a reader of its value.
func (f *conduitProxyFixture) withHeaderTimeout(t *testing.T, d time.Duration) (upstreamTimeouts func() int64) {
	t.Helper()
	f.srv.config.PortProxyResponseHeaderTimeout = d
	return portProxyCounter(t, f.srv)
}

// portProxyCounter wires a port proxy counter on s and returns a reader
// of its value.
func portProxyCounter(t *testing.T, s *Server) func() int64 {
	t.Helper()
	reader := sdkmetric.NewManualReader()
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	t.Cleanup(func() { _ = mp.Shutdown(context.Background()) })
	rec, err := NewOTelPortProxyMetrics(mp)
	require.NoError(t, err)
	s.SetPortProxyMetrics(rec)
	return func() int64 {
		var rm metricdata.ResourceMetrics
		require.NoError(t, reader.Collect(context.Background(), &rm))
		var total int64
		for _, sm := range rm.ScopeMetrics {
			for _, m := range sm.Metrics {
				if m.Name != "scion.hub.port_proxy.upstream_timeout" {
					continue
				}
				sum, ok := m.Data.(metricdata.Sum[int64])
				require.True(t, ok, "counter data %T", m.Data)
				for _, dp := range sum.DataPoints {
					total += dp.Value
				}
			}
		}
		return total
	}
}

// silentApp accepts each request and never answers; it reports on closed
// when the agent-side connection of a request has closed.
func silentApp(closed chan<- struct{}) http.Handler {
	return http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
		closed <- struct{}{}
	})
}

// requireStreamsReleased waits until the hub holds no tracked port proxy
// stream.
func (f *conduitProxyFixture) requireStreamsReleased(t *testing.T) {
	t.Helper()
	a := f.srv.conduitAuthz.Load()
	require.NotNil(t, a)
	require.Eventually(t, func() bool { return a.Len() == 0 }, 5*time.Second, 20*time.Millisecond,
		"the hub-side conduit stream was not closed")
}

func proxyErrorCode(t *testing.T, body []byte) string {
	t.Helper()
	var e struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	require.NoError(t, json.Unmarshal(body, &e), string(body))
	return e.Error.Code
}

// TestConduitProxyTransportBounds: the proxy transport bounds the wait
// for response headers and for a 100 Continue, and nothing else.
func TestConduitProxyTransportBounds(t *testing.T) {
	stream, peer := net.Pipe()
	t.Cleanup(func() { _ = stream.Close(); _ = peer.Close() })
	tr := conduitProxyTransport(stream, 7*time.Second)
	assert.Equal(t, 7*time.Second, tr.ResponseHeaderTimeout)
	assert.Equal(t, time.Second, tr.ExpectContinueTimeout)
	assert.Zero(t, tr.IdleConnTimeout)
	assert.Zero(t, tr.TLSHandshakeTimeout)

	s := &Server{}
	assert.Equal(t, config.PortProxyDefaultResponseHeaderTimeout, s.portProxyResponseHeaderTimeout(), "0 is the 60s default")
	s.config.PortProxyResponseHeaderTimeout = 90 * time.Second
	assert.Equal(t, 90*time.Second, s.portProxyResponseHeaderTimeout())

	assert.False(t, isResponseHeaderTimeout(errors.New("conduit: stream reset")))
	assert.False(t, isResponseHeaderTimeout(io.ErrUnexpectedEOF))
	assert.False(t, isResponseHeaderTimeout(nil))
	require.ErrorIs(t, timeoutNetError{}, context.DeadlineExceeded, "the stub mirrors net/http's timeout error")
	assert.True(t, isResponseHeaderTimeout(timeoutNetError{}), "a bare net.Error timeout that wraps context.DeadlineExceeded")
	assert.False(t, isResponseHeaderTimeout(fmt.Errorf("%w: %w", conduit.ErrSessionClosed, timeoutNetError{})),
		"a session closed on a deadline is a lost upstream")
	assert.False(t, isResponseHeaderTimeout(fmt.Errorf("conduit stream: %w", timeoutNetError{})),
		"a wrapped timeout is not the transport's header timeout")
	assert.False(t, isResponseHeaderTimeout(fmt.Errorf("read: %w", context.DeadlineExceeded)))
	assert.False(t, isResponseHeaderTimeout(context.DeadlineExceeded))
}

// timeoutNetError mirrors net/http's response header timeout error: a
// net.Error timeout that also matches context.DeadlineExceeded.
type timeoutNetError struct{}

func (timeoutNetError) Error() string        { return "timeout awaiting response headers" }
func (timeoutNetError) Timeout() bool        { return true }
func (timeoutNetError) Temporary() bool      { return true }
func (timeoutNetError) Is(target error) bool { return target == context.DeadlineExceeded }

// TestConduitProxyHeaderTimeout: a service that accepts and never answers
// gets a 504 runtime_error within the bound plus 2s; the hub-side stream
// and the agent-side connection are both closed, and the timeout is
// counted.
func TestConduitProxyHeaderTimeout(t *testing.T) {
	closed := make(chan struct{}, 4)
	f := newConduitProxyFixture(t, silentApp(closed))
	timeouts := f.withHeaderTimeout(t, headerTimeoutTestBound)
	f.startAgent(t)

	start := time.Now()
	resp := f.getWithin(t, "/slow", nil, headerTimeoutTestBound+2*time.Second)
	elapsed := time.Since(start)
	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	require.Equal(t, http.StatusGatewayTimeout, resp.StatusCode, string(body))
	assert.Equal(t, ErrCodeRuntimeError, proxyErrorCode(t, body))
	assert.GreaterOrEqual(t, elapsed, headerTimeoutTestBound, "answered before the bound")
	assert.Less(t, elapsed, headerTimeoutTestBound+2*time.Second)
	assert.Equal(t, int64(1), timeouts())

	select {
	case <-closed:
	case <-time.After(5 * time.Second):
		t.Fatal("the agent-side connection was not closed")
	}
	f.requireStreamsReleased(t)

	// A browser gets the proxy error page.
	resp = f.getWithin(t, "/slow", http.Header{"Accept": {"text/html"}}, headerTimeoutTestBound+2*time.Second)
	body, err = io.ReadAll(resp.Body)
	require.NoError(t, err)
	require.Equal(t, http.StatusGatewayTimeout, resp.StatusCode)
	assert.Contains(t, resp.Header.Get("Content-Type"), "text/html")
	assert.Contains(t, string(body), "Gateway Timeout")
	assert.Equal(t, int64(2), timeouts())
	select {
	case <-closed:
	case <-time.After(5 * time.Second):
		t.Fatal("the agent-side connection was not closed")
	}
	f.requireStreamsReleased(t)
}

// TestConduitProxyHeadersUnderBound: response headers that arrive just
// under the bound are proxied normally.
func TestConduitProxyHeadersUnderBound(t *testing.T) {
	const bound = 2 * time.Second
	f := newConduitProxyFixture(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-time.After(bound - 500*time.Millisecond):
		case <-r.Context().Done():
			return
		}
		_, _ = io.WriteString(w, "late but fine")
	}))
	timeouts := f.withHeaderTimeout(t, bound)
	f.startAgent(t)

	resp := f.getWithin(t, "/", nil, bound+5*time.Second)
	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, resp.StatusCode, string(body))
	assert.Equal(t, "late but fine", string(body))
	assert.Zero(t, timeouts())
}

// TestConduitProxyEventStreamPastHeaderBound: an event stream whose
// headers arrive promptly keeps flowing past the response header bound.
func TestConduitProxyEventStreamPastHeaderBound(t *testing.T) {
	const events = 5
	interval := 400 * time.Millisecond // 5 events: 2s, twice the bound
	f := newConduitProxyFixture(t, sseApp(nil, interval, events))
	timeouts := f.withHeaderTimeout(t, headerTimeoutTestBound)
	f.startAgent(t)

	start := time.Now()
	resp := f.get(t, "/events", http.Header{"Accept": {"text/event-stream"}})
	require.Equal(t, http.StatusOK, resp.StatusCode)
	readEvents(t, resp.Body, events, func(int) {})
	assert.Greater(t, time.Since(start), headerTimeoutTestBound, "the stream did not outlive the bound")
	assert.Zero(t, timeouts())
}

// TestConduitProxyWebSocketHeaderTimeout: a WebSocket handshake the app
// never answers gets a 504 within the bound plus 2s, and an upgraded
// WebSocket stays open past the bound.
func TestConduitProxyWebSocketHeaderTimeout(t *testing.T) {
	up := websocket.Upgrader{}
	closed := make(chan struct{}, 4)
	f := newConduitProxyFixture(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/silent" {
			silentApp(closed).ServeHTTP(w, r)
			return
		}
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
			if err := c.WriteMessage(mt, append([]byte("echo:"), msg...)); err != nil {
				return
			}
		}
	}))
	timeouts := f.withHeaderTimeout(t, headerTimeoutTestBound)
	f.startAgent(t)
	wsBase := "ws" + strings.TrimPrefix(f.base, "http") +
		"/api/v1/agents/" + f.launched.ID + "/ports/" + strconv.Itoa(f.app.port) + "/proxy"
	auth := http.Header{"Authorization": {"Bearer " + f.userToken}}

	t.Run("handshake not answered", func(t *testing.T) {
		before := timeouts()
		d := websocket.Dialer{HandshakeTimeout: headerTimeoutTestBound + 2*time.Second}
		start := time.Now()
		c, resp, err := d.Dial(wsBase+"/silent", auth)
		if c != nil {
			_ = c.Close()
		}
		require.ErrorIs(t, err, websocket.ErrBadHandshake)
		require.NotNil(t, resp)
		t.Cleanup(func() { _ = resp.Body.Close() })
		assert.Less(t, time.Since(start), headerTimeoutTestBound+2*time.Second)
		assert.Equal(t, http.StatusGatewayTimeout, resp.StatusCode)
		body, err := io.ReadAll(resp.Body)
		require.NoError(t, err)
		assert.Equal(t, ErrCodeRuntimeError, proxyErrorCode(t, body))
		assert.Equal(t, before+1, timeouts())
		select {
		case <-closed:
		case <-time.After(5 * time.Second):
			t.Fatal("the agent-side connection was not closed")
		}
		f.requireStreamsReleased(t)
	})

	t.Run("upgraded connection outlives the bound", func(t *testing.T) {
		before := timeouts()
		c, resp, err := websocket.DefaultDialer.Dial(wsBase+"/ws", auth)
		require.NoError(t, err)
		t.Cleanup(func() { _ = c.Close() })
		require.Equal(t, http.StatusSwitchingProtocols, resp.StatusCode)
		exchange := func(msg string) {
			t.Helper()
			require.NoError(t, c.WriteMessage(websocket.TextMessage, []byte(msg)))
			require.NoError(t, c.SetReadDeadline(time.Now().Add(10*time.Second)))
			_, got, err := c.ReadMessage()
			require.NoError(t, err)
			assert.Equal(t, "echo:"+msg, string(got))
		}
		exchange("one")
		idle := time.NewTimer(2 * headerTimeoutTestBound)
		<-idle.C
		exchange("two")
		assert.Equal(t, before, timeouts(), "no timeout counted for the upgraded connection")
	})
}

// erringStreamConn is a conduit stream stand-in for serveConduitProxy:
// writes succeed, and reads fail with err (or, when err is nil, block
// until the conn is closed).
type erringStreamConn struct {
	err    error
	closed chan struct{}
	once   sync.Once
}

func newErringStreamConn(err error) *erringStreamConn {
	return &erringStreamConn{err: err, closed: make(chan struct{})}
}

func (c *erringStreamConn) Read([]byte) (int, error) {
	if c.err != nil {
		return 0, c.err
	}
	<-c.closed
	return 0, net.ErrClosed
}
func (c *erringStreamConn) Write(p []byte) (int, error)      { return len(p), nil }
func (c *erringStreamConn) Close() error                     { c.once.Do(func() { close(c.closed) }); return nil }
func (c *erringStreamConn) LocalAddr() net.Addr              { return conduitAddr("hub") }
func (c *erringStreamConn) RemoteAddr() net.Addr             { return conduitAddr("agent") }
func (c *erringStreamConn) SetDeadline(time.Time) error      { return nil }
func (c *erringStreamConn) SetReadDeadline(time.Time) error  { return nil }
func (c *erringStreamConn) SetWriteDeadline(time.Time) error { return nil }

// TestConduitProxyTimeoutClassification pins, end to end through
// serveConduitProxy, which upstream failures are a header timeout: only
// the transport's own (unwrapped) response header timeout gives 504, the
// INFO line and the counter. A stream whose session failed on a write
// deadline, or a stream read failing with a wrapped deadline error, is a
// lost upstream: 502 as before, nothing counted, no timeout line.
func TestConduitProxyTimeoutClassification(t *testing.T) {
	const timeoutLine = "the agent port did not answer in time"
	logs := &syncBuffer{}
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prev) })

	for _, tc := range []struct {
		name       string
		readErr    error
		wantStatus int
		wantCount  int64
	}{
		{name: "session closed on a write deadline",
			readErr:    fmt.Errorf("%w: %w", conduit.ErrSessionClosed, timeoutNetError{}),
			wantStatus: http.StatusBadGateway},
		{name: "wrapped deadline error",
			readErr:    fmt.Errorf("conduit stream: %w", context.DeadlineExceeded),
			wantStatus: http.StatusBadGateway},
		{name: "wrapped net timeout (link read)",
			readErr:    fmt.Errorf("conduit link: %w", timeoutNetError{}),
			wantStatus: http.StatusBadGateway},
		{name: "transport response header timeout",
			wantStatus: http.StatusGatewayTimeout, wantCount: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := &Server{}
			s.config.PortProxyResponseHeaderTimeout = 100 * time.Millisecond
			count := portProxyCounter(t, s)
			mark := len(logs.String())
			conn := newErringStreamConn(tc.readErr)
			agent := &store.Agent{ID: "agent-classify"}
			req := httptest.NewRequest(http.MethodGet, "/api/v1/agents/agent-classify/ports/3000/proxy/x", nil)
			rec := httptest.NewRecorder()

			done := make(chan struct{})
			go func() {
				defer close(done)
				s.serveConduitProxy(rec, req, agent, 3000, "/x", conn)
			}()
			select {
			case <-done:
			case <-time.After(5 * time.Second):
				t.Fatal("serveConduitProxy did not return")
			}

			require.Equal(t, tc.wantStatus, rec.Code, rec.Body.String())
			assert.Equal(t, ErrCodeRuntimeError, proxyErrorCode(t, rec.Body.Bytes()))
			assert.Equal(t, tc.wantCount, count())
			newLogs := logs.String()[mark:]
			if tc.wantCount > 0 {
				assert.Contains(t, newLogs, timeoutLine)
				assert.Contains(t, newLogs, "level=INFO")
			} else {
				assert.NotContains(t, newLogs, timeoutLine)
			}
			select {
			case <-conn.closed:
			default:
				t.Error("the stream was not closed")
			}
		})
	}
}
