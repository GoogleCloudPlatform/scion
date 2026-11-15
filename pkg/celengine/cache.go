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

package celengine

import (
	"container/list"
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"sync"
)

// DefaultCacheMaxEntries is the default maximum number of compiled programs
// retained in an Engine's LRU cache.
const DefaultCacheMaxEntries = 1024

// CacheConfig configures an Engine's content-addressed compiled-program LRU cache.
type CacheConfig struct {
	MaxEntries     int
	MaxPerConsumer int
}

// DefaultCacheConfig returns a CacheConfig with DefaultCacheMaxEntries (1024).
func DefaultCacheConfig() CacheConfig {
	return CacheConfig{MaxEntries: DefaultCacheMaxEntries}
}

func (c CacheConfig) normalize() (CacheConfig, error) {
	if c.MaxEntries < 0 {
		return CacheConfig{}, fmt.Errorf("celengine: CacheConfig.MaxEntries must be >= 0 (got %d)", c.MaxEntries)
	}
	if c.MaxPerConsumer < 0 {
		return CacheConfig{}, fmt.Errorf("celengine: CacheConfig.MaxPerConsumer must be >= 0 (got %d)", c.MaxPerConsumer)
	}
	if c.MaxEntries == 0 {
		c.MaxEntries = DefaultCacheMaxEntries
	}
	return c, nil
}

// CacheScope optionally associates a consumer row identity and revision with a
// cached program for scoped invalidation and per-consumer bounding. CacheScope
// is never part of the program's content identity.
type CacheScope struct {
	Consumer string
	ID       string
	Revision int64
}

func (s CacheScope) hasID() bool {
	return s.Consumer != "" && s.ID != ""
}

// CacheStats reports point-in-time metrics for an Engine's program cache.
type CacheStats struct {
	Entries int
	Hits    uint64
	Misses  uint64
}

type cacheIdentity [32]byte

func computeCacheIdentity(envName string, envVersion int, fnSetHash [32]byte, kind Kind, expr string, maxBytes int) cacheIdentity {
	h := sha256.New()
	writeField := func(s string) {
		var lenBuf [8]byte
		binary.BigEndian.PutUint64(lenBuf[:], uint64(len(s)))
		_, _ = h.Write(lenBuf[:])
		_, _ = h.Write([]byte(s))
	}
	writeInt := func(v int64) {
		var buf [8]byte
		binary.BigEndian.PutUint64(buf[:], uint64(v))
		_, _ = h.Write(buf[:])
	}
	writeField(envName)
	writeInt(int64(envVersion))
	_, _ = h.Write(fnSetHash[:])
	writeField(kind.Name)
	writeField(kind.Result.String())
	writeField(expr)
	writeInt(int64(maxBytes))
	var out cacheIdentity
	copy(out[:], h.Sum(nil))
	return out
}

type scopeKey struct {
	consumer string
	id       string
}

type scopeBinding struct {
	key      cacheIdentity
	revision int64
}

type cacheEntry struct {
	key        cacheIdentity
	prog       Program
	elem       *list.Element
	scopes     map[scopeKey]struct{}
	everScoped bool
}

type programCache struct {
	mu             sync.Mutex
	maxEntries     int
	maxPerConsumer int
	lru            *list.List // front = most recently used; Value = *cacheEntry
	byKey          map[cacheIdentity]*cacheEntry
	byScope        map[scopeKey]scopeBinding
	hits           uint64
	misses         uint64
}

func newProgramCache(cfg CacheConfig) *programCache {
	return &programCache{
		maxEntries:     cfg.MaxEntries,
		maxPerConsumer: cfg.MaxPerConsumer,
		lru:            list.New(),
		byKey:          make(map[cacheIdentity]*cacheEntry),
		byScope:        make(map[scopeKey]scopeBinding),
	}
}

func (c *programCache) get(id cacheIdentity, scope CacheScope) (Program, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()

	ent, ok := c.byKey[id]
	if !ok {
		c.misses++
		return Program{}, false
	}
	c.hits++
	c.lru.MoveToFront(ent.elem)
	if scope.hasID() {
		c.bindScopeLocked(scope, ent)
	}
	return ent.prog, true
}

func (c *programCache) put(id cacheIdentity, prog Program, scope CacheScope) Program {
	c.mu.Lock()
	defer c.mu.Unlock()

	if ent, ok := c.byKey[id]; ok {
		c.lru.MoveToFront(ent.elem)
		if scope.hasID() {
			c.bindScopeLocked(scope, ent)
		}
		return ent.prog
	}

	ent := &cacheEntry{
		key:    id,
		prog:   prog,
		scopes: make(map[scopeKey]struct{}),
	}
	ent.elem = c.lru.PushFront(ent)
	c.byKey[id] = ent

	if scope.hasID() {
		c.bindScopeLocked(scope, ent)
	}

	if c.maxPerConsumer > 0 && scope.Consumer != "" {
		c.enforceConsumerCapLocked(scope.Consumer, ent)
	}
	for c.lru.Len() > c.maxEntries {
		tail := c.lru.Back()
		if tail == nil {
			break
		}
		c.removeEntryLocked(tail.Value.(*cacheEntry))
	}
	return prog
}

func (c *programCache) bindScopeLocked(scope CacheScope, target *cacheEntry) {
	sk := scopeKey{consumer: scope.Consumer, id: scope.ID}
	if prev, exists := c.byScope[sk]; exists {
		if prev.key != target.key {
			c.detachScopeFromEntryLocked(sk, prev.key)
		}
	}
	c.byScope[sk] = scopeBinding{key: target.key, revision: scope.Revision}
	target.scopes[sk] = struct{}{}
	target.everScoped = true
}

func (c *programCache) detachScopeFromEntryLocked(sk scopeKey, id cacheIdentity) {
	ent, ok := c.byKey[id]
	if !ok {
		return
	}
	delete(ent.scopes, sk)
	if ent.everScoped && len(ent.scopes) == 0 {
		c.removeEntryLocked(ent)
	}
}

func (c *programCache) enforceConsumerCapLocked(consumer string, keep *cacheEntry) {
	for c.consumerEntryCountLocked(consumer) > c.maxPerConsumer {
		var victim *cacheEntry
		for e := c.lru.Back(); e != nil; e = e.Prev() {
			cand := e.Value.(*cacheEntry)
			if cand == keep {
				continue
			}
			if entryHasConsumer(cand, consumer) {
				victim = cand
				break
			}
		}
		if victim == nil {
			break
		}
		// Detach all scopes belonging to this consumer from victim; if no other
		// consumer references victim, remove it.
		for sk := range victim.scopes {
			if sk.consumer == consumer {
				delete(victim.scopes, sk)
				delete(c.byScope, sk)
			}
		}
		if len(victim.scopes) == 0 {
			c.removeEntryLocked(victim)
		}
	}
}

func (c *programCache) consumerEntryCountLocked(consumer string) int {
	n := 0
	for _, ent := range c.byKey {
		if entryHasConsumer(ent, consumer) {
			n++
		}
	}
	return n
}

func entryHasConsumer(ent *cacheEntry, consumer string) bool {
	for sk := range ent.scopes {
		if sk.consumer == consumer {
			return true
		}
	}
	return false
}

func (c *programCache) removeEntryLocked(ent *cacheEntry) {
	delete(c.byKey, ent.key)
	if ent.elem != nil {
		c.lru.Remove(ent.elem)
		ent.elem = nil
	}
	for sk := range ent.scopes {
		if b, ok := c.byScope[sk]; ok && b.key == ent.key {
			delete(c.byScope, sk)
		}
	}
}

func (c *programCache) invalidate(scope CacheScope) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if scope.Consumer == "" {
		return
	}
	if scope.ID == "" {
		for sk, b := range c.byScope {
			if sk.consumer == scope.Consumer {
				delete(c.byScope, sk)
				c.detachScopeFromEntryLocked(sk, b.key)
			}
		}
		return
	}
	sk := scopeKey{consumer: scope.Consumer, id: scope.ID}
	b, ok := c.byScope[sk]
	if !ok {
		return
	}
	if scope.Revision > 0 && b.revision > scope.Revision {
		return
	}
	delete(c.byScope, sk)
	c.detachScopeFromEntryLocked(sk, b.key)
}

func (c *programCache) stats() CacheStats {
	c.mu.Lock()
	defer c.mu.Unlock()
	return CacheStats{
		Entries: c.lru.Len(),
		Hits:    c.hits,
		Misses:  c.misses,
	}
}
