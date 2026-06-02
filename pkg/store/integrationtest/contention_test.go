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

//go:build integration

// Category 1 — Contention. These tests put N writers in genuine, simultaneous
// conflict over a single row (or unique key) on a real multi-writer Postgres and
// assert the store's concurrency-control primitives hold: optimistic state_version
// compare-and-swap, the SKIP LOCKED / conditional-UPDATE event claim, and
// database unique constraints. None of this is observable on the single-writer
// SQLite fallback, so every test gates on requirePG via newStore.
package integrationtest

import (
	"context"
	"errors"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// TestContention_StateVersionCAS races N goroutines to increment a counter on a
// single agent through the optimistic state_version compare-and-swap in
// UpdateAgent.
//
// The workers proceed in two phases. A synchronization barrier forces all N to
// read the SAME starting version before any of them writes, so the first write
// round is guaranteed to produce exactly one winner and N-1 conflicts. Each loser
// then re-reads and retries until it too commits. This makes the lower bound on
// retries deterministic (>= N-1) rather than dependent on goroutine scheduling.
//
// Asserted invariants:
//   - exactly N successful UpdateAgent calls (every worker commits once);
//   - >= N-1 conflicts observed (real contention occurred);
//   - final state_version == initial(1) + N (each commit bumped it exactly once);
//   - final counter == N — NO LOST UPDATES (every increment landed on the row).
func TestContention_StateVersionCAS(t *testing.T) {
	cs := newStore(t)
	ctx := context.Background()
	n := concurrency(t)

	project := seedProject(t, cs)
	ag := makeAgent(project.ID, "cas-"+shortID())
	require.NoError(t, cs.CreateAgent(ctx, ag))
	require.Equal(t, int64(1), ag.StateVersion, "CreateAgent seeds state_version=1")

	var successes, retries int64
	errs := make(chan error, n)

	// readBarrier releases every worker's first write only after all N have done
	// their initial stale read, guaranteeing the first round conflicts N-1 times.
	var readBarrier sync.WaitGroup
	readBarrier.Add(n)

	bump := func(a *store.Agent) {
		cur := 0
		if a.Annotations != nil {
			if v, ok := a.Annotations["counter"]; ok {
				cur, _ = strconv.Atoi(v)
			}
		}
		a.Annotations = map[string]string{"counter": strconv.Itoa(cur + 1)}
	}

	runConcurrently(n, func(int) {
		// Phase 1: stale read shared across all workers.
		a, err := cs.GetAgent(ctx, ag.ID)
		if err != nil {
			readBarrier.Done()
			errs <- err
			return
		}
		readBarrier.Done()
		readBarrier.Wait() // every worker now holds version 1

		// Phase 2: first (lockstep) write attempt, then retry-until-success.
		for {
			bump(a)
			err = cs.UpdateAgent(ctx, a)
			if err == nil {
				atomic.AddInt64(&successes, 1)
				return
			}
			if errors.Is(err, store.ErrVersionConflict) {
				atomic.AddInt64(&retries, 1)
				a, err = cs.GetAgent(ctx, ag.ID) // re-read latest version
				if err != nil {
					errs <- err
					return
				}
				continue
			}
			errs <- err
			return
		}
	})
	close(errs)
	for err := range errs {
		require.NoError(t, err, "unexpected error during CAS contention")
	}

	assert.Equal(t, int64(n), successes, "every worker must commit exactly once")
	assert.GreaterOrEqualf(t, retries, int64(n-1),
		"expected >= N-1 (%d) conflicts under true contention, got %d", n-1, retries)

	final, err := cs.GetAgent(ctx, ag.ID)
	require.NoError(t, err)
	assert.Equal(t, int64(1+n), final.StateVersion,
		"final state_version must equal initial(1) + N commits")
	require.NotNil(t, final.Annotations)
	assert.Equal(t, strconv.Itoa(n), final.Annotations["counter"],
		"counter must equal N — a smaller value means a lost update")
	t.Logf("CAS contention: N=%d successes=%d retries=%d finalVersion=%d",
		n, successes, retries, final.StateVersion)
}

// TestContention_ClaimScheduledEventSingleWinner races N goroutines to claim the
// SAME pending scheduled event (ClaimScheduledEvent's conditional
// UPDATE ... WHERE status='pending'). This mirrors N hub replicas each recovering
// the same pending event on startup: exactly one must win the pending->fired
// transition and execute the side effect; the rest must lose cleanly.
func TestContention_ClaimScheduledEventSingleWinner(t *testing.T) {
	cs := newStore(t)
	ctx := context.Background()
	n := concurrency(t)

	project := seedProject(t, cs)
	evt := makeScheduledEvent(project.ID)
	require.NoError(t, cs.CreateScheduledEvent(ctx, evt))

	var wins int64
	errs := make(chan error, n)
	runConcurrently(n, func(int) {
		won, err := cs.ClaimScheduledEvent(ctx, evt.ID, store.ScheduledEventFired)
		if err != nil {
			errs <- err
			return
		}
		if won {
			atomic.AddInt64(&wins, 1)
		}
	})
	close(errs)
	for err := range errs {
		require.NoError(t, err, "unexpected error during claim race")
	}

	assert.Equal(t, int64(1), wins, "exactly one concurrent claim must win")

	got, err := cs.GetScheduledEvent(ctx, evt.ID)
	require.NoError(t, err)
	assert.Equal(t, store.ScheduledEventFired, got.Status)
	assert.NotNil(t, got.FiredAt)
}

// TestContention_SkipLockedDisjointClaims drains a pool of M pending events with
// N concurrent pollers, each looping ListPendingScheduledEvents (SELECT ... FOR
// UPDATE SKIP LOCKED on Postgres) followed by ClaimScheduledEvent. The
// SKIP-LOCKED select hands disjoint row sets to overlapping pollers and the
// conditional claim is the final dedup, so the invariant is: every event is
// claimed EXACTLY ONCE in total — no event dropped, none double-fired.
func TestContention_SkipLockedDisjointClaims(t *testing.T) {
	cs := newStore(t)
	ctx := context.Background()
	n := concurrency(t)
	m := 5 * n // comfortably more events than pollers

	project := seedProject(t, cs)
	for i := 0; i < m; i++ {
		require.NoError(t, cs.CreateScheduledEvent(ctx, makeScheduledEvent(project.ID)))
	}

	var mu sync.Mutex
	claimedBy := make(map[string]int) // event id -> number of winning claims
	errs := make(chan error, n)

	runConcurrently(n, func(int) {
		for {
			pending, err := cs.ListPendingScheduledEvents(ctx)
			if err != nil {
				errs <- err
				return
			}
			if len(pending) == 0 {
				return // nothing left for anyone
			}
			for _, e := range pending {
				won, err := cs.ClaimScheduledEvent(ctx, e.ID, store.ScheduledEventFired)
				if err != nil {
					errs <- err
					return
				}
				if won {
					mu.Lock()
					claimedBy[e.ID]++
					mu.Unlock()
				}
			}
		}
	})
	close(errs)
	for err := range errs {
		require.NoError(t, err, "unexpected error during SKIP LOCKED drain")
	}

	assert.Len(t, claimedBy, m, "every event must be claimed exactly once (count of distinct winners)")
	total := 0
	for id, c := range claimedBy {
		assert.Equalf(t, 1, c, "event %s was claimed %d times (want exactly 1)", id, c)
		total += c
	}
	assert.Equal(t, m, total, "total winning claims must equal the number of events")
}

// TestContention_UniqueProjectSlug races N goroutines to create projects with the
// same slug. The slug unique index must admit exactly one and reject the rest
// with store.ErrAlreadyExists.
func TestContention_UniqueProjectSlug(t *testing.T) {
	cs := newStore(t)
	ctx := context.Background()
	n := concurrency(t)
	slug := "dup-project-" + shortID()

	ok, dup := raceUniqueCreate(t, n, func() error {
		return cs.CreateProject(ctx, makeProject(slug))
	})
	assert.Equal(t, int64(1), ok, "exactly one project create must succeed")
	assert.Equal(t, int64(n-1), dup, "all other creates must return ErrAlreadyExists")
}

// TestContention_UniqueUserEmail races N goroutines to create users with the same
// (case-insensitively normalized) email.
func TestContention_UniqueUserEmail(t *testing.T) {
	cs := newStore(t)
	ctx := context.Background()
	n := concurrency(t)
	email := "dup-" + shortID() + "@example.com"

	ok, dup := raceUniqueCreate(t, n, func() error {
		return cs.CreateUser(ctx, makeUser(email))
	})
	assert.Equal(t, int64(1), ok, "exactly one user create must succeed")
	assert.Equal(t, int64(n-1), dup, "all other creates must return ErrAlreadyExists")
}

// TestContention_UniqueAgentSlug races N goroutines to create agents with the same
// (slug, project_id) composite unique key inside one project.
func TestContention_UniqueAgentSlug(t *testing.T) {
	cs := newStore(t)
	ctx := context.Background()
	n := concurrency(t)
	project := seedProject(t, cs)
	slug := "dup-agent-" + shortID()

	ok, dup := raceUniqueCreate(t, n, func() error {
		return cs.CreateAgent(ctx, makeAgent(project.ID, slug))
	})
	assert.Equal(t, int64(1), ok, "exactly one agent create must succeed")
	assert.Equal(t, int64(n-1), dup, "all other creates must return ErrAlreadyExists")
}

// raceUniqueCreate runs create n times concurrently and returns (successes,
// already-exists). Any other error fails the test. Each create must target the
// same unique key but a distinct primary-key id (the factories use a fresh UUID
// per call), so the only possible conflict is the intended unique-key violation.
func raceUniqueCreate(t *testing.T, n int, create func() error) (successes, alreadyExists int64) {
	t.Helper()
	errs := make(chan error, n)
	runConcurrently(n, func(int) {
		switch err := create(); {
		case err == nil:
			atomic.AddInt64(&successes, 1)
		case errors.Is(err, store.ErrAlreadyExists):
			atomic.AddInt64(&alreadyExists, 1)
		default:
			errs <- err
		}
	})
	close(errs)
	for err := range errs {
		require.NoError(t, err, "unexpected (non-duplicate) error during unique-create race")
	}
	return successes, alreadyExists
}
