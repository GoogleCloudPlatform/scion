//go:build !no_sqlite

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

package hub

import (
	"context"
	"errors"
	"log/slog"
	"sync/atomic"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Tests for the failed move's reservation reconcile (ptone/scion#5441):
// ensureBrokerReservationOn writes nothing when the agent's reservation is
// already on the source, so the source's slot is never freed in between.

// failedMoveRecord records a failed move of the fixture agent from the
// source to the target, with no source cleanup outcome yet.
func failedMoveRecord(t *testing.T, f *moveFixture) *store.AgentReincarnation {
	t.Helper()
	a, err := f.s.GetAgent(context.Background(), f.agent.ID)
	require.NoError(t, err)
	require.Equal(t, f.src.ID, a.RuntimeBrokerID, "setup: the agent is on the source")
	rec := &store.AgentReincarnation{
		AgentID: a.ID, FromGeneration: 1, ToGeneration: 2, State: store.AgentReincarnationStateFailed,
		PreviousAppliedConfig: a.AppliedConfig, SourceBrokerID: f.src.ID, TargetBrokerID: f.dst.ID,
	}
	require.NoError(t, f.s.CreateAgentReincarnation(context.Background(), rec))
	return rec
}

// agentBrokerReservation returns the agent's active broker reservation.
func agentBrokerReservation(t *testing.T, s store.Store, agentID string) *store.UsageReservation {
	t.Helper()
	def, err := s.GetLimitDefinitionByName(context.Background(), store.LimitMaxAgentsPerBroker)
	require.NoError(t, err)
	res, err := s.GetActiveReservation(context.Background(), def.ID, agentID)
	require.NoError(t, err)
	return res
}

// reservationReadStore runs hook on every GetActiveReservation while its
// switch is armed, then answers it; readErr, when set, answers the first
// armed read only (the fallback's own re-assert reads normally).
type reservationReadStore struct {
	store.Store
	fault   *storeFaultSwitch
	hook    func()
	readErr error
	reads   atomic.Int32
}

func (r *reservationReadStore) GetActiveReservation(ctx context.Context, limitDefinitionID, resourceID string) (*store.UsageReservation, error) {
	if r.fault.Active() {
		n := r.reads.Add(1)
		if r.hook != nil {
			r.hook()
		}
		if r.readErr != nil && n == 1 {
			return nil, r.readErr
		}
	}
	return r.Store.GetActiveReservation(ctx, limitDefinitionID, resourceID)
}

// The reservation is already on the source: the reconcile keeps that same
// reservation (no release, no new one) and records the skipped cleanup.
func TestReconcileFailedMove_ReservationOnSourceWritesNothing(t *testing.T) {
	f := setupMoveFixture(t, true, nil)
	prepareMoveWorkerFixture(t, f)
	rec := failedMoveRecord(t, f)
	before := agentBrokerReservation(t, f.s, f.agent.ID)

	f.srv.reconcileFailedMove(context.Background(), rec.ID)

	after := agentBrokerReservation(t, f.s, f.agent.ID)
	assert.Equal(t, before.ID, after.ID, "want the same reservation kept: the reconcile released and re-created it")
	assert.EqualValues(t, 1, brokerReservationCount(t, f.s, f.src.ID))
	assert.EqualValues(t, 0, brokerReservationCount(t, f.s, f.dst.ID))
	got, err := f.s.GetAgentReincarnation(context.Background(), rec.ID)
	require.NoError(t, err)
	assert.Equal(t, sourceCleanupSkippedInterrupted, got.SourceCleanup)
}

// The source is at its cap with the agent's reservation. Another start
// tries a checked reserve on the source each time the reconcile reads the
// agent's reservation. The reconcile never frees the slot, so every such
// reserve is refused and the source stays at its cap. (Released and
// re-asserted, the slot would go to the other start in between, and the
// unchecked re-assert would put the source one over.)
func TestReconcileFailedMove_SourceAtCapNeverGoesOver(t *testing.T) {
	f := setupMoveFixture(t, true, nil)
	prepareMoveWorkerFixture(t, f)
	setBrokerAgentCeiling(t, f.s, 1)
	rec := failedMoveRecord(t, f)
	other := &store.Agent{ID: "competing-agent", RuntimeBrokerID: f.src.ID}
	var admitted, refused atomic.Int32
	w, fault := installStoreFault(t, f.srv, func(inner store.Store, sw *storeFaultSwitch) *reservationReadStore {
		return &reservationReadStore{Store: inner, fault: sw}
	})
	w.hook = func() {
		if _, err := f.srv.checkAndReserveBrokerQuota(context.Background(), other); err == nil {
			admitted.Add(1)
		} else {
			assert.ErrorIs(t, err, store.ErrQuotaExceeded)
			refused.Add(1)
		}
	}
	fault.Arm()

	f.srv.reconcileFailedMove(context.Background(), rec.ID)

	require.Positive(t, w.reads.Load(), "setup: the reconcile read the agent's reservation")
	assert.Zero(t, admitted.Load(), "want no start admitted on the full source during the reconcile")
	assert.EqualValues(t, 1, brokerReservationCount(t, f.s, f.src.ID), "want the source at its cap of 1, not over it")
	assert.Equal(t, f.src.ID, agentBrokerReservation(t, f.s, f.agent.ID).ScopeID)
}

// The reservation is still on the target (the rollback could not move it):
// the reconcile moves it to the source.
func TestReconcileFailedMove_ReservationOnTargetMovesToSource(t *testing.T) {
	f := setupMoveFixture(t, true, nil)
	prepareMoveWorkerFixture(t, f)
	rec := failedMoveRecord(t, f)
	f.srv.releaseBrokerQuota(context.Background(), withBroker(f.agent, f.src.ID))
	reserveBrokerSlot(t, f.s, f.dst, f.agent.ID)

	f.srv.reconcileFailedMove(context.Background(), rec.ID)

	assert.EqualValues(t, 1, brokerReservationCount(t, f.s, f.src.ID))
	assert.EqualValues(t, 0, brokerReservationCount(t, f.s, f.dst.ID))
}

// The agent holds no reservation: the reconcile records one on the source.
func TestReconcileFailedMove_NoReservationCreatesOne(t *testing.T) {
	f := setupMoveFixture(t, true, nil)
	prepareMoveWorkerFixture(t, f)
	rec := failedMoveRecord(t, f)
	f.srv.releaseBrokerQuota(context.Background(), withBroker(f.agent, f.src.ID))
	require.EqualValues(t, 0, brokerReservationCount(t, f.s, f.src.ID))

	f.srv.reconcileFailedMove(context.Background(), rec.ID)

	assert.EqualValues(t, 1, brokerReservationCount(t, f.s, f.src.ID))
	assert.Equal(t, f.src.ID, agentBrokerReservation(t, f.s, f.agent.ID).ScopeID)
}

// A failed read of the agent's reservation is logged and falls back to
// release and re-assert, so the reservation still moves to the source: it
// starts on the target here, where skipping the move would leave it.
func TestReconcileFailedMove_ReadErrorFallsBack(t *testing.T) {
	f := setupMoveFixture(t, true, nil)
	prepareMoveWorkerFixture(t, f)
	rec := failedMoveRecord(t, f)
	f.srv.releaseBrokerQuota(context.Background(), withBroker(f.agent, f.src.ID))
	reserveBrokerSlot(t, f.s, f.dst, f.agent.ID)
	logs := &syncBuffer{}
	f.srv.agentLifecycleLog = slog.New(slog.NewTextHandler(logs, nil))
	w, fault := installStoreFault(t, f.srv, func(inner store.Store, sw *storeFaultSwitch) *reservationReadStore {
		return &reservationReadStore{Store: inner, fault: sw, readErr: errors.New("injected reservation read failure")}
	})
	fault.Arm()

	f.srv.reconcileFailedMove(context.Background(), rec.ID)

	assert.GreaterOrEqual(t, w.reads.Load(), int32(2), "want the failed read and then the re-assert's own read")
	assert.Contains(t, logs.String(), "reading the agent's broker reservation failed; moving it anyway")
	assert.EqualValues(t, 1, brokerReservationCount(t, f.s, f.src.ID), "want the reservation moved to the source despite the read error")
	assert.EqualValues(t, 0, brokerReservationCount(t, f.s, f.dst.ID), "want no reservation left on the target")
}

// limitDefReadStore fails GetLimitDefinitionByName once (the first armed
// call) with readErr.
type limitDefReadStore struct {
	store.Store
	fault   *storeFaultSwitch
	readErr error
	reads   atomic.Int32
}

func (l *limitDefReadStore) GetLimitDefinitionByName(ctx context.Context, name string) (*store.LimitDefinition, error) {
	if l.fault.Active() {
		if l.reads.Add(1) == 1 {
			return nil, l.readErr
		}
	}
	return l.Store.GetLimitDefinitionByName(ctx, name)
}

// A failed read of the limit definition is logged and falls back the same
// way: the reservation moves from the target to the source.
func TestReconcileFailedMove_LimitReadErrorFallsBack(t *testing.T) {
	f := setupMoveFixture(t, true, nil)
	prepareMoveWorkerFixture(t, f)
	rec := failedMoveRecord(t, f)
	f.srv.releaseBrokerQuota(context.Background(), withBroker(f.agent, f.src.ID))
	reserveBrokerSlot(t, f.s, f.dst, f.agent.ID)
	logs := &syncBuffer{}
	f.srv.agentLifecycleLog = slog.New(slog.NewTextHandler(logs, nil))
	w, fault := installStoreFault(t, f.srv, func(inner store.Store, sw *storeFaultSwitch) *limitDefReadStore {
		return &limitDefReadStore{Store: inner, fault: sw, readErr: errors.New("injected limit read failure")}
	})
	fault.Arm()

	f.srv.reconcileFailedMove(context.Background(), rec.ID)

	assert.GreaterOrEqual(t, w.reads.Load(), int32(2), "want the failed read and then the re-assert's own read")
	assert.Contains(t, logs.String(), "reading the broker limit failed; moving the reservation anyway")
	assert.EqualValues(t, 1, brokerReservationCount(t, f.s, f.src.ID), "want the reservation moved to the source despite the read error")
	assert.EqualValues(t, 0, brokerReservationCount(t, f.s, f.dst.ID), "want no reservation left on the target")
}

// A forward move refused at the target's quota leaves the source
// reservation untouched (moveBrokerQuota is one store transaction,
// ptone/scion#5951), and the failed move's reconcile then finds it already on
// the source (ensureBrokerReservationOn writes nothing). Together they keep
// the very same reservation row through the move's failure, the failed
// move's reconcile, a second reconcile (as the stale sweep would run it) and
// a direct ensureBrokerReservationOn call: no release, no new row, and the
// source and target counts unchanged.
func TestMoveBrokerQuota_TargetFullThenReconcileKeepsSourceReservation(t *testing.T) {
	ctx := context.Background()
	f := setupMoveFixture(t, true, nil)
	prepareMoveWorkerFixture(t, f)
	setBrokerAgentCeiling(t, f.s, 1)
	reserveBrokerSlot(t, f.s, f.dst, tid("occupant"))
	before := agentBrokerReservation(t, f.s, f.agent.ID)
	require.Equal(t, f.src.ID, before.ScopeID, "setup: the reservation is on the source")

	err := f.srv.moveBrokerQuota(ctx, f.agent, &reincarnationMove{SourceBrokerID: f.src.ID, TargetBrokerID: f.dst.ID})
	var exceeded *QuotaExceededError
	require.ErrorAs(t, err, &exceeded, "the full target refuses the forward move")
	afterMove := agentBrokerReservation(t, f.s, f.agent.ID)
	assert.Equal(t, before.ID, afterMove.ID, "the refused forward move keeps the same source reservation")

	rec := failedMoveRecord(t, f)
	f.srv.reconcileFailedMove(ctx, rec.ID)
	afterReconcile := agentBrokerReservation(t, f.s, f.agent.ID)
	assert.Equal(t, before.ID, afterReconcile.ID, "the failed move's reconcile writes nothing: the reservation is already on the source")
	got, err := f.s.GetAgentReincarnation(ctx, rec.ID)
	require.NoError(t, err)
	assert.Equal(t, sourceCleanupSkippedInterrupted, got.SourceCleanup, "the reconcile ran")

	f.srv.reconcileFailedMove(ctx, rec.ID)
	cur, err := f.s.GetAgent(ctx, f.agent.ID)
	require.NoError(t, err)
	f.srv.ensureBrokerReservationOn(ctx, cur, f.src.ID)
	final := agentBrokerReservation(t, f.s, f.agent.ID)
	assert.Equal(t, before.ID, final.ID, "a repeated reconcile and a direct ensureBrokerReservationOn write nothing")
	assert.Equal(t, f.src.ID, final.ScopeID)
	assert.EqualValues(t, 1, brokerReservationCount(t, f.s, f.src.ID), "the source keeps exactly the agent's reservation")
	assert.EqualValues(t, 1, brokerReservationCount(t, f.s, f.dst.ID), "the target keeps only its occupant")
}
