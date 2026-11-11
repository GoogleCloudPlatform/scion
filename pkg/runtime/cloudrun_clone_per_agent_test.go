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

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
)

// clonePerAgentEnv is the env a clone-per-agent start sends the runtime.
var clonePerAgentEnv = []string{"SCION_WORKSPACE_MODE=clone-per-agent", "SCION_GIT_CLONE_URL=https://example.com/org/repo.git"}

func TestIsClonePerAgentRun(t *testing.T) {
	gc := &api.GitCloneConfig{URL: "https://example.com/org/repo.git"}
	tests := []struct {
		name string
		cfg  RunConfig
		want bool
	}{
		{"clone-per-agent with a clone", RunConfig{Env: clonePerAgentEnv, GitClone: gc}, true},
		{"clone-per-agent without a clone", RunConfig{Env: clonePerAgentEnv}, false},
		{"shared-plain with a clone (an existing agent kept on the shared checkout)", RunConfig{Env: []string{"SCION_WORKSPACE_MODE=shared-plain"}, GitClone: gc}, false},
		{"empty-per-agent", RunConfig{Env: []string{"SCION_WORKSPACE_MODE=empty-per-agent"}, GitClone: gc}, false},
		{"no mode", RunConfig{GitClone: gc}, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := isClonePerAgentRun(tc.cfg); got != tc.want {
				t.Errorf("isClonePerAgentRun() = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestCloudRunAgentWorkspaceRel(t *testing.T) {
	got, err := cloudRunAgentWorkspaceRel("projects/proj-123/workspace", "my-agent")
	if err != nil {
		t.Fatal(err)
	}
	// The Kubernetes agent directory layout, which the broker's delete path
	// (pkg/agent removeNFSAgentWorkspace) removes.
	if want := "projects/proj-123/agents/my-agent/workspace"; got != want {
		t.Errorf("cloudRunAgentWorkspaceRel() = %q, want %q", got, want)
	}
	for _, slug := range []string{"", "..", "a/b", "My-Agent"} {
		if _, err := cloudRunAgentWorkspaceRel("projects/proj-123/workspace", slug); err == nil {
			t.Errorf("cloudRunAgentWorkspaceRel(slug %q) = nil error, want refusal", slug)
		}
	}
}

// TestCloudRunProvisionNFS_ClonePerAgentOwnWorkspace checks that a
// clone-per-agent agent gets its own empty workspace on the share,
// agents/<slug>/workspace, mounted at /workspace with its home and secrets
// unchanged, that the project's shared workspace is not cloned, and that a
// second provision (a restart) keeps what the agent wrote. The clone URL
// points nowhere, so the same start in shared-plain mode fails in the
// shared clone: the control that the skip is what makes it pass.
func TestCloudRunProvisionNFS_ClonePerAgentOwnWorkspace(t *testing.T) {
	orig := cloudRunRequireNFSFilesystem
	cloudRunRequireNFSFilesystem = func(string) error { return nil }
	t.Cleanup(func() { cloudRunRequireNFSFilesystem = orig })

	r := &CloudRunRuntime{config: &config.CloudRunConfig{
		ProjectID: "gcp-project",
		Location:  "us-central1",
		NFSServer: "10.0.0.2",
		NFSExport: "/scion-workspaces",
	}}
	unreachable := &api.GitCloneConfig{URL: "file:///nonexistent/scion-test-repo.git"}

	t.Run("clone-per-agent", func(t *testing.T) {
		share := t.TempDir()
		workspace := filepath.Join(share, "projects", "proj-123", "workspace")
		cfg := RunConfig{
			Labels:               crTestLabels,
			WorkspaceBackendName: "nfs",
			ProjectID:            "proj-123",
			Workspace:            workspace,
			Env:                  clonePerAgentEnv,
			GitClone:             unreachable,
		}
		paths, err := r.provisionCloudRunNFS(context.Background(), cfg, crTestAgentID, os.Getuid(), os.Getgid())
		if err != nil {
			t.Fatalf("provisionCloudRunNFS: %v", err)
		}
		wantExport := "/scion-workspaces/projects/proj-123/agents/" + crTestSlug + "/workspace"
		if paths.workspaceExportPath != wantExport {
			t.Errorf("workspace export path = %q, want %q", paths.workspaceExportPath, wantExport)
		}
		wantHost := filepath.Join(share, "projects", "proj-123", "agents", crTestSlug, "workspace")
		if paths.workspaceHostPath != wantHost {
			t.Errorf("workspace host path = %q, want %q", paths.workspaceHostPath, wantHost)
		}
		if entries, err := os.ReadDir(wantHost); err != nil || len(entries) != 0 {
			t.Errorf("agent workspace %s: %d entries, err %v; want an empty directory", wantHost, len(entries), err)
		}
		for _, dir := range []string{paths.homeHostPath, paths.secretsHostPath} {
			if fi, err := os.Stat(dir); err != nil || !fi.IsDir() {
				t.Errorf("agent directory %s not created: %v", dir, err)
			}
		}
		if entries, _ := os.ReadDir(workspace); len(entries) != 0 {
			t.Errorf("shared workspace %s has %d entries, want none (no shared clone)", workspace, len(entries))
		}

		// The instance mounts the agent's own workspace at /workspace and
		// keeps home and secrets on the share.
		rt, err := NewCloudRunRuntime(&config.CloudRunConfig{ProjectID: "p", Location: "us-central1"})
		if err != nil {
			t.Fatal(err)
		}
		inst := rt.buildCloudRunInstance(RunConfig{Image: "img", Labels: map[string]string{"agent_id": "a"}}, 1000, 1000, paths)
		got := map[string]string{}
		for _, v := range inst.Volumes {
			got[v.Name] = v.GetNfs().GetPath()
		}
		if got["workspace"] != wantExport || got["home"] != paths.homeExportPath || got["secrets"] != paths.secretsExportPath {
			t.Errorf("instance NFS volumes = %v, want workspace %s with home and secrets unchanged", got, wantExport)
		}

		// A restart provisions again and keeps the agent's work.
		if err := os.WriteFile(filepath.Join(wantHost, "work.txt"), []byte("kept"), 0o644); err != nil {
			t.Fatal(err)
		}
		if _, err := r.provisionCloudRunNFS(context.Background(), cfg, crTestAgentID, os.Getuid(), os.Getgid()); err != nil {
			t.Fatalf("second provisionCloudRunNFS: %v", err)
		}
		if b, err := os.ReadFile(filepath.Join(wantHost, "work.txt")); err != nil || string(b) != "kept" {
			t.Errorf("agent work after a restart = %q, %v; want it kept", b, err)
		}
	})

	t.Run("shared-plain control", func(t *testing.T) {
		share := t.TempDir()
		_, err := r.provisionCloudRunNFS(context.Background(), RunConfig{
			Labels:               crTestLabels,
			WorkspaceBackendName: "nfs",
			ProjectID:            "proj-123",
			Workspace:            filepath.Join(share, "projects", "proj-123", "workspace"),
			Env:                  []string{"SCION_WORKSPACE_MODE=shared-plain"},
			GitClone:             unreachable,
		}, crTestAgentID, os.Getuid(), os.Getgid())
		if err == nil || !strings.Contains(err.Error(), "provision NFS workspace") {
			t.Fatalf("provisionCloudRunNFS error = %v, want the shared workspace clone to fail", err)
		}
		if _, err := os.Stat(filepath.Join(share, "projects", "proj-123", "agents", crTestSlug, "workspace")); err == nil {
			t.Error("shared-plain created an agent workspace")
		}
	})
}
