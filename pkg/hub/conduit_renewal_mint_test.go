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
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/conduit/grant"
	"github.com/GoogleCloudPlatform/scion/pkg/conduit/registry"
	"github.com/GoogleCloudPlatform/scion/pkg/hub/permissions"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/GoogleCloudPlatform/scion/pkg/store/entadapter"
	"github.com/GoogleCloudPlatform/scion/pkg/store/enttest"
)

// Hub-side renewal of user stream deadlines from the stream's latest grant
// (design §3.5, Target-side deadlines, Re-check; criterion 29).

// TestConduitViaActions: the action set of every (origin, kind) pair in the
// origin table, and no actions for any other pair.
func TestConduitViaActions(t *testing.T) {
	for _, tc := range []struct {
		via, kind string
		want      []Action
	}{
		{grant.ViaTunnel, grant.StreamKindTCP, []Action{ActionPortAccess, ActionTunnel}},
		{grant.ViaTunnel, grant.StreamKindSSH, []Action{ActionAttach, ActionTunnel}},
		{grant.ViaTunnel, grant.StreamKindPTY, []Action{ActionAttach, ActionTunnel}},
		{grant.ViaProxy, grant.StreamKindTCP, []Action{ActionPortAccess}},
		{grant.ViaPTY, grant.StreamKindPTY, []Action{ActionAttach}},
	} {
		assert.ElementsMatch(t, tc.want, conduitViaActions(tc.via, tc.kind), "%s+%s", tc.via, tc.kind)
		assert.Len(t, conduitViaActions(tc.via, tc.kind), len(tc.want), "%s+%s", tc.via, tc.kind)
	}
	for _, p := range [][2]string{
		{grant.ViaProxy, grant.StreamKindSSH}, {grant.ViaProxy, grant.StreamKindPTY},
		{grant.ViaPTY, grant.StreamKindTCP}, {grant.ViaPTY, grant.StreamKindSSH},
		{grant.ViaTunnel, grant.StreamKindLogs}, {grant.ViaTunnel, grant.StreamKindEvents},
		{"", grant.StreamKindTCP}, {"elsewhere", grant.StreamKindPTY},
	} {
		assert.Nil(t, conduitViaActions(p[0], p[1]), "%s+%s", p[0], p[1])
	}
}

// renewalFixture is a hub with one live agent session in a real registry
// store and a user holding agent.attach and agent.port_access in the
// agent's project through separate role bindings.
type renewalFixture struct {
	*conduitFixture
	user       *AuthenticatedUser
	attachBind string
	portBind   string
	regTarget  grant.Target
	fault      *registry.FaultStore
}

const renewalSessionID = "sess-renew"

// newRenewalFixture builds the renewal fixture. Use one per test scope: a
// second one needs its own t.Run subtest, because the registry test database
// is named after the test.
func newRenewalFixture(t *testing.T) *renewalFixture {
	t.Helper()
	return newRenewalFixtureWithIncarnation(t, "inc-renew")
}

// newRenewalFixtureWithIncarnation is newRenewalFixture with the agent
// session's endpoint incarnation set to inc. Use one per test scope, as for
// newRenewalFixture.
func newRenewalFixtureWithIncarnation(t *testing.T, inc string) *renewalFixture {
	t.Helper()
	ctx := context.Background()
	f := &renewalFixture{conduitFixture: newConduitFixture(t)}

	attachRD, err := f.store.CreateRoleDefinition(ctx, &store.RoleDefinition{
		Name: "renew-attach", ScopeType: store.RoleScopeProject, Permissions: []string{"agent.attach"}})
	require.NoError(t, err)
	portRD, err := f.store.CreateRoleDefinition(ctx, &store.RoleDefinition{
		Name: "renew-port", ScopeType: store.RoleScopeProject, Permissions: []string{"agent.port_access"}})
	require.NoError(t, err)
	id := tid("renew-user")
	createTestUserWithProjectRole(t, f.store, id, "renew@conduit.test", f.agent.ProjectID, store.ProjectRoleMember)
	ensureHubMembership(ctx, f.store, id)
	for rd, into := range map[string]*string{attachRD.ID: &f.attachBind, portRD.ID: &f.portBind} {
		b, err := f.store.CreateRoleBinding(ctx, &store.RoleBinding{
			RoleDefinitionID: rd, PrincipalType: store.RoleBindingPrincipalUser, PrincipalID: id,
			ScopeType: store.RoleScopeProject, ScopeID: f.agent.ProjectID, CreatedBy: "test",
		})
		require.NoError(t, err)
		*into = b.ID
	}
	f.user = NewAuthenticatedUser(id, "renew@conduit.test", "Renew", store.UserRoleMember, "api")

	inner := entadapter.NewConduitRegistryStore(enttest.NewClient(t))
	now := time.Now()
	gen, err := inner.RegisterRelay(ctx, registry.RelayInstance{InstanceID: "hub-a", Generation: 1, StartedAt: now, LastSeen: now})
	require.NoError(t, err)
	epoch, err := inner.InsertSessionWithNextEpoch(ctx, registry.SessionRecord{
		SessionID: renewalSessionID, PrincipalKind: registry.PrincipalAgent, PrincipalID: f.agent.ID,
		ProjectID: f.agent.ProjectID, RelayInstanceID: "hub-a", RelayGeneration: gen, Transport: "ws",
		EndpointIncarnation: inc, ConnectedAt: now, LastSeen: now,
	})
	require.NoError(t, err)
	f.regTarget = grant.Target{
		Kind: grant.TargetKindAgent, ID: f.agent.ID, EndpointIncarnation: inc,
		SessionID: renewalSessionID, ConnectionEpoch: epoch,
	}
	f.fault = &registry.FaultStore{Inner: inner}
	f.srv.conduit.Store(&conduitRuntime{store: f.fault})
	t.Cleanup(func() { f.srv.conduit.Store(nil) })
	return f
}

// openGrant mints the stream's open grant as the hub does for a
// stream_renewal.v2 target: with authz_until, authz_interval and via.
func (f *renewalFixture) openGrant(t *testing.T, via string, h grant.StreamHeader, mut func(*grant.Claims)) ([]byte, grant.Claims) {
	t.Helper()
	signer, err := f.srv.conduitGrants.signer(context.Background())
	require.NoError(t, err)
	now := f.clock.Now().Truncate(time.Second)
	interval := f.srv.conduitUserStreamAuthzInterval()
	c := grant.Claims{
		Issuer: conduitGrantIssuer, Subject: grant.SubjectUserPrefix + f.user.ID(), ProjectID: f.agent.ProjectID,
		Target: f.regTarget, Stream: h, Via: via, JTI: "open-" + via + "-" + h.Kind,
		NotBefore: now, Expiry: now.Add(conduitGrantTTL), AuthzUntil: now.Add(interval), AuthzInterval: interval,
	}
	if mut != nil {
		mut(&c)
	}
	tok, err := grant.Mint(signer, c)
	require.NoError(t, err)
	return tok, c
}

// deleteBinding removes a role binding through the admin API.
func (f *renewalFixture) deleteBinding(t *testing.T, id string) {
	t.Helper()
	rec := doRequest(t, f.srv, http.MethodDelete, "/api/v1/admin/role-bindings/"+id, nil)
	require.Equal(t, http.StatusNoContent, rec.Code, rec.Body.String())
}

func (f *renewalFixture) renew(latest []byte, open grant.Claims, streamID uint32) (*conduitRenewal, error) {
	return f.srv.renewConduitStream(context.Background(), conduitRenewalRequest{
		LatestGrant: latest, TargetSessionID: renewalSessionID, TargetStreamID: streamID, OpenJTI: open.JTI,
	})
}

func renewalPTYHeader() grant.StreamHeader {
	return grant.StreamHeader{Kind: grant.StreamKindPTY, Params: map[string]string{grant.ParamSession: conduitPTYSession, grant.ParamCols: "80", grant.ParamRows: "24"}}
}

// targetAccepts plays the target: the renewal verifies against the hub's
// published keys and the admitted open grant, and moves the deadline.
func (f *renewalFixture) targetAccepts(t *testing.T, r *conduitRenewal, open grant.Claims, streamID uint32, after time.Time) {
	t.Helper()
	pubs, err := f.srv.ConduitGrantPublicKeys(context.Background())
	require.NoError(t, err)
	keys, err := grant.NewKeySet(pubs...)
	require.NoError(t, err)
	got, err := grant.VerifyRenewal(context.Background(), r.Token, keys, grant.RenewalExpectation{
		Target: open.Target, ProjectID: open.ProjectID, Subject: open.Subject, Stream: open.Stream, Via: open.Via,
		StreamID: streamID, OpenJTI: open.JTI, Issuer: conduitGrantIssuer,
		After: after, NotAfter: f.clock.Now().Add(open.AuthzInterval + 30*time.Second),
	}, grant.NewMemoryReplayCache(f.clock.Now, 0), f.clock.Now())
	require.NoError(t, err)
	assert.True(t, got.AuthzUntil.Equal(r.AuthzUntil))
}

// TestConduitRenewStream_RenewsFromAnyGrantInTheChain: the open grant and a
// renewal both renew; each renewal copies stream and via, binds the stream,
// and sets authz_until from mint time.
func TestConduitRenewStream_RenewsFromAnyGrantInTheChain(t *testing.T) {
	f := newRenewalFixture(t)
	tok, open := f.openGrant(t, grant.ViaProxy, tcpHeader("3000"), nil)

	f.clock.Advance(time.Hour)
	r1, err := f.renew(tok, open, 7)
	require.NoError(t, err)
	assert.Equal(t, f.clock.Now().Truncate(time.Second).Add(f.srv.conduitUserStreamAuthzInterval()), r1.AuthzUntil)
	assert.True(t, r1.Claims.Stream.Equal(open.Stream))
	assert.Equal(t, grant.ViaProxy, r1.Claims.Via)
	assert.Equal(t, uint32(7), r1.Claims.StreamID)
	assert.Equal(t, open.JTI, r1.Claims.OpenJTI)
	assert.Equal(t, f.regTarget, r1.Claims.Target)
	f.targetAccepts(t, r1, open, 7, open.AuthzUntil)

	f.clock.Advance(time.Hour)
	r2, err := f.renew(r1.Token, open, 7)
	require.NoError(t, err)
	assert.True(t, r2.Claims.Stream.Equal(open.Stream), "a renewal presented as the latest grant renews with the same stream")
	assert.True(t, r2.AuthzUntil.After(r1.AuthzUntil))
	f.targetAccepts(t, r2, open, 7, r1.AuthzUntil)
}

// TestConduitRenewStream_OriginDecidesActions: with agent.port_access
// removed and agent.attach kept, a PTY stream opened through the tunnel
// path gets no renewal and a PTY stream opened through hub PTY attach keeps
// renewing; TCP streams from either origin get none. With agent.attach
// removed instead, the tunnel PTY stream gets none either.
//
// ActionTunnel is decided as agent.port_access (conduitAuthzFor), so no
// permission set holds port access without ActionTunnel; tunnel and proxy
// TCP streams cannot diverge by permission today.
func TestConduitRenewStream_OriginDecidesActions(t *testing.T) {
	f := newRenewalFixture(t)
	tunnelPTY, tunnelPTYOpen := f.openGrant(t, grant.ViaTunnel, renewalPTYHeader(), nil)
	webPTY, webPTYOpen := f.openGrant(t, grant.ViaPTY, renewalPTYHeader(), nil)
	tunnelTCP, tunnelTCPOpen := f.openGrant(t, grant.ViaTunnel, tcpHeader("3000"), nil)
	proxyTCP, proxyTCPOpen := f.openGrant(t, grant.ViaProxy, tcpHeader("3000"), nil)
	for _, g := range []struct {
		tok  []byte
		open grant.Claims
	}{{tunnelPTY, tunnelPTYOpen}, {webPTY, webPTYOpen}, {tunnelTCP, tunnelTCPOpen}, {proxyTCP, proxyTCPOpen}} {
		_, err := f.renew(g.tok, g.open, 1)
		require.NoError(t, err, "%s+%s renews while both permissions are held", g.open.Via, g.open.Stream.Kind)
	}

	f.deleteBinding(t, f.portBind)
	_, err := f.renew(tunnelPTY, tunnelPTYOpen, 1)
	assert.ErrorIs(t, err, errConduitForbidden, "tunnel PTY without port access")
	_, err = f.renew(webPTY, webPTYOpen, 1)
	assert.NoError(t, err, "hub PTY attach keeps renewing with attach alone")
	_, err = f.renew(tunnelTCP, tunnelTCPOpen, 1)
	assert.ErrorIs(t, err, errConduitForbidden)
	_, err = f.renew(proxyTCP, proxyTCPOpen, 1)
	assert.ErrorIs(t, err, errConduitForbidden)

	// A second fixture runs in its own subtest: the registry test client is
	// named after the test, so two in one test would share one database.
	t.Run("attach removed", func(t *testing.T) {
		g := newRenewalFixture(t)
		tunnelPTY, tunnelPTYOpen := g.openGrant(t, grant.ViaTunnel, renewalPTYHeader(), nil)
		proxyTCP, proxyTCPOpen := g.openGrant(t, grant.ViaProxy, tcpHeader("3000"), nil)
		g.deleteBinding(t, g.attachBind)
		_, err := g.renew(tunnelPTY, tunnelPTYOpen, 1)
		assert.ErrorIs(t, err, errConduitForbidden, "tunnel PTY without attach")
		_, err = g.renew(proxyTCP, proxyTCPOpen, 1)
		assert.NoError(t, err, "a proxy TCP stream needs no attach")
	})
}

// TestConduitRenewStream_Refusals: requests the hub refuses without
// renewing.
func TestConduitRenewStream_Refusals(t *testing.T) {
	f := newRenewalFixture(t)
	tok, open := f.openGrant(t, grant.ViaProxy, tcpHeader("3000"), nil)
	ren, err := f.renew(tok, open, 7)
	require.NoError(t, err)
	agentTok, agentOpen := f.openGrant(t, grant.ViaProxy, tcpHeader("3000"), func(c *grant.Claims) {
		c.Subject, c.JTI = "agent:"+f.agent.ID, "open-agent"
	})
	staleTok, staleOpen := f.openGrant(t, grant.ViaProxy, tcpHeader("3000"), func(c *grant.Claims) {
		c.Target.ConnectionEpoch++
		c.JTI = "open-stale"
	})
	base := func() conduitRenewalRequest {
		return conduitRenewalRequest{LatestGrant: tok, TargetSessionID: renewalSessionID, TargetStreamID: 7, OpenJTI: open.JTI}
	}
	for _, tc := range []struct {
		name string
		mut  func(*conduitRenewalRequest)
		want error
	}{
		{"no grant", func(r *conduitRenewalRequest) { r.LatestGrant = nil }, errConduitRenewalUnavailable},
		{"not a grant", func(r *conduitRenewalRequest) { r.LatestGrant = []byte("abc.def.ghi") }, errConduitRenewalUnavailable},
		{"other open jti", func(r *conduitRenewalRequest) { r.OpenJTI = "open-other" }, errConduitRenewalUnavailable},
		{"renewal for another stream id", func(r *conduitRenewalRequest) { r.LatestGrant, r.TargetStreamID = ren.Token, 8 }, errConduitRenewalUnavailable},
		{"unknown target session", func(r *conduitRenewalRequest) { r.TargetSessionID = "sess-gone" }, errConduitRenewalUnavailable},
		{"grant for another epoch of the session", func(r *conduitRenewalRequest) { r.LatestGrant, r.OpenJTI = staleTok, staleOpen.JTI }, errConduitRenewalUnavailable},
		{"principal differs from the grant", func(r *conduitRenewalRequest) { r.PrincipalID = f.owner.ID() }, errConduitRenewalUnavailable},
		{"kind differs from the grant", func(r *conduitRenewalRequest) { r.Kind = grant.StreamKindPTY }, errConduitRenewalUnavailable},
		{"not a user stream", func(r *conduitRenewalRequest) { r.LatestGrant, r.OpenJTI = agentTok, agentOpen.JTI }, errConduitRenewalUnavailable},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := base()
			tc.mut(&r)
			got, err := f.srv.renewConduitStream(context.Background(), r)
			assert.ErrorIs(t, err, tc.want)
			assert.Nil(t, got)
		})
	}

	t.Run("matching principal and kind", func(t *testing.T) {
		r := base()
		r.PrincipalID, r.Kind = f.user.ID(), grant.StreamKindTCP
		_, err := f.srv.renewConduitStream(context.Background(), r)
		assert.NoError(t, err)
	})
	t.Run("past the renewal window", func(t *testing.T) {
		g := newRenewalFixture(t)
		tok, open := g.openGrant(t, grant.ViaProxy, tcpHeader("3000"), nil)
		g.clock.Advance(open.AuthzUntil.Sub(g.clock.Now()) + grant.RecheckAllowance)
		_, err := g.renew(tok, open, 7)
		require.NoError(t, err, "at authz_until + 30s")
		g.clock.Advance(time.Second)
		_, err = g.renew(tok, open, 7)
		assert.ErrorIs(t, err, errConduitRenewalUnavailable)
	})
	t.Run("experiment off", func(t *testing.T) {
		setConduitExperiment(t, f.srv, false)
		defer setConduitExperiment(t, f.srv, true)
		_, err := f.renew(tok, open, 7)
		assert.ErrorIs(t, err, errConduitDisabled)
	})
}

// TestConduitRenewStream_Keys: a grant signed by a rotated-out key still
// renews, and the renewal is signed by the current key; a grant signed by a
// revoked key gets no renewal.
func TestConduitRenewStream_Keys(t *testing.T) {
	f := newRenewalFixture(t)
	ctx := context.Background()
	tok, open := f.openGrant(t, grant.ViaProxy, tcpHeader("3000"), nil)
	first, err := f.srv.conduitGrants.signer(ctx)
	require.NoError(t, err)

	rot := rotateConduitGrantKeyAsAdmin(t, f.srv)
	f.clock.Advance(rot.ActivateAt.Sub(f.clock.Now()) + conduitGrantKeyRefresh)
	r, err := f.renew(tok, open, 7)
	require.NoError(t, err, "a grant from the rotated-out key renews")
	assert.Equal(t, rot.KeyID, r.Claims.KeyID)

	require.NoError(t, f.srv.RevokeConduitGrantKey(ctx, first.KeyID))
	_, err = f.renew(tok, open, 7)
	assert.ErrorIs(t, err, errConduitRenewalUnavailable, "a grant from a revoked key")
	_, err = f.renew(r.Token, open, 7)
	assert.NoError(t, err, "a renewal from the current key still renews")
}

// TestConduitRenewStream_StateChanges: an inactive user and a deleted agent
// get no renewal; a registry or store fault is unavailable, never a deny.
func TestConduitRenewStream_StateChanges(t *testing.T) {
	t.Run("user suspended", func(t *testing.T) {
		f := newRenewalFixture(t)
		tok, open := f.openGrant(t, grant.ViaProxy, tcpHeader("3000"), nil)
		adtSetUserStatus(t, f.store, f.user.ID(), store.UserStatusSuspended)
		_, err := f.renew(tok, open, 7)
		assert.ErrorIs(t, err, errConduitPrincipalInactive)
	})
	t.Run("agent deleted", func(t *testing.T) {
		f := newRenewalFixture(t)
		tok, open := f.openGrant(t, grant.ViaProxy, tcpHeader("3000"), nil)
		require.NoError(t, f.store.DeleteAgent(context.Background(), f.agent.ID))
		_, err := f.renew(tok, open, 7)
		assert.ErrorIs(t, err, errConduitAgentGone)
	})
	t.Run("port no longer exposed", func(t *testing.T) {
		f := newRenewalFixture(t)
		tok, open := f.openGrant(t, grant.ViaProxy, tcpHeader("3000"), nil)
		require.NoError(t, f.store.UpdateAgentExposedPorts(context.Background(), f.agent.ID, nil))
		_, err := f.renew(tok, open, 7)
		assert.ErrorIs(t, err, errConduitForbidden)
	})
	t.Run("registry fault", func(t *testing.T) {
		f := newRenewalFixture(t)
		tok, open := f.openGrant(t, grant.ViaProxy, tcpHeader("3000"), nil)
		f.fault.Fault = func(context.Context, string) error { return errors.New("registry down") }
		_, err := f.renew(tok, open, 7)
		assert.ErrorIs(t, err, errConduitAuthzUnavailable)
		assert.NotErrorIs(t, err, errConduitForbidden)
	})
	t.Run("no registry on this node", func(t *testing.T) {
		f := newRenewalFixture(t)
		tok, open := f.openGrant(t, grant.ViaProxy, tcpHeader("3000"), nil)
		f.srv.conduit.Store(nil)
		_, err := f.renew(tok, open, 7)
		assert.ErrorIs(t, err, errConduitAuthzUnavailable)
	})
}

// TestConduitRenewStream_TunnelTCPNeedsActionTunnel: with an authorizer
// that refuses ActionTunnel and decides every other action as the hub does,
// a TCP stream opened through the tunnel path gets no renewal and a TCP
// stream to the same port opened through the port proxy keeps renewing.
//
// The authorizer is installed through the Server's
// conduitAuthorizeActionTestHook field, which only tests set. Real roles cannot hold port access without ActionTunnel
// until ActionTunnel has a permission of its own (conduitAuthzFor decides it
// as agent.port_access).
func TestConduitRenewStream_TunnelTCPNeedsActionTunnel(t *testing.T) {
	f := newRenewalFixture(t)
	tunnelTok, tunnelOpen := f.openGrant(t, grant.ViaTunnel, tcpHeader("3000"), nil)
	proxyTok, proxyOpen := f.openGrant(t, grant.ViaProxy, tcpHeader("3000"), nil)

	_, err := f.renew(tunnelTok, tunnelOpen, 1)
	require.NoError(t, err, "with the hub's own authorizer both origins renew")

	f.srv.conduitAuthorizeActionTestHook = func(ctx context.Context, ident Identity, agent *store.Agent, action Action) error {
		if action == ActionTunnel {
			return errConduitForbidden
		}
		return f.srv.authorizeConduitAction(ctx, ident, agent, action)
	}
	_, err = f.renew(tunnelTok, tunnelOpen, 1)
	assert.ErrorIs(t, err, errConduitForbidden, "via=tunnel without ActionTunnel")
	_, err = f.renew(proxyTok, proxyOpen, 1)
	assert.NoError(t, err, "via=proxy needs only ActionPortAccess")
}

// TestConduitScopeFor: the scope claim of an identity is nil unless it is a
// scoped user token; for one it holds the token's scopes, boundary and
// ceiling as stored, a hub boundary staying hub.
func TestConduitScopeFor(t *testing.T) {
	f := newRenewalFixture(t)
	sc, err := conduitScopeFor(f.user)
	require.NoError(t, err)
	assert.Nil(t, sc, "a plain user identity has no scope")

	project := mustScope(t, NewScopedUserIdentity(f.user, f.agent.ProjectID, []string{store.UATScopeAgentPortAccess, store.UATScopeAgentPortAccess}))
	require.NotNil(t, project)
	assert.Equal(t, []string{store.UATScopeAgentPortAccess}, project.Scopes)
	assert.Equal(t, grant.ScopeBoundary{Kind: grant.ScopeBoundaryProject, ProjectID: f.agent.ProjectID}, project.Boundary)
	assert.Equal(t, int(permissions.CeilingVersionUnspecified), project.Ceiling.Version)

	ceiling := permissions.FrozenPermissionCeiling{Version: permissions.CeilingVersionV1, PermissionIDs: []string{"agent.port_access", "agent.attach"}}
	hub := mustScope(t, NewScopedUserIdentityWithBoundary(f.user, TokenBoundary{Kind: BoundaryKindHub}, []string{store.UATScopeAgentPortAccess}, "tok-1", ceiling))
	require.NotNil(t, hub)
	assert.Equal(t, grant.ScopeBoundary{Kind: grant.ScopeBoundaryHub}, hub.Boundary)
	assert.Equal(t, grant.ScopeCeiling{Version: int(permissions.CeilingVersionV1), PermissionIDs: []string{"agent.attach", "agent.port_access"}}, hub.Ceiling)

	rebuilt, ok := conduitScopedIdentity(f.user, hub).(*ScopedUserIdentity)
	require.True(t, ok)
	assert.Equal(t, BoundaryKindHub, rebuilt.Boundary().Kind, "a hub boundary is rebuilt as hub")
	assert.Equal(t, permissions.CeilingVersionV1, rebuilt.Ceiling().Version)
}

// TestConduitRenewStream_Scope: a stream opened with a scoped token renews
// only for what the token's scope permits, against the user's current
// permissions; each renewal copies the scope.
func TestConduitRenewStream_Scope(t *testing.T) {
	f := newRenewalFixture(t)
	portOnly := mustScope(t, NewScopedUserIdentity(f.user, f.agent.ProjectID, []string{store.UATScopeAgentPortAccess}))
	withScope := func(sc *grant.Scope, jti string) func(*grant.Claims) {
		return func(c *grant.Claims) { c.Scope, c.JTI = sc, jti }
	}

	tcpTok, tcpOpen := f.openGrant(t, grant.ViaProxy, tcpHeader("3000"), withScope(portOnly, "open-scoped-tcp"))
	r, err := f.renew(tcpTok, tcpOpen, 1)
	require.NoError(t, err, "a port-scoped proxy TCP stream renews")
	assert.True(t, grant.ScopeEqual(portOnly, r.Claims.Scope), "the renewal copies the scope")
	f.targetAcceptsScoped(t, r, tcpOpen, 1)
	_, err = f.renew(r.Token, tcpOpen, 1)
	require.NoError(t, err, "the renewal renews in turn")

	ptyTok, ptyOpen := f.openGrant(t, grant.ViaPTY, renewalPTYHeader(), withScope(portOnly, "open-scoped-pty"))
	_, err = f.renew(ptyTok, ptyOpen, 1)
	assert.ErrorIs(t, err, errConduitForbidden, "the user holds attach, the token's scope does not")
	plainTok, plainOpen := f.openGrant(t, grant.ViaPTY, renewalPTYHeader(), nil)
	_, err = f.renew(plainTok, plainOpen, 1)
	assert.NoError(t, err, "the same user's unscoped PTY stream renews")
	assert.Nil(t, plainOpen.Scope)

	otherProject := mustScope(t, NewScopedUserIdentity(f.user, tid("another-project"), []string{store.UATScopeAgentPortAccess}))
	otherTok, otherOpen := f.openGrant(t, grant.ViaProxy, tcpHeader("3000"), withScope(otherProject, "open-other-project"))
	_, err = f.renew(otherTok, otherOpen, 1)
	assert.ErrorIs(t, err, errConduitForbidden, "a project-boundary scope for another project")

	hubScope := mustScope(t, NewScopedUserIdentityWithBoundary(f.user, TokenBoundary{Kind: BoundaryKindHub}, []string{store.UATScopeAgentPortAccess}, "",
		permissions.FrozenPermissionCeiling{Version: permissions.CeilingVersionV1, PermissionIDs: []string{"agent.port_access"}}))
	hubTok, hubOpen := f.openGrant(t, grant.ViaProxy, tcpHeader("3000"), withScope(hubScope, "open-hub-scope"))
	_, err = f.renew(hubTok, hubOpen, 1)
	assert.NoError(t, err, "a hub-boundary scope renews while the user has the project access")

	f.deleteBinding(t, f.portBind)
	_, err = f.renew(tcpTok, tcpOpen, 1)
	assert.ErrorIs(t, err, errConduitForbidden, "the port-scoped stream stops renewing once the user loses port access")
}

// mustScope is conduitScopeFor for an identity that must have a scope.
func mustScope(t *testing.T, ident Identity) *grant.Scope {
	t.Helper()
	sc, err := conduitScopeFor(ident)
	require.NoError(t, err)
	require.NotNil(t, sc)
	return sc
}

// wrappedScoped embeds a scoped identity, so its accessors are promoted.
type wrappedScoped struct{ *ScopedUserIdentity }

// opaqueIdentity wraps an identity behind the Identity interface alone.
type opaqueIdentity struct{ Identity }

// TestConduitScopeForFailsClosed: an identity is unscoped only when it is a
// plain user identity. A wrapper exposing the scoped accessors is scoped
// exactly like the identity it wraps; a wrapper hiding them, a non-user
// identity, a scoped context that is not a user, and a scoped context that
// is not a valid scope are refused, never treated as unscoped.
func TestConduitScopeForFailsClosed(t *testing.T) {
	f := newRenewalFixture(t)
	scoped := NewScopedUserIdentity(f.user, f.agent.ProjectID, []string{store.UATScopeAgentPortAccess})

	got, err := conduitScopeFor(wrappedScoped{scoped})
	require.NoError(t, err)
	assert.True(t, grant.ScopeEqual(mustScope(t, scoped), got), "a wrapper exposing the accessors is scoped like the wrapped identity")

	for name, ident := range map[string]Identity{
		"wrapper hiding a scoped identity": opaqueIdentity{scoped},
		"wrapper hiding a plain user":      opaqueIdentity{f.user},
		"agent identity":                   newAgentIdentityFromStore(f.agent),
		"delegated agent identity (boundary and ceiling, no scopes)": newDelegatedAgentIdentity(
			&store.AgentDelegatedCredential{ID: "cred-1", AgentID: f.agent.ID},
			&store.AgentDelegationGrant{ID: "grant-1", AgentID: f.agent.ID, AgentProjectID: f.agent.ProjectID}),
		"scoped user with no scopes": NewScopedUserIdentity(f.user, f.agent.ProjectID, nil),
		"scoped user with an invalid boundary": NewScopedUserIdentityWithBoundary(f.user, TokenBoundary{Kind: BoundaryKindProject}, []string{store.UATScopeAgentPortAccess}, "",
			permissions.FrozenPermissionCeiling{Version: permissions.CeilingVersionV1, PermissionIDs: []string{"agent.port_access"}}),
		"no identity": nil,
	} {
		t.Run(name, func(t *testing.T) {
			sc, err := conduitScopeFor(ident)
			assert.ErrorIs(t, err, errConduitScope)
			assert.Nil(t, sc)
		})
	}
}

// targetAcceptsScoped is targetAccepts for a stream whose open grant carries
// a scope.
func (f *renewalFixture) targetAcceptsScoped(t *testing.T, r *conduitRenewal, open grant.Claims, streamID uint32) {
	t.Helper()
	pubs, err := f.srv.ConduitGrantPublicKeys(context.Background())
	require.NoError(t, err)
	keys, err := grant.NewKeySet(pubs...)
	require.NoError(t, err)
	_, err = grant.VerifyRenewal(context.Background(), r.Token, keys, grant.RenewalExpectation{
		Target: open.Target, ProjectID: open.ProjectID, Subject: open.Subject, Stream: open.Stream, Via: open.Via, Scope: open.Scope,
		StreamID: streamID, OpenJTI: open.JTI, Issuer: conduitGrantIssuer,
		After: open.AuthzUntil.Add(-time.Second), NotAfter: f.clock.Now().Add(open.AuthzInterval + 30*time.Second),
	}, grant.NewMemoryReplayCache(f.clock.Now, 0), f.clock.Now())
	require.NoError(t, err)
}

// TestConduitGrantLargestScopeFits: a grant and its renewal for a token
// holding every scope a new token may hold (permissions.UATValidScopes),
// with the ceiling those scopes resolve to at mint
// (permissions.BuildCeilingFromSelectors), fit the grant size limit.
func TestConduitGrantLargestScopeFits(t *testing.T) {
	f := newRenewalFixture(t)
	var scopes []string
	for s := range permissions.UATValidScopes() {
		scopes = append(scopes, s)
	}
	ceiling, ok := permissions.BuildCeilingFromSelectors(scopes)
	require.True(t, ok)
	largest := mustScope(t, NewScopedUserIdentityWithBoundary(f.user,
		TokenBoundary{Kind: BoundaryKindProject, ProjectID: f.agent.ProjectID}, scopes, "", ceiling))
	tok, open := f.openGrant(t, grant.ViaProxy, tcpHeader("3000"), func(c *grant.Claims) { c.Scope, c.JTI = largest, "open-largest" })
	t.Logf("open grant with %d scopes and %d permission ids: %d bytes", len(largest.Scopes), len(largest.Ceiling.PermissionIDs), len(tok))
	assert.Less(t, len(tok), 16<<10)
	r, err := f.renew(tok, open, 1)
	require.NoError(t, err)
	assert.Less(t, len(r.Token), 16<<10)
}

// TestConduitRenewStream_OversizeRenewalRefused: when the renewal of an
// open grant that fits the grant size limit would not fit, no renewal is
// minted and the refusal is unavailable (wrapping grant.ErrTooLarge), never
// a renewal without part of its claims.
func TestConduitRenewStream_OversizeRenewalRefused(t *testing.T) {
	probe := newRenewalFixture(t)
	ctx := context.Background()
	signer, err := probe.srv.conduitGrants.signer(ctx)
	require.NoError(t, err)
	_, sample := probe.openGrant(t, grant.ViaProxy, tcpHeader("3000"), nil)
	sizes := func(inc string) (openFits, renewalFits bool) {
		c := sample
		c.Target.EndpointIncarnation = inc
		_, err := grant.Mint(signer, c)
		openFits = err == nil
		jti, err := grant.NewJTI()
		require.NoError(t, err)
		now := probe.clock.Now().Truncate(time.Second)
		_, err = grant.MintRenewal(signer, grant.RenewalClaims{
			Issuer: c.Issuer, Subject: c.Subject, ProjectID: c.ProjectID, Target: c.Target, Stream: c.Stream, Via: c.Via,
			StreamID: 7, OpenJTI: c.JTI, JTI: jti,
			AuthzUntil: now.Add(probe.srv.conduitUserStreamAuthzInterval()), NotBefore: now, Expiry: now.Add(conduitGrantTTL),
		})
		return openFits, err == nil
	}
	lo, hi := 1, 16<<10 // the largest incarnation length whose open grant fits
	for lo < hi {
		mid := (lo + hi + 1) / 2
		if ok, _ := sizes(strings.Repeat("i", mid)); ok {
			lo = mid
		} else {
			hi = mid - 1
		}
	}
	inc := strings.Repeat("i", lo)
	openFits, renewalFits := sizes(inc)
	require.True(t, openFits)
	require.False(t, renewalFits, "a renewal is never larger than its open grant here; the test cannot build the case")

	// The renewal fixture runs in its own subtest: the registry test client
	// is named after the test, so a second fixture in this test would share
	// the probe's database.
	t.Run("renewal", func(t *testing.T) {
		f := newRenewalFixtureWithIncarnation(t, inc)
		tok, open := f.openGrant(t, grant.ViaProxy, tcpHeader("3000"), nil)
		r, err := f.renew(tok, open, 7)
		assert.Nil(t, r)
		assert.ErrorIs(t, err, errConduitAuthzUnavailable)
		assert.ErrorIs(t, err, grant.ErrTooLarge)
	})
}
