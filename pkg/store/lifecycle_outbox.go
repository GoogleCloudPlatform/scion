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

import (
	"context"
	"time"
)

// LifecycleOutboxRow is one serialized lifecycle event waiting for
// delivery (WP1 design 4, 5.3). The payload, including its event_id and
// observed_ts, is frozen at insert, so every delivery attempt sends the
// same bytes. Rows have no foreign key to agents or projects and outlive
// them; a row is deleted once delivered (or dropped by the bounds).
type LifecycleOutboxRow struct {
	Seq                int64
	EventID            string
	AgentID            string
	TransitionRevision int64
	Payload            []byte
	CreatedAt          time.Time
	Attempts           int
	PermanentFailures  int
	NextAttemptAt      time.Time
	LeaseOwner         string
	LeaseUntil         *time.Time
	LastError          string
}

// LifecycleOutboxFailure records a failed delivery attempt.
type LifecycleOutboxFailure struct {
	// CountAttempt increments attempts (transient and permanent failures).
	// A blocked sink releases the row without counting.
	CountAttempt bool
	// Permanent increments permanent_failures (poison accounting).
	Permanent     bool
	NextAttemptAt time.Time
	LastError     string
}

// LifecycleOutboxDropped identifies a row removed by a bound, for the drop
// log.
type LifecycleOutboxDropped struct {
	Seq                int64
	EventID            string
	AgentID            string
	TransitionRevision int64
}

// LifecycleOutboxTrim reports the rows one trim pass removed, by reason.
type LifecycleOutboxTrim struct {
	Age      []LifecycleOutboxDropped
	Overflow []LifecycleOutboxDropped
	Poison   []LifecycleOutboxDropped
}

// LifecycleOutboxStats is one sample of the outbox for the gauges.
type LifecycleOutboxStats struct {
	Pending int64
	// Oldest is the oldest row's created_at; nil when empty.
	Oldest *time.Time
	// MaxSeq is the highest seq present; 0 when empty.
	MaxSeq int64
}

// LifecycleOutboxStore is the drainer's access to the outbox and the
// cursor sweep. Implemented by entadapter.CompositeStore; the hub
// type-asserts for it. Every method runs in its own transaction, and every
// time bound is a Go time.Time bound as a parameter.
type LifecycleOutboxStore interface {
	// ClaimLifecycleOutbox leases up to limit due rows (no live lease,
	// next_attempt_at <= now), in seq order, to owner until leaseUntil.
	// On PostgreSQL the selection skips rows locked by another claimer.
	ClaimLifecycleOutbox(ctx context.Context, owner string, now, leaseUntil time.Time, limit int) ([]LifecycleOutboxRow, error)
	// RenewLifecycleOutboxLease extends owner's lease on seq; false when
	// owner no longer holds it.
	RenewLifecycleOutboxLease(ctx context.Context, seq int64, owner string, leaseUntil time.Time) (bool, error)
	// CompleteLifecycleOutboxRow deletes a delivered row still leased by
	// owner; false when the lease was lost (the row stays and may be
	// delivered again with the same bytes).
	CompleteLifecycleOutboxRow(ctx context.Context, seq int64, owner string) (bool, error)
	// FailLifecycleOutboxRow records a failed attempt on a row leased by
	// owner and releases the lease.
	FailLifecycleOutboxRow(ctx context.Context, seq int64, owner string, f LifecycleOutboxFailure) error
	// TrimLifecycleOutbox removes unleased rows (no lease, or an expired
	// one) older than maxAge, the oldest beyond maxRows, and those with
	// permanent_failures >= maxPermanentFailures.
	TrimLifecycleOutbox(ctx context.Context, now time.Time, maxAge time.Duration, maxRows, maxPermanentFailures int) (LifecycleOutboxTrim, error)
	// LifecycleOutboxHasRows reports whether the outbox holds any row, at
	// constant cost (an existence probe, not a count): the startup check
	// that decides whether a backlog-only drainer must run.
	LifecycleOutboxHasRows(ctx context.Context) (bool, error)
	// LifecycleOutboxStats samples the outbox.
	LifecycleOutboxStats(ctx context.Context) (LifecycleOutboxStats, error)
	// SweepOrphanLifecycleCursors deletes up to limit cursors whose agent
	// row no longer exists and returns how many it deleted.
	SweepOrphanLifecycleCursors(ctx context.Context, limit int) (int, error)
}
