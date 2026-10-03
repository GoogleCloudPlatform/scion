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
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/hubclient"
	"github.com/GoogleCloudPlatform/scion/pkg/runtime"
	"github.com/GoogleCloudPlatform/scion/pkg/storage"
	"github.com/GoogleCloudPlatform/scion/pkg/store"
	"github.com/GoogleCloudPlatform/scion/pkg/templatecache"
)

// newTestHydrator returns a *templatecache.Hydrator suitable only for
// satisfying resolveHubConnection's non-nil check in tests that exercise
// hydrateTemplate's LocalStorage branch, which resolves before ever calling
// into the returned Hydrator.
func newTestHydrator(t *testing.T) *templatecache.Hydrator {
	t.Helper()
	cache, err := templatecache.New(t.TempDir(), 1<<20)
	if err != nil {
		t.Fatalf("templatecache.New: %v", err)
	}
	return templatecache.NewHydrator(cache, nil)
}

// claudeAuthBlock is the declarative auth metadata for the claude harness,
// matching the production harnesses/claude/config.yaml. Tests that need
// config-driven auth preflight include this block in their harness-config fixture.
const claudeAuthBlock = `auth:
  default_type: api-key
  types:
    api-key:
      required_env:
        - any_of: ["ANTHROPIC_API_KEY"]
    vertex-ai:
      required_env:
        - any_of: ["GOOGLE_CLOUD_PROJECT"]
        - any_of: ["GOOGLE_CLOUD_REGION", "CLOUD_ML_REGION", "GOOGLE_CLOUD_LOCATION"]
      required_files:
        - name: gcloud-adc
          type: file
          description: "Google Cloud Application Default Credentials (ADC) file for vertex-ai authentication"
          field: GoogleAppCredentials
          alternative_env_keys: ["GOOGLE_APPLICATION_CREDENTIALS"]
          skipped_when_gcp_service_account_assigned: true
          required: true
  autodetect:
    env:
      GOOGLE_APPLICATION_CREDENTIALS: vertex-ai
      GOOGLE_CLOUD_PROJECT: vertex-ai
      ANTHROPIC_API_KEY: api-key
    files:
      gcloud-adc: vertex-ai
`

// geminiAuthBlock is the declarative auth metadata for the gemini harness,
// matching the production harnesses/gemini-cli/config.yaml.
const geminiAuthBlock = `auth:
  default_type: api-key
  types:
    api-key:
      required_env:
        - any_of: ["GEMINI_API_KEY", "GOOGLE_API_KEY"]
    auth-file:
      required_files:
        - name: GEMINI_OAUTH_CREDS
          type: file
          target_suffix: "/.gemini/oauth_creds.json"
          field: OAuthCreds
    vertex-ai:
      required_env:
        - any_of: ["GOOGLE_CLOUD_PROJECT"]
        - any_of: ["GOOGLE_CLOUD_REGION", "CLOUD_ML_REGION", "GOOGLE_CLOUD_LOCATION"]
      required_files:
        - name: gcloud-adc
          type: file
          description: "Google Cloud Application Default Credentials (ADC) file for vertex-ai authentication"
          field: GoogleAppCredentials
          alternative_env_keys: ["GOOGLE_APPLICATION_CREDENTIALS"]
          skipped_when_gcp_service_account_assigned: true
          required: true
  autodetect:
    env:
      GEMINI_API_KEY: api-key
      GOOGLE_API_KEY: api-key
      GOOGLE_APPLICATION_CREDENTIALS: vertex-ai
      GOOGLE_CLOUD_PROJECT: vertex-ai
    files:
      GEMINI_OAUTH_CREDS: auth-file
      gcloud-adc: vertex-ai
`

// antigravityAuthBlock mirrors the production antigravity harness config.
// default_type is oauth-token (which has no env requirements), so autodetect
// must see GEMINI_API_KEY to select api-key instead.
const antigravityAuthBlock = `auth:
  default_type: oauth-token
  types:
    oauth-token:
      required_files:
        - name: AGY_TOKEN
          type: file
          description: "Antigravity OAuth token JSON file"
          target_suffix: "/.gemini/antigravity-cli/antigravity-oauth-token"
    api-key:
      required_env:
        - any_of: ["GEMINI_API_KEY", "GOOGLE_API_KEY"]
    vertex-ai:
      required_env:
        - any_of: ["GOOGLE_CLOUD_PROJECT"]
        - any_of: ["GOOGLE_CLOUD_LOCATION", "GOOGLE_CLOUD_REGION"]
      required_files:
        - name: gcloud-adc
          type: file
          description: "Google Cloud ADC file for vertex-ai authentication"
          alternative_env_keys: ["GOOGLE_APPLICATION_CREDENTIALS"]
          skipped_when_gcp_service_account_assigned: true
          required: true
  autodetect:
    env:
      GEMINI_API_KEY: api-key
      GOOGLE_API_KEY: api-key
      AGY_TOKEN: oauth-token
      GOOGLE_CLOUD_PROJECT: vertex-ai
    files:
      gcloud-adc: vertex-ai
`

// newTestServerWithProjectPath creates a test server with a temporary project path
// that has versioned settings with declared env vars.
func newTestServerWithProjectPath(t *testing.T, settingsYAML string) (*Server, *envCapturingManager, string) {
	t.Helper()

	// Isolate HOME so LoadEffectiveSettings does not merge the developer's
	// personal ~/.scion/settings.yaml (which may declare harness-config
	// auth_selected_type values that would override the test fixture).
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", "")

	// Create temp project directory with settings
	// LoadEffectiveSettings expects a dir that contains settings.yaml directly
	projectDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(projectDir, "settings.yaml"), []byte(settingsYAML), 0644); err != nil {
		t.Fatal(err)
	}

	// Create template directories so FindTemplateInProjectPath can resolve them.
	// Each template needs a scion-agent.yaml that sets harness_config so that
	// provisioning doesn't fall back to the embedded default (gemini).
	for _, tpl := range []string{"claude", "gemini", "default"} {
		tplDir := filepath.Join(projectDir, "templates", tpl)
		if err := os.MkdirAll(tplDir, 0755); err != nil {
			t.Fatal(err)
		}
		cfg := "harness_config: " + tpl + "\n"
		if err := os.WriteFile(filepath.Join(tplDir, "scion-agent.yaml"), []byte(cfg), 0644); err != nil {
			t.Fatal(err)
		}
	}

	// Create harness-config directories so FindHarnessConfigDir can resolve them.
	// The on-disk directory name is "harness-configs" (with hyphen).
	// Each needs a config.yaml with harness and image fields.
	for _, hc := range []string{"claude", "gemini"} {
		hcDir := filepath.Join(projectDir, "harness-configs", hc)
		if err := os.MkdirAll(hcDir, 0755); err != nil {
			t.Fatal(err)
		}
		cfg := "harness: " + hc + "\nimage: test-image:" + hc + "\n"
		if err := os.WriteFile(filepath.Join(hcDir, "config.yaml"), []byte(cfg), 0644); err != nil {
			t.Fatal(err)
		}
	}

	cfg := DefaultServerConfig()
	cfg.BrokerID = "test-broker-id"
	cfg.BrokerName = "test-host"
	cfg.Debug = true
	cfg.StateDir = t.TempDir()
	cfg.ForceRuntime = "mock"

	mgr := &envCapturingManager{}
	// NameFunc returns "docker" so resolveManagerForOpts matches the settings-resolved runtime.
	rt := &runtime.MockRuntime{NameFunc: func() string { return "docker" }}

	return New(cfg, mgr, rt), mgr, projectDir
}

// TestEnvGather_AllSatisfied tests the fast path: all required env keys are provided
// by the Hub and/or Broker, so the agent starts immediately (200/201).
func TestEnvGather_AllSatisfied(t *testing.T) {
	settings := `
schema_version: "1"
harness_configs:
  claude:
    harness: claude
    env:
      API_KEY: ""
profiles:
  default:
    runtime: mock
`
	srv, mgr, projectDir := newTestServerWithProjectPath(t, settings)

	body := `{
		"name": "test-agent",
		"id": "agent-uuid-123",
		"gatherEnv": true,
		"projectPath": "` + projectDir + `",
		"resolvedEnv": {"API_KEY": "sk-test-key", "ANTHROPIC_API_KEY": "sk-ant-key"},
		"config": {"template": "claude", "profile": "default"}
	}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/agents", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	srv.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", w.Code, w.Body.String())
	}

	// Agent should have started with the key
	if mgr.lastEnv == nil {
		t.Fatal("expected env to be set")
	}
	if mgr.lastEnv["API_KEY"] != "sk-test-key" {
		t.Errorf("expected API_KEY='sk-test-key', got %q", mgr.lastEnv["API_KEY"])
	}
}

// TestEnvGather_NeedsKeys tests the gather path: required env keys are missing,
// so the broker returns 202 with requirements.
func TestEnvGather_NeedsKeys(t *testing.T) {
	settings := `
schema_version: "1"
harness_configs:
  claude:
    harness: claude
    env:
      API_KEY: ""
      SECRET_TOKEN: ""
profiles:
  default:
    runtime: mock
`
	srv, _, projectDir := newTestServerWithProjectPath(t, settings)

	body := `{
		"name": "test-agent-gather",
		"id": "agent-uuid-456",
		"gatherEnv": true,
		"projectPath": "` + projectDir + `",
		"resolvedEnv": {"API_KEY": "sk-from-hub"},
		"config": {"template": "claude", "profile": "default"}
	}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/agents", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	srv.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusAccepted {
		t.Fatalf("expected 202, got %d: %s", w.Code, w.Body.String())
	}

	var envReqs EnvRequirementsResponse
	if err := json.Unmarshal(w.Body.Bytes(), &envReqs); err != nil {
		t.Fatal("failed to decode response:", err)
	}

	if envReqs.AgentID != "agent-uuid-456" {
		t.Errorf("expected agentId='agent-uuid-456', got %q", envReqs.AgentID)
	}

	// API_KEY should be in hubHas
	found := false
	for _, k := range envReqs.HubHas {
		if k == "API_KEY" {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("expected API_KEY in hubHas, got %v", envReqs.HubHas)
	}

	// SECRET_TOKEN should be in needs
	found = false
	for _, k := range envReqs.Needs {
		if k == "SECRET_TOKEN" {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("expected SECRET_TOKEN in needs, got %v", envReqs.Needs)
	}
}

// TestEnvGather_BrokerHasKey tests that the broker does NOT use its own
// environment to satisfy missing keys — broker env should not leak into
// hub-dispatched agents.
func TestEnvGather_BrokerHasKey(t *testing.T) {
	settings := `
schema_version: "1"
harness_configs:
  claude:
    harness: claude
    env:
      BROKER_LOCAL_KEY: ""
profiles:
  default:
    runtime: mock
`
	srv, _, projectDir := newTestServerWithProjectPath(t, settings)

	// Set the keys in the broker's own environment — these should NOT be used
	t.Setenv("BROKER_LOCAL_KEY", "broker-value")
	t.Setenv("ANTHROPIC_API_KEY", "broker-anthropic-key")

	body := `{
		"name": "test-agent-broker-env",
		"id": "agent-uuid-789",
		"gatherEnv": true,
		"projectPath": "` + projectDir + `",
		"config": {"template": "claude", "profile": "default"}
	}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/agents", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	srv.Handler().ServeHTTP(w, req)

	// Should return 202 (needs) because broker env is no longer used
	if w.Code != http.StatusAccepted {
		t.Fatalf("expected 202, got %d: %s", w.Code, w.Body.String())
	}

	var envReqs EnvRequirementsResponse
	if err := json.Unmarshal(w.Body.Bytes(), &envReqs); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}

	// BROKER_LOCAL_KEY should be in needs, not satisfied by broker env
	found := false
	for _, k := range envReqs.Needs {
		if k == "BROKER_LOCAL_KEY" {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("expected BROKER_LOCAL_KEY in needs, got needs=%v", envReqs.Needs)
	}

	// BrokerHas should be empty
	if len(envReqs.BrokerHas) > 0 {
		t.Errorf("expected BrokerHas to be empty, got %v", envReqs.BrokerHas)
	}
}

// TestEnvGather_BrokerReplay_DifferentInstance tests the broker-side replay
// scenario: create returns 202, then a second create with complete env starts
// the agent on a different Server instance (HA scenario).
func TestEnvGather_BrokerReplay_DifferentInstance(t *testing.T) {
	settings := `
schema_version: "1"
harness_configs:
  claude:
    harness: claude
    env:
      NEEDED_KEY: ""
profiles:
  default:
    runtime: mock
`
	// Phase 1: first broker instance returns 202
	srv1, _, projectDir := newTestServerWithProjectPath(t, settings)
	createBody := `{
		"name": "test-agent-replay",
		"id": "agent-uuid-replay",
		"gatherEnv": true,
		"projectPath": "` + projectDir + `",
		"config": {"template": "claude", "profile": "default"}
	}`
	createReq := httptest.NewRequest(http.MethodPost, "/api/v1/agents", strings.NewReader(createBody))
	createReq.Header.Set("Content-Type", "application/json")
	createW := httptest.NewRecorder()
	srv1.Handler().ServeHTTP(createW, createReq)
	if createW.Code != http.StatusAccepted {
		t.Fatalf("phase 1: expected 202, got %d: %s", createW.Code, createW.Body.String())
	}

	// Phase 2: different broker instance receives a full create with gathered env
	srv2, mgr2, _ := newTestServerWithProjectPath(t, settings)
	replayBody := `{
		"name": "test-agent-replay",
		"id": "agent-uuid-replay",
		"requestId": "replay-req-id",
		"gatherEnv": true,
		"projectPath": "` + projectDir + `",
		"config": {"template": "claude", "profile": "default"},
		"resolvedEnv": {"NEEDED_KEY": "gathered-value", "ANTHROPIC_API_KEY": "test-key"}
	}`
	replayReq := httptest.NewRequest(http.MethodPost, "/api/v1/agents", strings.NewReader(replayBody))
	replayReq.Header.Set("Content-Type", "application/json")
	replayW := httptest.NewRecorder()
	srv2.Handler().ServeHTTP(replayW, replayReq)

	if replayW.Code != http.StatusCreated {
		t.Fatalf("phase 2: expected 201, got %d: %s", replayW.Code, replayW.Body.String())
	}

	if mgr2.lastEnv == nil {
		t.Fatal("expected env to be set on second instance")
	}
	if mgr2.lastEnv["NEEDED_KEY"] != "gathered-value" {
		t.Errorf("expected NEEDED_KEY='gathered-value', got %q", mgr2.lastEnv["NEEDED_KEY"])
	}
}

func TestCreateAgent_IdempotentByRequestID(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	cfg := DefaultServerConfig()
	cfg.BrokerID = "test-broker-id"
	cfg.BrokerName = "test-host"
	cfg.Debug = true
	cfg.StateDir = t.TempDir()
	cfg.ForceRuntime = "mock"
	projectDir := t.TempDir()
	settingsYAML := `schema_version: "1"
active_profile: local
profiles:
    local:
        runtime: mock
runtimes:
    mock:
        type: mock
`
	if err := os.WriteFile(filepath.Join(projectDir, "settings.yaml"), []byte(settingsYAML), 0644); err != nil {
		t.Fatal(err)
	}

	// Create templates with scion-agent.yaml so harness-config resolution
	// finds a harness_config value instead of falling through to the
	// embedded default ("gemini") which has no on-disk directory.
	tplDir := filepath.Join(projectDir, "templates", "claude")
	if err := os.MkdirAll(tplDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(tplDir, "scion-agent.yaml"), []byte("harness_config: claude\n"), 0644); err != nil {
		t.Fatal(err)
	}

	// Create harness-config directory so FindHarnessConfigDir can resolve it.
	hcDir := filepath.Join(projectDir, "harness-configs", "claude")
	if err := os.MkdirAll(hcDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(hcDir, "config.yaml"), []byte("harness: claude\nimage: test-image:claude\n"), 0644); err != nil {
		t.Fatal(err)
	}

	mgr := &envCapturingManager{}
	srv := New(cfg, mgr, &runtime.MockRuntime{NameFunc: func() string { return "mock" }})

	body := fmt.Sprintf(`{
		"requestId": "req-idempotent-1",
		"name": "test-agent-idem",
		"id": "agent-uuid-idem",
		"projectPath": %q,
		"config": {"template": "claude"}
	}`, projectDir)
	req1 := httptest.NewRequest(http.MethodPost, "/api/v1/agents", strings.NewReader(body))
	req1.Header.Set("Content-Type", "application/json")
	w1 := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w1, req1)
	if w1.Code != http.StatusCreated {
		t.Fatalf("first create: expected 201, got %d: %s", w1.Code, w1.Body.String())
	}
	if mgr.StartCalls() != 1 {
		t.Fatalf("first create: expected startCalls=1, got %d", mgr.StartCalls())
	}

	req2 := httptest.NewRequest(http.MethodPost, "/api/v1/agents", strings.NewReader(body))
	req2.Header.Set("Content-Type", "application/json")
	w2 := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w2, req2)
	if w2.Code != http.StatusCreated {
		t.Fatalf("second create: expected 201 replay, got %d: %s", w2.Code, w2.Body.String())
	}
	if mgr.StartCalls() != 1 {
		t.Fatalf("second create should replay without starting again, startCalls=%d", mgr.StartCalls())
	}
}

// newTestServerWithHarnessConfig creates a test server with a temporary project path
// that has a harness-config directory and optional settings YAML.
func newTestServerWithHarnessConfig(t *testing.T, harnessConfigName, configYAML, settingsYAML string) (*Server, *envCapturingManager, string) {
	t.Helper()

	// Isolate HOME so LoadEffectiveSettings does not merge the developer's
	// personal ~/.scion/settings.yaml (which may declare harness-config
	// auth_selected_type values that would override the test fixture).
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", "")

	projectDir := t.TempDir()

	// Create harness-configs/<name>/config.yaml
	hcDir := filepath.Join(projectDir, "harness-configs", harnessConfigName)
	if err := os.MkdirAll(hcDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(hcDir, "config.yaml"), []byte(configYAML), 0644); err != nil {
		t.Fatal(err)
	}

	// Write settings.yaml if provided
	if settingsYAML != "" {
		if err := os.WriteFile(filepath.Join(projectDir, "settings.yaml"), []byte(settingsYAML), 0644); err != nil {
			t.Fatal(err)
		}
	}

	cfg := DefaultServerConfig()
	cfg.BrokerID = "test-broker-id"
	cfg.BrokerName = "test-host"
	cfg.Debug = true
	cfg.StateDir = t.TempDir()
	cfg.ForceRuntime = "mock"

	mgr := &envCapturingManager{}
	// NameFunc returns "docker" so resolveManagerForOpts matches the settings-resolved runtime.
	rt := &runtime.MockRuntime{NameFunc: func() string { return "docker" }}

	return New(cfg, mgr, rt), mgr, projectDir
}

// unsetHostGCPEnv temporarily removes the broker process's own GCP env vars
// for the duration of the test (restored automatically via t.Setenv's
// cleanup). authCandidateKeyValue (handlers.go) mirrors buildAgentEnv's
// host-env passthrough for a literal empty dir/settings value, so any test
// asserting that an empty GCP-key value does NOT satisfy a requirement needs
// this — otherwise the result depends on whether the host running the test
// happens to have these vars set, which this container's own broker
// environment does.
func unsetHostGCPEnv(t *testing.T) {
	t.Helper()
	for _, k := range []string{
		"GOOGLE_CLOUD_PROJECT", "GCP_PROJECT", "ANTHROPIC_VERTEX_PROJECT_ID",
		"GOOGLE_CLOUD_REGION", "CLOUD_ML_REGION", "GOOGLE_CLOUD_LOCATION",
	} {
		t.Setenv(k, "")
		_ = os.Unsetenv(k)
	}
}

// TestEnvGather_SettingsEmptyEnv tests that env-gather extracts required keys
// from settings-defined empty-value env entries.
func TestEnvGather_SettingsEmptyEnv(t *testing.T) {
	// Settings declares ANTHROPIC_API_KEY as empty (needs gathering)
	srv, _, projectDir := newTestServerWithHarnessConfig(t, "claude",
		"harness: claude\nimage: test-image\nuser: scion\n",
		`
schema_version: "1"
harness_configs:
  claude:
    harness: claude
    env:
      ANTHROPIC_API_KEY: ""
profiles:
  default:
    runtime: mock
`)

	body := `{
		"name": "test-agent-settings-env",
		"id": "agent-uuid-se",
		"gatherEnv": true,
		"projectPath": "` + projectDir + `",
		"config": {"template": "claude", "profile": "default"}
	}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/agents", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	srv.Handler().ServeHTTP(w, req)

	// Should return 202 because ANTHROPIC_API_KEY is needed but not provided
	if w.Code != http.StatusAccepted {
		t.Fatalf("expected 202, got %d: %s", w.Code, w.Body.String())
	}

	var envReqs EnvRequirementsResponse
	if err := json.Unmarshal(w.Body.Bytes(), &envReqs); err != nil {
		t.Fatal("failed to decode response:", err)
	}

	// ANTHROPIC_API_KEY should be in needs
	found := false
	for _, k := range envReqs.Needs {
		if k == "ANTHROPIC_API_KEY" {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("expected ANTHROPIC_API_KEY in needs, got needs=%v required=%v", envReqs.Needs, envReqs.Required)
	}
}

// TestEnvGather_SettingsEmptyEnvVertexAI tests that env-gather extracts
// project-related keys declared as empty in settings.
func TestEnvGather_SettingsEmptyEnvVertexAI(t *testing.T) {
	// GOOGLE_CLOUD_PROJECT is an auth-candidate key: an empty settings value
	// falls through to a host-env passthrough (authCandidateKeyValue), so
	// this test needs the broker process's own GCP env vars out of the way
	// to be deterministic regardless of the host running it.
	unsetHostGCPEnv(t)
	// Settings declares GOOGLE_CLOUD_PROJECT as empty (needs gathering)
	srv, _, projectDir := newTestServerWithHarnessConfig(t, "gemini",
		"harness: gemini\nimage: test-image\nuser: scion\nauth_selected_type: vertex-ai\n",
		`
schema_version: "1"
harness_configs:
  gemini:
    harness: gemini
    env:
      GOOGLE_CLOUD_PROJECT: ""
profiles:
  default:
    runtime: mock
`)

	body := `{
		"name": "test-agent-gemini-vertex",
		"id": "agent-uuid-gv",
		"gatherEnv": true,
		"projectPath": "` + projectDir + `",
		"config": {"template": "gemini", "profile": "default"}
	}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/agents", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	srv.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusAccepted {
		t.Fatalf("expected 202, got %d: %s", w.Code, w.Body.String())
	}

	var envReqs EnvRequirementsResponse
	if err := json.Unmarshal(w.Body.Bytes(), &envReqs); err != nil {
		t.Fatal("failed to decode response:", err)
	}

	found := false
	for _, k := range envReqs.Needs {
		if k == "GOOGLE_CLOUD_PROJECT" {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("expected GOOGLE_CLOUD_PROJECT in needs, got needs=%v required=%v", envReqs.Needs, envReqs.Required)
	}
}

// TestEnvGather_SettingsAuthTypeOverride tests that a settings profile override
// for auth_selected_type takes precedence over the on-disk harness-config value.
func TestEnvGather_SettingsAuthTypeOverride(t *testing.T) {
	// On-disk config says api-key, but settings profile overrides to auth-file
	srv, mgr, projectDir := newTestServerWithHarnessConfig(t, "gemini",
		"harness: gemini\nimage: test-image\nuser: scion\nauth_selected_type: api-key\n",
		`
schema_version: "1"
profiles:
  default:
    runtime: mock
    harness_overrides:
      gemini:
        auth_selected_type: auth-file
`)

	body := `{
		"name": "test-agent-override",
		"id": "agent-uuid-ov",
		"gatherEnv": true,
		"projectPath": "` + projectDir + `",
		"config": {"template": "gemini", "profile": "default"}
	}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/agents", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	srv.Handler().ServeHTTP(w, req)

	// No settings-defined empty env keys, so the agent should start immediately
	if w.Code != http.StatusCreated {
		t.Fatalf("expected 201 (no required env keys), got %d: %s", w.Code, w.Body.String())
	}

	if mgr.lastEnv == nil {
		t.Fatal("expected env to be set")
	}
}

// TestEnvGather_FileSecretSatisfiesAuth tests that when auth type is unset (auto-detect)
// and a file-type secret like OAUTH_CREDS is available, the system detects that auth-file
// can be used and does not require GEMINI_API_KEY.
func TestEnvGather_FileSecretSatisfiesAuth(t *testing.T) {
	srv, mgr, projectDir := newTestServerWithHarnessConfig(t, "gemini",
		"harness: gemini\nimage: test-image\nuser: scion\n",
		`
schema_version: "1"
profiles:
  default:
    runtime: mock
`)

	body := `{
		"name": "test-agent-oauth",
		"id": "agent-uuid-oauth",
		"gatherEnv": true,
		"projectPath": "` + projectDir + `",
		"resolvedSecrets": [
			{"name": "GEMINI_OAUTH_CREDS", "type": "file", "target": "/home/gemini/.gemini/oauth_creds.json", "value": "{}", "source": "user"}
		],
		"config": {"template": "gemini", "profile": "default"}
	}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/agents", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	srv.Handler().ServeHTTP(w, req)

	// GEMINI_OAUTH_CREDS file secret should satisfy auth via auth-file detection,
	// so GEMINI_API_KEY should NOT be required.
	if w.Code != http.StatusCreated {
		t.Fatalf("expected 201 (file secret satisfies auth), got %d: %s", w.Code, w.Body.String())
	}

	if mgr.lastEnv == nil {
		t.Fatal("expected env to be set")
	}
}

// TestEnvGather_NoGatherFlag tests that env-gather is skipped when GatherEnv is false.
func TestEnvGather_NoGatherFlag(t *testing.T) {
	settings := `
schema_version: "1"
harness_configs:
  claude:
    harness: claude
    env:
      MISSING_KEY: ""
profiles:
  default:
    runtime: mock
`
	srv, mgr, projectDir := newTestServerWithProjectPath(t, settings)

	body := `{
		"name": "test-agent-no-gather",
		"id": "agent-uuid-no-gather",
		"gatherEnv": false,
		"projectPath": "` + projectDir + `",
		"config": {"template": "claude", "profile": "default"}
	}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/agents", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	srv.Handler().ServeHTTP(w, req)

	// Should create the agent normally (201) even though env is missing
	if w.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", w.Code, w.Body.String())
	}

	// Agent was started (env gather skipped)
	if mgr.lastEnv == nil {
		t.Fatal("expected env to be set")
	}
}

// TestEnvGather_NoAuthSkipsEnvGather tests that env-gather is skipped when
// NoAuth is true, even when GatherEnv is also true. When NoAuth is set, the hub
// intentionally strips credentials, so the broker must not report them as missing.
func TestEnvGather_NoAuthSkipsEnvGather(t *testing.T) {
	settings := `
schema_version: "1"
harness_configs:
  claude:
    harness: claude
    env:
      ANTHROPIC_API_KEY: ""
profiles:
  default:
    runtime: mock
`
	srv, mgr, projectDir := newTestServerWithProjectPath(t, settings)

	body := `{
		"name": "test-agent-noauth",
		"id": "agent-uuid-noauth",
		"gatherEnv": true,
		"noAuth": true,
		"projectPath": "` + projectDir + `",
		"config": {"template": "claude", "profile": "default"}
	}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/agents", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	srv.Handler().ServeHTTP(w, req)

	// Should create the agent normally (201), NOT return 202 with env requirements.
	if w.Code == http.StatusAccepted {
		t.Fatalf("expected env-gather to be skipped for noAuth, but got 202: %s", w.Body.String())
	}
	if w.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", w.Code, w.Body.String())
	}

	// Agent was started (env gather skipped)
	if mgr.lastEnv == nil {
		t.Fatal("expected env to be set")
	}
}

// TestEnvGather_SecretAutoUpgrade tests that when all required env keys are
// satisfied by resolved secrets, the env-gather check passes through without
// returning 202. The agent proceeds to creation (which may fail for other
// reasons in the test environment, but the key point is no 202 is returned).
func TestEnvGather_SecretAutoUpgrade(t *testing.T) {
	settings := `
schema_version: "1"
harness_configs:
  claude:
    harness: claude
    env:
      API_KEY: ""
profiles:
  default:
    runtime: mock
`
	srv, _, projectDir := newTestServerWithProjectPath(t, settings)

	body := `{
		"name": "test-agent-secret-upgrade",
		"id": "agent-uuid-secret",
		"gatherEnv": true,
		"projectPath": "` + projectDir + `",
		"resolvedSecrets": [
			{"name": "API_KEY", "type": "environment", "target": "API_KEY", "value": "secret-api-key", "source": "user"},
			{"name": "ANTHROPIC_API_KEY", "type": "environment", "target": "ANTHROPIC_API_KEY", "value": "secret-ant-key", "source": "user"}
		],
		"config": {"template": "claude", "profile": "default"}
	}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/agents", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	srv.Handler().ServeHTTP(w, req)

	// Should NOT return 202 — all env keys are satisfied (API_KEY by secret,
	// ANTHROPIC_API_KEY by broker env). The request proceeds past env-gather.
	if w.Code == http.StatusAccepted {
		t.Fatalf("expected env-gather to pass (not 202), but got 202: %s", w.Body.String())
	}
}

// TestEnvGather_SecretPartialSatisfaction tests that when one required key is
// satisfied by a resolved secret but another is not, the broker returns 202
// with only the unsatisfied key in needs.
func TestEnvGather_SecretPartialSatisfaction(t *testing.T) {
	settings := `
schema_version: "1"
harness_configs:
  claude:
    harness: claude
    env:
      API_KEY: ""
      OTHER_TOKEN: ""
profiles:
  default:
    runtime: mock
`
	srv, _, projectDir := newTestServerWithProjectPath(t, settings)

	body := `{
		"name": "test-agent-partial-secret",
		"id": "agent-uuid-partial",
		"gatherEnv": true,
		"projectPath": "` + projectDir + `",
		"resolvedSecrets": [
			{"name": "API_KEY", "type": "environment", "target": "API_KEY", "value": "secret-api-key", "source": "user"}
		],
		"config": {"template": "claude", "profile": "default"}
	}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/agents", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	srv.Handler().ServeHTTP(w, req)

	// Should return 202 because OTHER_TOKEN is still unsatisfied
	if w.Code != http.StatusAccepted {
		t.Fatalf("expected 202, got %d: %s", w.Code, w.Body.String())
	}

	var envReqs EnvRequirementsResponse
	if err := json.Unmarshal(w.Body.Bytes(), &envReqs); err != nil {
		t.Fatal("failed to decode response:", err)
	}

	// API_KEY should be in hubHas (satisfied by secret)
	found := false
	for _, k := range envReqs.HubHas {
		if k == "API_KEY" {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("expected API_KEY in hubHas, got %v", envReqs.HubHas)
	}

	// OTHER_TOKEN should be in needs
	found = false
	for _, k := range envReqs.Needs {
		if k == "OTHER_TOKEN" {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("expected OTHER_TOKEN in needs, got %v", envReqs.Needs)
	}

	// API_KEY should NOT be in needs
	for _, k := range envReqs.Needs {
		if k == "API_KEY" {
			t.Error("API_KEY should not be in needs (satisfied by secret)")
		}
	}
}

// TestEnvGather_SettingsHarnessSecrets tests that secrets declared in
// harness_configs[*].secrets are extracted as required keys.
func TestEnvGather_SettingsHarnessSecrets(t *testing.T) {
	settings := `
schema_version: "1"
harness_configs:
  claude:
    harness: claude
    secrets:
      - key: THIRD_PARTY_TOKEN
        description: "Token for third-party API integration"
profiles:
  default:
    runtime: mock
`
	srv, _, projectDir := newTestServerWithProjectPath(t, settings)

	body := `{
		"name": "test-agent-harness-secrets",
		"id": "agent-uuid-hs",
		"gatherEnv": true,
		"projectPath": "` + projectDir + `",
		"resolvedEnv": {"ANTHROPIC_API_KEY": "sk-ant-key"},
		"config": {"template": "claude", "profile": "default"}
	}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/agents", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	srv.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusAccepted {
		t.Fatalf("expected 202, got %d: %s", w.Code, w.Body.String())
	}

	var envReqs EnvRequirementsResponse
	if err := json.Unmarshal(w.Body.Bytes(), &envReqs); err != nil {
		t.Fatal("failed to decode response:", err)
	}

	// THIRD_PARTY_TOKEN should be in needs
	found := false
	for _, k := range envReqs.Needs {
		if k == "THIRD_PARTY_TOKEN" {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("expected THIRD_PARTY_TOKEN in needs, got %v", envReqs.Needs)
	}

	// SecretInfo should be populated with description and source
	if envReqs.SecretInfo == nil {
		t.Fatal("expected SecretInfo to be set")
	}
	info, ok := envReqs.SecretInfo["THIRD_PARTY_TOKEN"]
	if !ok {
		t.Fatal("expected THIRD_PARTY_TOKEN in SecretInfo")
	}
	if info.Description != "Token for third-party API integration" {
		t.Errorf("expected description='Token for third-party API integration', got %q", info.Description)
	}
	if info.Source != "settings" {
		t.Errorf("expected source='settings', got %q", info.Source)
	}
}

// TestEnvGather_SettingsProfileSecrets tests that secrets declared in
// profiles[*].secrets are extracted as required keys.
func TestEnvGather_SettingsProfileSecrets(t *testing.T) {
	settings := `
schema_version: "1"
harness_configs:
  claude:
    harness: claude
profiles:
  default:
    runtime: mock
    secrets:
      - key: PROFILE_SECRET
        description: "Secret required by this profile"
`
	srv, _, projectDir := newTestServerWithProjectPath(t, settings)

	// Satisfy harness key via broker env
	t.Setenv("ANTHROPIC_API_KEY", "broker-ant-key")

	body := `{
		"name": "test-agent-profile-secrets",
		"id": "agent-uuid-ps",
		"gatherEnv": true,
		"projectPath": "` + projectDir + `",
		"config": {"template": "claude", "profile": "default"}
	}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/agents", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	srv.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusAccepted {
		t.Fatalf("expected 202, got %d: %s", w.Code, w.Body.String())
	}

	var envReqs EnvRequirementsResponse
	if err := json.Unmarshal(w.Body.Bytes(), &envReqs); err != nil {
		t.Fatal("failed to decode response:", err)
	}

	// PROFILE_SECRET should be in needs
	found := false
	for _, k := range envReqs.Needs {
		if k == "PROFILE_SECRET" {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("expected PROFILE_SECRET in needs, got %v", envReqs.Needs)
	}
}

// TestEnvGather_RequestRequiredSecrets tests that RequiredSecrets in the
// create request (from Hub template) are extracted as required keys.
func TestEnvGather_RequestRequiredSecrets(t *testing.T) {
	settings := `
schema_version: "1"
harness_configs:
  claude:
    harness: claude
profiles:
  default:
    runtime: mock
`
	srv, _, projectDir := newTestServerWithProjectPath(t, settings)

	// Satisfy harness key via broker env
	t.Setenv("ANTHROPIC_API_KEY", "broker-ant-key")

	body := `{
		"name": "test-agent-req-secrets",
		"id": "agent-uuid-rs",
		"gatherEnv": true,
		"projectPath": "` + projectDir + `",
		"requiredSecrets": [
			{"key": "HUB_TEMPLATE_KEY", "description": "Key from Hub template"}
		],
		"config": {"template": "claude", "profile": "default"}
	}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/agents", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	srv.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusAccepted {
		t.Fatalf("expected 202, got %d: %s", w.Code, w.Body.String())
	}

	var envReqs EnvRequirementsResponse
	if err := json.Unmarshal(w.Body.Bytes(), &envReqs); err != nil {
		t.Fatal("failed to decode response:", err)
	}

	// HUB_TEMPLATE_KEY should be in needs
	found := false
	for _, k := range envReqs.Needs {
		if k == "HUB_TEMPLATE_KEY" {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("expected HUB_TEMPLATE_KEY in needs, got %v", envReqs.Needs)
	}

	// SecretInfo should include the key with template source
	if envReqs.SecretInfo == nil {
		t.Fatal("expected SecretInfo to be set")
	}
	info, ok := envReqs.SecretInfo["HUB_TEMPLATE_KEY"]
	if !ok {
		t.Fatal("expected HUB_TEMPLATE_KEY in SecretInfo")
	}
	if info.Description != "Key from Hub template" {
		t.Errorf("expected description='Key from Hub template', got %q", info.Description)
	}
	if info.Source != "template" {
		t.Errorf("expected source='template', got %q", info.Source)
	}
}

// TestEnvGather_SecretInfoOnlyNeeded tests that SecretInfo only includes
// keys that are in needs (not satisfied ones).
func TestEnvGather_SecretInfoOnlyNeeded(t *testing.T) {
	settings := `
schema_version: "1"
harness_configs:
  claude:
    harness: claude
    secrets:
      - key: SATISFIED_KEY
        description: "This key is satisfied"
      - key: MISSING_KEY
        description: "This key is missing"
profiles:
  default:
    runtime: mock
`
	srv, _, projectDir := newTestServerWithProjectPath(t, settings)

	// Satisfy harness key and SATISFIED_KEY via resolved secrets
	body := `{
		"name": "test-agent-si-needed",
		"id": "agent-uuid-sin",
		"gatherEnv": true,
		"projectPath": "` + projectDir + `",
		"resolvedEnv": {"ANTHROPIC_API_KEY": "sk-ant-key"},
		"resolvedSecrets": [
			{"name": "SATISFIED_KEY", "type": "environment", "target": "SATISFIED_KEY", "value": "satisfied-val", "source": "user"}
		],
		"config": {"template": "claude", "profile": "default"}
	}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/agents", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	srv.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusAccepted {
		t.Fatalf("expected 202, got %d: %s", w.Code, w.Body.String())
	}

	var envReqs EnvRequirementsResponse
	if err := json.Unmarshal(w.Body.Bytes(), &envReqs); err != nil {
		t.Fatal("failed to decode response:", err)
	}

	// SecretInfo should include MISSING_KEY but NOT SATISFIED_KEY
	if envReqs.SecretInfo == nil {
		t.Fatal("expected SecretInfo to be set")
	}
	if _, ok := envReqs.SecretInfo["SATISFIED_KEY"]; ok {
		t.Error("SecretInfo should NOT include satisfied keys")
	}
	if _, ok := envReqs.SecretInfo["MISSING_KEY"]; !ok {
		t.Error("SecretInfo should include MISSING_KEY")
	}
}

// TestEnvGather_SecretInfoIncludesType tests that the Type field from
// RequiredSecret declarations is propagated into SecretKeyInfo.
func TestEnvGather_SecretInfoIncludesType(t *testing.T) {
	settings := `
schema_version: "1"
harness_configs:
  claude:
    harness: claude
    secrets:
      - key: ENV_SECRET
        description: "An environment secret"
        type: environment
      - key: FILE_CERT
        description: "TLS certificate"
        type: file
profiles:
  default:
    runtime: mock
    secrets:
      - key: PROFILE_TOKEN
        description: "Profile token"
        type: variable
`
	srv, _, projectDir := newTestServerWithProjectPath(t, settings)

	// Satisfy harness key via broker env
	t.Setenv("ANTHROPIC_API_KEY", "broker-ant-key")

	body := `{
		"name": "test-agent-type-prop",
		"id": "agent-uuid-tp",
		"gatherEnv": true,
		"projectPath": "` + projectDir + `",
		"config": {"template": "claude", "profile": "default"}
	}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/agents", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	srv.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusAccepted {
		t.Fatalf("expected 202, got %d: %s", w.Code, w.Body.String())
	}

	var envReqs EnvRequirementsResponse
	if err := json.Unmarshal(w.Body.Bytes(), &envReqs); err != nil {
		t.Fatal("failed to decode response:", err)
	}

	if envReqs.SecretInfo == nil {
		t.Fatal("expected SecretInfo to be set")
	}

	// Check ENV_SECRET has type "environment"
	if info, ok := envReqs.SecretInfo["ENV_SECRET"]; !ok {
		t.Error("expected ENV_SECRET in SecretInfo")
	} else if info.Type != "environment" {
		t.Errorf("expected ENV_SECRET type='environment', got %q", info.Type)
	}

	// Check FILE_CERT has type "file"
	if info, ok := envReqs.SecretInfo["FILE_CERT"]; !ok {
		t.Error("expected FILE_CERT in SecretInfo")
	} else if info.Type != "file" {
		t.Errorf("expected FILE_CERT type='file', got %q", info.Type)
	}

	// Check PROFILE_TOKEN has type "variable"
	if info, ok := envReqs.SecretInfo["PROFILE_TOKEN"]; !ok {
		t.Error("expected PROFILE_TOKEN in SecretInfo")
	} else if info.Type != "variable" {
		t.Errorf("expected PROFILE_TOKEN type='variable', got %q", info.Type)
	}

	// ANTHROPIC_API_KEY is a harness key — should have no type (empty string)
	if info, ok := envReqs.SecretInfo["ANTHROPIC_API_KEY"]; ok {
		// Harness keys are auto-added to SecretInfo but shouldn't appear in
		// the response if they're already satisfied (in hubHas/brokerHas).
		// If it does appear, type should be empty.
		if info.Type != "" {
			t.Errorf("expected ANTHROPIC_API_KEY type='', got %q", info.Type)
		}
	}
}

// TestEnvGather_SecretInfoIncludesType_Template tests that Type is populated
// from template RequiredSecrets in the create request.
func TestEnvGather_SecretInfoIncludesType_Template(t *testing.T) {
	settings := `
schema_version: "1"
harness_configs:
  claude:
    harness: claude
profiles:
  default:
    runtime: mock
`
	srv, _, projectDir := newTestServerWithProjectPath(t, settings)

	// Satisfy harness key via broker env
	t.Setenv("ANTHROPIC_API_KEY", "broker-ant-key")

	body := `{
		"name": "test-agent-type-tmpl",
		"id": "agent-uuid-tt",
		"gatherEnv": true,
		"projectPath": "` + projectDir + `",
		"requiredSecrets": [
			{"key": "TMPL_FILE_SECRET", "description": "Template file secret", "type": "file"},
			{"key": "TMPL_ENV_SECRET", "description": "Template env secret", "type": "environment"}
		],
		"config": {"template": "claude", "profile": "default"}
	}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/agents", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	srv.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusAccepted {
		t.Fatalf("expected 202, got %d: %s", w.Code, w.Body.String())
	}

	var envReqs EnvRequirementsResponse
	if err := json.Unmarshal(w.Body.Bytes(), &envReqs); err != nil {
		t.Fatal("failed to decode response:", err)
	}

	if envReqs.SecretInfo == nil {
		t.Fatal("expected SecretInfo to be set")
	}

	if info, ok := envReqs.SecretInfo["TMPL_FILE_SECRET"]; !ok {
		t.Error("expected TMPL_FILE_SECRET in SecretInfo")
	} else {
		if info.Type != "file" {
			t.Errorf("expected TMPL_FILE_SECRET type='file', got %q", info.Type)
		}
		if info.Source != "template" {
			t.Errorf("expected TMPL_FILE_SECRET source='template', got %q", info.Source)
		}
	}

	if info, ok := envReqs.SecretInfo["TMPL_ENV_SECRET"]; !ok {
		t.Error("expected TMPL_ENV_SECRET in SecretInfo")
	} else if info.Type != "environment" {
		t.Errorf("expected TMPL_ENV_SECRET type='environment', got %q", info.Type)
	}
}

// TestEnvGather_SettingsSecretsMerge tests that when the same key is declared
// in both harness config and profile, the profile description wins (most specific).
func TestEnvGather_SettingsSecretsMerge(t *testing.T) {
	settings := `
schema_version: "1"
harness_configs:
  claude:
    harness: claude
    secrets:
      - key: SHARED_KEY
        description: "From harness config"
profiles:
  default:
    runtime: mock
    secrets:
      - key: SHARED_KEY
        description: "From profile"
`
	srv, _, projectDir := newTestServerWithProjectPath(t, settings)

	// Satisfy harness key via broker env
	t.Setenv("ANTHROPIC_API_KEY", "broker-ant-key")

	body := `{
		"name": "test-agent-merge",
		"id": "agent-uuid-merge",
		"gatherEnv": true,
		"projectPath": "` + projectDir + `",
		"config": {"template": "claude", "profile": "default"}
	}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/agents", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	srv.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusAccepted {
		t.Fatalf("expected 202, got %d: %s", w.Code, w.Body.String())
	}

	var envReqs EnvRequirementsResponse
	if err := json.Unmarshal(w.Body.Bytes(), &envReqs); err != nil {
		t.Fatal("failed to decode response:", err)
	}

	if envReqs.SecretInfo == nil {
		t.Fatal("expected SecretInfo to be set")
	}
	info, ok := envReqs.SecretInfo["SHARED_KEY"]
	if !ok {
		t.Fatal("expected SHARED_KEY in SecretInfo")
	}
	// Profile is processed after harness config, so profile description wins
	if info.Description != "From profile" {
		t.Errorf("expected description='From profile' (profile wins), got %q", info.Description)
	}
}

// TestEnvGather_HarnessFromConfig tests that harness-config env declarations
// drive env-gather even when using config.harnessConfig without an on-disk dir.
func TestEnvGather_HarnessFromConfig(t *testing.T) {
	// Settings declares GEMINI_API_KEY as empty via harness_configs
	settings := `
schema_version: "1"
harness_configs:
  gemini:
    harness: gemini
    env:
      GEMINI_API_KEY: ""
profiles:
  default:
    runtime: mock
`
	srv, _, projectDir := newTestServerWithProjectPath(t, settings)

	body := `{
		"name": "test-agent-harness-config",
		"id": "agent-uuid-hc",
		"gatherEnv": true,
		"projectPath": "` + projectDir + `",
		"config": {"template": "default", "harnessConfig": "gemini", "profile": "default"}
	}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/agents", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	srv.Handler().ServeHTTP(w, req)

	// GEMINI_API_KEY is empty in settings, so we should get 202 with env requirements.
	if w.Code != http.StatusAccepted {
		t.Fatalf("expected 202, got %d: %s", w.Code, w.Body.String())
	}

	var envReqs EnvRequirementsResponse
	if err := json.Unmarshal(w.Body.Bytes(), &envReqs); err != nil {
		t.Fatal("failed to decode response:", err)
	}

	// GEMINI_API_KEY should be in the needs list (declared as empty in settings)
	found := false
	for _, k := range envReqs.Needs {
		if k == "GEMINI_API_KEY" {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("expected GEMINI_API_KEY in needs, got needs=%v required=%v", envReqs.Needs, envReqs.Required)
	}
}

// TestEnvGather_VertexAI_RequiresADCFile tests that vertex-ai auth without
// an ADC file secret returns 202 with gcloud-adc in needs
// and SecretInfo showing type=file.
func TestEnvGather_VertexAI_RequiresADCFile(t *testing.T) {
	srv, _, projectDir := newTestServerWithHarnessConfig(t, "claude",
		"harness: claude\nimage: test-image\nuser: scion\nauth_selected_type: vertex-ai\n"+claudeAuthBlock,
		`
schema_version: "1"
harness_configs:
  claude:
    harness: claude
profiles:
  default:
    runtime: mock
`)

	// Provide project and region env vars so those are satisfied,
	// but do NOT provide ADC file secret
	body := `{
		"name": "test-agent-vertex-adc",
		"id": "agent-uuid-vadc",
		"gatherEnv": true,
		"projectPath": "` + projectDir + `",
		"resolvedEnv": {
			"GOOGLE_CLOUD_PROJECT": "my-project",
			"GOOGLE_CLOUD_REGION": "us-central1"
		},
		"config": {"template": "claude", "profile": "default"}
	}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/agents", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	srv.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusAccepted {
		t.Fatalf("expected 202, got %d: %s", w.Code, w.Body.String())
	}

	var envReqs EnvRequirementsResponse
	if err := json.Unmarshal(w.Body.Bytes(), &envReqs); err != nil {
		t.Fatal("failed to decode response:", err)
	}

	// gcloud-adc should be in needs
	found := false
	for _, k := range envReqs.Needs {
		if k == "gcloud-adc" {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("expected gcloud-adc in needs, got %v", envReqs.Needs)
	}

	// SecretInfo should show type=file and source=auth
	if envReqs.SecretInfo == nil {
		t.Fatal("expected SecretInfo to be set")
	}
	info, ok := envReqs.SecretInfo["gcloud-adc"]
	if !ok {
		t.Fatal("expected gcloud-adc in SecretInfo")
	}
	if info.Type != "file" {
		t.Errorf("expected type='file', got %q", info.Type)
	}
	if info.Source != "auth" {
		t.Errorf("expected source='auth', got %q", info.Source)
	}
	if info.Description == "" {
		t.Error("expected non-empty description for ADC secret")
	}
}

// TestEnvGather_VertexAI_ADCSatisfied tests that vertex-ai auth with a
// file-type resolved secret for ADC passes through without returning 202
// for gcloud-adc.
func TestEnvGather_VertexAI_ADCSatisfied(t *testing.T) {
	srv, _, projectDir := newTestServerWithHarnessConfig(t, "claude",
		"harness: claude\nimage: test-image\nuser: scion\nauth_selected_type: vertex-ai\n",
		`
schema_version: "1"
harness_configs:
  claude:
    harness: claude
profiles:
  default:
    runtime: mock
`)

	// Provide project, region, AND ADC file secret
	body := `{
		"name": "test-agent-vertex-adc-sat",
		"id": "agent-uuid-vadcs",
		"gatherEnv": true,
		"projectPath": "` + projectDir + `",
		"resolvedEnv": {
			"GOOGLE_CLOUD_PROJECT": "my-project",
			"GOOGLE_CLOUD_REGION": "us-central1"
		},
		"resolvedSecrets": [
			{"name": "gcloud-adc", "type": "file", "target": "/home/scion/.config/gcloud/application_default_credentials.json", "value": "{\"type\":\"authorized_user\"}", "source": "user"}
		],
		"config": {"template": "claude", "profile": "default"}
	}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/agents", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	srv.Handler().ServeHTTP(w, req)

	// Should NOT return 202 — all requirements are satisfied
	if w.Code == http.StatusAccepted {
		var envReqs EnvRequirementsResponse
		_ = json.Unmarshal(w.Body.Bytes(), &envReqs)
		// Check that gcloud-adc is not in needs
		for _, k := range envReqs.Needs {
			if k == "gcloud-adc" {
				t.Fatalf("gcloud-adc should not be in needs when ADC file secret is provided, got needs=%v", envReqs.Needs)
			}
		}
	}
}

// TestEnvGather_AutoDetectVertexAI_FromGACEnvVar tests that when no auth type
// is explicitly selected, providing GOOGLE_APPLICATION_CREDENTIALS auto-detects
// vertex-ai auth and requires project/region instead of an API key.
func TestEnvGather_AutoDetectVertexAI_FromGACEnvVar(t *testing.T) {
	// No auth_selected_type set — auto-detect should kick in
	srv, _, projectDir := newTestServerWithHarnessConfig(t, "claude",
		"harness: claude\nimage: test-image\nuser: scion\n"+claudeAuthBlock,
		`
schema_version: "1"
harness_configs:
  claude:
    harness: claude
profiles:
  default:
    runtime: mock
`)

	// Provide GOOGLE_APPLICATION_CREDENTIALS but no API key, project, or region
	body := `{
		"name": "test-agent-autodetect-gac",
		"id": "agent-uuid-adgac",
		"gatherEnv": true,
		"projectPath": "` + projectDir + `",
		"resolvedEnv": {
			"GOOGLE_APPLICATION_CREDENTIALS": "/path/to/service-account.json"
		},
		"config": {"template": "claude", "profile": "default"}
	}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/agents", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	srv.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusAccepted {
		t.Fatalf("expected 202 (auto-detect vertex-ai needs project/region), got %d: %s", w.Code, w.Body.String())
	}

	var envReqs EnvRequirementsResponse
	if err := json.Unmarshal(w.Body.Bytes(), &envReqs); err != nil {
		t.Fatal("failed to decode response:", err)
	}

	// Should require GOOGLE_CLOUD_PROJECT (vertex-ai env keys), NOT ANTHROPIC_API_KEY
	needsMap := make(map[string]struct{})
	for _, k := range envReqs.Needs {
		needsMap[k] = struct{}{}
	}
	if _, ok := needsMap["ANTHROPIC_API_KEY"]; ok {
		t.Errorf("ANTHROPIC_API_KEY should not be required when GOOGLE_APPLICATION_CREDENTIALS triggers vertex-ai auto-detect, got needs=%v", envReqs.Needs)
	}
	if _, ok := needsMap["GOOGLE_CLOUD_PROJECT"]; !ok {
		t.Errorf("expected GOOGLE_CLOUD_PROJECT in needs for auto-detected vertex-ai, got needs=%v", envReqs.Needs)
	}

	// gcloud-adc should NOT be required (GAC env var is the alternative)
	if _, ok := needsMap["gcloud-adc"]; ok {
		t.Errorf("gcloud-adc should not be required when GOOGLE_APPLICATION_CREDENTIALS is provided, got needs=%v", envReqs.Needs)
	}
}

// TestEnvGather_VertexAI_ADCSatisfiedByEnvVar tests that vertex-ai auth is
// satisfied when GOOGLE_APPLICATION_CREDENTIALS env var is provided instead
// of a gcloud-adc file secret.
func TestEnvGather_VertexAI_ADCSatisfiedByEnvVar(t *testing.T) {
	srv, _, projectDir := newTestServerWithHarnessConfig(t, "claude",
		"harness: claude\nimage: test-image\nuser: scion\nauth_selected_type: vertex-ai\n",
		`
schema_version: "1"
harness_configs:
  claude:
    harness: claude
profiles:
  default:
    runtime: mock
`)

	// Provide project, region, AND GOOGLE_APPLICATION_CREDENTIALS env var
	// (no gcloud-adc file secret)
	body := `{
		"name": "test-agent-vertex-gac",
		"id": "agent-uuid-vgac",
		"gatherEnv": true,
		"projectPath": "` + projectDir + `",
		"resolvedEnv": {
			"GOOGLE_CLOUD_PROJECT": "my-project",
			"GOOGLE_CLOUD_REGION": "us-central1",
			"GOOGLE_APPLICATION_CREDENTIALS": "/path/to/service-account.json"
		},
		"config": {"template": "claude", "profile": "default"}
	}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/agents", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	srv.Handler().ServeHTTP(w, req)

	// Should NOT return 202 — GOOGLE_APPLICATION_CREDENTIALS satisfies the ADC requirement
	if w.Code == http.StatusAccepted {
		var envReqs EnvRequirementsResponse
		_ = json.Unmarshal(w.Body.Bytes(), &envReqs)
		for _, k := range envReqs.Needs {
			if k == "gcloud-adc" {
				t.Fatalf("gcloud-adc should not be in needs when GOOGLE_APPLICATION_CREDENTIALS is provided, got needs=%v", envReqs.Needs)
			}
		}
	}
}

// TestEnvGather_VertexAI_ADCSatisfiedByProcessEnv tests that vertex-ai gcloud-adc
// requirement is satisfied when GOOGLE_APPLICATION_CREDENTIALS is set in the
// broker's process environment (not in resolvedEnv). This covers workstation mode
// where the ADC path is auto-detected at startup and exported as a process env var.
func TestEnvGather_VertexAI_ADCSatisfiedByProcessEnv(t *testing.T) {
	srv, _, projectDir := newTestServerWithHarnessConfig(t, "claude",
		"harness: claude\nimage: test-image\nuser: scion\nauth_selected_type: vertex-ai\n",
		`
schema_version: "1"
harness_configs:
  claude:
    harness: claude
profiles:
  default:
    runtime: mock
`)

	// Set GOOGLE_APPLICATION_CREDENTIALS in process env (not in resolvedEnv)
	t.Setenv("GOOGLE_APPLICATION_CREDENTIALS", "/tmp/fake-adc.json")

	// Provide project and region in resolvedEnv, but NO gcloud-adc file secret
	// and NO GOOGLE_APPLICATION_CREDENTIALS in resolvedEnv
	body := `{
		"name": "test-agent-vertex-proc-env",
		"id": "agent-uuid-vpe",
		"gatherEnv": true,
		"projectPath": "` + projectDir + `",
		"resolvedEnv": {
			"GOOGLE_CLOUD_PROJECT": "my-project",
			"GOOGLE_CLOUD_REGION": "us-central1"
		},
		"config": {"template": "claude", "profile": "default"}
	}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/agents", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	srv.Handler().ServeHTTP(w, req)

	// Should NOT return 202 with gcloud-adc in needs — process env satisfies it
	if w.Code == http.StatusAccepted {
		var envReqs EnvRequirementsResponse
		_ = json.Unmarshal(w.Body.Bytes(), &envReqs)
		for _, k := range envReqs.Needs {
			if k == "gcloud-adc" {
				t.Fatalf("gcloud-adc should not be in needs when GOOGLE_APPLICATION_CREDENTIALS is set in process env, got needs=%v", envReqs.Needs)
			}
		}
	}
}

// TestEnvGather_AutoDetectVertexAI_FromGCPProject tests that when no auth type
// is explicitly selected, providing GOOGLE_CLOUD_PROJECT (e.g. from hub-scoped
// env vars) auto-detects vertex-ai auth and requires region instead of an API key.
// Regression test: previously, only GOOGLE_APPLICATION_CREDENTIALS triggered
// vertex-ai detection, so hub-scoped GOOGLE_CLOUD_PROJECT was resolved but the
// auth type defaulted to api-key, requiring ANTHROPIC_API_KEY and blocking
// non-admin users who only have GCP credentials.
func TestEnvGather_AutoDetectVertexAI_FromGCPProject(t *testing.T) {
	srv, _, projectDir := newTestServerWithHarnessConfig(t, "claude",
		"harness: claude\nimage: test-image\nuser: scion\n"+claudeAuthBlock,
		`
schema_version: "1"
harness_configs:
  claude:
    harness: claude
profiles:
  default:
    runtime: mock
`)

	body := `{
		"name": "test-agent-autodetect-gcp",
		"id": "agent-uuid-adgcp",
		"gatherEnv": true,
		"projectPath": "` + projectDir + `",
		"resolvedEnv": {
			"GOOGLE_CLOUD_PROJECT": "my-hub-project"
		},
		"config": {"template": "claude", "profile": "default"}
	}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/agents", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	srv.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusAccepted {
		t.Fatalf("expected 202 (auto-detect vertex-ai needs region), got %d: %s", w.Code, w.Body.String())
	}

	var envReqs EnvRequirementsResponse
	if err := json.Unmarshal(w.Body.Bytes(), &envReqs); err != nil {
		t.Fatal("failed to decode response:", err)
	}

	needsMap := make(map[string]struct{})
	for _, k := range envReqs.Needs {
		needsMap[k] = struct{}{}
	}

	if _, ok := needsMap["ANTHROPIC_API_KEY"]; ok {
		t.Errorf("ANTHROPIC_API_KEY should not be required when GOOGLE_CLOUD_PROJECT triggers vertex-ai auto-detect, got needs=%v", envReqs.Needs)
	}
	if _, ok := needsMap["GOOGLE_CLOUD_PROJECT"]; ok {
		t.Errorf("GOOGLE_CLOUD_PROJECT should be satisfied (in resolvedEnv), not in needs, got needs=%v", envReqs.Needs)
	}
	if _, ok := needsMap["GOOGLE_CLOUD_REGION"]; !ok {
		t.Errorf("expected GOOGLE_CLOUD_REGION in needs for auto-detected vertex-ai (only project provided), got needs=%v", envReqs.Needs)
	}
}

// TestEnvGather_AutoDetect_APIKeyWinsOverGCPProject tests that when both an
// API key and GCP credentials are available (e.g. GEMINI_API_KEY secret + user-
// scoped GOOGLE_CLOUD_PROJECT), auto-detection prefers api-key over vertex-ai.
// Regression test: previously, DetectAuthTypeFromEnvVars would detect vertex-ai
// from GOOGLE_CLOUD_PROJECT without checking whether an API key was also present,
// causing env-gather to require gcloud-adc even though api-key auth was viable.
func TestEnvGather_AutoDetect_APIKeyWinsOverGCPProject(t *testing.T) {
	srv, _, projectDir := newTestServerWithHarnessConfig(t, "gemini",
		"harness: gemini\nimage: test-image\nuser: scion\n",
		`
schema_version: "1"
harness_configs:
  gemini:
    harness: gemini
profiles:
  default:
    runtime: mock
`)

	body := `{
		"name": "test-agent-apikey-gcp",
		"id": "agent-uuid-apikey-gcp",
		"gatherEnv": true,
		"projectPath": "` + projectDir + `",
		"resolvedEnv": {
			"GOOGLE_CLOUD_PROJECT": "my-project",
			"GOOGLE_CLOUD_REGION": "us-central1",
			"GOOGLE_CLOUD_LOCATION": "us-central1"
		},
		"resolvedSecrets": [
			{"name": "GEMINI_API_KEY", "type": "environment", "target": "GEMINI_API_KEY", "value": "sk-test", "source": "project"},
			{"name": "GOOGLE_APPLICATION_CREDENTIALS", "type": "file", "target": "/tmp/adc.json", "value": "{}", "source": "user"}
		],
		"config": {"template": "gemini", "profile": "default"}
	}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/agents", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	srv.Handler().ServeHTTP(w, req)

	// With GEMINI_API_KEY satisfied, auto-detect should pick api-key (not vertex-ai).
	// All auth requirements should be met → 201, not 202 requiring gcloud-adc.
	if w.Code != http.StatusCreated {
		t.Fatalf("expected 201 (API key should satisfy auth), got %d: %s", w.Code, w.Body.String())
	}
}

// TestEnvGather_AutoDetect_ClaudeAPIKeyWinsOverGCPProject tests the same
// api-key priority for the claude harness.
func TestEnvGather_AutoDetect_ClaudeAPIKeyWinsOverGCPProject(t *testing.T) {
	srv, _, projectDir := newTestServerWithHarnessConfig(t, "claude",
		"harness: claude\nimage: test-image\nuser: scion\n",
		`
schema_version: "1"
harness_configs:
  claude:
    harness: claude
profiles:
  default:
    runtime: mock
`)

	body := `{
		"name": "test-agent-claude-apikey-gcp",
		"id": "agent-uuid-claude-apikey-gcp",
		"gatherEnv": true,
		"projectPath": "` + projectDir + `",
		"resolvedEnv": {
			"GOOGLE_CLOUD_PROJECT": "my-project",
			"GOOGLE_CLOUD_REGION": "us-central1"
		},
		"resolvedSecrets": [
			{"name": "ANTHROPIC_API_KEY", "type": "environment", "target": "ANTHROPIC_API_KEY", "value": "sk-ant-test", "source": "project"}
		],
		"config": {"template": "claude", "profile": "default"}
	}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/agents", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	srv.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusCreated {
		t.Fatalf("expected 201 (ANTHROPIC_API_KEY should satisfy auth), got %d: %s", w.Code, w.Body.String())
	}
}

// TestEnvGather_HarnessAuthOverride tests that the --harness-auth CLI flag
// (passed as config.harnessAuth) overrides auth type detection in env-gather.
// This is a regression test: previously, --harness-auth api-key would fail
// because extractRequiredEnvKeys did not consider the harnessAuth field,
// so the broker skipped env-gather and then auth resolution failed because
// the API key was not in the broker's environment.
func TestEnvGather_HarnessAuthOverride(t *testing.T) {
	// Set up a gemini harness config with no auth_selected_type (auto-detect).
	// Provide an OAuth file secret so auto-detect would normally pick auth-file.
	// But --harness-auth api-key should override to api-key, requiring GEMINI_API_KEY.
	srv, _, projectDir := newTestServerWithHarnessConfig(t, "gemini",
		"harness: gemini\nimage: test-image\nuser: scion\n"+geminiAuthBlock,
		`
schema_version: "1"
profiles:
  default:
    runtime: mock
`)

	body := `{
		"name": "test-agent-harness-auth",
		"id": "agent-uuid-ha",
		"gatherEnv": true,
		"projectPath": "` + projectDir + `",
		"resolvedSecrets": [
			{"name": "GEMINI_OAUTH_CREDS", "type": "file", "target": "/home/gemini/.gemini/oauth_creds.json", "value": "{}", "source": "user"}
		],
		"config": {"template": "gemini", "profile": "default", "harnessAuth": "api-key"}
	}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/agents", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	srv.Handler().ServeHTTP(w, req)

	// harnessAuth=api-key should override the auto-detected auth-file type,
	// so GEMINI_API_KEY should be required and we should get 202.
	if w.Code != http.StatusAccepted {
		t.Fatalf("expected 202 (harnessAuth override requires GEMINI_API_KEY), got %d: %s", w.Code, w.Body.String())
	}

	var envReqs EnvRequirementsResponse
	if err := json.Unmarshal(w.Body.Bytes(), &envReqs); err != nil {
		t.Fatal("failed to decode response:", err)
	}

	found := false
	for _, k := range envReqs.Needs {
		if k == "GEMINI_API_KEY" {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("expected GEMINI_API_KEY in needs when harnessAuth=api-key, got needs=%v required=%v", envReqs.Needs, envReqs.Required)
	}
}

// TestEnvGather_HarnessAuthOverrideVertexAI tests that --harness-auth vertex-ai
// overrides auto-detect and requires vertex-ai credentials even when an API key
// would otherwise be detected as sufficient.
// TestEnvGather_NoProjectPath_GlobalFallback tests that when projectPath is empty
// (e.g. hub-only git projects), the broker falls back to the global ~/.scion
// directory for settings resolution, so auth env keys are still detected.
func TestEnvGather_NoProjectPath_GlobalFallback(t *testing.T) {
	// Set up a fake HOME with global .scion settings
	fakeHome := t.TempDir()
	globalDir := filepath.Join(fakeHome, ".scion")
	if err := os.MkdirAll(globalDir, 0755); err != nil {
		t.Fatal(err)
	}

	// Write settings with a gemini harness config
	settingsYAML := `
schema_version: "1"
default_harness_config: gemini
harness_configs:
  gemini:
    harness: gemini
profiles:
  default:
    runtime: mock
`
	if err := os.WriteFile(filepath.Join(globalDir, "settings.yaml"), []byte(settingsYAML), 0644); err != nil {
		t.Fatal(err)
	}

	// Create harness-config directory
	hcDir := filepath.Join(globalDir, "harness-configs", "gemini")
	if err := os.MkdirAll(hcDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(hcDir, "config.yaml"), []byte("harness: gemini\nimage: test-image\n"+geminiAuthBlock), 0644); err != nil {
		t.Fatal(err)
	}

	// Override HOME so GetGlobalDir() finds our fake home
	origHome := os.Getenv("HOME")
	t.Setenv("HOME", fakeHome)
	defer func() { _ = os.Setenv("HOME", origHome) }()

	cfg := DefaultServerConfig()
	cfg.BrokerID = "test-broker-id"
	cfg.BrokerName = "test-host"
	cfg.Debug = true
	cfg.StateDir = t.TempDir()
	mgr := &envCapturingManager{}
	// NameFunc returns "docker" so resolveManagerForOpts matches the settings-resolved runtime.
	rt := &runtime.MockRuntime{NameFunc: func() string { return "docker" }}
	srv := New(cfg, mgr, rt)

	// Send create request with NO projectPath — simulates a hub-only git project
	body := `{
		"name": "test-agent-no-project",
		"id": "agent-uuid-no-project",
		"gatherEnv": true,
		"config": {"profile": "default"}
	}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/agents", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	srv.Handler().ServeHTTP(w, req)

	// Should return 202 because GEMINI_API_KEY (or GOOGLE_API_KEY) is missing
	if w.Code != http.StatusAccepted {
		t.Fatalf("expected 202 (missing GEMINI_API_KEY with no projectPath), got %d: %s", w.Code, w.Body.String())
	}

	var envReqs EnvRequirementsResponse
	if err := json.Unmarshal(w.Body.Bytes(), &envReqs); err != nil {
		t.Fatal("failed to decode response:", err)
	}

	// GEMINI_API_KEY should be in the needs list
	found := false
	for _, k := range envReqs.Needs {
		if k == "GEMINI_API_KEY" {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("expected GEMINI_API_KEY in needs when no projectPath set, got needs=%v required=%v", envReqs.Needs, envReqs.Required)
	}
}

// TestEnvGather_SecretTargetFallbackToName tests that resolved secrets with
// empty Target fields fall back to Name for env-gather satisfaction checks.
func TestEnvGather_SecretTargetFallbackToName(t *testing.T) {
	settings := `
schema_version: "1"
harness_configs:
  gemini:
    harness: gemini
    env:
      CUSTOM_KEY: ""
profiles:
  default:
    runtime: mock
`
	srv, _, projectDir := newTestServerWithProjectPath(t, settings)

	// Send a request with a resolved secret that has Name but no Target
	body := `{
		"name": "test-agent-target-fallback",
		"id": "agent-uuid-target-fb",
		"gatherEnv": true,
		"projectPath": "` + projectDir + `",
		"config": {"template": "gemini", "profile": "default"},
		"resolvedSecrets": [
			{"name": "GEMINI_API_KEY", "type": "environment", "value": "sk-test", "target": ""},
			{"name": "CUSTOM_KEY", "type": "environment", "value": "custom-val", "target": ""}
		]
	}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/agents", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	srv.Handler().ServeHTTP(w, req)

	// Should return 201 because both GEMINI_API_KEY and CUSTOM_KEY are
	// satisfied via the Name fallback in resolved secrets
	if w.Code != http.StatusCreated {
		t.Fatalf("expected 201 (secrets with Name fallback should satisfy keys), got %d: %s", w.Code, w.Body.String())
	}
}

func TestEnvGather_HarnessAuthOverrideVertexAI(t *testing.T) {
	srv, _, projectDir := newTestServerWithHarnessConfig(t, "gemini",
		"harness: gemini\nimage: test-image\nuser: scion\n"+geminiAuthBlock,
		`
schema_version: "1"
profiles:
  default:
    runtime: mock
`)

	body := `{
		"name": "test-agent-harness-auth-vertex",
		"id": "agent-uuid-hav",
		"gatherEnv": true,
		"projectPath": "` + projectDir + `",
		"config": {"template": "gemini", "profile": "default", "harnessAuth": "vertex-ai"}
	}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/agents", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	srv.Handler().ServeHTTP(w, req)

	// harnessAuth=vertex-ai should require GOOGLE_CLOUD_PROJECT and region
	if w.Code != http.StatusAccepted {
		t.Fatalf("expected 202 (harnessAuth vertex-ai requires project/region), got %d: %s", w.Code, w.Body.String())
	}

	var envReqs EnvRequirementsResponse
	if err := json.Unmarshal(w.Body.Bytes(), &envReqs); err != nil {
		t.Fatal("failed to decode response:", err)
	}

	// Check that GOOGLE_CLOUD_PROJECT is required
	foundProject := false
	for _, k := range envReqs.Needs {
		if k == "GOOGLE_CLOUD_PROJECT" {
			foundProject = true
		}
	}
	// Also check required list (may be satisfied by resolvedEnv)
	for _, k := range envReqs.Required {
		if k == "GOOGLE_CLOUD_PROJECT" {
			foundProject = true
		}
	}
	if !foundProject {
		t.Errorf("expected GOOGLE_CLOUD_PROJECT in needs/required when harnessAuth=vertex-ai, got needs=%v required=%v", envReqs.Needs, envReqs.Required)
	}
}

// TestEnvGather_AlternativesForAnyOfGroup tests that when the broker returns a
// 202 with a needed key from an any_of group, the Alternatives field maps the
// canonical key to its alternative names from the group.
// Regression test for the GOOGLE_CLOUD_LOCATION as_needed bug: the hub needs
// to know that CLOUD_ML_REGION and GOOGLE_CLOUD_LOCATION are alternatives for
// GOOGLE_CLOUD_REGION so it can match as_needed env vars stored under those names.
func TestEnvGather_AlternativesForAnyOfGroup(t *testing.T) {
	srv, _, projectDir := newTestServerWithHarnessConfig(t, "claude",
		"harness: claude\nimage: test-image\nuser: scion\n"+claudeAuthBlock,
		`
schema_version: "1"
harness_configs:
  claude:
    harness: claude
profiles:
  default:
    runtime: mock
`)

	body := `{
		"name": "test-agent-alternatives",
		"id": "agent-uuid-alt",
		"gatherEnv": true,
		"projectPath": "` + projectDir + `",
		"resolvedEnv": {
			"GOOGLE_CLOUD_PROJECT": "my-project"
		},
		"config": {"template": "claude", "harnessConfig": "claude", "profile": "default"}
	}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/agents", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	srv.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusAccepted {
		t.Fatalf("expected 202 (vertex-ai needs region), got %d: %s", w.Code, w.Body.String())
	}

	var envReqs EnvRequirementsResponse
	if err := json.Unmarshal(w.Body.Bytes(), &envReqs); err != nil {
		t.Fatal("failed to decode response:", err)
	}

	// Verify GOOGLE_CLOUD_REGION is in Needs (canonical key)
	needsMap := make(map[string]struct{})
	for _, k := range envReqs.Needs {
		needsMap[k] = struct{}{}
	}
	if _, ok := needsMap["GOOGLE_CLOUD_REGION"]; !ok {
		t.Fatalf("expected GOOGLE_CLOUD_REGION in Needs, got %v", envReqs.Needs)
	}

	// Verify Alternatives map contains the any_of alternatives for GOOGLE_CLOUD_REGION
	if envReqs.Alternatives == nil {
		t.Fatal("expected non-nil Alternatives map")
	}
	alts, ok := envReqs.Alternatives["GOOGLE_CLOUD_REGION"]
	if !ok {
		t.Fatalf("expected GOOGLE_CLOUD_REGION in Alternatives map, got %v", envReqs.Alternatives)
	}

	altSet := make(map[string]struct{}, len(alts))
	for _, a := range alts {
		altSet[a] = struct{}{}
	}
	if _, ok := altSet["CLOUD_ML_REGION"]; !ok {
		t.Errorf("expected CLOUD_ML_REGION in alternatives for GOOGLE_CLOUD_REGION, got %v", alts)
	}
	if _, ok := altSet["GOOGLE_CLOUD_LOCATION"]; !ok {
		t.Errorf("expected GOOGLE_CLOUD_LOCATION in alternatives for GOOGLE_CLOUD_REGION, got %v", alts)
	}

	// Satisfied keys should NOT appear in Alternatives
	if _, ok := envReqs.Alternatives["GOOGLE_CLOUD_PROJECT"]; ok {
		t.Errorf("GOOGLE_CLOUD_PROJECT should not be in Alternatives (it is satisfied)")
	}
}

// TestEnvGather_VertexAI_GCPIdentitySkipsADC tests that vertex-ai auth with
// GCPIdentity.MetadataMode set to "assign" or "passthrough" does not require
// the gcloud-adc file secret. Both modes provide GCP credentials (assign via
// broker-managed SA, passthrough via ambient GCE metadata), so the ADC file
// is unnecessary.
// Regression test for https://github.com/ptone/scion/issues/1276.
func TestEnvGather_VertexAI_GCPIdentitySkipsADC(t *testing.T) {
	tests := []struct {
		name            string
		metadataMode    string
		gcpIdentityJSON string
	}{
		{
			name:            store.GCPMetadataModePassthrough,
			metadataMode:    store.GCPMetadataModePassthrough,
			gcpIdentityJSON: `{"metadata_mode": "passthrough"}`,
		},
		{
			name:            store.GCPMetadataModeAssign,
			metadataMode:    store.GCPMetadataModeAssign,
			gcpIdentityJSON: `{"metadata_mode": "assign", "sa_email": "test@proj.iam.gserviceaccount.com", "project_id": "my-project"}`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv, _, projectDir := newTestServerWithHarnessConfig(t, "claude",
				"harness: claude\nimage: test-image\nuser: scion\nauth_selected_type: vertex-ai\n"+claudeAuthBlock,
				`
schema_version: "1"
harness_configs:
  claude:
    harness: claude
profiles:
  default:
    runtime: mock
`)

			body := `{
				"name": "test-agent-vertex-` + tt.metadataMode + `",
				"id": "agent-uuid-` + tt.metadataMode + `",
				"gatherEnv": true,
				"projectPath": "` + projectDir + `",
				"resolvedEnv": {
					"GOOGLE_CLOUD_PROJECT": "my-project",
					"GOOGLE_CLOUD_REGION": "us-central1"
				},
				"config": {
					"template": "claude",
					"profile": "default",
					"gcpIdentity": ` + tt.gcpIdentityJSON + `
				}
			}`
			req := httptest.NewRequest(http.MethodPost, "/api/v1/agents", strings.NewReader(body))
			req.Header.Set("Content-Type", "application/json")
			w := httptest.NewRecorder()

			srv.Handler().ServeHTTP(w, req)

			if w.Code != http.StatusCreated {
				t.Fatalf("expected 201 (%s should skip ADC requirement), got %d: %s",
					tt.metadataMode, w.Code, w.Body.String())
			}
		})
	}
}

// TestExtractRequiredEnvKeys_KubernetesImplicitPassthroughSkipsADC covers
// ptone/scion#2328: a dispatch profile that resolves to the Kubernetes
// runtime gets passthrough by default when no GCP identity is configured at
// all (no GCPIdentity field here, unlike
// TestEnvGather_VertexAI_GCPIdentitySkipsADC above). extractRequiredEnvKeys
// must recognize that implicit passthrough as GCP-credentialed the same way
// buildStartContext would at actual dispatch time — otherwise an
// unconfigured Kubernetes agent using vertex-ai would be wrongly asked for
// an ADC file it will never need.
//
// Calls extractRequiredEnvKeys directly rather than through the full HTTP
// create handler: nothing else about the create path (template hydration,
// hub connectivity, actual dispatch) is relevant to this preflight
// computation. The preflight resolves the runtime's name via
// resolveRuntimeNameForOpts, which never builds a real runtime client (see
// that function's doc comment, handlers.go) — unlike resolveManagerForOpts,
// a settings profile that resolves to "kubernetes" here does not attempt a
// real cluster connection, so no resolveAuxiliaryRuntime mock is needed.
func TestExtractRequiredEnvKeys_KubernetesImplicitPassthroughSkipsADC(t *testing.T) {
	srv, _, projectDir := newTestServerWithHarnessConfig(t, "claude",
		"harness: claude\nimage: test-image\nuser: scion\nauth_selected_type: vertex-ai\n"+claudeAuthBlock,
		`
schema_version: "1"
harness_configs:
  claude:
    harness: claude
profiles:
  default:
    runtime: kubernetes
runtimes:
  kubernetes:
    type: kubernetes
`)

	req := CreateAgentRequest{
		Name:        "test-agent-vertex-k8s-implicit",
		ProjectPath: projectDir,
		ResolvedEnv: map[string]string{
			"GOOGLE_CLOUD_PROJECT": "my-project",
			"GOOGLE_CLOUD_REGION":  "us-central1",
		},
		Config: &CreateAgentConfig{
			Template: "claude",
			Profile:  "default",
		},
	}

	required, secretInfo, _, _ := srv.extractRequiredEnvKeys(req, "")
	if len(required) != 0 {
		t.Errorf("expected no required keys once Kubernetes' implicit passthrough is recognized as GCP-credentialed, got %v (secretInfo: %v)", required, secretInfo)
	}
}

// TestExtractRequiredEnvKeys_DockerResolvedEnvPassthroughSkipsADC is the
// Docker-side counterpart of
// TestExtractRequiredEnvKeys_KubernetesImplicitPassthroughSkipsADC, and a
// behavior-change regression pin: before ptone/scion#2328, this preflight's
// GCP-credential check (gcpSAAssigned) only ever consulted req.Config.GCPIdentity,
// so a mode carried in req.ResolvedEnv (e.g. a resolved project or hub
// default GCP identity, supplied the same way buildStartContext's own
// SCION_METADATA_MODE fallback reads it — see effectiveGCPMetadataMode) was
// invisible to it on every runtime, not just Kubernetes. Routing this
// preflight through effectiveGCPMetadataMode to add the Kubernetes-aware
// default also picked up that resolvedEnv source for Docker and every other
// runtime: a Docker dispatch with a resolvedEnv-carried "passthrough" (no
// Config.GCPIdentity at all here) now also skips the ADC file requirement,
// where previously it would not have. Disclosed in the PR body as a
// Docker-visible behavior change, not just a Kubernetes one.
//
// The resolvedEnv here includes SCION_METADATA_MODE_SOURCE=hub, matching
// what a real hub dispatch always sends alongside an elevated mode: absent
// that marker, effectiveGCPMetadataMode now treats a resolvedEnv-carried
// "passthrough"/"assign" as untrusted and downgrades it, so this preflight's
// required-keys answer stays consistent with what buildStartContext will
// actually resolve for the same dispatch.
func TestExtractRequiredEnvKeys_DockerResolvedEnvPassthroughSkipsADC(t *testing.T) {
	srv, _, projectDir := newTestServerWithHarnessConfig(t, "claude",
		"harness: claude\nimage: test-image\nuser: scion\nauth_selected_type: vertex-ai\n"+claudeAuthBlock,
		`
schema_version: "1"
harness_configs:
  claude:
    harness: claude
profiles:
  default:
    runtime: docker
runtimes:
  docker:
    type: docker
`)

	req := CreateAgentRequest{
		Name:        "test-agent-vertex-docker-resolvedenv-passthrough",
		ProjectPath: projectDir,
		ResolvedEnv: map[string]string{
			"GOOGLE_CLOUD_PROJECT":       "my-project",
			"GOOGLE_CLOUD_REGION":        "us-central1",
			"SCION_METADATA_MODE":        store.GCPMetadataModePassthrough,
			"SCION_METADATA_MODE_SOURCE": "hub",
		},
		Config: &CreateAgentConfig{
			Template: "claude",
			Profile:  "default",
		},
	}

	required, secretInfo, _, _ := srv.extractRequiredEnvKeys(req, "")
	if len(required) != 0 {
		t.Errorf("expected no required keys once a resolvedEnv-carried passthrough mode is recognized as GCP-credentialed on Docker, got %v (secretInfo: %v)", required, secretInfo)
	}
}

// TestEnvGather_DefaultTypeCredentialBeatsGCPIdentity is the handler-level
// regression pin for the extractRequiredEnvKeys change: a present credential
// for the harness's own default_type (claude's ANTHROPIC_API_KEY) must
// satisfy auto-detected auth even when a GCP SA is reachable via identity —
// it must not additionally demand vertex-ai's GOOGLE_CLOUD_PROJECT/LOCATION
// keys. No auth_selected_type is set here (unlike
// TestEnvGather_VertexAI_GCPIdentitySkipsADC above), so this exercises the
// auto-detect path through AutoDetectAuthType, not an explicit selection.
func TestEnvGather_DefaultTypeCredentialBeatsGCPIdentity(t *testing.T) {
	srv, _, projectDir := newTestServerWithHarnessConfig(t, "claude",
		"harness: claude\nimage: test-image\nuser: scion\n"+claudeAuthBlock,
		`
schema_version: "1"
harness_configs:
  claude:
    harness: claude
profiles:
  default:
    runtime: mock
`)

	body := `{
		"name": "test-agent-default-beats-identity",
		"id": "agent-uuid-default-beats-identity",
		"gatherEnv": true,
		"projectPath": "` + projectDir + `",
		"resolvedEnv": {
			"ANTHROPIC_API_KEY": "sk-ant-test"
		},
		"config": {
			"template": "claude",
			"profile": "default",
			"gcpIdentity": {"metadata_mode": "passthrough"}
		}
	}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/agents", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	srv.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusCreated {
		t.Fatalf("expected 201 (ANTHROPIC_API_KEY should satisfy auth without demanding vertex-ai keys), got %d: %s",
			w.Code, w.Body.String())
	}
}

// TestEnvGather_AvailableAsNeededKeys_AutodetectAPIKey is a regression test for
// #1447. When a harness has default_type: oauth-token (like antigravity) and
// GEMINI_API_KEY is an as_needed hub secret, autodetect must still see the key
// via AvailableAsNeededKeys and select api-key auth rather than falling through
// to oauth-token (which has no env requirements and causes the agent to start
// without credentials).
func TestEnvGather_AvailableAsNeededKeys_AutodetectAPIKey(t *testing.T) {
	// Use antigravity-like harness: default_type is oauth-token,
	// autodetect.env maps GEMINI_API_KEY → api-key.
	srv, _, projectDir := newTestServerWithHarnessConfig(t, "antigravity",
		"harness: antigravity\nimage: test-image\nuser: scion\n"+antigravityAuthBlock,
		`
schema_version: "1"
harness_configs:
  antigravity:
    harness: antigravity
profiles:
  default:
    runtime: mock
`)

	// Create template so FindTemplateInProjectPath can resolve it.
	tplDir := filepath.Join(projectDir, "templates", "antigravity")
	if err := os.MkdirAll(tplDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(tplDir, "scion-agent.yaml"),
		[]byte("harness_config: antigravity\n"), 0644); err != nil {
		t.Fatal(err)
	}

	// Send request with GEMINI_API_KEY in availableAsNeededKeys (simulating a
	// hub secret with injection_mode=as_needed) but NOT in resolvedEnv or
	// resolvedSecrets.
	body := `{
		"name": "test-agent-asneeded-autodetect",
		"id": "agent-uuid-asneeded",
		"gatherEnv": true,
		"projectPath": "` + projectDir + `",
		"availableAsNeededKeys": ["GEMINI_API_KEY"],
		"config": {"template": "antigravity", "profile": "default"}
	}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/agents", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	srv.Handler().ServeHTTP(w, req)

	// Autodetect should see GEMINI_API_KEY via AvailableAsNeededKeys and select
	// api-key auth. GEMINI_API_KEY is not in resolvedEnv, so it should appear
	// in the needs list (returned as 202).
	if w.Code != http.StatusAccepted {
		t.Fatalf("expected 202 (GEMINI_API_KEY needed), got %d: %s", w.Code, w.Body.String())
	}

	var envReqs EnvRequirementsResponse
	if err := json.Unmarshal(w.Body.Bytes(), &envReqs); err != nil {
		t.Fatal("failed to decode response:", err)
	}

	// GEMINI_API_KEY should be in the required/needs list because autodetect
	// selected api-key auth (not oauth-token, which has no env requirements).
	needsMap := make(map[string]struct{})
	for _, k := range envReqs.Needs {
		needsMap[k] = struct{}{}
	}
	requiredMap := make(map[string]struct{})
	for _, k := range envReqs.Required {
		requiredMap[k] = struct{}{}
	}
	if _, ok := requiredMap["GEMINI_API_KEY"]; !ok {
		t.Errorf("expected GEMINI_API_KEY in required (autodetect should pick api-key), got required=%v needs=%v", envReqs.Required, envReqs.Needs)
	}
	if _, ok := needsMap["GEMINI_API_KEY"]; !ok {
		t.Errorf("expected GEMINI_API_KEY in needs (not yet resolved), got needs=%v", envReqs.Needs)
	}
}

// TestEnvGather_NoAvailableAsNeededKeys_FallsBackToDefault is a complementary
// test for #1447: without AvailableAsNeededKeys, autodetect has no env-var
// signals and falls back to default_type (oauth-token), which has no env
// requirements. This demonstrates the pre-fix behavior.
func TestEnvGather_NoAvailableAsNeededKeys_FallsBackToDefault(t *testing.T) {
	srv, _, projectDir := newTestServerWithHarnessConfig(t, "antigravity",
		"harness: antigravity\nimage: test-image\nuser: scion\n"+antigravityAuthBlock,
		`
schema_version: "1"
harness_configs:
  antigravity:
    harness: antigravity
profiles:
  default:
    runtime: mock
`)

	tplDir := filepath.Join(projectDir, "templates", "antigravity")
	if err := os.MkdirAll(tplDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(tplDir, "scion-agent.yaml"),
		[]byte("harness_config: antigravity\n"), 0644); err != nil {
		t.Fatal(err)
	}

	// Same request but WITHOUT availableAsNeededKeys — autodetect has no
	// env-var signals and falls back to default_type (oauth-token).
	body := `{
		"name": "test-agent-no-asneeded",
		"id": "agent-uuid-no-asneeded",
		"gatherEnv": true,
		"projectPath": "` + projectDir + `",
		"config": {"template": "antigravity", "profile": "default"}
	}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/agents", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	srv.Handler().ServeHTTP(w, req)

	// Without AvailableAsNeededKeys, autodetect doesn't see GEMINI_API_KEY,
	// so it falls back to oauth-token. oauth-token only requires file secrets
	// (AGY_TOKEN) which are file-type, not env-type. The env-gather phase
	// sees no env keys needed and the agent should proceed past env-gather.
	// The broker returns 202 for the AGY_TOKEN file secret requirement.
	if w.Code == http.StatusAccepted {
		var envReqs EnvRequirementsResponse
		if err := json.Unmarshal(w.Body.Bytes(), &envReqs); err != nil {
			t.Fatal("failed to decode response:", err)
		}
		// GEMINI_API_KEY should NOT appear in required/needs
		for _, k := range envReqs.Required {
			if k == "GEMINI_API_KEY" {
				t.Errorf("GEMINI_API_KEY should NOT be in required without AvailableAsNeededKeys, got required=%v", envReqs.Required)
			}
		}
	}
	// Either 201 (started) or 202 (needs file secret AGY_TOKEN) is acceptable —
	// the point is that GEMINI_API_KEY is NOT required.
}

// The tests below cover the env-gather preflight honouring settings-resolved
// env (ptone/scion#2158): extractRequiredEnvKeys must count a non-empty
// GOOGLE_CLOUD_PROJECT / GOOGLE_CLOUD_LOCATION as satisfied when it comes
// from harness_configs.<h>.env, profiles.<p>.harness_overrides.<h>.env, or
// the harness-config directory's own `env:` block — the same sources
// agent.Start and ProvisionAgent already deliver to the pod. Each request
// uses gcpIdentity passthrough to waive the separate gcloud-adc file
// requirement, isolating the env-key check.

// TestEnvGather_VertexAI_SatisfiedByHarnessConfigsEnv confirms a key
// satisfied only by settings harness_configs.<h>.env passes.
func TestEnvGather_VertexAI_SatisfiedByHarnessConfigsEnv(t *testing.T) {
	srv, _, projectDir := newTestServerWithHarnessConfig(t, "claude",
		"harness: claude\nimage: test-image\nuser: scion\nauth_selected_type: vertex-ai\n"+claudeAuthBlock,
		`
schema_version: "1"
harness_configs:
  claude:
    harness: claude
    env:
      GOOGLE_CLOUD_PROJECT: "p1"
      GOOGLE_CLOUD_LOCATION: "us-east5"
profiles:
  default:
    runtime: mock
`)

	body := `{
		"name": "test-agent-vertex-hc-env",
		"id": "agent-uuid-vertex-hc-env",
		"gatherEnv": true,
		"projectPath": "` + projectDir + `",
		"config": {
			"template": "claude",
			"profile": "default",
			"gcpIdentity": {"metadata_mode": "passthrough"}
		}
	}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/agents", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	srv.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusCreated {
		t.Fatalf("expected 201 (harness_configs.env should satisfy GOOGLE_CLOUD_PROJECT/LOCATION), got %d: %s", w.Code, w.Body.String())
	}
}

// TestEnvGather_VertexAI_SatisfiedByHarnessOverridesEnv confirms a key
// satisfied only by profiles.<p>.harness_overrides.<h>.env passes.
func TestEnvGather_VertexAI_SatisfiedByHarnessOverridesEnv(t *testing.T) {
	srv, _, projectDir := newTestServerWithHarnessConfig(t, "claude",
		"harness: claude\nimage: test-image\nuser: scion\nauth_selected_type: vertex-ai\n"+claudeAuthBlock,
		`
schema_version: "1"
harness_configs:
  claude:
    harness: claude
profiles:
  default:
    runtime: mock
    harness_overrides:
      claude:
        env:
          GOOGLE_CLOUD_PROJECT: "p1"
          GOOGLE_CLOUD_LOCATION: "us-east5"
`)

	body := `{
		"name": "test-agent-vertex-override-env",
		"id": "agent-uuid-vertex-override-env",
		"gatherEnv": true,
		"projectPath": "` + projectDir + `",
		"config": {
			"template": "claude",
			"profile": "default",
			"gcpIdentity": {"metadata_mode": "passthrough"}
		}
	}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/agents", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	srv.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusCreated {
		t.Fatalf("expected 201 (harness_overrides.env should satisfy GOOGLE_CLOUD_PROJECT/LOCATION), got %d: %s", w.Code, w.Body.String())
	}
}

// TestEnvGather_VertexAI_SatisfiedByHarnessConfigDirEnv confirms a key
// satisfied only by the harness-config directory's own `env:` block passes.
func TestEnvGather_VertexAI_SatisfiedByHarnessConfigDirEnv(t *testing.T) {
	srv, _, projectDir := newTestServerWithHarnessConfig(t, "claude",
		"harness: claude\nimage: test-image\nuser: scion\nauth_selected_type: vertex-ai\n"+
			"env:\n  GOOGLE_CLOUD_PROJECT: p1\n  GOOGLE_CLOUD_LOCATION: us-east5\n"+claudeAuthBlock,
		`
schema_version: "1"
harness_configs:
  claude:
    harness: claude
profiles:
  default:
    runtime: mock
`)

	body := `{
		"name": "test-agent-vertex-dir-env",
		"id": "agent-uuid-vertex-dir-env",
		"gatherEnv": true,
		"projectPath": "` + projectDir + `",
		"config": {
			"template": "claude",
			"profile": "default",
			"gcpIdentity": {"metadata_mode": "passthrough"}
		}
	}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/agents", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	srv.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusCreated {
		t.Fatalf("expected 201 (harness-config dir env should satisfy GOOGLE_CLOUD_PROJECT/LOCATION), got %d: %s", w.Code, w.Body.String())
	}
}

// TestEnvGather_VertexAI_StillMissingSameError confirms that when none of the
// settings sources declare GOOGLE_CLOUD_PROJECT/LOCATION, the preflight
// returns the 202/needs response for both keys.
func TestEnvGather_VertexAI_StillMissingSameError(t *testing.T) {
	srv, _, projectDir := newTestServerWithHarnessConfig(t, "claude",
		"harness: claude\nimage: test-image\nuser: scion\nauth_selected_type: vertex-ai\n"+claudeAuthBlock,
		`
schema_version: "1"
harness_configs:
  claude:
    harness: claude
profiles:
  default:
    runtime: mock
`)

	body := `{
		"name": "test-agent-vertex-still-missing",
		"id": "agent-uuid-vertex-still-missing",
		"gatherEnv": true,
		"projectPath": "` + projectDir + `",
		"config": {
			"template": "claude",
			"profile": "default",
			"gcpIdentity": {"metadata_mode": "passthrough"}
		}
	}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/agents", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	srv.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusAccepted {
		t.Fatalf("expected 202, got %d: %s", w.Code, w.Body.String())
	}
	var envReqs EnvRequirementsResponse
	if err := json.Unmarshal(w.Body.Bytes(), &envReqs); err != nil {
		t.Fatal("failed to decode response:", err)
	}
	for _, want := range []string{"GOOGLE_CLOUD_PROJECT", "GOOGLE_CLOUD_REGION"} {
		found := false
		for _, k := range envReqs.Needs {
			if k == want {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("expected %q in needs, got %v", want, envReqs.Needs)
		}
	}
}

// TestEnvGather_VertexAI_EmptySettingsEnvValueDoesNotCount confirms that a
// settings env entry with an empty value does not itself satisfy the
// requirement when there is no harness-config directory entry to fall
// through to (authCandidateKeyValue's last resort is a host-env passthrough,
// which needs the broker process's own GCP env vars out of the way here to
// be deterministic regardless of the host running the test).
func TestEnvGather_VertexAI_EmptySettingsEnvValueDoesNotCount(t *testing.T) {
	unsetHostGCPEnv(t)
	srv, _, projectDir := newTestServerWithHarnessConfig(t, "claude",
		"harness: claude\nimage: test-image\nuser: scion\nauth_selected_type: vertex-ai\n"+claudeAuthBlock,
		`
schema_version: "1"
harness_configs:
  claude:
    harness: claude
    env:
      GOOGLE_CLOUD_PROJECT: ""
      GOOGLE_CLOUD_LOCATION: ""
profiles:
  default:
    runtime: mock
`)

	body := `{
		"name": "test-agent-vertex-empty-env",
		"id": "agent-uuid-vertex-empty-env",
		"gatherEnv": true,
		"projectPath": "` + projectDir + `",
		"config": {
			"template": "claude",
			"profile": "default",
			"gcpIdentity": {"metadata_mode": "passthrough"}
		}
	}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/agents", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	srv.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusAccepted {
		t.Fatalf("expected 202 (empty settings env value must not satisfy the requirement), got %d: %s", w.Code, w.Body.String())
	}
	var envReqs EnvRequirementsResponse
	if err := json.Unmarshal(w.Body.Bytes(), &envReqs); err != nil {
		t.Fatal("failed to decode response:", err)
	}
	for _, want := range []string{"GOOGLE_CLOUD_PROJECT", "GOOGLE_CLOUD_REGION"} {
		found := false
		for _, k := range envReqs.Needs {
			if k == want {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("expected %q in needs (empty settings value must not count as satisfied), got %v", want, envReqs.Needs)
		}
	}
}

// TestEnvGather_VertexAI_EmptySettingsEnvFallsThroughToHarnessConfigDir
// pins the launch model for an auth-candidate key: run.go's Start
// deletes an empty GOOGLE_CLOUD_PROJECT entry from opts.Env after auth
// resolves (its resolved.EnvVars only ever carries non-empty values), which
// clears the way for finalScionCfg.Env — here, the harness-config
// directory's own non-empty value — to reach the container. So an empty
// settings value for an auth key does NOT block the directory's
// fall-through the way it would for an ordinary key.
func TestEnvGather_VertexAI_EmptySettingsEnvFallsThroughToHarnessConfigDir(t *testing.T) {
	srv, _, projectDir := newTestServerWithHarnessConfig(t, "claude",
		"harness: claude\nimage: test-image\nuser: scion\nauth_selected_type: vertex-ai\n"+
			"env:\n  GOOGLE_CLOUD_PROJECT: p1\n  GOOGLE_CLOUD_LOCATION: us-east5\n"+claudeAuthBlock,
		`
schema_version: "1"
harness_configs:
  claude:
    harness: claude
    env:
      GOOGLE_CLOUD_PROJECT: ""
profiles:
  default:
    runtime: mock
`)

	body := `{
		"name": "test-agent-vertex-fallthrough",
		"id": "agent-uuid-vertex-fallthrough",
		"gatherEnv": true,
		"projectPath": "` + projectDir + `",
		"config": {
			"template": "claude",
			"profile": "default",
			"gcpIdentity": {"metadata_mode": "passthrough"}
		}
	}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/agents", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	srv.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusCreated {
		t.Fatalf("expected 201 (an empty settings value for an auth key must fall through to the harness-config dir's value), got %d: %s", w.Code, w.Body.String())
	}
}

// TestEnvGather_NonAuthKey_EmptySettingsEnvBlocksHarnessConfigDir is the
// control for the test above: for an ORDINARY (non-auth-candidate) key,
// launch does not delete an empty opts.Env entry, so it keeps blocking a
// lower-ranked source exactly as withDir models. CUSTOM_ENV_KEY is not part
// of any harness auth block, so it takes the ordinary-key path.
func TestEnvGather_NonAuthKey_EmptySettingsEnvBlocksHarnessConfigDir(t *testing.T) {
	srv, _, projectDir := newTestServerWithHarnessConfig(t, "claude",
		"harness: claude\nimage: test-image\nuser: scion\n"+
			"env:\n  CUSTOM_ENV_KEY: dir-value\n",
		`
schema_version: "1"
harness_configs:
  claude:
    harness: claude
    env:
      ANTHROPIC_API_KEY: ""
      CUSTOM_ENV_KEY: ""
profiles:
  default:
    runtime: mock
`)

	body := `{
		"name": "test-agent-nonauth-blocks",
		"id": "agent-uuid-nonauth-blocks",
		"gatherEnv": true,
		"projectPath": "` + projectDir + `",
		"resolvedEnv": {"ANTHROPIC_API_KEY": "sk-test"},
		"config": {"template": "claude", "profile": "default"}
	}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/agents", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	srv.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusAccepted {
		t.Fatalf("expected 202 (settings' empty CUSTOM_ENV_KEY must still block the harness-config dir's value for an ordinary key), got %d: %s", w.Code, w.Body.String())
	}
	var envReqs EnvRequirementsResponse
	if err := json.Unmarshal(w.Body.Bytes(), &envReqs); err != nil {
		t.Fatal("failed to decode response:", err)
	}
	found := false
	for _, k := range envReqs.Needs {
		if k == "CUSTOM_ENV_KEY" {
			found = true
		}
	}
	if !found {
		t.Errorf("expected CUSTOM_ENV_KEY in needs, got %v", envReqs.Needs)
	}
}

// The tests below cover: an empty ResolvedEnv/Config.Env entry must still
// block a lower-ranked settings/dir fill, the hydrated harness-config
// directory must be preferred over an on-disk one of the same name,
// auto-detect must see settings env but not directory env, and the outer
// needs/hubHas check must be exercised directly, not just the
// auth-key-group path.

// TestEnvGather_VertexAI_EmptyResolvedEnvBlocksSettingsFill pins the
// documented conservative choice for an empty ResolvedEnv/Config.Env entry
// on an auth-candidate key: at launch, run.go's Start deletes an empty
// auth-candidate opts.Env entry after auth resolves regardless of where the
// empty value came from, so the pod may still receive the settings or
// directory value — this test's own value actually reaches the container.
// The preflight deliberately reports the key as missing anyway, because it
// cannot tell this case apart from the common real Hub-dispatch case where
// the empty entry also outranks the directory/settings inside the
// container's own config and the pod really does end up empty (see
// authCandidateKeyValue).
func TestEnvGather_VertexAI_EmptyResolvedEnvBlocksSettingsFill(t *testing.T) {
	srv, _, projectDir := newTestServerWithHarnessConfig(t, "claude",
		"harness: claude\nimage: test-image\nuser: scion\nauth_selected_type: vertex-ai\n"+claudeAuthBlock,
		`
schema_version: "1"
harness_configs:
  claude:
    harness: claude
    env:
      GOOGLE_CLOUD_PROJECT: "p1"
      GOOGLE_CLOUD_LOCATION: "us-east5"
profiles:
  default:
    runtime: mock
`)

	body := `{
		"name": "test-agent-empty-resolvedenv-blocks",
		"id": "agent-uuid-empty-resolvedenv-blocks",
		"gatherEnv": true,
		"projectPath": "` + projectDir + `",
		"resolvedEnv": {"GOOGLE_CLOUD_PROJECT": ""},
		"config": {
			"template": "claude",
			"profile": "default",
			"gcpIdentity": {"metadata_mode": "passthrough"}
		}
	}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/agents", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	srv.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusAccepted {
		t.Fatalf("expected 202 (empty ResolvedEnv entry must block the settings fill), got %d: %s", w.Code, w.Body.String())
	}
	var envReqs EnvRequirementsResponse
	if err := json.Unmarshal(w.Body.Bytes(), &envReqs); err != nil {
		t.Fatal("failed to decode response:", err)
	}
	found := false
	for _, k := range envReqs.Needs {
		if k == "GOOGLE_CLOUD_PROJECT" {
			found = true
		}
	}
	if !found {
		t.Errorf("expected GOOGLE_CLOUD_PROJECT in needs, got %v", envReqs.Needs)
	}
}

// TestEnvGather_VertexAI_EmptyConfigEnvBlocksSettingsFill is the same as
// above but with the empty value coming from inline Config.Env instead of
// ResolvedEnv.
func TestEnvGather_VertexAI_EmptyConfigEnvBlocksSettingsFill(t *testing.T) {
	srv, _, projectDir := newTestServerWithHarnessConfig(t, "claude",
		"harness: claude\nimage: test-image\nuser: scion\nauth_selected_type: vertex-ai\n"+claudeAuthBlock,
		`
schema_version: "1"
harness_configs:
  claude:
    harness: claude
    env:
      GOOGLE_CLOUD_PROJECT: "p1"
      GOOGLE_CLOUD_LOCATION: "us-east5"
profiles:
  default:
    runtime: mock
`)

	body := `{
		"name": "test-agent-empty-configenv-blocks",
		"id": "agent-uuid-empty-configenv-blocks",
		"gatherEnv": true,
		"projectPath": "` + projectDir + `",
		"config": {
			"template": "claude",
			"profile": "default",
			"env": ["GOOGLE_CLOUD_PROJECT="],
			"gcpIdentity": {"metadata_mode": "passthrough"}
		}
	}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/agents", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	srv.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusAccepted {
		t.Fatalf("expected 202 (empty Config.Env entry must block the settings fill), got %d: %s", w.Code, w.Body.String())
	}
	var envReqs EnvRequirementsResponse
	if err := json.Unmarshal(w.Body.Bytes(), &envReqs); err != nil {
		t.Fatal("failed to decode response:", err)
	}
	found := false
	for _, k := range envReqs.Needs {
		if k == "GOOGLE_CLOUD_PROJECT" {
			found = true
		}
	}
	if !found {
		t.Errorf("expected GOOGLE_CLOUD_PROJECT in needs, got %v", envReqs.Needs)
	}
}

// writeHarnessConfigDirAt writes a minimal harness-config dir at an
// arbitrary path (used to build a standalone "hydrated" dir separate from
// the project's on-disk harness-configs/ tree).
func writeHarnessConfigDirAt(t *testing.T, dir, yaml string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "config.yaml"), []byte(yaml), 0o644); err != nil {
		t.Fatal(err)
	}
}

// TestEnvGather_VertexAI_HydratedDirPreferredOverOnDisk confirms that when a
// hydrated hub-managed harness-config is supplied, its env is what counts —
// not an on-disk directory of the same name — matching
// resolveHarnessConfigDir (pkg/agent/provision.go), which prefers the
// dispatch-context hydrated copy unconditionally and never merges it with an
// on-disk one.
func TestEnvGather_VertexAI_HydratedDirPreferredOverOnDisk(t *testing.T) {
	// On-disk dir has no env; hydrated dir has the vars launch will actually use.
	srv, _, projectDir := newTestServerWithHarnessConfig(t, "claude",
		"harness: claude\nimage: test-image\nuser: scion\nauth_selected_type: vertex-ai\n"+claudeAuthBlock,
		`
schema_version: "1"
profiles:
  default:
    runtime: mock
`)
	hydratedDir := filepath.Join(t.TempDir(), "claude")
	writeHarnessConfigDirAt(t, hydratedDir,
		"harness: claude\nimage: test-image\nuser: scion\nauth_selected_type: vertex-ai\nenv:\n  GOOGLE_CLOUD_PROJECT: p1\n  GOOGLE_CLOUD_LOCATION: us-east5\n"+claudeAuthBlock)

	var req CreateAgentRequest
	req.ProjectPath = projectDir
	req.Config = &CreateAgentConfig{HarnessConfig: "claude", Profile: "default"}
	required, _, _, _ := srv.extractRequiredEnvKeys(req, "", hydratedDir)
	for _, k := range required {
		if k == "GOOGLE_CLOUD_PROJECT" {
			t.Errorf("hydrated dir env (what launch uses) was ignored: GOOGLE_CLOUD_PROJECT reported missing, required=%v", required)
		}
	}
}

// TestEnvGather_VertexAI_OnDiskEnvIgnoredWhenHydratedLacksIt is the reverse:
// the on-disk dir has the vars, but the hydrated dir (what launch actually
// reads) does not, so the preflight must report them missing rather than
// crediting the on-disk copy launch will not use.
func TestEnvGather_VertexAI_OnDiskEnvIgnoredWhenHydratedLacksIt(t *testing.T) {
	srv, _, projectDir := newTestServerWithHarnessConfig(t, "claude",
		"harness: claude\nimage: test-image\nuser: scion\nauth_selected_type: vertex-ai\nenv:\n  GOOGLE_CLOUD_PROJECT: p1\n  GOOGLE_CLOUD_LOCATION: us-east5\n"+claudeAuthBlock,
		`
schema_version: "1"
profiles:
  default:
    runtime: mock
`)
	hydratedDir := filepath.Join(t.TempDir(), "claude")
	writeHarnessConfigDirAt(t, hydratedDir,
		"harness: claude\nimage: test-image\nuser: scion\nauth_selected_type: vertex-ai\n"+claudeAuthBlock)

	var req CreateAgentRequest
	req.ProjectPath = projectDir
	req.Config = &CreateAgentConfig{HarnessConfig: "claude", Profile: "default"}
	required, _, _, _ := srv.extractRequiredEnvKeys(req, "", hydratedDir)
	found := false
	for _, k := range required {
		if k == "GOOGLE_CLOUD_PROJECT" {
			found = true
		}
	}
	if !found {
		t.Errorf("on-disk env was counted although launch uses the hydrated dir, which lacks it; required=%v", required)
	}
}

// TestEnvGather_AutoDetect_SeesSettingsEnvNotDirEnv confirms that auto-detect
// (when auth_selected_type is unset) sees the resolved settings env — the
// same as launch's autoDetectAuthSelectedType, which runs after
// resolveAuthEnvOverlay has filled opts.Env from settings — but does not see
// the harness-config directory's env, which never reaches opts.Env.
func TestEnvGather_AutoDetect_SeesSettingsEnvNotDirEnv(t *testing.T) {
	srv, _, projectDir := newTestServerWithHarnessConfig(t, "claude",
		"harness: claude\nimage: test-image\nuser: scion\n"+claudeAuthBlock,
		`
schema_version: "1"
harness_configs:
  claude:
    harness: claude
    env:
      ANTHROPIC_API_KEY: "sk-test"
profiles:
  default:
    runtime: mock
`)

	body := `{
		"name": "test-agent-autodetect-settings-env",
		"id": "agent-uuid-autodetect-settings-env",
		"gatherEnv": true,
		"projectPath": "` + projectDir + `",
		"config": {
			"template": "claude",
			"profile": "default",
			"gcpIdentity": {"metadata_mode": "passthrough"}
		}
	}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/agents", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	srv.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusCreated {
		t.Fatalf("expected 201 (auto-detect should pick api-key from settings env, needing no GCP keys), got %d: %s", w.Code, w.Body.String())
	}
}

// TestEnvGather_OuterCheck_SettingsEnvSatisfiesNonAuthRequiredKey exercises
// the outer needs/hubHas fold-in directly: a key that Phase 2 marks
// required because some OTHER harness_configs entry declares it with
// an empty value (extractRequiredEnvKeys walks every harness_configs entry,
// not just the selected one) must still be satisfied when the SELECTED
// harness config's own resolved settings env supplies a non-empty value —
// a path the auth-key-group tests above never exercise, since that key is
// not part of any auth key group.
func TestEnvGather_OuterCheck_SettingsEnvSatisfiesNonAuthRequiredKey(t *testing.T) {
	srv, _, projectDir := newTestServerWithHarnessConfig(t, "claude",
		"harness: claude\nimage: test-image\nuser: scion\n",
		`
schema_version: "1"
harness_configs:
  claude:
    harness: claude
    env:
      CUSTOM_ENV_KEY: "provided-value"
  other:
    harness: claude
    env:
      CUSTOM_ENV_KEY: ""
profiles:
  default:
    runtime: mock
`)

	body := `{
		"name": "test-agent-outer-phase2-satisfied",
		"id": "agent-uuid-outer-phase2-satisfied",
		"gatherEnv": true,
		"projectPath": "` + projectDir + `",
		"resolvedEnv": {"ANTHROPIC_API_KEY": "sk-test"},
		"config": {"template": "claude", "profile": "default"}
	}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/agents", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	srv.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusCreated {
		t.Fatalf("expected 201 (CUSTOM_ENV_KEY should be satisfied by the selected harness config's own resolved settings env), got %d: %s", w.Code, w.Body.String())
	}
}

// The tests below cover the harness-config directory's own ${VAR}
// expansion, and the preflight's search of template-bundled harness-config
// dirs (matching launch's resolveHarnessConfigDir).

// TestEnvGather_HarnessConfigDirEnv_UnsetVarIsMissing confirms that a dir
// env value referencing an unset variable is dropped, matching
// buildAgentEnv (pkg/agent/run.go), which also drops it.
func TestEnvGather_HarnessConfigDirEnv_UnsetVarIsMissing(t *testing.T) {
	_ = os.Unsetenv("DIR_ENV_UNSET_VAR")
	srv, _, projectDir := newTestServerWithHarnessConfig(t, "claude",
		"harness: claude\nimage: test-image\nuser: scion\nauth_selected_type: vertex-ai\n"+
			"env:\n  GOOGLE_CLOUD_PROJECT: \"${DIR_ENV_UNSET_VAR}\"\n  GOOGLE_CLOUD_LOCATION: us-east5\n"+claudeAuthBlock,
		`
schema_version: "1"
harness_configs:
  claude:
    harness: claude
profiles:
  default:
    runtime: mock
`)

	body := `{
		"name": "test-agent-dirvar-unset",
		"id": "agent-uuid-dirvar-unset",
		"gatherEnv": true,
		"projectPath": "` + projectDir + `",
		"config": {"template": "claude", "profile": "default", "gcpIdentity": {"metadata_mode": "passthrough"}}
	}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/agents", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusAccepted {
		t.Fatalf("expected 202 (${DIR_ENV_UNSET_VAR} is unset; buildAgentEnv would drop it), got %d: %s", w.Code, w.Body.String())
	}
	var envReqs EnvRequirementsResponse
	if err := json.Unmarshal(w.Body.Bytes(), &envReqs); err != nil {
		t.Fatal("failed to decode response:", err)
	}
	found := false
	for _, k := range envReqs.Needs {
		if k == "GOOGLE_CLOUD_PROJECT" {
			found = true
		}
	}
	if !found {
		t.Errorf("expected GOOGLE_CLOUD_PROJECT in needs, got %v", envReqs.Needs)
	}
}

// TestEnvGather_HarnessConfigDirEnv_SetVarIsSatisfied confirms a dir env
// value referencing a set variable expands and satisfies the requirement.
func TestEnvGather_HarnessConfigDirEnv_SetVarIsSatisfied(t *testing.T) {
	t.Setenv("DIR_ENV_SET_VAR", "proj-x")
	srv, _, projectDir := newTestServerWithHarnessConfig(t, "claude",
		"harness: claude\nimage: test-image\nuser: scion\nauth_selected_type: vertex-ai\n"+
			"env:\n  GOOGLE_CLOUD_PROJECT: \"${DIR_ENV_SET_VAR}\"\n  GOOGLE_CLOUD_LOCATION: us-east5\n"+claudeAuthBlock,
		`
schema_version: "1"
harness_configs:
  claude:
    harness: claude
profiles:
  default:
    runtime: mock
`)

	body := `{
		"name": "test-agent-dirvar-set",
		"id": "agent-uuid-dirvar-set",
		"gatherEnv": true,
		"projectPath": "` + projectDir + `",
		"config": {"template": "claude", "profile": "default", "gcpIdentity": {"metadata_mode": "passthrough"}}
	}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/agents", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusCreated {
		t.Fatalf("expected 201 (${DIR_ENV_SET_VAR} expands to a non-empty value), got %d: %s", w.Code, w.Body.String())
	}
}

// TestEnvGather_HarnessConfigDirEnv_EmptyValueHostPassthrough confirms a
// literal empty dir env value falls back to the broker process's own env,
// matching buildAgentEnv's host-env passthrough.
func TestEnvGather_HarnessConfigDirEnv_EmptyValueHostPassthrough(t *testing.T) {
	unsetHostGCPEnv(t)
	t.Setenv("GOOGLE_CLOUD_PROJECT", "host-proj")
	srv, _, projectDir := newTestServerWithHarnessConfig(t, "claude",
		"harness: claude\nimage: test-image\nuser: scion\nauth_selected_type: vertex-ai\n"+
			"env:\n  GOOGLE_CLOUD_PROJECT: \"\"\n  GOOGLE_CLOUD_LOCATION: us-east5\n"+claudeAuthBlock,
		`
schema_version: "1"
harness_configs:
  claude:
    harness: claude
profiles:
  default:
    runtime: mock
`)

	body := `{
		"name": "test-agent-dirvar-hostpass",
		"id": "agent-uuid-dirvar-hostpass",
		"gatherEnv": true,
		"projectPath": "` + projectDir + `",
		"config": {"template": "claude", "profile": "default", "gcpIdentity": {"metadata_mode": "passthrough"}}
	}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/agents", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusCreated {
		t.Fatalf("expected 201 (empty dir value plus broker process env set means host passthrough), got %d: %s", w.Code, w.Body.String())
	}
}

// TestEnvGather_TemplateBundledHarnessConfigDirEnv confirms the preflight
// searches a template-bundled harness-config dir before the project/global
// one of the same name, matching config.FindHarnessConfigDir's own
// precedence (checked by launch's resolveHarnessConfigDir,
// pkg/agent/provision.go, via the same template chain).
func TestEnvGather_TemplateBundledHarnessConfigDirEnv(t *testing.T) {
	srv, _, projectDir := newTestServerWithHarnessConfig(t, "claude",
		"harness: claude\nimage: test-image\nuser: scion\nauth_selected_type: vertex-ai\n"+claudeAuthBlock,
		`
schema_version: "1"
harness_configs:
  claude:
    harness: claude
profiles:
  default:
    runtime: mock
`)
	tplHC := filepath.Join(projectDir, "templates", "mytpl", "harness-configs", "claude")
	if err := os.MkdirAll(tplHC, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(projectDir, "templates", "mytpl", "scion-agent.yaml"), []byte("harness_config: claude\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(tplHC, "config.yaml"),
		[]byte("harness: claude\nimage: test-image\nuser: scion\nauth_selected_type: vertex-ai\nenv:\n  GOOGLE_CLOUD_PROJECT: tpl-proj\n  GOOGLE_CLOUD_LOCATION: us-east5\n"+claudeAuthBlock),
		0o644); err != nil {
		t.Fatal(err)
	}

	body := `{
		"name": "test-agent-template-hc-dir",
		"id": "agent-uuid-template-hc-dir",
		"gatherEnv": true,
		"projectPath": "` + projectDir + `",
		"config": {"template": "mytpl", "harnessConfig": "claude", "profile": "default", "gcpIdentity": {"metadata_mode": "passthrough"}}
	}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/agents", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusCreated {
		t.Fatalf("expected 201 (template-bundled harness-config env, what launch uses, should satisfy GOOGLE_CLOUD_PROJECT/LOCATION), got %d: %s", w.Code, w.Body.String())
	}
}

// TestEnvGather_HarnessDeclaredAuthKey_EmptySettingsEnvFallsThroughToHarnessConfigDir
// covers authCandidateEnvKeys' harness-declared half (every required_env
// name across authMeta.Types, not just the six GCP shared names).
// CLAUDE_CODE_OAUTH_TOKEN is declared under the harness's "oauth-token"
// auth type, not the selected "vertex-ai" one, so it is required only via
// Phase 2 (settings declares it with an empty value) and checked only by
// the outer needs/hubHas path — not by the auth-key-group loop, which only
// ever sees the selected auth type's own groups. Settings' empty value
// must still fall through to the harness-config directory's value, exactly
// as for a GCP-shared key.
func TestEnvGather_HarnessDeclaredAuthKey_EmptySettingsEnvFallsThroughToHarnessConfigDir(t *testing.T) {
	unsetHostGCPEnv(t)
	_ = os.Unsetenv("CLAUDE_CODE_OAUTH_TOKEN")
	srv, _, projectDir := newTestServerWithHarnessConfig(t, "claude",
		"harness: claude\nimage: test-image\nuser: scion\nauth_selected_type: vertex-ai\n"+
			"env:\n  GOOGLE_CLOUD_PROJECT: p1\n  GOOGLE_CLOUD_LOCATION: us-east5\n  CLAUDE_CODE_OAUTH_TOKEN: dir-tok\n"+
			strings.Replace(claudeAuthBlock, "    vertex-ai:\n",
				"    oauth-token:\n      required_env:\n        - any_of: [\"CLAUDE_CODE_OAUTH_TOKEN\"]\n    vertex-ai:\n", 1),
		`
schema_version: "1"
harness_configs:
  claude:
    harness: claude
    env:
      CLAUDE_CODE_OAUTH_TOKEN: ""
profiles:
  default:
    runtime: mock
`)

	body := `{
		"name": "test-agent-harness-declared-fallthrough",
		"id": "agent-uuid-harness-declared-fallthrough",
		"gatherEnv": true,
		"projectPath": "` + projectDir + `",
		"config": {"template": "claude", "profile": "default", "gcpIdentity": {"metadata_mode": "passthrough"}}
	}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/agents", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusCreated {
		t.Fatalf("expected 201 (CLAUDE_CODE_OAUTH_TOKEN, a harness-declared auth key, should fall through to the harness-config dir's value), got %d: %s", w.Code, w.Body.String())
	}
}

// TestEnvGather_HydratedTemplate_PreferredOverStaleLocalTemplate covers:
// for a hub-dispatched agent (TemplateID set), launch's own dispatch path
// (start_context.go) hydrates the template and resolves the harness-config
// chain from that hydrated local path, not from a same-named template slug
// found on the broker's local disk. The preflight hydrates the
// template the same way (createAgent, next to the harness-config
// hydration) and uses the hydrated path in place of the slug — so a stale
// local template of the same name, which launch will never actually use,
// must be ignored in favor of the hydrated one.
func TestEnvGather_HydratedTemplate_PreferredOverStaleLocalTemplate(t *testing.T) {
	srv, _, projectDir := newTestServerWithHarnessConfig(t, "claude",
		"harness: claude\nimage: test-image\nuser: scion\nauth_selected_type: vertex-ai\n"+claudeAuthBlock,
		`
schema_version: "1"
profiles:
  default:
    runtime: mock
`)

	// A stale local template of the same slug, bundling a harness-config
	// without the needed env. Launch would never read this one for a
	// hub-dispatched agent — it hydrates the template instead.
	staleTplHC := filepath.Join(projectDir, "templates", "mytpl", "harness-configs", "claude")
	if err := os.MkdirAll(staleTplHC, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(projectDir, "templates", "mytpl", "scion-agent.yaml"), []byte("harness_config: claude\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(staleTplHC, "config.yaml"),
		[]byte("harness: claude\nimage: test-image\nuser: scion\nauth_selected_type: vertex-ai\n"+claudeAuthBlock),
		0o644); err != nil {
		t.Fatal(err)
	}

	// The hydrated (hub-managed) template of the SAME slug, in a separate
	// local-storage backend, bundling the correct env — matching how a
	// co-located Hub resolves a template hydration request (see
	// TestHydrateTemplate_LocalStorageDirectRead in hub_connection_test.go).
	stor, err := storage.NewLocal(storage.Config{
		Provider:  storage.ProviderLocal,
		LocalPath: t.TempDir(),
		Bucket:    "local",
	})
	if err != nil {
		t.Fatalf("NewLocal: %v", err)
	}
	hydratedDir := stor.ObjectFSPath(storage.TemplateStoragePath("", "global", "", "mytpl"))
	hydratedHC := filepath.Join(hydratedDir, "harness-configs", "claude")
	if err := os.MkdirAll(hydratedHC, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(hydratedDir, "scion-agent.yaml"), []byte("harness_config: claude\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(hydratedHC, "config.yaml"),
		[]byte("harness: claude\nimage: test-image\nuser: scion\nauth_selected_type: vertex-ai\nenv:\n  GOOGLE_CLOUD_PROJECT: hydrated-proj\n  GOOGLE_CLOUD_LOCATION: us-east5\n"+claudeAuthBlock),
		0o644); err != nil {
		t.Fatal(err)
	}

	conn := &HubConnection{
		Name:         "hub-1",
		IsColocated:  true,
		LocalStorage: stor,
		HubClient: &stubHubClient{templates: &stubTemplateService{
			getFunc: func(ctx context.Context, ref string) (*hubclient.Template, error) {
				return &hubclient.Template{ID: "tpl-uuid", Slug: "mytpl", Scope: "global"}, nil
			},
		}},
	}
	cfg := &CreateAgentConfig{Template: "mytpl", TemplateID: "tpl-uuid", HarnessConfig: "claude", Profile: "default"}

	// This is exactly what createAgent's env-gather preflight does before
	// calling extractRequiredEnvKeys (handlers.go, next to the
	// harness-config hydration): hydrate the template and pass the
	// resulting path through in place of the slug.
	hydratedPath, err := srv.hydrateTemplate(context.Background(), cfg, conn)
	if err != nil {
		t.Fatalf("hydrateTemplate failed: %v", err)
	}
	if hydratedPath != hydratedDir {
		t.Fatalf("expected hydrated path %q, got %q", hydratedDir, hydratedPath)
	}

	var req CreateAgentRequest
	req.ProjectPath = projectDir
	req.Config = cfg
	required, _, _, _ := srv.extractRequiredEnvKeys(req, hydratedPath)
	for _, k := range required {
		if k == "GOOGLE_CLOUD_PROJECT" {
			t.Errorf("hydrated template's bundled harness-config env (what launch uses) was ignored: GOOGLE_CLOUD_PROJECT reported missing, required=%v", required)
		}
	}
}

// newTemplateHydrationFailureServer sets up a server with a hub connection
// whose template metadata lookup fails with getErr, for the
// TestEnvGather_TemplateHydrationFailure_* tests below.
func newTemplateHydrationFailureServer(t *testing.T, getErr error) (*Server, string) {
	t.Helper()
	srv, _, projectDir := newTestServerWithHarnessConfig(t, "claude",
		"harness: claude\nimage: test-image\nuser: scion\nauth_selected_type: vertex-ai\n"+claudeAuthBlock,
		`
schema_version: "1"
profiles:
  default:
    runtime: mock
`)

	stor, err := storage.NewLocal(storage.Config{
		Provider:  storage.ProviderLocal,
		LocalPath: t.TempDir(),
		Bucket:    "local",
	})
	if err != nil {
		t.Fatalf("NewLocal: %v", err)
	}

	srv.hubMu.Lock()
	srv.hubConnections["hub-1"] = &HubConnection{
		Name:         "hub-1",
		IsColocated:  true,
		LocalStorage: stor,
		HubClient: &stubHubClient{templates: &stubTemplateService{
			getFunc: func(ctx context.Context, ref string) (*hubclient.Template, error) {
				return nil, getErr
			},
		}},
		Hydrator: newTestHydrator(t),
	}
	srv.hubMu.Unlock()
	return srv, projectDir
}

func postTemplateHydrationFailure(t *testing.T, srv *Server, projectDir string) *httptest.ResponseRecorder {
	t.Helper()
	body := `{
		"name": "test-agent-template-hydration-fails",
		"id": "agent-uuid-template-hydration-fails",
		"gatherEnv": true,
		"projectPath": "` + projectDir + `",
		"config": {
			"template": "mytpl",
			"templateId": "tpl-uuid",
			"harnessConfig": "claude",
			"profile": "default",
			"gcpIdentity": {"metadata_mode": "passthrough"}
		}
	}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/agents", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Scion-Hub-Connection", "hub-1")
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)
	return w
}

// TestEnvGather_TemplateHydrationFailure_FailsPreflight confirms the
// preflight does not silently fall back to the on-disk template slug when
// hub template hydration errors: launch returns the same startContextError
// mapping on a hydrateTemplate error (writeStartContextError — 500
// template_error here, since this error is not a Hub-connectivity one), so a
// graceful fallback here would make the preflight more lenient than launch —
// scoring a slug launch would never actually reach, and possibly reporting
// success for an agent whose real dispatch is about to fail.
func TestEnvGather_TemplateHydrationFailure_FailsPreflight(t *testing.T) {
	srv, projectDir := newTemplateHydrationFailureServer(t, fmt.Errorf("boom"))
	w := postTemplateHydrationFailure(t, srv, projectDir)

	var resp ErrorResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("failed to decode response: %v: %s", err, w.Body.String())
	}
	if w.Code != http.StatusInternalServerError || resp.Error.Code != ErrCodeTemplateError {
		t.Fatalf("expected 500 %s (matching launch's non-connectivity hydration error mapping), got %d %s: %s",
			ErrCodeTemplateError, w.Code, resp.Error.Code, w.Body.String())
	}
}

// TestEnvGather_TemplateHydrationConnectivityFailure_MatchesLaunch confirms
// that a Hub-connectivity hydration error (e.g. the Hub is temporarily
// unreachable) maps to the same retryable 503 hub_unreachable launch uses
// (writeStartContextError), not a generic 500 — a transient outage should
// not look identical to a real template error to a caller deciding whether
// to retry.
func TestEnvGather_TemplateHydrationConnectivityFailure_MatchesLaunch(t *testing.T) {
	srv, projectDir := newTemplateHydrationFailureServer(t, fmt.Errorf("dial tcp 10.0.0.1:443: connection refused"))
	w := postTemplateHydrationFailure(t, srv, projectDir)

	var resp ErrorResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("failed to decode response: %v: %s", err, w.Body.String())
	}
	if w.Code != http.StatusServiceUnavailable || resp.Error.Code != ErrCodeHubUnreachable {
		t.Fatalf("expected 503 %s (matching launch's Hub-connectivity hydration error mapping), got %d %s: %s",
			ErrCodeHubUnreachable, w.Code, resp.Error.Code, w.Body.String())
	}
}

// TestEnvGather_CreateAgent_ScoresHydratedTemplate complements
// TestEnvGather_HydratedTemplate_PreferredOverStaleLocalTemplate, which
// calls hydrateTemplate and extractRequiredEnvKeys directly — exercising the
// same logic createAgent's own preflight wiring uses, but not that wiring
// itself. This one goes through the full HTTP handler with a real hub
// connection, the same way TestEnvGather_TemplateHydrationFailure_*
// already does for the error path, so createAgent's own use of the hydrated
// path (not just extractRequiredEnvKeys accepting one) is covered end to
// end. The region key is deliberately left unset in both the stale and
// hydrated harness-configs, so the request always ends in 202 and never
// reaches full dispatch: past buildStartContext, createAgent's
// attachSkillResolver calls hubclient.Client.Skills(), which stubHubClient
// does not implement (nil embedded interface), so it would panic.
// The needs list shows which harness-config directory the preflight
// actually scored.
func TestEnvGather_CreateAgent_ScoresHydratedTemplate(t *testing.T) {
	unsetHostGCPEnv(t)
	hc := "harness: claude\nimage: test-image\nuser: scion\nauth_selected_type: vertex-ai\n" + claudeAuthBlock
	srv, _, projectDir := newTestServerWithHarnessConfig(t, "claude", hc,
		"schema_version: \"1\"\nprofiles:\n  default:\n    runtime: mock\n")

	// Stale local template "mytpl": bundled harness-config has no env.
	stale := filepath.Join(projectDir, "templates", "mytpl")
	writeHarnessConfigDirAt(t, filepath.Join(stale, "harness-configs", "claude"), hc)
	if err := os.WriteFile(filepath.Join(stale, "scion-agent.yaml"), []byte("harness_config: claude\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	// Hydrated (hub-managed) template of the same slug: bundled
	// harness-config sets GOOGLE_CLOUD_PROJECT only.
	stor, err := storage.NewLocal(storage.Config{Provider: storage.ProviderLocal, LocalPath: t.TempDir(), Bucket: "local"})
	if err != nil {
		t.Fatalf("NewLocal: %v", err)
	}
	hydratedDir := stor.ObjectFSPath(storage.TemplateStoragePath("", "global", "", "mytpl"))
	writeHarnessConfigDirAt(t, filepath.Join(hydratedDir, "harness-configs", "claude"),
		"harness: claude\nimage: test-image\nuser: scion\nauth_selected_type: vertex-ai\nenv:\n  GOOGLE_CLOUD_PROJECT: hydrated-proj\n"+claudeAuthBlock)
	if err := os.WriteFile(filepath.Join(hydratedDir, "scion-agent.yaml"), []byte("harness_config: claude\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	srv.hubMu.Lock()
	srv.hubConnections["hub-1"] = &HubConnection{
		Name:         "hub-1",
		IsColocated:  true,
		LocalStorage: stor,
		HubClient: &stubHubClient{templates: &stubTemplateService{
			getFunc: func(ctx context.Context, ref string) (*hubclient.Template, error) {
				return &hubclient.Template{ID: "tpl-uuid", Slug: "mytpl", Scope: "global"}, nil
			},
		}},
		Hydrator: newTestHydrator(t),
	}
	srv.hubMu.Unlock()

	body := `{
		"name": "test-agent-createagent-hydrated",
		"id": "agent-uuid-createagent-hydrated",
		"gatherEnv": true,
		"projectPath": "` + projectDir + `",
		"config": {
			"template": "mytpl",
			"templateId": "tpl-uuid",
			"harnessConfig": "claude",
			"profile": "default",
			"gcpIdentity": {"metadata_mode": "passthrough"}
		}
	}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/agents", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Scion-Hub-Connection", "hub-1")
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusAccepted {
		t.Fatalf("expected 202 (region key stays missing, so the request never reaches full dispatch), got %d: %s", w.Code, w.Body.String())
	}
	var resp EnvRequirementsResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	for _, k := range resp.Needs {
		if k == "GOOGLE_CLOUD_PROJECT" {
			t.Errorf("createAgent's own preflight wiring scored the stale local template, not the hydrated one: needs=%v", resp.Needs)
		}
	}
}

// TestEnvGather_AuthCandidateKeyValue_UsesExpandedDirEnvKey covers:
// authCandidateKeyValue must consult the same expanded dir-env view
// fillAbsentDirEnv uses (expandDirEnv), not look the directory up by a raw,
// unexpanded key. A dir entry whose KEY is itself a ${VAR} reference that
// expands to an auth-candidate name (here GOOGLE_CLOUD_PROJECT) reaches the
// container under that expanded name — buildAgentEnv (pkg/agent/run.go)
// expands dir-env keys too — so the preflight must find it. Looking the
// directory up by the literal, unexpanded key would report a value the pod
// actually receives as missing (false-missing).
func TestEnvGather_AuthCandidateKeyValue_UsesExpandedDirEnvKey(t *testing.T) {
	t.Setenv("DIR_ENV_KEY_NAME", "GOOGLE_CLOUD_PROJECT")
	srv, _, projectDir := newTestServerWithHarnessConfig(t, "claude",
		"harness: claude\nimage: test-image\nuser: scion\nauth_selected_type: vertex-ai\n"+
			"env:\n  \"${DIR_ENV_KEY_NAME}\": dir-proj\n  GOOGLE_CLOUD_LOCATION: us-east5\n"+claudeAuthBlock,
		`
schema_version: "1"
harness_configs:
  claude:
    harness: claude
profiles:
  default:
    runtime: mock
`)

	body := `{
		"name": "test-agent-dirkey-expand",
		"id": "agent-uuid-dirkey-expand",
		"gatherEnv": true,
		"projectPath": "` + projectDir + `",
		"config": {"template": "claude", "profile": "default", "gcpIdentity": {"metadata_mode": "passthrough"}}
	}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/agents", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusCreated {
		t.Fatalf("expected 201 (a dir-env key that expands to GOOGLE_CLOUD_PROJECT should satisfy it, matching buildAgentEnv's own key expansion), got %d: %s", w.Code, w.Body.String())
	}
}
