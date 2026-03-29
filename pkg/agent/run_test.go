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
	"os/user"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/runtime"
)

func TestExtractWorkspaceFromVolumes(t *testing.T) {
	tests := []struct {
		name     string
		volumes  []api.VolumeMount
		expected string
	}{
		{
			name:     "empty volumes",
			volumes:  nil,
			expected: "",
		},
		{
			name: "no workspace volume",
			volumes: []api.VolumeMount{
				{Source: "/host/data", Target: "/data"},
				{Source: "/host/config", Target: "/config"},
			},
			expected: "",
		},
		{
			name: "has workspace volume",
			volumes: []api.VolumeMount{
				{Source: "/host/data", Target: "/data"},
				{Source: "/path/to/shared/worktree", Target: "/workspace"},
				{Source: "/host/config", Target: "/config"},
			},
			expected: "/path/to/shared/worktree",
		},
		{
			name: "first workspace volume wins",
			volumes: []api.VolumeMount{
				{Source: "/first/workspace", Target: "/workspace"},
				{Source: "/second/workspace", Target: "/workspace"},
			},
			expected: "/first/workspace",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := extractWorkspaceFromVolumes(tt.volumes)
			if result != tt.expected {
				t.Errorf("extractWorkspaceFromVolumes() = %q, want %q", result, tt.expected)
			}
		})
	}
}

func TestFilterWorkspaceVolume(t *testing.T) {
	tests := []struct {
		name           string
		volumes        []api.VolumeMount
		expectedLen    int
		expectedAbsent string
	}{
		{
			name:           "empty volumes",
			volumes:        nil,
			expectedLen:    0,
			expectedAbsent: "/workspace",
		},
		{
			name: "no workspace volume",
			volumes: []api.VolumeMount{
				{Source: "/host/data", Target: "/data"},
				{Source: "/host/config", Target: "/config"},
			},
			expectedLen:    2,
			expectedAbsent: "/workspace",
		},
		{
			name: "filters workspace volume",
			volumes: []api.VolumeMount{
				{Source: "/host/data", Target: "/data"},
				{Source: "/path/to/worktree", Target: "/workspace"},
				{Source: "/host/config", Target: "/config"},
			},
			expectedLen:    2,
			expectedAbsent: "/workspace",
		},
		{
			name: "filters multiple workspace volumes",
			volumes: []api.VolumeMount{
				{Source: "/first", Target: "/workspace"},
				{Source: "/second", Target: "/workspace"},
				{Source: "/host/data", Target: "/data"},
			},
			expectedLen:    1,
			expectedAbsent: "/workspace",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := filterWorkspaceVolume(tt.volumes)
			if len(result) != tt.expectedLen {
				t.Errorf("filterWorkspaceVolume() returned %d volumes, want %d", len(result), tt.expectedLen)
			}
			for _, v := range result {
				if v.Target == tt.expectedAbsent {
					t.Errorf("filterWorkspaceVolume() should have removed volume with target %q", tt.expectedAbsent)
				}
			}
		})
	}
}

func TestBuildAgentEnv(t *testing.T) {
	// Setup host env for inheritance test
	os.Setenv("INHERITED_KEY", "inherited-value")
	defer os.Unsetenv("INHERITED_KEY")

	scionCfg := &api.ScionConfig{
		Env: map[string]string{
			"NORMAL_KEY":     "normal-value",
			"INHERITED_KEY":  "${INHERITED_KEY}",
			"EMPTY_CFG_KEY":  "",               // Should be omitted
			"OVERRIDDEN_KEY": "original-value", // Should be omitted because of override
		},
	}

	extraEnv := map[string]string{
		"EXTRA_KEY":       "extra-value",
		"OVERRIDDEN_KEY":  "", // Should cause omission
		"EMPTY_EXTRA_KEY": "", // Should be omitted
	}

	env, warnings, missingKeys := buildAgentEnv(scionCfg, extraEnv)

	expected := map[string]string{
		"NORMAL_KEY":    "normal-value",
		"INHERITED_KEY": "inherited-value",
		"EXTRA_KEY":     "extra-value",
	}

	envMap := make(map[string]string)
	for _, e := range env {
		parts := strings.SplitN(e, "=", 2)
		if len(parts) == 2 {
			envMap[parts[0]] = parts[1]
		}
	}

	if len(env) != len(expected) {
		t.Errorf("expected %d env vars, got %d: %v", len(expected), len(env), env)
	}

	if len(warnings) != 3 {
		t.Errorf("expected 3 warnings, got %d: %v", len(warnings), warnings)
	}

	if len(missingKeys) != 3 {
		t.Errorf("expected 3 missing keys, got %d: %v", len(missingKeys), missingKeys)
	}

	for k, v := range expected {
		if envMap[k] != v {
			t.Errorf("expected env[%s] = %q, got %q", k, v, envMap[k])
		}
	}

	// Explicitly check for omitted keys
	omitted := []string{"EMPTY_CFG_KEY", "OVERRIDDEN_KEY", "EMPTY_EXTRA_KEY"}
	for _, k := range omitted {
		if _, ok := envMap[k]; ok {
			t.Errorf("expected key %s to be omitted, but it was present", k)
		}
	}
}

func TestBuildAgentEnv_MissingKeysReturned(t *testing.T) {
	// Verify that buildAgentEnv returns the names of keys that could not
	// be resolved, so the caller can treat them as errors.
	scionCfg := &api.ScionConfig{
		Env: map[string]string{
			"GOOD_KEY":    "good-value",
			"MISSING_ONE": "",
			"MISSING_TWO": "",
		},
	}

	env, _, missingKeys := buildAgentEnv(scionCfg, nil)

	if len(env) != 1 {
		t.Errorf("expected 1 env var, got %d: %v", len(env), env)
	}
	if len(missingKeys) != 2 {
		t.Fatalf("expected 2 missing keys, got %d: %v", len(missingKeys), missingKeys)
	}

	sort.Strings(missingKeys)
	if missingKeys[0] != "MISSING_ONE" || missingKeys[1] != "MISSING_TWO" {
		t.Errorf("unexpected missing keys: %v", missingKeys)
	}
}

func TestStartBrokerMode_EmptyEnvNotFatal(t *testing.T) {
	// In broker mode, empty env vars from scion-agent.json that the hub
	// didn't resolve (e.g., profile-level keys irrelevant to the selected
	// harness) should produce warnings but NOT block agent start.
	tmpDir := t.TempDir()

	oldWd, _ := os.Getwd()
	os.Chdir(tmpDir)
	defer os.Chdir(oldWd)

	originalHome := os.Getenv("HOME")
	defer os.Setenv("HOME", originalHome)
	os.Setenv("HOME", tmpDir)

	globalScionDir := filepath.Join(tmpDir, ".scion")

	hcDir := filepath.Join(globalScionDir, "harness-configs", "test-harness")
	os.MkdirAll(hcDir, 0755)
	os.WriteFile(filepath.Join(hcDir, "config.yaml"), []byte("harness: gemini\nuser: scion\nimage: test-image:latest\n"), 0644)

	tplDir := filepath.Join(globalScionDir, "templates", "default")
	os.MkdirAll(tplDir, 0755)
	os.WriteFile(filepath.Join(tplDir, "scion-agent.json"), []byte(`{"default_harness_config": "test-harness"}`), 0644)

	os.WriteFile(filepath.Join(globalScionDir, "settings.yaml"), []byte(`schema_version: "1"
active_profile: local
profiles:
  local:
    runtime: docker
`), 0644)

	projectDir := filepath.Join(tmpDir, "project")
	projectScionDir := filepath.Join(projectDir, ".scion")
	os.MkdirAll(projectScionDir, 0755)

	var capturedEnv []string
	mockRT := &runtime.MockRuntime{
		ListFunc: func(ctx context.Context, labelFilter map[string]string) ([]api.AgentInfo, error) {
			return []api.AgentInfo{}, nil
		},
		RunFunc: func(ctx context.Context, config runtime.RunConfig) (string, error) {
			capturedEnv = config.Env
			return "mock-id", nil
		},
	}

	// Write scion-agent.json with empty env vars (simulating profile-level
	// passthrough markers that are irrelevant to the selected harness)
	agentDir := filepath.Join(projectScionDir, "agents", "broker-test")
	os.MkdirAll(filepath.Join(agentDir, "home"), 0755)
	os.WriteFile(filepath.Join(agentDir, "scion-agent.json"), []byte(`{
		"harness": "generic",
		"env": {
			"GEMINI_API_KEY": "",
			"OPENAI_API_KEY": "",
			"GOOD_KEY": "good-value"
		}
	}`), 0644)

	mgr := NewManager(mockRT)

	// In broker mode, empty env vars should NOT cause an error
	_, err := mgr.Start(context.Background(), api.StartOptions{
		Name:       "broker-test",
		GrovePath:  projectScionDir,
		BrokerMode: true,
		NoAuth:     true,
		Env: map[string]string{
			"GEMINI_API_KEY": "resolved-from-hub",
		},
	})
	if err != nil {
		t.Fatalf("Start in BrokerMode should not fail on empty env vars, got: %v", err)
	}

	// Verify GEMINI_API_KEY was resolved from hub env
	envMap := make(map[string]string)
	for _, e := range capturedEnv {
		parts := strings.SplitN(e, "=", 2)
		if len(parts) == 2 {
			envMap[parts[0]] = parts[1]
		}
	}
	if envMap["GEMINI_API_KEY"] != "resolved-from-hub" {
		t.Errorf("GEMINI_API_KEY = %q, want %q", envMap["GEMINI_API_KEY"], "resolved-from-hub")
	}
	if envMap["GOOD_KEY"] != "good-value" {
		t.Errorf("GOOD_KEY = %q, want %q", envMap["GOOD_KEY"], "good-value")
	}
	// OPENAI_API_KEY should be omitted (empty and not resolved)
	if _, ok := envMap["OPENAI_API_KEY"]; ok {
		t.Error("expected OPENAI_API_KEY to be omitted, but it was present")
	}
}

func TestStartLocalMode_EmptyEnvIsFatal(t *testing.T) {
	// In local (non-broker) mode, empty env vars that can't be resolved
	// should still cause a fatal error.
	tmpDir := t.TempDir()

	oldWd, _ := os.Getwd()
	os.Chdir(tmpDir)
	defer os.Chdir(oldWd)

	originalHome := os.Getenv("HOME")
	defer os.Setenv("HOME", originalHome)
	os.Setenv("HOME", tmpDir)

	globalScionDir := filepath.Join(tmpDir, ".scion")

	hcDir := filepath.Join(globalScionDir, "harness-configs", "test-harness")
	os.MkdirAll(hcDir, 0755)
	os.WriteFile(filepath.Join(hcDir, "config.yaml"), []byte("harness: gemini\nuser: scion\nimage: test-image:latest\n"), 0644)

	tplDir := filepath.Join(globalScionDir, "templates", "default")
	os.MkdirAll(tplDir, 0755)
	os.WriteFile(filepath.Join(tplDir, "scion-agent.json"), []byte(`{"default_harness_config": "test-harness"}`), 0644)

	os.WriteFile(filepath.Join(globalScionDir, "settings.yaml"), []byte(`schema_version: "1"
active_profile: local
profiles:
  local:
    runtime: docker
`), 0644)

	projectDir := filepath.Join(tmpDir, "project")
	projectScionDir := filepath.Join(projectDir, ".scion")
	os.MkdirAll(projectScionDir, 0755)

	mockRT := &runtime.MockRuntime{
		ListFunc: func(ctx context.Context, labelFilter map[string]string) ([]api.AgentInfo, error) {
			return []api.AgentInfo{}, nil
		},
		RunFunc: func(ctx context.Context, config runtime.RunConfig) (string, error) {
			return "mock-id", nil
		},
	}

	agentDir := filepath.Join(projectScionDir, "agents", "local-test")
	os.MkdirAll(filepath.Join(agentDir, "home"), 0755)
	os.WriteFile(filepath.Join(agentDir, "scion-agent.json"), []byte(`{
		"harness": "generic",
		"env": {
			"MISSING_KEY": ""
		}
	}`), 0644)

	mgr := NewManager(mockRT)

	// In local mode (BrokerMode=false), empty env vars should be fatal
	_, err := mgr.Start(context.Background(), api.StartOptions{
		Name:      "local-test",
		GrovePath: projectScionDir,
		NoAuth:    true,
	})
	if err == nil {
		t.Fatal("expected Start to fail on empty env vars in local mode")
	}
	if !strings.Contains(err.Error(), "MISSING_KEY") {
		t.Errorf("expected error to mention MISSING_KEY, got: %v", err)
	}
}

func TestBuildAgentEnv_EmptyValuePassthrough(t *testing.T) {
	// When a config env entry has an empty value (no ${VAR} reference),
	// buildAgentEnv should implicitly look up the host env var of the same name.
	os.Setenv("HOST_AVAILABLE_KEY", "host-value")
	defer os.Unsetenv("HOST_AVAILABLE_KEY")

	scionCfg := &api.ScionConfig{
		Env: map[string]string{
			"HOST_AVAILABLE_KEY": "", // empty → should pick up "host-value" from host
			"HOST_MISSING_KEY":   "", // empty → host doesn't have it → should be omitted
			"EXPLICIT_VALUE":     "explicit",
		},
	}

	env, warnings, missingKeys := buildAgentEnv(scionCfg, nil)

	envMap := make(map[string]string)
	for _, e := range env {
		parts := strings.SplitN(e, "=", 2)
		if len(parts) == 2 {
			envMap[parts[0]] = parts[1]
		}
	}

	if envMap["HOST_AVAILABLE_KEY"] != "host-value" {
		t.Errorf("expected HOST_AVAILABLE_KEY = %q, got %q", "host-value", envMap["HOST_AVAILABLE_KEY"])
	}
	if envMap["EXPLICIT_VALUE"] != "explicit" {
		t.Errorf("expected EXPLICIT_VALUE = %q, got %q", "explicit", envMap["EXPLICIT_VALUE"])
	}
	if _, ok := envMap["HOST_MISSING_KEY"]; ok {
		t.Error("expected HOST_MISSING_KEY to be omitted, but it was present")
	}

	// Only HOST_MISSING_KEY should produce a warning
	if len(warnings) != 1 {
		t.Errorf("expected 1 warning, got %d: %v", len(warnings), warnings)
	}
	if len(missingKeys) != 1 {
		t.Errorf("expected 1 missing key, got %d: %v", len(missingKeys), missingKeys)
	}
}

func TestBuildAgentEnv_ScionExtraPath(t *testing.T) {
	// SCION_EXTRA_PATH should pass through buildAgentEnv as a normal literal
	// env var (no special expansion needed since the value is a literal
	// container path like /home/scion/bin).
	scionCfg := &api.ScionConfig{
		Env: map[string]string{
			"SCION_EXTRA_PATH": "/home/scion/bin",
		},
	}

	env, warnings, _ := buildAgentEnv(scionCfg, nil)

	envMap := make(map[string]string)
	for _, e := range env {
		parts := strings.SplitN(e, "=", 2)
		if len(parts) == 2 {
			envMap[parts[0]] = parts[1]
		}
	}

	if got, ok := envMap["SCION_EXTRA_PATH"]; !ok {
		t.Error("expected SCION_EXTRA_PATH to be present in env")
	} else if got != "/home/scion/bin" {
		t.Errorf("SCION_EXTRA_PATH = %q, want %q", got, "/home/scion/bin")
	}

	// No warnings expected for a literal value
	for _, w := range warnings {
		if strings.Contains(w, "SCION_EXTRA_PATH") {
			t.Errorf("unexpected warning for SCION_EXTRA_PATH: %s", w)
		}
	}
}

func TestBuildAgentEnv_HubEndpointOverride(t *testing.T) {
	t.Run("scion config hub endpoint overrides extraEnv", func(t *testing.T) {
		scionCfg := &api.ScionConfig{
			Hub: &api.AgentHubConfig{
				Endpoint: "https://tunnel.example.com",
			},
		}

		// Simulate what Start() does: set hub endpoint in opts.Env from broker,
		// then override with scion config hub endpoint.
		extraEnv := map[string]string{
			"SCION_HUB_ENDPOINT": "http://localhost:9810",
			"SCION_HUB_URL":      "http://localhost:9810",
		}

		// Apply the override logic from Start()
		if scionCfg.Hub != nil && scionCfg.Hub.Endpoint != "" {
			extraEnv["SCION_HUB_ENDPOINT"] = scionCfg.Hub.Endpoint
			extraEnv["SCION_HUB_URL"] = scionCfg.Hub.Endpoint
		}

		env, _, _ := buildAgentEnv(scionCfg, extraEnv)

		envMap := make(map[string]string)
		for _, e := range env {
			parts := strings.SplitN(e, "=", 2)
			if len(parts) == 2 {
				envMap[parts[0]] = parts[1]
			}
		}

		if got := envMap["SCION_HUB_ENDPOINT"]; got != "https://tunnel.example.com" {
			t.Errorf("expected SCION_HUB_ENDPOINT='https://tunnel.example.com', got %q", got)
		}
		if got := envMap["SCION_HUB_URL"]; got != "https://tunnel.example.com" {
			t.Errorf("expected SCION_HUB_URL='https://tunnel.example.com', got %q", got)
		}
	})

	t.Run("no hub config preserves extraEnv", func(t *testing.T) {
		scionCfg := &api.ScionConfig{}
		extraEnv := map[string]string{
			"SCION_HUB_ENDPOINT": "https://hub.example.com",
			"SCION_HUB_URL":      "https://hub.example.com",
		}

		env, _, _ := buildAgentEnv(scionCfg, extraEnv)

		envMap := make(map[string]string)
		for _, e := range env {
			parts := strings.SplitN(e, "=", 2)
			if len(parts) == 2 {
				envMap[parts[0]] = parts[1]
			}
		}

		if got := envMap["SCION_HUB_ENDPOINT"]; got != "https://hub.example.com" {
			t.Errorf("expected SCION_HUB_ENDPOINT='https://hub.example.com', got %q", got)
		}
	})
}

func TestScionCreatorEnvVar(t *testing.T) {
	t.Run("SCION_CREATOR is set from OS user when not present", func(t *testing.T) {
		env := make(map[string]string)
		// Simulate the logic from Start(): if SCION_CREATOR is not set, set it from os/user
		if _, ok := env["SCION_CREATOR"]; !ok {
			if u, err := user.Current(); err == nil {
				env["SCION_CREATOR"] = u.Username
			}
		}

		if env["SCION_CREATOR"] == "" {
			t.Error("expected SCION_CREATOR to be set from OS user")
		}

		u, _ := user.Current()
		if env["SCION_CREATOR"] != u.Username {
			t.Errorf("expected SCION_CREATOR = %q, got %q", u.Username, env["SCION_CREATOR"])
		}
	})

	t.Run("SCION_CREATOR is preserved when already set", func(t *testing.T) {
		env := map[string]string{
			"SCION_CREATOR": "hub-user@example.com",
		}
		// Simulate the logic from Start(): if SCION_CREATOR is not set, set it from os/user
		if _, ok := env["SCION_CREATOR"]; !ok {
			if u, err := user.Current(); err == nil {
				env["SCION_CREATOR"] = u.Username
			}
		}

		if env["SCION_CREATOR"] != "hub-user@example.com" {
			t.Errorf("expected SCION_CREATOR = %q, got %q", "hub-user@example.com", env["SCION_CREATOR"])
		}
	})
}

func TestStartResumeNonExistentAgent(t *testing.T) {
	// Create a temporary directory to act as the grove
	tmpDir := t.TempDir()

	// Move to tmpDir to avoid being inside the project's git repo
	oldWd, _ := os.Getwd()
	os.Chdir(tmpDir)
	defer os.Chdir(oldWd)

	// Mock HOME for global settings
	originalHome := os.Getenv("HOME")
	defer os.Setenv("HOME", originalHome)
	os.Setenv("HOME", tmpDir)

	// Create .scion directory structure (minimum required)
	scionDir := filepath.Join(tmpDir, ".scion")
	if err := os.MkdirAll(scionDir, 0755); err != nil {
		t.Fatalf("failed to create .scion dir: %v", err)
	}

	// Create a mock runtime
	mockRuntime := &runtime.MockRuntime{
		ListFunc: func(ctx context.Context, labelFilter map[string]string) ([]api.AgentInfo, error) {
			return []api.AgentInfo{}, nil
		},
	}

	mgr := NewManager(mockRuntime)

	// Try to resume a non-existent agent
	opts := api.StartOptions{
		Name:      "non-existent-agent",
		GrovePath: scionDir,
		Resume:    true,
	}

	_, err := mgr.Start(context.Background(), opts)
	if err == nil {
		t.Fatal("expected error when resuming non-existent agent, got nil")
	}

	if !strings.Contains(err.Error(), "cannot resume agent") {
		t.Errorf("expected error message to contain 'cannot resume agent', got: %v", err)
	}

	if !strings.Contains(err.Error(), "does not exist") {
		t.Errorf("expected error message to contain 'does not exist', got: %v", err)
	}
}

func TestStartResolvesHarnessConfigUser(t *testing.T) {
	// Regression test: the container user (e.g. "scion") defined in the on-disk
	// harness-config config.yaml must flow into RunConfig.UnixUsername.
	// Previously, an empty User from settings.ResolveHarnessConfig() overwrote
	// the default, producing empty mount paths like /home//.config/gcloud.
	tmpDir := t.TempDir()

	oldWd, _ := os.Getwd()
	os.Chdir(tmpDir)
	defer os.Chdir(oldWd)

	originalHome := os.Getenv("HOME")
	defer os.Setenv("HOME", originalHome)
	os.Setenv("HOME", tmpDir)

	globalScionDir := filepath.Join(tmpDir, ".scion")

	// Create harness-config with user field
	hcDir := filepath.Join(globalScionDir, "harness-configs", "test-harness")
	os.MkdirAll(hcDir, 0755)
	os.WriteFile(filepath.Join(hcDir, "config.yaml"), []byte("harness: gemini\nuser: scion\nimage: test-image:latest\n"), 0644)

	// Create a minimal template
	tplDir := filepath.Join(globalScionDir, "templates", "default")
	os.MkdirAll(tplDir, 0755)
	os.WriteFile(filepath.Join(tplDir, "scion-agent.json"), []byte(`{"default_harness_config": "test-harness"}`), 0644)

	// Settings without harness_configs entries (simulating default_settings.yaml)
	os.WriteFile(filepath.Join(globalScionDir, "settings.yaml"), []byte(`schema_version: "1"
active_profile: local
profiles:
  local:
    runtime: docker
`), 0644)

	// Create project grove
	projectDir := filepath.Join(tmpDir, "project")
	projectScionDir := filepath.Join(projectDir, ".scion")
	os.MkdirAll(projectScionDir, 0755)

	// Capture the RunConfig
	var capturedConfig runtime.RunConfig
	mockRT := &runtime.MockRuntime{
		ListFunc: func(ctx context.Context, labelFilter map[string]string) ([]api.AgentInfo, error) {
			return []api.AgentInfo{}, nil
		},
		RunFunc: func(ctx context.Context, config runtime.RunConfig) (string, error) {
			capturedConfig = config
			return "mock-id", nil
		},
	}

	mgr := NewManager(mockRT)

	_, err := mgr.Start(context.Background(), api.StartOptions{
		Name:      "test-agent",
		GrovePath: projectScionDir,
		NoAuth:    true,
	})
	if err != nil {
		t.Fatalf("Start failed: %v", err)
	}

	if capturedConfig.UnixUsername != "scion" {
		t.Errorf("expected UnixUsername = %q, got %q", "scion", capturedConfig.UnixUsername)
	}
}

func TestStartResolvesHarnessConfigUserSettingsOverride(t *testing.T) {
	// When settings define a user in harness_configs, it should override
	// the on-disk harness-config user.
	tmpDir := t.TempDir()

	oldWd, _ := os.Getwd()
	os.Chdir(tmpDir)
	defer os.Chdir(oldWd)

	originalHome := os.Getenv("HOME")
	defer os.Setenv("HOME", originalHome)
	os.Setenv("HOME", tmpDir)

	globalScionDir := filepath.Join(tmpDir, ".scion")

	// Create harness-config with user field
	hcDir := filepath.Join(globalScionDir, "harness-configs", "test-harness")
	os.MkdirAll(hcDir, 0755)
	os.WriteFile(filepath.Join(hcDir, "config.yaml"), []byte("harness: gemini\nuser: scion\nimage: test-image:latest\n"), 0644)

	// Create a minimal template
	tplDir := filepath.Join(globalScionDir, "templates", "default")
	os.MkdirAll(tplDir, 0755)
	os.WriteFile(filepath.Join(tplDir, "scion-agent.json"), []byte(`{"default_harness_config": "test-harness"}`), 0644)

	// Settings WITH harness_configs that override the user
	os.WriteFile(filepath.Join(globalScionDir, "settings.yaml"), []byte(`schema_version: "1"
active_profile: local
harness_configs:
  test-harness:
    harness: gemini
    user: custom-user
    image: test-image:latest
profiles:
  local:
    runtime: docker
`), 0644)

	// Create project grove
	projectDir := filepath.Join(tmpDir, "project")
	projectScionDir := filepath.Join(projectDir, ".scion")
	os.MkdirAll(projectScionDir, 0755)

	// Capture the RunConfig
	var capturedConfig runtime.RunConfig
	mockRT := &runtime.MockRuntime{
		ListFunc: func(ctx context.Context, labelFilter map[string]string) ([]api.AgentInfo, error) {
			return []api.AgentInfo{}, nil
		},
		RunFunc: func(ctx context.Context, config runtime.RunConfig) (string, error) {
			capturedConfig = config
			return "mock-id", nil
		},
	}

	mgr := NewManager(mockRT)

	_, err := mgr.Start(context.Background(), api.StartOptions{
		Name:      "test-agent",
		GrovePath: projectScionDir,
		NoAuth:    true,
	})
	if err != nil {
		t.Fatalf("Start failed: %v", err)
	}

	if capturedConfig.UnixUsername != "custom-user" {
		t.Errorf("expected UnixUsername = %q, got %q", "custom-user", capturedConfig.UnixUsername)
	}
}

func TestStartResolvesHarnessConfigUserFromAbsTemplateDir(t *testing.T) {
	// Regression test: when opts.Template is an absolute path (e.g. a hydrated
	// template from the broker's template cache), the harness-config bundled
	// inside the template must be found and its User field applied. Previously,
	// the template path lookup in Start used the display name from agent-info.json
	// which could not be resolved in the grove, causing FindHarnessConfigDir to
	// miss the template-bundled harness config and defaulting to user "root".
	tmpDir := t.TempDir()

	oldWd, _ := os.Getwd()
	os.Chdir(tmpDir)
	defer os.Chdir(oldWd)

	originalHome := os.Getenv("HOME")
	defer os.Setenv("HOME", originalHome)
	os.Setenv("HOME", tmpDir)

	globalScionDir := filepath.Join(tmpDir, ".scion")

	// Create a template at an absolute path (simulating hydrated template cache)
	// with a bundled harness-config that has user: scion
	hydratedTplDir := filepath.Join(tmpDir, "template-cache", "web-dev")
	hcDir := filepath.Join(hydratedTplDir, "harness-configs", "claude-web")
	os.MkdirAll(hcDir, 0755)
	os.WriteFile(filepath.Join(hcDir, "config.yaml"), []byte("harness: claude\nuser: scion\nimage: scion-claude:latest\n"), 0644)
	os.MkdirAll(filepath.Join(hcDir, "home"), 0755)
	os.WriteFile(filepath.Join(hydratedTplDir, "scion-agent.json"), []byte(`{"default_harness_config": "claude-web"}`), 0644)

	// Minimal global settings (no harness_configs defined)
	os.MkdirAll(globalScionDir, 0755)
	os.WriteFile(filepath.Join(globalScionDir, "settings.yaml"), []byte(`schema_version: "1"
active_profile: local
profiles:
  local:
    runtime: docker
`), 0644)

	// Create project grove
	projectDir := filepath.Join(tmpDir, "project")
	projectScionDir := filepath.Join(projectDir, ".scion")
	os.MkdirAll(projectScionDir, 0755)

	// Capture the RunConfig
	var capturedConfig runtime.RunConfig
	mockRT := &runtime.MockRuntime{
		ListFunc: func(ctx context.Context, labelFilter map[string]string) ([]api.AgentInfo, error) {
			return []api.AgentInfo{}, nil
		},
		RunFunc: func(ctx context.Context, config runtime.RunConfig) (string, error) {
			capturedConfig = config
			return "mock-id", nil
		},
	}

	mgr := NewManager(mockRT)

	_, err := mgr.Start(context.Background(), api.StartOptions{
		Name:      "test-agent",
		Template:  hydratedTplDir, // absolute path, simulating hydrated template
		GrovePath: projectScionDir,
		NoAuth:    true,
	})
	if err != nil {
		t.Fatalf("Start failed: %v", err)
	}

	if capturedConfig.UnixUsername != "scion" {
		t.Errorf("expected UnixUsername = %q, got %q", "scion", capturedConfig.UnixUsername)
	}
}

func TestStartResolvesHarnessConfigFromNamedTemplate(t *testing.T) {
	// Regression test: when a non-default template bundles a custom harness-config
	// (e.g. .scion/templates/test4/harness-configs/claude2), the template name
	// stored in agent-info.json must be the derived template name (e.g. "test4"),
	// not the base "default" template. Previously, displayTemplateName used
	// chain[0].Name which was always "default" for non-default templates, causing
	// Start to fail to reconstruct the template chain and miss the bundled
	// harness-config.
	tmpDir := t.TempDir()

	oldWd, _ := os.Getwd()
	os.Chdir(tmpDir)
	defer os.Chdir(oldWd)

	originalHome := os.Getenv("HOME")
	defer os.Setenv("HOME", originalHome)
	os.Setenv("HOME", tmpDir)

	globalScionDir := filepath.Join(tmpDir, ".scion")

	// Create a "default" template (required as base layer)
	defaultTplDir := filepath.Join(globalScionDir, "templates", "default")
	os.MkdirAll(defaultTplDir, 0755)
	os.WriteFile(filepath.Join(defaultTplDir, "scion-agent.yaml"), []byte("default_harness_config: claude\n"), 0644)

	// Seed the default "claude" harness-config at global level
	claudeHcDir := filepath.Join(globalScionDir, "harness-configs", "claude")
	os.MkdirAll(filepath.Join(claudeHcDir, "home"), 0755)
	os.WriteFile(filepath.Join(claudeHcDir, "config.yaml"), []byte("harness: claude\nuser: scion\nimage: scion-claude:latest\n"), 0644)

	// Create a non-default template "test4" with a bundled harness-config "claude2"
	test4TplDir := filepath.Join(globalScionDir, "templates", "test4")
	os.MkdirAll(test4TplDir, 0755)
	os.WriteFile(filepath.Join(test4TplDir, "scion-agent.yaml"), []byte("default_harness_config: claude2\n"), 0644)

	claude2HcDir := filepath.Join(test4TplDir, "harness-configs", "claude2")
	os.MkdirAll(filepath.Join(claude2HcDir, "home"), 0755)
	os.WriteFile(filepath.Join(claude2HcDir, "config.yaml"), []byte("harness: claude\nuser: scion\nimage: custom-claude:latest\n"), 0644)

	// Minimal global settings
	os.WriteFile(filepath.Join(globalScionDir, "settings.yaml"), []byte(`schema_version: "1"
active_profile: local
profiles:
  local:
    runtime: docker
`), 0644)

	// Create project grove
	projectDir := filepath.Join(tmpDir, "project")
	projectScionDir := filepath.Join(projectDir, ".scion")
	os.MkdirAll(projectScionDir, 0755)

	// Capture the RunConfig
	var capturedConfig runtime.RunConfig
	mockRT := &runtime.MockRuntime{
		ListFunc: func(ctx context.Context, labelFilter map[string]string) ([]api.AgentInfo, error) {
			return []api.AgentInfo{}, nil
		},
		RunFunc: func(ctx context.Context, config runtime.RunConfig) (string, error) {
			capturedConfig = config
			return "mock-id", nil
		},
	}

	mgr := NewManager(mockRT)

	_, err := mgr.Start(context.Background(), api.StartOptions{
		Name:      "test-agent",
		Template:  "test4",
		GrovePath: projectScionDir,
		NoAuth:    true,
	})
	if err != nil {
		t.Fatalf("Start failed: %v", err)
	}

	// The harness-config "claude2" specifies image "custom-claude:latest"
	// If the template name was incorrectly stored as "default", this would
	// fall back to the default image instead.
	if capturedConfig.UnixUsername != "scion" {
		t.Errorf("expected UnixUsername = %q, got %q", "scion", capturedConfig.UnixUsername)
	}
}

func TestStartReturnsRunningStatus(t *testing.T) {
	// This tests the early-return path when a container is already running.
	// The runtime's List() may return a stale Status (e.g. "created") from the
	// container runtime, but Start() should override it to "running" since
	// isRunning is confirmed true via ContainerStatus.
	mockRT := &runtime.MockRuntime{
		ListFunc: func(ctx context.Context, labelFilter map[string]string) ([]api.AgentInfo, error) {
			return []api.AgentInfo{
				{
					ContainerID:     "abc123",
					Name:            "test-agent",
					ContainerStatus: "Up 2 hours",
					Phase:           "created", // stale phase from runtime
				},
			}, nil
		},
	}

	mgr := NewManager(mockRT)

	result, err := mgr.Start(context.Background(), api.StartOptions{
		Name: "test-agent",
		// No Task — triggers the early return for already-running containers
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if result.Phase != "running" {
		t.Errorf("expected Phase = %q, got %q", "running", result.Phase)
	}
}

func TestBuildAgentEnv_TelemetryInjection(t *testing.T) {
	// Simulate the telemetry injection that Start() performs before buildAgentEnv.
	enabled := true
	cloudEnabled := true
	insecure := false

	scionCfg := &api.ScionConfig{
		Telemetry: &api.TelemetryConfig{
			Enabled: &enabled,
			Cloud: &api.TelemetryCloudConfig{
				Enabled:  &cloudEnabled,
				Endpoint: "otel.example.com:4317",
				Protocol: "grpc",
				TLS: &api.TelemetryTLS{
					InsecureSkipVerify: &insecure,
				},
			},
		},
	}

	opts := make(map[string]string)

	// Replicate the injection logic from Start()
	if scionCfg.Telemetry != nil {
		telemetryEnv := config.TelemetryConfigToEnv(scionCfg.Telemetry)
		for k, v := range telemetryEnv {
			if _, exists := opts[k]; !exists {
				opts[k] = v
			}
		}
	}

	env, _, _ := buildAgentEnv(scionCfg, opts)

	envMap := make(map[string]string)
	for _, e := range env {
		parts := strings.SplitN(e, "=", 2)
		if len(parts) == 2 {
			envMap[parts[0]] = parts[1]
		}
	}

	expected := map[string]string{
		"SCION_TELEMETRY_ENABLED":       "true",
		"SCION_TELEMETRY_CLOUD_ENABLED": "true",
		"SCION_OTEL_ENDPOINT":           "otel.example.com:4317",
		"SCION_OTEL_PROTOCOL":           "grpc",
		"SCION_OTEL_INSECURE":           "false",
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

func TestTelemetryEnabledFlag(t *testing.T) {
	// Verify the TelemetryEnabled derivation logic used in Start().
	// telemetryEnabled = cfg != nil && cfg.Telemetry != nil &&
	//   (cfg.Telemetry.Enabled == nil || *cfg.Telemetry.Enabled)

	boolPtr := func(b bool) *bool { return &b }

	tests := []struct {
		name     string
		cfg      *api.ScionConfig
		expected bool
	}{
		{
			name:     "nil config",
			cfg:      nil,
			expected: false,
		},
		{
			name:     "nil telemetry",
			cfg:      &api.ScionConfig{},
			expected: false,
		},
		{
			name:     "telemetry enabled nil (default on)",
			cfg:      &api.ScionConfig{Telemetry: &api.TelemetryConfig{}},
			expected: true,
		},
		{
			name:     "telemetry explicitly enabled",
			cfg:      &api.ScionConfig{Telemetry: &api.TelemetryConfig{Enabled: boolPtr(true)}},
			expected: true,
		},
		{
			name:     "telemetry explicitly disabled",
			cfg:      &api.ScionConfig{Telemetry: &api.TelemetryConfig{Enabled: boolPtr(false)}},
			expected: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := tt.cfg != nil && tt.cfg.Telemetry != nil &&
				(tt.cfg.Telemetry.Enabled == nil || *tt.cfg.Telemetry.Enabled)
			if result != tt.expected {
				t.Errorf("telemetryEnabled = %v, want %v", result, tt.expected)
			}
		})
	}
}

func TestTaskFlagRunConfig(t *testing.T) {
	// Verify that when task_flag is set in scion-agent.json, the task is
	// delivered via CommandArgs (as a flag) instead of as a positional arg,
	// and RunConfig.Task is empty.
	tmpDir := t.TempDir()

	oldWd, _ := os.Getwd()
	os.Chdir(tmpDir)
	defer os.Chdir(oldWd)

	originalHome := os.Getenv("HOME")
	defer os.Setenv("HOME", originalHome)
	os.Setenv("HOME", tmpDir)

	globalScionDir := filepath.Join(tmpDir, ".scion")

	// Create harness-config
	hcDir := filepath.Join(globalScionDir, "harness-configs", "test-harness")
	os.MkdirAll(hcDir, 0755)
	os.WriteFile(filepath.Join(hcDir, "config.yaml"), []byte("harness: generic\nuser: scion\nimage: test-image:latest\n"), 0644)

	// Create template
	tplDir := filepath.Join(globalScionDir, "templates", "default")
	os.MkdirAll(tplDir, 0755)
	os.WriteFile(filepath.Join(tplDir, "scion-agent.json"), []byte(`{"default_harness_config": "test-harness"}`), 0644)

	os.WriteFile(filepath.Join(globalScionDir, "settings.yaml"), []byte(`schema_version: "1"
active_profile: local
profiles:
  local:
    runtime: docker
`), 0644)

	projectDir := filepath.Join(tmpDir, "project")
	projectScionDir := filepath.Join(projectDir, ".scion")
	os.MkdirAll(projectScionDir, 0755)

	t.Run("task_flag moves task into CommandArgs", func(t *testing.T) {
		var capturedConfig runtime.RunConfig
		mockRT := &runtime.MockRuntime{
			ListFunc: func(ctx context.Context, labelFilter map[string]string) ([]api.AgentInfo, error) {
				return []api.AgentInfo{}, nil
			},
			RunFunc: func(ctx context.Context, config runtime.RunConfig) (string, error) {
				capturedConfig = config
				return "mock-id", nil
			},
		}

		agentDir := filepath.Join(projectScionDir, "agents", "flag-test")
		os.MkdirAll(filepath.Join(agentDir, "home"), 0755)
		os.WriteFile(filepath.Join(agentDir, "scion-agent.json"), []byte(`{
			"harness": "generic",
			"task_flag": "--input",
			"command_args": ["adk", "run", "/opt/agent"]
		}`), 0644)

		mgr := NewManager(mockRT)
		_, err := mgr.Start(context.Background(), api.StartOptions{
			Name:      "flag-test",
			GrovePath: projectScionDir,
			Task:      "do something",
			NoAuth:    true,
		})
		if err != nil {
			t.Fatalf("Start failed: %v", err)
		}

		// Task should be empty since it's delivered via CommandArgs
		if capturedConfig.Task != "" {
			t.Errorf("expected Task='', got %q", capturedConfig.Task)
		}

		// CommandArgs should contain the task flag and value
		args := capturedConfig.CommandArgs
		found := false
		for i, arg := range args {
			if arg == "--input" && i+1 < len(args) && args[i+1] == "do something" {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("expected CommandArgs to contain '--input', 'do something', got %v", args)
		}
	})

	t.Run("no task_flag passes task normally", func(t *testing.T) {
		var capturedConfig runtime.RunConfig
		mockRT := &runtime.MockRuntime{
			ListFunc: func(ctx context.Context, labelFilter map[string]string) ([]api.AgentInfo, error) {
				return []api.AgentInfo{}, nil
			},
			RunFunc: func(ctx context.Context, config runtime.RunConfig) (string, error) {
				capturedConfig = config
				return "mock-id", nil
			},
		}

		agentDir := filepath.Join(projectScionDir, "agents", "noflag-test")
		os.MkdirAll(filepath.Join(agentDir, "home"), 0755)
		os.WriteFile(filepath.Join(agentDir, "scion-agent.json"), []byte(`{
			"harness": "generic",
			"command_args": ["adk", "run", "/opt/agent"]
		}`), 0644)

		mgr := NewManager(mockRT)
		_, err := mgr.Start(context.Background(), api.StartOptions{
			Name:      "noflag-test",
			GrovePath: projectScionDir,
			Task:      "do something",
			NoAuth:    true,
		})
		if err != nil {
			t.Fatalf("Start failed: %v", err)
		}

		// Task should be passed directly
		if capturedConfig.Task != "do something" {
			t.Errorf("expected Task='do something', got %q", capturedConfig.Task)
		}

		// CommandArgs should NOT contain task
		for _, arg := range capturedConfig.CommandArgs {
			if arg == "do something" {
				t.Error("expected CommandArgs to NOT contain the task text when task_flag is not set")
			}
		}
	})
}

func TestTelemetryEnabledRunConfig(t *testing.T) {
	// Integration test: verify that harness telemetry env vars appear in
	// RunConfig when telemetry is enabled, and are absent when disabled.
	tmpDir := t.TempDir()

	oldWd, _ := os.Getwd()
	os.Chdir(tmpDir)
	defer os.Chdir(oldWd)

	originalHome := os.Getenv("HOME")
	defer os.Setenv("HOME", originalHome)
	os.Setenv("HOME", tmpDir)

	globalScionDir := filepath.Join(tmpDir, ".scion")

	// Create harness-config
	hcDir := filepath.Join(globalScionDir, "harness-configs", "test-harness")
	os.MkdirAll(hcDir, 0755)
	os.WriteFile(filepath.Join(hcDir, "config.yaml"), []byte("harness: gemini\nuser: scion\nimage: test-image:latest\n"), 0644)

	// Create template
	tplDir := filepath.Join(globalScionDir, "templates", "default")
	os.MkdirAll(tplDir, 0755)
	os.WriteFile(filepath.Join(tplDir, "scion-agent.json"), []byte(`{"default_harness_config": "test-harness"}`), 0644)

	os.WriteFile(filepath.Join(globalScionDir, "settings.yaml"), []byte(`schema_version: "1"
active_profile: local
profiles:
  local:
    runtime: docker
`), 0644)

	projectDir := filepath.Join(tmpDir, "project")
	projectScionDir := filepath.Join(projectDir, ".scion")
	os.MkdirAll(projectScionDir, 0755)

	t.Run("telemetry enabled passes TelemetryEnabled to RunConfig", func(t *testing.T) {
		var capturedConfig runtime.RunConfig
		mockRT := &runtime.MockRuntime{
			ListFunc: func(ctx context.Context, labelFilter map[string]string) ([]api.AgentInfo, error) {
				return []api.AgentInfo{}, nil
			},
			RunFunc: func(ctx context.Context, config runtime.RunConfig) (string, error) {
				capturedConfig = config
				return "mock-id", nil
			},
		}

		// Create agent with telemetry enabled in scion-agent.json
		agentDir := filepath.Join(projectScionDir, "agents", "telem-on")
		os.MkdirAll(filepath.Join(agentDir, "home"), 0755)
		os.WriteFile(filepath.Join(agentDir, "scion-agent.json"), []byte(`{
			"harness": "gemini",
			"telemetry": {"enabled": true}
		}`), 0644)

		mgr := NewManager(mockRT)
		_, err := mgr.Start(context.Background(), api.StartOptions{
			Name:      "telem-on",
			GrovePath: projectScionDir,
			NoAuth:    true,
		})
		if err != nil {
			t.Fatalf("Start failed: %v", err)
		}

		if !capturedConfig.TelemetryEnabled {
			t.Error("expected TelemetryEnabled = true, got false")
		}
	})

	t.Run("telemetry disabled omits TelemetryEnabled from RunConfig", func(t *testing.T) {
		var capturedConfig runtime.RunConfig
		mockRT := &runtime.MockRuntime{
			ListFunc: func(ctx context.Context, labelFilter map[string]string) ([]api.AgentInfo, error) {
				return []api.AgentInfo{}, nil
			},
			RunFunc: func(ctx context.Context, config runtime.RunConfig) (string, error) {
				capturedConfig = config
				return "mock-id", nil
			},
		}

		agentDir := filepath.Join(projectScionDir, "agents", "telem-off")
		os.MkdirAll(filepath.Join(agentDir, "home"), 0755)
		os.WriteFile(filepath.Join(agentDir, "scion-agent.json"), []byte(`{
			"harness": "gemini",
			"telemetry": {"enabled": false}
		}`), 0644)

		mgr := NewManager(mockRT)
		_, err := mgr.Start(context.Background(), api.StartOptions{
			Name:      "telem-off",
			GrovePath: projectScionDir,
			NoAuth:    true,
		})
		if err != nil {
			t.Fatalf("Start failed: %v", err)
		}

		if capturedConfig.TelemetryEnabled {
			t.Error("expected TelemetryEnabled = false, got true")
		}
	})
}

func TestTelemetryOverrideFlag(t *testing.T) {
	// Verify that TelemetryOverride in StartOptions takes highest priority,
	// overriding the value from scion-agent.json.
	tmpDir := t.TempDir()

	oldWd, _ := os.Getwd()
	os.Chdir(tmpDir)
	defer os.Chdir(oldWd)

	originalHome := os.Getenv("HOME")
	defer os.Setenv("HOME", originalHome)
	os.Setenv("HOME", tmpDir)

	globalScionDir := filepath.Join(tmpDir, ".scion")

	hcDir := filepath.Join(globalScionDir, "harness-configs", "test-harness")
	os.MkdirAll(hcDir, 0755)
	os.WriteFile(filepath.Join(hcDir, "config.yaml"), []byte("harness: gemini\nuser: scion\nimage: test-image:latest\n"), 0644)

	tplDir := filepath.Join(globalScionDir, "templates", "default")
	os.MkdirAll(tplDir, 0755)
	os.WriteFile(filepath.Join(tplDir, "scion-agent.json"), []byte(`{"default_harness_config": "test-harness"}`), 0644)

	os.WriteFile(filepath.Join(globalScionDir, "settings.yaml"), []byte(`schema_version: "1"
active_profile: local
profiles:
  local:
    runtime: docker
`), 0644)

	projectDir := filepath.Join(tmpDir, "project")
	projectScionDir := filepath.Join(projectDir, ".scion")
	os.MkdirAll(projectScionDir, 0755)

	boolPtr := func(b bool) *bool { return &b }

	t.Run("override enables telemetry when config disables it", func(t *testing.T) {
		var capturedConfig runtime.RunConfig
		mockRT := &runtime.MockRuntime{
			ListFunc: func(ctx context.Context, labelFilter map[string]string) ([]api.AgentInfo, error) {
				return []api.AgentInfo{}, nil
			},
			RunFunc: func(ctx context.Context, config runtime.RunConfig) (string, error) {
				capturedConfig = config
				return "mock-id", nil
			},
		}

		agentDir := filepath.Join(projectScionDir, "agents", "override-enable")
		os.MkdirAll(filepath.Join(agentDir, "home"), 0755)
		os.WriteFile(filepath.Join(agentDir, "scion-agent.json"), []byte(`{
			"harness": "gemini",
			"telemetry": {"enabled": false}
		}`), 0644)

		mgr := NewManager(mockRT)
		_, err := mgr.Start(context.Background(), api.StartOptions{
			Name:              "override-enable",
			GrovePath:         projectScionDir,
			NoAuth:            true,
			TelemetryOverride: boolPtr(true),
		})
		if err != nil {
			t.Fatalf("Start failed: %v", err)
		}

		if !capturedConfig.TelemetryEnabled {
			t.Error("expected TelemetryEnabled = true (override should win), got false")
		}
	})

	t.Run("override disables telemetry when config enables it", func(t *testing.T) {
		var capturedConfig runtime.RunConfig
		mockRT := &runtime.MockRuntime{
			ListFunc: func(ctx context.Context, labelFilter map[string]string) ([]api.AgentInfo, error) {
				return []api.AgentInfo{}, nil
			},
			RunFunc: func(ctx context.Context, config runtime.RunConfig) (string, error) {
				capturedConfig = config
				return "mock-id", nil
			},
		}

		agentDir := filepath.Join(projectScionDir, "agents", "override-disable")
		os.MkdirAll(filepath.Join(agentDir, "home"), 0755)
		os.WriteFile(filepath.Join(agentDir, "scion-agent.json"), []byte(`{
			"harness": "gemini",
			"telemetry": {"enabled": true}
		}`), 0644)

		mgr := NewManager(mockRT)
		_, err := mgr.Start(context.Background(), api.StartOptions{
			Name:              "override-disable",
			GrovePath:         projectScionDir,
			NoAuth:            true,
			TelemetryOverride: boolPtr(false),
		})
		if err != nil {
			t.Fatalf("Start failed: %v", err)
		}

		if capturedConfig.TelemetryEnabled {
			t.Error("expected TelemetryEnabled = false (override should win), got true")
		}
	})

	t.Run("override enables telemetry when no telemetry config exists", func(t *testing.T) {
		var capturedConfig runtime.RunConfig
		mockRT := &runtime.MockRuntime{
			ListFunc: func(ctx context.Context, labelFilter map[string]string) ([]api.AgentInfo, error) {
				return []api.AgentInfo{}, nil
			},
			RunFunc: func(ctx context.Context, config runtime.RunConfig) (string, error) {
				capturedConfig = config
				return "mock-id", nil
			},
		}

		agentDir := filepath.Join(projectScionDir, "agents", "override-no-config")
		os.MkdirAll(filepath.Join(agentDir, "home"), 0755)
		os.WriteFile(filepath.Join(agentDir, "scion-agent.json"), []byte(`{
			"harness": "gemini"
		}`), 0644)

		mgr := NewManager(mockRT)
		_, err := mgr.Start(context.Background(), api.StartOptions{
			Name:              "override-no-config",
			GrovePath:         projectScionDir,
			NoAuth:            true,
			TelemetryOverride: boolPtr(true),
		})
		if err != nil {
			t.Fatalf("Start failed: %v", err)
		}

		if !capturedConfig.TelemetryEnabled {
			t.Error("expected TelemetryEnabled = true (override should create telemetry config), got false")
		}
	})
}

func TestSettingsTelemetryMergedIntoStart(t *testing.T) {
	// Verify that telemetry cloud config from settings.yaml gets merged into
	// the container env vars during Start(), enabling cloud export.
	tmpDir := t.TempDir()

	oldWd, _ := os.Getwd()
	os.Chdir(tmpDir)
	defer os.Chdir(oldWd)

	originalHome := os.Getenv("HOME")
	defer os.Setenv("HOME", originalHome)
	os.Setenv("HOME", tmpDir)

	globalScionDir := filepath.Join(tmpDir, ".scion")

	hcDir := filepath.Join(globalScionDir, "harness-configs", "test-harness")
	os.MkdirAll(hcDir, 0755)
	os.WriteFile(filepath.Join(hcDir, "config.yaml"), []byte("harness: gemini\nuser: scion\nimage: test-image:latest\n"), 0644)

	tplDir := filepath.Join(globalScionDir, "templates", "default")
	os.MkdirAll(tplDir, 0755)
	os.WriteFile(filepath.Join(tplDir, "scion-agent.json"), []byte(`{"default_harness_config": "test-harness"}`), 0644)

	// Settings with telemetry cloud config but telemetry.enabled: false
	// (the override should enable it)
	os.WriteFile(filepath.Join(globalScionDir, "settings.yaml"), []byte(`schema_version: "1"
active_profile: local
profiles:
  local:
    runtime: docker
telemetry:
  enabled: false
  cloud:
    enabled: true
    endpoint: otel-collector.example.com:4317
    protocol: grpc
`), 0644)

	projectDir := filepath.Join(tmpDir, "project")
	projectScionDir := filepath.Join(projectDir, ".scion")
	os.MkdirAll(projectScionDir, 0755)

	boolPtr := func(b bool) *bool { return &b }

	var capturedConfig runtime.RunConfig
	mockRT := &runtime.MockRuntime{
		ListFunc: func(ctx context.Context, labelFilter map[string]string) ([]api.AgentInfo, error) {
			return []api.AgentInfo{}, nil
		},
		RunFunc: func(ctx context.Context, config runtime.RunConfig) (string, error) {
			capturedConfig = config
			return "mock-id", nil
		},
	}

	agentDir := filepath.Join(projectScionDir, "agents", "settings-telem")
	os.MkdirAll(filepath.Join(agentDir, "home"), 0755)
	os.WriteFile(filepath.Join(agentDir, "scion-agent.json"), []byte(`{
		"harness": "gemini"
	}`), 0644)

	mgr := NewManager(mockRT)
	env := make(map[string]string)
	_, err := mgr.Start(context.Background(), api.StartOptions{
		Name:              "settings-telem",
		GrovePath:         projectScionDir,
		NoAuth:            true,
		TelemetryOverride: boolPtr(true),
		Env:               env,
	})
	if err != nil {
		t.Fatalf("Start failed: %v", err)
	}

	if !capturedConfig.TelemetryEnabled {
		t.Error("expected TelemetryEnabled = true")
	}

	// Verify that cloud config env vars from settings were injected
	if got := env["SCION_OTEL_ENDPOINT"]; got != "otel-collector.example.com:4317" {
		t.Errorf("SCION_OTEL_ENDPOINT = %q, want %q", got, "otel-collector.example.com:4317")
	}
	if got := env["SCION_OTEL_PROTOCOL"]; got != "grpc" {
		t.Errorf("SCION_OTEL_PROTOCOL = %q, want %q", got, "grpc")
	}
	if got := env["SCION_TELEMETRY_CLOUD_ENABLED"]; got != "true" {
		t.Errorf("SCION_TELEMETRY_CLOUD_ENABLED = %q, want %q", got, "true")
	}
}

func TestHarnessAuthOverrideFlag(t *testing.T) {
	// Verify that HarnessAuth in StartOptions takes highest priority,
	// overriding the auth_selected_type from scion-agent.json.
	tmpDir := t.TempDir()

	oldWd, _ := os.Getwd()
	os.Chdir(tmpDir)
	defer os.Chdir(oldWd)

	originalHome := os.Getenv("HOME")
	defer os.Setenv("HOME", originalHome)
	os.Setenv("HOME", tmpDir)

	globalScionDir := filepath.Join(tmpDir, ".scion")

	hcDir := filepath.Join(globalScionDir, "harness-configs", "test-harness")
	os.MkdirAll(hcDir, 0755)
	os.WriteFile(filepath.Join(hcDir, "config.yaml"), []byte("harness: gemini\nuser: scion\nimage: test-image:latest\n"), 0644)

	tplDir := filepath.Join(globalScionDir, "templates", "default")
	os.MkdirAll(tplDir, 0755)
	os.WriteFile(filepath.Join(tplDir, "scion-agent.json"), []byte(`{"default_harness_config": "test-harness"}`), 0644)

	os.WriteFile(filepath.Join(globalScionDir, "settings.yaml"), []byte(`schema_version: "1"
active_profile: local
profiles:
  local:
    runtime: docker
`), 0644)

	projectDir := filepath.Join(tmpDir, "project")
	projectScionDir := filepath.Join(projectDir, ".scion")
	os.MkdirAll(projectScionDir, 0755)

	t.Run("override changes auth_selected_type from api-key to vertex-ai", func(t *testing.T) {
		mockRT := &runtime.MockRuntime{
			ListFunc: func(ctx context.Context, labelFilter map[string]string) ([]api.AgentInfo, error) {
				return []api.AgentInfo{}, nil
			},
			RunFunc: func(ctx context.Context, config runtime.RunConfig) (string, error) {
				return "mock-id", nil
			},
		}

		agentDir := filepath.Join(projectScionDir, "agents", "auth-override")
		os.MkdirAll(filepath.Join(agentDir, "home"), 0755)
		os.WriteFile(filepath.Join(agentDir, "scion-agent.json"), []byte(`{
			"harness": "gemini",
			"auth_selectedType": "api-key"
		}`), 0644)

		mgr := NewManager(mockRT)
		_, err := mgr.Start(context.Background(), api.StartOptions{
			Name:        "auth-override",
			GrovePath:   projectScionDir,
			NoAuth:      true,
			HarnessAuth: "vertex-ai",
		})
		if err != nil {
			t.Fatalf("Start failed: %v", err)
		}

		// The override is applied in-memory to finalScionCfg.AuthSelectedType
		// before container launch. Verify the scion-agent.json was updated.
		data, err := os.ReadFile(filepath.Join(agentDir, "scion-agent.json"))
		if err != nil {
			t.Fatalf("failed to read scion-agent.json: %v", err)
		}
		if !strings.Contains(string(data), `"vertex-ai"`) {
			t.Errorf("expected scion-agent.json to contain vertex-ai, got: %s", string(data))
		}
	})
}

func TestBuildAgentEnv_TelemetryNoOverrideExplicit(t *testing.T) {
	// Explicit opts.Env values must not be overwritten by telemetry config.
	enabled := true

	scionCfg := &api.ScionConfig{
		Telemetry: &api.TelemetryConfig{
			Enabled: &enabled,
			Cloud: &api.TelemetryCloudConfig{
				Endpoint: "from-config.example.com:4317",
			},
		},
	}

	// Pre-set an explicit override in opts.Env (e.g. from Hub/broker)
	opts := map[string]string{
		"SCION_OTEL_ENDPOINT": "from-broker.example.com:4317",
	}

	// Replicate the injection logic from Start()
	if scionCfg.Telemetry != nil {
		telemetryEnv := config.TelemetryConfigToEnv(scionCfg.Telemetry)
		for k, v := range telemetryEnv {
			if _, exists := opts[k]; !exists {
				opts[k] = v
			}
		}
	}

	env, _, _ := buildAgentEnv(scionCfg, opts)

	envMap := make(map[string]string)
	for _, e := range env {
		parts := strings.SplitN(e, "=", 2)
		if len(parts) == 2 {
			envMap[parts[0]] = parts[1]
		}
	}

	// The broker's explicit value should win
	if got := envMap["SCION_OTEL_ENDPOINT"]; got != "from-broker.example.com:4317" {
		t.Errorf("SCION_OTEL_ENDPOINT = %q, want %q (explicit override should win)",
			got, "from-broker.example.com:4317")
	}

	// But the telemetry-derived enabled var should still be present
	if got := envMap["SCION_TELEMETRY_ENABLED"]; got != "true" {
		t.Errorf("SCION_TELEMETRY_ENABLED = %q, want %q", got, "true")
	}
}

func TestBuildAgentEnv_HubEnvVarsSurviveMerge(t *testing.T) {
	// Verify that hub env vars injected into opts.Env (from grove settings
	// or dev token resolution) survive the buildAgentEnv merge.
	scionCfg := &api.ScionConfig{}
	extraEnv := map[string]string{
		"SCION_HUB_ENDPOINT": "http://localhost:9810",
		"SCION_HUB_URL":      "http://localhost:9810",
		"SCION_AUTH_TOKEN":   "scion-dev-test-token-123",
		"SCION_AGENT_NAME":   "test-agent",
	}

	env, _, _ := buildAgentEnv(scionCfg, extraEnv)

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

func TestBuildAuthEnvOverlay_DoesNotMutateBaseEnv(t *testing.T) {
	baseEnv := map[string]string{
		"EXISTING_KEY": "existing-value",
		"API_KEY":      "explicit-value",
	}
	secrets := []api.ResolvedSecret{
		{
			Name:   "API_KEY",
			Type:   "environment",
			Target: "API_KEY",
			Value:  "secret-value",
			Source: "user",
		},
		{
			Name:   "GEMINI_API_KEY",
			Type:   "environment",
			Target: "GEMINI_API_KEY",
			Value:  "secret-api-key-value",
			Source: "user",
		},
	}

	overlay := buildAuthEnvOverlay(baseEnv, secrets)

	if baseEnv["API_KEY"] != "explicit-value" {
		t.Errorf("base env mutated: API_KEY = %q, want %q", baseEnv["API_KEY"], "explicit-value")
	}
	if _, ok := baseEnv["GEMINI_API_KEY"]; ok {
		t.Error("base env mutated: unexpected GEMINI_API_KEY entry")
	}
	if overlay["API_KEY"] != "explicit-value" {
		t.Errorf("overlay API_KEY = %q, want %q", overlay["API_KEY"], "explicit-value")
	}
	if overlay["GEMINI_API_KEY"] != "secret-api-key-value" {
		t.Errorf("overlay GEMINI_API_KEY = %q, want %q", overlay["GEMINI_API_KEY"], "secret-api-key-value")
	}
}

func TestBuildAuthEnvOverlay_EmptyValueOverriddenBySecret(t *testing.T) {
	// Empty-value passthrough markers in baseEnv should be overridden by
	// secrets so that auth resolution can detect the credential.
	baseEnv := map[string]string{
		"GEMINI_API_KEY": "",
		"EXISTING_KEY":   "keep-me",
	}
	secrets := []api.ResolvedSecret{
		{
			Name:   "GEMINI_API_KEY",
			Type:   "environment",
			Target: "GEMINI_API_KEY",
			Value:  "secret-api-key-value",
			Source: "user",
		},
	}

	overlay := buildAuthEnvOverlay(baseEnv, secrets)

	if overlay["GEMINI_API_KEY"] != "secret-api-key-value" {
		t.Errorf("overlay GEMINI_API_KEY = %q, want %q (secret should override empty passthrough)", overlay["GEMINI_API_KEY"], "secret-api-key-value")
	}
	if overlay["EXISTING_KEY"] != "keep-me" {
		t.Errorf("overlay EXISTING_KEY = %q, want %q", overlay["EXISTING_KEY"], "keep-me")
	}
	// Ensure baseEnv was not mutated
	if baseEnv["GEMINI_API_KEY"] != "" {
		t.Error("base env mutated: GEMINI_API_KEY should still be empty")
	}
}

func TestFilterResolvedSecretsForResolvedAuth(t *testing.T) {
	secrets := []api.ResolvedSecret{
		{Name: "GEMINI_API_KEY", Type: "environment", Target: "GEMINI_API_KEY", Value: "gemini"},
		{Name: "GOOGLE_APPLICATION_CREDENTIALS", Type: "file", Target: "/home/scion/.config/gcloud/application_default_credentials.json", Value: "adc"},
		{Name: "NOT_AUTH_SECRET", Type: "environment", Target: "NOT_AUTH_SECRET", Value: "keep"},
	}
	resolved := &api.ResolvedAuth{
		Method: "api-key",
		EnvVars: map[string]string{
			"GEMINI_API_KEY": "gemini",
		},
	}

	filtered := filterResolvedSecretsForResolvedAuth(secrets, resolved)
	if len(filtered) != 2 {
		t.Fatalf("expected 2 secrets after filtering, got %d", len(filtered))
	}

	got := make(map[string]struct{}, len(filtered))
	for _, s := range filtered {
		got[s.Name] = struct{}{}
	}
	if _, ok := got["GEMINI_API_KEY"]; !ok {
		t.Error("expected GEMINI_API_KEY to be kept")
	}
	if _, ok := got["NOT_AUTH_SECRET"]; !ok {
		t.Error("expected NOT_AUTH_SECRET to be kept")
	}
	if _, ok := got["GOOGLE_APPLICATION_CREDENTIALS"]; ok {
		t.Error("expected GOOGLE_APPLICATION_CREDENTIALS to be dropped for api-key auth")
	}
}

func TestStartInjectsHubEnvFromGroveSettings(t *testing.T) {
	// When grove settings have hub enabled with an endpoint, Start() should
	// inject SCION_HUB_ENDPOINT and SCION_HUB_URL into the container env.
	tmpDir := t.TempDir()

	oldWd, _ := os.Getwd()
	os.Chdir(tmpDir)
	defer os.Chdir(oldWd)

	originalHome := os.Getenv("HOME")
	defer os.Setenv("HOME", originalHome)
	os.Setenv("HOME", tmpDir)

	// Clear env vars that would interfere with settings loading
	for _, k := range []string{"SCION_DEV_TOKEN", "SCION_AUTH_TOKEN", "SCION_DEV_TOKEN_FILE", "SCION_HUB_ENDPOINT", "SCION_HUB_URL"} {
		if old, ok := os.LookupEnv(k); ok {
			defer os.Setenv(k, old)
			os.Unsetenv(k)
		}
	}

	globalScionDir := filepath.Join(tmpDir, ".scion")

	// Create harness-config
	hcDir := filepath.Join(globalScionDir, "harness-configs", "test-harness")
	os.MkdirAll(hcDir, 0755)
	os.WriteFile(filepath.Join(hcDir, "config.yaml"), []byte("harness: gemini\nuser: scion\nimage: test-image:latest\n"), 0644)

	// Create a minimal template
	tplDir := filepath.Join(globalScionDir, "templates", "default")
	os.MkdirAll(tplDir, 0755)
	os.WriteFile(filepath.Join(tplDir, "scion-agent.json"), []byte(`{"default_harness_config": "test-harness"}`), 0644)

	// Global settings
	os.WriteFile(filepath.Join(globalScionDir, "settings.yaml"), []byte(`schema_version: "1"
active_profile: local
profiles:
  local:
    runtime: docker
`), 0644)

	// Create project grove with hub-enabled settings
	projectDir := filepath.Join(tmpDir, "project")
	projectScionDir := filepath.Join(projectDir, ".scion")
	os.MkdirAll(projectScionDir, 0755)
	os.WriteFile(filepath.Join(projectScionDir, "settings.yaml"), []byte(`hub:
  enabled: true
  endpoint: "http://localhost:9810"
`), 0644)

	// Write a dev-token file so the token resolution finds it
	os.WriteFile(filepath.Join(globalScionDir, "dev-token"), []byte("scion-dev-test-token-abc"), 0644)

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
		Name:      "test-agent",
		GrovePath: projectScionDir,
		NoAuth:    true,
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
	if got := envMap["SCION_AUTH_TOKEN"]; got != "scion-dev-test-token-abc" {
		t.Errorf("SCION_AUTH_TOKEN = %q, want %q", got, "scion-dev-test-token-abc")
	}
}

func TestStartPreservesExplicitHubEndpoint(t *testing.T) {
	// When hub endpoint is already set in opts.Env (e.g. from broker dispatch),
	// grove settings should NOT override it.
	tmpDir := t.TempDir()

	oldWd, _ := os.Getwd()
	os.Chdir(tmpDir)
	defer os.Chdir(oldWd)

	originalHome := os.Getenv("HOME")
	defer os.Setenv("HOME", originalHome)
	os.Setenv("HOME", tmpDir)

	globalScionDir := filepath.Join(tmpDir, ".scion")

	// Create harness-config
	hcDir := filepath.Join(globalScionDir, "harness-configs", "test-harness")
	os.MkdirAll(hcDir, 0755)
	os.WriteFile(filepath.Join(hcDir, "config.yaml"), []byte("harness: gemini\nuser: scion\nimage: test-image:latest\n"), 0644)

	// Create a minimal template
	tplDir := filepath.Join(globalScionDir, "templates", "default")
	os.MkdirAll(tplDir, 0755)
	os.WriteFile(filepath.Join(tplDir, "scion-agent.json"), []byte(`{"default_harness_config": "test-harness"}`), 0644)

	// Global settings
	os.WriteFile(filepath.Join(globalScionDir, "settings.yaml"), []byte(`schema_version: "1"
active_profile: local
profiles:
  local:
    runtime: docker
`), 0644)

	// Create project grove with hub-enabled settings (different endpoint)
	projectDir := filepath.Join(tmpDir, "project")
	projectScionDir := filepath.Join(projectDir, ".scion")
	os.MkdirAll(projectScionDir, 0755)
	os.WriteFile(filepath.Join(projectScionDir, "settings.yaml"), []byte(`hub:
  enabled: true
  endpoint: "http://grove-setting:9810"
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
		Name:      "test-agent",
		GrovePath: projectScionDir,
		NoAuth:    true,
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

	// Broker-dispatched endpoint should be preserved, not overwritten by grove settings
	if got := envMap["SCION_HUB_ENDPOINT"]; got != "http://broker-dispatch:9810" {
		t.Errorf("SCION_HUB_ENDPOINT = %q, want %q (explicit should win over grove settings)", got, "http://broker-dispatch:9810")
	}
}

func TestBuildAgentEnv_EnvKeyScionHubEndpointOverride(t *testing.T) {
	// Unit test verifying that when scionCfg.Env has SCION_HUB_ENDPOINT and
	// it's pre-applied to extraEnv (simulating the new run.go logic), the
	// env-section value wins over the grove/broker value.
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
		// 1. CLI/grove settings sets initial value
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

		env, _, _ := buildAgentEnv(scionCfg, extraEnv)

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

		env, _, _ := buildAgentEnv(scionCfg, extraEnv)

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
	// When grove settings have hub.enabled=false, hub env vars should NOT be
	// injected into the container, even when hub.endpoint is configured and
	// agent-level hub config or template env section specifies an endpoint.
	tmpDir := t.TempDir()

	oldWd, _ := os.Getwd()
	os.Chdir(tmpDir)
	defer os.Chdir(oldWd)

	originalHome := os.Getenv("HOME")
	defer os.Setenv("HOME", originalHome)
	os.Setenv("HOME", tmpDir)

	// Clear dev token env vars so we control the test
	for _, k := range []string{"SCION_DEV_TOKEN", "SCION_AUTH_TOKEN", "SCION_DEV_TOKEN_FILE"} {
		if old, ok := os.LookupEnv(k); ok {
			defer os.Setenv(k, old)
			os.Unsetenv(k)
		}
	}

	globalScionDir := filepath.Join(tmpDir, ".scion")

	// Create harness-config
	hcDir := filepath.Join(globalScionDir, "harness-configs", "test-harness")
	os.MkdirAll(hcDir, 0755)
	os.WriteFile(filepath.Join(hcDir, "config.yaml"), []byte("harness: gemini\nuser: scion\nimage: test-image:latest\n"), 0644)

	// Create a minimal template
	tplDir := filepath.Join(globalScionDir, "templates", "default")
	os.MkdirAll(tplDir, 0755)
	os.WriteFile(filepath.Join(tplDir, "scion-agent.json"), []byte(`{"default_harness_config": "test-harness"}`), 0644)

	// Global settings
	os.WriteFile(filepath.Join(globalScionDir, "settings.yaml"), []byte(`schema_version: "1"
active_profile: local
profiles:
  local:
    runtime: docker
`), 0644)

	// Create project grove with hub explicitly DISABLED but endpoint configured
	projectDir := filepath.Join(tmpDir, "project")
	projectScionDir := filepath.Join(projectDir, ".scion")
	os.MkdirAll(projectScionDir, 0755)
	os.WriteFile(filepath.Join(projectScionDir, "settings.yaml"), []byte(`hub:
  enabled: false
  endpoint: "http://localhost:9810"
`), 0644)

	// Write a dev-token file (should NOT be used since hub is disabled)
	os.WriteFile(filepath.Join(globalScionDir, "dev-token"), []byte("scion-dev-test-token-abc"), 0644)

	t.Run("grove settings hub disabled suppresses hub env", func(t *testing.T) {
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
			Name:      "test-agent",
			GrovePath: projectScionDir,
			NoAuth:    true,
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
		// Agent scion-agent.json has hub.endpoint but grove says hub.enabled=false
		agentDir := filepath.Join(projectScionDir, "agents", "hub-disabled-agent")
		os.MkdirAll(filepath.Join(agentDir, "home"), 0755)
		os.WriteFile(filepath.Join(agentDir, "scion-agent.json"), []byte(`{
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
			Name:      "hub-disabled-agent",
			GrovePath: projectScionDir,
			NoAuth:    true,
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
		// Agent scion-agent.json has env.SCION_HUB_ENDPOINT but grove says hub.enabled=false
		agentDir := filepath.Join(projectScionDir, "agents", "hub-disabled-env")
		os.MkdirAll(filepath.Join(agentDir, "home"), 0755)
		os.WriteFile(filepath.Join(agentDir, "scion-agent.json"), []byte(`{
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
			Name:      "hub-disabled-env",
			GrovePath: projectScionDir,
			NoAuth:    true,
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
	// grove settings -> hub.endpoint -> env.SCION_HUB_ENDPOINT
	// The env-key value should be the final one in the container env.
	tmpDir := t.TempDir()

	oldWd, _ := os.Getwd()
	os.Chdir(tmpDir)
	defer os.Chdir(oldWd)

	originalHome := os.Getenv("HOME")
	defer os.Setenv("HOME", originalHome)
	os.Setenv("HOME", tmpDir)

	// Clear dev token env vars so we control the test
	for _, k := range []string{"SCION_DEV_TOKEN", "SCION_AUTH_TOKEN", "SCION_DEV_TOKEN_FILE"} {
		if old, ok := os.LookupEnv(k); ok {
			defer os.Setenv(k, old)
			os.Unsetenv(k)
		}
	}

	globalScionDir := filepath.Join(tmpDir, ".scion")

	// Create harness-config
	hcDir := filepath.Join(globalScionDir, "harness-configs", "test-harness")
	os.MkdirAll(hcDir, 0755)
	os.WriteFile(filepath.Join(hcDir, "config.yaml"), []byte("harness: gemini\nuser: scion\nimage: test-image:latest\n"), 0644)

	// Create a minimal template
	tplDir := filepath.Join(globalScionDir, "templates", "default")
	os.MkdirAll(tplDir, 0755)
	os.WriteFile(filepath.Join(tplDir, "scion-agent.json"), []byte(`{"default_harness_config": "test-harness"}`), 0644)

	// Global settings
	os.WriteFile(filepath.Join(globalScionDir, "settings.yaml"), []byte(`schema_version: "1"
active_profile: local
profiles:
  local:
    runtime: docker
`), 0644)

	// Create project grove with hub-enabled settings (priority 1)
	projectDir := filepath.Join(tmpDir, "project")
	projectScionDir := filepath.Join(projectDir, ".scion")
	os.MkdirAll(projectScionDir, 0755)
	os.WriteFile(filepath.Join(projectScionDir, "settings.yaml"), []byte(`hub:
  enabled: true
  endpoint: "http://grove-settings:9810"
`), 0644)

	// Create agent with both hub.endpoint (priority 2) and
	// env.SCION_HUB_ENDPOINT (priority 3 — should win)
	agentDir := filepath.Join(projectScionDir, "agents", "hub-env-test")
	os.MkdirAll(filepath.Join(agentDir, "home"), 0755)
	os.WriteFile(filepath.Join(agentDir, "scion-agent.json"), []byte(`{
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
		Name:      "hub-env-test",
		GrovePath: projectScionDir,
		NoAuth:    true,
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
	// overriding both grove settings (priority 1) and hub.endpoint (priority 2)
	if got := envMap["SCION_HUB_ENDPOINT"]; got != "http://host.docker.internal:8080" {
		t.Errorf("SCION_HUB_ENDPOINT = %q, want %q (env section should override all)", got, "http://host.docker.internal:8080")
	}
	if got := envMap["SCION_HUB_URL"]; got != "http://host.docker.internal:8080" {
		t.Errorf("SCION_HUB_URL = %q, want %q (env section should override all)", got, "http://host.docker.internal:8080")
	}
}

func TestProfileEnvVisibleInAuthOverlay(t *testing.T) {
	// Regression: profile env vars must be injected into opts.Env BEFORE
	// buildAuthEnvOverlay is called. Previously the overlay was built first,
	// so profile-provided vars like GOOGLE_CLOUD_PROJECT were invisible to
	// GatherAuthWithEnv, causing auth resolution to fail.
	optsEnv := map[string]string{"EXISTING": "val"}

	// Simulate profile env injection (what Start does before buildAuthEnvOverlay)
	profileEnv := map[string]string{
		"GOOGLE_CLOUD_PROJECT": "my-project",
		"GOOGLE_CLOUD_REGION":  "us-central1",
	}
	for k, v := range profileEnv {
		if _, exists := optsEnv[k]; !exists {
			optsEnv[k] = v
		}
	}

	overlay := buildAuthEnvOverlay(optsEnv, nil)

	if got := overlay["GOOGLE_CLOUD_PROJECT"]; got != "my-project" {
		t.Errorf("GOOGLE_CLOUD_PROJECT in auth overlay = %q, want %q", got, "my-project")
	}
	if got := overlay["GOOGLE_CLOUD_REGION"]; got != "us-central1" {
		t.Errorf("GOOGLE_CLOUD_REGION in auth overlay = %q, want %q", got, "us-central1")
	}
}

func TestStartInjectsProfileEnvForAuth(t *testing.T) {
	// When a profile defines env vars like GOOGLE_CLOUD_PROJECT and
	// GOOGLE_CLOUD_REGION, Start() should inject them into opts.Env so that
	// GatherAuthWithEnv can see them during local (non-broker) auth resolution.
	tmpDir := t.TempDir()

	oldWd, _ := os.Getwd()
	os.Chdir(tmpDir)
	defer os.Chdir(oldWd)

	originalHome := os.Getenv("HOME")
	defer os.Setenv("HOME", originalHome)
	os.Setenv("HOME", tmpDir)

	// Clear env vars that would interfere
	for _, k := range []string{"GOOGLE_CLOUD_PROJECT", "GOOGLE_CLOUD_REGION"} {
		if old, ok := os.LookupEnv(k); ok {
			defer os.Setenv(k, old)
			os.Unsetenv(k)
		}
	}

	globalScionDir := filepath.Join(tmpDir, ".scion")

	// Create harness-config on disk (claude type)
	hcDir := filepath.Join(globalScionDir, "harness-configs", "claude-cfg")
	os.MkdirAll(hcDir, 0755)
	os.WriteFile(filepath.Join(hcDir, "config.yaml"), []byte("harness: claude\nuser: scion\nimage: test-image:latest\n"), 0644)

	// Create a minimal template
	tplDir := filepath.Join(globalScionDir, "templates", "default")
	os.MkdirAll(tplDir, 0755)
	os.WriteFile(filepath.Join(tplDir, "scion-agent.json"), []byte(`{"default_harness_config": "claude-cfg"}`), 0644)

	// Global versioned settings with a profile that has env vars
	os.WriteFile(filepath.Join(globalScionDir, "settings.yaml"), []byte(`schema_version: "1"
active_profile: vertex
profiles:
  vertex:
    runtime: docker
    env:
      GOOGLE_CLOUD_PROJECT: my-gcp-project
      GOOGLE_CLOUD_REGION: us-central1
runtimes:
  docker:
    type: docker
`), 0644)

	// Create project grove
	projectDir := filepath.Join(tmpDir, "project")
	projectScionDir := filepath.Join(projectDir, ".scion")
	os.MkdirAll(projectScionDir, 0755)

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
		Name:      "test-agent",
		GrovePath: projectScionDir,
		NoAuth:    true,
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

	if got := envMap["GOOGLE_CLOUD_PROJECT"]; got != "my-gcp-project" {
		t.Errorf("GOOGLE_CLOUD_PROJECT = %q, want %q", got, "my-gcp-project")
	}
	if got := envMap["GOOGLE_CLOUD_REGION"]; got != "us-central1" {
		t.Errorf("GOOGLE_CLOUD_REGION = %q, want %q", got, "us-central1")
	}
}
