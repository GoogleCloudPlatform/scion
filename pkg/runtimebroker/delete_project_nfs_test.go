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
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
	"github.com/GoogleCloudPlatform/scion/pkg/config"
)

// Project delete on a broker with NFS workspace storage: the broker removes
// its own project directory; the project's tree on the export is the hub's
// to remove (ptone/scion#5835), so the broker leaves it.

// newNFSDeleteTestServer returns a broker whose workspace storage is an NFS
// share mounted at <tmp>/mnt/share1, and that share's subpath root.
func newNFSDeleteTestServer(t *testing.T) (*Server, string, string) {
	t.Helper()
	srv, home := newScopeTestServer(t, &filteringMockManager{})
	mountRoot := filepath.Join(t.TempDir(), "mnt")
	srv.config.NFSConfig = &config.V1NFSConfig{
		MountRoot:   mountRoot,
		SubPathRoot: "projects",
		Shares:      []config.V1NFSShare{{ID: "share1", Server: "10.0.0.2", Export: "/ws"}},
	}
	subRoot := filepath.Join(mountRoot, "share1", "projects")
	if err := os.MkdirAll(subRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	return srv, home, subRoot
}

// seedNFSProjectTree creates the layout an NFS project gets on the export:
// workspace, shared-dirs, provision (sentinel and lock), worktrees and an
// agent directory. It returns the project directory.
func seedNFSProjectTree(t *testing.T, subRoot, projectID string) string {
	t.Helper()
	dir := filepath.Join(subRoot, projectID)
	for _, d := range []string{"workspace/.git", "shared-dirs/scratch", "provision", "worktrees/dev", "agents/dev/workspace"} {
		if err := os.MkdirAll(filepath.Join(dir, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	for _, f := range []string{"workspace/README.md", "provision/.scion-provisioned", "provision/lock"} {
		if err := os.WriteFile(filepath.Join(dir, f), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

// The broker removes its local project directory and leaves the project's
// tree on the export, and every other project's, to the hub
// (ptone/scion#5835), with or without a local directory.
func TestDeleteProject_NFS_LeavesExportTreeToHub(t *testing.T) {
	srv, home, subRoot := newNFSDeleteTestServer(t)
	makeHubProject(t, home, "proj-a", scopeProjA, "dev")
	treeA := seedNFSProjectTree(t, subRoot, scopeProjA)
	treeB := seedNFSProjectTree(t, subRoot, scopeProjB)

	rec := doDeleteProject(t, srv, "proj-a", scopeProjA)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("expected 204, got %d: %s", rec.Code, rec.Body.String())
	}
	assertGone(t, filepath.Join(home, ".scion", "projects", "proj-a"))
	assertPresent(t, filepath.Join(treeA, "workspace", "README.md"))
	assertPresent(t, filepath.Join(treeA, "agents", "dev", "workspace"))
	assertPresent(t, filepath.Join(treeB, "provision", "lock"))

	// Without a local project directory (a git project whose agents run on
	// Kubernetes) the tree is left alone too.
	rec = doDeleteProject(t, srv, "proj-b", scopeProjB)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("expected 204, got %d: %s", rec.Code, rec.Body.String())
	}
	assertPresent(t, filepath.Join(treeB, "workspace", "README.md"))
}

func TestDeleteProject_NFS_MissingDirsAreSuccess(t *testing.T) {
	srv, _, _ := newNFSDeleteTestServer(t)
	rec := doDeleteProject(t, srv, "proj-a", scopeProjA)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("expected 204, got %d: %s", rec.Code, rec.Body.String())
	}
}

// The slug directory is left alone when its .scion entry names a different
// project than the one being deleted (a re-created project with the same
// slug). The deleted project's ID-keyed NFS tree is the hub's to remove.
func TestDeleteProject_SlugDirOfOtherProject_Kept(t *testing.T) {
	srv, home, subRoot := newNFSDeleteTestServer(t)
	makeHubProject(t, home, "proj-a", scopeProjB, "dev")
	treeA := seedNFSProjectTree(t, subRoot, scopeProjA)

	rec := doDeleteProject(t, srv, "proj-a", scopeProjA)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("expected 204, got %d: %s", rec.Code, rec.Body.String())
	}
	assertPresent(t, filepath.Join(home, ".scion", "projects", "proj-a", ".scion"))
	assertPresent(t, filepath.Join(treeA, "workspace", "README.md"))
}

// The same holds when only the broker's workspace record names the other
// project.
func TestDeleteProject_SlugDirRecordedForOtherProject_Kept(t *testing.T) {
	srv, home, _ := newNFSDeleteTestServer(t)
	dir := filepath.Join(home, ".scion", "projects", "proj-a")
	if err := os.MkdirAll(filepath.Join(dir, "work"), 0o755); err != nil {
		t.Fatal(err)
	}
	record, err := config.BrokerWorkspaceRecordPath("proj-a")
	if err != nil {
		t.Fatal(err)
	}
	if err := config.WriteWorkspaceRecord(record, scopeProjB); err != nil {
		t.Fatal(err)
	}

	rec := doDeleteProject(t, srv, "proj-a", scopeProjA)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("expected 204, got %d: %s", rec.Code, rec.Body.String())
	}
	assertPresent(t, filepath.Join(dir, "work"))
}

// A matching workspace record does not block removal.
func TestDeleteProject_SlugDirRecordedForSameProject_Removed(t *testing.T) {
	srv, home, _ := newNFSDeleteTestServer(t)
	dir := filepath.Join(home, ".scion", "projects", "proj-a")
	if err := os.MkdirAll(filepath.Join(dir, "work"), 0o755); err != nil {
		t.Fatal(err)
	}
	record, err := config.BrokerWorkspaceRecordPath("proj-a")
	if err != nil {
		t.Fatal(err)
	}
	if err := config.WriteWorkspaceRecord(record, scopeProjA); err != nil {
		t.Fatal(err)
	}

	rec := doDeleteProject(t, srv, "proj-a", scopeProjA)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("expected 204, got %d: %s", rec.Code, rec.Body.String())
	}
	assertGone(t, dir)
}

// A broker workspace record decides over the .scion entry: a broker copy
// whose identity alignment was skipped holds another ID in .scion but the
// hub ID in its record, and must still be removed.
func TestDeleteProject_RecordMatchesDespiteOtherScionEntry_Removed(t *testing.T) {
	srv, home, _ := newNFSDeleteTestServer(t)
	makeHubProject(t, home, "proj-a", scopeProjB, "dev")
	record, err := config.BrokerWorkspaceRecordPath("proj-a")
	if err != nil {
		t.Fatal(err)
	}
	if err := config.WriteWorkspaceRecord(record, scopeProjA); err != nil {
		t.Fatal(err)
	}

	rec := doDeleteProject(t, srv, "proj-a", scopeProjA)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("expected 204, got %d: %s", rec.Code, rec.Body.String())
	}
	assertGone(t, filepath.Join(home, ".scion", "projects", "proj-a"))
}

func TestAgentKey(t *testing.T) {
	cases := []struct {
		name string
		a    api.AgentInfo
		want string
	}{
		{"agent_id label first", api.AgentInfo{ID: "x", ContainerID: "c", Labels: map[string]string{"agent_id": "hub-1"}}, "hub-1"},
		{"k8s operation id", api.AgentInfo{ID: "x", ContainerID: "pod", Kubernetes: &api.AgentK8sMetadata{Namespace: "ns"}}, "ns/pod"},
		{"container id", api.AgentInfo{ID: "x", ContainerID: "c1"}, "c1"},
		{"AgentInfo.ID last", api.AgentInfo{ID: "x"}, "x"},
		{"none", api.AgentInfo{}, ""},
	}
	for _, tc := range cases {
		if got := agentKey(tc.a); got != tc.want {
			t.Errorf("%s: agentKey = %q, want %q", tc.name, got, tc.want)
		}
	}
}
