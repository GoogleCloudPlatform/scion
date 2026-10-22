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
	"bytes"
	"context"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"log/slog"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/conduit"
	"github.com/GoogleCloudPlatform/scion/pkg/conduit/grant"
	"github.com/GoogleCloudPlatform/scion/pkg/conduit/registry"
	"github.com/GoogleCloudPlatform/scion/pkg/conduit/relay"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/GoogleCloudPlatform/scion/pkg/store/entadapter"
	"github.com/GoogleCloudPlatform/scion/pkg/store/enttest"
)

// Tests of the stream evaluation's three entry points (design v2.21.40
// §3.9, One evaluation, three entry points): hub-held renewal
// (renewConduitStream), relay-held renewal (recheckRelayStream, through the
// hub internal API) and the in-process re-check (checkConduitUserStream).

// relayRenewalFixture is a renewal fixture whose hub runs external relay
// mode: the agent's session is in the shared registry, and the renewal
// user holds a session on relay-0.
type relayRenewalFixture struct {
	*renewalFixture
	ops *relayOpsFixture
	jwt string
	adm relay.HubAdmitResponse
}

const relayRenewalUserSession = "user-renew"

// newRelayRenewalFixture builds the fixture. Use one per test scope, as for
// newRenewalFixture.
func newRelayRenewalFixture(t *testing.T) *relayRenewalFixture {
	t.Helper()
	ctx := context.Background()
	rf := newRenewalFixture(t)
	// External relay mode replaces the in-process runtime.
	rf.srv.conduit.Store(nil)
	ops := startRelayOpsHub(t, rf.srv, rf.store)

	// The agent's session in the shared registry. On SQLite the registry
	// test database is named after the test, so the renewal fixture's row
	// may already be there; on Postgres each client has its own schema.
	found := false
	if ps, ok, err := ops.regStore.ListPrincipalSessionsBySession(ctx, renewalSessionID); err == nil && ok {
		for _, v := range ps.Sessions {
			if v.Session.SessionID == renewalSessionID {
				found = true
				rf.regTarget.ConnectionEpoch = v.Session.ConnectionEpoch
			}
		}
	}
	if !found {
		now := time.Now()
		epoch, err := ops.regStore.InsertSessionWithNextEpoch(ctx, registry.SessionRecord{
			SessionID: renewalSessionID, PrincipalKind: registry.PrincipalAgent, PrincipalID: rf.agent.ID,
			ProjectID: rf.agent.ProjectID, RelayInstanceID: "relay-0", RelayGeneration: ops.relayGen, Transport: "ws",
			EndpointIncarnation: rf.regTarget.EndpointIncarnation, ConnectedAt: now, LastSeen: now,
		})
		require.NoError(t, err)
		rf.regTarget.ConnectionEpoch = epoch
	}

	jwt, _, _, err := rf.srv.userTokenService.GenerateTokenPair(rf.user.ID(), "renew@conduit.test", "Renew", store.UserRoleMember, ClientTypeWeb)
	require.NoError(t, err)
	f := &relayRenewalFixture{renewalFixture: rf, ops: ops, jwt: jwt}
	f.adm = ops.mustAdmit(t, bearer(jwt), relayRenewalUserSession)
	return f
}

// item is the relay's re-check item for the stream with open grant open
// and latest grant latest, on the renewal user's session.
func (f *relayRenewalFixture) item(latest []byte, open grant.Claims, streamID uint32) relay.HubStreamRecheckItem {
	return relay.HubStreamRecheckItem{
		UserSessionID: relayRenewalUserSession, AdmissionToken: f.adm.AdmissionToken,
		TargetSessionID: renewalSessionID, TargetStreamID: streamID, OpenJTI: open.JTI,
		LatestGrant: latest, Trigger: "sweep",
	}
}

func (f *relayRenewalFixture) relayRecheck(t *testing.T, it relay.HubStreamRecheckItem) relay.HubStreamRecheckResult {
	t.Helper()
	return streamRecheck(t, f.ops.hub, it)[0]
}

// userStream is the hub-held record of the stream open was minted for, as
// a track site records it.
func (f *relayRenewalFixture) userStream(open grant.Claims) *conduitUserStream {
	port := 0
	if open.Stream.Kind == grant.StreamKindTCP {
		port = 3000
	}
	return &conduitUserStream{
		Kind: open.Stream.Kind, Identity: f.user, UserID: f.user.ID(), AgentID: f.agent.ID, ProjectID: open.ProjectID,
		Port: port, ViaTunnel: open.Via == grant.ViaTunnel, SessionID: renewalSessionID, StreamID: 7,
	}
}

// recheckClass names the outcome of a stream re-check in one vocabulary
// for all three entry points.
type recheckClass string

const (
	recheckRenewed     recheckClass = "renewed"
	recheckDeny        recheckClass = "deny"
	recheckTargetGone  recheckClass = "target_gone"
	recheckUnavailable recheckClass = "unavailable"
)

func classOfRenewal(err error) recheckClass {
	if err == nil {
		return recheckRenewed
	}
	switch ClassifyConduitRenewal(err) {
	case ConduitRenewalDeny:
		return recheckDeny
	case ConduitRenewalTargetGone:
		return recheckTargetGone
	default:
		return recheckUnavailable
	}
}

func classOfRelay(r relay.HubStreamRecheckResult) recheckClass {
	switch {
	case r.Renew:
		return recheckRenewed
	case r.Close && r.CloseCode == conduit.CloseUnauthenticated:
		return recheckDeny
	case r.Close && r.CloseCode == relay.CloseTargetNotFound:
		return recheckTargetGone
	case r.Close:
		return recheckClass("close_" + r.Reason)
	default:
		return recheckUnavailable
	}
}

func classOfVerdict(v conduitAuthzVerdict) recheckClass {
	switch v {
	case conduitAuthzAllowed:
		return recheckRenewed
	case conduitAuthzDenied:
		return recheckDeny
	case conduitAuthzTargetGone:
		return recheckTargetGone
	default:
		return recheckUnavailable
	}
}

// TestStreamRecheck_ThreeEntriesSameClass (design v2.21.40 §3.9): the same
// principal and agent state reach the same class through the hub-held
// renewal, the relay-held renewal and the in-process re-check.
func TestStreamRecheck_ThreeEntriesSameClass(t *testing.T) {
	otherProject := tid("recheck-other-project")
	for _, tc := range []struct {
		name     string
		mutGrant func(*grant.Claims)
		before   func(t *testing.T, f *relayRenewalFixture)
		want     recheckClass
	}{
		{name: "allowed", want: recheckRenewed},
		{name: "revoked", before: func(t *testing.T, f *relayRenewalFixture) { f.deleteBinding(t, f.portBind) }, want: recheckDeny},
		{name: "agent gone", before: func(t *testing.T, f *relayRenewalFixture) {
			require.NoError(t, f.store.DeleteAgent(context.Background(), f.agent.ID))
		}, want: recheckTargetGone},
		{name: "project mismatch", mutGrant: func(c *grant.Claims) { c.ProjectID = otherProject }, want: recheckDeny},
		{name: "store error", before: func(t *testing.T, f *relayRenewalFixture) {
			_, fault := installStoreFault(t, f.srv, func(inner store.Store, sw *storeFaultSwitch) *agentReadFaultStore {
				return &agentReadFaultStore{Store: inner, err: errors.New("injected agent read fault"), fault: sw}
			})
			fault.Arm()
		}, want: recheckUnavailable},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newRelayRenewalFixture(t)
			tok, open := f.openGrant(t, grant.ViaProxy, tcpHeader("3000"), tc.mutGrant)
			if tc.before != nil {
				tc.before(t, f)
			}
			_, err := f.renew(tok, open, 7)
			hub := classOfRenewal(err)
			relayHeld := classOfRelay(f.relayRecheck(t, f.item(tok, open, 7)))
			verdict, reason := f.srv.checkConduitUserStream(context.Background(), f.userStream(open))
			inProcess := classOfVerdict(verdict)

			assert.Equal(t, tc.want, hub, "hub-held renewal: %v", err)
			assert.Equal(t, tc.want, relayHeld, "relay-held renewal")
			assert.Equal(t, tc.want, inProcess, "in-process re-check: %q", reason)
		})
	}
}

// TestRelayStreamRecheck_ExternalModeRenewsAndDenies (design §3.9 gate 1):
// in external relay mode a relay-held stream's re-check is evaluated: it
// renews with a grant the target accepts while the permission holds, and
// is a deny (4401) once it is revoked. The hub-held renewal on the same
// node renews too.
func TestRelayStreamRecheck_ExternalModeRenewsAndDenies(t *testing.T) {
	f := newRelayRenewalFixture(t)
	tok, open := f.openGrant(t, grant.ViaPTY, renewalPTYHeader(), nil)

	// A renewal only extends the deadline: on the open grant's own clock its
	// authz_until would equal the open grant's, which the target refuses.
	f.clock.Advance(time.Hour)
	res := f.relayRecheck(t, f.item(tok, open, 7))
	require.True(t, res.Renew, "%+v", res)
	assert.False(t, res.Close)
	f.targetAccepts(t, &conduitRenewal{Token: res.RenewalGrant, AuthzUntil: res.AuthzUntil}, open, 7, open.AuthzUntil)

	_, err := f.renew(tok, open, 7)
	require.NoError(t, err, "the hub-held renewal is evaluated in external relay mode too")

	f.deleteBinding(t, f.attachBind)
	res = f.relayRecheck(t, f.item(tok, open, 7))
	assert.False(t, res.Renew)
	assert.True(t, res.Close)
	assert.Equal(t, conduit.CloseUnauthenticated, res.CloseCode)
	assert.Equal(t, conduitReasonAuthzExpired, res.Reason)
}

// TestRelayStreamRecheck_ScopedStaysScoped (B2): a relay-held stream opened
// with a scoped token is re-checked under the grant's scope, never as the
// unscoped user: it is denied where the scope denies, though the user holds
// the permission.
func TestRelayStreamRecheck_ScopedStaysScoped(t *testing.T) {
	f := newRelayRenewalFixture(t)
	portOnly := mustScope(t, NewScopedUserIdentity(f.user, f.agent.ProjectID, []string{store.UATScopeAgentPortAccess}))

	scopedPTY, scopedPTYOpen := f.openGrant(t, grant.ViaPTY, renewalPTYHeader(), func(c *grant.Claims) {
		c.Scope, c.JTI = portOnly, "open-relay-scoped-pty"
	})
	res := f.relayRecheck(t, f.item(scopedPTY, scopedPTYOpen, 7))
	assert.True(t, res.Close, "the user holds attach, the token's scope does not")
	assert.Equal(t, conduit.CloseUnauthenticated, res.CloseCode)

	plainPTY, plainPTYOpen := f.openGrant(t, grant.ViaPTY, renewalPTYHeader(), nil)
	assert.True(t, f.relayRecheck(t, f.item(plainPTY, plainPTYOpen, 8)).Renew, "the same user's unscoped PTY stream renews")

	scopedTCP, scopedTCPOpen := f.openGrant(t, grant.ViaProxy, tcpHeader("3000"), func(c *grant.Claims) {
		c.Scope, c.JTI = portOnly, "open-relay-scoped-tcp"
	})
	res = f.relayRecheck(t, f.item(scopedTCP, scopedTCPOpen, 9))
	require.True(t, res.Renew, "a port-scoped proxy TCP stream renews: %+v", res)
}

// TestRelayStreamRecheck_SessionBinding (D2): a relay presenting one
// user's grant on another user's session gets a deny (4401). On a hub-held
// stream the same mismatch is unavailable.
func TestRelayStreamRecheck_SessionBinding(t *testing.T) {
	f := newRelayRenewalFixture(t)
	tok, open := f.openGrant(t, grant.ViaProxy, tcpHeader("3000"), nil)

	owner := f.ops.mustAdmit(t, bearer(ownerJWT(t, f.conduitFixture)), "owner-s1")
	it := f.item(tok, open, 7)
	it.UserSessionID, it.AdmissionToken = "owner-s1", owner.AdmissionToken
	res := f.relayRecheck(t, it)
	assert.False(t, res.Renew)
	assert.True(t, res.Close)
	assert.Equal(t, conduit.CloseUnauthenticated, res.CloseCode)

	req := conduitRenewalRequest{LatestGrant: tok, TargetSessionID: renewalSessionID, TargetStreamID: 7, OpenJTI: open.JTI, PrincipalID: f.owner.ID()}
	_, err := f.srv.renewConduitStream(context.Background(), req)
	assert.ErrorIs(t, err, errConduitRenewalUnavailable)
	assert.NotErrorIs(t, err, errConduitRenewalSessionBinding)
	assert.Equal(t, ConduitRenewalUnavailable, ClassifyConduitRenewal(err), "hub-held: an internal inconsistency")

	req.RelayHeld = true
	_, err = f.srv.renewConduitStream(context.Background(), req)
	assert.ErrorIs(t, err, errConduitRenewalSessionBinding)
	assert.Equal(t, ConduitRenewalDeny, ClassifyConduitRenewal(err), "relay-held: a binding failure")
}

// TestRelayStreamRecheck_InactivePrincipal (B4): a suspended user's
// relay-held stream is denied; a grant naming a user with no row is an
// inactive principal.
func TestRelayStreamRecheck_InactivePrincipal(t *testing.T) {
	f := newRelayRenewalFixture(t)
	tok, open := f.openGrant(t, grant.ViaProxy, tcpHeader("3000"), nil)
	ghostTok, ghostOpen := f.openGrant(t, grant.ViaProxy, tcpHeader("3000"), func(c *grant.Claims) {
		c.Subject, c.JTI = grant.SubjectUserPrefix+tid("renew-ghost"), "open-ghost"
	})
	_, err := f.renew(ghostTok, ghostOpen, 7)
	assert.ErrorIs(t, err, errConduitPrincipalInactive)
	assert.Equal(t, ConduitRenewalDeny, ClassifyConduitRenewal(err))

	setUserStatus(t, f.store, f.user.ID(), store.UserStatusSuspended)
	res := f.relayRecheck(t, f.item(tok, open, 7))
	assert.True(t, res.Close)
	assert.Equal(t, conduit.CloseUnauthenticated, res.CloseCode)
}

// TestRelayStreamRecheck_UnavailableCarriesNoDeadline (B5): when the hub
// cannot evaluate, the relay gets neither a renewal nor a close, and no new
// deadline: the stream ends at its current one.
func TestRelayStreamRecheck_UnavailableCarriesNoDeadline(t *testing.T) {
	f := newRelayRenewalFixture(t)
	tok, open := f.openGrant(t, grant.ViaProxy, tcpHeader("3000"), nil)
	_, fault := installStoreFault(t, f.srv, func(inner store.Store, sw *storeFaultSwitch) *agentReadFaultStore {
		return &agentReadFaultStore{Store: inner, err: errors.New("injected agent read fault"), fault: sw}
	})
	fault.Arm()
	res := f.relayRecheck(t, f.item(tok, open, 7))
	assert.False(t, res.Renew)
	assert.False(t, res.Close)
	assert.Empty(t, res.RenewalGrant)
	assert.True(t, res.AuthzUntil.IsZero(), "no deadline is carried on unavailable")
}

// TestRelayStreamRecheck_PlacementInconsistentIsUnavailable (B9): a pinned
// agent whose saved placement is inconsistent gets no renewal on the relay
// path, and the stream is not closed.
func TestRelayStreamRecheck_PlacementInconsistentIsUnavailable(t *testing.T) {
	f := newRelayRenewalFixture(t)
	tok, open := f.openGrant(t, grant.ViaProxy, tcpHeader("3000"), nil)
	require.True(t, f.relayRecheck(t, f.item(tok, open, 7)).Renew, "consistent placement renews")

	pinInconsistently(t, f.conduitFixture)
	res := f.relayRecheck(t, f.item(tok, open, 7))
	assert.False(t, res.Renew)
	assert.False(t, res.Close, "an inconsistent placement is never a close")
}

// TestRelayStreamRecheck_ExpiredAdmissionTokenAccepted (D4): the stream
// re-check accepts an admission token past its exp, because it evaluates
// current state; the renewal's deadline is set from mint time.
func TestRelayStreamRecheck_ExpiredAdmissionTokenAccepted(t *testing.T) {
	f := newRelayRenewalFixture(t)
	now := relayClock(t, time.Now())
	late := f.ops.mustAdmit(t, bearer(f.jwt), "user-late")
	tok, open := f.openGrant(t, grant.ViaProxy, tcpHeader("3000"), nil)
	*now = now.Add(conduitAdmissionMaxAge + time.Hour)

	it := f.item(tok, open, 7)
	it.UserSessionID, it.AdmissionToken = "user-late", late.AdmissionToken
	res := f.relayRecheck(t, it)
	require.True(t, res.Renew, "%+v", res)
	assert.True(t, res.AuthzUntil.Equal(f.clock.Now().Truncate(time.Second).Add(f.srv.conduitUserStreamAuthzInterval())),
		"authz_until is mint time plus the interval, not clamped to the credential")

	bad := it
	bad.AdmissionToken = tamper(late.AdmissionToken)
	assert.True(t, f.relayRecheck(t, bad).Close, "the MAC is still checked")
}

// TestRelayHeldStream_UATRevokedEndsWithinOneSweep (S2): the stream
// re-check does not consult the access token's own status (design §3.5,
// Scope claim, for parity with the in-process path), so a revoked access
// token ends a relay-held stream through the session re-check: on the
// relay's next sweep the hub answers close 4401 for the session holding
// the stream's leaf.
func TestRelayHeldStream_UATRevokedEndsWithinOneSweep(t *testing.T) {
	f := newRelayRenewalFixture(t)
	ctx := context.Background()
	key, row, err := f.srv.uatService.CreateToken(rs4MintContext(f.user.ID()), f.user.ID(), "relay", f.agent.ProjectID, []string{store.UATScopeAgentPortAccess}, nil)
	require.NoError(t, err)
	uat := f.ops.mustAdmit(t, bearer(key), "user-uat")
	portOnly := mustScope(t, NewScopedUserIdentity(f.user, f.agent.ProjectID, []string{store.UATScopeAgentPortAccess}))
	tok, open := f.openGrant(t, grant.ViaProxy, tcpHeader("3000"), func(c *grant.Claims) { c.Scope, c.JTI = portOnly, "open-uat" })
	it := f.item(tok, open, 7)
	it.UserSessionID, it.AdmissionToken = "user-uat", uat.AdmissionToken
	require.True(t, f.relayRecheck(t, it).Renew, "renews before the revocation")

	require.NoError(t, f.store.RevokeUserAccessToken(ctx, row.ID))
	assert.True(t, f.relayRecheck(t, it).Renew, "the stream re-check does not consult the token's own status")
	var resp relay.HubSessionRecheckResponse
	require.NoError(t, f.ops.hub.Call(ctx, relay.HubSessionRecheckPath, relay.HubSessionRecheckRequest{
		Items: []relay.HubSessionRecheckItem{{SessionID: "user-uat", AdmissionToken: uat.AdmissionToken}},
	}, &resp))
	require.Len(t, resp.Results, 1)
	assert.True(t, resp.Results[0].Close, "the session holding the stream's leaf closes on the sweep")
	assert.Equal(t, conduit.CloseUnauthenticated, resp.Results[0].CloseCode)
}

// countingNilAgentStore returns the agent on its first read after Arm and
// neither an agent nor an error on every later one.
type countingNilAgentStore struct {
	store.Store
	fault *storeFaultSwitch
	reads atomic.Int64
}

func (s *countingNilAgentStore) GetAgent(ctx context.Context, id string) (*store.Agent, error) {
	if s.fault.Active() && s.reads.Add(1) > 1 {
		return nil, nil
	}
	return s.Store.GetAgent(ctx, id)
}

// TestConduitRenewStream_ConfirmDenyNilAgentIsUnavailable (B4): a deny is
// confirmed only when the agent re-read returns the agent; a re-read that
// returns no agent and no error makes the attempt unavailable.
func TestConduitRenewStream_ConfirmDenyNilAgentIsUnavailable(t *testing.T) {
	f := newRenewalFixture(t)
	tok, open := f.openGrant(t, grant.ViaProxy, tcpHeader("3000"), nil)
	f.srv.conduitAuthorizeActionTestHook = func(context.Context, Identity, *store.Agent, Action) error { return errConduitForbidden }
	_, err := f.renew(tok, open, 7)
	require.ErrorIs(t, err, errConduitForbidden, "a confirmed deny")

	_, fault := installStoreFault(t, f.srv, func(inner store.Store, sw *storeFaultSwitch) *countingNilAgentStore {
		return &countingNilAgentStore{Store: inner, fault: sw}
	})
	fault.Arm()
	_, err = f.renew(tok, open, 7)
	assert.ErrorIs(t, err, errConduitAuthzUnavailable)
	assert.Equal(t, ConduitRenewalUnavailable, ClassifyConduitRenewal(err))
}

// TestConduitConfirmAgentPresent (B4): only a present, undeleted agent
// confirms a deny.
func TestConduitConfirmAgentPresent(t *testing.T) {
	f := newConduitFixture(t)
	ctx := context.Background()
	assert.True(t, f.srv.conduitConfirmAgentPresent(ctx, f.agent.ID))
	assert.False(t, f.srv.conduitConfirmAgentPresent(ctx, tid("no-such-agent")))
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	assert.False(t, f.srv.conduitConfirmAgentPresent(cancelled, f.agent.ID))
	require.NoError(t, f.store.DeleteAgent(ctx, f.agent.ID))
	assert.False(t, f.srv.conduitConfirmAgentPresent(ctx, f.agent.ID))
}

// TestConduitRenewStream_DevUser (D5): a grant naming the development
// user is rebuilt as that user only while development authentication is
// enabled on the hub doing the re-check; otherwise the user id takes the
// normal path. Either way the account-status rule applies to the
// development user's row: a missing or suspended row is an inactive
// principal.
func TestConduitRenewStream_DevUser(t *testing.T) {
	f := newRenewalFixture(t)
	tok, open := f.openGrant(t, grant.ViaProxy, tcpHeader("3000"), func(c *grant.Claims) {
		c.Subject, c.JTI = grant.SubjectUserPrefix+DevUserID, "open-dev"
	})
	// The test server may seed a row for the development user; the store
	// reports it missing here.
	_, fault := installStoreFault(t, f.srv, func(inner store.Store, sw *storeFaultSwitch) *devUserMissingStore {
		return &devUserMissingStore{Store: inner, fault: sw}
	})
	fault.Arm()
	_, err := f.srv.store.GetUser(context.Background(), DevUserID)
	require.ErrorIs(t, err, store.ErrNotFound, "precondition: the development user has no row")

	prevEnabled, prevCfg := f.srv.authConfig.DevAuthEnabled, f.srv.authConfig.DevUserCfg
	t.Cleanup(func() { f.srv.authConfig.DevAuthEnabled, f.srv.authConfig.DevUserCfg = prevEnabled, prevCfg })
	f.srv.authConfig.DevAuthEnabled = false
	_, err = f.renew(tok, open, 7)
	assert.ErrorIs(t, err, errConduitPrincipalInactive, "dev auth off: the normal path")
	assert.Equal(t, ConduitRenewalDeny, ClassifyConduitRenewal(err))

	f.srv.authConfig.DevAuthEnabled = true
	f.srv.authConfig.DevUserCfg = DevUserConfig{Username: "dev"}
	_, err = f.renew(tok, open, 7)
	assert.ErrorIs(t, err, errConduitPrincipalInactive, "dev auth on, no row: the account-status rule refuses")
	assert.Equal(t, ConduitRenewalDeny, ClassifyConduitRenewal(err))

	t.Run("dev auth on with an active row for the development user", func(t *testing.T) {
		g := newRenewalFixture(t)
		ctx := context.Background()
		if _, err := g.store.GetUser(ctx, DevUserID); errors.Is(err, store.ErrNotFound) {
			createTestUserWithProjectRole(t, g.store, DevUserID, "dev@conduit.test", g.agent.ProjectID, store.ProjectRoleMember)
		}
		tok, open := g.openGrant(t, grant.ViaProxy, tcpHeader("3000"), func(c *grant.Claims) {
			c.Subject, c.JTI = grant.SubjectUserPrefix+DevUserID, "open-dev-active"
		})
		prevEnabled, prevCfg := g.srv.authConfig.DevAuthEnabled, g.srv.authConfig.DevUserCfg
		t.Cleanup(func() { g.srv.authConfig.DevAuthEnabled, g.srv.authConfig.DevUserCfg = prevEnabled, prevCfg })
		g.srv.authConfig.DevAuthEnabled = true
		g.srv.authConfig.DevUserCfg = DevUserConfig{Username: "dev"}
		var seen []Identity
		g.srv.conduitAuthorizeActionTestHook = func(_ context.Context, ident Identity, _ *store.Agent, _ Action) error {
			seen = append(seen, ident)
			return nil
		}
		r, err := g.renew(tok, open, 7)
		require.NoError(t, err, "dev auth on: the development user renews")
		assert.Equal(t, recheckRenewed, classOfRenewal(err))
		require.NotNil(t, r)
		assert.Equal(t, grant.SubjectUserPrefix+DevUserID, r.Claims.Subject)
		require.NotEmpty(t, seen)
		for _, ident := range seen {
			assert.IsType(t, &DevUser{}, ident, "evaluated as the development user")
		}

		row, err := g.store.GetUser(ctx, DevUserID)
		require.NoError(t, err)
		row.Status = store.UserStatusSuspended
		require.NoError(t, g.store.UpdateUser(ctx, row))
		_, err = g.renew(tok, open, 7)
		assert.ErrorIs(t, err, errConduitPrincipalInactive, "dev auth on, suspended row: the account-status rule refuses")
		assert.Equal(t, ConduitRenewalDeny, ClassifyConduitRenewal(err))
	})

	// A second fixture runs in its own subtest (the registry test database is
	// named after the test).
	t.Run("dev auth off with a row for the development user", func(t *testing.T) {
		g := newRenewalFixture(t)
		ctx := context.Background()
		if _, err := g.store.GetUser(ctx, DevUserID); errors.Is(err, store.ErrNotFound) {
			createTestUserWithProjectRole(t, g.store, DevUserID, "dev@conduit.test", g.agent.ProjectID, store.ProjectRoleMember)
		}
		row, err := g.store.GetUser(ctx, DevUserID)
		require.NoError(t, err, "precondition: the development user has a row")
		require.NotNil(t, row)
		tok, open := g.openGrant(t, grant.ViaProxy, tcpHeader("3000"), func(c *grant.Claims) {
			c.Subject, c.JTI = grant.SubjectUserPrefix+DevUserID, "open-dev-row"
		})
		prev := g.srv.authConfig.DevAuthEnabled
		t.Cleanup(func() { g.srv.authConfig.DevAuthEnabled = prev })
		g.srv.authConfig.DevAuthEnabled = false
		var seen []Identity
		g.srv.conduitAuthorizeActionTestHook = func(_ context.Context, ident Identity, _ *store.Agent, _ Action) error {
			seen = append(seen, ident)
			return nil
		}
		r, err := g.renew(tok, open, 7)
		require.NoError(t, err, "dev auth off with a row: the normal path renews")
		require.NotNil(t, r)
		require.NotEmpty(t, seen)
		for _, ident := range seen {
			assert.IsType(t, &AuthenticatedUser{}, ident, "evaluated as the stored user, not the development user")
		}
	})
}

// TestStartConduitExternal_RenewalPath (D3, S1): with target_stream_deadlines
// enforce, external relay mode refuses to start without the renewal path
// of relay-held streams and names the setting; with off it starts. A normal
// start installs the renewal re-check.
func TestStartConduitExternal_RenewalPath(t *testing.T) {
	newFixture := func(t *testing.T) *externalFixture {
		f := &externalFixture{conduitFixture: newConduitFixture(t)}
		f.regStore = entadapter.NewConduitRegistryStore(enttest.NewClient(t))
		f.reg = registry.New(f.regStore, registry.Config{})
		f.srv.config.SharedSigningSecret = testRelaySessionSecret
		return f
	}
	t.Run("enforce without the renewal path refuses", func(t *testing.T) {
		f := newFixture(t)
		f.srv.config.ConduitTargetStreamDeadlines = config.ConduitTargetStreamDeadlinesEnforce
		opts := externalOptions(t, f)
		opts.withoutStreamRenewer = true
		err := f.srv.StartConduitExternal(context.Background(), opts)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "server.hub.conduit.target_stream_deadlines")
		assert.Nil(t, f.srv.conduitExternal.Load(), "nothing started")
	})
	t.Run("off without the renewal path starts and warns", func(t *testing.T) {
		f := newFixture(t)
		logs := &syncBuffer{}
		prev := slog.Default()
		slog.SetDefault(slog.New(slog.NewTextHandler(logs, nil)))
		t.Cleanup(func() { slog.SetDefault(prev) })
		ctx, cancel := context.WithCancel(context.Background())
		t.Cleanup(cancel)
		opts := externalOptions(t, f)
		opts.withoutStreamRenewer = true
		require.NoError(t, f.srv.StartConduitExternal(ctx, opts))
		assert.Nil(t, f.srv.streamRenewer())
		out := logs.String()
		assert.Contains(t, out, "level=WARN")
		assert.Contains(t, out, "relay-held streams have no renewal path")
		assert.Contains(t, out, "target_stream_deadlines=off")
	})
	t.Run("enforce installs the renewal path", func(t *testing.T) {
		f := newFixture(t)
		ctx, cancel := context.WithCancel(context.Background())
		t.Cleanup(cancel)
		f.srv.config.ConduitTargetStreamDeadlines = config.ConduitTargetStreamDeadlinesEnforce
		require.NoError(t, f.srv.StartConduitExternal(ctx, externalOptions(t, f)))
		assert.NotNil(t, f.srv.streamRenewer())
	})
}

// TestRelayStreamRecheck_NoRenewalPathIsUnavailable: without the renewal
// path a relay stream re-check closes nothing and renews nothing.
func TestRelayStreamRecheck_NoRenewalPathIsUnavailable(t *testing.T) {
	f := newRelayRenewalFixture(t)
	tok, open := f.openGrant(t, grant.ViaProxy, tcpHeader("3000"), nil)
	f.srv.setConduitStreamRenewer(nil)
	res := f.relayRecheck(t, f.item(tok, open, 7))
	assert.False(t, res.Renew)
	assert.False(t, res.Close)
}

// productionFiles parses the non-test Go files of pkg/hub.
func productionFiles(t *testing.T) map[string]*ast.File {
	t.Helper()
	names, err := filepath.Glob("*.go")
	require.NoError(t, err)
	out := map[string]*ast.File{}
	for _, name := range names {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(token.NewFileSet(), name, nil, 0)
		require.NoError(t, err)
		out[name] = file
	}
	require.NotEmpty(t, out)
	return out
}

// TestConduitStreamRenewerInstalledOnlyAsTheRenewalReCheck (S5): production
// code installs the relay-held renewal path only as renewConduitStream.
func TestConduitStreamRenewerInstalledOnlyAsTheRenewalReCheck(t *testing.T) {
	calls := 0
	for name, file := range productionFiles(t) {
		ast.Inspect(file, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok || sel.Sel.Name != "setConduitStreamRenewer" {
				return true
			}
			calls++
			require.Len(t, call.Args, 1, name)
			arg, ok := call.Args[0].(*ast.SelectorExpr)
			if assert.True(t, ok, "%s installs something other than a method value", name) {
				assert.Equal(t, "renewConduitStream", arg.Sel.Name, name)
			}
			return true
		})
	}
	assert.Equal(t, 1, calls, "one production install site")
}

// TestConduitUserStreamTrackSitesSetProjectID (C2, STOP-level): every user
// stream a production site tracks records the project it was opened in,
// because the in-process re-check denies a stream whose agent is no longer
// in that project.
func TestConduitUserStreamTrackSitesSetProjectID(t *testing.T) {
	sites := 0
	for name, file := range productionFiles(t) {
		ast.Inspect(file, func(n ast.Node) bool {
			lit, ok := n.(*ast.CompositeLit)
			if !ok {
				return true
			}
			if id, ok := lit.Type.(*ast.Ident); !ok || id.Name != "conduitUserStream" {
				return true
			}
			keys := map[string]bool{}
			for _, e := range lit.Elts {
				if kv, ok := e.(*ast.KeyValueExpr); ok {
					if k, ok := kv.Key.(*ast.Ident); ok {
						keys[k.Name] = true
					}
				}
			}
			if !keys["AgentID"] {
				// Not a user stream to an agent (the broker session record).
				return true
			}
			sites++
			if !keys["ProjectID"] {
				t.Errorf("STOP: %s tracks a user stream without ProjectID", name)
			}
			return true
		})
	}
	assert.GreaterOrEqual(t, sites, 3, "the proxy, tunnel and PTY track sites")
}

// TestConduitRenewalUnsupportedPrincipal_OneDefinition (B12): one function
// decides renewal-unsupported principals; no other production function
// decides that class.
func TestConduitRenewalUnsupportedPrincipal_OneDefinition(t *testing.T) {
	var defs []string
	for name, file := range productionFiles(t) {
		for _, d := range file.Decls {
			fn, ok := d.(*ast.FuncDecl)
			if !ok {
				continue
			}
			lower := strings.ToLower(fn.Name.Name)
			if strings.Contains(lower, "renewal") && strings.Contains(lower, "unsupported") {
				defs = append(defs, name+":"+fn.Name.Name)
			}
		}
	}
	assert.Equal(t, []string{"conduit_authz.go:conduitRenewalUnsupportedPrincipal"}, defs)
}

// recheckNilUserStore returns neither a user nor an error from every user
// read while its switch is armed.
type recheckNilUserStore struct {
	store.Store
	fault *storeFaultSwitch
}

func (s *recheckNilUserStore) GetUser(ctx context.Context, id string) (*store.User, error) {
	if s.fault.Active() {
		return nil, nil
	}
	return s.Store.GetUser(ctx, id)
}

// TestConduitRenewalIdentity_NilUserIsUnavailable (r2 F1): a user read
// that returns neither a user nor an error is an internal inconsistency:
// unavailable, not an inactive principal and not an allow, and nothing is
// minted.
func TestConduitRenewalIdentity_NilUserIsUnavailable(t *testing.T) {
	f := newRenewalFixture(t)
	tok, open := f.openGrant(t, grant.ViaProxy, tcpHeader("3000"), nil)
	_, fault := installStoreFault(t, f.srv, func(inner store.Store, sw *storeFaultSwitch) *recheckNilUserStore {
		return &recheckNilUserStore{Store: inner, fault: sw}
	})
	fault.Arm()
	r, err := f.renew(tok, open, 7)
	assert.Nil(t, r, "nothing is minted")
	require.Error(t, err)
	assert.ErrorIs(t, err, errConduitAuthzUnavailable)
	assert.NotErrorIs(t, err, errConduitPrincipalInactive)
	assert.Equal(t, ConduitRenewalUnavailable, ClassifyConduitRenewal(err))
}

// TestRelayStreamRecheck_BadLatestGrantDenies (r2 F2): a relay-presented
// latest grant that fails re-check verification is a deny (4401): a
// tampered grant, and a grant bound to another epoch of the target session.
func TestRelayStreamRecheck_BadLatestGrantDenies(t *testing.T) {
	f := newRelayRenewalFixture(t)
	tok, open := f.openGrant(t, grant.ViaProxy, tcpHeader("3000"), nil)
	require.True(t, f.relayRecheck(t, f.item(tok, open, 7)).Renew, "the untouched grant renews")

	tampered := append([]byte(nil), tok...)
	// Flip a character inside the signature, away from its padding bits.
	sig := bytes.LastIndexByte(tampered, '.') + 5
	if tampered[sig] == 'A' {
		tampered[sig] = 'B'
	} else {
		tampered[sig] = 'A'
	}
	staleTok, staleOpen := f.openGrant(t, grant.ViaProxy, tcpHeader("3000"), func(c *grant.Claims) {
		c.Target.ConnectionEpoch++
		c.JTI = "open-relay-stale"
	})
	for _, tc := range []struct {
		name string
		it   relay.HubStreamRecheckItem
	}{
		{"tampered signature", f.item(tampered, open, 7)},
		{"another epoch of the target session", f.item(staleTok, staleOpen, 7)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			res := f.relayRecheck(t, tc.it)
			assert.False(t, res.Renew)
			assert.True(t, res.Close)
			assert.Equal(t, conduit.CloseUnauthenticated, res.CloseCode)
		})
	}
}

// devUserMissingStore reports the development user as missing (ErrNotFound)
// while its switch is armed, whatever rows the test server seeded.
type devUserMissingStore struct {
	store.Store
	fault *storeFaultSwitch
}

func (s *devUserMissingStore) GetUser(ctx context.Context, id string) (*store.User, error) {
	if id == DevUserID && s.fault.Active() {
		return nil, store.ErrNotFound
	}
	return s.Store.GetUser(ctx, id)
}

// TestEvaluateConduitStream_NilIdentityIsUnavailable: an evaluation without
// a principal resolver fails closed as unavailable. It is never an allow,
// so no entry point renews or keeps the stream on its account.
func TestEvaluateConduitStream_NilIdentityIsUnavailable(t *testing.T) {
	f := newRenewalFixture(t)
	verdict, reason, err := f.srv.evaluateConduitStream(context.Background(), conduitStreamEval{
		AgentID:   f.agent.ID,
		ProjectID: f.agent.ProjectID,
		Kind:      grant.StreamKindTCP,
		Port:      3000,
	})
	assert.Equal(t, conduitAuthzUnavailable, verdict)
	assert.NotEqual(t, conduitAuthzAllowed, verdict)
	assert.Equal(t, "principal lookup failed", reason)
	assert.ErrorIs(t, err, errConduitAuthzUnavailable)
	assert.Equal(t, ConduitRenewalUnavailable, ClassifyConduitRenewal(err))
}
