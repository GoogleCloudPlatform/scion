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
	"errors"
	"time"
)

// Forced user deletion (ptone/scion#2143).
//
// DELETE /api/v1/users/{id}?force=true revokes the user's authority and
// records a UserDeletion (the tombstone) in one transaction, sets the
// user's status to UserStatusDeleting, and lists the blocking agents as
// UserDeletionAgent rows. The hub then stops those agents through the
// agent delete engine and, once every one is stopped or removed, deletes
// the user row and marks the deletion completed in one transaction.

// ErrUserDeleting is returned by UserStore.UpdateUser for a user whose
// status is UserStatusDeleting, and for an update that would set that
// status.
var ErrUserDeleting = errors.New("the user is being deleted")

// UserDeletion states.
const (
	UserDeletionStateStopping  = "stopping"
	UserDeletionStateCompleted = "completed"
)

// UserDeletionAgent outcomes.
const (
	// UserDeletionOutcomePending: no delete of the agent is in flight.
	UserDeletionOutcomePending = "pending"
	// UserDeletionOutcomeStopping: a delete of the agent is in flight.
	UserDeletionOutcomeStopping = "stopping"
	// UserDeletionOutcomeFailed: the agent's latest delete failed.
	UserDeletionOutcomeFailed = "failed"
	// UserDeletionOutcomeStopped: the delete engine confirmed the agent's
	// runtime teardown (the row is soft-deleted, or a non-force engine run
	// removed it).
	UserDeletionOutcomeStopped = "stopped"
	// UserDeletionOutcomeRemoved: the agent row is gone and the cascade did
	// not confirm its runtime teardown (for example an operator force
	// delete).
	UserDeletionOutcomeRemoved = "removed"
)

// ValidUserDeletionOutcome reports whether o is a UserDeletionAgent outcome.
func ValidUserDeletionOutcome(o string) bool {
	switch o {
	case UserDeletionOutcomePending, UserDeletionOutcomeStopping, UserDeletionOutcomeFailed,
		UserDeletionOutcomeStopped, UserDeletionOutcomeRemoved:
		return true
	}
	return false
}

// UserDeletion is the tombstone of a forced user deletion. ID is the
// operation ID recorded on the edges and service-account assignments the
// request deactivated. The record outlives the user row and is never
// deleted, so UserID keeps resolving as deleted.
type UserDeletion struct {
	ID              string
	UserID          string
	State           string // UserDeletionState*
	RequestedByKind string
	RequestedByID   string
	CredentialKind  string
	RequestedAt     time.Time
	CompletedAt     *time.Time
	// LastReconciledAt is when the hub last reconciled the deletion.
	LastReconciledAt *time.Time
	// BeforeSummary is JSON: the user's role, status and kind before the
	// request. It never carries the email.
	BeforeSummary string
	// OrphanedByRevoke is JSON: the edges the request deactivated whose
	// delegate is an agent the cascade does not stop.
	OrphanedByRevoke string
	// LastError is the latest reconcile error, for operators.
	LastError string
}

// UserDeletionAgent is a blocking agent of a forced user deletion and the
// cascade's latest view of it.
type UserDeletionAgent struct {
	DeletionID string
	AgentID    string
	Slug       string
	ProjectID  string
	Outcome    string // UserDeletionOutcome*
	Attempts   int
	LastError  string
	UpdatedAt  time.Time
}

// UserDeletionStore persists forced user deletions.
type UserDeletionStore interface {
	// CreateUserDeletion records a deletion in state stopping. It
	// requires a UserID that is a UUID and a RequestedAt, generates ID
	// when empty, and returns ErrAlreadyExists when the user already has
	// a deletion (user_id is unique: an ID is never reused).
	CreateUserDeletion(ctx context.Context, d *UserDeletion) error

	// GetUserDeletion returns a deletion by ID, or ErrNotFound.
	GetUserDeletion(ctx context.Context, id string) (*UserDeletion, error)

	// GetUserDeletionByUser returns the deletion of a user, or
	// ErrNotFound. It answers after the user row is gone.
	GetUserDeletionByUser(ctx context.Context, userID string) (*UserDeletion, error)

	// ListUserDeletionsByState returns up to limit deletions in state,
	// least recently reconciled first (never reconciled before any other),
	// then oldest request first. A caller that reconciles each deletion it
	// lists and records that (TouchUserDeletionReconciled) so works through
	// every deletion in turn, however many there are.
	ListUserDeletionsByState(ctx context.Context, state string, limit int) ([]UserDeletion, error)

	// CompleteUserDeletion moves a deletion from stopping to completed at
	// at, and reports whether this call changed it (false when it is not
	// stopping). It returns ErrNotFound for an unknown ID.
	CompleteUserDeletion(ctx context.Context, id string, at time.Time) (bool, error)

	// TouchUserDeletionReconciled records a reconcile at at and its error
	// ("" for none) on a deletion in state stopping. A completed deletion
	// is left unchanged (nil); an unknown ID returns ErrNotFound.
	TouchUserDeletionReconciled(ctx context.Context, id string, at time.Time, lastErr string) error

	// UpsertUserDeletionAgent creates or replaces the record of one agent
	// of a deletion, keyed by (DeletionID, AgentID). A record whose
	// outcome is stopped or removed is final: the call leaves it unchanged
	// and returns nil, so a writer holding an older copy of the record
	// cannot replace a confirmed outcome. It returns ErrInvalidInput for
	// an unknown outcome and ErrNotFound for an unknown deletion. It is a
	// single conditional write per statement and never fails on a
	// concurrent insert, so it is safe inside a transaction.
	UpsertUserDeletionAgent(ctx context.Context, a *UserDeletionAgent) error

	// ReopenUserDeletionAgent creates or replaces the record of one agent
	// of a deletion whatever its outcome, including stopped and removed.
	// It is meant only for the completion of a deletion, for an agent that
	// blocks the user again. Lock order: the caller holds the user row
	// lock, then locks the agent row (LockAgentRows) and re-reads it, and
	// reopens only an agent that is present and not soft-deleted. The delete
	// engine's finalize takes the agent row and then the records, so a
	// finalize that commits first keeps its stopped record. Errors are as
	// for UpsertUserDeletionAgent.
	ReopenUserDeletionAgent(ctx context.Context, a *UserDeletionAgent) error

	// ListUserDeletionAgents returns the agents of a deletion, by agent ID.
	ListUserDeletionAgents(ctx context.Context, deletionID string) ([]UserDeletionAgent, error)

	// ListUserDeletionAgentsForAgent returns the records of an agent in
	// deletions in state stopping.
	ListUserDeletionAgentsForAgent(ctx context.Context, agentID string) ([]UserDeletionAgent, error)
}
