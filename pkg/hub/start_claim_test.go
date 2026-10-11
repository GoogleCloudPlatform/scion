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
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// claimTestDispatcher is an AgentDispatcher whose start and stop are
// supplied by the test; every other method is unused.
type claimTestDispatcher struct {
	AgentDispatcher
	start func(ctx context.Context, a *store.Agent) error
	stops atomic.Int32
}

func (d *claimTestDispatcher) DispatchAgentStart(ctx context.Context, a *store.Agent, task string, resume bool) error {
	if d.start == nil {
		return nil
	}
	return d.start(ctx, a)
}

func (d *claimTestDispatcher) DispatchAgentStop(ctx context.Context, a *store.Agent) error {
	d.stops.Add(1)
	return nil
}

// fastClaims shortens a claim run's lease timing so lease behaviour is
// observable in milliseconds.
func fastClaims(srv *Server, ttl time.Duration) {
	srv.startClaimTestHook = func(r *startClaimRun) {
		r.cfg.LeaseTTL = ttl
		r.fenceAt = time.Now().Add(ttl - ttl/3)
		r.renewEvery = ttl / 3
		r.retryEvery = ttl / 10
	}
}

func newClaimFixture(t *testing.T) (*reconcileFixture, *claimTestDispatcher, *store.Agent) {
	t.Helper()
	f := newReconcileFixture(t)
	d := &claimTestDispatcher{}
	f.srv.SetDispatcher(d)
	f.srv.startClaimsOn = true
	a := f.addAgent("claimed", "stopped", "")
	return f, d, a
}

func getAgent(t *testing.T, s store.Store, id string) *store.Agent {
	t.Helper()
	a, err := s.GetAgent(context.Background(), id)
	require.NoError(t, err)
	return a
}

func TestStartClaim_DisabledRecordsIntentAndTakesNoClaim(t *testing.T) {
	f, d, a := newClaimFixture(t)
	f.srv.startClaimsOn = false
	called := false
	d.start = func(ctx context.Context, cur *store.Agent) error {
		called = true
		assert.Empty(t, getAgent(t, f.s, a.ID).StartClaimID)
		return nil
	}
	require.NoError(t, f.srv.startAgentCore(context.Background(), a, StartOpts{Kind: store.StartClaimUser}))
	assert.True(t, called)
	assert.Equal(t, store.RunIntentRunning, getAgent(t, f.s, a.ID).RunIntent)
}

func TestStartClaim_SuccessHoldsClaimDuringDispatchThenReleases(t *testing.T) {
	f, d, a := newClaimFixture(t)
	d.start = func(ctx context.Context, cur *store.Agent) error {
		got := getAgent(t, f.s, a.ID)
		assert.Equal(t, store.StartClaimLive, got.StartClaimState, "the claim is held while the start is dispatched")
		assert.Equal(t, store.StartClaimUser, got.StartClaimKind)
		assert.Equal(t, "docker", got.StartClaimTarget)
		assert.Equal(t, f.srv.instanceID, got.StartClaimOwner)
		return nil
	}
	require.NoError(t, f.srv.startAgentCore(context.Background(), a, StartOpts{Kind: store.StartClaimUser}))
	got := getAgent(t, f.s, a.ID)
	assert.Empty(t, got.StartClaimID, "a successful start releases its claim")
	assert.Equal(t, store.RunIntentRunning, got.RunIntent)
	assert.Equal(t, int32(0), d.stops.Load())
}

func TestStartClaim_OutcomeByError(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want store.StartClaimState // "" = released
	}{
		{"timeout keeps the claim unconfirmed", context.DeadlineExceeded, store.StartClaimUnconfirmed},
		{"unreadable response keeps it", errors.New("decode response: unexpected EOF"), store.StartClaimUnconfirmed},
		{"request never sent releases", fmt.Errorf("x: %w", errStartRequestNotSent), ""},
		{"broker not connected releases", fmt.Errorf("x: %w", errStartBrokerNotConnected), ""},
		{"launch in flight releases", ErrLaunchInFlight, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f, d, a := newClaimFixture(t)
			d.start = func(ctx context.Context, cur *store.Agent) error { return tc.err }
			err := f.srv.startAgentCore(context.Background(), a, StartOpts{Kind: store.StartClaimCreate})
			require.ErrorIs(t, err, tc.err)
			got := getAgent(t, f.s, a.ID)
			assert.Equal(t, tc.want, got.StartClaimState)
			if tc.want == store.StartClaimUnconfirmed {
				require.NotNil(t, got.StartClaimHoldUntil)
				assert.Equal(t, f.srv.startClaimSettings().CreateUnconfirmedHold, got.StartClaimHoldUntil.Sub(*got.StartClaimUnconfirmedAt), "a create claim uses the create hold")
			}
		})
	}
}

func TestStartClaim_HeldClaimRefusesWithoutDispatch(t *testing.T) {
	f, d, a := newClaimFixture(t)
	_, err := f.s.ClaimAgentStart(context.Background(), a.ID, "other-hub", store.StartClaimRecovery, "", time.Minute)
	require.NoError(t, err)
	d.start = func(ctx context.Context, cur *store.Agent) error {
		t.Fatal("a start must not be dispatched while another claim is held")
		return nil
	}
	err = f.srv.startAgentCore(context.Background(), a, StartOpts{Kind: store.StartClaimUser})
	var held *store.ClaimHeldError
	require.ErrorAs(t, err, &held)
	assert.Equal(t, store.StartClaimRecovery, held.Kind)
}

func TestStartClaim_TwoReplicasOneWins(t *testing.T) {
	f, d, a := newClaimFixture(t)
	other := newTestServerFromStore(t, f.s, nil) // a second replica on the same store
	other.instanceID = "replica-b"
	other.SetDispatcher(d)
	other.startClaimsOn = true
	release := make(chan struct{})
	var dispatched atomic.Int32
	d.start = func(ctx context.Context, cur *store.Agent) error {
		dispatched.Add(1)
		<-release
		return nil
	}
	var wg sync.WaitGroup
	errs := make([]error, 2)
	for i, srv := range []*Server{f.srv, other} {
		wg.Add(1)
		go func(i int, srv *Server) {
			defer wg.Done()
			cp := *a
			errs[i] = srv.startAgentCore(context.Background(), &cp, StartOpts{Kind: store.StartClaimUser})
		}(i, srv)
	}
	require.Eventually(t, func() bool { return dispatched.Load() == 1 }, 5*time.Second, 5*time.Millisecond)
	time.Sleep(50 * time.Millisecond)
	close(release)
	wg.Wait()
	assert.Equal(t, int32(1), dispatched.Load(), "only one replica dispatches")
	held := 0
	for _, err := range errs {
		var h *store.ClaimHeldError
		if errors.As(err, &h) {
			held++
		}
	}
	assert.Equal(t, 1, held)
}

// stallingRenewStore blocks RenewAgentStart until its context ends, or
// answers from a script.
type stallingRenewStore struct {
	store.Store
	mu     sync.Mutex
	script []func() (bool, error) // consumed in order; when empty, stall
}

func (s *stallingRenewStore) RenewAgentStart(ctx context.Context, agentID, claimID, owner string, ttl time.Duration) (bool, error) {
	s.mu.Lock()
	var next func() (bool, error)
	if len(s.script) > 0 {
		next, s.script = s.script[0], s.script[1:]
	}
	s.mu.Unlock()
	if next != nil {
		return next()
	}
	<-ctx.Done()
	return false, ctx.Err()
}

// A holder whose renew stalls past its fence deadline treats the claim as
// lost: its start is cancelled and it writes no outcome, so the claim stays
// as the reaper (or a stop) left it.
func TestStartClaim_StalledRenewFencesTheHolder(t *testing.T) {
	f, d, a := newClaimFixture(t)
	f.srv.store = &stallingRenewStore{Store: f.s}
	fastClaims(f.srv, 300*time.Millisecond)
	var startErr error
	d.start = func(ctx context.Context, cur *store.Agent) error {
		<-ctx.Done()
		startErr = ctx.Err()
		return ctx.Err()
	}
	err := f.srv.startAgentCore(context.Background(), a, StartOpts{Kind: store.StartClaimRecovery})
	require.ErrorIs(t, err, errStartClaimLost)
	assert.ErrorIs(t, startErr, context.Canceled, "the start is cancelled at the fence deadline")
	got := getAgent(t, f.s, a.ID)
	assert.Equal(t, store.StartClaimLive, got.StartClaimState, "a fenced holder writes no outcome")
}

// Past the fence deadline before dispatch, the start is not dispatched.
func TestStartClaim_FencedBeforeDispatchDoesNotDispatch(t *testing.T) {
	f, d, a := newClaimFixture(t)
	f.srv.startClaimTestHook = func(r *startClaimRun) { r.fenceAt = time.Now().Add(-time.Second) }
	d.start = func(ctx context.Context, cur *store.Agent) error {
		t.Fatal("dispatched past the fence deadline")
		return nil
	}
	err := f.srv.startAgentCore(context.Background(), a, StartOpts{Kind: store.StartClaimUser})
	require.ErrorIs(t, err, errStartClaimLost)
	assert.Equal(t, store.StartClaimLive, getAgent(t, f.s, a.ID).StartClaimState, "no outcome is written")
}

func TestStartClaim_ZeroRowsRenewCancelsAtOnce(t *testing.T) {
	f, d, a := newClaimFixture(t)
	f.srv.store = &stallingRenewStore{Store: f.s, script: []func() (bool, error){func() (bool, error) { return false, nil }}}
	fastClaims(f.srv, 3*time.Second)
	started := time.Now()
	d.start = func(ctx context.Context, cur *store.Agent) error {
		<-ctx.Done()
		return ctx.Err()
	}
	err := f.srv.startAgentCore(context.Background(), a, StartOpts{Kind: store.StartClaimUser})
	require.ErrorIs(t, err, errStartClaimLost)
	assert.Less(t, time.Since(started), 2*time.Second, "a lost claim cancels at the first renew, before the fence deadline")
}

func TestStartClaim_RenewErrorsThenSuccessKeepsClaim(t *testing.T) {
	f, d, a := newClaimFixture(t)
	boom := func() (bool, error) { return false, errors.New("db busy") }
	ok := func() (bool, error) { return true, nil }
	f.srv.store = &stallingRenewStore{Store: f.s, script: []func() (bool, error){boom, boom, ok, ok, ok, ok, ok, ok, ok, ok}}
	fastClaims(f.srv, 600*time.Millisecond)
	d.start = func(ctx context.Context, cur *store.Agent) error {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(700 * time.Millisecond): // longer than one lease
			return nil
		}
	}
	require.NoError(t, f.srv.startAgentCore(context.Background(), a, StartOpts{Kind: store.StartClaimUser}))
	assert.Empty(t, getAgent(t, f.s, a.ID).StartClaimID, "the claim was kept and then released on success")
}

func TestStartClaim_MaxDurationBoundsTheStart(t *testing.T) {
	f, d, a := newClaimFixture(t)
	f.srv.startClaimTestHook = func(r *startClaimRun) { r.cfg.MaxDuration = 100 * time.Millisecond }
	d.start = func(ctx context.Context, cur *store.Agent) error {
		<-ctx.Done() // a cross-node wait that never answers
		return ctx.Err()
	}
	err := f.srv.startAgentCore(context.Background(), a, StartOpts{Kind: store.StartClaimUser})
	require.ErrorIs(t, err, context.DeadlineExceeded)
	assert.Equal(t, store.StartClaimUnconfirmed, getAgent(t, f.s, a.ID).StartClaimState, "a start cut off by the deadline is unconfirmed")
}

func TestStartClaim_CompensatingStop(t *testing.T) {
	f, d, a := newClaimFixture(t)
	d.start = func(ctx context.Context, cur *store.Agent) error {
		_, err := f.s.SetRunIntent(context.Background(), a.ID, store.RunIntentStopped) // a stop accepted meanwhile
		return err
	}
	require.NoError(t, f.srv.startAgentCore(context.Background(), a, StartOpts{Kind: store.StartClaimUser}))
	assert.Equal(t, int32(1), d.stops.Load(), "a start that completed after a stop was accepted is stopped")
	assert.Empty(t, getAgent(t, f.s, a.ID).StartClaimID, "the compensating stop releases its stop claim")
}

func TestStartClaim_OutcomeClassification(t *testing.T) {
	assert.Equal(t, startReleased, startOutcomeOf(nil))
	assert.Equal(t, startReleased, startOutcomeOf(&AgentCreateIncompleteError{}))
	assert.Equal(t, startReleased, startOutcomeOf(fmt.Errorf("w: %w", errBrokerLacksEmptyPerAgent)))
	assert.Equal(t, startUnconfirmed, startOutcomeOf(context.Canceled))
	assert.Equal(t, startUnconfirmed, startOutcomeOf(errors.New("request timeout after 120s")))
}

func TestStartClaim_ReincarnateRefusedWhileClaimHeld(t *testing.T) {
	disp := newReincarnateTestDispatcher()
	srv, s, project, broker := setupReincarnateTestServer(t, disp)
	agent := newReincarnateTestAgent(t, s, project, broker, nil)
	_, err := s.ClaimAgentStart(context.Background(), agent.ID, "hub", store.StartClaimUser, "", time.Minute)
	require.NoError(t, err)
	self := agentIdentityFor(agent.ID, project.ID)
	rec := httptest.NewRecorder()
	srv.handleReincarnateAgent(rec, reincarnateRequest(t, agent.ID, self, ReincarnateAgentRequest{Handoff: "h"}), agent.ID)
	assert.Equal(t, http.StatusConflict, rec.Code, rec.Body.String())
	list, err := s.ListAgentReincarnations(context.Background(), agent.ID)
	require.NoError(t, err)
	assert.Empty(t, list)
}

// startBeforeStopClaimStore takes a start claim (a newer start) just before
// the compensating stop's claim.
type startBeforeStopClaimStore struct {
	store.Store
}

func (s startBeforeStopClaimStore) ClaimAgentStop(ctx context.Context, agentID, owner string, intentAt time.Time, ttl time.Duration) (store.StartClaim, error) {
	if _, err := s.ClaimAgentStart(ctx, agentID, "newer-hub", store.StartClaimUser, "", time.Minute); err != nil {
		return store.StartClaim{}, err
	}
	return s.Store.ClaimAgentStop(ctx, agentID, owner, intentAt, ttl)
}

// The compensating stop is fenced: a start accepted after the stop's intent
// was read is never stopped by it.
func TestStartClaim_CompensatingStopSkipsNewerStart(t *testing.T) {
	f, d, a := newClaimFixture(t)
	d.start = func(ctx context.Context, cur *store.Agent) error {
		_, err := f.s.SetRunIntent(context.Background(), a.ID, store.RunIntentStopped)
		return err
	}
	f.srv.store = startBeforeStopClaimStore{Store: f.s}
	require.NoError(t, f.srv.startAgentCore(context.Background(), a, StartOpts{Kind: store.StartClaimUser}))
	assert.Equal(t, int32(0), d.stops.Load())
}

// A dispatch that ignores its context does not keep the claim renewed past
// the start deadline: renewal stops, the start reports the claim lost, and
// the lapsed lease is left to the reaper.
func TestStartClaim_RenewalStopsAtStartDeadline(t *testing.T) {
	f, d, a := newClaimFixture(t)
	f.srv.startClaimTestHook = func(r *startClaimRun) {
		r.cfg.LeaseTTL = 600 * time.Millisecond
		r.fenceAt = time.Now().Add(400 * time.Millisecond)
		r.renewEvery = 100 * time.Millisecond
		r.retryEvery = 50 * time.Millisecond
		r.cfg.MaxDuration = 150 * time.Millisecond
	}
	d.start = func(ctx context.Context, cur *store.Agent) error {
		time.Sleep(1500 * time.Millisecond) // ignores ctx
		return nil
	}
	err := f.srv.startAgentCore(context.Background(), a, StartOpts{Kind: store.StartClaimUser})
	require.ErrorIs(t, err, errStartClaimLost)
	got := getAgent(t, f.s, a.ID)
	require.NotNil(t, got.StartClaimLeaseUntil)
	assert.True(t, got.StartClaimLeaseUntil.Before(time.Now()), "the lease was not renewed past the deadline")
	assert.Equal(t, store.StartClaimLive, got.StartClaimState, "left for the reaper to demote")
}
