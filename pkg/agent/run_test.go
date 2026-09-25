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
	"os/user"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/harness"
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
	t.Setenv("INHERITED_KEY", "inherited-value")

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
	_ = os.Chdir(tmpDir)
	defer func() { _ = os.Chdir(oldWd) }()

	originalHome := os.Getenv("HOME")
	defer func() { _ = os.Setenv("HOME", originalHome) }()
	_ = os.Setenv("HOME", tmpDir)

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
`), 0644)

	projectDir := filepath.Join(tmpDir, "project")
	projectScionDir := filepath.Join(projectDir, ".scion")
	_ = os.MkdirAll(projectScionDir, 0755)

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
	_ = os.MkdirAll(filepath.Join(agentDir, "home"), 0755)
	_ = os.WriteFile(filepath.Join(agentDir, "scion-agent.json"), []byte(`{
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
		Name:        "broker-test",
		ProjectPath: projectScionDir,
		BrokerMode:  true,
		NoAuth:      true,
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
	_ = os.Chdir(tmpDir)
	defer func() { _ = os.Chdir(oldWd) }()

	originalHome := os.Getenv("HOME")
	defer func() { _ = os.Setenv("HOME", originalHome) }()
	_ = os.Setenv("HOME", tmpDir)

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
`), 0644)

	projectDir := filepath.Join(tmpDir, "project")
	projectScionDir := filepath.Join(projectDir, ".scion")
	_ = os.MkdirAll(projectScionDir, 0755)

	mockRT := &runtime.MockRuntime{
		ListFunc: func(ctx context.Context, labelFilter map[string]string) ([]api.AgentInfo, error) {
			return []api.AgentInfo{}, nil
		},
		RunFunc: func(ctx context.Context, config runtime.RunConfig) (string, error) {
			return "mock-id", nil
		},
	}

	agentDir := filepath.Join(projectScionDir, "agents", "local-test")
	_ = os.MkdirAll(filepath.Join(agentDir, "home"), 0755)
	_ = os.WriteFile(filepath.Join(agentDir, "scion-agent.json"), []byte(`{
		"harness": "generic",
		"env": {
			"MISSING_KEY": ""
		}
	}`), 0644)

	mgr := NewManager(mockRT)

	// In local mode (BrokerMode=false), empty env vars should be fatal
	_, err := mgr.Start(context.Background(), api.StartOptions{
		Name:        "local-test",
		ProjectPath: projectScionDir,
		NoAuth:      true,
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
	t.Setenv("HOST_AVAILABLE_KEY", "host-value")

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
	// Create a temporary directory to act as the project
	tmpDir := t.TempDir()

	// Move to tmpDir to avoid being inside the project's git repo
	oldWd, _ := os.Getwd()
	_ = os.Chdir(tmpDir)
	defer func() { _ = os.Chdir(oldWd) }()

	// Mock HOME for global settings
	originalHome := os.Getenv("HOME")
	defer func() { _ = os.Setenv("HOME", originalHome) }()
	_ = os.Setenv("HOME", tmpDir)

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
		Name:        "non-existent-agent",
		ProjectPath: scionDir,
		Resume:      true,
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
	_ = os.Chdir(tmpDir)
	defer func() { _ = os.Chdir(oldWd) }()

	originalHome := os.Getenv("HOME")
	defer func() { _ = os.Setenv("HOME", originalHome) }()
	_ = os.Setenv("HOME", tmpDir)

	globalScionDir := filepath.Join(tmpDir, ".scion")

	// Create harness-config with user field
	hcDir := filepath.Join(globalScionDir, "harness-configs", "test-harness")
	_ = os.MkdirAll(hcDir, 0755)
	_ = os.WriteFile(filepath.Join(hcDir, "config.yaml"), []byte("harness: gemini\nuser: scion\nimage: test-image:latest\n"), 0644)

	// Create a minimal template
	tplDir := filepath.Join(globalScionDir, "templates", "default")
	_ = os.MkdirAll(tplDir, 0755)
	_ = os.WriteFile(filepath.Join(tplDir, "scion-agent.json"), []byte(`{"default_harness_config": "test-harness"}`), 0644)

	// Settings without harness_configs entries (simulating default_settings.yaml)
	_ = os.WriteFile(filepath.Join(globalScionDir, "settings.yaml"), []byte(`schema_version: "1"
active_profile: local
profiles:
  local:
    runtime: docker
`), 0644)

	// Create project directory
	projectDir := filepath.Join(tmpDir, "project")
	projectScionDir := filepath.Join(projectDir, ".scion")
	_ = os.MkdirAll(projectScionDir, 0755)

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
		Name:        "test-agent",
		ProjectPath: projectScionDir,
		NoAuth:      true,
	})
	if err != nil {
		t.Fatalf("Start failed: %v", err)
	}

	if capturedConfig.UnixUsername != "scion" {
		t.Errorf("expected UnixUsername = %q, got %q", "scion", capturedConfig.UnixUsername)
	}
}

func TestStartPropagatesNFSWorkspaceBackendToRunConfig(t *testing.T) {
	tmpDir := t.TempDir()

	oldWd, err := os.Getwd()
	if err != nil {
		t.Fatalf("failed to get working directory: %v", err)
	}
	if err := os.Chdir(tmpDir); err != nil {
		t.Fatalf("failed to chdir to tmpDir: %v", err)
	}
	defer func() { _ = os.Chdir(oldWd) }()

	originalHome := os.Getenv("HOME")
	defer func() { _ = os.Setenv("HOME", originalHome) }()
	if err := os.Setenv("HOME", tmpDir); err != nil {
		t.Fatalf("failed to set HOME: %v", err)
	}

	projectDir := filepath.Join(tmpDir, "project")
	projectScionDir := filepath.Join(projectDir, ".scion")
	if err := os.MkdirAll(projectScionDir, 0755); err != nil {
		t.Fatalf("failed to create project .scion dir: %v", err)
	}

	nfsMountRoot := filepath.Join(tmpDir, "nfs")
	settingsYAML := fmt.Sprintf(`schema_version: "1"
active_profile: local
server:
  workspace_storage:
    backend: nfs
    nfs:
      mount_root: %s
      uid: 2000
      gid: 2001
      storage_class: filestore-sc
      shares:
        - id: share-1
          server: 10.0.0.2
          export: /scion-workspaces
          pv_name: scion-workspaces-pv
harness_configs:
  test-harness:
    harness: gemini
    user: scion
    image: test-image:latest
profiles:
  local:
    runtime: docker
`, nfsMountRoot)
	if err := os.WriteFile(filepath.Join(projectScionDir, "settings.yaml"), []byte(settingsYAML), 0644); err != nil {
		t.Fatalf("failed to write settings: %v", err)
	}

	hcDir := filepath.Join(projectScionDir, "harness-configs", "test-harness")
	if err := os.MkdirAll(hcDir, 0755); err != nil {
		t.Fatalf("failed to create harness-config dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(hcDir, "config.yaml"), []byte("harness: gemini\nuser: scion\nimage: test-image:latest\n"), 0644); err != nil {
		t.Fatalf("failed to write harness config: %v", err)
	}

	tplDir := filepath.Join(projectScionDir, "templates", "default")
	if err := os.MkdirAll(tplDir, 0755); err != nil {
		t.Fatalf("failed to create template dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(tplDir, "scion-agent.json"), []byte(`{"default_harness_config": "test-harness"}`), 0644); err != nil {
		t.Fatalf("failed to write template: %v", err)
	}

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
	_, err = mgr.Start(context.Background(), api.StartOptions{
		Name:        "test-agent",
		ProjectPath: projectScionDir,
		NoAuth:      true,
		Env: map[string]string{
			"SCION_AGENT_ID":   "agent-456",
			"SCION_PROJECT_ID": "proj-123",
		},
		GitClone: &api.GitCloneConfig{URL: "https://example.com/repo.git"},
	})
	if err != nil {
		t.Fatalf("Start failed: %v", err)
	}

	if capturedConfig.WorkspaceBackendName != "nfs" {
		t.Fatalf("WorkspaceBackendName = %q, want nfs", capturedConfig.WorkspaceBackendName)
	}
	wantWorkspace := filepath.Join(nfsMountRoot, "share-1", "projects", "proj-123", "workspace")
	if capturedConfig.Workspace != wantWorkspace {
		t.Fatalf("Workspace = %q, want %q", capturedConfig.Workspace, wantWorkspace)
	}
	if capturedConfig.NFSUID != 2000 || capturedConfig.NFSGID != 2001 {
		t.Fatalf("NFS uid/gid = %d/%d, want 2000/2001", capturedConfig.NFSUID, capturedConfig.NFSGID)
	}
	if capturedConfig.NFSPVClaimName != "scion-workspaces-pv" {
		t.Fatalf("NFSPVClaimName = %q", capturedConfig.NFSPVClaimName)
	}
	if capturedConfig.NFSSubPath != filepath.Join("projects", "proj-123", "workspace") {
		t.Fatalf("NFSSubPath = %q", capturedConfig.NFSSubPath)
	}
	if capturedConfig.NFSStorageClass != "filestore-sc" {
		t.Fatalf("NFSStorageClass = %q", capturedConfig.NFSStorageClass)
	}
	if capturedConfig.Labels["agent_id"] != "agent-456" {
		t.Fatalf("agent_id label = %q", capturedConfig.Labels["agent_id"])
	}
}

func TestStartResolvesHarnessConfigUserSettingsOverride(t *testing.T) {
	// When settings define a user in harness_configs, it should override
	// the on-disk harness-config user.
	tmpDir := t.TempDir()

	oldWd, _ := os.Getwd()
	_ = os.Chdir(tmpDir)
	defer func() { _ = os.Chdir(oldWd) }()

	originalHome := os.Getenv("HOME")
	defer func() { _ = os.Setenv("HOME", originalHome) }()
	_ = os.Setenv("HOME", tmpDir)

	globalScionDir := filepath.Join(tmpDir, ".scion")

	// Create harness-config with user field
	hcDir := filepath.Join(globalScionDir, "harness-configs", "test-harness")
	_ = os.MkdirAll(hcDir, 0755)
	_ = os.WriteFile(filepath.Join(hcDir, "config.yaml"), []byte("harness: gemini\nuser: scion\nimage: test-image:latest\n"), 0644)

	// Create a minimal template
	tplDir := filepath.Join(globalScionDir, "templates", "default")
	_ = os.MkdirAll(tplDir, 0755)
	_ = os.WriteFile(filepath.Join(tplDir, "scion-agent.json"), []byte(`{"default_harness_config": "test-harness"}`), 0644)

	// Settings WITH harness_configs that override the user
	_ = os.WriteFile(filepath.Join(globalScionDir, "settings.yaml"), []byte(`schema_version: "1"
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

	// Create project directory
	projectDir := filepath.Join(tmpDir, "project")
	projectScionDir := filepath.Join(projectDir, ".scion")
	_ = os.MkdirAll(projectScionDir, 0755)

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
		Name:        "test-agent",
		ProjectPath: projectScionDir,
		NoAuth:      true,
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
	// which could not be resolved in the project, causing FindHarnessConfigDir to
	// miss the template-bundled harness config and defaulting to user "root".
	tmpDir := t.TempDir()

	oldWd, _ := os.Getwd()
	_ = os.Chdir(tmpDir)
	defer func() { _ = os.Chdir(oldWd) }()

	originalHome := os.Getenv("HOME")
	defer func() { _ = os.Setenv("HOME", originalHome) }()
	_ = os.Setenv("HOME", tmpDir)

	globalScionDir := filepath.Join(tmpDir, ".scion")

	// Create a template at an absolute path (simulating hydrated template cache)
	// with a bundled harness-config that has user: scion
	hydratedTplDir := filepath.Join(tmpDir, "template-cache", "web-dev")
	hcDir := filepath.Join(hydratedTplDir, "harness-configs", "claude-web")
	_ = os.MkdirAll(hcDir, 0755)
	_ = os.WriteFile(filepath.Join(hcDir, "config.yaml"), []byte("harness: claude\nuser: scion\nimage: scion-claude:latest\n"), 0644)
	_ = os.MkdirAll(filepath.Join(hcDir, "home"), 0755)
	_ = os.WriteFile(filepath.Join(hydratedTplDir, "scion-agent.json"), []byte(`{"default_harness_config": "claude-web"}`), 0644)

	// Minimal global settings (no harness_configs defined)
	_ = os.MkdirAll(globalScionDir, 0755)
	_ = os.WriteFile(filepath.Join(globalScionDir, "settings.yaml"), []byte(`schema_version: "1"
active_profile: local
profiles:
  local:
    runtime: docker
`), 0644)

	// Create project directory
	projectDir := filepath.Join(tmpDir, "project")
	projectScionDir := filepath.Join(projectDir, ".scion")
	_ = os.MkdirAll(projectScionDir, 0755)

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
		Name:        "test-agent",
		Template:    hydratedTplDir, // absolute path, simulating hydrated template
		ProjectPath: projectScionDir,
		NoAuth:      true,
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
	_ = os.Chdir(tmpDir)
	defer func() { _ = os.Chdir(oldWd) }()

	originalHome := os.Getenv("HOME")
	defer func() { _ = os.Setenv("HOME", originalHome) }()
	_ = os.Setenv("HOME", tmpDir)

	globalScionDir := filepath.Join(tmpDir, ".scion")

	// Create a "default" template (required as base layer)
	defaultTplDir := filepath.Join(globalScionDir, "templates", "default")
	_ = os.MkdirAll(defaultTplDir, 0755)
	_ = os.WriteFile(filepath.Join(defaultTplDir, "scion-agent.yaml"), []byte("default_harness_config: claude\n"), 0644)

	// Seed the default "claude" harness-config at global level
	claudeHcDir := filepath.Join(globalScionDir, "harness-configs", "claude")
	_ = os.MkdirAll(filepath.Join(claudeHcDir, "home"), 0755)
	_ = os.WriteFile(filepath.Join(claudeHcDir, "config.yaml"), []byte("harness: claude\nuser: scion\nimage: scion-claude:latest\n"), 0644)

	// Create a non-default template "test4" with a bundled harness-config "claude2"
	test4TplDir := filepath.Join(globalScionDir, "templates", "test4")
	_ = os.MkdirAll(test4TplDir, 0755)
	_ = os.WriteFile(filepath.Join(test4TplDir, "scion-agent.yaml"), []byte("default_harness_config: claude2\n"), 0644)

	claude2HcDir := filepath.Join(test4TplDir, "harness-configs", "claude2")
	_ = os.MkdirAll(filepath.Join(claude2HcDir, "home"), 0755)
	_ = os.WriteFile(filepath.Join(claude2HcDir, "config.yaml"), []byte("harness: claude\nuser: scion\nimage: custom-claude:latest\n"), 0644)

	// Minimal global settings
	_ = os.WriteFile(filepath.Join(globalScionDir, "settings.yaml"), []byte(`schema_version: "1"
active_profile: local
profiles:
  local:
    runtime: docker
`), 0644)

	// Create project directory
	projectDir := filepath.Join(tmpDir, "project")
	projectScionDir := filepath.Join(projectDir, ".scion")
	_ = os.MkdirAll(projectScionDir, 0755)

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
		Name:        "test-agent",
		Template:    "test4",
		ProjectPath: projectScionDir,
		NoAuth:      true,
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
	// The runtime's Phase field is authoritative for running state detection.
	mockRT := &runtime.MockRuntime{
		ListFunc: func(ctx context.Context, labelFilter map[string]string) ([]api.AgentInfo, error) {
			return []api.AgentInfo{
				{
					ContainerID:     "abc123",
					Name:            "test-agent",
					ContainerStatus: "Up 2 hours",
					Phase:           string(state.PhaseRunning),
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
		"SCION_OTEL_SKIP_TLS_VERIFY":    "false",
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
	_ = os.Chdir(tmpDir)
	defer func() { _ = os.Chdir(oldWd) }()

	originalHome := os.Getenv("HOME")
	defer func() { _ = os.Setenv("HOME", originalHome) }()
	_ = os.Setenv("HOME", tmpDir)

	globalScionDir := filepath.Join(tmpDir, ".scion")

	// Create harness-config
	hcDir := filepath.Join(globalScionDir, "harness-configs", "test-harness")
	_ = os.MkdirAll(hcDir, 0755)
	_ = os.WriteFile(filepath.Join(hcDir, "config.yaml"), []byte("harness: generic\nuser: scion\nimage: test-image:latest\n"), 0644)

	// Create template
	tplDir := filepath.Join(globalScionDir, "templates", "default")
	_ = os.MkdirAll(tplDir, 0755)
	_ = os.WriteFile(filepath.Join(tplDir, "scion-agent.json"), []byte(`{"default_harness_config": "test-harness"}`), 0644)

	_ = os.WriteFile(filepath.Join(globalScionDir, "settings.yaml"), []byte(`schema_version: "1"
active_profile: local
profiles:
  local:
    runtime: docker
`), 0644)

	projectDir := filepath.Join(tmpDir, "project")
	projectScionDir := filepath.Join(projectDir, ".scion")
	_ = os.MkdirAll(projectScionDir, 0755)

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
		_ = os.MkdirAll(filepath.Join(agentDir, "home"), 0755)
		_ = os.WriteFile(filepath.Join(agentDir, "scion-agent.json"), []byte(`{
			"harness": "generic",
			"task_flag": "--input",
			"command_args": ["adk", "run", "/opt/agent"]
		}`), 0644)

		mgr := NewManager(mockRT)
		_, err := mgr.Start(context.Background(), api.StartOptions{
			Name:        "flag-test",
			ProjectPath: projectScionDir,
			Task:        "do something",
			NoAuth:      true,
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
		_ = os.MkdirAll(filepath.Join(agentDir, "home"), 0755)
		_ = os.WriteFile(filepath.Join(agentDir, "scion-agent.json"), []byte(`{
			"harness": "generic",
			"command_args": ["adk", "run", "/opt/agent"]
		}`), 0644)

		mgr := NewManager(mockRT)
		_, err := mgr.Start(context.Background(), api.StartOptions{
			Name:        "noflag-test",
			ProjectPath: projectScionDir,
			Task:        "do something",
			NoAuth:      true,
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

	// Create template
	tplDir := filepath.Join(globalScionDir, "templates", "default")
	_ = os.MkdirAll(tplDir, 0755)
	_ = os.WriteFile(filepath.Join(tplDir, "scion-agent.json"), []byte(`{"default_harness_config": "test-harness"}`), 0644)

	_ = os.WriteFile(filepath.Join(globalScionDir, "settings.yaml"), []byte(`schema_version: "1"
active_profile: local
profiles:
  local:
    runtime: docker
`), 0644)

	projectDir := filepath.Join(tmpDir, "project")
	projectScionDir := filepath.Join(projectDir, ".scion")
	_ = os.MkdirAll(projectScionDir, 0755)

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
		_ = os.MkdirAll(filepath.Join(agentDir, "home"), 0755)
		_ = os.WriteFile(filepath.Join(agentDir, "scion-agent.json"), []byte(`{
			"harness": "gemini",
			"telemetry": {"enabled": true}
		}`), 0644)

		mgr := NewManager(mockRT)
		_, err := mgr.Start(context.Background(), api.StartOptions{
			Name:        "telem-on",
			ProjectPath: projectScionDir,
			NoAuth:      true,
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
		_ = os.MkdirAll(filepath.Join(agentDir, "home"), 0755)
		_ = os.WriteFile(filepath.Join(agentDir, "scion-agent.json"), []byte(`{
			"harness": "gemini",
			"telemetry": {"enabled": false}
		}`), 0644)

		mgr := NewManager(mockRT)
		_, err := mgr.Start(context.Background(), api.StartOptions{
			Name:        "telem-off",
			ProjectPath: projectScionDir,
			NoAuth:      true,
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
	_ = os.Chdir(tmpDir)
	defer func() { _ = os.Chdir(oldWd) }()

	originalHome := os.Getenv("HOME")
	defer func() { _ = os.Setenv("HOME", originalHome) }()
	_ = os.Setenv("HOME", tmpDir)

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
`), 0644)

	projectDir := filepath.Join(tmpDir, "project")
	projectScionDir := filepath.Join(projectDir, ".scion")
	_ = os.MkdirAll(projectScionDir, 0755)

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
		_ = os.MkdirAll(filepath.Join(agentDir, "home"), 0755)
		_ = os.WriteFile(filepath.Join(agentDir, "scion-agent.json"), []byte(`{
			"harness": "gemini",
			"telemetry": {"enabled": false}
		}`), 0644)

		mgr := NewManager(mockRT)
		_, err := mgr.Start(context.Background(), api.StartOptions{
			Name:              "override-enable",
			ProjectPath:       projectScionDir,
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
		_ = os.MkdirAll(filepath.Join(agentDir, "home"), 0755)
		_ = os.WriteFile(filepath.Join(agentDir, "scion-agent.json"), []byte(`{
			"harness": "gemini",
			"telemetry": {"enabled": true}
		}`), 0644)

		mgr := NewManager(mockRT)
		_, err := mgr.Start(context.Background(), api.StartOptions{
			Name:              "override-disable",
			ProjectPath:       projectScionDir,
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
		_ = os.MkdirAll(filepath.Join(agentDir, "home"), 0755)
		_ = os.WriteFile(filepath.Join(agentDir, "scion-agent.json"), []byte(`{
			"harness": "gemini"
		}`), 0644)

		mgr := NewManager(mockRT)
		_, err := mgr.Start(context.Background(), api.StartOptions{
			Name:              "override-no-config",
			ProjectPath:       projectScionDir,
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
	for _, e := range os.Environ() {
		if strings.HasPrefix(e, "SCION_") {
			k := strings.SplitN(e, "=", 2)[0]
			t.Setenv(k, "") // registers cleanup to restore original value
			os.Unsetenv(k)  //nolint:errcheck
		}
	}
	// Verify that telemetry cloud config from settings.yaml gets merged into
	// the container env vars during Start(), enabling cloud export.
	tmpDir := t.TempDir()

	oldWd, _ := os.Getwd()
	_ = os.Chdir(tmpDir)
	defer func() { _ = os.Chdir(oldWd) }()

	originalHome := os.Getenv("HOME")
	defer func() { _ = os.Setenv("HOME", originalHome) }()
	_ = os.Setenv("HOME", tmpDir)

	globalScionDir := filepath.Join(tmpDir, ".scion")

	hcDir := filepath.Join(globalScionDir, "harness-configs", "test-harness")
	_ = os.MkdirAll(hcDir, 0755)
	_ = os.WriteFile(filepath.Join(hcDir, "config.yaml"), []byte("harness: gemini\nuser: scion\nimage: test-image:latest\n"), 0644)

	tplDir := filepath.Join(globalScionDir, "templates", "default")
	_ = os.MkdirAll(tplDir, 0755)
	_ = os.WriteFile(filepath.Join(tplDir, "scion-agent.json"), []byte(`{"default_harness_config": "test-harness"}`), 0644)

	// Settings with telemetry cloud config but telemetry.enabled: false
	// (the override should enable it)
	_ = os.WriteFile(filepath.Join(globalScionDir, "settings.yaml"), []byte(`schema_version: "1"
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
	_ = os.MkdirAll(projectScionDir, 0755)

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
	_ = os.MkdirAll(filepath.Join(agentDir, "home"), 0755)
	_ = os.WriteFile(filepath.Join(agentDir, "scion-agent.json"), []byte(`{
		"harness": "gemini"
	}`), 0644)

	mgr := NewManager(mockRT)
	env := make(map[string]string)
	_, err := mgr.Start(context.Background(), api.StartOptions{
		Name:              "settings-telem",
		ProjectPath:       projectScionDir,
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
	_ = os.Chdir(tmpDir)
	defer func() { _ = os.Chdir(oldWd) }()

	originalHome := os.Getenv("HOME")
	defer func() { _ = os.Setenv("HOME", originalHome) }()
	_ = os.Setenv("HOME", tmpDir)

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
`), 0644)

	projectDir := filepath.Join(tmpDir, "project")
	projectScionDir := filepath.Join(projectDir, ".scion")
	_ = os.MkdirAll(projectScionDir, 0755)

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
		_ = os.MkdirAll(filepath.Join(agentDir, "home"), 0755)
		_ = os.WriteFile(filepath.Join(agentDir, "scion-agent.json"), []byte(`{
			"harness": "gemini",
			"auth_selectedType": "api-key"
		}`), 0644)

		mgr := NewManager(mockRT)
		_, err := mgr.Start(context.Background(), api.StartOptions{
			Name:        "auth-override",
			ProjectPath: projectScionDir,
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

func TestHarnessAuthCorruptedValueNotPersisted(t *testing.T) {
	// Regression test: when opts.HarnessAuth contains a harness implementation
	// name (e.g. "container-script" from corrupted scion-agent.json), it must
	// NOT be persisted to scion-agent.json. The guards at run.go:493 and
	// run.go:754 prevent this.
	tmpDir := t.TempDir()

	oldWd, _ := os.Getwd()
	_ = os.Chdir(tmpDir)
	defer func() { _ = os.Chdir(oldWd) }()

	originalHome := os.Getenv("HOME")
	defer func() { _ = os.Setenv("HOME", originalHome) }()
	_ = os.Setenv("HOME", tmpDir)

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
`), 0644)

	projectDir := filepath.Join(tmpDir, "project")
	projectScionDir := filepath.Join(projectDir, ".scion")
	_ = os.MkdirAll(projectScionDir, 0755)

	for _, implName := range []string{"container-script", "generic", "builtin", "passthrough"} {
		t.Run(implName, func(t *testing.T) {
			mockRT := &runtime.MockRuntime{
				ListFunc: func(ctx context.Context, labelFilter map[string]string) ([]api.AgentInfo, error) {
					return []api.AgentInfo{}, nil
				},
				RunFunc: func(ctx context.Context, config runtime.RunConfig) (string, error) {
					return "mock-id", nil
				},
			}

			agentName := "corrupt-" + implName
			agentDir := filepath.Join(projectScionDir, "agents", agentName)
			_ = os.MkdirAll(filepath.Join(agentDir, "home"), 0755)
			// Simulate corrupted scion-agent.json with harness implementation name
			_ = os.WriteFile(filepath.Join(agentDir, "scion-agent.json"), []byte(`{
				"harness": "gemini",
				"auth_selectedType": "`+implName+`"
			}`), 0644)

			mgr := NewManager(mockRT)
			_, err := mgr.Start(context.Background(), api.StartOptions{
				Name:        agentName,
				ProjectPath: projectScionDir,
				NoAuth:      true,
				HarnessAuth: implName, // corrupted value from Hub
			})
			if err != nil {
				t.Fatalf("Start failed: %v", err)
			}

			data, err := os.ReadFile(filepath.Join(agentDir, "scion-agent.json"))
			if err != nil {
				t.Fatalf("failed to read scion-agent.json: %v", err)
			}
			// The corrupted implementation name must NOT appear as auth_selectedType.
			if strings.Contains(string(data), `"`+implName+`"`) {
				t.Errorf("scion-agent.json still contains corrupted value %q: %s", implName, string(data))
			}
		})
	}
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
	// Verify that hub env vars injected into opts.Env (from project settings
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
		{Name: "gcloud-adc", Type: "file", Target: "/home/scion/.config/gcloud/application_default_credentials.json", Value: "adc"},
		{Name: "NOT_AUTH_SECRET", Type: "environment", Target: "NOT_AUTH_SECRET", Value: "keep"},
	}
	resolved := &api.ResolvedAuth{
		Method: "api-key",
		EnvVars: map[string]string{
			"GEMINI_API_KEY": "gemini",
		},
	}

	filtered := filterResolvedSecretsForResolvedAuth(secrets, resolved, nil)
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
	if _, ok := got["gcloud-adc"]; ok {
		t.Error("expected gcloud-adc to be dropped for api-key auth")
	}
}

func TestIsAuthEnvKey_GCPSharedKeys(t *testing.T) {
	// GCP shared fields are always auth-related regardless of config
	gcpKeys := []string{
		"GOOGLE_CLOUD_PROJECT", "GCP_PROJECT", "ANTHROPIC_VERTEX_PROJECT_ID",
		"GOOGLE_CLOUD_REGION", "CLOUD_ML_REGION", "GOOGLE_CLOUD_LOCATION",
	}
	for _, key := range gcpKeys {
		if !isAuthEnvKey(key) {
			t.Errorf("isAuthEnvKey(%q) = false, want true", key)
		}
	}
	// Per-provider keys are no longer built-in — they come from config
	if isAuthEnvKey("GEMINI_API_KEY") {
		t.Error("isAuthEnvKey(GEMINI_API_KEY) without config = true, want false")
	}
	if isAuthEnvKey("RANDOM_ENV_VAR") {
		t.Error("isAuthEnvKey(RANDOM_ENV_VAR) = true, want false")
	}
}

func TestIsAuthEnvKey_ConfigDrivenKeys(t *testing.T) {
	configKeys := map[string]struct{}{
		"COPILOT_GITHUB_TOKEN": {},
		"GH_TOKEN":             {},
		"GITHUB_TOKEN":         {},
		"GEMINI_API_KEY":       {},
	}

	if !isAuthEnvKey("COPILOT_GITHUB_TOKEN", configKeys) {
		t.Error("isAuthEnvKey(COPILOT_GITHUB_TOKEN, configKeys) = false, want true")
	}
	if !isAuthEnvKey("GH_TOKEN", configKeys) {
		t.Error("isAuthEnvKey(GH_TOKEN, configKeys) = false, want true")
	}
	// Config-declared per-provider keys work with config keys present
	if !isAuthEnvKey("GEMINI_API_KEY", configKeys) {
		t.Error("isAuthEnvKey(GEMINI_API_KEY, configKeys) = false, want true")
	}
	// GCP shared keys always work
	if !isAuthEnvKey("GOOGLE_CLOUD_PROJECT", configKeys) {
		t.Error("isAuthEnvKey(GOOGLE_CLOUD_PROJECT, configKeys) = false, want true")
	}
	// Unknown key is still not auth
	if isAuthEnvKey("RANDOM_VAR", configKeys) {
		t.Error("isAuthEnvKey(RANDOM_VAR, configKeys) = true, want false")
	}
}

func TestConfigAuthEnvKeySet(t *testing.T) {
	authMeta := &config.HarnessAuthMetadata{
		Types: map[string]config.HarnessAuthTypeMetadata{
			"api-key": {
				RequiredEnv: []config.HarnessAuthEnvRequirement{
					{AnyOf: []string{"COPILOT_GITHUB_TOKEN", "GH_TOKEN", "GITHUB_TOKEN"}},
				},
			},
			"vertex-ai": {
				RequiredEnv: []config.HarnessAuthEnvRequirement{
					{AnyOf: []string{"GOOGLE_CLOUD_PROJECT"}},
				},
			},
		},
	}

	keys := configAuthEnvKeySet(authMeta)
	expected := []string{"COPILOT_GITHUB_TOKEN", "GH_TOKEN", "GITHUB_TOKEN", "GOOGLE_CLOUD_PROJECT"}
	for _, k := range expected {
		if _, ok := keys[k]; !ok {
			t.Errorf("expected key %q in configAuthEnvKeySet result", k)
		}
	}

	nilKeys := configAuthEnvKeySet(nil)
	if nilKeys != nil {
		t.Errorf("expected nil for nil authMeta, got %v", nilKeys)
	}
}

func TestFilterResolvedSecretsForResolvedAuth_ConfigDrivenKeys(t *testing.T) {
	configKeys := map[string]struct{}{
		"COPILOT_GITHUB_TOKEN": {},
		"GH_TOKEN":             {},
	}

	secrets := []api.ResolvedSecret{
		{Name: "COPILOT_GITHUB_TOKEN", Type: "environment", Target: "COPILOT_GITHUB_TOKEN", Value: "ghp_test"},
		{Name: "GH_TOKEN", Type: "environment", Target: "GH_TOKEN", Value: "gh_test"},
		{Name: "SOME_OTHER_SECRET", Type: "environment", Target: "SOME_OTHER_SECRET", Value: "other"},
	}

	resolved := &api.ResolvedAuth{
		Method: "api-key",
		EnvVars: map[string]string{
			"COPILOT_GITHUB_TOKEN": "ghp_test",
		},
	}

	filtered := filterResolvedSecretsForResolvedAuth(secrets, resolved, configKeys)

	got := make(map[string]struct{}, len(filtered))
	for _, s := range filtered {
		got[s.Name] = struct{}{}
	}

	if _, ok := got["COPILOT_GITHUB_TOKEN"]; !ok {
		t.Error("expected COPILOT_GITHUB_TOKEN to be kept (required by resolved auth)")
	}
	if _, ok := got["GH_TOKEN"]; ok {
		t.Error("expected GH_TOKEN to be dropped (config-driven auth key not required by resolved auth)")
	}
	if _, ok := got["SOME_OTHER_SECRET"]; !ok {
		t.Error("expected SOME_OTHER_SECRET to be kept (not an auth key)")
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
	// When the resolved harness config defines env vars like GOOGLE_CLOUD_PROJECT
	// and GOOGLE_CLOUD_REGION, Start() should inject them into opts.Env so that
	// GatherAuthWithEnv can see them during local (non-broker) auth resolution.
	//
	// NOTE ON THE NAME: this test is still called ...ProfileEnvForAuth, but its
	// fixture declares the env under harness_configs.<hc>.env, NOT under
	// profiles.<p>.env. G3-full retired profiles.<p>.env as an injection point
	// into harness-config resolution, so the old fixture could no longer reach
	// opts.Env at all. The fixture was migrated to the documented migration path
	// and EVERY ASSERTION BELOW IS BYTE-IDENTICAL to the pre-G3-full version --
	// that is the point: the injection behaviour under test did not change, only
	// the key you declare the env under. The name is left alone deliberately so
	// that this commit does not rename a test other agents are tracking by name;
	// renaming it is a follow-up, not a silent drive-by.
	tmpDir := t.TempDir()

	oldWd, _ := os.Getwd()
	_ = os.Chdir(tmpDir)
	defer func() { _ = os.Chdir(oldWd) }()

	originalHome := os.Getenv("HOME")
	defer func() { _ = os.Setenv("HOME", originalHome) }()
	_ = os.Setenv("HOME", tmpDir)

	// Clear env vars that would interfere
	for _, k := range []string{"GOOGLE_CLOUD_PROJECT", "GOOGLE_CLOUD_REGION"} {
		if old, ok := os.LookupEnv(k); ok {
			defer func() { _ = os.Setenv(k, old) }()
			_ = os.Unsetenv(k)
		}
	}

	globalScionDir := filepath.Join(tmpDir, ".scion")

	// Create harness-config on disk (claude type)
	hcDir := filepath.Join(globalScionDir, "harness-configs", "claude-cfg")
	_ = os.MkdirAll(hcDir, 0755)
	_ = os.WriteFile(filepath.Join(hcDir, "config.yaml"), []byte("harness: claude\nuser: scion\nimage: test-image:latest\n"), 0644)

	// Create a minimal template
	tplDir := filepath.Join(globalScionDir, "templates", "default")
	_ = os.MkdirAll(tplDir, 0755)
	_ = os.WriteFile(filepath.Join(tplDir, "scion-agent.json"), []byte(`{"default_harness_config": "claude-cfg"}`), 0644)

	// Global versioned settings. The auth env vars are declared under
	// harness_configs.claude-cfg.env; they were under profiles.vertex.env until
	// G3-full removed profiles.<p>.env as an injection point.
	//
	// The assertions are unchanged across the migration. The property this test
	// exists for — settings-declared auth env reaches GatherAuthWithEnv during
	// local, non-broker auth resolution — survives G3-full; only the key it is
	// spelled with moved. harness_overrides is deliberately not used.
	_ = os.WriteFile(filepath.Join(globalScionDir, "settings.yaml"), []byte(`schema_version: "1"
active_profile: vertex
profiles:
  vertex:
    runtime: docker
harness_configs:
  claude-cfg:
    harness: claude
    env:
      GOOGLE_CLOUD_PROJECT: my-gcp-project
      GOOGLE_CLOUD_REGION: us-central1
runtimes:
  docker:
    type: docker
`), 0644)

	// Create project directory
	projectDir := filepath.Join(tmpDir, "project")
	projectScionDir := filepath.Join(projectDir, ".scion")
	_ = os.MkdirAll(projectScionDir, 0755)

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

	if got := envMap["GOOGLE_CLOUD_PROJECT"]; got != "my-gcp-project" {
		t.Errorf("GOOGLE_CLOUD_PROJECT = %q, want %q", got, "my-gcp-project")
	}
	if got := envMap["GOOGLE_CLOUD_REGION"]; got != "us-central1" {
		t.Errorf("GOOGLE_CLOUD_REGION = %q, want %q", got, "us-central1")
	}
}

func TestStartImageRegistryRewriteAfterOptsImage(t *testing.T) {
	// Verify that the image_registry rewrite is applied AFTER the opts.Image
	// override, so a bare image from the dispatch request gets rewritten.
	tmpDir := t.TempDir()

	oldWd, _ := os.Getwd()
	_ = os.Chdir(tmpDir)
	defer func() { _ = os.Chdir(oldWd) }()

	originalHome := os.Getenv("HOME")
	defer func() { _ = os.Setenv("HOME", originalHome) }()
	_ = os.Setenv("HOME", tmpDir)

	globalScionDir := filepath.Join(tmpDir, ".scion")

	hcDir := filepath.Join(globalScionDir, "harness-configs", "test-harness")
	_ = os.MkdirAll(hcDir, 0755)
	_ = os.WriteFile(filepath.Join(hcDir, "config.yaml"), []byte("harness: generic\nuser: scion\nimage: default-image:latest\n"), 0644)

	tplDir := filepath.Join(globalScionDir, "templates", "default")
	_ = os.MkdirAll(tplDir, 0755)
	_ = os.WriteFile(filepath.Join(tplDir, "scion-agent.json"), []byte(`{"default_harness_config": "test-harness"}`), 0644)

	_ = os.WriteFile(filepath.Join(globalScionDir, "settings.yaml"), []byte(`schema_version: "1"
active_profile: local
profiles:
  local:
    runtime: docker
    image_registry: us-docker.pkg.dev/my-project/scion
`), 0644)

	projectDir := filepath.Join(tmpDir, "project")
	projectScionDir := filepath.Join(projectDir, ".scion")
	_ = os.MkdirAll(projectScionDir, 0755)

	var capturedConfig runtime.RunConfig
	mockRT := &runtime.MockRuntime{
		ListFunc: func(ctx context.Context, labelFilter map[string]string) ([]api.AgentInfo, error) {
			return []api.AgentInfo{}, nil
		},
		RunFunc: func(ctx context.Context, cfg runtime.RunConfig) (string, error) {
			capturedConfig = cfg
			return "mock-id", nil
		},
		ImageExistsFunc: func(ctx context.Context, image string) (bool, error) {
			return false, nil
		},
	}

	mgr := NewManager(mockRT)

	_, err := mgr.Start(context.Background(), api.StartOptions{
		Name:        "test-agent",
		ProjectPath: projectScionDir,
		Image:       "scion-custom:latest",
		BrokerMode:  true,
		NoAuth:      true,
	})
	if err != nil {
		t.Fatalf("Start failed: %v", err)
	}

	want := "us-docker.pkg.dev/my-project/scion/scion-custom:latest"
	if capturedConfig.Image != want {
		t.Errorf("expected image %q after registry rewrite of opts.Image, got %q",
			want, capturedConfig.Image)
	}
}

// --- Gap 3 / Phase 5: harness-config env and the BrokerMode gate ---------
//
// These tests exercise resolveAuthEnvOverlay, the extracted
// injection-then-overlay sequence that Start runs immediately before
// GatherAuthWithEnv. They deliberately assert on the AUTH OVERLAY and not on
// the container environment: harness-config env already reaches the container
// in broker mode via provision.go's ungated finalScionCfg merge, so a
// container-env assertion is green before the fix and discriminates nothing.
// See design §0.2.

// g3TestSettings builds settings with one harness config and one profile,
// so tests can tell the two sources apart.
func g3TestSettings(hcEnv map[string]string) *config.VersionedSettings {
	return &config.VersionedSettings{
		SchemaVersion: "1",
		ActiveProfile: "vertex",
		HarnessConfigs: map[string]config.HarnessConfigEntry{
			"claude-cfg": {Harness: "claude", Env: hcEnv},
		},
		Profiles: map[string]config.V1ProfileConfig{
			"vertex": {Runtime: "docker"},
		},
	}
}

// TestStart_BrokerMode_HarnessConfigEnv_VisibleToAuthOverlay is the
// discriminating test for Gap 3. Before the fix the !opts.BrokerMode gate
// skipped injection entirely for hub-dispatched agents, so credentials the
// harness config declares were invisible to GatherAuthWithEnv.
func TestStart_BrokerMode_HarnessConfigEnv_VisibleToAuthOverlay(t *testing.T) {
	settings := g3TestSettings(map[string]string{
		"GOOGLE_CLOUD_PROJECT": "hc-project",
		"GOOGLE_CLOUD_REGION":  "us-central1",
	})

	opts := api.StartOptions{
		Name:       "test-agent",
		BrokerMode: true, // hub-dispatched: every agent in a hub deployment
		Env:        map[string]string{"EXISTING": "val"},
	}

	overlay := resolveAuthEnvOverlay(&opts, settings, "vertex", "claude-cfg")

	if got := overlay["GOOGLE_CLOUD_PROJECT"]; got != "hc-project" {
		t.Errorf("auth overlay GOOGLE_CLOUD_PROJECT = %q, want %q "+
			"(harness-config env must be visible to GatherAuthWithEnv in broker mode)",
			got, "hc-project")
	}
	if got := overlay["GOOGLE_CLOUD_REGION"]; got != "us-central1" {
		t.Errorf("auth overlay GOOGLE_CLOUD_REGION = %q, want %q", got, "us-central1")
	}
	if got := overlay["EXISTING"]; got != "val" {
		t.Errorf("auth overlay EXISTING = %q, want %q (pre-existing keys must survive)", got, "val")
	}
}

// TestStart_BrokerMode_HubEnvNotClobberedByHarnessConfigEnv guards the
// only-if-absent guard: hub-resolved env is already in opts.Env by the time
// injection runs, and it must win over the harness config's value.
func TestStart_BrokerMode_HubEnvNotClobberedByHarnessConfigEnv(t *testing.T) {
	settings := g3TestSettings(map[string]string{
		"GOOGLE_CLOUD_PROJECT": "hc-project",
		"HC_ONLY":              "hc-value",
	})

	opts := api.StartOptions{
		Name:       "test-agent",
		BrokerMode: true,
		// Hub-supplied value, placed in opts.Env by start_context.go.
		Env: map[string]string{"GOOGLE_CLOUD_PROJECT": "hub-project"},
	}

	overlay := resolveAuthEnvOverlay(&opts, settings, "vertex", "claude-cfg")

	if got := overlay["GOOGLE_CLOUD_PROJECT"]; got != "hub-project" {
		t.Errorf("auth overlay GOOGLE_CLOUD_PROJECT = %q, want %q (hub value must win)",
			got, "hub-project")
	}
	// opts.Env is what is later projected into the container, so assert there
	// too: acceptance criterion 15 is about the value that reaches the agent.
	if got := opts.Env["GOOGLE_CLOUD_PROJECT"]; got != "hub-project" {
		t.Errorf("opts.Env GOOGLE_CLOUD_PROJECT = %q, want %q (hub value must reach the container)",
			got, "hub-project")
	}
	// Polarity control: the non-colliding key still gets injected, proving the
	// guard is per-key and not an all-or-nothing bail-out.
	if got := overlay["HC_ONLY"]; got != "hc-value" {
		t.Errorf("auth overlay HC_ONLY = %q, want %q", got, "hc-value")
	}
}

// TestResolveAuthEnvOverlay_NoHarnessConfigMeansNoInjection verifies that
// with no harness config named, nothing is injected into the auth overlay.
// profiles.<p>.env has been fully removed from the struct, so there is no
// remaining path for profile env to reach the overlay.
func TestResolveAuthEnvOverlay_NoHarnessConfigMeansNoInjection(t *testing.T) {
	settings := g3TestSettings(nil)

	opts := api.StartOptions{Name: "test-agent", BrokerMode: true}

	overlay := resolveAuthEnvOverlay(&opts, settings, "vertex", "" /* no harness config */)

	if len(overlay) != 0 {
		t.Errorf("auth overlay = %v, want empty (with no harness config named, nothing should be injected)", overlay)
	}
}

// TestResolveAuthEnvOverlay_OnlyHarnessConfigEnvArrives verifies that the auth
// overlay receives only harness-config env. profiles.<p>.env has been fully
// removed from V1ProfileConfig, so there is no profile env to arrive.
func TestResolveAuthEnvOverlay_OnlyHarnessConfigEnvArrives(t *testing.T) {
	settings := g3TestSettings(map[string]string{"HC_ONLY": "hc-value"})

	opts := api.StartOptions{Name: "test-agent", BrokerMode: true}

	overlay := resolveAuthEnvOverlay(&opts, settings, "vertex", "claude-cfg")

	if got := overlay["HC_ONLY"]; got != "hc-value" {
		t.Fatalf("existence control failed: auth overlay HC_ONLY = %q, want %q — "+
			"the harness config was not resolved",
			got, "hc-value")
	}
}

// TestResolveAuthEnvOverlay_NilSettings guards the nil path.
func TestResolveAuthEnvOverlay_NilSettings(t *testing.T) {
	opts := api.StartOptions{Name: "test-agent", BrokerMode: true, Env: map[string]string{"A": "1"}}

	overlay := resolveAuthEnvOverlay(&opts, nil, "vertex", "claude-cfg")

	if got := overlay["A"]; got != "1" {
		t.Errorf("auth overlay A = %q, want %q", got, "1")
	}
}

// --- autoDetectAuthSelectedType: ptone/scion#1873 ---------------------------
//
// These pin the fix for the antigravity vertex-ai-via-passthrough gap: Start()
// used to leave auth.SelectedType empty whenever nothing explicit chose one,
// deferring entirely to the container-side provisioner's own (harness-local,
// default_type-blind) guess. autoDetectAuthSelectedType makes Go the single
// source of truth, reusing the same file -> env -> GCP-identity precedence
// the hub preflight (extractRequiredEnvKeys) already uses.

// antigravityLikeAuthMeta returns auth metadata shaped like
// harnesses/antigravity/config.yaml's auth block, minus the ambient
// GOOGLE_CLOUD_PROJECT->vertex-ai env autodetect entry — so tests that use it
// isolate the GCP-identity leg instead of the (separate, pre-existing,
// already-tested) env-presence leg.
func antigravityLikeAuthMeta() *config.HarnessAuthMetadata {
	return &config.HarnessAuthMetadata{
		DefaultType: "oauth-token",
		Types: map[string]config.HarnessAuthTypeMetadata{
			"oauth-token": {
				RequiredEnv: []config.HarnessAuthEnvRequirement{
					{AnyOf: []string{"AGY_TOKEN"}},
				},
			},
			"api-key": {
				RequiredEnv: []config.HarnessAuthEnvRequirement{
					{AnyOf: []string{"GEMINI_API_KEY", "GOOGLE_API_KEY"}},
				},
			},
			"vertex-ai": {
				RequiredEnv: []config.HarnessAuthEnvRequirement{
					{AnyOf: []string{"GOOGLE_CLOUD_PROJECT"}},
					{AnyOf: []string{"GOOGLE_CLOUD_LOCATION", "GOOGLE_CLOUD_REGION"}},
				},
				RequiredFiles: []config.HarnessAuthFileRequirement{
					{
						Name: "gcloud-adc", Type: "file", Field: "GoogleAppCredentials",
						AlternativeEnvKeys:                   []string{"GOOGLE_APPLICATION_CREDENTIALS"},
						SkippedWhenGCPServiceAccountAssigned: true,
						Required:                             true,
					},
				},
			},
		},
		Autodetect: config.HarnessAuthAutodetect{
			Env: map[string]string{
				"AGY_TOKEN":      "oauth-token",
				"GEMINI_API_KEY": "api-key",
				"GOOGLE_API_KEY": "api-key",
			},
			Files: map[string]string{"gcloud-adc": "vertex-ai"},
		},
	}
}

// TestAutoDetectAuthSelectedType_GCPIdentityPassthrough proves the core
// target scenario: with a GCP SA reachable via passthrough (signalled by
// SCION_METADATA_MODE, the only channel this crosses into pkg/agent) and no
// other credential present at all, vertex-ai is selected from identity alone
// — with no ADC file, no AGY_TOKEN, no API key. BrokerMode: true because the
// identity signal only exists on the broker path (R1).
func TestAutoDetectAuthSelectedType_GCPIdentityPassthrough(t *testing.T) {
	auth := &api.AuthConfig{}
	opts := &api.StartOptions{
		BrokerMode: true,
		Env: map[string]string{
			"SCION_METADATA_MODE": "passthrough",
		},
	}

	autoDetectAuthSelectedType(auth, antigravityLikeAuthMeta(), opts)

	if auth.SelectedType != "vertex-ai" {
		t.Errorf("SelectedType = %q, want %q (GCP SA reachable via passthrough)", auth.SelectedType, "vertex-ai")
	}
}

// TestAutoDetectAuthSelectedType_GCPIdentityAssign is the "assign" twin of
// the passthrough test above.
func TestAutoDetectAuthSelectedType_GCPIdentityAssign(t *testing.T) {
	auth := &api.AuthConfig{}
	opts := &api.StartOptions{
		BrokerMode: true,
		Env: map[string]string{
			"SCION_METADATA_MODE": "assign",
		},
	}

	autoDetectAuthSelectedType(auth, antigravityLikeAuthMeta(), opts)

	if auth.SelectedType != "vertex-ai" {
		t.Errorf("SelectedType = %q, want %q (GCP SA assigned)", auth.SelectedType, "vertex-ai")
	}
}

// TestAutoDetectAuthSelectedType_BlockModeNoAutoVertexAI is the regression
// guard for "no SA means no vertex-ai auto-selection from identity": block
// mode (or no GCP identity signal at all) must not auto-select vertex-ai.
func TestAutoDetectAuthSelectedType_BlockModeNoAutoVertexAI(t *testing.T) {
	for _, metadataMode := range []string{"block", ""} {
		t.Run("mode="+metadataMode, func(t *testing.T) {
			auth := &api.AuthConfig{}
			opts := &api.StartOptions{BrokerMode: true, Env: map[string]string{}}
			if metadataMode != "" {
				opts.Env["SCION_METADATA_MODE"] = metadataMode
			}

			autoDetectAuthSelectedType(auth, antigravityLikeAuthMeta(), opts)

			if auth.SelectedType != "" {
				t.Errorf("SelectedType = %q, want %q (no GCP SA available, must not auto-select vertex-ai)", auth.SelectedType, "")
			}
		})
	}
}

// TestAutoDetectAuthSelectedType_FileSecretBeatsIdentity proves the file leg
// runs first: an actually-staged ADC file secret should win even without any
// identity signal at all (e.g. plain local ADC login, no hub-managed GCP
// identity).
func TestAutoDetectAuthSelectedType_FileSecretBeatsIdentity(t *testing.T) {
	auth := &api.AuthConfig{}
	opts := &api.StartOptions{
		BrokerMode: true,
		Env:        map[string]string{}, // no SCION_METADATA_MODE at all
		ResolvedSecrets: []api.ResolvedSecret{
			{Name: "gcloud-adc", Type: "file", Target: "/home/scion/.config/gcloud/application_default_credentials.json"},
		},
	}

	autoDetectAuthSelectedType(auth, antigravityLikeAuthMeta(), opts)

	if auth.SelectedType != "vertex-ai" {
		t.Errorf("SelectedType = %q, want %q (staged ADC file present)", auth.SelectedType, "vertex-ai")
	}
}

// TestAutoDetectAuthSelectedType_EnvCredentialBeatsIdentity proves the env
// leg outranks the GCP-identity leg: an actually-present, non-default-type
// API key alongside a reachable GCP SA should still pick api-key, not
// vertex-ai — identity is the fallback, not a takeover.
func TestAutoDetectAuthSelectedType_EnvCredentialBeatsIdentity(t *testing.T) {
	auth := &api.AuthConfig{}
	opts := &api.StartOptions{
		BrokerMode: true,
		Env: map[string]string{
			"SCION_METADATA_MODE": "passthrough",
			"GEMINI_API_KEY":      "some-key",
		},
	}

	autoDetectAuthSelectedType(auth, antigravityLikeAuthMeta(), opts)

	if auth.SelectedType != "api-key" {
		t.Errorf("SelectedType = %q, want %q (GEMINI_API_KEY present should win over bare identity)", auth.SelectedType, "api-key")
	}
}

// TestAutoDetectAuthSelectedType_DefaultTypeCredentialBeatsIdentity is the
// pkg/agent regression pin for ptone/scion#1882 — the exact scenario the
// review found: antigravity's own default_type is "oauth-token", and AGY_TOKEN
// (which maps to it) must still win over a reachable GCP SA. Before the fix,
// AutoDetectAuthType's env leg returning "" (pickAutodetectCandidate's
// "already on default" signal, indistinguishable from "nothing matched" to a
// naively-chaining caller) let this function fall through to the identity leg
// and wrongly select vertex-ai, silently evicting AGY_TOKEN from the
// container. See TestAutoDetectAuthType_DefaultTypeCredentialBeatsGCPIdentity
// in pkg/harness for the same pin against the real harness configs
// (including claude's ANTHROPIC_API_KEY case).
func TestAutoDetectAuthSelectedType_DefaultTypeCredentialBeatsIdentity(t *testing.T) {
	auth := &api.AuthConfig{}
	opts := &api.StartOptions{
		BrokerMode: true,
		Env: map[string]string{
			"SCION_METADATA_MODE": "passthrough",
			"AGY_TOKEN":           "some-token",
		},
	}

	autoDetectAuthSelectedType(auth, antigravityLikeAuthMeta(), opts)

	if auth.SelectedType != "oauth-token" {
		t.Errorf("SelectedType = %q, want %q (AGY_TOKEN — antigravity's default_type credential — must win over a reachable GCP SA)", auth.SelectedType, "oauth-token")
	}
}

// TestAutoDetectAuthSelectedType_ExplicitSelectionWins proves the function is
// a no-op once something explicit (CLI --harness-auth, template, profile,
// scion-agent.json) has already set SelectedType, even in a passthrough
// environment that would otherwise auto-select vertex-ai.
func TestAutoDetectAuthSelectedType_ExplicitSelectionWins(t *testing.T) {
	auth := &api.AuthConfig{SelectedType: "oauth-token"}
	opts := &api.StartOptions{
		BrokerMode: true,
		Env: map[string]string{
			"SCION_METADATA_MODE": "passthrough",
		},
	}

	autoDetectAuthSelectedType(auth, antigravityLikeAuthMeta(), opts)

	if auth.SelectedType != "oauth-token" {
		t.Errorf("SelectedType = %q, want %q (explicit selection must not be overridden)", auth.SelectedType, "oauth-token")
	}
}

// TestAutoDetectAuthSelectedType_NilAuthMetaNoOp guards the nil-authMeta path
// (harness configs without a declarative auth: block).
func TestAutoDetectAuthSelectedType_NilAuthMetaNoOp(t *testing.T) {
	auth := &api.AuthConfig{}
	opts := &api.StartOptions{BrokerMode: true, Env: map[string]string{"SCION_METADATA_MODE": "passthrough"}}

	autoDetectAuthSelectedType(auth, nil, opts)

	if auth.SelectedType != "" {
		t.Errorf("SelectedType = %q, want %q (nil authMeta must not auto-select anything)", auth.SelectedType, "")
	}
}

// TestAutoDetectAuthSelectedType_LocalModeNoOp is the R1 regression guard:
// in local/workstation mode (BrokerMode: false), this function must not make
// a binding decision from opts.Env/opts.ResolvedSecrets alone. Those only
// reflect what the broker pre-resolved; local mode's GatherAuthWithEnv
// separately discovers host env vars (os.Getenv) and host credential files
// (e.g. ~/.claude/.credentials.json, the local ADC file) that never pass
// through opts at all, and ResolveAuth / provision.py's own ordering already
// handles that case correctly. The identity signal is also broker-only by
// construction, so it has nothing to contribute locally.
func TestAutoDetectAuthSelectedType_LocalModeNoOp(t *testing.T) {
	auth := &api.AuthConfig{}
	opts := &api.StartOptions{
		BrokerMode: false,
		Env: map[string]string{
			// Would resolve to vertex-ai via identity in broker mode.
			"SCION_METADATA_MODE": "passthrough",
		},
		ResolvedSecrets: []api.ResolvedSecret{
			// Would resolve to vertex-ai via file secret in broker mode.
			{Name: "gcloud-adc", Type: "file", Target: "/home/scion/.config/gcloud/application_default_credentials.json"},
		},
	}

	autoDetectAuthSelectedType(auth, antigravityLikeAuthMeta(), opts)

	if auth.SelectedType != "" {
		t.Errorf("SelectedType = %q, want %q (local mode must defer to GatherAuthWithEnv's own host discovery, not opts alone)", auth.SelectedType, "")
	}
}

// TestAutoDetectAuthSelectedType_Seam_ResolveAuthForwardsSelectedType is the
// R2 Start()-wiring test: it exercises the actual seam Start() relies on —
// autoDetectAuthSelectedType's result flowing into
// ContainerScriptHarness.ResolveAuth as SCION_HARNESS_SELECTED_AUTH, which
// container_script_harness.go stages as auth-candidates.json's
// "explicit_type" and the container-side provisioner (e.g. antigravity's
// provision.py) trusts instead of re-guessing.
func TestAutoDetectAuthSelectedType_Seam_ResolveAuthForwardsSelectedType(t *testing.T) {
	h, err := harness.NewContainerScriptHarness("/fake/harness-config-dir", config.HarnessConfigEntry{
		Harness:     "antigravity",
		Provisioner: &config.HarnessProvisionerConfig{Type: "container-script"},
		Auth:        antigravityLikeAuthMeta(),
	})
	if err != nil {
		t.Fatalf("NewContainerScriptHarness: %v", err)
	}

	auth := api.AuthConfig{}
	opts := &api.StartOptions{
		BrokerMode: true,
		Env:        map[string]string{"SCION_METADATA_MODE": "passthrough"},
	}
	autoDetectAuthSelectedType(&auth, antigravityLikeAuthMeta(), opts)
	if auth.SelectedType != "vertex-ai" {
		t.Fatalf("precondition failed: SelectedType = %q, want %q", auth.SelectedType, "vertex-ai")
	}

	resolved, err := h.ResolveAuth(auth)
	if err != nil {
		t.Fatalf("ResolveAuth: %v", err)
	}
	if got := resolved.EnvVars["SCION_HARNESS_SELECTED_AUTH"]; got != "vertex-ai" {
		t.Errorf("resolved.EnvVars[SCION_HARNESS_SELECTED_AUTH] = %q, want %q — this is what the container-side provisioner trusts as explicit_type",
			got, "vertex-ai")
	}
}

// TestAutoDetectAuthSelectedType_Seam_DefaultTypeCredentialSurvivesResolveAuth
// is the Start()-wiring counterpart to
// TestAutoDetectAuthSelectedType_DefaultTypeCredentialBeatsIdentity: it
// confirms the seam forwards "oauth-token", not "vertex-ai", so
// provision.py's explicit_type never evicts AGY_TOKEN even when a GCP SA is
// also reachable (C1, end to end through the actual ResolveAuth call).
func TestAutoDetectAuthSelectedType_Seam_DefaultTypeCredentialSurvivesResolveAuth(t *testing.T) {
	h, err := harness.NewContainerScriptHarness("/fake/harness-config-dir", config.HarnessConfigEntry{
		Harness:     "antigravity",
		Provisioner: &config.HarnessProvisionerConfig{Type: "container-script"},
		Auth:        antigravityLikeAuthMeta(),
	})
	if err != nil {
		t.Fatalf("NewContainerScriptHarness: %v", err)
	}

	auth := api.AuthConfig{}
	opts := &api.StartOptions{
		BrokerMode: true,
		Env: map[string]string{
			"SCION_METADATA_MODE": "passthrough",
			"AGY_TOKEN":           "some-token",
		},
	}
	autoDetectAuthSelectedType(&auth, antigravityLikeAuthMeta(), opts)
	if auth.SelectedType != "oauth-token" {
		t.Fatalf("precondition failed: SelectedType = %q, want %q", auth.SelectedType, "oauth-token")
	}

	resolved, err := h.ResolveAuth(auth)
	if err != nil {
		t.Fatalf("ResolveAuth: %v", err)
	}
	if got := resolved.EnvVars["SCION_HARNESS_SELECTED_AUTH"]; got != "oauth-token" {
		t.Errorf("resolved.EnvVars[SCION_HARNESS_SELECTED_AUTH] = %q, want %q (AGY_TOKEN's type must survive to explicit_type, not be evicted by identity)",
			got, "oauth-token")
	}
}

// antigravityLikeAuthMetaYAML is antigravityLikeAuthMeta's config.yaml auth
// block, staged on disk for TestStartBrokerMode_AutoDetectsAuthSelectedType
// below — that test needs a real container-script harness-config directory
// (harness.Resolve requires one on disk), not the in-memory
// config.HarnessConfigEntry the seam tests above construct directly.
const antigravityLikeAuthMetaYAML = `
auth:
  default_type: oauth-token
  types:
    oauth-token:
      required_env:
        - any_of: ["AGY_TOKEN"]
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
          field: GoogleAppCredentials
          alternative_env_keys: ["GOOGLE_APPLICATION_CREDENTIALS"]
          skipped_when_gcp_service_account_assigned: true
          required: true
  autodetect:
    env:
      AGY_TOKEN: oauth-token
      GEMINI_API_KEY: api-key
      GOOGLE_API_KEY: api-key
    files:
      gcloud-adc: vertex-ai
`

// TestStartBrokerMode_AutoDetectsAuthSelectedType is a full, broker-mode
// Start() test (ptone/scion#1882) proving the wiring at the
// autoDetectAuthSelectedType(&auth, authMeta, &opts) call in Start() actually
// reaches the container, using a real on-disk container-script harness-config
// (not the in-memory entry the seam tests above use) and a real
// MockRuntime.Run capture — the same TestStartBrokerMode_EmptyEnvNotFatal
// pattern.
//
// Asserts on capturedConfig.ResolvedAuth.EnvVars, not capturedConfig.Env:
// SCION_HARNESS_SELECTED_AUTH is forwarded via ContainerScriptHarness.ResolveAuth's
// returned api.ResolvedAuth (run.go's runCfg.ResolvedAuth = resolvedAuth), not
// merged into the flat container env slice (that slice comes from
// buildAgentEnv(finalScionCfg, opts.Env) — a separate path auth EnvVars never
// join at the Go level; runtime implementations read ResolvedAuth.EnvVars
// directly when actually launching a container, see e.g. pkg/runtime/common.go).
func TestStartBrokerMode_AutoDetectsAuthSelectedType(t *testing.T) {
	cases := []struct {
		name string
		env  map[string]string
		want string
	}{
		{
			name: "GCPIdentityPassthrough_SelectsVertexAI",
			env: map[string]string{
				"SCION_METADATA_MODE":   "passthrough",
				"GOOGLE_CLOUD_PROJECT":  "my-gcp-project",
				"GOOGLE_CLOUD_LOCATION": "global",
			},
			want: "vertex-ai",
		},
		{
			name: "DefaultTypeCredentialBeatsIdentity_SelectsOAuthToken",
			env: map[string]string{
				"SCION_METADATA_MODE": "passthrough",
				"AGY_TOKEN":           "some-token",
			},
			want: "oauth-token",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tmpDir := t.TempDir()

			t.Chdir(tmpDir)
			t.Setenv("HOME", tmpDir)

			globalScionDir := filepath.Join(tmpDir, ".scion")

			// A real on-disk container-script harness-config: harness.Resolve
			// requires hcDir.Path to be non-empty for the provisioner branch
			// (pkg/harness/resolve.go), which the in-memory entries the seam
			// tests use above cannot satisfy.
			hcDir := filepath.Join(globalScionDir, "harness-configs", "antigravity-test")
			if err := os.MkdirAll(hcDir, 0755); err != nil {
				t.Fatalf("mkdir harness-config dir: %v", err)
			}
			hcYAML := "harness: antigravity-test\nuser: scion\nimage: test-image:latest\n" +
				"provisioner:\n  type: container-script\n  command: [\"python3\", \"provision.py\"]\n" +
				antigravityLikeAuthMetaYAML
			if err := os.WriteFile(filepath.Join(hcDir, "config.yaml"), []byte(hcYAML), 0644); err != nil {
				t.Fatalf("write harness-config config.yaml: %v", err)
			}

			tplDir := filepath.Join(globalScionDir, "templates", "default")
			if err := os.MkdirAll(tplDir, 0755); err != nil {
				t.Fatalf("mkdir template dir: %v", err)
			}
			if err := os.WriteFile(filepath.Join(tplDir, "scion-agent.json"), []byte(`{"default_harness_config": "antigravity-test"}`), 0644); err != nil {
				t.Fatalf("write template scion-agent.json: %v", err)
			}

			if err := os.WriteFile(filepath.Join(globalScionDir, "settings.yaml"), []byte(`schema_version: "1"
active_profile: local
profiles:
  local:
    runtime: docker
`), 0644); err != nil {
				t.Fatalf("write global settings.yaml: %v", err)
			}

			projectDir := filepath.Join(tmpDir, "project")
			projectScionDir := filepath.Join(projectDir, ".scion")
			if err := os.MkdirAll(projectScionDir, 0755); err != nil {
				t.Fatalf("mkdir project .scion dir: %v", err)
			}

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
				BrokerMode:  true,
				Env:         tc.env,
			})
			if err != nil {
				t.Fatalf("Start failed: %v", err)
			}

			if capturedConfig.ResolvedAuth == nil {
				t.Fatal("capturedConfig.ResolvedAuth is nil")
			}
			if got := capturedConfig.ResolvedAuth.EnvVars["SCION_HARNESS_SELECTED_AUTH"]; got != tc.want {
				t.Errorf("ResolvedAuth.EnvVars[SCION_HARNESS_SELECTED_AUTH] = %q, want %q", got, tc.want)
			}
		})
	}
}

// --- Gap 3 follow-up: the RANK limb -----------------------------------------
//
// Design §0.2 item 2: Gap 3 has a rank limb as well as a presence limb. The
// presence limb (harness-config env becomes VISIBLE to the auth overlay in
// broker mode) is pinned by the tests above. The rank limb is pinned here.
//
// Mechanism: resolveAuthEnvOverlay injects harness-config env into opts.Env
// through the *api.StartOptions pointer, and Start later calls
// buildAgentEnv(finalScionCfg, opts.Env) at run.go:807 — where opts.Env is
// extraEnv and OVERRIDES scionCfg.Env. Template env lives in finalScionCfg.Env.
// So in broker mode harness-config env now outranks template env in the
// container, where before the !opts.BrokerMode conjunct it lost.

// rankProbeFixture stands up a tmp HOME in which the SAME key is declared by
// both the template (RANK_PROBE=from-template, inside finalScionCfg.Env) and
// the settings harness config (RANK_PROBE=from-harness-config, which reaches
// opts.Env only via resolveAuthEnvOverlay). Returns the project .scion path.
func rankProbeFixture(t *testing.T) string {
	t.Helper()
	tmpDir := t.TempDir()

	oldWd, _ := os.Getwd()
	t.Cleanup(func() { _ = os.Chdir(oldWd) })
	_ = os.Chdir(tmpDir)

	originalHome := os.Getenv("HOME")
	t.Cleanup(func() { _ = os.Setenv("HOME", originalHome) })
	_ = os.Setenv("HOME", tmpDir)

	if old, ok := os.LookupEnv("RANK_PROBE"); ok {
		t.Cleanup(func() { _ = os.Setenv("RANK_PROBE", old) })
		_ = os.Unsetenv("RANK_PROBE")
	}

	globalScionDir := filepath.Join(tmpDir, ".scion")

	hcDir := filepath.Join(globalScionDir, "harness-configs", "claude-cfg")
	_ = os.MkdirAll(hcDir, 0755)
	_ = os.WriteFile(filepath.Join(hcDir, "config.yaml"),
		[]byte("harness: claude\nuser: scion\nimage: test-image:latest\n"), 0644)

	// Template declares RANK_PROBE — this lands in finalScionCfg.Env, the BASE
	// map for buildAgentEnv.
	tplDir := filepath.Join(globalScionDir, "templates", "default")
	_ = os.MkdirAll(tplDir, 0755)
	_ = os.WriteFile(filepath.Join(tplDir, "scion-agent.json"),
		[]byte(`{"default_harness_config": "claude-cfg", "env": {"RANK_PROBE": "from-template"}}`), 0644)

	// Settings harness config declares the SAME key with a different value.
	_ = os.WriteFile(filepath.Join(globalScionDir, "settings.yaml"), []byte(`schema_version: "1"
active_profile: vertex
profiles:
  vertex:
    runtime: docker
harness_configs:
  claude-cfg:
    harness: claude
    env:
      RANK_PROBE: from-harness-config
runtimes:
  docker:
    type: docker
`), 0644)

	projectScionDir := filepath.Join(tmpDir, "project", ".scion")
	_ = os.MkdirAll(projectScionDir, 0755)
	return projectScionDir
}

// rankProbeRunStart runs Start against a mock runtime and returns the CONTAINER
// env as a map. It deliberately reads runtime.RunConfig.Env — the container
// path — and not opts.Env: opts.Env is the thing being written, so asserting
// there would pass trivially. This is the mistake the abandoned §3.3.1 filter
// made (it satisfied an opts.Env assertion while the container still received
// the capability), and AC16a was rewritten because of it.
func rankProbeRunStart(t *testing.T, projectScionDir string, brokerMode bool) map[string]string {
	t.Helper()
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
		Name:        "rank-probe-agent",
		Template:    "default",
		ProjectPath: projectScionDir,
		NoAuth:      true,
		BrokerMode:  brokerMode,
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
	return envMap
}

// TestBrokerMode_HarnessConfigEnvOutranksTemplateEnv pins the RANK limb of
// Gap 3 (design §0.2 item 2).
//
// 🔴 IF YOU ARE ABOUT TO PUT THE INJECTION IN resolveAuthEnvOverlay BACK BEHIND
// A MODE CHECK, THIS TEST IS WHY YOU MAY NOT. The rank shift is an intended
// deliverable, not an accident of the presence fix. Restoring a
// `!opts.BrokerMode` conjunct flips this assertion back to "from-template".
func TestBrokerMode_HarnessConfigEnvOutranksTemplateEnv(t *testing.T) {
	projectScionDir := rankProbeFixture(t)

	envMap := rankProbeRunStart(t, projectScionDir, true /* brokerMode */)

	if got := envMap["RANK_PROBE"]; got != "from-harness-config" {
		t.Errorf("container env RANK_PROBE = %q, want %q\n"+
			"harness-config env must OUTRANK template env in broker mode: it is injected "+
			"into opts.Env, which is extraEnv at buildAgentEnv (run.go:807) and overrides "+
			"finalScionCfg.Env. Getting %q back means the injection is gated by mode again.",
			got, "from-harness-config", got)
	}
}

// TestLocalMode_HarnessConfigEnvOutranksTemplateEnv is the polarity control.
//
// NOTE: local mode is UNCHANGED by this commit — injection always ran here, so
// harness-config env already outranked template env before the fix. This test
// therefore asserts continuity, not a change. Its value is that it fails if a
// future "fix" inverts the precedence globally instead of only for broker mode,
// which the broker-mode test alone could not distinguish.
func TestLocalMode_HarnessConfigEnvOutranksTemplateEnv(t *testing.T) {
	projectScionDir := rankProbeFixture(t)

	envMap := rankProbeRunStart(t, projectScionDir, false /* local mode */)

	if got := envMap["RANK_PROBE"]; got != "from-harness-config" {
		t.Errorf("container env RANK_PROBE = %q, want %q (local mode precedence is unchanged)",
			got, "from-harness-config")
	}
}

// TestResolveAuthEnvOverlay_MutatesCallerOptsEnv closes I-4.
//
// resolveAuthEnvOverlay has a TWO-PART contract: it returns the auth overlay,
// AND it mutates opts.Env through the *api.StartOptions pointer. The second
// half is what feeds the container (buildAgentEnv's extraEnv), and it is what
// makes the rank limb above work. Every other test in this file asserts on the
// RETURNED overlay, so the reviewer was able to change the signature to a value
// receiver with the entire pkg/agent suite still green.
//
// 🔴 MEASURED, NOT ASSUMED — and the result is counter-intuitive. Flipping the
// signature to a value receiver fails ONLY the nil-map subtest below. The
// pre-existing-map subtest still PASSES, because Go maps are reference types:
// the copied StartOptions carries the same underlying map, so opts.Env[k] = v
// on the copy is still visible to the caller. The pointer is load-bearing for
// exactly one thing — the `opts.Env = make(...)` allocation on the nil path,
// which assigns to a FIELD and is lost on a copy.
//
// So the nil-map subtest is the one that closes I-4. Keep it. An I-4 test
// written only against a pre-populated map would be green under the very
// regression it is meant to catch.
func TestResolveAuthEnvOverlay_MutatesCallerOptsEnv(t *testing.T) {
	settings := g3TestSettings(map[string]string{
		"GOOGLE_CLOUD_PROJECT": "hc-project",
	})

	// Pins the injection contract. NOTE this subtest does NOT detect a value
	// receiver — see the comment above. It is here for the contract, not as
	// the I-4 guard.
	t.Run("injects into a pre-existing caller map", func(t *testing.T) {
		opts := api.StartOptions{
			Name:       "test-agent",
			BrokerMode: true,
			Env:        map[string]string{"EXISTING": "val"},
		}

		_ = resolveAuthEnvOverlay(&opts, settings, "vertex", "claude-cfg")

		if got := opts.Env["GOOGLE_CLOUD_PROJECT"]; got != "hc-project" {
			t.Errorf("CALLER's opts.Env[GOOGLE_CLOUD_PROJECT] = %q, want %q — the pointer "+
				"receiver is load-bearing: opts.Env is projected into the container via "+
				"buildAgentEnv(finalScionCfg, opts.Env). A value receiver compiles and "+
				"returns the right overlay, but the container gets nothing.", got, "hc-project")
		}
	})

	// 🔴 THIS is the I-4 guard. It is the only subtest that goes red on a value
	// receiver. Do not "simplify" it into the case above.
	t.Run("allocates a nil caller map", func(t *testing.T) {
		// opts.Env = make(...) inside the function assigns to a field, so it
		// reaches the caller only through the pointer. This is also the shape
		// Start actually uses: a hub-dispatched agent with no ResolvedEnv
		// arrives with opts.Env == nil.
		opts := api.StartOptions{Name: "test-agent", BrokerMode: true, Env: nil}

		_ = resolveAuthEnvOverlay(&opts, settings, "vertex", "claude-cfg")

		if opts.Env == nil {
			t.Fatal("CALLER's opts.Env is still nil — the allocation inside " +
				"resolveAuthEnvOverlay did not reach the caller")
		}
		if got := opts.Env["GOOGLE_CLOUD_PROJECT"]; got != "hc-project" {
			t.Errorf("CALLER's opts.Env[GOOGLE_CLOUD_PROJECT] = %q, want %q", got, "hc-project")
		}
	})
}

// TestReResolveModelAlias verifies the broker-side safety net that
// re-resolves leaked model aliases. When the hub dispatches SCION_MODEL with
// an unresolved alias (e.g. "large"), reResolveModelAlias should return the
// concrete model from finalScionCfg.Model.
func TestReResolveModelAlias(t *testing.T) {
	tests := []struct {
		name        string
		envModel    string
		cfg         *api.ScionConfig
		harnessName string
		wantModel   string
		wantResolv  bool
	}{
		{
			name:       "unresolved alias large is re-resolved",
			envModel:   "large",
			cfg:        &api.ScionConfig{Model: "gemini-3.1-pro-preview"},
			wantModel:  "gemini-3.1-pro-preview",
			wantResolv: true,
		},
		{
			name:       "unresolved alias small is re-resolved",
			envModel:   "small",
			cfg:        &api.ScionConfig{Model: "gemini-flash-lite"},
			wantModel:  "gemini-flash-lite",
			wantResolv: true,
		},
		{
			name:       "unresolved alias extra-large is re-resolved",
			envModel:   "extra-large",
			cfg:        &api.ScionConfig{Model: "gemini-3.1-pro-preview"},
			wantModel:  "gemini-3.1-pro-preview",
			wantResolv: true,
		},
		{
			name:       "uppercase alias shorthand is re-resolved",
			envModel:   "L",
			cfg:        &api.ScionConfig{Model: "gemini-3.1-pro-preview"},
			wantModel:  "gemini-3.1-pro-preview",
			wantResolv: true,
		},
		{
			name:       "concrete model name is not altered",
			envModel:   "gemini-3.1-pro-preview",
			cfg:        &api.ScionConfig{Model: "gemini-3.1-pro-preview"},
			wantModel:  "",
			wantResolv: false,
		},
		{
			name:       "concrete model differs from cfg but is not an alias",
			envModel:   "gemini-3.6-flash",
			cfg:        &api.ScionConfig{Model: "gemini-3.1-pro-preview"},
			wantModel:  "",
			wantResolv: false,
		},
		{
			name:       "nil config does not panic",
			envModel:   "large",
			cfg:        nil,
			wantModel:  "",
			wantResolv: false,
		},
		{
			name:       "empty envModel does not re-resolve",
			envModel:   "",
			cfg:        &api.ScionConfig{Model: "gemini-3.1-pro-preview"},
			wantModel:  "",
			wantResolv: false,
		},
		{
			name:       "empty config model does not re-resolve",
			envModel:   "large",
			cfg:        &api.ScionConfig{Model: ""},
			wantModel:  "",
			wantResolv: false,
		},
		// Regression coverage for ptone/scion#1869: when cfg.Model is itself
		// still the same unresolved alias as envModel (GetAgent had no local
		// template chain to resolve it against either), the broker falls
		// back to the harness's built-in model_aliases table instead of
		// no-op'ing and leaking the alias to the harness process.
		{
			name:        "both env and cfg carry the same unresolved alias falls back to built-in table",
			envModel:    "large",
			cfg:         &api.ScionConfig{Model: "large"},
			harnessName: "claude",
			wantModel:   "claude-opus-5-5",
			wantResolv:  true,
		},
		{
			name:        "nil cfg falls back to built-in table when harness is known",
			envModel:    "medium",
			cfg:         nil,
			harnessName: "claude",
			wantModel:   "claude-sonnet-5",
			wantResolv:  true,
		},
		{
			name:        "unresolved alias with unknown harness name does not re-resolve",
			envModel:    "large",
			cfg:         &api.ScionConfig{Model: "large"},
			harnessName: "not-a-real-harness",
			wantModel:   "",
			wantResolv:  false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := reResolveModelAlias(tt.envModel, tt.cfg, tt.harnessName)
			if ok != tt.wantResolv {
				t.Errorf("reResolveModelAlias() resolved = %v, want %v", ok, tt.wantResolv)
			}
			if got != tt.wantModel {
				t.Errorf("reResolveModelAlias() model = %q, want %q", got, tt.wantModel)
			}
		})
	}
}

// TestSortedEnvVarKeysOmitsValues verifies the helper backing the auth debug
// log line returns only key names, sorted, never the values -- so a caller
// formatting this result into a log message cannot accidentally print an env
// value alongside it.
func TestSortedEnvVarKeysOmitsValues(t *testing.T) {
	envVars := map[string]string{
		"ZEBRA_KEY": "should-not-appear-in-result",
		"ALPHA_KEY": "also-should-not-appear",
		"MID_KEY":   "value-must-not-appear-in-log",
	}

	got := sortedEnvVarKeys(envVars)

	want := []string{"ALPHA_KEY", "MID_KEY", "ZEBRA_KEY"}
	if len(got) != len(want) {
		t.Fatalf("sortedEnvVarKeys() = %v, want %v", got, want)
	}
	for i, k := range want {
		if got[i] != k {
			t.Errorf("sortedEnvVarKeys()[%d] = %q, want %q", i, got[i], k)
		}
	}

	for _, k := range got {
		for _, v := range envVars {
			if k == v {
				t.Errorf("sortedEnvVarKeys() returned a value (%q) instead of a key", k)
			}
		}
	}
}

func TestSortedEnvVarKeysEmpty(t *testing.T) {
	if got := sortedEnvVarKeys(nil); len(got) != 0 {
		t.Errorf("sortedEnvVarKeys(nil) = %v, want empty", got)
	}
}
