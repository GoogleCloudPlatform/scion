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
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ---------------------------------------------------------------------------
// lockingStoreWrapper wraps a real store.Store and overrides the
// TryAdvisoryLockObject method with a proper mutex-based implementation,
// making advisory locks effective on SQLite (where they are normally no-ops).
// ---------------------------------------------------------------------------

type lockingStoreWrapper struct {
	store.Store
	mu    sync.Mutex
	locks map[lockKey]*sync.Mutex
}

type lockKey struct {
	classID store.AdvisoryLockKey
	objID   int32
}

func newLockingStoreWrapper(s store.Store) *lockingStoreWrapper {
	return &lockingStoreWrapper{
		Store: s,
		locks: make(map[lockKey]*sync.Mutex),
	}
}

func (w *lockingStoreWrapper) TryAdvisoryLock(ctx context.Context, key store.AdvisoryLockKey) (bool, func() error, error) {
	return w.TryAdvisoryLockObject(ctx, key, 0)
}

func (w *lockingStoreWrapper) TryAdvisoryLockObject(_ context.Context, classID store.AdvisoryLockKey, objID int32) (bool, func() error, error) {
	k := lockKey{classID, objID}

	w.mu.Lock()
	m, ok := w.locks[k]
	if !ok {
		m = &sync.Mutex{}
		w.locks[k] = m
	}
	w.mu.Unlock()

	if !m.TryLock() {
		return false, func() error { return nil }, nil
	}

	return true, func() error {
		m.Unlock()
		return nil
	}, nil
}

// ---------------------------------------------------------------------------
// Helper to create a QuotaService backed by a real (migrated) test store
// wrapped with effective advisory locking.
// ---------------------------------------------------------------------------

func newTestQuotaService(t *testing.T) (*QuotaService, store.Store) {
	t.Helper()
	baseStore, err := newTestStore(":memory:")
	if err != nil {
		t.Fatalf("newTestStore: %v", err)
	}
	t.Cleanup(func() { _ = baseStore.Close() })

	wrapped := newLockingStoreWrapper(baseStore)

	qs := &QuotaService{
		store:  wrapped,
		logger: slog.Default().With("component", "quota-test"),
	}
	return qs, wrapped
}

// seedLimit creates a LimitDefinition in the store and returns it.
func seedLimit(t *testing.T, s store.Store, name string, defaultValue int64) *store.LimitDefinition {
	t.Helper()
	def, err := s.CreateLimitDefinition(context.Background(), &store.LimitDefinition{
		Name:         name,
		ResourceType: "test",
		Unit:         "count",
		Description:  "test limit: " + name,
		DefaultValue: defaultValue,
		System:       true,
	})
	require.NoError(t, err)
	return def
}

// seedBinding creates an EntitlementBinding in the store.
func seedBinding(t *testing.T, s store.Store, limitDefID, subjectType, subjectID, scopeType, scopeID string, value int64) {
	t.Helper()
	_, err := s.CreateEntitlementBinding(context.Background(), &store.EntitlementBinding{
		LimitDefinitionID: limitDefID,
		SubjectType:       subjectType,
		SubjectID:         subjectID,
		ScopeType:         scopeType,
		ScopeID:           scopeID,
		Value:             value,
		CreatedBy:         "test",
	})
	require.NoError(t, err)
}

// ===========================================================================
// CRITICAL ACCEPTANCE TEST: 100 parallel creates against a limit of 10
// ===========================================================================

func TestQuotaConcurrency_100Creates_Limit10(t *testing.T) {
	qs, s := newTestQuotaService(t)
	ctx := context.Background()

	// Seed a limit definition with DefaultValue=10.
	seedLimit(t, s, "max_agents_per_project", 10)

	const (
		goroutines = 100
		limit      = 10
	)

	var (
		successes atomic.Int64
		failures  atomic.Int64
		retries   atomic.Int64
	)

	var wg sync.WaitGroup
	wg.Add(goroutines)

	for i := 0; i < goroutines; i++ {
		go func(idx int) {
			defer wg.Done()
			resourceID := fmt.Sprintf("agent-%d", idx)

			// Retry loop for lock contention.
			for attempt := 0; attempt < 200; attempt++ {
				err := qs.CheckAndReserve(ctx, "max_agents_per_project", "user-1", "project", "project-1", resourceID)
				if err == nil {
					successes.Add(1)
					return
				}
				if errors.Is(err, store.ErrQuotaExceeded) {
					failures.Add(1)
					return
				}
				if errors.Is(err, ErrQuotaLockContention) {
					retries.Add(1)
					continue // retry
				}
				// Unexpected error
				t.Errorf("goroutine %d: unexpected error: %v", idx, err)
				failures.Add(1)
				return
			}
			// Exhausted retries
			t.Errorf("goroutine %d: exhausted retries due to lock contention", 0)
			failures.Add(1)
		}(i)
	}

	wg.Wait()

	t.Logf("successes=%d failures=%d retries=%d", successes.Load(), failures.Load(), retries.Load())

	assert.Equal(t, int64(limit), successes.Load(), "exactly %d reservations should succeed", limit)
	assert.Equal(t, int64(goroutines-limit), failures.Load(), "remaining should fail with ErrQuotaExceeded")
}

// ===========================================================================
// Merge rule tests
// ===========================================================================

func TestResolveEffectiveLimit_UserInGroupsMostGenerousWins(t *testing.T) {
	qs, s := newTestQuotaService(t)
	ctx := context.Background()

	def := seedLimit(t, s, "max_agents_per_project", 0)

	groupA := uuid.NewString()
	groupB := uuid.NewString()
	userID := uuid.NewString()

	// Create user and groups.
	require.NoError(t, s.CreateUser(ctx, &store.User{ID: userID, Email: userID + "@test.com", DisplayName: "Test User", Role: "member", Status: "active"}))
	require.NoError(t, s.CreateGroup(ctx, &store.Group{ID: groupA, Name: "Group A", Slug: "group-a-" + groupA[:8]}))
	require.NoError(t, s.CreateGroup(ctx, &store.Group{ID: groupB, Name: "Group B", Slug: "group-b-" + groupB[:8]}))

	// Add user to both groups.
	require.NoError(t, s.AddGroupMember(ctx, &store.GroupMember{GroupID: groupA, MemberType: "user", MemberID: userID, Role: "member"}))
	require.NoError(t, s.AddGroupMember(ctx, &store.GroupMember{GroupID: groupB, MemberType: "user", MemberID: userID, Role: "member"}))

	// Group A gets limit 5, group B gets limit 50.
	seedBinding(t, s, def.ID, store.EntitlementSubjectGroup, groupA, store.QuotaScopeSystem, "", 5)
	seedBinding(t, s, def.ID, store.EntitlementSubjectGroup, groupB, store.QuotaScopeSystem, "", 50)

	effective, err := qs.ResolveEffectiveLimit(ctx, def.ID, userID, store.QuotaScopeSystem, "")
	require.NoError(t, err)
	assert.Equal(t, int64(50), effective, "most generous group binding (50) should win")
}

func TestResolveEffectiveLimit_UserOverridesGroup(t *testing.T) {
	qs, s := newTestQuotaService(t)
	ctx := context.Background()

	def := seedLimit(t, s, "max_agents_per_project", 0)

	groupC := uuid.NewString()
	userID := uuid.NewString()

	// Create user and group.
	require.NoError(t, s.CreateUser(ctx, &store.User{ID: userID, Email: userID + "@test.com", DisplayName: "Test User", Role: "member", Status: "active"}))
	require.NoError(t, s.CreateGroup(ctx, &store.Group{ID: groupC, Name: "Group C", Slug: "group-c-" + groupC[:8]}))
	require.NoError(t, s.AddGroupMember(ctx, &store.GroupMember{GroupID: groupC, MemberType: "user", MemberID: userID, Role: "member"}))

	// Group binding: 50
	seedBinding(t, s, def.ID, store.EntitlementSubjectGroup, groupC, store.QuotaScopeSystem, "", 50)
	// User-specific binding: 100
	seedBinding(t, s, def.ID, store.EntitlementSubjectUser, userID, store.QuotaScopeSystem, "", 100)

	effective, err := qs.ResolveEffectiveLimit(ctx, def.ID, userID, store.QuotaScopeSystem, "")
	require.NoError(t, err)
	assert.Equal(t, int64(100), effective, "user-specific binding (100) should win over group (50)")
}

func TestResolveEffectiveLimit_SystemDefault(t *testing.T) {
	qs, s := newTestQuotaService(t)
	ctx := context.Background()

	def := seedLimit(t, s, "max_agents_per_project", 0)

	// System default binding: 10
	seedBinding(t, s, def.ID, store.EntitlementSubjectSystemDefault, "", store.QuotaScopeSystem, "", 10)

	effective, err := qs.ResolveEffectiveLimit(ctx, def.ID, "user-3", store.QuotaScopeSystem, "")
	require.NoError(t, err)
	assert.Equal(t, int64(10), effective, "system default (10) should apply when no user/group binding exists")
}

func TestResolveEffectiveLimit_NoBindingDefaultValueZero(t *testing.T) {
	qs, s := newTestQuotaService(t)
	ctx := context.Background()

	def := seedLimit(t, s, "max_agents_per_project", 0) // DefaultValue=0 → unlimited

	effective, err := qs.ResolveEffectiveLimit(ctx, def.ID, "user-4", store.QuotaScopeSystem, "")
	require.NoError(t, err)
	assert.Equal(t, int64(0), effective, "no binding + DefaultValue=0 should return 0 (unlimited)")
}

func TestResolveEffectiveLimit_NoBindingDefaultValuePositive(t *testing.T) {
	qs, s := newTestQuotaService(t)
	ctx := context.Background()

	def := seedLimit(t, s, "max_agents_per_project", 10) // DefaultValue=10

	effective, err := qs.ResolveEffectiveLimit(ctx, def.ID, "user-5", store.QuotaScopeSystem, "")
	require.NoError(t, err)
	assert.Equal(t, int64(10), effective, "no binding + DefaultValue=10 should return 10")
}

// ===========================================================================
// CheckAndReserve unit tests
// ===========================================================================

func TestCheckAndReserve_NoLimitDefined(t *testing.T) {
	qs, _ := newTestQuotaService(t)
	ctx := context.Background()

	// No limit definition exists — should return nil (no enforcement).
	err := qs.CheckAndReserve(ctx, "nonexistent_limit", "user-1", "project", "p1", "r1")
	assert.NoError(t, err)
}

func TestCheckAndReserve_UnlimitedDefaultValue(t *testing.T) {
	qs, s := newTestQuotaService(t)
	ctx := context.Background()

	// DefaultValue=0 → unlimited
	seedLimit(t, s, "unlimited_limit", 0)

	err := qs.CheckAndReserve(ctx, "unlimited_limit", "user-1", "project", "p1", "r1")
	assert.NoError(t, err)
}

func TestCheckAndReserve_ExceedsQuota(t *testing.T) {
	qs, s := newTestQuotaService(t)
	ctx := context.Background()

	seedLimit(t, s, "max_test", 2)

	// Reserve 2 resources — should succeed.
	require.NoError(t, qs.CheckAndReserve(ctx, "max_test", "user-1", "project", "p1", "r1"))
	require.NoError(t, qs.CheckAndReserve(ctx, "max_test", "user-1", "project", "p1", "r2"))

	// Third should fail.
	err := qs.CheckAndReserve(ctx, "max_test", "user-1", "project", "p1", "r3")
	assert.ErrorIs(t, err, store.ErrQuotaExceeded)
}

// ===========================================================================
// Release tests
// ===========================================================================

func TestRelease_DecreasesCount(t *testing.T) {
	qs, s := newTestQuotaService(t)
	ctx := context.Background()

	seedLimit(t, s, "max_test", 5)

	// Create 5 reservations.
	for i := 0; i < 5; i++ {
		err := qs.CheckAndReserve(ctx, "max_test", "user-1", "project", "p1", fmt.Sprintf("r-%d", i))
		require.NoError(t, err)
	}

	// Quota full — next should fail.
	err := qs.CheckAndReserve(ctx, "max_test", "user-1", "project", "p1", "r-overflow")
	require.ErrorIs(t, err, store.ErrQuotaExceeded)

	// Release 2.
	qs.Release(ctx, "max_test", "r-0")
	qs.Release(ctx, "max_test", "r-1")

	// Now we should be able to reserve 2 more.
	require.NoError(t, qs.CheckAndReserve(ctx, "max_test", "user-1", "project", "p1", "r-5"))
	require.NoError(t, qs.CheckAndReserve(ctx, "max_test", "user-1", "project", "p1", "r-6"))

	// But not a third.
	err = qs.CheckAndReserve(ctx, "max_test", "user-1", "project", "p1", "r-7")
	assert.ErrorIs(t, err, store.ErrQuotaExceeded)
}

func TestRelease_Idempotent(t *testing.T) {
	qs, s := newTestQuotaService(t)
	ctx := context.Background()

	seedLimit(t, s, "max_test", 5)

	// Release a non-existent reservation — should be a no-op (no panic/error).
	qs.Release(ctx, "max_test", "nonexistent-resource")
	// Call again to confirm idempotency.
	qs.Release(ctx, "max_test", "nonexistent-resource")
}

// ===========================================================================
// Unlimited merge rule tests
// ===========================================================================

func TestResolveEffectiveLimit_UnlimitedBindingWins(t *testing.T) {
	qs, s := newTestQuotaService(t)
	ctx := context.Background()

	def := seedLimit(t, s, "max_agents_per_project", 10)

	groupA := uuid.NewString()
	userID := uuid.NewString()

	require.NoError(t, s.CreateUser(ctx, &store.User{ID: userID, Email: userID + "@test.com", DisplayName: "Test User", Role: "member", Status: "active"}))
	require.NoError(t, s.CreateGroup(ctx, &store.Group{ID: groupA, Name: "Group U", Slug: "group-u-" + groupA[:8]}))
	require.NoError(t, s.AddGroupMember(ctx, &store.GroupMember{GroupID: groupA, MemberType: "user", MemberID: userID, Role: "member"}))

	// User has finite binding (50), but their group grants unlimited (0).
	// Unlimited should always win because it is the most generous.
	seedBinding(t, s, def.ID, store.EntitlementSubjectUser, userID, store.QuotaScopeSystem, "", 50)
	seedBinding(t, s, def.ID, store.EntitlementSubjectGroup, groupA, store.QuotaScopeSystem, "", 0)

	effective, err := qs.ResolveEffectiveLimit(ctx, def.ID, userID, store.QuotaScopeSystem, "")
	require.NoError(t, err)
	assert.Equal(t, int64(0), effective, "unlimited binding (Value=0) should win over finite binding (Value=50)")
}

func TestResolveEffectiveLimit_UnlimitedGroupBindingWins(t *testing.T) {
	qs, s := newTestQuotaService(t)
	ctx := context.Background()

	def := seedLimit(t, s, "max_agents_per_project", 10)

	groupA := uuid.NewString()
	userID := uuid.NewString()

	require.NoError(t, s.CreateUser(ctx, &store.User{ID: userID, Email: userID + "@test.com", DisplayName: "Test User", Role: "member", Status: "active"}))
	require.NoError(t, s.CreateGroup(ctx, &store.Group{ID: groupA, Name: "Group A", Slug: "group-a-" + groupA[:8]}))
	require.NoError(t, s.AddGroupMember(ctx, &store.GroupMember{GroupID: groupA, MemberType: "user", MemberID: userID, Role: "member"}))

	// User has finite binding (50), group has unlimited binding (0).
	// Unlimited should win across the merge.
	seedBinding(t, s, def.ID, store.EntitlementSubjectUser, userID, store.QuotaScopeSystem, "", 50)
	seedBinding(t, s, def.ID, store.EntitlementSubjectGroup, groupA, store.QuotaScopeSystem, "", 0)

	effective, err := qs.ResolveEffectiveLimit(ctx, def.ID, userID, store.QuotaScopeSystem, "")
	require.NoError(t, err)
	assert.Equal(t, int64(0), effective, "unlimited group binding (Value=0) should win over finite user binding (Value=50)")
}

func TestResolveEffectiveLimit_NegativeValueMeansUnlimited(t *testing.T) {
	qs, s := newTestQuotaService(t)
	ctx := context.Background()

	def := seedLimit(t, s, "max_agents_per_project", 10)

	userID := uuid.NewString()
	require.NoError(t, s.CreateUser(ctx, &store.User{ID: userID, Email: userID + "@test.com", DisplayName: "Test User", Role: "member", Status: "active"}))

	// Negative value also means unlimited.
	seedBinding(t, s, def.ID, store.EntitlementSubjectUser, userID, store.QuotaScopeSystem, "", -1)

	effective, err := qs.ResolveEffectiveLimit(ctx, def.ID, userID, store.QuotaScopeSystem, "")
	require.NoError(t, err)
	assert.Equal(t, int64(0), effective, "negative binding value should be treated as unlimited")
}

func TestCheckAndReserve_UnlimitedBindingSkipsEnforcement(t *testing.T) {
	qs, s := newTestQuotaService(t)
	ctx := context.Background()

	// Default is 2, but user has unlimited binding.
	def := seedLimit(t, s, "max_test_unlimited", 2)

	userID := "user-unlimited"
	seedBinding(t, s, def.ID, store.EntitlementSubjectUser, userID, store.QuotaScopeSystem, "", 0)

	// Should be able to create many resources without hitting quota.
	for i := 0; i < 20; i++ {
		err := qs.CheckAndReserve(ctx, "max_test_unlimited", userID, "project", "p1", fmt.Sprintf("r-%d", i))
		require.NoError(t, err, "reservation %d should succeed with unlimited binding", i)
	}
}

// ===========================================================================
// Lock contention tests
// ===========================================================================

func TestCheckAndReserve_LockContentionReturnsRetryableError(t *testing.T) {
	qs, s := newTestQuotaService(t)
	ctx := context.Background()

	seedLimit(t, s, "max_test_lock", 10)

	// Get the underlying locking wrapper to pre-acquire the lock.
	wrapper := qs.store.(*lockingStoreWrapper)
	objID := store.StableProjectHash("p1")
	acquired, release, err := wrapper.TryAdvisoryLockObject(ctx, store.LockQuotaEnforcement, objID)
	require.NoError(t, err)
	require.True(t, acquired, "should acquire lock in test setup")
	defer func() { _ = release() }()

	// Now CheckAndReserve should fail with ErrQuotaLockContention.
	err = qs.CheckAndReserve(ctx, "max_test_lock", "user-1", "project", "p1", "r1")
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrQuotaLockContention, "should return ErrQuotaLockContention when lock is held")
}

// ===========================================================================
// Scope matching tests
// ===========================================================================

func TestCheckAndReserve_ScopesAreIndependent(t *testing.T) {
	qs, s := newTestQuotaService(t)
	ctx := context.Background()

	seedLimit(t, s, "max_agents_per_project", 2)

	// Fill quota for project-1.
	require.NoError(t, qs.CheckAndReserve(ctx, "max_agents_per_project", "user-1", "project", "project-1", "a1"))
	require.NoError(t, qs.CheckAndReserve(ctx, "max_agents_per_project", "user-1", "project", "project-1", "a2"))
	require.ErrorIs(t, qs.CheckAndReserve(ctx, "max_agents_per_project", "user-1", "project", "project-1", "a3"), store.ErrQuotaExceeded)

	// project-2 should still have quota available.
	require.NoError(t, qs.CheckAndReserve(ctx, "max_agents_per_project", "user-1", "project", "project-2", "a4"))
}
