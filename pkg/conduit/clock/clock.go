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

// Package clock provides the injectable time source used by pkg/conduit.
//
// Every timer in the conduit session (keepalive, write deadline, stream open
// timeout, drain deadline, reconnect backoff) is created through a Clock so
// that tests can drive time deterministically with a Fake instead of
// sleeping.
package clock

import (
	"sort"
	"sync"
	"time"
)

// Clock is a source of time and timers.
type Clock interface {
	// Now returns the current time.
	Now() time.Time
	// AfterFunc calls f in its own goroutine (Real) or synchronously from
	// Advance (Fake) once d has elapsed.
	AfterFunc(d time.Duration, f func()) Timer
}

// Timer is a stoppable timer created by Clock.AfterFunc.
type Timer interface {
	// Stop prevents the timer from firing. It reports whether the call
	// stopped the timer (false if it already fired or was stopped).
	Stop() bool
}

// After returns a channel that receives the current time once d has elapsed
// on c. The returned stop function releases the timer early.
func After(c Clock, d time.Duration) (<-chan time.Time, func() bool) {
	ch := make(chan time.Time, 1)
	t := c.AfterFunc(d, func() { ch <- c.Now() })
	return ch, t.Stop
}

// Real returns a Clock backed by package time.
func Real() Clock { return realClock{} }

type realClock struct{}

func (realClock) Now() time.Time { return time.Now() }

func (realClock) AfterFunc(d time.Duration, f func()) Timer { return time.AfterFunc(d, f) }

// Fake is a manually advanced Clock for tests. Timers fire synchronously,
// in deadline order, from Advance, outside the clock's lock, so a callback
// may itself create or stop timers.
type Fake struct {
	mu      sync.Mutex
	now     time.Time
	seq     uint64
	timers  map[*fakeTimer]struct{}
	changed chan struct{}
}

// NewFake returns a Fake clock reading t.
func NewFake(t time.Time) *Fake {
	return &Fake{now: t, timers: map[*fakeTimer]struct{}{}, changed: make(chan struct{})}
}

type fakeTimer struct {
	c   *Fake
	at  time.Time
	seq uint64
	f   func()
}

// Now implements Clock.
func (c *Fake) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

// AfterFunc implements Clock. A non-positive d fires on the next Advance
// (including Advance(0)).
func (c *Fake) AfterFunc(d time.Duration, f func()) Timer {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.seq++
	t := &fakeTimer{c: c, at: c.now.Add(d), seq: c.seq, f: f}
	c.timers[t] = struct{}{}
	c.notifyLocked()
	return t
}

func (t *fakeTimer) Stop() bool {
	t.c.mu.Lock()
	defer t.c.mu.Unlock()
	if _, ok := t.c.timers[t]; !ok {
		return false
	}
	delete(t.c.timers, t)
	t.c.notifyLocked()
	return true
}

func (c *Fake) notifyLocked() {
	close(c.changed)
	c.changed = make(chan struct{})
}

// Advance moves the clock forward by d, firing every timer whose deadline is
// reached, in deadline order. Timers created by callbacks with a deadline
// inside the advanced window also fire.
func (c *Fake) Advance(d time.Duration) {
	c.mu.Lock()
	target := c.now.Add(d)
	c.mu.Unlock()
	for {
		c.mu.Lock()
		var due []*fakeTimer
		for t := range c.timers {
			if !t.at.After(target) {
				due = append(due, t)
			}
		}
		if len(due) == 0 {
			c.now = target
			c.mu.Unlock()
			return
		}
		sort.Slice(due, func(i, j int) bool {
			if due[i].at.Equal(due[j].at) {
				return due[i].seq < due[j].seq
			}
			return due[i].at.Before(due[j].at)
		})
		t := due[0]
		delete(c.timers, t)
		if t.at.After(c.now) {
			c.now = t.at
		}
		c.notifyLocked()
		c.mu.Unlock()
		t.f()
	}
}

// Pending returns the number of armed timers.
func (c *Fake) Pending() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.timers)
}

// WaitFor blocks until cond (evaluated with the number of armed timers)
// returns true or timeout of real time elapses. It reports whether cond
// became true. Tests use it to wait until the code under test has armed
// its timers before calling Advance.
func (c *Fake) WaitFor(timeout time.Duration, cond func(pending int) bool) bool {
	deadline := time.NewTimer(timeout)
	defer deadline.Stop()
	for {
		c.mu.Lock()
		ok := cond(len(c.timers))
		ch := c.changed
		c.mu.Unlock()
		if ok {
			return true
		}
		select {
		case <-ch:
		case <-deadline.C:
			return false
		}
	}
}
