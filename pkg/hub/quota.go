package hub

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// QuotaService provides quota enforcement for resource creation.
// It uses advisory locks to ensure atomic check-and-reserve under concurrency.
type QuotaService struct {
	store  store.Store
	logger *slog.Logger
}

// ErrQuotaLockContention is returned when the advisory lock cannot be acquired,
// indicating another concurrent request is checking the same quota scope.
// Callers may retry.
var ErrQuotaLockContention = errors.New("quota lock contention: retry")

// CheckAndReserve atomically checks quota and creates a reservation.
// It returns nil if the reservation succeeds or if no limit is defined.
// It returns store.ErrQuotaExceeded if the quota is exhausted.
// It returns ErrQuotaLockContention if the advisory lock is held by another request.
func (qs *QuotaService) CheckAndReserve(ctx context.Context, limitName string, subjectID string, scopeType string, scopeID string, resourceID string) error {
	// 1. Look up LimitDefinition by name.
	limitDef, err := qs.store.GetLimitDefinitionByName(ctx, limitName)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			// No limit defined — no enforcement.
			return nil
		}
		return fmt.Errorf("quota: lookup limit definition %q: %w", limitName, err)
	}

	// 2. Resolve effective limit for the subject.
	effectiveLimit, err := qs.ResolveEffectiveLimit(ctx, limitDef.ID, subjectID, scopeType, scopeID)
	if err != nil {
		return fmt.Errorf("quota: resolve effective limit for %q: %w", limitName, err)
	}

	// 3. Unlimited — no enforcement.
	if effectiveLimit <= 0 {
		return nil
	}

	// 4. Acquire advisory lock scoped to (quota class, scope hash).
	locker, ok := qs.store.(store.AdvisoryLocker)
	if !ok {
		return fmt.Errorf("quota enforcement unavailable: store does not support advisory locks")
	}

	objID := store.StableProjectHash(scopeID)
	acquired, release, err := locker.TryAdvisoryLockObject(ctx, store.LockQuotaEnforcement, objID)
	if err != nil {
		return fmt.Errorf("quota: advisory lock for %q: %w", limitName, err)
	}
	defer func() {
		if releaseErr := release(); releaseErr != nil {
			qs.logger.Warn("failed to release quota advisory lock",
				"limit", limitName, "error", releaseErr)
		}
	}()

	if !acquired {
		return ErrQuotaLockContention
	}

	// 5. Count active reservations.
	count, err := qs.store.CountActiveReservations(ctx, limitDef.ID, subjectID, scopeType, scopeID)
	if err != nil {
		return fmt.Errorf("quota: count active reservations for %q: %w", limitName, err)
	}

	// 6. Check quota.
	if count >= effectiveLimit {
		return store.ErrQuotaExceeded
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
		return fmt.Errorf("quota: create reservation for %q: %w", limitName, err)
	}

	return nil
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

// Release marks reservations for a resource as released (best-effort).
// Errors are logged but not returned — deletion must not be blocked by quota bookkeeping.
func (qs *QuotaService) Release(ctx context.Context, limitName string, resourceID string) {
	if err := qs.store.ReleaseReservationsByResource(ctx, resourceID); err != nil {
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
