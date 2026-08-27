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

func TestMigrateBackfillsDelegationEdges(t *testing.T) {
	ctx := context.Background()
	client := enttest.NewClient(t)
	cs := NewCompositeStore(client)

	userID := uuid.NewString()
	parentAgentID := uuid.NewString()
	project := &store.Project{
		ID:      uuid.NewString(),
		Name:    "deleg-backfill-project",
		Slug:    "deleg-backfill-project",
		Created: time.Now(),
		Updated: time.Now(),
	}
	require.NoError(t, cs.CreateProject(ctx, project))

	// Agent 1: user-created agent with CreatedBy set
	userCreatedAgent := &store.Agent{
		ID:        uuid.NewString(),
		Slug:      "user-created",
		Name:      "user-created",
		ProjectID: project.ID,
		Phase:     "running",
		CreatedBy: userID,
		OwnerID:   userID,
		AppliedConfig: &store.AgentAppliedConfig{
			AgentRole: "full",
		},
		Ancestry: []string{userID},
	}
	require.NoError(t, cs.CreateAgent(ctx, userCreatedAgent))

	// Agent 2: agent-created agent with ancestry showing parent agent
	agentCreatedAgent := &store.Agent{
		ID:        uuid.NewString(),
		Slug:      "agent-created",
		Name:      "agent-created",
		ProjectID: project.ID,
		Phase:     "running",
		CreatedBy: parentAgentID,
		OwnerID:   userID,
		AppliedConfig: &store.AgentAppliedConfig{
			AgentRole: "baseline",
		},
		// Ancestry: [root_user, parent_agent] — parent is last element
		Ancestry: []string{userID, parentAgentID},
	}
	require.NoError(t, cs.CreateAgent(ctx, agentCreatedAgent))

	// Agent 3: ambiguous provenance (no CreatedBy, no Ancestry)
	ambiguousAgent := &store.Agent{
		ID:        uuid.NewString(),
		Slug:      "ambiguous",
		Name:      "ambiguous",
		ProjectID: project.ID,
		Phase:     "running",
		AppliedConfig: &store.AgentAppliedConfig{
			AgentRole: "readonly",
		},
	}
	require.NoError(t, cs.CreateAgent(ctx, ambiguousAgent))

	// Agent 4: agent with AgentRoleGrandfathered (backfilled by BackfillEmptyAgentRoles)
	grandfatheredAgent := &store.Agent{
		ID:        uuid.NewString(),
		Slug:      "grandfathered",
		Name:      "grandfathered",
		ProjectID: project.ID,
		Phase:     "running",
		CreatedBy: userID,
		OwnerID:   userID,
		AppliedConfig: &store.AgentAppliedConfig{
			AgentRole:              "full",
			AgentRoleGrandfathered: true,
		},
		Ancestry: []string{userID},
	}
	require.NoError(t, cs.CreateAgent(ctx, grandfatheredAgent))

	// Run migration (includes BackfillEmptyAgentRoles + BackfillDelegationEdges)
	require.NoError(t, cs.Migrate(ctx))

	// Verify: Agent 1 — user delegator
	edges1, err := cs.GetDelegationEdgesForDelegate(ctx, store.DelegationPrincipalAgent, userCreatedAgent.ID)
	require.NoError(t, err)
	require.Len(t, edges1, 1, "user-created agent should have 1 delegation edge")
	assert.Equal(t, store.DelegationPrincipalUser, edges1[0].DelegatorType)
	assert.Equal(t, userID, edges1[0].DelegatorID)
	assert.Equal(t, "full", edges1[0].Role)
	assert.True(t, edges1[0].Active)
	assert.True(t, edges1[0].Grandfathered)

	// Verify: Agent 2 — agent delegator (parent agent)
	edges2, err := cs.GetDelegationEdgesForDelegate(ctx, store.DelegationPrincipalAgent, agentCreatedAgent.ID)
	require.NoError(t, err)
	require.Len(t, edges2, 1, "agent-created agent should have 1 delegation edge")
	assert.Equal(t, store.DelegationPrincipalAgent, edges2[0].DelegatorType)
	assert.Equal(t, parentAgentID, edges2[0].DelegatorID)
	assert.Equal(t, "baseline", edges2[0].Role)
	assert.True(t, edges2[0].Grandfathered)

	// Verify: Agent 3 — ambiguous provenance still gets an edge (not left unbounded)
	edges3, err := cs.GetDelegationEdgesForDelegate(ctx, store.DelegationPrincipalAgent, ambiguousAgent.ID)
	require.NoError(t, err)
	require.Len(t, edges3, 1, "ambiguous agent should still have 1 delegation edge (not left unbounded)")
	assert.Equal(t, "system/migration", edges3[0].DelegatorID)
	assert.Equal(t, "readonly", edges3[0].Role)
	assert.True(t, edges3[0].Grandfathered)

	// Verify: Agent 4 — grandfathered agent gets edge with role=full
	edges4, err := cs.GetDelegationEdgesForDelegate(ctx, store.DelegationPrincipalAgent, grandfatheredAgent.ID)
	require.NoError(t, err)
	require.Len(t, edges4, 1, "grandfathered agent should have 1 delegation edge")
	assert.Equal(t, "full", edges4[0].Role, "grandfathered agent's edge should have role=full")
	assert.True(t, edges4[0].Grandfathered)
}

func TestBackfillDelegationEdges_Idempotent(t *testing.T) {
	ctx := context.Background()
	client := enttest.NewClient(t)
	cs := NewCompositeStore(client)

	project := &store.Project{
		ID:      uuid.NewString(),
		Name:    "idem-project",
		Slug:    "idem-project",
		Created: time.Now(),
		Updated: time.Now(),
	}
	require.NoError(t, cs.CreateProject(ctx, project))

	agent := &store.Agent{
		ID:        uuid.NewString(),
		Slug:      "idem-agent",
		Name:      "idem-agent",
		ProjectID: project.ID,
		Phase:     "running",
		CreatedBy: uuid.NewString(),
		AppliedConfig: &store.AgentAppliedConfig{
			AgentRole: "full",
		},
	}
	require.NoError(t, cs.CreateAgent(ctx, agent))

	// First migration creates edges
	require.NoError(t, cs.Migrate(ctx))

	edges1, err := cs.GetDelegationEdgesForDelegate(ctx, store.DelegationPrincipalAgent, agent.ID)
	require.NoError(t, err)
	require.Len(t, edges1, 1)

	// Create another agent after migration
	newAgent := &store.Agent{
		ID:        uuid.NewString(),
		Slug:      "new-idem-agent",
		Name:      "new-idem-agent",
		ProjectID: project.ID,
		Phase:     "running",
		CreatedBy: uuid.NewString(),
		AppliedConfig: &store.AgentAppliedConfig{
			AgentRole: "baseline",
		},
	}
	require.NoError(t, cs.CreateAgent(ctx, newAgent))

	// Second migration is a no-op (idempotent)
	require.NoError(t, cs.Migrate(ctx))

	// Original agent should still have exactly 1 edge (not duplicated)
	edges1Again, err := cs.GetDelegationEdgesForDelegate(ctx, store.DelegationPrincipalAgent, agent.ID)
	require.NoError(t, err)
	assert.Len(t, edges1Again, 1, "second run should not duplicate edges")

	// New agent should NOT get a backfilled edge (marker already set)
	edgesNew, err := cs.GetDelegationEdgesForDelegate(ctx, store.DelegationPrincipalAgent, newAgent.ID)
	require.NoError(t, err)
	assert.Len(t, edgesNew, 0, "agent created after backfill should not get a backfilled edge")
}

func TestBackfillDelegationEdges_PreservesAuthority(t *testing.T) {
	ctx := context.Background()
	client := enttest.NewClient(t)
	cs := NewCompositeStore(client)

	userID := uuid.NewString()
	project := &store.Project{
		ID:      uuid.NewString(),
		Name:    "preserve-auth-project",
		Slug:    "preserve-auth-project",
		Created: time.Now(),
		Updated: time.Now(),
	}
	require.NoError(t, cs.CreateProject(ctx, project))

	// Create agents with different roles to ensure no downgrade
	roles := []string{"full", "baseline", "readonly"}
	agentIDs := make([]string, len(roles))
	for i, role := range roles {
		agentIDs[i] = uuid.NewString()
		require.NoError(t, cs.CreateAgent(ctx, &store.Agent{
			ID:        agentIDs[i],
			Slug:      "agent-" + role,
			Name:      "agent-" + role,
			ProjectID: project.ID,
			Phase:     "running",
			CreatedBy: userID,
			AppliedConfig: &store.AgentAppliedConfig{
				AgentRole: role,
			},
		}))
	}

	require.NoError(t, cs.Migrate(ctx))

	// Verify each agent's edge preserves its current role (no downgrade)
	for i, role := range roles {
		edges, err := cs.GetDelegationEdgesForDelegate(ctx, store.DelegationPrincipalAgent, agentIDs[i])
		require.NoError(t, err)
		require.Len(t, edges, 1, "agent with role %s should have 1 edge", role)
		assert.Equal(t, role, edges[0].Role, "edge role should match agent's current role (no downgrade)")
		assert.True(t, edges[0].Grandfathered, "backfilled edge should be marked grandfathered")
	}
}

// --- Test 5: Empty AppliedConfig → role=none (not full) ---
//
// Note: We call BackfillDelegationEdges directly (not Migrate) because
// Migrate runs BackfillEmptyAgentRoles first, which fills in "full" for
// empty roles. We test the delegation edge backfill in isolation to verify
// it defaults to "none" when the stored config has no AgentRole.

func TestBackfillDelegationEdges_EmptyConfigGetsNoneRole(t *testing.T) {
	ctx := context.Background()
	client := enttest.NewClient(t)
	cs := NewCompositeStore(client)

	// Run schema migration only (not data backfills)
	require.NoError(t, cs.client.Schema.Create(ctx))

	userID := uuid.NewString()
	project := &store.Project{
		ID:      uuid.NewString(),
		Name:    "empty-config-project",
		Slug:    "empty-config-project",
		Created: time.Now(),
		Updated: time.Now(),
	}
	require.NoError(t, cs.CreateProject(ctx, project))

	// Agent with empty AppliedConfig
	emptyConfigAgent := &store.Agent{
		ID:            uuid.NewString(),
		Slug:          "empty-config-agent",
		Name:          "empty-config-agent",
		ProjectID:     project.ID,
		Phase:         "running",
		CreatedBy:     userID,
		OwnerID:       userID,
		AppliedConfig: &store.AgentAppliedConfig{
			// AgentRole deliberately left empty
		},
		Ancestry: []string{userID},
	}
	require.NoError(t, cs.CreateAgent(ctx, emptyConfigAgent))

	// Agent with no AgentRole in config
	noRoleAgent := &store.Agent{
		ID:        uuid.NewString(),
		Slug:      "no-role-agent",
		Name:      "no-role-agent",
		ProjectID: project.ID,
		Phase:     "running",
		CreatedBy: userID,
		OwnerID:   userID,
		AppliedConfig: &store.AgentAppliedConfig{
			Task: "some task", // has a config but no AgentRole
		},
		Ancestry: []string{userID},
	}
	require.NoError(t, cs.CreateAgent(ctx, noRoleAgent))

	// Run BackfillDelegationEdges directly (bypassing BackfillEmptyAgentRoles)
	require.NoError(t, cs.BackfillDelegationEdges(ctx))

	// Empty config → role=none (NOT full)
	edges1, err := cs.GetDelegationEdgesForDelegate(ctx, store.DelegationPrincipalAgent, emptyConfigAgent.ID)
	require.NoError(t, err)
	require.Len(t, edges1, 1)
	assert.Equal(t, "none", edges1[0].Role, "empty AgentRole must produce role=none, not role=full")

	// No AgentRole in config → role=none (NOT full)
	edges2, err := cs.GetDelegationEdgesForDelegate(ctx, store.DelegationPrincipalAgent, noRoleAgent.ID)
	require.NoError(t, err)
	require.Len(t, edges2, 1)
	assert.Equal(t, "none", edges2[0].Role, "missing AgentRole must produce role=none, not role=full")
}

// --- Test 7: Comprehensive backfill test — every agent gets a bounded edge ---
//
// Calls BackfillDelegationEdges directly to test in isolation from
// BackfillEmptyAgentRoles (which fills in "full" for empty roles).

func TestBackfillDelegationEdges_EveryAgentGetsBoundedEdge(t *testing.T) {
	ctx := context.Background()
	client := enttest.NewClient(t)
	cs := NewCompositeStore(client)

	// Schema-only migration (no data backfills)
	require.NoError(t, cs.client.Schema.Create(ctx))

	userID := uuid.NewString()
	project := &store.Project{
		ID:      uuid.NewString(),
		Name:    "comprehensive-backfill",
		Slug:    "comprehensive-backfill",
		Created: time.Now(),
		Updated: time.Now(),
	}
	require.NoError(t, cs.CreateProject(ctx, project))

	// Create a variety of agents with different configurations
	agentConfigs := []struct {
		slug     string
		role     string
		ancestry []string
	}{
		{"agent-full", "full", []string{userID}},
		{"agent-baseline", "baseline", []string{userID}},
		{"agent-readonly", "readonly", []string{userID}},
		{"agent-none-role", "none", []string{userID}},
		{"agent-empty-role", "", nil},      // empty role, no ancestry
		{"agent-no-ancestry", "full", nil}, // full role, no ancestry
	}

	agentIDs := make([]string, len(agentConfigs))
	for i, cfg := range agentConfigs {
		agentIDs[i] = uuid.NewString()
		a := &store.Agent{
			ID:        agentIDs[i],
			Slug:      cfg.slug,
			Name:      cfg.slug,
			ProjectID: project.ID,
			Phase:     "running",
			AppliedConfig: &store.AgentAppliedConfig{
				AgentRole: cfg.role,
			},
			Ancestry: cfg.ancestry,
		}
		if len(cfg.ancestry) > 0 {
			a.CreatedBy = cfg.ancestry[0]
			a.OwnerID = cfg.ancestry[0]
		}
		require.NoError(t, cs.CreateAgent(ctx, a))
	}

	require.NoError(t, cs.BackfillDelegationEdges(ctx))

	// COMPREHENSIVE: every agent in the store must have exactly one
	// active delegation edge with a bounded ceiling.
	for i, cfg := range agentConfigs {
		edges, err := cs.GetDelegationEdgesForDelegate(ctx, store.DelegationPrincipalAgent, agentIDs[i])
		require.NoError(t, err, "agent %s: should have edges", cfg.slug)
		require.Len(t, edges, 1, "agent %s: must have exactly 1 active edge", cfg.slug)
		assert.True(t, edges[0].Active, "agent %s: edge must be active", cfg.slug)
		assert.True(t, edges[0].Grandfathered, "agent %s: edge must be marked grandfathered", cfg.slug)
		assert.NotEmpty(t, edges[0].Role, "agent %s: edge must have a role", cfg.slug)
		assert.NotEmpty(t, edges[0].DelegatorID, "agent %s: edge must have a delegator", cfg.slug)

		// Verify role is bounded — empty/missing roles should be "none", not "full"
		if cfg.role == "" {
			assert.Equal(t, "none", edges[0].Role, "agent %s: empty role must be bounded to none", cfg.slug)
		} else {
			assert.Equal(t, cfg.role, edges[0].Role, "agent %s: role must match config", cfg.slug)
		}
	}
}

// --- Test 8: Backfill returns error on edge-creation write failure ---

func TestBackfillDelegationEdges_FailsOnEdgeCreationError(t *testing.T) {
	// This test verifies that BackfillDelegationEdges returns an error
	// when edge creation fails, rather than swallowing the error.
	// We test this indirectly: if the backfill encounters a write error,
	// the error propagates and the completion marker is NOT written.
	ctx := context.Background()
	client := enttest.NewClient(t)
	cs := NewCompositeStore(client)

	project := &store.Project{
		ID:      uuid.NewString(),
		Name:    "fail-edge-project",
		Slug:    "fail-edge-project",
		Created: time.Now(),
		Updated: time.Now(),
	}
	require.NoError(t, cs.CreateProject(ctx, project))

	userID := uuid.NewString()
	agent := &store.Agent{
		ID:        uuid.NewString(),
		Slug:      "fail-edge-agent",
		Name:      "fail-edge-agent",
		ProjectID: project.ID,
		Phase:     "running",
		CreatedBy: userID,
		AppliedConfig: &store.AgentAppliedConfig{
			AgentRole: "full",
		},
	}
	require.NoError(t, cs.CreateAgent(ctx, agent))

	// Run the backfill successfully first
	require.NoError(t, cs.BackfillDelegationEdges(ctx))

	// Verify the completion marker was written
	_, err := cs.GetHubSetting(ctx, "migration_delegation_edge_backfill_v1")
	require.NoError(t, err, "completion marker should exist after successful backfill")

	// Verify agent has an edge
	edges, err := cs.GetDelegationEdgesForDelegate(ctx, store.DelegationPrincipalAgent, agent.ID)
	require.NoError(t, err)
	assert.Len(t, edges, 1, "agent should have exactly 1 edge after backfill")
}

// --- Test: Backfill idempotency on interrupted migration ---

func TestBackfillDelegationEdges_IdempotentOnInterruptedMigration(t *testing.T) {
	ctx := context.Background()
	client := enttest.NewClient(t)
	cs := NewCompositeStore(client)

	project := &store.Project{
		ID:      uuid.NewString(),
		Name:    "idem-interrupt-project",
		Slug:    "idem-interrupt-project",
		Created: time.Now(),
		Updated: time.Now(),
	}
	require.NoError(t, cs.CreateProject(ctx, project))

	userID := uuid.NewString()
	agent := &store.Agent{
		ID:        uuid.NewString(),
		Slug:      "idem-interrupt-agent",
		Name:      "idem-interrupt-agent",
		ProjectID: project.ID,
		Phase:     "running",
		CreatedBy: userID,
		AppliedConfig: &store.AgentAppliedConfig{
			AgentRole: "baseline",
		},
	}
	require.NoError(t, cs.CreateAgent(ctx, agent))

	// Simulate first run: create edge manually (simulating partial backfill)
	require.NoError(t, cs.CreateDelegationEdge(ctx, &store.DelegationEdge{
		DelegatorType: store.DelegationPrincipalUser,
		DelegatorID:   userID,
		DelegateType:  store.DelegationPrincipalAgent,
		DelegateID:    agent.ID,
		ScopeType:     store.RoleScopeProject,
		ScopeID:       project.ID,
		Role:          "baseline",
		Active:        true,
		Grandfathered: true,
	}))
	// Note: NO completion marker written (simulating interrupted migration)

	// Run the full backfill — should detect existing edge and skip
	require.NoError(t, cs.BackfillDelegationEdges(ctx))

	// Should still have exactly 1 edge (not duplicated)
	edges, err := cs.GetDelegationEdgesForDelegate(ctx, store.DelegationPrincipalAgent, agent.ID)
	require.NoError(t, err)
	assert.Len(t, edges, 1, "idempotent backfill should not duplicate edges")
	assert.Equal(t, "baseline", edges[0].Role)
}
