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
	"encoding/base64"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/conduit/grant"
)

// The route label of a stream open (contracts 1bbe35f0 §2, Renewal path
// unavailable, Message and observability): relay_tunnel for any
// relay-held tunnel, tunnel for a hub-held one, the same in off and
// enforce, for every reason and cause. mintConduitGrant takes it from
// conduitGrantRequest.Route (set by mintRelayTunnel), else from Via.

// routeLabels are the two tunnel route labels and the Route each comes
// from: a hub-held tunnel leaves Route empty, a relay-held tunnel sets it.
var routeLabels = map[string]string{
	conduitRouteTunnel:      "",
	conduitRouteRelayTunnel: conduitRouteRelayTunnel,
}

// TestConduitGrantRoute_UnstampedCount: under off, the unstamped count of a
// renewal-unsupported principal's tunnel carries route relay_tunnel when the
// open is relay-held and tunnel when it is hub-held.
func TestConduitGrantRoute_UnstampedCount(t *testing.T) {
	federated := NewFederatedUserIdentity("https://issuer.example.com", "subject-1", "fed@example.com", "Fed", "member", nil)
	for want, route := range routeLabels {
		t.Run(want, func(t *testing.T) {
			f := newConduitFixture(t)
			allowAllConduitActions(f.srv)
			rec := recordOpenRefusals(f.srv)
			_, c, err := f.srv.mintConduitGrant(context.Background(), conduitGrantRequest{
				Identity: federated, Agent: f.agent, Stream: tcpHeader("3000"), Target: f.target(),
				Via: grant.ViaTunnel, TargetRenews: true, CanRenew: true, Route: route,
			})
			require.NoError(t, err)
			assert.True(t, c.AuthzUntil.IsZero(), "minted unstamped")
			assert.Equal(t, []string{want + "/" + string(PrincipalKindFederatedUser) + "/" + conduitCauseRenewalPrincipalUnsupported}, rec.unstampedList())
			assert.Empty(t, rec.list())
		})
	}
}

// TestConduitGrantRoute_RenewalPathBackstop: under enforce, the mint's
// renewal-path backstop refuses a tunnel to a stream_renewal.v2 target that
// cannot be renewed with route relay_tunnel when relay-held and tunnel when
// hub-held.
func TestConduitGrantRoute_RenewalPathBackstop(t *testing.T) {
	for want, route := range routeLabels {
		t.Run(want, func(t *testing.T) {
			f := newConduitFixture(t)
			enforceTargetDeadlines(f.srv)
			rec := recordOpenRefusals(f.srv)
			_, _, err := f.srv.mintConduitGrant(context.Background(), conduitGrantRequest{
				Identity: f.owner, Agent: f.agent, Stream: tcpHeader("3000"), Target: f.target(),
				Via: grant.ViaTunnel, TargetRenews: true, CanRenew: false, Route: route,
			})
			require.ErrorIs(t, err, errConduitStreamRenewalUnavailable)
			assert.Equal(t, []string{want + "/" + conduitReasonStreamRenewalUnavailable + "/" + conduitCauseRenewalRouteUnavailable}, rec.list())
		})
	}
}

// TestConduitGrantRoute_UpgradeBackstop: in external relay mode under
// enforce, the mint's target-upgrade backstop refuses a tunnel to a target
// without stream_renewal.v2 with route relay_tunnel when relay-held and
// tunnel when hub-held.
func TestConduitGrantRoute_UpgradeBackstop(t *testing.T) {
	for want, route := range routeLabels {
		t.Run(want, func(t *testing.T) {
			o := &openSetup{enforce: true}
			f := newRelayTunnelFixture(t, o)
			require.True(t, f.cf.srv.ConduitExternal(), "external relay mode")
			_, _, err := f.cf.srv.mintConduitGrant(context.Background(), conduitGrantRequest{
				Identity: f.cf.owner, Agent: f.cf.agent, Stream: tcpHeader("3000"), Target: f.cf.target(),
				Via: grant.ViaTunnel, TargetRenews: false, Route: route,
			})
			require.ErrorIs(t, err, errConduitAgentUpgradeRequired)
			assert.Equal(t, []string{upgradeRefusal(want)}, o.rec.list())
		})
	}
}

// TestConduitGrantRoute_NotInTheGrant: Route is a label only. The claims of
// a grant minted with Route set equal those minted without it (apart from
// the per-grant jti), and the signed claims segment names neither the field
// nor its value.
func TestConduitGrantRoute_NotInTheGrant(t *testing.T) {
	f := newConduitFixture(t)
	mint := func(route string) ([]byte, *grant.Claims) {
		tok, c, err := f.srv.mintConduitGrant(context.Background(), conduitGrantRequest{
			Identity: f.owner, Agent: f.agent, Stream: tcpHeader("3000"), Target: f.target(),
			Via: grant.ViaTunnel, TargetRenews: true, CanRenew: true, Route: route,
		})
		require.NoError(t, err)
		return tok, c
	}
	_, plain := mint("")
	tok, labelled := mint(conduitRouteRelayTunnel)
	require.False(t, labelled.AuthzUntil.IsZero(), "a stamped grant, so via and authz_until are present")
	require.NotEqual(t, plain.JTI, labelled.JTI)
	same := *labelled
	same.JTI = plain.JTI
	assert.Equal(t, *plain, same, "the claims do not depend on Route")
	assert.Equal(t, grant.ViaTunnel, labelled.Via, "via stays tunnel")

	parts := strings.Split(string(tok), ".")
	require.Len(t, parts, 3)
	signed, err := base64.RawURLEncoding.DecodeString(parts[1])
	require.NoError(t, err)
	assert.NotContains(t, string(signed), conduitRouteRelayTunnel)
	assert.NotContains(t, strings.ToLower(string(signed)), "route")
}
