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

//go:build !no_sqlite && (!hubshard || hubshard_3)

package hub

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/config/opsettings"
	"github.com/GoogleCloudPlatform/scion/pkg/hubclient"
	"github.com/GoogleCloudPlatform/scion/pkg/runtimebroker"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Tests for ptone/scion#4222 (TS-2b): the hub stops writing its defaults and
// the template's config into the agent's inline config.

const (
	tiersHubEndpoint  = "https://hub-otel.example.com"
	tiersRowEndpoint  = "https://row-otel.example.com"
	tiersSnapEndpoint = "https://snap-otel.example.com"
	tiersReqEndpoint  = "https://req-otel.example.com"
)

// tiersTemplateEnvKeys are the env keys the fixture template sets, in its
// row config and in its snapshot. None may reach an agent through the hub.
var tiersTemplateEnvKeys = []string{"TEMPLATE_PLAIN", "SCION_ROW_KEY", "OTEL_ROW", "SNAP_PLAIN", "SCION_SNAP_KEY", "OTEL_SNAP"}

type tiersFixture struct {
	srv      *Server
	s        store.Store
	project  *store.Project
	template *store.Template
	client   *mockRuntimeBrokerClient
	d        *HTTPAgentDispatcher
}

// newTiersFixture builds a hub with a hub telemetry default, a project whose
// telemetry policy is off, a broker with or without templateTiers, and a
// template whose row and snapshot both set image, telemetry and env.
func newTiersFixture(t *testing.T, templateTiers bool) *tiersFixture {
	t.Helper()
	srv, s := testServer(t)
	ctx := t.Context()

	on := true
	srv.mu.Lock()
	srv.config.TelemetryConfig = &api.TelemetryConfig{Enabled: &on, Cloud: &api.TelemetryCloudConfig{Endpoint: tiersHubEndpoint}}
	srv.mu.Unlock()

	project := &store.Project{
		ID: tid("tiers-project"), Name: "Tiers Project", Slug: "tiers-project",
		OwnerID: "dev@localhost", GitRemote: "https://example.com/tiers/repo.git",
		Annotations: map[string]string{projectSettingTelemetryEnabled: "false"},
	}
	require.NoError(t, s.CreateProject(ctx, project))
	broker := &store.RuntimeBroker{
		ID: tid("tiers-broker"), Name: "tiers-broker", Slug: "tiers-broker",
		Endpoint: "http://localhost:9800", Status: store.BrokerStatusOnline,
		Capabilities: &store.BrokerCapabilities{TemplateTiers: templateTiers},
	}
	require.NoError(t, s.CreateRuntimeBroker(ctx, broker))
	require.NoError(t, s.AddProjectProvider(ctx, &store.ProjectProvider{
		ProjectID: project.ID, BrokerID: broker.ID, BrokerName: broker.Name,
		LocalPath: "/home/user/tiers/.scion", Status: store.BrokerStatusOnline,
	}))

	template := &store.Template{
		ID: tid("tiers-template"), Name: "Tiers Template", Slug: "tiers-template",
		Harness: "claude", Scope: store.TemplateScopeGlobal, Status: store.TemplateStatusActive,
		ContentHash: "tiers-template-hash",
		Image:       "row-top-image:v1",
		Config: &store.TemplateConfig{
			Image:     "row-image:v1",
			Env:       map[string]string{"TEMPLATE_PLAIN": "row", "SCION_ROW_KEY": "row", "OTEL_ROW": "row"},
			Telemetry: &api.TelemetryConfig{Enabled: &on, Cloud: &api.TelemetryCloudConfig{Endpoint: tiersRowEndpoint}},
			Secrets:   []api.RequiredSecret{{Key: "ROW_SECRET"}},
		},
		AgentConfig: &api.ScionConfig{
			Image:     "snap-image:v1",
			Env:       map[string]string{"SNAP_PLAIN": "snap", "SCION_SNAP_KEY": "snap", "OTEL_SNAP": "snap"},
			Telemetry: &api.TelemetryConfig{Enabled: &on, Cloud: &api.TelemetryCloudConfig{Endpoint: tiersSnapEndpoint}},
			Secrets:   []api.RequiredSecret{{Key: "SNAP_SECRET", Description: "from the snapshot", Type: "environment"}},
		},
	}
	require.NoError(t, s.CreateTemplate(ctx, template))

	client := &mockRuntimeBrokerClient{}
	d := NewHTTPAgentDispatcherWithClient(s, client, false, slog.Default())
	d.SetHubTelemetryProvider(srv.hubTelemetryDefault)
	d.SetTemplateJobConfigProvider(srv.templateJobConfigByID)
	d.SetHubAgentDefaultsProvider(srv.hubAgentDefaults)
	return &tiersFixture{srv: srv, s: s, project: project, template: template, client: client, d: d}
}

// newAgent runs create's derivation for a request config on this fixture.
func (f *tiersFixture) newAgent(t *testing.T, ctx context.Context, id string, reqCfg *api.ScionConfig) *store.Agent {
	t.Helper()
	ac := f.srv.buildAppliedConfig(CreateAgentRequest{Config: reqCfg, Workspace: "/tmp/tiers-ws"}, "dev@localhost", AgentRoleBaseline)
	agent := &store.Agent{
		ID: tid(id), Name: id, Slug: id, ProjectID: f.project.ID, OwnerID: "dev@localhost",
		RuntimeBrokerID: tid("tiers-broker"), Template: f.template.Slug, AppliedConfig: ac,
	}
	require.NoError(t, f.srv.deriveAgentConfig(ctx, agent, f.project, f.template))
	return agent
}

func tiersName(templateTiers bool) string {
	if templateTiers {
		return "templateTiers broker"
	}
	return "legacy broker"
}

// TestTemplateTiers_TemplateEnvironmentKeysNeverDispatched is amendment
// acceptance (a): for capable and legacy brokers, a template whose row and
// snapshot set image, telemetry and reserved env produces an agent whose
// AppliedConfig.Image, AppliedConfig.Env and InlineConfig.Telemetry carry
// none of them, and so does the dispatch payload.
func TestTemplateTiers_TemplateEnvironmentKeysNeverDispatched(t *testing.T) {
	for _, tiers := range []bool{true, false} {
		t.Run(tiersName(tiers), func(t *testing.T) {
			f := newTiersFixture(t, tiers)
			ctx := t.Context()
			agent := f.newAgent(t, ctx, "env-keys", nil)

			ac := agent.AppliedConfig
			assert.Empty(t, ac.Image, "a template image never fills AppliedConfig.Image")
			for _, k := range tiersTemplateEnvKeys {
				assert.NotContains(t, ac.Env, k, "template env never reaches AppliedConfig.Env")
			}
			if ac.InlineConfig != nil {
				assert.Nil(t, ac.InlineConfig.Telemetry, "a create without request telemetry leaves InlineConfig.Telemetry nil")
			}

			req, err := f.d.buildCreateRequest(ctx, agent, "test")
			require.NoError(t, err)
			require.NotNil(t, req.Config)
			assert.Empty(t, req.Config.Image)
			for _, k := range tiersTemplateEnvKeys {
				assert.NotContains(t, req.ResolvedEnv, k, "template env never reaches ResolvedEnv")
			}
			if req.InlineConfig != nil {
				assert.Empty(t, req.InlineConfig.Image)
				if tel := req.InlineConfig.Telemetry; tel != nil && tel.Cloud != nil {
					assert.NotEqual(t, tiersRowEndpoint, tel.Cloud.Endpoint, "row template telemetry never dispatched")
					assert.NotEqual(t, tiersSnapEndpoint, tel.Cloud.Endpoint, "snapshot template telemetry never dispatched")
				}
			}
		})
	}
}

// TestTemplateTiers_HubTelemetryDelivery: a templateTiers broker gets the
// hub default in HubAgentDefaults.Telemetry and the project policy in
// TelemetryPolicy, with no inline telemetry; a legacy broker gets the old
// payload, the hub default inline with the policy written over Enabled.
func TestTemplateTiers_HubTelemetryDelivery(t *testing.T) {
	t.Run("templateTiers broker", func(t *testing.T) {
		f := newTiersFixture(t, true)
		ctx := t.Context()
		req, err := f.d.buildCreateRequest(ctx, f.newAgent(t, ctx, "tiers-capable", nil), "test")
		require.NoError(t, err)
		if req.InlineConfig != nil {
			assert.Nil(t, req.InlineConfig.Telemetry, "the hub writes nothing into InlineConfig for a capable broker")
		}
		require.NotNil(t, req.Config.HubAgentDefaults)
		require.NotNil(t, req.Config.HubAgentDefaults.Telemetry)
		require.NotNil(t, req.Config.HubAgentDefaults.Telemetry.Cloud)
		assert.Equal(t, tiersHubEndpoint, req.Config.HubAgentDefaults.Telemetry.Cloud.Endpoint)
		require.NotNil(t, req.Config.TelemetryPolicy, "the project annotation travels as TelemetryPolicy")
		assert.False(t, *req.Config.TelemetryPolicy)
		assert.NotContains(t, req.ResolvedEnv, envTelemetryEnabled, "no requester flag without request telemetry")
	})
	t.Run("legacy broker", func(t *testing.T) {
		f := newTiersFixture(t, false)
		ctx := t.Context()
		req, err := f.d.buildCreateRequest(ctx, f.newAgent(t, ctx, "tiers-legacy", nil), "test")
		require.NoError(t, err)
		require.NotNil(t, req.InlineConfig)
		tel := req.InlineConfig.Telemetry
		require.NotNil(t, tel, "a legacy broker still gets the hub default inline")
		require.NotNil(t, tel.Cloud)
		assert.Equal(t, tiersHubEndpoint, tel.Cloud.Endpoint)
		require.NotNil(t, tel.Enabled)
		assert.False(t, *tel.Enabled, "the project policy is still written inline for a legacy broker")
		if req.Config.HubAgentDefaults != nil {
			assert.Nil(t, req.Config.HubAgentDefaults.Telemetry)
		}
		assert.Nil(t, req.Config.TelemetryPolicy)
	})
}

// TestTemplateTiers_RequesterTelemetry: a templateTiers broker gets only the
// requester's on/off, as SCION_TELEMETRY_ENABLED, and create warns about the
// dropped fields; a legacy broker gets the requester's block as before.
func TestTemplateTiers_RequesterTelemetry(t *testing.T) {
	reqCfg := func() *api.ScionConfig {
		on := true
		return &api.ScionConfig{Telemetry: &api.TelemetryConfig{Enabled: &on, Cloud: &api.TelemetryCloudConfig{Endpoint: tiersReqEndpoint}}}
	}
	t.Run("templateTiers broker", func(t *testing.T) {
		f := newTiersFixture(t, true)
		ctx, warnings := withDispatchWarnings(t.Context())
		agent := f.newAgent(t, ctx, "req-tel-capable", reqCfg())
		req, err := f.d.buildCreateRequest(ctx, agent, "test")
		require.NoError(t, err)
		if req.InlineConfig != nil {
			assert.Nil(t, req.InlineConfig.Telemetry)
		}
		assert.Equal(t, "true", req.ResolvedEnv[envTelemetryEnabled], "enabled arrives as SCION_TELEMETRY_ENABLED")
		warnings.mu.Lock()
		got := strings.Join(warnings.list, "\n")
		warnings.mu.Unlock()
		assert.Contains(t, got, "telemetry cloud", "the dropped fields are named in a create warning")
	})
	t.Run("legacy broker", func(t *testing.T) {
		f := newTiersFixture(t, false)
		ctx, warnings := withDispatchWarnings(t.Context())
		agent := f.newAgent(t, ctx, "req-tel-legacy", reqCfg())
		req, err := f.d.buildCreateRequest(ctx, agent, "test")
		require.NoError(t, err)
		require.NotNil(t, req.InlineConfig)
		require.NotNil(t, req.InlineConfig.Telemetry)
		assert.Equal(t, tiersReqEndpoint, req.InlineConfig.Telemetry.Cloud.Endpoint, "a legacy broker keeps the requester's block")
		assert.NotContains(t, req.ResolvedEnv, envTelemetryEnabled)
		warnings.mu.Lock()
		assert.Empty(t, warnings.list)
		warnings.mu.Unlock()
	})
}

// TestTemplateTiers_RestartDoesNotResendStoredHubCopy: an agent created by
// an older hub has a hub-default copy in its stored InlineConfig.Telemetry.
// A start rebuilds inline telemetry from CreateInputs instead, so the stale
// copy never reaches the broker at top tier; a restart carries the tiers.
func TestTemplateTiers_RestartDoesNotResendStoredHubCopy(t *testing.T) {
	staleAgent := func(f *tiersFixture) *store.Agent {
		on := true
		return &store.Agent{
			ID: tid("pre-change"), Name: "pre-change", Slug: "pre-change", ProjectID: f.project.ID,
			OwnerID: "dev@localhost", RuntimeBrokerID: tid("tiers-broker"),
			AppliedConfig: &store.AgentAppliedConfig{
				InlineConfig: &api.ScionConfig{Telemetry: &api.TelemetryConfig{
					Enabled: &on, Cloud: &api.TelemetryCloudConfig{Endpoint: "https://stale-hub.example.com"},
				}},
				CreateInputs: &store.AgentCreateInputs{},
			},
		}
	}
	t.Run("templateTiers broker", func(t *testing.T) {
		f := newTiersFixture(t, true)
		ctx := context.Background()
		ag := staleAgent(f)
		require.NoError(t, f.d.DispatchAgentStart(ctx, ag, "", false))
		if f.client.lastInlineConfig != nil {
			assert.Nil(t, f.client.lastInlineConfig.Telemetry, "the stored hub copy is not resent")
		}
		require.NotNil(t, f.client.lastStartExtras.HubAgentDefaults)
		require.NotNil(t, f.client.lastStartExtras.HubAgentDefaults.Telemetry)
		assert.Equal(t, tiersHubEndpoint, f.client.lastStartExtras.HubAgentDefaults.Telemetry.Cloud.Endpoint)
		require.NotNil(t, f.client.lastStartExtras.TelemetryPolicy)
		assert.False(t, *f.client.lastStartExtras.TelemetryPolicy)

		require.NoError(t, f.d.DispatchAgentRestart(ctx, ag))
		require.NotNil(t, f.client.lastRestartExtras.HubAgentDefaults)
		require.NotNil(t, f.client.lastRestartExtras.HubAgentDefaults.Telemetry)
		assert.Equal(t, tiersHubEndpoint, f.client.lastRestartExtras.HubAgentDefaults.Telemetry.Cloud.Endpoint)
		require.NotNil(t, f.client.lastRestartExtras.TelemetryPolicy)
		assert.False(t, *f.client.lastRestartExtras.TelemetryPolicy)
	})
	t.Run("legacy broker", func(t *testing.T) {
		f := newTiersFixture(t, false)
		ctx := context.Background()
		ag := staleAgent(f)
		require.NoError(t, f.d.DispatchAgentStart(ctx, ag, "", false))
		require.NotNil(t, f.client.lastInlineConfig)
		tel := f.client.lastInlineConfig.Telemetry
		require.NotNil(t, tel)
		assert.Equal(t, tiersHubEndpoint, tel.Cloud.Endpoint, "the current hub default, not the stored copy")
		require.NotNil(t, tel.Enabled)
		assert.False(t, *tel.Enabled)
		assert.Equal(t, "https://stale-hub.example.com", ag.AppliedConfig.InlineConfig.Telemetry.Cloud.Endpoint,
			"the agent record is not modified by dispatch")
		assert.Nil(t, f.client.lastStartExtras.TelemetryPolicy)

		require.NoError(t, f.d.DispatchAgentRestart(ctx, ag))
		assert.Nil(t, f.client.lastRestartExtras.TelemetryPolicy)
		if h := f.client.lastRestartExtras.HubAgentDefaults; h != nil {
			assert.Nil(t, h.Telemetry)
		}
	})
}

// TestTemplateTiers_ModelOrder: request > project > template (snapshot) >
// hub. The hub default fills only when the template and the project leave
// the model unset.
func TestTemplateTiers_ModelOrder(t *testing.T) {
	cases := []struct {
		name, request, project, snapshot, row, hub, want string
	}{
		{name: "request beats all", request: "req-model", project: "project-model", snapshot: "snap-model", hub: "hub-model", want: "req-model"},
		{name: "project default beats template", project: "project-model", snapshot: "snap-model", hub: "hub-model", want: "project-model"},
		{name: "template beats hub default", snapshot: "snap-model", hub: "hub-model", want: "snap-model"},
		{name: "hub default when nothing else", hub: "hub-model", want: "hub-model"},
		{name: "row config model is not a template source", row: "row-model", hub: "hub-model", want: "hub-model"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newTiersFixture(t, true)
			ctx := t.Context()
			setHubAgentDefaults(f.srv, opsettings.AgentDefaultsSettings{DefaultModel: tc.hub})
			f.project.Annotations = map[string]string{}
			if tc.project != "" {
				f.project.Annotations[projectSettingDefaultModel] = tc.project
			}
			f.template.AgentConfig = &api.ScionConfig{Model: tc.snapshot}
			f.template.Config = &store.TemplateConfig{Model: tc.row}
			var reqCfg *api.ScionConfig
			if tc.request != "" {
				reqCfg = &api.ScionConfig{Model: tc.request}
			}
			agent := f.newAgent(t, ctx, "model-order", reqCfg)
			assert.Equal(t, tc.want, agent.AppliedConfig.Model)
		})
	}
}

// TestTemplateTiers_OtherLaddersUnchanged is the regression test for the
// fields whose order TS-2b does not change: thinking level and harness
// config (project > template > hub), and limits (project inline, hub in
// HubAgentDefaults, never inline).
func TestTemplateTiers_OtherLaddersUnchanged(t *testing.T) {
	t.Run("thinking level: project beats hub", func(t *testing.T) {
		f := newTiersFixture(t, true)
		hubTL := 3
		setHubAgentDefaults(f.srv, opsettings.AgentDefaultsSettings{DefaultThinkingLevel: &hubTL})
		f.project.Annotations = map[string]string{projectSettingDefaultThinkingLevel: "7"}
		agent := f.newAgent(t, t.Context(), "tl-project", nil)
		require.NotNil(t, agent.AppliedConfig.ThinkingLevel)
		assert.Equal(t, 7, *agent.AppliedConfig.ThinkingLevel)
	})
	t.Run("thinking level: hub when project unset", func(t *testing.T) {
		f := newTiersFixture(t, true)
		hubTL := 3
		setHubAgentDefaults(f.srv, opsettings.AgentDefaultsSettings{DefaultThinkingLevel: &hubTL})
		f.project.Annotations = map[string]string{}
		agent := f.newAgent(t, t.Context(), "tl-hub", nil)
		require.NotNil(t, agent.AppliedConfig.ThinkingLevel)
		assert.Equal(t, 3, *agent.AppliedConfig.ThinkingLevel)
	})
	t.Run("harness config: project beats template beats hub", func(t *testing.T) {
		f := newTiersFixture(t, true)
		setHubAgentDefaults(f.srv, opsettings.AgentDefaultsSettings{DefaultHarnessConfig: "hub-hc"})
		f.template.DefaultHarnessConfig = "template-hc"
		f.project.Annotations = map[string]string{}
		assert.Equal(t, "template-hc", f.newAgent(t, t.Context(), "hc-template", nil).AppliedConfig.HarnessConfig)
		f.project.Annotations = map[string]string{projectSettingDefaultHarnessConfig: "project-hc"}
		assert.Equal(t, "project-hc", f.newAgent(t, t.Context(), "hc-project", nil).AppliedConfig.HarnessConfig)
		f.template.DefaultHarnessConfig = ""
		f.project.Annotations = map[string]string{}
		assert.Equal(t, "hub-hc", f.newAgent(t, t.Context(), "hc-hub", nil).AppliedConfig.HarnessConfig)
	})
	t.Run("limits: project inline, hub in HubAgentDefaults", func(t *testing.T) {
		f := newTiersFixture(t, true)
		ctx := t.Context()
		setHubAgentDefaults(f.srv, opsettings.AgentDefaultsSettings{DefaultMaxTurns: 99})
		f.project.Annotations = map[string]string{projectSettingDefaultMaxTurns: "40"}
		req, err := f.d.buildCreateRequest(ctx, f.newAgent(t, ctx, "limits-project", nil), "test")
		require.NoError(t, err)
		require.NotNil(t, req.InlineConfig)
		assert.Equal(t, 40, req.InlineConfig.MaxTurns)
		require.NotNil(t, req.Config.HubAgentDefaults)
		assert.Equal(t, 99, req.Config.HubAgentDefaults.MaxTurns)

		f.project.Annotations = map[string]string{}
		req, err = f.d.buildCreateRequest(ctx, f.newAgent(t, ctx, "limits-hub", nil), "test")
		require.NoError(t, err)
		if req.InlineConfig != nil {
			assert.Zero(t, req.InlineConfig.MaxTurns, "the hub limit never travels inline")
		}
		require.NotNil(t, req.Config.HubAgentDefaults)
		assert.Equal(t, 99, req.Config.HubAgentDefaults.MaxTurns)
	})
}

// TestTemplateTiers_RequiredSecretsFromSnapshot: the template's required
// secrets come from its stripped AgentConfig snapshot, not the row's Config.
func TestTemplateTiers_RequiredSecretsFromSnapshot(t *testing.T) {
	f := newTiersFixture(t, true)
	ctx := t.Context()
	req, err := f.d.buildCreateRequest(ctx, f.newAgent(t, ctx, "secrets", nil), "test")
	require.NoError(t, err)
	require.Len(t, req.RequiredSecrets, 1)
	assert.Equal(t, api.RequiredSecret{Key: "SNAP_SECRET", Description: "from the snapshot", Type: "environment"}, req.RequiredSecrets[0])
}

// TestTemplateJobConfig_ExcludesEnvironmentKeys is acceptance (b): values
// the hub derives from the snapshot exclude environment keys, and the
// stored snapshot stays a faithful parse.
func TestTemplateJobConfig_ExcludesEnvironmentKeys(t *testing.T) {
	f := newTiersFixture(t, true)
	jc := f.srv.templateJobConfig(t.Context(), f.template)
	require.NotNil(t, jc)
	assert.Empty(t, jc.Image)
	assert.Nil(t, jc.Telemetry)
	assert.Equal(t, map[string]string{"SNAP_PLAIN": "snap"}, jc.Env, "reserved env keys are stripped")
	assert.Len(t, jc.Secrets, 1, "job keys are kept")

	assert.Equal(t, "snap-image:v1", f.template.AgentConfig.Image, "the snapshot itself is not modified")
	assert.NotNil(t, f.template.AgentConfig.Telemetry)
}

// TestTemplateSkillRefs_FromSnapshot: dispatch skill pre-resolution reads
// the template's skills from the snapshot, with no storage read.
func TestTemplateSkillRefs_FromSnapshot(t *testing.T) {
	srv, s := testServer(t)
	ctx := t.Context()
	tmpl := &store.Template{
		ID: tid("skills-template"), Name: "Skills", Slug: "skills-template", Harness: "claude",
		Scope: store.TemplateScopeGlobal, Status: store.TemplateStatusActive, ContentHash: "skills-hash",
		AgentConfig: &api.ScionConfig{Skills: []api.SkillReference{{URI: "skill://scion/snap-skill"}}},
	}
	require.NoError(t, s.CreateTemplate(ctx, tmpl))
	refs := srv.templateSkillRefs(ctx, tmpl.ID)
	require.Len(t, refs, 1)
	assert.Equal(t, "skill://scion/snap-skill", refs[0].URI)
}

// TestCreateAgent_MessageModeFromSnapshot: the template's message_mode comes
// from its snapshot; the row's config.messageMode is not read, and an
// invalid snapshot mode is refused.
func TestCreateAgent_MessageModeFromSnapshot(t *testing.T) {
	cases := []struct {
		name, snapshot, row string
		wantCode            int
		wantMode            string
	}{
		{name: "snapshot mode applies", snapshot: store.MessageModeLineage, row: store.MessageModeBranch, wantCode: http.StatusCreated, wantMode: store.MessageModeLineage},
		{name: "row mode is ignored", row: store.MessageModeBranch, wantCode: http.StatusCreated, wantMode: store.MessageModeProject},
		{name: "invalid snapshot mode refused", snapshot: "bogus", wantCode: http.StatusBadRequest},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			disp := &createAgentDispatcher{createPhase: string(state.PhaseRunning)}
			srv, s, project := setupCreateAgentServer(t, disp)
			require.NoError(t, s.CreateTemplate(context.Background(), &store.Template{
				ID: tid("mm-template-" + t.Name()), Name: "mm-tmpl", Slug: "mm-tmpl", Harness: "claude",
				ContentHash: "d00dfeed", Scope: store.TemplateScopeGlobal, Status: store.TemplateStatusActive,
				Config:      &store.TemplateConfig{MessageMode: tc.row},
				AgentConfig: &api.ScionConfig{MessageMode: tc.snapshot},
			}))
			rec := doRequest(t, srv, http.MethodPost, "/api/v1/agents", CreateAgentRequest{
				Name: "mm-agent", ProjectID: project.ID, Template: "mm-tmpl", Task: "x",
			})
			require.Equal(t, tc.wantCode, rec.Code, rec.Body.String())
			if tc.wantCode != http.StatusCreated {
				return
			}
			var resp CreateAgentResponse
			require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
			assert.Equal(t, tc.wantMode, resp.Agent.MessageMode)
		})
	}
}

// TestValidateMessageMode_MatchesStore pins api.ValidateMessageMode (used by
// LoadConfig) to the hub's store message modes.
func TestValidateMessageMode_MatchesStore(t *testing.T) {
	for _, m := range []string{store.MessageModeNone, store.MessageModeLineage, store.MessageModeBranch, store.MessageModeProject, store.MessageModeHub} {
		assert.NoError(t, api.ValidateMessageMode(m), m)
		assert.True(t, store.IsValidMessageMode(m), m)
	}
	assert.Error(t, api.ValidateMessageMode("bogus"))
	assert.False(t, store.IsValidMessageMode("bogus"))
	assert.NoError(t, api.ValidateMessageMode(""))
}

// tiersCommittedTemplate commits a template with an agent config file, then
// clears its snapshot the way a row committed before the snapshot existed
// looks.
func tiersCommittedTemplate(t *testing.T, srv *Server, s store.Store, name string, files map[string]string) *store.Template {
	t.Helper()
	ctx := context.Background()
	tmpl := seedCommittedTemplate(t, srv, name, store.TemplateScopeGlobal, "", files)
	row, err := s.GetTemplate(ctx, tmpl.ID)
	require.NoError(t, err)
	row.AgentConfig = nil
	require.NoError(t, setTemplateContentForTest(ctx, s, row))
	row, err = s.GetTemplate(ctx, tmpl.ID)
	require.NoError(t, err)
	require.Nil(t, row.AgentConfig, "precondition: a row without a snapshot")
	return row
}

// TestTemplateAgentConfig_LazyDeriveAndBackfill: a row without a snapshot is
// derived lazily on first read and persisted; the startup backfill derives
// the rest, and a second backfill does nothing (idempotent).
func TestTemplateAgentConfig_LazyDeriveAndBackfill(t *testing.T) {
	ctx := context.Background()
	srv, s := newCommitTestServer(t, newCommitTestStorage(t))
	agentYAML := "model: lazy-model\nmessage_mode: lineage\nimage: lazy-image:v1\n"

	lazy := tiersCommittedTemplate(t, srv, "lazy-tmpl", map[string]string{"scion-agent.yaml": agentYAML})
	jc := srv.templateJobConfigByID(ctx, lazy.ID)
	require.NotNil(t, jc, "a missing snapshot is derived at read time")
	assert.Equal(t, "lazy-model", jc.Model)
	assert.Equal(t, store.MessageModeLineage, jc.MessageMode)
	assert.Empty(t, jc.Image, "the derived job config excludes environment keys")
	row, err := s.GetTemplate(ctx, lazy.ID)
	require.NoError(t, err)
	require.NotNil(t, row.AgentConfig, "the derived snapshot is persisted")
	assert.Equal(t, "lazy-image:v1", row.AgentConfig.Image, "the stored snapshot stays a faithful parse")
	assert.Equal(t, lazy.ContentHash, row.ContentHash, "content is unchanged")

	pending := tiersCommittedTemplate(t, srv, "backfill-tmpl", map[string]string{"scion-agent.yaml": "model: backfill-model\n"})
	_ = seedCommittedTemplate(t, srv, "no-config-tmpl", store.TemplateScopeGlobal, "", map[string]string{"README.md": "hi"})

	assert.Equal(t, 1, srv.BackfillTemplateAgentConfigs(ctx), "only the row without a snapshot is derived")
	row, err = s.GetTemplate(ctx, pending.ID)
	require.NoError(t, err)
	require.NotNil(t, row.AgentConfig)
	assert.Equal(t, "backfill-model", row.AgentConfig.Model)
	version := row.Updated
	assert.Equal(t, 0, srv.BackfillTemplateAgentConfigs(ctx), "a second backfill does nothing")
	row, err = s.GetTemplate(ctx, pending.ID)
	require.NoError(t, err)
	assert.Equal(t, version, row.Updated, "a second backfill writes nothing")
}

// TestBrokerCapabilities_TemplateTiersIngested: the hub reads templateTiers
// from a registration's capability names and from a heartbeat.
func TestBrokerCapabilities_TemplateTiersIngested(t *testing.T) {
	for _, name := range []string{"templateTiers", "template_tiers", "TemplateTiers"} {
		assert.True(t, capabilitiesFromStrings([]string{name}).TemplateTiers, name)
	}
	assert.False(t, capabilitiesFromStrings([]string{"sync"}).TemplateTiers)

	blob, err := json.Marshal(map[string]interface{}{
		"status":       "online",
		"capabilities": hubclient.BrokerCapabilities{TemplateTiers: true},
	})
	require.NoError(t, err)
	var hb brokerHeartbeatRequest
	require.NoError(t, json.Unmarshal(blob, &hb))
	require.NotNil(t, hb.Capabilities)
	assert.True(t, hb.Capabilities.TemplateTiers, "a heartbeat's templateTiers reaches the stored capabilities")
}

// TestTemplateTiers_WireCompatibleWithBroker: the hub telemetry default and
// the telemetry policy decode on the broker side under the names TS-2a reads.
func TestTemplateTiers_WireCompatibleWithBroker(t *testing.T) {
	on, off := true, false
	blob, err := json.Marshal(&RemoteAgentConfig{
		HubAgentDefaults: &RemoteHubAgentDefaults{Telemetry: &api.TelemetryConfig{Enabled: &on, Cloud: &api.TelemetryCloudConfig{Endpoint: tiersHubEndpoint}}},
		TelemetryPolicy:  &off,
	})
	require.NoError(t, err)
	var got runtimebroker.CreateAgentConfig
	require.NoError(t, json.Unmarshal(blob, &got))
	require.NotNil(t, got.HubAgentDefaults)
	require.NotNil(t, got.HubAgentDefaults.TelemetryDefault())
	assert.Equal(t, tiersHubEndpoint, got.HubAgentDefaults.TelemetryDefault().Cloud.Endpoint)
	require.NotNil(t, got.TelemetryPolicy)
	assert.False(t, *got.TelemetryPolicy)

	payload := map[string]interface{}{}
	applyStartExtras(payload, StartExtras{TelemetryPolicy: &off})
	assert.Equal(t, false, payload["telemetryPolicy"])
	payload = map[string]interface{}{}
	applyStartExtras(payload, StartExtras{})
	_, ok := payload["telemetryPolicy"]
	assert.False(t, ok, "no policy key without a policy")
}

// TestTelemetryDispatch_Units covers the delivery helpers directly.
func TestTelemetryDispatch_Units(t *testing.T) {
	on, off := true, false
	hub := &api.TelemetryConfig{Enabled: &on, Cloud: &api.TelemetryCloudConfig{Endpoint: tiersHubEndpoint}}
	requester := &api.TelemetryConfig{Enabled: &on, Cloud: &api.TelemetryCloudConfig{Endpoint: tiersReqEndpoint}}

	legacy := telemetryDispatch{hubDefault: hub, policy: &off}
	tel := legacy.inlineTelemetry()
	require.NotNil(t, tel)
	assert.Equal(t, tiersHubEndpoint, tel.Cloud.Endpoint)
	assert.False(t, *tel.Enabled)
	assert.True(t, *hub.Enabled, "the hub default is never modified")
	assert.Nil(t, legacy.telemetryPolicy())
	assert.Nil(t, legacy.hubAgentDefaults(nil))

	legacyReq := telemetryDispatch{hubDefault: hub, requester: requester}
	assert.Equal(t, tiersReqEndpoint, legacyReq.inlineTelemetry().Cloud.Endpoint, "the requester beats the hub default inline")

	capable := telemetryDispatch{templateTiers: true, hubDefault: hub, policy: &off, requester: requester}
	assert.Nil(t, capable.inlineTelemetry())
	stored := &api.ScionConfig{Model: "m", Telemetry: requester}
	out := capable.inlineConfig(stored)
	assert.Nil(t, out.Telemetry)
	assert.Equal(t, "m", out.Model)
	assert.Same(t, requester, stored.Telemetry, "the stored config is never modified")
	h := capable.hubAgentDefaults(nil)
	require.NotNil(t, h)
	assert.Equal(t, tiersHubEndpoint, h.Telemetry.Cloud.Endpoint)
	require.NotNil(t, capable.telemetryPolicy())
	assert.False(t, *capable.telemetryPolicy())

	env := map[string]string{}
	var cls map[string]api.EnvKind
	capable.injectRequesterEnabled(env, &cls)
	assert.Equal(t, "true", env[envTelemetryEnabled])
	env = map[string]string{envTelemetryEnabled: "false"}
	capable.injectRequesterEnabled(env, &cls)
	assert.Equal(t, "false", env[envTelemetryEnabled], "an explicit env flag is kept")

	assert.Nil(t, telemetryDispatch{}.inlineConfig(nil))
	assert.Equal(t, []string{"cloud", "hub", "resource"}, requesterTelemetryDroppedFields(&api.TelemetryConfig{
		Enabled: &on, Cloud: &api.TelemetryCloudConfig{}, Hub: &api.TelemetryHubConfig{}, Resource: map[string]string{"a": "b"},
	}))
	assert.Nil(t, requesterTelemetryDroppedFields(&api.TelemetryConfig{Enabled: &on}))
}
