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
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
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
	reader := sdkmetric.NewManualReader()
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	t.Cleanup(func() { _ = mp.Shutdown(context.Background()) })
	rec, err := NewOTelPortProxyMetrics(mp)
	require.NoError(t, err)
	f.srv.SetPortProxyMetrics(rec)
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
}

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
		assert.Equal(t, int64(1), timeouts())
		select {
		case <-closed:
		case <-time.After(5 * time.Second):
			t.Fatal("the agent-side connection was not closed")
		}
		f.requireStreamsReleased(t)
	})

	t.Run("upgraded connection outlives the bound", func(t *testing.T) {
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
		assert.Equal(t, int64(1), timeouts(), "no timeout counted for the upgraded connection")
	})
}
