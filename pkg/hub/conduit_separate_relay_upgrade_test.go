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
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/conduit"
	"github.com/GoogleCloudPlatform/scion/pkg/conduit/grant"
	"github.com/GoogleCloudPlatform/scion/pkg/wsprotocol"
	conduitv1 "github.com/GoogleCloudPlatform/scion/proto/conduit/v1"
)

// End to end, with separate relays and a real sciontool whose Hello
// advertises no stream renewal (design §3.5 Q14, v2.21.43; contracts §2
// "Target lacks stream renewal"): with enforce, nothing is opened or
// spliced toward the agent on any path.

// upgradeWorld is the enforce option set with an agent without renewal.
var upgradeWorld = separateRelayOptions{enforce: true, agentWithoutRenewal: true}

// requireAgentWithoutRenewal checks that the agent's registry row lacks
// stream_renewal.v2, so the world tests what it claims to.
func requireAgentWithoutRenewal(t *testing.T, w *separateRelayWorld) {
	t.Helper()
	sessions := w.agentSessions(t)
	require.Len(t, sessions, 1)
	require.False(t, sessions[0].Capabilities.HasFeature(conduit.FeatureStreamRenewal), "the agent advertises no stream renewal")
}

// agentStreamsOpened returns how many streams reached the agent's session.
func (w *separateRelayWorld) agentStreamsOpened() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return len(w.agentOpened)
}

// tunnelCall asks for a tcp tunnel to the agent's app and returns the RPC
// response, whatever its status.
func (u *relayUser) tunnelCall(t *testing.T, agentID string, port int) *conduitv1.RpcResponse {
	t.Helper()
	body, err := json.Marshal(conduitTunnelRequest{Agent: agentID, Kind: grant.StreamKindTCP, Params: map[string]string{grant.ParamPort: strconv.Itoa(port)}})
	require.NoError(t, err)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	resp, err := u.ls.Call(ctx, &conduitv1.RpcRequest{Method: http.MethodPost, Path: conduitTunnelsPath, Body: body})
	require.NoError(t, err)
	return resp
}

// TestSeparateRelay_UpgradeRequired_RelayTunnel (E1): a user session on
// relay-1 asks, through hub B, for a tunnel to the agent on relay-0. It
// gets 409 agent_upgrade_required; no relay holds a tunnel, no bridge is
// opened, no stream reaches the agent and no hub holds a leaf.
func TestSeparateRelay_UpgradeRequired_RelayTunnel(t *testing.T) {
	cw := newCrossRelayWorldWith(t, upgradeWorld)
	requireAgentWithoutRenewal(t, cw.separateRelayWorld)

	u := cw.dialUserAt(t, cw.relay1Addr, cw.userTokenB)
	resp := u.tunnelCall(t, cw.launched.ID, cw.app.port)
	require.Equal(t, int32(http.StatusConflict), resp.GetStatus(), string(resp.GetBody()))
	requireUpgradeRequiredBody(t, resp.GetBody())

	assert.Empty(t, cw.relay1.Role.Tunnels(), "the user's relay holds no tunnel")
	assert.Empty(t, cw.relay.Role.Tunnels(), "the agent's relay holds no tunnel")
	assert.Equal(t, int64(0), cw.relay.Relay.ActiveBridges(), "no bridge reached the agent's relay")
	assert.Zero(t, cw.agentStreamsOpened(), "no stream reached the agent")
	assert.Empty(t, trackedLeaves(t, cw.hubB))
	assert.Empty(t, trackedLeaves(t, cw.srv))
	select {
	case <-u.streams:
		t.Fatal("a user stream was opened")
	default:
	}
}

// TestSeparateRelay_UpgradeRequired_RelayTunnelOff (E1 control): with off
// the same tunnel opens and carries traffic.
func TestSeparateRelay_UpgradeRequired_RelayTunnelOff(t *testing.T) {
	cw := newCrossRelayWorldWith(t, separateRelayOptions{agentWithoutRenewal: true})
	requireAgentWithoutRenewal(t, cw.separateRelayWorld)

	u := cw.dialUserAt(t, cw.relay1Addr, cw.userTokenB)
	_, st := u.openTCPTunnel(t, cw.launched.ID, cw.app.port)
	assert.Contains(t, httpThrough(t, st, "/off"), "hello /off")
}

// TestSeparateRelay_UpgradeRequired_Proxy (E2): the port proxy on hub B
// answers 409 agent_upgrade_required, with no stream to the agent and no
// leaf; it does not fall back to the port-forward tunnel.
func TestSeparateRelay_UpgradeRequired_Proxy(t *testing.T) {
	w := newSeparateRelayWorldWith(t, upgradeWorld)
	requireAgentWithoutRenewal(t, w)

	resp := w.getB(t, "/hello")
	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	require.Equal(t, http.StatusConflict, resp.StatusCode, string(body))
	requireUpgradeRequiredBody(t, body)
	assert.Zero(t, w.agentStreamsOpened(), "no stream reached the agent")
	assert.Empty(t, w.trackedOnB(t))
	assert.False(t, w.hubB.portTunnels.has(w.launched.ID), "no port-forward tunnel is involved")
}

// TestSeparateRelay_UpgradeRequired_ProxyOff (E2 control): with off the
// proxy serves the agent's app.
func TestSeparateRelay_UpgradeRequired_ProxyOff(t *testing.T) {
	w := newSeparateRelayWorldWith(t, separateRelayOptions{agentWithoutRenewal: true})
	requireAgentWithoutRenewal(t, w)

	resp := w.getB(t, "/hello")
	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, resp.StatusCode, string(body))
	assert.Equal(t, "hello /hello", string(body))
}

// TestSeparateRelay_UpgradeRequired_PTY (E3): the PTY preflight and the
// WebSocket open on hub B get 503 runtime_attach_unsupported, reason
// stream_renewal_unsupported, with no upgrade and nothing spawned.
func TestSeparateRelay_UpgradeRequired_PTY(t *testing.T) {
	w, spawned := newPTYSeparateRelayWorldWith(t, upgradeWorld)
	requireAgentWithoutRenewal(t, w)
	url := w.publicB.URL + "/api/v1/agents/" + w.launched.ID + "/pty"
	header := http.Header{"Authorization": {"Bearer " + w.userTokenB}}

	req, err := http.NewRequest(http.MethodGet, url, nil)
	require.NoError(t, err)
	req.Header = header.Clone()
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	body, err := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	require.NoError(t, err)
	require.Equal(t, http.StatusServiceUnavailable, resp.StatusCode, string(body))
	var er ErrorResponse
	require.NoError(t, json.Unmarshal(body, &er), string(body))
	assert.Equal(t, wsprotocol.ErrCodeRuntimeAttachUnsupported, er.Error.Code)
	assert.Equal(t, conduitReasonStreamRenewalUnsupported, er.Error.Details["reason"])

	c, wsResp, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(url, "http"), header)
	if c != nil {
		_ = c.Close()
	}
	require.ErrorIs(t, err, websocket.ErrBadHandshake, "no WebSocket is upgraded")
	require.NotNil(t, wsResp)
	assert.Equal(t, http.StatusServiceUnavailable, wsResp.StatusCode)
	_ = wsResp.Body.Close()

	assert.Zero(t, w.agentStreamsOpened(), "no stream reached the agent")
	assert.Empty(t, trackedLeaves(t, w.hubB))
	select {
	case <-spawned:
		t.Fatal("a terminal was spawned")
	default:
	}
}
