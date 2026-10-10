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

// TestDeleteGroupMembershipsForUser_LockOrderVsProjectGroupCascade (review
// r4 L1; PostgreSQL only): a user delete (DeleteGroupMembershipsForUser,
// then DeleteUser, as deleteUser and the allow-list delete do) must not
// deadlock against ProjectDeletionService's group cascade, which deletes one
// project group at a time: its memberships, then its row.
//
// User U owns project group Ga (deleted first) and is a member of project
// group Gb (deleted later). T2 plays the project delete with the same store
// calls as ProjectDeletionService: lock the project and its agents, delete
// Ga (holding Ga's row), pause, delete Gb, delete the project. While T2 is
// paused, T1 deletes U. If T1 deleted M(U,Gb) before locking the groups U
// owns, it would hold M(U,Gb) while its user-row delete waits on Ga's row
// (owner_id ON DELETE SET NULL), and T2's delete of Gb would wait on
// M(U,Gb): deadlock (SQLSTATE 40P01). DeleteGroupMembershipsForUser locks
// U's owned groups first, so T1 waits on Ga before touching any membership.
func TestDeleteGroupMembershipsForUser_LockOrderVsProjectGroupCascade(t *testing.T) {
	skipUnlessPostgres(t)
	ctx, cancel := context.WithTimeout(context.Background(), lockOrderTimeout)
	defer cancel()
	cs := newTestCompositeStore(t)
	db := cs.DB()
	require.NotNil(t, db, "raw database handle needed to watch for lock waits")

	projectID := uuid.NewString()
	require.NoError(t, cs.CreateProject(ctx, &store.Project{ID: projectID, Name: "uo", Slug: "uo-" + projectID[:8]}))
	u := newCleanupUser(t, cs, "uo-user@example.com")
	ga, gb := uuid.NewString(), uuid.NewString()
	require.NoError(t, cs.CreateGroup(ctx, &store.Group{
		ID: ga, Name: "uo-ga", Slug: "uo-ga-" + ga[:8], GroupType: store.GroupTypeExplicit,
		ProjectID: projectID, OwnerID: u,
	}))
	require.NoError(t, cs.CreateGroup(ctx, &store.Group{
		ID: gb, Name: "uo-gb", Slug: "uo-gb-" + gb[:8], GroupType: store.GroupTypeExplicit,
		ProjectID: projectID,
	}))
	addCleanupMember(t, cs, gb, store.GroupMemberTypeUser, u, store.GroupMemberRoleMember)

	// T2: the project delete, paused after deleting Ga.
	paused := make(chan struct{})
	resume := make(chan struct{})
	t2Err := make(chan error, 1)
	go func() {
		t2Err <- cs.WithTx(ctx, func(tx store.Store) error {
			if err := tx.LockProjectForMembership(ctx, projectID); err != nil {
				return err
			}
			if err := tx.LockProjectAgents(ctx, projectID); err != nil {
				return err
			}
			if err := tx.DeleteGroup(ctx, ga); err != nil {
				return err
			}
			close(paused)
			select {
			case <-resume:
			case <-ctx.Done():
				return ctx.Err()
			}
			if err := tx.DeleteGroup(ctx, gb); err != nil {
				return err
			}
			return tx.DeleteProject(ctx, projectID)
		})
	}()
	select {
	case <-paused:
	case err := <-t2Err:
		t.Fatalf("project delete failed before pausing: %v", err)
	case <-ctx.Done():
		t.Fatal("project delete never paused")
	}

	// T1: the user delete, which must wait on T2's lock on Ga.
	t1Err := make(chan error, 1)
	go func() {
		t1Err <- cs.WithTx(ctx, func(tx store.Store) error {
			if _, err := tx.DeleteGroupMembershipsForUser(ctx, u); err != nil {
				return err
			}
			return tx.DeleteUser(ctx, u)
		})
	}()

	// Wait until T1 is blocked on a lock. With the fix it blocks on the
	// owned-group lock before deleting any membership; without it, it has
	// already deleted M(U,Gb) and blocks on the user-row delete.
	require.Eventually(t, func() bool {
		var waiting int
		if err := db.QueryRowContext(ctx,
			`SELECT count(*) FROM pg_stat_activity
			  WHERE datname = current_database() AND wait_event_type = 'Lock'`).Scan(&waiting); err != nil {
			return false
		}
		return waiting > 0
	}, lockOrderTimeout/2, 20*time.Millisecond, "the user delete never waited on the owned group's row lock")
	close(resume)

	var err1, err2 error
	for i := 0; i < 2; i++ {
		select {
		case err1 = <-t1Err:
		case err2 = <-t2Err:
		case <-time.After(lockOrderTimeout):
			t.Fatal("a delete did not finish")
		}
	}
	assert.NoError(t, err2, "the project delete must not fail (40P01 means a lock-order inversion)")
	assert.NoError(t, err1, "the user delete must not fail (40P01 means a lock-order inversion)")
	if t.Failed() {
		return
	}
	_, err := cs.GetUser(ctx, u)
	assert.ErrorIs(t, err, store.ErrNotFound)
	_, err = cs.GetProject(ctx, projectID)
	assert.ErrorIs(t, err, store.ErrNotFound)
	assert.Zero(t, orphanedMembershipCount(t, cs))
}

// TestDeleteGroupMembershipsForUser_OwnedGroupLockAllowsMemberInsert: the
// owned-group lock DeleteGroupMembershipsForUser takes is FOR NO KEY UPDATE
// (the lock the user-row delete's owner_id SET NULL takes), not FOR UPDATE.
// FOR UPDATE also conflicts with the FOR KEY SHARE lock PostgreSQL's FK check
// takes on a group row when a membership referencing it is inserted.
//
// User U owns group G and is a member of group Gx. T2 deletes M(U,Gx),
// holding that row. T1 deletes U: it locks G, then its membership delete
// waits on T2's M(U,Gx). T2 then inserts M(X,G) for another user X, whose FK
// check needs KEY SHARE on G. With FOR UPDATE that waits on T1, which waits
// on T2: deadlock (SQLSTATE 40P01). With FOR NO KEY UPDATE the insert does
// not wait, T2 commits, and T1 finishes.
func TestDeleteGroupMembershipsForUser_OwnedGroupLockAllowsMemberInsert(t *testing.T) {
	skipUnlessPostgres(t)
	ctx, cancel := context.WithTimeout(context.Background(), lockOrderTimeout)
	defer cancel()
	cs := newTestCompositeStore(t)
	db := cs.DB()
	require.NotNil(t, db, "raw database handle needed to watch for lock waits")

	u := newCleanupUser(t, cs, "ogl-owner@example.com")
	x := newCleanupUser(t, cs, "ogl-other@example.com")
	g := uuid.NewString()
	require.NoError(t, cs.CreateGroup(ctx, &store.Group{
		ID: g, Name: "ogl-g", Slug: "ogl-g-" + g[:8], GroupType: store.GroupTypeExplicit, OwnerID: u,
	}))
	gx := newCleanupGroup(t, cs, "ogl-gx-"+uuid.NewString()[:8])
	addCleanupMember(t, cs, gx, store.GroupMemberTypeUser, u, store.GroupMemberRoleMember)

	// T2: delete U's membership in Gx and hold the row lock.
	tx, err := cs.client.Tx(ctx)
	require.NoError(t, err)
	t2Done := false
	defer func() {
		if !t2Done {
			_ = tx.Rollback()
		}
	}()
	t2 := newTxCompositeStore(tx)
	require.NoError(t, t2.RemoveGroupMember(ctx, gx, store.GroupMemberTypeUser, u))

	// T1: the user delete, which must wait on T2's lock on M(U,Gx).
	t1Err := make(chan error, 1)
	go func() {
		t1Err <- cs.WithTx(ctx, func(tx store.Store) error {
			if _, err := tx.DeleteGroupMembershipsForUser(ctx, u); err != nil {
				return err
			}
			return tx.DeleteUser(ctx, u)
		})
	}()

	// Wait until T1 has locked G and is blocked on M(U,Gx).
	require.Eventually(t, func() bool {
		var waiting int
		if err := db.QueryRowContext(ctx,
			`SELECT count(*) FROM pg_stat_activity
			  WHERE datname = current_database() AND wait_event_type = 'Lock'`).Scan(&waiting); err != nil {
			return false
		}
		return waiting > 0
	}, lockOrderTimeout/2, 20*time.Millisecond, "the user delete never waited on U's membership row")

	// T2 inserts a membership of X into U's owned group G, then commits.
	t2Err := t2.AddGroupMember(ctx, &store.GroupMember{
		GroupID: g, MemberType: store.GroupMemberTypeUser, MemberID: x, Role: store.GroupMemberRoleMember,
	})
	if t2Err == nil {
		t2Err = tx.Commit()
	} else {
		_ = tx.Rollback()
	}
	t2Done = true

	var err1 error
	select {
	case err1 = <-t1Err:
	case <-time.After(lockOrderTimeout):
		t.Fatal("the user delete did not finish")
	}
	assert.NoError(t, t2Err, "the membership insert must not fail (40P01 means the owned-group lock blocks the FK check)")
	assert.NoError(t, err1, "the user delete must not fail (40P01 means the owned-group lock blocks the FK check)")
	if t.Failed() {
		return
	}
	_, err = cs.GetUser(ctx, u)
	assert.ErrorIs(t, err, store.ErrNotFound)
	members, err := cs.GetGroupMembers(ctx, g)
	require.NoError(t, err)
	require.Len(t, members, 1)
	assert.Equal(t, x, members[0].MemberID)
	assert.Zero(t, orphanedMembershipCount(t, cs))
}
