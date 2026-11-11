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
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/require"
)

func handleExistingAgentAuthzSetup(t *testing.T) *handleExistingAgentAuthzFixture {
	t.Helper()
	srv, s := testServer(t)
	ctx := context.Background()
	f := &handleExistingAgentAuthzFixture{srv: srv, store: s}

	f.owner = &store.User{
		ID: tid("hea-owner"), Email: "hea-owner@test.com",
		DisplayName: "Owner", Role: store.UserRoleMember, Status: "active", Created: time.Now(),
	}
	require.NoError(t, s.CreateUser(ctx, f.owner))
	ensureHubMembership(ctx, s, f.owner.ID)

	f.member = &store.User{
		ID: tid("hea-member"), Email: "hea-member@test.com",
		DisplayName: "Member", Role: store.UserRoleMember, Status: "active", Created: time.Now(),
	}
	require.NoError(t, s.CreateUser(ctx, f.member))
	ensureHubMembership(ctx, s, f.member.ID)

	f.project = &store.Project{
		ID: tid("hea-proj"), Name: "HEA Project", Slug: "hea-project",
		OwnerID: f.owner.ID, CreatedBy: f.owner.ID, Created: time.Now(), Updated: time.Now(),
	}
	require.NoError(t, s.CreateProject(ctx, f.project))
	srv.seedProjectCreatorMembership(ctx, f.project)
	createTestUserWithProjectRole(t, s, f.member.ID, f.member.Email, f.project.ID, store.ProjectRoleMember)

	// A runtime broker is required for createAgent to get past
	// resolveRuntimeBroker before it ever looks up an existing agent by name;
	// the denial this fixture tests happens later, inside handleExistingAgent.
	broker := &store.RuntimeBroker{
		ID: tid("hea-broker"), Name: "hea-broker", Slug: "hea-broker",
		Status: store.BrokerStatusOnline, AutoProvide: true,
		Created: time.Now(), Updated: time.Now(),
	}
	require.NoError(t, s.CreateRuntimeBroker(ctx, broker))
	require.NoError(t, s.AddProjectProvider(ctx, &store.ProjectProvider{
		ProjectID: f.project.ID, BrokerID: broker.ID, BrokerName: broker.Name,
		Status: store.BrokerStatusOnline,
	}))
	f.project.DefaultRuntimeBrokerID = broker.ID
	require.NoError(t, s.UpdateProject(ctx, f.project))

	return f
}

// handleExistingAgentAuthzFixture provides an owner and a plain project
// member (not owner/admin) in the same project, plus a helper to create the
// owner's agent at a given phase. project-member's curated permission set
// (seed.go projectMemberCuratedPermissionIDs) grants agent.create but not
// agent.lifecycle -- a plain member cannot start/stop/restart another
// member's agent through /agents/{id}/start, but before this fix could reach
// the same effect through POST /api/v1/agents against an existing agent's
// name, via handleExistingAgent's resume/restart branches, which performed
// no authorization of their own.
type handleExistingAgentAuthzFixture struct {
	srv     *Server
	store   store.Store
	project *store.Project
	owner   *store.User
	member  *store.User
}

// agent creates the owner's agent at the given phase, with a name that
// api.ValidateAgentName maps to itself so req.Name in the POST body matches
// the stored slug directly.
func (f *handleExistingAgentAuthzFixture) agent(t *testing.T, name, phase string) *store.Agent {
	t.Helper()
	a := &store.Agent{
		ID: tid("hea-agent-" + name), Slug: name, Name: name,
		ProjectID: f.project.ID, Phase: phase,
		CreatedBy: f.owner.ID, OwnerID: f.owner.ID,
		AppliedConfig: &store.AgentAppliedConfig{
			Env: map[string]string{"PLAIN_VAR": "plain-value", "GITHUB_TOKEN": "ghp_should_never_leak"},
			InlineConfig: &api.ScionConfig{
				Env: map[string]string{"INLINE_PLAIN_VAR": "inline-plain-value", "GITHUB_TOKEN": "ghp_should_never_leak"},
			},
		},
	}
	require.NoError(t, f.store.CreateAgent(context.Background(), a))
	return a
}
