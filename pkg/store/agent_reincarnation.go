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

// AgentReincarnation state values.
const (
	AgentReincarnationStatePending      = "pending"
	AgentReincarnationStateStopping     = "stopping"
	AgentReincarnationStateProvisioning = "provisioning"
	AgentReincarnationStateStarting     = "starting"
	AgentReincarnationStateCompleted    = "completed"
	AgentReincarnationStateFailed       = "failed"
)

// AgentReincarnationNonTerminalStates lists the states a pending/in-flight
// reincarnation can be in. Used by the 409-concurrency check (AC-8): a
// second `scion reincarnate` request while one of these is active for the
// same agent is rejected.
var AgentReincarnationNonTerminalStates = []string{
	AgentReincarnationStatePending,
	AgentReincarnationStateStopping,
	AgentReincarnationStateProvisioning,
	AgentReincarnationStateStarting,
}

// IsAgentReincarnationStateNonTerminal reports whether state is one of
// AgentReincarnationNonTerminalStates. Used before trusting a freshly-read
// State as a compare-and-swap expectState (design §3.4 Amendment A7): a
// caller that read the record long after it went terminal must reject that
// terminal value up front, rather than pass it as expectState — a CAS
// "WHERE state = 'failed'" against a row that is ALREADY 'failed' would
// trivially match itself and let a stale caller "win" a race it actually
// lost.
func IsAgentReincarnationStateNonTerminal(state string) bool {
	for _, st := range AgentReincarnationNonTerminalStates {
		if st == state {
			return true
		}
	}
	return false
}

// AgentReincarnation is one row of `scion reincarnate` history: the durable
// record of a single migration attempt (design
// /scion-volumes/scratchpad/projects/agent-migrate/design.md §3.2,
// ptone/scion#1821). A `--rollback` is recorded as its own row, so the
// history stays linear and auditable.
type AgentReincarnation struct {
	ID             string `json:"id"`
	AgentID        string `json:"agentId"`
	FromGeneration int    `json:"fromGeneration"`
	ToGeneration   int    `json:"toGeneration"`

	// RequestedBy is the principal ID (user or agent) that requested the
	// migration; polymorphic like Agent.CreatedBy, no FK.
	RequestedBy string    `json:"requestedBy,omitempty"`
	RequestedAt time.Time `json:"requestedAt"`
	// UpdatedAt is bumped on every UpdateAgentReincarnation call (ent
	// UpdateDefault). The replica-safe boot/periodic sweep (design §3.7)
	// uses it to distinguish a genuinely stale record (the hub that owned it
	// is gone) from one a live worker on another replica is actively
	// stepping through.
	UpdatedAt   time.Time  `json:"updatedAt"`
	CompletedAt *time.Time `json:"completedAt,omitempty"`

	State string `json:"state"`
	Error string `json:"error,omitempty"`

	// PreviousAppliedConfig and NewAppliedConfig are full AgentAppliedConfig
	// snapshots. PreviousAppliedConfig is the rollback source (design §3.7).
	PreviousAppliedConfig *AgentAppliedConfig `json:"previousAppliedConfig,omitempty"`
	NewAppliedConfig      *AgentAppliedConfig `json:"newAppliedConfig,omitempty"`

	// Handoff is the agent-authored handoff text delivered to the new
	// generation's first task.
	Handoff string `json:"handoff,omitempty"`
}

// AgentReincarnationStore defines agent-reincarnation persistence operations.
type AgentReincarnationStore interface {
	// CreateAgentReincarnation creates a new reincarnation record, typically
	// with State == AgentReincarnationStatePending. Returns ErrAlreadyExists
	// if a record with the same ID exists.
	CreateAgentReincarnation(ctx context.Context, r *AgentReincarnation) error

	// GetAgentReincarnation retrieves a reincarnation record by ID. Returns
	// ErrNotFound if it doesn't exist.
	GetAgentReincarnation(ctx context.Context, id string) (*AgentReincarnation, error)

	// UpdateAgentReincarnation updates an existing reincarnation record (state
	// transitions, CompletedAt, Error, NewAppliedConfig). Returns ErrNotFound
	// if the record doesn't exist.
	UpdateAgentReincarnation(ctx context.Context, r *AgentReincarnation) error

	// ListAgentReincarnations returns all reincarnation records for an agent,
	// ordered by RequestedAt descending (most recent first).
	ListAgentReincarnations(ctx context.Context, agentID string) ([]*AgentReincarnation, error)

	// GetPendingAgentReincarnation returns the agent's reincarnation record
	// currently in a non-terminal state (AgentReincarnationNonTerminalStates),
	// if any. Returns ErrNotFound if none is in flight. Used for the
	// 409-concurrency check (AC-8).
	GetPendingAgentReincarnation(ctx context.Context, agentID string) (*AgentReincarnation, error)

	// DeleteAgentReincarnationsForAgent hard-deletes all reincarnation
	// records for an agent. Called from the agent hard-delete cascade
	// (composite.go's DeleteAgent), matching the notification-subscription
	// precedent — agent_id is a plain field with no DB-level FK.
	DeleteAgentReincarnationsForAgent(ctx context.Context, agentID string) error

	// ListStaleNonTerminalAgentReincarnations returns every reincarnation
	// record, across all agents, that is both in a non-terminal state
	// (AgentReincarnationNonTerminalStates) AND has not been updated since
	// before olderThan. Used by the replica-safe boot/periodic sweep (design
	// §3.7): a record can only stay non-terminal past the staleness bound if
	// the hub replica running its worker is gone (crashed or restarted) —
	// claim-then-create prevents an orphan from ever being created by a
	// merely-failed request, and a live worker keeps bumping updated_at as it
	// steps through stopping/provisioning/starting. The bound is what keeps
	// a healthy in-flight migration on one replica safe from a sweep running
	// concurrently on another.
	ListStaleNonTerminalAgentReincarnations(ctx context.Context, olderThan time.Time) ([]*AgentReincarnation, error)

	// TryAdvanceAgentReincarnation atomically writes r's mutable fields
	// (State, Error, CompletedAt, PreviousAppliedConfig, NewAppliedConfig) to
	// the record with ID r.ID, but ONLY if that record's CURRENT State
	// exactly equals expectState AND (when olderThan is non-zero) its
	// CURRENT UpdatedAt is strictly before olderThan — a compare-and-swap
	// performed as a single conditional UPDATE
	// (`WHERE id=? AND state=? [AND updated_at<?]`), analogous to
	// UpdateAgent's state_version check. This is the only way the
	// reincarnation worker, failReincarnation, and the replica-safe sweep may
	// transition a record's state (design §3.4 Amendment A6/A7).
	//
	// expectState must be the exact state the caller is CASing away from, not
	// merely "some non-terminal state" — a coarser "still non-terminal" check
	// let a stale caller win the CAS as long as the record's CURRENT state
	// happened to be non-terminal too, even if it was a different non-terminal
	// state than the caller actually observed. The worker (reincarnate_worker.go's
	// tryAdvanceReincarnation) always passes the literal step it KNOWS it is
	// leaving — never a value read fresh off the record, which can already be
	// terminal — so its own check-and-write is atomic. The replica-safe sweep
	// (advanceListedRecord) passes the value it observed when it listed the
	// record as stale, NOT a fresh read: a fresh read would defeat the whole
	// point, since it would always match "the current state" trivially.
	//
	// olderThan, when non-zero, adds the same staleness bound the sweep used
	// to select this record in the first place (design §3.4 Amendment A7.1):
	// a record a live worker has since touched — bumping UpdatedAt, even
	// while leaving State unchanged — no longer matches, so the sweep cannot
	// act on a record that stopped being stale between its list query and
	// this write. Worker callers always pass the zero value: they are not
	// racing staleness, only ownership of the exact state transition.
	//
	// Returns (true, nil) if a row matched (and was updated). Returns
	// (false, nil) — not an error — if none did: the record does not exist,
	// or — the case this exists to catch — its State no longer equals
	// expectState (or, for the sweep, it is no longer stale) because
	// something else already moved it. Callers MUST treat false as "this
	// caller no longer owns the record as it observed it" and must not act
	// further on it, including writing the agent row.
	TryAdvanceAgentReincarnation(ctx context.Context, r *AgentReincarnation, expectState string, olderThan time.Time) (bool, error)

	// ListAgentReincarnationsPage returns up to limit reincarnation records
	// across all agents, including records whose agent row no longer exists,
	// ordered by ID ascending and starting strictly after afterID ("" starts
	// from the beginning). Used by maintenance sweeps that must visit every
	// record exactly once.
	ListAgentReincarnationsPage(ctx context.Context, afterID string, limit int) ([]*AgentReincarnation, error)

	// UpdateAgentReincarnationSnapshots rewrites only the
	// PreviousAppliedConfig and NewAppliedConfig columns of the record with
	// ID r.ID, and only if that record's CURRENT State equals expectState --
	// the same single conditional UPDATE as TryAdvanceAgentReincarnation. A
	// nil snapshot on r leaves that column unchanged. UpdatedAt is left as
	// stored: a maintenance rewrite is not a lifecycle step. Returns
	// (false, nil) if no row matched.
	UpdateAgentReincarnationSnapshots(ctx context.Context, r *AgentReincarnation, expectState string) (bool, error)
}
