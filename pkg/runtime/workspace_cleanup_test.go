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
	"os"
	"path/filepath"
	"testing"

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
