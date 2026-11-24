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
	"reflect"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/experiments"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/go-jose/go-jose/v4/jwt"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestShellAgentPatch_S7_EveryFieldTable verifies S7 (ptone/scion#8812 §3.4)
// across EVERY top-level PATCH /agents/{id} field and EVERY api.ScionConfig
// JSON field:
//   - Non-launch metadata (name, labels, annotations, taskSummary, stateVersion)
//     keeps agent.update: both project admin (no attach) and owner succeed.
//   - Harness switches to or from "shell" (harness_config, harness,
//     default_harness_config) are refused outright (400) for every caller.
//   - Every other launch-shaping field (every config.* field, explicitTimezone,
//     gcp_identity, agentRole) requires owner-human ActionAttach: project admin
//     without attach gets 403, agent principals (including ancestors) get 403,
//     and the owner human passes the S7 gate.
func TestShellAgentPatch_S7_EveryFieldTable(t *testing.T) {
	srv, s := testServer(t)
	project, broker := seedShellTestProjectAndBroker(t, s)

	owner := seedShellTestUser(t, s, "s7-owner@test.com", "S7 Owner")
	admin := seedShellTestUser(t, s, "s7-admin@test.com", "S7 Admin")
	bindProjectRole(t, s, project.ID, owner.ID, "project-member")
	// project-admin holds agent.update and agent.lifecycle, but NOT agent.attach.
	bindProjectRole(t, s, project.ID, admin.ID, "project-admin")

	parentLLM := seedShellTestAgent(t, s, project.ID, broker.ID, "s7-parent-llm", owner.ID, "claude", state.PhaseRunning, []string{owner.ID})
	ancestorIdent := &agentIdentityWrapper{&AgentTokenClaims{
		Claims:    jwt.Claims{Subject: parentLLM.ID},
		ProjectID: project.ID,
		Ancestry:  []string{owner.ID, parentLLM.ID},
		Scopes:    []AgentTokenScope{ScopeProjectRead, ScopeAgentLifecycle},
	}}

	type fieldCase struct {
		field       string
		configKey   string // non-empty for api.ScionConfig JSON fields (completeness check)
		body        string
		adminStatus int
		ownerStatus int // 0 means assert != 403 (e.g. 400 validation/immutable, not authz)
	}

	cases := []fieldCase{
		// Top-level non-launch metadata (keeps agent.update)
		{field: "name", body: `{"name":"Renamed Shell"}`, adminStatus: http.StatusOK, ownerStatus: http.StatusOK},
		{field: "labels", body: `{"labels":{"team":"ops"}}`, adminStatus: http.StatusOK, ownerStatus: http.StatusOK},
		{field: "annotations", body: `{"annotations":{"note":"hi"}}`, adminStatus: http.StatusOK, ownerStatus: http.StatusOK},
		{field: "taskSummary", body: `{"taskSummary":"updated summary"}`, adminStatus: http.StatusOK, ownerStatus: http.StatusOK},
		{field: "stateVersion", body: `{"stateVersion":0}`, adminStatus: http.StatusOK, ownerStatus: http.StatusOK},

		// Top-level launch-shaping fields (owner-human ActionAttach required)
		{field: "explicitTimezone", body: `{"explicitTimezone":"UTC"}`, adminStatus: http.StatusForbidden, ownerStatus: http.StatusOK},
		{field: "gcp_identity", body: `{"gcp_identity":{"metadata_mode":"block"}}`, adminStatus: http.StatusForbidden, ownerStatus: http.StatusOK},
		{field: "agentRole", body: `{"agentRole":"baseline"}`, adminStatus: http.StatusForbidden, ownerStatus: http.StatusOK},

		// Harness switch fields on a shell agent (refused outright 400 for every caller)
		{field: "config.harness", configKey: "harness", body: `{"config":{"harness":"claude"}}`, adminStatus: http.StatusBadRequest, ownerStatus: http.StatusBadRequest},
		{field: "config.harness_config", configKey: "harness_config", body: `{"config":{"harness_config":"claude"}}`, adminStatus: http.StatusBadRequest, ownerStatus: http.StatusBadRequest},
		{field: "config.default_harness_config", configKey: "default_harness_config", body: `{"config":{"default_harness_config":"claude"}}`, adminStatus: http.StatusBadRequest, ownerStatus: http.StatusBadRequest},

		// Every remaining api.ScionConfig JSON field (owner-human ActionAttach required)
		{field: "config.image", configKey: "image", body: `{"config":{"image":"custom-shell:v2"}}`, adminStatus: http.StatusForbidden, ownerStatus: http.StatusOK},
		{field: "config.env", configKey: "env", body: `{"config":{"env":{"FOO":"bar"}}}`, adminStatus: http.StatusForbidden, ownerStatus: http.StatusOK},
		{field: "config.volumes", configKey: "volumes", body: `{"config":{"volumes":[{"source":"/tmp/a","target":"/mnt/a"}]}}`, adminStatus: http.StatusForbidden, ownerStatus: http.StatusBadRequest},
		{field: "config.user", configKey: "user", body: `{"config":{"user":"dev"}}`, adminStatus: http.StatusForbidden, ownerStatus: http.StatusOK},
		{field: "config.command_args", configKey: "command_args", body: `{"config":{"command_args":["--flag"]}}`, adminStatus: http.StatusForbidden, ownerStatus: http.StatusOK},
		{field: "config.task_flag", configKey: "task_flag", body: `{"config":{"task_flag":"-p"}}`, adminStatus: http.StatusForbidden, ownerStatus: http.StatusOK},
		{field: "config.task", configKey: "task", body: `{"config":{"task":"run evil"}}`, adminStatus: http.StatusForbidden, ownerStatus: http.StatusBadRequest},
		{field: "config.model", configKey: "model", body: `{"config":{"model":"opus"}}`, adminStatus: http.StatusForbidden, ownerStatus: http.StatusOK},
		{field: "config.thinking_level", configKey: "thinking_level", body: `{"config":{"thinking_level":50}}`, adminStatus: http.StatusForbidden, ownerStatus: http.StatusOK},
		{field: "config.auth_selectedType", configKey: "auth_selectedType", body: `{"config":{"auth_selectedType":"api-key"}}`, adminStatus: http.StatusForbidden, ownerStatus: http.StatusBadRequest},
		{field: "config.resources", configKey: "resources", body: `{"config":{"resources":{"limits":{"cpu":"2"}}}}`, adminStatus: http.StatusForbidden, ownerStatus: http.StatusOK},
		{field: "config.kubernetes", configKey: "kubernetes", body: `{"config":{"kubernetes":{"imagePullPolicy":"Always"}}}`, adminStatus: http.StatusForbidden, ownerStatus: http.StatusOK},
		{field: "config.max_duration", configKey: "max_duration", body: `{"config":{"max_duration":"2h"}}`, adminStatus: http.StatusForbidden, ownerStatus: http.StatusOK},
		{field: "config.max_turns", configKey: "max_turns", body: `{"config":{"max_turns":10}}`, adminStatus: http.StatusForbidden, ownerStatus: http.StatusBadRequest},
		{field: "config.max_model_calls", configKey: "max_model_calls", body: `{"config":{"max_model_calls":10}}`, adminStatus: http.StatusForbidden, ownerStatus: http.StatusBadRequest},
		{field: "config.telemetry", configKey: "telemetry", body: `{"config":{"telemetry":{"enabled":true}}}`, adminStatus: http.StatusForbidden, ownerStatus: http.StatusOK},
		{field: "config.mcp_servers", configKey: "mcp_servers", body: `{"config":{"mcp_servers":{"s":{"command":"echo"}}}}`, adminStatus: http.StatusForbidden, ownerStatus: http.StatusOK},
		{field: "config.services", configKey: "services", body: `{"config":{"services":[{"name":"svc","command":["sleep","1"]}]}}`, adminStatus: http.StatusForbidden, ownerStatus: http.StatusOK},
		{field: "config.secrets", configKey: "secrets", body: `{"config":{"secrets":[{"key":"K"}]}}`, adminStatus: http.StatusForbidden, ownerStatus: http.StatusOK},
		{field: "config.skills", configKey: "skills", body: `{"config":{"skills":[{"name":"sk"}]}}`, adminStatus: http.StatusForbidden, ownerStatus: http.StatusOK},
		{field: "config.system_prompt", configKey: "system_prompt", body: `{"config":{"system_prompt":"hi"}}`, adminStatus: http.StatusForbidden, ownerStatus: http.StatusBadRequest},
		{field: "config.agent_instructions", configKey: "agent_instructions", body: `{"config":{"agent_instructions":"hi"}}`, adminStatus: http.StatusForbidden, ownerStatus: http.StatusBadRequest},
		{field: "config.config_dir", configKey: "config_dir", body: `{"config":{"config_dir":".evil"}}`, adminStatus: http.StatusForbidden, ownerStatus: http.StatusBadRequest},
		{field: "config.detached", configKey: "detached", body: `{"config":{"detached":false}}`, adminStatus: http.StatusForbidden, ownerStatus: http.StatusBadRequest},
		{field: "config.message_mode", configKey: "message_mode", body: `{"config":{"message_mode":"project"}}`, adminStatus: http.StatusForbidden, ownerStatus: http.StatusBadRequest},
		{field: "config.hub", configKey: "hub", body: `{"config":{"hub":{"endpoint":"https://evil"}}}`, adminStatus: http.StatusForbidden, ownerStatus: http.StatusBadRequest},
		{field: "config.persistence", configKey: "persistence", body: `{"config":{"persistence":"ephemeral"}}`, adminStatus: http.StatusForbidden, ownerStatus: http.StatusBadRequest},
		{field: "config.clone_depth", configKey: "clone_depth", body: `{"config":{"clone_depth":"full"}}`, adminStatus: http.StatusForbidden, ownerStatus: http.StatusBadRequest},
		{field: "config.agent_plugins", configKey: "agent_plugins", body: `{"config":{"agent_plugins":[{"path":"p"}]}}`, adminStatus: http.StatusForbidden, ownerStatus: http.StatusBadRequest},
		{field: "config.branch", configKey: "branch", body: `{"config":{"branch":"other"}}`, adminStatus: http.StatusForbidden, ownerStatus: http.StatusBadRequest},
		{field: "config.explicit_workspace", configKey: "explicit_workspace", body: `{"config":{"explicit_workspace":true}}`, adminStatus: http.StatusForbidden, ownerStatus: http.StatusBadRequest},
		{field: "config.empty_per_agent_workspace", configKey: "empty_per_agent_workspace", body: `{"config":{"empty_per_agent_workspace":true}}`, adminStatus: http.StatusForbidden, ownerStatus: http.StatusBadRequest},
		{field: "config.cleared_fields", configKey: "cleared_fields", body: `{"config":{"cleared_fields":["image"]}}`, adminStatus: http.StatusForbidden, ownerStatus: http.StatusBadRequest},
		{field: "config.changed_fields", configKey: "changed_fields", body: `{"config":{"changed_fields":["skills"]}}`, adminStatus: http.StatusForbidden, ownerStatus: http.StatusBadRequest},
	}

	// Exhaustiveness check: every JSON-encoded field of api.ScionConfig MUST
	// appear in cases above so no new config field can slip past S7 untested.
	covered := make(map[string]bool, len(cases))
	for _, tc := range cases {
		if tc.configKey != "" {
			covered[tc.configKey] = true
		}
	}
	typ := reflect.TypeOf(api.ScionConfig{})
	for i := 0; i < typ.NumField(); i++ {
		key := scionConfigJSONKey(typ.Field(i))
		if key == "" {
			continue
		}
		assert.True(t, covered[key], "api.ScionConfig field %q is missing from S7 PATCH field table", key)
	}

	for i, tc := range cases {
		t.Run(tc.field, func(t *testing.T) {
			slug := "s7-shell-" + strings.ReplaceAll(tc.field, ".", "-")
			shellAgent := seedShellTestAgent(t, s, project.ID, broker.ID, slug, owner.ID, config.ShellHarnessConfigName, state.PhaseCreated, []string{owner.ID, parentLLM.ID})
			_ = i

			// 1. Project admin (no attach).
			rec := doRequestAsUser(t, srv, admin, http.MethodPatch, "/api/v1/agents/"+shellAgent.ID, json.RawMessage(tc.body))
			assert.Equal(t, tc.adminStatus, rec.Code, "admin PATCH %s: %s", tc.field, rec.Body.String())

			// 2. Ancestor agent principal: denied on launch-shaping fields (403)
			//    or harness switch (400).
			if tc.adminStatus == http.StatusForbidden || tc.adminStatus == http.StatusBadRequest {
				recAgent := doRequestAsIdentity(t, srv, ancestorIdent, http.MethodPatch, "/api/v1/agents/"+shellAgent.ID, json.RawMessage(tc.body))
				assert.Contains(t, []int{http.StatusForbidden, http.StatusBadRequest}, recAgent.Code,
					"ancestor agent PATCH %s: %s", tc.field, recAgent.Body.String())
			}

			// 3. Owner human.
			recOwner := doRequestAsUser(t, srv, owner, http.MethodPatch, "/api/v1/agents/"+shellAgent.ID, json.RawMessage(tc.body))
			assert.Equal(t, tc.ownerStatus, recOwner.Code, "owner PATCH %s: %s", tc.field, recOwner.Body.String())
		})
	}

	// Harness switch TO "shell" on a non-shell agent is refused outright (400),
	// even for the owner.
	t.Run("non-shell to shell refused outright", func(t *testing.T) {
		llmAgent := seedShellTestAgent(t, s, project.ID, broker.ID, "s7-llm-to-shell", owner.ID, "claude", state.PhaseCreated, []string{owner.ID})
		for _, body := range []string{
			`{"config":{"harness_config":"shell"}}`,
			`{"config":{"harness":"shell"}}`,
			`{"config":{"default_harness_config":"shell"}}`,
		} {
			rec := doRequestAsUser(t, srv, owner, http.MethodPatch, "/api/v1/agents/"+llmAgent.ID, json.RawMessage(body))
			assert.Equal(t, http.StatusBadRequest, rec.Code, "body=%s resp=%s", body, rec.Body.String())
			assert.Contains(t, rec.Body.String(), shellHarnessSwitchRefusalMessage)
		}
	})

	// Unchanged echo of harness_config:"shell" alongside an owner image edit succeeds.
	t.Run("shell unchanged echo allowed for owner, refused for admin", func(t *testing.T) {
		shellAgent := seedShellTestAgent(t, s, project.ID, broker.ID, "s7-shell-echo", owner.ID, config.ShellHarnessConfigName, state.PhaseCreated, []string{owner.ID})
		body := json.RawMessage(`{"config":{"harness_config":"shell","harness":"shell","image":"custom:v3"}}`)

		recAdmin := doRequestAsUser(t, srv, admin, http.MethodPatch, "/api/v1/agents/"+shellAgent.ID, body)
		assert.Equal(t, http.StatusForbidden, recAdmin.Code, recAdmin.Body.String())

		recOwner := doRequestAsUser(t, srv, owner, http.MethodPatch, "/api/v1/agents/"+shellAgent.ID, body)
		assert.Equal(t, http.StatusOK, recOwner.Code, recOwner.Body.String())
	})
}

func TestShellAgentReincarnate_S7(t *testing.T) {
	srv, s := testServer(t)
	setExperimentOverrides(t, srv, map[string]bool{experiments.ReincarnateHarnessSwitch: true})
	project, broker := seedShellTestProjectAndBroker(t, s)
	broker.Capabilities = &store.BrokerCapabilities{
		Reprovision:                  true,
		ReprovisionResetHomeRuntimes: []string{"docker"},
	}
	require.NoError(t, s.UpdateRuntimeBroker(context.Background(), broker))

	owner := seedShellTestUser(t, s, "reinc-owner@test.com", "Reinc Owner")
	admin := seedShellTestUser(t, s, "reinc-admin@test.com", "Reinc Admin")
	bindProjectRole(t, s, project.ID, owner.ID, "project-member")
	bindProjectRole(t, s, project.ID, admin.ID, "project-admin")

	shellAgent := seedShellTestAgent(t, s, project.ID, broker.ID, "reinc-shell", owner.ID, config.ShellHarnessConfigName, state.PhaseStopped, []string{owner.ID})
	llmAgent := seedShellTestAgent(t, s, project.ID, broker.ID, "reinc-llm", owner.ID, "claude", state.PhaseStopped, []string{owner.ID})

	// 1. Harness switch FROM shell -> claude refused outright (400) for owner and admin.
	for _, u := range []*store.User{owner, admin} {
		rec := doRequestAsUser(t, srv, u, http.MethodPost, "/api/v1/agents/"+shellAgent.ID+"/reincarnate", ReincarnateAgentRequest{
			HarnessConfig: "claude",
			DryRun:        true,
		})
		assert.Equal(t, http.StatusBadRequest, rec.Code, "user=%s resp=%s", u.Email, rec.Body.String())
		assert.Contains(t, rec.Body.String(), shellHarnessSwitchRefusalMessage)
	}

	// 2. Harness switch TO shell (claude -> shell) refused outright (400) for owner.
	rec := doRequestAsUser(t, srv, owner, http.MethodPost, "/api/v1/agents/"+llmAgent.ID+"/reincarnate", ReincarnateAgentRequest{
		HarnessConfig: config.ShellHarnessConfigName,
		DryRun:        true,
	})
	assert.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
	assert.Contains(t, rec.Body.String(), shellHarnessSwitchRefusalMessage)

	// 3. Reincarnate with patch flags on shell agent: project admin (no attach) -> 403.
	tl := 50
	patchReqs := []struct {
		name string
		req  ReincarnateAgentRequest
	}{
		{"image", ReincarnateAgentRequest{Image: "evil:latest", DryRun: true}},
		{"role", ReincarnateAgentRequest{Role: "baseline", DryRun: true}},
		{"serviceAccount", ReincarnateAgentRequest{ServiceAccount: "sa-1", DryRun: true}},
		{"model", ReincarnateAgentRequest{Model: "opus", DryRun: true}},
		{"thinkingLevel", ReincarnateAgentRequest{ThinkingLevel: &tl, DryRun: true}},
		{"harnessAuth", ReincarnateAgentRequest{HarnessAuth: "api-key", DryRun: true}},
	}
	for _, tc := range patchReqs {
		t.Run("admin_patch_"+tc.name, func(t *testing.T) {
			r := doRequestAsUser(t, srv, admin, http.MethodPost, "/api/v1/agents/"+shellAgent.ID+"/reincarnate", tc.req)
			assert.Equal(t, http.StatusForbidden, r.Code, r.Body.String())
		})
	}

	// 4. Shell agent self-reincarnate with patch flag -> 403 (human principals only).
	selfIdent := &agentIdentityWrapper{&AgentTokenClaims{
		Claims:    jwt.Claims{Subject: shellAgent.ID},
		ProjectID: project.ID,
		Ancestry:  []string{owner.ID, shellAgent.ID},
		Scopes:    []AgentTokenScope{ScopeProjectRead, ScopeAgentLifecycle},
	}}
	recSelf := doRequestAsIdentity(t, srv, selfIdent, http.MethodPost, "/api/v1/agents/"+shellAgent.ID+"/reincarnate", ReincarnateAgentRequest{
		Image:  "evil:latest",
		DryRun: true,
	})
	assert.Equal(t, http.StatusForbidden, recSelf.Code, recSelf.Body.String())

	// 5. Owner human with image patch passes S7 authz (does not get 403).
	recOwner := doRequestAsUser(t, srv, owner, http.MethodPost, "/api/v1/agents/"+shellAgent.ID+"/reincarnate", ReincarnateAgentRequest{
		Image:  "custom-shell:v2",
		DryRun: true,
	})
	assert.NotEqual(t, http.StatusForbidden, recOwner.Code, "owner human should pass S7 reincarnate patch gate: %s", recOwner.Body.String())
}

func TestShellAgentPendingConfig_S7(t *testing.T) {
	srv, s := testServer(t)
	setExperimentOverrides(t, srv, map[string]bool{experiments.AgentEditHeld: true})
	project, broker := seedShellTestProjectAndBroker(t, s)

	owner := seedShellTestUser(t, s, "pend-owner@test.com", "Pend Owner")
	admin := seedShellTestUser(t, s, "pend-admin@test.com", "Pend Admin")
	bindProjectRole(t, s, project.ID, owner.ID, "project-member")
	bindProjectRole(t, s, project.ID, admin.ID, "project-admin")

	shellAgent := seedShellTestAgent(t, s, project.ID, broker.ID, "pend-shell", owner.ID, config.ShellHarnessConfigName, state.PhaseRunning, []string{owner.ID})

	// 1. Project admin cannot save a held config edit on a running shell agent -> 403.
	rec := doRequestAsUser(t, srv, admin, http.MethodPatch, "/api/v1/agents/"+shellAgent.ID, json.RawMessage(`{"config":{"image":"evil:latest"}}`))
	assert.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())

	// 2. Owner CAN save a held config edit on a running shell agent -> 200.
	rec = doRequestAsUser(t, srv, owner, http.MethodPatch, "/api/v1/agents/"+shellAgent.ID, json.RawMessage(`{"config":{"image":"owner-shell:v2"}}`))
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	// 3. Project admin cannot discard pending config on a shell agent -> 403.
	rec = doRequestAsUser(t, srv, admin, http.MethodDelete, "/api/v1/agents/"+shellAgent.ID+"/pending-config", nil)
	assert.Equal(t, http.StatusForbidden, rec.Code, rec.Body.String())

	// 4. Fold-time re-authorization (pendingSaverMayApply):
	//    - Admin saver (has agent.update, lacks agent.attach) is dropped (saverUnauthorized).
	//    - Owner saver (has agent.update and agent.attach) applies (saverApply).
	ctx := context.Background()
	adminRes, err := srv.resolvePendingSaver(ctx, "user:"+admin.ID, false)
	require.NoError(t, err)
	require.Equal(t, saverApply, adminRes.out)
	adminFold, err := srv.pendingSaverMayApply(ctx, shellAgent, adminRes)
	require.NoError(t, err)
	assert.Equal(t, saverUnauthorized, adminFold.out, "fold re-auth must drop shell pending edit saved by non-attach admin")

	ownerRes, err := srv.resolvePendingSaver(ctx, "user:"+owner.ID, false)
	require.NoError(t, err)
	require.Equal(t, saverApply, ownerRes.out)
	ownerFold, err := srv.pendingSaverMayApply(ctx, shellAgent, ownerRes)
	require.NoError(t, err)
	assert.Equal(t, saverApply, ownerFold.out, "fold re-auth must allow shell pending edit saved by owner")

	// 5. Owner CAN discard pending config on a shell agent -> 200.
	rec = doRequestAsUser(t, srv, owner, http.MethodDelete, "/api/v1/agents/"+shellAgent.ID+"/pending-config", nil)
	assert.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
}
