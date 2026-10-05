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
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// noObservationLag lets an inventory received right after a claim became
// unconfirmed count, so tests need not wait a heartbeat interval.
func noObservationLag(t *testing.T) {
	t.Helper()
	old := unconfirmedObservationLag
	unconfirmedObservationLag = 0
	t.Cleanup(func() { unconfirmedObservationLag = old })
}

func unconfirmedClaim(t *testing.T, f *reconcileFixture, a *store.Agent, kind store.StartClaimKind, hold time.Duration) store.StartClaim {
	t.Helper()
	ctx := context.Background()
	c, err := f.s.ClaimAgentStart(ctx, a.ID, "hub-x", kind, "", time.Minute)
	require.NoError(t, err)
	held, err := f.s.MarkStartUnconfirmed(ctx, a.ID, c.ID, "hub-x", hold)
	require.NoError(t, err)
	require.True(t, held)
	return c
}

func TestStartClaimReaper_DeadOwnerDemotedThenReleasedOnAbsent(t *testing.T) {
	f, _, a := newClaimFixture(t)
	noObservationLag(t)
	ctx := context.Background()
	_, err := f.s.ClaimAgentStart(ctx, a.ID, "dead-hub", store.StartClaimRecovery, "", time.Millisecond)
	require.NoError(t, err)
	time.Sleep(5 * time.Millisecond)

	f.srv.reapStartClaims(ctx)
	got := getAgent(t, f.s, a.ID)
	assert.Equal(t, store.StartClaimUnconfirmed, got.StartClaimState, "an expired lease is demoted, not released")

	f.srv.reapStartClaims(ctx)
	assert.Equal(t, store.StartClaimUnconfirmed, getAgent(t, f.s, a.ID).StartClaimState, "no inventory yet: kept")

	f.heartbeat(completeInventory()) // the agent is absent from a complete inventory
	f.srv.reapStartClaims(ctx)
	assert.Empty(t, getAgent(t, f.s, a.ID).StartClaimID, "released once a fresh complete inventory shows nothing running")
}

func TestStartClaimReaper_AbsentTooSoonAfterDemotionKeeps(t *testing.T) {
	f, _, a := newClaimFixture(t)
	unconfirmedClaim(t, f, a, store.StartClaimUser, time.Hour)
	f.heartbeat(completeInventory())
	f.srv.reapStartClaims(context.Background())
	assert.Equal(t, store.StartClaimUnconfirmed, getAgent(t, f.s, a.ID).StartClaimState,
		"an inventory received less than one interval after the claim became unconfirmed does not release it")
}

func TestStartClaimReaper_StartInFlight(t *testing.T) {
	for _, tc := range []struct {
		name       string
		capability bool
		released   bool
	}{
		{"broker reports the start in flight", true, false},
		{"broker without the capability: time rule only", false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, _, a := newClaimFixture(t)
			noObservationLag(t)
			unconfirmedClaim(t, f, a, store.StartClaimUser, time.Hour)
			hb := brokerHeartbeatRequest{
				Status:         store.BrokerStatusOnline,
				Inventory:      completeInventory(),
				StartsInFlight: []brokerStartInFlight{{ProjectID: f.projectID, Slug: a.Slug}},
				Projects:       []brokerProjectHeartbeat{{ProjectID: f.projectID}},
			}
			if tc.capability {
				hb.Capabilities = &store.BrokerCapabilities{StartsInFlight: true}
			}
			f.send(hb)
			f.srv.reapStartClaims(context.Background())
			got := getAgent(t, f.s, a.ID)
			if tc.released {
				assert.Empty(t, got.StartClaimID)
			} else {
				assert.Equal(t, store.StartClaimUnconfirmed, got.StartClaimState)
			}
		})
	}
}

func TestStartClaimReaper_ReleasedWhenAgentReportsRunning(t *testing.T) {
	f, _, a := newClaimFixture(t)
	unconfirmedClaim(t, f, a, store.StartClaimUser, time.Hour)
	// The late container's agent reports running (its own status report).
	require.NoError(t, f.s.UpdateAgentStatus(context.Background(), a.ID, store.AgentStatusUpdate{Phase: "running", Heartbeat: true}))
	f.srv.reapStartClaims(context.Background())
	assert.Empty(t, getAgent(t, f.s, a.ID).StartClaimID)
}

func TestStartClaimReaper_HoldExpiryStopsPresentContainer(t *testing.T) {
	f, d, a := newClaimFixture(t)
	noObservationLag(t)
	unconfirmedClaim(t, f, a, store.StartClaimUser, time.Millisecond)
	time.Sleep(5 * time.Millisecond)
	present := brokerHeartbeatRequest{
		Status:    store.BrokerStatusOnline,
		Inventory: completeInventory(),
		Projects: []brokerProjectHeartbeat{{ProjectID: f.projectID, Agents: []brokerAgentHeartbeat{
			{Slug: a.Slug, Phase: "starting", ContainerStatus: "Pending", RuntimeTarget: "docker"},
		}}},
	}
	f.send(present)
	ctx := context.Background()
	f.srv.reapStartClaims(ctx)
	assert.Equal(t, int32(1), d.stops.Load(), "a container still present past the hold is stopped (normal grace)")
	assert.Equal(t, store.StartClaimUnconfirmed, getAgent(t, f.s, a.ID).StartClaimState, "the claim is kept until it is gone")
	f.srv.reapStartClaims(ctx)
	assert.Equal(t, int32(1), d.stops.Load(), "the stop is rate-limited")

	f.heartbeat(completeInventory()) // now gone
	f.srv.reapStartClaims(ctx)
	assert.Empty(t, getAgent(t, f.s, a.ID).StartClaimID)
}

func TestStartClaimReaper_OfflineBrokerKeepsClaimPastHold(t *testing.T) {
	f, d, a := newClaimFixture(t)
	noObservationLag(t)
	unconfirmedClaim(t, f, a, store.StartClaimUser, time.Millisecond)
	time.Sleep(5 * time.Millisecond)
	f.srv.reapStartClaims(context.Background()) // no inventory of the target at all
	assert.Equal(t, store.StartClaimUnconfirmed, getAgent(t, f.s, a.ID).StartClaimState, "expiry alone never releases")
	assert.Equal(t, int32(0), d.stops.Load())

	// The first fresh complete inventory after the broker is back settles it.
	f.heartbeat(completeInventory())
	f.srv.reapStartClaims(context.Background())
	assert.Empty(t, getAgent(t, f.s, a.ID).StartClaimID)
}

func TestStartClaimReaper_LaunchActiveIsLeftAlone(t *testing.T) {
	f, _, _ := newClaimFixture(t)
	ctx := context.Background()
	a := f.addAgent("launching", "created", "")
	_, err := f.s.ClaimAgentStart(ctx, a.ID, "hub-x", store.StartClaimCreate, "", time.Millisecond)
	require.NoError(t, err)
	_, err = f.s.BeginLaunch(ctx, a.ID, store.LaunchKindCreate, time.Hour)
	require.NoError(t, err)
	time.Sleep(5 * time.Millisecond)
	f.srv.reapStartClaims(ctx)
	assert.Equal(t, store.StartClaimLive, getAgent(t, f.s, a.ID).StartClaimState, "no reaper action while the launch is active")

	// The launch ends without settling the claim (a running status write):
	// the reaper's backstop applies the end reason.
	require.NoError(t, f.s.UpdateAgentStatus(ctx, a.ID, store.AgentStatusUpdate{Phase: "running"}))
	f.srv.reapStartClaims(ctx)
	assert.Empty(t, getAgent(t, f.s, a.ID).StartClaimID)
}

func TestStartClaimReaper_StopKindFollowsLeaseRules(t *testing.T) {
	f, _, a := newClaimFixture(t)
	ctx := context.Background()
	at, err := f.s.SetRunIntent(ctx, a.ID, store.RunIntentStopped)
	require.NoError(t, err)
	_, err = f.s.ClaimAgentStop(ctx, a.ID, "dead-hub", at, time.Millisecond)
	require.NoError(t, err)
	time.Sleep(5 * time.Millisecond)
	f.srv.reapStartClaims(ctx)
	assert.Equal(t, store.StartClaimUnconfirmed, getAgent(t, f.s, a.ID).StartClaimState)
}

func TestStartClaimReaper_NoRecordedTargetUsesBrokersOnlyTarget(t *testing.T) {
	f, _, _ := newClaimFixture(t)
	noObservationLag(t)
	a := f.addAgent("k8s-new", "created", "", func(a *store.Agent) {
		a.Runtime = "kubernetes"
		a.AppliedConfig = &store.AgentAppliedConfig{}
	})
	unconfirmedClaim(t, f, a, store.StartClaimCreate, time.Hour)
	f.heartbeat(completeInventory())
	f.srv.reapStartClaims(context.Background())
	assert.Empty(t, getAgent(t, f.s, a.ID).StartClaimID)
}

func TestStartClaimReaper_PodRestartDemotesOwnPreviousClaims(t *testing.T) {
	f, _, a := newClaimFixture(t)
	t.Setenv("POD_NAME", "hub-0")
	f.srv.instanceID = "hub-0-new"
	ctx := context.Background()
	_, err := f.s.ClaimAgentStart(ctx, a.ID, "hub-0-old", store.StartClaimUser, "", time.Hour)
	require.NoError(t, err)
	mine := f.addAgent("mine", "stopped", "")
	_, err = f.s.ClaimAgentStart(ctx, mine.ID, "hub-0-new", store.StartClaimUser, "", time.Hour)
	require.NoError(t, err)

	f.srv.demoteOwnClaimsOnRestart(ctx)
	assert.Equal(t, store.StartClaimUnconfirmed, getAgent(t, f.s, a.ID).StartClaimState)
	assert.Equal(t, store.StartClaimLive, getAgent(t, f.s, mine.ID).StartClaimState, "this process's own claims are untouched")
}

// skewedClockStore moves the store clock forward, so inventories recorded
// now read as old.
type skewedClockStore struct {
	store.Store
	skew time.Duration
}

func (s *skewedClockStore) StoreClock(ctx context.Context) (time.Time, error) {
	t, err := s.Store.StoreClock(ctx)
	return t.Add(s.skew), err
}

func TestStartClaimReaper_StaleInventoryKeepsClaim(t *testing.T) {
	f, _, a := newClaimFixture(t)
	noObservationLag(t)
	unconfirmedClaim(t, f, a, store.StartClaimUser, time.Hour)
	f.heartbeat(completeInventory()) // absent, but this inventory will be stale
	f.srv.store = &skewedClockStore{Store: f.s, skew: 5 * time.Minute}
	f.srv.reapStartClaims(context.Background())
	assert.Equal(t, store.StartClaimUnconfirmed, getAgent(t, f.s, a.ID).StartClaimState,
		"an inventory older than two heartbeat intervals does not release the claim")
}

// A phase of running left from before the claim (no report since the claim
// became unconfirmed) is not a success: a restart's ambiguous start leg
// leaves the old phase in place.
func TestStartClaimReaper_StaleRunningPhaseIsNotSuccess(t *testing.T) {
	f, _, _ := newClaimFixture(t)
	a := f.addAgent("restarting", "running", "working") // last seen an hour ago
	unconfirmedClaim(t, f, a, store.StartClaimRestart, time.Hour)
	f.srv.reapStartClaims(context.Background())
	assert.Equal(t, store.StartClaimUnconfirmed, getAgent(t, f.s, a.ID).StartClaimState,
		"running without a report after the claim became unconfirmed does not release it")
}
