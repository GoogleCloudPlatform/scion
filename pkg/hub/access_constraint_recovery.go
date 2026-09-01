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

package hub

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// ---------------------------------------------------------------------------
// Offline recovery service (B6)
// ---------------------------------------------------------------------------

// RecoveryService provides offline disable and recovery operations for
// access boundaries. Every operation produces an atomic audit record:
// if the audit write fails, the operation rolls back.
type RecoveryService struct {
	store       store.Store
	auditWriter *BoundaryAuditWriter
	eventBus    *InvalidationEventBus
	logger      *slog.Logger

	// recoveryLock enforces database-level online/offline exclusion.
	// Only one recovery operation can run at a time per Hub instance.
	recoveryLock *RecoveryLock

	// nowFunc is injectable for testing. Defaults to time.Now.
	nowFunc func() time.Time
}

// NewRecoveryService creates a new RecoveryService.
func NewRecoveryService(s store.Store, auditWriter *BoundaryAuditWriter, eventBus *InvalidationEventBus, logger *slog.Logger) *RecoveryService {
	return &RecoveryService{
		store:        s,
		auditWriter:  auditWriter,
		eventBus:     eventBus,
		logger:       logger,
		recoveryLock: NewRecoveryLock(),
		nowFunc:      time.Now,
	}
}

// ---------------------------------------------------------------------------
// DisableAll — all-or-nothing boundary disable
// ---------------------------------------------------------------------------

// DisableAllResult is the outcome of disabling all boundaries.
type DisableAllResult struct {
	// DisabledCount is the number of boundaries that were disabled.
	DisabledCount int

	// DisabledIDs is the list of boundary IDs that were disabled.
	DisabledIDs []string

	// AuditID is the durable audit ID for this operation.
	AuditID string
}

// DisableAll deactivates all active boundaries in a single atomic operation.
// Either all boundaries are disabled and the audit record is written, or
// nothing changes. Returns the number of disabled boundaries.
func (rs *RecoveryService) DisableAll(ctx context.Context, actorID string) (*DisableAllResult, error) {
	// Acquire the recovery lock.
	if !rs.recoveryLock.TryAcquire(actorID) {
		return nil, fmt.Errorf("recovery lock already held — another recovery operation is in progress")
	}
	defer rs.recoveryLock.Release()

	// Publish recovery.started event.
	if rs.eventBus != nil {
		rs.eventBus.Publish(InvalidationEvent{
			Type:      EventRecoveryStarted,
			EntityID:  "all",
			Timestamp: rs.nowFunc(),
		})
	}

	// Load all constraints using the shared pagination helper.
	allConstraints, err := rs.listAllConstraints(ctx)
	if err != nil {
		return nil, err
	}

	// Filter to active (non-disabled) constraints.
	var activeIDs []string
	for _, c := range allConstraints {
		if !c.Disabled {
			activeIDs = append(activeIDs, c.ID)
		}
	}

	if len(activeIDs) == 0 {
		return &DisableAllResult{DisabledCount: 0}, nil
	}

	// Disable all active constraints.
	// NOTE: The Store interface lacks RunInTx, so we disable one-by-one.
	// If any disable fails, we attempt to re-enable the already-disabled ones
	// (best-effort rollback).
	var disabledIDs []string
	for _, id := range activeIDs {
		if err := rs.store.DisableAccessConstraint(ctx, id); err != nil {
			// Rollback: attempt to re-enable already-disabled constraints.
			rs.rollbackDisable(ctx, disabledIDs)
			return nil, fmt.Errorf("failed to disable constraint %s (rolled back %d): %w", id, len(disabledIDs), err)
		}
		disabledIDs = append(disabledIDs, id)
	}

	// Write the audit entry. If this fails, the disable must roll back.
	if rs.auditWriter != nil {
		auditID, err := rs.auditWriter.WriteAuditEntry(ctx, AuditRequest{
			ConstraintID:   "all",
			Operation:      "disable_all",
			ActorID:        actorID,
			CorrelationID:  correlationIDFromContext(ctx),
			BeforeRevision: int64(len(disabledIDs)),
			AfterRevision:  0,
			Classification: ClassificationRelax,
			ImpactCounts: ImpactCounts{
				AffectedPrincipals: len(disabledIDs),
			},
		})
		if err != nil {
			// Audit write failed — roll back the disable.
			rs.rollbackDisable(ctx, disabledIDs)
			return nil, fmt.Errorf("audit write failed, disable rolled back: %w", err)
		}

		rs.logger.Info("all boundaries disabled",
			"disabled_count", len(disabledIDs),
			"audit_id", auditID,
			"actor_id", actorID,
		)

		// Publish recovery.completed event (N4: fires regardless of audit config).
		if rs.eventBus != nil {
			rs.eventBus.Publish(InvalidationEvent{
				Type:      EventRecoveryCompleted,
				EntityID:  "all",
				Timestamp: rs.nowFunc(),
			})
		}

		return &DisableAllResult{
			DisabledCount: len(disabledIDs),
			DisabledIDs:   disabledIDs,
			AuditID:       auditID,
		}, nil
	}

	// Publish recovery.completed event even when audit is not configured (N4).
	if rs.eventBus != nil {
		rs.eventBus.Publish(InvalidationEvent{
			Type:      EventRecoveryCompleted,
			EntityID:  "all",
			Timestamp: rs.nowFunc(),
		})
	}

	return &DisableAllResult{
		DisabledCount: len(disabledIDs),
		DisabledIDs:   disabledIDs,
	}, nil
}

// rollbackDisable attempts to re-enable constraints that were disabled
// during a failed disable-all operation. Best-effort — errors are logged
// but not returned.
func (rs *RecoveryService) rollbackDisable(ctx context.Context, ids []string) {
	for _, id := range ids {
		c, err := rs.store.GetAccessConstraint(ctx, id)
		if err != nil {
			rs.logger.Error("rollback: failed to get constraint", "id", id, "error", err)
			continue
		}
		c.Disabled = false
		if _, err := rs.store.UpdateAccessConstraint(ctx, c, 0); err != nil {
			rs.logger.Error("rollback: failed to re-enable constraint", "id", id, "error", err)
		}
	}
}

// ---------------------------------------------------------------------------
// Recovery — re-enable boundaries
// ---------------------------------------------------------------------------

// RecoveryResult is the outcome of a recovery operation.
type RecoveryResult struct {
	// RecoveredCount is the number of boundaries that were re-enabled.
	RecoveredCount int

	// RecoveredIDs is the list of boundary IDs that were re-enabled.
	RecoveredIDs []string

	// AuditID is the durable audit ID for this operation.
	AuditID string
}

// RecoverAll re-enables all recovery-disabled boundaries. The operation
// verifies state consistency before re-enabling. If the audit write fails
// the recovery is rolled back.
func (rs *RecoveryService) RecoverAll(ctx context.Context, actorID string) (*RecoveryResult, error) {
	// Acquire the recovery lock.
	if !rs.recoveryLock.TryAcquire(actorID) {
		return nil, fmt.Errorf("recovery lock already held — another recovery operation is in progress")
	}
	defer rs.recoveryLock.Release()

	// Publish recovery.started event.
	if rs.eventBus != nil {
		rs.eventBus.Publish(InvalidationEvent{
			Type:      EventRecoveryStarted,
			EntityID:  "all",
			Timestamp: rs.nowFunc(),
		})
	}

	// Load all constraints using the shared pagination helper.
	allConstraints, err := rs.listAllConstraints(ctx)
	if err != nil {
		return nil, err
	}

	var disabledConstraints []*store.AccessConstraint
	for _, c := range allConstraints {
		if c.Disabled {
			disabledConstraints = append(disabledConstraints, c)
		}
	}

	if len(disabledConstraints) == 0 {
		return &RecoveryResult{RecoveredCount: 0}, nil
	}

	// Verify state consistency before re-enabling.
	// Each disabled constraint must still be valid (name, scope, permissions).
	for _, c := range disabledConstraints {
		hc := storeToHubAccessConstraint(c)
		if hc == nil {
			return nil, fmt.Errorf("constraint %s has invalid stored data — cannot recover", c.ID)
		}
		if hc.Degraded {
			return nil, fmt.Errorf("constraint %s is degraded — cannot recover without manual repair", c.ID)
		}
	}

	// Re-enable all disabled constraints.
	var recoveredIDs []string
	for _, c := range disabledConstraints {
		c.Disabled = false
		if _, err := rs.store.UpdateAccessConstraint(ctx, c, 0); err != nil {
			// Rollback: re-disable already-recovered constraints.
			rs.rollbackRecovery(ctx, recoveredIDs)
			return nil, fmt.Errorf("failed to recover constraint %s (rolled back %d): %w", c.ID, len(recoveredIDs), err)
		}
		recoveredIDs = append(recoveredIDs, c.ID)
	}

	// Write the audit entry. If this fails, recovery is rolled back.
	if rs.auditWriter != nil {
		auditID, err := rs.auditWriter.WriteAuditEntry(ctx, AuditRequest{
			ConstraintID:   "all",
			Operation:      "recovery",
			ActorID:        actorID,
			CorrelationID:  correlationIDFromContext(ctx),
			BeforeRevision: 0,
			AfterRevision:  int64(len(recoveredIDs)),
			Classification: ClassificationTighten,
			ImpactCounts: ImpactCounts{
				AffectedPrincipals: len(recoveredIDs),
			},
		})
		if err != nil {
			// Audit write failed — roll back the recovery.
			rs.rollbackRecovery(ctx, recoveredIDs)
			return nil, fmt.Errorf("recovery audit write failed, recovery rolled back: %w", err)
		}

		rs.logger.Info("all boundaries recovered",
			"recovered_count", len(recoveredIDs),
			"audit_id", auditID,
			"actor_id", actorID,
		)

		// Publish recovery.completed event (N4: fires regardless of audit config).
		if rs.eventBus != nil {
			rs.eventBus.Publish(InvalidationEvent{
				Type:      EventRecoveryCompleted,
				EntityID:  "all",
				Timestamp: rs.nowFunc(),
			})
		}

		return &RecoveryResult{
			RecoveredCount: len(recoveredIDs),
			RecoveredIDs:   recoveredIDs,
			AuditID:        auditID,
		}, nil
	}

	// Publish recovery.completed event even when audit is not configured (N4).
	if rs.eventBus != nil {
		rs.eventBus.Publish(InvalidationEvent{
			Type:      EventRecoveryCompleted,
			EntityID:  "all",
			Timestamp: rs.nowFunc(),
		})
	}

	return &RecoveryResult{
		RecoveredCount: len(recoveredIDs),
		RecoveredIDs:   recoveredIDs,
	}, nil
}

// rollbackRecovery re-disables constraints that were recovered during a
// failed recovery operation.
func (rs *RecoveryService) rollbackRecovery(ctx context.Context, ids []string) {
	for _, id := range ids {
		if err := rs.store.DisableAccessConstraint(ctx, id); err != nil {
			rs.logger.Error("recovery rollback: failed to re-disable constraint", "id", id, "error", err)
		}
	}
}

// ---------------------------------------------------------------------------
// ListRecoveryDisabled — read-only provenance
// ---------------------------------------------------------------------------

// ListRecoveryDisabled returns all currently recovery-disabled boundaries
// as read-only provenance. Does NOT expose HTTP disable/enable — that is
// B7 scope.
func (rs *RecoveryService) ListRecoveryDisabled(ctx context.Context) ([]*store.AccessConstraint, error) {
	allConstraints, err := rs.listAllConstraints(ctx)
	if err != nil {
		return nil, err
	}

	var disabled []*store.AccessConstraint
	for _, c := range allConstraints {
		if c.Disabled {
			disabled = append(disabled, c)
		}
	}
	return disabled, nil
}

// ---------------------------------------------------------------------------
// Pagination helper (N3)
// ---------------------------------------------------------------------------

// listAllConstraints loads all access constraints from the store using
// cursor-based pagination. Extracted to avoid repeating the pagination
// loop in DisableAll, RecoverAll, and ListRecoveryDisabled.
func (rs *RecoveryService) listAllConstraints(ctx context.Context) ([]*store.AccessConstraint, error) {
	const pageSize = 500
	var all []*store.AccessConstraint
	offset := 0
	for {
		page, err := rs.store.ListAccessConstraints(ctx, pageSize, offset)
		if err != nil {
			return nil, fmt.Errorf("failed to list constraints: %w", err)
		}
		all = append(all, page...)
		if len(page) < pageSize {
			break
		}
		offset += len(page)
	}
	return all, nil
}

// ---------------------------------------------------------------------------
// RecoveryLock — database-level online/offline exclusion (B6 §3)
// ---------------------------------------------------------------------------

// RecoveryLock provides database-level online/offline exclusion. Before any
// recovery read/write, the lock must be acquired. Two live Hub instances on
// the same Postgres must not both enter recovery mode. A live SQLite Hub
// must lock exclusively during recovery.
//
// This implementation uses an in-process mutex. For multi-instance Postgres
// deployments, this would be replaced with a database advisory lock
// (pg_advisory_lock) or similar mechanism. The interface is the same either
// way.
type RecoveryLock struct {
	mu     sync.Mutex
	held   bool
	holder string
}

// NewRecoveryLock creates a new RecoveryLock.
func NewRecoveryLock() *RecoveryLock {
	return &RecoveryLock{}
}

// TryAcquire attempts to acquire the recovery lock. Returns true if
// successful. The caller must call Release when done.
func (rl *RecoveryLock) TryAcquire(holderID string) bool {
	rl.mu.Lock()
	defer rl.mu.Unlock()
	if rl.held {
		return false
	}
	rl.held = true
	rl.holder = holderID
	return true
}

// Release releases the recovery lock.
func (rl *RecoveryLock) Release() {
	rl.mu.Lock()
	defer rl.mu.Unlock()
	rl.held = false
	rl.holder = ""
}

// IsHeld returns true if the lock is currently held.
func (rl *RecoveryLock) IsHeld() bool {
	rl.mu.Lock()
	defer rl.mu.Unlock()
	return rl.held
}

// Holder returns the ID of the current holder, or empty string if not held.
func (rl *RecoveryLock) Holder() string {
	rl.mu.Lock()
	defer rl.mu.Unlock()
	return rl.holder
}

// AcquireWithContext blocks until the lock is acquired or the context is
// cancelled/expired. This provides a context-based timeout for lock
// acquisition, preventing indefinite waits when another recovery operation
// is in progress.
//
// Usage:
//
//	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
//	defer cancel()
//	if err := lock.AcquireWithContext(ctx, "instance-1"); err != nil {
//	    // Lock not acquired within timeout.
//	}
//
// TODO: For multi-instance Postgres deployments, replace the in-process
// polling with a database advisory lock (pg_advisory_lock) that supports
// native timeout semantics.
func (rl *RecoveryLock) AcquireWithContext(ctx context.Context, holderID string) error {
	// Fast path: try to acquire immediately.
	if rl.TryAcquire(holderID) {
		return nil
	}

	// Poll with backoff until the context expires.
	ticker := time.NewTicker(50 * time.Millisecond)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return fmt.Errorf("recovery lock acquisition timed out: %w", ctx.Err())
		case <-ticker.C:
			if rl.TryAcquire(holderID) {
				return nil
			}
		}
	}
}
