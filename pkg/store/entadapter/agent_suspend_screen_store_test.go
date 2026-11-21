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
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/GoogleCloudPlatform/scion/pkg/store/enttest"
)

func TestAgentSuspendScreenStore_CRUDAndUpsert(t *testing.T) {
	ctx := context.Background()
	cs := NewCompositeStore(enttest.NewClient(t))
	t.Cleanup(func() { _ = cs.Close() })

	projectID := uuid.NewString()
	require.NoError(t, cs.CreateProject(ctx, &store.Project{
		ID:   projectID,
		Name: "screen-crud",
		Slug: "screen-crud-" + projectID[:8],
	}))
	ag := makeAgent(projectID, "worker-1")
	require.NoError(t, cs.CreateAgent(ctx, ag))

	// Missing row returns ErrNotFound.
	_, err := cs.GetAgentSuspendScreen(ctx, ag.ID)
	assert.ErrorIs(t, err, store.ErrNotFound)

	// Deleting a missing row is a no-op.
	require.NoError(t, cs.DeleteAgentSuspendScreen(ctx, ag.ID))

	// Missing agent returns ErrNotFound (FK violation mapped).
	err = cs.PutAgentSuspendScreen(ctx, &store.AgentSuspendScreen{
		AgentID:    uuid.NewString(),
		Content:    "orphan",
		CapturedAt: time.Now().UTC(),
	})
	assert.ErrorIs(t, err, store.ErrNotFound)

	// Invalid input.
	assert.ErrorIs(t, cs.PutAgentSuspendScreen(ctx, nil), store.ErrInvalidInput)
	assert.ErrorIs(t, cs.PutAgentSuspendScreen(ctx, &store.AgentSuspendScreen{AgentID: "not-a-uuid"}), store.ErrInvalidInput)

	// Insert and get.
	t1 := time.Now().UTC().Truncate(time.Microsecond)
	require.NoError(t, cs.PutAgentSuspendScreen(ctx, &store.AgentSuspendScreen{
		AgentID:    ag.ID,
		Content:    "\x1b[32mhello\x1b[0m\n",
		Truncated:  false,
		CapturedAt: t1,
	}))
	got, err := cs.GetAgentSuspendScreen(ctx, ag.ID)
	require.NoError(t, err)
	assert.Equal(t, ag.ID, got.AgentID)
	assert.Equal(t, "\x1b[32mhello\x1b[0m\n", got.Content)
	assert.False(t, got.Truncated)
	assert.True(t, got.CapturedAt.Equal(t1))

	// Upsert replaces the single row in place.
	t2 := t1.Add(time.Minute)
	require.NoError(t, cs.PutAgentSuspendScreen(ctx, &store.AgentSuspendScreen{
		AgentID:    ag.ID,
		Content:    "second screen\n",
		Truncated:  true,
		CapturedAt: t2,
	}))
	got, err = cs.GetAgentSuspendScreen(ctx, ag.ID)
	require.NoError(t, err)
	assert.Equal(t, "second screen\n", got.Content)
	assert.True(t, got.Truncated)
	assert.True(t, got.CapturedAt.Equal(t2))

	// Delete removes the row.
	require.NoError(t, cs.DeleteAgentSuspendScreen(ctx, ag.ID))
	_, err = cs.GetAgentSuspendScreen(ctx, ag.ID)
	assert.ErrorIs(t, err, store.ErrNotFound)
}

func TestAgentSuspendScreenStore_CascadeOnDelete(t *testing.T) {
	ctx := context.Background()
	cs := NewCompositeStore(enttest.NewClient(t))
	t.Cleanup(func() { _ = cs.Close() })

	seedAgentWithScreen := func(projectID, slug string) string {
		ag := makeAgent(projectID, slug)
		require.NoError(t, cs.CreateAgent(ctx, ag))
		require.NoError(t, cs.PutAgentSuspendScreen(ctx, &store.AgentSuspendScreen{
			AgentID:    ag.ID,
			Content:    "saved-" + slug + "\n",
			CapturedAt: time.Now().UTC(),
		}))
		return ag.ID
	}

	t.Run("hard delete agent", func(t *testing.T) {
		pid := uuid.NewString()
		require.NoError(t, cs.CreateProject(ctx, &store.Project{ID: pid, Name: "p-del", Slug: "p-del-" + pid[:8]}))
		aid := seedAgentWithScreen(pid, "del-1")
		require.NoError(t, cs.DeleteAgent(ctx, aid))
		_, err := cs.GetAgentSuspendScreen(ctx, aid)
		assert.ErrorIs(t, err, store.ErrNotFound)
	})

	t.Run("soft delete retains until purge", func(t *testing.T) {
		pid := uuid.NewString()
		require.NoError(t, cs.CreateProject(ctx, &store.Project{ID: pid, Name: "p-purge", Slug: "p-purge-" + pid[:8]}))
		aid := seedAgentWithScreen(pid, "purge-1")

		// Soft-delete the agent with a past timestamp so PurgeDeletedAgents removes it.
		ag, err := cs.GetAgent(ctx, aid)
		require.NoError(t, err)
		ag.DeletedAt = time.Now().UTC().Add(-2 * time.Hour)
		require.NoError(t, cs.UpdateAgent(ctx, ag))

		// Soft-deleted agent still has its row until purge.
		got, err := cs.GetAgentSuspendScreen(ctx, aid)
		require.NoError(t, err)
		assert.Equal(t, "saved-purge-1\n", got.Content)

		purged, err := cs.PurgeDeletedAgents(ctx, time.Now().UTC().Add(-time.Hour))
		require.NoError(t, err)
		assert.GreaterOrEqual(t, purged, 1)

		_, err = cs.GetAgentSuspendScreen(ctx, aid)
		assert.ErrorIs(t, err, store.ErrNotFound)
	})

	t.Run("delete project cascades", func(t *testing.T) {
		pid := uuid.NewString()
		require.NoError(t, cs.CreateProject(ctx, &store.Project{ID: pid, Name: "p-proj", Slug: "p-proj-" + pid[:8]}))
		aid := seedAgentWithScreen(pid, "proj-1")
		require.NoError(t, cs.DeleteProject(ctx, pid))
		_, err := cs.GetAgentSuspendScreen(ctx, aid)
		assert.ErrorIs(t, err, store.ErrNotFound)
	})
}
