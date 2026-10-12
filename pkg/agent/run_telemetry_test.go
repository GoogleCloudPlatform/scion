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
	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/runtime"
)

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

	env, _, _, _ := buildAgentEnv(scionCfg, opts, nil, false)

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

	env, _, _, _ := buildAgentEnv(scionCfg, opts, nil, false)

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
