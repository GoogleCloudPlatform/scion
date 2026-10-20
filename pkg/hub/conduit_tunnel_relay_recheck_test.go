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
	"log/slog"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/conduit/grant"
	"github.com/GoogleCloudPlatform/scion/pkg/conduit/relay"
	conduitv1 "github.com/GoogleCloudPlatform/scion/proto/conduit/v1"
)

// Relay feature tunnel_recheck (design v2.21.46 §3.9; contracts dc2f4f22
// §3): the hub stamps a relay-held tunnel's grant for renewal only when the
// forwarding relay lists the feature on its relay-peer-authenticated
// request.

// recheckFeatures are the features of a relay that re-checks every tunnel
// it opens.
var recheckFeatures = []string{relay.HubFeatureTunnels, relay.HubFeatureTunnelRecheck}

// relayTunnelClaims verifies the open grant of the instruction open and
// returns its claims.
func relayTunnelClaims(t *testing.T, f *relayTunnelFixture, open *relay.HubTunnelOpen) *grant.Claims {
	t.Helper()
	pubs, err := f.cf.srv.ConduitGrantPublicKeys(context.Background())
	require.NoError(t, err)
	keys, err := grant.NewKeySet(pubs...)
	require.NoError(t, err)
	c, err := grant.Verify(context.Background(), open.Grant, keys, grant.Expectation{
		Target: grant.Target{
			Kind: grant.TargetKindAgent, ID: f.cf.agent.ID, EndpointIncarnation: open.WantIncarnation,
			SessionID: open.TargetSessionID, ConnectionEpoch: f.epoch,
		},
		Header:    grant.StreamHeader{Kind: open.Kind, Params: open.Params},
		ProjectID: f.cf.agent.ProjectID, Issuer: conduitGrantIssuer,
	}, grant.NewMemoryReplayCache(f.cf.clock.Now, 0), f.cf.clock.Now())
	require.NoError(t, err)
	return c
}

// requireUnstampedRelayTunnel checks a served tunnel whose grant carries no
// via, scope or authz_until, and whose instruction carries no hints.
func requireUnstampedRelayTunnel(t *testing.T, f *relayTunnelFixture, resp *conduitv1.RpcResponse, open *relay.HubTunnelOpen) {
	t.Helper()
	require.Equal(t, int32(http.StatusOK), resp.GetStatus(), string(resp.GetBody()))
	require.NotNil(t, open, "the tunnel is served")
	c := relayTunnelClaims(t, f, open)
	assert.Empty(t, c.Via)
	assert.Nil(t, c.Scope)
	assert.True(t, c.AuthzUntil.IsZero())
	assert.Zero(t, c.AuthzInterval)
	assert.Zero(t, open.AuthzUntil)
	assert.Zero(t, open.AuthzIntervalS)
}

// relayGateUnstampedMessage is the log message of the relay gate's off
// branch (mintRelayTunnel).
const relayGateUnstampedMessage = "Conduit relay tunnel served unstamped: the forwarding relay does not re-check tunnels"

// captureRelayGateLogs sends slog's default logger to a buffer until the
// test ends, and returns a function listing the relay gate's off-branch
// lines written so far.
func captureRelayGateLogs(t *testing.T) func() []string {
	t.Helper()
	logs := &syncBuffer{}
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(logs, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return func() []string {
		var out []string
		for _, line := range strings.Split(logs.String(), "\n") {
			if strings.Contains(line, relayGateUnstampedMessage) {
				out = append(out, line)
			}
		}
		return out
	}
}

// TestRelayTunnel_RecheckListedStampedWithHints: with enforce, a relay that
// lists tunnel_recheck gets a tunnel to a stream_renewal.v2 target with a
// grant stamped for renewal (via=tunnel, authz_until) and the scheduling
// hints copied from it.
func TestRelayTunnel_RecheckListedStampedWithHints(t *testing.T) {
	o := &openSetup{enforce: true}
	f := newRelayTunnelFixture(t, o)
	gateLines := captureRelayGateLogs(t)

	o.keys.arm(f.cf.srv)
	resp, open := f.postWith(t, recheckFeatures, nil)
	require.Equal(t, int32(http.StatusOK), resp.GetStatus(), string(resp.GetBody()))
	require.NotNil(t, open)
	c := relayTunnelClaims(t, f, open)
	assert.Equal(t, grant.ViaTunnel, c.Via)
	require.False(t, c.AuthzUntil.IsZero())
	assert.Equal(t, c.AuthzUntil.Unix(), open.AuthzUntil)
	assert.Equal(t, int64(c.AuthzInterval/time.Second), open.AuthzIntervalS)
	assert.NotZero(t, open.AuthzIntervalS)
	assert.Empty(t, o.rec.list(), "nothing refused")
	assert.Empty(t, o.rec.unstampedList())
	assert.Empty(t, gateLines())
}

// TestRelayTunnel_NonV2TargetNotGatedByRecheck: a target without
// stream_renewal.v2 is not gated by tunnel_recheck. With enforce, a relay
// that does not list the feature gets 409 agent_upgrade_required, reason
// stream_renewal_unsupported, never the 503 stream_renewal_unavailable:
// nothing is signed, the relay gets no instruction, and only the upgrade
// refusal is counted. With off the tunnel is served with an unstamped
// grant and is not counted as an unstamped open. Each case has its own
// subtest, so each fixture gets its own registry store (the test store is
// named after the test).
func TestRelayTunnel_NonV2TargetNotGatedByRecheck(t *testing.T) {
	t.Run("enforce", func(t *testing.T) {
		o := &openSetup{enforce: true, targetNoRenewal: true}
		f := newRelayTunnelFixture(t, o)
		gateLines := captureRelayGateLogs(t)

		o.keys.arm(f.cf.srv)
		resp, open := f.post(t)
		require.Equal(t, int32(http.StatusConflict), resp.GetStatus(), string(resp.GetBody()))
		requireUpgradeRequiredBody(t, resp.GetBody())
		assert.Nil(t, open, "no instruction to open the tunnel")
		assert.Zero(t, o.keys.reads.Load(), "nothing was signed")
		assert.Equal(t, []string{upgradeRefusal(conduitRouteRelayTunnel)}, o.rec.list(), "the upgrade refusal, not stream_renewal_unavailable")
		assert.Empty(t, o.rec.unstampedList())
		assert.Empty(t, gateLines(), "the relay gate is not reached")
	})
	t.Run("off", func(t *testing.T) {
		o := &openSetup{targetNoRenewal: true}
		f := newRelayTunnelFixture(t, o)
		gateLines := captureRelayGateLogs(t)

		o.keys.arm(f.cf.srv)
		resp, open := f.post(t)
		requireUnstampedRelayTunnel(t, f, resp, open)
		assert.Empty(t, o.rec.list())
		assert.Empty(t, o.rec.unstampedList(), "a target without stream_renewal.v2 is never counted")
		assert.Empty(t, gateLines(), "nor logged by the relay gate")
	})
}

// TestRelayTunnel_RecheckUnlistedOffUnstamped: with off, a relay that does
// not list tunnel_recheck gets a tunnel to a stream_renewal.v2 target
// served as before, with an unstamped grant and no hints; nothing is
// refused, and the open is logged once and counted as unstamped, both with
// route relay_tunnel and cause renewal_route_unavailable.
func TestRelayTunnel_RecheckUnlistedOffUnstamped(t *testing.T) {
	o := &openSetup{}
	f := newRelayTunnelFixture(t, o)
	gateLines := captureRelayGateLogs(t)

	o.keys.arm(f.cf.srv)
	resp, open := f.post(t)
	requireUnstampedRelayTunnel(t, f, resp, open)
	assert.Empty(t, o.rec.list(), "nothing refused")
	assert.Equal(t, []string{conduitRouteRelayTunnel + "/" + string(PrincipalKindUser) + "/" + conduitCauseRenewalRouteUnavailable}, o.rec.unstampedList())
	lines := gateLines()
	require.Len(t, lines, 1)
	assert.Contains(t, lines[0], "route="+conduitRouteRelayTunnel)
	assert.Contains(t, lines[0], "cause="+conduitCauseRenewalRouteUnavailable)
}

// TestRelayTunnel_RecheckFromClientIgnored: the hub reads tunnel_recheck
// only from the forwarding relay's request. With enforce and a relay that
// does not list it, a client's RPC naming it in its headers or query still
// gets 503 stream_renewal_unavailable with nothing signed; one naming it in
// the request body gets 400 invalid_request (the body takes no unknown
// field), with nothing signed either.
func TestRelayTunnel_RecheckFromClientIgnored(t *testing.T) {
	t.Run("headers and query", func(t *testing.T) {
		o := &openSetup{enforce: true}
		f := newRelayTunnelFixture(t, o)

		o.keys.arm(f.cf.srv)
		resp, open := f.postWith(t, []string{relay.HubFeatureTunnels}, func(rpc *conduitv1.RpcRequest) {
			rpc.Headers = map[string]string{"Features": relay.HubFeatureTunnelRecheck, "X-Scion-Features": relay.HubFeatureTunnelRecheck}
			rpc.Query = "features=" + relay.HubFeatureTunnelRecheck
		})
		require.Equal(t, int32(http.StatusServiceUnavailable), resp.GetStatus(), string(resp.GetBody()))
		var er ErrorResponse
		require.NoError(t, json.Unmarshal(resp.GetBody(), &er))
		requireRenewalUnavailableBody(t, er)
		assert.Nil(t, open)
		assert.Zero(t, o.keys.reads.Load(), "nothing was signed")
	})
	t.Run("body", func(t *testing.T) {
		o := &openSetup{enforce: true}
		f := newRelayTunnelFixture(t, o)

		o.keys.arm(f.cf.srv)
		resp, open := f.postWith(t, []string{relay.HubFeatureTunnels}, func(rpc *conduitv1.RpcRequest) {
			rpc.Body = []byte(`{"agent":"` + f.cf.agent.ID + `","kind":"tcp","params":{"port":"3000"},"features":["` + relay.HubFeatureTunnelRecheck + `"]}`)
		})
		require.Equal(t, int32(http.StatusBadRequest), resp.GetStatus(), string(resp.GetBody()))
		var er ErrorResponse
		require.NoError(t, json.Unmarshal(resp.GetBody(), &er))
		assert.Equal(t, ErrCodeInvalidRequest, er.Error.Code)
		assert.Nil(t, open)
		assert.Zero(t, o.keys.reads.Load(), "nothing was signed")
	})
}
