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

package runtime

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/harness"
)

func TestPodmanRuntime_Run_NoInitFlag(t *testing.T) {
	// Create a temporary script to act as a mock podman
	tmpDir := t.TempDir()
	mockPodman := filepath.Join(tmpDir, "mock-podman")

	script := `#!/bin/sh
echo "$@"
`
	if err := os.WriteFile(mockPodman, []byte(script), 0755); err != nil {
		t.Fatalf("failed to write mock podman: %v", err)
	}

	rt := &PodmanRuntime{
		Command: mockPodman,
	}

	config := RunConfig{
		Harness:      &harness.GeminiCLI{},
		Name:         "test-agent",
		UnixUsername: "scion",
		Image:        "scion-agent:latest",
		Task:         "hello",
	}

	out, err := rt.Run(context.Background(), config)
	if err != nil {
		t.Fatalf("runtime.Run failed: %v", err)
	}

	// sciontool handles PID 1 responsibilities, so --init should NOT be present
	if strings.Contains(out, "--init") {
		t.Errorf("expected '--init' to be absent in output, got %q", out)
	}

	if !strings.Contains(out, "run -t") {
		t.Errorf("expected 'run -t' in output, got %q", out)
	}
}

func TestPodmanRuntime_Exec_UserFlag(t *testing.T) {
	// Create a temporary script to act as a mock podman
	tmpDir := t.TempDir()
	mockPodman := filepath.Join(tmpDir, "mock-podman")

	script := `#!/bin/sh
echo "$@"
`
	if err := os.WriteFile(mockPodman, []byte(script), 0755); err != nil {
		t.Fatalf("failed to write mock podman: %v", err)
	}

	t.Run("rootful uses scion user", func(t *testing.T) {
		rt := &PodmanRuntime{
			Command:  mockPodman,
			Rootless: false,
		}

		out, err := rt.Exec(context.Background(), "test-container", []string{"whoami"})
		if err != nil {
			t.Fatalf("runtime.Exec failed: %v", err)
		}

		if !strings.Contains(out, "--user scion") {
			t.Errorf("expected '--user scion' in exec output, got %q", out)
		}
	})

	t.Run("rootless uses scion user", func(t *testing.T) {
		rt := &PodmanRuntime{
			Command:  mockPodman,
			Rootless: true,
		}

		out, err := rt.Exec(context.Background(), "test-container", []string{"whoami"})
		if err != nil {
			t.Fatalf("runtime.Exec failed: %v", err)
		}

		if !strings.Contains(out, "--user scion") {
			t.Errorf("expected '--user scion' in exec output, got %q", out)
		}
	})
}

func TestPodmanRuntime_ExecUserMethod(t *testing.T) {
	t.Run("rootful returns scion", func(t *testing.T) {
		rt := &PodmanRuntime{Rootless: false}
		if got := rt.ExecUser(); got != "scion" {
			t.Errorf("expected 'scion', got %q", got)
		}
	})

	t.Run("rootless returns scion", func(t *testing.T) {
		rt := &PodmanRuntime{Rootless: true}
		if got := rt.ExecUser(); got != "scion" {
			t.Errorf("expected 'scion', got %q", got)
		}
	})
}

func TestPodmanRuntime_List_JSONArray(t *testing.T) {
	// Create a mock podman that returns Podman-style JSON array output
	tmpDir := t.TempDir()
	mockPodman := filepath.Join(tmpDir, "mock-podman")

	jsonOutput := `[{"Id":"abc123def456","Names":["test-agent"],"Status":"Up 2 hours","Image":"scion-agent:latest","Labels":{"scion.grove":"mygrove","scion.template":"default"}}]`

	script := `#!/bin/sh
echo '` + jsonOutput + `'
`
	if err := os.WriteFile(mockPodman, []byte(script), 0755); err != nil {
		t.Fatalf("failed to write mock podman: %v", err)
	}

	rt := &PodmanRuntime{
		Command: mockPodman,
	}

	agents, err := rt.List(context.Background(), nil)
	if err != nil {
		t.Fatalf("runtime.List failed: %v", err)
	}

	if len(agents) != 1 {
		t.Fatalf("expected 1 agent, got %d", len(agents))
	}

	a := agents[0]
	if a.ContainerID != "abc123def456" {
		t.Errorf("expected ContainerID 'abc123def456', got %q", a.ContainerID)
	}
	if a.Name != "test-agent" {
		t.Errorf("expected Name 'test-agent', got %q", a.Name)
	}
	if a.ContainerStatus != "Up 2 hours" {
		t.Errorf("expected ContainerStatus 'Up 2 hours', got %q", a.ContainerStatus)
	}
	if a.Image != "scion-agent:latest" {
		t.Errorf("expected Image 'scion-agent:latest', got %q", a.Image)
	}
	if a.Labels["scion.grove"] != "mygrove" {
		t.Errorf("expected label scion.grove='mygrove', got %q", a.Labels["scion.grove"])
	}
	if a.Template != "default" {
		t.Errorf("expected Template 'default', got %q", a.Template)
	}
	if a.Runtime != "podman" {
		t.Errorf("expected Runtime 'podman', got %q", a.Runtime)
	}
}

func TestPodmanRuntime_List_EmptyArray(t *testing.T) {
	tmpDir := t.TempDir()
	mockPodman := filepath.Join(tmpDir, "mock-podman")

	script := `#!/bin/sh
echo '[]'
`
	if err := os.WriteFile(mockPodman, []byte(script), 0755); err != nil {
		t.Fatalf("failed to write mock podman: %v", err)
	}

	rt := &PodmanRuntime{
		Command: mockPodman,
	}

	agents, err := rt.List(context.Background(), nil)
	if err != nil {
		t.Fatalf("runtime.List failed: %v", err)
	}

	if len(agents) != 0 {
		t.Errorf("expected 0 agents for empty array, got %d", len(agents))
	}
}

func TestPodmanRuntime_List_LabelFiltering(t *testing.T) {
	tmpDir := t.TempDir()
	mockPodman := filepath.Join(tmpDir, "mock-podman")

	jsonOutput := `[{"Id":"aaa","Names":["agent-a"],"Status":"Up","Image":"img","Labels":{"scion.grove":"grove1","scion.template":"default"}},{"Id":"bbb","Names":["agent-b"],"Status":"Up","Image":"img","Labels":{"scion.grove":"grove2","scion.template":"custom"}}]`

	script := `#!/bin/sh
echo '` + jsonOutput + `'
`
	if err := os.WriteFile(mockPodman, []byte(script), 0755); err != nil {
		t.Fatalf("failed to write mock podman: %v", err)
	}

	rt := &PodmanRuntime{
		Command: mockPodman,
	}

	// Filter for grove1 only
	agents, err := rt.List(context.Background(), map[string]string{"scion.grove": "grove1"})
	if err != nil {
		t.Fatalf("runtime.List failed: %v", err)
	}

	if len(agents) != 1 {
		t.Fatalf("expected 1 agent after filtering, got %d", len(agents))
	}
	if agents[0].Name != "agent-a" {
		t.Errorf("expected filtered agent 'agent-a', got %q", agents[0].Name)
	}
}

func TestPodmanRuntime_List_NoLabels(t *testing.T) {
	tmpDir := t.TempDir()
	mockPodman := filepath.Join(tmpDir, "mock-podman")

	jsonOutput := `[{"Id":"nolabel","Names":["no-label-agent"],"Status":"Up","Image":"img","Labels":null}]`

	script := `#!/bin/sh
echo '` + jsonOutput + `'
`
	if err := os.WriteFile(mockPodman, []byte(script), 0755); err != nil {
		t.Fatalf("failed to write mock podman: %v", err)
	}

	rt := &PodmanRuntime{
		Command: mockPodman,
	}

	agents, err := rt.List(context.Background(), nil)
	if err != nil {
		t.Fatalf("runtime.List failed: %v", err)
	}

	if len(agents) != 1 {
		t.Fatalf("expected 1 agent, got %d", len(agents))
	}
	if agents[0].Labels == nil {
		t.Errorf("expected non-nil Labels map, got nil")
	}
}

func TestPodmanRuntime_List_MultipleNames(t *testing.T) {
	tmpDir := t.TempDir()
	mockPodman := filepath.Join(tmpDir, "mock-podman")

	jsonOutput := `[{"Id":"multi","Names":["primary-name","alias-name"],"Status":"Up","Image":"img","Labels":{}}]`

	script := `#!/bin/sh
echo '` + jsonOutput + `'
`
	if err := os.WriteFile(mockPodman, []byte(script), 0755); err != nil {
		t.Fatalf("failed to write mock podman: %v", err)
	}

	rt := &PodmanRuntime{
		Command: mockPodman,
	}

	agents, err := rt.List(context.Background(), nil)
	if err != nil {
		t.Fatalf("runtime.List failed: %v", err)
	}

	if len(agents) != 1 {
		t.Fatalf("expected 1 agent, got %d", len(agents))
	}
	// Should use the first name
	if agents[0].Name != "primary-name" {
		t.Errorf("expected first name 'primary-name', got %q", agents[0].Name)
	}
}

func TestPodmanRuntime_Name(t *testing.T) {
	rt := &PodmanRuntime{Command: "podman"}
	if rt.Name() != "podman" {
		t.Errorf("expected Name() = 'podman', got %q", rt.Name())
	}
}

func TestParsePodmanVersion(t *testing.T) {
	tests := []struct {
		input    string
		expected string
	}{
		{"podman version 4.9.3", "4.9.3"},
		{"podman version 5.0.0", "5.0.0"},
		{"podman version 3.4.1", "3.4.1"},
		{"4.9.3", "4.9.3"},
	}

	for _, tc := range tests {
		result := parsePodmanVersion(tc.input)
		if result != tc.expected {
			t.Errorf("parsePodmanVersion(%q) = %q, want %q", tc.input, result, tc.expected)
		}
	}
}

func TestParseMajorVersion(t *testing.T) {
	tests := []struct {
		input    string
		expected int
		wantErr  bool
	}{
		{"4.9.3", 4, false},
		{"5.0.0", 5, false},
		{"3.4.1", 3, false},
		{"", 0, true},
	}

	for _, tc := range tests {
		result, err := parseMajorVersion(tc.input)
		if tc.wantErr {
			if err == nil {
				t.Errorf("parseMajorVersion(%q) expected error, got nil", tc.input)
			}
			continue
		}
		if err != nil {
			t.Errorf("parseMajorVersion(%q) unexpected error: %v", tc.input, err)
			continue
		}
		if result != tc.expected {
			t.Errorf("parseMajorVersion(%q) = %d, want %d", tc.input, result, tc.expected)
		}
	}
}
