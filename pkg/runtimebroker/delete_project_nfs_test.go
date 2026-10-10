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
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

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

// The slug directory is left alone when its .scion entry names a different
// project than the one being deleted (a re-created project with the same
// slug); the deleted project's ID-keyed NFS tree is still removed.
func TestDeleteProject_SlugDirOfOtherProject_Kept(t *testing.T) {
	srv, home, subRoot := newNFSDeleteTestServer(t)
	makeHubProject(t, home, "proj-a", scopeProjB, "dev")
	treeA := seedNFSProjectTree(t, subRoot, scopeProjA)

	rec := doDeleteProject(t, srv, "proj-a", scopeProjA)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("expected 204, got %d: %s", rec.Code, rec.Body.String())
	}
	assertPresent(t, filepath.Join(home, ".scion", "projects", "proj-a", ".scion"))
	assertGone(t, treeA)
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

// The NFS removal runs in the background: the response does not wait for a
// failed removal's retry, and the cleanup's own timeout ends the wait.
func TestDeleteProject_NFS_ResponseDoesNotWaitForRetry(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory permissions")
	}
	srv, home, subRoot := newNFSDeleteTestServer(t)
	makeHubProject(t, home, "proj-a", scopeProjA, "dev")
	treeA := seedNFSProjectTree(t, subRoot, scopeProjA)
	ws := filepath.Join(treeA, "workspace")
	if err := os.Chmod(ws, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(ws, 0o755) })

	origDelay, origTimeout := nfsProjectCleanupRetryDelay, nfsProjectCleanupTimeout
	const cleanupTimeout = 3 * time.Second
	nfsProjectCleanupRetryDelay, nfsProjectCleanupTimeout = time.Hour, cleanupTimeout
	t.Cleanup(func() { nfsProjectCleanupRetryDelay, nfsProjectCleanupTimeout = origDelay, origTimeout })

	start := time.Now()
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodDelete, "/api/v1/projects/proj-a?project_id="+scopeProjA, nil))
	responded := time.Since(start)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("expected 204, got %d: %s", rec.Code, rec.Body.String())
	}
	if responded >= cleanupTimeout/2 {
		t.Fatalf("response took %v: it waited for the NFS cleanup's retry", responded)
	}
	assertGone(t, filepath.Join(home, ".scion", "projects", "proj-a"))
	srv.nfsCleanupWG.Wait()
	if elapsed := time.Since(start); elapsed > time.Minute {
		t.Fatalf("background cleanup should end at its own timeout, took %v", elapsed)
	}
	assertPresent(t, filepath.Join(ws, "README.md"))
}

// A failed removal is retried once in the background and succeeds once the
// tree is removable.
func TestDeleteProject_NFS_RetriesFailedRemoval(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory permissions")
	}
	srv, _, subRoot := newNFSDeleteTestServer(t)
	treeA := seedNFSProjectTree(t, subRoot, scopeProjA)
	ws := filepath.Join(treeA, "workspace")
	if err := os.Chmod(ws, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(ws, 0o755) })

	origDelay := nfsProjectCleanupRetryDelay
	nfsProjectCleanupRetryDelay = 300 * time.Millisecond
	t.Cleanup(func() { nfsProjectCleanupRetryDelay = origDelay })

	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodDelete, "/api/v1/projects/proj-a?project_id="+scopeProjA, nil))
	if rec.Code != http.StatusNoContent {
		t.Fatalf("expected 204, got %d: %s", rec.Code, rec.Body.String())
	}
	// Make the tree removable after the first attempt has failed, before
	// the retry.
	time.Sleep(100 * time.Millisecond)
	if err := os.Chmod(ws, 0o755); err != nil {
		t.Fatal(err)
	}
	srv.nfsCleanupWG.Wait()
	assertGone(t, treeA)
}
