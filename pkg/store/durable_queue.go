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
	"fmt"
	"time"
)

// Durable queue column and operation bounds (stage1-design §4.5, stage2-design §6.2).
const (
	MaxDurableQueueKindLen        = 64
	MaxDurableQueueDedupKeyLen    = 256
	MaxDurableQueueLastErrorBytes = 256
	MaxDurableQueueNudgeKeys      = 1000
)

// ErrDuplicate is returned by EnqueueDurableItem (and durablequeue.Queue.Enqueue)
// when an item with the same (kind, dedup_key) already exists in the queue.
// It wraps ErrAlreadyExists so generic store conflict checks match as well.
var ErrDuplicate = fmt.Errorf("%w: duplicate durable queue item", ErrAlreadyExists)

// Tx is the store transaction handle passed to transactional queue producers
// (typically the store.Store yielded by Store.WithTx). Passing nil executes
// the enqueue on the DurableQueueStore's own connection in auto-commit mode.
type Tx any

// IsNudgeableDedupKey reports whether k can match a stored durable_queue_items
// dedup_key (non-empty and at most MaxDurableQueueDedupKeyLen bytes). Keys
// outside these bounds cannot exist in the store and are ignored as unknown
// keys by nudge operations.
func IsNudgeableDedupKey(k string) bool {
	return k != "" && len(k) <= MaxDurableQueueDedupKeyLen
}

// ValidateDurableQueueKind checks that kind matches ^[a-z0-9_]+(\.[a-z0-9_]+)*$
// and is at most 64 bytes.
func ValidateDurableQueueKind(kind string) error {
	if len(kind) == 0 || len(kind) > MaxDurableQueueKindLen {
		return fmt.Errorf("%w: durable queue kind length must be 1..%d bytes", ErrInvalidInput, MaxDurableQueueKindLen)
	}
	segLen := 0
	for i := 0; i < len(kind); i++ {
		c := kind[i]
		if c == '.' {
			if segLen == 0 {
				return fmt.Errorf("%w: invalid durable queue kind %q", ErrInvalidInput, kind)
			}
			segLen = 0
			continue
		}
		if (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') || c == '_' {
			segLen++
			continue
		}
		return fmt.Errorf("%w: invalid durable queue kind %q", ErrInvalidInput, kind)
	}
	if segLen == 0 {
		return fmt.Errorf("%w: invalid durable queue kind %q", ErrInvalidInput, kind)
	}
	return nil
}

// DurableQueueRow is one item in durable_queue_items.
//
// Payload is opaque and immutable to the queue, and must be valid UTF-8 without
// NUL (0x00) bytes (stored in a TEXT column on SQLite and PostgreSQL). Rows
// carry no foreign keys and are deleted on completion or when trimmed by age,
// overflow, or poison bounds. Rows returned by TrimDurableQueue omit Payload.
type DurableQueueRow struct {
	Seq               int64
	Kind              string
	DedupKey          string
	ProjectID         string
	Payload           []byte
	NotBefore         time.Time
	CreatedAt         time.Time
	Attempts          int
	PermanentFailures int
	NextAttemptAt     time.Time
	LeaseOwner        string
	LeaseUntil        *time.Time
	LastError         string
}

// DurableQueueFailure records a failed or released delivery attempt.
type DurableQueueFailure struct {
	// CountAttempt increments attempts (transient and permanent failures).
	// Releasing a row due to a blocked sink or cancelled pass leaves
	// CountAttempt false.
	CountAttempt bool
	// Permanent increments permanent_failures toward the poison bound.
	Permanent     bool
	NextAttemptAt time.Time
	LastError     string
}

// DurableQueueTrim reports the rows removed by one TrimDurableQueue pass,
// grouped by drop reason. Trimmed rows carry metadata only
// (Seq, Kind, ProjectID, DedupKey, CreatedAt); Payload is nil.
type DurableQueueTrim struct {
	Age      []DurableQueueRow
	Overflow []DurableQueueRow
	Poison   []DurableQueueRow
	// More is true when the per-pass candidate cap was reached and additional
	// unleased rows remain eligible for trimming on the next pass.
	More bool
}

// DurableQueueStats is a point-in-time sample of durable_queue_items for a
// kind (optionally scoped to a single project).
type DurableQueueStats struct {
	// Pending is the total number of incomplete items in the queue for the
	// sampled scope (including currently leased items).
	Pending int64
	// Oldest is the earliest created_at among items in the sampled scope,
	// or nil when Pending is 0.
	Oldest *time.Time
	// Leased is the number of items in the sampled scope currently holding
	// a non-expired lease.
	Leased int64
}

// DurableQueueStore is the persistence contract for pkg/durablequeue.
// Implemented by entadapter.CompositeStore and type-asserted by callers
// (like LifecycleOutboxStore) so store.Store fakes are unaffected.
type DurableQueueStore interface {
	// EnqueueDurableItem inserts row into durable_queue_items, running inside
	// tx when tx is non-nil. Attempt, failure, and lease fields on row are
	// ignored on insert (new items always start unleased with zero attempts).
	// Returns ErrDuplicate when (kind, dedup_key) already exists.
	EnqueueDurableItem(ctx context.Context, tx Tx, row DurableQueueRow) (int64, error)
	// ClaimDurableItems leases up to limit due rows of kind
	// (next_attempt_at <= now && (lease_until IS NULL || lease_until < now))
	// to owner until leaseUntil, ordered by (next_attempt_at, seq). On
	// PostgreSQL the selection uses FOR UPDATE SKIP LOCKED.
	ClaimDurableItems(ctx context.Context, kind, owner string, now, leaseUntil time.Time, limit int) ([]DurableQueueRow, error)
	// RenewDurableLease extends owner's lease on seq; returns false when owner
	// no longer holds the row.
	RenewDurableLease(ctx context.Context, seq int64, owner string, leaseUntil time.Time) (bool, error)
	// CompleteDurableItem deletes seq while it is still leased by owner;
	// returns false when the lease was lost.
	CompleteDurableItem(ctx context.Context, seq int64, owner string) (bool, error)
	// FailDurableItem records a failure or lease release on seq while leased
	// by owner and clears the lease; returns held=false when the lease was lost.
	FailDurableItem(ctx context.Context, seq int64, owner string, f DurableQueueFailure) (held bool, err error)
	// TrimDurableQueue removes up to a bounded batch of unleased rows of kind
	// exceeding maxAge, maxPermanent, or maxRows (removing oldest items of the
	// largest project first on overflow) and returns the dropped rows' metadata
	// (without Payload) by reason. When More is true on the returned struct,
	// additional candidates remain for the next maintenance pass.
	TrimDurableQueue(ctx context.Context, kind string, now time.Time, maxAge time.Duration, maxRows, maxPermanent int) (DurableQueueTrim, error)
	// DurableQueueStats samples pending count, oldest created_at, and active
	// leased count (evaluated against the caller's now, consistent with
	// ClaimDurableItems and TrimDurableQueue) for kind. When projectID is "",
	// all projects are sampled.
	DurableQueueStats(ctx context.Context, kind, projectID string, now time.Time) (DurableQueueStats, error)
	// NudgeDurableItems sets next_attempt_at = now for unleased items
	// (lease_until IS NULL || lease_until < now) of kind matching dedupKeys
	// whose current next_attempt_at is strictly after now, returning the
	// number of rows updated. Items already due at or before now keep their
	// earlier next_attempt_at so (next_attempt_at, seq) claim ordering never
	// delays them; leased items and other kinds/keys are untouched; unknown
	// keys are ignored; and an empty dedupKeys slice is a no-op. Zero now
	// defaults to time.Now().UTC(). Returns ErrInvalidInput when kind is
	// invalid or len(dedupKeys) > MaxDurableQueueNudgeKeys.
	NudgeDurableItems(ctx context.Context, kind string, dedupKeys []string, now time.Time) (int, error)
}

// DurableQueuePendingCounter counts incomplete durable_queue_items for kind
// (optionally scoped to projectID, or all projects when projectID is "") in a
// single COUNT query without sampling active leases or oldest created_at.
// Implemented by entadapter.CompositeStore and entadapter.DurableQueueStore.
type DurableQueuePendingCounter interface {
	CountDurableQueuePending(ctx context.Context, kind, projectID string) (int64, error)
}

// DurableQueueFairClaimer claims due, unleased items of a kind using
// lexicographic round-robin across active project_ids starting strictly after
// afterProjectID (wrapping around through <= afterProjectID), preserving
// (next_attempt_at, seq) order within each project and returning claimed rows
// interleaved round-robin across projects.
// Implemented by entadapter.CompositeStore and entadapter.DurableQueueStore.
type DurableQueueFairClaimer interface {
	ClaimDurableItemsFair(ctx context.Context, kind, owner, afterProjectID string, now, leaseUntil time.Time, limit int) ([]DurableQueueRow, error)
}
