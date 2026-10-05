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
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// stopHookDispatcher is a claimTestDispatcher whose stop also runs a hook.
type stopHookDispatcher struct {
	*claimTestDispatcher
	onStop func(ctx context.Context, a *store.Agent)
}

func (d *stopHookDispatcher) DispatchAgentStop(ctx context.Context, a *store.Agent) error {
	if d.onStop != nil {
		d.onStop(ctx, a)
	}
	return d.claimTestDispatcher.DispatchAgentStop(ctx, a)
}

func lifecycle(t *testing.T, f *reconcileFixture, id, action string) (int, map[string]interface{}) {
	t.Helper()
	rec := doRequest(t, f.srv, http.MethodPost, "/api/v1/agents/"+id+"/"+action, nil)
	var body map[string]interface{}
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	return rec.Code, body
}

func errorDetails(body map[string]interface{}) (string, map[string]interface{}) {
	e, _ := body["error"].(map[string]interface{})
	code, _ := e["code"].(string)
	details, _ := e["details"].(map[string]interface{})
	return code, details
}

func TestStartClaimWiring_StartRefusedWithStartInProgress(t *testing.T) {
	f, d, a := newClaimFixture(t)
	_, err := f.s.ClaimAgentStart(context.Background(), a.ID, "other-hub", store.StartClaimRecovery, "", time.Minute)
	require.NoError(t, err)
	d.start = func(ctx context.Context, cur *store.Agent) error {
		t.Fatal("dispatched while another start holds the claim")
		return nil
	}
	code, body := lifecycle(t, f, a.ID, "start")
	require.Equal(t, http.StatusConflict, code, body)
	errCode, details := errorDetails(body)
	assert.Equal(t, ErrCodeStartInProgress, errCode)
	assert.Equal(t, "recovery", details["holderKind"])
	assert.Equal(t, "live", details["state"])
	assert.NotEmpty(t, details["expectedRelease"])
}

func TestStartClaimWiring_RestartHoldsClaimAcrossStopLeg(t *testing.T) {
	f, base, a := newClaimFixture(t)
	d := &stopHookDispatcher{claimTestDispatcher: base}
	f.srv.SetDispatcher(d)
	var kindAtStop store.StartClaimKind
	d.onStop = func(ctx context.Context, cur *store.Agent) {
		kindAtStop = getAgent(t, f.s, a.ID).StartClaimKind
	}
	code, body := lifecycle(t, f, a.ID, "restart")
	require.Equal(t, http.StatusOK, code, body)
	assert.Equal(t, store.StartClaimRestart, kindAtStop, "the restart claim is held before the stop leg")
	assert.Empty(t, getAgent(t, f.s, a.ID).StartClaimID, "released after the start leg")
}

func TestStartClaimWiring_RestartRacingAClaimIsRefusedBeforeStopping(t *testing.T) {
	f, d, a := newClaimFixture(t)
	_, err := f.s.ClaimAgentStart(context.Background(), a.ID, "other-hub", store.StartClaimUser, "", time.Minute)
	require.NoError(t, err)
	code, body := lifecycle(t, f, a.ID, "restart")
	require.Equal(t, http.StatusConflict, code, body)
	assert.Equal(t, int32(0), d.stops.Load(), "the agent is not stopped when its restart cannot claim")
}

// A stop whose dispatch succeeded releases the claim it superseded, of
// every kind and state, and a following start is not refused.
func TestStartClaimWiring_StopReleasesSupersededClaim(t *testing.T) {
	for _, kind := range []store.StartClaimKind{store.StartClaimUser, store.StartClaimRestart, store.StartClaimWake,
		store.StartClaimCreate, store.StartClaimRecovery, store.StartClaimReincarnate} {
		t.Run(string(kind), func(t *testing.T) {
			f, _, a := newClaimFixture(t)
			c, err := f.s.ClaimAgentStart(context.Background(), a.ID, "other-hub", kind, "", time.Minute)
			require.NoError(t, err)
			_, err = f.s.MarkStartUnconfirmed(context.Background(), a.ID, c.ID, "other-hub", time.Hour)
			require.NoError(t, err)

			code, body := lifecycle(t, f, a.ID, "stop")
			require.Equal(t, http.StatusOK, code, body)
			assert.Empty(t, getAgent(t, f.s, a.ID).StartClaimID, "the stop released the claim it superseded")
			code, body = lifecycle(t, f, a.ID, "start")
			assert.Equal(t, http.StatusOK, code, body)
		})
	}
}

// A stop during a start still in flight releases the start's claim; the
// holder finds it gone, abandons the start (its context is cancelled), and
// no start outlives the stop.
func TestStartClaimWiring_StopDuringInFlightStart(t *testing.T) {
	f, d, a := newClaimFixture(t)
	fastClaims(f.srv, 300*time.Millisecond)
	started := make(chan struct{})
	var startErr atomic.Value
	d.start = func(ctx context.Context, cur *store.Agent) error {
		close(started)
		<-ctx.Done()
		startErr.Store(ctx.Err())
		return ctx.Err()
	}
	done := make(chan int, 1)
	go func() {
		code, _ := lifecycle(t, f, a.ID, "start")
		done <- code
	}()
	<-started
	code, body := lifecycle(t, f, a.ID, "stop")
	require.Equal(t, http.StatusOK, code, body)
	select {
	case code := <-done:
		assert.Equal(t, http.StatusConflict, code, "the superseded start is abandoned")
	case <-time.After(10 * time.Second):
		t.Fatal("the in-flight start was not abandoned after the stop")
	}
	assert.ErrorIs(t, startErr.Load().(error), context.Canceled)
	got := getAgent(t, f.s, a.ID)
	assert.Equal(t, store.RunIntentStopped, got.RunIntent)
	assert.Empty(t, got.StartClaimID)
}

// The hub stops an agent the heartbeat lists as running whose run intent
// is stopped and that holds no start claim; not one whose intent is running.
func TestStartClaimWiring_BackstopStopsRunningAgentWithIntentStopped(t *testing.T) {
	f, d, a := newClaimFixture(t)
	ctx := context.Background()
	other := f.addAgent("wanted", "running", "working")
	_, err := f.s.SetRunIntent(ctx, other.ID, store.RunIntentRunning)
	require.NoError(t, err)
	_, err = f.s.SetRunIntent(ctx, a.ID, store.RunIntentStopped)
	require.NoError(t, err)

	f.heartbeat(completeInventory(), a.Slug, other.Slug)
	require.Eventually(t, func() bool { return d.stops.Load() == 1 }, 5*time.Second, 10*time.Millisecond)
	time.Sleep(50 * time.Millisecond)
	assert.Equal(t, int32(1), d.stops.Load(), "only the agent with intent stopped is stopped")
	assert.Empty(t, getAgent(t, f.s, a.ID).StartClaimID, "the backstop's stop claim is released")

	f.heartbeat(completeInventory(), a.Slug, other.Slug)
	time.Sleep(50 * time.Millisecond)
	assert.Equal(t, int32(1), d.stops.Load(), "rate-limited to one per heartbeat interval")
}

// A failed delete that restores a running agent restores its intent too,
// so the backstop leaves it alone.
func TestStartClaimWiring_DeleteRollbackIsNotStoppedByBackstop(t *testing.T) {
	f, base, a := newClaimFixture(t)
	ctx := context.Background()
	running := f.addAgent("restored", "running", "working")
	_, err := f.s.SetRunIntent(ctx, running.ID, store.RunIntentRunning)
	require.NoError(t, err)
	f.srv.SetDispatcher(&failingDeleteClaimDispatcher{claimTestDispatcher: base})
	_ = a

	rec := doRequest(t, f.srv, http.MethodDelete, "/api/v1/agents/"+running.ID, nil)
	require.Equal(t, http.StatusBadGateway, rec.Code, rec.Body.String())
	got := getAgent(t, f.s, running.ID)
	require.Equal(t, store.RunIntentRunning, got.RunIntent)

	f.heartbeat(completeInventory(), running.Slug)
	time.Sleep(100 * time.Millisecond)
	assert.Equal(t, int32(0), base.stops.Load(), "a restored running agent is not stopped")
}

type failingDeleteClaimDispatcher struct {
	*claimTestDispatcher
}

func (d *failingDeleteClaimDispatcher) DispatchAgentDelete(context.Context, *store.Agent, bool, bool, bool, time.Time) error {
	return errors.New("broker refused")
}

func TestStartClaimWiring_WakeDefersWhileAnotherStartHoldsTheClaim(t *testing.T) {
	f, d, _ := newClaimFixture(t)
	a := f.addAgent("sleepy", "suspended", "")
	_, err := f.s.ClaimAgentStart(context.Background(), a.ID, "other-hub", store.StartClaimUser, "", time.Minute)
	require.NoError(t, err)
	d.start = func(ctx context.Context, cur *store.Agent) error {
		t.Fatal("dispatched while another start holds the claim")
		return nil
	}
	res, dmErr := f.srv.wakeAgentForDM(context.Background(), getAgent(t, f.s, a.ID))
	require.Nil(t, dmErr)
	require.NotNil(t, res)
	assert.Equal(t, WakeDeferred, res.Outcome)
	assert.Equal(t, "agent is already starting", deferredReason(getAgent(t, f.s, a.ID)))
}

func TestStartClaimWiring_QueuedStopDrainReleasesSupersededClaim(t *testing.T) {
	f, _, a := newClaimFixture(t)
	ctx := context.Background()
	c, err := f.s.ClaimAgentStart(ctx, a.ID, "other-hub", store.StartClaimRecovery, "", time.Minute)
	require.NoError(t, err)
	_, err = f.s.MarkStartUnconfirmed(ctx, a.ID, c.ID, "other-hub", time.Hour)
	require.NoError(t, err)
	at, err := f.s.SetRunIntent(ctx, a.ID, store.RunIntentStopped)
	require.NoError(t, err)
	args, err := MarshalDispatchArgs(StopDispatchArgs{IntentAt: &at, SupersedesClaim: c.ID})
	require.NoError(t, err)
	_, err = f.srv.execDispatchStop(ctx, store.BrokerDispatch{ID: tid("drain"), BrokerID: f.brokerID, AgentID: a.ID, AgentSlug: a.Slug, ProjectID: f.projectID, Op: "stop", Args: args})
	require.NoError(t, err)
	assert.Empty(t, getAgent(t, f.s, a.ID).StartClaimID)
}

func TestStartClaimWiring_ReincarnateRecordsIntentRunning(t *testing.T) {
	disp := newReincarnateTestDispatcher()
	srv, s, project, broker := setupReincarnateTestServer(t, disp)
	agent := newReincarnateTestAgent(t, s, project, broker, nil)
	_, err := s.SetRunIntent(context.Background(), agent.ID, store.RunIntentStopped)
	require.NoError(t, err)
	self := agentIdentityFor(agent.ID, project.ID)
	rec := httptest.NewRecorder()
	srv.handleReincarnateAgent(rec, reincarnateRequest(t, agent.ID, self, ReincarnateAgentRequest{Handoff: "h"}), agent.ID)
	require.Equal(t, http.StatusAccepted, rec.Code, rec.Body.String())
	got, err := s.GetAgent(context.Background(), agent.ID)
	require.NoError(t, err)
	assert.Equal(t, store.RunIntentRunning, got.RunIntent, "a reincarnation means the agent is meant to run, so the backstop does not stop it")
}

func TestStartClaimWiring_ServerTakesClaimsByDefault(t *testing.T) {
	srv, _ := testServer(t)
	assert.True(t, srv.startClaimsEnabled(), "a server built by New runs every start under a start claim")
}

// startJustBeforeBackstopStore records a start (a newer intent and claim)
// just before the backstop takes its stop claim.
type startJustBeforeBackstopStore struct {
	store.Store
}

func (s startJustBeforeBackstopStore) ClaimAgentStop(ctx context.Context, agentID, owner string, intentAt time.Time, ttl time.Duration) (store.StartClaim, error) {
	if _, err := s.ClaimAgentStart(ctx, agentID, "user-hub", store.StartClaimUser, "", time.Minute); err != nil {
		return store.StartClaim{}, err
	}
	return s.Store.ClaimAgentStop(ctx, agentID, owner, intentAt, ttl)
}

// The backstop's stop claim is pinned to the intent the heartbeat saw: a
// start accepted after the heartbeat listed the agent is never stopped.
func TestStartClaimWiring_BackstopSkipsStartAcceptedMeanwhile(t *testing.T) {
	f, d, a := newClaimFixture(t)
	_, err := f.s.SetRunIntent(context.Background(), a.ID, store.RunIntentStopped)
	require.NoError(t, err)
	f.srv.store = startJustBeforeBackstopStore{Store: f.s}
	f.heartbeat(completeInventory(), a.Slug)
	time.Sleep(200 * time.Millisecond)
	assert.Equal(t, int32(0), d.stops.Load())
}

// A start runs to its outcome even when its request's context ends.
func TestStartClaimWiring_StartSurvivesRequestCancel(t *testing.T) {
	f, d, a := newClaimFixture(t)
	reqCtx, cancel := context.WithCancel(context.Background())
	var sawCancel bool
	d.start = func(ctx context.Context, cur *store.Agent) error {
		cancel() // the client disconnects mid-dispatch
		time.Sleep(20 * time.Millisecond)
		sawCancel = ctx.Err() != nil
		return nil
	}
	require.NoError(t, f.srv.startAgentCore(reqCtx, a, StartOpts{Kind: store.StartClaimUser}))
	assert.False(t, sawCancel, "the dispatch is not cancelled with the request")
	assert.Empty(t, getAgent(t, f.s, a.ID).StartClaimID)
}

// startAgentCore owns the start's broker capacity: it reserves before the
// dispatch and rolls back a reservation it made when the start fails; a
// broker at capacity is refused with 429 before anything is dispatched.
func TestStartClaimWiring_CapacityReservedAndRolledBack(t *testing.T) {
	f, d, a := newClaimFixture(t)
	setBrokerAgentCeiling(t, f.s, 1)
	ctx := context.Background()
	def, err := f.s.GetLimitDefinitionByName(ctx, store.LimitMaxAgentsPerBroker)
	require.NoError(t, err)

	var heldDuringDispatch bool
	d.start = func(ctx context.Context, cur *store.Agent) error {
		heldDuringDispatch, _ = f.s.HasActiveReservation(context.Background(), def.ID, a.ID)
		return fmt.Errorf("x: %w", errStartRequestNotSent)
	}
	code, _ := lifecycle(t, f, a.ID, "start")
	require.NotEqual(t, http.StatusOK, code)
	assert.True(t, heldDuringDispatch, "capacity is reserved before the dispatch")
	has, err := f.s.HasActiveReservation(ctx, def.ID, a.ID)
	require.NoError(t, err)
	assert.False(t, has, "a reservation made by a failed start is rolled back")

	// Another agent takes the only slot: this start is refused with 429.
	other := f.addAgent("slot-taker", "running", "working")
	_, err = f.srv.checkAndReserveBrokerQuota(ctx, other)
	require.NoError(t, err)
	d.start = func(ctx context.Context, cur *store.Agent) error {
		t.Fatal("dispatched past the broker's capacity")
		return nil
	}
	code, body := lifecycle(t, f, a.ID, "start")
	assert.Equal(t, http.StatusTooManyRequests, code, body)
	assert.Empty(t, getAgent(t, f.s, a.ID).StartClaimID, "the claim is released")
}

// statusSpyStore records whether the agent still held a start claim when
// its started status was written.
type statusSpyStore struct {
	store.Store
	t          *testing.T
	agentID    string
	claimHeld  *bool
	phaseWrite *bool
}

func (s statusSpyStore) UpdateAgentStatus(ctx context.Context, id string, u store.AgentStatusUpdate) error {
	if id == s.agentID && u.Phase == "running" {
		cur, err := s.GetAgent(ctx, id)
		require.NoError(s.t, err)
		*s.phaseWrite = true
		*s.claimHeld = cur.StartClaimID != ""
	}
	return s.Store.UpdateAgentStatus(ctx, id, u)
}

// The started status is written while the start's claim is held, and the
// missing-container reconcile is kept away during the dispatch.
func TestStartClaimWiring_StatusWrittenUnderClaimAndLifecycleOpHeld(t *testing.T) {
	f, d, a := newClaimFixture(t)
	var claimHeld, phaseWrite, opActive bool
	f.srv.store = statusSpyStore{Store: f.s, t: t, agentID: a.ID, claimHeld: &claimHeld, phaseWrite: &phaseWrite}
	d.start = func(ctx context.Context, cur *store.Agent) error {
		opActive = f.srv.lifecycleOps.active(a.ID)
		return nil
	}
	require.NoError(t, f.srv.startAgentCore(context.Background(), a, StartOpts{Kind: store.StartClaimUser}))
	assert.True(t, phaseWrite)
	assert.True(t, claimHeld, "the started status is written before the claim is released")
	assert.True(t, opActive, "the lifecycle op is held during the dispatch")
	assert.Equal(t, "running", getAgent(t, f.s, a.ID).Phase)
}

// An agent whose stopped intent was written by earlier code (here the boot
// backfill, which never sets the marker) is logged, not stopped, by the
// backstop; one written by claim-aware code is stopped.
func TestStartClaimWiring_BackstopLeavesLegacyStoppedIntent(t *testing.T) {
	f, d, _ := newClaimFixture(t)
	ctx := context.Background()
	legacy := f.addAgent("legacy", "error", "") // backfill: not running -> stopped
	_, err := f.s.BackfillRunIntent(ctx)
	require.NoError(t, err)
	got := getAgent(t, f.s, legacy.ID)
	require.Equal(t, store.RunIntentStopped, got.RunIntent)
	require.False(t, got.RunIntentWrittenWithClaims())

	f.heartbeat(completeInventory(), legacy.Slug)
	time.Sleep(200 * time.Millisecond)
	assert.Equal(t, int32(0), d.stops.Load(), "a running agent with an older stopped intent is not stopped")

	_, err = f.s.SetRunIntent(ctx, legacy.ID, store.RunIntentStopped) // a claim-aware stop
	require.NoError(t, err)
	f.srv.intentStops.Delete(legacy.ID)
	f.heartbeat(completeInventory(), legacy.Slug)
	require.Eventually(t, func() bool { return d.stops.Load() == 1 }, 5*time.Second, 10*time.Millisecond)
}

// failRunningStatusStore fails the started-status write.
type failRunningStatusStore struct {
	store.Store
	agentID string
}

func (s failRunningStatusStore) UpdateAgentStatus(ctx context.Context, id string, u store.AgentStatusUpdate) error {
	if id == s.agentID && u.ClearExit {
		return errors.New("database is locked")
	}
	return s.Store.UpdateAgentStatus(ctx, id, u)
}

// A restart whose start leg succeeded but whose status write failed is not
// a failed restart: the reservation is kept and no stopped state is
// recorded.
func TestStartClaimWiring_RestartStatusWriteFailureIsNotAFailedStart(t *testing.T) {
	f, _, _ := newClaimFixture(t)
	a := f.addAgent("restarting-live", "running", "working")
	ctx := context.Background()
	setBrokerAgentCeiling(t, f.s, 5)
	def, err := f.s.GetLimitDefinitionByName(ctx, store.LimitMaxAgentsPerBroker)
	require.NoError(t, err)
	f.srv.store = failRunningStatusStore{Store: f.s, agentID: a.ID}
	code, _ := lifecycle(t, f, a.ID, "restart")
	assert.NotEqual(t, http.StatusBadGateway, code, "not answered as a failed dispatch")
	has, err := f.s.HasActiveReservation(ctx, def.ID, a.ID)
	require.NoError(t, err)
	assert.True(t, has, "the running agent keeps its reservation")
	assert.NotEqual(t, "stopped", getAgent(t, f.s, a.ID).Phase, "no stopped state is recorded for a started container")
}

// A start whose claim is lost before it dispatches is abandoned and undoes
// its capacity hold: the reservation it made is released and the starting
// phase restored, on a detached context (a database refuses work on a done
// one), whether the claim is lost while the reservation is being made or
// after the starting write, at the fence.
func TestStartClaimWiring_FenceFailureRollsBackCapacity(t *testing.T) {
	cases := []struct {
		name  string
		store func(s store.Store) ctxHonouringReleaseStore
	}{
		{"lost during the reservation", func(s store.Store) ctxHonouringReleaseStore {
			return ctxHonouringReleaseStore{Store: s, reserveDelay: 400 * time.Millisecond}
		}},
		{"lost after the starting write", func(s store.Store) ctxHonouringReleaseStore {
			return ctxHonouringReleaseStore{Store: s, startingDelay: 400 * time.Millisecond}
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f, d, a := newClaimFixture(t)
			ctx := context.Background()
			setBrokerAgentCeiling(t, f.s, 5)
			def, err := f.s.GetLimitDefinitionByName(ctx, store.LimitMaxAgentsPerBroker)
			require.NoError(t, err)
			// The fence passes while the delayed store call runs: the claim
			// is lost (its run context cancelled) before the dispatch.
			f.srv.startClaimTestHook = func(r *startClaimRun) {
				r.fenceAt = time.Now().Add(150 * time.Millisecond)
				r.renewEvery = time.Hour
			}
			d.start = func(ctx context.Context, cur *store.Agent) error {
				t.Fatal("dispatched past the fence")
				return nil
			}
			f.srv.store = tc.store(f.s)
			f.srv.quotaService.store = f.srv.store
			err = f.srv.startAgentCore(ctx, a, StartOpts{Kind: store.StartClaimUser})
			require.ErrorIs(t, err, errStartClaimLost)
			has, err := f.s.HasActiveReservation(ctx, def.ID, a.ID)
			require.NoError(t, err)
			assert.False(t, has, "the reservation made for the abandoned start is released")
			assert.Equal(t, string(state.PhaseStopped), getAgent(t, f.s, a.ID).Phase, "the starting phase is restored")
		})
	}
}

// A failed start keeps a reservation the agent already held.
func TestStartClaimWiring_FailedStartKeepsPreheldCapacity(t *testing.T) {
	f, d, a := newClaimFixture(t)
	ctx := context.Background()
	setBrokerAgentCeiling(t, f.s, 5)
	_, err := f.srv.checkAndReserveBrokerQuota(ctx, a)
	require.NoError(t, err)
	def, err := f.s.GetLimitDefinitionByName(ctx, store.LimitMaxAgentsPerBroker)
	require.NoError(t, err)
	d.start = func(ctx context.Context, cur *store.Agent) error {
		return fmt.Errorf("x: %w", errStartRequestNotSent)
	}
	require.Error(t, f.srv.startAgentCore(ctx, a, StartOpts{Kind: store.StartClaimUser}))
	has, err := f.s.HasActiveReservation(ctx, def.ID, a.ID)
	require.NoError(t, err)
	assert.True(t, has, "a reservation the agent held before the start is kept")
}

// A start refused by a stop-kind claim says a stop is in progress.
func TestStartClaimWiring_StopHolderMessage(t *testing.T) {
	f, _, a := newClaimFixture(t)
	ctx := context.Background()
	at, err := f.s.SetRunIntent(ctx, a.ID, store.RunIntentStopped)
	require.NoError(t, err)
	_, err = f.s.ClaimAgentStop(ctx, a.ID, "drain-hub", at, time.Minute)
	require.NoError(t, err)
	code, body := lifecycle(t, f, a.ID, "start")
	require.Equal(t, http.StatusConflict, code)
	e, _ := body["error"].(map[string]interface{})
	assert.Contains(t, e["message"], "A stop is in progress")
}

// wakeStatusSpyStore records whether the wake's running write happened
// while the claim was held.
type wakeStatusSpyStore struct {
	store.Store
	t         *testing.T
	agentID   string
	claimHeld *bool
}

func (s wakeStatusSpyStore) UpdateAgentStatus(ctx context.Context, id string, u store.AgentStatusUpdate) error {
	if id == s.agentID && u.Phase == "running" {
		cur, err := s.GetAgent(ctx, id)
		require.NoError(s.t, err)
		*s.claimHeld = cur.StartClaimID != ""
	}
	return s.Store.UpdateAgentStatus(ctx, id, u)
}

// The wake writes running after readiness while its claim is held.
func TestStartClaimWiring_WakeRunningWrittenUnderClaim(t *testing.T) {
	f, d, _ := newClaimFixture(t)
	a := f.addAgent("waker", "suspended", "")
	var claimHeld bool
	f.srv.store = wakeStatusSpyStore{Store: f.s, t: t, agentID: a.ID, claimHeld: &claimHeld}
	d.start = func(ctx context.Context, cur *store.Agent) error {
		go func() { // the agent reports activity: ready
			time.Sleep(50 * time.Millisecond)
			_ = f.s.UpdateAgentStatus(context.Background(), a.ID, store.AgentStatusUpdate{Activity: "thinking"})
		}()
		return nil
	}
	res, dmErr := f.srv.wakeAgentForDM(context.Background(), getAgent(t, f.s, a.ID))
	require.Nil(t, dmErr)
	require.Equal(t, WakeResumed, res.Outcome)
	assert.True(t, claimHeld, "running is written before the wake's claim is released")
}

// ctxHonouringReleaseStore refuses a reservation release on a done context,
// as a real database does, and delays returning from a reservation or a
// starting write.
type ctxHonouringReleaseStore struct {
	store.Store
	reserveDelay  time.Duration // after a reservation is made
	startingDelay time.Duration // after a write of phase starting
}

func (s ctxHonouringReleaseStore) UpdateAgentStatus(ctx context.Context, id string, upd store.AgentStatusUpdate) error {
	err := s.Store.UpdateAgentStatus(ctx, id, upd)
	if upd.Phase == string(state.PhaseStarting) {
		time.Sleep(s.startingDelay)
	}
	return err
}

func (s ctxHonouringReleaseStore) TryAdvisoryLock(ctx context.Context, key store.AdvisoryLockKey) (bool, func() error, error) {
	return s.Store.(store.AdvisoryLocker).TryAdvisoryLock(ctx, key)
}

func (s ctxHonouringReleaseStore) TryAdvisoryLockObject(ctx context.Context, classID store.AdvisoryLockKey, objID int32) (bool, func() error, error) {
	return s.Store.(store.AdvisoryLocker).TryAdvisoryLockObject(ctx, classID, objID)
}

func (s ctxHonouringReleaseStore) CreateUsageReservation(ctx context.Context, r *store.UsageReservation) (*store.UsageReservation, error) {
	out, err := s.Store.CreateUsageReservation(ctx, r)
	time.Sleep(s.reserveDelay)
	return out, err
}

func (s ctxHonouringReleaseStore) ReleaseReservation(ctx context.Context, limitID, resourceID string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return s.Store.ReleaseReservation(ctx, limitID, resourceID)
}

// A live start claim defers a heartbeat's uncounted phase on any replica,
// except a stop's or a wake's claim; an unconfirmed claim does not.
func TestStartClaimWiring_HeartbeatGuardingClaim(t *testing.T) {
	cases := []struct {
		kind  store.StartClaimKind
		state store.StartClaimState
		want  bool
	}{
		{store.StartClaimUser, store.StartClaimLive, true},
		{store.StartClaimRestart, store.StartClaimLive, true},
		{store.StartClaimCreate, store.StartClaimLive, true},
		{store.StartClaimRecovery, store.StartClaimLive, true},
		{store.StartClaimReincarnate, store.StartClaimLive, true},
		{store.StartClaimStop, store.StartClaimLive, false},
		{store.StartClaimWake, store.StartClaimLive, false},
		{store.StartClaimUser, store.StartClaimUnconfirmed, false},
	}
	srv, _ := testServer(t)
	for _, tc := range cases {
		a := &store.Agent{ID: "agent-claim-guard", StartClaimID: "c", StartClaimKind: tc.kind, StartClaimState: tc.state}
		assert.Equal(t, tc.want, srv.heartbeatPhaseGuarded(a, "stopped"), "kind=%s state=%s", tc.kind, tc.state)
		assert.False(t, srv.heartbeatPhaseGuarded(a, "running"), "a counted phase is never guarded")
	}
	assert.False(t, srv.heartbeatPhaseGuarded(&store.Agent{ID: "agent-no-claim"}, "stopped"))
}

// A restart run by another replica holds its claim across the stop leg: a
// heartbeat this replica handles, reporting the old container stopped, does
// not move the row off running or release its reservation. Once the claim
// is released, the same report applies.
func TestStartClaimWiring_HeartbeatDeferredUnderAnotherReplicasClaim(t *testing.T) {
	f, _, a := newClaimFixture(t)
	ctx := context.Background()
	setBrokerAgentCeiling(t, f.s, 5)
	require.NoError(t, f.s.UpdateAgentStatus(ctx, a.ID, store.AgentStatusUpdate{Phase: string(state.PhaseRunning)}))
	_, err := f.srv.checkAndReserveBrokerQuota(ctx, a)
	require.NoError(t, err)
	claim, err := f.s.ClaimAgentStart(ctx, a.ID, "other-replica", store.StartClaimRestart, "docker", time.Minute)
	require.NoError(t, err)

	stopped := brokerHeartbeatRequest{
		Status:    store.BrokerStatusOnline,
		Inventory: completeInventory(),
		Projects: []brokerProjectHeartbeat{{ProjectID: f.projectID, Agents: []brokerAgentHeartbeat{
			{Slug: a.Slug, Phase: string(state.PhaseStopped), ContainerStatus: "Exited (0)", RuntimeTarget: "docker"},
		}}},
	}
	f.send(stopped)
	assert.Equal(t, string(state.PhaseRunning), getAgent(t, f.s, a.ID).Phase, "the stopped report is deferred under the claim")
	assert.True(t, hasReservation(t, f.s, store.LimitMaxAgentsPerBroker, a.ID), "the reservation is kept under the claim")

	_, err = f.s.ReleaseAgentStart(ctx, a.ID, claim.ID, "other-replica")
	require.NoError(t, err)
	f.send(stopped)
	assert.Equal(t, string(state.PhaseStopped), getAgent(t, f.s, a.ID).Phase, "without the claim the report applies")
}
