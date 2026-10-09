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
	"sync/atomic"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/wsprotocol"
	"github.com/gorilla/websocket"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// preflightHub serves /api/v1/agents/a1/pty: a plain GET gets status and
// body; a WebSocket upgrade is counted (and accepted, then closed 1000).
type preflightHub struct {
	srv      *httptest.Server
	upgrades atomic.Int32
	gets     atomic.Int32
	auth     atomic.Value
}

func newPreflightHub(t *testing.T, status int, body string) *preflightHub {
	t.Helper()
	h := &preflightHub{}
	upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	h.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/agents/a1/pty" {
			http.NotFound(w, r)
			return
		}
		if websocket.IsWebSocketUpgrade(r) {
			h.upgrades.Add(1)
			conn, err := upgrader.Upgrade(w, r, nil)
			if err != nil {
				return
			}
			_ = conn.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(1000, ""), time.Now().Add(time.Second))
			_ = conn.Close()
			return
		}
		h.gets.Add(1)
		h.auth.Store(r.Header.Get("Authorization"))
		if status != http.StatusOK {
			w.Header().Set("Content-Type", "application/json")
		}
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(h.srv.Close)
	return h
}

const noPathBody = `{"error":{"code":"runtime_attach_unsupported","message":"The agent's runtime does not support attach, and the agent has no conduit session that serves PTY","details":{"reason":"agent_pty_unavailable","path":"none"}}}`

func TestPreflight_OK(t *testing.T) {
	h := newPreflightHub(t, http.StatusOK, `{"path":"agent"}`)
	c := NewPTYClient(PTYClientConfig{Endpoint: h.srv.URL, Token: "tok", Slug: "a1"})
	require.NoError(t, c.Preflight(context.Background()))
	assert.EqualValues(t, 1, h.gets.Load())
	assert.EqualValues(t, 0, h.upgrades.Load(), "the preflight is not a WebSocket upgrade")
	assert.Equal(t, "Bearer tok", h.auth.Load())
}

func TestPreflight_Refusals(t *testing.T) {
	tests := []struct {
		name       string
		status     int
		body       string
		wantNoPath bool
		wantCode   string
		wantReason string
		wantText   string
	}{
		{name: "no path", status: 503, body: noPathBody, wantNoPath: true,
			wantCode: wsprotocol.ErrCodeRuntimeAttachUnsupported, wantReason: "agent_pty_unavailable",
			wantText: "status 503 (runtime_attach_unsupported, reason agent_pty_unavailable): The agent's runtime does not support attach"},
		{name: "broker not connected", status: 503,
			body:     `{"error":{"code":"runtime_broker_unavailable","message":"Runtime broker not connected","details":{"reason":"broker_not_connected","path":"none"}}}`,
			wantCode: "runtime_broker_unavailable", wantReason: "broker_not_connected", wantText: "status 503"},
		{name: "forbidden", status: 403, body: `{"error":{"code":"forbidden","message":"no"}}`,
			wantCode: "forbidden", wantText: "status 403 (forbidden): no"},
		{name: "plain text body", status: 502, body: "bad gateway\n", wantText: "status 502: bad gateway"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			h := newPreflightHub(t, tc.status, tc.body)
			c := NewPTYClient(PTYClientConfig{Endpoint: h.srv.URL, Slug: "a1"})
			err := c.Preflight(context.Background())
			var pe *PTYPreflightError
			require.True(t, errors.As(err, &pe), "got %T: %v", err, err)
			assert.Equal(t, tc.status, pe.Status)
			assert.Equal(t, tc.wantCode, pe.Code)
			assert.Equal(t, tc.wantReason, pe.Reason)
			assert.Equal(t, tc.wantNoPath, pe.NoPath())
			assert.Contains(t, err.Error(), tc.wantText)
		})
	}
}

// TestAttachToAgent_PreflightRefusalNeverDials: a refused preflight ends the
// attach at once, with no WebSocket dial and no retry.
func TestAttachToAgent_PreflightRefusalNeverDials(t *testing.T) {
	h := newPreflightHub(t, http.StatusServiceUnavailable, noPathBody)
	err := AttachToAgent(context.Background(), h.srv.URL, "tok", "a1")
	var pe *PTYPreflightError
	require.True(t, errors.As(err, &pe), "got %T: %v", err, err)
	assert.True(t, pe.NoPath())
	assert.EqualValues(t, 1, h.gets.Load(), "exactly one preflight, no retry")
	assert.EqualValues(t, 0, h.upgrades.Load(), "no WebSocket dial after a refused preflight")
}

// TestAttachToAgent_PreflightOKDials: a 200 preflight proceeds to the dial.
func TestAttachToAgent_PreflightOKDials(t *testing.T) {
	h := newPreflightHub(t, http.StatusOK, `{"path":"agent"}`)
	c := NewPTYClient(PTYClientConfig{Endpoint: h.srv.URL, Token: "tok", Slug: "a1"})
	require.NoError(t, c.Preflight(context.Background()))
	require.NoError(t, c.Connect(context.Background()))
	t.Cleanup(func() { _ = c.Close() })
	assert.EqualValues(t, 1, h.upgrades.Load())
}
