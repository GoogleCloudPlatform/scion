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
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/hub/auditevent"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// ---------------------------------------------------------------------------
// Offline recovery service (B6)
// ---------------------------------------------------------------------------

// RecoveryService provides offline disable and recovery operations for
// access boundaries. Each affected constraint is changed in its own
// transaction together with exactly one access_constraint_history row
// (operation recovery_disable or recovery_enable). Constraints are processed
// sequentially; the first failure stops the operation, and the constraints
// already committed stay committed (see RecoveryPartialError). Each
// transaction re-reads its constraint first: one whose disabled state
// already matches the target (it changed after the list was taken) is
// skipped, with no mutation, no row, and no count.
//
// The service has no production caller; it is exercised by tests only.
type RecoveryService struct {
	store    store.Store
	eventBus *InvalidationEventBus
	logger   *slog.Logger

	// recoveryLock enforces database-level online/offline exclusion.
	// Only one recovery operation can run at a time per Hub instance.
	recoveryLock *RecoveryLock

	// nowFunc is injectable for testing. Defaults to time.Now.
	nowFunc func() time.Time
}

// NewRecoveryService creates a new RecoveryService.
func NewRecoveryService(s store.Store, eventBus *InvalidationEventBus, logger *slog.Logger) *RecoveryService {
	return &RecoveryService{
		store:        s,
		eventBus:     eventBus,
		logger:       logger,
		recoveryLock: NewRecoveryLock(),
		nowFunc:      time.Now,
	}
}

// RecoveryPartialError reports a recovery operation that stopped at its
// first failure. Committed constraints are NOT rolled back: each one was
// committed in its own transaction together with its history row. The
// failed constraint has neither its change nor a row.
type RecoveryPartialError struct {
	// Operation is the history operation: recovery_disable or
	// recovery_enable.
	Operation string

	// Committed is the number of constraints committed before the failure.
	Committed int

	// CommittedIDs are the committed constraint IDs, in processing order.
	CommittedIDs []string

	// AuditIDs are the history event IDs of the committed constraints'
	// rows, index-aligned with CommittedIDs.
	AuditIDs []string

	// FailedID is the constraint whose transaction failed.
	FailedID string

	// Err is the failure cause.
	Err error
}

func (e *RecoveryPartialError) Error() string {
	return fmt.Sprintf("%s stopped at constraint %s after committing %d constraint(s); committed constraints stay committed: %v",
		e.Operation, e.FailedID, e.Committed, e.Err)
}

func (e *RecoveryPartialError) Unwrap() error { return e.Err }

// errRecoveryAlreadyInState ends a per-constraint transaction without a
// change: the constraint already has the target state.
var errRecoveryAlreadyInState = errors.New("constraint already in the target state")

// ---------------------------------------------------------------------------
// DisableAll — disable every active boundary, one transaction per boundary
// ---------------------------------------------------------------------------

// DisableAllResult is the outcome of disabling all boundaries.
type DisableAllResult struct {
	// DisabledCount is the number of boundaries that were disabled.
	DisabledCount int

	// DisabledIDs is the list of boundary IDs that were disabled.
	DisabledIDs []string

	// AuditIDs are the event IDs of the history rows written for the
	// disabled boundaries, index-aligned with DisabledIDs.
	AuditIDs []string
}

// DisableAll deactivates every active boundary. Each boundary is disabled in
// its own transaction with one recovery_disable history row (classification
// relax) carrying that boundary's revisions before and after. On the first
// failure it returns a *RecoveryPartialError; boundaries disabled before the
// failure stay disabled.
func (rs *RecoveryService) DisableAll(ctx context.Context, actor PrincipalContext) (*DisableAllResult, error) {
	// Acquire the recovery lock.
	if !rs.recoveryLock.TryAcquire(actor.ID) {
		return nil, fmt.Errorf("recovery lock already held — another recovery operation is in progress")
	}
	defer rs.recoveryLock.Release()

	// One audit operation per call: the request ID when there is one, else
	// one fresh operation ID shared by every row this call writes.
	ctx = withAccessBoundaryAuditOperation(ctx)

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

	var disabledIDs, auditIDs []string
	for _, id := range activeIDs {
		var auditID string
		err := rs.store.WithTx(ctx, func(tx store.Store) error {
			before, err := tx.GetAccessConstraint(ctx, id)
			if err != nil {
				return fmt.Errorf("load constraint: %w", err)
			}
			if before.Disabled {
				return errRecoveryAlreadyInState
			}
			if err := tx.DisableAccessConstraint(ctx, id); err != nil {
				return fmt.Errorf("disable constraint: %w", err)
			}
			after, err := tx.GetAccessConstraint(ctx, id)
			if err != nil {
				return fmt.Errorf("reload constraint: %w", err)
			}
			auditID, err = appendRecoveryHistory(ctx, tx, auditevent.AccessBoundaryRecoveryDisable, actor, before, after, ClassificationRelax)
			return err
		})
		if errors.Is(err, errRecoveryAlreadyInState) {
			continue
		}
		if err != nil {
			return nil, &RecoveryPartialError{
				Operation:    string(auditevent.AccessBoundaryRecoveryDisable),
				Committed:    len(disabledIDs),
				CommittedIDs: disabledIDs,
				AuditIDs:     auditIDs,
				FailedID:     id,
				Err:          err,
			}
		}
		disabledIDs = append(disabledIDs, id)
		auditIDs = append(auditIDs, auditID)
	}

	rs.logger.Info("all boundaries disabled",
		"disabled_count", len(disabledIDs),
		"actor_id", actor.ID,
	)

	// Publish recovery.completed event.
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
		AuditIDs:      auditIDs,
	}, nil
}

// ---------------------------------------------------------------------------
// Recovery — re-enable boundaries, one transaction per boundary
// ---------------------------------------------------------------------------

// RecoveryResult is the outcome of a recovery operation.
type RecoveryResult struct {
	// RecoveredCount is the number of boundaries that were re-enabled.
	RecoveredCount int

	// RecoveredIDs is the list of boundary IDs that were re-enabled.
	RecoveredIDs []string

	// AuditIDs are the event IDs of the history rows written for the
	// re-enabled boundaries, index-aligned with RecoveredIDs.
	AuditIDs []string
}

// RecoverAll re-enables all recovery-disabled boundaries. It first verifies
// that every disabled boundary is still valid, then re-enables each one in
// its own transaction with one recovery_enable history row (classification
// tighten) carrying that boundary's revisions before and after. On the first
// failure it returns a *RecoveryPartialError; boundaries re-enabled before
// the failure stay enabled.
func (rs *RecoveryService) RecoverAll(ctx context.Context, actor PrincipalContext) (*RecoveryResult, error) {
	// Acquire the recovery lock.
	if !rs.recoveryLock.TryAcquire(actor.ID) {
		return nil, fmt.Errorf("recovery lock already held — another recovery operation is in progress")
	}
	defer rs.recoveryLock.Release()

	// One audit operation per call (see DisableAll).
	ctx = withAccessBoundaryAuditOperation(ctx)

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

	var recoveredIDs, auditIDs []string
	for _, c := range disabledConstraints {
		id := c.ID
		var auditID string
		err := rs.store.WithTx(ctx, func(tx store.Store) error {
			before, err := tx.GetAccessConstraint(ctx, id)
			if err != nil {
				return fmt.Errorf("load constraint: %w", err)
			}
			if !before.Disabled {
				return errRecoveryAlreadyInState
			}
			enabled := *before
			enabled.Disabled = false
			after, err := tx.UpdateAccessConstraint(ctx, &enabled, before.Revision)
			if err != nil {
				return fmt.Errorf("re-enable constraint: %w", err)
			}
			auditID, err = appendRecoveryHistory(ctx, tx, auditevent.AccessBoundaryRecoveryEnable, actor, before, after, ClassificationTighten)
			return err
		})
		if errors.Is(err, errRecoveryAlreadyInState) {
			continue
		}
		if err != nil {
			return nil, &RecoveryPartialError{
				Operation:    string(auditevent.AccessBoundaryRecoveryEnable),
				Committed:    len(recoveredIDs),
				CommittedIDs: recoveredIDs,
				AuditIDs:     auditIDs,
				FailedID:     id,
				Err:          err,
			}
		}
		recoveredIDs = append(recoveredIDs, id)
		auditIDs = append(auditIDs, auditID)
	}

	rs.logger.Info("all boundaries recovered",
		"recovered_count", len(recoveredIDs),
		"actor_id", actor.ID,
	)

	// Publish recovery.completed event.
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
		AuditIDs:       auditIDs,
	}, nil
}

// appendRecoveryHistory writes the single history row for one recovered or
// disabled constraint inside tx and returns its event ID. before and after
// are the constraint as read in tx around the mutation.
func appendRecoveryHistory(
	ctx context.Context,
	tx store.Store,
	action auditevent.AccessBoundaryHistoryAction,
	actor PrincipalContext,
	before, after *store.AccessConstraint,
	classification string,
) (string, error) {
	beforeRevision := before.Revision
	afterRevision := after.Revision
	event, err := buildAccessConstraintHistoryEvent(ctx, accessConstraintHistoryEventInput{
		Action:         action,
		Actor:          actor,
		Constraint:     after,
		BeforeRevision: &beforeRevision,
		AfterRevision:  &afterRevision,
		Classification: classification,
	})
	if err != nil {
		return "", fmt.Errorf("build audit event: %w", err)
	}
	history, err := accessConstraintHistoryFromEvent(event)
	if err != nil {
		return "", fmt.Errorf("map audit history: %w", err)
	}
	if err := tx.AppendConstraintHistoryTx(ctx, history); err != nil {
		return "", fmt.Errorf("append audit history: %w", err)
	}
	return event.EventID, nil
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
