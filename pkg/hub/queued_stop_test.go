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

	f.heartbeat(completeInventory(), a.Slug)
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
		if _, err := s.Store.ClaimAgentStart(ctx, agentID, "user-hub", store.StartClaimUser, "", time.Minute); err != nil {
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
