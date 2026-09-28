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

	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestProjectAgentGet_AuthorizationGap covers getProjectAgent
// (pkg/hub/handlers_projects_core.go), which used to serialize the raw agent
// (including, pre-redaction, its full env) to any authenticated caller with
// no per-agent or per-project check at all. Uses projectAgentAuthzFixture,
// defined alongside TestListProjectAgentsRequiresAuthorization.
func TestProjectAgentGet_AuthorizationGap(t *testing.T) {
	t.Run("non-member user is denied", func(t *testing.T) {
		f := projectAgentAuthzSetup(t)
		rec := doRequestAsUser(t, f.srv, f.nonMember, http.MethodGet, f.targetPath(), nil)
		assert.Equal(t, http.StatusForbidden, rec.Code,
			"a hub user who is not a project member must not read a project agent's record; got: %s", rec.Body.String())
	})

	t.Run("project member sees the agent", func(t *testing.T) {
		f := projectAgentAuthzSetup(t)
		rec := doRequestAsUser(t, f.srv, f.member, http.MethodGet, f.targetPath(), nil)
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	})

	t.Run("admin sees the agent", func(t *testing.T) {
		f := projectAgentAuthzSetup(t)
		rec := doRequestAsUser(t, f.srv, f.admin, http.MethodGet, f.targetPath(), nil)
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	})

	t.Run("another agent's token (same project) is denied", func(t *testing.T) {
		// agent.read has no AgentScopes mapping in the permission registry
		// (see TestBypassAgents_LegitimateFlowsStillWork's "agent reads a
		// project peer" case for the same rule on the global route): an
		// agent JWT cannot read another agent's record by ID, sibling or not.
		f := projectAgentAuthzSetup(t)
		rec := doRequestWithAgentToken(t, f.srv, http.MethodGet, f.targetPath(), nil, f.callerToken(t))
		assert.Equal(t, http.StatusForbidden, rec.Code,
			"an agent token must not read a different agent's record; got: %s", rec.Body.String())
	})

	t.Run("an agent's own token reading itself is allowed", func(t *testing.T) {
		// Preserves this route's pre-existing, tested contract
		// (TestReadEndpoint_ProjectScopedAgents_WithReadScope_Allowed): unlike
		// the global GET /api/v1/agents/{id} route (CO1), this route exempts
		// an agent reading its own record from the agent.read permission
		// check, which would otherwise deny it (agent.read has no
		// AgentScopes mapping).
		f := projectAgentAuthzSetup(t)
		selfPath := "/api/v1/projects/" + f.project.ID + "/agents/" + f.caller.ID
		rec := doRequestWithAgentToken(t, f.srv, http.MethodGet, selfPath, nil, f.callerToken(t))
		assert.Equal(t, http.StatusOK, rec.Code,
			"an agent reading its own record via the project-scoped route must be allowed; got: %s", rec.Body.String())
	})

	t.Run("a cross-project agent token 404s, not 403s", func(t *testing.T) {
		f := projectAgentAuthzSetup(t)
		rec := doRequestWithAgentToken(t, f.srv, http.MethodGet, f.targetPath(), nil, f.strangerToken(t))
		assert.Equal(t, http.StatusNotFound, rec.Code,
			"cross-project agent read must 404 to avoid disclosing existence; got: %s", rec.Body.String())
	})

	t.Run("env is redacted for a non-attach-capable member", func(t *testing.T) {
		// Regression guard for the structural redaction: getProjectAgent must
		// still go through the same env gate as the global getAgent route.
		f := projectAgentAuthzSetup(t)
		rec := doRequestAsUser(t, f.srv, f.member, http.MethodGet, f.targetPath(), nil)
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		var got store.Agent
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &got))
		if got.AppliedConfig != nil {
			_, hasToken := got.AppliedConfig.Env["GITHUB_TOKEN"]
			assert.False(t, hasToken, "GITHUB_TOKEN must never be returned")
		}
	})
}

// TestProjectAgentUpdate_AuthorizationGap covers updateProjectAgent
// (pkg/hub/handlers_projects_core.go), which used to apply a hand-rolled
// subset of field updates with no authorization check at all.
func TestProjectAgentUpdate_AuthorizationGap(t *testing.T) {
	t.Run("non-member user is denied and the row is unchanged", func(t *testing.T) {
		f := projectAgentAuthzSetup(t)
		rec := doRequestAsUser(t, f.srv, f.nonMember, http.MethodPatch, f.targetPath(),
			map[string]interface{}{"name": "hijacked"})
		assert.Equal(t, http.StatusForbidden, rec.Code,
			"a hub user who is not a project member must not update a project agent; got: %s", rec.Body.String())

		got, err := f.store.GetAgent(context.Background(), f.target.ID)
		require.NoError(t, err)
		assert.NotEqual(t, "hijacked", got.Name, "the denied update must not have been applied")
	})

	t.Run("project member can update", func(t *testing.T) {
		f := projectAgentAuthzSetup(t)
		rec := doRequestAsUser(t, f.srv, f.member, http.MethodPatch, f.targetPath(),
			map[string]interface{}{"name": "renamed-by-member"})
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

		got, err := f.store.GetAgent(context.Background(), f.target.ID)
		require.NoError(t, err)
		assert.Equal(t, "renamed-by-member", got.Name)
	})

	t.Run("admin can update", func(t *testing.T) {
		f := projectAgentAuthzSetup(t)
		rec := doRequestAsUser(t, f.srv, f.admin, http.MethodPatch, f.targetPath(),
			map[string]interface{}{"name": "renamed-by-admin"})
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	})

	t.Run("another agent's token (same project) is denied and the row is unchanged", func(t *testing.T) {
		f := projectAgentAuthzSetup(t)
		rec := doRequestWithAgentToken(t, f.srv, http.MethodPatch, f.targetPath(),
			map[string]interface{}{"name": "hijacked"}, f.callerToken(t))
		assert.Equal(t, http.StatusForbidden, rec.Code,
			"an agent token must not update a different agent's record; got: %s", rec.Body.String())

		got, err := f.store.GetAgent(context.Background(), f.target.ID)
		require.NoError(t, err)
		assert.NotEqual(t, "hijacked", got.Name, "the denied update must not have been applied")
	})

	t.Run("cross-project agent token is denied and the row is unchanged", func(t *testing.T) {
		// applyAgentUpdate's authorize(ActionUpdate) denies any agent
		// identity outright (agent.update has no AgentScopes mapping), same
		// project or not, so this 403s exactly like the same-project case
		// above and like the global PATCH /api/v1/agents/{id} route.
		f := projectAgentAuthzSetup(t)
		rec := doRequestWithAgentToken(t, f.srv, http.MethodPatch, f.targetPath(),
			map[string]interface{}{"name": "hijacked"}, f.strangerToken(t))
		assert.Equal(t, http.StatusForbidden, rec.Code,
			"a cross-project agent token must not update an agent in another project; got: %s", rec.Body.String())

		got, err := f.store.GetAgent(context.Background(), f.target.ID)
		require.NoError(t, err)
		assert.NotEqual(t, "hijacked", got.Name, "the denied update must not have been applied")
	})
}
