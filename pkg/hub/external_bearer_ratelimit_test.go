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
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// ---------------------------------------------------------------------------
// Review r1, Critical finding 1: the external-bearer rate limiter never had
// its stale entries cleaned up, so after maxEntries distinct client IPs it
// permanently refused every new one. Fixed by starting its cleanup goroutine
// in server.go's Start (StartBackgroundServices), alongside geExchangeRateLimiter's.
// ---------------------------------------------------------------------------

func newTestBearerRequest(remoteAddr string) *http.Request {
	req := httptest.NewRequest(http.MethodGet, "/api/v1/auth/me", nil)
	req.RemoteAddr = remoteAddr
	return req
}

// TestExternalBearerRateLimiter_CleanupAdmitsNewIPAfterMaxAge is the fix
// brief's probe (b): a full limiter, advanced past maxAge, admits a new IP —
// this is exactly what StartCleanup's background goroutine does on every
// tick.
func TestExternalBearerRateLimiter_CleanupAdmitsNewIPAfterMaxAge(t *testing.T) {
	limiter := newExternalBearerRateLimiter(nil)
	limiter.buckets.maxEntries = 2 // small, so filling it is cheap
	now := time.Now()
	limiter.buckets.nowFunc = func() time.Time { return now }

	if allowed, _ := limiter.Allow(newTestBearerRequest("198.51.100.1:1")); !allowed {
		t.Fatal("first IP should be allowed")
	}
	if allowed, _ := limiter.Allow(newTestBearerRequest("198.51.100.2:1")); !allowed {
		t.Fatal("second IP should be allowed")
	}
	// The limiter is now full (maxEntries=2): a third, never-seen IP is
	// refused outright, reproducing the reviewer's "after 10000 distinct
	// client IPs" scenario at a testable scale.
	if allowed, _ := limiter.Allow(newTestBearerRequest("198.51.100.3:1")); allowed {
		t.Fatal("third IP should be refused: the limiter is at capacity")
	}

	// Advance past maxAge and run Cleanup directly — this is exactly what
	// the background goroutine started by StartCleanup does periodically.
	limiter.buckets.Cleanup(now.Add(limiter.buckets.maxAge + time.Second))

	if allowed, _ := limiter.Allow(newTestBearerRequest("198.51.100.3:1")); !allowed {
		t.Error("the third IP should now be admitted: Cleanup must evict the stale entries and free capacity")
	}
}

// TestServer_ExternalBearerRateLimiter_CleanupRunsInBackground is the fix
// brief's probe (a)'s "cleanup is started" half: proves StartBackgroundServices
// (called from Server.Start) actually invokes externalBearerRateLimiter.StartCleanup,
// not just that the field is non-nil. It shrinks the limiter's cleanup
// interval and capacity so the background goroutine's first tick is
// observable within the test's lifetime, instead of waiting on the
// production 5-minute interval.
func TestServer_ExternalBearerRateLimiter_CleanupRunsInBackground(t *testing.T) {
	s, err := newTestStore(":memory:")
	if err != nil {
		t.Fatalf("failed to create test store: %v", err)
	}

	cfg := DefaultServerConfig()
	srv, err := New(cfg, s)
	if err != nil {
		t.Fatalf("New() failed: %v", err)
	}
	t.Cleanup(func() { _ = srv.Shutdown(context.Background()) })

	if srv.externalBearerRateLimiter == nil {
		t.Fatal("expected srv.externalBearerRateLimiter to be set")
	}
	srv.externalBearerRateLimiter.buckets.cleanupInterval = 5 * time.Millisecond
	srv.externalBearerRateLimiter.buckets.maxEntries = 1
	srv.externalBearerRateLimiter.buckets.maxAge = 10 * time.Millisecond

	if allowed, _ := srv.externalBearerRateLimiter.Allow(newTestBearerRequest("198.51.100.9:1")); !allowed {
		t.Fatal("first IP should be allowed")
	}
	if allowed, _ := srv.externalBearerRateLimiter.Allow(newTestBearerRequest("198.51.100.10:1")); allowed {
		t.Fatal("second IP should be refused: the limiter is at capacity (maxEntries=1)")
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	srv.StartBackgroundServices(ctx) // this must start externalBearerRateLimiter.StartCleanup

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if allowed, _ := srv.externalBearerRateLimiter.Allow(newTestBearerRequest("198.51.100.10:1")); allowed {
			return // success: the background cleanup ran and evicted the aged-out entry
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("the second IP was still refused after 2s of waiting for background cleanup; " +
		"StartBackgroundServices does not appear to run externalBearerRateLimiter.StartCleanup")
}
