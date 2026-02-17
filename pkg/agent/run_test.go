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
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ptone/scion-agent/pkg/api"
	"github.com/ptone/scion-agent/pkg/runtime"
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

	env, warnings := buildAgentEnv(scionCfg, extraEnv)

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
			"SCION_HUB_URL":     "http://localhost:9810",
		}

		// Apply the override logic from Start()
		if scionCfg.Hub != nil && scionCfg.Hub.Endpoint != "" {
			extraEnv["SCION_HUB_ENDPOINT"] = scionCfg.Hub.Endpoint
			extraEnv["SCION_HUB_URL"] = scionCfg.Hub.Endpoint
		}

		env, _ := buildAgentEnv(scionCfg, extraEnv)

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
			"SCION_HUB_URL":     "https://hub.example.com",
		}

		env, _ := buildAgentEnv(scionCfg, extraEnv)

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

func TestStartDurationTimer(t *testing.T) {
	t.Run("stops container after duration", func(t *testing.T) {
		var mu sync.Mutex
		var stoppedID string

		mockRT := &runtime.MockRuntime{
			StopFunc: func(ctx context.Context, id string) error {
				mu.Lock()
				defer mu.Unlock()
				stoppedID = id
				return nil
			},
		}

		startDurationTimer(mockRT, "test-container", 50*time.Millisecond)

		// Wait for the timer to fire
		time.Sleep(150 * time.Millisecond)

		mu.Lock()
		defer mu.Unlock()
		if stoppedID != "test-container" {
			t.Errorf("expected Stop to be called with 'test-container', got %q", stoppedID)
		}
	})

	t.Run("no-op for zero duration", func(t *testing.T) {
		stopCalled := false
		mockRT := &runtime.MockRuntime{
			StopFunc: func(ctx context.Context, id string) error {
				stopCalled = true
				return nil
			},
		}

		startDurationTimer(mockRT, "test-container", 0)

		time.Sleep(50 * time.Millisecond)
		if stopCalled {
			t.Error("Stop should not be called for zero duration")
		}
	})

	t.Run("no-op for negative duration", func(t *testing.T) {
		stopCalled := false
		mockRT := &runtime.MockRuntime{
			StopFunc: func(ctx context.Context, id string) error {
				stopCalled = true
				return nil
			},
		}

		startDurationTimer(mockRT, "test-container", -1*time.Second)

		time.Sleep(50 * time.Millisecond)
		if stopCalled {
			t.Error("Stop should not be called for negative duration")
		}
	})
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
					Status:          "created", // stale status from runtime
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

	if result.Status != "running" {
		t.Errorf("expected Status = %q, got %q", "running", result.Status)
	}
}
