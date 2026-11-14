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

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/runtime"
)

// TestStart_EphemeralRefusedBeforeSideEffects: on a runtime without
// ephemeral support, Start refuses an ephemeral agent before it lists,
// provisions or records anything, so no agent directory (workspace, home,
// agent-info phase) and no runtime call is left behind (ptone/scion#4815).
func TestStart_EphemeralRefusedBeforeSideEffects(t *testing.T) {
	tmp := t.TempDir()
	projectScionDir := filepath.Join(tmp, "project", ".scion")
	if err := os.MkdirAll(projectScionDir, 0o755); err != nil {
		t.Fatal(err)
	}
	called := false
	mgr := NewManager(&runtime.MockRuntime{
		ListFunc: func(context.Context, map[string]string) ([]api.AgentInfo, error) {
			called = true
			return nil, nil
		},
		RunFunc: func(context.Context, runtime.RunConfig) (string, error) {
			called = true
			return "id", errors.New("unexpected Run")
		},
	})
	_, err := mgr.Start(context.Background(), api.StartOptions{
		Name:        "eph",
		ProjectPath: projectScionDir,
		NoAuth:      true,
		Persistence: api.PersistenceEphemeral,
	})
	if err == nil || !strings.Contains(err.Error(), "ephemeral persistence is not supported by the mock runtime") {
		t.Fatalf("Start = %v, want the ephemeral refusal", err)
	}
	if called {
		t.Error("the runtime was called before the refusal")
	}
	if _, err := os.Stat(filepath.Join(projectScionDir, "agents", "eph")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("agent directory after the refusal: %v, want none (no workspace, home or agent-info)", err)
	}
}

// ephemeralMockRuntime is a mock runtime that supports ephemeral
// persistence.
type ephemeralMockRuntime struct{ *runtime.MockRuntime }

func (ephemeralMockRuntime) SupportsEphemeralPersistence() bool { return true }

// TestStart_EphemeralResumeStartsFresh: a resume of an ephemeral agent is a
// fresh start: the harness runs without its resume flag and gets the
// prompt-file task again.
func TestStart_EphemeralResumeStartsFresh(t *testing.T) {
	_, projectScionDir := newResumePhaseTestFixture(t, func(context.Context, map[string]string) ([]api.AgentInfo, error) {
		return nil, nil
	})
	agentDir := filepath.Join(projectScionDir, "agents", "resume-test")
	if err := os.WriteFile(filepath.Join(agentDir, "prompt.md"), []byte("the original task"), 0o644); err != nil {
		t.Fatal(err)
	}
	var got runtime.RunConfig
	mgr := NewManager(ephemeralMockRuntime{&runtime.MockRuntime{
		ListFunc: func(context.Context, map[string]string) ([]api.AgentInfo, error) { return nil, nil },
		RunFunc: func(_ context.Context, cfg runtime.RunConfig) (string, error) {
			got = cfg
			return "mock-id", nil
		},
	}})
	if _, err := mgr.Start(context.Background(), api.StartOptions{
		Name: "resume-test", ProjectPath: projectScionDir, BrokerMode: true, NoAuth: true,
		Resume: true, Persistence: api.PersistenceEphemeral,
	}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if got.Resume {
		t.Error("RunConfig.Resume = true, want a fresh harness session")
	}
	if got.Task != "the original task" {
		t.Errorf("RunConfig.Task = %q, want the prompt-file task again", got.Task)
	}
	if got.Persistence != api.PersistenceEphemeral {
		t.Errorf("RunConfig.Persistence = %q, want ephemeral", got.Persistence)
	}
}
