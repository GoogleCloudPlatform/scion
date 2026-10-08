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
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/conduit/registry"
	"github.com/GoogleCloudPlatform/scion/pkg/conduit/relay"
	"github.com/GoogleCloudPlatform/scion/pkg/conduit/router"
	"github.com/GoogleCloudPlatform/scion/pkg/wsprotocol"
	"github.com/gorilla/websocket"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeBroker is a broker connected to srv's control channel through the
// real upgrade path, so the hub runs its normal message loop for it.
type fakeBroker struct {
	ws        *websocket.Conn
	sessionID string
}

func connectFakeBroker(t *testing.T, srv *Server, brokerID string) *fakeBroker {
	t.Helper()
	require.NotNil(t, srv.controlChannel)
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = srv.controlChannel.HandleUpgrade(w, r, brokerID)
	}))
	t.Cleanup(ts.Close)
	ws, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(ts.URL, "http"), nil)
	require.NoError(t, err)
	t.Cleanup(func() { _ = ws.Close() })
	require.NoError(t, ws.SetReadDeadline(time.Now().Add(5*time.Second)))
	var connected wsprotocol.ConnectedMessage
	require.NoError(t, ws.ReadJSON(&connected))
	require.Equal(t, wsprotocol.TypeConnected, connected.Type)
	require.NotEmpty(t, connected.SessionID)
	return &fakeBroker{ws: ws, sessionID: connected.SessionID}
}

// installBrokerConnection registers a bare connection for brokerID under
// sessionID, as a newer control channel would.
func installBrokerConnection(t *testing.T, cc *ControlChannelManager, brokerID, sessionID string) *BrokerConnection {
	t.Helper()
	hc := &BrokerConnection{brokerID: brokerID, sessionID: sessionID, streams: map[string]*StreamProxy{}}
	cc.mu.Lock()
	cc.connections[brokerID] = hc
	cc.mu.Unlock()
	t.Cleanup(func() {
		cc.mu.Lock()
		if cc.connections[brokerID] == hc {
			delete(cc.connections, brokerID)
		}
		cc.mu.Unlock()
	})
	return hc
}

func brokerRequest(id string) router.Request {
	return router.Request{Op: router.OpStream, Kind: registry.PrincipalBroker, ID: id}
}

// TestLegacyBrokerAdapter_OnOwnerResolvesLocalSession: the hub's router,
// on the node that holds the broker's control channel, resolves the
// broker to that channel (a local legacy session), not to a conduit
// session.
func TestLegacyBrokerAdapter_OnOwnerResolvesLocalSession(t *testing.T) {
	f := newRelayFixture(t, nil)
	b := connectFakeBroker(t, f.srv, "broker-1")
	rtr := f.srv.conduit.Load().router

	res, err := rtr.Resolve(context.Background(), brokerRequest("broker-1"), nil)
	require.NoError(t, err)
	assert.Nil(t, res.Session, "a broker must not resolve to a conduit session before Phase 3")
	require.NotNil(t, res.Legacy)
	assert.True(t, res.Local)
	assert.Equal(t, b.sessionID, res.Record.SessionID)
	assert.Equal(t, "hub-a", res.Record.RelayInstanceID)
	assert.Equal(t, legacyBrokerTransport, res.Record.Transport)
	ls, ok := res.Legacy.(*legacyBrokerSession)
	require.True(t, ok, "legacy session type %T", res.Legacy)
	assert.Equal(t, "broker-1", ls.BrokerID())
	assert.Equal(t, b.sessionID, ls.SessionID())
	kind, id := ls.LegacyTarget()
	assert.Equal(t, registry.PrincipalBroker, kind)
	assert.Equal(t, "broker-1", id)

	// The session opens streams on the broker's control channel.
	st, err := ls.OpenStream(context.Background(), wsprotocol.StreamTypePTY, "slug", "proj", 80, 24)
	require.NoError(t, err)
	require.NotNil(t, st)
	require.NoError(t, b.ws.SetReadDeadline(time.Now().Add(5*time.Second)))
	var open wsprotocol.StreamOpenMessage
	require.NoError(t, b.ws.ReadJSON(&open))
	assert.Equal(t, wsprotocol.TypeStreamOpen, open.Type)
	assert.Equal(t, wsprotocol.StreamTypePTY, open.StreamType)
	assert.Equal(t, "slug", open.Slug)
	assert.Equal(t, "proj", open.ProjectID)
	assert.Equal(t, 80, open.Cols)
	assert.Equal(t, 24, open.Rows)
}

// TestLegacyBrokerAdapter_ClosedChannelIsStale: a resolved connection
// that is closed while it is still registered, before or during the
// open, is a stale route (so router.Do re-resolves), not a plain error.
func TestLegacyBrokerAdapter_ClosedChannelIsStale(t *testing.T) {
	t.Run("closed before open", func(t *testing.T) {
		f := newRelayFixture(t, nil)
		hc := installBrokerConnection(t, f.srv.controlChannel, "broker-1", "cc-1")
		ctx, cancel := context.WithCancel(context.Background())
		hc.ctx, hc.cancel = ctx, cancel
		res, err := f.srv.conduit.Load().router.Resolve(context.Background(), brokerRequest("broker-1"), nil)
		require.NoError(t, err)
		hc.Close()
		require.Same(t, hc, f.srv.controlChannel.GetConnection("broker-1"), "the closed connection stays registered")
		_, err = res.Legacy.(*legacyBrokerSession).OpenStream(context.Background(), wsprotocol.StreamTypePTY, "slug", "proj", 80, 24)
		require.ErrorIs(t, err, relay.ErrStaleRoute)
	})
	t.Run("closed during open", func(t *testing.T) {
		f := newRelayFixture(t, nil)
		_ = connectFakeBroker(t, f.srv, "broker-1")
		res, err := f.srv.conduit.Load().router.Resolve(context.Background(), brokerRequest("broker-1"), nil)
		require.NoError(t, err)
		ls := res.Legacy.(*legacyBrokerSession)
		ls.beforeOpen = func() { ls.conn.Close() }
		_, err = ls.OpenStream(context.Background(), wsprotocol.StreamTypePTY, "slug", "proj", 80, 24)
		require.ErrorIs(t, err, relay.ErrStaleRoute)
	})
}

// TestLegacyBrokerAdapter_OffOwnerNoSession: a node that does not hold
// the broker's control channel answers ErrNoSession (the caller keeps its
// pre-conduit behaviour), and so does a node without a control channel.
func TestLegacyBrokerAdapter_OffOwnerNoSession(t *testing.T) {
	f := newRelayFixture(t, nil)
	_ = connectFakeBroker(t, f.srv, "broker-other")
	rtr := f.srv.conduit.Load().router

	_, err := rtr.Resolve(context.Background(), brokerRequest("broker-1"), nil)
	require.ErrorIs(t, err, router.ErrNoSession)
	called := false
	err = rtr.Do(context.Background(), brokerRequest("broker-1"), func(context.Context, router.Resolved) error {
		called = true
		return nil
	})
	require.ErrorIs(t, err, router.ErrNoSession)
	assert.False(t, called)

	_, err = newLegacyBrokerResolver(nil, "hub-a").ResolveBroker(context.Background(), brokerRequest("broker-1"))
	require.ErrorIs(t, err, router.ErrNoSession, "no control channel on this node")
	_, err = newLegacyBrokerResolver(f.srv.controlChannel, "hub-a").ResolveBroker(context.Background(),
		router.Request{Op: router.OpStream, Kind: registry.PrincipalAgent, ID: "broker-other"})
	require.ErrorIs(t, err, router.ErrNoSession, "the adapter serves broker targets only")
}

// TestLegacyBrokerAdapter_ReplacedChannelIsStale: a legacy session is
// bound to the control channel it was resolved on. Once the broker
// reconnects, the old session's OpenStream is a stale route and router.Do
// re-resolves to the new channel.
func TestLegacyBrokerAdapter_ReplacedChannelIsStale(t *testing.T) {
	f := newRelayFixture(t, nil)
	cc := f.srv.controlChannel
	installBrokerConnection(t, cc, "broker-1", "cc-old")
	rtr := f.srv.conduit.Load().router

	var seen []string
	err := rtr.Do(context.Background(), brokerRequest("broker-1"), func(ctx context.Context, res router.Resolved) error {
		ls := res.Legacy.(*legacyBrokerSession)
		seen = append(seen, ls.SessionID())
		if len(seen) > 1 {
			return nil
		}
		installBrokerConnection(t, cc, "broker-1", "cc-new") // the broker reconnects
		_, err := ls.OpenStream(ctx, wsprotocol.StreamTypePTY, "slug", "proj", 80, 24)
		require.ErrorIs(t, err, relay.ErrStaleRoute)
		return err
	})
	require.NoError(t, err)
	assert.Equal(t, []string{"cc-old", "cc-new"}, seen)

	// A disconnected broker is a stale route too, and then no session.
	res, err := rtr.Resolve(context.Background(), brokerRequest("broker-1"), nil)
	require.NoError(t, err)
	cc.mu.Lock()
	delete(cc.connections, "broker-1")
	cc.mu.Unlock()
	_, err = res.Legacy.(*legacyBrokerSession).OpenStream(context.Background(), wsprotocol.StreamTypePTY, "slug", "proj", 80, 24)
	require.True(t, errors.Is(err, relay.ErrStaleRoute), "err = %v", err)
	_, err = rtr.Resolve(context.Background(), brokerRequest("broker-1"), nil)
	require.ErrorIs(t, err, router.ErrNoSession)
}

// TestBrokerPTYUnchanged_ExperimentOffAndOn: the existing broker PTY path
// (preflight states, then a full attach over the broker control channel)
// behaves identically with the conduit experiment off and with it on and
// the relay (and so the legacy broker adapter) running.
func TestBrokerPTYUnchanged_ExperimentOffAndOn(t *testing.T) {
	for _, tc := range []struct {
		name  string
		setup func(t *testing.T) *conduitFixture
	}{
		{name: "conduit off", setup: func(t *testing.T) *conduitFixture {
			f := newConduitFixture(t)
			setConduitExperiment(t, f.srv, false)
			require.Nil(t, f.srv.conduit.Load(), "no relay runs with the experiment off")
			return f
		}},
		{name: "conduit on", setup: func(t *testing.T) *conduitFixture {
			f := newRelayFixture(t, nil)
			require.NotNil(t, f.srv.conduit.Load())
			return f.conduitFixture
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := tc.setup(t)
			agent := f.agent
			require.Equal(t, "broker-1", agent.RuntimeBrokerID)
			pty := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				f.srv.handleAgentPTY(w, r.WithContext(contextWithIdentity(r.Context(), f.owner)))
			}))
			t.Cleanup(pty.Close)
			path := pty.URL + "/api/v1/agents/" + agent.ID + "/pty"

			// Preflight: broker not connected, then connected.
			resp, err := http.Get(path)
			require.NoError(t, err)
			_ = resp.Body.Close()
			assert.Equal(t, http.StatusServiceUnavailable, resp.StatusCode)
			b := connectFakeBroker(t, f.srv, "broker-1")
			resp, err = http.Get(path)
			require.NoError(t, err)
			_ = resp.Body.Close()
			assert.Equal(t, http.StatusOK, resp.StatusCode)

			// Attach: the hub opens a PTY stream on the broker's control
			// channel and relays both directions.
			client, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(path, "http")+"?cols=100&rows=30", nil)
			require.NoError(t, err)
			t.Cleanup(func() { _ = client.Close() })
			require.NoError(t, b.ws.SetReadDeadline(time.Now().Add(5*time.Second)))
			require.NoError(t, client.SetReadDeadline(time.Now().Add(5*time.Second)))

			var open wsprotocol.StreamOpenMessage
			require.NoError(t, b.ws.ReadJSON(&open))
			assert.Equal(t, wsprotocol.TypeStreamOpen, open.Type)
			assert.Equal(t, wsprotocol.StreamTypePTY, open.StreamType)
			assert.Equal(t, agent.Slug, open.Slug)
			assert.Equal(t, agent.ProjectID, open.ProjectID)
			assert.Equal(t, 100, open.Cols)
			assert.Equal(t, 30, open.Rows)

			require.NoError(t, b.ws.WriteJSON(wsprotocol.NewStreamFrame(open.StreamID, []byte("from broker"))))
			var out wsprotocol.PTYDataMessage
			require.NoError(t, client.ReadJSON(&out))
			assert.Equal(t, wsprotocol.TypeData, out.Type)
			assert.Equal(t, []byte("from broker"), out.Data)

			require.NoError(t, client.WriteJSON(wsprotocol.NewPTYDataMessage([]byte("from client"))))
			var in wsprotocol.StreamFrame
			require.NoError(t, b.ws.ReadJSON(&in))
			assert.Equal(t, open.StreamID, in.StreamID)
			assert.Equal(t, []byte("from client"), in.Data)

			require.NoError(t, client.WriteControl(websocket.CloseMessage,
				websocket.FormatCloseMessage(websocket.CloseNormalClosure, "detach"), time.Now().Add(time.Second)))
			var closed wsprotocol.StreamCloseMessage
			require.NoError(t, b.ws.ReadJSON(&closed))
			assert.Equal(t, wsprotocol.TypeStreamClose, closed.Type)
			assert.Equal(t, open.StreamID, closed.StreamID)
			assert.True(t, f.srv.controlChannel.IsConnected("broker-1"), "ending an attach keeps the broker connected")
		})
	}
}
