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
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/ent"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/GoogleCloudPlatform/scion/pkg/store/entadapter"
)

// Atomic broker reservation transfer (ptone/scion#5951): QuotaService.
// TransferBroker, transferBrokerReservationUnchecked and the forward move's
// moveBrokerQuota, on SQLite. The Postgres variants are in
// broker_reservation_transfer_postgres_test.go (-tags integration).

// transferTestFixture is a moving agent with its reservation on src, and a
// target broker dst.
type transferTestFixture struct {
	srv   *Server
	s     store.Store
	def   string
	src   *store.RuntimeBroker
	dst   *store.RuntimeBroker
	agent *store.Agent
	row   *store.UsageReservation
}

// newTransferTestFixture sets the max_agents_per_broker default to ceiling
// and creates the moving agent with its reservation on src.
func newTransferTestFixture(t *testing.T, srv *Server, s store.Store, ceiling int64) *transferTestFixture {
	t.Helper()
	setBrokerAgentCeiling(t, s, ceiling)
	def, err := s.GetLimitDefinitionByName(context.Background(), store.LimitMaxAgentsPerBroker)
	require.NoError(t, err)
	src, project := newQuotaTestBrokerAndProject(t, s, "xfer-src")
	dst, _ := newQuotaTestBrokerAndProject(t, s, "xfer-dst")
	agent := newQuotaTestAgent(t, s, src, project, "xfer-mover", state.PhaseStopped)
	row := seedBrokerRow(t, s, def.ID, src.ID, agent.ID, time.Time{})
	return &transferTestFixture{srv: srv, s: s, def: def.ID, src: src, dst: dst, agent: agent, row: row}
}

// fill makes n checked reservations on broker for fresh agents.
func (f *transferTestFixture) fill(t *testing.T, broker *store.RuntimeBroker, n int, prefix string) {
	t.Helper()
	for i := 0; i < n; i++ {
		created, err := f.srv.quotaService.Reserve(context.Background(), store.LimitMaxAgentsPerBroker,
			broker.ID, store.QuotaScopeBroker, broker.ID, tid(prefix+"-"+string(rune('a'+i))))
		require.NoError(t, err)
		require.True(t, created)
	}
}

// requireOnSource asserts the moving agent still holds exactly its seeded
// source row.
func (f *transferTestFixture) requireOnSource(t *testing.T) {
	t.Helper()
	got := activeReservation(t, f.s, f.def, f.agent.ID)
	require.NotNil(t, got, "the agent keeps its reservation")
	assert.Equal(t, f.row.ID, got.ID, "the source row is untouched")
	assert.Equal(t, f.src.ID, got.ScopeID)
}

// requireOnTarget asserts the moving agent holds one row on dst, a new one.
func (f *transferTestFixture) requireOnTarget(t *testing.T) {
	t.Helper()
	got := activeReservation(t, f.s, f.def, f.agent.ID)
	require.NotNil(t, got)
	assert.Equal(t, f.dst.ID, got.ScopeID)
	assert.Equal(t, store.QuotaScopeBroker, got.ScopeType)
	assert.NotEqual(t, f.row.ID, got.ID)
	assert.Zero(t, brokerReservationCount(t, f.s, f.src.ID), "the source row is released")
}

// contendedLockStore reports every quota advisory lock as held by another
// request, and counts the attempts.
type contendedLockStore struct {
	lockingPassthroughStore
	attempts *atomic.Int32
}

func (c contendedLockStore) TryAdvisoryLockObject(context.Context, store.AdvisoryLockKey, int32) (bool, func() error, error) {
	c.attempts.Add(1)
	return false, func() error { return nil }, nil
}

// contendLocks makes every advisory lock of srv's QuotaService contended and
// returns the attempt counter.
func contendLocks(srv *Server) *atomic.Int32 {
	var attempts atomic.Int32
	srv.quotaService.store = contendedLockStore{lockingPassthroughStore{srv.quotaService.store}, &attempts}
	return &attempts
}

// releaseTrackingLockStore forwards the advisory locks of the wrapped store
// and records when a quota advisory lock's release runs.
type releaseTrackingLockStore struct {
	lockingPassthroughStore
	released *atomic.Bool
}

func (r releaseTrackingLockStore) TryAdvisoryLockObject(ctx context.Context, class store.AdvisoryLockKey, obj int32) (bool, func() error, error) {
	acquired, release, err := r.lockingPassthroughStore.TryAdvisoryLockObject(ctx, class, obj)
	if err != nil {
		return acquired, release, err
	}
	return acquired, func() error {
		r.released.Store(true)
		return release()
	}, nil
}

// withScopeLockHeld holds the QuotaService's in-process mutex for broker's
// scope while fn runs, and fails the test if fn does not return within 10s
// (it would block on the mutex if it tried to take it).
func withScopeLockHeld(t *testing.T, qs *QuotaService, broker string, fn func()) {
	t.Helper()
	m := qs.lockForScope(store.LimitMaxAgentsPerBroker, store.QuotaScopeBroker, broker)
	m.Lock()
	defer m.Unlock()
	done := make(chan struct{})
	go func() {
		defer close(done)
		fn()
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("blocked on the scope mutex: the call took a lock it must not take")
	}
}

// AC1: the source is at its cap and the target is full. A checked Reserve
// for another agent on the source, run right after the move's quota write,
// is refused: the source never exceeds its cap, and the moving agent keeps
// its single reservation on the source.
func TestMoveBrokerQuota_SourceAtCapTargetFull_ConcurrentReserveRefused(t *testing.T) {
	srv, s := testServer(t)
	f := newTransferTestFixture(t, srv, s, 1)
	f.fill(t, f.dst, 1, "xfer-occupant")
	require.EqualValues(t, 1, brokerReservationCount(t, s, f.src.ID), "the source is at its cap")

	ran := false
	var reserveErr error
	t.Cleanup(setMoveBrokerQuotaTestHook(func(ctx context.Context) {
		ran = true
		_, reserveErr = srv.quotaService.Reserve(ctx, store.LimitMaxAgentsPerBroker,
			f.src.ID, store.QuotaScopeBroker, f.src.ID, tid("xfer-intruder"))
	}))

	err := srv.moveBrokerQuota(context.Background(), f.agent, &reincarnationMove{SourceBrokerID: f.src.ID, TargetBrokerID: f.dst.ID})
	var exceeded *QuotaExceededError
	require.ErrorAs(t, err, &exceeded, "AC1: the full target refuses the move")
	assert.Equal(t, f.dst.ID, exceeded.ScopeID)
	require.True(t, ran, "the hook ran")
	assert.ErrorIs(t, reserveErr, store.ErrQuotaExceeded, "the source had no free slot at any point")
	assert.EqualValues(t, 1, brokerReservationCount(t, s, f.src.ID), "the source never exceeds its cap")
	assert.EqualValues(t, 1, brokerReservationCount(t, s, f.dst.ID), "only the occupant")
	f.requireOnSource(t)
}

// AC2a: a transfer onto a target filled to its cap by checked Reserves is
// refused with a QuotaExceededError naming the target, and writes nothing.
func TestTransferBroker_TargetAtCapRefused(t *testing.T) {
	srv, s := testServer(t)
	f := newTransferTestFixture(t, srv, s, 2)
	f.fill(t, f.dst, 2, "xfer-full")

	_, limited, err := srv.quotaService.TransferBroker(context.Background(), f.agent.ID, f.dst.ID, TransferOpts{})
	assert.True(t, limited)
	var exceeded *QuotaExceededError
	require.ErrorAs(t, err, &exceeded, "AC2a: the full target refuses the transfer")
	assert.ErrorIs(t, err, store.ErrQuotaExceeded)
	assert.Equal(t, store.LimitMaxAgentsPerBroker, exceeded.Limit)
	assert.Equal(t, store.QuotaScopeBroker, exceeded.ScopeType)
	assert.Equal(t, f.dst.ID, exceeded.ScopeID)
	assert.EqualValues(t, 2, brokerReservationCount(t, s, f.dst.ID), "nothing written on the target")
	f.requireOnSource(t)
}

// Both target scope locks are held while the store transaction runs, and
// released only after TransferBroker's store call returns: at the store
// hook's "locked" step the target's scope mutex cannot be taken and the
// advisory lock's release has not run; after the call both are free. (On
// SQLite the mutex is what serializes the transfer against an in-process
// Reserve.)
func TestTransferBroker_TargetLocksHeldAcrossTransaction(t *testing.T) {
	srv, s := testServer(t)
	f := newTransferTestFixture(t, srv, s, 5)
	var released atomic.Bool
	srv.quotaService.store = releaseTrackingLockStore{lockingPassthroughStore{srv.quotaService.store}, &released}
	dstMutex := srv.quotaService.lockForScope(store.LimitMaxAgentsPerBroker, store.QuotaScopeBroker, f.dst.ID)

	ran, mutexFree, releasedInTx := false, false, false
	t.Cleanup(entadapter.SetTransferBrokerReservationTestHook(func(_ context.Context, _ *ent.Tx, step string, req store.BrokerReservationTransfer) error {
		if req.AgentID != f.agent.ID || step != "locked" {
			return nil
		}
		ran = true
		if dstMutex.TryLock() {
			dstMutex.Unlock()
			mutexFree = true
		}
		releasedInTx = released.Load()
		return nil
	}))

	res, limited, err := srv.quotaService.TransferBroker(context.Background(), f.agent.ID, f.dst.ID, TransferOpts{})
	require.NoError(t, err)
	assert.True(t, limited)
	assert.Equal(t, store.TransferMoved, res.Outcome)
	require.True(t, ran, "the hook ran")
	assert.False(t, mutexFree, "the target scope mutex is held during the store transaction")
	assert.False(t, releasedInTx, "the target advisory lock is held during the store transaction")
	assert.True(t, released.Load(), "the target advisory lock is released after the call")
	require.True(t, dstMutex.TryLock(), "the target scope mutex is released after the call")
	dstMutex.Unlock()
	f.requireOnTarget(t)
}

// Below the cap, the checked transfer moves the row.
func TestTransferBroker_BelowCapMoves(t *testing.T) {
	srv, s := testServer(t)
	f := newTransferTestFixture(t, srv, s, 2)
	f.fill(t, f.dst, 1, "xfer-room")

	res, limited, err := srv.quotaService.TransferBroker(context.Background(), f.agent.ID, f.dst.ID, TransferOpts{})
	require.NoError(t, err)
	assert.True(t, limited)
	assert.Equal(t, store.TransferMoved, res.Outcome)
	assert.Equal(t, f.row.ID, res.ReleasedID)
	f.requireOnTarget(t)
	assert.EqualValues(t, 2, brokerReservationCount(t, s, f.dst.ID))
}

// A self-move (source equals target) is AlreadyOn and writes nothing.
func TestMoveBrokerQuota_SelfMoveWritesNothing(t *testing.T) {
	srv, s := testServer(t)
	f := newTransferTestFixture(t, srv, s, 1)

	res, limited, err := srv.quotaService.TransferBroker(context.Background(), f.agent.ID, f.src.ID, TransferOpts{})
	require.NoError(t, err)
	assert.True(t, limited)
	assert.Equal(t, store.TransferAlreadyOn, res.Outcome)
	assert.Equal(t, f.row.ID, res.ReservationID)
	f.requireOnSource(t)

	require.NoError(t, srv.moveBrokerQuota(context.Background(), f.agent, &reincarnationMove{SourceBrokerID: f.src.ID, TargetBrokerID: f.src.ID, SelfMove: true}))
	f.requireOnSource(t)
	assert.EqualValues(t, 1, brokerReservationCount(t, s, f.src.ID))
}

// Lock contention with enforcement on returns ErrQuotaLockContention, and
// the source reservation is unchanged.
func TestTransferBroker_LockContentionEnforced(t *testing.T) {
	srv, s := testServer(t)
	f := newTransferTestFixture(t, srv, s, 5)
	attempts := contendLocks(srv)

	_, limited, err := srv.quotaService.TransferBroker(context.Background(), f.agent.ID, f.dst.ID, TransferOpts{})
	assert.True(t, limited)
	require.ErrorIs(t, err, ErrQuotaLockContention, "a contended target lock refuses the transfer")
	assert.EqualValues(t, 1, attempts.Load())
	f.requireOnSource(t)
	assert.Zero(t, brokerReservationCount(t, s, f.dst.ID))

	err = srv.moveBrokerQuota(context.Background(), f.agent, &reincarnationMove{SourceBrokerID: f.src.ID, TargetBrokerID: f.dst.ID})
	require.ErrorIs(t, err, ErrQuotaLockContention)
	f.requireOnSource(t)
}

// With enforcement off the transfer moves the row with Cap 0: onto a full
// target, and also when the advisory lock is contended.
func TestTransferBroker_EnforcementOffMovesUnchecked(t *testing.T) {
	t.Run("full target", func(t *testing.T) {
		srv, s := testServer(t)
		f := newTransferTestFixture(t, srv, s, 1)
		f.fill(t, f.dst, 1, "xfer-off-full")
		srv.quotaService.enforced = func(string) bool { return false }

		res, limited, err := srv.quotaService.TransferBroker(context.Background(), f.agent.ID, f.dst.ID, TransferOpts{})
		require.NoError(t, err)
		assert.True(t, limited)
		assert.Equal(t, store.TransferMoved, res.Outcome)
		f.requireOnTarget(t)
		assert.EqualValues(t, 2, brokerReservationCount(t, s, f.dst.ID), "over the cap: enforcement is off")
	})
	t.Run("contended lock", func(t *testing.T) {
		srv, s := testServer(t)
		f := newTransferTestFixture(t, srv, s, 5)
		attempts := contendLocks(srv)
		srv.quotaService.enforced = func(string) bool { return false }

		res, limited, err := srv.quotaService.TransferBroker(context.Background(), f.agent.ID, f.dst.ID, TransferOpts{})
		require.NoError(t, err)
		assert.True(t, limited)
		assert.Equal(t, store.TransferMoved, res.Outcome)
		assert.EqualValues(t, 1, attempts.Load(), "enforcement off still tries the target lock")
		f.requireOnTarget(t)
	})
}

// An unlimited target takes no lock (neither the scope mutex nor the
// advisory lock) and moves the row.
func TestTransferBroker_UnlimitedTakesNoLock(t *testing.T) {
	srv, s := testServer(t)
	f := newTransferTestFixture(t, srv, s, 0)
	attempts := contendLocks(srv)

	var res store.BrokerReservationTransferResult
	var limited bool
	var err error
	withScopeLockHeld(t, srv.quotaService, f.dst.ID, func() {
		res, limited, err = srv.quotaService.TransferBroker(context.Background(), f.agent.ID, f.dst.ID, TransferOpts{})
	})
	require.NoError(t, err)
	assert.True(t, limited)
	assert.Equal(t, store.TransferMoved, res.Outcome)
	assert.Zero(t, attempts.Load(), "no advisory lock attempted")
	f.requireOnTarget(t)
}

// With no limit definition the transfer is a no-op: limited is false and
// nothing is written.
func TestTransferBroker_NoLimitDefinitionNoOp(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()
	def, err := s.GetLimitDefinitionByName(ctx, store.LimitMaxAgentsPerBroker)
	require.NoError(t, err)
	require.NoError(t, s.DeleteLimitDefinition(ctx, def.ID))
	src, project := newQuotaTestBrokerAndProject(t, s, "xfer-nodef-src")
	dst, _ := newQuotaTestBrokerAndProject(t, s, "xfer-nodef-dst")
	agent := newQuotaTestAgent(t, s, src, project, "xfer-nodef", state.PhaseStopped)

	res, limited, err := srv.quotaService.TransferBroker(ctx, agent.ID, dst.ID, TransferOpts{})
	require.NoError(t, err)
	assert.False(t, limited)
	assert.Equal(t, store.BrokerReservationTransferResult{}, res)

	require.NoError(t, srv.moveBrokerQuota(ctx, agent, &reincarnationMove{SourceBrokerID: src.ID, TargetBrokerID: dst.ID}))

	res, limited, err = srv.transferBrokerReservationUnchecked(ctx, agent.ID, src.ID, TransferOpts{CreateIfAbsent: true})
	require.NoError(t, err)
	assert.False(t, limited)
	assert.Equal(t, store.BrokerReservationTransferResult{}, res)
}

// A transfer to an empty broker ID is refused with store.ErrInvalidTransfer
// before any lookup, on the checked and the unchecked path, and nothing is
// written: the agent keeps its source row and no row lands on scope "".
func TestTransferBroker_EmptyTargetRefused(t *testing.T) {
	srv, s := testServer(t)
	f := newTransferTestFixture(t, srv, s, 5)

	res, limited, err := srv.quotaService.TransferBroker(context.Background(), f.agent.ID, "", TransferOpts{})
	require.ErrorIs(t, err, store.ErrInvalidTransfer, "an empty target broker is refused")
	assert.False(t, limited)
	assert.Equal(t, store.BrokerReservationTransferResult{}, res)

	res, limited, err = srv.transferBrokerReservationUnchecked(context.Background(), f.agent.ID, "", TransferOpts{CreateIfAbsent: true})
	require.ErrorIs(t, err, store.ErrInvalidTransfer, "an empty target broker is refused unchecked too")
	assert.False(t, limited)
	assert.Equal(t, store.BrokerReservationTransferResult{}, res)

	f.requireOnSource(t)
	assert.Zero(t, brokerReservationCount(t, s, ""), "nothing on an empty scope")
}

// moveBrokerQuota of an agent holding no reservation (an idle release freed
// the source slot) makes the plain checked reservation on the target.
func TestMoveBrokerQuota_AbsentReservesOnTarget(t *testing.T) {
	srv, s := testServer(t)
	f := newTransferTestFixture(t, srv, s, 1)
	require.NoError(t, s.ReleaseReservationByID(context.Background(), f.row.ID))

	require.NoError(t, srv.moveBrokerQuota(context.Background(), f.agent, &reincarnationMove{SourceBrokerID: f.src.ID, TargetBrokerID: f.dst.ID}))
	got := activeReservation(t, s, f.def, f.agent.ID)
	require.NotNil(t, got)
	assert.Equal(t, f.dst.ID, got.ScopeID)
}

// moveBrokerQuota moves a reservation found on a broker other than the
// source onto the target (and logs the anomaly).
func TestMoveBrokerQuota_RowOnOtherBrokerMovesToTarget(t *testing.T) {
	srv, s := testServer(t)
	f := newTransferTestFixture(t, srv, s, 5)
	other, _ := newQuotaTestBrokerAndProject(t, s, "xfer-other")
	require.NoError(t, s.ReleaseReservationByID(context.Background(), f.row.ID))
	stray := seedBrokerRow(t, s, f.def, other.ID, f.agent.ID, time.Time{})

	require.NoError(t, srv.moveBrokerQuota(context.Background(), f.agent, &reincarnationMove{SourceBrokerID: f.src.ID, TargetBrokerID: f.dst.ID}))
	got := activeReservation(t, s, f.def, f.agent.ID)
	require.NotNil(t, got)
	assert.Equal(t, f.dst.ID, got.ScopeID)
	assert.NotEqual(t, stray.ID, got.ID)
	assert.Zero(t, brokerReservationCount(t, s, other.ID))
}

// transferBrokerReservationUnchecked applies no cap and takes no lock: it
// moves onto a full target while the target's scope mutex is held and every
// advisory lock is contended, and creates when CreateIfAbsent is set.
func TestTransferBrokerReservationUnchecked_NoCapNoLocks(t *testing.T) {
	srv, s := testServer(t)
	f := newTransferTestFixture(t, srv, s, 1)
	f.fill(t, f.dst, 1, "xfer-unchecked")
	attempts := contendLocks(srv)

	var res store.BrokerReservationTransferResult
	var limited bool
	var err error
	withScopeLockHeld(t, srv.quotaService, f.dst.ID, func() {
		res, limited, err = srv.transferBrokerReservationUnchecked(context.Background(), f.agent.ID, f.dst.ID, TransferOpts{})
	})
	require.NoError(t, err)
	assert.True(t, limited)
	assert.Equal(t, store.TransferMoved, res.Outcome)
	assert.Zero(t, attempts.Load(), "no advisory lock attempted")
	f.requireOnTarget(t)
	assert.EqualValues(t, 2, brokerReservationCount(t, s, f.dst.ID), "unchecked: over the cap")

	// None held: CreateIfAbsent with RequireAgentOnTo records it on the
	// agent row's broker.
	require.NoError(t, s.ReleaseReservationByID(context.Background(), res.ReservationID))
	res, limited, err = srv.transferBrokerReservationUnchecked(context.Background(), f.agent.ID, f.src.ID, TransferOpts{CreateIfAbsent: true, RequireAgentOnTo: true})
	require.NoError(t, err)
	assert.True(t, limited)
	assert.Equal(t, store.TransferCreated, res.Outcome)
	got := activeReservation(t, s, f.def, f.agent.ID)
	require.NotNil(t, got)
	assert.Equal(t, f.src.ID, got.ScopeID)

	// A store error is returned wrapped.
	_, _, err = srv.transferBrokerReservationUnchecked(context.Background(), f.agent.ID, f.src.ID, TransferOpts{CreateIfAbsent: true, FromReservationID: got.ID})
	assert.ErrorIs(t, err, store.ErrInvalidTransfer)
}
