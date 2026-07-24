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
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
	"time"
)

const (
	defaultUATCacheTTL = 60 * time.Second
	maxUATCacheTTL     = 300 * time.Second
)

// UATValidator validates Scion user access tokens (scion_pat_*) by calling
// the Hub's GET /api/v1/auth/me endpoint. Results are cached by SHA-256(token)
// with a configurable TTL to avoid a round-trip on every request.
type UATValidator struct {
	hubEndpoint string
	httpClient  *http.Client
	ttl         time.Duration

	mu    sync.Mutex
	cache map[[32]byte]*uatCacheEntry
}

type uatCacheEntry struct {
	identity  *CallerIdentity
	expiresAt time.Time
}

// userResponse mirrors the fields returned by GET /api/v1/auth/me.
type userResponse struct {
	ID          string `json:"id"`
	Email       string `json:"email"`
	DisplayName string `json:"display_name"`
	Role        string `json:"role"`
}

// NewUATValidator creates a validator that introspects UATs via the Hub.
func NewUATValidator(hubEndpoint string, ttl time.Duration) *UATValidator {
	if ttl <= 0 {
		ttl = defaultUATCacheTTL
	}
	if ttl > maxUATCacheTTL {
		ttl = maxUATCacheTTL
	}
	return &UATValidator{
		hubEndpoint: hubEndpoint,
		httpClient: &http.Client{
			Timeout: 10 * time.Second,
		},
		ttl:   ttl,
		cache: make(map[[32]byte]*uatCacheEntry),
	}
}

// Validate introspects a UAT by calling Hub /api/v1/auth/me. The result
// is cached by SHA-256(token) to avoid a DB round-trip on every A2A message.
// A revoked UAT stops working within one cache TTL of revocation.
func (v *UATValidator) Validate(ctx context.Context, token string) (*CallerIdentity, error) {
	key := sha256.Sum256([]byte(token))

	v.mu.Lock()
	if entry, ok := v.cache[key]; ok {
		if time.Now().Before(entry.expiresAt) {
			id := entry.identity
			v.mu.Unlock()
			return id, nil
		}
		// Expired — remove and re-validate.
		delete(v.cache, key)
	}
	v.mu.Unlock()

	// Call Hub /api/v1/auth/me with the caller's UAT as the bearer token.
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, v.hubEndpoint+"/api/v1/auth/me", nil)
	if err != nil {
		return nil, fmt.Errorf("create auth/me request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+token)

	resp, err := v.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("call auth/me: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		return nil, fmt.Errorf("UAT rejected by Hub (HTTP %d)", resp.StatusCode)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("unexpected Hub response: HTTP %d", resp.StatusCode)
	}

	var user userResponse
	if err := json.NewDecoder(resp.Body).Decode(&user); err != nil {
		return nil, fmt.Errorf("decode auth/me response: %w", err)
	}
	if user.ID == "" || user.Email == "" {
		return nil, fmt.Errorf("auth/me returned incomplete identity (id=%q, email=%q)", user.ID, user.Email)
	}

	identity := &CallerIdentity{
		UserID:    user.ID,
		Email:     user.Email,
		Role:      user.Role,
		RawToken:  token,
		TokenType: "uat",
	}

	v.mu.Lock()
	v.cache[key] = &uatCacheEntry{
		identity:  identity,
		expiresAt: time.Now().Add(v.ttl),
	}
	// Lazy eviction: remove expired entries while we hold the lock.
	now := time.Now()
	for k, e := range v.cache {
		if now.After(e.expiresAt) {
			delete(v.cache, k)
		}
	}
	v.mu.Unlock()

	return identity, nil
}
