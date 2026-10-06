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
	"sync/atomic"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// queueStop puts a in the state an offline stop leaves: intent stopped,
// container status stop_queued with the notice, and a pending stop row
// carrying the intent time. It returns the intent time.
func queueStop(t *testing.T, f *reconcileFixture, a *store.Agent, supersedes string) time.Time {
	t.Helper()
	ctx := context.Background()
	at, err := f.s.SetRunIntent(ctx, a.ID, store.RunIntentStopped)
	require.NoError(t, err)
	require.NoError(t, f.s.UpdateAgentStatus(ctx, a.ID, store.AgentStatusUpdate{
		Phase: "stopped", ContainerStatus: containerStatusStopQueued, Message: offlineStopMessage,
	}))
	args, err := MarshalDispatchArgs(StopDispatchArgs{IntentAt: &at, SupersedesClaim: supersedes})
	require.NoError(t, err)
	require.NoError(t, f.s.InsertBrokerDispatch(ctx, &store.BrokerDispatch{
		ID: tid("qs-" + a.Slug), BrokerID: f.brokerID, AgentID: a.ID, AgentSlug: a.Slug, ProjectID: f.projectID, Op: "stop", Args: args,
	}))
	return at
}

// httpOnlyBroker gives the fixture's broker an endpoint and no control
// channel, as a broker reached over HTTP only.
func httpOnlyBroker(t *testing.T, f *reconcileFixture) {
	t.Helper()
	b, err := f.s.GetRuntimeBroker(context.Background(), f.brokerID)
	require.NoError(t, err)
	b.Endpoint = "http://broker.invalid:9800"
	require.NoError(t, f.s.UpdateRuntimeBroker(context.Background(), b))
}

func pendingStops(t *testing.T, f *reconcileFixture) int {
	t.Helper()
	rows, err := f.s.ListPendingDispatch(context.Background(), f.brokerID)
	require.NoError(t, err)
	n := 0
	for _, r := range rows {
		if r.Op == "stop" {
			n++
		}
	}
	return n
}

func reserved(t *testing.T, f *reconcileFixture, agentID string) bool {
	t.Helper()
	ctx := context.Background()
	def, err := f.s.GetLimitDefinitionByName(ctx, store.LimitMaxAgentsPerBroker)
	require.NoError(t, err)
	has, err := f.s.HasActiveReservation(ctx, def.ID, agentID)
	require.NoError(t, err)
	return has
}

// A stop queued for a broker reached over HTTP only is applied from the
// broker's heartbeat once it is back online; capacity is released on the
// stop result.
func TestQueuedStop_HTTPOnlyBrokerDrainedFromHeartbeat(t *testing.T) {
	f, d, a := newClaimFixture(t)
	setBrokerAgentCeiling(t, f.s, 5)
	_, err := f.srv.checkAndReserveBrokerQuota(context.Background(), a)
	require.NoError(t, err)
	httpOnlyBroker(t, f)
	queueStop(t, f, a, "")

	f.heartbeat(completeInventory(), a.Slug) // the container is still running
	require.Eventually(t, func() bool { return d.stops.Load() == 1 && pendingStops(t, f) == 0 }, 5*time.Second, 10*time.Millisecond)
	assert.False(t, reserved(t, f, a.ID), "capacity is released on the stop result")
	got := getAgent(t, f.s, a.ID)
	assert.Equal(t, "stopped", got.ContainerStatus)
	assert.Empty(t, got.Message, "the queued-stop notice is cleared")
}

// A start accepted after the stop was queued supersedes it: the drain does
// not stop the newer run.
func TestQueuedStop_LaterStartSupersedesQueuedStop(t *testing.T) {
	f, d, a := newClaimFixture(t)
	httpOnlyBroker(t, f)
	queueStop(t, f, a, "")
	code, body := lifecycle(t, f, a.ID, "start")
	require.Equal(t, 200, code, body)

	f.heartbeat(completeInventory(), a.Slug)
	require.Eventually(t, func() bool { return pendingStops(t, f) == 0 }, 5*time.Second, 10*time.Millisecond)
	assert.Equal(t, int32(0), d.stops.Load(), "the queued stop is not applied over the newer start")
	assert.Equal(t, store.RunIntentRunning, getAgent(t, f.s, a.ID).RunIntent)
}

// While the stop is not applied, a running report keeps stop_queued, its
// notice and the reservation.
func TestQueuedStop_RunningReportKeepsQueuedStatusAndCapacity(t *testing.T) {
	f, _, a := newClaimFixture(t)
	setBrokerAgentCeiling(t, f.s, 5)
	_, err := f.srv.checkAndReserveBrokerQuota(context.Background(), a)
	require.NoError(t, err)
	f.srv.SetDispatcher(nil) // no drain in this test
	queueStop(t, f, a, "")

	f.send(brokerHeartbeatRequest{
		Status:    store.BrokerStatusOnline,
		Inventory: completeInventory(),
		Projects: []brokerProjectHeartbeat{{ProjectID: f.projectID, Agents: []brokerAgentHeartbeat{
			{Slug: a.Slug, Phase: "running", ContainerStatus: "Up 5 minutes", Message: "working on it", RuntimeTarget: "docker"},
		}}},
	})
	got := getAgent(t, f.s, a.ID)
	assert.Equal(t, containerStatusStopQueued, got.ContainerStatus)
	assert.Equal(t, offlineStopMessage, got.Message)
	assert.True(t, reserved(t, f, a.ID))
	assert.True(t, agentHoldsBrokerCapacity(got), "the stale-reservation job keeps a stop_queued agent counted")
}

// Absent from a fresh complete inventory, the queued-stop agent has
// terminated: capacity is released and the notice cleared. A stale
// inventory (first heartbeat after a gap) is not enough.
func TestQueuedStop_AbsentFromFreshInventoryReleasesCapacity(t *testing.T) {
	f, _, a := newClaimFixture(t)
	setBrokerAgentCeiling(t, f.s, 5)
	_, err := f.srv.checkAndReserveBrokerQuota(context.Background(), a)
	require.NoError(t, err)
	f.srv.SetDispatcher(nil)
	queueStop(t, f, a, "")

	later := time.Now().Add(10 * time.Minute)
	f.srv.missingAgents.nowFor = func() time.Time { return later }
	f.heartbeat(completeInventory()) // after a gap: not a usable inventory
	assert.True(t, reserved(t, f, a.ID), "an inventory after a gap does not release")
	assert.Equal(t, containerStatusStopQueued, getAgent(t, f.s, a.ID).ContainerStatus)
	f.srv.missingAgents.nowFor = nil

	f.heartbeat(completeInventory())
	assert.False(t, reserved(t, f, a.ID))
	got := getAgent(t, f.s, a.ID)
	assert.Equal(t, "stopped", got.ContainerStatus)
	assert.Empty(t, got.Message)
}

// raceStartBeforeStopClaimStore takes a start claim (as a user start would)
// right after the drain read the agent, before the drain's stop claim.
type raceStartBeforeStopClaimStore struct {
	store.Store
	fired bool
}

func (s *raceStartBeforeStopClaimStore) ClaimAgentStop(ctx context.Context, agentID, owner string, intentAt time.Time, ttl time.Duration) (store.StartClaim, error) {
	if !s.fired {
		s.fired = true
		if _, err := s.ClaimAgentStart(ctx, agentID, "user-hub", store.StartClaimUser, "", time.Minute); err != nil {
			return store.StartClaim{}, err
		}
	}
	return s.Store.ClaimAgentStop(ctx, agentID, owner, intentAt, ttl)
}

// A start accepted between the drain's read and its stop is never stopped
// by it.
func TestQueuedStop_StartAcceptedDuringDrainIsNotStopped(t *testing.T) {
	f, d, a := newClaimFixture(t)
	at := queueStop(t, f, a, "")
	f.srv.store = &raceStartBeforeStopClaimStore{Store: f.s}
	args, err := MarshalDispatchArgs(StopDispatchArgs{IntentAt: &at})
	require.NoError(t, err)
	res, err := f.srv.execDispatchStop(context.Background(), store.BrokerDispatch{ID: tid("race"), BrokerID: f.brokerID, AgentID: a.ID, AgentSlug: a.Slug, ProjectID: f.projectID, Op: "stop", Args: args})
	require.NoError(t, err)
	assert.Equal(t, stopSupersededResult, res)
	assert.Equal(t, int32(0), d.stops.Load(), "the stop is not applied over the start accepted meanwhile")

	// While the drain holds its stop claim, a start waits for it.
	b := f.addAgent("held", "stopped", "")
	at2 := queueStop(t, f, b, "")
	_, err = f.s.ClaimAgentStop(context.Background(), b.ID, "drain-hub", at2, time.Minute)
	require.NoError(t, err)
	_, err = f.s.ClaimAgentStart(context.Background(), b.ID, "user-hub", store.StartClaimUser, "", time.Minute)
	var held *store.ClaimHeldError
	require.True(t, errors.As(err, &held))
	assert.Equal(t, store.StartClaimStop, held.Kind)
}

// A claim the queued stop itself superseded does not block it.
func TestQueuedStop_SupersededClaimDoesNotBlockTheDrain(t *testing.T) {
	f, d, a := newClaimFixture(t)
	ctx := context.Background()
	c, err := f.s.ClaimAgentStart(ctx, a.ID, "old-hub", store.StartClaimRecovery, "", time.Minute)
	require.NoError(t, err)
	_, err = f.s.MarkStartUnconfirmed(ctx, a.ID, c.ID, "old-hub", time.Hour)
	require.NoError(t, err)
	at := queueStop(t, f, a, c.ID)
	args, err := MarshalDispatchArgs(StopDispatchArgs{IntentAt: &at, SupersedesClaim: c.ID})
	require.NoError(t, err)
	_, err = f.srv.execDispatchStop(ctx, store.BrokerDispatch{ID: tid("sup"), BrokerID: f.brokerID, AgentID: a.ID, AgentSlug: a.Slug, ProjectID: f.projectID, Op: "stop", Args: args})
	require.NoError(t, err)
	assert.Equal(t, int32(1), d.stops.Load())
	assert.Empty(t, getAgent(t, f.s, a.ID).StartClaimID, "the superseded claim is released after the stop")
}

// A start that takes the agent after its stop was queued is not settled as
// the queued stop: a fresh inventory without its container (the start is
// still dispatching) keeps the start's reservation and status.
func TestQueuedStop_SettleLeavesAConcurrentStartAlone(t *testing.T) {
	f, _, a := newClaimFixture(t)
	ctx := context.Background()
	setBrokerAgentCeiling(t, f.s, 5)
	f.srv.SetDispatcher(nil) // no drain in this test
	queueStop(t, f, a, "")
	_, err := f.s.ClaimAgentStart(ctx, a.ID, "user-hub", store.StartClaimUser, "", time.Minute)
	require.NoError(t, err)
	_, err = f.srv.checkAndReserveBrokerQuota(ctx, getAgent(t, f.s, a.ID))
	require.NoError(t, err)

	f.heartbeat(completeInventory())
	assert.True(t, reserved(t, f, a.ID), "the start's reservation is kept")
	assert.NotEqual(t, "stopped", getAgent(t, f.s, a.ID).ContainerStatus)
}

// A start that succeeds after a queued stop supersedes it: stop_queued and
// its notice are cleared, and a later report applies as usual.
func TestQueuedStop_StartAfterQueuedStopClearsTheQueuedStatus(t *testing.T) {
	f, _, a := newClaimFixture(t)
	queueStop(t, f, a, "")
	code, body := lifecycle(t, f, a.ID, "start")
	require.Equal(t, http.StatusOK, code, body)
	got := getAgent(t, f.s, a.ID)
	assert.NotEqual(t, containerStatusStopQueued, got.ContainerStatus, "the start superseded the queued stop")
	assert.NotEqual(t, offlineStopMessage, got.Message)

	f.send(brokerHeartbeatRequest{
		Status:    store.BrokerStatusOnline,
		Inventory: completeInventory(),
		Projects: []brokerProjectHeartbeat{{ProjectID: f.projectID, Agents: []brokerAgentHeartbeat{
			{Slug: a.Slug, Phase: "running", ContainerStatus: "Up 1 minute", RuntimeTarget: "docker"},
		}}},
	})
	assert.Equal(t, "Up 1 minute", getAgent(t, f.s, a.ID).ContainerStatus)
}

// With run intent running, a report never keeps a stale stop_queued.
func TestQueuedStop_ReportReplacesStopQueuedOnceIntentIsRunning(t *testing.T) {
	f, _, a := newClaimFixture(t)
	ctx := context.Background()
	f.srv.SetDispatcher(nil)
	queueStop(t, f, a, "")
	_, err := f.s.SetRunIntent(ctx, a.ID, store.RunIntentRunning)
	require.NoError(t, err)
	f.send(brokerHeartbeatRequest{
		Status:    store.BrokerStatusOnline,
		Inventory: completeInventory(),
		Projects: []brokerProjectHeartbeat{{ProjectID: f.projectID, Agents: []brokerAgentHeartbeat{
			{Slug: a.Slug, Phase: "running", ContainerStatus: "Up 1 minute", RuntimeTarget: "docker"},
		}}},
	})
	assert.Equal(t, "Up 1 minute", getAgent(t, f.s, a.ID).ContainerStatus)
}

// timeoutStopDispatcher fails every stop, as a broker that times out would.
type timeoutStopDispatcher struct{ *claimTestDispatcher }

func (d timeoutStopDispatcher) DispatchAgentStop(ctx context.Context, a *store.Agent) error {
	d.stops.Add(1)
	return errors.New("broker timeout")
}

// A drained stop that failed leaves the row failed; once the broker reports
// the container terminal in a fresh complete inventory, the queued stop is
// confirmed: capacity released and the notice cleared.
func TestQueuedStop_FailedDrainSettledByATerminalReport(t *testing.T) {
	f, base, a := newClaimFixture(t)
	d := timeoutStopDispatcher{base}
	f.srv.SetDispatcher(d)
	setBrokerAgentCeiling(t, f.s, 5)
	_, err := f.srv.checkAndReserveBrokerQuota(context.Background(), a)
	require.NoError(t, err)
	httpOnlyBroker(t, f)
	queueStop(t, f, a, "")

	f.heartbeat(completeInventory(), a.Slug)
	require.Eventually(t, func() bool { return d.stops.Load() == 1 && pendingStops(t, f) == 0 }, 5*time.Second, 10*time.Millisecond)
	assert.True(t, reserved(t, f, a.ID), "a failed stop releases nothing")

	f.send(brokerHeartbeatRequest{
		Status:    store.BrokerStatusOnline,
		Inventory: completeInventory(),
		Projects: []brokerProjectHeartbeat{{ProjectID: f.projectID, Agents: []brokerAgentHeartbeat{
			{Slug: a.Slug, Phase: "stopped", ContainerStatus: "Exited (0)", RuntimeTarget: "docker"},
		}}},
	})
	assert.False(t, reserved(t, f, a.ID), "a terminal container confirms the stop")
	got := getAgent(t, f.s, a.ID)
	assert.Equal(t, "stopped", got.ContainerStatus)
	assert.Empty(t, got.Message)
}

func TestQueuedStop_BrokerHasNoControlChannel(t *testing.T) {
	f, _, _ := newClaimFixture(t)
	other := "other-hub"
	empty := ""
	cases := []struct {
		name   string
		broker *store.RuntimeBroker
		want   bool
	}{
		{"no broker", nil, false},
		{"no endpoint", &store.RuntimeBroker{ID: "b1"}, false},
		{"owned by another hub node", &store.RuntimeBroker{ID: "b1", Endpoint: "http://b", ConnectedHubID: &other}, false},
		{"HTTP only", &store.RuntimeBroker{ID: "b1", Endpoint: "http://b"}, true},
		{"HTTP only, empty hub ID", &store.RuntimeBroker{ID: "b1", Endpoint: "http://b", ConnectedHubID: &empty}, true},
	}
	for _, tc := range cases {
		assert.Equal(t, tc.want, f.srv.brokerHasNoControlChannel(tc.broker), tc.name)
	}
}

// A broker whose control channel another hub node holds is drained there
// on reconnect, never from its heartbeat on this node.
func TestQueuedStop_BrokerOwnedByAnotherNodeNotDrainedFromHeartbeat(t *testing.T) {
	f, d, a := newClaimFixture(t)
	httpOnlyBroker(t, f)
	b, err := f.s.GetRuntimeBroker(context.Background(), f.brokerID)
	require.NoError(t, err)
	other := "other-hub"
	b.ConnectedHubID = &other
	require.NoError(t, f.s.UpdateRuntimeBroker(context.Background(), b))
	queueStop(t, f, a, "")

	f.heartbeat(completeInventory(), a.Slug)
	f.heartbeat(completeInventory(), a.Slug)
	time.Sleep(100 * time.Millisecond)
	assert.Equal(t, int32(0), d.stops.Load())
	assert.Equal(t, 1, pendingStops(t, f))
}

// countingDispatchStore counts pending-dispatch reads.
type countingDispatchStore struct {
	store.Store
	lists *atomic.Int32
}

func (s countingDispatchStore) ListPendingDispatch(ctx context.Context, brokerID string) ([]store.BrokerDispatch, error) {
	s.lists.Add(1)
	return s.Store.ListPendingDispatch(ctx, brokerID)
}

// At most one heartbeat drain per broker runs at a time on a node: a second
// heartbeat while one drain is applying a stop does not start another.
func TestQueuedStop_OneHeartbeatDrainPerBroker(t *testing.T) {
	f, base, a := newClaimFixture(t)
	release := make(chan struct{})
	entered := make(chan struct{}, 1)
	d := &stopHookDispatcher{claimTestDispatcher: base, onStop: func(context.Context, *store.Agent) {
		entered <- struct{}{}
		<-release
	}}
	f.srv.SetDispatcher(d)
	httpOnlyBroker(t, f)
	queueStop(t, f, a, "")
	second := f.addAgent("second", "running", "")
	queueStop(t, f, second, "") // still pending while the first stop runs
	var lists atomic.Int32
	f.srv.store = countingDispatchStore{Store: f.s, lists: &lists}
	b, err := f.s.GetRuntimeBroker(context.Background(), f.brokerID)
	require.NoError(t, err)
	hb := &brokerHeartbeatRequest{Status: store.BrokerStatusOnline}

	f.srv.drainQueuedStopsFromHeartbeat(context.Background(), f.brokerID, b, hb, newHeartbeatReport())
	<-entered // the first drain is applying the stop
	before := lists.Load()
	f.srv.drainQueuedStopsFromHeartbeat(context.Background(), f.brokerID, b, hb, newHeartbeatReport())
	time.Sleep(50 * time.Millisecond)
	assert.Equal(t, before+1, lists.Load(), "only the second heartbeat's own read: no second drain")
	close(release) // the first drain then applies the second stop too
	require.Eventually(t, func() bool { return pendingStops(t, f) == 0 }, 5*time.Second, 10*time.Millisecond)
	assert.Equal(t, int32(2), base.stops.Load(), "each queued stop applied once")
}

// queuedStopFixture queues a stop for a reserved agent with no drain, and
// returns the fixture, the agent and the queued intent time.
func queuedStopFixture(t *testing.T) (*reconcileFixture, *claimTestDispatcher, *store.Agent, time.Time) {
	t.Helper()
	f, d, a := newClaimFixture(t)
	setBrokerAgentCeiling(t, f.s, 5)
	_, err := f.srv.checkAndReserveBrokerQuota(context.Background(), a)
	require.NoError(t, err)
	f.srv.SetDispatcher(nil) // settle only, no drain
	at := queueStop(t, f, a, "")
	return f, d, a, at
}

// assertNotSettled checks the queued stop was left as it was.
func assertNotSettled(t *testing.T, f *reconcileFixture, a *store.Agent, why string) {
	t.Helper()
	assert.True(t, reserved(t, f, a.ID), "reservation kept: "+why)
	assert.Equal(t, containerStatusStopQueued, getAgent(t, f.s, a.ID).ContainerStatus, "not settled: "+why)
}

// Each settle guard on its own: the queued stop is not settled while it is
// no longer the agent's intent or a start may be under way.

func TestQueuedStop_SettleGuard_IntentRunningWithoutClaim(t *testing.T) {
	f, _, a, _ := queuedStopFixture(t)
	_, err := f.s.SetRunIntent(context.Background(), a.ID, store.RunIntentRunning)
	require.NoError(t, err)
	f.heartbeat(completeInventory())
	assertNotSettled(t, f, a, "intent running")
}

// A stop-kind claim (the drain's or the backstop's own) holds the agent with
// intent still stopped: that stop owns the outcome, so settle waits.
func TestQueuedStop_SettleGuard_ClaimWithIntentStopped(t *testing.T) {
	f, _, a, at := queuedStopFixture(t)
	_, err := f.s.ClaimAgentStop(context.Background(), a.ID, "other-hub", at, time.Minute)
	require.NoError(t, err)
	require.Equal(t, store.RunIntentStopped, getAgent(t, f.s, a.ID).RunIntent)
	f.heartbeat(completeInventory())
	assertNotSettled(t, f, a, "a claim is held")
}

func TestQueuedStop_SettleGuard_LifecycleOpActive(t *testing.T) {
	f, _, a, _ := queuedStopFixture(t)
	end := f.srv.beginLifecycleOp(a.ID)
	f.heartbeat(completeInventory())
	assertNotSettled(t, f, a, "a lifecycle operation is active")
	end()
	f.heartbeat(completeInventory())
	assert.False(t, reserved(t, f, a.ID), "settled once the operation ended")
}

func TestQueuedStop_SettleGuard_BrokerStartInFlight(t *testing.T) {
	f, _, a, _ := queuedStopFixture(t)
	f.send(brokerHeartbeatRequest{
		Status:         store.BrokerStatusOnline,
		Inventory:      completeInventory(),
		Capabilities:   &store.BrokerCapabilities{StartsInFlight: true},
		StartsInFlight: []brokerStartInFlight{{ProjectID: f.projectID, Slug: a.Slug}},
		Projects:       []brokerProjectHeartbeat{{ProjectID: f.projectID}},
	})
	assertNotSettled(t, f, a, "the broker reports a start in flight")
	f.heartbeat(completeInventory())
	assert.False(t, reserved(t, f, a.ID), "settled once no start is in flight")
}

func TestQueuedStop_SettleGuard_PendingStartRow(t *testing.T) {
	f, _, a, _ := queuedStopFixture(t)
	require.NoError(t, f.s.InsertBrokerDispatch(context.Background(), &store.BrokerDispatch{
		ID: tid("qs-start-" + a.Slug), BrokerID: f.brokerID, AgentID: a.ID, AgentSlug: a.Slug, ProjectID: f.projectID, Op: "start",
	}))
	f.heartbeat(completeInventory())
	assertNotSettled(t, f, a, "a start is queued for the broker")
}

// rereadChangesStore returns, from the first GetAgent of agentID, a row on
// which a start has already taken the agent (as the settle's re-read would
// see one), without changing the stored row.
type rereadChangesStore struct {
	store.Store
	agentID string
	change  func(a *store.Agent)
	fired   *atomic.Bool
}

func (s rereadChangesStore) GetAgent(ctx context.Context, id string) (*store.Agent, error) {
	a, err := s.Store.GetAgent(ctx, id)
	if err == nil && id == s.agentID && s.fired.CompareAndSwap(false, true) {
		cp := *a
		s.change(&cp)
		return &cp, nil
	}
	return a, err
}

// The list sees the queued stop, but the re-read just before the release
// sees a claim or a newer intent: nothing is released.
func TestQueuedStop_SettleGuard_RereadSeesAStart(t *testing.T) {
	cases := map[string]func(a *store.Agent){
		"claim held":   func(a *store.Agent) { a.StartClaimID = "c" },
		"newer intent": func(a *store.Agent) { t := a.RunIntentAt.Add(time.Second); a.RunIntentAt = &t },
	}
	for name, change := range cases {
		t.Run(name, func(t *testing.T) {
			f, _, a, _ := queuedStopFixture(t)
			var fired atomic.Bool
			var releases atomic.Int32
			f.srv.store = rereadChangesStore{Store: f.s, agentID: a.ID, change: change, fired: &fired}
			f.srv.quotaService.store = countingReleaseStore{Store: f.s, releases: &releases}
			f.heartbeat(completeInventory())
			require.True(t, fired.Load(), "the settle re-read the row")
			assertNotSettled(t, f, a, name)
			assert.Equal(t, int32(0), releases.Load(), "the re-read before the release stops it: nothing is released")
		})
	}
}

// countingReleaseStore counts reservation releases.
type countingReleaseStore struct {
	store.Store
	releases *atomic.Int32
}

func (s countingReleaseStore) ReleaseReservation(ctx context.Context, limitID, resourceID string) error {
	s.releases.Add(1)
	return s.Store.ReleaseReservation(ctx, limitID, resourceID)
}

// failSecondGetAgentStore fails the second GetAgent of agentID: the settle's
// read after the release.
type failSecondGetAgentStore struct {
	store.Store
	agentID string
	calls   *atomic.Int32
}

func (s failSecondGetAgentStore) GetAgent(ctx context.Context, id string) (*store.Agent, error) {
	if id == s.agentID && s.calls.Add(1) == 2 {
		return nil, errors.New("database is locked")
	}
	return s.Store.GetAgent(ctx, id)
}

// When the read after the release fails, the reservation is put back and no
// stopped status is written; a later settle releases it.
func TestQueuedStop_FailedReadAfterTheReleasePutsTheReservationBack(t *testing.T) {
	f, _, a, _ := queuedStopFixture(t)
	var calls atomic.Int32
	f.srv.store = failSecondGetAgentStore{Store: f.s, agentID: a.ID, calls: &calls}
	f.heartbeat(completeInventory())
	require.GreaterOrEqual(t, calls.Load(), int32(2), "the settle read the row after the release")
	assertNotSettled(t, f, a, "the read after the release failed")

	f.srv.store = f.s
	f.heartbeat(completeInventory())
	assert.False(t, reserved(t, f, a.ID), "the next settle releases it")
}

// claimAfterRereadStore takes a user start claim right after the settle's
// re-read returns, before the release, as a racing start would.
type claimAfterRereadStore struct {
	store.Store
	agentID string
	fired   *atomic.Bool
}

func (s claimAfterRereadStore) GetAgent(ctx context.Context, id string) (*store.Agent, error) {
	a, err := s.Store.GetAgent(ctx, id)
	if err == nil && id == s.agentID && s.fired.CompareAndSwap(false, true) {
		if _, cerr := s.Store.ClaimAgentStart(ctx, id, "user-hub", store.StartClaimUser, "", time.Minute); cerr != nil {
			return nil, cerr
		}
	}
	return a, err
}

// A start claimed between the settle's re-read and its release keeps its
// reservation (put back after the release) and its status.
func TestQueuedStop_StartClaimedDuringTheReleaseKeepsItsReservation(t *testing.T) {
	f, _, a, _ := queuedStopFixture(t)
	var fired atomic.Bool
	f.srv.store = claimAfterRereadStore{Store: f.s, agentID: a.ID, fired: &fired}
	f.heartbeat(completeInventory())
	require.True(t, fired.Load(), "the start was claimed during the settle")
	assert.True(t, reserved(t, f, a.ID), "the start's reservation is held")
	got := getAgent(t, f.s, a.ID)
	assert.NotEqual(t, "stopped", got.ContainerStatus)
	assert.Equal(t, store.RunIntentRunning, got.RunIntent)
}

// A start after a queued stop whose broker reports a container status keeps
// that status; the queued-stop notice is cleared. Every start path clears the
// notice with its started write (a new generation), so this pins the started
// write and that the queued-stop clear then leaves the row alone.
func TestQueuedStop_StartAfterQueuedStopKeepsTheBrokersStatus(t *testing.T) {
	f, d, a := newClaimFixture(t)
	queueStop(t, f, a, "")
	d.start = func(ctx context.Context, cur *store.Agent) error {
		cur.ContainerStatus = "Up 1 second"
		return nil
	}
	code, body := lifecycle(t, f, a.ID, "start")
	require.Equal(t, http.StatusOK, code, body)
	got := getAgent(t, f.s, a.ID)
	assert.Equal(t, "Up 1 second", got.ContainerStatus)
	assert.NotEqual(t, offlineStopMessage, got.Message)
}

// A stop recorded after a start superseded a queued stop keeps its own
// state: the start's clear runs only while intent is still running.
func TestQueuedStop_ClearSkipsAStopRecordedAfterTheStart(t *testing.T) {
	f, _, a := newClaimFixture(t)
	ctx := context.Background()
	queueStop(t, f, a, "")
	snapshot := getAgent(t, f.s, a.ID)
	// A newer stop queued after the start: intent stopped again.
	_, err := f.s.SetRunIntent(ctx, a.ID, store.RunIntentStopped)
	require.NoError(t, err)
	f.srv.clearSupersededQueuedStop(ctx, snapshot)
	got := getAgent(t, f.s, a.ID)
	assert.Equal(t, containerStatusStopQueued, got.ContainerStatus)
	assert.Equal(t, offlineStopMessage, got.Message)
}
