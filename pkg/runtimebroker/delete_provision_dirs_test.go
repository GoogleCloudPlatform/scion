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
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/agent"
	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
	"github.com/GoogleCloudPlatform/scion/pkg/runtime"
)

// Tests for ptone/scion#2839: an agent delete must remove the agent's
// provision directory for every hub-native project layout.

// makeHubNativeMarkerProject creates a hub-native project without git, as
// the broker does on first dispatch: ~/.scion/projects/<slug>/.scion is a
// marker FILE, and the agent's directory lives in the external config dir
// ~/.scion/project-configs/<slug>__<short-id>/.scion/agents/<name>. The
// agent is provisioned but never started (no container). It returns the
// external .scion dir and the agent dir.
func makeHubNativeMarkerProject(t *testing.T, home, slug, projectID, name string) (string, string) {
	t.Helper()
	projectRoot := filepath.Join(home, ".scion", "projects", slug)
	if err := os.MkdirAll(projectRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	marker := &config.ProjectMarker{ProjectID: projectID, ProjectName: slug, ProjectSlug: slug}
	if err := config.WriteProjectMarker(filepath.Join(projectRoot, config.DotScion), marker); err != nil {
		t.Fatal(err)
	}
	extDir, err := marker.ExternalProjectPath()
	if err != nil {
		t.Fatal(err)
	}
	agentDir := filepath.Join(extDir, "agents", name)
	if err := os.MkdirAll(filepath.Join(agentDir, "home"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(agentDir, "scion-agent.json"), []byte(`{}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(agentDir, "home", "agent-info.json"), []byte(`{"name":"`+name+`","phase":"created"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	return extDir, agentDir
}

// Shape 1, resolution: a delete of a provisioned-never-started agent in a
// marker-file hub-native project resolves the external agents dir (it used
// to 404, leaving the dir behind), and only for the matching project.
func TestFindAgentInHubManagedProjects_MarkerFileProject(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	extDir, _ := makeHubNativeMarkerProject(t, home, "proj-b", scopeProjB, "dev")

	for _, tc := range []struct {
		name, projectID, want string
	}{
		{"matching project", scopeProjB, extDir},
		{"no project (solo)", "", extDir},
		{"other project", scopeProjA, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := findAgentInHubManagedProjects("dev", tc.projectID)
			if err != nil {
				t.Fatalf("findAgentInHubManagedProjects: %v", err)
			}
			if got != tc.want {
				t.Errorf("got %q, want %q", got, tc.want)
			}
		})
	}
	if got, err := findAgentInHubManagedProjects("other-agent", scopeProjB); err != nil || got != "" {
		t.Errorf("an agent the project does not hold: got (%q, %v), want none", got, err)
	}
}

// Shape 1, end to end through the broker's delete handler with a real
// agent.Manager: the provision dir is removed and the delete answers 204.
func TestDeleteAgent_MarkerFileHubNativeProject_ProvisionedNeverStarted_RemovesDir(t *testing.T) {
	var entries []api.AgentInfo
	var deleted []string
	srv, home := newProvisionDirsServer(t, &entries, &deleted)
	extDir, agentDir := makeHubNativeMarkerProject(t, home, "proj-b", scopeProjB, "dev")
	// A same-named agent in another marker project must survive.
	_, otherDir := makeHubNativeMarkerProject(t, home, "proj-a", scopeProjA, "dev")

	rec := doDelete(t, srv, "dev", "projectId="+scopeProjB+"&deleteFiles=true&removeBranch=true")
	if rec.Code != http.StatusNoContent {
		t.Fatalf("expected 204, got %d: %s", rec.Code, rec.Body.String())
	}
	if _, err := os.Stat(agentDir); !os.IsNotExist(err) {
		t.Fatalf("the provision dir %s was left behind (stat err %v)", agentDir, err)
	}
	if _, err := os.Stat(filepath.Join(extDir, "agents")); err != nil {
		t.Errorf("the project's agents dir itself was removed: %v", err)
	}
	if _, err := os.Stat(otherDir); err != nil {
		t.Errorf("the other project's same-named agent was touched: %v", err)
	}
	if len(deleted) != 0 {
		t.Errorf("runtime deletes = %v, want none (no container)", deleted)
	}
}

// newProvisionDirsServer builds a broker over a real agent.Manager whose
// runtime lists entries (filtered by label) and records deletes, with HOME
// and CWD isolated.
func newProvisionDirsServer(t *testing.T, entries *[]api.AgentInfo, deleted *[]string) (*Server, string) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Chdir(t.TempDir())
	rt := &runtime.MockRuntime{
		NameFunc: func() string { return "docker" },
		ListFunc: func(_ context.Context, filter map[string]string) ([]api.AgentInfo, error) {
			var out []api.AgentInfo
			for _, e := range *entries {
				ok := true
				for k, v := range filter {
					if e.Labels[k] != v {
						ok = false
						break
					}
				}
				if ok {
					out = append(out, e)
				}
			}
			return out, nil
		},
		DeleteFunc: func(_ context.Context, ref runtime.RunRef) error {
			*deleted = append(*deleted, ref.ID)
			return nil
		},
	}
	cfg := DefaultServerConfig()
	cfg.BrokerID = "test-broker-id"
	cfg.BrokerName = "test-host"
	return New(cfg, agent.NewManager(rt), rt), home
}

// Shape 2: a started agent of a hub-native project with git, provisioned
// where the broker resolves the project by slug
// (~/.scion/projects/<slug>/.scion, a directory with a project-id file):
// the delete removes its directory. (The agent used to land under the
// broker's global ~/.scion when the provider recorded that path; the
// provide fix keeps it out of there, and the trust check still refuses a
// global path for a non-global project.)
func TestDeleteAgent_GitHubNativeProject_StartedAgent_RemovesDir(t *testing.T) {
	var entries []api.AgentInfo
	var deleted []string
	srv, home := newProvisionDirsServer(t, &entries, &deleted)
	scionDir := filepath.Join(home, ".scion", "projects", "proj-b", ".scion")
	if err := os.MkdirAll(scionDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := config.WriteProjectID(scionDir, scopeProjB); err != nil {
		t.Fatal(err)
	}
	agentHome := config.GetAgentHomePath(scionDir, "dev")
	agentDir := filepath.Dir(agentHome)
	if err := os.MkdirAll(agentHome, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(agentHome, "agent-info.json"), []byte(`{"name":"dev","phase":"running"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	entries = []api.AgentInfo{{
		Name: "dev", ContainerID: "cid-b", ProjectID: scopeProjB, ProjectPath: scionDir,
		Labels: map[string]string{"scion.agent": "true", "scion.name": "dev", "scion.project_id": scopeProjB},
	}}

	rec := doDelete(t, srv, "dev", "projectId="+scopeProjB+"&deleteFiles=true")
	if rec.Code != http.StatusNoContent {
		t.Fatalf("expected 204, got %d: %s", rec.Code, rec.Body.String())
	}
	if len(deleted) != 1 || deleted[0] != "cid-b" {
		t.Errorf("runtime deletes = %v, want cid-b", deleted)
	}
	if _, err := os.Stat(agentDir); !os.IsNotExist(err) {
		t.Fatalf("the agent dir %s was left behind (stat err %v)", agentDir, err)
	}
}

// The trust check is unchanged: a runtime entry claiming the broker's
// global dir as a non-global project's path never directs file deletion
// there.
func TestDeleteAgent_GlobalDirPathForProject_FilesUntouched(t *testing.T) {
	var entries []api.AgentInfo
	var deleted []string
	srv, home := newProvisionDirsServer(t, &entries, &deleted)
	globalDir := filepath.Join(home, ".scion")
	globalAgentDir := filepath.Join(globalDir, "agents", "dev")
	if err := os.MkdirAll(filepath.Join(globalAgentDir, "home"), 0o755); err != nil {
		t.Fatal(err)
	}
	entries = []api.AgentInfo{{
		Name: "dev", ContainerID: "cid-b", ProjectID: scopeProjB, ProjectPath: globalDir,
		Labels: map[string]string{"scion.agent": "true", "scion.name": "dev", "scion.project_id": scopeProjB},
	}}

	rec := doDelete(t, srv, "dev", "projectId="+scopeProjB+"&deleteFiles=true")
	if rec.Code != http.StatusNoContent && rec.Code != http.StatusNotFound {
		t.Fatalf("unexpected status %d: %s", rec.Code, rec.Body.String())
	}
	if _, err := os.Stat(globalAgentDir); err != nil {
		t.Fatalf("a non-global project's delete removed files in the global dir: %v", err)
	}
}
