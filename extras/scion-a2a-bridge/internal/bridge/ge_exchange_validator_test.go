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
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// fakeHubExchange returns an httptest.Server that mimics the Hub
// POST /api/v1/auth/integrations/google/exchange endpoint.
func fakeHubExchange(t *testing.T, handler http.HandlerFunc) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/auth/integrations/google/exchange" {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		handler(w, r)
	}))
}

func writeExchangeResponse(w http.ResponseWriter, user geExchangeUser, token string, expiresAt, upstreamExpiresAt time.Time) {
	resp := geExchangeResponse{
		AccessToken: token,
		TokenType:   "Bearer",
		User:        &user,
	}
	if !expiresAt.IsZero() {
		resp.ExpiresAt = expiresAt.Format(time.RFC3339)
	}
	if !upstreamExpiresAt.IsZero() {
		resp.UpstreamExpiresAt = upstreamExpiresAt.Format(time.RFC3339)
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(resp)
}

// ---------------------------------------------------------------------------
// Tests
// ---------------------------------------------------------------------------

func TestGEExchangeValidator_ValidExchange(t *testing.T) {
	hub := fakeHubExchange(t, func(w http.ResponseWriter, r *http.Request) {
		// Verify request body.
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("read body: %v", err)
		}
		var req map[string]string
		if err := json.Unmarshal(body, &req); err != nil {
			t.Fatalf("unmarshal body: %v", err)
		}
		if req["credentialType"] != "id_token" {
			t.Errorf("credentialType = %q, want id_token", req["credentialType"])
		}
		if req["credential"] == "" {
			t.Error("credential is empty")
		}

		writeExchangeResponse(w,
			geExchangeUser{ID: "user-1", Email: "alice@gmail.com", DisplayName: "Alice", Role: "user"},
			"hub-token-abc",
			time.Now().Add(5*time.Minute),
			time.Now().Add(55*time.Minute),
		)
	})
	defer hub.Close()

	v := NewGEExchangeValidator(hub.URL, GEExchangeConfig{
		CredentialType: "id_token",
		CacheTTL:       60 * time.Second,
	}, testLogger())

	id, err := v.Validate(context.Background(), "google-id-token-xxx")
	if err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if id.UserID != "user-1" {
		t.Errorf("UserID = %q, want user-1", id.UserID)
	}
	if id.Email != "alice@gmail.com" {
		t.Errorf("Email = %q, want alice@gmail.com", id.Email)
	}
	if id.RawToken != "hub-token-abc" {
		t.Errorf("RawToken = %q, want hub-token-abc", id.RawToken)
	}
	if id.TokenType != "ge_exchange" {
		t.Errorf("TokenType = %q, want ge_exchange", id.TokenType)
	}
	if id.Role != "user" {
		t.Errorf("Role = %q, want user", id.Role)
	}
}

func TestGEExchangeValidator_AccessTokenType(t *testing.T) {
	hub := fakeHubExchange(t, func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var req map[string]string
		json.Unmarshal(body, &req)
		if req["credentialType"] != "access_token" {
			t.Errorf("credentialType = %q, want access_token", req["credentialType"])
		}
		writeExchangeResponse(w,
			geExchangeUser{ID: "user-2", Email: "bob@gmail.com", Role: "user"},
			"hub-token-def",
			time.Now().Add(5*time.Minute),
			time.Now().Add(30*time.Minute),
		)
	})
	defer hub.Close()

	v := NewGEExchangeValidator(hub.URL, GEExchangeConfig{
		CredentialType: "access_token",
		CacheTTL:       60 * time.Second,
	}, testLogger())

	id, err := v.Validate(context.Background(), "ya29.access-token-xxx")
	if err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if id.UserID != "user-2" {
		t.Errorf("UserID = %q, want user-2", id.UserID)
	}
}

func TestGEExchangeValidator_CacheHit(t *testing.T) {
	callCount := int32(0)
	hub := fakeHubExchange(t, func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&callCount, 1)
		writeExchangeResponse(w,
			geExchangeUser{ID: "user-cached", Email: "cached@gmail.com", Role: "user"},
			"hub-token-cached",
			time.Now().Add(5*time.Minute),
			time.Now().Add(55*time.Minute),
		)
	})
	defer hub.Close()

	v := NewGEExchangeValidator(hub.URL, GEExchangeConfig{
		CredentialType: "id_token",
		CacheTTL:       60 * time.Second,
	}, testLogger())

	ctx := context.Background()

	// First call: cache miss → Hub call.
	id1, err := v.Validate(ctx, "cached-cred")
	if err != nil {
		t.Fatalf("first: %v", err)
	}
	if c := atomic.LoadInt32(&callCount); c != 1 {
		t.Fatalf("after first: callCount=%d, want 1", c)
	}

	// Second call: cache hit → no Hub call.
	id2, err := v.Validate(ctx, "cached-cred")
	if err != nil {
		t.Fatalf("cached: %v", err)
	}
	if c := atomic.LoadInt32(&callCount); c != 1 {
		t.Fatalf("after cached: callCount=%d, want 1", c)
	}

	// Same identity returned.
	if id1.UserID != id2.UserID {
		t.Errorf("cache returned different user: %q vs %q", id1.UserID, id2.UserID)
	}
}

func TestGEExchangeValidator_CacheExpiry(t *testing.T) {
	callCount := int32(0)
	hub := fakeHubExchange(t, func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&callCount, 1)
		writeExchangeResponse(w,
			geExchangeUser{ID: "user-expiry", Email: "expiry@gmail.com", Role: "user"},
			"hub-token-expiry",
			time.Now().Add(5*time.Minute),
			time.Now().Add(55*time.Minute),
		)
	})
	defer hub.Close()

	v := NewGEExchangeValidator(hub.URL, GEExchangeConfig{
		CredentialType: "id_token",
		CacheTTL:       50 * time.Millisecond, // Very short for testing.
	}, testLogger())

	ctx := context.Background()

	// First call.
	_, err := v.Validate(ctx, "expiry-cred")
	if err != nil {
		t.Fatalf("first: %v", err)
	}
	if c := atomic.LoadInt32(&callCount); c != 1 {
		t.Fatalf("after first: callCount=%d, want 1", c)
	}

	// Wait for TTL expiry.
	time.Sleep(100 * time.Millisecond)

	// Second call: cache expired → new Hub call.
	_, err = v.Validate(ctx, "expiry-cred")
	if err != nil {
		t.Fatalf("after expiry: %v", err)
	}
	if c := atomic.LoadInt32(&callCount); c != 2 {
		t.Fatalf("after expiry: callCount=%d, want 2", c)
	}
}

func TestGEExchangeValidator_CacheTTLCappedByHubExpiry(t *testing.T) {
	// Hub returns token expiring in 1 second (second-precision RFC3339),
	// configured TTL is 60s. Cache should use the Hub expiry (shorter).
	callCount := int32(0)
	hub := fakeHubExchange(t, func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&callCount, 1)
		writeExchangeResponse(w,
			geExchangeUser{ID: "user-cap", Email: "cap@gmail.com", Role: "user"},
			"hub-token-capped",
			time.Now().Truncate(time.Second).Add(2*time.Second), // 2s Hub expiry (second-precision safe).
			time.Now().Add(55*time.Minute),
		)
	})
	defer hub.Close()

	v := NewGEExchangeValidator(hub.URL, GEExchangeConfig{
		CredentialType: "id_token",
		CacheTTL:       60 * time.Second,
	}, testLogger())

	ctx := context.Background()

	_, err := v.Validate(ctx, "cap-cred")
	if err != nil {
		t.Fatalf("first: %v", err)
	}

	// Wait for Hub expiry to pass (but well within configured TTL).
	time.Sleep(2500 * time.Millisecond)

	_, err = v.Validate(ctx, "cap-cred")
	if err != nil {
		t.Fatalf("after hub-expiry: %v", err)
	}

	// Should have made a second Hub call because cache was capped by Hub expiry.
	if c := atomic.LoadInt32(&callCount); c != 2 {
		t.Fatalf("callCount=%d, want 2 (capped by Hub expiry)", c)
	}
}

func TestGEExchangeValidator_CacheTTLCappedByUpstreamExpiry(t *testing.T) {
	callCount := int32(0)
	hub := fakeHubExchange(t, func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&callCount, 1)
		writeExchangeResponse(w,
			geExchangeUser{ID: "user-up", Email: "up@gmail.com", Role: "user"},
			"hub-token-up",
			time.Now().Add(55*time.Minute),
			time.Now().Truncate(time.Second).Add(2*time.Second), // 2s upstream expiry (second-precision safe).
		)
	})
	defer hub.Close()

	v := NewGEExchangeValidator(hub.URL, GEExchangeConfig{
		CredentialType: "id_token",
		CacheTTL:       60 * time.Second,
	}, testLogger())

	ctx := context.Background()

	_, err := v.Validate(ctx, "up-cred")
	if err != nil {
		t.Fatalf("first: %v", err)
	}

	time.Sleep(2500 * time.Millisecond)

	_, err = v.Validate(ctx, "up-cred")
	if err != nil {
		t.Fatalf("after upstream-expiry: %v", err)
	}

	if c := atomic.LoadInt32(&callCount); c != 2 {
		t.Fatalf("callCount=%d, want 2 (capped by upstream expiry)", c)
	}
}

func TestGEExchangeValidator_ConcurrentMissCoalescing(t *testing.T) {
	callCount := int32(0)
	hub := fakeHubExchange(t, func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&callCount, 1)
		// Simulate some latency so concurrent requests overlap.
		time.Sleep(50 * time.Millisecond)
		writeExchangeResponse(w,
			geExchangeUser{ID: "user-sf", Email: "sf@gmail.com", Role: "user"},
			"hub-token-sf",
			time.Now().Add(5*time.Minute),
			time.Now().Add(55*time.Minute),
		)
	})
	defer hub.Close()

	v := NewGEExchangeValidator(hub.URL, GEExchangeConfig{
		CredentialType: "id_token",
		CacheTTL:       60 * time.Second,
	}, testLogger())

	ctx := context.Background()

	// Launch 10 concurrent requests with the same credential.
	const n = 10
	var wg sync.WaitGroup
	errs := make([]error, n)
	ids := make([]*CallerIdentity, n)
	wg.Add(n)
	for i := 0; i < n; i++ {
		go func(idx int) {
			defer wg.Done()
			ids[idx], errs[idx] = v.Validate(ctx, "singleflight-cred")
		}(i)
	}
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Fatalf("goroutine %d: %v", i, err)
		}
	}

	// Singleflight should coalesce into exactly 1 Hub call.
	if c := atomic.LoadInt32(&callCount); c != 1 {
		t.Fatalf("callCount=%d, want 1 (singleflight coalesced)", c)
	}

	// All goroutines got the same user.
	for i, id := range ids {
		if id.UserID != "user-sf" {
			t.Errorf("goroutine %d: UserID=%q, want user-sf", i, id.UserID)
		}
	}
}

func TestGEExchangeValidator_DifferentCredentialsDifferentKeys(t *testing.T) {
	callCount := int32(0)
	hub := fakeHubExchange(t, func(w http.ResponseWriter, r *http.Request) {
		c := atomic.AddInt32(&callCount, 1)
		body, _ := io.ReadAll(r.Body)
		var req map[string]string
		json.Unmarshal(body, &req)
		writeExchangeResponse(w,
			geExchangeUser{ID: fmt.Sprintf("user-%d", c), Email: fmt.Sprintf("u%d@gmail.com", c), Role: "user"},
			fmt.Sprintf("hub-token-%d", c),
			time.Now().Add(5*time.Minute),
			time.Now().Add(55*time.Minute),
		)
	})
	defer hub.Close()

	v := NewGEExchangeValidator(hub.URL, GEExchangeConfig{
		CredentialType: "id_token",
		CacheTTL:       60 * time.Second,
	}, testLogger())

	ctx := context.Background()

	id1, err := v.Validate(ctx, "cred-alpha")
	if err != nil {
		t.Fatalf("alpha: %v", err)
	}
	id2, err := v.Validate(ctx, "cred-beta")
	if err != nil {
		t.Fatalf("beta: %v", err)
	}

	if id1.UserID == id2.UserID {
		t.Error("different credentials should map to different cache entries")
	}
	if c := atomic.LoadInt32(&callCount); c != 2 {
		t.Fatalf("callCount=%d, want 2 (different credentials)", c)
	}
}

func TestGEExchangeValidator_HubReturnsError(t *testing.T) {
	hub := fakeHubExchange(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		json.NewEncoder(w).Encode(map[string]string{"error": "invalid_credential"})
	})
	defer hub.Close()

	v := NewGEExchangeValidator(hub.URL, GEExchangeConfig{
		CredentialType: "id_token",
		CacheTTL:       60 * time.Second,
	}, testLogger())

	_, err := v.Validate(context.Background(), "bad-cred")
	if err == nil {
		t.Fatal("expected error for rejected credential")
	}
	// Should not cache failed results.
	if v.CacheLen() != 0 {
		t.Errorf("cache should be empty after failure, got %d entries", v.CacheLen())
	}
}

func TestGEExchangeValidator_Hub403Forbidden(t *testing.T) {
	hub := fakeHubExchange(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		json.NewEncoder(w).Encode(map[string]string{"error": "exchange_disabled"})
	})
	defer hub.Close()

	v := NewGEExchangeValidator(hub.URL, GEExchangeConfig{
		CredentialType: "id_token",
		CacheTTL:       60 * time.Second,
	}, testLogger())

	_, err := v.Validate(context.Background(), "disabled-cred")
	if err == nil {
		t.Fatal("expected error for 403")
	}
}

func TestGEExchangeValidator_Hub500ServerError(t *testing.T) {
	hub := fakeHubExchange(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	})
	defer hub.Close()

	v := NewGEExchangeValidator(hub.URL, GEExchangeConfig{
		CredentialType: "id_token",
		CacheTTL:       60 * time.Second,
	}, testLogger())

	_, err := v.Validate(context.Background(), "error-cred")
	if err == nil {
		t.Fatal("expected error for 500")
	}
	if v.CacheLen() != 0 {
		t.Error("failed exchange should not be cached")
	}
}

func TestGEExchangeValidator_HubNetworkError(t *testing.T) {
	// Point at a closed server to simulate network failure.
	hub := fakeHubExchange(t, func(w http.ResponseWriter, r *http.Request) {})
	hub.Close() // Close immediately.

	v := NewGEExchangeValidator(hub.URL, GEExchangeConfig{
		CredentialType: "id_token",
		CacheTTL:       60 * time.Second,
	}, testLogger())

	_, err := v.Validate(context.Background(), "network-fail-cred")
	if err == nil {
		t.Fatal("expected error for network failure")
	}
}

func TestGEExchangeValidator_IncompleteResponse_NoUser(t *testing.T) {
	hub := fakeHubExchange(t, func(w http.ResponseWriter, r *http.Request) {
		resp := geExchangeResponse{
			AccessToken: "token",
			TokenType:   "Bearer",
			User:        nil, // Missing user.
		}
		json.NewEncoder(w).Encode(resp)
	})
	defer hub.Close()

	v := NewGEExchangeValidator(hub.URL, GEExchangeConfig{
		CredentialType: "id_token",
		CacheTTL:       60 * time.Second,
	}, testLogger())

	_, err := v.Validate(context.Background(), "no-user-cred")
	if err == nil {
		t.Fatal("expected error for nil user")
	}
}

func TestGEExchangeValidator_IncompleteResponse_NoToken(t *testing.T) {
	hub := fakeHubExchange(t, func(w http.ResponseWriter, r *http.Request) {
		resp := geExchangeResponse{
			AccessToken: "", // Missing token.
			TokenType:   "Bearer",
			User:        &geExchangeUser{ID: "user-1", Email: "a@gmail.com", Role: "user"},
		}
		json.NewEncoder(w).Encode(resp)
	})
	defer hub.Close()

	v := NewGEExchangeValidator(hub.URL, GEExchangeConfig{
		CredentialType: "id_token",
		CacheTTL:       60 * time.Second,
	}, testLogger())

	_, err := v.Validate(context.Background(), "no-token-cred")
	if err == nil {
		t.Fatal("expected error for missing access token")
	}
}

func TestGEExchangeValidator_IncompleteResponse_NoUserID(t *testing.T) {
	hub := fakeHubExchange(t, func(w http.ResponseWriter, r *http.Request) {
		resp := geExchangeResponse{
			AccessToken: "tok",
			TokenType:   "Bearer",
			User:        &geExchangeUser{ID: "", Email: "a@gmail.com", Role: "user"},
		}
		json.NewEncoder(w).Encode(resp)
	})
	defer hub.Close()

	v := NewGEExchangeValidator(hub.URL, GEExchangeConfig{
		CredentialType: "id_token",
		CacheTTL:       60 * time.Second,
	}, testLogger())

	_, err := v.Validate(context.Background(), "no-uid-cred")
	if err == nil {
		t.Fatal("expected error for empty user ID")
	}
}

func TestGEExchangeValidator_InvalidJSON(t *testing.T) {
	hub := fakeHubExchange(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{not valid json`))
	})
	defer hub.Close()

	v := NewGEExchangeValidator(hub.URL, GEExchangeConfig{
		CredentialType: "id_token",
		CacheTTL:       60 * time.Second,
	}, testLogger())

	_, err := v.Validate(context.Background(), "bad-json-cred")
	if err == nil {
		t.Fatal("expected error for invalid JSON")
	}
}

func TestGEExchangeValidator_ConfigInvalidation(t *testing.T) {
	callCount := int32(0)
	hub := fakeHubExchange(t, func(w http.ResponseWriter, r *http.Request) {
		c := atomic.AddInt32(&callCount, 1)
		writeExchangeResponse(w,
			geExchangeUser{ID: fmt.Sprintf("user-v%d", c), Email: "inv@gmail.com", Role: "user"},
			fmt.Sprintf("hub-token-v%d", c),
			time.Now().Add(5*time.Minute),
			time.Now().Add(55*time.Minute),
		)
	})
	defer hub.Close()

	v := NewGEExchangeValidator(hub.URL, GEExchangeConfig{
		CredentialType: "id_token",
		CacheTTL:       60 * time.Second,
	}, testLogger())

	ctx := context.Background()

	// First call: populate cache.
	id1, err := v.Validate(ctx, "config-cred")
	if err != nil {
		t.Fatalf("v1: %v", err)
	}
	if c := atomic.LoadInt32(&callCount); c != 1 {
		t.Fatalf("after v1: callCount=%d, want 1", c)
	}

	// Second call: cache hit.
	_, err = v.Validate(ctx, "config-cred")
	if err != nil {
		t.Fatalf("v1 cached: %v", err)
	}
	if c := atomic.LoadInt32(&callCount); c != 1 {
		t.Fatalf("after v1 cached: callCount=%d, want 1", c)
	}

	// Invalidate cache (simulates config/trust change).
	v.InvalidateCache()

	// Third call: cache miss due to invalidation.
	id2, err := v.Validate(ctx, "config-cred")
	if err != nil {
		t.Fatalf("v2: %v", err)
	}
	if c := atomic.LoadInt32(&callCount); c != 2 {
		t.Fatalf("after invalidation: callCount=%d, want 2", c)
	}

	// Should be a fresh exchange with potentially different identity.
	if id1.UserID == id2.UserID {
		// The fake Hub returns different IDs per call, so they should differ.
		t.Error("after invalidation, expected different user from fresh exchange")
	}
}

func TestGEExchangeValidator_ConfigInvalidationClearsCache(t *testing.T) {
	hub := fakeHubExchange(t, func(w http.ResponseWriter, r *http.Request) {
		writeExchangeResponse(w,
			geExchangeUser{ID: "user-1", Email: "a@gmail.com", Role: "user"},
			"tok", time.Now().Add(5*time.Minute), time.Now().Add(55*time.Minute),
		)
	})
	defer hub.Close()

	v := NewGEExchangeValidator(hub.URL, GEExchangeConfig{
		CredentialType: "id_token",
		CacheTTL:       60 * time.Second,
	}, testLogger())

	// Populate cache.
	v.Validate(context.Background(), "cred-a")
	v.Validate(context.Background(), "cred-b")
	if v.CacheLen() != 2 {
		t.Fatalf("cache len = %d, want 2", v.CacheLen())
	}

	// Invalidate.
	v.InvalidateCache()
	if v.CacheLen() != 0 {
		t.Fatalf("cache len after invalidation = %d, want 0", v.CacheLen())
	}
}

func TestGEExchangeValidator_BoundedCacheEviction(t *testing.T) {
	hub := fakeHubExchange(t, func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var req map[string]string
		json.Unmarshal(body, &req)
		writeExchangeResponse(w,
			geExchangeUser{ID: "user-" + req["credential"], Email: "e@gmail.com", Role: "user"},
			"tok-" + req["credential"],
			time.Now().Add(5*time.Minute),
			time.Now().Add(55*time.Minute),
		)
	})
	defer hub.Close()

	v := NewGEExchangeValidator(hub.URL, GEExchangeConfig{
		CredentialType: "id_token",
		CacheTTL:       60 * time.Second,
	}, testLogger())

	ctx := context.Background()

	// Fill the cache to maxGECacheEntries. We use a reduced test to avoid
	// spending 10k HTTP round-trips. Instead, directly set cache entries
	// and verify eviction logic.
	v.mu.Lock()
	for i := 0; i < maxGECacheEntries; i++ {
		key := fmt.Sprintf("key-%d", i)
		v.cache[key] = &geCacheEntry{
			identity:      &CallerIdentity{UserID: fmt.Sprintf("u-%d", i)},
			hubToken:      "tok",
			expiresAt:     time.Now().Add(time.Duration(i) * time.Second), // Stagger expiry for eviction order.
			configVersion: 0,
		}
	}
	v.mu.Unlock()

	if v.CacheLen() != maxGECacheEntries {
		t.Fatalf("cache len = %d, want %d", v.CacheLen(), maxGECacheEntries)
	}

	// Add one more via real exchange — should evict the oldest.
	_, err := v.Validate(ctx, "overflow-cred")
	if err != nil {
		t.Fatalf("overflow: %v", err)
	}

	// Cache should not exceed maxGECacheEntries.
	if v.CacheLen() > maxGECacheEntries {
		t.Errorf("cache len = %d, exceeds max %d", v.CacheLen(), maxGECacheEntries)
	}
}

func TestGEExchangeValidator_EvictOldestByExpiry(t *testing.T) {
	v := NewGEExchangeValidator("http://unused", GEExchangeConfig{
		CredentialType: "id_token",
		CacheTTL:       60 * time.Second,
	}, testLogger())

	// Manually populate with known entries, oldest = "oldest-key".
	v.mu.Lock()
	v.cache["oldest-key"] = &geCacheEntry{
		identity:  &CallerIdentity{UserID: "old"},
		expiresAt: time.Now().Add(-10 * time.Second), // Already past.
	}
	v.cache["middle-key"] = &geCacheEntry{
		identity:  &CallerIdentity{UserID: "mid"},
		expiresAt: time.Now().Add(30 * time.Second),
	}
	v.cache["newest-key"] = &geCacheEntry{
		identity:  &CallerIdentity{UserID: "new"},
		expiresAt: time.Now().Add(60 * time.Second),
	}
	v.evictOldest()
	v.mu.Unlock()

	// "oldest-key" should have been evicted.
	v.mu.Lock()
	defer v.mu.Unlock()
	if _, ok := v.cache["oldest-key"]; ok {
		t.Error("oldest-key should have been evicted")
	}
	if _, ok := v.cache["middle-key"]; !ok {
		t.Error("middle-key should still be present")
	}
	if _, ok := v.cache["newest-key"]; !ok {
		t.Error("newest-key should still be present")
	}
}

func TestGEExchangeValidator_CacheKeyIncludesConfigVersion(t *testing.T) {
	v := NewGEExchangeValidator("http://unused", GEExchangeConfig{
		CredentialType: "id_token",
		CacheTTL:       60 * time.Second,
	}, testLogger())

	key1 := v.cacheKey("same-credential", 0)
	key2 := v.cacheKey("same-credential", 1)
	key3 := v.cacheKey("same-credential", 0)

	if key1 == key2 {
		t.Error("different config versions should produce different cache keys")
	}
	if key1 != key3 {
		t.Error("same credential + same version should produce same key")
	}
}

func TestGEExchangeValidator_CacheKeyDoesNotExposeCredential(t *testing.T) {
	v := NewGEExchangeValidator("http://unused", GEExchangeConfig{
		CredentialType: "id_token",
		CacheTTL:       60 * time.Second,
	}, testLogger())

	cred := "secret-google-token-12345"
	key := v.cacheKey(cred, 0)

	// The cache key is a hex-encoded SHA-256 hash, not the raw credential.
	if key == cred {
		t.Error("cache key should not be the raw credential")
	}
	// SHA-256 hex is always 64 characters.
	if len(key) != 64 {
		t.Errorf("cache key length = %d, want 64 (SHA-256 hex)", len(key))
	}
}

func TestGEExchangeValidator_DefaultTTL(t *testing.T) {
	v := NewGEExchangeValidator("http://hub", GEExchangeConfig{
		CredentialType: "id_token",
		CacheTTL:       0, // Zero → default.
	}, testLogger())

	if v.configuredTTL != defaultGECacheTTL {
		t.Errorf("TTL = %v, want %v", v.configuredTTL, defaultGECacheTTL)
	}
}

func TestGEExchangeValidator_NegativeTTL(t *testing.T) {
	v := NewGEExchangeValidator("http://hub", GEExchangeConfig{
		CredentialType: "id_token",
		CacheTTL:       -5 * time.Second,
	}, testLogger())

	if v.configuredTTL != defaultGECacheTTL {
		t.Errorf("TTL = %v, want %v", v.configuredTTL, defaultGECacheTTL)
	}
}

func TestGEExchangeValidator_MaxTTL(t *testing.T) {
	v := NewGEExchangeValidator("http://hub", GEExchangeConfig{
		CredentialType: "id_token",
		CacheTTL:       600 * time.Second,
	}, testLogger())

	if v.configuredTTL != maxGECacheTTL {
		t.Errorf("TTL = %v, want %v", v.configuredTTL, maxGECacheTTL)
	}
}

func TestGEExchangeValidator_DefaultCredentialType(t *testing.T) {
	v := NewGEExchangeValidator("http://hub", GEExchangeConfig{
		CredentialType: "", // Not set.
		CacheTTL:       60 * time.Second,
	}, testLogger())

	if v.credentialType != "id_token" {
		t.Errorf("credentialType = %q, want id_token", v.credentialType)
	}
}

func TestGEExchangeValidator_SetHTTPClient(t *testing.T) {
	v := NewGEExchangeValidator("http://hub", GEExchangeConfig{
		CredentialType: "id_token",
		CacheTTL:       60 * time.Second,
	}, testLogger())

	custom := &http.Client{Timeout: 30 * time.Second}
	v.SetHTTPClient(custom)

	if v.httpClient != custom {
		t.Error("SetHTTPClient did not replace the HTTP client")
	}
}

func TestGEExchangeValidator_TokenRotation(t *testing.T) {
	// Simulates a user whose token is rotated: old token should expire
	// from cache, new token should work independently.
	callCount := int32(0)
	hub := fakeHubExchange(t, func(w http.ResponseWriter, r *http.Request) {
		c := atomic.AddInt32(&callCount, 1)
		body, _ := io.ReadAll(r.Body)
		var req map[string]string
		json.Unmarshal(body, &req)

		writeExchangeResponse(w,
			geExchangeUser{ID: "user-rotate", Email: "rotate@gmail.com", Role: "user"},
			fmt.Sprintf("hub-token-%d", c),
			time.Now().Add(5*time.Minute),
			time.Now().Add(55*time.Minute),
		)
	})
	defer hub.Close()

	v := NewGEExchangeValidator(hub.URL, GEExchangeConfig{
		CredentialType: "id_token",
		CacheTTL:       60 * time.Second,
	}, testLogger())

	ctx := context.Background()

	// Old token.
	id1, err := v.Validate(ctx, "old-token")
	if err != nil {
		t.Fatalf("old: %v", err)
	}

	// New token (different credential → different cache key).
	id2, err := v.Validate(ctx, "new-token")
	if err != nil {
		t.Fatalf("new: %v", err)
	}

	// Both should resolve to the same user (same Hub user).
	if id1.UserID != id2.UserID {
		t.Errorf("rotation: user IDs differ: %q vs %q", id1.UserID, id2.UserID)
	}

	// But the Hub tokens should be different (from different exchanges).
	if id1.RawToken == id2.RawToken {
		t.Error("rotation: expected different Hub tokens for different credentials")
	}

	// Two Hub calls total (different cache keys).
	if c := atomic.LoadInt32(&callCount); c != 2 {
		t.Fatalf("callCount=%d, want 2", c)
	}
}

func TestGEExchangeValidator_ColdReplica(t *testing.T) {
	// A "cold replica" starts with an empty cache and must exchange every
	// credential on first access.
	callCount := int32(0)
	hub := fakeHubExchange(t, func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&callCount, 1)
		writeExchangeResponse(w,
			geExchangeUser{ID: "user-cold", Email: "cold@gmail.com", Role: "user"},
			"hub-token-cold",
			time.Now().Add(5*time.Minute),
			time.Now().Add(55*time.Minute),
		)
	})
	defer hub.Close()

	// Create a new validator (simulating cold start).
	v := NewGEExchangeValidator(hub.URL, GEExchangeConfig{
		CredentialType: "id_token",
		CacheTTL:       60 * time.Second,
	}, testLogger())

	if v.CacheLen() != 0 {
		t.Fatalf("cold start should have empty cache, got %d", v.CacheLen())
	}

	ctx := context.Background()
	id, err := v.Validate(ctx, "cold-cred")
	if err != nil {
		t.Fatalf("cold: %v", err)
	}
	if id.UserID != "user-cold" {
		t.Errorf("UserID = %q, want user-cold", id.UserID)
	}
	if c := atomic.LoadInt32(&callCount); c != 1 {
		t.Fatalf("callCount=%d, want 1 (no cache on cold start)", c)
	}
}

func TestGEExchangeValidator_ContextCancellation(t *testing.T) {
	hub := fakeHubExchange(t, func(w http.ResponseWriter, r *http.Request) {
		// Simulate slow Hub response.
		time.Sleep(2 * time.Second)
		writeExchangeResponse(w,
			geExchangeUser{ID: "user-slow", Email: "slow@gmail.com", Role: "user"},
			"hub-token-slow",
			time.Now().Add(5*time.Minute),
			time.Now().Add(55*time.Minute),
		)
	})
	defer hub.Close()

	v := NewGEExchangeValidator(hub.URL, GEExchangeConfig{
		CredentialType: "id_token",
		CacheTTL:       60 * time.Second,
	}, testLogger())

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	_, err := v.Validate(ctx, "cancelled-cred")
	if err == nil {
		t.Fatal("expected error for cancelled context")
	}
}

func TestGEExchangeValidator_RawTokenPassthrough(t *testing.T) {
	// Verifies that the Hub-issued token is stored in RawToken for
	// direct use by downstream Hub API calls (contract: direct returned
	// Hub token use, no re-minting).
	hub := fakeHubExchange(t, func(w http.ResponseWriter, r *http.Request) {
		writeExchangeResponse(w,
			geExchangeUser{ID: "user-pt", Email: "pt@gmail.com", Role: "user"},
			"hub-issued-access-token-xyz",
			time.Now().Add(5*time.Minute),
			time.Now().Add(55*time.Minute),
		)
	})
	defer hub.Close()

	v := NewGEExchangeValidator(hub.URL, GEExchangeConfig{
		CredentialType: "id_token",
		CacheTTL:       60 * time.Second,
	}, testLogger())

	id, err := v.Validate(context.Background(), "passthrough-cred")
	if err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if id.RawToken != "hub-issued-access-token-xyz" {
		t.Errorf("RawToken = %q, want hub-issued-access-token-xyz", id.RawToken)
	}
}

func TestGEExchangeValidator_ProtectedAccessRequiresAuth(t *testing.T) {
	// Verifies that every Validate call results in either a cached identity
	// or a Hub exchange — there is no bypass that returns a nil identity
	// without error.
	hub := fakeHubExchange(t, func(w http.ResponseWriter, r *http.Request) {
		writeExchangeResponse(w,
			geExchangeUser{ID: "user-guard", Email: "guard@gmail.com", Role: "user"},
			"hub-token-guard",
			time.Now().Add(5*time.Minute),
			time.Now().Add(55*time.Minute),
		)
	})
	defer hub.Close()

	v := NewGEExchangeValidator(hub.URL, GEExchangeConfig{
		CredentialType: "id_token",
		CacheTTL:       60 * time.Second,
	}, testLogger())

	id, err := v.Validate(context.Background(), "guard-cred")
	if err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if id == nil {
		t.Fatal("Validate returned nil identity without error — auth bypass")
	}
	if id.UserID == "" {
		t.Error("Validate returned identity with empty UserID")
	}
}

func TestGEExchangeValidator_NoSharedClientFallback(t *testing.T) {
	// Verifies that when the Hub rejects a credential (e.g., foreign client ID),
	// the bridge does not fall back to a shared/admin client — it propagates
	// the error.
	hub := fakeHubExchange(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		json.NewEncoder(w).Encode(map[string]string{
			"error": "foreign_client_id",
		})
	})
	defer hub.Close()

	v := NewGEExchangeValidator(hub.URL, GEExchangeConfig{
		CredentialType: "id_token",
		CacheTTL:       60 * time.Second,
	}, testLogger())

	_, err := v.Validate(context.Background(), "foreign-client-cred")
	if err == nil {
		t.Fatal("expected error — no shared client fallback should exist")
	}
}

func TestGEExchangeValidator_MultipleInvalidations(t *testing.T) {
	callCount := int32(0)
	hub := fakeHubExchange(t, func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&callCount, 1)
		writeExchangeResponse(w,
			geExchangeUser{ID: "user-mi", Email: "mi@gmail.com", Role: "user"},
			"tok",
			time.Now().Add(5*time.Minute),
			time.Now().Add(55*time.Minute),
		)
	})
	defer hub.Close()

	v := NewGEExchangeValidator(hub.URL, GEExchangeConfig{
		CredentialType: "id_token",
		CacheTTL:       60 * time.Second,
	}, testLogger())

	ctx := context.Background()

	// Populate, invalidate, re-populate, invalidate again.
	v.Validate(ctx, "mi-cred")
	v.InvalidateCache()
	v.Validate(ctx, "mi-cred")
	v.InvalidateCache()
	v.Validate(ctx, "mi-cred")

	if c := atomic.LoadInt32(&callCount); c != 3 {
		t.Fatalf("callCount=%d, want 3 (each invalidation forces a re-exchange)", c)
	}
}

// ---------------------------------------------------------------------------
// Singleflight context isolation (Finding #7) — cancelling one caller's
// context must not abort the in-flight exchange for other concurrent callers.
// ---------------------------------------------------------------------------

func TestGEExchangeValidator_SingleflightContextIsolation(t *testing.T) {
	var hubCallCount int32
	hub := fakeHubExchange(t, func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hubCallCount, 1)
		// Simulate a slow exchange — give time for contexts to be cancelled.
		time.Sleep(200 * time.Millisecond)
		writeExchangeResponse(w,
			geExchangeUser{ID: "user-sf", Email: "sf@gmail.com", Role: "user"},
			"hub-token-sf",
			time.Now().Add(5*time.Minute),
			time.Now().Add(55*time.Minute),
		)
	})
	defer hub.Close()

	v := NewGEExchangeValidator(hub.URL, GEExchangeConfig{
		CredentialType: "id_token",
		CacheTTL:       60 * time.Second,
	}, testLogger())

	// Caller 1: will be cancelled after 50ms.
	ctx1, cancel1 := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel1()

	// Caller 2: has a long deadline — should succeed.
	ctx2, cancel2 := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel2()

	var wg sync.WaitGroup
	var err1, err2 error
	var id2 *CallerIdentity

	wg.Add(2)
	go func() {
		defer wg.Done()
		_, err1 = v.Validate(ctx1, "same-cred")
	}()
	go func() {
		defer wg.Done()
		id2, err2 = v.Validate(ctx2, "same-cred")
	}()
	wg.Wait()

	// Caller 1 should have been cancelled (context deadline exceeded).
	if err1 == nil {
		// It's acceptable if caller 1 succeeded before its context was cancelled
		// (the exchange was fast enough). But it must NOT have caused caller 2 to fail.
	}

	// Caller 2 MUST succeed — the in-flight exchange should use a detached context
	// that is not tied to caller 1's cancellation.
	if err2 != nil {
		t.Fatalf("caller 2 failed (should succeed even if caller 1 is cancelled): %v", err2)
	}
	if id2.UserID != "user-sf" {
		t.Errorf("caller 2 user = %q, want %q", id2.UserID, "user-sf")
	}

	// Only 1 Hub call should have been made (singleflight coalescing).
	if c := atomic.LoadInt32(&hubCallCount); c != 1 {
		t.Errorf("hub call count = %d, want 1 (singleflight should coalesce)", c)
	}
}
