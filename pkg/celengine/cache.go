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

// DefaultCacheMaxBytes is the default maximum cumulative expression source
// byte budget retained across cached programs (1 MiB: holds all 1,024 entries
// for <= 1 KiB rules and ~256 full 4 KiB rules — enough to keep an entire
// project's 200-trigger cap warm without per-event recompilation churn — while
// bounding worst-case retained AST+plan heap at ~93-98 MiB vs ~337 MiB
// unbounded).
const DefaultCacheMaxBytes = 1 << 20

// CacheConfig configures an Engine's content-addressed compiled-program LRU cache.
type CacheConfig struct {
	MaxEntries     int
	MaxBytes       int
	MaxPerConsumer int
}

// DefaultCacheConfig returns a CacheConfig with DefaultCacheMaxEntries (1024)
// and DefaultCacheMaxBytes (1 MiB).
func DefaultCacheConfig() CacheConfig {
	return CacheConfig{
		MaxEntries: DefaultCacheMaxEntries,
		MaxBytes:   DefaultCacheMaxBytes,
	}
}

func (c CacheConfig) normalize() (CacheConfig, error) {
	if c.MaxEntries < 0 {
		return CacheConfig{}, fmt.Errorf("celengine: CacheConfig.MaxEntries must be >= 0 (got %d)", c.MaxEntries)
	}
	if c.MaxBytes < 0 {
		return CacheConfig{}, fmt.Errorf("celengine: CacheConfig.MaxBytes must be >= 0 (got %d)", c.MaxBytes)
	}
	if c.MaxPerConsumer < 0 {
		return CacheConfig{}, fmt.Errorf("celengine: CacheConfig.MaxPerConsumer must be >= 0 (got %d)", c.MaxPerConsumer)
	}
	if c.MaxEntries == 0 {
		c.MaxEntries = DefaultCacheMaxEntries
	}
	if c.MaxBytes == 0 {
		c.MaxBytes = DefaultCacheMaxBytes
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
	Bytes   int
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

type slotKey struct {
	consumer string
	id       string
	kind     string
}

type scopeRecord struct {
	revision int64
	byKind   map[string]cacheIdentity
}

type cacheEntry struct {
	key        cacheIdentity
	prog       Program
	weight     int
	elem       *list.Element
	slots      map[slotKey]struct{}
	everScoped bool
}

type programCache struct {
	mu             sync.Mutex
	maxEntries     int
	maxBytes       int
	maxPerConsumer int
	bytes          int
	lru            *list.List // front = most recently used; Value = *cacheEntry
	byKey          map[cacheIdentity]*cacheEntry
	byScope        map[scopeKey]*scopeRecord
	hits           uint64
	misses         uint64
}

func newProgramCache(cfg CacheConfig) *programCache {
	return &programCache{
		maxEntries:     cfg.MaxEntries,
		maxBytes:       cfg.MaxBytes,
		maxPerConsumer: cfg.MaxPerConsumer,
		lru:            list.New(),
		byKey:          make(map[cacheIdentity]*cacheEntry),
		byScope:        make(map[scopeKey]*scopeRecord),
	}
}

func (c *programCache) get(id cacheIdentity, kindName string, scope CacheScope) (Program, bool) {
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
		c.bindScopeLocked(scope, kindName, ent)
	}
	return ent.prog, true
}

func (c *programCache) put(id cacheIdentity, kindName string, exprBytes int, prog Program, scope CacheScope) Program {
	c.mu.Lock()
	defer c.mu.Unlock()

	if ent, ok := c.byKey[id]; ok {
		c.lru.MoveToFront(ent.elem)
		if scope.hasID() {
			c.bindScopeLocked(scope, kindName, ent)
		}
		return ent.prog
	}

	weight := max(exprBytes, 1)
	ent := &cacheEntry{
		key:    id,
		prog:   prog,
		weight: weight,
		slots:  make(map[slotKey]struct{}),
	}
	ent.elem = c.lru.PushFront(ent)
	c.byKey[id] = ent
	c.bytes += weight

	if scope.hasID() {
		c.bindScopeLocked(scope, kindName, ent)
	}

	if c.maxPerConsumer > 0 && scope.Consumer != "" {
		c.enforceConsumerCapLocked(scope.Consumer, ent)
	}
	for c.lru.Len() > c.maxEntries || (c.maxBytes > 0 && c.bytes > c.maxBytes) {
		tail := c.lru.Back()
		if tail == nil {
			break
		}
		c.removeEntryLocked(tail.Value.(*cacheEntry))
	}
	return prog
}

func (c *programCache) bindScopeLocked(scope CacheScope, kindName string, target *cacheEntry) {
	sk := scopeKey{consumer: scope.Consumer, id: scope.ID}
	slot := slotKey{consumer: scope.Consumer, id: scope.ID, kind: kindName}

	rec, exists := c.byScope[sk]
	if !exists {
		c.byScope[sk] = &scopeRecord{
			revision: scope.Revision,
			byKind:   map[string]cacheIdentity{kindName: target.key},
		}
		target.slots[slot] = struct{}{}
		target.everScoped = true
		return
	}

	// Do not allow an older revision to clobber bindings recorded for a newer revision.
	if scope.Revision > 0 && rec.revision > scope.Revision {
		return
	}

	// Attach target first so an unchanged expression across a revision bump is never
	// transiently left with zero slots while detaching prior-revision kinds.
	target.slots[slot] = struct{}{}
	target.everScoped = true

	if scope.Revision > 0 && rec.revision > 0 && scope.Revision > rec.revision {
		for oldKind, oldID := range rec.byKind {
			if oldKind == kindName && oldID == target.key {
				continue
			}
			c.detachSlotFromEntryLocked(slotKey{consumer: sk.consumer, id: sk.id, kind: oldKind}, oldID)
			delete(rec.byKind, oldKind)
		}
		rec.revision = scope.Revision
		rec.byKind[kindName] = target.key
		return
	}

	if prevID, hadKind := rec.byKind[kindName]; hadKind && prevID != target.key {
		c.detachSlotFromEntryLocked(slot, prevID)
	}
	rec.byKind[kindName] = target.key
	if scope.Revision > rec.revision {
		rec.revision = scope.Revision
	}
}

func (c *programCache) detachSlotFromEntryLocked(slot slotKey, id cacheIdentity) {
	ent, ok := c.byKey[id]
	if !ok {
		return
	}
	delete(ent.slots, slot)
	if ent.everScoped && len(ent.slots) == 0 {
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
		// Detach all slots belonging to this consumer from victim; if no other
		// consumer references victim, remove it.
		for sl := range victim.slots {
			if sl.consumer == consumer {
				delete(victim.slots, sl)
				sk := scopeKey{consumer: sl.consumer, id: sl.id}
				if rec, ok := c.byScope[sk]; ok {
					if rec.byKind[sl.kind] == victim.key {
						delete(rec.byKind, sl.kind)
					}
					if len(rec.byKind) == 0 {
						delete(c.byScope, sk)
					}
				}
			}
		}
		if len(victim.slots) == 0 {
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
	for sl := range ent.slots {
		if sl.consumer == consumer {
			return true
		}
	}
	return false
}

func (c *programCache) removeEntryLocked(ent *cacheEntry) {
	if _, existed := c.byKey[ent.key]; existed {
		delete(c.byKey, ent.key)
		c.bytes -= ent.weight
		if c.bytes < 0 {
			c.bytes = 0
		}
	}
	if ent.elem != nil {
		c.lru.Remove(ent.elem)
		ent.elem = nil
	}
	for sl := range ent.slots {
		sk := scopeKey{consumer: sl.consumer, id: sl.id}
		if rec, ok := c.byScope[sk]; ok {
			if rec.byKind[sl.kind] == ent.key {
				delete(rec.byKind, sl.kind)
			}
			if len(rec.byKind) == 0 {
				delete(c.byScope, sk)
			}
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
		for sk, rec := range c.byScope {
			if sk.consumer == scope.Consumer {
				delete(c.byScope, sk)
				for k, id := range rec.byKind {
					c.detachSlotFromEntryLocked(slotKey{consumer: sk.consumer, id: sk.id, kind: k}, id)
				}
			}
		}
		return
	}
	sk := scopeKey{consumer: scope.Consumer, id: scope.ID}
	rec, ok := c.byScope[sk]
	if !ok {
		return
	}
	if scope.Revision > 0 && rec.revision > scope.Revision {
		return
	}
	delete(c.byScope, sk)
	for k, id := range rec.byKind {
		c.detachSlotFromEntryLocked(slotKey{consumer: sk.consumer, id: sk.id, kind: k}, id)
	}
}

func (c *programCache) stats() CacheStats {
	c.mu.Lock()
	defer c.mu.Unlock()
	return CacheStats{
		Entries: c.lru.Len(),
		Bytes:   c.bytes,
		Hits:    c.hits,
		Misses:  c.misses,
	}
}
