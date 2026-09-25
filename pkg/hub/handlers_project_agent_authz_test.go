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
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// projectAgentAuthzFixture exercises the project-scoped agent routes
// (GET/PATCH /api/v1/projects/{id}/agents/{agentId} and the project-scoped
// list) against every caller kind that matters: a project member, a hub user
// who is not a project member, a hub admin, another agent's JWT (same
// project, different agent), and an agent JWT scoped to a different project
// entirely. Shared by the list, get, and update authorization test files.
type projectAgentAuthzFixture struct {
	srv       *Server
	store     store.Store
	project   *store.Project
	other     *store.Project
	member    *store.User  // project owner/member
	nonMember *store.User  // hub member, not a project member
	admin     *store.User  // hub admin (system-scope bypass)
	target    *store.Agent // the agent under test, in project
	caller    *store.Agent // a different agent in the same project
	stranger  *store.Agent // an agent in a different project
}

func projectAgentAuthzSetup(t *testing.T) *projectAgentAuthzFixture {
	t.Helper()
	srv, s := testServer(t)
	ctx := context.Background()
	f := &projectAgentAuthzFixture{srv: srv, store: s}

	f.member = &store.User{
		ID: tid("paa-member"), Email: "paa-member@test.com",
		DisplayName: "Member", Role: store.UserRoleMember, Status: "active", Created: time.Now(),
	}
	require.NoError(t, s.CreateUser(ctx, f.member))
	ensureHubMembership(ctx, s, f.member.ID)

	f.nonMember = &store.User{
		ID: tid("paa-nonmember"), Email: "paa-nonmember@test.com",
		DisplayName: "NonMember", Role: store.UserRoleMember, Status: "active", Created: time.Now(),
	}
	require.NoError(t, s.CreateUser(ctx, f.nonMember))
	ensureHubMembership(ctx, s, f.nonMember.ID)

	f.admin = &store.User{
		ID: tid("paa-admin"), Email: "paa-admin@test.com",
		DisplayName: "Admin", Role: store.UserRoleAdmin, Status: "active", Created: time.Now(),
	}
	require.NoError(t, s.CreateUser(ctx, f.admin))
	ensureHubMembership(ctx, s, f.admin.ID)
	saRD, err := s.GetRoleDefinitionByName(ctx, store.SystemRoleSuperAdmin, store.RoleScopeSystem)
	require.NoError(t, err)
	_, err = s.CreateRoleBinding(ctx, &store.RoleBinding{
		RoleDefinitionID: saRD.ID,
		PrincipalType:    store.RoleBindingPrincipalUser,
		PrincipalID:      f.admin.ID,
		ScopeType:        store.RoleScopeSystem,
		CreatedBy:        store.SystemReconcileCreatedBy,
	})
	require.NoError(t, err)

	f.project = &store.Project{
		ID: tid("paa-proj"), Name: "PAA Project", Slug: "paa-project",
		OwnerID: f.member.ID, CreatedBy: f.member.ID, Created: time.Now(), Updated: time.Now(),
	}
	require.NoError(t, s.CreateProject(ctx, f.project))
	srv.createProjectMembersGroup(ctx, f.project)

	f.other = &store.Project{
		ID: tid("paa-other-proj"), Name: "PAA Other Project", Slug: "paa-other-project",
		OwnerID: f.member.ID, CreatedBy: f.member.ID, Created: time.Now(), Updated: time.Now(),
	}
	require.NoError(t, s.CreateProject(ctx, f.other))
	srv.createProjectMembersGroup(ctx, f.other)

	mk := func(name, projectID string) *store.Agent {
		a := &store.Agent{
			ID: tid(name), Slug: tid(name), Name: name,
			ProjectID: projectID, Phase: string(state.PhaseStopped),
			CreatedBy: f.member.ID, OwnerID: f.member.ID,
			AppliedConfig: &store.AgentAppliedConfig{
				Env: map[string]string{"PLAIN_VAR": "plain-value", "GITHUB_TOKEN": "ghp_should_never_leak"},
			},
		}
		require.NoError(t, s.CreateAgent(ctx, a))
		return a
	}
	f.target = mk("paa-target", f.project.ID)
	f.caller = mk("paa-caller", f.project.ID)
	f.stranger = mk("paa-stranger", f.other.ID)

	return f
}

// callerToken mints an agent JWT for f.caller -- a different agent from
// f.target, in the same project.
func (f *projectAgentAuthzFixture) callerToken(t *testing.T, scopes ...AgentTokenScope) string {
	t.Helper()
	svc := f.srv.GetAgentTokenService()
	require.NotNil(t, svc)
	allScopes := append([]AgentTokenScope{ScopeProjectRead}, scopes...)
	tok, err := svc.GenerateAgentToken(f.caller.ID, f.caller.ProjectID, allScopes, nil)
	require.NoError(t, err)
	return tok
}

// strangerToken mints an agent JWT for f.stranger -- an agent in a different
// project from f.target.
func (f *projectAgentAuthzFixture) strangerToken(t *testing.T, scopes ...AgentTokenScope) string {
	t.Helper()
	svc := f.srv.GetAgentTokenService()
	require.NotNil(t, svc)
	allScopes := append([]AgentTokenScope{ScopeProjectRead}, scopes...)
	tok, err := svc.GenerateAgentToken(f.stranger.ID, f.stranger.ProjectID, allScopes, nil)
	require.NoError(t, err)
	return tok
}

func (f *projectAgentAuthzFixture) targetPath() string {
	return "/api/v1/projects/" + f.project.ID + "/agents/" + f.target.ID
}

// TestListProjectAgentsRequiresAuthorization is F1:
// GET /api/v1/projects/{id}/agents (listProjectAgents) had no authorization
// check for a user identity at all -- a hub user who was not a project
// member got every agent record in the project.
func TestListProjectAgentsRequiresAuthorization(t *testing.T) {
	listPath := func(f *projectAgentAuthzFixture) string {
		return "/api/v1/projects/" + f.project.ID + "/agents"
	}

	t.Run("non-member user is denied", func(t *testing.T) {
		f := projectAgentAuthzSetup(t)
		rec := doRequestAsUser(t, f.srv, f.nonMember, http.MethodGet, listPath(f), nil)
		assert.Equal(t, http.StatusForbidden, rec.Code,
			"a hub user who is not a project member must not list a project's agents; got: %s", rec.Body.String())
	})

	t.Run("project member sees the project's agents", func(t *testing.T) {
		f := projectAgentAuthzSetup(t)
		rec := doRequestAsUser(t, f.srv, f.member, http.MethodGet, listPath(f), nil)
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		var resp ListAgentsResponse
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
		ids := map[string]bool{}
		for _, a := range resp.Agents {
			ids[a.ID] = true
		}
		assert.True(t, ids[f.target.ID], "member must see the target agent")
		assert.True(t, ids[f.caller.ID], "member must see the caller agent")
	})

	t.Run("admin is unaffected", func(t *testing.T) {
		f := projectAgentAuthzSetup(t)
		rec := doRequestAsUser(t, f.srv, f.admin, http.MethodGet, listPath(f), nil)
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	})

	t.Run("an agent token still works for sibling listing", func(t *testing.T) {
		// Agent-JWT callers are exempted from the new project-level
		// agent.list gate and remain gated solely by checkAgentReadScope, to
		// preserve the existing sibling-agent-listing use case.
		f := projectAgentAuthzSetup(t)
		rec := doRequestWithAgentToken(t, f.srv, http.MethodGet, listPath(f), nil, f.callerToken(t))
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		var resp ListAgentsResponse
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
		ids := map[string]bool{}
		for _, a := range resp.Agents {
			ids[a.ID] = true
		}
		assert.True(t, ids[f.target.ID], "agent token must still see project agents (sibling listing)")
	})

	t.Run("a cross-project agent token is denied", func(t *testing.T) {
		// checkAgentReadScope only checks that the token carries the
		// project:read scope bit; it never compared the token's own project
		// to the {id} in the URL. A token minted for f.other used to be able
		// to list f.project's agents here regardless. listProjectAgents now
		// checks the match explicitly for an agent identity, matching the
		// isolation check getProjectAgent already applies and the blanket
		// denial updateProjectAgent already gets (via applyAgentUpdate's
		// authorize(ActionUpdate), which has no agent-identity grant path at
		// all, same project or not).
		f := projectAgentAuthzSetup(t)
		rec := doRequestWithAgentToken(t, f.srv, http.MethodGet, listPath(f), nil, f.strangerToken(t))
		assert.Equal(t, http.StatusNotFound, rec.Code, rec.Body.String())
	})
}
