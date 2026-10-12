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

//go:build !no_sqlite && (!hubshard || hubshard_5)

package hub

import (
	"context"
	"net/http"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/conduit/grant"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// Re-checks of streams opened through the tunnel path evaluate the same
// actions as the tunnel open: the kind's action and ActionTunnel. Streams
// opened by the agent PTY and port-proxy paths keep their kind's action
// only.

// openTunnel tracks a stream opened by ident through the tunnel path and
// returns its watch. It checks that the tunnel open would be admitted now.
func (f *recheckFixture) openTunnel(t *testing.T, ident UserIdentity, kind string, id uint32) *streamWatch {
	t.Helper()
	port := 0
	params := map[string]string(nil)
	if kind == grant.StreamKindTCP {
		port = 3000
		params = map[string]string{grant.ParamHost: "127.0.0.1", grant.ParamPort: "3000"}
	}
	got, err := f.srv.authorizeConduitTunnel(context.Background(), ident, f.agent, kind, params)
	require.NoError(t, err, "the tunnel would not be opened")
	require.Equal(t, port, got)
	w := &streamWatch{closed: make(chan string, 1)}
	untrack := f.srv.trackConduitUserStream(&conduitUserStream{
		Kind: kind, Identity: ident, AgentID: f.agent.ID, ProjectID: f.agent.ProjectID,
		Port: port, ViaTunnel: true, SessionID: "sess-1", StreamID: id, Close: w.close,
	})
	t.Cleanup(untrack)
	return w
}

// trackedStreams returns the streams a tracks.
func trackedStreams(a *conduitStreamAuthz) []*conduitUserStream {
	a.mu.Lock()
	defer a.mu.Unlock()
	var out []*conduitUserStream
	for st := range a.streams {
		out = append(out, st)
	}
	return out
}

// TestConduitStreamActions pins the action set of every stream kind, for
// streams opened through the tunnel path and for the others.
func TestConduitStreamActions(t *testing.T) {
	for _, tc := range []struct {
		kind      string
		viaTunnel bool
		want      []Action
	}{
		{grant.StreamKindPTY, false, []Action{ActionAttach}},
		{grant.StreamKindPTY, true, []Action{ActionAttach, ActionTunnel}},
		{grant.StreamKindSSH, false, []Action{ActionAttach}},
		{grant.StreamKindSSH, true, []Action{ActionAttach, ActionTunnel}},
		{grant.StreamKindTCP, false, []Action{ActionPortAccess}},
		{grant.StreamKindTCP, true, []Action{ActionPortAccess, ActionTunnel}},
		{grant.StreamKindLogs, false, []Action{ActionRead}},
		{grant.StreamKindLogs, true, []Action{ActionRead, ActionTunnel}},
		{grant.StreamKindEvents, false, []Action{ActionRead}},
		{grant.StreamKindEvents, true, []Action{ActionRead, ActionTunnel}},
		{"unknown", false, nil},
		{"unknown", true, nil},
		{"", false, nil},
		{"", true, nil},
	} {
		t.Run(tc.kind+"/viaTunnel="+strconv.FormatBool(tc.viaTunnel), func(t *testing.T) {
			assert.Equal(t, tc.want, conduitStreamActions(tc.kind, tc.viaTunnel))
		})
	}
}

// tunnelRemovals are the removals of one required action of a tunnel
// stream. ActionTunnel is decided as agent.port_access, so for tcp the
// kind's action and ActionTunnel are one binding.
var tunnelRemovals = []struct {
	name    string
	kind    string
	binding func(f *recheckFixture, userID string) string
}{
	{"ssh/tunnel action removed", grant.StreamKindSSH, func(f *recheckFixture, id string) string { return f.portBind[id] }},
	{"ssh/kind action removed", grant.StreamKindSSH, func(f *recheckFixture, id string) string { return f.attachBind[id] }},
	{"tcp/kind and tunnel action removed", grant.StreamKindTCP, func(f *recheckFixture, id string) string { return f.portBind[id] }},
}

// TestConduitTunnelRecheck_ActionRemovedClosesOnNotify: removing either
// required action of a tunnel stream through the API closes it with 4401
// on the notify path; another user's tunnel stream stays open.
func TestConduitTunnelRecheck_ActionRemovedClosesOnNotify(t *testing.T) {
	for _, tc := range tunnelRemovals {
		t.Run(tc.name, func(t *testing.T) {
			f := newRecheckFixture(t, nil, -1)
			w := f.openTunnel(t, f.u1, tc.kind, 2)
			kept := f.openTunnel(t, f.u2, tc.kind, 4)

			f.deleteBinding(t, tc.binding(f, f.u1.ID()))

			assert.Equal(t, "4401 authz_expired", w.waitClosed(t))
			f.metrics.wait(t, "notify/closed/"+tc.kind, 1)
			kept.assertOpen(t)
			assert.Equal(t, 1, f.a.Len())
		})
	}
}

// TestConduitTunnelRecheck_ActionRemovedClosesOnSweep: a removal that
// publishes no event closes the tunnel stream with 4401 at the sweep, one
// re-check interval later on the fake clock.
func TestConduitTunnelRecheck_ActionRemovedClosesOnSweep(t *testing.T) {
	for _, tc := range tunnelRemovals {
		t.Run(tc.name, func(t *testing.T) {
			f := newRecheckFixture(t, nil, 60*time.Second)
			w := f.openTunnel(t, f.u1, tc.kind, 2)
			require.NoError(t, f.store.DeleteRoleBinding(context.Background(), tc.binding(f, f.u1.ID())))

			f.clk.Advance(59 * time.Second)
			w.assertOpen(t)
			f.clk.Advance(time.Second)
			assert.Equal(t, "4401 authz_expired", w.waitClosed(t))
			f.metrics.wait(t, "sweep/closed/"+tc.kind, 1)
		})
	}
}

// TestConduitTunnelRecheck_RenewalRefusedWithoutEitherAction: at the
// stream's authorization deadline, a tunnel stream missing either required
// action is closed with 4401 instead of renewed; a stream holding both is
// renewed.
func TestConduitTunnelRecheck_RenewalRefusedWithoutEitherAction(t *testing.T) {
	for _, tc := range tunnelRemovals {
		t.Run(tc.name, func(t *testing.T) {
			f := newRecheckFixture(t, nil, -1)
			w := f.openTunnel(t, f.u1, tc.kind, 2)
			kept := f.openTunnel(t, f.u2, tc.kind, 4)
			require.NoError(t, f.store.DeleteRoleBinding(context.Background(), tc.binding(f, f.u1.ID())))

			f.clk.Advance(f.a.cfg.UserStreamAuthzMax)
			assert.Equal(t, "4401 authz_expired", w.waitClosed(t))
			f.metrics.wait(t, "interval/closed/"+tc.kind, 1)
			f.metrics.wait(t, "interval/renewed/"+tc.kind, 1)
			kept.assertOpen(t)
		})
	}
}

// TestConduitTunnelRecheck_PTYAndPortProxyUnchanged pins the re-check of
// streams not opened through the tunnel path: a PTY stream needs attach
// only and a port-proxy stream needs port access only, on the notify path
// and at renewal.
func TestConduitTunnelRecheck_PTYAndPortProxyUnchanged(t *testing.T) {
	for _, tc := range []struct {
		name    string
		kind    string
		binding func(f *recheckFixture, userID string) string
	}{
		{"pty keeps without port access", grant.StreamKindPTY, func(f *recheckFixture, id string) string { return f.portBind[id] }},
		{"port proxy keeps without attach", grant.StreamKindTCP, func(f *recheckFixture, id string) string { return f.attachBind[id] }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newRecheckFixture(t, nil, -1)
			w := f.open(t, f.u1, tc.kind, 1)

			f.deleteBinding(t, tc.binding(f, f.u1.ID()))
			f.metrics.wait(t, "notify/passed/"+tc.kind, 1)
			w.assertOpen(t)

			f.clk.Advance(f.a.cfg.UserStreamAuthzMax)
			f.metrics.wait(t, "interval/renewed/"+tc.kind, 1)
			w.assertOpen(t)
			assert.Equal(t, 1, f.a.Len())
		})
	}
}

// TestConduitTunnelRecheck_PTYPathStreamNotMarked: the agent PTY path
// tracks its stream without the tunnel marker.
func TestConduitTunnelRecheck_PTYPathStreamNotMarked(t *testing.T) {
	f := newPTYConduitFixture(t)
	f.setBrokerRow(t, attachCaps(false), "")
	f.startPTYAgent(t, f.public.URL)
	a := f.srv.conduitAuthz.Load()
	require.NotNil(t, a)

	c, _, err := f.dialPTY(t, "")
	require.NoError(t, err)
	waitSpawn(t, f.spawned)
	echoRoundTrip(t, c, "tracked")
	streams := trackedStreams(a)
	require.Len(t, streams, 1)
	assert.Equal(t, grant.StreamKindPTY, streams[0].Kind)
	assert.False(t, streams[0].ViaTunnel)
}

// TestConduitTunnelRecheck_PortProxyStreamNotMarked: the port-proxy path
// tracks its stream without the tunnel marker.
func TestConduitTunnelRecheck_PortProxyStreamNotMarked(t *testing.T) {
	up := websocket.Upgrader{}
	f := newConduitProxyFixture(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !websocket.IsWebSocketUpgrade(r) {
			_, _ = w.Write([]byte("ok"))
			return
		}
		c, err := up.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer func() { _ = c.Close() }()
		for {
			mt, msg, err := c.ReadMessage()
			if err != nil {
				return
			}
			if err := c.WriteMessage(mt, msg); err != nil {
				return
			}
		}
	}))
	f.startAgent(t)
	a := f.srv.conduitAuthz.Load()
	require.NotNil(t, a)

	wsURL := "ws" + strings.TrimPrefix(f.base, "http") +
		"/api/v1/agents/" + f.launched.ID + "/ports/" + strconv.Itoa(f.app.port) + "/proxy/ws"
	c, _, err := websocket.DefaultDialer.Dial(wsURL, http.Header{"Authorization": {"Bearer " + f.userToken}})
	require.NoError(t, err)
	t.Cleanup(func() { _ = c.Close() })
	require.NoError(t, c.SetReadDeadline(time.Now().Add(10*time.Second)))
	require.NoError(t, c.WriteMessage(websocket.TextMessage, []byte("one")))
	_, got, err := c.ReadMessage()
	require.NoError(t, err)
	require.Equal(t, "one", string(got))

	streams := trackedStreams(a)
	require.Len(t, streams, 1)
	assert.Equal(t, grant.StreamKindTCP, streams[0].Kind)
	assert.False(t, streams[0].ViaTunnel)
}

// TestConduitTunnelRecheck_StoreFaultDefers: a store fault during a
// re-check of a tunnel stream records deferred_unavailable and never closes
// it early, even when a required action was in fact removed; once the
// store recovers the next check closes it.
func TestConduitTunnelRecheck_StoreFaultDefers(t *testing.T) {
	for _, tc := range []struct {
		name  string
		fault func(s *conduitFaultStore, on bool)
	}{
		{"agent read fails", func(s *conduitFaultStore, on bool) { s.failGetAgent.Store(on) }},
		{"user read fails", func(s *conduitFaultStore, on bool) { s.failGetUser.Store(on) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var fs *conduitFaultStore
			f := newRecheckFixture(t, nil, 60*time.Second, func(f *recheckFixture) {
				fs, _ = installStoreFault(t, f.srv, func(inner store.Store, _ *storeFaultSwitch) *conduitFaultStore {
					return &conduitFaultStore{Store: inner}
				})
			})
			w := f.openTunnel(t, f.u1, grant.StreamKindSSH, 2)
			require.NoError(t, f.store.DeleteRoleBinding(context.Background(), f.portBind[f.u1.ID()]))
			tc.fault(fs, true)

			f.srv.publishConduitAuthzChanged(conduitAuthzMatch{UserID: f.u1.ID()})
			f.metrics.wait(t, "notify/deferred_unavailable/ssh", 1)
			f.clk.Advance(60 * time.Second)
			f.metrics.wait(t, "sweep/deferred_unavailable/ssh", 1)
			w.assertOpen(t)
			assert.Equal(t, 1, f.a.Len())

			tc.fault(fs, false) // recovered
			f.clk.Advance(60 * time.Second)
			assert.Equal(t, "4401 authz_expired", w.waitClosed(t))
		})
	}
}

// bindingsFaultStore counts ListRoleBindingsForPrincipals calls and, while
// armed, fails every call after the first okCalls.
type bindingsFaultStore struct {
	store.Store
	armed   atomic.Bool
	okCalls atomic.Int32
	calls   atomic.Int32
}

func (s *bindingsFaultStore) ListRoleBindingsForPrincipals(ctx context.Context, principals []store.PrincipalRef, scopeTypes []string, scopeIDs []string) ([]*store.RoleBinding, error) {
	if n := s.calls.Add(1); s.armed.Load() && n > s.okCalls.Load() {
		return nil, errInjectedStoreFault
	}
	return s.Store.ListRoleBindingsForPrincipals(ctx, principals, scopeTypes, scopeIDs)
}

// TestConduitTunnelRecheck_TunnelActionFaultDefers: a fault met while the
// re-check of a tunnel stream evaluates ActionTunnel, the action after the
// kind's, is deferred_unavailable and never closes the stream: neither a
// deny whose store probe fails nor an authorization lookup that fails.
// Once the store recovers the next check closes it. u1 keeps attach (the
// kind's action of ssh) and loses port access (ActionTunnel), so the
// kind's action passes and ActionTunnel is the action evaluated when the
// fault is met.
func TestConduitTunnelRecheck_TunnelActionFaultDefers(t *testing.T) {
	ctx := context.Background()
	// requireTunnelActionDenies pins the premise: the kind's action is
	// allowed and ActionTunnel is denied by policy.
	requireTunnelActionDenies := func(t *testing.T, f *recheckFixture) {
		t.Helper()
		require.NoError(t, f.srv.authorizeConduitAction(ctx, f.u1, f.agent, ActionAttach))
		err := f.srv.authorizeConduitAction(ctx, f.u1, f.agent, ActionTunnel)
		require.ErrorIs(t, err, errConduitForbidden)
		require.NotErrorIs(t, err, errConduitAuthzUnavailable)
	}

	t.Run("deny probe fails", func(t *testing.T) {
		var fs *conduitFaultStore
		f := newRecheckFixture(t, nil, -1, func(f *recheckFixture) {
			fs, _ = installStoreFault(t, f.srv, func(inner store.Store, _ *storeFaultSwitch) *conduitFaultStore {
				return &conduitFaultStore{Store: inner}
			})
		})
		w := f.openTunnel(t, f.u1, grant.StreamKindSSH, 2)
		require.NoError(t, f.store.DeleteRoleBinding(ctx, f.portBind[f.u1.ID()]))
		requireTunnelActionDenies(t, f)
		// The check's agent read succeeds; the probe after the ActionTunnel
		// deny is the next agent read, and fails. The actions are decided
		// on the authorization service's store, which is not wrapped.
		fs.getAgentOKCalls.Store(1)
		fs.failGetAgent.Store(true)

		f.a.Recheck(ctx, conduitAuthzTriggerNotify, conduitAuthzMatch{})
		assert.Equal(t, []string{"notify/deferred_unavailable/ssh"}, f.metrics.list())
		assert.Equal(t, int32(-1), fs.getAgentOKCalls.Load(), "exactly the check's read and the probe")
		w.assertOpen(t)

		fs.failGetAgent.Store(false) // recovered
		f.a.Recheck(ctx, conduitAuthzTriggerNotify, conduitAuthzMatch{})
		assert.Equal(t, "4401 authz_expired", w.waitClosed(t))
	})

	t.Run("authz lookup fails", func(t *testing.T) {
		var bs *bindingsFaultStore
		f := newRecheckFixture(t, nil, -1, func(f *recheckFixture) {
			bs = &bindingsFaultStore{Store: f.srv.authzService.store}
			f.srv.authzService.store = bs
		})
		w := f.openTunnel(t, f.u1, grant.StreamKindSSH, 2)
		require.NoError(t, f.store.DeleteRoleBinding(ctx, f.portBind[f.u1.ID()]))
		requireTunnelActionDenies(t, f)
		// Count the binding lookups of the kind's action alone; the
		// lookups after them belong to ActionTunnel and fail.
		bs.calls.Store(0)
		require.NoError(t, f.srv.authorizeConduitAction(ctx, f.u1, f.agent, ActionAttach))
		kindCalls := bs.calls.Load()
		bs.okCalls.Store(kindCalls)
		bs.calls.Store(0)
		bs.armed.Store(true)

		f.a.Recheck(ctx, conduitAuthzTriggerNotify, conduitAuthzMatch{})
		assert.Equal(t, []string{"notify/deferred_unavailable/ssh"}, f.metrics.list())
		assert.Greater(t, bs.calls.Load(), kindCalls, "ActionTunnel looked up bindings after the kind's action")
		w.assertOpen(t)

		bs.armed.Store(false) // recovered
		f.a.Recheck(ctx, conduitAuthzTriggerNotify, conduitAuthzMatch{})
		assert.Equal(t, "4401 authz_expired", w.waitClosed(t))
	})
}
