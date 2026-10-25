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
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// A reincarnation starts the agent, also one that was stopped, so it is
// admitted like a start (ptone/scion#6033): the handler reserves the project
// slot, and unless the agent moves its broker slot, with the cap check,
// before the reincarnation is claimed.

// reincarnateHookStore is a store wrapper for the reincarnation capacity
// tests. While armed it runs onProjectRead once, at the first read of
// agentID's active project reservation (the handler reading the row it
// created, right before its claim), and onStopping at each write of
// agentID's stopping step (the worker's first write). It forwards the
// advisory lock methods, which store.Store does not include.
type reincarnateHookStore struct {
	store.Store
	fault         *storeFaultSwitch
	agentID       string
	projectDefID  string
	projectRead   atomic.Bool
	onProjectRead func()
	onStopping    func()
}

func (h *reincarnateHookStore) GetActiveReservation(ctx context.Context, limitDefID, resourceID string) (*store.UsageReservation, error) {
	if h.fault.Active() && resourceID == h.agentID && limitDefID == h.projectDefID && h.onProjectRead != nil &&
		h.projectRead.CompareAndSwap(false, true) {
		h.onProjectRead()
	}
	return h.Store.GetActiveReservation(ctx, limitDefID, resourceID)
}

func (h *reincarnateHookStore) UpdateAgent(ctx context.Context, a *store.Agent) error {
	if h.fault.Active() && a.ID == h.agentID && h.onStopping != nil &&
		a.Phase == string(state.PhaseStopping) && a.ReincarnationState == store.ReincarnationStateStopping {
		h.onStopping()
	}
	return h.Store.UpdateAgent(ctx, a)
}

func (h *reincarnateHookStore) TryAdvisoryLock(ctx context.Context, key store.AdvisoryLockKey) (bool, func() error, error) {
	return h.Store.(store.AdvisoryLocker).TryAdvisoryLock(ctx, key)
}

func (h *reincarnateHookStore) TryAdvisoryLockObject(ctx context.Context, classID store.AdvisoryLockKey, objID int32) (bool, func() error, error) {
	return h.Store.(store.AdvisoryLocker).TryAdvisoryLockObject(ctx, classID, objID)
}

// setupReincarnateHookServer is setupReincarnateTestServer with a
// reincarnateHookStore installed before any setup runs.
func setupReincarnateHookServer(t *testing.T, disp AgentDispatcher) (*Server, store.Store, *reincarnateHookStore, *storeFaultSwitch, *store.Project, *store.RuntimeBroker) {
	t.Helper()
	srv, s, hook, fault := testServerWithStoreFault(t, func(inner store.Store, f *storeFaultSwitch) *reincarnateHookStore {
		return &reincarnateHookStore{Store: inner, fault: f}
	})
	ctx := context.Background()
	project := &store.Project{
		ID:   tid("reincarnate-project-" + t.Name()),
		Name: "Reincarnate Test Project",
		Slug: "reincarnate-test-project-" + tidSlugSafe(t.Name()),
	}
	require.NoError(t, s.CreateProject(ctx, project))
	broker := &store.RuntimeBroker{
		ID:           tid("reincarnate-broker-" + t.Name()),
		Name:         "Reincarnate Test Broker",
		Slug:         "reincarnate-test-broker-" + tidSlugSafe(t.Name()),
		Status:       store.BrokerStatusOnline,
		Capabilities: &store.BrokerCapabilities{Reprovision: true},
	}
	require.NoError(t, s.CreateRuntimeBroker(ctx, broker))
	require.NoError(t, s.AddProjectProvider(ctx, &store.ProjectProvider{
		ProjectID: project.ID, BrokerID: broker.ID, BrokerName: broker.Name, Status: store.BrokerStatusOnline,
	}))
	project.DefaultRuntimeBrokerID = broker.ID
	require.NoError(t, s.UpdateProject(ctx, project))
	srv.SetDispatcher(disp)
	hook.projectDefID = projectLimitDefID(t, s)
	return srv, s, hook, fault, project, broker
}

// stoppedReincarnateAgent is newReincarnateTestAgent in phase stopped.
func stoppedReincarnateAgent(t *testing.T, s store.Store, project *store.Project, broker *store.RuntimeBroker) *store.Agent {
	t.Helper()
	return newReincarnateTestAgent(t, s, project, broker, func(a *store.Agent) { a.Phase = string(state.PhaseStopped) })
}

// seedProjectOccupant fills the project with one running agent holding its
// project row.
func seedProjectOccupant(t *testing.T, s store.Store, project *store.Project, broker *store.RuntimeBroker) *store.Agent {
	t.Helper()
	occ := newCreatorAgent(t, s, broker, project, "pq-reinc-occupant", state.PhaseRunning, tid("user-occupant"))
	seedProjectRow(t, s, occ)
	return occ
}

// 14: reincarnating a stopped agent at a full project is refused with the
// project message before anything is claimed or written.
func TestProjectQuota_ReincarnateStoppedAtFullProjectRefused(t *testing.T) {
	disp := newReincarnateTestDispatcher()
	srv, s, project, broker := setupReincarnateTestServer(t, disp)
	setProjectAgentCeiling(t, s, 1)
	seedProjectOccupant(t, s, project, broker)
	a := stoppedReincarnateAgent(t, s, project, broker)
	before := snapshotAgent(t, s, a.ID)

	rec := reincarnateAsDev(t, srv, a.ID, ReincarnateAgentRequest{})
	assertProjectQuotaExceeded(t, rec, project.ID, 1, 1)
	assertAgentUntouched(t, s, disp, a.ID, before)
	assert.False(t, hasProjectRow(t, s, a.ID))
}

// 14b: with room on the broker and a full project, the broker slot the
// handler took first is released after the project refusal.
func TestProjectQuota_ReincarnateProjectRefusalReleasesBrokerSlot(t *testing.T) {
	disp := newReincarnateTestDispatcher()
	srv, s, project, broker := setupReincarnateTestServer(t, disp)
	setBrokerAgentCeiling(t, s, 5)
	setProjectAgentCeiling(t, s, 1)
	seedProjectOccupant(t, s, project, broker)
	a := stoppedReincarnateAgent(t, s, project, broker)

	rec := reincarnateAsDev(t, srv, a.ID, ReincarnateAgentRequest{})
	assertProjectQuotaExceeded(t, rec, project.ID, 1, 1)
	assert.False(t, hasReservation(t, s, store.LimitMaxAgentsPerBroker, a.ID), "the broker slot is released")
	assert.False(t, hasProjectRow(t, s, a.ID))
}

// 15: a reincarnation that stays on its broker reserves the broker slot
// with the cap check: at a full broker it is refused with the unchanged
// broker message, before anything is claimed.
func TestProjectQuota_ReincarnateStoppedAtFullBrokerRefused(t *testing.T) {
	disp := newReincarnateTestDispatcher()
	srv, s, project, broker := setupReincarnateTestServer(t, disp)
	setBrokerAgentCeiling(t, s, 1)
	occ := newCreatorAgent(t, s, broker, project, "pq-reinc-broker-occ", state.PhaseRunning, tid("user-occupant"))
	reserveBrokerSlot(t, s, broker, occ.ID)
	a := stoppedReincarnateAgent(t, s, project, broker)
	before := snapshotAgent(t, s, a.ID)

	rec := reincarnateAsDev(t, srv, a.ID, ReincarnateAgentRequest{})
	assertBrokerQuotaExceeded(t, rec)
	assertAgentUntouched(t, s, disp, a.ID, before)
	assert.False(t, hasReservation(t, s, store.LimitMaxAgentsPerBroker, a.ID))
	assert.False(t, hasProjectRow(t, s, a.ID))
}

// 16 (running case): a running agent already holds its slots, so its
// reincarnation at a full project is admitted.
func TestProjectQuota_ReincarnateRunningAtFullProjectAdmitted(t *testing.T) {
	disp := newReincarnateTestDispatcher()
	srv, s, project, broker := setupReincarnateTestServer(t, disp)
	setProjectAgentCeiling(t, s, 1)
	a := newReincarnateTestAgent(t, s, project, broker, nil)
	seedProjectRow(t, s, a)

	rec := reincarnateAsDev(t, srv, a.ID, ReincarnateAgentRequest{})
	require.Equal(t, http.StatusAccepted, rec.Code, rec.Body.String())
	r := waitForReincarnationSettled(t, s, a.ID)
	assert.Equal(t, store.AgentReincarnationStateCompleted, r.State, r.Error)
	assert.True(t, hasProjectRow(t, s, a.ID))
}

// A claim that fails after the handler created both slots for a
// stopped agent (a version conflict) rolls both back.
func TestProjectQuota_ReincarnateClaimConflictRollsBack(t *testing.T) {
	disp := newReincarnateTestDispatcher()
	srv, s, hook, fault, project, broker := setupReincarnateHookServer(t, disp)
	setBrokerAgentCeiling(t, s, 5)
	setProjectAgentCeiling(t, s, 5)
	a := stoppedReincarnateAgent(t, s, project, broker)
	hook.agentID = a.ID
	hook.onProjectRead = func() {
		// A concurrent writer bumps the row's version before the claim.
		cur, err := s.GetAgent(context.Background(), a.ID)
		if assert.NoError(t, err) {
			cur.Labels = map[string]string{"bumped": "yes"}
			assert.NoError(t, s.UpdateAgent(context.Background(), cur))
		}
	}
	fault.Arm()

	rec := reincarnateAsDev(t, srv, a.ID, ReincarnateAgentRequest{})
	require.Equal(t, http.StatusConflict, rec.Code, rec.Body.String())
	require.True(t, hook.projectRead.Load(), "the claim failed after the handler created its rows")
	assert.False(t, hasReservation(t, s, store.LimitMaxAgentsPerBroker, a.ID), "the created broker slot is rolled back")
	assert.False(t, hasProjectRow(t, s, a.ID), "the created project slot is rolled back")
}

// When the claim fails because a start holds the agent, the
// conditional rollback leaves the rows to that start; once its claim is
// released with the agent idle, the claim-finish release frees them.
func TestProjectQuota_ReincarnateClaimHeldLeavesRowsToTheStart(t *testing.T) {
	disp := newReincarnateTestDispatcher()
	srv, s, project, broker := setupReincarnateTestServer(t, disp)
	ctx := context.Background()
	a := stoppedReincarnateAgent(t, s, project, broker)
	run, err := srv.acquireStartClaim(ctx, a, store.StartClaimUser)
	require.NoError(t, err)

	rec := reincarnateAsDev(t, srv, a.ID, ReincarnateAgentRequest{})
	require.Equal(t, http.StatusConflict, rec.Code, rec.Body.String())
	assert.True(t, hasProjectRow(t, s, a.ID), "the held start keeps the project row")

	run.finish(startReleased)
	assert.False(t, hasProjectRow(t, s, a.ID), "freed once the start's claim cleared with the agent idle")
	assert.False(t, hasReservation(t, s, store.LimitMaxAgentsPerBroker, a.ID))
}

// After the stop step succeeded, a failure that
// leaves no container releases the rows the reincarnation created,
// including one its own re-assert wrote; a stop-step failure releases
// nothing; rows the agent held before are left to the normal rules.
func TestProjectQuota_ReincarnateFailureReleasesOwnedRows(t *testing.T) {
	t.Run("post-stop-failure-releases-created", func(t *testing.T) {
		disp := newReincarnateTestDispatcher()
		disp.reprovisionErr = errors.New("simulated reprovision failure")
		srv, s, project, broker := setupReincarnateTestServer(t, disp)
		a := stoppedReincarnateAgent(t, s, project, broker)
		rec := reincarnateAsDev(t, srv, a.ID, ReincarnateAgentRequest{})
		require.Equal(t, http.StatusAccepted, rec.Code, rec.Body.String())
		r := waitForReincarnationSettled(t, s, a.ID)
		require.Equal(t, store.AgentReincarnationStateFailed, r.State)
		assert.False(t, hasReservation(t, s, store.LimitMaxAgentsPerBroker, a.ID))
		assert.False(t, hasProjectRow(t, s, a.ID))
	})
	t.Run("stop-failure-keeps", func(t *testing.T) {
		disp := newReincarnateTestDispatcher()
		disp.stopErr = errors.New("simulated stop failure")
		srv, s, project, broker := setupReincarnateTestServer(t, disp)
		a := stoppedReincarnateAgent(t, s, project, broker)
		rec := reincarnateAsDev(t, srv, a.ID, ReincarnateAgentRequest{})
		require.Equal(t, http.StatusAccepted, rec.Code, rec.Body.String())
		r := waitForReincarnationSettled(t, s, a.ID)
		require.Equal(t, store.AgentReincarnationStateFailed, r.State)
		assert.True(t, hasProjectRow(t, s, a.ID), "the old container may run: nothing is released")
	})
	t.Run("pre-existing-rows-kept", func(t *testing.T) {
		disp := newReincarnateTestDispatcher()
		disp.reprovisionErr = errors.New("simulated reprovision failure")
		srv, s, project, broker := setupReincarnateTestServer(t, disp)
		a := newReincarnateTestAgent(t, s, project, broker, nil)
		reserveBrokerSlot(t, s, broker, a.ID)
		seedProjectRow(t, s, a)
		rec := reincarnateAsDev(t, srv, a.ID, ReincarnateAgentRequest{})
		require.Equal(t, http.StatusAccepted, rec.Code, rec.Body.String())
		r := waitForReincarnationSettled(t, s, a.ID)
		require.Equal(t, store.AgentReincarnationStateFailed, r.State)
		assert.True(t, hasProjectRow(t, s, a.ID), "a row held before the reincarnation is not its to release")
	})
	t.Run("re-asserted-row-is-owned", func(t *testing.T) {
		disp := newReincarnateTestDispatcher()
		disp.reprovisionErr = errors.New("simulated reprovision failure")
		srv, s, hook, fault, project, broker := setupReincarnateHookServer(t, disp)
		a := stoppedReincarnateAgent(t, s, project, broker)
		hook.agentID = a.ID
		var released atomic.Bool
		hook.onStopping = func() {
			// A concurrent rollback frees the handler's project row just
			// before the stopping write; the worker's re-assert writes a
			// new one.
			if released.CompareAndSwap(false, true) {
				srv.quotaService.Release(context.Background(), store.LimitMaxAgentsPerProject, a.ID)
			}
		}
		fault.Arm()
		rec := reincarnateAsDev(t, srv, a.ID, ReincarnateAgentRequest{})
		require.Equal(t, http.StatusAccepted, rec.Code, rec.Body.String())
		r := waitForReincarnationSettled(t, s, a.ID)
		require.Equal(t, store.AgentReincarnationStateFailed, r.State)
		require.True(t, released.Load())
		require.NotEmpty(t, capacityAudits(t, s, auditQuotaProjectBackstopReassert, a.ID), "the re-assert wrote a row")
		assert.False(t, hasProjectRow(t, s, a.ID), "the re-asserted row is owned and released")
	})
}

// Two reincarnate requests X and Y on one stopped agent.
// Y claims while X waits right before its claim, so X's claim fails with a
// version conflict and X rolls back the rows it created, which Y relies on.
// Y's worker re-asserts them after its stopping write. The order is fixed
// with hooks: Y's worker is parked before its stopping write while X rolls
// back (control: the rows are gone), then released and parked again inside
// its stop dispatch, after the re-assert (the rows are back).
func TestProjectQuota_ConcurrentReincarnateWinnerKeepsRows(t *testing.T) {
	disp := newReincarnateTestDispatcher()
	stopEntered := make(chan struct{})
	releaseStop := make(chan struct{})
	var stopOnce, releaseStopOnce sync.Once
	disp.stopHook = func() {
		stopOnce.Do(func() { close(stopEntered) })
		<-releaseStop
	}
	srv, s, hook, fault, project, broker := setupReincarnateHookServer(t, disp)
	setBrokerAgentCeiling(t, s, 5)
	setProjectAgentCeiling(t, s, 5)
	a := stoppedReincarnateAgent(t, s, project, broker)
	hook.agentID = a.ID

	yParked := make(chan struct{})
	releaseY := make(chan struct{})
	var parkOnce, releaseYOnce sync.Once
	openY := func() { releaseYOnce.Do(func() { close(releaseY) }) }
	hook.onStopping = func() {
		parkOnce.Do(func() {
			close(yParked)
			<-releaseY
		})
	}
	var yCode atomic.Int32
	hook.onProjectRead = func() {
		// X created both rows and waits right before its claim: Y runs to
		// its claim and its worker is launched.
		yCode.Store(int32(reincarnateAsDev(t, srv, a.ID, ReincarnateAgentRequest{}).Code))
	}
	t.Cleanup(func() {
		// Open both parking points, so a failed assertion never leaves Y's
		// worker blocked and the settle wait cannot hang.
		openY()
		releaseStopOnce.Do(func() { close(releaseStop) })
		waitForReincarnationSettled(t, s, a.ID)
	})
	fault.Arm()

	recX := reincarnateAsDev(t, srv, a.ID, ReincarnateAgentRequest{})
	require.EqualValues(t, http.StatusAccepted, yCode.Load(), "Y was claimed")
	require.Equal(t, http.StatusConflict, recX.Code, recX.Body.String())

	select {
	case <-yParked:
	case <-time.After(5 * time.Second):
		t.Fatal("Y's worker did not reach its stopping write")
	}
	// Control: X's rollback released the rows (the agent still reads
	// stopped, and a reincarnation is not a start-kind claim).
	assert.False(t, hasReservation(t, s, store.LimitMaxAgentsPerBroker, a.ID), "control: X released the broker row")
	assert.False(t, hasProjectRow(t, s, a.ID), "control: X released the project row")

	openY()
	select {
	case <-stopEntered:
	case <-time.After(5 * time.Second):
		t.Fatal("Y's worker did not reach its stop dispatch")
	}
	assert.True(t, hasReservation(t, s, store.LimitMaxAgentsPerBroker, a.ID), "Y's re-assert restored the broker row")
	assert.True(t, hasProjectRow(t, s, a.ID), "Y's re-assert restored the project row")
}

// 21: a move reaches the same reserve (moves enter startReincarnation from
// the move planner): a running agent with no project row, at a full
// project, is refused before anything is claimed.
func TestProjectQuota_ReincarnateMoveAtFullProjectRefused(t *testing.T) {
	f := setupMoveFixture(t, true, nil)
	setProjectAgentCeiling(t, f.s, 1)
	seedProjectOccupant(t, f.s, f.project, f.src)

	rec := f.reincarnate(t, ReincarnateAgentRequest{Handoff: "moving on", TargetBroker: f.dst.ID})
	assertProjectQuotaExceeded(t, rec, f.project.ID, 1, 1)
	list, err := f.s.ListAgentReincarnations(context.Background(), f.agent.ID)
	require.NoError(t, err)
	assert.Empty(t, list, "nothing was claimed")
	assert.False(t, hasProjectRow(t, f.s, f.agent.ID))
}

// A move whose target filled after the request was
// accepted is refused when the worker moves the reservation
// (moveBrokerQuota's existing cap check), and afterwards the stopped agent
// holds no broker row (neither source nor target) and no project row.
func TestProjectQuota_MoveToFullTargetRefusedHoldsNoSlot(t *testing.T) {
	f := setupMoveFixture(t, true, nil)
	ctx := context.Background()
	setBrokerAgentCeiling(t, f.s, 1)
	occ := newCreatorAgent(t, f.s, f.dst, f.project, "pq-move-occ", state.PhaseRunning, tid("user-occupant"))
	reserveBrokerSlot(t, f.s, f.dst, occ.ID)

	a, err := f.s.GetAgent(ctx, f.agent.ID)
	require.NoError(t, err)
	a.Phase = string(state.PhaseStopped)
	a.ReincarnationState = store.ReincarnationStatePending
	require.NoError(t, f.s.UpdateAgent(ctx, a))
	_, err = f.s.SetAgentRunID(ctx, a.ID, "run-src", nil)
	require.NoError(t, err)
	created := seedProjectRow(t, f.s, a)
	rec := &store.AgentReincarnation{
		AgentID: a.ID, FromGeneration: 1, ToGeneration: 2, State: store.AgentReincarnationStatePending,
		PreviousAppliedConfig: a.AppliedConfig, SourceBrokerID: f.src.ID, TargetBrokerID: f.dst.ID,
	}
	require.NoError(t, f.s.CreateAgentReincarnation(ctx, rec))
	fresh := *a.AppliedConfig
	mv := &reincarnationMove{SourceBrokerID: f.src.ID, TargetBrokerID: f.dst.ID, ProjectID: f.project.ID, SelfMove: true, AgentDirWorkspace: true}
	a, err = f.s.GetAgent(ctx, a.ID)
	require.NoError(t, err)

	f.srv.runReincarnationWorker(ctx, a.ID, rec.ID, a.AppliedConfig, &fresh, "h", time.Now(), "", &ReincarnationPlan{}, 2, a.DeletionClaim, mv,
		&reincarnationCapacity{projectReservationID: created.ID})

	got, err := f.s.GetAgentReincarnation(ctx, rec.ID)
	require.NoError(t, err)
	require.Equal(t, store.AgentReincarnationStateFailed, got.State)
	assert.Contains(t, got.Error, "target broker quota")
	assert.EqualValues(t, 0, brokerReservationCount(t, f.s, f.src.ID), "no source row is left behind")
	assert.EqualValues(t, 1, brokerReservationCount(t, f.s, f.dst.ID), "only the target's occupant")
	assert.False(t, hasProjectRow(t, f.s, a.ID), "no project row is left behind")
}
