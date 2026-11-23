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
	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/experiments"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestPatchAgent_EnvMergePreservesUnseenKeys verifies ptone/scion#5599:
// a PATCH that sets one env key merges by key and leaves other stored keys
// (including keys GET does not return, such as GITHUB_TOKEN) unchanged.
func TestPatchAgent_EnvMergePreservesUnseenKeys(t *testing.T) {
	disp := newReincarnateTestDispatcher()
	srv, s, project, broker := setupReincarnateTestServer(t, disp)
	setBrokerCaps(t, s, broker, &store.BrokerCapabilities{ClearedFields: true})
	ctx := context.Background()

	agent := newReincarnateTestAgent(t, s, project, broker, func(a *store.Agent) {
		a.Phase = string(state.PhaseStopped)
		a.AppliedConfig.Env = map[string]string{
			"KEEP":         "v1",
			"OTHER":        "v2",
			"GITHUB_TOKEN": "ghp_hidden_secret",
		}
		a.AppliedConfig.InlineConfig = &api.ScionConfig{
			Harness: "claude",
			Env: map[string]string{
				"KEEP":  "v1",
				"OTHER": "v2",
			},
		}
		a.AppliedConfig.CreateInputs = &store.AgentCreateInputs{
			InlineConfig: &api.ScionConfig{
				Env: map[string]string{"KEEP": "v1"},
			},
		}
	})

	// Setting one key leaves all omitted keys (including GITHUB_TOKEN) intact.
	rec := patchAgentConfig(t, srv, agent.ID, map[string]interface{}{
		"env": map[string]interface{}{"ADDED": "v3"},
	})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	var resp agentPatchResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.Equal(t, []string{"config.env"}, resp.Disposition.Applied)
	assert.Empty(t, resp.Disposition.HeldForReincarnate)

	got, err := s.GetAgent(ctx, agent.ID)
	require.NoError(t, err)
	assert.Equal(t, map[string]string{
		"KEEP":         "v1",
		"OTHER":        "v2",
		"GITHUB_TOKEN": "ghp_hidden_secret",
		"ADDED":        "v3",
	}, got.AppliedConfig.Env)
	assert.Equal(t, map[string]string{
		"KEEP":  "v1",
		"OTHER": "v2",
		"ADDED": "v3",
	}, got.AppliedConfig.InlineConfig.Env)
	assert.Empty(t, got.AppliedConfig.InlineConfig.ClearedFields)
	assert.Equal(t, map[string]string{
		"KEEP":  "v1",
		"ADDED": "v3",
	}, got.AppliedConfig.CreateInputs.InlineConfig.Env)

	// Updating an existing key preserves all other keys (including GITHUB_TOKEN).
	rec = patchAgentConfig(t, srv, agent.ID, map[string]interface{}{
		"env": map[string]interface{}{"KEEP": "v1-updated"},
	})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	got, err = s.GetAgent(ctx, agent.ID)
	require.NoError(t, err)
	assert.Equal(t, "v1-updated", got.AppliedConfig.Env["KEEP"])
	assert.Equal(t, "v2", got.AppliedConfig.Env["OTHER"])
	assert.Equal(t, "v3", got.AppliedConfig.Env["ADDED"])
	assert.Equal(t, "ghp_hidden_secret", got.AppliedConfig.Env["GITHUB_TOKEN"])

	// On a workstation server, setting an empty string stores "" (does not remove the key).
	srv.workstation = true
	rec = patchAgentConfig(t, srv, agent.ID, map[string]interface{}{
		"env": map[string]interface{}{"EMPTY_VAL": ""},
	})
	srv.workstation = false
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	got, err = s.GetAgent(ctx, agent.ID)
	require.NoError(t, err)
	val, ok := got.AppliedConfig.Env["EMPTY_VAL"]
	assert.True(t, ok, "empty string must be stored, not removed")
	assert.Equal(t, "", val)
	assert.Equal(t, "ghp_hidden_secret", got.AppliedConfig.Env["GITHUB_TOKEN"])

	// Sending an empty env map or top-level null is a no-op and preserves all keys.
	for _, body := range []map[string]interface{}{
		{"env": map[string]interface{}{}},
		{"env": nil},
	} {
		rec = patchAgentConfig(t, srv, agent.ID, body)
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		var noopResp agentPatchResponse
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &noopResp))
		assert.Empty(t, noopResp.Disposition.Applied)
		assert.Empty(t, noopResp.Disposition.HeldForReincarnate)

		after, err := s.GetAgent(ctx, agent.ID)
		require.NoError(t, err)
		assert.Equal(t, got.AppliedConfig.Env, after.AppliedConfig.Env)
		assert.Equal(t, got.AppliedConfig.InlineConfig.Env, after.AppliedConfig.InlineConfig.Env)
	}
}

// TestPatchAgent_EnvExplicitNullRemovesKey verifies ptone/scion#5599:
// removing an env key requires an explicit JSON null value for that key,
// while leaving all other stored keys unchanged.
func TestPatchAgent_EnvExplicitNullRemovesKey(t *testing.T) {
	disp := newReincarnateTestDispatcher()
	srv, s, project, broker := setupReincarnateTestServer(t, disp)
	setBrokerCaps(t, s, broker, &store.BrokerCapabilities{ClearedFields: true})
	ctx := context.Background()

	agent := newReincarnateTestAgent(t, s, project, broker, func(a *store.Agent) {
		a.Phase = string(state.PhaseStopped)
		a.AppliedConfig.Env = map[string]string{
			"KEEP":         "v1",
			"REMOVE_ME":    "old",
			"GITHUB_TOKEN": "ghp_hidden_secret",
		}
		a.AppliedConfig.InlineConfig = &api.ScionConfig{
			Harness: "claude",
			Env: map[string]string{
				"KEEP":      "v1",
				"REMOVE_ME": "old",
			},
		}
		a.AppliedConfig.CreateInputs = &store.AgentCreateInputs{
			InlineConfig: &api.ScionConfig{
				Env: map[string]string{
					"KEEP":      "v1",
					"REMOVE_ME": "old",
				},
			},
		}
	})

	// Explicitly null REMOVE_ME while adding NEW_KEY; KEEP and GITHUB_TOKEN survive.
	rec := patchAgentConfig(t, srv, agent.ID, map[string]interface{}{
		"env": map[string]interface{}{
			"REMOVE_ME": nil,
			"NEW_KEY":   "new",
		},
	})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	var resp agentPatchResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	assert.Equal(t, []string{"config.env"}, resp.Disposition.Applied)
	assert.Empty(t, resp.Disposition.HeldForReincarnate)

	got, err := s.GetAgent(ctx, agent.ID)
	require.NoError(t, err)
	assert.Equal(t, map[string]string{
		"KEEP":         "v1",
		"GITHUB_TOKEN": "ghp_hidden_secret",
		"NEW_KEY":      "new",
	}, got.AppliedConfig.Env)
	assert.Equal(t, map[string]string{
		"KEEP":    "v1",
		"NEW_KEY": "new",
	}, got.AppliedConfig.InlineConfig.Env)
	assert.Equal(t, []string{"env.REMOVE_ME"}, got.AppliedConfig.InlineConfig.ClearedFields)
	assert.Equal(t, map[string]string{
		"KEEP":    "v1",
		"NEW_KEY": "new",
	}, got.AppliedConfig.CreateInputs.InlineConfig.Env)

	// On a broker without ClearedFields, an explicit null removal is reported
	// heldForReincarnate with a warning naming only the removed key.
	setBrokerCaps(t, s, broker, &store.BrokerCapabilities{ClearedFields: false})
	rec = patchAgentConfig(t, srv, agent.ID, map[string]interface{}{
		"env": map[string]interface{}{"KEEP": nil},
	})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var oldBrokerResp agentPatchResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &oldBrokerResp))
	assert.Equal(t, []string{"config.env"}, oldBrokerResp.Disposition.HeldForReincarnate)
	require.NotEmpty(t, oldBrokerResp.Warnings)
	assert.Contains(t, oldBrokerResp.Warnings[0], "KEEP")

	got, err = s.GetAgent(ctx, agent.ID)
	require.NoError(t, err)
	assert.NotContains(t, got.AppliedConfig.Env, "KEEP")
	assert.Equal(t, "new", got.AppliedConfig.Env["NEW_KEY"])
	assert.Equal(t, "ghp_hidden_secret", got.AppliedConfig.Env["GITHUB_TOKEN"])
}

// TestPatchAgent_EnvEditRequiresAttach verifies ptone/scion#5599:
// a config PATCH that sets or removes env keys requires agent.attach in
// addition to agent.update. Uses a project owner and project admin acting on
// another member's agent (matching TestCrossMemberAttach_Matrix: allowed
// agent.update, denied agent.attach). Without attach, the request is refused
// with 403 and nothing changes.
func TestPatchAgent_EnvEditRequiresAttach(t *testing.T) {
	disp := newReincarnateTestDispatcher()
	srv, s, project, broker := setupReincarnateTestServer(t, disp)
	ctx := context.Background()
	srv.createProjectMembersGroup(ctx, project)

	projOwner := makeProjectMemberUser(t, s, project, tid("env-authz-owner"), "Proj Owner", store.GroupMemberRoleOwner)
	projAdmin := makeProjectMemberUser(t, s, project, tid("env-authz-admin"), "Proj Admin", store.GroupMemberRoleAdmin)
	agentOwner := makeProjectMemberUser(t, s, project, tid("env-authz-member"), "Agent Owner", store.GroupMemberRoleMember)

	agent := newReincarnateTestAgent(t, s, project, broker, func(a *store.Agent) {
		a.Name = "original-name"
		a.Phase = string(state.PhaseStopped)
		a.OwnerID = agentOwner.ID
		a.CreatedBy = agentOwner.ID
		a.Ancestry = []string{agentOwner.ID}
		a.AppliedConfig.Model = "old-model"
		a.AppliedConfig.Env = map[string]string{"SECRET_ENV": "keep-me"}
		a.AppliedConfig.InlineConfig = &api.ScionConfig{
			Harness: "claude",
			Model:   "old-model",
			Env:     map[string]string{"SECRET_ENV": "keep-me"},
		}
	})

	// Verify preconditions: project owner and admin hold update but NOT attach
	// on another member's agent; agent owner holds both.
	for _, tc := range []struct {
		name   string
		user   *store.User
		update bool
		attach bool
	}{
		{"project owner", projOwner, true, false},
		{"project admin", projAdmin, true, false},
		{"agent owner", agentOwner, true, true},
	} {
		ident := NewAuthenticatedUser(tc.user.ID, tc.user.Email, tc.user.DisplayName, tc.user.Role, "session")
		assert.Equal(t, tc.update, srv.authzService.CheckAccess(ctx, ident, agentResource(agent), ActionUpdate).Allowed, "%s update", tc.name)
		assert.Equal(t, tc.attach, srv.authzService.CheckAccess(ctx, ident, agentResource(agent), ActionAttach).Allowed, "%s attach", tc.name)
	}

	// Project owner can still PATCH non-env fields on another member's agent.
	rec := doRequestAsUser(t, srv, projOwner, http.MethodPatch, "/api/v1/agents/"+agent.ID, map[string]interface{}{
		"config": map[string]interface{}{"model": "updated-model"},
	})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	// Project owner and admin are refused with 403 on setting, echoing, or
	// removing env keys — on both global and project-scoped routes — and
	// nothing in the agent changes (including co-sent metadata like name).
	for _, caller := range []*store.User{projOwner, projAdmin} {
		for _, tc := range []struct {
			name string
			path string
			body map[string]interface{}
		}{
			{
				name: "global set env + rename",
				path: "/api/v1/agents/" + agent.ID,
				body: map[string]interface{}{
					"name":   "should-not-stick",
					"config": map[string]interface{}{"env": map[string]interface{}{"INJECTED": "1"}},
				},
			},
			{
				name: "global echo existing env (no oracle)",
				path: "/api/v1/agents/" + agent.ID,
				body: map[string]interface{}{
					"config": map[string]interface{}{"env": map[string]interface{}{"SECRET_ENV": "keep-me"}},
				},
			},
			{
				name: "global remove env via null",
				path: "/api/v1/agents/" + agent.ID,
				body: map[string]interface{}{
					"config": map[string]interface{}{"env": map[string]interface{}{"SECRET_ENV": nil}},
				},
			},
			{
				name: "project-scoped set env",
				path: "/api/v1/projects/" + project.ID + "/agents/" + agent.ID,
				body: map[string]interface{}{
					"config": map[string]interface{}{"env": map[string]interface{}{"INJECTED": "1"}},
				},
			},
		} {
			t.Run(caller.DisplayName+"/"+tc.name, func(t *testing.T) {
				rec := doRequestAsUser(t, srv, caller, http.MethodPatch, tc.path, tc.body)
				assert.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())

				stored, err := s.GetAgent(ctx, agent.ID)
				require.NoError(t, err)
				assert.Equal(t, "original-name", stored.Name, "name must not mutate on refused env PATCH")
				assert.Equal(t, map[string]string{"SECRET_ENV": "keep-me"}, stored.AppliedConfig.Env)
				assert.Equal(t, map[string]string{"SECRET_ENV": "keep-me"}, stored.AppliedConfig.InlineConfig.Env)
			})
		}
	}

	// Held edits path also enforces ActionAttach before saving pending config.
	t.Run("held edits refused without attach", func(t *testing.T) {
		cur, err := s.GetAgent(ctx, agent.ID)
		require.NoError(t, err)
		cur.Phase = string(state.PhaseRunning)
		require.NoError(t, s.UpdateAgent(ctx, cur))
		var active []experiments.Experiment
		for _, e := range experiments.Default().All() {
			if e.Name == experiments.AgentEditHeld {
				e.Default = true
			}
			active = append(active, e)
		}
		reg, err := experiments.NewRegistry(active, nil)
		require.NoError(t, err)
		prevReg := srv.experiments
		srv.experiments = reg
		defer func() {
			srv.experiments = prevReg
			cur, _ := s.GetAgent(ctx, agent.ID)
			cur.Phase = string(state.PhaseStopped)
			_ = s.UpdateAgent(ctx, cur)
		}()

		rec := doRequestAsUser(t, srv, projOwner, http.MethodPatch, "/api/v1/agents/"+agent.ID, map[string]interface{}{
			"config": map[string]interface{}{"env": map[string]interface{}{"HELD_INJECT": "1"}},
		})
		assert.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())

		stored, err := s.GetAgent(ctx, agent.ID)
		require.NoError(t, err)
		assert.True(t, stored.PendingConfig.IsEmpty(), "no pending config saved on refused env PATCH")
	})

	// Agent owner (holds both update and attach) succeeds and preserves existing keys.
	rec = doRequestAsUser(t, srv, agentOwner, http.MethodPatch, "/api/v1/agents/"+agent.ID, map[string]interface{}{
		"config": map[string]interface{}{"env": map[string]interface{}{"OWNER_ADDED": "ok"}},
	})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	stored, err := s.GetAgent(ctx, agent.ID)
	require.NoError(t, err)
	assert.Equal(t, map[string]string{
		"SECRET_ENV":  "keep-me",
		"OWNER_ADDED": "ok",
	}, stored.AppliedConfig.Env)
}
