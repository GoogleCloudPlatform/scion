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
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// writeKeepTree creates dir with a file in it and returns the file's path.
func writeKeepTree(t *testing.T, dir string) string {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(dir, "workspace"), 0o755); err != nil {
		t.Fatal(err)
	}
	keep := filepath.Join(dir, "workspace", "keep.txt")
	if err := os.WriteFile(keep, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	return keep
}

// The project tree is removed only at the canonical
// <host base>/<subpath_root>/<id>. A subpath root that is a symlink to
// another directory on the same share, holding a directory with the same
// project ID, is refused, and nothing is removed.
func TestCleanupNFSProject_RefusesSymlinkedSubPathRootOnShare(t *testing.T) {
	cfg, mountRoot := testNFSCleanupConfig(t)
	share := filepath.Join(mountRoot, "share1")
	keep := writeKeepTree(t, filepath.Join(share, "other", "proj-1"))
	if err := os.Symlink(filepath.Join(share, "other"), filepath.Join(share, "projects")); err != nil {
		t.Fatal(err)
	}

	err := CleanupNFSProject(cfg, "proj-1")
	if err == nil {
		t.Fatal("expected a refusal for a symlinked subpath root")
	}
	if !errors.Is(err, ErrNFSCleanupRefused) {
		t.Errorf("err = %v, want ErrNFSCleanupRefused (retrying cannot help)", err)
	}
	if _, statErr := os.Stat(keep); statErr != nil {
		t.Fatalf("the directory the symlink points to must be untouched: %v", statErr)
	}
	if fi, lerr := os.Lstat(filepath.Join(share, "projects")); lerr != nil || fi.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("the symlink itself must be left in place (lstat: %v)", lerr)
	}
}

// A symlink in the middle of a multi-component subpath root is refused the
// same way, even when it points inside the share.
func TestCleanupNFSProject_RefusesSymlinkedSubPathRootComponent(t *testing.T) {
	cfg, mountRoot := testNFSCleanupConfig(t)
	cfg.SubPathRoot = "tenants/projects"
	share := filepath.Join(mountRoot, "share1")
	keep := writeKeepTree(t, filepath.Join(share, "elsewhere", "projects", "proj-1"))
	if err := os.Symlink(filepath.Join(share, "elsewhere"), filepath.Join(share, "tenants")); err != nil {
		t.Fatal(err)
	}

	err := CleanupNFSProject(cfg, "proj-1")
	if err == nil {
		t.Fatal("expected a refusal for a symlinked subpath root component")
	}
	if !errors.Is(err, ErrNFSCleanupRefused) {
		t.Errorf("err = %v, want ErrNFSCleanupRefused", err)
	}
	if _, statErr := os.Stat(keep); statErr != nil {
		t.Fatalf("the directory behind the symlinked component must be untouched: %v", statErr)
	}
}

// The canonical layout with a multi-component subpath root is still
// removed, and only the named project.
func TestCleanupNFSProject_CanonicalMultiComponentSubPathRootRemoved(t *testing.T) {
	cfg, mountRoot := testNFSCleanupConfig(t)
	cfg.SubPathRoot = "tenants/projects"
	root := filepath.Join(mountRoot, "share1", "tenants", "projects")
	writeKeepTree(t, filepath.Join(root, "proj-1"))
	other := writeKeepTree(t, filepath.Join(root, "proj-2"))

	if err := CleanupNFSProject(cfg, "proj-1"); err != nil {
		t.Fatalf("CleanupNFSProject: %v", err)
	}
	if _, err := os.Lstat(filepath.Join(root, "proj-1")); !os.IsNotExist(err) {
		t.Errorf("proj-1 should be removed (lstat: %v)", err)
	}
	if _, err := os.Stat(other); err != nil {
		t.Errorf("proj-2 must be untouched: %v", err)
	}
	if _, err := os.Stat(root); err != nil {
		t.Errorf("the subpath root itself must be kept: %v", err)
	}
}
