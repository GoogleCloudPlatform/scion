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
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/runtime"
)

// C-ROOT-INT-1: in strict mode (broker mode, Hub project ID), the agent state
// dir GetAgent and ProvisionAgent use is config.AgentDirForProject(...,
// hubProjectID) — the broker-side external dir located from the Hub project
// ID — whatever the project-id marker inside the shared workspace's project
// dir says, and never the in-project agents dir.

const (
	rootHubProjectID       = "66666666-6666-6666-6666-666666666666"
	rootMismatchedMarkerID = "77777777-7777-7777-7777-777777777777"
	rootAgentName          = "root-agent"
)

// stateRootFixture sets up HOME with a harness-config and default template,
// and a git-style project (.scion directory) with project-id marker markerID
// ("" = none). It returns the project .scion dir.
func stateRootFixture(t *testing.T, markerID string) string {
	t.Helper()
	tmpDir := t.TempDir()
	oldWd, _ := os.Getwd()
	_ = os.Chdir(tmpDir)
	t.Cleanup(func() { _ = os.Chdir(oldWd) })
	t.Setenv("HOME", tmpDir)

	globalScionDir := filepath.Join(tmpDir, ".scion")
	hcDir := filepath.Join(globalScionDir, "harness-configs", "test-harness")
	if err := os.MkdirAll(hcDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(hcDir, "config.yaml"), []byte("harness: generic\nuser: scion\nimage: root-image:v1\n"), 0644); err != nil {
		t.Fatal(err)
	}
	tplDir := filepath.Join(globalScionDir, "templates", "default")
	if err := os.MkdirAll(tplDir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(tplDir, "scion-agent.json"), []byte(`{"default_harness_config": "test-harness"}`), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(globalScionDir, "settings.yaml"), []byte("schema_version: \"1\"\nactive_profile: local\nprofiles:\n  local:\n    runtime: docker\n"), 0644); err != nil {
		t.Fatal(err)
	}
	projectScionDir := filepath.Join(tmpDir, "shared-project", ".scion")
	if err := os.MkdirAll(projectScionDir, 0755); err != nil {
		t.Fatal(err)
	}
	if markerID != "" {
		if err := config.WriteProjectID(projectScionDir, markerID); err != nil {
			t.Fatal(err)
		}
	}
	return projectScionDir
}

func strictSharedOpts(projectScionDir string) api.StartOptions {
	return api.StartOptions{
		Name:            rootAgentName,
		ProjectPath:     projectScionDir,
		SharedWorkspace: true,
		HubProjectID:    rootHubProjectID,
		BrokerMode:      true,
		NoAuth:          true,
	}
}

func strictSharedCtx() context.Context {
	ctx := api.ContextWithSharedWorkspace(context.Background())
	ctx = api.ContextWithHubProjectID(ctx, rootHubProjectID)
	return api.ContextWithBrokerMode(ctx)
}

func hubRootAgentDir(t *testing.T, projectScionDir string) string {
	t.Helper()
	dir, err := config.AgentDirForProject(projectScionDir, rootAgentName, true, rootHubProjectID)
	if err != nil {
		t.Fatal(err)
	}
	return dir
}

func newRootManager() Manager {
	return NewManager(&runtime.MockRuntime{
		ListFunc: func(ctx context.Context, labelFilter map[string]string) ([]api.AgentInfo, error) {
			return []api.AgentInfo{}, nil
		},
		RunFunc: func(ctx context.Context, cfg runtime.RunConfig) (string, error) {
			return "mock-id", nil
		},
	})
}

func assertNoAgentDirAt(t *testing.T, dir, what string) {
	t.Helper()
	if _, err := os.Stat(filepath.Join(dir, "scion-agent.json")); err == nil {
		t.Errorf("agent state was written to %s (%s)", dir, what)
	}
}

// provisionAndResolve provisions through the real AgentManager, then resolves
// the agent dir through GetAgent, both in strict shared mode, and checks both
// use the Hub-project-ID root.
func provisionAndResolve(t *testing.T, projectScionDir string) string {
	t.Helper()
	want := hubRootAgentDir(t, projectScionDir)
	createOpts := strictSharedOpts(projectScionDir)
	createOpts.FreshProvision = true // a create dispatch
	if _, err := newRootManager().Provision(context.Background(), createOpts); err != nil {
		t.Fatalf("Provision: %v", err)
	}
	if _, err := os.Stat(filepath.Join(want, "scion-agent.json")); err != nil {
		t.Fatalf("ProvisionAgent did not write the agent state under the Hub-project-ID root %s: %v", want, err)
	}
	agentDir, _, _, _, err := GetAgent(strictSharedCtx(), rootAgentName, "", "", "", projectScionDir, "", "", "", "")
	if err != nil {
		t.Fatalf("GetAgent: %v", err)
	}
	if agentDir != want {
		t.Fatalf("GetAgent agent dir = %q, want the Hub-project-ID root %q", agentDir, want)
	}
	assertNoAgentDirAt(t, filepath.Join(projectScionDir, "agents", rootAgentName), "in-project agents dir")
	return want
}

// (a) No (deleted) marker: strict mode still resolves the Hub-project-ID
// root; there is no in-project fallback.
func TestAgentStateRoot_StrictNoMarkerUsesHubRoot(t *testing.T) {
	projectScionDir := stateRootFixture(t, "")
	provisionAndResolve(t, projectScionDir)
}

// (a') A marker present at provision and deleted afterwards does not move the
// dir a later start resolves.
func TestAgentStateRoot_MissingMarkerDoesNotRedirect(t *testing.T) {
	projectScionDir := stateRootFixture(t, rootHubProjectID)
	want := provisionAndResolve(t, projectScionDir)
	if err := os.Remove(filepath.Join(projectScionDir, "project-id")); err != nil {
		t.Fatal(err)
	}
	agentDir, _, _, _, err := GetAgent(strictSharedCtx(), rootAgentName, "", "", "", projectScionDir, "", "", "", "")
	if err != nil || agentDir != want {
		t.Fatalf("after deleting the marker: GetAgent dir = %q, err = %v; want %q", agentDir, err, want)
	}
	if _, err := newRootManager().Start(context.Background(), strictSharedOpts(projectScionDir)); err != nil {
		t.Fatalf("Start after deleting the marker: %v", err)
	}
	assertNoAgentDirAt(t, filepath.Join(projectScionDir, "agents", rootAgentName), "in-project agents dir")
}

// (b) A marker rewritten to another project's ID (before provision, or after)
// never redirects the agent state to that project's external dir.
func TestAgentStateRoot_MismatchedMarkerDoesNotRedirect(t *testing.T) {
	t.Run("rewritten before provision", func(t *testing.T) {
		projectScionDir := stateRootFixture(t, rootMismatchedMarkerID)
		provisionAndResolve(t, projectScionDir)
		mismatched, err := config.AgentDirForProject(projectScionDir, rootAgentName, true, rootMismatchedMarkerID)
		if err != nil {
			t.Fatal(err)
		}
		assertNoAgentDirAt(t, mismatched, "the mismatched marker's project dir")
	})
	t.Run("rewritten after provision", func(t *testing.T) {
		projectScionDir := stateRootFixture(t, rootHubProjectID)
		want := provisionAndResolve(t, projectScionDir)
		if err := config.WriteProjectID(projectScionDir, rootMismatchedMarkerID); err != nil {
			t.Fatal(err)
		}
		agentDir, _, _, _, err := GetAgent(strictSharedCtx(), rootAgentName, "", "", "", projectScionDir, "", "", "", "")
		if err != nil || agentDir != want {
			t.Fatalf("after rewriting the marker: GetAgent dir = %q, err = %v; want %q", agentDir, err, want)
		}
		if _, err := newRootManager().Start(context.Background(), strictSharedOpts(projectScionDir)); err != nil {
			t.Fatalf("Start after rewriting the marker: %v", err)
		}
		mismatched, _ := config.AgentDirForProject(projectScionDir, rootAgentName, true, rootMismatchedMarkerID)
		assertNoAgentDirAt(t, mismatched, "the mismatched marker's project dir")
	})
}

// (c) Marker ID != Hub ID with the Hub-project-ID agent dir missing: strict
// mode fails closed (config.ErrAgentStateDirUnavailable) rather than using the
// marker's project dir or the in-project dir.
func TestAgentStateRoot_MarkerMismatchWithMissingHubDirFailsClosed(t *testing.T) {
	projectScionDir := stateRootFixture(t, rootHubProjectID)
	want := provisionAndResolve(t, projectScionDir)
	if err := config.WriteProjectID(projectScionDir, rootMismatchedMarkerID); err != nil {
		t.Fatal(err)
	}
	// Write a complete agent dir under the mismatched marker's project and the
	// in-project root; neither may be used.
	mismatched, _ := config.AgentDirForProject(projectScionDir, rootAgentName, true, rootMismatchedMarkerID)
	for _, dir := range []string{mismatched, filepath.Join(projectScionDir, "agents", rootAgentName)} {
		if err := os.MkdirAll(filepath.Join(dir, "home"), 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "scion-agent.json"), []byte(`{"harness_config": "test-harness", "image": "other-image:v9"}`), 0644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.RemoveAll(want); err != nil {
		t.Fatal(err)
	}

	var captured runtime.RunConfig
	mgr := NewManager(&runtime.MockRuntime{
		ListFunc: func(ctx context.Context, labelFilter map[string]string) ([]api.AgentInfo, error) {
			return []api.AgentInfo{}, nil
		},
		RunFunc: func(ctx context.Context, cfg runtime.RunConfig) (string, error) {
			captured = cfg
			return "mock-id", nil
		},
	})
	_, err := mgr.Start(context.Background(), strictSharedOpts(projectScionDir))
	if !errors.Is(err, config.ErrAgentStateDirUnavailable) {
		t.Fatalf("expected a fail-closed agent-state error, got err=%v (image %q)", err, captured.Image)
	}
	if captured.Image != "" {
		t.Fatalf("nothing may be started, got image %q", captured.Image)
	}
}
