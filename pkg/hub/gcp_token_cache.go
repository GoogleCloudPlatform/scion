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
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/transportauth"
	"golang.org/x/sync/singleflight"
)

const (
	// idTokenCacheTTL bounds how long the Hub caches a minted GCP identity token.
	// Keeping this short (5m vs 1h token lifetime) ensures clients receiving a
	// cached token still get ~50-55m of validity while deduplicating bursts,
	// restarts, and shared-SA calls.
	idTokenCacheTTL = 5 * time.Minute

	// idTokenExpiryMargin ensures the Hub never serves a cached ID token that is
	// close to its JWT exp claim.
	idTokenExpiryMargin = 5 * time.Minute

	// defaultMaxIDTokenCacheEntries caps the Hub ID token cache because audience
	// is caller-controlled and unbounded.
	defaultMaxIDTokenCacheEntries = 1024

	// idTokenFetchTimeout bounds detached singleflight IAM calls so a cancelled
	// caller does not abort a shared fetch for concurrent waiters.
	idTokenFetchTimeout = 15 * time.Second
)

// CachedGCPTokenGenerator wraps a GCPTokenGenerator and caches access tokens
// per service account + scopes combination, and identity tokens per service
// account + audience combination with a short TTL and bounded size. This avoids
// redundant IAM API calls when multiple agents share the same service account
// or burst on the same audience.
type CachedGCPTokenGenerator struct {
	inner GCPTokenGenerator

	mu    sync.RWMutex
	cache map[string]*cachedTokenEntry

	group singleflight.Group

	idMu              sync.RWMutex
	idCache           map[string]*cachedIDTokenEntry
	idGroup           singleflight.Group
	maxIDCacheEntries int

	// now, if set, overrides time.Now (tests).
	now func() time.Time
}

type cachedTokenEntry struct {
	token     *GCPAccessToken
	fetchedAt time.Time
}

type cachedIDTokenEntry struct {
	token     *GCPIDToken
	expiresAt time.Time
}

// NewCachedGCPTokenGenerator wraps an existing generator with a per-SA cache.
func NewCachedGCPTokenGenerator(inner GCPTokenGenerator) *CachedGCPTokenGenerator {
	return &CachedGCPTokenGenerator{
		inner:             inner,
		cache:             make(map[string]*cachedTokenEntry),
		idCache:           make(map[string]*cachedIDTokenEntry),
		maxIDCacheEntries: defaultMaxIDTokenCacheEntries,
	}
}

func (c *CachedGCPTokenGenerator) clock() time.Time {
	if c.now != nil {
		return c.now()
	}
	return time.Now()
}

func idTokenCacheKey(email, audience string) string {
	return email + "\x00" + audience
}

func tokenCacheKey(email string, scopes []string) string {
	sorted := make([]string, len(scopes))
	copy(sorted, scopes)
	sort.Strings(sorted)
	return email + "|" + strings.Join(sorted, ",")
}

func (c *CachedGCPTokenGenerator) GenerateAccessToken(ctx context.Context, serviceAccountEmail string, scopes []string) (*GCPAccessToken, error) {
	key := tokenCacheKey(serviceAccountEmail, scopes)

	// Check cache
	c.mu.RLock()
	entry := c.cache[key]
	c.mu.RUnlock()

	if entry != nil {
		elapsed := time.Since(entry.fetchedAt)
		remaining := time.Duration(entry.token.ExpiresIn)*time.Second - elapsed
		if remaining > 5*time.Minute {
			return &GCPAccessToken{
				AccessToken: entry.token.AccessToken,
				ExpiresIn:   int(remaining.Seconds()),
				TokenType:   entry.token.TokenType,
			}, nil
		}
	}

	// Singleflight: deduplicate concurrent requests for the same SA+scopes
	result, err, _ := c.group.Do(key, func() (interface{}, error) {
		token, err := c.inner.GenerateAccessToken(ctx, serviceAccountEmail, scopes)
		if err != nil {
			return nil, err
		}
		c.mu.Lock()
		c.cache[key] = &cachedTokenEntry{
			token:     token,
			fetchedAt: time.Now(),
		}
		c.mu.Unlock()
		return token, nil
	})
	if err != nil {
		return nil, err
	}

	orig := result.(*GCPAccessToken)
	return &GCPAccessToken{
		AccessToken: orig.AccessToken,
		ExpiresIn:   orig.ExpiresIn,
		TokenType:   orig.TokenType,
	}, nil
}

func (c *CachedGCPTokenGenerator) GenerateIDToken(ctx context.Context, serviceAccountEmail string, audience string) (*GCPIDToken, error) {
	key := idTokenCacheKey(serviceAccountEmail, audience)

	if tok := c.peekIDToken(key); tok != nil {
		return tok, nil
	}

	result, err, _ := c.idGroup.Do(key, func() (interface{}, error) {
		if tok := c.peekIDToken(key); tok != nil {
			return tok, nil
		}

		// Detach from the leader's request cancellation so one disconnected
		// caller does not fail concurrent waiters on the same singleflight.
		fetchCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), idTokenFetchTimeout)
		defer cancel()

		token, err := c.inner.GenerateIDToken(fetchCtx, serviceAccountEmail, audience)
		if err != nil {
			return nil, err
		}
		if token == nil {
			return nil, fmt.Errorf("gcp token generator returned nil ID token")
		}

		now := c.clock()
		cacheExp := now.Add(idTokenCacheTTL)
		if jwtExp, parseErr := transportauth.ParseTokenExpiry(token.Token); parseErr == nil {
			safeJWTExp := jwtExp.Add(-idTokenExpiryMargin)
			if safeJWTExp.Before(cacheExp) {
				cacheExp = safeJWTExp
			}
		}

		if now.Before(cacheExp) {
			c.storeIDToken(key, token, cacheExp, now)
		}

		return token, nil
	})
	if err != nil {
		return nil, err
	}

	orig := result.(*GCPIDToken)
	return &GCPIDToken{Token: orig.Token}, nil
}

func (c *CachedGCPTokenGenerator) peekIDToken(key string) *GCPIDToken {
	now := c.clock()
	c.idMu.RLock()
	entry := c.idCache[key]
	c.idMu.RUnlock()

	if entry != nil && now.Before(entry.expiresAt) {
		return &GCPIDToken{Token: entry.token.Token}
	}
	return nil
}

func (c *CachedGCPTokenGenerator) storeIDToken(key string, token *GCPIDToken, expiresAt, now time.Time) {
	c.idMu.Lock()
	defer c.idMu.Unlock()

	maxEntries := c.maxIDCacheEntries
	if maxEntries <= 0 {
		maxEntries = defaultMaxIDTokenCacheEntries
	}

	if _, exists := c.idCache[key]; !exists && len(c.idCache) >= maxEntries {
		// First sweep expired entries.
		for k, e := range c.idCache {
			if !now.Before(e.expiresAt) {
				delete(c.idCache, k)
			}
		}
		// If still at capacity, evict the entry expiring soonest.
		if len(c.idCache) >= maxEntries {
			var oldestKey string
			var oldestExp time.Time
			first := true
			for k, e := range c.idCache {
				if first || e.expiresAt.Before(oldestExp) {
					oldestKey = k
					oldestExp = e.expiresAt
					first = false
				}
			}
			if !first {
				delete(c.idCache, oldestKey)
			}
		}
	}

	c.idCache[key] = &cachedIDTokenEntry{
		token:     &GCPIDToken{Token: token.Token},
		expiresAt: expiresAt,
	}
}

func (c *CachedGCPTokenGenerator) VerifyImpersonation(ctx context.Context, serviceAccountEmail string) error {
	return c.inner.VerifyImpersonation(ctx, serviceAccountEmail)
}

func (c *CachedGCPTokenGenerator) ServiceAccountEmail() string {
	return c.inner.ServiceAccountEmail()
}
