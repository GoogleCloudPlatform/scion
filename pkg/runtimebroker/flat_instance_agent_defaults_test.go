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

package runtimebroker

import (
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/brokeridentity"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
)

// Required-env extraction on a flat instance reads the instance's
// agent_defaults at the profile's rung
// (.design/flat-runtime-brokers/contract-appendix-p3.md FC5). These are the
// call-site tests for the three converted sites in extractRequiredEnvKeys:
// the auth type override, the empty-value env keys and the step-3c secret
// declarations. Each serves the request through the server's flat settings
// view (settingsView, so the settings it reads have no profiles), with no
// profile name and an instance value set: the former call-site guards
// (a non-empty profile name, a non-nil Profiles map) would skip the
// instance value.

// flatDefaultsSettings has an active profile whose values must not reach a
// flat request.
const flatDefaultsSettings = `
schema_version: "1"
active_profile: batch
harness_configs:
  claude:
    harness: claude
    secrets:
      - key: SHARED_SECRET
        description: from the harness config
profiles:
  batch:
    runtime: docker
    secrets:
      - key: PROFILE_SECRET
    harness_overrides:
      claude:
        env:
          PROFILE_EMPTY_KEY: ""
runtimes:
  docker:
    type: docker
`

// newFlatDefaultsServer returns a Runtime Broker serving a flat Docker
// instance whose agent_defaults are d, over flatDefaultsSettings and a
// claude harness config whose own auth type is api-key.
func newFlatDefaultsServer(t *testing.T, d *config.V1InstanceAgentDefaults) (*Server, string) {
	t.Helper()
	srv, _, projectDir := newTestServerWithHarnessConfig(t, "claude",
		"harness: claude\nimage: test-image\nuser: scion\nauth_selected_type: api-key\n"+claudeAuthBlock, flatDefaultsSettings)
	srv.config.FlatInstance = &FlatInstanceConfig{
		Identity: &brokeridentity.Identity{
			InstanceKey: "docker-1", RuntimeBrokerID: "rb-docker-1",
			RuntimeTarget: api.RuntimeTargetDescriptor{ID: "target-docker-1", Type: "docker"},
		},
		Instance: config.V1RuntimeBrokerInstanceConfig{Key: "docker-1", Name: "docker-1",
			RuntimeTarget: &config.V1RuntimeTargetConfig{Type: config.RuntimeTargetTypeDocker}, AgentDefaults: d},
	}
	if !srv.isFlat() {
		t.Fatal("the server must serve a flat instance")
	}
	return srv, projectDir
}

func flatDefaultsRequest(projectDir string, secrets ...api.RequiredSecret) CreateAgentRequest {
	return CreateAgentRequest{
		Name:            "flat-defaults",
		ProjectPath:     projectDir,
		RequiredSecrets: secrets,
		// No profile: a flat request never carries one.
		Config: &CreateAgentConfig{Template: "claude", HarnessConfig: "claude"},
	}
}

func requiredHas(list []string, want string) bool {
	for _, k := range list {
		if k == want {
			return true
		}
	}
	return false
}

// The view extractRequiredEnvKeys reads on this server has no profile tier,
// so these tests exercise the flat view, not a hand-built settings value.
func TestFlatDefaults_ServerReadsFlatView(t *testing.T) {
	srv, projectDir := newFlatDefaultsServer(t, &config.V1InstanceAgentDefaults{ImageRegistry: "r"})
	vs, _, err := config.LoadEffectiveSettings(projectDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(vs.Profiles) == 0 || vs.ActiveProfile != "batch" {
		t.Fatalf("the fixture must carry an active profile: %+v", vs.Profiles)
	}
	view := srv.settingsView(vs)
	if view.Profiles != nil || view.ActiveProfile != "" {
		t.Fatalf("the flat view has a profile tier: %+v", view)
	}
	if got := view.ResolveImageRegistry(""); got != "r" {
		t.Fatalf("the flat view does not carry the instance policy (registry %q)", got)
	}
}

// Auth type call site: the instance's agent_defaults.harness_overrides.claude
// .auth_selected_type (vertex-ai) overrides the harness config's own
// (api-key), so vertex-ai's keys are required.
func TestFlatDefaults_AuthTypeCallSiteUsesInstanceOverride(t *testing.T) {
	unsetHostGCPEnv(t)
	srv, projectDir := newFlatDefaultsServer(t, &config.V1InstanceAgentDefaults{
		HarnessOverrides: map[string]config.V1HarnessOverride{"claude": {AuthSelectedType: "vertex-ai"}},
	})
	required, secretInfo, _, _ := srv.extractRequiredEnvKeys(flatDefaultsRequest(projectDir), "")
	if !requiredHas(required, "GOOGLE_CLOUD_PROJECT") {
		t.Fatalf("required %v (secretInfo %v): want vertex-ai's GOOGLE_CLOUD_PROJECT, from the instance's auth_selected_type override", required, secretInfo)
	}
	if requiredHas(required, "ANTHROPIC_API_KEY") {
		t.Fatalf("required %v: the harness config's api-key auth must be overridden", required)
	}
}

// Empty-value env key call site: an empty value in any of the instance's
// harness overrides (here the gemini one, not the active claude config) is a
// required key, as a profile's was; the active profile's is not.
func TestFlatDefaults_EnvKeysCallSiteUsesInstanceOverrides(t *testing.T) {
	srv, projectDir := newFlatDefaultsServer(t, &config.V1InstanceAgentDefaults{
		HarnessOverrides: map[string]config.V1HarnessOverride{"gemini": {Env: map[string]string{"INSTANCE_EMPTY_KEY": "", "INSTANCE_SET_KEY": "x"}}},
	})
	required, _, _, _ := srv.extractRequiredEnvKeys(flatDefaultsRequest(projectDir), "")
	if !requiredHas(required, "INSTANCE_EMPTY_KEY") {
		t.Fatalf("required %v: want INSTANCE_EMPTY_KEY from the instance's harness overrides", required)
	}
	for _, k := range []string{"INSTANCE_SET_KEY", "PROFILE_EMPTY_KEY"} {
		if requiredHas(required, k) {
			t.Fatalf("required %v must not contain %s", required, k)
		}
	}
}

// Secrets call site (step 3c) and the W8 rung: the instance's secret
// declarations are required; for one key, the instance declaration (3c)
// overrides the settings harness config's (3b) and the template's (3d)
// overrides the instance's. The active profile's are not used.
func TestFlatDefaults_SecretsCallSiteUsesInstanceSecrets(t *testing.T) {
	srv, projectDir := newFlatDefaultsServer(t, &config.V1InstanceAgentDefaults{
		Secrets: []api.RequiredSecret{
			{Key: "INSTANCE_SECRET", Description: "from the instance", Type: "environment"},
			{Key: "SHARED_SECRET", Description: "from the instance"},
			{Key: "TEMPLATE_SECRET", Description: "from the instance"},
		},
	})
	required, secretInfo, _, _ := srv.extractRequiredEnvKeys(
		flatDefaultsRequest(projectDir, api.RequiredSecret{Key: "TEMPLATE_SECRET", Description: "from the template"}), "")
	if !requiredHas(required, "INSTANCE_SECRET") {
		t.Fatalf("required %v: want INSTANCE_SECRET from the instance's agent_defaults.secrets", required)
	}
	if got := secretInfo["INSTANCE_SECRET"]; got.Description != "from the instance" || got.Source != "settings" || got.Type != "environment" {
		t.Fatalf("INSTANCE_SECRET info %+v", got)
	}
	if got := secretInfo["SHARED_SECRET"].Description; got != "from the instance" {
		t.Fatalf("SHARED_SECRET description %q: the instance (3c) must override the harness config (3b)", got)
	}
	if got := secretInfo["TEMPLATE_SECRET"]; got.Description != "from the template" || got.Source != "template" {
		t.Fatalf("TEMPLATE_SECRET info %+v: the template (3d) must override the instance (3c)", got)
	}
	if requiredHas(required, "PROFILE_SECRET") {
		t.Fatalf("required %v: the active profile's secrets must not reach a flat request", required)
	}
}
