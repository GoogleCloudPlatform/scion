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
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/conduit"
	"github.com/GoogleCloudPlatform/scion/pkg/conduit/grant"
	"github.com/GoogleCloudPlatform/scion/pkg/conduit/relay"
	"github.com/GoogleCloudPlatform/scion/pkg/conduit/relayrole"
	"github.com/GoogleCloudPlatform/scion/pkg/conduit/relayrole/relayserver"
	"github.com/GoogleCloudPlatform/scion/pkg/conduit/target"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	conduitv1 "github.com/GoogleCloudPlatform/scion/proto/conduit/v1"
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
// unstamped with route relay_tunnel, the principal kind and the cause
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
			// The relay-held tunnel's count carries route relay_tunnel (a
			// caller that left Route unset would fall back to tunnel).
			assert.Equal(t, []string{conduitRouteRelayTunnel + "/" + kind + "/" + conduitCauseRenewalPrincipalUnsupported}, o.rec.unstampedList())
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

// Relay-held tunnels end to end (design 11f26d77 §3.9 "Scheduling hints"
// and "Relay feature tunnel_recheck"; contracts 1bbe35f0 §3): hubs A and B
// in external relay mode under enforce with a 2m user stream interval, the
// agent's session on relay-0, and a second relay, relay-1. One clock is
// shared: grants are minted on f.clock, the agent's fixture clock and both
// relays' clocks read it (relayClockFor), and advance moves every timer by
// the same amount. If any clock drifted, the relay would find the hint out
// of range and fall back to the sweep, so every case requires the hint
// fallback counters to stay zero: the re-checks were armed by the hint.

// relayE2ESweep is the relays' re-check sweep in these cases: after the
// first hint re-check (at 80% of the interval, 96s) and before the target
// deadline (authz_until plus target.DeadlineGrace, 150s), so the hint, not
// the sweep, makes the first renewal, and one sweep fits before a
// deadline.
const relayE2ESweep = 100 * time.Second

// relayRenewalE2E is the cross-relay world on one shared fake clock.
type relayRenewalE2E struct {
	cw *crossRelayWorld

	mu sync.Mutex
	// refreshed are the stream ids of the AuthRefresh frames the agent's
	// session received.
	refreshed []uint32
	// closed are the close codes of the StreamClose frames the agent's
	// session received, by stream id.
	closed map[uint32]uint32
}

func newRelayRenewalE2E(t *testing.T) *relayRenewalE2E {
	t.Helper()
	e := &relayRenewalE2E{closed: map[uint32]uint32{}}
	e.cw = newCrossRelayWorldWith(t, separateRelayOptions{
		enforce:              true,
		srvMod:               func(s *Server) { s.config.ConduitUserStreamAuthzMax = e2eInterval },
		relayFakeClocks:      true,
		relayRecheckInterval: relayE2ESweep,
		worldMod: func(w *separateRelayWorld) {
			// Keepalive and write timers far out, so advancing the agent's
			// clock fires only the timers under test.
			w.agentSession.PingInterval = time.Hour
			w.agentSession.PongWait = 24 * time.Hour
			w.agentSession.WriteWait = time.Hour
			next := w.agentSession.Interceptor
			w.agentSession.Interceptor = func(dir conduit.Direction, f *conduitv1.Frame) []*conduitv1.Frame {
				if dir == conduit.Inbound {
					if ar := f.GetAuthRefresh(); ar != nil {
						e.mu.Lock()
						e.refreshed = append(e.refreshed, ar.GetStreamId())
						e.mu.Unlock()
					}
					if sc := f.GetStreamClose(); sc != nil {
						e.mu.Lock()
						e.closed[sc.GetStreamId()] = sc.GetCode()
						e.mu.Unlock()
					}
				}
				if next == nil {
					return []*conduitv1.Frame{f}
				}
				return next(dir, f)
			}
		},
	})
	for _, id := range []string{"relay-0", "relay-1"} {
		require.NotNil(t, e.cw.relayClocks[id], "%s runs on a fake clock", id)
		require.True(t, e.cw.relayClocks[id].Now().Equal(e.cw.clock.Now()), "%s's clock starts at the mint clock", id)
	}
	require.True(t, e.cw.agentClock.Now().Equal(e.cw.clock.Now()), "the agent's clock starts at the mint clock")
	return e
}

// advance moves the mint clock first, then the agent's timers, the hubs'
// re-check clocks and last the relays' clocks: a relay re-check fires
// synchronously from its clock's Advance, and the hub mints the renewal on
// a mint clock, and the agent checks it on a clock, that already read the
// same time.
func (e *relayRenewalE2E) advance(d time.Duration) {
	e.cw.clock.Advance(d)
	e.cw.agentClock.Advance(d)
	e.cw.authzClocks["hub-a"].Advance(d)
	e.cw.authzClocks["hub-b"].Advance(d)
	e.cw.relayClocks["relay-0"].Advance(d)
	e.cw.relayClocks["relay-1"].Advance(d)
}

// advanceTo advances every clock to at.
func (e *relayRenewalE2E) advanceTo(t *testing.T, at time.Time) {
	t.Helper()
	d := at.Sub(e.cw.clock.Now())
	require.Positive(t, d, "advance to %v from %v", at, e.cw.clock.Now())
	e.advance(d)
}

// waitFor polls cond (under e.mu) for up to 10s.
func (e *relayRenewalE2E) waitFor(cond func() bool) bool {
	deadline := time.Now().Add(10 * time.Second)
	for {
		e.mu.Lock()
		ok := cond()
		e.mu.Unlock()
		if ok {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// refreshedFor reports whether an AuthRefresh for stream id reached the
// agent.
func (e *relayRenewalE2E) refreshedFor(id uint32) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	return slices.Contains(e.refreshed, id)
}

// requireNoHintFallback requires that neither relay scheduled a tunnel by
// the sweep alone: every hint was present and in range.
func (e *relayRenewalE2E) requireNoHintFallback(t *testing.T) {
	t.Helper()
	for name, srv := range map[string]*relayserver.Server{"relay-0": e.cw.relay, "relay-1": e.cw.relay1} {
		assert.Zero(t, srv.Role.Streams.HintFallbacks(relayrole.HintFallbackAbsent), "%s: hint absent", name)
		assert.Zero(t, srv.Role.Streams.HintFallbacks(relayrole.HintFallbackInvalid), "%s: hint invalid", name)
	}
}

// relayTunnelE2E is one tunnel opened from a user session on a relay.
type relayTunnelE2E struct {
	user *relayUser
	st   conduit.Stream
	// holder is the relay holding the user's session, which schedules the
	// tunnel's re-checks.
	holder *relayserver.Server
	tn     relayrole.Tunnel
	// authzUntil is the open grant's authz_until; firstRecheck is when the
	// hint arms the first re-check.
	authzUntil   time.Time
	firstRecheck time.Time
}

// openTunnel opens a tcp tunnel to the agent's app from a user session on
// holder (at addr, with bearer), requires the relay to track it with valid
// hints, the agent to enforce the open grant's deadline, and the holder to
// arm the first re-check at the hint's time on its clock.
func (e *relayRenewalE2E) openTunnel(t *testing.T, holder *relayserver.Server, holderID, addr, bearer string) *relayTunnelE2E {
	t.Helper()
	x := &relayTunnelE2E{holder: holder}
	x.user = e.cw.dialUserAt(t, addr, bearer)
	_, x.st = x.user.openTCPTunnel(t, e.cw.launched.ID, e.cw.app.port)
	portHello(t, x.st, "/opened")
	tunnels := holder.Role.Tunnels()
	require.Len(t, tunnels, 1)
	x.tn = tunnels[0]
	require.Equal(t, 1, holder.Role.Streams.Len(), "the holder tracks the tunnel in its stream scheduler")
	until, intervalS := grantDeadlineClaims(t, x.tn.OpenGrant)
	require.NotZero(t, until, "the open grant to a stream_renewal.v2 target carries authz_until")
	require.Equal(t, int64(e2eInterval/time.Second), intervalS)
	x.authzUntil = time.Unix(until, 0)
	x.firstRecheck = x.authzUntil.Add(-time.Duration(float64(e2eInterval) * (1 - relayrole.RenewalLead)))
	e.requireNoHintFallback(t)
	require.True(t, e.cw.agentClock.WaitForTimer(10*time.Second, x.authzUntil.Add(target.DeadlineGrace)),
		"the agent enforces the open grant's deadline")
	require.True(t, e.cw.relayClocks[holderID].WaitForTimer(10*time.Second, x.firstRecheck),
		"%s armed the first re-check from the hint, on its clock", holderID)
	return x
}

// requireRenewed advances past the first re-check and requires the renewal
// to reach the agent's stream and move its deadline, and the tunnel to
// outlive the open grant's deadline.
func (e *relayRenewalE2E) requireRenewed(t *testing.T, x *relayTunnelE2E) {
	t.Helper()
	e.advanceTo(t, x.firstRecheck.Add(time.Second))
	renewedUntil := e.cw.clock.Now().Truncate(time.Second).Add(e2eInterval)
	require.Truef(t, e.waitFor(func() bool { return slices.Contains(e.refreshed, x.tn.Target.StreamID) }),
		"the renewal reached the agent's stream (AuthRefresh for stream %d)", x.tn.Target.StreamID)
	require.True(t, e.cw.agentClock.WaitForTimer(10*time.Second, renewedUntil.Add(target.DeadlineGrace)),
		"the agent accepted the renewal and moved its deadline")
	// Past the open grant's deadline plus the target's grace.
	e.advanceTo(t, x.authzUntil.Add(target.DeadlineGrace).Add(10*time.Second))
	portHello(t, x.st, "/still-open")
	e.requireNoHintFallback(t)
}

// TestRelayRenewalE2E_SameRelay (criterion 29, relay-held tunnel, no
// bridge): a tunnel whose user session and target session relay-0 holds is
// re-checked at the hint's time, renewed by hub A, and the renewal goes to
// the agent's stream on relay-0's own session: the agent's deadline moves
// and the tunnel outlives the open grant's deadline.
func TestRelayRenewalE2E_SameRelay(t *testing.T) {
	e := newRelayRenewalE2E(t)
	x := e.openTunnel(t, e.cw.relay, "relay-0", e.cw.relay.PublicAddr.String(), e.cw.ownerTokenA(t))
	require.Nil(t, x.tn.Bridge, "no bridge on the same relay")
	e.requireRenewed(t, x)
}

// TestRelayRenewalE2E_Bridged (criterion 29, relay-held tunnel across two
// relays): a tunnel from a user session on relay-1 to the agent on relay-0
// is re-checked by relay-1 at the hint's time, renewed by hub B, and the
// renewal travels on the tunnel's one bridge through relay-0 to the agent.
func TestRelayRenewalE2E_Bridged(t *testing.T) {
	e := newRelayRenewalE2E(t)
	x := e.openTunnel(t, e.cw.relay1, "relay-1", e.cw.relay1Addr, e.cw.userTokenB)
	require.NotNil(t, x.tn.Bridge, "relay-1 bridges to the owner relay")
	assert.Equal(t, "relay-0", x.tn.Bridge.Owner().InstanceID)
	e.requireRenewed(t, x)
}

// TestRelayRenewalE2E_UATRevokedBridged (S2, end to end): a tunnel opened
// with a port-access token from a user session on relay-1 to the agent on
// relay-0. Once the token is revoked, relay-1's next sweep gets close 4401
// for the session from hub B, and both legs end with 4401: the user's
// stream, and the agent's stream at the far end of the bridge.
func TestRelayRenewalE2E_UATRevokedBridged(t *testing.T) {
	e := newRelayRenewalE2E(t)
	ownerID := e.cw.launched.OwnerID
	key, row, err := e.cw.hubB.uatService.CreateToken(rs4MintContext(ownerID), ownerID, "relay", e.cw.launched.ProjectID, []string{store.UATScopeAgentPortAccess}, nil)
	require.NoError(t, err)
	x := e.openTunnel(t, e.cw.relay1, "relay-1", e.cw.relay1Addr, key)
	require.NotNil(t, x.tn.Bridge)

	require.NoError(t, e.cw.store.RevokeUserAccessToken(context.Background(), row.ID))
	sweep := e.cw.relayClocks["relay-1"].Now().Add(relayE2ESweep)
	require.True(t, sweep.Before(x.authzUntil.Add(target.DeadlineGrace)), "the sweep comes before the target's deadline")
	e.advance(relayE2ESweep)

	err = streamEnd(t, x.st)
	assert.Equal(t, conduit.CloseUnauthenticated, conduit.CodeOf(err, 0), "the user's leg ends with 4401: %v", err)
	require.Truef(t, e.waitFor(func() bool { _, ok := e.closed[x.tn.Target.StreamID]; return ok }),
		"the agent's stream %d was closed", x.tn.Target.StreamID)
	e.mu.Lock()
	agentCode := e.closed[x.tn.Target.StreamID]
	e.mu.Unlock()
	assert.Equal(t, conduit.CloseUnauthenticated, agentCode, "the agent's leg ends with 4401")
	require.Eventually(t, func() bool { return len(e.cw.relay1.Role.Tunnels()) == 0 }, 10*time.Second, 10*time.Millisecond,
		"relay-1 no longer holds the tunnel")
	e.requireNoHintFallback(t)
}

// TestRelayRenewalE2E_NoRenewalClosesAtTargetDeadline (the control): hub
// A's renewal re-check answers unavailable for the control tunnel (a
// stub at the entry the relay re-check RPC uses, returning
// errConduitAuthzUnavailable at once). The hint and the sweep run
// normally and re-check the tunnel, nothing reaches the agent, and both
// legs stay open until the target's deadline (authz_until plus
// target.DeadlineGrace); past it the agent closes its stream with 4401
// authz_expired and the relay ends the user's leg with the same close.
// No counter or log exists for an unavailable result, so the stub counts
// the attempts.
func TestRelayRenewalE2E_NoRenewalClosesAtTargetDeadline(t *testing.T) {
	e := newRelayRenewalE2E(t)
	x := e.openTunnel(t, e.cw.relay, "relay-0", e.cw.relay.PublicAddr.String(), e.cw.ownerTokenA(t))
	control := x.tn.Target.StreamID
	renew := e.cw.srv.streamRenewer()
	require.NotNil(t, renew, "hub A serves relay re-checks")
	var mu sync.Mutex
	attempts := 0
	e.cw.srv.setConduitStreamRenewer(func(ctx context.Context, req conduitRenewalRequest) (*conduitRenewal, error) {
		if req.TargetStreamID == control {
			mu.Lock()
			attempts++
			mu.Unlock()
			return nil, errConduitAuthzUnavailable
		}
		return renew(ctx, req)
	})

	deadline := x.authzUntil.Add(target.DeadlineGrace)
	e.advanceTo(t, deadline.Add(-time.Second))
	mu.Lock()
	got := attempts
	mu.Unlock()
	assert.GreaterOrEqual(t, got, 1, "the relay re-checked the tunnel")
	assert.False(t, e.refreshedFor(control), "no renewal reached the agent's stream")
	portHello(t, x.st, "/before-deadline")
	require.Len(t, e.cw.relay.Role.Tunnels(), 1, "both legs are still open")

	e.advance(2 * time.Second)
	requireAuthzExpired(t, streamEnd(t, x.st))
	require.Eventually(t, func() bool { return len(e.cw.relay.Role.Tunnels()) == 0 }, 10*time.Second, 10*time.Millisecond,
		"relay-0 no longer holds the tunnel")
	e.requireNoHintFallback(t)
}
