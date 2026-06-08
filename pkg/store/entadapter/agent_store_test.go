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

	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/GoogleCloudPlatform/scion/pkg/store/enttest"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

var agentTestProjectUID = uuid.MustParse("30000000-0000-0000-0000-0000000000a1")

// newTestAgentStore returns a fresh Ent-backed AgentStore with a single project
// seeded to satisfy the required project FK. MaxOpenConns is pinned to 1 so the
// in-memory SQLite backend serializes the transactional RMW paths.
func newTestAgentStore(t *testing.T) (*AgentStore, string) {
	t.Helper()
	client := enttest.NewClient(t)

	_, err := client.Project.Create().
		SetID(agentTestProjectUID).
		SetName("test-project").
		SetSlug("test-project").
		Save(context.Background())
	require.NoError(t, err)

	return NewAgentStore(client), agentTestProjectUID.String()
}

// makeAgent builds a minimal valid agent for the seeded project.
func makeAgent(projectID, slug string) *store.Agent {
	return &store.Agent{
		ID:        uuid.NewString(),
		Slug:      slug,
		Name:      "Agent " + slug,
		Template:  "default",
		ProjectID: projectID,
		Phase:     "running",
		Activity:  "thinking",
		Labels:    map[string]string{"k": "v"},
	}
}

func TestAgentStore_CRUD(t *testing.T) {
	ctx := context.Background()
	s, projectID := newTestAgentStore(t)

	a := makeAgent(projectID, "crud-1")
	a.AppliedConfig = &store.AgentAppliedConfig{Image: "img:1", Model: "opus"}
	require.NoError(t, s.CreateAgent(ctx, a))
	assert.Equal(t, int64(1), a.StateVersion, "CreateAgent should initialize state_version to 1")
	assert.False(t, a.Created.IsZero())

	// Get by ID round-trips all the fields we set.
	got, err := s.GetAgent(ctx, a.ID)
	require.NoError(t, err)
	assert.Equal(t, a.Slug, got.Slug)
	assert.Equal(t, a.Name, got.Name)
	assert.Equal(t, a.ProjectID, got.ProjectID)
	assert.Equal(t, "running", got.Phase)
	assert.Equal(t, map[string]string{"k": "v"}, got.Labels)
	require.NotNil(t, got.AppliedConfig)
	assert.Equal(t, "img:1", got.AppliedConfig.Image)
	assert.Equal(t, "opus", got.AppliedConfig.Model)

	// Get by slug.
	bySlug, err := s.GetAgentBySlug(ctx, projectID, "crud-1")
	require.NoError(t, err)
	assert.Equal(t, a.ID, bySlug.ID)

	// Missing IDs surface as ErrNotFound.
	_, err = s.GetAgent(ctx, uuid.NewString())
	assert.ErrorIs(t, err, store.ErrNotFound)
	_, err = s.GetAgentBySlug(ctx, projectID, "does-not-exist")
	assert.ErrorIs(t, err, store.ErrNotFound)

	// Update bumps state_version and persists changes.
	got.Name = "Renamed"
	got.Phase = "stopped"
	require.NoError(t, s.UpdateAgent(ctx, got))
	assert.Equal(t, int64(2), got.StateVersion)

	reread, err := s.GetAgent(ctx, a.ID)
	require.NoError(t, err)
	assert.Equal(t, "Renamed", reread.Name)
	assert.Equal(t, "stopped", reread.Phase)
	assert.Equal(t, int64(2), reread.StateVersion)

	// Delete is a hard delete.
	require.NoError(t, s.DeleteAgent(ctx, a.ID))
	_, err = s.GetAgent(ctx, a.ID)
	assert.ErrorIs(t, err, store.ErrNotFound)
	assert.ErrorIs(t, s.DeleteAgent(ctx, a.ID), store.ErrNotFound)
}

// TestAgentStore_CreatedByNonUserPrincipal guards against the regression where
// created_by/owner_id carried a foreign-key edge to the users table. When an
// agent creates a sub-agent, those columns hold the *creating agent's* ID, which
// has no users-table row — under the FK that produced a constraint violation
// (mapped to ErrInvalidInput → a 400 "Invalid input" on agent creation). They
// are polymorphic principal references and must accept an arbitrary principal ID.
func TestAgentStore_CreatedByNonUserPrincipal(t *testing.T) {
	ctx := context.Background()
	s, projectID := newTestAgentStore(t)

	// A principal ID that is NOT a user (e.g. another agent). No users row exists.
	creatorPrincipalID := uuid.NewString()

	a := makeAgent(projectID, "sub-agent")
	a.CreatedBy = creatorPrincipalID
	a.OwnerID = creatorPrincipalID
	require.NoError(t, s.CreateAgent(ctx, a),
		"creating an agent owned by a non-user principal must not violate a foreign key")

	got, err := s.GetAgent(ctx, a.ID)
	require.NoError(t, err)
	assert.Equal(t, creatorPrincipalID, got.CreatedBy)
	assert.Equal(t, creatorPrincipalID, got.OwnerID)
}

// TestAgentStore_AncestryFilter exercises the dialect-switched json_each /
// json_array_elements_text membership filter.
func TestAgentStore_AncestryFilter(t *testing.T) {
	ctx := context.Background()
	s, projectID := newTestAgentStore(t)

	root := "user-root"
	mid := "agent-mid"

	// child is a descendant of both root and mid.
	child := makeAgent(projectID, "child")
	child.Ancestry = []string{root, mid}
	require.NoError(t, s.CreateAgent(ctx, child))

	// sibling descends only from root.
	sibling := makeAgent(projectID, "sibling")
	sibling.Ancestry = []string{root}
	require.NoError(t, s.CreateAgent(ctx, sibling))

	// orphan has no ancestry at all.
	orphan := makeAgent(projectID, "orphan")
	require.NoError(t, s.CreateAgent(ctx, orphan))

	// Filtering by root returns both descendants but not the orphan.
	byRoot, err := s.ListAgents(ctx, store.AgentFilter{AncestorID: root}, store.ListOptions{})
	require.NoError(t, err)
	assert.Equal(t, 2, byRoot.TotalCount)
	assert.ElementsMatch(t, []string{child.ID, sibling.ID}, ids(byRoot.Items))

	// Filtering by mid returns only the child.
	byMid, err := s.ListAgents(ctx, store.AgentFilter{AncestorID: mid}, store.ListOptions{})
	require.NoError(t, err)
	assert.Equal(t, 1, byMid.TotalCount)
	require.Len(t, byMid.Items, 1)
	assert.Equal(t, child.ID, byMid.Items[0].ID)

	// An ancestor that matches nobody returns no rows.
	none, err := s.ListAgents(ctx, store.AgentFilter{AncestorID: "nobody"}, store.ListOptions{})
	require.NoError(t, err)
	assert.Equal(t, 0, none.TotalCount)
	assert.Empty(t, none.Items)
}

// TestAgentStore_SoftDeleteExclusion verifies soft-deleted agents are hidden
// from default listings but returned when explicitly included.
func TestAgentStore_SoftDeleteExclusion(t *testing.T) {
	ctx := context.Background()
	s, projectID := newTestAgentStore(t)

	live := makeAgent(projectID, "live")
	require.NoError(t, s.CreateAgent(ctx, live))

	gone := makeAgent(projectID, "gone")
	require.NoError(t, s.CreateAgent(ctx, gone))

	// Soft-delete via UpdateAgent setting DeletedAt.
	gone.DeletedAt = time.Now()
	require.NoError(t, s.UpdateAgent(ctx, gone))

	// Default listing excludes the soft-deleted agent.
	def, err := s.ListAgents(ctx, store.AgentFilter{}, store.ListOptions{})
	require.NoError(t, err)
	assert.Equal(t, 1, def.TotalCount)
	require.Len(t, def.Items, 1)
	assert.Equal(t, live.ID, def.Items[0].ID)

	// IncludeDeleted brings it back.
	incl, err := s.ListAgents(ctx, store.AgentFilter{IncludeDeleted: true}, store.ListOptions{})
	require.NoError(t, err)
	assert.Equal(t, 2, incl.TotalCount)
	assert.ElementsMatch(t, []string{live.ID, gone.ID}, ids(incl.Items))
}

// TestAgentStore_OptimisticLockConflict verifies the state_version CAS guard:
// a second update issued against a stale version is rejected with
// ErrVersionConflict rather than silently overwriting the first.
func TestAgentStore_OptimisticLockConflict(t *testing.T) {
	ctx := context.Background()
	s, projectID := newTestAgentStore(t)

	a := makeAgent(projectID, "locked")
	require.NoError(t, s.CreateAgent(ctx, a))

	// Two readers load the same version (1).
	readerA, err := s.GetAgent(ctx, a.ID)
	require.NoError(t, err)
	readerB, err := s.GetAgent(ctx, a.ID)
	require.NoError(t, err)
	require.Equal(t, int64(1), readerA.StateVersion)
	require.Equal(t, int64(1), readerB.StateVersion)

	// First writer wins and advances the version to 2.
	readerA.Name = "WriterA"
	require.NoError(t, s.UpdateAgent(ctx, readerA))
	assert.Equal(t, int64(2), readerA.StateVersion)

	// Second writer holds the now-stale version 1 and must conflict.
	readerB.Name = "WriterB"
	err = s.UpdateAgent(ctx, readerB)
	assert.ErrorIs(t, err, store.ErrVersionConflict)

	// The losing write left no trace.
	final, err := s.GetAgent(ctx, a.ID)
	require.NoError(t, err)
	assert.Equal(t, "WriterA", final.Name)
	assert.Equal(t, int64(2), final.StateVersion)

	// Updating a non-existent agent reports ErrNotFound, not a conflict.
	ghost := makeAgent(projectID, "ghost")
	ghost.StateVersion = 1
	assert.ErrorIs(t, s.UpdateAgent(ctx, ghost), store.ErrNotFound)
}

func TestAgentStore_UpdateAgentStatus(t *testing.T) {
	ctx := context.Background()
	s, projectID := newTestAgentStore(t)

	a := makeAgent(projectID, "status")
	a.Activity = "thinking"
	require.NoError(t, s.CreateAgent(ctx, a))

	// A normal status report updates activity, tool, and refreshes last_seen.
	require.NoError(t, s.UpdateAgentStatus(ctx, a.ID, store.AgentStatusUpdate{
		Activity: "executing",
		ToolName: "Bash",
	}))
	got, err := s.GetAgent(ctx, a.ID)
	require.NoError(t, err)
	assert.Equal(t, "executing", got.Activity)
	assert.Equal(t, "Bash", got.ToolName)
	assert.False(t, got.LastSeen.IsZero(), "last_seen should be refreshed")
	assert.False(t, got.LastActivityEvent.IsZero(), "last_activity_event should be set")

	// Drive the agent to a terminal sticky state.
	require.NoError(t, s.UpdateAgentStatus(ctx, a.ID, store.AgentStatusUpdate{
		Phase:    "stopped",
		Activity: "crashed",
	}))
	// A subsequent non-terminal report must NOT overwrite the sticky activity.
	require.NoError(t, s.UpdateAgentStatus(ctx, a.ID, store.AgentStatusUpdate{
		Activity: "thinking",
	}))
	got, err = s.GetAgent(ctx, a.ID)
	require.NoError(t, err)
	assert.Equal(t, "crashed", got.Activity, "terminal activity must stick")

	// Unknown agent reports ErrNotFound.
	assert.ErrorIs(t, s.UpdateAgentStatus(ctx, uuid.NewString(), store.AgentStatusUpdate{Phase: "running"}), store.ErrNotFound)
}

// TestAgentStore_TerminalPhaseClearsStalledActivity verifies that transitioning
// to a terminal phase (stopped/error) without an explicit activity clears a
// lingering live activity such as "stalled", while preserving terminal
// activities like "crashed".
func TestAgentStore_TerminalPhaseClearsStalledActivity(t *testing.T) {
	ctx := context.Background()

	t.Run("stalled cleared on stop", func(t *testing.T) {
		s, projectID := newTestAgentStore(t)
		a := makeAgent(projectID, "stalled-stop")
		a.Phase = "running"
		a.Activity = "stalled"
		require.NoError(t, s.CreateAgent(ctx, a))

		require.NoError(t, s.UpdateAgentStatus(ctx, a.ID, store.AgentStatusUpdate{Phase: "stopped"}))
		got, err := s.GetAgent(ctx, a.ID)
		require.NoError(t, err)
		assert.Equal(t, "stopped", got.Phase)
		assert.Equal(t, "", got.Activity, "stalled activity must be cleared on stop")
	})

	t.Run("stalled cleared on error", func(t *testing.T) {
		s, projectID := newTestAgentStore(t)
		a := makeAgent(projectID, "stalled-error")
		a.Phase = "running"
		a.Activity = "stalled"
		require.NoError(t, s.CreateAgent(ctx, a))

		require.NoError(t, s.UpdateAgentStatus(ctx, a.ID, store.AgentStatusUpdate{Phase: "error"}))
		got, err := s.GetAgent(ctx, a.ID)
		require.NoError(t, err)
		assert.Equal(t, "error", got.Phase)
		assert.Equal(t, "", got.Activity, "stalled activity must be cleared on error")
	})

	t.Run("terminal activity preserved when explicitly provided", func(t *testing.T) {
		s, projectID := newTestAgentStore(t)
		a := makeAgent(projectID, "crashed-keep")
		a.Phase = "running"
		a.Activity = "stalled"
		require.NoError(t, s.CreateAgent(ctx, a))

		require.NoError(t, s.UpdateAgentStatus(ctx, a.ID, store.AgentStatusUpdate{
			Phase:    "stopped",
			Activity: "crashed",
		}))
		got, err := s.GetAgent(ctx, a.ID)
		require.NoError(t, err)
		assert.Equal(t, "crashed", got.Activity, "explicit terminal activity must be kept")
	})
}

// TestAgentStore_RunningPhaseClearsStaleMessage verifies that a (re)start to the
// running phase clears a lingering terminal message (e.g. a crash message) and
// any leftover stalled marker, while an explicit message in the same update is
// preserved.
func TestAgentStore_RunningPhaseClearsStaleMessage(t *testing.T) {
	ctx := context.Background()

	t.Run("crash message cleared on restart", func(t *testing.T) {
		s, projectID := newTestAgentStore(t)
		a := makeAgent(projectID, "crash-clear")
		a.Phase = "error"
		a.Activity = "crashed"
		a.Message = "Agent crashed with exit code 1"
		a.StalledFromActivity = "working"
		require.NoError(t, s.CreateAgent(ctx, a))

		require.NoError(t, s.UpdateAgentStatus(ctx, a.ID, store.AgentStatusUpdate{
			Phase:    "running",
			Activity: "working",
		}))
		got, err := s.GetAgent(ctx, a.ID)
		require.NoError(t, err)
		assert.Equal(t, "running", got.Phase)
		assert.Equal(t, "", got.Message, "stale crash message must be cleared on restart")
		assert.Equal(t, "", got.StalledFromActivity, "stalled marker must be cleared on restart")
	})

	t.Run("explicit message preserved on restart", func(t *testing.T) {
		s, projectID := newTestAgentStore(t)
		a := makeAgent(projectID, "msg-keep")
		a.Phase = "error"
		a.Message = "Agent crashed with exit code 1"
		require.NoError(t, s.CreateAgent(ctx, a))

		require.NoError(t, s.UpdateAgentStatus(ctx, a.ID, store.AgentStatusUpdate{
			Phase:   "running",
			Message: "Restarting",
		}))
		got, err := s.GetAgent(ctx, a.ID)
		require.NoError(t, err)
		assert.Equal(t, "Restarting", got.Message, "explicit message must be kept on restart")
	})
}

func TestAgentStore_MarkStaleAgentsOffline(t *testing.T) {
	ctx := context.Background()
	s, projectID := newTestAgentStore(t)

	old := time.Now().Add(-1 * time.Hour)
	threshold := time.Now().Add(-30 * time.Minute)

	// Stale running agent with an old heartbeat -> should be marked offline.
	stale := makeAgent(projectID, "stale")
	stale.Phase = "running"
	stale.Activity = "thinking"
	stale.LastSeen = old
	require.NoError(t, s.CreateAgent(ctx, stale))

	// Recent agent -> untouched.
	fresh := makeAgent(projectID, "fresh")
	fresh.Phase = "running"
	fresh.Activity = "thinking"
	fresh.LastSeen = time.Now()
	require.NoError(t, s.CreateAgent(ctx, fresh))

	// Already-completed agent -> sticky, untouched.
	done := makeAgent(projectID, "done")
	done.Phase = "running"
	done.Activity = "completed"
	done.LastSeen = old
	require.NoError(t, s.CreateAgent(ctx, done))

	updated, err := s.MarkStaleAgentsOffline(ctx, threshold)
	require.NoError(t, err)
	require.Len(t, updated, 1)
	assert.Equal(t, stale.ID, updated[0].ID)
	assert.Equal(t, "offline", updated[0].Activity)

	gotFresh, _ := s.GetAgent(ctx, fresh.ID)
	assert.Equal(t, "thinking", gotFresh.Activity)
	gotDone, _ := s.GetAgent(ctx, done.ID)
	assert.Equal(t, "completed", gotDone.Activity)
}

func TestAgentStore_MarkStalledAgents(t *testing.T) {
	ctx := context.Background()
	s, projectID := newTestAgentStore(t)

	now := time.Now()
	activityThreshold := now.Add(-15 * time.Minute)
	heartbeatRecency := now.Add(-2 * time.Minute)

	// Recent heartbeat but stale activity -> stalled.
	stalled := makeAgent(projectID, "stalled")
	stalled.Phase = "running"
	stalled.Activity = "executing"
	stalled.LastActivityEvent = now.Add(-30 * time.Minute)
	stalled.LastSeen = now
	require.NoError(t, s.CreateAgent(ctx, stalled))

	// Active recently -> untouched.
	active := makeAgent(projectID, "active")
	active.Phase = "running"
	active.Activity = "executing"
	active.LastActivityEvent = now
	active.LastSeen = now
	require.NoError(t, s.CreateAgent(ctx, active))

	updated, err := s.MarkStalledAgents(ctx, activityThreshold, heartbeatRecency)
	require.NoError(t, err)
	require.Len(t, updated, 1)
	assert.Equal(t, stalled.ID, updated[0].ID)
	assert.Equal(t, "stalled", updated[0].Activity)
	assert.Equal(t, "executing", updated[0].StalledFromActivity, "prior activity should be preserved")

	gotActive, _ := s.GetAgent(ctx, active.ID)
	assert.Equal(t, "executing", gotActive.Activity)
}

func TestAgentStore_PurgeDeletedAgents(t *testing.T) {
	ctx := context.Background()
	s, projectID := newTestAgentStore(t)

	// Old soft-deleted agent -> purged.
	oldDeleted := makeAgent(projectID, "old-deleted")
	require.NoError(t, s.CreateAgent(ctx, oldDeleted))
	oldDeleted.DeletedAt = time.Now().Add(-48 * time.Hour)
	require.NoError(t, s.UpdateAgent(ctx, oldDeleted))

	// Recently soft-deleted agent -> retained.
	recentDeleted := makeAgent(projectID, "recent-deleted")
	require.NoError(t, s.CreateAgent(ctx, recentDeleted))
	recentDeleted.DeletedAt = time.Now().Add(-1 * time.Hour)
	require.NoError(t, s.UpdateAgent(ctx, recentDeleted))

	// Live agent -> retained.
	live := makeAgent(projectID, "live")
	require.NoError(t, s.CreateAgent(ctx, live))

	purged, err := s.PurgeDeletedAgents(ctx, time.Now().Add(-24*time.Hour))
	require.NoError(t, err)
	assert.Equal(t, 1, purged)

	_, err = s.GetAgent(ctx, oldDeleted.ID)
	assert.ErrorIs(t, err, store.ErrNotFound)
	_, err = s.GetAgent(ctx, recentDeleted.ID)
	assert.NoError(t, err)
}

// ids extracts the agent IDs from a slice for order-independent comparison.
func ids(agents []store.Agent) []string {
	out := make([]string, len(agents))
	for i := range agents {
		out[i] = agents[i].ID
	}
	return out
}
