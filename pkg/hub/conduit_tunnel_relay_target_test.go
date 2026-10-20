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
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"

	"github.com/GoogleCloudPlatform/scion/pkg/conduit"
	"github.com/GoogleCloudPlatform/scion/pkg/conduit/grant"
	"github.com/GoogleCloudPlatform/scion/pkg/conduit/registry"
	"github.com/GoogleCloudPlatform/scion/pkg/conduit/relay"
	conduitv1 "github.com/GoogleCloudPlatform/scion/proto/conduit/v1"
)

// postExcluding forwards a tcp tunnel request for the agent as relay-0,
// ruling out the target sessions in exclude (the relay's retry after a
// target it found stale).
func (f *relayTunnelFixture) postExcluding(t *testing.T, exclude ...string) (*conduitv1.RpcResponse, *relay.HubTunnelOpen) {
	t.Helper()
	body, err := json.Marshal(conduitTunnelRequest{Agent: f.cf.agent.ID, Kind: grant.StreamKindTCP, Params: map[string]string{grant.ParamPort: "3000"}})
	require.NoError(t, err)
	b, err := proto.Marshal(&conduitv1.RpcRequest{RequestId: "r-excl", Method: http.MethodPost, Path: conduitTunnelsPath, Body: body})
	require.NoError(t, err)
	var out relay.HubRPCResponse
	require.NoError(t, f.ops.hub.Call(context.Background(), relay.HubSessionPath("s1", "rpc"),
		relay.HubRPCRequest{AdmissionToken: f.token, RPC: b, Features: []string{relay.HubFeatureTunnels}, ExcludeTargets: exclude}, &out))
	resp := &conduitv1.RpcResponse{}
	require.NoError(t, proto.Unmarshal(out.RPC, resp))
	return resp, out.Tunnel
}

// verifyRelayTunnelGrant verifies tok with the hub's published grant keys
// against target, the row the test expects the grant to bind.
func verifyRelayTunnelGrant(t *testing.T, f *relayTunnelFixture, tok []byte, target grant.Target) (*grant.Claims, error) {
	t.Helper()
	pubs, err := f.cf.srv.ConduitGrantPublicKeys(context.Background())
	require.NoError(t, err)
	keys, err := grant.NewKeySet(pubs...)
	require.NoError(t, err)
	return grant.Verify(context.Background(), tok, keys, grant.Expectation{
		Target:    target,
		Header:    grant.StreamHeader{Kind: grant.StreamKindTCP, Params: map[string]string{grant.ParamPort: "3000"}},
		ProjectID: f.cf.agent.ProjectID, Issuer: conduitGrantIssuer,
	}, grant.NewMemoryReplayCache(f.cf.clock.Now, 0), f.cf.clock.Now())
}

// TestRelayTunnel_GrantBindsResolvedRow (design §3.5 Check point): on the
// relay path the open grant binds exactly the row the router resolved: its
// session, connection epoch and the incarnation the resolution wanted; the
// instruction names the same session and its owner relay. On the relay's
// retry (the first target stale, ruled out), after the agent was launched
// again and reconnected through another relay, the hub resolves again and
// re-mints for the new row: the new grant binds the new session, epoch and
// incarnation, and no longer verifies for the first row.
func TestRelayTunnel_GrantBindsResolvedRow(t *testing.T) {
	f := newRelayTunnelFixture(t, &openSetup{})
	ctx := context.Background()
	rowA := grant.Target{
		Kind: grant.TargetKindAgent, ID: f.cf.agent.ID, EndpointIncarnation: f.cf.agent.RunID,
		SessionID: "s-agent", ConnectionEpoch: f.epoch,
	}

	resp, open := f.post(t)
	require.Equal(t, int32(http.StatusOK), resp.GetStatus(), string(resp.GetBody()))
	require.NotNil(t, open)
	assert.Equal(t, "s-agent", open.TargetSessionID)
	assert.Equal(t, "relay-0", open.OwnerInstanceID)
	assert.Equal(t, f.ops.relayGen, open.OwnerGeneration)
	assert.Equal(t, f.cf.agent.RunID, open.WantIncarnation)
	assert.Equal(t, f.cf.agent.ProjectID, open.WantProjectID)
	c, err := verifyRelayTunnelGrant(t, f, open.Grant, rowA)
	require.NoError(t, err, "the grant binds row A")
	assert.Equal(t, rowA, c.Target)
	assert.Equal(t, c.JTI, open.OpenJTI)

	// The agent is launched again and its new session is held by relay-1,
	// with the next connection epoch.
	_, err = f.cf.store.SetAgentRunID(ctx, f.cf.agent.ID, "launch-2", nil)
	require.NoError(t, err)
	gen1, err := f.ops.reg.RegisterRelay(ctx, registry.RelayInstance{InstanceID: "relay-1", InternalEndpoint: "http://relay-1.invalid"})
	require.NoError(t, err)
	now := time.Now()
	epochB, err := f.ops.reg.InsertSessionWithNextEpoch(ctx, registry.SessionRecord{
		SessionID: "s-agent-b", PrincipalKind: registry.PrincipalAgent, PrincipalID: f.cf.agent.ID, ProjectID: f.cf.agent.ProjectID,
		RelayInstanceID: "relay-1", RelayGeneration: gen1, Transport: registry.TransportWS,
		EndpointIncarnation: "launch-2", ConnectedAt: now, LastSeen: now,
		Capabilities: registry.Capabilities{
			StreamKinds: []string{grant.StreamKindTCP}, EndpointIncarnation: "launch-2", IncarnationSource: "launch_id",
			Features: []string{conduit.FeatureStreamRenewal},
		},
	})
	require.NoError(t, err)
	require.NotEqual(t, f.epoch, epochB)
	rowB := grant.Target{
		Kind: grant.TargetKindAgent, ID: f.cf.agent.ID, EndpointIncarnation: "launch-2",
		SessionID: "s-agent-b", ConnectionEpoch: epochB,
	}

	resp, open = f.postExcluding(t, "s-agent")
	require.Equal(t, int32(http.StatusOK), resp.GetStatus(), string(resp.GetBody()))
	require.NotNil(t, open)
	assert.Equal(t, "s-agent-b", open.TargetSessionID)
	assert.Equal(t, "relay-1", open.OwnerInstanceID)
	assert.Equal(t, gen1, open.OwnerGeneration)
	assert.Equal(t, "http://relay-1.invalid", open.OwnerEndpoint)
	assert.Equal(t, "launch-2", open.WantIncarnation)
	c, err = verifyRelayTunnelGrant(t, f, open.Grant, rowB)
	require.NoError(t, err, "the re-minted grant binds row B")
	assert.Equal(t, rowB, c.Target)
	assert.Equal(t, c.JTI, open.OpenJTI)
	_, err = verifyRelayTunnelGrant(t, f, open.Grant, rowA)
	assert.ErrorIs(t, err, grant.ErrTarget, "the re-minted grant does not verify for row A")
}
