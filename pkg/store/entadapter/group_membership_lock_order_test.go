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

	"github.com/GoogleCloudPlatform/scion/pkg/ent/agent"
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
