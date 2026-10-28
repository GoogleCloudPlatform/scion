// Copyright 2026 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     https://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

//go:build !no_sqlite

package hub

// P6 (#2404 remainder): RecoveryService writes one transaction and one
// history row per affected constraint, and stops at the first failure with
// earlier constraints left committed.

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ---------------------------------------------------------------------------
// T5: recovery writes one transaction and one row per affected constraint
// ---------------------------------------------------------------------------

// p6RecoveryFaults is shared by every transaction-scoped p6RecoveryStore of
// one test, so the test can observe call order across transactions.
type p6RecoveryFaults struct {
	mode      string // "mutation" or "history"; empty = disarmed
	failOn    int    // 1-based attempt number that fails
	err       error
	txCount   int
	mutations []string // constraint ID per mutation attempt, in order
	appends   []string // constraint ID per history append attempt, in order
	// afterList, when set, runs once after the first ListAccessConstraints
	// call: it simulates a change made after recovery took its list.
	afterList func()
}

type p6RecoveryStore struct {
	store.Store
	faults *p6RecoveryFaults
}

func (s *p6RecoveryStore) WithTx(ctx context.Context, fn func(tx store.Store) error) error {
	s.faults.txCount++
	return s.Store.WithTx(ctx, func(tx store.Store) error {
		return fn(&p6RecoveryStore{Store: tx, faults: s.faults})
	})
}

func (s *p6RecoveryStore) ListAccessConstraints(ctx context.Context, limit, offset int) ([]*store.AccessConstraint, error) {
	page, err := s.Store.ListAccessConstraints(ctx, limit, offset)
	if hook := s.faults.afterList; hook != nil {
		s.faults.afterList = nil
		hook()
	}
	return page, err
}

func (s *p6RecoveryStore) mutationFault(id string) error {
	s.faults.mutations = append(s.faults.mutations, id)
	if s.faults.mode == "mutation" && len(s.faults.mutations) == s.faults.failOn {
		return s.faults.err
	}
	return nil
}

func (s *p6RecoveryStore) DisableAccessConstraint(ctx context.Context, id string) error {
	if err := s.mutationFault(id); err != nil {
		return err
	}
	return s.Store.DisableAccessConstraint(ctx, id)
}

func (s *p6RecoveryStore) UpdateAccessConstraint(ctx context.Context, c *store.AccessConstraint, expectedRevision int64) (*store.AccessConstraint, error) {
	if err := s.mutationFault(c.ID); err != nil {
		return nil, err
	}
	return s.Store.UpdateAccessConstraint(ctx, c, expectedRevision)
}

func (s *p6RecoveryStore) AppendConstraintHistoryTx(ctx context.Context, entry *store.AccessConstraintHistory) error {
	s.faults.appends = append(s.faults.appends, entry.ConstraintID)
	if s.faults.mode == "history" && len(s.faults.appends) == s.faults.failOn {
		return s.faults.err
	}
	return s.Store.AppendConstraintHistoryTx(ctx, entry)
}

func p6SeedRecoveryConstraints(t *testing.T, s store.Store, prefix string, n int, disabled bool) []*store.AccessConstraint {
	t.Helper()
	var constraints []*store.AccessConstraint
	for i := 0; i < n; i++ {
		name := fmt.Sprintf("%s-%d", prefix, i)
		targetID := pvSeedUser(t, s, name+"-target")
		created, err := s.CreateAccessConstraint(context.Background(), &store.AccessConstraint{
			Name:                 name,
			SubjectKind:          store.ConstraintSubjectPrincipal,
			SubjectPrincipalType: pvStrPtr("user"),
			SubjectPrincipalID:   &targetID,
			ScopeType:            store.RoleScopeSystem,
			MaximumPermissions:   []string{"agent.read"},
			Purpose:              "p6 recovery " + name,
			CreatedBy:            "p6-recovery-seed",
		})
		require.NoError(t, err)
		if disabled {
			require.NoError(t, s.DisableAccessConstraint(context.Background(), created.ID))
			created, err = s.GetAccessConstraint(context.Background(), created.ID)
			require.NoError(t, err)
		}
		constraints = append(constraints, created)
	}
	return constraints
}

func p6Snapshot(t *testing.T, s store.Store, ids []string) map[string]*store.AccessConstraint {
	t.Helper()
	snapshot := make(map[string]*store.AccessConstraint, len(ids))
	for _, id := range ids {
		c, err := s.GetAccessConstraint(context.Background(), id)
		require.NoError(t, err)
		snapshot[id] = c
	}
	return snapshot
}

func p6IDs(constraints []*store.AccessConstraint) []string {
	ids := make([]string, len(constraints))
	for i, c := range constraints {
		ids[i] = c.ID
	}
	return ids
}

// p6AssertRecoveryRows checks the C4 properties for one recovery call:
// AuditIDs[i] is the event ID of the single row for ids[i]; the row has the
// operation, classification, opaque actor, and the revisions read from the
// store before and after; all rows share one non-empty correlation, which is
// returned.
func p6AssertRecoveryRows(t *testing.T, s store.Store, operation, classification, actorID string,
	ids, auditIDs []string, before map[string]*store.AccessConstraint) string {
	t.Helper()
	require.Len(t, auditIDs, len(ids), "AuditIDs are index-aligned with the constraint IDs")
	after := p6Snapshot(t, s, ids)
	correlation := ""
	for i, id := range ids {
		rows, err := s.ListConstraintHistory(context.Background(), id)
		require.NoError(t, err)
		var matching []*store.AccessConstraintHistory
		for _, row := range rows {
			if row.Operation == operation {
				matching = append(matching, row)
			}
		}
		require.Len(t, matching, 1, "exactly one %s row for constraint %s", operation, id)
		row := matching[0]
		p6RequireUUID(t, auditIDs[i])
		assert.Equal(t, auditIDs[i], row.EventID, "AuditIDs[%d] is the row of %s", i, id)
		assert.Equal(t, classification, row.Classification)
		assert.Equal(t, "user", row.ActorKind)
		assert.Equal(t, actorID, row.ActorID, "actor is the opaque principal ID")
		require.NotNil(t, row.BeforeRevision)
		require.NotNil(t, row.AfterRevision)
		assert.Equal(t, before[id].Revision, *row.BeforeRevision, "before revision read from the store")
		assert.Equal(t, after[id].Revision, *row.AfterRevision, "after revision read from the store")
		require.NotEmpty(t, row.CorrelationID)
		if correlation == "" {
			correlation = row.CorrelationID
		}
		assert.Equal(t, correlation, row.CorrelationID, "all rows of one call share one correlation")
	}
	return correlation
}

func TestRecoveryHistory_DisableAllOneTransactionPerConstraint(t *testing.T) {
	for _, n := range []int{2, 3} {
		t.Run(fmt.Sprintf("N=%d", n), func(t *testing.T) {
			_, _, _, s := govTestSetup(t)
			actorID := govSeedAdminUser(t, s, fmt.Sprintf("p6-disable-actor-%d", n))
			constraints := p6SeedRecoveryConstraints(t, s, fmt.Sprintf("p6-disable-%d", n), n, false)
			ids := p6IDs(constraints)
			before := p6Snapshot(t, s, ids)
			faults := &p6RecoveryFaults{}
			rs := NewRecoveryService(&p6RecoveryStore{Store: s, faults: faults}, nil, slog.Default())

			result, err := rs.DisableAll(context.Background(), pvTestActor(actorID))
			require.NoError(t, err)
			assert.Equal(t, n, result.DisabledCount)
			assert.ElementsMatch(t, ids, result.DisabledIDs)
			assert.Equal(t, n, faults.txCount, "one transaction per constraint")
			assert.Equal(t, result.DisabledIDs, faults.mutations)
			assert.Equal(t, result.DisabledIDs, faults.appends)
			for _, id := range ids {
				c, getErr := s.GetAccessConstraint(context.Background(), id)
				require.NoError(t, getErr)
				assert.True(t, c.Disabled)
			}
			p6AssertRecoveryRows(t, s, "recovery_disable", ClassificationRelax, actorID, result.DisabledIDs, result.AuditIDs, before)
			// Pin: DisableAccessConstraint does not bump the revision, so the
			// truthful recovery_disable row shows before == after.
			after := p6Snapshot(t, s, ids)
			for _, id := range ids {
				assert.Equal(t, before[id].Revision, after[id].Revision)
			}
		})
	}
}

func TestRecoveryHistory_RecoverAllOneTransactionPerConstraint(t *testing.T) {
	_, _, _, s := govTestSetup(t)
	actorID := govSeedAdminUser(t, s, "p6-recover-actor")
	constraints := p6SeedRecoveryConstraints(t, s, "p6-recover", 3, true)
	ids := p6IDs(constraints)
	before := p6Snapshot(t, s, ids)
	faults := &p6RecoveryFaults{}
	rs := NewRecoveryService(&p6RecoveryStore{Store: s, faults: faults}, nil, slog.Default())

	result, err := rs.RecoverAll(context.Background(), pvTestActor(actorID))
	require.NoError(t, err)
	assert.Equal(t, 3, result.RecoveredCount)
	assert.ElementsMatch(t, ids, result.RecoveredIDs)
	assert.Equal(t, 3, faults.txCount, "one transaction per constraint")
	after := p6Snapshot(t, s, ids)
	for _, id := range ids {
		assert.False(t, after[id].Disabled)
		assert.Equal(t, before[id].Revision+1, after[id].Revision)
	}
	p6AssertRecoveryRows(t, s, "recovery_enable", ClassificationTighten, actorID, result.RecoveredIDs, result.AuditIDs, before)
}

func TestRecoveryHistory_DisableRecoverCycle(t *testing.T) {
	_, _, _, s := govTestSetup(t)
	actorID := govSeedAdminUser(t, s, "p6-cycle-actor")
	constraints := p6SeedRecoveryConstraints(t, s, "p6-cycle", 1, false)
	ids := p6IDs(constraints)
	rs := NewRecoveryService(s, nil, slog.Default())

	beforeDisable := p6Snapshot(t, s, ids)
	disabled, err := rs.DisableAll(context.Background(), pvTestActor(actorID))
	require.NoError(t, err)
	assert.Equal(t, 1, disabled.DisabledCount)
	require.Len(t, disabled.AuditIDs, 1)
	c, err := s.GetAccessConstraint(context.Background(), ids[0])
	require.NoError(t, err)
	assert.True(t, c.Disabled, "constraint should be disabled")
	disableCorrelation := p6AssertRecoveryRows(t, s, "recovery_disable", ClassificationRelax, actorID, disabled.DisabledIDs, disabled.AuditIDs, beforeDisable)

	beforeRecover := p6Snapshot(t, s, ids)
	recovered, err := rs.RecoverAll(context.Background(), pvTestActor(actorID))
	require.NoError(t, err)
	assert.Equal(t, 1, recovered.RecoveredCount)
	require.Len(t, recovered.AuditIDs, 1)
	assert.NotEmpty(t, recovered.AuditIDs[0])
	c, err = s.GetAccessConstraint(context.Background(), ids[0])
	require.NoError(t, err)
	assert.False(t, c.Disabled, "constraint should be re-enabled after recovery")
	recoverCorrelation := p6AssertRecoveryRows(t, s, "recovery_enable", ClassificationTighten, actorID, recovered.RecoveredIDs, recovered.AuditIDs, beforeRecover)
	assert.NotEqual(t, disableCorrelation, recoverCorrelation, "each call is its own audit operation")

	rows, err := s.ListConstraintHistory(context.Background(), ids[0])
	require.NoError(t, err)
	require.Len(t, rows, 2)
	assert.Equal(t, "recovery_enable", rows[0].Operation, "newest first")
	assert.Equal(t, "recovery_disable", rows[1].Operation)
	assert.False(t, rs.recoveryLock.IsHeld())
}

// F3: each per-constraint transaction re-reads its constraint and skips one
// whose state changed after the list was taken: no mutation, no row, no
// count.
func TestRecoveryHistory_SkipsConstraintAlreadyInTargetState(t *testing.T) {
	t.Run("DisableAll", func(t *testing.T) {
		_, _, _, s := govTestSetup(t)
		actorID := govSeedAdminUser(t, s, "p6-skip-disable-actor")
		constraints := p6SeedRecoveryConstraints(t, s, "p6-skip-disable", 3, false)
		ids := p6IDs(constraints)
		before := p6Snapshot(t, s, ids)
		changed := ids[1]
		faults := &p6RecoveryFaults{afterList: func() {
			require.NoError(t, s.DisableAccessConstraint(context.Background(), changed))
		}}
		rs := NewRecoveryService(&p6RecoveryStore{Store: s, faults: faults}, nil, slog.Default())

		result, err := rs.DisableAll(context.Background(), pvTestActor(actorID))
		require.NoError(t, err)
		assert.Equal(t, 2, result.DisabledCount, "the already-disabled constraint is not counted")
		assert.ElementsMatch(t, []string{ids[0], ids[2]}, result.DisabledIDs)
		assert.NotContains(t, faults.mutations, changed, "no mutation for the skipped constraint")
		assert.NotContains(t, faults.appends, changed, "no row for the skipped constraint")
		p6AssertRecoveryRows(t, s, "recovery_disable", ClassificationRelax, actorID, result.DisabledIDs, result.AuditIDs, before)
		rows, err := s.ListConstraintHistory(context.Background(), changed)
		require.NoError(t, err)
		assert.Empty(t, rows)
	})
	t.Run("RecoverAll", func(t *testing.T) {
		_, _, _, s := govTestSetup(t)
		actorID := govSeedAdminUser(t, s, "p6-skip-recover-actor")
		constraints := p6SeedRecoveryConstraints(t, s, "p6-skip-recover", 3, true)
		ids := p6IDs(constraints)
		changed := ids[1]
		faults := &p6RecoveryFaults{afterList: func() {
			current, err := s.GetAccessConstraint(context.Background(), changed)
			require.NoError(t, err)
			current.Disabled = false
			_, err = s.UpdateAccessConstraint(context.Background(), current, current.Revision)
			require.NoError(t, err)
		}}
		rs := NewRecoveryService(&p6RecoveryStore{Store: s, faults: faults}, nil, slog.Default())
		before := p6Snapshot(t, s, ids)

		result, err := rs.RecoverAll(context.Background(), pvTestActor(actorID))
		require.NoError(t, err)
		assert.Equal(t, 2, result.RecoveredCount, "the already-enabled constraint is not counted")
		assert.ElementsMatch(t, []string{ids[0], ids[2]}, result.RecoveredIDs)
		assert.NotContains(t, faults.mutations, changed)
		assert.NotContains(t, faults.appends, changed)
		p6AssertRecoveryRows(t, s, "recovery_enable", ClassificationTighten, actorID, result.RecoveredIDs, result.AuditIDs, before)
		rows, err := s.ListConstraintHistory(context.Background(), changed)
		require.NoError(t, err)
		assert.Empty(t, rows)
	})
}

// O2-1: a constraint deleted after the list was taken is skipped (not found
// inside its transaction): no mutation, no row, not counted, no error.
func TestRecoveryHistory_SkipsConstraintDeletedAfterList(t *testing.T) {
	for _, tc := range []struct {
		name     string
		disabled bool
		run      func(rs *RecoveryService, actor PrincipalContext) ([]string, []string, int, error)
	}{
		{name: "DisableAll", run: func(rs *RecoveryService, actor PrincipalContext) ([]string, []string, int, error) {
			result, err := rs.DisableAll(context.Background(), actor)
			if err != nil {
				return nil, nil, 0, err
			}
			return result.DisabledIDs, result.AuditIDs, result.DisabledCount, nil
		}},
		{name: "RecoverAll", disabled: true, run: func(rs *RecoveryService, actor PrincipalContext) ([]string, []string, int, error) {
			result, err := rs.RecoverAll(context.Background(), actor)
			if err != nil {
				return nil, nil, 0, err
			}
			return result.RecoveredIDs, result.AuditIDs, result.RecoveredCount, nil
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, _, _, s := govTestSetup(t)
			prefix := "p6-skip-deleted-" + strings.ToLower(tc.name)
			actorID := govSeedAdminUser(t, s, prefix+"-actor")
			constraints := p6SeedRecoveryConstraints(t, s, prefix, 3, tc.disabled)
			ids := p6IDs(constraints)
			deleted := ids[1]
			faults := &p6RecoveryFaults{afterList: func() {
				require.NoError(t, s.DeleteAccessConstraint(context.Background(), deleted))
			}}
			rs := NewRecoveryService(&p6RecoveryStore{Store: s, faults: faults}, nil, slog.Default())

			gotIDs, auditIDs, count, err := tc.run(rs, pvTestActor(actorID))
			require.NoError(t, err, "a concurrently deleted constraint does not stop the run")
			assert.Equal(t, 2, count)
			assert.ElementsMatch(t, []string{ids[0], ids[2]}, gotIDs)
			require.Len(t, auditIDs, 2)
			assert.NotContains(t, faults.mutations, deleted)
			assert.NotContains(t, faults.appends, deleted)
			assert.Equal(t, 3, faults.txCount, "the deleted constraint's transaction opened and ended without a change")
			assert.False(t, rs.recoveryLock.IsHeld())
		})
	}
}

func TestRecoveryHistory_FaultOnKthConstraintKeepsPriorCommits(t *testing.T) {
	type operationCase struct {
		name           string
		operation      string
		classification string
		seedDisabled   bool
		run            func(rs *RecoveryService, actor PrincipalContext) ([]string, []string, error)
	}
	operations := []operationCase{
		{
			name: "DisableAll", operation: "recovery_disable", classification: ClassificationRelax,
			run: func(rs *RecoveryService, actor PrincipalContext) ([]string, []string, error) {
				result, err := rs.DisableAll(context.Background(), actor)
				if err != nil {
					return nil, nil, err
				}
				return result.DisabledIDs, result.AuditIDs, nil
			},
		},
		{
			name: "RecoverAll", operation: "recovery_enable", classification: ClassificationTighten, seedDisabled: true,
			run: func(rs *RecoveryService, actor PrincipalContext) ([]string, []string, error) {
				result, err := rs.RecoverAll(context.Background(), actor)
				if err != nil {
					return nil, nil, err
				}
				return result.RecoveredIDs, result.AuditIDs, nil
			},
		},
	}
	for _, op := range operations {
		for _, mode := range []string{"mutation", "history"} {
			for _, shape := range []struct{ n, k int }{{n: 1, k: 1}, {n: 3, k: 2}} {
				name := fmt.Sprintf("%s/%s-fault/k=%d-of-N=%d", op.name, mode, shape.k, shape.n)
				t.Run(name, func(t *testing.T) {
					_, _, _, s := govTestSetup(t)
					prefix := fmt.Sprintf("p6-kth-%s-%s-%d-%d", strings.ToLower(op.name), mode, shape.k, shape.n)
					actorID := govSeedAdminUser(t, s, prefix+"-actor")
					actor := pvTestActor(actorID)
					constraints := p6SeedRecoveryConstraints(t, s, prefix, shape.n, op.seedDisabled)
					allIDs := p6IDs(constraints)
					before := p6Snapshot(t, s, allIDs)
					injected := errors.New("injected " + mode + " fault")
					faults := &p6RecoveryFaults{mode: mode, failOn: shape.k, err: injected}
					rs := NewRecoveryService(&p6RecoveryStore{Store: s, faults: faults}, nil, slog.Default())

					_, _, err := op.run(rs, actor)
					require.Error(t, err)
					require.ErrorIs(t, err, injected)
					var partial *RecoveryPartialError
					require.ErrorAs(t, err, &partial)
					assert.Equal(t, shape.k-1, partial.Committed)
					assert.Equal(t, op.operation, partial.Operation)

					// Order comes from the observed mutation attempts: the
					// k-th attempt is the failed constraint, and processing
					// stopped there.
					require.Len(t, faults.mutations, shape.k, "processing stops at the first failure")
					// Copy: an empty prefix becomes nil (the service's
					// "nothing committed" value), and the copy is detached
					// from faults.mutations before the rerun resets it.
					committedIDs := append([]string(nil), faults.mutations[:shape.k-1]...)
					failedID := faults.mutations[shape.k-1]
					assert.Equal(t, failedID, partial.FailedID)
					assert.Equal(t, committedIDs, partial.CommittedIDs)
					require.Len(t, partial.AuditIDs, shape.k-1)

					// Committed constraints stay committed, each with its row.
					if len(committedIDs) > 0 {
						p6AssertRecoveryRows(t, s, op.operation, op.classification, actorID, committedIDs, partial.AuditIDs, before)
					}
					// The failed constraint and the untouched ones keep their
					// state and have no row.
					committed := map[string]bool{}
					for _, id := range committedIDs {
						committed[id] = true
					}
					var remaining []string
					for _, id := range allIDs {
						if committed[id] {
							continue
						}
						remaining = append(remaining, id)
						p6AssertUnchanged(t, s, before[id], 0)
					}
					require.Len(t, remaining, shape.n-shape.k+1)
					assert.Contains(t, remaining, failedID)
					assert.False(t, rs.recoveryLock.IsHeld(), "the lock is released after a failure")

					// A rerun on the same service processes exactly the
					// remaining constraints, each with its row.
					faults.mode = ""
					faults.mutations = nil
					rerunIDs, rerunAuditIDs, err := op.run(rs, actor)
					require.NoError(t, err)
					assert.ElementsMatch(t, remaining, rerunIDs)
					remainingBefore := make(map[string]*store.AccessConstraint, len(remaining))
					for _, id := range remaining {
						remainingBefore[id] = before[id]
					}
					p6AssertRecoveryRows(t, s, op.operation, op.classification, actorID, rerunIDs, rerunAuditIDs, remainingBefore)
					// Earlier commits still have exactly one row each.
					for _, id := range committedIDs {
						rows, listErr := s.ListConstraintHistory(context.Background(), id)
						require.NoError(t, listErr)
						assert.Len(t, rows, 1)
					}
					assert.False(t, rs.recoveryLock.IsHeld())
				})
			}
		}
	}
}
