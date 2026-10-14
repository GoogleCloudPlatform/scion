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
	"sort"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/ent"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// purgeByIDFixture creates a project and, for each entry of deletedAgo, an
// agent with an identity key, soft-deleted that long ago (0: live). It
// returns the agent IDs in the same order.
func purgeByIDFixture(t *testing.T, cs *CompositeStore, deletedAgo ...time.Duration) (string, []string) {
	t.Helper()
	ctx := context.Background()
	projectID := uuid.New().String()
	require.NoError(t, cs.CreateProject(ctx, &store.Project{
		ID: projectID, Name: "Purge By ID", Slug: "purge-by-id-" + projectID[:8],
		Created: time.Now(), Updated: time.Now(),
	}))
	ids := make([]string, len(deletedAgo))
	for i, ago := range deletedAgo {
		id := uuid.New().String()
		slug := "pbi-" + id[:8]
		a := &store.Agent{ID: id, Slug: slug, Name: slug, ProjectID: projectID, Phase: "stopped"}
		require.NoError(t, cs.CreateAgent(ctx, a))
		require.NoError(t, cs.ReplaceAgentIdentityKeys(ctx, id, projectID, []string{slug}))
		if ago > 0 {
			a.DeletedAt = time.Now().Add(-ago)
			require.NoError(t, cs.UpdateAgent(ctx, a))
		}
		ids[i] = id
	}
	return projectID, ids
}

// PurgeDeletedAgentsByID removes only the listed agents that are past the
// cutoff, with their identity keys; an expired agent not listed, a listed
// one inside the window, a live one and a malformed ID are left alone.
func TestCompositeStore_PurgeDeletedAgentsByID(t *testing.T) {
	cs := newTestCompositeStore(t)
	ctx := context.Background()
	projectID, ids := purgeByIDFixture(t, cs, 48*time.Hour, 48*time.Hour, time.Hour, 0)
	expired, expiredUnlisted, recent, live := ids[0], ids[1], ids[2], ids[3]

	n, err := cs.PurgeDeletedAgentsByID(ctx, []string{expired, recent, live, "not-a-uuid"}, time.Now().Add(-24*time.Hour))
	require.NoError(t, err)
	assert.Equal(t, 1, n)

	_, err = cs.GetAgent(ctx, expired)
	assert.ErrorIs(t, err, store.ErrNotFound, "the listed expired agent is purged")
	for _, id := range []string{expiredUnlisted, recent, live} {
		_, err := cs.GetAgent(ctx, id)
		assert.NoError(t, err, "agent %s must be left alone", id)
	}
	keys, err := cs.ListAgentIdentityKeys(ctx, projectID)
	require.NoError(t, err)
	assert.Len(t, keys, 3, "only the purged agent's identity key is freed")

	n, err = cs.PurgeDeletedAgentsByID(ctx, nil, time.Now())
	require.NoError(t, err)
	assert.Zero(t, n, "no IDs purges nothing")
}

// A listed agent restored before its purge runs survives it (the same
// eligibility re-check as PurgeDeletedAgents).
func TestCompositeStore_PurgeDeletedAgentsByID_RestoreWins(t *testing.T) {
	cs := newTestCompositeStore(t)
	ctx := context.Background()
	_, ids := purgeByIDFixture(t, cs, 48*time.Hour, 48*time.Hour)
	restored, other := ids[0], ids[1]

	restoredUUID := uuid.MustParse(restored)
	purgeDeletedAgentsTestHook = func(tx *ent.Tx, batch []uuid.UUID) {
		require.NoError(t, tx.Agent.UpdateOneID(restoredUUID).ClearDeletedAt().Exec(ctx))
	}
	t.Cleanup(func() { purgeDeletedAgentsTestHook = nil })

	n, err := cs.PurgeDeletedAgentsByID(ctx, []string{restored, other}, time.Now().Add(-24*time.Hour))
	require.NoError(t, err)
	assert.Equal(t, 1, n)
	_, err = cs.GetAgent(ctx, restored)
	assert.NoError(t, err, "the agent restored mid-purge survives")
	_, err = cs.GetAgent(ctx, other)
	assert.ErrorIs(t, err, store.ErrNotFound)
}

// ListSoftDeletedAgentsBefore pages through the agents soft-deleted before
// the cutoff, in ID order, and returns no live or recent ones.
func TestCompositeStore_ListSoftDeletedAgentsBefore(t *testing.T) {
	cs := newTestCompositeStore(t)
	ctx := context.Background()
	_, ids := purgeByIDFixture(t, cs, 48*time.Hour, 72*time.Hour, 96*time.Hour, time.Hour, 0)
	want := append([]string(nil), ids[:3]...)
	sort.Strings(want)
	cutoff := time.Now().Add(-24 * time.Hour)

	var got []string
	after := ""
	for {
		page, err := cs.ListSoftDeletedAgentsBefore(ctx, cutoff, after, 2)
		require.NoError(t, err)
		require.LessOrEqual(t, len(page), 2)
		for _, a := range page {
			assert.False(t, a.DeletedAt.IsZero())
			got = append(got, a.ID)
		}
		if len(page) < 2 {
			break
		}
		after = page[len(page)-1].ID
	}
	assert.Equal(t, want, got)

	none, err := cs.ListSoftDeletedAgentsBefore(ctx, cutoff, "", 0)
	require.NoError(t, err)
	assert.Empty(t, none)
}
