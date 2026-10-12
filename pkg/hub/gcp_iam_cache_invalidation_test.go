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
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// TestInvalidateActAsCache_NonCachingChecker verifies that the server helper
// is a no-op when the configured checkers are not CachedCallerPermissionCheckers
// (e.g. DisabledCallerPermissionChecker when gcpIamCheckMode=off).
func TestInvalidateActAsCache_NonCachingChecker(t *testing.T) {
	srv := &Server{}
	srv.saAssignChecker = store.NewDisabledCallerPermissionChecker()
	srv.hookIdentityChecker = store.NewDisabledCallerPermissionChecker()

	// Must not panic or error.
	srv.invalidateActAsCache("anything@proj.iam.gserviceaccount.com")
}

// TestInvalidateActAsCache_InvalidatesBothCheckers verifies that the helper
// invalidates entries in both the SA-assign and hook-identity checkers.
func TestInvalidateActAsCache_InvalidatesBothCheckers(t *testing.T) {
	inner1 := newCountingChecker()
	inner1.inner.AllowTarget(cacheTargetSA.Email)

	inner2 := newCountingChecker()
	inner2.inner.AllowTarget(cacheTargetSA.Email)

	assignCached := NewCachedCallerPermissionChecker(inner1, 60*time.Second, 10*time.Second)
	hookCached := NewCachedCallerPermissionChecker(inner2, 60*time.Second, 10*time.Second)

	srv := &Server{}
	srv.saAssignChecker = assignCached
	srv.hookIdentityChecker = hookCached

	ctx := context.Background()

	// Populate both caches.
	_, _ = assignCached.CanActAs(ctx, cacheCaller, cacheTargetSA)
	_, _ = hookCached.CanActAs(ctx, cacheCaller, cacheTargetSA)
	if inner1.callCount() != 1 || inner2.callCount() != 1 {
		t.Fatalf("setup: expected 1 call each, got assign=%d hook=%d",
			inner1.callCount(), inner2.callCount())
	}

	// Invalidate via the server helper.
	srv.invalidateActAsCache(cacheTargetSA.Email)

	// Both caches should be cleared — next call should go to inner.
	_, _ = assignCached.CanActAs(ctx, cacheCaller, cacheTargetSA)
	_, _ = hookCached.CanActAs(ctx, cacheCaller, cacheTargetSA)
	if inner1.callCount() != 2 {
		t.Errorf("assign checker: expected inner called after invalidation, got %d", inner1.callCount())
	}
	if inner2.callCount() != 2 {
		t.Errorf("hook checker: expected inner called after invalidation, got %d", inner2.callCount())
	}
}

// TestInvalidateActAsCache_OnlyTargetEmail verifies that invalidation is
// scoped to the specific SA email — other cached entries are preserved.
func TestInvalidateActAsCache_OnlyTargetEmail(t *testing.T) {
	inner := newCountingChecker()
	inner.inner.AllowTarget(cacheTargetSA.Email)
	inner.inner.AllowTarget(cacheTargetSA2.Email)

	cached := NewCachedCallerPermissionChecker(inner, 60*time.Second, 10*time.Second)

	srv := &Server{}
	srv.saAssignChecker = cached
	srv.hookIdentityChecker = store.NewDisabledCallerPermissionChecker()

	ctx := context.Background()

	// Populate cache for both SAs.
	_, _ = cached.CanActAs(ctx, cacheCaller, cacheTargetSA)
	_, _ = cached.CanActAs(ctx, cacheCaller, cacheTargetSA2)
	if inner.callCount() != 2 {
		t.Fatalf("setup: expected 2 calls, got %d", inner.callCount())
	}

	// Invalidate only cacheTargetSA.
	srv.invalidateActAsCache(cacheTargetSA.Email)

	// cacheTargetSA should call inner again.
	_, _ = cached.CanActAs(ctx, cacheCaller, cacheTargetSA)
	if inner.callCount() != 3 {
		t.Errorf("invalidated SA should call inner, got %d calls", inner.callCount())
	}

	// cacheTargetSA2 should still be cached.
	_, _ = cached.CanActAs(ctx, cacheCaller, cacheTargetSA2)
	if inner.callCount() != 3 {
		t.Errorf("other SA should still be cached, got %d calls", inner.callCount())
	}
}

// TestInvalidateActAsCache_MixedCheckerTypes verifies that the helper works
// when one checker is cached and the other is not.
func TestInvalidateActAsCache_MixedCheckerTypes(t *testing.T) {
	inner := newCountingChecker()
	inner.inner.AllowTarget(cacheTargetSA.Email)

	cached := NewCachedCallerPermissionChecker(inner, 60*time.Second, 10*time.Second)

	srv := &Server{}
	srv.saAssignChecker = cached
	srv.hookIdentityChecker = store.NewDisabledCallerPermissionChecker() // not a cached checker

	ctx := context.Background()

	// Populate cache.
	_, _ = cached.CanActAs(ctx, cacheCaller, cacheTargetSA)
	if inner.callCount() != 1 {
		t.Fatalf("setup: expected 1 call, got %d", inner.callCount())
	}

	// Must not panic on the disabled hook checker.
	srv.invalidateActAsCache(cacheTargetSA.Email)

	// Cached assign checker should be invalidated.
	_, _ = cached.CanActAs(ctx, cacheCaller, cacheTargetSA)
	if inner.callCount() != 2 {
		t.Errorf("expected inner called after invalidation, got %d", inner.callCount())
	}
}

// TestSetSAAssignChecker_InvalidatesOld verifies that replacing the checker
// drains the old cached checker's entries.
func TestSetSAAssignChecker_InvalidatesOld(t *testing.T) {
	inner := newCountingChecker()
	inner.inner.AllowTarget(cacheTargetSA.Email)

	oldCached := NewCachedCallerPermissionChecker(inner, 60*time.Second, 10*time.Second)

	srv := &Server{}
	srv.saAssignChecker = oldCached

	ctx := context.Background()

	// Populate the old cache.
	_, _ = oldCached.CanActAs(ctx, cacheCaller, cacheTargetSA)
	if inner.callCount() != 1 {
		t.Fatalf("setup: expected 1 call, got %d", inner.callCount())
	}

	// Replace the checker.
	newCached := NewCachedCallerPermissionChecker(inner, 60*time.Second, 10*time.Second)
	srv.SetSAAssignChecker(newCached)

	// Old cache should have been cleared — verify by calling it directly.
	_, _ = oldCached.CanActAs(ctx, cacheCaller, cacheTargetSA)
	if inner.callCount() != 2 {
		t.Errorf("old checker should have been invalidated, got %d calls", inner.callCount())
	}
}

// TestSetHookIdentityChecker_InvalidatesOld verifies that replacing the hook
// checker drains the old cached checker's entries.
func TestSetHookIdentityChecker_InvalidatesOld(t *testing.T) {
	inner := newCountingChecker()
	inner.inner.AllowTarget(cacheTargetSA.Email)

	oldCached := NewCachedCallerPermissionChecker(inner, 60*time.Second, 10*time.Second)

	srv := &Server{}
	srv.hookIdentityChecker = oldCached

	ctx := context.Background()

	// Populate the old cache.
	_, _ = oldCached.CanActAs(ctx, cacheCaller, cacheTargetSA)
	if inner.callCount() != 1 {
		t.Fatalf("setup: expected 1 call, got %d", inner.callCount())
	}

	// Replace the checker.
	newCached := NewCachedCallerPermissionChecker(inner, 60*time.Second, 10*time.Second)
	srv.SetHookIdentityChecker(newCached)

	// Old cache should have been cleared.
	_, _ = oldCached.CanActAs(ctx, cacheCaller, cacheTargetSA)
	if inner.callCount() != 2 {
		t.Errorf("old checker should have been invalidated, got %d calls", inner.callCount())
	}
}

// TestSetSAAssignChecker_NonCachedOld verifies that replacing a non-cached
// checker (e.g. disabled) with a new one does not panic.
func TestSetSAAssignChecker_NonCachedOld(t *testing.T) {
	srv := &Server{}
	srv.saAssignChecker = store.NewDisabledCallerPermissionChecker()

	// Must not panic.
	newCached := NewCachedCallerPermissionChecker(
		newCountingChecker(), 60*time.Second, 10*time.Second)
	srv.SetSAAssignChecker(newCached)
}
