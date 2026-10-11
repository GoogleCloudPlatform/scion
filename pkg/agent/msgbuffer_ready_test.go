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

package agent

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type recordedDelivery struct {
	agent, message string
}

type deliveryRecorder struct {
	mu   sync.Mutex
	list []recordedDelivery
}

func (r *deliveryRecorder) deliver(agentID, projectID, message string, interrupt bool) error {
	r.mu.Lock()
	r.list = append(r.list, recordedDelivery{agentID, message})
	r.mu.Unlock()
	return nil
}

func (r *deliveryRecorder) forAgent(agent string) []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []string
	for _, d := range r.list {
		if d.agent == agent {
			out = append(out, d.message)
		}
	}
	return out
}

// TestMessageBuffer_ReadinessWaitIsolatedPerAgent checks that a long
// readiness wait for one agent neither delays another agent's flush nor
// blocks new enqueues, and that messages arriving during the wait keep
// their order: no later flush for the same agent pastes first.
//
// Only the first wait blocks; later waits return at once. So without the
// per-agent flushGate, the flush started by a2's timer would wait (not
// blocked), take the pending messages and paste ahead of the first flush.
// With the gate it queues behind the first flush and never even starts its
// wait until the first flush is done.
func TestMessageBuffer_ReadinessWaitIsolatedPerAgent(t *testing.T) {
	rec := &deliveryRecorder{}
	buf := NewMessageBuffer(20*time.Millisecond, rec.deliver)
	defer buf.Close()

	release := make(chan struct{})
	var waitsA atomic.Int32
	buf.waitReady = func(ctx context.Context, agentID, projectID string) {
		if agentID != "a" {
			return
		}
		if waitsA.Add(1) != 1 {
			return // only the first wait blocks
		}
		select {
		case <-release:
		case <-ctx.Done():
		}
	}

	buf.Send("a", "p", "a1")
	waitFor(t, time.Second, func() bool { return waitsA.Load() == 1 })

	// Agent a is waiting. Enqueues must not block, for a or for b.
	start := time.Now()
	buf.Send("a", "p", "a2")
	buf.Send("b", "p", "b1")
	buf.Send("a", "p", "a3")
	if el := time.Since(start); el > 100*time.Millisecond {
		t.Fatalf("Send blocked for %v during another flush's readiness wait", el)
	}

	// Agent b's flush is not held up by a's wait.
	waitFor(t, time.Second, func() bool { return len(rec.forAgent("b")) == 1 })
	// a's later timer fires a second flush, which must queue behind the
	// first one on the flush gate rather than paste ahead of it.
	gateRefs := func() int {
		buf.mu.Lock()
		defer buf.mu.Unlock()
		if g := buf.gates[bufferKey("a", "p")]; g != nil {
			return g.refs
		}
		return 0
	}
	waitFor(t, time.Second, func() bool { return gateRefs() >= 2 })
	if got := rec.forAgent("a"); len(got) != 0 {
		t.Fatalf("agent a delivered %q while its first flush was still waiting", got)
	}
	if w := waitsA.Load(); w != 1 {
		t.Fatalf("agent a waits = %d while the first flush holds; a later flush must queue behind it, not start its own wait", w)
	}

	close(release)
	waitFor(t, time.Second, func() bool { return len(rec.forAgent("a")) >= 1 })
	time.Sleep(100 * time.Millisecond)
	got := rec.forAgent("a")
	if len(got) != 1 || got[0] != "a1\n\na2\n\na3" {
		t.Fatalf("agent a deliveries = %q, want one in-order delivery", got)
	}
}

// TestMessageBuffer_ReadinessWaitOncePerFlush checks retries do not wait
// again.
func TestMessageBuffer_ReadinessWaitOncePerFlush(t *testing.T) {
	var attempts atomic.Int32
	done := make(chan struct{})
	buf := NewMessageBuffer(10*time.Millisecond, func(agentID, projectID, message string, interrupt bool) error {
		if attempts.Add(1) < 3 {
			return errors.New("transient")
		}
		close(done)
		return nil
	})
	buf.flushRetryBackoff = 5 * time.Millisecond
	defer buf.Close()
	var waits atomic.Int32
	buf.waitReady = func(ctx context.Context, agentID, projectID string) { waits.Add(1) }

	buf.Send("a", "p", "m")
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("delivery did not succeed")
	}
	if w := waits.Load(); w != 1 {
		t.Fatalf("waitReady called %d times, want 1", w)
	}
}

// TestMessageBuffer_CloseSkipsAndCutsShortReadinessWait checks Close never
// waits for readiness: pending messages are pasted without a wait, and a
// wait already in progress is cancelled.
func TestMessageBuffer_CloseSkipsAndCutsShortReadinessWait(t *testing.T) {
	rec := &deliveryRecorder{}
	buf := NewMessageBuffer(10*time.Millisecond, rec.deliver)
	var waiting atomic.Int32
	buf.waitReady = func(ctx context.Context, agentID, projectID string) {
		waiting.Add(1)
		<-ctx.Done() // never ready until shutdown
	}

	buf.Send("a", "p", "in-progress")
	waitFor(t, time.Second, func() bool { return waiting.Load() == 1 })

	// A second agent whose timer has not fired yet.
	buf.bufferDelay = time.Hour
	buf.Send("b", "p", "pending")

	start := time.Now()
	buf.Close()
	if el := time.Since(start); el > time.Second {
		t.Fatalf("Close took %v", el)
	}
	if got := rec.forAgent("a"); len(got) != 1 || got[0] != "in-progress" {
		t.Fatalf("agent a = %q", got)
	}
	if got := rec.forAgent("b"); len(got) != 1 || got[0] != "pending" {
		t.Fatalf("agent b = %q", got)
	}
	if w := waiting.Load(); w != 1 {
		t.Fatalf("waitReady called %d times, want 1 (Close itself must not wait)", w)
	}
}
