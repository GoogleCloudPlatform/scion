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

// Tests for ptone/scion#4506: deleting a hub-managed project on a broker
// also removes its whole project-configs/<slug>__<id8> directory.

// projectConfigsEntry returns ~/.scion/project-configs/<slug>__<short id>.
func projectConfigsEntry(home, slug, projectID string) string {
	return filepath.Join(home, ".scion", config.ProjectConfigsDir,
		config.ProjectMarker{ProjectID: projectID, ProjectSlug: slug}.DirName())
}

// seedProjectConfigsEntry creates a launch marker in the project-configs
// entry for slug and projectID and returns the entry.
func seedProjectConfigsEntry(t *testing.T, home, slug, projectID string) string {
	t.Helper()
	dir := projectConfigsEntry(home, slug, projectID)
	if err := os.MkdirAll(filepath.Join(dir, ".scion", "agents", "dev", "home"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "launch-marker"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

func writeWorkspaceRecordFor(t *testing.T, slug, projectID string) {
	t.Helper()
	record, err := config.BrokerWorkspaceRecordPath(slug)
	if err != nil {
		t.Fatal(err)
	}
	if err := config.WriteWorkspaceRecord(record, projectID); err != nil {
		t.Fatal(err)
	}
}

func TestDeleteProject_RemovesProjectConfigDir(t *testing.T) {
	srv, home := newScopeTestServer(t, &filteringMockManager{})
	makeHubMarkerProject(t, home, "proj-a", scopeProjA, "dev")
	makeHubMarkerProject(t, home, "proj-b", scopeProjB, "dev")
	dirA := seedProjectConfigsEntry(t, home, "proj-a", scopeProjA)
	seedSharedDir(t, filepath.Join(dirA, ".scion"), "scratch")
	// Another project with the same slug but a different ID.
	sameSlug := seedProjectConfigsEntry(t, home, "proj-a", scopeProjB)
	dirB := seedProjectConfigsEntry(t, home, "proj-b", scopeProjB)

	rec := doDeleteProject(t, srv, "proj-a", scopeProjA)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("expected 204, got %d: %s", rec.Code, rec.Body.String())
	}
	assertGone(t, filepath.Join(home, ".scion", "projects", "proj-a"))
	assertGone(t, dirA)
	assertPresent(t, filepath.Join(sameSlug, "launch-marker"))
	assertPresent(t, filepath.Join(dirB, "launch-marker"))
	assertPresent(t, filepath.Join(home, ".scion", "projects", "proj-b"))
}

func TestDeleteProject_GitSplitStorage_RemovesProjectConfigDir(t *testing.T) {
	srv, home := newScopeTestServer(t, &filteringMockManager{})
	scionDir, _ := makeHubProject(t, home, "proj-a", scopeProjA, "dev")
	ext, err := config.GetGitProjectExternalConfigDir(scionDir)
	if err != nil {
		t.Fatal(err)
	}
	dirA := seedProjectConfigsEntry(t, home, "proj-a", scopeProjA)
	if filepath.Dir(ext) != dirA {
		t.Fatalf("test setup: external config dir %q not in %q", ext, dirA)
	}

	rec := doDeleteProject(t, srv, "proj-a", scopeProjA)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("expected 204, got %d: %s", rec.Code, rec.Body.String())
	}
	assertGone(t, dirA)
}

func TestDeleteProject_KeepsProjectConfigDir(t *testing.T) {
	cases := []struct {
		name string
		// setup creates proj-a and returns the request's project ID.
		setup func(t *testing.T, home string) string
	}{
		{"no project ID", func(t *testing.T, home string) string {
			makeHubMarkerProject(t, home, "proj-a", scopeProjA, "dev")
			return ""
		}},
		{"marker records another project", func(t *testing.T, home string) string {
			makeHubMarkerProject(t, home, "proj-a", scopeProjB, "dev")
			return scopeProjA
		}},
		{"record names another project", func(t *testing.T, home string) string {
			makeHubMarkerProject(t, home, "proj-a", scopeProjA, "dev")
			writeWorkspaceRecordFor(t, "proj-a", scopeProjB)
			return scopeProjA
		}},
		{"record matches but marker disagrees", func(t *testing.T, home string) string {
			makeHubMarkerProject(t, home, "proj-a", scopeProjB, "dev")
			writeWorkspaceRecordFor(t, "proj-a", scopeProjA)
			return scopeProjA
		}},
		{"shared-dir check refuses the entry", func(t *testing.T, home string) string {
			ext, _ := makeHubMarkerProject(t, home, "proj-a", scopeProjA, "dev")
			if err := config.WriteProjectID(ext, scopeProjB); err != nil {
				t.Fatal(err)
			}
			return scopeProjA
		}},
		{"marker slug disagrees", func(t *testing.T, home string) string {
			root := filepath.Join(home, ".scion", "projects", "proj-a")
			if err := os.MkdirAll(root, 0o755); err != nil {
				t.Fatal(err)
			}
			m := &config.ProjectMarker{ProjectID: scopeProjA, ProjectName: "other", ProjectSlug: "other"}
			if err := config.WriteProjectMarker(filepath.Join(root, ".scion"), m); err != nil {
				t.Fatal(err)
			}
			return scopeProjA
		}},
		{"marker missing", func(t *testing.T, home string) string {
			if err := os.MkdirAll(filepath.Join(home, ".scion", "projects", "proj-a", "src"), 0o755); err != nil {
				t.Fatal(err)
			}
			return scopeProjA
		}},
		{"marker unreadable", func(t *testing.T, home string) string {
			root := filepath.Join(home, ".scion", "projects", "proj-a")
			if err := os.MkdirAll(root, 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(root, ".scion"), []byte("{not yaml: ["), 0o644); err != nil {
				t.Fatal(err)
			}
			return scopeProjA
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv, home := newScopeTestServer(t, &filteringMockManager{})
			id := tc.setup(t, home)
			dirA := seedProjectConfigsEntry(t, home, "proj-a", scopeProjA)
			sameSlug := seedProjectConfigsEntry(t, home, "proj-a", scopeProjB)
			makeHubMarkerProject(t, home, "proj-b", scopeProjB, "dev")
			dirB := seedProjectConfigsEntry(t, home, "proj-b", scopeProjB)

			rec := doDeleteProject(t, srv, "proj-a", id)
			if rec.Code != http.StatusNoContent {
				t.Fatalf("expected 204, got %d: %s", rec.Code, rec.Body.String())
			}
			assertPresent(t, filepath.Join(dirA, "launch-marker"))
			assertPresent(t, filepath.Join(dirA, ".scion", "agents", "dev", "home"))
			assertPresent(t, filepath.Join(sameSlug, "launch-marker"))
			assertPresent(t, filepath.Join(dirB, "launch-marker"))
			assertPresent(t, filepath.Join(home, ".scion", "projects", "proj-b", ".scion"))
		})
	}
}

// Project IDs sharing scopeProjA's short ID, so they map to the same
// <slug>__<short ID> entry: only the ID checks tell them apart.
const (
	samePrefixMarkerID = "11111111-bbbb-bbbb-bbbb-111111111111"
	samePrefixGitID    = "11111111-cccc-cccc-cccc-111111111111"
)

func TestProjectConfigDirFor(t *testing.T) {
	cases := []struct {
		name string
		// setup creates the project and returns the projectConfigDirFor
		// arguments globalDir, slug and projectID; projectPath is
		// <home>/.scion/projects/proj-a.
		setup func(t *testing.T, home string) (globalDir, slug, projectID string)
		want  bool
		// sharedEntry marks cases whose <slug>__<short ID> entry exists
		// and matches the request, so only the ID checks refuse it.
		sharedEntry bool
	}{
		{name: "marker project", setup: func(t *testing.T, home string) (string, string, string) {
			makeHubMarkerProject(t, home, "proj-a", scopeProjA, "dev")
			return filepath.Join(home, ".scion"), "proj-a", scopeProjA
		}, want: true},
		{name: "git project", setup: func(t *testing.T, home string) (string, string, string) {
			makeHubProject(t, home, "proj-a", scopeProjA, "dev")
			seedProjectConfigsEntry(t, home, "proj-a", scopeProjA)
			return filepath.Join(home, ".scion"), "proj-a", scopeProjA
		}, want: true},
		{name: "marker records another ID with the same short ID", setup: func(t *testing.T, home string) (string, string, string) {
			makeHubMarkerProject(t, home, "proj-a", samePrefixMarkerID, "dev")
			return filepath.Join(home, ".scion"), "proj-a", scopeProjA
		}, sharedEntry: true},
		{name: "git project-id records another ID with the same short ID", setup: func(t *testing.T, home string) (string, string, string) {
			makeHubProject(t, home, "proj-a", samePrefixGitID, "dev")
			seedProjectConfigsEntry(t, home, "proj-a", scopeProjA)
			return filepath.Join(home, ".scion"), "proj-a", scopeProjA
		}, sharedEntry: true},
		{name: "marker slug differs", setup: func(t *testing.T, home string) (string, string, string) {
			root := filepath.Join(home, ".scion", "projects", "proj-a")
			if err := os.MkdirAll(root, 0o755); err != nil {
				t.Fatal(err)
			}
			m := &config.ProjectMarker{ProjectID: scopeProjA, ProjectName: "proj-b", ProjectSlug: "proj-b"}
			if err := config.WriteProjectMarker(filepath.Join(root, ".scion"), m); err != nil {
				t.Fatal(err)
			}
			seedProjectConfigsEntry(t, home, "proj-a", scopeProjA)
			return filepath.Join(home, ".scion"), "proj-a", scopeProjA
		}},
		{name: "requested slug differs from the project dir", setup: func(t *testing.T, home string) (string, string, string) {
			makeHubMarkerProject(t, home, "proj-a", scopeProjA, "dev")
			seedProjectConfigsEntry(t, home, "proj-b", scopeProjA)
			return filepath.Join(home, ".scion"), "proj-b", scopeProjA
		}},
		{name: "external path outside the expected entry", setup: func(t *testing.T, home string) (string, string, string) {
			// The marker resolves under HOME; globalDir is elsewhere and
			// has its own proj-a entry, which must not be returned.
			makeHubMarkerProject(t, home, "proj-a", scopeProjA, "dev")
			other := filepath.Join(t.TempDir(), ".scion")
			if err := os.MkdirAll(filepath.Join(other, config.ProjectConfigsDir,
				config.ProjectMarker{ProjectID: scopeProjA, ProjectSlug: "proj-a"}.DirName()), 0o755); err != nil {
				t.Fatal(err)
			}
			return other, "proj-a", scopeProjA
		}},
		{name: "symlinked entry", setup: func(t *testing.T, home string) (string, string, string) {
			root := filepath.Join(home, ".scion", "projects", "proj-a")
			if err := os.MkdirAll(root, 0o755); err != nil {
				t.Fatal(err)
			}
			m := &config.ProjectMarker{ProjectID: scopeProjA, ProjectName: "proj-a", ProjectSlug: "proj-a"}
			if err := config.WriteProjectMarker(filepath.Join(root, ".scion"), m); err != nil {
				t.Fatal(err)
			}
			entry := projectConfigsEntry(home, "proj-a", scopeProjA)
			if err := os.MkdirAll(filepath.Dir(entry), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(t.TempDir(), entry); err != nil {
				t.Fatal(err)
			}
			return filepath.Join(home, ".scion"), "proj-a", scopeProjA
		}},
		{name: "missing entry", setup: func(t *testing.T, home string) (string, string, string) {
			root := filepath.Join(home, ".scion", "projects", "proj-a")
			if err := os.MkdirAll(root, 0o755); err != nil {
				t.Fatal(err)
			}
			m := &config.ProjectMarker{ProjectID: scopeProjA, ProjectName: "proj-a", ProjectSlug: "proj-a"}
			if err := config.WriteProjectMarker(filepath.Join(root, ".scion"), m); err != nil {
				t.Fatal(err)
			}
			return filepath.Join(home, ".scion"), "proj-a", scopeProjA
		}},
		{name: "invalid project ID", setup: func(t *testing.T, home string) (string, string, string) {
			makeHubMarkerProject(t, home, "proj-a", scopeProjA, "dev")
			return filepath.Join(home, ".scion"), "proj-a", "not-a-project-id"
		}},
		{name: "empty project ID", setup: func(t *testing.T, home string) (string, string, string) {
			makeHubMarkerProject(t, home, "proj-a", scopeProjA, "dev")
			return filepath.Join(home, ".scion"), "proj-a", ""
		}},
		{name: "invalid slug", setup: func(t *testing.T, home string) (string, string, string) {
			makeHubMarkerProject(t, home, "proj-a", scopeProjA, "dev")
			return filepath.Join(home, ".scion"), "Proj_A", scopeProjA
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			home := t.TempDir()
			t.Setenv("HOME", home)
			globalDir, slug, id := tc.setup(t, home)
			projectPath := filepath.Join(home, ".scion", "projects", "proj-a")
			got := projectConfigDirFor(globalDir, projectPath, slug, id)
			want := ""
			if tc.want {
				want = projectConfigsEntry(home, slug, id)
			}
			if got != want {
				t.Errorf("projectConfigDirFor(%q, %q) = %q, want %q", slug, id, got, want)
			}
			if tc.sharedEntry {
				assertPresent(t, projectConfigsEntry(home, "proj-a", scopeProjA))
			}
		})
	}
}
