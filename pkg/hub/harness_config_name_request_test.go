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
	"github.com/GoogleCloudPlatform/scion/pkg/config/opsettings"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// nonComponentNames are template and harness-config names that are not
// exactly one path component.
var nonComponentNames = map[string]string{
	"escaping":      "../x",
	"escaping two":  "../../x",
	"separator":     "a/b",
	"backslash":     `a\b`,
	"absolute":      "/srv/outside",
	"parent":        "..",
	"remote source": ":local:/srv/outside",
}

// requireNameRefusal checks a 400 with the fixed text that does not echo
// the value.
func requireNameRefusal(t *testing.T, code int, body, value, want string) {
	t.Helper()
	require.Equal(t, http.StatusBadRequest, code, body)
	assert.Contains(t, body, want)
	if value != ".." {
		assert.NotContains(t, body, value, "the refusal must not echo the value")
	}
}

func TestHarnessConfigNameRefusedOnWrite(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()
	existing := &store.HarnessConfig{
		ID: api.NewUUID(), Slug: "hc-existing", Name: "hc-existing", Harness: "claude",
		Scope: store.HarnessConfigScopeGlobal, Status: store.HarnessConfigStatusActive,
	}
	require.NoError(t, s.CreateHarnessConfig(ctx, existing))

	for label, name := range nonComponentNames {
		t.Run(label, func(t *testing.T) {
			rec := doRequest(t, srv, http.MethodPost, "/api/v1/harness-configs", CreateHarnessConfigRequest{
				Name: name, Slug: "hc-refused", Harness: "claude", Scope: store.HarnessConfigScopeGlobal,
			})
			requireNameRefusal(t, rec.Code, rec.Body.String(), name, resourceNameNotComponentMessage)

			rec = doRequest(t, srv, http.MethodPut, "/api/v1/harness-configs/"+existing.ID, UpdateHarnessConfigRequest{Name: name})
			requireNameRefusal(t, rec.Code, rec.Body.String(), name, resourceNameNotComponentMessage)

			rec = doRequest(t, srv, http.MethodPatch, "/api/v1/harness-configs/"+existing.ID, map[string]string{"name": name})
			requireNameRefusal(t, rec.Code, rec.Body.String(), name, resourceNameNotComponentMessage)

			rec = doRequest(t, srv, http.MethodPost, "/api/v1/harness-configs/"+existing.ID+"/clone", CloneTemplateRequest{
				Name: name, Scope: store.HarnessConfigScopeGlobal,
			})
			requireNameRefusal(t, rec.Code, rec.Body.String(), name, resourceNameNotComponentMessage)
		})
	}
	got, err := s.GetHarnessConfig(ctx, existing.ID)
	require.NoError(t, err)
	assert.Equal(t, "hc-existing", got.Name, "refused updates leave the stored name")

	// Controls: a one-component name is accepted on each route.
	rec := doRequest(t, srv, http.MethodPost, "/api/v1/harness-configs", CreateHarnessConfigRequest{
		Name: "hc-created", Harness: "claude", Scope: store.HarnessConfigScopeGlobal,
	})
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	rec = doRequest(t, srv, http.MethodPatch, "/api/v1/harness-configs/"+existing.ID, map[string]string{"name": "hc-renamed"})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	rec = doRequest(t, srv, http.MethodPut, "/api/v1/harness-configs/"+existing.ID, UpdateHarnessConfigRequest{Name: "Hc_Renamed.v2"})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	rec = doRequest(t, srv, http.MethodPost, "/api/v1/harness-configs/"+existing.ID+"/clone", CloneTemplateRequest{
		Name: "hc-cloned", Scope: store.HarnessConfigScopeGlobal,
	})
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
}

func TestTemplateNameRefusedOnWrite(t *testing.T) {
	srv, s := testServer(t)
	ctx := context.Background()
	existing := &store.Template{
		ID: api.NewUUID(), Slug: "tpl-existing", Name: "tpl-existing", Harness: "claude",
		Scope: store.TemplateScopeGlobal, Status: store.TemplateStatusActive,
	}
	require.NoError(t, s.CreateTemplate(ctx, existing))

	for label, name := range nonComponentNames {
		t.Run(label, func(t *testing.T) {
			rec := doRequest(t, srv, http.MethodPost, "/api/v1/templates", CreateTemplateRequest{
				Name: name, Slug: "tpl-refused", Harness: "claude", Scope: store.TemplateScopeGlobal,
			})
			requireNameRefusal(t, rec.Code, rec.Body.String(), name, resourceNameNotComponentMessage)

			rec = doRequest(t, srv, http.MethodPut, "/api/v1/templates/"+existing.ID, UpdateTemplateRequest{Name: name})
			requireNameRefusal(t, rec.Code, rec.Body.String(), name, resourceNameNotComponentMessage)

			rec = doRequest(t, srv, http.MethodPatch, "/api/v1/templates/"+existing.ID, map[string]string{"name": name})
			requireNameRefusal(t, rec.Code, rec.Body.String(), name, resourceNameNotComponentMessage)

			rec = doRequest(t, srv, http.MethodPost, "/api/v1/templates/"+existing.ID+"/clone", CloneTemplateRequest{
				Name: name, Scope: store.TemplateScopeGlobal,
			})
			requireNameRefusal(t, rec.Code, rec.Body.String(), name, resourceNameNotComponentMessage)
		})
	}
	got, err := s.GetTemplate(ctx, existing.ID)
	require.NoError(t, err)
	assert.Equal(t, "tpl-existing", got.Name, "refused updates leave the stored name")

	rec := doRequest(t, srv, http.MethodPatch, "/api/v1/templates/"+existing.ID, map[string]string{"name": "tpl-renamed"})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	rec = doRequest(t, srv, http.MethodPut, "/api/v1/templates/"+existing.ID, UpdateTemplateRequest{Name: "Tpl_Renamed.v2"})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
}

func TestUserTemplateNameRefusedOnWrite(t *testing.T) {
	srv, s, alice, _ := setupUserTemplateTest(t)
	existing := createUserTemplate(t, s, alice.ID, "alice-existing")

	for label, name := range nonComponentNames {
		t.Run(label, func(t *testing.T) {
			rec := doRequestAsUser(t, srv, alice, http.MethodPost, "/api/v1/users/me/templates", CreateTemplateRequest{
				Name: name, Slug: "alice-refused", Harness: "claude",
			})
			requireNameRefusal(t, rec.Code, rec.Body.String(), name, resourceNameNotComponentMessage)

			rec = doRequestAsUser(t, srv, alice, http.MethodPut, "/api/v1/users/me/templates/"+existing.ID, UpdateTemplateRequest{Name: name})
			requireNameRefusal(t, rec.Code, rec.Body.String(), name, resourceNameNotComponentMessage)
		})
	}
	got, err := s.GetTemplate(context.Background(), existing.ID)
	require.NoError(t, err)
	assert.Equal(t, "alice-existing", got.Name)
}

func TestAgentCreateRefusesNonComponentHarnessConfig(t *testing.T) {
	disp := &createAgentDispatcher{createPhase: string(state.PhaseRunning)}
	srv, s, project := setupCreateAgentServer(t, disp)

	for label, name := range nonComponentNames {
		t.Run(label, func(t *testing.T) {
			rec := doRequest(t, srv, http.MethodPost, "/api/v1/agents", CreateAgentRequest{
				Name: "refused-hc-agent", ProjectID: project.ID, HarnessConfig: name,
			})
			requireNameRefusal(t, rec.Code, rec.Body.String(), name, harnessConfigNameRefusal)

			rec = doRequest(t, srv, http.MethodPost, "/api/v1/agents", CreateAgentRequest{
				Name: "refused-hc-agent", ProjectID: project.ID, Config: &api.ScionConfig{HarnessConfig: name},
			})
			requireNameRefusal(t, rec.Code, rec.Body.String(), name, harnessConfigNameRefusal)
		})
	}
	agent, err := s.GetAgentBySlug(context.Background(), project.ID, "refused-hc-agent")
	assert.True(t, agent == nil || err != nil, "no agent is created")
	assert.Nil(t, disp.capturedAgent, "nothing is dispatched")

	// Control: a harness-config name is stored and dispatched.
	rec := doRequest(t, srv, http.MethodPost, "/api/v1/agents", CreateAgentRequest{
		Name: "named-hc-agent", ProjectID: project.ID, HarnessConfig: "hc-one",
	})
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	require.NotNil(t, disp.capturedAgent)
	assert.Equal(t, "hc-one", disp.capturedAgent.AppliedConfig.HarnessConfig)
}

// A default harness config (project, template or hub tier) that is not a
// harness-config name is dropped with a warning naming the tier: it is never
// stored or dispatched, and the create still succeeds.
func TestDefaultNonComponentHarnessConfigIsCleared(t *testing.T) {
	const bad = "../../outside"
	for _, tc := range []struct {
		tier  string
		setup func(t *testing.T, srv *Server, s store.Store, project *store.Project) string
	}{
		{harnessConfigTierProject, func(t *testing.T, srv *Server, s store.Store, project *store.Project) string {
			setProjectAnnotations(t, s, project, map[string]string{projectSettingDefaultHarnessConfig: bad})
			return ""
		}},
		{harnessConfigTierTemplate, func(t *testing.T, srv *Server, s store.Store, project *store.Project) string {
			createHarnessTemplate(t, s, "tmpl-bad-hc", bad)
			return "tmpl-bad-hc"
		}},
		{harnessConfigTierHub, func(t *testing.T, srv *Server, s store.Store, project *store.Project) string {
			setHubAgentDefaults(srv, opsettings.AgentDefaultsSettings{DefaultHarnessConfig: bad})
			return ""
		}},
	} {
		t.Run(tc.tier, func(t *testing.T) {
			disp := &createAgentDispatcher{createPhase: string(state.PhaseRunning)}
			srv, s, project := setupCreateAgentServer(t, disp)
			template := tc.setup(t, srv, s, project)

			rec := doRequest(t, srv, http.MethodPost, "/api/v1/agents", CreateAgentRequest{
				Name: "default-hc-agent", ProjectID: project.ID, Template: template,
			})
			require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
			assert.NotContains(t, rec.Body.String(), bad, "the value is not echoed")
			assert.Contains(t, rec.Body.String(), tc.tier, "the warning names the tier")

			var resp CreateAgentResponse
			require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
			stored, err := s.GetAgent(context.Background(), resp.Agent.ID)
			require.NoError(t, err)
			require.NotNil(t, stored.AppliedConfig)
			assert.Empty(t, stored.AppliedConfig.HarnessConfig, "never stored")
			require.NotNil(t, disp.capturedAgent)
			assert.Empty(t, disp.capturedAgent.AppliedConfig.HarnessConfig, "never dispatched")
		})
	}
}

// The agent PATCH route keeps a harness-config name fixed: a new value (any
// value, including one that is not a harness-config name) is refused and the
// stored name stays.
func TestAgentConfigPatchKeepsHarnessConfigFixed(t *testing.T) {
	disp := &createAgentDispatcher{createPhase: string(state.PhaseRunning)}
	srv, s, project := setupCreateAgentServer(t, disp)
	rec := doRequest(t, srv, http.MethodPost, "/api/v1/agents", CreateAgentRequest{
		Name: "patch-hc-agent", ProjectID: project.ID, HarnessConfig: "hc-one",
	})
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	var resp CreateAgentResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))

	rec = doRequest(t, srv, http.MethodPatch, "/api/v1/agents/"+resp.Agent.ID, map[string]interface{}{
		"config": map[string]interface{}{"harness_config": "../x"},
	})
	assert.GreaterOrEqual(t, rec.Code, 400, rec.Body.String())
	stored, err := s.GetAgent(context.Background(), resp.Agent.ID)
	require.NoError(t, err)
	assert.Equal(t, "hc-one", stored.AppliedConfig.HarnessConfig)
}

// A reincarnate request cannot carry a harness-config override.
func TestReincarnateRefusesHarnessConfigOverride(t *testing.T) {
	disp := &createAgentDispatcher{createPhase: string(state.PhaseRunning)}
	srv, _, project := setupCreateAgentServer(t, disp)
	rec := doRequest(t, srv, http.MethodPost, "/api/v1/agents", CreateAgentRequest{
		Name: "reinc-hc-agent", ProjectID: project.ID, HarnessConfig: "hc-one",
	})
	require.Equal(t, http.StatusCreated, rec.Code, rec.Body.String())
	var resp CreateAgentResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))

	rec = doRequest(t, srv, http.MethodPost, "/api/v1/agents/"+resp.Agent.ID+"/reincarnate", ReincarnateAgentRequest{HarnessConfig: "../x"})
	require.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
	assert.NotContains(t, rec.Body.String(), "../x")
}

// An explicit harness-config name replayed from an agent's recorded create
// inputs that is not a harness-config name (recorded before the request
// check) is dropped, never dispatched.
func TestRecordedNonComponentHarnessConfigIsCleared(t *testing.T) {
	disp := &createAgentDispatcher{createPhase: string(state.PhaseRunning)}
	srv, _, project := setupCreateAgentServer(t, disp)
	agent := &store.Agent{
		Name:          "recorded-hc-agent",
		AppliedConfig: &store.AgentAppliedConfig{HarnessConfig: "../x"},
	}
	ctx, warnings := withDispatchWarnings(context.Background())
	require.NoError(t, srv.deriveAgentConfig(ctx, agent, project, nil))
	assert.Empty(t, agent.AppliedConfig.HarnessConfig)
	assert.Contains(t, warnings.Warnings(), "the recorded harness config was ignored: it is not a harness-config name")
}
