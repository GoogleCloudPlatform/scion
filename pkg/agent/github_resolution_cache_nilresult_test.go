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
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// TestCoalesceFetch_NilFlightResultIsAnError checks that a caller joining a
// flight that finishes with neither a value nor an error gets an error back
// instead of panicking on the type assertion (ptone/scion#6181). A test
// barrier of flight.Do(key, func() { return nil, nil }) started such a
// flight, and a background refresh that joined it crashed the test binary.
//
// The joining caller's own fetch must never run: if it does, the caller
// arrived after the nil flight had finished and led a flight of its own, so
// the attempt is repeated until it joins.
func TestCoalesceFetch_NilFlightResultIsAnError(t *testing.T) {
	cache, err := newTestResolutionCache(t.TempDir(), time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	const flightKey = "nil-result-flight"
	joining := make(chan struct{}, 1)
	hook := func(key string) {
		if key == flightKey {
			select {
			case joining <- struct{}{}:
			default:
			}
		}
	}
	flightJoinHook.Store(&hook)
	t.Cleanup(func() { flightJoinHook.Store(nil) })

	for attempt := 0; attempt < 100; attempt++ {
		select {
		case <-joining:
		default:
		}
		var fetched atomic.Bool
		fetch := func(context.Context) (ResolvedSkill, error) {
			fetched.Store(true)
			return ResolvedSkill{Name: "own"}, nil
		}
		leaderDone := make(chan struct{})
		leaderRunning := make(chan struct{})
		go func() {
			defer close(leaderDone)
			_, _, _ = cache.flight.Do(flightKey, func() (interface{}, error) {
				close(leaderRunning)
				select {
				case <-joining:
				case <-time.After(5 * time.Second):
				}
				// The joiner has reached DoChan; give it time to register.
				time.Sleep(time.Millisecond)
				return nil, nil
			})
		}()
		<-leaderRunning

		// A fresh cache key per attempt, so a value an earlier attempt
		// stored is never found by the in-flight re-check.
		cacheKey := fmt.Sprintf("gh://o/r/s@v%d", attempt)
		skill, ferr := cache.coalesceFetch(context.Background(), flightKey, "cred", cacheKey, "test-ref", false, fetch)
		<-leaderDone
		if fetched.Load() {
			continue
		}
		if ferr == nil {
			t.Fatalf("joined a flight with no result: got %+v and a nil error, want an error", skill)
		}
		if !strings.Contains(ferr.Error(), "test-ref") {
			t.Fatalf("error %q does not name the ref", ferr)
		}
		return
	}
	t.Fatal("the caller never joined the nil-result flight")
}
