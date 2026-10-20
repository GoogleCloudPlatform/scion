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
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/conduit/grant"
	"github.com/GoogleCloudPlatform/scion/pkg/conduit/relay"
)

// Relay-held tunnels and principals without a renewal path (design 11f26d77
// §3.5 "Principals without a renewal path" and §3.9; contracts 1bbe35f0 §2
// "Renewal path unavailable"). The relay's admission does not admit a
// federated credential (TestRelayAdmit_FederatedCredentialNotAdmitted), so
// these principals reach the relay tunnel route through openForRelay
// directly, as a forwarded tunnel request from an admitted session would.

// relayRenewalUnsupportedPrincipals are the renewal-unsupported principals a
// relay tunnel request is decided for, by the principal kind their metrics
// carry: a federated user and a visitor. A scoped identity over either has
// a user row to check, which these principals lack, so the tunnel route
// refuses it as inactive before any renewal decision; the mint-level tests
// cover scoped identities. Visitors reach the renewal decision only because
// the test allows every stream action (allowAllConduitActions); without it
// they are denied before the mint (TestMintConduitGrant_VisitorDeniedBeforeMint).
func relayRenewalUnsupportedPrincipals() map[string]UserIdentity {
	return map[string]UserIdentity{
		string(PrincipalKindFederatedUser): NewFederatedUserIdentity("https://issuer.example.com", "subject-1", "fed@example.com", "Fed", "member", nil),
		string(PrincipalKindVisitor):       NewVisitorIdentity(tid("visitor-user"), "https://issuer.example.com", "subject-2", "visitor@example.com", "Visitor"),
	}
}

// openRelayTunnelAs runs a forwarded tcp tunnel request for f's agent from
// the relay-held user session "user-session", as identity, with a relay
// that lists tunnel_recheck.
func openRelayTunnelAs(t *testing.T, f *relayTunnelFixture, identity UserIdentity) (*conduitTunnelResponse, *relay.HubTunnelOpen, error) {
	t.Helper()
	rt := f.cf.srv.conduitExternal.Load()
	require.NotNil(t, rt, "the hub is in external relay mode")
	body, err := json.Marshal(conduitTunnelRequest{Agent: f.cf.agent.ID, Kind: grant.StreamKindTCP, Params: map[string]string{grant.ParamPort: "3000"}})
	require.NoError(t, err)
	ts := newConduitTunnelSession(f.cf.srv, identity, time.Time{})
	return ts.openForRelay(context.Background(), rt, "user-session", body, relay.HubRPCRequest{Features: recheckFeatures})
}

// TestRelayTunnel_RenewalUnsupportedPrincipalEnforce: under enforce, a
// renewal-unsupported principal's tunnel to a stream_renewal.v2 target,
// forwarded by a relay that lists tunnel_recheck, is refused with 503
// unavailable, reason stream_renewal_unavailable. The metric carries route
// relay_tunnel and the cause renewal_principal_unsupported; nothing is
// minted and the relay gets no instruction.
func TestRelayTunnel_RenewalUnsupportedPrincipalEnforce(t *testing.T) {
	for kind, ident := range relayRenewalUnsupportedPrincipals() {
		t.Run(kind, func(t *testing.T) {
			o := &openSetup{enforce: true, allowAll: true}
			f := newRelayTunnelFixture(t, o)

			o.keys.arm(f.cf.srv)
			resp, open, err := openRelayTunnelAs(t, f, ident)
			var te *conduitTunnelError
			require.True(t, errors.As(err, &te), "a typed refusal, not an internal error: %v", err)
			answer := te.response()
			require.Equal(t, int32(http.StatusServiceUnavailable), answer.GetStatus(), string(answer.GetBody()))
			var er ErrorResponse
			require.NoError(t, json.Unmarshal(answer.GetBody(), &er))
			requireRenewalUnavailableBody(t, er)
			assert.Nil(t, resp)
			assert.Nil(t, open, "no instruction to open the tunnel")
			assert.Zero(t, o.keys.reads.Load(), "nothing was minted")
			assert.Equal(t, []string{conduitRouteRelayTunnel + "/" + conduitReasonStreamRenewalUnavailable + "/" + conduitCauseRenewalPrincipalUnsupported}, o.rec.list())
			assert.Empty(t, o.rec.unstampedList())
		})
	}
}

// TestRelayTunnel_RenewalUnsupportedPrincipalOff: under off, the same
// tunnel is served with an unstamped grant (no via, scope or authz_until)
// and no scheduling hints; nothing is refused, and the open is counted as
// unstamped with the principal kind and the cause
// renewal_principal_unsupported.
func TestRelayTunnel_RenewalUnsupportedPrincipalOff(t *testing.T) {
	for kind, ident := range relayRenewalUnsupportedPrincipals() {
		t.Run(kind, func(t *testing.T) {
			o := &openSetup{allowAll: true}
			f := newRelayTunnelFixture(t, o)

			o.keys.arm(f.cf.srv)
			resp, open, err := openRelayTunnelAs(t, f, ident)
			require.NoError(t, err)
			require.NotNil(t, resp)
			require.NotNil(t, open, "the tunnel is served")
			c := relayTunnelClaims(t, f, open)
			assert.Empty(t, c.Via)
			assert.Nil(t, c.Scope)
			assert.True(t, c.AuthzUntil.IsZero())
			assert.Zero(t, c.AuthzInterval)
			assert.Zero(t, open.AuthzUntil)
			assert.Zero(t, open.AuthzIntervalS)
			assert.Empty(t, o.rec.list(), "off refuses nothing")
			unstamped := o.rec.unstampedList()
			require.Len(t, unstamped, 1)
			// TODO(route-label follow-up): assert the route label relay_tunnel
			// here once the off renewal-unsupported-principal count carries it;
			// until then only the principal kind and the cause are pinned.
			assert.True(t, strings.HasSuffix(unstamped[0], "/"+kind+"/"+conduitCauseRenewalPrincipalUnsupported),
				"counted with principal kind %s and cause %s: %s", kind, conduitCauseRenewalPrincipalUnsupported, unstamped[0])
		})
	}
}

// TestRelayAdmit_FederatedCredentialNotAdmitted: a relay's admission does
// not admit a federated credential. A handshake the auth middleware left
// with a federated or visitor identity and the federation auth type is
// refused with 403 before any upgrade, so no relay-held session of such a
// principal exists.
func TestRelayAdmit_FederatedCredentialNotAdmitted(t *testing.T) {
	f := newRelayTunnelFixture(t, &openSetup{})
	for kind, ident := range relayRenewalUnsupportedPrincipals() {
		t.Run(kind, func(t *testing.T) {
			rec := httptest.NewRecorder()
			_, ok := f.cf.srv.decideConduitFor(rec, userHandshakeRequest(ident, AuthTypeFederation), true)
			assert.False(t, ok, "not admitted")
			require.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())
			code, msg := handshakeRefusal(t, rec)
			assert.Equal(t, ErrCodeForbidden, code)
			assert.Equal(t, "This credential cannot open conduit sessions", msg)
		})
	}
}
