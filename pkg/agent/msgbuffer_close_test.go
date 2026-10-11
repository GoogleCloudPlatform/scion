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
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// TestMessageBuffer_CloseWaitsForInFlightFlush covers ptone/scion#4589: a
// timer-driven flush that has already taken its messages (and so is no
// longer visible in mb.buffers) must not retry delivery or run failure
// handlers after Close returns.
//
// The first delivery attempt blocks until the test releases it, so the
// flush is pinned inside deliverFunc when Close is called. No sleeps are
// used to order events; the time.After values below are only failure
// guards.
func TestMessageBuffer_CloseWaitsForInFlightFlush(t *testing.T) {
	var (
		closed      atomic.Bool
		calls       atomic.Int32
		lateCalls   atomic.Int32
		lateHandler atomic.Int32
		entered     = make(chan struct{})
		release     = make(chan struct{})
		handlerDone = make(chan struct{})
		releaseOnce sync.Once
	)
	doRelease := func() { releaseOnce.Do(func() { close(release) }) }
	t.Cleanup(doRelease) // never leave the flush goroutine blocked

	errTransient := errors.New("transient delivery failure")
	buf := NewMessageBuffer(time.Millisecond, func(agentID, projectID, message string, interrupt bool) error {
		if closed.Load() {
			lateCalls.Add(1)
		}
		if calls.Add(1) == 1 {
			close(entered)
			<-release
		}
		return errTransient // retryable: flush retries, then reports failure
	})
	buf.maxFlushAttempts = 2
	buf.flushRetryBackoff = 0

	buf.SendWithFailureHandler("agent-1", "project-a", "hello", func(error) {
		if closed.Load() {
			lateHandler.Add(1)
		}
		close(handlerDone)
	})

	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("timer flush never reached deliverFunc")
	}

	closeDone := make(chan struct{})
	go func() {
		buf.Close()
		closed.Store(true)
		close(closeDone)
	}()

	// If Close returns while the first attempt is still blocked, the
	// remaining work of that flush (retry, Warn, failure handler) is bound
	// to run after Close returned. If Close is waiting for the flush, this
	// guard expires and the attempt is released below. The guard length
	// cannot make the fixed code fail; it only has to be long enough for
	// a Close that does not wait to return, which takes microseconds.
	select {
	case <-closeDone:
	case <-time.After(250 * time.Millisecond):
	}
	doRelease()

	select {
	case <-closeDone:
	case <-time.After(5 * time.Second):
		t.Fatal("Close did not return after the in-flight delivery finished")
	}
	select {
	case <-handlerDone:
	case <-time.After(5 * time.Second):
		t.Fatal("failure handler never ran")
	}

	if n := lateCalls.Load(); n != 0 {
		t.Errorf("deliverFunc ran %d time(s) after Close returned; want 0", n)
	}
	if n := lateHandler.Load(); n != 0 {
		t.Errorf("failure handler ran after Close returned; want it to run before Close returns")
	}
	if n := calls.Load(); n != 2 {
		t.Errorf("deliverFunc calls = %d, want 2 (one attempt plus one retry)", n)
	}
}

// TestMessageBuffer_CloseWaitIsBounded checks that a delivery that never
// returns cannot hold Close past closeWaitTimeout (ptone/scion#4589).
func TestMessageBuffer_CloseWaitIsBounded(t *testing.T) {
	entered := make(chan struct{})
	release := make(chan struct{})
	t.Cleanup(func() { close(release) }) // free the stuck flush goroutine

	buf := NewMessageBuffer(time.Millisecond, func(agentID, projectID, message string, interrupt bool) error {
		close(entered)
		<-release // stuck until the test ends
		return nil
	})
	const bound = 300 * time.Millisecond
	buf.closeWaitTimeout = bound

	buf.Send("agent-1", "project-a", "hello")
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("timer flush never reached deliverFunc")
	}

	start := time.Now()
	closeDone := make(chan struct{})
	go func() {
		buf.Close()
		close(closeDone)
	}()
	select {
	case <-closeDone:
	case <-time.After(10 * time.Second):
		t.Fatal("Close did not return while a delivery was stuck")
	}
	el := time.Since(start)
	if el < bound {
		t.Errorf("Close returned after %v, want it to wait about %v for the stuck delivery", el, bound)
	}
	if el > bound+2*time.Second {
		t.Errorf("Close returned after %v, want close to the %v bound", el, bound)
	}
}

// TestMessageBuffer_CloseEndsRetryBackoffEarly checks that a flush waiting
// out its retry backoff resumes as soon as Close starts, so Close does not
// sit through the full backoff.
func TestMessageBuffer_CloseEndsRetryBackoffEarly(t *testing.T) {
	var calls atomic.Int32
	firstFailed := make(chan struct{})
	buf := NewMessageBuffer(time.Millisecond, func(agentID, projectID, message string, interrupt bool) error {
		if calls.Add(1) == 1 {
			defer close(firstFailed)
			return errors.New("transient delivery failure")
		}
		return nil
	})
	buf.maxFlushAttempts = 2
	buf.flushRetryBackoff = time.Hour
	buf.closeWaitTimeout = time.Minute

	buf.Send("agent-1", "project-a", "hello")
	select {
	case <-firstFailed:
	case <-time.After(5 * time.Second):
		t.Fatal("first delivery attempt never ran")
	}

	closeDone := make(chan struct{})
	go func() {
		buf.Close()
		close(closeDone)
	}()
	select {
	case <-closeDone:
	case <-time.After(10 * time.Second):
		t.Fatal("Close sat through the retry backoff")
	}
	if n := calls.Load(); n != 2 {
		t.Errorf("deliverFunc calls = %d, want 2 (the retry runs before Close returns)", n)
	}
}

// TestMessageBuffer_SendAfterCloseFails checks that a message sent after
// Close is never delivered and its failure handler reports
// ErrMessageBufferClosed before Send returns.
func TestMessageBuffer_SendAfterCloseFails(t *testing.T) {
	var calls atomic.Int32
	buf := NewMessageBuffer(time.Millisecond, func(agentID, projectID, message string, interrupt bool) error {
		calls.Add(1)
		return nil
	})
	buf.Close()

	var got error
	handled := 0
	buf.SendWithFailureHandler("agent-1", "project-a", "late", func(err error) {
		handled++
		got = err
	})
	if handled != 1 {
		t.Fatalf("failure handler ran %d time(s) before Send returned, want 1", handled)
	}
	if !errors.Is(got, ErrMessageBufferClosed) {
		t.Errorf("failure handler error = %v, want ErrMessageBufferClosed", got)
	}

	// A plain Send (no handler) after Close must not panic or deliver.
	buf.Send("agent-1", "project-a", "late too")
	buf.Close() // repeated Close is safe and returns at once

	if n := calls.Load(); n != 0 {
		t.Errorf("deliverFunc ran %d time(s) for messages sent after Close, want 0", n)
	}
	buf.mu.Lock()
	pending := len(buf.buffers)
	buf.mu.Unlock()
	if pending != 0 {
		t.Errorf("%d buffer(s) pending after Close, want 0", pending)
	}
}

// TestMessageBuffer_CloseFlushesOtherKeysPastStuckKey checks that a key
// whose gate is held by a stuck timer flush does not hold up Close's flush
// of other keys: every other pending key is delivered before Close returns.
// Before Close flushed each key in its own goroutine, keys that came after
// the stuck one (in random map order) were left undelivered.
func TestMessageBuffer_CloseFlushesOtherKeysPastStuckKey(t *testing.T) {
	const others = 16
	var (
		mu        sync.Mutex
		delivered = map[string]int{}
		stuckOnce sync.Once
		entered   = make(chan struct{})
		release   = make(chan struct{})
	)
	t.Cleanup(func() { close(release) }) // free the stuck flush goroutine

	buf := NewMessageBuffer(time.Millisecond, func(agentID, projectID, message string, interrupt bool) error {
		if agentID == "stuck" && message == "first" {
			stuckOnce.Do(func() { close(entered) })
			<-release
			return nil
		}
		mu.Lock()
		delivered[agentID]++
		mu.Unlock()
		return nil
	})
	buf.closeWaitTimeout = 500 * time.Millisecond

	buf.Send("stuck", "p", "first")
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("timer flush never reached deliverFunc")
	}

	// Pending messages whose timers will not fire before Close: one more
	// for the stuck key (its Close flush queues behind the stuck gate) and
	// one for each other key.
	buf.mu.Lock()
	buf.bufferDelay = time.Hour
	buf.mu.Unlock()
	buf.Send("stuck", "p", "second")
	for i := 0; i < others; i++ {
		buf.Send(fmt.Sprintf("other-%02d", i), "p", "hello")
	}

	buf.Close()

	mu.Lock()
	defer mu.Unlock()
	for i := 0; i < others; i++ {
		id := fmt.Sprintf("other-%02d", i)
		if delivered[id] != 1 {
			t.Errorf("%s delivered %d time(s) before Close returned, want 1", id, delivered[id])
		}
	}
	if delivered["stuck"] != 0 {
		t.Errorf("stuck key's second message delivered %d time(s) while its gate was held, want 0", delivered["stuck"])
	}
}

// TestMessageBuffer_CloseFlushesKeysConcurrently checks that Close flushes
// pending keys concurrently rather than one after another. Each delivery
// waits, up to a 5s guard, for the other key's delivery to start too. When
// Close flushes the keys at the same time, both start and meet at once.
// If Close flushed them one by one, the first delivery could never see the
// second start, whatever the order, so this fails on every run.
func TestMessageBuffer_CloseFlushesKeysConcurrently(t *testing.T) {
	// Both deliveries call started.Done in either order, so the waiter
	// below always finishes and nothing leaks.
	var (
		started  sync.WaitGroup
		allIn    = make(chan struct{})
		metCount atomic.Int32
	)
	started.Add(2)
	go func() {
		started.Wait()
		close(allIn)
	}()

	buf := NewMessageBuffer(time.Hour, func(agentID, projectID, message string, interrupt bool) error {
		started.Done()
		select {
		case <-allIn:
			metCount.Add(1)
		case <-time.After(5 * time.Second): // generous: free on the passing path
		}
		return nil
	})
	buf.closeWaitTimeout = 30 * time.Second

	buf.Send("agent-a", "p", "hello")
	buf.Send("agent-b", "p", "hello")
	buf.Close()

	if n := metCount.Load(); n != 2 {
		t.Errorf("%d of 2 Close deliveries saw the other key's delivery start; want 2 (keys flushed concurrently)", n)
	}
}

// TestMessageBuffer_RepeatedCloseKeepsFirstDeadline checks that a Close
// after a timed-out Close does not wait another full closeWaitTimeout: all
// Closes share the first Close's deadline, so once it has passed a later
// Close returns at once even though the delivery is still stuck.
func TestMessageBuffer_RepeatedCloseKeepsFirstDeadline(t *testing.T) {
	entered := make(chan struct{})
	release := make(chan struct{})
	t.Cleanup(func() { close(release) }) // free the stuck flush goroutine

	buf := NewMessageBuffer(time.Millisecond, func(agentID, projectID, message string, interrupt bool) error {
		close(entered)
		<-release // stuck until the test ends
		return nil
	})
	const bound = 500 * time.Millisecond
	buf.closeWaitTimeout = bound

	buf.Send("agent-1", "project-a", "hello")
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("timer flush never reached deliverFunc")
	}

	start := time.Now()
	buf.Close() // times out: the delivery is stuck
	if el := time.Since(start); el < bound {
		t.Fatalf("first Close returned after %v, want it to wait out the %v bound", el, bound)
	}

	start = time.Now()
	buf.Close()
	if el := time.Since(start); el > bound/2 {
		t.Errorf("second Close took %v after the first timed out, want well under the %v bound", el, bound)
	}
}
