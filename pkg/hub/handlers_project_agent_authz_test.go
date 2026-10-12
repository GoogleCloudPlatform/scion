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
	"encoding/json"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

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
