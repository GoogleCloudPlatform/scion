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
	"errors"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/delegationadoption"
	"github.com/GoogleCloudPlatform/scion/pkg/ent"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/GoogleCloudPlatform/scion/pkg/store/enttest"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

var errRevertStalePlan = errors.New("revert plan changed since the preview")

// commitRevert reverts recordID the way the admin commit does: inside one
// transaction it re-plans the revert, refuses when the plan fingerprint
// differs from the preview's, applies each hop, and rolls back when a hop is
// not reverted. It returns the hop results and the transaction's error.
func (w *adoptionWorld) commitRevert(recordID, previewFingerprint string) ([]delegationadoption.Result, error) {
	w.t.Helper()
	var results []delegationadoption.Result
	err := w.cs.WithTx(w.ctx, func(tx store.Store) error {
		results = nil
		plan, err := delegationadoption.BuildRevert(w.ctx, tx, []string{recordID}, nil)
		if err != nil {
			return err
		}
		if plan.Fingerprint() != previewFingerprint {
			return errRevertStalePlan
		}
		for _, h := range plan.Hops {
			res, err := delegationadoption.ApplyRevert(w.ctx, tx, h, h.RecordID)
			if err != nil {
				return err
			}
			results = append(results, res)
			if res.Status != store.DelegationAdoptionReverted {
				return errRevertStalePlan
			}
		}
		return nil
	})
	return results, err
}

// assertNotReverted checks that the adopted edge is still the active edge
// and the original is still inactive with its adoption cause.
func assertNotReverted(t *testing.T, w *adoptionWorld, a *store.Agent, original, adopted *store.DelegationEdge) {
	t.Helper()
	assert.Equal(t, adopted.ID, w.activeEdge(a.ID).ID, "the adopted edge stays active")
	o := w.edge(original.ID)
	assert.False(t, o.Active, "the original stays inactive")
	assert.Equal(t, store.EdgeDeactivationProvenanceAdopted, o.Cause)
}

// An adopted edge whose stored updated text names the same instant in
// another form is reverted: the deactivation guard does not compare the
// updated text.
func TestAdoptionRevertNonCanonicalUpdatedText(t *testing.T) {
	enttest.SkipOnPostgres(t, "writes non-canonical SQLite TEXT timestamps; Postgres stores timestamptz")
	for name, form := range nonCanonicalUpdatedForms {
		t.Run(name, func(t *testing.T) {
			w, a, original, adopted := adoptOne(t)
			inst := adopted.UpdatedAt
			w.setUpdatedText(adopted.ID, form(inst))
			require.Equal(t, form(inst), w.updatedText(adopted.ID), "the non-canonical text is what is stored")
			require.True(t, inst.Equal(w.edge(adopted.ID).UpdatedAt), "the driver reads the same instant back")

			preview := w.buildRevert(w.record(a.ID).ID)
			require.Equal(t, delegationadoption.RevertOutcomeRevert, preview.Hops[0].Outcome)

			results, err := w.commitRevert(w.record(a.ID).ID, preview.Fingerprint())
			require.NoError(t, err)
			require.Len(t, results, 1)
			assert.Equal(t, store.DelegationAdoptionReverted, results[0].Status, "reason %q", results[0].Reason)
			assert.Equal(t, original.ID, w.activeEdge(a.ID).ID, "the original is reactivated")
			e := w.edge(adopted.ID)
			assert.False(t, e.Active)
			assert.Equal(t, store.EdgeDeactivationAdoptionReverted, e.Cause)
		})
	}
}

// An adopted edge that changed after the preview is not reverted, and
// nothing is written. A change the plan fingerprint covers (the updated
// instant, the ceiling permissions or kind) refuses the commit as stale.
func TestAdoptionRevertChangedAdoptedEdgeIsRefused(t *testing.T) {
	for name, change := range map[string]func(w *adoptionWorld, e *store.DelegationEdge){
		"updated instant": func(w *adoptionWorld, e *store.DelegationEdge) {
			w.setEdge(e, func(u *ent.DelegationEdgeUpdateOne) { u.SetUpdated(e.UpdatedAt.Add(time.Second)) })
		},
		"ceiling permissions": func(w *adoptionWorld, e *store.DelegationEdge) {
			ids := append([]string{}, e.PermissionIDs...)
			require.NotEmpty(w.t, ids)
			w.setEdge(e, func(u *ent.DelegationEdgeUpdateOne) {
				u.SetNillableCeilingPermissionIds(marshalCeilingPermissionIDs(ids[1:])).SetUpdated(e.UpdatedAt)
			})
		},
		"ceiling kind": func(w *adoptionWorld, e *store.DelegationEdge) {
			kind := store.EffectCeilingPrincipal
			if e.Kind == kind {
				kind = store.EffectCeilingBounded
			}
			w.setEdge(e, func(u *ent.DelegationEdgeUpdateOne) { u.SetCeilingKind(string(kind)).SetUpdated(e.UpdatedAt) })
		},
	} {
		t.Run(name, func(t *testing.T) {
			w, a, original, adopted := adoptOne(t)
			rec := w.record(a.ID)
			preview := w.buildRevert(rec.ID)
			require.Equal(t, delegationadoption.RevertOutcomeRevert, preview.Hops[0].Outcome)

			change(w, adopted)

			_, err := w.commitRevert(rec.ID, preview.Fingerprint())
			require.ErrorIs(t, err, errRevertStalePlan)
			assertNotReverted(t, w, a, original, adopted)
			assert.Equal(t, store.DelegationAdoptionAdopted, w.record(a.ID).Status)
		})
	}
}

// A hop applied against an adopted edge that is no longer recorded returns
// skipped_changed (adopted_edge_changed) and writes nothing.
func TestAdoptionRevertUnrecordedAdoptedEdgeSkipped(t *testing.T) {
	w, a, original, adopted := adoptOne(t)
	preview := w.buildRevert(w.record(a.ID).ID)
	require.Equal(t, delegationadoption.RevertOutcomeRevert, preview.Hops[0].Outcome)

	w.setEdge(adopted, func(u *ent.DelegationEdgeUpdateOne) {
		u.SetCeilingKind(string(store.EffectCeilingUnrecorded)).SetUpdated(adopted.UpdatedAt)
	})

	res := w.applyRevert(preview.Hops[0])
	assert.Equal(t, store.DelegationAdoptionSkippedChanged, res.Status)
	assert.Equal(t, delegationadoption.ReasonAdoptedEdgeChanged, res.Reason)
	assertNotReverted(t, w, a, original, adopted)
}

// setEdge applies set to e's row through ent, bypassing the store's write
// guards, to simulate a concurrent change.
func (w *adoptionWorld) setEdge(e *store.DelegationEdge, set func(u *ent.DelegationEdgeUpdateOne)) {
	w.t.Helper()
	uid, err := uuid.Parse(e.ID)
	require.NoError(w.t, err)
	u := w.cs.client.DelegationEdge.UpdateOneID(uid)
	set(u)
	_, err = u.Save(w.ctx)
	require.NoError(w.t, err)
}
