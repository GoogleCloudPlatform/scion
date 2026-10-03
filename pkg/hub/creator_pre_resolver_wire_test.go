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

package hub

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/GoogleCloudPlatform/scion/pkg/store"
)

// TestCreateAuthenticatedDispatcher_CreatorSkillPreResolver pins the
// production wiring in CreateAuthenticatedDispatcher that installs the
// creator-based skill pre-resolver used by start and restart.
//
// Dispatcher tests elsewhere call SetCreatorSkillPreResolver on a dispatcher
// they build themselves, and the resolver tests call
// preResolveAgentSkillsAsCreator directly, so neither notices if the factory
// stops installing it or installs the caller-based resolver in its place.
// This test builds the dispatcher through the same factory the running Hub
// uses and dispatches alice's agent as bob, who cannot read the skill:
//   - start and restart must carry alice's successful resolution;
//   - create still resolves as the dispatching caller, so bob's dispatch
//     reports the skill as not found. This keeps the two setters from being
//     swapped without a test failure.
func TestCreateAuthenticatedDispatcher_CreatorSkillPreResolver(t *testing.T) {
	srv, s, alice, bob, project := setupSkillAuthzTest(t)
	ctx := context.Background()

	skill := createTestSkill(t, s, "wired-creator-skill", store.SkillScopeProject, project.ID, alice.ID)
	publishTestSkillVersion(t, s, skill)
	uri := "skill://scion/project/" + project.ID + "/wired-creator-skill"

	broker := &store.RuntimeBroker{
		ID:       tid("wire-broker"),
		Name:     "wire-broker",
		Slug:     "wire-broker",
		Endpoint: "http://localhost:9800",
		Status:   store.BrokerStatusOnline,
	}
	require.NoError(t, s.CreateRuntimeBroker(ctx, broker))
	require.NoError(t, s.AddProjectProvider(ctx, &store.ProjectProvider{
		ProjectID:  project.ID,
		BrokerID:   broker.ID,
		BrokerName: broker.Name,
		LocalPath:  "/home/user/projects/wire/.scion",
		Status:     store.BrokerStatusOnline,
	}))

	agent := dispatchTestAgent(alice.ID, project.ID, uri)
	agent.Name = "wired-agent"
	agent.Slug = "wired-agent"
	agent.RuntimeBrokerID = broker.ID

	d := srv.CreateAuthenticatedDispatcher()
	mockClient := &mockRuntimeBrokerClient{}
	d.client = mockClient

	bobIdent := NewAuthenticatedUser(bob.ID, bob.Email, bob.DisplayName, bob.Role, "api")
	bobCtx := contextWithIdentity(ctx, bobIdent)

	assertCreatorResolution := func(t *testing.T, got *ResolveSkillsResponse) {
		t.Helper()
		require.NotNil(t, got, "the production dispatcher must attach PreResolvedSkills on start/restart")
		assert.Empty(t, got.Errors, "start/restart must resolve as the creator (alice), not the dispatching caller (bob)")
		require.Len(t, got.Resolved, 1)
		assert.Equal(t, uri, got.Resolved[0].URI)
	}

	t.Run("start resolves as creator", func(t *testing.T) {
		require.NoError(t, d.DispatchAgentStart(bobCtx, agent, "", false))
		assertCreatorResolution(t, mockClient.lastStartExtras.PreResolvedSkills)
	})

	t.Run("restart resolves as creator", func(t *testing.T) {
		require.NoError(t, d.DispatchAgentRestart(bobCtx, agent))
		assertCreatorResolution(t, mockClient.lastRestartExtras.PreResolvedSkills)
	})

	t.Run("create resolves as dispatching caller", func(t *testing.T) {
		req, err := d.buildCreateRequest(bobCtx, agent, "test")
		require.NoError(t, err)
		require.NotNil(t, req.PreResolvedSkills)
		assert.Empty(t, req.PreResolvedSkills.Resolved)
		require.Len(t, req.PreResolvedSkills.Errors, 1)
		assert.Equal(t, "not_found", req.PreResolvedSkills.Errors[0].Code)
	})
}
