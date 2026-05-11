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
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/runtime"
)

// newTestServerWithProjectPath creates a test server with a temporary grove path
// that has versioned settings with declared env vars.
func newTestServerWithProjectPath(t *testing.T, settingsYAML string) (*Server, *envCapturingManager, string) {
	t.Helper()

	// Isolate HOME so LoadEffectiveSettings does not merge the developer's
	// personal ~/.scion/settings.yaml (which may declare harness-config
	// auth_selected_type values that would override the test fixture).
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", "")

	// Create temp grove directory with settings
	// LoadEffectiveSettings expects a dir that contains settings.yaml directly
	groveDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(groveDir, "settings.yaml"), []byte(settingsYAML), 0644); err != nil {
		t.Fatal(err)
	}

	// Create template directories so FindTemplateInProjectPath can resolve them.
	// Each template needs a scion-agent.yaml that sets harness_config so that
	// provisioning doesn't fall back to the embedded default (gemini).
	for _, tpl := range []string{"claude", "gemini", "default"} {
		tplDir := filepath.Join(groveDir, "templates", tpl)
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
		hcDir := filepath.Join(groveDir, "harness-configs", hc)
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

	return New(cfg, mgr, rt), mgr, groveDir
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
	srv, mgr, groveDir := newTestServerWithProjectPath(t, settings)

	body := `{
		"name": "test-agent",
		"id": "agent-uuid-123",
		"gatherEnv": true,
		"grovePath": "` + groveDir + `",
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
	srv, _, groveDir := newTestServerWithProjectPath(t, settings)

	body := `{
		"name": "test-agent-gather",
		"id": "agent-uuid-456",
		"gatherEnv": true,
		"grovePath": "` + groveDir + `",
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
	srv, _, groveDir := newTestServerWithProjectPath(t, settings)

	// Set the keys in the broker's own environment — these should NOT be used
	t.Setenv("BROKER_LOCAL_KEY", "broker-value")
	t.Setenv("ANTHROPIC_API_KEY", "broker-anthropic-key")

	body := `{
		"name": "test-agent-broker-env",
		"id": "agent-uuid-789",
		"gatherEnv": true,
		"grovePath": "` + groveDir + `",
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

// TestEnvGather_FinalizeEnv tests the finalize-env endpoint: after receiving
// a 202, the caller submits gathered env and the agent starts.
func TestEnvGather_FinalizeEnv(t *testing.T) {
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
	srv, mgr, groveDir := newTestServerWithProjectPath(t, settings)

	// Phase 1: Create agent with gather — should get 202
	createBody := `{
		"name": "test-agent-finalize",
		"id": "agent-uuid-fin",
		"gatherEnv": true,
		"grovePath": "` + groveDir + `",
		"config": {"template": "claude", "profile": "default"}
	}`
	createReq := httptest.NewRequest(http.MethodPost, "/api/v1/agents", strings.NewReader(createBody))
	createReq.Header.Set("Content-Type", "application/json")
	createW := httptest.NewRecorder()

	srv.Handler().ServeHTTP(createW, createReq)

	if createW.Code != http.StatusAccepted {
		t.Fatalf("phase 1: expected 202, got %d: %s", createW.Code, createW.Body.String())
	}

	// Phase 2: Submit gathered env via finalize-env
	finalizeBody := `{"env": {"NEEDED_KEY": "gathered-value"}}`
	finalizeReq := httptest.NewRequest(http.MethodPost, "/api/v1/agents/agent-uuid-fin/finalize-env", strings.NewReader(finalizeBody))
	finalizeReq.Header.Set("Content-Type", "application/json")
	finalizeW := httptest.NewRecorder()

	srv.Handler().ServeHTTP(finalizeW, finalizeReq)

	if finalizeW.Code != http.StatusCreated {
		t.Fatalf("phase 2: expected 201, got %d: %s", finalizeW.Code, finalizeW.Body.String())
	}

	// Verify agent was started with the gathered key
	if mgr.lastEnv == nil {
		t.Fatal("expected env to be set after finalize")
	}
	if mgr.lastEnv["NEEDED_KEY"] != "gathered-value" {
		t.Errorf("expected NEEDED_KEY='gathered-value', got %q", mgr.lastEnv["NEEDED_KEY"])
	}

	// Verify template slug was preserved through finalize-env (regression test:
	// without this, container image resolution fails with "no container image resolved")
	if mgr.lastTemplateName != "claude" {
		t.Errorf("expected TemplateName='claude', got %q", mgr.lastTemplateName)
	}
}

func TestEnvGather_FinalizeEnv_RetryOnStartFailure(t *testing.T) {
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
	srv, mgr, groveDir := newTestServerWithProjectPath(t, settings)
	mgr.startErr = os.ErrPermission

	createBody := `{
		"name": "test-agent-retry",
		"id": "agent-uuid-retry",
		"gatherEnv": true,
		"grovePath": "` + groveDir + `",
		"config": {"template": "claude", "profile": "default"}
	}`
	createReq := httptest.NewRequest(http.MethodPost, "/api/v1/agents", strings.NewReader(createBody))
	createReq.Header.Set("Content-Type", "application/json")
	createW := httptest.NewRecorder()
	srv.Handler().ServeHTTP(createW, createReq)
	if createW.Code != http.StatusAccepted {
		t.Fatalf("phase 1: expected 202, got %d: %s", createW.Code, createW.Body.String())
	}

	finalizeBody := `{"env": {"NEEDED_KEY": "gathered-value"}}`
	finalizeReq := httptest.NewRequest(http.MethodPost, "/api/v1/agents/agent-uuid-retry/finalize-env", strings.NewReader(finalizeBody))
	finalizeReq.Header.Set("Content-Type", "application/json")
	finalizeW := httptest.NewRecorder()
	srv.Handler().ServeHTTP(finalizeW, finalizeReq)
	if finalizeW.Code != http.StatusInternalServerError {
		t.Fatalf("first finalize: expected 500, got %d: %s", finalizeW.Code, finalizeW.Body.String())
	}

	mgr.startErr = nil
	retryReq := httptest.NewRequest(http.MethodPost, "/api/v1/agents/agent-uuid-retry/finalize-env", strings.NewReader(finalizeBody))
	retryReq.Header.Set("Content-Type", "application/json")
	retryW := httptest.NewRecorder()
	srv.Handler().ServeHTTP(retryW, retryReq)
	if retryW.Code != http.StatusCreated {
		t.Fatalf("retry finalize: expected 201, got %d: %s", retryW.Code, retryW.Body.String())
	}
}

func TestEnvGather_FinalizeEnv_SurvivesBrokerRestart(t *testing.T) {
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
	groveDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(groveDir, "settings.yaml"), []byte(settings), 0644); err != nil {
		t.Fatal(err)
	}
	for _, tpl := range []string{"claude", "default"} {
		tplDir := filepath.Join(groveDir, "templates", tpl)
		if err := os.MkdirAll(tplDir, 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(tplDir, "scion-agent.yaml"), []byte("harness_config: claude\n"), 0644); err != nil {
			t.Fatal(err)
		}
	}
	hcDir := filepath.Join(groveDir, "harness-configs", "claude")
	if err := os.MkdirAll(hcDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(hcDir, "config.yaml"), []byte("harness: claude\nimage: test-image:claude\n"), 0644); err != nil {
		t.Fatal(err)
	}

	stateDir := t.TempDir()
	cfg := DefaultServerConfig()
	cfg.BrokerID = "test-broker-id"
	cfg.BrokerName = "test-host"
	cfg.Debug = true
	cfg.StateDir = stateDir
	cfg.ForceRuntime = "mock"

	createMgr := &envCapturingManager{}
	srv1 := New(cfg, createMgr, &runtime.MockRuntime{NameFunc: func() string { return "docker" }})

	createBody := `{
		"name": "test-agent-restart",
		"id": "agent-uuid-restart",
		"gatherEnv": true,
		"grovePath": "` + groveDir + `",
		"config": {"template": "claude", "profile": "default"}
	}`
	createReq := httptest.NewRequest(http.MethodPost, "/api/v1/agents", strings.NewReader(createBody))
	createReq.Header.Set("Content-Type", "application/json")
	createW := httptest.NewRecorder()
	srv1.Handler().ServeHTTP(createW, createReq)
	if createW.Code != http.StatusAccepted {
		t.Fatalf("phase 1: expected 202, got %d: %s", createW.Code, createW.Body.String())
	}

	finalizeMgr := &envCapturingManager{}
	srv2 := New(cfg, finalizeMgr, &runtime.MockRuntime{NameFunc: func() string { return "docker" }})

	finalizeBody := `{"env": {"NEEDED_KEY": "gathered-value"}}`
	finalizeReq := httptest.NewRequest(http.MethodPost, "/api/v1/agents/agent-uuid-restart/finalize-env", strings.NewReader(finalizeBody))
	finalizeReq.Header.Set("Content-Type", "application/json")
	finalizeW := httptest.NewRecorder()
	srv2.Handler().ServeHTTP(finalizeW, finalizeReq)
	if finalizeW.Code != http.StatusCreated {
		t.Fatalf("phase 2: expected 201, got %d: %s", finalizeW.Code, finalizeW.Body.String())
	}
	if finalizeMgr.lastEnv["NEEDED_KEY"] != "gathered-value" {
		t.Fatalf("expected gathered value to survive restart, got %q", finalizeMgr.lastEnv["NEEDED_KEY"])
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
	groveDir := t.TempDir()
	settingsYAML := `schema_version: "1"
active_profile: local
profiles:
    local:
        runtime: mock
runtimes:
    mock:
        type: mock
`
	if err := os.WriteFile(filepath.Join(groveDir, "settings.yaml"), []byte(settingsYAML), 0644); err != nil {
		t.Fatal(err)
	}

	// Create templates with scion-agent.yaml so harness-config resolution
	// finds a harness_config value instead of falling through to the
	// embedded default ("gemini") which has no on-disk directory.
	tplDir := filepath.Join(groveDir, "templates", "claude")
	if err := os.MkdirAll(tplDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(tplDir, "scion-agent.yaml"), []byte("harness_config: claude\n"), 0644); err != nil {
		t.Fatal(err)
	}

	// Create harness-config directory so FindHarnessConfigDir can resolve it.
	hcDir := filepath.Join(groveDir, "harness-configs", "claude")
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
		"grovePath": %q,
		"config": {"template": "claude"}
	}`, groveDir)
	req1 := httptest.NewRequest(http.MethodPost, "/api/v1/agents", strings.NewReader(body))
	req1.Header.Set("Content-Type", "application/json")
	w1 := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w1, req1)
	if w1.Code != http.StatusCreated {
		t.Fatalf("first create: expected 201, got %d: %s", w1.Code, w1.Body.String())
	}
	if mgr.startCalls != 1 {
		t.Fatalf("first create: expected startCalls=1, got %d", mgr.startCalls)
	}

	req2 := httptest.NewRequest(http.MethodPost, "/api/v1/agents", strings.NewReader(body))
	req2.Header.Set("Content-Type", "application/json")
	w2 := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w2, req2)
	if w2.Code != http.StatusCreated {
		t.Fatalf("second create: expected 201 replay, got %d: %s", w2.Code, w2.Body.String())
	}
	if mgr.startCalls != 1 {
		t.Fatalf("second create should replay without starting again, startCalls=%d", mgr.startCalls)
	}
}

// newTestServerWithHarnessConfig creates a test server with a temporary grove path
// that has a harness-config directory and optional settings YAML.
func newTestServerWithHarnessConfig(t *testing.T, harnessConfigName, configYAML, settingsYAML string) (*Server, *envCapturingManager, string) {
	t.Helper()

	// Isolate HOME so LoadEffectiveSettings does not merge the developer's
	// personal ~/.scion/settings.yaml (which may declare harness-config
	// auth_selected_type values that would override the test fixture).
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", "")

	groveDir := t.TempDir()

	// Create harness-configs/<name>/config.yaml
	hcDir := filepath.Join(groveDir, "harness-configs", harnessConfigName)
	if err := os.MkdirAll(hcDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(hcDir, "config.yaml"), []byte(configYAML), 0644); err != nil {
		t.Fatal(err)
	}

	// Write settings.yaml if provided
	if settingsYAML != "" {
		if err := os.WriteFile(filepath.Join(groveDir, "settings.yaml"), []byte(settingsYAML), 0644); err != nil {
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

	return New(cfg, mgr, rt), mgr, groveDir
}

// TestEnvGather_SettingsEmptyEnv tests that env-gather extracts required keys
// from settings-defined empty-value env entries.
func TestEnvGather_SettingsEmptyEnv(t *testing.T) {
	// Settings declares ANTHROPIC_API_KEY as empty (needs gathering)
	srv, _, groveDir := newTestServerWithHarnessConfig(t, "claude",
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
		"grovePath": "` + groveDir + `",
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
	// Settings declares GOOGLE_CLOUD_PROJECT as empty (needs gathering)
	srv, _, groveDir := newTestServerWithHarnessConfig(t, "gemini",
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
		"grovePath": "` + groveDir + `",
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
	srv, mgr, groveDir := newTestServerWithHarnessConfig(t, "gemini",
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
		"grovePath": "` + groveDir + `",
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
	srv, mgr, groveDir := newTestServerWithHarnessConfig(t, "gemini",
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
		"grovePath": "` + groveDir + `",
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
	srv, mgr, groveDir := newTestServerWithProjectPath(t, settings)

	body := `{
		"name": "test-agent-no-gather",
		"id": "agent-uuid-no-gather",
		"gatherEnv": false,
		"grovePath": "` + groveDir + `",
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
	srv, _, groveDir := newTestServerWithProjectPath(t, settings)

	body := `{
		"name": "test-agent-secret-upgrade",
		"id": "agent-uuid-secret",
		"gatherEnv": true,
		"grovePath": "` + groveDir + `",
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
	srv, _, groveDir := newTestServerWithProjectPath(t, settings)

	body := `{
		"name": "test-agent-partial-secret",
		"id": "agent-uuid-partial",
		"gatherEnv": true,
		"grovePath": "` + groveDir + `",
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
	srv, _, groveDir := newTestServerWithProjectPath(t, settings)

	body := `{
		"name": "test-agent-harness-secrets",
		"id": "agent-uuid-hs",
		"gatherEnv": true,
		"grovePath": "` + groveDir + `",
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
	srv, _, groveDir := newTestServerWithProjectPath(t, settings)

	// Satisfy harness key via broker env
	t.Setenv("ANTHROPIC_API_KEY", "broker-ant-key")

	body := `{
		"name": "test-agent-profile-secrets",
		"id": "agent-uuid-ps",
		"gatherEnv": true,
		"grovePath": "` + groveDir + `",
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
	srv, _, groveDir := newTestServerWithProjectPath(t, settings)

	// Satisfy harness key via broker env
	t.Setenv("ANTHROPIC_API_KEY", "broker-ant-key")

	body := `{
		"name": "test-agent-req-secrets",
		"id": "agent-uuid-rs",
		"gatherEnv": true,
		"grovePath": "` + groveDir + `",
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
	srv, _, groveDir := newTestServerWithProjectPath(t, settings)

	// Satisfy harness key and SATISFIED_KEY via resolved secrets
	body := `{
		"name": "test-agent-si-needed",
		"id": "agent-uuid-sin",
		"gatherEnv": true,
		"grovePath": "` + groveDir + `",
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
	srv, _, groveDir := newTestServerWithProjectPath(t, settings)

	// Satisfy harness key via broker env
	t.Setenv("ANTHROPIC_API_KEY", "broker-ant-key")

	body := `{
		"name": "test-agent-type-prop",
		"id": "agent-uuid-tp",
		"gatherEnv": true,
		"grovePath": "` + groveDir + `",
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
	srv, _, groveDir := newTestServerWithProjectPath(t, settings)

	// Satisfy harness key via broker env
	t.Setenv("ANTHROPIC_API_KEY", "broker-ant-key")

	body := `{
		"name": "test-agent-type-tmpl",
		"id": "agent-uuid-tt",
		"gatherEnv": true,
		"grovePath": "` + groveDir + `",
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
	srv, _, groveDir := newTestServerWithProjectPath(t, settings)

	// Satisfy harness key via broker env
	t.Setenv("ANTHROPIC_API_KEY", "broker-ant-key")

	body := `{
		"name": "test-agent-merge",
		"id": "agent-uuid-merge",
		"gatherEnv": true,
		"grovePath": "` + groveDir + `",
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

// TestEnvGather_FinalizeEnv_EmptyTemplate tests that finalize-env succeeds when
// the create request has no template specified, verifying that harness-config
// resolution falls back to settings defaults (default_harness_config) and
// image resolution succeeds.
func TestEnvGather_FinalizeEnv_EmptyTemplate(t *testing.T) {
	groveDir := t.TempDir()

	// Settings with default_harness_config that points to "claude"
	settingsYAML := `
schema_version: "1"
default_harness_config: claude
harness_configs:
  claude:
    harness: claude
    env:
      NEEDED_KEY: ""
profiles:
  default:
    runtime: mock
`
	if err := os.WriteFile(filepath.Join(groveDir, "settings.yaml"), []byte(settingsYAML), 0644); err != nil {
		t.Fatal(err)
	}

	// Create "default" template WITHOUT harness_config so the fallback to
	// settings.default_harness_config is exercised.
	defaultTplDir := filepath.Join(groveDir, "templates", "default")
	if err := os.MkdirAll(defaultTplDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(defaultTplDir, "scion-agent.yaml"), []byte("# no harness_config\n"), 0644); err != nil {
		t.Fatal(err)
	}

	// Create "claude" harness-config directory with image
	claudeHCDir := filepath.Join(groveDir, "harness-configs", "claude")
	if err := os.MkdirAll(claudeHCDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(claudeHCDir, "config.yaml"), []byte("harness: claude\nimage: test-image:claude\n"), 0644); err != nil {
		t.Fatal(err)
	}

	cfg := DefaultServerConfig()
	cfg.BrokerID = "test-broker-id"
	cfg.BrokerName = "test-host"
	cfg.Debug = true

	mgr := &envCapturingManager{}
	// NameFunc returns "docker" so resolveManagerForOpts matches the settings-resolved runtime.
	rt := &runtime.MockRuntime{NameFunc: func() string { return "docker" }}

	srv := New(cfg, mgr, rt)

	// Phase 1: Create agent with gatherEnv but NO template — should get 202
	createBody := `{
		"name": "test-agent-empty-tpl",
		"id": "agent-uuid-empty-tpl",
		"gatherEnv": true,
		"grovePath": "` + groveDir + `",
		"config": {"profile": "default"}
	}`
	createReq := httptest.NewRequest(http.MethodPost, "/api/v1/agents", strings.NewReader(createBody))
	createReq.Header.Set("Content-Type", "application/json")
	createW := httptest.NewRecorder()

	srv.Handler().ServeHTTP(createW, createReq)

	if createW.Code != http.StatusAccepted {
		t.Fatalf("phase 1: expected 202, got %d: %s", createW.Code, createW.Body.String())
	}

	// Phase 2: Submit gathered env via finalize-env
	finalizeBody := `{"env": {"NEEDED_KEY": "gathered-value"}}`
	finalizeReq := httptest.NewRequest(http.MethodPost, "/api/v1/agents/agent-uuid-empty-tpl/finalize-env", strings.NewReader(finalizeBody))
	finalizeReq.Header.Set("Content-Type", "application/json")
	finalizeW := httptest.NewRecorder()

	srv.Handler().ServeHTTP(finalizeW, finalizeReq)

	if finalizeW.Code != http.StatusCreated {
		t.Fatalf("phase 2: expected 201, got %d: %s", finalizeW.Code, finalizeW.Body.String())
	}

	// Verify agent was started with the gathered key
	if mgr.lastEnv == nil {
		t.Fatal("expected env to be set after finalize")
	}
	if mgr.lastEnv["NEEDED_KEY"] != "gathered-value" {
		t.Errorf("expected NEEDED_KEY='gathered-value', got %q", mgr.lastEnv["NEEDED_KEY"])
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
	srv, _, groveDir := newTestServerWithProjectPath(t, settings)

	body := `{
		"name": "test-agent-harness-config",
		"id": "agent-uuid-hc",
		"gatherEnv": true,
		"grovePath": "` + groveDir + `",
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

// TestEnvGather_FinalizeEnv_HarnessConfigPreserved tests that the harnessConfig
// from the original create request is preserved through the env-gather/finalize-env
// flow and passed to Start() in the StartOptions.
func TestEnvGather_FinalizeEnv_HarnessConfigPreserved(t *testing.T) {
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
	srv, mgr, groveDir := newTestServerWithProjectPath(t, settings)

	// Phase 1: Create agent with gatherEnv and explicit harnessConfig — should get 202
	createBody := `{
		"name": "test-agent-hc-preserve",
		"id": "agent-uuid-hcp",
		"gatherEnv": true,
		"grovePath": "` + groveDir + `",
		"config": {"template": "claude", "harnessConfig": "claude", "profile": "default"}
	}`
	createReq := httptest.NewRequest(http.MethodPost, "/api/v1/agents", strings.NewReader(createBody))
	createReq.Header.Set("Content-Type", "application/json")
	createW := httptest.NewRecorder()

	srv.Handler().ServeHTTP(createW, createReq)

	if createW.Code != http.StatusAccepted {
		t.Fatalf("phase 1: expected 202, got %d: %s", createW.Code, createW.Body.String())
	}

	// Phase 2: Submit gathered env via finalize-env
	finalizeBody := `{"env": {"NEEDED_KEY": "gathered-value"}}`
	finalizeReq := httptest.NewRequest(http.MethodPost, "/api/v1/agents/agent-uuid-hcp/finalize-env", strings.NewReader(finalizeBody))
	finalizeReq.Header.Set("Content-Type", "application/json")
	finalizeW := httptest.NewRecorder()

	srv.Handler().ServeHTTP(finalizeW, finalizeReq)

	if finalizeW.Code != http.StatusCreated {
		t.Fatalf("phase 2: expected 201, got %d: %s", finalizeW.Code, finalizeW.Body.String())
	}

	// Verify harnessConfig was preserved through finalize-env
	if mgr.lastHarnessConfig != "claude" {
		t.Errorf("expected HarnessConfig='claude', got %q", mgr.lastHarnessConfig)
	}
}

// TestEnvGather_VertexAI_RequiresADCFile tests that vertex-ai auth without
// an ADC file secret returns 202 with gcloud-adc in needs
// and SecretInfo showing type=file.
func TestEnvGather_VertexAI_RequiresADCFile(t *testing.T) {
	srv, _, groveDir := newTestServerWithHarnessConfig(t, "claude",
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

	// Provide project and region env vars so those are satisfied,
	// but do NOT provide ADC file secret
	body := `{
		"name": "test-agent-vertex-adc",
		"id": "agent-uuid-vadc",
		"gatherEnv": true,
		"grovePath": "` + groveDir + `",
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
	srv, _, groveDir := newTestServerWithHarnessConfig(t, "claude",
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
		"grovePath": "` + groveDir + `",
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
		json.Unmarshal(w.Body.Bytes(), &envReqs)
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
	srv, _, groveDir := newTestServerWithHarnessConfig(t, "claude",
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

	// Provide GOOGLE_APPLICATION_CREDENTIALS but no API key, project, or region
	body := `{
		"name": "test-agent-autodetect-gac",
		"id": "agent-uuid-adgac",
		"gatherEnv": true,
		"grovePath": "` + groveDir + `",
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
	srv, _, groveDir := newTestServerWithHarnessConfig(t, "claude",
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
		"grovePath": "` + groveDir + `",
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
		json.Unmarshal(w.Body.Bytes(), &envReqs)
		for _, k := range envReqs.Needs {
			if k == "gcloud-adc" {
				t.Fatalf("gcloud-adc should not be in needs when GOOGLE_APPLICATION_CREDENTIALS is provided, got needs=%v", envReqs.Needs)
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
	srv, _, groveDir := newTestServerWithHarnessConfig(t, "claude",
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
		"name": "test-agent-autodetect-gcp",
		"id": "agent-uuid-adgcp",
		"gatherEnv": true,
		"grovePath": "` + groveDir + `",
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
	srv, _, groveDir := newTestServerWithHarnessConfig(t, "gemini",
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
		"grovePath": "` + groveDir + `",
		"resolvedEnv": {
			"GOOGLE_CLOUD_PROJECT": "my-project",
			"GOOGLE_CLOUD_REGION": "us-central1",
			"GOOGLE_CLOUD_LOCATION": "us-central1"
		},
		"resolvedSecrets": [
			{"name": "GEMINI_API_KEY", "type": "environment", "target": "GEMINI_API_KEY", "value": "sk-test", "source": "grove"},
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
	srv, _, groveDir := newTestServerWithHarnessConfig(t, "claude",
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
		"grovePath": "` + groveDir + `",
		"resolvedEnv": {
			"GOOGLE_CLOUD_PROJECT": "my-project",
			"GOOGLE_CLOUD_REGION": "us-central1"
		},
		"resolvedSecrets": [
			{"name": "ANTHROPIC_API_KEY", "type": "environment", "target": "ANTHROPIC_API_KEY", "value": "sk-ant-test", "source": "grove"}
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
	srv, _, groveDir := newTestServerWithHarnessConfig(t, "gemini",
		"harness: gemini\nimage: test-image\nuser: scion\n",
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
		"grovePath": "` + groveDir + `",
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
// (e.g. hub-only git groves), the broker falls back to the global ~/.scion
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
	if err := os.WriteFile(filepath.Join(hcDir, "config.yaml"), []byte("harness: gemini\nimage: test-image\n"), 0644); err != nil {
		t.Fatal(err)
	}

	// Override HOME so GetGlobalDir() finds our fake home
	origHome := os.Getenv("HOME")
	t.Setenv("HOME", fakeHome)
	defer os.Setenv("HOME", origHome)

	cfg := DefaultServerConfig()
	cfg.BrokerID = "test-broker-id"
	cfg.BrokerName = "test-host"
	cfg.Debug = true
	cfg.StateDir = t.TempDir()
	mgr := &envCapturingManager{}
	// NameFunc returns "docker" so resolveManagerForOpts matches the settings-resolved runtime.
	rt := &runtime.MockRuntime{NameFunc: func() string { return "docker" }}
	srv := New(cfg, mgr, rt)

	// Send create request with NO projectPath — simulates hub-only git grove
	body := `{
		"name": "test-agent-no-grove",
		"id": "agent-uuid-no-grove",
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
	srv, _, groveDir := newTestServerWithProjectPath(t, settings)

	// Send a request with a resolved secret that has Name but no Target
	body := `{
		"name": "test-agent-target-fallback",
		"id": "agent-uuid-target-fb",
		"gatherEnv": true,
		"grovePath": "` + groveDir + `",
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
	srv, _, groveDir := newTestServerWithHarnessConfig(t, "gemini",
		"harness: gemini\nimage: test-image\nuser: scion\n",
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
		"grovePath": "` + groveDir + `",
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
