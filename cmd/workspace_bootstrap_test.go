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

package cmd

import (
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/config"
)

// setupMarkerLayoutProject creates a non-git workspace whose .scion is a
// marker file, under a temporary HOME. It returns the workspace root and the
// project path the marker resolves to.
func setupMarkerLayoutProject(t *testing.T) (workspace, projectPath string) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	workspace = filepath.Join(t.TempDir(), "demo")
	writeWorkspaceTree(t, workspace, "main.go", "sub/notes.txt")
	marker := &config.ProjectMarker{
		ProjectID:   "0f1e2d3c-4b5a-6978-8a9b-0c1d2e3f4a5b",
		ProjectName: "demo",
		ProjectSlug: "demo",
	}
	if err := config.WriteProjectMarker(filepath.Join(workspace, config.DotScion), marker); err != nil {
		t.Fatal(err)
	}
	projectPath, err := marker.ExternalProjectPath()
	if err != nil {
		t.Fatal(err)
	}
	writeWorkspaceTree(t, projectPath, "settings.yaml", "templates/default/agent.md")
	return workspace, projectPath
}

func TestCollectBootstrapWorkspace_MarkerLayout(t *testing.T) {
	workspace, projectPath := setupMarkerLayoutProject(t)
	t.Chdir(filepath.Join(workspace, "sub"))

	dir, files, _, err := collectBootstrapWorkspace(projectPath, "")
	if err != nil {
		t.Fatal(err)
	}
	if !sameLocalPath(dir, workspace) {
		t.Errorf("collected from %q, want workspace root %q", dir, workspace)
	}
	got := collectedPaths(files)
	slices.Sort(got)
	want := []string{"main.go", "sub/notes.txt"}
	if !slices.Equal(got, want) {
		t.Errorf("collected %v, want %v", got, want)
	}
}

func TestWorkspaceBootstrapCollectsOnlyWorkspaceRootFiles(t *testing.T) {
	t.Run("workspace bootstrap collects only workspace-root files", func(t *testing.T) {
		workspace, projectPath := setupMarkerLayoutProject(t)
		writeWorkspaceTree(t, filepath.Join(filepath.Dir(workspace), "sibling"), "stray.txt")
		t.Chdir(workspace)

		dir, files, _, err := collectBootstrapWorkspace(projectPath, "")
		if err != nil {
			t.Fatal(err)
		}
		if !sameLocalPath(dir, workspace) {
			t.Errorf("collected from %q, want workspace root %q", dir, workspace)
		}
		got := collectedPaths(files)
		slices.Sort(got)
		want := []string{"main.go", "sub/notes.txt"}
		if !slices.Equal(got, want) {
			t.Errorf("collected %v, want exactly the workspace-root files %v", got, want)
		}
	})

	t.Run("workspace root of another project", func(t *testing.T) {
		_, projectPath := setupMarkerLayoutProject(t)
		other := filepath.Join(t.TempDir(), "other")
		writeWorkspaceTree(t, other, "other.go")
		otherMarker := &config.ProjectMarker{
			ProjectID:   "9f8e7d6c-5b4a-4321-8fed-cba987654321",
			ProjectName: "other",
			ProjectSlug: "other",
		}
		if err := config.WriteProjectMarker(filepath.Join(other, config.DotScion), otherMarker); err != nil {
			t.Fatal(err)
		}
		t.Chdir(other)

		dir, files, _, err := collectBootstrapWorkspace(projectPath, "")
		if err != nil {
			t.Fatal(err)
		}
		if dir != "" || len(files) != 0 {
			t.Errorf("collected %v from %q, want nothing", collectedPaths(files), dir)
		}
	})

	t.Run("project path spelled through a link", func(t *testing.T) {
		workspace, projectPath := setupMarkerLayoutProject(t)
		link := filepath.Join(t.TempDir(), "home-link")
		if err := os.Symlink(os.Getenv("HOME"), link); err != nil {
			t.Skipf("symlinks unavailable: %v", err)
		}
		t.Setenv("HOME", link)
		t.Chdir(workspace)

		dir, files, _, err := collectBootstrapWorkspace(projectPath, "")
		if err != nil {
			t.Fatal(err)
		}
		if !sameLocalPath(dir, workspace) {
			t.Errorf("collected from %q, want workspace root %q", dir, workspace)
		}
		got := collectedPaths(files)
		slices.Sort(got)
		if want := []string{"main.go", "sub/notes.txt"}; !slices.Equal(got, want) {
			t.Errorf("collected %v, want exactly the workspace-root files %v", got, want)
		}
	})

	t.Run("project path given from another directory", func(t *testing.T) {
		workspace, projectPath := setupMarkerLayoutProject(t)
		t.Chdir(t.TempDir())

		dir, files, _, err := collectBootstrapWorkspace(projectPath, workspace)
		if err != nil {
			t.Fatal(err)
		}
		if !sameLocalPath(dir, workspace) {
			t.Errorf("collected from %q, want workspace root %q", dir, workspace)
		}
		got := collectedPaths(files)
		slices.Sort(got)
		want := []string{"main.go", "sub/notes.txt"}
		if !slices.Equal(got, want) {
			t.Errorf("collected %v, want exactly the workspace-root files %v", got, want)
		}
	})

	t.Run("recorded workspace path from another directory", func(t *testing.T) {
		workspace, projectPath := setupMarkerLayoutProject(t)
		settings := "workspace_path: " + strconv.Quote(workspace) + "\n"
		if err := os.WriteFile(filepath.Join(projectPath, "settings.yaml"), []byte(settings), 0o644); err != nil {
			t.Fatal(err)
		}
		t.Chdir(t.TempDir())

		dir, files, _, err := collectBootstrapWorkspace(projectPath, "")
		if err != nil {
			t.Fatal(err)
		}
		if !sameLocalPath(dir, workspace) {
			t.Errorf("collected from %q, want workspace root %q", dir, workspace)
		}
		got := collectedPaths(files)
		slices.Sort(got)
		want := []string{"main.go", "sub/notes.txt"}
		if !slices.Equal(got, want) {
			t.Errorf("collected %v, want exactly the workspace-root files %v", got, want)
		}
	})
}

func TestCollectBootstrapWorkspace_GitProject(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	t.Setenv("HOME", t.TempDir())
	workspace := filepath.Join(t.TempDir(), "repo")
	writeWorkspaceTree(t, workspace, "main.go", ".scion/settings.yaml")
	if out, err := exec.Command("git", "init", "-q", workspace).CombinedOutput(); err != nil {
		t.Fatalf("git init: %v: %s", err, out)
	}
	t.Chdir(workspace)

	dir, files, _, err := collectBootstrapWorkspace(filepath.Join(workspace, config.DotScion), "")
	if err != nil {
		t.Fatal(err)
	}
	if dir != "" || len(files) != 0 {
		t.Errorf("git project: collected %v from %q, want nothing", collectedPaths(files), dir)
	}
}

func TestCollectBootstrapWorkspace_InWorkspaceScionDir(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	workspace := filepath.Join(t.TempDir(), "legacy")
	writeWorkspaceTree(t, workspace, "main.go", "docs/readme.md", ".scion/settings.yaml", ".scion/templates/x/agent.md")
	if err := os.MkdirAll(workspace, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Chdir(workspace)

	dir, files, _, err := collectBootstrapWorkspace(filepath.Join(workspace, config.DotScion), "")
	if err != nil {
		t.Fatal(err)
	}
	if !sameLocalPath(dir, workspace) {
		t.Errorf("collected from %q, want %q", dir, workspace)
	}
	got := collectedPaths(files)
	slices.Sort(got)
	want := []string{"docs/readme.md", "main.go"}
	if !slices.Equal(got, want) {
		t.Errorf("collected %v, want %v", got, want)
	}
}

func TestCollectBootstrapWorkspace_InWorkspaceScionDirFromElsewhere(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	workspace := filepath.Join(t.TempDir(), "legacy")
	writeWorkspaceTree(t, workspace, "main.go", ".scion/settings.yaml")
	t.Chdir(t.TempDir())

	dir, files, _, err := collectBootstrapWorkspace(filepath.Join(workspace, config.DotScion), workspace)
	if err != nil {
		t.Fatal(err)
	}
	if !sameLocalPath(dir, workspace) {
		t.Errorf("collected from %q, want %q", dir, workspace)
	}
	if got := collectedPaths(files); !slices.Equal(got, []string{"main.go"}) {
		t.Errorf("collected %v, want [main.go]", got)
	}
}
