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
	"net/http/httptest"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestCreateAgent_AllowGitCredentials_FromTemplateOnly pins that the
// allowance is resolved from the hub template at create, and that a create
// request cannot set it.
func TestCreateAgent_AllowGitCredentials_FromTemplateOnly(t *testing.T) {
	cases := []struct {
		name     string
		template *store.TemplateConfig // nil: no Config on the template
		useTmpl  bool
		want     bool
	}{
		{"template allows", &store.TemplateConfig{AllowGitCredentials: true}, true, true},
		{"template unset", &store.TemplateConfig{}, true, false},
		{"template without config", nil, true, false},
		{"no template", nil, false, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			disp := &createAgentDispatcher{createPhase: string(state.PhaseRunning)}
			srv, s, project := setupCreateAgentServer(t, disp)

			body := map[string]any{
				"name":      "git-cred-agent",
				"projectId": project.ID,
				// Neither of these is a create input; both must be ignored.
				"allowGitCredentials": true,
				"appliedConfig":       map[string]any{"allowGitCredentials": true},
			}
			if tc.useTmpl {
				tmpl := createHarnessTemplate(t, s, "git-cred-tmpl", "")
				tmpl.Config = tc.template
				require.NoError(t, s.UpdateTemplate(context.Background(), tmpl))
				body["template"] = "git-cred-tmpl"
			}

			rec := doRequest(t, srv, http.MethodPost, "/api/v1/agents", body)
			require.Equal(t, http.StatusCreated, rec.Code, "body: %s", rec.Body.String())
			var resp CreateAgentResponse
			require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
			ag, err := s.GetAgent(context.Background(), resp.Agent.ID)
			require.NoError(t, err)
			require.NotNil(t, ag.AppliedConfig)
			assert.Equal(t, tc.want, ag.AppliedConfig.AllowGitCredentials)
		})
	}
}

// TestResolveDerivedConfig_AllowGitCredentials_Recomputed pins that the
// value is recomputed from the template, never kept from an earlier
// resolution (reincarnate re-runs this on a fresh config).
func TestResolveDerivedConfig_AllowGitCredentials_Recomputed(t *testing.T) {
	srv, _ := testServer(t)
	ag := &store.Agent{ID: "a", AppliedConfig: &store.AgentAppliedConfig{AllowGitCredentials: true}}
	srv.resolveDerivedConfig(context.Background(), ag, nil, &store.Template{ID: "t", Config: &store.TemplateConfig{}})
	assert.False(t, ag.AppliedConfig.AllowGitCredentials, "template without the allowance")

	ag.AppliedConfig.AllowGitCredentials = true
	srv.resolveDerivedConfig(context.Background(), ag, nil, nil)
	assert.False(t, ag.AppliedConfig.AllowGitCredentials, "no template")

	srv.resolveDerivedConfig(context.Background(), ag, nil, &store.Template{ID: "t", Config: &store.TemplateConfig{AllowGitCredentials: true}})
	assert.True(t, ag.AppliedConfig.AllowGitCredentials, "template with the allowance")
}

// TestDispatch_AllowGitCredentials_OnCreateStartAndRestart pins that the
// resolved value reaches the broker on every dispatch kind, and that a
// missing AppliedConfig sends false.
func TestDispatch_AllowGitCredentials_OnCreateStartAndRestart(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name    string
		applied *store.AgentAppliedConfig
		want    bool
	}{
		{"allowed", &store.AgentAppliedConfig{AllowGitCredentials: true}, true},
		{"unset", &store.AgentAppliedConfig{}, false},
		{"nil applied config", nil, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d, client, ag := autoExposeDispatchFixture(t)
			ag.AppliedConfig = tc.applied

			req, err := d.buildCreateRequest(ctx, ag, "test")
			require.NoError(t, err)
			assert.Equal(t, tc.want, req.AllowGitCredentials, "create")

			require.NoError(t, d.DispatchAgentStart(ctx, ag, "", false))
			assert.Equal(t, tc.want, client.lastStartExtras.AllowGitCredentials, "start")

			require.NoError(t, d.DispatchAgentRestart(ctx, ag))
			assert.Equal(t, tc.want, client.lastRestartExtras.AllowGitCredentials, "restart")
		})
	}
}

func TestApplyStartExtras_AllowGitCredentials(t *testing.T) {
	payload := map[string]interface{}{}
	applyStartExtras(payload, StartExtras{})
	_, present := payload["allowGitCredentials"]
	assert.False(t, present, "false is sent as absent")

	payload = map[string]interface{}{}
	applyStartExtras(payload, StartExtras{AllowGitCredentials: true})
	assert.Equal(t, true, payload["allowGitCredentials"])
}

// TestReincarnate_AllowGitCredentials_RecomputedFromCurrentTemplate pins that
// reincarnate recomputes the allowance from the agent's CURRENT template, in
// both directions, rather than carrying the old generation's value: a
// template turned on grants it to an agent created without it, and a
// template turned off revokes it from an agent created with it. Each
// direction is checked on the fresh config builder and through the
// reincarnate endpoint (the config dispatched to the broker and the stored
// row).
func TestReincarnate_AllowGitCredentials_RecomputedFromCurrentTemplate(t *testing.T) {
	cases := []struct {
		name          string
		templateAllow bool
		oldAllow      bool
	}{
		{name: "template-on-grants", templateAllow: true, oldAllow: false},
		{name: "template-off-revokes", templateAllow: false, oldAllow: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			disp := newReincarnateTestDispatcher()
			srv, s, project, broker := setupReincarnateTestServer(t, disp)

			template := &store.Template{
				ID:          tid("tmpl-gitcred-" + t.Name()),
				Name:        "t",
				Slug:        "reincarnate-template-gitcred-" + tidSlugSafe(t.Name()),
				Harness:     "claude",
				Scope:       store.TemplateScopeGlobal,
				Status:      store.TemplateStatusActive,
				ContentHash: "template-hash-gitcred",
				Config:      &store.TemplateConfig{AllowGitCredentials: tc.templateAllow},
			}
			require.NoError(t, s.CreateTemplate(ctx, template))

			agent := newReincarnateTestAgent(t, s, project, broker, func(a *store.Agent) {
				a.Template = template.Slug
				a.AppliedConfig.AllowGitCredentials = tc.oldAllow
			})

			fresh, _, err := srv.buildFreshAppliedConfig(ctx, agent, project, "")
			require.NoError(t, err)
			assert.Equal(t, tc.templateAllow, fresh.AllowGitCredentials, "fresh config builder")

			rec := httptest.NewRecorder()
			self := agentIdentityFor(agent.ID, project.ID)
			srv.handleReincarnateAgent(rec, reincarnateRequest(t, agent.ID, self, ReincarnateAgentRequest{Handoff: "h"}), agent.ID)
			require.Equal(t, http.StatusAccepted, rec.Code, rec.Body.String())
			r := waitForReincarnationSettled(t, s, agent.ID)
			require.Equal(t, store.AgentReincarnationStateCompleted, r.State, r.Error)

			calls, cfgs := disp.reprovisionSnapshot()
			require.Equal(t, 1, calls)
			assert.Equal(t, tc.templateAllow, cfgs[0].AllowGitCredentials, "config dispatched to the broker")

			final, err := s.GetAgent(ctx, agent.ID)
			require.NoError(t, err)
			require.NotNil(t, final.AppliedConfig)
			assert.Equal(t, tc.templateAllow, final.AppliedConfig.AllowGitCredentials, "stored applied config")
		})
	}
}
