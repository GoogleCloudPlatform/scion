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

// The queued-stop contract under P3.3-b (design §4.3, §4.4): the heartbeat
// settle's release is one conditional store write, the drain's release on
// the stop result is unchanged, and the two racing for one agent end in the
// same observable state as either alone. The settle's post-release
// re-assert is a metered backstop.

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

// settleHookStore runs beforeRelease and afterRelease, once each, around
// the settle's conditional release of agentID, and counts that agent's status writes while
// countWrites is set. Transparent until armed.
type settleHookStore struct {
	store.Store
	fault         *storeFaultSwitch
	agentID       string
	beforeRelease func()
	afterRelease  func()
	// releaseErr, while set, is returned by the settle's conditional
	// release instead of calling the store (a store failure).
	releaseErr   error
	countWrites  atomic.Bool
	statusWrites atomic.Int32
}

func (h *settleHookStore) ReleaseReservationIfAgent(ctx context.Context, limitDefID, agentID string, cond store.AgentReleaseCond) (bool, error) {
	// The settle's call is the only conditional release with neither
	// NoStartKindClaim nor NotCounted (the observed-phase release,
	// releaseSlotIfIdle and the sweep all set at least one), so a mutant that
	// changes the settle's queued-stop condition still reaches the hooks.
	if !h.fault.Active() || agentID != h.agentID || cond.NoStartKindClaim || cond.NotCounted {
		return h.Store.ReleaseReservationIfAgent(ctx, limitDefID, agentID, cond)
	}
	if h.releaseErr != nil {
		return false, h.releaseErr
	}
	if f := h.beforeRelease; f != nil {
		h.beforeRelease = nil
		f()
	}
	released, err := h.Store.ReleaseReservationIfAgent(ctx, limitDefID, agentID, cond)
	if f := h.afterRelease; f != nil {
		h.afterRelease = nil
		f()
	}
	return released, err
}

func (h *settleHookStore) UpdateAgentStatus(ctx context.Context, id string, su store.AgentStatusUpdate) error {
	if h.countWrites.Load() && id == h.agentID {
		h.statusWrites.Add(1)
	}
	return h.Store.UpdateAgentStatus(ctx, id, su)
}

// settleFixture is a reconcile fixture with the settle hook store installed,
// a claim dispatcher, and one agent whose stop is queued while it holds its
// slot.
type settleFixture struct {
	*reconcileFixture
	hook      *settleHookStore
	fault     *storeFaultSwitch
	disp      *claimTestDispatcher
	agent     *store.Agent
	intentAt  time.Time
	slot      *store.UsageReservation
	backstops func(site string) int64
}

func newSettleFixture(t *testing.T, slug string) *settleFixture {
	t.Helper()
	f := newReconcileFixture(t)
	hook, fault := installStoreFault(t, f.srv, func(inner store.Store, fs *storeFaultSwitch) *settleHookStore {
		return &settleHookStore{Store: inner, fault: fs}
	})
	d := &claimTestDispatcher{}
	f.srv.SetDispatcher(d)
	f.srv.startClaimsOn = true
	a := f.addAgent(slug, "running", "working")
	slot := seedSlot(t, f.s, f.brokerID, a.ID)
	at := queueStop(t, f, a, "")
	hook.agentID = a.ID
	return &settleFixture{
		reconcileFixture: f, hook: hook, fault: fault, disp: d, agent: getAgent(t, f.s, a.ID),
		intentAt: at, slot: slot, backstops: backstopCounter(t, f.srv),
	}
}

// drain applies the queued stop as the reconnect drain does.
func (sf *settleFixture) drain() {
	sf.srv.drainBrokerDispatch(context.Background(), sf.brokerID, func(op string) bool { return op == "stop" })
}

// settle sends a heartbeat whose fresh complete inventory no longer lists
// the agent, which settles its queued stop.
func (sf *settleFixture) settle() {
	sf.heartbeat(completeInventory())
}

func (sf *settleFixture) held(t *testing.T) *store.UsageReservation {
	t.Helper()
	return activeReservation(t, sf.s, brokerLimitDefID(t, sf.s), sf.agent.ID)
}

// k8s-runtime-lead answer 6: the drain's release (execDispatchStop) and the
// heartbeat settle race for one agent. Release is idempotent by agent, so
// the observable end state is asserted: no active reservation, no backstop
// write, container status stopped, the notice cleared once, and no status
// write after the drain's.
func TestQueuedStop_DoubleSettle_EndState(t *testing.T) {
	for _, order := range []string{"drain-before-release", "drain-after-release"} {
		t.Run(order, func(t *testing.T) {
			sf := newSettleFixture(t, "dbl-"+order[:11])
			run := func() {
				sf.drain()
				sf.hook.countWrites.Store(true)
			}
			if order == "drain-before-release" {
				sf.hook.beforeRelease = run
			} else {
				sf.hook.afterRelease = run
			}
			sf.fault.Arm()

			sf.settle()

			assert.Equal(t, int32(1), sf.disp.stops.Load(), "the drain applied the stop")
			assert.Nil(t, sf.held(t), "no active reservation")
			assert.Zero(t, sf.backstops(backstopSiteSettle), "the backstop wrote nothing")
			got := getAgent(t, sf.s, sf.agent.ID)
			assert.Equal(t, "stopped", got.ContainerStatus)
			assert.Empty(t, got.Message, "the queued-stop notice is cleared")
			assert.Zero(t, sf.hook.statusWrites.Load(), "no status write after the drain's")
			assert.Equal(t, 0, pendingStops(t, sf.reconcileFixture))
		})
	}
}

// The settle arrives after the drain already released and wrote stopped:
// nothing is re-asserted, overwritten or released.
func TestQueuedStop_SettleAfterDrain_NoOp(t *testing.T) {
	sf := newSettleFixture(t, "after-drain")
	sf.fault.Arm()
	sf.drain()
	require.Nil(t, sf.held(t), "the drain released on the stop result")
	before := getAgent(t, sf.s, sf.agent.ID)
	sf.hook.countWrites.Store(true)

	sf.settle()

	assert.Nil(t, sf.held(t))
	assert.Zero(t, sf.backstops(backstopSiteSettle))
	assert.Zero(t, sf.hook.statusWrites.Load(), "no status overwrite")
	got := getAgent(t, sf.s, sf.agent.ID)
	assert.Equal(t, before.ContainerStatus, got.ContainerStatus)
	assert.Equal(t, before.Message, got.Message)
}

// failingStopClaimDispatcher fails every stop dispatch.
type failingStopClaimDispatcher struct {
	claimTestDispatcher
}

func (d *failingStopClaimDispatcher) DispatchAgentStop(ctx context.Context, a *store.Agent) error {
	d.stops.Add(1)
	return errors.New("broker unreachable")
}

// The drain's dispatch fails after the settle's release (k8s-runtime-lead,
// 12:13Z): the drain releases its claim and leaves the stop queued, so the
// settle's final re-read still shows the queued stop intended, and the
// settle confirms the termination its inventory showed. Released, container
// status stopped, no backstop write.
func TestQueuedStop_DoubleSettle_DrainFailed_EndState(t *testing.T) {
	sf := newSettleFixture(t, "drain-failed")
	failing := &failingStopClaimDispatcher{}
	sf.srv.SetDispatcher(failing)
	sf.hook.afterRelease = sf.drain
	sf.fault.Arm()

	sf.settle()

	assert.Equal(t, int32(1), failing.stops.Load(), "the drain tried the stop")
	got := getAgent(t, sf.s, sf.agent.ID)
	assert.Empty(t, got.StartClaimID, "the drain released its claim")
	assert.Nil(t, sf.held(t), "released by the settle")
	assert.Equal(t, "stopped", got.ContainerStatus)
	assert.Zero(t, sf.backstops(backstopSiteSettle))
}

// The re-assert is skipped only when the drain owns or has reached the
// outcome (a stop-kind claim, or container status stopped). The same intent
// with no claim and a container not recorded stopped keeps the backstop
// re-assert, so a container that may still run is not left uncounted.
func TestQueuedStop_SettleReasserts_SameIntentNoClaimNotStopped(t *testing.T) {
	sf := newSettleFixture(t, "same-intent")
	sf.hook.afterRelease = func() {
		require.NoError(t, sf.s.UpdateAgentStatus(context.Background(), sf.agent.ID, store.AgentStatusUpdate{ContainerStatus: "Up 1 second"}))
	}
	sf.fault.Arm()

	sf.settle()

	assert.NotNil(t, sf.held(t), "the backstop put the reservation back")
	assert.EqualValues(t, 1, sf.backstops(backstopSiteSettle))
	audits := capacityAudits(t, sf.s, auditQuotaBackstopReassert, sf.agent.ID)
	require.Len(t, audits, 1)
	assert.Contains(t, audits[0].AfterSummary, sf.brokerID)
	assert.Contains(t, audits[0].AfterSummary, backstopSiteSettle)
}

// A start claim written between the settle's re-read and its release keeps
// the slot: the conditional release does not free it, so the backstop has
// nothing to repair.
func TestSettle_SupersedingStart_KeepsReservation(t *testing.T) {
	sf := newSettleFixture(t, "supersede")
	sf.hook.beforeRelease = func() {
		_, err := sf.s.ClaimAgentStart(context.Background(), sf.agent.ID, "other-hub", store.StartClaimUser, "", time.Minute)
		require.NoError(t, err)
	}
	sf.fault.Arm()

	sf.settle()

	require.Nil(t, sf.hook.beforeRelease, "the claim was injected before the settle's release")
	held := sf.held(t)
	require.NotNil(t, held, "the start keeps its slot")
	assert.Equal(t, sf.slot.ID, held.ID, "the same reservation, never released and re-created")
	assert.Zero(t, sf.backstops(backstopSiteSettle), "nothing slipped")
	assert.Equal(t, containerStatusStopQueued, getAgent(t, sf.s, sf.agent.ID).ContainerStatus, "the settle did not confirm a superseded stop")
}

// The settle's release commits first; a start that comes after finds no
// reservation and is admitted with the cap check: at a full instance it is
// refused, and the agent's stop is still confirmed.
func TestSettle_ReleaseCommitsFirst_StartGetsCheckedAdmission(t *testing.T) {
	t.Run("room", func(t *testing.T) {
		sf := newSettleFixture(t, "commit-room")
		setBrokerAgentCeiling(t, sf.s, 1)
		var code int
		sf.hook.afterRelease = func() {
			code = doRequest(t, sf.srv, http.MethodPost, "/api/v1/agents/"+sf.agent.ID+"/start", nil).Code
		}
		sf.fault.Arm()

		sf.settle()

		require.Equal(t, http.StatusOK, code)
		held := sf.held(t)
		require.NotNil(t, held, "the start reserved")
		assert.NotEqual(t, sf.slot.ID, held.ID, "a new, checked reservation")
		assert.EqualValues(t, 1, brokerReservationCount(t, sf.s, sf.brokerID), "not over the cap")
		assert.Zero(t, sf.backstops(backstopSiteSettle))
	})
	t.Run("full", func(t *testing.T) {
		sf := newSettleFixture(t, "commit-full")
		setBrokerAgentCeiling(t, sf.s, 1)
		filler := sf.addAgent("commit-full-filler", "running", "working")
		var code int
		sf.hook.afterRelease = func() {
			seedSlot(t, sf.s, sf.brokerID, filler.ID) // another admission took the slot
			code = doRequest(t, sf.srv, http.MethodPost, "/api/v1/agents/"+sf.agent.ID+"/start", nil).Code
		}
		sf.fault.Arm()

		sf.settle()

		assert.Equal(t, http.StatusTooManyRequests, code, "a checked admission at the cap")
		assert.Nil(t, sf.held(t), "no unchecked re-assert after the settle's own release")
		assert.EqualValues(t, 1, brokerReservationCount(t, sf.s, sf.brokerID))
		got := getAgent(t, sf.s, sf.agent.ID)
		assert.Equal(t, store.RunIntentRunning, got.RunIntent, "the refused start left its run intent")
		assert.Equal(t, containerStatusStopQueued, got.ContainerStatus, "a superseded stop is not confirmed")
		assert.Zero(t, sf.backstops(backstopSiteSettle))
	})
}

// A start that claimed the agent after its stop was queued supersedes the
// stop: the settle leaves the agent and its slot alone.
func TestQueuedStop_SupersedingClaim_ContractHolds(t *testing.T) {
	sf := newSettleFixture(t, "claim-contract")
	_, err := sf.s.ClaimAgentStart(context.Background(), sf.agent.ID, "other-hub", store.StartClaimUser, "", time.Minute)
	require.NoError(t, err)
	sf.fault.Arm()

	sf.settle()

	held := sf.held(t)
	require.NotNil(t, held)
	assert.Equal(t, sf.slot.ID, held.ID)
	assert.Zero(t, sf.backstops(backstopSiteSettle))
}

// pinnedQueuedStopAgent adds a pinned agent of f's broker with no recorded
// runtime target, holding its slot, with its stop queued.
func pinnedQueuedStopAgent(t *testing.T, f *reconcileFixture, slug string, pinned bool) *store.Agent {
	t.Helper()
	a := f.addAgent(slug, "running", "working", func(a *store.Agent) {
		a.AppliedConfig = nil
		if pinned {
			a.PinnedRuntimeBrokerID = f.brokerID
			a.PinnedRuntimeTargetID = "target-" + slug
			a.PinnedRuntimeTargetType = "kubernetes"
		}
	})
	seedSlot(t, f.s, f.brokerID, a.ID)
	queueStop(t, f, a, "")
	got := getAgent(t, f.s, a.ID)
	require.Equal(t, pinned, got.IsPinned())
	require.Empty(t, agentRuntimeTarget(got), "no recorded target yet")
	return got
}

// A pinned agent whose runtime target is not recorded yet is settled on the
// single target its own flat instance reports; the completeness check
// applies to that target unchanged.
func TestSettle_FlatPinnedEmptyTargetUsesInstanceTarget(t *testing.T) {
	f := newReconcileFixture(t)
	f.srv.SetDispatcher(nil) // no drain
	a := pinnedQueuedStopAgent(t, f, "flat-target", true)

	f.heartbeat(completeInventory(k8sTargetA))

	assert.False(t, hasReservation(t, f.s, store.LimitMaxAgentsPerBroker, a.ID), "settled on the instance's only target")
	assert.Equal(t, "stopped", getAgent(t, f.s, a.ID).ContainerStatus)
}

// Otherwise nothing changes: an incomplete target, more than one target, or
// an unpinned (legacy) agent with no recorded target is not settled.
func TestSettle_FlatInstanceIncompleteTargetSkips(t *testing.T) {
	cases := []struct {
		name   string
		pinned bool
		inv    *brokerInventory
	}{
		{name: "incomplete", pinned: true, inv: &brokerInventory{Targets: []brokerInventoryTarget{
			{ID: k8sTargetA, Complete: true}, {ID: k8sTargetA, Complete: false},
		}}},
		{name: "two-targets", pinned: true, inv: &brokerInventory{Targets: []brokerInventoryTarget{
			{ID: k8sTargetA, Complete: true}, {ID: k8sTargetB, Complete: true},
		}}},
		{name: "unpinned", pinned: false, inv: completeInventory(k8sTargetA)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newReconcileFixture(t)
			f.srv.SetDispatcher(nil)
			a := pinnedQueuedStopAgent(t, f, "flat-skip-"+tc.name, tc.pinned)

			f.heartbeat(tc.inv)

			assert.True(t, hasReservation(t, f.s, store.LimitMaxAgentsPerBroker, a.ID), "not settled")
			assert.Equal(t, containerStatusStopQueued, getAgent(t, f.s, a.ID).ContainerStatus)
		})
	}
}

// G2: the sweep's backfill restores the reservation of an agent whose stop
// is queued and still intended and that lost its reservation by any route.
func TestBackfill_RestoresQueuedStopReservation(t *testing.T) {
	f := newReconcileFixture(t)
	a := f.addAgent("bf-queued", "running", "working")
	queueStop(t, f, a, "")
	require.False(t, hasReservation(t, f.s, store.LimitMaxAgentsPerBroker, a.ID))

	f.srv.ReconcileStaleBrokerQuotaReservations(context.Background())

	held := activeReservation(t, f.s, brokerLimitDefID(t, f.s), a.ID)
	require.NotNil(t, held, "restored")
	assert.Equal(t, f.brokerID, held.ScopeID)
}

// Container status alone is not enough: a stop_queued agent whose stop is
// no longer the intent (a start intent, or a claim) is not backfilled.
func TestBackfill_SkipsStopQueuedWithClaimOrStartIntent(t *testing.T) {
	for _, tc := range []string{"start-intent", "stop-claim"} {
		t.Run(tc, func(t *testing.T) {
			f := newReconcileFixture(t)
			ctx := context.Background()
			a := f.addAgent("bf-skip-"+tc, "running", "working")
			at := queueStop(t, f, a, "")
			if tc == "start-intent" {
				_, err := f.s.SetRunIntent(ctx, a.ID, store.RunIntentRunning)
				require.NoError(t, err)
			} else {
				_, err := f.s.ClaimAgentStop(ctx, a.ID, "other-hub", at, time.Minute)
				require.NoError(t, err)
			}
			got := getAgent(t, f.s, a.ID)
			require.Equal(t, containerStatusStopQueued, got.ContainerStatus)

			f.srv.ReconcileStaleBrokerQuotaReservations(ctx)

			assert.False(t, hasReservation(t, f.s, store.LimitMaxAgentsPerBroker, a.ID))
		})
	}
}

// A store failure in the settle's conditional release is not a condition
// that did not hold: nothing was released, so the termination is not
// confirmed and the stop stays queued; the next heartbeat settles it.
func TestSettle_ReleaseStoreError_NotConfirmed_RetriedNextHeartbeat(t *testing.T) {
	sf := newSettleFixture(t, "release-err")
	sf.hook.releaseErr = errors.New("database is locked")
	sf.fault.Arm()

	sf.settle()

	require.NotNil(t, sf.held(t), "nothing released")
	assert.Equal(t, containerStatusStopQueued, getAgent(t, sf.s, sf.agent.ID).ContainerStatus, "not confirmed")
	assert.Zero(t, sf.backstops(backstopSiteSettle))

	sf.hook.releaseErr = nil
	sf.settle()

	assert.Nil(t, sf.held(t), "released on the next heartbeat")
	assert.Equal(t, "stopped", getAgent(t, sf.s, sf.agent.ID).ContainerStatus)
	assert.Zero(t, sf.backstops(backstopSiteSettle))
}

// The claim branch of the settle's re-assert skip: the drain took its
// stop-kind claim for the same queued stop after the settle's release and
// has not written the stop result yet. Nothing is re-asserted, and the
// settle does not overwrite the queued-stop status the drain owns.
func TestQueuedStop_SettleSkip_DrainClaimHeld(t *testing.T) {
	sf := newSettleFixture(t, "drain-claim")
	sf.hook.afterRelease = func() {
		_, err := sf.s.ClaimAgentStop(context.Background(), sf.agent.ID, "drain-hub", sf.intentAt, time.Minute)
		require.NoError(t, err)
		sf.hook.countWrites.Store(true)
	}
	sf.fault.Arm()

	sf.settle()

	assert.Nil(t, sf.held(t), "released by the settle; no re-assert")
	assert.Zero(t, sf.backstops(backstopSiteSettle))
	got := getAgent(t, sf.s, sf.agent.ID)
	assert.Equal(t, store.StartClaimStop, got.StartClaimKind, "the drain's claim is held")
	assert.Equal(t, containerStatusStopQueued, got.ContainerStatus, "not overwritten")
	assert.Zero(t, sf.hook.statusWrites.Load(), "no status write after the drain claimed")
}

// A start that claims the agent between the settle's release and its
// re-read, and reserves after the re-read, finds no reservation: its
// admission is checked, so at a full instance it is refused. The settle
// does not re-assert a slot it has just released safely.
func TestSettle_ClaimBetweenReleaseAndReread_ReserveAfter_Checked(t *testing.T) {
	sf := newSettleFixture(t, "claim-between")
	setBrokerAgentCeiling(t, sf.s, 1)
	filler := sf.addAgent("claim-between-filler", "running", "working")
	sf.hook.afterRelease = func() {
		_, err := sf.s.ClaimAgentStart(context.Background(), sf.agent.ID, "other-hub", store.StartClaimUser, "", time.Minute)
		require.NoError(t, err)
		seedSlot(t, sf.s, sf.brokerID, filler.ID) // the freed slot is taken
	}
	sf.fault.Arm()

	sf.settle()

	assert.Nil(t, sf.held(t), "nothing re-asserted")
	assert.Zero(t, sf.backstops(backstopSiteSettle))
	created, err := sf.srv.checkAndReserveBrokerQuota(context.Background(), getAgent(t, sf.s, sf.agent.ID))
	assert.ErrorIs(t, err, store.ErrQuotaExceeded, "the start's reserve is a checked admission at the full instance")
	assert.False(t, created)
	assert.Nil(t, sf.held(t))
	assert.EqualValues(t, 1, brokerReservationCount(t, sf.s, sf.brokerID))
}

// A superseding start refused at a full instance (429) after the settle's
// release: no reservation is ever created for the agent (no settle
// re-assert, no backfill, nothing from the sweep or a later heartbeat). Its
// container status stays stop_queued with run intent running, display only,
// until the agent's next lifecycle action; here a start once a slot frees,
// which reserves with the cap check and replaces the status.
func TestSettle_RefusedSupersedingStart_NoReservationUntilNextAction(t *testing.T) {
	sf := newSettleFixture(t, "refused-start")
	setBrokerAgentCeiling(t, sf.s, 1)
	filler := sf.addAgent("refused-start-filler", "running", "working")
	var code int
	sf.hook.afterRelease = func() {
		seedSlot(t, sf.s, sf.brokerID, filler.ID)
		code = doRequest(t, sf.srv, http.MethodPost, "/api/v1/agents/"+sf.agent.ID+"/start", nil).Code
	}
	sf.fault.Arm()

	sf.settle()
	require.Equal(t, http.StatusTooManyRequests, code)
	assert.Nil(t, sf.held(t))

	sf.srv.ReconcileStaleBrokerQuotaReservations(context.Background())
	sf.settle() // a later heartbeat
	assert.Nil(t, sf.held(t), "no reservation is ever created for the refused start")
	got := getAgent(t, sf.s, sf.agent.ID)
	assert.Equal(t, containerStatusStopQueued, got.ContainerStatus, "display only, until the next lifecycle action")
	assert.Equal(t, store.RunIntentRunning, got.RunIntent)

	require.NoError(t, sf.s.ReleaseReservation(context.Background(), brokerLimitDefID(t, sf.s), filler.ID))
	sf.disp.start = func(ctx context.Context, cur *store.Agent) error {
		cur.ContainerStatus = "running"
		return nil
	}
	require.Equal(t, http.StatusOK, doRequest(t, sf.srv, http.MethodPost, "/api/v1/agents/"+sf.agent.ID+"/start", nil).Code)
	require.NotNil(t, sf.held(t), "the next start reserves with the cap check")
	assert.Equal(t, "running", getAgent(t, sf.s, sf.agent.ID).ContainerStatus, "stop_queued is replaced")
	assert.Zero(t, sf.backstops(backstopSiteSettle))
}

// The two conjuncts of the settle's no-re-assert rule: the intent goes back
// to running at the SAME time as the stop (a revert, as a failed
// system-initiated stop does), not a newer start. The settle released
// safely, but the agent is meant to run on the same intent, so the backstop
// re-asserts (counted).
func TestSettle_SameTimeRevertToRunning_Reasserts(t *testing.T) {
	sf := newSettleFixture(t, "same-time-revert")
	require.NotNil(t, sf.agent.RunIntentAt)
	sf.hook.afterRelease = func() {
		reverted, err := sf.s.RevertRunIntent(context.Background(), sf.agent.ID, store.RunIntentStopped, *sf.agent.RunIntentAt, store.RunIntentRunning)
		require.NoError(t, err)
		require.True(t, reverted)
	}
	sf.fault.Arm()

	sf.settle()

	require.Nil(t, sf.hook.afterRelease, "the revert ran after the settle's release")
	got := getAgent(t, sf.s, sf.agent.ID)
	require.Equal(t, store.RunIntentRunning, got.RunIntent)
	assert.True(t, sameTime(got.RunIntentAt, sf.agent.RunIntentAt), "the same intent time")
	assert.NotNil(t, sf.held(t), "re-asserted: not a newer start")
	assert.EqualValues(t, 1, sf.backstops(backstopSiteSettle))
}

// The released conjunct of the settle's no-re-assert rule: a start claims
// the agent before the settle's release (so the conditional release does
// not happen), and its slot is then freed by an unguarded path. The settle
// did not release, so the backstop re-asserts, once, counted and audited.
func TestSettle_SupersedingStart_UnguardedReleaseSlipped_BackstopOnce(t *testing.T) {
	sf := newSettleFixture(t, "supersede-slip")
	sf.hook.beforeRelease = func() {
		_, err := sf.s.ClaimAgentStart(context.Background(), sf.agent.ID, "other-hub", store.StartClaimUser, "", time.Minute)
		require.NoError(t, err)
		sf.srv.quotaService.Release(context.Background(), store.LimitMaxAgentsPerBroker, sf.agent.ID) // unguarded
	}
	sf.fault.Arm()

	sf.settle()

	require.Nil(t, sf.hook.beforeRelease, "the claim and the slip happened before the settle's release")
	assert.NotNil(t, sf.held(t), "the backstop put the slot back")
	assert.EqualValues(t, 1, sf.backstops(backstopSiteSettle), "the backstop fired once")
	audits := capacityAudits(t, sf.s, auditQuotaBackstopReassert, sf.agent.ID)
	require.Len(t, audits, 1)
	assert.Contains(t, audits[0].AfterSummary, sf.brokerID)
}
