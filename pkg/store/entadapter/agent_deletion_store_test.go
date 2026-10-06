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
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// seedDeletion puts a delete marker on agent id via UpdateAgentDeletion.
func seedDeletion(t *testing.T, s *AgentStore, id, state string, leaseAt time.Time, code string) {
	t.Helper()
	set := store.DeletionFields{
		State:     strPtr(state),
		BumpClaim: true,
		LeaseAt:   &leaseAt,
		StartedAt: &leaseAt,
	}
	if code != "" {
		set.Code = strPtr(code)
	}
	if state == store.DeletionStateFailed {
		set.FailedAt = &leaseAt
	}
	n, err := s.UpdateAgentDeletion(context.Background(), id, store.DeletionPredicate{}, set)
	require.NoError(t, err)
	require.Equal(t, 1, n)
}

func TestUpdateAgentDeletion_PredicateSemantics(t *testing.T) {
	ctx := context.Background()
	s, projectID := newTestAgentStore(t)
	a := makeAgent(projectID, "del-pred")
	require.NoError(t, s.CreateAgent(ctx, a))

	deleting := store.DeletionStateDeleting
	lease := time.Now().Add(time.Minute)

	// Claim: no marker yet ("" matches NULL), claim 0 → bump to 1.
	zero := int64(0)
	n, err := s.UpdateAgentDeletion(ctx, a.ID, store.DeletionPredicate{
		Claim:         &zero,
		States:        []string{store.DeletionStateNone, store.DeletionStateFailed},
		DeletedAtNull: true,
	}, store.DeletionFields{State: &deleting, BumpClaim: true, LeaseAt: &lease, Prior: strPtr(`{"phase":"running"}`)})
	require.NoError(t, err)
	assert.Equal(t, 1, n)

	got, err := s.GetAgent(ctx, a.ID)
	require.NoError(t, err)
	assert.Equal(t, deleting, got.DeletionState)
	assert.Equal(t, int64(1), got.DeletionClaim)
	require.NotNil(t, got.DeletionLeaseAt)
	assert.WithinDuration(t, lease, *got.DeletionLeaseAt, time.Millisecond)
	assert.Equal(t, `{"phase":"running"}`, got.DeletionPrior)

	// The same claim again loses: the claim epoch moved on.
	n, err = s.UpdateAgentDeletion(ctx, a.ID, store.DeletionPredicate{Claim: &zero}, store.DeletionFields{State: &deleting})
	require.NoError(t, err)
	assert.Equal(t, 0, n, "a stale claim must affect 0 rows")

	// State mismatch → 0.
	n, err = s.UpdateAgentDeletion(ctx, a.ID, store.DeletionPredicate{States: []string{store.DeletionStateFinalizing}}, store.DeletionFields{Code: strPtr("x")})
	require.NoError(t, err)
	assert.Equal(t, 0, n)

	// LeaseExpiredBefore: lease is in the future → 0; past cutoff → 1.
	now := time.Now()
	n, err = s.UpdateAgentDeletion(ctx, a.ID, store.DeletionPredicate{LeaseExpiredBefore: &now}, store.DeletionFields{Code: strPtr("x")})
	require.NoError(t, err)
	assert.Equal(t, 0, n, "a live lease is not expired")
	later := lease.Add(time.Second)
	one := int64(1)
	n, err = s.UpdateAgentDeletion(ctx, a.ID, store.DeletionPredicate{Claim: &one, LeaseExpiredBefore: &later},
		store.DeletionFields{State: strPtr(store.DeletionStateFailed), Code: strPtr(store.DeletionCodeAbandoned), FailedAt: &now})
	require.NoError(t, err)
	assert.Equal(t, 1, n)

	// DeletedAtNull: a soft-deleted row does not match.
	soft := makeAgent(projectID, "del-soft")
	require.NoError(t, s.CreateAgent(ctx, soft))
	require.NoError(t, s.client.Agent.UpdateOneID(uuid.MustParse(soft.ID)).SetDeletedAt(time.Now()).Exec(ctx))
	n, err = s.UpdateAgentDeletion(ctx, soft.ID, store.DeletionPredicate{DeletedAtNull: true}, store.DeletionFields{State: &deleting})
	require.NoError(t, err)
	assert.Equal(t, 0, n)

	// Not found → (0, nil).
	n, err = s.UpdateAgentDeletion(ctx, uuid.NewString(), store.DeletionPredicate{}, store.DeletionFields{State: &deleting})
	require.NoError(t, err)
	assert.Equal(t, 0, n)
}

func TestUpdateAgentDeletion_ClearFields(t *testing.T) {
	ctx := context.Background()
	s, projectID := newTestAgentStore(t)
	a := makeAgent(projectID, "del-clear")
	require.NoError(t, s.CreateAgent(ctx, a))
	seedDeletion(t, s, a.ID, store.DeletionStateFailed, time.Now(), store.DeletionCodeRuntimeError)
	n, err := s.UpdateAgentDeletion(ctx, a.ID, store.DeletionPredicate{}, store.DeletionFields{
		Error: strPtr("boom"), Prior: strPtr(`{"phase":"running"}`), Request: strPtr(`{"soft":true}`),
	})
	require.NoError(t, err)
	require.Equal(t, 1, n)
	seeded, err := s.GetAgent(ctx, a.ID)
	require.NoError(t, err)
	require.Equal(t, "boom", seeded.DeletionError)
	require.NotNil(t, seeded.DeletionStartedAt)

	n, err = s.UpdateAgentDeletion(ctx, a.ID, store.DeletionPredicate{States: []string{store.DeletionStateFailed}}, store.DeletionFields{
		State: strPtr(store.DeletionStateNone), Code: strPtr(""),
		Error: strPtr(""), Prior: strPtr(""), Request: strPtr(""),
		ClearLeaseAt: true, ClearStartedAt: true, ClearFailedAt: true,
	})
	require.NoError(t, err)
	require.Equal(t, 1, n)
	got, err := s.GetAgent(ctx, a.ID)
	require.NoError(t, err)
	assert.Equal(t, "", got.DeletionState)
	assert.Equal(t, "", got.DeletionCode)
	assert.Equal(t, "", got.DeletionError)
	assert.Equal(t, "", got.DeletionPrior)
	assert.Equal(t, "", got.DeletionRequest)
	assert.Nil(t, got.DeletionLeaseAt)
	assert.Nil(t, got.DeletionStartedAt)
	assert.Nil(t, got.DeletionFailedAt)
	assert.Equal(t, int64(1), got.DeletionClaim, "the claim epoch is kept")
}

func TestUpdateAgentDeletion_BumpsStateVersion_StaleUpdateAgentConflicts(t *testing.T) {
	ctx := context.Background()
	s, projectID := newTestAgentStore(t)
	a := makeAgent(projectID, "del-version")
	require.NoError(t, s.CreateAgent(ctx, a))

	stale, err := s.GetAgent(ctx, a.ID)
	require.NoError(t, err)

	phase := "stopping"
	n, err := s.UpdateAgentDeletion(ctx, a.ID, store.DeletionPredicate{}, store.DeletionFields{
		State: strPtr(store.DeletionStateDeleting), BumpClaim: true, Phase: &phase, Activity: strPtr(""),
	})
	require.NoError(t, err)
	require.Equal(t, 1, n)

	got, err := s.GetAgent(ctx, a.ID)
	require.NoError(t, err)
	assert.Equal(t, stale.StateVersion+1, got.StateVersion, "UpdateAgentDeletion bumps state_version by one")
	assert.Equal(t, "stopping", got.Phase)
	assert.Equal(t, "", got.Activity)

	stale.Message = "stale write"
	err = s.UpdateAgent(ctx, stale)
	assert.ErrorIs(t, err, store.ErrVersionConflict, "a writer holding a pre-claim read must conflict")

	// UpdateAgent never writes the deletion columns.
	got.Message = "fresh write"
	require.NoError(t, s.UpdateAgent(ctx, got))
	again, err := s.GetAgent(ctx, a.ID)
	require.NoError(t, err)
	assert.Equal(t, store.DeletionStateDeleting, again.DeletionState)
}

func TestUpdateAgentDeletion_DeriveSeesInTxRow(t *testing.T) {
	ctx := context.Background()
	s, projectID := newTestAgentStore(t)
	a := makeAgent(projectID, "del-derive")
	a.Phase = "running"
	require.NoError(t, s.CreateAgent(ctx, a))

	n, err := s.UpdateAgentDeletion(ctx, a.ID, store.DeletionPredicate{}, store.DeletionFields{
		State: strPtr(store.DeletionStateDeleting),
		Derive: func(cur *store.Agent, f *store.DeletionFields) {
			f.Prior = strPtr(`{"phase":"` + cur.Phase + `"}`)
		},
	})
	require.NoError(t, err)
	require.Equal(t, 1, n)
	got, err := s.GetAgent(ctx, a.ID)
	require.NoError(t, err)
	assert.Equal(t, `{"phase":"running"}`, got.DeletionPrior)
}

// A concurrent write between the read and the CAS (no row lock on SQLite)
// makes the CAS miss; the attempt is rolled back and retried from a fresh
// read. The rival write is simulated through the attempt's own tx — an
// out-of-tx write would block on SQLite's single connection — so it is
// rolled back with the attempt, and the retry then succeeds.
func TestUpdateAgentDeletion_CASMissRetries(t *testing.T) {
	ctx := context.Background()
	s, projectID := newTestAgentStore(t)
	a := makeAgent(projectID, "del-race")
	require.NoError(t, s.CreateAgent(ctx, a))
	uid := uuid.MustParse(a.ID)
	before, err := s.GetAgent(ctx, a.ID)
	require.NoError(t, err)

	calls := 0
	updateAgentDeletionHook = func(ctx context.Context, tx *ent.Tx, id string) {
		calls++
		if calls == 1 {
			require.NoError(t, tx.Agent.UpdateOneID(uid).AddStateVersion(1).Exec(ctx))
		}
	}
	t.Cleanup(func() { updateAgentDeletionHook = nil })

	zero := int64(0)
	n, err := s.UpdateAgentDeletion(ctx, a.ID, store.DeletionPredicate{Claim: &zero}, store.DeletionFields{
		State: strPtr(store.DeletionStateDeleting), BumpClaim: true,
	})
	require.NoError(t, err)
	assert.Equal(t, 1, n)
	assert.Equal(t, 2, calls, "the CAS miss is retried once")
	got, err := s.GetAgent(ctx, a.ID)
	require.NoError(t, err)
	assert.Equal(t, int64(1), got.DeletionClaim)
	assert.Equal(t, before.StateVersion+1, got.StateVersion)
}

// A CAS that misses on every attempt surfaces ErrVersionConflict.
func TestUpdateAgentDeletion_ExhaustedRetriesConflict(t *testing.T) {
	ctx := context.Background()
	s, projectID := newTestAgentStore(t)
	a := makeAgent(projectID, "del-exhaust")
	require.NoError(t, s.CreateAgent(ctx, a))
	uid := uuid.MustParse(a.ID)

	updateAgentDeletionHook = func(ctx context.Context, tx *ent.Tx, id string) {
		require.NoError(t, tx.Agent.UpdateOneID(uid).AddStateVersion(1).Exec(ctx))
	}
	t.Cleanup(func() { updateAgentDeletionHook = nil })

	n, err := s.UpdateAgentDeletion(ctx, a.ID, store.DeletionPredicate{}, store.DeletionFields{State: strPtr(store.DeletionStateDeleting)})
	assert.ErrorIs(t, err, store.ErrVersionConflict)
	assert.Equal(t, 0, n)
}

func TestBrokerDispatch_DeleteIntentQueries(t *testing.T) {
	ctx := context.Background()
	s, projectID := newTestAgentStore(t)
	ds := NewBrokerDispatchStore(s.client)
	a := makeAgent(projectID, "del-dispatch")
	require.NoError(t, s.CreateAgent(ctx, a))

	has, err := ds.HasOutstandingBrokerDispatch(ctx, a.ID, "delete")
	require.NoError(t, err)
	assert.False(t, has)

	before := time.Now().Add(-time.Second)
	d := newDispatch(uuid.NewString(), "delete")
	d.AgentID = a.ID
	require.NoError(t, ds.InsertBrokerDispatch(ctx, d))

	// Pending counts; a different op does not.
	has, err = ds.HasOutstandingBrokerDispatch(ctx, a.ID, "delete")
	require.NoError(t, err)
	assert.True(t, has, "pending delete intent is outstanding")
	has, err = ds.HasOutstandingBrokerDispatch(ctx, a.ID, "start")
	require.NoError(t, err)
	assert.False(t, has)

	// in_progress counts.
	claimed, err := ds.ClaimBrokerDispatch(ctx, d.ID, "hub-1")
	require.NoError(t, err)
	require.True(t, claimed)
	has, err = ds.HasOutstandingBrokerDispatch(ctx, a.ID, "delete")
	require.NoError(t, err)
	assert.True(t, has, "in_progress delete intent is outstanding")

	done, err := ds.HasCompletedBrokerDispatchSince(ctx, a.ID, "delete", before)
	require.NoError(t, err)
	assert.False(t, done)

	// done: no longer outstanding; completed since `before`, not since later.
	require.NoError(t, ds.CompleteBrokerDispatch(ctx, d.ID, ""))
	has, err = ds.HasOutstandingBrokerDispatch(ctx, a.ID, "delete")
	require.NoError(t, err)
	assert.False(t, has)
	done, err = ds.HasCompletedBrokerDispatchSince(ctx, a.ID, "delete", before)
	require.NoError(t, err)
	assert.True(t, done)
	done, err = ds.HasCompletedBrokerDispatchSince(ctx, a.ID, "delete", time.Now().Add(time.Hour))
	require.NoError(t, err)
	assert.False(t, done)

	// failed is neither outstanding nor completed.
	d2 := newDispatch(uuid.NewString(), "delete")
	other := makeAgent(projectID, "del-dispatch-2")
	require.NoError(t, s.CreateAgent(ctx, other))
	d2.AgentID = other.ID
	require.NoError(t, ds.InsertBrokerDispatch(ctx, d2))
	_, err = ds.ClaimBrokerDispatch(ctx, d2.ID, "hub-1")
	require.NoError(t, err)
	require.NoError(t, ds.FailBrokerDispatch(ctx, d2.ID, "boom", ""))
	has, err = ds.HasOutstandingBrokerDispatch(ctx, other.ID, "delete")
	require.NoError(t, err)
	assert.False(t, has)
	done, err = ds.HasCompletedBrokerDispatchSince(ctx, other.ID, "delete", before)
	require.NoError(t, err)
	assert.False(t, done)
}

// Acceptance (dd): the missing-container reconcile leaves a row the delete
// engine owns alone, but still applies to a failed one.
func TestAgentStore_MarkAgentContainerMissing_DeletionGuard(t *testing.T) {
	ctx := context.Background()
	s, projectID := newTestAgentStore(t)
	old := time.Now().Add(-time.Hour)
	cutoff := time.Now().Add(-5 * time.Minute)

	create := func(slug string) *store.Agent {
		a := makeAgent(projectID, slug)
		a.RuntimeBrokerID = "broker-1"
		a.LastSeen = old
		a.ContainerStatus = "Running"
		require.NoError(t, s.CreateAgent(ctx, a))
		return a
	}

	// The clause is state-based, so a lease-expired deleting/finalizing row
	// is left alone too (only retry, force or the engine moves it on).
	for _, tc := range []struct {
		state   string
		leaseIn time.Duration
	}{
		{store.DeletionStateDeleting, time.Minute},
		{store.DeletionStateFinalizing, time.Minute},
		{store.DeletionStateDeleting, -time.Minute},
		{store.DeletionStateFinalizing, -time.Minute},
	} {
		st := tc.state
		label := st + "-live"
		if tc.leaseIn < 0 {
			label = st + "-expired"
		}
		t.Run(label+" is left alone", func(t *testing.T) {
			a := create("dd-" + label)
			seedDeletion(t, s, a.ID, st, time.Now().Add(tc.leaseIn), "")
			got, err := s.MarkAgentContainerMissing(ctx, a.ID, "broker-1", cutoff, "gone")
			require.NoError(t, err)
			assert.Nil(t, got, "0 rows affected")
			stored, err := s.GetAgent(ctx, a.ID)
			require.NoError(t, err)
			assert.Equal(t, "running", stored.Phase)
		})
	}

	t.Run("failed still applies", func(t *testing.T) {
		a := create("dd-failed")
		seedDeletion(t, s, a.ID, store.DeletionStateFailed, time.Now().Add(-time.Minute), store.DeletionCodeRuntimeError)
		got, err := s.MarkAgentContainerMissing(ctx, a.ID, "broker-1", cutoff, "gone")
		require.NoError(t, err)
		require.NotNil(t, got)
		assert.Equal(t, "error", got.Phase)
		assert.Equal(t, "container_missing", got.ExitReason)
	})
}

// Acceptance (c), store half: UpdateAgentStatus re-checks the delete marker
// inside its transaction, so a report that raced a delete claim changes no
// status field.
func TestAgentStore_UpdateAgentStatus_DeletionGuard(t *testing.T) {
	ctx := context.Background()
	s, projectID := newTestAgentStore(t)

	report := store.AgentStatusUpdate{
		Phase: "error", Activity: "crashed", Message: "boom",
		ExitReason: "crashed", ContainerStatus: "Exited (1)",
	}
	code := 1
	report.ExitCode = &code

	t.Run("live deleting row", func(t *testing.T) {
		a := makeAgent(projectID, "c-deleting")
		require.NoError(t, s.CreateAgent(ctx, a))
		seedDeletion(t, s, a.ID, store.DeletionStateDeleting, time.Now().Add(time.Minute), "")
		require.NoError(t, s.UpdateAgentStatus(ctx, a.ID, report))
		got, err := s.GetAgent(ctx, a.ID)
		require.NoError(t, err)
		assert.Equal(t, "running", got.Phase)
		assert.Equal(t, "thinking", got.Activity)
		assert.Equal(t, "", got.Message)
		assert.Equal(t, "", got.ExitReason)
		assert.Nil(t, got.ExitCode)
	})

	t.Run("soft-deleted row", func(t *testing.T) {
		a := makeAgent(projectID, "c-soft")
		require.NoError(t, s.CreateAgent(ctx, a))
		require.NoError(t, s.client.Agent.UpdateOneID(uuid.MustParse(a.ID)).SetDeletedAt(time.Now()).Exec(ctx))
		require.NoError(t, s.UpdateAgentStatus(ctx, a.ID, report))
		got, err := s.GetAgent(ctx, a.ID)
		require.NoError(t, err)
		assert.Equal(t, "running", got.Phase)
		assert.Equal(t, "", got.ExitReason)
	})

	t.Run("lease-expired deleting row applies", func(t *testing.T) {
		a := makeAgent(projectID, "c-expired")
		require.NoError(t, s.CreateAgent(ctx, a))
		seedDeletion(t, s, a.ID, store.DeletionStateDeleting, time.Now().Add(-time.Minute), "")
		require.NoError(t, s.UpdateAgentStatus(ctx, a.ID, report))
		got, err := s.GetAgent(ctx, a.ID)
		require.NoError(t, err)
		assert.Equal(t, "error", got.Phase)
	})

	// A finalizing row whose lease expired reads as failed for a status
	// report, but holds the row against a start (DeletionHoldsRow): a
	// start's own write (StartWrite) keeps the delete's phase, while a
	// report still applies.
	t.Run("lease-expired finalizing row", func(t *testing.T) {
		start := store.AgentStatusUpdate{Phase: "running", ClearExit: true, ClearTerminalRemnants: true, StartWrite: true}
		for _, tc := range []struct {
			name  string
			upd   store.AgentStatusUpdate
			phase string
		}{
			{"start-held", start, "stopped"},
			{"report-applies", report, "error"},
		} {
			t.Run(tc.name, func(t *testing.T) {
				a := makeAgent(projectID, "c-fin-"+tc.name)
				a.Phase = "stopped"
				require.NoError(t, s.CreateAgent(ctx, a))
				seedDeletion(t, s, a.ID, store.DeletionStateFinalizing, time.Now().Add(-time.Minute), "")
				require.NoError(t, s.UpdateAgentStatus(ctx, a.ID, tc.upd))
				got, err := s.GetAgent(ctx, a.ID)
				require.NoError(t, err)
				assert.Equal(t, tc.phase, got.Phase)
			})
		}
	})

	t.Run("start write on a live row applies", func(t *testing.T) {
		a := makeAgent(projectID, "c-start-live")
		a.Phase = "stopped"
		require.NoError(t, s.CreateAgent(ctx, a))
		require.NoError(t, s.UpdateAgentStatus(ctx, a.ID, store.AgentStatusUpdate{Phase: "running", StartWrite: true}))
		got, err := s.GetAgent(ctx, a.ID)
		require.NoError(t, err)
		assert.Equal(t, "running", got.Phase)
	})
}

// Review round 2 (A): every DeletionFields field written through
// UpdateAgentDeletion reads back through GetAgent (entAgentToStore), and
// every field can be cleared again.
func TestUpdateAgentDeletion_RoundTripsEveryColumn(t *testing.T) {
	ctx := context.Background()
	s, projectID := newTestAgentStore(t)
	a := makeAgent(projectID, "del-roundtrip")
	require.NoError(t, s.CreateAgent(ctx, a))

	base := time.Now().UTC().Truncate(time.Millisecond)
	lease, started, failed := base.Add(time.Minute), base.Add(-time.Minute), base.Add(-time.Second)
	claim := int64(7)
	n, err := s.UpdateAgentDeletion(ctx, a.ID, store.DeletionPredicate{}, store.DeletionFields{
		State: strPtr(store.DeletionStateFailed), Claim: &claim,
		LeaseAt: &lease, StartedAt: &started, FailedAt: &failed,
		Code: strPtr(store.DeletionCodeConflict), Error: strPtr("broker busy"),
		Prior: strPtr(`{"phase":"running"}`), Request: strPtr(`{"soft":true}`),
		Phase: strPtr("stopping"), Activity: strPtr("idle"),
	})
	require.NoError(t, err)
	require.Equal(t, 1, n)

	got, err := s.GetAgent(ctx, a.ID)
	require.NoError(t, err)
	assert.Equal(t, store.DeletionStateFailed, got.DeletionState)
	assert.Equal(t, int64(7), got.DeletionClaim)
	require.NotNil(t, got.DeletionLeaseAt)
	require.NotNil(t, got.DeletionStartedAt)
	require.NotNil(t, got.DeletionFailedAt)
	assert.WithinDuration(t, lease, *got.DeletionLeaseAt, time.Millisecond)
	assert.WithinDuration(t, started, *got.DeletionStartedAt, time.Millisecond)
	assert.WithinDuration(t, failed, *got.DeletionFailedAt, time.Millisecond)
	assert.Equal(t, store.DeletionCodeConflict, got.DeletionCode)
	assert.Equal(t, "broker busy", got.DeletionError)
	assert.Equal(t, `{"phase":"running"}`, got.DeletionPrior)
	assert.Equal(t, `{"soft":true}`, got.DeletionRequest)
	assert.Equal(t, "stopping", got.Phase)
	assert.Equal(t, "idle", got.Activity)

	n, err = s.UpdateAgentDeletion(ctx, a.ID, store.DeletionPredicate{}, store.DeletionFields{
		State: strPtr(store.DeletionStateNone),
		Code:  strPtr(""), Error: strPtr(""), Prior: strPtr(""), Request: strPtr(""),
		ClearLeaseAt: true, ClearStartedAt: true, ClearFailedAt: true,
	})
	require.NoError(t, err)
	require.Equal(t, 1, n)
	got, err = s.GetAgent(ctx, a.ID)
	require.NoError(t, err)
	assert.Equal(t, store.DeletionStateNone, got.DeletionState)
	assert.Equal(t, int64(7), got.DeletionClaim, "the claim epoch is kept")
	assert.Nil(t, got.DeletionLeaseAt)
	assert.Nil(t, got.DeletionStartedAt)
	assert.Nil(t, got.DeletionFailedAt)
	assert.Empty(t, got.DeletionCode)
	assert.Empty(t, got.DeletionError)
	assert.Empty(t, got.DeletionPrior)
	assert.Empty(t, got.DeletionRequest)
}
