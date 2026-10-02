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
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
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
	"github.com/GoogleCloudPlatform/scion/pkg/util"
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

// TestStart_RejectsPersistedWorkspaceSourceEqualToFilesystemRoot is the fail-closed
// regression test for a workspace source that was resolved and persisted to
// scion-agent.json by an older version of the resolution logic, before the
// per-project workspace source validation existed. Start must still refuse
// it on resume, and — critically — must not reach the point of calling the
// runtime to set up the container: RunFunc must never be invoked.
func TestStart_RejectsPersistedWorkspaceSourceEqualToFilesystemRoot(t *testing.T) {
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

	runCalled := false
	mockRT := &runtime.MockRuntime{
		ListFunc: func(ctx context.Context, labelFilter map[string]string) ([]api.AgentInfo, error) {
			return []api.AgentInfo{}, nil
		},
		RunFunc: func(ctx context.Context, config runtime.RunConfig) (string, error) {
			runCalled = true
			return "mock-id", nil
		},
	}

	agentDir := filepath.Join(projectScionDir, "agents", "local-test")
	_ = os.MkdirAll(filepath.Join(agentDir, "home"), 0755)
	_ = os.WriteFile(filepath.Join(agentDir, "scion-agent.json"), []byte(`{
		"harness": "generic",
		"explicit_workspace": true,
		"volumes": [{"source": "/", "target": "/workspace"}]
	}`), 0644)

	mgr := NewManager(mockRT)

	_, err := mgr.Start(context.Background(), api.StartOptions{
		Name:        "local-test",
		ProjectPath: projectScionDir,
		NoAuth:      true,
	})
	if err == nil {
		t.Fatal("expected Start to fail for a persisted workspace source of '/'")
	}
	if !strings.Contains(err.Error(), "is not an allowed workspace path") {
		t.Errorf("expected the rejection to come from workspace source validation, got: %v", err)
	}
	if runCalled {
		t.Error("expected the runtime's Run to never be invoked (fail closed), but it was called")
	}
}

// TestStart_RejectsPersistedWorkspaceSourceEqualToHome is the $HOME sibling
// of TestStart_RejectsPersistedWorkspaceSourceEqualToFilesystemRoot.
func TestStart_RejectsPersistedWorkspaceSourceEqualToHome(t *testing.T) {
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

	runCalled := false
	mockRT := &runtime.MockRuntime{
		ListFunc: func(ctx context.Context, labelFilter map[string]string) ([]api.AgentInfo, error) {
			return []api.AgentInfo{}, nil
		},
		RunFunc: func(ctx context.Context, config runtime.RunConfig) (string, error) {
			runCalled = true
			return "mock-id", nil
		},
	}

	agentDir := filepath.Join(projectScionDir, "agents", "local-test")
	_ = os.MkdirAll(filepath.Join(agentDir, "home"), 0755)
	agentJSON := fmt.Sprintf(`{
		"harness": "generic",
		"explicit_workspace": true,
		"volumes": [{"source": %q, "target": "/workspace"}]
	}`, tmpDir)
	_ = os.WriteFile(filepath.Join(agentDir, "scion-agent.json"), []byte(agentJSON), 0644)

	mgr := NewManager(mockRT)

	_, err := mgr.Start(context.Background(), api.StartOptions{
		Name:        "local-test",
		ProjectPath: projectScionDir,
		NoAuth:      true,
	})
	if err == nil {
		t.Fatal("expected Start to fail for a persisted workspace source equal to $HOME")
	}
	if !strings.Contains(err.Error(), "is not an allowed workspace path") {
		t.Errorf("expected the rejection to come from the deny-set floor, got: %v", err)
	}
	if runCalled {
		t.Error("expected the runtime's Run to never be invoked (fail closed), but it was called")
	}
}

// TestStart_RejectsUnresolvableGitRepoRoot is the regression test for a
// fail-open gap: a project directory can satisfy git's own
// is-inside-work-tree check while having no top-level work tree to resolve
// at all -- a bare repository is the standard example (`git rev-parse
// --is-inside-work-tree` succeeds and prints "false" there, but `git
// rev-parse --show-toplevel` fails). workspaceSourceRoots must refuse this
// case outright rather than returning no roots, which the caller would
// otherwise treat as "no root available, check the rootless floor only" and
// accept any absolute path that isn't '/', $HOME, or under ~/.scion.
func TestStart_RejectsUnresolvableGitRepoRoot(t *testing.T) {
	t.Setenv("SCION_HOST_UID", "")

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

	// A bare repository: git considers a path inside it "inside a work
	// tree" (is-inside-work-tree succeeds, printing "false") without there
	// being any top-level work tree to resolve (show-toplevel fails). This
	// is the real, reproducible way util.IsGitRepoDir can report true while
	// util.RepoRootDir has nothing to return.
	bareRepo := filepath.Join(tmpDir, "bare-project.git")
	initCmd := exec.Command("git", "init", "--bare", bareRepo)
	if out, err := initCmd.CombinedOutput(); err != nil {
		t.Fatalf("git init --bare: %v: %s", err, out)
	}
	projectScionDir := filepath.Join(bareRepo, ".scion")
	_ = os.MkdirAll(projectScionDir, 0755)

	// A persisted, non-explicit workspace pointing somewhere unrelated --
	// with the bug, this would be accepted by the rootless deny-set-only
	// floor since it is neither '/', $HOME, nor under ~/.scion.
	unrelatedWorkspace := filepath.Join(tmpDir, "unrelated-workspace")
	_ = os.MkdirAll(unrelatedWorkspace, 0755)

	agentDir := filepath.Join(projectScionDir, "agents", "bare-repo-agent")
	_ = os.MkdirAll(filepath.Join(agentDir, "home"), 0755)
	agentJSON := fmt.Sprintf(`{"harness": "generic", "volumes": [{"source": %q, "target": "/workspace"}]}`, unrelatedWorkspace)
	_ = os.WriteFile(filepath.Join(agentDir, "scion-agent.json"), []byte(agentJSON), 0644)

	runCalled := false
	mockRT := &runtime.MockRuntime{
		ListFunc: func(ctx context.Context, labelFilter map[string]string) ([]api.AgentInfo, error) {
			return []api.AgentInfo{}, nil
		},
		RunFunc: func(ctx context.Context, config runtime.RunConfig) (string, error) {
			runCalled = true
			return "mock-id", nil
		},
	}

	mgr := NewManager(mockRT)
	_, err := mgr.Start(context.Background(), api.StartOptions{
		Name:        "bare-repo-agent",
		ProjectPath: projectScionDir,
		NoAuth:      true,
	})
	if err == nil {
		t.Fatal("expected Start to refuse a workspace source when the project's own repo root cannot be resolved")
	}
	if !strings.Contains(err.Error(), "does not resolve to its own git work tree") {
		t.Errorf("expected the rejection to name the unresolvable repo root, got: %v", err)
	}
	if !strings.Contains(err.Error(), projectScionDir) {
		t.Errorf("expected the rejection to include the project directory path, got: %v", err)
	}
	if runCalled {
		t.Error("expected the runtime's Run to never be invoked (fail closed), but it was called")
	}
}

// TestStart_RejectsNestedBareRepoResolvingToEnclosingRepo is the regression
// test for a residual variant of the unresolvable-repo-root gap: a bare
// repository nested inside another repository's work tree does not error at
// all when util.RepoRootDir resolves it. RepoRootDir retries from the parent
// directory on a --show-toplevel failure, so the walk-up moves past the bare
// repo's own directory entirely and returns the ENCLOSING repository's top
// level -- a real, successfully-resolved root, just not projectDir's own.
// Without workspaceSharesProjectRepo's common-git-dir cross-check, a
// persisted workspace anywhere in the enclosing repo would be accepted.
// SharedWorkspace: true makes Start validate the persisted outer-repo
// source itself rather than a self-healed managed worktree, matching the
// reported shape.
func TestStart_RejectsNestedBareRepoResolvingToEnclosingRepo(t *testing.T) {
	t.Setenv("SCION_HOST_UID", "")

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

	// The enclosing repository: a normal, non-bare work tree.
	outerRepo := filepath.Join(tmpDir, "outer")
	setupTestGitRepoWithBranch(t, outerRepo, "unused-branch")

	// A bare repository nested inside the outer repo's work tree, with
	// .scion inside IT -- the exact shape that makes util.RepoRootDir's
	// parent-retry walk past the bare repo and land on the outer repo.
	nestedBare := filepath.Join(outerRepo, "vendor", "some-bare.git")
	initCmd := exec.Command("git", "init", "--bare", nestedBare)
	if out, err := initCmd.CombinedOutput(); err != nil {
		t.Fatalf("git init --bare: %v: %s", err, out)
	}
	projectScionDir := filepath.Join(nestedBare, ".scion")
	_ = os.MkdirAll(projectScionDir, 0755)

	// A persisted workspace elsewhere in the OUTER repo -- with the bug,
	// this is accepted because RepoRootDir silently resolves to the outer
	// repo's root and containment is checked against that.
	outerWorkspace := filepath.Join(outerRepo, "some-other-dir")
	_ = os.MkdirAll(outerWorkspace, 0755)

	agentDir := filepath.Join(projectScionDir, "agents", "nested-bare-agent")
	_ = os.MkdirAll(filepath.Join(agentDir, "home"), 0755)
	agentJSON := fmt.Sprintf(`{"harness": "generic", "volumes": [{"source": %q, "target": "/workspace"}]}`, outerWorkspace)
	_ = os.WriteFile(filepath.Join(agentDir, "scion-agent.json"), []byte(agentJSON), 0644)

	runCalled := false
	mockRT := &runtime.MockRuntime{
		ListFunc: func(ctx context.Context, labelFilter map[string]string) ([]api.AgentInfo, error) {
			return []api.AgentInfo{}, nil
		},
		RunFunc: func(ctx context.Context, config runtime.RunConfig) (string, error) {
			runCalled = true
			return "mock-id", nil
		},
	}

	mgr := NewManager(mockRT)
	_, err := mgr.Start(context.Background(), api.StartOptions{
		Name:            "nested-bare-agent",
		ProjectPath:     projectScionDir,
		NoAuth:          true,
		SharedWorkspace: true,
	})
	if err == nil {
		t.Fatal("expected Start to refuse a workspace source when the resolved repo root belongs to a different (enclosing) repository")
	}
	if !strings.Contains(err.Error(), "does not resolve to its own git work tree") {
		t.Errorf("expected the rejection to say the project does not resolve to its own git work tree, got: %v", err)
	}
	if !strings.Contains(err.Error(), projectScionDir) {
		t.Errorf("expected the rejection to include the project directory path, got: %v", err)
	}
	if runCalled {
		t.Error("expected the runtime's Run to never be invoked (fail closed), but it was called")
	}
}

// setupTestGitRepoWithBranch creates a real git repository at dir with an
// initial commit and a branch named branchName pointing at that commit, for
// tests that need util.BranchExists / util.FindWorktreeByBranch to see a
// real repository, not a mocked one.
func setupTestGitRepoWithBranch(t *testing.T, dir, branchName string) {
	t.Helper()
	run := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatal(err)
	}
	run("init")
	run("config", "user.email", "you@example.com")
	run("config", "user.name", "Your Name")
	run("commit", "--allow-empty", "-m", "root commit")
	run("branch", branchName)
}

// TestStart_AcceptsExistingWorktreeOutsideRepoRoot is a regression test: a
// git project attaching to an existing worktree that
// lives outside the repo root (provision.go's FindWorktreeByBranch path,
// e.g. from a worktree created directly with `git worktree add
// ../repo-feature`, or the legacy `.scion_worktrees` layout) must still be
// accepted at Start() -- it worked before the workspace source validator
// existed, via buildCommonRunArgs's "workspace outside repo root" fallback,
// and must keep working now that Start() validates ahead of that.
func TestStart_AcceptsExistingWorktreeOutsideRepoRoot(t *testing.T) {
	// This test creates a real git worktree and needs isGit detection in
	// ProvisionAgent to actually run: it's forced off when SCION_HOST_UID is
	// set (the "running inside an agent container" guard), which this test
	// binary's own environment may set regardless of the test.
	t.Setenv("SCION_HOST_UID", "")

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

	agentName := "worktree-agent"
	targetBranch := api.Slugify(agentName)

	projectDir := filepath.Join(tmpDir, "project")
	setupTestGitRepoWithBranch(t, projectDir, targetBranch)
	projectScionDir := filepath.Join(projectDir, ".scion")
	_ = os.MkdirAll(projectScionDir, 0755)
	_ = os.WriteFile(filepath.Join(projectDir, ".gitignore"), []byte("agents/\n"), 0644)

	// Attach an existing worktree for targetBranch OUTSIDE the repo root, as
	// a plain `git worktree add` sibling would.
	existingWorktree := filepath.Join(tmpDir, "project-worktree-sibling")
	addCmd := exec.Command("git", "-C", projectDir, "worktree", "add", existingWorktree, targetBranch)
	if out, err := addCmd.CombinedOutput(); err != nil {
		t.Fatalf("git worktree add: %v: %s", err, out)
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

	// provision.go's git-branch/worktree lookups (util.BranchExists,
	// util.FindWorktreeByBranch) run plain `git` commands with no explicit
	// -C/dir argument, so they operate against the process's cwd.
	if err := os.Chdir(projectDir); err != nil {
		t.Fatal(err)
	}

	_, err := mgr.Start(context.Background(), api.StartOptions{
		Name:        agentName,
		ProjectPath: projectScionDir,
		NoAuth:      true,
	})
	if err != nil {
		t.Fatalf("expected Start to accept an existing worktree outside the repo root, got error: %v", err)
	}

	wantWorkspace, evalErr := filepath.EvalSymlinks(existingWorktree)
	if evalErr != nil {
		t.Fatalf("EvalSymlinks(%q): %v", existingWorktree, evalErr)
	}
	if capturedConfig.Workspace != wantWorkspace {
		t.Errorf("RunConfig.Workspace = %q, want %q", capturedConfig.Workspace, wantWorkspace)
	}
}

// TestStart_RejectsNonWorktreeSourceOutsideRepoRoot is the negative sibling
// of TestStart_AcceptsExistingWorktreeOutsideRepoRoot: a source outside the
// repo root that is NOT a registered worktree of that repository (git
// verification correctly returns false) must still be rejected. This is
// also the missing "Start() non-explicit source outside root" containment
// reject case.
//
// StartOptions.SharedWorkspace is set to true below, which keeps
// GetAgent's agentWorkspace empty throughout -- without it, GetAgent's
// resume path self-heals a missing <agentDir>/workspace by recreating a
// managed worktree there (the project directory here is a real git repo,
// and the target branch already exists), regardless of the persisted
// volume's own source, which would silently replace the very path this
// test needs to stay in place: an environment where that recreation isn't
// itself blocked (SCION_HOST_UID unset) would make Start() succeed with
// the freshly recreated worktree, never exercising the containment check
// this test exists to prove. See TestStart_RejectsStaleRecreatedWorktree
// for the same pattern.
func TestStart_RejectsNonWorktreeSourceOutsideRepoRoot(t *testing.T) {
	t.Setenv("SCION_HOST_UID", "")

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

	agentName := "non-worktree-agent"
	projectDir := filepath.Join(tmpDir, "project")
	setupTestGitRepoWithBranch(t, projectDir, api.Slugify(agentName))
	projectScionDir := filepath.Join(projectDir, ".scion")
	_ = os.MkdirAll(projectScionDir, 0755)
	_ = os.WriteFile(filepath.Join(projectDir, ".gitignore"), []byte("agents/\n"), 0644)

	// A directory that merely sits outside the repo -- never registered
	// with git as a worktree of it -- passed as a persisted, non-explicit
	// workspace source (the shape a bad or stale value would take).
	notAWorktree := filepath.Join(tmpDir, "not-a-worktree")
	_ = os.MkdirAll(notAWorktree, 0755)

	runCalled := false
	mockRT := &runtime.MockRuntime{
		ListFunc: func(ctx context.Context, labelFilter map[string]string) ([]api.AgentInfo, error) {
			return []api.AgentInfo{}, nil
		},
		RunFunc: func(ctx context.Context, config runtime.RunConfig) (string, error) {
			runCalled = true
			return "mock-id", nil
		},
	}

	agentDir := filepath.Join(projectScionDir, "agents", agentName)
	_ = os.MkdirAll(filepath.Join(agentDir, "home"), 0755)
	agentJSON := fmt.Sprintf(`{"harness": "generic", "volumes": [{"source": %q, "target": "/workspace"}]}`, notAWorktree)
	_ = os.WriteFile(filepath.Join(agentDir, "scion-agent.json"), []byte(agentJSON), 0644)

	mgr := NewManager(mockRT)
	_, err := mgr.Start(context.Background(), api.StartOptions{
		Name:            agentName,
		ProjectPath:     projectScionDir,
		NoAuth:          true,
		SharedWorkspace: true,
	})
	if err == nil {
		t.Fatal("expected Start to reject a non-worktree source outside the repo root")
	}
	if !strings.Contains(err.Error(), "is outside the permitted workspace root") {
		t.Errorf("expected the rejection to come from containment, got: %v", err)
	}
	if runCalled {
		t.Error("expected the runtime's Run to never be invoked (fail closed), but it was called")
	}
}

// TestStart_RejectsAnotherRepositoryWorktree is the caller-context
// regression test for a worktree-membership bug: containment must be
// verified against THIS project's own repo, derived from projectDir, never
// against whatever repo the source itself happens to belong to. A worktree
// (main or linked) of a completely different repository must be refused at
// Start(), even though util.IsRegisteredWorktree would report it as a real,
// non-prunable registration -- just of the wrong repo.
//
// It reproduces the actual mechanism behind that bug: provision.go's
// util.BranchExists / util.FindWorktreeByBranch run bare `git` commands with
// no -C/dir argument, so they resolve against the process's current
// directory, not necessarily the project actually being provisioned. If the
// process happens to be sitting inside a DIFFERENT repository that has a
// branch with the same name, Case 2's "attach to existing worktree" logic
// discovers and attaches to that other repository's worktree, not this
// project's. Root/membership containment must still catch that: it must be
// verified against the project's OWN repo (derived from projectDir), never
// against whatever repo the discovered source happens to belong to.
func TestStart_RejectsAnotherRepositoryWorktree(t *testing.T) {
	t.Setenv("SCION_HOST_UID", "")

	tmpDir := t.TempDir()

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

	agentName := "cross-repo-agent"
	branch := api.Slugify(agentName)

	// The agent's own project -- has the branch, but no worktree for it.
	projectDir := filepath.Join(tmpDir, "project")
	setupTestGitRepoWithBranch(t, projectDir, branch)
	projectScionDir := filepath.Join(projectDir, ".scion")
	_ = os.MkdirAll(projectScionDir, 0755)
	_ = os.WriteFile(filepath.Join(projectDir, ".gitignore"), []byte("agents/\n"), 0644)

	// Two completely different, unrelated repositories that happen to use
	// the SAME branch name -- one with that branch checked out in its main
	// worktree, the other with it checked out in a linked worktree instead
	// (git refuses to check the same branch out twice in one repo, so this
	// needs two separate repos to cover both shapes).
	otherRepoMain := filepath.Join(tmpDir, "other-repo-main")
	setupTestGitRepoWithBranch(t, otherRepoMain, branch)
	checkoutCmd := exec.Command("git", "-C", otherRepoMain, "checkout", branch)
	if out, err := checkoutCmd.CombinedOutput(); err != nil {
		t.Fatalf("git checkout: %v: %s", err, out)
	}

	otherRepoLinked := filepath.Join(tmpDir, "other-repo-linked")
	setupTestGitRepoWithBranch(t, otherRepoLinked, branch)
	otherRepoWorktree := filepath.Join(tmpDir, "other-repo-linked-wt")
	addCmd := exec.Command("git", "-C", otherRepoLinked, "worktree", "add", otherRepoWorktree, branch)
	if out, err := addCmd.CombinedOutput(); err != nil {
		t.Fatalf("git worktree add: %v: %s", err, out)
	}

	oldWd, _ := os.Getwd()
	defer func() { _ = os.Chdir(oldWd) }()

	for _, tt := range []struct {
		name string
		// cwd is where the process is sitting when Start()'s bare,
		// CWD-dependent git discovery runs -- inside the OTHER repository,
		// not the project being provisioned.
		cwd string
	}{
		{name: "another repo's main worktree", cwd: otherRepoMain},
		{name: "another repo's linked worktree", cwd: otherRepoLinked},
	} {
		t.Run(tt.name, func(t *testing.T) {
			runCalled := false
			mockRT := &runtime.MockRuntime{
				ListFunc: func(ctx context.Context, labelFilter map[string]string) ([]api.AgentInfo, error) {
					return []api.AgentInfo{}, nil
				},
				RunFunc: func(ctx context.Context, config runtime.RunConfig) (string, error) {
					runCalled = true
					return "mock-id", nil
				},
			}

			// Same agentName (and so the same target branch) in every
			// subtest -- provision.go slugifies opts.Name to compute the
			// branch it looks for, so this must match the branch set up in
			// each "other repo" above for the discovery bug to trigger at
			// all. Only agentDir is reset between subtests.
			agentDir := filepath.Join(projectScionDir, "agents", agentName)
			_ = os.RemoveAll(agentDir)

			if err := os.Chdir(tt.cwd); err != nil {
				t.Fatal(err)
			}

			mgr := NewManager(mockRT)
			_, err := mgr.Start(context.Background(), api.StartOptions{
				Name:        agentName,
				ProjectPath: projectScionDir,
				NoAuth:      true,
			})
			if err == nil {
				t.Fatal("expected Start to reject a worktree belonging to a different repository")
			}
			if !strings.Contains(err.Error(), "is outside the permitted workspace root") {
				t.Errorf("expected the rejection to come from containment, got: %v", err)
			}
			if runCalled {
				t.Error("expected the runtime's Run to never be invoked (fail closed), but it was called")
			}
		})
	}
}

// TestStart_RejectsRegisteredWorktreeResolvingToHome is the Start()-level
// complement to TestValidateWorkspaceSource_UnusableSuppliedRootIsRejected
// (the precise unit test, in workspace_source_guard_test.go, for the rule
// that worktree/root verification decides whether repo-root containment
// applies, never whether the universal '/' and $HOME deny-set floor
// applies -- a supplied or verified root of $HOME is refused outright, not
// silently treated as "no root"). This test exercises the real git-aware
// code path in Start(): a git project whose effectiveWorkspace resolves to
// $HOME must still be rejected, regardless of whether git worktree
// verification runs against it.
func TestStart_RejectsRegisteredWorktreeResolvingToHome(t *testing.T) {
	t.Setenv("SCION_HOST_UID", "")

	tmpDir := t.TempDir()

	oldWd, _ := os.Getwd()
	_ = os.Chdir(tmpDir)
	defer func() { _ = os.Chdir(oldWd) }()

	agentName := "home-worktree-agent"
	projectDir := filepath.Join(tmpDir, "project")
	setupTestGitRepoWithBranch(t, projectDir, api.Slugify(agentName))

	// A REAL worktree, registered with the project's own repo, whose path
	// becomes $HOME -- not just a directory that happens to equal $HOME.
	// Even a git-verified worktree of the project's own repo must still be
	// refused if it resolves to $HOME, because the deny-set floor runs
	// unconditionally before worktree membership is ever considered.
	homeWorktree := filepath.Join(tmpDir, "home-wt")
	addCmd := exec.Command("git", "-C", projectDir, "worktree", "add", homeWorktree, api.Slugify(agentName))
	if out, err := addCmd.CombinedOutput(); err != nil {
		t.Fatalf("git worktree add: %v: %s", err, out)
	}

	originalHome := os.Getenv("HOME")
	defer func() { _ = os.Setenv("HOME", originalHome) }()
	_ = os.Setenv("HOME", homeWorktree)

	globalScionDir := filepath.Join(homeWorktree, ".scion")
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

	projectScionDir := filepath.Join(projectDir, ".scion")
	_ = os.MkdirAll(projectScionDir, 0755)
	_ = os.WriteFile(filepath.Join(projectDir, ".gitignore"), []byte("agents/\n"), 0644)

	// A persisted config whose workspace volume is $HOME itself (the real
	// registered worktree from above). Membership deciding whether repo-root
	// containment applies must never override the unconditional $HOME
	// refusal that runs first.
	runCalled := false
	mockRT := &runtime.MockRuntime{
		ListFunc: func(ctx context.Context, labelFilter map[string]string) ([]api.AgentInfo, error) {
			return []api.AgentInfo{}, nil
		},
		RunFunc: func(ctx context.Context, config runtime.RunConfig) (string, error) {
			runCalled = true
			return "mock-id", nil
		},
	}

	agentDir := filepath.Join(projectScionDir, "agents", agentName)
	_ = os.MkdirAll(filepath.Join(agentDir, "home"), 0755)
	agentJSON := fmt.Sprintf(`{"harness": "generic", "volumes": [{"source": %q, "target": "/workspace"}]}`, homeWorktree)
	_ = os.WriteFile(filepath.Join(agentDir, "scion-agent.json"), []byte(agentJSON), 0644)

	mgr := NewManager(mockRT)
	_, err := mgr.Start(context.Background(), api.StartOptions{
		Name:        agentName,
		ProjectPath: projectScionDir,
		NoAuth:      true,
	})
	if err == nil {
		t.Fatal("expected Start to reject a registered worktree of the project's own repo that resolves to $HOME")
	}
	if !strings.Contains(err.Error(), "is not an allowed workspace path") {
		t.Errorf("expected the rejection to come from the deny-set floor, got: %v", err)
	}
	if runCalled {
		t.Error("expected the runtime's Run to never be invoked (fail closed), but it was called")
	}
}

// TestStart_RejectsStaleRecreatedWorktree is the Start()-level counterpart
// to the unit-level TestIsRegisteredWorktree_PrunableRecreatedPathRejected:
// it exercises the same prunable-worktree-recreated-as-a-plain-directory
// shape through Start() itself.
// StartOptions.SharedWorkspace is set to true below, which keeps
// GetAgent's agentWorkspace empty throughout -- without it, GetAgent's
// resume path self-heals a missing <agentDir>/workspace by recreating a
// managed worktree there regardless of the persisted volume's own source,
// which would silently replace the very path this test needs to stay stale.
func TestStart_RejectsStaleRecreatedWorktree(t *testing.T) {
	t.Setenv("SCION_HOST_UID", "")

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

	agentName := "stale-worktree-agent"
	projectDir := filepath.Join(tmpDir, "project")
	setupTestGitRepoWithBranch(t, projectDir, api.Slugify(agentName))
	projectScionDir := filepath.Join(projectDir, ".scion")
	_ = os.MkdirAll(projectScionDir, 0755)
	_ = os.WriteFile(filepath.Join(projectDir, ".gitignore"), []byte("agents/\n"), 0644)

	// A real worktree, registered with the project's own repo...
	staleWorktree := filepath.Join(tmpDir, "stale-wt")
	addCmd := exec.Command("git", "-C", projectDir, "worktree", "add", staleWorktree, api.Slugify(agentName))
	if out, err := addCmd.CombinedOutput(); err != nil {
		t.Fatalf("git worktree add: %v: %s", err, out)
	}
	// ...then removed directly (not via `git worktree remove`), leaving
	// git's own registration in place but pointing at a gitdir that no
	// longer resolves -- exactly what makes `git worktree list --porcelain`
	// report the entry as prunable...
	if err := os.RemoveAll(staleWorktree); err != nil {
		t.Fatal(err)
	}
	// ...and recreated as a plain directory at the same path: nothing
	// git-related, just a directory that happens to have the same name.
	if err := os.MkdirAll(staleWorktree, 0755); err != nil {
		t.Fatal(err)
	}

	runCalled := false
	mockRT := &runtime.MockRuntime{
		ListFunc: func(ctx context.Context, labelFilter map[string]string) ([]api.AgentInfo, error) {
			return []api.AgentInfo{}, nil
		},
		RunFunc: func(ctx context.Context, config runtime.RunConfig) (string, error) {
			runCalled = true
			return "mock-id", nil
		},
	}

	agentDir := filepath.Join(projectScionDir, "agents", agentName)
	_ = os.MkdirAll(filepath.Join(agentDir, "home"), 0755)
	agentJSON := fmt.Sprintf(`{"harness": "generic", "volumes": [{"source": %q, "target": "/workspace"}]}`, staleWorktree)
	_ = os.WriteFile(filepath.Join(agentDir, "scion-agent.json"), []byte(agentJSON), 0644)

	mgr := NewManager(mockRT)
	// SharedWorkspace: true keeps GetAgent's agentWorkspace empty throughout,
	// which skips the managed-worktree self-heal (recreating a fresh worktree
	// at <agentDir>/workspace when it's missing). Without this, self-heal
	// would prune the stale registration and recreate a valid worktree
	// there, masking the exact persisted-volume shape this test needs to
	// reach the containment check with.
	_, err := mgr.Start(context.Background(), api.StartOptions{
		Name:            agentName,
		ProjectPath:     projectScionDir,
		NoAuth:          true,
		SharedWorkspace: true,
	})
	if err == nil {
		t.Fatal("expected Start to reject a stale worktree registration recreated as a plain directory")
	}
	if !strings.Contains(err.Error(), "is outside the permitted workspace root") {
		t.Errorf("expected the rejection to come from containment (no longer a verified worktree, so only repo-root containment applies), got: %v", err)
	}
	if runCalled {
		t.Error("expected the runtime's Run to never be invoked (fail closed), but it was called")
	}
}

// TestStart_AcceptsMainWorktreeWhenProjectLivesInLinkedWorktree covers a
// project whose own .scion directory lives in a linked worktree rather than
// the repository's main one: the containment root workspaceSourceRoots
// derives from projectDir is that linked worktree's own path, not the main
// worktree's. A persisted source that is the repository's MAIN worktree
// must still be accepted -- it is the same repository, just reached from a
// project sitting in a different (linked) checkout of it -- which requires
// recognizing the main worktree structurally rather than by repoRoot
// equality (see util.IsRegisteredWorktree's doc comment).
func TestStart_AcceptsMainWorktreeWhenProjectLivesInLinkedWorktree(t *testing.T) {
	t.Setenv("SCION_HOST_UID", "")

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

	agentName := "linked-project-agent"

	// The repository's main worktree.
	mainWorktree := filepath.Join(tmpDir, "main-repo")
	setupTestGitRepoWithBranch(t, mainWorktree, "linked-branch")

	// A linked worktree of the SAME repository -- this is where the
	// project's own .scion directory lives, not the main worktree.
	linkedWorktree := filepath.Join(tmpDir, "linked-repo")
	addCmd := exec.Command("git", "-C", mainWorktree, "worktree", "add", linkedWorktree, "linked-branch")
	if out, err := addCmd.CombinedOutput(); err != nil {
		t.Fatalf("git worktree add: %v: %s", err, out)
	}

	projectScionDir := filepath.Join(linkedWorktree, ".scion")
	_ = os.MkdirAll(projectScionDir, 0755)
	_ = os.WriteFile(filepath.Join(linkedWorktree, ".gitignore"), []byte("agents/\n"), 0644)

	// The persisted source is the repository's MAIN worktree -- a
	// legitimate location in the same repository as the project, reached
	// from a project living in a different (linked) checkout of it.
	runCalled := false
	var capturedConfig runtime.RunConfig
	mockRT := &runtime.MockRuntime{
		ListFunc: func(ctx context.Context, labelFilter map[string]string) ([]api.AgentInfo, error) {
			return []api.AgentInfo{}, nil
		},
		RunFunc: func(ctx context.Context, config runtime.RunConfig) (string, error) {
			runCalled = true
			capturedConfig = config
			return "mock-id", nil
		},
	}

	agentDir := filepath.Join(projectScionDir, "agents", agentName)
	_ = os.MkdirAll(filepath.Join(agentDir, "home"), 0755)
	agentJSON := fmt.Sprintf(`{"harness": "generic", "volumes": [{"source": %q, "target": "/workspace"}]}`, mainWorktree)
	_ = os.WriteFile(filepath.Join(agentDir, "scion-agent.json"), []byte(agentJSON), 0644)

	mgr := NewManager(mockRT)
	_, err := mgr.Start(context.Background(), api.StartOptions{
		Name:            agentName,
		ProjectPath:     projectScionDir,
		NoAuth:          true,
		SharedWorkspace: true,
	})
	if err != nil {
		t.Fatalf("expected Start to accept the repository's main worktree as a source for a project living in a linked worktree, got error: %v", err)
	}
	if !runCalled {
		t.Fatal("expected the runtime's Run to be invoked")
	}
	wantWorkspace, evalErr := filepath.EvalSymlinks(mainWorktree)
	if evalErr != nil {
		t.Fatalf("EvalSymlinks(%q): %v", mainWorktree, evalErr)
	}
	if capturedConfig.Workspace != wantWorkspace {
		t.Errorf("RunConfig.Workspace = %q, want %q", capturedConfig.Workspace, wantWorkspace)
	}
}

// TestStart_FiltersUnsafeWorkspaceVolumeInWorktreeCase is the regression test
// for the gap in the worktree case (agentWorkspace set, not volume-derived):
// buildCommonRunArgs mounts a worktree-subdirectory workspace at
// /repo-root/<rel>, not /workspace, leaving the /workspace target slot free
// for a raw, unvalidated volume from a persisted config to claim. A /workspace
// -target volume must be filtered out of the generic volume list whenever
// there is an effective workspace at all, not only when effectiveWorkspace
// happens to differ from agentWorkspace by value (a comparison that silently
// flips depending on symlink canonicalization -- see the comment at the
// Volumes builder in Start()).
func TestStart_FiltersUnsafeWorkspaceVolumeInWorktreeCase(t *testing.T) {
	t.Setenv("SCION_HOST_UID", "")

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

	agentName := "fresh-worktree-agent"
	projectDir := filepath.Join(tmpDir, "project")
	run := func(args ...string) {
		cmd := exec.Command("git", args...)
		cmd.Dir = projectDir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
	_ = os.MkdirAll(projectDir, 0755)
	run("init")
	run("config", "user.email", "you@example.com")
	run("config", "user.name", "Your Name")
	run("commit", "--allow-empty", "-m", "root commit")
	// Deliberately do NOT pre-create the agent's target branch, so
	// ProvisionAgent creates a fresh managed worktree (agentWorkspace set)
	// rather than attaching to an existing one.
	projectScionDir := filepath.Join(projectDir, ".scion")
	_ = os.MkdirAll(projectScionDir, 0755)
	_ = os.WriteFile(filepath.Join(projectDir, ".gitignore"), []byte("agents/\n"), 0644)

	mockRT := &runtime.MockRuntime{
		ListFunc: func(ctx context.Context, labelFilter map[string]string) ([]api.AgentInfo, error) {
			return []api.AgentInfo{}, nil
		},
		RunFunc: func(ctx context.Context, config runtime.RunConfig) (string, error) {
			return "mock-id", nil
		},
	}
	mgr := NewManager(mockRT)

	if err := os.Chdir(projectDir); err != nil {
		t.Fatal(err)
	}
	if _, err := mgr.Start(context.Background(), api.StartOptions{
		Name:        agentName,
		ProjectPath: projectScionDir,
		NoAuth:      true,
	}); err != nil {
		t.Fatalf("initial Start failed: %v", err)
	}

	agentDir := filepath.Join(projectScionDir, "agents", agentName)
	agentWorkspace := filepath.Join(agentDir, "workspace")
	if _, err := os.Stat(agentWorkspace); err != nil {
		t.Fatalf("expected a managed worktree at %s: %v", agentWorkspace, err)
	}

	// Simulate a template or a buggy persisted config with an extra
	// /workspace-target volume alongside the legitimate worktree-derived
	// workspace.
	agentConfigPath := filepath.Join(agentDir, "scion-agent.json")
	raw, err := os.ReadFile(agentConfigPath)
	if err != nil {
		t.Fatal(err)
	}
	var cfg map[string]interface{}
	if err := json.Unmarshal(raw, &cfg); err != nil {
		t.Fatal(err)
	}
	cfg["volumes"] = []map[string]interface{}{
		{"source": "/", "target": "/workspace"},
	}
	rewritten, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(agentConfigPath, rewritten, 0644); err != nil {
		t.Fatal(err)
	}

	var capturedConfig runtime.RunConfig
	mockRT.RunFunc = func(ctx context.Context, config runtime.RunConfig) (string, error) {
		capturedConfig = config
		return "mock-id-2", nil
	}

	info, err := mgr.Start(context.Background(), api.StartOptions{
		Name:        agentName,
		ProjectPath: projectScionDir,
		NoAuth:      true,
	})
	if err != nil {
		t.Fatalf("resume Start failed: %v", err)
	}

	for _, v := range capturedConfig.Volumes {
		if v.Target == "/workspace" {
			t.Errorf("expected the extra /workspace-target volume to be filtered out, found: %+v", v)
		}
	}

	foundWarning := false
	for _, w := range info.Warnings {
		if strings.Contains(w, "/workspace") {
			foundWarning = true
		}
	}
	if !foundWarning {
		t.Errorf("expected a warning about the dropped /workspace volume, got warnings: %v", info.Warnings)
	}

	wantWorkspace, evalErr := filepath.EvalSymlinks(agentWorkspace)
	if evalErr != nil {
		t.Fatalf("EvalSymlinks(%q): %v", agentWorkspace, evalErr)
	}
	if capturedConfig.Workspace != wantWorkspace {
		t.Errorf("RunConfig.Workspace = %q, want the legitimate managed worktree %q", capturedConfig.Workspace, wantWorkspace)
	}
}

// TestStart_RejectsPreExistingGlobalAgentWorkspaceOutsideProjectDir covers
// the intended edge case in the global project's containment root: a global
// agent provisioned before <projectDir>/workspace existed (or otherwise
// persisted with a workspace outside it) is refused on resume, now that the
// global project's root is <projectDir>/workspace itself. This is a
// deliberate behavior change, not a bug -- the error must name both ways to
// recover (recreate the agent, or restart with an explicit --workspace),
// since neither is obvious from the base rejection alone, whichever of
// ValidateWorkspaceSource's two base messages ("outside the permitted
// workspace root" for plain containment, "not an allowed workspace path" for
// the ~/.scion floor) a given case happens to surface.
func TestStart_RejectsPreExistingGlobalAgentWorkspaceOutsideProjectDir(t *testing.T) {
	for _, tt := range []struct {
		name string
		// workspace, relative to tmpDir (== $HOME for this test), is the
		// persisted volume source to simulate. "$HOME/wherever-cli-was-run-from"
		// is outside ~/.scion entirely, so it would be refused under either
		// root and does not by itself prove which root is actually being
		// enforced. "~/.scion/templates" is the discriminating case: a
		// ~/.scion root would accept it; the ~/.scion/workspace root
		// refuses it, so only this case tells the two apart.
		workspace string
		// wantBaseError is the base-rejection substring each case surfaces.
		// The two cases fail closed for different reasons: the first is
		// outside ~/.scion entirely, so containment against the global
		// project's own root (~/.scion/workspace) is what refuses it; the
		// second is under ~/.scion but not in the named allow list, so the
		// unconditional ~/.scion floor (ValidateWorkspaceSource) refuses it
		// before containment is even checked.
		wantBaseError string
	}{
		{name: "workspace outside $HOME/.scion entirely", workspace: "wherever-cli-was-run-from", wantBaseError: "is outside the permitted workspace root"},
		{name: "workspace under $HOME/.scion but not .scion/workspace", workspace: filepath.Join(".scion", "templates"), wantBaseError: "is not an allowed workspace path"},
	} {
		t.Run(tt.name, func(t *testing.T) {
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

			// Simulate a workspace persisted by a global agent from before the
			// current containment root (<projectDir>/workspace) took effect.
			persistedWorkspace := filepath.Join(tmpDir, tt.workspace)
			_ = os.MkdirAll(persistedWorkspace, 0755)

			agentName := "pre-existing-global-agent"
			agentDir := filepath.Join(globalScionDir, "agents", agentName)
			_ = os.MkdirAll(filepath.Join(agentDir, "home"), 0755)
			agentJSON := fmt.Sprintf(`{"harness": "generic", "volumes": [{"source": %q, "target": "/workspace"}]}`, persistedWorkspace)
			_ = os.WriteFile(filepath.Join(agentDir, "scion-agent.json"), []byte(agentJSON), 0644)

			runCalled := false
			mockRT := &runtime.MockRuntime{
				ListFunc: func(ctx context.Context, labelFilter map[string]string) ([]api.AgentInfo, error) {
					return []api.AgentInfo{}, nil
				},
				RunFunc: func(ctx context.Context, config runtime.RunConfig) (string, error) {
					runCalled = true
					return "mock-id", nil
				},
			}

			mgr := NewManager(mockRT)
			_, err := mgr.Start(context.Background(), api.StartOptions{
				Name:        agentName,
				ProjectPath: globalScionDir,
				NoAuth:      true,
			})
			if err == nil {
				t.Fatal("expected Start to reject a pre-existing global agent's workspace outside <projectDir>/workspace")
			}
			if !strings.Contains(err.Error(), tt.wantBaseError) {
				t.Errorf("expected the base rejection to contain %q, got: %v", tt.wantBaseError, err)
			}
			if !strings.Contains(err.Error(), "recreate") || !strings.Contains(err.Error(), "--workspace") {
				t.Errorf("expected an actionable error naming both recovery paths (recreate the agent, or an explicit --workspace), got: %v", err)
			}
			if runCalled {
				t.Error("expected the runtime's Run to never be invoked (fail closed), but it was called")
			}
		})
	}
}

// TestStart_RejectsGlobalProjectInsideGitWorkTree covers the global project
// whose own directory (~/.scion) is itself a git work tree -- a dotfiles
// repository, for example. isGitWorkspaceProject then routes it through
// workspaceSourceRoots' git branch, so ProvisionAgent creates a per-agent
// worktree/workspace at ~/.scion/agents/<name>/workspace, the ordinary git-
// project shape, not <projectDir>/workspace. That source can never pass the
// ~/.scion floor (only ~/.scion/workspace and ~/.scion/projects/<slug> are
// admitted), and root == ~/.scion itself is refused outright as a
// misconfigured root, so the agent -- freshly created by this same call --
// must fail closed with its own actionable message, not the generic
// "delete and recreate" hint that cannot help a shape this fixed.
func TestStart_RejectsGlobalProjectInsideGitWorkTree(t *testing.T) {
	t.Setenv("SCION_HOST_UID", "")

	tmpDir := t.TempDir()

	oldWd, _ := os.Getwd()
	_ = os.Chdir(tmpDir)
	defer func() { _ = os.Chdir(oldWd) }()

	originalHome := os.Getenv("HOME")
	defer func() { _ = os.Setenv("HOME", originalHome) }()
	_ = os.Setenv("HOME", tmpDir)

	if oldAutoExpose, ok := os.LookupEnv("SCION_AUTO_EXPOSE_PORTS"); ok {
		_ = os.Unsetenv("SCION_AUTO_EXPOSE_PORTS")
		defer func() { _ = os.Setenv("SCION_AUTO_EXPOSE_PORTS", oldAutoExpose) }()
	}

	globalScionDir := filepath.Join(tmpDir, ".scion")
	// ~/.scion is itself a git work tree -- the dotfiles-repository shape.
	// agents/ must be gitignored, the same requirement provision.go
	// enforces for any project-local git project, checked before
	// workspaceSourceRoots's own global-project handling runs.
	if err := os.MkdirAll(globalScionDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(globalScionDir, ".gitignore"), []byte("agents/\n"), 0644); err != nil {
		t.Fatal(err)
	}
	setupTestGitRepoWithBranch(t, globalScionDir, "unused-branch")

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

	runCalled := false
	mockRT := &runtime.MockRuntime{
		ListFunc: func(ctx context.Context, labelFilter map[string]string) ([]api.AgentInfo, error) {
			return []api.AgentInfo{}, nil
		},
		RunFunc: func(ctx context.Context, config runtime.RunConfig) (string, error) {
			runCalled = true
			return "mock-id", nil
		},
	}
	mgr := NewManager(mockRT)

	_, err := mgr.Start(context.Background(), api.StartOptions{
		Name:        "git-tracked-global-agent",
		ProjectPath: globalScionDir,
		NoAuth:      true,
	})
	if err == nil {
		t.Fatal("expected Start to refuse a fresh global agent when ~/.scion is itself a git work tree")
	}
	if !strings.Contains(err.Error(), "is itself inside a git work tree") {
		t.Errorf("expected an error naming the actual shape (global project inside a git work tree), got: %v", err)
	}
	if !strings.Contains(err.Error(), "--workspace") {
		t.Errorf("expected the error to name the actual recovery path (an explicit --workspace), got: %v", err)
	}
	if strings.Contains(err.Error(), "delete and recreate") {
		t.Errorf("expected the generic recreate hint to be absent -- recreating cannot fix this shape, got: %v", err)
	}
	if runCalled {
		t.Error("expected the runtime's Run to never be invoked (fail closed), but it was called")
	}
}

// TestStart_AcceptsOrdinaryGitRepoNamedGlobal covers an ordinary git
// project whose own repository root happens to be named "global" -- not
// the real global project, which is identified by its resolved directory,
// not by name. Start must treat it like any other git project: create a
// worktree and reach Run, rather than refusing it as if it were the global
// project's own directory misconfigured as a git work tree.
func TestStart_AcceptsOrdinaryGitRepoNamedGlobal(t *testing.T) {
	t.Setenv("SCION_HOST_UID", "")

	tmpDir := t.TempDir()

	oldWd, _ := os.Getwd()
	_ = os.Chdir(tmpDir)
	defer func() { _ = os.Chdir(oldWd) }()

	// HOME is a separate directory from the project below, so the real
	// global directory (HOME/.scion) and this project's own repository
	// root are unambiguously different paths.
	originalHome := os.Getenv("HOME")
	defer func() { _ = os.Setenv("HOME", originalHome) }()
	home := filepath.Join(tmpDir, "home")
	_ = os.Setenv("HOME", home)

	globalScionDir := filepath.Join(home, ".scion")
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

	agentName := "ordinary-agent"
	projectDir := filepath.Join(tmpDir, "global")
	setupTestGitRepoWithBranch(t, projectDir, api.Slugify(agentName))
	projectScionDir := filepath.Join(projectDir, ".scion")
	if err := os.MkdirAll(projectScionDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(projectDir, ".gitignore"), []byte("agents/\n"), 0644); err != nil {
		t.Fatal(err)
	}

	if err := os.Chdir(projectDir); err != nil {
		t.Fatal(err)
	}

	runCalled := false
	mockRT := &runtime.MockRuntime{
		ListFunc: func(ctx context.Context, labelFilter map[string]string) ([]api.AgentInfo, error) {
			return []api.AgentInfo{}, nil
		},
		RunFunc: func(ctx context.Context, config runtime.RunConfig) (string, error) {
			runCalled = true
			return "mock-id", nil
		},
	}
	mgr := NewManager(mockRT)

	_, err := mgr.Start(context.Background(), api.StartOptions{
		Name:        agentName,
		ProjectPath: projectScionDir,
		NoAuth:      true,
	})
	if err != nil {
		t.Fatalf("expected Start to accept an ordinary git repo named %q, got error: %v", filepath.Base(projectDir), err)
	}
	if !runCalled {
		t.Error("expected the runtime's Run to be invoked")
	}
}

// TestStart_GlobalProjectResumeUsesConsistentWorkspace covers restart/resume
// for a global-project agent: ProvisionAgent's Case 3 creates a per-agent
// subdirectory under ~/.scion/workspace (not a directory shared by every
// global agent), and a second Start() call for the same agent -- simulating
// a restart/resume, not a fresh provision -- must resolve to that exact
// same directory again, not recreate a new one or fall back to the bare
// ~/.scion/workspace root.
func TestStart_GlobalProjectResumeUsesConsistentWorkspace(t *testing.T) {
	t.Setenv("SCION_HOST_UID", "")

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

	agentName := "resumable-global-agent"
	var capturedWorkspaces []string
	mockRT := &runtime.MockRuntime{
		ListFunc: func(ctx context.Context, labelFilter map[string]string) ([]api.AgentInfo, error) {
			return []api.AgentInfo{}, nil
		},
		RunFunc: func(ctx context.Context, config runtime.RunConfig) (string, error) {
			capturedWorkspaces = append(capturedWorkspaces, config.Workspace)
			return "mock-id", nil
		},
	}
	mgr := NewManager(mockRT)

	for i := 0; i < 2; i++ {
		if _, err := mgr.Start(context.Background(), api.StartOptions{
			Name:        agentName,
			ProjectPath: globalScionDir,
			NoAuth:      true,
		}); err != nil {
			t.Fatalf("Start call %d failed: %v", i+1, err)
		}
	}

	if len(capturedWorkspaces) != 2 {
		t.Fatalf("expected 2 captured RunConfig.Workspace values, got %d: %v", len(capturedWorkspaces), capturedWorkspaces)
	}

	wantWorkspace, err := filepath.EvalSymlinks(filepath.Join(globalScionDir, "workspace", agentName))
	if err != nil {
		t.Fatalf("EvalSymlinks(want): %v", err)
	}
	for i, got := range capturedWorkspaces {
		evalGot, err := filepath.EvalSymlinks(got)
		if err != nil {
			t.Fatalf("EvalSymlinks(call %d): %v", i+1, err)
		}
		if evalGot != wantWorkspace {
			t.Errorf("Start call %d: RunConfig.Workspace = %q, want %q", i+1, evalGot, wantWorkspace)
		}
	}
	if capturedWorkspaces[0] != capturedWorkspaces[1] {
		t.Errorf("expected the first and second Start calls to resolve to the identical workspace value, got %q then %q", capturedWorkspaces[0], capturedWorkspaces[1])
	}
}

// setupHubMarkerProjectConfigsDir creates the externalized .scion directory
// a hub-dispatched project's marker file resolves to
// (config.ResolveProjectMarker's output: ~/.scion/project-configs/<dir>/.scion,
// pkg/config/project_marker.go's ExternalProjectPath), along with the
// harness-config, template and settings.yaml a fresh provision needs. It
// returns the resolved .scion directory to pass as StartOptions.ProjectPath.
// The marker file itself (~/.scion/projects/<slug>/.scion) is not created:
// Start() never reads it -- resolution happens upstream, in whichever
// dispatcher set ProjectPath -- so passing the already-resolved directory
// directly reproduces the shape Start() actually sees.
func setupHubMarkerProjectConfigsDir(t *testing.T, tmpHome, dirName string) string {
	t.Helper()
	projectConfigsDir := filepath.Join(tmpHome, ".scion", "project-configs", dirName)
	projectScionDir := filepath.Join(projectConfigsDir, ".scion")

	hcDir := filepath.Join(projectScionDir, "harness-configs", "test-harness")
	if err := os.MkdirAll(hcDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(hcDir, "config.yaml"), []byte("harness: gemini\nuser: scion\nimage: test-image:latest\n"), 0644); err != nil {
		t.Fatal(err)
	}
	tplDir := filepath.Join(projectScionDir, "templates", "default")
	if err := os.MkdirAll(tplDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(tplDir, "scion-agent.json"), []byte(`{"default_harness_config": "test-harness"}`), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(projectScionDir, "settings.yaml"), []byte("schema_version: \"1\"\nactive_profile: local\nprofiles:\n  local:\n    runtime: docker\n"), 0644); err != nil {
		t.Fatal(err)
	}
	return projectScionDir
}

// TestStart_AcceptsGitCloneHubMarkerWorkspace covers a hub-dispatched git
// project whose marker file resolves projectDir to its externalized
// ~/.scion/project-configs/<dir>/.scion (a split-storage layout: the
// project's own directory holds only a .scion marker file, not a full
// .scion directory). ProvisionAgent's git-clone branch creates the
// workspace at agentDir/workspace under that externalized directory
// (pkg/agent/provision.go), i.e.
// ~/.scion/project-configs/<dir>/.scion/agents/<agent>/workspace. Start()
// must accept that shape, not just the ~/.scion/projects/<slug> family.
func TestStart_AcceptsGitCloneHubMarkerWorkspace(t *testing.T) {
	t.Setenv("SCION_HOST_UID", "")

	tmpDir := t.TempDir()
	originalHome := os.Getenv("HOME")
	defer func() { _ = os.Setenv("HOME", originalHome) }()
	_ = os.Setenv("HOME", tmpDir)

	projectScionDir := setupHubMarkerProjectConfigsDir(t, tmpDir, "hub-slug__11111111")

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
		Name:        "gc-agent",
		ProjectPath: projectScionDir,
		NoAuth:      true,
		GitClone:    &api.GitCloneConfig{URL: "https://example.com/repo.git"},
	})
	if err != nil {
		t.Fatalf("expected Start to accept a git-clone hub-marker workspace, got error: %v", err)
	}

	wantWorkspace := filepath.Join(projectScionDir, "agents", "gc-agent", "workspace")
	resolvedWant, evalErr := filepath.EvalSymlinks(wantWorkspace)
	if evalErr != nil {
		t.Fatalf("EvalSymlinks(%q): %v", wantWorkspace, evalErr)
	}
	if capturedConfig.Workspace != resolvedWant {
		t.Errorf("RunConfig.Workspace = %q, want %q", capturedConfig.Workspace, resolvedWant)
	}
}

// TestStart_AcceptsNonGitHubMarkerWorkspace covers the non-git counterpart:
// a hub-dispatched, non-git project whose marker file resolves projectDir to
// the same externalized ~/.scion/project-configs/<dir>/.scion shape, with no
// GitClone and no settings.WorkspacePath. ProvisionAgent resolves this to
// its own agentDir/workspace, the same per-agent shape the git-clone branch
// uses, rather than mounting the bare project-configs directory (which held
// only configuration, never the project's actual files).
func TestStart_AcceptsNonGitHubMarkerWorkspace(t *testing.T) {
	t.Setenv("SCION_HOST_UID", "")

	tmpDir := t.TempDir()
	originalHome := os.Getenv("HOME")
	defer func() { _ = os.Setenv("HOME", originalHome) }()
	_ = os.Setenv("HOME", tmpDir)

	projectScionDir := setupHubMarkerProjectConfigsDir(t, tmpDir, "hub-slug2__21111111")

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
		Name:        "non-git-agent",
		ProjectPath: projectScionDir,
		NoAuth:      true,
	})
	if err != nil {
		t.Fatalf("expected Start to accept a non-git hub-marker workspace, got error: %v", err)
	}

	wantWorkspace := filepath.Join(projectScionDir, "agents", "non-git-agent", "workspace")
	resolvedWant, evalErr := filepath.EvalSymlinks(wantWorkspace)
	if evalErr != nil {
		t.Fatalf("EvalSymlinks(%q): %v", wantWorkspace, evalErr)
	}
	if capturedConfig.Workspace != resolvedWant {
		t.Errorf("RunConfig.Workspace = %q, want %q", capturedConfig.Workspace, resolvedWant)
	}
}

// TestStart_RejectsProjectConfigsAgentHome covers the refusal side of the
// same project-configs shape: an agent's home directory
// (~/.scion/project-configs/<dir>/.scion/agents/<agent>/home,
// config.GetAgentHomePath, credential-bearing) must stay refused even
// though its sibling .../workspace is now admitted, and even when the
// persisted source somehow ends up pointing at it directly.
func TestStart_RejectsProjectConfigsAgentHome(t *testing.T) {
	t.Setenv("SCION_HOST_UID", "")

	tmpDir := t.TempDir()
	originalHome := os.Getenv("HOME")
	defer func() { _ = os.Setenv("HOME", originalHome) }()
	_ = os.Setenv("HOME", tmpDir)

	projectScionDir := setupHubMarkerProjectConfigsDir(t, tmpDir, "hub-slug3__31111111")

	agentHome := config.GetAgentHomePath(projectScionDir, "bad-agent")
	if err := os.MkdirAll(agentHome, 0755); err != nil {
		t.Fatal(err)
	}
	agentDir := filepath.Join(projectScionDir, "agents", "bad-agent")
	agentJSON := fmt.Sprintf(`{"harness": "generic", "volumes": [{"source": %q, "target": "/workspace"}]}`, agentHome)
	if err := os.WriteFile(filepath.Join(agentDir, "scion-agent.json"), []byte(agentJSON), 0644); err != nil {
		t.Fatal(err)
	}

	runCalled := false
	mockRT := &runtime.MockRuntime{
		ListFunc: func(ctx context.Context, labelFilter map[string]string) ([]api.AgentInfo, error) {
			return []api.AgentInfo{}, nil
		},
		RunFunc: func(ctx context.Context, config runtime.RunConfig) (string, error) {
			runCalled = true
			return "mock-id", nil
		},
	}
	mgr := NewManager(mockRT)

	_, err := mgr.Start(context.Background(), api.StartOptions{
		Name:        "bad-agent",
		ProjectPath: projectScionDir,
		NoAuth:      true,
	})
	if err == nil {
		t.Fatal("expected Start to refuse a persisted workspace pointing at an agent's own home directory under project-configs")
	}
	if !strings.Contains(err.Error(), "is not an allowed workspace path") {
		t.Errorf("expected the rejection to come from the workspace-source floor, got: %v", err)
	}
	if runCalled {
		t.Error("expected the runtime's Run to never be invoked (fail closed), but it was called")
	}
}

// TestStart_RejectsPreExistingProjectConfigsAgentBareWorkspace covers the
// project-configs counterpart to
// TestStart_RejectsPreExistingGlobalAgentWorkspaceOutsideProjectDir: an
// agent whose persisted workspace is its project's own bare externalized
// directory (~/.scion/project-configs/<dir>, not the per-agent
// .../agents/<agent-id>/workspace shape underneath it) is refused on
// resume, since isAllowedProjectConfigsSubtree never admits the bare
// directory itself. The error must name both ways to recover, the same as
// the global case, since isProjectConfigsPath(projectDir) now joins
// IsGlobalProjectDir(projectDir) in the recovery-hint wrap.
func TestStart_RejectsPreExistingProjectConfigsAgentBareWorkspace(t *testing.T) {
	t.Setenv("SCION_HOST_UID", "")

	tmpDir := t.TempDir()
	originalHome := os.Getenv("HOME")
	defer func() { _ = os.Setenv("HOME", originalHome) }()
	_ = os.Setenv("HOME", tmpDir)

	projectScionDir := setupHubMarkerProjectConfigsDir(t, tmpDir, "hub-slug4__41111111")

	// The project's own bare externalized directory
	// (~/.scion/project-configs/<dir>), not a per-agent subdirectory under
	// it -- the shape isAllowedProjectConfigsSubtree never admits.
	bareProjectConfigsDir := filepath.Dir(projectScionDir)

	agentName := "pre-existing-pc-agent"
	agentDir := filepath.Join(projectScionDir, "agents", agentName)
	if err := os.MkdirAll(agentDir, 0755); err != nil {
		t.Fatal(err)
	}
	agentJSON := fmt.Sprintf(`{"harness": "generic", "volumes": [{"source": %q, "target": "/workspace"}]}`, bareProjectConfigsDir)
	if err := os.WriteFile(filepath.Join(agentDir, "scion-agent.json"), []byte(agentJSON), 0644); err != nil {
		t.Fatal(err)
	}

	runCalled := false
	mockRT := &runtime.MockRuntime{
		ListFunc: func(ctx context.Context, labelFilter map[string]string) ([]api.AgentInfo, error) {
			return []api.AgentInfo{}, nil
		},
		RunFunc: func(ctx context.Context, config runtime.RunConfig) (string, error) {
			runCalled = true
			return "mock-id", nil
		},
	}
	mgr := NewManager(mockRT)

	_, err := mgr.Start(context.Background(), api.StartOptions{
		Name:        agentName,
		ProjectPath: projectScionDir,
		NoAuth:      true,
	})
	if err == nil {
		t.Fatal("expected Start to refuse a pre-existing project-configs agent's bare-directory workspace")
	}
	if !strings.Contains(err.Error(), "is not an allowed workspace path") {
		t.Errorf("expected the base rejection to come from the ~/.scion allow-list floor, got: %v", err)
	}
	if !strings.Contains(err.Error(), "recreate") || !strings.Contains(err.Error(), "--workspace") {
		t.Errorf("expected an actionable error naming both recovery paths (recreate the agent, or an explicit --workspace), got: %v", err)
	}
	if runCalled {
		t.Error("expected the runtime's Run to never be invoked (fail closed), but it was called")
	}
}

// TestStart_AcceptsSettingsWorkspacePath is the missing Start()-level
// positive regression test for the externalized-project branch: a non-git
// project whose settings.yaml sets workspace_path must have that path
// accepted, mounted, and returned as-is (it already equals its own root).
func TestStart_AcceptsSettingsWorkspacePath(t *testing.T) {
	tmpDir := t.TempDir()

	oldWd, _ := os.Getwd()
	_ = os.Chdir(tmpDir)
	defer func() { _ = os.Chdir(oldWd) }()

	originalHome := os.Getenv("HOME")
	defer func() { _ = os.Setenv("HOME", originalHome) }()
	_ = os.Setenv("HOME", tmpDir)

	// A broker/container environment can export the ambient port-publishing
	// setting as a plain boolean string. Koanf's env provider maps it onto a
	// bare key that collides with VersionedSettings' struct-typed field of
	// the same name and fails the whole decode -- not just that one field --
	// taking settings.WorkspacePath down with it. t.Setenv(key, "") is not
	// enough here: the key merely being present still collides, so it must
	// be fully unset for the duration of the test.
	if oldAutoExpose, ok := os.LookupEnv("SCION_AUTO_EXPOSE_PORTS"); ok {
		_ = os.Unsetenv("SCION_AUTO_EXPOSE_PORTS")
		defer func() { _ = os.Setenv("SCION_AUTO_EXPOSE_PORTS", oldAutoExpose) }()
	}

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

	externalWorkspace := filepath.Join(tmpDir, "external-workspace")
	_ = os.MkdirAll(externalWorkspace, 0755)

	projectScionDir := filepath.Join(tmpDir, "project", ".scion")
	_ = os.MkdirAll(projectScionDir, 0755)
	_ = os.WriteFile(filepath.Join(projectScionDir, "settings.yaml"), []byte(fmt.Sprintf(`schema_version: "1"
workspace_path: %q
`, externalWorkspace)), 0644)

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
		Name:        "externalized-agent",
		ProjectPath: projectScionDir,
		NoAuth:      true,
	})
	if err != nil {
		t.Fatalf("expected Start to accept settings.WorkspacePath, got error: %v", err)
	}

	wantWorkspace, evalErr := filepath.EvalSymlinks(externalWorkspace)
	if evalErr != nil {
		t.Fatalf("EvalSymlinks(%q): %v", externalWorkspace, evalErr)
	}
	if capturedConfig.Workspace != wantWorkspace {
		t.Errorf("RunConfig.Workspace = %q, want %q", capturedConfig.Workspace, wantWorkspace)
	}
}

// TestStart_AcceptsSettingsWorkspacePathInGitProjectUnderContainerUID is the
// regression test for a git/non-git detection mismatch between provisioning
// and validation: ProvisionAgent treats a project as non-git whenever
// SCION_HOST_UID is set (isGitWorkspaceProject's container override, to
// avoid creating a worktree whose --relative-paths would be computed against
// the container's mount layout rather than the host's), so a git project
// with settings.WorkspacePath set is provisioned through the externalized,
// non-git branch -- its workspace can legitimately sit outside the repo.
// workspaceSourceRoots must apply the exact same override when validating
// that workspace at Start(), including on every resume, or it re-classifies
// the project as git and checks the externally configured workspace against
// repo-root containment instead -- rejecting a configuration that
// provisioning itself just accepted, purely because SCION_HOST_UID happened
// to be set both times.
func TestStart_AcceptsSettingsWorkspacePathInGitProjectUnderContainerUID(t *testing.T) {
	t.Setenv("SCION_HOST_UID", "1001")

	tmpDir := t.TempDir()

	oldWd, _ := os.Getwd()
	_ = os.Chdir(tmpDir)
	defer func() { _ = os.Chdir(oldWd) }()

	originalHome := os.Getenv("HOME")
	defer func() { _ = os.Setenv("HOME", originalHome) }()
	_ = os.Setenv("HOME", tmpDir)

	if oldAutoExpose, ok := os.LookupEnv("SCION_AUTO_EXPOSE_PORTS"); ok {
		_ = os.Unsetenv("SCION_AUTO_EXPOSE_PORTS")
		defer func() { _ = os.Setenv("SCION_AUTO_EXPOSE_PORTS", oldAutoExpose) }()
	}

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

	// The workspace lives OUTSIDE the git repo entirely -- only valid because
	// this project is provisioned through the non-git, externalized branch.
	externalWorkspace := filepath.Join(tmpDir, "external-workspace")
	_ = os.MkdirAll(externalWorkspace, 0755)

	projectDir := filepath.Join(tmpDir, "project")
	setupTestGitRepoWithBranch(t, projectDir, "unused-branch")
	projectScionDir := filepath.Join(projectDir, ".scion")
	_ = os.MkdirAll(projectScionDir, 0755)
	_ = os.WriteFile(filepath.Join(projectDir, ".gitignore"), []byte("agents/\n"), 0644)
	_ = os.WriteFile(filepath.Join(projectScionDir, "settings.yaml"), []byte(fmt.Sprintf(`schema_version: "1"
workspace_path: %q
`, externalWorkspace)), 0644)

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
		Name:        "container-uid-agent",
		ProjectPath: projectScionDir,
		NoAuth:      true,
	})
	if err != nil {
		t.Fatalf("expected Start to accept settings.WorkspacePath for a git project under SCION_HOST_UID, got error: %v", err)
	}

	wantWorkspace, evalErr := filepath.EvalSymlinks(externalWorkspace)
	if evalErr != nil {
		t.Fatalf("EvalSymlinks(%q): %v", externalWorkspace, evalErr)
	}
	if capturedConfig.Workspace != wantWorkspace {
		t.Errorf("RunConfig.Workspace = %q, want %q", capturedConfig.Workspace, wantWorkspace)
	}
}

// TestStart_AcceptsExplicitHubManagedProjectsWorkspace is a Start()-level
// positive test that was still missing: an explicit
// opts.Workspace under ~/.scion/projects/<slug> (the shape
// pkg/runtimebroker/handlers.go sets for hub-dispatched, non-git projects on
// a local-runtime broker) must be accepted, and RunConfig.Workspace must be
// the resolved path. An explicit workspace skips containment entirely (see
// workspaceSourceRoots), so this exercises the rootless floor's
// ~/.scion/projects/<slug> allowance specifically.
func TestStart_AcceptsExplicitHubManagedProjectsWorkspace(t *testing.T) {
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

	hubWorkspace := filepath.Join(tmpDir, ".scion", "projects", "hub-slug", "workspace")
	_ = os.MkdirAll(hubWorkspace, 0755)

	projectDir := filepath.Join(tmpDir, "hub-project")
	projectScionDir := filepath.Join(projectDir, ".scion")
	_ = os.MkdirAll(projectScionDir, 0755)

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
		Name:        "hub-managed-agent",
		ProjectPath: projectScionDir,
		Workspace:   hubWorkspace,
		NoAuth:      true,
	})
	if err != nil {
		t.Fatalf("expected Start to accept an explicit workspace under ~/.scion/projects/<slug>, got error: %v", err)
	}

	wantWorkspace, evalErr := filepath.EvalSymlinks(hubWorkspace)
	if evalErr != nil {
		t.Fatalf("EvalSymlinks(%q): %v", hubWorkspace, evalErr)
	}
	if capturedConfig.Workspace != wantWorkspace {
		t.Errorf("RunConfig.Workspace = %q, want %q", capturedConfig.Workspace, wantWorkspace)
	}
}

// TestStart_AcceptsNonGitInRepoFallbackWorkspace is the missing Start()-level
// positive regression test for the plain non-git project fallback: no
// settings.WorkspacePath, not the global project -- the workspace is the
// project's own directory (parent of its .scion directory).
func TestStart_AcceptsNonGitInRepoFallbackWorkspace(t *testing.T) {
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

	projectDir := filepath.Join(tmpDir, "plain-project")
	projectScionDir := filepath.Join(projectDir, ".scion")
	_ = os.MkdirAll(projectScionDir, 0755)

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
		Name:        "plain-agent",
		ProjectPath: projectScionDir,
		NoAuth:      true,
	})
	if err != nil {
		t.Fatalf("expected Start to accept the non-git in-repo fallback workspace, got error: %v", err)
	}

	wantWorkspace, evalErr := filepath.EvalSymlinks(projectDir)
	if evalErr != nil {
		t.Fatalf("EvalSymlinks(%q): %v", projectDir, evalErr)
	}
	if capturedConfig.Workspace != wantWorkspace {
		t.Errorf("RunConfig.Workspace = %q, want %q", capturedConfig.Workspace, wantWorkspace)
	}
}

// TestStart_RunConfigWorkspaceIsResolvedPathForSymlinkedProject is the
// missing Start()-level test asserting that RunConfig.Workspace, as handed
// to the runtime, is the resolved path -- not just at the guard and
// buildCommonRunArgs level, but end to end through Start() -- when the
// project directory itself is reached through a symlink.
func TestStart_RunConfigWorkspaceIsResolvedPathForSymlinkedProject(t *testing.T) {
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

	realProjectDir := filepath.Join(tmpDir, "real-project")
	_ = os.MkdirAll(realProjectDir, 0755)
	projectDirLink := filepath.Join(tmpDir, "project-link")
	if err := os.Symlink(realProjectDir, projectDirLink); err != nil {
		t.Fatal(err)
	}
	projectScionDir := filepath.Join(projectDirLink, ".scion")
	_ = os.MkdirAll(projectScionDir, 0755)

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
		Name:        "symlinked-project-agent",
		ProjectPath: projectScionDir,
		NoAuth:      true,
	})
	if err != nil {
		t.Fatalf("expected Start to succeed for a symlinked project directory, got error: %v", err)
	}

	wantRealProjectDir, evalErr := filepath.EvalSymlinks(realProjectDir)
	if evalErr != nil {
		t.Fatalf("EvalSymlinks(%q): %v", realProjectDir, evalErr)
	}
	if capturedConfig.Workspace != wantRealProjectDir {
		t.Errorf("RunConfig.Workspace = %q, want the resolved real path %q (not a path through the symlink)", capturedConfig.Workspace, wantRealProjectDir)
	}
}

// TestStart_RunConfigRepoRootIsResolvedPathForSymlinkedRepo is the
// RepoRoot-side sibling of TestStart_RunConfigWorkspaceIsResolvedPathForSymlinkedProject:
// a git project reached through a symlink must have RunConfig.RepoRoot
// resolved the same way RunConfig.Workspace already is, so the two spellings
// agree by the time buildCommonRunArgs compares them with filepath.Rel.
func TestStart_RunConfigRepoRootIsResolvedPathForSymlinkedRepo(t *testing.T) {
	t.Setenv("SCION_HOST_UID", "")

	tmpDir := t.TempDir()

	oldWd, _ := os.Getwd()
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

	realProjectDir := filepath.Join(tmpDir, "real-project")
	agentName := "symlinked-repo-agent"
	setupTestGitRepoWithBranch(t, realProjectDir, api.Slugify(agentName))
	projectDirLink := filepath.Join(tmpDir, "project-link")
	if err := os.Symlink(realProjectDir, projectDirLink); err != nil {
		t.Fatal(err)
	}
	projectScionDir := filepath.Join(projectDirLink, ".scion")
	_ = os.MkdirAll(projectScionDir, 0755)
	_ = os.WriteFile(filepath.Join(projectDirLink, ".gitignore"), []byte("agents/\n"), 0644)

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

	if err := os.Chdir(projectDirLink); err != nil {
		t.Fatal(err)
	}

	_, err := mgr.Start(context.Background(), api.StartOptions{
		Name:        agentName,
		ProjectPath: projectScionDir,
		NoAuth:      true,
	})
	if err != nil {
		t.Fatalf("expected Start to succeed for a symlinked git project, got error: %v", err)
	}

	realResolvedRepoRoot, evalErr := filepath.EvalSymlinks(realProjectDir)
	if evalErr != nil {
		t.Fatalf("EvalSymlinks(%q): %v", realProjectDir, evalErr)
	}
	if capturedConfig.RepoRoot != realResolvedRepoRoot {
		t.Errorf("RunConfig.RepoRoot = %q, want the resolved real path %q (not a path through the symlink)", capturedConfig.RepoRoot, realResolvedRepoRoot)
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
	// F-111 (design §9): GitCloneForInit must carry the same GitClone config
	// passed to Start — this is what the k8s runtime's NFS init container
	// uses to decide clone-vs-plain-provision (nfsProvisionCommand). Before
	// this fix nothing set this field at all, for git or non-git projects,
	// so the init container never ran for anyone.
	if capturedConfig.GitCloneForInit == nil {
		t.Fatal("GitCloneForInit is nil, want the GitClone config passed to Start")
	}
	if capturedConfig.GitCloneForInit.URL != "https://example.com/repo.git" {
		t.Fatalf("GitCloneForInit.URL = %q, want %q", capturedConfig.GitCloneForInit.URL, "https://example.com/repo.git")
	}
}

// TestStartPropagatesNFSWorkspaceBackend_NonGit_StillRequestsProvisioning is
// the F-111 (design §9) regression test: a non-git, shared-plain NFS project
// must still get NFSPVClaimName/NFSSubPath populated (so the k8s runtime
// still injects its provisioning init container) even though there is no
// GitClone config to carry. Before this fix, the k8s runtime's init
// container was gated on GitCloneForInit != nil, so a nil value here — which
// is correct, there's nothing to clone — silently meant "no provisioning at
// all" instead of "provision, but don't clone." The gate is now independent
// of this field (k8s_runtime.go's nfsInitContainerInjected); this test pins
// the pkg/agent half of that fix: RunConfig must still carry everything the
// gate itself needs, whether or not the project is git-backed.
func TestStartPropagatesNFSWorkspaceBackend_NonGit_StillRequestsProvisioning(t *testing.T) {
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
		Name:            "test-agent",
		ProjectPath:     projectScionDir,
		NoAuth:          true,
		SharedWorkspace: true, // non-git shared-plain: no GitClone at all
		Env: map[string]string{
			"SCION_AGENT_ID":   "agent-789",
			"SCION_PROJECT_ID": "proj-456",
		},
		// GitClone intentionally omitted — this is the non-git case.
	})
	if err != nil {
		t.Fatalf("Start failed: %v", err)
	}

	if capturedConfig.WorkspaceBackendName != "nfs" {
		t.Fatalf("WorkspaceBackendName = %q, want nfs", capturedConfig.WorkspaceBackendName)
	}
	if capturedConfig.NFSPVClaimName != "scion-workspaces-pv" {
		t.Fatalf("NFSPVClaimName = %q, want scion-workspaces-pv (provisioning must still be requested)", capturedConfig.NFSPVClaimName)
	}
	if capturedConfig.NFSSubPath == "" {
		t.Fatal("NFSSubPath is empty, want a resolved subPath (provisioning must still be requested)")
	}
	if capturedConfig.GitCloneForInit != nil {
		t.Fatalf("GitCloneForInit = %+v, want nil (nothing to clone)", capturedConfig.GitCloneForInit)
	}
}

// startRepoRootProjectScaffold creates a minimal project under tmpDir that
// Start can resolve harness/template/settings from (docker profile, a
// "test-harness" harness-config, and a "default" template), changes the
// working directory and HOME to tmpDir for the duration of the test, and
// returns the project's .scion directory (the ProjectPath Start expects).
// Mirrors pkg/runtimebroker's setupRepoRootProjectScaffold. A caller that
// needs non-default settings.yaml content (e.g. an NFS-backed
// workspace_storage config) can overwrite
// filepath.Join(projectScionDir, "settings.yaml") after calling this.
func startRepoRootProjectScaffold(t *testing.T, tmpDir string) string {
	t.Helper()

	t.Chdir(tmpDir)
	t.Setenv("HOME", tmpDir)

	projectDir := filepath.Join(tmpDir, "project")
	projectScionDir := filepath.Join(projectDir, ".scion")
	if err := os.MkdirAll(projectScionDir, 0755); err != nil {
		t.Fatalf("failed to create project .scion dir: %v", err)
	}
	settingsYAML := `schema_version: "1"
active_profile: local
harness_configs:
  test-harness:
    harness: gemini
    user: scion
    image: test-image:latest
profiles:
  local:
    runtime: docker
`
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
	return projectScionDir
}

// TestStartInvalidatesProvisionedRepoRootWhenNFSBackendReplacesWorkspace
// covers server.workspace_storage.backend == "nfs", which applies to
// worktree-per-agent mode too (runtime.SelectWorkspaceBackend), and when it
// fires it replaces effectiveWorkspace with the NFS-backed host path — which
// can combine with a ctx-provisioned repo root from tryProvisionWorktree's
// local host-side worktree in the same dispatch. The repo root validated
// against the pre-backend workspace no longer corresponds to the final
// workspace RunConfig actually uses, so it must be re-validated (and here,
// correctly invalidated) rather than left stale.
func TestStartInvalidatesProvisionedRepoRootWhenNFSBackendReplacesWorkspace(t *testing.T) {
	t.Setenv("SCION_HOST_UID", "")
	tmpDir := t.TempDir()
	projectScionDir := startRepoRootProjectScaffold(t, tmpDir)

	nfsMountRoot := filepath.Join(tmpDir, "nfs")
	settingsYAML := fmt.Sprintf(`schema_version: "1"
active_profile: local
server:
  workspace_storage:
    backend: nfs
    nfs:
      mount_root: %s
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

	// A REAL local worktree — this is what tryProvisionWorktree would have
	// provisioned on the host before the NFS backend applies. The ctx signal
	// below validates successfully against THIS workspace, on purpose: the
	// point of the test is that the NFS backend then replaces it.
	sharedBase := filepath.Join(tmpDir, "shared-base")
	if err := os.MkdirAll(sharedBase, 0755); err != nil {
		t.Fatalf("failed to create shared base dir: %v", err)
	}
	setupGitRepo(t, sharedBase)
	localWorktree := createRealWorktree(t, sharedBase, "agent-a")

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

	ctx := api.ContextWithProvisionedWorktreeRepoRoot(context.Background(), sharedBase)
	if _, err := mgr.Start(ctx, api.StartOptions{
		Name:        "agent-a",
		ProjectPath: projectScionDir,
		NoAuth:      true,
		Workspace:   localWorktree,
		Env: map[string]string{
			"SCION_AGENT_ID":   "agent-a",
			"SCION_PROJECT_ID": "proj-123",
		},
	}); err != nil {
		t.Fatalf("Start failed: %v", err)
	}

	// Confirm the NFS backend actually fired and replaced the workspace —
	// otherwise this test would pass vacuously.
	if capturedConfig.WorkspaceBackendName != "nfs" {
		t.Fatalf("WorkspaceBackendName = %q, want nfs (test setup broken)", capturedConfig.WorkspaceBackendName)
	}
	wantWorkspace := filepath.Join(nfsMountRoot, "share-1", "projects", "proj-123", "workspace")
	if capturedConfig.Workspace != wantWorkspace {
		t.Fatalf("Workspace = %q, want %q (test setup broken)", capturedConfig.Workspace, wantWorkspace)
	}

	// The actual assertion: RepoRoot must NOT be the local sharedBase — that
	// value was validated against localWorktree, not the NFS path RunConfig
	// now actually uses. An explicit --workspace-shaped dispatch (opts.Workspace
	// set) with no valid provisioned root falls through to detectRepoRoot,
	// which stays "" for an explicit workspace.
	if capturedConfig.RepoRoot != "" {
		t.Fatalf("RunConfig.RepoRoot = %q, want \"\" — stale repo root from before the NFS backend replaced the workspace", capturedConfig.RepoRoot)
	}
}

// TestStartUserWorkspaceOverrideYieldsEmptyRepoRoot is the required
// counterpart to the broker-provisioned-worktree RepoRoot stitching fix: a
// user-supplied --workspace (opts.Workspace set with no
// api.ContextWithProvisionedWorktreeRepoRoot signal on ctx) must still
// produce an empty RunConfig.RepoRoot, exactly like before the fix — even
// when the workspace happens to sit inside a git repo, which is the case
// #642 added the explicit-workspace skip for in the first place.
func TestStartUserWorkspaceOverrideYieldsEmptyRepoRoot(t *testing.T) {
	tmpDir := t.TempDir()
	projectScionDir := startRepoRootProjectScaffold(t, tmpDir)

	// The operator's own workspace: a real git repo, so the "explicit
	// workspace skips git detection" guarantee is actually exercised, not
	// vacuously true because there was no repo to detect.
	userWorkspace := filepath.Join(tmpDir, "operators-own-repo")
	if err := os.MkdirAll(userWorkspace, 0755); err != nil {
		t.Fatalf("failed to create user workspace dir: %v", err)
	}
	setupGitRepo(t, userWorkspace)

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
	// context.Background(): no api.ContextWithProvisionedWorktreeRepoRoot
	// signal — this is the plain CLI/local dispatch shape for a user
	// --workspace flag.
	if _, err := mgr.Start(context.Background(), api.StartOptions{
		Name:        "test-agent",
		ProjectPath: projectScionDir,
		NoAuth:      true,
		Workspace:   userWorkspace,
		Env: map[string]string{
			"SCION_AGENT_ID":   "agent-456",
			"SCION_PROJECT_ID": "proj-123",
		},
	}); err != nil {
		t.Fatalf("Start failed: %v", err)
	}

	if capturedConfig.RepoRoot != "" {
		t.Fatalf("RunConfig.RepoRoot = %q, want \"\" for a user --workspace override", capturedConfig.RepoRoot)
	}
	if capturedConfig.Workspace != userWorkspace {
		t.Fatalf("RunConfig.Workspace = %q, want %q", capturedConfig.Workspace, userWorkspace)
	}
}

// TestStartPersistsFreshProvisionedWorktreeRepoRootWhenProvisionAgentIsSkipped
// covers GetAgent skipping ProvisionAgent entirely once an agent directory
// already exists on disk (e.g. a leftover from a deleted hub agent recreated
// under the same name), so a fresh ctx signal on that dispatch would
// otherwise never be persisted — stranding RepoRoot on the very next resume,
// which has no ctx signal of its own. Start must persist the fresh value
// itself whenever it validates and differs from what's already on disk,
// independent of whether ProvisionAgent ran.
func TestStartPersistsFreshProvisionedWorktreeRepoRootWhenProvisionAgentIsSkipped(t *testing.T) {
	t.Setenv("SCION_HOST_UID", "")
	tmpDir := t.TempDir()
	projectScionDir := startRepoRootProjectScaffold(t, tmpDir)

	// The workspace must be a REAL worktree of sharedBase (not just a plain
	// directory) — Start only persists a ctx signal that actually validates,
	// and validation requires a genuine git worktree relationship, not just
	// a matching directory shape.
	sharedBase := t.TempDir()
	setupGitRepo(t, sharedBase)
	worktreesDir := filepath.Join(sharedBase, "worktrees")
	if err := os.MkdirAll(worktreesDir, 0755); err != nil {
		t.Fatalf("failed to create worktrees dir: %v", err)
	}
	userWorkspace := filepath.Join(worktreesDir, "agent-a")
	if err := util.CreateWorktree(userWorkspace, "agent-a"); err != nil {
		t.Fatalf("failed to create real worktree: %v", err)
	}

	mockRT := &runtime.MockRuntime{
		ListFunc: func(ctx context.Context, labelFilter map[string]string) ([]api.AgentInfo, error) {
			return []api.AgentInfo{}, nil
		},
		RunFunc: func(ctx context.Context, config runtime.RunConfig) (string, error) {
			return "mock-id", nil
		},
	}
	mgr := NewManager(mockRT)
	env := map[string]string{"SCION_AGENT_ID": "agent-a", "SCION_PROJECT_ID": "proj-123"}

	// Step 1: create the agent normally as a plain --workspace agent (no ctx
	// signal) — the exact provisioning shape doesn't matter here, only that
	// the agent directory now exists on disk with no repo root persisted.
	if _, err := mgr.Start(context.Background(), api.StartOptions{
		Name: "agent-a", ProjectPath: projectScionDir, NoAuth: true, Workspace: userWorkspace, Env: env,
	}); err != nil {
		t.Fatalf("initial Start failed: %v", err)
	}

	// Step 2: a later dispatch for the SAME agent name whose ctx carries a
	// fresh broker-provisioned-worktree signal (the real relationship: user
	// Workspace is genuinely sharedBase's worktree). Because the agent
	// directory already exists, GetAgent's "agent dir exists" branch skips
	// ProvisionAgent entirely — the only way this value can reach disk is the
	// persistence added to Start for exactly this case.
	ctx := api.ContextWithProvisionedWorktreeRepoRoot(context.Background(), sharedBase)
	if _, err := mgr.Start(ctx, api.StartOptions{
		Name: "agent-a", ProjectPath: projectScionDir, NoAuth: true, Workspace: userWorkspace, Env: env,
	}); err != nil {
		t.Fatalf("second Start failed: %v", err)
	}

	agentDir := config.GetAgentDir(projectScionDir, "agent-a", false)
	gotRoot, err := filepath.EvalSymlinks(readProvisionedWorktreeRepoRoot(agentDir))
	if err != nil {
		t.Fatalf("EvalSymlinks(persisted repo root): %v", err)
	}
	wantRoot, err := filepath.EvalSymlinks(sharedBase)
	if err != nil {
		t.Fatalf("EvalSymlinks(sharedBase): %v", err)
	}
	if gotRoot != wantRoot {
		t.Fatalf("persisted repo root = %q, want %q — the fresh ctx signal was not persisted when ProvisionAgent was skipped", gotRoot, wantRoot)
	}
}

// TestStartPersistsFreshProvisionedWorktreeRepoRootOnSymlinkedBrokerPath is
// TestStartPersistsFreshProvisionedWorktreeRepoRootWhenProvisionAgentIsSkipped's
// sibling for a broker project path that runs through a symlinked ancestor.
// ValidateWorkspaceSource always returns effectiveWorkspace fully resolved,
// so the persistence gate's ctxRepoRoot argument (passed through unresolved,
// exactly as the broker constructs it) and effectiveWorkspace (resolved) can
// name the same real worktree while disagreeing lexically. Before the fix,
// persistProvisionedWorktreeRepoRootIfValid compared them as given and
// silently discarded a value that was actually correct, reproducing the
// original empty-RepoRoot-on-resume bug on any host where the project path
// runs through a symlink.
func TestStartPersistsFreshProvisionedWorktreeRepoRootOnSymlinkedBrokerPath(t *testing.T) {
	t.Setenv("SCION_HOST_UID", "")
	tmpDir := t.TempDir()
	projectScionDir := startRepoRootProjectScaffold(t, tmpDir)

	// The broker-provisioned base sits behind a symlinked ancestor, exactly
	// like a broker project path whose parent directory is itself a symlink.
	realParent := t.TempDir()
	linkParent := t.TempDir()
	linkDir := filepath.Join(linkParent, "link")
	if err := os.Symlink(realParent, linkDir); err != nil {
		t.Fatal(err)
	}
	sharedBase := filepath.Join(linkDir, "shared-base")
	if err := os.MkdirAll(sharedBase, 0755); err != nil {
		t.Fatalf("failed to create shared base dir: %v", err)
	}
	setupGitRepo(t, sharedBase)
	worktreesDir := filepath.Join(sharedBase, "worktrees")
	if err := os.MkdirAll(worktreesDir, 0755); err != nil {
		t.Fatalf("failed to create worktrees dir: %v", err)
	}
	userWorkspace := filepath.Join(worktreesDir, "agent-a")
	if err := util.CreateWorktree(userWorkspace, "agent-a"); err != nil {
		t.Fatalf("failed to create real worktree: %v", err)
	}

	mockRT := &runtime.MockRuntime{
		ListFunc: func(ctx context.Context, labelFilter map[string]string) ([]api.AgentInfo, error) {
			return []api.AgentInfo{}, nil
		},
		RunFunc: func(ctx context.Context, config runtime.RunConfig) (string, error) {
			return "mock-id", nil
		},
	}
	mgr := NewManager(mockRT)
	env := map[string]string{"SCION_AGENT_ID": "agent-a", "SCION_PROJECT_ID": "proj-123"}

	// Step 1: create the agent normally as a plain --workspace agent (no ctx
	// signal) — the agent directory now exists on disk with no repo root
	// persisted.
	if _, err := mgr.Start(context.Background(), api.StartOptions{
		Name: "agent-a", ProjectPath: projectScionDir, NoAuth: true, Workspace: userWorkspace, Env: env,
	}); err != nil {
		t.Fatalf("initial Start failed: %v", err)
	}

	// Step 2: a later dispatch for the SAME agent whose ctx carries a fresh
	// broker-provisioned-worktree signal — the symlinked, unresolved
	// sharedBase, exactly as the broker would pass it. Because the agent
	// directory already exists, GetAgent's "agent dir exists" branch skips
	// ProvisionAgent entirely, so Start's own persistence call is the only
	// way this value can reach disk.
	ctx := api.ContextWithProvisionedWorktreeRepoRoot(context.Background(), sharedBase)
	if _, err := mgr.Start(ctx, api.StartOptions{
		Name: "agent-a", ProjectPath: projectScionDir, NoAuth: true, Workspace: userWorkspace, Env: env,
	}); err != nil {
		t.Fatalf("second Start failed: %v", err)
	}

	agentDir := config.GetAgentDir(projectScionDir, "agent-a", false)
	persisted := readProvisionedWorktreeRepoRoot(agentDir)
	if persisted == "" {
		t.Fatal("persisted repo root is empty — the fresh ctx signal was not persisted on a symlinked broker path")
	}
	gotRoot, err := filepath.EvalSymlinks(persisted)
	if err != nil {
		t.Fatalf("EvalSymlinks(persisted repo root): %v", err)
	}
	wantRoot, err := filepath.EvalSymlinks(sharedBase)
	if err != nil {
		t.Fatalf("EvalSymlinks(sharedBase): %v", err)
	}
	if gotRoot != wantRoot {
		t.Fatalf("persisted repo root = %q, want %q — the fresh ctx signal was not persisted on a symlinked broker path", gotRoot, wantRoot)
	}

	// Step 3: a plain resume dispatch (no ctx signal, no Workspace — exactly
	// what the hub sends on restart) must recover RepoRoot from the
	// persisted state alone, proving the persisted value is not just
	// non-empty but actually usable.
	var capturedConfig runtime.RunConfig
	mockRT.RunFunc = func(ctx context.Context, config runtime.RunConfig) (string, error) {
		capturedConfig = config
		return "mock-id", nil
	}
	if _, err := mgr.Start(context.Background(), api.StartOptions{
		Name: "agent-a", ProjectPath: projectScionDir, NoAuth: true, Workspace: "", Resume: true, Env: env,
	}); err != nil {
		t.Fatalf("resume Start failed: %v", err)
	}
	if capturedConfig.RepoRoot == "" {
		t.Fatal("resume RunConfig.RepoRoot is empty — the persisted repo root from a symlinked broker path did not survive resume")
	}
}

// TestStartRejectsSymlinkedWorktreeLeafPointingAtSiblingWorktree is the
// permanent regression test for why run.go's post-ValidateWorkspaceSource
// repo-root re-validation block was removed entirely, rather than kept or
// patched: that block fed an ALREADY-RESOLVED effectiveWorkspace into
// validatedWorktreeRepoRoot, which
// erases exactly the lexical-vs-resolved name mismatch
// provision.ValidateWorktreeForBase depends on to catch a worktree leaf that
// is itself a symlink to a sibling worktree. Fed the ORIGINAL (unresolved)
// pair, as Start's first validation pass and detectRepoRoot's fallback both
// do, the mismatch is caught and the dispatch falls back to a plain
// /workspace mount instead of trusting a candidate repo root it must not —
// mounting that root's .git read-write would give the container the shared
// base, not just the one sibling worktree it was ever entitled to.
func TestStartRejectsSymlinkedWorktreeLeafPointingAtSiblingWorktree(t *testing.T) {
	t.Setenv("SCION_HOST_UID", "")
	tmpDir := t.TempDir()
	projectScionDir := startRepoRootProjectScaffold(t, tmpDir)

	sharedBase := t.TempDir()
	setupGitRepo(t, sharedBase)
	worktreesDir := filepath.Join(sharedBase, "worktrees")
	if err := os.MkdirAll(worktreesDir, 0755); err != nil {
		t.Fatalf("failed to create worktrees dir: %v", err)
	}

	// agent-b's worktree is genuine.
	siblingWorktree := filepath.Join(worktreesDir, "agent-b")
	if err := util.CreateWorktree(siblingWorktree, "agent-b"); err != nil {
		t.Fatalf("failed to create sibling worktree: %v", err)
	}

	// agent-a has no real worktree of its own: the path scion would expect
	// to find it at is instead a symlink to agent-b's — a leaf that
	// lexically looks like this agent's own worktree but resolves to a
	// different agent's.
	leafPath := filepath.Join(worktreesDir, "agent-a")
	if err := os.Symlink(siblingWorktree, leafPath); err != nil {
		t.Fatal(err)
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
	env := map[string]string{"SCION_AGENT_ID": "agent-a", "SCION_PROJECT_ID": "proj-123"}

	ctx := api.ContextWithProvisionedWorktreeRepoRoot(context.Background(), sharedBase)
	if _, err := mgr.Start(ctx, api.StartOptions{
		Name: "agent-a", ProjectPath: projectScionDir, NoAuth: true, Workspace: leafPath, Env: env,
	}); err != nil {
		t.Fatalf("Start failed: %v", err)
	}

	if capturedConfig.RepoRoot != "" {
		t.Fatalf("RunConfig.RepoRoot = %q, want empty — a worktree leaf symlinked to a sibling worktree must never validate sharedBase as this agent's repo root", capturedConfig.RepoRoot)
	}
	if capturedConfig.ContainerWorkspace != "/workspace" {
		t.Fatalf("RunConfig.ContainerWorkspace = %q, want %q (the plain mount fallback, not the worktree dual-mount branch that would expose sharedBase's .git)", capturedConfig.ContainerWorkspace, "/workspace")
	}
}

// TestStartDoesNotPersistUnvalidatedCtxRepoRoot covers the persistence path:
// it must only ever write a repo root that actually validated (repoRoot ==
// ctxRepoRoot), never a bare ctx value that the validator rejected and
// detectRepoRoot then fell back past. Otherwise a bad value could reach disk
// even though it never reached RunConfig on the dispatch that produced it.
func TestStartDoesNotPersistUnvalidatedCtxRepoRoot(t *testing.T) {
	tmpDir := t.TempDir()
	projectScionDir := startRepoRootProjectScaffold(t, tmpDir)

	// ctxRoot is a real git repo, but it has no relationship at all to
	// userWorkspace (a plain, unrelated directory) — the validator must
	// reject this pairing.
	ctxRoot := t.TempDir()
	setupGitRepo(t, ctxRoot)
	userWorkspace := filepath.Join(tmpDir, "operators-own-dir")
	if err := os.MkdirAll(userWorkspace, 0755); err != nil {
		t.Fatalf("failed to create user workspace dir: %v", err)
	}

	mockRT := &runtime.MockRuntime{
		ListFunc: func(ctx context.Context, labelFilter map[string]string) ([]api.AgentInfo, error) {
			return []api.AgentInfo{}, nil
		},
		RunFunc: func(ctx context.Context, config runtime.RunConfig) (string, error) {
			return "mock-id", nil
		},
	}
	mgr := NewManager(mockRT)

	ctx := api.ContextWithProvisionedWorktreeRepoRoot(context.Background(), ctxRoot)
	if _, err := mgr.Start(ctx, api.StartOptions{
		Name:        "agent-a",
		ProjectPath: projectScionDir,
		NoAuth:      true,
		Workspace:   userWorkspace,
		Env:         map[string]string{"SCION_AGENT_ID": "agent-a", "SCION_PROJECT_ID": "proj-123"},
	}); err != nil {
		t.Fatalf("Start failed: %v", err)
	}

	agentDir := config.GetAgentDir(projectScionDir, "agent-a", false)
	if got := readProvisionedWorktreeRepoRoot(agentDir); got != "" {
		t.Fatalf("persisted repo root = %q, want \"\" — an unvalidated ctx value must never be persisted", got)
	}
}

// TestStartPersistsNothingWhenWorkspaceLeafResolvesOutsideItsOwnWorktree
// covers persistProvisionedWorktreeRepoRootIfValid's own comparison
// directly: the workspace value it receives must be compared in its
// original, as-given form, not a form already resolved ahead of the call.
// Resolving ahead of time would make this comparison accept a workspace leaf
// whose own final path element resolves to a different worktree name than
// the one it lexically presents — the same shape covered for
// RunConfig.RepoRoot's own, separate comparison.
func TestStartPersistsNothingWhenWorkspaceLeafResolvesOutsideItsOwnWorktree(t *testing.T) {
	t.Setenv("SCION_HOST_UID", "")
	tmpDir := t.TempDir()
	projectScionDir := startRepoRootProjectScaffold(t, tmpDir)

	sharedBase := t.TempDir()
	setupGitRepo(t, sharedBase)
	worktreesDir := filepath.Join(sharedBase, "worktrees")
	if err := os.MkdirAll(worktreesDir, 0755); err != nil {
		t.Fatalf("failed to create worktrees dir: %v", err)
	}
	otherWorktree := filepath.Join(worktreesDir, "agent-b")
	if err := util.CreateWorktree(otherWorktree, "agent-b"); err != nil {
		t.Fatalf("failed to create other worktree: %v", err)
	}
	// agent-a's own expected path is a symlink to agent-b's worktree rather
	// than a worktree of its own.
	leafPath := filepath.Join(worktreesDir, "agent-a")
	if err := os.Symlink(otherWorktree, leafPath); err != nil {
		t.Fatal(err)
	}

	mockRT := &runtime.MockRuntime{
		ListFunc: func(ctx context.Context, labelFilter map[string]string) ([]api.AgentInfo, error) {
			return []api.AgentInfo{}, nil
		},
		RunFunc: func(ctx context.Context, config runtime.RunConfig) (string, error) {
			return "mock-id", nil
		},
	}
	mgr := NewManager(mockRT)

	ctx := api.ContextWithProvisionedWorktreeRepoRoot(context.Background(), sharedBase)
	if _, err := mgr.Start(ctx, api.StartOptions{
		Name:        "agent-a",
		ProjectPath: projectScionDir,
		NoAuth:      true,
		Workspace:   leafPath,
		Env:         map[string]string{"SCION_AGENT_ID": "agent-a", "SCION_PROJECT_ID": "proj-123"},
	}); err != nil {
		t.Fatalf("Start failed: %v", err)
	}

	agentDir := config.GetAgentDir(projectScionDir, "agent-a", false)
	if got := readProvisionedWorktreeRepoRoot(agentDir); got != "" {
		t.Fatalf("persisted repo root = %q, want \"\"", got)
	}
	if _, err := os.Stat(filepath.Join(agentDir, provisionedWorktreeStateFile)); !os.IsNotExist(err) {
		t.Fatalf("expected no persisted-worktree state file to exist, stat error = %v", err)
	}
}

// TestStartPersistsTheAliasNotTheResolvedBase covers the other direction of
// the same comparison: when the ctx-provided repo root itself is reached
// through an ancestor symlink, and the workspace value given to this same
// dispatch is reached through the identical, as-given ancestor spelling, the
// comparison accepts the pair and persists the value exactly as given — not
// a form resolved ahead of persisting it.
func TestStartPersistsTheAliasNotTheResolvedBase(t *testing.T) {
	t.Setenv("SCION_HOST_UID", "")
	tmpDir := t.TempDir()
	projectScionDir := startRepoRootProjectScaffold(t, tmpDir)

	realParent := t.TempDir()
	linkParent := t.TempDir()
	linkDir := filepath.Join(linkParent, "link")
	if err := os.Symlink(realParent, linkDir); err != nil {
		t.Fatal(err)
	}
	sharedBase := filepath.Join(linkDir, "shared-base")
	if err := os.MkdirAll(sharedBase, 0755); err != nil {
		t.Fatalf("failed to create shared base dir: %v", err)
	}
	setupGitRepo(t, sharedBase)
	worktreesDir := filepath.Join(sharedBase, "worktrees")
	if err := os.MkdirAll(worktreesDir, 0755); err != nil {
		t.Fatalf("failed to create worktrees dir: %v", err)
	}
	userWorkspace := filepath.Join(worktreesDir, "agent-a")
	if err := util.CreateWorktree(userWorkspace, "agent-a"); err != nil {
		t.Fatalf("failed to create real worktree: %v", err)
	}

	mockRT := &runtime.MockRuntime{
		ListFunc: func(ctx context.Context, labelFilter map[string]string) ([]api.AgentInfo, error) {
			return []api.AgentInfo{}, nil
		},
		RunFunc: func(ctx context.Context, config runtime.RunConfig) (string, error) {
			return "mock-id", nil
		},
	}
	mgr := NewManager(mockRT)

	ctx := api.ContextWithProvisionedWorktreeRepoRoot(context.Background(), sharedBase)
	if _, err := mgr.Start(ctx, api.StartOptions{
		Name:        "agent-a",
		ProjectPath: projectScionDir,
		NoAuth:      true,
		Workspace:   userWorkspace,
		Env:         map[string]string{"SCION_AGENT_ID": "agent-a", "SCION_PROJECT_ID": "proj-123"},
	}); err != nil {
		t.Fatalf("Start failed: %v", err)
	}

	agentDir := config.GetAgentDir(projectScionDir, "agent-a", false)
	got := readProvisionedWorktreeRepoRoot(agentDir)
	if got != sharedBase {
		t.Fatalf("persisted repo root = %q, want %q (the value as given, not its resolved form)", got, sharedBase)
	}
	if resolvedBase, err := filepath.EvalSymlinks(sharedBase); err == nil && got == resolvedBase {
		t.Fatalf("persisted repo root = %q, must not be the resolved form %q", got, resolvedBase)
	}
}

// TestStartDoesNotPersistWhenRepoRootStaysEmpty ties persistProvisionedWorktreeRepoRootIfValid's
// decision to Start's own: whenever Start's own comparison for
// RunConfig.RepoRoot rejects a pair (leaving RepoRoot empty for this
// dispatch), the persistence gate must reject the identical pair too, rather
// than saving a value this same dispatch did not trust enough to use.
func TestStartDoesNotPersistWhenRepoRootStaysEmpty(t *testing.T) {
	t.Setenv("SCION_HOST_UID", "")
	tmpDir := t.TempDir()
	projectScionDir := startRepoRootProjectScaffold(t, tmpDir)

	sharedBase := t.TempDir()
	setupGitRepo(t, sharedBase)
	worktreesDir := filepath.Join(sharedBase, "worktrees")
	if err := os.MkdirAll(worktreesDir, 0755); err != nil {
		t.Fatalf("failed to create worktrees dir: %v", err)
	}
	otherWorktree := filepath.Join(worktreesDir, "agent-b")
	if err := util.CreateWorktree(otherWorktree, "agent-b"); err != nil {
		t.Fatalf("failed to create other worktree: %v", err)
	}
	leafPath := filepath.Join(worktreesDir, "agent-a")
	if err := os.Symlink(otherWorktree, leafPath); err != nil {
		t.Fatal(err)
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

	ctx := api.ContextWithProvisionedWorktreeRepoRoot(context.Background(), sharedBase)
	if _, err := mgr.Start(ctx, api.StartOptions{
		Name:        "agent-a",
		ProjectPath: projectScionDir,
		NoAuth:      true,
		Workspace:   leafPath,
		Env:         map[string]string{"SCION_AGENT_ID": "agent-a", "SCION_PROJECT_ID": "proj-123"},
	}); err != nil {
		t.Fatalf("Start failed: %v", err)
	}

	if capturedConfig.RepoRoot != "" {
		t.Fatalf("RunConfig.RepoRoot = %q, want empty for this pair", capturedConfig.RepoRoot)
	}
	agentDir := config.GetAgentDir(projectScionDir, "agent-a", false)
	if got := readProvisionedWorktreeRepoRoot(agentDir); got != "" {
		t.Fatalf("persisted repo root = %q, want \"\" — Start did not set RepoRoot for this pair, so it must not be persisted either", got)
	}
}

// TestStartResolvesRepoRootWhenPersistedRootIsSymlinkedAndWorkspaceIsResolved
// and the test following it cover run.go's own RunConfig.RepoRoot comparison
// (distinct from persistProvisionedWorktreeRepoRootIfValid's, covered above):
// the persisted repo root and the dispatch's own workspace value can each
// independently arrive already resolved or not, and a resolve-parent-only
// comparison must accept the pair either way while still telling apart a
// workspace leaf whose own final path element names something else.
// setPersistedWorkspaceVolumeSource rewrites the Source of agentDir's
// persisted /workspace volume mount in scion-agent.json, so a later Start
// dispatch's extractWorkspaceFromVolumes recovers exactly newSource rather
// than whatever spelling an earlier dispatch recorded.
func setPersistedWorkspaceVolumeSource(t *testing.T, agentDir, newSource string) {
	t.Helper()
	cfgPath := filepath.Join(agentDir, "scion-agent.json")
	data, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatalf("read %s: %v", cfgPath, err)
	}
	var cfg api.ScionConfig
	if err := json.Unmarshal(data, &cfg); err != nil {
		t.Fatalf("unmarshal %s: %v", cfgPath, err)
	}
	found := false
	for i := range cfg.Volumes {
		if cfg.Volumes[i].Target == "/workspace" {
			cfg.Volumes[i].Source = newSource
			found = true
		}
	}
	if !found {
		t.Fatalf("no /workspace volume found in %s to rewrite", cfgPath)
	}
	newData, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		t.Fatalf("marshal updated config: %v", err)
	}
	if err := os.WriteFile(cfgPath, newData, 0644); err != nil {
		t.Fatalf("write %s: %v", cfgPath, err)
	}
}

func TestStartResolvesRepoRootWhenPersistedRootIsSymlinkedAndWorkspaceIsResolved(t *testing.T) {
	t.Setenv("SCION_HOST_UID", "")
	tmpDir := t.TempDir()
	projectScionDir := startRepoRootProjectScaffold(t, tmpDir)

	realParent := t.TempDir()
	linkParent := t.TempDir()
	linkDir := filepath.Join(linkParent, "link")
	if err := os.Symlink(realParent, linkDir); err != nil {
		t.Fatal(err)
	}
	sharedBase := filepath.Join(linkDir, "shared-base")
	if err := os.MkdirAll(sharedBase, 0755); err != nil {
		t.Fatalf("failed to create shared base dir: %v", err)
	}
	setupGitRepo(t, sharedBase)
	worktreesDir := filepath.Join(sharedBase, "worktrees")
	if err := os.MkdirAll(worktreesDir, 0755); err != nil {
		t.Fatalf("failed to create worktrees dir: %v", err)
	}
	userWorkspace := filepath.Join(worktreesDir, "agent-a")
	if err := util.CreateWorktree(userWorkspace, "agent-a"); err != nil {
		t.Fatalf("failed to create real worktree: %v", err)
	}
	resolvedWorkspace, err := filepath.EvalSymlinks(userWorkspace)
	if err != nil {
		t.Fatalf("EvalSymlinks(userWorkspace): %v", err)
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
	env := map[string]string{"SCION_AGENT_ID": "agent-a", "SCION_PROJECT_ID": "proj-123"}

	// Materialize the agent directory, then set the persisted repo root
	// directly in its symlinked, as-given form, independent of whichever
	// path a prior dispatch would have used to get it there.
	if _, err := mgr.Start(context.Background(), api.StartOptions{
		Name: "agent-a", ProjectPath: projectScionDir, NoAuth: true, Workspace: userWorkspace, Env: env,
	}); err != nil {
		t.Fatalf("initial Start failed: %v", err)
	}
	agentDir := config.GetAgentDir(projectScionDir, "agent-a", false)
	if err := writeProvisionedWorktreeRepoRoot(agentDir, sharedBase); err != nil {
		t.Fatalf("writeProvisionedWorktreeRepoRoot: %v", err)
	}
	// An explicit-workspace agent recovers its workspace on every later
	// dispatch from the persisted /workspace volume, not from opts.Workspace
	// again — rewrite that persisted volume's source directly to the
	// resolved form, to arrange the "workspace already resolved" side of
	// this test independent of the initial dispatch's own spelling.
	setPersistedWorkspaceVolumeSource(t, agentDir, resolvedWorkspace)

	// This dispatch recovers the persisted, symlinked repo root with no
	// fresh ctx signal, and its own recovered workspace value is already
	// resolved.
	if _, err := mgr.Start(context.Background(), api.StartOptions{
		Name: "agent-a", ProjectPath: projectScionDir, NoAuth: true, Env: env,
	}); err != nil {
		t.Fatalf("second Start failed: %v", err)
	}

	if capturedConfig.RepoRoot == "" {
		t.Fatal("RunConfig.RepoRoot is empty")
	}
	gotRoot, err := filepath.EvalSymlinks(capturedConfig.RepoRoot)
	if err != nil {
		t.Fatalf("EvalSymlinks(RepoRoot): %v", err)
	}
	wantRoot, err := filepath.EvalSymlinks(sharedBase)
	if err != nil {
		t.Fatalf("EvalSymlinks(sharedBase): %v", err)
	}
	if gotRoot != wantRoot {
		t.Fatalf("RunConfig.RepoRoot resolves to %q, want %q", gotRoot, wantRoot)
	}
}

// TestStartResolvesRepoRootWhenPersistedRootIsResolvedAndWorkspaceIsSymlinked
// is the other order: the persisted repo root already arrives resolved,
// while this dispatch's own workspace value is symlinked.
func TestStartResolvesRepoRootWhenPersistedRootIsResolvedAndWorkspaceIsSymlinked(t *testing.T) {
	t.Setenv("SCION_HOST_UID", "")
	tmpDir := t.TempDir()
	projectScionDir := startRepoRootProjectScaffold(t, tmpDir)

	realParent := t.TempDir()
	linkParent := t.TempDir()
	linkDir := filepath.Join(linkParent, "link")
	if err := os.Symlink(realParent, linkDir); err != nil {
		t.Fatal(err)
	}
	sharedBase := filepath.Join(linkDir, "shared-base")
	if err := os.MkdirAll(sharedBase, 0755); err != nil {
		t.Fatalf("failed to create shared base dir: %v", err)
	}
	setupGitRepo(t, sharedBase)
	worktreesDir := filepath.Join(sharedBase, "worktrees")
	if err := os.MkdirAll(worktreesDir, 0755); err != nil {
		t.Fatalf("failed to create worktrees dir: %v", err)
	}
	userWorkspace := filepath.Join(worktreesDir, "agent-a")
	if err := util.CreateWorktree(userWorkspace, "agent-a"); err != nil {
		t.Fatalf("failed to create real worktree: %v", err)
	}
	resolvedBase, err := filepath.EvalSymlinks(sharedBase)
	if err != nil {
		t.Fatalf("EvalSymlinks(sharedBase): %v", err)
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
	env := map[string]string{"SCION_AGENT_ID": "agent-a", "SCION_PROJECT_ID": "proj-123"}

	if _, err := mgr.Start(context.Background(), api.StartOptions{
		Name: "agent-a", ProjectPath: projectScionDir, NoAuth: true, Workspace: userWorkspace, Env: env,
	}); err != nil {
		t.Fatalf("initial Start failed: %v", err)
	}
	agentDir := config.GetAgentDir(projectScionDir, "agent-a", false)
	// Set the persisted repo root directly in its resolved form, independent
	// of whichever path a prior dispatch would have used to get it there.
	if err := writeProvisionedWorktreeRepoRoot(agentDir, resolvedBase); err != nil {
		t.Fatalf("writeProvisionedWorktreeRepoRoot: %v", err)
	}

	// This dispatch recovers the persisted, resolved repo root with no fresh
	// ctx signal, but its own workspace value is symlinked.
	if _, err := mgr.Start(context.Background(), api.StartOptions{
		Name: "agent-a", ProjectPath: projectScionDir, NoAuth: true, Workspace: userWorkspace, Env: env,
	}); err != nil {
		t.Fatalf("second Start failed: %v", err)
	}

	if capturedConfig.RepoRoot == "" {
		t.Fatal("RunConfig.RepoRoot is empty")
	}
	gotRoot, err := filepath.EvalSymlinks(capturedConfig.RepoRoot)
	if err != nil {
		t.Fatalf("EvalSymlinks(RepoRoot): %v", err)
	}
	if gotRoot != resolvedBase {
		t.Fatalf("RunConfig.RepoRoot resolves to %q, want %q", gotRoot, resolvedBase)
	}
}

// TestStartResumeDoesNotAdoptRepoRootFromAgentInfoFile is a regression test
// for the storage boundary that keeps the persisted repo root out of
// container-writable storage: a user --workspace agent on a directory shaped
// like "<repo>/worktrees/<name>" (not a real git worktree). The first Start
// correctly yields an empty RepoRoot. agent-info.json is writable at
// runtime, so this test writes a "provisionedWorktreeRepoRoot" key into it
// directly to prove run.go must never source RepoRoot from agentHome on
// resume; the value lives in a broker-owned file under agentDir that is not
// mounted into the container, and the validator rejects a non-worktree
// directory regardless.
func TestStartResumeDoesNotAdoptRepoRootFromAgentInfoFile(t *testing.T) {
	tmpDir := t.TempDir()
	projectScionDir := startRepoRootProjectScaffold(t, tmpDir)

	// Use a REAL git worktree so the isolation is precise: the written value
	// below would validate successfully if run.go consulted agent-info.json
	// for it. That isolates "is the container-writable file even consulted"
	// (this test) from "does the validator reject a fake worktree shape"
	// (covered separately in pkg/provision's validator tests).
	t.Setenv("SCION_HOST_UID", "")
	userRepo := filepath.Join(tmpDir, "userrepo")
	if err := os.MkdirAll(userRepo, 0755); err != nil {
		t.Fatalf("failed to create user repo dir: %v", err)
	}
	setupGitRepo(t, userRepo)
	userWorkspace := createRealWorktree(t, userRepo, "foo")

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
	env := map[string]string{"SCION_AGENT_ID": "agent-a", "SCION_PROJECT_ID": "proj-123"}

	// First Start: plain user --workspace, no ctx signal. Correct baseline:
	// RepoRoot is empty.
	if _, err := mgr.Start(context.Background(), api.StartOptions{
		Name: "agent-a", ProjectPath: projectScionDir, NoAuth: true, Workspace: userWorkspace, Env: env,
	}); err != nil {
		t.Fatalf("initial Start failed: %v", err)
	}
	if capturedConfig.RepoRoot != "" {
		t.Fatalf("baseline RunConfig.RepoRoot = %q, want \"\" before the state file is written", capturedConfig.RepoRoot)
	}

	// Write a "provisionedWorktreeRepoRoot" key into agent-info.json in
	// agentHome (bind-mounted read-write into the container); RepoRoot must
	// never be read from agentHome.
	agentHome := config.GetAgentHomePath(projectScionDir, "agent-a")
	containerWritten := []byte(`{"provisionedWorktreeRepoRoot":"` + userRepo + `"}`)
	if err := os.WriteFile(filepath.Join(agentHome, "agent-info.json"), containerWritten, 0644); err != nil {
		t.Fatalf("failed to write agent-info.json: %v", err)
	}

	// Resume: no ctx signal, empty Workspace — the shape that would surface
	// a container-writable value as a live RepoRoot if it were still consulted.
	capturedConfig = runtime.RunConfig{}
	if _, err := mgr.Start(context.Background(), api.StartOptions{
		Name: "agent-a", ProjectPath: projectScionDir, NoAuth: true, Resume: true, Env: env,
	}); err != nil {
		t.Fatalf("resume Start failed: %v", err)
	}

	if capturedConfig.RepoRoot != "" {
		t.Fatalf("resume RunConfig.RepoRoot = %q, want \"\" — a container-writable agent-info.json must not be able to set RepoRoot", capturedConfig.RepoRoot)
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

// --- ptone/scion#2156: Hub settings image / imagePullPolicy precedence ---
//
// ProvisionAgent bakes the harness-config file's own `image` (and, per the
// fix below, a Hub settings harness_configs.<h>.image) into
// finalScionCfg.Image as a fallback base, with an explicit template/inline
// `image:` merged in on top as an override. Both end up as a non-empty
// finalScionCfg.Image, indistinguishable from each other by value alone —
// which matters because for an EXISTING agent, finalScionCfg.Image is
// whatever was persisted in scion-agent.json at a previous provision, and
// may now be stale relative to current settings. Start's own
// image-resolution chain resolves the on-disk file default and the current
// settings value fresh on every call, so it must determine "is there a
// genuine template/inline override" directly (by re-reading the template
// chain and inline config) rather than by
// comparing finalScionCfg.Image against anything — a value comparison can't
// tell a stale persisted fallback apart from a real override. These tests
// pin the fixed precedence end to end through Manager.Start, capturing the
// runtime.RunConfig that would become the container/pod spec.

// TestStart_SettingsImageOverridesHarnessConfigFileDefault: with no
// template/inline override, a Hub settings harness_configs.<h>.image must
// win over the harness-config file's own `image:` default.
func TestStart_SettingsImageOverridesHarnessConfigFileDefault(t *testing.T) {
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
	_ = os.WriteFile(filepath.Join(hcDir, "config.yaml"), []byte("harness: generic\nuser: scion\nimage: file-default:latest\n"), 0644)

	tplDir := filepath.Join(globalScionDir, "templates", "default")
	_ = os.MkdirAll(tplDir, 0755)
	_ = os.WriteFile(filepath.Join(tplDir, "scion-agent.json"), []byte(`{"default_harness_config": "test-harness"}`), 0644)

	_ = os.WriteFile(filepath.Join(globalScionDir, "settings.yaml"), []byte(`schema_version: "1"
active_profile: local
profiles:
  local:
    runtime: docker
harness_configs:
  test-harness:
    harness: generic
    image: settings-pinned:v1
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
	}

	mgr := NewManager(mockRT)

	_, err := mgr.Start(context.Background(), api.StartOptions{
		Name:        "test-agent",
		ProjectPath: projectScionDir,
		BrokerMode:  true,
		NoAuth:      true,
	})
	if err != nil {
		t.Fatalf("Start failed: %v", err)
	}

	if capturedConfig.Image != "settings-pinned:v1" {
		t.Errorf("expected settings image to win over the harness-config file default, got %q", capturedConfig.Image)
	}
}

// TestStart_DispatchImageStillOutranksSettings: the agent-create request /
// dispatch --image (opts.Image, tier 1 in the precedence table) must still
// outrank a Hub settings image (ptone/scion#2156).
func TestStart_DispatchImageStillOutranksSettings(t *testing.T) {
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
	_ = os.WriteFile(filepath.Join(hcDir, "config.yaml"), []byte("harness: generic\nuser: scion\nimage: file-default:latest\n"), 0644)

	tplDir := filepath.Join(globalScionDir, "templates", "default")
	_ = os.MkdirAll(tplDir, 0755)
	_ = os.WriteFile(filepath.Join(tplDir, "scion-agent.json"), []byte(`{"default_harness_config": "test-harness"}`), 0644)

	_ = os.WriteFile(filepath.Join(globalScionDir, "settings.yaml"), []byte(`schema_version: "1"
active_profile: local
profiles:
  local:
    runtime: docker
harness_configs:
  test-harness:
    harness: generic
    image: settings-pinned:v1
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
	}

	mgr := NewManager(mockRT)

	_, err := mgr.Start(context.Background(), api.StartOptions{
		Name:        "test-agent",
		ProjectPath: projectScionDir,
		Image:       "dispatch-pinned:v9",
		BrokerMode:  true,
		NoAuth:      true,
	})
	if err != nil {
		t.Fatalf("Start failed: %v", err)
	}

	if capturedConfig.Image != "dispatch-pinned:v9" {
		t.Errorf("expected dispatch --image to outrank the settings image, got %q", capturedConfig.Image)
	}
}

// TestStart_TemplateImageStillOutranksSettingsAndFileDefault: an explicit
// template `image:` / `kubernetes.imagePullPolicy` must still outrank both a
// Hub settings value and the harness-config file default (ptone/scion#2156).
func TestStart_TemplateImageStillOutranksSettingsAndFileDefault(t *testing.T) {
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
	_ = os.WriteFile(filepath.Join(hcDir, "config.yaml"), []byte("harness: generic\nuser: scion\nimage: file-default:latest\n"), 0644)

	tplDir := filepath.Join(globalScionDir, "templates", "default")
	_ = os.MkdirAll(tplDir, 0755)
	_ = os.WriteFile(filepath.Join(tplDir, "scion-agent.json"), []byte(`{"default_harness_config": "test-harness", "image": "template-pinned:v2", "kubernetes": {"imagePullPolicy": "Never"}}`), 0644)

	_ = os.WriteFile(filepath.Join(globalScionDir, "settings.yaml"), []byte(`schema_version: "1"
active_profile: local
profiles:
  local:
    runtime: docker
harness_configs:
  test-harness:
    harness: generic
    image: settings-pinned:v1
    image_pull_policy: Always
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
	}

	mgr := NewManager(mockRT)

	_, err := mgr.Start(context.Background(), api.StartOptions{
		Name:        "test-agent",
		ProjectPath: projectScionDir,
		BrokerMode:  true,
		NoAuth:      true,
	})
	if err != nil {
		t.Fatalf("Start failed: %v", err)
	}

	if capturedConfig.Image != "template-pinned:v2" {
		t.Errorf("expected the explicit template image to outrank settings, got %q", capturedConfig.Image)
	}
	gotPolicy := ""
	if capturedConfig.Kubernetes != nil {
		gotPolicy = capturedConfig.Kubernetes.ImagePullPolicy
	}
	if gotPolicy != "Never" {
		t.Errorf("expected the explicit template imagePullPolicy to outrank settings, got %q", gotPolicy)
	}
}

// TestStart_RestartAfterSettingsImageAdded_SettingsWinsOverStalePersistedFileDefault
// pins ptone/scion#2156: an agent provisioned BEFORE a Hub settings image
// existed has the harness-config file's default persisted in its
// scion-agent.json. Restarting that agent after an operator adds
// harness_configs.<h>.image to settings must pick up the settings image, not
// keep re-running the stale persisted file default. A fresh single-Start
// test cannot exercise this: ProvisionAgent's settings-precedence fix
// already makes finalScionCfg.Image correct on a first, from-scratch
// provision, regardless of how Start treats it afterward — only a genuine
// restart of an EXISTING agent distinguishes "settings win" from "the file
// default happens to be baked into the persisted config too".
func TestStart_RestartAfterSettingsImageAdded_SettingsWinsOverStalePersistedFileDefault(t *testing.T) {
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
	_ = os.WriteFile(filepath.Join(hcDir, "config.yaml"), []byte("harness: generic\nuser: scion\nimage: file-default:latest\n"), 0644)

	tplDir := filepath.Join(globalScionDir, "templates", "default")
	_ = os.MkdirAll(tplDir, 0755)
	_ = os.WriteFile(filepath.Join(tplDir, "scion-agent.json"), []byte(`{"default_harness_config": "test-harness"}`), 0644)

	settingsPath := filepath.Join(globalScionDir, "settings.yaml")
	noSettingsImage := []byte(`schema_version: "1"
active_profile: local
profiles:
  local:
    runtime: docker
`)
	withSettingsImage := []byte(`schema_version: "1"
active_profile: local
profiles:
  local:
    runtime: docker
harness_configs:
  test-harness:
    harness: generic
    image: settings-pinned:v1
`)
	_ = os.WriteFile(settingsPath, noSettingsImage, 0644)

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
	}

	mgr := NewManager(mockRT)
	startOpts := api.StartOptions{
		Name:        "test-agent",
		ProjectPath: projectScionDir,
		BrokerMode:  true,
		NoAuth:      true,
	}

	// First Start: no settings image yet, so the agent's persisted
	// scion-agent.json ends up with the harness-config file's default.
	if _, err := mgr.Start(context.Background(), startOpts); err != nil {
		t.Fatalf("first Start failed: %v", err)
	}
	if capturedConfig.Image != "file-default:latest" {
		t.Fatalf("precondition failed: first-provision image = %q, want the harness-config file default", capturedConfig.Image)
	}

	// An operator now pins harness_configs.test-harness.image in settings.
	_ = os.WriteFile(settingsPath, withSettingsImage, 0644)

	// Second Start (restart of the now-existing agent) must pick up the
	// settings image, not the stale persisted file default.
	if _, err := mgr.Start(context.Background(), startOpts); err != nil {
		t.Fatalf("restart Start failed: %v", err)
	}
	if capturedConfig.Image != "settings-pinned:v1" {
		t.Errorf("restart image = %q, want the newly-added settings image %q (not the stale persisted file default)",
			capturedConfig.Image, "settings-pinned:v1")
	}
}

// TestStart_RestartAfterSettingsPullPolicyRemoved_ClearsStalePersistedValue
// pins ptone/scion#2156: image_pull_policy must behave exactly like image on
// a restart — removing a Hub settings harness_configs.<h>.image_pull_policy
// must be honoured, not masked by the pull policy value ProvisionAgent
// persisted into scion-agent.json (via finalScionCfg.Kubernetes) at an
// earlier provision. Unlike image, RunConfig.Kubernetes is built by copying
// finalScionCfg.Kubernetes and overlaying resolvedPullPolicy, so this only
// holds if that overlay is unconditional.
func TestStart_RestartAfterSettingsPullPolicyRemoved_ClearsStalePersistedValue(t *testing.T) {
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
	_ = os.WriteFile(filepath.Join(hcDir, "config.yaml"), []byte("harness: generic\nuser: scion\nimage: file-default:latest\n"), 0644)

	tplDir := filepath.Join(globalScionDir, "templates", "default")
	_ = os.MkdirAll(tplDir, 0755)
	_ = os.WriteFile(filepath.Join(tplDir, "scion-agent.json"), []byte(`{"default_harness_config": "test-harness"}`), 0644)

	settingsPath := filepath.Join(globalScionDir, "settings.yaml")
	withPullPolicy := []byte(`schema_version: "1"
active_profile: local
profiles:
  local:
    runtime: docker
harness_configs:
  test-harness:
    harness: generic
    image_pull_policy: Always
`)
	withoutPullPolicy := []byte(`schema_version: "1"
active_profile: local
profiles:
  local:
    runtime: docker
harness_configs:
  test-harness:
    harness: generic
`)
	_ = os.WriteFile(settingsPath, withPullPolicy, 0644)

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
	}

	mgr := NewManager(mockRT)
	startOpts := api.StartOptions{
		Name:        "test-agent",
		ProjectPath: projectScionDir,
		BrokerMode:  true,
		NoAuth:      true,
	}

	if _, err := mgr.Start(context.Background(), startOpts); err != nil {
		t.Fatalf("create Start failed: %v", err)
	}
	gotPolicy := ""
	if capturedConfig.Kubernetes != nil {
		gotPolicy = capturedConfig.Kubernetes.ImagePullPolicy
	}
	if gotPolicy != "Always" {
		t.Fatalf("precondition failed: create-time imagePullPolicy = %q, want Always", gotPolicy)
	}

	// An operator removes harness_configs.test-harness.image_pull_policy.
	_ = os.WriteFile(settingsPath, withoutPullPolicy, 0644)

	if _, err := mgr.Start(context.Background(), startOpts); err != nil {
		t.Fatalf("restart Start failed: %v", err)
	}
	gotPolicy = ""
	if capturedConfig.Kubernetes != nil {
		gotPolicy = capturedConfig.Kubernetes.ImagePullPolicy
	}
	if gotPolicy != "" {
		t.Errorf("restart imagePullPolicy = %q, want empty (the settings value was removed, and there is no lower tier to fall back to)", gotPolicy)
	}
}

// TestStart_RestartAfterTemplatePullPolicyRemoved_FallsToSettings pins
// ptone/scion#2156: removing a template's kubernetes.imagePullPolicy pin
// (the template file still resolves; it just no longer sets that field)
// must let the settings tier apply on the next restart, not the pull policy
// ProvisionAgent persisted from the template's original pin.
func TestStart_RestartAfterTemplatePullPolicyRemoved_FallsToSettings(t *testing.T) {
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
	_ = os.WriteFile(filepath.Join(hcDir, "config.yaml"), []byte("harness: generic\nuser: scion\nimage: file-default:latest\n"), 0644)

	tplPath := filepath.Join(globalScionDir, "templates", "default", "scion-agent.json")
	_ = os.MkdirAll(filepath.Dir(tplPath), 0755)
	withTemplatePin := `{"default_harness_config": "test-harness", "kubernetes": {"imagePullPolicy": "Never"}}`
	withoutTemplatePin := `{"default_harness_config": "test-harness"}`
	_ = os.WriteFile(tplPath, []byte(withTemplatePin), 0644)

	_ = os.WriteFile(filepath.Join(globalScionDir, "settings.yaml"), []byte(`schema_version: "1"
active_profile: local
profiles:
  local:
    runtime: docker
harness_configs:
  test-harness:
    harness: generic
    image_pull_policy: Always
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
	}

	mgr := NewManager(mockRT)
	startOpts := api.StartOptions{
		Name:        "test-agent",
		ProjectPath: projectScionDir,
		BrokerMode:  true,
		NoAuth:      true,
	}

	if _, err := mgr.Start(context.Background(), startOpts); err != nil {
		t.Fatalf("create Start failed: %v", err)
	}
	gotPolicy := ""
	if capturedConfig.Kubernetes != nil {
		gotPolicy = capturedConfig.Kubernetes.ImagePullPolicy
	}
	if gotPolicy != "Never" {
		t.Fatalf("precondition failed: create-time imagePullPolicy = %q, want Never", gotPolicy)
	}

	// The template's pull-policy pin is removed (the template still exists
	// and still resolves; it just no longer sets kubernetes.imagePullPolicy).
	_ = os.WriteFile(tplPath, []byte(withoutTemplatePin), 0644)

	if _, err := mgr.Start(context.Background(), startOpts); err != nil {
		t.Fatalf("restart Start failed: %v", err)
	}
	gotPolicy = ""
	if capturedConfig.Kubernetes != nil {
		gotPolicy = capturedConfig.Kubernetes.ImagePullPolicy
	}
	if gotPolicy != "Always" {
		t.Errorf("restart imagePullPolicy = %q, want the settings value %q (not the stale removed template pin)", gotPolicy, "Always")
	}
}

// TestStart_SettingsImagePullPolicyReachesRunConfigKubernetes: a Hub
// settings harness_configs.<h>.image_pull_policy default reaches
// runtime.RunConfig.Kubernetes.ImagePullPolicy — the field the Kubernetes
// runtime's buildPod reads to set the pod's container ImagePullPolicy — and
// an explicit template kubernetes.imagePullPolicy still outranks it.
func TestStart_SettingsImagePullPolicyReachesRunConfigKubernetes(t *testing.T) {
	tests := []struct {
		name           string
		templateExtra  string
		wantPullPolicy string
	}{
		{
			name:           "settings default applies with no template override",
			wantPullPolicy: "Always",
		},
		{
			name:           "explicit template imagePullPolicy still outranks settings",
			templateExtra:  `, "kubernetes": {"imagePullPolicy": "Never"}`,
			wantPullPolicy: "Never",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
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
			_ = os.WriteFile(filepath.Join(hcDir, "config.yaml"), []byte("harness: generic\nuser: scion\nimage: file-default:latest\n"), 0644)

			tplDir := filepath.Join(globalScionDir, "templates", "default")
			_ = os.MkdirAll(tplDir, 0755)
			tplJSON := `{"default_harness_config": "test-harness"` + tt.templateExtra + `}`
			_ = os.WriteFile(filepath.Join(tplDir, "scion-agent.json"), []byte(tplJSON), 0644)

			_ = os.WriteFile(filepath.Join(globalScionDir, "settings.yaml"), []byte(`schema_version: "1"
active_profile: k8s
profiles:
  k8s:
    runtime: kubernetes
harness_configs:
  test-harness:
    harness: generic
    image_pull_policy: Always
runtimes:
  kubernetes:
    type: kubernetes
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
			}

			mgr := NewManager(mockRT)

			_, err := mgr.Start(context.Background(), api.StartOptions{
				Name:        "test-agent",
				ProjectPath: projectScionDir,
				BrokerMode:  true,
				NoAuth:      true,
			})
			if err != nil {
				t.Fatalf("Start failed: %v", err)
			}

			gotPolicy := ""
			if capturedConfig.Kubernetes != nil {
				gotPolicy = capturedConfig.Kubernetes.ImagePullPolicy
			}
			if gotPolicy != tt.wantPullPolicy {
				t.Errorf("RunConfig.Kubernetes.ImagePullPolicy = %q, want %q", gotPolicy, tt.wantPullPolicy)
			}
		})
	}
}

// TestStart_InlineConfigImageAndPullPolicyOutrankSettings: an inline config
// (opts.InlineConfig, e.g. a per-dispatch `--config` override) supplying an
// explicit image / kubernetes.imagePullPolicy must outrank a Hub settings
// value, on a first (fresh-provision) Start — the same rule a template
// override already has (ptone/scion#2156).
func TestStart_InlineConfigImageAndPullPolicyOutrankSettings(t *testing.T) {
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
	_ = os.WriteFile(filepath.Join(hcDir, "config.yaml"), []byte("harness: generic\nuser: scion\nimage: file-default:latest\n"), 0644)

	tplDir := filepath.Join(globalScionDir, "templates", "default")
	_ = os.MkdirAll(tplDir, 0755)
	_ = os.WriteFile(filepath.Join(tplDir, "scion-agent.json"), []byte(`{"default_harness_config": "test-harness"}`), 0644)

	_ = os.WriteFile(filepath.Join(globalScionDir, "settings.yaml"), []byte(`schema_version: "1"
active_profile: local
profiles:
  local:
    runtime: docker
harness_configs:
  test-harness:
    harness: generic
    image: settings-pinned:v1
    image_pull_policy: Always
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
	}

	mgr := NewManager(mockRT)

	_, err := mgr.Start(context.Background(), api.StartOptions{
		Name:        "test-agent",
		ProjectPath: projectScionDir,
		BrokerMode:  true,
		NoAuth:      true,
		InlineConfig: &api.ScionConfig{
			Image:      "inline-pinned:v3",
			Kubernetes: &api.KubernetesConfig{ImagePullPolicy: "Never"},
		},
	})
	if err != nil {
		t.Fatalf("Start failed: %v", err)
	}

	if capturedConfig.Image != "inline-pinned:v3" {
		t.Errorf("expected inline-config image to outrank settings, got %q", capturedConfig.Image)
	}
	gotPolicy := ""
	if capturedConfig.Kubernetes != nil {
		gotPolicy = capturedConfig.Kubernetes.ImagePullPolicy
	}
	if gotPolicy != "Never" {
		t.Errorf("expected inline-config imagePullPolicy to outrank settings, got %q", gotPolicy)
	}
}

// TestStart_RestartAfterInlineOnlyCreate_ExplicitInlineSurvivesWithoutLiveInlineConfig
// pins ptone/scion#2156: an agent created with an inline-config image/pull
// policy and no template pin has that explicit choice persisted to
// agent-info.json (AgentInfo.ExplicitImage / .ExplicitImagePullPolicy). A
// local restart with no --config (opts.InlineConfig nil, unlike a
// hub-dispatched restart, which resends AppliedConfig.InlineConfig) must
// still honour it — not silently fall back to a Hub settings value, which is
// a lower tier than an explicit override, live or persisted.
func TestStart_RestartAfterInlineOnlyCreate_ExplicitInlineSurvivesWithoutLiveInlineConfig(t *testing.T) {
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
	_ = os.WriteFile(filepath.Join(hcDir, "config.yaml"), []byte("harness: generic\nuser: scion\nimage: file-default:latest\n"), 0644)

	tplDir := filepath.Join(globalScionDir, "templates", "default")
	_ = os.MkdirAll(tplDir, 0755)
	_ = os.WriteFile(filepath.Join(tplDir, "scion-agent.json"), []byte(`{"default_harness_config": "test-harness"}`), 0644)

	_ = os.WriteFile(filepath.Join(globalScionDir, "settings.yaml"), []byte(`schema_version: "1"
active_profile: local
profiles:
  local:
    runtime: docker
harness_configs:
  test-harness:
    harness: generic
    image: settings-pinned:v1
    image_pull_policy: Always
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
	}

	mgr := NewManager(mockRT)

	// Create with an explicit inline image and pull policy; no template pin.
	_, err := mgr.Start(context.Background(), api.StartOptions{
		Name:        "test-agent",
		ProjectPath: projectScionDir,
		BrokerMode:  true,
		NoAuth:      true,
		InlineConfig: &api.ScionConfig{
			Image:      "inline-pinned:v3",
			Kubernetes: &api.KubernetesConfig{ImagePullPolicy: "Never"},
		},
	})
	if err != nil {
		t.Fatalf("create Start failed: %v", err)
	}
	if capturedConfig.Image != "inline-pinned:v3" {
		t.Fatalf("precondition failed: create-time image = %q, want inline-pinned:v3", capturedConfig.Image)
	}

	// Restart with no --config: opts.InlineConfig is nil, as on a plain local
	// `scion start <existing-agent>`.
	_, err = mgr.Start(context.Background(), api.StartOptions{
		Name:        "test-agent",
		ProjectPath: projectScionDir,
		BrokerMode:  true,
		NoAuth:      true,
	})
	if err != nil {
		t.Fatalf("restart Start failed: %v", err)
	}

	if capturedConfig.Image != "inline-pinned:v3" {
		t.Errorf("restart image = %q, want the create-time explicit inline image %q (not the settings value)",
			capturedConfig.Image, "inline-pinned:v3")
	}
	gotPolicy := ""
	if capturedConfig.Kubernetes != nil {
		gotPolicy = capturedConfig.Kubernetes.ImagePullPolicy
	}
	if gotPolicy != "Never" {
		t.Errorf("restart imagePullPolicy = %q, want the create-time explicit inline value %q (not the settings value)",
			gotPolicy, "Never")
	}
}

// TestStart_RestartWithHarnessAuthOnly_ExplicitInlineImageStillSurvives pins
// ptone/scion#2156: a restart request can make opts.InlineConfig-derived
// startInlineConfig non-nil for reasons that have nothing to do with image —
// --harness-auth alone does this — and that must not defeat the persisted
// create-time inline image/pull-policy fallback. The fallback is keyed on
// whether the CURRENT request's inline config sets that specific field, not
// on whether some inline config object exists at all.
func TestStart_RestartWithHarnessAuthOnly_ExplicitInlineImageStillSurvives(t *testing.T) {
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
	_ = os.WriteFile(filepath.Join(hcDir, "config.yaml"), []byte("harness: generic\nuser: scion\nimage: file-default:latest\n"), 0644)

	tplDir := filepath.Join(globalScionDir, "templates", "default")
	_ = os.MkdirAll(tplDir, 0755)
	_ = os.WriteFile(filepath.Join(tplDir, "scion-agent.json"), []byte(`{"default_harness_config": "test-harness"}`), 0644)

	_ = os.WriteFile(filepath.Join(globalScionDir, "settings.yaml"), []byte(`schema_version: "1"
active_profile: local
profiles:
  local:
    runtime: docker
harness_configs:
  test-harness:
    harness: generic
    image: settings-pinned:v1
    image_pull_policy: Always
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
	}

	mgr := NewManager(mockRT)

	// Create with an explicit inline image and pull policy; no template pin.
	_, err := mgr.Start(context.Background(), api.StartOptions{
		Name:        "test-agent",
		ProjectPath: projectScionDir,
		BrokerMode:  true,
		NoAuth:      true,
		InlineConfig: &api.ScionConfig{
			Image:      "inline-pinned:v3",
			Kubernetes: &api.KubernetesConfig{ImagePullPolicy: "Never"},
		},
	})
	if err != nil {
		t.Fatalf("create Start failed: %v", err)
	}
	if capturedConfig.Image != "inline-pinned:v3" {
		t.Fatalf("precondition failed: create-time image = %q, want inline-pinned:v3", capturedConfig.Image)
	}

	// Restart with --harness-auth only: opts.HarnessAuth makes run.go's
	// startInlineConfig non-nil (used for the GetAgent merge), but
	// opts.InlineConfig itself is nil — the create-time inline image/pull
	// policy must still apply, not the settings values.
	_, err = mgr.Start(context.Background(), api.StartOptions{
		Name:        "test-agent",
		ProjectPath: projectScionDir,
		BrokerMode:  true,
		NoAuth:      true,
		HarnessAuth: "api-key",
	})
	if err != nil {
		t.Fatalf("restart Start failed: %v", err)
	}

	if capturedConfig.Image != "inline-pinned:v3" {
		t.Errorf("restart (--harness-auth only) image = %q, want the create-time explicit inline image %q",
			capturedConfig.Image, "inline-pinned:v3")
	}
	gotPolicy := ""
	if capturedConfig.Kubernetes != nil {
		gotPolicy = capturedConfig.Kubernetes.ImagePullPolicy
	}
	if gotPolicy != "Never" {
		t.Errorf("restart (--harness-auth only) imagePullPolicy = %q, want the create-time explicit inline value %q",
			gotPolicy, "Never")
	}
}

// TestStart_RestartWithUnrelatedInlineConfigField_ExplicitInlineSurvivesPerField
// pins ptone/scion#2156: the explicit tier's fallback to the recorded
// create-time inline value is keyed per field (does THIS request's inline
// config set THIS field), not on whether any inline config object is
// present at all. A restart whose --config sets only an unrelated field
// (e.g. --model) must not drop the create-time inline image/pull-policy
// pin, and a restart whose --config sets only the image must still fall
// back to the recorded pull policy independently (field independence).
func TestStart_RestartWithUnrelatedInlineConfigField_ExplicitInlineSurvivesPerField(t *testing.T) {
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
	_ = os.WriteFile(filepath.Join(hcDir, "config.yaml"), []byte("harness: generic\nuser: scion\nimage: file-default:latest\n"), 0644)

	tplDir := filepath.Join(globalScionDir, "templates", "default")
	_ = os.MkdirAll(tplDir, 0755)
	_ = os.WriteFile(filepath.Join(tplDir, "scion-agent.json"), []byte(`{"default_harness_config": "test-harness"}`), 0644)

	_ = os.WriteFile(filepath.Join(globalScionDir, "settings.yaml"), []byte(`schema_version: "1"
active_profile: local
profiles:
  local:
    runtime: docker
harness_configs:
  test-harness:
    harness: generic
    image: settings-pinned:v1
    image_pull_policy: Always
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
	}

	mgr := NewManager(mockRT)

	// Create with an explicit inline image and pull policy; no template pin.
	_, err := mgr.Start(context.Background(), api.StartOptions{
		Name:        "test-agent",
		ProjectPath: projectScionDir,
		BrokerMode:  true,
		NoAuth:      true,
		InlineConfig: &api.ScionConfig{
			Image:      "inline-pinned:v3",
			Kubernetes: &api.KubernetesConfig{ImagePullPolicy: "Never"},
		},
	})
	if err != nil {
		t.Fatalf("create Start failed: %v", err)
	}
	if capturedConfig.Image != "inline-pinned:v3" {
		t.Fatalf("precondition failed: create-time image = %q, want inline-pinned:v3", capturedConfig.Image)
	}

	t.Run("restart with only an unrelated inline field (--model) keeps both recorded values", func(t *testing.T) {
		_, err := mgr.Start(context.Background(), api.StartOptions{
			Name:         "test-agent",
			ProjectPath:  projectScionDir,
			BrokerMode:   true,
			NoAuth:       true,
			InlineConfig: &api.ScionConfig{Model: "some-model"},
		})
		if err != nil {
			t.Fatalf("restart Start failed: %v", err)
		}
		if capturedConfig.Image != "inline-pinned:v3" {
			t.Errorf("image = %q, want the recorded inline image %q", capturedConfig.Image, "inline-pinned:v3")
		}
		gotPolicy := ""
		if capturedConfig.Kubernetes != nil {
			gotPolicy = capturedConfig.Kubernetes.ImagePullPolicy
		}
		if gotPolicy != "Never" {
			t.Errorf("imagePullPolicy = %q, want the recorded inline value %q", gotPolicy, "Never")
		}
	})

	t.Run("restart with only inline image set falls back to the recorded pull policy independently", func(t *testing.T) {
		_, err := mgr.Start(context.Background(), api.StartOptions{
			Name:         "test-agent",
			ProjectPath:  projectScionDir,
			BrokerMode:   true,
			NoAuth:       true,
			InlineConfig: &api.ScionConfig{Image: "other:v9"},
		})
		if err != nil {
			t.Fatalf("restart Start failed: %v", err)
		}
		if capturedConfig.Image != "other:v9" {
			t.Errorf("image = %q, want the current request's inline image %q", capturedConfig.Image, "other:v9")
		}
		gotPolicy := ""
		if capturedConfig.Kubernetes != nil {
			gotPolicy = capturedConfig.Kubernetes.ImagePullPolicy
		}
		if gotPolicy != "Never" {
			t.Errorf("imagePullPolicy = %q, want the recorded inline value %q (image and pull policy fall back independently)",
				gotPolicy, "Never")
		}
	})
}

// TestStart_RestartAfterTemplateImageEdited_LiveTemplateWinsOverCreateTimeSnapshot
// pins ptone/scion#2156: the value ProvisionAgent persists to
// agent-info.json for the explicit tier's restart fallback must be the
// INLINE config's contribution only, never the template's — a template is
// re-read live on every Start, so persisting its create-time value would
// let a stale template snapshot outrank the CURRENT template on every later
// restart. Here the agent is created with only a template image (no inline
// config at all), the template file is then edited, and a restart must
// pick up the new template value.
func TestStart_RestartAfterTemplateImageEdited_LiveTemplateWinsOverCreateTimeSnapshot(t *testing.T) {
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
	_ = os.WriteFile(filepath.Join(hcDir, "config.yaml"), []byte("harness: generic\nuser: scion\nimage: file-default:latest\n"), 0644)

	tplPath := filepath.Join(globalScionDir, "templates", "default", "scion-agent.json")
	_ = os.MkdirAll(filepath.Dir(tplPath), 0755)
	_ = os.WriteFile(tplPath, []byte(`{"default_harness_config": "test-harness", "image": "template-pinned:v1"}`), 0644)

	_ = os.WriteFile(filepath.Join(globalScionDir, "settings.yaml"), []byte(`schema_version: "1"
active_profile: local
profiles:
  local:
    runtime: docker
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
	}

	mgr := NewManager(mockRT)

	// Create: template pins v1, no inline config at all.
	_, err := mgr.Start(context.Background(), api.StartOptions{
		Name:        "test-agent",
		ProjectPath: projectScionDir,
		BrokerMode:  true,
		NoAuth:      true,
	})
	if err != nil {
		t.Fatalf("create Start failed: %v", err)
	}
	if capturedConfig.Image != "template-pinned:v1" {
		t.Fatalf("precondition failed: create-time image = %q, want template-pinned:v1", capturedConfig.Image)
	}

	// Edit the template to pin v2, then restart with no --config.
	_ = os.WriteFile(tplPath, []byte(`{"default_harness_config": "test-harness", "image": "template-pinned:v2"}`), 0644)

	_, err = mgr.Start(context.Background(), api.StartOptions{
		Name:        "test-agent",
		ProjectPath: projectScionDir,
		BrokerMode:  true,
		NoAuth:      true,
	})
	if err != nil {
		t.Fatalf("restart Start failed: %v", err)
	}

	if capturedConfig.Image != "template-pinned:v2" {
		t.Errorf("restart image = %q, want the current template's image %q (not a create-time snapshot)",
			capturedConfig.Image, "template-pinned:v2")
	}
}

// TestStart_RestartAfterTemplateDeleted_FallsBackToRecordedImage pins
// ptone/scion#2156: if an agent's template can no longer be resolved at all
// on a local restart (renamed or deleted since the agent was created — not
// merely "no image pin"), the restart must still run the image/pull-policy
// recorded at an earlier provision, matching pre-existing (origin/main)
// behaviour, rather than silently falling through to Hub settings or the
// file default just because the template disappeared.
func TestStart_RestartAfterTemplateDeleted_FallsBackToRecordedImage(t *testing.T) {
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
	_ = os.WriteFile(filepath.Join(hcDir, "config.yaml"), []byte("harness: generic\nuser: scion\nimage: file-default:latest\n"), 0644)

	tplDir := filepath.Join(globalScionDir, "templates", "custom")
	_ = os.MkdirAll(tplDir, 0755)
	_ = os.WriteFile(filepath.Join(tplDir, "scion-agent.json"), []byte(`{"default_harness_config": "test-harness", "image": "tpl:v1", "kubernetes": {"imagePullPolicy": "Never"}}`), 0644)

	_ = os.WriteFile(filepath.Join(globalScionDir, "settings.yaml"), []byte(`schema_version: "1"
active_profile: local
profiles:
  local:
    runtime: docker
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
	}

	mgr := NewManager(mockRT)

	// Create from the "custom" template: image tpl:v1, pull policy Never.
	_, err := mgr.Start(context.Background(), api.StartOptions{
		Name:        "test-agent",
		Template:    "custom",
		ProjectPath: projectScionDir,
		BrokerMode:  true,
		NoAuth:      true,
	})
	if err != nil {
		t.Fatalf("create Start failed: %v", err)
	}
	if capturedConfig.Image != "tpl:v1" {
		t.Fatalf("precondition failed: create-time image = %q, want tpl:v1", capturedConfig.Image)
	}

	// Delete the template entirely — renamed or removed since creation.
	if err := os.RemoveAll(tplDir); err != nil {
		t.Fatalf("failed to delete template dir: %v", err)
	}

	agentInfo, err := mgr.Start(context.Background(), api.StartOptions{
		Name:        "test-agent",
		ProjectPath: projectScionDir,
		BrokerMode:  true,
		NoAuth:      true,
	})
	if err != nil {
		t.Fatalf("restart Start failed: %v", err)
	}

	if capturedConfig.Image != "tpl:v1" {
		t.Errorf("restart image = %q, want the recorded image %q (matching origin/main's behaviour when the template is gone)",
			capturedConfig.Image, "tpl:v1")
	}
	gotPolicy := ""
	if capturedConfig.Kubernetes != nil {
		gotPolicy = capturedConfig.Kubernetes.ImagePullPolicy
	}
	if gotPolicy != "Never" {
		t.Errorf("restart imagePullPolicy = %q, want the recorded value %q", gotPolicy, "Never")
	}
	if agentInfo == nil || !strings.Contains(strings.Join(agentInfo.Warnings, "\n"), "could not be found") {
		t.Errorf("expected a warning about the unresolvable template, got warnings=%v", agentInfoWarningsOrNil(agentInfo))
	}

	// A live inline config must still outrank the unresolvable-template
	// snapshot — the same as it outranks a live template — for image and
	// pull policy independently. This is the ordering half of the
	// unresolvable-template fallback: the snapshot is applied BEFORE the
	// inline tier, not after, so it never gets a chance to override a
	// current request's own inline values.
	agentInfo, err = mgr.Start(context.Background(), api.StartOptions{
		Name:        "test-agent",
		ProjectPath: projectScionDir,
		BrokerMode:  true,
		NoAuth:      true,
		InlineConfig: &api.ScionConfig{
			Image:      "inline:v2",
			Kubernetes: &api.KubernetesConfig{ImagePullPolicy: "IfNotPresent"},
		},
	})
	if err != nil {
		t.Fatalf("third Start (with inline config) failed: %v", err)
	}
	if capturedConfig.Image != "inline:v2" {
		t.Errorf("image with a live inline config = %q, want the inline value %q (inline must outrank the unresolvable-template snapshot)",
			capturedConfig.Image, "inline:v2")
	}
	gotPolicy = ""
	if capturedConfig.Kubernetes != nil {
		gotPolicy = capturedConfig.Kubernetes.ImagePullPolicy
	}
	if gotPolicy != "IfNotPresent" {
		t.Errorf("imagePullPolicy with a live inline config = %q, want the inline value %q", gotPolicy, "IfNotPresent")
	}
	// Both fields came from the live inline config, not the snapshot, so the
	// "recorded at an earlier provision" warning must not appear.
	if agentInfo != nil && strings.Contains(strings.Join(agentInfo.Warnings, "\n"), "recorded at an earlier provision") {
		t.Errorf("did not expect a 'recorded at an earlier provision' warning when inline overrides both fields, got warnings=%v", agentInfo.Warnings)
	}

	// A dispatch --image (opts.Image) overrides only image, never pull
	// policy — there is no equivalent per-dispatch pull-policy flag. The
	// warning must then name only "pull policy" (the field the snapshot
	// still supplies), not "image and pull policy" (image came from
	// opts.Image, not the snapshot).
	agentInfo, err = mgr.Start(context.Background(), api.StartOptions{
		Name:        "test-agent",
		ProjectPath: projectScionDir,
		BrokerMode:  true,
		NoAuth:      true,
		Image:       "dispatch:v9",
	})
	if err != nil {
		t.Fatalf("fourth Start (with dispatch --image) failed: %v", err)
	}
	if capturedConfig.Image != "dispatch:v9" {
		t.Errorf("image with dispatch --image = %q, want the dispatch value %q", capturedConfig.Image, "dispatch:v9")
	}
	gotPolicy = ""
	if capturedConfig.Kubernetes != nil {
		gotPolicy = capturedConfig.Kubernetes.ImagePullPolicy
	}
	if gotPolicy != "Never" {
		t.Errorf("imagePullPolicy with dispatch --image = %q, want the recorded snapshot value %q", gotPolicy, "Never")
	}
	if agentInfo == nil {
		t.Fatal("expected a non-nil AgentInfo")
	}
	joinedWarnings := strings.Join(agentInfo.Warnings, "\n")
	if !strings.Contains(joinedWarnings, "using the pull policy recorded") {
		t.Errorf("expected a warning naming only the pull policy as recorded, got warnings=%v", agentInfo.Warnings)
	}
	if strings.Contains(joinedWarnings, "image and pull policy") || strings.Contains(joinedWarnings, "using the image") {
		t.Errorf("did not expect the warning to name image (it came from dispatch --image, not the snapshot), got warnings=%v", agentInfo.Warnings)
	}
}

// agentInfoWarningsOrNil is a small helper so the warning-mismatch error
// message above doesn't panic on a nil agentInfo.
func agentInfoWarningsOrNil(info *api.AgentInfo) []string {
	if info == nil {
		return nil
	}
	return info.Warnings
}

// TestStart_RestartWithNoRecordedTemplate_NotTreatedAsUnresolvable pins the
// guard half of the unresolvable-template fallback: an EMPTY template chain
// must not be conflated with a template that failed to resolve. An agent
// whose Info.Template is empty (no template name to look up at all) has no
// template tier to fall back FROM in the first place — treating that the
// same as "the named template is gone" would pin whatever settings/file
// image happened to be baked into scion-agent.json at creation forever,
// which is the precedence bug ptone/scion#2156 exists to fix. Settings
// changed after creation must still apply on restart, and no "could not be
// found" warning should appear.
func TestStart_RestartWithNoRecordedTemplate_NotTreatedAsUnresolvable(t *testing.T) {
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
	_ = os.WriteFile(filepath.Join(hcDir, "config.yaml"), []byte("harness: generic\nuser: scion\nimage: file-default:latest\n"), 0644)

	tplDir := filepath.Join(globalScionDir, "templates", "default")
	_ = os.MkdirAll(tplDir, 0755)
	_ = os.WriteFile(filepath.Join(tplDir, "scion-agent.json"), []byte(`{"default_harness_config": "test-harness"}`), 0644)

	settingsPath := filepath.Join(globalScionDir, "settings.yaml")
	settingsV1 := []byte(`schema_version: "1"
active_profile: local
profiles:
  local:
    runtime: docker
harness_configs:
  test-harness:
    harness: generic
    image: settings-v1:latest
`)
	settingsV2 := []byte(`schema_version: "1"
active_profile: local
profiles:
  local:
    runtime: docker
harness_configs:
  test-harness:
    harness: generic
    image: settings-v2:latest
`)
	_ = os.WriteFile(settingsPath, settingsV1, 0644)

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
	}

	mgr := NewManager(mockRT)

	if _, err := mgr.Start(context.Background(), api.StartOptions{
		Name:        "test-agent",
		ProjectPath: projectScionDir,
		BrokerMode:  true,
		NoAuth:      true,
	}); err != nil {
		t.Fatalf("create Start failed: %v", err)
	}
	if capturedConfig.Image != "settings-v1:latest" {
		t.Fatalf("precondition failed: create-time image = %q, want settings-v1:latest", capturedConfig.Image)
	}

	// Simulate an agent with no recorded template name at all (e.g. an
	// agent predating template tracking, or one whose template was always
	// blank) — empty this agent's Info.Template directly on disk.
	agentInfoPath := filepath.Join(config.GetAgentHomePath(projectScionDir, "test-agent"), "agent-info.json")
	infoData, err := os.ReadFile(agentInfoPath)
	if err != nil {
		t.Fatalf("failed to read agent-info.json: %v", err)
	}
	var info api.AgentInfo
	if err := json.Unmarshal(infoData, &info); err != nil {
		t.Fatalf("failed to unmarshal agent-info.json: %v", err)
	}
	info.Template = ""
	updated, err := json.Marshal(&info)
	if err != nil {
		t.Fatalf("failed to re-marshal agent-info.json: %v", err)
	}
	if err := os.WriteFile(agentInfoPath, updated, 0644); err != nil {
		t.Fatalf("failed to write agent-info.json: %v", err)
	}

	// An operator changes the settings image.
	_ = os.WriteFile(settingsPath, settingsV2, 0644)

	agentInfo, err := mgr.Start(context.Background(), api.StartOptions{
		Name:        "test-agent",
		ProjectPath: projectScionDir,
		BrokerMode:  true,
		NoAuth:      true,
	})
	if err != nil {
		t.Fatalf("restart Start failed: %v", err)
	}

	if capturedConfig.Image != "settings-v2:latest" {
		t.Errorf("restart image = %q, want the new settings image %q (an empty recorded template must not pin the old settings-baked image)",
			capturedConfig.Image, "settings-v2:latest")
	}
	if agentInfo != nil && strings.Contains(strings.Join(agentInfo.Warnings, "\n"), "could not be found") {
		t.Errorf("did not expect an unresolvable-template warning when there was never a template to resolve, got warnings=%v", agentInfo.Warnings)
	}
}

// TestStart_ProfileOverrideImageAndPullPolicy: a profile's
// harness_overrides.<h> image/pull policy must outrank the base
// harness_configs.<h> entry when Start resolves the settings tier
// (ptone/scion#2156; ProvisionAgent's table test covers the provision-time
// path, this pins it at Start too).
func TestStart_ProfileOverrideImageAndPullPolicy(t *testing.T) {
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
	_ = os.WriteFile(filepath.Join(hcDir, "config.yaml"), []byte("harness: generic\nuser: scion\nimage: file-default:latest\n"), 0644)

	tplDir := filepath.Join(globalScionDir, "templates", "default")
	_ = os.MkdirAll(tplDir, 0755)
	_ = os.WriteFile(filepath.Join(tplDir, "scion-agent.json"), []byte(`{"default_harness_config": "test-harness"}`), 0644)

	_ = os.WriteFile(filepath.Join(globalScionDir, "settings.yaml"), []byte(`schema_version: "1"
active_profile: staging
profiles:
  staging:
    runtime: docker
    harness_overrides:
      test-harness:
        image: profile-pinned:v4
        image_pull_policy: Never
harness_configs:
  test-harness:
    harness: generic
    image: settings-pinned:v1
    image_pull_policy: Always
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
	}

	mgr := NewManager(mockRT)

	_, err := mgr.Start(context.Background(), api.StartOptions{
		Name:        "test-agent",
		ProjectPath: projectScionDir,
		Profile:     "staging",
		BrokerMode:  true,
		NoAuth:      true,
	})
	if err != nil {
		t.Fatalf("Start failed: %v", err)
	}

	if capturedConfig.Image != "profile-pinned:v4" {
		t.Errorf("expected profile harness_overrides image to outrank the base settings entry, got %q", capturedConfig.Image)
	}
	gotPolicy := ""
	if capturedConfig.Kubernetes != nil {
		gotPolicy = capturedConfig.Kubernetes.ImagePullPolicy
	}
	if gotPolicy != "Never" {
		t.Errorf("expected profile harness_overrides imagePullPolicy to outrank the base settings entry, got %q", gotPolicy)
	}
}

// TestStart_RestartWithNoProfile_ResolvesSettingsAgainstCreatedWithProfile
// pins ptone/scion#2156: a local restart with no --profile must resolve the
// Hub-settings tier against the profile the agent was actually CREATED with
// (finalScionCfg.Info.Profile), not whatever settings.active_profile happens
// to be — matching the broker's own restart-dispatch behavior
// (agent.GetSavedProfile). The agent here is created under "staging" (whose
// harness_overrides pins an image) while active_profile is a different
// profile with no such override; restarting with Profile: "" must still
// resolve to the staging image, not fall through to active_profile's.
func TestStart_RestartWithNoProfile_ResolvesSettingsAgainstCreatedWithProfile(t *testing.T) {
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
	_ = os.WriteFile(filepath.Join(hcDir, "config.yaml"), []byte("harness: generic\nuser: scion\nimage: file-default:latest\n"), 0644)

	tplDir := filepath.Join(globalScionDir, "templates", "default")
	_ = os.MkdirAll(tplDir, 0755)
	_ = os.WriteFile(filepath.Join(tplDir, "scion-agent.json"), []byte(`{"default_harness_config": "test-harness"}`), 0644)

	// active_profile is "local", which has no harness_overrides for
	// test-harness; "staging" (the created-with profile) pins one.
	_ = os.WriteFile(filepath.Join(globalScionDir, "settings.yaml"), []byte(`schema_version: "1"
active_profile: local
profiles:
  local:
    runtime: docker
  staging:
    runtime: docker
    harness_overrides:
      test-harness:
        image: staging-pinned:v5
harness_configs:
  test-harness:
    harness: generic
    image: settings-pinned:v1
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
	}

	mgr := NewManager(mockRT)

	// Create under the "staging" profile.
	_, err := mgr.Start(context.Background(), api.StartOptions{
		Name:        "test-agent",
		ProjectPath: projectScionDir,
		Profile:     "staging",
		BrokerMode:  true,
		NoAuth:      true,
	})
	if err != nil {
		t.Fatalf("create Start failed: %v", err)
	}
	if capturedConfig.Image != "staging-pinned:v5" {
		t.Fatalf("precondition failed: create-time image = %q, want staging-pinned:v5", capturedConfig.Image)
	}

	// Restart with no --profile at all.
	_, err = mgr.Start(context.Background(), api.StartOptions{
		Name:        "test-agent",
		ProjectPath: projectScionDir,
		BrokerMode:  true,
		NoAuth:      true,
	})
	if err != nil {
		t.Fatalf("restart Start failed: %v", err)
	}

	if capturedConfig.Image != "staging-pinned:v5" {
		t.Errorf("restart (no --profile) image = %q, want the created-with profile's image %q (not active_profile's)",
			capturedConfig.Image, "staging-pinned:v5")
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
	// The built-in fallback cases derive their expectations from the
	// embedded claude alias table so routine model bumps in
	// harnesses/claude/config.yaml do not break this test. The guards
	// ensure each alias really resolves to a concrete model, so the
	// fallback assertions cannot pass vacuously.
	builtin := harness.DefaultModelAliases("claude")
	for _, alias := range []string{"large", "medium"} {
		if v := builtin[alias]; v == "" || v == alias {
			t.Fatalf("built-in claude alias %q = %q, want a concrete model", alias, v)
		}
	}

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
			wantModel:   builtin["large"],
			wantResolv:  true,
		},
		{
			name:        "nil cfg falls back to built-in table when harness is known",
			envModel:    "medium",
			cfg:         nil,
			harnessName: "claude",
			wantModel:   builtin["medium"],
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
