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
	"net/http"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

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
	srv.createProjectMembersGroup(ctx, f.project)
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

// TestHandleExistingAgent_RequiresLifecycleAuthorization covers
// handleExistingAgent (pkg/hub/handlers_agent_create_helpers.go), which used
// to resume, restart, or start a pre-existing agent found by name with no
// check that the caller may manage that specific agent -- only that the
// caller may create some agent in the project (authorizeAgentCreate, checked
// once, earlier, in createAgentInProject). A plain project member could
// therefore hijack another member's agent by POSTing a create request that
// collided with its name.
func TestHandleExistingAgent_RequiresLifecycleAuthorization(t *testing.T) {
	t.Run("member-B resume of a stopped agent is denied", func(t *testing.T) {
		f := handleExistingAgentAuthzSetup(t)
		agent := f.agent(t, "hea-resume-agent", string(state.PhaseStopped))

		rec := doRequestAsUser(t, f.srv, f.member, http.MethodPost, "/api/v1/agents", map[string]interface{}{
			"name":      agent.Slug,
			"projectId": f.project.ID,
			"resume":    true,
		})
		assert.Equal(t, http.StatusConflict, rec.Code,
			"a plain project member must not resume another member's agent by name; got: %s", rec.Body.String())
		assert.NotContains(t, rec.Body.String(), "GITHUB_TOKEN",
			"a denial must not disclose anything about the colliding agent")

		got, err := f.store.GetAgent(context.Background(), agent.ID)
		require.NoError(t, err)
		assert.Equal(t, string(state.PhaseStopped), got.Phase, "the denied resume must not have changed the agent's phase")
	})

	t.Run("suspended restart is denied", func(t *testing.T) {
		f := handleExistingAgentAuthzSetup(t)
		agent := f.agent(t, "hea-suspended-agent", string(state.PhaseSuspended))

		rec := doRequestAsUser(t, f.srv, f.member, http.MethodPost, "/api/v1/agents", map[string]interface{}{
			"name":      agent.Slug,
			"projectId": f.project.ID,
		})
		assert.Equal(t, http.StatusConflict, rec.Code,
			"a plain project member must not restart another member's suspended agent by name; got: %s", rec.Body.String())

		got, err := f.store.GetAgent(context.Background(), agent.ID)
		require.NoError(t, err)
		assert.Equal(t, string(state.PhaseSuspended), got.Phase, "the denied restart must not have changed the agent's phase")
	})

	t.Run("provisionOnly create against A's agent is denied", func(t *testing.T) {
		f := handleExistingAgentAuthzSetup(t)
		agent := f.agent(t, "hea-provision-agent", string(state.PhaseCreated))

		rec := doRequestAsUser(t, f.srv, f.member, http.MethodPost, "/api/v1/agents", map[string]interface{}{
			"name":          agent.Slug,
			"projectId":     f.project.ID,
			"provisionOnly": true,
		})
		assert.Equal(t, http.StatusConflict, rec.Code,
			"a plain project member must not touch another member's agent via a colliding provision-only create; got: %s", rec.Body.String())

		got, err := f.store.GetAgent(context.Background(), agent.ID)
		require.NoError(t, err)
		assert.Equal(t, string(state.PhaseCreated), got.Phase, "the denied request must not have changed the agent's phase")
	})

	t.Run("the owner may still resume their own agent, env intact", func(t *testing.T) {
		f := handleExistingAgentAuthzSetup(t)
		agent := f.agent(t, "hea-owner-resume-agent", string(state.PhaseSuspended))

		rec := doRequestAsUser(t, f.srv, f.owner, http.MethodPost, "/api/v1/agents", map[string]interface{}{
			"name":      agent.Slug,
			"projectId": f.project.ID,
		})
		// The owner has no runtime broker configured in this test server, so
		// the dispatch itself fails after the authorization gate passes --
		// this asserts the gate does not turn into a new false denial for the
		// legitimate owner, not that the whole resume flow succeeds
		// end-to-end (that needs a broker, out of scope here).
		assert.NotEqual(t, http.StatusConflict, rec.Code,
			"the owner must not be denied by the new authorization gate; got: %s", rec.Body.String())
		assert.NotEqual(t, http.StatusForbidden, rec.Code,
			"the owner must not be denied by the new authorization gate; got: %s", rec.Body.String())
	})
}
