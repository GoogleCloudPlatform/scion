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
	"errors"
	"fmt"
	"sort"
	"strings"
	"testing"
	"time"

	entsql "entgo.io/ent/dialect/sql"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/ent"
	"github.com/GoogleCloudPlatform/scion/pkg/ent/agenthold"
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
		require.NoError(t, tx.LockAgentRows(ctx, []string{a.ID, uuid.NewString(), a.ID}))
		require.NoError(t, tx.LockAgentRows(ctx, nil))
		assert.ErrorIs(t, tx.LockAgentRows(ctx, []string{"not-a-uuid"}), store.ErrInvalidInput)
		return nil
	})
	require.NoError(t, err)
}

func TestLockAgentRows_RequiresTx(t *testing.T) {
	ctx := context.Background()
	cs := NewCompositeStore(enttest.NewClient(t))
	projectID := uuid.NewString()
	require.NoError(t, cs.CreateProject(ctx, &store.Project{ID: projectID, Name: "lock", Slug: "lock-" + projectID[:8]}))
	a := makeAgent(projectID, "lock-a")
	require.NoError(t, cs.CreateAgent(ctx, a))

	// Outside a transaction no lock would outlive the statement, so the
	// call is refused on every backend.
	assert.ErrorIs(t, cs.LockAgentRows(ctx, []string{a.ID}), store.ErrInvalidInput)
	assert.ErrorIs(t, cs.LockAgentRows(ctx, nil), store.ErrInvalidInput)
}

// TestAgentHold_ClearReasonIsCapped: the stored clear reason is cut to a
// valid UTF-8 prefix of at most 2000 bytes.
func TestAgentHold_ClearReasonIsCapped(t *testing.T) {
	ctx := context.Background()
	client := enttest.NewClient(t)
	cs := NewCompositeStore(client)
	projectID := uuid.NewString()
	require.NoError(t, cs.CreateProject(ctx, &store.Project{ID: projectID, Name: "hold", Slug: "hold-" + projectID[:8]}))
	a := makeAgent(projectID, "hold-a")
	require.NoError(t, cs.CreateAgent(ctx, a))
	_, err := cs.CreateAgentHolds(ctx, []*store.AgentHold{{
		AgentID:           a.ID,
		ProjectID:         projectID,
		Cause:             store.AgentHoldCauseOwnerAccessEnded,
		RootPrincipalType: store.AgentHoldRootUser,
		RootPrincipalID:   uuid.NewString(),
		Trigger:           store.MembershipLossTriggerMemberRemove,
	}})
	require.NoError(t, err)

	reason := strings.Repeat("é", 3000)
	actor := uuid.New()
	n, err := cs.ClearAgentHolds(ctx, a.ID, store.ClearActor{Kind: store.ClearActorUser, ID: "{" + strings.ToUpper(actor.String()) + "}"}, reason)
	require.NoError(t, err)
	require.Equal(t, 1, n)

	h, err := client.AgentHold.Query().Where(agenthold.AgentIDEQ(uuid.MustParse(a.ID))).Only(ctx)
	require.NoError(t, err)
	assert.NotEmpty(t, h.ClearReason)
	assert.LessOrEqual(t, len(h.ClearReason), 2000)
	assert.True(t, strings.HasPrefix(reason, h.ClearReason))
	assert.Equal(t, actor.String(), h.ClearedByID)
}

// TestAgentHold_CreateIsAtomic: when a later batch of a call fails at insert,
// the rows of its earlier batches are not kept, the call returns 0, and the
// holds it was given are left unchanged.
func TestAgentHold_CreateIsAtomic(t *testing.T) {
	ctx := context.Background()
	client := enttest.NewClient(t)
	cs := NewCompositeStore(client)
	t.Cleanup(func() { _ = cs.Close() })
	// One hold per batch, so the calls below run two batches.
	cs.insertBatch = 1

	projectID := uuid.NewString()
	require.NoError(t, cs.CreateProject(ctx, &store.Project{ID: projectID, Name: "hold", Slug: "hold-" + projectID[:8]}))
	a := makeAgent(projectID, "hold-a")
	require.NoError(t, cs.CreateAgent(ctx, a))
	b := makeAgent(projectID, "hold-b")
	require.NoError(t, cs.CreateAgent(ctx, b))

	// Holds are inserted in ascending agent ID order, so the agent with
	// the higher ID is in the second batch. Its insert fails.
	first, second := a, b
	if aID, bID := uuid.MustParse(a.ID), uuid.MustParse(b.ID); bytes.Compare(aID[:], bID[:]) > 0 {
		first, second = b, a
	}
	failAgent := uuid.MustParse(second.ID)
	client.AgentHold.Use(func(next ent.Mutator) ent.Mutator {
		return ent.MutateFunc(func(ctx context.Context, m ent.Mutation) (ent.Value, error) {
			if hm, ok := m.(*ent.AgentHoldMutation); ok {
				if id, ok := hm.AgentID(); ok && id == failAgent {
					return nil, errors.New("insert refused by test")
				}
			}
			return next.Mutate(ctx, m)
		})
	})

	hold := func(agentID string) *store.AgentHold {
		return &store.AgentHold{
			AgentID:           agentID,
			ProjectID:         projectID,
			Cause:             store.AgentHoldCauseOwnerAccessEnded,
			RootPrincipalType: store.AgentHoldRootUser,
			RootPrincipalID:   uuid.NewString(),
			Trigger:           store.MembershipLossTriggerMemberRemove,
		}
	}
	assertUnchanged := func(holds []*store.AgentHold) {
		t.Helper()
		for _, h := range holds {
			assert.Empty(t, h.ID, "a call that returns an error leaves its holds unchanged")
			assert.True(t, h.CreatedAt.IsZero(), "a call that returns an error leaves its holds unchanged")
		}
	}

	holds := []*store.AgentHold{hold(second.ID), hold(first.ID)}
	n, err := cs.CreateAgentHolds(ctx, holds)
	require.Error(t, err)
	assert.Equal(t, 0, n)
	assertUnchanged(holds)
	count, err := client.AgentHold.Query().Count(ctx)
	require.NoError(t, err)
	assert.Equal(t, 0, count, "no row of the first batch is kept")

	// Inside WithTx the call runs in the ambient transaction: its first
	// batch is inserted there, and rolling the transaction back removes it.
	holds = []*store.AgentHold{hold(second.ID), hold(first.ID)}
	err = cs.WithTx(ctx, func(tx store.Store) error {
		txStore := tx.(*CompositeStore)
		txStore.insertBatch = 1
		n, err := tx.CreateAgentHolds(ctx, holds)
		assert.Equal(t, 0, n)
		inTx, cerr := txStore.client.AgentHold.Query().Count(ctx)
		require.NoError(t, cerr)
		assert.Equal(t, 1, inTx, "the first batch is inserted in the ambient transaction")
		return err
	})
	require.Error(t, err)
	assertUnchanged(holds)
	count, err = client.AgentHold.Query().Count(ctx)
	require.NoError(t, err)
	assert.Equal(t, 0, count, "the rollback leaves no rows")

	// Without the failing hold the same call inserts.
	n, err = cs.CreateAgentHolds(ctx, []*store.AgentHold{hold(first.ID)})
	require.NoError(t, err)
	assert.Equal(t, 1, n)
}

// TestAgentHold_CreateInsertsInAgentIDOrder: holds passed in descending agent
// ID order are inserted in ascending agent ID order, all of them are
// inserted, and each hold gets the ID of its own row.
func TestAgentHold_CreateInsertsInAgentIDOrder(t *testing.T) {
	ctx := context.Background()
	client := enttest.NewClient(t)
	cs := NewCompositeStore(client)
	t.Cleanup(func() { _ = cs.Close() })
	cs.insertBatch = 2

	projectID := uuid.NewString()
	require.NoError(t, cs.CreateProject(ctx, &store.Project{ID: projectID, Name: "hold", Slug: "hold-" + projectID[:8]}))
	var agentIDs []uuid.UUID
	for i := 0; i < 5; i++ {
		a := makeAgent(projectID, fmt.Sprintf("hold-%d", i))
		require.NoError(t, cs.CreateAgent(ctx, a))
		agentIDs = append(agentIDs, uuid.MustParse(a.ID))
	}
	sort.Slice(agentIDs, func(i, j int) bool { return bytes.Compare(agentIDs[i][:], agentIDs[j][:]) > 0 })

	var order []uuid.UUID
	client.AgentHold.Use(func(next ent.Mutator) ent.Mutator {
		return ent.MutateFunc(func(ctx context.Context, m ent.Mutation) (ent.Value, error) {
			if hm, ok := m.(*ent.AgentHoldMutation); ok && hm.Op().Is(ent.OpCreate) {
				if id, ok := hm.AgentID(); ok {
					order = append(order, id)
				}
			}
			return next.Mutate(ctx, m)
		})
	})

	holds := make([]*store.AgentHold, len(agentIDs))
	for i, id := range agentIDs {
		holds[i] = &store.AgentHold{
			AgentID:           id.String(),
			ProjectID:         projectID,
			Cause:             store.AgentHoldCauseOwnerAccessEnded,
			RootPrincipalType: store.AgentHoldRootUser,
			RootPrincipalID:   uuid.NewString(),
			Trigger:           store.MembershipLossTriggerMemberRemove,
		}
	}
	n, err := cs.CreateAgentHolds(ctx, holds)
	require.NoError(t, err)
	assert.Equal(t, len(holds), n)

	require.Len(t, order, len(agentIDs))
	assert.True(t, sort.SliceIsSorted(order, func(i, j int) bool { return bytes.Compare(order[i][:], order[j][:]) < 0 }),
		"holds are inserted in ascending agent ID order")

	for i, h := range holds {
		assert.Equal(t, agentIDs[i].String(), h.AgentID, "the input order is kept")
		require.NotEmpty(t, h.ID)
		row, err := client.AgentHold.Get(ctx, uuid.MustParse(h.ID))
		require.NoError(t, err)
		assert.Equal(t, agentIDs[i], row.AgentID, "each hold gets the ID of its own row")
		assert.Equal(t, h.RootPrincipalID, row.RootPrincipalID)
	}
}

// lockTestBackendPID returns the PostgreSQL backend PID of txStore's
// transaction.
func lockTestBackendPID(t *testing.T, ctx context.Context, txStore *CompositeStore) int {
	t.Helper()
	rows := &entsql.Rows{}
	require.NoError(t, txStore.client.Driver().Query(ctx, "SELECT pg_backend_pid()", []any{}, rows))
	defer func() { _ = rows.Close() }()
	require.True(t, rows.Next(), "no backend PID returned")
	var pid int
	require.NoError(t, rows.Scan(&pid))
	require.NoError(t, rows.Err())
	return pid
}

// waitForBackendBlocked waits until the backend pid is blocked on a lock.
func waitForBackendBlocked(t *testing.T, db *sql.DB, pid int) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		var blockers int
		require.NoError(t, db.QueryRow("SELECT cardinality(pg_blocking_pids($1))", pid).Scan(&blockers))
		if blockers > 0 {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("backend %d did not start waiting on a lock", pid)
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
	pid2 := lockTestBackendPID(t, ctx, s2)
	done := make(chan error, 1)
	go func() { done <- s2.LockAgentRows(ctx, []string{b, a}) }()
	waitForBackendBlocked(t, cs.DB(), pid2)

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
