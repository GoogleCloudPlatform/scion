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

package runtimebroker

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/agent"
	"github.com/GoogleCloudPlatform/scion/pkg/agent/state"
	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/projectkeys"
	"github.com/GoogleCloudPlatform/scion/pkg/runtime"
)

// startWindowRuntime is a MockRuntime that declares the
// runtime.CopiedAgentHomeRuntime capability, as the Kubernetes runtime does.
type startWindowRuntime struct {
	*runtime.MockRuntime
}

func (startWindowRuntime) CopiesAgentHome() bool { return true }

// writeStartWindowFixture writes the global settings, harness config and
// template a broker-mode Start needs, and a provisioned agent in a project.
// It returns the project's .scion dir.
func writeStartWindowFixture(t *testing.T, agentName string) string {
	t.Helper()
	// The container exports SCION_* variables a broker-mode Start reads.
	t.Setenv("SCION_HOST_UID", "")
	t.Setenv("SCION_PROJECT", "")
	tmpDir := t.TempDir()
	t.Chdir(tmpDir)
	t.Setenv("HOME", tmpDir)

	write := func(path, content string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	globalScionDir := filepath.Join(tmpDir, ".scion")
	write(filepath.Join(globalScionDir, "harness-configs", "test-harness", "config.yaml"),
		"harness: generic\nuser: scion\nimage: test-image:latest\n")
	write(filepath.Join(globalScionDir, "templates", "default", "scion-agent.json"),
		`{"default_harness_config": "test-harness"}`)
	write(filepath.Join(globalScionDir, "settings.yaml"),
		"schema_version: \"1\"\nactive_profile: local\nprofiles:\n  local:\n    runtime: docker\n")

	projectScionDir := filepath.Join(tmpDir, "project", ".scion")
	agentDir := filepath.Join(projectScionDir, "agents", agentName)
	write(filepath.Join(agentDir, "scion-agent.json"), `{"harness": "generic", "harness_config": "test-harness"}`)
	write(filepath.Join(agentDir, "home", "agent-info.json"), `{"name":"`+agentName+`","phase":"stopped"}`)
	return projectScionDir
}

// TestHeartbeat_CopiedHomeRuntimeReportsStartingUntilRunReturns
// (ptone/scion#5368): on a runtime whose agent home is a one-way copy made
// during Run (Kubernetes), the container exists, and lists with no phase
// (a Running pod whose startup gate is still closed), long before Run
// returns. While Run is in progress, the broker heartbeat reports the agent
// as starting, so the hub keeps its launch active; once Run has returned it
// reports running.
func TestHeartbeat_CopiedHomeRuntimeReportsStartingUntilRunReturns(t *testing.T) {
	const agentName = "window-agent"
	projectScionDir := writeStartWindowFixture(t, agentName)

	var created atomic.Bool
	entered := make(chan struct{})
	release := make(chan struct{})
	rt := startWindowRuntime{&runtime.MockRuntime{
		NameFunc: func() string { return "fake" },
		ListFunc: func(context.Context, map[string]string) ([]api.AgentInfo, error) {
			if !created.Load() {
				return nil, nil
			}
			return []api.AgentInfo{{
				Name: agentName, ContainerID: "cid-run-2", RunID: "run-2",
				ContainerStatus: "Running", ProjectPath: projectScionDir,
				Labels: map[string]string{
					"scion.name":               agentName,
					"scion.agent":              "true",
					api.LabelRunID:             "run-2",
					projectkeys.LabelProjectID: "project-1",
				},
			}}, nil
		},
		RunFunc: func(ctx context.Context, _ runtime.RunConfig) (string, error) {
			created.Store(true)
			close(entered)
			select {
			case <-release:
			case <-ctx.Done():
				return "", ctx.Err()
			}
			return "cid-run-2", nil
		},
	}}
	mgr := agent.NewManager(rt)

	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	startErr := make(chan error, 1)
	startDone := make(chan struct{})
	// On an early failure, unblock Run and wait for Start, so it does not
	// write into the temp dirs while they are removed.
	t.Cleanup(func() {
		cancel()
		<-startDone
	})
	go func() {
		defer close(startDone)
		_, err := mgr.Start(ctx, api.StartOptions{
			Name: agentName, ProjectPath: projectScionDir, BrokerMode: true, NoAuth: true, RunID: "run-2",
			Template: "default",
		})
		startErr <- err
	}()
	select {
	case <-entered:
	case err := <-startErr:
		t.Fatalf("Start returned before Run was entered: %v", err)
	case <-ctx.Done():
		t.Fatal("Run was never entered")
	}

	client := &mockRuntimeBrokerService{}
	svc := NewHeartbeatService(client, "test-host", time.Hour, mgr, nil, slog.Default())
	heartbeatPhase := func() string {
		t.Helper()
		if err := svc.ForceHeartbeat(ctx); err != nil {
			t.Fatalf("ForceHeartbeat: %v", err)
		}
		calls := client.getHeartbeatCalls()
		if len(calls) == 0 {
			t.Fatal("no heartbeat sent")
		}
		var phases []string
		for _, p := range calls[len(calls)-1].Heartbeat.Projects {
			for _, a := range p.Agents {
				if a.Slug == agentName {
					phases = append(phases, a.Phase)
				}
			}
		}
		if len(phases) != 1 {
			t.Fatalf("%s reported %d times, want 1: %v", agentName, len(phases), phases)
		}
		return phases[0]
	}

	if got := heartbeatPhase(); got != string(state.PhaseStarting) {
		t.Errorf("heartbeat phase while Run is in progress = %q, want %q", got, state.PhaseStarting)
	}

	close(release)
	if err := <-startErr; err != nil {
		t.Fatalf("Start: %v", err)
	}
	if got := heartbeatPhase(); got != string(state.PhaseRunning) {
		t.Errorf("heartbeat phase after Run returned = %q, want %q", got, state.PhaseRunning)
	}
}
