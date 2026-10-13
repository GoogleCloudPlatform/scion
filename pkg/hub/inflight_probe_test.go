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
	"sync"
	"testing"
	"time"
)

// TestJoinOrStartInFlight pins the shared probe dedupe (ptone/scion#4157):
// a second caller joins the in-flight call without calling start, the
// goroutine removes the entry before beforeDone runs and before done is
// closed, and the first caller after done starts a fresh call.
func TestJoinOrStartInFlight(t *testing.T) {
	type key struct {
		path string
		flag bool
	}
	var m sync.Map
	k := key{path: "/mnt", flag: true}
	release := make(chan struct{})
	starts := 0
	var inFlightAtHook, doneAtHook bool
	var first *inFlightCall[int]
	start := func() (func() int, func()) {
		starts++
		return func() int { <-release; return 42 }, func() {
			_, inFlightAtHook = m.Load(k)
			select {
			case <-first.done:
				doneAtHook = true
			default:
			}
		}
	}

	first = joinOrStartInFlight(&m, k, start)
	if starts != 1 {
		t.Fatalf("starts = %d after the first call, want 1", starts)
	}
	joined := joinOrStartInFlight(&m, k, func() (func() int, func()) {
		t.Fatal("start called for a key already in flight")
		return nil, nil
	})
	if joined != first {
		t.Fatal("second caller did not join the in-flight call")
	}
	// A different key is independent.
	other := joinOrStartInFlight(&m, key{path: "/mnt"}, func() (func() int, func()) {
		return func() int { return 7 }, nil
	})
	<-other.done
	if other.res != 7 {
		t.Errorf("other.res = %d, want 7", other.res)
	}

	close(release)
	select {
	case <-first.done:
	case <-time.After(5 * time.Second):
		t.Fatal("call did not finish after release")
	}
	if first.res != 42 {
		t.Errorf("res = %d, want 42", first.res)
	}
	if inFlightAtHook {
		t.Error("entry still in flight when beforeDone ran")
	}
	if doneAtHook {
		t.Error("done already closed when beforeDone ran")
	}
	if _, ok := m.Load(k); ok {
		t.Error("entry still in flight after done")
	}
	if next := joinOrStartInFlight(&m, k, start); next == first || starts != 2 {
		t.Errorf("call after done: fresh=%v starts=%d, want a fresh call and 2 starts", next != first, starts)
	}
}
