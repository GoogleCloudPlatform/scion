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

	"github.com/GoogleCloudPlatform/scion/pkg/config"
)

// Tests for ptone/scion#2569: deleting a project on a broker with NFS
// workspace storage also removes the project's tree on the export.

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

func TestDeleteProject_NFS_RemovesExportTreeAndLocalDir(t *testing.T) {
	srv, home, subRoot := newNFSDeleteTestServer(t)
	makeHubProject(t, home, "proj-a", scopeProjA, "dev")
	treeA := seedNFSProjectTree(t, subRoot, scopeProjA)
	treeB := seedNFSProjectTree(t, subRoot, scopeProjB)

	rec := doDeleteProject(t, srv, "proj-a", scopeProjA)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("expected 204, got %d: %s", rec.Code, rec.Body.String())
	}
	assertGone(t, treeA)
	assertGone(t, filepath.Join(home, ".scion", "projects", "proj-a"))
	assertPresent(t, filepath.Join(treeB, "workspace", "README.md"))
	assertPresent(t, filepath.Join(treeB, "provision", "lock"))
	assertPresent(t, subRoot)
}

// A git project whose agents run on Kubernetes may have no local project
// directory on the broker; its export tree must still be removed.
func TestDeleteProject_NFS_RemovesExportTreeWithoutLocalDir(t *testing.T) {
	srv, home, subRoot := newNFSDeleteTestServer(t)
	treeA := seedNFSProjectTree(t, subRoot, scopeProjA)

	rec := doDeleteProject(t, srv, "proj-a", scopeProjA)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("expected 204, got %d: %s", rec.Code, rec.Body.String())
	}
	assertGone(t, treeA)
	assertGone(t, filepath.Join(home, ".scion", "projects", "proj-a"))
}

func TestDeleteProject_NFS_MissingDirsAreSuccess(t *testing.T) {
	srv, _, _ := newNFSDeleteTestServer(t)
	rec := doDeleteProject(t, srv, "proj-a", scopeProjA)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("expected 204, got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestDeleteProject_NFS_ShareNotMountedIsSuccess(t *testing.T) {
	srv, home, _ := newNFSDeleteTestServer(t)
	srv.config.NFSConfig.MountRoot = filepath.Join(t.TempDir(), "absent")
	makeHubProject(t, home, "proj-a", scopeProjA, "dev")
	rec := doDeleteProject(t, srv, "proj-a", scopeProjA)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("expected 204, got %d: %s", rec.Code, rec.Body.String())
	}
	assertGone(t, filepath.Join(home, ".scion", "projects", "proj-a"))
}

// A cleanup the guard refuses is logged, not returned: the delete still
// succeeds and the local project directory is still removed.
func TestDeleteProject_NFS_CleanupErrorDoesNotFailDelete(t *testing.T) {
	srv, home, subRoot := newNFSDeleteTestServer(t)
	makeHubProject(t, home, "proj-a", scopeProjA, "dev")
	treeB := seedNFSProjectTree(t, subRoot, scopeProjB)
	link := filepath.Join(subRoot, scopeProjA)
	if err := os.Symlink(treeB, link); err != nil {
		t.Fatal(err)
	}

	rec := doDeleteProject(t, srv, "proj-a", scopeProjA)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("expected 204, got %d: %s", rec.Code, rec.Body.String())
	}
	assertGone(t, filepath.Join(home, ".scion", "projects", "proj-a"))
	if _, err := os.Lstat(link); err != nil {
		t.Errorf("refused symlink should be left in place: %v", err)
	}
	assertPresent(t, filepath.Join(treeB, "workspace", "README.md"))
}

func TestDeleteProject_NFS_NoProjectIDKeepsExportTree(t *testing.T) {
	srv, home, subRoot := newNFSDeleteTestServer(t)
	makeHubProject(t, home, "proj-a", scopeProjA, "dev")
	treeA := seedNFSProjectTree(t, subRoot, scopeProjA)

	rec := doDeleteProject(t, srv, "proj-a", "")
	if rec.Code != http.StatusNoContent {
		t.Fatalf("expected 204, got %d: %s", rec.Code, rec.Body.String())
	}
	assertPresent(t, filepath.Join(treeA, "workspace", "README.md"))
}

func TestDeleteProject_NoNFSConfigLeavesExportAlone(t *testing.T) {
	srv, home, subRoot := newNFSDeleteTestServer(t)
	srv.config.NFSConfig = nil
	makeHubProject(t, home, "proj-a", scopeProjA, "dev")
	treeA := seedNFSProjectTree(t, subRoot, scopeProjA)

	rec := doDeleteProject(t, srv, "proj-a", scopeProjA)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("expected 204, got %d: %s", rec.Code, rec.Body.String())
	}
	assertPresent(t, filepath.Join(treeA, "workspace", "README.md"))
	assertGone(t, filepath.Join(home, ".scion", "projects", "proj-a"))
}
