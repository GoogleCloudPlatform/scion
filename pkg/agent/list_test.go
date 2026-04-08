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
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/runtime"
)

func TestListEnrichesTemplateAndHarnessFromAgentInfo(t *testing.T) {
	// Create a temp grove structure
	tmpDir := t.TempDir()
	grovePath := filepath.Join(tmpDir, ".scion")
	agentName := "test-agent"
	agentHome := filepath.Join(grovePath, "agents", agentName, "home")
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
	if err := os.WriteFile(filepath.Join(grovePath, "agents", agentName, "scion-agent.json"), []byte("{}"), 0644); err != nil {
		t.Fatal(err)
	}

	// Create mock runtime that returns an agent with empty template (simulating
	// a container where the label wasn't set)
	mock := &runtime.MockRuntime{
		ListFunc: func(_ context.Context, _ map[string]string) ([]api.AgentInfo, error) {
			return []api.AgentInfo{
				{
					Name:            agentName,
					GrovePath:       grovePath,
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
	grovePath := filepath.Join(tmpDir, ".scion")
	agentName := "labeled-agent"
	agentHome := filepath.Join(grovePath, "agents", agentName, "home")
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
	if err := os.WriteFile(filepath.Join(grovePath, "agents", agentName, "scion-agent.json"), []byte("{}"), 0644); err != nil {
		t.Fatal(err)
	}

	mock := &runtime.MockRuntime{
		ListFunc: func(_ context.Context, _ map[string]string) ([]api.AgentInfo, error) {
			return []api.AgentInfo{
				{
					Name:      agentName,
					GrovePath: grovePath,
					Template:  "from-runtime-label", // already set by runtime
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
	grovePath := filepath.Join(tmpDir, ".scion")
	agentName := "mtime-agent"
	agentHome := filepath.Join(grovePath, "agents", agentName, "home")
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
	if err := os.WriteFile(filepath.Join(grovePath, "agents", agentName, "scion-agent.json"), []byte("{}"), 0644); err != nil {
		t.Fatal(err)
	}

	mock := &runtime.MockRuntime{
		ListFunc: func(_ context.Context, _ map[string]string) ([]api.AgentInfo, error) {
			return []api.AgentInfo{
				{
					Name:      agentName,
					GrovePath: grovePath,
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
	grovePath := filepath.Join(tmpDir, ".scion")
	agentName := "stopped-agent"
	agentHome := filepath.Join(grovePath, "agents", agentName, "home")
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
	if err := os.WriteFile(filepath.Join(grovePath, "agents", agentName, "scion-agent.json"), []byte("{}"), 0644); err != nil {
		t.Fatal(err)
	}

	// No running containers
	mock := &runtime.MockRuntime{}

	mgr := NewManager(mock)
	agents, err := mgr.List(context.Background(), map[string]string{
		"scion.grove_path": grovePath,
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
	tests := []struct {
		name            string
		containerStatus string
		infoPhase       string
		infoActivity    string
		wantPhase       string
		wantActivity    string
	}{
		{
			name:            "running container overrides stopped phase",
			containerStatus: "Up 2 hours",
			infoPhase:       string(state.PhaseStopped),
			wantPhase:       string(state.PhaseRunning),
		},
		{
			name:            "running status overrides stopped phase",
			containerStatus: "running",
			infoPhase:       string(state.PhaseStopped),
			wantPhase:       string(state.PhaseRunning),
		},
		{
			name:            "exited container overrides running phase",
			containerStatus: "Exited (0) 5 minutes ago",
			infoPhase:       string(state.PhaseRunning),
			infoActivity:    string(state.ActivityThinking),
			wantPhase:       string(state.PhaseStopped),
			wantActivity:    "",
		},
		{
			name:            "stopped container overrides running phase",
			containerStatus: "stopped",
			infoPhase:       string(state.PhaseRunning),
			infoActivity:    string(state.ActivityExecuting),
			wantPhase:       string(state.PhaseStopped),
			wantActivity:    "",
		},
		{
			name:            "consistent running state unchanged",
			containerStatus: "Up 10 minutes",
			infoPhase:       string(state.PhaseRunning),
			infoActivity:    string(state.ActivityThinking),
			wantPhase:       string(state.PhaseRunning),
			wantActivity:    string(state.ActivityThinking),
		},
		{
			name:            "consistent stopped state unchanged",
			containerStatus: "Exited (0) 1 hour ago",
			infoPhase:       string(state.PhaseStopped),
			wantPhase:       string(state.PhaseStopped),
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			tmpDir := t.TempDir()
			grovePath := filepath.Join(tmpDir, ".scion")
			agentName := "reconcile-agent"
			agentHome := filepath.Join(grovePath, "agents", agentName, "home")
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
			if err := os.WriteFile(filepath.Join(grovePath, "agents", agentName, "scion-agent.json"), []byte("{}"), 0644); err != nil {
				t.Fatal(err)
			}

			mock := &runtime.MockRuntime{
				ListFunc: func(_ context.Context, _ map[string]string) ([]api.AgentInfo, error) {
					return []api.AgentInfo{
						{
							Name:            agentName,
							GrovePath:       grovePath,
							ContainerStatus: tc.containerStatus,
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
	tests := []struct {
		name            string
		runtimePhase    string
		containerStatus string
		wantPhase       string
	}{
		{
			name:            "legacy ended maps completed pod to stopped",
			runtimePhase:    "ended",
			containerStatus: "Succeeded (Completed)",
			wantPhase:       string(state.PhaseStopped),
		},
		{
			name:            "legacy ended maps failed pod to error",
			runtimePhase:    "ended",
			containerStatus: "Failed (Error)",
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
			grovePath := filepath.Join(tmpDir, ".scion")
			agentName := "k8s-agent"
			agentHome := filepath.Join(grovePath, "agents", agentName, "home")
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
			if err := os.WriteFile(filepath.Join(grovePath, "agents", agentName, "scion-agent.json"), []byte("{}"), 0644); err != nil {
				t.Fatal(err)
			}

			mock := &runtime.MockRuntime{
				ListFunc: func(_ context.Context, _ map[string]string) ([]api.AgentInfo, error) {
					return []api.AgentInfo{
						{
							Name:            agentName,
							GrovePath:       grovePath,
							Runtime:         "kubernetes",
							Phase:           tc.runtimePhase,
							ContainerStatus: tc.containerStatus,
						},
					}, nil
				},
			}

			mgr := NewManager(mock)
			agents, err := mgr.List(context.Background(), nil)
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
