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
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/delegationadoption"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// nonCanonicalUpdatedForms are text forms of one instant that SQLite can hold
// in delegation_edges.updated and that the driver reads back as that instant,
// but that differ from the form a bound time.Time is written in.
var nonCanonicalUpdatedForms = map[string]func(time.Time) string{
	"rfc3339-z":           func(i time.Time) string { return i.UTC().Format(time.RFC3339Nano) },
	"rfc3339-offset":      func(i time.Time) string { return i.In(time.FixedZone("CEST", 2*3600)).Format(time.RFC3339Nano) },
	"go-string-utc":       func(i time.Time) string { return i.UTC().String() },
	"go-string-offset":    func(i time.Time) string { return i.In(time.FixedZone("CEST", 2*3600)).String() },
	"go-string-monotonic": func(i time.Time) string { return i.UTC().String() + " m=+12.345678901" },
}

// setUpdatedText rewrites the stored updated text of edgeID.
func (w *adoptionWorld) setUpdatedText(edgeID, text string) {
	w.t.Helper()
	_, err := w.cs.DB().ExecContext(w.ctx, "UPDATE delegation_edges SET updated = ? WHERE id = ?", text, edgeID)
	require.NoError(w.t, err)
}

func (w *adoptionWorld) updatedText(edgeID string) string {
	w.t.Helper()
	var raw string
	require.NoError(w.t, w.cs.DB().QueryRowContext(w.ctx, "SELECT CAST(updated AS TEXT) FROM delegation_edges WHERE id = ?", edgeID).Scan(&raw))
	return raw
}

// A legacy edge whose stored updated text is not in canonical form, but
// names the same instant, is adopted on boot, and so is its child.
func TestProvenanceAdoptionNonCanonicalUpdatedText(t *testing.T) {
	inst := time.Date(2026, 9, 18, 1, 24, 24, 503338758, time.UTC)
	for name, form := range nonCanonicalUpdatedForms {
		t.Run(name, func(t *testing.T) {
			w := newAdoptionWorld(t)
			u := w.user()
			a := w.agent(u, nil, "full")
			b := w.agent(u, a, "baseline")
			ea, eb := w.activeEdge(a.ID), w.activeEdge(b.ID)
			for _, id := range []string{ea.ID, eb.ID} {
				w.setUpdatedText(id, form(inst))
			}
			require.Equal(t, form(inst), w.updatedText(ea.ID), "the non-canonical text is what is stored")
			readBack := w.activeEdge(a.ID)
			require.True(t, inst.Equal(readBack.UpdatedAt), "the driver reads the same instant back: %v", readBack.UpdatedAt)
			w.backfillMarker()

			w.migrate()

			assertAdopted(t, w, a, readBack, compat(t, "full", false))
			assertAdopted(t, w, b, w.mustEdge(eb.ID), compat(t, "baseline", false))
			assert.Equal(t, store.DelegationAdoptionAdopted, w.record(a.ID).Status)
			assert.Equal(t, store.DelegationAdoptionAdopted, w.record(b.ID).Status)
			m := w.marker()
			require.NotNil(t, m)
			assert.Equal(t, 2, m.Counts["adopted"])
			assert.Equal(t, delegationadoption.RetryVersion, m.RetryVersion)
		})
	}
}

// Times written through ent in a non-UTC zone or with a monotonic clock
// reading are adopted too.
func TestProvenanceAdoptionNonUTCUpdatedThroughEnt(t *testing.T) {
	for name, mk := range map[string]func() time.Time{
		"monotonic-local": func() time.Time { return time.Now() },
		"non-utc-offset":  func() time.Time { return time.Now().In(time.FixedZone("CEST", 2*3600)).Round(0) },
	} {
		t.Run(name, func(t *testing.T) {
			w := newAdoptionWorld(t)
			u := w.user()
			a := w.agent(u, nil, "full")
			b := w.agent(u, a, "baseline")
			for _, e := range []*store.DelegationEdge{w.activeEdge(a.ID), w.activeEdge(b.ID)} {
				uid, err := uuid.Parse(e.ID)
				require.NoError(t, err)
				_, err = w.cs.client.DelegationEdge.UpdateOneID(uid).SetUpdated(mk()).Save(w.ctx)
				require.NoError(t, err)
			}
			w.backfillMarker()
			w.migrate()
			assert.Equal(t, store.DelegationAdoptionAdopted, w.record(a.ID).Status)
			assert.Equal(t, store.DelegationAdoptionAdopted, w.record(b.ID).Status)
		})
	}
}

func (w *adoptionWorld) mustEdge(id string) *store.DelegationEdge {
	w.t.Helper()
	e, err := w.cs.GetDelegationEdge(w.ctx, id)
	require.NoError(w.t, err)
	return e
}

// An edge replaced after the snapshot is still skipped (edge_changed), its
// child is not adopted under it (ancestor_not_adopted), and the retry pass
// that runs before the marker leaves both skipped.
func TestProvenanceAdoptionReplacedEdgeStillSkipped(t *testing.T) {
	w := newAdoptionWorld(t)
	u := w.user()
	a := w.agent(u, nil, "full")
	b := w.agent(u, a, "baseline")
	ea := w.activeEdge(a.ID)
	w.backfillMarker()
	var replacement string
	w.cs.adoptionHopHook = func(i int, rec *store.DelegationAdoption) error {
		if i != 0 {
			return nil
		}
		// Another writer replaces a's edge with a new unrecorded row.
		if _, err := w.cs.DeactivateDelegationEdgesForDelegate(w.ctx, store.DelegationPrincipalAgent, rec.DelegateID,
			store.Deactivation{Cause: store.EdgeDeactivationAgentSoftDelete, OpID: "replace"}); err != nil {
			return err
		}
		next := *ea
		next.ID = ""
		if err := w.cs.CreateDelegationEdge(w.ctx, &next); err != nil {
			return err
		}
		replacement = next.ID
		return nil
	}
	w.migrate()

	ra, rb := w.record(a.ID), w.record(b.ID)
	assert.Equal(t, store.DelegationAdoptionSkippedChanged, ra.Status)
	assert.Equal(t, string(delegationadoption.ReasonEdgeChanged), ra.Reason)
	assert.Equal(t, store.DelegationAdoptionSkippedChanged, rb.Status)
	assert.Equal(t, string(delegationadoption.ReasonAncestorNotAdopted), rb.Reason)
	e := w.activeEdge(a.ID)
	assert.Equal(t, replacement, e.ID, "the replacement row is not adopted by the boot cohort")
	assert.Equal(t, store.EffectCeilingUnrecorded, e.Kind)
	assert.Equal(t, store.EffectCeilingUnrecorded, w.activeEdge(b.ID).Kind)
	m := w.marker()
	require.NotNil(t, m)
	assert.Equal(t, delegationadoption.RetryVersion, m.RetryVersion)
	assert.Equal(t, 2, m.Counts["skipped_changed"])
}

// oldMarkerWorld builds the state a hub reaches when an earlier build ran the
// boot migration and its write guard skipped a parent (edge_changed) and its
// child (ancestor_not_adopted): the cohort header, those records, and a
// marker with no retry version.
func oldMarkerWorld(t *testing.T) (w *adoptionWorld, a, b *store.Agent) {
	t.Helper()
	w = newAdoptionWorld(t)
	u := w.user()
	a = w.agent(u, nil, "full")
	b = w.agent(u, a, "baseline")
	w.backfillMarker()

	plan, err := delegationadoption.Build(w.ctx, w.cs, delegationadoption.Scope{})
	require.NoError(t, err)
	cohortID := uuid.NewString()
	recs := delegationadoption.SnapshotRecords(plan, cohortID, store.DelegationAdoptionOriginBoot)
	require.Len(t, recs, 2)
	for _, r := range recs {
		require.Equal(t, store.DelegationAdoptionPending, r.Status)
		r.Status = store.DelegationAdoptionSkippedChanged
		r.Reason = string(delegationadoption.ReasonEdgeChanged)
		if r.DelegateID == b.ID {
			r.Reason = string(delegationadoption.ReasonAncestorNotAdopted)
		}
		require.NoError(t, w.cs.CreateDelegationAdoption(w.ctx, r))
	}
	header := delegationadoption.Header{
		SchemaVersion: delegationadoption.HeaderSchemaVersion,
		PolicyVersion: int(delegationadoption.PolicyVersion),
		CohortID:      cohortID,
		Counts:        map[string]int{"pending": 2},
	}
	hv, err := json.Marshal(header)
	require.NoError(t, err)
	_, err = w.cs.UpsertHubSetting(w.ctx, delegationadoption.CohortSection, hv, "migration", 0, "seeded")
	require.NoError(t, err)
	// The marker layout of the earlier build: no retry_version.
	mv, err := json.Marshal(map[string]any{
		"schema_version": 1, "policy_version": 1, "cohort_id": cohortID, "completed": true,
		"counts": map[string]int{"skipped_changed": 2},
	})
	require.NoError(t, err)
	_, err = w.cs.UpsertHubSetting(w.ctx, delegationadoption.MarkerSection, mv, "migration", 0, "seeded")
	require.NoError(t, err)
	return w, a, b
}

// A hub whose marker predates the retry pass re-adopts the guard-skipped
// records on the next start, top-down, and the pass runs once.
func TestProvenanceAdoptionRetriesSkippedRecordsAfterOldMarker(t *testing.T) {
	w, a, b := oldMarkerWorld(t)
	ea, eb := w.activeEdge(a.ID), w.activeEdge(b.ID)
	cohort := w.marker().CohortID

	w.migrate()

	assertAdopted(t, w, a, ea, compat(t, "full", false))
	assertAdopted(t, w, b, eb, compat(t, "baseline", false))
	ra, rb := w.record(a.ID), w.record(b.ID)
	assert.Equal(t, store.DelegationAdoptionAdopted, ra.Status)
	assert.Empty(t, ra.Reason)
	assert.Equal(t, w.activeEdge(a.ID).ID, ra.AdoptedEdgeID)
	assert.Equal(t, store.DelegationAdoptionAdopted, rb.Status)
	assert.Len(t, w.records(), 2, "no new records")
	m := w.marker()
	require.NotNil(t, m)
	assert.Equal(t, cohort, m.CohortID, "the same cohort")
	assert.True(t, m.Completed)
	assert.Equal(t, delegationadoption.RetryVersion, m.RetryVersion)
	assert.Equal(t, 2, m.Counts["adopted"])
	assert.Zero(t, m.Counts["skipped_changed"])
	assert.Contains(t, w.logs.String(), "retried skipped hops")

	s, err := w.cs.GetHubSetting(w.ctx, delegationadoption.MarkerSection)
	require.NoError(t, err)
	w.migrate()
	s2, err := w.cs.GetHubSetting(w.ctx, delegationadoption.MarkerSection)
	require.NoError(t, err)
	assert.Equal(t, s.Revision, s2.Revision, "the retry pass does not run again")
	assert.Len(t, w.allEdges(a.ID), 2)
}

// The retry keeps every rule: a stranded hop whose fingerprinted state
// changed since the snapshot stays skipped (fingerprint_changed), its child
// stays skipped (ancestor_not_adopted), and a record skipped for a reason
// the pass does not retry is left untouched.
func TestProvenanceAdoptionRetryKeepsRules(t *testing.T) {
	w, a, b := oldMarkerWorld(t)
	stored, err := w.cs.GetAgent(w.ctx, a.ID)
	require.NoError(t, err)
	stored.AppliedConfig.AgentRole = "baseline"
	require.NoError(t, w.cs.UpdateAgent(w.ctx, stored))

	w.migrate()

	ra, rb := w.record(a.ID), w.record(b.ID)
	assert.Equal(t, store.DelegationAdoptionSkippedChanged, ra.Status)
	assert.Equal(t, string(delegationadoption.ReasonFingerprintChanged), ra.Reason)
	assert.Equal(t, store.DelegationAdoptionSkippedChanged, rb.Status)
	assert.Equal(t, string(delegationadoption.ReasonAncestorNotAdopted), rb.Reason)
	assert.Equal(t, store.EffectCeilingUnrecorded, w.activeEdge(a.ID).Kind)
	assert.Equal(t, store.EffectCeilingUnrecorded, w.activeEdge(b.ID).Kind)
	require.Equal(t, delegationadoption.RetryVersion, w.marker().RetryVersion)

	// fingerprint_changed is not retryable: a forced re-run of the pass
	// leaves the record as it is.
	assert.False(t, delegationadoption.Retryable(ra.Reason))
	failed := w.cs.retrySkippedHops(w.ctx, ra.CohortID)
	assert.False(t, failed)
	assert.Equal(t, ra.UpdatedAt, w.record(a.ID).UpdatedAt, "a non-retryable record is not rewritten")
}

// A hop write failure during the retry pass leaves the old marker in place,
// so the next start runs the pass again.
func TestProvenanceAdoptionRetryWriteFailureRetriesNextStart(t *testing.T) {
	w, a, b := oldMarkerWorld(t)
	ea := w.activeEdge(a.ID)
	w.cs.adoptionTxHook = func(store.Store, *store.DelegationAdoption) error {
		return errors.New("injected write failure")
	}
	w.migrate()
	assert.Zero(t, w.marker().RetryVersion, "the marker is not advanced")
	assert.Equal(t, store.DelegationAdoptionSkippedChanged, w.record(a.ID).Status)
	assert.Equal(t, ea.ID, w.activeEdge(a.ID).ID)
	assert.Contains(t, w.logs.String(), "retry of a skipped hop failed")

	w.cs.adoptionTxHook = nil
	w.migrate()
	assert.Equal(t, store.DelegationAdoptionAdopted, w.record(a.ID).Status)
	assert.Equal(t, store.DelegationAdoptionAdopted, w.record(b.ID).Status)
	assert.Equal(t, delegationadoption.RetryVersion, w.marker().RetryVersion)
}

// A resumed cohort (no marker yet) whose earlier run left guard-caused
// skips alongside pending records adopts both before writing the marker.
func TestProvenanceAdoptionResumedCohortRetriesSkips(t *testing.T) {
	w, a, b := oldMarkerWorld(t)
	require.NoError(t, w.cs.DeleteHubSetting(w.ctx, delegationadoption.MarkerSection))
	w.migrate()
	assert.Equal(t, store.DelegationAdoptionAdopted, w.record(a.ID).Status)
	assert.Equal(t, store.DelegationAdoptionAdopted, w.record(b.ID).Status)
	m := w.marker()
	require.NotNil(t, m)
	assert.Equal(t, delegationadoption.RetryVersion, m.RetryVersion)
}
