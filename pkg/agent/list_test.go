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
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/runtime"
)

func TestListEnrichesTemplateAndHarnessFromAgentInfo(t *testing.T) {
	// Create a temp project structure
	tmpDir := t.TempDir()
	projectPath := filepath.Join(tmpDir, ".scion")
	agentName := "test-agent"
	agentHome := filepath.Join(projectPath, "agents", agentName, "home")
	if err := os.MkdirAll(agentHome, 0755); err != nil {
		t.Fatal(err)
	}

	// Write agent-info.json with template and harness-config
	info := api.AgentInfo{
		Name:          agentName,
		Template:      "my-template",
		HarnessConfig: "claude",
		Phase:         "running",
		Runtime:       "docker",
	}
	infoData, _ := json.MarshalIndent(info, "", "  ")
	infoPath := filepath.Join(agentHome, "agent-info.json")
	if err := os.WriteFile(infoPath, infoData, 0644); err != nil {
		t.Fatal(err)
	}

	// Write scion-agent.json so the agent dir is recognized
	if err := os.WriteFile(filepath.Join(projectPath, "agents", agentName, "scion-agent.json"), []byte("{}"), 0644); err != nil {
		t.Fatal(err)
	}

	// Create mock runtime that returns an agent with empty template (simulating
	// a container where the label wasn't set)
	mock := &runtime.MockRuntime{
		ListFunc: func(_ context.Context, _ map[string]string) ([]api.AgentInfo, error) {
			return []api.AgentInfo{
				{
					Name:            agentName,
					ProjectPath:     projectPath,
					ContainerStatus: "Up 2 hours",
					// Template and HarnessConfig intentionally empty
				},
			}, nil
		},
	}

	mgr := NewManager(mock)
	agents, err := mgr.List(context.Background(), nil)
	if err != nil {
		t.Fatalf("List() error: %v", err)
	}

	// Find our agent
	var found *api.AgentInfo
	for i := range agents {
		if agents[i].Name == agentName {
			found = &agents[i]
			break
		}
	}
	if found == nil {
		t.Fatal("agent not found in list results")
	}

	if found.Template != "my-template" {
		t.Errorf("Template = %q, want %q", found.Template, "my-template")
	}
	if found.HarnessConfig != "claude" {
		t.Errorf("HarnessConfig = %q, want %q", found.HarnessConfig, "claude")
	}
	if found.Phase != "running" {
		t.Errorf("Phase = %q, want %q", found.Phase, "running")
	}
}

func TestListDoesNotOverrideRuntimeTemplate(t *testing.T) {
	// When the runtime already provides a template via label, it should not
	// be overwritten by agent-info.json.
	tmpDir := t.TempDir()
	projectPath := filepath.Join(tmpDir, ".scion")
	agentName := "labeled-agent"
	agentHome := filepath.Join(projectPath, "agents", agentName, "home")
	if err := os.MkdirAll(agentHome, 0755); err != nil {
		t.Fatal(err)
	}

	info := api.AgentInfo{
		Name:          agentName,
		Template:      "from-info-json",
		HarnessConfig: "claude",
		Phase:         "running",
	}
	infoData, _ := json.MarshalIndent(info, "", "  ")
	if err := os.WriteFile(filepath.Join(agentHome, "agent-info.json"), infoData, 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(projectPath, "agents", agentName, "scion-agent.json"), []byte("{}"), 0644); err != nil {
		t.Fatal(err)
	}

	mock := &runtime.MockRuntime{
		ListFunc: func(_ context.Context, _ map[string]string) ([]api.AgentInfo, error) {
			return []api.AgentInfo{
				{
					Name:        agentName,
					ProjectPath: projectPath,
					Template:    "from-runtime-label", // already set by runtime
				},
			}, nil
		},
	}

	mgr := NewManager(mock)
	agents, err := mgr.List(context.Background(), nil)
	if err != nil {
		t.Fatalf("List() error: %v", err)
	}

	var found *api.AgentInfo
	for i := range agents {
		if agents[i].Name == agentName {
			found = &agents[i]
			break
		}
	}
	if found == nil {
		t.Fatal("agent not found")
	}

	// Runtime label should take precedence
	if found.Template != "from-runtime-label" {
		t.Errorf("Template = %q, want %q (runtime label should not be overwritten)", found.Template, "from-runtime-label")
	}
}

func TestListSetsLastSeenFromAgentInfoMtime(t *testing.T) {
	tmpDir := t.TempDir()
	projectPath := filepath.Join(tmpDir, ".scion")
	agentName := "mtime-agent"
	agentHome := filepath.Join(projectPath, "agents", agentName, "home")
	if err := os.MkdirAll(agentHome, 0755); err != nil {
		t.Fatal(err)
	}

	info := api.AgentInfo{
		Name:  agentName,
		Phase: "running",
	}
	infoData, _ := json.MarshalIndent(info, "", "  ")
	infoPath := filepath.Join(agentHome, "agent-info.json")
	if err := os.WriteFile(infoPath, infoData, 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(projectPath, "agents", agentName, "scion-agent.json"), []byte("{}"), 0644); err != nil {
		t.Fatal(err)
	}

	mock := &runtime.MockRuntime{
		ListFunc: func(_ context.Context, _ map[string]string) ([]api.AgentInfo, error) {
			return []api.AgentInfo{
				{
					Name:        agentName,
					ProjectPath: projectPath,
				},
			}, nil
		},
	}

	mgr := NewManager(mock)
	agents, err := mgr.List(context.Background(), nil)
	if err != nil {
		t.Fatalf("List() error: %v", err)
	}

	var found *api.AgentInfo
	for i := range agents {
		if agents[i].Name == agentName {
			found = &agents[i]
			break
		}
	}
	if found == nil {
		t.Fatal("agent not found")
	}

	if found.LastSeen.IsZero() {
		t.Error("LastSeen should be populated from agent-info.json mtime")
	}

	// LastSeen should be very recent (within the last few seconds)
	if time.Since(found.LastSeen) > 5*time.Second {
		t.Errorf("LastSeen = %v, expected to be within last 5s", found.LastSeen)
	}
}

func TestListNonRunningAgentIncludesHarnessConfig(t *testing.T) {
	tmpDir := t.TempDir()
	projectPath := filepath.Join(tmpDir, ".scion")
	agentName := "stopped-agent"
	agentHome := filepath.Join(projectPath, "agents", agentName, "home")
	if err := os.MkdirAll(agentHome, 0755); err != nil {
		t.Fatal(err)
	}

	info := api.AgentInfo{
		Name:          agentName,
		Template:      "research",
		HarnessConfig: "gemini",
		Phase:         "stopped",
	}
	infoData, _ := json.MarshalIndent(info, "", "  ")
	infoPath := filepath.Join(agentHome, "agent-info.json")
	if err := os.WriteFile(infoPath, infoData, 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(projectPath, "agents", agentName, "scion-agent.json"), []byte("{}"), 0644); err != nil {
		t.Fatal(err)
	}

	// No running containers
	mock := &runtime.MockRuntime{}

	mgr := NewManager(mock)
	agents, err := mgr.List(context.Background(), map[string]string{
		"scion.project_path": projectPath,
	})
	if err != nil {
		t.Fatalf("List() error: %v", err)
	}

	var found *api.AgentInfo
	for i := range agents {
		if agents[i].Name == agentName {
			found = &agents[i]
			break
		}
	}
	if found == nil {
		t.Fatal("stopped agent not found in list results")
	}

	if found.Template != "research" {
		t.Errorf("Template = %q, want %q", found.Template, "research")
	}
	if found.HarnessConfig != "gemini" {
		t.Errorf("HarnessConfig = %q, want %q", found.HarnessConfig, "gemini")
	}
	if found.LastSeen.IsZero() {
		t.Error("LastSeen should be populated for non-running agents")
	}
}

func TestListReconcilesPhaseWithContainerStatus(t *testing.T) {
	zero := 0
	tests := []struct {
		name            string
		runtimePhase    string
		containerStatus string
		exitCode        *int
		infoPhase       string
		infoActivity    string
		wantPhase       string
		wantActivity    string
	}{
		{
			name:            "running container overrides stopped phase",
			runtimePhase:    string(state.PhaseRunning),
			containerStatus: "Up 2 hours",
			infoPhase:       string(state.PhaseStopped),
			wantPhase:       string(state.PhaseRunning),
		},
		{
			name:            "running status overrides stopped phase",
			runtimePhase:    string(state.PhaseRunning),
			containerStatus: "running",
			infoPhase:       string(state.PhaseStopped),
			wantPhase:       string(state.PhaseRunning),
		},
		{
			name:            "exited container overrides running phase",
			runtimePhase:    string(state.PhaseStopped),
			containerStatus: "Exited (0) 5 minutes ago",
			exitCode:        &zero,
			infoPhase:       string(state.PhaseRunning),
			infoActivity:    string(state.ActivityThinking),
			wantPhase:       string(state.PhaseStopped),
			wantActivity:    "",
		},
		{
			name:            "stopped container overrides running phase",
			runtimePhase:    string(state.PhaseStopped),
			containerStatus: "stopped",
			exitCode:        &zero,
			infoPhase:       string(state.PhaseRunning),
			infoActivity:    string(state.ActivityExecuting),
			wantPhase:       string(state.PhaseStopped),
			wantActivity:    "",
		},
		{
			name:            "consistent running state unchanged",
			runtimePhase:    string(state.PhaseRunning),
			containerStatus: "Up 10 minutes",
			infoPhase:       string(state.PhaseRunning),
			infoActivity:    string(state.ActivityThinking),
			wantPhase:       string(state.PhaseRunning),
			wantActivity:    string(state.ActivityThinking),
		},
		{
			name:            "consistent stopped state unchanged",
			runtimePhase:    string(state.PhaseStopped),
			containerStatus: "Exited (0) 1 hour ago",
			exitCode:        &zero,
			infoPhase:       string(state.PhaseStopped),
			wantPhase:       string(state.PhaseStopped),
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			tmpDir := t.TempDir()
			projectPath := filepath.Join(tmpDir, ".scion")
			agentName := "reconcile-agent"
			agentHome := filepath.Join(projectPath, "agents", agentName, "home")
			if err := os.MkdirAll(agentHome, 0755); err != nil {
				t.Fatal(err)
			}

			info := api.AgentInfo{
				Name:     agentName,
				Phase:    tc.infoPhase,
				Activity: tc.infoActivity,
			}
			infoData, _ := json.MarshalIndent(info, "", "  ")
			if err := os.WriteFile(filepath.Join(agentHome, "agent-info.json"), infoData, 0644); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(projectPath, "agents", agentName, "scion-agent.json"), []byte("{}"), 0644); err != nil {
				t.Fatal(err)
			}

			mock := &runtime.MockRuntime{
				ListFunc: func(_ context.Context, _ map[string]string) ([]api.AgentInfo, error) {
					return []api.AgentInfo{
						{
							Name:            agentName,
							ProjectPath:     projectPath,
							Phase:           tc.runtimePhase,
							ContainerStatus: tc.containerStatus,
							ExitCode:        tc.exitCode,
						},
					}, nil
				},
			}

			mgr := NewManager(mock)
			agents, err := mgr.List(context.Background(), nil)
			if err != nil {
				t.Fatalf("List() error: %v", err)
			}

			var found *api.AgentInfo
			for i := range agents {
				if agents[i].Name == agentName {
					found = &agents[i]
					break
				}
			}
			if found == nil {
				t.Fatal("agent not found in list results")
			}

			if found.Phase != tc.wantPhase {
				t.Errorf("Phase = %q, want %q", found.Phase, tc.wantPhase)
			}
			if found.Activity != tc.wantActivity {
				t.Errorf("Activity = %q, want %q", found.Activity, tc.wantActivity)
			}
		})
	}
}

func TestListPreservesRuntimeTerminalStateForKubernetes(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	nonZero := 1
	tests := []struct {
		name            string
		runtimePhase    string
		containerStatus string
		exitCode        *int
		wantPhase       string
	}{
		{
			name:            "legacy ended maps completed pod to stopped",
			runtimePhase:    runtime.LegacyAgentPhaseEnded,
			containerStatus: "Succeeded (Completed)",
			wantPhase:       string(state.PhaseStopped),
		},
		{
			name:            "legacy ended maps failed pod to error",
			runtimePhase:    runtime.LegacyAgentPhaseEnded,
			containerStatus: "Failed (Error)",
			exitCode:        &nonZero,
			wantPhase:       string(state.PhaseError),
		},
		{
			name:            "structured stopped phase wins over stale info",
			runtimePhase:    string(state.PhaseStopped),
			containerStatus: "Succeeded (Completed)",
			wantPhase:       string(state.PhaseStopped),
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			tmpDir := t.TempDir()
			projectPath := filepath.Join(tmpDir, ".scion")
			agentName := "k8s-agent"
			agentHome := filepath.Join(projectPath, "agents", agentName, "home")
			if err := os.MkdirAll(agentHome, 0755); err != nil {
				t.Fatal(err)
			}

			info := api.AgentInfo{
				Name:     agentName,
				Phase:    string(state.PhaseRunning),
				Activity: string(state.ActivityThinking),
				Runtime:  "kubernetes",
			}
			infoData, _ := json.MarshalIndent(info, "", "  ")
			infoPath := filepath.Join(agentHome, "agent-info.json")
			if err := os.WriteFile(infoPath, infoData, 0644); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(projectPath, "agents", agentName, "scion-agent.json"), []byte("{}"), 0644); err != nil {
				t.Fatal(err)
			}

			mock := &runtime.MockRuntime{
				ListFunc: func(_ context.Context, _ map[string]string) ([]api.AgentInfo, error) {
					return []api.AgentInfo{
						{
							Name:            agentName,
							ProjectPath:     projectPath,
							Runtime:         "kubernetes",
							Phase:           tc.runtimePhase,
							ContainerStatus: tc.containerStatus,
							ExitCode:        tc.exitCode,
						},
					}, nil
				},
			}

			mgr := NewManager(mock)
			agents, err := mgr.List(context.Background(), map[string]string{
				"scion.project_path": projectPath,
			})
			if err != nil {
				t.Fatalf("List() error: %v", err)
			}

			if len(agents) != 1 {
				t.Fatalf("expected 1 agent, got %d", len(agents))
			}
			if agents[0].Phase != tc.wantPhase {
				t.Errorf("Phase = %q, want %q", agents[0].Phase, tc.wantPhase)
			}
			if agents[0].Activity != "" {
				t.Errorf("Activity = %q, want empty", agents[0].Activity)
			}

			updatedData, err := os.ReadFile(infoPath)
			if err != nil {
				t.Fatalf("failed to read updated agent-info.json: %v", err)
			}
			var updated api.AgentInfo
			if err := json.Unmarshal(updatedData, &updated); err != nil {
				t.Fatalf("failed to decode updated agent-info.json: %v", err)
			}
			if updated.Phase != tc.wantPhase {
				t.Errorf("persisted Phase = %q, want %q", updated.Phase, tc.wantPhase)
			}
			if updated.Activity != "" {
				t.Errorf("persisted Activity = %q, want empty", updated.Activity)
			}
		})
	}
}

func TestListPhaseErrorPreservedWithNilExitCode(t *testing.T) {
	// Verify that when the runtime reports PhaseError but ExitCode is nil
	// (unknown), the phase is NOT downgraded to PhaseStopped. This covers
	// the case where a K8s pod fails without a captured exit code.
	nonZero := 137
	tests := []struct {
		name         string
		runtimePhase string
		exitCode     *int
		infoPhase    string
		wantPhase    string
	}{
		{
			name:         "PhaseError with nil exit code stays PhaseError",
			runtimePhase: string(state.PhaseError),
			exitCode:     nil,
			infoPhase:    string(state.PhaseRunning),
			wantPhase:    string(state.PhaseError),
		},
		{
			name:         "PhaseError with non-zero exit code stays PhaseError",
			runtimePhase: string(state.PhaseError),
			exitCode:     &nonZero,
			infoPhase:    string(state.PhaseRunning),
			wantPhase:    string(state.PhaseError),
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			tmpDir := t.TempDir()
			projectPath := filepath.Join(tmpDir, ".scion")
			agentName := "phase-error-agent"
			agentHome := filepath.Join(projectPath, "agents", agentName, "home")
			if err := os.MkdirAll(agentHome, 0755); err != nil {
				t.Fatal(err)
			}

			info := api.AgentInfo{
				Name:     agentName,
				Phase:    tc.infoPhase,
				Activity: string(state.ActivityThinking),
			}
			infoData, _ := json.MarshalIndent(info, "", "  ")
			if err := os.WriteFile(filepath.Join(agentHome, "agent-info.json"), infoData, 0644); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(projectPath, "agents", agentName, "scion-agent.json"), []byte("{}"), 0644); err != nil {
				t.Fatal(err)
			}

			mock := &runtime.MockRuntime{
				ListFunc: func(_ context.Context, _ map[string]string) ([]api.AgentInfo, error) {
					return []api.AgentInfo{
						{
							Name:        agentName,
							ProjectPath: projectPath,
							Phase:       tc.runtimePhase,
							ExitCode:    tc.exitCode,
						},
					}, nil
				},
			}

			mgr := NewManager(mock)
			agents, err := mgr.List(context.Background(), nil)
			if err != nil {
				t.Fatalf("List() error: %v", err)
			}

			var found *api.AgentInfo
			for i := range agents {
				if agents[i].Name == agentName {
					found = &agents[i]
					break
				}
			}
			if found == nil {
				t.Fatal("agent not found in list results")
			}

			if found.Phase != tc.wantPhase {
				t.Errorf("Phase = %q, want %q", found.Phase, tc.wantPhase)
			}
			if found.Activity != "" {
				t.Errorf("Activity = %q, want empty (should be cleared for terminal phase)", found.Activity)
			}
		})
	}
}

func TestListTerminalPhaseOverridesAgentInfoPhase(t *testing.T) {
	// When the runtime reports a terminal phase (stopped/error), it takes
	// precedence over whatever agent-info.json says. The runtimePhases map
	// captures the runtime's Phase before agent-info.json merge to make the
	// reconciliation authoritative.
	nonZero := 137
	zero := 0
	tests := []struct {
		name         string
		runtimePhase string
		exitCode     *int
		infoPhase    string
		infoActivity string
		wantPhase    string
		wantActivity string
	}{
		{
			name:         "runtime stopped overrides info running",
			runtimePhase: string(state.PhaseStopped),
			exitCode:     &zero,
			infoPhase:    string(state.PhaseRunning),
			infoActivity: string(state.ActivityThinking),
			wantPhase:    string(state.PhaseStopped),
			wantActivity: "",
		},
		{
			name:         "runtime error overrides info running",
			runtimePhase: string(state.PhaseError),
			exitCode:     &nonZero,
			infoPhase:    string(state.PhaseRunning),
			infoActivity: string(state.ActivityExecuting),
			wantPhase:    string(state.PhaseError),
			wantActivity: "",
		},
		{
			name:         "runtime stopped with non-zero exit stays stopped locally",
			runtimePhase: string(state.PhaseStopped),
			exitCode:     &nonZero,
			infoPhase:    string(state.PhaseRunning),
			infoActivity: string(state.ActivityThinking),
			wantPhase:    string(state.PhaseStopped),
			wantActivity: "",
		},
		{
			name:         "runtime stopped with nil exit code stays stopped",
			runtimePhase: string(state.PhaseStopped),
			exitCode:     nil,
			infoPhase:    string(state.PhaseRunning),
			infoActivity: string(state.ActivityThinking),
			wantPhase:    string(state.PhaseStopped),
			wantActivity: "",
		},
		{
			name:         "runtime running allows info phase through",
			runtimePhase: string(state.PhaseRunning),
			infoPhase:    string(state.PhaseRunning),
			infoActivity: string(state.ActivityThinking),
			wantPhase:    string(state.PhaseRunning),
			wantActivity: string(state.ActivityThinking),
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			tmpDir := t.TempDir()
			projectPath := filepath.Join(tmpDir, ".scion")
			agentName := "terminal-phase-agent"
			agentHome := filepath.Join(projectPath, "agents", agentName, "home")
			if err := os.MkdirAll(agentHome, 0755); err != nil {
				t.Fatal(err)
			}

			info := api.AgentInfo{
				Name:     agentName,
				Phase:    tc.infoPhase,
				Activity: tc.infoActivity,
			}
			infoData, _ := json.MarshalIndent(info, "", "  ")
			if err := os.WriteFile(filepath.Join(agentHome, "agent-info.json"), infoData, 0644); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(projectPath, "agents", agentName, "scion-agent.json"), []byte("{}"), 0644); err != nil {
				t.Fatal(err)
			}

			mock := &runtime.MockRuntime{
				ListFunc: func(_ context.Context, _ map[string]string) ([]api.AgentInfo, error) {
					return []api.AgentInfo{
						{
							Name:            agentName,
							ProjectPath:     projectPath,
							Phase:           tc.runtimePhase,
							ContainerStatus: "Exited (137) 2 minutes ago",
							ExitCode:        tc.exitCode,
						},
					}, nil
				},
			}

			mgr := NewManager(mock)
			agents, err := mgr.List(context.Background(), nil)
			if err != nil {
				t.Fatalf("List() error: %v", err)
			}

			var found *api.AgentInfo
			for i := range agents {
				if agents[i].Name == agentName {
					found = &agents[i]
					break
				}
			}
			if found == nil {
				t.Fatal("agent not found in list results")
			}

			if found.Phase != tc.wantPhase {
				t.Errorf("Phase = %q, want %q", found.Phase, tc.wantPhase)
			}
			if found.Activity != tc.wantActivity {
				t.Errorf("Activity = %q, want %q", found.Activity, tc.wantActivity)
			}
		})
	}
}

func TestListLegacyEndedWithExitCode(t *testing.T) {
	// Verify that the legacy "ended" Phase uses the structured ExitCode
	// field (not ContainerStatus string parsing) to decide stopped vs error.
	t.Setenv("HOME", t.TempDir())
	zero := 0
	nonZero := 1
	tests := []struct {
		name            string
		exitCode        *int
		containerStatus string
		wantPhase       string
	}{
		{
			name:            "ended with zero exit code maps to stopped",
			exitCode:        &zero,
			containerStatus: "Succeeded (Completed)",
			wantPhase:       string(state.PhaseStopped),
		},
		{
			name:            "ended with non-zero exit code maps to error",
			exitCode:        &nonZero,
			containerStatus: "Failed (Error)",
			wantPhase:       string(state.PhaseError),
		},
		{
			name:            "ended with nil exit code maps to stopped",
			exitCode:        nil,
			containerStatus: "Succeeded (Completed)",
			wantPhase:       string(state.PhaseStopped),
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			tmpDir := t.TempDir()
			projectPath := filepath.Join(tmpDir, ".scion")
			agentName := "legacy-ended-agent"
			agentHome := filepath.Join(projectPath, "agents", agentName, "home")
			if err := os.MkdirAll(agentHome, 0755); err != nil {
				t.Fatal(err)
			}

			info := api.AgentInfo{
				Name:     agentName,
				Phase:    string(state.PhaseRunning),
				Activity: string(state.ActivityThinking),
				Runtime:  "kubernetes",
			}
			infoData, _ := json.MarshalIndent(info, "", "  ")
			infoPath := filepath.Join(agentHome, "agent-info.json")
			if err := os.WriteFile(infoPath, infoData, 0644); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(projectPath, "agents", agentName, "scion-agent.json"), []byte("{}"), 0644); err != nil {
				t.Fatal(err)
			}

			mock := &runtime.MockRuntime{
				ListFunc: func(_ context.Context, _ map[string]string) ([]api.AgentInfo, error) {
					return []api.AgentInfo{
						{
							Name:            agentName,
							ProjectPath:     projectPath,
							Runtime:         "kubernetes",
							Phase:           runtime.LegacyAgentPhaseEnded,
							ContainerStatus: tc.containerStatus,
							ExitCode:        tc.exitCode,
						},
					}, nil
				},
			}

			mgr := NewManager(mock)
			agents, err := mgr.List(context.Background(), map[string]string{
				"scion.project_path": projectPath,
			})
			if err != nil {
				t.Fatalf("List() error: %v", err)
			}

			if len(agents) != 1 {
				t.Fatalf("expected 1 agent, got %d", len(agents))
			}
			if agents[0].Phase != tc.wantPhase {
				t.Errorf("Phase = %q, want %q", agents[0].Phase, tc.wantPhase)
			}
			if agents[0].Activity != "" {
				t.Errorf("Activity = %q, want empty", agents[0].Activity)
			}
		})
	}
}

func TestPersistAgentInfoState_AtomicallyRewritesAndPreservesMode(t *testing.T) {
	tmpDir := t.TempDir()
	infoPath := filepath.Join(tmpDir, "agent-info.json")

	info := api.AgentInfo{
		Name:     "agent",
		Phase:    string(state.PhaseRunning),
		Activity: string(state.ActivityThinking),
	}
	data, err := json.MarshalIndent(info, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(infoPath, data, 0600); err != nil {
		t.Fatal(err)
	}

	if err := persistAgentInfoState(infoPath, string(state.PhaseStopped), ""); err != nil {
		t.Fatalf("persistAgentInfoState() error = %v", err)
	}

	updatedData, err := os.ReadFile(infoPath)
	if err != nil {
		t.Fatal(err)
	}
	var updated api.AgentInfo
	if err := json.Unmarshal(updatedData, &updated); err != nil {
		t.Fatal(err)
	}
	if updated.Phase != string(state.PhaseStopped) {
		t.Fatalf("Phase = %q, want %q", updated.Phase, state.PhaseStopped)
	}
	if updated.Activity != "" {
		t.Fatalf("Activity = %q, want empty", updated.Activity)
	}

	fi, err := os.Stat(infoPath)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0600 {
		t.Fatalf("mode = %o, want %o", fi.Mode().Perm(), os.FileMode(0600))
	}
	tempFiles, err := filepath.Glob(filepath.Join(tmpDir, ".agent-info.json.tmp-*"))
	if err != nil {
		t.Fatal(err)
	}
	if len(tempFiles) != 0 {
		t.Fatalf("temp files should not remain: %v", tempFiles)
	}
}

// writeCreatedAgentDir lays out an on-disk agent directory (no container)
// under projectPath, the shape List's created-agent scan recognises.
func writeCreatedAgentDir(t *testing.T, projectPath, name string) {
	t.Helper()
	agentHome := filepath.Join(projectPath, "agents", name, "home")
	if err := os.MkdirAll(agentHome, 0755); err != nil {
		t.Fatal(err)
	}
	infoData, _ := json.Marshal(api.AgentInfo{Name: name, Phase: "created"})
	if err := os.WriteFile(filepath.Join(agentHome, "agent-info.json"), infoData, 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(projectPath, "agents", name, "scion-agent.json"), []byte("{}"), 0644); err != nil {
		t.Fatal(err)
	}
}

func listedNames(agents []api.AgentInfo) []string {
	names := make([]string, 0, len(agents))
	for _, a := range agents {
		names = append(names, a.Name)
	}
	sort.Strings(names)
	return names
}

// TestListCreatedAgentScanHonoursNameFilter covers the created-agent
// (no container) on-disk scan: a "scion.name" filter must select only the
// matching agent directory, exactly as the runtime label filter does for
// containers, and must never fall back to other agents in the project.
func TestListCreatedAgentScanHonoursNameFilter(t *testing.T) {
	tests := []struct {
		name   string
		agents []string
		filter string
		want   []string
	}{
		{name: "valid name among two agents", agents: []string{"alpha", "beta"}, filter: "beta", want: []string{"beta"}},
		{name: "unknown name among two agents", agents: []string{"alpha", "beta"}, filter: "gamma", want: []string{}},
		{name: "unknown name with a single agent", agents: []string{"alpha"}, filter: "gamma", want: []string{}},
		{name: "empty name does not match", agents: []string{"alpha"}, filter: "", want: []string{}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			projectPath := filepath.Join(t.TempDir(), ".scion")
			for _, n := range tt.agents {
				writeCreatedAgentDir(t, projectPath, n)
			}

			mgr := NewManager(&runtime.MockRuntime{})
			agents, err := mgr.List(context.Background(), map[string]string{
				"scion.name":         tt.filter,
				"scion.project_path": projectPath,
			})
			if err != nil {
				t.Fatalf("List() error: %v", err)
			}
			got := listedNames(agents)
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("List() names = %v, want %v", got, tt.want)
			}
		})
	}
}

// TestListCreatedAgentScanWithoutNameFilterUnchanged pins the behaviour
// existing callers rely on: with no "scion.name" filter, every created
// agent in the project is returned.
func TestListCreatedAgentScanWithoutNameFilterUnchanged(t *testing.T) {
	projectPath := filepath.Join(t.TempDir(), ".scion")
	for _, n := range []string{"alpha", "beta", "gamma"} {
		writeCreatedAgentDir(t, projectPath, n)
	}

	mgr := NewManager(&runtime.MockRuntime{})
	agents, err := mgr.List(context.Background(), map[string]string{
		"scion.agent":        "true",
		"scion.project_path": projectPath,
	})
	if err != nil {
		t.Fatalf("List() error: %v", err)
	}
	want := []string{"alpha", "beta", "gamma"}
	if got := listedNames(agents); !reflect.DeepEqual(got, want) {
		t.Errorf("List() names = %v, want %v", got, want)
	}
}

// writeCreatedAgentDirWithInfo is writeCreatedAgentDir with a caller-supplied
// agent-info.json payload.
func writeCreatedAgentDirWithInfo(t *testing.T, projectPath string, info api.AgentInfo) {
	t.Helper()
	writeCreatedAgentDir(t, projectPath, info.Name)
	data, _ := json.Marshal(info)
	if err := os.WriteFile(filepath.Join(projectPath, "agents", info.Name, "home", "agent-info.json"), data, 0644); err != nil {
		t.Fatal(err)
	}
}

// writeHubLinkedSettings links the project at projectPath (its .scion
// directory) to Hub project hubProjectID.
func writeHubLinkedSettings(t *testing.T, projectPath, hubProjectID string) {
	t.Helper()
	if err := os.MkdirAll(projectPath, 0755); err != nil {
		t.Fatal(err)
	}
	data, _ := json.Marshal(map[string]interface{}{
		"hub": map[string]interface{}{"projectId": hubProjectID},
	})
	if err := os.WriteFile(filepath.Join(projectPath, "settings.json"), data, 0644); err != nil {
		t.Fatal(err)
	}
}

// TestListCreatedAgentScanFilterParity covers every filter key the runtime
// layer applies, evaluated by the on-disk created-agent scan through the
// shared matcher. Each case also carries scion.project_path, which is what
// enables the scan.
func TestListCreatedAgentScanFilterParity(t *testing.T) {
	tests := []struct {
		name      string
		hubLinked string // Hub project ID written to settings; "" = unlinked
		filter    func(projectPath string) map[string]string
		want      []string
	}{
		{name: "scion.agent true matches", filter: func(string) map[string]string { return map[string]string{"scion.agent": "true"} }, want: []string{"alpha", "beta"}},
		{name: "scion.agent false matches nothing", filter: func(string) map[string]string { return map[string]string{"scion.agent": "false"} }, want: []string{}},
		{name: "scion.name selects one", filter: func(string) map[string]string { return map[string]string{"scion.name": "alpha"} }, want: []string{"alpha"}},
		{name: "scion.project matching name", filter: func(p string) map[string]string { return map[string]string{"scion.project": config.GetProjectName(p)} }, want: []string{"alpha", "beta"}},
		{name: "scion.project other name", filter: func(string) map[string]string { return map[string]string{"scion.project": "some-other-project"} }, want: []string{}},
		{name: "scion.project_id matches linked project", hubLinked: "hub-proj-1", filter: func(string) map[string]string { return map[string]string{"scion.project_id": "hub-proj-1"} }, want: []string{"alpha", "beta"}},
		{name: "scion.project_id other ID on linked project", hubLinked: "hub-proj-1", filter: func(string) map[string]string { return map[string]string{"scion.project_id": "hub-proj-2"} }, want: []string{}},
		{name: "scion.project_id on unlinked project", filter: func(string) map[string]string { return map[string]string{"scion.project_id": "hub-proj-1"} }, want: []string{}},
		{name: "scion.project_id ignores the local project-id marker in agent-info", filter: func(string) map[string]string { return map[string]string{"scion.project_id": "local-marker-id"} }, want: []string{}},
		{name: "status never matches", filter: func(string) map[string]string { return map[string]string{"status": "created"} }, want: []string{}},
		{name: "scion.template selects one", filter: func(string) map[string]string { return map[string]string{"scion.template": "tmpl-b"} }, want: []string{"beta"}},
		{name: "scion.harness_config selects one", filter: func(string) map[string]string { return map[string]string{"scion.harness_config": "hc-a"} }, want: []string{"alpha"}},
		{name: "agent_id is unknown before start", filter: func(string) map[string]string { return map[string]string{"agent_id": "alpha"} }, want: []string{}},
		{name: "all keys combined", hubLinked: "hub-proj-1", filter: func(p string) map[string]string {
			return map[string]string{"scion.agent": "true", "scion.name": "beta", "scion.project": config.GetProjectName(p), "scion.project_id": "hub-proj-1"}
		}, want: []string{"beta"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tmp := t.TempDir()
			t.Setenv("HOME", tmp)
			projectPath := filepath.Join(tmp, "proj", ".scion")
			if tt.hubLinked != "" {
				writeHubLinkedSettings(t, projectPath, tt.hubLinked)
			}
			writeCreatedAgentDirWithInfo(t, projectPath, api.AgentInfo{Name: "alpha", Phase: "created", Template: "tmpl-a", HarnessConfig: "hc-a", ProjectID: "local-marker-id"})
			writeCreatedAgentDirWithInfo(t, projectPath, api.AgentInfo{Name: "beta", Phase: "created", Template: "tmpl-b", HarnessConfig: "hc-b", ProjectID: "local-marker-id"})

			filter := tt.filter(projectPath)
			filter["scion.project_path"] = projectPath

			mgr := NewManager(&runtime.MockRuntime{})
			agents, err := mgr.List(context.Background(), filter)
			if err != nil {
				t.Fatalf("List() error: %v", err)
			}
			if got := listedNames(agents); !reflect.DeepEqual(got, tt.want) {
				t.Errorf("List(%v) names = %v, want %v", filter, got, tt.want)
			}
		})
	}
}

// TestListCreatedAgentScanMatchesRuntimeFilter pins parity directly: a
// runtime that filters containers with the shared matcher, given a
// container carrying exactly the labels a started agent gets, must agree
// with the on-disk scan on every filter.
func TestListCreatedAgentScanMatchesRuntimeFilter(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)
	createdPath := filepath.Join(tmp, "proj", ".scion")
	writeHubLinkedSettings(t, createdPath, "hub-proj-1")
	writeCreatedAgentDirWithInfo(t, createdPath, api.AgentInfo{Name: "alpha", Phase: "created", Template: "tmpl-a", HarnessConfig: "hc-a"})

	containerLabels := map[string]string{
		"scion.agent":          "true",
		"scion.name":           "alpha",
		"scion.template":       "tmpl-a",
		"scion.harness_config": "hc-a",
		"scion.harness_auth":   "",
		"scion.project":        config.GetProjectName(createdPath),
		"scion.project_id":     "hub-proj-1",
		"scion.project_path":   createdPath,
	}
	rt := &runtime.MockRuntime{ListFunc: func(_ context.Context, filter map[string]string) ([]api.AgentInfo, error) {
		if runtime.LabelsMatchFilter(containerLabels, filter) {
			return []api.AgentInfo{{Name: "alpha", ContainerID: "c1", Labels: containerLabels}}, nil
		}
		return nil, nil
	}}

	filters := []map[string]string{
		{"scion.agent": "true"},
		{"scion.name": "alpha"},
		{"scion.name": "other"},
		{"scion.project": config.GetProjectName(createdPath)},
		{"scion.project": "other"},
		{"scion.project_id": "hub-proj-1"},
		{"scion.project_id": "hub-proj-2"},
		{"status": "running"},
		{"scion.template": "tmpl-a"},
		{"scion.template": "other"},
		{"scion.harness_config": "hc-a"},
		{"agent_id": "x"},
	}
	matched := 0
	for _, f := range filters {
		f["scion.project_path"] = createdPath
		running, err := NewManager(rt).List(context.Background(), f)
		if err != nil {
			t.Fatal(err)
		}
		created, err := NewManager(&runtime.MockRuntime{}).List(context.Background(), f)
		if err != nil {
			t.Fatal(err)
		}
		if len(running) != len(created) {
			t.Errorf("filter %v: runtime path returned %d agents, on-disk scan returned %d", f, len(running), len(created))
		}
		if len(created) > 0 {
			matched++
		}
	}
	if matched == 0 || matched == len(filters) {
		t.Errorf("matched %d of %d filters; the fixture must exercise both outcomes", matched, len(filters))
	}
}
