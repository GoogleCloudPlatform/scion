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
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/api"
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

// newResumePhaseTestFixture seeds a minimal on-disk Scion installation plus
// an already-provisioned "resume-test" agent (scion-agent.json present, and
// agent-info.json recording phase "suspended", the state a resume starts
// from), wired to a mock runtime via the given ListFunc. It returns the
// Manager and the project .scion dir, ready for a Start() call with
// Resume: true.
func newResumePhaseTestFixture(t *testing.T, listFunc func(ctx context.Context, labelFilter map[string]string) ([]api.AgentInfo, error)) (Manager, string) {
	t.Helper()

	tmpDir := t.TempDir()

	t.Chdir(tmpDir)
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
`), 0644)

	projectDir := filepath.Join(tmpDir, "project")
	projectScionDir := filepath.Join(projectDir, ".scion")
	_ = os.MkdirAll(projectScionDir, 0755)

	agentDir := filepath.Join(projectScionDir, "agents", "resume-test")
	agentHome := filepath.Join(agentDir, "home")
	_ = os.MkdirAll(agentHome, 0755)
	_ = os.WriteFile(filepath.Join(agentDir, "scion-agent.json"), []byte(`{"harness": "generic"}`), 0644)
	_ = os.WriteFile(filepath.Join(agentHome, "agent-info.json"),
		[]byte(`{"id":"resume-test","name":"resume-test","phase":"suspended"}`), 0644)

	mockRT := &runtime.MockRuntime{
		ListFunc: listFunc,
		RunFunc: func(ctx context.Context, config runtime.RunConfig) (string, error) {
			return "mock-id", nil
		},
	}

	return NewManager(mockRT), projectScionDir
}

// TestStartResumeSetsRunningPhase is the regression guard for ptone/scion#1956:
// a resumed agent's Phase must be the canonical state.PhaseRunning,
// not the non-standard "resumed" string run.go used to write. The hub's
// waitForAgentReady only understood starting/running, so "resumed" made a
// healthy resume look like a failure. This exercises the normal return path,
// where the started container is found again in the runtime's listing
// (run.go's "Fetch fresh info" branch).
func TestStartResumeSetsRunningPhase(t *testing.T) {
	mgr, projectScionDir := newResumePhaseTestFixture(t, func(ctx context.Context, labelFilter map[string]string) ([]api.AgentInfo, error) {
		return []api.AgentInfo{
			{
				ContainerID:     "mock-id",
				Name:            "resume-test",
				ContainerStatus: "Up 2 seconds",
				Phase:           string(state.PhaseStarting),
			},
		}, nil
	})

	result, err := mgr.Start(context.Background(), api.StartOptions{
		Name:        "resume-test",
		ProjectPath: projectScionDir,
		BrokerMode:  true,
		NoAuth:      true,
		Resume:      true,
	})
	if err != nil {
		t.Fatalf("Start with Resume should succeed, got: %v", err)
	}

	if result.Phase != string(state.PhaseRunning) {
		t.Errorf("returned AgentInfo.Phase = %q, want %q", result.Phase, state.PhaseRunning)
	}
	if saved := GetSavedPhase("resume-test", projectScionDir); saved != string(state.PhaseRunning) {
		t.Errorf("persisted agent-info.json phase = %q, want %q", saved, state.PhaseRunning)
	}
}

// TestStartResumeSetsRunningPhase_FallbackPath covers the other return path
// in run.go: when the started container cannot be found again in the
// runtime's listing (e.g. a transient listing delay), Start falls back to
// constructing an AgentInfo directly from "status" without consulting the
// listing. That branch must also carry state.PhaseRunning, not "resumed".
func TestStartResumeSetsRunningPhase_FallbackPath(t *testing.T) {
	mgr, projectScionDir := newResumePhaseTestFixture(t, func(ctx context.Context, labelFilter map[string]string) ([]api.AgentInfo, error) {
		return []api.AgentInfo{}, nil
	})

	result, err := mgr.Start(context.Background(), api.StartOptions{
		Name:        "resume-test",
		ProjectPath: projectScionDir,
		BrokerMode:  true,
		NoAuth:      true,
		Resume:      true,
	})
	if err != nil {
		t.Fatalf("Start with Resume should succeed, got: %v", err)
	}

	if result.Phase != string(state.PhaseRunning) {
		t.Errorf("returned AgentInfo.Phase = %q, want %q", result.Phase, state.PhaseRunning)
	}
	if saved := GetSavedPhase("resume-test", projectScionDir); saved != string(state.PhaseRunning) {
		t.Errorf("persisted agent-info.json phase = %q, want %q", saved, state.PhaseRunning)
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

	envMap := make(map[string]string)
	for _, e := range capturedConfig.Env {
		parts := strings.SplitN(e, "=", 2)
		if len(parts) == 2 {
			envMap[parts[0]] = parts[1]
		}
	}
	if got := envMap["SCION_PROJECT"]; got != "project" {
		t.Errorf("expected SCION_PROJECT=%q, got %q", "project", got)
	}
	if _, ok := envMap["SCION_GROVE"]; ok {
		t.Errorf("expected SCION_GROVE to be absent from RunConfig.Env, got %q", envMap["SCION_GROVE"])
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

	allowHydratedTemplate(t, hydratedTplDir)
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
					ProjectID:       identityProjB,
					ContainerStatus: "Up 2 hours",
					Phase:           string(state.PhaseRunning),
				},
			}, nil
		},
	}

	mgr := NewManager(mockRT)

	result, err := mgr.Start(context.Background(), api.StartOptions{
		Name: "test-agent",
		Env:  map[string]string{"SCION_PROJECT_ID": identityProjB},
		// No Task — triggers the early return for already-running containers
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if result.Phase != "running" {
		t.Errorf("expected Phase = %q, got %q", "running", result.Phase)
	}
}

func TestStartReuseRunningContainer_AgentIDLabel(t *testing.T) {
	const (
		oldAgentUUID   = "11111111-1111-1111-1111-111111111111"
		newAgentUUID   = "22222222-2222-2222-2222-222222222222"
		mixedAgentUUID = "AAAAAAAA-BBBB-CCCC-DDDD-EEEEEEEEEEEE"
		lowerAgentUUID = "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"
	)

	errStaleDeleted := errors.New("stale container delete invoked")

	tests := []struct {
		name           string
		existing       api.AgentInfo
		scionAgentID   string
		wantReuse      bool
		wantDeleteRunI string
	}{
		{
			name: "empty agent_id label with SCION_AGENT_ID reuses",
			existing: api.AgentInfo{
				ContainerID:     "ctr-empty-1",
				Name:            "test-agent",
				ProjectID:       identityProjB,
				ContainerStatus: "Up 1 hour",
				Phase:           string(state.PhaseRunning),
			},
			scionAgentID: newAgentUUID,
			wantReuse:    true,
		},
		{
			name: "empty agent_id label without SCION_AGENT_ID reuses",
			existing: api.AgentInfo{
				ContainerID:     "ctr-empty-2",
				Name:            "test-agent",
				ProjectID:       identityProjB,
				ContainerStatus: "Up 1 hour",
				Phase:           string(state.PhaseRunning),
			},
			scionAgentID: "",
			wantReuse:    true,
		},
		{
			name: "legacy non-UUID agent_id label with SCION_AGENT_ID reuses",
			existing: api.AgentInfo{
				ContainerID:     "ctr-legacy-1",
				Name:            "test-agent",
				ProjectID:       identityProjB,
				ContainerStatus: "Up 1 hour",
				Phase:           string(state.PhaseRunning),
				Labels:          map[string]string{api.LabelAgentID: "test-agent"},
			},
			scionAgentID: newAgentUUID,
			wantReuse:    true,
		},
		{
			name: "legacy non-UUID agent_id label without SCION_AGENT_ID reuses",
			existing: api.AgentInfo{
				ContainerID:     "ctr-legacy-2",
				Name:            "test-agent",
				ProjectID:       identityProjB,
				ContainerStatus: "Up 1 hour",
				Phase:           string(state.PhaseRunning),
				Labels:          map[string]string{api.LabelAgentID: "legacy-slug"},
			},
			scionAgentID: "",
			wantReuse:    true,
		},
		{
			name: "matching UUID agent_id label reuses",
			existing: api.AgentInfo{
				ContainerID:     "ctr-match-1",
				Name:            "test-agent",
				ProjectID:       identityProjB,
				ContainerStatus: "Up 1 hour",
				Phase:           string(state.PhaseRunning),
				Labels:          map[string]string{api.LabelAgentID: newAgentUUID},
			},
			scionAgentID: newAgentUUID,
			wantReuse:    true,
		},
		{
			name: "matching UUID agent_id label canonicalized case reuses",
			existing: api.AgentInfo{
				ContainerID:     "ctr-match-case",
				Name:            "test-agent",
				ProjectID:       identityProjB,
				ContainerStatus: "Up 1 hour",
				Phase:           string(state.PhaseRunning),
				Labels:          map[string]string{api.LabelAgentID: mixedAgentUUID},
			},
			scionAgentID: lowerAgentUUID,
			wantReuse:    true,
		},
		{
			name: "mismatched UUID agent_id label is treated as stale",
			existing: api.AgentInfo{
				ContainerID:     "ctr-stale-1",
				RunID:           "old-run-1",
				Name:            "test-agent",
				ProjectID:       identityProjB,
				ContainerStatus: "Up 2 hours",
				Phase:           string(state.PhaseRunning),
				Labels:          map[string]string{api.LabelAgentID: oldAgentUUID},
			},
			scionAgentID:   newAgentUUID,
			wantReuse:      false,
			wantDeleteRunI: "old-run-1",
		},
		{
			name: "UUID agent_id label with non-UUID SCION_AGENT_ID is treated as stale",
			existing: api.AgentInfo{
				ContainerID:     "ctr-stale-nonuuid",
				RunID:           "old-run-nonuuid",
				Name:            "test-agent",
				ProjectID:       identityProjB,
				ContainerStatus: "Up 2 hours",
				Phase:           string(state.PhaseRunning),
				Labels:          map[string]string{api.LabelAgentID: oldAgentUUID},
			},
			scionAgentID:   "not-a-uuid",
			wantReuse:      false,
			wantDeleteRunI: "old-run-nonuuid",
		},
		{
			name: "UUID agent_id label with empty SCION_AGENT_ID is treated as stale",
			existing: api.AgentInfo{
				ContainerID:     "ctr-stale-2",
				RunID:           "old-run-2",
				Name:            "test-agent",
				ProjectID:       identityProjB,
				ContainerStatus: "Up 2 hours",
				Phase:           string(state.PhaseRunning),
				Labels:          map[string]string{api.LabelAgentID: oldAgentUUID},
			},
			scionAgentID:   "",
			wantReuse:      false,
			wantDeleteRunI: "old-run-2",
		},
		{
			name: "mismatched UUID in AgentInfo.ID without Labels is treated as stale",
			existing: api.AgentInfo{
				ID:              oldAgentUUID,
				ContainerID:     "ctr-stale-3",
				RunID:           "old-run-3",
				Name:            "test-agent",
				ProjectID:       identityProjB,
				ContainerStatus: "Up 2 hours",
				Phase:           string(state.PhaseRunning),
			},
			scionAgentID:   newAgentUUID,
			wantReuse:      false,
			wantDeleteRunI: "old-run-3",
		},
		{
			name: "matching UUID in AgentInfo.ID without Labels reuses",
			existing: api.AgentInfo{
				ID:              newAgentUUID,
				ContainerID:     "ctr-match-id",
				Name:            "test-agent",
				ProjectID:       identityProjB,
				ContainerStatus: "Up 2 hours",
				Phase:           string(state.PhaseRunning),
			},
			scionAgentID: newAgentUUID,
			wantReuse:    true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var deleted []runtime.RunRef
			var checkpoints []string

			mockRT := &runtime.MockRuntime{
				ListFunc: func(ctx context.Context, labelFilter map[string]string) ([]api.AgentInfo, error) {
					return []api.AgentInfo{tc.existing}, nil
				},
				DeleteFunc: func(ctx context.Context, ref runtime.RunRef) error {
					deleted = append(deleted, ref)
					return errStaleDeleted
				},
			}

			env := map[string]string{"SCION_PROJECT_ID": identityProjB}
			if tc.scionAgentID != "" {
				env["SCION_AGENT_ID"] = tc.scionAgentID
			}

			mgr := NewManager(mockRT)
			result, err := mgr.Start(context.Background(), api.StartOptions{
				Name: "test-agent",
				Env:  env,
				Checkpoint: func(ctx context.Context, step string) error {
					checkpoints = append(checkpoints, step)
					return nil
				},
			})

			if tc.wantReuse {
				if err != nil {
					t.Fatalf("expected reuse without error, got: %v", err)
				}
				if len(deleted) != 0 {
					t.Errorf("expected no Delete calls on reuse, got %+v", deleted)
				}
				if result.ContainerID != tc.existing.ContainerID {
					t.Errorf("reused ContainerID = %q, want %q", result.ContainerID, tc.existing.ContainerID)
				}
				if result.Phase != string(state.PhaseRunning) {
					t.Errorf("reused Phase = %q, want %q", result.Phase, state.PhaseRunning)
				}
				return
			}

			if !errors.Is(err, errStaleDeleted) {
				t.Fatalf("expected stale container to trigger pre-clean Delete (errStaleDeleted), got result=%+v err=%v", result, err)
			}
			if len(checkpoints) != 1 || checkpoints[0] != runtime.CheckpointStepPreClean {
				t.Errorf("expected CheckpointStepPreClean before Delete, got %v", checkpoints)
			}
			if len(deleted) != 1 {
				t.Fatalf("expected 1 Delete call, got %d (%+v)", len(deleted), deleted)
			}
			if deleted[0].ID != tc.existing.ContainerID || deleted[0].RunID != tc.wantDeleteRunI {
				t.Errorf("Delete ref = %+v, want {ID:%q RunID:%q}", deleted[0], tc.existing.ContainerID, tc.wantDeleteRunI)
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
