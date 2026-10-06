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
	"bytes"
	"context"
	"database/sql"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/ent/membershiplosscheck"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/GoogleCloudPlatform/scion/pkg/store/enttest"
	"github.com/GoogleCloudPlatform/scion/pkg/store/storetest"
)

// compositeTestFactory builds a CompositeStore-backed store.Store on the
// enttest backend (SQLite by default, PostgreSQL under -tags integration).
func compositeTestFactory(t *testing.T) store.Store {
	t.Helper()
	cs := NewCompositeStore(enttest.NewClient(t))
	t.Cleanup(func() { _ = cs.Close() })
	return cs
}

func TestAgentHold_Conformance(t *testing.T) {
	storetest.AgentHoldConformance(t, compositeTestFactory)
}

func TestMembershipLossCheck_Conformance(t *testing.T) {
	storetest.MembershipLossCheckConformance(t, compositeTestFactory)
}

func TestDelegationDescendants_Conformance(t *testing.T) {
	storetest.DelegationDescendantsConformance(t, compositeTestFactory)
}

// seedLockAgents creates a project and n agents and returns the agent IDs in
// ascending order.
func seedLockAgents(t *testing.T, ctx context.Context, cs *CompositeStore, n int) []string {
	t.Helper()
	projectID := uuid.NewString()
	require.NoError(t, cs.CreateProject(ctx, &store.Project{ID: projectID, Name: "lock-" + projectID[:8], Slug: "lock-" + projectID[:8]}))
	uids := make([]uuid.UUID, n)
	for i := range uids {
		uids[i] = uuid.New()
	}
	for i := 1; i < n; i++ {
		for j := i; j > 0 && bytes.Compare(uids[j][:], uids[j-1][:]) < 0; j-- {
			uids[j], uids[j-1] = uids[j-1], uids[j]
		}
	}
	ids := make([]string, n)
	for i, u := range uids {
		ids[i] = u.String()
		ag := makeAgent(projectID, "lock-"+ids[i][:8])
		ag.ID = ids[i]
		require.NoError(t, cs.CreateAgent(ctx, ag))
	}
	return ids
}

func TestLockAgentRows_InTx(t *testing.T) {
	ctx := context.Background()
	cs := NewCompositeStore(enttest.NewClient(t))
	projectID := uuid.NewString()
	require.NoError(t, cs.CreateProject(ctx, &store.Project{ID: projectID, Name: "lock", Slug: "lock-" + projectID[:8]}))
	a := makeAgent(projectID, "lock-a")
	require.NoError(t, cs.CreateAgent(ctx, a))

	err := cs.WithTx(ctx, func(tx store.Store) error {
		// Existing, missing and duplicate IDs are accepted.
		return tx.LockAgentRows(ctx, []string{a.ID, uuid.NewString(), a.ID})
	})
	require.NoError(t, err)
	require.NoError(t, cs.LockAgentRows(ctx, nil))
	assert.ErrorIs(t, cs.LockAgentRows(ctx, []string{"not-a-uuid"}), store.ErrInvalidInput)
}

// waitForLockWaiter waits until some backend is waiting on a row lock.
func waitForLockWaiter(t *testing.T, db *sql.DB) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		var waiting int
		require.NoError(t, db.QueryRow("SELECT count(*) FROM pg_locks WHERE NOT granted").Scan(&waiting))
		if waiting > 0 {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("no transaction started waiting on a lock")
}

// TestLockAgentRows_AscendingOrder_Postgres: tx1 holds a; tx2 asks for
// [b, a] and, locking in ascending order, waits on a without taking b; tx1
// can then lock b. Locking in request order would let tx2 take b first and
// deadlock with tx1.
func TestLockAgentRows_AscendingOrder_Postgres(t *testing.T) {
	if !enttest.Active() {
		t.Skip("requires -tags integration and SCION_TEST_POSTGRES_URL")
	}
	ctx := context.Background()
	client := enttest.NewClient(t)
	cs := NewCompositeStore(client)
	ids := seedLockAgents(t, ctx, cs, 2)
	a, b := ids[0], ids[1]

	tx1, err := client.Tx(ctx)
	require.NoError(t, err)
	defer func() { _ = tx1.Rollback() }()
	s1 := newTxCompositeStore(tx1)
	require.NoError(t, s1.LockAgentRows(ctx, []string{a}))

	tx2, err := client.Tx(ctx)
	require.NoError(t, err)
	defer func() { _ = tx2.Rollback() }()
	s2 := newTxCompositeStore(tx2)
	done := make(chan error, 1)
	go func() { done <- s2.LockAgentRows(ctx, []string{b, a}) }()
	waitForLockWaiter(t, cs.DB())

	lockCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	require.NoError(t, s1.LockAgentRows(lockCtx, []string{b}), "b must be free while tx2 waits on a")
	require.NoError(t, tx1.Commit())

	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(10 * time.Second):
		t.Fatal("tx2 did not acquire its locks after tx1 committed")
	}
	require.NoError(t, tx2.Commit())
}

// TestMembershipLossCheck_ClaimSkipLocked_Postgres: a row locked by another
// transaction is skipped, not waited on, and is claimable once released.
func TestMembershipLossCheck_ClaimSkipLocked_Postgres(t *testing.T) {
	if !enttest.Active() {
		t.Skip("requires -tags integration and SCION_TEST_POSTGRES_URL")
	}
	ctx := context.Background()
	client := enttest.NewClient(t)
	cs := NewCompositeStore(client)

	older := &store.MembershipLossCheck{UserID: uuid.NewString(), Trigger: store.MembershipLossTriggerMemberRemove, CreatedAt: time.Now().Add(-time.Minute)}
	newer := &store.MembershipLossCheck{UserID: uuid.NewString(), Trigger: store.MembershipLossTriggerMemberRemove}
	require.NoError(t, cs.EnqueueMembershipLossCheck(ctx, older))
	require.NoError(t, cs.EnqueueMembershipLossCheck(ctx, newer))

	tx, err := client.Tx(ctx)
	require.NoError(t, err)
	defer func() { _ = tx.Rollback() }()
	_, err = tx.MembershipLossCheck.Query().
		Where(membershiplosscheck.IDEQ(uuid.MustParse(older.ID))).
		ForUpdate().
		Only(ctx)
	require.NoError(t, err)

	claimCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	claimed, err := cs.ClaimMembershipLossChecks(claimCtx, 10, time.Hour)
	require.NoError(t, err, "the claim must not wait on the locked row")
	require.Len(t, claimed, 1)
	assert.Equal(t, newer.ID, claimed[0].ID)

	require.NoError(t, tx.Rollback())
	claimed, err = cs.ClaimMembershipLossChecks(ctx, 10, time.Hour)
	require.NoError(t, err)
	require.Len(t, claimed, 1)
	assert.Equal(t, older.ID, claimed[0].ID)
}
