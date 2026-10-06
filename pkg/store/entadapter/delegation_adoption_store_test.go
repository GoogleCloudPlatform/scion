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
	"encoding/json"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	entgo "entgo.io/ent"
	"github.com/GoogleCloudPlatform/scion/pkg/delegationadoption"
	"github.com/GoogleCloudPlatform/scion/pkg/ent"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// These tests use enttest.NewClient through newAdoptionWorld, so they run on
// SQLite by default and on Postgres under -tags integration with
// SCION_TEST_POSTGRES_URL set.

func adoptionRecord(cohort string, status store.DelegationAdoptionStatus) *store.DelegationAdoption {
	rec := &store.DelegationAdoption{
		CohortID:   cohort,
		Origin:     store.DelegationAdoptionOriginAdmin,
		DelegateID: uuid.NewString(),
		Status:     status,
	}
	switch status {
	case store.DelegationAdoptionPending:
		rec.OriginalEdgeID = uuid.NewString()
	case store.DelegationAdoptionAdopted:
		rec.OriginalEdgeID = uuid.NewString()
		rec.AdoptedEdgeID = uuid.NewString()
	case store.DelegationAdoptionRecognized, store.DelegationAdoptionRecognizedAbovePolicy:
		rec.AdoptedEdgeID = uuid.NewString()
	case store.DelegationAdoptionExcluded:
		rec.Reason = "role_none"
	}
	return rec
}

func TestDelegationAdoptionStoreRejectsInvalidRecords(t *testing.T) {
	w := newAdoptionWorld(t)
	now := time.Now()
	cases := map[string]func(r *store.DelegationAdoption){
		"missing cohort":          func(r *store.DelegationAdoption) { r.CohortID = "" },
		"missing delegate":        func(r *store.DelegationAdoption) { r.DelegateID = "" },
		"unknown origin":          func(r *store.DelegationAdoption) { r.Origin = "elsewhere" },
		"unknown status":          func(r *store.DelegationAdoption) { r.Status = "done" },
		"empty status":            func(r *store.DelegationAdoption) { r.Status = "" },
		"negative depth":          func(r *store.DelegationAdoption) { r.Depth = -1 },
		"negative policy version": func(r *store.DelegationAdoption) { r.PolicyVersion = -1 },
		"adopted without original": func(r *store.DelegationAdoption) {
			r.Status = store.DelegationAdoptionAdopted
			r.OriginalEdgeID = ""
			r.AdoptedEdgeID = uuid.NewString()
		},
		"adopted without adopted": func(r *store.DelegationAdoption) { r.Status = store.DelegationAdoptionAdopted },
		"adopted onto itself": func(r *store.DelegationAdoption) {
			r.Status = store.DelegationAdoptionAdopted
			r.AdoptedEdgeID = r.OriginalEdgeID
		},
		"pending with adopted edge":  func(r *store.DelegationAdoption) { r.AdoptedEdgeID = uuid.NewString() },
		"pending without original":   func(r *store.DelegationAdoption) { r.OriginalEdgeID = "" },
		"recognized without adopted": func(r *store.DelegationAdoption) { r.Status = store.DelegationAdoptionRecognized },
		"excluded without reason":    func(r *store.DelegationAdoption) { r.Status = store.DelegationAdoptionExcluded },
		"skipped with adopted edge": func(r *store.DelegationAdoption) {
			r.Status = store.DelegationAdoptionSkippedChanged
			r.AdoptedEdgeID = uuid.NewString()
		},
		"revert fields while pending": func(r *store.DelegationAdoption) { r.RevertedByID = "u"; r.RevertedByKind = "user" },
		"reverted without reverter": func(r *store.DelegationAdoption) {
			r.Status = store.DelegationAdoptionReverted
			r.AdoptedEdgeID = uuid.NewString()
			r.RevertedAt = &now
		},
		"reverted without time": func(r *store.DelegationAdoption) {
			r.Status = store.DelegationAdoptionReverted
			r.AdoptedEdgeID = uuid.NewString()
			r.RevertedByKind, r.RevertedByID = "user", "u"
		},
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			rec := adoptionRecord("c-invalid", store.DelegationAdoptionPending)
			mutate(rec)
			assert.ErrorIs(t, w.cs.CreateDelegationAdoption(w.ctx, rec), store.ErrInvalidInput)

			valid := adoptionRecord("c-invalid-"+uuid.NewString()[:8], store.DelegationAdoptionPending)
			require.NoError(t, w.cs.CreateDelegationAdoption(w.ctx, valid))
			upd := *valid
			mutate(&upd)
			assert.ErrorIs(t, w.cs.UpdateDelegationAdoption(w.ctx, &upd), store.ErrInvalidInput)
		})
	}
	assert.ErrorIs(t, w.cs.CreateDelegationAdoption(w.ctx, nil), store.ErrInvalidInput)
	assert.Empty(t, w.records()[0].RevertedByID, "no rejected write is stored")
}

func TestDelegationAdoptionStoreIndexesAndErrorMapping(t *testing.T) {
	w := newAdoptionWorld(t)

	// A duplicate (cohort, original edge) is rejected.
	first := adoptionRecord("c-idx", store.DelegationAdoptionPending)
	require.NoError(t, w.cs.CreateDelegationAdoption(w.ctx, first))
	dup := adoptionRecord("c-idx", store.DelegationAdoptionPending)
	dup.OriginalEdgeID = first.OriginalEdgeID
	assert.ErrorIs(t, w.cs.CreateDelegationAdoption(w.ctx, dup), store.ErrAlreadyExists)
	// The same original edge in another cohort is allowed.
	other := adoptionRecord("c-idx-2", store.DelegationAdoptionPending)
	other.OriginalEdgeID = first.OriginalEdgeID
	require.NoError(t, w.cs.CreateDelegationAdoption(w.ctx, other))

	// Two adopted records on one edge are rejected, on create and on update.
	adopted := adoptionRecord("c-idx", store.DelegationAdoptionAdopted)
	require.NoError(t, w.cs.CreateDelegationAdoption(w.ctx, adopted))
	second := adoptionRecord("c-idx-3", store.DelegationAdoptionAdopted)
	second.AdoptedEdgeID = adopted.AdoptedEdgeID
	assert.ErrorIs(t, w.cs.CreateDelegationAdoption(w.ctx, second), store.ErrAlreadyExists)
	promote := adoptionRecord("c-idx-4", store.DelegationAdoptionPending)
	require.NoError(t, w.cs.CreateDelegationAdoption(w.ctx, promote))
	promote.Status = store.DelegationAdoptionAdopted
	promote.AdoptedEdgeID = adopted.AdoptedEdgeID
	assert.ErrorIs(t, w.cs.UpdateDelegationAdoption(w.ctx, promote), store.ErrAlreadyExists)

	// An adopted and a recognized record may name the same edge.
	recognized := adoptionRecord("c-idx-5", store.DelegationAdoptionRecognized)
	recognized.AdoptedEdgeID = adopted.AdoptedEdgeID
	require.NoError(t, w.cs.CreateDelegationAdoption(w.ctx, recognized))
	recs, _, err := w.cs.ListDelegationAdoptions(w.ctx, store.DelegationAdoptionFilter{AdoptedEdgeID: adopted.AdoptedEdgeID})
	require.NoError(t, err)
	assert.Len(t, recs, 2)

	// Repeated NULL original edges in one cohort are allowed.
	for i := 0; i < 2; i++ {
		r := adoptionRecord("c-idx", store.DelegationAdoptionRecognized)
		require.Empty(t, r.OriginalEdgeID)
		require.NoError(t, w.cs.CreateDelegationAdoption(w.ctx, r))
	}

	// Unknown IDs.
	_, err = w.cs.GetDelegationAdoption(w.ctx, uuid.NewString())
	assert.ErrorIs(t, err, store.ErrNotFound)
	missing := adoptionRecord("c-idx", store.DelegationAdoptionPending)
	missing.ID = uuid.NewString()
	assert.ErrorIs(t, w.cs.UpdateDelegationAdoption(w.ctx, missing), store.ErrNotFound)
}

// A revert writes the reverted_by and revert_summary fields and keeps the
// adopter in actor_* and the adopted edge summary in after_summary.
func TestDelegationAdoptionStoreRevertFieldsKeepAdopter(t *testing.T) {
	w := newAdoptionWorld(t)
	rec := adoptionRecord("c-rev", store.DelegationAdoptionAdopted)
	rec.ActorKind, rec.ActorID, rec.AfterSummary = "user", "adopter", `{"adopted":true}`
	require.NoError(t, w.cs.CreateDelegationAdoption(w.ctx, rec))

	at := time.Now().UTC().Truncate(time.Second)
	rec.Status = store.DelegationAdoptionReverted
	rec.RevertedByKind, rec.RevertedByID, rec.RevertSummary, rec.RevertedAt = "user", "reverter", `{"original":true}`, &at
	require.NoError(t, w.cs.UpdateDelegationAdoption(w.ctx, rec))
	got, err := w.cs.GetDelegationAdoption(w.ctx, rec.ID)
	require.NoError(t, err)
	assert.Equal(t, "adopter", got.ActorID)
	assert.Equal(t, "user", got.ActorKind)
	assert.Equal(t, `{"adopted":true}`, got.AfterSummary)
	assert.Equal(t, "reverter", got.RevertedByID)
	assert.Equal(t, "user", got.RevertedByKind)
	assert.Equal(t, `{"original":true}`, got.RevertSummary)
	require.NotNil(t, got.RevertedAt)
	assert.True(t, at.Equal(*got.RevertedAt))
}

func TestDelegationEdgeGuardedDeactivateRejectsInvalidGuard(t *testing.T) {
	w := newAdoptionWorld(t)
	a := w.agent(w.user(), nil, "full")
	e := w.activeEdge(a.ID)
	_, err := w.cs.DeactivateDelegationEdgeGuarded(w.ctx, e.ID, store.DelegationEdgeDeactivateGuard{Unrecorded: true, Recorded: true}, store.EdgeDeactivationProvenanceAdopted, "op")
	assert.ErrorIs(t, err, store.ErrInvalidInput)
	_, err = w.cs.DeactivateDelegationEdgeGuarded(w.ctx, e.ID, store.DelegationEdgeDeactivateGuard{Unrecorded: true}, "", "op")
	assert.ErrorIs(t, err, store.ErrInvalidInput)
	_, err = w.cs.DeactivateDelegationEdgeGuarded(w.ctx, e.ID, store.DelegationEdgeDeactivateGuard{Unrecorded: true}, store.EdgeDeactivationProvenanceAdopted, "")
	assert.ErrorIs(t, err, store.ErrInvalidInput)
	_, err = w.cs.DeactivateDelegationEdgeGuarded(w.ctx, uuid.NewString(), store.DelegationEdgeDeactivateGuard{Unrecorded: true}, store.EdgeDeactivationProvenanceAdopted, "op")
	assert.ErrorIs(t, err, store.ErrNotFound)
	assert.True(t, w.activeEdge(a.ID).Active, "no rejected call writes")
}

// The updated-time guard is part of the UPDATE predicate: an update that
// lands after the store's own read of the row, and before its write, makes
// the guarded deactivation affect no row.
func TestDelegationEdgeGuardedDeactivateFailsOnConcurrentUpdate(t *testing.T) {
	w := newAdoptionWorld(t)
	a := w.agent(w.user(), nil, "full")
	e := w.activeEdge(a.ID)
	uid, err := uuid.Parse(e.ID)
	require.NoError(t, err)

	var armed, fired atomic.Bool
	w.cs.client.DelegationEdge.Intercept(entgo.InterceptFunc(func(next entgo.Querier) entgo.Querier {
		return entgo.QuerierFunc(func(ctx context.Context, q entgo.Query) (entgo.Value, error) {
			v, err := next.Query(ctx, q)
			if err == nil && armed.CompareAndSwap(true, false) {
				// Another writer touches the row between the read and the
				// UPDATE.
				_, uerr := w.cs.client.DelegationEdge.UpdateOneID(uid).SetUpdated(e.UpdatedAt.Add(time.Second)).Save(ctx)
				if uerr != nil {
					return nil, uerr
				}
				fired.Store(true)
			}
			return v, err
		})
	}))

	armed.Store(true)
	updated := e.UpdatedAt
	ok, err := w.cs.DeactivateDelegationEdgeGuarded(w.ctx, e.ID, store.DelegationEdgeDeactivateGuard{Unrecorded: true, UpdatedAt: &updated}, store.EdgeDeactivationProvenanceAdopted, "op")
	require.NoError(t, err)
	require.True(t, fired.Load(), "the concurrent update ran inside the call")
	assert.False(t, ok, "a row changed after the read is not deactivated")
	got, err := w.cs.GetDelegationEdge(w.ctx, e.ID)
	require.NoError(t, err)
	assert.True(t, got.Active)
	assert.Empty(t, got.Cause)
}

// A unique violation inside a transaction maps to ErrAlreadyExists and rolls
// the whole transaction back; the store stays usable afterwards. On Postgres
// the failed statement aborts the transaction, so nothing after it may run
// in the same transaction.
func TestDelegationAdoptionUniqueViolationInTxRollsBack(t *testing.T) {
	w := newAdoptionWorld(t)
	a := w.agent(w.user(), nil, "full")
	e := w.activeEdge(a.ID)
	rec := adoptionRecord("c-tx", store.DelegationAdoptionPending)
	rec.OriginalEdgeID = e.ID
	rec.DelegateID = a.ID
	require.NoError(t, w.cs.CreateDelegationAdoption(w.ctx, rec))

	err := w.cs.WithTx(w.ctx, func(tx store.Store) error {
		ok, err := tx.DeactivateDelegationEdgeGuarded(w.ctx, e.ID, store.DelegationEdgeDeactivateGuard{Unrecorded: true}, store.EdgeDeactivationProvenanceAdopted, rec.ID)
		if err != nil || !ok {
			return errors.New("deactivate failed")
		}
		dup := adoptionRecord("c-tx", store.DelegationAdoptionPending)
		dup.OriginalEdgeID = e.ID
		return tx.CreateDelegationAdoption(w.ctx, dup)
	})
	require.ErrorIs(t, err, store.ErrAlreadyExists)
	assert.True(t, w.activeEdge(a.ID).Active, "the deactivation in the failed transaction is rolled back")

	// A second active edge for the same delegate and scope violates the
	// active-edge index inside a transaction.
	err = w.cs.WithTx(w.ctx, func(tx store.Store) error {
		again := *e
		again.ID = ""
		return tx.CreateDelegationEdge(w.ctx, &again)
	})
	require.ErrorIs(t, err, store.ErrAlreadyExists)
	assert.Len(t, w.allEdges(a.ID), 1)

	// The store is usable after the rolled-back transactions.
	rec.Status = store.DelegationAdoptionSkippedChanged
	rec.Reason = "edge_changed"
	require.NoError(t, w.cs.UpdateDelegationAdoption(w.ctx, rec))
}

// The snapshot writes the records and the cohort header in one
// transaction: a failure on the header leaves no record behind.
func TestProvenanceAdoptionSnapshotIsAtomic(t *testing.T) {
	w := newAdoptionWorld(t)
	a := w.agent(w.user(), nil, "full")
	w.backfillMarker()

	// A conflicting header row created inside the snapshot transaction's
	// window makes the header insert fail.
	var once atomic.Bool
	w.cs.client.DelegationAdoption.Use(func(next ent.Mutator) ent.Mutator {
		return ent.MutateFunc(func(ctx context.Context, m ent.Mutation) (ent.Value, error) {
			v, err := next.Mutate(ctx, m)
			if err == nil && once.CompareAndSwap(false, true) {
				return nil, errors.New("injected snapshot failure")
			}
			return v, err
		})
	})
	require.NoError(t, w.cs.Migrate(w.ctx), "a snapshot failure does not fail the boot")
	assert.Empty(t, w.records(), "no record of a failed snapshot is kept")
	_, err := w.cs.GetHubSetting(w.ctx, delegationadoption.CohortSection)
	assert.ErrorIs(t, err, store.ErrNotFound)
	assert.Nil(t, w.marker())

	// The next boot takes the snapshot and adopts.
	w.migrate()
	require.NotNil(t, w.marker())
	assert.Equal(t, store.DelegationAdoptionAdopted, w.record(a.ID).Status)
	var h delegationadoption.Header
	s, err := w.cs.GetHubSetting(w.ctx, delegationadoption.CohortSection)
	require.NoError(t, err)
	require.NoError(t, json.Unmarshal(s.Value, &h))
	assert.Equal(t, 1, h.Counts[string(store.DelegationAdoptionPending)])
}
