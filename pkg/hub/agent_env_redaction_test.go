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
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestAgentResponseOmitsNonPlainEnv covers the read-side redaction gate
// (redactAppliedConfigEnvForResponse) across the three response surfaces
// that serialize agent.AppliedConfig: single-agent GET, list, and PATCH.
//
// The agent's owner (who could reach the same values by attaching to the
// agent's own container) sees every key except GITHUB_TOKEN, which is
// withheld unconditionally, from every caller, regardless of classification.
// A different project member who holds the project-owner role -- and can
// therefore manage this agent's lifecycle, per miller79/scion#88 -- but has
// no attach-equivalent grant on it, must not see the map at all: this is the
// exact cross-user disclosure this gate exists to close.
func TestAgentResponseOmitsNonPlainEnv(t *testing.T) {
	srv, s, alice, _, project := setupDemoPolicyTest(t)
	ctx := context.Background()

	agent := &store.Agent{
		ID:        tid("agent-env-redaction"),
		Slug:      "agent-env-redaction",
		Name:      "Env Redaction Agent",
		ProjectID: project.ID,
		OwnerID:   alice.ID,
		Phase:     string(state.PhaseStopped),
		AppliedConfig: &store.AgentAppliedConfig{
			Env: map[string]string{
				"PLAIN_VAR":    "plain-value",
				"GITHUB_TOKEN": "ghp_should_never_be_returned",
			},
		},
	}
	require.NoError(t, s.CreateAgent(ctx, agent))

	// Bob holds the project-owner role but is not this agent's owner and has
	// no relationship grant on it -- the miller79/scion#88 split means he can
	// still manage its lifecycle, but must not be able to attach to it.
	bob := makeProjectMemberUser(t, s, project, tid("user-bob-env-redaction"), "Bob", store.GroupMemberRoleOwner)
	createTestUserWithProjectRole(t, s, bob.ID, bob.Email, project.ID, store.ProjectRoleOwner)

	t.Run("owner GET sees plain env but never GITHUB_TOKEN", func(t *testing.T) {
		rec := doRequestAsUser(t, srv, alice, http.MethodGet, "/api/v1/agents/"+agent.ID, nil)
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		var got store.Agent
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &got))
		require.NotNil(t, got.AppliedConfig)
		assert.Equal(t, "plain-value", got.AppliedConfig.Env["PLAIN_VAR"])
		_, hasToken := got.AppliedConfig.Env["GITHUB_TOKEN"]
		assert.False(t, hasToken, "GITHUB_TOKEN must never be returned, even to the owner")
	})

	t.Run("non-attach-capable project member GET sees no env at all", func(t *testing.T) {
		rec := doRequestAsUser(t, srv, bob, http.MethodGet, "/api/v1/agents/"+agent.ID, nil)
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		var got store.Agent
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &got))
		if got.AppliedConfig != nil {
			assert.Empty(t, got.AppliedConfig.Env, "non-attach-capable caller must not see any persisted env")
		}
	})

	t.Run("non-attach-capable project member list sees no env at all", func(t *testing.T) {
		rec := doRequestAsUser(t, srv, bob, http.MethodGet, "/api/v1/agents?projectId="+project.ID, nil)
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		var listResp ListAgentsResponse
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &listResp))

		found := false
		for _, a := range listResp.Agents {
			if a.ID != agent.ID {
				continue
			}
			found = true
			if a.AppliedConfig != nil {
				assert.Empty(t, a.AppliedConfig.Env, "non-attach-capable caller must not see any persisted env in list")
			}
		}
		assert.True(t, found, "expected to find the agent in bob's project-scoped list")
	})

	t.Run("non-attach-capable project member PATCH response sees no env at all", func(t *testing.T) {
		rec := doRequestAsUser(t, srv, bob, http.MethodPatch, "/api/v1/agents/"+agent.ID, map[string]any{
			"taskSummary": "updated by project owner",
		})
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		var got store.Agent
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &got))
		if got.AppliedConfig != nil {
			assert.Empty(t, got.AppliedConfig.Env, "non-attach-capable caller must not see any persisted env in the PATCH response")
		}
	})

	// Sanity: bob's PATCH must not have wiped alice's stored data, since the
	// redaction is response-only and must not touch the persisted row.
	t.Run("PATCH by non-owner does not alter the stored env", func(t *testing.T) {
		stored, err := s.GetAgent(ctx, agent.ID)
		require.NoError(t, err)
		require.NotNil(t, stored.AppliedConfig)
		assert.Equal(t, "plain-value", stored.AppliedConfig.Env["PLAIN_VAR"])
		assert.Equal(t, "ghp_should_never_be_returned", stored.AppliedConfig.Env["GITHUB_TOKEN"],
			"the persisted row is untouched by response redaction; cleanup migration handles storage separately")
	})
}
