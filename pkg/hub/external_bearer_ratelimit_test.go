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
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// ---------------------------------------------------------------------------
// The external-bearer rate limiter's stale entries must be cleaned up, or
// after maxEntries distinct client IPs it
// permanently refuses every new one. Its cleanup goroutine is started
// in server.go's Start (StartBackgroundServices), alongside geExchangeRateLimiter's.
// ---------------------------------------------------------------------------

func newTestBearerRequest(remoteAddr string) *http.Request {
	req := httptest.NewRequest(http.MethodGet, "/api/v1/auth/me", nil)
	req.RemoteAddr = remoteAddr
	return req
}

// TestExternalBearerRateLimiter_CleanupAdmitsNewIPAfterMaxAge proves that
// a full limiter, advanced past maxAge, admits a new IP —
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
	// refused outright, reproducing the "after 10000 distinct
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

// TestServer_ExternalBearerRateLimiter_CleanupRunsInBackground proves
// StartBackgroundServices
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

// ---------------------------------------------------------------------------
// The limiter defaults (5 rps /
// burst 20) were not pinned: both of the tests
// in auth_external_bearer_access_token_test.go override rate/burst to small
// test values, so a change to the production constants (e.g. 500 rps / burst
// 200) would survive the whole suite.
// ---------------------------------------------------------------------------

func TestExternalBearerRateLimiter_DefaultsMatchDesign(t *testing.T) {
	limiter := newExternalBearerRateLimiter(nil)
	if limiter.buckets.rate != externalBearerRatePerSecond {
		t.Errorf("rate = %v, want %v (design §4.4: 5 rps)", limiter.buckets.rate, externalBearerRatePerSecond)
	}
	if limiter.buckets.burst != externalBearerBurst {
		t.Errorf("burst = %v, want %v (design §4.4: burst 20)", limiter.buckets.burst, externalBearerBurst)
	}
	if limiter.buckets.rate != 5.0 {
		t.Errorf("rate = %v, want the literal design value 5.0", limiter.buckets.rate)
	}
	if limiter.buckets.burst != 20 {
		t.Errorf("burst = %v, want the literal design value 20", limiter.buckets.burst)
	}
}

// TestExternalBearerRateLimiter_DefaultBurstExhaustion exercises the
// unmodified production defaults end to end (unlike the tests above, which
// shrink burst for speed): 20 requests from one IP succeed, the 21st is
// refused. This fails if the burst constant is ever widened (e.g. to 200)
// without a corresponding, deliberate test change.
func TestExternalBearerRateLimiter_DefaultBurstExhaustion(t *testing.T) {
	limiter := newExternalBearerRateLimiter(nil)

	for i := 0; i < externalBearerBurst; i++ {
		if allowed, _ := limiter.Allow(newTestBearerRequest("198.51.100.42:1")); !allowed {
			t.Fatalf("request %d (of %d burst) should be allowed", i+1, externalBearerBurst)
		}
	}
	if allowed, _ := limiter.Allow(newTestBearerRequest("198.51.100.42:1")); allowed {
		t.Errorf("request %d should be refused: the default burst (%d) is exhausted", externalBearerBurst+1, externalBearerBurst)
	}
}

// ---------------------------------------------------------------------------
// The limiter's X-Forwarded-For spoof
// resistance depends on its own wiring: it reuses geExchangeClientIP, and
// server.go passes the same cfg.TrustedProxies the middleware itself uses.
// If the constructor were changed to trust every
// proxy (parseTrustedProxies([]string{"0.0.0.0/0", "::/0"})), any untrusted
// peer could
// rotate X-Forwarded-For to get a fresh bucket per request and bypass the
// rate limit entirely, reviving the "make the Hub call Google for every
// random string" amplification the limiter exists to stop.
// ---------------------------------------------------------------------------

func TestExternalBearerRateLimiter_HonoursXForwardedForOnlyFromTrustedProxy(t *testing.T) {
	limiter := newExternalBearerRateLimiter([]string{"10.0.0.0/8"})

	// From an UNTRUSTED peer, X-Forwarded-For must be ignored — every
	// request is keyed by the peer address itself, so a rotating XFF header
	// cannot manufacture a fresh bucket per request.
	for i := 0; i < externalBearerBurst; i++ {
		req := httptest.NewRequest(http.MethodGet, "/api/v1/auth/me", nil)
		req.RemoteAddr = "203.0.113.5:1"
		req.Header.Set("X-Forwarded-For", fmt.Sprintf("198.51.100.%d", i))
		if allowed, _ := limiter.Allow(req); !allowed {
			t.Fatalf("request %d (of %d burst) from the untrusted peer should be allowed", i+1, externalBearerBurst)
		}
	}
	beyondBurst := httptest.NewRequest(http.MethodGet, "/api/v1/auth/me", nil)
	beyondBurst.RemoteAddr = "203.0.113.5:1"
	beyondBurst.Header.Set("X-Forwarded-For", "198.51.100.250")
	if allowed, _ := limiter.Allow(beyondBurst); allowed {
		t.Error("a request beyond the burst from the untrusted peer must be refused, even with a fresh X-Forwarded-For each time (spoof resistance)")
	}

	// From a TRUSTED peer, X-Forwarded-For IS honoured, and the bucket is
	// keyed by the XFF client, not the shared trusted peer address: two
	// distinct XFF clients behind the same trusted proxy each get their own
	// full burst.
	for _, client := range []string{"198.51.100.10", "198.51.100.20"} {
		for i := 0; i < externalBearerBurst; i++ {
			req := httptest.NewRequest(http.MethodGet, "/api/v1/auth/me", nil)
			req.RemoteAddr = "10.0.0.1:1"
			req.Header.Set("X-Forwarded-For", client)
			if allowed, _ := limiter.Allow(req); !allowed {
				t.Fatalf("client %s: request %d (of %d burst) via the trusted proxy should be allowed", client, i+1, externalBearerBurst)
			}
		}
	}
}
