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
	"testing"
	"time"

	"github.com/ptone/scion-agent/pkg/api"
	"github.com/ptone/scion-agent/pkg/store"
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
		Status:     store.AgentStatusPending,
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
	retrieved.Status = store.AgentStatusRunning
	err = s.UpdateAgent(ctx, retrieved)
	require.NoError(t, err)
	assert.Equal(t, int64(2), retrieved.StateVersion)

	// Verify update
	retrieved, err = s.GetAgent(ctx, agent.ID)
	require.NoError(t, err)
	assert.Equal(t, "Updated Agent", retrieved.Name)
	assert.Equal(t, store.AgentStatusRunning, retrieved.Status)

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
			Slug:    api.Slugify("agent-" + string(rune('a'+i))),
			Name:       "Agent " + string(rune('A'+i)),
			Template:   "claude",
			GroveID:    grove.ID,
			Status:     store.AgentStatusRunning,
			Visibility: store.VisibilityPrivate,
		}
		if i%2 == 0 {
			agent.Status = store.AgentStatusStopped
		}
		require.NoError(t, s.CreateAgent(ctx, agent))
	}

	// List all
	result, err := s.ListAgents(ctx, store.AgentFilter{}, store.ListOptions{})
	require.NoError(t, err)
	assert.Equal(t, 5, result.TotalCount)
	assert.Len(t, result.Items, 5)

	// List by status
	result, err = s.ListAgents(ctx, store.AgentFilter{Status: store.AgentStatusRunning}, store.ListOptions{})
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
		Slug:    "test-agent",
		Name:       "Test Agent",
		Template:   "claude",
		GroveID:    grove.ID,
		Status:     store.AgentStatusPending,
		Visibility: store.VisibilityPrivate,
	}
	require.NoError(t, s.CreateAgent(ctx, agent))

	// Update status
	err := s.UpdateAgentStatus(ctx, agent.ID, store.AgentStatusUpdate{
		Status:          store.AgentStatusRunning,
		SessionStatus:   "waiting",
		ContainerStatus: "Up 5 minutes",
	})
	require.NoError(t, err)

	// Verify
	retrieved, err := s.GetAgent(ctx, agent.ID)
	require.NoError(t, err)
	assert.Equal(t, store.AgentStatusRunning, retrieved.Status)
	assert.Equal(t, "waiting", retrieved.SessionStatus)
	assert.Equal(t, "Up 5 minutes", retrieved.ContainerStatus)
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

	// Get by git remote
	retrieved, err = s.GetGroveByGitRemote(ctx, "github.com/org/repo")
	require.NoError(t, err)
	assert.Equal(t, grove.ID, retrieved.ID)

	// Test unique constraint on git remote
	duplicate := &store.Grove{
		ID:         api.NewUUID(),
		Name:       "Duplicate",
		Slug:       "duplicate",
		GitRemote:  "github.com/org/repo",
		Visibility: store.VisibilityPrivate,
	}
	err = s.CreateGrove(ctx, duplicate)
	assert.ErrorIs(t, err, store.ErrAlreadyExists)

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

func TestGroveList(t *testing.T) {
	s := setupTestStore(t)
	ctx := context.Background()

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
	}

	// List all
	result, err := s.ListGroves(ctx, store.GroveFilter{}, store.ListOptions{})
	require.NoError(t, err)
	assert.Equal(t, 3, result.TotalCount)

	// List by visibility
	result, err = s.ListGroves(ctx, store.GroveFilter{Visibility: store.VisibilityPublic}, store.ListOptions{})
	require.NoError(t, err)
	assert.Equal(t, 1, result.TotalCount)
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
		Slug:    "test-agent",
		Name:       "Test Agent",
		Template:   "claude",
		GroveID:    grove.ID,
		Status:     store.AgentStatusRunning,
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
