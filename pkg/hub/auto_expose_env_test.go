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
	"log/slog"
	"net/http"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/config/opsettings"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const aeKey = api.EnvAutoExposePorts

// createAutoExposeTemplate creates a global template whose config env carries
// the given entries.
func createAutoExposeTemplate(t *testing.T, s store.Store, slug string, env map[string]string) {
	t.Helper()
	require.NoError(t, s.CreateTemplate(context.Background(), &store.Template{
		ID:          tid("template-" + slug + "-" + t.Name()),
		Name:        slug,
		Slug:        slug,
		Harness:     "claude",
		ContentHash: "d00dfeed",
		Scope:       store.TemplateScopeGlobal,
		Status:      "active",
		Config:      &store.TemplateConfig{Env: env},
	}))
}

type autoExposeCreate struct {
	hubDefault  *bool
	projectAnno string            // "" = no annotation
	templateEnv map[string]string // nil = no template
	requestEnv  map[string]string // nil = no request config
}

// createAutoExposeAgent runs a real POST /api/v1/agents and returns the
// persisted agent record.
func createAutoExposeAgent(t *testing.T, in autoExposeCreate) *store.Agent {
	t.Helper()
	disp := &createAgentDispatcher{createPhase: string(state.PhaseRunning)}
	srv, s, project := setupCreateAgentServer(t, disp)

	srv.mu.Lock()
	srv.config.AutoExposePortsDefault = in.hubDefault
	srv.mu.Unlock()
	if in.projectAnno != "" {
		setProjectAnnotations(t, s, project, map[string]string{projectSettingAutoExposePortsEnabled: in.projectAnno})
	}
	req := CreateAgentRequest{Name: "ae-agent", ProjectID: project.ID}
	if in.templateEnv != nil {
		createAutoExposeTemplate(t, s, "ae-tmpl", in.templateEnv)
		req.Template = "ae-tmpl"
	}
	if in.requestEnv != nil {
		req.Config = &api.ScionConfig{Env: in.requestEnv}
	}

	rec := doRequest(t, srv, http.MethodPost, "/api/v1/agents", req)
	require.Equal(t, http.StatusCreated, rec.Code, "body: %s", rec.Body.String())
	var resp CreateAgentResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	ag, err := s.GetAgent(context.Background(), resp.Agent.ID)
	require.NoError(t, err)
	require.NotNil(t, ag.AppliedConfig)
	return ag
}

func inlineEnv(ag *store.Agent) map[string]string {
	if ag.AppliedConfig.InlineConfig == nil {
		return nil
	}
	return ag.AppliedConfig.InlineConfig.Env
}

func createInputsEnv(ag *store.Agent) map[string]string {
	ci := ag.AppliedConfig.CreateInputs
	if ci == nil || ci.InlineConfig == nil {
		return nil
	}
	return ci.InlineConfig.Env
}

func ptrBool(b bool) *bool { return &b }

// TestCreateAgent_AutoExposePrecedence pins the hub half of the auto-expose
// precedence (user-explicit > project annotation > template env > hub
// default) on the persisted maps. Each case runs with and without unrelated
// request env, since a request without config takes a different branch in
// buildAppliedConfig.
func TestCreateAgent_AutoExposePrecedence(t *testing.T) {
	cases := []struct {
		name string
		in   autoExposeCreate
		// wantEnv is AppliedConfig.Env[AE]; "" means the key must be absent.
		wantEnv string
		// wantExplicit is the value expected in InlineConfig.Env and
		// CreateInputs.InlineConfig.Env; "" means the key must be absent.
		wantExplicit string
	}{
		{
			name:    "AC1 template false beats hub default true",
			in:      autoExposeCreate{hubDefault: ptrBool(true), templateEnv: map[string]string{aeKey: "false"}},
			wantEnv: "false",
		},
		{
			name:    "AC2 project true beats template false",
			in:      autoExposeCreate{projectAnno: "true", templateEnv: map[string]string{aeKey: "false"}},
			wantEnv: "true",
		},
		{
			name:         "AC3 explicit false beats project true",
			in:           autoExposeCreate{projectAnno: "true", requestEnv: map[string]string{aeKey: "false"}},
			wantEnv:      "false",
			wantExplicit: "false",
		},
		{
			name:         "AC3 explicit false beats project and template",
			in:           autoExposeCreate{projectAnno: "true", templateEnv: map[string]string{aeKey: "true"}, requestEnv: map[string]string{aeKey: "false"}},
			wantEnv:      "false",
			wantExplicit: "false",
		},
		{
			// AC4 at the hub: a hub default false writes nothing, so the
			// broker's harness-config env is free to win (tested in pkg/agent).
			name:    "AC4 hub default false writes nothing",
			in:      autoExposeCreate{hubDefault: ptrBool(false)},
			wantEnv: "",
		},
		{
			name:    "AC5 hub default alone is never persisted",
			in:      autoExposeCreate{hubDefault: ptrBool(true)},
			wantEnv: "",
		},
		{
			// AC6: a project-derived value never looks explicit.
			name:    "AC6 project value stays out of the explicit maps",
			in:      autoExposeCreate{projectAnno: "false", hubDefault: ptrBool(true)},
			wantEnv: "false",
		},
	}
	for _, tc := range cases {
		for _, withOther := range []bool{false, true} {
			name := tc.name
			in := tc.in
			if withOther {
				name += " (with request env)"
				env := map[string]string{"OTHER": "x"}
				for k, v := range tc.in.requestEnv {
					env[k] = v
				}
				in.requestEnv = env
			}
			t.Run(name, func(t *testing.T) {
				ag := createAutoExposeAgent(t, in)

				got, ok := ag.AppliedConfig.Env[aeKey]
				if tc.wantEnv == "" {
					assert.False(t, ok, "AppliedConfig.Env must not carry %s, got %q", aeKey, got)
				} else {
					assert.Equal(t, tc.wantEnv, got, "AppliedConfig.Env[%s]", aeKey)
				}

				for label, m := range map[string]map[string]string{
					"InlineConfig.Env":              inlineEnv(ag),
					"CreateInputs.InlineConfig.Env": createInputsEnv(ag),
				} {
					v, has := m[aeKey]
					if tc.wantExplicit == "" {
						assert.False(t, has, "%s must not carry %s, got %q", label, aeKey, v)
					} else {
						assert.Equal(t, tc.wantExplicit, v, "%s[%s]", label, aeKey)
					}
					if withOther {
						assert.Equal(t, "x", m["OTHER"], "%s keeps the requester's other keys", label)
					}
				}
			})
		}
	}
}

// TestCreateAgent_AppliedEnvDoesNotAliasInlineEnv pins buildAppliedConfig's
// separate Env map: the template-env fill and the project tier write
// AppliedConfig.Env, and none of it may leak into InlineConfig.Env, which
// holds the requester's explicit keys only.
func TestCreateAgent_AppliedEnvDoesNotAliasInlineEnv(t *testing.T) {
	ag := createAutoExposeAgent(t, autoExposeCreate{
		projectAnno: "true",
		templateEnv: map[string]string{"TPL": "t", aeKey: "false"},
		requestEnv:  map[string]string{"OTHER": "x"},
	})

	assert.Equal(t, map[string]string{"OTHER": "x"}, inlineEnv(ag), "InlineConfig.Env")
	assert.Equal(t, map[string]string{"OTHER": "x"}, createInputsEnv(ag), "CreateInputs.InlineConfig.Env")
	assert.Equal(t, map[string]string{"OTHER": "x", "TPL": "t", aeKey: "true"}, ag.AppliedConfig.Env, "AppliedConfig.Env")
}

func TestResolveAutoExposeEnv(t *testing.T) {
	project := func(v string) *store.Project {
		return &store.Project{Annotations: map[string]string{projectSettingAutoExposePortsEnabled: v}}
	}
	t.Run("explicit key wins even when project is set", func(t *testing.T) {
		ac := &store.AgentAppliedConfig{Env: map[string]string{aeKey: "false"}}
		resolveAutoExposeEnv(ac, project("true"), map[string]string{aeKey: "false"})
		assert.Equal(t, "false", ac.Env[aeKey])
	})
	t.Run("project overwrites template value", func(t *testing.T) {
		ac := &store.AgentAppliedConfig{Env: map[string]string{aeKey: "false"}}
		resolveAutoExposeEnv(ac, project("true"), nil)
		assert.Equal(t, "true", ac.Env[aeKey])
	})
	t.Run("project fills a nil env", func(t *testing.T) {
		ac := &store.AgentAppliedConfig{}
		resolveAutoExposeEnv(ac, project("false"), nil)
		assert.Equal(t, map[string]string{aeKey: "false"}, ac.Env)
	})
	t.Run("unparsable annotation leaves env alone", func(t *testing.T) {
		ac := &store.AgentAppliedConfig{Env: map[string]string{aeKey: "false"}}
		resolveAutoExposeEnv(ac, project("maybe"), nil)
		assert.Equal(t, "false", ac.Env[aeKey])
	})
	t.Run("nil project and nil config are safe", func(t *testing.T) {
		resolveAutoExposeEnv(nil, project("true"), nil)
		ac := &store.AgentAppliedConfig{}
		resolveAutoExposeEnv(ac, nil, nil)
		assert.Nil(t, ac.Env)
	})
}

// autoExposeDispatchFixture builds a dispatcher with a store holding one
// project/broker/provider, so start and restart dispatches can run.
func autoExposeDispatchFixture(t *testing.T) (*HTTPAgentDispatcher, *mockRuntimeBrokerClient, *store.Agent) {
	t.Helper()
	ctx := context.Background()
	memStore := createTestStore(t)
	require.NoError(t, memStore.CreateProject(ctx, &store.Project{
		ID: tid("project-1"), Name: "test-project", Slug: "test-project",
		GitRemote: "https://github.com/example/repo.git",
	}))
	require.NoError(t, memStore.CreateRuntimeBroker(ctx, &store.RuntimeBroker{
		ID: tid("broker-1"), Name: "test-broker", Slug: "test-broker",
		Endpoint: "http://localhost:9800", Status: store.BrokerStatusOnline,
	}))
	require.NoError(t, memStore.AddProjectProvider(ctx, &store.ProjectProvider{
		ProjectID: tid("project-1"), BrokerID: tid("broker-1"), BrokerName: "test-broker",
		LocalPath: "/home/user/projects/myproject/.scion", Status: store.BrokerStatusOnline,
	}))
	client := &mockRuntimeBrokerClient{}
	d := NewHTTPAgentDispatcherWithClient(memStore, client, false, slog.Default())
	ag := &store.Agent{
		ID: "agent-uuid-123", Name: "test-agent", Slug: "test-agent-slug",
		ProjectID: tid("project-1"), OwnerID: "owner-uuid-789", RuntimeBrokerID: tid("broker-1"),
	}
	return d, client, ag
}

// TestDispatch_AutoExposeDefault_OnCreateStartAndRestart pins the hub default's
// transport: it rides HubAgentDefaults on create, start and restart, so a
// change to it reaches an agent at its next start (AC5). Start and restart
// carry the auto-expose default only: the hub limit/resource defaults stay
// create/provision-only, so start behaviour for limits is unchanged.
func TestDispatch_AutoExposeDefault_OnCreateStartAndRestart(t *testing.T) {
	ctx := context.Background()
	d, client, ag := autoExposeDispatchFixture(t)
	hubDefault := ptrBool(true)
	d.SetAutoExposePortsDefaultProvider(func() *bool { return hubDefault })
	d.SetHubAgentDefaultsProvider(func() opsettings.AgentDefaultsSettings {
		var s opsettings.AgentDefaultsSettings
		s.DefaultMaxTurns = 7
		s.DefaultMaxModelCalls = 70
		s.DefaultMaxDuration = "1h"
		s.DefaultResources = &api.ResourceSpec{Limits: api.ResourceList{CPU: "2"}}
		return s
	})

	req, err := d.buildCreateRequest(ctx, hubDefaultsDispatchAgent(), "test")
	require.NoError(t, err)
	require.NotNil(t, req.Config)
	hd := req.Config.HubAgentDefaults
	require.NotNil(t, hd, "create must carry the hub defaults")
	require.NotNil(t, hd.AutoExposePorts)
	assert.True(t, *hd.AutoExposePorts)
	assert.Equal(t, 7, hd.MaxTurns, "create carries the hub limits")
	assert.Equal(t, "1h", hd.MaxDuration)
	assert.NotNil(t, hd.Resources)

	assertAutoExposeOnly := func(label string, hd *RemoteHubAgentDefaults, want bool) {
		t.Helper()
		require.NotNil(t, hd, "%s must carry the hub auto-expose default", label)
		require.NotNil(t, hd.AutoExposePorts, label)
		assert.Equal(t, want, *hd.AutoExposePorts, label)
		assert.Zero(t, hd.MaxTurns, "%s must not carry hub limits", label)
		assert.Zero(t, hd.MaxModelCalls, "%s must not carry hub limits", label)
		assert.Empty(t, hd.MaxDuration, "%s must not carry hub limits", label)
		assert.Nil(t, hd.Resources, "%s must not carry hub resources", label)
	}

	require.NoError(t, d.DispatchAgentStart(ctx, ag, "", false))
	assertAutoExposeOnly("start", client.lastStartExtras.HubAgentDefaults, true)

	// A changed hub default reaches the next restart.
	hubDefault = ptrBool(false)
	require.NoError(t, d.DispatchAgentRestart(ctx, ag))
	assertAutoExposeOnly("restart", client.lastRestartExtras.HubAgentDefaults, false)
}

func TestDispatch_AutoExposeDefault_UnsetSendsNothingOnStart(t *testing.T) {
	ctx := context.Background()
	d, client, ag := autoExposeDispatchFixture(t)
	d.SetAutoExposePortsDefaultProvider(func() *bool { return nil })

	require.NoError(t, d.DispatchAgentStart(ctx, ag, "", false))
	assert.Nil(t, client.lastStartExtras.HubAgentDefaults)
}

func TestApplyStartExtras_HubAgentDefaults(t *testing.T) {
	payload := map[string]interface{}{}
	applyStartExtras(payload, StartExtras{})
	_, ok := payload["hubAgentDefaults"]
	assert.False(t, ok, "no hubAgentDefaults key without a value")

	applyStartExtras(payload, StartExtras{HubAgentDefaults: startHubAgentDefaults(ptrBool(false), nil)})
	blob, err := json.Marshal(payload)
	require.NoError(t, err)
	assert.JSONEq(t, `{"hubAgentDefaults":{"autoExposePorts":false}}`, string(blob))
}

func TestServer_InstallsAutoExposeDefaultProvider(t *testing.T) {
	srv := &Server{store: createTestStore(t), maintenance: NewMaintenanceState(false, "")}
	srv.config.AutoExposePortsDefault = ptrBool(true)
	d := srv.CreateAuthenticatedDispatcher()
	require.NotNil(t, d.autoExposePortsDefaultProvider,
		"CreateAuthenticatedDispatcher must install the auto-expose default provider")
	got := d.autoExposePortsDefault()
	require.NotNil(t, got)
	assert.True(t, *got)
}
