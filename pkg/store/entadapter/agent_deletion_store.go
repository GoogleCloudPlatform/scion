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
	"fmt"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/ent"
	"github.com/GoogleCloudPlatform/scion/pkg/ent/agent"
	"github.com/GoogleCloudPlatform/scion/pkg/ent/brokerdispatch"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/google/uuid"
)

// copyTimePtr returns a copy of t, so a store.Agent never aliases an
// ent.Agent's time pointer.
func copyTimePtr(t *time.Time) *time.Time {
	if t == nil {
		return nil
	}
	c := *t
	return &c
}

// entAgentDeletionActive is store.Agent.DeletionActive on an ent row, for
// writers that already hold the locked ent.Agent. It delegates through
// entAgentToStore so there is a single definition of the predicate.
func entAgentDeletionActive(a *ent.Agent, now time.Time) bool {
	return entAgentToStore(a).DeletionActive(now)
}

// updateAgentDeletionAttempts bounds UpdateAgentDeletion's retry when its
// state_version CAS misses (only possible on dialects without row locks).
const updateAgentDeletionAttempts = 5

// updateAgentDeletionHook, when set by a test, runs inside each
// UpdateAgentDeletion attempt between the read and the conditional write.
var updateAgentDeletionHook func(ctx context.Context, tx *ent.Tx, id string)

// UpdateAgentDeletion implements store.AgentStore. See the interface for the
// contract.
//
// Each attempt reads the row inside a transaction (SELECT ... FOR UPDATE
// where supported), evaluates the predicate in Go with
// DeletionPredicate.Matches, and writes conditionally on the state_version
// it read, bumping it. On a CAS miss the attempt is retried from a fresh
// read, so the predicate is always evaluated against the row the write
// lands on.
func (s *AgentStore) UpdateAgentDeletion(ctx context.Context, id string, pred store.DeletionPredicate, set store.DeletionFields) (int, error) {
	uid, err := parseUUID(id)
	if err != nil {
		return 0, err
	}
	useLock := s.usesRowLocks(ctx)

	for attempt := 0; attempt < updateAgentDeletionAttempts; attempt++ {
		affected, retry, err := s.updateAgentDeletionOnce(ctx, uid, pred, set, useLock)
		if err != nil || !retry {
			return affected, err
		}
	}
	return 0, fmt.Errorf("update agent deletion %s: %w", id, store.ErrVersionConflict)
}

func (s *AgentStore) updateAgentDeletionOnce(ctx context.Context, uid uuid.UUID, pred store.DeletionPredicate, set store.DeletionFields, useLock bool) (affected int, retry bool, err error) {
	tx, err := s.client.Tx(ctx)
	if err != nil {
		return 0, false, err
	}
	defer func() { _ = tx.Rollback() }()

	q := tx.Agent.Query().Where(agent.IDEQ(uid))
	if useLock {
		q = q.ForUpdate()
	}
	row, err := q.Only(ctx)
	if err != nil {
		if ent.IsNotFound(err) {
			return 0, false, nil
		}
		return 0, false, mapError(err)
	}
	current := entAgentToStore(row)
	if !pred.Matches(current) {
		return 0, false, nil
	}
	if set.Derive != nil {
		set.Derive(current, &set)
	}

	if updateAgentDeletionHook != nil {
		updateAgentDeletionHook(ctx, tx, uid.String())
	}

	n, err := applyAgentDeletionFields(ctx, tx, row, set)
	if err != nil {
		return 0, false, err
	}
	if n == 0 {
		// The row changed between the read and the write (no row lock on
		// this dialect). Re-read and re-evaluate.
		return 0, true, nil
	}
	if err := tx.Commit(); err != nil {
		return 0, false, err
	}
	return n, false, nil
}

// applyAgentDeletionFields writes set onto row inside tx, conditional on the
// state_version it read (bumped). n == 0 means the CAS missed.
func applyAgentDeletionFields(ctx context.Context, tx *ent.Tx, row *ent.Agent, set store.DeletionFields) (int, error) {
	now := time.Now()
	upd := tx.Agent.Update().
		Where(agent.IDEQ(row.ID), agent.StateVersionEQ(row.StateVersion)).
		SetStateVersion(row.StateVersion + 1)
	if set.KeepUpdated {
		// The schema's UpdateDefault would stamp updated; pin the old value.
		upd.SetUpdated(row.Updated)
	} else {
		upd.SetUpdated(now)
	}
	if set.State != nil {
		upd.SetDeletionState(*set.State)
	}
	switch {
	case set.BumpClaim:
		upd.SetDeletionClaim(row.DeletionClaim + 1)
	case set.Claim != nil:
		upd.SetDeletionClaim(*set.Claim)
	}
	applyTime := func(v *time.Time, clear bool, setFn func(time.Time) *ent.AgentUpdate, clearFn func() *ent.AgentUpdate) {
		switch {
		case v != nil:
			setFn(*v)
		case clear:
			clearFn()
		}
	}
	applyTime(set.LeaseAt, set.ClearLeaseAt, upd.SetDeletionLeaseAt, upd.ClearDeletionLeaseAt)
	applyTime(set.StartedAt, set.ClearStartedAt, upd.SetDeletionStartedAt, upd.ClearDeletionStartedAt)
	applyTime(set.FailedAt, set.ClearFailedAt, upd.SetDeletionFailedAt, upd.ClearDeletionFailedAt)
	if set.Code != nil {
		upd.SetDeletionCode(*set.Code)
	}
	if set.Error != nil {
		upd.SetDeletionError(*set.Error)
	}
	if set.Prior != nil {
		upd.SetDeletionPrior(*set.Prior)
	}
	if set.Request != nil {
		upd.SetDeletionRequest(*set.Request)
	}
	if set.Phase != nil {
		upd.SetPhase(*set.Phase)
	}
	if set.Activity != nil {
		upd.SetActivity(*set.Activity)
	}
	if set.DeletedAt != nil {
		upd.SetDeletedAt(*set.DeletedAt)
	}

	n, err := upd.Save(ctx)
	if err != nil {
		return 0, mapError(err)
	}
	return n, nil
}

// outstandingDispatchStates are the broker_dispatch states that mean an
// intent may still execute.
var outstandingDispatchStates = []string{store.DispatchStatePending, store.DispatchStateInProgress}

// HasOutstandingBrokerDispatch implements store.BrokerDispatchStore.
func (s *BrokerDispatchStore) HasOutstandingBrokerDispatch(ctx context.Context, agentID, op string) (bool, error) {
	uid, err := parseUUID(agentID)
	if err != nil {
		return false, err
	}
	ok, err := s.client.BrokerDispatch.Query().
		Where(
			brokerdispatch.AgentIDEQ(uid),
			brokerdispatch.OpEQ(op),
			brokerdispatch.StateIn(outstandingDispatchStates...),
		).
		Exist(ctx)
	if err != nil {
		return false, mapError(err)
	}
	return ok, nil
}

// HasCompletedBrokerDispatchSince implements store.BrokerDispatchStore.
func (s *BrokerDispatchStore) HasCompletedBrokerDispatchSince(ctx context.Context, agentID, op string, since time.Time) (bool, error) {
	uid, err := parseUUID(agentID)
	if err != nil {
		return false, err
	}
	ok, err := s.client.BrokerDispatch.Query().
		Where(
			brokerdispatch.AgentIDEQ(uid),
			brokerdispatch.OpEQ(op),
			brokerdispatch.StateEQ(store.DispatchStateDone),
			brokerdispatch.UpdatedAtGTE(since),
		).
		Exist(ctx)
	if err != nil {
		return false, mapError(err)
	}
	return ok, nil
}
