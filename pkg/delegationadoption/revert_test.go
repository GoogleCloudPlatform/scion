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

package delegationadoption

import (
	"context"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// revertWorld is an in-memory RevertReader over a planner world.
type revertWorld struct {
	*world
	records map[string]*store.DelegationAdoption
}

func newRevertWorld() *revertWorld {
	return &revertWorld{world: newWorld(), records: map[string]*store.DelegationAdoption{}}
}

func (w *revertWorld) GetDelegationAdoption(_ context.Context, id string) (*store.DelegationAdoption, error) {
	if r, ok := w.records[id]; ok {
		cp := *r
		return &cp, nil
	}
	return nil, store.ErrNotFound
}

func (w *revertWorld) ListDelegationAdoptions(_ context.Context, f store.DelegationAdoptionFilter) ([]*store.DelegationAdoption, int, error) {
	var out []*store.DelegationAdoption
	for _, r := range w.records {
		if f.AdoptedEdgeID != "" && r.AdoptedEdgeID != f.AdoptedEdgeID {
			continue
		}
		cp := *r
		out = append(out, &cp)
	}
	return out, len(out), nil
}

func (w *revertWorld) GetDelegationEdge(_ context.Context, id string) (*store.DelegationEdge, error) {
	for _, e := range w.edges {
		if e.ID == id {
			cp := *e
			return &cp, nil
		}
	}
	return nil, store.ErrNotFound
}

// adoptedPair builds agent a with an inactive original row and an active
// migration-recorded replacement, and returns both.
func (w *revertWorld) adoptedPair(t *testing.T) (original, adopted *store.DelegationEdge) {
	t.Helper()
	w.user("u")
	w.agent("a", "u", "full")
	original = w.edgeOf(t, "a")
	original.Active = false
	original.Cause = store.EdgeDeactivationProvenanceAdopted
	adopted = w.edge(store.DelegationPrincipalUser, "u", "a", "full")
	recorded(adopted, migrationCeiling(policy(t, "full", false)), store.SourceCredentialSystemMigration)
	return original, adopted
}

func (w *revertWorld) record(id string, status store.DelegationAdoptionStatus, original, adopted string) *store.DelegationAdoption {
	r := &store.DelegationAdoption{ID: id, CohortID: "c-" + id, Origin: store.DelegationAdoptionOriginBoot,
		DelegateID: "a", Status: status, OriginalEdgeID: original, AdoptedEdgeID: adopted}
	w.records[id] = r
	return r
}

func recordIDs(recs []*store.DelegationAdoption) []string {
	var out []string
	for _, r := range recs {
		out = append(out, r.ID)
	}
	return out
}

// Two records that point at one adopted edge are reverted by one hop: the
// edge is reverted once and both records are marked.
func TestBuildRevertDedupesRecordsByAdoptedEdge(t *testing.T) {
	ctx := context.Background()
	w := newRevertWorld()
	orig, adopted := w.adoptedPair(t)
	w.record("r1", store.DelegationAdoptionAdopted, orig.ID, adopted.ID)
	w.record("r2", store.DelegationAdoptionRecognized, "", adopted.ID)

	// Both requested: one hop, covering the other record.
	p, err := BuildRevert(ctx, w, []string{"r1", "r2"}, nil)
	require.NoError(t, err)
	require.Len(t, p.Hops, 1)
	h := p.Hops[0]
	assert.Equal(t, RevertOutcomeRevert, h.Outcome)
	assert.Equal(t, "r1", h.RecordID)
	assert.Equal(t, orig.ID, h.OriginalEdgeID)
	assert.Equal(t, []string{"r2"}, h.CoveredRecordIDs)
	assert.Equal(t, []string{"r1", "r2"}, recordIDs(h.Records()))
	assert.Zero(t, p.Refused())

	// The recognized record first: it alone has no original and is
	// refused, but the adopted record's revert covers it.
	p2, err := BuildRevert(ctx, w, []string{"r2", "r1"}, nil)
	require.NoError(t, err)
	require.Len(t, p2.Hops, 1)
	assert.Equal(t, "r1", p2.Hops[0].RecordID)
	assert.Equal(t, []string{"r2"}, p2.Hops[0].CoveredRecordIDs)
	assert.Zero(t, p2.Refused())

	// Only one requested: the sibling is covered, so no record is
	// left pointing at an inactive edge.
	p3, err := BuildRevert(ctx, w, []string{"r1"}, nil)
	require.NoError(t, err)
	require.Len(t, p3.Hops, 1)
	assert.Equal(t, []string{"r2"}, p3.Hops[0].CoveredRecordIDs)

	// A sibling that is not revertible is not covered.
	w.records["r2"].Status = store.DelegationAdoptionReverted
	p4, err := BuildRevert(ctx, w, []string{"r1"}, nil)
	require.NoError(t, err)
	assert.Empty(t, p4.Hops[0].CoveredRecordIDs)
	assert.NotEqual(t, p3.Fingerprint(), p4.Fingerprint(), "the covered records are fingerprinted")
}

// A covered record whose original edge is empty or equals the hop's is
// reverted with the hop. A covered record that names a different original
// edge refuses the hop.
func TestBuildRevertRefusesCoveredRecordWithOtherOriginal(t *testing.T) {
	ctx := context.Background()
	w := newRevertWorld()
	orig, adopted := w.adoptedPair(t)
	w.record("r1", store.DelegationAdoptionAdopted, orig.ID, adopted.ID)
	w.record("r2", store.DelegationAdoptionRecognized, orig.ID, adopted.ID)
	w.record("r3", store.DelegationAdoptionRecognized, "", adopted.ID)

	// Equal and empty covered originals: the hop is admitted.
	p, err := BuildRevert(ctx, w, []string{"r1"}, nil)
	require.NoError(t, err)
	require.Len(t, p.Hops, 1)
	h := p.Hops[0]
	assert.Equal(t, RevertOutcomeRevert, h.Outcome)
	assert.Empty(t, h.Reason)
	assert.Equal(t, orig.ID, h.OriginalEdgeID)
	assert.Equal(t, []string{"r2", "r3"}, h.CoveredRecordIDs)
	assert.Zero(t, p.Refused())

	// A covered record names a different original edge: the hop is
	// refused, whichever record is requested first.
	w.record("r4", store.DelegationAdoptionRecognized, "e-other", adopted.ID)
	for _, req := range [][]string{{"r1"}, {"r1", "r4"}, {"r4", "r1"}} {
		p2, err := BuildRevert(ctx, w, req, nil)
		require.NoError(t, err)
		require.Len(t, p2.Hops, 1, "request %v", req)
		h2 := p2.Hops[0]
		assert.Equal(t, "r1", h2.RecordID, "request %v", req)
		assert.Equal(t, RevertOutcomeRefused, h2.Outcome, "request %v", req)
		assert.Equal(t, ReasonCoveredOriginalDiffers, h2.Reason, "request %v", req)
		assert.Equal(t, []string{"r2", "r3", "r4"}, h2.CoveredRecordIDs, "request %v", req)
		assert.Equal(t, 1, p2.Refused(), "request %v", req)
		assert.NotEqual(t, p.Fingerprint(), p2.Fingerprint(), "request %v", req)
	}
}

// MatchesOriginal requires every identifying field, including the role, to
// equal the adopted edge's.
func TestMatchesOriginalComparesEveryField(t *testing.T) {
	w := newRevertWorld()
	orig, adopted := w.adoptedPair(t)
	require.True(t, MatchesOriginal(adopted, orig))
	mutations := map[string]func(e *store.DelegationEdge){
		"role":           func(e *store.DelegationEdge) { e.Role = "baseline" },
		"delegator type": func(e *store.DelegationEdge) { e.DelegatorType = store.DelegationPrincipalAgent },
		"delegator":      func(e *store.DelegationEdge) { e.DelegatorID = "other" },
		"delegate type":  func(e *store.DelegationEdge) { e.DelegateType = store.DelegationPrincipalUser },
		"delegate":       func(e *store.DelegationEdge) { e.DelegateID = "other" },
		"scope type":     func(e *store.DelegationEdge) { e.ScopeType = "hub" },
		"scope":          func(e *store.DelegationEdge) { e.ScopeID = "other" },
		"active":         func(e *store.DelegationEdge) { e.Active = true },
		"recorded":       func(e *store.DelegationEdge) { e.ProvenanceVersion = store.ProvenanceVersionV1 },
		"other cause":    func(e *store.DelegationEdge) { e.Cause = store.EdgeDeactivationAgentSoftDelete },
	}
	for name, mutate := range mutations {
		t.Run(name, func(t *testing.T) {
			cand := *orig
			mutate(&cand)
			assert.False(t, MatchesOriginal(adopted, &cand))
		})
	}
}

// A confirmed original whose role differs from the adopted edge's is
// refused.
func TestBuildRevertRefusesConfirmedOriginalWithOtherRole(t *testing.T) {
	ctx := context.Background()
	w := newRevertWorld()
	orig, adopted := w.adoptedPair(t)
	orig.Role = "baseline"
	w.record("r1", store.DelegationAdoptionRecognized, "", adopted.ID)
	p, err := BuildRevert(ctx, w, []string{"r1"}, map[string]string{"r1": orig.ID})
	require.NoError(t, err)
	require.Len(t, p.Hops, 1)
	assert.Equal(t, RevertOutcomeRefused, p.Hops[0].Outcome)
	assert.Equal(t, ReasonOriginalChanged, p.Hops[0].Reason)
}
