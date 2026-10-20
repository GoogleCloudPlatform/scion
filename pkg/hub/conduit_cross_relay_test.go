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
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/conduit"
	"github.com/GoogleCloudPlatform/scion/pkg/conduit/clock"
	"github.com/GoogleCloudPlatform/scion/pkg/conduit/grant"
	"github.com/GoogleCloudPlatform/scion/pkg/conduit/relay"
	"github.com/GoogleCloudPlatform/scion/pkg/conduit/relayrole/relayserver"
	"github.com/GoogleCloudPlatform/scion/pkg/conduit/transport/ws"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/GoogleCloudPlatform/scion/pkg/store/entadapter"
	conduitv1 "github.com/GoogleCloudPlatform/scion/proto/conduit/v1"
)

// crossRelayWorld is the separate relay world (hubs A and B in external
// relay mode, relay-0 holding the agent's session and admitting through
// hub A) plus a second separate relay, relay-1, admitting through hub B.
// Each relay has its own configuration (with the one shared relay-peer
// secret) and its own registry store handle on the shared database.
type crossRelayWorld struct {
	*separateRelayWorld
	relay1     *relayserver.Server
	relay1Addr string
	spawned    chan *fakePTY
}

func newCrossRelayWorld(t *testing.T) *crossRelayWorld {
	t.Helper()
	return newCrossRelayWorldWith(t, separateRelayOptions{})
}

// newCrossRelayWorldWith is newCrossRelayWorld with opts.
func newCrossRelayWorldWith(t *testing.T, opts separateRelayOptions) *crossRelayWorld {
	t.Helper()
	cw := &crossRelayWorld{}
	cw.separateRelayWorld, cw.spawned = newPTYSeparateRelayWorldWith(t, opts)
	internalB := httptest.NewServer(cw.hubB.ConduitInternalHandler())
	t.Cleanup(internalB.Close)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	cw.relay1Addr = ln.Addr().String()
	cw.relay1, err = relayserver.Start(context.Background(), relayserver.Config{
		InstanceID: "relay-1", PublicURL: "http://" + cw.relay1Addr, HubInternalURL: internalB.URL,
		PublicListener: ln, InternalListen: "127.0.0.1:0",
		PeerAuth: externalRelayAuth(t, "relay-1"), Registry: entadapter.NewConduitRegistryStore(cw.regClient), Experiment: conduitAlwaysOn{},
	})
	require.NoError(t, err)
	t.Cleanup(func() {
		sctx, scancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer scancel()
		_ = cw.relay1.Shutdown(sctx, true, time.Second)
	})
	return cw
}

// relayUser is a user's conduit session on a separate relay.
type relayUser struct {
	ls      conduit.LocalSession
	streams chan conduit.Stream
}

// dialUserAt opens the owner's user session on the relay at addr with
// bearer, as the CLI does.
func (cw *crossRelayWorld) dialUserAt(t *testing.T, addr, bearer string) *relayUser {
	t.Helper()
	u := &relayUser{streams: make(chan conduit.Stream, 4)}
	cfg := conduit.Config{
		Clock: clock.Real(),
		StreamHandler: conduit.StreamHandlerFunc(func(_ context.Context, _ *conduitv1.StreamOpen, ps conduit.PendingStream) error {
			st, err := ps.Accept()
			if err != nil {
				return nil
			}
			u.streams <- st
			return nil
		}),
	}
	d := &ws.Dialer{
		URL: "ws://" + addr + "/api/v1/conduit",
		Header: func(context.Context) (http.Header, error) {
			return http.Header{"Authorization": {"Bearer " + bearer}}, nil
		},
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	s, _, err := conduit.Dial(ctx, d, cfg, &conduitv1.Hello{PrincipalKind: conduitv1.PrincipalKind_PRINCIPAL_KIND_USER, PrincipalId: cw.launched.OwnerID})
	require.NoError(t, err)
	u.ls = s.(conduit.LocalSession)
	t.Cleanup(func() { _ = u.ls.Close() })
	return u
}

// ownerTokenA is a user token for the agent's owner issued by hub A.
func (cw *crossRelayWorld) ownerTokenA(t *testing.T) string {
	t.Helper()
	tok, _, _, err := cw.srv.userTokenService.GenerateTokenPair(
		cw.launched.OwnerID, "owner@conduit.test", "Owner", store.UserRoleMember, ClientTypeWeb)
	require.NoError(t, err)
	return tok
}

// openTCPTunnel asks for a tcp tunnel to the agent's app and returns the
// user stream.
func (u *relayUser) openTCPTunnel(t *testing.T, agentID string, port int) (conduitTunnelResponse, conduit.Stream) {
	t.Helper()
	body, err := json.Marshal(conduitTunnelRequest{Agent: agentID, Kind: grant.StreamKindTCP, Params: map[string]string{grant.ParamPort: strconv.Itoa(port)}})
	require.NoError(t, err)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	resp, err := u.ls.Call(ctx, &conduitv1.RpcRequest{Method: http.MethodPost, Path: conduitTunnelsPath, Body: body})
	require.NoError(t, err)
	require.Equal(t, int32(http.StatusOK), resp.GetStatus(), string(resp.GetBody()))
	var ok conduitTunnelResponse
	require.NoError(t, json.Unmarshal(resp.GetBody(), &ok))
	require.NotZero(t, ok.StreamID, "the relay set the user stream id")
	select {
	case st := <-u.streams:
		require.Equal(t, ok.StreamID, st.ID())
		return ok, st
	case <-time.After(10 * time.Second):
		t.Fatal("no user stream was opened toward the session")
	}
	return ok, nil
}

// httpThrough sends one HTTP/1.0 request over st and returns the reply.
func httpThrough(t *testing.T, st conduit.Stream, path string) string {
	t.Helper()
	_, err := io.WriteString(st, "GET "+path+" HTTP/1.0\r\nHost: app\r\n\r\n")
	require.NoError(t, err)
	done := make(chan []byte, 1)
	go func() {
		b, _ := io.ReadAll(st)
		done <- b
	}()
	select {
	case b := <-done:
		return string(b)
	case <-time.After(10 * time.Second):
		t.Fatal("no reply through the tunnel")
	}
	return ""
}

// TestCrossRelay_TunnelBridgedByUserRelay (design §3.9 Delivery, criterion
// 15): a user session held by relay-1 asks for a tunnel to an agent whose
// session relay-0 holds; the request enters through hub B. Hub B authorizes
// and mints, relay-1 opens exactly one bridge to relay-0 and splices it to
// the user stream, the bridge carries relay-0's ids for the stream on the
// agent's own session, and no hub holds any part of the tunnel.
func TestCrossRelay_TunnelBridgedByUserRelay(t *testing.T) {
	cw := newCrossRelayWorld(t)
	sessions := cw.agentSessions(t)
	require.Len(t, sessions, 1)
	require.Equal(t, "relay-0", sessions[0].RelayInstanceID)

	u := cw.dialUserAt(t, cw.relay1Addr, cw.userTokenB)
	resp, st := u.openTCPTunnel(t, cw.launched.ID, cw.app.port)
	assert.Equal(t, u.ls.Info().SessionID, resp.SessionID)

	tunnels := cw.relay1.Role.Tunnels()
	require.Len(t, tunnels, 1)
	tn := tunnels[0]
	require.NotNil(t, tn.Bridge, "the user's relay bridges to the owner relay")
	assert.Equal(t, "relay-0", tn.Bridge.Owner().InstanceID)
	want := relay.TargetStream{SessionID: sessions[0].SessionID, StreamID: cw.lastAgentStream(t)}
	assert.Equal(t, want, tn.Target, "the stream on the agent's own session")
	assert.Equal(t, want, tn.Bridge.Target())
	assert.NotEmpty(t, tn.OpenJTI, "the hub's open grant is kept")
	assert.Equal(t, int64(1), cw.relay.Relay.ActiveBridges(), "exactly one bridge reaches the owner relay")
	assert.Empty(t, trackedLeaves(t, cw.hubB), "hub B holds no leaf of the tunnel")
	assert.Empty(t, trackedLeaves(t, cw.srv), "hub A holds no leaf of the tunnel")

	assert.Contains(t, httpThrough(t, st, "/via-relay-1"), "hello /via-relay-1")
}

// TestCrossRelay_TunnelSameRelayNoBridge: when the user's session and the
// agent's are on the same relay, the relay opens the target stream itself,
// with no bridge.
func TestCrossRelay_TunnelSameRelayNoBridge(t *testing.T) {
	cw := newCrossRelayWorld(t)
	sessions := cw.agentSessions(t)
	require.Len(t, sessions, 1)

	u := cw.dialUserAt(t, cw.relay.PublicAddr.String(), cw.ownerTokenA(t))
	_, st := u.openTCPTunnel(t, cw.launched.ID, cw.app.port)

	tunnels := cw.relay.Role.Tunnels()
	require.Len(t, tunnels, 1)
	assert.Nil(t, tunnels[0].Bridge, "no bridge on the same relay")
	assert.Equal(t, relay.TargetStream{SessionID: sessions[0].SessionID, StreamID: cw.lastAgentStream(t)}, tunnels[0].Target)
	assert.Equal(t, int64(0), cw.relay.Relay.ActiveBridges())
	assert.Empty(t, cw.relay1.Role.Tunnels())

	assert.Contains(t, httpThrough(t, st, "/same-relay"), "hello /same-relay")
}
