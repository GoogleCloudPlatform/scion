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

package config

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/GoogleCloudPlatform/scion/pkg/api"
)

// The tests in this file use the real git check (no isGitRepoFunc override)
// so that the layout decision is made from the directories on disk.

// gitCommitAll stages and commits everything in dir.
func gitCommitAll(t *testing.T, dir string) {
	t.Helper()
	for _, args := range [][]string{
		{"add", "-A"},
		{"commit", "-m", "add files"},
	} {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v failed: %v\n%s", args, err, out)
		}
	}
}

// plainDir creates a directory under home that is not inside a repository.
func plainDir(t *testing.T, home, name string) string {
	t.Helper()
	dir := filepath.Join(home, name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	return dir
}

func assertInRepoProjectID(t *testing.T, scionDir, want string) {
	t.Helper()
	info, err := os.Lstat(scionDir)
	if err != nil || !info.IsDir() {
		t.Fatalf("expected an in-repo %s directory, stat err = %v", DotScion, err)
	}
	got, err := ReadProjectID(scionDir)
	if err != nil {
		t.Fatalf("ReadProjectID: %v", err)
	}
	if got != want {
		t.Errorf("project-id = %q, want %q", got, want)
	}
}

func TestInitProject_ExplicitTargetInRepoFromOutsideRepoUsesInRepoLayout(t *testing.T) {
	home, clone := ownershipTestEnvNamed(t, "clone")
	setupGitRepoDir(t, clone)
	t.Chdir(plainDir(t, home, "elsewhere"))

	id := api.NewUUID()
	scionDir := filepath.Join(clone, DotScion)
	if err := InitProject(scionDir, nil, InitProjectOpts{SkipRuntimeCheck: true, ProjectID: id}); err != nil {
		t.Fatalf("InitProject: %v", err)
	}
	assertInRepoProjectID(t, scionDir, id)
}

func TestInitProject_ExplicitPlainTargetFromInsideRepoUsesExternalLayout(t *testing.T) {
	home, repo := ownershipTestEnvNamed(t, "repo")
	setupGitRepoDir(t, repo)
	t.Chdir(repo)
	target := plainDir(t, home, "plain")

	if err := InitProject(filepath.Join(target, DotScion), nil, InitProjectOpts{SkipRuntimeCheck: true}); err != nil {
		t.Fatalf("InitProject: %v", err)
	}
	if !IsProjectMarkerFile(filepath.Join(target, DotScion)) {
		t.Error("expected the external layout (marker file) for a target outside any repository")
	}
	// The repository the process runs in is not touched.
	if _, err := os.Lstat(filepath.Join(repo, ".gitignore")); !os.IsNotExist(err) {
		t.Errorf("expected no .gitignore written in the current directory's repository, stat err = %v", err)
	}
	if _, err := os.Lstat(filepath.Join(repo, DotScion)); !os.IsNotExist(err) {
		t.Errorf("expected no %s in the current directory's repository, stat err = %v", DotScion, err)
	}
}

func TestInitProject_ExplicitTargetLeavesGitignoreUntouched(t *testing.T) {
	home, clone := ownershipTestEnvNamed(t, "clone")
	setupGitRepoDir(t, clone)
	const content = "build/\n"
	gitignore := filepath.Join(clone, ".gitignore")
	if err := os.WriteFile(gitignore, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	gitCommitAll(t, clone)
	t.Chdir(plainDir(t, home, "elsewhere"))

	if err := InitProject(filepath.Join(clone, DotScion), nil, InitProjectOpts{SkipRuntimeCheck: true, ProjectID: api.NewUUID()}); err != nil {
		t.Fatalf("InitProject: %v", err)
	}
	got, err := os.ReadFile(gitignore)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != content {
		t.Errorf(".gitignore changed to %q, want %q", got, content)
	}
}

func TestInitProject_ExplicitTargetCommittedScionDirTakesGivenProjectID(t *testing.T) {
	home, clone := ownershipTestEnvNamed(t, "clone")
	setupGitRepoDir(t, clone)
	scionDir := filepath.Join(clone, DotScion)
	if err := os.MkdirAll(filepath.Join(scionDir, "templates"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := WriteProjectID(scionDir, api.NewUUID()); err != nil {
		t.Fatal(err)
	}
	gitCommitAll(t, clone)
	t.Chdir(plainDir(t, home, "elsewhere"))

	id := api.NewUUID()
	if err := InitProject(scionDir, nil, InitProjectOpts{SkipRuntimeCheck: true, ProjectID: id}); err != nil {
		t.Fatalf("InitProject: %v", err)
	}
	assertInRepoProjectID(t, scionDir, id)
}

func TestInitProject_ExplicitTargetCommittedMarkerFileKept(t *testing.T) {
	for _, tc := range []struct {
		name   string
		sameID bool
	}{
		{"same project ID", true},
		{"different project ID", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home, clone := ownershipTestEnvNamed(t, "clone")
			setupGitRepoDir(t, clone)
			markerPath := filepath.Join(clone, DotScion)
			committedID := api.NewUUID()
			if err := WriteProjectMarker(markerPath, &ProjectMarker{
				ProjectID: committedID, ProjectName: "clone", ProjectSlug: "clone",
			}); err != nil {
				t.Fatal(err)
			}
			gitCommitAll(t, clone)
			t.Chdir(plainDir(t, home, "elsewhere"))

			id := api.NewUUID()
			if tc.sameID {
				id = committedID
			}
			if err := InitProject(markerPath, nil, InitProjectOpts{SkipRuntimeCheck: true, ProjectID: id}); err != nil {
				t.Fatalf("InitProject: %v", err)
			}
			if !IsProjectMarkerFile(markerPath) {
				t.Fatal("expected the committed marker file to stay a marker file")
			}
			marker, err := ReadProjectMarker(markerPath)
			if err != nil {
				t.Fatal(err)
			}
			if marker.ProjectID != id {
				t.Errorf("marker project ID = %q, want %q", marker.ProjectID, id)
			}
			ext, err := marker.ExternalProjectPath()
			if err != nil {
				t.Fatal(err)
			}
			if info, err := os.Stat(ext); err != nil || !info.IsDir() {
				t.Errorf("expected the external project directory %s, stat err = %v", ext, err)
			}
		})
	}
}

func TestInitProject_CurrentDirRepoStillUpdatesGitignore(t *testing.T) {
	_, repo := ownershipTestEnvNamed(t, "repo")
	setupGitRepoDir(t, repo)
	t.Chdir(repo)

	if err := InitProject("", nil, InitProjectOpts{SkipRuntimeCheck: true}); err != nil {
		t.Fatalf("InitProject: %v", err)
	}
	info, err := os.Lstat(filepath.Join(repo, DotScion))
	if err != nil || !info.IsDir() {
		t.Fatalf("expected an in-repo %s directory, stat err = %v", DotScion, err)
	}
	got, err := os.ReadFile(filepath.Join(repo, ".gitignore"))
	if err != nil {
		t.Fatalf("expected a .gitignore in the repository: %v", err)
	}
	if !strings.Contains(string(got), ".scion/agents") {
		t.Errorf(".gitignore = %q, want an entry for .scion/agents", got)
	}
}
