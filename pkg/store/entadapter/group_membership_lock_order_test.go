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

package entadapter

import (
	"context"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/ent"
	"github.com/GoogleCloudPlatform/scion/pkg/ent/agent"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/GoogleCloudPlatform/scion/pkg/store/enttest"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Lock-order tests for ptone/scion#2769 PR4 (PostgreSQL only). Purge and
// finalize-hard lock an agent row FOR UPDATE, then delete its group
// memberships, then delete the agent. CompositeStore.DeleteProject and
// CompositeStore.DeleteAgent must take locks in the same order (agent, then
// membership, then agent delete). If they deleted the memberships first they
// would hold the membership row locks while waiting for the agent row, and a
// concurrent purge or finalize holding that agent row and then deleting the
// same membership would deadlock with it (SQLSTATE 40P01).
//
// Each test plays the purge/finalize side by hand in transaction T1: lock
// agent A FOR UPDATE, wait until the delete under test is blocked on a lock,
// delete A's membership, commit. Neither side may fail.

// lockOrderTimeout bounds every wait in these tests, so a regression fails
// instead of hanging the job. It is well above PostgreSQL's default
// deadlock_timeout (1s), so a deadlock is detected and reported as an error
// before the timeout fires.
const lockOrderTimeout = 30 * time.Second

func skipUnlessPostgres(t *testing.T) {
	t.Helper()
	if !enttest.Active() {
		t.Skip("requires -tags integration and SCION_TEST_POSTGRES_URL")
	}
}

func runMembershipLockOrderRace(t *testing.T, f agentGroupFixture, deleteUnderTest func(context.Context) error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), lockOrderTimeout)
	defer cancel()
	db := f.cs.DB()
	require.NotNil(t, db, "raw database handle needed to watch for lock waits")

	// T1: the purge/finalize side. Lock agent A first.
	tx, err := f.cs.client.Tx(ctx)
	require.NoError(t, err)
	t1Done := false
	defer func() {
		if !t1Done {
			_ = tx.Rollback()
		}
	}()
	_, err = tx.Agent.Query().Where(agent.IDEQ(uuid.MustParse(f.a.ID))).ForUpdate().IDs(ctx)
	require.NoError(t, err)

	// T2: the delete under test, which must wait for T1's lock on A.
	deleteErr := make(chan error, 1)
	go func() { deleteErr <- deleteUnderTest(ctx) }()

	// Wait until T2 is blocked on a lock. With the fix it blocks on the
	// agent-row FOR UPDATE before touching any membership; without it, it
	// has already deleted the memberships and blocks on the agent delete.
	// enttest gives each package its own database, so current_database()
	// scopes the check to this test's connections.
	require.Eventually(t, func() bool {
		var waiting int
		if err := db.QueryRowContext(ctx,
			`SELECT count(*) FROM pg_stat_activity
			  WHERE datname = current_database() AND wait_event_type = 'Lock'`).Scan(&waiting); err != nil {
			return false
		}
		return waiting > 0
	}, lockOrderTimeout/2, 20*time.Millisecond, "the delete under test never waited on agent A's row lock")

	// T1 now deletes A's membership, as purge and finalize-hard do after
	// locking the agent, and commits.
	_, t1Err := newTxCompositeStore(tx).DeleteGroupMembershipsForAgents(ctx, []string{f.a.ID})
	if t1Err == nil {
		t1Err = tx.Commit()
	} else {
		_ = tx.Rollback()
	}
	t1Done = true

	var t2Err error
	select {
	case t2Err = <-deleteErr:
	case <-time.After(lockOrderTimeout):
		t.Fatal("the delete under test did not finish")
	}
	assert.NoError(t, t1Err, "the agent-first transaction must not fail (40P01 means a lock-order inversion)")
	assert.NoError(t, t2Err, "the delete under test must not fail (40P01 means a lock-order inversion)")
}

// TestCompositeDeleteProject_LockOrderNoDeadlock: DeleteProject locks the
// project's agent rows before deleting their memberships.
func TestCompositeDeleteProject_LockOrderNoDeadlock(t *testing.T) {
	skipUnlessPostgres(t)
	f := newAgentGroupFixture(t)
	runMembershipLockOrderRace(t, f, func(ctx context.Context) error {
		return f.cs.DeleteProject(ctx, f.projectID)
	})
	if t.Failed() {
		return
	}
	assert.Zero(t, orphanedMembershipCount(t, f.cs), "no NULL row left behind")
	assert.Equal(t, 1, membershipRowCount(t, f.cs, f.group), "only the user owner remains")
}

// TestCompositeDeleteAgent_LockOrderNoDeadlock: DeleteAgent locks the agent
// row before deleting its memberships.
func TestCompositeDeleteAgent_LockOrderNoDeadlock(t *testing.T) {
	skipUnlessPostgres(t)
	f := newAgentGroupFixture(t)
	runMembershipLockOrderRace(t, f, func(ctx context.Context) error {
		return f.cs.DeleteAgent(ctx, f.a.ID)
	})
	if t.Failed() {
		return
	}
	assert.Zero(t, orphanedMembershipCount(t, f.cs), "no NULL row left behind")
	assert.Equal(t, 2, membershipRowCount(t, f.cs, f.group), "owner and the other agent remain")
}

// waitForLockWaiter blocks until some connection to this test's database is
// waiting on a lock (see runMembershipLockOrderRace).
func waitForLockWaiter(t *testing.T, ctx context.Context, cs *CompositeStore) {
	t.Helper()
	db := cs.DB()
	require.NotNil(t, db, "raw database handle needed to watch for lock waits")
	require.Eventually(t, func() bool {
		var waiting int
		if err := db.QueryRowContext(ctx,
			`SELECT count(*) FROM pg_stat_activity
			  WHERE datname = current_database() AND wait_event_type = 'Lock'`).Scan(&waiting); err != nil {
			return false
		}
		return waiting > 0
	}, lockOrderTimeout/2, 20*time.Millisecond, "nothing ever waited on a row lock")
}

// Fixed agent IDs at both ends of the ID range, so the expected lock order
// does not depend on random UUIDs.
const (
	lockOrderLoID = "00000000-0000-4000-8000-000000000001"
	lockOrderHiID = "ffffffff-ffff-4fff-bfff-ffffffffffff"
)

// newLoHiProject creates a project with two agents, hi created before lo, so
// heap (insertion) order is the reverse of ID order. Only an explicit
// ORDER BY id makes a query see lo first.
func newLoHiProject(t *testing.T, cs *CompositeStore, softDeleted bool) string {
	t.Helper()
	ctx := context.Background()
	pid := uuid.NewString()
	require.NoError(t, cs.CreateProject(ctx, &store.Project{ID: pid, Name: "lohi", Slug: "lohi-" + pid[:8]}))
	hi := makeAgent(pid, "hi")
	hi.ID = lockOrderHiID
	lo := makeAgent(pid, "lo")
	lo.ID = lockOrderLoID
	for _, a := range []*store.Agent{hi, lo} {
		require.NoError(t, cs.CreateAgent(ctx, a))
		if softDeleted {
			got, err := cs.GetAgent(ctx, a.ID)
			require.NoError(t, err)
			got.DeletedAt = time.Now().Add(-48 * time.Hour)
			require.NoError(t, cs.UpdateAgent(ctx, got))
		}
	}
	return pid
}

// TestPurgeDeletedAgents_CrossBatchLockOrderNoDeadlock (review r3 L1): the
// purge's batches must be in ascending ID order, so the purge locks agents in
// ID order across batches, like DeleteProject. With batch size 1 and hi
// inserted before lo, an unordered candidate query puts hi in batch 1: the
// purge locks hi, DeleteProject locks lo and waits on hi, and purge batch 2
// waits on lo (40P01). Ordered, batch 1 is lo, so DeleteProject waits on lo
// while holding nothing and both finish.
func TestPurgeDeletedAgents_CrossBatchLockOrderNoDeadlock(t *testing.T) {
	skipUnlessPostgres(t)
	ctx, cancel := context.WithTimeout(context.Background(), lockOrderTimeout)
	defer cancel()
	cs := newTestCompositeStore(t)
	pid := newLoHiProject(t, cs, true)

	orig := purgeDeletedAgentsBatchSize
	purgeDeletedAgentsBatchSize = 1
	t.Cleanup(func() {
		purgeDeletedAgentsBatchSize = orig
		purgeDeletedAgentsTestHook = nil
	})

	var order []string
	deleteErr := make(chan error, 1)
	purgeDeletedAgentsTestHook = func(_ *ent.Tx, batch []uuid.UUID) {
		order = append(order, batch[0].String())
		// Batch 1 is locked by now; start the project delete before batch 2
		// locks, and let it block.
		if len(order) == 2 {
			go func() { deleteErr <- cs.DeleteProject(ctx, pid) }()
			waitForLockWaiter(t, ctx, cs)
		}
	}
	_, purgeErr := cs.PurgeDeletedAgents(ctx, time.Now().Add(-24*time.Hour))

	var dErr error
	select {
	case dErr = <-deleteErr:
	case <-time.After(lockOrderTimeout):
		t.Fatal("DeleteProject did not finish")
	}
	assert.Equal(t, []string{lockOrderLoID, lockOrderHiID}, order, "purge batches must be in ascending agent-ID order")
	assert.NoError(t, purgeErr, "purge must not fail (40P01 means batches lock out of ID order)")
	assert.NoError(t, dErr, "DeleteProject must not fail (40P01 means batches lock out of ID order)")
}

// TestCompositeDeleteProject_LocksAgentsInIDOrder (review r3 N1): pins the
// ascending order of DeleteProject's agent locks (lockProjectAgentIDs, which
// LockProjectAgents shares). T1 locks lo, DeleteProject starts and must block
// on lo while holding no agent lock, then T1 locks hi and commits. If
// DeleteProject locked in any other order (hi first), T1's lock on hi would
// close a cycle and one side would fail with 40P01.
func TestCompositeDeleteProject_LocksAgentsInIDOrder(t *testing.T) {
	skipUnlessPostgres(t)
	ctx, cancel := context.WithTimeout(context.Background(), lockOrderTimeout)
	defer cancel()
	cs := newTestCompositeStore(t)
	pid := newLoHiProject(t, cs, false)

	tx, err := cs.client.Tx(ctx)
	require.NoError(t, err)
	t1Done := false
	defer func() {
		if !t1Done {
			_ = tx.Rollback()
		}
	}()
	_, err = tx.Agent.Query().Where(agent.IDEQ(uuid.MustParse(lockOrderLoID))).ForUpdate().IDs(ctx)
	require.NoError(t, err)

	deleteErr := make(chan error, 1)
	go func() { deleteErr <- cs.DeleteProject(ctx, pid) }()
	waitForLockWaiter(t, ctx, cs)

	_, t1Err := tx.Agent.Query().Where(agent.IDEQ(uuid.MustParse(lockOrderHiID))).ForUpdate().IDs(ctx)
	if t1Err == nil {
		t1Err = tx.Commit()
	} else {
		_ = tx.Rollback()
	}
	t1Done = true

	var dErr error
	select {
	case dErr = <-deleteErr:
	case <-time.After(lockOrderTimeout):
		t.Fatal("DeleteProject did not finish")
	}
	assert.NoError(t, t1Err, "the ascending locker must not fail (40P01 means DeleteProject locks out of ID order)")
	assert.NoError(t, dErr, "DeleteProject must not fail (40P01 means it locks out of ID order)")
}
