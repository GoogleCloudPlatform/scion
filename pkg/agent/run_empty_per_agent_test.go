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
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/runtime"
)

// setupEmptyPerAgentStartProject writes an in-place project .scion dir under
// parent with a test harness-config and template. storage is spliced into
// settings.yaml under server: (e.g. a workspace_storage block), or omitted.
func setupEmptyPerAgentStartProject(t *testing.T, parent, storage string) string {
	t.Helper()
	projectScionDir := filepath.Join(parent, ".scion")
	if err := os.MkdirAll(projectScionDir, 0755); err != nil {
		t.Fatal(err)
	}
	server := ""
	if storage != "" {
		server = "server:\n" + storage
	}
	settingsYAML := `schema_version: "1"
active_profile: local
` + server + `harness_configs:
  test-harness:
    harness: gemini
    user: scion
    image: test-image:latest
profiles:
  local:
    runtime: docker
`
	if err := os.WriteFile(filepath.Join(projectScionDir, "settings.yaml"), []byte(settingsYAML), 0644); err != nil {
		t.Fatal(err)
	}
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
	return projectScionDir
}

func emptyPerAgentStartRuntime(captured *runtime.RunConfig, ran *bool) *runtime.MockRuntime {
	return &runtime.MockRuntime{
		ListFunc: func(ctx context.Context, labelFilter map[string]string) ([]api.AgentInfo, error) {
			return []api.AgentInfo{}, nil
		},
		RunFunc: func(ctx context.Context, cfg runtime.RunConfig) (string, error) {
			*captured = cfg
			*ran = true
			return "mock-id", nil
		},
	}
}

// TestStart_EmptyPerAgent_MountsPrivateWorkspace covers the Start half of
// design #2703 P2: the container's workspace is exactly the private
// agents/<slug>/workspace dir, no repo root is mounted even when the
// project sits in a git repository, and SCION_WORKSPACE_MODE alone (the
// start/restart shape, where the hub pre-resolves it into the env) is
// enough to select the mode.
func TestStart_EmptyPerAgent_MountsPrivateWorkspace(t *testing.T) {
	for _, tc := range []struct {
		name    string
		viaFlag bool
		inGit   bool
	}{
		{name: "StartOptions flag", viaFlag: true},
		{name: "SCION_WORKSPACE_MODE env only"},
		{name: "project inside a git repository", viaFlag: true, inGit: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tmpDir, err := filepath.EvalSymlinks(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			t.Chdir(tmpDir)
			t.Setenv("HOME", tmpDir)
			projectRoot := filepath.Join(tmpDir, "project")
			if tc.inGit {
				gitInitForTest(t, projectRoot)
			}
			projectScionDir := setupEmptyPerAgentStartProject(t, projectRoot, "")

			var captured runtime.RunConfig
			var ran bool
			mgr := NewManager(emptyPerAgentStartRuntime(&captured, &ran))
			_, err = mgr.Start(context.Background(), api.StartOptions{
				Name:                   "worker",
				ProjectPath:            projectScionDir,
				NoAuth:                 true,
				EmptyPerAgentWorkspace: tc.viaFlag,
				Env: map[string]string{
					"SCION_AGENT_ID":        "agent-1",
					"SCION_PROJECT_ID":      "proj-1",
					"SCION_WORKSPACE_MODE":  "empty-per-agent",
					"SCION_SOMETHING_OTHER": "x",
				},
			})
			if err != nil {
				t.Fatalf("Start failed: %v", err)
			}
			if !ran {
				t.Fatal("runtime Run was not called")
			}
			want := filepath.Join(projectScionDir, "agents", "worker", "workspace")
			if captured.Workspace != want {
				t.Fatalf("Workspace = %q, want %q", captured.Workspace, want)
			}
			if captured.RepoRoot != "" {
				t.Fatalf("RepoRoot = %q, want empty (empty-per-agent is never a git checkout)", captured.RepoRoot)
			}
			for _, kv := range captured.Env {
				if strings.HasPrefix(kv, "SCION_WORKSPACE_GIT=") {
					t.Fatalf("unexpected %s in container env", kv)
				}
			}
		})
	}
}

// TestStart_EmptyPerAgent_PersistedModeWithoutFlag pins review N1 of #2760:
// a restart whose request lost the mode (no flag, no SCION_WORKSPACE_MODE)
// still mounts the private workspace from the mode persisted at provision,
// never the enclosing repo root, and the container gets the mode env.
func TestStart_EmptyPerAgent_PersistedModeWithoutFlag(t *testing.T) {
	tmpDir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Chdir(tmpDir)
	t.Setenv("HOME", tmpDir)
	projectRoot := filepath.Join(tmpDir, "project")
	gitInitForTest(t, projectRoot)
	projectScionDir := setupEmptyPerAgentStartProject(t, projectRoot, "")

	var captured runtime.RunConfig
	var ran bool
	mgr := NewManager(emptyPerAgentStartRuntime(&captured, &ran))
	env := map[string]string{"SCION_AGENT_ID": "agent-1", "SCION_PROJECT_ID": "proj-1"}
	if _, err := mgr.Start(context.Background(), api.StartOptions{
		Name: "worker", ProjectPath: projectScionDir, NoAuth: true,
		EmptyPerAgentWorkspace: true, Env: env,
	}); err != nil {
		t.Fatalf("first Start failed: %v", err)
	}

	captured, ran = runtime.RunConfig{}, false
	if _, err := mgr.Start(context.Background(), api.StartOptions{
		Name: "worker", ProjectPath: projectScionDir, NoAuth: true, Env: env,
	}); err != nil {
		t.Fatalf("restart without the mode failed: %v", err)
	}
	if !ran {
		t.Fatal("runtime Run was not called")
	}
	want := filepath.Join(projectScionDir, "agents", "worker", "workspace")
	if captured.Workspace != want {
		t.Fatalf("Workspace = %q, want %q", captured.Workspace, want)
	}
	if captured.RepoRoot != "" {
		t.Fatalf("RepoRoot = %q, want empty", captured.RepoRoot)
	}
	hasMode := false
	for _, kv := range captured.Env {
		if kv == "SCION_WORKSPACE_MODE=empty-per-agent" {
			hasMode = true
		}
		if strings.HasPrefix(kv, "SCION_WORKSPACE_GIT=") {
			t.Fatalf("unexpected %s in container env", kv)
		}
	}
	if !hasMode {
		t.Fatalf("container env must carry SCION_WORKSPACE_MODE=empty-per-agent, got %v", captured.Env)
	}
	if _, ok := env["SCION_WORKSPACE_MODE"]; ok {
		t.Fatal("Start must not mutate the caller's Env map")
	}
}

// TestStart_EmptyPerAgent_NFSFailsClosed pins that empty-per-agent on NFS
// workspace storage with a runtime other than Kubernetes is refused before
// anything is started: only the Kubernetes runtime mounts the agent's own
// directory on the export (design #2703 P3), so any other runtime would
// get the project's shared workspace path.
func TestStart_EmptyPerAgent_NFSFailsClosed(t *testing.T) {
	tmpDir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Chdir(tmpDir)
	t.Setenv("HOME", tmpDir)
	storage := fmt.Sprintf(`  workspace_storage:
    backend: nfs
    nfs:
      mount_root: %s
      shares:
        - id: share-1
          server: 10.0.0.2
          export: /scion-workspaces
          pv_name: scion-workspaces-pv
`, filepath.Join(tmpDir, "nfs"))
	projectScionDir := setupEmptyPerAgentStartProject(t, filepath.Join(tmpDir, "project"), storage)

	var captured runtime.RunConfig
	var ran bool
	mgr := NewManager(emptyPerAgentStartRuntime(&captured, &ran))
	_, err = mgr.Start(context.Background(), api.StartOptions{
		Name:        "worker",
		ProjectPath: projectScionDir,
		NoAuth:      true,
		Env: map[string]string{
			"SCION_AGENT_ID":       "agent-1",
			"SCION_PROJECT_ID":     "proj-1",
			"SCION_WORKSPACE_MODE": "empty-per-agent",
		},
	})
	if !errors.Is(err, errEmptyPerAgentNFSRuntime) {
		t.Fatalf("Start error = %v, want errEmptyPerAgentNFSRuntime", err)
	}
	if ran {
		t.Fatalf("runtime Run must not be called; got workspace %q backend %q", captured.Workspace, captured.WorkspaceBackendName)
	}
}

// TestReprovision_EmptyPerAgent_RefusedWorkspaceUntouched pins that
// reincarnate, which supports only clone-per-agent and explicit-mount
// agents, refuses an empty-per-agent agent cleanly (409 at the handler)
// and leaves its private workspace as it was.
func TestReprovision_EmptyPerAgent_RefusedWorkspaceUntouched(t *testing.T) {
	tmpDir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Chdir(tmpDir)
	t.Setenv("HOME", tmpDir)
	projectScionDir := setupEmptyPerAgentStartProject(t, filepath.Join(tmpDir, "project"), "")
	ws := filepath.Join(projectScionDir, "agents", "worker", "workspace")
	if err := os.MkdirAll(ws, 0755); err != nil {
		t.Fatal(err)
	}
	note := filepath.Join(ws, "notes.md")
	if err := os.WriteFile(note, []byte("keep"), 0644); err != nil {
		t.Fatal(err)
	}

	var captured runtime.RunConfig
	var ran bool
	_, err = NewManager(emptyPerAgentStartRuntime(&captured, &ran)).Reprovision(context.Background(), api.StartOptions{
		Name:                   "worker",
		ProjectPath:            projectScionDir,
		NoAuth:                 true,
		EmptyPerAgentWorkspace: true,
	})
	if !errors.Is(err, ErrReprovisionRefused) {
		t.Fatalf("Reprovision error = %v, want ErrReprovisionRefused", err)
	}
	if got, err := os.ReadFile(note); err != nil || string(got) != "keep" {
		t.Fatalf("workspace content changed: %q, %v", got, err)
	}
}
