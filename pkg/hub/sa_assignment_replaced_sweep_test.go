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

package hub

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// A reincarnation with --service-account replaces the agent's assignment in
// its claim transaction. When the pending sweep (ptone/scion#3986) fails
// that reincarnation because its worker never started, nothing of the patch
// is applied: the agent keeps its previous account while the replaced
// assignment, naming the next account, stays the active row. These tests
// pin what that row can and cannot do: it grants no token, use or restore
// authority for either account, it satisfies no parent-ceiling check, it
// does not block a later assignment, delete or restore, and cleanup removes
// it like any other assignment.

// replacedAssignmentFixture is an spcFixture agent whose assignment for
// f.sa was replaced, by a reincarnation claim, with one for next; the
// pending sweep then failed that reincarnation.
type replacedAssignmentFixture struct {
	*spcFixture
	next     *store.GCPServiceAccount
	previous *store.AgentServiceAccountAssignment
	replaced *store.AgentServiceAccountAssignment
}

func newReplacedAssignmentFixture(t *testing.T) *replacedAssignmentFixture {
	t.Helper()
	ctx := context.Background()
	spc := newSPCFixture(t)
	f := &replacedAssignmentFixture{spcFixture: spc}
	f.previous = spc.record(t, nil)
	assertParentCeilingAllowed(t, spc.eval(t))
	f.next = spcServiceAccount(t, spc.s, "spc-sa-next", store.ScopeProject, spc.projectID, true)

	// The claim, as startReincarnation commits it, with no worker started.
	a := mustGetAgent(t, spc.s, spc.agent.ID)
	claimedAt := time.Now().Truncate(time.Microsecond)
	a.ReincarnationState = store.ReincarnationStatePending
	a.ReincarnationUpdatedAt = &claimedAt
	rec := &store.AgentReincarnation{
		AgentID:               a.ID,
		FromGeneration:        a.Generation,
		ToGeneration:          a.Generation + 1,
		State:                 store.AgentReincarnationStatePending,
		PreviousAppliedConfig: a.AppliedConfig,
	}
	f.replaced = &store.AgentServiceAccountAssignment{
		ServiceAccountID:    f.next.ID,
		Origin:              store.SAAssignmentOriginReincarnate,
		AuthorityProvenance: recordedProv(store.DelegationPrincipalUser, spc.assigner, store.SourceCredentialSession),
		EffectCeiling:       store.EffectCeiling{Kind: store.EffectCeilingPrincipal},
	}
	require.NoError(t, spc.srv.reincarnateClaimTx(ctx, a, rec, nil, f.replaced, AuditActor{}))
	rows := activeAssignments(t, spc.s, spc.agent.ID)
	require.Len(t, rows, 1)
	require.Equal(t, f.replaced.ID, rows[0].ID, "the claim replaced the assignment")

	// The pending sweep fails it (a pending cutoff in the future treats the
	// claim as stale, as for a hub that restarted after the claim).
	now := time.Now()
	n, err := spc.srv.sweepStaleReincarnationsWithCutoffs(ctx, now.Add(-reincarnationStaleAfter), now.Add(time.Hour))
	require.NoError(t, err)
	require.Equal(t, 1, n)
	swept := mustGetAgent(t, spc.s, spc.agent.ID)
	require.Equal(t, store.ReincarnationStateFailed, swept.ReincarnationState)
	require.Equal(t, spc.sa.ID, agentAssignedServiceAccountID(swept), "nothing of the patch is applied")
	got, err := spc.s.GetAgentReincarnation(ctx, rec.ID)
	require.NoError(t, err)
	require.Equal(t, store.AgentReincarnationStateFailed, got.State)
	require.Equal(t, reincarnationDidNotStartReason, got.Error)
	return f
}

func (f *replacedAssignmentFixture) evalFor(saID string) ParentCeilingDecision {
	return f.srv.authzService.EvaluateServiceAccountParentCeiling(context.Background(), f.agent.ID, saID)
}

// assertReplacedRowAuthorizesNothing checks that neither account passes the
// parent ceiling and that the agent's token scopes carry neither account.
func (f *replacedAssignmentFixture) assertReplacedRowAuthorizesNothing(t *testing.T) {
	t.Helper()
	assertParentCeilingDenied(t, f.evalFor(f.sa.ID), DenyCauseCeilingProvenanceStale)
	assertParentCeilingDenied(t, f.evalFor(f.next.ID), DenyCauseCeilingProvenanceStale)
	agent := mustGetAgent(t, f.s, f.agent.ID)
	scopes, err := f.srv.authzService.ceilingFilteredAgentScopes(context.Background(), agent, f.srv.authzService.mintCandidateScopes(agent))
	require.NoError(t, err)
	assert.NotContains(t, scopes, GCPTokenScopeForSA(f.sa.ID), "no token scope for the applied account")
	assert.NotContains(t, scopes, GCPTokenScopeForSA(f.next.ID), "no token scope for the replaced row's account")
}

// TestSAParentCeiling_ReplacedAssignmentAuthorizesNothing: after the sweep,
// the replaced row satisfies neither the applied account's parent ceiling
// (the row names another account) nor the next account's (the agent does
// not use it), so no token scope and no use of either account is allowed.
func TestSAParentCeiling_ReplacedAssignmentAuthorizesNothing(t *testing.T) {
	f := newReplacedAssignmentFixture(t)
	rows := activeAssignments(t, f.s, f.agent.ID)
	require.Len(t, rows, 1, "exactly one active row")
	assert.Equal(t, f.replaced.ID, rows[0].ID)
	assert.Equal(t, f.next.ID, rows[0].ServiceAccountID)
	f.assertReplacedRowAuthorizesNothing(t)
}

// TestSAParentCeiling_ReplacedAssignmentYieldsToLaterAssign: the failed
// reincarnation does not block a later one, and its assignment replaces the
// row as usual; assigning the applied account again restores its use.
func TestSAParentCeiling_ReplacedAssignmentYieldsToLaterAssign(t *testing.T) {
	f := newReplacedAssignmentFixture(t)
	agent := mustGetAgent(t, f.s, f.agent.ID)
	again := &store.AgentServiceAccountAssignment{
		ServiceAccountID:    f.sa.ID,
		Origin:              store.SAAssignmentOriginReincarnate,
		AuthorityProvenance: recordedProv(store.DelegationPrincipalUser, f.assigner, store.SourceCredentialSession),
		EffectCeiling:       store.EffectCeiling{Kind: store.EffectCeilingPrincipal},
	}
	require.NoError(t, reincarnateClaimFor(t, f.srv, agent, again), "a new claim is accepted")
	rows := activeAssignments(t, f.s, f.agent.ID)
	require.Len(t, rows, 1, "exactly one active row")
	assert.Equal(t, again.ID, rows[0].ID)
	assert.NotEqual(t, f.replaced.ID, rows[0].ID, "the replaced row is inactive")
	assertParentCeilingAllowed(t, f.evalFor(f.sa.ID))
	assertParentCeilingDenied(t, f.evalFor(f.next.ID), DenyCauseCeilingProvenanceStale)
}

// TestSAParentCeiling_ReplacedAssignmentRemovedByHardDelete: a hard delete
// deactivates the replaced row like any other assignment, and counts it.
func TestSAParentCeiling_ReplacedAssignmentRemovedByHardDelete(t *testing.T) {
	f := newReplacedAssignmentFixture(t)
	f.srv.SetDispatcher(&engineStubDispatcher{})
	rec := doRequest(t, f.srv, http.MethodDelete, "/api/v1/agents/"+f.agent.ID, nil)
	require.Equal(t, http.StatusNoContent, rec.Code, rec.Body.String())
	assert.True(t, agentGone(t, f.s, f.agent.ID))
	assert.Empty(t, activeAssignments(t, f.s, f.agent.ID), "no active assignment remains")
	assert.EqualValues(t, 1, auditSummary(t, f.s, mutationTypeAgentHardDelete, f.agent.ID)["assignments_deactivated"],
		"the replaced row is the one assignment deactivated")
	assertParentCeilingDenied(t, f.evalFor(f.next.ID), DenyCauseCeilingOrphaned)
}

// TestSAParentCeiling_ReplacedAssignmentRestoreGrantsNothing: a soft delete
// deactivates the replaced row and a restore brings back exactly that row,
// which authorizes neither account.
func TestSAParentCeiling_ReplacedAssignmentRestoreGrantsNothing(t *testing.T) {
	f := newReplacedAssignmentFixture(t)
	f.srv.SetDispatcher(&engineStubDispatcher{})
	f.srv.config.SoftDeleteRetention = time.Hour

	softDeleteForTest(t, f.srv, f.agent.ID)
	assert.Empty(t, activeAssignments(t, f.s, f.agent.ID))
	assert.EqualValues(t, 1, auditSummary(t, f.s, mutationTypeAgentSoftDelete, f.agent.ID)["assignments_deactivated"])

	rec := restoreForTest(t, f.srv, f.agent.ID)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	rows := activeAssignments(t, f.s, f.agent.ID)
	require.Len(t, rows, 1, "exactly the soft-deleted row returns")
	assert.Equal(t, f.replaced.ID, rows[0].ID)
	f.assertReplacedRowAuthorizesNothing(t)
}
