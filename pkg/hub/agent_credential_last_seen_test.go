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
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// credLastSeenWrite is one UpdateAgentCredentialLastSeen call.
type credLastSeenWrite struct {
	id string
	at time.Time
}

// credLastSeenRecorder records last_seen_at writes and answers them with err.
// With a non-nil inner store and no err, it forwards the write.
type credLastSeenRecorder struct {
	store.AgentCredentialStore
	err   error
	wrote chan struct{} // when non-nil, signalled after each write
	mu    sync.Mutex
	got   []credLastSeenWrite
}

func (r *credLastSeenRecorder) UpdateAgentCredentialLastSeen(ctx context.Context, id string, at time.Time) error {
	r.mu.Lock()
	r.got = append(r.got, credLastSeenWrite{id: id, at: at})
	r.mu.Unlock()
	if r.wrote != nil {
		defer func() { r.wrote <- struct{}{} }()
	}
	if r.err != nil {
		return r.err
	}
	if r.AgentCredentialStore != nil {
		return r.AgentCredentialStore.UpdateAgentCredentialLastSeen(ctx, id, at)
	}
	return nil
}

func (r *credLastSeenRecorder) writes() []credLastSeenWrite {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]credLastSeenWrite(nil), r.got...)
}

// credLastSeenClock is a settable clock.
type credLastSeenClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *credLastSeenClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *credLastSeenClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

// testCredLastSeenThrottle returns a throttle bounded at max on a fake clock,
// whose writes run inline (no goroutine to race the test).
func testCredLastSeenThrottle(max int) (*agentCredentialLastSeenThrottle, *credLastSeenClock) {
	clk := &credLastSeenClock{t: time.Date(2026, 10, 12, 12, 0, 0, 0, time.UTC)}
	th := newAgentCredentialLastSeenThrottle(max, agentCredentialLastSeenInterval)
	th.now = clk.now
	th.spawn = func(f func()) { f() }
	return th, clk
}

// TestAgentCredentialLastSeenThrottle_Interval: within the interval a
// second touch of the same credential writes nothing; once the interval
// has passed it writes, with the clock's time.
func TestAgentCredentialLastSeenThrottle_Interval(t *testing.T) {
	th, clk := testCredLastSeenThrottle(agentCredentialLastSeenMax)
	rec := &credLastSeenRecorder{}
	t0 := clk.now()

	th.touch(rec, "cred-1")
	require.Equal(t, []credLastSeenWrite{{"cred-1", t0}}, rec.writes())

	clk.advance(agentCredentialLastSeenInterval - time.Second)
	th.touch(rec, "cred-1")
	assert.Len(t, rec.writes(), 1, "no write within the interval")

	clk.advance(time.Second)
	th.touch(rec, "cred-1")
	assert.Equal(t, []credLastSeenWrite{{"cred-1", t0}, {"cred-1", t0.Add(agentCredentialLastSeenInterval)}}, rec.writes(),
		"a write once the interval has passed")
}

// TestAgentCredentialLastSeenThrottle_DistinctCredentials: each
// credential has its own interval.
func TestAgentCredentialLastSeenThrottle_DistinctCredentials(t *testing.T) {
	th, clk := testCredLastSeenThrottle(agentCredentialLastSeenMax)
	rec := &credLastSeenRecorder{}
	t0 := clk.now()

	th.touch(rec, "cred-1")
	clk.advance(10 * time.Second)
	th.touch(rec, "cred-2")
	th.touch(rec, "cred-1")
	th.touch(rec, "cred-2")
	assert.Equal(t, []credLastSeenWrite{{"cred-1", t0}, {"cred-2", t0.Add(10 * time.Second)}}, rec.writes())
}

// TestAgentCredentialLastSeenThrottle_WriteErrorIgnored: a failed write
// is dropped (no panic, no retry within the interval).
func TestAgentCredentialLastSeenThrottle_WriteErrorIgnored(t *testing.T) {
	th, clk := testCredLastSeenThrottle(agentCredentialLastSeenMax)
	rec := &credLastSeenRecorder{err: errors.New("last_seen write failed for testing")}

	th.touch(rec, "cred-1")
	th.touch(rec, "cred-1")
	assert.Len(t, rec.writes(), 1)
	clk.advance(agentCredentialLastSeenInterval)
	th.touch(rec, "cred-1")
	assert.Len(t, rec.writes(), 2)
}

// TestAgentCredentialLastSeenThrottle_Bounded: the set never exceeds its
// bound. When full, expired ids are dropped first; when none has
// expired, the set is cleared, which only costs extra writes.
func TestAgentCredentialLastSeenThrottle_Bounded(t *testing.T) {
	t.Run("expired dropped first", func(t *testing.T) {
		th, clk := testCredLastSeenThrottle(2)
		rec := &credLastSeenRecorder{}
		th.touch(rec, "a")
		clk.advance(agentCredentialLastSeenInterval)
		th.touch(rec, "b")
		th.touch(rec, "c") // full: "a" has expired and is dropped
		assert.LessOrEqual(t, len(th.seen.seen), 2)
		th.touch(rec, "b") // still remembered
		assert.Len(t, rec.writes(), 3)
	})
	t.Run("cleared when none expired", func(t *testing.T) {
		th, _ := testCredLastSeenThrottle(2)
		rec := &credLastSeenRecorder{}
		th.touch(rec, "a")
		th.touch(rec, "b")
		th.touch(rec, "c") // full, none expired: cleared, then "c" kept
		assert.Len(t, th.seen.seen, 1)
		th.touch(rec, "a") // forgotten by the clear: written again
		assert.Len(t, rec.writes(), 4)
		assert.LessOrEqual(t, len(th.seen.seen), 2)
	})
}

// TestAgentCredentialLastSeenThrottle_NilWritesEveryTime: a nil throttle
// keeps the unthrottled behaviour, writing on a goroutine every time.
func TestAgentCredentialLastSeenThrottle_NilWritesEveryTime(t *testing.T) {
	var th *agentCredentialLastSeenThrottle
	rec := &credLastSeenRecorder{wrote: make(chan struct{}, 2)}
	th.touch(rec, "cred-1")
	th.touch(rec, "cred-1")
	for range 2 {
		select {
		case <-rec.wrote:
		case <-time.After(10 * time.Second):
			t.Fatal("last_seen_at write not made")
		}
	}
	assert.Len(t, rec.writes(), 2)
}
