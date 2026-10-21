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

// A restart whose start leg fails ambiguously (the start may have reached
// the runtime) keeps its broker slot and its unconfirmed restart claim
// (ptone/scion#5143, erratum E-33c-1): nothing is recorded stopped and
// nothing is released, the agent rests at stopped when the restart's hold
// marked it starting, and the start-claim reaper settles claim and slot
// once a fresh inventory shows the outcome. Every agent here has a run ID,
// so the restart's confirmed-failure branch would record and release.

package hub

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// errAmbiguousStart is a start-leg failure whose outcome is unknown.
var errAmbiguousStart = errors.New("connection reset by peer")

// restartAgentIn adds an agent of the claim fixture in phase, with a run ID
// and, when withSlot, its reservation.
func restartAgentIn(t *testing.T, f *reconcileFixture, slug string, phase state.Phase, withSlot bool) *store.Agent {
	t.Helper()
	a := f.addAgent(slug, string(phase), "")
	if withSlot {
		seedSlot(t, f.s, f.brokerID, a.ID)
	}
	_, err := f.s.SetAgentRunID(context.Background(), a.ID, "run-x", nil)
	require.NoError(t, err)
	return getAgent(t, f.s, a.ID)
}

// failStartAmbiguously makes the fixture dispatcher's start leg fail with
// an unknown outcome.
func failStartAmbiguously(d *claimTestDispatcher) {
	d.start = func(ctx context.Context, a *store.Agent) error { return errAmbiguousStart }
}

// assertRestartHeld checks the end state of an ambiguous restart: a 5xx
// answer, the reservation still active on the agent's broker, and the
// restart claim kept, unconfirmed.
func assertRestartHeld(t *testing.T, f *reconcileFixture, code int, agentID, why string) *store.Agent {
	t.Helper()
	assert.GreaterOrEqual(t, code, 500, "the restart fails")
	held := activeReservation(t, f.s, brokerLimitDefID(t, f.s), agentID)
	if assert.NotNil(t, held, why) {
		assert.Equal(t, f.brokerID, held.ScopeID, why)
	}
	got := getAgent(t, f.s, agentID)
	assert.Equal(t, store.StartClaimRestart, got.StartClaimKind, "the restart claim is kept")
	assert.Equal(t, store.StartClaimUnconfirmed, got.StartClaimState, "the restart claim is kept, unconfirmed")
	return got
}

func TestRestartAmbiguousStartLeg_KeepsSlotAndClaim(t *testing.T) {
	cases := []struct {
		name     string
		phase    state.Phase
		withSlot bool
		why      string
	}{
		// (a) The running agent keeps the slot it held; its phase is not
		// recorded stopped.
		{"running", state.PhaseRunning, true, "an ambiguous restart keeps the reservation"},
		// (b) The restart's hold created the reservation of a stopped agent.
		{"stopped", state.PhaseStopped, false, "the hold's reservation outlives an ambiguous start"},
		// (d), (e) Any uncounted prior phase rests at stopped.
		{"suspended", state.PhaseSuspended, false, "the hold's reservation outlives an ambiguous start"},
		{"halted", state.PhaseHalted, false, "the hold's reservation outlives an ambiguous start"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f, d, _ := newClaimFixture(t)
			setBrokerAgentCeiling(t, f.s, 5)
			a := restartAgentIn(t, f, "amb-"+tc.name, tc.phase, tc.withSlot)
			ev := &trackingEventPublisher{}
			f.srv.events = ev
			failStartAmbiguously(d)

			code, _ := lifecycle(t, f, a.ID, "restart")

			got := assertRestartHeld(t, f, code, a.ID, tc.why)
			if tc.phase == state.PhaseRunning {
				assert.NotEqual(t, string(state.PhaseStopped), got.Phase, "an ambiguous restart keeps the reservation")
				assert.NotEqual(t, "stopped", got.ContainerStatus, "nothing is recorded stopped")
			} else {
				assert.Equal(t, string(state.PhaseStopped), got.Phase, "an ambiguous restart rests at stopped")
			}
			published := false
			for _, p := range ev.publishedAgents() {
				published = published || p.ID == a.ID
			}
			assert.True(t, published, "the restart's end state is published")
		})
	}
}

// A stop supersedes the restart claim while the start leg is in flight,
// and the start leg then fails with an unknown outcome (its context was
// cancelled). The hold keeps its reservation, and the stop that took the
// claim releases the slot itself: no reservation is left.
func TestRestartAmbiguousStartLeg_LostClaim_StopSettlesSlot(t *testing.T) {
	f, d, _ := newClaimFixture(t)
	setBrokerAgentCeiling(t, f.s, 5)
	a := restartAgentIn(t, f, "amb-lost", state.PhaseStopped, false)
	fastClaims(f.srv, 300*time.Millisecond)
	started := make(chan struct{})
	d.start = func(ctx context.Context, cur *store.Agent) error {
		close(started)
		<-ctx.Done()
		return ctx.Err()
	}
	done := make(chan int, 1)
	go func() {
		code, _ := lifecycle(t, f, a.ID, "restart")
		done <- code
	}()
	<-started
	code, body := lifecycle(t, f, a.ID, "stop")
	require.Equal(t, http.StatusOK, code, body)
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("the in-flight restart was not abandoned after the stop")
	}

	got := getAgent(t, f.s, a.ID)
	assert.Equal(t, store.RunIntentStopped, got.RunIntent)
	assert.Empty(t, got.StartClaimID, "the stop released the claim")
	assert.False(t, hasReservation(t, f.s, store.LimitMaxAgentsPerBroker, a.ID), "the stop's release leaves no reservation")
}

// A restart of a stopped agent whose start leg was ambiguous: once a fresh
// complete inventory shows nothing running, the reaper releases the claim
// and the slot frees at that release, with no sweep.
func TestRestartAmbiguousStartLeg_ReaperSettles_ThenSlotFrees(t *testing.T) {
	f, d, _ := newClaimFixture(t)
	noObservationLag(t)
	setBrokerAgentCeiling(t, f.s, 5)
	a := restartAgentIn(t, f, "amb-reap", state.PhaseStopped, false)
	failStartAmbiguously(d)
	code, _ := lifecycle(t, f, a.ID, "restart")
	assertRestartHeld(t, f, code, a.ID, "the hold's reservation outlives an ambiguous start")

	f.heartbeat(completeInventory()) // nothing running
	f.srv.reapStartClaims(context.Background())

	assert.Empty(t, getAgent(t, f.s, a.ID).StartClaimID, "the reaper released the claim")
	assert.False(t, hasReservation(t, f.s, store.LimitMaxAgentsPerBroker, a.ID), "the slot frees at the reaper's release")
}

// A restart of a running agent whose start leg was ambiguous keeps reading
// a counted phase: the reaper's release leaves the slot, and the
// missing-container reconcile then moves the phase to error and the
// observed-phase release frees it.
func TestRestartAmbiguousStartLeg_CountedPhase_FreesWhenMissing(t *testing.T) {
	f, d, _ := newClaimFixture(t)
	noObservationLag(t)
	setBrokerAgentCeiling(t, f.s, 5)
	a := restartAgentIn(t, f, "amb-counted", state.PhaseRunning, true)
	failStartAmbiguously(d)
	code, _ := lifecycle(t, f, a.ID, "restart")
	assertRestartHeld(t, f, code, a.ID, "an ambiguous restart keeps the reservation")

	f.heartbeat(completeInventory())
	f.srv.reapStartClaims(context.Background())
	require.Empty(t, getAgent(t, f.s, a.ID).StartClaimID, "the reaper released the claim")
	assert.True(t, hasReservation(t, f.s, store.LimitMaxAgentsPerBroker, a.ID), "a counted phase keeps the slot")

	f.heartbeat(completeInventory())
	f.expireClock(a.ID)
	f.heartbeat(completeInventory())
	assert.Equal(t, string(state.PhaseError), getAgent(t, f.s, a.ID).Phase, "marked missing")
	assert.False(t, hasReservation(t, f.s, store.LimitMaxAgentsPerBroker, a.ID), "freed by the observed-phase release")
}

// A restart of an agent whose stop is still queued for its broker (the
// queued row has not drained yet): the stop leg stopped the old container,
// so the ambiguous branch replaces stop_queued with stopped and releases
// nothing; the reaper's release then frees the slot.
func TestRestartAmbiguousStartLeg_QueuedStopAgent_SlotFrees(t *testing.T) {
	f, d, a := newClaimFixture(t)
	noObservationLag(t)
	setBrokerAgentCeiling(t, f.s, 5)
	seedSlot(t, f.s, f.brokerID, a.ID)
	_, err := f.s.SetAgentRunID(context.Background(), a.ID, "run-x", nil)
	require.NoError(t, err)
	queueStop(t, f, a, "")
	failStartAmbiguously(d)

	code, _ := lifecycle(t, f, a.ID, "restart")

	got := assertRestartHeld(t, f, code, a.ID, "an ambiguous restart keeps the reservation")
	assert.Equal(t, "stopped", got.ContainerStatus, "stop_queued is replaced")
	assert.NotEqual(t, offlineStopMessage, got.Message, "the offline-stop notice is cleared")

	f.heartbeat(completeInventory())
	f.srv.reapStartClaims(context.Background())
	assert.Empty(t, getAgent(t, f.s, a.ID).StartClaimID, "the reaper released the claim")
	assert.False(t, hasReservation(t, f.s, store.LimitMaxAgentsPerBroker, a.ID), "the queued-stop agent's slot frees at the reaper's release")
}

// After an ambiguous restart of a suspended agent, the agent rests at
// stopped, so when the restarted container does run, the agent's own
// running report moves it to running and the reaper releases the claim on
// "reported running"; the slot stays, held by a running agent.
func TestRestartAmbiguousStartLeg_SuspendedThenReportsRunning(t *testing.T) {
	f, d, _ := newClaimFixture(t)
	noObservationLag(t)
	setBrokerAgentCeiling(t, f.s, 5)
	a := restartAgentIn(t, f, "amb-susp-run", state.PhaseSuspended, false)
	failStartAmbiguously(d)
	code, _ := lifecycle(t, f, a.ID, "restart")
	got := assertRestartHeld(t, f, code, a.ID, "the hold's reservation outlives an ambiguous start")
	require.Equal(t, string(state.PhaseStopped), got.Phase, "an ambiguous restart rests at stopped")

	selfReport(t, f.srv, got, state.PhaseRunning)
	f.heartbeat(completeInventory(), a.Slug)
	f.srv.reapStartClaims(context.Background())

	got = getAgent(t, f.s, a.ID)
	assert.Equal(t, string(state.PhaseRunning), got.Phase)
	assert.Empty(t, got.StartClaimID, "the reaper released the claim on the running report")
	assert.True(t, hasReservation(t, f.s, store.LimitMaxAgentsPerBroker, a.ID), "a running agent keeps its slot")
}

// The agent's Runtime Broker is deregistered while the ambiguous restart's
// claim is held: the reaper releases the claim, and the slot frees at that
// release for a stopped agent, or through the sweep's deregistered-scope
// pass for one that still reads a counted phase.
func TestRestartAmbiguousStartLeg_DeregisteredBroker_SlotFrees(t *testing.T) {
	for _, tc := range []struct {
		name     string
		phase    state.Phase
		withSlot bool
	}{
		{"stopped", state.PhaseStopped, false},
		{"running", state.PhaseRunning, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, d, _ := newClaimFixture(t)
			ctx := context.Background()
			setBrokerAgentCeiling(t, f.s, 5)
			a := restartAgentIn(t, f, "amb-dereg-"+tc.name, tc.phase, tc.withSlot)
			failStartAmbiguously(d)
			code, _ := lifecycle(t, f, a.ID, "restart")
			assertRestartHeld(t, f, code, a.ID, "an ambiguous restart keeps the reservation")
			require.NoError(t, f.s.DeleteRuntimeBroker(ctx, f.brokerID))

			f.srv.reapStartClaims(ctx)
			require.Empty(t, getAgent(t, f.s, a.ID).StartClaimID, "the reaper released the claim of the deregistered broker")
			if tc.phase == state.PhaseRunning {
				assert.True(t, hasReservation(t, f.s, store.LimitMaxAgentsPerBroker, a.ID), "the slot is the sweep's to free")
				f.srv.ReconcileStaleBrokerQuotaReservations(ctx)
			}
			assert.False(t, hasReservation(t, f.s, store.LimitMaxAgentsPerBroker, a.ID), "the slot frees once the broker is deregistered")
		})
	}
}

// While the ambiguous restart's claim is held, a plain start is refused
// with start_in_progress and takes no slot; once the reaper releases the
// claim, the next start reserves with the cap check.
func TestRestartAmbiguousStartLeg_StartWhileHeld_409(t *testing.T) {
	f, d, _ := newClaimFixture(t)
	noObservationLag(t)
	setBrokerAgentCeiling(t, f.s, 5)
	a := restartAgentIn(t, f, "amb-held", state.PhaseStopped, false)
	failStartAmbiguously(d)
	code, _ := lifecycle(t, f, a.ID, "restart")
	assertRestartHeld(t, f, code, a.ID, "the hold's reservation outlives an ambiguous start")
	before := brokerReservationCount(t, f.s, f.brokerID)

	code, body := lifecycle(t, f, a.ID, "start")
	require.Equal(t, http.StatusConflict, code, body)
	errCode, _ := errorDetails(body)
	assert.Equal(t, ErrCodeStartInProgress, errCode)
	assert.Equal(t, before, brokerReservationCount(t, f.s, f.brokerID), "the refused start takes no slot")

	f.heartbeat(completeInventory())
	f.srv.reapStartClaims(context.Background())
	require.Empty(t, getAgent(t, f.s, a.ID).StartClaimID, "the reaper released the claim")
	d.start = nil
	code, body = lifecycle(t, f, a.ID, "start")
	require.Equal(t, http.StatusOK, code, body)
	assert.True(t, hasReservation(t, f.s, store.LimitMaxAgentsPerBroker, a.ID), "the next start reserves")
}

// A restart's start leg that definitely did not act keeps the confirmed
// branch: the agent is recorded stopped and its slot released.
func TestRestartConfirmedStartLegFailure_StillRecordsStoppedAndReleases(t *testing.T) {
	f, d, _ := newClaimFixture(t)
	setBrokerAgentCeiling(t, f.s, 5)
	a := restartAgentIn(t, f, "conf-fail", state.PhaseRunning, true)
	d.start = func(ctx context.Context, cur *store.Agent) error {
		return fmt.Errorf("x: %w", errStartRequestNotSent)
	}

	code, _ := lifecycle(t, f, a.ID, "restart")

	require.GreaterOrEqual(t, code, 400)
	got := getAgent(t, f.s, a.ID)
	assert.Equal(t, string(state.PhaseStopped), got.Phase, "a confirmed failure records stopped")
	assert.Equal(t, "stopped", got.ContainerStatus)
	assert.False(t, hasReservation(t, f.s, store.LimitMaxAgentsPerBroker, a.ID), "a confirmed failure releases the slot")
	assert.Empty(t, got.StartClaimID, "the claim is released")
}
