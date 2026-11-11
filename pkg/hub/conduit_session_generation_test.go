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
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/conduit"
	"github.com/GoogleCloudPlatform/scion/pkg/conduit/grant"
	"github.com/GoogleCloudPlatform/scion/pkg/conduit/relay"
	"github.com/GoogleCloudPlatform/scion/pkg/hub/authctx"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// Tests of the session generation on conduit paths
// (conduit_session_generation.go). Each path that compares the generation
// has its own test, which fails if that path's comparison is removed:
//
//   - relay-held session re-check (relayUserIdentity):
//     TestConduitSessionGen_RelaySessionRecheck;
//   - relay-held stream re-check (recheckRelayStream):
//     TestConduitSessionGen_RelayStreamRecheck;
//   - hub-held stream renewal (renewConduitUserStream):
//     TestConduitSessionGen_HubHeldRenewal;
//   - in-process stream re-check, notify and sweep (conduitUserStatus):
//     TestConduitSessionGen_InProcessStreamNotify and _InProcessStreamSweep;
//   - in-process stream opens and tunnel requests (conduitUserStatus via
//     authorizeConduitUserStream): TestConduitSessionGen_InProcessOpenRefused;
//   - the track-time guard in authorizeConduitUserStream (a hub user token
//     request whose identity carries no generation is denied):
//     TestConduitSessionGen_TrackTimeGuard;
//   - capture at the request entry points:
//     TestConduitSessionGen_EntryPointsRefuseMissingGeneration and
//     TestConduitSessionGen_RelayAdmitCapturesGeneration (the missing
//     generation is produced by a hub without a user store; a deleted
//     user's token is refused earlier, by the JWT arm:
//     TestConduitSessionGen_DeletedUserRefusedBeforeCapture).

// revokeSessions revokes all of userID's sessions through the admin API.
func revokeSessions(t *testing.T, srv *Server, userID string) {
	t.Helper()
	rec := doRequest(t, srv, http.MethodPost, "/api/v1/users/"+userID+"/revoke-sessions", map[string]interface{}{})
	require.Less(t, rec.Code, 300, "revoke-sessions: %d %s", rec.Code, rec.Body.String())
}

// admissionClaims opens an admission token the relay fixture's relay-0
// holds for sessionID.
func (f *relayRenewalFixture) admissionClaims(t *testing.T, token, sessionID string) conduitAdmissionClaims {
	t.Helper()
	c, err := f.srv.verifyAdmissionToken(token, admissionCheck{sessionID: sessionID, relay: "relay-0", relayGen: f.ops.relayGen, allowExpired: true}, conduitRelayNow())
	require.NoError(t, err)
	return c
}

// reissueWithGeneration re-issues the fixture user's admission token with
// session generation gen in its sg claim.
func (f *relayRenewalFixture) reissueWithGeneration(t *testing.T, gen int64) string {
	t.Helper()
	c := f.admissionClaims(t, f.adm.AdmissionToken, relayRenewalUserSession)
	c.SessionGen = gen
	tok, err := f.srv.reissueAdmissionToken(c, time.Time{}, conduitRelayNow())
	require.NoError(t, err)
	return tok
}

func (f *relayRenewalFixture) sessionRecheck(t *testing.T, token string) relay.HubSessionRecheckResult {
	t.Helper()
	var resp relay.HubSessionRecheckResponse
	require.NoError(t, f.ops.hub.Call(context.Background(), relay.HubSessionRecheckPath, relay.HubSessionRecheckRequest{
		Items: []relay.HubSessionRecheckItem{{SessionID: relayRenewalUserSession, AdmissionToken: token}},
	}, &resp))
	require.Len(t, resp.Results, 1)
	return resp.Results[0]
}

// TestConduitSessionGen_RevokeConfirmationInverted is the conduit half of
// the revoke confirmation test, inverted: after revoke-sessions, the
// relay-held hub-JWT session closes on its session re-check and its stream
// closes on the stream re-check (both 4401).
func TestConduitSessionGen_RevokeConfirmationInverted(t *testing.T) {
	f := newRelayRenewalFixture(t)
	tok, open := f.openGrant(t, grant.ViaProxy, tcpHeader("3000"), nil)
	it := f.item(tok, open, 7)
	it.Trigger = "notify"
	require.True(t, f.relayRecheck(t, it).Renew, "the stream renews before the revocation")
	require.True(t, f.sessionRecheck(t, f.adm.AdmissionToken).OK, "the session passes before the revocation")

	revokeSessions(t, f.srv, f.user.ID())

	res := f.sessionRecheck(t, f.adm.AdmissionToken)
	assert.True(t, res.Close, "the session closes")
	assert.Equal(t, conduit.CloseUnauthenticated, res.CloseCode)
	sres := f.relayRecheck(t, it)
	assert.True(t, sres.Close, "the stream closes")
	assert.False(t, sres.Renew)
	assert.Equal(t, conduit.CloseUnauthenticated, sres.CloseCode)
}

// TestConduitSessionGen_RelaySessionRecheck: the session re-check of a
// relay-held hub-JWT session compares the admission token's sg with the
// user row: equal passes; the row ahead (a revocation) and sg ahead (a
// rollback) both close 4401 with reason session revoked. A token without
// sg reads 0: a never-revoked user passes, an ever-revoked user closes.
func TestConduitSessionGen_RelaySessionRecheck(t *testing.T) {
	f := newRelayRenewalFixture(t)
	ctx := context.Background()

	assert.True(t, f.sessionRecheck(t, f.adm.AdmissionToken).OK, "current generation passes")
	assert.True(t, f.sessionRecheck(t, f.reissueWithGeneration(t, 0)).OK, "no sg, never revoked: passes")

	newer := f.sessionRecheck(t, f.reissueWithGeneration(t, 1))
	assert.True(t, newer.Close, "sg ahead of the row closes")
	assert.Equal(t, conduit.CloseUnauthenticated, newer.CloseCode)
	assert.Equal(t, conduitReasonSessionRevoked, newer.Reason)

	require.NoError(t, f.store.IncrementSessionGeneration(ctx, f.user.ID()))
	legacy := f.sessionRecheck(t, f.reissueWithGeneration(t, 0))
	assert.True(t, legacy.Close, "no sg, ever revoked: closes")
	assert.Equal(t, conduitReasonSessionRevoked, legacy.Reason)
	behind := f.sessionRecheck(t, f.adm.AdmissionToken)
	assert.True(t, behind.Close, "the row ahead of sg closes")
	assert.Equal(t, conduit.CloseUnauthenticated, behind.CloseCode)
	assert.True(t, f.sessionRecheck(t, f.reissueWithGeneration(t, 1)).OK, "sg equal to the row passes")
}

// TestConduitSessionGen_RelayStreamRecheck: the stream re-check of a stream
// on a relay-held hub-JWT session compares the session's sg with the user
// row; it does not stand in for the session re-check (it is asked
// separately and closes the stream itself).
func TestConduitSessionGen_RelayStreamRecheck(t *testing.T) {
	f := newRelayRenewalFixture(t)
	tok, open := f.openGrant(t, grant.ViaProxy, tcpHeader("3000"), nil)
	it := f.item(tok, open, 7)
	require.True(t, f.relayRecheck(t, it).Renew)

	newer := it
	newer.AdmissionToken = f.reissueWithGeneration(t, 1)
	res := f.relayRecheck(t, newer)
	assert.True(t, res.Close, "sg ahead of the row closes the stream")
	assert.Equal(t, conduit.CloseUnauthenticated, res.CloseCode)

	require.NoError(t, f.store.IncrementSessionGeneration(context.Background(), f.user.ID()))
	for _, trigger := range []string{"notify", "sweep"} {
		it.Trigger = trigger
		res := f.relayRecheck(t, it)
		assert.True(t, res.Close, "%s: the row ahead of sg closes the stream", trigger)
		assert.Equal(t, conduit.CloseUnauthenticated, res.CloseCode, trigger)
	}
	current := it
	current.AdmissionToken = f.reissueWithGeneration(t, 1)
	assert.True(t, f.relayRecheck(t, current).Renew, "sg equal to the row renews")
}

// TestConduitSessionGen_RelayAdmitCapturesGeneration: relay admission
// records the generation of the replayed handshake's request in sg (the
// user row's, as the JWT arm validated it); a fresh token after a
// revocation admits with the new generation and passes its re-check; and
// a hub JWT request that carries no generation is refused, never admitted
// with sg 0.
func TestConduitSessionGen_RelayAdmitCapturesGeneration(t *testing.T) {
	f := newRelayRenewalFixture(t)
	assert.Equal(t, int64(0), f.admissionClaims(t, f.adm.AdmissionToken, relayRenewalUserSession).SessionGen)

	revokeSessions(t, f.srv, f.user.ID())
	// A token issued after the revocation carries the new generation (1);
	// the JWT arm refuses one that does not.
	jwt, _, _, err := f.srv.userTokenService.MintTokenPair(f.user.ID(), "renew@conduit.test", "Renew", store.UserRoleMember, 1, ClientTypeCLI)
	require.NoError(t, err)
	fresh := f.ops.mustAdmit(t, bearer(jwt), "user-fresh")
	assert.Equal(t, int64(1), f.admissionClaims(t, fresh.AdmissionToken, "user-fresh").SessionGen)
	var resp relay.HubSessionRecheckResponse
	require.NoError(t, f.ops.hub.Call(context.Background(), relay.HubSessionRecheckPath, relay.HubSessionRecheckRequest{
		Items: []relay.HubSessionRecheckItem{{SessionID: "user-fresh", AdmissionToken: fresh.AdmissionToken}},
	}, &resp))
	require.Len(t, resp.Results, 1)
	assert.True(t, resp.Results[0].OK, "a session admitted after the revocation passes")

	// Without a user store the JWT arm sets no generation.
	saved := f.srv.authConfig.UserStore
	f.srv.authConfig.UserStore = nil
	t.Cleanup(func() { f.srv.authConfig.UserStore = saved })
	_, err = f.ops.admit(t, bearer(jwt), "user-nogen")
	assert.Error(t, err, "a hub JWT request without a generation is not admitted")
}

// TestConduitSessionGen_HubHeldRenewal: the renewal of a hub-held stream
// compares the generation its identity captured with the user row.
func TestConduitSessionGen_HubHeldRenewal(t *testing.T) {
	f := newRenewalFixture(t)
	tok, open := f.openGrant(t, grant.ViaProxy, tcpHeader("3000"), nil)
	st := &conduitUserStream{
		Kind: grant.StreamKindTCP, Identity: f.user.WithCapturedSessionGeneration(0), UserID: f.user.ID(),
		AgentID: f.agent.ID, ProjectID: f.agent.ProjectID, Port: 3000,
		SessionID: renewalSessionID, StreamID: 7, Grant: tok, OpenJTI: open.JTI,
	}
	_, err := f.srv.renewConduitUserStream(context.Background(), st)
	require.NoError(t, err, "renews before the revocation")

	revokeSessions(t, f.srv, f.user.ID())
	_, err = f.srv.renewConduitUserStream(context.Background(), st)
	require.ErrorIs(t, err, errConduitPrincipalInactive)
	assert.Equal(t, ConduitRenewalDeny, ClassifyConduitRenewal(err))

	// An identity without a captured generation (not a hub JWT) is unchanged.
	st.Identity = f.user
	_, err = f.srv.renewConduitUserStream(context.Background(), st)
	assert.NoError(t, err)
}

// TestConduitSessionGen_InProcessStreamNotify: revoke-sessions closes an
// in-process stream whose identity a hub JWT authenticated with 4401 on the
// notify path; a stream of another credential (no captured generation)
// stays open.
func TestConduitSessionGen_InProcessStreamNotify(t *testing.T) {
	f := newRecheckFixture(t, nil, -1)
	jwtStream := f.open(t, f.u1.WithCapturedSessionGeneration(0), grant.StreamKindPTY, 1)
	other := f.open(t, f.u1, grant.StreamKindPTY, 3)

	revokeSessions(t, f.srv, f.u1.ID())

	assert.Equal(t, "4401 authz_expired", jwtStream.waitClosed(t))
	f.metrics.wait(t, "notify/closed/pty", 1)
	f.metrics.wait(t, "notify/passed/pty", 1)
	other.assertOpen(t)
}

// TestConduitSessionGen_InProcessStreamSweep: a revocation whose event never
// arrives closes the stream on the sweep (trigger=sweep).
func TestConduitSessionGen_InProcessStreamSweep(t *testing.T) {
	f := newRecheckFixture(t, nil, 60*time.Second)
	w := f.open(t, f.u1.WithCapturedSessionGeneration(0), grant.StreamKindPTY, 1)
	// In the store only: no event is published.
	require.NoError(t, f.store.IncrementSessionGeneration(context.Background(), f.u1.ID()))

	f.clk.Advance(59 * time.Second)
	w.assertOpen(t)
	f.clk.Advance(time.Second)
	assert.Equal(t, "4401 authz_expired", w.waitClosed(t))
	assert.Contains(t, f.metrics.list(), "sweep/closed/pty")
}

// TestConduitSessionGen_InProcessOpenRefused: once the user's sessions are
// revoked, a hub-JWT identity can neither open a stream nor pass the
// tunnel's per-request check; an identity whose captured generation is
// ahead of the row is refused too.
func TestConduitSessionGen_InProcessOpenRefused(t *testing.T) {
	f := newRecheckFixture(t, nil, -1)
	ctx := context.Background()
	authz := func(ident Identity) error {
		_, _, err := f.srv.authorizeConduitUserStream(ctx, conduitUserStreamAuthz{Identity: ident, Agent: f.agent, Kind: grant.StreamKindPTY})
		return err
	}
	require.NoError(t, authz(f.u1.WithCapturedSessionGeneration(0)))
	assert.ErrorIs(t, authz(f.u1.WithCapturedSessionGeneration(1)), errConduitPrincipalInactive, "captured ahead of the row")

	revokeSessions(t, f.srv, f.u1.ID())
	assert.ErrorIs(t, authz(f.u1.WithCapturedSessionGeneration(0)), errConduitPrincipalInactive, "stream open after the revocation")
	assert.NoError(t, authz(f.u1.WithCapturedSessionGeneration(1)), "a token issued after the revocation")
	assert.NoError(t, authz(f.u1), "another credential is unchanged")

	ts := &conduitTunnelSession{s: f.srv, identity: f.u1.WithCapturedSessionGeneration(0)}
	assert.Error(t, ts.userActive(ctx), "tunnel request after the revocation")
	ts.identity = f.u1.WithCapturedSessionGeneration(1)
	assert.NoError(t, ts.userActive(ctx))
}

// TestConduitSessionGen_EntryPointsRefuseMissingGeneration: each in-process
// entry point that keeps the request's identity refuses a hub JWT request
// without a generation (here: no user store, so the JWT arm sets none).
func TestConduitSessionGen_EntryPointsRefuseMissingGeneration(t *testing.T) {
	f := newRenewalFixture(t)
	jwt, _, _, err := f.srv.userTokenService.GenerateTokenPair(f.user.ID(), "renew@conduit.test", "Renew", store.UserRoleMember, ClientTypeCLI)
	require.NoError(t, err)
	saved := f.srv.authConfig.UserStore
	f.srv.authConfig.UserStore = nil
	t.Cleanup(func() { f.srv.authConfig.UserStore = saved })

	for name, path := range map[string]string{
		"conduit dial": "/api/v1/conduit",
		"pty":          "/api/v1/agents/" + f.agent.ID + "/pty",
		"port proxy":   "/api/v1/agents/" + f.agent.ID + "/ports/3000/proxy/",
	} {
		rec := doRequestWithToken(t, f.srv, jwt, http.MethodGet, path, nil)
		assert.Equal(t, http.StatusUnauthorized, rec.Code, "%s: %s", name, rec.Body.String())
		assert.Equal(t, `{"error":{"code":"unauthorized","message":"invalid access token"}}`+"\n", rec.Body.String(), name)
	}
}

// TestConduitSessionGen_TrackTimeGuard: a stream open or tunnel request
// whose request a hub user token authenticated is denied when its identity
// carries no session generation (an entry point that missed the capture),
// so it fails closed rather than skipping the comparison. Other credentials
// and captured identities are unaffected.
func TestConduitSessionGen_TrackTimeGuard(t *testing.T) {
	f := newRecheckFixture(t, nil, -1)
	authz := func(ctx context.Context, ident Identity) error {
		_, _, err := f.srv.authorizeConduitUserStream(ctx, conduitUserStreamAuthz{Identity: ident, Agent: f.agent, Kind: grant.StreamKindPTY})
		return err
	}
	jwt := contextWithAuthType(context.Background(), AuthTypeJWT)
	assert.ErrorIs(t, authz(jwt, f.u1), errConduitPrincipalInactive, "hub JWT request, no captured generation")
	assert.NoError(t, authz(jwt, f.u1.WithCapturedSessionGeneration(0)), "hub JWT request, captured generation")
	for _, at := range []string{"", AuthTypeUAT, AuthTypeDevToken, AuthTypeProxy} {
		assert.NoError(t, authz(contextWithAuthType(context.Background(), at), f.u1), "%q: unaffected", at)
	}
}

// TestConduitSessionGen_DeletedUserRefusedBeforeCapture: a valid hub JWT
// for a deleted user carries no generation, but it never reaches the
// capture: the JWT arm refuses it first (no user record). The capture
// refusal (above) is the backstop for any path that reaches it without a
// generation.
//
// CROSS-LANE INVARIANT: this test pins an ordering between the hub's JWT
// validation (auth.go) and the conduit capture. It fails if the JWT arm ever
// stops refusing a deleted user's token early (for example to let a later,
// more specific check give a better error). If that change is wanted, keep
// the deleted user refused before any conduit entry point, or show that the
// capture refusal then handles it; do not delete this test to make it pass.
func TestConduitSessionGen_DeletedUserRefusedBeforeCapture(t *testing.T) {
	f := newRenewalFixture(t)
	jwt, _, _, err := f.srv.userTokenService.GenerateTokenPair(f.user.ID(), "renew@conduit.test", "Renew", store.UserRoleMember, ClientTypeCLI)
	require.NoError(t, err)
	require.NoError(t, f.store.DeleteUser(context.Background(), f.user.ID()))

	for name, path := range map[string]string{
		"conduit dial": "/api/v1/conduit",
		"pty":          "/api/v1/agents/" + f.agent.ID + "/pty",
		"port proxy":   "/api/v1/agents/" + f.agent.ID + "/ports/3000/proxy/",
	} {
		rec := doRequestWithToken(t, f.srv, jwt, http.MethodGet, path, nil)
		assert.Equal(t, http.StatusUnauthorized, rec.Code, "%s: %s", name, rec.Body.String())
		assert.Contains(t, rec.Body.String(), ErrCodeUserNotFound, "%s: refused by the JWT arm", name)
	}
}

// TestConduitCaptureSessionGeneration covers the capture rule.
func TestConduitCaptureSessionGeneration(t *testing.T) {
	user := NewAuthenticatedUser("u-1", "u1@example.test", "U1", store.UserRoleMember, "cli")
	jwt := contextWithAuthType(context.Background(), AuthTypeJWT)

	got, ok := conduitCaptureSessionGeneration(jwt, user)
	assert.False(t, ok, "hub JWT without a generation")
	assert.Nil(t, got)

	got, ok = conduitCaptureSessionGeneration(authctx.WithSessionGeneration(jwt, 3), user)
	require.True(t, ok)
	gen, set := conduitCapturedSessionGeneration(got)
	assert.True(t, set)
	assert.Equal(t, int64(3), gen)
	_, set = user.CapturedSessionGeneration()
	assert.False(t, set, "the original identity is not modified")

	_, ok = conduitCaptureSessionGeneration(authctx.WithSessionGeneration(jwt, 3), NewBrokerIdentity("b-1"))
	assert.False(t, ok, "a hub JWT whose identity is not an authenticated user")

	for _, at := range []string{"", AuthTypeUAT, AuthTypeDevToken, AuthTypeProxy} {
		got, ok := conduitCaptureSessionGeneration(contextWithAuthType(context.Background(), at), user)
		assert.True(t, ok, at)
		assert.Same(t, user, got, "%q: unchanged", at)
	}

	scoped := NewScopedUserIdentity(user, "proj-1", []string{store.UATScopeAgentPortAccess})
	withGen := conduitIdentityWithSessionGeneration(scoped, 5)
	gen, set = conduitCapturedSessionGeneration(withGen)
	assert.True(t, set)
	assert.Equal(t, int64(5), gen)
	_, set = conduitCapturedSessionGeneration(scoped)
	assert.False(t, set, "the scoped original is not modified")

	assert.True(t, conduitSessionGenerationCurrent(2, 2))
	assert.False(t, conduitSessionGenerationCurrent(3, 2), "row ahead")
	assert.False(t, conduitSessionGenerationCurrent(2, 3), "captured ahead")
}
