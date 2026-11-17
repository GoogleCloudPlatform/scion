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
	OnEvictScan        func(steps int)
}

// EndpointCache caches webhook endpoint rows, unsealed secrets keyed by
// (endpoint_id, secret_version), and a bounded negative cache of unknown IDs
// per replica (§8.1 step 3, F4, F14).
type EndpointCache struct {
	mu             sync.Mutex
	endpoints      *boundedTable[string, cachedEndpoint]
	secrets        *boundedTable[secretCacheKey, cachedSecret]
	secretVersions map[string]map[int]struct{}
	unknown        *boundedTable[string, time.Time]
	posTTL         time.Duration
	negTTL         time.Duration
	now            func() time.Time
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
	c := &EndpointCache{
		endpoints:      newBoundedTable[string, cachedEndpoint](maxPos, cfg.OnEvictScan, nil),
		secretVersions: make(map[string]map[int]struct{}),
		unknown:        newBoundedTable[string, time.Time](maxNeg, cfg.OnEvictScan, nil),
		posTTL:         posTTL,
		negTTL:         negTTL,
		now:            nowFn,
	}
	c.secrets = newBoundedTable[secretCacheKey, cachedSecret](maxPos, cfg.OnEvictScan, func(k secretCacheKey, _ cachedSecret) {
		if vers := c.secretVersions[k.endpointID]; vers != nil {
			delete(vers, k.version)
			if len(vers) == 0 {
				delete(c.secretVersions, k.endpointID)
			}
		}
	})
	return c
}

// LookupEndpoint resolves endpointID from the cache or ts.GetWebhookEndpoint.
// Unknown IDs (store.ErrNotFound) are cached in the bounded negative cache and
// returned as (nil, store.ErrNotFound). Other store errors are returned without
// negative-caching.
func (c *EndpointCache) LookupEndpoint(ctx context.Context, ts store.TriggerStore, endpointID string) (*store.WebhookEndpoint, error) {
	now := c.now()

	c.mu.Lock()
	if exp, neg := c.unknown.get(endpointID); neg {
		if now.Before(exp) {
			c.unknown.recordScan(0)
			c.mu.Unlock()
			return nil, store.ErrNotFound
		}
		c.unknown.delete(endpointID)
	}
	if entry, ok := c.endpoints.get(endpointID); ok {
		if now.Before(entry.expiresAt) {
			c.endpoints.recordScan(0)
			ep := entry.ep
			c.mu.Unlock()
			return &ep, nil
		}
		c.endpoints.delete(endpointID)
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
	c.unknown.putEvictOldest(endpointID, now.Add(c.negTTL))
}

func (c *EndpointCache) putEndpoint(ep store.WebhookEndpoint, now time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.unknown.delete(ep.ID)
	c.endpoints.putEvictOldest(ep.ID, cachedEndpoint{
		ep:        ep,
		expiresAt: now.Add(c.posTTL),
	})
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
	if entry, ok := c.secrets.get(key); ok {
		if now.Before(entry.expiresAt) {
			c.secrets.recordScan(0)
			sec := entry.secret
			c.mu.Unlock()
			return sec, nil
		}
		c.secrets.delete(key)
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
	c.secrets.putEvictOldest(key, cachedSecret{
		secret:    sec,
		expiresAt: c.now().Add(c.posTTL),
	})
	if c.secrets.len() > 0 {
		vers := c.secretVersions[endpointID]
		if vers == nil {
			vers = make(map[int]struct{})
			c.secretVersions[endpointID] = vers
		}
		vers[version] = struct{}{}
	}
	c.mu.Unlock()

	return sec, nil
}

// Invalidate evicts cached endpoint metadata, negative entries, and any cached
// secret versions for endpointID on this replica in O(versions for endpointID).
func (c *EndpointCache) Invalidate(endpointID string) {
	if c == nil || endpointID == "" {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.endpoints.delete(endpointID)
	c.unknown.delete(endpointID)
	vers := c.secretVersions[endpointID]
	delete(c.secretVersions, endpointID)
	for v := range vers {
		c.secrets.delete(secretCacheKey{endpointID: endpointID, version: v})
	}
}

// NegativeLen returns the current negative-cache entry count (for tests).
func (c *EndpointCache) NegativeLen() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.unknown.len()
}

// PositiveLen returns the current positive endpoint-cache entry count (for tests).
func (c *EndpointCache) PositiveLen() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.endpoints.len()
}

// SecretLen returns the current secret-cache entry count (for tests).
func (c *EndpointCache) SecretLen() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.secrets.len()
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
