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
	"sync"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// CachedCallerPermissionChecker wraps a CallerPermissionChecker with an
// asymmetric-TTL decision cache. It is a performance control, not a security
// one — see architect-decisions.md Q6.
//
// Cache key: (callerPrincipalID, targetSAEmail, permission).
// Permission is currently always PermissionActAs but keyed for future-proofing.
//
// The asymmetry is deliberate: a user who just granted actAs should not wait
// 60 seconds for the denial to clear, while a burst of agent creations by the
// same orchestrator should not generate N identical PT calls.
type CachedCallerPermissionChecker struct {
	inner    store.CallerPermissionChecker
	allowTTL time.Duration // 60s default
	denyTTL  time.Duration // 10s default

	// mu guards entries. The cache is intentionally simple — a map with
	// explicit expiry rather than an LRU, because the key space is bounded
	// by (callers × SAs) and PT's own quota pressure keeps the call rate low.
	mu      sync.RWMutex
	entries map[actAsCacheKey]actAsCacheEntry

	// now is the time source, overridable in tests.
	now func() time.Time
}

type actAsCacheKey struct {
	PrincipalID string // caller.GCPPrincipalID()
	SAEmail     string // targetSA.Email
	Permission  string // store.PermissionActAs
}

type actAsCacheEntry struct {
	Result    store.ActAsResult
	ExpiresAt time.Time
}

// NewCachedCallerPermissionChecker wraps an inner checker with asymmetric TTL
// caching. allowTTL and denyTTL control how long allow and deny results are
// cached, respectively. Indeterminate results and errors are never cached.
func NewCachedCallerPermissionChecker(
	inner store.CallerPermissionChecker,
	allowTTL, denyTTL time.Duration,
) *CachedCallerPermissionChecker {
	return &CachedCallerPermissionChecker{
		inner:    inner,
		allowTTL: allowTTL,
		denyTTL:  denyTTL,
		entries:  make(map[actAsCacheKey]actAsCacheEntry),
		now:      time.Now,
	}
}

// CanActAs implements store.CallerPermissionChecker.
func (c *CachedCallerPermissionChecker) CanActAs(
	ctx context.Context,
	caller store.Principal,
	targetSA *store.GCPServiceAccount,
) (store.ActAsResult, error) {
	key := actAsCacheKey{
		PrincipalID: caller.GCPPrincipalID(),
		SAEmail:     targetSA.Email,
		Permission:  store.PermissionActAs,
	}

	// Check cache under read lock.
	now := c.now()
	c.mu.RLock()
	if entry, ok := c.entries[key]; ok && now.Before(entry.ExpiresAt) {
		c.mu.RUnlock()
		return entry.Result, nil
	}
	c.mu.RUnlock()

	// Cache miss or expired — call inner.
	result, err := c.inner.CanActAs(ctx, caller, targetSA)

	// Cache the result only for definitive outcomes without errors.
	// Indeterminate means the check could not reach an answer. Caching
	// "I don't know" turns a transient failure into a TTL-length outage.
	// Errors are similarly transient and must not be cached.
	if err == nil {
		var ttl time.Duration
		switch result.Outcome {
		case store.ActAsAllowed:
			ttl = c.allowTTL
		case store.ActAsDenied:
			ttl = c.denyTTL
		default:
			// ActAsIndeterminate — do NOT cache.
		}

		if ttl > 0 {
			c.mu.Lock()
			c.entries[key] = actAsCacheEntry{
				Result:    result,
				ExpiresAt: c.now().Add(ttl),
			}
			c.mu.Unlock()
		}
	}

	return result, err
}

// InvalidateForSA removes all cache entries for a specific target SA.
// Called on SA delete and on Hub-initiated IAM policy changes.
func (c *CachedCallerPermissionChecker) InvalidateForSA(saEmail string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for key := range c.entries {
		if key.SAEmail == saEmail {
			delete(c.entries, key)
		}
	}
}

// InvalidateAll clears the entire cache. Used in tests and for config changes.
func (c *CachedCallerPermissionChecker) InvalidateAll() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.entries = make(map[actAsCacheKey]actAsCacheEntry)
}

// Compile-time assertion that the cached checker satisfies the interface.
var _ store.CallerPermissionChecker = (*CachedCallerPermissionChecker)(nil)
