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

// TestAgentStore_GetAgentBySlugExcludesSoftDeleted verifies that GetAgentBySlug
// does not return a soft-deleted agent, returning ErrNotFound instead.
func TestAgentStore_GetAgentBySlugExcludesSoftDeleted(t *testing.T) {
	ctx := context.Background()
	s, projectID := newTestAgentStore(t)

	a := makeAgent(projectID, "reusable-slug")
	require.NoError(t, s.CreateAgent(ctx, a))

	// Soft-delete the agent.
	a.DeletedAt = time.Now()
	require.NoError(t, s.UpdateAgent(ctx, a))

	// Slug lookup must not return the soft-deleted record.
	_, err := s.GetAgentBySlug(ctx, projectID, "reusable-slug")
	assert.ErrorIs(t, err, store.ErrNotFound)
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

func TestAgentStore_UpdateAgentExposedPorts(t *testing.T) {
	ctx := context.Background()
	s, projectID := newTestAgentStore(t)

	a := makeAgent(projectID, "ports")
	require.NoError(t, s.CreateAgent(ctx, a))

	exposedAt := time.Now().UTC().Truncate(time.Second)
	ports := []store.ExposedPort{{
		Port:      3000,
		Label:     "dev",
		Host:      "127.0.0.1",
		Mode:      "rw",
		ExposedAt: exposedAt,
		ExposedBy: "agent",
	}}
	require.NoError(t, s.UpdateAgentExposedPorts(ctx, a.ID, ports))

	got, err := s.GetAgent(ctx, a.ID)
	require.NoError(t, err)
	require.Len(t, got.ExposedPorts, 1)
	assert.Equal(t, ports[0].Port, got.ExposedPorts[0].Port)
	assert.Equal(t, ports[0].Label, got.ExposedPorts[0].Label)
	assert.Equal(t, ports[0].Host, got.ExposedPorts[0].Host)
	assert.Equal(t, ports[0].Mode, got.ExposedPorts[0].Mode)
	assert.Equal(t, ports[0].ExposedBy, got.ExposedPorts[0].ExposedBy)

	require.NoError(t, s.UpdateAgentExposedPorts(ctx, a.ID, nil))
	got, err = s.GetAgent(ctx, a.ID)
	require.NoError(t, err)
	assert.Empty(t, got.ExposedPorts)
	assert.ErrorIs(t, s.UpdateAgentExposedPorts(ctx, uuid.NewString(), ports), store.ErrNotFound)
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

func TestAgentStore_LabelFiltering(t *testing.T) {
	ctx := context.Background()
	s, projectID := newTestAgentStore(t)

	a1 := makeAgent(projectID, "label-1")
	a1.Labels = map[string]string{"env": "prod", "team": "platform"}
	require.NoError(t, s.CreateAgent(ctx, a1))

	a2 := makeAgent(projectID, "label-2")
	a2.Labels = map[string]string{"env": "staging", "team": "platform"}
	require.NoError(t, s.CreateAgent(ctx, a2))

	a3 := makeAgent(projectID, "label-3")
	a3.Labels = map[string]string{"env": "prod", "team": "data"}
	require.NoError(t, s.CreateAgent(ctx, a3))

	a4 := makeAgent(projectID, "label-4")
	a4.Labels = nil
	require.NoError(t, s.CreateAgent(ctx, a4))

	a5 := makeAgent(projectID, "label-5")
	a5.Labels = map[string]string{"scion.dev/role": "worker"}
	require.NoError(t, s.CreateAgent(ctx, a5))

	tests := []struct {
		name    string
		labels  map[string]string
		wantIDs []string
	}{
		{
			name:    "single label match",
			labels:  map[string]string{"env": "prod"},
			wantIDs: []string{a1.ID, a3.ID},
		},
		{
			name:    "multi-label AND",
			labels:  map[string]string{"env": "prod", "team": "platform"},
			wantIDs: []string{a1.ID},
		},
		{
			name:    "no match",
			labels:  map[string]string{"env": "dev"},
			wantIDs: nil,
		},
		{
			name:    "empty filter returns all",
			labels:  map[string]string{},
			wantIDs: []string{a1.ID, a2.ID, a3.ID, a4.ID, a5.ID},
		},
		{
			name:    "dotted key",
			labels:  map[string]string{"scion.dev/role": "worker"},
			wantIDs: []string{a5.ID},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result, err := s.ListAgents(ctx, store.AgentFilter{
				ProjectID: projectID,
				Labels:    tt.labels,
			}, store.ListOptions{})
			require.NoError(t, err)
			gotIDs := ids(result.Items)
			assert.ElementsMatch(t, tt.wantIDs, gotIDs)
		})
	}
}

// TestListAgents_CursorPagination verifies ListAgents honors ListOptions.Cursor
// and enumerates every agent across pages with no gaps or duplicates. Before the
// keyset-pagination fix the cursor was ignored, so a caller could only ever see
// the first page.
func TestListAgents_CursorPagination(t *testing.T) {
	ctx := context.Background()
	s, projectID := newTestAgentStore(t)

	const total = 125 // more than one page when using a small limit
	created := make(map[string]bool, total)
	for i := 0; i < total; i++ {
		a := makeAgent(projectID, fmt.Sprintf("cursor-%03d", i))
		require.NoError(t, s.CreateAgent(ctx, a))
		created[a.ID] = true
	}

	// Walk through pages using a limit of 25 to exercise cursor across 5 pages.
	const pageSize = 25
	seen := make(map[string]bool, total)
	cursor := ""
	for pages := 0; ; pages++ {
		require.LessOrEqual(t, pages, total, "pagination did not terminate")
		page, err := s.ListAgents(ctx, store.AgentFilter{}, store.ListOptions{Limit: pageSize, Cursor: cursor})
		require.NoError(t, err)
		for _, a := range page.Items {
			require.False(t, seen[a.ID], "duplicate agent across pages: %s", a.ID)
			seen[a.ID] = true
		}
		if page.NextCursor == "" {
			break
		}
		cursor = page.NextCursor
	}

	assert.Len(t, seen, total, "cursor pagination must enumerate every agent")
	for id := range created {
		assert.True(t, seen[id], "agent missing from pagination: %s", id)
	}
}

// TestListAgents_DefaultLimit verifies that ListAgents with Limit=0 returns up
// to 500 agents (the new default), not the old default of 50.
func TestListAgents_DefaultLimit(t *testing.T) {
	ctx := context.Background()
	s, projectID := newTestAgentStore(t)

	const total = 75 // more than the old default of 50, less than new default of 500
	for i := 0; i < total; i++ {
		a := makeAgent(projectID, fmt.Sprintf("default-%03d", i))
		require.NoError(t, s.CreateAgent(ctx, a))
	}

	// Limit=0 should use the default limit of 500, so all 75 agents are returned.
	result, err := s.ListAgents(ctx, store.AgentFilter{}, store.ListOptions{Limit: 0})
	require.NoError(t, err)
	assert.Equal(t, total, result.TotalCount)
	assert.Len(t, result.Items, total, "with the default limit of 500, all %d agents should be returned", total)
	assert.Empty(t, result.NextCursor, "no next page should exist when all agents fit in one page")
}

// TestListAgents_MaxLimit verifies that ListAgents with a Limit exceeding 500 is
// capped at the max of 500.
func TestListAgents_MaxLimit(t *testing.T) {
	ctx := context.Background()
	s, projectID := newTestAgentStore(t)

	// Create 510 agents — just over the max limit.
	const total = 510
	for i := 0; i < total; i++ {
		a := makeAgent(projectID, fmt.Sprintf("max-%03d", i))
		require.NoError(t, s.CreateAgent(ctx, a))
	}

	// Requesting a limit above the max (1000) should cap at 500.
	result, err := s.ListAgents(ctx, store.AgentFilter{}, store.ListOptions{Limit: 1000})
	require.NoError(t, err)
	assert.Equal(t, total, result.TotalCount, "TotalCount should reflect all agents")
	assert.Len(t, result.Items, 500, "Limit>500 must be capped at maxAgentListLimit=500")
	assert.NotEmpty(t, result.NextCursor, "more agents exist, so NextCursor must be set")
}

// ids extracts the agent IDs from a slice for order-independent comparison.
func ids(agents []store.Agent) []string {
	out := make([]string, len(agents))
	for i := range agents {
		out[i] = agents[i].ID
	}
	return out
}

// =============================================================================
// applied_config validation on read (P7.4)
// =============================================================================

// TestParseAppliedConfig covers the decode-and-sanitise rules directly. The two
// failure modes used to be silent in different ways: the unmarshal error was
// dropped by an `err == nil` guard, and an unusable metadata mode was passed
// through untouched.
func TestParseAppliedConfig(t *testing.T) {
	t.Run("valid config round-trips", func(t *testing.T) {
		cfg, err := parseAppliedConfig(`{"image":"img:1","gcpIdentity":{"metadataMode":"assign","serviceAccountEmail":"sa@x.iam.gserviceaccount.com"}}`)
		require.NoError(t, err)
		require.NotNil(t, cfg)
		assert.Equal(t, "img:1", cfg.Image)
		require.NotNil(t, cfg.GCPIdentity)
		assert.Equal(t, "assign", cfg.GCPIdentity.MetadataMode)
	})

	t.Run("all three modes are accepted", func(t *testing.T) {
		for _, mode := range []string{"assign", "block", "passthrough"} {
			cfg, err := parseAppliedConfig(`{"gcpIdentity":{"metadataMode":"` + mode + `"}}`)
			require.NoError(t, err, "mode %q should be valid", mode)
			require.NotNil(t, cfg.GCPIdentity, "mode %q should be retained", mode)
			assert.Equal(t, mode, cfg.GCPIdentity.MetadataMode)
		}
	})

	t.Run("absent gcpIdentity is not an error", func(t *testing.T) {
		// No GCP decision was made. That is different from a decision that
		// names no mode, and only the latter is corruption.
		cfg, err := parseAppliedConfig(`{"image":"img:1"}`)
		require.NoError(t, err)
		require.NotNil(t, cfg)
		assert.Nil(t, cfg.GCPIdentity)
	})

	t.Run("corrupt JSON returns an error and no config", func(t *testing.T) {
		cfg, err := parseAppliedConfig(`{"image": "img:1"`)
		require.Error(t, err, "a corrupt applied_config must not be silently discarded")
		assert.Nil(t, cfg)
		assert.Contains(t, err.Error(), "not valid JSON")
	})

	t.Run("not-JSON-at-all returns an error", func(t *testing.T) {
		cfg, err := parseAppliedConfig(`this is not json`)
		require.Error(t, err)
		assert.Nil(t, cfg)
	})

	t.Run("empty metadata mode drops the GCP identity but keeps the rest", func(t *testing.T) {
		// The case that motivates the check: a non-nil GCPIdentity asserting a
		// GCP decision was made while naming no decision. Strictly worse than
		// nil, because nothing downstream has a safe default to read from it.
		cfg, err := parseAppliedConfig(`{"image":"img:1","harnessConfig":"hc","gcpIdentity":{"metadataMode":""}}`)
		require.Error(t, err)
		require.NotNil(t, cfg, "unrelated config must survive a bad metadata mode")
		assert.Nil(t, cfg.GCPIdentity, "the unusable GCP identity must be dropped")
		assert.Equal(t, "img:1", cfg.Image)
		assert.Equal(t, "hc", cfg.HarnessConfig)
	})

	t.Run("unknown metadata mode drops the GCP identity", func(t *testing.T) {
		for _, mode := range []string{"blocked", "Block", "sandbox", " block"} {
			cfg, err := parseAppliedConfig(`{"image":"img:1","gcpIdentity":{"metadataMode":"` + mode + `"}}`)
			require.Error(t, err, "mode %q should be rejected", mode)
			require.NotNil(t, cfg)
			assert.Nil(t, cfg.GCPIdentity, "mode %q should have been dropped", mode)
			assert.Equal(t, "img:1", cfg.Image)
		}
	})
}

// TestAgentStore_AppliedConfigValidatedOnRead exercises the same rules through
// a real round-trip, so the sanitising is pinned where callers actually meet it
// rather than only in the helper.
func TestAgentStore_AppliedConfigValidatedOnRead(t *testing.T) {
	ctx := context.Background()

	t.Run("unusable metadata mode is dropped on read", func(t *testing.T) {
		s, projectID := newTestAgentStore(t)
		a := makeAgent(projectID, "bad-mode")
		a.AppliedConfig = &store.AgentAppliedConfig{
			Image:       "img:1",
			GCPIdentity: &store.GCPIdentityConfig{MetadataMode: ""},
		}
		require.NoError(t, s.CreateAgent(ctx, a))

		got, err := s.GetAgent(ctx, a.ID)
		require.NoError(t, err, "one bad field must not make the agent unreadable")
		require.NotNil(t, got.AppliedConfig)
		assert.Nil(t, got.AppliedConfig.GCPIdentity,
			"an unusable metadata mode must not reach callers; the agent falls back to the secure default")
		assert.Equal(t, "img:1", got.AppliedConfig.Image)
	})

	t.Run("valid config is untouched", func(t *testing.T) {
		s, projectID := newTestAgentStore(t)
		a := makeAgent(projectID, "good-mode")
		a.AppliedConfig = &store.AgentAppliedConfig{
			GCPIdentity: &store.GCPIdentityConfig{
				MetadataMode:        store.GCPMetadataModeAssign,
				ServiceAccountEmail: "sa@x.iam.gserviceaccount.com",
			},
		}
		require.NoError(t, s.CreateAgent(ctx, a))

		got, err := s.GetAgent(ctx, a.ID)
		require.NoError(t, err)
		require.NotNil(t, got.AppliedConfig.GCPIdentity)
		assert.Equal(t, store.GCPMetadataModeAssign, got.AppliedConfig.GCPIdentity.MetadataMode)
		assert.Equal(t, "sa@x.iam.gserviceaccount.com", got.AppliedConfig.GCPIdentity.ServiceAccountEmail)
	})

	t.Run("listing survives a corrupt row", func(t *testing.T) {
		// The reason entAgentToStore logs instead of returning: a corrupt row
		// must not take the whole listing down with it.
		s, projectID := newTestAgentStore(t)
		good := makeAgent(projectID, "good-row")
		require.NoError(t, s.CreateAgent(ctx, good))
		bad := makeAgent(projectID, "corrupt-row")
		require.NoError(t, s.CreateAgent(ctx, bad))

		// Write raw invalid JSON straight past the store's own marshalling.
		require.NoError(t, s.client.Agent.UpdateOneID(uuid.MustParse(bad.ID)).
			SetAppliedConfig(`{"image": "img:1"`).Exec(ctx))

		result, err := s.ListAgents(ctx, store.AgentFilter{}, store.ListOptions{Limit: 10})
		require.NoError(t, err, "a corrupt applied_config must not break listing")
		assert.Len(t, result.Items, 2)
		for _, item := range result.Items {
			if item.ID == bad.ID {
				assert.Nil(t, item.AppliedConfig, "the corrupt config must be dropped, not half-parsed")
			}
		}
	})
}

// TestAgentStore_ExitCodeExitReason_Persistence verifies that ExitCode and
// ExitReason survive a round-trip through UpdateAgentStatus and are readable
// via GetAgent.
func TestAgentStore_ExitCodeExitReason_Persistence(t *testing.T) {
	ctx := context.Background()

	t.Run("UpdateAgentStatus sets ExitCode and ExitReason", func(t *testing.T) {
		s, projectID := newTestAgentStore(t)
		a := makeAgent(projectID, "exit-fields")
		require.NoError(t, s.CreateAgent(ctx, a))

		ec := 137
		require.NoError(t, s.UpdateAgentStatus(ctx, a.ID, store.AgentStatusUpdate{
			Phase:      "error",
			Activity:   "crashed",
			ExitCode:   &ec,
			ExitReason: "crashed",
		}))

		got, err := s.GetAgent(ctx, a.ID)
		require.NoError(t, err)
		require.NotNil(t, got.ExitCode, "ExitCode should be set")
		assert.Equal(t, 137, *got.ExitCode)
		assert.Equal(t, "crashed", got.ExitReason)
	})

	t.Run("ExitCode zero is persisted (clean exit)", func(t *testing.T) {
		s, projectID := newTestAgentStore(t)
		a := makeAgent(projectID, "exit-zero")
		require.NoError(t, s.CreateAgent(ctx, a))

		ec := 0
		require.NoError(t, s.UpdateAgentStatus(ctx, a.ID, store.AgentStatusUpdate{
			Phase:    "stopped",
			ExitCode: &ec,
		}))

		got, err := s.GetAgent(ctx, a.ID)
		require.NoError(t, err)
		require.NotNil(t, got.ExitCode, "ExitCode 0 should be persisted, not dropped")
		assert.Equal(t, 0, *got.ExitCode)
	})

	t.Run("ExitCode nil leaves field unchanged", func(t *testing.T) {
		s, projectID := newTestAgentStore(t)
		a := makeAgent(projectID, "exit-nil")
		require.NoError(t, s.CreateAgent(ctx, a))

		// First set an exit code
		ec := 1
		require.NoError(t, s.UpdateAgentStatus(ctx, a.ID, store.AgentStatusUpdate{
			ExitCode: &ec,
		}))

		// Then update without ExitCode — should not clear
		require.NoError(t, s.UpdateAgentStatus(ctx, a.ID, store.AgentStatusUpdate{
			Activity: "working",
		}))

		got, err := s.GetAgent(ctx, a.ID)
		require.NoError(t, err)
		require.NotNil(t, got.ExitCode, "nil ExitCode in status update must not clear existing value")
		assert.Equal(t, 1, *got.ExitCode)
	})

	t.Run("ExitReason empty leaves field unchanged", func(t *testing.T) {
		s, projectID := newTestAgentStore(t)
		a := makeAgent(projectID, "reason-empty")
		require.NoError(t, s.CreateAgent(ctx, a))

		ec := 1
		require.NoError(t, s.UpdateAgentStatus(ctx, a.ID, store.AgentStatusUpdate{
			ExitCode:   &ec,
			ExitReason: "crashed",
		}))

		// Update with empty ExitReason — should not clear
		require.NoError(t, s.UpdateAgentStatus(ctx, a.ID, store.AgentStatusUpdate{
			Activity: "working",
		}))

		got, err := s.GetAgent(ctx, a.ID)
		require.NoError(t, err)
		assert.Equal(t, "crashed", got.ExitReason, "empty ExitReason in status update must not clear existing value")
	})

	t.Run("UpdateAgent round-trips ExitCode and ExitReason", func(t *testing.T) {
		s, projectID := newTestAgentStore(t)
		a := makeAgent(projectID, "exit-update")
		require.NoError(t, s.CreateAgent(ctx, a))

		ec := 42
		a.ExitCode = &ec
		a.ExitReason = "limits_exceeded"
		require.NoError(t, s.UpdateAgent(ctx, a))

		got, err := s.GetAgent(ctx, a.ID)
		require.NoError(t, err)
		require.NotNil(t, got.ExitCode)
		assert.Equal(t, 42, *got.ExitCode)
		assert.Equal(t, "limits_exceeded", got.ExitReason)
	})

	t.Run("restart clears ExitCode and ExitReason", func(t *testing.T) {
		s, projectID := newTestAgentStore(t)
		a := makeAgent(projectID, "exit-restart")
		a.Phase = "running"
		require.NoError(t, s.CreateAgent(ctx, a))

		// Set ExitCode=137 and ExitReason="crashed" via UpdateAgentStatus
		ec := 137
		require.NoError(t, s.UpdateAgentStatus(ctx, a.ID, store.AgentStatusUpdate{
			Phase:      "error",
			Activity:   "crashed",
			ExitCode:   &ec,
			ExitReason: "crashed",
		}))

		// Verify they were set
		got, err := s.GetAgent(ctx, a.ID)
		require.NoError(t, err)
		require.NotNil(t, got.ExitCode)
		assert.Equal(t, 137, *got.ExitCode)
		assert.Equal(t, "crashed", got.ExitReason)

		// Simulate restart: transition from error to running
		require.NoError(t, s.UpdateAgentStatus(ctx, a.ID, store.AgentStatusUpdate{
			Phase:    "running",
			Activity: "working",
		}))

		// Verify ExitCode is nil and ExitReason is "" after restart
		got, err = s.GetAgent(ctx, a.ID)
		require.NoError(t, err)
		assert.Nil(t, got.ExitCode, "ExitCode must be cleared on restart")
		assert.Equal(t, "", got.ExitReason, "ExitReason must be cleared on restart")
	})

	t.Run("UpdateAgent clears nil ExitCode", func(t *testing.T) {
		s, projectID := newTestAgentStore(t)
		a := makeAgent(projectID, "exit-clear")
		require.NoError(t, s.CreateAgent(ctx, a))

		// Set an exit code via UpdateAgent
		ec := 42
		a.ExitCode = &ec
		a.ExitReason = "crashed"
		require.NoError(t, s.UpdateAgent(ctx, a))

		got, err := s.GetAgent(ctx, a.ID)
		require.NoError(t, err)
		require.NotNil(t, got.ExitCode)
		assert.Equal(t, 42, *got.ExitCode)

		// Clear ExitCode by setting it to nil via UpdateAgent
		got.ExitCode = nil
		got.ExitReason = ""
		require.NoError(t, s.UpdateAgent(ctx, got))

		got2, err := s.GetAgent(ctx, a.ID)
		require.NoError(t, err)
		assert.Nil(t, got2.ExitCode, "nil ExitCode via UpdateAgent must clear the field")
		assert.Equal(t, "", got2.ExitReason, "empty ExitReason via UpdateAgent must clear the field")
	})
}
