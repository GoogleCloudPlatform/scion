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
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

func TestUATValidator_ValidToken(t *testing.T) {
	callCount := int32(0)
	hub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&callCount, 1)
		if r.URL.Path != "/api/v1/auth/me" {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		auth := r.Header.Get("Authorization")
		if auth != "Bearer scion_pat_test123" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		json.NewEncoder(w).Encode(userResponse{
			ID:    "user-1",
			Email: "alice@example.com",
			Role:  "user",
		})
	}))
	defer hub.Close()

	v := NewUATValidator(hub.URL, 60*time.Second)
	ctx := context.Background()

	id, err := v.Validate(ctx, "scion_pat_test123")
	if err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if id.UserID != "user-1" || id.Email != "alice@example.com" || id.TokenType != "uat" {
		t.Errorf("unexpected identity: %+v", id)
	}
	if id.RawToken != "scion_pat_test123" {
		t.Errorf("RawToken = %q, want %q", id.RawToken, "scion_pat_test123")
	}
}

func TestUATValidator_InvalidToken(t *testing.T) {
	hub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
	}))
	defer hub.Close()

	v := NewUATValidator(hub.URL, 60*time.Second)
	ctx := context.Background()

	_, err := v.Validate(ctx, "scion_pat_invalid")
	if err == nil {
		t.Fatal("expected error for invalid token")
	}
}

func TestUATValidator_CacheTTL(t *testing.T) {
	callCount := int32(0)
	hub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&callCount, 1)
		json.NewEncoder(w).Encode(userResponse{
			ID:    "user-1",
			Email: "alice@example.com",
			Role:  "user",
		})
	}))
	defer hub.Close()

	// Use a very short TTL so we can test expiry.
	v := NewUATValidator(hub.URL, 50*time.Millisecond)
	ctx := context.Background()

	// First call — hits Hub.
	_, err := v.Validate(ctx, "scion_pat_cached")
	if err != nil {
		t.Fatalf("first call: %v", err)
	}
	if c := atomic.LoadInt32(&callCount); c != 1 {
		t.Fatalf("call count after first = %d, want 1", c)
	}

	// Second call — should be cached (no Hub call).
	_, err = v.Validate(ctx, "scion_pat_cached")
	if err != nil {
		t.Fatalf("cached call: %v", err)
	}
	if c := atomic.LoadInt32(&callCount); c != 1 {
		t.Fatalf("call count after cached = %d, want 1 (cached)", c)
	}

	// Wait for TTL to expire.
	time.Sleep(100 * time.Millisecond)

	// Third call — cache expired, should hit Hub again.
	_, err = v.Validate(ctx, "scion_pat_cached")
	if err != nil {
		t.Fatalf("expired call: %v", err)
	}
	if c := atomic.LoadInt32(&callCount); c != 2 {
		t.Fatalf("call count after expiry = %d, want 2", c)
	}
}

func TestUATValidator_RevokedToken(t *testing.T) {
	// Simulates a token that's valid initially, then revoked.
	revoked := int32(0)
	hub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if atomic.LoadInt32(&revoked) == 1 {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		json.NewEncoder(w).Encode(userResponse{
			ID:    "user-revoke",
			Email: "revoke@example.com",
			Role:  "user",
		})
	}))
	defer hub.Close()

	v := NewUATValidator(hub.URL, 50*time.Millisecond)
	ctx := context.Background()

	// Valid initially.
	id, err := v.Validate(ctx, "scion_pat_revokable")
	if err != nil {
		t.Fatalf("initial validate: %v", err)
	}
	if id.UserID != "user-revoke" {
		t.Fatalf("unexpected user: %s", id.UserID)
	}

	// Revoke the token.
	atomic.StoreInt32(&revoked, 1)

	// Still cached — should succeed.
	_, err = v.Validate(ctx, "scion_pat_revokable")
	if err != nil {
		t.Fatalf("cached after revoke: %v", err)
	}

	// Wait for cache expiry.
	time.Sleep(100 * time.Millisecond)

	// Now should fail.
	_, err = v.Validate(ctx, "scion_pat_revokable")
	if err == nil {
		t.Fatal("expected error after revocation and cache expiry")
	}
}

func TestUATValidator_DefaultAndMaxTTL(t *testing.T) {
	// Default TTL
	v1 := NewUATValidator("http://hub", 0)
	if v1.ttl != defaultUATCacheTTL {
		t.Errorf("default TTL = %v, want %v", v1.ttl, defaultUATCacheTTL)
	}

	// Negative TTL
	v2 := NewUATValidator("http://hub", -1*time.Second)
	if v2.ttl != defaultUATCacheTTL {
		t.Errorf("negative TTL = %v, want %v", v2.ttl, defaultUATCacheTTL)
	}

	// Over max TTL
	v3 := NewUATValidator("http://hub", 600*time.Second)
	if v3.ttl != maxUATCacheTTL {
		t.Errorf("over-max TTL = %v, want %v", v3.ttl, maxUATCacheTTL)
	}
}

func TestUATValidator_IncompleteResponse(t *testing.T) {
	hub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Return a response with missing email
		json.NewEncoder(w).Encode(userResponse{
			ID:   "user-1",
			Role: "user",
		})
	}))
	defer hub.Close()

	v := NewUATValidator(hub.URL, 60*time.Second)
	_, err := v.Validate(context.Background(), "scion_pat_test")
	if err == nil {
		t.Fatal("expected error for incomplete response")
	}
}
