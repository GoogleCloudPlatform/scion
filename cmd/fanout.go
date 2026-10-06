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

package cmd

import (
	"context"
	"sync"
)

// maxFanOutConcurrency caps how many per-recipient sends a group send or a
// broadcast runs at once. The broker limits concurrent dispatch and each
// message costs several runtime lookups, so an unbounded fan-out queues
// requests past the Hub's dispatch deadline (ptone/scion#3521); a handful in
// flight still overlaps request latency without flooding the dispatch path.
const maxFanOutConcurrency = 6

// boundedFanOut calls run(i) for every i in [0, n), in index order, with at
// most limit calls running at once, and returns once every call has
// returned. Calls are dispatched in index order, and none is dispatched once
// ctx is seen to be done. A dispatched call's goroutine may run late, so on
// an interrupt the indices whose work never started are not strictly the
// tail of the list; callers keep results by index instead of relying on it.
//
// Once ctx is done, no further call is started: skip(i) is called instead
// for each remaining index, without waiting for a free slot, so an interrupt
// never hangs on the limit. run must still check ctx itself, since ctx can
// end between a slot being taken and run starting its work.
//
// onQueued, if set, is called each time an index has to wait for a free
// slot (tests use it to observe that the limit is reached).
func boundedFanOut(ctx context.Context, n, limit int, onQueued func(), run, skip func(i int)) {
	if limit < 1 {
		limit = 1
	}
	sem := make(chan struct{}, limit)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		if ctx.Err() != nil {
			skip(i)
			continue
		}
		select {
		case sem <- struct{}{}:
		default:
			if onQueued != nil {
				onQueued()
			}
			select {
			case sem <- struct{}{}:
			case <-ctx.Done():
				skip(i)
				continue
			}
		}
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			defer func() { <-sem }()
			run(idx)
		}(i)
	}
	wg.Wait()
}
