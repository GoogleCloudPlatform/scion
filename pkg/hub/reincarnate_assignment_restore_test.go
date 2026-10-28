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
	"errors"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// A failed reincarnation that restores the previous configuration restores,
// in the same transaction, the service-account assignment its claim
// replaced (restoreReincarnationAssignment). These tests cover each failure
// path, the paths that keep the new configuration, the coupling with the
// agent write, and a soft delete during the reincarnation. The pending
// sweep and the superseded case are in sa_assignment_replaced_sweep_test.go.

var errRestoreProbeRollback = errors.New("probe rollback")

// countDeactivatedAssignments counts the agent's inactive assignment rows
// deactivated with cause under opID (only the row rowID when set). It
// selects them with a re-mark in a transaction that is rolled back, so
// nothing changes.
func countDeactivatedAssignments(t *testing.T, s store.Store, agentID, rowID string, cause store.EdgeDeactivationCause, opID string) int {
	t.Helper()
	ctx := context.Background()
	n := 0
	err := s.WithTx(ctx, func(tx store.Store) error {
		m, err := tx.RemarkDeactivatedAgentServiceAccountAssignments(ctx, agentID, rowID, cause, opID,
			store.Deactivation{Cause: store.EdgeDeactivationSACleared, OpID: "count-probe"})
		if err != nil {
			return err
		}
		n = m
		return errRestoreProbeRollback
	})
	require.ErrorIs(t, err, errRestoreProbeRollback)
	return n
}

// advance moves the fixture's reincarnation record through states, in
// order, as the worker does.
func (f *replacedAssignmentFixture) advance(t *testing.T, states ...string) {
	t.Helper()
	from := store.AgentReincarnationStatePending
	for _, to := range states {
		_, ok, err := f.srv.tryAdvanceReincarnation(context.Background(), f.rec.ID, from, to, reincarnationStepMaxAttempts, nil)
		require.NoError(t, err)
		require.True(t, ok, "advance %s -> %s", from, to)
		from = to
	}
}

// writeFresh writes the next generation's configuration, naming next, onto
// the agent with reincarnation state state, as the worker's provisioning
// step does.
func (f *replacedAssignmentFixture) writeFresh(t *testing.T, state string) {
	t.Helper()
	fresh := *f.rec.PreviousAppliedConfig
	fresh.GCPIdentity = &store.GCPIdentityConfig{MetadataMode: store.GCPMetadataModeAssign, ServiceAccountID: f.next.ID}
	_, err := f.srv.updateReincarnationStep(context.Background(), f.agent.ID, reincarnationStepUpdate{
		reincarnationState: state,
		appliedConfig:      &fresh,
	}, reincarnationStepMaxAttempts)
	require.NoError(t, err)
	require.Equal(t, f.next.ID, agentAssignedServiceAccountID(mustGetAgent(t, f.s, f.agent.ID)))
}

// assertRestored checks the failed agent row, the restored prior row, the
// replacement's deactivation record and the restore audit record.
func (f *replacedAssignmentFixture) assertRestored(t *testing.T, cause store.EdgeDeactivationCause) {
	t.Helper()
	agent := mustGetAgent(t, f.s, f.agent.ID)
	assert.Equal(t, store.ReincarnationStateFailed, agent.ReincarnationState)
	assert.Equal(t, f.sa.ID, agentAssignedServiceAccountID(agent), "the previous configuration is restored")
	rows := activeAssignments(t, f.s, f.agent.ID)
	require.Len(t, rows, 1, "exactly one active row")
	assert.Equal(t, f.previous.ID, rows[0].ID, "the replaced row is active again")
	assert.Equal(t, f.previous.AuthorityProvenance, rows[0].AuthorityProvenance)
	assert.Equal(t, 1, countDeactivatedAssignments(t, f.s, f.agent.ID, f.replaced.ID, cause, f.rec.ID))
	sum := auditSummary(t, f.s, mutationTypeAgentReincarnateAssignmentRestore, f.agent.ID)
	assert.Equal(t, f.rec.ID, sum["reincarnation_id"])
	assert.Equal(t, string(cause), sum["cause"])
	assert.EqualValues(t, 1, sum["deactivated"])
	assert.EqualValues(t, 1, sum["reactivated"])
	f.assertPriorAssignmentAuthorizes(t)
}

// assertKept checks that the failure left the next generation's
// configuration and the replacement in force, with no restore record.
func (f *replacedAssignmentFixture) assertKept(t *testing.T) {
	t.Helper()
	agent := mustGetAgent(t, f.s, f.agent.ID)
	assert.Equal(t, store.ReincarnationStateFailed, agent.ReincarnationState)
	assert.Equal(t, f.next.ID, agentAssignedServiceAccountID(agent), "the configuration on disk stays")
	rows := activeAssignments(t, f.s, f.agent.ID)
	require.Len(t, rows, 1, "exactly one active row")
	assert.Equal(t, f.replaced.ID, rows[0].ID, "the replacement stays active")
	assert.Empty(t, agentAudits(t, f.s, mutationTypeAgentReincarnateAssignmentRestore, f.agent.ID))
	assertParentCeilingAllowed(t, f.evalFor(f.next.ID))
	assertParentCeilingDenied(t, f.evalFor(f.sa.ID), DenyCauseCeilingProvenanceStale)
}

// TestReincarnateFailureBeforeReprovisionRestoresAssignment: a worker
// failure before the reprovision succeeded restores the previous
// configuration and, with it, the replaced assignment (reincarnate_failed).
func TestReincarnateFailureBeforeReprovisionRestoresAssignment(t *testing.T) {
	f := newClaimedAssignmentFixture(t, true)
	f.advance(t, store.AgentReincarnationStateStopping, store.AgentReincarnationStateProvisioning)
	f.writeFresh(t, store.ReincarnationStateProvisioning)

	f.srv.failReincarnation(context.Background(), f.agent.ID, f.rec.ID, store.AgentReincarnationStateProvisioning,
		"reprovision failed", f.rec.PreviousAppliedConfig)
	f.assertRestored(t, store.EdgeDeactivationReincarnateFailed)
}

// TestReincarnateFailureWithoutRestoreKeepsAssignment: a failure that keeps
// the next generation's configuration (the record was in starting) keeps
// the replacement, which matches it.
func TestReincarnateFailureWithoutRestoreKeepsAssignment(t *testing.T) {
	f := newClaimedAssignmentFixture(t, true)
	f.advance(t, store.AgentReincarnationStateStopping, store.AgentReincarnationStateProvisioning, store.AgentReincarnationStateStarting)
	f.writeFresh(t, store.ReincarnationStateStarting)

	f.srv.failReincarnation(context.Background(), f.agent.ID, f.rec.ID, store.AgentReincarnationStateStarting, "start failed", nil)
	f.assertKept(t)
}

// TestReincarnateFailAfterReprovisionRestoresOnlyAfterRerender: after a
// successful reprovision, the previous configuration (and assignment) is
// restored only when the re-render of the previous configuration succeeds.
func TestReincarnateFailAfterReprovisionRestoresOnlyAfterRerender(t *testing.T) {
	t.Run("re-render succeeds", func(t *testing.T) {
		f := newClaimedAssignmentFixture(t, true)
		f.advance(t, store.AgentReincarnationStateStopping, store.AgentReincarnationStateProvisioning, store.AgentReincarnationStateStarting)
		f.writeFresh(t, store.ReincarnationStateStarting)
		f.srv.failAfterReprovision(context.Background(), newReincarnateTestDispatcher(), f.agent.ID, f.rec.ID,
			store.AgentReincarnationStateStarting, "start failed", f.rec.PreviousAppliedConfig)
		f.assertRestored(t, store.EdgeDeactivationReincarnateFailed)
	})
	t.Run("re-render fails", func(t *testing.T) {
		f := newClaimedAssignmentFixture(t, true)
		f.advance(t, store.AgentReincarnationStateStopping, store.AgentReincarnationStateProvisioning, store.AgentReincarnationStateStarting)
		f.writeFresh(t, store.ReincarnationStateStarting)
		disp := newReincarnateTestDispatcher()
		disp.reprovisionErr = errSPCInjected
		f.srv.failAfterReprovision(context.Background(), disp, f.agent.ID, f.rec.ID,
			store.AgentReincarnationStateStarting, "start failed", f.rec.PreviousAppliedConfig)
		f.assertKept(t)
	})
}

// TestReincarnateStaleSweepRestoresAssignment: the 30-minute sweep restores
// the previous configuration and assignment for a record it finds before
// starting (reincarnate_stale_swept), and keeps both for one in starting.
func TestReincarnateStaleSweepRestoresAssignment(t *testing.T) {
	sweep := func(t *testing.T, f *replacedAssignmentFixture) {
		t.Helper()
		n, err := f.srv.sweepStaleReincarnationsOlderThan(context.Background(), time.Now().Add(time.Hour))
		require.NoError(t, err)
		require.Equal(t, 1, n)
	}
	t.Run("provisioning", func(t *testing.T) {
		f := newClaimedAssignmentFixture(t, true)
		f.advance(t, store.AgentReincarnationStateStopping, store.AgentReincarnationStateProvisioning)
		f.writeFresh(t, store.ReincarnationStateProvisioning)
		sweep(t, f)
		f.assertRestored(t, store.EdgeDeactivationReincarnateStaleSwept)
	})
	t.Run("starting", func(t *testing.T) {
		f := newClaimedAssignmentFixture(t, true)
		f.advance(t, store.AgentReincarnationStateStopping, store.AgentReincarnationStateProvisioning, store.AgentReincarnationStateStarting)
		f.writeFresh(t, store.ReincarnationStateStarting)
		sweep(t, f)
		f.assertKept(t)
	})
}

// TestReincarnateRecordOnlySweepLeavesAssignment: a stale record whose
// agent side another writer already resolved is failed without touching the
// agent, so its assignment is not touched either.
func TestReincarnateRecordOnlySweepLeavesAssignment(t *testing.T) {
	f := newClaimedAssignmentFixture(t, true)
	_, err := f.srv.updateReincarnationStep(context.Background(), f.agent.ID, reincarnationStepUpdate{
		reincarnationState: store.ReincarnationStateFailed,
	}, reincarnationStepMaxAttempts)
	require.NoError(t, err)

	now := time.Now()
	n, err := f.srv.sweepStaleReincarnationsWithCutoffs(context.Background(), now.Add(-reincarnationStaleAfter), now.Add(time.Hour))
	require.NoError(t, err)
	require.Equal(t, 1, n)
	got, err := f.s.GetAgentReincarnation(context.Background(), f.rec.ID)
	require.NoError(t, err)
	require.Equal(t, store.AgentReincarnationStateFailed, got.State)

	rows := activeAssignments(t, f.s, f.agent.ID)
	require.Len(t, rows, 1)
	assert.Equal(t, f.replaced.ID, rows[0].ID, "the replacement is not touched")
	assert.Empty(t, agentAudits(t, f.s, mutationTypeAgentReincarnateAssignmentRestore, f.agent.ID))
}

// TestReincarnateMoveRollbackRestoresAssignment: a move that fails after the
// agent was assigned to the target rolls back to the source and restores
// the previous configuration and assignment.
func TestReincarnateMoveRollbackRestoresAssignment(t *testing.T) {
	f := newClaimedAssignmentFixture(t, true)
	src := mustGetAgent(t, f.s, f.agent.ID)
	f.advance(t, store.AgentReincarnationStateStopping, store.AgentReincarnationStateProvisioning)
	f.writeFresh(t, store.ReincarnationStateProvisioning)

	move := &reincarnationMove{SourceBrokerID: src.RuntimeBrokerID, TargetBrokerID: "restore-move-target", ProjectID: src.ProjectID}
	f.srv.failMoveAndRollBack(context.Background(), nil, f.agent.ID, f.rec.ID, store.AgentReincarnationStateProvisioning,
		"provision on the target broker failed", f.rec.PreviousAppliedConfig, move, src, true)
	f.assertRestored(t, store.EdgeDeactivationReincarnateFailed)
	assert.Equal(t, src.RuntimeBrokerID, mustGetAgent(t, f.s, f.agent.ID).RuntimeBrokerID, "back on the source broker")
}

// TestReincarnateFailureWithoutPriorAssignmentLeavesNone: when the agent
// had no assignment before the claim, the failure deactivates the
// replacement and none is active, as for the restored configuration's
// history before the claim.
func TestReincarnateFailureWithoutPriorAssignmentLeavesNone(t *testing.T) {
	f := newClaimedAssignmentFixture(t, false)
	f.srv.failReincarnation(context.Background(), f.agent.ID, f.rec.ID, store.AgentReincarnationStatePending,
		"no dispatcher available", f.rec.PreviousAppliedConfig)

	assert.Equal(t, store.ReincarnationStateFailed, mustGetAgent(t, f.s, f.agent.ID).ReincarnationState)
	assert.Empty(t, activeAssignments(t, f.s, f.agent.ID), "no assignment is active")
	assert.Equal(t, 1, countDeactivatedAssignments(t, f.s, f.agent.ID, f.replaced.ID,
		store.EdgeDeactivationReincarnateFailed, f.rec.ID))
	sum := auditSummary(t, f.s, mutationTypeAgentReincarnateAssignmentRestore, f.agent.ID)
	assert.EqualValues(t, 1, sum["deactivated"])
	assert.EqualValues(t, 0, sum["reactivated"])
	assertParentCeilingDenied(t, f.evalFor(f.next.ID), DenyCauseCeilingProvenanceMissing)
}

// TestReincarnateSameAccountClaimIsNotRestored: a claim that records an
// assignment for the agent's current account replaces the row as any other
// write does (sa_replaced, a fresh row ID), and a failure that restores the
// previous configuration leaves that row in force.
func TestReincarnateSameAccountClaimIsNotRestored(t *testing.T) {
	f := newSPCFixture(t)
	previous := f.record(t, nil)
	a := mustGetAgent(t, f.s, f.agent.ID)
	now := time.Now()
	a.ReincarnationUpdatedAt = &now
	rec := &store.AgentReincarnation{
		AgentID: a.ID, FromGeneration: a.Generation, ToGeneration: a.Generation + 1,
		State: store.AgentReincarnationStatePending, PreviousAppliedConfig: a.AppliedConfig,
	}
	same := &store.AgentServiceAccountAssignment{
		ServiceAccountID:    f.sa.ID,
		Origin:              store.SAAssignmentOriginReincarnate,
		AuthorityProvenance: recordedProv(store.DelegationPrincipalUser, f.assigner, store.SourceCredentialSession),
		EffectCeiling:       store.EffectCeiling{Kind: store.EffectCeilingPrincipal},
	}
	require.NoError(t, f.srv.reincarnateClaimTx(context.Background(), a, rec, nil, same, AuditActor{}))
	assert.NotEqual(t, rec.ID, same.ID, "not marked restorable")
	assert.NotContains(t, auditSummary(t, f.s, mutationTypeAgentReincarnateClaim, f.agent.ID), "service_account_restorable")
	assert.Zero(t, countDeactivatedAssignments(t, f.s, f.agent.ID, previous.ID, store.EdgeDeactivationReincarnateReplaced, rec.ID))

	f.srv.failReincarnation(context.Background(), f.agent.ID, rec.ID, store.AgentReincarnationStatePending,
		"no dispatcher available", rec.PreviousAppliedConfig)
	assert.Equal(t, store.ReincarnationStateFailed, mustGetAgent(t, f.s, f.agent.ID).ReincarnationState,
		"the failure write lands")
	rows := activeAssignments(t, f.s, f.agent.ID)
	require.Len(t, rows, 1)
	assert.Equal(t, same.ID, rows[0].ID, "the same-account row stays")
	assert.Empty(t, agentAudits(t, f.s, mutationTypeAgentReincarnateAssignmentRestore, f.agent.ID))
	assertParentCeilingAllowed(t, f.eval(t))
}

// restoreStepFaultStore fails the reactivation of the replaced assignment,
// inside transactions too, while fail is set. While moved is above zero, a
// deactivation by row ID selects nothing and decrements it, as when another
// write ended the row first; deactivations counts those calls.
type restoreStepFaultStore struct {
	store.Store
	fail          *atomic.Bool
	moved         *atomic.Int32
	deactivations *atomic.Int32
}

func (f *restoreStepFaultStore) WithTx(ctx context.Context, fn func(tx store.Store) error) error {
	return f.Store.WithTx(ctx, func(tx store.Store) error {
		return fn(&restoreStepFaultStore{Store: tx, fail: f.fail, moved: f.moved, deactivations: f.deactivations})
	})
}

func (f *restoreStepFaultStore) DeactivateAgentServiceAccountAssignment(ctx context.Context, agentID, rowID string, d store.Deactivation) (int, error) {
	f.deactivations.Add(1)
	if f.moved.Load() > 0 {
		f.moved.Add(-1)
		return 0, nil
	}
	return f.Store.DeactivateAgentServiceAccountAssignment(ctx, agentID, rowID, d)
}

// newRestoreStepFaultFixture is a claimed agent, with a previous
// assignment, on a server whose store is a restoreStepFaultStore, advanced
// to provisioning with the next generation's configuration written.
func newRestoreStepFaultFixture(t *testing.T) (*replacedAssignmentFixture, *restoreStepFaultStore) {
	t.Helper()
	srv, s := testServer(t)
	wrapped, _ := installStoreFault(t, srv, func(inner store.Store, _ *storeFaultSwitch) *restoreStepFaultStore {
		return &restoreStepFaultStore{Store: inner, fail: &atomic.Bool{}, moved: &atomic.Int32{}, deactivations: &atomic.Int32{}}
	})
	f := newClaimedAssignmentFixtureOn(t, newSPCFixtureOn(t, srv, s), true)
	f.advance(t, store.AgentReincarnationStateStopping, store.AgentReincarnationStateProvisioning)
	f.writeFresh(t, store.ReincarnationStateProvisioning)
	return f, wrapped
}

func (f *restoreStepFaultStore) ReactivateAgentServiceAccountAssignments(ctx context.Context, agentID string, cause store.EdgeDeactivationCause, opID string) (int, error) {
	if f.fail.Load() {
		return 0, errSPCInjected
	}
	return f.Store.ReactivateAgentServiceAccountAssignments(ctx, agentID, cause, opID)
}

// TestReincarnateAssignmentRestoreCommitsWithAgentWrite: the assignment
// restore and the agent write that restores the previous configuration
// commit together. A failing restore step rolls the agent write back,
// including the replacement's deactivation; a later write lands both.
func TestReincarnateAssignmentRestoreCommitsWithAgentWrite(t *testing.T) {
	f, wrapped := newRestoreStepFaultFixture(t)

	wrapped.fail.Store(true)
	f.srv.failReincarnation(context.Background(), f.agent.ID, f.rec.ID, store.AgentReincarnationStateProvisioning,
		"reprovision failed", f.rec.PreviousAppliedConfig)
	got, err := f.s.GetAgentReincarnation(context.Background(), f.rec.ID)
	require.NoError(t, err)
	require.Equal(t, store.AgentReincarnationStateFailed, got.State, "the record side is written first")
	agent := mustGetAgent(t, f.s, f.agent.ID)
	assert.Equal(t, store.ReincarnationStateProvisioning, agent.ReincarnationState, "the agent write rolled back")
	assert.Equal(t, f.next.ID, agentAssignedServiceAccountID(agent), "the configuration is not restored")
	rows := activeAssignments(t, f.s, f.agent.ID)
	require.Len(t, rows, 1)
	assert.Equal(t, f.replaced.ID, rows[0].ID, "the replacement's deactivation rolled back")
	assert.Empty(t, agentAudits(t, f.s, mutationTypeAgentReincarnateAssignmentRestore, f.agent.ID))

	wrapped.fail.Store(false)
	f.srv.writeFailedAgent(context.Background(), f.agent.ID, f.rec.ID, store.EdgeDeactivationReincarnateFailed,
		"reprovision failed", f.rec.PreviousAppliedConfig, time.Now())
	f.assertRestored(t, store.EdgeDeactivationReincarnateFailed)
}

// TestReincarnateAssignmentRestoreRetriesWhenReplacementChanges: the
// replacement is deactivated by row ID, and when that selects nothing (a
// write ended the row after the restore read it) the failure write rolls
// back and runs again on a fresh read.
func TestReincarnateAssignmentRestoreRetriesWhenReplacementChanges(t *testing.T) {
	t.Run("once", func(t *testing.T) {
		f, wrapped := newRestoreStepFaultFixture(t)
		wrapped.moved.Store(1)
		f.srv.failReincarnation(context.Background(), f.agent.ID, f.rec.ID, store.AgentReincarnationStateProvisioning,
			"reprovision failed", f.rec.PreviousAppliedConfig)
		assert.EqualValues(t, 2, wrapped.deactivations.Load(), "the failure write ran again")
		f.assertRestored(t, store.EdgeDeactivationReincarnateFailed)
	})
	t.Run("every attempt", func(t *testing.T) {
		f, wrapped := newRestoreStepFaultFixture(t)
		wrapped.moved.Store(1000)
		f.srv.failReincarnation(context.Background(), f.agent.ID, f.rec.ID, store.AgentReincarnationStateProvisioning,
			"reprovision failed", f.rec.PreviousAppliedConfig)
		assert.EqualValues(t, reincarnationStepMaxAttempts+3, wrapped.deactivations.Load(), "each attempt retried")
		agent := mustGetAgent(t, f.s, f.agent.ID)
		assert.Equal(t, store.ReincarnationStateProvisioning, agent.ReincarnationState, "nothing is written")
		assert.Equal(t, f.next.ID, agentAssignedServiceAccountID(agent))
		rows := activeAssignments(t, f.s, f.agent.ID)
		require.Len(t, rows, 1)
		assert.Equal(t, f.replaced.ID, rows[0].ID)
		assert.Empty(t, agentAudits(t, f.s, mutationTypeAgentReincarnateAssignmentRestore, f.agent.ID))
	})
}

// TestReincarnateSoftDeleteDuringReincarnationRestoresPriorOnRestore: a soft
// delete while the reincarnation is pending deactivates the replacement.
// The sweep that then fails the reincarnation re-marks the rows, leaving
// none active, so a restore of the agent returns the replaced row.
func TestReincarnateSoftDeleteDuringReincarnationRestoresPriorOnRestore(t *testing.T) {
	f := newClaimedAssignmentFixture(t, true)
	f.srv.SetDispatcher(&engineStubDispatcher{})
	f.srv.config.SoftDeleteRetention = time.Hour

	softDeleteForTest(t, f.srv, f.agent.ID)
	deleted := mustGetAgent(t, f.s, f.agent.ID)
	require.False(t, deleted.DeletedAt.IsZero())
	require.NotEmpty(t, deleted.SoftDeleteOpID)
	assert.Empty(t, activeAssignments(t, f.s, f.agent.ID))
	require.Equal(t, 1, countDeactivatedAssignments(t, f.s, f.agent.ID, f.replaced.ID,
		store.EdgeDeactivationAgentSoftDelete, deleted.SoftDeleteOpID), "the soft delete deactivated the replacement")

	f.sweepPending(t)
	assert.Empty(t, activeAssignments(t, f.s, f.agent.ID), "nothing becomes active")
	assert.Equal(t, 1, countDeactivatedAssignments(t, f.s, f.agent.ID, f.replaced.ID,
		store.EdgeDeactivationReincarnatePendingSwept, f.rec.ID), "the replacement names the sweep")
	assert.Equal(t, 1, countDeactivatedAssignments(t, f.s, f.agent.ID, f.previous.ID,
		store.EdgeDeactivationAgentSoftDelete, deleted.SoftDeleteOpID), "the replaced row returns with the agent")
	sum := auditSummary(t, f.s, mutationTypeAgentReincarnateAssignmentRestore, f.agent.ID)
	assert.EqualValues(t, 2, sum["remarked"])
	assert.EqualValues(t, 0, sum["reactivated"])

	rec := restoreForTest(t, f.srv, f.agent.ID)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	rows := activeAssignments(t, f.s, f.agent.ID)
	require.Len(t, rows, 1, "exactly one active row")
	assert.Equal(t, f.previous.ID, rows[0].ID)
	f.assertPriorAssignmentAuthorizes(t)
}
