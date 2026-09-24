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
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/sync/singleflight"
)

// ---------------------------------------------------------------------------
// Caching decorator for GoogleCredentialValidator (design §4.2(iii)).
//
// Wraps a base GoogleCredentialValidator (the shared instance also used by
// GEExchangeService, see server.go) with a bounded, per-instance cache of
// validation results, keyed by the credential itself and the audience it was
// checked against. This exists to blunt two costs on the external-bearer
// path, which — unlike the exchange endpoint — runs on every request rather
// than once per sign-in:
//   - ID tokens are cheap (local JWKS signature check), but a client that
//     re-presents the same token on every request would otherwise re-verify
//     it every time.
//   - Access tokens require two outbound Google calls per validation (see
//     google_credential_validator.go for which endpoints); without a cache, N
//     requests with the same token cost N round trips to Google.
//
// No package-level state: the cache lives entirely on the instance returned
// by NewCachingGoogleCredentialValidator, which the caller (server.go) stores
// on Server/AuthConfig (design §4.2(iii), I4).
// ---------------------------------------------------------------------------

const (
	// defaultGoogleCredentialCacheMaxTTL is the upper bound on how long a
	// successful validation is cached, regardless of the credential's own
	// remaining lifetime.
	defaultGoogleCredentialCacheMaxTTL = 5 * time.Minute
	// defaultGoogleCredentialCacheNegTTL is how long a rejected credential's
	// negative result is cached, to blunt repeated-garbage-token
	// amplification (design §4.2(iii), C5).
	defaultGoogleCredentialCacheNegTTL = 30 * time.Second
	// defaultGoogleCredentialCacheMaxEntries bounds cache memory under
	// hostile unique-token churn. When full, new keys are refused rather
	// than evicting a live entry (no LRU).
	defaultGoogleCredentialCacheMaxEntries = 10000
)

// CacheOption configures a NewCachingGoogleCredentialValidator instance.
type CacheOption func(*cachingGoogleCredentialValidator)

// WithCacheMaxTTL overrides the default 5-minute positive-cache ceiling.
func WithCacheMaxTTL(d time.Duration) CacheOption {
	return func(c *cachingGoogleCredentialValidator) { c.maxTTL = d }
}

// WithCacheNegativeTTL overrides the default 30-second negative-cache TTL.
func WithCacheNegativeTTL(d time.Duration) CacheOption {
	return func(c *cachingGoogleCredentialValidator) { c.negTTL = d }
}

// WithCacheMaxEntries overrides the default 10000-entry cap.
func WithCacheMaxEntries(n int) CacheOption {
	return func(c *cachingGoogleCredentialValidator) { c.maxEntries = n }
}

// withCacheNowFunc overrides the cache's clock. Test-only (unexported): C3
// (TTL never outlives UpstreamExpiry) and C4/eviction tests need to advance
// time deterministically instead of sleeping.
func withCacheNowFunc(now func() time.Time) CacheOption {
	return func(c *cachingGoogleCredentialValidator) { c.now = now }
}

// WithCacheMetrics wires the cache-outcome counter (design §4.7:
// scion_hub_google_validator_cache_total). nil (the default) disables
// recording. See SetMetrics for wiring this after construction, which
// production needs: server.go's New() builds this decorator before an OTel
// MeterProvider exists (cmd/server_foreground.go builds one only once the
// server's Hub ID is known).
func WithCacheMetrics(m GoogleValidatorCacheMetricsRecorder) CacheOption {
	return func(c *cachingGoogleCredentialValidator) { c.metrics.Store(&m) }
}

// googleCredCacheEntry is a completed validation result, positive or negative.
type googleCredCacheEntry struct {
	identity  *ValidatedGoogleIdentity // nil for a negative (error) entry
	err       error
	expiresAt time.Time
}

// cachingGoogleCredentialValidator wraps a base GoogleCredentialValidator
// with a bounded cache and singleflight collapsing of concurrent first
// requests for the same credential. It implements GoogleCredentialValidator
// itself, so it is a drop-in decorator.
type cachingGoogleCredentialValidator struct {
	base GoogleCredentialValidator

	maxTTL     time.Duration
	negTTL     time.Duration
	maxEntries int
	now        func() time.Time

	mu      sync.Mutex
	entries map[string]googleCredCacheEntry

	group singleflight.Group

	// metrics records hit/miss/negative_hit (design §4.7). A plain
	// atomic.Pointer, not a mutex-guarded field, so the hot Validate*
	// path never contends with SetMetrics (called once, at startup, from a
	// different goroutine — see server.go's SetGoogleValidatorCacheMetrics).
	metrics atomic.Pointer[GoogleValidatorCacheMetricsRecorder]
}

// SetMetrics wires (or rewires) the cache-outcome counter after
// construction. Safe for concurrent use with ValidateIDToken/
// ValidateAccessToken.
func (c *cachingGoogleCredentialValidator) SetMetrics(m GoogleValidatorCacheMetricsRecorder) {
	c.metrics.Store(&m)
}

// recordCache is nil-safe: SetMetrics/WithCacheMetrics is never called (most
// tests, and any production server before its OTel exporter is wired), or is
// called with a nil recorder (defensive).
func (c *cachingGoogleCredentialValidator) recordCache(result GoogleValidatorCacheResult) {
	rec := c.metrics.Load()
	if rec == nil || *rec == nil {
		return
	}
	(*rec).RecordGoogleValidatorCache(result)
}

// cacheResultFor classifies a completed cache entry's error as a positive or
// negative cache result for the metric above. It does not decide whether the
// entry was actually served from cache — callers only invoke it once they
// know that.
func cacheResultFor(err error) GoogleValidatorCacheResult {
	if err != nil {
		return GoogleValidatorCacheNegativeHit
	}
	return GoogleValidatorCacheHit
}

// NewCachingGoogleCredentialValidator wraps v with a bounded, per-instance
// cache of successful and (selectively) failed validations. See the package
// doc comment above for the caching policy; defaults match design §4.2(iii):
// maxTTL 5m, negTTL 30s, maxEntries 10000.
func NewCachingGoogleCredentialValidator(v GoogleCredentialValidator, opts ...CacheOption) GoogleCredentialValidator {
	c := &cachingGoogleCredentialValidator{
		base:       v,
		maxTTL:     defaultGoogleCredentialCacheMaxTTL,
		negTTL:     defaultGoogleCredentialCacheNegTTL,
		maxEntries: defaultGoogleCredentialCacheMaxEntries,
		now:        time.Now,
		entries:    make(map[string]googleCredCacheEntry),
	}
	for _, opt := range opts {
		opt(c)
	}
	return c
}

// cacheKey derives the cache key sha256(token) || sha256(sorted(aud)). The
// audience list is sorted before hashing so the same set of allowed client
// IDs in a different order still hits the same entry; each half is hashed
// separately (rather than hashing the concatenation) so the two components
// can never collide into one another (e.g. token "ab" + aud "c" vs token "a"
// + aud "bc").
func cacheKey(token string, allowedClientIDs []string) string {
	tokenSum := sha256.Sum256([]byte(token))
	sorted := append([]string(nil), allowedClientIDs...)
	sort.Strings(sorted)
	audSum := sha256.Sum256([]byte(strings.Join(sorted, "\x00")))
	return hex.EncodeToString(tokenSum[:]) + "." + hex.EncodeToString(audSum[:])
}

// negativelyCacheableGoogleError reports whether err is one of the four
// errors design §4.2(iii) allows to be cached negatively. ErrGoogleUpstreamError
// is deliberately excluded — and so is every other error not on this list —
// so a transient upstream blip, or any credential fault the design didn't
// explicitly vet for negative caching, is never remembered against the
// caller: the next request always retries upstream (C4).
func negativelyCacheableGoogleError(err error) bool {
	return errors.Is(err, ErrGoogleInvalidCredential) ||
		errors.Is(err, ErrGoogleExpiredCredential) ||
		errors.Is(err, ErrGoogleUntrustedAudience) ||
		errors.Is(err, ErrGoogleUnverifiedEmail)
}

// Cached reports whether a validation result for (token, allowedClientIDs) is
// currently live in the cache. auth_external_bearer.go's rate limiter uses
// this (via a type assertion) to skip rate-limiting on cache hits (design
// §4.4: the limiter is consulted "only on cache misses").
func (c *cachingGoogleCredentialValidator) Cached(token string, allowedClientIDs []string) bool {
	_, ok := c.lookup(cacheKey(token, allowedClientIDs))
	return ok
}

func (c *cachingGoogleCredentialValidator) lookup(key string) (googleCredCacheEntry, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.entries[key]
	if !ok {
		return googleCredCacheEntry{}, false
	}
	if !c.now().Before(e.expiresAt) {
		delete(c.entries, key)
		return googleCredCacheEntry{}, false
	}
	return e, true
}

// store inserts a completed result, subject to the negative-caching
// allowlist and the maxEntries bound. It evicts expired entries once to try
// to make room, and refuses the insert (rather than evicting a live entry)
// if the cache is still full afterwards — there is no LRU (design §4.2(iii)).
// A refused insert only means the next request re-validates; it never
// changes the result returned to the current caller.
func (c *cachingGoogleCredentialValidator) store(key string, identity *ValidatedGoogleIdentity, err error) {
	var ttl time.Duration
	switch {
	case err == nil:
		ttl = c.maxTTL
		// Use c.now(), not the wall clock (time.Until), so tests that inject
		// a fake clock exercise the real expiry arithmetic instead of
		// silently mixing clocks (review r1 nit 7).
		if remaining := identity.UpstreamExpiry.Sub(c.now()); remaining < ttl {
			ttl = remaining
		}
		if ttl <= 0 {
			return // already at/after its own expiry: nothing useful to cache
		}
	case negativelyCacheableGoogleError(err):
		ttl = c.negTTL
	default:
		return // ErrGoogleUpstreamError, or anything not on the allowlist: never cached
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	if _, exists := c.entries[key]; !exists && len(c.entries) >= c.maxEntries {
		c.evictExpiredLocked()
		if len(c.entries) >= c.maxEntries {
			return // still full: refuse to insert
		}
	}
	c.entries[key] = googleCredCacheEntry{identity: identity, err: err, expiresAt: c.now().Add(ttl)}
}

func (c *cachingGoogleCredentialValidator) evictExpiredLocked() {
	now := c.now()
	for k, e := range c.entries {
		if !now.Before(e.expiresAt) {
			delete(c.entries, k)
		}
	}
}

// upstreamCallTimeout bounds the upstream call singleflight makes on behalf
// of every waiter, once detached from whichever caller happens to be the
// leader (see validate below). It matches NewGoogleCredentialValidator's
// default http.Client timeout, so in the common case the HTTP client's own
// timeout fires first and this is only a backstop against a base validator
// configured with a longer or absent timeout.
const upstreamCallTimeout = 10 * time.Second

// validate is the shared cache/singleflight wrapper around a single upstream
// call, used by both ValidateIDToken and ValidateAccessToken. upstream is
// called with a context that is independent of any specific caller's ctx
// (see the WithoutCancel comment below), even though it was built from the
// ctx of whichever caller happens to become the singleflight leader.
// googleCredValidateResult is validate's singleflight.Do return type: the
// completed entry, plus whether producing it actually required an upstream
// call. Every waiter on a given key — the leader that ran the callback and
// every follower collapsed into it — receives the same value from Do, so
// tagging viaUpstream here (rather than recomputing it per-caller) is what
// lets every one of them record the correct cache-outcome metric (design
// §4.7), not just the leader.
type googleCredValidateResult struct {
	entry       googleCredCacheEntry
	viaUpstream bool
}

func (c *cachingGoogleCredentialValidator) validate(
	ctx context.Context, token string, allowedClientIDs []string,
	upstream func(context.Context) (*ValidatedGoogleIdentity, error),
) (*ValidatedGoogleIdentity, error) {
	key := cacheKey(token, allowedClientIDs)
	if e, ok := c.lookup(key); ok {
		c.recordCache(cacheResultFor(e.err))
		return e.identity, e.err
	}

	// singleflight collapses concurrent first requests for the same key into
	// one upstream call (design §4.2(iii), C6).
	v, _, _ := c.group.Do(key, func() (interface{}, error) {
		// Re-check: a concurrent Do call for a *different* key that finished
		// first, or a request that arrived just as the previous flight for
		// this key completed, may already have populated the cache.
		if e, ok := c.lookup(key); ok {
			return googleCredValidateResult{entry: e}, nil
		}
		// Detach from the leader's own request context: this call is shared
		// by every waiter on this key, so the leader cancelling (or timing
		// out) its own request must not fail every follower's request too
		// (review r1 finding 4). Still bounded by upstreamCallTimeout so a
		// leaderless call can't hang forever.
		upstreamCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), upstreamCallTimeout)
		defer cancel()
		identity, err := upstream(upstreamCtx)
		c.store(key, identity, err)
		return googleCredValidateResult{entry: googleCredCacheEntry{identity: identity, err: err}, viaUpstream: true}, nil
	})
	result := v.(googleCredValidateResult)
	if result.viaUpstream {
		c.recordCache(GoogleValidatorCacheMiss)
	} else {
		c.recordCache(cacheResultFor(result.entry.err))
	}
	return result.entry.identity, result.entry.err
}

// ValidateIDToken implements GoogleCredentialValidator, serving from cache
// when possible and delegating to the base validator on a miss.
func (c *cachingGoogleCredentialValidator) ValidateIDToken(ctx context.Context, token string, allowedClientIDs []string) (*ValidatedGoogleIdentity, error) {
	return c.validate(ctx, token, allowedClientIDs, func(upstreamCtx context.Context) (*ValidatedGoogleIdentity, error) {
		return c.base.ValidateIDToken(upstreamCtx, token, allowedClientIDs)
	})
}

// ValidateAccessToken implements GoogleCredentialValidator, serving from
// cache when possible and delegating to the base validator on a miss.
func (c *cachingGoogleCredentialValidator) ValidateAccessToken(ctx context.Context, token string, allowedClientIDs []string) (*ValidatedGoogleIdentity, error) {
	return c.validate(ctx, token, allowedClientIDs, func(upstreamCtx context.Context) (*ValidatedGoogleIdentity, error) {
		return c.base.ValidateAccessToken(upstreamCtx, token, allowedClientIDs)
	})
}
