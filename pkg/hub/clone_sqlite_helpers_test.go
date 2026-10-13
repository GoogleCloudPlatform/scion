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

//go:build !no_sqlite

package hub

import (
	"context"
	"sync"
	"time"
)

func raceRequestIDFromContext(ctx context.Context) (int, bool) {
	id, ok := ctx.Value(raceRequestIDKey{}).(int)
	return id, ok
}

// copyBarrier makes concurrent clone requests interleave deterministically.
// arrive blocks the calling goroutine until n distinct requests have all
// made their first Copy call, then releases every one of them at once. That
// guarantees every racing request has already passed its pre-check
// (GetTemplateBySlug/GetHarnessConfigBySlug, which runs before the storage
// path is even computed) before any of them reaches
// CreateTemplate/CreateHarnessConfig — the interleaving the per-request
// storage suffix fix (ptone/scion#1975) exists for, rather than one the
// goroutine scheduler only produces some of the time.
//
// Each request is identified by the raceRequestID carried on its context
// (see raceRequest), not by the path it copies into: with the fix reverted,
// every racing request computes the identical storage path, so a
// path-derived identity would collapse all of them into a single arrival
// and the barrier would never release. A request that copies more than one
// file only ever contributes to the arrival count once: its first Copy call
// arrives and blocks, and by the time it is released its own ID is already
// marked seen, so a second Copy call for the same request returns
// immediately from the already-closed release channel instead of
// re-arriving at the barrier.
//
// If a future change let some racing request exit before its first Copy
// call (a new early validation, a transient store error on the fast-path
// read), fewer than n requests would ever arrive and a waiter would block
// forever. arrive guards against that by giving up after
// copyBarrierArriveTimeout and recording the timeout so the test goroutine
// can fail with a clear message instead of hanging until go test's global
// timeout.
type copyBarrier struct {
	mu          sync.Mutex
	n           int
	seen        map[int]bool
	arrived     int
	release     chan struct{}
	releaseOnce sync.Once
	timedOut    bool
}

func (b *copyBarrier) arrive(id int) {
	b.mu.Lock()
	if b.seen[id] {
		b.mu.Unlock()
		b.wait()
		return
	}
	b.seen[id] = true
	b.arrived++
	last := b.arrived == b.n
	b.mu.Unlock()

	if last {
		b.releaseOnce.Do(func() { close(b.release) })
		return
	}
	b.wait()
}

// wait blocks until the barrier releases or copyBarrierArriveTimeout
// elapses, whichever comes first. On timeout it records the failure so
// `failed` can report it, then closes release through releaseOnce so it
// happens exactly once even if arrive already closed it or an earlier
// timeout already did: later Copy calls from the same request then find
// release already closed instead of each waiting out their own
// copyBarrierArriveTimeout. Either way wait returns so the calling request
// proceeds rather than hanging.
func (b *copyBarrier) wait() {
	select {
	case <-b.release:
	case <-time.After(copyBarrierArriveTimeout):
		b.mu.Lock()
		b.timedOut = true
		b.mu.Unlock()
		b.releaseOnce.Do(func() { close(b.release) })
	}
}

// failed reports whether any arrive call gave up waiting instead of being
// released normally, i.e. fewer than n racing requests ever reached Copy.
// Tests that use a copyBarrier should call this after every request they
// fired has returned and t.Fatalf if it reports true, since that means the
// assertions that follow would be checking a race that never happened as
// intended.
func (b *copyBarrier) failed() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.timedOut
}

// copyBarrierArriveTimeout bounds how long a single arrive call waits to be
// released. It only matters when the barrier is stuck (see copyBarrier's doc
// comment above); it just needs to be comfortably longer than any real test
// run.
const copyBarrierArriveTimeout = 10 * time.Second

// raceRequestIDKey carries a per-goroutine request identity through the
// request context so copyBarrier (below) can tell racing requests apart even
// when they compute the identical storage path — which is exactly what
// happens with the per-request storage suffix reverted (ptone/scion#1975).
// Identifying requests by the directory portion of the path they copy into
// would break in that scenario: with the suffix reverted, every racing
// request computes the same deterministic (scope, scopeID, slug) path with
// no per-request suffix, so all of them would look like the same "request"
// to the barrier and only one would ever arrive, hanging the other n-1
// forever.
type raceRequestIDKey struct{}
