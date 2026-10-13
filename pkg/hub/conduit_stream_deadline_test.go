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
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/gorilla/websocket"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/conduit"
	"github.com/GoogleCloudPlatform/scion/pkg/conduit/clock"
	"github.com/GoogleCloudPlatform/scion/pkg/conduit/transport/ws"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	conduitv1 "github.com/GoogleCloudPlatform/scion/proto/conduit/v1"
)

// Hub-level tests of the authorization deadline of a port-proxy stream:
// a WebSocket proxied over a conduit TCP stream to a real sciontool
// target. The deadline runs on the relay's fake clock.

// testStreamAuthzMax is the user stream interval of these tests: short
// enough that reaching it fires no keepalive on the relay's clock.
const testStreamAuthzMax = time.Second

// deadlineProxyFixture is a port-proxy fixture whose user streams are
// bounded by testStreamAuthzMax.
type deadlineProxyFixture struct {
	*conduitProxyFixture
	authz *conduitStreamAuthz
	clk   *clock.Fake
}

func newDeadlineProxyFixture(t *testing.T, app http.Handler) *deadlineProxyFixture {
	t.Helper()
	f := &deadlineProxyFixture{conduitProxyFixture: newConduitProxyFixture(t, app)}
	f.startAgent(t)
	f.authz = f.srv.conduitAuthz.Load()
	require.NotNil(t, f.authz, "the re-check runs with the relay")
	var ok bool
	f.clk, ok = f.authz.cfg.Clock.(*clock.Fake)
	require.True(t, ok, "the relay fixture runs on a fake clock")
	// No stream is tracked yet; streams tracked from now on take this
	// interval.
	require.Zero(t, f.authz.Len())
	f.authz.cfg.UserStreamAuthzMax = testStreamAuthzMax
	return f
}

// trackedStream returns the only tracked stream.
func (f *deadlineProxyFixture) trackedStream(t *testing.T) *conduitUserStream {
	t.Helper()
	f.authz.mu.Lock()
	defer f.authz.mu.Unlock()
	require.Len(t, f.authz.streams, 1)
	for st := range f.authz.streams {
		return st
	}
	return nil
}

// streamCalls wraps a tracked stream's Close and Renew to record calls.
// It must run before the stream is next checked.
type streamCalls struct {
	mu     sync.Mutex
	closes []uint32
	renews []error
}

func watchStream(st *conduitUserStream) *streamCalls {
	c := &streamCalls{}
	closeFn, renewFn := st.Close, st.Renew
	st.Close = func(code uint32, reason string) {
		c.mu.Lock()
		c.closes = append(c.closes, code)
		c.mu.Unlock()
		closeFn(code, reason)
	}
	if renewFn != nil {
		st.Renew = func() error {
			err := renewFn()
			c.mu.Lock()
			c.renews = append(c.renews, err)
			c.mu.Unlock()
			return err
		}
	}
	return c
}

func (c *streamCalls) snapshot() (closes []uint32, renews []error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]uint32(nil), c.closes...), append([]error(nil), c.renews...)
}

// echo checks one more round trip on c.
func echo(t *testing.T, c *websocket.Conn, msg string) {
	t.Helper()
	require.NoError(t, c.SetReadDeadline(time.Now().Add(recheckWait)))
	require.NoError(t, c.WriteMessage(websocket.TextMessage, []byte(msg)))
	_, got, err := c.ReadMessage()
	require.NoError(t, err)
	require.Equal(t, msg, string(got))
}

// TestConduitProxyIntervalRenewsStream (C16b renew, and the target
// accepting the renewal): at its deadline a stream whose user still holds
// port access is renewed. The hub sends AuthRefresh{stream_id} on the
// agent's session (held by this node), the deadline moves forward by
// exactly one interval, and the leaf WebSocket, the stream and the
// agent's session all stay open and keep carrying data.
func TestConduitProxyIntervalRenewsStream(t *testing.T) {
	f := newDeadlineProxyFixture(t, echoApp())
	c := proxyWS(t, f.conduitProxyFixture)
	st := f.trackedStream(t)
	require.NotNil(t, st.Renew, "the agent's session is local: a notice sender is set")
	calls := watchStream(st)
	d0 := st.Deadline()
	require.Equal(t, st.Admitted.Add(testStreamAuthzMax), d0)

	f.clk.Advance(testStreamAuthzMax)
	assert.Equal(t, d0.Add(testStreamAuthzMax), st.Deadline(), "the deadline moved by exactly one interval")
	closes, renews := calls.snapshot()
	assert.Empty(t, closes)
	require.Len(t, renews, 1, "one renewal notice")
	assert.NoError(t, renews[0], "the notice was sent on the agent's session")

	// The notice is queued as a control frame ahead of this data, so the
	// echo proves the target processed it and kept the stream open.
	echo(t, c, "after-renewal")
	assert.Equal(t, 1, f.authz.Len())
	assert.EqualValues(t, 1, f.sessions.Load(), "the agent session was closed or re-established")

	// The next deadline renews again.
	f.clk.Advance(testStreamAuthzMax)
	assert.Equal(t, d0.Add(2*testStreamAuthzMax), st.Deadline())
	echo(t, c, "after-second-renewal")
}

// TestConduitProxyIntervalClosesRevokedStream (C16b close): the user lost
// port access (without any revocation event) and the stream reaches its
// deadline. The leaf WebSocket receives 4401 authz_expired, the agent-side
// socket is closed, the stream is closed once and untracked, and the
// agent's session carries on.
func TestConduitProxyIntervalClosesRevokedStream(t *testing.T) {
	up := websocket.Upgrader{}
	targetClosed := make(chan struct{})
	f := newDeadlineProxyFixture(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cn, err := up.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer func() { _ = cn.Close() }()
		for {
			mt, msg, err := cn.ReadMessage()
			if err != nil {
				close(targetClosed)
				return
			}
			if err := cn.WriteMessage(mt, msg); err != nil {
				return
			}
		}
	}))
	c := proxyWS(t, f.conduitProxyFixture)
	st := f.trackedStream(t)
	calls := watchStream(st)

	// Revoke in the store only: no event is published, so only the
	// deadline can catch it here.
	ctx := context.Background()
	bindings, err := f.store.ListRoleBindingsForPrincipal(ctx, store.RoleBindingPrincipalUser, f.launched.OwnerID)
	require.NoError(t, err)
	removed := 0
	for _, b := range bindings {
		if b.ScopeType == store.RoleScopeProject && b.ScopeID == f.launched.ProjectID {
			require.NoError(t, f.store.DeleteRoleBinding(ctx, b.ID))
			removed++
		}
	}
	require.NotZero(t, removed)

	f.clk.Advance(testStreamAuthzMax)
	requireAuthzClose(t, c)
	select {
	case <-targetClosed:
	case <-time.After(recheckWait):
		t.Fatal("the agent-side socket was not closed")
	}
	closes, renews := calls.snapshot()
	assert.Equal(t, []uint32{conduit.CloseUnauthenticated}, closes, "the stream was closed exactly once, with 4401")
	assert.Empty(t, renews)
	assert.Zero(t, f.authz.Len())
	f.clk.Advance(testStreamAuthzMax)
	closes, _ = calls.snapshot()
	assert.Len(t, closes, 1, "closed again after its deadline")
	assert.EqualValues(t, 1, f.sessions.Load(), "the agent session was closed or re-established")
}

// TestConduitProxyDialerRenewalChangesNoDeadline: a dialer-sent
// AuthRefresh{stream_id}, even one naming the tracked stream's id, is
// refused with 4400 bad_frame on the sender's session only, and changes
// no deadline. The tracked stream stays open with no renewal notice.
func TestConduitProxyDialerRenewalChangesNoDeadline(t *testing.T) {
	f := newDeadlineProxyFixture(t, echoApp())
	c := proxyWS(t, f.conduitProxyFixture)
	st := f.trackedStream(t)
	calls := watchStream(st)
	d0 := st.Deadline()

	// A second agent's conduit session plays the misbehaving dialer.
	ctx := context.Background()
	other := &store.Agent{
		ID: tid("conduit-dialer"), Slug: "conduit-dialer", Name: "Dialer",
		ProjectID: f.launched.ProjectID, OwnerID: f.launched.OwnerID, Ancestry: f.launched.Ancestry,
		RuntimeBrokerID: "broker-1", Phase: string(state.PhaseCreated),
	}
	require.NoError(t, f.store.CreateAgent(ctx, other))
	runID := uuid.NewString()
	_, err := f.store.SetAgentRunID(ctx, other.ID, runID, nil)
	require.NoError(t, err)
	tok, err := f.srv.GenerateAgentToken(other.ID, other.ProjectID, nil, AgentRoleFull, nil)
	require.NoError(t, err)
	dctx, cancel := context.WithTimeout(ctx, recheckWait)
	defer cancel()
	d := &ws.Dialer{
		URL: "ws" + strings.TrimPrefix(f.public.URL, "http") + "/api/v1/conduit",
		Header: func(context.Context) (http.Header, error) {
			return http.Header{"X-Scion-Agent-Token": {tok}}, nil
		},
	}
	s, _, err := conduit.Dial(dctx, d, conduit.Config{Clock: clock.Real()}, &conduitv1.Hello{
		PrincipalKind: conduitv1.PrincipalKind_PRINCIPAL_KIND_AGENT,
		PrincipalId:   other.ID,
		Capabilities:  &conduitv1.Capabilities{StreamKinds: []string{"pty"}, EndpointIncarnation: runID},
	})
	require.NoError(t, err)
	ls := s.(conduit.LocalSession)
	t.Cleanup(func() { _ = ls.Close() })

	require.NoError(t, ls.RefreshAuth([]byte("credential"), st.StreamID))
	select {
	case <-ls.Done():
	case <-time.After(recheckWait):
		t.Fatal("the dialer's session was not closed")
	}
	assert.EqualValues(t, conduit.CloseProtocolError, conduit.CodeOf(ls.Err(), 0), "want 4400 bad_frame, got %v", ls.Err())

	assert.Equal(t, d0, st.Deadline(), "a dialer-sent renewal moved the deadline")
	closes, renews := calls.snapshot()
	assert.Empty(t, closes)
	assert.Empty(t, renews)
	echo(t, c, "still-open")
	assert.Equal(t, 1, f.authz.Len())
}

// TestConduitStreamAuthzMaxWiring: the hub's re-check takes the user
// stream interval from ServerConfig.ConduitUserStreamAuthzMax (8h when
// unset), and a tracked user stream's deadline uses it.
func TestConduitStreamAuthzMaxWiring(t *testing.T) {
	for name, tc := range map[string]struct {
		configured, want time.Duration
	}{"unset": {0, 8 * time.Hour}, "configured": {90 * time.Minute, 90 * time.Minute}} {
		t.Run(name, func(t *testing.T) {
			f := newConduitFixture(t)
			f.srv.config.ConduitUserStreamAuthzMax = tc.configured
			clk := clock.NewFake(time.Date(2026, 10, 8, 1, 0, 0, 0, time.UTC))
			a, stop, err := f.srv.startConduitStreamAuthz(context.Background(), clk, -1)
			require.NoError(t, err)
			t.Cleanup(stop)
			assert.Equal(t, tc.want, a.cfg.UserStreamAuthzMax)

			ident := NewAuthenticatedUser("u1", "u1@conduit.test", "u1", store.UserRoleMember, "api")
			st := &conduitUserStream{Kind: "pty", Identity: ident, AgentID: f.agent.ID, ProjectID: f.agent.ProjectID, Close: func(uint32, string) {}}
			untrack := f.srv.trackConduitUserStream(st)
			t.Cleanup(untrack)
			assert.Equal(t, clk.Now().Add(tc.want), st.Deadline())
		})
	}
}
