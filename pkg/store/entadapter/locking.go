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

package entadapter

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"entgo.io/ent/dialect"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// advisoryLockTimeout bounds the two short, non-blocking database operations the
// advisory lock performs: checking a connection out of the pool + running
// pg_try_advisory_lock on acquire, and running pg_advisory_unlock on release.
//
// It is deliberately MUCH shorter than the scheduler's 55s per-handler timeout.
// Both operations are expected to complete in milliseconds: pg_try_advisory_lock
// never waits on the lock (it returns immediately), and checking out a
// connection only blocks when the pool has no usable connection to hand back.
//
// Binding acquire to a short deadline keeps a single bad tick cheap. If the pool
// cannot produce a healthy connection quickly we want to fail this tick fast and
// retry on the next one, NOT block a scheduler goroutine (and its pending pool
// connection request) for nearly the whole 55s window. Letting acquisition hang
// for ~55s lets slow ticks overlap across the 60s scheduler interval and across
// the several singleton handlers that fire each minute, which compounds pool
// pressure instead of shedding it.
//
// Binding release with its own fresh deadline (rather than context.Background)
// guarantees the unlock cannot hang forever on a connection that died while the
// lock was held, which would otherwise prevent conn.Close() from ever running
// and leak the connection out of the pool permanently.
const advisoryLockTimeout = 5 * time.Second

// This file implements the dialect-aware cluster-coordination primitives that
// let N stateless hub processes share one database safely (multi-replica
// Postgres, D3). Every helper degrades to a correct single-process no-op on
// SQLite, where there is only ever one writer.
//
// Compile-time assertions that the Ent-backed store provides the optional
// cluster-coordination capabilities. AdvisoryLocker lives on CompositeStore;
// ScheduledEventClaimer is provided by the embedded ScheduleStore and is thus
// promoted onto CompositeStore as well.
var (
	_ store.AdvisoryLocker        = (*CompositeStore)(nil)
	_ store.ScheduledEventClaimer = (*ScheduleStore)(nil)
	_ store.ScheduledEventClaimer = (*CompositeStore)(nil)
)

// TryAdvisoryLockObject acquires a per-object advisory lock using Postgres's
// two-integer form: pg_try_advisory_lock(int4 classid, int4 objid).
//
// This is the per-project provisioning guard. Two agents for the same project
// (same classID + same objID derived from the project ID hash) contend on the
// same lock; agents for different projects never contend.
//
// The implementation mirrors TryAdvisoryLock but uses the two-int form for
// both lock and unlock. On SQLite it is a no-op (always acquired).
func (c *CompositeStore) TryAdvisoryLockObject(ctx context.Context, classID store.AdvisoryLockKey, objID int32) (bool, func() error, error) {
	if !c.isPostgres() {
		return true, noopRelease, nil
	}

	db := c.DB()
	if db == nil {
		return true, noopRelease, nil
	}

	acquireCtx, cancelAcquire := context.WithTimeout(ctx, advisoryLockTimeout)
	defer cancelAcquire()

	conn, err := db.Conn(acquireCtx)
	if err != nil {
		return false, noopRelease, fmt.Errorf("advisory lock object: acquiring connection: %w", err)
	}

	var acquired bool
	if err := conn.QueryRowContext(acquireCtx,
		"SELECT pg_try_advisory_lock($1, $2)", int32(classID), objID,
	).Scan(&acquired); err != nil {
		_ = conn.Close()
		return false, noopRelease, fmt.Errorf("advisory lock object: pg_try_advisory_lock(%d, %d): %w", int32(classID), objID, err)
	}

	if !acquired {
		_ = conn.Close()
		return false, noopRelease, nil
	}

	release := func() error {
		unlockCtx, cancel := context.WithTimeout(context.Background(), advisoryLockTimeout)
		defer cancel()
		_, unlockErr := conn.ExecContext(unlockCtx,
			"SELECT pg_advisory_unlock($1, $2)", int32(classID), objID,
		)
		closeErr := conn.Close()
		if unlockErr != nil {
			return fmt.Errorf("advisory lock object: pg_advisory_unlock(%d, %d): %w", int32(classID), objID, unlockErr)
		}
		return closeErr
	}
	return true, release, nil
}

// isPostgres reports whether the shared Ent client is talking to Postgres.
func (c *CompositeStore) isPostgres() bool {
	return c.client.Driver().Dialect() == dialect.Postgres
}

// noopRelease is returned whenever there is nothing to unlock (SQLite, or a lock
// that was not acquired). It is always safe to call.
func noopRelease() error { return nil }

// TryAdvisoryLock acquires a cluster-wide advisory lock without blocking.
//
// On Postgres it grabs a dedicated *sql.Conn from the pool and runs
// pg_try_advisory_lock(key) on it. The lock is a SESSION-level lock, so it is
// held for exactly as long as that connection stays checked out: the returned
// release func runs pg_advisory_unlock(key) on the same connection and then
// returns it to the pool. Holding the connection for the duration of the
// critical section is what keeps the lock alive, so callers must keep the work
// short and always call release.
//
// On SQLite (and any non-Postgres backend) the lock is a no-op that always
// succeeds: the single-writer model already guarantees the work runs on one
// process at a time.
func (c *CompositeStore) TryAdvisoryLock(ctx context.Context, key store.AdvisoryLockKey) (bool, func() error, error) {
	if !c.isPostgres() {
		return true, noopRelease, nil
	}

	db := c.DB()
	if db == nil {
		// No *sql.DB to lock against; fail open to single-process behavior
		// rather than blocking cluster work.
		return true, noopRelease, nil
	}

	// Bound connection checkout + the try-lock query to a short deadline derived
	// from ctx (but never longer than advisoryLockTimeout). A healthy pool serves
	// these in milliseconds; if it cannot, we fail this tick fast and let the next
	// one retry rather than parking a scheduler goroutine for the full 55s.
	acquireCtx, cancelAcquire := context.WithTimeout(ctx, advisoryLockTimeout)
	defer cancelAcquire()

	conn, err := db.Conn(acquireCtx)
	if err != nil {
		return false, noopRelease, fmt.Errorf("advisory lock: acquiring connection: %w", err)
	}

	var acquired bool
	// pg_try_advisory_lock returns immediately: true if the lock was granted,
	// false if it is already held (by this or another session).
	if err := conn.QueryRowContext(acquireCtx, "SELECT pg_try_advisory_lock($1)", int64(key)).Scan(&acquired); err != nil {
		_ = conn.Close()
		return false, noopRelease, fmt.Errorf("advisory lock: pg_try_advisory_lock(%d): %w", int64(key), err)
	}

	if !acquired {
		// Another replica holds it. Return the connection to the pool now.
		_ = conn.Close()
		return false, noopRelease, nil
	}

	// We own the lock. release unlocks on the same connection, then frees it.
	// cancelAcquire above only tears down acquireCtx; it does NOT close conn, so
	// the session (and therefore the lock) stays alive until release runs.
	release := func() error {
		// Use a fresh, bounded context detached from the critical section's ctx
		// so the unlock still runs even if that ctx was cancelled, but cannot
		// hang forever on a connection that silently died while we held the
		// lock. Without the bound, a dead connection would block this Exec
		// indefinitely, conn.Close() below would never run, and the connection
		// would leak out of the pool permanently. Closing the connection would
		// also drop the session lock, but unlocking explicitly is cleaner and
		// lets the connection be reused.
		unlockCtx, cancel := context.WithTimeout(context.Background(), advisoryLockTimeout)
		defer cancel()
		_, unlockErr := conn.ExecContext(unlockCtx, "SELECT pg_advisory_unlock($1)", int64(key))
		closeErr := conn.Close()
		if unlockErr != nil {
			return fmt.Errorf("advisory lock: pg_advisory_unlock(%d): %w", int64(key), unlockErr)
		}
		return closeErr
	}
	return true, release, nil
}

// isSerializationFailure reports whether err is a Postgres serialization failure
// that warrants a retry: SQLSTATE 40001 (serialization_failure) or 40P01
// (deadlock_detected). It matches on the SQLSTATE string carried in the error
// message so it does not need a hard dependency on the pgx error type.
func isSerializationFailure(err error) bool {
	if err == nil {
		return false
	}
	type sqlStater interface{ SQLState() string }
	var ss sqlStater
	if errors.As(err, &ss) {
		switch ss.SQLState() {
		case "40001", "40P01":
			return true
		}
	}
	msg := err.Error()
	return contains(msg, "40001") || contains(msg, "40P01") ||
		contains(msg, "serialization") || contains(msg, "deadlock detected")
}

// contains is a tiny substring check kept local to avoid importing strings for a
// single call.
func contains(haystack, needle string) bool {
	if len(needle) == 0 {
		return true
	}
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			return true
		}
	}
	return false
}

// maxSerializableRetries bounds the retry loop so a pathologically contended
// transaction cannot spin forever.
const maxSerializableRetries = 5

// RunSerializable runs fn inside a transaction and, on Postgres, retries it when
// the transaction aborts with a serialization failure (SQLSTATE 40001/40P01).
//
// It is the multi-row-invariant primitive from P3-4: use it when correctness
// depends on a set of rows being read and written as one atomic snapshot and the
// invariant cannot be reduced to a single-row state_version CAS or a SELECT ...
// FOR UPDATE critical section.
//
// fn MUST be idempotent — it can be invoked more than once. It receives the
// transaction it must use for all its statements; using the ambient pooled
// client instead would escape the serializable snapshot.
//
//   - Postgres: BEGIN ISOLATION LEVEL SERIALIZABLE; on commit failure with a
//     serialization error, the whole closure is retried up to
//     maxSerializableRetries times.
//   - SQLite: a single plain transaction with no retry. SQLite executes writes
//     serially, so 40001 cannot occur and the SERIALIZABLE escalation is
//     unnecessary.
func (c *CompositeStore) RunSerializable(ctx context.Context, fn func(ctx context.Context, tx *sql.Tx) error) error {
	db := c.DB()
	if db == nil {
		return fmt.Errorf("RunSerializable: store is not backed by a *sql.DB")
	}

	opts := &sql.TxOptions{}
	if c.isPostgres() {
		opts.Isolation = sql.LevelSerializable
	}

	var lastErr error
	attempts := 1
	if c.isPostgres() {
		attempts = maxSerializableRetries
	}

	for attempt := 0; attempt < attempts; attempt++ {
		tx, err := db.BeginTx(ctx, opts)
		if err != nil {
			if isSerializationFailure(err) {
				lastErr = err
				continue
			}
			return err
		}

		if err := fn(ctx, tx); err != nil {
			_ = tx.Rollback()
			if isSerializationFailure(err) {
				lastErr = err
				continue
			}
			return err
		}

		if err := tx.Commit(); err != nil {
			if isSerializationFailure(err) {
				lastErr = err
				continue
			}
			return err
		}
		return nil
	}
	return fmt.Errorf("RunSerializable: exhausted %d attempts: %w", attempts, lastErr)
}
