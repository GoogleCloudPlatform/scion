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

//go:build unix

package projectfs

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/shareddirs"
)

const projectTreeTestID = "proj-1"

// OpenProjectTree returns the canonical subpath root and the project's
// entry name in it.
func TestOpenProjectTree_Canonical(t *testing.T) {
	base := t.TempDir()
	l := Layout{Backend: BackendNFS, HostBase: base, SubPathRoot: "tenants/projects"}
	if err := os.MkdirAll(filepath.Join(base, "tenants", "projects", projectTreeTestID, "workspace"), 0o755); err != nil {
		t.Fatal(err)
	}
	root, name, err := l.OpenProjectTree(projectTreeTestID)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = root.Close() }()
	if name != projectTreeTestID {
		t.Errorf("name = %q, want %q", name, projectTreeTestID)
	}
	if fi, err := root.Lstat(filepath.Join(name, "workspace")); err != nil || !fi.IsDir() {
		t.Errorf("workspace through the root: %v", err)
	}
}

// A missing subpath root or host base is reported as not existing.
func TestOpenProjectTree_Missing(t *testing.T) {
	base := t.TempDir()
	for _, l := range []Layout{
		{Backend: BackendNFS, HostBase: base},
		{Backend: BackendNFS, HostBase: filepath.Join(base, "absent")},
	} {
		if _, _, err := l.OpenProjectTree(projectTreeTestID); !errors.Is(err, fs.ErrNotExist) {
			t.Errorf("host base %q: err = %v, want fs.ErrNotExist", l.HostBase, err)
		}
	}
}

// A subpath root component that is a symlink, even one pointing inside the
// share, or one that is not a directory, is refused as unconfined.
func TestOpenProjectTree_RefusesUnconfinedComponents(t *testing.T) {
	base := t.TempDir()
	if err := os.MkdirAll(filepath.Join(base, "elsewhere", "projects", projectTreeTestID), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(base, "elsewhere"), filepath.Join(base, "tenants")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(base, "file"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	for _, sub := range []string{"tenants/projects", "elsewhere/projects/../projects", "file/projects"} {
		l := Layout{Backend: BackendNFS, HostBase: base, SubPathRoot: sub}
		_, _, err := l.OpenProjectTree(projectTreeTestID)
		if sub == "elsewhere/projects/../projects" {
			if err == nil {
				t.Errorf("subpath root %q: an unclean root must be refused", sub)
			}
			continue
		}
		if !errors.Is(err, shareddirs.ErrUnconfinedPath) {
			t.Errorf("subpath root %q: err = %v, want shareddirs.ErrUnconfinedPath", sub, err)
		}
	}
}
