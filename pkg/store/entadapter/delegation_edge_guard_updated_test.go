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
	"sync/atomic"
	"testing"
	"time"

	entgo "entgo.io/ent"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/GoogleCloudPlatform/scion/pkg/store/enttest"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// guardedDeactivate runs the unrecorded guarded deactivation of edgeID with
// the given updated instant on s.
func guardedDeactivate(ctx context.Context, s store.Store, edgeID string, updated time.Time) (bool, error) {
	return s.DeactivateDelegationEdgeGuarded(ctx, edgeID,
		store.DelegationEdgeDeactivateGuard{Unrecorded: true, UpdatedAt: &updated},
		store.EdgeDeactivationProvenanceAdopted, "op")
}

// An edge whose stored updated text names the guard's instant in a
// non-canonical form is deactivated, outside and inside WithTx.
func TestDelegationEdgeGuardedDeactivateNonCanonicalUpdatedText(t *testing.T) {
	enttest.SkipOnPostgres(t, "writes non-canonical SQLite TEXT timestamps; Postgres stores timestamptz")
	for name, form := range nonCanonicalUpdatedForms {
		for _, inTx := range []bool{false, true} {
			sub := name
			if inTx {
				sub += "/in-tx"
			}
			t.Run(sub, func(t *testing.T) {
				w := newAdoptionWorld(t)
				a := w.agent(w.user(), nil, "full")
				e := w.activeEdge(a.ID)
				inst := e.UpdatedAt
				w.setUpdatedText(e.ID, form(inst))
				require.Equal(t, form(inst), w.updatedText(e.ID), "the non-canonical text is what is stored")
				require.True(t, inst.Equal(w.edge(e.ID).UpdatedAt), "the driver reads the same instant back")

				var ok bool
				var err error
				if inTx {
					err = w.cs.WithTx(w.ctx, func(tx store.Store) error {
						ok, err = guardedDeactivate(w.ctx, tx, e.ID, inst)
						return err
					})
				} else {
					ok, err = guardedDeactivate(w.ctx, w.cs, e.ID, inst)
				}
				require.NoError(t, err)
				assert.True(t, ok)
				got := w.edge(e.ID)
				assert.False(t, got.Active)
				assert.Equal(t, store.EdgeDeactivationProvenanceAdopted, got.Cause)
			})
		}
	}
}

// A guard naming a different instant than the row's, whatever text form the
// row stores, deactivates nothing and leaves the stored text as it was.
func TestDelegationEdgeGuardedDeactivateDifferentInstant(t *testing.T) {
	enttest.SkipOnPostgres(t, "writes non-canonical SQLite TEXT timestamps; Postgres stores timestamptz")
	for name, form := range nonCanonicalUpdatedForms {
		t.Run(name, func(t *testing.T) {
			w := newAdoptionWorld(t)
			a := w.agent(w.user(), nil, "full")
			e := w.activeEdge(a.ID)
			inst := e.UpdatedAt
			w.setUpdatedText(e.ID, form(inst))

			for _, other := range []time.Time{inst.Add(time.Nanosecond), inst.Add(-time.Second)} {
				ok, err := guardedDeactivate(w.ctx, w.cs, e.ID, other)
				require.NoError(t, err)
				assert.False(t, ok)
			}
			got := w.edge(e.ID)
			assert.True(t, got.Active, "the row stays active")
			assert.Empty(t, got.Cause)
			assert.Equal(t, form(inst), w.updatedText(e.ID), "the stored text is untouched")
		})
	}
}

// An inactive edge is not deactivated again even when the guard names its
// current updated instant; a missing edge is ErrNotFound.
func TestDelegationEdgeGuardedDeactivateInactiveAndMissing(t *testing.T) {
	w := newAdoptionWorld(t)
	a := w.agent(w.user(), nil, "full")
	e := w.activeEdge(a.ID)
	ok, err := w.cs.DeactivateDelegationEdgeGuarded(w.ctx, e.ID, store.DelegationEdgeDeactivateGuard{}, store.EdgeDeactivationScopeReissueReplaced, "first")
	require.NoError(t, err)
	require.True(t, ok)
	inactive := w.edge(e.ID)

	ok, err = guardedDeactivate(w.ctx, w.cs, e.ID, inactive.UpdatedAt)
	require.NoError(t, err)
	assert.False(t, ok)
	got := w.edge(e.ID)
	assert.False(t, got.Active)
	assert.Equal(t, store.EdgeDeactivationScopeReissueReplaced, got.Cause, "the first deactivation stands")
	assert.True(t, inactive.UpdatedAt.Equal(got.UpdatedAt), "no write")

	_, err = guardedDeactivate(w.ctx, w.cs, uuid.NewString(), inactive.UpdatedAt)
	assert.ErrorIs(t, err, store.ErrNotFound)
}

// A write that lands after the store has read the row's raw updated text,
// and before its UPDATE, makes the guarded deactivation affect no row, also
// when the row started in a non-canonical form.
func TestDelegationEdgeGuardedDeactivateNonCanonicalConcurrentUpdate(t *testing.T) {
	enttest.SkipOnPostgres(t, "writes non-canonical SQLite TEXT timestamps; Postgres stores timestamptz")
	w := newAdoptionWorld(t)
	a := w.agent(w.user(), nil, "full")
	e := w.activeEdge(a.ID)
	inst := e.UpdatedAt
	form := nonCanonicalUpdatedForms["rfc3339-offset"]
	w.setUpdatedText(e.ID, form(inst))
	later := form(inst.Add(time.Second))

	var armed, fired atomic.Bool
	w.cs.client.DelegationEdge.Intercept(entgo.InterceptFunc(func(next entgo.Querier) entgo.Querier {
		return entgo.QuerierFunc(func(ctx context.Context, q entgo.Query) (entgo.Value, error) {
			v, err := next.Query(ctx, q)
			if err == nil && armed.CompareAndSwap(true, false) {
				// The raw read has run; another writer moves the row on
				// before the UPDATE.
				if _, uerr := w.cs.DB().ExecContext(ctx, "UPDATE delegation_edges SET updated = ? WHERE id = ?", later, e.ID); uerr != nil {
					return nil, uerr
				}
				fired.Store(true)
			}
			return v, err
		})
	}))

	armed.Store(true)
	ok, err := guardedDeactivate(w.ctx, w.cs, e.ID, inst)
	require.NoError(t, err)
	require.True(t, fired.Load(), "the concurrent update ran inside the call")
	assert.False(t, ok, "a row changed after the read is not deactivated")
	got := w.edge(e.ID)
	assert.True(t, got.Active)
	assert.Empty(t, got.Cause)
	assert.Equal(t, later, w.updatedText(e.ID))
}
