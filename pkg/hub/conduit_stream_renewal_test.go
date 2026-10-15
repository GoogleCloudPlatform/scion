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
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/conduit/clock"
	"github.com/GoogleCloudPlatform/scion/pkg/conduit/grant"
	"github.com/GoogleCloudPlatform/scion/pkg/conduit/registry"
	"github.com/GoogleCloudPlatform/scion/pkg/conduit/relay"
	"github.com/GoogleCloudPlatform/scion/pkg/conduit/router"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// The renewal driver and its classifier (design v2.21.36 §3.5,
// Target-side deadlines; criterion 29). Unit tests run the tracker on a
// fake clock with a stubbed renewal re-check.

// TestClassifyConduitRenewal pins the classification table of design
// v2.21.36 §3.5 (Refusal meaning).
func TestClassifyConduitRenewal(t *testing.T) {
	wrapped := func(err error) error { return fmt.Errorf("%w: %w", errConduitRenewalUnavailable, err) }
	for _, tc := range []struct {
		name string
		err  error
		want ConduitRenewalOutcome
	}{
		{"agent gone", errConduitAgentGone, ConduitRenewalTargetGone},
		{"forbidden", fmt.Errorf("%w: permission removed", errConduitForbidden), ConduitRenewalDeny},
		{"principal inactive", fmt.Errorf("%w: user suspended", errConduitPrincipalInactive), ConduitRenewalDeny},
		{"project mismatch", fmt.Errorf("%w: agent is no longer in the grant's project", errConduitForbidden), ConduitRenewalDeny},
		{"bad signature", wrapped(grant.ErrSignature), ConduitRenewalDeny},
		{"wrong typ or algorithm", wrapped(grant.ErrAlgorithm), ConduitRenewalDeny},
		{"wrong audience", wrapped(grant.ErrAudience), ConduitRenewalDeny},
		{"wrong issuer", wrapped(grant.ErrIssuer), ConduitRenewalDeny},
		{"malformed", wrapped(grant.ErrMalformed), ConduitRenewalDeny},
		{"binding: session, epoch, incarnation", wrapped(grant.ErrTarget), ConduitRenewalDeny},
		{"binding: open_jti, stream_id", wrapped(grant.ErrStreamBinding), ConduitRenewalDeny},
		{"window passed", wrapped(grant.ErrRecheckWindow), ConduitRenewalDeny},
		{"nbf too far ahead", wrapped(grant.ErrNotYetValid), ConduitRenewalDeny},
		{"unknown kid after a successful reload (incl. revoked for compromise)", wrapped(grant.ErrUnknownKey), ConduitRenewalDeny},
		{"expired key plus bad signature", wrapped(grant.ErrSignature), ConduitRenewalDeny},
		{"expired key plus window passed", wrapped(grant.ErrRecheckWindow), ConduitRenewalDeny},
		{"expired key alone", wrapped(grant.ErrKeyExpired), ConduitRenewalUnavailable},
		{"key set load failure", fmt.Errorf("%w: grant keys: boom", errConduitAuthzUnavailable), ConduitRenewalUnavailable},
		{"forbidden that could not be evaluated", fmt.Errorf("%w: %w: lookup", errConduitForbidden, errConduitAuthzUnavailable), ConduitRenewalUnavailable},
		{"transient store error", fmt.Errorf("%w: agent lookup: boom", errConduitAuthzUnavailable), ConduitRenewalUnavailable},
		{"placement", fmt.Errorf("%w: agent placement: %w", errConduitRenewalUnavailable, errors.New("pin stale")), ConduitRenewalUnavailable},
		{"validly signed grant without via", wrapped(grant.ErrVia), ConduitRenewalUnavailable},
		{"validly signed grant without authz_until", wrapped(grant.ErrAuthzUntil), ConduitRenewalUnavailable},
		{"via outside the table", fmt.Errorf("%w: origin \"proxy\" does not carry kind \"pty\"", errConduitRenewalUnavailable), ConduitRenewalUnavailable},
		{"invalid claims", wrapped(grant.ErrInvalidClaims), ConduitRenewalUnavailable},
		{"unclassified", errors.New("boom"), ConduitRenewalUnavailable},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, ClassifyConduitRenewal(tc.err))
		})
	}
}

// renewalDriverFixture is a tracker with a stubbed renewal re-check.
type renewalDriverFixture struct {
	clk *clock.Fake
	a   *conduitStreamAuthz
	rec *authzRecorder

	mu        sync.Mutex
	calls     []time.Time
	presented []string
	result    func(n int) (*conduitRenewal, error)
}

const driverInterval = 10 * time.Minute

func newRenewalDriverFixture(t *testing.T, enforce bool) *renewalDriverFixture {
	t.Helper()
	rec, logger := newAuthzRecorder()
	f := &renewalDriverFixture{clk: clock.NewFake(time.Date(2026, 10, 15, 1, 0, 0, 0, time.UTC)), rec: rec}
	f.result = f.renewed
	f.a = newConduitStreamAuthz(conduitStreamAuthzConfig{
		Check:              (&stubCheck{verdicts: map[string]conduitAuthzVerdict{}}).check,
		Clock:              f.clk,
		RecheckInterval:    -1,
		UserStreamAuthzMax: driverInterval,
		Metrics:            rec,
		Logger:             logger,
		Renew: func(_ context.Context, st *conduitUserStream) (*conduitRenewal, error) {
			f.mu.Lock()
			f.calls = append(f.calls, f.clk.Now())
			f.presented = append(f.presented, string(st.Grant))
			n, result := len(f.calls), f.result
			f.mu.Unlock()
			return result(n)
		},
		Enforce: func() bool { return enforce },
		Jitter:  func(time.Duration) time.Duration { return 0 },
	})
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	f.a.Start(ctx)
	return f
}

// renewed is a successful renewal, numbered n, moving authz_until to now
// plus the interval.
func (f *renewalDriverFixture) renewed(n int) (*conduitRenewal, error) {
	return &conduitRenewal{Token: []byte(fmt.Sprintf("renewal-%d", n)), AuthzUntil: f.clk.Now().Add(driverInterval)}, nil
}

func (f *renewalDriverFixture) setResult(r func(n int) (*conduitRenewal, error)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.result = r
}

func (f *renewalDriverFixture) callTimes() []time.Time {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]time.Time(nil), f.calls...)
}

func (f *renewalDriverFixture) presentedGrants() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.presented...)
}

// track tracks a stream whose open grant carried authz_until = now plus
// the interval.
func (f *renewalDriverFixture) track() (*conduitUserStream, *closeRecorder, *renewRecorder) {
	c, r := &closeRecorder{}, &renewRecorder{}
	now := f.clk.Now()
	st := &conduitUserStream{
		Kind: grant.StreamKindPTY, Identity: NewAuthenticatedUser("u1", "u1@x", "u1", "member", "api"),
		UserID: "u1", AgentID: "agent-x", ProjectID: "p1", SessionID: "sess-1", StreamID: 7,
		Admitted: now, Close: c.close, Renew: r.renew,
		Grant: []byte("open"), OpenJTI: "open-jti", AuthzUntil: now.Add(driverInterval), AuthzInterval: driverInterval,
	}
	f.a.Track(st)
	return st, c, r
}

// TestRenewalDriver_RenewsAt80Percent: with enforce, the stream is renewed
// at 80% of its interval; the renewal grant is sent unchanged, the hub
// deadline moves to its authz_until, and the next renewal presents it.
func TestRenewalDriver_RenewsAt80Percent(t *testing.T) {
	f := newRenewalDriverFixture(t, true)
	st, c, r := f.track()
	start := f.clk.Now()
	assert.True(t, st.Deadline().Equal(start.Add(driverInterval)), "with enforce the deadline is authz_until")

	f.clk.Advance(8*time.Minute - time.Second)
	assert.Empty(t, f.callTimes(), "no renewal before 80%")
	f.clk.Advance(time.Second)
	require.Len(t, f.callTimes(), 1)
	assert.Equal(t, [][]byte{[]byte("renewal-1")}, r.grants, "the renewal grant is sent unchanged")
	assert.True(t, st.Deadline().Equal(start.Add(8*time.Minute+driverInterval)), "the deadline moved to the renewal's authz_until")

	f.clk.Advance(8 * time.Minute)
	assert.Equal(t, []string{"open", "renewal-1"}, f.presentedGrants(), "each renewal presents the latest grant")
	f.clk.Advance(30 * time.Minute)
	assert.Empty(t, c.list(), "a renewed stream stays open")
	assert.Contains(t, f.rec.metricList(), conduitAuthzTriggerRenewal+"/"+conduitAuthzOutcomeRenewed+"/"+grant.StreamKindPTY)
}

// TestRenewalDriver_UnavailableClosesAtDeadline (C2): with enforce, a
// stream whose renewals are always unavailable, or fail with an
// unclassified error, closes with 4401 exactly at authz_until, and
// unavailable never moves authz_until or the deadline. Retries follow half
// the remaining time with a 30s floor and stop at authz_until.
func TestRenewalDriver_UnavailableClosesAtDeadline(t *testing.T) {
	for name, err := range map[string]error{
		"unavailable":  fmt.Errorf("%w: store", errConduitAuthzUnavailable),
		"unclassified": errors.New("boom"),
	} {
		t.Run(name, func(t *testing.T) {
			f := newRenewalDriverFixture(t, true)
			f.setResult(func(int) (*conduitRenewal, error) { return nil, err })
			st, c, r := f.track()
			start := f.clk.Now()
			until := st.AuthzUntil

			f.clk.Advance(driverInterval - time.Second)
			assert.Empty(t, c.list(), "no close before authz_until")
			assert.True(t, st.Deadline().Equal(until), "unavailable never moves the deadline")
			assert.True(t, st.AuthzUntil.Equal(until), "unavailable never moves authz_until")
			assert.Zero(t, r.count(), "nothing is sent")
			f.clk.Advance(time.Second)
			assert.Equal(t, []string{"4401 authz_expired"}, c.list(), "closed exactly at authz_until")

			want := []time.Time{start.Add(8 * time.Minute), start.Add(9 * time.Minute), start.Add(9*time.Minute + 30*time.Second)}
			calls := f.callTimes()
			require.GreaterOrEqual(t, len(calls), len(want))
			for i, w := range want {
				assert.True(t, calls[i].Equal(w), "attempt %d at %v, want %v", i, calls[i], w)
			}
			for _, at := range calls {
				assert.False(t, at.After(until), "no attempt after authz_until")
			}
		})
	}
}

// TestRenewalDriver_DenyAndGoneClose: with enforce, a deny closes the
// stream with 4401 at once and a gone agent with 4404.
func TestRenewalDriver_DenyAndGoneClose(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		want string
	}{
		{"deny", fmt.Errorf("%w: permission removed", errConduitForbidden), "4401 authz_expired"},
		{"deny on verification", fmt.Errorf("%w: %w", errConduitRenewalUnavailable, grant.ErrSignature), "4401 authz_expired"},
		{"gone", errConduitAgentGone, fmt.Sprintf("%d %s", relay.CloseTargetNotFound, relay.ReasonTargetNotFound)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newRenewalDriverFixture(t, true)
			f.setResult(func(int) (*conduitRenewal, error) { return nil, tc.err })
			_, c, r := f.track()
			f.clk.Advance(8 * time.Minute)
			assert.Equal(t, []string{tc.want}, c.list())
			assert.Zero(t, r.count())
			assert.Zero(t, f.a.Len(), "untracked")
		})
	}
}

// TestRenewalDriver_OffTakesNoCloseAction (C4): with off, the driver still
// sends renewals, but a stream never renewed is not closed by the driver at
// authz_until, and a deny takes no close action and stops renewals.
func TestRenewalDriver_OffTakesNoCloseAction(t *testing.T) {
	t.Run("renewals sent", func(t *testing.T) {
		f := newRenewalDriverFixture(t, false)
		_, c, r := f.track()
		f.clk.Advance(8 * time.Minute)
		assert.Equal(t, 1, r.count(), "off still sends renewals")
		assert.Empty(t, c.list())
	})
	t.Run("never renewed", func(t *testing.T) {
		f := newRenewalDriverFixture(t, false)
		f.setResult(func(int) (*conduitRenewal, error) { return nil, fmt.Errorf("%w: store", errConduitAuthzUnavailable) })
		st, c, _ := f.track()
		f.clk.Advance(driverInterval + time.Minute)
		assert.Empty(t, c.list(), "the driver does not close at authz_until under off")
		assert.False(t, st.isEnforced())
	})
	t.Run("deny", func(t *testing.T) {
		f := newRenewalDriverFixture(t, false)
		f.setResult(func(int) (*conduitRenewal, error) { return nil, fmt.Errorf("%w: removed", errConduitForbidden) })
		_, c, _ := f.track()
		f.clk.Advance(8 * time.Minute)
		f.clk.Advance(time.Minute)
		assert.Empty(t, c.list(), "no close action from the driver under off")
		assert.Len(t, f.callTimes(), 1, "renewals stop after the deny")
		assert.Contains(t, f.rec.metricList(), conduitAuthzTriggerRenewal+"/"+conduitAuthzOutcomeRenewalStopped+"/"+grant.StreamKindPTY)
	})
}

// TestRenewalDriver_SendFailureMovesNothing: a minted renewal that cannot
// be sent moves nothing and is retried.
func TestRenewalDriver_SendFailureMovesNothing(t *testing.T) {
	f := newRenewalDriverFixture(t, true)
	st, c, r := f.track()
	r.err = errors.New("session gone")
	until := st.AuthzUntil
	f.clk.Advance(8 * time.Minute)
	assert.Equal(t, 1, r.count())
	assert.True(t, st.Deadline().Equal(until))
	assert.Equal(t, "open", string(st.Grant), "the latest grant stays the open grant")
	r.mu.Lock()
	r.err = nil
	r.mu.Unlock()
	f.clk.Advance(time.Minute)
	assert.Equal(t, 2, r.count(), "retried")
	assert.True(t, st.Deadline().After(until))
	assert.Empty(t, c.list())
	assert.Contains(t, f.rec.metricList(), conduitAuthzTriggerRenewal+"/"+conduitAuthzOutcomeNotDelivered+"/"+grant.StreamKindPTY)
}

// TestRenewalDriver_NonDeadlineStreamUnchanged: a stream whose grant
// carried no authz_until gets no renewal driver and keeps today's
// in-process deadline.
func TestRenewalDriver_NonDeadlineStreamUnchanged(t *testing.T) {
	f := newRenewalDriverFixture(t, true)
	c, r := &closeRecorder{}, &renewRecorder{}
	st := &conduitUserStream{
		Kind: grant.StreamKindPTY, Identity: NewAuthenticatedUser("u1", "u1@x", "u1", "member", "api"),
		UserID: "u1", AgentID: "agent-x", ProjectID: "p1", SessionID: "sess-1", StreamID: 9,
		Admitted: f.clk.Now(), Close: c.close, Renew: r.renew,
	}
	f.a.Track(st)
	f.clk.Advance(driverInterval)
	assert.Empty(t, f.callTimes(), "no renewal re-check")
	assert.Empty(t, c.list(), "the in-process interval check renewed it")
	assert.Equal(t, [][]byte{nil}, r.grants, "the advisory notice carries no grant")
}

// TestRenewalJitterBounds: the default jitter stays within plus or minus
// 5% of the interval.
func TestRenewalJitterBounds(t *testing.T) {
	for i := 0; i < 1000; i++ {
		j := defaultConduitRenewalJitter(driverInterval)
		require.LessOrEqual(t, j, driverInterval/20)
		require.GreaterOrEqual(t, j, -driverInterval/20)
	}
	assert.Zero(t, defaultConduitRenewalJitter(0))
}

// TestConduitRouteCanRenew: in-process hub-held routes deliver renewals;
// external relay mode (bridged leaves, until 6.6b-3) and legacy sessions do
// not.
func TestConduitRouteCanRenew(t *testing.T) {
	rec := registry.SessionRecord{SessionID: "s-1", PrincipalKind: registry.PrincipalAgent, RelayInstanceID: "hub-x", RelayGeneration: 1}
	res := router.Resolved{Session: relay.NewRemoteSession(&relay.PeerClient{}, "http://hub-x", rec, registry.Want{ProjectID: "p"}), Record: rec}

	f := newConduitFixture(t)
	assert.True(t, f.srv.conduitRouteCanRenew(res), "in-process: renewals cross the hop")
	assert.False(t, f.srv.conduitRouteCanRenew(router.Resolved{}), "no session")

	_, cf := newUserRelayOps(t)
	assert.False(t, cf.srv.conduitRouteCanRenew(res), "external relay mode: bridged leaves do not renew until 6.6b-3")
}

// pinInconsistently pins the agent to its own Runtime Broker, which has no
// broker row: an inconsistent saved placement (runtime_broker_missing).
func pinInconsistently(t *testing.T, f *conduitFixture) {
	t.Helper()
	ctx := context.Background()
	_, err := f.store.SetAgentPinnedRuntimeTarget(ctx, f.agent.ID,
		store.PinnedPlacement{RuntimeBrokerID: f.agent.RuntimeBrokerID},
		store.PinnedPlacement{RuntimeBrokerID: f.agent.RuntimeBrokerID, RuntimeTargetID: "target-1", RuntimeTargetType: "docker"})
	require.NoError(t, err)
	got, err := f.store.GetAgent(ctx, f.agent.ID)
	require.NoError(t, err)
	f.agent = got
}

// TestCheckConduitUserStream_PlacementIsUnavailable (66b2-T2): the
// hub-held re-check applies the open's placement guard after the
// authorization verdict; an inconsistent saved placement is unavailable,
// never a deny.
func TestCheckConduitUserStream_PlacementIsUnavailable(t *testing.T) {
	f := newRenewalFixture(t)
	st := &conduitUserStream{
		Kind: grant.StreamKindTCP, Identity: f.user, UserID: f.user.ID(), AgentID: f.agent.ID, ProjectID: f.agent.ProjectID,
		Port: 3000, SessionID: renewalSessionID, StreamID: 7,
	}
	verdict, _ := f.srv.checkConduitUserStream(context.Background(), st)
	require.Equal(t, conduitAuthzAllowed, verdict, "consistent placement: allowed")

	pinInconsistently(t, f.conduitFixture)
	verdict, reason := f.srv.checkConduitUserStream(context.Background(), st)
	assert.Equal(t, conduitAuthzUnavailable, verdict)
	assert.Equal(t, "agent placement inconsistent", reason)
}

// TestRenewalDriver_InconsistentPlacementEndsAtDeadline (66b2-T2, end to
// end at the tracker with the real re-check and renewal): with enforce, a
// hub-held stream to a pinned agent whose saved placement is inconsistent
// gets no renewal minted or sent, is not closed early by a sweep, and
// closes with 4401 at its deadline.
func TestRenewalDriver_InconsistentPlacementEndsAtDeadline(t *testing.T) {
	f := newRenewalFixture(t)
	tok, open := f.openGrant(t, grant.ViaProxy, tcpHeader("3000"), nil)
	pinInconsistently(t, f.conduitFixture)

	clk := clock.NewFake(f.clock.Now())
	a := newConduitStreamAuthz(conduitStreamAuthzConfig{
		Check:           f.srv.checkConduitUserStream,
		Clock:           clk,
		RecheckInterval: -1,
		Renew:           f.srv.renewConduitUserStream,
		Enforce:         func() bool { return true },
		Jitter:          func(time.Duration) time.Duration { return 0 },
	})
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	a.Start(ctx)
	c, r := &closeRecorder{}, &renewRecorder{}
	st := &conduitUserStream{
		Kind: grant.StreamKindTCP, Identity: f.user, AgentID: f.agent.ID, ProjectID: f.agent.ProjectID,
		Port: 3000, SessionID: renewalSessionID, StreamID: 7, Admitted: clk.Now(), Close: c.close, Renew: r.renew,
	}
	applyConduitOpenGrant(st, tok, &open)
	a.trackForTest(f.srv, st)

	clk.Advance(open.AuthzUntil.Sub(clk.Now()) - time.Second)
	a.Recheck(ctx, conduitAuthzTriggerSweep, conduitAuthzMatch{})
	assert.Zero(t, r.count(), "no renewal is minted or sent")
	assert.Empty(t, c.list(), "no early close")
	assert.True(t, st.Deadline().Equal(open.AuthzUntil))
	clk.Advance(time.Second)
	assert.Equal(t, []string{"4401 authz_expired"}, c.list(), "closed at the deadline")
}

// trackForTest tracks st for srv's user (trackConduitUserStream's
// bookkeeping) on a.
func (a *conduitStreamAuthz) trackForTest(srv *Server, st *conduitUserStream) {
	if ident, ok := st.Identity.(UserIdentity); ok {
		st.UserID = ident.ID()
	}
	a.Track(st)
}

// deadlineE2E is a PTY fixture under enforce with a 2m user stream
// interval, whose stream re-check runs on clk; f.clock, clk and the
// sciontool agent's clock move together (advance). The relay's own
// session timers stay on their own clock (AuthzClock).
type deadlineE2E struct {
	f   *ptyConduitFixture
	clk *clock.Fake
}

const e2eInterval = 2 * time.Minute

func newDeadlineE2E(t *testing.T, crossNode bool) *deadlineE2E {
	t.Helper()
	// newConduitFixture's clock starts here; the re-check clock must read
	// the same time, since grants carry deadlines on f.clock.
	clk := clock.NewFake(time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC))
	f := newPTYConduitFixtureServer(t, func(s *Server) {
		enforceTargetDeadlines(s)
		s.config.ConduitUserStreamAuthzMax = e2eInterval
	}, func(o *ConduitRelayOptions) { o.AuthzClock = clk })
	require.True(t, f.clock.Now().Equal(clk.Now()), "the clocks start together")
	f.setBrokerRow(t, attachCaps(false), "")
	hubURL := f.public.URL
	if crossNode {
		hubURL = f.startPeerRelay(t, "hub-b")
	}
	f.startPTYAgent(t, hubURL)
	return &deadlineE2E{f: f, clk: clk}
}

// advance moves the three clocks by d: f.clock (grants, the agent's time)
// first, then the agent's timers, then the hub's re-check, so a renewal the
// hub sends reaches an agent whose clock already reads the same time.
func (e *deadlineE2E) advance(d time.Duration) {
	e.f.clock.Advance(d)
	e.f.agentClock.Advance(d)
	e.clk.Advance(d)
}

// open attaches a PTY and returns the client and the tracked stream.
func (e *deadlineE2E) open(t *testing.T) (*websocket.Conn, *conduitUserStream) {
	t.Helper()
	c, _, err := e.f.dialPTY(t, "")
	require.NoError(t, err)
	waitSpawn(t, e.f.spawned)
	echoRoundTrip(t, c, "opened")
	st := soleTrackedStream(t, e.f.srv.conduitAuthz.Load())
	require.True(t, st.isEnforced())
	require.False(t, st.AuthzUntil.IsZero())
	return c, st
}

// TestDeadlineE2E_RenewalMovesTargetDeadline (criterion 29, every route
// with a renewal path): under enforce, a hub-held PTY stream to a real
// sciontool agent, renewed by the driver at 80% of its interval, survives
// past its first authz_until + 30s on the agent's clock, on a session this
// node holds and on one another hub node's relay holds. With the driver's
// sender disabled, the same stream closes at its deadline.
func TestDeadlineE2E_RenewalMovesTargetDeadline(t *testing.T) {
	for name, crossNode := range map[string]bool{"local session": false, "cross-node session": true} {
		t.Run(name, func(t *testing.T) {
			e := newDeadlineE2E(t, crossNode)
			c, st := e.open(t)
			first := st.AuthzUntil

			// Past 80% plus the largest jitter, before the first deadline.
			e.advance(e2eInterval*4/5 + e2eInterval/20 + time.Second)
			renewedUntil := e.f.clock.Now().Truncate(time.Second).Add(e2eInterval)
			require.True(t, e.f.agentClock.WaitForTimer(10*time.Second, renewedUntil.Add(30*time.Second)),
				"the target moved its deadline to the renewal's authz_until")
			assert.True(t, st.Deadline().Equal(renewedUntil), "the hub deadline moved with it")

			// Past the first deadline plus the target's grace.
			e.advance(first.Add(30*time.Second).Sub(e.f.clock.Now()) + 10*time.Second)
			echoRoundTrip(t, c, "still open")
		})
	}
	for name, crossNode := range map[string]bool{"local session, no sender": false, "cross-node session, no sender": true} {
		t.Run(name, func(t *testing.T) {
			e := newDeadlineE2E(t, crossNode)
			c, st := e.open(t)
			st.Renew = nil // the driver mints, but nothing is delivered
			e.advance(st.AuthzUntil.Sub(e.f.clock.Now()) + time.Second)
			ce := readUntilClose(t, c)
			assert.Equal(t, 4401, ce.Code)
			assert.Equal(t, conduitReasonAuthzExpired, ce.Text)
		})
	}
}
