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
	"bytes"
	"io"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/conduit/registry"
)

var (
	pendingRelayA = registry.RelayInstance{InstanceID: "relay-a", Generation: 1}
	pendingRelayB = registry.RelayInstance{InstanceID: "relay-b", Generation: 1}
)

// burstChoose runs n concurrent picks between the two relays on p, with
// session counts that never move (no dial is admitted yet), and returns how
// many went to relay-a and relay-b. Placements are not released.
func burstChoose(p *conduitIngressPending, now time.Time, n int) (onA, onB int) {
	var mu sync.Mutex
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			pick, _ := p.choose(now, pendingRelayA, pendingRelayB, 0, 0)
			mu.Lock()
			defer mu.Unlock()
			if pick.InstanceID == pendingRelayA.InstanceID {
				onA++
			} else {
				onB++
			}
		}()
	}
	wg.Wait()
	return onA, onB
}

// TestConduitIngressPending_BurstSpreads: a burst of dials picked before any
// is admitted (the session counts stay equal) splits across the two relays
// instead of herding onto one (ptone/scion#6641).
func TestConduitIngressPending_BurstSpreads(t *testing.T) {
	var p conduitIngressPending
	onA, onB := burstChoose(&p, time.Now(), 50)
	assert.Equal(t, 50, onA+onB)
	assert.GreaterOrEqual(t, onA, 15, "relay-a holds at least 30%%")
	assert.GreaterOrEqual(t, onB, 15, "relay-b holds at least 30%%")
	assert.LessOrEqual(t, abs(onA-onB), 1, "one hub alternates its own burst")
	assert.Equal(t, onA, p.pendingFor(pendingRelayA))
	assert.Equal(t, onB, p.pendingFor(pendingRelayB))
}

// TestConduitIngressPending_TwoHubsSpread: two hubs placing independently
// over the same stale counts each spread their own burst, so the total
// splits too.
func TestConduitIngressPending_TwoHubsSpread(t *testing.T) {
	var hub1, hub2 conduitIngressPending
	now := time.Now()
	a1, b1 := burstChoose(&hub1, now, 25)
	a2, b2 := burstChoose(&hub2, now, 25)
	onA, onB := a1+a2, b1+b2
	assert.GreaterOrEqual(t, onA, 15, "relay-a holds at least 30%% of 50")
	assert.GreaterOrEqual(t, onB, 15, "relay-b holds at least 30%% of 50")
}

// TestConduitIngressPending_AdmittedCountedOnce: once a dial's outcome is
// seen its placement is released, so an admitted, long-lived session is
// counted once (by the relay's session count), not twice.
func TestConduitIngressPending_AdmittedCountedOnce(t *testing.T) {
	var p conduitIngressPending
	now := time.Now()
	pick, release := p.choose(now, pendingRelayA, pendingRelayB, 0, 1)
	require.Equal(t, pendingRelayA, pick, "fewer sessions wins")
	require.Equal(t, 1, p.pendingFor(pendingRelayA))

	// The relay admits the dial (its count becomes 1) and the hub reads its
	// Welcome: the placement is released well before the TTL.
	body := &conduitIngressOutcomeBody{ReadWriteCloser: nopRWC{bytes.NewBufferString("welcome")}, release: release}
	buf := make([]byte, 3)
	_, err := body.Read(buf)
	require.NoError(t, err)
	assert.Zero(t, p.pendingFor(pendingRelayA), "released on the relay's first byte")

	release() // idempotent
	assert.Zero(t, p.pendingFor(pendingRelayA))
}

// TestConduitIngressPending_ReleaseAndTTL: an explicit release drops a
// placement; one never released stops counting after the TTL; a placement
// on a stale generation does not count for the current one.
func TestConduitIngressPending_ReleaseAndTTL(t *testing.T) {
	now := time.Now()
	t.Run("release", func(t *testing.T) {
		var p conduitIngressPending
		release := p.add(now, pendingRelayA)
		require.Equal(t, 1, p.pendingFor(pendingRelayA))
		release()
		release()
		assert.Zero(t, p.pendingFor(pendingRelayA))
	})
	t.Run("ttl backstop", func(t *testing.T) {
		var p conduitIngressPending
		p.add(now, pendingRelayA)
		pick, _ := p.choose(now.Add(conduitIngressPendingTTL-time.Second), pendingRelayA, pendingRelayB, 0, 0)
		assert.Equal(t, pendingRelayB, pick, "still pending before the TTL")
		var q conduitIngressPending
		q.add(now, pendingRelayA)
		_, _ = q.choose(now.Add(conduitIngressPendingTTL), pendingRelayB, pendingRelayB, 0, 0)
		assert.Zero(t, q.pendingFor(pendingRelayA), "evicted at the TTL")
	})
	t.Run("stale generation", func(t *testing.T) {
		var p conduitIngressPending
		p.add(now, registry.RelayInstance{InstanceID: "relay-a", Generation: 0})
		assert.Zero(t, p.pendingFor(pendingRelayA), "generation 0 placements do not count for generation 1")
	})
	t.Run("non-upgrade answer releases", func(t *testing.T) {
		var p conduitIngressPending
		release := p.add(now, pendingRelayA)
		body := &conduitIngressOutcomeBody{ReadWriteCloser: nopRWC{bytes.NewBuffer(nil)}, release: release}
		_, err := body.Read(make([]byte, 1))
		assert.ErrorIs(t, err, io.EOF)
		assert.Zero(t, p.pendingFor(pendingRelayA), "a closed leg releases too")
	})
}

// nopRWC adapts a buffer to io.ReadWriteCloser.
type nopRWC struct{ *bytes.Buffer }

func (nopRWC) Close() error { return nil }

func abs(n int) int {
	if n < 0 {
		return -n
	}
	return n
}
