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

package clock

import (
	"testing"
	"time"
)

var t0 = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

func TestFakeFiresInDeadlineOrder(t *testing.T) {
	c := NewFake(t0)
	var got []string
	c.AfterFunc(3*time.Second, func() { got = append(got, "c") })
	c.AfterFunc(time.Second, func() {
		got = append(got, "a")
		// Re-armed inside the window: fires in the same Advance.
		c.AfterFunc(time.Second, func() { got = append(got, "b") })
	})
	stopped := c.AfterFunc(2*time.Second, func() { got = append(got, "stopped") })
	if !stopped.Stop() || stopped.Stop() {
		t.Fatal("Stop should report true once")
	}
	c.AfterFunc(10*time.Second, func() { got = append(got, "late") })
	c.Advance(5 * time.Second)
	if want := "abc"; join(got) != want {
		t.Fatalf("fired %v, want %s", got, want)
	}
	if !c.Now().Equal(t0.Add(5 * time.Second)) {
		t.Fatalf("Now = %v", c.Now())
	}
	if c.Pending() != 1 {
		t.Fatalf("Pending = %d, want 1", c.Pending())
	}
}

func TestFakeTimerSeesItsDeadline(t *testing.T) {
	c := NewFake(t0)
	var at time.Time
	c.AfterFunc(2*time.Second, func() { at = c.Now() })
	c.Advance(time.Minute)
	if !at.Equal(t0.Add(2 * time.Second)) {
		t.Fatalf("callback saw %v, want deadline", at)
	}
}

func TestFakeWaitFor(t *testing.T) {
	c := NewFake(t0)
	go c.AfterFunc(time.Second, func() {})
	if !c.WaitFor(10*time.Second, func(n int) bool { return n == 1 }) {
		t.Fatal("WaitFor missed the armed timer")
	}
	if c.WaitFor(time.Millisecond, func(n int) bool { return n == 2 }) {
		t.Fatal("WaitFor reported an impossible condition")
	}
}

func TestAfter(t *testing.T) {
	c := NewFake(t0)
	ch, stop := After(c, time.Second)
	c.Advance(time.Second)
	select {
	case <-ch:
	default:
		t.Fatal("After did not fire")
	}
	_, stop2 := After(c, time.Second)
	if !stop2() {
		t.Fatal("stop of a pending After returned false")
	}
	_ = stop
	r := Real()
	ch, _ = After(r, time.Millisecond)
	<-ch
}

func join(s []string) string {
	out := ""
	for _, x := range s {
		out += x
	}
	return out
}
