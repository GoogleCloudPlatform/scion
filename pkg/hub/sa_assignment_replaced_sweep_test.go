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

// A reincarnation with --service-account for another account replaces the
// agent's assignment in its claim transaction, restorably: the new row takes
// the reincarnation ID and the replaced row is marked with it. When the
// pending sweep (ptone/scion#3986) fails that reincarnation because its
// worker never started, nothing of the patch is applied: the agent keeps its
// previous account and, in the same transaction, the replaced assignment is
// active again. These tests pin that restore, and what a replacement that a
// later write superseded before the sweep can and cannot do: the sweep
// leaves it as it is, so it grants no use of either account until the
// account is assigned again.

// replacedAssignmentFixture is an spcFixture agent whose assignment for
// f.sa was replaced, by a reincarnation claim, with one for next.
type replacedAssignmentFixture struct {
	*spcFixture
	next     *store.GCPServiceAccount
	previous *store.AgentServiceAccountAssignment // nil when the agent had none
	replaced *store.AgentServiceAccountAssignment
	rec      *store.AgentReincarnation
}

// newClaimedAssignmentFixture claims the agent, as startReincarnation
// commits it, with an assignment for next and no worker started. With
// withPrevious the agent holds an assignment for f.sa before the claim.
func newClaimedAssignmentFixture(t *testing.T, withPrevious bool) *replacedAssignmentFixture {
	t.Helper()
	return newClaimedAssignmentFixtureOn(t, newSPCFixture(t), withPrevious)
}

// newClaimedAssignmentFixtureOn is newClaimedAssignmentFixture on spc.
func newClaimedAssignmentFixtureOn(t *testing.T, spc *spcFixture, withPrevious bool) *replacedAssignmentFixture {
	t.Helper()
	ctx := context.Background()
	f := &replacedAssignmentFixture{spcFixture: spc}
	if withPrevious {
		f.previous = spc.record(t, nil)
		assertParentCeilingAllowed(t, spc.eval(t))
	}
	f.next = spcServiceAccount(t, spc.s, "spc-sa-next", store.ScopeProject, spc.projectID, true)

	a := mustGetAgent(t, spc.s, spc.agent.ID)
	claimedAt := time.Now().Truncate(time.Microsecond)
	a.ReincarnationState = store.ReincarnationStatePending
	a.ReincarnationUpdatedAt = &claimedAt
	f.rec = &store.AgentReincarnation{
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
	require.NoError(t, spc.srv.reincarnateClaimTx(ctx, a, f.rec, nil, f.replaced, AuditActor{}))
	rows := activeAssignments(t, spc.s, spc.agent.ID)
	require.Len(t, rows, 1)
	require.Equal(t, f.replaced.ID, rows[0].ID, "the claim replaced the assignment")
	require.Equal(t, f.rec.ID, f.replaced.ID, "a claim that changes the account takes the reincarnation ID as the row ID")
	if f.previous != nil {
		require.Equal(t, 1, countDeactivatedAssignments(t, spc.s, spc.agent.ID, f.previous.ID,
			store.EdgeDeactivationReincarnateReplaced, f.rec.ID), "the replaced row is marked with the reincarnation ID")
	}
	assert.Equal(t, true, auditSummary(t, spc.s, mutationTypeAgentReincarnateClaim, spc.agent.ID)["service_account_restorable"])
	return f
}

// sweepPending runs the pending sweep (a pending cutoff in the future treats
// the claim as stale, as for a hub that restarted after the claim) and
// checks that it failed the reincarnation without applying the patch.
func (f *replacedAssignmentFixture) sweepPending(t *testing.T) {
	t.Helper()
	ctx := context.Background()
	now := time.Now()
	n, err := f.srv.sweepStaleReincarnationsWithCutoffs(ctx, now.Add(-reincarnationStaleAfter), now.Add(time.Hour))
	require.NoError(t, err)
	require.Equal(t, 1, n)
	swept := mustGetAgent(t, f.s, f.agent.ID)
	require.Equal(t, store.ReincarnationStateFailed, swept.ReincarnationState)
	require.Equal(t, f.sa.ID, agentAssignedServiceAccountID(swept), "nothing of the patch is applied")
	got, err := f.s.GetAgentReincarnation(ctx, f.rec.ID)
	require.NoError(t, err)
	require.Equal(t, store.AgentReincarnationStateFailed, got.State)
	require.Equal(t, reincarnationDidNotStartReason, got.Error)
}

// newReplacedAssignmentFixture is a claimed agent with a previous
// assignment whose reincarnation the pending sweep failed.
func newReplacedAssignmentFixture(t *testing.T) *replacedAssignmentFixture {
	t.Helper()
	f := newClaimedAssignmentFixture(t, true)
	f.sweepPending(t)
	return f
}

// newSupersededAssignmentFixture is newReplacedAssignmentFixture where a
// later write replaced the claim's replacement, with another row for next,
// before the sweep.
func newSupersededAssignmentFixture(t *testing.T) *replacedAssignmentFixture {
	t.Helper()
	f := newClaimedAssignmentFixture(t, true)
	f.replaced = f.record(t, func(a *store.AgentServiceAccountAssignment) {
		a.ServiceAccountID = f.next.ID
		a.Origin = store.SAAssignmentOriginUpdate
	})
	require.NotEqual(t, f.rec.ID, f.replaced.ID)
	f.sweepPending(t)
	return f
}

func (f *replacedAssignmentFixture) evalFor(saID string) ParentCeilingDecision {
	return f.srv.authzService.EvaluateServiceAccountParentCeiling(context.Background(), f.agent.ID, saID)
}

// agentScopes returns the scopes a mint for the agent would issue.
func (f *replacedAssignmentFixture) agentScopes(t *testing.T) []AgentTokenScope {
	t.Helper()
	agent := mustGetAgent(t, f.s, f.agent.ID)
	scopes, err := f.srv.authzService.ceilingFilteredAgentScopes(context.Background(), agent, f.srv.authzService.mintCandidateScopes(agent))
	require.NoError(t, err)
	return scopes
}

// assertPriorAssignmentAuthorizes checks that the applied account passes
// the parent ceiling and is in the agent's mint scopes, and next is
// neither.
func (f *replacedAssignmentFixture) assertPriorAssignmentAuthorizes(t *testing.T) {
	t.Helper()
	assertParentCeilingAllowed(t, f.evalFor(f.sa.ID))
	assertParentCeilingDenied(t, f.evalFor(f.next.ID), DenyCauseCeilingProvenanceStale)
	scopes := f.agentScopes(t)
	assert.Contains(t, scopes, GCPTokenScopeForSA(f.sa.ID), "mint scope for the applied account")
	assert.NotContains(t, scopes, GCPTokenScopeForSA(f.next.ID), "no mint scope for the replaced account")
}

// assertReplacedRowAuthorizesNothing checks that neither account passes the
// parent ceiling and that the agent's token scopes carry neither account.
func (f *replacedAssignmentFixture) assertReplacedRowAuthorizesNothing(t *testing.T) {
	t.Helper()
	assertParentCeilingDenied(t, f.evalFor(f.sa.ID), DenyCauseCeilingProvenanceStale)
	assertParentCeilingDenied(t, f.evalFor(f.next.ID), DenyCauseCeilingProvenanceStale)
	scopes := f.agentScopes(t)
	assert.NotContains(t, scopes, GCPTokenScopeForSA(f.sa.ID), "no token scope for the applied account")
	assert.NotContains(t, scopes, GCPTokenScopeForSA(f.next.ID), "no token scope for the replaced row's account")
}

// TestSAParentCeiling_PendingSweepRestoresPriorAssignment: the sweep that
// restores the previous configuration reactivates the replaced row, with
// its own source, and deactivates the replacement with cause
// reincarnate_pending_swept under the reincarnation ID, in one audited
// transaction. The applied account is usable again; next is not.
func TestSAParentCeiling_PendingSweepRestoresPriorAssignment(t *testing.T) {
	f := newReplacedAssignmentFixture(t)
	rows := activeAssignments(t, f.s, f.agent.ID)
	require.Len(t, rows, 1, "exactly one active row")
	assert.Equal(t, f.previous.ID, rows[0].ID, "the replaced row is active again")
	assert.Equal(t, f.sa.ID, rows[0].ServiceAccountID)
	assert.Equal(t, f.previous.Origin, rows[0].Origin)
	assert.Equal(t, f.previous.AuthorityProvenance, rows[0].AuthorityProvenance, "its own source")
	assert.Equal(t, f.previous.EffectCeiling, rows[0].EffectCeiling, "its own frozen ceiling")
	assert.Equal(t, store.Deactivation{}, rows[0].Deactivation)
	assert.Equal(t, 1, countDeactivatedAssignments(t, f.s, f.agent.ID, f.replaced.ID,
		store.EdgeDeactivationReincarnatePendingSwept, f.rec.ID), "the replacement names the sweep")

	sum := auditSummary(t, f.s, mutationTypeAgentReincarnateAssignmentRestore, f.agent.ID)
	assert.Equal(t, f.rec.ID, sum["reincarnation_id"])
	assert.Equal(t, string(store.EdgeDeactivationReincarnatePendingSwept), sum["cause"])
	assert.EqualValues(t, 1, sum["deactivated"])
	assert.EqualValues(t, 1, sum["reactivated"])

	f.assertPriorAssignmentAuthorizes(t)
}

// TestSAParentCeiling_ReplacedAssignmentYieldsToLaterAssign: the failed
// reincarnation does not block a later one, and its assignment replaces the
// restored row as usual.
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
	assert.NotEqual(t, f.previous.ID, rows[0].ID, "the restored row is replaced")
	assertParentCeilingAllowed(t, f.evalFor(f.sa.ID))
	assertParentCeilingDenied(t, f.evalFor(f.next.ID), DenyCauseCeilingProvenanceStale)
}

// TestSAParentCeiling_ReplacedAssignmentRemovedByHardDelete: a hard delete
// deactivates the restored row like any other assignment, and counts it.
func TestSAParentCeiling_ReplacedAssignmentRemovedByHardDelete(t *testing.T) {
	f := newReplacedAssignmentFixture(t)
	f.srv.SetDispatcher(&engineStubDispatcher{})
	rec := doRequest(t, f.srv, http.MethodDelete, "/api/v1/agents/"+f.agent.ID, nil)
	require.Equal(t, http.StatusNoContent, rec.Code, rec.Body.String())
	assert.True(t, agentGone(t, f.s, f.agent.ID))
	assert.Empty(t, activeAssignments(t, f.s, f.agent.ID), "no active assignment remains")
	sum := auditSummary(t, f.s, mutationTypeAgentHardDelete, f.agent.ID)
	assert.EqualValues(t, 1, sum["assignments_deactivated"], "one assignment is deactivated")
	opID, _ := sum["op_id"].(string)
	require.NotEmpty(t, opID, "the hard delete audit records its operation ID")
	assert.Equal(t, 1, countDeactivatedAssignments(t, f.s, f.agent.ID, f.previous.ID,
		store.EdgeDeactivationAgentHardDelete, opID), "the restored row is the one deactivated")
	assertParentCeilingDenied(t, f.evalFor(f.sa.ID), DenyCauseCeilingOrphaned)
	assertParentCeilingDenied(t, f.evalFor(f.next.ID), DenyCauseCeilingOrphaned)
}

// TestSAParentCeiling_SoftDeleteRestoreReturnsPriorAssignment: a soft delete
// deactivates the restored row and a restore brings back exactly that row,
// which authorizes the applied account.
func TestSAParentCeiling_SoftDeleteRestoreReturnsPriorAssignment(t *testing.T) {
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
	assert.Equal(t, f.previous.ID, rows[0].ID)
	f.assertPriorAssignmentAuthorizes(t)
}

// TestSAParentCeiling_SupersededReplacementNotRestored: when a later write
// replaced the claim's replacement before the sweep, the sweep changes no
// assignment and writes no restore record. The later row names an account
// the restored configuration does not use, so neither account is usable
// until the account is assigned again.
func TestSAParentCeiling_SupersededReplacementNotRestored(t *testing.T) {
	f := newSupersededAssignmentFixture(t)
	rows := activeAssignments(t, f.s, f.agent.ID)
	require.Len(t, rows, 1, "exactly one active row")
	assert.Equal(t, f.replaced.ID, rows[0].ID, "the later row stays active")
	assert.Equal(t, 1, countDeactivatedAssignments(t, f.s, f.agent.ID, f.previous.ID,
		store.EdgeDeactivationReincarnateReplaced, f.rec.ID), "the replaced row stays inactive")
	assert.Empty(t, agentAudits(t, f.s, mutationTypeAgentReincarnateAssignmentRestore, f.agent.ID))
	f.assertReplacedRowAuthorizesNothing(t)

	// Assigning the account again restores its use.
	f.record(t, nil)
	assertParentCeilingAllowed(t, f.evalFor(f.sa.ID))
}

// TestSAParentCeiling_SupersededReplacementRestoreGrantsNothing: a soft
// delete and restore of an agent whose replacement was superseded returns
// exactly the later row, which authorizes neither account.
func TestSAParentCeiling_SupersededReplacementRestoreGrantsNothing(t *testing.T) {
	f := newSupersededAssignmentFixture(t)
	f.srv.SetDispatcher(&engineStubDispatcher{})
	f.srv.config.SoftDeleteRetention = time.Hour

	softDeleteForTest(t, f.srv, f.agent.ID)
	assert.Empty(t, activeAssignments(t, f.s, f.agent.ID))
	rec := restoreForTest(t, f.srv, f.agent.ID)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	rows := activeAssignments(t, f.s, f.agent.ID)
	require.Len(t, rows, 1, "exactly the soft-deleted row returns")
	assert.Equal(t, f.replaced.ID, rows[0].ID)
	f.assertReplacedRowAuthorizesNothing(t)
}
