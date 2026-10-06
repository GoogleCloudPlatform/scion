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

package wsclient

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/wsprotocol"
	"github.com/gorilla/websocket"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/term"
)

// runAgainstCloseServer runs a PTY client against a server that optionally
// sends one data frame and then closes with code/reason. If code is 0 the
// server drops the TCP connection without a close frame.
func runAgainstCloseServer(t *testing.T, sendData bool, code int, reason string) error {
	t.Helper()
	r, w, err := os.Pipe()
	require.NoError(t, err)
	t.Cleanup(func() { _ = r.Close(); _ = w.Close() })

	upgrader := websocket.Upgrader{CheckOrigin: func(r *http.Request) bool { return true }}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer func() { _ = conn.Close() }()
		if sendData {
			_ = conn.WriteJSON(wsprotocol.NewPTYDataMessage([]byte("hello")))
		}
		if code == 0 {
			return // drop without a close frame
		}
		_ = conn.WriteControl(websocket.CloseMessage,
			websocket.FormatCloseMessage(code, reason), time.Now().Add(time.Second))
	}))
	t.Cleanup(srv.Close)

	client := NewPTYClient(PTYClientConfig{Endpoint: srv.URL, Slug: "a1"})
	client.stdin = r
	require.NoError(t, client.Connect(context.Background()))
	return client.Run()
}

func TestRun_CloseCodes(t *testing.T) {
	tests := []struct {
		name       string
		code       int
		reason     string
		wantNil    bool
		wantCode   int
		wantReason string
	}{
		{name: "clean detach", code: wsprotocol.ClosePTYNormal, wantNil: true},
		{name: "going away is not a clean detach", code: wsprotocol.ClosePTYGoingAway, wantCode: wsprotocol.ClosePTYGoingAway},
		{name: "broker disconnected", code: wsprotocol.ClosePTYUpstreamUnavailable, reason: wsprotocol.CloseReasonBrokerDisconnected,
			wantCode: wsprotocol.ClosePTYUpstreamUnavailable, wantReason: wsprotocol.CloseReasonBrokerDisconnected},
		{name: "agent stopped", code: wsprotocol.ClosePTYSessionGone, reason: wsprotocol.CloseReasonAgentStopped,
			wantCode: wsprotocol.ClosePTYSessionGone, wantReason: wsprotocol.CloseReasonAgentStopped},
		{name: "unknown application code", code: 4999, reason: "x", wantCode: 4999, wantReason: "x"},
		{name: "dropped without close frame", code: 0, wantCode: wsprotocol.ClosePTYAbnormal},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := runAgainstCloseServer(t, true, tc.code, tc.reason)
			if tc.wantNil {
				require.NoError(t, err)
				return
			}
			var ce *PTYCloseError
			require.True(t, errors.As(err, &ce), "want *PTYCloseError, got %T: %v", err, err)
			assert.Equal(t, tc.wantCode, ce.Code)
			if tc.wantReason != "" {
				assert.Equal(t, tc.wantReason, ce.Reason)
			}
		})
	}
}

func TestPTYCloseErrorMessage(t *testing.T) {
	assert.Equal(t, "attach session closed by server (code 4410: agent_stopped)",
		(&PTYCloseError{Code: 4410, Reason: "agent_stopped"}).Error())
	assert.Equal(t, "attach session closed by server (code 1006)",
		(&PTYCloseError{Code: 1006}).Error())
}

// captureStdout runs fn with os.Stdout redirected to a pipe and returns
// what fn wrote.
func captureStdout(t *testing.T, fn func(fd int)) string {
	t.Helper()
	orig := os.Stdout
	r, w, err := os.Pipe()
	require.NoError(t, err)
	os.Stdout = w
	fn(int(w.Fd()))
	_ = w.Close()
	os.Stdout = orig
	buf := make([]byte, 4096)
	n, _ := r.Read(buf)
	_ = r.Close()
	return string(buf[:n])
}

func TestRestoreAfterRun(t *testing.T) {
	tests := []struct {
		name         string
		runErr       error
		receivedData bool
		want         string
	}{
		{name: "clean end resets", runErr: nil, want: terminalResetSequences},
		{name: "error after remote drew resets and starts a new line", runErr: errors.New("x"), receivedData: true,
			want: terminalResetSequences + "\r\n"},
		{name: "error before any data leaves screen alone", runErr: errors.New("x"), want: ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := captureStdout(t, func(fd int) {
				c := &PTYClient{oldFd: fd, termState: &term.State{}}
				c.receivedData.Store(tc.receivedData)
				c.restoreAfterRun(tc.runErr)
			})
			assert.Equal(t, tc.want, got)
		})
	}
}

func TestBuildWebSocketURL_KeepsEndpointPathPrefix(t *testing.T) {
	tests := []struct {
		endpoint string
		want     string
	}{
		{"https://hub.example.com", "wss://hub.example.com/api/v1/agents/a1/pty?cols=80&rows=24"},
		{"https://hub.example.com/", "wss://hub.example.com/api/v1/agents/a1/pty?cols=80&rows=24"},
		{"https://example.com/scion", "wss://example.com/scion/api/v1/agents/a1/pty?cols=80&rows=24"},
		{"http://example.com/a/b/", "ws://example.com/a/b/api/v1/agents/a1/pty?cols=80&rows=24"},
	}
	for _, tc := range tests {
		t.Run(tc.endpoint, func(t *testing.T) {
			c := NewPTYClient(PTYClientConfig{Endpoint: tc.endpoint, Slug: "a1", Cols: 80, Rows: 24})
			got, err := c.buildWebSocketURL()
			require.NoError(t, err)
			assert.Equal(t, tc.want, got)
		})
	}
}

func TestJoinEndpointPath(t *testing.T) {
	tests := []struct {
		prefix, apiPath, want string
	}{
		{"", "/api/v1/x", "/api/v1/x"},
		{"", "api/v1/x", "/api/v1/x"},
		{"/", "/api/v1/x", "/api/v1/x"},
		{"/", "api/v1/x", "/api/v1/x"},
		{"/scion", "/api/v1/x", "/scion/api/v1/x"},
		{"/scion", "api/v1/x", "/scion/api/v1/x"},
		{"/scion/", "/api/v1/x", "/scion/api/v1/x"},
		{"/scion/", "api/v1/x", "/scion/api/v1/x"},
		{"/a/b//", "//api/v1/x", "/a/b/api/v1/x"},
	}
	for _, tc := range tests {
		t.Run(tc.prefix+"+"+tc.apiPath, func(t *testing.T) {
			assert.Equal(t, tc.want, joinEndpointPath(tc.prefix, tc.apiPath))
		})
	}
}

func TestConnect_PrefixedEndpointDialsPrefixedPath(t *testing.T) {
	var gotPath string
	upgrader := websocket.Upgrader{CheckOrigin: func(r *http.Request) bool { return true }}
	mux := http.NewServeMux()
	mux.HandleFunc("/scion/api/v1/agents/a1/pty", func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		_ = conn.Close()
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	c := NewPTYClient(PTYClientConfig{Endpoint: srv.URL + "/scion", Slug: "a1"})
	require.NoError(t, c.Connect(context.Background()))
	defer func() { _ = c.Close() }()
	assert.Equal(t, "/scion/api/v1/agents/a1/pty", gotPath)
}

func TestBuildDirectAttachURL_KeepsEndpointPathPrefix(t *testing.T) {
	got, err := BuildDirectAttachURL("https://broker.example.com/rb/", "a1", 80, 24)
	require.NoError(t, err)
	assert.Equal(t, "wss://broker.example.com/rb/api/v1/agents/a1/attach?cols=80&rows=24", got)
}
