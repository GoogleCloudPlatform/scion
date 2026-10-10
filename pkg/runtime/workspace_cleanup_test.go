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
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
)

func testNFSCleanupConfig(t *testing.T) (*config.V1NFSConfig, string) {
	t.Helper()
	mountRoot := t.TempDir()
	cfg := &config.V1NFSConfig{
		MountRoot:   mountRoot,
		SubPathRoot: "projects",
		Shares: []config.V1NFSShare{
			{ID: "share1", Server: "10.0.0.2", Export: "/scion-workspaces"},
		},
	}
	return cfg, mountRoot
}

// createProjectSubtree creates a project subtree structure for testing.
func createProjectSubtree(t *testing.T, mountRoot, shareID, projectID string) string {
	t.Helper()
	projectPath := filepath.Join(mountRoot, shareID, "projects", projectID)
	wsPath := filepath.Join(projectPath, "workspace")
	sdPath := filepath.Join(projectPath, "shared-dirs", "data")

	if err := os.MkdirAll(wsPath, 0770); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(sdPath, 0770); err != nil {
		t.Fatal(err)
	}

	// Write some files to verify deletion.
	if err := os.WriteFile(filepath.Join(wsPath, "test.txt"), []byte("hello"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(projectPath, ".scion-provisioned"), []byte("test"), 0644); err != nil {
		t.Fatal(err)
	}

	return projectPath
}

// --- Basic cleanup ---

func TestCleanupNFSProject_RemovesSubtree(t *testing.T) {
	cfg, mountRoot := testNFSCleanupConfig(t)
	projectPath := createProjectSubtree(t, mountRoot, "share1", "proj-1")

	// Verify structure exists.
	if _, err := os.Stat(projectPath); err != nil {
		t.Fatalf("project subtree should exist: %v", err)
	}

	err := CleanupNFSProject(cfg, "proj-1")
	if err != nil {
		t.Fatalf("CleanupNFSProject: %v", err)
	}

	// Verify project subtree is gone.
	if _, err := os.Stat(projectPath); !os.IsNotExist(err) {
		t.Errorf("project subtree should be deleted, but still exists")
	}

	// Verify the share root still exists.
	shareRoot := filepath.Join(mountRoot, "share1")
	if _, err := os.Stat(shareRoot); err != nil {
		t.Errorf("share root should still exist: %v", err)
	}
}

// --- Idempotent: non-existent project is a no-op ---

func TestCleanupNFSProject_Idempotent(t *testing.T) {
	cfg, _ := testNFSCleanupConfig(t)

	// No subtree exists — should succeed silently.
	err := CleanupNFSProject(cfg, "nonexistent-project")
	if err != nil {
		t.Fatalf("cleanup of nonexistent project should be idempotent: %v", err)
	}
}

// --- Double cleanup ---

func TestCleanupNFSProject_DoubleCleanup(t *testing.T) {
	cfg, mountRoot := testNFSCleanupConfig(t)
	createProjectSubtree(t, mountRoot, "share1", "proj-double")

	if err := CleanupNFSProject(cfg, "proj-double"); err != nil {
		t.Fatalf("first cleanup: %v", err)
	}
	if err := CleanupNFSProject(cfg, "proj-double"); err != nil {
		t.Fatalf("second cleanup should be idempotent: %v", err)
	}
}

// --- Isolation: refuses share root ---

func TestCleanupNFSProject_RefusesShareRoot(t *testing.T) {
	cfg := &config.V1NFSConfig{
		MountRoot:   "/mnt/nfs",
		SubPathRoot: "",
		Shares: []config.V1NFSShare{
			{ID: "", Server: "10.0.0.2", Export: "/ws"},
		},
	}

	// An empty projectID would compute a path that equals the base.
	err := CleanupNFSProject(cfg, "")
	if err == nil {
		t.Fatal("should refuse empty project ID")
	}
}

// --- Isolation: refuses path traversal ---

func TestCleanupNFSProject_RefusesPathTraversal(t *testing.T) {
	cfg, _ := testNFSCleanupConfig(t)

	err := CleanupNFSProject(cfg, "../../etc")
	if err == nil {
		t.Fatal("should refuse path traversal")
	}
}

// --- Does not affect other projects ---

func TestCleanupNFSProject_IsolatesProjects(t *testing.T) {
	cfg, mountRoot := testNFSCleanupConfig(t)

	projAPath := createProjectSubtree(t, mountRoot, "share1", "proj-A")
	projBPath := createProjectSubtree(t, mountRoot, "share1", "proj-B")

	// Delete project A.
	if err := CleanupNFSProject(cfg, "proj-A"); err != nil {
		t.Fatalf("cleanup proj-A: %v", err)
	}

	// Project A is gone.
	if _, err := os.Stat(projAPath); !os.IsNotExist(err) {
		t.Error("proj-A should be deleted")
	}

	// Project B is untouched.
	if _, err := os.Stat(projBPath); err != nil {
		t.Errorf("proj-B should still exist: %v", err)
	}

	// Files in B are intact.
	bFile := filepath.Join(projBPath, "workspace", "test.txt")
	if _, err := os.Stat(bFile); err != nil {
		t.Errorf("proj-B workspace file should be intact: %v", err)
	}
}

// --- Nil config ---

func TestCleanupNFSProject_NilConfig(t *testing.T) {
	err := CleanupNFSProject(nil, "proj-1")
	if err == nil {
		t.Fatal("should error on nil config")
	}
}

// --- No shares ---

func TestCleanupNFSProject_NoShares(t *testing.T) {
	cfg := &config.V1NFSConfig{
		MountRoot: "/mnt/nfs",
		Shares:    nil,
	}
	err := CleanupNFSProject(cfg, "proj-1")
	if err == nil {
		t.Fatal("should error on no shares")
	}
}

// --- SubPathRoot defaults ---

func TestCleanupNFSProject_SubPathRootDefault(t *testing.T) {
	mountRoot := t.TempDir()
	cfg := &config.V1NFSConfig{
		MountRoot:   mountRoot,
		SubPathRoot: "", // should default to "projects"
		Shares: []config.V1NFSShare{
			{ID: "share1", Server: "10.0.0.2", Export: "/ws"},
		},
	}

	// Create the subtree at the default path.
	projectPath := createProjectSubtree(t, mountRoot, "share1", "proj-default")

	if err := CleanupNFSProject(cfg, "proj-default"); err != nil {
		t.Fatalf("cleanup with default SubPathRoot: %v", err)
	}

	if _, err := os.Stat(projectPath); !os.IsNotExist(err) {
		t.Error("project subtree should be deleted")
	}
}

// --- ptone/scion#2569: full project tree, path guard, missing dirs ---

func TestCleanupNFSProject_RemovesProvisionWorktreesAndAgentDirs(t *testing.T) {
	cfg, mountRoot := testNFSCleanupConfig(t)
	projectPath := createProjectSubtree(t, mountRoot, "share1", "proj-full")
	for _, d := range []string{"provision", "worktrees/agent-a", "agents/agent-b/workspace"} {
		if err := os.MkdirAll(filepath.Join(projectPath, d), 0o770); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(projectPath, "provision", "lock"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := CleanupNFSProject(cfg, "proj-full"); err != nil {
		t.Fatalf("CleanupNFSProject: %v", err)
	}
	if _, err := os.Lstat(projectPath); !os.IsNotExist(err) {
		t.Fatalf("project tree should be gone, Lstat err = %v", err)
	}
	if _, err := os.Stat(filepath.Join(mountRoot, "share1", "projects")); err != nil {
		t.Fatalf("subpath root should be kept: %v", err)
	}
}

func TestCleanupNFSProject_MissingShareHostBaseIsSuccess(t *testing.T) {
	cfg, _ := testNFSCleanupConfig(t)
	cfg.MountRoot = filepath.Join(cfg.MountRoot, "not-mounted")
	if err := CleanupNFSProject(cfg, "proj-1"); err != nil {
		t.Fatalf("missing share host base should be success: %v", err)
	}
}

func TestCleanupNFSProject_MissingSubPathRootIsSuccess(t *testing.T) {
	cfg, mountRoot := testNFSCleanupConfig(t)
	if err := os.MkdirAll(filepath.Join(mountRoot, "share1"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := CleanupNFSProject(cfg, "proj-1"); err != nil {
		t.Fatalf("missing subpath root should be success: %v", err)
	}
}

func TestCleanupNFSProject_RefusesSymlinkedSubPathRootOutsideShare(t *testing.T) {
	cfg, mountRoot := testNFSCleanupConfig(t)
	outside := t.TempDir()
	victim := filepath.Join(outside, "proj-1")
	if err := os.MkdirAll(victim, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(victim, "keep.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(mountRoot, "share1"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(mountRoot, "share1", "projects")); err != nil {
		t.Fatal(err)
	}
	if err := CleanupNFSProject(cfg, "proj-1"); err == nil {
		t.Fatal("expected refusal for a subpath root that resolves outside the share")
	}
	if _, err := os.Stat(filepath.Join(victim, "keep.txt")); err != nil {
		t.Fatalf("directory outside the share must be untouched: %v", err)
	}
}

func TestCleanupNFSProject_RefusesSymlinkedProjectDir(t *testing.T) {
	cfg, mountRoot := testNFSCleanupConfig(t)
	other := createProjectSubtree(t, mountRoot, "share1", "proj-other")
	link := filepath.Join(mountRoot, "share1", "projects", "proj-link")
	if err := os.Symlink(other, link); err != nil {
		t.Fatal(err)
	}
	if err := CleanupNFSProject(cfg, "proj-link"); err == nil {
		t.Fatal("expected refusal for a project directory that is a symlink")
	}
	if _, err := os.Lstat(link); err != nil {
		t.Fatalf("symlink should be left in place: %v", err)
	}
	if _, err := os.Stat(filepath.Join(other, "workspace", "test.txt")); err != nil {
		t.Fatalf("symlink target must be untouched: %v", err)
	}
}

func TestCleanupNFSProject_RefusesInvalidProjectIDs(t *testing.T) {
	cfg, mountRoot := testNFSCleanupConfig(t)
	createProjectSubtree(t, mountRoot, "share1", "proj-keep")
	for _, id := range []string{".", "..", "a/b", "/abs", "../share1", ".hidden"} {
		if err := CleanupNFSProject(cfg, id); err == nil {
			t.Errorf("project ID %q: expected an error", id)
		}
	}
	if _, err := os.Stat(filepath.Join(mountRoot, "share1", "projects", "proj-keep", "workspace", "test.txt")); err != nil {
		t.Fatalf("other project must be untouched: %v", err)
	}
}

func TestCleanupNFSProject_RefusesRootHostBase(t *testing.T) {
	cfg := &config.V1NFSConfig{
		MountRoot: "/",
		Shares:    []config.V1NFSShare{{ID: "", Server: "10.0.0.2", Export: "/ws"}},
	}
	if err := CleanupNFSProject(cfg, "proj-1"); err == nil {
		t.Fatal("expected refusal when the share host base is the filesystem root")
	}
}

func TestNFSProjectPathGuard(t *testing.T) {
	base := filepath.Join(t.TempDir(), "share1")
	cases := []struct {
		name                         string
		dir, hostBase, subRoot, proj string
		ok                           bool
	}{
		{"valid", filepath.Join(base, "projects", "p1"), base, "projects", "p1", true},
		{"valid multi-segment root", filepath.Join(base, "a", "b", "p1"), base, "a/b", "p1", true},
		{"relative host base", "share1/projects/p1", "share1", "projects", "p1", false},
		{"empty host base", "/projects/p1", "", "projects", "p1", false},
		{"root host base", "/projects/p1", "/", "projects", "p1", false},
		{"empty project", filepath.Join(base, "projects"), base, "projects", "", false},
		{"dot project", filepath.Join(base, "projects"), base, "projects", ".", false},
		{"dotdot project", base, base, "projects", "..", false},
		{"dotdot in subpath root", filepath.Join(base, "p1"), base, "x/..", "p1", false},
		{"empty subpath root segment", filepath.Join(base, "a", "p1"), base, "a//", "p1", false},
		{"sibling prefix", filepath.Join(base, "projects-other", "p1"), base, "projects", "p1", false},
		{"dir equals share root", base, base, "projects", "p1", false},
		{"dir is subpath root", filepath.Join(base, "projects"), base, "projects", "p1", false},
		{"dir outside share", filepath.Join(filepath.Dir(base), "elsewhere", "p1"), base, "projects", "p1", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := nfsProjectPathGuard(tc.dir, tc.hostBase, tc.subRoot, tc.proj)
			if tc.ok && err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if !tc.ok && err == nil {
				t.Fatal("expected an error")
			}
		})
	}
}

func TestNFSProjectHostPath_MatchesResolvedWorkspaceParent(t *testing.T) {
	cfg, mountRoot := testNFSCleanupConfig(t)
	dir, hostBase, err := NFSProjectHostPath(cfg, "proj-1")
	if err != nil {
		t.Fatalf("NFSProjectHostPath: %v", err)
	}
	if want := filepath.Join(mountRoot, "share1"); hostBase != want {
		t.Fatalf("hostBase = %q, want %q", hostBase, want)
	}
	res, err := NewNFSBackend(cfg).Resolve(ResolveInput{ProjectID: "proj-1"})
	if err != nil {
		t.Fatal(err)
	}
	if dir != filepath.Dir(res.HostPath) {
		t.Fatalf("project dir %q is not the parent of the resolved workspace %q", dir, res.HostPath)
	}
	// The k8s provisioning state dir sits in that same project dir.
	stateSub, err := NFSProvisionStateSubPath(res.ServerRelativePath, "proj-1")
	if err != nil {
		t.Fatal(err)
	}
	if got := filepath.Join(hostBase, stateSub); filepath.Dir(got) != dir {
		t.Fatalf("provision state dir %q is not inside project dir %q", got, dir)
	}
}

func TestPathStrictlyUnder(t *testing.T) {
	cases := []struct {
		target, root string
		want         bool
	}{
		{"/a/b/c", "/a/b", true},
		{"/a/b", "/a/b", false},
		{"/a/bc", "/a/b", false},
		{"/a/b/../c", "/a/b", false},
		{"/x", "/", true},
		{"/", "/", false},
	}
	for _, tc := range cases {
		if got := pathStrictlyUnder(tc.target, tc.root); got != tc.want {
			t.Errorf("pathStrictlyUnder(%q, %q) = %v, want %v", tc.target, tc.root, got, tc.want)
		}
	}
}

func TestCleanupNFSProject_RefusesHostBaseNotUnderMountRoot(t *testing.T) {
	cfg, mountRoot := testNFSCleanupConfig(t)
	// A share ID of ".." would put the host base above the mount root.
	cfg.Shares[0].ID = ".."
	victim := filepath.Join(filepath.Dir(mountRoot), "projects", "proj-1")
	if err := os.MkdirAll(victim, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := CleanupNFSProject(cfg, "proj-1"); err == nil {
		t.Fatal("expected refusal for a share host base outside the mount root")
	}
	if _, err := os.Stat(victim); err != nil {
		t.Fatalf("directory outside the mount root must be untouched: %v", err)
	}
	cfg.Shares[0].ID = ""
	if err := CleanupNFSProject(cfg, "proj-1"); err == nil {
		t.Fatal("expected refusal for a share host base equal to the mount root")
	}
}

func TestCleanupNFSProjectRetry(t *testing.T) {
	cfg, mountRoot := testNFSCleanupConfig(t)
	projectPath := createProjectSubtree(t, mountRoot, "share1", "proj-retry")
	if err := CleanupNFSProjectRetry(context.Background(), cfg, "proj-retry", time.Hour); err != nil {
		t.Fatalf("CleanupNFSProjectRetry: %v", err)
	}
	if _, err := os.Lstat(projectPath); !os.IsNotExist(err) {
		t.Fatalf("project tree should be gone, Lstat err = %v", err)
	}
	// A guard refusal is returned at once, without waiting to retry.
	other := createProjectSubtree(t, mountRoot, "share1", "proj-other")
	if err := os.Symlink(other, filepath.Join(mountRoot, "share1", "projects", "proj-link")); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	err := CleanupNFSProjectRetry(context.Background(), cfg, "proj-link", time.Hour)
	if !errors.Is(err, ErrNFSCleanupRefused) {
		t.Fatalf("expected ErrNFSCleanupRefused, got %v", err)
	}
	if time.Since(start) > time.Minute {
		t.Fatal("a refusal must not wait for the retry")
	}
}

// A removal error is retried after the delay; a done context skips the
// retry instead of waiting.
func TestCleanupNFSProjectRetry_RemovalErrorRetriesUntilContextDone(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory permissions")
	}
	cfg, mountRoot := testNFSCleanupConfig(t)
	projectPath := createProjectSubtree(t, mountRoot, "share1", "proj-ro")
	ws := filepath.Join(projectPath, "workspace")
	if err := os.Chmod(ws, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(ws, 0o755) })

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := CleanupNFSProjectRetry(ctx, cfg, "proj-ro", time.Hour)
	if err == nil || errors.Is(err, ErrNFSCleanupRefused) {
		t.Fatalf("expected a removal error, got %v", err)
	}

	// With a short delay the retry runs and succeeds once the tree is
	// removable again.
	go func() {
		time.Sleep(20 * time.Millisecond)
		_ = os.Chmod(ws, 0o755)
	}()
	if err := CleanupNFSProjectRetry(context.Background(), cfg, "proj-ro", 200*time.Millisecond); err != nil {
		t.Fatalf("retry should succeed: %v", err)
	}
	if _, err := os.Lstat(projectPath); !os.IsNotExist(err) {
		t.Fatalf("project tree should be gone after the retry, Lstat err = %v", err)
	}
}
