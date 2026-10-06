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

package entadapter

import (
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/delegationadoption"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The agent lifecycle and adoption revert select edges by deactivation
// cause, so neither reactivates an edge the other deactivated. These tests
// drive the same store calls the lifecycle transactions make
// (agent_lifecycle_tx.go) against adopted edges.

// adoptOne adopts a single full agent and returns it with its original and
// adopted edges.
func adoptOne(t *testing.T) (*adoptionWorld, *store.Agent, *store.DelegationEdge, *store.DelegationEdge) {
	t.Helper()
	w := newAdoptionWorld(t)
	a := w.agent(w.user(), nil, "full")
	original := w.activeEdge(a.ID)
	w.backfillMarker()
	w.migrate()
	adopted := w.activeEdge(a.ID)
	require.NotEqual(t, original.ID, adopted.ID)
	require.Equal(t, store.DelegationAdoptionAdopted, w.record(a.ID).Status)
	return w, a, original, adopted
}

func (w *adoptionWorld) edge(id string) *store.DelegationEdge {
	w.t.Helper()
	e, err := w.cs.GetDelegationEdge(w.ctx, id)
	require.NoError(w.t, err)
	return e
}

func (w *adoptionWorld) buildRevert(recordID string) *delegationadoption.RevertPlan {
	w.t.Helper()
	plan, err := delegationadoption.BuildRevert(w.ctx, w.cs, []string{recordID}, nil)
	require.NoError(w.t, err)
	require.Len(w.t, plan.Hops, 1)
	return plan
}

func (w *adoptionWorld) applyRevert(hop *delegationadoption.RevertHop) delegationadoption.Result {
	w.t.Helper()
	var res delegationadoption.Result
	require.NoError(w.t, w.cs.WithTx(w.ctx, func(tx store.Store) error {
		var err error
		res, err = delegationadoption.ApplyRevert(w.ctx, tx, hop, "revert-op")
		return err
	}))
	return res
}

func (w *adoptionWorld) lifecycleDeactivate(agentID string, cause store.EdgeDeactivationCause, opID string) int {
	w.t.Helper()
	n, err := w.cs.DeactivateDelegationEdgesForDelegate(w.ctx, store.DelegationPrincipalAgent, agentID,
		store.Deactivation{Cause: cause, OpID: opID})
	require.NoError(w.t, err)
	return n
}

// A soft delete deactivates the adopted edge only; a revert planned before
// or after it changes nothing, and the restore reactivates the adopted edge
// and leaves the original inactive with its adoption cause.
func TestAdoptionRevertAndSoftDeleteRestore(t *testing.T) {
	w, a, original, adopted := adoptOne(t)
	rec := w.record(a.ID)
	stale := w.buildRevert(rec.ID)
	require.Equal(t, delegationadoption.RevertOutcomeRevert, stale.Hops[0].Outcome)

	assert.Equal(t, 1, w.lifecycleDeactivate(a.ID, store.EdgeDeactivationAgentSoftDelete, "soft-1"), "only the adopted edge was active")
	assert.Equal(t, store.EdgeDeactivationProvenanceAdopted, w.edge(original.ID).Cause)

	refused := w.buildRevert(rec.ID)
	assert.Equal(t, delegationadoption.RevertOutcomeRefused, refused.Hops[0].Outcome)
	assert.Equal(t, delegationadoption.ReasonAdoptedEdgeChanged, refused.Hops[0].Reason)

	res := w.applyRevert(stale.Hops[0])
	assert.Equal(t, delegationadoption.ReasonAdoptedEdgeChanged, res.Reason, "the guarded UPDATE matches no row")
	assert.False(t, w.edge(original.ID).Active, "a revert does not reactivate the original of a soft-deleted agent")
	assert.Equal(t, store.EdgeDeactivationAgentSoftDelete, w.edge(adopted.ID).Cause)

	n, err := w.cs.ReactivateDelegationEdgesForDelegate(w.ctx, store.DelegationPrincipalAgent, a.ID, store.EdgeDeactivationAgentSoftDelete, "soft-1")
	require.NoError(t, err)
	assert.Equal(t, 1, n)
	assert.Equal(t, adopted.ID, w.activeEdge(a.ID).ID, "the restore reactivates the adopted edge")
	o := w.edge(original.ID)
	assert.False(t, o.Active)
	assert.Equal(t, store.EdgeDeactivationProvenanceAdopted, o.Cause, "the restore leaves the adoption's original alone")

	// After the restore the record is revertible again.
	again := w.buildRevert(rec.ID)
	require.Equal(t, delegationadoption.RevertOutcomeRevert, again.Hops[0].Outcome)
	assert.Equal(t, store.DelegationAdoptionReverted, w.applyRevert(again.Hops[0]).Status)
	assert.Equal(t, original.ID, w.activeEdge(a.ID).ID)
}

// A restore after a revert reactivates nothing the revert deactivated: the
// reverted edge carries adoption_reverted, not the soft-delete cause.
func TestAdoptionRevertedEdgeIsNotRestored(t *testing.T) {
	w, a, original, adopted := adoptOne(t)
	plan := w.buildRevert(w.record(a.ID).ID)
	require.Equal(t, store.DelegationAdoptionReverted, w.applyRevert(plan.Hops[0]).Status)
	assert.Equal(t, store.EdgeDeactivationAdoptionReverted, w.edge(adopted.ID).Cause)

	assert.Equal(t, 1, w.lifecycleDeactivate(a.ID, store.EdgeDeactivationAgentSoftDelete, "soft-2"))
	got, err := w.cs.GetDeactivatedDelegationEdgesForDelegate(w.ctx, store.DelegationPrincipalAgent, a.ID, store.EdgeDeactivationAgentSoftDelete, "soft-2")
	require.NoError(t, err)
	require.Len(t, got, 1)
	assert.Equal(t, original.ID, got[0].ID, "the restore set holds the reactivated original only")
	n, err := w.cs.ReactivateDelegationEdgesForDelegate(w.ctx, store.DelegationPrincipalAgent, a.ID, store.EdgeDeactivationAgentSoftDelete, "soft-2")
	require.NoError(t, err)
	assert.Equal(t, 1, n)
	assert.Equal(t, original.ID, w.activeEdge(a.ID).ID)
	assert.False(t, w.edge(adopted.ID).Active)
}

// A reincarnation by another principal replaces the adopted edge with a
// recorded edge; the revert is refused and writes nothing.
func TestAdoptionRevertRefusedAfterReincarnateReplaced(t *testing.T) {
	w, a, original, adopted := adoptOne(t)
	rec := w.record(a.ID)
	stale := w.buildRevert(rec.ID)

	assert.Equal(t, 1, w.lifecycleDeactivate(a.ID, store.EdgeDeactivationReincarnateReplaced, "reinc-1"))
	replacement := *adopted
	replacement.ID = ""
	replacement.Active = true
	replacement.Deactivation = store.Deactivation{}
	replacement.AuthorityProvenance = store.AuthorityProvenance{
		ProvenanceVersion: store.ProvenanceVersionV1, SourcePrincipalKind: adopted.DelegatorType,
		SourcePrincipalID: adopted.DelegatorID, SourceCredentialKind: store.SourceCredentialSession,
	}
	replacement.EffectCeiling = store.EffectCeiling{Kind: store.EffectCeilingPrincipal}
	require.NoError(t, w.cs.CreateDelegationEdge(w.ctx, &replacement))

	refused := w.buildRevert(rec.ID)
	assert.Equal(t, delegationadoption.RevertOutcomeRefused, refused.Hops[0].Outcome)
	assert.Equal(t, delegationadoption.ReasonAdoptedEdgeChanged, refused.Hops[0].Reason)
	assert.Equal(t, delegationadoption.ReasonAdoptedEdgeChanged, w.applyRevert(stale.Hops[0]).Reason)
	assert.Equal(t, replacement.ID, w.activeEdge(a.ID).ID)
	assert.False(t, w.edge(original.ID).Active)
	assert.Equal(t, store.EdgeDeactivationReincarnateReplaced, w.edge(adopted.ID).Cause)
}
