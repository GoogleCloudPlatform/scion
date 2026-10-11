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
