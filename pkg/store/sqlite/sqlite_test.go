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

package sqlite

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func setupTestStore(t *testing.T) *SQLiteStore {
	t.Helper()
	s, err := New(":memory:")
	require.NoError(t, err)

	err = s.Migrate(context.Background())
	require.NoError(t, err)

	t.Cleanup(func() {
		s.Close()
	})

	return s
}

// ============================================================================
// Agent Tests
// ============================================================================

func TestAgentCRUD(t *testing.T) {
	s := setupTestStore(t)
	ctx := context.Background()

	// First create a grove for the agent
	grove := &store.Grove{
		ID:         api.NewUUID(),
		Name:       "Test Grove",
		Slug:       "test-grove",
		Visibility: store.VisibilityPrivate,
	}
	require.NoError(t, s.CreateGrove(ctx, grove))

	// Create agent
	agent := &store.Agent{
		ID:         api.NewUUID(),
		Slug:       "test-agent",
		Name:       "Test Agent",
		Template:   "claude",
		GroveID:    grove.ID,
		Phase:      string(state.PhaseCreated),
		Visibility: store.VisibilityPrivate,
		Labels:     map[string]string{"env": "test"},
	}

	err := s.CreateAgent(ctx, agent)
	require.NoError(t, err)
	assert.NotZero(t, agent.Created)
	assert.Equal(t, int64(1), agent.StateVersion)

	// Get agent
	retrieved, err := s.GetAgent(ctx, agent.ID)
	require.NoError(t, err)
	assert.Equal(t, agent.ID, retrieved.ID)
	assert.Equal(t, agent.Slug, retrieved.Slug)
	assert.Equal(t, agent.Name, retrieved.Name)
	assert.Equal(t, agent.Template, retrieved.Template)
	assert.Equal(t, "test", retrieved.Labels["env"])

	// Get by slug
	retrieved, err = s.GetAgentBySlug(ctx, grove.ID, "test-agent")
	require.NoError(t, err)
	assert.Equal(t, agent.ID, retrieved.ID)

	// Update agent
	retrieved.Name = "Updated Agent"
	retrieved.Phase = string(state.PhaseRunning)
	err = s.UpdateAgent(ctx, retrieved)
	require.NoError(t, err)
	assert.Equal(t, int64(2), retrieved.StateVersion)

	// Verify update
	retrieved, err = s.GetAgent(ctx, agent.ID)
	require.NoError(t, err)
	assert.Equal(t, "Updated Agent", retrieved.Name)
	assert.Equal(t, string(state.PhaseRunning), retrieved.Phase)

	// Test version conflict
	oldVersion := retrieved.StateVersion
	retrieved.StateVersion = 1 // Use old version
	err = s.UpdateAgent(ctx, retrieved)
	assert.ErrorIs(t, err, store.ErrVersionConflict)

	// Restore correct version for delete
	retrieved.StateVersion = oldVersion

	// Delete agent
	err = s.DeleteAgent(ctx, agent.ID)
	require.NoError(t, err)

	// Verify deleted
	_, err = s.GetAgent(ctx, agent.ID)
	assert.ErrorIs(t, err, store.ErrNotFound)
}

func TestAgentList(t *testing.T) {
	s := setupTestStore(t)
	ctx := context.Background()

	// Create grove
	grove := &store.Grove{
		ID:         api.NewUUID(),
		Name:       "Test Grove",
		Slug:       "test-grove",
		Visibility: store.VisibilityPrivate,
	}
	require.NoError(t, s.CreateGrove(ctx, grove))

	// Create multiple agents
	for i := 0; i < 5; i++ {
		agent := &store.Agent{
			ID:         api.NewUUID(),
			Slug:       api.Slugify("agent-" + string(rune('a'+i))),
			Name:       "Agent " + string(rune('A'+i)),
			Template:   "claude",
			GroveID:    grove.ID,
			Phase:      string(state.PhaseRunning),
			Visibility: store.VisibilityPrivate,
		}
		if i%2 == 0 {
			agent.Phase = string(state.PhaseStopped)
		}
		require.NoError(t, s.CreateAgent(ctx, agent))
	}

	// List all
	result, err := s.ListAgents(ctx, store.AgentFilter{}, store.ListOptions{})
	require.NoError(t, err)
	assert.Equal(t, 5, result.TotalCount)
	assert.Len(t, result.Items, 5)

	// List by status
	result, err = s.ListAgents(ctx, store.AgentFilter{Phase: string(state.PhaseRunning)}, store.ListOptions{})
	require.NoError(t, err)
	assert.Equal(t, 2, result.TotalCount)

	// List by grove
	result, err = s.ListAgents(ctx, store.AgentFilter{GroveID: grove.ID}, store.ListOptions{})
	require.NoError(t, err)
	assert.Equal(t, 5, result.TotalCount)

	// Test pagination
	result, err = s.ListAgents(ctx, store.AgentFilter{}, store.ListOptions{Limit: 2})
	require.NoError(t, err)
	assert.Len(t, result.Items, 2)
}

func TestAgentAncestry(t *testing.T) {
	s := setupTestStore(t)
	ctx := context.Background()

	grove := &store.Grove{
		ID: api.NewUUID(), Name: "Ancestry Grove", Slug: "ancestry-grove",
		Visibility: store.VisibilityPrivate,
	}
	require.NoError(t, s.CreateGrove(ctx, grove))

	userID := "user-root-123"

	// Agent A: created by user (ancestry = [userID])
	agentA := &store.Agent{
		ID: api.NewUUID(), Slug: "agent-a", Name: "Agent A",
		Template: "claude", GroveID: grove.ID,
		Phase: string(state.PhaseRunning), Visibility: store.VisibilityPrivate,
		CreatedBy: userID, OwnerID: userID,
		Ancestry: []string{userID},
	}
	require.NoError(t, s.CreateAgent(ctx, agentA))

	// Agent B: created by Agent A (ancestry = [userID, agentA.ID])
	agentB := &store.Agent{
		ID: api.NewUUID(), Slug: "agent-b", Name: "Agent B",
		Template: "claude", GroveID: grove.ID,
		Phase: string(state.PhaseRunning), Visibility: store.VisibilityPrivate,
		CreatedBy: agentA.ID, OwnerID: agentA.ID,
		Ancestry: []string{userID, agentA.ID},
	}
	require.NoError(t, s.CreateAgent(ctx, agentB))

	// Agent C: created by Agent B (ancestry = [userID, agentA.ID, agentB.ID])
	agentC := &store.Agent{
		ID: api.NewUUID(), Slug: "agent-c", Name: "Agent C",
		Template: "claude", GroveID: grove.ID,
		Phase: string(state.PhaseRunning), Visibility: store.VisibilityPrivate,
		CreatedBy: agentB.ID, OwnerID: agentB.ID,
		Ancestry: []string{userID, agentA.ID, agentB.ID},
	}
	require.NoError(t, s.CreateAgent(ctx, agentC))

	// Verify ancestry is persisted and retrieved correctly
	t.Run("GetAgent preserves ancestry", func(t *testing.T) {
		retrieved, err := s.GetAgent(ctx, agentC.ID)
		require.NoError(t, err)
		assert.Equal(t, []string{userID, agentA.ID, agentB.ID}, retrieved.Ancestry)
	})

	t.Run("GetAgentBySlug preserves ancestry", func(t *testing.T) {
		retrieved, err := s.GetAgentBySlug(ctx, grove.ID, "agent-b")
		require.NoError(t, err)
		assert.Equal(t, []string{userID, agentA.ID}, retrieved.Ancestry)
	})

	t.Run("ListAgents preserves ancestry", func(t *testing.T) {
		result, err := s.ListAgents(ctx, store.AgentFilter{GroveID: grove.ID}, store.ListOptions{})
		require.NoError(t, err)
		assert.Len(t, result.Items, 3)
		for _, agent := range result.Items {
			assert.NotEmpty(t, agent.Ancestry, "agent %s should have ancestry", agent.Slug)
		}
	})

	t.Run("AncestorID filter - user sees all descendants", func(t *testing.T) {
		result, err := s.ListAgents(ctx, store.AgentFilter{AncestorID: userID}, store.ListOptions{})
		require.NoError(t, err)
		assert.Equal(t, 3, result.TotalCount)
	})

	t.Run("AncestorID filter - agentA sees B and C", func(t *testing.T) {
		result, err := s.ListAgents(ctx, store.AgentFilter{AncestorID: agentA.ID}, store.ListOptions{})
		require.NoError(t, err)
		assert.Equal(t, 2, result.TotalCount)
	})

	t.Run("AncestorID filter - agentB sees only C", func(t *testing.T) {
		result, err := s.ListAgents(ctx, store.AgentFilter{AncestorID: agentB.ID}, store.ListOptions{})
		require.NoError(t, err)
		assert.Equal(t, 1, result.TotalCount)
		assert.Equal(t, agentC.ID, result.Items[0].ID)
	})

	t.Run("AncestorID filter - agentC sees none", func(t *testing.T) {
		result, err := s.ListAgents(ctx, store.AgentFilter{AncestorID: agentC.ID}, store.ListOptions{})
		require.NoError(t, err)
		assert.Equal(t, 0, result.TotalCount)
	})

	t.Run("AncestorID filter - unknown user sees none", func(t *testing.T) {
		result, err := s.ListAgents(ctx, store.AgentFilter{AncestorID: "unknown-user"}, store.ListOptions{})
		require.NoError(t, err)
		assert.Equal(t, 0, result.TotalCount)
	})

	t.Run("nil ancestry persists as empty", func(t *testing.T) {
		agentNoAnc := &store.Agent{
			ID: api.NewUUID(), Slug: "agent-no-anc", Name: "No Ancestry",
			Template: "claude", GroveID: grove.ID,
			Phase: string(state.PhaseRunning), Visibility: store.VisibilityPrivate,
		}
		require.NoError(t, s.CreateAgent(ctx, agentNoAnc))
		retrieved, err := s.GetAgent(ctx, agentNoAnc.ID)
		require.NoError(t, err)
		assert.Nil(t, retrieved.Ancestry)
	})

	t.Run("NULL ancestry column does not crash scan", func(t *testing.T) {
		// Create agent normally, then set ancestry to NULL to simulate pre-migration state
		agentNullAnc := &store.Agent{
			ID: api.NewUUID(), Slug: "agent-null-anc", Name: "Null Ancestry",
			Template: "claude", GroveID: grove.ID,
			Phase: string(state.PhaseRunning), Visibility: store.VisibilityPrivate,
			Ancestry: []string{"some-user"},
		}
		require.NoError(t, s.CreateAgent(ctx, agentNullAnc))
		_, err := s.db.ExecContext(ctx, `UPDATE agents SET ancestry = NULL WHERE id = ?`, agentNullAnc.ID)
		require.NoError(t, err)
		agentID := agentNullAnc.ID

		retrieved, err := s.GetAgent(ctx, agentID)
		require.NoError(t, err)
		assert.Nil(t, retrieved.Ancestry)

		retrievedBySlug, err := s.GetAgentBySlug(ctx, grove.ID, "agent-null-anc")
		require.NoError(t, err)
		assert.Nil(t, retrievedBySlug.Ancestry)

		result, err := s.ListAgents(ctx, store.AgentFilter{GroveID: grove.ID}, store.ListOptions{})
		require.NoError(t, err)
		assert.True(t, result.TotalCount > 0)
	})
}

func TestAgentStatusUpdate(t *testing.T) {
	s := setupTestStore(t)
	ctx := context.Background()

	// Create grove and agent
	grove := &store.Grove{
		ID:         api.NewUUID(),
		Name:       "Test Grove",
		Slug:       "test-grove",
		Visibility: store.VisibilityPrivate,
	}
	require.NoError(t, s.CreateGrove(ctx, grove))

	agent := &store.Agent{
		ID:         api.NewUUID(),
		Slug:       "test-agent",
		Name:       "Test Agent",
		Template:   "claude",
		GroveID:    grove.ID,
		Phase:      string(state.PhaseCreated),
		Visibility: store.VisibilityPrivate,
	}
	require.NoError(t, s.CreateAgent(ctx, agent))

	// Legacy path: update flat status only (backward compat)
	err := s.UpdateAgentStatus(ctx, agent.ID, store.AgentStatusUpdate{
		Phase:           string(state.PhaseRunning),
		ContainerStatus: "Up 5 minutes",
	})
	require.NoError(t, err)

	retrieved, err := s.GetAgent(ctx, agent.ID)
	require.NoError(t, err)
	assert.Equal(t, string(state.PhaseRunning), retrieved.Phase)
	assert.Equal(t, "Up 5 minutes", retrieved.ContainerStatus)

	// Structured path: set phase + activity
	err = s.UpdateAgentStatus(ctx, agent.ID, store.AgentStatusUpdate{
		Phase:    "running",
		Activity: "thinking",
	})
	require.NoError(t, err)

	retrieved, err = s.GetAgent(ctx, agent.ID)
	require.NoError(t, err)
	assert.Equal(t, "running", retrieved.Phase)
	assert.Equal(t, "thinking", retrieved.Activity)

	// Set activity=executing with toolName
	err = s.UpdateAgentStatus(ctx, agent.ID, store.AgentStatusUpdate{
		Phase:    "running",
		Activity: "executing",
		ToolName: "Bash",
	})
	require.NoError(t, err)

	retrieved, err = s.GetAgent(ctx, agent.ID)
	require.NoError(t, err)
	assert.Equal(t, "executing", retrieved.Activity)
	assert.Equal(t, "Bash", retrieved.ToolName)

	// Change activity from executing to idle → toolName is cleared
	err = s.UpdateAgentStatus(ctx, agent.ID, store.AgentStatusUpdate{
		Phase:    "running",
		Activity: "idle",
	})
	require.NoError(t, err)

	retrieved, err = s.GetAgent(ctx, agent.ID)
	require.NoError(t, err)
	assert.Equal(t, "idle", retrieved.Activity)
	assert.Equal(t, "", retrieved.ToolName, "toolName should be cleared when activity changes away from executing")

	// Set only activity (phase preserved from previous update)
	err = s.UpdateAgentStatus(ctx, agent.ID, store.AgentStatusUpdate{
		Activity: "waiting_for_input",
	})
	require.NoError(t, err)

	retrieved, err = s.GetAgent(ctx, agent.ID)
	require.NoError(t, err)
	assert.Equal(t, "running", retrieved.Phase, "phase should be preserved")
	assert.Equal(t, "waiting_for_input", retrieved.Activity)

	// Non-running phase
	err = s.UpdateAgentStatus(ctx, agent.ID, store.AgentStatusUpdate{
		Phase: "stopped",
	})
	require.NoError(t, err)

	retrieved, err = s.GetAgent(ctx, agent.ID)
	require.NoError(t, err)
	assert.Equal(t, "stopped", retrieved.Phase)
}

func TestAgentStatusUpdate_PhaseActivityRoundTrip(t *testing.T) {
	s := setupTestStore(t)
	ctx := context.Background()

	grove := &store.Grove{
		ID:         api.NewUUID(),
		Name:       "Test Grove",
		Slug:       "test-grove-rt",
		Visibility: store.VisibilityPrivate,
	}
	require.NoError(t, s.CreateGrove(ctx, grove))

	// Create agent with initial phase/activity
	agent := &store.Agent{
		ID:         api.NewUUID(),
		Slug:       "roundtrip-agent",
		Name:       "Roundtrip Agent",
		Template:   "claude",
		GroveID:    grove.ID,
		Phase:      "running",
		Activity:   "idle",
		Visibility: store.VisibilityPrivate,
	}
	require.NoError(t, s.CreateAgent(ctx, agent))

	// Verify round-trip through Get
	retrieved, err := s.GetAgent(ctx, agent.ID)
	require.NoError(t, err)
	assert.Equal(t, "running", retrieved.Phase)
	assert.Equal(t, "idle", retrieved.Activity)

	// Verify round-trip through GetBySlug
	retrieved, err = s.GetAgentBySlug(ctx, grove.ID, "roundtrip-agent")
	require.NoError(t, err)
	assert.Equal(t, "running", retrieved.Phase)
	assert.Equal(t, "idle", retrieved.Activity)

	// Verify round-trip through List
	result, err := s.ListAgents(ctx, store.AgentFilter{GroveID: grove.ID}, store.ListOptions{})
	require.NoError(t, err)
	require.Len(t, result.Items, 1)
	assert.Equal(t, "running", result.Items[0].Phase)
	assert.Equal(t, "idle", result.Items[0].Activity)
}

func TestSoftDeleteFilterExclusion(t *testing.T) {
	s := setupTestStore(t)
	ctx := context.Background()

	// Create grove
	grove := &store.Grove{
		ID:         api.NewUUID(),
		Name:       "Test Grove",
		Slug:       "test-grove-sd",
		Visibility: store.VisibilityPrivate,
	}
	require.NoError(t, s.CreateGrove(ctx, grove))

	// Create 3 agents: 2 running, 1 soft-deleted
	for i := 0; i < 3; i++ {
		agent := &store.Agent{
			ID:         api.NewUUID(),
			Slug:       api.Slugify("sd-agent-" + string(rune('a'+i))),
			Name:       "SD Agent " + string(rune('A'+i)),
			Template:   "claude",
			GroveID:    grove.ID,
			Phase:      string(state.PhaseRunning),
			Visibility: store.VisibilityPrivate,
		}
		if i == 2 {
			agent.DeletedAt = time.Now()
		}
		require.NoError(t, s.CreateAgent(ctx, agent))
	}

	// List without IncludeDeleted: should see 2
	result, err := s.ListAgents(ctx, store.AgentFilter{}, store.ListOptions{})
	require.NoError(t, err)
	assert.Equal(t, 2, result.TotalCount)
	assert.Len(t, result.Items, 2)
	for _, a := range result.Items {
		assert.True(t, a.DeletedAt.IsZero(), "non-deleted agent should have zero DeletedAt")
	}

	// List with IncludeDeleted: should see 3
	result, err = s.ListAgents(ctx, store.AgentFilter{IncludeDeleted: true}, store.ListOptions{})
	require.NoError(t, err)
	assert.Equal(t, 3, result.TotalCount)
	assert.Len(t, result.Items, 3)

	// List with IncludeDeleted: should see all 3 (including the deleted one)
	// Verify we can find the soft-deleted agent
	var deletedCount int
	for _, a := range result.Items {
		if !a.DeletedAt.IsZero() {
			deletedCount++
		}
	}
	assert.Equal(t, 1, deletedCount, "should have exactly one soft-deleted agent")
}

func TestPurgeDeletedAgents(t *testing.T) {
	s := setupTestStore(t)
	ctx := context.Background()

	grove := &store.Grove{
		ID:         api.NewUUID(),
		Name:       "Test Grove",
		Slug:       "test-grove-purge",
		Visibility: store.VisibilityPrivate,
	}
	require.NoError(t, s.CreateGrove(ctx, grove))

	now := time.Now()

	// Create 2 deleted agents: one expired (old), one recent
	oldAgent := &store.Agent{
		ID:         api.NewUUID(),
		Slug:       "old-deleted",
		Name:       "Old Deleted",
		Template:   "claude",
		GroveID:    grove.ID,
		Phase:      string(state.PhaseStopped),
		DeletedAt:  now.Add(-48 * time.Hour),
		Visibility: store.VisibilityPrivate,
	}
	recentAgent := &store.Agent{
		ID:         api.NewUUID(),
		Slug:       "recent-deleted",
		Name:       "Recent Deleted",
		Template:   "claude",
		GroveID:    grove.ID,
		Phase:      string(state.PhaseStopped),
		DeletedAt:  now.Add(-1 * time.Hour),
		Visibility: store.VisibilityPrivate,
	}
	activeAgent := &store.Agent{
		ID:         api.NewUUID(),
		Slug:       "active-agent",
		Name:       "Active Agent",
		Template:   "claude",
		GroveID:    grove.ID,
		Phase:      string(state.PhaseRunning),
		Visibility: store.VisibilityPrivate,
	}
	require.NoError(t, s.CreateAgent(ctx, oldAgent))
	require.NoError(t, s.CreateAgent(ctx, recentAgent))
	require.NoError(t, s.CreateAgent(ctx, activeAgent))

	// Purge with cutoff of 24h ago: should only purge the old one
	cutoff := now.Add(-24 * time.Hour)
	purged, err := s.PurgeDeletedAgents(ctx, cutoff)
	require.NoError(t, err)
	assert.Equal(t, 1, purged)

	// Old agent should be gone
	_, err = s.GetAgent(ctx, oldAgent.ID)
	assert.ErrorIs(t, err, store.ErrNotFound)

	// Recent deleted agent should still exist
	_, err = s.GetAgent(ctx, recentAgent.ID)
	require.NoError(t, err)

	// Active agent should still exist
	_, err = s.GetAgent(ctx, activeAgent.ID)
	require.NoError(t, err)
}

func TestDeletedAtPersistence(t *testing.T) {
	s := setupTestStore(t)
	ctx := context.Background()

	grove := &store.Grove{
		ID:         api.NewUUID(),
		Name:       "Test Grove",
		Slug:       "test-grove-dat",
		Visibility: store.VisibilityPrivate,
	}
	require.NoError(t, s.CreateGrove(ctx, grove))

	// Create and soft-delete an agent
	agent := &store.Agent{
		ID:         api.NewUUID(),
		Slug:       "soft-del-test",
		Name:       "Soft Delete Test",
		Template:   "claude",
		GroveID:    grove.ID,
		Phase:      string(state.PhaseRunning),
		Visibility: store.VisibilityPrivate,
	}
	require.NoError(t, s.CreateAgent(ctx, agent))

	// Verify DeletedAt is zero initially
	retrieved, err := s.GetAgent(ctx, agent.ID)
	require.NoError(t, err)
	assert.True(t, retrieved.DeletedAt.IsZero())

	// Soft-delete
	deletedAt := time.Now().Truncate(time.Second)
	retrieved.DeletedAt = deletedAt
	retrieved.Updated = time.Now()
	require.NoError(t, s.UpdateAgent(ctx, retrieved))

	// Retrieve and verify DeletedAt is set
	retrieved2, err := s.GetAgent(ctx, agent.ID)
	require.NoError(t, err)
	assert.False(t, retrieved2.DeletedAt.IsZero(), "soft-deleted agent should have non-zero DeletedAt")
	assert.WithinDuration(t, deletedAt, retrieved2.DeletedAt, time.Second)

	// Verify GetAgentBySlug also returns DeletedAt
	bySlug, err := s.GetAgentBySlug(ctx, grove.ID, "soft-del-test")
	require.NoError(t, err)
	assert.False(t, bySlug.DeletedAt.IsZero(), "soft-deleted agent fetched by slug should have non-zero DeletedAt")

	// Verify restore clears DeletedAt
	bySlug.DeletedAt = time.Time{}
	bySlug.Updated = time.Now()
	require.NoError(t, s.UpdateAgent(ctx, bySlug))

	restored, err := s.GetAgent(ctx, agent.ID)
	require.NoError(t, err)
	assert.True(t, restored.DeletedAt.IsZero(), "restored agent should have zero DeletedAt")
}

// ============================================================================
// Grove Tests
// ============================================================================

func TestGroveCRUD(t *testing.T) {
	s := setupTestStore(t)
	ctx := context.Background()

	// Create grove
	grove := &store.Grove{
		ID:         api.NewUUID(),
		Name:       "My Project",
		Slug:       "my-project",
		GitRemote:  "github.com/org/repo",
		Visibility: store.VisibilityPrivate,
		Labels:     map[string]string{"team": "platform"},
	}

	err := s.CreateGrove(ctx, grove)
	require.NoError(t, err)
	assert.NotZero(t, grove.Created)

	// Get grove
	retrieved, err := s.GetGrove(ctx, grove.ID)
	require.NoError(t, err)
	assert.Equal(t, grove.Name, retrieved.Name)
	assert.Equal(t, grove.GitRemote, retrieved.GitRemote)
	assert.Equal(t, "platform", retrieved.Labels["team"])

	// Get by slug
	retrieved, err = s.GetGroveBySlug(ctx, "my-project")
	require.NoError(t, err)
	assert.Equal(t, grove.ID, retrieved.ID)

	// Get by git remote (plural)
	groves, err := s.GetGrovesByGitRemote(ctx, "github.com/org/repo")
	require.NoError(t, err)
	require.Len(t, groves, 1)
	assert.Equal(t, grove.ID, groves[0].ID)

	// Duplicate git remotes are now allowed (slug must still be unique)
	duplicate := &store.Grove{
		ID:         api.NewUUID(),
		Name:       "Duplicate",
		Slug:       "duplicate",
		GitRemote:  "github.com/org/repo",
		Visibility: store.VisibilityPrivate,
	}
	err = s.CreateGrove(ctx, duplicate)
	require.NoError(t, err)

	// Verify both groves are returned
	groves, err = s.GetGrovesByGitRemote(ctx, "github.com/org/repo")
	require.NoError(t, err)
	assert.Len(t, groves, 2)

	// Update grove
	retrieved.Name = "Updated Project"
	err = s.UpdateGrove(ctx, retrieved)
	require.NoError(t, err)

	// Verify update
	retrieved, err = s.GetGrove(ctx, grove.ID)
	require.NoError(t, err)
	assert.Equal(t, "Updated Project", retrieved.Name)

	// Delete grove
	err = s.DeleteGrove(ctx, grove.ID)
	require.NoError(t, err)

	// Verify deleted
	_, err = s.GetGrove(ctx, grove.ID)
	assert.ErrorIs(t, err, store.ErrNotFound)
}

func TestMultiGrovePerGitRemote(t *testing.T) {
	s := setupTestStore(t)
	ctx := context.Background()

	remote := "github.com/acme/widgets"

	// Create 3 groves with the same git remote but different slugs
	slugs := []string{"acme-widgets", "acme-widgets-1", "acme-widgets-2"}
	for i, slug := range slugs {
		grove := &store.Grove{
			ID:         api.NewUUID(),
			Name:       fmt.Sprintf("acme-widgets grove %d", i),
			Slug:       slug,
			GitRemote:  remote,
			Visibility: store.VisibilityPrivate,
		}
		require.NoError(t, s.CreateGrove(ctx, grove))
	}

	groves, err := s.GetGrovesByGitRemote(ctx, remote)
	require.NoError(t, err)
	assert.Len(t, groves, 3)

	// Verify ordering is by created_at ASC
	assert.Equal(t, "acme-widgets", groves[0].Slug)
	assert.Equal(t, "acme-widgets-1", groves[1].Slug)
	assert.Equal(t, "acme-widgets-2", groves[2].Slug)
}

func TestGetGrovesByGitRemoteEmpty(t *testing.T) {
	s := setupTestStore(t)
	ctx := context.Background()

	groves, err := s.GetGrovesByGitRemote(ctx, "github.com/nonexistent/repo")
	require.NoError(t, err)
	assert.Empty(t, groves)
}

func TestSlugUniqueness(t *testing.T) {
	s := setupTestStore(t)
	ctx := context.Background()

	grove1 := &store.Grove{
		ID: api.NewUUID(), Name: "Test", Slug: "test-grove",
		Visibility: store.VisibilityPrivate,
	}
	require.NoError(t, s.CreateGrove(ctx, grove1))

	// Duplicate slug should fail
	grove2 := &store.Grove{
		ID: api.NewUUID(), Name: "Test 2", Slug: "test-grove",
		Visibility: store.VisibilityPrivate,
	}
	err := s.CreateGrove(ctx, grove2)
	assert.ErrorIs(t, err, store.ErrAlreadyExists)
}

func TestNextAvailableSlug(t *testing.T) {
	s := setupTestStore(t)
	ctx := context.Background()

	// Base slug available
	slug, err := s.NextAvailableSlug(ctx, "acme-widgets")
	require.NoError(t, err)
	assert.Equal(t, "acme-widgets", slug)

	// Create the base slug
	require.NoError(t, s.CreateGrove(ctx, &store.Grove{
		ID: api.NewUUID(), Name: "acme-widgets", Slug: "acme-widgets",
		Visibility: store.VisibilityPrivate,
	}))

	// Should get -1
	slug, err = s.NextAvailableSlug(ctx, "acme-widgets")
	require.NoError(t, err)
	assert.Equal(t, "acme-widgets-1", slug)

	// Create -1
	require.NoError(t, s.CreateGrove(ctx, &store.Grove{
		ID: api.NewUUID(), Name: "acme-widgets (1)", Slug: "acme-widgets-1",
		Visibility: store.VisibilityPrivate,
	}))

	// Should get -2
	slug, err = s.NextAvailableSlug(ctx, "acme-widgets")
	require.NoError(t, err)
	assert.Equal(t, "acme-widgets-2", slug)
}

func TestGetInstallationForRepository(t *testing.T) {
	s := setupTestStore(t)
	ctx := context.Background()

	// Create an installation with repos
	inst := &store.GitHubInstallation{
		InstallationID: 12345,
		AccountLogin:   "acme",
		AccountType:    "Organization",
		AppID:          100,
		Repositories:   []string{"acme/widgets", "acme/gizmos"},
		Status:         store.GitHubInstallationStatusActive,
	}
	require.NoError(t, s.CreateGitHubInstallation(ctx, inst))

	// Look up by repo
	found, err := s.GetInstallationForRepository(ctx, "acme/widgets")
	require.NoError(t, err)
	assert.Equal(t, int64(12345), found.InstallationID)
	assert.Contains(t, found.Repositories, "acme/widgets")

	// Look up non-existent repo
	_, err = s.GetInstallationForRepository(ctx, "acme/nonexistent")
	assert.ErrorIs(t, err, store.ErrNotFound)

	// Suspended installation should not match
	inst2 := &store.GitHubInstallation{
		InstallationID: 67890,
		AccountLogin:   "other",
		AccountType:    "User",
		AppID:          100,
		Repositories:   []string{"other/project"},
		Status:         store.GitHubInstallationStatusSuspended,
	}
	require.NoError(t, s.CreateGitHubInstallation(ctx, inst2))

	_, err = s.GetInstallationForRepository(ctx, "other/project")
	assert.ErrorIs(t, err, store.ErrNotFound)
}

func TestDisplayNameWithSerial(t *testing.T) {
	assert.Equal(t, "acme-widgets", api.DisplayNameWithSerial("acme-widgets", "acme-widgets", "acme-widgets"))
	assert.Equal(t, "acme-widgets (1)", api.DisplayNameWithSerial("acme-widgets", "acme-widgets-1", "acme-widgets"))
	assert.Equal(t, "acme-widgets (2)", api.DisplayNameWithSerial("acme-widgets", "acme-widgets-2", "acme-widgets"))
	assert.Equal(t, "My Project (3)", api.DisplayNameWithSerial("My Project", "my-project-3", "my-project"))
}

func TestGroveList(t *testing.T) {
	s := setupTestStore(t)
	ctx := context.Background()

	// Create a broker for ActiveBrokerCount
	broker := &store.RuntimeBroker{
		ID:     api.NewUUID(),
		Name:   "Test Broker",
		Slug:   "test-broker",
		Status: store.BrokerStatusOnline,
	}
	require.NoError(t, s.CreateRuntimeBroker(ctx, broker))

	// Create groves
	for i := 0; i < 3; i++ {
		grove := &store.Grove{
			ID:         api.NewUUID(),
			Name:       "Grove " + string(rune('A'+i)),
			Slug:       "grove-" + string(rune('a'+i)),
			Visibility: store.VisibilityPrivate,
		}
		if i == 0 {
			grove.Visibility = store.VisibilityPublic
		}
		require.NoError(t, s.CreateGrove(ctx, grove))

		// Add an agent to the first grove
		if i == 0 {
			agent := &store.Agent{
				ID:      api.NewUUID(),
				Slug:    "test-agent",
				Name:    "Test Agent",
				GroveID: grove.ID,
				Phase:   string(state.PhaseRunning),
			}
			require.NoError(t, s.CreateAgent(ctx, agent))

			// Link the broker to the first grove
			require.NoError(t, s.AddGroveProvider(ctx, &store.GroveProvider{
				GroveID:    grove.ID,
				BrokerID:   broker.ID,
				BrokerName: broker.Name,
				Status:     store.BrokerStatusOnline,
			}))
		}
	}

	// List all
	result, err := s.ListGroves(ctx, store.GroveFilter{}, store.ListOptions{})
	require.NoError(t, err)
	assert.Equal(t, 3, result.TotalCount)

	// Verify computed fields on the first grove (index 2 due to DESC sort by created_at)
	var firstGrove store.Grove
	for _, g := range result.Items {
		if g.Name == "Grove A" {
			firstGrove = g
			break
		}
	}
	assert.Equal(t, 1, firstGrove.AgentCount)
	assert.Equal(t, 1, firstGrove.ActiveBrokerCount)

	// List by visibility
	result, err = s.ListGroves(ctx, store.GroveFilter{Visibility: store.VisibilityPublic}, store.ListOptions{})
	require.NoError(t, err)
	assert.Equal(t, 1, result.TotalCount)
	assert.Equal(t, "Grove A", result.Items[0].Name)
}

// ============================================================================
// RuntimeBroker Tests
// ============================================================================

func TestGroveLookupCaseInsensitive(t *testing.T) {
	s := setupTestStore(t)
	ctx := context.Background()

	// Create a grove with mixed case name
	grove := &store.Grove{
		ID:         api.NewUUID(),
		Name:       "Global",
		Slug:       "global",
		Visibility: store.VisibilityPrivate,
	}
	require.NoError(t, s.CreateGrove(ctx, grove))

	// Look up with exact case - should work
	retrieved, err := s.GetGroveBySlugCaseInsensitive(ctx, "global")
	require.NoError(t, err)
	assert.Equal(t, grove.ID, retrieved.ID)

	// Look up with different case - should still work
	retrieved, err = s.GetGroveBySlugCaseInsensitive(ctx, "GLOBAL")
	require.NoError(t, err)
	assert.Equal(t, grove.ID, retrieved.ID)

	// Look up with mixed case - should still work
	retrieved, err = s.GetGroveBySlugCaseInsensitive(ctx, "Global")
	require.NoError(t, err)
	assert.Equal(t, grove.ID, retrieved.ID)

	// Look up non-existent - should return ErrNotFound
	_, err = s.GetGroveBySlugCaseInsensitive(ctx, "nonexistent")
	assert.ErrorIs(t, err, store.ErrNotFound)
}

func TestGroveListBySlug(t *testing.T) {
	s := setupTestStore(t)
	ctx := context.Background()

	// Create two groves with distinct slugs
	grove1 := &store.Grove{
		ID:         api.NewUUID(),
		Name:       "Alpha Project",
		Slug:       "alpha-project",
		Visibility: store.VisibilityPrivate,
	}
	grove2 := &store.Grove{
		ID:         api.NewUUID(),
		Name:       "Beta Project",
		Slug:       "beta-project",
		Visibility: store.VisibilityPrivate,
	}
	require.NoError(t, s.CreateGrove(ctx, grove1))
	require.NoError(t, s.CreateGrove(ctx, grove2))

	// Filter by slug — exact match
	result, err := s.ListGroves(ctx, store.GroveFilter{Slug: "alpha-project"}, store.ListOptions{})
	require.NoError(t, err)
	assert.Equal(t, 1, result.TotalCount)
	assert.Equal(t, grove1.ID, result.Items[0].ID)

	// Filter by slug — case-insensitive
	result, err = s.ListGroves(ctx, store.GroveFilter{Slug: "ALPHA-PROJECT"}, store.ListOptions{})
	require.NoError(t, err)
	assert.Equal(t, 1, result.TotalCount)
	assert.Equal(t, grove1.ID, result.Items[0].ID)

	// Filter by slug — no match
	result, err = s.ListGroves(ctx, store.GroveFilter{Slug: "nonexistent"}, store.ListOptions{})
	require.NoError(t, err)
	assert.Equal(t, 0, result.TotalCount)
}

func TestListGrovesByGitRemoteExactMatch(t *testing.T) {
	s := setupTestStore(t)
	ctx := context.Background()

	grove1 := &store.Grove{
		ID:         api.NewUUID(),
		Name:       "Repo",
		Slug:       "repo",
		GitRemote:  "github.com/org/repo",
		Visibility: store.VisibilityPrivate,
	}
	grove2 := &store.Grove{
		ID:         api.NewUUID(),
		Name:       "Repo Clone",
		Slug:       "repo-clone",
		GitRemote:  "github.com/org/repo-clone",
		Visibility: store.VisibilityPrivate,
	}
	require.NoError(t, s.CreateGrove(ctx, grove1))
	require.NoError(t, s.CreateGrove(ctx, grove2))

	// Exact match should return only the exact grove, not the one with repo-clone
	result, err := s.ListGroves(ctx, store.GroveFilter{GitRemote: "github.com/org/repo"}, store.ListOptions{})
	require.NoError(t, err)
	assert.Equal(t, 1, result.TotalCount)
	assert.Equal(t, grove1.ID, result.Items[0].ID)

	// Exact match on the clone URL should return only that grove
	result, err = s.ListGroves(ctx, store.GroveFilter{GitRemote: "github.com/org/repo-clone"}, store.ListOptions{})
	require.NoError(t, err)
	assert.Equal(t, 1, result.TotalCount)
	assert.Equal(t, grove2.ID, result.Items[0].ID)
}

func TestListGrovesSharedScope(t *testing.T) {
	s := setupTestStore(t)
	ctx := context.Background()

	ownerID := api.NewUUID()
	otherOwnerID := api.NewUUID()

	ownedGrove := &store.Grove{
		ID:         api.NewUUID(),
		Name:       "Owned Grove",
		Slug:       "owned-grove",
		OwnerID:    ownerID,
		Visibility: store.VisibilityPrivate,
	}
	sharedGrove := &store.Grove{
		ID:         api.NewUUID(),
		Name:       "Shared Grove",
		Slug:       "shared-grove",
		OwnerID:    otherOwnerID,
		Visibility: store.VisibilityPrivate,
	}
	unrelatedGrove := &store.Grove{
		ID:         api.NewUUID(),
		Name:       "Unrelated Grove",
		Slug:       "unrelated-grove",
		OwnerID:    otherOwnerID,
		Visibility: store.VisibilityPrivate,
	}
	require.NoError(t, s.CreateGrove(ctx, ownedGrove))
	require.NoError(t, s.CreateGrove(ctx, sharedGrove))
	require.NoError(t, s.CreateGrove(ctx, unrelatedGrove))

	// scope=mine: only groves owned by the user
	result, err := s.ListGroves(ctx, store.GroveFilter{OwnerID: ownerID}, store.ListOptions{})
	require.NoError(t, err)
	assert.Equal(t, 1, result.TotalCount)
	assert.Equal(t, ownedGrove.ID, result.Items[0].ID)

	// scope=shared: MemberGroveIDs includes both owned and shared grove IDs,
	// but ExcludeOwnerID removes the owned one
	result, err = s.ListGroves(ctx, store.GroveFilter{
		MemberGroveIDs: []string{ownedGrove.ID, sharedGrove.ID},
		ExcludeOwnerID: ownerID,
	}, store.ListOptions{})
	require.NoError(t, err)
	assert.Equal(t, 1, result.TotalCount)
	assert.Equal(t, sharedGrove.ID, result.Items[0].ID)

	// MemberGroveIDs without ExcludeOwnerID returns all matched groves
	result, err = s.ListGroves(ctx, store.GroveFilter{
		MemberGroveIDs: []string{ownedGrove.ID, sharedGrove.ID},
	}, store.ListOptions{})
	require.NoError(t, err)
	assert.Equal(t, 2, result.TotalCount)

	// Empty MemberGroveIDs with ExcludeOwnerID is a no-op on membership filter
	result, err = s.ListGroves(ctx, store.GroveFilter{
		ExcludeOwnerID: ownerID,
	}, store.ListOptions{})
	require.NoError(t, err)
	// Returns all groves not owned by ownerID
	assert.Equal(t, 2, result.TotalCount)
}

func TestListGrovesSharedScopeTransitiveGroup(t *testing.T) {
	s := setupTestStore(t)
	ctx := context.Background()

	userID := "user_transitive"
	otherOwnerID := api.NewUUID()

	// Create a grove owned by someone else
	sharedGrove := &store.Grove{
		ID:         api.NewUUID(),
		Name:       "Transitively Shared Grove",
		Slug:       "trans-shared-grove",
		OwnerID:    otherOwnerID,
		Visibility: store.VisibilityPrivate,
	}
	require.NoError(t, s.CreateGrove(ctx, sharedGrove))

	// Create a grove_agents group linked to the grove (simulates the grove
	// membership group that is created when a grove gains members).
	groveGroup := &store.Group{
		ID:        api.NewUUID(),
		Name:      "Grove Agents",
		Slug:      "grove-agents-trans",
		GroupType: "grove_agents",
		GroveID:   sharedGrove.ID,
		Created:   time.Now(),
		Updated:   time.Now(),
	}
	// Create an intermediate parent group that is a member of the grove group
	parentGroup := &store.Group{
		ID:      api.NewUUID(),
		Name:    "Team Alpha",
		Slug:    "team-alpha",
		Created: time.Now(),
		Updated: time.Now(),
	}
	// Create the child group the user is a direct member of
	childGroup := &store.Group{
		ID:      api.NewUUID(),
		Name:    "Sub-Team",
		Slug:    "sub-team",
		Created: time.Now(),
		Updated: time.Now(),
	}

	for _, g := range []*store.Group{groveGroup, parentGroup, childGroup} {
		require.NoError(t, s.CreateGroup(ctx, g))
	}

	// parentGroup is a member of groveGroup (admin access to the grove)
	require.NoError(t, s.AddGroupMember(ctx, &store.GroupMember{
		GroupID:    groveGroup.ID,
		MemberType: "group",
		MemberID:   parentGroup.ID,
		Role:       "admin",
		AddedAt:    time.Now(),
	}))

	// childGroup is a member of parentGroup
	require.NoError(t, s.AddGroupMember(ctx, &store.GroupMember{
		GroupID:    parentGroup.ID,
		MemberType: "group",
		MemberID:   childGroup.ID,
		Role:       "member",
		AddedAt:    time.Now(),
	}))

	// User is a direct member of childGroup only
	require.NoError(t, s.AddGroupMember(ctx, &store.GroupMember{
		GroupID:    childGroup.ID,
		MemberType: "user",
		MemberID:   userID,
		Role:       "member",
		AddedAt:    time.Now(),
	}))

	// GetEffectiveGroups should return all three groups (child, parent, grove)
	effectiveGroupIDs, err := s.GetEffectiveGroups(ctx, userID)
	require.NoError(t, err)
	assert.Len(t, effectiveGroupIDs, 3)

	// Resolve grove IDs from effective groups (mirrors resolveUserGroveIDs)
	groups, err := s.GetGroupsByIDs(ctx, effectiveGroupIDs)
	require.NoError(t, err)

	var groveIDs []string
	for _, g := range groups {
		if g.GroveID != "" {
			groveIDs = append(groveIDs, g.GroveID)
		}
	}
	require.Len(t, groveIDs, 1, "should find grove via transitive group membership")
	assert.Equal(t, sharedGrove.ID, groveIDs[0])

	// Using the resolved grove IDs in a shared scope filter should return the grove
	result, err := s.ListGroves(ctx, store.GroveFilter{
		MemberGroveIDs: groveIDs,
		ExcludeOwnerID: userID,
	}, store.ListOptions{})
	require.NoError(t, err)
	assert.Equal(t, 1, result.TotalCount)
	assert.Equal(t, sharedGrove.ID, result.Items[0].ID)
}

func TestRuntimeBrokerLookupByName(t *testing.T) {
	s := setupTestStore(t)
	ctx := context.Background()

	// Create a broker
	broker := &store.RuntimeBroker{
		ID:     api.NewUUID(),
		Name:   "My-Laptop",
		Slug:   "my-laptop",
		Status: store.BrokerStatusOnline,
	}
	require.NoError(t, s.CreateRuntimeBroker(ctx, broker))

	// Look up with exact case - should work
	retrieved, err := s.GetRuntimeBrokerByName(ctx, "My-Laptop")
	require.NoError(t, err)
	assert.Equal(t, broker.ID, retrieved.ID)

	// Look up with different case - should still work (case-insensitive)
	retrieved, err = s.GetRuntimeBrokerByName(ctx, "my-laptop")
	require.NoError(t, err)
	assert.Equal(t, broker.ID, retrieved.ID)

	// Look up with all caps - should still work
	retrieved, err = s.GetRuntimeBrokerByName(ctx, "MY-LAPTOP")
	require.NoError(t, err)
	assert.Equal(t, broker.ID, retrieved.ID)

	// Look up non-existent - should return ErrNotFound
	_, err = s.GetRuntimeBrokerByName(ctx, "nonexistent")
	assert.ErrorIs(t, err, store.ErrNotFound)
}

// ============================================================================
// RuntimeBroker Tests
// ============================================================================

func TestRuntimeBrokerCRUD(t *testing.T) {
	s := setupTestStore(t)
	ctx := context.Background()

	// Create broker with CreatedBy tracking
	broker := &store.RuntimeBroker{
		ID:      api.NewUUID(),
		Name:    "Dev Laptop",
		Slug:    "dev-laptop",
		Version: "1.0.0",
		Status:  store.BrokerStatusOnline,
		Capabilities: &store.BrokerCapabilities{
			WebPTY: true,
			Sync:   true,
			Attach: true,
		},
		Profiles: []store.BrokerProfile{
			{Name: "default", Type: "docker", Available: true},
		},
		CreatedBy: "admin-user-456",
	}

	err := s.CreateRuntimeBroker(ctx, broker)
	require.NoError(t, err)
	assert.NotZero(t, broker.Created)

	// Get broker
	retrieved, err := s.GetRuntimeBroker(ctx, broker.ID)
	require.NoError(t, err)
	assert.Equal(t, broker.Name, retrieved.Name)
	assert.True(t, retrieved.Capabilities.WebPTY)
	assert.Len(t, retrieved.Profiles, 1)
	assert.Equal(t, "docker", retrieved.Profiles[0].Type)
	assert.Equal(t, "admin-user-456", retrieved.CreatedBy)

	// Update broker
	retrieved.Status = store.BrokerStatusOffline
	err = s.UpdateRuntimeBroker(ctx, retrieved)
	require.NoError(t, err)

	// Verify update
	retrieved, err = s.GetRuntimeBroker(ctx, broker.ID)
	require.NoError(t, err)
	assert.Equal(t, store.BrokerStatusOffline, retrieved.Status)

	// Update heartbeat
	err = s.UpdateRuntimeBrokerHeartbeat(ctx, broker.ID, store.BrokerStatusOnline)
	require.NoError(t, err)

	// Verify heartbeat
	retrieved, err = s.GetRuntimeBroker(ctx, broker.ID)
	require.NoError(t, err)
	assert.Equal(t, store.BrokerStatusOnline, retrieved.Status)
	assert.NotZero(t, retrieved.LastHeartbeat)

	// Delete broker
	err = s.DeleteRuntimeBroker(ctx, broker.ID)
	require.NoError(t, err)

	_, err = s.GetRuntimeBroker(ctx, broker.ID)
	assert.ErrorIs(t, err, store.ErrNotFound)
}

func TestRuntimeBrokerList(t *testing.T) {
	s := setupTestStore(t)
	ctx := context.Background()

	// Create brokers
	for i := 0; i < 3; i++ {
		broker := &store.RuntimeBroker{
			ID:     api.NewUUID(),
			Name:   "Host " + string(rune('A'+i)),
			Slug:   "host-" + string(rune('a'+i)),
			Status: store.BrokerStatusOnline,
			Profiles: []store.BrokerProfile{
				{Name: "default", Type: "docker", Available: true},
			},
		}
		if i == 0 {
			broker.Status = store.BrokerStatusOffline
		}
		require.NoError(t, s.CreateRuntimeBroker(ctx, broker))
	}

	// List all
	result, err := s.ListRuntimeBrokers(ctx, store.RuntimeBrokerFilter{}, store.ListOptions{})
	require.NoError(t, err)
	assert.Equal(t, 3, result.TotalCount)

	// List by status
	result, err = s.ListRuntimeBrokers(ctx, store.RuntimeBrokerFilter{Status: store.BrokerStatusOffline}, store.ListOptions{})
	require.NoError(t, err)
	assert.Equal(t, 1, result.TotalCount)

	// List by name (exact match, case-insensitive)
	result, err = s.ListRuntimeBrokers(ctx, store.RuntimeBrokerFilter{Name: "Host A"}, store.ListOptions{})
	require.NoError(t, err)
	assert.Equal(t, 1, result.TotalCount)
	assert.Equal(t, "Host A", result.Items[0].Name)

	// List by name (case-insensitive)
	result, err = s.ListRuntimeBrokers(ctx, store.RuntimeBrokerFilter{Name: "host b"}, store.ListOptions{})
	require.NoError(t, err)
	assert.Equal(t, 1, result.TotalCount)
	assert.Equal(t, "Host B", result.Items[0].Name)

	// List by name (no match)
	result, err = s.ListRuntimeBrokers(ctx, store.RuntimeBrokerFilter{Name: "nonexistent"}, store.ListOptions{})
	require.NoError(t, err)
	assert.Equal(t, 0, result.TotalCount)
}

func TestRuntimeBrokerListByGroveIncludesAutoProvide(t *testing.T) {
	s := setupTestStore(t)
	ctx := context.Background()

	// Create a grove
	grove := &store.Grove{
		ID:      "grove-autoprovide-test",
		Slug:    "autoprovide-test",
		Name:    "AutoProvide Test",
		Created: time.Now(),
		Updated: time.Now(),
	}
	require.NoError(t, s.CreateGrove(ctx, grove))

	// Create a regular broker explicitly linked to the grove
	linkedBroker := &store.RuntimeBroker{
		ID:     "broker-linked",
		Name:   "Linked Broker",
		Slug:   "linked-broker",
		Status: store.BrokerStatusOnline,
	}
	require.NoError(t, s.CreateRuntimeBroker(ctx, linkedBroker))
	require.NoError(t, s.AddGroveProvider(ctx, &store.GroveProvider{
		GroveID:    grove.ID,
		BrokerID:   linkedBroker.ID,
		BrokerName: linkedBroker.Name,
		Status:     store.BrokerStatusOnline,
	}))

	// Create an auto-provide broker (NOT explicitly linked to the grove)
	autoBroker := &store.RuntimeBroker{
		ID:          "broker-auto",
		Name:        "Auto Broker",
		Slug:        "auto-broker",
		Status:      store.BrokerStatusOnline,
		AutoProvide: true,
	}
	require.NoError(t, s.CreateRuntimeBroker(ctx, autoBroker))

	// Create a regular broker NOT linked to the grove
	unlinkedBroker := &store.RuntimeBroker{
		ID:     "broker-unlinked",
		Name:   "Unlinked Broker",
		Slug:   "unlinked-broker",
		Status: store.BrokerStatusOnline,
	}
	require.NoError(t, s.CreateRuntimeBroker(ctx, unlinkedBroker))

	// List brokers for the grove — should include linked + auto-provide, but not unlinked
	result, err := s.ListRuntimeBrokers(ctx, store.RuntimeBrokerFilter{GroveID: grove.ID}, store.ListOptions{})
	require.NoError(t, err)
	assert.Equal(t, 2, result.TotalCount)

	ids := make(map[string]bool)
	for _, b := range result.Items {
		ids[b.ID] = true
	}
	assert.True(t, ids["broker-linked"], "linked broker should be included")
	assert.True(t, ids["broker-auto"], "auto-provide broker should be included")
	assert.False(t, ids["broker-unlinked"], "unlinked broker should not be included")
}

// ============================================================================
// Template Tests
// ============================================================================

func TestTemplateCRUD(t *testing.T) {
	s := setupTestStore(t)
	ctx := context.Background()

	// Create template
	template := &store.Template{
		ID:         api.NewUUID(),
		Name:       "Claude Default",
		Slug:       "claude-default",
		Harness:    "claude",
		Image:      "scion-claude:latest",
		Scope:      "global",
		Visibility: store.VisibilityPublic,
		Config: &store.TemplateConfig{
			Harness:  "claude",
			Detached: true,
		},
	}

	err := s.CreateTemplate(ctx, template)
	require.NoError(t, err)
	assert.NotZero(t, template.Created)

	// Get template
	retrieved, err := s.GetTemplate(ctx, template.ID)
	require.NoError(t, err)
	assert.Equal(t, template.Name, retrieved.Name)
	assert.Equal(t, template.Harness, retrieved.Harness)
	assert.True(t, retrieved.Config.Detached)

	// Get by slug
	retrieved, err = s.GetTemplateBySlug(ctx, "claude-default", "global", "")
	require.NoError(t, err)
	assert.Equal(t, template.ID, retrieved.ID)

	// Update template
	retrieved.Image = "scion-claude:v2"
	err = s.UpdateTemplate(ctx, retrieved)
	require.NoError(t, err)

	// Verify update
	retrieved, err = s.GetTemplate(ctx, template.ID)
	require.NoError(t, err)
	assert.Equal(t, "scion-claude:v2", retrieved.Image)

	// Delete template
	err = s.DeleteTemplate(ctx, template.ID)
	require.NoError(t, err)

	_, err = s.GetTemplate(ctx, template.ID)
	assert.ErrorIs(t, err, store.ErrNotFound)
}

func TestTemplateList(t *testing.T) {
	s := setupTestStore(t)
	ctx := context.Background()

	// Create templates
	for i := 0; i < 3; i++ {
		template := &store.Template{
			ID:         api.NewUUID(),
			Name:       "Template " + string(rune('A'+i)),
			Slug:       "template-" + string(rune('a'+i)),
			Harness:    "claude",
			Scope:      "global",
			Visibility: store.VisibilityPublic,
		}
		if i == 0 {
			template.Harness = "gemini"
		}
		require.NoError(t, s.CreateTemplate(ctx, template))
	}

	// List all
	result, err := s.ListTemplates(ctx, store.TemplateFilter{}, store.ListOptions{})
	require.NoError(t, err)
	assert.Equal(t, 3, result.TotalCount)

	// List by harness
	result, err = s.ListTemplates(ctx, store.TemplateFilter{Harness: "gemini"}, store.ListOptions{})
	require.NoError(t, err)
	assert.Equal(t, 1, result.TotalCount)
}

// ============================================================================
// HarnessConfig Tests
// ============================================================================

func TestHarnessConfigCRUD(t *testing.T) {
	s := setupTestStore(t)
	ctx := context.Background()

	// Create harness config
	hc := &store.HarnessConfig{
		ID:         api.NewUUID(),
		Name:       "Claude Default",
		Slug:       "claude-default",
		Harness:    "claude",
		Scope:      "global",
		Visibility: store.VisibilityPublic,
		Config: &store.HarnessConfigData{
			Harness: "claude",
			Image:   "scion-claude:latest",
		},
	}

	err := s.CreateHarnessConfig(ctx, hc)
	require.NoError(t, err)
	assert.NotZero(t, hc.Created)

	// Get harness config
	retrieved, err := s.GetHarnessConfig(ctx, hc.ID)
	require.NoError(t, err)
	assert.Equal(t, hc.Name, retrieved.Name)
	assert.Equal(t, hc.Harness, retrieved.Harness)
	assert.Equal(t, "claude", retrieved.Config.Harness)
	assert.Equal(t, "scion-claude:latest", retrieved.Config.Image)

	// Get by slug
	retrieved, err = s.GetHarnessConfigBySlug(ctx, "claude-default", "global", "")
	require.NoError(t, err)
	assert.Equal(t, hc.ID, retrieved.ID)

	// Update harness config
	retrieved.Description = "Updated description"
	err = s.UpdateHarnessConfig(ctx, retrieved)
	require.NoError(t, err)

	// Verify update
	retrieved, err = s.GetHarnessConfig(ctx, hc.ID)
	require.NoError(t, err)
	assert.Equal(t, "Updated description", retrieved.Description)

	// Delete harness config
	err = s.DeleteHarnessConfig(ctx, hc.ID)
	require.NoError(t, err)

	_, err = s.GetHarnessConfig(ctx, hc.ID)
	assert.ErrorIs(t, err, store.ErrNotFound)
}

func TestHarnessConfigList(t *testing.T) {
	s := setupTestStore(t)
	ctx := context.Background()

	// Create harness configs
	for i := 0; i < 3; i++ {
		hc := &store.HarnessConfig{
			ID:         api.NewUUID(),
			Name:       "HC " + string(rune('A'+i)),
			Slug:       "hc-" + string(rune('a'+i)),
			Harness:    "claude",
			Scope:      "global",
			Visibility: store.VisibilityPublic,
		}
		if i == 0 {
			hc.Harness = "gemini"
		}
		require.NoError(t, s.CreateHarnessConfig(ctx, hc))
	}

	// List all
	result, err := s.ListHarnessConfigs(ctx, store.HarnessConfigFilter{}, store.ListOptions{})
	require.NoError(t, err)
	assert.Equal(t, 3, result.TotalCount)

	// List by harness
	result, err = s.ListHarnessConfigs(ctx, store.HarnessConfigFilter{Harness: "gemini"}, store.ListOptions{})
	require.NoError(t, err)
	assert.Equal(t, 1, result.TotalCount)
}

// ============================================================================
// User Tests
// ============================================================================

func TestUserCRUD(t *testing.T) {
	s := setupTestStore(t)
	ctx := context.Background()

	// Create user
	user := &store.User{
		ID:          api.NewUUID(),
		Email:       "test@example.com",
		DisplayName: "Test User",
		Role:        store.UserRoleMember,
		Status:      "active",
		Preferences: &store.UserPreferences{
			Theme: "dark",
		},
	}

	err := s.CreateUser(ctx, user)
	require.NoError(t, err)
	assert.NotZero(t, user.Created)

	// Get user
	retrieved, err := s.GetUser(ctx, user.ID)
	require.NoError(t, err)
	assert.Equal(t, user.Email, retrieved.Email)
	assert.Equal(t, "dark", retrieved.Preferences.Theme)

	// Get by email
	retrieved, err = s.GetUserByEmail(ctx, "test@example.com")
	require.NoError(t, err)
	assert.Equal(t, user.ID, retrieved.ID)

	// Test unique constraint on email
	duplicate := &store.User{
		ID:          api.NewUUID(),
		Email:       "test@example.com",
		DisplayName: "Duplicate User",
		Role:        store.UserRoleMember,
		Status:      "active",
	}
	err = s.CreateUser(ctx, duplicate)
	assert.ErrorIs(t, err, store.ErrAlreadyExists)

	// Update user
	retrieved.DisplayName = "Updated User"
	retrieved.LastLogin = time.Now()
	err = s.UpdateUser(ctx, retrieved)
	require.NoError(t, err)

	// Verify update
	retrieved, err = s.GetUser(ctx, user.ID)
	require.NoError(t, err)
	assert.Equal(t, "Updated User", retrieved.DisplayName)
	assert.NotZero(t, retrieved.LastLogin)

	// Delete user
	err = s.DeleteUser(ctx, user.ID)
	require.NoError(t, err)

	_, err = s.GetUser(ctx, user.ID)
	assert.ErrorIs(t, err, store.ErrNotFound)
}

func TestUserList(t *testing.T) {
	s := setupTestStore(t)
	ctx := context.Background()

	// Create users
	for i := 0; i < 3; i++ {
		user := &store.User{
			ID:          api.NewUUID(),
			Email:       "user" + string(rune('a'+i)) + "@example.com",
			DisplayName: "User " + string(rune('A'+i)),
			Role:        store.UserRoleMember,
			Status:      "active",
		}
		if i == 0 {
			user.Role = store.UserRoleAdmin
		}
		require.NoError(t, s.CreateUser(ctx, user))
	}

	// List all
	result, err := s.ListUsers(ctx, store.UserFilter{}, store.ListOptions{})
	require.NoError(t, err)
	assert.Equal(t, 3, result.TotalCount)

	// List by role
	result, err = s.ListUsers(ctx, store.UserFilter{Role: store.UserRoleAdmin}, store.ListOptions{})
	require.NoError(t, err)
	assert.Equal(t, 1, result.TotalCount)
}

func TestUserListSorting(t *testing.T) {
	s := setupTestStore(t)
	ctx := context.Background()

	// Create users with distinct names so sort order is deterministic
	names := []string{"Charlie", "Alice", "Bob"}
	for _, name := range names {
		user := &store.User{
			ID:          api.NewUUID(),
			Email:       strings.ToLower(name) + "@example.com",
			DisplayName: name,
			Role:        store.UserRoleMember,
			Status:      "active",
		}
		require.NoError(t, s.CreateUser(ctx, user))
	}

	// Sort by name ascending
	result, err := s.ListUsers(ctx, store.UserFilter{}, store.ListOptions{SortBy: "name", SortDir: "asc"})
	require.NoError(t, err)
	require.Len(t, result.Items, 3)
	assert.Equal(t, "Alice", result.Items[0].DisplayName)
	assert.Equal(t, "Bob", result.Items[1].DisplayName)
	assert.Equal(t, "Charlie", result.Items[2].DisplayName)

	// Sort by name descending
	result, err = s.ListUsers(ctx, store.UserFilter{}, store.ListOptions{SortBy: "name", SortDir: "desc"})
	require.NoError(t, err)
	require.Len(t, result.Items, 3)
	assert.Equal(t, "Charlie", result.Items[0].DisplayName)
	assert.Equal(t, "Bob", result.Items[1].DisplayName)
	assert.Equal(t, "Alice", result.Items[2].DisplayName)

	// Sort by created descending (default) — most recent first
	result, err = s.ListUsers(ctx, store.UserFilter{}, store.ListOptions{SortBy: "created", SortDir: "desc"})
	require.NoError(t, err)
	require.Len(t, result.Items, 3)
	// Last created should be first
	assert.Equal(t, "Bob", result.Items[0].DisplayName)

	// Sorting should work across pages: page 1 with limit 2, sorted by name asc
	result, err = s.ListUsers(ctx, store.UserFilter{}, store.ListOptions{Limit: 2, SortBy: "name", SortDir: "asc"})
	require.NoError(t, err)
	require.Len(t, result.Items, 2)
	assert.Equal(t, "Alice", result.Items[0].DisplayName)
	assert.Equal(t, "Bob", result.Items[1].DisplayName)
	assert.NotEmpty(t, result.NextCursor)

	// Page 2 should have the remaining user
	result, err = s.ListUsers(ctx, store.UserFilter{}, store.ListOptions{Limit: 2, Cursor: result.NextCursor, SortBy: "name", SortDir: "asc"})
	require.NoError(t, err)
	require.Len(t, result.Items, 1)
	assert.Equal(t, "Charlie", result.Items[0].DisplayName)
	assert.Empty(t, result.NextCursor)
}

// ============================================================================
// GroveProvider Tests
// ============================================================================

func TestGroveProviders(t *testing.T) {
	s := setupTestStore(t)
	ctx := context.Background()

	// Create grove
	grove := &store.Grove{
		ID:         api.NewUUID(),
		Name:       "Test Grove",
		Slug:       "test-grove",
		Visibility: store.VisibilityPrivate,
	}
	require.NoError(t, s.CreateGrove(ctx, grove))

	// Create brokers
	broker1 := &store.RuntimeBroker{
		ID:     api.NewUUID(),
		Name:   "Host 1",
		Slug:   "host-1",
		Status: store.BrokerStatusOnline,
		Profiles: []store.BrokerProfile{
			{Name: "docker", Type: "docker", Available: true},
			{Name: "dev", Type: "docker", Available: true},
		},
	}
	require.NoError(t, s.CreateRuntimeBroker(ctx, broker1))

	broker2 := &store.RuntimeBroker{
		ID:     api.NewUUID(),
		Name:   "Host 2",
		Slug:   "host-2",
		Status: store.BrokerStatusOnline,
		Profiles: []store.BrokerProfile{
			{Name: "k8s-prod", Type: "kubernetes", Available: true},
		},
	}
	require.NoError(t, s.CreateRuntimeBroker(ctx, broker2))

	// Add providers with user tracking
	provider1 := &store.GroveProvider{
		GroveID:    grove.ID,
		BrokerID:   broker1.ID,
		BrokerName: broker1.Name,
		Status:     store.BrokerStatusOnline,
		LinkedBy:   "user-123",
	}
	require.NoError(t, s.AddGroveProvider(ctx, provider1))

	provider2 := &store.GroveProvider{
		GroveID:    grove.ID,
		BrokerID:   broker2.ID,
		BrokerName: broker2.Name,
		Status:     store.BrokerStatusOnline,
	}
	require.NoError(t, s.AddGroveProvider(ctx, provider2))

	// Get grove providers
	providers, err := s.GetGroveProviders(ctx, grove.ID)
	require.NoError(t, err)
	assert.Len(t, providers, 2)

	// Verify user tracking fields are stored
	for _, p := range providers {
		if p.BrokerID == broker1.ID {
			assert.Equal(t, "user-123", p.LinkedBy)
			assert.False(t, p.LinkedAt.IsZero(), "LinkedAt should be set")
		}
	}

	// Verify GetGroveProvider also returns user tracking fields
	provider, err := s.GetGroveProvider(ctx, grove.ID, broker1.ID)
	require.NoError(t, err)
	assert.Equal(t, "user-123", provider.LinkedBy)
	assert.False(t, provider.LinkedAt.IsZero(), "LinkedAt should be set")

	// Get broker groves
	groves, err := s.GetBrokerGroves(ctx, broker1.ID)
	require.NoError(t, err)
	assert.Len(t, groves, 1)
	assert.Equal(t, grove.ID, groves[0].GroveID)

	// Update provider status
	err = s.UpdateProviderStatus(ctx, grove.ID, broker1.ID, store.BrokerStatusOffline)
	require.NoError(t, err)

	// Verify update
	providers, err = s.GetGroveProviders(ctx, grove.ID)
	require.NoError(t, err)
	for _, p := range providers {
		if p.BrokerID == broker1.ID {
			assert.Equal(t, store.BrokerStatusOffline, p.Status)
		}
	}

	// Verify grove's active broker count
	retrievedGrove, err := s.GetGrove(ctx, grove.ID)
	require.NoError(t, err)
	assert.Equal(t, 1, retrievedGrove.ActiveBrokerCount) // Only broker2 is online

	// Remove provider
	err = s.RemoveGroveProvider(ctx, grove.ID, broker1.ID)
	require.NoError(t, err)

	providers, err = s.GetGroveProviders(ctx, grove.ID)
	require.NoError(t, err)
	assert.Len(t, providers, 1)
	assert.Equal(t, broker2.ID, providers[0].BrokerID)
}

// ============================================================================
// Migration Tests
// ============================================================================

func TestMigration(t *testing.T) {
	s, err := New(":memory:")
	require.NoError(t, err)
	defer s.Close()

	ctx := context.Background()

	// Run migrations
	err = s.Migrate(ctx)
	require.NoError(t, err)

	// Run again (should be idempotent)
	err = s.Migrate(ctx)
	require.NoError(t, err)

	// Verify tables exist by inserting data
	grove := &store.Grove{
		ID:         api.NewUUID(),
		Name:       "Test",
		Slug:       "test",
		Visibility: store.VisibilityPrivate,
	}
	err = s.CreateGrove(ctx, grove)
	require.NoError(t, err)
}

func TestDropTableCascadesWithForeignKeysOn(t *testing.T) {
	// Demonstrates the root cause: DROP TABLE with foreign_keys=ON
	// triggers ON DELETE CASCADE, removing all child rows.
	s, err := New(":memory:")
	require.NoError(t, err)
	defer s.Close()

	ctx := context.Background()
	err = s.Migrate(ctx)
	require.NoError(t, err)

	groveID := api.NewUUID()
	err = s.CreateGrove(ctx, &store.Grove{
		ID: groveID, Name: "G", Slug: "g-cascade-test", Visibility: store.VisibilityPrivate,
	})
	require.NoError(t, err)

	agentID := api.NewUUID()
	err = s.CreateAgent(ctx, &store.Agent{
		ID: agentID, Slug: "a", Name: "A", GroveID: groveID, Visibility: store.VisibilityPrivate,
	})
	require.NoError(t, err)

	// With foreign_keys ON (default), DROP TABLE cascades
	_, err = s.db.ExecContext(ctx, `
		CREATE TABLE groves_copy AS SELECT * FROM groves;
		DROP TABLE groves;
		ALTER TABLE groves_copy RENAME TO groves;
	`)
	require.NoError(t, err)

	// Agent was cascade-deleted — this is the bug V40 originally triggered
	_, err = s.GetAgent(ctx, agentID)
	assert.ErrorIs(t, err, store.ErrNotFound, "agent should be cascade-deleted when FK is ON")
}

func TestMigrationV40PreservesAgents(t *testing.T) {
	// Regression test: V40 drops and recreates the groves table. Without
	// PRAGMA foreign_keys=OFF (which must be set OUTSIDE the transaction),
	// DROP TABLE groves triggers ON DELETE CASCADE on agents, deleting all rows.
	s, err := New(":memory:")
	require.NoError(t, err)
	defer s.Close()

	ctx := context.Background()

	err = s.Migrate(ctx)
	require.NoError(t, err)

	// Create a grove and an agent
	groveID := api.NewUUID()
	err = s.CreateGrove(ctx, &store.Grove{
		ID:         groveID,
		Name:       "TestGrove",
		Slug:       "test-grove",
		Visibility: store.VisibilityPrivate,
	})
	require.NoError(t, err)

	agentID := api.NewUUID()
	err = s.CreateAgent(ctx, &store.Agent{
		ID:         agentID,
		Slug:       "test-agent",
		Name:       "Test Agent",
		GroveID:    groveID,
		Visibility: store.VisibilityPrivate,
	})
	require.NoError(t, err)

	// Verify agent exists
	agent, err := s.GetAgent(ctx, agentID)
	require.NoError(t, err)
	assert.Equal(t, "Test Agent", agent.Name)

	// Simulate re-running V40 by dropping and recreating groves table
	// using the same pattern as the migration, with proper FK handling.
	_, err = s.db.ExecContext(ctx, "PRAGMA foreign_keys=OFF")
	require.NoError(t, err)

	_, err = s.db.ExecContext(ctx, `
		CREATE TABLE groves_new2 AS SELECT * FROM groves;
		DROP TABLE groves;
		ALTER TABLE groves_new2 RENAME TO groves;
	`)
	require.NoError(t, err)

	_, err = s.db.ExecContext(ctx, "PRAGMA foreign_keys=ON")
	require.NoError(t, err)

	// Agent must still exist after table recreation
	agent, err = s.GetAgent(ctx, agentID)
	require.NoError(t, err)
	assert.Equal(t, "Test Agent", agent.Name)
}

func TestPing(t *testing.T) {
	s := setupTestStore(t)
	ctx := context.Background()

	err := s.Ping(ctx)
	require.NoError(t, err)
}

// ============================================================================
// Error Cases
// ============================================================================

func TestNotFoundErrors(t *testing.T) {
	s := setupTestStore(t)
	ctx := context.Background()

	nonExistentID := api.NewUUID()

	// Agent
	_, err := s.GetAgent(ctx, nonExistentID)
	assert.ErrorIs(t, err, store.ErrNotFound)

	err = s.DeleteAgent(ctx, nonExistentID)
	assert.ErrorIs(t, err, store.ErrNotFound)

	// Grove
	_, err = s.GetGrove(ctx, nonExistentID)
	assert.ErrorIs(t, err, store.ErrNotFound)

	err = s.DeleteGrove(ctx, nonExistentID)
	assert.ErrorIs(t, err, store.ErrNotFound)

	// RuntimeBroker
	_, err = s.GetRuntimeBroker(ctx, nonExistentID)
	assert.ErrorIs(t, err, store.ErrNotFound)

	err = s.DeleteRuntimeBroker(ctx, nonExistentID)
	assert.ErrorIs(t, err, store.ErrNotFound)

	// Template
	_, err = s.GetTemplate(ctx, nonExistentID)
	assert.ErrorIs(t, err, store.ErrNotFound)

	err = s.DeleteTemplate(ctx, nonExistentID)
	assert.ErrorIs(t, err, store.ErrNotFound)

	// User
	_, err = s.GetUser(ctx, nonExistentID)
	assert.ErrorIs(t, err, store.ErrNotFound)

	err = s.DeleteUser(ctx, nonExistentID)
	assert.ErrorIs(t, err, store.ErrNotFound)
}

func TestCascadeDelete(t *testing.T) {
	s := setupTestStore(t)
	ctx := context.Background()

	// Create grove with agent
	grove := &store.Grove{
		ID:         api.NewUUID(),
		Name:       "Test Grove",
		Slug:       "test-grove",
		Visibility: store.VisibilityPrivate,
	}
	require.NoError(t, s.CreateGrove(ctx, grove))

	agent := &store.Agent{
		ID:         api.NewUUID(),
		Slug:       "test-agent",
		Name:       "Test Agent",
		Template:   "claude",
		GroveID:    grove.ID,
		Phase:      string(state.PhaseRunning),
		Visibility: store.VisibilityPrivate,
	}
	require.NoError(t, s.CreateAgent(ctx, agent))

	// Delete grove
	err := s.DeleteGrove(ctx, grove.ID)
	require.NoError(t, err)

	// Verify agent was cascade deleted
	_, err = s.GetAgent(ctx, agent.ID)
	assert.ErrorIs(t, err, store.ErrNotFound)
}

func TestCascadeDeleteEnvVarsSecrets(t *testing.T) {
	s := setupTestStore(t)
	ctx := context.Background()

	groveID := api.NewUUID()
	require.NoError(t, s.CreateGrove(ctx, &store.Grove{
		ID: groveID, Name: "Cascade EV/S", Slug: "cascade-ev-s",
		Visibility: store.VisibilityPrivate,
	}))

	// Create grove-scoped env vars
	require.NoError(t, s.CreateEnvVar(ctx, &store.EnvVar{
		ID: api.NewUUID(), Key: "A", Value: "1",
		Scope: store.ScopeGrove, ScopeID: groveID,
	}))
	require.NoError(t, s.CreateEnvVar(ctx, &store.EnvVar{
		ID: api.NewUUID(), Key: "B", Value: "2",
		Scope: store.ScopeGrove, ScopeID: groveID,
	}))

	// Create grove-scoped secrets
	require.NoError(t, s.CreateSecret(ctx, &store.Secret{
		ID: api.NewUUID(), Key: "S1", EncryptedValue: "enc1",
		Scope: store.ScopeGrove, ScopeID: groveID, Version: 1,
	}))

	// Create a hub-scoped env var (should not be deleted)
	require.NoError(t, s.CreateEnvVar(ctx, &store.EnvVar{
		ID: api.NewUUID(), Key: "HUB_VAR", Value: "hub",
		Scope: store.ScopeHub, ScopeID: "test-hub-id",
	}))

	// Delete by scope
	n, err := s.DeleteEnvVarsByScope(ctx, store.ScopeGrove, groveID)
	require.NoError(t, err)
	assert.Equal(t, 2, n)

	n, err = s.DeleteSecretsByScope(ctx, store.ScopeGrove, groveID)
	require.NoError(t, err)
	assert.Equal(t, 1, n)

	// Verify grove-scoped are gone
	envVars, err := s.ListEnvVars(ctx, store.EnvVarFilter{Scope: store.ScopeGrove, ScopeID: groveID})
	require.NoError(t, err)
	assert.Empty(t, envVars)

	secrets, err := s.ListSecrets(ctx, store.SecretFilter{Scope: store.ScopeGrove, ScopeID: groveID})
	require.NoError(t, err)
	assert.Empty(t, secrets)

	// Verify hub-scoped env var still exists
	hubVars, err := s.ListEnvVars(ctx, store.EnvVarFilter{Scope: store.ScopeHub, ScopeID: "test-hub-id"})
	require.NoError(t, err)
	assert.Len(t, hubVars, 1)

	// Delete with no matches returns 0, no error
	n, err = s.DeleteEnvVarsByScope(ctx, store.ScopeGrove, "nonexistent")
	require.NoError(t, err)
	assert.Equal(t, 0, n)
}

func TestDeleteHarnessConfigsByScope(t *testing.T) {
	s := setupTestStore(t)
	ctx := context.Background()

	groveID := api.NewUUID()
	require.NoError(t, s.CreateGrove(ctx, &store.Grove{
		ID: groveID, Name: "HC Cascade", Slug: "hc-cascade",
		Visibility: store.VisibilityPrivate,
	}))

	require.NoError(t, s.CreateHarnessConfig(ctx, &store.HarnessConfig{
		ID: api.NewUUID(), Name: "hc1", Slug: "hc1",
		Harness: "claude", Scope: store.ScopeGrove, ScopeID: groveID,
		Status: store.HarnessConfigStatusActive, Visibility: store.VisibilityPrivate,
	}))

	n, err := s.DeleteHarnessConfigsByScope(ctx, store.ScopeGrove, groveID)
	require.NoError(t, err)
	assert.Equal(t, 1, n)

	result, err := s.ListHarnessConfigs(ctx, store.HarnessConfigFilter{Scope: store.ScopeGrove, ScopeID: groveID}, store.ListOptions{})
	require.NoError(t, err)
	assert.Empty(t, result.Items)
}

func TestDeleteTemplatesByScope(t *testing.T) {
	s := setupTestStore(t)
	ctx := context.Background()

	groveID := api.NewUUID()
	require.NoError(t, s.CreateGrove(ctx, &store.Grove{
		ID: groveID, Name: "Tmpl Cascade", Slug: "tmpl-cascade",
		Visibility: store.VisibilityPrivate,
	}))

	require.NoError(t, s.CreateTemplate(ctx, &store.Template{
		ID: api.NewUUID(), Name: "tmpl1", Slug: "tmpl1",
		Harness: "claude", Scope: store.ScopeGrove, ScopeID: groveID,
		Status: store.TemplateStatusActive, Visibility: store.VisibilityPrivate,
	}))
	require.NoError(t, s.CreateTemplate(ctx, &store.Template{
		ID: api.NewUUID(), Name: "tmpl2", Slug: "tmpl2",
		Harness: "gemini", Scope: store.ScopeGrove, ScopeID: groveID,
		Status: store.TemplateStatusActive, Visibility: store.VisibilityPrivate,
	}))

	n, err := s.DeleteTemplatesByScope(ctx, store.ScopeGrove, groveID)
	require.NoError(t, err)
	assert.Equal(t, 2, n)

	result, err := s.ListTemplates(ctx, store.TemplateFilter{Scope: store.ScopeGrove, ScopeID: groveID}, store.ListOptions{})
	require.NoError(t, err)
	assert.Empty(t, result.Items)
}

// ============================================================================
// MarkStaleAgentsOffline Tests
// ============================================================================

func TestMarkStaleAgentsOffline(t *testing.T) {
	s := setupTestStore(t)
	ctx := context.Background()

	grove := &store.Grove{
		ID:         api.NewUUID(),
		Name:       "Heartbeat Grove",
		Slug:       "heartbeat-grove",
		Visibility: store.VisibilityPrivate,
	}
	require.NoError(t, s.CreateGrove(ctx, grove))

	staleTime := time.Now().Add(-5 * time.Minute)
	threshold := time.Now().Add(-2 * time.Minute)

	// These agents have phase=running with non-sticky activities → should be marked offline
	activeActivities := []string{"idle", "thinking", "executing", "waiting_for_input"}

	var expectedIDs []string
	for i, activity := range activeActivities {
		agent := &store.Agent{
			ID:         api.NewUUID(),
			Slug:       "active-agent-" + activity,
			Name:       "Active Agent " + activity,
			Template:   "claude",
			GroveID:    grove.ID,
			Phase:      string(state.PhaseCreated),
			Visibility: store.VisibilityPrivate,
		}
		require.NoError(t, s.CreateAgent(ctx, agent))

		// Set to running phase with activity
		err := s.UpdateAgentStatus(ctx, agent.ID, store.AgentStatusUpdate{
			Phase:    "running",
			Activity: activity,
		})
		require.NoError(t, err, "setting activity for agent %d", i)

		// Manually set last_seen to stale time
		_, err = s.db.ExecContext(ctx, "UPDATE agents SET last_seen = ? WHERE id = ?", staleTime, agent.ID)
		require.NoError(t, err)

		expectedIDs = append(expectedIDs, agent.ID)
	}

	// These agents should NOT be marked offline

	// Sticky activity: completed (phase=running)
	completedAgent := &store.Agent{
		ID: api.NewUUID(), Slug: "completed-agent", Name: "Completed Agent",
		Template: "claude", GroveID: grove.ID, Phase: string(state.PhaseCreated),
		Visibility: store.VisibilityPrivate,
	}
	require.NoError(t, s.CreateAgent(ctx, completedAgent))
	require.NoError(t, s.UpdateAgentStatus(ctx, completedAgent.ID, store.AgentStatusUpdate{
		Phase: "running", Activity: "completed",
	}))
	_, err := s.db.ExecContext(ctx, "UPDATE agents SET last_seen = ? WHERE id = ?", staleTime, completedAgent.ID)
	require.NoError(t, err)

	// Sticky activity: limits_exceeded (phase=running)
	limitsAgent := &store.Agent{
		ID: api.NewUUID(), Slug: "limits-agent", Name: "Limits Agent",
		Template: "claude", GroveID: grove.ID, Phase: string(state.PhaseCreated),
		Visibility: store.VisibilityPrivate,
	}
	require.NoError(t, s.CreateAgent(ctx, limitsAgent))
	require.NoError(t, s.UpdateAgentStatus(ctx, limitsAgent.ID, store.AgentStatusUpdate{
		Phase: "running", Activity: "limits_exceeded",
	}))
	_, err = s.db.ExecContext(ctx, "UPDATE agents SET last_seen = ? WHERE id = ?", staleTime, limitsAgent.ID)
	require.NoError(t, err)

	// Non-running phase: stopped
	stoppedAgent := &store.Agent{
		ID: api.NewUUID(), Slug: "stopped-agent", Name: "Stopped Agent",
		Template: "claude", GroveID: grove.ID, Phase: string(state.PhaseStopped),
		Visibility: store.VisibilityPrivate,
	}
	require.NoError(t, s.CreateAgent(ctx, stoppedAgent))
	_, err = s.db.ExecContext(ctx, "UPDATE agents SET last_seen = ? WHERE id = ?", staleTime, stoppedAgent.ID)
	require.NoError(t, err)

	// Recent heartbeat (should not be affected)
	recentAgent := &store.Agent{
		ID: api.NewUUID(), Slug: "recent-agent", Name: "Recent Agent",
		Template: "claude", GroveID: grove.ID, Phase: string(state.PhaseCreated),
		Visibility: store.VisibilityPrivate,
	}
	require.NoError(t, s.CreateAgent(ctx, recentAgent))
	require.NoError(t, s.UpdateAgentStatus(ctx, recentAgent.ID, store.AgentStatusUpdate{
		Phase: "running", Activity: "idle",
	}))
	// last_seen is set to now by UpdateAgentStatus, which is within the threshold

	// Execute
	agents, err := s.MarkStaleAgentsOffline(ctx, threshold)
	require.NoError(t, err)
	assert.Len(t, agents, len(activeActivities), "should only mark running stale agents with non-sticky activities")

	// Verify the returned agents
	returnedIDs := make(map[string]bool)
	for _, a := range agents {
		returnedIDs[a.ID] = true
		assert.Equal(t, "offline", a.Activity, "returned agent should have offline activity")
		assert.Equal(t, "running", a.Phase, "returned agent should still have running phase")
		assert.Equal(t, string(state.ActivityOffline), a.Activity, "returned agent should have offline activity")
	}
	for _, id := range expectedIDs {
		assert.True(t, returnedIDs[id], "expected agent %s to be in returned set", id)
	}

	// Verify sticky activities were NOT affected
	a, err := s.GetAgent(ctx, completedAgent.ID)
	require.NoError(t, err)
	assert.Equal(t, "completed", a.Activity)

	a, err = s.GetAgent(ctx, limitsAgent.ID)
	require.NoError(t, err)
	assert.Equal(t, "limits_exceeded", a.Activity)

	// Verify stopped agent was NOT affected
	a, err = s.GetAgent(ctx, stoppedAgent.ID)
	require.NoError(t, err)
	assert.Equal(t, "stopped", a.Phase)

	// Verify recent agent was NOT affected
	a, err = s.GetAgent(ctx, recentAgent.ID)
	require.NoError(t, err)
	assert.Equal(t, "idle", a.Activity)
}

func TestMarkStaleAgentsOffline_Idempotent(t *testing.T) {
	s := setupTestStore(t)
	ctx := context.Background()

	grove := &store.Grove{
		ID:         api.NewUUID(),
		Name:       "Idempotent Grove",
		Slug:       "idempotent-grove",
		Visibility: store.VisibilityPrivate,
	}
	require.NoError(t, s.CreateGrove(ctx, grove))

	staleTime := time.Now().Add(-5 * time.Minute)
	threshold := time.Now().Add(-2 * time.Minute)

	agent := &store.Agent{
		ID: api.NewUUID(), Slug: "stale-agent", Name: "Stale Agent",
		Template: "claude", GroveID: grove.ID, Phase: string(state.PhaseCreated),
		Visibility: store.VisibilityPrivate,
	}
	require.NoError(t, s.CreateAgent(ctx, agent))
	require.NoError(t, s.UpdateAgentStatus(ctx, agent.ID, store.AgentStatusUpdate{
		Phase: "running", Activity: "idle",
	}))
	_, err := s.db.ExecContext(ctx, "UPDATE agents SET last_seen = ? WHERE id = ?", staleTime, agent.ID)
	require.NoError(t, err)

	// First call should mark it offline
	agents, err := s.MarkStaleAgentsOffline(ctx, threshold)
	require.NoError(t, err)
	assert.Len(t, agents, 1)

	// Second call should return empty (already offline)
	agents, err = s.MarkStaleAgentsOffline(ctx, threshold)
	require.NoError(t, err)
	assert.Len(t, agents, 0, "should not re-mark already offline agents")
}

func TestMarkStaleAgentsOffline_NoStaleAgents(t *testing.T) {
	s := setupTestStore(t)
	ctx := context.Background()

	threshold := time.Now().Add(-2 * time.Minute)

	// No agents at all
	agents, err := s.MarkStaleAgentsOffline(ctx, threshold)
	require.NoError(t, err)
	assert.Len(t, agents, 0)
}

// ============================================================================
// Stalled Agent Detection Tests
// ============================================================================

func TestMarkStalledAgents(t *testing.T) {
	s := setupTestStore(t)
	ctx := context.Background()

	grove := &store.Grove{
		ID:         api.NewUUID(),
		Name:       "Stalled Grove",
		Slug:       "stalled-grove",
		Visibility: store.VisibilityPrivate,
	}
	require.NoError(t, s.CreateGrove(ctx, grove))

	staleActivityTime := time.Now().Add(-10 * time.Minute)
	recentHeartbeat := time.Now().Add(-30 * time.Second)
	activityThreshold := time.Now().Add(-5 * time.Minute)
	heartbeatRecency := time.Now().Add(-2 * time.Minute)

	// --- Should be marked stalled: stale activity + recent heartbeat ---
	stalledActivities := []string{"thinking", "executing", "idle"}
	var expectedIDs []string
	for _, activity := range stalledActivities {
		agent := &store.Agent{
			ID: api.NewUUID(), Slug: "stalled-" + activity, Name: "Stalled " + activity,
			Template: "claude", GroveID: grove.ID, Phase: string(state.PhaseCreated),
			Visibility: store.VisibilityPrivate,
		}
		require.NoError(t, s.CreateAgent(ctx, agent))
		require.NoError(t, s.UpdateAgentStatus(ctx, agent.ID, store.AgentStatusUpdate{
			Phase: "running", Activity: activity,
		}))
		// Manually set stale activity time + recent heartbeat
		_, err := s.db.ExecContext(ctx,
			"UPDATE agents SET last_activity_event = ?, last_seen = ? WHERE id = ?",
			staleActivityTime, recentHeartbeat, agent.ID)
		require.NoError(t, err)
		expectedIDs = append(expectedIDs, agent.ID)
	}

	// --- Should NOT be marked stalled ---

	// Recent activity (within threshold)
	recentAgent := &store.Agent{
		ID: api.NewUUID(), Slug: "recent-activity", Name: "Recent Activity",
		Template: "claude", GroveID: grove.ID, Phase: string(state.PhaseCreated),
		Visibility: store.VisibilityPrivate,
	}
	require.NoError(t, s.CreateAgent(ctx, recentAgent))
	require.NoError(t, s.UpdateAgentStatus(ctx, recentAgent.ID, store.AgentStatusUpdate{
		Phase: "running", Activity: "idle",
	}))
	// last_activity_event is set to now by UpdateAgentStatus, which is within threshold

	// Stale activity + stale heartbeat (offline territory, not stalled)
	offlineAgent := &store.Agent{
		ID: api.NewUUID(), Slug: "offline-territory", Name: "Offline Territory",
		Template: "claude", GroveID: grove.ID, Phase: string(state.PhaseCreated),
		Visibility: store.VisibilityPrivate,
	}
	require.NoError(t, s.CreateAgent(ctx, offlineAgent))
	require.NoError(t, s.UpdateAgentStatus(ctx, offlineAgent.ID, store.AgentStatusUpdate{
		Phase: "running", Activity: "idle",
	}))
	staleHeartbeat := time.Now().Add(-5 * time.Minute)
	_, err := s.db.ExecContext(ctx,
		"UPDATE agents SET last_activity_event = ?, last_seen = ? WHERE id = ?",
		staleActivityTime, staleHeartbeat, offlineAgent.ID)
	require.NoError(t, err)

	// Completed activity (sticky — should not be stalled)
	completedAgent := &store.Agent{
		ID: api.NewUUID(), Slug: "completed-stall", Name: "Completed Stall",
		Template: "claude", GroveID: grove.ID, Phase: string(state.PhaseCreated),
		Visibility: store.VisibilityPrivate,
	}
	require.NoError(t, s.CreateAgent(ctx, completedAgent))
	require.NoError(t, s.UpdateAgentStatus(ctx, completedAgent.ID, store.AgentStatusUpdate{
		Phase: "running", Activity: "completed",
	}))
	_, err = s.db.ExecContext(ctx,
		"UPDATE agents SET last_activity_event = ?, last_seen = ? WHERE id = ?",
		staleActivityTime, recentHeartbeat, completedAgent.ID)
	require.NoError(t, err)

	// limits_exceeded activity (sticky)
	limitsAgent := &store.Agent{
		ID: api.NewUUID(), Slug: "limits-stall", Name: "Limits Stall",
		Template: "claude", GroveID: grove.ID, Phase: string(state.PhaseCreated),
		Visibility: store.VisibilityPrivate,
	}
	require.NoError(t, s.CreateAgent(ctx, limitsAgent))
	require.NoError(t, s.UpdateAgentStatus(ctx, limitsAgent.ID, store.AgentStatusUpdate{
		Phase: "running", Activity: "limits_exceeded",
	}))
	_, err = s.db.ExecContext(ctx,
		"UPDATE agents SET last_activity_event = ?, last_seen = ? WHERE id = ?",
		staleActivityTime, recentHeartbeat, limitsAgent.ID)
	require.NoError(t, err)

	// Stopped phase (not running)
	stoppedAgent := &store.Agent{
		ID: api.NewUUID(), Slug: "stopped-stall", Name: "Stopped Stall",
		Template: "claude", GroveID: grove.ID, Phase: string(state.PhaseStopped),
		Visibility: store.VisibilityPrivate,
	}
	require.NoError(t, s.CreateAgent(ctx, stoppedAgent))
	_, err = s.db.ExecContext(ctx,
		"UPDATE agents SET last_activity_event = ?, last_seen = ? WHERE id = ?",
		staleActivityTime, recentHeartbeat, stoppedAgent.ID)
	require.NoError(t, err)

	// Already offline
	alreadyOfflineAgent := &store.Agent{
		ID: api.NewUUID(), Slug: "already-offline", Name: "Already Offline",
		Template: "claude", GroveID: grove.ID, Phase: string(state.PhaseCreated),
		Visibility: store.VisibilityPrivate,
	}
	require.NoError(t, s.CreateAgent(ctx, alreadyOfflineAgent))
	require.NoError(t, s.UpdateAgentStatus(ctx, alreadyOfflineAgent.ID, store.AgentStatusUpdate{
		Phase: "running", Activity: "offline",
	}))
	_, err = s.db.ExecContext(ctx,
		"UPDATE agents SET last_activity_event = ?, last_seen = ? WHERE id = ?",
		staleActivityTime, recentHeartbeat, alreadyOfflineAgent.ID)
	require.NoError(t, err)

	// waiting_for_input activity (sticky waiting state — must NOT stall)
	waitingAgent := &store.Agent{
		ID: api.NewUUID(), Slug: "waiting-for-input", Name: "Waiting For Input",
		Template: "claude", GroveID: grove.ID, Phase: string(state.PhaseCreated),
		Visibility: store.VisibilityPrivate,
	}
	require.NoError(t, s.CreateAgent(ctx, waitingAgent))
	require.NoError(t, s.UpdateAgentStatus(ctx, waitingAgent.ID, store.AgentStatusUpdate{
		Phase: "running", Activity: "waiting_for_input",
	}))
	_, err = s.db.ExecContext(ctx,
		"UPDATE agents SET last_activity_event = ?, last_seen = ? WHERE id = ?",
		staleActivityTime, recentHeartbeat, waitingAgent.ID)
	require.NoError(t, err)

	// Execute
	agents, err := s.MarkStalledAgents(ctx, activityThreshold, heartbeatRecency)
	require.NoError(t, err)
	assert.Len(t, agents, len(stalledActivities), "should only mark running agents with stale activity and recent heartbeat")

	// Verify the returned agents
	returnedIDs := make(map[string]bool)
	// Build a map from ID to pre-stall activity for validation
	expectedPreStall := make(map[string]string)
	for i, id := range expectedIDs {
		expectedPreStall[id] = stalledActivities[i]
	}
	for _, a := range agents {
		returnedIDs[a.ID] = true
		assert.Equal(t, "stalled", a.Activity, "returned agent should have stalled activity")
		assert.Equal(t, "running", a.Phase, "returned agent should still have running phase")
		if expected, ok := expectedPreStall[a.ID]; ok {
			assert.Equal(t, expected, a.StalledFromActivity,
				"stalled_from_activity should record the pre-stall activity for agent %s", a.Slug)
		}
	}
	for _, id := range expectedIDs {
		assert.True(t, returnedIDs[id], "expected agent %s to be in returned set", id)
	}

	// Verify excluded agents were NOT affected
	a, err := s.GetAgent(ctx, recentAgent.ID)
	require.NoError(t, err)
	assert.Equal(t, "idle", a.Activity)

	a, err = s.GetAgent(ctx, offlineAgent.ID)
	require.NoError(t, err)
	assert.Equal(t, "idle", a.Activity, "stale heartbeat agent should not be stalled")

	a, err = s.GetAgent(ctx, completedAgent.ID)
	require.NoError(t, err)
	assert.Equal(t, "completed", a.Activity)

	a, err = s.GetAgent(ctx, limitsAgent.ID)
	require.NoError(t, err)
	assert.Equal(t, "limits_exceeded", a.Activity)

	a, err = s.GetAgent(ctx, stoppedAgent.ID)
	require.NoError(t, err)
	assert.Equal(t, string(state.PhaseStopped), a.Phase)

	a, err = s.GetAgent(ctx, alreadyOfflineAgent.ID)
	require.NoError(t, err)
	assert.Equal(t, "offline", a.Activity)

	a, err = s.GetAgent(ctx, waitingAgent.ID)
	require.NoError(t, err)
	assert.Equal(t, "waiting_for_input", a.Activity, "waiting_for_input agent should not be marked stalled")
}

func TestMarkStalledAgents_Idempotent(t *testing.T) {
	s := setupTestStore(t)
	ctx := context.Background()

	grove := &store.Grove{
		ID:         api.NewUUID(),
		Name:       "Idempotent Stalled Grove",
		Slug:       "idempotent-stalled",
		Visibility: store.VisibilityPrivate,
	}
	require.NoError(t, s.CreateGrove(ctx, grove))

	staleActivityTime := time.Now().Add(-10 * time.Minute)
	recentHeartbeat := time.Now().Add(-30 * time.Second)
	activityThreshold := time.Now().Add(-5 * time.Minute)
	heartbeatRecency := time.Now().Add(-2 * time.Minute)

	agent := &store.Agent{
		ID: api.NewUUID(), Slug: "stalled-idem", Name: "Stalled Idem",
		Template: "claude", GroveID: grove.ID, Phase: string(state.PhaseCreated),
		Visibility: store.VisibilityPrivate,
	}
	require.NoError(t, s.CreateAgent(ctx, agent))
	require.NoError(t, s.UpdateAgentStatus(ctx, agent.ID, store.AgentStatusUpdate{
		Phase: "running", Activity: "thinking",
	}))
	_, err := s.db.ExecContext(ctx,
		"UPDATE agents SET last_activity_event = ?, last_seen = ? WHERE id = ?",
		staleActivityTime, recentHeartbeat, agent.ID)
	require.NoError(t, err)

	// First call should mark it stalled
	agents, err := s.MarkStalledAgents(ctx, activityThreshold, heartbeatRecency)
	require.NoError(t, err)
	assert.Len(t, agents, 1)

	// Second call should return empty (already stalled)
	agents, err = s.MarkStalledAgents(ctx, activityThreshold, heartbeatRecency)
	require.NoError(t, err)
	assert.Len(t, agents, 0, "should not re-mark already stalled agents")
}

func TestMarkStalledAgents_NoAgents(t *testing.T) {
	s := setupTestStore(t)
	ctx := context.Background()

	activityThreshold := time.Now().Add(-5 * time.Minute)
	heartbeatRecency := time.Now().Add(-2 * time.Minute)

	agents, err := s.MarkStalledAgents(ctx, activityThreshold, heartbeatRecency)
	require.NoError(t, err)
	assert.Len(t, agents, 0)
}

func TestUpdateAgentStatus_SetsLastActivityEvent(t *testing.T) {
	s := setupTestStore(t)
	ctx := context.Background()

	grove := &store.Grove{
		ID:         api.NewUUID(),
		Name:       "Activity Event Grove",
		Slug:       "activity-event-grove",
		Visibility: store.VisibilityPrivate,
	}
	require.NoError(t, s.CreateGrove(ctx, grove))

	agent := &store.Agent{
		ID: api.NewUUID(), Slug: "activity-tracker", Name: "Activity Tracker",
		Template: "claude", GroveID: grove.ID, Phase: string(state.PhaseCreated),
		Visibility: store.VisibilityPrivate,
	}
	require.NoError(t, s.CreateAgent(ctx, agent))

	// Activity update should set last_activity_event
	require.NoError(t, s.UpdateAgentStatus(ctx, agent.ID, store.AgentStatusUpdate{
		Phase: "running", Activity: "thinking",
	}))

	a, err := s.GetAgent(ctx, agent.ID)
	require.NoError(t, err)
	assert.False(t, a.LastActivityEvent.IsZero(), "last_activity_event should be set after activity update")
	activityTime := a.LastActivityEvent

	// Heartbeat-only update (no activity) should NOT change last_activity_event
	// Manually set last_activity_event to a known past time first
	pastTime := time.Now().Add(-10 * time.Minute)
	_, err = s.db.ExecContext(ctx, "UPDATE agents SET last_activity_event = ? WHERE id = ?", pastTime, agent.ID)
	require.NoError(t, err)

	require.NoError(t, s.UpdateAgentStatus(ctx, agent.ID, store.AgentStatusUpdate{
		Heartbeat: true,
	}))

	a, err = s.GetAgent(ctx, agent.ID)
	require.NoError(t, err)
	// last_activity_event should still be the past time, not updated
	assert.True(t, a.LastActivityEvent.Before(activityTime),
		"heartbeat-only update should not change last_activity_event")

	// Another activity update should update last_activity_event
	require.NoError(t, s.UpdateAgentStatus(ctx, agent.ID, store.AgentStatusUpdate{
		Activity: "executing",
	}))

	a, err = s.GetAgent(ctx, agent.ID)
	require.NoError(t, err)
	assert.True(t, a.LastActivityEvent.After(pastTime),
		"activity update should update last_activity_event")
}
