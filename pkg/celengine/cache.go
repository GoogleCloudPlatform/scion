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

// DefaultCacheMaxBytes is the default maximum cumulative program weight budget
// retained across cached programs (1 MiB: holds all 1,024 entries for <= 1 KiB
// rules and ~256 full 4 KiB non-regex rules — enough to keep an entire
// project's 200-trigger cap warm without per-event recompilation churn — while
// bounding worst-case retained AST+plan+RE2 heap at ~93-98 MiB vs ~337-671 MiB
// unbounded).
const DefaultCacheMaxBytes = 1 << 20

// RegexInstructionWeightBytes is the cache-weight charge per precompiled RE2
// instruction in a cached Program (3a security F1, ptone/scion#8030). Go's
// regexp.Regexp retains ~250-300B of heap per instruction (onePass + syntax
// tables), whereas non-regex CEL AST+plan entries retain ~85-95B of heap per
// source byte; charging 64 weight bytes per RE2 instruction keeps regex-heavy
// expressions well within the same heap-per-weight envelope.
const RegexInstructionWeightBytes = 64

// CacheConfig configures an Engine's content-addressed compiled-program LRU cache.
type CacheConfig struct {
	MaxEntries         int
	MaxBytes           int
	MaxPerConsumer     int
	MaxPerProject      int
	MaxBytesPerProject int
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
	if c.MaxPerProject < 0 {
		return CacheConfig{}, fmt.Errorf("celengine: CacheConfig.MaxPerProject must be >= 0 (got %d)", c.MaxPerProject)
	}
	if c.MaxBytesPerProject < 0 {
		return CacheConfig{}, fmt.Errorf("celengine: CacheConfig.MaxBytesPerProject must be >= 0 (got %d)", c.MaxBytesPerProject)
	}
	if c.MaxEntries == 0 {
		c.MaxEntries = DefaultCacheMaxEntries
	}
	if c.MaxBytes == 0 {
		c.MaxBytes = DefaultCacheMaxBytes
	}
	return c, nil
}

// CacheScope optionally associates a consumer, project, row identity, and
// revision with a cached program for scoped invalidation and per-consumer /
// per-project residency bounding. CacheScope is never part of the program's
// content identity.
type CacheScope struct {
	Consumer  string
	ProjectID string
	ID        string
	Revision  int64
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

type scopeRecord struct {
	projectID string
	revision  int64
	entries   map[cacheIdentity]struct{}
}

type cacheEntry struct {
	key        cacheIdentity
	prog       Program
	weight     int
	elem       *list.Element
	scopes     map[scopeKey]struct{}
	everScoped bool
}

type programCache struct {
	mu                 sync.Mutex
	maxEntries         int
	maxBytes           int
	maxPerConsumer     int
	maxPerProject      int
	maxBytesPerProject int
	bytes              int
	lru                *list.List // front = most recently used; Value = *cacheEntry
	byKey              map[cacheIdentity]*cacheEntry
	byScope            map[scopeKey]*scopeRecord
	hits               uint64
	misses             uint64
}

func newProgramCache(cfg CacheConfig) *programCache {
	return &programCache{
		maxEntries:         cfg.MaxEntries,
		maxBytes:           cfg.MaxBytes,
		maxPerConsumer:     cfg.MaxPerConsumer,
		maxPerProject:      cfg.MaxPerProject,
		maxBytesPerProject: cfg.MaxBytesPerProject,
		lru:                list.New(),
		byKey:              make(map[cacheIdentity]*cacheEntry),
		byScope:            make(map[scopeKey]*scopeRecord),
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
		c.enforceScopeCapsLocked(scope, ent)
	}
	return ent.prog, true
}

func (c *programCache) put(id cacheIdentity, exprBytes int, prog Program, scope CacheScope) Program {
	c.mu.Lock()
	defer c.mu.Unlock()

	if ent, ok := c.byKey[id]; ok {
		c.lru.MoveToFront(ent.elem)
		if scope.hasID() {
			c.bindScopeLocked(scope, ent)
			c.enforceScopeCapsLocked(scope, ent)
		}
		return ent.prog
	}

	weight := max(exprBytes, 1) + prog.regexCacheWeight()
	ent := &cacheEntry{
		key:    id,
		prog:   prog,
		weight: weight,
		scopes: make(map[scopeKey]struct{}),
	}
	ent.elem = c.lru.PushFront(ent)
	c.byKey[id] = ent
	c.bytes += weight

	if scope.hasID() {
		c.bindScopeLocked(scope, ent)
		c.enforceScopeCapsLocked(scope, ent)
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

func (c *programCache) enforceScopeCapsLocked(scope CacheScope, keep *cacheEntry) {
	if scope.Consumer == "" {
		return
	}
	if scope.ProjectID != "" && (c.maxPerProject > 0 || c.maxBytesPerProject > 0) {
		c.enforceProjectCapLocked(scope.Consumer, scope.ProjectID, keep)
	}
	if c.maxPerConsumer > 0 {
		c.enforceConsumerCapLocked(scope.Consumer, keep)
	}
}

func (c *programCache) bindScopeLocked(scope CacheScope, target *cacheEntry) {
	sk := scopeKey{consumer: scope.Consumer, id: scope.ID}

	rec, exists := c.byScope[sk]
	if !exists {
		c.byScope[sk] = &scopeRecord{
			projectID: scope.ProjectID,
			revision:  scope.Revision,
			entries:   map[cacheIdentity]struct{}{target.key: {}},
		}
		target.scopes[sk] = struct{}{}
		target.everScoped = true
		return
	}

	// Do not allow an older revision to clobber bindings recorded for a newer revision.
	if scope.Revision > 0 && rec.revision > scope.Revision {
		return
	}
	if scope.ProjectID != "" {
		rec.projectID = scope.ProjectID
	}

	// Attach target first so an unchanged expression across a revision bump is never
	// transiently left with zero scopes while detaching prior-revision entries.
	target.scopes[sk] = struct{}{}
	target.everScoped = true

	if scope.Revision > 0 && rec.revision > 0 && scope.Revision > rec.revision {
		for oldID := range rec.entries {
			if oldID == target.key {
				continue
			}
			c.detachScopeFromEntryLocked(sk, oldID)
			delete(rec.entries, oldID)
		}
		rec.revision = scope.Revision
		rec.entries[target.key] = struct{}{}
		return
	}

	rec.entries[target.key] = struct{}{}
	if scope.Revision > rec.revision {
		rec.revision = scope.Revision
	}
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
		// scope references victim, remove it.
		for sk := range victim.scopes {
			if sk.consumer == consumer {
				delete(victim.scopes, sk)
				if rec, ok := c.byScope[sk]; ok {
					delete(rec.entries, victim.key)
					if len(rec.entries) == 0 {
						delete(c.byScope, sk)
					}
				}
			}
		}
		if len(victim.scopes) == 0 {
			c.removeEntryLocked(victim)
		}
	}
}

func (c *programCache) enforceProjectCapLocked(consumer, projectID string, keep *cacheEntry) {
	for {
		count, bytes := c.projectUsageLocked(consumer, projectID)
		overCount := c.maxPerProject > 0 && count > c.maxPerProject
		overBytes := c.maxBytesPerProject > 0 && bytes > c.maxBytesPerProject
		if !overCount && !overBytes {
			break
		}
		var victim *cacheEntry
		for e := c.lru.Back(); e != nil; e = e.Prev() {
			cand := e.Value.(*cacheEntry)
			if cand == keep {
				continue
			}
			if c.entryHasProjectLocked(cand, consumer, projectID) {
				victim = cand
				break
			}
		}
		if victim == nil {
			break
		}
		for sk := range victim.scopes {
			if sk.consumer != consumer {
				continue
			}
			rec, ok := c.byScope[sk]
			if !ok || rec.projectID != projectID {
				continue
			}
			delete(victim.scopes, sk)
			delete(rec.entries, victim.key)
			if len(rec.entries) == 0 {
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

func (c *programCache) projectUsageLocked(consumer, projectID string) (entries, bytes int) {
	for _, ent := range c.byKey {
		if c.entryHasProjectLocked(ent, consumer, projectID) {
			entries++
			bytes += ent.weight
		}
	}
	return entries, bytes
}

func entryHasConsumer(ent *cacheEntry, consumer string) bool {
	for sk := range ent.scopes {
		if sk.consumer == consumer {
			return true
		}
	}
	return false
}

func (c *programCache) entryHasProjectLocked(ent *cacheEntry, consumer, projectID string) bool {
	for sk := range ent.scopes {
		if sk.consumer != consumer {
			continue
		}
		if rec, ok := c.byScope[sk]; ok && rec.projectID == projectID {
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
	for sk := range ent.scopes {
		if rec, ok := c.byScope[sk]; ok {
			delete(rec.entries, ent.key)
			if len(rec.entries) == 0 {
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
			if sk.consumer != scope.Consumer {
				continue
			}
			if scope.ProjectID != "" && rec.projectID != scope.ProjectID {
				continue
			}
			delete(c.byScope, sk)
			for id := range rec.entries {
				c.detachScopeFromEntryLocked(sk, id)
			}
		}
		return
	}
	sk := scopeKey{consumer: scope.Consumer, id: scope.ID}
	rec, ok := c.byScope[sk]
	if !ok {
		return
	}
	if scope.ProjectID != "" && rec.projectID != "" && rec.projectID != scope.ProjectID {
		return
	}
	if scope.Revision > 0 && rec.revision > scope.Revision {
		return
	}
	delete(c.byScope, sk)
	for id := range rec.entries {
		c.detachScopeFromEntryLocked(sk, id)
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
