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
	"container/list"
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

	"github.com/GoogleCloudPlatform/scion/pkg/transportauth"
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

	// Transport auth for reaching Hubs behind Cloud Run / IAP.
	transportSrc  transportauth.TokenSource
	transportMode transportauth.HeaderMode

	// configVersion is incremented when trust/config changes to invalidate
	// all cache entries (contract-review point 6).
	configVersion uint64

	// LRU cache: mu protects both the map and the LRU list.
	// The list orders entries from most-recently-used (front) to
	// least-recently-used (back). Eviction removes from the back.
	mu      sync.Mutex
	cache   map[string]*list.Element // key → list element wrapping *geCacheEntry
	lruList *list.List               // doubly-linked list for O(1) LRU eviction
	sfg     singleflight.Group
}

// geCacheEntry holds a cached exchange result.
type geCacheEntry struct {
	key           string // cache key for reverse lookup in map
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

// GEValidatorOption configures optional GEExchangeValidator behaviour.
type GEValidatorOption func(*GEExchangeValidator)

// WithGETransportAuth stores the resolved transport-layer OIDC auth so that
// Hub exchange requests include Cloud Run / IAP invoker identity headers.
func WithGETransportAuth(src transportauth.TokenSource, mode transportauth.HeaderMode) GEValidatorOption {
	return func(v *GEExchangeValidator) {
		v.transportSrc = src
		v.transportMode = mode
	}
}

// WithGEHTTPClient overrides the HTTP client (for testing).
func WithGEHTTPClient(client *http.Client) GEValidatorOption {
	return func(v *GEExchangeValidator) {
		v.httpClient = client
	}
}

// NewGEExchangeValidator creates a new GE exchange validator.
func NewGEExchangeValidator(hubEndpoint string, cfg GEExchangeConfig, log *slog.Logger, opts ...GEValidatorOption) *GEExchangeValidator {
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

	v := &GEExchangeValidator{
		hubEndpoint:    hubEndpoint,
		httpClient:     &http.Client{Timeout: 15 * time.Second},
		credentialType: credType,
		configuredTTL:  ttl,
		log:            log,
		cache:          make(map[string]*list.Element),
		lruList:        list.New(),
	}

	for _, opt := range opts {
		opt(v)
	}

	// Compose transport auth into the HTTP client's transport if configured.
	if v.transportSrc != nil {
		base := http.DefaultTransport
		if v.httpClient.Transport != nil {
			base = v.httpClient.Transport
		}
		v.httpClient.Transport = transportauth.Wrap(base, v.transportSrc, v.transportMode)
	}

	return v
}

// SetHTTPClient sets a custom HTTP client (for testing).
func (v *GEExchangeValidator) SetHTTPClient(client *http.Client) {
	v.httpClient = client
}

// SetTransportAuth applies transport auth (Cloud Run / IAP invoker headers)
// to the existing HTTP client. Safe to call once before the first Validate.
func (v *GEExchangeValidator) SetTransportAuth(src transportauth.TokenSource, mode transportauth.HeaderMode) {
	v.transportSrc = src
	v.transportMode = mode
	base := http.DefaultTransport
	if v.httpClient.Transport != nil {
		base = v.httpClient.Transport
	}
	v.httpClient.Transport = transportauth.Wrap(base, src, mode)
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
	v.cache = make(map[string]*list.Element)
	v.lruList.Init()
}

// Validate exchanges a Google credential with the Hub and returns the
// caller identity. Results are cached with bounded size and TTL.
func (v *GEExchangeValidator) Validate(ctx context.Context, credential string) (*CallerIdentity, error) {
	// Build cache key including credential hash and config version.
	v.mu.Lock()
	currentVersion := v.configVersion
	v.mu.Unlock()

	key := v.cacheKey(credential, currentVersion)

	// Check cache first (LRU: move to front on hit).
	v.mu.Lock()
	if elem, ok := v.cache[key]; ok {
		entry := elem.Value.(*geCacheEntry)
		if time.Now().Before(entry.expiresAt) && entry.configVersion == currentVersion {
			v.lruList.MoveToFront(elem)
			id := entry.identity
			v.mu.Unlock()
			return id, nil
		}
		// Expired or stale config version — remove.
		v.lruList.Remove(elem)
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
	// Fail closed if either remaining lifetime is ≤ 0 (already expired or
	// clock skew); never cache an expired token.
	cacheTTL := v.configuredTTL
	now := time.Now()
	if !hubExpiry.IsZero() {
		hubRemaining := time.Until(hubExpiry)
		if hubRemaining <= 0 {
			return nil, fmt.Errorf("Hub token already expired (remaining: %v)", hubRemaining)
		}
		if hubRemaining < cacheTTL {
			cacheTTL = hubRemaining
		}
	}
	if !upstreamExpiry.IsZero() {
		upRemaining := time.Until(upstreamExpiry)
		if upRemaining <= 0 {
			return nil, fmt.Errorf("upstream credential already expired (remaining: %v)", upRemaining)
		}
		if upRemaining < cacheTTL {
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
	entry := &geCacheEntry{
		key:           cacheKey,
		identity:      identity,
		hubToken:      exchangeResp.AccessToken,
		expiresAt:     now.Add(cacheTTL),
		configVersion: configVersion,
	}
	v.mu.Lock()
	// If key already exists (concurrent exchange resolved same credential),
	// update in place and move to front.
	if elem, ok := v.cache[cacheKey]; ok {
		v.lruList.MoveToFront(elem)
		elem.Value = entry
	} else {
		// Enforce bounded cache size by evicting LRU entries when at capacity.
		for len(v.cache) >= maxGECacheEntries {
			v.evictLRU()
		}
		elem := v.lruList.PushFront(entry)
		v.cache[cacheKey] = elem
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

// evictLRU removes the least-recently-used cache entry (back of list).
// O(1) operation. Must be called with mu held.
func (v *GEExchangeValidator) evictLRU() {
	back := v.lruList.Back()
	if back == nil {
		return
	}
	entry := back.Value.(*geCacheEntry)
	v.lruList.Remove(back)
	delete(v.cache, entry.key)
}

// CacheLen returns the number of cached entries (for testing).
func (v *GEExchangeValidator) CacheLen() int {
	v.mu.Lock()
	defer v.mu.Unlock()
	return len(v.cache)
}
