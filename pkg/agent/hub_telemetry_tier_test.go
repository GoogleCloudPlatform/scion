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

package agent

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/runtime"
)

// Tests for the hub default telemetry tier (ptone/scion#4218, TS-2a):
// broker settings telemetry < hub default telemetry < agent config
// (template chain + stored scion-agent.json + inline), applied per Start and
// never persisted to scion-agent.json.

// hubTelemetryFixture sets up an isolated HOME with a settings.yaml whose
// telemetry block is settingsTelemetry (YAML, may be empty), and returns the
// project .scion dir.
func hubTelemetryFixture(t *testing.T, settingsTelemetry string) string {
	t.Helper()
	for _, e := range os.Environ() {
		if strings.HasPrefix(e, "SCION_") {
			k := strings.SplitN(e, "=", 2)[0]
			t.Setenv(k, "")
			os.Unsetenv(k) //nolint:errcheck
		}
	}
	tmpDir := t.TempDir()
	oldWd, _ := os.Getwd()
	_ = os.Chdir(tmpDir)
	t.Cleanup(func() { _ = os.Chdir(oldWd) })
	t.Setenv("HOME", tmpDir)

	globalScionDir := filepath.Join(tmpDir, ".scion")
	hcDir := filepath.Join(globalScionDir, "harness-configs", "test-harness")
	_ = os.MkdirAll(hcDir, 0755)
	_ = os.WriteFile(filepath.Join(hcDir, "config.yaml"), []byte("harness: gemini\nuser: scion\nimage: test-image:latest\n"), 0644)
	tplDir := filepath.Join(globalScionDir, "templates", "default")
	_ = os.MkdirAll(tplDir, 0755)
	_ = os.WriteFile(filepath.Join(tplDir, "scion-agent.json"), []byte(`{"default_harness_config": "test-harness"}`), 0644)
	_ = os.WriteFile(filepath.Join(globalScionDir, "settings.yaml"), []byte(`schema_version: "1"
active_profile: local
profiles:
  local:
    runtime: docker
`+settingsTelemetry), 0644)

	projectScionDir := filepath.Join(tmpDir, "project", ".scion")
	_ = os.MkdirAll(projectScionDir, 0755)
	return projectScionDir
}

// writeHubTelemetryAgent writes the agent's stored config. Its telemetry
// block stands for the template's telemetry: block, which ProvisionAgent
// folds into scion-agent.json.
func writeHubTelemetryAgent(t *testing.T, projectScionDir, name, cfgJSON string) string {
	t.Helper()
	agentDir := filepath.Join(projectScionDir, "agents", name)
	_ = os.MkdirAll(filepath.Join(agentDir, "home"), 0755)
	if err := os.WriteFile(filepath.Join(agentDir, "scion-agent.json"), []byte(cfgJSON), 0644); err != nil {
		t.Fatal(err)
	}
	return agentDir
}

// startWithHubTelemetry runs Manager.Start with hd on the context and returns
// the env handed to the container and the captured run config.
func startWithHubTelemetry(t *testing.T, projectScionDir, name string, hd *api.HubAgentDefaults, mutate func(*api.StartOptions)) (map[string]string, runtime.RunConfig) {
	t.Helper()
	var captured runtime.RunConfig
	mockRT := &runtime.MockRuntime{
		ListFunc: func(ctx context.Context, labelFilter map[string]string) ([]api.AgentInfo, error) {
			return []api.AgentInfo{}, nil
		},
		RunFunc: func(ctx context.Context, config runtime.RunConfig) (string, error) {
			captured = config
			return "mock-id", nil
		},
	}
	env := map[string]string{}
	opts := api.StartOptions{
		Name:        name,
		ProjectPath: projectScionDir,
		NoAuth:      true,
		Env:         env,
	}
	if mutate != nil {
		mutate(&opts)
	}
	ctx := context.Background()
	if hd != nil {
		ctx = api.ContextWithHubAgentDefaults(ctx, hd)
	}
	if _, err := NewManager(mockRT).Start(ctx, opts); err != nil {
		t.Fatalf("Start failed: %v", err)
	}
	return opts.Env, captured
}

func hubTelemetryDefault(endpoint string) *api.HubAgentDefaults {
	return &api.HubAgentDefaults{Telemetry: &api.TelemetryConfig{
		Cloud: &api.TelemetryCloudConfig{
			Endpoint: endpoint,
			Protocol: "grpc",
			Provider: "gcp",
		},
		Hub: &api.TelemetryHubConfig{ReportInterval: "45s"},
	}}
}

const hubTierSettingsTelemetry = `telemetry:
  cloud:
    endpoint: settings:4317
    protocol: http
    batch:
      timeout: 9s
`

func assertEnv(t *testing.T, env map[string]string, key, want string) {
	t.Helper()
	if got := env[key]; got != want {
		t.Errorf("%s = %q, want %q", key, got, want)
	}
}

// Acceptance 1: template fields win per field; hub fields fill the rest; and
// the hub default sits above broker settings.
func TestStart_HubTelemetryDefault_TemplateWinsPerField(t *testing.T) {
	project := hubTelemetryFixture(t, hubTierSettingsTelemetry)
	writeHubTelemetryAgent(t, project, "tpl-block", `{
		"harness": "gemini",
		"telemetry": {"cloud": {"endpoint": "template:4317"}}
	}`)

	env, _ := startWithHubTelemetry(t, project, "tpl-block", hubTelemetryDefault("hub:4317"), nil)

	assertEnv(t, env, "SCION_OTEL_ENDPOINT", "template:4317")       // template wins
	assertEnv(t, env, "SCION_OTEL_PROTOCOL", "grpc")                // hub beats settings
	assertEnv(t, env, "SCION_TELEMETRY_CLOUD_PROVIDER", "gcp")      // hub fills
	assertEnv(t, env, "SCION_TELEMETRY_HUB_REPORT_INTERVAL", "45s") // hub fills
	assertEnv(t, env, "SCION_TELEMETRY_CLOUD_BATCH_TIMEOUT", "9s")  // settings fills below hub
}

// Acceptance 2: no template block -> the hub default applies in full; no hub
// default -> unchanged (settings telemetry as before).
func TestStart_HubTelemetryDefault_NoTemplateBlock(t *testing.T) {
	t.Run("hub default applies in full", func(t *testing.T) {
		project := hubTelemetryFixture(t, "")
		writeHubTelemetryAgent(t, project, "no-block", `{"harness": "gemini"}`)

		env, _ := startWithHubTelemetry(t, project, "no-block", hubTelemetryDefault("hub:4317"), nil)

		assertEnv(t, env, "SCION_OTEL_ENDPOINT", "hub:4317")
		assertEnv(t, env, "SCION_OTEL_PROTOCOL", "grpc")
		assertEnv(t, env, "SCION_TELEMETRY_CLOUD_PROVIDER", "gcp")
		assertEnv(t, env, "SCION_TELEMETRY_HUB_REPORT_INTERVAL", "45s")
	})
	t.Run("no hub default: unchanged", func(t *testing.T) {
		project := hubTelemetryFixture(t, hubTierSettingsTelemetry)
		writeHubTelemetryAgent(t, project, "no-hub", `{"harness": "gemini"}`)

		env, _ := startWithHubTelemetry(t, project, "no-hub", nil, nil)

		assertEnv(t, env, "SCION_OTEL_ENDPOINT", "settings:4317")
		assertEnv(t, env, "SCION_OTEL_PROTOCOL", "http")
		assertEnv(t, env, "SCION_TELEMETRY_CLOUD_PROVIDER", "")
	})
}

// Acceptance 3: the hub default is not written to scion-agent.json (even on
// the Start paths that rewrite it), and after a restart with a changed hub
// default the new default applies.
func TestStart_HubTelemetryDefault_NotPersistedAndRestartSeesChange(t *testing.T) {
	project := hubTelemetryFixture(t, "")
	agentDir := writeHubTelemetryAgent(t, project, "persist", `{"harness": "gemini"}`)

	// HarnessAuth makes Start rewrite scion-agent.json.
	withAuth := func(o *api.StartOptions) { o.HarnessAuth = "vertex-ai" }
	env, _ := startWithHubTelemetry(t, project, "persist", hubTelemetryDefault("hub-a:4317"), withAuth)
	assertEnv(t, env, "SCION_OTEL_ENDPOINT", "hub-a:4317")

	data, err := os.ReadFile(filepath.Join(agentDir, "scion-agent.json"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "vertex-ai") {
		t.Fatalf("test precondition: Start did not rewrite scion-agent.json: %s", data)
	}
	if strings.Contains(string(data), "hub-a") || strings.Contains(string(data), "45s") {
		t.Fatalf("hub default telemetry was persisted to scion-agent.json: %s", data)
	}

	// Restart with a changed hub default.
	env, _ = startWithHubTelemetry(t, project, "persist", hubTelemetryDefault("hub-b:4317"), withAuth)
	assertEnv(t, env, "SCION_OTEL_ENDPOINT", "hub-b:4317")
}

// The agent's own telemetry block survives the scion-agent.json rewrite
// while the hub default stays out of it.
func TestStart_HubTelemetryDefault_PersistKeepsAgentTelemetry(t *testing.T) {
	project := hubTelemetryFixture(t, "")
	agentDir := writeHubTelemetryAgent(t, project, "persist-keep", `{
		"harness": "gemini",
		"telemetry": {"cloud": {"endpoint": "template:4317"}}
	}`)
	startWithHubTelemetry(t, project, "persist-keep", hubTelemetryDefault("hub:4317"),
		func(o *api.StartOptions) { o.HarnessAuth = "vertex-ai" })

	data, err := os.ReadFile(filepath.Join(agentDir, "scion-agent.json"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "template:4317") {
		t.Errorf("agent telemetry lost from scion-agent.json: %s", data)
	}
	if strings.Contains(string(data), "hub:4317") || strings.Contains(string(data), `"gcp"`) {
		t.Errorf("hub default telemetry was persisted to scion-agent.json: %s", data)
	}
}

// Acceptance 4 (Start half): the override computed from TelemetryPolicy is
// applied last and beats a config enabled: true and the hub default; the hub
// default object on the context is never mutated by it (no aliasing).
func TestStart_TelemetryOverrideBeatsHubDefaultWithoutAliasing(t *testing.T) {
	project := hubTelemetryFixture(t, "")
	writeHubTelemetryAgent(t, project, "policy", `{
		"harness": "gemini",
		"telemetry": {"enabled": true}
	}`)
	hd := hubTelemetryDefault("hub:4317")
	enabled := true
	hd.Telemetry.Enabled = &enabled

	off := false
	env, rc := startWithHubTelemetry(t, project, "policy", hd, func(o *api.StartOptions) { o.TelemetryOverride = &off })

	if rc.TelemetryEnabled {
		t.Error("TelemetryEnabled = true, want false (override is applied last)")
	}
	assertEnv(t, env, "SCION_TELEMETRY_ENABLED", "false")
	if hd.Telemetry.Enabled == nil || !*hd.Telemetry.Enabled {
		t.Error("hub default on the context was mutated by Start")
	}
	if hd.Telemetry.Cloud.Endpoint != "hub:4317" {
		t.Error("hub default cloud config on the context was mutated by Start")
	}
}
