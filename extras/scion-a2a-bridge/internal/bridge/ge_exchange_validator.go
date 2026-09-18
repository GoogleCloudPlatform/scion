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
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"golang.org/x/sync/singleflight"
)

// ---------------------------------------------------------------------------
// GEExchangeValidator — bridge-side Google credential exchange with caching.
//
// Authenticates every protected A2A request by exchanging the caller's Google
// credential with the Hub's exchange endpoint. Results are cached per-replica
// with bounded size, configurable TTL capped by Hub and upstream expiry, and
// singleflight concurrent-miss coalescing.
//
// Per contract:
// - Uses documented end-user Authorization: Bearer
// - Returns Hub access token directly for caller user operations
// - Returns local user ID for ownership
// - No minting from external subject, shared admin fallback, refresh session
// - Token hashes are never logged
// ---------------------------------------------------------------------------

const (
	defaultGECacheTTL = 60 * time.Second
	maxGECacheTTL     = 300 * time.Second
	// maxGECacheEntries is the bounded cache size per replica.
	maxGECacheEntries = 10000
)

// GEExchangeConfig holds the GE exchange configuration for the bridge.
type GEExchangeConfig struct {
	// CredentialType is the credential type to send to the Hub.
	// Must be "id_token" or "access_token" — comes from bridge config,
	// not heuristic payload decoding (contract-review point 5).
	CredentialType string `yaml:"credential_type"`
	// CacheTTL is the configured cache TTL. Capped by Hub and upstream expiry.
	CacheTTL time.Duration `yaml:"cache_ttl"`
}

// GEExchangeValidator exchanges Google credentials with the Hub and caches
// the results with bounded size, TTL, and singleflight coalescing.
type GEExchangeValidator struct {
	hubEndpoint    string
	httpClient     *http.Client
	credentialType string
	configuredTTL  time.Duration
	log            *slog.Logger

	// configVersion is incremented when trust/config changes to invalidate
	// all cache entries (contract-review point 6).
	configVersion uint64

	mu    sync.Mutex
	cache map[string]*geCacheEntry // key: SHA-256(credential + configVersion)
	sfg   singleflight.Group
}

// geCacheEntry holds a cached exchange result.
type geCacheEntry struct {
	identity      *CallerIdentity
	hubToken      string
	expiresAt     time.Time
	configVersion uint64
}

// geExchangeResponse mirrors the Hub's exchange response JSON.
type geExchangeResponse struct {
	AccessToken       string          `json:"accessToken"`
	TokenType         string          `json:"tokenType"`
	ExpiresAt         string          `json:"expiresAt"`
	UpstreamExpiresAt string          `json:"upstreamExpiresAt"`
	User              *geExchangeUser `json:"user"`
}

type geExchangeUser struct {
	ID          string `json:"id"`
	Email       string `json:"email"`
	DisplayName string `json:"displayName"`
	Role        string `json:"role"`
}

// NewGEExchangeValidator creates a new GE exchange validator.
func NewGEExchangeValidator(hubEndpoint string, cfg GEExchangeConfig, log *slog.Logger) *GEExchangeValidator {
	ttl := cfg.CacheTTL
	if ttl <= 0 {
		ttl = defaultGECacheTTL
	}
	if ttl > maxGECacheTTL {
		ttl = maxGECacheTTL
	}

	credType := cfg.CredentialType
	if credType == "" {
		credType = "id_token" // Default, but should be explicitly configured
	}

	return &GEExchangeValidator{
		hubEndpoint:    hubEndpoint,
		httpClient:     &http.Client{Timeout: 15 * time.Second},
		credentialType: credType,
		configuredTTL:  ttl,
		log:            log,
		cache:          make(map[string]*geCacheEntry),
	}
}

// SetHTTPClient sets a custom HTTP client (for transport auth / testing).
func (v *GEExchangeValidator) SetHTTPClient(client *http.Client) {
	v.httpClient = client
}

// InvalidateCache increments the config version, causing all existing cache
// entries to be treated as invalid on next access. This handles trust/config
// changes including Hub endpoint, allowed client IDs, or credential mode
// (contract-review point 6).
func (v *GEExchangeValidator) InvalidateCache() {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.configVersion++
	// Clear the cache immediately to free memory.
	v.cache = make(map[string]*geCacheEntry)
}

// Validate exchanges a Google credential with the Hub and returns the
// caller identity. Results are cached with bounded size and TTL.
func (v *GEExchangeValidator) Validate(ctx context.Context, credential string) (*CallerIdentity, error) {
	// Build cache key including credential hash and config version.
	v.mu.Lock()
	currentVersion := v.configVersion
	v.mu.Unlock()

	key := v.cacheKey(credential, currentVersion)

	// Check cache first.
	v.mu.Lock()
	if entry, ok := v.cache[key]; ok {
		if time.Now().Before(entry.expiresAt) && entry.configVersion == currentVersion {
			id := entry.identity
			v.mu.Unlock()
			return id, nil
		}
		// Expired or stale config version — remove.
		delete(v.cache, key)
	}
	v.mu.Unlock()

	// Use singleflight DoChan to coalesce concurrent misses for the same
	// credential. DoChan isolates each waiter's context: a cancelled caller
	// does not abort the in-flight exchange for other waiters.
	ch := v.sfg.DoChan(key, func() (interface{}, error) {
		// Use context.WithoutCancel so the shared exchange call is not tied
		// to any single caller's context lifetime. Each waiter selects on
		// its own ctx.Done() independently below.
		detached := context.WithoutCancel(ctx)
		return v.exchange(detached, credential, currentVersion)
	})

	select {
	case res := <-ch:
		if res.Err != nil {
			return nil, res.Err
		}
		return res.Val.(*CallerIdentity), nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// exchange performs the actual Hub exchange call and caches the result.
func (v *GEExchangeValidator) exchange(ctx context.Context, credential string, configVersion uint64) (*CallerIdentity, error) {
	// Build exchange request body.
	reqBody, err := json.Marshal(map[string]string{
		"credential":     credential,
		"credentialType": v.credentialType,
	})
	if err != nil {
		return nil, fmt.Errorf("marshal exchange request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		v.hubEndpoint+"/api/v1/auth/integrations/google/exchange",
		bytes.NewReader(reqBody))
	if err != nil {
		return nil, fmt.Errorf("create exchange request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := v.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("exchange request failed: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 64*1024))
	if err != nil {
		return nil, fmt.Errorf("read exchange response: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		// Parse error response for better logging.
		v.log.Debug("GE exchange failed",
			"status", resp.StatusCode,
			"response_length", len(body))
		return nil, fmt.Errorf("Hub exchange returned HTTP %d", resp.StatusCode)
	}

	var exchangeResp geExchangeResponse
	if err := json.Unmarshal(body, &exchangeResp); err != nil {
		return nil, fmt.Errorf("decode exchange response: %w", err)
	}

	if exchangeResp.User == nil || exchangeResp.User.ID == "" {
		return nil, fmt.Errorf("exchange returned incomplete identity")
	}
	if exchangeResp.AccessToken == "" {
		return nil, fmt.Errorf("exchange returned no access token")
	}

	// Parse Hub expiry and upstream expiry for cache TTL capping.
	var hubExpiry, upstreamExpiry time.Time
	if exchangeResp.ExpiresAt != "" {
		hubExpiry, _ = time.Parse(time.RFC3339, exchangeResp.ExpiresAt)
	}
	if exchangeResp.UpstreamExpiresAt != "" {
		upstreamExpiry, _ = time.Parse(time.RFC3339, exchangeResp.UpstreamExpiresAt)
	}

	// Compute effective cache TTL: min(configured, Hub expiry, upstream expiry).
	cacheTTL := v.configuredTTL
	now := time.Now()
	if !hubExpiry.IsZero() {
		hubRemaining := time.Until(hubExpiry)
		if hubRemaining > 0 && hubRemaining < cacheTTL {
			cacheTTL = hubRemaining
		}
	}
	if !upstreamExpiry.IsZero() {
		upRemaining := time.Until(upstreamExpiry)
		if upRemaining > 0 && upRemaining < cacheTTL {
			cacheTTL = upRemaining
		}
	}

	identity := &CallerIdentity{
		UserID:    exchangeResp.User.ID,
		Email:     exchangeResp.User.Email,
		Role:      exchangeResp.User.Role,
		RawToken:  exchangeResp.AccessToken, // Hub-issued token for direct use
		TokenType: "ge_exchange",
	}

	// Cache the result with bounded size and TTL.
	cacheKey := v.cacheKey(credential, configVersion)
	v.mu.Lock()
	// Enforce bounded cache size by evicting oldest entries when at capacity.
	if len(v.cache) >= maxGECacheEntries {
		v.evictOldest()
	}
	v.cache[cacheKey] = &geCacheEntry{
		identity:      identity,
		hubToken:      exchangeResp.AccessToken,
		expiresAt:     now.Add(cacheTTL),
		configVersion: configVersion,
	}
	v.mu.Unlock()

	return identity, nil
}

// cacheKey builds a deterministic cache key from the credential and config
// version. The credential is hashed to avoid storing raw tokens in memory
// as map keys (contract: tokens are never logged or stored as metadata).
func (v *GEExchangeValidator) cacheKey(credential string, configVersion uint64) string {
	h := sha256.New()
	h.Write([]byte(credential))
	h.Write([]byte(fmt.Sprintf(":%d", configVersion)))
	return hex.EncodeToString(h.Sum(nil))
}

// evictOldest removes the oldest cache entry. Must be called with mu held.
func (v *GEExchangeValidator) evictOldest() {
	var oldestKey string
	var oldestTime time.Time
	first := true
	for k, entry := range v.cache {
		if first || entry.expiresAt.Before(oldestTime) {
			oldestKey = k
			oldestTime = entry.expiresAt
			first = false
		}
	}
	if oldestKey != "" {
		delete(v.cache, oldestKey)
	}
}

// CacheLen returns the number of cached entries (for testing).
func (v *GEExchangeValidator) CacheLen() int {
	v.mu.Lock()
	defer v.mu.Unlock()
	return len(v.cache)
}
