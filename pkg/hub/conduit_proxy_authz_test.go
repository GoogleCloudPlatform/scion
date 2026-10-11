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
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// TestConduitProxyRevocationClosesWebSocket (R2 and R9, unit tier): a
// WebSocket proxied over a conduit TCP stream is closed when its user
// loses port access. The client receives 4401 authz_expired in the close
// frame, the agent-side socket is closed, the stream leaves the re-check
// table, and the agent's conduit session is untouched: it is not closed
// or re-established, and serves the next permitted request.
func TestConduitProxyRevocationClosesWebSocket(t *testing.T) {
	up := websocket.Upgrader{}
	targetClosed := make(chan struct{})
	f := newConduitProxyFixture(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !websocket.IsWebSocketUpgrade(r) {
			_, _ = w.Write([]byte("ok"))
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
				close(targetClosed)
				return
			}
			if err := c.WriteMessage(mt, msg); err != nil {
				return
			}
		}
	}))
	f.startAgent(t)
	authz := f.srv.conduitAuthz.Load()
	require.NotNil(t, authz, "the re-check runs with the relay")

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
	require.Equal(t, 1, authz.Len(), "the proxied stream is tracked")

	// Revoke: the owner leaves the project, which ends its port access.
	ctx := context.Background()
	bindings, err := f.store.ListRoleBindingsForPrincipal(ctx, store.RoleBindingPrincipalUser, f.launched.OwnerID)
	require.NoError(t, err)
	var removed []*store.RoleBinding
	for _, b := range bindings {
		if b.ScopeType == store.RoleScopeProject && b.ScopeID == f.launched.ProjectID {
			require.NoError(t, f.store.DeleteRoleBinding(ctx, b.ID))
			removed = append(removed, b)
		}
	}
	require.NotEmpty(t, removed)
	authz.Recheck(ctx, conduitAuthzTriggerNotify, conduitAuthzMatch{UserID: f.launched.OwnerID})

	_, _, err = c.ReadMessage()
	var ce *websocket.CloseError
	require.ErrorAs(t, err, &ce)
	assert.Equal(t, 4401, ce.Code)
	assert.Equal(t, "authz_expired", ce.Text)
	select {
	case <-targetClosed:
	case <-time.After(10 * time.Second):
		t.Fatal("the agent-side socket was not closed")
	}
	assert.Zero(t, authz.Len())

	// The agent's session carried on: restore access and serve again on
	// the same session.
	for _, b := range removed {
		b.ID = ""
		_, err := f.store.CreateRoleBinding(ctx, b)
		require.NoError(t, err)
	}
	resp := f.get(t, "/", nil)
	assert.Equal(t, http.StatusOK, resp.StatusCode)
	assert.EqualValues(t, 1, f.sessions.Load(), "the agent session was re-established")
}

// proxyWS opens a WebSocket through the port proxy and checks one echo.
func proxyWS(t *testing.T, f *conduitProxyFixture) *websocket.Conn {
	t.Helper()
	wsURL := "ws" + strings.TrimPrefix(f.base, "http") +
		"/api/v1/agents/" + f.launched.ID + "/ports/" + strconv.Itoa(f.app.port) + "/proxy/ws"
	c, _, err := websocket.DefaultDialer.Dial(wsURL, http.Header{"Authorization": {"Bearer " + f.userToken}})
	require.NoError(t, err)
	t.Cleanup(func() { _ = c.Close() })
	require.NoError(t, c.SetReadDeadline(time.Now().Add(10*time.Second)))
	require.NoError(t, c.WriteMessage(websocket.TextMessage, []byte("ping")))
	_, got, err := c.ReadMessage()
	require.NoError(t, err)
	require.Equal(t, "ping", string(got))
	return c
}

// requireAuthzClose reads until the WebSocket closes and checks the code.
func requireAuthzClose(t *testing.T, c *websocket.Conn) {
	t.Helper()
	require.NoError(t, c.SetReadDeadline(time.Now().Add(recheckWait)))
	_, _, err := c.ReadMessage()
	var ce *websocket.CloseError
	require.ErrorAs(t, err, &ce)
	assert.Equal(t, 4401, ce.Code)
	assert.Equal(t, "authz_expired", ce.Text)
}

func echoApp() http.Handler {
	up := websocket.Upgrader{}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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
	})
}

// TestConduitAuthzNotifyInServerStartupOrder: the server starts the relay
// while its event publisher is still the no-op one and sets the real
// publisher afterwards (cmd/server_foreground.go). A revocation committed
// through the API must still close the proxied stream on the notify path.
func TestConduitAuthzNotifyInServerStartupOrder(t *testing.T) {
	f := newConduitProxyFixture(t, echoApp())
	_, noop := f.srv.events.(noopEventPublisher)
	require.True(t, noop, "the relay started before an event publisher was set")
	metrics := newMetricWaiter()
	var m conduitStreamAuthzMetrics = metrics
	f.srv.conduitAuthzMetrics.Store(&m)
	f.srv.SetEventPublisher(NewChannelEventPublisher())
	f.startAgent(t)
	c := proxyWS(t, f)

	rec := doRequest(t, f.srv, http.MethodPatch, "/api/v1/users/"+f.launched.OwnerID, map[string]string{"status": store.UserStatusSuspended})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	requireAuthzClose(t, c)
	metrics.wait(t, "notify/closed/tcp", 1)
}

// TestConduitAuthzResyncInServerStartupOrder: with the publisher set
// after the relay started, a LISTEN reconnect still re-checks every
// stream: a revocation whose event was lost while the listener was down
// closes the proxied stream with trigger=resync.
func TestConduitAuthzResyncInServerStartupOrder(t *testing.T) {
	f := newConduitProxyFixture(t, echoApp())
	metrics := newMetricWaiter()
	var m conduitStreamAuthzMetrics = metrics
	f.srv.conduitAuthzMetrics.Store(&m)
	pub := newListenGapPublisher()
	f.srv.SetEventPublisher(pub)
	f.startAgent(t)
	c := proxyWS(t, f)

	pub.down.Store(true)
	rec := doRequest(t, f.srv, http.MethodPatch, "/api/v1/users/"+f.launched.OwnerID, map[string]string{"status": store.UserStatusSuspended})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	pub.reconnect()

	requireAuthzClose(t, c)
	metrics.wait(t, "resync/closed/tcp", 1)
	assert.NotContains(t, metrics.list(), "notify/closed/tcp")
}

// TestConduitAuthzRebindsOnPublisherChange: replacing the event publisher
// moves the notify subscription to the new one.
func TestConduitAuthzRebindsOnPublisherChange(t *testing.T) {
	f := newConduitProxyFixture(t, echoApp())
	metrics := newMetricWaiter()
	var m conduitStreamAuthzMetrics = metrics
	f.srv.conduitAuthzMetrics.Store(&m)
	first := NewChannelEventPublisher()
	f.srv.SetEventPublisher(first)
	second := NewChannelEventPublisher()
	f.srv.SetEventPublisher(second)
	// The previous binding is released synchronously: the old publisher
	// has no conduit subscriber left.
	first.mu.RLock()
	for _, pattern := range conduitAuthzEventPatterns {
		assert.Empty(t, first.subscribers[pattern], "pattern %q is still subscribed on the old publisher", pattern)
	}
	first.mu.RUnlock()
	f.startAgent(t)
	c := proxyWS(t, f)

	first.PublishRaw(conduitAuthzChangedSubject, conduitAuthzMatch{})
	second.PublishRaw(conduitAuthzChangedSubject, conduitAuthzMatch{UserID: "someone-else"})
	rec := doRequest(t, f.srv, http.MethodPatch, "/api/v1/users/"+f.launched.OwnerID, map[string]string{"status": store.UserStatusSuspended})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	requireAuthzClose(t, c)
	metrics.wait(t, "notify/closed/tcp", 1)
	assert.Equal(t, []string{"notify/closed/tcp"}, metrics.list(), "the old publisher's event was acted on")
}
