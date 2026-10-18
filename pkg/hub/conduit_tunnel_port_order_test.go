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
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/conduit/grant"
	"github.com/GoogleCloudPlatform/scion/pkg/conduit/relay"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	conduitv1 "github.com/GoogleCloudPlatform/scion/proto/conduit/v1"
)

// relayTunnelTCP sends a tcp tunnel request to agentID on port, forwarded
// from the relay-held user session s1 by a relay that opens tunnels, and
// returns the hub's answer.
func relayTunnelTCP(t *testing.T, f *relayOpsFixture, token, agentID, port string) *conduitv1.RpcResponse {
	t.Helper()
	body, err := json.Marshal(conduitTunnelRequest{Agent: agentID, Kind: grant.StreamKindTCP, Params: tcpParams(port)})
	require.NoError(t, err)
	b, err := proto.Marshal(&conduitv1.RpcRequest{RequestId: "r1", Method: http.MethodPost, Path: conduitTunnelsPath, Body: body})
	require.NoError(t, err)
	var out relay.HubRPCResponse
	require.NoError(t, f.hub.Call(context.Background(), relay.HubSessionPath("s1", "rpc"),
		relay.HubRPCRequest{AdmissionToken: token, RPC: b, Features: []string{relay.HubFeatureTunnels}}, &out))
	resp := &conduitv1.RpcResponse{}
	require.NoError(t, proto.Unmarshal(out.RPC, resp))
	return resp
}

// setTunnelTarget sets the agent's phase and exposed tcp ports.
func setTunnelTarget(t *testing.T, st store.Store, agentID string, phase state.Phase, ports ...int) {
	t.Helper()
	ctx := context.Background()
	a, err := st.GetAgent(ctx, agentID)
	require.NoError(t, err)
	a.Phase = string(phase)
	require.NoError(t, st.UpdateAgent(ctx, a))
	var eps []store.ExposedPort
	for _, p := range ports {
		eps = append(eps, store.ExposedPort{Port: p, Host: "127.0.0.1", Mode: "rw"})
	}
	require.NoError(t, st.UpdateAgentExposedPorts(ctx, agentID, eps))
}

// TestRelayTunnel_PortCheckedAfterAgentState (design §3.8 rule 7): on a
// relay-held session, the tcp target port is checked after the agent's
// state, as on a hub-held one. A stopped agent with no exposed port is not
// running (409 agent_not_running); the same agent running is refused the
// port (403 forbidden). Neither gets an instruction to open a tunnel.
func TestRelayTunnel_PortCheckedAfterAgentState(t *testing.T) {
	f, cf := newUserRelayOps(t)
	adm := f.mustAdmit(t, bearer(ownerJWT(t, cf)), "s1")

	setTunnelTarget(t, cf.store, cf.agent.ID, state.PhaseStopped)
	resp := relayTunnelTCP(t, f, adm.AdmissionToken, cf.agent.ID, "3000")
	assert.Equal(t, int32(http.StatusConflict), resp.GetStatus(), string(resp.GetBody()))
	assert.Contains(t, string(resp.GetBody()), ErrCodeAgentNotRunning)

	setTunnelTarget(t, cf.store, cf.agent.ID, state.PhaseRunning)
	resp = relayTunnelTCP(t, f, adm.AdmissionToken, cf.agent.ID, "3000")
	assert.Equal(t, int32(http.StatusForbidden), resp.GetStatus(), string(resp.GetBody()))
	assert.Contains(t, string(resp.GetBody()), ErrCodeForbidden)
}

// TestConduitTunnel_PortCheckAppliesOnBothPaths: authorizeConduitTunnel
// leaves the tcp target port to its caller (DeferTCPTarget), and
// checkTunnelTarget checks it for both tunnel paths. On a hub-held session
// and on a relay-held session, a running agent's unexposed port is refused
// 403 forbidden. Once the port is exposed the same request passes the
// port check: it opens on the hub-held path, and on the relay-held path it
// goes on to the next step (no agent session, 503).
func TestConduitTunnel_PortCheckAppliesOnBothPaths(t *testing.T) {
	t.Run("hub-held session", func(t *testing.T) {
		tf := newTunnelFixture(t)
		tf.startAgent(t, tf.public.URL, tf.launched)
		c := tf.ownerClient(t)

		require.Empty(t, tf.exposePort(t, tf.launched).ExposedPorts)
		r := c.post(t, tf.launched.ID, grant.StreamKindTCP, tcpParams("3000"))
		assert.Equal(t, http.StatusForbidden, r.status, r.body)
		assert.Equal(t, ErrCodeForbidden, r.err.Code, r.body)

		tf.exposePort(t, tf.launched, 3000)
		r = c.post(t, tf.launched.ID, grant.StreamKindTCP, tcpParams("3000"))
		assert.Equal(t, http.StatusOK, r.status, r.body)
	})
	t.Run("relay-held session", func(t *testing.T) {
		f, cf := newUserRelayOps(t)
		adm := f.mustAdmit(t, bearer(ownerJWT(t, cf)), "s1")

		setTunnelTarget(t, cf.store, cf.agent.ID, state.PhaseRunning)
		resp := relayTunnelTCP(t, f, adm.AdmissionToken, cf.agent.ID, "3000")
		assert.Equal(t, int32(http.StatusForbidden), resp.GetStatus(), string(resp.GetBody()))
		assert.Contains(t, string(resp.GetBody()), ErrCodeForbidden)

		setTunnelTarget(t, cf.store, cf.agent.ID, state.PhaseRunning, 3000)
		resp = relayTunnelTCP(t, f, adm.AdmissionToken, cf.agent.ID, "3000")
		assert.Equal(t, int32(http.StatusServiceUnavailable), resp.GetStatus(), "past the port check, the next step answers: %s", resp.GetBody())
		assert.NotContains(t, string(resp.GetBody()), ErrCodeForbidden)
	})
}
