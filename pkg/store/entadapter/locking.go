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

	"entgo.io/ent/dialect"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

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

	conn, err := db.Conn(ctx)
	if err != nil {
		return false, noopRelease, fmt.Errorf("advisory lock: acquiring connection: %w", err)
	}

	var acquired bool
	// pg_try_advisory_lock returns immediately: true if the lock was granted,
	// false if it is already held (by this or another session).
	if err := conn.QueryRowContext(ctx, "SELECT pg_try_advisory_lock($1)", int64(key)).Scan(&acquired); err != nil {
		_ = conn.Close()
		return false, noopRelease, fmt.Errorf("advisory lock: pg_try_advisory_lock(%d): %w", int64(key), err)
	}

	if !acquired {
		// Another replica holds it. Return the connection to the pool now.
		_ = conn.Close()
		return false, noopRelease, nil
	}

	// We own the lock. release unlocks on the same connection, then frees it.
	release := func() error {
		// Use a background context so the unlock still runs even if the
		// critical section's ctx has been cancelled. Closing the connection
		// would also drop the session lock, but unlocking explicitly is
		// cleaner and lets the connection be reused.
		_, unlockErr := conn.ExecContext(context.Background(), "SELECT pg_advisory_unlock($1)", int64(key))
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
