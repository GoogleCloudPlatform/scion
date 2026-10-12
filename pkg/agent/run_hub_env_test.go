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
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/runtime"
)

func TestBuildAgentEnv_HubEnvVarsSurviveMerge(t *testing.T) {
	// Verify that hub env vars injected into opts.Env (from project settings
	// or dev token resolution) survive the buildAgentEnv merge.
	scionCfg := &api.ScionConfig{}
	extraEnv := map[string]string{
		"SCION_HUB_ENDPOINT": "http://localhost:9810",
		"SCION_HUB_URL":      "http://localhost:9810",
		"SCION_AUTH_TOKEN":   "scion-dev-test-token-123",
		"SCION_AGENT_NAME":   "test-agent",
	}

	env, _, _, _ := buildAgentEnv(scionCfg, extraEnv, nil, false)

	envMap := make(map[string]string)
	for _, e := range env {
		parts := strings.SplitN(e, "=", 2)
		if len(parts) == 2 {
			envMap[parts[0]] = parts[1]
		}
	}

	expected := map[string]string{
		"SCION_HUB_ENDPOINT": "http://localhost:9810",
		"SCION_HUB_URL":      "http://localhost:9810",
		"SCION_AUTH_TOKEN":   "scion-dev-test-token-123",
		"SCION_AGENT_NAME":   "test-agent",
	}
	for k, want := range expected {
		got, ok := envMap[k]
		if !ok {
			t.Errorf("missing env var %s", k)
			continue
		}
		if got != want {
			t.Errorf("%s = %q, want %q", k, got, want)
		}
	}
}

func TestStartInjectsHubEnvFromProjectSettings(t *testing.T) {
	// When project settings have hub enabled with an endpoint, Start() should
	// inject SCION_HUB_ENDPOINT and SCION_HUB_URL into the container env.
	tmpDir := t.TempDir()

	oldWd, _ := os.Getwd()
	_ = os.Chdir(tmpDir)
	defer func() { _ = os.Chdir(oldWd) }()

	t.Setenv("HOME", tmpDir)

	// Clear env vars that would interfere with settings loading
	for _, k := range []string{"SCION_DEV_TOKEN", "SCION_AUTH_TOKEN", "SCION_DEV_TOKEN_FILE", "SCION_HUB_ENDPOINT", "SCION_HUB_URL"} {
		t.Setenv(k, "")
		_ = os.Unsetenv(k)
	}

	globalScionDir := filepath.Join(tmpDir, ".scion")

	// Create harness-config
	hcDir := filepath.Join(globalScionDir, "harness-configs", "test-harness")
	_ = os.MkdirAll(hcDir, 0755)
	_ = os.WriteFile(filepath.Join(hcDir, "config.yaml"), []byte("harness: gemini\nuser: scion\nimage: test-image:latest\n"), 0644)

	// Create a minimal template
	tplDir := filepath.Join(globalScionDir, "templates", "default")
	_ = os.MkdirAll(tplDir, 0755)
	_ = os.WriteFile(filepath.Join(tplDir, "scion-agent.json"), []byte(`{"default_harness_config": "test-harness"}`), 0644)

	// Global settings
	_ = os.WriteFile(filepath.Join(globalScionDir, "settings.yaml"), []byte(`schema_version: "1"
active_profile: local
profiles:
  local:
    runtime: docker
`), 0644)

	// Create project directory with hub-enabled settings
	projectDir := filepath.Join(tmpDir, "project")
	projectScionDir := filepath.Join(projectDir, ".scion")
	_ = os.MkdirAll(projectScionDir, 0755)
	_ = os.WriteFile(filepath.Join(projectScionDir, "settings.yaml"), []byte(`hub:
  enabled: true
  endpoint: "http://localhost:9810"
`), 0644)

	// Write a dev-token file so the token resolution finds it
	_ = os.WriteFile(filepath.Join(globalScionDir, "dev-token"), []byte("scion-dev-test-token-abc"), 0644)

	// Capture the RunConfig
	var capturedConfig runtime.RunConfig
	mockRT := &runtime.MockRuntime{
		ListFunc: func(ctx context.Context, labelFilter map[string]string) ([]api.AgentInfo, error) {
			return []api.AgentInfo{}, nil
		},
		RunFunc: func(ctx context.Context, cfg runtime.RunConfig) (string, error) {
			capturedConfig = cfg
			return "mock-id", nil
		},
	}

	mgr := NewManager(mockRT)

	_, err := mgr.Start(context.Background(), api.StartOptions{
		Name:        "test-agent",
		ProjectPath: projectScionDir,
		NoAuth:      true,
	})
	if err != nil {
		t.Fatalf("Start failed: %v", err)
	}

	// Convert env slice to map
	envMap := make(map[string]string)
	for _, e := range capturedConfig.Env {
		parts := strings.SplitN(e, "=", 2)
		if len(parts) == 2 {
			envMap[parts[0]] = parts[1]
		}
	}

	if got := envMap["SCION_HUB_ENDPOINT"]; got != "http://localhost:9810" {
		t.Errorf("SCION_HUB_ENDPOINT = %q, want %q", got, "http://localhost:9810")
	}
	if got := envMap["SCION_HUB_URL"]; got != "http://localhost:9810" {
		t.Errorf("SCION_HUB_URL = %q, want %q", got, "http://localhost:9810")
	}
	// SCION_AUTH_TOKEN should NOT be in the container env (it's written to the token file instead)
	if _, exists := envMap["SCION_AUTH_TOKEN"]; exists {
		t.Error("expected SCION_AUTH_TOKEN to NOT be in container env (should be in token file)")
	}

	// Verify the token was written to the agent home token file
	tokenData, err := os.ReadFile(filepath.Join(capturedConfig.HomeDir, ".scion", "scion-token"))
	if err != nil {
		t.Fatalf("failed to read token file: %v", err)
	}
	if got := strings.TrimSpace(string(tokenData)); got != "scion-dev-test-token-abc" {
		t.Errorf("token file = %q, want %q", got, "scion-dev-test-token-abc")
	}
}

func TestStartPreservesExplicitHubEndpoint(t *testing.T) {
	// When hub endpoint is already set in opts.Env (e.g. from broker dispatch),
	// project settings should NOT override it.
	tmpDir := t.TempDir()

	oldWd, _ := os.Getwd()
	_ = os.Chdir(tmpDir)
	defer func() { _ = os.Chdir(oldWd) }()

	originalHome := os.Getenv("HOME")
	defer func() { _ = os.Setenv("HOME", originalHome) }()
	_ = os.Setenv("HOME", tmpDir)

	globalScionDir := filepath.Join(tmpDir, ".scion")

	// Create harness-config
	hcDir := filepath.Join(globalScionDir, "harness-configs", "test-harness")
	_ = os.MkdirAll(hcDir, 0755)
	_ = os.WriteFile(filepath.Join(hcDir, "config.yaml"), []byte("harness: gemini\nuser: scion\nimage: test-image:latest\n"), 0644)

	// Create a minimal template
	tplDir := filepath.Join(globalScionDir, "templates", "default")
	_ = os.MkdirAll(tplDir, 0755)
	_ = os.WriteFile(filepath.Join(tplDir, "scion-agent.json"), []byte(`{"default_harness_config": "test-harness"}`), 0644)

	// Global settings
	_ = os.WriteFile(filepath.Join(globalScionDir, "settings.yaml"), []byte(`schema_version: "1"
active_profile: local
profiles:
  local:
    runtime: docker
`), 0644)

	// Create project directory with hub-enabled settings (different endpoint)
	projectDir := filepath.Join(tmpDir, "project")
	projectScionDir := filepath.Join(projectDir, ".scion")
	_ = os.MkdirAll(projectScionDir, 0755)
	_ = os.WriteFile(filepath.Join(projectScionDir, "settings.yaml"), []byte(`hub:
  enabled: true
  endpoint: "http://project-setting:9810"
`), 0644)

	// Capture the RunConfig
	var capturedConfig runtime.RunConfig
	mockRT := &runtime.MockRuntime{
		ListFunc: func(ctx context.Context, labelFilter map[string]string) ([]api.AgentInfo, error) {
			return []api.AgentInfo{}, nil
		},
		RunFunc: func(ctx context.Context, cfg runtime.RunConfig) (string, error) {
			capturedConfig = cfg
			return "mock-id", nil
		},
	}

	mgr := NewManager(mockRT)

	_, err := mgr.Start(context.Background(), api.StartOptions{
		Name:        "test-agent",
		ProjectPath: projectScionDir,
		NoAuth:      true,
		Env: map[string]string{
			"SCION_HUB_ENDPOINT": "http://broker-dispatch:9810",
		},
	})
	if err != nil {
		t.Fatalf("Start failed: %v", err)
	}

	envMap := make(map[string]string)
	for _, e := range capturedConfig.Env {
		parts := strings.SplitN(e, "=", 2)
		if len(parts) == 2 {
			envMap[parts[0]] = parts[1]
		}
	}

	// Broker-dispatched endpoint should be preserved, not overwritten by project settings
	if got := envMap["SCION_HUB_ENDPOINT"]; got != "http://broker-dispatch:9810" {
		t.Errorf("SCION_HUB_ENDPOINT = %q, want %q (explicit should win over project settings)", got, "http://broker-dispatch:9810")
	}
}

func TestBuildAgentEnv_EnvKeyScionHubEndpointOverride(t *testing.T) {
	// Unit test verifying that when scionCfg.Env has SCION_HUB_ENDPOINT and
	// it's pre-applied to extraEnv (simulating the new run.go logic), the
	// env-section value wins over the project/broker value.
	t.Run("env section SCION_HUB_ENDPOINT overrides all via pre-apply", func(t *testing.T) {
		scionCfg := &api.ScionConfig{
			Hub: &api.AgentHubConfig{
				Endpoint: "https://hub-endpoint.example.com",
			},
			Env: map[string]string{
				"SCION_HUB_ENDPOINT": "http://host.docker.internal:8080",
			},
		}

		// Simulate the priority chain from Start():
		// 1. CLI/project settings sets initial value
		extraEnv := map[string]string{
			"SCION_HUB_ENDPOINT": "http://localhost:8080",
			"SCION_HUB_URL":      "http://localhost:8080",
		}

		// 2. hub.endpoint overrides
		if scionCfg.Hub != nil && scionCfg.Hub.Endpoint != "" {
			extraEnv["SCION_HUB_ENDPOINT"] = scionCfg.Hub.Endpoint
			extraEnv["SCION_HUB_URL"] = scionCfg.Hub.Endpoint
		}

		// 3. env section SCION_HUB_ENDPOINT takes final priority
		if scionCfg.Env != nil {
			if ep, ok := scionCfg.Env["SCION_HUB_ENDPOINT"]; ok && ep != "" {
				extraEnv["SCION_HUB_ENDPOINT"] = ep
				extraEnv["SCION_HUB_URL"] = ep
			}
		}

		env, _, _, _ := buildAgentEnv(scionCfg, extraEnv, nil, false)

		envMap := make(map[string]string)
		for _, e := range env {
			parts := strings.SplitN(e, "=", 2)
			if len(parts) == 2 {
				envMap[parts[0]] = parts[1]
			}
		}

		// The env section value should be the final winner
		if got := envMap["SCION_HUB_ENDPOINT"]; got != "http://host.docker.internal:8080" {
			t.Errorf("SCION_HUB_ENDPOINT = %q, want %q (env section should win)", got, "http://host.docker.internal:8080")
		}
		if got := envMap["SCION_HUB_URL"]; got != "http://host.docker.internal:8080" {
			t.Errorf("SCION_HUB_URL = %q, want %q (env section should win)", got, "http://host.docker.internal:8080")
		}
	})

	t.Run("no env section key preserves hub.endpoint", func(t *testing.T) {
		scionCfg := &api.ScionConfig{
			Hub: &api.AgentHubConfig{
				Endpoint: "https://hub-endpoint.example.com",
			},
			Env: map[string]string{
				"OTHER_VAR": "value",
			},
		}

		extraEnv := map[string]string{
			"SCION_HUB_ENDPOINT": "http://localhost:8080",
			"SCION_HUB_URL":      "http://localhost:8080",
		}

		// hub.endpoint overrides
		if scionCfg.Hub != nil && scionCfg.Hub.Endpoint != "" {
			extraEnv["SCION_HUB_ENDPOINT"] = scionCfg.Hub.Endpoint
			extraEnv["SCION_HUB_URL"] = scionCfg.Hub.Endpoint
		}

		// No SCION_HUB_ENDPOINT in env section — should not change
		if scionCfg.Env != nil {
			if ep, ok := scionCfg.Env["SCION_HUB_ENDPOINT"]; ok && ep != "" {
				extraEnv["SCION_HUB_ENDPOINT"] = ep
				extraEnv["SCION_HUB_URL"] = ep
			}
		}

		env, _, _, _ := buildAgentEnv(scionCfg, extraEnv, nil, false)

		envMap := make(map[string]string)
		for _, e := range env {
			parts := strings.SplitN(e, "=", 2)
			if len(parts) == 2 {
				envMap[parts[0]] = parts[1]
			}
		}

		if got := envMap["SCION_HUB_ENDPOINT"]; got != "https://hub-endpoint.example.com" {
			t.Errorf("SCION_HUB_ENDPOINT = %q, want %q (hub.endpoint should win when no env key)", got, "https://hub-endpoint.example.com")
		}
	})
}

func TestStartSuppressesHubEnvWhenHubDisabled(t *testing.T) {
	// When project settings have hub.enabled=false, hub env vars should NOT be
	// injected into the container, even when hub.endpoint is configured and
	// agent-level hub config or template env section specifies an endpoint.
	tmpDir := t.TempDir()

	oldWd, _ := os.Getwd()
	_ = os.Chdir(tmpDir)
	defer func() { _ = os.Chdir(oldWd) }()

	originalHome := os.Getenv("HOME")
	defer func() { _ = os.Setenv("HOME", originalHome) }()
	_ = os.Setenv("HOME", tmpDir)

	// Clear dev token env vars so we control the test
	for _, k := range []string{"SCION_DEV_TOKEN", "SCION_AUTH_TOKEN", "SCION_DEV_TOKEN_FILE"} {
		if old, ok := os.LookupEnv(k); ok {
			defer func() { _ = os.Setenv(k, old) }()
			_ = os.Unsetenv(k)
		}
	}

	globalScionDir := filepath.Join(tmpDir, ".scion")

	// Create harness-config
	hcDir := filepath.Join(globalScionDir, "harness-configs", "test-harness")
	_ = os.MkdirAll(hcDir, 0755)
	_ = os.WriteFile(filepath.Join(hcDir, "config.yaml"), []byte("harness: gemini\nuser: scion\nimage: test-image:latest\n"), 0644)

	// Create a minimal template
	tplDir := filepath.Join(globalScionDir, "templates", "default")
	_ = os.MkdirAll(tplDir, 0755)
	_ = os.WriteFile(filepath.Join(tplDir, "scion-agent.json"), []byte(`{"default_harness_config": "test-harness"}`), 0644)

	// Global settings
	_ = os.WriteFile(filepath.Join(globalScionDir, "settings.yaml"), []byte(`schema_version: "1"
active_profile: local
profiles:
  local:
    runtime: docker
`), 0644)

	// Create project directory with hub explicitly DISABLED but endpoint configured
	projectDir := filepath.Join(tmpDir, "project")
	projectScionDir := filepath.Join(projectDir, ".scion")
	_ = os.MkdirAll(projectScionDir, 0755)
	_ = os.WriteFile(filepath.Join(projectScionDir, "settings.yaml"), []byte(`hub:
  enabled: false
  endpoint: "http://localhost:9810"
`), 0644)

	// Write a dev-token file (should NOT be used since hub is disabled)
	_ = os.WriteFile(filepath.Join(globalScionDir, "dev-token"), []byte("scion-dev-test-token-abc"), 0644)

	t.Run("project settings hub disabled suppresses hub env", func(t *testing.T) {
		var capturedConfig runtime.RunConfig
		mockRT := &runtime.MockRuntime{
			ListFunc: func(ctx context.Context, labelFilter map[string]string) ([]api.AgentInfo, error) {
				return []api.AgentInfo{}, nil
			},
			RunFunc: func(ctx context.Context, cfg runtime.RunConfig) (string, error) {
				capturedConfig = cfg
				return "mock-id", nil
			},
		}

		mgr := NewManager(mockRT)

		_, err := mgr.Start(context.Background(), api.StartOptions{
			Name:        "test-agent",
			ProjectPath: projectScionDir,
			NoAuth:      true,
		})
		if err != nil {
			t.Fatalf("Start failed: %v", err)
		}

		envMap := make(map[string]string)
		for _, e := range capturedConfig.Env {
			parts := strings.SplitN(e, "=", 2)
			if len(parts) == 2 {
				envMap[parts[0]] = parts[1]
			}
		}

		if _, exists := envMap["SCION_HUB_ENDPOINT"]; exists {
			t.Error("expected SCION_HUB_ENDPOINT to NOT be set when hub.enabled=false")
		}
		if _, exists := envMap["SCION_HUB_URL"]; exists {
			t.Error("expected SCION_HUB_URL to NOT be set when hub.enabled=false")
		}
		if _, exists := envMap["SCION_AUTH_TOKEN"]; exists {
			t.Error("expected SCION_AUTH_TOKEN to NOT be set when hub.enabled=false")
		}
	})

	t.Run("agent-level hub endpoint suppressed when hub disabled", func(t *testing.T) {
		// Agent scion-agent.json has hub.endpoint but project says hub.enabled=false
		agentDir := filepath.Join(projectScionDir, "agents", "hub-disabled-agent")
		_ = os.MkdirAll(filepath.Join(agentDir, "home"), 0755)
		_ = os.WriteFile(filepath.Join(agentDir, "scion-agent.json"), []byte(`{
			"harness": "gemini",
			"hub": {
				"endpoint": "http://agent-hub:9810"
			}
		}`), 0644)

		var capturedConfig runtime.RunConfig
		mockRT := &runtime.MockRuntime{
			ListFunc: func(ctx context.Context, labelFilter map[string]string) ([]api.AgentInfo, error) {
				return []api.AgentInfo{}, nil
			},
			RunFunc: func(ctx context.Context, cfg runtime.RunConfig) (string, error) {
				capturedConfig = cfg
				return "mock-id", nil
			},
		}

		mgr := NewManager(mockRT)

		_, err := mgr.Start(context.Background(), api.StartOptions{
			Name:        "hub-disabled-agent",
			ProjectPath: projectScionDir,
			NoAuth:      true,
		})
		if err != nil {
			t.Fatalf("Start failed: %v", err)
		}

		envMap := make(map[string]string)
		for _, e := range capturedConfig.Env {
			parts := strings.SplitN(e, "=", 2)
			if len(parts) == 2 {
				envMap[parts[0]] = parts[1]
			}
		}

		if _, exists := envMap["SCION_HUB_ENDPOINT"]; exists {
			t.Error("expected SCION_HUB_ENDPOINT to NOT be set when hub.enabled=false, even with agent hub.endpoint")
		}
	})

	t.Run("template env section hub endpoint suppressed when hub disabled", func(t *testing.T) {
		// Agent scion-agent.json has env.SCION_HUB_ENDPOINT but project says hub.enabled=false
		agentDir := filepath.Join(projectScionDir, "agents", "hub-disabled-env")
		_ = os.MkdirAll(filepath.Join(agentDir, "home"), 0755)
		_ = os.WriteFile(filepath.Join(agentDir, "scion-agent.json"), []byte(`{
			"harness": "gemini",
			"env": {
				"SCION_HUB_ENDPOINT": "http://host.docker.internal:8080"
			}
		}`), 0644)

		var capturedConfig runtime.RunConfig
		mockRT := &runtime.MockRuntime{
			ListFunc: func(ctx context.Context, labelFilter map[string]string) ([]api.AgentInfo, error) {
				return []api.AgentInfo{}, nil
			},
			RunFunc: func(ctx context.Context, cfg runtime.RunConfig) (string, error) {
				capturedConfig = cfg
				return "mock-id", nil
			},
		}

		mgr := NewManager(mockRT)

		_, err := mgr.Start(context.Background(), api.StartOptions{
			Name:        "hub-disabled-env",
			ProjectPath: projectScionDir,
			NoAuth:      true,
		})
		if err != nil {
			t.Fatalf("Start failed: %v", err)
		}

		envMap := make(map[string]string)
		for _, e := range capturedConfig.Env {
			parts := strings.SplitN(e, "=", 2)
			if len(parts) == 2 {
				envMap[parts[0]] = parts[1]
			}
		}

		if _, exists := envMap["SCION_HUB_ENDPOINT"]; exists {
			t.Error("expected SCION_HUB_ENDPOINT to NOT be set when hub.enabled=false, even with env section override")
		}
	})
}

func TestStartScionConfigEnvHubEndpointOverridesAll(t *testing.T) {
	// Integration test verifying the full priority chain:
	// project settings -> hub.endpoint -> env.SCION_HUB_ENDPOINT
	// The env-key value should be the final one in the container env.
	tmpDir := t.TempDir()

	oldWd, _ := os.Getwd()
	_ = os.Chdir(tmpDir)
	defer func() { _ = os.Chdir(oldWd) }()

	originalHome := os.Getenv("HOME")
	defer func() { _ = os.Setenv("HOME", originalHome) }()
	_ = os.Setenv("HOME", tmpDir)

	// Clear dev token env vars so we control the test
	for _, k := range []string{"SCION_DEV_TOKEN", "SCION_AUTH_TOKEN", "SCION_DEV_TOKEN_FILE"} {
		if old, ok := os.LookupEnv(k); ok {
			defer func() { _ = os.Setenv(k, old) }()
			_ = os.Unsetenv(k)
		}
	}

	globalScionDir := filepath.Join(tmpDir, ".scion")

	// Create harness-config
	hcDir := filepath.Join(globalScionDir, "harness-configs", "test-harness")
	_ = os.MkdirAll(hcDir, 0755)
	_ = os.WriteFile(filepath.Join(hcDir, "config.yaml"), []byte("harness: gemini\nuser: scion\nimage: test-image:latest\n"), 0644)

	// Create a minimal template
	tplDir := filepath.Join(globalScionDir, "templates", "default")
	_ = os.MkdirAll(tplDir, 0755)
	_ = os.WriteFile(filepath.Join(tplDir, "scion-agent.json"), []byte(`{"default_harness_config": "test-harness"}`), 0644)

	// Global settings
	_ = os.WriteFile(filepath.Join(globalScionDir, "settings.yaml"), []byte(`schema_version: "1"
active_profile: local
profiles:
  local:
    runtime: docker
`), 0644)

	// Create project directory with hub-enabled settings (priority 1)
	projectDir := filepath.Join(tmpDir, "project")
	projectScionDir := filepath.Join(projectDir, ".scion")
	_ = os.MkdirAll(projectScionDir, 0755)
	_ = os.WriteFile(filepath.Join(projectScionDir, "settings.yaml"), []byte(`hub:
  enabled: true
  endpoint: "http://project-settings:9810"
`), 0644)

	// Create agent with both hub.endpoint (priority 2) and
	// env.SCION_HUB_ENDPOINT (priority 3 — should win)
	agentDir := filepath.Join(projectScionDir, "agents", "hub-env-test")
	_ = os.MkdirAll(filepath.Join(agentDir, "home"), 0755)
	_ = os.WriteFile(filepath.Join(agentDir, "scion-agent.json"), []byte(`{
		"harness": "gemini",
		"hub": {
			"endpoint": "http://hub-endpoint-field:9810"
		},
		"env": {
			"SCION_HUB_ENDPOINT": "http://host.docker.internal:8080"
		}
	}`), 0644)

	// Capture the RunConfig
	var capturedConfig runtime.RunConfig
	mockRT := &runtime.MockRuntime{
		ListFunc: func(ctx context.Context, labelFilter map[string]string) ([]api.AgentInfo, error) {
			return []api.AgentInfo{}, nil
		},
		RunFunc: func(ctx context.Context, cfg runtime.RunConfig) (string, error) {
			capturedConfig = cfg
			return "mock-id", nil
		},
	}

	mgr := NewManager(mockRT)

	_, err := mgr.Start(context.Background(), api.StartOptions{
		Name:        "hub-env-test",
		ProjectPath: projectScionDir,
		NoAuth:      true,
	})
	if err != nil {
		t.Fatalf("Start failed: %v", err)
	}

	// Convert env slice to map
	envMap := make(map[string]string)
	for _, e := range capturedConfig.Env {
		parts := strings.SplitN(e, "=", 2)
		if len(parts) == 2 {
			envMap[parts[0]] = parts[1]
		}
	}

	// The env section value (priority 3) should be the final winner,
	// overriding both project settings (priority 1) and hub.endpoint (priority 2)
	if got := envMap["SCION_HUB_ENDPOINT"]; got != "http://host.docker.internal:8080" {
		t.Errorf("SCION_HUB_ENDPOINT = %q, want %q (env section should override all)", got, "http://host.docker.internal:8080")
	}
	if got := envMap["SCION_HUB_URL"]; got != "http://host.docker.internal:8080" {
		t.Errorf("SCION_HUB_URL = %q, want %q (env section should override all)", got, "http://host.docker.internal:8080")
	}
}

// setupTrustedHubEndpointTestProject writes the project/template/harness-config
// fixture TestStartTrustedHubEndpoint's cases share: a project with
// projectSettingsHubEndpoint configured (used only by the non-broker-mode
// case), and an agent whose scion-agent.json sets both hub.endpoint and
// env.SCION_HUB_ENDPOINT to creator-controlled values distinct from the
// caller-provided one, so a test can assert neither ever reaches
// TrustedHubEndpoint.
func setupTrustedHubEndpointTestProject(t *testing.T, agentName, agentLevelHubEndpoint, templateEnvHubEndpoint string) (projectScionDir string) {
	t.Helper()
	tmpDir := t.TempDir()

	oldWd, _ := os.Getwd()
	_ = os.Chdir(tmpDir)
	t.Cleanup(func() { _ = os.Chdir(oldWd) })
	t.Setenv("HOME", tmpDir)
	for _, k := range []string{"SCION_DEV_TOKEN", "SCION_AUTH_TOKEN", "SCION_DEV_TOKEN_FILE", "SCION_HUB_ENDPOINT", "SCION_HUB_URL"} {
		t.Setenv(k, "")
		_ = os.Unsetenv(k)
	}

	globalScionDir := filepath.Join(tmpDir, ".scion")
	hcDir := filepath.Join(globalScionDir, "harness-configs", "test-harness")
	_ = os.MkdirAll(hcDir, 0755)
	_ = os.WriteFile(filepath.Join(hcDir, "config.yaml"), []byte("harness: gemini\nuser: scion\nimage: test-image:latest\n"), 0644)
	tplDir := filepath.Join(globalScionDir, "templates", "default")
	_ = os.MkdirAll(tplDir, 0755)
	_ = os.WriteFile(filepath.Join(tplDir, "scion-agent.json"), []byte(`{"default_harness_config": "test-harness"}`), 0644)
	_ = os.WriteFile(filepath.Join(globalScionDir, "settings.yaml"), []byte("schema_version: \"1\"\nactive_profile: local\nprofiles:\n  local:\n    runtime: docker\n"), 0644)

	projectDir := filepath.Join(tmpDir, "project")
	projectScionDir = filepath.Join(projectDir, ".scion")
	_ = os.MkdirAll(projectScionDir, 0755)
	_ = os.WriteFile(filepath.Join(projectScionDir, "settings.yaml"), []byte("hub:\n  enabled: true\n  endpoint: \"http://project-settings:9810\"\n"), 0644)

	agentDir := filepath.Join(projectScionDir, "agents", agentName)
	_ = os.MkdirAll(filepath.Join(agentDir, "home"), 0755)
	agentJSON := fmt.Sprintf(`{"harness": "gemini", "hub": {"endpoint": %q}, "env": {"SCION_HUB_ENDPOINT": %q}}`,
		agentLevelHubEndpoint, templateEnvHubEndpoint)
	_ = os.WriteFile(filepath.Join(agentDir, "scion-agent.json"), []byte(agentJSON), 0644)

	return projectScionDir
}

// TestStartTrustedHubEndpoint pins the Start->RunConfig.TrustedHubEndpoint
// path directly (not just substrateEgressHostnames), so that capturing
// callerHubEndpoint anywhere other than the top of Start — in particular,
// after the agent-level Hub-config override applies — fails one of these
// cases.
func TestStartTrustedHubEndpoint(t *testing.T) {
	const (
		brokerHubEndpoint      = "http://broker-hub:9810"
		agentLevelHubEndpoint  = "http://169.254.169.254"
		templateEnvHubEndpoint = "http://host.docker.internal:8080"
	)

	t.Run("broker opts.Env hub is trusted", func(t *testing.T) {
		projectScionDir := setupTrustedHubEndpointTestProject(t, "agent-1", agentLevelHubEndpoint, templateEnvHubEndpoint)
		var capturedConfig runtime.RunConfig
		mockRT := &runtime.MockRuntime{
			ListFunc: func(ctx context.Context, labelFilter map[string]string) ([]api.AgentInfo, error) {
				return []api.AgentInfo{}, nil
			},
			RunFunc: func(ctx context.Context, cfg runtime.RunConfig) (string, error) {
				capturedConfig = cfg
				return "mock-id", nil
			},
		}
		mgr := NewManager(mockRT)
		_, err := mgr.Start(context.Background(), api.StartOptions{
			Name:               "agent-1",
			ProjectPath:        projectScionDir,
			BrokerMode:         true,
			NoAuth:             true,
			Env:                map[string]string{"SCION_HUB_ENDPOINT": brokerHubEndpoint},
			TrustedHubEndpoint: brokerHubEndpoint,
		})
		if err != nil {
			t.Fatalf("Start failed: %v", err)
		}
		if capturedConfig.TrustedHubEndpoint != brokerHubEndpoint {
			t.Errorf("TrustedHubEndpoint = %q, want %q (the broker-supplied opts.TrustedHubEndpoint value)", capturedConfig.TrustedHubEndpoint, brokerHubEndpoint)
		}
	})

	t.Run("agent-level hub.endpoint never trusted, broker host still is", func(t *testing.T) {
		projectScionDir := setupTrustedHubEndpointTestProject(t, "agent-2", agentLevelHubEndpoint, "")
		var capturedConfig runtime.RunConfig
		mockRT := &runtime.MockRuntime{
			ListFunc: func(ctx context.Context, labelFilter map[string]string) ([]api.AgentInfo, error) {
				return []api.AgentInfo{}, nil
			},
			RunFunc: func(ctx context.Context, cfg runtime.RunConfig) (string, error) {
				capturedConfig = cfg
				return "mock-id", nil
			},
		}
		mgr := NewManager(mockRT)
		_, err := mgr.Start(context.Background(), api.StartOptions{
			Name:               "agent-2",
			ProjectPath:        projectScionDir,
			BrokerMode:         true,
			NoAuth:             true,
			Env:                map[string]string{"SCION_HUB_ENDPOINT": brokerHubEndpoint},
			TrustedHubEndpoint: brokerHubEndpoint,
		})
		if err != nil {
			t.Fatalf("Start failed: %v", err)
		}
		if capturedConfig.TrustedHubEndpoint != brokerHubEndpoint {
			t.Errorf("TrustedHubEndpoint = %q, want %q (agent-level hub.endpoint %q must never win)", capturedConfig.TrustedHubEndpoint, brokerHubEndpoint, agentLevelHubEndpoint)
		}
	})

	t.Run("template env SCION_HUB_ENDPOINT override never trusted", func(t *testing.T) {
		projectScionDir := setupTrustedHubEndpointTestProject(t, "agent-3", "", templateEnvHubEndpoint)
		var capturedConfig runtime.RunConfig
		mockRT := &runtime.MockRuntime{
			ListFunc: func(ctx context.Context, labelFilter map[string]string) ([]api.AgentInfo, error) {
				return []api.AgentInfo{}, nil
			},
			RunFunc: func(ctx context.Context, cfg runtime.RunConfig) (string, error) {
				capturedConfig = cfg
				return "mock-id", nil
			},
		}
		mgr := NewManager(mockRT)
		_, err := mgr.Start(context.Background(), api.StartOptions{
			Name:               "agent-3",
			ProjectPath:        projectScionDir,
			BrokerMode:         true,
			NoAuth:             true,
			Env:                map[string]string{"SCION_HUB_ENDPOINT": brokerHubEndpoint},
			TrustedHubEndpoint: brokerHubEndpoint,
		})
		if err != nil {
			t.Fatalf("Start failed: %v", err)
		}
		if capturedConfig.TrustedHubEndpoint != brokerHubEndpoint {
			t.Errorf("TrustedHubEndpoint = %q, want %q (template env override %q must never win)", capturedConfig.TrustedHubEndpoint, brokerHubEndpoint, templateEnvHubEndpoint)
		}
		// Confirm the override DOES still reach the final container env,
		// proving this case actually exercises the override path rather
		// than a no-op.
		envMap := make(map[string]string)
		for _, e := range capturedConfig.Env {
			parts := strings.SplitN(e, "=", 2)
			if len(parts) == 2 {
				envMap[parts[0]] = parts[1]
			}
		}
		if got := envMap["SCION_HUB_ENDPOINT"]; got != templateEnvHubEndpoint {
			t.Fatalf("existence control failed: final env SCION_HUB_ENDPOINT = %q, want %q — the override was not applied", got, templateEnvHubEndpoint)
		}
	})

	t.Run("empty caller hub in broker mode adds no hub host", func(t *testing.T) {
		projectScionDir := setupTrustedHubEndpointTestProject(t, "agent-4", agentLevelHubEndpoint, "")
		var capturedConfig runtime.RunConfig
		mockRT := &runtime.MockRuntime{
			ListFunc: func(ctx context.Context, labelFilter map[string]string) ([]api.AgentInfo, error) {
				return []api.AgentInfo{}, nil
			},
			RunFunc: func(ctx context.Context, cfg runtime.RunConfig) (string, error) {
				capturedConfig = cfg
				return "mock-id", nil
			},
		}
		mgr := NewManager(mockRT)
		_, err := mgr.Start(context.Background(), api.StartOptions{
			Name:        "agent-4",
			ProjectPath: projectScionDir,
			BrokerMode:  true,
			NoAuth:      true,
			// No SCION_HUB_ENDPOINT in opts.Env at all.
		})
		if err != nil {
			t.Fatalf("Start failed: %v", err)
		}
		if capturedConfig.TrustedHubEndpoint != "" {
			t.Errorf("TrustedHubEndpoint = %q, want \"\" (broker mode with no caller-supplied hub endpoint must not fall back to agent-level config or project settings)", capturedConfig.TrustedHubEndpoint)
		}
	})

	// A tenant-controllable value (ResolvedEnv/Config.Env, mirrored here by
	// setting opts.Env directly without opts.TrustedHubEndpoint) must never
	// feed egress trust, even though it still reaches opts.Env and is
	// delivered to the agent as SCION_HUB_ENDPOINT: BrokerMode must read
	// opts.TrustedHubEndpoint only, never fall back to
	// opts.Env["SCION_HUB_ENDPOINT"].
	t.Run("tenant env hub endpoint present but TrustedHubEndpoint empty: not trusted", func(t *testing.T) {
		projectScionDir := setupTrustedHubEndpointTestProject(t, "agent-5", agentLevelHubEndpoint, "")
		var capturedConfig runtime.RunConfig
		mockRT := &runtime.MockRuntime{
			ListFunc: func(ctx context.Context, labelFilter map[string]string) ([]api.AgentInfo, error) {
				return []api.AgentInfo{}, nil
			},
			RunFunc: func(ctx context.Context, cfg runtime.RunConfig) (string, error) {
				capturedConfig = cfg
				return "mock-id", nil
			},
		}
		mgr := NewManager(mockRT)
		_, err := mgr.Start(context.Background(), api.StartOptions{
			Name:        "agent-5",
			ProjectPath: projectScionDir,
			BrokerMode:  true,
			NoAuth:      true,
			// A tenant-controllable value present in opts.Env, exactly as
			// resolveHubEndpointForCreate's ResolvedEnv tier would leave it
			// when every operator tier is empty — but TrustedHubEndpoint
			// itself is left unset, as buildStartContext now does in that
			// case.
			Env: map[string]string{"SCION_HUB_ENDPOINT": "http://169.254.169.254"},
		})
		if err != nil {
			t.Fatalf("Start failed: %v", err)
		}
		if capturedConfig.TrustedHubEndpoint != "" {
			t.Errorf("TrustedHubEndpoint = %q, want \"\" — a tenant-controllable opts.Env value must never feed egress trust", capturedConfig.TrustedHubEndpoint)
		}
	})

	// Non-broker mode (BrokerMode: false — a local CLI start, not a
	// broker-dispatched one): trustedHubEndpoint falls back to
	// callerHubEndpoint (opts.Env, captured before any override),
	// then projectSettingsHubEndpoint (the project's own settings.yaml
	// hub.endpoint — an operator-controlled file). Neither of these two
	// non-broker cases was exercised anywhere before this test: every
	// other case above sets BrokerMode: true.
	t.Run("non-broker mode: caller-supplied env hub endpoint is trusted", func(t *testing.T) {
		projectScionDir := setupTrustedHubEndpointTestProject(t, "agent-6", agentLevelHubEndpoint, templateEnvHubEndpoint)
		const callerHubEndpoint = "http://caller-hub:9810"
		var capturedConfig runtime.RunConfig
		mockRT := &runtime.MockRuntime{
			ListFunc: func(ctx context.Context, labelFilter map[string]string) ([]api.AgentInfo, error) {
				return []api.AgentInfo{}, nil
			},
			RunFunc: func(ctx context.Context, cfg runtime.RunConfig) (string, error) {
				capturedConfig = cfg
				return "mock-id", nil
			},
		}
		mgr := NewManager(mockRT)
		_, err := mgr.Start(context.Background(), api.StartOptions{
			Name:        "agent-6",
			ProjectPath: projectScionDir,
			BrokerMode:  false,
			NoAuth:      true,
			Env:         map[string]string{"SCION_HUB_ENDPOINT": callerHubEndpoint},
		})
		if err != nil {
			t.Fatalf("Start failed: %v", err)
		}
		if capturedConfig.TrustedHubEndpoint != callerHubEndpoint {
			t.Errorf("TrustedHubEndpoint = %q, want %q (the caller-supplied opts.Env value, agent-level config and template env must never win)", capturedConfig.TrustedHubEndpoint, callerHubEndpoint)
		}
	})

	t.Run("non-broker mode: falls back to project settings hub endpoint when the caller supplies none", func(t *testing.T) {
		// Neither an agent-level hub.endpoint nor a template env override:
		// either would set opts.Env["SCION_HUB_ENDPOINT"] itself (run.go's
		// earlier agent-level-hub-config branch), which would skip the
		// project-settings branch entirely (its own guard is "only if
		// opts.Env[\"SCION_HUB_ENDPOINT\"] isn't already set") before
		// trustedHubEndpoint's fallback is ever reached.
		projectScionDir := setupTrustedHubEndpointTestProject(t, "agent-7", "", "")
		const projectSettingsHubEndpoint = "http://project-settings:9810" // set by setupTrustedHubEndpointTestProject's settings.yaml fixture.
		var capturedConfig runtime.RunConfig
		mockRT := &runtime.MockRuntime{
			ListFunc: func(ctx context.Context, labelFilter map[string]string) ([]api.AgentInfo, error) {
				return []api.AgentInfo{}, nil
			},
			RunFunc: func(ctx context.Context, cfg runtime.RunConfig) (string, error) {
				capturedConfig = cfg
				return "mock-id", nil
			},
		}
		mgr := NewManager(mockRT)
		_, err := mgr.Start(context.Background(), api.StartOptions{
			Name:        "agent-7",
			ProjectPath: projectScionDir,
			BrokerMode:  false,
			NoAuth:      true,
		})
		if err != nil {
			t.Fatalf("Start failed: %v", err)
		}
		if capturedConfig.TrustedHubEndpoint != projectSettingsHubEndpoint {
			t.Errorf("TrustedHubEndpoint = %q, want %q (project settings' own hub.endpoint, with no caller-supplied value present)", capturedConfig.TrustedHubEndpoint, projectSettingsHubEndpoint)
		}
	})
}
