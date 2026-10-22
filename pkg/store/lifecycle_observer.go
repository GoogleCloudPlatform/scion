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

// This file holds the agent-write observer contract (WP1 design 5.1): the
// store calls one observer inside the transaction of every observed agent
// write, with the row before and after the write, so lifecycle events are
// derived from committed transitions, in commit order, whichever hub path
// made the write. The observer is installed on the concrete store
// (entadapter.CompositeStore.SetAgentWriteObserver), not through the Store
// interface, so store fakes are unaffected.

import (
	"context"
	"time"
)

// AgentWriteOp names the kind of observed agent write.
type AgentWriteOp string

const (
	// AgentWriteCreate: CreateAgent. Before is nil.
	AgentWriteCreate AgentWriteOp = "create"
	// AgentWriteSoftDelete: a soft finalize (deleted_at set; the row stays).
	AgentWriteSoftDelete AgentWriteOp = "soft_delete"
	// AgentWriteHardDelete: a hard finalize or DeleteAgent. After is nil.
	AgentWriteHardDelete AgentWriteOp = "hard_delete"
	// AgentWriteProjectDelete: DeleteProject's bulk removal of the
	// project's agents. After is nil.
	AgentWriteProjectDelete AgentWriteOp = "project_delete"
	// AgentWritePurge: the retention purge of a soft-deleted row. After
	// is nil.
	AgentWritePurge AgentWriteOp = "purge"
)

// AgentWrite is one observed agent write.
type AgentWrite struct {
	Op AgentWriteOp
	// Before is the full row read under lock before the write (nil for a
	// create). After is the row as committed (nil when the row was
	// removed).
	Before, After *Agent
	// Now is the writer's commit-time clock, the transition's event time.
	Now time.Time
}

// AgentID returns the written agent's ID.
func (w AgentWrite) AgentID() string {
	if w.After != nil {
		return w.After.ID
	}
	if w.Before != nil {
		return w.Before.ID
	}
	return ""
}

// AgentWriteObserver is called inside the writer's transaction, after the
// write and before commit. A returned error rolls the write back, so an
// observer returns errors only for database failures of its own writes;
// it handles derivation and serialization failures itself (counted and
// logged, never blocking the product write).
type AgentWriteObserver interface {
	ObserveAgentWrite(ctx context.Context, tx LifecycleTx, w AgentWrite) error
}

// LifecycleTx is the observer's access to the writer's transaction: the
// per-agent lifecycle cursor and the outbox. Every method runs in that
// transaction.
type LifecycleTx interface {
	// GetLifecycleCursor reads the agent's cursor, locked for update
	// where the database supports row locks. It returns nil, nil when the
	// agent has no cursor.
	GetLifecycleCursor(ctx context.Context, agentID string) (*LifecycleCursor, error)
	// BumpLifecycleCursor allocates n transition revisions for the agent
	// (transition_revision += n, n >= 0) and writes upd, then returns the
	// cursor as written. With upd.Create it inserts the cursor with
	// transition_revision = n instead.
	BumpLifecycleCursor(ctx context.Context, agentID string, n int, upd LifecycleCursorUpdate) (*LifecycleCursor, error)
	// DeleteLifecycleCursor removes the agent's cursor, if any.
	DeleteLifecycleCursor(ctx context.Context, agentID string) error
	// InsertLifecycleOutbox inserts serialized events, ready to drain.
	InsertLifecycleOutbox(ctx context.Context, rows []LifecycleOutboxRow) error
	// AfterCommit runs f once the writer's transaction has committed, and
	// never if it rolls back.
	AfterCommit(f func())
}

// LifecycleCursor is one agent's lifecycle sequencing state (design 4):
// operational state, not analytics, deleted with the agent.
type LifecycleCursor struct {
	AgentID            string
	ProjectID          string
	TransitionRevision int64
	MetadataRevision   int64
	MetadataDigest     string
	LastFingerprint    string
	// LastDeleted is whether the last observed row was deleted (deleted_at
	// set), so a removal that finds a live last state knows a deletion
	// went unobserved (review r6 N2).
	LastDeleted bool

	// Run-segment state. Created in slice 1 and written only by slice 2.
	OpenRun                 string
	OpenRunGeneration       string
	OpenRunMetadataRevision int64
	PendingRunChangeAt      *time.Time
	ReincarnationBaseRunID  string
	PendingAdoptedFrom      string
	PendingAdoptedAt        *time.Time
	PendingAdoptionSource   string

	// Creator principal captured at created, copied into later snapshots.
	CreatorPrincipalType string
	CreatorPrincipalID   string

	UpdatedAt time.Time
}

// LifecycleCursorUpdate is what one observed write records in the cursor.
type LifecycleCursorUpdate struct {
	// Create inserts the cursor (the agent has none yet).
	Create    bool
	ProjectID string
	// MetadataRevision and MetadataDigest are the values after this write.
	MetadataRevision int64
	MetadataDigest   string
	LastFingerprint  string
	LastDeleted      bool
	// Creator fields are written only with Create.
	CreatorPrincipalType string
	CreatorPrincipalID   string
	Now                  time.Time
}
