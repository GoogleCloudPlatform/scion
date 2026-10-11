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
	"sync"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// countingChecker wraps a FakeCallerPermissionChecker and tracks call counts
// per target SA email.
type countingChecker struct {
	mu    sync.Mutex
	inner *store.FakeCallerPermissionChecker
	calls int
}

func newCountingChecker() *countingChecker {
	return &countingChecker{
		inner: store.NewFakeCallerPermissionChecker(),
	}
}

func (c *countingChecker) CanActAs(ctx context.Context, caller store.Principal, targetSA *store.GCPServiceAccount) (store.ActAsResult, error) {
	c.mu.Lock()
	c.calls++
	c.mu.Unlock()
	return c.inner.CanActAs(ctx, caller, targetSA)
}

func (c *countingChecker) callCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.calls
}

var (
	cacheCaller = store.Principal{
		Kind:                store.PrincipalAgent,
		ID:                  "agent-1",
		ServiceAccountEmail: "caller@proj.iam.gserviceaccount.com",
	}

	cacheCaller2 = store.Principal{
		Kind:                store.PrincipalAgent,
		ID:                  "agent-2",
		ServiceAccountEmail: "caller2@proj.iam.gserviceaccount.com",
	}

	cacheTargetSA = &store.GCPServiceAccount{
		ID:        "sa-1",
		Email:     "target@proj.iam.gserviceaccount.com",
		ProjectID: "proj",
	}

	cacheTargetSA2 = &store.GCPServiceAccount{
		ID:        "sa-2",
		Email:     "target2@proj.iam.gserviceaccount.com",
		ProjectID: "proj",
	}
)

func TestCachedChecker_MissCallsInner(t *testing.T) {
	inner := newCountingChecker()
	inner.inner.AllowTarget(cacheTargetSA.Email)

	cached := NewCachedCallerPermissionChecker(inner, 60*time.Second, 10*time.Second)

	result, err := cached.CanActAs(context.Background(), cacheCaller, cacheTargetSA)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Outcome != store.ActAsAllowed {
		t.Errorf("expected ActAsAllowed, got %v", result.Outcome)
	}
	if inner.callCount() != 1 {
		t.Errorf("expected 1 call to inner, got %d", inner.callCount())
	}
}

func TestCachedChecker_HitAllowWithinTTL(t *testing.T) {
	inner := newCountingChecker()
	inner.inner.AllowTarget(cacheTargetSA.Email)

	now := time.Now()
	cached := NewCachedCallerPermissionChecker(inner, 60*time.Second, 10*time.Second)
	cached.now = func() time.Time { return now }

	// First call — cache miss.
	_, _ = cached.CanActAs(context.Background(), cacheCaller, cacheTargetSA)

	// Second call — cache hit, inner should NOT be called again.
	result, err := cached.CanActAs(context.Background(), cacheCaller, cacheTargetSA)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Outcome != store.ActAsAllowed {
		t.Errorf("expected cached ActAsAllowed, got %v", result.Outcome)
	}
	if inner.callCount() != 1 {
		t.Errorf("expected 1 call to inner (cached hit), got %d", inner.callCount())
	}
}

func TestCachedChecker_HitDenyWithinTTL(t *testing.T) {
	inner := newCountingChecker()
	inner.inner.DenyTarget(cacheTargetSA.Email, "no actAs")

	now := time.Now()
	cached := NewCachedCallerPermissionChecker(inner, 60*time.Second, 10*time.Second)
	cached.now = func() time.Time { return now }

	// First call — cache miss.
	_, _ = cached.CanActAs(context.Background(), cacheCaller, cacheTargetSA)

	// Second call — cache hit.
	result, _ := cached.CanActAs(context.Background(), cacheCaller, cacheTargetSA)
	if result.Outcome != store.ActAsDenied {
		t.Errorf("expected cached ActAsDenied, got %v", result.Outcome)
	}
	if inner.callCount() != 1 {
		t.Errorf("expected 1 call to inner (cached deny), got %d", inner.callCount())
	}
}

func TestCachedChecker_AllowExpired(t *testing.T) {
	inner := newCountingChecker()
	inner.inner.AllowTarget(cacheTargetSA.Email)

	now := time.Now()
	cached := NewCachedCallerPermissionChecker(inner, 60*time.Second, 10*time.Second)
	cached.now = func() time.Time { return now }

	// First call — cache miss.
	_, _ = cached.CanActAs(context.Background(), cacheCaller, cacheTargetSA)

	// Advance past allowTTL.
	now = now.Add(61 * time.Second)
	cached.now = func() time.Time { return now }

	// Should call inner again.
	result, _ := cached.CanActAs(context.Background(), cacheCaller, cacheTargetSA)
	if result.Outcome != store.ActAsAllowed {
		t.Errorf("expected ActAsAllowed, got %v", result.Outcome)
	}
	if inner.callCount() != 2 {
		t.Errorf("expected 2 calls after expiry, got %d", inner.callCount())
	}
}

func TestCachedChecker_AllowTTLGreaterThanDenyTTL(t *testing.T) {
	inner := newCountingChecker()
	inner.inner.AllowTarget(cacheTargetSA.Email)
	inner.inner.DenyTarget(cacheTargetSA2.Email, "denied")

	now := time.Now()
	cached := NewCachedCallerPermissionChecker(inner, 60*time.Second, 10*time.Second)
	cached.now = func() time.Time { return now }

	// Populate cache for both.
	_, _ = cached.CanActAs(context.Background(), cacheCaller, cacheTargetSA)
	_, _ = cached.CanActAs(context.Background(), cacheCaller, cacheTargetSA2)
	if inner.callCount() != 2 {
		t.Fatalf("expected 2 initial calls, got %d", inner.callCount())
	}

	// Advance past denyTTL but within allowTTL.
	now = now.Add(15 * time.Second)
	cached.now = func() time.Time { return now }

	// Allow should still be cached.
	r1, _ := cached.CanActAs(context.Background(), cacheCaller, cacheTargetSA)
	if r1.Outcome != store.ActAsAllowed {
		t.Errorf("allow should still be cached, got %v", r1.Outcome)
	}

	// Deny should be expired — inner called again.
	_, _ = cached.CanActAs(context.Background(), cacheCaller, cacheTargetSA2)

	if inner.callCount() != 3 {
		t.Errorf("expected 3 calls (deny expired but allow cached), got %d", inner.callCount())
	}
}

func TestCachedChecker_IndeterminateNotCached(t *testing.T) {
	inner := newCountingChecker()
	inner.inner.IndeterminateTarget(cacheTargetSA.Email, "unknown group")

	cached := NewCachedCallerPermissionChecker(inner, 60*time.Second, 10*time.Second)

	// First call.
	result, _ := cached.CanActAs(context.Background(), cacheCaller, cacheTargetSA)
	if result.Outcome != store.ActAsIndeterminate {
		t.Errorf("expected ActAsIndeterminate, got %v", result.Outcome)
	}

	// Second call — inner should be called AGAIN because indeterminate is not cached.
	_, _ = cached.CanActAs(context.Background(), cacheCaller, cacheTargetSA)

	if inner.callCount() != 2 {
		t.Errorf("indeterminate should not be cached; expected 2 calls, got %d", inner.callCount())
	}
}

func TestCachedChecker_ErrorNotCached(t *testing.T) {
	// A checker that returns an error on every call.
	errChecker := &errorChecker{err: errors.New("connection refused")}
	cached := NewCachedCallerPermissionChecker(errChecker, 60*time.Second, 10*time.Second)

	// First call.
	_, err := cached.CanActAs(context.Background(), cacheCaller, cacheTargetSA)
	if err == nil {
		t.Fatal("expected error")
	}

	// Second call — should call inner again because errors are not cached.
	_, err = cached.CanActAs(context.Background(), cacheCaller, cacheTargetSA)
	if err == nil {
		t.Fatal("expected error on second call")
	}

	if errChecker.calls != 2 {
		t.Errorf("errors should not be cached; expected 2 calls, got %d", errChecker.calls)
	}
}

func TestCachedChecker_InvalidateForSA(t *testing.T) {
	inner := newCountingChecker()
	inner.inner.AllowTarget(cacheTargetSA.Email)
	inner.inner.AllowTarget(cacheTargetSA2.Email)

	cached := NewCachedCallerPermissionChecker(inner, 60*time.Second, 10*time.Second)

	// Populate cache.
	_, _ = cached.CanActAs(context.Background(), cacheCaller, cacheTargetSA)
	_, _ = cached.CanActAs(context.Background(), cacheCaller, cacheTargetSA2)
	if inner.callCount() != 2 {
		t.Fatalf("setup: expected 2 calls, got %d", inner.callCount())
	}

	// Invalidate only cacheTargetSA.
	cached.InvalidateForSA(cacheTargetSA.Email)

	// cacheTargetSA should call inner again.
	_, _ = cached.CanActAs(context.Background(), cacheCaller, cacheTargetSA)
	if inner.callCount() != 3 {
		t.Errorf("expected inner called after invalidation, got %d calls", inner.callCount())
	}

	// cacheTargetSA2 should still be cached.
	_, _ = cached.CanActAs(context.Background(), cacheCaller, cacheTargetSA2)
	if inner.callCount() != 3 {
		t.Errorf("other SA should still be cached, got %d calls", inner.callCount())
	}
}

func TestCachedChecker_InvalidateAll(t *testing.T) {
	inner := newCountingChecker()
	inner.inner.AllowTarget(cacheTargetSA.Email)
	inner.inner.AllowTarget(cacheTargetSA2.Email)

	cached := NewCachedCallerPermissionChecker(inner, 60*time.Second, 10*time.Second)

	// Populate cache.
	_, _ = cached.CanActAs(context.Background(), cacheCaller, cacheTargetSA)
	_, _ = cached.CanActAs(context.Background(), cacheCaller, cacheTargetSA2)
	if inner.callCount() != 2 {
		t.Fatalf("setup: expected 2 calls, got %d", inner.callCount())
	}

	// Invalidate all.
	cached.InvalidateAll()

	// Both should call inner again.
	_, _ = cached.CanActAs(context.Background(), cacheCaller, cacheTargetSA)
	_, _ = cached.CanActAs(context.Background(), cacheCaller, cacheTargetSA2)
	if inner.callCount() != 4 {
		t.Errorf("expected 4 total calls after InvalidateAll, got %d", inner.callCount())
	}
}

func TestCachedChecker_DifferentCallersSameSA(t *testing.T) {
	inner := newCountingChecker()
	inner.inner.AllowTarget(cacheTargetSA.Email)

	cached := NewCachedCallerPermissionChecker(inner, 60*time.Second, 10*time.Second)

	// caller1 queries.
	_, _ = cached.CanActAs(context.Background(), cacheCaller, cacheTargetSA)
	// caller2 queries the same SA — should be a separate cache entry.
	_, _ = cached.CanActAs(context.Background(), cacheCaller2, cacheTargetSA)

	if inner.callCount() != 2 {
		t.Errorf("different callers should have separate cache entries; expected 2, got %d", inner.callCount())
	}

	// caller1 again — should be cached.
	_, _ = cached.CanActAs(context.Background(), cacheCaller, cacheTargetSA)
	if inner.callCount() != 2 {
		t.Errorf("caller1 repeat should be cached; expected 2, got %d", inner.callCount())
	}
}

// errorChecker is a CallerPermissionChecker that always returns an error.
type errorChecker struct {
	mu    sync.Mutex
	calls int
	err   error
}

func (e *errorChecker) CanActAs(_ context.Context, _ store.Principal, _ *store.GCPServiceAccount) (store.ActAsResult, error) {
	e.mu.Lock()
	e.calls++
	e.mu.Unlock()
	return store.ActAsResult{
		Outcome:   store.ActAsIndeterminate,
		Mechanism: "test-error",
		Reason:    "test error",
	}, e.err
}
