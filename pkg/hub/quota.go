package hub

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// QuotaService provides quota enforcement for resource creation.
// It uses advisory locks to ensure atomic check-and-reserve under concurrency
// across Hub replicas (Postgres), and an in-process per-scope mutex to ensure
// the same atomicity within a single process — including single-node/SQLite
// deployments, where the advisory lock is a documented no-op (see
// entadapter.CompositeStore.TryAdvisoryLockObject) because there is only ever
// one process. Without the in-process mutex, two goroutines in that one
// process could still interleave the count-then-create sequence below (e.g.
// two concurrent agent starts at cap-1) and both succeed, over-allocating the
// scope by one (ptone/scion#1963).
type QuotaService struct {
	store  store.Store
	logger *slog.Logger

	scopeLocksMu sync.Mutex
	scopeLocks   map[string]*sync.Mutex
}

// lockForScope returns the process-local mutex for (limitName, scopeType,
// scopeID), creating it on first use. Locks are never removed — the key
// space is bounded by the number of distinct (limit, scope) pairs actually
// exercised, which is small and stable (one entry per project/broker/etc
// that has ever hit CheckAndReserve).
func (qs *QuotaService) lockForScope(limitName, scopeType, scopeID string) *sync.Mutex {
	key := limitName + "\x00" + scopeType + "\x00" + scopeID
	qs.scopeLocksMu.Lock()
	defer qs.scopeLocksMu.Unlock()
	if qs.scopeLocks == nil {
		qs.scopeLocks = make(map[string]*sync.Mutex)
	}
	m, ok := qs.scopeLocks[key]
	if !ok {
		m = &sync.Mutex{}
		qs.scopeLocks[key] = m
	}
	return m
}

// ErrQuotaLockContention is returned when the advisory lock cannot be acquired,
// indicating another concurrent request is checking the same quota scope.
// Callers may retry.
var ErrQuotaLockContention = errors.New("quota lock contention: retry")

// CheckAndReserve atomically checks quota and creates a reservation.
// It returns nil if the reservation succeeds, is already held, or if no
// limit is defined. It returns store.ErrQuotaExceeded if the quota is
// exhausted. It returns ErrQuotaLockContention if the advisory lock is held
// by another request.
func (qs *QuotaService) CheckAndReserve(ctx context.Context, limitName string, subjectID string, scopeType string, scopeID string, resourceID string) error {
	_, err := qs.Reserve(ctx, limitName, subjectID, scopeType, scopeID, resourceID)
	return err
}

// Reserve is CheckAndReserve that also reports whether this call created a
// new reservation (ptone/scion#1978). created is false when no limit is
// defined, the limit is unlimited, the resource already held an active
// reservation for this limit, or err is non-nil. Callers that roll back a
// reservation after a failed operation must only do so when created is
// true; otherwise they would release a reservation an earlier call made.
func (qs *QuotaService) Reserve(ctx context.Context, limitName string, subjectID string, scopeType string, scopeID string, resourceID string) (created bool, err error) {
	// 1. Look up LimitDefinition by name.
	limitDef, err := qs.store.GetLimitDefinitionByName(ctx, limitName)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			// No limit defined — no enforcement.
			return false, nil
		}
		return false, fmt.Errorf("quota: lookup limit definition %q: %w", limitName, err)
	}

	// 2. Resolve effective limit for the subject.
	effectiveLimit, err := qs.ResolveEffectiveLimit(ctx, limitDef.ID, subjectID, scopeType, scopeID)
	if err != nil {
		return false, fmt.Errorf("quota: resolve effective limit for %q: %w", limitName, err)
	}

	// 3. Unlimited — no enforcement.
	if effectiveLimit <= 0 {
		return false, nil
	}

	// 4. Acquire the in-process scope lock first (see lockForScope): this is
	// what actually serializes concurrent goroutines within this Hub process,
	// including on SQLite where the advisory lock below is a no-op.
	scopeLock := qs.lockForScope(limitName, scopeType, scopeID)
	scopeLock.Lock()
	defer scopeLock.Unlock()

	// 4b. Acquire advisory lock scoped to (quota class, scope hash). This is
	// the cross-replica primitive (Postgres); on SQLite it always succeeds
	// without providing mutual exclusion, which is why 4 above exists.
	locker, ok := qs.store.(store.AdvisoryLocker)
	if !ok {
		return false, fmt.Errorf("quota enforcement unavailable: store does not support advisory locks")
	}

	objID := store.StableProjectHash(scopeID)
	acquired, release, err := locker.TryAdvisoryLockObject(ctx, store.LockQuotaEnforcement, objID)
	if err != nil {
		return false, fmt.Errorf("quota: advisory lock for %q: %w", limitName, err)
	}
	defer func() {
		if releaseErr := release(); releaseErr != nil {
			qs.logger.Warn("failed to release quota advisory lock",
				"limit", limitName, "error", releaseErr)
		}
	}()

	if !acquired {
		return false, ErrQuotaLockContention
	}

	// 4.5. Idempotency (#1963): a resource that already holds an active
	// reservation for this limit must not accumulate a second one. Without
	// this, re-reserving on every agent start/resume (necessary because stop
	// and suspend now release the reservation) would let repeated
	// start/stop cycles — or a start call against an agent that is already
	// counted (e.g. a fresh create, or start called again on an already-
	// running agent) — silently consume extra quota slots for one resource.
	// Checked under the advisory lock so a concurrent release can't race this
	// read.
	alreadyReserved, err := qs.store.HasActiveReservation(ctx, limitDef.ID, resourceID)
	if err != nil {
		return false, fmt.Errorf("quota: check existing reservation for %q: %w", limitName, err)
	}
	if alreadyReserved {
		return false, nil
	}

	// 5. Count active reservations.
	count, err := qs.store.CountActiveReservations(ctx, limitDef.ID, subjectID, scopeType, scopeID)
	if err != nil {
		return false, fmt.Errorf("quota: count active reservations for %q: %w", limitName, err)
	}

	// 6. Check quota.
	if count >= effectiveLimit {
		return false, store.ErrQuotaExceeded
	}

	// 7. Create reservation.
	reservation := &store.UsageReservation{
		LimitDefinitionID: limitDef.ID,
		SubjectID:         subjectID,
		ScopeType:         scopeType,
		ScopeID:           scopeID,
		ResourceID:        resourceID,
		Reserved:          1,
	}
	if _, err := qs.store.CreateUsageReservation(ctx, reservation); err != nil {
		return false, fmt.Errorf("quota: create reservation for %q: %w", limitName, err)
	}

	return true, nil
}

// ResolveEffectiveLimit determines the effective limit using the merge rule:
// most generous (maximum value) wins across all matching bindings.
// Value <= 0 means "unlimited" — if ANY binding grants unlimited, the result
// is unlimited (0) because that is the most generous possible limit.
func (qs *QuotaService) ResolveEffectiveLimit(ctx context.Context, limitDefID string, subjectID string, scopeType string, scopeID string) (int64, error) {
	// Collect all matching bindings from every source.
	var matchingBindings []*store.EntitlementBinding

	// 1. Collect user-specific bindings.
	userBindings, err := qs.store.ListEntitlementBindingsForSubject(ctx, store.EntitlementSubjectUser, subjectID)
	if err != nil {
		return 0, fmt.Errorf("list user bindings: %w", err)
	}
	for _, b := range userBindings {
		if b.LimitDefinitionID == limitDefID && matchesScope(b, scopeType, scopeID) {
			matchingBindings = append(matchingBindings, b)
		}
	}

	// 2. Collect group bindings via user's group memberships.
	groupMemberships, err := qs.store.GetUserGroups(ctx, subjectID)
	if err != nil {
		// Not all store implementations may have groups; log and continue.
		qs.logger.Debug("failed to get user groups for quota resolution",
			"subject_id", subjectID, "error", err)
	} else {
		for _, gm := range groupMemberships {
			groupBindings, err := qs.store.ListEntitlementBindingsForSubject(ctx, store.EntitlementSubjectGroup, gm.GroupID)
			if err != nil {
				qs.logger.Debug("failed to list group bindings",
					"group_id", gm.GroupID, "error", err)
				continue
			}
			for _, b := range groupBindings {
				if b.LimitDefinitionID == limitDefID && matchesScope(b, scopeType, scopeID) {
					matchingBindings = append(matchingBindings, b)
				}
			}
		}
	}

	// 3. Collect system default bindings.
	sysBindings, err := qs.store.ListEntitlementBindingsForSubject(ctx, store.EntitlementSubjectSystemDefault, "")
	if err != nil {
		qs.logger.Debug("failed to list system default bindings", "error", err)
	} else {
		for _, b := range sysBindings {
			if b.LimitDefinitionID == limitDefID && matchesScope(b, scopeType, scopeID) {
				matchingBindings = append(matchingBindings, b)
			}
		}
	}

	// 4. If no binding found, fall back to limit definition's default value.
	if len(matchingBindings) == 0 {
		limitDef, err := qs.store.GetLimitDefinition(ctx, limitDefID)
		if err != nil {
			return 0, fmt.Errorf("get limit definition: %w", err)
		}
		// DefaultValue <= 0 means unlimited (return 0).
		if limitDef.DefaultValue <= 0 {
			return 0, nil
		}
		return limitDef.DefaultValue, nil
	}

	// 5. Apply merge rule: if ANY binding grants unlimited (Value <= 0),
	// the result is unlimited — that's the most generous possible limit.
	for _, b := range matchingBindings {
		if b.Value <= 0 {
			return 0, nil // unlimited
		}
	}

	// 6. Otherwise, take the maximum positive value (most generous finite limit).
	var maxValue int64
	for _, b := range matchingBindings {
		if b.Value > maxValue {
			maxValue = b.Value
		}
	}

	return maxValue, nil
}

// Release marks resourceID's active reservation for limitName as released
// (best-effort). It touches only that limit: a resource can hold
// reservations under several limits (an agent holds both
// max_agents_per_broker and max_agents_per_project), and releasing one must
// not release the others (ptone/scion#1978). It is a no-op when the limit is
// not defined or the resource holds no active reservation for it. Errors
// are logged but not returned: deletion must not be blocked by quota
// bookkeeping.
func (qs *QuotaService) Release(ctx context.Context, limitName string, resourceID string) {
	limitDef, err := qs.store.GetLimitDefinitionByName(ctx, limitName)
	if err != nil {
		if !errors.Is(err, store.ErrNotFound) {
			qs.logger.Warn("failed to release quota reservation: limit lookup failed",
				"limit", limitName, "resource_id", resourceID, "error", err)
		}
		return
	}
	if err := qs.store.ReleaseReservation(ctx, limitDef.ID, resourceID); err != nil && !errors.Is(err, store.ErrNotFound) {
		qs.logger.Warn("failed to release quota reservation",
			"limit", limitName, "resource_id", resourceID, "error", err)
	}
}

// matchesScope checks whether a binding's scope matches the requested scope.
// A system-scoped binding matches any request scope (it applies globally).
func matchesScope(b *store.EntitlementBinding, scopeType, scopeID string) bool {
	// System-scoped bindings apply to all scopes.
	if b.ScopeType == store.QuotaScopeSystem {
		return true
	}
	return b.ScopeType == scopeType && b.ScopeID == scopeID
}
