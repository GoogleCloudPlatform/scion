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
	"errors"
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
	"github.com/GoogleCloudPlatform/scion/pkg/config/opsettings"
	"github.com/GoogleCloudPlatform/scion/pkg/experiments"
	"github.com/GoogleCloudPlatform/scion/pkg/hubclient"
	"github.com/GoogleCloudPlatform/scion/pkg/runtime"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/knadh/koanf/providers/confmap"
	"github.com/knadh/koanf/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func ensureTestOps(t *testing.T, srv *Server, s store.Store) *OperationalSettings {
	t.Helper()
	if ops := srv.operationalSettings.Load(); ops != nil {
		return ops
	}
	ops := NewOperationalSettings(s, emptyKoanf(), emptyKoanf())
	_, err := ops.Refresh(t.Context())
	require.NoError(t, err)
	ops.server = srv
	srv.SetOperationalSettings(ops)
	ApplySnapshot(srv, ops.Snapshot())
	return ops
}

func setTelemetryEnforcementExperiment(t *testing.T, srv *Server, s store.Store, on bool) {
	t.Helper()
	ctx := t.Context()
	ops := ensureTestOps(t, srv, s)
	expDoc, err := json.Marshal(map[string]any{
		"overrides": map[string]bool{
			experiments.AgentPolicyTelemetryEnforcement: on,
		},
	})
	require.NoError(t, err)
	_, err = s.UpsertHubSetting(ctx, "experiments", json.RawMessage(expDoc), "test", -1, "managed")
	require.NoError(t, err)
	_, err = ops.Refresh(ctx)
	require.NoError(t, err)
	ApplySnapshot(srv, ops.Snapshot())
}

func TestTelemetryEnforced_AdminSettingsDB(t *testing.T) {
	srv, s := testServer(t)
	ops := ensureTestOps(t, srv, s)

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

	// Enforced=true with enabled=false is refused with 422 validation error.
	rr = putConfig(`{"telemetry":{"enabled":false,"enforced":true}}`)
	require.Equal(t, http.StatusUnprocessableEntity, rr.Code, rr.Body.String())
	assert.Contains(t, rr.Body.String(), "telemetry.enforced requires telemetry.enabled=true")
	assert.False(t, srv.telemetryEnforced())

	// Enforced=true with enabled omitted is refused with 422 validation error.
	rr = putConfig(`{"telemetry":{"enforced":true}}`)
	require.Equal(t, http.StatusUnprocessableEntity, rr.Code, rr.Body.String())
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

	// Partial telemetry save omitting enforced preserves stored enforced=true and keeps enforcement live.
	rr = putConfig(`{"telemetry":{"cloud":{"endpoint":"otel-partial.example.com:4317"}}}`)
	require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())
	assert.True(t, srv.telemetryEnforced())
	got = getServerConfigDB(t, srv, ops)
	require.NotNil(t, got.Telemetry)
	assert.True(t, got.Telemetry.Enforced, "partial telemetry save omitting enforced must keep stored enforced=true")

	// Partial telemetry save setting enabled=false while enforced=true is stored is refused with 422.
	rr = putConfig(`{"telemetry":{"enabled":false}}`)
	require.Equal(t, http.StatusUnprocessableEntity, rr.Code, rr.Body.String())
	assert.Contains(t, rr.Body.String(), "telemetry.enforced requires telemetry.enabled=true")
	assert.True(t, srv.telemetryEnforced())

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

// TestTelemetryEnforced_BootstrapAndInvalidDBRowIgnored verifies that
// telemetry.enforced is strictly DB-only and fail-closed:
//   - (a) settings.yaml / bootstrap koanf with enforced:true + enabled:true and
//     no DB row does not enable enforcement and strips enforced on extraction;
//   - (b) bootstrap enforced:true + enabled:false (and a hand-written invalid DB
//     row with enforced:true + enabled:false) never enables enforcement;
//   - (c) an admin PUT with enabled:true + enforced:true still enforces.
func TestTelemetryEnforced_BootstrapAndInvalidDBRowIgnored(t *testing.T) {
	ctx := t.Context()

	t.Run("bootstrap enabled=true enforced=true with no DB row is not enforced", func(t *testing.T) {
		srv, s := testServer(t)
		bootstrap := koanf.New(".")
		require.NoError(t, bootstrap.Load(confmap.Provider(map[string]any{
			"telemetry.enabled":  true,
			"telemetry.enforced": true,
		}, "."), nil))
		envK := koanf.New(".")
		require.NoError(t, envK.Load(confmap.Provider(map[string]any{
			"telemetry.enforced": true,
		}, "."), nil))

		ops := NewOperationalSettings(s, bootstrap, envK)
		ops.server = srv
		srv.SetOperationalSettings(ops)

		setTelemetryEnforcementExperiment(t, srv, s, true)

		snap := ops.Snapshot()
		assert.False(t, snap.TelemetryEnforced, "bootstrap telemetry.enforced must not populate snapshot")
		assert.False(t, srv.telemetryEnforced(), "bootstrap telemetry.enforced must not activate enforcement")
		assert.NotContains(t, snap.EnvOverrides, "telemetry.enforced", "DB-only key must not appear in env overrides")

		seededDoc, err := opsettings.ExtractSectionFromKoanf(bootstrap, "telemetry")
		require.NoError(t, err)
		var seeded map[string]any
		require.NoError(t, json.Unmarshal(seededDoc, &seeded))
		assert.NotContains(t, seeded, "enforced", "seeded telemetry row must strip enforced")

		// Admin PUT path still enforces when enabled=true + enforced=true.
		rr := httptest.NewRecorder()
		srv.handlePutServerConfigDB(rr, adminRequest(http.MethodPut, "/api/v1/admin/server-config",
			`{"telemetry":{"enabled":true,"enforced":true}}`), ops)
		require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())
		assert.True(t, ops.Snapshot().TelemetryEnforced)
		assert.True(t, srv.telemetryEnforced())
	})

	t.Run("bootstrap or invalid DB row with enabled=false enforced=true is never enforced", func(t *testing.T) {
		srv, s := testServer(t)
		bootstrap := koanf.New(".")
		require.NoError(t, bootstrap.Load(confmap.Provider(map[string]any{
			"telemetry.enabled":  false,
			"telemetry.enforced": true,
		}, "."), nil))

		ops := NewOperationalSettings(s, bootstrap, emptyKoanf())
		ops.server = srv
		srv.SetOperationalSettings(ops)

		setTelemetryEnforcementExperiment(t, srv, s, true)
		assert.False(t, ops.Snapshot().TelemetryEnforced, "bootstrap enabled=false + enforced=true must never enforce")
		assert.False(t, srv.telemetryEnforced())

		// Even a hand-written invalid DB row (enabled=false, enforced=true) fails closed.
		_, err := s.UpsertHubSetting(ctx, "telemetry", json.RawMessage(`{"enabled":false,"enforced":true}`), "test", -1, "managed")
		require.NoError(t, err)
		_, err = ops.Refresh(ctx)
		require.NoError(t, err)
		ApplySnapshot(srv, ops.Snapshot())

		assert.False(t, ops.Snapshot().TelemetryEnforced, "invalid DB row failing Resolve() must not enforce")
		assert.False(t, srv.telemetryEnforced())
	})
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

	// Experiment on + TelemetryEnforced=true:
	// (a) Full-object resend of an already-stored false is allowed (I1: does not
	//     block unrelated saves; stored false stays shadowed at dispatch).
	srv.mu.Lock()
	srv.config.TelemetryEnforced = true
	srv.mu.Unlock()

	rec = doRequest(t, srv, http.MethodPut, "/api/v1/projects/"+project.ID+"/settings",
		json.RawMessage(`{"telemetryEnabled":false,"defaultTemplate":"other-tpl"}`))
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var settings hubclient.ProjectSettings
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &settings))
	assert.Equal(t, "other-tpl", settings.DefaultTemplate)
	require.NotNil(t, settings.TelemetryEnabled)
	assert.False(t, *settings.TelemetryEnabled, "unchanged resend preserves shadowed stored false")

	// (b) Unrelated partial PUT also preserves stored false.
	rec = doRequest(t, srv, http.MethodPut, "/api/v1/projects/"+project.ID+"/settings",
		json.RawMessage(`{"defaultTemplate":"third-tpl"}`))
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	// (c) PUT telemetryEnabled=true is accepted.
	rec = doRequest(t, srv, http.MethodPut, "/api/v1/projects/"+project.ID+"/settings",
		json.RawMessage(`{"telemetryEnabled":true}`))
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	// (d) Changing from true → false returns 409 telemetry_enforced_by_hub.
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

	// (e) PUT telemetryEnabled=null (clear) is accepted.
	rec = doRequest(t, srv, http.MethodPut, "/api/v1/projects/"+project.ID+"/settings",
		json.RawMessage(`{"telemetryEnabled":null}`))
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var cleared hubclient.ProjectSettings
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &cleared))
	assert.Nil(t, cleared.TelemetryEnabled)

	// (f) Changing from null → false also returns 409 telemetry_enforced_by_hub.
	rec = doRequest(t, srv, http.MethodPut, "/api/v1/projects/"+project.ID+"/settings",
		json.RawMessage(`{"telemetryEnabled":false}`))
	require.Equal(t, http.StatusConflict, rec.Code, rec.Body.String())
}

func TestTelemetryEnforced_DispatchPolicy(t *testing.T) {
	srv, s := testServer(t)
	ctx := t.Context()

	on := true
	srv.mu.Lock()
	srv.config.TelemetryConfig = &api.TelemetryConfig{
		Enabled: &on,
		Cloud:   &api.TelemetryCloudConfig{Endpoint: "hub-default.example.com:4317"},
	}
	srv.mu.Unlock()

	project := &store.Project{
		ID: tid("enforced-proj"), Name: "Enforced Project", Slug: "enforced-proj",
		OwnerID: "dev@localhost", GitRemote: "https://example.com/enforced/repo.git",
		Annotations: map[string]string{projectSettingTelemetryEnabled: "false"},
	}
	require.NoError(t, s.CreateProject(ctx, project))
	broker := &store.RuntimeBroker{
		ID: tid("enforced-broker"), Name: "enforced-broker", Slug: "enforced-broker",
		Endpoint: "http://localhost:9800", Status: store.BrokerStatusOnline,
		Capabilities: &store.BrokerCapabilities{TemplateTiers: true},
	}
	require.NoError(t, s.CreateRuntimeBroker(ctx, broker))
	require.NoError(t, s.AddProjectProvider(ctx, &store.ProjectProvider{
		ProjectID: project.ID, BrokerID: broker.ID, BrokerName: broker.Name,
		LocalPath: "/home/user/enforced/.scion", Status: store.BrokerStatusOnline,
	}))

	client := &mockRuntimeBrokerClient{}
	d := NewHTTPAgentDispatcherWithClient(s, client, false, slog.Default())
	d.SetHubTelemetryProvider(srv.hubTelemetryDefault)
	d.SetTelemetryEnforcedProvider(srv.telemetryEnforced)
	d.SetHubAgentDefaultsProvider(srv.hubAgentDefaults)

	ac := srv.buildAppliedConfig(CreateAgentRequest{Workspace: "/tmp/enforced-ws"}, "dev@localhost", AgentRoleBaseline)
	ag := &store.Agent{
		ID: tid("enforced-dispatch"), Name: "enforced-dispatch", Slug: "enforced-dispatch",
		ProjectID: project.ID, OwnerID: "dev@localhost",
		RuntimeBrokerID: broker.ID, AppliedConfig: ac,
	}
	require.NoError(t, srv.deriveAgentConfig(ctx, ag, project, nil))

	// Experiment off + TelemetryEnforced=true: byte-identical to today (project false wins).
	srv.mu.Lock()
	srv.config.TelemetryEnforced = true
	srv.mu.Unlock()

	req, err := d.buildCreateRequest(ctx, ag, "test")
	require.NoError(t, err)
	require.NotNil(t, req.Config.TelemetryPolicy)
	assert.False(t, *req.Config.TelemetryPolicy, "experiment off must leave project policy untouched")

	// Experiment on + TelemetryEnforced=false: byte-identical to today (project false wins).
	setTelemetryEnforcementExperiment(t, srv, s, true)
	srv.mu.Lock()
	srv.config.TelemetryEnforced = false
	srv.mu.Unlock()

	req, err = d.buildCreateRequest(ctx, ag, "test")
	require.NoError(t, err)
	require.NotNil(t, req.Config.TelemetryPolicy)
	assert.False(t, *req.Config.TelemetryPolicy, "enforced=false must leave project policy untouched")

	// Experiment on + TelemetryEnforced=true: forces TelemetryPolicy=true on create, start, and restart.
	srv.mu.Lock()
	srv.config.TelemetryEnforced = true
	srv.mu.Unlock()

	req, err = d.buildCreateRequest(ctx, ag, "test")
	require.NoError(t, err)
	require.NotNil(t, req.Config.TelemetryPolicy)
	assert.True(t, *req.Config.TelemetryPolicy, "enforced=true must override project false on create")

	require.NoError(t, d.DispatchAgentStart(ctx, ag, "go", false))
	require.NotNil(t, client.lastStartExtras.TelemetryPolicy)
	assert.True(t, *client.lastStartExtras.TelemetryPolicy, "enforced=true must override project false on start")

	require.NoError(t, d.DispatchAgentRestart(ctx, ag))
	require.NotNil(t, client.lastRestartExtras.TelemetryPolicy)
	assert.True(t, *client.lastRestartExtras.TelemetryPolicy, "enforced=true must override project false on restart")

	// Project with no annotation (nil) also gets TelemetryPolicy=true when enforced.
	delete(project.Annotations, projectSettingTelemetryEnabled)
	require.NoError(t, s.UpdateProject(ctx, project))

	req, err = d.buildCreateRequest(ctx, ag, "test")
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
	ops := ensureTestOps(t, srv, s)
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
	hcDir := filepath.Join(globalScionDir, "harness-configs", "test-claude")
	require.NoError(t, os.MkdirAll(hcDir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(hcDir, "config.yaml"), []byte(
		"harness: claude\nuser: scion\nimage: test-image:latest\n"+
			"provisioner:\n  type: container-script\n  interface_version: 1\n  command: [\"python3\", \"/home/scion/.scion/harness/provision.py\"]\n",
	), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(hcDir, "provision.py"), []byte("#!/usr/bin/env python3\n"), 0o755))
	tplDir := filepath.Join(globalScionDir, "templates", "default")
	require.NoError(t, os.MkdirAll(tplDir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(tplDir, "scion-agent.json"),
		[]byte(`{"default_harness_config": "test-claude"}`), 0o644))
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
		[]byte(`{"harness": "claude", "harness_config": "test-claude", "telemetry": {"enabled": false}}`), 0o644))

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

func TestTelemetryEnforced_ResolvedSettings(t *testing.T) {
	srv, s := testServer(t)
	ctx := t.Context()
	project := createTestProjectForSettings(t, s)

	getResolved := func() hubclient.ResolvedProjectSettings {
		rec := doRequest(t, srv, http.MethodGet, "/api/v1/projects/"+project.ID+"/settings/resolved", nil)
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		var resolved hubclient.ResolvedProjectSettings
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resolved))
		return resolved
	}

	// Experiment off + TelemetryEnforced=true: unlocked, effective=null, storedValue=null.
	srv.mu.Lock()
	srv.config.TelemetryEnforced = true
	srv.mu.Unlock()
	res := getResolved()
	tel := res.Settings[projectSettingTelemetryEnabled]
	assert.Nil(t, tel.LockedBy)
	assert.Nil(t, tel.Effective)
	assert.Nil(t, tel.StoredValue)

	// Experiment on + TelemetryEnforced=true, no project annotation.
	setTelemetryEnforcementExperiment(t, srv, s, true)
	srv.mu.Lock()
	srv.config.TelemetryEnforced = true
	srv.mu.Unlock()
	res = getResolved()
	tel = res.Settings[projectSettingTelemetryEnabled]
	require.NotNil(t, tel.LockedBy)
	assert.Equal(t, "hub", *tel.LockedBy)
	assert.Equal(t, true, tel.Effective)
	assert.Nil(t, tel.StoredValue)
	// Other settings stay unlocked.
	other := res.Settings[projectSettingDefaultModel]
	assert.Nil(t, other.LockedBy)
	assert.Nil(t, other.Effective)
	assert.Nil(t, other.StoredValue)

	// Stored project telemetry-enabled=false → storedValue=false (shadowed).
	project.Annotations = map[string]string{projectSettingTelemetryEnabled: "false"}
	require.NoError(t, s.UpdateProject(ctx, project))
	res = getResolved()
	tel = res.Settings[projectSettingTelemetryEnabled]
	require.NotNil(t, tel.LockedBy)
	assert.Equal(t, "hub", *tel.LockedBy)
	assert.Equal(t, true, tel.Effective)
	assert.Equal(t, false, tel.StoredValue)

	// Stored project telemetry-enabled=true → not shadowed, storedValue=nil.
	project.Annotations = map[string]string{projectSettingTelemetryEnabled: "true"}
	require.NoError(t, s.UpdateProject(ctx, project))
	res = getResolved()
	tel = res.Settings[projectSettingTelemetryEnabled]
	require.NotNil(t, tel.LockedBy)
	assert.Equal(t, "hub", *tel.LockedBy)
	assert.Equal(t, true, tel.Effective)
	assert.Nil(t, tel.StoredValue)
}

func TestTelemetryEnforced_InvalidDBRowWarning(t *testing.T) {
	srv, s := testServer(t)
	ctx := t.Context()
	ops := ensureTestOps(t, srv, s)

	// Directly persist an invalid telemetry DB row (enforced=true with enabled=false).
	_, err := s.UpsertHubSetting(ctx, "telemetry", json.RawMessage(`{"enabled":false,"enforced":true}`), "test", -1, "managed")
	require.NoError(t, err)
	_, err = ops.Refresh(ctx)
	require.NoError(t, err)

	rr := httptest.NewRecorder()
	srv.handleGetServerConfigDB(rr, adminRequest(http.MethodGet, "/api/v1/admin/server-config", ""), ops)
	require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())

	var body struct {
		Telemetry                *opsettings.TelemetrySettings `json:"telemetry"`
		TelemetryEnforcedWarning string                        `json:"telemetry_enforced_warning"`
	}
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &body))
	if body.Telemetry != nil {
		assert.False(t, body.Telemetry.Enforced, "invalid row must not report enforced=true")
	}
	assert.NotEmpty(t, body.TelemetryEnforcedWarning, "L1: invalid enforced row must surface warning")

	// Valid row clears the warning.
	_, err = s.UpsertHubSetting(ctx, "telemetry", json.RawMessage(`{"enabled":true,"enforced":true}`), "test", -1, "managed")
	require.NoError(t, err)
	_, err = ops.Refresh(ctx)
	require.NoError(t, err)

	rr = httptest.NewRecorder()
	srv.handleGetServerConfigDB(rr, adminRequest(http.MethodGet, "/api/v1/admin/server-config", ""), ops)
	require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())
	body.Telemetry = nil
	body.TelemetryEnforcedWarning = ""
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &body))
	require.NotNil(t, body.Telemetry)
	assert.True(t, body.Telemetry.Enforced)
	assert.Empty(t, body.TelemetryEnforcedWarning)
}

func TestTelemetryEnforced_CreateAndPatchRefusal(t *testing.T) {
	srv, s := testServer(t)
	ctx := t.Context()
	project := createTestProjectForSettings(t, s)

	// Turn on experiment and enforcement.
	setTelemetryEnforcementExperiment(t, srv, s, true)
	srv.mu.Lock()
	srv.config.TelemetryEnforced = true
	srv.mu.Unlock()

	t.Run("create with config.telemetry.enabled=false returns 400 telemetry_enforced_by_hub", func(t *testing.T) {
		rec := doRequest(t, srv, http.MethodPost, "/api/v1/projects/"+project.ID+"/agents",
			json.RawMessage(`{"name":"tel-refuse-cfg","config":{"telemetry":{"enabled":false}}}`))
		require.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
		var errResp struct {
			Error struct {
				Code    string         `json:"code"`
				Details map[string]any `json:"details"`
			} `json:"error"`
		}
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &errResp))
		assert.Equal(t, ErrCodeTelemetryEnforcedByHub, errResp.Error.Code)
		assert.Equal(t, "config.telemetry.enabled", errResp.Error.Details["field"])
	})

	t.Run("create with SCION_TELEMETRY_ENABLED=false or 0 returns 400 telemetry_enforced_by_hub", func(t *testing.T) {
		for _, val := range []string{"false", "0", ""} {
			rec := doRequest(t, srv, http.MethodPost, "/api/v1/projects/"+project.ID+"/agents",
				json.RawMessage(`{"name":"tel-refuse-env","config":{"env":{"SCION_TELEMETRY_ENABLED":"`+val+`"}}}`))
			require.Equal(t, http.StatusBadRequest, rec.Code, "val=%q body=%s", val, rec.Body.String())
			var errResp struct {
				Error struct {
					Code    string         `json:"code"`
					Details map[string]any `json:"details"`
				} `json:"error"`
			}
			require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &errResp))
			assert.Equal(t, ErrCodeTelemetryEnforcedByHub, errResp.Error.Code)
			assert.Equal(t, "config.env.SCION_TELEMETRY_ENABLED", errResp.Error.Details["field"])
		}
	})

	t.Run("PATCH with config.telemetry.enabled=false returns 400 telemetry_enforced_by_hub", func(t *testing.T) {
		agent := &store.Agent{
			ID:        tid("agent-tel-patch"),
			Slug:      "agent-tel-patch",
			Name:      "agent-tel-patch",
			ProjectID: project.ID,
			Phase:     "stopped",
			AppliedConfig: &store.AgentAppliedConfig{
				InlineConfig: &api.ScionConfig{},
			},
		}
		require.NoError(t, s.CreateAgent(ctx, agent))

		rec := doRequest(t, srv, http.MethodPatch, "/api/v1/agents/"+agent.ID,
			json.RawMessage(`{"config":{"telemetry":{"enabled":false}}}`))
		require.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
		var errResp struct {
			Error struct {
				Code string `json:"code"`
			} `json:"error"`
		}
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &errResp))
		assert.Equal(t, ErrCodeTelemetryEnforcedByHub, errResp.Error.Code)

		// PATCH with telemetry.enabled=true is allowed.
		rec = doRequest(t, srv, http.MethodPatch, "/api/v1/agents/"+agent.ID,
			json.RawMessage(`{"config":{"telemetry":{"enabled":true}}}`))
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	})

	t.Run("PATCH resending already-stored telemetry.enabled=false is allowed (O6)", func(t *testing.T) {
		off := false
		agent := &store.Agent{
			ID:        tid("agent-tel-patch-stored-false"),
			Slug:      "agent-tel-patch-stored-false",
			Name:      "agent-tel-patch-stored-false",
			ProjectID: project.ID,
			Phase:     "stopped",
			AppliedConfig: &store.AgentAppliedConfig{
				CreateInputs: &store.AgentCreateInputs{
					InlineConfig: &api.ScionConfig{
						Telemetry: &api.TelemetryConfig{Enabled: &off},
					},
				},
				InlineConfig: &api.ScionConfig{
					Telemetry: &api.TelemetryConfig{Enabled: &off},
				},
			},
		}
		require.NoError(t, s.CreateAgent(ctx, agent))

		// Unchanged resend of already-stored false alongside another edit succeeds.
		rec := doRequest(t, srv, http.MethodPatch, "/api/v1/agents/"+agent.ID,
			json.RawMessage(`{"config":{"telemetry":{"enabled":false},"max_duration":"30m"}}`))
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

		// Changing to true, then back to false is refused with 400 telemetry_enforced_by_hub.
		rec = doRequest(t, srv, http.MethodPatch, "/api/v1/agents/"+agent.ID,
			json.RawMessage(`{"config":{"telemetry":{"enabled":true}}}`))
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

		rec = doRequest(t, srv, http.MethodPatch, "/api/v1/agents/"+agent.ID,
			json.RawMessage(`{"config":{"telemetry":{"enabled":false}}}`))
		require.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
		var errResp struct {
			Error struct {
				Code string `json:"code"`
			} `json:"error"`
		}
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &errResp))
		assert.Equal(t, ErrCodeTelemetryEnforcedByHub, errResp.Error.Code)
	})

	t.Run("PATCH refuses telemetry.enabled=false when only non-requester InlineConfig has false (L1)", func(t *testing.T) {
		off := false
		agent := &store.Agent{
			ID:        tid("agent-tel-patch-nonrequester-false"),
			Slug:      "agent-tel-patch-nonrequester-false",
			Name:      "agent-tel-patch-nonrequester-false",
			ProjectID: project.ID,
			Phase:     "stopped",
			AppliedConfig: &store.AgentAppliedConfig{
				InlineConfig: &api.ScionConfig{
					Telemetry: &api.TelemetryConfig{Enabled: &off},
				},
			},
		}
		require.NoError(t, s.CreateAgent(ctx, agent))

		rec := doRequest(t, srv, http.MethodPatch, "/api/v1/agents/"+agent.ID,
			json.RawMessage(`{"config":{"telemetry":{"enabled":false}}}`))
		require.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
		var errResp struct {
			Error struct {
				Code string `json:"code"`
			} `json:"error"`
		}
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &errResp))
		assert.Equal(t, ErrCodeTelemetryEnforcedByHub, errResp.Error.Code)
	})
}

func TestTelemetryEnforced_OldBrokerRefusal(t *testing.T) {
	srv, s := testServer(t)
	ctx := t.Context()

	project := createTestProjectForSettings(t, s)

	var brokerCalled bool
	brokerSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		brokerCalled = true
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"agent":{"id":"a","status":"running"}}`))
	}))
	defer brokerSrv.Close()

	oldBroker := &store.RuntimeBroker{
		ID:       tid("broker-no-tiers"),
		Name:     "broker-no-tiers",
		Slug:     "broker-no-tiers",
		Endpoint: brokerSrv.URL,
		Status:   store.BrokerStatusOnline,
		// Capabilities nil or TemplateTiers=false
		Capabilities: &store.BrokerCapabilities{Sync: true},
	}
	require.NoError(t, s.CreateRuntimeBroker(ctx, oldBroker))
	require.NoError(t, s.AddProjectProvider(ctx, &store.ProjectProvider{
		ProjectID:  project.ID,
		BrokerID:   oldBroker.ID,
		BrokerName: oldBroker.Name,
		Status:     store.BrokerStatusOnline,
	}))

	agent := &store.Agent{
		ID:              tid("agent-old-broker"),
		Slug:            "agent-old-broker",
		Name:            "agent-old-broker",
		ProjectID:       project.ID,
		RuntimeBrokerID: oldBroker.ID,
		AppliedConfig:   &store.AgentAppliedConfig{},
	}

	d := NewHTTPAgentDispatcher(s, true, slog.Default())
	d.SetTelemetryEnforcedProvider(srv.telemetryEnforced)

	// Experiment off + TelemetryEnforced=true: old broker is NOT refused.
	srv.mu.Lock()
	srv.config.TelemetryEnforced = true
	srv.mu.Unlock()
	require.NoError(t, d.telemetryEnforcementPrecheck(ctx, agent))

	// Experiment on + TelemetryEnforced=true: create, start, and restart all
	// refuse with typed telemetry_enforcement_unsupported and make zero broker calls.
	setTelemetryEnforcementExperiment(t, srv, s, true)
	srv.mu.Lock()
	srv.config.TelemetryEnforced = true
	srv.mu.Unlock()

	for _, tc := range []struct {
		name string
		call func() error
	}{
		{"create", func() error { _, err := d.DispatchAgentCreate(ctx, agent); return err }},
		{"start", func() error { return d.DispatchAgentStart(ctx, agent, "", false) }},
		{"restart", func() error { return d.DispatchAgentRestart(ctx, agent) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			brokerCalled = false
			err := tc.call()
			require.Error(t, err)
			var refusal *RuntimeTargetRefusal
			require.True(t, errors.As(err, &refusal), "expected RuntimeTargetRefusal, got %v", err)
			assert.Equal(t, api.LaunchCodeTelemetryEnforcementUnsupported, refusal.Code)
			assert.Equal(t, http.StatusPreconditionFailed, refusal.Status)
			assert.Equal(t, oldBroker.ID, refusal.Details["brokerId"])
			assert.False(t, brokerCalled, "broker must not be called when refused")
		})
	}

	// Upgrading broker to TemplateTiers=true passes precheck.
	oldBroker.Capabilities.TemplateTiers = true
	require.NoError(t, s.UpdateRuntimeBroker(ctx, oldBroker))
	require.NoError(t, d.telemetryEnforcementPrecheck(ctx, agent))
}
