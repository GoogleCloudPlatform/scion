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
	"path/filepath"
	"testing"
)

// initWithProjectID runs InitProject for a project directory named
// "id-project" with opts.ProjectID set to projectID. gitForm selects the
// in-repo .scion/project-id form; otherwise the .scion marker form is used.
// seed, when non-nil, prepares the project root before InitProject runs.
// It returns the recorded identity and the external config directory.
func initWithProjectID(t *testing.T, gitForm bool, projectID string, seed func(root string)) (string, string) {
	t.Helper()
	t.Setenv("HOME", t.TempDir())

	root := filepath.Join(t.TempDir(), "id-project")
	if err := os.MkdirAll(root, 0755); err != nil {
		t.Fatal(err)
	}
	if gitForm {
		setupGitRepoDir(t, root)
	}
	t.Chdir(root)
	orig := isGitRepoFunc
	isGitRepoFunc = func() bool { return gitForm }
	t.Cleanup(func() { isGitRepoFunc = orig })

	if seed != nil {
		seed(root)
	}

	scionDir := filepath.Join(root, DotScion)
	if err := InitProject(scionDir, nil, InitProjectOpts{SkipRuntimeCheck: true, ProjectID: projectID}); err != nil {
		t.Fatalf("InitProject: %v", err)
	}

	if gitForm {
		id, err := ReadProjectID(scionDir)
		if err != nil {
			t.Fatalf("ReadProjectID: %v", err)
		}
		ext, err := GetGitProjectExternalConfigDir(scionDir)
		if err != nil {
			t.Fatalf("GetGitProjectExternalConfigDir: %v", err)
		}
		return id, ext
	}
	marker, err := ReadProjectMarker(scionDir)
	if err != nil {
		t.Fatalf("ReadProjectMarker: %v", err)
	}
	ext, err := marker.ExternalProjectPath()
	if err != nil {
		t.Fatal(err)
	}
	return marker.ProjectID, ext
}

func TestInitProject_ProjectIDOptionIsRecordedIdentity(t *testing.T) {
	const projectID = "3f2a9c1e-0b7d-4e55-9a10-6c2b8d4e7f01"
	const otherID = "9d8c7b6a-5f4e-4d3c-8b2a-1f0e9d8c7b6a"

	for _, form := range []struct {
		name    string
		gitForm bool
	}{
		{"project-id file", true},
		{"marker file", false},
	} {
		t.Run(form.name+"/new project", func(t *testing.T) {
			id, ext := initWithProjectID(t, form.gitForm, projectID, nil)
			if id != projectID {
				t.Errorf("identity = %q, want %q", id, projectID)
			}
			if got, want := filepath.Base(filepath.Dir(ext)), "id-project__3f2a9c1e"; got != want {
				t.Errorf("config dir = %q, want %q", got, want)
			}
			if _, err := os.Stat(ext); err != nil {
				t.Errorf("config dir not created: %v", err)
			}
			entries, err := os.ReadDir(filepath.Dir(filepath.Dir(ext)))
			if err != nil {
				t.Fatal(err)
			}
			if len(entries) != 1 {
				t.Errorf("project-configs holds %d entries, want only the one named after the given ID", len(entries))
			}
		})

		t.Run(form.name+"/identity present", func(t *testing.T) {
			seed := func(root string) {
				scionPath := filepath.Join(root, DotScion)
				if form.gitForm {
					if err := os.MkdirAll(scionPath, 0755); err != nil {
						t.Fatal(err)
					}
					if err := WriteProjectID(scionPath, otherID); err != nil {
						t.Fatal(err)
					}
					return
				}
				if err := WriteProjectMarker(scionPath, &ProjectMarker{
					ProjectID: otherID, ProjectName: "other", ProjectSlug: "other",
				}); err != nil {
					t.Fatal(err)
				}
			}
			id, ext := initWithProjectID(t, form.gitForm, projectID, seed)
			if id != projectID {
				t.Errorf("identity = %q, want %q", id, projectID)
			}
			if got, want := filepath.Base(filepath.Dir(ext)), "id-project__3f2a9c1e"; got != want {
				t.Errorf("config dir = %q, want %q", got, want)
			}
		})
	}
}

func TestInitProject_WithoutProjectIDKeepsPresentIdentity(t *testing.T) {
	const presentID = "9d8c7b6a-5f4e-4d3c-8b2a-1f0e9d8c7b6a"
	for _, form := range []struct {
		name    string
		gitForm bool
	}{
		{"project-id file", true},
		{"marker file", false},
	} {
		t.Run(form.name, func(t *testing.T) {
			seed := func(root string) {
				scionPath := filepath.Join(root, DotScion)
				if form.gitForm {
					if err := os.MkdirAll(scionPath, 0755); err != nil {
						t.Fatal(err)
					}
					if err := WriteProjectID(scionPath, presentID); err != nil {
						t.Fatal(err)
					}
					return
				}
				if err := WriteProjectMarker(scionPath, &ProjectMarker{
					ProjectID: presentID, ProjectName: "id-project", ProjectSlug: "id-project",
				}); err != nil {
					t.Fatal(err)
				}
			}
			id, _ := initWithProjectID(t, form.gitForm, "", seed)
			if id != presentID {
				t.Errorf("identity = %q, want the present %q", id, presentID)
			}
		})
	}
}
