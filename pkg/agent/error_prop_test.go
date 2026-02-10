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

	"github.com/ptone/scion-agent/pkg/api"
	"github.com/ptone/scion-agent/pkg/runtime"
)

func TestStart_ErrorPropagation_Tmux(t *testing.T) {
	// Create a temporary project directory
	tmpDir, err := os.MkdirTemp("", "scion-test-project")
	if err != nil {
		t.Fatalf("failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tmpDir)

	// Mock HOME for global settings and templates
	originalHome := os.Getenv("HOME")
	defer os.Setenv("HOME", originalHome)
	os.Setenv("HOME", tmpDir)

	// Setup global templates (needed by GetAgent)
	globalScionDir := filepath.Join(tmpDir, ".scion")
	globalTemplatesDir := filepath.Join(globalScionDir, "templates")
	if err := os.MkdirAll(globalTemplatesDir, 0755); err != nil {
		t.Fatalf("failed to create global templates dir: %v", err)
	}

	// Create a dummy "gemini" template
	tplDir := filepath.Join(globalTemplatesDir, "gemini")
	if err := os.MkdirAll(tplDir, 0755); err != nil {
		t.Fatalf("failed to create gemini template dir: %v", err)
	}
	tplConfig := `{"harness": "generic"}`
	if err := os.WriteFile(filepath.Join(tplDir, "scion-agent.json"), []byte(tplConfig), 0644); err != nil {
		t.Fatalf("failed to write template config: %v", err)
	}

	// Create .scion/settings.json with tmux enabled for "mock" runtime
	// We put this in the project dir
	projectDir := filepath.Join(tmpDir, "project")
	projectScionDir := filepath.Join(projectDir, ".scion")
	if err := os.MkdirAll(projectScionDir, 0755); err != nil {
		t.Fatalf("failed to create project .scion dir: %v", err)
	}

	settingsJSON := `
{
  "runtimes": {
    "mock": {
      "tmux": true
    }
  },
  "profiles": {
    "test": {
      "runtime": "mock"
    }
  }
}
`
	if err := os.WriteFile(filepath.Join(projectScionDir, "settings.json"), []byte(settingsJSON), 0644); err != nil {
		t.Fatalf("failed to write settings.json: %v", err)
	}

	// Create empty prompt.md to satisfy Start checks if no task provided (though we provide one)
	// We also need to init the project properly or just make sure directories exist
	if err := os.MkdirAll(projectDir, 0755); err != nil {
		t.Fatal(err)
	}

	// Setup MockRuntime
	originalErr := fmt.Errorf("container run failed: some random file: no such file or directory")
	mockRuntime := &runtime.MockRuntime{
		RunFunc: func(ctx context.Context, config runtime.RunConfig) (string, error) {
			return "", originalErr
		},
		ListFunc: func(ctx context.Context, labelFilter map[string]string) ([]api.AgentInfo, error) {
			return nil, nil
		},
		ImageExistsFunc: func(ctx context.Context, image string) (bool, error) {
			return true, nil
		},
	}

	manager := &AgentManager{
		Runtime: mockRuntime,
	}

	// Run Start
	opts := api.StartOptions{
		Name:      "test-agent",
		GrovePath: projectScionDir,
		Profile:   "test",
		Task:      "do something",
		Template:  "gemini",
	}

	_, err = manager.Start(context.Background(), opts)
	if err == nil {
		t.Fatal("expected error, got nil")
	}

	// Verify the error message
	// Correct behavior: should NOT wrap in "failed to launch container with tmux..." if the error is "no such file or directory"
	
	unexpectedPart := "failed to launch container with tmux: tmux binary not found"
	if strings.Contains(err.Error(), unexpectedPart) {
		t.Errorf("Error should NOT contain '%s', but got: %v", unexpectedPart, err)
	}

	expectedPart := "container run failed: some random file: no such file or directory"
	if !strings.Contains(err.Error(), expectedPart) {
		t.Errorf("Expected error to contain '%s', but got: %v", expectedPart, err)
	}
}

func TestStart_ErrorPropagation_Tmux_Missing(t *testing.T) {
	// Create a temporary project directory
	tmpDir, err := os.MkdirTemp("", "scion-test-project-missing")
	if err != nil {
		t.Fatalf("failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tmpDir)

	// Mock HOME for global settings and templates
	originalHome := os.Getenv("HOME")
	defer os.Setenv("HOME", originalHome)
	os.Setenv("HOME", tmpDir)

	// Setup global templates (needed by GetAgent)
	globalScionDir := filepath.Join(tmpDir, ".scion")
	globalTemplatesDir := filepath.Join(globalScionDir, "templates")
	if err := os.MkdirAll(globalTemplatesDir, 0755); err != nil {
		t.Fatalf("failed to create global templates dir: %v", err)
	}

	// Create a dummy "gemini" template
	tplDir := filepath.Join(globalTemplatesDir, "gemini")
	if err := os.MkdirAll(tplDir, 0755); err != nil {
		t.Fatalf("failed to create gemini template dir: %v", err)
	}
	tplConfig := `{"harness": "generic"}`
	if err := os.WriteFile(filepath.Join(tplDir, "scion-agent.json"), []byte(tplConfig), 0644); err != nil {
		t.Fatalf("failed to write template config: %v", err)
	}

	// Create .scion/settings.json with tmux enabled for "mock" runtime
	projectDir := filepath.Join(tmpDir, "project")
	projectScionDir := filepath.Join(projectDir, ".scion")
	if err := os.MkdirAll(projectScionDir, 0755); err != nil {
		t.Fatalf("failed to create project .scion dir: %v", err)
	}

	settingsJSON := `
{
  "runtimes": {
    "mock": {
      "tmux": true
    }
  },
  "profiles": {
    "test": {
      "runtime": "mock"
    }
  }
}
`
	if err := os.WriteFile(filepath.Join(projectScionDir, "settings.json"), []byte(settingsJSON), 0644); err != nil {
		t.Fatalf("failed to write settings.json: %v", err)
	}

	if err := os.MkdirAll(projectDir, 0755); err != nil {
		t.Fatal(err)
	}

	// Setup MockRuntime with "executable file not found" error
	originalErr := fmt.Errorf("container run failed: executable file not found in $PATH")
	mockRuntime := &runtime.MockRuntime{
		RunFunc: func(ctx context.Context, config runtime.RunConfig) (string, error) {
			return "", originalErr
		},
		ListFunc: func(ctx context.Context, labelFilter map[string]string) ([]api.AgentInfo, error) {
			return nil, nil
		},
		ImageExistsFunc: func(ctx context.Context, image string) (bool, error) {
			return true, nil
		},
	}

	manager := &AgentManager{
		Runtime: mockRuntime,
	}

	// Run Start
	opts := api.StartOptions{
		Name:      "test-agent",
		GrovePath: projectScionDir,
		Profile:   "test",
		Task:      "do something",
		Template:  "gemini",
	}

	_, err = manager.Start(context.Background(), opts)
	if err == nil {
		t.Fatal("expected error, got nil")
	}

	// Verify the error message
	// Should wrap in "failed to launch container with tmux: tmux binary not found..."
	expectedPart := "failed to launch container with tmux: tmux binary not found"
	if !strings.Contains(err.Error(), expectedPart) {
		t.Errorf("Expected error to contain '%s', but got: %v", expectedPart, err)
	}
}

func TestStart_ErrorPropagation_FalsePositive_Tmux(t *testing.T) {
	// Create a temporary project directory
	tmpDir, err := os.MkdirTemp("", "scion-repro-test")
	if err != nil {
		t.Fatalf("failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tmpDir)

	// Mock HOME
	originalHome := os.Getenv("HOME")
	defer os.Setenv("HOME", originalHome)
	os.Setenv("HOME", tmpDir)

	// Setup global templates
	globalScionDir := filepath.Join(tmpDir, ".scion")
	globalTemplatesDir := filepath.Join(globalScionDir, "templates")
	if err := os.MkdirAll(globalTemplatesDir, 0755); err != nil {
		t.Fatalf("failed to create global templates dir: %v", err)
	}

	// Create a dummy "gemini" template
	tplDir := filepath.Join(globalTemplatesDir, "gemini")
	if err := os.MkdirAll(tplDir, 0755); err != nil {
		t.Fatalf("failed to create gemini template dir: %v", err)
	}
	tplConfig := `{"harness": "generic"}`
	if err := os.WriteFile(filepath.Join(tplDir, "scion-agent.json"), []byte(tplConfig), 0644); err != nil {
		t.Fatalf("failed to write template config: %v", err)
	}

	// Create project structure
	projectDir := filepath.Join(tmpDir, "project")
	projectScionDir := filepath.Join(projectDir, ".scion")
	if err := os.MkdirAll(projectScionDir, 0755); err != nil {
		t.Fatalf("failed to create project .scion dir: %v", err)
	}

	// Create settings.json with tmux enabled
	settingsJSON := `
{
  "runtimes": {
    "mock": {
      "tmux": true
    }
  },
  "profiles": {
    "test": {
      "runtime": "mock"
    }
  }
}
`
	if err := os.WriteFile(filepath.Join(projectScionDir, "settings.json"), []byte(settingsJSON), 0644); err != nil {
		t.Fatalf("failed to write settings.json: %v", err)
	}

	// MockRuntime that simulates an error containing "tmux" in the command string
	// but is NOT a missing binary error.
	// This simulates the user's case:
	// "container run ... tmux new-session ... failed: ... Error: invalidArgument: path ... does not exist"
	originalErr := fmt.Errorf("container run -d ... tmux new-session ... failed: exit status 1 (output: Error: invalidArgument: \"path '/foo' does not exist\")")
	
mockRuntime := &runtime.MockRuntime{
		RunFunc: func(ctx context.Context, config runtime.RunConfig) (string, error) {
			return "", originalErr
		},
		ListFunc: func(ctx context.Context, labelFilter map[string]string) ([]api.AgentInfo, error) {
			return nil, nil
		},
		ImageExistsFunc: func(ctx context.Context, image string) (bool, error) {
			return true, nil
		},
	}

	manager := &AgentManager{
		Runtime: mockRuntime,
	}

	opts := api.StartOptions{
		Name:      "test-agent",
		GrovePath: projectScionDir,
		Profile:   "test",
		Task:      "do something",
		Template:  "gemini",
	}

	_, err = manager.Start(context.Background(), opts)
	if err == nil {
		t.Fatal("expected error, got nil")
	}

	// The error SHOULD NOT contain "tmux binary not found"
	unexpectedPart := "tmux binary not found"
	if strings.Contains(err.Error(), unexpectedPart) {
		t.Errorf("Error should NOT contain '%s', but got: %v", unexpectedPart, err)
	}
}

func TestStart_ErrorPropagation_Tmux_CommandNotFound(t *testing.T) {
	// Create a temporary project directory
	tmpDir, err := os.MkdirTemp("", "scion-repro-test-missing")
	if err != nil {
		t.Fatalf("failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(tmpDir)

	// Mock HOME
	originalHome := os.Getenv("HOME")
	defer os.Setenv("HOME", originalHome)
	os.Setenv("HOME", tmpDir)

	// Setup global templates
	globalScionDir := filepath.Join(tmpDir, ".scion")
	globalTemplatesDir := filepath.Join(globalScionDir, "templates")
	if err := os.MkdirAll(globalTemplatesDir, 0755); err != nil {
		t.Fatalf("failed to create global templates dir: %v", err)
	}

	// Create a dummy "gemini" template
	tplDir := filepath.Join(globalTemplatesDir, "gemini")
	if err := os.MkdirAll(tplDir, 0755); err != nil {
		t.Fatalf("failed to create gemini template dir: %v", err)
	}
	tplConfig := `{"harness": "generic"}`
	if err := os.WriteFile(filepath.Join(tplDir, "scion-agent.json"), []byte(tplConfig), 0644); err != nil {
		t.Fatalf("failed to write template config: %v", err)
	}

	// Create project structure
	projectDir := filepath.Join(tmpDir, "project")
	projectScionDir := filepath.Join(projectDir, ".scion")
	if err := os.MkdirAll(projectScionDir, 0755); err != nil {
		t.Fatalf("failed to create project .scion dir: %v", err)
	}

	// Create settings.json with tmux enabled
	settingsJSON := `
{
  "runtimes": {
    "mock": {
      "tmux": true
    }
  },
  "profiles": {
    "test": {
      "runtime": "mock"
    }
  }
}
`
	if err := os.WriteFile(filepath.Join(projectScionDir, "settings.json"), []byte(settingsJSON), 0644); err != nil {
		t.Fatalf("failed to write settings.json: %v", err)
	}

	// MockRuntime that simulates "tmux: command not found"
	originalErr := fmt.Errorf("container run failed: bash: tmux: command not found")
	
mockRuntime := &runtime.MockRuntime{
		RunFunc: func(ctx context.Context, config runtime.RunConfig) (string, error) {
			return "", originalErr
		},
		ListFunc: func(ctx context.Context, labelFilter map[string]string) ([]api.AgentInfo, error) {
			return nil, nil
		},
		ImageExistsFunc: func(ctx context.Context, image string) (bool, error) {
			return true, nil
		},
	}

	manager := &AgentManager{
		Runtime: mockRuntime,
	}

	opts := api.StartOptions{
		Name:      "test-agent",
		GrovePath: projectScionDir,
		Profile:   "test",
		Task:      "do something",
		Template:  "gemini",
	}

	_, err = manager.Start(context.Background(), opts)
	if err == nil {
		t.Fatal("expected error, got nil")
	}

	// The error SHOULD contain "tmux binary not found"
	expectedPart := "tmux binary not found"
	if !strings.Contains(err.Error(), expectedPart) {
		t.Errorf("Error SHOULD contain '%s', but got: %v", expectedPart, err)
	}
}