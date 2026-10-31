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
	"math/rand/v2"
	"sync"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/conduit"
	"github.com/GoogleCloudPlatform/scion/pkg/conduit/registry"
)

// conduitIngressPendingTTL bounds how long a placement counts as pending
// when the hub never sees its outcome: the relay admits or refuses a dial
// within its handshake timeout, so past this the session is either in the
// relay's session count or gone. The margin covers the proxy round trip.
const conduitIngressPendingTTL = conduit.DefaultHandshakeTimeout + 5*time.Second

// conduitIngressPendingKey names one relay generation.
type conduitIngressPendingKey struct {
	instanceID string
	generation int64
}

// conduitIngressPending holds this hub's placements that a relay's session
// count may not include yet (ptone/scion#6641). A relay inserts the session
// row only when it admits the dial, up to its handshake timeout after the
// pick, so dials picked in between all saw the same counts and went to the
// same relay. Each placement is added when its relay is picked and released
// when the hub sees the outcome (the relay's first byte after the upgrade:
// the Welcome, once the row exists, or the refusal), when the dial ends
// without a session, or after conduitIngressPendingTTL. The map is bounded
// by this hub's in-flight dials.
type conduitIngressPending struct {
	mu      sync.Mutex
	next    uint64
	entries map[conduitIngressPendingKey]map[uint64]time.Time
}

// countLocked returns the pending placements on key. The caller holds p.mu.
func (p *conduitIngressPending) countLocked(key conduitIngressPendingKey) int {
	return len(p.entries[key])
}

// evictLocked drops the placements older than the TTL at now, and empty
// keys (including those of stale generations). The caller holds p.mu.
func (p *conduitIngressPending) evictLocked(now time.Time) {
	for key, m := range p.entries {
		for id, at := range m {
			if now.Sub(at) >= conduitIngressPendingTTL {
				delete(m, id)
			}
		}
		if len(m) == 0 {
			delete(p.entries, key)
		}
	}
}

// choose picks between a and b, whose relay session counts are na and nb,
// by the lower count plus this hub's pending placements (a tie is random),
// and adds a pending placement on the chosen relay. Comparing and adding
// happen under one lock, so concurrent picks on this hub see each other.
// It returns the chosen relay and the release of its placement, which is
// idempotent.
func (p *conduitIngressPending) choose(now time.Time, a, b registry.RelayInstance, na, nb int) (registry.RelayInstance, func()) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.evictLocked(now)
	ka := conduitIngressPendingKey{a.InstanceID, a.Generation}
	kb := conduitIngressPendingKey{b.InstanceID, b.Generation}
	la, lb := na+p.countLocked(ka), nb+p.countLocked(kb)
	pick := a
	switch {
	case lb < la:
		pick = b
	case la == lb && rand.IntN(2) == 1:
		pick = b
	}
	return pick, p.addLocked(now, pick)
}

// add adds a pending placement on ri without a comparison (a single
// candidate, or a pick made without counts) and returns its release.
func (p *conduitIngressPending) add(now time.Time, ri registry.RelayInstance) func() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.evictLocked(now)
	return p.addLocked(now, ri)
}

func (p *conduitIngressPending) addLocked(now time.Time, ri registry.RelayInstance) func() {
	if p.entries == nil {
		p.entries = make(map[conduitIngressPendingKey]map[uint64]time.Time)
	}
	key := conduitIngressPendingKey{ri.InstanceID, ri.Generation}
	m := p.entries[key]
	if m == nil {
		m = make(map[uint64]time.Time)
		p.entries[key] = m
	}
	p.next++
	id := p.next
	m[id] = now
	var once sync.Once
	return func() {
		once.Do(func() {
			p.mu.Lock()
			defer p.mu.Unlock()
			if m := p.entries[key]; m != nil {
				delete(m, id)
				if len(m) == 0 {
					delete(p.entries, key)
				}
			}
		})
	}
}

// pendingFor returns the pending placements on ri (tests).
func (p *conduitIngressPending) pendingFor(ri registry.RelayInstance) int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.countLocked(conduitIngressPendingKey{ri.InstanceID, ri.Generation})
}
