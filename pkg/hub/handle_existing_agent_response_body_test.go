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

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/require"
)

// TestHandleExistingAgentResponseBody_EnvHiding pins env visibility for both
// AppliedConfig.Env and AppliedConfig.InlineConfig.Env in the resume
// response body handleExistingAgent writes (handlers_agent_create_helpers.go),
// which goes through redactedAgentCopy just like the other agent-response
// call sites: visible (minus GITHUB_TOKEN) for the owner, hidden for a
// caller who can manage the agent's lifecycle but cannot attach to it. Each
// case must fail if either field's hiding is removed.
func TestHandleExistingAgentResponseBody_EnvHiding(t *testing.T) {
	t.Run("owner resume sees env minus GITHUB_TOKEN", func(t *testing.T) {
		f := handleExistingAgentAuthzSetup(t)
		f.srv.SetDispatcher(&createAgentDispatcher{})
		agent := f.agent(t, "hea-body-owner-resume", string(state.PhaseSuspended))

		rec := doRequestAsUser(t, f.srv, f.owner, http.MethodPost, "/api/v1/agents", map[string]interface{}{
			"name":      agent.Slug,
			"projectId": f.project.ID,
		})
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

		var resp struct {
			Agent rawAppliedConfigView `json:"agent"`
		}
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
		assertEnvVisibleMinusGitHubToken(t, resp.Agent, "owner resume handleExistingAgent")
	})

	t.Run("a project admin (lifecycle-capable, no attach) resumes but does not see env", func(t *testing.T) {
		f := handleExistingAgentAuthzSetup(t)
		f.srv.SetDispatcher(&createAgentDispatcher{})
		agent := f.agent(t, "hea-body-admin-resume", string(state.PhaseSuspended))

		ctx := context.Background()
		projectAdmin := &store.User{
			ID: tid("hea-project-admin"), Email: "hea-project-admin@test.com",
			DisplayName: "Project Admin", Role: store.UserRoleMember, Status: "active",
		}
		require.NoError(t, f.store.CreateUser(ctx, projectAdmin))
		ensureHubMembership(ctx, f.store, projectAdmin.ID)
		// Project admin gets every capability except ActionAttach/ActionPortAccess
		// (ownerAdminExcludedActions, capabilities.go) unless they own or are an
		// ancestor of the resource -- neither is true here, so this admin can
		// manage the agent's lifecycle (and so reach a 200) but must not see env.
		createTestUserWithProjectRole(t, f.store, projectAdmin.ID, projectAdmin.Email, f.project.ID, store.ProjectRoleAdmin)

		rec := doRequestAsUser(t, f.srv, projectAdmin, http.MethodPost, "/api/v1/agents", map[string]interface{}{
			"name":      agent.Slug,
			"projectId": f.project.ID,
		})
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

		var resp struct {
			Agent rawAppliedConfigView `json:"agent"`
		}
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
		assertEnvHidden(t, resp.Agent, "project admin resume handleExistingAgent")
	})
}
