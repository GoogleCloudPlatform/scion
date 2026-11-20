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
	"fmt"
	"math/rand"
	"testing"
)

func checkCacheInvariants(t *testing.T, c *programCache, step int, op string) {
	t.Helper()
	bytes := 0
	n := 0
	for e := c.lru.Front(); e != nil; e = e.Next() {
		ent := e.Value.(*cacheEntry)
		n++
		bytes += ent.weight
		if c.byKey[ent.key] != ent {
			t.Fatalf("step %d %s: lru entry not in byKey", step, op)
		}
	}
	if n != len(c.byKey) {
		t.Fatalf("step %d %s: lru len %d != byKey %d", step, op, n, len(c.byKey))
	}
	if bytes != c.bytes {
		t.Fatalf("step %d %s: bytes %d != c.bytes %d", step, op, bytes, c.bytes)
	}
	if c.lru.Len() > c.maxEntries || c.bytes > c.maxBytes {
		t.Fatalf("step %d %s: global bound violated %d/%d", step, op, c.lru.Len(), c.bytes)
	}
	wantCons := map[string]int{}
	wantProj := map[projectKey]projectStats{}
	for _, ent := range c.byKey {
		cons := map[string]int{}
		projs := map[projectKey]int{}
		for sk, pid := range ent.scopes {
			cons[sk.consumer]++
			if pid != "" {
				projs[projectKey{sk.consumer, pid}]++
			}
			rec, ok := c.byScope[sk]
			if !ok {
				t.Fatalf("step %d %s: entry linked to scope %v with no byScope record", step, op, sk)
			}
			if _, ok := rec.entries[ent.key]; !ok {
				t.Fatalf("step %d %s: entry linked to scope %v not in rec.entries", step, op, sk)
			}
			if rec.projectID != pid {
				t.Fatalf("step %d %s: link project %q != rec project %q", step, op, pid, rec.projectID)
			}
		}
		if fmt.Sprint(cons) != fmt.Sprint(ent.consumerRefs) {
			t.Fatalf("step %d %s: consumerRefs %v want %v", step, op, ent.consumerRefs, cons)
		}
		if fmt.Sprint(projs) != fmt.Sprint(ent.projectRefs) {
			t.Fatalf("step %d %s: projectRefs %v want %v", step, op, ent.projectRefs, projs)
		}
		for k := range cons {
			wantCons[k]++
		}
		for pk := range projs {
			u := wantProj[pk]
			u.entries++
			u.bytes += ent.weight
			wantProj[pk] = u
		}
		if ent.everScoped && len(ent.scopes) == 0 {
			t.Fatalf("step %d %s: orphan everScoped entry retained", step, op)
		}
	}
	for sk, rec := range c.byScope {
		if len(rec.entries) == 0 {
			t.Fatalf("step %d %s: empty scope record %v", step, op, sk)
		}
		for id := range rec.entries {
			ent, ok := c.byKey[id]
			if !ok {
				t.Fatalf("step %d %s: scope %v references evicted entry", step, op, sk)
			}
			if _, ok := ent.scopes[sk]; !ok {
				t.Fatalf("step %d %s: scope %v references entry not linked back", step, op, sk)
			}
		}
	}
	if fmt.Sprint(wantCons) != fmt.Sprint(c.consumerCounts) {
		t.Fatalf("step %d %s: consumerCounts %v want %v", step, op, c.consumerCounts, wantCons)
	}
	if fmt.Sprint(wantProj) != fmt.Sprint(c.projectUsage) {
		t.Fatalf("step %d %s: projectUsage %v want %v", step, op, c.projectUsage, wantProj)
	}
	for pk, u := range c.projectUsage {
		if pk.consumer == "trigger" && (u.entries > c.maxPerProject || u.bytes > c.maxBytesPerProject) {
			t.Fatalf("step %d %s: per-project bound violated %v %+v", step, op, pk, u)
		}
	}
}

func TestReviewCacheInvariantsRandomized(t *testing.T) {
	// 20 seeds x 600 ops (12,000 invariant-checked transitions) stays at or
	// under 2 s even with -race enabled.
	for seed := int64(1); seed <= 20; seed++ {
		r := rand.New(rand.NewSource(seed))
		cfg, _ := CacheConfig{MaxEntries: 12, MaxBytes: 4000, MaxPerProject: 5, MaxBytesPerProject: 1500, MaxPerConsumer: 9}.normalize()
		c := newProgramCache(cfg)
		consumers := []string{"trigger", "trigger", "message_policy"}
		projects := []string{"p1", "p2", "p3", ""}
		for step := 0; step < 600; step++ {
			cons := consumers[r.Intn(len(consumers))]
			scope := CacheScope{Consumer: cons, ID: fmt.Sprintf("id%d", r.Intn(8)), Revision: int64(r.Intn(5))}
			if cons == "trigger" {
				// a trigger id maps to a fixed project
				scope.ProjectID = projects[int(scope.ID[2]-'0')%3]
			}
			if r.Intn(10) == 0 {
				scope = CacheScope{}
			}
			var id cacheIdentity
			id[0] = byte(r.Intn(30))
			w := 1 + r.Intn(900)
			if id[0] == 0 {
				w = 1600 // heavier than per-project bound
			}
			op := ""
			switch k := r.Intn(10); {
			case k < 6:
				op = "put"
				if _, ok := c.get(id, scope); !ok {
					c.put(id, w, Program{}, scope)
				}
			case k < 8:
				op = "inv-id"
				c.invalidate(CacheScope{Consumer: cons, ProjectID: scope.ProjectID, ID: scope.ID, Revision: int64(r.Intn(5))})
			case k < 9:
				op = "inv-proj"
				c.invalidate(CacheScope{Consumer: "trigger", ProjectID: projects[r.Intn(3)]})
			default:
				op = "inv-consumer"
				c.invalidate(CacheScope{Consumer: cons})
			}
			checkCacheInvariants(t, c, step, fmt.Sprintf("seed%d/%s", seed, op))
		}
	}
}

func TestEvictionLoopsFailSafeOnStuckCounters(t *testing.T) {
	t.Run("project_cap_no_progress_breaks_and_drops_keep", func(t *testing.T) {
		cfg, _ := CacheConfig{MaxEntries: 16, MaxBytes: 4000, MaxPerProject: 2, MaxBytesPerProject: 1000}.normalize()
		c := newProgramCache(cfg)
		var id1, id2 cacheIdentity
		id1[0], id2[0] = 1, 2
		// id1 stays alive via p2, but we inject a phantom projectRefs/projectUsage leak on p1
		// (matching the decProjectRefLocked-removed mutant that previously spun forever).
		c.put(id1, 100, Program{}, CacheScope{Consumer: "trigger", ProjectID: "p2", ID: "t-p2", Revision: 1})
		pk := projectKey{consumer: "trigger", projectID: "p1"}
		c.byKey[id1].projectRefs[pk] = 1
		c.projectUsage[pk] = projectStats{entries: 2, bytes: 200}

		c.put(id2, 100, Program{}, CacheScope{Consumer: "trigger", ProjectID: "p1", ID: "t-p1", Revision: 1})
		if _, retained := c.byKey[id2]; retained {
			t.Fatalf("expected fail-safe eviction of id2 when enforceProjectCapLocked makes no progress")
		}
	})

	t.Run("consumer_cap_no_progress_breaks_and_drops_keep", func(t *testing.T) {
		cfg, _ := CacheConfig{MaxEntries: 16, MaxBytes: 4000, MaxPerConsumer: 1}.normalize()
		c := newProgramCache(cfg)
		var id1, id2 cacheIdentity
		id1[0], id2[0] = 1, 2
		c.put(id1, 100, Program{}, CacheScope{Consumer: "other", ID: "o1", Revision: 1})
		c.byKey[id1].consumerRefs["trigger"] = 1
		c.consumerCounts["trigger"] = 1

		c.put(id2, 100, Program{}, CacheScope{Consumer: "trigger", ID: "t1", Revision: 1})
		if _, retained := c.byKey[id2]; retained {
			t.Fatalf("expected fail-safe eviction of id2 when enforceConsumerCapLocked makes no progress")
		}
	})

	t.Run("global_cap_phantom_bytes_breaks_and_drops_entry", func(t *testing.T) {
		cfg, _ := CacheConfig{MaxEntries: 16, MaxBytes: 500}.normalize()
		c := newProgramCache(cfg)
		c.bytes = 1000 // phantom byte leak with empty LRU
		var id1 cacheIdentity
		id1[0] = 1
		c.put(id1, 100, Program{}, CacheScope{})
		if _, retained := c.byKey[id1]; retained {
			t.Fatalf("expected fail-safe eviction of id1 when global byte cap cannot be satisfied")
		}
	})
}
