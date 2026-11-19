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
	// OnCapCheck is an optional test/instrumentation hook invoked whenever a
	// newly bound scope checks per-project / per-consumer caps, reporting the
	// number of LRU candidates inspected during eviction (0 when within cap).
	// Already-bound scoped cache hits bypass cap checks completely.
	OnCapCheck func(evictSteps int)
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

type projectKey struct {
	consumer  string
	projectID string
}

type projectStats struct {
	entries int
	bytes   int
}

type scopeRecord struct {
	projectID string
	revision  int64
	entries   map[cacheIdentity]struct{}
}

type cacheEntry struct {
	key          cacheIdentity
	prog         Program
	weight       int
	elem         *list.Element
	scopes       map[scopeKey]string // scopeKey -> projectID recorded for that link
	consumerRefs map[string]int      // consumer -> count of attached scopeKeys
	projectRefs  map[projectKey]int  // (consumer, projectID) -> count of attached scopeKeys
	everScoped   bool
}

type programCache struct {
	mu                 sync.Mutex
	maxEntries         int
	maxBytes           int
	maxPerConsumer     int
	maxPerProject      int
	maxBytesPerProject int
	onCapCheck         func(evictSteps int)
	bytes              int
	lru                *list.List // front = most recently used; Value = *cacheEntry
	byKey              map[cacheIdentity]*cacheEntry
	byScope            map[scopeKey]*scopeRecord
	consumerCounts     map[string]int
	projectUsage       map[projectKey]projectStats
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
		onCapCheck:         cfg.OnCapCheck,
		lru:                list.New(),
		byKey:              make(map[cacheIdentity]*cacheEntry),
		byScope:            make(map[scopeKey]*scopeRecord),
		consumerCounts:     make(map[string]int),
		projectUsage:       make(map[projectKey]projectStats),
	}
}

func (c *programCache) exceedsProjectByteCapLocked(scope CacheScope, weight int) bool {
	return scope.ProjectID != "" && c.maxBytesPerProject > 0 && weight > c.maxBytesPerProject
}

func (c *programCache) retireSupersededRevisionLocked(scope CacheScope) {
	if !scope.hasID() || scope.Revision <= 0 {
		return
	}
	sk := scopeKey{consumer: scope.Consumer, id: scope.ID}
	rec, ok := c.byScope[sk]
	if !ok || rec.revision <= 0 || scope.Revision <= rec.revision {
		return
	}
	delete(c.byScope, sk)
	for oldID := range rec.entries {
		c.detachScopeFromEntryLocked(sk, oldID)
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
		if c.exceedsProjectByteCapLocked(scope, ent.weight) {
			c.retireSupersededRevisionLocked(scope)
		} else if c.bindScopeLocked(scope, ent) {
			c.enforceScopeCapsLocked(scope, ent)
		}
	}
	return ent.prog, true
}

func (c *programCache) put(id cacheIdentity, exprBytes int, prog Program, scope CacheScope) Program {
	c.mu.Lock()
	defer c.mu.Unlock()

	if ent, ok := c.byKey[id]; ok {
		c.lru.MoveToFront(ent.elem)
		if scope.hasID() {
			if c.exceedsProjectByteCapLocked(scope, ent.weight) {
				c.retireSupersededRevisionLocked(scope)
			} else if c.bindScopeLocked(scope, ent) {
				c.enforceScopeCapsLocked(scope, ent)
			}
		}
		return ent.prog
	}

	weight := max(exprBytes, 1) + prog.regexCacheWeight()

	// Never retain an entry heavier than the global or per-project byte bound
	// (O1): compile and return prog for immediate use without caching it.
	if c.maxEntries == 0 ||
		(c.maxBytes > 0 && weight > c.maxBytes) ||
		(scope.hasID() && c.exceedsProjectByteCapLocked(scope, weight)) {
		if scope.hasID() {
			c.retireSupersededRevisionLocked(scope)
		}
		return prog
	}

	ent := &cacheEntry{
		key:          id,
		prog:         prog,
		weight:       weight,
		scopes:       make(map[scopeKey]string),
		consumerRefs: make(map[string]int),
		projectRefs:  make(map[projectKey]int),
	}
	ent.elem = c.lru.PushFront(ent)
	c.byKey[id] = ent
	c.bytes += weight

	if scope.hasID() {
		if c.bindScopeLocked(scope, ent) {
			c.enforceScopeCapsLocked(scope, ent)
		}
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

func (c *programCache) attachEntryScopeLocked(ent *cacheEntry, sk scopeKey, projectID string) {
	if oldProj, already := ent.scopes[sk]; already {
		if projectID == "" || oldProj == projectID {
			return
		}
		if oldProj != "" {
			c.decProjectRefLocked(ent, projectKey{consumer: sk.consumer, projectID: oldProj})
		}
		ent.scopes[sk] = projectID
		c.incProjectRefLocked(ent, projectKey{consumer: sk.consumer, projectID: projectID})
		return
	}
	ent.scopes[sk] = projectID
	ent.everScoped = true
	if ent.consumerRefs[sk.consumer] == 0 {
		c.consumerCounts[sk.consumer]++
	}
	ent.consumerRefs[sk.consumer]++
	if projectID != "" {
		c.incProjectRefLocked(ent, projectKey{consumer: sk.consumer, projectID: projectID})
	}
}

func (c *programCache) unlinkEntryScopeLocked(ent *cacheEntry, sk scopeKey) {
	projID, exists := ent.scopes[sk]
	if !exists {
		return
	}
	delete(ent.scopes, sk)

	rc := ent.consumerRefs[sk.consumer] - 1
	if rc <= 0 {
		delete(ent.consumerRefs, sk.consumer)
		cc := c.consumerCounts[sk.consumer] - 1
		if cc <= 0 {
			delete(c.consumerCounts, sk.consumer)
		} else {
			c.consumerCounts[sk.consumer] = cc
		}
	} else {
		ent.consumerRefs[sk.consumer] = rc
	}

	if projID != "" {
		c.decProjectRefLocked(ent, projectKey{consumer: sk.consumer, projectID: projID})
	}
}

func (c *programCache) incProjectRefLocked(ent *cacheEntry, pk projectKey) {
	if ent.projectRefs[pk] == 0 {
		u := c.projectUsage[pk]
		u.entries++
		u.bytes += ent.weight
		c.projectUsage[pk] = u
	}
	ent.projectRefs[pk]++
}

func (c *programCache) decProjectRefLocked(ent *cacheEntry, pk projectKey) {
	rc := ent.projectRefs[pk] - 1
	if rc <= 0 {
		delete(ent.projectRefs, pk)
		u := c.projectUsage[pk]
		u.entries--
		u.bytes -= ent.weight
		if u.entries <= 0 {
			delete(c.projectUsage, pk)
		} else {
			if u.bytes < 0 {
				u.bytes = 0
			}
			c.projectUsage[pk] = u
		}
	} else {
		ent.projectRefs[pk] = rc
	}
}

// bindScopeLocked associates target with scope and reports whether any binding
// state changed. When (scope, target) is already bound at the current revision
// and project, it returns false in O(1) without touching cap counters.
func (c *programCache) bindScopeLocked(scope CacheScope, target *cacheEntry) bool {
	sk := scopeKey{consumer: scope.Consumer, id: scope.ID}

	rec, exists := c.byScope[sk]
	if !exists {
		c.byScope[sk] = &scopeRecord{
			projectID: scope.ProjectID,
			revision:  scope.Revision,
			entries:   map[cacheIdentity]struct{}{target.key: {}},
		}
		c.attachEntryScopeLocked(target, sk, scope.ProjectID)
		return true
	}

	// Do not allow an older revision to clobber bindings recorded for a newer revision.
	if scope.Revision > 0 && rec.revision > scope.Revision {
		return false
	}

	// Fast path (R1): hit on an already-bound (scope, entry) pair at the current
	// revision and project performs zero mutations and zero cap checks.
	if (scope.Revision <= 0 || scope.Revision == rec.revision) &&
		(scope.ProjectID == "" || scope.ProjectID == rec.projectID) {
		if _, already := rec.entries[target.key]; already {
			return false
		}
	}

	if scope.ProjectID != "" && scope.ProjectID != rec.projectID {
		rec.projectID = scope.ProjectID
		for existingID := range rec.entries {
			if existingEnt, ok := c.byKey[existingID]; ok {
				c.attachEntryScopeLocked(existingEnt, sk, rec.projectID)
			}
		}
	}

	// Attach target first so an unchanged expression across a revision bump is never
	// transiently left with zero scopes while detaching prior-revision entries.
	c.attachEntryScopeLocked(target, sk, rec.projectID)

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
		return true
	}

	rec.entries[target.key] = struct{}{}
	if scope.Revision > rec.revision {
		rec.revision = scope.Revision
	}
	return true
}

func (c *programCache) detachScopeFromEntryLocked(sk scopeKey, id cacheIdentity) {
	ent, ok := c.byKey[id]
	if !ok {
		return
	}
	c.unlinkEntryScopeLocked(ent, sk)
	if ent.everScoped && len(ent.scopes) == 0 {
		c.removeEntryLocked(ent)
	}
}

func (c *programCache) enforceScopeCapsLocked(scope CacheScope, keep *cacheEntry) {
	if scope.Consumer == "" {
		return
	}
	evictSteps := 0
	if scope.ProjectID != "" && (c.maxPerProject > 0 || c.maxBytesPerProject > 0) {
		evictSteps += c.enforceProjectCapLocked(scope.Consumer, scope.ProjectID, keep)
	}
	if c.maxPerConsumer > 0 {
		evictSteps += c.enforceConsumerCapLocked(scope.Consumer, keep)
	}
	if c.onCapCheck != nil {
		c.onCapCheck(evictSteps)
	}
}

func (c *programCache) enforceConsumerCapLocked(consumer string, keep *cacheEntry) int {
	steps := 0
	for c.consumerCounts[consumer] > c.maxPerConsumer {
		var victim *cacheEntry
		for e := c.lru.Back(); e != nil; e = e.Prev() {
			steps++
			cand := e.Value.(*cacheEntry)
			if cand == keep {
				continue
			}
			if cand.consumerRefs[consumer] > 0 {
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
			c.unlinkEntryScopeLocked(victim, sk)
			if rec, ok := c.byScope[sk]; ok {
				delete(rec.entries, victim.key)
				if len(rec.entries) == 0 {
					delete(c.byScope, sk)
				}
			}
		}
		if len(victim.scopes) == 0 {
			c.removeEntryLocked(victim)
		}
	}
	return steps
}

func (c *programCache) enforceProjectCapLocked(consumer, projectID string, keep *cacheEntry) int {
	pk := projectKey{consumer: consumer, projectID: projectID}
	steps := 0
	for {
		u := c.projectUsage[pk]
		overCount := c.maxPerProject > 0 && u.entries > c.maxPerProject
		overBytes := c.maxBytesPerProject > 0 && u.bytes > c.maxBytesPerProject
		if !overCount && !overBytes {
			break
		}
		var victim *cacheEntry
		for e := c.lru.Back(); e != nil; e = e.Prev() {
			steps++
			cand := e.Value.(*cacheEntry)
			if cand == keep {
				continue
			}
			if cand.projectRefs[pk] > 0 {
				victim = cand
				break
			}
		}
		if victim == nil {
			break
		}
		for sk, skProj := range victim.scopes {
			if sk.consumer != consumer || skProj != projectID {
				continue
			}
			c.unlinkEntryScopeLocked(victim, sk)
			if rec, ok := c.byScope[sk]; ok {
				delete(rec.entries, victim.key)
				if len(rec.entries) == 0 {
					delete(c.byScope, sk)
				}
			}
		}
		if len(victim.scopes) == 0 {
			c.removeEntryLocked(victim)
		}
	}
	return steps
}

func (c *programCache) removeEntryLocked(ent *cacheEntry) {
	for sk := range ent.scopes {
		c.unlinkEntryScopeLocked(ent, sk)
		if rec, ok := c.byScope[sk]; ok {
			delete(rec.entries, ent.key)
			if len(rec.entries) == 0 {
				delete(c.byScope, sk)
			}
		}
	}
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
