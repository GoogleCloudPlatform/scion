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
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/conduit/clock"
	"github.com/GoogleCloudPlatform/scion/pkg/conduit/grant"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// Hub-level tests of the user-stream re-check: the real authorization
// check against the store, the revocation events published by the hub's
// mutation handlers, and the LISTEN resync.

const recheckWait = 10 * time.Second

// streamWatch observes one tracked stream: its close and the re-checks
// recorded for it.
type streamWatch struct {
	closed chan string // "<code> <reason>", once
	once   sync.Once
}

func (w *streamWatch) close(code uint32, reason string) {
	w.once.Do(func() { w.closed <- strconv.FormatUint(uint64(code), 10) + " " + reason })
}

// waitClosed returns the stream's close, failing after recheckWait.
func (w *streamWatch) waitClosed(t *testing.T) string {
	t.Helper()
	select {
	case c := <-w.closed:
		return c
	case <-time.After(recheckWait):
		t.Fatal("stream was not closed")
		return ""
	}
}

// assertOpen fails if the stream was closed.
func (w *streamWatch) assertOpen(t *testing.T) {
	t.Helper()
	select {
	case c := <-w.closed:
		t.Fatalf("stream closed: %s", c)
	default:
	}
}

// metricWaiter records re-check metrics and lets a test wait for one.
type metricWaiter struct {
	mu   sync.Mutex
	seen []string
	cond chan struct{}
}

func newMetricWaiter() *metricWaiter { return &metricWaiter{cond: make(chan struct{})} }

func (m *metricWaiter) RecordConduitStreamAuthz(trigger, outcome, kind string) {
	m.mu.Lock()
	m.seen = append(m.seen, trigger+"/"+outcome+"/"+kind)
	close(m.cond)
	m.cond = make(chan struct{})
	m.mu.Unlock()
}

// wait blocks until want has been recorded n times in total.
func (m *metricWaiter) wait(t *testing.T, want string, n int) {
	t.Helper()
	deadline := time.After(recheckWait)
	for {
		m.mu.Lock()
		count := 0
		for _, s := range m.seen {
			if s == want {
				count++
			}
		}
		ch := m.cond
		m.mu.Unlock()
		if count >= n {
			return
		}
		select {
		case <-ch:
		case <-deadline:
			t.Fatalf("metric %q not recorded %d times; seen %v", want, n, m.list())
		}
	}
}

func (m *metricWaiter) list() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]string(nil), m.seen...)
}

// recheckFixture is a hub with the re-check running on a fake clock, the
// in-process event publisher, and two users who may attach to and reach
// the ports of the fixture's agent through custom project roles.
type recheckFixture struct {
	*conduitFixture
	clk     *clock.Fake
	metrics *metricWaiter
	a       *conduitStreamAuthz
	// u1, u2 hold agent.attach and agent.port_access in the project
	// through separate role bindings.
	u1, u2       *AuthenticatedUser
	attachBind   map[string]string // user id → attach role binding id
	portBind     map[string]string // user id → port role binding id
	attachRoleID string
	portRoleID   string
}

func newRecheckFixture(t *testing.T, events EventPublisher, interval time.Duration) *recheckFixture {
	t.Helper()
	f := &recheckFixture{
		conduitFixture: newConduitFixture(t),
		clk:            clock.NewFake(time.Date(2026, 10, 8, 1, 0, 0, 0, time.UTC)),
		metrics:        newMetricWaiter(),
		attachBind:     map[string]string{},
		portBind:       map[string]string{},
	}
	ctx := context.Background()
	attachRD, err := f.store.CreateRoleDefinition(ctx, &store.RoleDefinition{
		Name: "conduit-attach", ScopeType: store.RoleScopeProject, Permissions: []string{"agent.attach"}})
	require.NoError(t, err)
	portRD, err := f.store.CreateRoleDefinition(ctx, &store.RoleDefinition{
		Name: "conduit-port", ScopeType: store.RoleScopeProject, Permissions: []string{"agent.port_access"}})
	require.NoError(t, err)
	f.attachRoleID, f.portRoleID = attachRD.ID, portRD.ID
	f.u1 = f.newUser(t, "u1")
	f.u2 = f.newUser(t, "u2")

	if events == nil {
		events = NewChannelEventPublisher()
	}
	f.srv.SetEventPublisher(events)
	var m conduitStreamAuthzMetrics = f.metrics
	f.srv.conduitAuthzMetrics.Store(&m)
	runCtx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	f.a = f.srv.startConduitStreamAuthz(runCtx, f.clk, interval)
	return f
}

// newUser creates a project member holding custom attach and port roles.
func (f *recheckFixture) newUser(t *testing.T, name string) *AuthenticatedUser {
	t.Helper()
	ctx := context.Background()
	id := tid("recheck-" + name)
	email := name + "@conduit.test"
	createTestUserWithProjectRole(t, f.store, id, email, f.agent.ProjectID, store.ProjectRoleMember)
	ensureHubMembership(ctx, f.store, id)
	for rd, into := range map[string]map[string]string{f.attachRoleID: f.attachBind, f.portRoleID: f.portBind} {
		b, err := f.store.CreateRoleBinding(ctx, &store.RoleBinding{
			RoleDefinitionID: rd, PrincipalType: store.RoleBindingPrincipalUser, PrincipalID: id,
			ScopeType: store.RoleScopeProject, ScopeID: f.agent.ProjectID, CreatedBy: "test",
		})
		require.NoError(t, err)
		into[id] = b.ID
	}
	return NewAuthenticatedUser(id, email, name, store.UserRoleMember, "api")
}

// open tracks a stream opened by ident and returns its watch. It checks
// that the stream would be admitted now.
func (f *recheckFixture) open(t *testing.T, ident Identity, kind string, id uint32) *streamWatch {
	t.Helper()
	action := conduitStreamAction(kind)
	require.NoError(t, f.srv.authorizeConduitAction(context.Background(), ident, f.agent, action), "the stream would not be admitted")
	w := &streamWatch{closed: make(chan string, 1)}
	untrack := f.srv.trackConduitUserStream(&conduitUserStream{
		Kind: kind, Identity: ident, AgentID: f.agent.ID, ProjectID: f.agent.ProjectID,
		Port: 3000, SessionID: "sess-1", StreamID: id, Close: w.close,
	})
	t.Cleanup(untrack)
	return w
}

// deleteBinding removes a role binding through the admin API.
func (f *recheckFixture) deleteBinding(t *testing.T, id string) {
	t.Helper()
	rec := doRequest(t, f.srv, http.MethodDelete, "/api/v1/admin/role-bindings/"+id, nil)
	require.Equal(t, http.StatusNoContent, rec.Code, rec.Body.String())
}

// TestConduitAuthzRecheck_PermissionRemovedClosesOnNotify (R1/R4, unit
// tier): removing U1's attach binding through the API closes U1's PTY
// stream with 4401 authz_expired on the notify path (trigger=notify), and
// U2's PTY stream to the same agent stays open.
func TestConduitAuthzRecheck_PermissionRemovedClosesOnNotify(t *testing.T) {
	f := newRecheckFixture(t, nil, -1)
	u1 := f.open(t, f.u1, grant.StreamKindPTY, 1)
	u2 := f.open(t, f.u2, grant.StreamKindPTY, 3)

	f.deleteBinding(t, f.attachBind[f.u1.ID()])

	assert.Equal(t, "4401 authz_expired", u1.waitClosed(t))
	f.metrics.wait(t, "notify/closed/pty", 1)
	u2.assertOpen(t)
	assert.NotContains(t, f.metrics.list(), "interval/closed/pty")
}

// TestConduitAuthzRecheck_AttachRevokeKeepsTCP (R3): revoking only attach
// closes U1's PTY stream; U1's TCP stream stays open and records no close.
func TestConduitAuthzRecheck_AttachRevokeKeepsTCP(t *testing.T) {
	f := newRecheckFixture(t, nil, -1)
	pty := f.open(t, f.u1, grant.StreamKindPTY, 1)
	tcp := f.open(t, f.u1, grant.StreamKindTCP, 3)

	f.deleteBinding(t, f.attachBind[f.u1.ID()])

	assert.Equal(t, "4401 authz_expired", pty.waitClosed(t))
	f.metrics.wait(t, "notify/passed/tcp", 1)
	tcp.assertOpen(t)
	assert.Equal(t, 1, f.a.Len())
}

// TestConduitAuthzRecheck_PortRevokeKeepsPTY (R3, reverse): revoking only
// port access closes the TCP stream and leaves the PTY stream open.
func TestConduitAuthzRecheck_PortRevokeKeepsPTY(t *testing.T) {
	f := newRecheckFixture(t, nil, -1)
	pty := f.open(t, f.u1, grant.StreamKindPTY, 1)
	tcp := f.open(t, f.u1, grant.StreamKindTCP, 3)

	f.deleteBinding(t, f.portBind[f.u1.ID()])

	assert.Equal(t, "4401 authz_expired", tcp.waitClosed(t))
	f.metrics.wait(t, "notify/passed/pty", 1)
	pty.assertOpen(t)
}

// TestConduitAuthzRecheck_UserSuspendedCloses (R5a): suspending U1
// through the API closes U1's stream with 4401 on the notify path.
func TestConduitAuthzRecheck_UserSuspendedCloses(t *testing.T) {
	f := newRecheckFixture(t, nil, -1)
	u1 := f.open(t, f.u1, grant.StreamKindPTY, 1)
	u2 := f.open(t, f.u2, grant.StreamKindPTY, 3)

	rec := doRequest(t, f.srv, http.MethodPatch, "/api/v1/users/"+f.u1.ID(), map[string]string{"status": store.UserStatusSuspended})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	assert.Equal(t, "4401 authz_expired", u1.waitClosed(t))
	f.metrics.wait(t, "notify/closed/pty", 1)
	u2.assertOpen(t)
}

// TestConduitAuthzRecheck_UserDeletedCloses (R5a): deleting U1 through the
// API closes U1's stream with 4401 on the notify path.
func TestConduitAuthzRecheck_UserDeletedCloses(t *testing.T) {
	f := newRecheckFixture(t, nil, -1)
	u1 := f.open(t, f.u1, grant.StreamKindTCP, 1)

	rec := doRequest(t, f.srv, http.MethodDelete, "/api/v1/users/"+f.u1.ID(), nil)
	require.Equal(t, http.StatusNoContent, rec.Code, rec.Body.String())

	assert.Equal(t, "4401 authz_expired", u1.waitClosed(t))
	f.metrics.wait(t, "notify/closed/tcp", 1)
}

// TestConduitAuthzRecheck_TokenRevokedStaysOpen (R5b, negative control):
// revoking the access token U1 opened the stream with, while U1 stays
// active and permitted, runs a notify re-check that passes; the stream
// stays open (authorization is bound to the user, not the credential).
func TestConduitAuthzRecheck_TokenRevokedStaysOpen(t *testing.T) {
	f := newRecheckFixture(t, nil, -1)
	key, tok, err := f.srv.uatService.CreateToken(rs4MintContext(f.u1.ID()), f.u1.ID(), "stream", f.agent.ProjectID, []string{"agent:attach"}, nil)
	require.NoError(t, err)
	ident, err := f.srv.uatService.ValidateToken(context.Background(), key)
	require.NoError(t, err)
	w := f.open(t, ident, grant.StreamKindPTY, 1)

	req := httptest.NewRequest(http.MethodPost, "/api/v1/auth/tokens/"+tok.ID+"/revoke", nil)
	ctx := rs4MintContext(f.u1.ID())
	req = req.WithContext(context.WithValue(ctx, userContextKey{}, GetIdentityFromContext(ctx)))
	rec := httptest.NewRecorder()
	f.srv.handleRevokeToken(rec, req, tok.ID)
	require.Equal(t, http.StatusNoContent, rec.Code, rec.Body.String())

	f.metrics.wait(t, "notify/passed/pty", 1)
	w.assertOpen(t)
}

// TestConduitAuthzRecheck_AgentDeletedCloses4404 (R6): the agent deleted
// event closes the stream with 4404 target_not_found, not 4401.
func TestConduitAuthzRecheck_AgentDeletedCloses4404(t *testing.T) {
	f := newRecheckFixture(t, nil, -1)
	w := f.open(t, f.u1, grant.StreamKindPTY, 1)
	ctx := context.Background()
	require.NoError(t, f.store.DeleteAgent(ctx, f.agent.ID))
	f.srv.events.PublishAgentDeleted(ctx, f.agent.ID, f.agent.ProjectID)

	assert.Equal(t, "4404 target_not_found", w.waitClosed(t))
	f.metrics.wait(t, "notify/target_gone/pty", 1)
}

// TestConduitAuthzRecheck_PortUnexposedClosesTCP: an exposed-port change
// that removes the stream's port closes the TCP stream (the target is no
// longer authorized); a PTY stream to the agent stays open.
func TestConduitAuthzRecheck_PortUnexposedClosesTCP(t *testing.T) {
	f := newRecheckFixture(t, nil, -1)
	tcp := f.open(t, f.u1, grant.StreamKindTCP, 1)
	pty := f.open(t, f.u1, grant.StreamKindPTY, 3)
	ctx := context.Background()
	require.NoError(t, f.store.UpdateAgentExposedPorts(ctx, f.agent.ID, nil))
	got, err := f.store.GetAgent(ctx, f.agent.ID)
	require.NoError(t, err)
	f.srv.events.PublishAgentPorts(ctx, got)

	assert.Equal(t, "4401 authz_expired", tcp.waitClosed(t))
	f.metrics.wait(t, "notify/passed/pty", 1)
	pty.assertOpen(t)
}

// TestConduitAuthzRecheck_RestoredBeforeCheckPasses (R7): a permission
// removed and restored before the re-check runs leaves the stream open;
// the check reads the restored state and records notify/passed.
func TestConduitAuthzRecheck_RestoredBeforeCheckPasses(t *testing.T) {
	f := newRecheckFixture(t, nil, -1)
	w := f.open(t, f.u1, grant.StreamKindPTY, 1)
	ctx := context.Background()
	require.NoError(t, f.store.DeleteRoleBinding(ctx, f.attachBind[f.u1.ID()]))
	_, err := f.store.CreateRoleBinding(ctx, &store.RoleBinding{
		RoleDefinitionID: f.attachRoleID, PrincipalType: store.RoleBindingPrincipalUser, PrincipalID: f.u1.ID(),
		ScopeType: store.RoleScopeProject, ScopeID: f.agent.ProjectID, CreatedBy: "test",
	})
	require.NoError(t, err)
	f.srv.publishConduitAuthzChanged(conduitAuthzMatch{UserID: f.u1.ID()})

	f.metrics.wait(t, "notify/passed/pty", 1)
	w.assertOpen(t)
}

// TestConduitAuthzRecheck_SweepClosesMissedRevocation (backstop): a
// revocation whose event never arrives is closed by the sweep within one
// authz_recheck_interval, with trigger=sweep.
func TestConduitAuthzRecheck_SweepClosesMissedRevocation(t *testing.T) {
	f := newRecheckFixture(t, nil, 60*time.Second)
	w := f.open(t, f.u1, grant.StreamKindPTY, 1)
	// Revoke in the store only: no event is published.
	require.NoError(t, f.store.DeleteRoleBinding(context.Background(), f.attachBind[f.u1.ID()]))

	f.clk.Advance(59 * time.Second)
	w.assertOpen(t)
	f.clk.Advance(time.Second)
	assert.Equal(t, "4401 authz_expired", w.waitClosed(t))
	assert.Contains(t, f.metrics.list(), "sweep/closed/pty")
}

// listenGapPublisher is an in-process publisher whose "LISTEN connection"
// can be dropped: while down, published events are lost, and reconnect
// runs the AddOnListen callbacks, as PostgresEventPublisher does.
type listenGapPublisher struct {
	*ChannelEventPublisher
	down  atomic.Bool
	mu    sync.Mutex
	hooks map[*listenHook]struct{}
}

func newListenGapPublisher() *listenGapPublisher {
	return &listenGapPublisher{ChannelEventPublisher: NewChannelEventPublisher(), hooks: map[*listenHook]struct{}{}}
}

func (p *listenGapPublisher) PublishRaw(subject string, data interface{}) {
	if p.down.Load() {
		return // missed while the listener is down
	}
	p.ChannelEventPublisher.PublishRaw(subject, data)
}

func (p *listenGapPublisher) AddOnListen(fn func()) func() {
	h := &listenHook{fn: fn}
	p.mu.Lock()
	p.hooks[h] = struct{}{}
	p.mu.Unlock()
	return func() {
		p.mu.Lock()
		delete(p.hooks, h)
		p.mu.Unlock()
	}
}

// reconnect brings the listener back and runs the callbacks.
func (p *listenGapPublisher) reconnect() {
	p.down.Store(false)
	p.mu.Lock()
	var fns []func()
	for h := range p.hooks {
		fns = append(fns, h.fn)
	}
	p.mu.Unlock()
	for _, fn := range fns {
		go fn()
	}
}

// TestConduitAuthzRecheck_ResyncAfterListenGap (R8, unit tier): the
// LISTEN connection drops, a revocation lands during the gap (its event
// is lost), and the reconnect's resync closes the stream with 4401 and
// trigger=resync, well before the sweep would.
func TestConduitAuthzRecheck_ResyncAfterListenGap(t *testing.T) {
	pub := newListenGapPublisher()
	f := newRecheckFixture(t, pub, 60*time.Second)
	w := f.open(t, f.u1, grant.StreamKindPTY, 1)
	kept := f.open(t, f.u2, grant.StreamKindPTY, 3)

	pub.down.Store(true)
	f.deleteBinding(t, f.attachBind[f.u1.ID()])
	w.assertOpen(t)

	pub.reconnect()
	assert.Equal(t, "4401 authz_expired", w.waitClosed(t))
	f.metrics.wait(t, "resync/closed/pty", 1)
	f.metrics.wait(t, "resync/passed/pty", 1)
	kept.assertOpen(t)
	assert.NotContains(t, f.metrics.list(), "notify/closed/pty")
}

// faultStore makes selected reads fail.
type faultStore struct {
	store.Store
	failGetAgent     atomic.Bool
	getAgentOKCalls  atomic.Int32 // GetAgent calls that still succeed when failGetAgent is set
	failGetUser      atomic.Bool
	failBindingsList atomic.Bool
}

var errInjectedStoreFault = errors.New("injected store fault")

func (s *faultStore) GetAgent(ctx context.Context, id string) (*store.Agent, error) {
	if s.failGetAgent.Load() {
		if s.getAgentOKCalls.Add(-1) < 0 {
			return nil, errInjectedStoreFault
		}
	}
	return s.Store.GetAgent(ctx, id)
}

func (s *faultStore) GetUser(ctx context.Context, id string) (*store.User, error) {
	if s.failGetUser.Load() {
		return nil, errInjectedStoreFault
	}
	return s.Store.GetUser(ctx, id)
}

func (s *faultStore) ListRoleBindingsForPrincipals(ctx context.Context, principals []store.PrincipalRef, scopeTypes []string, scopeIDs []string) ([]*store.RoleBinding, error) {
	if s.failBindingsList.Load() {
		return nil, errInjectedStoreFault
	}
	return s.Store.ListRoleBindingsForPrincipals(ctx, principals, scopeTypes, scopeIDs)
}

// TestConduitAuthzRecheck_StoreFaultDefers (gate condition 4; O1 without
// the interval): a store fault during a re-check records
// deferred_unavailable and never closes the stream early, on every
// trigger, even when the permission was in fact revoked; once the store
// recovers the next check closes it.
func TestConduitAuthzRecheck_StoreFaultDefers(t *testing.T) {
	for _, tc := range []struct {
		name  string
		fault func(*faultStore)
	}{
		{"agent read fails", func(s *faultStore) { s.failGetAgent.Store(true) }},
		{"user read fails", func(s *faultStore) { s.failGetUser.Store(true) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newRecheckFixture(t, nil, 60*time.Second)
			w := f.open(t, f.u1, grant.StreamKindPTY, 1)
			require.NoError(t, f.store.DeleteRoleBinding(context.Background(), f.attachBind[f.u1.ID()]))
			fs := &faultStore{Store: f.srv.store}
			tc.fault(fs)
			f.srv.store = fs

			f.srv.publishConduitAuthzChanged(conduitAuthzMatch{UserID: f.u1.ID()})
			f.metrics.wait(t, "notify/deferred_unavailable/pty", 1)
			f.clk.Advance(60 * time.Second)
			f.metrics.wait(t, "sweep/deferred_unavailable/pty", 1)
			w.assertOpen(t)
			assert.Equal(t, 1, f.a.Len())

			f.srv.store = fs.Store // recovered
			f.clk.Advance(60 * time.Second)
			assert.Equal(t, "4401 authz_expired", w.waitClosed(t))
		})
	}
}

// TestConduitAuthzRecheck_AuthzLookupFaultDefers (O3): the authorization
// lookup failing while the store answers is deferred_unavailable, not a
// close.
func TestConduitAuthzRecheck_AuthzLookupFaultDefers(t *testing.T) {
	f := newRecheckFixture(t, nil, -1)
	w := f.open(t, f.u1, grant.StreamKindPTY, 1)
	fs := &faultStore{Store: f.srv.authzService.store}
	fs.failBindingsList.Store(true)
	f.srv.authzService.store = fs
	t.Cleanup(func() { f.srv.authzService.store = fs.Store })

	f.srv.publishConduitAuthzChanged(conduitAuthzMatch{UserID: f.u1.ID()})
	f.metrics.wait(t, "notify/deferred_unavailable/pty", 1)
	w.assertOpen(t)
}

// TestConduitAuthzRecheck_DenyWithStoreProbeFailureDefers (gate condition
// 2): a deny observed while the store stops answering resolves to
// deferred_unavailable, never a close.
func TestConduitAuthzRecheck_DenyWithStoreProbeFailureDefers(t *testing.T) {
	f := newRecheckFixture(t, nil, -1)
	w := f.open(t, f.u1, grant.StreamKindPTY, 1)
	require.NoError(t, f.store.DeleteRoleBinding(context.Background(), f.attachBind[f.u1.ID()]))
	fs := &faultStore{Store: f.srv.store}
	fs.getAgentOKCalls.Store(1) // the check's agent read succeeds, the probe after the deny fails
	fs.failGetAgent.Store(true)
	f.srv.store = fs
	t.Cleanup(func() { f.srv.store = fs.Store })

	f.a.Recheck(context.Background(), conduitAuthzTriggerNotify, conduitAuthzMatch{})
	assert.Equal(t, []string{"notify/deferred_unavailable/pty"}, f.metrics.list())
	w.assertOpen(t)
}

// TestConduitAuthzRecheck_NotTrackedForNonUsers: only user-originated
// streams are tracked; an agent's own stream is not.
func TestConduitAuthzRecheck_NotTrackedForNonUsers(t *testing.T) {
	f := newRecheckFixture(t, nil, -1)
	untrack := f.srv.trackConduitUserStream(&conduitUserStream{
		Kind: grant.StreamKindTCP, Identity: &storedAgentIdentity{}, AgentID: f.agent.ID, Close: func(uint32, string) {},
	})
	defer untrack()
	assert.Zero(t, f.a.Len())
}

// TestConduitAuthzChanged_NotPublishedWhenDisabled: no event is published
// while hub.conduit is off.
func TestConduitAuthzChanged_NotPublishedWhenDisabled(t *testing.T) {
	f := newRecheckFixture(t, nil, -1)
	ch, unsubscribe := f.srv.events.Subscribe(conduitAuthzChangedSubject)
	defer unsubscribe()
	setConduitExperiment(t, f.srv, false)
	f.srv.publishConduitAuthzChanged(conduitAuthzMatch{})
	setConduitExperiment(t, f.srv, true)
	f.srv.publishConduitAuthzChanged(conduitAuthzMatch{UserID: "marker"})
	select {
	case evt := <-ch:
		assert.True(t, strings.Contains(string(evt.Data), "marker"), "an event was published while disabled: %s", evt.Data)
	case <-time.After(recheckWait):
		t.Fatal("no event")
	}
}
