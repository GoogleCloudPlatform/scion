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

	"github.com/GoogleCloudPlatform/scion/pkg/api"
)

const outsideContent = "outside-secret"

// outsideDir is a directory outside every tree under test, holding a file
// no Open'd root may ever reach.
func outsideDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "secret.txt"), []byte(outsideContent), 0o600); err != nil {
		t.Fatal(err)
	}
	return dir
}

func resolveNFSScratch(t *testing.T, hostBase string) Resolved {
	t.Helper()
	l := Layout{Backend: BackendNFS, HostBase: hostBase}
	r, err := l.Resolve(Project{ID: testPID}, "", SharedDirMount(api.SharedDir{Name: "scratchpad"}, ""))
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func TestOpenNFS_CreateThenRoundTrip(t *testing.T) {
	base := t.TempDir()
	r := resolveNFSScratch(t, base)

	if _, err := r.Open(false); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("Open(false) on a missing leaf: err = %v, want fs.ErrNotExist", err)
	}
	if _, err := os.Stat(r.HostPath); !os.IsNotExist(err) {
		t.Fatalf("Open(false) created the leaf: %v", err)
	}

	root, err := r.Open(true)
	if err != nil {
		t.Fatalf("Open(true): %v", err)
	}
	if err := root.WriteFile("hello.txt", []byte("hi"), 0o644); err != nil {
		t.Fatal(err)
	}
	_ = root.Close()

	b, err := os.ReadFile(filepath.Join(base, "projects", testPID, "shared-dirs", "scratchpad", "hello.txt"))
	if err != nil || string(b) != "hi" {
		t.Fatalf("file not at the uuid tree path: %q, %v", b, err)
	}

	root2, err := r.Open(false)
	if err != nil {
		t.Fatalf("Open(false) after create: %v", err)
	}
	defer func() { _ = root2.Close() }()
	if _, err := root2.Stat("hello.txt"); err != nil {
		t.Fatalf("Stat through reopened root: %v", err)
	}
}

func TestOpenNFS_RefusesSymlinkedComponents(t *testing.T) {
	for _, comp := range []string{"project", "shared-dirs", "leaf"} {
		t.Run(comp, func(t *testing.T) {
			base := t.TempDir()
			outside := outsideDir(t)
			r := resolveNFSScratch(t, base)
			projectDir := filepath.Join(base, "projects", testPID)
			var link string
			switch comp {
			case "project":
				link = projectDir
			case "shared-dirs":
				link = filepath.Join(projectDir, "shared-dirs")
			case "leaf":
				link = r.HostPath
			}
			if err := os.MkdirAll(filepath.Dir(link), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(outside, link); err != nil {
				t.Fatal(err)
			}
			for _, create := range []bool{false, true} {
				root, err := r.Open(create)
				if err == nil {
					_ = root.Close()
					t.Fatalf("Open(%v) followed a symlinked %s component", create, comp)
				}
			}
			if _, err := r.VerifyNoSymlinks(); comp == "leaf" && err == nil {
				t.Fatalf("VerifyNoSymlinks accepted a symlinked %s component", comp)
			}
			if b, err := os.ReadFile(filepath.Join(outside, "secret.txt")); err != nil || string(b) != outsideContent {
				t.Fatalf("outside tree changed: %q, %v", b, err)
			}
			entries, _ := os.ReadDir(outside)
			if len(entries) != 1 {
				t.Fatalf("Open created entries outside the tree: %v", entries)
			}
		})
	}
}

// TestVerifyNoSymlinks_StructuralLinkToExistingTree: a structural component
// linked to a tree in which the leaf exists is refused by the backstop.
func TestVerifyNoSymlinks_StructuralLinkToExistingTree(t *testing.T) {
	base := t.TempDir()
	outside := t.TempDir()
	if err := os.MkdirAll(filepath.Join(outside, "shared-dirs", "scratchpad"), 0o755); err != nil {
		t.Fatal(err)
	}
	r := resolveNFSScratch(t, base)
	projectDir := filepath.Join(base, "projects", testPID)
	if err := os.MkdirAll(filepath.Dir(projectDir), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, projectDir); err != nil {
		t.Fatal(err)
	}
	if _, err := r.VerifyNoSymlinks(); err == nil {
		t.Fatal("VerifyNoSymlinks accepted a leaf reached through a symlinked project dir")
	}
	if root, err := r.Open(false); err == nil {
		_ = root.Close()
		t.Fatal("Open followed a symlinked project dir")
	}
}

func TestOpenNFS_RootConfinesEscapes(t *testing.T) {
	base := t.TempDir()
	outside := outsideDir(t)
	r := resolveNFSScratch(t, base)
	root, err := r.Open(true)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = root.Close() }()
	if err := os.Symlink(outside, filepath.Join(r.HostPath, "escape")); err != nil {
		t.Fatal(err)
	}
	if _, err := root.ReadFile("escape/secret.txt"); err == nil {
		t.Fatal("root followed a symlink out of the shared dir")
	}
	if _, err := root.ReadFile("../../../../../secret.txt"); err == nil {
		t.Fatal("root allowed a .. escape")
	}
}

func TestOpenNFS_SymlinkedHostBaseAllowed(t *testing.T) {
	realDir := t.TempDir()
	linkParent := t.TempDir()
	base := filepath.Join(linkParent, "mnt")
	if err := os.Symlink(realDir, base); err != nil {
		t.Fatal(err)
	}
	r := resolveNFSScratch(t, base)
	root, err := r.Open(true)
	if err != nil {
		t.Fatalf("Open through a symlinked host base (operator mount point): %v", err)
	}
	_ = root.Close()
	got, err := r.VerifyNoSymlinks()
	if err != nil {
		t.Fatal(err)
	}
	realResolved, _ := filepath.EvalSymlinks(realDir)
	if want := filepath.Join(realResolved, r.RelPath); got != want {
		t.Fatalf("VerifyNoSymlinks = %q, want %q", got, want)
	}
}

func TestOpenNFS_RelativeHostBaseRefused(t *testing.T) {
	r := resolveNFSScratch(t, "relative/base")
	if _, err := r.Open(true); err == nil {
		t.Fatal("Open accepted a relative host base")
	}
}

func TestVerifyNoSymlinks_MissingLeafPasses(t *testing.T) {
	base := t.TempDir()
	r := resolveNFSScratch(t, base)
	got, err := r.VerifyNoSymlinks()
	if err != nil {
		t.Fatal(err)
	}
	realBase, _ := filepath.EvalSymlinks(base)
	if want := filepath.Join(realBase, r.RelPath); got != want {
		t.Fatalf("VerifyNoSymlinks = %q, want %q", got, want)
	}
	if _, err := os.Stat(r.HostPath); !os.IsNotExist(err) {
		t.Fatal("VerifyNoSymlinks created the leaf")
	}
	if _, err := (Resolved{Backend: BackendLocal}).VerifyNoSymlinks(); err == nil {
		t.Fatal("VerifyNoSymlinks on local should be refused")
	}
}

func TestOpenLocal(t *testing.T) {
	home := t.TempDir()
	l := Layout{Backend: BackendLocal, HostBase: home}
	r, err := l.Resolve(Project{ID: testPID, Slug: "acme"}, "", SharedDirMount(api.SharedDir{Name: "scratchpad"}, ""))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.Open(false); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("Open(false) on a missing dir: err = %v, want fs.ErrNotExist", err)
	}
	root, err := r.Open(true)
	if err != nil {
		t.Fatal(err)
	}
	_ = root.Close()
	if fi, err := os.Stat(filepath.Join(home, "project-configs", "acme__550e8400", "shared-dirs", "scratchpad")); err != nil || !fi.IsDir() {
		t.Fatalf("local shared dir not created at the slug path: %v", err)
	}
}

func TestOpenLocal_RefusesSymlinkedLeaf(t *testing.T) {
	home := t.TempDir()
	outside := outsideDir(t)
	l := Layout{Backend: BackendLocal, HostBase: home}
	r, err := l.Resolve(Project{ID: testPID, Slug: "acme"}, "", SharedDirMount(api.SharedDir{Name: "scratchpad"}, ""))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(r.HostPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, r.HostPath); err != nil {
		t.Fatal(err)
	}
	for _, create := range []bool{false, true} {
		root, err := r.Open(create)
		if err == nil {
			_ = root.Close()
			t.Fatalf("Open(%v) rooted at a symlinked local dir", create)
		}
	}
}

func TestOpenUnknownBackend(t *testing.T) {
	if _, err := (Resolved{Backend: "gcs", HostPath: t.TempDir()}).Open(false); err == nil {
		t.Fatal("Open on an unknown backend should fail")
	}
}
