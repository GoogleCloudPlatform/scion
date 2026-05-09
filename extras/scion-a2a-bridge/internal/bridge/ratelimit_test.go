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

package bridge

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestRateLimiterAllowAndDeny(t *testing.T) {
	rl := NewRateLimiter(10, 2)

	if !rl.Allow("k1") {
		t.Error("first request should be allowed")
	}
	if !rl.Allow("k1") {
		t.Error("second request (within burst) should be allowed")
	}
	if rl.Allow("k1") {
		t.Error("third request should be denied (burst=2 exhausted)")
	}
}

func TestRateLimiterBurst(t *testing.T) {
	rl := NewRateLimiter(1, 5)

	for i := 0; i < 5; i++ {
		if !rl.Allow("burst") {
			t.Errorf("request %d should be allowed within burst", i+1)
		}
	}
	if rl.Allow("burst") {
		t.Error("request beyond burst should be denied")
	}
}

func TestRateLimiterPerKey(t *testing.T) {
	rl := NewRateLimiter(10, 1)

	if !rl.Allow("a") {
		t.Error("key 'a' first request should be allowed")
	}
	if rl.Allow("a") {
		t.Error("key 'a' second request should be denied")
	}

	if !rl.Allow("b") {
		t.Error("key 'b' should have its own bucket and be allowed")
	}
}

func TestRateLimitMiddlewareDisabled(t *testing.T) {
	called := false
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
	})

	handler := RateLimitMiddleware(inner, RateLimitConfig{Enabled: false})

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	handler.ServeHTTP(httptest.NewRecorder(), req)

	if !called {
		t.Error("inner handler should be called when rate limiting is disabled")
	}
}

func TestRateLimitMiddlewareRejects(t *testing.T) {
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	handler := RateLimitMiddleware(inner, RateLimitConfig{
		Enabled:        true,
		RequestsPerSec: 100,
		Burst:          1,
	})

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("X-API-Key", "client-1")

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Errorf("first request: status = %d, want 200", rec.Code)
	}

	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusTooManyRequests {
		t.Errorf("second request: status = %d, want 429", rec.Code)
	}
}

func TestRateLimitMiddlewareFallsBackToRemoteAddr(t *testing.T) {
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	handler := RateLimitMiddleware(inner, RateLimitConfig{
		Enabled:        true,
		RequestsPerSec: 100,
		Burst:          1,
	})

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	// No X-API-Key header — should key on RemoteAddr.

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Errorf("first request: status = %d, want 200", rec.Code)
	}

	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusTooManyRequests {
		t.Errorf("second request (same RemoteAddr): status = %d, want 429", rec.Code)
	}
}

func TestRateLimitMiddlewareDefaultRateAndBurst(t *testing.T) {
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	handler := RateLimitMiddleware(inner, RateLimitConfig{
		Enabled:        true,
		RequestsPerSec: 0,
		Burst:          0,
	})

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("X-API-Key", "default-test")

	// Defaults are rate=10, burst=20. Should allow at least 20 requests.
	for i := 0; i < 20; i++ {
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Errorf("request %d with defaults should be allowed, got %d", i+1, rec.Code)
		}
	}
}
