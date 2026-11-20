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
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

func refusalLogSize(l *githubAssociationRefusalLog) int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.entries)
}

func TestGitHubAssociationRefusalLog_BoundedAndPruned(t *testing.T) {
	now := time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC)
	prevNow := timeNow
	timeNow = func() time.Time { return now }
	t.Cleanup(func() { timeNow = prevNow })

	l := &githubAssociationRefusalLog{
		maxEntries: 3,
		ttl:        time.Hour,
	}

	// First refusal logs; repeated identical refusal suppresses and refreshes lastSeen.
	assert.True(t, l.shouldLog("proj-a", 10, "reason-1"))
	assert.False(t, l.shouldLog("proj-a", 10, "reason-1"))
	assert.Equal(t, 1, refusalLogSize(l))

	// Changed reason or installation logs again without adding a second entry for the same project.
	assert.True(t, l.shouldLog("proj-a", 10, "reason-2"))
	assert.False(t, l.shouldLog("proj-a", 10, "reason-2"))
	assert.True(t, l.shouldLog("proj-a", 11, "reason-2"))
	assert.Equal(t, 1, refusalLogSize(l))

	// Fill up to maxEntries (3) with advancing timestamps; refresh proj-a so proj-b is oldest.
	now = now.Add(time.Minute)
	assert.True(t, l.shouldLog("proj-b", 20, "reason-1"))
	now = now.Add(time.Minute)
	assert.True(t, l.shouldLog("proj-c", 30, "reason-1"))
	now = now.Add(time.Minute)
	assert.False(t, l.shouldLog("proj-a", 11, "reason-2")) // refreshes proj-a.lastSeen
	assert.Equal(t, 3, refusalLogSize(l))

	// Adding proj-d must evict the least-recently-seen entry (proj-b) and stay <= maxEntries.
	now = now.Add(time.Minute)
	assert.True(t, l.shouldLog("proj-d", 40, "reason-1"))
	assert.Equal(t, 3, refusalLogSize(l))
	assert.False(t, l.shouldLog("proj-a", 11, "reason-2"), "recently seen proj-a should remain suppressed")
	assert.True(t, l.shouldLog("proj-b", 20, "reason-1"), "evicted proj-b should log again")
	assert.Equal(t, 3, refusalLogSize(l))

	// Advance past TTL: stale entries are pruned on the next write so old projects do not leak.
	now = now.Add(2 * time.Hour)
	assert.True(t, l.shouldLog("proj-fresh", 50, "reason-1"))
	assert.Equal(t, 1, refusalLogSize(l), "expired projects should be pruned")

	// Clear removes state so the next identical refusal logs again.
	l.clear("proj-fresh")
	assert.Equal(t, 0, refusalLogSize(l))
	assert.True(t, l.shouldLog("proj-fresh", 50, "reason-1"))
}

func TestGitHubAssociationRefusalLog_ConcurrentAccess(t *testing.T) {
	l := &githubAssociationRefusalLog{maxEntries: 16}
	var wg sync.WaitGroup
	for g := 0; g < 8; g++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			proj := fmt.Sprintf("proj-%d", id)
			for i := 0; i < 50; i++ {
				l.shouldLog(proj, int64(id+1), "unauthorized")
				if i%10 == 0 {
					l.clear(proj)
				}
			}
		}(g)
	}
	wg.Wait()
	assert.LessOrEqual(t, refusalLogSize(l), 16)
}
