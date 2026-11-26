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
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/agent"
	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/experiments"
	"github.com/GoogleCloudPlatform/scion/pkg/hubclient"
	"github.com/GoogleCloudPlatform/scion/pkg/runtime"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func setTelemetryEnforcementExperiment(t *testing.T, srv *Server, s store.Store, on bool) {
	t.Helper()
	ctx := t.Context()
	expDoc, err := json.Marshal(map[string]any{
		"overrides": map[string]bool{
			experiments.AgentPolicyTelemetryEnforcement: on,
		},
	})
	require.NoError(t, err)
	_, err = s.UpsertHubSetting(ctx, "experiments", json.RawMessage(expDoc), "test", -1, "managed")
	require.NoError(t, err)
	if ops := srv.OperationalSettings(); ops != nil {
		require.NoError(t, ops.ReloadFromStore(ctx))
		srv.ApplySnapshot(ops.Snapshot())
	}
}

func TestTelemetryEnforced_AdminSettingsDB(t *testing.T) {
	srv, s := testServer(t)
	ops := srv.OperationalSettings()
	require.NotNil(t, ops)

	putConfig := func(body string) *httptest.ResponseRecorder {
		rr := httptest.NewRecorder()
		srv.handlePutServerConfigDB(rr, adminRequest(http.MethodPut, "/api/v1/admin/server-config", body), ops)
		return rr
	}

	// Experiment off: setting telemetry.enforced=true is refused with 412.
	rr := putConfig(`{"telemetry":{"enabled":true,"enforced":true}}`)
	require.Equal(t, http.StatusPreconditionFailed, rr.Code, rr.Body.String())
	assert.Contains(t, rr.Body.String(), ErrCodeExperimentDisabled)
	assert.False(t, srv.telemetryEnforced())

	// Turn experiment on.
	setTelemetryEnforcementExperiment(t, srv, s, true)

	// Enforced=true with enabled=false is refused with 400 validation error.
	rr = putConfig(`{"telemetry":{"enabled":false,"enforced":true}}`)
	require.Equal(t, http.StatusBadRequest, rr.Code, rr.Body.String())
	assert.Contains(t, rr.Body.String(), "telemetry.enforced requires telemetry.enabled=true")
	assert.False(t, srv.telemetryEnforced())

	// Enforced=true with enabled omitted is refused with 400 validation error.
	rr = putConfig(`{"telemetry":{"enforced":true}}`)
	require.Equal(t, http.StatusBadRequest, rr.Code, rr.Body.String())
	assert.Contains(t, rr.Body.String(), "telemetry.enforced requires telemetry.enabled=true")
	assert.False(t, srv.telemetryEnforced())

	// Enforced=true with enabled=true succeeds and live-applies.
	rr = putConfig(`{"telemetry":{"enabled":true,"enforced":true,"cloud":{"endpoint":"otel.example.com:4317"}}}`)
	require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())
	assert.True(t, srv.telemetryEnforced())
	assert.True(t, srv.config.TelemetryEnforced)

	got := getServerConfigDB(t, srv, ops)
	require.NotNil(t, got.Telemetry)
	assert.True(t, got.Telemetry.Enforced)
	require.NotNil(t, got.Telemetry.Enabled)
	assert.True(t, *got.Telemetry.Enabled)

	// Turning the experiment off disables enforcement immediately without touching the stored row.
	setTelemetryEnforcementExperiment(t, srv, s, false)
	assert.False(t, srv.telemetryEnforced())

	// While experiment is off, echoing an already-stored enforced=true passes so other fields can be saved.
	rr = putConfig(`{"telemetry":{"enabled":true,"enforced":true,"cloud":{"endpoint":"otel2.example.com:4317"}}}`)
	require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())

	// Clearing enforced=false while experiment is off also passes and live-applies.
	rr = putConfig(`{"telemetry":{"enabled":true,"enforced":false}}`)
	require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())
	assert.False(t, srv.config.TelemetryEnforced)
	assert.False(t, srv.telemetryEnforced())
}

func TestTelemetryEnforced_ProjectSettingsPut(t *testing.T) {
	srv, s := testServer(t)
	project := createTestProjectForSettings(t, s)

	// Experiment off + TelemetryEnforced=true: PUT telemetryEnabled=false still succeeds.
	srv.mu.Lock()
	srv.config.TelemetryEnforced = true
	srv.mu.Unlock()

	rec := doRequest(t, srv, http.MethodPut, "/api/v1/projects/"+project.ID+"/settings",
		json.RawMessage(`{"telemetryEnabled":false}`))
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	// Experiment on + TelemetryEnforced=false: PUT telemetryEnabled=false still succeeds.
	setTelemetryEnforcementExperiment(t, srv, s, true)
	srv.mu.Lock()
	srv.config.TelemetryEnforced = false
	srv.mu.Unlock()

	rec = doRequest(t, srv, http.MethodPut, "/api/v1/projects/"+project.ID+"/settings",
		json.RawMessage(`{"telemetryEnabled":false}`))
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	// Experiment on + TelemetryEnforced=true: PUT telemetryEnabled=false returns 409 telemetry_enforced_by_hub.
	srv.mu.Lock()
	srv.config.TelemetryEnforced = true
	srv.mu.Unlock()

	rec = doRequest(t, srv, http.MethodPut, "/api/v1/projects/"+project.ID+"/settings",
		json.RawMessage(`{"telemetryEnabled":false}`))
	require.Equal(t, http.StatusConflict, rec.Code, rec.Body.String())
	var errResp struct {
		Error struct {
			Code    string         `json:"code"`
			Message string         `json:"message"`
			Details map[string]any `json:"details"`
		} `json:"error"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &errResp))
	assert.Equal(t, ErrCodeTelemetryEnforcedByHub, errResp.Error.Code)
	assert.Equal(t, projectSettingsFieldTelemetryEnabled, errResp.Error.Details["field"])

	// Unrelated partial PUT still succeeds even though stored project annotation is false.
	rec = doRequest(t, srv, http.MethodPut, "/api/v1/projects/"+project.ID+"/settings",
		json.RawMessage(`{"defaultTemplate":"other-tpl"}`))
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var settings hubclient.ProjectSettings
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &settings))
	assert.Equal(t, "other-tpl", settings.DefaultTemplate)
	require.NotNil(t, settings.TelemetryEnabled)
	assert.False(t, *settings.TelemetryEnabled, "unrelated partial PUT preserves stored value")

	// PUT telemetryEnabled=true is accepted.
	rec = doRequest(t, srv, http.MethodPut, "/api/v1/projects/"+project.ID+"/settings",
		json.RawMessage(`{"telemetryEnabled":true}`))
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	// PUT telemetryEnabled=null (clear) is accepted.
	rec = doRequest(t, srv, http.MethodPut, "/api/v1/projects/"+project.ID+"/settings",
		json.RawMessage(`{"telemetryEnabled":null}`))
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &settings))
	assert.Nil(t, settings.TelemetryEnabled)
}

func TestTelemetryEnforced_DispatchPolicy(t *testing.T) {
	f := newTiersFixture(t, tiersBrokerCapable)
	f.d.SetTelemetryEnforcedProvider(f.srv.telemetryEnforced)
	ctx := t.Context()

	agent := f.newAgent(t, ctx, "enforced-dispatch", nil)

	// Experiment off + TelemetryEnforced=true: byte-identical to today (project false wins).
	f.srv.mu.Lock()
	f.srv.config.TelemetryEnforced = true
	f.srv.mu.Unlock()

	req, err := f.d.buildCreateRequest(ctx, agent, "test")
	require.NoError(t, err)
	require.NotNil(t, req.Config.TelemetryPolicy)
	assert.False(t, *req.Config.TelemetryPolicy, "experiment off must leave project policy untouched")

	// Experiment on + TelemetryEnforced=false: byte-identical to today (project false wins).
	setTelemetryEnforcementExperiment(t, f.srv, f.s, true)
	f.srv.mu.Lock()
	f.srv.config.TelemetryEnforced = false
	f.srv.mu.Unlock()

	req, err = f.d.buildCreateRequest(ctx, agent, "test")
	require.NoError(t, err)
	require.NotNil(t, req.Config.TelemetryPolicy)
	assert.False(t, *req.Config.TelemetryPolicy, "enforced=false must leave project policy untouched")

	// Experiment on + TelemetryEnforced=true: forces TelemetryPolicy=true on create, start, and restart.
	f.srv.mu.Lock()
	f.srv.config.TelemetryEnforced = true
	f.srv.mu.Unlock()

	req, err = f.d.buildCreateRequest(ctx, agent, "test")
	require.NoError(t, err)
	require.NotNil(t, req.Config.TelemetryPolicy)
	assert.True(t, *req.Config.TelemetryPolicy, "enforced=true must override project false on create")

	require.NoError(t, f.d.DispatchAgentStart(ctx, agent, "go"))
	require.NotNil(t, f.client.lastStartExtras.TelemetryPolicy)
	assert.True(t, *f.client.lastStartExtras.TelemetryPolicy, "enforced=true must override project false on start")

	require.NoError(t, f.d.DispatchAgentRestart(ctx, agent))
	require.NotNil(t, f.client.lastRestartExtras.TelemetryPolicy)
	assert.True(t, *f.client.lastRestartExtras.TelemetryPolicy, "enforced=true must override project false on restart")

	// Project with no annotation (nil) also gets TelemetryPolicy=true when enforced.
	delete(f.project.Annotations, projectSettingTelemetryEnabled)
	require.NoError(t, f.s.UpdateProject(ctx, f.project))

	req, err = f.d.buildCreateRequest(ctx, agent, "test")
	require.NoError(t, err)
	require.NotNil(t, req.Config.TelemetryPolicy)
	assert.True(t, *req.Config.TelemetryPolicy, "enforced=true must set TelemetryPolicy=true when project policy is unset")
}

// TestTelemetryEnforced_E2E_ProjectFalseOverridesToEnabled exercises the P3a
// vertical slice end to end: admin opsettings PUT telemetry.enforced=true with
// live apply → Hub dispatch for a project with stored telemetryEnabled=false
// and requester enabled=false → broker TelemetryPolicy override → agent Manager
// Start with a real ContainerScriptHarness → container env has
// SCION_TELEMETRY_ENABLED=true and the native harness telemetry file is enabled.
func TestTelemetryEnforced_E2E_ProjectFalseOverridesToEnabled(t *testing.T) {
	for _, e := range os.Environ() {
		if strings.HasPrefix(e, "SCION_") {
			k := strings.SplitN(e, "=", 2)[0]
			t.Setenv(k, "")
			_ = os.Unsetenv(k)
		}
	}

	srv, s := testServer(t)
	ops := srv.OperationalSettings()
	require.NotNil(t, ops)
	ctx := t.Context()

	// 1. Enable experiment and save telemetry.enforced=true via admin opsettings API.
	setTelemetryEnforcementExperiment(t, srv, s, true)
	rr := httptest.NewRecorder()
	srv.handlePutServerConfigDB(rr, adminRequest(http.MethodPut, "/api/v1/admin/server-config",
		`{"telemetry":{"enabled":true,"enforced":true,"cloud":{"endpoint":"enforced.example.com:4317","protocol":"grpc"}}}`), ops)
	require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())
	require.True(t, srv.telemetryEnforced())

	// 2. Create project with stored telemetry-enabled=false and broker with templateTiers.
	project := &store.Project{
		ID: tid("e2e-tel-project"), Name: "E2E Telemetry Project", Slug: "e2e-tel-project",
		OwnerID: "dev@localhost", GitRemote: "https://example.com/e2e/repo.git",
		Annotations: map[string]string{projectSettingTelemetryEnabled: "false"},
	}
	require.NoError(t, s.CreateProject(ctx, project))
	broker := &store.RuntimeBroker{
		ID: tid("e2e-tel-broker"), Name: "e2e-tel-broker", Slug: "e2e-tel-broker",
		Endpoint: "http://localhost:9800", Status: store.BrokerStatusOnline,
		Capabilities: &store.BrokerCapabilities{TemplateTiers: true},
	}
	require.NoError(t, s.CreateRuntimeBroker(ctx, broker))
	require.NoError(t, s.AddProjectProvider(ctx, &store.ProjectProvider{
		ProjectID: project.ID, BrokerID: broker.ID, BrokerName: broker.Name,
		LocalPath: "/home/user/e2e/.scion", Status: store.BrokerStatusOnline,
	}))

	// 3. Build dispatch payload for an agent whose stored requester config asked for enabled=false.
	client := &mockRuntimeBrokerClient{}
	d := NewHTTPAgentDispatcherWithClient(s, client, false, slog.Default())
	d.SetHubTelemetryProvider(srv.hubTelemetryDefault)
	d.SetTelemetryEnforcedProvider(srv.telemetryEnforced)
	d.SetHubAgentDefaultsProvider(srv.hubAgentDefaults)

	off := false
	ac := srv.buildAppliedConfig(CreateAgentRequest{
		Config:    &api.ScionConfig{Telemetry: &api.TelemetryConfig{Enabled: &off}},
		Workspace: "/tmp/e2e-ws",
	}, "dev@localhost", AgentRoleBaseline)
	ag := &store.Agent{
		ID: tid("e2e-tel-agent"), Name: "e2e-tel-agent", Slug: "e2e-tel-agent",
		ProjectID: project.ID, OwnerID: "dev@localhost",
		RuntimeBrokerID: broker.ID, AppliedConfig: ac,
	}
	require.NoError(t, srv.deriveAgentConfig(ctx, ag, project, nil))

	createReq, err := d.buildCreateRequest(ctx, ag, "test")
	require.NoError(t, err)
	require.NotNil(t, createReq.Config)
	require.NotNil(t, createReq.Config.TelemetryPolicy, "Hub dispatch must send TelemetryPolicy")
	assert.True(t, *createReq.Config.TelemetryPolicy, "Hub enforcement must force TelemetryPolicy=true")
	require.NotNil(t, createReq.Config.HubAgentDefaults)
	require.NotNil(t, createReq.Config.HubAgentDefaults.Telemetry)

	// 4. Feed the Hub dispatch payload into the broker's TelemetryPolicy override
	//    and agent.Manager.Start with a real ContainerScriptHarness.
	tmpDir := t.TempDir()
	t.Chdir(tmpDir)
	t.Setenv("HOME", tmpDir)

	globalScionDir := filepath.Join(tmpDir, ".scion")
	hcDir := filepath.Join(globalScionDir, "harness-configs", "test-gemini")
	require.NoError(t, os.MkdirAll(hcDir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(hcDir, "config.yaml"),
		[]byte("harness: gemini\nuser: scion\nimage: test-image:latest\n"), 0o644))
	tplDir := filepath.Join(globalScionDir, "templates", "default")
	require.NoError(t, os.MkdirAll(tplDir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(tplDir, "scion-agent.json"),
		[]byte(`{"default_harness_config": "test-gemini"}`), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(globalScionDir, "settings.yaml"), []byte(`schema_version: "1"
active_profile: local
profiles:
  local:
    runtime: docker
`), 0o644))

	projectScionDir := filepath.Join(tmpDir, "project", ".scion")
	agentDir := filepath.Join(projectScionDir, "agents", "e2e-tel-agent")
	agentHome := filepath.Join(agentDir, "home")
	require.NoError(t, os.MkdirAll(agentHome, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(agentDir, "scion-agent.json"),
		[]byte(`{"harness": "gemini", "harness_config": "test-gemini", "telemetry": {"enabled": false}}`), 0o644))

	// Mirror runtimebroker.resolveTelemetryOverride: TelemetryPolicy takes precedence
	// and rewrites SCION_TELEMETRY_ENABLED in env.
	env := make(map[string]string, len(createReq.ResolvedEnv))
	for k, v := range createReq.ResolvedEnv {
		env[k] = v
	}
	var telemetryOverride *bool
	if createReq.Config.TelemetryPolicy != nil {
		v := *createReq.Config.TelemetryPolicy
		telemetryOverride = &v
		if _, exists := env[envTelemetryEnabled]; exists {
			env[envTelemetryEnabled] = strconv.FormatBool(v)
		}
	}

	var captured runtime.RunConfig
	mockRT := &runtime.MockRuntime{
		ListFunc: func(ctx context.Context, labelFilter map[string]string) ([]api.AgentInfo, error) {
			return []api.AgentInfo{}, nil
		},
		RunFunc: func(ctx context.Context, cfg runtime.RunConfig) (string, error) {
			captured = cfg
			return "mock-container-id", nil
		},
	}

	hd := &api.HubAgentDefaults{
		Telemetry: createReq.Config.HubAgentDefaults.Telemetry,
	}
	startCtx := api.ContextWithHubAgentDefaults(ctx, hd)
	opts := api.StartOptions{
		Name:              "e2e-tel-agent",
		ProjectPath:       projectScionDir,
		NoAuth:            true,
		Env:               env,
		TelemetryOverride: telemetryOverride,
	}
	_, err = agent.NewManager(mockRT).Start(startCtx, opts)
	require.NoError(t, err)

	// 5. Verify container env and native harness telemetry file both show enabled.
	assert.True(t, captured.TelemetryEnabled, "RunConfig.TelemetryEnabled must be true")
	assert.Equal(t, "true", opts.Env[envTelemetryEnabled], "container env SCION_TELEMETRY_ENABLED must be true")
	assert.Equal(t, "enforced.example.com:4317", opts.Env["SCION_OTEL_ENDPOINT"])

	nativeTelemetryPath := filepath.Join(agentHome, ".scion", "harness", "inputs", "telemetry.json")
	nativeData, err := os.ReadFile(nativeTelemetryPath)
	require.NoError(t, err, "native harness telemetry input file must be written")
	var staged struct {
		Telemetry *api.TelemetryConfig `json:"telemetry"`
	}
	require.NoError(t, json.Unmarshal(nativeData, &staged))
	require.NotNil(t, staged.Telemetry)
	require.NotNil(t, staged.Telemetry.Enabled)
	assert.True(t, *staged.Telemetry.Enabled, "native harness telemetry file must have enabled=true")
	require.NotNil(t, staged.Telemetry.Cloud)
	assert.Equal(t, "enforced.example.com:4317", staged.Telemetry.Cloud.Endpoint)
}
