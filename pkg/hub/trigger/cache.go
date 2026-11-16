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

package trigger

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// Cache and counter-flush defaults (§4.1 F8, §8.1 step 3 F4/F14).
const (
	EndpointCacheTTL        = 5 * time.Minute
	NegativeCacheTTL        = 60 * time.Second
	MaxNegativeCacheEntries = 10000
	MaxPositiveCacheEntries = 10000
	CounterFlushInterval    = 10 * time.Second
)

type cachedEndpoint struct {
	ep        store.WebhookEndpoint
	expiresAt time.Time
}

type secretCacheKey struct {
	endpointID string
	version    int
}

type cachedSecret struct {
	secret    string
	expiresAt time.Time
}

// EndpointCacheConfig overrides cache TTLs/bounds for deterministic tests.
type EndpointCacheConfig struct {
	PositiveTTL        time.Duration
	NegativeTTL        time.Duration
	MaxPositiveEntries int
	MaxNegativeEntries int
	Now                func() time.Time
}

// EndpointCache caches webhook endpoint rows, unsealed secrets keyed by
// (endpoint_id, secret_version), and a bounded negative cache of unknown IDs
// per replica (§8.1 step 3, F4, F14).
type EndpointCache struct {
	mu        sync.Mutex
	endpoints map[string]cachedEndpoint
	secrets   map[secretCacheKey]cachedSecret
	unknown   map[string]time.Time
	posTTL    time.Duration
	negTTL    time.Duration
	maxPos    int
	maxNeg    int
	now       func() time.Time
}

// NewEndpointCache constructs a per-replica EndpointCache.
func NewEndpointCache(cfg EndpointCacheConfig) *EndpointCache {
	posTTL := cfg.PositiveTTL
	if posTTL <= 0 {
		posTTL = EndpointCacheTTL
	}
	negTTL := cfg.NegativeTTL
	if negTTL <= 0 {
		negTTL = NegativeCacheTTL
	}
	maxPos := cfg.MaxPositiveEntries
	if maxPos <= 0 {
		maxPos = MaxPositiveCacheEntries
	}
	maxNeg := cfg.MaxNegativeEntries
	if maxNeg <= 0 {
		maxNeg = MaxNegativeCacheEntries
	}
	nowFn := cfg.Now
	if nowFn == nil {
		nowFn = time.Now
	}
	return &EndpointCache{
		endpoints: make(map[string]cachedEndpoint),
		secrets:   make(map[secretCacheKey]cachedSecret),
		unknown:   make(map[string]time.Time),
		posTTL:    posTTL,
		negTTL:    negTTL,
		maxPos:    maxPos,
		maxNeg:    maxNeg,
		now:       nowFn,
	}
}

// LookupEndpoint resolves endpointID from the cache or ts.GetWebhookEndpoint.
// Unknown IDs (store.ErrNotFound) are cached in the bounded negative cache and
// returned as (nil, store.ErrNotFound). Other store errors are returned without
// negative-caching.
func (c *EndpointCache) LookupEndpoint(ctx context.Context, ts store.TriggerStore, endpointID string) (*store.WebhookEndpoint, error) {
	now := c.now()

	c.mu.Lock()
	if exp, neg := c.unknown[endpointID]; neg {
		if now.Before(exp) {
			c.mu.Unlock()
			return nil, store.ErrNotFound
		}
		delete(c.unknown, endpointID)
	}
	if entry, ok := c.endpoints[endpointID]; ok {
		if now.Before(entry.expiresAt) {
			ep := entry.ep
			c.mu.Unlock()
			return &ep, nil
		}
		delete(c.endpoints, endpointID)
	}
	c.mu.Unlock()

	ep, err := ts.GetWebhookEndpoint(ctx, endpointID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			c.putUnknown(endpointID, c.now())
			return nil, store.ErrNotFound
		}
		return nil, err
	}

	c.putEndpoint(*ep, c.now())
	cp := *ep
	return &cp, nil
}

func (c *EndpointCache) putUnknown(endpointID string, now time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if _, exists := c.unknown[endpointID]; !exists && len(c.unknown) >= c.maxNeg {
		var oldestKey string
		var oldestExp time.Time
		evictedExpired := false
		for k, exp := range c.unknown {
			if !now.Before(exp) {
				delete(c.unknown, k)
				evictedExpired = true
				if len(c.unknown) < c.maxNeg {
					break
				}
				continue
			}
			if oldestKey == "" || exp.Before(oldestExp) {
				oldestKey = k
				oldestExp = exp
			}
		}
		if !evictedExpired && len(c.unknown) >= c.maxNeg && oldestKey != "" {
			delete(c.unknown, oldestKey)
		}
	}
	c.unknown[endpointID] = now.Add(c.negTTL)
}

func (c *EndpointCache) putEndpoint(ep store.WebhookEndpoint, now time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()

	delete(c.unknown, ep.ID)
	if _, exists := c.endpoints[ep.ID]; !exists && len(c.endpoints) >= c.maxPos {
		var oldestKey string
		var oldestExp time.Time
		for k, v := range c.endpoints {
			if !now.Before(v.expiresAt) {
				delete(c.endpoints, k)
				if len(c.endpoints) < c.maxPos {
					break
				}
				continue
			}
			if oldestKey == "" || v.expiresAt.Before(oldestExp) {
				oldestKey = k
				oldestExp = v.expiresAt
			}
		}
		if len(c.endpoints) >= c.maxPos && oldestKey != "" {
			delete(c.endpoints, oldestKey)
		}
	}
	c.endpoints[ep.ID] = cachedEndpoint{
		ep:        ep,
		expiresAt: now.Add(c.posTTL),
	}
}

// SecretLoader loads and unseals the plaintext secret string for
// (projectID, endpointID, version).
type SecretLoader func(ctx context.Context, projectID, endpointID string, version int) (string, error)

// LookupSecret resolves the unsealed signing secret keyed by
// (endpointID, version) with a 5-minute TTL (§8.1 step 3, F14). Failures are
// never cached so transient backend issues recover immediately.
func (c *EndpointCache) LookupSecret(ctx context.Context, loader SecretLoader, projectID, endpointID string, version int) (string, error) {
	now := c.now()
	key := secretCacheKey{endpointID: endpointID, version: version}

	c.mu.Lock()
	if entry, ok := c.secrets[key]; ok {
		if now.Before(entry.expiresAt) {
			sec := entry.secret
			c.mu.Unlock()
			return sec, nil
		}
		delete(c.secrets, key)
	}
	c.mu.Unlock()

	if loader == nil {
		return "", errors.New("webhook secret loader is not configured")
	}
	sec, err := loader(ctx, projectID, endpointID, version)
	if err != nil {
		return "", err
	}
	if sec == "" {
		return "", errors.New("webhook secret resolved to empty string")
	}

	c.mu.Lock()
	if _, exists := c.secrets[key]; !exists && len(c.secrets) >= c.maxPos {
		var oldestKey secretCacheKey
		var oldestExp time.Time
		hasOldest := false
		for k, v := range c.secrets {
			if !now.Before(v.expiresAt) {
				delete(c.secrets, k)
				if len(c.secrets) < c.maxPos {
					break
				}
				continue
			}
			if !hasOldest || v.expiresAt.Before(oldestExp) {
				oldestKey = k
				oldestExp = v.expiresAt
				hasOldest = true
			}
		}
		if len(c.secrets) >= c.maxPos && hasOldest {
			delete(c.secrets, oldestKey)
		}
	}
	c.secrets[key] = cachedSecret{
		secret:    sec,
		expiresAt: c.now().Add(c.posTTL),
	}
	c.mu.Unlock()

	return sec, nil
}

// Invalidate evicts cached endpoint metadata, negative entries, and any cached
// secret versions for endpointID on this replica.
func (c *EndpointCache) Invalidate(endpointID string) {
	if c == nil || endpointID == "" {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.endpoints, endpointID)
	delete(c.unknown, endpointID)
	for k := range c.secrets {
		if k.endpointID == endpointID {
			delete(c.secrets, k)
		}
	}
}

// NegativeLen returns the current negative-cache entry count (for tests).
func (c *EndpointCache) NegativeLen() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.unknown)
}

type endpointCounterDelta struct {
	receiveDelta   int
	rejectDelta    int
	lastReceivedAt *time.Time
}

// CounterAccumulator buffers per-endpoint receive_count, reject_count, and
// last_received_at increments in memory and flushes them periodically (every
// 10 s by default) so unauthenticated or refused requests never cause a DB row
// write on the request path (§4.1 F8).
type CounterAccumulator struct {
	mu       sync.Mutex
	pending  map[string]endpointCounterDelta
	interval time.Duration
	closed   bool
}

// NewCounterAccumulator constructs an in-memory endpoint counter buffer.
func NewCounterAccumulator(flushInterval time.Duration) *CounterAccumulator {
	if flushInterval <= 0 {
		flushInterval = CounterFlushInterval
	}
	return &CounterAccumulator{
		pending:  make(map[string]endpointCounterDelta),
		interval: flushInterval,
	}
}

// RecordAccepted records an accepted (queued) webhook delivery in memory.
func (a *CounterAccumulator) RecordAccepted(endpointID string, receivedAt time.Time) {
	if a == nil || endpointID == "" {
		return
	}
	ts := receivedAt.UTC()
	a.mu.Lock()
	defer a.mu.Unlock()
	d := a.pending[endpointID]
	d.receiveDelta++
	if d.lastReceivedAt == nil || ts.After(*d.lastReceivedAt) {
		d.lastReceivedAt = &ts
	}
	a.pending[endpointID] = d
}

// RecordRejected records a refused (400/401/413/415) webhook request for a
// known active endpoint in memory without touching storage.
func (a *CounterAccumulator) RecordRejected(endpointID string) {
	if a == nil || endpointID == "" {
		return
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	d := a.pending[endpointID]
	d.rejectDelta++
	a.pending[endpointID] = d
}

// PendingLen reports how many endpoints currently hold unflushed counter deltas.
func (a *CounterAccumulator) PendingLen() int {
	if a == nil {
		return 0
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	return len(a.pending)
}

// Flush drains all accumulated deltas to ts.FlushWebhookEndpointCounters.
// Deleted endpoints (store.ErrNotFound) are dropped silently; transient store
// errors restore that endpoint's delta for the next pass and return the first
// error encountered.
func (a *CounterAccumulator) Flush(ctx context.Context, ts store.TriggerStore) error {
	if a == nil || ts == nil {
		return nil
	}
	a.mu.Lock()
	if len(a.pending) == 0 {
		a.mu.Unlock()
		return nil
	}
	snapshot := a.pending
	a.pending = make(map[string]endpointCounterDelta)
	a.mu.Unlock()

	var firstErr error
	for id, d := range snapshot {
		if d.receiveDelta == 0 && d.rejectDelta == 0 && d.lastReceivedAt == nil {
			continue
		}
		err := ts.FlushWebhookEndpointCounters(ctx, id, d.receiveDelta, d.rejectDelta, d.lastReceivedAt)
		if err == nil || errors.Is(err, store.ErrNotFound) {
			continue
		}
		if firstErr == nil {
			firstErr = err
		}
		// Restore unflushed delta so transient failures do not drop counts.
		a.mu.Lock()
		cur := a.pending[id]
		cur.receiveDelta += d.receiveDelta
		cur.rejectDelta += d.rejectDelta
		if d.lastReceivedAt != nil && (cur.lastReceivedAt == nil || d.lastReceivedAt.After(*cur.lastReceivedAt)) {
			tsCopy := *d.lastReceivedAt
			cur.lastReceivedAt = &tsCopy
		}
		a.pending[id] = cur
		a.mu.Unlock()
	}
	return firstErr
}

// StartLoop flushes accumulated endpoint counters on a.interval until ctx is
// cancelled.
func (a *CounterAccumulator) StartLoop(ctx context.Context, ts store.TriggerStore, logger *slog.Logger) {
	if a == nil || ts == nil {
		return
	}
	if logger == nil {
		logger = slog.Default()
	}
	go func() {
		ticker := time.NewTicker(a.interval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				if err := a.Flush(ctx, ts); err != nil && ctx.Err() == nil {
					logger.Warn("Failed to flush webhook endpoint counters", "error", err)
				}
			}
		}
	}()
}

// Close performs one final best-effort flush on shutdown (idempotent).
func (a *CounterAccumulator) Close(ctx context.Context, ts store.TriggerStore) {
	if a == nil || ts == nil {
		return
	}
	a.mu.Lock()
	if a.closed {
		a.mu.Unlock()
		return
	}
	a.closed = true
	a.mu.Unlock()
	_ = a.Flush(ctx, ts)
}
