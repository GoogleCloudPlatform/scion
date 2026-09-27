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

//go:build !no_sqlite

package entadapter

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/ent"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestChunkUUIDs pins chunkUUIDs' batch-boundary behavior directly: the
// purge's correctness depends on every ID appearing in exactly one chunk, in
// order, with no chunk exceeding size -- an off-by-one here would silently
// drop or duplicate purge candidates.
func TestChunkUUIDs(t *testing.T) {
	mkIDs := func(n int) []uuid.UUID {
		ids := make([]uuid.UUID, n)
		for i := range ids {
			ids[i] = uuid.New()
		}
		return ids
	}

	tests := []struct {
		name       string
		count      int
		size       int
		wantChunks []int // expected length of each chunk, in order
	}{
		{name: "empty", count: 0, size: 2, wantChunks: nil},
		{name: "single item", count: 1, size: 2, wantChunks: []int{1}},
		{name: "exact multiple", count: 4, size: 2, wantChunks: []int{2, 2}},
		{name: "off-by-one remainder", count: 5, size: 2, wantChunks: []int{2, 2, 1}},
		{name: "size larger than input", count: 3, size: 10, wantChunks: []int{3}},
		{name: "size of 1", count: 3, size: 1, wantChunks: []int{1, 1, 1}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ids := mkIDs(tt.count)
			chunks := chunkUUIDs(ids, tt.size)

			require.Len(t, chunks, len(tt.wantChunks))
			var reassembled []uuid.UUID
			for i, chunk := range chunks {
				assert.LessOrEqual(t, len(chunk), tt.size, "chunk %d exceeds size", i)
				assert.Equal(t, tt.wantChunks[i], len(chunk), "chunk %d length", i)
				reassembled = append(reassembled, chunk...)
			}
			if len(ids) == 0 {
				assert.Empty(t, reassembled, "chunks must cover every id exactly once, in order")
			} else {
				assert.Equal(t, ids, reassembled, "chunks must cover every id exactly once, in order")
			}
		})
	}
}

// findIdentityKey returns the row in keys with the given agentID and key
// value, or nil if there is none.
func findIdentityKey(keys []*store.AgentIdentityKey, agentID, key string) *store.AgentIdentityKey {
	for _, k := range keys {
		if k.AgentID == agentID && k.Key == key {
			return k
		}
	}
	return nil
}

// TestCompositeStore_PurgeDeletedAgents_FreesIdentityKeys guards
// CompositeStore.PurgeDeletedAgents against the same hole DeleteAgent and
// DeleteProject already guard against: agent_identity_keys has no DB-level
// FK to agents, so a bulk purge that only deletes the agent row (the
// embedded AgentStore's own implementation) leaves the purged agent's keys
// -- including its own slug -- reserved forever, permanently blocking any
// later agent from taking them.
func TestCompositeStore_PurgeDeletedAgents_FreesIdentityKeys(t *testing.T) {
	cs := newTestCompositeStore(t)
	ctx := context.Background()

	projectID := uuid.New().String()
	require.NoError(t, cs.CreateProject(ctx, &store.Project{
		ID:      projectID,
		Name:    "Purge Project",
		Slug:    "purge-project",
		Created: time.Now(),
		Updated: time.Now(),
	}))

	agentID := uuid.New().String()
	agent := &store.Agent{
		ID:        agentID,
		Slug:      "purge-me",
		Name:      "purge-me",
		ProjectID: projectID,
		Phase:     "stopped",
	}
	require.NoError(t, cs.CreateAgent(ctx, agent))
	require.NoError(t, cs.ReplaceAgentIdentityKeys(ctx, agentID, projectID, []string{"purge-me"}))

	// Soft-delete, old enough to be in scope for the purge cutoff below.
	agent.DeletedAt = time.Now().Add(-48 * time.Hour)
	require.NoError(t, cs.UpdateAgent(ctx, agent))

	purged, err := cs.PurgeDeletedAgents(ctx, time.Now().Add(-24*time.Hour))
	require.NoError(t, err)
	assert.Equal(t, 1, purged)

	_, err = cs.GetAgent(ctx, agentID)
	assert.ErrorIs(t, err, store.ErrNotFound, "purge must remove the agent row")

	keys, err := cs.ListAgentIdentityKeys(ctx, projectID)
	require.NoError(t, err)
	assert.Empty(t, keys, "purge must also free the purged agent's identity keys")

	// A new agent can now take the freed slug/key without conflict.
	newAgentID := uuid.New().String()
	require.NoError(t, cs.CreateAgent(ctx, &store.Agent{
		ID:        newAgentID,
		Slug:      "purge-me",
		Name:      "purge-me",
		ProjectID: projectID,
		Phase:     "stopped",
	}))
	require.NoError(t, cs.ReplaceAgentIdentityKeys(ctx, newAgentID, projectID, []string{"purge-me"}),
		"the freed key must be reusable by a new agent")
}

// TestCompositeStore_PurgeDeletedAgents_RetainsRecentAgentsAndKeys verifies
// the cutoff boundary still holds with identity keys in the mix: an agent
// soft-deleted more recently than cutoff is left alone, row and keys both.
func TestCompositeStore_PurgeDeletedAgents_RetainsRecentAgentsAndKeys(t *testing.T) {
	cs := newTestCompositeStore(t)
	ctx := context.Background()

	projectID := uuid.New().String()
	require.NoError(t, cs.CreateProject(ctx, &store.Project{
		ID:      projectID,
		Name:    "Purge Project",
		Slug:    "purge-project-2",
		Created: time.Now(),
		Updated: time.Now(),
	}))

	agentID := uuid.New().String()
	agent := &store.Agent{
		ID:        agentID,
		Slug:      "recently-deleted",
		Name:      "recently-deleted",
		ProjectID: projectID,
		Phase:     "stopped",
	}
	require.NoError(t, cs.CreateAgent(ctx, agent))
	require.NoError(t, cs.ReplaceAgentIdentityKeys(ctx, agentID, projectID, []string{"recently-deleted"}))

	agent.DeletedAt = time.Now().Add(-1 * time.Hour)
	require.NoError(t, cs.UpdateAgent(ctx, agent))

	purged, err := cs.PurgeDeletedAgents(ctx, time.Now().Add(-24*time.Hour))
	require.NoError(t, err)
	assert.Equal(t, 0, purged)

	_, err = cs.GetAgent(ctx, agentID)
	require.NoError(t, err, "recently soft-deleted agent must be retained")

	keys, err := cs.ListAgentIdentityKeys(ctx, projectID)
	require.NoError(t, err)
	require.Len(t, keys, 1, "retained agent's identity key must not be freed")
}

// TestCompositeStore_PurgeDeletedAgents_RestoreDuringPurgeSurvives is the
// regression anchor for the race a select-then-delete purge (with no
// re-applied eligibility check) opened: on Postgres under READ COMMITTED, an
// agent restored after the purge resolves its candidate IDs, but before it
// deletes them, must survive -- row and identity key both -- rather than
// being hard-deleted anyway because it was still in the stale candidate
// list. A second eligible agent that the hook never touches must still be
// purged normally, proving the fix discriminates within a batch rather than
// exempting it outright. purgeDeletedAgentsTestHook lets this test write the
// restore deterministically into the exact window, through the purge's own
// in-flight transaction rather than a second, genuinely concurrent one (see
// the hook's doc comment: this package's single-connection SQLite test setup
// would deadlock a second writer against this transaction rather than race
// it, since SQLite allows only one writer at a time in production too and so
// has no such window to reproduce in the first place).
func TestCompositeStore_PurgeDeletedAgents_RestoreDuringPurgeSurvives(t *testing.T) {
	cs := newTestCompositeStore(t)
	ctx := context.Background()

	projectID := uuid.New().String()
	require.NoError(t, cs.CreateProject(ctx, &store.Project{
		ID:      projectID,
		Name:    "Purge Race Project",
		Slug:    "purge-race-project",
		Created: time.Now(),
		Updated: time.Now(),
	}))

	agentID := uuid.New().String()
	agentUID := uuid.MustParse(agentID)
	raceAgent := &store.Agent{
		ID:        agentID,
		Slug:      "race-agent",
		Name:      "race-agent",
		ProjectID: projectID,
		Phase:     "stopped",
	}
	require.NoError(t, cs.CreateAgent(ctx, raceAgent))
	require.NoError(t, cs.ReplaceAgentIdentityKeys(ctx, agentID, projectID, []string{"race-agent"}))

	raceAgent.DeletedAt = time.Now().Add(-48 * time.Hour)
	require.NoError(t, cs.UpdateAgent(ctx, raceAgent))

	// A second equally-eligible agent, in the same batch, that is never
	// touched by the hook: it must still be purged normally. This is what
	// pins the predicated delete to discriminating WITHIN a batch, rather
	// than a bug that happens to pass by exempting the whole batch whenever
	// any restore occurs in it.
	stayDeletedID := uuid.New().String()
	stayDeletedAgent := &store.Agent{
		ID:        stayDeletedID,
		Slug:      "stay-deleted-agent",
		Name:      "stay-deleted-agent",
		ProjectID: projectID,
		Phase:     "stopped",
	}
	require.NoError(t, cs.CreateAgent(ctx, stayDeletedAgent))
	require.NoError(t, cs.ReplaceAgentIdentityKeys(ctx, stayDeletedID, projectID, []string{"stay-deleted-agent"}))

	stayDeletedAgent.DeletedAt = time.Now().Add(-48 * time.Hour)
	require.NoError(t, cs.UpdateAgent(ctx, stayDeletedAgent))

	purgeDeletedAgentsTestHook = func(tx *ent.Tx, batchCandidateIDs []uuid.UUID) {
		require.NoError(t, tx.Agent.UpdateOneID(agentUID).ClearDeletedAt().Exec(ctx))
	}
	t.Cleanup(func() { purgeDeletedAgentsTestHook = nil })

	purged, err := cs.PurgeDeletedAgents(ctx, time.Now().Add(-24*time.Hour))
	require.NoError(t, err)
	assert.Equal(t, 1, purged, "exactly the untouched agent must be counted as purged")

	got, err := cs.GetAgent(ctx, agentID)
	require.NoError(t, err, "the agent restored mid-purge must still exist")
	assert.True(t, got.DeletedAt.IsZero(), "the agent restored mid-purge must remain live, not re-deleted")

	_, err = cs.GetAgent(ctx, stayDeletedID)
	assert.ErrorIs(t, err, store.ErrNotFound, "the untouched agent in the same batch must still be purged")

	keys, err := cs.ListAgentIdentityKeys(ctx, projectID)
	require.NoError(t, err)
	require.NotNil(t, findIdentityKey(keys, agentID, "race-agent"),
		"the restored agent's identity key must survive the purge")
	require.Nil(t, findIdentityKey(keys, stayDeletedID, "stay-deleted-agent"),
		"the purged agent's identity key must not survive")
}

// TestCompositeStore_PurgeDeletedAgents_ScopesRemovedSetToBatch is the
// multi-batch regression anchor the single-batch race test above cannot
// reach on its own: with only ever one batch, "the removed set for this
// batch" and "the full candidate list" are the same thing, so a mistake that
// scopes the identity-key delete's ID list to every purge candidate instead
// of just the batch actually removed would pass unnoticed. Here, with
// purgeDeletedAgentsBatchSize forced small, an agent belonging to a LATER
// batch is restored while the FIRST batch is still being processed. If the
// key-delete's ID list were the full candidate list rather than the diff
// actually computed for the batch just deleted, the first batch would
// immediately free every remaining candidate's identity key, including the
// later, now-restored agent's -- leaving it live, once its own batch's
// eligibility predicate correctly excludes its (by-then-restored) row from
// the agent delete, but with no key.
func TestCompositeStore_PurgeDeletedAgents_ScopesRemovedSetToBatch(t *testing.T) {
	originalBatchSize := purgeDeletedAgentsBatchSize
	purgeDeletedAgentsBatchSize = 2
	t.Cleanup(func() { purgeDeletedAgentsBatchSize = originalBatchSize })

	cs := newTestCompositeStore(t)
	ctx := context.Background()

	projectID := uuid.New().String()
	require.NoError(t, cs.CreateProject(ctx, &store.Project{
		ID:      projectID,
		Name:    "Purge Multi-Batch Project",
		Slug:    "purge-multi-batch-project",
		Created: time.Now(),
		Updated: time.Now(),
	}))

	const agentCount = 5
	slugOf := make(map[string]string, agentCount) // agent ID -> slug/key
	ids := make([]string, agentCount)
	for i := 0; i < agentCount; i++ {
		id := uuid.New().String()
		slug := fmt.Sprintf("multi-batch-agent-%d", i)
		ids[i] = id
		slugOf[id] = slug

		a := &store.Agent{
			ID: id, Slug: slug, Name: slug,
			ProjectID: projectID, Phase: "stopped",
		}
		require.NoError(t, cs.CreateAgent(ctx, a))
		require.NoError(t, cs.ReplaceAgentIdentityKeys(ctx, id, projectID, []string{slug}))

		a.DeletedAt = time.Now().Add(-48 * time.Hour)
		require.NoError(t, cs.UpdateAgent(ctx, a))
	}

	// On the first batch invocation only, restore whichever known agent ID is
	// absent from that batch's own candidate list -- with batch size 2 and 5
	// candidates, at least 3 of the 5 are necessarily in a later batch,
	// regardless of the DB's actual row-return order.
	var survivor uuid.UUID
	batchCalls := 0
	purgeDeletedAgentsTestHook = func(tx *ent.Tx, batchCandidateIDs []uuid.UUID) {
		batchCalls++
		if batchCalls != 1 {
			return
		}
		inFirstBatch := make(map[uuid.UUID]bool, len(batchCandidateIDs))
		for _, id := range batchCandidateIDs {
			inFirstBatch[id] = true
		}
		for _, idStr := range ids {
			id := uuid.MustParse(idStr)
			if !inFirstBatch[id] {
				survivor = id
				break
			}
		}
		require.NotEqual(t, uuid.Nil, survivor, "expected at least one candidate outside the first batch")
		require.NoError(t, tx.Agent.UpdateOneID(survivor).ClearDeletedAt().Exec(ctx))
	}
	t.Cleanup(func() { purgeDeletedAgentsTestHook = nil })

	purged, err := cs.PurgeDeletedAgents(ctx, time.Now().Add(-24*time.Hour))
	require.NoError(t, err)
	require.Equal(t, 3, batchCalls, "5 candidates at batch size 2 must span 3 batches (2, 2, 1)")
	assert.Equal(t, agentCount-1, purged, "exactly the untouched agents must be purged")

	survivorIDStr := survivor.String()
	got, err := cs.GetAgent(ctx, survivorIDStr)
	require.NoError(t, err, "the agent restored mid-purge, from a later batch, must still exist")
	assert.True(t, got.DeletedAt.IsZero(), "the restored agent must remain live")

	keys, err := cs.ListAgentIdentityKeys(ctx, projectID)
	require.NoError(t, err)
	for _, idStr := range ids {
		key := findIdentityKey(keys, idStr, slugOf[idStr])
		if idStr == survivorIDStr {
			assert.NotNil(t, key, "the restored agent's identity key must survive the purge, "+
				"not be pre-freed by an earlier batch that scoped its removed-set too broadly")
		} else {
			assert.Nil(t, key, "each actually-purged agent's identity key must not survive")
		}
	}
}
