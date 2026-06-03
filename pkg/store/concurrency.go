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

package store

import "context"

// The interfaces in this file are OPTIONAL capabilities that a store backend may
// implement to support running N stateless hub processes against one shared
// database (the multi-replica Postgres deployment, D3).
//
// They are deliberately kept out of the core store.Store interface so that:
//   - backends that do not need cluster coordination (e.g. the single-writer
//     SQLite store, or test fakes that embed store.Store) are unaffected;
//   - callers degrade gracefully via a type assertion: when the capability is
//     absent the caller falls back to the historical single-process behavior,
//     which is correct for a single replica.
//
// See /scion-volumes/scratchpad/postgres-integration/CONCURRENCY-AUDIT.md for
// the per-site mapping of which primitive guards which read-modify-write path.

// AdvisoryLockKey identifies a piece of cluster-wide-once work. Keys must be
// stable across releases and unique per logical job, because they are passed to
// pg_try_advisory_lock as the lock identifier. The chosen values are arbitrary
// but fixed; the 0x5C10 ("SCIO") prefix namespaces them away from any advisory
// keys a future feature might pick.
type AdvisoryLockKey int64

const (
	// LockScheduleEvaluator guards the recurring schedule-evaluator tick so a
	// single replica claims and fires due schedules per tick.
	LockScheduleEvaluator AdvisoryLockKey = 0x5C100001
	// LockAgentHeartbeatTimeout guards the stale-agent → offline sweep.
	LockAgentHeartbeatTimeout AdvisoryLockKey = 0x5C100002
	// LockAgentStalledDetection guards the stalled-agent sweep.
	LockAgentStalledDetection AdvisoryLockKey = 0x5C100003
	// LockSoftDeletePurge guards the soft-deleted-agent / old-event purge.
	LockSoftDeletePurge AdvisoryLockKey = 0x5C100004
	// LockGitHubAppHealthCheck guards the periodic GitHub App installation
	// health check.
	LockGitHubAppHealthCheck AdvisoryLockKey = 0x5C100005
	// LockBrokerAffinityReap guards the stale broker-affinity + stuck dispatch reaper.
	LockBrokerAffinityReap AdvisoryLockKey = 0x5C100006
	// LockBrokerMessageSweep guards the periodic stuck-pending-message sweep (B5-2).
	LockBrokerMessageSweep AdvisoryLockKey = 0x5C100007
)

// AdvisoryLocker is implemented by backends that can take a cluster-wide
// advisory lock. It is the singleton/leader primitive for "run this work on
// exactly one replica per tick" jobs (schedule tick, maintenance, cleanup).
//
// On Postgres this is backed by session-level pg_try_advisory_lock held on a
// dedicated connection for the lifetime of the returned release func. On
// single-writer backends (SQLite) the lock is a no-op that always succeeds:
// there is only ever one writer, so the work is already effectively singleton.
type AdvisoryLocker interface {
	// TryAdvisoryLock attempts to acquire the named advisory lock without
	// blocking. If acquired is true the caller owns the lock and MUST call the
	// returned release func exactly once when the critical section ends
	// (release is always non-nil and safe to call even when acquired is false).
	// If acquired is false another replica currently holds the lock and the
	// caller should skip the work this round.
	TryAdvisoryLock(ctx context.Context, key AdvisoryLockKey) (acquired bool, release func() error, err error)
}

// NOTE: the SERIALIZABLE + retry-on-serialization-failure primitive (P3-4) is
// provided as a concrete, dialect-aware helper on the Ent-backed store
// (entadapter.CompositeStore.RunSerializable) rather than as a store-level
// interface here, because its callback operates on a *sql.Tx and is intended
// for backend-internal multi-row-invariant paths. No core store path requires
// it today (the hot RMW paths use single-row state_version CAS or SELECT ...
// FOR UPDATE, and cross-row uniqueness is enforced by DB constraints); it is
// kept available and tested for future multi-row invariants. See
// CONCURRENCY-AUDIT.md §"Serializable retry".

// ScheduledEventClaimer is implemented by backends that can atomically claim a
// one-shot scheduled event for execution. It is the multi-replica dedup
// primitive for the scheduler's in-memory timers: several replicas may each
// recover the same pending event from the database on startup, but only the
// replica whose atomic UPDATE ... WHERE status = 'pending' affects a row may
// execute the event's side effect (deliver a message, dispatch an agent).
type ScheduledEventClaimer interface {
	// ClaimScheduledEvent atomically transitions a scheduled event from
	// "pending" to claimedStatus. It returns claimed=true if this caller won
	// the claim (the conditional UPDATE affected exactly one row), and
	// claimed=false if the event was already claimed by another replica, was
	// cancelled, or no longer exists. claimedStatus is normally
	// ScheduledEventFired or ScheduledEventExpired.
	ClaimScheduledEvent(ctx context.Context, id string, claimedStatus string) (claimed bool, err error)
}
