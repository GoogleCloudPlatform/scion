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

	"github.com/GoogleCloudPlatform/scion/pkg/ent"
	"github.com/GoogleCloudPlatform/scion/pkg/ent/agent"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/google/uuid"
)

// FinalizeAgentDeletion implements store.Store. See the interface for the
// contract. Like UpdateAgentDeletion, an attempt whose state_version CAS
// misses (only possible without row locks) is retried from a fresh read.
func (c *CompositeStore) FinalizeAgentDeletion(ctx context.Context, id string, pred store.DeletionPredicate, mode store.DeletionFinalizeMode, set store.DeletionFields, hook store.DeletionFinalizeHook) (int, error) {
	if c.inTx {
		return 0, fmt.Errorf("finalize agent deletion %s: %w: called inside WithTx", id, store.ErrInvalidInput)
	}
	switch mode {
	case store.DeletionFinalizeSoft, store.DeletionFinalizeHard:
	default:
		return 0, fmt.Errorf("finalize agent deletion %s: %w: mode %q", id, store.ErrInvalidInput, mode)
	}
	uid, err := parseUUID(id)
	if err != nil {
		return 0, err
	}
	useLock := c.AgentStore.usesRowLocks(ctx)
	for attempt := 0; attempt < updateAgentDeletionAttempts; attempt++ {
		affected, retry, err := c.finalizeAgentDeletionOnce(ctx, uid, pred, mode, set, hook, useLock)
		if err != nil || !retry {
			return affected, err
		}
	}
	return 0, fmt.Errorf("finalize agent deletion %s: %w", id, store.ErrVersionConflict)
}

func (c *CompositeStore) finalizeAgentDeletionOnce(ctx context.Context, uid uuid.UUID, pred store.DeletionPredicate, mode store.DeletionFinalizeMode, set store.DeletionFields, hook store.DeletionFinalizeHook, useLock bool) (affected int, retry bool, err error) {
	tx, err := c.client.Tx(ctx)
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
	txStore := newTxCompositeStore(tx)

	hookAgent := current
	switch mode {
	case store.DeletionFinalizeSoft:
		if set.Derive != nil {
			set.Derive(current, &set)
		}
		n, err := applyAgentDeletionFields(ctx, tx, row, set)
		if err != nil {
			return 0, false, err
		}
		if n == 0 {
			return 0, true, nil
		}
		post, err := tx.Agent.Get(ctx, uid)
		if err != nil {
			return 0, false, mapError(err)
		}
		hookAgent = entAgentToStore(post)
	case store.DeletionFinalizeHard:
		n, err := tx.Agent.Delete().
			Where(agent.IDEQ(uid), agent.StateVersionEQ(row.StateVersion)).
			Exec(ctx)
		if err != nil {
			return 0, false, mapError(err)
		}
		if n == 0 {
			return 0, true, nil
		}
		if err := txStore.deleteAgentDependents(ctx, uid.String()); err != nil {
			return 0, false, mapError(err)
		}
	}

	if hook != nil {
		if err := hook(ctx, txStore, hookAgent, mode); err != nil {
			return 0, false, err
		}
	}
	if err := tx.Commit(); err != nil {
		return 0, false, err
	}
	return 1, false, nil
}
